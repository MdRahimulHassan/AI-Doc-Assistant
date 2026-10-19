package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/models"
)

type fakeSearcher struct {
	terms      []string
	termsErr   error
	chunks     []models.RetrievedChunk
	searchErr  error

	gotQuestion string
	gotMax      int
	gotTerms    []string
	gotLimit    int
	gotUser     uuid.UUID
	gotDoc      uuid.UUID
	searchCalls int
}

func (f *fakeSearcher) QueryTerms(_ context.Context, question string, maxTerms int) ([]string, error) {
	f.gotQuestion, f.gotMax = question, maxTerms
	return f.terms, f.termsErr
}

func (f *fakeSearcher) SearchChunks(_ context.Context, userID, docID uuid.UUID, terms []string, limit int) ([]models.RetrievedChunk, error) {
	f.searchCalls++
	f.gotUser, f.gotDoc, f.gotTerms, f.gotLimit = userID, docID, terms, limit
	return f.chunks, f.searchErr
}

func TestRetrieve_CleansTheQuestionAndPassesTheSettings(t *testing.T) {
	user, doc := uuid.New(), uuid.New()
	fs := &fakeSearcher{
		terms:  []string{"zebrafish"},
		chunks: []models.RetrievedChunk{{Index: 3, Content: "x", Score: 1.5, MatchedTerms: 1}},
	}
	svc, err := NewRetrievalService(fs, 4)
	if err != nil {
		t.Fatal(err)
	}

	res, err := svc.Retrieve(context.Background(), user, doc, "  What\x00 is\r\nthis zebrafish?  ")
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	// No NUL bytes or carriage returns reach the database, and whitespace is tidy.
	if strings.ContainsAny(fs.gotQuestion, "\x00\r") || strings.Join(strings.Fields(fs.gotQuestion), " ") != "What is this zebrafish?" {
		t.Errorf("the repository received %q", fs.gotQuestion)
	}
	if fs.gotMax != maxQueryTerms || fs.gotLimit != 4 || fs.gotUser != user || fs.gotDoc != doc {
		t.Errorf("settings passed: maxTerms %d, limit %d, user %v, doc %v", fs.gotMax, fs.gotLimit, fs.gotUser, fs.gotDoc)
	}
	if len(fs.gotTerms) != 1 || fs.gotTerms[0] != "zebrafish" {
		t.Errorf("the search received terms %v, want the ones QueryTerms produced", fs.gotTerms)
	}
	if res.Empty() || len(res.Chunks) != 1 || res.Terms[0] != "zebrafish" {
		t.Errorf("result = %+v", res)
	}
}

func TestRetrieve_NoUsableWordsIsEmptyNotAnError(t *testing.T) {
	fs := &fakeSearcher{terms: []string{}, chunks: []models.RetrievedChunk{}}
	svc, _ := NewRetrievalService(fs, 4)

	res, err := svc.Retrieve(context.Background(), uuid.New(), uuid.New(), "what is the")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !res.Empty() {
		t.Error("expected an empty result")
	}
	if fs.searchCalls != 1 {
		t.Errorf("SearchChunks was called %d times: it must still run so ownership is always checked", fs.searchCalls)
	}
}

func TestRetrieve_DocumentErrorsPassThrough(t *testing.T) {
	fs := &fakeSearcher{terms: []string{}, searchErr: models.ErrNotFound}
	svc, _ := NewRetrievalService(fs, 4)

	_, err := svc.Retrieve(context.Background(), uuid.New(), uuid.New(), "anything at all")
	if !errors.Is(err, models.ErrNotFound) {
		t.Errorf("err = %v, want it to wrap models.ErrNotFound", err)
	}
}

func TestRetrieve_RepositoryFailuresAreWrapped(t *testing.T) {
	boom := errors.New("database is down")

	for name, fs := range map[string]*fakeSearcher{
		"terms step":  {termsErr: boom},
		"search step": {terms: []string{"x"}, searchErr: fmt.Errorf("search chunks: %w", boom)},
	} {
		t.Run(name, func(t *testing.T) {
			svc, _ := NewRetrievalService(fs, 4)
			res, err := svc.Retrieve(context.Background(), uuid.New(), uuid.New(), "a question")
			if !errors.Is(err, boom) || res != nil {
				t.Errorf("got (%v, %v), want (nil, an error wrapping the cause)", res, err)
			}
		})
	}
}

func TestNewRetrievalService_Validation(t *testing.T) {
	if _, err := NewRetrievalService(nil, 4); err == nil {
		t.Error("a nil searcher should be rejected")
	}
	if _, err := NewRetrievalService(&fakeSearcher{}, 0); err == nil {
		t.Error("topK 0 should be rejected")
	}
}