package service

import (
	"context"

	"github.com/google/uuid"
)

// InlineIngester processes a document synchronously, inside the upload request.
//
// TEMPORARY (Step 7): it exists so the whole pipeline can be exercised through
// HTTP before any concurrency is involved. Step 8 replaces it with a bounded
// background worker pool; nothing else has to change.
type InlineIngester struct{ svc *IngestService }

func NewInlineIngester(svc *IngestService) *InlineIngester { return &InlineIngester{svc: svc} }

func (i *InlineIngester) Submit(docID uuid.UUID) error {
	// Ingest records its own outcome (ready or failed) and logs failures. A
	// failed DOCUMENT is not a failed submission, so the error is not returned.
	_ = i.svc.Ingest(context.WithoutCancel(context.Background()), docID)
	return nil
}