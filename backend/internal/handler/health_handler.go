package handler

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/pkg/response"
)

// Pinger is the only thing the health check needs from the database.
// *pgxpool.Pool satisfies it, and tests can pass a fake.
type Pinger interface {
	Ping(ctx context.Context) error
}

type HealthHandler struct {
	db Pinger
}

func NewHealthHandler(db Pinger) *HealthHandler {
	return &HealthHandler{db: db}
}

func (h *HealthHandler) Check(c *gin.Context) {
	// Derive from the request context so a disconnected client cancels the ping.
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()

	if err := h.db.Ping(ctx); err != nil {
		// Log the detail server-side; don't expose it to callers.
		log.Printf("health check: database ping failed: %v", err)
		
		response.Error(c, http.StatusServiceUnavailable, response.CodeUnavailable, "database unavailable")
		return
	}

	response.OK(c, gin.H{
		"status":   "ok",
		"database": "up",
	})
}