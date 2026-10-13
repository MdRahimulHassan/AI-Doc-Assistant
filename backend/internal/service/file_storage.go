package service

import (
	"context"
	"io"

	"github.com/google/uuid"
)

// StoredFile is an open file. Parsers need ReaderAt plus the size, because
// zip.NewReader (DOCX) and most PDF readers take an io.ReaderAt and a length.
type StoredFile interface {
	io.Reader
	io.ReaderAt
	io.Closer
	Size() int64
}

// FileStorage is everything the document service needs from file storage.
type FileStorage interface {
	// Save stores r as <user_id>/<doc_id>.<ext>, reading at most maxBytes.
	// It returns the relative path to store in the database and the number of
	// bytes written. ext must come from the validated file type, never from
	// the uploaded filename. Exceeding maxBytes returns models.ErrFileTooLarge.
	Save(ctx context.Context, userID, docID uuid.UUID, ext string, r io.Reader, maxBytes int64) (relPath string, size int64, err error)

	// Open returns models.ErrFileNotFound if the file doesn't exist.
	Open(ctx context.Context, relPath string) (StoredFile, error)

	// Delete is idempotent: deleting a file that is already gone is not an error.
	Delete(ctx context.Context, relPath string) error
}