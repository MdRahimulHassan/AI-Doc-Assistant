package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/models"
)

const (
	maxSearchLimit = 50  // most chunks one search returns
	maxSearchTerms = 32  // most terms one search uses
	maxTermBytes   = 200 // longer "terms" are not real words
)

// ChunkRepository searches the text chunks of documents.
type ChunkRepository struct {
	pool *pgxpool.Pool
}

func NewChunkRepository(pool *pgxpool.Pool) *ChunkRepository {
	return &ChunkRepository{pool: pool}
}

// QueryTerms reduces a question to the stemmed words used for searching.
//
// Postgres does the work with the SAME text search configuration that built the
// chunks' search column (migration 000004), so a question word and the same word
// in a chunk reduce to the same term ("laboratory" and "laboratories" are both
// "laboratori"), and stop words ("what", "the", "about") disappear.
//
// If there are more than maxTerms, the longest are kept, because long words are
// on average more specific than short ones. The result is never nil: a question
// of only stop words gives an empty slice, which is a normal outcome.
func (r *ChunkRepository) QueryTerms(ctx context.Context, question string, maxTerms int) ([]string, error) {
	maxTerms = min(max(maxTerms, 1), maxSearchTerms)
	question = strings.ReplaceAll(question, "\x00", " ") // Postgres text cannot hold NUL

	// 'english' must match the configuration used by the chunks.tsv column.
	const q = `
		SELECT lexeme
		FROM unnest(to_tsvector('english', $1)) AS u(lexeme, positions, weights)
		ORDER BY length(lexeme) DESC, lexeme
		LIMIT $2`

	rows, err := r.pool.Query(ctx, q, question, maxTerms)
	if err != nil {
		return nil, fmt.Errorf("query terms: %w", err)
	}
	defer rows.Close()

	terms := make([]string, 0, maxTerms)
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("scan term: %w", err)
		}
		terms = append(terms, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query terms: %w", err)
	}
	return terms, nil
}

// SearchChunks returns the chunks of ONE document that best match the terms,
// best first. The ranking is BM25-style: a term's weight grows with how rare it
// is across the document's chunks, and repeating a term helps less and less.
//
// Ownership is built in: the document must belong to userID and be 'ready',
// otherwise models.ErrNotFound (the same answer for "does not exist", "not
// yours" and "not finished"). No usable terms, or no matching chunk, is not an
// error: the result is an empty, non-nil slice.
func (r *ChunkRepository) SearchChunks(ctx context.Context, userID, documentID uuid.UUID, terms []string, limit int) ([]models.RetrievedChunk, error) {
	return r.search(ctx, userID, documentID, terms, limit, bm25Query)
}

// SearchChunksByRankCD ranks with Postgres's ts_rank_cd instead. It exists so
// the two rankings can be compared on real documents (see cmd/retrievecheck).
// MatchedTerms is not measured by this ranking and is always 0.
func (r *ChunkRepository) SearchChunksByRankCD(ctx context.Context, userID, documentID uuid.UUID, terms []string, limit int) ([]models.RetrievedChunk, error) {
	return r.search(ctx, userID, documentID, terms, limit, rankCDQuery)
}

func (r *ChunkRepository) search(ctx context.Context, userID, documentID uuid.UUID, terms []string, limit int, query string) ([]models.RetrievedChunk, error) {
	// 1. Ownership and readiness, before anything else.
	var chunkCount int
	err := r.pool.QueryRow(ctx, `
		SELECT chunk_count FROM documents
		WHERE id = $1 AND user_id = $2 AND status = $3`,
		documentID, userID, string(models.StatusReady)).Scan(&chunkCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, models.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("check document: %w", err)
	}

	// 2. Nothing to search for is a normal result.
	out := make([]models.RetrievedChunk, 0)
	terms = cleanTerms(terms)
	if len(terms) == 0 || chunkCount == 0 {
		return out, nil
	}
	limit = min(max(limit, 1), maxSearchLimit)

	// 3. The search itself. Terms and limit are parameters, never part of the SQL text.
	rows, err := r.pool.Query(ctx, query, documentID, terms, limit)
	if err != nil {
		return nil, fmt.Errorf("search chunks: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var c models.RetrievedChunk
		if err := rows.Scan(&c.Index, &c.Content, &c.Score, &c.MatchedTerms); err != nil {
			return nil, fmt.Errorf("scan chunk: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search chunks: %w", err)
	}
	return out, nil
}

// Snippet returns the passage of a chunk around the matched terms (at most two
// fragments of about 40 words), with the matches wrapped in [[double brackets]].
// It is for DISPLAY: a developer reading retrieval results now, and the source
// previews of the ask endpoint later. Without usable terms it returns the
// beginning of the text.
func (r *ChunkRepository) Snippet(ctx context.Context, content string, terms []string) (string, error) {
	terms = cleanTerms(terms)
	if len(terms) == 0 {
		return leadingText(content, 200), nil
	}

	const q = `
		SELECT ts_headline('english', $1::text,
		       (SELECT string_agg('''' || replace(replace(t, '\', '\\'), '''', '''''') || '''', ' | ')
		        FROM unnest($2::text[]) AS t)::tsquery,
		       'StartSel=[[, StopSel=]], MaxFragments=2, MinWords=15, MaxWords=40')`

	var s string
	if err := r.pool.QueryRow(ctx, q, strings.ReplaceAll(content, "\x00", " "), terms).Scan(&s); err != nil {
		return "", fmt.Errorf("snippet: %w", err)
	}
	return s, nil
}

// cleanTerms drops everything that is not a usable search term, and duplicates.
// It protects the query syntax (an empty quoted term is a syntax error), the
// text encoding (NUL bytes, invalid UTF-8) and the query size.
func cleanTerms(terms []string) []string {
	seen := make(map[string]struct{}, len(terms))
	out := make([]string, 0, len(terms))
	for _, t := range terms {
		if strings.TrimSpace(t) == "" || len(t) > maxTermBytes ||
			strings.ContainsRune(t, 0) || !utf8.ValidString(t) {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
		if len(out) == maxSearchTerms {
			break
		}
	}
	return out
}

// leadingText flattens whitespace and cuts at max characters (never mid-character).
func leadingText(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// bm25Query: $1 document, $2 terms (text[]), $3 limit.
//
//	hits    one row per (chunk, query term the chunk contains) with how often it occurs there
//	idf     each term's weight from how many of the document's chunks contain it
//	scored  per chunk: the sum over its terms of weight * saturated frequency
//
// unnest(tsvector) lists a chunk's distinct terms with their positions, so
// cardinality(positions) is the term's frequency in that chunk. The search
// column is the one generated in migration 000004.
const bm25Query = `
WITH q AS (
    SELECT DISTINCT t AS lexeme FROM unnest($2::text[]) AS t
),
hits AS (
    SELECT c.chunk_index, u.lexeme, cardinality(u.positions) AS tf
    FROM chunks c
    CROSS JOIN LATERAL unnest(c.tsv) AS u(lexeme, positions, weights)
    JOIN q ON q.lexeme = u.lexeme
    WHERE c.document_id = $1
),
n AS (
    SELECT count(*)::float8 AS total FROM chunks WHERE document_id = $1
),
idf AS (
    SELECT h.lexeme,
           ln(1 + (n.total - count(*)::float8 + 0.5) / (count(*)::float8 + 0.5)) AS w
    FROM hits h CROSS JOIN n
    GROUP BY h.lexeme, n.total
),
scored AS (
    SELECT h.chunk_index,
           sum(idf.w * (h.tf::float8 * 2.2) / (h.tf::float8 + 1.2)) AS score,
           count(*) AS matched
    FROM hits h
    JOIN idf ON idf.lexeme = h.lexeme
    GROUP BY h.chunk_index
)
SELECT c.chunk_index, c.content, s.score, s.matched
FROM scored s
JOIN chunks c ON c.document_id = $1 AND c.chunk_index = s.chunk_index
ORDER BY s.score DESC, c.chunk_index
LIMIT $3`

// rankCDQuery: the same parameters, ranked with ts_rank_cd. The terms are
// joined with | into a tsquery by a plain cast, which uses them exactly as
// given (no second round of stemming). Each term is single-quoted, with quotes
// and backslashes doubled, so no character in a term can change the query.
const rankCDQuery = `
WITH qt AS (
    SELECT (string_agg('''' || replace(replace(t, '\', '\\'), '''', '''''') || '''', ' | '))::tsquery AS query
    FROM unnest($2::text[]) AS t
)
SELECT c.chunk_index, c.content, ts_rank_cd(c.tsv, qt.query)::float8 AS score, 0::bigint AS matched
FROM chunks c, qt
WHERE c.document_id = $1 AND qt.query IS NOT NULL AND c.tsv @@ qt.query
ORDER BY score DESC, c.chunk_index
LIMIT $3`

// ChunkTexts returns the text of a document's chunks in reading order, at most
// limit of them. System method (no user filter): the caller has already checked
// ownership. The result is never nil.
func (r *ChunkRepository) ChunkTexts(ctx context.Context, documentID uuid.UUID, limit int) ([]string, error) {
	limit = min(max(limit, 1), 10000)

	rows, err := r.pool.Query(ctx,
		`SELECT content FROM chunks WHERE document_id = $1 ORDER BY chunk_index LIMIT $2`, documentID, limit)
	if err != nil {
		return nil, fmt.Errorf("chunk texts: %w", err)
	}
	defer rows.Close()

	out := make([]string, 0)
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("scan chunk: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chunk texts: %w", err)
	}
	return out, nil
}