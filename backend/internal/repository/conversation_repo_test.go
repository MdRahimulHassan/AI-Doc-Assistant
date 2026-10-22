package repository

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/models"
)

type convEnv struct {
	conv  *ConversationRepository
	docs  *DocumentRepository
	users *UserRepository
	pool  *pgxpool.Pool
}

func setupConv(t *testing.T) *convEnv {
	t.Helper()
	users, docs, pool := setupDocs(t)
	return &convEnv{conv: NewConversationRepository(pool), docs: docs, users: users, pool: pool}
}

// userDocConv creates a user, a document and the conversation about it.
func (e *convEnv) userDocConv(t *testing.T, email string) (*models.User, *models.Document, *models.Conversation) {
	t.Helper()
	u := mustUser(t, e.users, email)
	d := mustDoc(t, e.docs, u.ID, "paper.pdf")
	c, err := e.conv.GetOrCreateConversation(context.Background(), u.ID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	return u, d, c
}

func (e *convEnv) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *convEnv) messageCount(t *testing.T, conv uuid.UUID) int {
	return e.count(t, `SELECT count(*)::int FROM messages WHERE conversation_id = $1`, conv)
}

func question(text string) models.NewMessage {
	return models.NewMessage{Role: models.RoleUser, Content: text}
}

func answer(text string) models.NewMessage {
	return models.NewMessage{Role: models.RoleAssistant, Content: text}
}

func contents(page *models.MessagePage) []string {
	out := make([]string, len(page.Messages))
	for i, m := range page.Messages {
		out[i] = m.Content
	}
	return out
}

func intPtr(n int) *int { return &n }

// --- get or create ---

func TestConv_GetOrCreate(t *testing.T) {
	e := setupConv(t)
	ctx := context.Background()
	alice := mustUser(t, e.users, "alice@example.com")
	bob := mustUser(t, e.users, "bob@example.com")
	doc := mustDoc(t, e.docs, alice.ID, "a.pdf")
	doc2 := mustDoc(t, e.docs, alice.ID, "b.pdf")

	first, err := e.conv.GetOrCreateConversation(ctx, alice.ID, doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == uuid.Nil || first.UserID != alice.ID || first.DocumentID != doc.ID || first.CreatedAt.IsZero() {
		t.Errorf("conversation = %+v", first)
	}

	again, err := e.conv.GetOrCreateConversation(ctx, alice.ID, doc.ID)
	if err != nil || again.ID != first.ID {
		t.Fatalf("second call = (%v, %v), want the same conversation %v", again, err, first.ID)
	}

	other, err := e.conv.GetOrCreateConversation(ctx, alice.ID, doc2.ID)
	if err != nil || other.ID == first.ID {
		t.Errorf("a second document must get its own conversation, got (%v, %v)", other, err)
	}
	if n := e.count(t, `SELECT count(*)::int FROM conversations`); n != 2 {
		t.Errorf("%d conversations, want 2", n)
	}

	notFound := map[string]struct{ user, doc uuid.UUID }{
		"another user's document":        {bob.ID, doc.ID},
		"swapped arguments":              {doc.ID, alice.ID},
		"a document that does not exist": {alice.ID, uuid.New()},
		"a user that does not exist":     {uuid.New(), doc.ID},
	}
	for label, c := range notFound {
		if _, err := e.conv.GetOrCreateConversation(ctx, c.user, c.doc); !errors.Is(err, models.ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", label, err)
		}
	}
	if n := e.count(t, `SELECT count(*)::int FROM conversations`); n != 2 {
		t.Errorf("a refused call created a conversation: %d rows", n)
	}
}

// The race from the plan: two simultaneous FIRST questions must not fail.
func TestConv_GetOrCreate_ConcurrentFirstQuestions(t *testing.T) {
	e := setupConv(t)
	ctx := context.Background()
	alice := mustUser(t, e.users, "alice@example.com")

	for round := 0; round < 5; round++ { // several fresh documents, so the race is hit repeatedly
		doc := mustDoc(t, e.docs, alice.ID, fmt.Sprintf("race-%d.pdf", round))

		const n = 8
		start := make(chan struct{})
		ids := make(chan uuid.UUID, n)
		errs := make(chan error, n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				c, err := e.conv.GetOrCreateConversation(ctx, alice.ID, doc.ID)
				if err != nil {
					errs <- err
					return
				}
				ids <- c.ID
			}()
		}
		close(start)
		wg.Wait()
		close(ids)
		close(errs)

		for err := range errs {
			t.Errorf("round %d: a concurrent first question failed: %v", round, err)
		}
		seen := map[uuid.UUID]bool{}
		for id := range ids {
			seen[id] = true
		}
		if len(seen) != 1 {
			t.Errorf("round %d: %d different conversations were returned, want 1", round, len(seen))
		}
		if rows := e.count(t, `SELECT count(*)::int FROM conversations WHERE document_id = $1`, doc.ID); rows != 1 {
			t.Errorf("round %d: %d conversation rows, want 1", round, rows)
		}
	}
}

// --- adding messages ---

func TestConv_AddMessage_RoundTrip(t *testing.T) {
	e := setupConv(t)
	ctx := context.Background()
	u, _, c := e.userDocConv(t, "a@example.com")

	q, err := e.conv.AddMessage(ctx, u.ID, c.ID, question("What is this about?"))
	if err != nil {
		t.Fatal(err)
	}
	if q.ID <= 0 || q.CreatedAt.IsZero() || q.Role != models.RoleUser || q.Status != models.MessageComplete ||
		q.Content != "What is this about?" || q.Sources != nil || q.Model != nil || q.PromptTokens != nil || q.CompletionTokens != nil {
		t.Errorf("question = %+v", q)
	}

	full, err := e.conv.AddMessage(ctx, u.ID, c.ID, models.NewMessage{
		Role: models.RoleAssistant, Content: "It is about X. [P1]", Sources: []int{3, 1},
		Model: "openai/gpt-oss-120b", PromptTokens: intPtr(1200), CompletionTokens: intPtr(80),
	})
	if err != nil {
		t.Fatal(err)
	}
	if full.ID <= q.ID {
		t.Errorf("ids must increase: %d then %d", q.ID, full.ID)
	}
	if full.Status != models.MessageComplete || !slices.Equal(full.Sources, []int{3, 1}) ||
		full.Model == nil || *full.Model != "openai/gpt-oss-120b" ||
		full.PromptTokens == nil || *full.PromptTokens != 1200 || full.CompletionTokens == nil || *full.CompletionTokens != 80 {
		t.Errorf("answer = %+v", full)
	}

	// nil sources are stored as NULL and an empty list as [] (a "not found" answer, for instance).
	none, err := e.conv.AddMessage(ctx, u.ID, c.ID, models.NewMessage{Role: models.RoleAssistant, Content: "No passages.", Sources: []int{}})
	if err != nil || none.Sources == nil || len(none.Sources) != 0 {
		t.Errorf("empty sources = (%#v, %v), want a non-nil empty slice", none.Sources, err)
	}

	// Cut-short and failed answers may have no text.
	for _, status := range []models.MessageStatus{models.MessagePartial, models.MessageError} {
		m, err := e.conv.AddMessage(ctx, u.ID, c.ID, models.NewMessage{Role: models.RoleAssistant, Content: "", Status: status})
		if err != nil || m.Status != status || m.Content != "" {
			t.Errorf("empty %s answer = (%+v, %v)", status, m, err)
		}
	}
	if n := e.messageCount(t, c.ID); n != 5 {
		t.Errorf("%d messages stored, want 5", n)
	}
}

func TestConv_AddMessage_OwnershipIsBuiltIn(t *testing.T) {
	e := setupConv(t)
	ctx := context.Background()
	alice, doc, c := e.userDocConv(t, "alice@example.com")
	bob := mustUser(t, e.users, "bob@example.com")

	notFound := map[string]struct{ user, conv uuid.UUID }{
		"another user's conversation":        {bob.ID, c.ID},
		"swapped arguments":                  {c.ID, alice.ID},
		"a conversation that does not exist": {alice.ID, uuid.New()},
	}
	for label, x := range notFound {
		if _, err := e.conv.AddMessage(ctx, x.user, x.conv, question("hello")); !errors.Is(err, models.ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", label, err)
		}
	}
	if n := e.messageCount(t, c.ID); n != 0 {
		t.Errorf("%d messages were stored by refused calls", n)
	}

	// Once the document is gone, so is the conversation.
	if _, err := e.docs.Delete(ctx, alice.ID, doc.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.conv.AddMessage(ctx, alice.ID, c.ID, question("hello")); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("after deleting the document: err = %v, want ErrNotFound", err)
	}
}

func TestConv_AddMessage_InvalidMessagesAreRejectedBeforeTheDatabase(t *testing.T) {
	e := setupConv(t)
	ctx := context.Background()
	u, _, c := e.userDocConv(t, "a@example.com")

	bad := map[string]models.NewMessage{
		"unknown role":              {Role: "system", Content: "x"},
		"unknown status":            {Role: models.RoleAssistant, Content: "x", Status: "bogus"},
		"an empty question":         {Role: models.RoleUser, Content: ""},
		"a question of only NULs":   {Role: models.RoleUser, Content: "\x00\x00"},
		"an empty complete answer":  {Role: models.RoleAssistant, Content: ""},
		"a question with a model":   {Role: models.RoleUser, Content: "x", Model: "m"},
		"a negative source":         {Role: models.RoleAssistant, Content: "x", Sources: []int{-1}},
		"negative tokens":           {Role: models.RoleAssistant, Content: "x", PromptTokens: intPtr(-5)},
		"a question marked partial": {Role: models.RoleUser, Content: "x", Status: models.MessagePartial},
	}
	for label, m := range bad {
		if _, err := e.conv.AddMessage(ctx, u.ID, c.ID, m); !errors.Is(err, models.ErrInvalidMessage) {
			t.Errorf("%s: err = %v, want ErrInvalidMessage (not a database error)", label, err)
		}
	}
	if n := e.messageCount(t, c.ID); n != 0 {
		t.Errorf("%d messages were stored", n)
	}
}

func TestConv_AddMessage_SanitizesInsteadOfLosingTheMessage(t *testing.T) {
	e := setupConv(t)
	ctx := context.Background()
	u, _, c := e.userDocConv(t, "a@example.com")

	m, err := e.conv.AddMessage(ctx, u.ID, c.ID, answer("a\x00b\xffc"))
	if err != nil || m.Content != "abc" {
		t.Errorf("got (%+v, %v), want the NUL byte and the invalid byte removed", m, err)
	}

	long := strings.Repeat("é", models.MaxMessageChars+10)
	m, err = e.conv.AddMessage(ctx, u.ID, c.ID, answer(long))
	if err != nil {
		t.Fatalf("an over-long answer must be cut, not refused: %v", err)
	}
	if n := utf8.RuneCountInString(m.Content); n != models.MaxMessageChars || !utf8.ValidString(m.Content) {
		t.Errorf("stored %d characters, want %d", n, models.MaxMessageChars)
	}
	if n := e.count(t, `SELECT char_length(content)::int FROM messages WHERE id = $1`, m.ID); n != models.MaxMessageChars {
		t.Errorf("the database holds %d characters", n)
	}
}

// --- listing and paging ---

func TestConv_ListMessages_PagesNewestFirstReturnedOldestFirst(t *testing.T) {
	e := setupConv(t)
	ctx := context.Background()
	u, d, c := e.userDocConv(t, "a@example.com")

	var ids []int64
	for i := 1; i <= 7; i++ {
		in := question(fmt.Sprintf("message %d", i))
		if i%2 == 0 {
			in = answer(fmt.Sprintf("message %d", i))
		}
		m, err := e.conv.AddMessage(ctx, u.ID, c.ID, in)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m.ID)
	}
	if !slices.IsSorted(ids) {
		t.Fatalf("message ids must increase: %v", ids)
	}

	list := func(limit int, before int64) *models.MessagePage {
		page, err := e.conv.ListMessages(ctx, u.ID, d.ID, limit, before)
		if err != nil {
			t.Fatalf("ListMessages(%d, %d): %v", limit, before, err)
		}
		return page
	}

	p1 := list(3, 0) // the newest three, oldest first
	if !slices.Equal(contents(p1), []string{"message 5", "message 6", "message 7"}) || !p1.HasMore {
		t.Fatalf("page 1 = %v (more: %v)", contents(p1), p1.HasMore)
	}
	p2 := list(3, p1.OldestID())
	if !slices.Equal(contents(p2), []string{"message 2", "message 3", "message 4"}) || !p2.HasMore {
		t.Fatalf("page 2 = %v (more: %v)", contents(p2), p2.HasMore)
	}
	p3 := list(3, p2.OldestID())
	if !slices.Equal(contents(p3), []string{"message 1"}) || p3.HasMore {
		t.Fatalf("page 3 = %v (more: %v)", contents(p3), p3.HasMore)
	}

	if all := list(7, 0); len(all.Messages) != 7 || all.HasMore {
		t.Errorf("limit 7 returned %d messages (more: %v), want all 7 and no more", len(all.Messages), all.HasMore)
	}
	if six := list(6, 0); len(six.Messages) != 6 || !six.HasMore {
		t.Errorf("limit 6 returned %d messages (more: %v), want 6 and more", len(six.Messages), six.HasMore)
	}
	if def := list(0, 0); len(def.Messages) != 7 {
		t.Errorf("the default limit returned %d messages, want all 7", len(def.Messages))
	}
	if huge := list(5000, 0); len(huge.Messages) != 7 {
		t.Errorf("a huge limit returned %d messages", len(huge.Messages))
	}
	if gone := list(10, ids[0]); len(gone.Messages) != 0 || gone.HasMore || gone.Messages == nil {
		t.Errorf("before the first message = %+v, want an empty non-nil page", gone)
	}
	if neg := list(2, -9); len(neg.Messages) != 2 {
		t.Errorf("a negative cursor must mean 'from the newest', got %d messages", len(neg.Messages))
	}
}

func TestConv_ListMessages_ScopeAndOwnership(t *testing.T) {
	e := setupConv(t)
	ctx := context.Background()

	alice, docA, convA := e.userDocConv(t, "alice@example.com")
	docB := mustDoc(t, e.docs, alice.ID, "second.pdf") // a second document of the SAME user
	convB, err := e.conv.GetOrCreateConversation(ctx, alice.ID, docB.ID)
	if err != nil {
		t.Fatal(err)
	}
	empty := mustDoc(t, e.docs, alice.ID, "never-asked.pdf") // owned, but no conversation yet
	bob := mustUser(t, e.users, "bob@example.com")

	for _, m := range []struct {
		conv uuid.UUID
		text string
	}{{convA.ID, "A one"}, {convB.ID, "B one"}, {convA.ID, "A two"}} {
		if _, err := e.conv.AddMessage(ctx, alice.ID, m.conv, question(m.text)); err != nil {
			t.Fatal(err)
		}
	}

	page, err := e.conv.ListMessages(ctx, alice.ID, docA.ID, 10, 0)
	if err != nil || !slices.Equal(contents(page), []string{"A one", "A two"}) {
		t.Errorf("document A's page = (%v, %v): messages of other documents must not appear", contents(page), err)
	}

	if page, err = e.conv.ListMessages(ctx, alice.ID, empty.ID, 10, 0); err != nil || page.Messages == nil || len(page.Messages) != 0 || page.HasMore {
		t.Errorf("a document without a conversation = (%+v, %v), want an empty non-nil page and no error", page, err)
	}

	notFound := map[string]struct{ user, doc uuid.UUID }{
		"another user's document":        {bob.ID, docA.ID},
		"swapped arguments":              {docA.ID, alice.ID},
		"a document that does not exist": {alice.ID, uuid.New()},
	}
	for label, x := range notFound {
		if _, err := e.conv.ListMessages(ctx, x.user, x.doc, 10, 0); !errors.Is(err, models.ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", label, err)
		}
	}

	// Deleting the document removes the conversation and its messages.
	if _, err := e.docs.Delete(ctx, alice.ID, docA.ID); err != nil {
		t.Fatal(err)
	}
	if n := e.messageCount(t, convA.ID); n != 0 {
		t.Errorf("%d messages survived the document", n)
	}
	if _, err := e.conv.ListMessages(ctx, alice.ID, docA.ID, 10, 0); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("after the delete: err = %v, want ErrNotFound", err)
	}
	if page, err := e.conv.ListMessages(ctx, alice.ID, docB.ID, 10, 0); err != nil || len(page.Messages) != 1 {
		t.Errorf("document B's messages were affected: (%v, %v)", contents(page), err)
	}
}