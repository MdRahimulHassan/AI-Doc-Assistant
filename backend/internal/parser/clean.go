package parser

import (
	"strings"
	"unicode"
)

// PDFs from LaTeX and Word often use single-glyph ligatures. Without this,
// the text contains "ﬁndings" and a search for "findings" misses it.
var ligatures = strings.NewReplacer(
	"\uFB00", "ff", "\uFB01", "fi", "\uFB02", "fl", "\uFB03", "ffi",
	"\uFB04", "ffl", "\uFB05", "st", "\uFB06", "st",
)

// Clean makes extracted text safe and tidy:
//   - removes NUL and other control characters (Postgres TEXT rejects NUL)
//   - repairs invalid UTF-8
//   - normalizes line endings to \n
//   - collapses runs of spaces/tabs/non-breaking spaces into one space
//   - trims spaces around lines
//   - keeps paragraph breaks as ONE blank line (at most two newlines in a row)
//
// It is idempotent: Clean(Clean(x)) == Clean(x).
func Clean(s string) string {
	s = strings.ToValidUTF8(s, " ")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = ligatures.Replace(s)

	var b strings.Builder
	b.Grow(len(s))

	newlines := 0        // line breaks seen since the last visible character
	pendingSpace := false // a space is owed before the next visible character

	for _, r := range s {
		switch {
		case r == '\n' || r == '\v':
			newlines++
			pendingSpace = false // drops trailing spaces on the line
		case r == '\f': // form feed = page break
			newlines += 2
			pendingSpace = false
		case r == '\uFEFF' || r == '\u200B' || r == '\u00AD':
			// BOM, zero-width space, soft hyphen: invisible noise, dropped.
			// U+200C/U+200D (joiners) are deliberately KEPT: they change how
			// Bengali and other Indic scripts are written.
		case unicode.IsSpace(r): // tabs, NBSP, and other Unicode spaces
			pendingSpace = true
		case unicode.IsControl(r): // NUL and remaining control characters
			// dropped
		default:
			if b.Len() > 0 { // nothing is written before the first visible character
				switch {
				case newlines > 0:
					if newlines > 2 {
						newlines = 2
					}
					b.WriteString("\n\n"[:newlines])
				case pendingSpace:
					b.WriteByte(' ')
				}
			}
			newlines = 0
			pendingSpace = false
			b.WriteRune(r)
		}
	}
	return b.String() // pending whitespace at the end is simply never written
}