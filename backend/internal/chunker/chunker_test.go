package chunker

import (
	"fmt"
	"math/rand"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// --- helpers ---

// gen produces words that are unique across a whole document ("W1", "W2", ...),
// so tests can tell exactly which words ended up where.
type gen struct{ next int }

func (g *gen) words(n int) []string {
	out := make([]string, n)
	for i := range out {
		g.next++
		out[i] = "W" + strconv.Itoa(g.next)
	}
	return out
}

// paragraph builds n words in sentences of sentLen words (a period ends each
// sentence). sentLen <= 0 means no punctuation at all.
func (g *gen) paragraph(n, sentLen int) string {
	ws := g.words(n)
	if sentLen > 0 {
		for i := sentLen - 1; i < len(ws); i += sentLen {
			ws[i] += "."
		}
		if last := len(ws) - 1; !strings.HasSuffix(ws[last], ".") {
			ws[last] += "."
		}
	}
	return strings.Join(ws, " ")
}

func (g *gen) doc(sizes []int, sentLen int) string {
	paras := make([]string, len(sizes))
	for i, n := range sizes {
		paras[i] = g.paragraph(n, sentLen)
	}
	return strings.Join(paras, "\n\n")
}

// checkInvariants verifies the properties that must hold for any input made
// of unique words. It measures sizes with strings.Fields, independent of the
// implementation's own counting.
func checkInvariants(t *testing.T, c *Chunker, input string, chunks []string) {
	t.Helper()

	// 1. No empty chunks, valid UTF-8, and never more than max words.
	for i, ch := range chunks {
		if strings.TrimSpace(ch) == "" {
			t.Errorf("chunk %d is empty", i)
		}
		if !utf8.ValidString(ch) {
			t.Errorf("chunk %d is not valid UTF-8", i)
		}
		if n := len(strings.Fields(ch)); n > c.max {
			t.Errorf("chunk %d has %d words, the maximum is %d", i, n, c.max)
		}
	}

	// 2. Nothing is lost, and order is preserved: every input word appears,
	// and first appearances never go backwards through the chunks.
	inWords := strings.Fields(input)
	first := make(map[string]int, len(inWords))
	for i, ch := range chunks {
		for _, w := range strings.Fields(ch) {
			if _, seen := first[w]; !seen {
				first[w] = i
			}
		}
	}
	prevIdx := 0
	for _, w := range inWords {
		idx, ok := first[w]
		if !ok {
			t.Fatalf("word %q is missing from every chunk", w)
		}
		if idx < prevIdx {
			t.Fatalf("word %q first appears in chunk %d, but a later word already appeared in chunk %d", w, idx, prevIdx)
		}
		prevIdx = idx
	}

	// 3. Overlap: each chunk starts with the last words of the previous one.
	for i := 1; i < len(chunks); i++ {
		prev := strings.Fields(chunks[i-1])
		cur := strings.Fields(chunks[i])
		n := min(c.overlap, len(prev))
		if len(cur) < n || !slices.Equal(prev[len(prev)-n:], cur[:n]) {
			t.Errorf("chunk %d does not start with the last %d words of chunk %d", i, n, i-1)
		}
	}
}

// --- configuration ---

func TestNew_Validation(t *testing.T) {
	tests := []struct {
		name                  string
		target, max, overlap  int
		wantErr               bool
	}{
		{"defaults", DefaultTargetWords, DefaultMaxWords, DefaultOverlapWords, false},
		{"no overlap", 100, 150, 0, false},
		{"max exactly target plus overlap", 100, 120, 20, false},
		{"zero target", 0, 100, 0, true},
		{"negative target", -5, 100, 0, true},
		{"negative overlap", 100, 200, -1, true},
		{"overlap equals target", 100, 300, 100, true},
		{"overlap larger than target", 100, 300, 150, true},
		{"max too small for target plus overlap", 100, 119, 20, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := New(tc.target, tc.max, tc.overlap)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if err == nil && (c.target != tc.target || c.max != tc.max || c.overlap != tc.overlap) {
				t.Errorf("configuration not stored: %+v", c)
			}
		})
	}
}

func TestDefault_IsValid(t *testing.T) {
	c := Default() // panics if the constants are inconsistent
	if c.target != DefaultTargetWords || c.max != DefaultMaxWords || c.overlap != DefaultOverlapWords {
		t.Errorf("unexpected defaults: %+v", c)
	}
	if c.bodyMax != DefaultMaxWords-DefaultOverlapWords {
		t.Errorf("bodyMax = %d, want %d", c.bodyMax, DefaultMaxWords-DefaultOverlapWords)
	}
}

// --- the cases from the step ---

func TestChunk_EmptyInput(t *testing.T) {
	for _, in := range []string{"", " ", "\n\n\n", " \t\r\n \n  "} {
		if got := Default().Chunk(in); len(got) != 0 {
			t.Errorf("Chunk(%q) returned %d chunks, want 0", in, len(got))
		}
	}
}

func TestChunk_TinyInputIsExactlyOneChunk(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"one sentence", "Hello world.", "Hello world."},
		{"line breaks inside a paragraph are kept", "line one\nline two", "line one\nline two"},
		{"short paragraphs stay together",
			"First para.\n\nSecond para.\n\nThird para.",
			"First para.\n\nSecond para.\n\nThird para."},
		{"extra blank lines collapse", "A.\n\n\n\n   \n\nB.", "A.\n\nB."},
		{"Windows line endings", "A.\r\n\r\nB.", "A.\n\nB."},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Default().Chunk(tc.in)
			if len(got) != 1 || got[0] != tc.want {
				t.Errorf("got %q, want exactly [%q]", got, tc.want)
			}
		})
	}
}

func TestChunk_ManyShortParagraphs(t *testing.T) {
	c := Default()
	g := &gen{}

	sizes := make([]int, 400) // 400 paragraphs of 10 words = 4000 words
	for i := range sizes {
		sizes[i] = 10
	}
	paras := make([]string, len(sizes))
	for i, n := range sizes {
		paras[i] = g.paragraph(n, 0)
	}
	input := strings.Join(paras, "\n\n")

	chunks := c.Chunk(input)
	if len(chunks) < 2 {
		t.Fatalf("got %d chunks, want several", len(chunks))
	}
	checkInvariants(t, c, input, chunks)

	// Every chunk except the last is filled to at least the target.
	for i, ch := range chunks[:len(chunks)-1] {
		if n := len(strings.Fields(ch)); n < c.target {
			t.Errorf("chunk %d has only %d words, want at least %d", i, n, c.target)
		}
	}

	// Paragraphs are never cut: each one appears whole in some chunk.
	for _, p := range paras {
		found := false
		for _, ch := range chunks {
			if strings.Contains(ch, p) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("paragraph %q was split across chunks", p)
		}
	}
}

func TestChunk_HugeParagraphSplitsOnSentences(t *testing.T) {
	c := Default()
	g := &gen{}
	input := g.paragraph(3000, 20) // one paragraph, 150 sentences

	chunks := c.Chunk(input)
	if len(chunks) < 2 {
		t.Fatalf("got %d chunks, want several", len(chunks))
	}
	checkInvariants(t, c, input, chunks)

	for i, ch := range chunks {
		if !strings.HasSuffix(ch, ".") {
			t.Errorf("chunk %d does not end at a sentence boundary: ...%q", i, ch[max(0, len(ch)-30):])
		}
		if strings.Contains(ch, "\n\n") {
			t.Errorf("chunk %d contains a paragraph break, but the input is one paragraph", i)
		}
	}
}

func TestChunk_HugeSentenceFallsBackToWords(t *testing.T) {
	c := Default()
	g := &gen{}
	input := g.paragraph(2000, 0) // 2000 words, no punctuation at all

	chunks := c.Chunk(input)
	checkInvariants(t, c, input, chunks)

	// Groups of 450 words: 450, 450, 450, 450, 200.
	if len(chunks) != 5 {
		t.Errorf("got %d chunks, want 5", len(chunks))
	}
}

func TestChunk_SizeBoundaries(t *testing.T) {
	c := Default()

	t.Run("a paragraph of exactly the largest allowed size stays whole", func(t *testing.T) {
		g := &gen{}
		input := g.doc([]int{c.bodyMax, c.bodyMax}, 0)

		chunks := c.Chunk(input)
		checkInvariants(t, c, input, chunks)
		if len(chunks) != 2 {
			t.Fatalf("got %d chunks, want 2", len(chunks))
		}
		if n := len(strings.Fields(chunks[0])); n != c.bodyMax {
			t.Errorf("chunk 0 has %d words, want %d", n, c.bodyMax)
		}
		// 55 words of overlap + 545 new words is exactly the maximum.
		if n := len(strings.Fields(chunks[1])); n != c.max {
			t.Errorf("chunk 1 has %d words, want exactly %d", n, c.max)
		}
	})

	t.Run("one word more is split instead", func(t *testing.T) {
		g := &gen{}
		input := g.paragraph(c.bodyMax+1, 0)

		chunks := c.Chunk(input)
		checkInvariants(t, c, input, chunks)
		if len(chunks) != 2 {
			t.Errorf("got %d chunks, want 2", len(chunks))
		}
	})
}

// --- Unicode ---

func TestChunk_Bengali(t *testing.T) {
	c := Default()
	g := &gen{}

	// 12-word sentences ending in a danda (।). The big paragraph (1200 words)
	// forces the sentence splitter to handle the danda.
	block := func(sentences int) string {
		var sb strings.Builder
		for s := 0; s < sentences; s++ {
			for w := 0; w < 11; w++ {
				g.next++
				fmt.Fprintf(&sb, "শব্দ%d ", g.next)
			}
			g.next++
			fmt.Fprintf(&sb, "শব্দ%d। ", g.next)
		}
		return strings.TrimSpace(sb.String())
	}
	input := strings.Join([]string{block(10), block(10), block(100), block(10)}, "\n\n")

	chunks := c.Chunk(input)
	if len(chunks) < 2 {
		t.Fatalf("got %d chunks, want several", len(chunks))
	}
	checkInvariants(t, c, input, chunks)

	for i, ch := range chunks {
		if strings.ContainsRune(ch, utf8.RuneError) {
			t.Errorf("chunk %d contains U+FFFD: a character was damaged", i)
		}
	}
}

func TestChunk_CJKWithoutSpaces(t *testing.T) {
	c := Default()

	const n = 3000
	var sb strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&sb, "第%d句。", i) // no spaces anywhere
	}

	chunks := c.Chunk(sb.String())
	if len(chunks) < 2 {
		t.Fatalf("got %d chunk(s): unspaced text was not split", len(chunks))
	}

	joined := strings.Join(chunks, "\n")
	for i := 1; i <= n; i++ {
		if !strings.Contains(joined, fmt.Sprintf("第%d句。", i)) {
			t.Fatalf("sentence %d is missing", i)
		}
	}
	for i, ch := range chunks {
		if !utf8.ValidString(ch) {
			t.Errorf("chunk %d is not valid UTF-8", i)
		}
		if w := weight(ch); w > c.max {
			t.Errorf("chunk %d weighs %d words, the maximum is %d", i, w, c.max)
		}
	}
}

func TestChunk_HugeTokensAreSplitSafely(t *testing.T) {
	cases := map[string]string{
		"ascii":      strings.Repeat("a", 100_000),
		"bengali":    strings.Repeat("বই", 20_000),
		"emoji mix":  strings.Repeat("😀বই", 10_000),
	}

	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			c := Default()
			chunks := c.Chunk(input)
			if len(chunks) < 2 {
				t.Fatalf("got %d chunk(s), want several", len(chunks))
			}

			total := 0
			for i, ch := range chunks {
				if !utf8.ValidString(ch) || strings.ContainsRune(ch, utf8.RuneError) {
					t.Fatalf("chunk %d has a damaged character (a rune was cut in half)", i)
				}
				if w := weight(ch); w > c.max {
					t.Errorf("chunk %d weighs %d words, the maximum is %d", i, w, c.max)
				}
				if n := len(strings.Fields(ch)); n > c.max {
					t.Errorf("chunk %d has %d tokens, the maximum is %d", i, n, c.max)
				}
				total += utf8.RuneCountInString(strings.Join(strings.Fields(ch), ""))
			}
			// Overlap repeats some characters, so the total can only be larger.
			if want := utf8.RuneCountInString(input); total < want {
				t.Errorf("characters lost: chunks hold %d, input has %d", total, want)
			}
		})
	}
}

// --- other behaviors ---

func TestChunk_NoOverlap(t *testing.T) {
	c, err := New(100, 150, 0)
	if err != nil {
		t.Fatal(err)
	}

	g := &gen{}
	sizes := make([]int, 34) // 34 paragraphs of 30 words = 1020 words
	for i := range sizes {
		sizes[i] = 30
	}
	input := g.doc(sizes, 10)

	chunks := c.Chunk(input)
	checkInvariants(t, c, input, chunks)

	total := 0
	for _, ch := range chunks {
		total += len(strings.Fields(ch))
	}
	if total != 1020 {
		t.Errorf("chunks hold %d words in total, want exactly 1020 (no duplicates without overlap)", total)
	}
}

func TestChunk_IsDeterministic(t *testing.T) {
	g := &gen{}
	input := g.doc([]int{300, 700, 50, 1500, 20}, 15)

	a := Default().Chunk(input)
	b := Default().Chunk(input)
	if !slices.Equal(a, b) {
		t.Error("the same input produced different chunks")
	}
}

// Random documents with paragraphs of 1-900 words and sentences of 0-44 words,
// run through three very different configurations. A seeded generator keeps
// any failure reproducible.
func TestChunk_RandomDocuments(t *testing.T) {
	small, err := New(20, 40, 5)
	if err != nil {
		t.Fatal(err)
	}
	tiny, err := New(3, 7, 2)
	if err != nil {
		t.Fatal(err)
	}

	for seed := int64(1); seed <= 10; seed++ {
		r := rand.New(rand.NewSource(seed))
		g := &gen{}

		paras := make([]string, 1+r.Intn(25))
		for i := range paras {
			paras[i] = g.paragraph(1+r.Intn(900), r.Intn(45))
		}
		input := strings.Join(paras, "\n\n")

		for name, c := range map[string]*Chunker{"default": Default(), "small": small, "tiny": tiny} {
			t.Run(fmt.Sprintf("seed %d/%s", seed, name), func(t *testing.T) {
				chunks := c.Chunk(input)
				if len(chunks) == 0 {
					t.Fatal("no chunks")
				}
				checkInvariants(t, c, input, chunks)
			})
		}
	}
}

// ~700,000 words is about the 5 MB text cap. This catches accidentally
// quadratic code, and, together with -timeout, an infinite loop.
func TestChunk_LargeInputIsFast(t *testing.T) {
	build := func(g *gen, sizes []int) string { return g.doc(sizes, 20) }

	t.Run("many paragraphs", func(t *testing.T) {
		g := &gen{}
		sizes := make([]int, 3000)
		for i := range sizes {
			sizes[i] = 233
		}
		input := build(g, sizes)
		lastWord := fmt.Sprintf("W%d.", g.next)

		start := time.Now()
		chunks := Default().Chunk(input)
		took := time.Since(start)
		t.Logf("%d chunks in %v", len(chunks), took)

		if took > 15*time.Second {
			t.Errorf("took %v, expected far less", took)
		}
		if len(chunks) < 1000 || !strings.HasPrefix(chunks[0], "W1 ") || !strings.HasSuffix(chunks[len(chunks)-1], lastWord) {
			t.Errorf("unexpected result: %d chunks, first %.20q, last ...%q", len(chunks), chunks[0], lastWord)
		}
	})

	t.Run("one giant paragraph", func(t *testing.T) {
		g := &gen{}
		input := build(g, []int{700_000})
		lastWord := fmt.Sprintf("W%d.", g.next)

		start := time.Now()
		chunks := Default().Chunk(input)
		took := time.Since(start)
		t.Logf("%d chunks in %v", len(chunks), took)

		if took > 15*time.Second {
			t.Errorf("took %v, expected far less", took)
		}
		if len(chunks) < 1000 || !strings.HasSuffix(chunks[len(chunks)-1], lastWord) {
			t.Errorf("unexpected result: %d chunks", len(chunks))
		}
	})
}

// --- helpers under test ---

func TestSentences(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"basic terminators", "One. Two! Three? Four.", []string{"One.", "Two!", "Three?", "Four."}},
		{"decimal point is not a sentence end", "Pi is 3.14 exactly. Yes.", []string{"Pi is 3.14 exactly.", "Yes."}},
		{"figure reference", "See Fig. 3 for details. Next one.", []string{"See Fig. 3 for details.", "Next one."}},
		{"abbreviation before a lowercase word", "Use e.g. the first one. Done.", []string{"Use e.g. the first one.", "Done."}},
		{"ellipsis then lowercase", "Wait... what happened? Nothing.", []string{"Wait... what happened?", "Nothing."}},
		{"closing quote stays with its sentence", `He said "stop." Then left.`, []string{`He said "stop."`, "Then left."}},
		{"Bengali danda", "আমি বই পড়ি। তুমি কী করো?", []string{"আমি বই পড়ি।", "তুমি কী করো?"}},
		{"CJK full stop without spaces", "第一句。第二句。", []string{"第一句。", "第二句。"}},
		{"no terminator", "no punctuation here", []string{"no punctuation here"}},
		{"text after the last terminator", "First. Second part without end", []string{"First.", "Second part without end"}},
		{"empty", "", nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sentences(tc.in); !slices.Equal(got, tc.want) {
				t.Errorf("sentences(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParagraphs(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"blank line separates", "a\nb\n\n\nc", []string{"a\nb", "c"}},
		{"whitespace and CRLF", "  x  \r\n\r\n y ", []string{"x", "y"}},
		{"empty", "", nil},
		{"only blank lines", "\n \n\t\n", nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := paragraphs(tc.in); !slices.Equal(got, tc.want) {
				t.Errorf("paragraphs(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestWeightAndSplitWords(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"   \n\t ", 0},
		{"one two", 2},
		{"  spaced   out  ", 2},
		{strings.Repeat("a", 24), 1},
		{strings.Repeat("a", 25), 2},
		{strings.Repeat("a", 48), 2},
		{strings.Repeat("a", 49), 3},
		{strings.Repeat("ব", 49), 3},
	}

	for _, tc := range tests {
		if got := weight(tc.in); got != tc.want {
			t.Errorf("weight(%.12q...) = %d, want %d", tc.in, got, tc.want)
		}
		// splitWords and weight must agree, or the size guarantee breaks.
		if got := len(splitWords(tc.in)); got != tc.want {
			t.Errorf("len(splitWords(%.12q...)) = %d, want %d", tc.in, got, tc.want)
		}
	}
}