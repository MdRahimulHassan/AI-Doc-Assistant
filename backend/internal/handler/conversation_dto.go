package handler

import (
	"time"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/models"
)

type AskRequest struct {
	Question string `json:"question" binding:"required"`
}

// The wire format of the SSE events. Documented in the README.
type sourcePayload struct {
	Label      string `json:"label"`
	ChunkIndex int    `json:"chunk_index"`
	Preview    string `json:"preview"`
	Truncated  bool   `json:"truncated,omitempty"`
}

type sourcesPayload struct {
	Sources []sourcePayload `json:"sources"`
}

type tokenPayload struct {
	Text string `json:"text"`
}

type donePayload struct {
	MessageID    int64  `json:"message_id"`
	Status       string `json:"status"`
	FinishReason string `json:"finish_reason,omitempty"`
}

type errorPayload struct {
	Code              string `json:"code"`
	Message           string `json:"message"`
	RetryAfterSeconds int    `json:"retry_after_seconds,omitempty"`
	MessageID         int64  `json:"message_id,omitempty"`
}

// MessageResponse is the API's view of a stored message. Model and token counts
// stay internal.
type MessageResponse struct {
	ID        int64     `json:"id"`
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	Status    string    `json:"status"`
	Sources   []int     `json:"sources,omitempty"` // chunk indexes behind an answer
	CreatedAt time.Time `json:"created_at"`
}

func toMessageResponse(m *models.Message) MessageResponse {
	return MessageResponse{
		ID: m.ID, Role: string(m.Role), Content: m.Content, Status: string(m.Status),
		Sources: m.Sources, CreatedAt: m.CreatedAt,
	}
}