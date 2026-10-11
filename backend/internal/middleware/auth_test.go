package middleware

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/pkg/response"
)

type fakeParser struct {
	id  uuid.UUID
	err error
}

func (p fakeParser) Parse(string) (uuid.UUID, error) { return p.id, p.err }

func TestAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	id := uuid.New()

	tests := []struct {
		name      string
		header    string
		parserErr error
		wantCode  int
	}{
		{"no header", "", nil, http.StatusUnauthorized},
		{"wrong scheme", "Basic abc123", nil, http.StatusUnauthorized},
		{"scheme only", "Bearer", nil, http.StatusUnauthorized},
		{"empty token", "Bearer    ", nil, http.StatusUnauthorized},
		{"parser rejects token", "Bearer bad", errors.New("expired"), http.StatusUnauthorized},
		{"valid", "Bearer good", nil, http.StatusOK},
		{"scheme is case-insensitive", "bearer good", nil, http.StatusOK},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var reached bool
			var gotID uuid.UUID

			r := gin.New()
			r.GET("/x", Auth(fakeParser{id: id, err: tc.parserErr}), func(c *gin.Context) {
				reached = true
				gotID, _ = UserIDFrom(c)
				c.Status(http.StatusOK)
			})

			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d", w.Code, tc.wantCode)
			}

			if tc.wantCode == http.StatusUnauthorized {
				if reached {
					t.Error("handler ran after a 401: the middleware did not abort")
				}
				if w.Header().Get("WWW-Authenticate") == "" {
					t.Error("missing WWW-Authenticate header")
				}
				if !strings.Contains(w.Body.String(), response.CodeUnauthorized) {
					t.Errorf("body is not the error envelope: %s", w.Body.String())
				}
				return
			}

			if !reached || gotID != id {
				t.Errorf("reached=%v gotID=%v, want true and %v", reached, gotID, id)
			}
		})
	}
}