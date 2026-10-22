package service

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/models"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/parser"
)

const (
	defaultMaxQuestionChars = 1000
	minContextBudget        = 200 // a smaller passage budget cannot answer anything
	passageOverheadTokens   = 12  // accounted per passage for its tags (they really cost about 8)

	// History: at most this many earlier exchanges, each side truncated, under a
	// small total budget. Follow-ups still retrieve badly (retrieval only sees
	// the new question); history just helps the model understand what "it" means.
	maxHistoryTurns     = 2
	historyBudgetTokens = 300
	historyQuestionToks = 45
	historyAnswerToks   = 90
	historyTurnOverhead = 8

	truncationMark = " …"
)

var (
	ErrNoPassages      = errors.New("prompt: there are no passages to answer from")
	ErrEmptyQuestion   = errors.New("prompt: the question is empty")
	ErrQuestionTooLong = errors.New("prompt: the question is too long")
)

// HistoryTurn is one earlier exchange of the conversation.
type HistoryTurn struct {
	Question string
	Answer   string
}

// PromptInput is everything the builder needs.
type PromptInput struct {
	Question         string
	Chunks           []models.RetrievedChunk // best first, as retrieval returns them
	History          []HistoryTurn           // oldest first; optional
	Model            string
	MaxOutputTokens  int
	ContextBudget    int // tokens available for the passages
	MaxQuestionChars int // 0 means 1000
}

// PromptSource says which chunk a passage label stands for.
type PromptSource struct {
	Label      string // "P1": what the model cites
	ChunkIndex int    // chunks.chunk_index of the document
	Truncated  bool   // the passage had to be cut to fit the budget
}

// Prompt is a request ready to send, plus what went into it.
type Prompt struct {
	Request         ChatRequest
	Sources         []PromptSource // in the order of the passages: P1, P2, ...
	Version         string
	PassageTokens   int  // estimated tokens spent on passages (including their tags)
	EstimatedTokens int  // estimated tokens of the whole prompt
	DroppedChunks   int  // retrieved chunks that did not fit the budget
	Truncated       bool // the best passage had to be cut to fit
}

// BuildPrompt turns a question and its retrieved chunks into messages for the
// model. It is a pure function: no I/O, no clock, no randomness.
//
// It returns ErrNoPassages when there is nothing to answer from. The caller
// should then answer NotFoundAnswer WITHOUT calling the model.
func BuildPrompt(in PromptInput) (*Prompt, error) {
	switch {
	case strings.TrimSpace(in.Model) == "":
		return nil, errors.New("prompt: a model is required")
	case in.MaxOutputTokens < 1:
		return nil, errors.New("prompt: MaxOutputTokens must be set")
	case in.ContextBudget < minContextBudget:
		return nil, fmt.Errorf("prompt: ContextBudget must be at least %d tokens, got %d", minContextBudget, in.ContextBudget)
	}

	maxQ := in.MaxQuestionChars
	if maxQ <= 0 {
		maxQ = defaultMaxQuestionChars
	}
	question := parser.Clean(in.Question) // same cleaning as the stored text: no NUL bytes, tidy whitespace
	switch {
	case question == "":
		return nil, ErrEmptyQuestion
	case utf8.RuneCountInString(question) > maxQ:
		return nil, ErrQuestionTooLong
	}

	sel := selectPassages(in.Chunks, in.ContextBudget)
	if len(sel.texts) == 0 {
		return nil, ErrNoPassages
	}

	messages := []ChatMessage{{Role: "system", Content: systemPrompt}}
	messages = append(messages, buildHistory(in.History)...)
	messages = append(messages, ChatMessage{Role: "user", Content: renderUserMessage(sel.texts, question)})

	return &Prompt{
		Request: ChatRequest{
			Model:                 in.Model,
			Messages:              messages,
			MaxOutputTokens:       in.MaxOutputTokens,
			Temperature:           qaTemperature,
			ReasoningEffort:       qaReasoningEffort,
			EstimatedPromptTokens: EstimateMessagesTokens(messages), // what the pacer charges
		},
		Sources:         sel.sources,
		Version:         PromptVersion,
		PassageTokens:   sel.tokens,
		EstimatedTokens: EstimateMessagesTokens(messages),
		DroppedChunks:   sel.dropped,
		Truncated:       sel.truncated,
	}, nil
}

// ---------------------------------------------------------------------------
// passages and the token budget
// ---------------------------------------------------------------------------

type selection struct {
	texts     []string
	sources   []PromptSource
	tokens    int
	dropped   int
	truncated bool
}

// selectPassages adds chunks in rank order until the budget is used up, then
// drops the rest. It does NOT skip a chunk that does not fit to squeeze in a
// lower-ranked small one: rank order stays strict. One exception: if the very
// BEST chunk alone is over budget it is cut to fit, because answering from
// nothing is worse than answering from part of the best passage.
func selectPassages(chunks []models.RetrievedChunk, budget int) selection {
	var sel selection
	remaining := budget

	for i, c := range chunks {
		text := neutralize(strings.TrimSpace(c.Content))
		if text == "" {
			continue
		}

		cost := EstimateTokens(text) + passageOverheadTokens
		truncated := false

		switch {
		case cost <= remaining:
			// fits as it is
		case len(sel.texts) == 0:
			text, truncated = truncateToTokens(text, remaining-passageOverheadTokens)
			if text == "" {
				sel.dropped = countNonBlank(chunks[i:])
				return sel
			}
			cost = EstimateTokens(text) + passageOverheadTokens
		default:
			sel.dropped = countNonBlank(chunks[i:])
			return sel
		}

		sel.texts = append(sel.texts, text)
		sel.sources = append(sel.sources, PromptSource{
			Label:      fmt.Sprintf("%s%d", citationPrefix, len(sel.texts)),
			ChunkIndex: c.Index,
			Truncated:  truncated,
		})
		sel.tokens += cost
		remaining -= cost
		if truncated {
			sel.truncated = true
		}
	}
	return sel
}

func countNonBlank(chunks []models.RetrievedChunk) int {
	n := 0
	for _, c := range chunks {
		if strings.TrimSpace(c.Content) != "" {
			n++
		}
	}
	return n
}

// renderUserMessage lays out the passages, then the question OUTSIDE them.
func renderUserMessage(passages []string, question string) string {
	var b strings.Builder
	b.WriteString("<passages>\n")
	for i, text := range passages {
		fmt.Fprintf(&b, "<passage id=\"%s%d\">\n%s\n</passage>\n", citationPrefix, i+1, text)
	}
	b.WriteString("</passages>\n\n<question>\n")
	b.WriteString(neutralize(question))
	b.WriteString("\n</question>")
	return b.String()
}

// ---------------------------------------------------------------------------
// untrusted text
// ---------------------------------------------------------------------------

// reservedTag matches the start of any tag the prompt itself uses, in any case
// and with any spacing: "<passage", "</PASSAGES", "< / passage", "<question".
var reservedTag = regexp.MustCompile(`(?i)<(\s*/?\s*)(passages?|question)`)

// neutralize makes text that looks like one of the prompt's own tags harmless
// by replacing its "<" with the lookalike "‹". A document (or a question) then
// cannot close its own block, open a fake one, or forge the question. The text
// stays readable; everything else is untouched.
//
// This reduces the risk of prompt injection, it does not remove it: a model can
// still be talked into obeying instructions that appear in a passage.
func neutralize(s string) string {
	return reservedTag.ReplaceAllString(s, "‹${1}${2}")
}

// ---------------------------------------------------------------------------
// truncation
// ---------------------------------------------------------------------------

// truncateToTokens shortens text so that its ESTIMATED size is at most
// maxTokens, cutting at a word boundary when one is close and marking the cut
// with "…". The boolean says whether anything was cut. It never splits a
// character. If not even a few words fit it returns "".
func truncateToTokens(text string, maxTokens int) (string, bool) {
	if EstimateTokens(text) <= maxTokens {
		return text, false
	}
	budget := maxTokens - EstimateTokens(truncationMark) - 1
	if budget < 1 {
		return "", true
	}

	// The estimate never decreases as text grows, so the longest prefix that
	// fits can be found by binary search.
	runes := []rune(text)
	lo, hi := 0, len(runes)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if EstimateTokens(string(runes[:mid])) <= budget {
			lo = mid
		} else {
			hi = mid - 1
		}
	}

	// Prefer ending at a word boundary, if one is within the last ~14%.
	end := lo
	for i := lo; i > 0 && lo-i <= lo/7; i-- {
		if i < len(runes) && unicode.IsSpace(runes[i]) {
			end = i
			break
		}
	}

	cut := strings.TrimRightFunc(string(runes[:end]), unicode.IsSpace)
	if cut == "" {
		return "", true
	}
	return cut + truncationMark, true
}

// ---------------------------------------------------------------------------
// conversation history
// ---------------------------------------------------------------------------

// buildHistory turns the last exchanges into chat messages: each side
// truncated, old citations removed (the numbering restarts every question), and
// the total kept under a small budget. Newest turns win when space is short.
func buildHistory(turns []HistoryTurn) []ChatMessage {
	if len(turns) > maxHistoryTurns {
		turns = turns[len(turns)-maxHistoryTurns:]
	}

	type pair struct{ q, a string }
	var picked []pair // newest first
	used := 0

	for i := len(turns) - 1; i >= 0; i-- {
		q := neutralize(parser.Clean(turns[i].Question))
		a := neutralize(StripCitations(parser.Clean(turns[i].Answer)))
		if q == "" || a == "" {
			continue // an unanswered question is not worth the tokens
		}
		q, _ = truncateToTokens(q, historyQuestionToks)
		a, _ = truncateToTokens(a, historyAnswerToks)
		if q == "" || a == "" {
			continue
		}

		cost := EstimateTokens(q) + EstimateTokens(a) + historyTurnOverhead
		if used+cost > historyBudgetTokens {
			break
		}
		used += cost
		picked = append(picked, pair{q, a})
	}

	out := make([]ChatMessage, 0, 2*len(picked))
	for i := len(picked) - 1; i >= 0; i-- { // back to chronological order
		out = append(out,
			ChatMessage{Role: "user", Content: picked[i].q},
			ChatMessage{Role: "assistant", Content: picked[i].a})
	}
	return out
}

// ---------------------------------------------------------------------------
// citations
// ---------------------------------------------------------------------------

var (
	// [P1], [P2, P3], [P1][P2]: one bracket group each.
	citationGroup  = regexp.MustCompile(`\[\s*` + citationPrefix + `\d+(?:\s*[,;]\s*` + citationPrefix + `\d+)*\s*\]`)
	citationNumber = regexp.MustCompile(citationPrefix + `(\d+)`)
)

// StripCitations removes [P1]-style citations and tidies the whitespace.
func StripCitations(s string) string {
	s = citationGroup.ReplaceAllString(s, "")

	// Remove spaces before punctuation left behind by citation removal.
	s = regexp.MustCompile(`\s+([.,;:!?])`).ReplaceAllString(s, "$1")

	// Collapse remaining whitespace, including newlines.
	return strings.Join(strings.Fields(s), " ")
}

// Citations lists the passage numbers an answer cites, each once, in order of
// first appearance, split into numbers that exist (1..sources) and numbers that
// do not. An impossible number such as P0 or one too large to read is reported
// as invalid (-1 for unreadable). Ask uses it to flag answers that cite
// passages that were never given.
func Citations(answer string, sources int) (valid, invalid []int) {
	seen := make(map[int]bool)
	for _, group := range citationGroup.FindAllString(answer, -1) {
		for _, m := range citationNumber.FindAllStringSubmatch(group, -1) {
			n, err := strconv.Atoi(m[1])
			if err != nil {
				n = -1
			}
			if seen[n] {
				continue
			}
			seen[n] = true
			if n >= 1 && n <= sources {
				valid = append(valid, n)
			} else {
				invalid = append(invalid, n)
			}
		}
	}
	return valid, invalid
}
