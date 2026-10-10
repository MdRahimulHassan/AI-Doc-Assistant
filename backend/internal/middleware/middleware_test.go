package middleware

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func newTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RequestID(), Logger(), CORS([]string{"http://localhost:5173"}))
	r.GET("/ping", func(c *gin.Context) { c.String(http.StatusOK, "pong") })
	return r
}

func TestRequestID_SetAndUnique(t *testing.T) {
	r := newTestRouter()

	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ping", nil))

		id := w.Header().Get(RequestIDHeader)
		if id == "" {
			t.Fatal("missing X-Request-ID header")
		}
		seen[id] = true
	}
	if len(seen) != 2 {
		t.Error("request IDs must be unique per request")
	}
}

func TestCORS(t *testing.T) {
	tests := []struct {
		name            string
		method          string
		origin          string
		wantStatus      int
		wantAllowOrigin string
	}{
		{"allowed origin", http.MethodGet, "http://localhost:5173", http.StatusOK, "http://localhost:5173"},
		{"disallowed origin", http.MethodGet, "http://evil.example", http.StatusForbidden, ""},
		{"no origin header", http.MethodGet, "", http.StatusOK, ""},
		{"preflight from allowed origin", http.MethodOptions, "http://localhost:5173", http.StatusNoContent, "http://localhost:5173"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestRouter()
			req := httptest.NewRequest(tc.method, "/ping", nil)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.method == http.MethodOptions {
				req.Header.Set("Access-Control-Request-Method", "GET")
			}

			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tc.wantStatus)
			}
			got := w.Header().Get("Access-Control-Allow-Origin")
			if got != tc.wantAllowOrigin {
				t.Errorf("Allow-Origin = %q, want %q", got, tc.wantAllowOrigin)
			}
		})
	}
}

func TestLogger_WritesFieldsAndHidesQuery(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	r := newTestRouter()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ping?token=secret", nil))

	out := buf.String()
	for _, want := range []string{"method=GET", "path=/ping", "status=200", "request_id="} {
		if !strings.Contains(out, want) {
			t.Errorf("log output missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "secret") {
		t.Error("query string must never be logged")
	}
}
