package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain doubles as the serve helper: the integration tests below
// re-exec this test binary with PUNK_SERVE_HELPER_CONFIG set, so the
// REAL cmdServe path (config load, migration check, wiring, HTTP
// listener) runs in a child process against a temp config and a temp
// SQLite DB - the same code an operator runs, not a hand-wired api
// test server.
func TestMain(m *testing.M) {
	if cfg := os.Getenv("PUNK_SERVE_HELPER_CONFIG"); cfg != "" {
		if err := run([]string{"serve", "--config", cfg}); err != nil {
			fmt.Fprintln(os.Stderr, "punk:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// startRealServer writes a temp config (extraCfg carries e.g. the authz
// section), migrates the temp DB up through the real CLI path, then
// boots the real server in a child process and waits for /healthz.
func startRealServer(t *testing.T, extraCfg string) (baseURL, cfgPath string) {
	t.Helper()
	dir := t.TempDir()
	port := freePort(t)
	dbPath := filepath.Join(dir, "punk.db")
	cfgPath = filepath.Join(dir, "config.yaml")
	body := fmt.Sprintf(`http:
  addr: "127.0.0.1:%d"
db:
  driver: sqlite
  dsn: %s
specs:
  dir: %s
%s`, port, dbPath, filepath.Join(dir, "specs"), extraCfg)
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// flags before the positional action: Go's flag parsing stops at
	// the first non-flag
	if err := run([]string{"migrate", "--config", cfgPath, "up"}); err != nil {
		t.Fatalf("migrate up: %v", err)
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
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-waitCh:
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
			<-waitCh
		}
	})

	baseURL = fmt.Sprintf("http://127.0.0.1:%d", port)
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
				return baseURL, cfgPath
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never became ready (last err %v); stderr: %s", err, errBuf.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func httpStatus(t *testing.T, method, url, token, body string) int {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// punkCLI runs a provisioning subcommand in-process against the same
// config the child server uses, the way an operator with local DB
// access would.
func punkCLI(t *testing.T, args ...string) string {
	t.Helper()
	out, err := captureStdout(t, func() error { return run(args) })
	if err != nil {
		t.Fatalf("punk %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(out)
}

// Runtime proof for the A01 review finding: a server built by the real
// cmdServe from a config with authz.enforcement=deny actually denies -
// zero keys and no credentials no longer sails through with HTTP 200.
// It then proves the local recovery path: `punk apikey create` + `punk
// authz grant` provision access without any unauthenticated HTTP route.
func TestServeDenyModeEnforcedFromConfig(t *testing.T) {
	baseURL, cfgPath := startRealServer(t, "authz:\n  enforcement: deny\n")
	memB := baseURL + "/v1/namespaces/ns-b/memories"

	// the reviewer's case: deny mode, zero keys, no credentials
	if got := httpStatus(t, http.MethodGet, memB, "", ""); got != http.StatusForbidden {
		t.Fatalf("deny-mode bootstrap GET = %d, want 403 (was 200 before the wiring fix)", got)
	}

	// local recovery: key + grant are provisioned locally, never over
	// unauthenticated HTTP
	token := punkCLI(t, "apikey", "create", "--config", cfgPath, "--name", "ops", "--subject", "ops-admin")
	if !strings.HasPrefix(token, "prk_") {
		t.Fatalf("token = %q", token)
	}
	// a verified key with no grants is still denied
	if got := httpStatus(t, http.MethodGet, memB, token, ""); got != http.StatusForbidden {
		t.Fatalf("verified key without grant = %d, want 403", got)
	}

	punkCLI(t, "authz", "grant", "--config", cfgPath, "--subject", "ops-admin", "--namespace", "ns-b", "--op", "read")
	if got := httpStatus(t, http.MethodGet, memB, token, ""); got != http.StatusOK {
		t.Fatalf("granted read = %d, want 200", got)
	}
	// read-only: no write, no other namespace
	if got := httpStatus(t, http.MethodPost, memB, token, `{"key":"/a01","body":"v"}`); got != http.StatusForbidden {
		t.Fatalf("read grant POST = %d, want 403", got)
	}
	if got := httpStatus(t, http.MethodGet, baseURL+"/v1/namespaces/ns-c/memories", token, ""); got != http.StatusForbidden {
		t.Fatalf("cross-namespace read = %d, want 403", got)
	}

	punkCLI(t, "authz", "grant", "--config", cfgPath, "--subject", "ops-admin", "--namespace", "ns-b", "--op", "write")
	if got := httpStatus(t, http.MethodPost, memB, token, `{"key":"/a01","body":"v"}`); got != http.StatusCreated {
		t.Fatalf("write grant POST = %d, want 201", got)
	}

	// a key with no subject claim holds no grants even when some other
	// subject is granted
	noSubjectToken := punkCLI(t, "apikey", "create", "--config", cfgPath, "--name", "anon")
	if got := httpStatus(t, http.MethodGet, memB, noSubjectToken, ""); got != http.StatusForbidden {
		t.Fatalf("empty-subject key = %d, want 403", got)
	}

	punkCLI(t, "authz", "revoke", "--config", cfgPath, "--subject", "ops-admin", "--namespace", "ns-b", "--op", "read")
	if got := httpStatus(t, http.MethodGet, memB, token, ""); got != http.StatusForbidden {
		t.Fatalf("post-revoke read = %d, want 403", got)
	}
}

// Default-off compatibility at the real-server level: with no authz
// section (enforcement off), cmdServe wires no authorizer and the
// trusted single-user behavior is unchanged - the zero-key bootstrap
// still passes.
func TestServeOffModeStaysCompatible(t *testing.T) {
	baseURL, _ := startRealServer(t, "")
	if got := httpStatus(t, http.MethodGet, baseURL+"/v1/namespaces/ns-b/memories", "", ""); got != http.StatusOK {
		t.Fatalf("off-mode bootstrap GET = %d, want 200", got)
	}
}
