package parser

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"runtime/debug"
)

const (
	MimePDF  = "application/pdf"
	MimeDOCX = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	MimeTXT  = "text/plain"

	// MaxTextBytes caps the text extracted from ONE document, so a single
	// upload cannot turn into millions of chunks.
	MaxTextBytes = 5 << 20
)

// Parser extracts raw text from a file. ReaderAt plus size is what both
// archive/zip (DOCX) and the PDF library accept.
type Parser interface {
	Parse(ctx context.Context, r io.ReaderAt, size int64) (string, error)
}

type typeInfo struct {
	ext    string
	parser Parser
}

// The single source of truth for supported types.
var types = map[string]typeInfo{
	MimePDF:  {ext: "pdf", parser: guarded{inner: PDF{}}},
	MimeDOCX: {ext: "docx", parser: guarded{inner: DOCX{}}},
	MimeTXT:  {ext: "txt", parser: guarded{inner: TXT{}}},
}

func lookup(contentType string) (typeInfo, bool) {
	// Tolerates "text/plain; charset=utf-8" and different letter case.
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return typeInfo{}, false
	}
	ti, ok := types[mt]
	return ti, ok
}

// ForType returns the parser for a content type. The result is always the
// guarded version.
func ForType(contentType string) (Parser, bool) {
	ti, ok := lookup(contentType)
	return ti.parser, ok
}

// ExtensionFor returns the file extension for a supported type (Step 7 uses
// it to name stored files; the extension never comes from the upload).
func ExtensionFor(contentType string) (string, bool) {
	ti, ok := lookup(contentType)
	return ti.ext, ok
}

// guarded wraps a parser with the rules every parser must obey.
type guarded struct{ inner Parser }

func (g guarded) Parse(ctx context.Context, r io.ReaderAt, size int64) (text string, err error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if size < 0 {
		return "", fmt.Errorf("%w: negative size", ErrCorrupt)
	}

	// Rule 4: third-party parsing code panics on malformed input.
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("parser panic", "panic", rec, "stack", string(debug.Stack()))
			text, err = "", fmt.Errorf("%w: parser panic: %v", ErrCorrupt, rec)
		}
	}()

	raw, err := g.inner.Parse(ctx, r, size)
	if err != nil {
		return "", err
	}

	// Rule 2: backstop for the size cap (parsers also stop early).
	if len(raw) > MaxTextBytes {
		return "", ErrTextTooLarge
	}

	// Rule 1: one shared cleaning step.
	cleaned := Clean(raw)

	// Rule 3: nothing usable is a failure with a clear reason.
	if cleaned == "" {
		return "", ErrNoText
	}
	return cleaned, nil
}