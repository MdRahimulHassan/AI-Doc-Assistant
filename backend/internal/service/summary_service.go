package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/models"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/parser"
)

const (
	// DefaultSummaryTimeout bounds one whole job. On the free plan a long
	// document takes minutes, because the token budget paces every call.
	DefaultSummaryTimeout = 30 * time.Minute

	// SummaryQueueSize is how many summary jobs may wait for a worker.
	SummaryQueueSize = 20

	// summaryCallMaxWait is how long ONE call may wait for token budget or for a
	// Retry-After. A background job is patient; an interactive question is not.
	summaryCallMaxWait = 10 * time.Minute

	summaryMaxLayers         = 8
	summaryRetryOutputFactor = 2     // a cut-off call is retried once with this many times the output limit
	maxSummaryChars          = 20000 // safety cap on the stored summary
)

var (
	errSummaryEmpty      = errors.New("the model returned an empty summary")
	errSummaryCutOff     = errors.New("the model's summary was cut off, even with a larger limit")
	errSummaryNoProgress = errors.New("summarizing is not shrinking the text")
	errSummaryNoText     = errors.New("the document has no text to summarize")
)

// SummaryStore is what the service needs from the document repository.
type SummaryStore interface {
	StartSummary(ctx context.Context, userID, id uuid.UUID, maxChunks int) (*models.SummaryStart, error)
	RevertSummaryStart(ctx context.Context, userID, id uuid.UUID, previous models.SummaryStatus, previousError *string) error
	GetForSummary(ctx context.Context, id uuid.UUID) (*models.Document, error)
	CompleteSummary(ctx context.Context, id uuid.UUID, summary string) error
	FailSummary(ctx context.Context, id uuid.UUID, safeMessage string) error
}

// SummaryChunks reads a document's chunk texts in reading order.
type SummaryChunks interface {
	ChunkTexts(ctx context.Context, documentID uuid.UUID, limit int) ([]string, error)
}

type SummaryDeps struct {
	Store  SummaryStore
	Chunks SummaryChunks
	Jobs   Ingester   // the queue the worker pool reads from
	LLM    ChatClient // nil means the AI features are disabled (no API key)
}

type SummarySettings struct {
	Model           string
	GroupTokens     int           // most estimated tokens of document text sent in one call
	MaxOutputTokens int           // per call; a cut-off call is retried once with double
	MaxChunks       int           // longer documents are refused
	Timeout         time.Duration // one whole job; 0 means DefaultSummaryTimeout
}

type SummaryService struct {
	store  SummaryStore
	chunks SummaryChunks
	jobs   Ingester
	llm    ChatClient
	cfg    SummarySettings
}

func NewSummaryService(d SummaryDeps, cfg SummarySettings) (*SummaryService, error) {
	switch {
	case d.Store == nil || d.Chunks == nil || d.Jobs == nil:
		return nil, errors.New("summary: a store, a chunk reader and a job queue are required")
	case cfg.Model == "":
		return nil, errors.New("summary: a model is required")
	case cfg.GroupTokens < 100:
		return nil, fmt.Errorf("summary: GroupTokens must be at least 100, got %d", cfg.GroupTokens)
	case cfg.MaxOutputTokens < 1:
		return nil, errors.New("summary: MaxOutputTokens must be set")
	case cfg.MaxChunks < 1:
		return nil, errors.New("summary: MaxChunks must be set")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultSummaryTimeout
	}
	return &SummaryService{store: d.Store, chunks: d.Chunks, jobs: d.Jobs, llm: d.LLM, cfg: cfg}, nil
}

// ---------------------------------------------------------------------------
// requesting a summary (called by the HTTP handler)
// ---------------------------------------------------------------------------

// Request claims the document, queues the job and returns at once with the
// document in state 'processing'. The work happens in a background worker.
//
// Errors, all before anything is queued: models.ErrNotFound (missing, or not the
// user's), *models.DocumentNotReadyError, models.ErrDocumentTooLong,
// models.ErrSummaryInProgress, ErrBusy (the queue is full: the claim is undone),
// ErrLLMUnavailable (no API key).
func (s *SummaryService) Request(ctx context.Context, userID, docID uuid.UUID) (*models.Document, error) {
	if s.llm == nil {
		return nil, ErrLLMUnavailable
	}

	start, err := s.store.StartSummary(ctx, userID, docID, s.cfg.MaxChunks)
	if err != nil {
		return nil, fmt.Errorf("summarize: %w", err)
	}

	if err := s.jobs.Submit(docID); err != nil {
		// Put the document back exactly as it was, so a busy server never turns
		// a good summary into a failed one.
		rctx, cancel := persistContext(ctx)
		defer cancel()
		if rerr := s.store.RevertSummaryStart(rctx, userID, docID, start.PreviousStatus, start.PreviousError); rerr != nil &&
			!errors.Is(rerr, models.ErrNotFound) {
			slog.Error("could not undo the summary claim after the queue refused the job; "+
				"the document stays 'processing' until the next start",
				"document_id", docID, "error", rerr)
		}
		return nil, fmt.Errorf("summarize: %w", err)
	}
	return start.Document, nil
}

// ---------------------------------------------------------------------------
// the background job
// ---------------------------------------------------------------------------

// Run writes the summary of a document claimed with Request. It records its own
// outcome (ready or failed) and logs its own failures; the returned error is for
// callers that want to react, and must not be shown to a user or logged again.
//
//   - A document that is gone, or no longer 'processing', is a quiet no-op.
//   - If parent is cancelled (shutdown) the document is LEFT as 'processing', so
//     the next start runs the job again.
//   - A job that exceeds its own timeout is marked failed.
func (s *SummaryService) Run(parent context.Context, docID uuid.UUID) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("summary panic", "document_id", docID, "panic", rec, "stack", string(debug.Stack()))
			s.failAfterPanic(parent, docID)
			err = fmt.Errorf("summary %s: %w: %v", docID, errPanic, rec)
		}
	}()

	start := time.Now()
	doc, err := s.store.GetForSummary(parent, docID)
	switch {
	case errors.Is(err, models.ErrNotFound):
		slog.Info("summary skipped: the document no longer exists", "document_id", docID)
		return nil
	case err != nil:
		slog.Error("summary failed: could not load the document", "document_id", docID, "error", err)
		return fmt.Errorf("summary %s: load document: %w", docID, err)
	}
	if doc.SummaryStatus != models.SummaryProcessing {
		slog.Info("summary skipped: no summary is being written for this document",
			"document_id", docID, "summary_status", doc.SummaryStatus)
		return nil
	}
	if s.llm == nil {
		return s.recordFailure(parent, doc, ErrLLMUnavailable)
	}

	ctx, cancel := context.WithTimeout(parent, s.cfg.Timeout)
	defer cancel()

	summary, err := s.write(ctx, doc)
	if err != nil {
		return s.recordFailure(parent, doc, err)
	}

	// Saved with a detached context: the job's tokens are already spent, so a
	// finished summary is stored even if shutdown has just begun.
	wctx, wcancel := persistContext(parent)
	defer wcancel()
	switch err := s.store.CompleteSummary(wctx, docID, summary); {
	case err == nil:
	case errors.Is(err, models.ErrNotFound):
		slog.Info("summary abandoned: the document was deleted while it was being written", "document_id", docID)
		return nil
	default:
		return s.recordFailure(parent, doc, fmt.Errorf("store summary: %w", err))
	}

	slog.Info("summary finished", "document_id", docID, "chars", utf8.RuneCountInString(summary),
		"took", time.Since(start).Round(time.Second), "prompt_version", SummaryPromptVersion)
	return nil
}

// write reads the chunks, plans the work and produces the summary text.
func (s *SummaryService) write(ctx context.Context, doc *models.Document) (string, error) {
	if doc.Status != models.StatusReady {
		return "", &models.DocumentNotReadyError{Status: doc.Status}
	}

	// One more than the limit is read, to learn whether the limit was passed.
	chunks, err := s.chunks.ChunkTexts(ctx, doc.ID, s.cfg.MaxChunks+1)
	if err != nil {
		return "", fmt.Errorf("read chunks: %w", err)
	}
	if len(chunks) > s.cfg.MaxChunks {
		return "", models.ErrDocumentTooLong
	}

	plan := planSummary(chunks, s.cfg.GroupTokens)
	if len(plan.Items) == 0 {
		return "", errSummaryNoText
	}
	est := plan.estimate(s.cfg.MaxOutputTokens)
	slog.Info("summary planned",
		"document_id", doc.ID, "model", s.cfg.Model, "prompt_version", SummaryPromptVersion,
		"chunks", est.Chunks, "items", est.Items, "first_layer_calls", est.MapCalls, "calls_at_least", est.Calls,
		"input_tokens_estimated", est.InputTokens, "tokens_estimated_total", est.Tokens, "group_tokens", s.cfg.GroupTokens)

	summary, err := s.reduce(ctx, plan)
	if err != nil {
		return "", err
	}
	return capRunes(summary, maxSummaryChars), nil
}

// reduce is the map-reduce loop. Each layer condenses groups of items into one
// summary each; when what is left fits ONE prompt, the final call writes the
// finished summary. A failed call fails everything: a summary with a silent gap
// is never produced.
func (s *SummaryService) reduce(ctx context.Context, plan summaryPlan) (string, error) {
	level, groups := plan.Items, plan.Groups
	budget := s.cfg.GroupTokens

	for layer := 0; ; layer++ {
		if len(groups) == 1 {
			return s.call(ctx, summaryFinalPrompt, groups[0], layer, 1, 1)
		}
		// Safety nets (the config guarantees progress): stop if a layer would
		// not reduce the number of items, or if there are absurdly many layers.
		if layer >= summaryMaxLayers || (layer > 0 && len(groups) >= len(level)) {
			return "", errSummaryNoProgress
		}

		next := make([]string, 0, len(groups))
		for i, g := range groups {
			part, err := s.call(ctx, summaryPartPrompt, g, layer, i+1, len(groups))
			if err != nil {
				return "", fmt.Errorf("layer %d, group %d of %d: %w", layer, i+1, len(groups), err)
			}
			next = append(next, part)
		}
		level = prepareItems(next, budget)
		groups = groupItems(level, budget)
	}
}

// call makes one model call. A cut-off answer (finish reason "length") is
// retried ONCE with double the output limit; a second cut-off fails. The pacer
// inside the client spaces the calls out, and the long MaxWait lets a
// background job wait for budget instead of giving up.
func (s *SummaryService) call(ctx context.Context, system string, items []string, layer, n, total int) (string, error) {
	msgs := []ChatMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: renderParts(items)},
	}
	estimated := EstimateMessagesTokens(msgs)

	maxOut := s.cfg.MaxOutputTokens
	for attempt := 1; ; attempt++ {
		resp, err := s.llm.Complete(ctx, ChatRequest{
			Model:                 s.cfg.Model,
			Messages:              msgs,
			MaxOutputTokens:       maxOut,
			Temperature:           summaryTemperature,
			ReasoningEffort:       summaryReasoningEffort,
			EstimatedPromptTokens: estimated,
			MaxWait:               summaryCallMaxWait,
		})
		if err != nil {
			return "", err
		}

		text := parser.Clean(resp.Content)
		slog.Info("summary call finished",
			"layer", layer, "part", n, "of", total, "attempt", attempt, "finish_reason", resp.FinishReason,
			"tokens_estimated", estimated, "tokens_prompt", resp.Usage.PromptTokens,
			"tokens_completion", resp.Usage.CompletionTokens, "tokens_reasoning", resp.Usage.ReasoningTokens)

		switch {
		case resp.FinishReason == "length":
			if attempt == 1 {
				maxOut *= summaryRetryOutputFactor
				continue
			}
			return "", errSummaryCutOff
		case text == "":
			return "", errSummaryEmpty
		}
		return text, nil
	}
}

// recordFailure logs the full error and stores the safe message. It returns the
// original cause.
func (s *SummaryService) recordFailure(parent context.Context, doc *models.Document, cause error) error {
	// Shutdown is not a failure of the document: leave it 'processing' so the
	// next start runs the job again.
	if parent.Err() != nil {
		slog.Warn("summary interrupted; leaving the document as processing so it can be retried",
			"document_id", doc.ID, "reason", parent.Err())
		return fmt.Errorf("summary %s: interrupted: %w", doc.ID, cause)
	}

	msg, expected := summaryFailure(cause)
	attrs := []any{"document_id", doc.ID, "stored_message", msg, "error", cause} // the FULL error: logs only
	if expected {
		slog.Warn("summary failed", attrs...)
	} else {
		slog.Error("summary failed", attrs...)
	}

	wctx, cancel := persistContext(parent)
	defer cancel()
	switch err := s.store.FailSummary(wctx, doc.ID, msg); {
	case err == nil:
	case errors.Is(err, models.ErrNotFound):
		slog.Info("the document was deleted before the failure could be recorded", "document_id", doc.ID)
	default:
		slog.Error("could not record the summary failure; the document stays 'processing'",
			"document_id", doc.ID, "error", err)
	}
	return fmt.Errorf("summary %s: %w", doc.ID, cause)
}

func (s *SummaryService) failAfterPanic(parent context.Context, docID uuid.UUID) {
	if parent.Err() != nil {
		return
	}
	wctx, cancel := persistContext(parent)
	defer cancel()
	if err := s.store.FailSummary(wctx, docID, msgSummaryFailed); err != nil && !errors.Is(err, models.ErrNotFound) {
		slog.Error("could not record the failure after a panic", "document_id", docID, "error", err)
	}
}

// summaryFailure maps any error to the fixed message a user may see. Anything
// unrecognized becomes the generic message: raw errors never get through.
func summaryFailure(err error) (message string, expected bool) {
	var le *LLMError
	switch {
	case errors.Is(err, models.ErrDocumentTooLong):
		return msgSummaryTooLong, true
	case errors.Is(err, errSummaryNoText):
		return msgSummaryNoText, true
	case errors.Is(err, ErrLLMUnavailable):
		return msgSummaryUnavailable, true
	case errors.As(err, &le):
		switch le.Kind {
		case LLMBusy:
			return msgSummaryBusy, true
		case LLMTimeout:
			return msgSummaryTimeout, true
		case LLMUnavailable:
			return msgSummaryUnavailable, true
		default: // our key, model or request is wrong: OUR problem
			return msgSummaryFailed, false
		}
	case errors.Is(err, context.DeadlineExceeded):
		return msgSummaryTimeout, true
	case errors.Is(err, errSummaryEmpty), errors.Is(err, errSummaryCutOff), errors.Is(err, errSummaryNoProgress):
		return msgSummaryCutOff, false
	default:
		return msgSummaryFailed, false
	}
}

func capRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
}

// SummaryRunner adapts a SummaryService to the queue's runner interface. The
// queue and the service need each other (the service submits jobs to the queue,
// the queue runs them on the service), so: build the runner, then the queue,
// then the service, then Bind. The method is called Ingest only because that is
// what the shared queue's interface calls it.
type SummaryRunner struct{ svc atomic.Pointer[SummaryService] }

func (r *SummaryRunner) Bind(s *SummaryService) { r.svc.Store(s) }

func (r *SummaryRunner) Ingest(ctx context.Context, id uuid.UUID) error {
	s := r.svc.Load()
	if s == nil {
		return errors.New("the summary runner is not bound to a service")
	}
	return s.Run(ctx, id)
}