package llm

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/service"
)

func TestPacer_SpendsTheBudgetThenWaits(t *testing.T) {
	ctx := context.Background()
	p := newPacer(60000) // 1000 tokens per second, at most 60000 stored

	if err := p.wait(ctx, 60000, time.Second); err != nil {
		t.Fatalf("a full bucket should not wait: %v", err)
	}

	start := time.Now()
	if err := p.wait(ctx, 100, time.Second); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 70*time.Millisecond || d > time.Second {
		t.Errorf("waited %v for 100 tokens at 1000/s, want about 100ms", d)
	}
}

func TestPacer_FailsFastWhenTheWaitIsTooLong(t *testing.T) {
	ctx := context.Background()
	p := newPacer(60000)
	if err := p.wait(ctx, 60000, time.Second); err != nil {
		t.Fatal(err)
	}

	err := p.wait(ctx, 30000, 100*time.Millisecond)
	le := asLLMError(t, err)
	if le.Kind != service.LLMBusy || le.Status != 0 {
		t.Errorf("kind %v, status %d; want busy with no HTTP status", le.Kind, le.Status)
	}
	if le.RetryAfter < 25*time.Second || le.RetryAfter > 31*time.Second {
		t.Errorf("RetryAfter = %v, want about 30s", le.RetryAfter)
	}
	if retryable(err) {
		t.Error("our own busy answer must not be retried")
	}

	// The refused reservation was given back, so a small call still goes through.
	start := time.Now()
	if err := p.wait(ctx, 100, time.Second); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("a refused reservation was not returned: next call waited %v", d)
	}
}

func TestPacer_StopsWaitingWhenCancelled(t *testing.T) {
	p := newPacer(60000)
	if err := p.wait(context.Background(), 60000, time.Second); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	err := p.wait(ctx, 30000, time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Error("a cancelled wait must return promptly")
	}

	start = time.Now() // and its reservation was returned
	if err := p.wait(context.Background(), 100, time.Second); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Error("a cancelled reservation was not returned")
	}
}

func TestPacer_RejectsRequestsLargerThanTheBudget(t *testing.T) {
	err := newPacer(1000).wait(context.Background(), 1001, time.Hour)
	if le := asLLMError(t, err); le.Kind != service.LLMBadRequest {
		t.Errorf("kind = %v, want bad_request", le.Kind)
	}
}

func TestPacer_ChecksTheContextFirst(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := newPacer(1000).wait(ctx, 10, time.Second); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}