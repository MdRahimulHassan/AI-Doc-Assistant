package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
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

// fakeParser, envelope and the Authenticator fakes come from auth_handler_test.go.

type fakeDocs struct {
	doc  *models.Document
	list []models.Document
	err  error

	uploads, lists, gets, deletes int
	gotUser                       uuid.UUID
	gotFilename                   string
	gotSize                       int64
	gotContent                    []byte
	gotLimit, gotOffset           int
}

func (f *fakeDocs) Upload(_ context.Context, userID uuid.UUID, filename string, file service.UploadFile, size int64) (*models.Document, error) {
	f.uploads++
	f.gotUser, f.gotFilename, f.gotSize = userID, filename, size
	f.gotContent, _ = io.ReadAll(file)
	return f.doc, f.err
}

func (f *fakeDocs) Get(context.Context, uuid.UUID, uuid.UUID) (*models.Document, error) {
	f.gets++
	return f.doc, f.err
}

func (f *fakeDocs) List(_ context.Context, _ uuid.UUID, limit, offset int) ([]models.Document, error) {
	f.lists++
	f.gotLimit, f.gotOffset = limit, offset
	return f.list, f.err
}

func (f *fakeDocs) Delete(context.Context, uuid.UUID, uuid.UUID) error {
	f.deletes++
	return f.err
}

func newDocRouter(d Documents, user uuid.UUID, maxUpload int64) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.MaxMultipartMemory = 1 << 20

	h := NewDocumentHandler(d, maxUpload)
	g := r.Group("/api", middleware.Auth(fakeParser{id: user}))
	g.POST("/documents", h.Upload)
	g.GET("/documents", h.List)
	g.GET("/documents/:id", h.Get)
	g.DELETE("/documents/:id", h.Delete)
	return r
}

func multipartBody(t *testing.T, field, filename string, content []byte) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile(field, filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &body, mw.FormDataContentType()
}

func doReq(t *testing.T, r http.Handler, method, path string, body io.Reader, ctype, bearer string) (*httptest.ResponseRecorder, envelope) {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var env envelope
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("response is not an envelope: %v\n%s", err, w.Body.String())
		}
	}
	return w, env
}

func sampleDoc(user uuid.UUID, status models.DocumentStatus) *models.Document {
	return &models.Document{
		ID: uuid.New(), UserID: user, Filename: "paper.pdf", MimeType: "application/pdf",
		SizeBytes: 14, StoragePath: "SECRET/PATH.pdf", Status: status, CreatedAt: time.Now(),
	}
}

// --- upload ---

func TestUpload_Success(t *testing.T) {
	user := uuid.New()
	fd := &fakeDocs{doc: sampleDoc(user, models.StatusProcessing)}
	r := newDocRouter(fd, user, 1<<20)
	content := []byte("%PDF-1.7 hello")

	body, ctype := multipartBody(t, "file", "paper.pdf", content)
	w, env := doReq(t, r, http.MethodPost, "/api/documents", body, ctype, "tok")

	if w.Code != http.StatusAccepted || !env.Success {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if fd.uploads != 1 || fd.gotUser != user || fd.gotFilename != "paper.pdf" ||
		fd.gotSize != int64(len(content)) || !bytes.Equal(fd.gotContent, content) {
		t.Errorf("service received user=%v name=%q size=%d content=%q", fd.gotUser, fd.gotFilename, fd.gotSize, fd.gotContent)
	}

	var data struct {
		Document struct {
			Status string `json:"status"`
		} `json:"document"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil || data.Document.Status != "processing" {
		t.Errorf("document status = %q (%v)", data.Document.Status, err)
	}

	for _, leaked := range []string{"storage_path", "SECRET/PATH", "user_id", user.String(), "error_message"} {
		if strings.Contains(w.Body.String(), leaked) {
			t.Errorf("response contains %q: %s", leaked, w.Body.String())
		}
	}
}

func TestUpload_RequestProblems(t *testing.T) {
	user := uuid.New()

	t.Run("no token", func(t *testing.T) {
		fd := &fakeDocs{}
		body, ctype := multipartBody(t, "file", "a.pdf", []byte("%PDF-1.7"))
		w, _ := doReq(t, newDocRouter(fd, user, 1<<20), http.MethodPost, "/api/documents", body, ctype, "")
		if w.Code != http.StatusUnauthorized || fd.uploads != 0 {
			t.Errorf("status %d, uploads %d", w.Code, fd.uploads)
		}
	})

	t.Run("form has no file field", func(t *testing.T) {
		fd := &fakeDocs{}
		body, ctype := multipartBody(t, "attachment", "a.pdf", []byte("%PDF-1.7")) // wrong field name
		w, env := doReq(t, newDocRouter(fd, user, 1<<20), http.MethodPost, "/api/documents", body, ctype, "tok")
		if w.Code != http.StatusBadRequest || env.Error == nil || env.Error.Code != response.CodeNoFile || fd.uploads != 0 {
			t.Errorf("status %d, error %+v, uploads %d", w.Code, env.Error, fd.uploads)
		}
	})

	t.Run("not multipart", func(t *testing.T) {
		fd := &fakeDocs{}
		w, env := doReq(t, newDocRouter(fd, user, 1<<20), http.MethodPost, "/api/documents",
			strings.NewReader(`{"file":"x"}`), "application/json", "tok")
		if w.Code != http.StatusBadRequest || env.Error == nil || env.Error.Code != response.CodeBadRequest || fd.uploads != 0 {
			t.Errorf("status %d, error %+v, uploads %d", w.Code, env.Error, fd.uploads)
		}
	})
}

func TestUpload_BodyTooLarge(t *testing.T) {
	const max = 1 << 10
	big := bytes.Repeat([]byte("a"), 200<<10) // far beyond max plus the framing slack

	for _, announced := range []bool{true, false} {
		name := "announced length"
		if !announced {
			name = "unknown length (chunked)"
		}
		t.Run(name, func(t *testing.T) {
			fd := &fakeDocs{}
			body, ctype := multipartBody(t, "file", "big.txt", big)

			req := httptest.NewRequest(http.MethodPost, "/api/documents", body)
			req.Header.Set("Content-Type", ctype)
			req.Header.Set("Authorization", "Bearer tok")
			if !announced {
				req.ContentLength = -1 // the early check cannot help: the cap must hold while reading
			}
			w := httptest.NewRecorder()
			newDocRouter(fd, uuid.New(), max).ServeHTTP(w, req)

			var env envelope
			if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
				t.Fatalf("not an envelope: %v\n%s", err, w.Body.String())
			}
			if w.Code != http.StatusRequestEntityTooLarge || env.Error == nil || env.Error.Code != response.CodeFileTooLarge {
				t.Errorf("status %d, error %+v", w.Code, env.Error)
			}
			if fd.uploads != 0 {
				t.Error("the service must not be called for an oversized body")
			}
		})
	}
}

func TestUpload_ServiceErrorsMapToStatuses(t *testing.T) {
	user := uuid.New()
	leaky := errors.New("pq: connection to 10.0.0.5:5432 refused")

	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"too large", fmt.Errorf("upload: %w", models.ErrFileTooLarge), 413, response.CodeFileTooLarge},
		{"empty", service.ErrEmptyFile, 400, response.CodeNoFile},
		{"unsupported type (wrapped like the service does)",
			fmt.Errorf("%w: the content is \"pdf\" but the filename ends in \"docx\"", service.ErrUnsupportedFileType), 415, response.CodeUnsupportedFileType},
		{"busy", fmt.Errorf("upload: %w", service.ErrBusy), 503, response.CodeUnavailable},
		{"unexpected", leaky, 500, response.CodeInternal},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fd := &fakeDocs{err: tc.err}
			body, ctype := multipartBody(t, "file", "a.pdf", []byte("%PDF-1.7"))
			w, env := doReq(t, newDocRouter(fd, user, 1<<20), http.MethodPost, "/api/documents", body, ctype, "tok")

			if w.Code != tc.wantStatus || env.Error == nil || env.Error.Code != tc.wantCode {
				t.Errorf("status %d, error %+v; want %d %s", w.Code, env.Error, tc.wantStatus, tc.wantCode)
			}
			if tc.wantStatus == 503 && w.Header().Get("Retry-After") == "" {
				t.Error("503 should carry a Retry-After header")
			}
			if strings.Contains(w.Body.String(), "10.0.0.5") || strings.Contains(w.Body.String(), "filename ends in") {
				t.Errorf("internal error text reached the client: %s", w.Body.String())
			}
		})
	}
}

// --- list ---

func TestList_Paging(t *testing.T) {
	tests := []struct {
		query                  string
		wantStatus             int
		wantLimit, wantOffset  int
		wantBadField           string
	}{
		{"", 200, 20, 0, ""},
		{"?limit=5&offset=10", 200, 5, 10, ""},
		{"?limit=500", 200, 100, 0, ""}, // clamped to the maximum
		{"?limit=", 200, 20, 0, ""},     // empty means "not given"
		{"?limit=0", 400, 0, 0, "limit"},
		{"?limit=-3", 400, 0, 0, "limit"},
		{"?limit=abc", 400, 0, 0, "limit"},
		{"?limit=99999999999999999999", 400, 0, 0, "limit"},
		{"?offset=-1", 400, 0, 0, "offset"},
		{"?offset=x", 400, 0, 0, "offset"},
	}

	for _, tc := range tests {
		t.Run(tc.query, func(t *testing.T) {
			user := uuid.New()
			fd := &fakeDocs{}
			w, env := doReq(t, newDocRouter(fd, user, 1<<20), http.MethodGet, "/api/documents"+tc.query, nil, "", "tok")

			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.wantStatus, w.Body.String())
			}
			if tc.wantStatus == 400 {
				if env.Error == nil || env.Error.Code != response.CodeValidation ||
					len(env.Error.Details) != 1 || env.Error.Details[0].Field != tc.wantBadField {
					t.Errorf("error = %+v, want a validation error for %q", env.Error, tc.wantBadField)
				}
				if fd.lists != 0 {
					t.Error("the service must not be called for invalid parameters")
				}
				return
			}
			if fd.gotLimit != tc.wantLimit || fd.gotOffset != tc.wantOffset {
				t.Errorf("service got limit=%d offset=%d, want %d and %d", fd.gotLimit, fd.gotOffset, tc.wantLimit, tc.wantOffset)
			}
			var data struct {
				Limit  int `json:"limit"`
				Offset int `json:"offset"`
			}
			if err := json.Unmarshal(env.Data, &data); err != nil || data.Limit != tc.wantLimit || data.Offset != tc.wantOffset {
				t.Errorf("response limit/offset = %d/%d (%v)", data.Limit, data.Offset, err)
			}
		})
	}
}

func TestList_ResponseShape(t *testing.T) {
	user := uuid.New()
	failedMsg := "No readable text was found in this file."
	failed := sampleDoc(user, models.StatusFailed)
	failed.ErrorMessage = &failedMsg
	processing := sampleDoc(user, models.StatusProcessing)

	t.Run("empty list is [] not null", func(t *testing.T) {
		w, _ := doReq(t, newDocRouter(&fakeDocs{}, user, 1<<20), http.MethodGet, "/api/documents", nil, "", "tok")
		if !strings.Contains(w.Body.String(), `"documents":[]`) {
			t.Errorf("body = %s", w.Body.String())
		}
	})

	t.Run("error message only on failed documents, no internals", func(t *testing.T) {
		fd := &fakeDocs{list: []models.Document{*failed, *processing}}
		w, _ := doReq(t, newDocRouter(fd, user, 1<<20), http.MethodGet, "/api/documents", nil, "", "tok")

		if strings.Count(w.Body.String(), `"error_message"`) != 1 || !strings.Contains(w.Body.String(), failedMsg) {
			t.Errorf("error_message should appear exactly once: %s", w.Body.String())
		}
		for _, leaked := range []string{"storage_path", "SECRET/PATH", "user_id"} {
			if strings.Contains(w.Body.String(), leaked) {
				t.Errorf("response contains %q", leaked)
			}
		}
	})

	t.Run("unexpected error", func(t *testing.T) {
		fd := &fakeDocs{err: errors.New("boom")}
		w, env := doReq(t, newDocRouter(fd, user, 1<<20), http.MethodGet, "/api/documents", nil, "", "tok")
		if w.Code != 500 || env.Error == nil || env.Error.Code != response.CodeInternal {
			t.Errorf("status %d, error %+v", w.Code, env.Error)
		}
	})
}

// --- get and delete ---

func TestGetAndDelete(t *testing.T) {
	user := uuid.New()
	id := uuid.New()

	t.Run("get", func(t *testing.T) {
		fd := &fakeDocs{doc: sampleDoc(user, models.StatusReady)}
		w, env := doReq(t, newDocRouter(fd, user, 1<<20), http.MethodGet, "/api/documents/"+id.String(), nil, "", "tok")
		if w.Code != 200 || !env.Success {
			t.Errorf("status %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("delete returns 204 with no body", func(t *testing.T) {
		fd := &fakeDocs{}
		w, _ := doReq(t, newDocRouter(fd, user, 1<<20), http.MethodDelete, "/api/documents/"+id.String(), nil, "", "tok")
		if w.Code != http.StatusNoContent || w.Body.Len() != 0 || fd.deletes != 1 {
			t.Errorf("status %d, body %q, deletes %d", w.Code, w.Body.String(), fd.deletes)
		}
	})

	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		t.Run(method+" with a malformed id is 404 and never reaches the service", func(t *testing.T) {
			fd := &fakeDocs{}
			w, env := doReq(t, newDocRouter(fd, user, 1<<20), method, "/api/documents/not-a-uuid", nil, "", "tok")
			if w.Code != 404 || env.Error == nil || env.Error.Code != response.CodeNotFound || fd.gets+fd.deletes != 0 {
				t.Errorf("status %d, error %+v, calls %d", w.Code, env.Error, fd.gets+fd.deletes)
			}
		})

		t.Run(method+" not found", func(t *testing.T) {
			fd := &fakeDocs{err: fmt.Errorf("delete document: %w", models.ErrNotFound)}
			w, env := doReq(t, newDocRouter(fd, user, 1<<20), method, "/api/documents/"+id.String(), nil, "", "tok")
			if w.Code != 404 || env.Error == nil || env.Error.Code != response.CodeNotFound {
				t.Errorf("status %d, error %+v", w.Code, env.Error)
			}
		})

		t.Run(method+" unexpected error", func(t *testing.T) {
			fd := &fakeDocs{err: errors.New("boom")}
			w, env := doReq(t, newDocRouter(fd, user, 1<<20), method, "/api/documents/"+id.String(), nil, "", "tok")
			if w.Code != 500 || env.Error == nil || env.Error.Code != response.CodeInternal {
				t.Errorf("status %d, error %+v", w.Code, env.Error)
			}
		})

		t.Run(method+" without a token", func(t *testing.T) {
			fd := &fakeDocs{}
			w, _ := doReq(t, newDocRouter(fd, user, 1<<20), method, "/api/documents/"+id.String(), nil, "", "")
			if w.Code != 401 || fd.gets+fd.deletes != 0 {
				t.Errorf("status %d, calls %d", w.Code, fd.gets+fd.deletes)
			}
		})
	}
}