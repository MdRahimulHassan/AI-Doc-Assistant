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

// --- fakes and helpers ---

type fakeAuth struct {
	registerUser *models.User
	registerErr  error
	loginRes     *service.LoginResult
	loginErr     error
	getUser      *models.User
	getUserErr   error
}

func (f *fakeAuth) Register(context.Context, string, string) (*models.User, error) {
	return f.registerUser, f.registerErr
}
func (f *fakeAuth) Login(context.Context, string, string) (*service.LoginResult, error) {
	return f.loginRes, f.loginErr
}
func (f *fakeAuth) GetUser(context.Context, uuid.UUID) (*models.User, error) {
	return f.getUser, f.getUserErr
}

type fakeParser struct {
	id  uuid.UUID
	err error
}

func (p fakeParser) Parse(string) (uuid.UUID, error) { return p.id, p.err }

type envelope struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   *struct {
		Code    string                `json:"code"`
		Message string                `json:"message"`
		Details []response.FieldError `json:"details"`
	} `json:"error"`
}

func newRouter(a Authenticator, p middleware.TokenParser) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewAuthHandler(a)
	r.POST("/register", h.Register)
	r.POST("/login", h.Login)
	r.GET("/me", middleware.Auth(p), h.Me)
	return r
}

func call(t *testing.T, r http.Handler, method, path, body, bearer string) (*httptest.ResponseRecorder, envelope) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var env envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("response is not a valid envelope: %v\n%s", err, w.Body.String())
	}
	return w, env
}

// --- tests ---

func TestRegister(t *testing.T) {
	user := &models.User{ID: uuid.New(), Email: "a@x.com", PasswordHash: "secret-hash", CreatedAt: time.Now()}
	good := `{"email":"a@x.com","password":"password123"}`

	tests := []struct {
		name      string
		body      string
		auth      *fakeAuth
		wantCode  int
		wantErr   string // envelope error code; "" means success
		wantField string // field named by the first validation detail
	}{
		{"success", good, &fakeAuth{registerUser: user}, 201, "", ""},
		{"malformed json", `{bad`, &fakeAuth{}, 400, response.CodeBadRequest, ""},
		{"empty body", ``, &fakeAuth{}, 400, response.CodeBadRequest, ""},
		{"missing password", `{"email":"a@x.com"}`, &fakeAuth{}, 400, response.CodeValidation, "password"},
		{"service validation error", good,
			&fakeAuth{registerErr: &service.ValidationError{Field: "email", Message: "bad"}},
			400, response.CodeValidation, "email"},
		{"email taken (wrapped error)", good,
			&fakeAuth{registerErr: fmt.Errorf("register: %w", models.ErrEmailTaken)},
			409, response.CodeEmailTaken, ""},
		{"unexpected error", good,
			&fakeAuth{registerErr: errors.New("dial tcp 10.0.0.5:5432: connection refused")},
			500, response.CodeInternal, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w, env := call(t, newRouter(tc.auth, fakeParser{}), http.MethodPost, "/register", tc.body, "")

			if w.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d\n%s", w.Code, tc.wantCode, w.Body.String())
			}

			if tc.wantErr == "" {
				if !env.Success || env.Error != nil {
					t.Error("expected a success envelope")
				}
				body := strings.ToLower(w.Body.String())
				if strings.Contains(body, "password") || strings.Contains(body, "secret-hash") {
					t.Error("response leaks password data")
				}
				return
			}

			if env.Success || env.Error == nil || env.Error.Code != tc.wantErr {
				t.Fatalf("error = %+v, want code %s", env.Error, tc.wantErr)
			}
			if tc.wantField != "" && (len(env.Error.Details) == 0 || env.Error.Details[0].Field != tc.wantField) {
				t.Errorf("details = %+v, want first field %q", env.Error.Details, tc.wantField)
			}
			if strings.Contains(w.Body.String(), "10.0.0.5") {
				t.Error("raw internal error text leaked to the client")
			}
		})
	}
}

func TestLogin(t *testing.T) {
	user := &models.User{ID: uuid.New(), Email: "a@x.com", CreatedAt: time.Now()}
	res := &service.LoginResult{User: user, Token: "tok123", ExpiresAt: time.Now().Add(time.Hour)}
	good := `{"email":"a@x.com","password":"whatever"}`

	tests := []struct {
		name     string
		body     string
		auth     *fakeAuth
		wantCode int
		wantErr  string
	}{
		{"success", good, &fakeAuth{loginRes: res}, 200, ""},
		{"missing fields", `{}`, &fakeAuth{}, 400, response.CodeValidation},
		{"invalid credentials", good, &fakeAuth{loginErr: service.ErrInvalidCredentials}, 401, response.CodeInvalidCredentials},
		{"unexpected error", good, &fakeAuth{loginErr: errors.New("boom")}, 500, response.CodeInternal},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w, env := call(t, newRouter(tc.auth, fakeParser{}), http.MethodPost, "/login", tc.body, "")

			if w.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d\n%s", w.Code, tc.wantCode, w.Body.String())
			}
			if tc.wantErr == "" {
				if !strings.Contains(string(env.Data), `"token":"tok123"`) {
					t.Errorf("data does not contain the token: %s", env.Data)
				}
				if w.Header().Get("Cache-Control") != "no-store" {
					t.Error("token responses must send Cache-Control: no-store")
				}
				return
			}
			if env.Error == nil || env.Error.Code != tc.wantErr {
				t.Errorf("error = %+v, want code %s", env.Error, tc.wantErr)
			}
		})
	}
}

func TestMe(t *testing.T) {
	id := uuid.New()
	user := &models.User{ID: id, Email: "a@x.com", CreatedAt: time.Now()}

	tests := []struct {
		name     string
		auth     *fakeAuth
		bearer   string
		wantCode int
		wantErr  string
	}{
		{"no token", &fakeAuth{getUser: user}, "", 401, response.CodeUnauthorized},
		{"success", &fakeAuth{getUser: user}, "tok", 200, ""},
		{"account deleted after token was issued",
			&fakeAuth{getUserErr: fmt.Errorf("get user: %w", models.ErrNotFound)}, "tok", 401, response.CodeUnauthorized},
		{"unexpected error", &fakeAuth{getUserErr: errors.New("boom")}, "tok", 500, response.CodeInternal},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w, env := call(t, newRouter(tc.auth, fakeParser{id: id}), http.MethodGet, "/me", "", tc.bearer)

			if w.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d\n%s", w.Code, tc.wantCode, w.Body.String())
			}
			if tc.wantErr == "" {
				if !env.Success {
					t.Error("expected a success envelope")
				}
				return
			}
			if env.Error == nil || env.Error.Code != tc.wantErr {
				t.Errorf("error = %+v, want code %s", env.Error, tc.wantErr)
			}
		})
	}
}