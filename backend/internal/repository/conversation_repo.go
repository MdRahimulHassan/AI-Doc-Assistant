package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/models"
)

const (
	defaultMessageLimit = 50
	maxMessageLimit     = 100
)

// ConversationRepository stores each user's conversation about each document.
// Ownership is built in: every method takes the user's ID, and every write
// checks it in the SAME statement that writes.
type ConversationRepository struct {
	pool *pgxpool.Pool
}

func NewConversationRepository(pool *pgxpool.Pool) *ConversationRepository {
	return &ConversationRepository{pool: pool}
}

// GetOrCreateConversation returns the user's conversation about a document,
// creating it on the first question. It is safe when two first questions arrive
// at once: both get the same conversation and neither fails.
//
// models.ErrNotFound means the document does not exist or is not the user's.
func (r *ConversationRepository) GetOrCreateConversation(ctx context.Context, userID, documentID uuid.UUID) (*models.Conversation, error) {
	// Statement 1: create it, but only for the document's owner. If the
	// conversation already exists (or another request is creating it right now)
	// nothing happens: ON CONFLICT waits for that request to commit and then
	// does nothing.
	const insert = `
		INSERT INTO conversations (user_id, document_id)
		SELECT d.user_id, d.id
		FROM documents d
		WHERE d.id = $2 AND d.user_id = $1
		ON CONFLICT (user_id, document_id) DO NOTHING`
	if _, err := r.pool.Exec(ctx, insert, userID, documentID); err != nil {
		return nil, fmt.Errorf("create conversation: %w", err)
	}

	// Statement 2: read it. It is a SEPARATE statement on purpose: under READ
	// COMMITTED each statement gets a fresh snapshot, so a conversation committed
	// by a competing request a moment ago is visible here. A single CTE
	// ("insert, else select") takes its snapshot before the insert and can
	// return nothing in exactly this race.
	const sel = `
		SELECT id, user_id, document_id, created_at
		FROM conversations
		WHERE user_id = $1 AND document_id = $2`
	var c models.Conversation
	err := r.pool.QueryRow(ctx, sel, userID, documentID).Scan(&c.ID, &c.UserID, &c.DocumentID, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, models.ErrNotFound // no such document for this user (or it was deleted meanwhile)
	}
	if err != nil {
		return nil, fmt.Errorf("get conversation: %w", err)
	}
	return &c, nil
}

// AddMessage appends a message to the user's own conversation.
//
//   - The message is normalized and validated first: a mistake is
//     models.ErrInvalidMessage, never a database error.
//   - models.ErrNotFound means the conversation does not exist or is not the
//     user's (the ownership check is part of the INSERT itself).
func (r *ConversationRepository) AddMessage(ctx context.Context, userID, conversationID uuid.UUID, in models.NewMessage) (*models.Message, error) {
	m := in.Normalize()
	if err := m.Validate(); err != nil {
		return nil, err
	}

	var sources *string
	if m.Sources != nil {
		b, err := json.Marshal(m.Sources)
		if err != nil {
			return nil, fmt.Errorf("encode sources: %w", err)
		}
		s := string(b)
		sources = &s
	}
	var model *string
	if m.Model != "" {
		model = &m.Model
	}

	// The explicit casts matter: in "INSERT ... SELECT $n" Postgres cannot infer
	// parameter types from the target columns.
	const q = `
		INSERT INTO messages (conversation_id, role, content, status, sources, model, prompt_tokens, completion_tokens)
		SELECT c.id, $3::text, $4::text, $5::text, $6::text::jsonb, $7::text, $8::integer, $9::integer
		FROM conversations c
		WHERE c.id = $2 AND c.user_id = $1
		RETURNING id, role, content, status, sources, model, prompt_tokens, completion_tokens, created_at`

	msg, err := scanMessage(r.pool.QueryRow(ctx, q,
		userID, conversationID, string(m.Role), m.Content, string(m.Status), sources, model, m.PromptTokens, m.CompletionTokens))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, models.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("add message: %w", err)
	}
	return msg, nil
}

// ListMessages returns one page of the user's conversation about a document,
// OLDEST FIRST, built for a chat screen:
//
//   - the first call (before = 0) returns the newest `limit` messages
//   - while HasMore is true, call again with before = page.OldestID() to get
//     the next older page
//
// limit defaults to 50 and is capped at 100. A document the user owns but has
// not asked about yet gives an empty page. models.ErrNotFound means the
// document does not exist or is not the user's.
func (r *ConversationRepository) ListMessages(ctx context.Context, userID, documentID uuid.UUID, limit int, before int64) (*models.MessagePage, error) {
	var owns bool
	if err := r.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM documents WHERE id = $1 AND user_id = $2)`,
		documentID, userID).Scan(&owns); err != nil {
		return nil, fmt.Errorf("check document: %w", err)
	}
	if !owns {
		return nil, models.ErrNotFound
	}

	if limit <= 0 {
		limit = defaultMessageLimit
	}
	limit = min(limit, maxMessageLimit)
	before = max(before, 0)

	// One extra row tells whether older messages exist, without a count query.
	const q = `
		SELECT m.id, m.role, m.content, m.status, m.sources, m.model, m.prompt_tokens, m.completion_tokens, m.created_at
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		WHERE c.user_id = $1 AND c.document_id = $2
		  AND ($3::bigint = 0 OR m.id < $3::bigint)
		ORDER BY m.id DESC
		LIMIT $4`
	rows, err := r.pool.Query(ctx, q, userID, documentID, before, limit+1)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer rows.Close()

	newestFirst := make([]models.Message, 0, limit+1)
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		newestFirst = append(newestFirst, *m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}

	page := &models.MessagePage{}
	if len(newestFirst) > limit {
		page.HasMore = true
		newestFirst = newestFirst[:limit] // the extra row is the OLDEST one
	}
	slices.Reverse(newestFirst)
	page.Messages = newestFirst // never nil
	return page, nil
}

func scanMessage(row pgx.Row) (*models.Message, error) {
	var m models.Message
	var rawSources []byte
	err := row.Scan(&m.ID, &m.Role, &m.Content, &m.Status, &rawSources, &m.Model,
		&m.PromptTokens, &m.CompletionTokens, &m.CreatedAt)
	if err != nil {
		return nil, err
	}
	if rawSources != nil { // NULL stays nil; [] becomes an empty, non-nil slice
		if err := json.Unmarshal(rawSources, &m.Sources); err != nil {
			return nil, fmt.Errorf("decode sources: %w", err)
		}
	}
	return &m, nil
}