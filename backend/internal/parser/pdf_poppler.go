package parser

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// popplerTimeout bounds one pdftotext run. Unlike the pure-Go library, a
// subprocess CAN be killed when a pathological PDF makes it spin.
const popplerTimeout = 60 * time.Second

// Poppler extracts text by running poppler's pdftotext.
type Poppler struct{ bin string }

// NewPoppler returns a Poppler parser if pdftotext is installed.
func NewPoppler() (Poppler, bool) {
	bin, err := exec.LookPath("pdftotext")
	if err != nil {
		return Poppler{}, false
	}
	return Poppler{bin: bin}, true
}

func (p Poppler) Parse(ctx context.Context, r io.ReaderAt, size int64) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, popplerTimeout)
	defer cancel()

	// pdftotext needs a real file. Writing a temp copy keeps this working for
	// any FileStorage (local disk today, S3 later). It exists only for this call.
	tmp, err := os.CreateTemp("", "ingest-*.pdf")
	if err != nil {
		return "", fmt.Errorf("pdftotext: create temp file: %w", err)
	}
	defer os.Remove(tmp.Name()) // runs second...
	defer tmp.Close()           // ...because this runs first (Windows can't delete an open file)

	if _, err := io.Copy(tmp, io.NewSectionReader(r, 0, size)); err != nil {
		return "", fmt.Errorf("pdftotext: write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("pdftotext: close temp file: %w", err)
	}

	// Arguments are passed as a list (no shell), and the path is one we created.
	// Default mode (no -layout) uses poppler's reading-order logic, which copes
	// with multi-column pages. Pages are separated by a form feed, which Clean
	// turns into a paragraph break. "-" sends the text to stdout.
	cmd := exec.CommandContext(ctx, p.bin, "-enc", "UTF-8", "-eol", "unix", tmp.Name(), "-")

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("pdftotext: stdout pipe: %w", err)
	}
	stderr := &cappedBuffer{max: 4 << 10} // poppler can print thousands of warnings
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("pdftotext: start: %w", err)
	}

	// Read at most MaxTextBytes+1 bytes: the extra byte proves the cap was passed.
	out, readErr := io.ReadAll(io.LimitReader(stdout, MaxTextBytes+1))
	tooLarge := len(out) > MaxTextBytes
	if tooLarge || readErr != nil {
		_ = cmd.Process.Kill() // we stopped reading, so it would block forever on a full pipe
	}
	waitErr := cmd.Wait()

	switch {
	case ctx.Err() != nil: // timeout, or the caller cancelled
		return "", ctx.Err()
	case tooLarge:
		return "", ErrTextTooLarge
	case readErr != nil:
		return "", fmt.Errorf("pdftotext: read output: %w", readErr)
	case waitErr != nil:
		// A non-zero exit means poppler could not open the file: damaged,
		// not a PDF, or encrypted. The detail is for server logs only.
		return "", fmt.Errorf("%w: pdftotext: %v: %s", ErrCorrupt, waitErr, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// cappedBuffer keeps only the first max bytes written to it.
type cappedBuffer struct {
	buf bytes.Buffer
	max int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		if len(p) > room {
			c.buf.Write(p[:room])
		} else {
			c.buf.Write(p)
		}
	}
	return len(p), nil // claim success, so the process never blocks on a full stderr
}

func (c *cappedBuffer) String() string { return c.buf.String() }