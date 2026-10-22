package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/models"
)

type ctxKey struct{}

type addCall struct {
	userID, convID uuid.UUID
	msg            models.NewMessage
	ctxErr         error // ctx.Err() at the moment of the write
	ctxValue       any
	hasDeadline    bool
}

type fakeConvStore struct {
	mu sync.Mutex

	conv    *models.Conversation
	getErr  error
	addErr  error
	page    *models.MessagePage
	listErr error

	added      []addCall
	nextID     int64
	getCalls   int
	listUser   uuid.UUID
	listDoc    uuid.UUID
	listLimit  int
	listBefore int64
}

func (f *fakeConvStore) GetOrCreateConversation(_ context.Context, userID, docID uuid.UUID) (*models.Conversation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.conv, nil
}

func (f *fakeConvStore) AddMessage(ctx context.Context, userID, convID uuid.UUID, m models.NewMessage) (*models.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, hasDeadline := ctx.Deadline()
	f.added = append(f.added, addCall{userID, convID, m, ctx.Err(), ctx.Value(ctxKey{}), hasDeadline})
	if f.addErr != nil {
		return nil, f.addErr
	}
	f.nextID++
	n := m.Normalize()
	return &models.Message{ID: f.nextID, Role: n.Role, Content: n.Content, Status: n.Status}, nil
}

func (f *fakeConvStore) ListMessages(_ context.Context, userID, docID uuid.UUID, limit int, before int64) (*models.MessagePage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listUser, f.listDoc, f.listLimit, f.listBefore = userID, docID, limit, before
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.page == nil {
		return &models.MessagePage{Messages: []models.Message{}}, nil
	}
	return f.page, nil
}

// --- write order and detached context ---

func TestRecordAnswer_UsesADetachedContext(t *testing.T) {
	store := &fakeConvStore{}
	svc := NewConversationService(store)
	user, conv := uuid.New(), uuid.New()

	// The client has disconnected: the request context is already cancelled.
	parent, cancel := context.WithCancel(context.WithValue(context.Background(), ctxKey{}, "req-42"))
	cancel()

	m, err := svc.RecordAnswer(parent, user, conv, Answer{Content: "Half an answ", Status: models.MessagePartial})
	if err != nil || m == nil {
		t.Fatalf("RecordAnswer = (%v, %v): a disconnected client must not lose the answer", m, err)
	}

	got := store.added[0]
	if got.ctxErr != nil {
		t.Errorf("the write used a cancelled context: %v", got.ctxErr)
	}
	if got.ctxValue != "req-42" {
		t.Errorf("the request's values (request id for logs) were lost: %v", got.ctxValue)
	}
	if !got.hasDeadline {
		t.Error("the detached write needs its own timeout")
	}
	if got.userID != user || got.convID != conv || got.msg.Role != models.RoleAssistant || got.msg.Status != models.MessagePartial {
		t.Errorf("stored %+v", got)
	}
}

func TestRecordQuestion_UsesTheRequestContext(t *testing.T) {
	store := &fakeConvStore{}
	svc := NewConversationService(store)

	t.Run("a live request saves the question", func(t *testing.T) {
		m, err := svc.RecordQuestion(context.Background(), uuid.New(), uuid.New(), "What is this?")
		if err != nil || m.Role != models.RoleUser {
			t.Fatalf("got (%+v, %v)", m, err)
		}
		if c := store.added[0]; c.msg.Role != models.RoleUser || c.msg.Content != "What is this?" || c.msg.Status != "" {
			t.Errorf("stored %+v", c.msg)
		}
	})

	t.Run("a request that is already gone saves nothing", func(t *testing.T) {
		before := len(store.added)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if _, err := svc.RecordQuestion(ctx, uuid.New(), uuid.New(), "hello"); !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
		if len(store.added) != before {
			t.Error("an orphan question was saved for a client that had already left")
		}
	})
}

func TestRecordAnswer_UsageSourcesAndModel(t *testing.T) {
	store := &fakeConvStore{}
	svc := NewConversationService(store)

	_, err := svc.RecordAnswer(context.Background(), uuid.New(), uuid.New(), Answer{
		Content: "It is 83 [P1].", Status: models.MessageComplete, Sources: []int{12, 4}, Model: "openai/gpt-oss-120b",
		Usage: TokenUsage{PromptTokens: 1200, CompletionTokens: 90, TotalTokens: 1290, ReasoningTokens: 30},
	})
	if err != nil {
		t.Fatal(err)
	}
	m := store.added[0].msg
	if !slices.Equal(m.Sources, []int{12, 4}) || m.Model != "openai/gpt-oss-120b" ||
		m.PromptTokens == nil || *m.PromptTokens != 1200 || m.CompletionTokens == nil || *m.CompletionTokens != 90 {
		t.Errorf("stored %+v", m)
	}

	// Unknown usage is stored as unknown (NULL), not as zero.
	if _, err := svc.RecordAnswer(context.Background(), uuid.New(), uuid.New(), Answer{Content: NotFoundAnswer, Sources: []int{}}); err != nil {
		t.Fatal(err)
	}
	m = store.added[1].msg
	if m.PromptTokens != nil || m.CompletionTokens != nil || m.Model != "" || m.Sources == nil {
		t.Errorf("an app-written answer should carry no usage or model, but an empty (non-nil) sources list: %+v", m)
	}
}

func TestConversationService_ErrorsKeepTheirCause(t *testing.T) {
	user, conv, doc := uuid.New(), uuid.New(), uuid.New()
	boom := errors.New("database is down")

	store := &fakeConvStore{getErr: models.ErrNotFound, addErr: boom, listErr: models.ErrNotFound}
	svc := NewConversationService(store)

	if _, err := svc.Open(context.Background(), user, doc); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("Open err = %v, want ErrNotFound", err)
	}
	if _, err := svc.RecordQuestion(context.Background(), user, conv, "q"); !errors.Is(err, boom) {
		t.Errorf("RecordQuestion err = %v, want the cause", err)
	}
	if _, err := svc.RecordAnswer(context.Background(), user, conv, Answer{Content: "a"}); !errors.Is(err, boom) {
		t.Errorf("RecordAnswer err = %v, want the cause", err)
	}
	if _, err := svc.Messages(context.Background(), user, doc, 10, 0); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("Messages err = %v, want ErrNotFound", err)
	}
	if _, err := svc.History(context.Background(), user, doc); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("History err = %v, want ErrNotFound", err)
	}
}

func TestConversationService_PassesThePagingAndTheUserThrough(t *testing.T) {
	user, doc := uuid.New(), uuid.New()
	store := &fakeConvStore{page: &models.MessagePage{Messages: []models.Message{{ID: 5}}, HasMore: true}}
	svc := NewConversationService(store)

	page, err := svc.Messages(context.Background(), user, doc, 25, 99)
	if err != nil || len(page.Messages) != 1 || !page.HasMore {
		t.Fatalf("got (%+v, %v)", page, err)
	}
	if store.listUser != user || store.listDoc != doc || store.listLimit != 25 || store.listBefore != 99 {
		t.Errorf("the repository received user %v, doc %v, limit %d, before %d", store.listUser, store.listDoc, store.listLimit, store.listBefore)
	}
}

// --- status ---

func TestClassifyAnswer(t *testing.T) {
	boom := io.ErrUnexpectedEOF

	tests := []struct {
		name    string
		content string
		finish  string
		err     error
		want    models.MessageStatus
	}{
		{"a normal answer", "The answer.", "stop", nil, models.MessageComplete},
		{"no finish reason reported", "The answer.", "", nil, models.MessageComplete},
		{"cut off by the token limit", "The ans", "length", nil, models.MessagePartial},
		{"stopped for an unexpected reason", "The ans", "content_filter", nil, models.MessagePartial},
		{"the stream broke after some text", "The ans", "", boom, models.MessagePartial},
		{"the provider failed after some text", "The ans", "", &LLMError{Kind: LLMUnavailable}, models.MessagePartial},
		{"the client left after some text", "The ans", "", context.Canceled, models.MessagePartial},
		{"the client left before any text", "", "", context.Canceled, models.MessagePartial},
		{"a timeout before any text", "", "", context.DeadlineExceeded, models.MessageError},
		{"a provider failure before any text", "", "", &LLMError{Kind: LLMBusy, Status: 429}, models.MessageError},
		{"an unknown failure before any text", "", "", boom, models.MessageError},
		{"finished with nothing at all", "", "stop", nil, models.MessageError},
		{"finished with only whitespace", "  \n", "stop", nil, models.MessageError},
		{"cut off with nothing at all", "", "length", nil, models.MessageError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyAnswer(tc.content, tc.finish, tc.err); got != tc.want {
				t.Errorf("ClassifyAnswer = %q, want %q", got, tc.want)
			}
		})
	}
}

// The status must never produce a message the database would refuse.
func TestClassifyAnswer_AlwaysProducesAValidMessage(t *testing.T) {
	errs := []error{nil, io.ErrUnexpectedEOF, context.Canceled, context.DeadlineExceeded,
		&LLMError{Kind: LLMBusy, Status: 429}, fmt.Errorf("wrapped: %w", context.Canceled)}

	for _, content := range []string{"", "   ", "An answer."} {
		for _, finish := range []string{"", "stop", "length", "content_filter"} {
			for _, err := range errs {
				status := ClassifyAnswer(content, finish, err)
				m := models.NewMessage{Role: models.RoleAssistant, Content: content, Status: status}.Normalize()
				if verr := m.Validate(); verr != nil {
					t.Errorf("content %q, finish %q, err %v gives status %q, which is invalid: %v", content, finish, err, status, verr)
				}
			}
		}
	}
}

// --- history ---

func msg(role models.MessageRole, status models.MessageStatus, content string) models.Message {
	return models.Message{Role: role, Status: status, Content: content}
}

func q(text string) models.Message { return msg(models.RoleUser, models.MessageComplete, text) }
func a(text string) models.Message { return msg(models.RoleAssistant, models.MessageComplete, text) }

func TestPairHistory(t *testing.T) {
	tests := []struct {
		name string
		msgs []models.Message
		max  int
		want []HistoryTurn
	}{
		{"nothing", nil, 2, nil},
		{"one exchange", []models.Message{q("q1"), a("a1")}, 2, []HistoryTurn{{"q1", "a1"}}},
		{"keeps the latest turns, oldest first",
			[]models.Message{q("q1"), a("a1"), q("q2"), a("a2"), q("q3"), a("a3")}, 2,
			[]HistoryTurn{{"q2", "a2"}, {"q3", "a3"}}},
		{"an unanswered last question is skipped",
			[]models.Message{q("q1"), a("a1"), q("q2")}, 2, []HistoryTurn{{"q1", "a1"}}},
		{"a cut-short answer is skipped together with its question",
			[]models.Message{q("q1"), a("a1"), q("q2"), msg(models.RoleAssistant, models.MessagePartial, "half")}, 2,
			[]HistoryTurn{{"q1", "a1"}}},
		{"a failed answer is skipped",
			[]models.Message{q("q1"), msg(models.RoleAssistant, models.MessageError, ""), q("q2"), a("a2")}, 2,
			[]HistoryTurn{{"q2", "a2"}}},
		{"an orphan question followed by a normal exchange",
			[]models.Message{q("lost"), q("q2"), a("a2")}, 2, []HistoryTurn{{"q2", "a2"}}},
		{"a page that starts with an answer",
			[]models.Message{a("a0"), q("q1"), a("a1")}, 2, []HistoryTurn{{"q1", "a1"}}},
		{"blank text is skipped", []models.Message{q("  "), a("a1"), q("q2"), a("\n")}, 2, nil},
		{"no limit returns every turn",
			[]models.Message{q("q1"), a("a1"), q("q2"), a("a2"), q("q3"), a("a3")}, 0,
			[]HistoryTurn{{"q1", "a1"}, {"q2", "a2"}, {"q3", "a3"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := PairHistory(tc.msgs, tc.max); !slices.Equal(got, tc.want) {
				t.Errorf("PairHistory = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestHistory_ReadsTheLatestMessagesAndPairsThem(t *testing.T) {
	user, doc := uuid.New(), uuid.New()
	store := &fakeConvStore{page: &models.MessagePage{Messages: []models.Message{
		q("q1"), a("a1"), q("q2"), a("a2"), q("q3"), a("a3"), q("q4"),
	}}}
	svc := NewConversationService(store)

	turns, err := svc.History(context.Background(), user, doc)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(turns, []HistoryTurn{{"q2", "a2"}, {"q3", "a3"}}) {
		t.Errorf("turns = %+v, want the last two answered exchanges", turns)
	}
	if store.listUser != user || store.listDoc != doc || store.listLimit != historyFetchSize || store.listBefore != 0 {
		t.Errorf("read user %v, doc %v, limit %d, before %d", store.listUser, store.listDoc, store.listLimit, store.listBefore)
	}
}

func TestPersistContext(t *testing.T) {
	parent, cancel := context.WithCancel(context.WithValue(context.Background(), ctxKey{}, "v"))
	cancel()

	ctx, release := persistContext(parent)
	defer release()
	if ctx.Err() != nil || ctx.Value(ctxKey{}) != "v" {
		t.Errorf("err %v, value %v", ctx.Err(), ctx.Value(ctxKey{}))
	}
	if dl, ok := ctx.Deadline(); !ok || time.Until(dl) > persistTimeout || time.Until(dl) < persistTimeout/2 {
		t.Errorf("deadline %v (set %v)", dl, ok)
	}
}