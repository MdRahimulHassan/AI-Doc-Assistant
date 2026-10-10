package router

import (

	"github.com/gin-gonic/gin"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/handler"
)

// Deps holds everything the routes need. It grows as you add features
// (config, services, token manager...) without changing New's signature.
type Deps struct {
	DB handler.Pinger
}

func New(d Deps) *gin.Engine {
	r := gin.New()

	// Temporary: Gin's default logger. You'll replace it with your own in Step 7.
	r.Use(gin.Recovery(), gin.Logger())

	health := handler.NewHealthHandler(d.DB)
	r.GET("/health", health.Check)

	return r
}
