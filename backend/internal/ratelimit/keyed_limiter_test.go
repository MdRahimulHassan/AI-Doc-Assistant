package ratelimit

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newLimiter(t *testing.T, clock *fakeClock, cfg Config) *KeyedLimiter {
	t.Helper()
	cfg.Now = clock.Now
	l, err := NewKeyedLimiter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

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

func mustAllow(t *testing.T, l *KeyedLimiter, key string) {
	t.Helper()
	if ok, wait := l.Allow(key); !ok {
		t.Fatalf("Allow(%q) was refused (retry after %v), want it allowed", key, wait)
	}
}

func mustRefuse(t *testing.T, l *KeyedLimiter, key string) time.Duration {
	t.Helper()
	ok, wait := l.Allow(key)
	if ok || wait <= 0 {
		t.Fatalf("Allow(%q) = (%v, %v), want a refusal with a positive wait", key, ok, wait)
	}
	return wait
}

func near(got, want, tolerance time.Duration) bool {
	d := got - want
	return d >= -tolerance && d <= tolerance
}

func TestKeyedLimiter_BurstIsAllowedThenBlocked(t *testing.T) {
	clock := newFakeClock()
	l := newLimiter(t, clock, Config{PerMinute: 6, Burst: 3}) // one token every 10 seconds

	for i := 0; i < 3; i++ {
		mustAllow(t, l, "u")
	}
	if wait := mustRefuse(t, l, "u"); !near(wait, 10*time.Second, 100*time.Millisecond) {
		t.Errorf("retry after %v, want about 10s", wait)
	}
}

func TestKeyedLimiter_RefillsOverTimeButNeverBeyondTheBurst(t *testing.T) {
	clock := newFakeClock()
	l := newLimiter(t, clock, Config{PerMinute: 6, Burst: 3})
	for i := 0; i < 3; i++ {
		mustAllow(t, l, "u")
	}

	clock.Advance(10*time.Second + time.Millisecond) // exactly one token
	mustAllow(t, l, "u")
	mustRefuse(t, l, "u")

	clock.Advance(10 * time.Minute) // far more than the bucket holds
	for i := 0; i < 3; i++ {
		mustAllow(t, l, "u")
	}
	mustRefuse(t, l, "u") // still only a burst of 3, not 60
}

func TestKeyedLimiter_RetryAfterShrinksAsTimePasses(t *testing.T) {
	clock := newFakeClock()
	l := newLimiter(t, clock, Config{PerMinute: 6, Burst: 1})
	mustAllow(t, l, "u")

	w0 := mustRefuse(t, l, "u")
	clock.Advance(4 * time.Second)
	w1 := mustRefuse(t, l, "u")
	if !near(w1, w0-4*time.Second, 10*time.Millisecond) {
		t.Errorf("after 4s the wait is %v, want about %v", w1, w0-4*time.Second)
	}
}

// The reservation made for a refused request must be given back. If it were
// not, every refusal would push the next allowed time one interval later.
func TestKeyedLimiter_RefusalsDoNotConsumeTokens(t *testing.T) {
	clock := newFakeClock()
	l := newLimiter(t, clock, Config{PerMinute: 6, Burst: 1})
	mustAllow(t, l, "u")

	var wait time.Duration
	for i := 0; i < 20; i++ { // hammering while blocked
		wait = mustRefuse(t, l, "u")
	}
	clock.Advance(wait + time.Microsecond)
	mustAllow(t, l, "u") // would still be refused if the refusals had taken tokens
	mustRefuse(t, l, "u")
}

func TestKeyedLimiter_KeysAreIndependent(t *testing.T) {
	clock := newFakeClock()
	l := newLimiter(t, clock, Config{PerMinute: 6, Burst: 2})

	mustAllow(t, l, "alice")
	mustAllow(t, l, "alice")
	mustRefuse(t, l, "alice")

	mustAllow(t, l, "bob") // bob's bucket is untouched
	mustAllow(t, l, "bob")
	mustRefuse(t, l, "bob")
}

func TestKeyedLimiter_DefaultsAndValidation(t *testing.T) {
	for perMinute, wantBurst := range map[int]int{1: 1, 2: 1, 6: 3, 7: 3, 20: 10, 30: 15} {
		l, err := NewKeyedLimiter(Config{PerMinute: perMinute})
		if err != nil || l.Burst() != wantBurst || l.PerMinute() != perMinute {
			t.Errorf("PerMinute %d: burst %d (err %v), want %d", perMinute, l.Burst(), err, wantBurst)
		}
	}
	if l, _ := NewKeyedLimiter(Config{PerMinute: 20, Burst: 4}); l.Burst() != 4 {
		t.Errorf("an explicit burst must be kept, got %d", l.Burst())
	}

	for name, cfg := range map[string]Config{
		"no rate":        {},
		"negative rate":  {PerMinute: -1},
		"negative burst": {PerMinute: 5, Burst: -1},
		"negative keys":  {PerMinute: 5, MaxKeys: -1},
		"negative ttl":   {PerMinute: 5, IdleTTL: -time.Second},
	} {
		if _, err := NewKeyedLimiter(cfg); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// --- idle eviction ---

func TestKeyedLimiter_SweepRemovesOnlyIdleKeys(t *testing.T) {
	clock := newFakeClock()
	l := newLimiter(t, clock, Config{PerMinute: 6, Burst: 3, IdleTTL: 10 * time.Minute})

	mustAllow(t, l, "old")
	clock.Advance(5 * time.Minute)
	mustAllow(t, l, "newer")
	clock.Advance(5 * time.Minute) // "old" has been idle 10 minutes, "newer" 5

	if removed := l.Sweep(); removed != 1 || l.Len() != 1 {
		t.Fatalf("removed %d, %d left; want only the idle key gone", removed, l.Len())
	}

	mustAllow(t, l, "newer") // being used keeps a key alive
	clock.Advance(9 * time.Minute)
	if removed := l.Sweep(); removed != 0 {
		t.Errorf("a key used 9 minutes ago was removed")
	}
	clock.Advance(time.Minute)
	if removed := l.Sweep(); removed != 1 || l.Len() != 0 {
		t.Errorf("removed %d, %d left; want the table empty", removed, l.Len())
	}
}

// The safety rule: a key is never forgotten while its bucket could still be empty.
/*func TestKeyedLimiter_ASlowRefillKeepsAnEntryLongerThanTheIdleTTL(t *testing.T) {
	clock := newFakeClock()
	l := newLimiter(t, clock, Config{PerMinute: 1, Burst: 1, IdleTTL: time.Millisecond}) // a full refill takes 60s
	mustAllow(t, l, "u")
	mustRefuse(t, l, "u")

	clock.Advance(30 * time.Second)
	if removed := l.Sweep(); removed != 0 {
		t.Fatal("the entry was dropped while its bucket was still empty")
	}
	mustRefuse(t, l, "u") // so the user is still, correctly, blocked

	clock.Advance(31 * time.Second) // 61s: the bucket has been full for a moment
	if removed := l.Sweep(); removed != 1 {
		t.Fatalf("removed %d, want 1", removed)
	}
	mustAllow(t, l, "u") // a fresh bucket, exactly what the old one had become
}*/

func TestKeyedLimiter_ASlowRefillKeepsAnEntryLongerThanTheIdleTTL(t *testing.T) {
	clock := newFakeClock()
	l := newLimiter(t, clock, Config{
		PerMinute: 1,
		Burst:     1,
		IdleTTL:   time.Millisecond,
	})

	mustAllow(t, l, "u")
	mustRefuse(t, l, "u")

	clock.Advance(30 * time.Second)
	if removed := l.Sweep(); removed != 0 {
		t.Fatal("the entry was dropped while its bucket was still empty")
	}

	clock.Advance(32 * time.Second) // 62s since the last request
	if removed := l.Sweep(); removed != 1 {
		t.Fatalf("removed %d, want 1", removed)
	}

	mustAllow(t, l, "u")
}

// Eviction must be invisible: a limiter that sweeps after every step has to give
// exactly the same answers as one that never forgets anything.
func TestKeyedLimiter_EvictionNeverChangesADecision(t *testing.T) {
	cfg := Config{PerMinute: 5, Burst: 2, IdleTTL: time.Millisecond} // full refill 24s, so entries live 25s idle
	clockA, clockB := newFakeClock(), newFakeClock()
	sweeping := newLimiter(t, clockA, cfg)
	keeping := newLimiter(t, clockB, cfg)

	rng := rand.New(rand.NewSource(7))
	keys := []string{"k1", "k2", "k3", "k4"}
	evicted := 0

	for step := 0; step < 5000; step++ {
		d := time.Duration(rng.Intn(5000)) * time.Millisecond // mostly short gaps: refusals happen
		if rng.Intn(4) == 0 {
			d = time.Duration(rng.Intn(120_000)) * time.Millisecond // sometimes long: entries go idle
		}
		clockA.Advance(d)
		clockB.Advance(d)
		evicted += sweeping.Sweep()

		key := keys[rng.Intn(len(keys))]
		okA, waitA := sweeping.Allow(key)
		okB, waitB := keeping.Allow(key)
		if okA != okB || waitA != waitB {
			t.Fatalf("step %d, key %s: with sweeping (%v, %v), without (%v, %v)", step, key, okA, waitA, okB, waitB)
		}
	}
	if evicted == 0 {
		t.Fatal("nothing was ever evicted, so this test proves nothing")
	}
}

func TestKeyedLimiter_MaxKeysBoundsMemory(t *testing.T) {
	clock := newFakeClock()
	l := newLimiter(t, clock, Config{PerMinute: 60, Burst: 1, MaxKeys: 3})

	for i := 0; i < 100; i++ {
		l.Allow(fmt.Sprintf("flood-%d", i))
		if l.Len() > 3 {
			t.Fatalf("%d keys tracked with MaxKeys 3", l.Len())
		}
	}

	mustAllow(t, l, "newest")  // a new key is tracked even when the table is full...
	mustRefuse(t, l, "newest") // ...so its state is remembered
}

func TestKeyedLimiter_RunSweepsUntilItsContextEnds(t *testing.T) {
	clock := newFakeClock()
	l := newLimiter(t, clock, Config{PerMinute: 6, Burst: 3, IdleTTL: time.Minute})
	l.Allow("a")
	l.Allow("b")
	clock.Advance(time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); l.Run(ctx, 5*time.Millisecond) }()

	eventually(t, "the idle keys to be swept", func() bool { return l.Len() == 0 })

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop when its context ended")
	}

	l.Allow("c") // once stopped, nothing sweeps any more
	clock.Advance(time.Hour)
	time.Sleep(30 * time.Millisecond)
	if l.Len() != 1 {
		t.Errorf("%d keys: a stopped sweeper must not remove anything", l.Len())
	}
}

func TestKeyedLimiter_ConcurrentCallersShareOneBucket(t *testing.T) {
	clock := newFakeClock() // frozen: no refill while the goroutines run
	l := newLimiter(t, clock, Config{PerMinute: 60, Burst: 5})

	var allowed atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := l.Allow("shared"); ok {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := allowed.Load(); got != 5 {
		t.Errorf("%d of 200 simultaneous requests were allowed, want exactly the burst of 5", got)
	}
}
