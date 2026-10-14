package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/config"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/database"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/router"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/repository"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/service"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/storage"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/parser"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/chunker"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	if err := run(); err != nil {
		log.Fatalf("fatal: %v", err)
	}
}

// run holds the real logic. Returning an error (instead of calling
// log.Fatal deep inside) lets deferred cleanup like pool.Close() always run.
func run() error {
	// 1. Config
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("invalid configuration:\n%w", err)
	}

	// 2. Database
	pool, err := database.NewPool(context.Background(), cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("database connection: %w", err)
	}
	defer pool.Close() // runs LAST, after the server has drained
	log.Println("database connected")

	// 3. Migrations
	if err := database.RunMigrations(cfg.DatabaseURL); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	log.Println("migrations applied")

	files, err := storage.NewLocal(cfg.UploadDir)
	if err != nil {
		return fmt.Errorf("file storage: %w", err)
	}
	// Safe at startup: nothing is being uploaded yet.
	if n, err := files.CleanTemp(time.Hour); err != nil {
		slog.Warn("temp upload cleanup failed", "error", err)
	} else if n > 0 {
		slog.Info("removed stale temp uploads", "count", n)
	}
	log.Printf("upload directory ready: %s", cfg.UploadDir)
	
	if parser.PDFBackend() == "go" {
		slog.Warn("pdftotext not found: using the built-in PDF extractor, which can glue words together on some PDFs (install poppler)")
	} else {
		slog.Info("pdf extractor ready", "backend", parser.PDFBackend())
	}

	// 4. Router and server
	users := repository.NewUserRepository(pool)

	tokens, err := service.NewTokenManager(cfg.JWTSecret, cfg.JWTLifetime)
	if err != nil {
		return fmt.Errorf("token manager: %w", err)
	}

	authService, err := service.NewAuthService(users, tokens, service.DefaultBcryptCost)
	if err != nil {
		return fmt.Errorf("auth service: %w", err)
	}

	// Documents. Ingestion runs inline (inside the upload request) for now;
	// Step 8 replaces the inline ingester with a bounded background worker pool.
	docRepo := repository.NewDocumentRepository(pool)
	ingestSvc := service.NewIngestService(docRepo, files, chunker.Default(), service.DefaultIngestTimeout)
	docService := service.NewDocumentService(docRepo, files, service.NewInlineIngester(ingestSvc), cfg.MaxUploadBytes)

	r := router.New(router.Deps{
		DB:             pool,
		Auth:           authService,
		Documents:      docService,
		Tokens:         tokens,
		CORSOrigins:    cfg.CORSOrigins,
		MaxUploadBytes: cfg.MaxUploadBytes,
	})

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// 5. Listen for Ctrl+C / SIGTERM
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 6. Run the server in a goroutine so main can wait for a signal
	serverErr := make(chan error, 1)
	go func() {
		log.Printf("server listening on :%s", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	// 7. Block until the server fails or a signal arrives
	select {
	case err := <-serverErr:
		return fmt.Errorf("server error: %w", err)
	case <-ctx.Done():
		log.Println("shutdown signal received")
	}

	// A second Ctrl+C now force-kills instead of being swallowed.
	stop()

	// 8. Graceful shutdown: stop accepting, let in-flight requests finish
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown failed: %w", err)
	}

	log.Println("server stopped cleanly")
	return nil
}