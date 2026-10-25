package models

import "errors"

// Sentinel errors: higher layers compare against these with errors.Is
// and never need to know anything about Postgres.
var (
	ErrNotFound   = errors.New("not found")
	ErrEmailTaken = errors.New("email already registered")

	// File storage
	ErrFileTooLarge = errors.New("file too large")
	ErrFileNotFound = errors.New("file not found")
	ErrInvalidPath  = errors.New("invalid storage path")
	ErrSummaryInProgress = errors.New("a summary is already being written")
	ErrDocumentTooLong   = errors.New("the document is too long to summarize")
)