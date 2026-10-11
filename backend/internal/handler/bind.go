package handler

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/pkg/response"
)

// Auth bodies are tiny. A hard cap stops someone from streaming megabytes
// at an endpoint that anyone on the internet can call.
const maxBodyBytes = 8 << 10

// bindJSON parses the body into dst. On failure it writes the error
// response itself and returns false, so callers just do:
//
//	if !bindJSON(c, &req) { return }
func bindJSON(c *gin.Context, dst any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)

	err := c.ShouldBindJSON(dst)
	if err == nil {
		return true
	}

	var verrs validator.ValidationErrors
	if errors.As(err, &verrs) {
		details := make([]response.FieldError, 0, len(verrs))
		for _, fe := range verrs {
			details = append(details, response.FieldError{
				// Our JSON names are single lowercase words, so lowercasing the
				// struct field name ("Email" -> "email") gives the JSON name.
				Field:   strings.ToLower(fe.Field()),
				Message: fieldMessage(fe),
			})
		}
		response.ErrorWithDetails(c, http.StatusBadRequest, response.CodeValidation, "invalid input", details)
		return false
	}

	// Broken JSON, wrong types, an empty body, or a body over the size limit.
	// The parser's own message is deliberately not forwarded to the client.
	response.Error(c, http.StatusBadRequest, response.CodeBadRequest, "request body must be valid JSON")
	return false
}

func fieldMessage(fe validator.FieldError) string {
	if fe.Tag() == "required" {
		return "this field is required"
	}
	return "this field is invalid"
}