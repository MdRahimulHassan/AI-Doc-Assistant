package parser

import (
	"archive/zip"
	"bytes"
	"io"
	"unicode/utf8"
)

// How much of a file is inspected to decide whether it is text. The TXT parser
// validates the whole file later; this only has to catch binaries cheaply.
const sniffTextBytes = 8 << 10

// Sniff decides a file's type from its CONTENT. The client's Content-Type and
// filename are user-controlled and are never consulted. It returns one of the
// Mime* constants, or ok=false if the content is not a supported type.
//
//	PDF   starts with "%PDF-"
//	DOCX  starts with the ZIP signature AND contains word/document.xml
//	      (XLSX and PPTX are ZIPs too, so the signature alone proves nothing)
//	TXT   has no signature: valid UTF-8 with no NUL bytes
func Sniff(r io.ReaderAt, size int64) (mimeType string, ok bool) {
	if size <= 0 {
		return "", false
	}

	head := make([]byte, min(size, sniffTextBytes))
	n, err := r.ReadAt(head, 0)
	if err != nil && err != io.EOF {
		return "", false
	}
	head = head[:n]

	switch {
	case bytes.HasPrefix(head, []byte("%PDF-")):
		return MimePDF, true
	case bytes.HasPrefix(head, []byte("PK\x03\x04")):
		if hasWordDocument(r, size) {
			return MimeDOCX, true
		}
		return "", false
	case looksLikeText(head, size > int64(len(head))):
		return MimeTXT, true
	}
	return "", false
}

// hasWordDocument reports whether a ZIP archive contains word/document.xml.
// It reads only the archive's central directory, not the contents.
func hasWordDocument(r io.ReaderAt, size int64) bool {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return false
	}
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			return true
		}
	}
	return false
}

// looksLikeText reports whether head is UTF-8 text without NUL bytes.
// truncated says head is only the start of a larger file.
func looksLikeText(head []byte, truncated bool) bool {
	head = bytes.TrimPrefix(head, []byte{0xEF, 0xBB, 0xBF}) // UTF-8 BOM
	if len(head) == 0 || bytes.IndexByte(head, 0) >= 0 {
		return false // empty, or binary (UTF-16 text is full of NULs too)
	}
	if truncated {
		// The window may end in the middle of a multi-byte character. Drop at
		// most 3 bytes of incomplete tail before judging validity.
		for i := 0; i < utf8.UTFMax-1 && !utf8.Valid(head); i++ {
			head = head[:len(head)-1]
		}
	}
	return utf8.Valid(head)
}