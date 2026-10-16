package service_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/chunker"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/database"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/models"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/parser"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/repository"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/service"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/storage"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/testutil"
)

type env struct {
	pool  *pgxpool.Pool
	docs  *repository.DocumentRepository
	files *storage.Local
	svc   *service.IngestService
	user  *models.User
}

func newEnv(t *testing.T) *env {
	t.Helper()

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	if !strings.Contains(url, "_test") { // this test TRUNCATES tables
		t.Fatal("TEST_DATABASE_URL must point to a database whose name contains '_test'")
	}

	ctx := context.Background()
	if err := database.RunMigrations(url); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	pool, err := database.NewPool(ctx, url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	testutil.LockDatabase(t, url)

	if _, err := pool.Exec(ctx, "TRUNCATE users CASCADE"); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	user, err := repository.NewUserRepository(pool).Create(ctx, "ingest@example.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	files, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	docs := repository.NewDocumentRepository(pool)

	return &env{
		pool:  pool,
		docs:  docs,
		files: files,
		svc:   service.NewIngestService(docs, files, chunker.Default(), service.DefaultIngestTimeout),
		user:  user,
	}
}

// upload does what the upload handler will do in Step 7: store the file, then
// insert the document row (status 'processing').
func (e *env) upload(t *testing.T, fixture, mimeType string) *models.Document {
	t.Helper()
	ctx := context.Background()

	data, err := os.ReadFile(filepath.Join("..", "parser", "testdata", fixture))
	if err != nil {
		t.Fatalf("fixture %s: %v", fixture, err)
	}
	ext, ok := parser.ExtensionFor(mimeType)
	if !ok {
		t.Fatalf("unsupported type %s", mimeType)
	}

	docID := uuid.New()
	rel, size, err := e.files.Save(ctx, e.user.ID, docID, ext, bytes.NewReader(data), 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := e.docs.Create(ctx, &models.Document{
		ID: docID, UserID: e.user.ID, Filename: fixture,
		MimeType: mimeType, SizeBytes: size, StoragePath: rel,
	})
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func (e *env) get(t *testing.T, id uuid.UUID) *models.Document {
	t.Helper()
	doc, err := e.docs.GetByID(context.Background(), e.user.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func (e *env) chunkRows(t *testing.T, id uuid.UUID) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM chunks WHERE document_id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// search runs the full-text query from the plan.
func (e *env) search(t *testing.T, id uuid.UUID, query string) []string {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), `
		SELECT left(content, 80)
		FROM chunks
		WHERE document_id = $1 AND tsv @@ plainto_tsquery('english', $2)
		ORDER BY ts_rank(tsv, plainto_tsquery('english', $2)) DESC
		LIMIT 5`, id, query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestIngest_Fixtures(t *testing.T) {
	e := newEnv(t)

	tests := []struct{ fixture, mime string }{
		{"sample.txt", parser.MimeTXT},
		{"sample.docx", parser.MimeDOCX},
		{"sample.pdf", parser.MimePDF},
	}

	for _, tc := range tests {
		t.Run(tc.fixture, func(t *testing.T) {
			doc := e.upload(t, tc.fixture, tc.mime)

			if err := e.svc.Ingest(context.Background(), doc.ID); err != nil {
				t.Fatalf("Ingest: %v", err)
			}

			got := e.get(t, doc.ID)
			if got.Status != models.StatusReady || got.ChunkCount < 1 || got.ErrorMessage != nil {
				t.Fatalf("document = status %q, chunks %d, error %v", got.Status, got.ChunkCount, got.ErrorMessage)
			}
			if n := e.chunkRows(t, doc.ID); n != got.ChunkCount {
				t.Errorf("chunk rows = %d, chunk_count = %d", n, got.ChunkCount)
			}

			// "laboratory" matches "laboratories" through stemming.
			for _, q := range []string{"zebrafish", "laboratory", "retrieval"} {
				if hits := e.search(t, doc.ID, q); len(hits) == 0 {
					t.Errorf("no chunk matches %q", q)
				}
			}
		})
	}
}

func TestIngest_BadFilesFailWithSafeMessages(t *testing.T) {
	e := newEnv(t)

	tests := []struct{ fixture, mime, wantPhrase string }{
		{"binary.txt", parser.MimeTXT, "not valid UTF-8"},
		{"not-a-zip.docx", parser.MimeDOCX, "could not be read"},
	}

	for _, tc := range tests {
		t.Run(tc.fixture, func(t *testing.T) {
			doc := e.upload(t, tc.fixture, tc.mime)

			if err := e.svc.Ingest(context.Background(), doc.ID); err == nil {
				t.Error("expected Ingest to report the failure")
			}

			got := e.get(t, doc.ID)
			if got.Status != models.StatusFailed || got.ChunkCount != 0 || got.ErrorMessage == nil {
				t.Fatalf("document = status %q, chunks %d, error %v", got.Status, got.ChunkCount, got.ErrorMessage)
			}
			msg := *got.ErrorMessage
			if !strings.Contains(msg, tc.wantPhrase) {
				t.Errorf("message %q does not contain %q", msg, tc.wantPhrase)
			}
			if len(msg) > 160 || strings.ContainsAny(msg, "\n\r") {
				t.Errorf("message is not short and plain: %q", msg)
			}
			if n := e.chunkRows(t, doc.ID); n != 0 {
				t.Errorf("%d chunks stored for a failed document", n)
			}
		})
	}
}

func TestIngest_IsIdempotentAndRecoversFromAnEarlierFailure(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	doc := e.upload(t, "sample.txt", parser.MimeTXT)

	// Simulate an earlier failed attempt.
	if err := e.docs.MarkFailed(ctx, doc.ID, "earlier failure"); err != nil {
		t.Fatal(err)
	}

	var firstCount int
	for run := 1; run <= 2; run++ {
		if err := e.svc.Ingest(ctx, doc.ID); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		got := e.get(t, doc.ID)
		if got.Status != models.StatusReady || got.ErrorMessage != nil {
			t.Fatalf("run %d: status %q, error %v", run, got.Status, got.ErrorMessage)
		}
		if run == 1 {
			firstCount = got.ChunkCount
		}
		if got.ChunkCount != firstCount || e.chunkRows(t, doc.ID) != firstCount {
			t.Errorf("run %d: chunk_count %d, rows %d, want %d (no duplicates)",
				run, got.ChunkCount, e.chunkRows(t, doc.ID), firstCount)
		}
	}
}

func TestIngest_DeletedDocumentIsANoOp(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	doc := e.upload(t, "sample.txt", parser.MimeTXT)

	if _, err := e.docs.Delete(ctx, e.user.ID, doc.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Ingest(ctx, doc.ID); err != nil {
		t.Errorf("Ingest of a deleted document should be a quiet no-op, got %v", err)
	}
}

func (e *env) waitSettled(t *testing.T, id uuid.UUID) *models.Document {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		doc := e.get(t, id)
		if doc.Status != models.StatusProcessing {
			return doc
		}
		if time.Now().After(deadline) {
			t.Fatalf("document %s is still processing after 30s", id)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// What a crash leaves behind is a stored file and a row marked 'processing'.
// At the next start the document must be found and processed again.
func TestRecovery_DocumentsLeftProcessingAreIngestedAgain(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// Two uploads whose ingestion never ran...
	a := e.upload(t, "sample.txt", parser.MimeTXT)
	b := e.upload(t, "sample.docx", parser.MimeDOCX)

	// ...and one finished document, which recovery must leave alone.
	done := e.upload(t, "sample.txt", parser.MimeTXT)
	if err := e.svc.Ingest(ctx, done.ID); err != nil {
		t.Fatal(err)
	}

	ids, err := e.docs.ListProcessing(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := map[uuid.UUID]bool{}
	for _, id := range ids {
		found[id] = true
	}
	if len(ids) != 2 || !found[a.ID] || !found[b.ID] || found[done.ID] {
		t.Fatalf("ListProcessing = %v, want exactly the two unfinished documents", ids)
	}

	queue, err := service.NewIngestQueue(ctx, e.svc, 2, 10)
	if err != nil {
		t.Fatal(err)
	}
	queue.Start()
	t.Cleanup(func() { _ = queue.Shutdown(10 * time.Second) })

	if n := queue.EnqueueAll(ctx, ids); n != 2 {
		t.Fatalf("EnqueueAll queued %d, want 2", n)
	}

	for _, id := range []uuid.UUID{a.ID, b.ID} {
		got := e.waitSettled(t, id)
		if got.Status != models.StatusReady || got.ChunkCount < 1 {
			t.Errorf("document %s after recovery: status %q, chunks %d", id, got.Status, got.ChunkCount)
		}
	}
	if rest, err := e.docs.ListProcessing(ctx); err != nil || len(rest) != 0 {
		t.Errorf("ListProcessing after recovery = (%v, %v), want empty", rest, err)
	}
}