package models

import (
	"time"

	"github.com/google/uuid"
)

// DocumentStatus mirrors the CHECK constraint on documents.status.
// (Named DocumentStatus rather than Status so it stays unambiguous in this package.)
type DocumentStatus string

const (
	StatusProcessing DocumentStatus = "processing"
	StatusReady      DocumentStatus = "ready"
	StatusFailed     DocumentStatus = "failed"
)

// Document is the database model. API responses will use a separate DTO
// (Step 7), but UserID and StoragePath are hidden here too, as a second
// line of defense against leaking them.
type Document struct {
	ID           uuid.UUID      `json:"id"`
	UserID       uuid.UUID      `json:"-"`
	Filename     string         `json:"filename"`
	MimeType     string         `json:"mime_type"`
	SizeBytes    int64          `json:"size_bytes"`
	StoragePath  string         `json:"-"`
	Status       DocumentStatus `json:"status"`
	ErrorMessage *string        `json:"error_message,omitempty"` // nil unless failed
	ChunkCount   int            `json:"chunk_count"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
	Summary          *string       `json:"-"`
	SummaryStatus    SummaryStatus `json:"-"`
	SummaryError     *string       `json:"-"`
	SummaryUpdatedAt *time.Time    `json:"-"`
}

// Chunk is one searchable piece of a document's text. The retrieval
// service (Week 3) returns these. Ingestion only passes plain strings
// to the repository, which assigns the indexes itself.
type Chunk struct {
	ID         int64     `json:"id"`
	DocumentID uuid.UUID `json:"document_id"`
	Index      int       `json:"index"`
	Content    string    `json:"content"`
}

// RetrievedChunk is one search result: a chunk plus how well it matched.
type RetrievedChunk struct {
	Index        int     // the chunk's position in its document (chunks.chunk_index)
	Content      string
	Score        float64 // higher is better; comparable only within one search
	MatchedTerms int     // how many of the question's terms the chunk contains (0 = not measured)
}

// SummaryStatus is the state of a document's summary (migration 000005).
type SummaryStatus string

const (
	SummaryNone       SummaryStatus = "none"
	SummaryProcessing SummaryStatus = "processing"
	SummaryReady      SummaryStatus = "ready"
	SummaryFailed     SummaryStatus = "failed"
)

// SummaryStart is the result of claiming a document for summarizing. The
// previous state is kept so a refused hand-off can put the document back exactly
// as it was.
type SummaryStart struct {
	Document       *Document
	PreviousStatus SummaryStatus
	PreviousError  *string
}

// DocumentNotReadyError says why a document cannot be used yet. It lives here
// (not in the service package) because the repository returns it too.
type DocumentNotReadyError struct{ Status DocumentStatus }

func (e *DocumentNotReadyError) Error() string {
	return "the document is not ready (" + string(e.Status) + ")"
}