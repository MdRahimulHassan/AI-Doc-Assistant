package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/middleware"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/models"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/service"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/pkg/response"
)

const (
	// multipart framing (boundaries, part headers) adds a little on top of the file itself
	multipartSlack = 64 << 10

	defaultListLimit = 20
	maxListLimit     = 100
)

// Documents is what the handlers need from the document service.
type Documents interface {
	Upload(ctx context.Context, userID uuid.UUID, filename string, f service.UploadFile, size int64) (*models.Document, error)
	Get(ctx context.Context, userID, id uuid.UUID) (*models.Document, error)
	List(ctx context.Context, userID uuid.UUID, limit, offset int) ([]models.Document, error)
	Delete(ctx context.Context, userID, id uuid.UUID) error
}

// Compile-time check that the real service satisfies the interface.
var _ Documents = (*service.DocumentService)(nil)

type DocumentHandler struct {
	docs           Documents
	maxUploadBytes int64
}

func NewDocumentHandler(docs Documents, maxUploadBytes int64) *DocumentHandler {
	return &DocumentHandler{docs: docs, maxUploadBytes: maxUploadBytes}
}

// POST /api/documents (multipart/form-data, field "file")
func (h *DocumentHandler) Upload(c *gin.Context) {
	userID, ok := middleware.UserIDFrom(c)
	if !ok {
		response.InternalError(c, errors.New("upload: user id missing from context"))
		return
	}

	limit := h.maxUploadBytes + multipartSlack

	// Cheap early exit when the client announces a body that is too big.
	if c.Request.ContentLength > limit {
		h.fileTooLarge(c)
		return
	}
	// The announced length can be missing or a lie, so the cap is also enforced
	// while reading.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)

	header, err := c.FormFile("file")
	if c.Request.MultipartForm != nil {
		// Files above Gin's MaxMultipartMemory spill to temp files on disk.
		// Deferred BEFORE f.Close below, so it runs after it (Windows cannot
		// delete a file that is still open).
		defer c.Request.MultipartForm.RemoveAll()
	}
	if err != nil {
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(err, &tooBig):
			h.fileTooLarge(c)
		case errors.Is(err, http.ErrMissingFile):
			response.Error(c, http.StatusBadRequest, response.CodeNoFile,
				`no file was uploaded: send it in a form field named "file"`)
		default:
			response.Error(c, http.StatusBadRequest, response.CodeBadRequest,
				`the request must be multipart/form-data with the file in a field named "file"`)
		}
		return
	}

	f, err := header.Open()
	if err != nil {
		response.InternalError(c, fmt.Errorf("open uploaded file: %w", err))
		return
	}
	defer f.Close()

	doc, err := h.docs.Upload(c.Request.Context(), userID, header.Filename, f, header.Size)
	if err != nil {
		h.writeUploadError(c, err)
		return
	}

	// 202: stored and queued, not processed yet. The client polls GET /:id.
	response.Accepted(c, gin.H{"document": toDocumentResponse(doc)})
}

// GET /api/documents?limit=20&offset=0
func (h *DocumentHandler) List(c *gin.Context) {
	userID, ok := middleware.UserIDFrom(c)
	if !ok {
		response.InternalError(c, errors.New("list: user id missing from context"))
		return
	}

	limit, offset, problems := parsePaging(c)
	if len(problems) > 0 {
		response.ErrorWithDetails(c, http.StatusBadRequest, response.CodeValidation, "invalid query parameters", problems)
		return
	}

	docs, err := h.docs.List(c.Request.Context(), userID, limit, offset)
	if err != nil {
		response.InternalError(c, err)
		return
	}

	out := make([]DocumentResponse, len(docs)) // never nil, so it encodes as [] and not null
	for i := range docs {
		out[i] = toDocumentResponse(&docs[i])
		out[i].Summary = nil // the list shows summary_status only; read a document to get its summary
	}
	response.OK(c, gin.H{"documents": out, "limit": limit, "offset": offset})
}

// GET /api/documents/:id
func (h *DocumentHandler) Get(c *gin.Context) {
	userID, ok := middleware.UserIDFrom(c)
	if !ok {
		response.InternalError(c, errors.New("get: user id missing from context"))
		return
	}
	id, ok := documentID(c)
	if !ok {
		return
	}

	doc, err := h.docs.Get(c.Request.Context(), userID, id)
	if err != nil {
		writeDocumentError(c, err)
		return
	}
	response.OK(c, gin.H{"document": toDocumentResponse(doc)})
}

// DELETE /api/documents/:id
func (h *DocumentHandler) Delete(c *gin.Context) {
	userID, ok := middleware.UserIDFrom(c)
	if !ok {
		response.InternalError(c, errors.New("delete: user id missing from context"))
		return
	}
	id, ok := documentID(c)
	if !ok {
		return
	}

	if err := h.docs.Delete(c.Request.Context(), userID, id); err != nil {
		writeDocumentError(c, err)
		return
	}
	response.NoContent(c)
}

// documentID parses :id. A malformed ID is simply "no such document": 404, not 500.
func documentID(c *gin.Context) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Error(c, http.StatusNotFound, response.CodeNotFound, "document not found")
		return uuid.Nil, false
	}
	return id, true
}

// parsePaging validates limit and offset. A missing limit means 20; a limit
// above 100 is clamped to 100 (the response echoes the value actually used).
func parsePaging(c *gin.Context) (limit, offset int, problems []response.FieldError) {
	limit, offset = defaultListLimit, 0

	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			problems = append(problems, response.FieldError{Field: "limit", Message: "must be a whole number of at least 1"})
		} else {
			limit = min(n, maxListLimit)
		}
	}
	if raw := c.Query("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			problems = append(problems, response.FieldError{Field: "offset", Message: "must be a whole number of at least 0"})
		} else {
			offset = n
		}
	}
	return limit, offset, problems
}

func (h *DocumentHandler) fileTooLarge(c *gin.Context) {
	response.Error(c, http.StatusRequestEntityTooLarge, response.CodeFileTooLarge,
		fmt.Sprintf("the file is too large (the maximum is %s)", humanSize(h.maxUploadBytes)))
}

// writeUploadError is where upload errors become HTTP responses. Anything not
// listed is a 500: it is logged, and the client sees a generic message.
func (h *DocumentHandler) writeUploadError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, models.ErrFileTooLarge):
		h.fileTooLarge(c)
	case errors.Is(err, service.ErrEmptyFile):
		response.Error(c, http.StatusBadRequest, response.CodeNoFile, "the uploaded file is empty")
	case errors.Is(err, service.ErrUnsupportedFileType):
		response.Error(c, http.StatusUnsupportedMediaType, response.CodeUnsupportedFileType,
			"unsupported file type: upload a PDF, a DOCX or a UTF-8 text (.txt) file, and make sure the extension matches the content")
	case errors.Is(err, service.ErrBusy):
		c.Header("Retry-After", "30")
		response.Error(c, http.StatusServiceUnavailable, response.CodeUnavailable,
			"the server is busy processing documents, please try again shortly")
	default:
		response.InternalError(c, err)
	}
}

func writeDocumentError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, models.ErrNotFound):
		// A document that does not exist and one that belongs to someone else
		// look exactly the same.
		response.Error(c, http.StatusNotFound, response.CodeNotFound, "document not found")
	default:
		response.InternalError(c, err)
	}
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%d MB", n>>20)
	case n >= 1<<10 && n%(1<<10) == 0:
		return fmt.Sprintf("%d KB", n>>10)
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}