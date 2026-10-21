// Command askcheck runs retrieve -> prompt -> Groq for questions about one
// ingested document in the DEVELOPMENT database and prints the answer, its
// sources, a citation check, and estimated vs. actual token counts. It is the
// manual checkpoint for Week 3, Step 4: this is where the prompt gets tuned.
//
//	go run ./cmd/askcheck -doc <document-id> "question one" "question two"
//	go run ./cmd/askcheck -doc <id> -dry -show "question"     (no Groq call, no tokens spent)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/config"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/database"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/llm"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/models"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/repository"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/service"
)

func main() { os.Exit(run()) }

func run() int {
	docFlag := flag.String("doc", "", "document id (printed by ingestcheck)")
	dry := flag.Bool("dry", false, "build and show the prompt, but do not call Groq")
	show := flag.Bool("show", false, "print the complete prompt that is sent")
	k := flag.Int("k", 0, "chunks to retrieve (default ASK_TOP_K)")
	budget := flag.Int("budget", 0, "tokens for the passages (default ASK_CONTEXT_TOKENS)")
	wait := flag.Duration("wait", 90*time.Second, "how long a call may wait for token budget before giving up")
	flag.Parse()
	questions := flag.Args()

	if *docFlag == "" || len(questions) == 0 {
		fmt.Fprintln(os.Stderr, `usage: go run ./cmd/askcheck -doc <document-id> [-dry] [-show] [-k 4] [-budget 3000] "question" ["question" ...]`)
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
	if !*dry && !cfg.LLMEnabled() {
		fmt.Fprintln(os.Stderr, "GROQ_API_KEY is not set in .env (use -dry to build prompts without calling Groq)")
		return 2
	}

	ctx := context.Background()
	pool, err := database.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer pool.Close()

	// A developer tool with database access: find the owner, so the same
	// ownership-checked code path the app uses is exercised.
	var owner uuid.UUID
	var filename, status string
	err = pool.QueryRow(ctx, `SELECT user_id, filename, status FROM documents WHERE id = $1`, docID).Scan(&owner, &filename, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		fmt.Fprintln(os.Stderr, "no such document in the development database")
		return 1
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	topK, ctxBudget := cfg.AskTopK, cfg.AskContextTokens
	if *k > 0 {
		topK = *k
	}
	if *budget > 0 {
		ctxBudget = *budget
	}

	repo := repository.NewChunkRepository(pool)
	retrieval, err := service.NewRetrievalService(repo, topK)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	var client *llm.Client
	if !*dry {
		if client, err = llm.New(llm.Config{APIKey: cfg.GroqAPIKey, TPMBudget: cfg.GroqTPMBudget}); err != nil {
			fmt.Fprintln(os.Stderr, "client:", err)
			return 1
		}
		defer client.Close()
	}

	fmt.Printf("document %s (%s), status %s\nmodel %s, prompt %s, top-k %d, passage budget %d tokens, max output %d tokens\n",
		filename, docID, status, cfg.GroqModel, service.PromptVersion, topK, ctxBudget, cfg.AskMaxOutputTokens)

	for i, q := range questions {
		fmt.Printf("\n=== Q%d: %s\n", i+1, q)
		askOne(ctx, repo, retrieval, client, owner, docID, q, cfg, ctxBudget, *dry, *show, *wait)
	}
	return 0
}

func askOne(ctx context.Context, repo *repository.ChunkRepository, retrieval *service.RetrievalService, client *llm.Client,
	owner, docID uuid.UUID, question string, cfg *config.Config, ctxBudget int, dry, show bool, wait time.Duration) {

	res, err := retrieval.Retrieve(ctx, owner, docID, question)
	if err != nil {
		if errors.Is(err, models.ErrNotFound) {
			fmt.Println("the document is missing, not yours, or not ready")
			return
		}
		fmt.Println("retrieval error:", err)
		return
	}
	fmt.Printf("terms: %s\n", strings.Join(res.Terms, ", "))
	if res.Empty() {
		fmt.Printf("NO PASSAGES: the app answers %q without calling Groq (0 tokens spent)\n", service.NotFoundAnswer)
		return
	}

	prompt, err := service.BuildPrompt(service.PromptInput{
		Question:         question,
		Chunks:           res.Chunks,
		Model:            cfg.GroqModel,
		MaxOutputTokens:  cfg.AskMaxOutputTokens,
		ContextBudget:    ctxBudget,
		MaxQuestionChars: cfg.AskMaxQuestionChars,
	})
	if err != nil {
		fmt.Println("prompt error:", err)
		return
	}

	byIndex := make(map[int]models.RetrievedChunk, len(res.Chunks))
	for _, c := range res.Chunks {
		byIndex[c.Index] = c
	}
	fmt.Printf("sources (%d used, %d dropped for budget%s):\n", len(prompt.Sources), prompt.DroppedChunks, cutNote(prompt))
	for _, s := range prompt.Sources {
		c := byIndex[s.ChunkIndex]
		snippet, serr := repo.Snippet(ctx, c.Content, res.Terms)
		if serr != nil {
			snippet = "(no snippet)"
		}
		fmt.Printf("  [%s] chunk %3d  score %6.2f  matched %d/%d  %s\n",
			s.Label, s.ChunkIndex, c.Score, c.MatchedTerms, len(res.Terms), strings.Join(strings.Fields(snippet), " "))
	}
	fmt.Printf("prompt: passages ~%d tokens, whole prompt ~%d tokens (estimated)\n", prompt.PassageTokens, prompt.EstimatedTokens)

	if show {
		for _, m := range prompt.Request.Messages {
			fmt.Printf("\n---- %s ----\n%s\n", m.Role, m.Content)
		}
		fmt.Println("\n---- end of prompt ----")
	}
	if dry {
		return
	}

	req := prompt.Request
	req.MaxWait = wait // a developer tool may wait for the token budget; the app itself will not
	start := time.Now()
	resp, err := client.Complete(ctx, req)
	if err != nil {
		var le *service.LLMError
		if errors.As(err, &le) {
			fmt.Printf("MODEL ERROR: %s (HTTP %d, code %q, retry after %v)\n  provider message (logs only): %s\n",
				le.Kind, le.Status, le.Code, le.RetryAfter.Round(time.Second), le.Message)
		} else {
			fmt.Println("error:", err)
		}
		return
	}

	fmt.Printf("\nANSWER (%v):\n%s\n\n", time.Since(start).Round(time.Millisecond), resp.Content)

	valid, invalid := service.Citations(resp.Content, len(prompt.Sources))
	switch {
	case len(invalid) > 0:
		fmt.Printf("citations: valid %v, INVALID %v  <- cites a passage that was never given\n", valid, invalid)
	case len(valid) == 0:
		fmt.Println("citations: none (expected for a 'the document does not say' answer; suspicious for a factual one)")
	default:
		fmt.Printf("citations: %v (all exist)\n", valid)
	}

	u := resp.Usage
	if u.PromptTokens > 0 {
		fmt.Printf("tokens: prompt estimated %d vs actual %d (the estimate is %.0f%% of actual); completion %d (reasoning %d); total %d\n",
			prompt.EstimatedTokens, u.PromptTokens, 100*float64(prompt.EstimatedTokens)/float64(u.PromptTokens),
			u.CompletionTokens, u.ReasoningTokens, u.TotalTokens)
	}
	if resp.FinishReason == "length" {
		fmt.Println("WARNING: the answer was CUT OFF (finish reason \"length\"): raise ASK_MAX_OUTPUT_TOKENS or check that reasoning stays low")
	}
}

func cutNote(p *service.Prompt) string {
	if p.Truncated {
		return ", the best passage was cut to fit"
	}
	return ""
}