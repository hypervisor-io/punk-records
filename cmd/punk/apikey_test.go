package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/hypervisor-io/punk-records/internal/store"
)

// captureStderr swaps os.Stderr for a pipe while fn runs and returns
// what was written - the create/revoke commands print their
// human-readable notes there, while the token itself goes to stdout for
// scripting (see captureStdout in authz_cli_test.go).
func captureStderr(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	done := make(chan string)
	go func() {
		out, _ := io.ReadAll(r)
		done <- string(out)
	}()
	runErr := fn()
	_ = w.Close()
	os.Stderr = old
	return <-done, runErr
}

// TestAPIKeySubjectDefaultsToName pins the fix for `punk apikey create`
// storing an empty subject when --subject is omitted: under
// authz.enforcement=deny that key could never be granted anything
// (validateSubject rejects empty). An explicit --subject always wins;
// omitting it falls back to the key name.
func TestAPIKeySubjectDefaultsToName(t *testing.T) {
	cases := []struct {
		name, subject, want string
	}{
		{"ci", "", "ci"},
		{"ci", "alice", "alice"},
		{"ci", "  ", "ci"},   // blank is as good as empty
		{"  ci  ", "", "ci"}, // padded name falls back trimmed
	}
	for _, c := range cases {
		if got := apiKeySubject(c.name, c.subject); got != c.want {
			t.Errorf("apiKeySubject(%q, %q) = %q, want %q", c.name, c.subject, got, c.want)
		}
	}
}

// TestCmdAPIKeyCreateDefaultsSubjectToName exercises the real CLI path
// (through run()): creating a key without --subject stores the key name
// as the subject and prints a note about the default.
func TestCmdAPIKeyCreateDefaultsSubjectToName(t *testing.T) {
	cfgPath, dbPath := authzCLISetup(t)

	var stdout string
	stderr, runErr := captureStderr(t, func() error {
		var innerErr error
		stdout, innerErr = captureStdout(t, func() error {
			return run([]string{"apikey", "create", "--config", cfgPath, "--name", "ci"})
		})
		return innerErr
	})
	if runErr != nil {
		t.Fatalf("apikey create: %v", runErr)
	}
	if !strings.Contains(stdout, "prk_") {
		t.Fatalf("stdout %q missing the token", stdout)
	}
	if !strings.Contains(stderr, "subject defaults to name") {
		t.Fatalf("stderr %q missing the default-subject note", stderr)
	}

	db, err := store.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var subject string
	if err := db.QueryRowContext(t.Context(), "SELECT subject FROM api_keys WHERE name = 'ci'").Scan(&subject); err != nil {
		t.Fatal(err)
	}
	if subject != "ci" {
		t.Fatalf("stored subject = %q, want the key name %q", subject, "ci")
	}

	// an explicit --subject is unaffected, and prints no default note
	stderr2, runErr2 := captureStderr(t, func() error {
		return run([]string{"apikey", "create", "--config", cfgPath, "--name", "ops", "--subject", "alice"})
	})
	if runErr2 != nil {
		t.Fatalf("apikey create with subject: %v", runErr2)
	}
	if strings.Contains(stderr2, "defaults to name") {
		t.Fatalf("stderr %q should not mention defaulting when --subject was given", stderr2)
	}
	if err := db.QueryRowContext(t.Context(), "SELECT subject FROM api_keys WHERE name = 'ops'").Scan(&subject); err != nil {
		t.Fatal(err)
	}
	if subject != "alice" {
		t.Fatalf("stored subject = %q, want the explicit %q", subject, "alice")
	}
}
