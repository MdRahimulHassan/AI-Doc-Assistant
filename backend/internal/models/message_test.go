package models

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func intp(n int) *int { return &n }

func TestNewMessage_Normalize(t *testing.T) {
	t.Run("removes NUL bytes and invalid UTF-8", func(t *testing.T) {
		got := NewMessage{Role: RoleUser, Content: "a\x00b\xffc"}.Normalize()
		if got.Content != "abc" {
			t.Errorf("content = %q, want abc", got.Content)
		}
	})

	t.Run("an empty status becomes complete", func(t *testing.T) {
		if got := (NewMessage{Role: RoleUser, Content: "x"}).Normalize(); got.Status != MessageComplete {
			t.Errorf("status = %q", got.Status)
		}
		if got := (NewMessage{Role: RoleAssistant, Content: "x", Status: MessagePartial}).Normalize(); got.Status != MessagePartial {
			t.Errorf("an explicit status must be kept, got %q", got.Status)
		}
	})

	t.Run("over-long content is cut at a character boundary", func(t *testing.T) {
		got := NewMessage{Role: RoleUser, Content: strings.Repeat("é", MaxMessageChars+5)}.Normalize()
		if n := utf8.RuneCountInString(got.Content); n != MaxMessageChars || !utf8.ValidString(got.Content) {
			t.Errorf("%d characters, valid %v", n, utf8.ValidString(got.Content))
		}
	})

	t.Run("the model name is tidied and capped", func(t *testing.T) {
		got := NewMessage{Role: RoleAssistant, Content: "x", Model: "  openai/gpt-oss-120b \x00 "}.Normalize()
		if got.Model != "openai/gpt-oss-120b" {
			t.Errorf("model = %q", got.Model)
		}
		long := NewMessage{Role: RoleAssistant, Content: "x", Model: strings.Repeat("m", 500)}.Normalize()
		if utf8.RuneCountInString(long.Model) != maxModelChars {
			t.Errorf("model length = %d", utf8.RuneCountInString(long.Model))
		}
	})

	t.Run("sources are copied, and nil stays nil", func(t *testing.T) {
		src := []int{3, 1}
		got := NewMessage{Role: RoleAssistant, Content: "x", Sources: src}.Normalize()
		src[0] = 99
		if !slices.Equal(got.Sources, []int{3, 1}) {
			t.Errorf("sources share memory with the caller's slice: %v", got.Sources)
		}
		if n := (NewMessage{Role: RoleAssistant, Content: "x"}).Normalize(); n.Sources != nil {
			t.Errorf("nil sources became %#v", n.Sources)
		}
		if e := (NewMessage{Role: RoleAssistant, Content: "x", Sources: []int{}}).Normalize(); e.Sources == nil {
			t.Error("an empty non-nil slice must stay non-nil")
		}
	})
}

func TestNewMessage_Validate(t *testing.T) {
	tests := []struct {
		name string
		m    NewMessage
		ok   bool
	}{
		{"a question", NewMessage{Role: RoleUser, Content: "What is this?"}, true},
		{"a full answer", NewMessage{Role: RoleAssistant, Content: "It is X. [P1]", Sources: []int{3, 1}, Model: "m", PromptTokens: intp(10), CompletionTokens: intp(5)}, true},
		{"an answer with an empty sources list", NewMessage{Role: RoleAssistant, Content: "x", Sources: []int{}}, true},
		{"a partial answer with no text", NewMessage{Role: RoleAssistant, Content: "", Status: MessagePartial}, true},
		{"a failed answer with no text", NewMessage{Role: RoleAssistant, Content: "  ", Status: MessageError}, true},

		{"unknown role", NewMessage{Role: "system", Content: "x"}, false},
		{"no role", NewMessage{Content: "x"}, false},
		{"unknown status", NewMessage{Role: RoleAssistant, Content: "x", Status: "bogus"}, false},
		{"an empty question", NewMessage{Role: RoleUser, Content: ""}, false},
		{"a blank question", NewMessage{Role: RoleUser, Content: " \n\t"}, false},
		{"a complete answer with no text", NewMessage{Role: RoleAssistant, Content: ""}, false},
		{"a question marked partial", NewMessage{Role: RoleUser, Content: "x", Status: MessagePartial}, false},
		{"a question with sources", NewMessage{Role: RoleUser, Content: "x", Sources: []int{1}}, false},
		{"a question with an empty sources list", NewMessage{Role: RoleUser, Content: "x", Sources: []int{}}, false},
		{"a question with a model", NewMessage{Role: RoleUser, Content: "x", Model: "m"}, false},
		{"a question with token counts", NewMessage{Role: RoleUser, Content: "x", PromptTokens: intp(1)}, false},
		{"a negative source index", NewMessage{Role: RoleAssistant, Content: "x", Sources: []int{1, -2}}, false},
		{"too many sources", NewMessage{Role: RoleAssistant, Content: "x", Sources: make([]int, maxSources+1)}, false},
		{"negative prompt tokens", NewMessage{Role: RoleAssistant, Content: "x", PromptTokens: intp(-1)}, false},
		{"negative completion tokens", NewMessage{Role: RoleAssistant, Content: "x", CompletionTokens: intp(-1)}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.m.Normalize().Validate()
			switch {
			case tc.ok && err != nil:
				t.Errorf("unexpected error: %v", err)
			case !tc.ok && !errors.Is(err, ErrInvalidMessage):
				t.Errorf("err = %v, want ErrInvalidMessage", err)
			}
		})
	}
}

func TestMessagePage_OldestID(t *testing.T) {
	if id := (MessagePage{}).OldestID(); id != 0 {
		t.Errorf("an empty page has OldestID %d, want 0", id)
	}
	p := MessagePage{Messages: []Message{{ID: 7}, {ID: 9}, {ID: 12}}}
	if id := p.OldestID(); id != 7 {
		t.Errorf("OldestID = %d, want 7 (messages are oldest first)", id)
	}
}

func TestMessageJSONHidesInternals(t *testing.T) {
	model := "openai/gpt-oss-120b"
	b, err := json.Marshal(Message{
		ID: 1, Role: RoleAssistant, Content: "hello", Status: MessageComplete, Sources: []int{2},
		Model: &model, PromptTokens: intp(1200), CompletionTokens: intp(80), CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	for _, leaked := range []string{"gpt-oss", "model", "prompt_tokens", "completion_tokens", "1200"} {
		if strings.Contains(out, leaked) {
			t.Errorf("JSON leaks %q: %s", leaked, out)
		}
	}
	for _, want := range []string{`"id":1`, `"role":"assistant"`, `"content":"hello"`, `"status":"complete"`, `"sources":[2]`} {
		if !strings.Contains(out, want) {
			t.Errorf("JSON is missing %s: %s", want, out)
		}
	}
}