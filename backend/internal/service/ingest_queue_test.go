package service

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// queueRunner is a controllable IngestRunner.
type queueRunner struct {
	mu         sync.Mutex
	started    []uuid.UUID
	finished   []uuid.UUID
	ctxs       []context.Context
	running    int
	maxRunning int

	gate    chan struct{}      // when non-nil, jobs wait until it is closed (or their context ends)
	panicOn map[uuid.UUID]bool // set before the queue starts, then only read
}

func (r *queueRunner) Ingest(ctx context.Context, id uuid.UUID) error {
	r.mu.Lock()
	r.started = append(r.started, id)
	r.ctxs = append(r.ctxs, ctx)
	r.running++
	r.maxRunning = max(r.maxRunning, r.running)
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		r.running--
		r.mu.Unlock()
	}()

	if r.panicOn[id] {
		panic("boom")
	}
	if r.gate != nil {
		select {
		case <-r.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	r.mu.Lock()
	r.finished = append(r.finished, id)
	r.mu.Unlock()
	return nil
}

func (r *queueRunner) startedCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.started)
}

func (r *queueRunner) finishedIDs() []uuid.UUID {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.finished)
}

func (r *queueRunner) maxRunningSeen() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.maxRunning
}

func (r *queueRunner) firstCtx() context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ctxs[0]
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func newTestQueue(t *testing.T, r IngestRunner, workers, capacity int, start bool) *IngestQueue {
	t.Helper()
	q, err := NewIngestQueue(context.Background(), r, workers, capacity)
	if err != nil {
		t.Fatal(err)
	}
	if start {
		q.Start()
	}
	t.Cleanup(func() { _ = q.Shutdown(2 * time.Second) })
	return q
}

func TestNewIngestQueue_Validation(t *testing.T) {
	r := &queueRunner{}
	if _, err := NewIngestQueue(context.Background(), r, 0, 10); err == nil {
		t.Error("zero workers should be rejected")
	}
	if _, err := NewIngestQueue(context.Background(), r, 2, 0); err == nil {
		t.Error("zero capacity should be rejected")
	}
	if _, err := NewIngestQueue(context.Background(), nil, 2, 10); err == nil {
		t.Error("a nil runner should be rejected")
	}
}

// The reason for a pool instead of `go ingest(...)` per upload.
func TestQueue_NeverRunsMoreThanTheConfiguredWorkers(t *testing.T) {
	r := &queueRunner{gate: make(chan struct{})}
	q := newTestQueue(t, r, 3, 50, true)

	const jobs = 20
	for i := 0; i < jobs; i++ {
		if err := q.Submit(uuid.New()); err != nil {
			t.Fatalf("Submit %d: %v", i, err)
		}
	}

	waitFor(t, "three jobs running", func() bool { return r.startedCount() == 3 })
	time.Sleep(100 * time.Millisecond) // give a 4th job every chance to (wrongly) start

	if got := r.startedCount(); got != 3 {
		t.Fatalf("%d jobs started while all 3 workers were busy", got)
	}
	if q.Running() != 3 || q.Queued() != jobs-3 {
		t.Errorf("running %d, queued %d; want 3 and %d", q.Running(), q.Queued(), jobs-3)
	}

	close(r.gate)
	waitFor(t, "all jobs finished", func() bool { return len(r.finishedIDs()) == jobs })
	if got := r.maxRunningSeen(); got != 3 {
		t.Errorf("at most %d jobs ran at once, want exactly 3", got)
	}
}

func TestQueue_SubmitIsNonBlockingAndReportsBusy(t *testing.T) {
	q := newTestQueue(t, &queueRunner{}, 1, 2, false) // workers never start: nothing drains the queue

	for i := 0; i < 2; i++ {
		if err := q.Submit(uuid.New()); err != nil {
			t.Fatalf("Submit %d: %v", i, err)
		}
	}

	done := make(chan error, 1)
	go func() { done <- q.Submit(uuid.New()) }()

	select {
	case err := <-done:
		if !errors.Is(err, ErrBusy) {
			t.Errorf("err = %v, want ErrBusy", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Submit blocked on a full queue")
	}
}

func TestQueue_WorkerSurvivesAPanic(t *testing.T) {
	bad, good := uuid.New(), uuid.New()
	r := &queueRunner{panicOn: map[uuid.UUID]bool{bad: true}}
	q := newTestQueue(t, r, 1, 10, true) // ONE worker: if it died, the next job would never run

	if err := q.Submit(bad); err != nil {
		t.Fatal(err)
	}
	if err := q.Submit(good); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the job after the panic", func() bool { return slices.Contains(r.finishedIDs(), good) })
	if slices.Contains(r.finishedIDs(), bad) {
		t.Error("the panicking job should not count as finished")
	}
	waitFor(t, "the worker to go idle", func() bool { return q.Running() == 0 })
}

func TestQueue_ShutdownFinishesRunningJobsAndLeavesQueuedOnes(t *testing.T) {
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	r := &queueRunner{gate: make(chan struct{})}
	q := newTestQueue(t, r, 1, 10, true)

	for _, id := range []uuid.UUID{a, b, c} {
		if err := q.Submit(id); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "job a to start", func() bool { return r.startedCount() == 1 })

	shutdownErr := make(chan error, 1)
	go func() { shutdownErr <- q.Shutdown(5 * time.Second) }()
	waitFor(t, "the queue to close", q.isClosed)

	// Closed: new work is refused with ErrBusy, which the handler turns into a 503.
	if err := q.Submit(uuid.New()); !errors.Is(err, ErrBusy) {
		t.Errorf("Submit after shutdown began: err = %v, want ErrBusy", err)
	}

	close(r.gate) // the running job is allowed to finish
	select {
	case err := <-shutdownErr:
		if err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return")
	}

	if got := r.finishedIDs(); !slices.Equal(got, []uuid.UUID{a}) {
		t.Errorf("finished = %v, want only the running job", got)
	}
	if got := r.startedCount(); got != 1 {
		t.Errorf("%d jobs started; queued jobs must NOT be started during shutdown", got)
	}
	if err := q.Shutdown(time.Second); err != nil {
		t.Errorf("a second Shutdown should be a no-op, got %v", err)
	}
}

func TestQueue_ShutdownTimeoutInterruptsRunningJobs(t *testing.T) {
	r := &queueRunner{gate: make(chan struct{})} // never released
	q := newTestQueue(t, r, 1, 10, true)

	if err := q.Submit(uuid.New()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the job to start", func() bool { return r.startedCount() == 1 })

	start := time.Now()
	err := q.Shutdown(50 * time.Millisecond)

	if err == nil {
		t.Error("Shutdown should report that a job was interrupted")
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("Shutdown took %v", took)
	}
	if len(r.finishedIDs()) != 0 {
		t.Error("an interrupted job must not count as finished")
	}
	waitFor(t, "the worker to stop", func() bool { return q.Running() == 0 })
}

func TestQueue_JobsRunWithTheApplicationContext(t *testing.T) {
	appCtx, cancelApp := context.WithCancel(context.Background())
	r := &queueRunner{gate: make(chan struct{})}
	q, err := NewIngestQueue(appCtx, r, 1, 5)
	if err != nil {
		t.Fatal(err)
	}
	q.Start()
	t.Cleanup(func() { _ = q.Shutdown(2 * time.Second) })

	if err := q.Submit(uuid.New()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the job to start", func() bool { return r.startedCount() == 1 })

	jobCtx := r.firstCtx()
	if jobCtx.Err() != nil {
		t.Fatal("the job's context was already cancelled when it started")
	}

	cancelApp() // the application's context ends...
	waitFor(t, "the job's context to follow", func() bool { return jobCtx.Err() != nil }) // ...and so does the job's
	waitFor(t, "the job to unwind", func() bool { return q.Running() == 0 })
}

func TestQueue_EnqueueWaitsForRoom(t *testing.T) {
	r := &queueRunner{gate: make(chan struct{})}
	q := newTestQueue(t, r, 1, 1, true)
	a, b, c := uuid.New(), uuid.New(), uuid.New()

	if err := q.Submit(a); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "job a to occupy the worker", func() bool { return r.startedCount() == 1 })
	if err := q.Submit(b); err != nil { // fills the single queue slot
		t.Fatal(err)
	}
	if err := q.Submit(c); !errors.Is(err, ErrBusy) {
		t.Fatalf("the queue should be full, got err = %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- q.Enqueue(context.Background(), c) }()

	select {
	case <-done:
		t.Fatal("Enqueue returned while the queue was still full")
	case <-time.After(100 * time.Millisecond):
	}

	close(r.gate) // a finishes, the worker takes b, a slot opens for c
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Enqueue: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Enqueue never returned")
	}
	waitFor(t, "all three jobs finished", func() bool { return len(r.finishedIDs()) == 3 })
}

func TestQueue_EnqueueStopsWhenCancelledOrClosed(t *testing.T) {
	q := newTestQueue(t, &queueRunner{}, 1, 1, false)
	if err := q.Submit(uuid.New()); err != nil { // now full, and nothing drains it
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	if err := q.Enqueue(ctx, uuid.New()); !errors.Is(err, context.Canceled) {
		t.Errorf("Enqueue err = %v, want context.Canceled", err)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	if n := q.EnqueueAll(ctx2, []uuid.UUID{uuid.New(), uuid.New()}); n != 0 {
		t.Errorf("EnqueueAll queued %d, want 0", n)
	}

	if err := q.Shutdown(time.Second); err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(context.Background(), uuid.New()); !errors.Is(err, ErrBusy) {
		t.Errorf("Enqueue after shutdown: err = %v, want ErrBusy", err)
	}
}