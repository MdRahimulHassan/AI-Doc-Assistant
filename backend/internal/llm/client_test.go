package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/service"
)

const testKey = "gsk_test_key_0123456789abcdef"

// The shape of a real non-streaming response (from your capture).
const okBody = `{"id":"chatcmpl-1","object":"chat.completion","model":"openai/gpt-oss-20b",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"Hello there, how are you?"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":77,"completion_tokens":63,"total_tokens":140,"completion_tokens_details":{"reasoning_tokens":47}}}`

// --- a fake Groq ---

type recorded struct {
	header http.Header
	body   []byte
}

type fakeGroq struct {
	srv      *httptest.Server
	mu       sync.Mutex
	requests []recorded
}

// newFakeGroq starts a server for POST /chat/completions. The handler gets the
// 1-based number of the request.
func newFakeGroq(t *testing.T, handler func(n int, w http.ResponseWriter, r *http.Request)) *fakeGroq {
	t.Helper()
	f := &fakeGroq{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, recorded{header: r.Header.Clone(), body: body})
		n := len(f.requests)
		f.mu.Unlock()
		handler(n, w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGroq) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeGroq) request(i int) recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[i]
}

type recordedSleeps struct {
	mu        sync.Mutex
	durations []time.Duration
}

func (s *recordedSleeps) sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.durations = append(s.durations, d)
	s.mu.Unlock()
	return ctx.Err()
}

func (s *recordedSleeps) all() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.durations...)
}

// newTestClient points a client at the fake. Sleeps are recorded instead of
// performed and jitter returns its ceiling, so retry timing is exact and instant.
func newTestClient(t *testing.T, f *fakeGroq, tweak func(*Config)) (*Client, *recordedSleeps) {
	t.Helper()
	cfg := Config{APIKey: testKey, BaseURL: f.srv.URL, TPMBudget: 600_000}
	if tweak != nil {
		tweak(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)

	s := &recordedSleeps{}
	c.sleep = s.sleep
	c.jitter = func(ceiling time.Duration) time.Duration { return ceiling }
	return c, s
}

func chatReq() service.ChatRequest {
	return service.ChatRequest{
		Model:           "openai/gpt-oss-20b",
		Messages:        []service.ChatMessage{{Role: "system", Content: "Be brief."}, {Role: "user", Content: "Say hello."}},
		MaxOutputTokens: 300,
		Temperature:     0.2,
		ReasoningEffort: "low",
	}
}

func writeJSON(w http.ResponseWriter, status int, body string, headers ...string) {
	for i := 0; i+1 < len(headers); i += 2 {
		w.Header().Set(headers[i], headers[i+1])
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func errorJSON(message, typ, code string) string {
	b, _ := json.Marshal(map[string]any{"error": map[string]string{"message": message, "type": typ, "code": code}})
	return string(b)
}

func asLLMError(t *testing.T, err error) *service.LLMError {
	t.Helper()
	var le *service.LLMError
	if !errors.As(err, &le) {
		t.Fatalf("err = %v (%T), want a *service.LLMError", err, err)
	}
	return le
}

// blockUntilGone holds a handler until the client disconnects.
func blockUntilGone(r *http.Request) {
	select {
	case <-r.Context().Done():
	case <-time.After(10 * time.Second):
	}
}

// --- success, and what is sent ---

func TestComplete_Success(t *testing.T) {
	f := newFakeGroq(t, func(n int, w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, okBody) })
	c, _ := newTestClient(t, f, nil)

	got, err := c.Complete(context.Background(), chatReq())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got.Content != "Hello there, how are you?" || got.FinishReason != "stop" || got.Model != "openai/gpt-oss-20b" {
		t.Errorf("response = %+v", got)
	}
	if want := (service.TokenUsage{PromptTokens: 77, CompletionTokens: 63, TotalTokens: 140, ReasoningTokens: 47}); got.Usage != want {
		t.Errorf("usage = %+v, want %+v", got.Usage, want)
	}

	rec := f.request(0)
	if rec.header.Get("Authorization") != "Bearer "+testKey || rec.header.Get("Content-Type") != "application/json" {
		t.Errorf("headers = %v", rec.header)
	}
	var sent map[string]any
	if err := json.Unmarshal(rec.body, &sent); err != nil {
		t.Fatal(err)
	}
	checks := map[string]any{
		"model": "openai/gpt-oss-20b", "stream": false, "max_completion_tokens": 300.0,
		"temperature": 0.2, "reasoning_effort": "low", "include_reasoning": false,
	}
	for k, want := range checks {
		if sent[k] != want {
			t.Errorf("request field %s = %v, want %v", k, sent[k], want)
		}
	}
	if msgs, _ := sent["messages"].([]any); len(msgs) != 2 {
		t.Errorf("messages = %v", sent["messages"])
	}
}

func TestComplete_OmitsReasoningSettingsWhenNotRequested(t *testing.T) {
	f := newFakeGroq(t, func(n int, w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, okBody) })
	c, _ := newTestClient(t, f, nil)

	req := chatReq()
	req.ReasoningEffort = ""
	if _, err := c.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	var sent map[string]any
	if err := json.Unmarshal(f.request(0).body, &sent); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"reasoning_effort", "include_reasoning"} {
		if _, present := sent[k]; present {
			t.Errorf("%s must not be sent when no reasoning effort is requested", k)
		}
	}
}

func TestComplete_RejectsInvalidRequestsBeforeSending(t *testing.T) {
	tests := map[string]func(*service.ChatRequest){
		"no model":         func(r *service.ChatRequest) { r.Model = " " },
		"no messages":      func(r *service.ChatRequest) { r.Messages = nil },
		"no output limit":  func(r *service.ChatRequest) { r.MaxOutputTokens = 0 },
		"unknown role":     func(r *service.ChatRequest) { r.Messages[0].Role = "tool" },
		"unknown reasoning": func(r *service.ChatRequest) { r.ReasoningEffort = "extreme" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			f := newFakeGroq(t, func(n int, w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, okBody) })
			c, _ := newTestClient(t, f, nil)

			req := chatReq()
			mutate(&req)
			if _, err := c.Complete(context.Background(), req); err == nil {
				t.Error("expected an error")
			}
			if f.count() != 0 {
				t.Errorf("%d request(s) were sent for an invalid call", f.count())
			}
		})
	}
}

func TestComplete_BadBodiesAreNotRetried(t *testing.T) {
	for name, body := range map[string]string{
		"not JSON":   "this is not json",
		"no choices": `{"choices":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeGroq(t, func(n int, w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, body) })
			c, sleeps := newTestClient(t, f, nil)

			_, err := c.Complete(context.Background(), chatReq())
			if le := asLLMError(t, err); le.Kind != service.LLMBadResponse {
				t.Errorf("kind = %v, want bad_response", le.Kind)
			}
			if f.count() != 1 || len(sleeps.all()) != 0 {
				t.Errorf("requests %d, sleeps %v: a bad body must not be retried", f.count(), sleeps.all())
			}
		})
	}
}

// --- retries ---

func TestRetry_429ThenSuccess_HonorsRetryAfter(t *testing.T) {
	f := newFakeGroq(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			writeJSON(w, 429, errorJSON("Rate limit reached", "tokens", "rate_limit_exceeded"), "Retry-After", "2")
			return
		}
		writeJSON(w, 200, okBody)
	})
	c, sleeps := newTestClient(t, f, nil)

	if _, err := c.Complete(context.Background(), chatReq()); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if f.count() != 2 {
		t.Errorf("%d requests, want 2", f.count())
	}
	if got := sleeps.all(); len(got) != 1 || got[0] != 2*time.Second {
		t.Errorf("sleeps = %v, want exactly the 2s the provider asked for", got)
	}
}

func TestRetry_LongRetryAfterFailsFast(t *testing.T) {
	f := newFakeGroq(t, func(n int, w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 429, errorJSON("Rate limit reached", "tokens", "rate_limit_exceeded"), "Retry-After", "30")
	})
	c, sleeps := newTestClient(t, f, nil) // the default interactive cap is 20s

	_, err := c.Complete(context.Background(), chatReq())
	le := asLLMError(t, err)
	if le.Kind != service.LLMBusy || le.Status != 429 || le.RetryAfter != 30*time.Second {
		t.Errorf("error = %+v", le)
	}
	if f.count() != 1 || len(sleeps.all()) != 0 {
		t.Errorf("requests %d, sleeps %v: a long Retry-After must fail fast", f.count(), sleeps.all())
	}
}

func TestRetry_MaxWaitAllowsPatientCallers(t *testing.T) {
	f := newFakeGroq(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			writeJSON(w, 429, errorJSON("Rate limit reached", "tokens", "rate_limit_exceeded"), "Retry-After", "30")
			return
		}
		writeJSON(w, 200, okBody)
	})
	c, sleeps := newTestClient(t, f, nil)

	req := chatReq()
	req.MaxWait = time.Minute // a background job
	if _, err := c.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := sleeps.all(); len(got) != 1 || got[0] != 30*time.Second {
		t.Errorf("sleeps = %v, want [30s]", got)
	}
}

func TestRetry_ExponentialBackoffWithoutRetryAfter(t *testing.T) {
	f := newFakeGroq(t, func(n int, w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 429, errorJSON("slow down", "tokens", "rate_limit_exceeded"))
	})
	c, sleeps := newTestClient(t, f, nil)

	_, err := c.Complete(context.Background(), chatReq())
	if le := asLLMError(t, err); le.Kind != service.LLMBusy {
		t.Errorf("kind = %v, want busy", le.Kind)
	}
	if f.count() != 4 {
		t.Errorf("%d requests, want 4 (the default number of attempts)", f.count())
	}
	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second} // ceilings; jitter is pinned to them
	got := sleeps.all()
	if len(got) != len(want) {
		t.Fatalf("sleeps = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sleep %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestRetry_ServerErrorsThenSuccess(t *testing.T) {
	for _, status := range []int{500, 502, 503, 504} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f := newFakeGroq(t, func(n int, w http.ResponseWriter, r *http.Request) {
				if n == 1 {
					writeJSON(w, status, errorJSON("upstream trouble", "server_error", "internal"))
					return
				}
				writeJSON(w, 200, okBody)
			})
			c, sleeps := newTestClient(t, f, nil)

			if _, err := c.Complete(context.Background(), chatReq()); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if f.count() != 2 || len(sleeps.all()) != 1 {
				t.Errorf("requests %d, sleeps %v", f.count(), sleeps.all())
			}
		})
	}
}

func TestRetry_ServerErrorForeverGivesUp(t *testing.T) {
	f := newFakeGroq(t, func(n int, w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 500, errorJSON("oops", "server_error", "internal"))
	})
	c, _ := newTestClient(t, f, nil)

	_, err := c.Complete(context.Background(), chatReq())
	le := asLLMError(t, err)
	if le.Kind != service.LLMUnavailable || le.Status != 500 {
		t.Errorf("error = %+v", le)
	}
	if f.count() != 4 {
		t.Errorf("%d requests, want 4", f.count())
	}
}

func TestNoRetryForClientErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		code   string
		kind   service.LLMErrorKind
	}{
		{"bad request", 400, "invalid_request", service.LLMBadRequest},
		{"invalid key", 401, "invalid_api_key", service.LLMMisconfigured},
		{"forbidden", 403, "permission_denied", service.LLMMisconfigured},
		{"unknown model", 404, "model_not_found", service.LLMMisconfigured},
		{"request too large", 413, "request_too_large", service.LLMBadRequest},
		{"unprocessable", 422, "unprocessable", service.LLMBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg := "provider says " + tc.code
			f := newFakeGroq(t, func(n int, w http.ResponseWriter, r *http.Request) {
				writeJSON(w, tc.status, errorJSON(msg, "invalid_request_error", tc.code))
			})
			c, sleeps := newTestClient(t, f, nil)

			_, err := c.Complete(context.Background(), chatReq())
			le := asLLMError(t, err)
			if le.Kind != tc.kind || le.Status != tc.status || le.Code != tc.code || !strings.Contains(le.Message, msg) {
				t.Errorf("error = %+v", le)
			}
			if f.count() != 1 || len(sleeps.all()) != 0 {
				t.Errorf("requests %d, sleeps %v: a %d must never be retried", f.count(), sleeps.all(), tc.status)
			}
		})
	}
}

func TestAPIKeyNeverAppearsInErrors(t *testing.T) {
	f := newFakeGroq(t, func(n int, w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 401, errorJSON("Invalid API Key: "+testKey, "invalid_request_error", "invalid_api_key"))
	})
	c, _ := newTestClient(t, f, nil)

	_, err := c.Complete(context.Background(), chatReq())
	le := asLLMError(t, err)
	if strings.Contains(le.Message, testKey) || strings.Contains(err.Error(), testKey) {
		t.Errorf("the key leaked: %v", err)
	}
	if !strings.Contains(le.Message, "[redacted]") {
		t.Errorf("message = %q, want the key replaced by [redacted]", le.Message)
	}
}

func TestRetry_NetworkErrorThenSuccess(t *testing.T) {
	f := newFakeGroq(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			panic(http.ErrAbortHandler) // the connection is dropped without any response
		}
		writeJSON(w, 200, okBody)
	})
	c, sleeps := newTestClient(t, f, nil)

	if _, err := c.Complete(context.Background(), chatReq()); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if f.count() != 2 || len(sleeps.all()) != 1 {
		t.Errorf("requests %d, sleeps %v", f.count(), sleeps.all())
	}
}

func TestRetry_ResponseHeaderTimeoutThenSuccess(t *testing.T) {
	f := newFakeGroq(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			blockUntilGone(r) // never answers: the client gives up and disconnects
			return
		}
		writeJSON(w, 200, okBody)
	})
	c, _ := newTestClient(t, f, func(cfg *Config) { cfg.ResponseHeaderTimeout = 50 * time.Millisecond })

	if _, err := c.Complete(context.Background(), chatReq()); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if f.count() != 2 {
		t.Errorf("%d requests, want 2", f.count())
	}
}

func TestComplete_AttemptTimeoutIsRetriedThenReported(t *testing.T) {
	f := newFakeGroq(t, func(n int, w http.ResponseWriter, r *http.Request) { blockUntilGone(r) })
	c, _ := newTestClient(t, f, func(cfg *Config) { cfg.RequestTimeout = 80 * time.Millisecond })

	_, err := c.Complete(context.Background(), chatReq())
	if le := asLLMError(t, err); le.Kind != service.LLMTimeout {
		t.Errorf("kind = %v, want timeout", le.Kind)
	}
	if f.count() != 4 {
		t.Errorf("%d requests, want 4 (each attempt times out and is retried)", f.count())
	}
}

// --- the caller leaves ---

func TestComplete_CallerCancelStopsBackoffImmediately(t *testing.T) {
	f := newFakeGroq(t, func(n int, w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 429, errorJSON("slow down", "tokens", "rate_limit_exceeded"), "Retry-After", "5")
	})
	c, _ := newTestClient(t, f, nil)
	c.sleep = sleepContext // the REAL sleep, so cancellation is what ends it

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)

	start := time.Now()
	_, err := c.Complete(ctx, chatReq())
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	var le *service.LLMError
	if errors.As(err, &le) {
		t.Error("a caller who left is not a provider failure")
	}
	if time.Since(start) > 3*time.Second || f.count() != 1 {
		t.Errorf("took %v with %d requests: the 5s wait was not interrupted", time.Since(start), f.count())
	}
}

func TestComplete_CallerDeadlineIsReportedAsSuch(t *testing.T) {
	f := newFakeGroq(t, func(n int, w http.ResponseWriter, r *http.Request) { blockUntilGone(r) })
	c, _ := newTestClient(t, f, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	_, err := c.Complete(ctx, chatReq())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
	var le *service.LLMError
	if errors.As(err, &le) {
		t.Error("the caller's own deadline is not a provider failure")
	}
}

// --- pacing ---

func TestPacing_RejectsRequestsLargerThanTheBudget(t *testing.T) {
	f := newFakeGroq(t, func(n int, w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, okBody) })
	c, _ := newTestClient(t, f, func(cfg *Config) { cfg.TPMBudget = 1000 })

	req := chatReq()
	req.MaxOutputTokens = 2000
	_, err := c.Complete(context.Background(), req)
	if le := asLLMError(t, err); le.Kind != service.LLMBadRequest {
		t.Errorf("kind = %v, want bad_request", le.Kind)
	}
	if f.count() != 0 {
		t.Error("an over-budget request must never reach the provider")
	}
}

func TestPacing_FailsFastWhenTheSharedBudgetIsUsedUp(t *testing.T) {
	f := newFakeGroq(t, func(n int, w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, okBody) })
	c, _ := newTestClient(t, f, func(cfg *Config) { cfg.TPMBudget = 6000 }) // 100 tokens per second

	req := chatReq()
	req.EstimatedPromptTokens = 3000
	req.MaxOutputTokens = 2900 // costs 5900 of the 6000

	if _, err := c.Complete(context.Background(), req); err != nil {
		t.Fatalf("first call: %v", err)
	}
	_, err := c.Complete(context.Background(), req) // would need about a minute of refill
	le := asLLMError(t, err)
	if le.Kind != service.LLMBusy || le.RetryAfter < 30*time.Second {
		t.Errorf("error = %+v, want busy with a long RetryAfter", le)
	}
	if f.count() != 1 {
		t.Errorf("%d requests reached the provider, want only the first", f.count())
	}
}