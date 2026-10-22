package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/models"
)

const (
	// persistTimeout bounds a write made after the request may be over.
	persistTimeout = 10 * time.Second

	// historyFetchSize is how many recent messages are read to find the last
	// exchanges: more than 2 turns, because unanswered questions and failed
	// answers are skipped.
	historyFetchSize = 10
)

// ConversationStore is what the service needs from the conversation repository.
type ConversationStore interface {
	GetOrCreateConversation(ctx context.Context, userID, documentID uuid.UUID) (*models.Conversation, error)
	AddMessage(ctx context.Context, userID, conversationID uuid.UUID, m models.NewMessage) (*models.Message, error)
	ListMessages(ctx context.Context, userID, documentID uuid.UUID, limit int, before int64) (*models.MessagePage, error)
}

// ConversationService records and reads the questions and answers of a
// conversation. The rules it enforces:
//
//  1. The question is saved BEFORE the model is called, so a crash never loses it.
//  2. The answer is saved AFTER the stream ends, with a status (see ClassifyAnswer).
//  3. The answer is written with a DETACHED context: the client may have
//     disconnected, which cancels the request context, and a write using it
//     would be lost along with answers that cost real tokens.
//  4. Every call carries the user's ID, and the repository checks it.
type ConversationService struct {
	store ConversationStore
}

func NewConversationService(store ConversationStore) *ConversationService {
	return &ConversationService{store: store}
}

// Open returns (creating it if needed) the user's conversation about a document.
// The error wraps models.ErrNotFound for a document that is missing or not theirs.
func (s *ConversationService) Open(ctx context.Context, userID, documentID uuid.UUID) (*models.Conversation, error) {
	c, err := s.store.GetOrCreateConversation(ctx, userID, documentID)
	if err != nil {
		return nil, fmt.Errorf("open conversation: %w", err)
	}
	return c, nil
}

// RecordQuestion saves the user's question. It uses the REQUEST context on
// purpose: if the client has already gone, nobody will read an answer, so the
// request should stop here instead of leaving an orphan question behind.
func (s *ConversationService) RecordQuestion(ctx context.Context, userID, conversationID uuid.UUID, question string) (*models.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m, err := s.store.AddMessage(ctx, userID, conversationID, models.NewMessage{Role: models.RoleUser, Content: question})
	if err != nil {
		return nil, fmt.Errorf("record question: %w", err)
	}
	return m, nil
}

// Answer is the assistant's reply, as it stood when the model call ended.
type Answer struct {
	Content string
	Status  models.MessageStatus // use ClassifyAnswer for model output
	Sources []int                // chunk indexes the answer was based on; nil if none
	Model   string               // empty for answers the app wrote itself
	Usage   TokenUsage           // zero values mean "unknown"
}

// RecordAnswer saves the reply with a DETACHED context: it keeps the request's
// values (such as the request ID in logs) but ignores its cancellation, and has
// its own timeout.
func (s *ConversationService) RecordAnswer(ctx context.Context, userID, conversationID uuid.UUID, a Answer) (*models.Message, error) {
	pctx, cancel := persistContext(ctx)
	defer cancel()

	m := models.NewMessage{
		Role:    models.RoleAssistant,
		Content: a.Content,
		Status:  a.Status,
		Sources: a.Sources,
		Model:   a.Model,
	}
	if a.Usage.PromptTokens > 0 {
		n := a.Usage.PromptTokens
		m.PromptTokens = &n
	}
	if a.Usage.CompletionTokens > 0 {
		n := a.Usage.CompletionTokens
		m.CompletionTokens = &n
	}

	saved, err := s.store.AddMessage(pctx, userID, conversationID, m)
	if err != nil {
		return nil, fmt.Errorf("record answer: %w", err)
	}
	return saved, nil
}

// Messages returns one page of the conversation (oldest first). See
// ConversationRepository.ListMessages for the paging rules.
func (s *ConversationService) Messages(ctx context.Context, userID, documentID uuid.UUID, limit int, before int64) (*models.MessagePage, error) {
	page, err := s.store.ListMessages(ctx, userID, documentID, limit, before)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	return page, nil
}

// History returns the last exchanges for the prompt builder. Call it BEFORE
// RecordQuestion for the new question.
func (s *ConversationService) History(ctx context.Context, userID, documentID uuid.UUID) ([]HistoryTurn, error) {
	page, err := s.store.ListMessages(ctx, userID, documentID, historyFetchSize, 0)
	if err != nil {
		return nil, fmt.Errorf("load history: %w", err)
	}
	return PairHistory(page.Messages, maxHistoryTurns), nil
}

// persistContext is a context that outlives the request: same values, no
// cancellation, its own deadline.
func persistContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
}

// ClassifyAnswer decides the status to store for the output of a model call,
// from the text received, the finish reason, and the error (nil if the call
// succeeded). Every result is a message the database accepts: in particular,
// an answer with no text is never "complete".
//
//	finished normally with text                      -> complete
//	stopped by the token limit (or any odd reason)   -> partial
//	failed or was cut off after some text            -> partial
//	the client left (context cancelled)              -> partial (may be empty)
//	failed before any text                           -> error
//	"finished" with no text at all                   -> error
func ClassifyAnswer(content, finishReason string, err error) models.MessageStatus {
	hasText := strings.TrimSpace(content) != ""

	if err != nil {
		switch {
		case hasText:
			return models.MessagePartial
		case errors.Is(err, context.Canceled):
			return models.MessagePartial
		default:
			return models.MessageError
		}
	}
	if !hasText {
		return models.MessageError
	}
	if finishReason == "" || finishReason == "stop" {
		return models.MessageComplete
	}
	return models.MessagePartial // "length" (the token limit) or anything unexpected
}

// PairHistory turns stored messages (oldest first) into question/answer pairs
// for the prompt: a question followed directly by a COMPLETE answer. Unanswered
// questions, cut-short answers and failed ones are skipped, because they would
// only confuse the model. At most maxTurns pairs are returned, the latest ones.
//
// It relies on questions and answers alternating. Step 6 therefore refuses a
// second question in a conversation while the first is still being answered.
func PairHistory(msgs []models.Message, maxTurns int) []HistoryTurn {
	var turns []HistoryTurn
	for i := 0; i+1 < len(msgs); i++ {
		q, a := msgs[i], msgs[i+1]
		if q.Role == models.RoleUser && a.Role == models.RoleAssistant && a.Status == models.MessageComplete &&
			strings.TrimSpace(q.Content) != "" && strings.TrimSpace(a.Content) != "" {
			turns = append(turns, HistoryTurn{Question: q.Content, Answer: a.Content})
			i++ // the answer is used up
		}
	}
	if maxTurns > 0 && len(turns) > maxTurns {
		turns = turns[len(turns)-maxTurns:]
	}
	return turns
}