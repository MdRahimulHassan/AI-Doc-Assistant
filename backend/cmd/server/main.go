package main

import (
	"context"
	"log"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/config"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/database"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("invalid configuration:\n%v", err)
	}

	ctx := context.Background()

	pool, err := database.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("database connection failed: %v", err)
	}
	defer pool.Close()
	log.Println("database connected")

	if err := database.RunMigrations(cfg.DatabaseURL); err != nil {
		log.Fatalf("migrations failed: %v", err)
	}
	log.Println("migrations applied")

	log.Printf("config loaded: port=%s cors=%v jwt_ttl=%s",
		cfg.Port, cfg.CORSOrigins, cfg.JWTLifetime)
}