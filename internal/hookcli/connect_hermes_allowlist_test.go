package hookcli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Allowlist tests for ConnectHermesAllowlist: Hermes matches approvals by
// EXACT (event, command) string equality (hermes-agent agent/shell_hooks.py),
// so every assertion below pins the entry strings byte-for-byte against the
// same builders connect_hermes.go writes into config.yaml.

// testApproval is the decode shape the assertions use: typed for the four
// schema fields, plus a raw map view for foreign-entry preservation checks.
type testApproval struct {
	Event                 string  `json:"event"`
	Command               string  `json:"command"`
	ApprovedAt            string  `json:"approved_at"`
	ScriptMtimeAtApproval *string `json:"script_mtime_at_approval"`
}

func readAllowlist(t *testing.T, path string) ([]testApproval, string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Approvals []testApproval `json:"approvals"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("written allowlist is not valid JSON: %v\n%s", err, raw)
	}
	return doc.Approvals, string(raw)
}

// TestConnectHermesAllowlistCreatesApprovals covers the fresh-install path:
// no file at all, so punk creates it with exactly one approval per wired
// event, each command byte-identical to the config.yaml entry it approves.
func TestConnectHermesAllowlistCreatesApprovals(t *testing.T) {
	punkPath := filepath.Join(t.TempDir(), "no-such-punk") // stat must fail -> null mtime
	path := filepath.Join(t.TempDir(), "nested", hermesAllowlistFile)
	changed, err := ConnectHermesAllowlist(path, punkPath, "http://localhost:9090", false)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("creating an allowlist must report changed")
	}

	approvals, raw := readAllowlist(t, path)
	want := hermesDesiredApprovals(punkPath, "http://localhost:9090", false)
	if len(approvals) != len(want) {
		t.Fatalf("%d approvals, want %d: %s", len(approvals), len(want), raw)
	}
	for i, a := range approvals {
		if a.Event != want[i].Event || a.Command != want[i].Command {
			t.Fatalf("approval %d = (%q, %q), want (%q, %q)", i, a.Event, a.Command, want[i].Event, want[i].Command)
		}
		if _, err := time.Parse(time.RFC3339, a.ApprovedAt); err != nil {
			t.Fatalf("approved_at %q is not ISO-8601: %v", a.ApprovedAt, err)
		}
		if !strings.HasSuffix(a.ApprovedAt, "Z") {
			t.Fatalf("approved_at %q is not UTC Zulu", a.ApprovedAt)
		}
		if a.ScriptMtimeAtApproval != nil {
			t.Fatalf("script_mtime_at_approval = %q, want null for a nonexistent binary", *a.ScriptMtimeAtApproval)
		}
	}
	// The capture command must be exactly what connect writes into config.yaml.
	wantCmd := punkHermesHookCommand(punkPath, "http://localhost:9090")
	for _, a := range approvals {
		if a.Command != wantCmd {
			t.Fatalf("command = %q, want %q", a.Command, wantCmd)
		}
	}
}

// TestConnectHermesAllowlistRecordsBinaryMtime pins the non-null branch:
// when the punk binary the command invokes exists, its mtime is recorded
// as script_mtime_at_approval.
func TestConnectHermesAllowlistRecordsBinaryMtime(t *testing.T) {
	punkPath := filepath.Join(t.TempDir(), "punk")
	if err := os.WriteFile(punkPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(punkPath, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), hermesAllowlistFile)
	if _, err := ConnectHermesAllowlist(path, punkPath, "http://localhost:9090", false); err != nil {
		t.Fatal(err)
	}
	approvals, _ := readAllowlist(t, path)
	if len(approvals) == 0 {
		t.Fatal("no approvals written")
	}
	for _, a := range approvals {
		if a.ScriptMtimeAtApproval == nil {
			t.Fatal("script_mtime_at_approval = null, want the binary's mtime")
		}
		if *a.ScriptMtimeAtApproval != stamp.Format(time.RFC3339) {
			t.Fatalf("script_mtime_at_approval = %q, want %q", *a.ScriptMtimeAtApproval, stamp.Format(time.RFC3339))
		}
	}
}

// TestConnectHermesAllowlistMessagingAddsInboxApproval pins the
// --messaging shape: the pre_llm_call inbox entry gets its own approval,
// with the exact command punkInboxHookCommand builds for config.yaml.
func TestConnectHermesAllowlistMessagingAddsInboxApproval(t *testing.T) {
	punkPath := filepath.Join(t.TempDir(), "no-such-punk")
	path := filepath.Join(t.TempDir(), hermesAllowlistFile)
	if _, err := ConnectHermesAllowlist(path, punkPath, "http://localhost:9090", true); err != nil {
		t.Fatal(err)
	}
	approvals, raw := readAllowlist(t, path)
	if len(approvals) != len(hermesHookEvents)+1 {
		t.Fatalf("%d approvals, want %d: %s", len(approvals), len(hermesHookEvents)+1, raw)
	}
	last := approvals[len(approvals)-1]
	wantCmd := punkInboxHookCommand(punkPath, "hermes", "context", "", "http://localhost:9090", "")
	if last.Event != "pre_llm_call" || last.Command != wantCmd {
		t.Fatalf("inbox approval = (%q, %q), want (%q, %q)", last.Event, last.Command, "pre_llm_call", wantCmd)
	}
}

// TestConnectHermesAllowlistPreservesForeignEntries is the guarantee that
// matters for a live file Hermes itself maintains: entries punk did not
// write - including a differently-shaped element - survive as-is and in
// their original order, with punk's approvals appended after them.
func TestConnectHermesAllowlistPreservesForeignEntries(t *testing.T) {
	punkPath := filepath.Join(t.TempDir(), "no-such-punk")
	path := filepath.Join(t.TempDir(), hermesAllowlistFile)
	body := `{
  "approvals": [
    {"event": "pre_tool_call", "command": "~/.hermes/agent-hooks/block-rm-rf.sh", "approved_at": "2026-01-01T00:00:00Z", "script_mtime_at_approval": null},
    "bogus-element",
    {"event": "on_session_start", "command": "/opt/other/tool hook", "approved_at": "2026-01-02T00:00:00Z", "script_mtime_at_approval": "2026-01-02T00:00:00Z", "custom": "keep-me"}
  ]
}
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectHermesAllowlist(path, punkPath, "http://localhost:9090", false); err != nil {
		t.Fatal(err)
	}

	var doc struct {
		Approvals []json.RawMessage `json:"approvals"`
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("written allowlist is not valid JSON: %v\n%s", err, raw)
	}
	if len(doc.Approvals) != 3+len(hermesHookEvents) {
		t.Fatalf("%d approvals, want %d (3 foreign + punk's): %s", len(doc.Approvals), 3+len(hermesHookEvents), raw)
	}
	// The foreign entries kept their position, content and extra fields.
	// The encoder re-indents RawMessage bytes (same documented whitespace
	// normalization as ConnectHermes' YAML round-trip), so the assertions
	// compare compacted JSON - content, not layout.
	compact := func(raw json.RawMessage) string {
		t.Helper()
		var buf bytes.Buffer
		if err := json.Compact(&buf, raw); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	if !strings.Contains(compact(doc.Approvals[0]), "block-rm-rf.sh") {
		t.Fatalf("foreign entry 0 was reordered or replaced: %s", doc.Approvals[0])
	}
	if compact(doc.Approvals[1]) != `"bogus-element"` {
		t.Fatalf("differently-shaped element was dropped or altered: %s", doc.Approvals[1])
	}
	if !strings.Contains(compact(doc.Approvals[2]), `"custom":"keep-me"`) {
		t.Fatalf("foreign entry 2 lost its unknown field: %s", doc.Approvals[2])
	}
	// punk's approvals follow, one per wired event.
	for i, ev := range hermesHookEvents {
		var a testApproval
		if err := json.Unmarshal(doc.Approvals[3+i], &a); err != nil {
			t.Fatal(err)
		}
		if a.Event != ev || a.Command != punkHermesHookCommand(punkPath, "http://localhost:9090") {
			t.Fatalf("punk approval %d = (%q, %q)", i, a.Event, a.Command)
		}
	}
}

// TestConnectHermesAllowlistIdempotent pins the no-op contract: a rerun
// keeps punk's own entries (including their original timestamps) instead
// of rewriting them, reports changed=false without touching the file, and
// a changed URL replaces the stale approvals rather than accumulating.
func TestConnectHermesAllowlistIdempotent(t *testing.T) {
	punkPath := filepath.Join(t.TempDir(), "no-such-punk")
	path := filepath.Join(t.TempDir(), hermesAllowlistFile)
	if _, err := ConnectHermesAllowlist(path, punkPath, "http://localhost:9090", false); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	changed, err := ConnectHermesAllowlist(path, punkPath, "http://localhost:9090", false)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("an unchanged reconnect must report changed=false")
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("an unchanged reconnect rewrote the file:\n%s\n---\n%s", first, second)
	}

	// A changed URL replaces the stale approvals in place; never accumulates.
	if _, err := ConnectHermesAllowlist(path, punkPath, "http://memory.internal:9090", false); err != nil {
		t.Fatal(err)
	}
	approvals, raw := readAllowlist(t, path)
	if len(approvals) != len(hermesHookEvents) {
		t.Fatalf("%d approvals after re-connect, want %d: %s", len(approvals), len(hermesHookEvents), raw)
	}
	seen := map[string]bool{}
	for _, a := range approvals {
		if strings.Contains(a.Command, "localhost:9090") {
			t.Fatalf("stale approval survived: %q", a.Command)
		}
		if !strings.Contains(a.Command, "memory.internal") {
			t.Fatalf("approval was not refreshed: %q", a.Command)
		}
		seen[a.Event] = true
	}
	if len(seen) != len(hermesHookEvents) {
		t.Fatalf("events covered: %v", seen)
	}
}

// TestConnectHermesAllowlistReplacesStaleBinaryEntries covers the
// binary-relocation case: approvals written under the OLD punk path are
// still recognized as punk's (isPunkManagedHermes' fallback rule) and
// replaced, never duplicated.
func TestConnectHermesAllowlistReplacesStaleBinaryEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, hermesAllowlistFile)
	old := filepath.Join(dir, "old", "punk")
	fresh := filepath.Join(dir, "new", "punk")
	if _, err := ConnectHermesAllowlist(path, old, "http://localhost:9090", false); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectHermesAllowlist(path, fresh, "http://localhost:9090", false); err != nil {
		t.Fatal(err)
	}
	approvals, raw := readAllowlist(t, path)
	if len(approvals) != len(hermesHookEvents) {
		t.Fatalf("%d approvals, want the relocated ones to replace the stale ones: %s", len(approvals), raw)
	}
	for _, a := range approvals {
		if a.Command != punkHermesHookCommand(fresh, "http://localhost:9090") {
			t.Fatalf("command = %q", a.Command)
		}
	}
}

// TestConnectHermesAllowlistRefusesWrongShapes pins that punk names the
// file and leaves it byte-for-byte untouched rather than replacing a
// document it does not understand.
func TestConnectHermesAllowlistRefusesWrongShapes(t *testing.T) {
	cases := map[string]struct {
		body    string
		wantErr string
	}{
		"root is a list":        {"[]", "parse allowlist"},
		"root is a string":      {`"approvals"`, "parse allowlist"},
		"approvals is a map":    {`{"approvals":{"event":"x"}}`, "parse allowlist"},
		"approvals is a string": {`{"approvals":"yes"}`, "parse allowlist"},
		"trailing content":      {`{"approvals":[]} {}`, "trailing content"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), hermesAllowlistFile)
			if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := ConnectHermesAllowlist(path, "/opt/punk", "http://localhost:9090", false)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(after) != tc.body {
				t.Fatalf("refused merge modified the file:\n%s\n---\n%s", tc.body, after)
			}
		})
	}
}

// TestConnectHermesAllowlistUnwritableLocation pins that a location punk
// cannot create surfaces as an error (the connect command turns it into
// the hooks_auto_accept guidance) instead of a silent success.
func TestConnectHermesAllowlistUnwritableLocation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("/dev/null semantics are POSIX-only")
	}
	path := filepath.Join("/dev/null", "sub", hermesAllowlistFile)
	if _, err := ConnectHermesAllowlist(path, "/opt/punk", "http://localhost:9090", false); err == nil {
		t.Fatal("writing under /dev/null must fail")
	}
}

// TestHermesAllowlistPath pins the path resolution Hermes itself uses:
// $HERMES_HOME when set, else the platform default home.
func TestHermesAllowlistPath(t *testing.T) {
	hermesHome := t.TempDir()
	t.Setenv("HERMES_HOME", hermesHome)
	if got, want := HermesAllowlistPath(), filepath.Join(hermesHome, hermesAllowlistFile); got != want {
		t.Fatalf("HERMES_HOME path = %q, want %q", got, want)
	}

	t.Setenv("HERMES_HOME", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	var want string
	if runtime.GOOS == "windows" {
		local := t.TempDir()
		t.Setenv("LOCALAPPDATA", local)
		want = filepath.Join(local, "hermes", hermesAllowlistFile)
	} else {
		want = filepath.Join(home, ".hermes", hermesAllowlistFile)
	}
	if got := HermesAllowlistPath(); got != want {
		t.Fatalf("default path = %q, want %q", got, want)
	}
}
