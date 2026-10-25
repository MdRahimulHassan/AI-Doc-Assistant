package handler

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/middleware"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/models"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/service"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/pkg/response"
)

// Summarizer starts summaries. *service.SummaryService satisfies it.
type Summarizer interface {
	Request(ctx context.Context, userID, docID uuid.UUID) (*models.Document, error)
}

var _ Summarizer = (*service.SummaryService)(nil)

type SummaryHandler struct{ svc Summarizer }

// NewSummaryHandler builds the handler. A nil Summarizer means the AI features
// are not configured: the route answers 503.
func NewSummaryHandler(svc Summarizer) *SummaryHandler { return &SummaryHandler{svc: svc} }

// POST /api/documents/:id/summarize
//
// 202: the job is queued and the document is "processing". Poll
// GET /api/documents/:id until summary_status is "ready" or "failed".
func (h *SummaryHandler) Summarize(c *gin.Context) {
	userID, ok := middleware.UserIDFrom(c)
	if !ok {
		response.InternalError(c, errors.New("summarize: user id missing from context"))
		return
	}
	docID, ok := documentID(c)
	if !ok {
		return
	}
	if h.svc == nil {
		response.Error(c, http.StatusServiceUnavailable, response.CodeLLMUnavailable,
			"the AI features are not available on this server")
		return
	}

	doc, err := h.svc.Request(c.Request.Context(), userID, docID)
	if err != nil {
		writeSummaryError(c, err)
		return
	}
	response.Accepted(c, gin.H{"document": toDocumentResponse(doc)})
}

// writeSummaryError maps errors to HTTP responses. Anything not listed is a 500
// with a generic message (the real error is logged).
func writeSummaryError(c *gin.Context, err error) {
	var notReady *service.DocumentNotReadyError
	var limited *service.RateLimitError

	switch {
	case errors.Is(err, models.ErrNotFound):
		response.Error(c, http.StatusNotFound, response.CodeNotFound, "document not found")
	case errors.As(err, &notReady):
		response.ErrorWithDetails(c, http.StatusConflict, response.CodeDocumentNotReady,
			"this document is not ready yet", gin.H{"status": string(notReady.Status)})
	case errors.Is(err, models.ErrSummaryInProgress):
		response.Error(c, http.StatusConflict, response.CodeSummaryInProgress,
			"a summary of this document is already being written")
	case errors.Is(err, models.ErrDocumentTooLong):
		response.Error(c, http.StatusUnprocessableEntity, response.CodeDocumentTooLong,
			"this document is too long to summarize")
	case errors.Is(err, service.ErrBusy):
		c.Header("Retry-After", "30")
		response.Error(c, http.StatusServiceUnavailable, response.CodeUnavailable,
			"the server is busy writing other summaries, please try again shortly")
	case errors.As(err, &limited):
		if s := retryAfterSeconds(limited.RetryAfter); s > 0 {
			c.Header("Retry-After", itoa(s))
		}
		response.Error(c, http.StatusTooManyRequests, response.CodeRateLimited,
			"you are asking too fast, please wait a moment")
	case errors.Is(err, service.ErrLLMUnavailable):
		response.Error(c, http.StatusServiceUnavailable, response.CodeLLMUnavailable,
			"the AI features are not available on this server")
	default:
		response.InternalError(c, err)
	}
}
func itoa(n int) string { return strconv.Itoa(n) }