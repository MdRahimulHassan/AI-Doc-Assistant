package middleware

import (
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
)

// CORS allows browsers to call the API only from the listed origins.
func CORS(allowedOrigins []string) gin.HandlerFunc {
	return cors.New(cors.Config{
		AllowOrigins:     allowedOrigins,
		AllowMethods:     []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Authorization", "Content-Type"},
		ExposeHeaders:    []string{RequestIDHeader, "Retry-After"}, // lets frontend JS read it
		AllowCredentials: false,
		MaxAge:           12 * time.Hour, // browsers cache the preflight answer
	})
}
