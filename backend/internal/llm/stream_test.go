package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/service"
)

// Events shaped like your real capture.
const firstChunk = `data: {"id":"c1","object":"chat.completion.chunk","model":"openai/gpt-oss-20b",` +
	`"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}` + "\n\n"

const doneEvent = "data: [DONE]\n\n"

func contentChunk(text string) string {
	b, _ := json.Marshal(text)
	return `data: {"id":"c1","object":"chat.completion.chunk","model":"openai/gpt-oss-20b",` +
		`"choices":[{"index":0,"delta":{"content":` + string(b) + `},"finish_reason":null}]}` + "\n\n"
}

// finalChunk has the finish reason and the usage twice: top level and x_groq.
func finalChunk(reason string) string {
	usage := `{"prompt_tokens":77,"completion_tokens":21,"total_tokens":98,"completion_tokens_details":{"reasoning_tokens":6}}`
	return `data: {"id":"c1","object":"chat.completion.chunk","model":"openai/gpt-oss-20b",` +
		`"choices":[{"index":0,"delta":{},"finish_reason":"` + reason + `"}],` +
		`"x_groq":{"id":"req_1","usage":` + usage + `},"usage":` + usage + `}` + "\n\n"
}

// streamOut starts an SSE response and writes the parts, flushing after each.
func streamOut(w http.ResponseWriter, parts ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl := w.(http.Flusher)
	fl.Flush()
	for _, p := range parts {
		_, _ = io.WriteString(w, p)
		fl.Flush()
	}
}

type textLog struct {
	mu    sync.Mutex
	texts []string
}

func (l *textLog) add(s string) error {
	l.mu.Lock()
	l.texts = append(l.texts, s)
	l.mu.Unlock()
	return nil
}

func (l *textLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.texts...)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestStream_Success(t *testing.T) {
	f := newFakeGroq(t, func(n int, w http.ResponseWriter, r *http.Request) {
		streamOut(w, firstChunk, contentChunk("Hel"), contentChunk("lo "), contentChunk("world"), finalChunk("stop"), doneEvent)
	})
	c, _ := newTestClient(t, f, nil)

	var log textLog
	got, err := c.Stream(context.Background(), chatReq(), log.add)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	// The empty first chunk and the text-less last chunk are not delivered.
	if want := []string{"Hel", "lo ", "world"}; !equalStrings(log.all(), want) {
		t.Errorf("delivered %q, want %q", log.all(), want)
	}
	if got.Content != "Hello world" || got.FinishReason != "stop" || got.Model != "openai/gpt-oss-20b" {
		t.Errorf("response = %+v", got)
	}
	if want := (service.TokenUsage{PromptTokens: 77, CompletionTokens: 21, TotalTokens: 98, ReasoningTokens: 6}); got.Usage != want {
		t.Errorf("usage = %+v, want %+v", got.Usage, want)
	}

	rec := f.request(0)
	if rec.header.Get("Accept") != "text/event-stream" {
		t.Errorf("Accept = %q", rec.header.Get("Accept"))
	}
	var sent map[string]any
	if err := json.Unmarshal(rec.body, &sent); err != nil || sent["stream"] != true {
		t.Errorf("stream flag in the request = %v (%v)", sent["stream"], err)
	}
}

func TestStream_ToleratesCommentsCRLFAndMultilineEvents(t *testing.T) {
	multiline := "event: message\n" +
		"data: {\"choices\":[{\"index\":0,\n" +
		"data: \"delta\":{\"content\":\"B\"},\"finish_reason\":null}]}\n\n"
	crlf := strings.ReplaceAll(contentChunk("C")+finalChunk("stop")+doneEvent, "\n", "\r\n")

	f := newFakeGroq(t, func(n int, w http.ResponseWriter, r *http.Request) {
		streamOut(w, ": keep-alive\n\n", firstChunk, contentChunk("A"), ": ping\n\n", multiline, crlf)
	})
	c, _ := newTestClient(t, f, nil)

	var log textLog
	got, err := c.Stream(context.Background(), chatReq(), log.add)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got.Content != "ABC" || got.FinishReason != "stop" {
		t.Errorf("response = %+v, delivered %q", got, log.all())
	}
}

func TestStream_LengthFinishIsNotAnError(t *testing.T) {
	f := newFakeGroq(t, func(n int, w http.ResponseWriter, r *http.Request) {
		streamOut(w, firstChunk, contentChunk("Greetings, world—bright"), finalChunk("length"), doneEvent)
	})
	c, _ := newTestClient(t, f, nil)

	var log textLog
	got, err := c.Stream(context.Background(), chatReq(), log.add)
	if err != nil {
		t.Fatalf("a cut-off answer is a normal result, got %v", err)
	}
	if got.FinishReason != "length" || got.Content != "Greetings, world—bright" {
		t.Errorf("response = %+v", got)
	}
}

func TestStream_SkipsEmptyAndReasoningOnlyDeltas(t *testing.T) {
	reasoningOnly := `data: {"choices":[{"index":0,"delta":{"reasoning":"thinking..."},"finish_reason":null}]}` + "\n\n"
	emptyContent := `data: {"choices":[{"index":0,"delta":{"content":""},"finish_reason":null}]}` + "\n\n"
	noChoices := `data: {"id":"c1","choices":[]}` + "\n\n"

	f := newFakeGroq(t, func(n int, w http.ResponseWriter, r *http.Request) {
		streamOut(w, firstChunk, reasoningOnly, noChoices, contentChunk("A"), emptyContent, finalChunk("stop"), doneEvent)
	})
	c, _ := newTestClient(t, f, nil)

	var log textLog
	got, err := c.Stream(context.Background(), chatReq(), log.add)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if !equalStrings(log.all(), []string{"A"}) || got.Content != "A" {
		t.Errorf("delivered %q, content %q: only real text may be delivered", log.all(), got.Content)
	}
}

func TestStream_HugeEventAndOversizedLine(t *testing.T) {
	t.Run("a 200 KB event is fine (no 64 KB scanner limit)", func(t *testing.T) {
		big := strings.Repeat("a", 200<<10)
		f := newFakeGroq(t, func(n int, w http.ResponseWriter, r *http.Request) {
			streamOut(w, firstChunk, contentChunk(big), finalChunk("stop"), doneEvent)
		})
		c, _ := newTestClient(t, f, nil)

		got, err := c.Stream(context.Background(), chatReq(), func(string) error { return nil })
		if err != nil || got.Content != big {
			t.Errorf("err = %v, content length %d, want %d", err, len(got.Content), len(big))
		}
	})

	t.Run("a 2 MB line is refused", func(t *testing.T) {
		f := newFakeGroq(t, func(n int, w http.ResponseWriter, r *http.Request) {
			streamOut(w, firstChunk, "data: "+strings.Repeat("x", 2<<20)+"\n\n")
		})
		c, _ := newTestClient(t, f, nil)

		_, err := c.Stream(context.Background(), chatReq(), func(string) error { return nil })
		if le := asLLMError(t, err); le.Kind != service.LLMBadResponse {
			t.Errorf("kind = %v, want bad_response", le.Kind)
		}
	})
}

// --- failures ---

func TestStream_MidStreamDisconnectIsNotRetried(t *testing.T) {
	f := newFakeGroq(t, func(n int, w http.ResponseWriter, r *http.Request) {
		streamOut(w, firstChunk, contentChunk("Par"), contentChunk("tial"))
		panic(http.ErrAbortHandler) // the connection dies in the middle of the answer
	})
	c, sleeps := newTestClient(t, f, nil)

	var log textLog
	got, err := c.Stream(context.Background(), chatReq(), log.add)
	if le := asLLMError(t, err); le.Kind != service.LLMUnavailable {
		t.Errorf("kind = %v, want unavailable", le.Kind)
	}
	if got.Content != "Partial" || !equalStrings(log.all(), []string{"Par", "tial"}) {
		t.Errorf("partial content = %q, delivered %q", got.Content, log.all())
	}
	if f.count() != 1 || len(sleeps.all()) != 0 {
		t.Errorf("requests %d, sleeps %v: a retry would repeat text the caller already has", f.count(), sleeps.all())
	}
}

func TestStream_CleanEndWithoutDoneOrFinishIsAnError(t *testing.T) {
	f := newFakeGroq(t, func(n int, w http.ResponseWriter, r *http.Request) {
		streamOut(w, firstChunk, contentChunk("Hi")) // then the handler simply returns
	})
	c, _ := newTestClient(t, f, nil)

	got, err := c.Stream(context.Background(), chatReq(), func(string) error { return nil })
	if le := asLLMError(t, err); le.Kind != service.LLMUnavailable {
		t.Errorf("kind = %v, want unavailable", le.Kind)
	}
	if got.Content != "Hi" || f.count() != 1 {
		t.Errorf("content %q, requests %d", got.Content, f.count())
	}
}

func TestStream_EndAfterFinishWithoutDoneIsSuccess(t *testing.T) {
	f := newFakeGroq(t, func(n int, w http.ResponseWriter, r *http.Request) {
		streamOut(w, firstChunk, contentChunk("Hi"), finalChunk("stop"))
	})
	c, _ := newTestClient(t, f, nil)

	got, err := c.Stream(context.Background(), chatReq(), func(string) error { return nil })
	if err != nil || got.Content != "Hi" || got.FinishReason != "stop" {
		t.Errorf("got (%+v, %v)", got, err)
	}
}

func TestStream_FailureBeforeTheFirstTokenIsRetried(t *testing.T) {
	f := newFakeGroq(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			streamOut(w) // 200 and headers, then the connection dies before any text
			panic(http.ErrAbortHandler)
		}
		streamOut(w, firstChunk, contentChunk("Hi"), finalChunk("stop"), doneEvent)
	})
	c, sleeps := newTestClient(t, f, nil)

	var log textLog
	got, err := c.Stream(context.Background(), chatReq(), log.add)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if f.count() != 2 || len(sleeps.all()) != 1 {
		t.Errorf("requests %d, sleeps %v, want 2 and 1", f.count(), sleeps.all())
	}
	if !equalStrings(log.all(), []string{"Hi"}) || got.Content != "Hi" {
		t.Errorf("delivered %q, content %q: the retry must not duplicate text", log.all(), got.Content)
	}
}

func TestStream_RetriesStatusErrorsBeforeStreaming(t *testing.T) {
	f := newFakeGroq(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			writeJSON(w, 429, errorJSON("slow down", "tokens", "rate_limit_exceeded"), "Retry-After", "1")
			return
		}
		streamOut(w, firstChunk, contentChunk("Hi"), finalChunk("stop"), doneEvent)
	})
	c, sleeps := newTestClient(t, f, nil)

	got, err := c.Stream(context.Background(), chatReq(), func(string) error { return nil })
	if err != nil || got.Content != "Hi" {
		t.Fatalf("got (%+v, %v)", got, err)
	}
	if s := sleeps.all(); len(s) != 1 || s[0] != time.Second {
		t.Errorf("sleeps = %v, want [1s]", s)
	}
}

func TestStream_IdleTimeout(t *testing.T) {
	tweak := func(cfg *Config) { cfg.StreamIdleTimeout = 100 * time.Millisecond }

	t.Run("after some text: reported, not retried, partial text kept", func(t *testing.T) {
		f := newFakeGroq(t, func(n int, w http.ResponseWriter, r *http.Request) {
			streamOut(w, firstChunk, contentChunk("Hi"))
			blockUntilGone(r) // goes silent
		})
		c, _ := newTestClient(t, f, tweak)

		start := time.Now()
		got, err := c.Stream(context.Background(), chatReq(), func(string) error { return nil })
		if le := asLLMError(t, err); le.Kind != service.LLMTimeout {
			t.Errorf("kind = %v, want timeout", le.Kind)
		}
		if got.Content != "Hi" || f.count() != 1 {
			t.Errorf("content %q, requests %d", got.Content, f.count())
		}
		if time.Since(start) > 3*time.Second {
			t.Errorf("took %v: the idle watchdog did not fire", time.Since(start))
		}
	})

	t.Run("before any text: retried", func(t *testing.T) {
		f := newFakeGroq(t, func(n int, w http.ResponseWriter, r *http.Request) {
			if n == 1 {
				streamOut(w, firstChunk)
				blockUntilGone(r)
				return
			}
			streamOut(w, firstChunk, contentChunk("Hi"), finalChunk("stop"), doneEvent)
		})
		c, _ := newTestClient(t, f, tweak)

		got, err := c.Stream(context.Background(), chatReq(), func(string) error { return nil })
		if err != nil || got.Content != "Hi" || f.count() != 2 {
			t.Errorf("got (%+v, %v) after %d requests", got, err, f.count())
		}
	})
}

func TestStream_MaxDurationEndsASlowButLivelyStream(t *testing.T) {
	f := newFakeGroq(t, func(n int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		for r.Context().Err() == nil {
			_, _ = io.WriteString(w, contentChunk("x"))
			fl.Flush()
			time.Sleep(15 * time.Millisecond)
		}
	})
	c, _ := newTestClient(t, f, func(cfg *Config) { cfg.StreamMaxDuration = 150 * time.Millisecond })

	got, err := c.Stream(context.Background(), chatReq(), func(string) error { return nil })
	if le := asLLMError(t, err); le.Kind != service.LLMTimeout {
		t.Errorf("kind = %v, want timeout", le.Kind)
	}
	if got.Content == "" || f.count() != 1 {
		t.Errorf("content %q, requests %d: partial text must be kept and nothing retried", got.Content, f.count())
	}
}

func TestStream_ErrorEventInsideTheStream(t *testing.T) {
	f := newFakeGroq(t, func(n int, w http.ResponseWriter, r *http.Request) {
		streamOut(w, firstChunk, contentChunk("Hi"),
			`data: {"error":{"message":"overloaded","type":"server_error"}}`+"\n\n")
	})
	c, _ := newTestClient(t, f, nil)

	got, err := c.Stream(context.Background(), chatReq(), func(string) error { return nil })
	le := asLLMError(t, err)
	if le.Kind != service.LLMUnavailable || !strings.Contains(le.Message, "") {
		t.Errorf("error = %+v", le)
	}
	if got.Content != "Hi" || f.count() != 1 {
		t.Errorf("content %q, requests %d", got.Content, f.count())
	}
}

// --- the consumer leaves ---

// slowStream sends a chunk every few milliseconds until the client disconnects.
func slowStream(sawDisconnect chan struct{}) func(int, http.ResponseWriter, *http.Request) {
	var once sync.Once
	return func(n int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		for i := 0; i < 600; i++ {
			if r.Context().Err() != nil {
				once.Do(func() { close(sawDisconnect) })
				return
			}
			_, _ = io.WriteString(w, contentChunk("x"))
			fl.Flush()
			time.Sleep(5 * time.Millisecond)
		}
	}
}

func waitClosed(t *testing.T, ch chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Errorf("the server never noticed: %s", what)
	}
}

func TestStream_CallbackErrorStopsTheStream(t *testing.T) {
	saw := make(chan struct{})
	f := newFakeGroq(t, slowStream(saw))
	c, _ := newTestClient(t, f, nil)

	boom := errors.New("client is gone")
	calls := 0
	got, err := c.Stream(context.Background(), chatReq(), func(string) error {
		calls++
		if calls == 2 {
			return boom
		}
		return nil
	})
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap the callback's error", err)
	}
	if calls != 2 || got.Content != "xx" {
		t.Errorf("callback calls %d, content %q: the stream must stop at the first failure", calls, got.Content)
	}
	if f.count() != 1 {
		t.Errorf("%d requests: a callback failure must not be retried", f.count())
	}
	waitClosed(t, saw, "the abandoned stream was left open")
}

func TestStream_CallerCancelAbortsTheUpstreamCall(t *testing.T) {
	saw := make(chan struct{})
	f := newFakeGroq(t, slowStream(saw))
	c, _ := newTestClient(t, f, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got, err := c.Stream(ctx, chatReq(), func(string) error {
		cancel() // the HTTP client went away after the first piece of text
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	var le *service.LLMError
	if errors.As(err, &le) {
		t.Error("a caller who left is not a provider failure")
	}
	if got.Content == "" {
		t.Error("the partial text received so far must be returned")
	}
	waitClosed(t, saw, "the upstream call was not cancelled, so tokens keep being paid for")
}

func TestStream_RequiresACallback(t *testing.T) {
	f := newFakeGroq(t, func(n int, w http.ResponseWriter, r *http.Request) { streamOut(w, doneEvent) })
	c, _ := newTestClient(t, f, nil)

	if _, err := c.Stream(context.Background(), chatReq(), nil); err == nil {
		t.Error("expected an error")
	}
	if f.count() != 0 {
		t.Error("nothing should be sent")
	}
}