package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/ratelimit"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/pkg/response"
)

// --- helpers ---

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestLimiter(t *testing.T, clock *fakeClock, cfg ratelimit.Config) *ratelimit.KeyedLimiter {
	t.Helper()
	cfg.Now = clock.Now
	l, err := ratelimit.NewKeyedLimiter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func newTestSemaphore(t *testing.T, limit int) *ratelimit.KeyedSemaphore {
	t.Helper()
	s, err := ratelimit.NewKeyedSemaphore(limit)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// tokenUsers is a TokenParser: "alice" -> alice's user id.
type tokenUsers map[string]uuid.UUID

func (m tokenUsers) Parse(token string) (uuid.UUID, error) {
	if id, ok := m[token]; ok {
		return id, nil
	}
	return uuid.Nil, errors.New("unknown token")
}

// limitedApp is one route, GET /work, behind Auth and the given limits.
//
//	?mode=hold            enters, then waits until release is closed
//	?mode=panic           panics
//	?mode=wait-for-client waits until the client disconnects
type limitedApp struct {
	router  *gin.Engine
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func newLimitedApp(users tokenUsers, limits ...gin.HandlerFunc) *limitedApp {
	gin.SetMode(gin.TestMode)
	app := &limitedApp{entered: make(chan struct{}, 16), release: make(chan struct{})}

	chain := []gin.HandlerFunc{Auth(users)}
	chain = append(chain, limits...)
	chain = append(chain, func(c *gin.Context) {
		app.calls.Add(1)
		switch c.Query("mode") {
		case "hold":
			app.entered <- struct{}{}
			<-app.release
		case "panic":
			panic("handler bug")
		case "wait-for-client":
			<-c.Request.Context().Done()
		}
		c.String(http.StatusOK, "ok")
	})

	r := gin.New()
	r.Use(gin.Recovery())
	r.GET("/work", chain...)
	app.router = r
	return app
}

func getAs(app *limitedApp, token, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/work"+query, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	app.router.ServeHTTP(w, req)
	return w
}

type limitBody struct {
	Error *struct {
		Code    string `json:"code"`
		Details struct {
			Reason            string `json:"reason"`
			RetryAfterSeconds int    `json:"retry_after_seconds"`
		} `json:"details"`
	} `json:"error"`
}

func decodeLimit(t *testing.T, w *httptest.ResponseRecorder) limitBody {
	t.Helper()
	var b limitBody
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("not a JSON envelope: %v\n%s", err, w.Body.String())
	}
	return b
}

func requireOK(t *testing.T, w *httptest.ResponseRecorder, label string) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("%s: status %d, want 200: %s", label, w.Code, w.Body.String())
	}
}

// --- the rate limit ---

func TestRateLimitByUser_BurstBlockAndRefill(t *testing.T) {
	clock := newFakeClock()
	lim := newTestLimiter(t, clock, ratelimit.Config{PerMinute: 6, Burst: 2})
	alice, bob := uuid.New(), uuid.New()
	app := newLimitedApp(tokenUsers{"alice": alice, "bob": bob}, RateLimitByUser(lim))

	requireOK(t, getAs(app, "alice", ""), "first")
	requireOK(t, getAs(app, "alice", ""), "second")

	w := getAs(app, "alice", "")
	body := decodeLimit(t, w)
	if w.Code != http.StatusTooManyRequests || body.Error == nil ||
		body.Error.Code != response.CodeRateLimited || body.Error.Details.Reason != "rate_limit" {
		t.Fatalf("third request: status %d: %s", w.Code, w.Body.String())
	}
	secs, err := strconv.Atoi(w.Header().Get("Retry-After"))
	if err != nil || secs < 9 || secs > 10 || body.Error.Details.RetryAfterSeconds != secs {
		t.Errorf("Retry-After %q, details %d; want the same whole number of seconds, about 10", w.Header().Get("Retry-After"), body.Error.Details.RetryAfterSeconds)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
	if app.calls.Load() != 2 {
		t.Errorf("the handler ran %d times: a refused request must never reach it", app.calls.Load())
	}

	requireOK(t, getAs(app, "bob", ""), "bob") // independent of alice

	clock.Advance(time.Duration(secs) * time.Second) // waiting what the header says is enough
	requireOK(t, getAs(app, "alice", ""), "after waiting")
}

func TestRateLimit_RefusalsDoNotUseTokens(t *testing.T) {
	clock := newFakeClock()
	lim := newTestLimiter(t, clock, ratelimit.Config{PerMinute: 6, Burst: 1})
	app := newLimitedApp(tokenUsers{"alice": uuid.New()}, RateLimitByUser(lim))

	requireOK(t, getAs(app, "alice", ""), "first")
	var secs int
	for i := 0; i < 10; i++ {
		w := getAs(app, "alice", "")
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("request %d: status %d", i, w.Code)
		}
		secs, _ = strconv.Atoi(w.Header().Get("Retry-After"))
	}

	clock.Advance(time.Duration(secs) * time.Second)
	requireOK(t, getAs(app, "alice", ""), "after the advertised wait")
	if w := getAs(app, "alice", ""); w.Code != http.StatusTooManyRequests {
		t.Errorf("immediately again: status %d, want 429", w.Code)
	}
}

func TestRateLimit_RunsAfterAuthentication(t *testing.T) {
	clock := newFakeClock()
	lim := newTestLimiter(t, clock, ratelimit.Config{PerMinute: 6, Burst: 1})
	app := newLimitedApp(tokenUsers{"alice": uuid.New()}, RateLimitByUser(lim))

	for i := 0; i < 10; i++ {
		if w := getAs(app, "", ""); w.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous request %d: status %d, want 401 (never 429)", i, w.Code)
		}
	}
	if lim.Len() != 0 {
		t.Errorf("%d keys tracked: anonymous requests must never reach the limiter", lim.Len())
	}
	requireOK(t, getAs(app, "alice", ""), "the real user still has her whole burst")
}

func TestRateLimitByUser_WithoutAuthIsAWiringBugNotASilentPass(t *testing.T) {
	gin.SetMode(gin.TestMode)
	lim := newTestLimiter(t, newFakeClock(), ratelimit.Config{PerMinute: 6})
	r := gin.New()
	r.GET("/x", RateLimitByUser(lim), func(c *gin.Context) { c.String(200, "ok") }) // no Auth in front

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status %d, want 500: an unlimited route must not appear to work", w.Code)
	}
}

// --- the concurrency cap ---

func TestLimitConcurrencyByUser(t *testing.T) {
	sem := newTestSemaphore(t, 1)
	alice, bob := uuid.New(), uuid.New()
	app := newLimitedApp(tokenUsers{"alice": alice, "bob": bob}, LimitConcurrencyByUser(sem))

	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- getAs(app, "alice", "?mode=hold") }()
	<-app.entered

	w := getAs(app, "alice", "") // refused at once, without waiting
	body := decodeLimit(t, w)
	if w.Code != http.StatusTooManyRequests || body.Error == nil ||
		body.Error.Code != response.CodeRateLimited || body.Error.Details.Reason != "concurrent_limit" {
		t.Fatalf("second request: status %d: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") != "" {
		t.Errorf("Retry-After = %q: there is no honest number to give", w.Header().Get("Retry-After"))
	}
	if app.calls.Load() != 1 {
		t.Errorf("the handler ran %d times", app.calls.Load())
	}
	requireOK(t, getAs(app, "bob", ""), "bob is independent")

	close(app.release)
	requireOK(t, <-first, "the first request")
	if sem.Keys() != 0 {
		t.Errorf("%d keys still hold a slot", sem.Keys())
	}
	requireOK(t, getAs(app, "alice", ""), "after the first finished")
}

func TestLimitConcurrencyByUser_SlotIsReleasedWhenTheHandlerPanics(t *testing.T) {
	sem := newTestSemaphore(t, 1)
	app := newLimitedApp(tokenUsers{"alice": uuid.New()}, LimitConcurrencyByUser(sem))

	if w := getAs(app, "alice", "?mode=panic"); w.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500 from the recovery middleware", w.Code)
	}
	if sem.Keys() != 0 {
		t.Fatal("a panic leaked the slot")
	}
	requireOK(t, getAs(app, "alice", ""), "after the panic")
}

func TestLimitConcurrencyByUser_SlotIsReleasedWhenTheClientDisconnects(t *testing.T) {
	sem := newTestSemaphore(t, 1)
	alice := uuid.New()
	app := newLimitedApp(tokenUsers{"alice": alice}, LimitConcurrencyByUser(sem))

	srv := httptest.NewServer(app.router)
	defer srv.Close() // registered first, so it runs after cancel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/work?mode=wait-for-client", nil)
	req.Header.Set("Authorization", "Bearer alice")
	go func() {
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()

	eventually(t, "the request to hold its slot", func() bool { return sem.InFlight(alice.String()) == 1 })
	cancel() // the browser tab is closed
	eventually(t, "the slot to be released after the disconnect", func() bool { return sem.Keys() == 0 })
}

// --- both together, as the router uses them ---

func TestLLMChain_ARateRefusalFreesTheSlot_AConcurrencyRefusalCostsNoToken(t *testing.T) {
	t.Run("rate refusal releases the slot", func(t *testing.T) {
		clock := newFakeClock()
		lim := newTestLimiter(t, clock, ratelimit.Config{PerMinute: 6, Burst: 1})
		sem := newTestSemaphore(t, 1)
		app := newLimitedApp(tokenUsers{"alice": uuid.New()}, LimitConcurrencyByUser(sem), RateLimitByUser(lim))

		requireOK(t, getAs(app, "alice", ""), "first")
		w := getAs(app, "alice", "")
		if body := decodeLimit(t, w); w.Code != 429 || body.Error.Details.Reason != "rate_limit" {
			t.Fatalf("second: status %d, %s", w.Code, w.Body.String())
		}
		if sem.Keys() != 0 {
			t.Error("a request refused by the rate limiter kept its concurrency slot")
		}
	})

	t.Run("concurrency refusal takes no rate token", func(t *testing.T) {
		clock := newFakeClock()
		lim := newTestLimiter(t, clock, ratelimit.Config{PerMinute: 6, Burst: 2}) // two tokens in all
		sem := newTestSemaphore(t, 1)
		app := newLimitedApp(tokenUsers{"alice": uuid.New()}, LimitConcurrencyByUser(sem), RateLimitByUser(lim))

		held := make(chan *httptest.ResponseRecorder, 1)
		go func() { held <- getAs(app, "alice", "?mode=hold") }() // token 1
		<-app.entered

		for i := 0; i < 5; i++ { // refused for concurrency, five times
			if w := getAs(app, "alice", ""); decodeLimit(t, w).Error.Details.Reason != "concurrent_limit" {
				t.Fatalf("request %d: %s", i, w.Body.String())
			}
		}
		close(app.release)
		requireOK(t, <-held, "the held request")

		requireOK(t, getAs(app, "alice", ""), "token 2 must still be there") // it would be gone if refusals took tokens
		if w := getAs(app, "alice", ""); w.Code != http.StatusTooManyRequests {
			t.Errorf("a third token appeared: status %d", w.Code)
		}
	})
}

// --- limiting by IP ---

func newIPApp(lim *ratelimit.KeyedLimiter) http.Handler {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	_ = r.SetTrustedProxies(nil) // as the real router does
	r.GET("/x", RateLimitByIP(lim), func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	return r
}

func fromIP(h http.Handler, ip string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = net.JoinHostPort(ip, "4321")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestRateLimitByIP(t *testing.T) {
	clock := newFakeClock()
	h := newIPApp(newTestLimiter(t, clock, ratelimit.Config{PerMinute: 6, Burst: 2}))

	for i := 0; i < 2; i++ {
		requireOK(t, fromIP(h, "203.0.113.7"), "within the burst")
	}
	w := fromIP(h, "203.0.113.7")
	body := decodeLimit(t, w)
	if w.Code != 429 || body.Error == nil || body.Error.Details.Reason != "rate_limit" || w.Header().Get("Retry-After") == "" {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}

	requireOK(t, fromIP(h, "203.0.113.8"), "a different address")

	// A forged header must not buy a new bucket.
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = net.JoinHostPort("203.0.113.7", "4321")
	req.Header.Set("X-Forwarded-For", "198.51.100.99")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("X-Forwarded-For bought a fresh bucket: status %d", rec.Code)
	}
}

func TestRateLimitByIP_IPv6IsLimitedBySlash64(t *testing.T) {
	h := newIPApp(newTestLimiter(t, newFakeClock(), ratelimit.Config{PerMinute: 6, Burst: 1}))

	requireOK(t, fromIP(h, "2001:db8:1:2:aaaa:bbbb:cccc:dddd"), "first")
	if w := fromIP(h, "2001:db8:1:2:1:2:3:4"); w.Code != http.StatusTooManyRequests {
		t.Errorf("another address in the same /64: status %d, want 429", w.Code)
	}
	requireOK(t, fromIP(h, "2001:db8:1:3::1"), "a different /64")
}

func TestClientKey(t *testing.T) {
	tests := map[string]string{
		"203.0.113.7":                      "203.0.113.7",
		"::ffff:203.0.113.7":               "203.0.113.7",
		"2001:db8:1:2:aaaa:bbbb:cccc:dddd": "2001:db8:1:2::/64",
		"2001:db8:1:2:1:2:3:4":             "2001:db8:1:2::/64",
		"fe80::1%eth0":                     "fe80::/64",
		"  198.51.100.5 ":                  "198.51.100.5",
		"":                                 "unknown",
		"not an address":                   "unknown",
	}
	for in, want := range tests {
		if got := clientKey(in); got != want {
			t.Errorf("clientKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRetryAfterSeconds(t *testing.T) {
	tests := map[time.Duration]int{
		time.Nanosecond:        1,
		500 * time.Millisecond: 1,
		time.Second:            1,
		1200 * time.Millisecond: 2,
		10 * time.Second:       10,
		0:                      1,
		-time.Second:           1,
	}
	for d, want := range tests {
		if got := retryAfterSeconds(d); got != want {
			t.Errorf("retryAfterSeconds(%v) = %d, want %d", d, got, want)
		}
	}
}