package middleware

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/pkg/response"
)

const userIDKey = "user_id"

// TokenParser is what the middleware needs. *service.TokenManager satisfies
// it, so the middleware uses the exact same validation as token creation
// and never imports the service package.
type TokenParser interface {
	Parse(token string) (uuid.UUID, error)
}

// Auth rejects any request without a valid "Authorization: Bearer <token>".
func Auth(tokens TokenParser) gin.HandlerFunc {
	return func(c *gin.Context) {
		token, ok := bearerToken(c.GetHeader("Authorization"))
		if !ok {
			unauthorized(c, "missing or malformed Authorization header")
			return
		}

		userID, err := tokens.Parse(token)
		if err != nil {
			// The exact reason (expired? tampered?) is for our logs only.
			slog.Debug("token rejected", "request_id", RequestIDFrom(c), "reason", err)
			unauthorized(c, "invalid or expired token")
			return
		}

		c.Set(userIDKey, userID)
		c.Next()
	}
}

// UserIDFrom returns the authenticated user's ID, if the middleware ran.
func UserIDFrom(c *gin.Context) (uuid.UUID, bool) {
	v, ok := c.Get(userIDKey)
	if !ok {
		return uuid.Nil, false
	}
	id, ok := v.(uuid.UUID)
	return id, ok
}

// bearerToken extracts the token from "Bearer <token>".
// The scheme name is case-insensitive per the HTTP spec.
func bearerToken(header string) (string, bool) {
	scheme, token, found := strings.Cut(header, " ")
	token = strings.TrimSpace(token)
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", false
	}
	return token, true
}

func unauthorized(c *gin.Context, message string) {
	// Tells clients which auth scheme is expected (RFC 6750).
	c.Header("WWW-Authenticate", "Bearer")
	// response.Error uses AbortWithStatusJSON, so the chain stops here.
	// That Abort is what keeps the real handler from running.
	response.Error(c, http.StatusUnauthorized, response.CodeUnauthorized, message)
}