package router_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/service"
)

type sumDoc struct {
	ID            string  `json:"id"`
	Summary       *string `json:"summary"`
	SummaryStatus string  `json:"summary_status"`
	SummaryError  *string `json:"summary_error"`
}

func decodeSumDoc(t *testing.T, res result) sumDoc {
	t.Helper()
	var d struct {
		Document sumDoc `json:"document"`
	}
	if err := json.Unmarshal(res.Data, &d); err != nil {
		t.Fatalf("decode: %v\n%s", err, res.Body)
	}
	return d.Document
}

func (s *stack) getSumDoc(t *testing.T, token, id string) sumDoc {
	t.Helper()
	res := s.do(t, http.MethodGet, "/api/documents/"+id, token, nil, "")
	if res.Code != http.StatusOK {
		t.Fatalf("get: status %d: %s", res.Code, res.Body)
	}
	return decodeSumDoc(t, res)
}

func (s *stack) requestSummary(t *testing.T, token, id string) result {
	t.Helper()
	return s.do(t, http.MethodPost, "/api/documents/"+id+"/summarize", token, nil, "")
}

// waitSummary polls until the summary leaves "processing".
func (s *stack) waitSummary(t *testing.T, token, id string) sumDoc {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		d := s.getSumDoc(t, token, id)
		if d.SummaryStatus != "processing" {
			return d
		}
		if time.Now().After(deadline) {
			t.Fatalf("the summary of %s is still processing after 30s", id)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestSummaryAPI_EndToEnd(t *testing.T) {
	s := newStack(t, 10<<20)
	id := s.readyDoc(t, s.alice)

	if d := s.getSumDoc(t, s.alice, id); d.SummaryStatus != "none" || d.Summary != nil {
		t.Fatalf("a new document: %+v", d)
	}

	res := s.requestSummary(t, s.alice, id)
	if res.Code != http.StatusAccepted {
		t.Fatalf("status %d: %s", res.Code, res.Body)
	}
	if d := decodeSumDoc(t, res); d.SummaryStatus != "processing" {
		t.Errorf("the 202 response shows summary_status %q, want processing", d.SummaryStatus)
	}

	d := s.waitSummary(t, s.alice, id)
	if d.SummaryStatus != "ready" || d.Summary == nil || *d.Summary != "A SHORT SUMMARY OF THE DOCUMENT." || d.SummaryError != nil {
		t.Fatalf("after the job: %+v", d)
	}
	if n := s.chat.summaryCalls(); n != 1 {
		t.Errorf("%d model calls for a short document, want 1", n)
	}

	// The list carries the status but not the text.
	list := s.do(t, http.MethodGet, "/api/documents", s.alice, nil, "")
	if strings.Contains(list.Body, "A SHORT SUMMARY") || !strings.Contains(list.Body, `"summary_status":"ready"`) {
		t.Errorf("list = %s", list.Body)
	}

	// Regenerate: the old text stays visible while the new one is written.
	gate := make(chan struct{})
	s.chat.configure(func(c *scriptedChat) {
		c.complete = func(ctx context.Context, _ int, req service.ChatRequest) (service.ChatResponse, error) {
			select {
			case <-gate:
			case <-ctx.Done():
				return service.ChatResponse{}, ctx.Err()
			}
			return service.ChatResponse{Content: "A NEW SUMMARY.", FinishReason: "stop", Model: req.Model}, nil
		}
	})
	if res := s.requestSummary(t, s.alice, id); res.Code != http.StatusAccepted {
		t.Fatalf("regenerate: status %d: %s", res.Code, res.Body)
	}
	mid := s.getSumDoc(t, s.alice, id)
	if mid.SummaryStatus != "processing" || mid.Summary == nil || *mid.Summary != "A SHORT SUMMARY OF THE DOCUMENT." {
		t.Errorf("while regenerating: %+v", mid)
	}

	// And a second request while one runs is refused.
	if res := s.requestSummary(t, s.alice, id); res.Code != http.StatusConflict || res.ErrCode != "SUMMARY_IN_PROGRESS" {
		t.Errorf("second request: status %d, code %q", res.Code, res.ErrCode)
	}

	close(gate)
	if d := s.waitSummary(t, s.alice, id); d.SummaryStatus != "ready" || d.Summary == nil || *d.Summary != "A NEW SUMMARY." {
		t.Errorf("after regenerating: %+v", d)
	}
}

func TestSummaryAPI_Refusals(t *testing.T) {
	s := newStack(t, 10<<20)
	id := s.readyDoc(t, s.alice)

	tests := []struct {
		name   string
		token  string
		doc    string
		status int
		code   string
	}{
		{"another user's document", s.bob, id, 404, "NOT_FOUND"},
		{"a malformed id", s.alice, "not-a-uuid", 404, "NOT_FOUND"},
		{"a document that does not exist", s.alice, "6f1c0c8e-2f4b-4f55-9d3e-0b1a2c3d4e5f", 404, "NOT_FOUND"},
		{"no token", "", id, 401, "UNAUTHORIZED"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if res := s.requestSummary(t, tc.token, tc.doc); res.Code != tc.status || res.ErrCode != tc.code {
				t.Errorf("status %d, code %q; want %d %s", res.Code, res.ErrCode, tc.status, tc.code)
			}
		})
	}

	if n := s.chat.summaryCalls(); n != 0 {
		t.Errorf("the model was called %d times for refused requests", n)
	}
	if d := s.getSumDoc(t, s.alice, id); d.SummaryStatus != "none" {
		t.Errorf("refused requests changed the document: %+v", d)
	}
}

func TestSummaryAPI_DocumentNotReady(t *testing.T) {
	s := newStackWith(t, 10<<20, 50, false) // ingestion workers never start
	doc := decodeDoc(t, s.upload(t, s.alice, "sample.txt", fixture(t, "sample.txt")))

	res := s.requestSummary(t, s.alice, doc.ID)
	if res.Code != http.StatusConflict || res.ErrCode != "DOCUMENT_NOT_READY" || !strings.Contains(res.Body, `"processing"`) {
		t.Errorf("status %d, code %q: %s", res.Code, res.ErrCode, res.Body)
	}
}

func TestSummaryAPI_FailureIsReportedSafely(t *testing.T) {
	s := newStack(t, 10<<20)
	id := s.readyDoc(t, s.alice)
	s.chat.configure(func(c *scriptedChat) {
		c.complete = func(context.Context, int, service.ChatRequest) (service.ChatResponse, error) {
			return service.ChatResponse{}, &service.LLMError{Kind: service.LLMBusy, Status: 429, Message: "secret provider text 10.0.0.5"}
		}
	})

	if res := s.requestSummary(t, s.alice, id); res.Code != http.StatusAccepted {
		t.Fatalf("status %d", res.Code)
	}
	d := s.waitSummary(t, s.alice, id)
	if d.SummaryStatus != "failed" || d.Summary != nil || d.SummaryError == nil ||
		!strings.Contains(*d.SummaryError, "too busy") {
		t.Fatalf("after the failure: %+v", d)
	}
	body := s.do(t, http.MethodGet, "/api/documents/"+id, s.alice, nil, "").Body
	if strings.Contains(body, "secret provider text") || strings.Contains(body, "10.0.0.5") {
		t.Errorf("the provider's message reached the client: %s", body)
	}

	// A later attempt works, and clears the error.
	s.chat.configure(func(c *scriptedChat) { c.complete = nil })
	if res := s.requestSummary(t, s.alice, id); res.Code != http.StatusAccepted {
		t.Fatalf("retry: status %d: %s", res.Code, res.Body)
	}
	if d := s.waitSummary(t, s.alice, id); d.SummaryStatus != "ready" || d.SummaryError != nil || d.Summary == nil {
		t.Errorf("after the retry: %+v", d)
	}
}