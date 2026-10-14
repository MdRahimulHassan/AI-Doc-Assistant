package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/models"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/parser"
)

// --- fakes ---

type fakeIngestRepo struct {
	mu  sync.Mutex
	doc *models.Document // nil means "not found"

	getErr, completeErr, markErr error

	completed  [][]string
	failedMsgs []string
	markCtxErr error // ctx.Err() as seen inside MarkFailed
}

func (r *fakeIngestRepo) GetForIngest(_ context.Context, id uuid.UUID) (*models.Document, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.getErr != nil {
		return nil, r.getErr
	}
	if r.doc == nil || r.doc.ID != id {
		return nil, models.ErrNotFound
	}
	cp := *r.doc
	return &cp, nil
}

func (r *fakeIngestRepo) CompleteIngestion(_ context.Context, _ uuid.UUID, chunks []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.completed = append(r.completed, chunks)
	return r.completeErr
}

func (r *fakeIngestRepo) MarkFailed(ctx context.Context, _ uuid.UUID, msg string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failedMsgs = append(r.failedMsgs, msg)
	r.markCtxErr = ctx.Err()
	return r.markErr
}

type fakeFiles struct {
	mu             sync.Mutex
	files          map[string][]byte
	openErr        error
	opened, closed int
}

func (f *fakeFiles) Save(context.Context, uuid.UUID, uuid.UUID, string, io.Reader, int64) (string, int64, error) {
	return "", 0, errors.New("not used in these tests")
}

func (f *fakeFiles) Open(_ context.Context, rel string) (StoredFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.openErr != nil {
		return nil, f.openErr
	}
	data, ok := f.files[rel]
	if !ok {
		return nil, models.ErrFileNotFound
	}
	f.opened++
	return &trackedFile{Reader: bytes.NewReader(data), onClose: func() {
		f.mu.Lock()
		f.closed++
		f.mu.Unlock()
	}}, nil
}

func (f *fakeFiles) Delete(context.Context, string) error { return nil }

// trackedFile is a StoredFile that counts Close calls.
type trackedFile struct {
	*bytes.Reader
	onClose func()
}

func (t *trackedFile) Close() error { t.onClose(); return nil }

type stubParser struct {
	text  string
	err   error
	block bool // wait until the context ends (simulates a slow parse)
}

func (p *stubParser) Parse(ctx context.Context, _ io.ReaderAt, _ int64) (string, error) {
	if p.block {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return p.text, p.err
}

type stubChunker struct {
	out  []string
	boom bool // panic, to prove panics are contained
}

func (c *stubChunker) Chunk(string) []string {
	if c.boom {
		panic("chunker bug")
	}
	return c.out
}

type ingestHarness struct {
	svc   *IngestService
	repo  *fakeIngestRepo
	files *fakeFiles
	fp    *stubParser
	fc    *stubChunker
	doc   *models.Document
}

func newIngestHarness(t *testing.T) *ingestHarness {
	t.Helper()

	doc := &models.Document{
		ID:          uuid.New(),
		UserID:      uuid.New(),
		Filename:    "paper.pdf",
		MimeType:    parser.MimePDF,
		SizeBytes:   100,
		StoragePath: "user/doc.pdf",
		Status:      models.StatusProcessing,
	}
	h := &ingestHarness{
		repo:  &fakeIngestRepo{doc: doc},
		files: &fakeFiles{files: map[string][]byte{doc.StoragePath: []byte("%PDF-fake")}},
		fp:    &stubParser{text: "some extracted text"},
		fc:    &stubChunker{out: []string{"chunk one", "chunk two", "chunk three"}},
		doc:   doc,
	}
	h.svc = NewIngestService(h.repo, h.files, h.fc, time.Minute)
	h.svc.parse = func(string) (parser.Parser, bool) { return h.fp, true }
	return h
}

// --- tests ---

func TestIngest_Success(t *testing.T) {
	h := newIngestHarness(t)

	if err := h.svc.Ingest(context.Background(), h.doc.ID); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(h.repo.completed) != 1 || !slices.Equal(h.repo.completed[0], h.fc.out) {
		t.Errorf("CompleteIngestion calls = %v, want exactly one with %v", h.repo.completed, h.fc.out)
	}
	if len(h.repo.failedMsgs) != 0 {
		t.Errorf("unexpected failure recorded: %v", h.repo.failedMsgs)
	}
	if h.files.opened != 1 || h.files.closed != 1 {
		t.Errorf("file opened %d times and closed %d times, want 1 and 1", h.files.opened, h.files.closed)
	}
}

func TestIngest_ParserErrorsBecomeSafeMessages(t *testing.T) {
	leaky := errors.New("dial tcp 10.0.0.5:5432: connection refused")

	tests := []struct {
		name     string
		parseErr error
		root     error // what the returned error must wrap
		want     string
	}{
		{"no text", fmt.Errorf("%w", parser.ErrNoText), parser.ErrNoText, msgNoText},
		{"unreliable text", fmt.Errorf("%w: pdf: no spaces", parser.ErrUnreliableText), parser.ErrUnreliableText, msgUnreliable},
		{"too large", parser.ErrTextTooLarge, parser.ErrTextTooLarge, msgTooLarge},
		{"invalid text", fmt.Errorf("%w: NUL bytes", parser.ErrInvalidText), parser.ErrInvalidText, msgInvalidText},
		{"corrupt (wrapped like the real parsers do)",
			fmt.Errorf("%w: pdftotext: exit status 1: Syntax Error at /tmp/x.pdf", parser.ErrCorrupt), parser.ErrCorrupt, msgCorrupt},
		{"unknown error never leaks", leaky, leaky, msgGeneric},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newIngestHarness(t)
			h.fp.err = tc.parseErr

			err := h.svc.Ingest(context.Background(), h.doc.ID)
			if !errors.Is(err, tc.root) {
				t.Errorf("returned error %v does not wrap %v", err, tc.root)
			}
			if len(h.repo.completed) != 0 {
				t.Error("chunks were stored for a failed document")
			}
			if len(h.repo.failedMsgs) != 1 || h.repo.failedMsgs[0] != tc.want {
				t.Errorf("recorded messages = %q, want exactly [%q]", h.repo.failedMsgs, tc.want)
			}
			if h.files.closed != h.files.opened {
				t.Error("the file was left open")
			}
		})
	}
}

func TestIngest_FileProblems(t *testing.T) {
	t.Run("stored file is missing", func(t *testing.T) {
		h := newIngestHarness(t)
		delete(h.files.files, h.doc.StoragePath)
		_ = h.svc.Ingest(context.Background(), h.doc.ID)
		assertOneFailure(t, h, msgFileMissing)
	})

	t.Run("storage fails for another reason", func(t *testing.T) {
		h := newIngestHarness(t)
		h.files.openErr = errors.New("disk on fire")
		_ = h.svc.Ingest(context.Background(), h.doc.ID)
		assertOneFailure(t, h, msgGeneric)
	})

	t.Run("unsupported file type", func(t *testing.T) {
		h := newIngestHarness(t)
		h.svc.parse = func(string) (parser.Parser, bool) { return nil, false }
		_ = h.svc.Ingest(context.Background(), h.doc.ID)
		assertOneFailure(t, h, msgUnsupported)
	})

	t.Run("chunker produces nothing", func(t *testing.T) {
		h := newIngestHarness(t)
		h.fc.out = nil
		_ = h.svc.Ingest(context.Background(), h.doc.ID)
		assertOneFailure(t, h, msgNoText)
	})

	t.Run("chunker panics", func(t *testing.T) {
		h := newIngestHarness(t)
		h.fc.boom = true
		if err := h.svc.Ingest(context.Background(), h.doc.ID); err == nil {
			t.Error("expected an error")
		}
		assertOneFailure(t, h, msgGeneric)
		if h.files.closed != h.files.opened {
			t.Error("the file was left open after a panic")
		}
	})
}

func assertOneFailure(t *testing.T, h *ingestHarness, want string) {
	t.Helper()
	if len(h.repo.completed) != 0 {
		t.Error("chunks were stored for a failed document")
	}
	if len(h.repo.failedMsgs) != 1 || h.repo.failedMsgs[0] != want {
		t.Errorf("recorded messages = %q, want exactly [%q]", h.repo.failedMsgs, want)
	}
}

func TestIngest_DocumentGoneIsAQuietNoOp(t *testing.T) {
	h := newIngestHarness(t)
	h.repo.doc = nil

	if err := h.svc.Ingest(context.Background(), h.doc.ID); err != nil {
		t.Errorf("err = %v, want nil", err)
	}
	if h.files.opened != 0 || len(h.repo.completed) != 0 || len(h.repo.failedMsgs) != 0 {
		t.Error("nothing should happen for a document that no longer exists")
	}
}

func TestIngest_LoadErrorRecordsNothing(t *testing.T) {
	h := newIngestHarness(t)
	h.repo.getErr = errors.New("database is down")

	if err := h.svc.Ingest(context.Background(), h.doc.ID); err == nil {
		t.Error("expected an error")
	}
	if len(h.repo.failedMsgs) != 0 {
		t.Error("a document that could not even be loaded must not be marked failed")
	}
}

func TestIngest_DeletedWhileProcessing(t *testing.T) {
	h := newIngestHarness(t)
	h.repo.completeErr = fmt.Errorf("update document: %w", models.ErrNotFound)

	if err := h.svc.Ingest(context.Background(), h.doc.ID); err != nil {
		t.Errorf("err = %v, want nil", err)
	}
	if len(h.repo.failedMsgs) != 0 {
		t.Errorf("a deleted document must not be marked failed, got %v", h.repo.failedMsgs)
	}
}

func TestIngest_StoreFailureIsRecorded(t *testing.T) {
	h := newIngestHarness(t)
	h.repo.completeErr = errors.New("deadlock detected")

	if err := h.svc.Ingest(context.Background(), h.doc.ID); err == nil {
		t.Error("expected an error")
	}
	assertOneFailure2(t, h, msgGeneric)
}

// like assertOneFailure, but CompleteIngestion was (unsuccessfully) called once
func assertOneFailure2(t *testing.T, h *ingestHarness, want string) {
	t.Helper()
	if len(h.repo.failedMsgs) != 1 || h.repo.failedMsgs[0] != want {
		t.Errorf("recorded messages = %q, want exactly [%q]", h.repo.failedMsgs, want)
	}
}

func TestIngest_TimeoutIsRecordedWithAFreshContext(t *testing.T) {
	h := newIngestHarness(t)
	h.fp.block = true
	h.svc.timeout = 30 * time.Millisecond

	err := h.svc.Ingest(context.Background(), h.doc.ID)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want a deadline error", err)
	}
	assertOneFailure(t, h, msgTimeout)
	if h.repo.markCtxErr != nil {
		t.Errorf("the failure was written with an expired context: %v", h.repo.markCtxErr)
	}
}

func TestIngest_ShutdownLeavesDocumentProcessing(t *testing.T) {
	h := newIngestHarness(t)
	h.fp.block = true

	parent, cancel := context.WithCancel(context.Background())
	time.AfterFunc(30*time.Millisecond, cancel)

	err := h.svc.Ingest(parent, h.doc.ID)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if len(h.repo.failedMsgs) != 0 {
		t.Errorf("an interrupted document must stay 'processing' for retry, but was marked failed: %v", h.repo.failedMsgs)
	}
}

func TestIngest_FailureToRecordStillReturnsTheCause(t *testing.T) {
	h := newIngestHarness(t)
	h.fp.err = parser.ErrNoText
	h.repo.markErr = errors.New("database is down")

	err := h.svc.Ingest(context.Background(), h.doc.ID)
	if !errors.Is(err, parser.ErrNoText) {
		t.Errorf("err = %v, want the original cause (ErrNoText)", err)
	}
}

func TestMessagesAreShortAndSafe(t *testing.T) {
	for _, msg := range []string{msgNoText, msgUnreliable, msgTooLarge, msgInvalidText,
		msgCorrupt, msgFileMissing, msgUnsupported, msgTimeout, msgGeneric} {
		if len(msg) == 0 || len(msg) > 160 {
			t.Errorf("message length %d out of range: %q", len(msg), msg)
		}
		if strings.ContainsAny(msg, "\n\r") || !strings.HasSuffix(msg, ".") {
			t.Errorf("message should be one sentence ending in a period: %q", msg)
		}
	}
}