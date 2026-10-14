package parser

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"unicode"

	"github.com/ledongthuc/pdf"
)

// PDF extracts text from PDFs that have a text layer. Scanned (image-only)
// PDFs yield no text, so they end as ErrNoText. OCR is out of scope.
type PDFGo struct{}

func (PDFGo) Parse(ctx context.Context, r io.ReaderAt, size int64) (string, error) {
	reader, err := pdf.NewReader(r, size)
	if err != nil {
		return "", fmt.Errorf("%w: pdf: %v", ErrCorrupt, err)
	}

	pages := reader.NumPage()
	var sb strings.Builder
	failed := 0

	for i := 1; i <= pages; i++ {
		// The library can't be interrupted mid-page. Checking between pages is
		// the best cooperative cancellation available.
		if err := ctx.Err(); err != nil {
			return "", err
		}

		text, err := pdfPageText(reader, i)
		if err != nil {
			// One unreadable page shouldn't discard the whole document.
			failed++
			slog.Warn("pdf page skipped", "page", i, "error", err)
			continue
		}

		sb.WriteString(text)
		sb.WriteString("\n\n") // pages are separated by a blank line
		if sb.Len() > MaxTextBytes {
			return "", ErrTextTooLarge
		}
	}

	if pages > 0 && failed == pages {
		return "", fmt.Errorf("%w: pdf: none of the %d pages could be read", ErrCorrupt, pages)
	}
	text := sb.String()
	if looksGlued(text) {
		return "", fmt.Errorf("%w: pdf: the text has almost no spaces between words; "+
			"this PDF needs poppler's pdftotext", ErrUnreliableText)
	}
	return text, nil
}

// pdfPageText extracts one page. A fresh font map per page avoids mixing up
// fonts when different pages reuse the same font name for different fonts.
// The recover turns a panic on one bad page into an ordinary error.
func pdfPageText(reader *pdf.Reader, n int) (text string, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			text, err = "", fmt.Errorf("page %d: panic: %v", n, rec)
		}
	}()

	page := reader.Page(n)
	if page.V.IsNull() {
		return "", nil
	}

	fonts := make(map[string]*pdf.Font)
	for _, name := range page.Fonts() {
		f := page.Font(name)
		fonts[name] = &f
	}
	text, err = page.GetPlainText(fonts)
	if err != nil {
		return "", err
	}
	// The library's line breaks are unreliable (some PDFs come out one word per
	// line), so flatten each page into a single paragraph.
	return strings.Join(strings.Fields(text), " "), nil
}

func looksGlued(text string) bool {
	var letters, spaces int
	for _, r := range text {
		switch {
		case unicode.IsSpace(r):
			spaces++
		case unicode.IsLetter(r) && r < 0x250:
			// Latin letters only. CJK and other scripts are not judged by
			// this rule: Chinese legitimately has no spaces.
			letters++
		}
	}
	// English averages about one space per 5-6 letters. Fewer than one per 20
	// means the words are fused. Short samples are not judged.
	return letters >= 500 && spaces*20 < letters
}