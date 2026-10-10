package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	RequestIDHeader = "X-Request-ID"
	requestIDKey    = "request_id"
)

// RequestID gives every request a unique ID, stores it in the Gin context
// for other middleware, and returns it to the client in a response header.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := newRequestID()
		c.Set(requestIDKey, id)
		c.Header(RequestIDHeader, id)
		c.Next()
	}
}

// RequestIDFrom returns the current request's ID ("" if middleware didn't run).
func RequestIDFrom(c *gin.Context) string {
	return c.GetString(requestIDKey)
}

func newRequestID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("t%d", time.Now().UnixNano()) // extremely unlikely fallback
	}
	return hex.EncodeToString(b)
}