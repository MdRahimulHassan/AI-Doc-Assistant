package ratelimit

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestKeyedSemaphore_LimitsEachKeySeparately(t *testing.T) {
	s, err := NewKeyedSemaphore(2)
	if err != nil {
		t.Fatal(err)
	}

	r1, ok1 := s.TryAcquire("alice")
	r2, ok2 := s.TryAcquire("alice")
	if !ok1 || !ok2 {
		t.Fatal("the first two slots must be free")
	}
	if _, ok := s.TryAcquire("alice"); ok {
		t.Error("a third slot must be refused")
	}
	rb, ok := s.TryAcquire("bob")
	if !ok {
		t.Error("another key must not be affected")
	}

	r1()
	r3, ok := s.TryAcquire("alice")
	if !ok {
		t.Error("a released slot must be reusable")
	}
	r2()
	r3()
	rb()
	if s.Keys() != 0 || s.InFlight("alice") != 0 {
		t.Errorf("%d keys still hold slots after everything was released", s.Keys())
	}
}

// Releasing the same slot twice must not free somebody else's slot.
func TestKeyedSemaphore_ReleaseIsIdempotent(t *testing.T) {
	s, _ := NewKeyedSemaphore(2)
	r1, _ := s.TryAcquire("k")
	r2, _ := s.TryAcquire("k")

	r1()
	r1()
	r1()
	if got := s.InFlight("k"); got != 1 {
		t.Fatalf("%d slots held after releasing one slot three times, want 1", got)
	}
	if _, ok := s.TryAcquire("k"); !ok {
		t.Error("one slot should be free")
	}
	if _, ok := s.TryAcquire("k"); ok {
		t.Error("only one slot should be free: the repeated release freed a slot it did not own")
	}
	r2()
}

func TestKeyedSemaphore_SlotIsReleasedOnEveryExitPath(t *testing.T) {
	s, _ := NewKeyedSemaphore(1)

	work := func(exit string) (recovered any) {
		defer func() { recovered = recover() }()
		release, ok := s.TryAcquire("k")
		if !ok {
			t.Fatalf("%s: the slot should be free", exit)
		}
		defer release()

		switch exit {
		case "panic":
			panic("handler bug")
		case "early return":
			return nil
		}
		return nil // normal return
	}

	for _, exit := range []string{"normal return", "early return", "panic"} {
		recovered := work(exit)
		if (exit == "panic") != (recovered != nil) {
			t.Errorf("%s: recovered %v", exit, recovered)
		}
		if s.InFlight("k") != 0 || s.Keys() != 0 {
			t.Errorf("%s: the slot was not released", exit)
		}
	}
}

func TestKeyedSemaphore_NeverExceedsTheLimitUnderStress(t *testing.T) {
	s, _ := NewKeyedSemaphore(3)

	var inFlight, peak, granted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 300; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, ok := s.TryAcquire("k")
			if !ok {
				return
			}
			granted.Add(1)
			cur := inFlight.Add(1)
			for {
				p := peak.Load()
				if cur <= p || peak.CompareAndSwap(p, cur) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			inFlight.Add(-1)
			release()
		}()
	}
	wg.Wait()

	if peak.Load() > 3 || granted.Load() == 0 {
		t.Errorf("peak %d (limit 3), %d granted", peak.Load(), granted.Load())
	}
	if s.Keys() != 0 {
		t.Errorf("%d keys leaked", s.Keys())
	}
}

func TestNewKeyedSemaphore_Validation(t *testing.T) {
	for _, n := range []int{0, -1} {
		if _, err := NewKeyedSemaphore(n); err == nil {
			t.Errorf("limit %d should be rejected", n)
		}
	}
}