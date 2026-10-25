package repository

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/models"
)

type sumEnv struct {
	docs   *DocumentRepository
	chunks *ChunkRepository
	users  *UserRepository
	pool   *pgxpool.Pool
}

func setupSummary(t *testing.T) *sumEnv {
	t.Helper()
	users, docs, pool := setupDocs(t)
	return &sumEnv{docs: docs, chunks: NewChunkRepository(pool), users: users, pool: pool}
}

// readyDoc creates a document and ingests the chunks, which marks it ready.
func (e *sumEnv) readyDoc(t *testing.T, userID uuid.UUID, chunkTexts ...string) *models.Document {
	t.Helper()
	if len(chunkTexts) == 0 {
		chunkTexts = []string{"one chunk of text"}
	}
	d := mustDoc(t, e.docs, userID, "paper.pdf")
	if err := e.docs.CompleteIngestion(context.Background(), d.ID, chunkTexts); err != nil {
		t.Fatal(err)
	}
	return d
}

func (e *sumEnv) get(t *testing.T, userID, id uuid.UUID) *models.Document {
	t.Helper()
	d, err := e.docs.GetByID(context.Background(), userID, id)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func strVal(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func TestSummary_StartRefusals(t *testing.T) {
	e := setupSummary(t)
	ctx := context.Background()
	alice := mustUser(t, e.users, "alice@example.com")
	bob := mustUser(t, e.users, "bob@example.com")
	doc := e.readyDoc(t, alice.ID, "a", "b", "c")

	for label, c := range map[string]struct{ user, doc uuid.UUID }{
		"another user's document":        {bob.ID, doc.ID},
		"swapped arguments":              {doc.ID, alice.ID},
		"a document that does not exist": {alice.ID, uuid.New()},
	} {
		if _, err := e.docs.StartSummary(ctx, c.user, c.doc, 100); !errors.Is(err, models.ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", label, err)
		}
	}

	processing := mustDoc(t, e.docs, alice.ID, "p.pdf") // never ingested
	failed := mustDoc(t, e.docs, alice.ID, "f.pdf")
	if err := e.docs.MarkFailed(ctx, failed.ID, "failed"); err != nil {
		t.Fatal(err)
	}
	for label, c := range map[string]struct {
		id   uuid.UUID
		want models.DocumentStatus
	}{"still being ingested": {processing.ID, models.StatusProcessing}, "failed ingestion": {failed.ID, models.StatusFailed}} {
		_, err := e.docs.StartSummary(ctx, alice.ID, c.id, 100)
		var nr *models.DocumentNotReadyError
		if !errors.As(err, &nr) || nr.Status != c.want {
			t.Errorf("%s: err = %v, want a not-ready error with status %q", label, err, c.want)
		}
	}

	if _, err := e.docs.StartSummary(ctx, alice.ID, doc.ID, 2); !errors.Is(err, models.ErrDocumentTooLong) {
		t.Errorf("3 chunks with a limit of 2: err = %v, want ErrDocumentTooLong", err)
	}
	if got := e.get(t, alice.ID, doc.ID); got.SummaryStatus != models.SummaryNone {
		t.Errorf("a refused start changed the document: %q", got.SummaryStatus)
	}

	if _, err := e.docs.StartSummary(ctx, alice.ID, doc.ID, 3); err != nil { // exactly at the limit
		t.Fatalf("3 chunks with a limit of 3 must be accepted: %v", err)
	}
	if _, err := e.docs.StartSummary(ctx, alice.ID, doc.ID, 3); !errors.Is(err, models.ErrSummaryInProgress) {
		t.Errorf("second start: err = %v, want ErrSummaryInProgress", err)
	}
}

// Ten simultaneous requests: exactly one may win.
func TestSummary_StartIsExclusiveUnderConcurrency(t *testing.T) {
	e := setupSummary(t)
	ctx := context.Background()
	alice := mustUser(t, e.users, "alice@example.com")

	for round := 0; round < 3; round++ {
		doc := e.readyDoc(t, alice.ID)

		const n = 10
		start := make(chan struct{})
		results := make(chan error, n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := e.docs.StartSummary(ctx, alice.ID, doc.ID, 100)
				results <- err
			}()
		}
		close(start)
		wg.Wait()
		close(results)

		won, refused := 0, 0
		for err := range results {
			switch {
			case err == nil:
				won++
			case errors.Is(err, models.ErrSummaryInProgress):
				refused++
			default:
				t.Errorf("round %d: unexpected error: %v", round, err)
			}
		}
		if won != 1 || refused != n-1 {
			t.Errorf("round %d: %d won and %d refused, want 1 and %d", round, won, refused, n-1)
		}
	}
}

func TestSummary_Lifecycle(t *testing.T) {
	e := setupSummary(t)
	ctx := context.Background()
	u := mustUser(t, e.users, "a@example.com")
	doc := e.readyDoc(t, u.ID)

	// 1. start: previous state "none"
	s, err := e.docs.StartSummary(ctx, u.ID, doc.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if s.PreviousStatus != models.SummaryNone || s.PreviousError != nil || s.Document.SummaryStatus != models.SummaryProcessing {
		t.Errorf("start = previous %q / %v, document %q", s.PreviousStatus, s.PreviousError, s.Document.SummaryStatus)
	}

	// 2. complete
	if err := e.docs.CompleteSummary(ctx, doc.ID, "First summary."); err != nil {
		t.Fatal(err)
	}
	got := e.get(t, u.ID, doc.ID)
	if got.SummaryStatus != models.SummaryReady || strVal(got.Summary) != "First summary." || got.SummaryError != nil || got.SummaryUpdatedAt == nil {
		t.Errorf("after completing: %q, %q, error %v, updated %v", got.SummaryStatus, strVal(got.Summary), got.SummaryError, got.SummaryUpdatedAt)
	}

	// 3. regenerate: the OLD summary stays visible while the new one is written
	s, err = e.docs.StartSummary(ctx, u.ID, doc.ID, 100)
	if err != nil || s.PreviousStatus != models.SummaryReady {
		t.Fatalf("regenerate = (%+v, %v)", s, err)
	}
	if got := e.get(t, u.ID, doc.ID); got.SummaryStatus != models.SummaryProcessing || strVal(got.Summary) != "First summary." {
		t.Errorf("while regenerating: %q, %q", got.SummaryStatus, strVal(got.Summary))
	}

	// 4. a failed regeneration keeps the old summary and records the message
	if err := e.docs.FailSummary(ctx, doc.ID, "It broke."); err != nil {
		t.Fatal(err)
	}
	got = e.get(t, u.ID, doc.ID)
	if got.SummaryStatus != models.SummaryFailed || strVal(got.SummaryError) != "It broke." || strVal(got.Summary) != "First summary." {
		t.Errorf("after failing: %q, error %q, summary %q", got.SummaryStatus, strVal(got.SummaryError), strVal(got.Summary))
	}

	// 5. try again: the error is cleared; completing REPLACES the old summary
	s, err = e.docs.StartSummary(ctx, u.ID, doc.ID, 100)
	if err != nil || s.PreviousStatus != models.SummaryFailed || strVal(s.PreviousError) != "It broke." {
		t.Fatalf("retry = (%+v, %v)", s, err)
	}
	if got := e.get(t, u.ID, doc.ID); got.SummaryError != nil {
		t.Errorf("starting must clear the old error, got %q", strVal(got.SummaryError))
	}
	if err := e.docs.CompleteSummary(ctx, doc.ID, "Second summary."); err != nil {
		t.Fatal(err)
	}
	if got := e.get(t, u.ID, doc.ID); strVal(got.Summary) != "Second summary." || got.SummaryStatus != models.SummaryReady || got.SummaryError != nil {
		t.Errorf("after the second summary: %q, %q, %v", got.SummaryStatus, strVal(got.Summary), got.SummaryError)
	}
}

func TestSummary_CompleteAndFailOnlyWhileProcessing(t *testing.T) {
	e := setupSummary(t)
	ctx := context.Background()
	u := mustUser(t, e.users, "a@example.com")
	doc := e.readyDoc(t, u.ID)

	// Never started: both are refused.
	if err := e.docs.CompleteSummary(ctx, doc.ID, "text"); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("complete without a started summary: %v", err)
	}
	if err := e.docs.FailSummary(ctx, doc.ID, "msg"); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("fail without a started summary: %v", err)
	}

	if _, err := e.docs.StartSummary(ctx, u.ID, doc.ID, 100); err != nil {
		t.Fatal(err)
	}
	if err := e.docs.CompleteSummary(ctx, doc.ID, "  \n "); err == nil || errors.Is(err, models.ErrNotFound) {
		t.Errorf("a blank summary must be refused with its own error, got %v", err)
	}
	if err := e.docs.CompleteSummary(ctx, doc.ID, "text"); err != nil {
		t.Fatal(err)
	}
	// Finished: a late, duplicate completion or failure changes nothing.
	if err := e.docs.CompleteSummary(ctx, doc.ID, "late text"); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("duplicate completion: %v", err)
	}
	if err := e.docs.FailSummary(ctx, doc.ID, "late failure"); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("late failure: %v", err)
	}
	if got := e.get(t, u.ID, doc.ID); strVal(got.Summary) != "text" || got.SummaryStatus != models.SummaryReady {
		t.Errorf("document changed: %q, %q", got.SummaryStatus, strVal(got.Summary))
	}

	// Deleted while processing.
	doc2 := e.readyDoc(t, u.ID)
	if _, err := e.docs.StartSummary(ctx, u.ID, doc2.ID, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := e.docs.Delete(ctx, u.ID, doc2.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.docs.CompleteSummary(ctx, doc2.ID, "text"); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("complete after delete: %v", err)
	}
	if err := e.docs.FailSummary(ctx, doc2.ID, "msg"); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("fail after delete: %v", err)
	}
}

func TestSummary_RevertRestoresThePreviousState(t *testing.T) {
	e := setupSummary(t)
	ctx := context.Background()
	u := mustUser(t, e.users, "a@example.com")
	other := mustUser(t, e.users, "b@example.com")

	check := func(label string, doc *models.Document, wantStatus models.SummaryStatus, wantSummary, wantErr string) {
		t.Helper()
		start, err := e.docs.StartSummary(ctx, u.ID, doc.ID, 100)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if err := e.docs.RevertSummaryStart(ctx, other.ID, doc.ID, start.PreviousStatus, start.PreviousError); !errors.Is(err, models.ErrNotFound) {
			t.Errorf("%s: another user's revert: %v, want ErrNotFound", label, err)
		}
		if err := e.docs.RevertSummaryStart(ctx, u.ID, doc.ID, start.PreviousStatus, start.PreviousError); err != nil {
			t.Fatalf("%s: revert: %v", label, err)
		}
		got := e.get(t, u.ID, doc.ID)
		if got.SummaryStatus != wantStatus || strVal(got.Summary) != wantSummary || strVal(got.SummaryError) != wantErr {
			t.Errorf("%s: after the revert: %q, summary %q, error %q; want %q, %q, %q",
				label, got.SummaryStatus, strVal(got.Summary), strVal(got.SummaryError), wantStatus, wantSummary, wantErr)
		}
		if err := e.docs.RevertSummaryStart(ctx, u.ID, doc.ID, start.PreviousStatus, start.PreviousError); !errors.Is(err, models.ErrNotFound) {
			t.Errorf("%s: a second revert must do nothing, got %v", label, err)
		}
	}

	check("from none", e.readyDoc(t, u.ID), models.SummaryNone, "<nil>", "<nil>")

	ready := e.readyDoc(t, u.ID)
	mustStartAndComplete(t, e, u.ID, ready.ID, "Kept summary.")
	check("from ready", ready, models.SummaryReady, "Kept summary.", "<nil>")

	failed := e.readyDoc(t, u.ID)
	if _, err := e.docs.StartSummary(ctx, u.ID, failed.ID, 100); err != nil {
		t.Fatal(err)
	}
	if err := e.docs.FailSummary(ctx, failed.ID, "Earlier failure."); err != nil {
		t.Fatal(err)
	}
	check("from failed", failed, models.SummaryFailed, "<nil>", "Earlier failure.")
}

func mustStartAndComplete(t *testing.T, e *sumEnv, userID, docID uuid.UUID, summary string) {
	t.Helper()
	ctx := context.Background()
	if _, err := e.docs.StartSummary(ctx, userID, docID, 100); err != nil {
		t.Fatal(err)
	}
	if err := e.docs.CompleteSummary(ctx, docID, summary); err != nil {
		t.Fatal(err)
	}
}

func TestSummary_ListSummarizing(t *testing.T) {
	e := setupSummary(t)
	ctx := context.Background()
	alice := mustUser(t, e.users, "alice@example.com")
	bob := mustUser(t, e.users, "bob@example.com")

	oldest := e.readyDoc(t, alice.ID)
	idle := e.readyDoc(t, bob.ID) // never started
	done := e.readyDoc(t, alice.ID)
	newest := e.readyDoc(t, bob.ID)

	for _, d := range []*models.Document{oldest, done, newest} {
		owner := alice.ID
		if d.ID == newest.ID {
			owner = bob.ID
		}
		if _, err := e.docs.StartSummary(ctx, owner, d.ID, 100); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.docs.CompleteSummary(ctx, done.ID, "done"); err != nil {
		t.Fatal(err)
	}

	base := time.Now().Add(-time.Hour) // explicit timestamps: the order never depends on test speed
	for i, d := range []*models.Document{oldest, idle, done, newest} {
		if _, err := e.pool.Exec(ctx, `UPDATE documents SET created_at = $1 WHERE id = $2`, base.Add(time.Duration(i)*time.Minute), d.ID); err != nil {
			t.Fatal(err)
		}
	}

	got, err := e.docs.ListSummarizing(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []uuid.UUID{oldest.ID, newest.ID}; !slices.Equal(got, want) {
		t.Errorf("ListSummarizing = %v, want %v (both users, oldest first, finished and idle ones excluded)", got, want)
	}

	for _, id := range []uuid.UUID{oldest.ID, newest.ID} {
		if err := e.docs.FailSummary(ctx, id, "x"); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := e.docs.ListSummarizing(ctx); err != nil || got == nil || len(got) != 0 {
		t.Errorf("ListSummarizing = (%v, %v), want an empty non-nil slice", got, err)
	}
}

func TestSummary_ChunkTexts(t *testing.T) {
	e := setupSummary(t)
	ctx := context.Background()
	u := mustUser(t, e.users, "a@example.com")
	doc := e.readyDoc(t, u.ID, "first", "second", "third")

	got, err := e.chunks.ChunkTexts(ctx, doc.ID, 10)
	if err != nil || !slices.Equal(got, []string{"first", "second", "third"}) {
		t.Errorf("ChunkTexts = (%v, %v), want the chunks in reading order", got, err)
	}
	if got, _ := e.chunks.ChunkTexts(ctx, doc.ID, 2); !slices.Equal(got, []string{"first", "second"}) {
		t.Errorf("limit 2 = %v", got)
	}
	if got, _ := e.chunks.ChunkTexts(ctx, doc.ID, 0); len(got) != 1 {
		t.Errorf("limit 0 must be clamped to 1, got %v", got)
	}

	empty := mustDoc(t, e.docs, u.ID, "empty.pdf")
	if got, err := e.chunks.ChunkTexts(ctx, empty.ID, 10); err != nil || got == nil || len(got) != 0 {
		t.Errorf("a document without chunks = (%#v, %v), want an empty non-nil slice", got, err)
	}
}