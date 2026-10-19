package llm

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/service"
)

// retryable reports whether sending the same request again can help.
func retryable(err error) bool {
	var le *service.LLMError
	if !errors.As(err, &le) {
		return false // context errors, programming errors
	}
	switch le.Kind {
	case service.LLMBusy:
		// Only a 429 from the provider clears with time. Our own pacer's
		// "busy" (Status 0) already waited as long as it was allowed to.
		return le.Status == http.StatusTooManyRequests
	case service.LLMUnavailable, service.LLMTimeout:
		return true
	}
	return false // bad configuration, bad request, bad response
}

// backoffCeiling is the upper bound of the wait before retry number `attempt`
// (1 = after the first failure): base, 2*base, 4*base ... capped at max.
func backoffCeiling(base, max time.Duration, attempt int) time.Duration {
	d := base
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= max {
			return max
		}
	}
	return min(d, max)
}

// fullJitter picks a random wait between 0 and the ceiling. Randomness stops many
// clients that failed together from retrying together.
func fullJitter(ceiling time.Duration) time.Duration {
	if ceiling <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(ceiling) + 1))
}

// sleepContext waits for d, or returns early with the context's error.
func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// parseRetryAfter reads a Retry-After header: a number of seconds (possibly
// fractional) or an HTTP date. Missing, zero, negative or unreadable values
// give 0, meaning "the provider did not say". Absurd values are capped at 24h.
func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(value, 64); err == nil {
		if math.IsNaN(secs) || secs <= 0 {
			return 0
		}
		return time.Duration(math.Min(secs, 24*3600) * float64(time.Second))
	}
	if t, err := http.ParseTime(value); err == nil {
		if d := t.Sub(now); d > 0 {
			return min(d, 24*time.Hour)
		}
	}
	return 0
}