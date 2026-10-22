package router_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/service"
)

// scriptedChat is a ChatClient that plays a script, so no test calls a real model.
type scriptedChat struct {
	mu       sync.Mutex
	pieces   []string
	finish   string
	gate     chan struct{} // if set, the stream pauses after the first piece until released or cancelled
	onStream func()        // called when a stream begins, before any text
	requests []service.ChatRequest
}

func (c *scriptedChat) configure(f func(*scriptedChat)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f(c)
}

func (c *scriptedChat) requestCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.requests)
}

func (c *scriptedChat) lastRequest() service.ChatRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.requests[len(c.requests)-1]
}

func (c *scriptedChat) Complete(context.Context, service.ChatRequest) (service.ChatResponse, error) {
	return service.ChatResponse{}, fmt.Errorf("not used")
}

func (c *scriptedChat) Stream(ctx context.Context, req service.ChatRequest, onText func(string) error) (service.ChatResponse, error) {
	c.mu.Lock()
	c.requests = append(c.requests, req)
	pieces, finish, gate, onStream := c.pieces, c.finish, c.gate, c.onStream
	c.mu.Unlock()

	if onStream != nil {
		onStream()
	}
	var got strings.Builder
	partial := func() service.ChatResponse { return service.ChatResponse{Model: req.Model, Content: got.String()} }
	for i, p := range pieces {
		if err := onText(p); err != nil {
			return partial(), fmt.Errorf("llm: stream stopped by the caller: %w", err)
		}
		got.WriteString(p)
		if i == 0 && gate != nil {
			select {
			case <-gate:
			case <-ctx.Done():
				return partial(), ctx.Err()
			}
		}
	}
	if finish == "" {
		finish = "stop"
	}
	return service.ChatResponse{
		Model: req.Model, Content: got.String(), FinishReason: finish,
		Usage: service.TokenUsage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120},
	}, nil
}

type sse struct{ Name, Data string }

func parseSSE(body string) []sse {
	var out []sse
	for _, block := range strings.Split(body, "\n\n") {
		var ev sse
		for _, line := range strings.Split(strings.Trim(block, "\n"), "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.Name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.Data = strings.TrimPrefix(line, "data: ")
			}
		}
		if ev.Name != "" {
			out = append(out, ev)
		}
	}
	return out
}

func names(events []sse) string {
	n := make([]string, len(events))
	for i, e := range events {
		n[i] = e.Name
	}
	return strings.Join(n, ",")
}

func tokens(events []sse) string {
	var b strings.Builder
	for _, e := range events {
		if e.Name == "token" {
			var p struct{ Text string }
			_ = json.Unmarshal([]byte(e.Data), &p)
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// ask runs a question through the whole router and returns the recorded response.
func (s *stack) ask(t *testing.T, token, docID, question string) (*httptest.ResponseRecorder, []sse) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"question": question})
	req := httptest.NewRequest(http.MethodPost, "/api/documents/"+docID+"/ask", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.handler.ServeHTTP(w, req)
	return w, parseSSE(w.Body.String())
}

func (s *stack) readyDoc(t *testing.T, token string) string {
	t.Helper()
	res := s.upload(t, token, "sample.txt", fixture(t, "sample.txt"))
	if res.Code != http.StatusAccepted {
		t.Fatalf("upload: status %d: %s", res.Code, res.Body)
	}
	doc := decodeDoc(t, res)
	if got := s.waitSettled(t, token, doc.ID); got.Status != "ready" {
		t.Fatalf("document %s ended as %q", doc.ID, got.Status)
	}
	return doc.ID
}

type messageJSON struct {
	ID      int64  `json:"id"`
	Role    string `json:"role"`
	Content string `json:"content"`
	Status  string `json:"status"`
	Sources []int  `json:"sources"`
}

type messagesPage struct {
	Messages   []messageJSON `json:"messages"`
	HasMore    bool          `json:"has_more"`
	NextBefore int64         `json:"next_before"`
}

func (s *stack) messages(t *testing.T, token, docID, query string) (int, messagesPage, string) {
	t.Helper()
	res := s.do(t, http.MethodGet, "/api/documents/"+docID+"/messages"+query, token, nil, "")
	var page messagesPage
	if res.Code == http.StatusOK {
		if err := json.Unmarshal(res.Data, &page); err != nil {
			t.Fatalf("decode messages: %v\n%s", err, res.Body)
		}
	}
	return res.Code, page, res.Body
}

// --- tests ---

func TestAskAPI_StreamsAnAnswerAndStoresTheConversation(t *testing.T) {
	s := newStack(t, 10<<20)
	docID := s.readyDoc(t, s.alice)

	savedWhenTheModelWasCalled := -1
	s.chat.configure(func(c *scriptedChat) {
		c.onStream = func() {
			_ = s.pool.QueryRow(context.Background(), `SELECT count(*)::int FROM messages`).Scan(&savedWhenTheModelWasCalled)
		}
	})

	w, events := s.ask(t, s.alice, docID, "What is a zebrafish?")
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d, content type %q: %s", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
	if names(events) != "sources,token,token,done" {
		t.Fatalf("events = %s", names(events))
	}

	var sources struct {
		Sources []struct {
			Label      string `json:"label"`
			ChunkIndex int    `json:"chunk_index"`
			Preview    string `json:"preview"`
		} `json:"sources"`
	}
	if err := json.Unmarshal([]byte(events[0].Data), &sources); err != nil || len(sources.Sources) == 0 ||
		sources.Sources[0].Label != "P1" || !strings.Contains(sources.Sources[0].Preview, "zebrafish") {
		t.Errorf("sources = %+v (%v)", sources, err)
	}
	answer := tokens(events)
	if answer != "The zebrafish is a small freshwater fish [P1]." {
		t.Errorf("answer = %q", answer)
	}
	var done struct {
		MessageID int64  `json:"message_id"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal([]byte(events[3].Data), &done); err != nil || done.MessageID <= 0 || done.Status != "complete" {
		t.Errorf("done = %+v (%v)", done, err)
	}

	// The question was in the database before the model was called.
	if savedWhenTheModelWasCalled != 1 {
		t.Errorf("%d messages existed when the model was called, want 1 (the question)", savedWhenTheModelWasCalled)
	}
	if req := s.chat.lastRequest(); !strings.Contains(req.Messages[len(req.Messages)-1].Content, "<passage id=\"P1\">") {
		t.Errorf("the model was not given the retrieved passage")
	}

	// The conversation can be read back: the question, then the answer, oldest first.
	code, page, body := s.messages(t, s.alice, docID, "")
	if code != http.StatusOK || len(page.Messages) != 2 || page.HasMore {
		t.Fatalf("messages: status %d, %+v", code, page)
	}
	q, a := page.Messages[0], page.Messages[1]
	if q.Role != "user" || q.Content != "What is a zebrafish?" || len(q.Sources) != 0 ||
		a.Role != "assistant" || a.Content != answer || a.Status != "complete" || len(a.Sources) != 1 || a.ID != done.MessageID {
		t.Errorf("messages = %+v", page.Messages)
	}
	for _, leaked := range []string{"model", "prompt_tokens", "completion_tokens", "test-model"} {
		if strings.Contains(body, leaked) {
			t.Errorf("the messages response leaks %q", leaked)
		}
	}
}

func TestAskAPI_NoPassagesAnswersWithoutCallingTheModel(t *testing.T) {
	s := newStack(t, 10<<20)
	docID := s.readyDoc(t, s.alice)

	w, events := s.ask(t, s.alice, docID, "Tell me about xylophones and quantum chromodynamics")
	if w.Code != http.StatusOK || names(events) != "sources,token,done" {
		t.Fatalf("status %d, events %s", w.Code, names(events))
	}
	if !strings.Contains(events[0].Data, `"sources":[]`) || tokens(events) != service.NotFoundAnswer {
		t.Errorf("sources %s, answer %q", events[0].Data, tokens(events))
	}
	if s.chat.requestCount() != 0 {
		t.Error("the model was called although nothing matched")
	}
	if _, page, _ := s.messages(t, s.alice, docID, ""); len(page.Messages) != 2 || page.Messages[1].Content != service.NotFoundAnswer {
		t.Errorf("messages = %+v", page.Messages)
	}
}

func TestAskAPI_OwnershipAndRefusals(t *testing.T) {
	s := newStack(t, 10<<20)
	docID := s.readyDoc(t, s.alice)

	errCode := func(w *httptest.ResponseRecorder) string {
		var env struct {
			Error *struct{ Code string } `json:"error"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &env)
		if env.Error == nil {
			return ""
		}
		return env.Error.Code
	}

	tests := []struct {
		name     string
		token    string
		doc      string
		question string
		status   int
		code     string
	}{
		{"another user's document", s.bob, docID, "What is a zebrafish?", 404, "NOT_FOUND"},
		{"a malformed document id", s.alice, "not-a-uuid", "What is a zebrafish?", 404, "NOT_FOUND"},
		{"a document that does not exist", s.alice, "6f1c0c8e-2f4b-4f55-9d3e-0b1a2c3d4e5f", "What is a zebrafish?", 404, "NOT_FOUND"},
		{"no token", "", docID, "What is a zebrafish?", 401, "UNAUTHORIZED"},
		{"an empty question", s.alice, docID, "   ", 400, "VALIDATION_ERROR"},
		{"a question that is too long", s.alice, docID, strings.Repeat("a", 1001), 400, "VALIDATION_ERROR"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w, events := s.ask(t, tc.token, tc.doc, tc.question)
			if w.Code != tc.status || errCode(w) != tc.code || len(events) != 0 {
				t.Errorf("status %d, code %q, events %d; want %d %s: %s", w.Code, errCode(w), len(events), tc.status, tc.code, w.Body.String())
			}
		})
	}

	// None of it left a trace, and the model was never involved.
	if n := s.count(t, `SELECT count(*) FROM messages`); n != 0 {
		t.Errorf("%d messages were stored by refused questions", n)
	}
	if s.chat.requestCount() != 0 {
		t.Error("the model was called for a refused question")
	}

	// Bob cannot read Alice's conversation either.
	_, _ = s.ask(t, s.alice, docID, "What is a zebrafish?")
	if code, _, _ := s.messages(t, s.bob, docID, ""); code != http.StatusNotFound {
		t.Errorf("Bob reading Alice's messages: status %d, want 404", code)
	}
	if code, page, _ := s.messages(t, s.alice, docID, ""); code != http.StatusOK || len(page.Messages) != 2 {
		t.Errorf("Alice's messages: status %d, %+v", code, page)
	}
}

func TestAskAPI_DocumentNotReady(t *testing.T) {
	s := newStackWith(t, 10<<20, 50, false) // workers never start: the document stays "processing"
	res := s.upload(t, s.alice, "sample.txt", fixture(t, "sample.txt"))
	doc := decodeDoc(t, res)

	w, _ := s.ask(t, s.alice, doc.ID, "What is a zebrafish?")
	var env struct {
		Error *struct {
			Code    string          `json:"code"`
			Details json.RawMessage `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusConflict || env.Error == nil || env.Error.Code != "DOCUMENT_NOT_READY" ||
		!strings.Contains(string(env.Error.Details), `"processing"`) {
		t.Errorf("status %d: %s", w.Code, w.Body.String())
	}
	if s.chat.requestCount() != 0 {
		t.Error("the model was called for a document that is not ready")
	}
}

func TestAskAPI_SecondQuestionWhileStreamingIsRefused(t *testing.T) {
	s := newStack(t, 10<<20)
	docID := s.readyDoc(t, s.alice)
	gate := make(chan struct{})
	s.chat.configure(func(c *scriptedChat) { c.gate = gate })

	srv := httptest.NewServer(s.handler)
	defer srv.Close()

	// The first question: read until its first token has arrived, so the answer is mid-stream.
	body, _ := json.Marshal(map[string]string{"question": "What is a zebrafish?"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/api/documents/"+docID+"/ask", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.alice)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rd := bufio.NewReader(resp.Body)
	for seen := ""; seen != "token"; {
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("the first stream ended early: %v", err)
		}
		if strings.HasPrefix(line, "event: ") {
			seen = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
		}
	}

	// The second question, while the first is still being answered.
	w, events := s.ask(t, s.alice, docID, "Another question about the zebrafish")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "ANSWER_IN_PROGRESS") || len(events) != 0 {
		t.Errorf("second question: status %d: %s", w.Code, w.Body.String())
	}
	if n := s.chat.requestCount(); n != 1 {
		t.Errorf("the model was called %d times", n)
	}

	close(gate) // let the first answer finish, and read the stream to its end
	for {
		if _, err := rd.ReadString('\n'); err != nil {
			break
		}
	}

	// The conversation is free again.
	if w, events := s.ask(t, s.alice, docID, "And a third question"); w.Code != http.StatusOK || !strings.HasSuffix(names(events), "done") {
		t.Errorf("third question: status %d, events %s", w.Code, names(events))
	}
}

func TestAskAPI_ClientDisconnectKeepsThePartialAnswer(t *testing.T) {
	s := newStack(t, 10<<20)
	docID := s.readyDoc(t, s.alice)
	s.chat.configure(func(c *scriptedChat) { c.gate = make(chan struct{}) }) // never released

	srv := httptest.NewServer(s.handler)
	defer srv.Close()

	body, _ := json.Marshal(map[string]string{"question": "What is a zebrafish?"})
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/api/documents/"+docID+"/ask", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.alice)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	rd := bufio.NewReader(resp.Body)
	for seen := ""; seen != "token"; {
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("the stream ended early: %v", err)
		}
		if strings.HasPrefix(line, "event: ") {
			seen = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
		}
	}
	cancel() // the user closes the tab after the first piece of text
	resp.Body.Close()

	// The server notices, stops the model call, and still saves what was received.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var status, content string
		err := s.pool.QueryRow(context.Background(),
			`SELECT status, content FROM messages WHERE role = 'assistant'`).Scan(&status, &content)
		if err == nil {
			if status != "partial" || content != "The zebrafish is a small freshwater fish " {
				t.Fatalf("stored answer: status %q, content %q; want the partial text the client received", status, content)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the partial answer was never saved after the client disconnected")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestMessagesAPI_Paging(t *testing.T) {
	s := newStack(t, 10<<20)
	docID := s.readyDoc(t, s.alice)
	otherDoc := s.readyDoc(t, s.alice)

	for i := 1; i <= 3; i++ {
		if w, events := s.ask(t, s.alice, docID, fmt.Sprintf("Question %d about the zebrafish?", i)); w.Code != 200 || !strings.HasSuffix(names(events), "done") {
			t.Fatalf("question %d: status %d, events %s", i, w.Code, names(events))
		}
	}

	_, first, _ := s.messages(t, s.alice, docID, "?limit=4") // the newest four of six
	if len(first.Messages) != 4 || !first.HasMore || first.NextBefore != first.Messages[0].ID {
		t.Fatalf("first page = %+v", first)
	}
	if first.Messages[3].Role != "assistant" || !strings.Contains(first.Messages[3].Content, "zebrafish") ||
		!strings.Contains(first.Messages[0].Content, "Question 2") {
		t.Errorf("first page messages = %+v", first.Messages)
	}

	_, second, _ := s.messages(t, s.alice, docID, fmt.Sprintf("?limit=4&before=%d", first.NextBefore))
	if len(second.Messages) != 2 || second.HasMore || second.Messages[0].Content != "Question 1 about the zebrafish?" {
		t.Errorf("second page = %+v", second)
	}

	// Another document of the same user has its own, empty conversation.
	if code, page, body := s.messages(t, s.alice, otherDoc, ""); code != 200 || len(page.Messages) != 0 || !strings.Contains(body, `"messages":[]`) {
		t.Errorf("other document: status %d, %+v", code, page)
	}
	for _, bad := range []string{"?limit=0", "?limit=abc", "?before=-1"} {
		if code, _, _ := s.messages(t, s.alice, docID, bad); code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", bad, code)
		}
	}
	if code, _, _ := s.messages(t, s.alice, "not-a-uuid", ""); code != http.StatusNotFound {
		t.Errorf("malformed id: status %d, want 404", code)
	}
}