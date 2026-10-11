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
	Auth        handler.Authenticator
	Tokens      middleware.TokenParser
	CORSOrigins []string
}

func New(d Deps) *gin.Engine {
	r := gin.New()
	r.HandleMethodNotAllowed = true
	_ = r.SetTrustedProxies(nil)

	r.Use(
		middleware.RequestID(),
		middleware.Logger(),
		gin.CustomRecovery(func(c *gin.Context, _ any) {
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

	// --- Routes ---
	health := handler.NewHealthHandler(d.DB)
	authH := handler.NewAuthHandler(d.Auth)

	r.GET("/health", health.Check)

	api := r.Group("/api")

	// Public: no token needed.
	auth := api.Group("/auth")
	auth.POST("/register", authH.Register)
	auth.POST("/login", authH.Login)

	// Protected: the middleware is attached to THIS group only.
	protected := api.Group("")
	protected.Use(middleware.Auth(d.Tokens))
	protected.GET("/me", authH.Me)
	// Week 2's document routes will be added to `protected` as well.

	return r
}