package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/google/uuid"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/models"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/parser"
)

const (
	// DefaultIngestTimeout bounds one whole ingestion: read, parse, chunk, store.
	DefaultIngestTimeout = 2 * time.Minute

	// failureWriteTimeout bounds the write that records a failure. It gets its
	// own context because the ingestion's context may already have expired.
	failureWriteTimeout = 10 * time.Second
)

// The ONLY failure text that can reach a client (stored in error_message).
// Short, fixed, and free of technical detail.
const (
	msgNoText      = "No readable text was found in this file (a scanned PDF would need OCR, which is not supported)."
	msgUnreliable  = "The text of this PDF could not be extracted reliably."
	msgTooLarge    = "The text in this file is too large to process."
	msgInvalidText = "This text file is not valid UTF-8. Save it as UTF-8 and upload it again."
	msgCorrupt     = "This file could not be read. It may be damaged, password-protected, or not a valid document."
	msgFileMissing = "The uploaded file is missing. Please upload it again."
	msgUnsupported = "This file type is not supported."
	msgTimeout     = "Processing took too long."
	msgGeneric     = "Processing failed."
)

var (
	errUnsupportedType = errors.New("unsupported file type")
	errPanic           = errors.New("panic during ingestion")
)

// IngestRepository is what ingestion needs from the document repository.
// These are SYSTEM methods: none of them filters by user, because ingestion
// runs in the background with no user. They are deliberately NOT part of any
// interface the HTTP handlers depend on.
type IngestRepository interface {
	GetForIngest(ctx context.Context, id uuid.UUID) (*models.Document, error)
	MarkFailed(ctx context.Context, id uuid.UUID, safeMessage string) error
	CompleteIngestion(ctx context.Context, id uuid.UUID, chunks []string) error
}

// TextChunker splits text into chunks. *chunker.Chunker satisfies it.
type TextChunker interface {
	Chunk(text string) []string
}

type IngestService struct {
	docs    IngestRepository
	files   FileStorage
	chunker TextChunker
	timeout time.Duration

	// parse picks the parser for a content type. A field (not a direct call)
	// so tests can substitute a stub.
	parse func(contentType string) (parser.Parser, bool)
}

func NewIngestService(docs IngestRepository, files FileStorage, chunker TextChunker, timeout time.Duration) *IngestService {
	if timeout <= 0 {
		timeout = DefaultIngestTimeout
	}
	return &IngestService{
		docs:    docs,
		files:   files,
		chunker: chunker,
		timeout: timeout,
		parse:   parser.ForType,
	}
}

// Ingest turns a stored upload into searchable chunks: open the file, parse,
// clean, chunk, then replace the document's chunks and mark it ready in one
// transaction. It is safe to run again on the same document.
//
// Ingest records the outcome itself and has already logged any failure. The
// returned error is for callers that want to react (tests, metrics): do not
// log it again, and never show it to a user.
func (s *IngestService) Ingest(parent context.Context, docID uuid.UUID) error {
	start := time.Now()
	ctx, cancel := context.WithTimeout(parent, s.timeout)
	defer cancel()

	doc, err := s.docs.GetForIngest(ctx, docID)
	switch {
	case errors.Is(err, models.ErrNotFound):
		slog.Info("ingest skipped: document no longer exists", "document_id", docID)
		return nil
	case err != nil:
		slog.Error("ingest failed: could not load document", "document_id", docID, "error", err)
		return fmt.Errorf("ingest %s: load document: %w", docID, err)
	}

	chunks, err := s.process(ctx, doc)
	if err == nil {
		err = s.docs.CompleteIngestion(ctx, doc.ID, chunks)
		if errors.Is(err, models.ErrNotFound) {
			slog.Info("ingest abandoned: document was deleted while processing", "document_id", doc.ID)
			return nil
		}
	}
	if err != nil {
		return s.recordFailure(parent, doc, err)
	}

	// Log IDs and sizes, never the filename: it is user-provided content.
	slog.Info("ingest finished",
		"document_id", doc.ID,
		"mime_type", doc.MimeType,
		"size_bytes", doc.SizeBytes,
		"chunks", len(chunks),
		"took", time.Since(start).Round(time.Millisecond))
	return nil
}

// process does the work that can fail because of the FILE: open, parse, chunk.
// A panic anywhere in it becomes an ordinary error.
func (s *IngestService) process(ctx context.Context, doc *models.Document) (chunks []string, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("ingest panic", "document_id", doc.ID, "panic", rec, "stack", string(debug.Stack()))
			chunks, err = nil, fmt.Errorf("%w: %v", errPanic, rec)
		}
	}()

	p, ok := s.parse(doc.MimeType)
	if !ok {
		return nil, fmt.Errorf("%w: %q", errUnsupportedType, doc.MimeType)
	}

	f, err := s.files.Open(ctx, doc.StoragePath)
	if err != nil {
		return nil, fmt.Errorf("open stored file: %w", err) // models.ErrFileNotFound passes through
	}
	defer f.Close() // Windows cannot delete an open file, so always close

	// The parser also cleans the text (parser.Clean) and caps its size.
	text, err := p.Parse(ctx, f, f.Size())
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", doc.MimeType, err)
	}

	chunks = s.chunker.Chunk(text)
	if len(chunks) == 0 {
		return nil, fmt.Errorf("%w: chunker produced nothing", parser.ErrNoText)
	}
	return chunks, nil
}

// recordFailure logs the full error and stores the safe message. It returns
// the original cause so callers can still inspect it.
func (s *IngestService) recordFailure(parent context.Context, doc *models.Document, cause error) error {
	// Shutdown or cancellation is NOT a failure of the document. Leave it as
	// 'processing' so startup recovery (Step 8) can pick it up and retry.
	if parent.Err() != nil {
		slog.Warn("ingest interrupted; leaving the document as processing so it can be retried",
			"document_id", doc.ID, "reason", parent.Err())
		return fmt.Errorf("ingest %s: interrupted: %w", doc.ID, cause)
	}

	f := classify(cause)
	attrs := []any{
		"document_id", doc.ID,
		"mime_type", doc.MimeType,
		"size_bytes", doc.SizeBytes,
		"stored_message", f.message,
		"error", cause, // the FULL error: server log only
	}
	if f.expected {
		slog.Warn("ingest failed: the file could not be processed", attrs...)
	} else {
		slog.Error("ingest failed", attrs...)
	}

	// A fresh context: the ingestion's own context may already be expired.
	wctx, cancel := context.WithTimeout(parent, failureWriteTimeout)
	defer cancel()

	switch err := s.docs.MarkFailed(wctx, doc.ID, f.message); {
	case err == nil:
	case errors.Is(err, models.ErrNotFound):
		slog.Info("document was deleted before its failure could be recorded", "document_id", doc.ID)
	default:
		slog.Error("could not record the failure; the document stays 'processing'",
			"document_id", doc.ID, "error", err)
	}
	return fmt.Errorf("ingest %s: %w", doc.ID, cause)
}

type failure struct {
	message  string // stored in documents.error_message
	expected bool   // a problem with the FILE, as opposed to with our system
}

// classify maps any error to the fixed message shown to the user. Anything it
// does not recognise becomes the generic message: raw errors never get through.
func classify(err error) failure {
	switch {
	case errors.Is(err, parser.ErrNoText):
		return failure{msgNoText, true}
	case errors.Is(err, parser.ErrUnreliableText):
		return failure{msgUnreliable, true}
	case errors.Is(err, parser.ErrTextTooLarge):
		return failure{msgTooLarge, true}
	case errors.Is(err, parser.ErrInvalidText):
		return failure{msgInvalidText, true}
	case errors.Is(err, parser.ErrCorrupt):
		return failure{msgCorrupt, true}
	case errors.Is(err, errUnsupportedType):
		return failure{msgUnsupported, false}
	case errors.Is(err, models.ErrFileNotFound):
		return failure{msgFileMissing, false}
	case errors.Is(err, context.DeadlineExceeded):
		return failure{msgTimeout, false}
	default:
		return failure{msgGeneric, false}
	}
}