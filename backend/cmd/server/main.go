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

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/chunker"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/config"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/database"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/parser"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/repository"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/router"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/service"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/storage"
)

const (
	httpShutdownTimeout = 10 * time.Second // in-flight HTTP requests
	ingestDrainTimeout  = 30 * time.Second // running ingestion jobs
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	if err := run(); err != nil {
		log.Fatalf("fatal: %v", err)
	}
}

// run holds the real logic. Returning an error (instead of calling log.Fatal
// deep inside) lets deferred cleanup like pool.Close() always run.
func run() error {
	// 1. Config
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("invalid configuration:\n%w", err)
	}

	// 2. Database. Closed LAST (deferred first), because running ingestion
	//    jobs still need it while the rest of the app shuts down.
	pool, err := database.NewPool(context.Background(), cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("database connection: %w", err)
	}
	defer pool.Close()
	log.Println("database connected")

	// 3. Migrations
	if err := database.RunMigrations(cfg.DatabaseURL); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	log.Println("migrations applied")

	// 4. File storage
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

	if cfg.LLMEnabled() {
		slog.Info("AI features enabled", "model", cfg.GroqModel, "summary_model", cfg.GroqSummaryModel, "tpm_budget", cfg.GroqTPMBudget)
	} else {
		slog.Warn("GROQ_API_KEY is not set: the AI features (ask, summarize) are disabled")
	}

	// 5. Services
	users := repository.NewUserRepository(pool)
	tokens, err := service.NewTokenManager(cfg.JWTSecret, cfg.JWTLifetime)
	if err != nil {
		return fmt.Errorf("token manager: %w", err)
	}
	authService, err := service.NewAuthService(users, tokens, service.DefaultBcryptCost)
	if err != nil {
		return fmt.Errorf("auth service: %w", err)
	}

	// 6. Ingestion: a bounded pool of workers fed by a bounded queue.
	//    appCtx lives as long as the application. Jobs run with it, never with a
	//    request context (which is cancelled the moment its handler returns).
	appCtx, stopApp := context.WithCancel(context.Background())
	defer stopApp()

	docRepo := repository.NewDocumentRepository(pool)
	ingestSvc := service.NewIngestService(docRepo, files, chunker.Default(), service.DefaultIngestTimeout)

	queue, err := service.NewIngestQueue(appCtx, ingestSvc, cfg.IngestWorkers, cfg.IngestQueueSize)
	if err != nil {
		return fmt.Errorf("ingest queue: %w", err)
	}
	queue.Start()
	slog.Info("ingestion workers started", "workers", cfg.IngestWorkers, "queue_size", cfg.IngestQueueSize)

	// Startup recovery. The list is read BEFORE the server accepts requests, so
	// it holds exactly the documents left over from an earlier run: an upload
	// made later can never appear in it and be processed twice. Queueing them
	// waits for room, so it runs in the background and the server starts at once.
	unfinished, err := docRepo.ListProcessing(context.Background())
	if err != nil {
		return fmt.Errorf("list unfinished documents: %w", err)
	}
	recoveryDone := make(chan struct{})
	go func() {
		defer close(recoveryDone)
		if len(unfinished) == 0 {
			return
		}
		slog.Info("recovering documents left unfinished by an earlier run", "count", len(unfinished))
		queue.EnqueueAll(appCtx, unfinished)
	}()

	docService := service.NewDocumentService(docRepo, files, queue, cfg.MaxUploadBytes)

	// 7. Router and server
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

	// 8. Listen for Ctrl+C / SIGTERM
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serverErr := make(chan error, 1)
	go func() {
		log.Printf("server listening on :%s", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		return fmt.Errorf("server error: %w", err)
	case <-ctx.Done():
		log.Println("shutdown signal received")
	}
	stop() // a second Ctrl+C now force-kills instead of being swallowed

	// 9. Shutdown, in this order:
	//    1) HTTP server: no new uploads; requests in flight finish.
	//    2) Ingestion queue: running jobs finish; queued jobs are not started and
	//       stay 'processing' for the next start.
	//    3) The database pool (deferred above), last.
	httpCtx, cancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
	defer cancel()
	httpErr := srv.Shutdown(httpCtx)
	if httpErr != nil {
		slog.Error("http shutdown failed", "error", httpErr)
	}

	if err := queue.Shutdown(ingestDrainTimeout); err != nil {
		slog.Warn("ingestion did not stop cleanly", "error", err)
	}

	stopApp()
	select {
	case <-recoveryDone:
	case <-time.After(5 * time.Second):
		slog.Warn("startup recovery did not stop in time")
	}

	if httpErr != nil {
		return fmt.Errorf("graceful shutdown failed: %w", httpErr)
	}
	slog.Info("server stopped cleanly")
	return nil
}