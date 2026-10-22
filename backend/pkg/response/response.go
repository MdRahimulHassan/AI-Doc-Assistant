package response

import (
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
)

// Machine-readable error codes. The React app branches on these,
// never on the human-readable message. Add new ones here as features appear.
const (
	CodeBadRequest         = "BAD_REQUEST"
	CodeValidation         = "VALIDATION_ERROR"
	CodeUnauthorized       = "UNAUTHORIZED"
	CodeInvalidCredentials = "INVALID_CREDENTIALS"
	CodeEmailTaken         = "EMAIL_TAKEN"
	CodeNotFound           = "NOT_FOUND"
	CodeMethodNotAllowed   = "METHOD_NOT_ALLOWED"
	CodeInternal           = "INTERNAL_ERROR"
	CodeUnavailable        = "SERVICE_UNAVAILABLE"
	CodeFileTooLarge        = "FILE_TOO_LARGE"
	CodeUnsupportedFileType = "UNSUPPORTED_FILE_TYPE"
	CodeNoFile              = "NO_FILE"
	CodeDocumentNotReady = "DOCUMENT_NOT_READY"
	CodeAnswerInProgress = "ANSWER_IN_PROGRESS"
	CodeRateLimited      = "RATE_LIMITED"
	CodeLLMUnavailable   = "LLM_UNAVAILABLE"
	CodeLLMBusy          = "LLM_BUSY"
	CodeLLMTimeout       = "LLM_TIMEOUT"
	CodeLLMError         = "LLM_ERROR"
	CodeShuttingDown     = "SERVER_SHUTTING_DOWN"
)

// Envelope is the one JSON shape every endpoint returns.
type Envelope struct {
	Success bool       `json:"success"`
	Data    any        `json:"data,omitempty"`
	Error   *ErrorBody `json:"error,omitempty"`
}

type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"` // e.g. per-field validation errors
}

// FieldError describes one invalid input field. It goes in the "details"
// of a VALIDATION_ERROR so the React form can highlight the right input.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// OK sends 200 with the data wrapped in the envelope.
func OK(c *gin.Context, data any) {
	c.JSON(http.StatusOK, Envelope{Success: true, Data: data})
}

func Accepted(c *gin.Context, data any) {
	c.JSON(http.StatusAccepted, Envelope{Success: true, Data: data})
}

// Created sends 201 with the data wrapped in the envelope.
func Created(c *gin.Context, data any) {
	c.JSON(http.StatusCreated, Envelope{Success: true, Data: data})
}

func NoContent(c *gin.Context) {
	c.Status(http.StatusNoContent)
	c.Writer.WriteHeaderNow()
}

// Error sends an error envelope and stops the handler chain.
func Error(c *gin.Context, status int, code, message string) {
	ErrorWithDetails(c, status, code, message, nil)
}

// ErrorWithDetails is Error plus extra structured info (such as which
// form field failed validation).
func ErrorWithDetails(c *gin.Context, status int, code, message string, details any) {
	c.AbortWithStatusJSON(status, Envelope{
		Success: false,
		Error:   &ErrorBody{Code: code, Message: message, Details: details},
	})
}

// InternalError is the single place that enforces rule 4: the real error
// is logged server-side, and the client only sees a generic message.
func InternalError(c *gin.Context, err error) {
	log.Printf("internal error: %s %s: %v", c.Request.Method, c.Request.URL.Path, err)
	Error(c, http.StatusInternalServerError, CodeInternal, "something went wrong")
}