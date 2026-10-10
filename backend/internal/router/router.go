package router

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/handler"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/middleware"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/pkg/response"
)

type Deps struct {
	DB          handler.Pinger
	CORSOrigins []string
}

func New(d Deps) *gin.Engine {
	r := gin.New()
	r.HandleMethodNotAllowed = true

	// No reverse proxy yet, so don't trust X-Forwarded-For. Otherwise any
	// client can fake its IP in your logs. Revisit when deploying behind
	// nginx/Caddy.
	_ = r.SetTrustedProxies(nil)

	r.Use(
		middleware.RequestID(),
		middleware.Logger(),
		gin.CustomRecovery(func(c *gin.Context, _ any) {
			// Gin already logged the panic and stack trace.
			response.Error(c, http.StatusInternalServerError, response.CodeInternal, "something went wrong")
		}),
		middleware.CORS(d.CORSOrigins),
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
