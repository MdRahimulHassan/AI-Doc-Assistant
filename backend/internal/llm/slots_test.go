package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/service"
)

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// holdingServer answers only once `release` is closed, and tracks how many
// requests are inside it at the same time.
type holdingServer struct {
	*fakeGroq
	running, peak atomic.Int32
	release       chan struct{}
}

func newHoldingServer(t *testing.T) *holdingServer {
	t.Helper()
	h := &holdingServer{release: make(chan struct{})}
	h.fakeGroq = newFakeGroq(t, func(n int, w http.ResponseWriter, r *http.Request) {
		cur := h.running.Add(1)
		for {
			p := h.peak.Load()
			if cur <= p || h.peak.CompareAndSwap(p, cur) {
				break
			}
		}
		select {
		case <-h.release:
		case <-r.Context().Done():
		}
		h.running.Add(-1)
		writeJSON(w, 200, okBody)
	})
	return h
}

func TestSlots_DefaultIsFour(t *testing.T) {
	c, err := New(Config{APIKey: testKey, TPMBudget: 6000})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := cap(c.slots.ch); got != 4 {
		t.Errorf("default slots = %d, want 4", got)
	}
}

func TestSlots_AtMostMaxConcurrentCallsRunAtOnce(t *testing.T) {
	h := newHoldingServer(t)
	c, _ := newTestClient(t, h.fakeGroq, func(cfg *Config) { cfg.MaxConcurrent = 2 })

	const calls = 6
	var wg sync.WaitGroup
	errs := make(chan error, calls)
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := chatReq()
			req.MaxWait = 30 * time.Second // patient, like a background job
			_, err := c.Complete(context.Background(), req)
			errs <- err
		}()
	}

	eventually(t, "two calls to be running", func() bool { return h.running.Load() == 2 })
	time.Sleep(60 * time.Millisecond) // give a third call every chance to (wrongly) start
	if h.running.Load() != 2 || c.InFlight() != 2 {
		t.Fatalf("%d calls running, %d slots held; want 2 and 2", h.running.Load(), c.InFlight())
	}

	close(h.release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("a waiting call failed: %v", err)
		}
	}
	if h.peak.Load() != 2 || c.InFlight() != 0 {
		t.Errorf("peak %d (want 2), %d slots still held", h.peak.Load(), c.InFlight())
	}
}

func TestSlots_AWaitingCallGivesUpAfterItsMaxWait(t *testing.T) {
	h := newHoldingServer(t)
	c, _ := newTestClient(t, h.fakeGroq, func(cfg *Config) { cfg.MaxConcurrent = 1 })

	first := make(chan error, 1)
	go func() { _, err := c.Complete(context.Background(), chatReq()); first <- err }()
	eventually(t, "the first call to hold the slot", func() bool { return h.running.Load() == 1 })

	second := chatReq()
	second.MaxWait = 50 * time.Millisecond
	start := time.Now()
	_, err := c.Complete(context.Background(), second)

	le := asLLMError(t, err)
	if le.Kind != service.LLMBusy || le.Status != 0 {
		t.Errorf("error = %+v, want busy with no HTTP status", le)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("took %v: it must give up after its MaxWait", time.Since(start))
	}
	if h.count() != 1 {
		t.Errorf("%d requests reached the provider: a call that never got a slot must not be sent", h.count())
	}

	close(h.release)
	if err := <-first; err != nil {
		t.Errorf("first call: %v", err)
	}
	if _, err := c.Complete(context.Background(), chatReq()); err != nil {
		t.Errorf("a later call must get the slot: %v", err)
	}
}

// With ONE slot, a single leaked slot would make the last call hang.
func TestSlots_AreReleasedWhateverHappens(t *testing.T) {
	var mode atomic.Int32 // 0 fine, 1 hangs, 2 unauthorized, 3 server error, 4 stream that breaks
	f := newFakeGroq(t, func(n int, w http.ResponseWriter, r *http.Request) {
		switch mode.Load() {
		case 1:
			blockUntilGone(r)
		case 2:
			writeJSON(w, 401, errorJSON("nope", "invalid_request_error", "invalid_api_key"))
		case 3:
			writeJSON(w, 500, errorJSON("oops", "server_error", "internal"))
		case 4:
			streamOut(w, firstChunk, contentChunk("Hi"))
			panic(http.ErrAbortHandler)
		default:
			writeJSON(w, 200, okBody)
		}
	})
	c, _ := newTestClient(t, f, func(cfg *Config) { cfg.MaxConcurrent = 1 })
	ctx := context.Background()

	mode.Store(1) // the caller leaves while the call is in flight
	cctx, cancel := context.WithCancel(ctx)
	time.AfterFunc(50*time.Millisecond, cancel)
	if _, err := c.Complete(cctx, chatReq()); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled call: %v", err)
	}

	mode.Store(2) // a client error
	if _, err := c.Complete(ctx, chatReq()); err == nil {
		t.Error("expected an error")
	}

	mode.Store(3) // a server error, retried several times (a slot per attempt)
	if _, err := c.Complete(ctx, chatReq()); err == nil {
		t.Error("expected an error")
	}

	mode.Store(4) // a stream that breaks after some text
	if _, err := c.Stream(ctx, chatReq(), func(string) error { return nil }); err == nil {
		t.Error("expected an error")
	}

	if c.InFlight() != 0 {
		t.Fatalf("%d slots are still held", c.InFlight())
	}

	mode.Store(0)
	done := make(chan error, 1)
	go func() { _, err := c.Complete(ctx, chatReq()); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("final call: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the final call never got a slot: one was leaked")
	}
}

func TestSlots_AStreamHoldsItsSlotUntilItEnds(t *testing.T) {
	gate := make(chan struct{})
	f := newFakeGroq(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n != 1 {
			writeJSON(w, 200, okBody)
			return
		}
		streamOut(w, firstChunk, contentChunk("Hi"))
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, finalChunk("stop")+doneEvent)
		w.(http.Flusher).Flush()
	})
	c, _ := newTestClient(t, f, func(cfg *Config) { cfg.MaxConcurrent = 1 })

	gotText := make(chan struct{}, 1)
	streamDone := make(chan error, 1)
	go func() {
		_, err := c.Stream(context.Background(), chatReq(), func(string) error {
			select {
			case gotText <- struct{}{}:
			default:
			}
			return nil
		})
		streamDone <- err
	}()
	<-gotText // the stream is mid-way and holds the only slot

	other := chatReq()
	other.MaxWait = 50 * time.Millisecond
	if _, err := c.Complete(context.Background(), other); asLLMError(t, err).Kind != service.LLMBusy {
		t.Errorf("a second call while the stream runs: %v", err)
	}
	if f.count() != 1 {
		t.Errorf("%d requests reached the provider, want only the stream", f.count())
	}

	close(gate)
	if err := <-streamDone; err != nil {
		t.Fatalf("stream: %v", err)
	}
	if _, err := c.Complete(context.Background(), chatReq()); err != nil {
		t.Errorf("after the stream ended the slot must be free: %v", err)
	}
}