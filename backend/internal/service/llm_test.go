package service

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestEstimateTokens(t *testing.T) {
	english := strings.Repeat("The zebrafish is a small freshwater fish. ", 20) // 140 words
	bengali := strings.Repeat("আমি বই পড়ি এবং তুমি গান শোনো। ", 20)           // 140 words

	if got := EstimateTokens(""); got != 0 {
		t.Errorf("empty text = %d, want 0", got)
	}
	if got := EstimateTokens("x"); got != 1 {
		t.Errorf("one character = %d, want 1", got)
	}

	e, b := EstimateTokens(english), EstimateTokens(bengali)
	if e < 140 || e > 260 {
		t.Errorf("140 English words estimated at %d tokens, want roughly 140-260", e)
	}
	if b <= e {
		t.Errorf("the same number of Bengali words (%d) must estimate higher than English (%d)", b, e)
	}
	if more := EstimateTokens(english + english); more <= e {
		t.Errorf("longer text must never estimate lower: %d vs %d", more, e)
	}
}

func TestEstimateMessagesTokens(t *testing.T) {
	msgs := []ChatMessage{{Role: "system", Content: "Be brief."}, {Role: "user", Content: "Hello there"}}
	want := 3 + EstimateTokens("Be brief.") + 4 + EstimateTokens("Hello there") + 4
	if got := EstimateMessagesTokens(msgs); got != want {
		t.Errorf("EstimateMessagesTokens = %d, want %d", got, want)
	}
}

func TestLLMError(t *testing.T) {
	cause := errors.New("boom")
	e := &LLMError{Kind: LLMBusy, Status: 429, Code: "rate_limit_exceeded", Message: "slow down", RetryAfter: 3 * time.Second, Err: cause}

	s := e.Error()
	for _, want := range []string{"busy", "429", "rate_limit_exceeded", "slow down", "3s"} {
		if !strings.Contains(s, want) {
			t.Errorf("%q does not mention %q", s, want)
		}
	}

	wrapped := fmt.Errorf("while asking: %w", e)
	var target *LLMError
	if !errors.As(wrapped, &target) || target != e {
		t.Error("errors.As cannot find the LLMError")
	}
	if !errors.Is(wrapped, cause) {
		t.Error("Unwrap does not expose the cause")
	}
}