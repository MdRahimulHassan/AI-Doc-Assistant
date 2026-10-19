package service

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// ChatMessage is one message of a conversation with the model.
type ChatMessage struct {
	Role    string // "system", "user" or "assistant"
	Content string
}

// ChatRequest is everything one model call needs.
type ChatRequest struct {
	Model    string
	Messages []ChatMessage

	// MaxOutputTokens is required. For a reasoning model it must also cover the
	// model's thinking, so keep it generous (and check FinishReason).
	MaxOutputTokens int
	Temperature     float64

	// ReasoningEffort is "low", "medium" or "high" for models that reason.
	// Empty sends no reasoning settings at all.
	ReasoningEffort string

	// EstimatedPromptTokens is the caller's guess at the prompt's cost, used to
	// pace calls against the shared per-minute budget. Zero means "estimate it
	// from the messages".
	EstimatedPromptTokens int

	// MaxWait is how long this call may wait for token budget or retries.
	// Zero uses the client's interactive default (about 20 seconds). Background
	// jobs set it much higher.
	MaxWait time.Duration
}

type TokenUsage struct {
	PromptTokens     int
	CompletionTokens int // includes reasoning tokens
	TotalTokens      int
	ReasoningTokens  int
}

type ChatResponse struct {
	Model        string
	Content      string
	FinishReason string // "stop" is normal; "length" means the answer was cut off
	Usage        TokenUsage
}

// ChatClient is everything the services need from a model provider.
type ChatClient interface {
	// Complete makes one non-streaming call.
	Complete(ctx context.Context, req ChatRequest) (ChatResponse, error)

	// Stream makes a streaming call and passes each piece of text to onText as
	// it arrives. If onText returns an error the stream is abandoned and that
	// error is returned. The returned response holds everything received, so on
	// failure it is the PARTIAL answer.
	Stream(ctx context.Context, req ChatRequest, onText func(text string) error) (ChatResponse, error)
}

type LLMErrorKind int

const (
	LLMBusy          LLMErrorKind = iota + 1 // the shared token or request budget is used up (429, or our own pacer)
	LLMMisconfigured                         // 401, 403, 404: our key, permissions or model id are wrong
	LLMBadRequest                            // 400, 413, 422: our request is invalid or too large (a bug)
	LLMUnavailable                           // 5xx or a network failure
	LLMTimeout                               // no answer in time
	LLMBadResponse                           // the provider answered in a form we do not understand
)

func (k LLMErrorKind) String() string {
	switch k {
	case LLMBusy:
		return "busy"
	case LLMMisconfigured:
		return "misconfigured"
	case LLMBadRequest:
		return "bad_request"
	case LLMUnavailable:
		return "unavailable"
	case LLMTimeout:
		return "timeout"
	case LLMBadResponse:
		return "bad_response"
	}
	return "unknown"
}

// LLMError describes a failed model call. Message is the provider's own text:
// it is for LOGS ONLY and must never be sent to a client.
type LLMError struct {
	Kind       LLMErrorKind
	Status     int // HTTP status; 0 when there was no HTTP response (or our own pacer refused)
	Code       string
	Message    string
	RetryAfter time.Duration // how long to wait before trying again, if the provider said
	Err        error
}

func (e *LLMError) Error() string {
	var b strings.Builder
	b.WriteString("llm: ")
	b.WriteString(e.Kind.String())
	if e.Status != 0 {
		fmt.Fprintf(&b, " (HTTP %d", e.Status)
		if e.Code != "" {
			fmt.Fprintf(&b, ", %s", e.Code)
		}
		b.WriteString(")")
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if e.RetryAfter > 0 {
		fmt.Fprintf(&b, " (retry after %v)", e.RetryAfter)
	}
	return b.String()
}

func (e *LLMError) Unwrap() error { return e.Err }

// EstimateTokens is a deliberately crude guess, because there is no tokenizer
// here. English costs about 1.3 tokens per word; Bengali, Chinese and other
// non-Latin text costs far more per character, so those characters are counted
// at 1.5 characters per token. Compare it with the real usage the provider
// reports and adjust.
func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	var ascii, other int
	for _, r := range text {
		if r < utf8.RuneSelf {
			ascii++
		} else {
			other++
		}
	}
	byChars := (ascii+3)/4 + (other*2+2)/3
	byWords := len(strings.Fields(text)) * 14 / 10
	return max(byChars, byWords, 1)
}

// EstimateMessagesTokens adds a small per-message overhead for the chat format.
func EstimateMessagesTokens(msgs []ChatMessage) int {
	n := 3
	for _, m := range msgs {
		n += EstimateTokens(m.Content) + 4
	}
	return n
}