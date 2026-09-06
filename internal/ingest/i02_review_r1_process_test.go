package ingest

import (
	"context"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestReviewerI02AdapterDescendantCannotDefeatTimeout(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("needs sh")
	}
	l := PDFLoader{Command: []string{"sh", "-c", "sleep 2 & wait"}, Timeout: 30 * time.Millisecond}
	start := time.Now()
	_, err := l.Load(context.Background(), LoadInput{Body: []byte("%PDF-1.0")})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("adapter descendant held inherited pipes for %s despite 30ms timeout", elapsed)
	}
}
func TestReviewerI02StderrCapConsumesWholeStream(t *testing.T) {
	w := &capWriter{max: 4096}
	n, err := io.Copy(w, strings.NewReader(strings.Repeat("x", 6000)))
	if err != nil || n != 6000 {
		t.Fatalf("bounded stderr writer violates io.Writer contract: n=%d err=%v; should consume all6000, retain4096", n, err)
	}
	if w.buf.Len() != 4096 {
		t.Fatal(w.buf.Len())
	}
}
