package ratelimit

import (
	"fmt"
	"sync"
)

// KeyedSemaphore caps how many things one key may have running at once, for
// example how many answers one user may have streaming. A key with nothing
// running takes no memory.
type KeyedSemaphore struct {
	mu    sync.Mutex
	limit int
	held  map[string]int
}

func NewKeyedSemaphore(limit int) (*KeyedSemaphore, error) {
	if limit < 1 {
		return nil, fmt.Errorf("ratelimit: the concurrency limit must be at least 1, got %d", limit)
	}
	return &KeyedSemaphore{limit: limit, held: make(map[string]int)}, nil
}

// TryAcquire takes a slot for key without waiting. On success the caller MUST
// call release, normally with defer, which also covers a panic. Calling
// release more than once is harmless: only the first call frees the slot.
func (s *KeyedSemaphore) TryAcquire(key string) (release func(), ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.held[key] >= s.limit {
		return nil, false
	}
	s.held[key]++

	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.held[key] <= 1 {
				delete(s.held, key)
			} else {
				s.held[key]--
			}
		})
	}, true
}

// InFlight is how many slots key holds right now.
func (s *KeyedSemaphore) InFlight(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.held[key]
}

// Keys is the number of keys holding at least one slot. Zero when nothing runs:
// a nonzero value at rest would be a leaked slot.
func (s *KeyedSemaphore) Keys() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.held)
}