package chunker

import (
	"fmt"
	"strings"
	"unicode"
)

const (
	// Sizes are in words. At roughly 0.75 words per token these are about
	// 600 and 800 tokens, with a 12% overlap.
	DefaultTargetWords  = 450
	DefaultMaxWords     = 600
	DefaultOverlapWords = 55

	// A whitespace-separated token longer than this counts as several words
	// (and is cut into pieces if it must be split). Without it, text with no
	// spaces (Chinese, Japanese, base64, minified JSON) would count as one
	// "word" and never get split.
	maxWordRunes = 24

	paraSep = "\n\n" // goes before a unit that starts a new paragraph
	sentSep = " "    // goes before a unit that continues the same paragraph
)

// Chunker splits text into overlapping chunks. It holds only configuration,
// so one value can be shared by many goroutines.
type Chunker struct {
	target  int // close a chunk once its new text reaches this many words
	max     int // hard limit for a whole chunk, overlap included
	overlap int // words repeated from the end of the previous chunk
	bodyMax int // max - overlap: the most NEW text one chunk can hold
}

// New validates the parameters up front, so a bad configuration fails at
// startup instead of producing odd chunks in production.
func New(targetWords, maxWords, overlapWords int) (*Chunker, error) {
	switch {
	case targetWords <= 0:
		return nil, fmt.Errorf("chunker: target words must be positive, got %d", targetWords)
	case overlapWords < 0:
		return nil, fmt.Errorf("chunker: overlap words must not be negative, got %d", overlapWords)
	case overlapWords >= targetWords:
		return nil, fmt.Errorf("chunker: overlap (%d) must be smaller than target (%d)", overlapWords, targetWords)
	case maxWords < targetWords+overlapWords:
		return nil, fmt.Errorf("chunker: max (%d) must be at least target+overlap (%d)", maxWords, targetWords+overlapWords)
	}
	return &Chunker{
		target:  targetWords,
		max:     maxWords,
		overlap: overlapWords,
		bodyMax: maxWords - overlapWords,
	}, nil
}

// Default returns a Chunker with the default sizes.
func Default() *Chunker {
	c, err := New(DefaultTargetWords, DefaultMaxWords, DefaultOverlapWords)
	if err != nil {
		panic(err) // the constants are valid; a test pins that
	}
	return c
}

// unit is the smallest piece the packer will not break up.
type unit struct {
	text  string
	words int
	sep   string // what goes before this unit when it follows another in a chunk
}

// Chunk splits text into chunks. The result's index is the chunk's position
// in the document. Empty or whitespace-only text yields no chunks.
func (c *Chunker) Chunk(text string) []string {
	units := c.split(text)
	if len(units) == 0 {
		return nil
	}

	var (
		chunks    []string
		body      strings.Builder // the NEW text of the chunk being built
		bodyWords int
		firstSep  string   // separator to use between overlap and the first unit
		prev      []string // all words of the previous chunk, for the overlap
	)

	flush := func() {
		if bodyWords == 0 {
			return
		}
		out := body.String()
		if c.overlap > 0 && len(prev) > 0 {
			n := min(c.overlap, len(prev))
			out = strings.Join(prev[len(prev)-n:], " ") + firstSep + out
		}
		chunks = append(chunks, out)
		prev = splitWords(out)
		body.Reset()
		bodyWords = 0
	}

	for _, u := range units {
		// Close the chunk if it is full enough, or if this unit would not fit.
		if bodyWords > 0 && (bodyWords >= c.target || bodyWords+u.words > c.bodyMax) {
			flush()
		}
		if bodyWords == 0 {
			firstSep = u.sep
		} else {
			body.WriteString(u.sep)
		}
		body.WriteString(u.text)
		bodyWords += u.words
	}
	flush()
	return chunks
}

// split turns text into units, every one of them at most bodyMax words, so
// any single unit always fits in a chunk.
func (c *Chunker) split(text string) []unit {
	var units []unit

	for _, p := range paragraphs(text) {
		n := weight(p)
		if n <= c.bodyMax {
			units = append(units, unit{text: p, words: n, sep: paraSep})
			continue
		}

		// Paragraph too big for one chunk: fall back to sentences.
		sep := paraSep
		for _, s := range sentences(p) {
			sn := weight(s)
			if sn <= c.bodyMax {
				units = append(units, unit{text: s, words: sn, sep: sep})
				sep = sentSep
				continue
			}

			// Sentence still too big: fall back to groups of words.
			words := splitWords(s)
			for i := 0; i < len(words); i += c.target {
				piece := words[i:min(i+c.target, len(words))]
				units = append(units, unit{text: strings.Join(piece, " "), words: len(piece), sep: sep})
				sep = sentSep
			}
		}
	}
	return units
}

// paragraphs splits on blank lines. Line breaks inside a paragraph are kept.
func paragraphs(text string) []string {
	var out, lines []string

	flush := func() {
		if len(lines) > 0 {
			out = append(out, strings.Join(lines, "\n"))
			lines = lines[:0]
		}
	}

	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line) // also removes a trailing \r
		if line == "" {
			flush()
			continue
		}
		lines = append(lines, line)
	}
	flush()
	return out
}

// sentences splits a paragraph with a simple heuristic: a sentence ends at
// . ! ? … followed by whitespace, or at a CJK/Bengali terminator (which is
// usually NOT followed by a space). A period is not treated as an ending when
// the next word starts with a lowercase letter or a digit, which covers
// "e.g. the", "et al. showed" and "Fig. 3". It will sometimes split after
// "Dr." or "U.S.A."; the only cost is an extra unit boundary.
func sentences(p string) []string {
	runes := []rune(p)
	var out []string
	start := 0

	emit := func(end int) {
		if s := strings.TrimSpace(string(runes[start:end])); s != "" {
			out = append(out, s)
		}
		start = end
	}

	for i := 0; i < len(runes); i++ {
		if !isTerminator(runes[i]) {
			continue
		}
		first := runes[i]

		// Take the whole run of terminators and closing quotes/brackets:
		// "?!", "...", `."`, ".)"
		j := i + 1
		for j < len(runes) && (isTerminator(runes[j]) || isCloser(runes[j])) {
			j++
		}

		if !isHardTerminator(first) && j < len(runes) {
			if !unicode.IsSpace(runes[j]) {
				i = j - 1 // "3.14", "e.g.", "file.txt": not a sentence end
				continue
			}
			if (first == '.' || first == '…') && nextStartsLowerOrDigit(runes, j) {
				i = j - 1 // "Fig. 3", "e.g. the", "et al. showed"
				continue
			}
		}

		emit(j)
		i = j - 1
	}
	emit(len(runes))
	return out
}

func nextStartsLowerOrDigit(runes []rune, from int) bool {
	for k := from; k < len(runes); k++ {
		if unicode.IsSpace(runes[k]) {
			continue
		}
		return unicode.IsLower(runes[k]) || unicode.IsDigit(runes[k])
	}
	return false
}

func isTerminator(r rune) bool {
	switch r {
	case '.', '!', '?', '…', '。', '！', '？', '।', '॥':
		return true
	}
	return false
}

// Hard terminators end a sentence even without a following space.
func isHardTerminator(r rune) bool {
	switch r {
	case '。', '！', '？', '।', '॥':
		return true
	}
	return false
}

func isCloser(r rune) bool {
	switch r {
	case '"', '\'', ')', ']', '}', '”', '’', '»', '」', '』':
		return true
	}
	return false
}

// weight is the size of s in words: whitespace-separated tokens, where a
// token longer than maxWordRunes counts as several. For ordinary text it is
// exactly len(strings.Fields(s)).
func weight(s string) int {
	total, runes := 0, 0
	for _, r := range s {
		if unicode.IsSpace(r) {
			total += tokenWeight(runes)
			runes = 0
			continue
		}
		runes++
	}
	return total + tokenWeight(runes)
}

func tokenWeight(runes int) int {
	return (runes + maxWordRunes - 1) / maxWordRunes // 0 for no token, 1 for a normal word
}

// splitWords is strings.Fields, except that a token longer than maxWordRunes
// is cut into pieces of that many characters. It cuts only at character
// boundaries, so a multi-byte character is never split, and it makes a single
// pass, so a huge token cannot make it slow.
func splitWords(s string) []string {
	fields := strings.Fields(s)
	out := make([]string, 0, len(fields))

	for _, w := range fields {
		start, n := 0, 0
		for i := range w { // i is the byte index where each character starts
			if n == maxWordRunes {
				out = append(out, w[start:i])
				start, n = i, 0
			}
			n++
		}
		out = append(out, w[start:])
	}
	return out
}