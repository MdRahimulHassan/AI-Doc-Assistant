package parser

import "errors"

// Callers (the ingest service) match these with errors.Is and show the user
// a short fixed message. The wrapped detail is for server logs only.
var (
	ErrNoText       = errors.New("no extractable text")
	ErrTextTooLarge = errors.New("document text is too large")
	ErrCorrupt      = errors.New("file is corrupt or not a valid document")
	ErrInvalidText  = errors.New("file is not valid UTF-8 text")
)