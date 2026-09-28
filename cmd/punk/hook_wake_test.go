package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// punk hook wake is dispatched from cmdHook, fails open on bad flags and
// unknown actions, prints nothing to stdout, and stays inert (no worker
// spawned, no requests) when the native wake capability is missing.
// Lifecycle semantics live in internal/hookcli/wake_test.go; these tests
// pin the CLI surface the generated hook entries invoke.

func hookWakeStdin(t *testing.T, payload string) {
	t.Helper()
	stdin, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = stdin.WriteString(payload)
	_, _ = stdin.Seek(0, 0)
	old := os.Stdin
	os.Stdin = stdin
	t.Cleanup(func() { os.Stdin = old })
}

func TestCmdHookWakeDispatchFailOpen(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("PUNK_MESSAGING", "")
	t.Setenv("PUNK_NAMESPACE", "")
	t.Setenv("PUNK_API_KEY", "")
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "")
	t.Setenv("CLAUDE_CODE_MESSAGING_TOKEN", "")

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	payload := `{"session_id":"s1","cwd":"/p","hook_event_name":"SessionStart"}`

	// ensure with no native endpoint: concise stderr, exit nil, no
	// stdout, and (namespace pinned by --ns) zero server requests.
	hookWakeStdin(t, payload)
	out, err := captureStdout(t, func() error {
		return cmdHook([]string{"wake", "--client", "claude-code", "--action", "ensure",
			"--url", srv.URL, "--ns", "ns1", "--messaging"})
	})
	if err != nil || out != "" || hits.Load() != 0 {
		t.Fatalf("unavailable ensure: err=%v out=%q requests=%d", err, out, hits.Load())
	}

	// stop tears down nothing and stays quiet on stdout.
	hookWakeStdin(t, payload)
	out, err = captureStdout(t, func() error {
		return cmdHook([]string{"wake", "--client", "claude-code", "--action", "stop",
			"--url", srv.URL, "--ns", "ns1", "--messaging"})
	})
	if err != nil || out != "" {
		t.Fatalf("stop: err=%v out=%q", err, out)
	}

	// run without a piped config fails open.
	hookWakeStdin(t, "")
	out, err = captureStdout(t, func() error {
		return cmdHook([]string{"wake", "--client", "codex", "--action", "run",
			"--url", srv.URL, "--ns", "ns1", "--messaging"})
	})
	if err != nil || out != "" {
		t.Fatalf("run without config: err=%v out=%q", err, out)
	}

	// Unknown action and unknown client fail open too.
	hookWakeStdin(t, payload)
	if out, err = captureStdout(t, func() error {
		return cmdHook([]string{"wake", "--client", "claude-code", "--action", "dance", "--url", srv.URL})
	}); err != nil || out != "" {
		t.Fatalf("unknown action: err=%v out=%q", err, out)
	}
	hookWakeStdin(t, payload)
	if out, err = captureStdout(t, func() error {
		return cmdHook([]string{"wake", "--client", "cursor", "--action", "ensure", "--url", srv.URL})
	}); err != nil || out != "" {
		t.Fatalf("unknown client: err=%v out=%q", err, out)
	}

	// Bad flags fail open exactly like cmdHookInbox.
	if out, err = captureStdout(t, func() error {
		return cmdHook([]string{"wake", "--no-such-flag"})
	}); err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("bad flag must fail open: err=%v out=%q", err, out)
	}
}
