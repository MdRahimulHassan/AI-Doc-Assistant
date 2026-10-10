package router

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/handler"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/pkg/response"
)

type Deps struct {
	DB handler.Pinger
}

func New(d Deps) *gin.Engine {
	r := gin.New()

	// Without this, a wrong method on a known path (POST /health) is
	// reported as 404 instead of 405.
	r.HandleMethodNotAllowed = true

	r.Use(
		// Recovery goes first so it also catches panics in later middleware.
		gin.CustomRecovery(func(c *gin.Context, _ any) {
			// Gin has already logged the panic and stack trace by this point.
			response.Error(c, http.StatusInternalServerError, response.CodeInternal, "something went wrong")
		}),
		gin.Logger(), // temporary, replaced in Step 7
	)

	r.NoRoute(func(c *gin.Context) {
		response.Error(c, http.StatusNotFound, response.CodeNotFound, "route not found")
	})
	r.NoMethod(func(c *gin.Context) {
		response.Error(c, http.StatusMethodNotAllowed, response.CodeMethodNotAllowed, "method not allowed")
	})

	health := handler.NewHealthHandler(d.DB)
	r.GET("/health", health.Check)

	return r
}
