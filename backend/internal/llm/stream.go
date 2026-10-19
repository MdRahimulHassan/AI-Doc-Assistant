package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/service"
)

var errLineTooLong = errors.New("llm: stream line too long")

type streamChunk struct {
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *usageBody `json:"usage"`
	XGroq *struct {
		Usage *usageBody `json:"usage"`
	} `json:"x_groq"`
	Error *apiError `json:"error"`
}

// streamState is what has been received so far. It survives retries only
// while nothing has been delivered.
type streamState struct {
	content   strings.Builder
	delivered bool
	model     string
	finish    string
	usage     service.TokenUsage
	sawDone   bool
}

func (st *streamState) reset() {
	st.content.Reset()
	st.delivered, st.model, st.finish, st.usage, st.sawDone = false, "", "", service.TokenUsage{}, false
}

func (st *streamState) response() service.ChatResponse {
	return service.ChatResponse{
		Model:        st.model,
		Content:      st.content.String(),
		FinishReason: st.finish,
		Usage:        st.usage,
	}
}

func (c *Client) Stream(ctx context.Context, req service.ChatRequest, onText func(text string) error) (service.ChatResponse, error) {
	if onText == nil {
		return service.ChatResponse{}, errors.New("llm: onText is required")
	}
	p, err := c.newCall(req, true)
	if err != nil {
		return service.ChatResponse{}, err
	}

	st := &streamState{}
	err = c.do(ctx, p, func(resp *http.Response, abort context.CancelFunc) (bool, error) {
		return c.consume(resp, abort, st, onText)
	})
	return st.response(), err // on failure this is the PARTIAL answer
}

// consume reads one streaming response. Events are separated by blank lines;
// "data:" lines carry the payload; lines starting with ":" are comments or
// keep-alives. It reports whether text was already handed to onText.
func (c *Client) consume(resp *http.Response, abort context.CancelFunc, st *streamState, onText func(string) error) (bool, error) {
	defer resp.Body.Close()
	if !st.delivered {
		st.reset() // a retry starts from nothing
	}

	// The idle watchdog: if no line (not even a keep-alive) arrives for too
	// long, abort this attempt. Aborting makes the pending read fail, and
	// idleFired tells us why.
	var idleFired atomic.Bool
	watchdog := time.AfterFunc(c.cfg.StreamIdleTimeout, func() {
		idleFired.Store(true)
		abort()
	})
	defer watchdog.Stop()

	reader := bufio.NewReaderSize(resp.Body, 64<<10)
	var data []string // the "data:" lines of the event being assembled

	dispatch := func() (done bool, err error) {
		if len(data) == 0 {
			return false, nil
		}
		payload := strings.Join(data, "\n")
		data = data[:0]
		return c.handleEvent(payload, st, onText)
	}

	for {
		line, err := readLine(reader, maxLineBytes)
		if err != nil {
			switch {
			case errors.Is(err, io.EOF):
				// A clean end of the body. An event with no blank line after it still counts.
				done, derr := dispatch()
				if derr != nil {
					return st.delivered, derr
				}
				if done || st.sawDone || st.finish != "" {
					return st.delivered, nil
				}
				return st.delivered, &service.LLMError{
					Kind: service.LLMUnavailable, Message: "the stream ended before it was complete"}
			case idleFired.Load():
				return st.delivered, &service.LLMError{
					Kind:    service.LLMTimeout,
					Message: fmt.Sprintf("no data received for %v", c.cfg.StreamIdleTimeout),
				}
			case errors.Is(err, errLineTooLong):
				return st.delivered, &service.LLMError{
					Kind: service.LLMBadResponse, Message: "the stream contains an oversized line"}
			default:
				return st.delivered, c.transportError(err)
			}
		}
		watchdog.Reset(c.cfg.StreamIdleTimeout)

		switch {
		case len(line) == 0: // a blank line completes the event
			done, derr := dispatch()
			if derr != nil {
				return st.delivered, derr
			}
			if done {
				return st.delivered, nil
			}
		case line[0] == ':': // comment or keep-alive
		case bytes.HasPrefix(line, []byte("data:")):
			value := line[len("data:"):]
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
			data = append(data, string(value))
		default: // event:, id:, retry: or an unknown field: not needed
		}
	}
}

// handleEvent processes one event's payload.
func (c *Client) handleEvent(payload string, st *streamState, onText func(string) error) (done bool, err error) {
	if payload == "[DONE]" {
		st.sawDone = true
		return true, nil
	}

	var chunk streamChunk
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return false, &service.LLMError{
			Kind: service.LLMBadResponse, Message: "a stream event is not valid JSON", Err: err}
	}

	if chunk.Error != nil && (chunk.Error.Message != "" || chunk.Error.Type != "") {
		kind := service.LLMUnavailable
		if chunk.Error.Code == "rate_limit_exceeded" {
			kind = service.LLMBusy
		}
		return false, &service.LLMError{Kind: kind, Code: chunk.Error.Code, Message: c.redact(chunk.Error.Message)}
	}

	if chunk.Model != "" {
		st.model = chunk.Model
	}
	if len(chunk.Choices) > 0 {
		ch := chunk.Choices[0]
		// Chunks without text (the first one, reasoning-only deltas, the last
		// one) are skipped: only real text is delivered.
		if ch.Delta.Content != "" {
			st.content.WriteString(ch.Delta.Content)
			st.delivered = true
			if err := onText(ch.Delta.Content); err != nil {
				return false, fmt.Errorf("llm: stream stopped by the caller: %w", err)
			}
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			st.finish = *ch.FinishReason
		}
	}

	// Usage comes in the last chunk, twice: at the top level and inside x_groq.
	switch {
	case chunk.Usage != nil:
		st.usage = chunk.Usage.toUsage()
	case chunk.XGroq != nil && chunk.XGroq.Usage != nil:
		st.usage = chunk.XGroq.Usage.toUsage()
	}
	return false, nil
}

// readLine reads one line without a newline, however long, up to max bytes.
// (bufio.Scanner would stop at 64 KB by default.)
func readLine(r *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	for {
		frag, err := r.ReadSlice('\n')
		if len(line)+len(frag) > max+1 {
			return nil, errLineTooLong
		}
		line = append(line, frag...) // a copy: the reader reuses its buffer
		switch {
		case err == nil:
			return trimEOL(line), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(line) > 0:
			return trimEOL(line), nil // the last line had no newline
		default:
			return nil, err
		}
	}
}

func trimEOL(b []byte) []byte {
	b = bytes.TrimSuffix(b, []byte("\n"))
	return bytes.TrimSuffix(b, []byte("\r"))
}