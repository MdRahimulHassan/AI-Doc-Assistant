package parser

import (
	"archive/zip"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	// Limit on the DECOMPRESSED size of word/document.xml (XML is much bulkier
	// than the text inside it). A ZIP bomb is a few KB that inflates to gigabytes.
	maxDocXMLBytes = 50 << 20

	wordNS       = "http://schemas.openxmlformats.org/wordprocessingml/2006/main"
	wordNSStrict = "http://purl.oclc.org/ooxml/wordprocessingml/main"
	mcNS         = "http://schemas.openxmlformats.org/markup-compatibility/2006"
)

// DOCX extracts text from a .docx, which is a ZIP archive containing
// word/document.xml. Legacy .doc files are not supported.
type DOCX struct{}

func (DOCX) Parse(ctx context.Context, r io.ReaderAt, size int64) (string, error) {
	// Legacy .doc is an OLE2 compound file, not a ZIP. Give a clear reason.
	var magic [4]byte
	if n, _ := r.ReadAt(magic[:], 0); n == 4 && magic == [4]byte{0xD0, 0xCF, 0x11, 0xE0} {
		return "", fmt.Errorf("%w: legacy .doc format is not supported", ErrCorrupt)
	}

	zr, err := zip.NewReader(r, size)
	if err != nil {
		return "", fmt.Errorf("%w: docx: not a valid zip: %v", ErrCorrupt, err)
	}

	var doc *zip.File
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			doc = f
			break
		}
	}
	if doc == nil {
		// Also catches XLSX/PPTX and other ZIPs that merely look like DOCX.
		return "", fmt.Errorf("%w: docx: word/document.xml not found", ErrCorrupt)
	}
	// Fast rejection on the DECLARED size. Declared sizes can lie, which is why
	// the reader below also enforces the limit while decompressing.
	if doc.UncompressedSize64 > maxDocXMLBytes {
		return "", fmt.Errorf("%w: docx: document.xml is too large", ErrTextTooLarge)
	}

	rc, err := doc.Open()
	if err != nil {
		return "", fmt.Errorf("%w: docx: open document.xml: %v", ErrCorrupt, err)
	}
	defer rc.Close()

	lr := &limitedReader{r: rc, remaining: maxDocXMLBytes + 1}
	text, err := extractWordText(ctx, lr)

	switch {
	case lr.exceeded:
		return "", fmt.Errorf("%w: docx: decompressed content too large", ErrTextTooLarge)
	case errors.Is(err, ErrTextTooLarge),
		errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded):
		return "", err
	case err != nil:
		return "", fmt.Errorf("%w: docx: %v", ErrCorrupt, err)
	}
	return text, nil
}

// extractWordText streams through the XML (no whole-document tree in memory)
// and collects text:
//
//	w:t       -> its text
//	w:tab     -> a tab
//	w:br/w:cr -> a line break
//	end of w:p -> a paragraph break (blank line)
//
// Not collected: w:delText (deleted tracked changes), w:instrText (field
// codes like PAGE), and mc:Fallback (a duplicate copy of text boxes).
func extractWordText(ctx context.Context, r io.Reader) (string, error) {
	dec := xml.NewDecoder(r)

	var sb strings.Builder
	inText := false
	sawRoot := false

	for n := 0; ; n++ {
		if n%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return "", err
			}
		}

		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}

		switch t := tok.(type) {
		case xml.StartElement:
			// The first element must be <w:document>. Go's decoder accepts any
			// XML (even bare text), so a genuine DOCX has to be verified here.
			if !sawRoot {
				if !isWordNS(t.Name.Space) || t.Name.Local != "document" {
					return "", fmt.Errorf("root element is <%s>, expected w:document", t.Name.Local)
				}
				sawRoot = true
			}

			switch {
			case t.Name.Space == mcNS && t.Name.Local == "Fallback":
				if err := dec.Skip(); err != nil {
					return "", err
				}
			case isWordNS(t.Name.Space):
				switch t.Name.Local {
				case "t":
					inText = true
				case "tab":
					sb.WriteByte('\t')
				case "br", "cr":
					sb.WriteByte('\n')
				}
			}
		case xml.EndElement:
			if isWordNS(t.Name.Space) {
				switch t.Name.Local {
				case "t":
					inText = false
				case "p":
					sb.WriteString("\n\n")
				}
			}
		case xml.CharData:
			if inText {
				sb.Write(t) // copies; t is only valid until the next Token call
			}
		}

		if sb.Len() > MaxTextBytes {
			return "", ErrTextTooLarge
		}
	}

	if !sawRoot {
		return "", errors.New("no XML root element found")
	}
	return sb.String(), nil
}

func isWordNS(space string) bool { return space == wordNS || space == wordNSStrict }

// limitedReader fails with an ERROR once the limit is passed. A plain
// io.LimitReader would report a clean EOF instead, making a truncated
// (and therefore malformed) document look like a normal parse failure.
type limitedReader struct {
	r         io.Reader
	remaining int64 // starts at limit+1: reading that extra byte proves we went over
	exceeded  bool
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.remaining <= 0 {
		l.exceeded = true
		return 0, ErrTextTooLarge
	}
	if int64(len(p)) > l.remaining {
		p = p[:l.remaining]
	}
	n, err := l.r.Read(p)
	l.remaining -= int64(n)
	return n, err
}