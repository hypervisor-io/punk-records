package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

// punk hook inbox is dispatched from cmdHook, fails open on bad flags,
// and stays inert (zero requests, empty stdout) without the opt-in.
func TestCmdHookInboxDispatchAndOptIn(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()
	t.Setenv("PUNK_MESSAGING", "")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	stdin, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = stdin.WriteString(`{"session_id":"s1","cwd":"/p","hook_event_name":"Stop"}`)
	_, _ = stdin.Seek(0, 0)
	old := os.Stdin
	os.Stdin = stdin
	defer func() { os.Stdin = old }()

	out, err := captureStdout(t, func() error {
		return cmdHook([]string{"inbox", "--client", "codex", "--mode", "continue", "--url", srv.URL, "--ns", "ns1"})
	})
	if err != nil || out != "" || hits.Load() != 0 {
		t.Fatalf("disabled inbox: err=%v out=%q requests=%d", err, out, hits.Load())
	}
	out, err = captureStdout(t, func() error {
		return cmdHook([]string{"inbox", "--no-such-flag"})
	})
	if err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("bad flag must fail open: err=%v out=%q", err, out)
	}
}
