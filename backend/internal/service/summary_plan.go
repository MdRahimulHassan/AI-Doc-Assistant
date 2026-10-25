package service

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/config"
)

const (
	summaryItemOverhead   = config.SummaryItemOverheadTokens
	summaryPromptOverhead = config.SummaryPromptOverheadTokens
)

// itemCost is what one text costs inside a prompt: its own estimate plus its tags.
func itemCost(text string) int { return EstimateTokens(text) + summaryItemOverhead }

// splitToFit divides text into consecutive pieces that each fit maxTokens,
// cutting at a space when one is close and otherwise at a character boundary
// (never inside a character). NOTHING is dropped or repeated: the pieces, put
// back together, are the text. Any text, even one with no spaces at all, ends up
// in pieces; the loop always advances by at least one character.
func splitToFit(text string, maxTokens int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	maxTokens = max(maxTokens, 1)

	var pieces []string
	runes := []rune(text)
	for len(runes) > 0 {
		if EstimateTokens(string(runes)) <= maxTokens {
			pieces = append(pieces, string(runes))
			break
		}

		// The longest prefix that fits. The estimate never decreases as text
		// grows, so a binary search works. At least one character, always.
		best, lo, hi := 1, 1, len(runes)
		for lo <= hi {
			mid := (lo + hi) / 2
			if EstimateTokens(string(runes[:mid])) <= maxTokens {
				best, lo = mid, mid+1
			} else {
				hi = mid - 1
			}
		}

		// Prefer to end at a space, if one is within the last ~14%.
		end := best
		for i := end; i > 0 && end-i <= end/7; i-- {
			if i < len(runes) && unicode.IsSpace(runes[i]) {
				end = i
				break
			}
		}

		if piece := strings.TrimSpace(string(runes[:end])); piece != "" {
			pieces = append(pieces, piece)
		}
		runes = []rune(strings.TrimLeftFunc(string(runes[end:]), unicode.IsSpace))
	}
	return pieces
}

// prepareItems makes every text fit one group on its own: blank texts are
// dropped, and a text that is too big is split (never cut).
func prepareItems(texts []string, budget int) []string {
	var out []string
	for _, t := range texts {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if itemCost(t) <= budget {
			out = append(out, t)
			continue
		}
		out = append(out, splitToFit(t, budget-summaryItemOverhead)...)
	}
	return out
}

// groupItems packs consecutive items, in order, into groups whose total cost
// stays within the budget. Order is strict: an item never jumps ahead.
func groupItems(items []string, budget int) [][]string {
	var groups [][]string
	var cur []string
	used := 0
	for _, it := range items {
		c := itemCost(it)
		if len(cur) > 0 && used+c > budget {
			groups = append(groups, cur)
			cur, used = nil, 0
		}
		cur = append(cur, it)
		used += c
	}
	if len(cur) > 0 {
		groups = append(groups, cur)
	}
	return groups
}

// summaryPlan is the first layer of a job, worked out before anything is sent.
type summaryPlan struct {
	Chunks      int        // non-blank chunks read
	Items       []string   // the chunk texts, split where one alone would not fit
	Groups      [][]string // consecutive items packed under the budget
	InputTokens int        // estimated tokens of all items, tags included
	Budget      int
}

func planSummary(chunks []string, budget int) summaryPlan {
	items := prepareItems(chunks, budget)

	nonBlank := 0
	for _, c := range chunks {
		if strings.TrimSpace(c) != "" {
			nonBlank++
		}
	}
	input := 0
	for _, it := range items {
		input += itemCost(it)
	}
	return summaryPlan{Chunks: nonBlank, Items: items, Groups: groupItems(items, budget), InputTokens: input, Budget: budget}
}

// SummaryEstimate is a rough cost of summarizing a document, for logs and for
// the summarycheck tool. Calls and Tokens are LOWER-BOUND-ish: if the first
// layer's summaries do not fit one prompt, extra layers add calls.
type SummaryEstimate struct {
	Chunks      int
	Items       int
	MapCalls    int // first-layer calls (0 when everything fits in one call)
	Calls       int // MapCalls plus the final call
	InputTokens int
	Tokens      int // about, across all calls: inputs + prompts + the output limits
}

// EstimateSummary estimates the cost of summarizing these chunks.
func EstimateSummary(chunks []string, groupTokens, maxOutputTokens int) SummaryEstimate {
	return planSummary(chunks, groupTokens).estimate(maxOutputTokens)
}

func (p summaryPlan) estimate(maxOutput int) SummaryEstimate {
	e := SummaryEstimate{Chunks: p.Chunks, Items: len(p.Items), InputTokens: p.InputTokens}
	switch len(p.Groups) {
	case 0:
	case 1:
		e.Calls = 1
	default:
		e.MapCalls = len(p.Groups)
		e.Calls = e.MapCalls + 1
	}
	e.Tokens = p.InputTokens + e.Calls*(summaryPromptOverhead+maxOutput)
	if e.MapCalls > 0 { // the final call reads the summaries
		e.Tokens += min(e.MapCalls*maxOutput, p.Budget)
	}
	return e
}

// partTag matches the start of any tag the summary prompts use, in any case and
// with any spacing: "<part", "</PARTS", "< / part".
var partTag = regexp.MustCompile(`(?i)<(\s*/?\s*)(parts?)`)

// neutralizeParts makes prompt-like tags harmless while preserving their text.
func neutralizeParts(s string) string {
	return partTag.ReplaceAllString(s, "‹${1}${2}")
}

// renderParts lays the texts out for the model.
func renderParts(items []string) string {
	var b strings.Builder
	b.WriteString("<parts>\n")
	for i, it := range items {
		fmt.Fprintf(&b, "<part n=\"%d\" of=\"%d\">\n%s\n</part>\n", i+1, len(items), neutralizeParts(it))
	}
	b.WriteString("</parts>")
	return b.String()
}