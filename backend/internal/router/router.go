package router

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/handler"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/middleware"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/pkg/response"
)

// Limits are optional middleware for the routes that need protecting. A nil or
// empty list means "no limit" (tests rely on that). Each list runs in order,
// after the authentication middleware where the route has one.
type Limits struct {
	Auth   []gin.HandlerFunc // POST /api/auth/register and /api/auth/login (before anyone is signed in)
	Upload []gin.HandlerFunc // POST /api/documents
	LLM    []gin.HandlerFunc // POST /api/documents/:id/ask and /summarize, which share these limits
}

type Deps struct {
	DB             handler.Pinger
	Auth           handler.Authenticator
	Documents      handler.Documents
	Asker          handler.Asker // nil disables the ask route (503)
	Messages       handler.MessageLister
	Summaries      handler.Summarizer // nil disables the summarize route (503)
	Tokens         middleware.TokenParser
	CORSOrigins    []string
	MaxUploadBytes int64
	Limits         Limits
}

// with returns a NEW slice: the route's middleware, then its handler. Appending
// to the shared Limits slices directly could make two routes overwrite each
// other's last element.
func with(mws []gin.HandlerFunc, h gin.HandlerFunc) []gin.HandlerFunc {
	out := make([]gin.HandlerFunc, 0, len(mws)+1)
	out = append(out, mws...)
	return append(out, h)
}

func New(d Deps) *gin.Engine {
	r := gin.New()
	r.HandleMethodNotAllowed = true

	// No proxy is trusted, so c.ClientIP() is the real peer address and a forged
	// X-Forwarded-For cannot dodge the per-IP limits. Behind a reverse proxy,
	// trust ITS address here, or every client will look like the proxy.
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

	// Public: no token needed, so limited by client address. Register and login
	// share one budget per address.
	auth := api.Group("/auth")
	auth.POST("/register", with(d.Limits.Auth, authH.Register)...)
	auth.POST("/login", with(d.Limits.Auth, authH.Login)...)

	// Protected: Auth runs first, then (per route) its limits, then the handler.
	protected := api.Group("")
	protected.Use(middleware.Auth(d.Tokens))
	protected.GET("/me", authH.Me)

	protected.POST("/documents", with(d.Limits.Upload, docsH.Upload)...)
	protected.GET("/documents", docsH.List)
	protected.GET("/documents/:id", docsH.Get)
	protected.DELETE("/documents/:id", docsH.Delete)

	// The AI routes cost real tokens, so they carry the limits. Reading the
	// conversation does not.
	protected.POST("/documents/:id/ask", with(d.Limits.LLM, convH.Ask)...)
	protected.GET("/documents/:id/messages", convH.Messages)
	protected.POST("/documents/:id/summarize", with(d.Limits.LLM, sumH.Summarize)...)

	return r
}