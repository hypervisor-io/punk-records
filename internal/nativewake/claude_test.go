package nativewake

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClaudeServer is a real unix-socket stand-in for one Claude Code
// session's own messaging inbox: it accepts connections and records every
// newline-delimited frame it receives. The wire is real; only the peer is
// fake. It mirrors the real inbox's close-on-EOF behavior and bounded wait
// for a first line.
type fakeClaudeServer struct {
	listener net.Listener

	mu    sync.Mutex
	lines []string
	conns int
}

func newFakeClaudeServer(t *testing.T) *fakeClaudeServer {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude-inbox.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeClaudeServer{listener: l}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.conns++
			f.mu.Unlock()
			go f.serve(conn)
		}
	}()
	return f
}

func (f *fakeClaudeServer) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	// The real inbox closes a connection that sends no complete line
	// within 30s; a test-friendly deadline keeps a stuck client fast.
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	rd := bufio.NewReader(conn)
	for {
		line, err := rd.ReadString('\n')
		if line != "" {
			f.mu.Lock()
			f.lines = append(f.lines, strings.TrimRight(line, "\n"))
			f.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (f *fakeClaudeServer) addr() string { return f.listener.Addr().String() }

func (f *fakeClaudeServer) received() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.lines...)
}

func (f *fakeClaudeServer) connCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conns
}

// waitLines polls until n lines arrived or the deadline passes, so the
// assertions do not race the server reader goroutine.
func (f *fakeClaudeServer) waitLines(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got := f.received()
		if len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("server received %d lines, want >= %d: %q", len(f.received()), n, f.received())
	return nil
}

// assertNoLeak fails when err names the socket path or token: Go's
// net.OpError embeds unix paths, so every transport error must be static.
func assertNoLeak(t *testing.T, err error, path, token string) {
	t.Helper()
	if err == nil {
		return
	}
	if path != "" && strings.Contains(err.Error(), path) {
		t.Fatalf("error leaks socket path: %v", err)
	}
	if token != "" && strings.Contains(err.Error(), token) {
		t.Fatalf("error leaks token: %v", err)
	}
}

func TestClaudeWakePostsVerifiedFrames(t *testing.T) {
	srv := newFakeClaudeServer(t)
	const token = "session-child-token-value"
	tr := NewClaudeTransport(srv.addr(), token)
	defer func() { _ = tr.Close() }()

	out, err := tr.Wake(context.Background(), "punk wake notification: read your punk inbox")
	if err != nil {
		t.Fatalf("wake: %v", err)
	}
	if out.Accepted || !out.Unconfirmed {
		t.Fatalf("outcome = %+v, want Unconfirmed-only host handoff", out)
	}

	got := srv.waitLines(t, 2)
	if len(got) != 2 {
		t.Fatalf("server received %d lines, want 2 (auth, user): %q", len(got), got)
	}
	var auth map[string]any
	if err := json.Unmarshal([]byte(got[0]), &auth); err != nil {
		t.Fatalf("auth line not JSON: %v", err)
	}
	if auth["type"] != "auth" || auth["token"] != token {
		t.Fatalf("auth frame mismatch: %v", auth)
	}
	var user map[string]any
	if err := json.Unmarshal([]byte(got[1]), &user); err != nil {
		t.Fatalf("user line not JSON: %v", err)
	}
	msg, _ := user["message"].(map[string]any)
	if user["type"] != "user" || msg == nil || msg["role"] != "user" ||
		msg["content"] != "punk wake notification: read your punk inbox" {
		t.Fatalf("user frame mismatch: %v", user)
	}
}

func TestClaudeWakeEscapesNudge(t *testing.T) {
	srv := newFakeClaudeServer(t)
	tr := NewClaudeTransport(srv.addr(), "tok")
	defer func() { _ = tr.Close() }()

	// Quotes, a literal newline, backslashes, a tab, a NUL and unicode:
	// JSON encoding must keep this ONE line and one frame.
	nudge := "quote\" newline\n backslash\\ tab\t nul\x00 unicode \u00e9\u4e2d"
	if _, err := tr.Wake(context.Background(), nudge); err != nil {
		t.Fatalf("wake: %v", err)
	}
	got := srv.waitLines(t, 2)
	if len(got) != 2 {
		t.Fatalf("escaping broke framing, %d lines: %q", len(got), got)
	}
	var user map[string]any
	if err := json.Unmarshal([]byte(got[1]), &user); err != nil {
		t.Fatalf("user line not JSON: %v", err)
	}
	msg, _ := user["message"].(map[string]any)
	if msg["content"] != nudge {
		t.Fatalf("nudge mangled: got %q want %q", msg["content"], nudge)
	}
}

func TestClaudeWakeWithoutTokenOmitsAuth(t *testing.T) {
	srv := newFakeClaudeServer(t)
	tr := NewClaudeTransport(srv.addr(), "")
	defer func() { _ = tr.Close() }()

	if _, err := tr.Wake(context.Background(), "nudge"); err != nil {
		t.Fatalf("wake: %v", err)
	}
	got := srv.waitLines(t, 1)
	if len(got) != 1 {
		t.Fatalf("server received %d lines, want 1 (user only): %q", len(got), got)
	}
	var frame map[string]any
	if err := json.Unmarshal([]byte(got[0]), &frame); err != nil {
		t.Fatalf("line not JSON: %v", err)
	}
	if frame["type"] != "user" {
		t.Fatalf("unexpected frame without token: %v", frame)
	}
}

func TestClaudeProbeLiveSocketPostsNothing(t *testing.T) {
	srv := newFakeClaudeServer(t)
	tr := NewClaudeTransport(srv.addr(), "tok")
	defer func() { _ = tr.Close() }()

	if err := tr.Probe(context.Background()); err != nil {
		t.Fatalf("probe live socket: %v", err)
	}
	// Probe must not post: allow a beat for any stray bytes, then check.
	time.Sleep(50 * time.Millisecond)
	if got := srv.received(); len(got) != 0 {
		t.Fatalf("probe posted %d lines: %q", len(got), got)
	}
}

func TestClaudeProbeMissingSocketUnavailable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gone.sock")
	tr := NewClaudeTransport(path, "sekrit-token")
	defer func() { _ = tr.Close() }()

	err := tr.Probe(context.Background())
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("probe missing socket = %v, want ErrUnavailable", err)
	}
	assertNoLeak(t, err, path, "sekrit-token")
}

func TestClaudeProbeRejectsNonSocketPaths(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "regular.sock")
	if err := os.WriteFile(regular, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A symlink pointing at a REAL socket must still be refused: the token
	// goes to whatever the path resolves to, so links are never followed.
	srv := newFakeClaudeServer(t)
	link := filepath.Join(dir, "link.sock")
	if err := os.Symlink(srv.addr(), link); err != nil {
		t.Fatal(err)
	}

	for name, path := range map[string]string{
		"regular file": regular,
		"symlink":      link,
		"relative":     "relative.sock",
		"empty":        "",
	} {
		tr := NewClaudeTransport(path, "sekrit-token")
		err := tr.Probe(context.Background())
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("%s: probe = %v, want ErrUnavailable", name, err)
		}
		assertNoLeak(t, err, path, "sekrit-token")
		if err := tr.Close(); err != nil {
			t.Fatal(err)
		}
	}
	// The real socket behind the symlink must never have been touched.
	if n := srv.connCount(); n != 0 {
		t.Fatalf("symlink probe reached real socket (%d conns)", n)
	}
}

func TestClaudeWakeRefusedListenerUnavailable(t *testing.T) {
	// Stale socket file whose listener is gone: definite no target.
	l, err := net.Listen("unix", filepath.Join(t.TempDir(), "stale.sock"))
	if err != nil {
		t.Fatal(err)
	}
	path := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	tr := NewClaudeTransport(path, "sekrit-token")
	defer func() { _ = tr.Close() }()
	out, err := tr.Wake(context.Background(), "nudge")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("wake stale socket = %v (out %+v), want ErrUnavailable", err, out)
	}
	assertNoLeak(t, err, path, "sekrit-token")
	if out != (Outcome{}) {
		t.Fatalf("failed wake returned outcome %+v", out)
	}
}

func TestClaudeWakeMissingSocketUnavailable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.sock")
	tr := NewClaudeTransport(path, "sekrit-token")
	defer func() { _ = tr.Close() }()

	_, err := tr.Wake(context.Background(), "nudge")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("wake missing socket = %v, want ErrUnavailable", err)
	}
	assertNoLeak(t, err, path, "sekrit-token")
}

func TestClaudeWakeValidatesTextBeforeDialing(t *testing.T) {
	srv := newFakeClaudeServer(t)
	tr := NewClaudeTransport(srv.addr(), "tok")
	defer func() { _ = tr.Close() }()

	if _, err := tr.Wake(context.Background(), ""); err == nil {
		t.Fatal("empty nudge accepted")
	}
	if _, err := tr.Wake(context.Background(), strings.Repeat("x", claudeMaxNudgeBytes+1)); err == nil {
		t.Fatal("oversize nudge accepted")
	}
	if n := srv.connCount(); n != 0 {
		t.Fatalf("validation failure still dialed (%d conns)", n)
	}
}

func TestClaudeWakeCanceledContext(t *testing.T) {
	srv := newFakeClaudeServer(t)
	tr := NewClaudeTransport(srv.addr(), "tok")
	defer func() { _ = tr.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := tr.Wake(ctx, "nudge")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled wake = %v, want context.Canceled", err)
	}
	if n := srv.connCount(); n != 0 {
		t.Fatalf("canceled wake dialed (%d conns)", n)
	}
}

func TestClaudeClose(t *testing.T) {
	srv := newFakeClaudeServer(t)
	path := srv.addr()
	tr := NewClaudeTransport(path, "sekrit-token")

	if err := tr.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if err := tr.Probe(context.Background()); !errors.Is(err, errClaudeClosed) {
		t.Fatalf("probe after close = %v, want closed error", err)
	}
	_, err := tr.Wake(context.Background(), "nudge")
	if !errors.Is(err, errClaudeClosed) {
		t.Fatalf("wake after close = %v, want closed error", err)
	}
	assertNoLeak(t, err, path, "sekrit-token")
	if n := srv.connCount(); n != 0 {
		t.Fatalf("closed transport dialed (%d conns)", n)
	}
}

// TestClaudePostPeerClosed exercises the ambiguous failed-write path on a
// real connection whose peer is already gone: generic error, never
// ErrUnavailable (the nudge may have left a prefix behind).
func TestClaudePostPeerClosed(t *testing.T) {
	l, err := net.Listen("unix", filepath.Join(t.TempDir(), "drop.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	accepted := make(chan struct{})
	go func() {
		conn, err := l.Accept()
		if err == nil {
			_ = conn.Close() // peer vanishes before any read
		}
		close(accepted)
	}()

	conn, err := net.Dial("unix", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	<-accepted // the close is processed before we write: write fails

	payload, err := claudeFrames("tok", "nudge")
	if err != nil {
		t.Fatal(err)
	}
	err = claudePost(context.Background(), conn, payload)
	if err == nil {
		t.Fatal("write to closed peer succeeded")
	}
	if errors.Is(err, ErrUnavailable) {
		t.Fatalf("ambiguous write mapped to ErrUnavailable: %v", err)
	}
	assertNoLeak(t, err, l.Addr().String(), "tok")
}

// shortWriteConn simulates a peer that accepts only a prefix without an
// error and then stalls at zero progress: the transport must treat short
// writes as failure, not handoff.
type shortWriteConn struct {
	net.Conn
	n      int
	called bool
}

func (c *shortWriteConn) Write(p []byte) (int, error) {
	if !c.called {
		c.called = true
		if len(p) > c.n {
			return c.n, nil
		}
		return len(p), nil
	}
	return 0, nil // peer wedged: zero progress, no error
}
func (c *shortWriteConn) SetWriteDeadline(time.Time) error { return nil }

func TestClaudePostShortWrite(t *testing.T) {
	payload, err := claudeFrames("tok", "nudge")
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) < 2 {
		t.Fatalf("payload too small for the test: %d", len(payload))
	}
	err = claudePost(context.Background(), &shortWriteConn{n: len(payload) - 1}, payload)
	if err == nil {
		t.Fatal("short write reported as success")
	}
	if errors.Is(err, ErrUnavailable) {
		t.Fatalf("short write mapped to ErrUnavailable: %v", err)
	}
}

// TestClaudePostWriteTimeout proves a wedged peer cannot hang the runner:
// net.Pipe blocks until the peer reads; nobody reads; the deadline fires.
func TestClaudePostWriteTimeout(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := claudePost(ctx, client, []byte("payload\n"))
	if err == nil {
		t.Fatal("wedged write reported as success")
	}
	if errors.Is(err, ErrUnavailable) {
		t.Fatalf("wedged write mapped to ErrUnavailable: %v", err)
	}
	// Deadline-bound: well under claudeWriteTimeout, and the error is
	// either the context expiry or the generic bounded-write failure.
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("wedged write took %v, want deadline-bounded", elapsed)
	}
}

// TestClaudeTransportInterface guards the compile-time contract the runner
// and hookcli rely on.
func TestClaudeTransportInterface(t *testing.T) {
	tr := NewClaudeTransport("/nonexistent", "tok")
	if _, ok := tr.(*ClaudeTransport); !ok {
		t.Fatalf("NewClaudeTransport returned %T, want *ClaudeTransport", tr)
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
}
