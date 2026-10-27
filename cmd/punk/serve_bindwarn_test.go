package main

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// openBindWarningHead renders the exact headline line warnOpenBind must
// log for a given address - the string the serve-path tests grep for in
// captured output. Duplicated from the implementation on purpose: the
// assertion pins the operator-visible line, not a shared constant.
func openBindWarningHead(addr string) string {
	return "OPEN HTTP BIND: " + addr + " has ZERO API keys"
}

// captureLogger builds a text-format slog logger over a buffer, the
// same shape obs.NewLogger produces for log.format=text, so tests can
// assert on exact rendered lines.
func captureLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

// TestShouldWarnBind pins the pure decision: a non-loopback bind with
// zero keys warns; every loopback form never warns (regardless of
// keys); a non-loopback bind with at least one key never warns.
func TestShouldWarnBind(t *testing.T) {
	cases := []struct {
		addr string
		keys int
		want bool
		note string
	}{
		{"0.0.0.0:9090", 0, true, "all-interfaces IPv4, no keys"},
		{":9090", 0, true, "empty host means all interfaces"},
		{"[::]:9090", 0, true, "all-interfaces IPv6 is non-loopback"},
		{"192.168.1.5:9090", 0, true, "LAN address"},
		{"192.0.2.1:9090", 0, true, "any other non-loopback IP"},
		{"myhost.example:9090", 0, true, "unknown hostname errs loud, no DNS at startup"},
		{"0.0.0.0:9090", 1, false, "a key exists"},
		{"192.168.1.5:9090", 3, false, "keys exist"},
		{"127.0.0.1:9090", 0, false, "IPv4 loopback"},
		{"127.0.0.1:9090", 2, false, "loopback never warns regardless of keys"},
		{"localhost:9090", 0, false, "localhost counts as loopback"},
		{"LOCALHOST:9090", 0, false, "loopback hostname is case-insensitive"},
		{"[::1]:9090", 0, false, "IPv6 loopback"},
		{"[::ffff:127.0.0.1]:9090", 0, false, "IPv4-mapped IPv6 loopback"},
		{"127.0.0.1", 0, false, "bare loopback IP without port"},
	}
	for _, c := range cases {
		if got := shouldWarnBind(c.addr, c.keys); got != c.want {
			t.Errorf("shouldWarnBind(%q, %d) = %v, want %v (%s)",
				c.addr, c.keys, got, c.want, c.note)
		}
	}
}

// TestWarnOpenBindLogCapture is the P3-F acceptance at the unit level:
// the exact warning line is captured for a non-loopback bind with zero
// keys, and nothing is logged for a loopback bind or for a non-loopback
// bind once a key exists.
func TestWarnOpenBindLogCapture(t *testing.T) {
	log, buf := captureLogger()

	warnOpenBind(log, "0.0.0.0:9090", 0)
	out := buf.String()
	if !strings.Contains(out, openBindWarningHead("0.0.0.0:9090")) {
		t.Fatalf("open-bind warning missing for 0.0.0.0:9090 with 0 keys; log:\n%s", out)
	}
	if n := strings.Count(out, "level=WARN"); n < 3 {
		t.Fatalf("open-bind warning should be a loud multi-line banner, got %d WARN line(s):\n%s", n, out)
	}

	buf.Reset()
	warnOpenBind(log, "127.0.0.1:9090", 0)
	if out = buf.String(); strings.Contains(out, "OPEN HTTP BIND") {
		t.Fatalf("loopback bind must not warn; log:\n%s", out)
	}

	buf.Reset()
	warnOpenBind(log, "0.0.0.0:9090", 1)
	if out = buf.String(); strings.Contains(out, "OPEN HTTP BIND") {
		t.Fatalf("non-loopback bind with a key must not warn; log:\n%s", out)
	}

	buf.Reset()
	warnOpenBind(log, "[::]:9090", 0)
	if out = buf.String(); !strings.Contains(out, openBindWarningHead("[::]:9090")) {
		t.Fatalf("all-interfaces IPv6 must warn; log:\n%s", out)
	}
}

// bootServeChild runs the REAL serve path - TestMain re-executes this
// test binary with PUNK_SERVE_HELPER_CONFIG, which calls run(serve) -
// against a freshly migrated temp SQLite DB, and returns the child plus
// its captured stderr. addr is the configured http.addr; preBoot, when
// set, runs between migrate and boot (e.g. `punk apikey create`).
func bootServeChild(t *testing.T, addr string, preBoot func(cfgPath string)) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "punk.db")
	cfgPath := filepath.Join(dir, "config.yaml")
	body := fmt.Sprintf(`http:
  addr: %q
db:
  driver: sqlite
  dsn: %s
specs:
  dir: %s
`, addr, dbPath, filepath.Join(dir, "specs"))
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// flags before the positional action: Go's flag parsing stops at
	// the first non-flag
	if err := run([]string{"migrate", "--config", cfgPath, "up"}); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	if preBoot != nil {
		preBoot(cfgPath)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PUNK_SERVE_HELPER_CONFIG="+cfgPath)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
	})
	return cmd, &errBuf
}

// TestServeWarnsOnNonLoopbackBindWithoutKeys drives the real cmdServe
// with http.addr on TEST-NET-1 (RFC 5737: guaranteed never assigned to
// a real interface), so the OS refuses the listen and the child exits
// without any test ever holding a non-loopback port open. The warning
// is emitted before the listener starts, so it must already be on
// stderr - and the failed listen proves the warning never refused
// startup on its own.
func TestServeWarnsOnNonLoopbackBindWithoutKeys(t *testing.T) {
	addr := fmt.Sprintf("192.0.2.1:%d", freePort(t))
	cmd, errBuf := bootServeChild(t, addr, nil)

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	select {
	case <-waitCh: // listen refused on the unroutable address: expected
	case <-time.After(20 * time.Second):
		// The address was somehow routable and the server is alive;
		// the warning fired before the listener either way.
		_ = cmd.Process.Signal(syscall.SIGTERM)
		<-waitCh
	}
	out := errBuf.String()
	if !strings.Contains(out, openBindWarningHead(addr)) {
		t.Fatalf("real serve path must log the open-bind warning for %s with 0 keys; stderr:\n%s", addr, out)
	}
}

// TestServeNoOpenBindWarningOnLoopback boots the real server on an
// ephemeral loopback port with zero keys and asserts the warning never
// appears: loopback is the single-user default and stays quiet.
func TestServeNoOpenBindWarningOnLoopback(t *testing.T) {
	port := freePort(t)
	cmd, errBuf := bootServeChild(t, fmt.Sprintf("127.0.0.1:%d", port), nil)

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	deadline := time.Now().Add(20 * time.Second)
	for {
		select {
		case err := <-waitCh:
			t.Fatalf("server exited early: %v; stderr: %s", err, errBuf.String())
		default:
		}
		resp, err := http.Get(baseURL + "/healthz")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never became ready; stderr: %s", errBuf.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	<-waitCh
	if out := errBuf.String(); strings.Contains(out, "OPEN HTTP BIND") {
		t.Fatalf("loopback bind must not warn in the real serve path; stderr:\n%s", out)
	}
}

// TestServeNoOpenBindWarningWithKey drives the real serve path on a
// non-loopback (unroutable TEST-NET-1) address WITH an API key already
// provisioned: the boundary is credentialed, so no warning.
func TestServeNoOpenBindWarningWithKey(t *testing.T) {
	addr := fmt.Sprintf("192.0.2.1:%d", freePort(t))
	cmd, errBuf := bootServeChild(t, addr, func(cfgPath string) {
		punkCLI(t, "apikey", "create", "--config", cfgPath, "--name", "ops")
	})

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	select {
	case <-waitCh:
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Signal(syscall.SIGTERM)
		<-waitCh
	}
	if out := errBuf.String(); strings.Contains(out, "OPEN HTTP BIND") {
		t.Fatalf("non-loopback bind with a key must not warn; stderr:\n%s", out)
	}
}
