package parser

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// --- helpers ---

// openFixture opens a file from testdata. A missing REQUIRED fixture fails the
// test (a silent skip would hide a broken setup); optional ones skip.
func openFixture(t *testing.T, name string, optional bool) (*os.File, int64) {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		if optional && errors.Is(err, os.ErrNotExist) {
			t.Skipf("optional fixture %s not found", name)
		}
		t.Fatalf("fixture %s: %v", name, err)
	}
	t.Cleanup(func() { f.Close() })

	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	return f, info.Size()
}

func parserFor(t *testing.T, mimeType string) Parser {
	t.Helper()
	p, ok := ForType(mimeType)
	if !ok {
		t.Fatalf("no parser for %q", mimeType)
	}
	return p
}

func parseBytes(t *testing.T, mimeType string, data []byte) (string, error) {
	t.Helper()
	return parserFor(t, mimeType).Parse(context.Background(), bytes.NewReader(data), int64(len(data)))
}

func zipWith(t *testing.T, name, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func squash(s string) string { return strings.Join(strings.Fields(s), "") }

// --- good files ---

func TestParse_ValidFixtures(t *testing.T) {
	all := []string{"zebrafish", "transparent", "BM25", "café", "Indexing"}
	asciiOnly := []string{"zebrafish", "transparent", "BM25", "Indexing"}

	tests := []struct {
		file       string
		mime       string
		markers    []string
		squash     bool // PDF word spacing is the library's weakest point, so compare without whitespace
		paragraphs bool
	}{
		{"sample.txt", MimeTXT, all, false, false},
		{"sample.docx", MimeDOCX, all, false, true},
		{"sample.pdf", MimePDF, asciiOnly, true, false},
	}

	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) {
			f, size := openFixture(t, tc.file, false)

			text, err := parserFor(t, tc.mime).Parse(context.Background(), f, size)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}

			haystack := text
			if tc.squash {
				haystack = squash(text)
			}
			for _, m := range tc.markers {
				want := m
				if tc.squash {
					want = squash(m)
				}
				if !strings.Contains(haystack, want) {
					t.Errorf("output is missing %q", m)
				}
			}

			if strings.ContainsRune(text, 0) || !utf8.ValidString(text) {
				t.Error("output must be valid UTF-8 without NUL bytes")
			}
			if Clean(text) != text {
				t.Error("output was not cleaned")
			}
			if tc.paragraphs && strings.Count(text, "\n\n") < 3 {
				t.Errorf("expected paragraph breaks (blank lines), got: %q", text)
			}

			preview := text
			if r := []rune(preview); len(r) > 300 {
				preview = string(r[:300])
			}
			t.Logf("%s: %d bytes, preview: %q", tc.file, len(text), preview)
		})
	}
}

func TestParse_UnicodeText(t *testing.T) {
	f, size := openFixture(t, "unicode.txt", false)

	text, err := parserFor(t, MimeTXT).Parse(context.Background(), f, size)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !utf8.ValidString(text) || !strings.ContainsFunc(text, func(r rune) bool { return r >= 0x0980 && r <= 0x09FF }) {
		t.Errorf("Bengali characters were lost or corrupted: %q", text)
	}
}

// --- files that must fail (and never panic) ---

func TestParse_BadFiles(t *testing.T) {
	tests := []struct {
		name     string
		file     string
		mime     string
		want     error
		optional bool
	}{
		{"truncated pdf", "corrupt.pdf", MimePDF, ErrCorrupt, false},
		{"binary junk as txt", "binary.txt", MimeTXT, ErrInvalidText, false},
		{"plain text renamed docx", "not-a-zip.docx", MimeDOCX, ErrCorrupt, false},
		{"zip without word/document.xml", "no-document-xml.docx", MimeDOCX, ErrCorrupt, false},
		{"docx parsed as pdf", "sample.docx", MimePDF, ErrCorrupt, false},
		{"pdf parsed as docx", "sample.pdf", MimeDOCX, ErrCorrupt, false},
		{"docx parsed as txt", "sample.docx", MimeTXT, ErrInvalidText, false},
		{"scanned pdf has no text layer", "scanned.pdf", MimePDF, ErrNoText, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, size := openFixture(t, tc.file, tc.optional)

			text, err := parserFor(t, tc.mime).Parse(context.Background(), f, size)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if text != "" {
				t.Errorf("text should be empty on error, got %q", text)
			}
		})
	}
}

func TestParse_TruncatedFilesNeverPanic(t *testing.T) {
	for _, tc := range []struct{ file, mime string }{
		{"sample.pdf", MimePDF},
		{"sample.docx", MimeDOCX},
	} {
		f, size := openFixture(t, tc.file, false)
		data := make([]byte, size)
		if _, err := f.ReadAt(data, 0); err != nil && err != io.EOF {
			t.Fatal(err)
		}

		for _, frac := range []float64{0.1, 0.5, 0.9} {
			cut := int(float64(len(data)) * frac)
			if _, err := parseBytes(t, tc.mime, data[:cut]); err == nil {
				t.Errorf("%s truncated to %d%% parsed without an error", tc.file, int(frac*100))
			}
		}
	}
}

func TestParse_EdgeCases(t *testing.T) {
	tests := []struct {
		name string
		mime string
		data []byte
		want error // nil means success
		text string
	}{
		{"empty txt", MimeTXT, nil, ErrNoText, ""},
		{"whitespace-only txt", MimeTXT, []byte("  \n\t\r\n "), ErrNoText, ""},
		{"empty docx", MimeDOCX, nil, ErrCorrupt, ""},
		{"empty pdf", MimePDF, nil, ErrCorrupt, ""},
		{"txt with BOM", MimeTXT, []byte("\xEF\xBB\xBFhello"), nil, "hello"},
		{"txt with CRLF", MimeTXT, []byte("one\r\ntwo"), nil, "one\ntwo"},
		{"UTF-16 txt", MimeTXT, []byte{0xFF, 0xFE, 'h', 0, 'i', 0}, ErrInvalidText, ""},
		{"Latin-1 txt", MimeTXT, []byte("caf\xe9"), ErrInvalidText, ""},
		{"txt over the cap", MimeTXT, make([]byte, MaxTextBytes+1), ErrTextTooLarge, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			text, err := parseBytes(t, tc.mime, tc.data)
			if tc.want == nil {
				if err != nil || text != tc.text {
					t.Errorf("got (%q, %v), want (%q, nil)", text, err, tc.text)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// --- DOCX structure and safety ---

func TestDOCX_ExtractsStructure(t *testing.T) {
	const documentXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"
            xmlns:mc="http://schemas.openxmlformats.org/markup-compatibility/2006">
 <w:body>
  <w:p><w:r><w:t>First</w:t></w:r><w:r><w:t xml:space="preserve"> paragraph</w:t></w:r></w:p>
  <w:p><w:r><w:t>Line one</w:t><w:br/><w:t>line two</w:t></w:r></w:p>
  <w:p><w:r><w:t>Col A</w:t><w:tab/><w:t>Col B</w:t></w:r></w:p>
  <w:p><w:del><w:r><w:delText>deleted text</w:delText></w:r></w:del><w:r><w:t>kept</w:t></w:r></w:p>
  <w:p><w:r><w:instrText> PAGE </w:instrText></w:r></w:p>
  <w:p><mc:AlternateContent>
        <mc:Choice><w:r><w:t>boxed</w:t></w:r></mc:Choice>
        <mc:Fallback><w:r><w:t>boxed</w:t></w:r></mc:Fallback>
      </mc:AlternateContent></w:p>
 </w:body>
</w:document>`

	got, err := parseBytes(t, MimeDOCX, zipWith(t, "word/document.xml", documentXML))
	if err != nil {
		t.Fatal(err)
	}

	// Tracked-change deletions, field codes and the duplicate Fallback copy are gone;
	// paragraphs are separated by blank lines, <w:br/> by a single newline.
	const want = "First paragraph\n\nLine one\nline two\n\nCol A Col B\n\nkept\n\nboxed"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestDOCX_MalformedXML(t *testing.T) {
	cases := map[string]string{
		"truncated xml": `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>hi`,
		"not xml":       "this is not xml at all",
		"wrong root":    `<?xml version="1.0"?><root><p>hello</p></root>`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parseBytes(t, MimeDOCX, zipWith(t, "word/document.xml", content))
			if !errors.Is(err, ErrCorrupt) {
				t.Errorf("err = %v, want ErrCorrupt", err)
			}
		})
	}
}

// A valid but empty document is "no text", not "corrupt". This pins the
// difference between the two outcomes.
func TestDOCX_ValidButEmpty(t *testing.T) {
	const emptyDoc = `<?xml version="1.0" encoding="UTF-8"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body/></w:document>`

	_, err := parseBytes(t, MimeDOCX, zipWith(t, "word/document.xml", emptyDoc))
	if !errors.Is(err, ErrNoText) {
		t.Errorf("err = %v, want ErrNoText", err)
	}
}

func TestDOCX_ZipBombIsRejected(t *testing.T) {
	// ~51 MB of 'a' compresses to roughly 50 KB: tiny on disk, huge when inflated.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	chunk := bytes.Repeat([]byte("a"), 1<<20)
	for written := 0; written <= maxDocXMLBytes; written += len(chunk) {
		if _, err := w.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("bomb is %d bytes on disk", buf.Len())

	_, err = parseBytes(t, MimeDOCX, buf.Bytes())
	if !errors.Is(err, ErrTextTooLarge) {
		t.Errorf("err = %v, want ErrTextTooLarge", err)
	}
}

// --- the guard ---

type panicParser struct{}

func (panicParser) Parse(context.Context, io.ReaderAt, int64) (string, error) { panic("boom") }

func TestGuard_RecoversFromPanics(t *testing.T) {
	g := guarded{inner: panicParser{}}

	text, err := g.Parse(context.Background(), bytes.NewReader([]byte("x")), 1)
	if !errors.Is(err, ErrCorrupt) || text != "" {
		t.Errorf("got (%q, %v), want ('', ErrCorrupt)", text, err)
	}
}

func TestGuard_RespectsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	for _, mt := range []string{MimePDF, MimeDOCX, MimeTXT} {
		_, err := parserFor(t, mt).Parse(ctx, bytes.NewReader([]byte("x")), 1)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s: err = %v, want context.Canceled", mt, err)
		}
	}
}

// --- lookup ---

func TestForTypeAndExtension(t *testing.T) {
	tests := []struct {
		in      string
		wantExt string // "" means unsupported
	}{
		{MimePDF, "pdf"},
		{MimeDOCX, "docx"},
		{MimeTXT, "txt"},
		{"text/plain; charset=utf-8", "txt"},
		{"APPLICATION/PDF", "pdf"},
		{"application/msword", ""}, // legacy .doc
		{"image/png", ""},
		{"application/octet-stream", ""},
		{"", ""},
		{"not a mime type", ""},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			p, ok := ForType(tc.in)
			ext, okExt := ExtensionFor(tc.in)

			if (tc.wantExt != "") != ok || ok != okExt {
				t.Fatalf("ForType ok=%v, ExtensionFor ok=%v, want supported=%v", ok, okExt, tc.wantExt != "")
			}
			if ok && (ext != tc.wantExt || p == nil) {
				t.Errorf("ext = %q (parser nil: %v), want %q", ext, p == nil, tc.wantExt)
			}
		})
	}
}

func TestPDF_WordsStayInOneParagraph(t *testing.T) {
	f, size := openFixture(t, "sample.pdf", false)

	text, err := parserFor(t, MimePDF).Parse(context.Background(), f, size)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "small freshwater fish") {
		t.Errorf("words are not separated by single spaces: %q", text)
	}
	// sample.pdf is a single page, so there should be no paragraph breaks at all.
	if n := strings.Count(text, "\n\n"); n > 3 {
		t.Errorf("found %d paragraph breaks in a one-page document: words are being split into paragraphs", n)
	}
}