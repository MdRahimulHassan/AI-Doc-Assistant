package parser

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSniff(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want string // "" means unsupported
	}{
		{"pdf", []byte("%PDF-1.7 content"), MimePDF},
		{"docx", zipWith(t, "word/document.xml", "<x/>"), MimeDOCX},
		{"xlsx-like zip", zipWith(t, "xl/workbook.xml", "<x/>"), ""},
		{"plain text", []byte("hello world"), MimeTXT},
		{"utf-8 text with BOM", []byte("\xEF\xBB\xBFhello"), MimeTXT},
		{"Bengali text", []byte("আমি বই পড়ি"), MimeTXT},
		{"multi-byte character cut by the window",
			[]byte(strings.Repeat("a", sniffTextBytes-1) + "é" + strings.Repeat("b", 100)), MimeTXT},
		{"invalid UTF-8 after the window is left to the TXT parser",
			[]byte(strings.Repeat("a", 20000) + "\xe9"), MimeTXT},
		{"NUL byte", []byte("abc\x00def"), ""},
		{"UTF-16 text", []byte{0xFF, 0xFE, 'h', 0, 'i', 0}, ""},
		{"Latin-1 text", []byte("caf\xe9"), ""},
		{"PNG image", []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 0x0D}, ""},
		{"only a BOM", []byte("\xEF\xBB\xBF"), ""},
		{"empty", nil, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Sniff(bytes.NewReader(tc.data), int64(len(tc.data)))
			if got != tc.want || ok != (tc.want != "") {
				t.Errorf("Sniff = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.want != "")
			}
		})
	}
}

func TestSniff_Fixtures(t *testing.T) {
	tests := []struct{ file, want string }{
		{"sample.pdf", MimePDF},
		{"sample.docx", MimeDOCX},
		{"sample.txt", MimeTXT},
		{"unicode.txt", MimeTXT},
		{"binary.txt", ""},
		{"no-document-xml.docx", ""},
		{"not-a-zip.docx", MimeTXT}, // plain text: the service rejects it, the .docx name disagrees
		{"corrupt.pdf", MimePDF},    // only the header is checked; ingestion reports the damage later
	}

	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			got, _ := Sniff(bytes.NewReader(data), int64(len(data)))
			if got != tc.want {
				t.Errorf("Sniff(%s) = %q, want %q", tc.file, got, tc.want)
			}
		})
	}
}