// Package ratelimit holds the in-memory limiters used by the HTTP layer: a token
// bucket per key, and a cap on simultaneous requests per key. Both are per
// PROCESS: several server instances would each allow their own share.
package ratelimit

import (
	"context"
	"fmt"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	defaultMaxKeys = 100_000
	defaultIdleTTL = 10 * time.Minute

	// refillMargin makes an entry wait slightly LONGER than the time a drained
	// bucket needs to refill, so rounding can never make eviction hand out a
	// token that was not earned.
	refillMargin = time.Second
)

// Config describes a keyed limiter. Zero values mean "use the default".
type Config struct {
	PerMinute int              // sustained rate per key (required)
	Burst     int              // bucket size; default DefaultBurst(PerMinute)
	MaxKeys   int              // most keys tracked at once; default 100,000
	IdleTTL   time.Duration    // forget a key idle this long; default 10 minutes, never shorter than a full refill
	Now       func() time.Time // the clock; tests inject a fake one (default time.Now)
}

// DefaultBurst is half a minute's worth of requests, and at least one.
func DefaultBurst(perMinute int) int { return max(1, perMinute/2) }

type entry struct {
	lim      *rate.Limiter
	lastSeen time.Time
}

// KeyedLimiter is a token bucket per key (a user ID, an IP address). It is safe
// for concurrent use.
type KeyedLimiter struct {
	mu        sync.Mutex
	entries   map[string]*entry
	limit     rate.Limit
	burst     int
	perMinute int
	maxKeys   int
	idle      time.Duration
	now       func() time.Time
}

func NewKeyedLimiter(cfg Config) (*KeyedLimiter, error) {
	switch {
	case cfg.PerMinute < 1:
		return nil, fmt.Errorf("ratelimit: PerMinute must be at least 1, got %d", cfg.PerMinute)
	case cfg.Burst < 0:
		return nil, fmt.Errorf("ratelimit: Burst must not be negative, got %d", cfg.Burst)
	case cfg.MaxKeys < 0:
		return nil, fmt.Errorf("ratelimit: MaxKeys must not be negative, got %d", cfg.MaxKeys)
	case cfg.IdleTTL < 0:
		return nil, fmt.Errorf("ratelimit: IdleTTL must not be negative, got %v", cfg.IdleTTL)
	}

	burst := cfg.Burst
	if burst == 0 {
		burst = DefaultBurst(cfg.PerMinute)
	}
	maxKeys := cfg.MaxKeys
	if maxKeys == 0 {
		maxKeys = defaultMaxKeys
	}
	idleTTL := cfg.IdleTTL
	if idleTTL == 0 {
		idleTTL = defaultIdleTTL
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	limit := rate.Limit(float64(cfg.PerMinute) / 60)
	refill := time.Duration(float64(burst) / float64(limit) * float64(time.Second)) // empty bucket -> full

	return &KeyedLimiter{
		entries:   make(map[string]*entry),
		limit:     limit,
		burst:     burst,
		perMinute: cfg.PerMinute,
		maxKeys:   maxKeys,
		idle:      max(idleTTL, refill+refillMargin),
		now:       now,
	}, nil
}

// Allow takes one token for key. If there is none it returns false and how long
// to wait for the next one. A refused request takes NOTHING: the reservation is
// cancelled, so being refused never pushes the next allowed time later.
func (l *KeyedLimiter) Allow(key string) (allowed bool, retryAfter time.Duration) {
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	e, ok := l.entries[key]
	if !ok {
		if len(l.entries) >= l.maxKeys {
			l.evictOneLocked()
		}
		e = &entry{lim: rate.NewLimiter(l.limit, l.burst)}
		l.entries[key] = e
	}
	e.lastSeen = now

	r := e.lim.ReserveN(now, 1)
	if !r.OK() { // cannot happen for one token and burst >= 1; refuse safely anyway
		return false, time.Second
	}
	if delay := r.DelayFrom(now); delay > 0 {
		r.CancelAt(now) // give the token back: this request will not happen
		return false, delay
	}
	return true, 0
}

// Sweep forgets keys that have been idle long enough that their bucket is full
// again, and returns how many it removed. Forgetting such a key changes nothing:
// a new bucket starts full too.
func (l *KeyedLimiter) Sweep() int {
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	removed := 0
	for k, e := range l.entries {
		if now.Sub(e.lastSeen) >= l.idle {
			delete(l.entries, k)
			removed++
		}
	}
	return removed
}

/*func (l *KeyedLimiter) Sweep() int {
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	removed := 0
	for k, e := range l.entries {
		idleFor := now.Sub(e.lastSeen)
		if idleFor >= l.idle {
			delete(l.entries, k)
			removed++
		}
		fmt.Printf("DEBUG key=%q idleFor=%v requiredIdle=%v removed=%v\n",
			k, idleFor, l.idle, idleFor >= l.idle)
	}
	return removed
}*/

// Run sweeps every interval until ctx ends. Start it with `go l.Run(appCtx, time.Minute)`.
func (l *KeyedLimiter) Run(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			l.Sweep()
		case <-ctx.Done():
			return
		}
	}
}

// Len is the number of keys currently tracked.
func (l *KeyedLimiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

func (l *KeyedLimiter) PerMinute() int { return l.perMinute }
func (l *KeyedLimiter) Burst() int     { return l.burst }

// evictOneLocked makes room for a new key by dropping an arbitrary one (Go's
// map iteration order is unspecified). It is O(1), which matters under a flood
// of unique keys; idle keys are normally removed by the periodic Sweep.
func (l *KeyedLimiter) evictOneLocked() {
	for k := range l.entries {
		delete(l.entries, k)
		return
	}
}
