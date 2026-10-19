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