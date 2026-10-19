// Package llm is the client for Groq's OpenAI-compatible chat API.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/service"
)

const (
	DefaultBaseURL = "https://api.groq.com/openai/v1"

	maxErrorBody    = 64 << 10 // how much of an error response is read
	maxResponseBody = 8 << 20  // largest non-streaming response accepted
	maxLineBytes    = 1 << 20  // largest single SSE line accepted
)

// Config holds the client's settings. Zero or negative values use the defaults.
type Config struct {
	APIKey    string // required
	BaseURL   string // default DefaultBaseURL
	TPMBudget int    // required: tokens per minute this app may spend

	MaxAttempts  int           // total tries per call, default 4
	MaxRetryWait time.Duration // longest wait for an interactive call, default 20s
	BackoffBase  time.Duration // first retry wait ceiling, default 500ms
	BackoffMax   time.Duration // retry wait ceiling, default 8s

	RequestTimeout        time.Duration // one non-streaming attempt, default 60s
	StreamMaxDuration     time.Duration // one streaming attempt, default 5m
	StreamIdleTimeout     time.Duration // no data for this long aborts a stream, default 30s
	ResponseHeaderTimeout time.Duration // waiting for the response to start, default 30s
}

func (c *Config) applyDefaults() {
	if c.BaseURL == "" {
		c.BaseURL = DefaultBaseURL
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 4
	}
	if c.MaxRetryWait <= 0 {
		c.MaxRetryWait = 20 * time.Second
	}
	if c.BackoffBase <= 0 {
		c.BackoffBase = 500 * time.Millisecond
	}
	if c.BackoffMax <= 0 {
		c.BackoffMax = 8 * time.Second
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 60 * time.Second
	}
	if c.StreamMaxDuration <= 0 {
		c.StreamMaxDuration = 5 * time.Minute
	}
	if c.StreamIdleTimeout <= 0 {
		c.StreamIdleTimeout = 30 * time.Second
	}
	if c.ResponseHeaderTimeout <= 0 {
		c.ResponseHeaderTimeout = 30 * time.Second
	}
}

// Client talks to Groq. It is safe for concurrent use.
type Client struct {
	cfg      Config
	http     *http.Client
	pacer    *pacer
	endpoint string

	// Replaceable in tests, so retries need no real waiting.
	sleep  func(ctx context.Context, d time.Duration) error
	jitter func(ceiling time.Duration) time.Duration
	now    func() time.Time
}

var _ service.ChatClient = (*Client)(nil)

func New(cfg Config) (*Client, error) {
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	if cfg.APIKey == "" {
		return nil, errors.New("llm: an API key is required")
	}
	if cfg.TPMBudget < 1 {
		return nil, errors.New("llm: TPMBudget must be positive")
	}
	cfg.applyDefaults()

	base := strings.TrimRight(cfg.BaseURL, "/")
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("llm: invalid base URL %q", cfg.BaseURL)
	}

	// There is deliberately NO http.Client.Timeout: it also covers reading the
	// body, so it would cut long streams. The limits that matter live here
	// (connecting, TLS, waiting for the response to start), in per-attempt
	// context deadlines, and in the stream's idle watchdog.
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          20,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: cfg.ResponseHeaderTimeout,
		ExpectContinueTimeout: time.Second,
	}

	return &Client{
		cfg: cfg,
		http: &http.Client{
			Transport: transport,
			// A redirect would turn the POST into something else and could
			// carry the API key elsewhere: treat it as an error instead.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		pacer:    newPacer(cfg.TPMBudget),
		endpoint: base + "/chat/completions",
		sleep:    sleepContext,
		jitter:   fullJitter,
		now:      time.Now,
	}, nil
}

// Close releases idle connections.
func (c *Client) Close() { c.http.CloseIdleConnections() }

// ---------------------------------------------------------------------------
// building a call
// ---------------------------------------------------------------------------

type messageBody struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type requestBody struct {
	Model               string        `json:"model"`
	Messages            []messageBody `json:"messages"`
	Temperature         float64       `json:"temperature"`
	MaxCompletionTokens int           `json:"max_completion_tokens"`
	ReasoningEffort     string        `json:"reasoning_effort,omitempty"`
	IncludeReasoning    *bool         `json:"include_reasoning,omitempty"`
	Stream              bool          `json:"stream"`
}

// call is one prepared request.
type call struct {
	body    []byte
	cost    int           // estimated tokens, charged to the pacer on every attempt
	stream  bool
	timeout time.Duration // per attempt
	maxWait time.Duration // how long this call may wait for budget or retries
}

func (c *Client) newCall(req service.ChatRequest, stream bool) (call, error) {
	switch {
	case strings.TrimSpace(req.Model) == "":
		return call{}, errors.New("llm: a model is required")
	case len(req.Messages) == 0:
		return call{}, errors.New("llm: at least one message is required")
	case req.MaxOutputTokens < 1:
		return call{}, errors.New("llm: MaxOutputTokens must be set")
	}

	body := requestBody{
		Model:               req.Model,
		Temperature:         req.Temperature,
		MaxCompletionTokens: req.MaxOutputTokens,
		Stream:              stream,
	}
	for _, m := range req.Messages {
		if m.Role != "system" && m.Role != "user" && m.Role != "assistant" {
			return call{}, fmt.Errorf("llm: unknown message role %q", m.Role)
		}
		body.Messages = append(body.Messages, messageBody{Role: m.Role, Content: m.Content})
	}

	// Reasoning settings are sent only when asked for, because models that do
	// not reason may reject them. When they are sent, the reasoning text is
	// switched off, so it can never reach a client.
	switch req.ReasoningEffort {
	case "":
	case "low", "medium", "high":
		off := false
		body.ReasoningEffort = req.ReasoningEffort
		body.IncludeReasoning = &off
	default:
		return call{}, fmt.Errorf("llm: unknown reasoning effort %q", req.ReasoningEffort)
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return call{}, fmt.Errorf("llm: encode request: %w", err)
	}

	prompt := req.EstimatedPromptTokens
	if prompt <= 0 {
		prompt = service.EstimateMessagesTokens(req.Messages)
	}
	maxWait := req.MaxWait
	if maxWait <= 0 {
		maxWait = c.cfg.MaxRetryWait
	}
	timeout := c.cfg.RequestTimeout
	if stream {
		timeout = c.cfg.StreamMaxDuration
	}

	return call{body: raw, cost: prompt + req.MaxOutputTokens, stream: stream, timeout: timeout, maxWait: maxWait}, nil
}

// ---------------------------------------------------------------------------
// the retry loop
// ---------------------------------------------------------------------------

// handler reads the body of a 2xx response and closes it. delivered reports
// whether it already handed output to the caller: after that a retry would
// repeat text, so none is attempted.
type handler func(resp *http.Response, abort context.CancelFunc) (delivered bool, err error)

func (c *Client) do(ctx context.Context, p call, handle handler) error {
	for attempt := 1; ; attempt++ {
		delivered, err := c.attempt(ctx, p, handle)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil { // the caller gave up: stop at once
			return fmt.Errorf("llm: request stopped: %w", ctx.Err())
		}
		if delivered || attempt >= c.cfg.MaxAttempts || !retryable(err) {
			return err
		}

		wait, ok := c.retryDelay(err, attempt, p.maxWait)
		if !ok { // the provider asked for a longer wait than this caller allows
			return err
		}
		slog.Warn("model request failed; retrying",
			"attempt", attempt, "max_attempts", c.cfg.MaxAttempts, "wait", wait, "error", err)
		if serr := c.sleep(ctx, wait); serr != nil {
			return fmt.Errorf("llm: request stopped: %w", serr)
		}
	}
}

// attempt makes one try: wait for budget, send, and let the handler read the answer.
func (c *Client) attempt(ctx context.Context, p call, handle handler) (bool, error) {
	if err := c.pacer.wait(ctx, p.cost, p.maxWait); err != nil {
		return false, err
	}

	actx, abort := context.WithTimeout(ctx, p.timeout)
	defer abort()

	resp, err := c.send(actx, p)
	if err != nil {
		return false, err
	}
	return handle(resp, abort)
}

// retryDelay says how long to wait before retry number `attempt`. A
// Retry-After from the provider wins; without one the wait is an exponential
// backoff with full jitter. ok is false when the provider asks for more than
// maxWait: the caller should fail fast instead.
func (c *Client) retryDelay(err error, attempt int, maxWait time.Duration) (time.Duration, bool) {
	var le *service.LLMError
	if errors.As(err, &le) && le.RetryAfter > 0 {
		if le.RetryAfter > maxWait {
			return 0, false
		}
		return le.RetryAfter, true
	}
	return c.jitter(backoffCeiling(c.cfg.BackoffBase, c.cfg.BackoffMax, attempt)), true
}

// ---------------------------------------------------------------------------
// one HTTP exchange
// ---------------------------------------------------------------------------

func (c *Client) send(ctx context.Context, p call) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(p.body))
	if err != nil {
		return nil, fmt.Errorf("llm: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey) // never logged
	req.Header.Set("Content-Type", "application/json")
	if p.stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, c.transportError(err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	return nil, c.statusError(resp)
}

type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// statusError turns a non-2xx response into an LLMError and closes the body.
func (c *Client) statusError(resp *http.Response) *service.LLMError {
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))

	var env struct {
		Error apiError `json:"error"`
	}
	_ = json.Unmarshal(raw, &env) // a body that is not JSON just leaves the fields empty

	msg := env.Error.Message
	if msg == "" {
		msg = strings.TrimSpace(string(raw))
		if len(msg) > 300 {
			msg = msg[:300]
		}
	}
	code := env.Error.Code
	if code == "" {
		code = env.Error.Type
	}

	return &service.LLMError{
		Kind:       kindForStatus(resp.StatusCode),
		Status:     resp.StatusCode,
		Code:       code,
		Message:    c.redact(msg),
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), c.now()),
	}
}

func kindForStatus(status int) service.LLMErrorKind {
	switch {
	case status == http.StatusTooManyRequests:
		return service.LLMBusy
	case status == http.StatusUnauthorized, status == http.StatusForbidden, status == http.StatusNotFound:
		return service.LLMMisconfigured // your capture: an unknown model is a 404 (model_not_found)
	case status == http.StatusRequestTimeout:
		return service.LLMTimeout
	case status == http.StatusBadRequest, status == http.StatusRequestEntityTooLarge, status == http.StatusUnprocessableEntity:
		return service.LLMBadRequest
	case status >= 500:
		return service.LLMUnavailable
	case status >= 400:
		return service.LLMBadRequest
	default: // an unexpected 1xx or 3xx
		return service.LLMBadResponse
	}
}

// transportError classifies a failure to connect, send or read.
func (c *Client) transportError(err error) *service.LLMError {
	kind := service.LLMUnavailable
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() { // includes the response-header timeout and deadlines
		kind = service.LLMTimeout
	}
	return &service.LLMError{Kind: kind, Message: c.redact(err.Error()), Err: err}
}

// redact is a safety net: whatever text arrives, the API key is never part of it.
func (c *Client) redact(s string) string {
	return strings.ReplaceAll(s, c.cfg.APIKey, "[redacted]")
}

// ---------------------------------------------------------------------------
// Complete (non-streaming)
// ---------------------------------------------------------------------------

type usageBody struct {
	PromptTokens            int `json:"prompt_tokens"`
	CompletionTokens        int `json:"completion_tokens"`
	TotalTokens             int `json:"total_tokens"`
	ReasoningTokens         int `json:"reasoning_tokens"`
	CompletionTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

func (u usageBody) toUsage() service.TokenUsage {
	reasoning := u.ReasoningTokens
	if reasoning == 0 {
		reasoning = u.CompletionTokensDetails.ReasoningTokens
	}
	total := u.TotalTokens
	if total == 0 {
		total = u.PromptTokens + u.CompletionTokens
	}
	return service.TokenUsage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      total,
		ReasoningTokens:  reasoning,
	}
}

func (c *Client) Complete(ctx context.Context, req service.ChatRequest) (service.ChatResponse, error) {
	p, err := c.newCall(req, false)
	if err != nil {
		return service.ChatResponse{}, err
	}

	var out service.ChatResponse
	err = c.do(ctx, p, func(resp *http.Response, _ context.CancelFunc) (bool, error) {
		defer resp.Body.Close()

		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
		if err != nil {
			return false, c.transportError(err)
		}
		if len(raw) > maxResponseBody {
			return false, &service.LLMError{Kind: service.LLMBadResponse, Message: "the response is too large"}
		}
		out, err = c.parseCompletion(raw)
		return false, err
	})
	if err != nil {
		return service.ChatResponse{}, err
	}
	return out, nil
}

func (c *Client) parseCompletion(raw []byte) (service.ChatResponse, error) {
	var body struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *usageBody `json:"usage"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return service.ChatResponse{}, &service.LLMError{
			Kind: service.LLMBadResponse, Message: "the response is not valid JSON", Err: err}
	}
	if len(body.Choices) == 0 {
		return service.ChatResponse{}, &service.LLMError{
			Kind: service.LLMBadResponse, Message: "the response has no choices"}
	}

	out := service.ChatResponse{
		Model:        body.Model,
		Content:      body.Choices[0].Message.Content,
		FinishReason: body.Choices[0].FinishReason,
	}
	if body.Usage != nil {
		out.Usage = body.Usage.toUsage()
	}
	return out, nil
}