package parser

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"unicode/utf8"
)

// TXT reads plain UTF-8 text.
type TXT struct{}

func (TXT) Parse(_ context.Context, r io.ReaderAt, size int64) (string, error) {
	// Reject before reading anything: memory use stays bounded.
	if size > MaxTextBytes {
		return "", ErrTextTooLarge
	}

	data, err := io.ReadAll(io.NewSectionReader(r, 0, size))
	if err != nil {
		return "", fmt.Errorf("%w: txt: read: %v", ErrCorrupt, err)
	}

	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF}) // UTF-8 BOM

	// Older Notepad's "Unicode" option writes UTF-16, which is full of NUL
	// bytes. Say so clearly instead of calling it "binary".
	if len(data) >= 2 && ((data[0] == 0xFF && data[1] == 0xFE) || (data[0] == 0xFE && data[1] == 0xFF)) {
		return "", fmt.Errorf("%w: UTF-16 is not supported, save the file as UTF-8", ErrInvalidText)
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return "", fmt.Errorf("%w: contains NUL bytes (binary file?)", ErrInvalidText)
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("%w: invalid UTF-8 (a Windows-1252 or Latin-1 file?)", ErrInvalidText)
	}
	return string(data), nil
}