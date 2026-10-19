// Command retrievecheck shows what retrieval returns for questions about one
// ingested document in the DEVELOPMENT database. It is the manual check for
// Week 3, Step 3: if retrieval is wrong, the model cannot save the answer.
//
//	go run ./cmd/retrievecheck -doc <document-id> "question one" "question two"
//	go run ./cmd/retrievecheck -doc <id> -cd "question"      (also show ts_rank_cd's ranking)
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
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/models"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/repository"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/service"
)

func main() { os.Exit(run()) }

func run() int {
	docFlag := flag.String("doc", "", "document id (printed by ingestcheck)")
	k := flag.Int("k", 0, "chunks per question (default: ASK_TOP_K)")
	compare := flag.Bool("cd", false, "also show the ts_rank_cd ranking, for comparison")
	flag.Parse()
	questions := flag.Args()

	if *docFlag == "" || len(questions) == 0 {
		fmt.Fprintln(os.Stderr, `usage: go run ./cmd/retrievecheck -doc <document-id> [-k 4] [-cd] "question" ["question" ...]`)
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
	ctx := context.Background()
	pool, err := database.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer pool.Close()

	// A developer tool with database access: find the owner so that the same
	// ownership-checked code path the app uses can be exercised.
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

	topK := cfg.AskTopK
	if *k > 0 {
		topK = *k
	}
	repo := repository.NewChunkRepository(pool)
	svc, err := service.NewRetrievalService(repo, topK)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	for i, q := range questions {
		fmt.Printf("\nQ%d: %s\n", i+1, q)

		start := time.Now()
		res, err := svc.Retrieve(ctx, owner, docID, q)
		took := time.Since(start).Round(time.Millisecond)
		if err != nil {
			fmt.Printf("    error: %v\n", err)
			continue
		}
		fmt.Printf("    terms (%d): %s   [%v]\n", len(res.Terms), strings.Join(res.Terms, ", "), took)

		if res.Empty() {
			fmt.Println("    no chunk matches: the ask flow would answer \"not found\" WITHOUT calling the model")
			continue
		}
		show(ctx, repo, res.Terms, res.Chunks, false)

		if *compare {
			byCD, err := repo.SearchChunksByRankCD(ctx, owner, docID, res.Terms, topK)
			if err != nil {
				fmt.Printf("    ts_rank_cd error: %v\n", err)
				continue
			}
			fmt.Println("    ts_rank_cd ranking, for comparison:")
			show(ctx, repo, res.Terms, byCD, true)
			fmt.Printf("    %d of %d chunks appear in both rankings\n", overlap(res.Chunks, byCD), len(res.Chunks))
		}
	}
	return 0
}

func show(ctx context.Context, repo *repository.ChunkRepository, terms []string, chunks []models.RetrievedChunk, byCD bool) {
	for i, c := range chunks {
		matched := fmt.Sprintf("%d/%d", c.MatchedTerms, len(terms))
		if byCD {
			matched = "-"
		}
		snippet, err := repo.Snippet(ctx, c.Content, terms)
		if err != nil {
			snippet = "(no snippet: " + err.Error() + ")"
		}
		fmt.Printf("    #%d  chunk %3d  score %7.3f  matched %-5s %s\n",
			i+1, c.Index, c.Score, matched, strings.Join(strings.Fields(snippet), " "))
	}
}

func overlap(a, b []models.RetrievedChunk) int {
	in := make(map[int]bool, len(a))
	for _, c := range a {
		in[c.Index] = true
	}
	n := 0
	for _, c := range b {
		if in[c.Index] {
			n++
		}
	}
	return n
}