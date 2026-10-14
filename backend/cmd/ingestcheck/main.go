// Command ingestcheck runs the ingestion pipeline on one file against the
// DEVELOPMENT database, then searches the resulting chunks. It is the manual
// check for Week 2, Step 6, before any HTTP code exists.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/chunker"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/config"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/database"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/models"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/parser"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/repository"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/service"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/storage"
	"github.com/jackc/pgx/v5/pgxpool"
)

const devEmail = "ingestcheck@example.com"

func main() { os.Exit(run()) }

func run() int {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, `usage: go run ./cmd/ingestcheck <file.pdf|file.docx|file.txt> [search words]`)
		return 2
	}
	path := os.Args[1]
	query := strings.Join(os.Args[2:], " ")

	byExt := map[string]string{".pdf": parser.MimePDF, ".docx": parser.MimeDOCX, ".txt": parser.MimeTXT}
	mimeType, ok := byExt[strings.ToLower(filepath.Ext(path))]
	if !ok {
		fmt.Fprintln(os.Stderr, "unsupported extension (use .pdf, .docx or .txt)")
		return 2
	}

	if err := execute(path, mimeType, query); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

func execute(path, mimeType, query string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	ctx := context.Background()
	pool, err := database.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := database.RunMigrations(cfg.DatabaseURL); err != nil {
		return err
	}

	files, err := storage.NewLocal(cfg.UploadDir)
	if err != nil {
		return err
	}

	// A fixed dev user owns the test documents. "!disabled" is not a valid
	// bcrypt hash, so nobody can log in as this user.
	users := repository.NewUserRepository(pool)
	user, err := users.Create(ctx, devEmail, "!disabled")
	if errors.Is(err, models.ErrEmailTaken) {
		user, err = users.GetByEmail(ctx, devEmail)
	}
	if err != nil {
		return fmt.Errorf("dev user: %w", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if int64(len(data)) > cfg.MaxUploadBytes {
		return fmt.Errorf("file is %d bytes; the upload limit is %d", len(data), cfg.MaxUploadBytes)
	}

	// What the upload handler will do in Step 7: save the file, insert the row.
	ext, _ := parser.ExtensionFor(mimeType)
	docID := uuid.New()
	rel, size, err := files.Save(ctx, user.ID, docID, ext, bytes.NewReader(data), cfg.MaxUploadBytes)
	if err != nil {
		return err
	}
	docs := repository.NewDocumentRepository(pool)
	if _, err := docs.Create(ctx, &models.Document{
		ID: docID, UserID: user.ID, Filename: filepath.Base(path),
		MimeType: mimeType, SizeBytes: size, StoragePath: rel,
	}); err != nil {
		return err
	}

	svc := service.NewIngestService(docs, files, chunker.Default(), service.DefaultIngestTimeout)
	start := time.Now()
	ingestErr := svc.Ingest(ctx, docID) // failures are logged above this line
	took := time.Since(start).Round(time.Millisecond)

	doc, err := docs.GetByID(ctx, user.ID, docID)
	if err != nil {
		return err
	}

	fmt.Printf("\ndocument: %s\n", docID)
	fmt.Printf("status:   %s (%v)\n", doc.Status, took)
	fmt.Printf("chunks:   %d\n", doc.ChunkCount)
	if doc.ErrorMessage != nil {
		fmt.Printf("message:  %s\n", *doc.ErrorMessage)
	}
	if ingestErr != nil {
		fmt.Println("(Ingest returned an error: the full detail is in the log lines above)")
	}

	if query != "" && doc.Status == models.StatusReady {
		if err := search(ctx, pool, docID, query); err != nil {
			return err
		}
	}

	fmt.Printf("\nClean up with:\n  DELETE FROM users WHERE email = '%s';\n", devEmail)
	return nil
}

// search runs the full-text query from the plan.
// search runs the full-text query from the plan.
func search(ctx context.Context, pool *pgxpool.Pool, docID uuid.UUID, query string) error {
	rows, err := pool.Query(ctx, `
		SELECT chunk_index, left(content, 80)
		FROM chunks
		WHERE document_id = $1 AND tsv @@ plainto_tsquery('english', $2)
		ORDER BY ts_rank(tsv, plainto_tsquery('english', $2)) DESC
		LIMIT 5`, docID, query)
	if err != nil {
		return err
	}
	defer rows.Close()

	fmt.Printf("\nsearch %q:\n", query)
	found := 0
	for rows.Next() {
		var idx int
		var preview string
		if err := rows.Scan(&idx, &preview); err != nil {
			return err
		}
		fmt.Printf("  chunk %3d  %s\n", idx, strings.ReplaceAll(preview, "\n", " "))
		found++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if found == 0 {
		fmt.Println("  no chunk matches. plainto_tsquery needs EVERY word in the same chunk and drops")
		fmt.Println("  common words like 'the'; try fewer or rarer words.")
	}
	return nil
}