package llm

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/time/rate"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/service"
)

// pacer keeps this app inside its share of the provider's per-minute token
// limit, which all users and background jobs share. It is a token bucket that
// holds up to one minute's budget and refills continuously, like the provider's
// own limiter (your captured reset-tokens header showed that shape).
type pacer struct {
	lim      *rate.Limiter
	capacity int
}

func newPacer(tokensPerMinute int) *pacer {
	return &pacer{
		lim:      rate.NewLimiter(rate.Limit(float64(tokensPerMinute)/60), tokensPerMinute),
		capacity: tokensPerMinute,
	}
}

// wait reserves `tokens` and sleeps until they are available.
//   - If the wait would be longer than maxWait it returns an LLMBusy error
//     right away, carrying how long the caller should wait. That is the
//     "fail fast instead of making a user wait" rule.
//   - A refused or interrupted reservation is given back.
func (p *pacer) wait(ctx context.Context, tokens int, maxWait time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tokens = max(tokens, 1)

	r := p.lim.ReserveN(time.Now(), tokens)
	if !r.OK() {
		return &service.LLMError{
			Kind: service.LLMBadRequest,
			Message: fmt.Sprintf("the request is estimated at %d tokens, more than the whole per-minute budget of %d",
				tokens, p.capacity),
		}
	}

	d := r.Delay()
	if d <= 0 {
		return nil
	}
	if d > maxWait {
		r.Cancel()
		return &service.LLMError{
			Kind:       service.LLMBusy,
			RetryAfter: d,
			Message:    "the shared token budget is used up for now",
		}
	}
	if err := sleepContext(ctx, d); err != nil {
		r.Cancel()
		return err
	}
	return nil
}