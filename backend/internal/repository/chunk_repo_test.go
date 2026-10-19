package repository

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/models"
)

type searchEnv struct {
	chunks *ChunkRepository
	docs   *DocumentRepository
	users  *UserRepository
}

func setupSearch(t *testing.T) *searchEnv {
	t.Helper()
	users, docs, pool := setupDocs(t)
	return &searchEnv{chunks: NewChunkRepository(pool), docs: docs, users: users}
}

// readyDocFor creates a document for an existing user and ingests the chunks,
// which marks it 'ready'.
func (e *searchEnv) readyDocFor(t *testing.T, userID uuid.UUID, chunks []string) *models.Document {
	t.Helper()
	d := mustDoc(t, e.docs, userID, "doc.pdf")
	if err := e.docs.CompleteIngestion(context.Background(), d.ID, chunks); err != nil {
		t.Fatal(err)
	}
	return d
}

func (e *searchEnv) readyDoc(t *testing.T, email string, chunks []string) (*models.User, *models.Document) {
	t.Helper()
	u := mustUser(t, e.users, email)
	return u, e.readyDocFor(t, u.ID, chunks)
}

func (e *searchEnv) terms(t *testing.T, question string) []string {
	t.Helper()
	got, err := e.chunks.QueryTerms(context.Background(), question, 12)
	if err != nil {
		t.Fatalf("QueryTerms(%q): %v", question, err)
	}
	return got
}

func indexes(cs []models.RetrievedChunk) []int {
	out := make([]int, len(cs))
	for i, c := range cs {
		out[i] = c.Index
	}
	return out
}

// Both rankings go through the same ownership and safety checks.
type searchFunc func(ctx context.Context, userID, docID uuid.UUID, terms []string, limit int) ([]models.RetrievedChunk, error)

func (e *searchEnv) modes() map[string]searchFunc {
	return map[string]searchFunc{"bm25": e.chunks.SearchChunks, "rank_cd": e.chunks.SearchChunksByRankCD}
}

// --- turning a question into terms ---

func TestChunkRepo_QueryTerms(t *testing.T) {
	e := setupSearch(t)

	t.Run("a natural question keeps the content words and drops the stop words", func(t *testing.T) {
		got := e.terms(t, "What did the paper conclude about gold editorials?")
		for _, stop := range []string{"what", "did", "the", "about"} {
			if slices.Contains(got, stop) {
				t.Errorf("stop word %q was kept: %v", stop, got)
			}
		}
		for _, want := range []string{"paper", "gold"} {
			if !slices.Contains(got, want) {
				t.Errorf("%q is missing: %v", want, got)
			}
		}
		if !slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, "editori") }) {
			t.Errorf("the stem of 'editorials' is missing: %v", got)
		}
	})

	t.Run("only stop words, or nothing, gives an empty non-nil slice", func(t *testing.T) {
		for _, q := range []string{"what is the", "", "   ", "?!..."} {
			if got := e.terms(t, q); got == nil || len(got) != 0 {
				t.Errorf("QueryTerms(%q) = %#v, want an empty non-nil slice", q, got)
			}
		}
	})

	t.Run("the cap keeps the longest, most specific terms", func(t *testing.T) {
		var words []string
		for i := 1; i <= 30; i++ {
			words = append(words, strings.Repeat("7", i)) // "7", "77", "777", ...
		}
		got, err := e.chunks.QueryTerms(context.Background(), strings.Join(words, " "), 12)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 12 {
			t.Fatalf("%d terms, want 12", len(got))
		}
		for _, w := range got {
			if len(w) < 19 {
				t.Errorf("kept a short term %q while longer ones were dropped", w)
			}
		}
	})

	t.Run("odd input does not break it", func(t *testing.T) {
		for _, q := range []string{
			"'; DROP TABLE users; --",
			`back\slash and "quotes" and 'apostrophes'`,
			"nul\x00byte",
			"emoji 😀 and আমি বই পড়ি",
			strings.Repeat("word ", 2000),
		} {
			if _, err := e.chunks.QueryTerms(context.Background(), q, 12); err != nil {
				t.Errorf("QueryTerms(%.30q): %v", q, err)
			}
		}
	})
}

// --- ranking ---

func TestChunkRepo_Search_RareTermsBeatRepeatedCommonOnes(t *testing.T) {
	e := setupSearch(t)
	ctx := context.Background()

	u, doc := e.readyDoc(t, "a@example.com", []string{
		strings.TrimSpace(strings.Repeat("document ", 12)),        // 0: a common word, twelve times
		"A document describes something. Another document follows.", // 1
		"Every document has a title and a body.",                   // 2
		"The word inverse is rare here.",                           // 3: the rare word, once
		"One more document in the pile.",                           // 4
		"Nothing relevant lives in this paragraph about cats.",     // 5
	})
	terms := e.terms(t, "document inverse")

	got, err := e.chunks.SearchChunks(ctx, u.ID, doc.ID, terms, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0].Index != 3 {
		t.Fatalf("top result %v: the chunk with the rare word must beat the chunk that repeats a common word", indexes(got))
	}
	if !slices.Contains(indexes(got), 0) {
		t.Errorf("the repetitive chunk should still be found: %v", indexes(got))
	}
}

func TestChunkRepo_Search_MoreMatchedTermsRankHigher(t *testing.T) {
	e := setupSearch(t)
	ctx := context.Background()

	u, doc := e.readyDoc(t, "a@example.com", []string{
		"Zebrafish live in freshwater.",                          // 0: one term
		"Zebrafish are studied in laboratories around the world.", // 1: both terms
		"The cat sat on the mat.",                                // 2: neither
		"Dogs bark at night.",                                    // 3: neither
	})
	terms := e.terms(t, "zebrafish laboratory") // "laboratory" matches "laboratories" through stemming

	got, err := e.chunks.SearchChunks(ctx, u.ID, doc.ID, terms, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(indexes(got), []int{1, 0}) {
		t.Fatalf("order = %v, want [1 0]: chunks without a match must be absent, the one matching both terms first", indexes(got))
	}
	if got[0].MatchedTerms != 2 || got[1].MatchedTerms != 1 {
		t.Errorf("matched terms = %d and %d, want 2 and 1", got[0].MatchedTerms, got[1].MatchedTerms)
	}
	if got[0].Score <= got[1].Score {
		t.Errorf("scores %v and %v: the better chunk must score higher", got[0].Score, got[1].Score)
	}
}

func TestChunkRepo_Search_LimitAndDeterministicTies(t *testing.T) {
	e := setupSearch(t)
	ctx := context.Background()

	same := "The zebrafish is small."
	u, doc := e.readyDoc(t, "a@example.com", []string{same, same, same, same, same})
	terms := e.terms(t, "zebrafish")

	for name, search := range e.modes() {
		t.Run(name, func(t *testing.T) {
			got, err := search(ctx, u.ID, doc.ID, terms, 3)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(indexes(got), []int{0, 1, 2}) {
				t.Errorf("limit 3 gave %v, want [0 1 2] (equal scores are ordered by chunk index)", indexes(got))
			}
			if all, _ := search(ctx, u.ID, doc.ID, terms, 50); len(all) != 5 {
				t.Errorf("limit 50 gave %d chunks, want all 5", len(all))
			}
			if one, _ := search(ctx, u.ID, doc.ID, terms, 0); len(one) != 1 {
				t.Errorf("limit 0 gave %d chunks, want it clamped to 1", len(one))
			}
		})
	}
}

// --- scope and ownership ---

func TestChunkRepo_Search_ScopedToTheDocumentAndOwner(t *testing.T) {
	e := setupSearch(t)
	ctx := context.Background()

	alice, docA := e.readyDoc(t, "alice@example.com", []string{"zebrafish marker-A", "unrelated text"})
	docB := e.readyDocFor(t, alice.ID, []string{"zebrafish marker-B"}) // the same user's OTHER document
	bob := mustUser(t, e.users, "bob@example.com")
	processing := mustDoc(t, e.docs, alice.ID, "p.pdf") // never ingested

	failed := e.readyDocFor(t, alice.ID, []string{"zebrafish old chunks"}) // has chunks, then fails
	if err := e.docs.MarkFailed(ctx, failed.ID, "failed"); err != nil {
		t.Fatal(err)
	}
	terms := e.terms(t, "zebrafish")

	for name, search := range e.modes() {
		t.Run(name, func(t *testing.T) {
			got, err := search(ctx, alice.ID, docA.ID, terms, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || !strings.Contains(got[0].Content, "marker-A") {
				t.Errorf("results %+v: only the searched document's chunks may be returned (not %v's)", got, docB.ID)
			}

			notFound := map[string]struct{ user, doc uuid.UUID }{
				"another user's document":      {bob.ID, docA.ID},
				"swapped arguments":            {docA.ID, alice.ID},
				"a document that is not ready": {alice.ID, processing.ID},
				"a failed document":            {alice.ID, failed.ID},
				"a document that is not there": {alice.ID, uuid.New()},
			}
			for label, c := range notFound {
				if _, err := search(ctx, c.user, c.doc, terms, 10); !errors.Is(err, models.ErrNotFound) {
					t.Errorf("%s: err = %v, want ErrNotFound", label, err)
				}
			}
		})
	}
}

func TestChunkRepo_Search_NoTermsAndNoMatchesAreEmptyNotErrors(t *testing.T) {
	e := setupSearch(t)
	ctx := context.Background()
	u, doc := e.readyDoc(t, "a@example.com", []string{"The zebrafish is small.", "Another chunk."})

	for name, search := range e.modes() {
		t.Run(name, func(t *testing.T) {
			for label, terms := range map[string][]string{
				"nil terms":   nil,
				"empty terms": {},
				"blank terms": {"", "   "},
				"no match":    {"nonexistentterm"},
			} {
				got, err := search(ctx, u.ID, doc.ID, terms, 5)
				if err != nil || got == nil || len(got) != 0 {
					t.Errorf("%s: got (%#v, %v), want an empty non-nil slice and no error", label, got, err)
				}
			}
		})
	}
}

// --- quoting ---

func TestChunkRepo_Search_HostileTermsCannotBreakTheQuery(t *testing.T) {
	e := setupSearch(t)
	ctx := context.Background()
	u, doc := e.readyDoc(t, "a@example.com", []string{"plain text about zebrafish", "something else"})

	hostile := []string{
		"a'b", `c\d`, "e:*f", "g|h", "(i)", "!j", "k&l", "m<->n", "'", `\`, "''", `\\`, ") | (",
		"x\x00y", "", strings.Repeat("z", 300), "bad\xffutf8",
	}

	for name, search := range e.modes() {
		t.Run(name, func(t *testing.T) {
			got, err := search(ctx, u.ID, doc.ID, hostile, 5)
			if err != nil || len(got) != 0 {
				t.Errorf("hostile terms alone: got (%v, %v), want no results and no error", indexes(got), err)
			}

			// One hostile term must not spoil the good ones next to it.
			got, err = search(ctx, u.ID, doc.ID, append(slices.Clone(hostile), "zebrafish"), 5)
			if err != nil || !slices.Equal(indexes(got), []int{0}) {
				t.Errorf("mixed terms: got (%v, %v), want chunk 0", indexes(got), err)
			}
		})
	}
}

func TestChunkRepo_Search_TermsWithPunctuationAreQuotedCorrectly(t *testing.T) {
	e := setupSearch(t)
	ctx := context.Background()
	u, doc := e.readyDoc(t, "a@example.com", []string{
		"Send questions to user@example.com for details.",
		"Nothing about contact addresses here.",
	})

	terms := e.terms(t, "write to user@example.com")
	if !slices.Contains(terms, "user@example.com") {
		t.Fatalf("terms = %v: the address should be one term", terms)
	}
	for name, search := range e.modes() {
		got, err := search(ctx, u.ID, doc.ID, terms, 5)
		if err != nil || len(got) == 0 || got[0].Index != 0 {
			t.Errorf("%s: got (%v, %v), want chunk 0 first", name, indexes(got), err)
		}
	}
}

// --- snippets ---

func TestChunkRepo_Snippet(t *testing.T) {
	e := setupSearch(t)
	ctx := context.Background()

	long := strings.Repeat("Filler words that say nothing at all. ", 30) +
		"The zebrafish embryo is transparent under the microscope. " +
		strings.Repeat("More filler text goes here. ", 30)

	t.Run("shows the passage around the match, marked", func(t *testing.T) {
		got, err := e.chunks.Snippet(ctx, long, e.terms(t, "transparent zebrafish"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "[[") || !strings.Contains(got, "transparent") {
			t.Errorf("snippet %q does not show the match", got)
		}
		if len(got) > len(long)/2 {
			t.Errorf("snippet is %d bytes for %d bytes of text: it should be a short extract", len(got), len(long))
		}
	})

	t.Run("without terms it shows the beginning", func(t *testing.T) {
		got, err := e.chunks.Snippet(ctx, long, nil)
		if err != nil {
			t.Fatal(err)
		}
		if n := utf8.RuneCountInString(got); n < 100 || n > 201 || !strings.HasPrefix(got, "Filler words") {
			t.Errorf("got %d characters: %q", n, got)
		}
	})

	t.Run("odd terms do not break it", func(t *testing.T) {
		if _, err := e.chunks.Snippet(ctx, long, []string{"a'b", `c\d`, "x\x00y", ""}); err != nil {
			t.Errorf("Snippet: %v", err)
		}
	})
}