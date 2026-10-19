package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/models"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/parser"
)

// maxQueryTerms is the most question terms one search uses.
const maxQueryTerms = 12

// ChunkSearcher is what retrieval needs from the chunk repository.
type ChunkSearcher interface {
	QueryTerms(ctx context.Context, question string, maxTerms int) ([]string, error)
	SearchChunks(ctx context.Context, userID, documentID uuid.UUID, terms []string, limit int) ([]models.RetrievedChunk, error)
}

// Retrieval is what a search found.
type Retrieval struct {
	Terms  []string                // the question words that were searched for
	Chunks []models.RetrievedChunk // best first; empty when nothing matched
}

// Empty reports that nothing in the document matched the question. The ask
// flow then answers "I couldn't find that in this document" WITHOUT calling the
// model: that saves tokens, and it is honest.
func (r *Retrieval) Empty() bool { return len(r.Chunks) == 0 }

type RetrievalService struct {
	repo ChunkSearcher
	topK int
}

func NewRetrievalService(repo ChunkSearcher, topK int) (*RetrievalService, error) {
	if repo == nil {
		return nil, errors.New("retrieval: a searcher is required")
	}
	if topK < 1 {
		return nil, fmt.Errorf("retrieval: topK must be at least 1, got %d", topK)
	}
	return &RetrievalService{repo: repo, topK: topK}, nil
}

// Retrieve finds the chunks of one document that best match a question.
//
// The document must belong to userID and be ready, otherwise the error wraps
// models.ErrNotFound. A question with no usable words, or no matching chunk,
// is NOT an error: the result is simply Empty.
func (s *RetrievalService) Retrieve(ctx context.Context, userID, documentID uuid.UUID, question string) (*Retrieval, error) {
	// The same cleaning as the stored text: no NUL bytes or control characters
	// (Postgres rejects NUL), valid UTF-8, tidy whitespace.
	question = parser.Clean(question)

	terms, err := s.repo.QueryTerms(ctx, question, maxQueryTerms)
	if err != nil {
		return nil, fmt.Errorf("retrieve: %w", err)
	}

	// Called even when there are no terms, so that a document that is missing,
	// someone else's, or not ready is reported the same way in every case.
	chunks, err := s.repo.SearchChunks(ctx, userID, documentID, terms, s.topK)
	if err != nil {
		return nil, fmt.Errorf("retrieve: %w", err) // models.ErrNotFound stays detectable
	}
	return &Retrieval{Terms: terms, Chunks: chunks}, nil
}