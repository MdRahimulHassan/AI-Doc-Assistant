package llm

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/service"
)

// slotGate caps how many model calls run at once, ALL USERS TOGETHER. Groq's
// limits belong to the whole organization, so one busy user must not be able to
// use every connection. A streaming call holds its slot until the stream ends.
type slotGate struct{ ch chan struct{} }

func newSlotGate(n int) *slotGate { return &slotGate{ch: make(chan struct{}, n)} }

// acquire waits for a free slot, but at most maxWait. If none frees up in time
// it returns an LLMBusy error: the caller is told "busy" instead of waiting
// forever. If ctx ends first it returns ctx's error. release is idempotent.
func (g *slotGate) acquire(ctx context.Context, maxWait time.Duration) (release func(), err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	select {
	case g.ch <- struct{}{}: // a free slot: no waiting
		return g.releaser(), nil
	default:
	}

	timer := time.NewTimer(maxWait)
	defer timer.Stop()
	select {
	case g.ch <- struct{}{}:
		return g.releaser(), nil
	case <-timer.C:
		return nil, &service.LLMError{
			Kind:    service.LLMBusy,
			Message: fmt.Sprintf("all %d model-call slots stayed busy for %v", cap(g.ch), maxWait),
		}
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (g *slotGate) releaser() func() {
	var once sync.Once
	return func() { once.Do(func() { <-g.ch }) }
}

func (g *slotGate) inFlight() int { return len(g.ch) }