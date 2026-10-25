package handler

import (
	"time"

	"github.com/google/uuid"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/models"
)

// DocumentResponse is the API's view of a document. It deliberately has no
// storage_path and no user_id: internal details never leave the server.
//
// summary is the latest FINISHED summary. While a new one is being written
// (summary_status "processing") or after a failed attempt ("failed") it still
// holds the previous text, if there was one.
type DocumentResponse struct {
	ID               uuid.UUID  `json:"id"`
	Filename         string     `json:"filename"`
	MimeType         string     `json:"mime_type"`
	SizeBytes        int64      `json:"size_bytes"`
	Status           string     `json:"status"`
	ErrorMessage     *string    `json:"error_message,omitempty"` // only present when status is "failed"
	ChunkCount       int        `json:"chunk_count"`
	CreatedAt        time.Time  `json:"created_at"`
	Summary          *string    `json:"summary,omitempty"`
	SummaryStatus    string     `json:"summary_status"` // none, processing, ready or failed
	SummaryError     *string    `json:"summary_error,omitempty"`
	SummaryUpdatedAt *time.Time `json:"summary_updated_at,omitempty"`
}

func toDocumentResponse(d *models.Document) DocumentResponse {
	var msg, sumErr *string
	if d.Status == models.StatusFailed {
		msg = d.ErrorMessage
	}
	if d.SummaryStatus == models.SummaryFailed {
		sumErr = d.SummaryError
	}
	sumStatus := string(d.SummaryStatus)
	if sumStatus == "" {
		sumStatus = string(models.SummaryNone)
	}
	return DocumentResponse{
		ID:               d.ID,
		Filename:         d.Filename,
		MimeType:         d.MimeType,
		SizeBytes:        d.SizeBytes,
		Status:           string(d.Status),
		ErrorMessage:     msg,
		ChunkCount:       d.ChunkCount,
		CreatedAt:        d.CreatedAt,
		Summary:          d.Summary,
		SummaryStatus:    sumStatus,
		SummaryError:     sumErr,
		SummaryUpdatedAt: d.SummaryUpdatedAt,
	}
}