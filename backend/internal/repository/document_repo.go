package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/models"
)

const (
	defaultListLimit = 20
	maxListLimit     = 100
)

const documentColumns = `id, user_id, filename, mime_type, size_bytes, storage_path,
	status, error_message, chunk_count, created_at, updated_at,
	summary, summary_status, summary_error, summary_updated_at`

type DocumentRepository struct {
	pool *pgxpool.Pool
}

func NewDocumentRepository(pool *pgxpool.Pool) *DocumentRepository {
	return &DocumentRepository{pool: pool}
}

// ---------------------------------------------------------------------------
// User-facing methods. Every one takes userID and filters on it, so a caller
// cannot forget the ownership check. A missing document and someone else's
// document both return models.ErrNotFound.
//
// NOTE: userID and id are both uuid.UUID, so the compiler cannot catch swapped
// arguments. The integration tests do.
// ---------------------------------------------------------------------------

// Create inserts a document. The caller supplies the ID (Step 7 generates it
// first, so the file path can be built before the row exists). The status
// always starts as 'processing' via the column default.
func (r *DocumentRepository) Create(ctx context.Context, doc *models.Document) (*models.Document, error) {
	if doc.ID == uuid.Nil || doc.UserID == uuid.Nil {
		return nil, errors.New("create document: id and user id are required")
	}

	const q = `
		INSERT INTO documents (id, user_id, filename, mime_type, size_bytes, storage_path)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING ` + documentColumns

	created, err := scanDocument(r.pool.QueryRow(ctx, q,
		doc.ID, doc.UserID, doc.Filename, doc.MimeType, doc.SizeBytes, doc.StoragePath))
	if err != nil {
		return nil, fmt.Errorf("insert document: %w", err)
	}
	return created, nil
}

func (r *DocumentRepository) GetByID(ctx context.Context, userID, id uuid.UUID) (*models.Document, error) {
	const q = `SELECT ` + documentColumns + ` FROM documents WHERE id = $1 AND user_id = $2`

	doc, err := scanDocument(r.pool.QueryRow(ctx, q, id, userID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, models.ErrNotFound
		}
		return nil, fmt.Errorf("get document: %w", err)
	}
	return doc, nil
}

// List returns the user's documents, newest first. Limit and offset are
// clamped here as a safety net; the handler validates them properly (Step 7).
// The result is never nil, so it encodes as [] and not null.
func (r *DocumentRepository) List(ctx context.Context, userID uuid.UUID, limit, offset int) ([]models.Document, error) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	if offset < 0 {
		offset = 0
	}

	// id DESC breaks ties so pages never overlap or skip rows.
	const q = `SELECT ` + documentColumns + `
		FROM documents
		WHERE user_id = $1
		ORDER BY created_at DESC, id DESC
		LIMIT $2 OFFSET $3`

	rows, err := r.pool.Query(ctx, q, userID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list documents: %w", err)
	}
	defer rows.Close()

	docs := make([]models.Document, 0, limit)
	for rows.Next() {
		d, err := scanDocument(rows)
		if err != nil {
			return nil, fmt.Errorf("scan document: %w", err)
		}
		docs = append(docs, *d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list documents: %w", err)
	}
	return docs, nil
}

// Delete removes the document (its chunks go with it, via ON DELETE CASCADE)
// and returns its storage path so the service can delete the file.
func (r *DocumentRepository) Delete(ctx context.Context, userID, id uuid.UUID) (string, error) {
	const q = `DELETE FROM documents WHERE id = $1 AND user_id = $2 RETURNING storage_path`

	var path string
	if err := r.pool.QueryRow(ctx, q, id, userID).Scan(&path); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", models.ErrNotFound
		}
		return "", fmt.Errorf("delete document: %w", err)
	}
	return path, nil
}

// ---------------------------------------------------------------------------
// System methods, used only by the ingestion pipeline. There is no user in
// that context, so there is no ownership filter. The names say so on purpose,
// and the interfaces handlers depend on (Step 7) will not include them.
// ---------------------------------------------------------------------------

// GetForIngest loads a document by ID alone. Never call it from a handler.
func (r *DocumentRepository) GetForIngest(ctx context.Context, id uuid.UUID) (*models.Document, error) {
	const q = `SELECT ` + documentColumns + ` FROM documents WHERE id = $1`

	doc, err := scanDocument(r.pool.QueryRow(ctx, q, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, models.ErrNotFound
		}
		return nil, fmt.Errorf("get document for ingest: %w", err)
	}
	return doc, nil
}

// ListProcessing returns the IDs of documents still marked 'processing',
// oldest first. At startup these are documents whose ingestion was
// interrupted (a crash, or queued jobs left behind at shutdown). It has no
// user filter: it is a system method, like GetForIngest.
func (r *DocumentRepository) ListProcessing(ctx context.Context) ([]uuid.UUID, error) {
	const q = `
		SELECT id FROM documents
		WHERE status = $1
		ORDER BY created_at ASC, id ASC`

	rows, err := r.pool.Query(ctx, q, string(models.StatusProcessing))
	if err != nil {
		return nil, fmt.Errorf("list processing documents: %w", err)
	}
	defer rows.Close()

	ids := make([]uuid.UUID, 0)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan document id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list processing documents: %w", err)
	}
	return ids, nil
}

// MarkFailed records a failure. The message must be a short, safe, fixed
// string chosen by the caller, never a raw err.Error().
func (r *DocumentRepository) MarkFailed(ctx context.Context, id uuid.UUID, safeMessage string) error {
	const q = `
		UPDATE documents
		SET status = $2, error_message = $3, updated_at = now()
		WHERE id = $1`

	tag, err := r.pool.Exec(ctx, q, id, string(models.StatusFailed), safeMessage)
	if err != nil {
		return fmt.Errorf("mark document failed: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return models.ErrNotFound // deleted while it was being processed
	}
	return nil
}

// CompleteIngestion replaces the document's chunks and marks it ready, in ONE
// transaction: afterwards either everything changed or nothing did. A crash
// can never leave a "ready" document without chunks, or chunks on a
// "processing" one. Re-running it is safe because old chunks are replaced.
func (r *DocumentRepository) CompleteIngestion(ctx context.Context, id uuid.UUID, chunks []string) error {
	if len(chunks) == 0 {
		return errors.New("complete ingestion: no chunks")
	}

	// BeginFunc commits if the function returns nil and rolls back otherwise.
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		// 1. Update the document FIRST. This takes the row lock, so a concurrent
		//    Delete, or a second ingestion of the same document, waits for us.
		//    It also tells us whether the document still exists.
		const upd = `
			UPDATE documents
			SET status = $2, error_message = NULL, chunk_count = $3, updated_at = now()
			WHERE id = $1`
		tag, err := tx.Exec(ctx, upd, id, string(models.StatusReady), len(chunks))
		if err != nil {
			return fmt.Errorf("update document: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return models.ErrNotFound
		}

		// 2. Remove chunks from any earlier run.
		if _, err := tx.Exec(ctx, `DELETE FROM chunks WHERE document_id = $1`, id); err != nil {
			return fmt.Errorf("delete old chunks: %w", err)
		}

		// 3. Bulk-insert with COPY: one round trip instead of one INSERT per chunk.
		//    id and tsv are not listed: Postgres generates both.
		docID := pgtype.UUID{Bytes: id, Valid: true} // explicit type keeps COPY's binary encoding unambiguous
		rows := pgx.CopyFromSlice(len(chunks), func(i int) ([]any, error) {
			return []any{docID, int32(i), chunks[i]}, nil
		})
		n, err := tx.CopyFrom(ctx, pgx.Identifier{"chunks"},
			[]string{"document_id", "chunk_index", "content"}, rows)
		if err != nil {
			return fmt.Errorf("copy chunks: %w", err)
		}
		if int(n) != len(chunks) {
			return fmt.Errorf("copy chunks: inserted %d of %d", n, len(chunks))
		}
		return nil
	})
}

// documentDest lists the scan targets in the order of documentColumns.
func documentDest(d *models.Document) []any {
	return []any{&d.ID, &d.UserID, &d.Filename, &d.MimeType, &d.SizeBytes, &d.StoragePath,
		&d.Status, &d.ErrorMessage, &d.ChunkCount, &d.CreatedAt, &d.UpdatedAt,
		&d.Summary, &d.SummaryStatus, &d.SummaryError, &d.SummaryUpdatedAt}
}

// scanDocument keeps the column order in one place. pgx.Row is satisfied by
// both QueryRow results and the rows of a Query loop.
func scanDocument(row pgx.Row) (*models.Document, error) {
	var d models.Document
	if err := row.Scan(documentDest(&d)...); err != nil {
		return nil, err
	}
	return &d, nil
}

// ---------------------------------------------------------------------------
// Summaries
// ---------------------------------------------------------------------------

// StartSummary claims a document for summarizing and returns the state it was in.
// The claim is one statement: the row is locked (FOR UPDATE), so of several
// simultaneous requests exactly one wins and the rest are refused.
//
// Refusals, checked in this order:
//
//	missing, or not the user's        models.ErrNotFound
//	not ready (still processing...)   *models.DocumentNotReadyError
//	more than maxChunks chunks        models.ErrDocumentTooLong
//	a summary is being written        models.ErrSummaryInProgress
func (r *DocumentRepository) StartSummary(ctx context.Context, userID, id uuid.UUID, maxChunks int) (*models.SummaryStart, error) {
	const q = `
		WITH prev AS (
			SELECT id AS prev_id, summary_status AS prev_status, summary_error AS prev_error
			FROM documents
			WHERE id = $1 AND user_id = $2 AND status = $4 AND summary_status <> $5 AND chunk_count <= $3
			FOR UPDATE
		)
		UPDATE documents
		SET summary_status = $5, summary_error = NULL, updated_at = now()
		FROM prev
		WHERE documents.id = prev.prev_id
		RETURNING ` + documentColumns + `, prev.prev_status, prev.prev_error`

	var d models.Document
	var prevStatus models.SummaryStatus
	var prevErr *string
	dest := append(documentDest(&d), &prevStatus, &prevErr)

	err := r.pool.QueryRow(ctx, q, id, userID, maxChunks, string(models.StatusReady), string(models.SummaryProcessing)).Scan(dest...)
	switch {
	case err == nil:
		return &models.SummaryStart{Document: &d, PreviousStatus: prevStatus, PreviousError: prevErr}, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf("start summary: %w", err)
	}

	// Nothing was updated: find out why.
	var status models.DocumentStatus
	var chunkCount int
	err = r.pool.QueryRow(ctx,
		`SELECT status, chunk_count FROM documents WHERE id = $1 AND user_id = $2`, id, userID).Scan(&status, &chunkCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, models.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("start summary: %w", err)
	}
	switch {
	case status != models.StatusReady:
		return nil, &models.DocumentNotReadyError{Status: status}
	case chunkCount > maxChunks:
		return nil, models.ErrDocumentTooLong
	default:
		return nil, models.ErrSummaryInProgress
	}
}

// RevertSummaryStart puts a document back to the state StartSummary returned,
// when the job could not be queued. It only acts while the summary is still
// 'processing'. models.ErrNotFound means the document is gone.
func (r *DocumentRepository) RevertSummaryStart(ctx context.Context, userID, id uuid.UUID, previous models.SummaryStatus, previousError *string) error {
	const q = `
		UPDATE documents
		SET summary_status = $3, summary_error = $4, updated_at = now()
		WHERE id = $1 AND user_id = $2 AND summary_status = $5`

	tag, err := r.pool.Exec(ctx, q, id, userID, string(previous), previousError, string(models.SummaryProcessing))
	if err != nil {
		return fmt.Errorf("revert summary start: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return models.ErrNotFound
	}
	return nil
}

// GetForSummary loads a document by ID alone, for the summary worker. System
// method, like GetForIngest: never call it from a handler.
func (r *DocumentRepository) GetForSummary(ctx context.Context, id uuid.UUID) (*models.Document, error) {
	return r.GetForIngest(ctx, id)
}

// CompleteSummary stores the finished summary. It only acts while the summary
// is 'processing'. models.ErrNotFound means the document was deleted meanwhile
// (or is no longer being summarized).
func (r *DocumentRepository) CompleteSummary(ctx context.Context, id uuid.UUID, summary string) error {
	if strings.TrimSpace(summary) == "" {
		return errors.New("complete summary: the summary is empty")
	}
	const q = `
		UPDATE documents
		SET summary = $2, summary_status = $4, summary_error = NULL, summary_updated_at = now(), updated_at = now()
		WHERE id = $1 AND summary_status = $3`

	tag, err := r.pool.Exec(ctx, q, id, summary, string(models.SummaryProcessing), string(models.SummaryReady))
	if err != nil {
		return fmt.Errorf("complete summary: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return models.ErrNotFound
	}
	return nil
}

// FailSummary records a failure. The message must be a short, safe, fixed
// string, never a raw err.Error(). An earlier summary, if there was one, stays.
func (r *DocumentRepository) FailSummary(ctx context.Context, id uuid.UUID, safeMessage string) error {
	const q = `
		UPDATE documents
		SET summary_status = $4, summary_error = $2, updated_at = now()
		WHERE id = $1 AND summary_status = $3`

	tag, err := r.pool.Exec(ctx, q, id, safeMessage, string(models.SummaryProcessing), string(models.SummaryFailed))
	if err != nil {
		return fmt.Errorf("fail summary: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return models.ErrNotFound
	}
	return nil
}

// ListSummarizing returns the IDs of documents whose summary is still
// 'processing', oldest first. At startup these are jobs interrupted by a crash
// or a restart. System method, no user filter.
func (r *DocumentRepository) ListSummarizing(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id FROM documents WHERE summary_status = $1 ORDER BY created_at ASC, id ASC`,
		string(models.SummaryProcessing))
	if err != nil {
		return nil, fmt.Errorf("list summarizing documents: %w", err)
	}
	defer rows.Close()

	ids := make([]uuid.UUID, 0)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan document id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list summarizing documents: %w", err)
	}
	return ids, nil
}