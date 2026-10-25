package router_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/middleware"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/models"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/ratelimit"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/router"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/service"
)

// --- fakes: just enough for the router ---

type limClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *limClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *limClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type limUsers map[string]uuid.UUID

func (u limUsers) Parse(token string) (uuid.UUID, error) {
	if id, ok := u[token]; ok {
		return id, nil
	}
	return uuid.Nil, errors.New("unknown token")
}

type nopPinger struct{}

func (nopPinger) Ping(context.Context) error { return nil }

type nopAuth struct{}

func (nopAuth) Register(_ context.Context, email, _ string) (*models.User, error) {
	return &models.User{ID: uuid.New(), Email: email, CreatedAt: time.Now()}, nil
}

func (nopAuth) Login(_ context.Context, email, _ string) (*service.LoginResult, error) {
	return &service.LoginResult{
		User: &models.User{ID: uuid.New(), Email: email}, Token: "t", ExpiresAt: time.Now().Add(time.Hour),
	}, nil
}

func (nopAuth) GetUser(context.Context, uuid.UUID) (*models.User, error) { return &models.User{}, nil }

type nopDocs struct{}

func (nopDocs) Upload(context.Context, uuid.UUID, string, service.UploadFile, int64) (*models.Document, error) {
	return &models.Document{ID: uuid.New()}, nil
}
func (nopDocs) Get(context.Context, uuid.UUID, uuid.UUID) (*models.Document, error) {
	return &models.Document{ID: uuid.New()}, nil
}
func (nopDocs) List(context.Context, uuid.UUID, int, int) ([]models.Document, error) { return nil, nil }
func (nopDocs) Delete(context.Context, uuid.UUID, uuid.UUID) error                  { return nil }

type nopMessages struct{}

func (nopMessages) Messages(context.Context, uuid.UUID, uuid.UUID, int, int64) (*models.MessagePage, error) {
	return &models.MessagePage{Messages: []models.Message{}}, nil
}

type nopSummarizer struct{}

func (nopSummarizer) Request(_ context.Context, _, docID uuid.UUID) (*models.Document, error) {
	return &models.Document{ID: docID, SummaryStatus: models.SummaryProcessing}, nil
}

// gateAsker answers at once, except for the question "hold", which waits.
type gateAsker struct {
	entered chan struct{}
	release chan struct{}
}

func (g *gateAsker) Ask(ctx context.Context, req service.AskRequest, emit func(service.AskEvent) error) error {
	if req.Question == "hold" {
		g.entered <- struct{}{}
		select {
		case <-g.release:
		case <-ctx.Done():
		}
	}
	return emit(service.AskEvent{Kind: service.EventSources, Sources: []service.SourceInfo{}})
}

type limitEnv struct {
	handler http.Handler
	clock   *limClock
	asker   *gateAsker
}

func (e *limitEnv) call(method, path, token, body string, tweak ...func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for _, f := range tweak {
		f(req)
	}
	w := httptest.NewRecorder()
	e.handler.ServeHTTP(w, req)
	return w
}

func newLimitEnv(t *testing.T, limits func(clock *limClock) router.Limits) *limitEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)

	clock := &limClock{t: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	asker := &gateAsker{entered: make(chan struct{}, 8), release: make(chan struct{})}
	var l router.Limits
	if limits != nil {
		l = limits(clock)
	}

	h := router.New(router.Deps{
		DB: nopPinger{}, Auth: nopAuth{}, Documents: nopDocs{}, Asker: asker, Messages: nopMessages{},
		Summaries: nopSummarizer{}, Tokens: limUsers{"alice": uuid.New(), "bob": uuid.New()},
		CORSOrigins: []string{"http://localhost:5173"}, MaxUploadBytes: 1 << 20, Limits: l,
	})
	return &limitEnv{handler: h, clock: clock, asker: asker}
}

func mustLimiter(t *testing.T, clock *limClock, perMinute, burst int) *ratelimit.KeyedLimiter {
	t.Helper()
	l, err := ratelimit.NewKeyedLimiter(ratelimit.Config{PerMinute: perMinute, Burst: burst, Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func mustSemaphore(t *testing.T, limit int) *ratelimit.KeyedSemaphore {
	t.Helper()
	s, err := ratelimit.NewKeyedSemaphore(limit)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func reason(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var b struct {
		Error *struct {
			Code    string `json:"code"`
			Details struct {
				Reason string `json:"reason"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil || b.Error == nil || b.Error.Code != "RATE_LIMITED" {
		t.Fatalf("not a RATE_LIMITED envelope (status %d): %s", w.Code, w.Body.String())
	}
	return b.Error.Details.Reason
}

const (
	loginJSON = `{"email":"a@example.com","password":"password123"}`
	askJSON   = `{"question":"hello"}`
	holdJSON  = `{"question":"hold"}`
)

var docPath = "/api/documents/" + uuid.NewString()

// --- tests ---

func TestLimits_AuthRoutesAreLimitedPerAddressAndForgedHeadersDoNotHelp(t *testing.T) {
	e := newLimitEnv(t, func(c *limClock) router.Limits {
		return router.Limits{Auth: []gin.HandlerFunc{middleware.RateLimitByIP(mustLimiter(t, c, 6, 3))}}
	})

	for i := 0; i < 3; i++ {
		if w := e.call("POST", "/api/auth/login", "", loginJSON); w.Code != http.StatusOK {
			t.Fatalf("login %d: status %d: %s", i, w.Code, w.Body.String())
		}
	}

	w := e.call("POST", "/api/auth/login", "", loginJSON)
	if w.Code != http.StatusTooManyRequests || reason(t, w) != "rate_limit" || w.Header().Get("Retry-After") == "" {
		t.Fatalf("the 4th login: status %d: %s", w.Code, w.Body.String())
	}

	// Register shares the budget with login for the same address.
	if w := e.call("POST", "/api/auth/register", "", loginJSON); w.Code != http.StatusTooManyRequests {
		t.Errorf("register: status %d, want 429", w.Code)
	}

	// Forged headers must not buy a fresh bucket.
	for i, header := range []string{"X-Forwarded-For", "X-Real-IP", "Forwarded"} {
		w := e.call("POST", "/api/auth/login", "", loginJSON, func(r *http.Request) { r.Header.Set(header, "198.51.100."+string(rune('1'+i))) })
		if w.Code != http.StatusTooManyRequests {
			t.Errorf("a forged %s header bypassed the limit: status %d", header, w.Code)
		}
	}

	// A different real address has its own bucket.
	if w := e.call("POST", "/api/auth/login", "", loginJSON, func(r *http.Request) { r.RemoteAddr = "203.0.113.9:5555" }); w.Code != http.StatusOK {
		t.Errorf("another address: status %d, want 200", w.Code)
	}

	// Routes that are not limited are untouched.
	if w := e.call("GET", "/health", "", ""); w.Code != http.StatusOK {
		t.Errorf("/health: status %d", w.Code)
	}

	e.clock.Advance(11 * time.Second) // a token refills every 10 seconds
	if w := e.call("POST", "/api/auth/login", "", loginJSON); w.Code != http.StatusOK {
		t.Errorf("after waiting: status %d, want 200", w.Code)
	}
}

func TestLimits_AskAndSummarizeShareOneBudgetPerUser(t *testing.T) {
	e := newLimitEnv(t, func(c *limClock) router.Limits {
		return router.Limits{LLM: []gin.HandlerFunc{
			middleware.LimitConcurrencyByUser(mustSemaphore(t, 2)),
			middleware.RateLimitByUser(mustLimiter(t, c, 6, 3)),
		}}
	})

	if w := e.call("POST", docPath+"/ask", "alice", askJSON); w.Code != http.StatusOK {
		t.Fatalf("ask 1: status %d", w.Code)
	}
	if w := e.call("POST", docPath+"/summarize", "alice", ""); w.Code != http.StatusAccepted {
		t.Fatalf("summarize: status %d: %s", w.Code, w.Body.String())
	}
	if w := e.call("POST", docPath+"/ask", "alice", askJSON); w.Code != http.StatusOK {
		t.Fatalf("ask 2: status %d", w.Code)
	}

	// Three tokens used across both routes: the fourth request is refused, on EITHER route.
	for _, path := range []string{"/ask", "/summarize"} {
		w := e.call("POST", docPath+path, "alice", askJSON)
		if w.Code != http.StatusTooManyRequests || reason(t, w) != "rate_limit" {
			t.Errorf("%s: status %d, want 429 rate_limit", path, w.Code)
		}
	}

	// Reading the conversation is not an AI route.
	if w := e.call("GET", docPath+"/messages", "alice", ""); w.Code != http.StatusOK {
		t.Errorf("messages: status %d, want 200", w.Code)
	}
	// Another user has a full budget; an anonymous caller gets 401, never 429.
	if w := e.call("POST", docPath+"/ask", "bob", askJSON); w.Code != http.StatusOK {
		t.Errorf("bob: status %d", w.Code)
	}
	for i := 0; i < 5; i++ {
		if w := e.call("POST", docPath+"/ask", "", askJSON); w.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous ask: status %d, want 401", w.Code)
		}
	}
}

func TestLimits_AUserWithAStreamRunningCannotStartAnotherAIRequest(t *testing.T) {
	e := newLimitEnv(t, func(c *limClock) router.Limits {
		return router.Limits{LLM: []gin.HandlerFunc{
			middleware.LimitConcurrencyByUser(mustSemaphore(t, 1)),
			middleware.RateLimitByUser(mustLimiter(t, c, 60, 30)),
		}}
	})

	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- e.call("POST", docPath+"/ask", "alice", holdJSON) }()
	<-e.asker.entered // alice's answer is streaming

	w := e.call("POST", docPath+"/summarize", "alice", "")
	if w.Code != http.StatusTooManyRequests || reason(t, w) != "concurrent_limit" || w.Header().Get("Retry-After") != "" {
		t.Errorf("summarize while asking: status %d: %s", w.Code, w.Body.String())
	}
	if w := e.call("GET", docPath+"/messages", "alice", ""); w.Code != http.StatusOK {
		t.Errorf("reading messages is not limited: status %d", w.Code)
	}
	if w := e.call("POST", docPath+"/ask", "bob", askJSON); w.Code != http.StatusOK {
		t.Errorf("another user: status %d", w.Code)
	}

	close(e.asker.release)
	if w := <-first; w.Code != http.StatusOK {
		t.Errorf("the held ask: status %d", w.Code)
	}
	if w := e.call("POST", docPath+"/summarize", "alice", ""); w.Code != http.StatusAccepted {
		t.Errorf("after the answer finished: status %d", w.Code)
	}
}

func TestLimits_UploadsAreLimitedPerUser(t *testing.T) {
	e := newLimitEnv(t, func(c *limClock) router.Limits {
		return router.Limits{Upload: []gin.HandlerFunc{middleware.RateLimitByUser(mustLimiter(t, c, 6, 2))}}
	})

	// No file is attached, so the handler answers 400 each time: the limiter
	// counts the attempt regardless, which is the point.
	for i := 0; i < 2; i++ {
		if w := e.call("POST", "/api/documents", "alice", ""); w.Code != http.StatusBadRequest {
			t.Fatalf("upload %d: status %d, want 400 from the handler", i, w.Code)
		}
	}
	if w := e.call("POST", "/api/documents", "alice", ""); w.Code != http.StatusTooManyRequests || reason(t, w) != "rate_limit" {
		t.Errorf("third upload: status %d", w.Code)
	}
	if w := e.call("POST", "/api/documents", "bob", ""); w.Code != http.StatusBadRequest {
		t.Errorf("bob: status %d, want his own budget", w.Code)
	}
	if w := e.call("GET", "/api/documents", "alice", ""); w.Code != http.StatusOK {
		t.Errorf("listing is not limited: status %d", w.Code)
	}
}

func TestLimits_NoLimitsConfiguredMeansNoLimits(t *testing.T) {
	e := newLimitEnv(t, nil)
	for i := 0; i < 60; i++ {
		if w := e.call("POST", "/api/auth/login", "", loginJSON); w.Code != http.StatusOK {
			t.Fatalf("login %d: status %d", i, w.Code)
		}
		if w := e.call("POST", docPath+"/ask", "alice", askJSON); w.Code != http.StatusOK {
			t.Fatalf("ask %d: status %d", i, w.Code)
		}
	}
}