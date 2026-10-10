package middleware

import (
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"
)

// Logger writes one structured log line per request, after it finishes.
func Logger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()

		// Path only, never RawQuery: query strings can carry tokens or emails.
		path := c.Request.URL.Path

		c.Next() // run the rest of the chain, then come back here

		status := c.Writer.Status()

		level := slog.LevelInfo
		switch {
		case status >= 500:
			level = slog.LevelError
		case status >= 400:
			level = slog.LevelWarn
		}

		slog.Log(c.Request.Context(), level, "http request",
			"request_id", RequestIDFrom(c),
			"method", c.Request.Method,
			"path", path,
			"status", status,
			"latency_ms", float64(time.Since(start).Microseconds())/1000,
			"client_ip", c.ClientIP(),
		)
	}
}