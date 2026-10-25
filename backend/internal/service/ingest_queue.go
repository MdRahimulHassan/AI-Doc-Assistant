package service

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// interruptGrace is how long Shutdown waits for cancelled jobs to unwind.
const interruptGrace = 5 * time.Second

// IngestRunner processes one document. *IngestService satisfies it.
type IngestRunner interface {
	Ingest(ctx context.Context, docID uuid.UUID) error
}

// Compile-time proof that the document service can hand uploads to the queue.
var _ Ingester = (*IngestQueue)(nil)

// IngestQueue runs ingestion jobs on a FIXED number of worker goroutines fed
// by a bounded channel. Resource use is therefore predictable: at most
// `workers` documents are processed at once, however many are uploaded.
//
// The channel is only a convenience. The source of truth is the document's
// status in the database: anything that is lost here (a crash, jobs left in the
// queue at shutdown) is still 'processing' and is queued again at the next start.
type IngestQueue struct {
	runner  IngestRunner
	jobs    chan uuid.UUID
	workers int
	label   string

	// ctx is what jobs run with. It descends from the APPLICATION context
	// given by main, never from an HTTP request: a request context is
	// cancelled the moment its handler returns, which would kill the job.
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	started bool
	closed  bool
	stop    chan struct{} // closed by Shutdown: workers finish the job in hand, then exit
	wg      sync.WaitGroup
	running atomic.Int64
}

func NewJobQueue(appCtx context.Context, label string, runner IngestRunner, workers, capacity int) (*IngestQueue, error) {
	switch {
	case runner == nil:
		return nil, fmt.Errorf("ingest queue: a runner is required")
	case workers < 1:
		return nil, fmt.Errorf("ingest queue: workers must be at least 1, got %d", workers)
	case capacity < 1:
		return nil, fmt.Errorf("ingest queue: capacity must be at least 1, got %d", capacity)
	}

	ctx, cancel := context.WithCancel(appCtx)
	return &IngestQueue{
		runner:  runner,
		jobs:    make(chan uuid.UUID, capacity),
		workers: workers,
		label: label,
		ctx:     ctx,
		cancel:  cancel,
		stop:    make(chan struct{}),
	}, nil
}

// NewIngestQueue is the queue for document ingestion.
func NewIngestQueue(appCtx context.Context, runner IngestRunner, workers, capacity int) (*IngestQueue, error) {
	return NewJobQueue(appCtx, "ingest", runner, workers, capacity)
}


// Start launches the workers. Calling it again does nothing.
func (q *IngestQueue) Start() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.started || q.closed {
		return
	}
	q.started = true

	for i := 1; i <= q.workers; i++ {
		q.wg.Add(1)
		go q.worker(i)
	}
}

// Submit queues a document WITHOUT BLOCKING. If the queue is full it returns
// ErrBusy immediately and the upload handler answers 503. Blocking the request
// on a full queue would only move the problem somewhere harder to see.
func (q *IngestQueue) Submit(id uuid.UUID) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.closed {
		return fmt.Errorf("%w: the server is shutting down", ErrBusy)
	}
	select {
	case q.jobs <- id:
		return nil
	default:
		return ErrBusy
	}
}

// Enqueue is Submit that WAITS for room. It is for startup recovery, where
// there may be more unfinished documents than the queue can hold. It returns
// when the document is queued, the context ends, or the queue closes.
func (q *IngestQueue) Enqueue(ctx context.Context, id uuid.UUID) error {
	if q.isClosed() {
		return fmt.Errorf("%w: the queue is closed", ErrBusy)
	}
	select {
	case q.jobs <- id:
		return nil
	case <-q.stop:
		return fmt.Errorf("%w: the queue is closed", ErrBusy)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// EnqueueAll queues every id, waiting for room as needed, and returns how many
// were queued. It stops early if the context ends or the queue closes; the
// rest stay 'processing' and are found again at the next start.
func (q *IngestQueue) EnqueueAll(ctx context.Context, ids []uuid.UUID) int {
	for i, id := range ids {
		if err := q.Enqueue(ctx, id); err != nil {
			slog.Warn("recovery stopped before every document was queued",
				"queued", i, "total", len(ids), "reason", err)
			return i
		}
	}
	return len(ids)
}

func (q *IngestQueue) worker(n int) {
	defer q.wg.Done()
	for {
		// A stop request wins over waiting jobs: finish the job in hand, never
		// start another one.
		select {
		case <-q.stop:
			return
		default:
		}

		select {
		case <-q.stop:
			return
		case id := <-q.jobs:
			q.run(n, id)
		}
	}
}

// run executes one job. Ingest records its own outcome and logs its own
// failures, so the returned error is deliberately not handled here. The
// recover guarantees one bad job can never kill the worker.
func (q *IngestQueue) run(worker int, id uuid.UUID) {
	running := q.running.Add(1)
	defer q.running.Add(-1)
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("ingest worker recovered from a panic; the worker keeps running",
				"worker", worker, 
				"document_id", id, 
				"panic", rec,
				"queue", q.label,
				"stack", string(debug.Stack()))
		}
	}()

	slog.Info("ingest job started",
		"worker", worker, 
		"document_id", id, 
		"running", running, 
		"queued", len(q.jobs),
		"queue", q.label,
	)

	_ = q.runner.Ingest(q.ctx, id)
}

// Shutdown stops accepting work, lets each running job finish, and does NOT
// start the jobs still waiting (they stay 'processing' for the next start). If
// the running jobs take longer than timeout, they are cancelled, which also
// leaves them 'processing', and an error says how many were interrupted.
func (q *IngestQueue) Shutdown(timeout time.Duration) error {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return nil
	}
	q.closed = true
	close(q.stop)
	q.mu.Unlock()
	defer q.cancel()

	slog.Info("ingest queue stopping", 
	"queue", q.label,
	"running", q.running.Load(), 
	"left_in_queue", len(q.jobs))

	done := make(chan struct{})
	go func() {
		q.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-time.After(timeout):
	}

	// Out of time: interrupt what is still running.
	interrupted := q.running.Load()
	q.cancel()
	select {
	case <-done:
	case <-time.After(interruptGrace):
	}
	return fmt.Errorf("%s queue: %d job(s) did not finish within %v and were interrupted; they will be retried at the next start",
		q.label, interrupted, timeout)
}

// Running is the number of jobs being processed right now.
func (q *IngestQueue) Running() int { return int(q.running.Load()) }

// Queued is the number of jobs waiting for a worker.
func (q *IngestQueue) Queued() int { return len(q.jobs) }

func (q *IngestQueue) isClosed() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.closed
}