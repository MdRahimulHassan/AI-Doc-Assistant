// Command summarycheck estimates, and then really writes, the summary of one
// ingested document in the DEVELOPMENT database. Run it first with -dry: it
// spends nothing and shows what a summary will cost.
//
//	go run ./cmd/summarycheck -doc <document-id> -dry
//	go run ./cmd/summarycheck -doc <document-id>
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/config"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/database"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/llm"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/models"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/repository"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/service"
)

// noQueue is a job queue that does nothing: this tool runs the job itself.
type noQueue struct{}

func (noQueue) Submit(uuid.UUID) error { return nil }

func main() { os.Exit(run()) }

func run() int {
	docFlag := flag.String("doc", "", "document id (printed by ingestcheck)")
	dry := flag.Bool("dry", false, "only estimate the cost; call nothing, change nothing")
	timeout := flag.Duration("timeout", 30*time.Minute, "give up after this long")
	flag.Parse()

	if *docFlag == "" {
		fmt.Fprintln(os.Stderr, "usage: go run ./cmd/summarycheck -doc <document-id> [-dry]")
		return 2
	}
	docID, err := uuid.Parse(*docFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "the -doc value is not a valid document id")
		return 2
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	pool, err := database.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer pool.Close()

	var owner uuid.UUID
	var filename, status string
	var chunkCount int
	err = pool.QueryRow(ctx, `SELECT user_id, filename, status, chunk_count FROM documents WHERE id = $1`, docID).
		Scan(&owner, &filename, &status, &chunkCount)
	if errors.Is(err, pgx.ErrNoRows) {
		fmt.Fprintln(os.Stderr, "no such document in the development database")
		return 1
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("document: %s (%s), status %s, %d chunks\n", filename, docID, status, chunkCount)

	docs := repository.NewDocumentRepository(pool)
	chunks := repository.NewChunkRepository(pool)

	texts, err := chunks.ChunkTexts(ctx, docID, cfg.SummaryMaxChunks+1)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if len(texts) > cfg.SummaryMaxChunks {
		fmt.Printf("TOO LONG: more than SUMMARY_MAX_CHUNKS (%d) chunks, the app would refuse (422)\n", cfg.SummaryMaxChunks)
		return 1
	}

	est := service.EstimateSummary(texts, cfg.SummaryGroupTokens, cfg.SummaryMaxOutputTokens)
	fmt.Printf("\nestimate (group %d tokens, output limit %d per call):\n", cfg.SummaryGroupTokens, cfg.SummaryMaxOutputTokens)
	fmt.Printf("  chunks %d, texts after splitting %d\n", est.Chunks, est.Items)
	fmt.Printf("  first-layer calls %d, at least %d calls in total (more if the summaries do not fit one prompt)\n", est.MapCalls, est.Calls)
	fmt.Printf("  about %d tokens in all\n", est.Tokens)
	fmt.Printf("  about %.1f minutes at %d tokens/minute (GROQ_TPM_BUDGET), if nothing else is using it\n",
		float64(est.Tokens)/float64(cfg.GroqTPMBudget), cfg.GroqTPMBudget)
	if *dry {
		return 0
	}

	if !cfg.LLMEnabled() {
		fmt.Fprintln(os.Stderr, "GROQ_API_KEY is not set in .env")
		return 2
	}
	if status != string(models.StatusReady) {
		fmt.Fprintln(os.Stderr, "the document is not ready")
		return 1
	}
	client, err := llm.New(llm.Config{APIKey: cfg.GroqAPIKey, TPMBudget: cfg.GroqTPMBudget})
	if err != nil {
		fmt.Fprintln(os.Stderr, "client:", err)
		return 1
	}
	defer client.Close()

	svc, err := service.NewSummaryService(service.SummaryDeps{Store: docs, Chunks: chunks, Jobs: noQueue{}, LLM: client},
		service.SummarySettings{
			Model: cfg.GroqSummaryModel, GroupTokens: cfg.SummaryGroupTokens,
			MaxOutputTokens: cfg.SummaryMaxOutputTokens, MaxChunks: cfg.SummaryMaxChunks, Timeout: *timeout,
		})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	// Claim the document exactly as the endpoint does, then run the job here.
	if _, err := docs.StartSummary(ctx, owner, docID, cfg.SummaryMaxChunks); err != nil {
		fmt.Fprintln(os.Stderr, "could not start:", err, "(is a summary already running?)")
		return 1
	}
	fmt.Printf("\nwriting the summary with %s. Progress is in the log lines below (this can take minutes)...\n\n", cfg.GroqSummaryModel)
	start := time.Now()
	runErr := svc.Run(ctx, docID)
	took := time.Since(start).Round(time.Second)

	doc, err := docs.GetByID(ctx, owner, docID)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("\nsummary status: %s (%v)\n", doc.SummaryStatus, took)
	if doc.SummaryError != nil {
		fmt.Printf("message: %s\n", *doc.SummaryError)
	}
	if runErr != nil {
		fmt.Println("(the full error is in the log lines above)")
	}
	if doc.Summary != nil && doc.SummaryStatus == models.SummaryReady {
		fmt.Printf("\n--- summary (%d words) ---\n%s\n--- end ---\n",
			len(strings.Fields(*doc.Summary)), *doc.Summary)
		if utf8.RuneCountInString(*doc.Summary) < 50 {
			fmt.Println("WARNING: that is suspiciously short")
		}
	}
	fmt.Println("\nClean up test data with:  DELETE FROM users WHERE email = 'ingestcheck@example.com';")
	return 0
}