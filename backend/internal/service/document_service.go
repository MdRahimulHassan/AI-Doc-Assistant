package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/models"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/parser"
)

const maxFilenameBytes = 255

// UploadFile is what Upload needs from the uploaded file. multipart.File
// satisfies it (it can be read, read at an offset, and rewound).
type UploadFile interface {
	io.Reader
	io.ReaderAt
	io.Seeker
}

// DocumentStore is the USER-FACING part of the document repository. Every
// method takes the owner's ID. The system methods used by ingestion
// (GetForIngest, MarkFailed, CompleteIngestion) are deliberately absent.
type DocumentStore interface {
	Create(ctx context.Context, doc *models.Document) (*models.Document, error)
	GetByID(ctx context.Context, userID, id uuid.UUID) (*models.Document, error)
	List(ctx context.Context, userID uuid.UUID, limit, offset int) ([]models.Document, error)
	Delete(ctx context.Context, userID, id uuid.UUID) (storagePath string, err error)
}

// Ingester takes a stored document and processes it.
type Ingester interface {
	// Submit hands a document over for processing. It returns ErrBusy when it
	// cannot accept more work right now. A document that later FAILS to process
	// is not a Submit error: that outcome is recorded on the document itself.
	Submit(docID uuid.UUID) error
}

type DocumentService struct {
	docs     DocumentStore
	files    FileStorage
	ingest   Ingester
	maxBytes int64
}

func NewDocumentService(docs DocumentStore, files FileStorage, ingest Ingester, maxUploadBytes int64) *DocumentService {
	return &DocumentService{docs: docs, files: files, ingest: ingest, maxBytes: maxUploadBytes}
}

// Upload validates, stores and queues one document, in this order:
//
//  1. size, then content sniffing (the bytes decide the type, never the client)
//  2. the filename's extension must agree with the sniffed type
//  3. save the file (the path is generated from IDs, never from the filename)
//  4. insert the row
//  5. hand the document to ingestion
//
// If a later step fails, the earlier ones are undone: a failed insert deletes
// the file, and a refused hand-off deletes both. Nothing is left half-done.
func (s *DocumentService) Upload(ctx context.Context, userID uuid.UUID, clientName string, f UploadFile, size int64) (*models.Document, error) {
	if userID == uuid.Nil {
		return nil, errors.New("upload: user id is required")
	}
	switch {
	case size <= 0:
		return nil, ErrEmptyFile
	case size > s.maxBytes:
		return nil, models.ErrFileTooLarge
	}

	mimeType, ok := parser.Sniff(f, size)
	if !ok {
		return nil, ErrUnsupportedFileType
	}
	ext, _ := parser.ExtensionFor(mimeType)

	// The sniffed type must agree with the claimed name. These details are for
	// logs only: the handler never sends err.Error() to the client.
	if claimed := extension(clientName); claimed != ext {
		return nil, fmt.Errorf("%w: the content is %q but the filename ends in %q", ErrUnsupportedFileType, ext, claimed)
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("upload: rewind: %w", err)
	}

	docID := uuid.New()
	rel, written, err := s.files.Save(ctx, userID, docID, ext, f, s.maxBytes)
	if err != nil {
		return nil, fmt.Errorf("upload: save file: %w", err) // models.ErrFileTooLarge stays detectable
	}

	doc, err := s.docs.Create(ctx, &models.Document{
		ID:          docID,
		UserID:      userID,
		Filename:    SanitizeFilename(clientName, ext),
		MimeType:    mimeType, // the sniffed type, not the client's claim
		SizeBytes:   written,
		StoragePath: rel,
	})
	if err != nil {
		s.discardFile(ctx, rel) // no row will ever point at this file
		return nil, fmt.Errorf("upload: create document: %w", err)
	}

	if err := s.ingest.Submit(doc.ID); err != nil {
		s.rollback(ctx, userID, doc)
		return nil, fmt.Errorf("upload: submit for processing: %w", err)
	}
	return doc, nil
}

func (s *DocumentService) Get(ctx context.Context, userID, id uuid.UUID) (*models.Document, error) {
	return s.docs.GetByID(ctx, userID, id)
}

func (s *DocumentService) List(ctx context.Context, userID uuid.UUID, limit, offset int) ([]models.Document, error) {
	return s.docs.List(ctx, userID, limit, offset)
}

// Delete removes the row first (its chunks go with it), then the file. If the
// file cannot be removed it is only logged: an orphan file is harmless, while a
// row pointing at a missing file would be a bug.
func (s *DocumentService) Delete(ctx context.Context, userID, id uuid.UUID) error {
	rel, err := s.docs.Delete(ctx, userID, id)
	if err != nil {
		return fmt.Errorf("delete document: %w", err) // models.ErrNotFound stays detectable
	}
	s.discardFile(ctx, rel)
	return nil
}

// rollback undoes a stored upload. If the row cannot be removed, the file stays
// so that the row does not end up pointing at nothing.
func (s *DocumentService) rollback(ctx context.Context, userID uuid.UUID, doc *models.Document) {
	ctx = context.WithoutCancel(ctx) // cleanup must finish even if the client has gone away
	rel, err := s.docs.Delete(ctx, userID, doc.ID)
	if err != nil {
		slog.Error("upload rollback: could not remove the document row; leaving row and file in place",
			"document_id", doc.ID, "error", err)
		return
	}
	s.discardFile(ctx, rel)
}

func (s *DocumentService) discardFile(ctx context.Context, rel string) {
	if err := s.files.Delete(context.WithoutCancel(ctx), rel); err != nil {
		slog.Error("could not remove a stored file (it is now an orphan)", "path", rel, "error", err)
	}
}

// extension returns the lower-case extension of a client-supplied filename,
// without the dot ("" if none). Both / and \ count as separators.
func extension(name string) string {
	name = strings.ReplaceAll(strings.TrimSpace(name), `\`, "/")
	return strings.ToLower(strings.TrimPrefix(path.Ext(path.Base(name)), "."))
}

// SanitizeFilename turns a client-supplied filename into a safe DISPLAY name.
// The name is never used as a path (stored files are named by IDs), but it ends
// up in the database, JSON and logs, so it still has to be tame:
//   - any directory part is dropped, whether it uses / or \
//   - invalid UTF-8 is removed (Postgres rejects it), as are control characters
//     (NUL breaks Postgres, newlines break logs) and bidi overrides (used to
//     fake extensions: "evil\u202Efdp.exe" displays as "evilexe.pdf")
//   - the result is capped at 255 bytes without cutting a character or the extension
func SanitizeFilename(name, ext string) string {
	name = strings.ToValidUTF8(name, "")
	name = path.Base(strings.ReplaceAll(name, `\`, "/"))
	name = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsControl(r),
			r >= 0x202A && r <= 0x202E, // bidi embeddings and overrides
			r >= 0x2066 && r <= 0x2069: // bidi isolates
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)

	switch name {
	case "", ".", "..", "/":
		return "document." + ext
	}

	if len(name) > maxFilenameBytes {
		suffix := path.Ext(name)
		if len(suffix) > 16 {
			suffix = ""
		}
		stem := name[:len(name)-len(suffix)]
		cut := maxFilenameBytes - len(suffix)
		for cut > 0 && !utf8.RuneStart(stem[cut]) {
			cut--
		}
		name = stem[:cut] + suffix
	}
	return name
}