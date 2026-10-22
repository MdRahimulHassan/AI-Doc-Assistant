package handler

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/middleware"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/models"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/service"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/pkg/response"
)

// --- fakes and helpers (fakeParser comes from auth_handler_test.go) ---

type askFn func(ctx context.Context, req service.AskRequest, emit func(service.AskEvent) error) error

type fakeAsker struct {
	mu    sync.Mutex
	calls int
	got   service.AskRequest
	run   askFn
}

func (f *fakeAsker) Ask(ctx context.Context, req service.AskRequest, emit func(service.AskEvent) error) error {
	f.mu.Lock()
	f.calls++
	f.got = req
	run := f.run
	f.mu.Unlock()
	if run == nil {
		return nil
	}
	return run(ctx, req, emit)
}

func (f *fakeAsker) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type fakeLister struct {
	page *models.MessagePage
	err  error

	calls     int
	gotUser   uuid.UUID
	gotDoc    uuid.UUID
	gotLimit  int
	gotBefore int64
}

func (f *fakeLister) Messages(_ context.Context, userID, docID uuid.UUID, limit int, before int64) (*models.MessagePage, error) {
	f.calls++
	f.gotUser, f.gotDoc, f.gotLimit, f.gotBefore = userID, docID, limit, before
	return f.page, f.err
}

func newConvRouter(ask Asker, msgs MessageLister, user uuid.UUID, heartbeat time.Duration) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.RequestID())
	h := NewConversationHandler(ask, msgs).WithHeartbeat(heartbeat)
	g := r.Group("/api", middleware.Auth(fakeParser{id: user}))
	g.POST("/documents/:id/ask", h.Ask)
	g.GET("/documents/:id/messages", h.Messages)
	return r
}

func askPost(t *testing.T, h http.Handler, docID, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/documents/"+docID+"/ask", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

type sseEvent struct{ Name, Data string }

// parseSSE splits a stream into events and counts comment lines (heartbeats).
// It also checks the framing: one "event:" line and one "data:" line each.
func parseSSE(t *testing.T, body string) (events []sseEvent, comments int) {
	t.Helper()
	for _, block := range strings.Split(body, "\n\n") {
		block = strings.Trim(block, "\n")
		if block == "" {
			continue
		}
		var ev sseEvent
		dataLines := 0
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, ":"):
				comments++
			case strings.HasPrefix(line, "event: "):
				ev.Name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.Data = strings.TrimPrefix(line, "data: ")
				dataLines++
			default:
				t.Errorf("unexpected line in the stream: %q", line)
			}
		}
		if dataLines > 1 {
			t.Errorf("an event spans several data lines (a raw newline got in): %q", block)
		}
		if ev.Name != "" {
			events = append(events, ev)
		}
	}
	return events, comments
}

// readEvent reads one event from a live stream.
func readEvent(t *testing.T, r *bufio.Reader) sseEvent {
	t.Helper()
	var ev sseEvent
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("the stream ended while waiting for an event: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if ev.Name != "" {
				return ev
			}
		case strings.HasPrefix(line, "event: "):
			ev.Name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			ev.Data = strings.TrimPrefix(line, "data: ")
		}
	}
}

type rawEnvelope struct {
	Success bool `json:"success"`
	Error   *struct {
		Code    string          `json:"code"`
		Message string          `json:"message"`
		Details json.RawMessage `json:"details"`
	} `json:"error"`
}

func decodeError(t *testing.T, w *httptest.ResponseRecorder) rawEnvelope {
	t.Helper()
	var env rawEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("the response is not a JSON envelope: %v\n%s", err, w.Body.String())
	}
	return env
}

func emitAll(events ...service.AskEvent) askFn {
	return func(_ context.Context, _ service.AskRequest, emit func(service.AskEvent) error) error {
		for _, e := range events {
			if err := emit(e); err != nil {
				return nil
			}
		}
		return nil
	}
}

// --- wire format ---

func TestAsk_StreamHeadersAndEventFormat(t *testing.T) {
	user, doc := uuid.New(), uuid.New()
	hard := "line one\nline two \"quoted\" <b>tag</b> & 😀 আমি\ttab"

	fa := &fakeAsker{run: emitAll(
		service.AskEvent{Kind: service.EventSources, Sources: []service.SourceInfo{{Label: "P1", ChunkIndex: 12, Preview: "A preview", Truncated: true}}},
		service.AskEvent{Kind: service.EventToken, Text: hard},
		service.AskEvent{Kind: service.EventDone, MessageID: 77, Status: models.MessageComplete, FinishReason: "stop"},
	)}
	w := askPost(t, newConvRouter(fa, nil, user, 0), doc.String(), `{"question":"  What is this? "}`, "tok")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Errorf("Cache-Control = %q", cc)
	}
	if w.Header().Get("X-Accel-Buffering") != "no" {
		t.Errorf("X-Accel-Buffering = %q", w.Header().Get("X-Accel-Buffering"))
	}

	events, _ := parseSSE(t, w.Body.String())
	names := make([]string, len(events))
	for i, e := range events {
		names[i] = e.Name
	}
	if strings.Join(names, ",") != "sources,token,done" {
		t.Fatalf("events = %v", names)
	}

	var sources sourcesPayload
	if err := json.Unmarshal([]byte(events[0].Data), &sources); err != nil || len(sources.Sources) != 1 ||
		sources.Sources[0] != (sourcePayload{Label: "P1", ChunkIndex: 12, Preview: "A preview", Truncated: true}) {
		t.Errorf("sources = %+v (%v)", sources, err)
	}

	// A token with newlines, quotes, angle brackets and emoji survives intact on ONE line.
	var token tokenPayload
	if err := json.Unmarshal([]byte(events[1].Data), &token); err != nil || token.Text != hard {
		t.Errorf("token = %q (%v), want %q", token.Text, err, hard)
	}
	if strings.Contains(events[1].Data, "\n") || !strings.Contains(events[1].Data, "<b>") {
		t.Errorf("the data line must be single-line, unescaped JSON: %q", events[1].Data)
	}

	var done donePayload
	if err := json.Unmarshal([]byte(events[2].Data), &done); err != nil ||
		done != (donePayload{MessageID: 77, Status: "complete", FinishReason: "stop"}) {
		t.Errorf("done = %+v (%v)", done, err)
	}

	// The request reaches the service as sent, with the authenticated user.
	if fa.got.UserID != user || fa.got.DocumentID != doc || fa.got.Question != "  What is this? " || fa.got.RequestID == "" {
		t.Errorf("service received %+v", fa.got)
	}
}

func TestAsk_ErrorEventPayload(t *testing.T) {
	fa := &fakeAsker{run: emitAll(
		service.AskEvent{Kind: service.EventSources, Sources: []service.SourceInfo{}},
		service.AskEvent{Kind: service.EventError, Code: service.AskCodeBusy, Message: "Busy.", RetryAfter: 1500 * time.Millisecond, MessageID: 9},
	)}
	w := askPost(t, newConvRouter(fa, nil, uuid.New(), 0), uuid.NewString(), `{"question":"q"}`, "tok")

	events, _ := parseSSE(t, w.Body.String())
	if len(events) != 2 || events[1].Name != "error" {
		t.Fatalf("events = %+v", events)
	}
	if !strings.Contains(events[0].Data, `"sources":[]`) {
		t.Errorf("empty sources must encode as [] and not null: %s", events[0].Data)
	}
	var p errorPayload
	if err := json.Unmarshal([]byte(events[1].Data), &p); err != nil ||
		p != (errorPayload{Code: "LLM_BUSY", Message: "Busy.", RetryAfterSeconds: 2, MessageID: 9}) {
		t.Errorf("error payload = %+v (%v): 1.5s must round UP to 2", p, err)
	}
}

func TestAsk_ErrorAfterStreamingBeganBecomesAnEvent(t *testing.T) {
	fa := &fakeAsker{run: func(_ context.Context, _ service.AskRequest, emit func(service.AskEvent) error) error {
		_ = emit(service.AskEvent{Kind: service.EventSources, Sources: []service.SourceInfo{}})
		return errors.New("pq: connection to 10.0.0.5 refused")
	}}
	w := askPost(t, newConvRouter(fa, nil, uuid.New(), 0), uuid.NewString(), `{"question":"q"}`, "tok")

	if w.Code != http.StatusOK {
		t.Errorf("status = %d: it cannot change once streaming has begun", w.Code)
	}
	events, _ := parseSSE(t, w.Body.String())
	if len(events) != 2 || events[1].Name != "error" || !strings.Contains(events[1].Data, service.AskCodeInternal) {
		t.Fatalf("events = %+v", events)
	}
	if strings.Contains(w.Body.String(), "10.0.0.5") {
		t.Error("the internal error text reached the client")
	}
}

// --- streaming for real ---

func TestAsk_EventsAreFlushedAsTheyHappen(t *testing.T) {
	user := uuid.New()
	release := make(chan struct{})
	fa := &fakeAsker{run: func(ctx context.Context, _ service.AskRequest, emit func(service.AskEvent) error) error {
		_ = emit(service.AskEvent{Kind: service.EventSources, Sources: []service.SourceInfo{{Label: "P1"}}})
		select {
		case <-release:
		case <-ctx.Done():
			return nil
		}
		_ = emit(service.AskEvent{Kind: service.EventToken, Text: "late"})
		_ = emit(service.AskEvent{Kind: service.EventDone, MessageID: 1, Status: models.MessageComplete})
		return nil
	}}
	srv := httptest.NewServer(newConvRouter(fa, nil, user, 0))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/api/documents/"+uuid.NewString()+"/ask", strings.NewReader(`{"question":"q"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok")

	// Do returns when the response HEADERS arrive. The service is still blocked,
	// so this only works if the first event was flushed immediately.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("no response while the service was still working: %v (events are not flushed)", err)
	}
	defer resp.Body.Close()

	rd := bufio.NewReader(resp.Body)
	if first := readEvent(t, rd); first.Name != "sources" {
		t.Fatalf("first event = %+v", first)
	}
	close(release)
	if second := readEvent(t, rd); second.Name != "token" || !strings.Contains(second.Data, "late") {
		t.Errorf("second event = %+v", second)
	}
	if third := readEvent(t, rd); third.Name != "done" {
		t.Errorf("third event = %+v", third)
	}
}

func TestAsk_ClientDisconnectCancelsTheService(t *testing.T) {
	cancelled := make(chan struct{})
	fa := &fakeAsker{run: func(ctx context.Context, _ service.AskRequest, emit func(service.AskEvent) error) error {
		_ = emit(service.AskEvent{Kind: service.EventSources, Sources: []service.SourceInfo{}})
		select {
		case <-ctx.Done():
			close(cancelled)
		case <-time.After(10 * time.Second):
		}
		return nil
	}}
	srv := httptest.NewServer(newConvRouter(fa, nil, uuid.New(), 0))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/api/documents/"+uuid.NewString()+"/ask", strings.NewReader(`{"question":"q"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	readEvent(t, bufio.NewReader(resp.Body))

	cancel() // the browser tab is closed
	resp.Body.Close()

	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("the service never learned that the client left, so the model call would keep running")
	}
}

func TestAsk_HeartbeatKeepsAQuietStreamAlive(t *testing.T) {
	fa := &fakeAsker{run: func(_ context.Context, _ service.AskRequest, emit func(service.AskEvent) error) error {
		_ = emit(service.AskEvent{Kind: service.EventSources, Sources: []service.SourceInfo{}})
		time.Sleep(200 * time.Millisecond) // for example, waiting out a 429
		_ = emit(service.AskEvent{Kind: service.EventDone, MessageID: 1, Status: models.MessageComplete})
		return nil
	}}
	w := askPost(t, newConvRouter(fa, nil, uuid.New(), 20*time.Millisecond), uuid.NewString(), `{"question":"q"}`, "tok")

	events, pings := parseSSE(t, w.Body.String())
	if pings < 3 {
		t.Errorf("%d heartbeats in 200ms at a 20ms interval, want at least 3", pings)
	}
	if len(events) != 2 || events[1].Name != "done" {
		t.Errorf("events = %+v: heartbeats must not disturb the events", events)
	}
}

func TestAsk_NoHeartbeatBeforeTheStreamStarts(t *testing.T) {
	fa := &fakeAsker{run: func(_ context.Context, _ service.AskRequest, _ func(service.AskEvent) error) error {
		time.Sleep(100 * time.Millisecond)
		return service.ErrAnswerInProgress // refused before any event
	}}
	w := askPost(t, newConvRouter(fa, nil, uuid.New(), 10*time.Millisecond), uuid.NewString(), `{"question":"q"}`, "tok")

	if w.Code != http.StatusConflict || strings.Contains(w.Body.String(), ": ping") {
		t.Errorf("status %d, body %q: a pre-stream error must be a clean JSON response", w.Code, w.Body.String())
	}
}

// --- errors before streaming ---

func TestAsk_ErrorsBeforeStreamingUseProperStatuses(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		status  int
		code    string
		header  [2]string // name, expected value
		details string    // substring expected in details
	}{
		{"empty question", service.ErrEmptyQuestion, 400, response.CodeValidation, [2]string{}, `"field":"question"`},
		{"question too long", fmt.Errorf("ask: %w", service.ErrQuestionTooLong), 400, response.CodeValidation, [2]string{}, `"field":"question"`},
		{"document missing or not yours", fmt.Errorf("ask: %w", models.ErrNotFound), 404, response.CodeNotFound, [2]string{}, ""},
		{"document processing", &service.DocumentNotReadyError{Status: models.StatusProcessing}, 409, response.CodeDocumentNotReady, [2]string{}, `"status":"processing"`},
		{"document failed", &service.DocumentNotReadyError{Status: models.StatusFailed}, 409, response.CodeDocumentNotReady, [2]string{}, `"status":"failed"`},
		{"answer in progress", service.ErrAnswerInProgress, 409, response.CodeAnswerInProgress, [2]string{}, ""},
		{"rate limited", &service.RateLimitError{RetryAfter: 2500 * time.Millisecond}, 429, response.CodeRateLimited, [2]string{"Retry-After", "3"}, ""},
		{"no API key", service.ErrLLMUnavailable, 503, response.CodeLLMUnavailable, [2]string{}, ""},
		{"unexpected", errors.New("pq: connection to 10.0.0.5:5432 refused"), 500, response.CodeInternal, [2]string{}, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fa := &fakeAsker{run: func(context.Context, service.AskRequest, func(service.AskEvent) error) error { return tc.err }}
			w := askPost(t, newConvRouter(fa, nil, uuid.New(), 0), uuid.NewString(), `{"question":"q"}`, "tok")

			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
			if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("Content-Type = %q: a pre-stream error is a normal JSON response", ct)
			}
			env := decodeError(t, w)
			if env.Success || env.Error == nil || env.Error.Code != tc.code {
				t.Fatalf("error = %+v, want code %s", env.Error, tc.code)
			}
			if tc.header[0] != "" && w.Header().Get(tc.header[0]) != tc.header[1] {
				t.Errorf("%s = %q, want %q", tc.header[0], w.Header().Get(tc.header[0]), tc.header[1])
			}
			if tc.details != "" && !strings.Contains(string(env.Error.Details), tc.details) {
				t.Errorf("details = %s, want it to contain %s", env.Error.Details, tc.details)
			}
			if strings.Contains(w.Body.String(), "10.0.0.5") {
				t.Error("an internal error message reached the client")
			}
		})
	}
}

func TestAsk_RequestProblems(t *testing.T) {
	user := uuid.New()

	t.Run("no token", func(t *testing.T) {
		fa := &fakeAsker{}
		w := askPost(t, newConvRouter(fa, nil, user, 0), uuid.NewString(), `{"question":"q"}`, "")
		if w.Code != http.StatusUnauthorized || fa.callCount() != 0 {
			t.Errorf("status %d, calls %d", w.Code, fa.callCount())
		}
	})

	t.Run("malformed document id is 404 and never reaches the service", func(t *testing.T) {
		fa := &fakeAsker{}
		w := askPost(t, newConvRouter(fa, nil, user, 0), "not-a-uuid", `{"question":"q"}`, "tok")
		if w.Code != http.StatusNotFound || fa.callCount() != 0 {
			t.Errorf("status %d, calls %d", w.Code, fa.callCount())
		}
	})

	for name, body := range map[string]string{
		"missing question": `{}`,
		"empty question":   `{"question":""}`,
	} {
		t.Run(name, func(t *testing.T) {
			fa := &fakeAsker{}
			w := askPost(t, newConvRouter(fa, nil, user, 0), uuid.NewString(), body, "tok")
			env := decodeError(t, w)
			if w.Code != http.StatusBadRequest || env.Error == nil || env.Error.Code != response.CodeValidation || fa.callCount() != 0 {
				t.Errorf("status %d, error %+v, calls %d", w.Code, env.Error, fa.callCount())
			}
		})
	}

	t.Run("not JSON", func(t *testing.T) {
		fa := &fakeAsker{}
		w := askPost(t, newConvRouter(fa, nil, user, 0), uuid.NewString(), `question=hello`, "tok")
		env := decodeError(t, w)
		if w.Code != http.StatusBadRequest || env.Error == nil || env.Error.Code != response.CodeBadRequest || fa.callCount() != 0 {
			t.Errorf("status %d, error %+v", w.Code, env.Error)
		}
	})

	t.Run("AI disabled (no asker configured)", func(t *testing.T) {
		w := askPost(t, newConvRouter(nil, nil, user, 0), uuid.NewString(), `{"question":"q"}`, "tok")
		env := decodeError(t, w)
		if w.Code != http.StatusServiceUnavailable || env.Error == nil || env.Error.Code != response.CodeLLMUnavailable {
			t.Errorf("status %d, error %+v", w.Code, env.Error)
		}
	})
}

// The in-stream codes are the same strings as the plain JSON error codes.
func TestStreamErrorCodesMatchTheResponseCodes(t *testing.T) {
	pairs := map[string][2]string{
		"busy":          {service.AskCodeBusy, response.CodeLLMBusy},
		"timeout":       {service.AskCodeTimeout, response.CodeLLMTimeout},
		"unavailable":   {service.AskCodeUnavailable, response.CodeLLMUnavailable},
		"error":         {service.AskCodeError, response.CodeLLMError},
		"internal":      {service.AskCodeInternal, response.CodeInternal},
		"shutting down": {service.AskCodeShuttingDown, response.CodeShuttingDown},
	}
	for name, p := range pairs {
		if p[0] != p[1] {
			t.Errorf("%s: service code %q != response code %q", name, p[0], p[1])
		}
	}
}

// --- messages ---

func TestMessages_PagingParameters(t *testing.T) {
	tests := []struct {
		query     string
		status    int
		limit     int
		before    int64
		badFields string
	}{
		{"", 200, 50, 0, ""},
		{"?limit=10&before=77", 200, 10, 77, ""},
		{"?limit=500", 200, 100, 0, ""},
		{"?limit=", 200, 50, 0, ""},
		{"?before=0", 200, 50, 0, ""},
		{"?limit=0", 400, 0, 0, "limit"},
		{"?limit=-2", 400, 0, 0, "limit"},
		{"?limit=abc", 400, 0, 0, "limit"},
		{"?before=-1", 400, 0, 0, "before"},
		{"?before=abc", 400, 0, 0, "before"},
		{"?before=99999999999999999999", 400, 0, 0, "before"},
		{"?limit=x&before=y", 400, 0, 0, "limit"},
	}

	for _, tc := range tests {
		t.Run(tc.query, func(t *testing.T) {
			user, doc := uuid.New(), uuid.New()
			fl := &fakeLister{page: &models.MessagePage{Messages: []models.Message{}}}
			r := newConvRouter(nil, fl, user, 0)

			req := httptest.NewRequest(http.MethodGet, "/api/documents/"+doc.String()+"/messages"+tc.query, nil)
			req.Header.Set("Authorization", "Bearer tok")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
			if tc.status == http.StatusBadRequest {
				env := decodeError(t, w)
				if env.Error == nil || env.Error.Code != response.CodeValidation || !strings.Contains(string(env.Error.Details), tc.badFields) {
					t.Errorf("error = %+v", env.Error)
				}
				if fl.calls != 0 {
					t.Error("the service must not be called for invalid parameters")
				}
				return
			}
			if fl.gotUser != user || fl.gotDoc != doc || fl.gotLimit != tc.limit || fl.gotBefore != tc.before {
				t.Errorf("service got user %v, doc %v, limit %d, before %d", fl.gotUser, fl.gotDoc, fl.gotLimit, fl.gotBefore)
			}
		})
	}
}

func TestMessages_ResponseShape(t *testing.T) {
	user, doc := uuid.New(), uuid.New()
	model := "openai/gpt-oss-120b"
	prompt, completion := 1200, 80
	fl := &fakeLister{page: &models.MessagePage{
		HasMore: true,
		Messages: []models.Message{
			{ID: 41, Role: models.RoleUser, Status: models.MessageComplete, Content: "What is it?", CreatedAt: time.Now()},
			{ID: 42, Role: models.RoleAssistant, Status: models.MessageComplete, Content: "A thing [P1].", Sources: []int{3, 1},
				Model: &model, PromptTokens: &prompt, CompletionTokens: &completion, CreatedAt: time.Now()},
		},
	}}
	r := newConvRouter(nil, fl, user, 0)

	req := httptest.NewRequest(http.MethodGet, "/api/documents/"+doc.String()+"/messages?limit=2", nil)
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var env struct {
		Data struct {
			Messages   []map[string]any `json:"messages"`
			HasMore    bool             `json:"has_more"`
			NextBefore int64            `json:"next_before"`
			Limit      int              `json:"limit"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	d := env.Data
	if len(d.Messages) != 2 || !d.HasMore || d.NextBefore != 41 || d.Limit != 2 {
		t.Fatalf("data = %+v: next_before must be the id of the OLDEST message on the page", d)
	}
	if _, has := d.Messages[0]["sources"]; has {
		t.Error("a question has no sources")
	}
	if src, _ := d.Messages[1]["sources"].([]any); len(src) != 2 {
		t.Errorf("answer sources = %v", d.Messages[1]["sources"])
	}
	for _, leaked := range []string{"gpt-oss", "model", "prompt_tokens", "completion_tokens", "1200"} {
		if strings.Contains(w.Body.String(), leaked) {
			t.Errorf("the response leaks %q", leaked)
		}
	}
}

func TestMessages_EmptyAndErrors(t *testing.T) {
	user, doc := uuid.New(), uuid.New()
	get := func(fl *fakeLister, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/documents/"+doc.String()+"/messages", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		newConvRouter(nil, fl, user, 0).ServeHTTP(w, req)
		return w
	}

	t.Run("an empty page is [] and has no cursor", func(t *testing.T) {
		w := get(&fakeLister{page: &models.MessagePage{Messages: []models.Message{}}}, "tok")
		if !strings.Contains(w.Body.String(), `"messages":[]`) || strings.Contains(w.Body.String(), "next_before") || !strings.Contains(w.Body.String(), `"has_more":false`) {
			t.Errorf("body = %s", w.Body.String())
		}
	})

	t.Run("not found", func(t *testing.T) {
		w := get(&fakeLister{err: fmt.Errorf("list messages: %w", models.ErrNotFound)}, "tok")
		if env := decodeError(t, w); w.Code != 404 || env.Error == nil || env.Error.Code != response.CodeNotFound {
			t.Errorf("status %d, error %+v", w.Code, env.Error)
		}
	})

	t.Run("unexpected error", func(t *testing.T) {
		w := get(&fakeLister{err: errors.New("boom 10.0.0.5")}, "tok")
		if w.Code != 500 || strings.Contains(w.Body.String(), "10.0.0.5") {
			t.Errorf("status %d, body %s", w.Code, w.Body.String())
		}
	})

	t.Run("no token", func(t *testing.T) {
		fl := &fakeLister{}
		if w := get(fl, ""); w.Code != 401 || fl.calls != 0 {
			t.Errorf("status %d, calls %d", w.Code, fl.calls)
		}
	})

	t.Run("malformed document id", func(t *testing.T) {
		fl := &fakeLister{}
		req := httptest.NewRequest(http.MethodGet, "/api/documents/not-a-uuid/messages", nil)
		req.Header.Set("Authorization", "Bearer tok")
		w := httptest.NewRecorder()
		newConvRouter(nil, fl, user, 0).ServeHTTP(w, req)
		if w.Code != 404 || fl.calls != 0 {
			t.Errorf("status %d, calls %d", w.Code, fl.calls)
		}
	})
}