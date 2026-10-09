package main

import (
	"log"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/config"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("invalid configuration:\n%v", err)
	}

	// Never log the whole struct: it contains the JWT secret and DB password.
	log.Printf("config loaded: port=%s cors=%v jwt_ttl=%s",
		cfg.Port, cfg.CORSOrigins, cfg.JWTLifetime)
}