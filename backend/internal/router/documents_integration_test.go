package router_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/chunker"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/database"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/repository"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/router"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/service"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/storage"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/testutil"
)

const testSecret = "0123456789abcdef0123456789abcdef"

type stack struct {
	handler http.Handler
	pool    *pgxpool.Pool
	uploads string // the upload directory
	alice   string // bearer tokens for two different users
	bob     string
}

func newStack(t *testing.T, maxUpload int64) *stack {
	t.Helper()

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	if !strings.Contains(url, "_test") { // this test TRUNCATES tables
		t.Fatal("TEST_DATABASE_URL must point to a database whose name contains '_test'")
	}

	gin.SetMode(gin.TestMode)
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

	tokens, err := service.NewTokenManager(testSecret, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	users := repository.NewUserRepository(pool)
	tokenFor := func(email string) string {
		u, err := users.Create(ctx, email, "hash")
		if err != nil {
			t.Fatal(err)
		}
		tok, _, err := tokens.Generate(u.ID)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	authSvc, err := service.NewAuthService(users, tokens, bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}

	uploads := t.TempDir()
	files, err := storage.NewLocal(uploads)
	if err != nil {
		t.Fatal(err)
	}
	docRepo := repository.NewDocumentRepository(pool)
	ingest := service.NewIngestService(docRepo, files, chunker.Default(), service.DefaultIngestTimeout)
	docs := service.NewDocumentService(docRepo, files, service.NewInlineIngester(ingest), maxUpload)

	return &stack{
		handler: router.New(router.Deps{
			DB: pool, Auth: authSvc, Documents: docs, Tokens: tokens,
			CORSOrigins: []string{"http://localhost:5173"}, MaxUploadBytes: maxUpload,
		}),
		pool:    pool,
		uploads: uploads,
		alice:   tokenFor("alice@example.com"),
		bob:     tokenFor("bob@example.com"),
	}
}

type result struct {
	Code    int
	Body    string
	Data    json.RawMessage
	ErrCode string
}

func (s *stack) do(t *testing.T, method, path, token string, body io.Reader, ctype string) result {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.handler.ServeHTTP(w, req)

	res := result{Code: w.Code, Body: w.Body.String()}
	if w.Body.Len() > 0 {
		var env struct {
			Data  json.RawMessage `json:"data"`
			Error *struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("not an envelope: %v\n%s", err, w.Body.String())
		}
		res.Data = env.Data
		if env.Error != nil {
			res.ErrCode = env.Error.Code
		}
	}
	return res
}

func (s *stack) upload(t *testing.T, token, filename string, content []byte) result {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return s.do(t, http.MethodPost, "/api/documents", token, &body, mw.FormDataContentType())
}

type docJSON struct {
	ID           string  `json:"id"`
	Filename     string  `json:"filename"`
	MimeType     string  `json:"mime_type"`
	Status       string  `json:"status"`
	ErrorMessage *string `json:"error_message"`
	ChunkCount   int     `json:"chunk_count"`
}

func decodeDoc(t *testing.T, res result) docJSON {
	t.Helper()
	var d struct {
		Document docJSON `json:"document"`
	}
	if err := json.Unmarshal(res.Data, &d); err != nil {
		t.Fatalf("decode document: %v\n%s", err, res.Body)
	}
	return d.Document
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "parser", "testdata", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return data
}

func (s *stack) count(t *testing.T, query string) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func filesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// --- tests ---

func TestDocumentsAPI_UploadIngestSearchDelete(t *testing.T) {
	s := newStack(t, 10<<20)

	for _, name := range []string{"sample.txt", "sample.docx", "sample.pdf"} {
		t.Run(name, func(t *testing.T) {
			res := s.upload(t, s.alice, name, fixture(t, name))
			if res.Code != http.StatusAccepted {
				t.Fatalf("upload: status %d: %s", res.Code, res.Body)
			}
			for _, leaked := range []string{"storage_path", "user_id"} {
				if strings.Contains(res.Body, leaked) {
					t.Errorf("response contains %q", leaked)
				}
			}
			doc := decodeDoc(t, res)
			if doc.Status != "processing" || doc.Filename != name {
				t.Errorf("created document = %+v", doc)
			}

			// Ingestion ran inline, so the document is already ready.
			got := decodeDoc(t, s.do(t, http.MethodGet, "/api/documents/"+doc.ID, s.alice, nil, ""))
			if got.Status != "ready" || got.ChunkCount < 1 || got.ErrorMessage != nil {
				t.Fatalf("after ingestion: %+v", got)
			}

			hits := s.count(t, `SELECT count(*) FROM chunks WHERE document_id = '`+doc.ID+
				`' AND tsv @@ plainto_tsquery('english', 'zebrafish')`)
			if hits == 0 {
				t.Error("the uploaded document is not searchable")
			}

			// Delete: 204, then gone everywhere (row, chunks and file).
			if res := s.do(t, http.MethodDelete, "/api/documents/"+doc.ID, s.alice, nil, ""); res.Code != http.StatusNoContent {
				t.Fatalf("delete: status %d: %s", res.Code, res.Body)
			}
			if res := s.do(t, http.MethodGet, "/api/documents/"+doc.ID, s.alice, nil, ""); res.Code != http.StatusNotFound {
				t.Errorf("get after delete: status %d", res.Code)
			}
			if n := s.count(t, `SELECT count(*) FROM chunks WHERE document_id = '`+doc.ID+`'`); n != 0 {
				t.Errorf("%d chunks survived the delete", n)
			}
			if left := filesUnder(t, s.uploads); len(left) != 0 {
				t.Errorf("files left on disk: %v", left)
			}
		})
	}
}

func TestDocumentsAPI_OwnershipIsolation(t *testing.T) {
	s := newStack(t, 10<<20)

	doc := decodeDoc(t, s.upload(t, s.alice, "sample.txt", fixture(t, "sample.txt")))

	// Bob cannot read, delete, or see Alice's document; each answer looks like "no such document".
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		res := s.do(t, method, "/api/documents/"+doc.ID, s.bob, nil, "")
		if res.Code != http.StatusNotFound || res.ErrCode != "NOT_FOUND" {
			t.Errorf("Bob %s: status %d, code %q", method, res.Code, res.ErrCode)
		}
	}
	if res := s.do(t, http.MethodGet, "/api/documents", s.bob, nil, ""); !strings.Contains(res.Body, `"documents":[]`) {
		t.Errorf("Bob's list should be empty: %s", res.Body)
	}

	// Alice's document is untouched by Bob's attempts.
	if res := s.do(t, http.MethodGet, "/api/documents/"+doc.ID, s.alice, nil, ""); res.Code != http.StatusOK {
		t.Errorf("Alice's document was affected: status %d", res.Code)
	}
	if len(filesUnder(t, s.uploads)) != 1 {
		t.Error("Alice's file should still exist")
	}
}

func TestDocumentsAPI_RejectsBadUploads(t *testing.T) {
	s := newStack(t, 10<<20)

	tests := []struct {
		name     string
		filename string
		content  []byte
		token    string
		want     int
		wantCode string
	}{
		{"pdf renamed to .docx", "evil.docx", fixture(t, "sample.pdf"), s.alice, 415, "UNSUPPORTED_FILE_TYPE"},
		{"docx renamed to .pdf", "evil.pdf", fixture(t, "sample.docx"), s.alice, 415, "UNSUPPORTED_FILE_TYPE"},
		{"binary junk named .txt", "binary.txt", fixture(t, "binary.txt"), s.alice, 415, "UNSUPPORTED_FILE_TYPE"},
		{"plain text named .docx", "not-a-zip.docx", fixture(t, "not-a-zip.docx"), s.alice, 415, "UNSUPPORTED_FILE_TYPE"},
		{"zip without word/document.xml", "x.docx", fixture(t, "no-document-xml.docx"), s.alice, 415, "UNSUPPORTED_FILE_TYPE"},
		{"empty file", "empty.pdf", nil, s.alice, 400, "NO_FILE"},
		{"no token", "sample.txt", fixture(t, "sample.txt"), "", 401, "UNAUTHORIZED"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := s.upload(t, tc.token, tc.filename, tc.content)
			if res.Code != tc.want || res.ErrCode != tc.wantCode {
				t.Errorf("status %d, code %q; want %d %q: %s", res.Code, res.ErrCode, tc.want, tc.wantCode, res.Body)
			}
		})
	}

	t.Run("no file field, and not multipart", func(t *testing.T) {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		_ = mw.WriteField("note", "hello")
		_ = mw.Close()
		res := s.do(t, http.MethodPost, "/api/documents", s.alice, &body, mw.FormDataContentType())
		if res.Code != 400 || res.ErrCode != "NO_FILE" {
			t.Errorf("no file field: status %d, code %q", res.Code, res.ErrCode)
		}

		res = s.do(t, http.MethodPost, "/api/documents", s.alice, strings.NewReader(`{}`), "application/json")
		if res.Code != 400 || res.ErrCode != "BAD_REQUEST" {
			t.Errorf("not multipart: status %d, code %q", res.Code, res.ErrCode)
		}
	})

	// Nothing from any rejected request may remain anywhere.
	if n := s.count(t, `SELECT count(*) FROM documents`); n != 0 {
		t.Errorf("%d documents were stored", n)
	}
	if left := filesUnder(t, s.uploads); len(left) != 0 {
		t.Errorf("files left on disk: %v", left)
	}
}

func TestDocumentsAPI_TooLarge(t *testing.T) {
	s := newStack(t, 4<<10) // a 4 KiB limit

	// 5000 bytes: fits in the body cap (limit + 64 KiB framing) but exceeds the file limit.
	res := s.upload(t, s.alice, "big.txt", []byte(strings.Repeat("word ", 1000)))
	if res.Code != http.StatusRequestEntityTooLarge || res.ErrCode != "FILE_TOO_LARGE" {
		t.Errorf("slightly over the limit: status %d, code %q", res.Code, res.ErrCode)
	}

	// 200 KiB: rejected by the body cap before the service is involved.
	res = s.upload(t, s.alice, "huge.txt", []byte(strings.Repeat("word ", 40000)))
	if res.Code != http.StatusRequestEntityTooLarge || res.ErrCode != "FILE_TOO_LARGE" {
		t.Errorf("far over the limit: status %d, code %q", res.Code, res.ErrCode)
	}

	if n := s.count(t, `SELECT count(*) FROM documents`); n != 0 || len(filesUnder(t, s.uploads)) != 0 {
		t.Errorf("an oversized upload left traces: %d rows, files %v", n, filesUnder(t, s.uploads))
	}
}

func TestDocumentsAPI_FilenameCannotEscapeTheUploadDirectory(t *testing.T) {
	s := newStack(t, 10<<20)

	for _, name := range []string{"../../evil.txt", `..\..\evil.txt`, "/etc/evil.txt"} {
		res := s.upload(t, s.alice, name, fixture(t, "sample.txt"))
		if res.Code != http.StatusAccepted {
			t.Fatalf("%q: status %d: %s", name, res.Code, res.Body)
		}
		if doc := decodeDoc(t, res); doc.Filename != "evil.txt" {
			t.Errorf("%q: display name = %q, want evil.txt", name, doc.Filename)
		}
	}

	files := filesUnder(t, s.uploads)
	if len(files) != 3 {
		t.Fatalf("stored %d files, want 3: %v", len(files), files)
	}
	for _, f := range files {
		rel, err := filepath.Rel(s.uploads, f)
		if err != nil || strings.Contains(rel, "evil") || strings.Contains(rel, "..") {
			t.Errorf("stored file %q (relative %q) is not named by IDs", f, rel)
		}
	}
}

func TestDocumentsAPI_ListPaging(t *testing.T) {
	s := newStack(t, 10<<20)
	for i := 0; i < 3; i++ {
		if res := s.upload(t, s.alice, "sample.txt", fixture(t, "sample.txt")); res.Code != http.StatusAccepted {
			t.Fatalf("upload %d: status %d", i, res.Code)
		}
	}

	count := func(query string) int {
		res := s.do(t, http.MethodGet, "/api/documents"+query, s.alice, nil, "")
		if res.Code != http.StatusOK {
			t.Fatalf("list%s: status %d: %s", query, res.Code, res.Body)
		}
		var data struct {
			Documents []docJSON `json:"documents"`
		}
		if err := json.Unmarshal(res.Data, &data); err != nil {
			t.Fatal(err)
		}
		return len(data.Documents)
	}

	if got := count(""); got != 3 {
		t.Errorf("default page: %d documents, want 3", got)
	}
	if got := count("?limit=2"); got != 2 {
		t.Errorf("limit=2: %d documents, want 2", got)
	}
	if got := count("?limit=2&offset=2"); got != 1 {
		t.Errorf("limit=2&offset=2: %d documents, want 1", got)
	}
	if got := count("?offset=10"); got != 0 {
		t.Errorf("offset past the end: %d documents, want 0", got)
	}
}