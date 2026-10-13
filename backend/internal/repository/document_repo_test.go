package repository

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/models"
)

// --- helpers (setup() comes from user_repo_test.go) ---

func setupDocs(t *testing.T) (*UserRepository, *DocumentRepository, *pgxpool.Pool) {
	t.Helper()
	users, pool := setup(t)
	return users, NewDocumentRepository(pool), pool
}

func mustUser(t *testing.T, users *UserRepository, email string) *models.User {
	t.Helper()
	u, err := users.Create(context.Background(), email, "hash")
	if err != nil {
		t.Fatalf("create user %s: %v", email, err)
	}
	return u
}

func mustDoc(t *testing.T, docs *DocumentRepository, userID uuid.UUID, filename string) *models.Document {
	t.Helper()
	id := uuid.New()
	d, err := docs.Create(context.Background(), &models.Document{
		ID:          id,
		UserID:      userID,
		Filename:    filename,
		MimeType:    "application/pdf",
		SizeBytes:   1234,
		StoragePath: userID.String() + "/" + id.String() + ".pdf",
	})
	if err != nil {
		t.Fatalf("create document %s: %v", filename, err)
	}
	return d
}

func readChunks(t *testing.T, pool *pgxpool.Pool, docID uuid.UUID) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT content FROM chunks WHERE document_id = $1 ORDER BY chunk_index`, docID)
	if err != nil {
		t.Fatalf("read chunks: %v", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan chunk: %v", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// --- tests ---

func TestDocument_CreateAndGet(t *testing.T) {
	users, docs, _ := setupDocs(t)
	ctx := context.Background()
	alice := mustUser(t, users, "alice@example.com")

	created := mustDoc(t, docs, alice.ID, "paper.pdf")

	if created.Status != models.StatusProcessing {
		t.Errorf("status = %q, want processing", created.Status)
	}
	if created.ChunkCount != 0 || created.ErrorMessage != nil {
		t.Errorf("new document should have 0 chunks and no error, got %d / %v", created.ChunkCount, created.ErrorMessage)
	}
	if created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
		t.Error("timestamps should be filled by the database")
	}

	got, err := docs.GetByID(ctx, alice.ID, created.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Filename != "paper.pdf" || got.StoragePath != created.StoragePath {
		t.Errorf("GetByID returned %+v", got)
	}
}

// The check from the step: user B can never see or touch user A's documents.
func TestDocument_OwnershipIsolation(t *testing.T) {
	users, docs, _ := setupDocs(t)
	ctx := context.Background()
	alice := mustUser(t, users, "alice@example.com")
	bob := mustUser(t, users, "bob@example.com")
	aliceDoc := mustDoc(t, docs, alice.ID, "alice.pdf")

	// Bob cannot read it.
	_, errOthers := docs.GetByID(ctx, bob.ID, aliceDoc.ID)
	if !errors.Is(errOthers, models.ErrNotFound) {
		t.Fatalf("Bob GetByID err = %v, want ErrNotFound", errOthers)
	}

	// The error is identical to "no such document", so existence isn't revealed.
	_, errMissing := docs.GetByID(ctx, bob.ID, uuid.New())
	if !errors.Is(errMissing, models.ErrNotFound) || errMissing.Error() != errOthers.Error() {
		t.Errorf("missing (%v) and not-yours (%v) must look the same", errMissing, errOthers)
	}

	// Bob cannot delete it...
	if _, err := docs.Delete(ctx, bob.ID, aliceDoc.ID); !errors.Is(err, models.ErrNotFound) {
		t.Fatalf("Bob Delete err = %v, want ErrNotFound", err)
	}
	// ...and his attempt must not have affected it.
	if _, err := docs.GetByID(ctx, alice.ID, aliceDoc.ID); err != nil {
		t.Fatalf("Alice's document was affected by Bob's delete: %v", err)
	}

	// Swapped arguments (both are uuid.UUID, so this compiles) must not find anything.
	if _, err := docs.GetByID(ctx, aliceDoc.ID, alice.ID); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("swapped arguments err = %v, want ErrNotFound", err)
	}

	// Lists are isolated both ways.
	bobList, err := docs.List(ctx, bob.ID, 20, 0)
	if err != nil || len(bobList) != 0 {
		t.Fatalf("Bob's list = %v, %v; want empty", bobList, err)
	}

	bobDoc := mustDoc(t, docs, bob.ID, "bob.pdf")

	aliceList, _ := docs.List(ctx, alice.ID, 20, 0)
	bobList, _ = docs.List(ctx, bob.ID, 20, 0)
	if len(aliceList) != 1 || aliceList[0].ID != aliceDoc.ID {
		t.Errorf("Alice's list = %v, want only her document", aliceList)
	}
	if len(bobList) != 1 || bobList[0].ID != bobDoc.ID {
		t.Errorf("Bob's list = %v, want only his document", bobList)
	}
}

func TestDocument_ListOrderingAndPaging(t *testing.T) {
	users, docs, pool := setupDocs(t)
	ctx := context.Background()
	alice := mustUser(t, users, "alice@example.com")

	// Five documents with explicit, distinct timestamps (oldest = index 0),
	// so the order never depends on how fast the test runs.
	base := time.Now().Add(-time.Hour)
	ids := make([]uuid.UUID, 5)
	for i := range ids {
		d := mustDoc(t, docs, alice.ID, fmt.Sprintf("doc-%d.pdf", i))
		ids[i] = d.ID
		_, err := pool.Exec(ctx, `UPDATE documents SET created_at = $1 WHERE id = $2`,
			base.Add(time.Duration(i)*time.Minute), d.ID)
		if err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name          string
		limit, offset int
		want          []int // indexes into ids, in expected order
	}{
		{"first page", 2, 0, []int{4, 3}},
		{"second page", 2, 2, []int{2, 1}},
		{"last page", 2, 4, []int{0}},
		{"past the end", 2, 10, nil},
		{"zero limit falls back to the default", 0, 0, []int{4, 3, 2, 1, 0}},
		{"negative offset is treated as zero", 2, -5, []int{4, 3}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := docs.List(ctx, alice.ID, tc.limit, tc.offset)
			if err != nil {
				t.Fatal(err)
			}
			if got == nil {
				t.Fatal("List must return a non-nil slice so JSON encodes []")
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d documents, want %d", len(got), len(tc.want))
			}
			for i, idx := range tc.want {
				if got[i].ID != ids[idx] {
					t.Errorf("position %d: got %v, want doc-%d", i, got[i].Filename, idx)
				}
			}
		})
	}
}

func TestDocument_CompleteIngestion(t *testing.T) {
	users, docs, pool := setupDocs(t)
	ctx := context.Background()
	alice := mustUser(t, users, "alice@example.com")
	doc := mustDoc(t, docs, alice.ID, "paper.pdf")

	// Validation: no chunks, and a document that doesn't exist.
	if err := docs.CompleteIngestion(ctx, doc.ID, nil); err == nil {
		t.Error("expected an error for zero chunks")
	}
	if err := docs.CompleteIngestion(ctx, uuid.New(), []string{"x"}); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("missing document err = %v, want ErrNotFound", err)
	}

	chunks := []string{
		"The zebrafish is a small freshwater fish used in biology laboratories.",
		"Information retrieval systems rank documents by how well they match a query.",
		"Classic methods such as BM25 score documents using term frequency.",
	}
	if err := docs.CompleteIngestion(ctx, doc.ID, chunks); err != nil {
		t.Fatalf("CompleteIngestion: %v", err)
	}

	got, err := docs.GetByID(ctx, alice.ID, doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != models.StatusReady || got.ChunkCount != 3 || got.ErrorMessage != nil {
		t.Errorf("document = status %q, chunks %d, error %v", got.Status, got.ChunkCount, got.ErrorMessage)
	}
	if stored := readChunks(t, pool, doc.ID); !reflect.DeepEqual(stored, chunks) {
		t.Errorf("stored chunks = %v, want %v (in order)", stored, chunks)
	}

	// The week's "done when": the chunks are searchable. "laboratory" must
	// match "laboratories" through stemming.
	var hits int
	err = pool.QueryRow(ctx,
		`SELECT count(*) FROM chunks WHERE document_id = $1 AND tsv @@ plainto_tsquery('english', $2)`,
		doc.ID, "laboratory").Scan(&hits)
	if err != nil || hits != 1 {
		t.Errorf("full-text hits = %d (err %v), want 1", hits, err)
	}

	// Idempotent: running again replaces the chunks instead of duplicating them.
	if err := docs.CompleteIngestion(ctx, doc.ID, chunks[:2]); err != nil {
		t.Fatalf("second run: %v", err)
	}
	got, _ = docs.GetByID(ctx, alice.ID, doc.ID)
	if got.ChunkCount != 2 || len(readChunks(t, pool, doc.ID)) != 2 {
		t.Errorf("after re-run: chunk_count %d, rows %d; want 2 and 2", got.ChunkCount, len(readChunks(t, pool, doc.ID)))
	}
}

// Proves the single transaction: a failure halfway through leaves the
// previous good state untouched (old chunks restored, status unchanged).
func TestDocument_CompleteIngestion_RollsBackOnFailure(t *testing.T) {
	users, docs, pool := setupDocs(t)
	ctx := context.Background()
	alice := mustUser(t, users, "alice@example.com")
	doc := mustDoc(t, docs, alice.ID, "paper.pdf")

	good := []string{"first good chunk", "second good chunk"}
	if err := docs.CompleteIngestion(ctx, doc.ID, good); err != nil {
		t.Fatal(err)
	}

	// Postgres text columns reject NUL bytes, so the COPY fails midway.
	bad := []string{"fine", "bad\x00chunk", "fine"}
	if err := docs.CompleteIngestion(ctx, doc.ID, bad); err == nil {
		t.Fatal("expected an error for a chunk containing a NUL byte")
	}

	got, err := docs.GetByID(ctx, alice.ID, doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != models.StatusReady || got.ChunkCount != 2 {
		t.Errorf("document changed despite rollback: status %q, chunks %d", got.Status, got.ChunkCount)
	}
	if stored := readChunks(t, pool, doc.ID); !reflect.DeepEqual(stored, good) {
		t.Errorf("chunks after failed run = %v, want the original %v", stored, good)
	}
}

// Five simultaneous ingestions of the same document must serialize cleanly:
// no unique violations, no mixed-up chunks.
func TestDocument_CompleteIngestion_Concurrent(t *testing.T) {
	users, docs, pool := setupDocs(t)
	ctx := context.Background()
	alice := mustUser(t, users, "alice@example.com")
	doc := mustDoc(t, docs, alice.ID, "paper.pdf")

	const n = 5
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for size := 1; size <= n; size++ {
		wg.Add(1)
		go func(size int) {
			defer wg.Done()
			chunks := make([]string, size)
			for j := range chunks {
				chunks[j] = fmt.Sprintf("run %d chunk %d", size, j)
			}
			errs <- docs.CompleteIngestion(ctx, doc.ID, chunks)
		}(size)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent ingestion failed: %v", err)
		}
	}

	got, _ := docs.GetByID(ctx, alice.ID, doc.ID)
	stored := readChunks(t, pool, doc.ID)
	if len(stored) != got.ChunkCount || got.ChunkCount < 1 || got.ChunkCount > n {
		t.Fatalf("chunk_count %d and stored rows %d disagree", got.ChunkCount, len(stored))
	}
	// Every stored chunk must come from the SAME run (the winner).
	for i, content := range stored {
		if want := fmt.Sprintf("run %d chunk %d", got.ChunkCount, i); content != want {
			t.Errorf("chunk %d = %q, want %q: runs interleaved", i, content, want)
		}
	}
}

func TestDocument_MarkFailed(t *testing.T) {
	users, docs, _ := setupDocs(t)
	ctx := context.Background()
	alice := mustUser(t, users, "alice@example.com")
	doc := mustDoc(t, docs, alice.ID, "scan.pdf")

	if err := docs.MarkFailed(ctx, doc.ID, "no extractable text"); err != nil {
		t.Fatal(err)
	}
	got, _ := docs.GetByID(ctx, alice.ID, doc.ID)
	if got.Status != models.StatusFailed || got.ErrorMessage == nil || *got.ErrorMessage != "no extractable text" {
		t.Errorf("after MarkFailed: status %q, error %v", got.Status, got.ErrorMessage)
	}

	if err := docs.MarkFailed(ctx, uuid.New(), "x"); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("missing document err = %v, want ErrNotFound", err)
	}

	// A later successful ingestion clears the old error.
	if err := docs.CompleteIngestion(ctx, doc.ID, []string{"now it works"}); err != nil {
		t.Fatal(err)
	}
	got, _ = docs.GetByID(ctx, alice.ID, doc.ID)
	if got.Status != models.StatusReady || got.ErrorMessage != nil {
		t.Errorf("after recovery: status %q, error %v", got.Status, got.ErrorMessage)
	}
}

func TestDocument_DeleteReturnsPathAndCascades(t *testing.T) {
	users, docs, pool := setupDocs(t)
	ctx := context.Background()
	alice := mustUser(t, users, "alice@example.com")
	doc := mustDoc(t, docs, alice.ID, "paper.pdf")
	if err := docs.CompleteIngestion(ctx, doc.ID, []string{"one", "two"}); err != nil {
		t.Fatal(err)
	}

	path, err := docs.Delete(ctx, alice.ID, doc.ID)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if path != doc.StoragePath {
		t.Errorf("returned path %q, want %q", path, doc.StoragePath)
	}
	if _, err := docs.GetByID(ctx, alice.ID, doc.ID); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("document still readable after delete: %v", err)
	}
	if left := readChunks(t, pool, doc.ID); len(left) != 0 {
		t.Errorf("%d chunks survived the delete; cascade is broken", len(left))
	}
	if _, err := docs.Delete(ctx, alice.ID, doc.ID); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("second delete err = %v, want ErrNotFound", err)
	}

	// Deleting a USER removes their documents and chunks too.
	doc2 := mustDoc(t, docs, alice.ID, "other.pdf")
	if err := docs.CompleteIngestion(ctx, doc2.ID, []string{"x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, alice.ID); err != nil {
		t.Fatal(err)
	}
	var remaining int
	err = pool.QueryRow(ctx, `SELECT count(*) FROM documents WHERE id = $1`, doc2.ID).Scan(&remaining)
	if err != nil || remaining != 0 || len(readChunks(t, pool, doc2.ID)) != 0 {
		t.Errorf("user delete did not cascade (documents left: %d, err: %v)", remaining, err)
	}
}

// Keeps the Go constants and the database CHECK constraint from drifting apart.
func TestDocument_StatusConstantsMatchDatabase(t *testing.T) {
	users, docs, pool := setupDocs(t)
	ctx := context.Background()
	alice := mustUser(t, users, "alice@example.com")
	doc := mustDoc(t, docs, alice.ID, "paper.pdf")

	for _, s := range []models.DocumentStatus{models.StatusProcessing, models.StatusReady, models.StatusFailed} {
		if _, err := pool.Exec(ctx, `UPDATE documents SET status = $1 WHERE id = $2`, string(s), doc.ID); err != nil {
			t.Errorf("database rejected status %q: %v", s, err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE documents SET status = 'bogus' WHERE id = $1`, doc.ID); err == nil {
		t.Error("database accepted an invalid status")
	}
}