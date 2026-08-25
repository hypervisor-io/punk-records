package hookcli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readAntigravityHooks(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestConnectAntigravityCreatesGoldenContent pins the exact byte-for-byte
// JSON a fresh hooks.json gets: the "punk" top-level key, PostToolUse in
// the matcher+hooks GROUP shape, PreInvocation/Stop in the FLAT
// handler-list shape, each command carrying the right --event value. A
// single-fact "it contains the string somewhere" check would miss a wrong
// shape, a dropped event, or a wrong --event value.
func TestConnectAntigravityCreatesGoldenContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	changed, err := ConnectAntigravity(path, "/usr/local/bin/punk", "http://localhost:9090")
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "punk": {
    "PostToolUse": [
      {
        "hooks": [
          {
            "command": "/usr/local/bin/punk hook --from antigravity --event PostToolUse --url http://localhost:9090",
            "timeout": 10,
            "type": "command"
          }
        ],
        "matcher": "*"
      }
    ],
    "PreInvocation": [
      {
        "command": "/usr/local/bin/punk hook --from antigravity --event PreInvocation --url http://localhost:9090",
        "timeout": 10,
        "type": "command"
      }
    ],
    "Stop": [
      {
        "command": "/usr/local/bin/punk hook --from antigravity --event Stop --url http://localhost:9090",
        "timeout": 10,
        "type": "command"
      }
    ]
  }
}
`
	if string(got) != want {
		t.Fatalf("golden mismatch:\n got: %s\nwant: %s", got, want)
	}
}

// TestConnectAntigravityIdempotent verifies a re-run with identical inputs
// reports changed=false and does not rewrite the file.
func TestConnectAntigravityIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	if changed, err := ConnectAntigravity(path, "/usr/local/bin/punk", "http://localhost:9090"); err != nil || !changed {
		t.Fatal(changed, err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := ConnectAntigravity(path, "/usr/local/bin/punk", "http://localhost:9090")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("idempotent re-run must report changed=false")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("idempotent re-run must not rewrite the file")
	}
}

// TestConnectAntigravityPreservesOtherHookNames verifies every other
// top-level hook-name key (a user's own "my-linter-hook"/"safety-gate", per
// antigravity.google/docs/hooks' own documented example) is left completely
// untouched - punk only ever owns the "punk" key.
func TestConnectAntigravityPreservesOtherHookNames(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	original := `{
  "my-linter-hook": {
    "PostToolUse": [
      {
        "matcher": "run_command",
        "hooks": [
          {
            "type": "command",
            "command": "./scripts/lint.sh",
            "timeout": 10
          }
        ]
      }
    ]
  },
  "safety-gate": {
    "enabled": false,
    "PreToolUse": [
      {
        "matcher": "run_command",
        "hooks": [
          {
            "command": "./scripts/safety-check.sh"
          }
        ]
      }
    ]
  }
}`
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := ConnectAntigravity(path, "/usr/local/bin/punk", "http://localhost:9090")
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	m := readAntigravityHooks(t, path)
	if _, ok := m["punk"]; !ok {
		t.Fatal("expected a punk key to be added")
	}
	linter, ok := m["my-linter-hook"].(map[string]any)
	if !ok {
		t.Fatalf("my-linter-hook must survive as an object: %#v", m["my-linter-hook"])
	}
	raw, _ := json.Marshal(linter)
	if !strings.Contains(string(raw), "./scripts/lint.sh") {
		t.Fatalf("my-linter-hook's own command must survive untouched: %s", raw)
	}
	gate, ok := m["safety-gate"].(map[string]any)
	if !ok {
		t.Fatalf("safety-gate must survive as an object: %#v", m["safety-gate"])
	}
	if enabled, ok := gate["enabled"].(bool); !ok || enabled {
		t.Fatalf("safety-gate's own enabled:false must survive untouched, got %v", gate["enabled"])
	}
	gateRaw, _ := json.Marshal(gate)
	if !strings.Contains(string(gateRaw), "./scripts/safety-check.sh") {
		t.Fatalf("safety-gate's own PreToolUse command must survive untouched: %s", gateRaw)
	}
	// PreToolUse must never be touched by punk anywhere in this file - not
	// even inside a foreign hook-name's own PreToolUse array.
	if strings.Contains(string(gateRaw), "punk hook") {
		t.Fatalf("punk must never write into a foreign hook-name's PreToolUse: %s", gateRaw)
	}
}

// TestConnectAntigravityMergesWithinPunkKey verifies a foreign, non-punk
// entry hand-added INSIDE the "punk" key's own event arrays survives a
// merge (the same per-command-string dedup discipline Claude/Cursor apply
// within their own shared arrays, applied one level deeper here), while a
// stale punk-managed entry is replaced in place.
func TestConnectAntigravityMergesWithinPunkKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	original := `{
  "punk": {
    "PostToolUse": [
      {
        "matcher": "*",
        "hooks": [
          {"type":"command","command":"./hand-added-observer.sh","timeout":5}
        ]
      }
    ],
    "PreInvocation": [
      {"type":"command","command":"/old/bin/punk hook --from antigravity --event PreInvocation --url http://stale:1","timeout":10}
    ]
  }
}`
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := ConnectAntigravity(path, "/old/bin/punk", "http://localhost:9090")
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	m := readAntigravityHooks(t, path)
	punk := m["punk"].(map[string]any)

	postRaw, _ := json.Marshal(punk["PostToolUse"])
	if !strings.Contains(string(postRaw), "./hand-added-observer.sh") {
		t.Fatalf("foreign hand-added PostToolUse group must survive: %s", postRaw)
	}
	if strings.Count(string(postRaw), "punk hook --from antigravity") != 1 {
		t.Fatalf("expected exactly one punk-managed PostToolUse group, got: %s", postRaw)
	}

	preRaw, _ := json.Marshal(punk["PreInvocation"])
	if strings.Contains(string(preRaw), "stale:1") {
		t.Fatalf("stale punk-managed PreInvocation entry must be replaced, not kept: %s", preRaw)
	}
	if !strings.Contains(string(preRaw), "http://localhost:9090") {
		t.Fatalf("expected the fresh serverURL in PreInvocation: %s", preRaw)
	}
	if strings.Count(string(preRaw), "punk hook --from antigravity") != 1 {
		t.Fatalf("expected exactly one PreInvocation entry after replacing the stale one: %s", preRaw)
	}
}

// TestConnectAntigravityRefusesWrongShapedPunkKey verifies a "punk" key
// that already exists but holds the wrong JSON shape (a string, not an
// object) is refused with an error naming the key, and the file is left
// completely untouched - never silently discarding whatever it actually
// was.
func TestConnectAntigravityRefusesWrongShapedPunkKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	original := `{"punk":"not an object"}`
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ConnectAntigravity(path, "/usr/local/bin/punk", "http://localhost:9090")
	if err == nil {
		t.Fatal("expected an error for a wrong-shaped punk key")
	}
	if !strings.Contains(err.Error(), "punk") {
		t.Fatalf("error should name the offending key, got: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != original {
		t.Fatalf("file must be left untouched on refusal: got %q, want %q", after, original)
	}
}

// TestConnectAntigravityRefusesWrongShapedEventArray verifies one of
// punk's own managed event keys holding a non-array value (e.g. an object
// instead of an array) is refused the same way, naming the offending
// nested key.
func TestConnectAntigravityRefusesWrongShapedEventArray(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	original := `{"punk":{"Stop":{"not":"an array"}}}`
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ConnectAntigravity(path, "/usr/local/bin/punk", "http://localhost:9090")
	if err == nil {
		t.Fatal("expected an error for a wrong-shaped punk.Stop value")
	}
	if !strings.Contains(err.Error(), "punk.Stop") {
		t.Fatalf("error should name punk.Stop, got: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != original {
		t.Fatalf("file must be left untouched on refusal: got %q, want %q", after, original)
	}
}

// TestConnectAntigravitySymlinkedHooksStaysSymlink mirrors the same
// symlink-preservation guarantee every other Connect* writer in this
// package pins.
func TestConnectAntigravitySymlinkedHooksStaysSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real-hooks.json")
	if err := os.WriteFile(real, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "hooks.json")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectAntigravity(link, "/usr/local/bin/punk", "http://localhost:9090"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("hooks.json symlink was replaced by a regular file")
	}
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	if target != real {
		t.Fatalf("symlink now points elsewhere: %s", target)
	}
	raw, err := os.ReadFile(real)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "punk hook --from antigravity") {
		t.Fatalf("symlink target missing punk's hook entries: %s", raw)
	}
}

// TestConnectAntigravityPreservesExistingFileMode mirrors the same
// mode-preservation guarantee every other Connect* writer in this package
// pins.
func TestConnectAntigravityPreservesExistingFileMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	if _, err := ConnectAntigravity(path, "/usr/local/bin/punk", "http://a:1"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectAntigravity(path, "/usr/local/bin/punk", "http://b:2"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("expected mode 0600 preserved, got %o", fi.Mode().Perm())
	}
}

// TestConnectAntigravityCreatesParentDirs verifies ConnectAntigravity
// creates the hooks.json's parent directory tree (e.g. ./.agents/ or
// ~/.gemini/config/, neither of which typically pre-exists) rather than
// requiring the caller to mkdir first.
func TestConnectAntigravityCreatesParentDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agents-dir", "hooks.json")
	changed, err := ConnectAntigravity(path, "/usr/local/bin/punk", "http://localhost:9090")
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected hooks.json to exist after parent dir creation: %v", err)
	}
}

// TestConnectAntigravityHostilePunkPathQuoting mirrors ConnectClaudeCode/
// ConnectCursor's whitespace-in-path handling: a punkPath containing a
// space must round-trip through isPunkManagedAntigravity's optional-quote
// tolerance on a second run (replace in place, not duplicate).
func TestConnectAntigravityHostilePunkPathQuoting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	punkPath := "/opt/my apps/punk"
	if _, err := ConnectAntigravity(path, punkPath, "http://localhost:9090"); err != nil {
		t.Fatal(err)
	}
	changed, err := ConnectAntigravity(path, punkPath, "http://localhost:9090")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("second run with unchanged whitespace-containing punkPath must report changed=false")
	}
	m := readAntigravityHooks(t, path)
	punk := m["punk"].(map[string]any)

	wantCmd := `"/opt/my apps/punk" hook --from antigravity --event %s --url http://localhost:9090`
	postGroups := punk["PostToolUse"].([]any)
	if len(postGroups) != 1 {
		t.Fatalf("expected exactly one PostToolUse group, got %d", len(postGroups))
	}
	postHooks := postGroups[0].(map[string]any)["hooks"].([]any)
	if got := postHooks[0].(map[string]any)["command"].(string); got != fmt.Sprintf(wantCmd, "PostToolUse") {
		t.Fatalf("PostToolUse command = %q, want %q", got, fmt.Sprintf(wantCmd, "PostToolUse"))
	}
	pre := punk["PreInvocation"].([]any)
	if len(pre) != 1 {
		t.Fatalf("expected exactly one PreInvocation entry, got %d", len(pre))
	}
	if got := pre[0].(map[string]any)["command"].(string); got != fmt.Sprintf(wantCmd, "PreInvocation") {
		t.Fatalf("PreInvocation command = %q, want %q", got, fmt.Sprintf(wantCmd, "PreInvocation"))
	}
	stop := punk["Stop"].([]any)
	if len(stop) != 1 {
		t.Fatalf("expected exactly one Stop entry, got %d", len(stop))
	}
	if got := stop[0].(map[string]any)["command"].(string); got != fmt.Sprintf(wantCmd, "Stop") {
		t.Fatalf("Stop command = %q, want %q", got, fmt.Sprintf(wantCmd, "Stop"))
	}
}

// isPunkManagedAntigravityHookInspectorNeverMatches is a distractor input
// name pinning the same word-boundary precision isPunkManaged/
// isPunkManagedCursor already guarantee: a user's own tool invoked as
// "<punkPath> hook-inspector ..." must never be misdetected as punk's own
// antigravity entry.
func TestIsPunkManagedAntigravityWordBoundary(t *testing.T) {
	const punkPath = "/usr/local/bin/punk"
	cases := []struct {
		cmd  string
		want bool
	}{
		{`/usr/local/bin/punk hook --from antigravity --event Stop --url http://localhost:9090`, true},
		{`"/usr/local/bin/punk" hook --from antigravity --event Stop --url http://localhost:9090`, true},
		{`/usr/local/bin/punk hook-inspector --from antigravity`, false},
		{`/usr/local/bin/punk hook --from cursor --url http://localhost:9090`, false},
		{`./scripts/lint.sh`, false},
	}
	for _, tc := range cases {
		if got := isPunkManagedAntigravity(tc.cmd, punkPath); got != tc.want {
			t.Errorf("isPunkManagedAntigravity(%q, %q) = %v, want %v", tc.cmd, punkPath, got, tc.want)
		}
	}
	// ...but the relocation fallback marker still catches it independent of
	// punkPath, the same deliberate false-positive tradeoff isPunkManaged/
	// isPunkManagedCursor document.
	if !isPunkManagedAntigravity(`/old/bin/punk hook --from antigravity --event Stop --url http://stale:1`, "/new/bin/punk") {
		t.Fatal("relocation fallback must still recognize a stale-path punk antigravity command")
	}
}
