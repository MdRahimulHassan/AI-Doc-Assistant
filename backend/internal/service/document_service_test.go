package service

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/models"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/parser"
)

// --- fakes ---

// callLog records the order in which the fakes are called.
type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *callLog) add(call string) {
	l.mu.Lock()
	l.calls = append(l.calls, call)
	l.mu.Unlock()
}

func (l *callLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

type memFiles struct {
	log       *callLog
	mu        sync.Mutex
	saved     map[string][]byte
	deleted   []string
	saveErr   error
	deleteErr error
}

func (f *memFiles) Save(_ context.Context, userID, docID uuid.UUID, ext string, r io.Reader, maxBytes int64) (string, int64, error) {
	f.log.add("save")
	if f.saveErr != nil {
		return "", 0, f.saveErr
	}
	data, _ := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if int64(len(data)) > maxBytes {
		return "", 0, models.ErrFileTooLarge
	}
	rel := userID.String() + "/" + docID.String() + "." + ext
	f.mu.Lock()
	f.saved[rel] = data
	f.mu.Unlock()
	return rel, int64(len(data)), nil
}

func (f *memFiles) Open(context.Context, string) (StoredFile, error) {
	return nil, errors.New("not used in these tests")
}

func (f *memFiles) Delete(_ context.Context, rel string) error {
	f.log.add("delete-file")
	f.mu.Lock()
	f.deleted = append(f.deleted, rel)
	delete(f.saved, rel)
	f.mu.Unlock()
	return f.deleteErr
}

type fakeDocStore struct {
	log                   *callLog
	docs                  map[uuid.UUID]*models.Document
	createErr             error
	lastLimit, lastOffset int
}

func (s *fakeDocStore) Create(_ context.Context, d *models.Document) (*models.Document, error) {
	s.log.add("create")
	if s.createErr != nil {
		return nil, s.createErr
	}
	cp := *d
	cp.Status = models.StatusProcessing
	cp.CreatedAt = time.Now()
	s.docs[cp.ID] = &cp
	out := cp
	return &out, nil
}

func (s *fakeDocStore) GetByID(_ context.Context, userID, id uuid.UUID) (*models.Document, error) {
	d, ok := s.docs[id]
	if !ok || d.UserID != userID {
		return nil, models.ErrNotFound
	}
	cp := *d
	return &cp, nil
}

func (s *fakeDocStore) List(_ context.Context, userID uuid.UUID, limit, offset int) ([]models.Document, error) {
	s.lastLimit, s.lastOffset = limit, offset
	out := []models.Document{}
	for _, d := range s.docs {
		if d.UserID == userID {
			out = append(out, *d)
		}
	}
	return out, nil
}

func (s *fakeDocStore) Delete(_ context.Context, userID, id uuid.UUID) (string, error) {
	s.log.add("delete-row")
	d, ok := s.docs[id]
	if !ok || d.UserID != userID {
		return "", models.ErrNotFound
	}
	delete(s.docs, id)
	return d.StoragePath, nil
}

type fakeIngester struct {
	log       *callLog
	submitted []uuid.UUID
	err       error
}

func (i *fakeIngester) Submit(id uuid.UUID) error {
	i.log.add("submit")
	i.submitted = append(i.submitted, id)
	return i.err
}

type docHarness struct {
	svc    *DocumentService
	docs   *fakeDocStore
	files  *memFiles
	ingest *fakeIngester
	log    *callLog
	user   uuid.UUID
}

func newDocHarness(maxBytes int64) *docHarness {
	log := &callLog{}
	h := &docHarness{
		docs:   &fakeDocStore{log: log, docs: map[uuid.UUID]*models.Document{}},
		files:  &memFiles{log: log, saved: map[string][]byte{}},
		ingest: &fakeIngester{log: log},
		log:    log,
		user:   uuid.New(),
	}
	h.svc = NewDocumentService(h.docs, h.files, h.ingest, maxBytes)
	return h
}

func zipBytes(t *testing.T, entry string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(entry)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("<x/>")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func upload(h *docHarness, name string, data []byte) (*models.Document, error) {
	return h.svc.Upload(context.Background(), h.user, name, bytes.NewReader(data), int64(len(data)))
}

// --- upload ---

func TestUpload_Success(t *testing.T) {
	h := newDocHarness(1 << 20)
	data := []byte("%PDF-1.7 hello")

	doc, err := upload(h, `C:\Users\me\My Paper.PDF`, data)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	if doc.Filename != "My Paper.PDF" || doc.MimeType != parser.MimePDF || doc.Status != models.StatusProcessing {
		t.Errorf("document = %+v", doc)
	}
	wantPath := h.user.String() + "/" + doc.ID.String() + ".pdf"
	if doc.StoragePath != wantPath || !bytes.Equal(h.files.saved[wantPath], data) {
		t.Errorf("storage path %q, stored bytes %q", doc.StoragePath, h.files.saved[wantPath])
	}
	if doc.SizeBytes != int64(len(data)) {
		t.Errorf("size = %d, want %d", doc.SizeBytes, len(data))
	}
	// The order is the point: save the file, THEN insert the row, THEN queue it.
	if got := h.log.all(); !slices.Equal(got, []string{"save", "create", "submit"}) {
		t.Errorf("call order = %v, want [save create submit]", got)
	}
	if len(h.ingest.submitted) != 1 || h.ingest.submitted[0] != doc.ID {
		t.Errorf("submitted = %v", h.ingest.submitted)
	}
}

func TestUpload_RejectsBeforeTouchingStorage(t *testing.T) {
	pdf := []byte("%PDF-1.7 hello")
	txt := []byte("just some text")
	docx := zipBytes(t, "word/document.xml")
	xlsx := zipBytes(t, "xl/workbook.xml")

	tests := []struct {
		name     string
		data     []byte
		filename string
		size     int64 // 0 means len(data)
		want     error
	}{
		{"empty file", nil, "a.pdf", 0, ErrEmptyFile},
		{"announced size over the limit", pdf, "a.pdf", 2 << 20, models.ErrFileTooLarge},
		{"binary content", []byte{0x89, 'P', 'N', 'G', 0, 0, 0, 1}, "a.pdf", 0, ErrUnsupportedFileType},
		{"pdf content named .docx", pdf, "evil.docx", 0, ErrUnsupportedFileType},
		{"pdf content with no extension", pdf, "paper", 0, ErrUnsupportedFileType},
		{"text content named .pdf", txt, "notes.pdf", 0, ErrUnsupportedFileType},
		{"docx content named .txt", docx, "notes.txt", 0, ErrUnsupportedFileType},
		{"xlsx named .docx", xlsx, "sheet.docx", 0, ErrUnsupportedFileType},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newDocHarness(1 << 20)
			size := int64(len(tc.data))
			if tc.size != 0 {
				size = tc.size
			}

			_, err := h.svc.Upload(context.Background(), h.user, tc.filename, bytes.NewReader(tc.data), size)
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
			if calls := h.log.all(); len(calls) != 0 {
				t.Errorf("storage, database or queue was touched: %v", calls)
			}
		})
	}
}

func TestUpload_FilenameNeverReachesThePath(t *testing.T) {
	h := newDocHarness(1 << 20)

	doc, err := upload(h, "../../etc/passwd.txt", []byte("some text"))
	if err != nil {
		t.Fatal(err)
	}
	if doc.Filename != "passwd.txt" {
		t.Errorf("display name = %q, want passwd.txt", doc.Filename)
	}
	if strings.Contains(doc.StoragePath, "passwd") || strings.Contains(doc.StoragePath, "..") {
		t.Errorf("storage path %q contains client input", doc.StoragePath)
	}
}

func TestUpload_UndoesWhatItDidWhenALaterStepFails(t *testing.T) {
	data := []byte("%PDF-1.7 hello")

	t.Run("insert fails: the file is removed", func(t *testing.T) {
		h := newDocHarness(1 << 20)
		boom := errors.New("insert failed")
		h.docs.createErr = boom

		_, err := upload(h, "a.pdf", data)
		if !errors.Is(err, boom) {
			t.Errorf("err = %v, want the repository error", err)
		}
		if len(h.files.deleted) != 1 || len(h.files.saved) != 0 {
			t.Errorf("deleted %v, still stored %d: the file was not cleaned up", h.files.deleted, len(h.files.saved))
		}
		if len(h.ingest.submitted) != 0 {
			t.Error("a document that was never stored must not be queued")
		}
	})

	t.Run("queue refuses: row and file are removed", func(t *testing.T) {
		h := newDocHarness(1 << 20)
		h.ingest.err = ErrBusy

		_, err := upload(h, "a.pdf", data)
		if !errors.Is(err, ErrBusy) {
			t.Errorf("err = %v, want ErrBusy", err)
		}
		if len(h.docs.docs) != 0 || len(h.files.saved) != 0 {
			t.Errorf("rows left: %d, files left: %d", len(h.docs.docs), len(h.files.saved))
		}
		want := []string{"save", "create", "submit", "delete-row", "delete-file"}
		if got := h.log.all(); !slices.Equal(got, want) {
			t.Errorf("call order = %v, want %v", got, want)
		}
	})

	t.Run("storage fails: nothing else happens", func(t *testing.T) {
		h := newDocHarness(1 << 20)
		h.files.saveErr = errors.New("disk full")

		if _, err := upload(h, "a.pdf", data); err == nil {
			t.Error("expected an error")
		}
		if got := h.log.all(); !slices.Equal(got, []string{"save"}) {
			t.Errorf("call order = %v, want [save]", got)
		}
	})

	t.Run("storage enforces the size limit too", func(t *testing.T) {
		h := newDocHarness(1 << 20)
		h.files.saveErr = models.ErrFileTooLarge

		if _, err := upload(h, "a.pdf", data); !errors.Is(err, models.ErrFileTooLarge) {
			t.Errorf("err = %v, want ErrFileTooLarge", err)
		}
	})
}

// --- delete and read ---

func TestDelete_RowFirstThenFile(t *testing.T) {
	h := newDocHarness(1 << 20)
	doc, err := upload(h, "a.pdf", []byte("%PDF-1.7 hello"))
	if err != nil {
		t.Fatal(err)
	}
	h.log.calls = nil

	if err := h.svc.Delete(context.Background(), h.user, doc.ID); err != nil {
		t.Fatal(err)
	}
	if got := h.log.all(); !slices.Equal(got, []string{"delete-row", "delete-file"}) {
		t.Errorf("call order = %v, want [delete-row delete-file]", got)
	}
	if len(h.files.saved) != 0 {
		t.Error("the file was not removed")
	}
}

func TestDelete_OtherUsersDocumentIsNotFoundAndFileSurvives(t *testing.T) {
	h := newDocHarness(1 << 20)
	doc, err := upload(h, "a.pdf", []byte("%PDF-1.7 hello"))
	if err != nil {
		t.Fatal(err)
	}

	err = h.svc.Delete(context.Background(), uuid.New(), doc.ID) // a different user
	if !errors.Is(err, models.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if len(h.files.saved) != 1 {
		t.Error("another user's delete attempt removed the file")
	}
}

func TestDelete_FileRemovalFailureStillSucceeds(t *testing.T) {
	h := newDocHarness(1 << 20)
	doc, err := upload(h, "a.pdf", []byte("%PDF-1.7 hello"))
	if err != nil {
		t.Fatal(err)
	}
	h.files.deleteErr = errors.New("file is locked")

	if err := h.svc.Delete(context.Background(), h.user, doc.ID); err != nil {
		t.Errorf("err = %v: an orphan file must not turn a delete into an error", err)
	}
	if len(h.docs.docs) != 0 {
		t.Error("the row should be gone")
	}
}

func TestGetAndList_AreScopedToTheOwner(t *testing.T) {
	h := newDocHarness(1 << 20)
	doc, err := upload(h, "a.pdf", []byte("%PDF-1.7 hello"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if got, err := h.svc.Get(ctx, h.user, doc.ID); err != nil || got.ID != doc.ID {
		t.Errorf("owner Get = (%v, %v)", got, err)
	}
	if _, err := h.svc.Get(ctx, uuid.New(), doc.ID); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("other user's Get err = %v, want ErrNotFound", err)
	}

	list, err := h.svc.List(ctx, uuid.New(), 5, 10)
	if err != nil || len(list) != 0 {
		t.Errorf("other user's List = (%v, %v), want empty", list, err)
	}
	if h.docs.lastLimit != 5 || h.docs.lastOffset != 10 {
		t.Errorf("paging was not passed through: %d/%d", h.docs.lastLimit, h.docs.lastOffset)
	}
}

// --- filenames ---

func TestSanitizeFilename(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"plain", "paper.pdf", "paper.pdf"},
		{"spaces are kept", "My Paper (final).pdf", "My Paper (final).pdf"},
		{"windows path", `C:\Users\me\paper.pdf`, "paper.pdf"},
		{"unix path", "/home/me/paper.pdf", "paper.pdf"},
		{"traversal", "../../etc/passwd", "passwd"},
		{"mixed separators", `..\..\a/b\c.txt`, "c.txt"},
		{"empty", "", "document.pdf"},
		{"dot", ".", "document.pdf"},
		{"dot dot", "..", "document.pdf"},
		{"slash", "/", "document.pdf"},
		{"only spaces", "   ", "document.pdf"},
		{"control characters", "line\nbreak\x00.pdf", "linebreak.pdf"},
		{"bidi override", "evil\u202Efdp.exe", "evilfdp.exe"},
		{"invalid UTF-8", "bad\xffname.pdf", "badname.pdf"},
		{"Bengali", "বই.pdf", "বই.pdf"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizeFilename(tc.in, "pdf"); got != tc.want {
				t.Errorf("SanitizeFilename(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeFilename_LongNames(t *testing.T) {
	for name, in := range map[string]string{
		"ascii":   strings.Repeat("a", 300) + ".pdf",
		"Bengali": strings.Repeat("বই", 200) + ".pdf",
	} {
		t.Run(name, func(t *testing.T) {
			got := SanitizeFilename(in, "pdf")
			if len(got) > maxFilenameBytes {
				t.Errorf("length %d, want at most %d", len(got), maxFilenameBytes)
			}
			if !utf8.ValidString(got) || strings.ContainsRune(got, utf8.RuneError) {
				t.Error("a character was cut in half")
			}
			if !strings.HasSuffix(got, ".pdf") {
				t.Errorf("extension lost: ...%q", got[max(0, len(got)-10):])
			}
		})
	}
}

func TestExtension(t *testing.T) {
	tests := map[string]string{
		"paper.pdf":          "pdf",
		"PAPER.PDF":          "pdf",
		`C:\dir\paper.docx`:  "docx",
		"archive.tar.gz":     "gz",
		"no-extension":       "",
		"trailing-dot.":      "",
		".hidden":            "hidden",
		"  spaced.txt  ":     "txt",
		"":                   "",
	}
	for in, want := range tests {
		if got := extension(in); got != want {
			t.Errorf("extension(%q) = %q, want %q", in, got, want)
		}
	}
}