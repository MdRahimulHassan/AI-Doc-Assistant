package database

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/testutil"
)

// --- helpers ---

// testDB migrates the test database, empties it, and returns a pool.
func testDB(t *testing.T) (string, *pgxpool.Pool) {
	t.Helper()

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	if !strings.Contains(url, "_test") { // these tests TRUNCATE tables
		t.Fatal("TEST_DATABASE_URL must point to a database whose name contains '_test'")
	}

	testutil.LockDatabase(t, url) // taken FIRST, so it is released LAST

	if err := RunMigrations(url); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	pool, err := NewPool(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(context.Background(), "TRUNCATE users CASCADE"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return url, pool
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec failed: %v\n%s", err, sql)
	}
}

// wantViolation checks that err is a constraint violation by the NAMED
// constraint, so a test cannot pass because some other rule fired.
func wantViolation(t *testing.T, label string, err error, constraint string) {
	t.Helper()
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		t.Errorf("%s: the database accepted it, want a violation of %s", label, constraint)
	case !errors.As(err, &pgErr):
		t.Errorf("%s: err = %v, want a Postgres constraint error", label, err)
	case pgErr.ConstraintName != constraint:
		t.Errorf("%s: violated %q (%s), want %q", label, pgErr.ConstraintName, pgErr.Message, constraint)
	}
}

func seedUserDoc(t *testing.T, pool *pgxpool.Pool, email string) (userID, docID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash) VALUES ($1, 'h') RETURNING id`, email).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO documents (user_id, filename, mime_type, size_bytes, storage_path)
		VALUES ($1, 'a.txt', 'text/plain', 1, $2) RETURNING id`,
		userID, "t/"+uuid.NewString()+".txt").Scan(&docID); err != nil {
		t.Fatal(err)
	}
	return userID, docID
}

func seedConversation(t *testing.T, pool *pgxpool.Pool, email string) (userID, docID, convID uuid.UUID) {
	t.Helper()
	userID, docID = seedUserDoc(t, pool, email)
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO conversations (user_id, document_id) VALUES ($1, $2) RETURNING id`,
		userID, docID).Scan(&convID); err != nil {
		t.Fatal(err)
	}
	return userID, docID, convID
}

func count(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// --- migration 000005: document summary ---

func TestSchema_DocumentSummary(t *testing.T) {
	_, pool := testDB(t)
	ctx := context.Background()
	_, doc := seedUserDoc(t, pool, "a@example.com")

	// A new document starts as "none" with nothing filled in.
	var status string
	var noSummary, noError, noUpdated bool
	if err := pool.QueryRow(ctx, `
		SELECT summary_status, summary IS NULL, summary_error IS NULL, summary_updated_at IS NULL
		FROM documents WHERE id = $1`, doc).Scan(&status, &noSummary, &noError, &noUpdated); err != nil {
		t.Fatal(err)
	}
	if status != "none" || !noSummary || !noError || !noUpdated {
		t.Errorf("defaults: status %q, summary null %v, error null %v, updated null %v", status, noSummary, noError, noUpdated)
	}

	rejected := []struct{ name, sql, constraint string }{
		{"unknown status",
			`UPDATE documents SET summary_status = 'bogus' WHERE id = $1`,
			"documents_summary_status_valid"},
		{"ready without a summary",
			`UPDATE documents SET summary_status = 'ready' WHERE id = $1`,
			"documents_summary_ready_has_text"},
		{"an error on a document that did not fail",
			`UPDATE documents SET summary_status = 'processing', summary_error = 'x' WHERE id = $1`,
			"documents_summary_error_only_when_failed"},
	}
	for _, tc := range rejected {
		_, err := pool.Exec(ctx, tc.sql, doc)
		wantViolation(t, tc.name, err, tc.constraint)
	}

	// The normal life of a summary, in order.
	allowed := []struct{ name, sql string }{
		{"processing", `UPDATE documents SET summary_status = 'processing' WHERE id = $1`},
		{"ready with text", `UPDATE documents SET summary_status = 'ready', summary = 'A summary.', summary_error = NULL, summary_updated_at = now() WHERE id = $1`},
		{"a failed regeneration keeps the old summary", `UPDATE documents SET summary_status = 'failed', summary_error = 'Processing failed.' WHERE id = $1`},
		{"retry clears the error", `UPDATE documents SET summary_status = 'processing', summary_error = NULL WHERE id = $1`},
	}
	for _, tc := range allowed {
		if _, err := pool.Exec(ctx, tc.sql, doc); err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

// --- migration 000006: conversations and messages ---

func TestSchema_Conversations(t *testing.T) {
	_, pool := testDB(t)
	ctx := context.Background()

	alice, aliceDoc, _ := seedConversation(t, pool, "alice@example.com")
	bob, _ := seedUserDoc(t, pool, "bob@example.com")

	_, err := pool.Exec(ctx, `INSERT INTO conversations (user_id, document_id) VALUES ($1, $2)`, alice, aliceDoc)
	wantViolation(t, "a second conversation for the same user and document", err, "conversations_user_document_key")

	// The composite foreign key: a conversation can only belong to the document's owner.
	_, err = pool.Exec(ctx, `INSERT INTO conversations (user_id, document_id) VALUES ($1, $2)`, bob, aliceDoc)
	wantViolation(t, "a conversation for someone else's document", err, "conversations_document_owner_fk")

	_, err = pool.Exec(ctx, `INSERT INTO conversations (user_id, document_id) VALUES ($1, $2)`, alice, uuid.New())
	wantViolation(t, "a conversation for a document that does not exist", err, "conversations_document_owner_fk")
}

func TestSchema_Messages(t *testing.T) {
	_, pool := testDB(t)
	ctx := context.Background()
	_, _, conv := seedConversation(t, pool, "a@example.com")

	// Each case breaks exactly ONE rule.
	rejected := []struct{ name, sql, constraint string }{
		{"role system",
			`INSERT INTO messages (conversation_id, role, content) VALUES ($1, 'system', 'x')`,
			"messages_role_valid"},
		{"unknown status",
			`INSERT INTO messages (conversation_id, role, content, status) VALUES ($1, 'assistant', 'x', 'bogus')`,
			"messages_status_valid"},
		{"empty question",
			`INSERT INTO messages (conversation_id, role, content) VALUES ($1, 'user', '')`,
			"messages_content_not_empty"},
		{"a complete answer with no text",
			`INSERT INTO messages (conversation_id, role, content) VALUES ($1, 'assistant', '')`,
			"messages_content_not_empty"},
		{"a question that carries a model name",
			`INSERT INTO messages (conversation_id, role, content, model) VALUES ($1, 'user', 'hi', 'm')`,
			"messages_user_has_no_metadata"},
		{"a question that carries sources",
			`INSERT INTO messages (conversation_id, role, content, sources) VALUES ($1, 'user', 'hi', '[1]')`,
			"messages_user_has_no_metadata"},
		{"a question marked partial",
			`INSERT INTO messages (conversation_id, role, content, status) VALUES ($1, 'user', 'hi', 'partial')`,
			"messages_user_has_no_metadata"},
		{"sources that are not an array",
			`INSERT INTO messages (conversation_id, role, content, sources) VALUES ($1, 'assistant', 'x', '{"a":1}')`,
			"messages_sources_is_array"},
		{"negative token counts",
			`INSERT INTO messages (conversation_id, role, content, prompt_tokens) VALUES ($1, 'assistant', 'x', -1)`,
			"messages_tokens_nonnegative"},
		{"content over the length cap",
			`INSERT INTO messages (conversation_id, role, content) VALUES ($1, 'user', repeat('x', 100001))`,
			"messages_content_length"},
	}
	for _, tc := range rejected {
		_, err := pool.Exec(ctx, tc.sql, conv)
		wantViolation(t, tc.name, err, tc.constraint)
	}

	accepted := []struct{ name, sql string }{
		{"a question",
			`INSERT INTO messages (conversation_id, role, content) VALUES ($1, 'user', 'What is this about?')`},
		{"a full answer with sources, model and tokens",
			`INSERT INTO messages (conversation_id, role, content, sources, model, prompt_tokens, completion_tokens)
			 VALUES ($1, 'assistant', 'It is about X. [1]', '[3, 1]', 'openai/gpt-oss-120b', 1200, 80)`},
		{"an answer cut short before any token arrived",
			`INSERT INTO messages (conversation_id, role, content, status) VALUES ($1, 'assistant', '', 'partial')`},
		{"a failed answer with no text",
			`INSERT INTO messages (conversation_id, role, content, status) VALUES ($1, 'assistant', '', 'error')`},
	}
	for _, tc := range accepted {
		if _, err := pool.Exec(ctx, tc.sql, conv); err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
	}

	if n := count(t, pool, `SELECT count(*)::int FROM messages WHERE conversation_id = $1`, conv); n != 4 {
		t.Errorf("%d messages stored, want exactly the 4 accepted ones", n)
	}
}

func TestSchema_Cascades(t *testing.T) {
	_, pool := testDB(t)

	addMessages := func(conv uuid.UUID) {
		mustExec(t, pool, `INSERT INTO messages (conversation_id, role, content) VALUES ($1, 'user', 'q')`, conv)
		mustExec(t, pool, `INSERT INTO messages (conversation_id, role, content) VALUES ($1, 'assistant', 'a')`, conv)
	}
	totals := func() (convs, msgs int) {
		return count(t, pool, `SELECT count(*)::int FROM conversations`),
			count(t, pool, `SELECT count(*)::int FROM messages`)
	}

	aliceUser, aliceDoc, aliceConv := seedConversation(t, pool, "alice@example.com")
	_, _, bobConv := seedConversation(t, pool, "bob@example.com")
	addMessages(aliceConv)
	addMessages(bobConv)
	if c, m := totals(); c != 2 || m != 4 {
		t.Fatalf("setup: %d conversations and %d messages, want 2 and 4", c, m)
	}

	// Deleting a document removes ITS conversation and messages, and only those.
	mustExec(t, pool, `DELETE FROM documents WHERE id = $1`, aliceDoc)
	if c, m := totals(); c != 1 || m != 2 {
		t.Errorf("after deleting a document: %d conversations and %d messages, want 1 and 2", c, m)
	}
	if n := count(t, pool, `SELECT count(*)::int FROM messages WHERE conversation_id = $1`, bobConv); n != 2 {
		t.Errorf("the other user's messages were affected: %d left, want 2", n)
	}

	// Deleting a user removes everything that user owns.
	_, aliceDoc2, aliceConv2 := seedConversationFor(t, pool, aliceUser)
	addMessages(aliceConv2)
	mustExec(t, pool, `DELETE FROM users WHERE id = $1`, aliceUser)
	if n := count(t, pool, `SELECT count(*)::int FROM documents WHERE id = $1`, aliceDoc2); n != 0 {
		t.Errorf("%d documents survived the user", n)
	}
	if c, m := totals(); c != 1 || m != 2 {
		t.Errorf("after deleting a user: %d conversations and %d messages, want only the other user's 1 and 2", c, m)
	}
}

// seedConversationFor adds a document and conversation for an existing user.
func seedConversationFor(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) (uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	var docID, convID uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO documents (user_id, filename, mime_type, size_bytes, storage_path)
		VALUES ($1, 'b.txt', 'text/plain', 1, $2) RETURNING id`,
		userID, "t/"+uuid.NewString()+".txt").Scan(&docID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO conversations (user_id, document_id) VALUES ($1, $2) RETURNING id`,
		userID, docID).Scan(&convID); err != nil {
		t.Fatal(err)
	}
	return userID, docID, convID
}

// --- the down files, on a database that holds data ---

type schemaState struct {
	summaryColumns int
	conversations  bool
	messages       bool
	ownerKey       bool
}

func inspect(t *testing.T, pool *pgxpool.Pool) schemaState {
	t.Helper()
	var s schemaState
	err := pool.QueryRow(context.Background(), `
		SELECT
		  (SELECT count(*)::int FROM information_schema.columns
		     WHERE table_schema = 'public' AND table_name = 'documents'
		       AND column_name IN ('summary', 'summary_status', 'summary_error', 'summary_updated_at')),
		  to_regclass('public.conversations') IS NOT NULL,
		  to_regclass('public.messages') IS NOT NULL,
		  EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'documents_id_user_key')`).
		Scan(&s.summaryColumns, &s.conversations, &s.messages, &s.ownerKey)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMigrations_RollBackAndReapply(t *testing.T) {
	url, pool := testDB(t)

	// Real data, so the down files are tested on a populated database.
	_, doc, conv := seedConversation(t, pool, "a@example.com")
	mustExec(t, pool, `UPDATE documents SET summary_status = 'ready', summary = 'A summary.', summary_updated_at = now() WHERE id = $1`, doc)
	mustExec(t, pool, `INSERT INTO messages (conversation_id, role, content) VALUES ($1, 'user', 'hello')`, conv)

	m, err := newMigrator(url)
	if err != nil {
		t.Fatal(err)
	}
	// Registered last, so it runs FIRST on cleanup: whatever happened above,
	// the schema is put back before the lock is released.
	t.Cleanup(func() {
		_, _ = m.Close()
		if err := RunMigrations(url); err != nil {
			t.Errorf("could not restore the schema: %v", err)
		}
	})

	version, dirty, err := m.Version()
	if err != nil || dirty {
		t.Fatalf("version %d, dirty %v, err %v", version, dirty, err)
	}
	if version < 6 {
		t.Fatalf("this test needs the schema at version 6 or later, got %d", version)
	}
	if got := inspect(t, pool); got != (schemaState{4, true, true, true}) {
		t.Fatalf("before the rollback: %+v", got)
	}

	// Down to version 4 runs every down file above it, newest first.
	if err := m.Migrate(4); err != nil {
		t.Fatalf("rolling back to version 4: %v", err)
	}
	if v, dirty, err := m.Version(); err != nil || dirty || v != 4 {
		t.Fatalf("after the rollback: version %d, dirty %v, err %v", v, dirty, err)
	}
	if got := inspect(t, pool); got != (schemaState{0, false, false, false}) {
		t.Errorf("after the rollback the new objects should be gone: %+v", got)
	}

	// And back up: a clean database ends at the same schema.
	if err := RunMigrations(url); err != nil {
		t.Fatalf("re-applying: %v", err)
	}
	if got := inspect(t, pool); got != (schemaState{4, true, true, true}) {
		t.Errorf("after re-applying: %+v", got)
	}
}