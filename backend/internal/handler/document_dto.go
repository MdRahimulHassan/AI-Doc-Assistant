package handler

import (
	"time"

	"github.com/google/uuid"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/models"
)

// DocumentResponse is the API's view of a document. It deliberately has no
// storage_path and no user_id: internal details never leave the server.
type DocumentResponse struct {
	ID           uuid.UUID `json:"id"`
	Filename     string    `json:"filename"`
	MimeType     string    `json:"mime_type"`
	SizeBytes    int64     `json:"size_bytes"`
	Status       string    `json:"status"`
	ErrorMessage *string   `json:"error_message,omitempty"` // only present when status is "failed"
	ChunkCount   int       `json:"chunk_count"`
	CreatedAt    time.Time `json:"created_at"`
}

func toDocumentResponse(d *models.Document) DocumentResponse {
	var msg *string
	if d.Status == models.StatusFailed {
		msg = d.ErrorMessage
	}
	return DocumentResponse{
		ID:           d.ID,
		Filename:     d.Filename,
		MimeType:     d.MimeType,
		SizeBytes:    d.SizeBytes,
		Status:       string(d.Status),
		ErrorMessage: msg,
		ChunkCount:   d.ChunkCount,
		CreatedAt:    d.CreatedAt,
	}
}