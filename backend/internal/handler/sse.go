package handler

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// sseStream writes server-sent events to one response.
//
//   - The headers (and status 200) are written LAZILY, by the first event, so a
//     failure before that can still be a normal JSON error with a proper status.
//   - Every event is flushed at once.
//   - Data is always JSON on ONE line: a raw newline inside a token would end
//     the event early.
//   - A heartbeat comment (": ping") is written periodically, so proxies do not
//     close a connection that is quietly waiting (for example during a 429 backoff).
//   - All writes go through one mutex: a ResponseWriter is not safe for
//     concurrent use, and the heartbeat runs on its own goroutine.
//   - After the first failed write the error is remembered: later events return
//     it at once, which is how the service learns that the client is gone.
type sseStream struct {
	mu       sync.Mutex
	w        gin.ResponseWriter
	started  bool
	err      error
	interval time.Duration
	stop     chan struct{}
	wg       sync.WaitGroup
}

func newSSEStream(w gin.ResponseWriter, heartbeat time.Duration) *sseStream {
	return &sseStream{w: w, interval: heartbeat, stop: make(chan struct{})}
}

// Started reports whether the headers have been written.
func (s *sseStream) Started() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

// Event writes one named event with a JSON payload.
func (s *sseStream) Event(name string, payload any) error {
	data, err := encodeOneLineJSON(payload)
	if err != nil {
		return err // a programming error, not a client problem
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.startLocked()
	if _, err := io.WriteString(s.w, "event: "+name+"\ndata: "+data+"\n\n"); err != nil {
		s.err = err
		return err
	}
	s.w.Flush()
	return nil
}

func (s *sseStream) startLocked() {
	if s.started {
		return
	}
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("X-Accel-Buffering", "no") // tells nginx not to buffer the stream
	s.w.WriteHeader(http.StatusOK)
	s.w.Flush()
	s.started = true

	if s.interval > 0 {
		s.wg.Add(1)
		go s.heartbeat()
	}
}

func (s *sseStream) heartbeat() {
	defer s.wg.Done()
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.ping()
		case <-s.stop:
			return
		}
	}
}

func (s *sseStream) ping() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || s.err != nil {
		return
	}
	if _, err := io.WriteString(s.w, ": ping\n\n"); err != nil {
		s.err = err
		return
	}
	s.w.Flush()
}

// Close stops the heartbeat and WAITS for it. It must run before the handler
// returns: writing to a ResponseWriter after that is a bug.
func (s *sseStream) Close() {
	s.mu.Lock()
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// encodeOneLineJSON is json.Marshal without HTML escaping (so "<" stays
// readable in curl output). The result never contains a raw newline.
func encodeOneLineJSON(v any) (string, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}