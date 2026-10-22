package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/models"
)

// --- fakes ---

type askDocsFake struct {
	doc *models.Document
	err error

	calls int
}

func (f *askDocsFake) Get(_ context.Context, userID, id uuid.UUID) (*models.Document, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if f.doc == nil || f.doc.ID != id || f.doc.UserID != userID {
		return nil, models.ErrNotFound
	}
	cp := *f.doc
	return &cp, nil
}

type askRetrieverFake struct {
	res *Retrieval
	err error

	calls       int
	gotQuestion string
}

func (f *askRetrieverFake) Retrieve(_ context.Context, _, _ uuid.UUID, question string) (*Retrieval, error) {
	f.calls++
	f.gotQuestion = question
	return f.res, f.err
}

// askChat is a scripted ChatClient.
type askChat struct {
	mu sync.Mutex

	pieces    []string
	finish    string // default "stop"
	usage     TokenUsage
	respModel string
	err       error // returned after the pieces

	block      bool          // wait before piece number blockAfter
	blockAfter int
	release    chan struct{} // releases a blocked stream

	onStart   func()        // called when Stream begins, before any text
	startedCh chan struct{} // closed when the first Stream begins
	startOnce sync.Once

	requests []ChatRequest
}

func (c *askChat) Complete(context.Context, ChatRequest) (ChatResponse, error) {
	return ChatResponse{}, errors.New("not used")
}

func (c *askChat) Stream(ctx context.Context, req ChatRequest, onText func(string) error) (ChatResponse, error) {
	c.mu.Lock()
	c.requests = append(c.requests, req)
	onStart := c.onStart
	c.mu.Unlock()

	if onStart != nil {
		onStart()
	}
	if c.startedCh != nil {
		c.startOnce.Do(func() { close(c.startedCh) })
	}

	var got strings.Builder
	partial := func() ChatResponse { return ChatResponse{Model: c.respModel, Content: got.String(), Usage: c.usage} }
	wait := func() error {
		select {
		case <-c.release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	for i, p := range c.pieces {
		if c.block && i == c.blockAfter {
			if err := wait(); err != nil {
				return partial(), err
			}
		}
		if err := onText(p); err != nil {
			return partial(), fmt.Errorf("llm: stream stopped by the caller: %w", err)
		}
		got.WriteString(p)
	}
	if c.block && c.blockAfter >= len(c.pieces) {
		if err := wait(); err != nil {
			return partial(), err
		}
	}
	if c.err != nil {
		return partial(), c.err
	}
	r := partial()
	r.FinishReason = c.finish
	if r.FinishReason == "" {
		r.FinishReason = "stop"
	}
	return r, nil
}

func (c *askChat) requestCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.requests)
}

func addedMessages(f *fakeConvStore) []addCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]addCall(nil), f.added...)
}

// --- harness ---

type askHarness struct {
	svc   *AskService
	docs  *askDocsFake
	retr  *askRetrieverFake
	chat  *askChat
	store *fakeConvStore
	user  uuid.UUID
	doc   uuid.UUID
}

var askTestSettings = AskSettings{Model: "test-model", MaxOutputTokens: 1500, ContextBudget: 3000, MaxQuestionChars: 1000}

func newAskHarness(t *testing.T) *askHarness {
	t.Helper()
	return newAskHarnessWith(t, context.Background(), true)
}

func newAskHarnessWith(t *testing.T, shutdown context.Context, withLLM bool) *askHarness {
	t.Helper()

	user, docID := uuid.New(), uuid.New()
	h := &askHarness{
		user: user, doc: docID,
		docs: &askDocsFake{doc: &models.Document{ID: docID, UserID: user, Status: models.StatusReady}},
		retr: &askRetrieverFake{res: &Retrieval{
			Terms: []string{"dataset", "problem"},
			Chunks: []models.RetrievedChunk{
				{Index: 12, Content: "The dataset has 83 problems from seven contests.", Score: 5, MatchedTerms: 2},
				{Index: 4, Content: "Gold editorials  improve\nresults.", Score: 3, MatchedTerms: 1},
			},
		}},
		chat:  &askChat{pieces: []string{"The dataset ", "has 83 problems [P1]."}, usage: TokenUsage{PromptTokens: 1200, CompletionTokens: 40, TotalTokens: 1240}, respModel: "test-model-resolved"},
		store: &fakeConvStore{conv: &models.Conversation{ID: uuid.New(), UserID: user, DocumentID: docID}},
	}

	deps := AskDeps{Documents: h.docs, Retriever: h.retr, Conversations: NewConversationService(h.store), Shutdown: shutdown}
	if withLLM {
		deps.LLM = h.chat
	}
	svc, err := NewAskService(deps, askTestSettings)
	if err != nil {
		t.Fatal(err)
	}
	h.svc = svc
	return h
}

func (h *askHarness) askWith(ctx context.Context, question string, emit func(AskEvent) error) error {
	return h.svc.Ask(ctx, AskRequest{UserID: h.user, DocumentID: h.doc, Question: question, RequestID: "req-1"}, emit)
}

// ask runs a question and collects every event.
func (h *askHarness) ask(question string) ([]AskEvent, error) {
	var mu sync.Mutex
	var evs []AskEvent
	err := h.askWith(context.Background(), question, func(e AskEvent) error {
		mu.Lock()
		evs = append(evs, e)
		mu.Unlock()
		return nil
	})
	return evs, err
}

func kinds(evs []AskEvent) []AskEventKind {
	out := make([]AskEventKind, len(evs))
	for i, e := range evs {
		out[i] = e.Kind
	}
	return out
}

func tokenText(evs []AskEvent) string {
	var b strings.Builder
	for _, e := range evs {
		if e.Kind == EventToken {
			b.WriteString(e.Text)
		}
	}
	return b.String()
}

func lastEvent(t *testing.T, evs []AskEvent) AskEvent {
	t.Helper()
	if len(evs) == 0 {
		t.Fatal("no events were emitted")
	}
	return evs[len(evs)-1]
}

// --- the normal path ---

func TestAsk_StreamsAnAnswerAndSavesTheConversation(t *testing.T) {
	h := newAskHarness(t)
	var savedBeforeTheModel []addCall
	h.chat.onStart = func() { savedBeforeTheModel = addedMessages(h.store) }

	evs, err := h.ask("  How many problems are in the dataset?  ")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	if want := []AskEventKind{EventSources, EventToken, EventToken, EventDone}; !slices.Equal(kinds(evs), want) {
		t.Fatalf("events = %v, want %v", kinds(evs), want)
	}

	src := evs[0].Sources
	if len(src) != 2 || src[0].Label != "P1" || src[0].ChunkIndex != 12 || src[1].Label != "P2" || src[1].ChunkIndex != 4 {
		t.Fatalf("sources = %+v", src)
	}
	if src[0].Preview != "The dataset has 83 problems from seven contests." || src[1].Preview != "Gold editorials improve results." {
		t.Errorf("previews = %q and %q (whitespace must be flattened)", src[0].Preview, src[1].Preview)
	}

	done := lastEvent(t, evs)
	if done.MessageID <= 0 || done.Status != models.MessageComplete || done.FinishReason != "stop" {
		t.Errorf("done event = %+v", done)
	}
	if tokenText(evs) != "The dataset has 83 problems [P1]." {
		t.Errorf("tokens = %q", tokenText(evs))
	}

	// The question was saved BEFORE the model was called.
	if len(savedBeforeTheModel) != 1 || savedBeforeTheModel[0].msg.Role != models.RoleUser ||
		savedBeforeTheModel[0].msg.Content != "How many problems are in the dataset?" {
		t.Errorf("messages saved when the model was called = %+v", savedBeforeTheModel)
	}

	// Then the answer, with sources, the model that actually answered, and usage.
	saved := addedMessages(h.store)
	if len(saved) != 2 {
		t.Fatalf("%d messages saved, want 2", len(saved))
	}
	ans := saved[1]
	if ans.userID != h.user || ans.convID != h.store.conv.ID {
		t.Errorf("the answer was stored for user %v, conversation %v", ans.userID, ans.convID)
	}
	m := ans.msg
	if m.Role != models.RoleAssistant || m.Status != models.MessageComplete || m.Content != "The dataset has 83 problems [P1]." ||
		!slices.Equal(m.Sources, []int{12, 4}) || m.Model != "test-model-resolved" ||
		m.PromptTokens == nil || *m.PromptTokens != 1200 || m.CompletionTokens == nil || *m.CompletionTokens != 40 {
		t.Errorf("stored answer = %+v", m)
	}

	// What the model was sent.
	if h.chat.requestCount() != 1 {
		t.Fatalf("the model was called %d times", h.chat.requestCount())
	}
	req := h.chat.requests[0]
	last := req.Messages[len(req.Messages)-1]
	if req.Model != "test-model" || req.ReasoningEffort != "low" || last.Role != "user" ||
		!strings.Contains(last.Content, `<passage id="P1">`) || !strings.Contains(last.Content, "83 problems") {
		t.Errorf("model request = %+v", req)
	}
	if h.retr.gotQuestion != "How many problems are in the dataset?" {
		t.Errorf("retrieval received %q", h.retr.gotQuestion)
	}
}

func TestAsk_CleansTheQuestionBeforeSavingIt(t *testing.T) {
	h := newAskHarness(t)
	if _, err := h.ask("  What\x00 is\r\nthis?  "); err != nil {
		t.Fatal(err)
	}
	if got := addedMessages(h.store)[0].msg.Content; got != "What is\nthis?" {
		t.Errorf("stored question = %q", got)
	}
}

func TestAsk_HistoryIsUsedAndOptional(t *testing.T) {
	t.Run("earlier exchanges reach the prompt", func(t *testing.T) {
		h := newAskHarness(t)
		h.store.page = &models.MessagePage{Messages: []models.Message{
			{Role: models.RoleUser, Status: models.MessageComplete, Content: "What is the dataset?"},
			{Role: models.RoleAssistant, Status: models.MessageComplete, Content: "An ICPC-style set [P1]."},
		}}
		if _, err := h.ask("How big is it?"); err != nil {
			t.Fatal(err)
		}
		msgs := h.chat.requests[0].Messages
		if len(msgs) != 4 || msgs[1].Content != "What is the dataset?" || msgs[2].Content != "An ICPC-style set." {
			t.Errorf("messages = %+v: want system, the earlier exchange (citations stripped), the new question", msgs)
		}
	})

	t.Run("a history failure does not stop the answer", func(t *testing.T) {
		h := newAskHarness(t)
		h.store.listErr = errors.New("database hiccup")
		evs, err := h.ask("How big is it?")
		if err != nil || lastEvent(t, evs).Kind != EventDone {
			t.Fatalf("got (%v, %v), want a normal answer", kinds(evs), err)
		}
		if n := len(h.chat.requests[0].Messages); n != 2 {
			t.Errorf("%d messages in the prompt, want 2 (no history)", n)
		}
	})
}

// --- refusals: nothing is emitted, saved or sent to the model ---

func TestAsk_RefusalsHappenBeforeAnythingElse(t *testing.T) {
	notReady := func(s models.DocumentStatus) func(*askHarness) {
		return func(h *askHarness) { h.docs.doc.Status = s }
	}

	tests := []struct {
		name     string
		question string
		setup    func(*askHarness)
		noLLM    bool
		check    func(error) bool
	}{
		{"empty question", "", nil, false, func(e error) bool { return errors.Is(e, ErrEmptyQuestion) }},
		{"blank question", " \n\t ", nil, false, func(e error) bool { return errors.Is(e, ErrEmptyQuestion) }},
		{"only control characters", "\x00\x07", nil, false, func(e error) bool { return errors.Is(e, ErrEmptyQuestion) }},
		{"question too long", strings.Repeat("a", 1001), nil, false, func(e error) bool { return errors.Is(e, ErrQuestionTooLong) }},
		{"no API key", "a question", nil, true, func(e error) bool { return errors.Is(e, ErrLLMUnavailable) }},
		{"document missing", "a question", func(h *askHarness) { h.docs.doc = nil }, false, func(e error) bool { return errors.Is(e, models.ErrNotFound) }},
		{"another user's document", "a question", func(h *askHarness) { h.docs.doc.UserID = uuid.New() }, false, func(e error) bool { return errors.Is(e, models.ErrNotFound) }},
		{"document still processing", "a question", notReady(models.StatusProcessing), false, func(e error) bool {
			var nr *DocumentNotReadyError
			return errors.As(e, &nr) && nr.Status == models.StatusProcessing
		}},
		{"document failed", "a question", notReady(models.StatusFailed), false, func(e error) bool {
			var nr *DocumentNotReadyError
			return errors.As(e, &nr) && nr.Status == models.StatusFailed
		}},
		{"conversation cannot be opened", "a question", func(h *askHarness) { h.store.getErr = models.ErrNotFound }, false, func(e error) bool { return errors.Is(e, models.ErrNotFound) }},
		{"retrieval says the document is gone", "a question", func(h *askHarness) { h.retr.err = models.ErrNotFound }, false, func(e error) bool { return errors.Is(e, models.ErrNotFound) }},
		{"retrieval fails", "a question", func(h *askHarness) { h.retr.err = errors.New("database is down") }, false, func(e error) bool { return e != nil }},
		{"the question cannot be saved", "a question", func(h *askHarness) { h.store.addErr = errors.New("database is down") }, false, func(e error) bool { return e != nil }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newAskHarnessWith(t, context.Background(), !tc.noLLM)
			if tc.setup != nil {
				tc.setup(h)
			}
			evs, err := h.ask(tc.question)
			if err == nil || !tc.check(err) {
				t.Fatalf("err = %v", err)
			}
			if len(evs) != 0 {
				t.Errorf("events %v were emitted before an error: the HTTP status could no longer be set", kinds(evs))
			}
			if h.chat.requestCount() != 0 {
				t.Error("the model was called")
			}
			for _, a := range addedMessages(h.store) {
				if a.msg.Role == models.RoleAssistant {
					t.Errorf("an answer was saved for a refused question: %+v", a.msg)
				}
			}
		})
	}

	// Validation must not even look at the document.
	h := newAskHarness(t)
	_, _ = h.ask("")
	if h.docs.calls != 0 {
		t.Error("an invalid question must be rejected before the document is looked up")
	}
}

// --- nothing matched ---

func TestAsk_NoPassagesAnswersWithoutCallingTheModel(t *testing.T) {
	h := newAskHarness(t)
	h.retr.res = &Retrieval{Terms: []string{"xylophone"}, Chunks: []models.RetrievedChunk{}}

	evs, err := h.ask("Tell me about xylophones")
	if err != nil {
		t.Fatal(err)
	}
	if want := []AskEventKind{EventSources, EventToken, EventDone}; !slices.Equal(kinds(evs), want) {
		t.Fatalf("events = %v, want %v", kinds(evs), want)
	}
	if evs[0].Sources == nil || len(evs[0].Sources) != 0 {
		t.Errorf("sources = %#v, want an empty non-nil slice", evs[0].Sources)
	}
	if evs[1].Text != NotFoundAnswer {
		t.Errorf("answer = %q", evs[1].Text)
	}
	if h.chat.requestCount() != 0 {
		t.Error("the model was called although nothing matched")
	}

	saved := addedMessages(h.store)
	if len(saved) != 2 || saved[0].msg.Role != models.RoleUser || saved[1].msg.Role != models.RoleAssistant {
		t.Fatalf("saved %+v", saved)
	}
	a := saved[1].msg
	if a.Content != NotFoundAnswer || a.Status != models.MessageComplete || a.Sources == nil || len(a.Sources) != 0 ||
		a.Model != "" || a.PromptTokens != nil {
		t.Errorf("stored answer = %+v", a)
	}
}

func TestAsk_BlankChunksCountAsNothingFound(t *testing.T) {
	h := newAskHarness(t)
	h.retr.res = &Retrieval{Terms: []string{"x"}, Chunks: []models.RetrievedChunk{{Index: 1, Content: "  \n "}}}

	evs, err := h.ask("a question")
	if err != nil || h.chat.requestCount() != 0 || tokenText(evs) != NotFoundAnswer {
		t.Errorf("got (%v, %v), model calls %d", kinds(evs), err, h.chat.requestCount())
	}
}

// --- failures after streaming began ---

func TestAsk_FailuresBecomeErrorEvents(t *testing.T) {
	boom := errors.New("pq: connection to 10.0.0.5 refused")

	tests := []struct {
		name       string
		pieces     []string
		err        error
		wantCode   string
		wantStatus models.MessageStatus
		wantText   string
		wantRetry  time.Duration
	}{
		{"provider unavailable after some text", []string{"Part"}, &LLMError{Kind: LLMUnavailable, Status: 503, Message: "secret provider text"},
			AskCodeUnavailable, models.MessagePartial, "Part", 0},
		{"busy before any text", nil, &LLMError{Kind: LLMBusy, Status: 429, RetryAfter: 20 * time.Second},
			AskCodeBusy, models.MessageError, "", 20 * time.Second},
		{"our pacer refuses", nil, &LLMError{Kind: LLMBusy, RetryAfter: 45 * time.Second},
			AskCodeBusy, models.MessageError, "", 45 * time.Second},
		{"timeout", nil, &LLMError{Kind: LLMTimeout}, AskCodeTimeout, models.MessageError, "", 0},
		{"deadline", nil, context.DeadlineExceeded, AskCodeTimeout, models.MessageError, "", 0},
		{"our key is wrong", nil, &LLMError{Kind: LLMMisconfigured, Status: 401, Code: "invalid_api_key", Message: "Invalid API Key"},
			AskCodeError, models.MessageError, "", 0},
		{"our request is wrong", []string{"x"}, &LLMError{Kind: LLMBadRequest, Status: 400}, AskCodeError, models.MessagePartial, "x", 0},
		{"an unknown error", nil, boom, AskCodeInternal, models.MessageError, "", 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newAskHarness(t)
			h.chat.pieces, h.chat.err = tc.pieces, tc.err

			evs, err := h.ask("a question")
			if err != nil {
				t.Fatalf("Ask returned %v: after streaming began failures must be events", err)
			}

			e := lastEvent(t, evs)
			if e.Kind != EventError || e.Code != tc.wantCode || e.RetryAfter != tc.wantRetry || e.MessageID <= 0 {
				t.Fatalf("last event = %+v", e)
			}
			for _, secret := range []string{"secret provider text", "Invalid API Key", "10.0.0.5", "401"} {
				if strings.Contains(e.Message, secret) {
					t.Errorf("the event message leaks %q: %q", secret, e.Message)
				}
			}
			for _, ev := range evs {
				if ev.Kind == EventDone {
					t.Error("a failed answer must not end with a done event")
				}
			}

			saved := addedMessages(h.store)
			ans := saved[len(saved)-1].msg
			if ans.Status != tc.wantStatus || ans.Content != tc.wantText {
				t.Errorf("stored answer = status %q, content %q; want %q and %q", ans.Status, ans.Content, tc.wantStatus, tc.wantText)
			}
		})
	}
}

func TestAsk_LengthFinishIsPartialButDone(t *testing.T) {
	h := newAskHarness(t)
	h.chat.finish = "length"

	evs, err := h.ask("a question")
	if err != nil {
		t.Fatal(err)
	}
	done := lastEvent(t, evs)
	if done.Kind != EventDone || done.Status != models.MessagePartial || done.FinishReason != "length" {
		t.Errorf("last event = %+v, want done with status partial and finish length", done)
	}
	saved := addedMessages(h.store)
	if got := saved[len(saved)-1].msg.Status; got != models.MessagePartial {
		t.Errorf("stored status = %q, want partial", got)
	}
}

func TestAsk_AnEmptyAnswerIsAnErrorNotADone(t *testing.T) {
	h := newAskHarness(t)
	h.chat.pieces = nil // the model "finishes" without saying anything

	evs, err := h.ask("a question")
	if err != nil {
		t.Fatal(err)
	}
	e := lastEvent(t, evs)
	if e.Kind != EventError || e.Code != AskCodeError {
		t.Errorf("last event = %+v", e)
	}
	saved := addedMessages(h.store)
	if got := saved[len(saved)-1].msg; got.Status != models.MessageError || got.Content != "" {
		t.Errorf("stored answer = %+v", got)
	}
}

func TestAsk_AnAnswerThatCannotBeSavedIsReported(t *testing.T) {
	h := newAskHarness(t)
	// The question is saved first; the database then fails before the answer.
	h.chat.onStart = func() { h.store.mu.Lock(); h.store.addErr = errors.New("database is down"); h.store.mu.Unlock() }

	evs, err := h.ask("a question")
	if err != nil {
		t.Fatal(err)
	}
	e := lastEvent(t, evs)
	if e.Kind != EventError || e.Code != AskCodeInternal || !strings.Contains(e.Message, "could not be saved") {
		t.Errorf("last event = %+v", e)
	}
	if tokenText(evs) == "" {
		t.Error("the user should still have received the text")
	}
}

// --- the client leaves ---

func TestAsk_ClientLeavesMidStream(t *testing.T) {
	h := newAskHarness(t)
	h.chat.pieces = []string{"one ", "two ", "three"}

	var delivered []string
	err := h.askWith(context.Background(), "a question", func(e AskEvent) error {
		if e.Kind == EventToken {
			delivered = append(delivered, e.Text)
			if len(delivered) == 2 {
				return errors.New("write: broken pipe")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Ask = %v: a client that left is not an error", err)
	}

	if len(delivered) != 2 {
		t.Errorf("delivered %v: nothing may be sent after a failed write", delivered)
	}
	saved := addedMessages(h.store)
	ans := saved[len(saved)-1].msg
	if ans.Status != models.MessagePartial || ans.Content != "one " {
		t.Errorf("stored answer = status %q, content %q; want the partial text the client actually received", ans.Status, ans.Content)
	}
}

func TestAsk_ClientLeavesBeforeTheFirstEvent(t *testing.T) {
	h := newAskHarness(t)

	err := h.askWith(context.Background(), "a question", func(AskEvent) error { return errors.New("write: broken pipe") })
	if err != nil {
		t.Fatalf("Ask = %v", err)
	}
	if h.chat.requestCount() != 0 {
		t.Error("the model must not be called for a client that is already gone")
	}
	saved := addedMessages(h.store)
	if len(saved) != 2 || saved[1].msg.Role != models.RoleAssistant || saved[1].msg.Status != models.MessagePartial || saved[1].msg.Content != "" {
		t.Errorf("saved %+v: want the question and an empty cut-short answer", saved)
	}
}

func TestAsk_RequestCancelledMidStreamStillSavesThePartialAnswer(t *testing.T) {
	h := newAskHarness(t)
	h.chat.pieces = []string{"first ", "second"}
	h.chat.block, h.chat.blockAfter = true, 1 // after the first piece, wait for the context

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var evs []AskEvent
	errCh := make(chan error, 1)
	go func() {
		errCh <- h.askWith(ctx, "a question", func(e AskEvent) error {
			evs = append(evs, e)
			if e.Kind == EventToken {
				go cancel() // the client disconnects right after the first piece
			}
			return nil
		})
	}()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Ask = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Ask did not stop after the request was cancelled")
	}

	saved := addedMessages(h.store)
	ans := saved[len(saved)-1]
	if ans.msg.Role != models.RoleAssistant || ans.msg.Status != models.MessagePartial || ans.msg.Content != "first " {
		t.Errorf("stored answer = %+v", ans.msg)
	}
	if ans.ctxErr != nil {
		t.Errorf("the answer was written with a cancelled context: %v", ans.ctxErr)
	}
	if last := lastEvent(t, evs); last.Kind == EventDone || last.Kind == EventError {
		t.Errorf("last event = %+v: a client that left is told nothing", last)
	}
}

// --- shutdown ---

func TestAsk_ShutdownStopsTheStreamAndTellsTheClient(t *testing.T) {
	shutdown, stopServer := context.WithCancel(context.Background())
	defer stopServer()
	h := newAskHarnessWith(t, shutdown, true)
	h.chat.pieces = []string{"first ", "second"}
	h.chat.block, h.chat.blockAfter = true, 1

	var mu sync.Mutex
	var evs []AskEvent
	errCh := make(chan error, 1)
	go func() {
		errCh <- h.askWith(context.Background(), "a question", func(e AskEvent) error {
			mu.Lock()
			evs = append(evs, e)
			mu.Unlock()
			if e.Kind == EventToken {
				go stopServer() // the server starts to stop while the answer is streaming
			}
			return nil
		})
	}()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Ask = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a shutdown must end a running stream")
	}

	mu.Lock()
	defer mu.Unlock()
	e := lastEvent(t, evs)
	if e.Kind != EventError || e.Code != AskCodeShuttingDown || e.MessageID <= 0 {
		t.Errorf("last event = %+v", e)
	}
	saved := addedMessages(h.store)
	if ans := saved[len(saved)-1].msg; ans.Status != models.MessagePartial || ans.Content != "first " {
		t.Errorf("stored answer = %+v", ans)
	}
}

// --- one answer at a time ---

func TestAsk_SecondQuestionInTheSameConversationIsRefused(t *testing.T) {
	h := newAskHarness(t)
	h.chat.pieces = []string{"first ", "second"}
	h.chat.block, h.chat.blockAfter = true, 1
	h.chat.release = make(chan struct{})
	h.chat.startedCh = make(chan struct{})

	firstDone := make(chan error, 1)
	go func() { _, err := h.ask("first question"); firstDone <- err }()

	select {
	case <-h.chat.startedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("the first answer never started")
	}

	before := len(addedMessages(h.store))
	evs, err := h.ask("second question")
	if !errors.Is(err, ErrAnswerInProgress) || len(evs) != 0 {
		t.Fatalf("got (%v, %v), want ErrAnswerInProgress and no events", kinds(evs), err)
	}
	if got := len(addedMessages(h.store)); got != before {
		t.Errorf("the refused question was saved (%d -> %d messages)", before, got)
	}
	if h.chat.requestCount() != 1 {
		t.Errorf("the model was called %d times", h.chat.requestCount())
	}

	close(h.chat.release) // let the first answer finish
	if err := <-firstDone; err != nil {
		t.Fatalf("first Ask: %v", err)
	}

	// The conversation is free again.
	h.chat.block = false
	if evs, err := h.ask("third question"); err != nil || lastEvent(t, evs).Kind != EventDone {
		t.Errorf("after the first answer: (%v, %v)", kinds(evs), err)
	}
}

func TestAsk_TheConversationIsFreeBeforeTheLastEventIsSent(t *testing.T) {
	h := newAskHarness(t)

	// A client that sends its next question the moment it sees "done".
	var nextErr error
	err := h.askWith(context.Background(), "first", func(e AskEvent) error {
		if e.Kind == EventDone {
			_, nextErr = h.ask("second, sent immediately")
		}
		return nil
	})
	if err != nil || nextErr != nil {
		t.Errorf("first Ask = %v, immediate second Ask = %v: the guard must be released before the last event", err, nextErr)
	}
}

func TestConversationGuard(t *testing.T) {
	g := newConversationGuard()
	a, b := uuid.New(), uuid.New()

	releaseA, ok := g.tryAcquire(a)
	if !ok {
		t.Fatal("a free conversation must be acquirable")
	}
	if _, ok := g.tryAcquire(a); ok {
		t.Error("a busy conversation must be refused")
	}
	releaseB, ok := g.tryAcquire(b)
	if !ok {
		t.Error("a different conversation must not be blocked")
	}

	releaseA()
	releaseA() // harmless
	if _, ok := g.tryAcquire(a); !ok {
		t.Error("a released conversation must be acquirable again")
	}
	releaseB()
}

// --- helpers ---

func TestStreamFailure(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code string
		busy time.Duration
	}{
		{"busy", &LLMError{Kind: LLMBusy, RetryAfter: 7 * time.Second}, AskCodeBusy, 7 * time.Second},
		{"timeout kind", &LLMError{Kind: LLMTimeout}, AskCodeTimeout, 0},
		{"unavailable", &LLMError{Kind: LLMUnavailable}, AskCodeUnavailable, 0},
		{"misconfigured", &LLMError{Kind: LLMMisconfigured}, AskCodeError, 0},
		{"bad request", &LLMError{Kind: LLMBadRequest}, AskCodeError, 0},
		{"bad response", &LLMError{Kind: LLMBadResponse}, AskCodeError, 0},
		{"wrapped", fmt.Errorf("ctx: %w", &LLMError{Kind: LLMBusy}), AskCodeBusy, 0},
		{"deadline", context.DeadlineExceeded, AskCodeTimeout, 0},
		{"anything else", io.ErrUnexpectedEOF, AskCodeInternal, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := streamFailure(tc.err)
			if f.code != tc.code || f.retryAfter != tc.busy || f.message == "" || strings.HasSuffix(f.message, ".") == false {
				t.Errorf("failure = %+v", f)
			}
		})
	}
}

func TestPreviewText(t *testing.T) {
	if got := previewText("  a   b\n c  ", 50); got != "a b c" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("আমি ", 100)
	if got := previewText(long, 10); []rune(got)[len([]rune(got))-1] != '…' || len([]rune(got)) != 11 {
		t.Errorf("got %q", got)
	}
}

func TestNewAskService_Validation(t *testing.T) {
	good := AskDeps{Documents: &askDocsFake{}, Retriever: &askRetrieverFake{}, Conversations: NewConversationService(&fakeConvStore{})}

	tests := map[string]func(*AskDeps, *AskSettings){
		"no documents":      func(d *AskDeps, _ *AskSettings) { d.Documents = nil },
		"no retriever":      func(d *AskDeps, _ *AskSettings) { d.Retriever = nil },
		"no conversations":  func(d *AskDeps, _ *AskSettings) { d.Conversations = nil },
		"no model":          func(_ *AskDeps, s *AskSettings) { s.Model = " " },
		"no output limit":   func(_ *AskDeps, s *AskSettings) { s.MaxOutputTokens = 0 },
		"budget too small":  func(_ *AskDeps, s *AskSettings) { s.ContextBudget = 10 },
		"no question limit": func(_ *AskDeps, s *AskSettings) { s.MaxQuestionChars = 0 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			d, s := good, askTestSettings
			mutate(&d, &s)
			if _, err := NewAskService(d, s); err == nil {
				t.Error("expected an error")
			}
		})
	}
	if _, err := NewAskService(good, askTestSettings); err != nil {
		t.Errorf("valid settings (and no LLM) must be accepted: %v", err)
	}
}