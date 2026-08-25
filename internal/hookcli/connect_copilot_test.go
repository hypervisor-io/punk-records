package hookcli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// copilotHookEventNames mirrors copilotHookEvents for test assertions
// without exporting the package var's identity to callers.
var copilotHookEventNames = []string{"SessionStart", "UserPromptSubmit", "PostToolUse", "Stop", "SessionEnd"}

func readCopilotHooks(t *testing.T, path string) map[string]any {
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

// TestConnectCopilotCreatesGoldenContent pins the exact byte-for-byte JSON
// a fresh punk.json gets: all five mapped events wired, version:1,
// 2-space indent, sorted keys, trailing newline, and Copilot's own
// type/command/timeoutSec entry shape (not Cursor's bare {"command":...}).
func TestConnectCopilotCreatesGoldenContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punk.json")
	changed, err := ConnectCopilot(path, "/usr/local/bin/punk", "http://localhost:9090")
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "hooks": {
    "PostToolUse": [
      {
        "command": "/usr/local/bin/punk hook --from copilot --url http://localhost:9090",
        "timeoutSec": 10,
        "type": "command"
      }
    ],
    "SessionEnd": [
      {
        "command": "/usr/local/bin/punk hook --from copilot --url http://localhost:9090",
        "timeoutSec": 10,
        "type": "command"
      }
    ],
    "SessionStart": [
      {
        "command": "/usr/local/bin/punk hook --from copilot --url http://localhost:9090",
        "timeoutSec": 10,
        "type": "command"
      }
    ],
    "Stop": [
      {
        "command": "/usr/local/bin/punk hook --from copilot --url http://localhost:9090",
        "timeoutSec": 10,
        "type": "command"
      }
    ],
    "UserPromptSubmit": [
      {
        "command": "/usr/local/bin/punk hook --from copilot --url http://localhost:9090",
        "timeoutSec": 10,
        "type": "command"
      }
    ]
  },
  "version": 1
}
`
	if string(got) != want {
		t.Fatalf("golden mismatch:\n got: %s\nwant: %s", got, want)
	}
}

// TestConnectCopilotIdempotentRerun verifies a second connect with
// identical inputs reports changed=false and never touches the file.
func TestConnectCopilotIdempotentRerun(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punk.json")
	if _, err := ConnectCopilot(path, "/usr/local/bin/punk", "http://localhost:9090"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := ConnectCopilot(path, "/usr/local/bin/punk", "http://localhost:9090")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("re-running with identical inputs must report changed=false")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("file must be byte-identical after a no-op reconnect")
	}
}

// TestConnectCopilotMergesAllFiveEvents verifies every one of the five
// Copilot events translateCopilot maps gets a punk entry, and a foreign
// entry in an unrelated event (preToolUse) is left completely alone.
func TestConnectCopilotMergesAllFiveEvents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punk.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"hooks":{"PreToolUse":[{"type":"command","command":"./scripts/policy-gate.sh"}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := ConnectCopilot(path, "/usr/local/bin/punk", "http://localhost:9090")
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	m := readCopilotHooks(t, path)
	hooks := m["hooks"].(map[string]any)
	for _, ev := range copilotHookEventNames {
		raw, _ := json.Marshal(hooks[ev])
		if !strings.Contains(string(raw), "/usr/local/bin/punk hook --from copilot --url http://localhost:9090") {
			t.Fatalf("%s missing punk entry: %s", ev, raw)
		}
	}
	raw, _ := json.Marshal(hooks["PreToolUse"])
	if !strings.Contains(string(raw), "policy-gate.sh") {
		t.Fatal("foreign PreToolUse entry must survive untouched")
	}
	if strings.Contains(string(raw), "punk hook") {
		t.Fatal("PreToolUse must not get a punk entry - permission gating is deliberately never wired")
	}
}

// TestConnectCopilotForeignEntriesUntouchedByteExact verifies a non-punk
// entry already present in a mapped event's array survives byte-exact
// alongside the punk entry, including entries using Copilot's own
// bash/powershell fields instead of the cross-platform command field.
func TestConnectCopilotForeignEntriesUntouchedByteExact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punk.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"hooks":{"Stop":[{"type":"command","bash":"./notify.sh","powershell":"./notify.ps1"}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectCopilot(path, "/usr/local/bin/punk", "http://localhost:9090"); err != nil {
		t.Fatal(err)
	}
	m := readCopilotHooks(t, path)
	hooks := m["hooks"].(map[string]any)
	raw, _ := json.Marshal(hooks["Stop"])
	s := string(raw)
	if !strings.Contains(s, `"bash":"./notify.sh"`) || !strings.Contains(s, `"powershell":"./notify.ps1"`) {
		t.Fatalf("foreign entry must survive byte-exact: %s", s)
	}
	if !strings.Contains(s, "punk hook --from copilot") {
		t.Fatalf("punk entry missing: %s", s)
	}
}

// TestConnectCopilotURLChangeReplaces verifies reconnecting with a new
// server URL replaces the stale punk-managed entry (still exactly one
// punk entry per event) instead of appending a second one.
func TestConnectCopilotURLChangeReplaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punk.json")
	if _, err := ConnectCopilot(path, "/usr/local/bin/punk", "http://a:1"); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectCopilot(path, "/usr/local/bin/punk", "http://b:2"); err != nil {
		t.Fatal(err)
	}
	hooks := readCopilotHooks(t, path)["hooks"].(map[string]any)
	for _, ev := range copilotHookEventNames {
		raw, _ := json.Marshal(hooks[ev])
		s := string(raw)
		if strings.Count(s, "punk hook") != 1 {
			t.Fatalf("%s: expected exactly one punk entry after url change, got: %s", ev, s)
		}
		if !strings.Contains(s, "http://b:2") {
			t.Fatalf("%s: expected replaced entry to carry new url: %s", ev, s)
		}
		if strings.Contains(s, "http://a:1") {
			t.Fatalf("%s: old url entry must be replaced, not retained: %s", ev, s)
		}
	}
}

// TestConnectCopilotRelocatedBinaryReplacesStale verifies that when the
// punk binary moves, reconnecting with the new path replaces the stale
// entry left by the old path (fallback "punk hook --from copilot"
// detection).
func TestConnectCopilotRelocatedBinaryReplacesStale(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punk.json")
	if _, err := ConnectCopilot(path, "/old/bin/punk", "http://localhost:9090"); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectCopilot(path, "/new/bin/punk", "http://localhost:9090"); err != nil {
		t.Fatal(err)
	}
	hooks := readCopilotHooks(t, path)["hooks"].(map[string]any)
	for _, ev := range copilotHookEventNames {
		raw, _ := json.Marshal(hooks[ev])
		s := string(raw)
		if strings.Count(s, "punk hook") != 1 {
			t.Fatalf("%s: expected exactly one punk entry after relocation, got: %s", ev, s)
		}
		if !strings.Contains(s, "/new/bin/punk") {
			t.Fatalf("%s: expected new path present: %s", ev, s)
		}
		if strings.Contains(s, "/old/bin/punk") {
			t.Fatalf("%s: expected old path removed: %s", ev, s)
		}
	}
}

// TestIsPunkManagedCopilotPrecision verifies detection requires the exact
// "<punkPath> hook --from copilot" invocation shape, not merely a command
// that mentions the punk binary path with an unrelated subcommand or a
// plain (non-copilot) punk hook entry.
func TestIsPunkManagedCopilotPrecision(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punk.json")
	settings := `{"version":1,"hooks":{"Stop":[{"type":"command","command":"/usr/local/bin/punk hook-inspector --verbose"},{"type":"command","command":"/usr/local/bin/punk hook --url http://localhost:9090"}]}}`
	if err := os.WriteFile(path, []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectCopilot(path, "/usr/local/bin/punk", "http://localhost:9090"); err != nil {
		t.Fatal(err)
	}
	hooks := readCopilotHooks(t, path)["hooks"].(map[string]any)
	raw, _ := json.Marshal(hooks["Stop"])
	s := string(raw)
	if !strings.Contains(s, "hook-inspector") {
		t.Fatalf("unrelated punk hook-inspector entry must survive: %s", s)
	}
	if !strings.Contains(s, "hook --url http://localhost:9090\"") {
		t.Fatalf("plain punk hook entry (not ours) must survive untouched: %s", s)
	}
	if strings.Count(s, "--from copilot") != 1 {
		t.Fatalf("expected exactly one copilot-managed entry added: %s", s)
	}
}

// TestConnectCopilotNullBodyNoPanic verifies a punk.json whose entire body
// is the JSON literal `null` does not panic and connects successfully.
func TestConnectCopilotNullBodyNoPanic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punk.json")
	if err := os.WriteFile(path, []byte(`null`), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := ConnectCopilot(path, "/usr/local/bin/punk", "http://localhost:9090")
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	m := readCopilotHooks(t, path)
	hooks, ok := m["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("expected hooks object in output, got: %v", m)
	}
	raw, _ := json.Marshal(hooks["Stop"])
	if !strings.Contains(string(raw), "punk hook --from copilot") {
		t.Fatalf("Stop missing punk entry: %s", raw)
	}
}

// TestConnectCopilotRefusesWrongTypedHooksValue verifies that when "hooks"
// is present but not a JSON object, ConnectCopilot refuses to modify the
// file and names the offending key in the error, leaving the file
// byte-identical.
func TestConnectCopilotRefusesWrongTypedHooksValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punk.json")
	original := []byte(`{"hooks":"not-an-object"}`)
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ConnectCopilot(path, "/usr/local/bin/punk", "http://localhost:9090")
	if err == nil {
		t.Fatal("expected error for hooks not being an object")
	}
	if !strings.Contains(err.Error(), "hooks is not an object") {
		t.Fatalf("expected error naming the offending key, got: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatalf("file must be left untouched: got %q, want %q", after, original)
	}
}

// TestConnectCopilotRefusesWrongTypedEventValue verifies that when a
// mapped event's value under "hooks" is present but not a JSON array (e.g.
// a string), ConnectCopilot refuses to modify the file and names
// hooks.<event> in the error, leaving the file byte-identical.
func TestConnectCopilotRefusesWrongTypedEventValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punk.json")
	original := []byte(`{"hooks":{"Stop":"not-an-array"}}`)
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ConnectCopilot(path, "/usr/local/bin/punk", "http://localhost:9090")
	if err == nil {
		t.Fatal("expected error for hooks.Stop not being an array")
	}
	if !strings.Contains(err.Error(), "hooks.Stop is not an array") {
		t.Fatalf("expected error naming the offending key, got: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatalf("file must be left untouched: got %q, want %q", after, original)
	}
}
