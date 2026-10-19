package llm

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/service"
)

// capture reads a file saved with "curl -i" and splits it into the HTTP status
// and the body. A missing file skips the test.
func capture(t *testing.T, name string) (status int, body []byte) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if errors.Is(err, os.ErrNotExist) {
		t.Skipf(`copy scratch\%s into internal\llm\testdata to enable this test`, name)
	}
	if err != nil {
		t.Fatal(err)
	}

	sep := []byte("\r\n\r\n")
	i := bytes.Index(data, sep)
	if i < 0 {
		sep = []byte("\n\n")
		i = bytes.Index(data, sep)
	}
	if i < 0 {
		t.Fatalf("%s has no blank line between headers and body: was it saved with curl -i?", name)
	}
	fields := strings.Fields(strings.SplitN(string(data[:i]), "\n", 2)[0])
	if len(fields) < 2 {
		t.Fatalf("%s: cannot read the status line", name)
	}
	status, err = strconv.Atoi(fields[1])
	if err != nil {
		t.Fatalf("%s: bad status %q", name, fields[1])
	}
	return status, data[i+len(sep):]
}

func replay(t *testing.T, status int, contentType string, body []byte) (*Client, *fakeGroq) {
	t.Helper()
	f := newFakeGroq(t, func(n int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	})
	c, _ := newTestClient(t, f, nil)
	return c, f
}

func TestGolden_RealStream(t *testing.T) {
	status, body := capture(t, "stream.txt")
	c, _ := replay(t, status, "text/event-stream", body)

	var log textLog
	got, err := c.Stream(context.Background(), chatReq(), log.add)
	if err != nil {
		t.Fatalf("the real stream did not parse: %v", err)
	}
	if got.Content == "" || got.Content != strings.Join(log.all(), "") {
		t.Errorf("content %q does not equal the delivered pieces %q", got.Content, log.all())
	}
	if got.FinishReason != "stop" {
		t.Errorf("finish reason = %q, want stop", got.FinishReason)
	}
	if u := got.Usage; u.PromptTokens == 0 || u.CompletionTokens == 0 || u.TotalTokens != u.PromptTokens+u.CompletionTokens {
		t.Errorf("usage = %+v", u)
	}
	t.Logf("real stream: %q, usage %+v", got.Content, got.Usage)
}

func TestGolden_RealNonStreaming(t *testing.T) {
	status, body := capture(t, "nonstream.txt")
	c, _ := replay(t, status, "application/json", body)

	got, err := c.Complete(context.Background(), chatReq())
	if err != nil {
		t.Fatalf("the real response did not parse: %v", err)
	}
	if got.Content == "" || got.FinishReason != "stop" {
		t.Errorf("response = %+v", got)
	}
	if u := got.Usage; u.TotalTokens != u.PromptTokens+u.CompletionTokens || u.ReasoningTokens == 0 {
		t.Errorf("usage = %+v: reasoning tokens should be reported", u)
	}
}

func TestGolden_RealErrors(t *testing.T) {
	tests := []struct {
		file   string
		status int
		code   string
	}{
		{"badmodel.txt", 404, "model_not_found"},
		{"badkey.txt", 401, "invalid_api_key"},
	}
	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) {
			status, body := capture(t, tc.file)
			c, f := replay(t, status, "application/json", body)

			_, err := c.Complete(context.Background(), chatReq())
			le := asLLMError(t, err)
			if le.Status != tc.status || le.Code != tc.code || le.Kind != service.LLMMisconfigured {
				t.Errorf("error = %+v, want status %d, code %s, misconfigured", le, tc.status, tc.code)
			}
			if f.count() != 1 {
				t.Errorf("%d requests: these errors must not be retried", f.count())
			}
		})
	}
}