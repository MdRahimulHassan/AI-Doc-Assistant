package router

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/handler"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/middleware"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/pkg/response"
)

type Deps struct {
	DB             handler.Pinger
	Auth           handler.Authenticator
	Documents      handler.Documents
	Asker          handler.Asker         // nil disables the ask route (503)
	Messages       handler.MessageLister
	Tokens         middleware.TokenParser
	CORSOrigins    []string
	MaxUploadBytes int64
	Summaries      handler.Summarizer // nil disables the summarize route (503)
}

func New(d Deps) *gin.Engine {
	r := gin.New()
	r.HandleMethodNotAllowed = true
	_ = r.SetTrustedProxies(nil)

	// Gin keeps up to 32 MiB of a multipart upload in RAM by default. With 1 MiB,
	// bigger files spill to temporary files on disk instead.
	r.MaxMultipartMemory = 1 << 20

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
	docsH := handler.NewDocumentHandler(d.Documents, d.MaxUploadBytes)
	convH := handler.NewConversationHandler(d.Asker, d.Messages)
	sumH := handler.NewSummaryHandler(d.Summaries)

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

	protected.POST("/documents", docsH.Upload)
	protected.GET("/documents", docsH.List)
	protected.GET("/documents/:id", docsH.Get)
	protected.DELETE("/documents/:id", docsH.Delete)
	protected.POST("/documents/:id/ask", convH.Ask)
	protected.GET("/documents/:id/messages", convH.Messages)
		protected.POST("/documents/:id/summarize", sumH.Summarize)

	return r
}