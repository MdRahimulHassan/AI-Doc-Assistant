package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/middleware"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/models"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/service"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/pkg/response"
)

const (
	defaultHeartbeat    = 15 * time.Second
	defaultMessageLimit = 50
	maxMessageLimit     = 100
)

// Asker answers a question, reporting progress through emit. *service.AskService
// satisfies it.
type Asker interface {
	Ask(ctx context.Context, req service.AskRequest, emit func(service.AskEvent) error) error
}

// MessageLister returns a page of a conversation. *service.ConversationService
// satisfies it.
type MessageLister interface {
	Messages(ctx context.Context, userID, documentID uuid.UUID, limit int, before int64) (*models.MessagePage, error)
}

var (
	_ Asker         = (*service.AskService)(nil)
	_ MessageLister = (*service.ConversationService)(nil)
)

type ConversationHandler struct {
	ask       Asker
	msgs      MessageLister
	heartbeat time.Duration
}

// NewConversationHandler builds the handler. A nil Asker means the AI features
// are not configured: the ask route answers 503.
func NewConversationHandler(ask Asker, msgs MessageLister) *ConversationHandler {
	return &ConversationHandler{ask: ask, msgs: msgs, heartbeat: defaultHeartbeat}
}

// WithHeartbeat changes how often a quiet stream sends ": ping" (tests use a
// short interval).
func (h *ConversationHandler) WithHeartbeat(d time.Duration) *ConversationHandler {
	h.heartbeat = d
	return h
}

// POST /api/documents/:id/ask   {"question": "..."}
//
// Everything that can fail BEFORE streaming returns a normal JSON error with a
// proper status. Once the first event is sent the status is 200 for good, and a
// failure becomes an "error" event.
func (h *ConversationHandler) Ask(c *gin.Context) {
	userID, ok := middleware.UserIDFrom(c)
	if !ok {
		response.InternalError(c, errors.New("ask: user id missing from context"))
		return
	}
	docID, ok := documentID(c)
	if !ok {
		return
	}
	if h.ask == nil {
		response.Error(c, http.StatusServiceUnavailable, response.CodeLLMUnavailable,
			"the AI features are not available on this server")
		return
	}

	var body AskRequest
	if !bindJSON(c, &body) {
		return
	}

	stream := newSSEStream(c.Writer, h.heartbeat)
	defer stream.Close()

	err := h.ask.Ask(c.Request.Context(), service.AskRequest{
		UserID:     userID,
		DocumentID: docID,
		Question:   body.Question,
		RequestID:  middleware.RequestIDFrom(c),
	}, func(ev service.AskEvent) error { return sendAskEvent(stream, ev) })
	if err == nil {
		return
	}

	if !stream.Started() {
		writeAskError(c, err)
		return
	}
	// Not expected (the service reports failures as events once streaming has
	// begun), but never leave a started stream without an ending.
	slog.Error("ask failed after streaming began", "request_id", middleware.RequestIDFrom(c), "error", err)
	_ = stream.Event("error", errorPayload{Code: service.AskCodeInternal, Message: "Something went wrong while answering."})
}

// GET /api/documents/:id/messages?limit=50&before=123
//
// The newest page first, returned oldest message first. While has_more is true,
// pass next_before as "before" to get the next older page.
func (h *ConversationHandler) Messages(c *gin.Context) {
	userID, ok := middleware.UserIDFrom(c)
	if !ok {
		response.InternalError(c, errors.New("messages: user id missing from context"))
		return
	}
	docID, ok := documentID(c)
	if !ok {
		return
	}
	limit, before, problems := parseMessagePaging(c)
	if len(problems) > 0 {
		response.ErrorWithDetails(c, http.StatusBadRequest, response.CodeValidation, "invalid query parameters", problems)
		return
	}
	if h.msgs == nil {
		response.Error(c, http.StatusServiceUnavailable, response.CodeUnavailable, "messages are not available on this server")
		return
	}

	page, err := h.msgs.Messages(c.Request.Context(), userID, docID, limit, before)
	if err != nil {
		writeDocumentError(c, err) // ErrNotFound -> 404, anything else -> 500
		return
	}

	out := make([]MessageResponse, len(page.Messages)) // never nil: encodes as []
	for i := range page.Messages {
		out[i] = toMessageResponse(&page.Messages[i])
	}
	data := gin.H{"messages": out, "has_more": page.HasMore, "limit": limit}
	if page.HasMore {
		data["next_before"] = page.OldestID()
	}
	response.OK(c, data)
}

// sendAskEvent turns a service event into its SSE form.
func sendAskEvent(s *sseStream, ev service.AskEvent) error {
	switch ev.Kind {
	case service.EventSources:
		p := sourcesPayload{Sources: make([]sourcePayload, 0, len(ev.Sources))}
		for _, src := range ev.Sources {
			p.Sources = append(p.Sources, sourcePayload{
				Label: src.Label, ChunkIndex: src.ChunkIndex, Preview: src.Preview, Truncated: src.Truncated,
			})
		}
		return s.Event("sources", p)
	case service.EventToken:
		return s.Event("token", tokenPayload{Text: ev.Text})
	case service.EventDone:
		return s.Event("done", donePayload{MessageID: ev.MessageID, Status: string(ev.Status), FinishReason: ev.FinishReason})
	case service.EventError:
		return s.Event("error", errorPayload{
			Code: ev.Code, Message: ev.Message,
			RetryAfterSeconds: retryAfterSeconds(ev.RetryAfter), MessageID: ev.MessageID,
		})
	default:
		return fmt.Errorf("unknown ask event kind %d", ev.Kind)
	}
}

// writeAskError maps errors that happen BEFORE streaming to HTTP responses.
// Anything not listed is a 500 with a generic message (the real error is logged).
func writeAskError(c *gin.Context, err error) {
	var notReady *service.DocumentNotReadyError
	var limited *service.RateLimitError

	switch {
	case errors.Is(err, service.ErrEmptyQuestion):
		validationError(c, "the question must not be empty")
	case errors.Is(err, service.ErrQuestionTooLong):
		validationError(c, "the question is too long")
	case errors.Is(err, models.ErrNotFound):
		// Missing and someone else's look exactly the same.
		response.Error(c, http.StatusNotFound, response.CodeNotFound, "document not found")
	case errors.As(err, &notReady):
		response.ErrorWithDetails(c, http.StatusConflict, response.CodeDocumentNotReady,
			"this document is not ready for questions yet", gin.H{"status": string(notReady.Status)})
	case errors.Is(err, service.ErrAnswerInProgress):
		response.Error(c, http.StatusConflict, response.CodeAnswerInProgress,
			"an earlier question about this document is still being answered")
	case errors.As(err, &limited):
		if s := retryAfterSeconds(limited.RetryAfter); s > 0 {
			c.Header("Retry-After", strconv.Itoa(s))
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

func validationError(c *gin.Context, message string) {
	response.ErrorWithDetails(c, http.StatusBadRequest, response.CodeValidation, "invalid input",
		[]response.FieldError{{Field: "question", Message: message}})
}

// retryAfterSeconds rounds up, and is at least 1 for any positive wait.
func retryAfterSeconds(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	return max(1, int(math.Ceil(d.Seconds())))
}

// parseMessagePaging validates limit (default 50, at most 100) and before (a
// message id; 0 or missing means "from the newest").
func parseMessagePaging(c *gin.Context) (limit int, before int64, problems []response.FieldError) {
	limit = defaultMessageLimit

	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			problems = append(problems, response.FieldError{Field: "limit", Message: "must be a whole number of at least 1"})
		} else {
			limit = min(n, maxMessageLimit)
		}
	}
	if raw := c.Query("before"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			problems = append(problems, response.FieldError{Field: "before", Message: "must be a whole number of at least 0"})
		} else {
			before = n
		}
	}
	return limit, before, problems
}