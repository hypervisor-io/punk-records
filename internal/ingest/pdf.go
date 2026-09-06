package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/hypervisor-io/punk-records/internal/memory"
)

// DefaultWaitDelay bounds how long cmd.Wait may block on adapter I/O pipes
// after the process has been cancelled. It is the portable backstop (all
// platforms) against a descendant that escaped the process group and keeps
// an inherited stdout/stderr pipe open, which would otherwise pin cmd.Wait
// (and the caller) indefinitely. group SIGKILL alone cannot reap an escaped
// descendant; WaitDelay is what guarantees the bound.
const DefaultWaitDelay = 250 * time.Millisecond

// PDFLoader delegates extraction to an optional external adapter process
// instead of embedding a PDF/OCR stack (the static binary stays useful
// without one; the unsupported-adapter error is actionable).
//
// Adapter contract: punk writes the PDF bytes to a temporary file and
// runs `<command...> <file>` with the Spec timeout. On exit code 0 the
// adapter's stdout must be one JSON object:
//
//	{"revision":"...", "sections":[{"name":"...", "page":1, "text":"..."}]}
//
// revision is optional (used only when the caller supplied none); page
// and name are optional per-section provenance. stdout is capped at
// MaxBytes, stderr is captured (bounded) into errors, a nonzero exit or
// unparsable/empty output fails the load, and the timeout kills the
// process - a failed adapter never yields a partially ingested document.
type PDFLoader struct {
	Command  []string      // executable plus fixed args; the input path is appended
	Timeout  time.Duration // <=0 uses DefaultTimeout
	MaxBytes int64         // <=0 uses DefaultMaxBytes; caps adapter stdout
}

func (PDFLoader) MediaType() string { return PDFMediaType }

// adapterWire is the adapter's stdout contract.
type adapterWire struct {
	Revision string `json:"revision"`
	Sections []struct {
		Name string `json:"name"`
		Page int    `json:"page"`
		Text string `json:"text"`
	} `json:"sections"`
}

// capWriter absorbs a stream without retaining more than max bytes
// (stderr: retained for error messages, never fatal).
type capWriter struct {
	buf bytes.Buffer
	max int
}

func (c *capWriter) Write(p []byte) (int, error) {
	// Report the original input length, never a truncated one, so an
	// io.Copy / pipe pump never sees ErrShortWrite and stops early: the
	// contract is to consume the whole stream, retaining only a bounded
	// prefix for error messages.
	n := len(p)
	if room := c.max - c.buf.Len(); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		c.buf.Write(p)
	}
	return n, nil
}

// limitWriter fails the stream once it grows past max bytes (stdout:
// runaway adapter output is an error, not a silent truncation).
type limitWriter struct {
	buf      bytes.Buffer
	max      int64
	exceeded bool
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if int64(l.buf.Len())+int64(len(p)) > l.max {
		l.exceeded = true
		return 0, fmt.Errorf("ingest: adapter output exceeds max_bytes (%d)", l.max)
	}
	return l.buf.Write(p)
}

func (l PDFLoader) Load(ctx context.Context, in LoadInput) (*Result, error) {
	if len(l.Command) == 0 || strings.TrimSpace(l.Command[0]) == "" {
		return nil, errors.New("ingest: pdf: no external extractor configured - punk does not bundle one; pass --pdf-adapter or set PUNK_INGEST_PDF_ADAPTER to a command implementing the adapter contract (docs/CONFIG.md#document-ingest)")
	}
	timeout := l.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	maxOut := l.MaxBytes
	if maxOut <= 0 {
		maxOut = DefaultMaxBytes
	}
	dir, err := os.MkdirTemp("", "punk-ingest-pdf-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	pdfPath := filepath.Join(dir, "input.pdf")
	if err := os.WriteFile(pdfPath, in.Body, 0o600); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	argv := append(append([]string{}, l.Command...), pdfPath)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	// configureAdapterCancel installs the process-group / cancellation
	// strategy (OS-specific) and cmd.Cancel. Group SIGKILL signals the
	// adapter's process group but does not reap the process nor kill
	// descendants that already escaped the group; cmd.WaitDelay is what
	// bounds cmd.Wait on a still-open inherited pipe held by an escaped
	// descendant.
	configureAdapterCancel(cmd)
	cmd.WaitDelay = DefaultWaitDelay
	stdout := &limitWriter{max: maxOut}
	stderr := &capWriter{max: 4 << 10}
	// Explicit bounded writers (never nil-inherit *os.Stdout/Stderr) so the
	// pipe copy goroutines terminate and nothing unbounded is retained.
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	runErr := cmd.Run()
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		return nil, fmt.Errorf("ingest: pdf adapter %q timed out after %s", l.Command[0], timeout)
	case runErr != nil:
		msg := strings.TrimSpace(stderr.buf.String())
		if msg != "" {
			return nil, fmt.Errorf("ingest: pdf adapter %q failed: %w: %s", l.Command[0], runErr, msg)
		}
		return nil, fmt.Errorf("ingest: pdf adapter %q failed: %w", l.Command[0], runErr)
	case stdout.exceeded:
		return nil, fmt.Errorf("ingest: pdf adapter %q output exceeds max_bytes (%d)", l.Command[0], maxOut)
	}

	var out adapterWire
	if err := json.Unmarshal(stdout.buf.Bytes(), &out); err != nil {
		return nil, fmt.Errorf("ingest: pdf adapter %q produced unparsable output: %w", l.Command[0], err)
	}
	if len(out.Sections) == 0 {
		return nil, fmt.Errorf("ingest: pdf adapter %q produced no sections", l.Command[0])
	}
	var sections []memory.DocumentSection
	for _, sec := range out.Sections {
		if strings.TrimSpace(sec.Text) == "" {
			continue // an empty page carries no memory content
		}
		sections = append(sections, memory.DocumentSection{
			Name: sec.Name, Page: sec.Page, Text: sec.Text})
	}
	if len(sections) == 0 {
		return nil, fmt.Errorf("ingest: pdf adapter %q produced only empty sections", l.Command[0])
	}
	return &Result{Sections: sections, Revision: out.Revision}, nil
}
