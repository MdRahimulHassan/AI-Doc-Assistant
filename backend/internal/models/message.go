package models

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// A message longer than this is cut. It matches the messages_content_length
// CHECK in migration 000006 (characters, not bytes). The app's own limits are
// far lower (questions 1000 characters, answers a few thousand), so this is a
// safety net.
const (
	MaxMessageChars = 100000
	maxSources      = 100
	maxModelChars   = 100
)

var ErrInvalidMessage = errors.New("invalid message")

// Conversation is one user's conversation about one document.
type Conversation struct {
	ID         uuid.UUID `json:"id"`
	UserID     uuid.UUID `json:"-"`
	DocumentID uuid.UUID `json:"document_id"`
	CreatedAt  time.Time `json:"created_at"`
}

type MessageRole string

const (
	RoleUser      MessageRole = "user"
	RoleAssistant MessageRole = "assistant"
)

type MessageStatus string

const (
	MessageComplete MessageStatus = "complete"
	MessagePartial  MessageStatus = "partial" // cut short: the client left, the stream broke, or the token limit was hit
	MessageError    MessageStatus = "error"   // failed before any text arrived
)

// Message is a stored question or answer. The model name and token counts are
// internal and never serialized.
type Message struct {
	ID               int64         `json:"id"`
	Role             MessageRole   `json:"role"`
	Content          string        `json:"content"`
	Status           MessageStatus `json:"status"`
	Sources          []int         `json:"sources,omitempty"` // chunk indexes that backed an answer
	Model            *string       `json:"-"`
	PromptTokens     *int          `json:"-"`
	CompletionTokens *int          `json:"-"`
	CreatedAt        time.Time     `json:"created_at"`
}

// MessagePage is one page of a conversation, oldest message first.
type MessagePage struct {
	Messages []Message
	HasMore  bool // older messages exist: ask again with before = OldestID()
}

// OldestID is the cursor for the next (older) page, or 0 for an empty page.
func (p MessagePage) OldestID() int64 {
	if len(p.Messages) == 0 {
		return 0
	}
	return p.Messages[0].ID
}

// NewMessage is what the repository accepts. Call Normalize, then Validate.
type NewMessage struct {
	Role             MessageRole
	Content          string
	Status           MessageStatus // empty means complete
	Sources          []int         // nil means "none recorded"; an empty non-nil slice is stored as []
	Model            string        // empty means unknown
	PromptTokens     *int
	CompletionTokens *int
}

// Normalize returns a cleaned copy, so that harmless problems do not lose a
// message that cost real tokens to produce:
//   - NUL bytes (which Postgres text cannot hold) and invalid UTF-8 are removed
//   - over-long content is cut to MaxMessageChars, at a character boundary
//   - an empty status becomes "complete"
//   - the model name is tidied
func (m NewMessage) Normalize() NewMessage {
	m.Content = strings.ReplaceAll(strings.ToValidUTF8(m.Content, ""), "\x00", "")
	if utf8.RuneCountInString(m.Content) > MaxMessageChars {
		m.Content = string([]rune(m.Content)[:MaxMessageChars])
	}

	if m.Status == "" {
		m.Status = MessageComplete
	}

	m.Model = strings.TrimSpace(strings.ReplaceAll(strings.ToValidUTF8(m.Model, ""), "\x00", ""))
	if utf8.RuneCountInString(m.Model) > maxModelChars {
		m.Model = string([]rune(m.Model)[:maxModelChars])
	}

	if m.Sources != nil {
		m.Sources = append([]int{}, m.Sources...) // a copy: the caller's slice is never shared
	}
	return m
}

// Validate checks the same rules as the CHECK constraints of migration 000006,
// so a mistake becomes a clear error here instead of a database error. It
// expects a normalized message.
func (m NewMessage) Validate() error {
	switch m.Role {
	case RoleUser, RoleAssistant:
	default:
		return fmt.Errorf("%w: unknown role %q", ErrInvalidMessage, m.Role)
	}
	switch m.Status {
	case MessageComplete, MessagePartial, MessageError:
	default:
		return fmt.Errorf("%w: unknown status %q", ErrInvalidMessage, m.Status)
	}

	// Only an assistant message that was cut short or failed may be empty.
	if strings.TrimSpace(m.Content) == "" && !(m.Role == RoleAssistant && m.Status != MessageComplete) {
		return fmt.Errorf("%w: the content is empty", ErrInvalidMessage)
	}

	if m.Role == RoleUser &&
		(m.Status != MessageComplete || m.Sources != nil || m.Model != "" || m.PromptTokens != nil || m.CompletionTokens != nil) {
		return fmt.Errorf("%w: a question carries no status, sources, model or token counts", ErrInvalidMessage)
	}

	if len(m.Sources) > maxSources {
		return fmt.Errorf("%w: too many sources (%d)", ErrInvalidMessage, len(m.Sources))
	}
	for _, s := range m.Sources {
		if s < 0 {
			return fmt.Errorf("%w: a source chunk index is negative", ErrInvalidMessage)
		}
	}
	if (m.PromptTokens != nil && *m.PromptTokens < 0) || (m.CompletionTokens != nil && *m.CompletionTokens < 0) {
		return fmt.Errorf("%w: token counts cannot be negative", ErrInvalidMessage)
	}
	return nil
}