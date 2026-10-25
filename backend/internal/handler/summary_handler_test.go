package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/middleware"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/models"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/service"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/pkg/response"
)

type fakeSummarizer struct {
	doc *models.Document
	err error

	calls   int
	gotUser uuid.UUID
	gotDoc  uuid.UUID
}

func (f *fakeSummarizer) Request(_ context.Context, userID, docID uuid.UUID) (*models.Document, error) {
	f.calls++
	f.gotUser, f.gotDoc = userID, docID
	return f.doc, f.err
}

func newSummaryRouter(s Summarizer, user uuid.UUID) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.RequestID())
	g := r.Group("/api", middleware.Auth(fakeParser{id: user}))
	g.POST("/documents/:id/summarize", NewSummaryHandler(s).Summarize)
	return r
}

func summarize(t *testing.T, h http.Handler, docID, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/documents/"+docID+"/summarize", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestSummarize_Accepted(t *testing.T) {
	user, docID := uuid.New(), uuid.New()
	old := "An earlier summary."
	fs := &fakeSummarizer{doc: &models.Document{
		ID: docID, UserID: user, Filename: "paper.pdf", MimeType: "application/pdf", StoragePath: "SECRET/PATH.pdf",
		Status: models.StatusReady, ChunkCount: 12, CreatedAt: time.Now(),
		Summary: &old, SummaryStatus: models.SummaryProcessing,
	}}

	w := summarize(t, newSummaryRouter(fs, user), docID.String(), "tok")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if fs.calls != 1 || fs.gotUser != user || fs.gotDoc != docID {
		t.Errorf("service received user %v, doc %v (%d calls)", fs.gotUser, fs.gotDoc, fs.calls)
	}

	var env struct {
		Data struct {
			Document struct {
				Status        string  `json:"status"`
				Summary       *string `json:"summary"`
				SummaryStatus string  `json:"summary_status"`
			} `json:"document"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	d := env.Data.Document
	if d.SummaryStatus != "processing" || d.Summary == nil || *d.Summary != old {
		t.Errorf("document = %+v: while regenerating, the old summary stays visible", d)
	}
	for _, leaked := range []string{"storage_path", "SECRET/PATH", "user_id", "summary_error"} {
		if strings.Contains(w.Body.String(), leaked) {
			t.Errorf("the response contains %q", leaked)
		}
	}
}

func TestSummarize_ErrorMapping(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		status  int
		code    string
		header  [2]string
		details string
	}{
		{"not found", fmt.Errorf("summarize: %w", models.ErrNotFound), 404, response.CodeNotFound, [2]string{}, ""},
		{"document processing", fmt.Errorf("summarize: %w", &models.DocumentNotReadyError{Status: models.StatusProcessing}), 409, response.CodeDocumentNotReady, [2]string{}, `"status":"processing"`},
		{"document failed", &models.DocumentNotReadyError{Status: models.StatusFailed}, 409, response.CodeDocumentNotReady, [2]string{}, `"status":"failed"`},
		{"summary in progress", fmt.Errorf("summarize: %w", models.ErrSummaryInProgress), 409, response.CodeSummaryInProgress, [2]string{}, ""},
		{"too long", fmt.Errorf("summarize: %w", models.ErrDocumentTooLong), 422, response.CodeDocumentTooLong, [2]string{}, ""},
		{"queue full", fmt.Errorf("summarize: %w", service.ErrBusy), 503, response.CodeUnavailable, [2]string{"Retry-After", "30"}, ""},
		{"rate limited", &service.RateLimitError{RetryAfter: 2500 * time.Millisecond}, 429, response.CodeRateLimited, [2]string{"Retry-After", "3"}, ""},
		{"no API key", service.ErrLLMUnavailable, 503, response.CodeLLMUnavailable, [2]string{}, ""},
		{"unexpected", errors.New("pq: connection to 10.0.0.5:5432 refused"), 500, response.CodeInternal, [2]string{}, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeSummarizer{err: tc.err}
			w := summarize(t, newSummaryRouter(fs, uuid.New()), uuid.NewString(), "tok")

			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
			env := decodeError(t, w)
			if env.Success || env.Error == nil || env.Error.Code != tc.code {
				t.Fatalf("error = %+v, want code %s", env.Error, tc.code)
			}
			if tc.header[0] != "" && w.Header().Get(tc.header[0]) != tc.header[1] {
				t.Errorf("%s = %q, want %q", tc.header[0], w.Header().Get(tc.header[0]), tc.header[1])
			}
			if tc.details != "" && !strings.Contains(string(env.Error.Details), tc.details) {
				t.Errorf("details = %s, want it to contain %s", env.Error.Details, tc.details)
			}
			if strings.Contains(w.Body.String(), "10.0.0.5") {
				t.Error("an internal error message reached the client")
			}
		})
	}
}

func TestSummarize_RequestProblems(t *testing.T) {
	user := uuid.New()

	t.Run("no token", func(t *testing.T) {
		fs := &fakeSummarizer{}
		if w := summarize(t, newSummaryRouter(fs, user), uuid.NewString(), ""); w.Code != 401 || fs.calls != 0 {
			t.Errorf("status %d, calls %d", w.Code, fs.calls)
		}
	})
	t.Run("malformed id never reaches the service", func(t *testing.T) {
		fs := &fakeSummarizer{}
		if w := summarize(t, newSummaryRouter(fs, user), "not-a-uuid", "tok"); w.Code != 404 || fs.calls != 0 {
			t.Errorf("status %d, calls %d", w.Code, fs.calls)
		}
	})
	t.Run("AI disabled (no summarizer configured)", func(t *testing.T) {
		w := summarize(t, newSummaryRouter(nil, user), uuid.NewString(), "tok")
		if env := decodeError(t, w); w.Code != 503 || env.Error == nil || env.Error.Code != response.CodeLLMUnavailable {
			t.Errorf("status %d, error %+v", w.Code, env.Error)
		}
	})
}

func TestDocumentResponse_SummaryFields(t *testing.T) {
	text, msg := "A summary.", "The AI service was too busy to finish the summary. Please try again later."
	when := time.Now()

	tests := []struct {
		name    string
		doc     models.Document
		present []string
		absent  []string
	}{
		{"never summarized", models.Document{SummaryStatus: models.SummaryNone},
			[]string{`"summary_status":"none"`}, []string{`"summary":`, "summary_error", "summary_updated_at"}},
		{"a document built without the column", models.Document{},
			[]string{`"summary_status":"none"`}, []string{`"summary":`}},
		{"ready", models.Document{Summary: &text, SummaryStatus: models.SummaryReady, SummaryUpdatedAt: &when},
			[]string{`"summary":"A summary."`, `"summary_status":"ready"`, "summary_updated_at"}, []string{"summary_error"}},
		{"regenerating keeps the old text", models.Document{Summary: &text, SummaryStatus: models.SummaryProcessing},
			[]string{`"summary":"A summary."`, `"summary_status":"processing"`}, []string{"summary_error"}},
		{"failed shows the message", models.Document{Summary: &text, SummaryStatus: models.SummaryFailed, SummaryError: &msg},
			[]string{`"summary_status":"failed"`, "summary_error", "too busy"}, nil},
		{"an error message is hidden unless the summary failed", models.Document{SummaryStatus: models.SummaryReady, Summary: &text, SummaryError: &msg},
			[]string{`"summary_status":"ready"`}, []string{"summary_error", "too busy"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(toDocumentResponse(&tc.doc))
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.present {
				if !strings.Contains(string(b), want) {
					t.Errorf("missing %q in %s", want, b)
				}
			}
			for _, bad := range tc.absent {
				if strings.Contains(string(b), bad) {
					t.Errorf("unexpected %q in %s", bad, b)
				}
			}
		})
	}
}

// The list shows each document's summary STATUS but not its text; reading one document gives the text.
func TestSummary_ListOmitsTheTextButGetKeepsIt(t *testing.T) {
	user := uuid.New()
	text := "A fairly long summary text that must not be repeated for every document in the list."
	doc := models.Document{ID: uuid.New(), UserID: user, Filename: "a.pdf", Status: models.StatusReady,
		Summary: &text, SummaryStatus: models.SummaryReady, CreatedAt: time.Now()}
	fd := &fakeDocs{doc: &doc, list: []models.Document{doc}}
	r := newDocRouter(fd, user, 1<<20)

	w, _ := doReq(t, r, http.MethodGet, "/api/documents", nil, "", "tok")
	if strings.Contains(w.Body.String(), "fairly long summary") || !strings.Contains(w.Body.String(), `"summary_status":"ready"`) {
		t.Errorf("list body = %s", w.Body.String())
	}

	w, _ = doReq(t, r, http.MethodGet, "/api/documents/"+doc.ID.String(), nil, "", "tok")
	if !strings.Contains(w.Body.String(), "fairly long summary") {
		t.Errorf("get body = %s", w.Body.String())
	}
}