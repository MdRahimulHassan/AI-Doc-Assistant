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
	"sync"
	"syscall"
	"time"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/chunker"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/config"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/database"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/llm"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/middleware"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/parser"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/ratelimit"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/repository"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/router"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/service"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/storage"
	"github.com/gin-gonic/gin"
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

	// 7b. Questions and answers.
	chunkRepo := repository.NewChunkRepository(pool)
	retrieval, err := service.NewRetrievalService(chunkRepo, cfg.AskTopK)
	if err != nil {
		return fmt.Errorf("retrieval service: %w", err)
	}
	conversations := service.NewConversationService(repository.NewConversationRepository(pool))

	// The chat client exists only if there is an API key. Assigning a nil
	// *llm.Client to the interface would make it non-nil, so it is assigned
	// only inside the if.
	var chat service.ChatClient
	if cfg.LLMEnabled() {
		client, err := llm.New(llm.Config{
			APIKey:        cfg.GroqAPIKey,
			TPMBudget:     cfg.GroqTPMBudget,
			MaxConcurrent: cfg.LLMGlobalConcurrency,
		})
		if err != nil {
			return fmt.Errorf("groq client: %w", err)
		}
		defer client.Close()
		chat = client
	}

	// Cancelled when the server starts to stop, so open answer streams end at
	// once (and are saved as partial) instead of holding up the shutdown.
	asksCtx, stopAsks := context.WithCancel(context.Background())
	defer stopAsks()

	askService, err := service.NewAskService(service.AskDeps{
		Documents:     docService,
		Retriever:     retrieval,
		Conversations: conversations,
		LLM:           chat,
		Shutdown:      asksCtx,
	}, service.AskSettings{
		Model:            cfg.GroqModel,
		MaxOutputTokens:  cfg.AskMaxOutputTokens,
		ContextBudget:    cfg.AskContextTokens,
		MaxQuestionChars: cfg.AskMaxQuestionChars,
	})
	if err != nil {
		return fmt.Errorf("ask service: %w", err)
	}

	// 7c. Summaries: the same pattern as ingestion (bounded workers, bounded
	// queue, application-lifetime context, startup recovery).
	summaryRunner := &service.SummaryRunner{}
	summaryQueue, err := service.NewJobQueue(appCtx, "summary", summaryRunner, cfg.SummaryWorkers, service.SummaryQueueSize)
	if err != nil {
		return fmt.Errorf("summary queue: %w", err)
	}
	summarySvc, err := service.NewSummaryService(service.SummaryDeps{
		Store: docRepo, Chunks: chunkRepo, Jobs: summaryQueue, LLM: chat,
	}, service.SummarySettings{
		Model:           cfg.GroqSummaryModel,
		GroupTokens:     cfg.SummaryGroupTokens,
		MaxOutputTokens: cfg.SummaryMaxOutputTokens,
		MaxChunks:       cfg.SummaryMaxChunks,
	})
	if err != nil {
		return fmt.Errorf("summary service: %w", err)
	}
	summaryRunner.Bind(summarySvc) // before Start: a worker must never see an unbound runner
	summaryQueue.Start()

	// Recovery: summaries left 'processing' by an earlier run are written again
	// (or, with no API key, marked failed so they can be requested later). Every
	// restart during a long summary spends its tokens again: in development,
	// avoid restarting while one is running.
	unfinishedSummaries, err := docRepo.ListSummarizing(context.Background())
	if err != nil {
		return fmt.Errorf("list unfinished summaries: %w", err)
	}
	summaryRecoveryDone := make(chan struct{})
	go func() {
		defer close(summaryRecoveryDone)
		if len(unfinishedSummaries) == 0 {
			return
		}
		slog.Info("recovering summaries left unfinished by an earlier run", "count", len(unfinishedSummaries))
		summaryQueue.EnqueueAll(appCtx, unfinishedSummaries)
	}()

	// 7d. Rate limits: in memory, per process. The sweepers forget idle keys and
	// stop with appCtx.
	authLimiter, err := ratelimit.NewKeyedLimiter(ratelimit.Config{PerMinute: cfg.AuthRatePerMinute})
	if err != nil {
		return fmt.Errorf("auth rate limiter: %w", err)
	}
	uploadLimiter, err := ratelimit.NewKeyedLimiter(ratelimit.Config{PerMinute: cfg.UploadRatePerMinute})
	if err != nil {
		return fmt.Errorf("upload rate limiter: %w", err)
	}
	llmLimiter, err := ratelimit.NewKeyedLimiter(ratelimit.Config{PerMinute: cfg.LLMRatePerMinute})
	if err != nil {
		return fmt.Errorf("ai rate limiter: %w", err)
	}
	llmSlots, err := ratelimit.NewKeyedSemaphore(cfg.LLMMaxConcurrentPerUser)
	if err != nil {
		return fmt.Errorf("ai concurrency limit: %w", err)
	}
	var sweepers sync.WaitGroup
	for _, l := range []*ratelimit.KeyedLimiter{authLimiter, uploadLimiter, llmLimiter} {
		sweepers.Add(1)
		go func(l *ratelimit.KeyedLimiter) {
			defer sweepers.Done()
			l.Run(appCtx, time.Minute)
		}(l)
	}
	limits := router.Limits{
		Auth:   []gin.HandlerFunc{middleware.RateLimitByIP(authLimiter)},
		Upload: []gin.HandlerFunc{middleware.RateLimitByUser(uploadLimiter)},
		// The concurrency cap comes first: a request refused for having too many
		// answers running must not use up a token of the rate limit.
		LLM: []gin.HandlerFunc{middleware.LimitConcurrencyByUser(llmSlots), middleware.RateLimitByUser(llmLimiter)},
	}
	slog.Info("rate limits active",
		"auth_per_min", authLimiter.PerMinute(), "auth_burst", authLimiter.Burst(),
		"upload_per_min", uploadLimiter.PerMinute(), "upload_burst", uploadLimiter.Burst(),
		"ai_per_min", llmLimiter.PerMinute(), "ai_burst", llmLimiter.Burst(),
		"ai_concurrent_per_user", cfg.LLMMaxConcurrentPerUser, "ai_concurrent_global", cfg.LLMGlobalConcurrency)

	// 7. Router and server
	r := router.New(router.Deps{
		DB:             pool,
		Auth:           authService,
		Documents:      docService,
		Tokens:         tokens,
		CORSOrigins:    cfg.CORSOrigins,
		MaxUploadBytes: cfg.MaxUploadBytes,
		Asker:          askService,
		Messages:       conversations,
		Summaries:      summarySvc,
		Limits:         limits,
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
	stop()    // a second Ctrl+C now force-kills instead of being swallowed
	stopAsks() // running answers end now; they are saved as partial and the client is told

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

	if err := summaryQueue.Shutdown(ingestDrainTimeout); err != nil {
		slog.Warn("summaries did not stop cleanly", "error", err)
	}

	stopApp()
	select {
	case <-recoveryDone:
	case <-time.After(5 * time.Second):
		slog.Warn("startup recovery did not stop in time")
	}

	select {
	case <-summaryRecoveryDone:
	case <-time.After(5 * time.Second):
		slog.Warn("summary recovery did not stop in time")
	}

	sweepersDone := make(chan struct{})
	go func() { sweepers.Wait(); close(sweepersDone) }()
	select {
	case <-sweepersDone:
	case <-time.After(5 * time.Second):
		slog.Warn("rate limit sweepers did not stop in time")
	}

	if httpErr != nil {
		return fmt.Errorf("graceful shutdown failed: %w", httpErr)
	}
	slog.Info("server stopped cleanly")
	return nil
}