package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hypervisor-io/punk-records/internal/authz"
	"github.com/hypervisor-io/punk-records/internal/store"
)

// captureStdout swaps os.Stdout for a pipe while fn runs (the CLI
// commands print results there) and returns what was written.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string)
	go func() {
		out, _ := io.ReadAll(r)
		done <- string(out)
	}()
	runErr := fn()
	_ = w.Close()
	os.Stdout = old
	return <-done, runErr
}

// authzCLISetup writes a temp config + SQLite DB and migrates it up via
// the real CLI path (flags before the positional action: Go's flag
// parsing stops at the first non-flag).
func authzCLISetup(t *testing.T) (cfgPath string, dbPath string) {
	t.Helper()
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "authz-cli.db")
	cfgPath = filepath.Join(dir, "config.yaml")
	body := "db:\n  driver: sqlite\n  dsn: " + dbPath + "\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"migrate", "--config", cfgPath, "up"}); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	return cfgPath, dbPath
}

func authzChecker(t *testing.T, dbPath string) (*authz.Authorizer, *store.DB) {
	t.Helper()
	db, err := store.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return authz.New(db, nil), db
}

// The CLI provisions independent read/write/admin grants, lists them,
// and revokes them; both flag orderings work (action-first and
// flags-before-action).
func TestAuthzCLIGrantListRevoke(t *testing.T) {
	cfgPath, dbPath := authzCLISetup(t)
	az, _ := authzChecker(t, dbPath)
	ctx := context.Background()

	// flags before the action
	if _, err := captureStdout(t, func() error {
		return run([]string{"authz", "--config", cfgPath, "--subject", "alice", "--namespace", "ns-a", "--op", "read", "grant"})
	}); err != nil {
		t.Fatalf("grant read: %v", err)
	}
	if !az.Allow(ctx, "alice", "ns-a", authz.OpRead) {
		t.Fatal("read grant not in effect")
	}
	if az.Allow(ctx, "alice", "ns-a", authz.OpWrite) || az.Allow(ctx, "alice", "ns-a", authz.OpAdmin) {
		t.Fatal("read grant must not imply write or admin")
	}

	// action first, flags after
	if _, err := captureStdout(t, func() error {
		return run([]string{"authz", "grant", "--config", cfgPath, "--subject", "alice", "--namespace", "ns-a", "--op", "write"})
	}); err != nil {
		t.Fatalf("grant write: %v", err)
	}
	if !az.Allow(ctx, "alice", "ns-a", authz.OpWrite) {
		t.Fatal("write grant not in effect")
	}
	if az.Allow(ctx, "alice", "ns-a", authz.OpAdmin) {
		t.Fatal("write grant must not imply admin")
	}
	if _, err := captureStdout(t, func() error {
		return run([]string{"authz", "grant", "--config", cfgPath, "--subject", "alice", "--namespace", "ns-a", "--op", "admin"})
	}); err != nil {
		t.Fatalf("grant admin: %v", err)
	}
	if !az.Allow(ctx, "alice", "ns-a", authz.OpAdmin) {
		t.Fatal("admin grant not in effect")
	}

	out, err := captureStdout(t, func() error {
		return run([]string{"authz", "list", "--config", cfgPath, "--subject", "alice"})
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, want := range []string{"ns-a read", "ns-a write", "ns-a admin"} {
		if !strings.Contains(out, want) {
			t.Fatalf("list output %q missing %q", out, want)
		}
	}

	if _, err := captureStdout(t, func() error {
		return run([]string{"authz", "revoke", "--config", cfgPath, "--subject", "alice", "--namespace", "ns-a", "--op", "write"})
	}); err != nil {
		t.Fatalf("revoke write: %v", err)
	}
	if az.Allow(ctx, "alice", "ns-a", authz.OpWrite) {
		t.Fatal("revoked write grant still allows")
	}
	if !az.Allow(ctx, "alice", "ns-a", authz.OpRead) {
		t.Fatal("revoking write must not touch the read grant")
	}

	// revoking what does not exist is an error, not silent success
	if _, err := captureStdout(t, func() error {
		return run([]string{"authz", "revoke", "--config", cfgPath, "--subject", "alice", "--namespace", "ns-a", "--op", "write"})
	}); err == nil {
		t.Fatal("double revoke should error")
	}
}

func TestAuthzCLIValidation(t *testing.T) {
	cfgPath, _ := authzCLISetup(t)

	cases := [][]string{
		// wildcards rejected (grants are exact-match, never globs)
		{"authz", "grant", "--config", cfgPath, "--subject", "*", "--namespace", "ns-a", "--op", "read"},
		{"authz", "grant", "--config", cfgPath, "--subject", "alice", "--namespace", "ns-*", "--op", "read"},
		{"authz", "grant", "--config", cfgPath, "--subject", "alice", "--namespace", "*", "--op", "read"},
		// empty subject denied by the model
		{"authz", "grant", "--config", cfgPath, "--subject", "  ", "--namespace", "ns-a", "--op", "read"},
		// unknown op
		{"authz", "grant", "--config", cfgPath, "--subject", "alice", "--namespace", "ns-a", "--op", "everything"},
		// missing required flags
		{"authz", "grant", "--config", cfgPath, "--subject", "alice", "--namespace", "ns-a"},
		{"authz", "grant", "--config", cfgPath, "--subject", "alice", "--op", "read"},
		{"authz", "revoke", "--config", cfgPath, "--subject", "alice"},
		{"authz", "list", "--config", cfgPath},
		// unknown / missing action
		{"authz", "frobnicate", "--config", cfgPath, "--subject", "alice"},
		{"authz", "--config", cfgPath, "--subject", "alice"},
	}
	for _, args := range cases {
		if _, err := captureStdout(t, func() error { return run(args) }); err == nil {
			t.Fatalf("run(%v) should error", args[1:])
		}
	}
}

// Default-off compatibility: with no authz section in the config
// (enforcement off, the default), the CLI still provisions grants -
// provisioning is independent of enforcement, which is exactly what the
// local recovery path relies on.
func TestAuthzCLIDefaultOffCompatible(t *testing.T) {
	cfgPath, dbPath := authzCLISetup(t) // config has no authz: section
	az, _ := authzChecker(t, dbPath)
	ctx := context.Background()

	if _, err := captureStdout(t, func() error {
		return run([]string{"authz", "grant", "--config", cfgPath, "--subject", "ops-admin", "--namespace", "ns-x", "--op", "admin"})
	}); err != nil {
		t.Fatalf("grant with default config: %v", err)
	}
	if !az.Allow(ctx, "ops-admin", "ns-x", authz.OpAdmin) {
		t.Fatal("grant not in effect under default-off config")
	}
	out, err := captureStdout(t, func() error {
		return run([]string{"authz", "list", "--config", cfgPath, "--subject", "ops-admin"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "ns-x admin") {
		t.Fatalf("list output %q missing the admin grant", out)
	}
	out, err = captureStdout(t, func() error {
		return run([]string{"authz", "list", "--config", cfgPath, "--subject", "nobody"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no active grants") {
		t.Fatalf("list for unknown subject = %q", out)
	}
}
