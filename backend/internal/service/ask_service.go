package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/models"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/parser"
)

const sourcePreviewChars = 200

// The codes carried by "error" events. They are the same strings as the
// response.Code* constants used for plain JSON errors (a handler test checks).
const (
	AskCodeBusy         = "LLM_BUSY"
	AskCodeTimeout      = "LLM_TIMEOUT"
	AskCodeUnavailable  = "LLM_UNAVAILABLE"
	AskCodeError        = "LLM_ERROR"
	AskCodeInternal     = "INTERNAL_ERROR"
	AskCodeShuttingDown = "SERVER_SHUTTING_DOWN"
)

// Errors returned BEFORE the first event is emitted. The handler turns each
// into a proper HTTP status. (An empty or too-long question is reported with
// the prompt package's ErrEmptyQuestion and ErrQuestionTooLong.)
var (
	ErrAnswerInProgress = errors.New("an answer to an earlier question is still being written")
	ErrLLMUnavailable   = errors.New("the AI features are not configured")

	errClientGone = errors.New("the client went away")
)

// DocumentNotReadyError says why a document cannot be asked about yet.
type DocumentNotReadyError struct{ Status models.DocumentStatus }

func (e *DocumentNotReadyError) Error() string {
	return "the document is not ready (" + string(e.Status) + ")"
}

// RateLimitError is returned when a user is asking too fast. The per-user
// limiter of Step 9 produces it.
type RateLimitError struct{ RetryAfter time.Duration }

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("rate limited: retry after %v", e.RetryAfter)
}

type AskEventKind int

const (
	EventSources AskEventKind = iota + 1 // first: which passages the answer is based on
	EventToken                           // a piece of the answer
	EventDone                            // the answer is complete and saved
	EventError                           // something failed after streaming began
)

// SourceInfo describes one passage given to the model.
type SourceInfo struct {
	Label      string // "P1": how the answer cites it
	ChunkIndex int
	Preview    string // the start of the passage, for display
	Truncated  bool   // the passage was cut to fit the token budget
}

// AskEvent is one step of an answer. Which fields are set depends on Kind.
type AskEvent struct {
	Kind         AskEventKind
	Sources      []SourceInfo         // EventSources (never nil)
	Text         string               // EventToken
	MessageID    int64                // EventDone, and EventError when the answer was saved
	Status       models.MessageStatus // EventDone
	FinishReason string               // EventDone
	Code         string               // EventError
	Message      string               // EventError: safe to show to a user
	RetryAfter   time.Duration        // EventError (busy)
}

// AskRequest is one question about one document.
type AskRequest struct {
	UserID     uuid.UUID
	DocumentID uuid.UUID
	Question   string
	RequestID  string // for log correlation only
}

type AskSettings struct {
	Model            string
	MaxOutputTokens  int
	ContextBudget    int // tokens for the retrieved passages
	MaxQuestionChars int
}

// What AskService needs from the rest of the app. The real types satisfy them.
type AskDocuments interface {
	Get(ctx context.Context, userID, id uuid.UUID) (*models.Document, error)
}

type AskRetriever interface {
	Retrieve(ctx context.Context, userID, documentID uuid.UUID, question string) (*Retrieval, error)
}

type AskConversations interface {
	Open(ctx context.Context, userID, documentID uuid.UUID) (*models.Conversation, error)
	History(ctx context.Context, userID, documentID uuid.UUID) ([]HistoryTurn, error)
	RecordQuestion(ctx context.Context, userID, conversationID uuid.UUID, question string) (*models.Message, error)
	RecordAnswer(ctx context.Context, userID, conversationID uuid.UUID, a Answer) (*models.Message, error)
}

var (
	_ AskDocuments     = (*DocumentService)(nil)
	_ AskRetriever     = (*RetrievalService)(nil)
	_ AskConversations = (*ConversationService)(nil)
)

type AskDeps struct {
	Documents     AskDocuments
	Retriever     AskRetriever
	Conversations AskConversations
	LLM           ChatClient      // nil means the AI features are disabled (no API key)
	Shutdown      context.Context // cancelled when the server starts to stop; nil means never
}

type AskService struct {
	docs      AskDocuments
	retriever AskRetriever
	convs     AskConversations
	llm       ChatClient
	shutdown  context.Context
	cfg       AskSettings
	guard     *conversationGuard
}

func NewAskService(d AskDeps, cfg AskSettings) (*AskService, error) {
	switch {
	case d.Documents == nil || d.Retriever == nil || d.Conversations == nil:
		return nil, errors.New("ask: documents, retriever and conversations are required")
	case strings.TrimSpace(cfg.Model) == "":
		return nil, errors.New("ask: a model is required")
	case cfg.MaxOutputTokens < 1:
		return nil, errors.New("ask: MaxOutputTokens must be set")
	case cfg.ContextBudget < minContextBudget:
		return nil, fmt.Errorf("ask: ContextBudget must be at least %d tokens", minContextBudget)
	case cfg.MaxQuestionChars < 1:
		return nil, errors.New("ask: MaxQuestionChars must be set")
	}
	shutdown := d.Shutdown
	if shutdown == nil {
		shutdown = context.Background()
	}
	return &AskService{
		docs: d.Documents, retriever: d.Retriever, convs: d.Conversations,
		llm: d.LLM, shutdown: shutdown, cfg: cfg, guard: newConversationGuard(),
	}, nil
}

// Ask answers one question about a document, reporting progress through emit.
//
//   - An error is returned only if it happens BEFORE the first event: the
//     caller can still answer with a proper HTTP status.
//   - From the first event on, failures become "error" events and Ask returns
//     nil. If emit fails the client is gone: the model call is stopped and the
//     partial answer is still saved.
func (s *AskService) Ask(ctx context.Context, req AskRequest, emit func(AskEvent) error) error {
	if s.llm == nil {
		return ErrLLMUnavailable
	}

	question := parser.Clean(req.Question)
	switch {
	case question == "":
		return ErrEmptyQuestion
	case utf8.RuneCountInString(question) > s.cfg.MaxQuestionChars:
		return ErrQuestionTooLong
	}

	doc, err := s.docs.Get(ctx, req.UserID, req.DocumentID)
	if err != nil {
		return fmt.Errorf("ask: %w", err) // models.ErrNotFound stays detectable
	}
	if doc.Status != models.StatusReady {
		return &DocumentNotReadyError{Status: doc.Status}
	}

	conv, err := s.convs.Open(ctx, req.UserID, req.DocumentID)
	if err != nil {
		return fmt.Errorf("ask: %w", err)
	}

	// One answer at a time per conversation: the history pairs each question
	// with the answer that follows it.
	release, ok := s.guard.tryAcquire(conv.ID)
	if !ok {
		return ErrAnswerInProgress
	}
	defer release()

	history, err := s.convs.History(ctx, req.UserID, req.DocumentID)
	if err != nil { // history is a nicety: answer without it
		slog.Warn("could not load the conversation history; answering without it",
			"request_id", req.RequestID, "document_id", req.DocumentID, "error", err)
		history = nil
	}

	retrieval, err := s.retriever.Retrieve(ctx, req.UserID, req.DocumentID, question)
	if err != nil {
		return fmt.Errorf("ask: %w", err)
	}

	var prompt *Prompt
	if !retrieval.Empty() {
		prompt, err = BuildPrompt(PromptInput{
			Question:         question,
			Chunks:           retrieval.Chunks,
			History:          history,
			Model:            s.cfg.Model,
			MaxOutputTokens:  s.cfg.MaxOutputTokens,
			ContextBudget:    s.cfg.ContextBudget,
			MaxQuestionChars: s.cfg.MaxQuestionChars,
		})
		switch {
		case errors.Is(err, ErrNoPassages): // only blank chunks: the same as finding nothing
			prompt = nil
		case err != nil:
			return fmt.Errorf("ask: build prompt: %w", err)
		}
	}

	// The question is saved BEFORE the model is called, so a crash cannot lose
	// it. This uses the request context: a client that is already gone gets
	// no orphan question.
	if _, err := s.convs.RecordQuestion(ctx, req.UserID, conv.ID, question); err != nil {
		return fmt.Errorf("ask: %w", err)
	}

	run := &askRun{svc: s, ctx: ctx, req: req, conv: conv, emit: emit, release: release}
	if prompt == nil {
		run.notFound()
	} else {
		run.answer(prompt, retrieval)
	}
	return nil
}

// askRun is one question's progress after the first event is possible.
type askRun struct {
	svc     *AskService
	ctx     context.Context
	req     AskRequest
	conv    *models.Conversation
	emit    func(AskEvent) error
	release func() // frees the conversation; safe to call more than once
	gone    bool   // emit failed: the client is no longer there
}

// send delivers an event unless the client is already known to be gone.
func (r *askRun) send(ev AskEvent) {
	if r.gone {
		return
	}
	if err := r.emit(ev); err != nil {
		r.gone = true
	}
}

// persist saves the answer (RecordAnswer uses a detached context). It returns
// nil if saving failed; that is logged here.
func (r *askRun) persist(a Answer) *models.Message {
	saved, err := r.svc.convs.RecordAnswer(r.ctx, r.req.UserID, r.conv.ID, a)
	if err != nil {
		slog.Error("could not save the answer",
			"request_id", r.req.RequestID, "document_id", r.req.DocumentID,
			"status", a.Status, "answer_chars", utf8.RuneCountInString(a.Content), "error", err)
		return nil
	}
	return saved
}

// notFound answers "nothing in the document matches" without calling the model.
func (r *askRun) notFound() {
	r.send(AskEvent{Kind: EventSources, Sources: []SourceInfo{}})
	r.send(AskEvent{Kind: EventToken, Text: NotFoundAnswer})

	saved := r.persist(Answer{Content: NotFoundAnswer, Status: models.MessageComplete, Sources: []int{}})
	r.release() // before the last event, so the next question is never refused
	slog.Info("ask answered without calling the model: no passage matched",
		"request_id", r.req.RequestID, "document_id", r.req.DocumentID)

	if saved == nil {
		r.send(AskEvent{Kind: EventError, Code: AskCodeInternal,
			Message: "The answer was shown but could not be saved to your history."})
		return
	}
	r.send(AskEvent{Kind: EventDone, MessageID: saved.ID, Status: models.MessageComplete})
}

func (r *askRun) answer(p *Prompt, retrieval *Retrieval) {
	s := r.svc
	indexes := sourceChunkIndexes(p.Sources)

	// The first event. Until now a failure was a plain error with a proper HTTP
	// status; from here on it is an "error" event.
	r.send(AskEvent{Kind: EventSources, Sources: sourceInfos(p, retrieval)})
	if r.gone {
		// The client left before the answer started: nothing was generated.
		// An empty, cut-short answer keeps the history tidy.
		r.persist(Answer{Status: models.MessagePartial, Sources: indexes, Model: p.Request.Model})
		r.release()
		slog.Info("ask abandoned: the client left before the answer started",
			"request_id", r.req.RequestID, "document_id", r.req.DocumentID)
		return
	}

	// The model call ends when the client leaves (the request context) or when
	// the server starts to stop (the shutdown context).
	sctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	stop := context.AfterFunc(s.shutdown, cancel)
	defer stop()

	started := time.Now()
	var firstToken time.Duration
	resp, streamErr := s.llm.Stream(sctx, p.Request, func(text string) error {
		if firstToken == 0 {
			firstToken = time.Since(started)
		}
		if err := r.emit(AskEvent{Kind: EventToken, Text: text}); err != nil {
			r.gone = true
			return fmt.Errorf("%w: %v", errClientGone, err) // stops the model call
		}
		return nil
	})

	status := ClassifyAnswer(resp.Content, resp.FinishReason, streamErr)
	model := resp.Model
	if model == "" {
		model = p.Request.Model
	}
	saved := r.persist(Answer{
		Content: resp.Content, Status: status, Sources: indexes, Model: model, Usage: resp.Usage,
	})
	r.release() // before the last event, so the next question is never refused
	r.logAnswer(p, resp, status, streamErr, firstToken, time.Since(started))

	switch {
	case errors.Is(streamErr, errClientGone):
		return // nobody to tell
	case streamErr != nil && s.shutdown.Err() != nil:
		r.send(AskEvent{Kind: EventError, Code: AskCodeShuttingDown,
			Message: "The server is restarting. Please ask again in a moment.", MessageID: messageID(saved)})
	case streamErr != nil && r.ctx.Err() != nil:
		return // the client left; nobody to tell
	case streamErr != nil:
		f := streamFailure(streamErr)
		r.send(AskEvent{Kind: EventError, Code: f.code, Message: f.message,
			RetryAfter: f.retryAfter, MessageID: messageID(saved)})
	case saved == nil:
		r.send(AskEvent{Kind: EventError, Code: AskCodeInternal,
			Message: "The answer was shown but could not be saved to your history."})
	case status == models.MessageError:
		r.send(AskEvent{Kind: EventError, Code: AskCodeError,
			Message: "The AI service returned no answer. Please try again.", MessageID: saved.ID})
	default:
		r.send(AskEvent{Kind: EventDone, MessageID: saved.ID, Status: status, FinishReason: resp.FinishReason})
	}
}

// logAnswer records one line about the answer: sizes, tokens, timing and
// status, never the question or the answer text.
func (r *askRun) logAnswer(p *Prompt, resp ChatResponse, status models.MessageStatus, streamErr error, firstToken, took time.Duration) {
	valid, invalid := Citations(resp.Content, len(p.Sources))
	attrs := []any{
		"request_id", r.req.RequestID,
		"document_id", r.req.DocumentID,
		"conversation_id", r.conv.ID,
		"status", status,
		"finish_reason", resp.FinishReason,
		"prompt_version", p.Version,
		"passages", len(p.Sources),
		"dropped_chunks", p.DroppedChunks,
		"tokens_estimated", p.EstimatedTokens, // compare with tokens_prompt to calibrate EstimateTokens
		"tokens_prompt", resp.Usage.PromptTokens,
		"tokens_completion", resp.Usage.CompletionTokens,
		"tokens_reasoning", resp.Usage.ReasoningTokens,
		"answer_chars", utf8.RuneCountInString(resp.Content),
		"citations", len(valid),
		"first_token", firstToken.Round(time.Millisecond),
		"took", took.Round(time.Millisecond),
	}
	switch {
	case streamErr == nil:
		slog.Info("ask finished", attrs...)
	case streamFailure(streamErr).expected || errors.Is(streamErr, errClientGone) || r.ctx.Err() != nil || r.svc.shutdown.Err() != nil:
		slog.Warn("ask ended early", append(attrs, "error", streamErr)...)
	default:
		slog.Error("ask failed", append(attrs, "error", streamErr)...)
	}
	if len(invalid) > 0 {
		slog.Warn("the answer cites passages that were never provided",
			"request_id", r.req.RequestID, "invalid_citations", len(invalid))
	}
}

// failure is what a client is told about a failed model call.
type askFailure struct {
	code       string
	message    string
	retryAfter time.Duration
	expected   bool // a normal outcome (busy, timeout), as opposed to our own problem
}

// streamFailure maps a model-call error to a safe message and a code. The
// provider's own message is never included: it is for logs only.
func streamFailure(err error) askFailure {
	var le *LLMError
	switch {
	case errors.As(err, &le):
		switch le.Kind {
		case LLMBusy:
			return askFailure{AskCodeBusy, "The AI service is busy right now. Please try again in a moment.", le.RetryAfter, true}
		case LLMTimeout:
			return askFailure{AskCodeTimeout, "The AI service took too long to answer. Please try again.", 0, true}
		case LLMUnavailable:
			return askFailure{AskCodeUnavailable, "The AI service is temporarily unavailable. Please try again shortly.", 0, true}
		default: // misconfigured, bad request, bad response: OUR problem
			return askFailure{AskCodeError, "The AI service could not answer this question.", 0, false}
		}
	case errors.Is(err, context.DeadlineExceeded):
		return askFailure{AskCodeTimeout, "The AI service took too long to answer. Please try again.", 0, true}
	default:
		return askFailure{AskCodeInternal, "Something went wrong while answering.", 0, false}
	}
}

func messageID(m *models.Message) int64 {
	if m == nil {
		return 0
	}
	return m.ID
}

func sourceChunkIndexes(sources []PromptSource) []int {
	out := make([]int, 0, len(sources))
	for _, s := range sources {
		out = append(out, s.ChunkIndex)
	}
	return out
}

func sourceInfos(p *Prompt, r *Retrieval) []SourceInfo {
	content := make(map[int]string, len(r.Chunks))
	for _, c := range r.Chunks {
		content[c.Index] = c.Content
	}
	out := make([]SourceInfo, 0, len(p.Sources))
	for _, s := range p.Sources {
		out = append(out, SourceInfo{
			Label:      s.Label,
			ChunkIndex: s.ChunkIndex,
			Preview:    previewText(content[s.ChunkIndex], sourcePreviewChars),
			Truncated:  s.Truncated,
		})
	}
	return out
}

// previewText flattens whitespace and cuts at max characters (never mid-character).
func previewText(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// conversationGuard lets one answer at a time be written per conversation.
type conversationGuard struct {
	mu   sync.Mutex
	busy map[uuid.UUID]struct{}
}

func newConversationGuard() *conversationGuard {
	return &conversationGuard{busy: make(map[uuid.UUID]struct{})}
}

// tryAcquire returns a release function, or false if the conversation is busy.
// Calling release more than once is harmless.
func (g *conversationGuard) tryAcquire(id uuid.UUID) (release func(), ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, taken := g.busy[id]; taken {
		return nil, false
	}
	g.busy[id] = struct{}{}

	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			delete(g.busy, id)
			g.mu.Unlock()
		})
	}, true
}