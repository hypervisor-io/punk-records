package hookcli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cursorHookEventNames mirrors cursorHookEvents for test assertions without
// exporting the package var's identity to callers.
var cursorHookEventNames = []string{"sessionStart", "beforeSubmitPrompt", "postToolUse", "afterFileEdit", "stop", "sessionEnd"}

func readCursorHooks(t *testing.T, path string) map[string]any {
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

// TestConnectCursorCreatesGoldenContent pins the exact byte-for-byte JSON a
// fresh hooks.json gets: all six mapped events wired, version:1, 2-space
// indent, sorted keys, trailing newline. A single-fact "it contains the
// string somewhere" check would miss an accidental extra key, wrong
// indentation, or an event silently dropped from the six-event mapping.
func TestConnectCursorCreatesGoldenContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	changed, err := ConnectCursor(path, "/usr/local/bin/punk", "http://localhost:9090")
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "hooks": {
    "afterFileEdit": [
      {
        "command": "/usr/local/bin/punk hook --from cursor --url http://localhost:9090"
      }
    ],
    "beforeSubmitPrompt": [
      {
        "command": "/usr/local/bin/punk hook --from cursor --url http://localhost:9090"
      }
    ],
    "postToolUse": [
      {
        "command": "/usr/local/bin/punk hook --from cursor --url http://localhost:9090"
      }
    ],
    "sessionEnd": [
      {
        "command": "/usr/local/bin/punk hook --from cursor --url http://localhost:9090"
      }
    ],
    "sessionStart": [
      {
        "command": "/usr/local/bin/punk hook --from cursor --url http://localhost:9090"
      }
    ],
    "stop": [
      {
        "command": "/usr/local/bin/punk hook --from cursor --url http://localhost:9090"
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

// TestConnectCursorPreservesExistingVersion verifies an existing "version"
// value is kept as-is (never overwritten to 1) when the file already has
// one, mirroring ConnectClaudeCode's "unrelated settings must survive".
func TestConnectCursorPreservesExistingVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	if err := os.WriteFile(path, []byte(`{"version":2,"hooks":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectCursor(path, "/usr/local/bin/punk", "http://localhost:9090"); err != nil {
		t.Fatal(err)
	}
	m := readCursorHooks(t, path)
	if n, ok := m["version"].(float64); !ok || n != 2 {
		t.Fatalf("expected existing version 2 preserved, got %v", m["version"])
	}
}

// TestConnectCursorMergesAllSixEvents verifies every one of the six Cursor
// events T1.1's translator maps gets a punk entry, and a foreign entry in
// an unrelated event (workspaceOpen) is left completely alone.
func TestConnectCursorMergesAllSixEvents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"hooks":{"workspaceOpen":[{"command":"./register-workspace-plugins.sh"}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := ConnectCursor(path, "/usr/local/bin/punk", "http://localhost:9090")
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	m := readCursorHooks(t, path)
	hooks := m["hooks"].(map[string]any)
	for _, ev := range cursorHookEventNames {
		raw, _ := json.Marshal(hooks[ev])
		if !strings.Contains(string(raw), "/usr/local/bin/punk hook --from cursor --url http://localhost:9090") {
			t.Fatalf("%s missing punk entry: %s", ev, raw)
		}
	}
	raw, _ := json.Marshal(hooks["workspaceOpen"])
	if !strings.Contains(string(raw), "register-workspace-plugins.sh") {
		t.Fatal("foreign workspaceOpen entry must survive untouched")
	}
	if strings.Contains(string(raw), "punk hook") {
		t.Fatal("workspaceOpen must not get a punk entry - it is not one of the six mapped events")
	}
}

// TestConnectCursorForeignEntriesUntouchedByteExact verifies a non-punk
// entry already present in a mapped event's array survives byte-exact
// alongside the punk entry, rather than being reformatted or dropped.
func TestConnectCursorForeignEntriesUntouchedByteExact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"hooks":{"stop":[{"command":"./my-audit.sh","loop_limit":10}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectCursor(path, "/usr/local/bin/punk", "http://localhost:9090"); err != nil {
		t.Fatal(err)
	}
	m := readCursorHooks(t, path)
	hooks := m["hooks"].(map[string]any)
	raw, _ := json.Marshal(hooks["stop"])
	s := string(raw)
	if !strings.Contains(s, `"command":"./my-audit.sh","loop_limit":10`) {
		t.Fatalf("foreign entry must survive byte-exact: %s", s)
	}
	if !strings.Contains(s, "punk hook --from cursor") {
		t.Fatalf("punk entry missing: %s", s)
	}
}

// TestConnectCursorURLChangeReplaces verifies reconnecting with a new
// server URL replaces the stale punk-managed entry (still exactly one
// punk entry per event) instead of appending a second one.
func TestConnectCursorURLChangeReplaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	if _, err := ConnectCursor(path, "/usr/local/bin/punk", "http://a:1"); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectCursor(path, "/usr/local/bin/punk", "http://b:2"); err != nil {
		t.Fatal(err)
	}
	hooks := readCursorHooks(t, path)["hooks"].(map[string]any)
	for _, ev := range cursorHookEventNames {
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

// TestConnectCursorRelocatedBinaryReplacesStale verifies that when the punk
// binary moves, reconnecting with the new path replaces the stale group
// left by the old path (fallback "punk hook --from cursor" detection).
func TestConnectCursorRelocatedBinaryReplacesStale(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	if _, err := ConnectCursor(path, "/old/bin/punk", "http://localhost:9090"); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectCursor(path, "/new/bin/punk", "http://localhost:9090"); err != nil {
		t.Fatal(err)
	}
	hooks := readCursorHooks(t, path)["hooks"].(map[string]any)
	for _, ev := range cursorHookEventNames {
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

// TestIsPunkManagedCursorPrecision verifies detection requires the exact
// "<punkPath> hook --from cursor" invocation shape, not merely a command
// that mentions the punk binary path with an unrelated subcommand or a
// plain (non-cursor) punk hook entry.
func TestIsPunkManagedCursorPrecision(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	settings := `{"version":1,"hooks":{"stop":[{"command":"/usr/local/bin/punk hook-inspector --verbose"},{"command":"/usr/local/bin/punk hook --url http://localhost:9090"}]}}`
	if err := os.WriteFile(path, []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectCursor(path, "/usr/local/bin/punk", "http://localhost:9090"); err != nil {
		t.Fatal(err)
	}
	hooks := readCursorHooks(t, path)["hooks"].(map[string]any)
	raw, _ := json.Marshal(hooks["stop"])
	s := string(raw)
	if !strings.Contains(s, "hook-inspector") {
		t.Fatalf("unrelated punk hook-inspector entry must survive: %s", s)
	}
	// The plain (non-cursor) "punk hook --url ..." entry is a DIFFERENT
	// managed group (Claude Code's, not Cursor's) and must also survive
	// untouched - ConnectCursor only owns "--from cursor" entries.
	if !strings.Contains(s, "hook --url http://localhost:9090\"") {
		t.Fatalf("plain punk hook entry (not ours) must survive untouched: %s", s)
	}
	if strings.Count(s, "--from cursor") != 1 {
		t.Fatalf("expected exactly one cursor-managed entry added: %s", s)
	}
}

// TestConnectCursorNullBodyNoPanic verifies a hooks.json whose entire body
// is the JSON literal `null` does not panic and connects successfully.
func TestConnectCursorNullBodyNoPanic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	if err := os.WriteFile(path, []byte(`null`), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := ConnectCursor(path, "/usr/local/bin/punk", "http://localhost:9090")
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	m := readCursorHooks(t, path)
	hooks, ok := m["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("expected hooks object in output, got: %v", m)
	}
	raw, _ := json.Marshal(hooks["stop"])
	if !strings.Contains(string(raw), "punk hook --from cursor") {
		t.Fatalf("stop missing punk entry: %s", raw)
	}
}

// TestConnectCursorRefusesWrongTypedHooksValue verifies that when "hooks"
// is present but not a JSON object, ConnectCursor refuses to modify the
// file and names the offending key in the error.
func TestConnectCursorRefusesWrongTypedHooksValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	original := []byte(`{"hooks":"not-an-object"}`)
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ConnectCursor(path, "/usr/local/bin/punk", "http://localhost:9090")
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

// TestConnectCursorRefusesWrongTypedEventValue verifies that when a mapped
// event's value under "hooks" is present but not a JSON array (e.g. a
// string), ConnectCursor refuses to modify the file and names hooks.<event>
// in the error, leaving the file byte-identical.
func TestConnectCursorRefusesWrongTypedEventValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	original := []byte(`{"hooks":{"stop":"not-an-array"}}`)
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ConnectCursor(path, "/usr/local/bin/punk", "http://localhost:9090")
	if err == nil {
		t.Fatal("expected error for hooks.stop not being an array")
	}
	if !strings.Contains(err.Error(), "hooks.stop is not an array") {
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

// TestConnectCursorEntryThatIsNotAnObjectSurvives verifies a hostile entry
// in a mapped event's array that is not a JSON object (e.g. a bare string)
// does not panic, is left untouched, and the punk entry is still added.
func TestConnectCursorEntryThatIsNotAnObjectSurvives(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	original := []byte(`{"hooks":{"stop":["not-an-object", 42, null]}}`)
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := ConnectCursor(path, "/usr/local/bin/punk", "http://localhost:9090")
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	m := readCursorHooks(t, path)
	hooks := m["hooks"].(map[string]any)
	raw, _ := json.Marshal(hooks["stop"])
	s := string(raw)
	if !strings.Contains(s, "not-an-object") {
		t.Fatalf("hostile non-object entry must survive: %s", s)
	}
	if !strings.Contains(s, "punk hook --from cursor") {
		t.Fatalf("punk entry missing: %s", s)
	}
}

// TestConnectCursorNoOpRerunReportsUnchanged verifies that reconnecting
// with identical inputs against a hooks.json that already carries the
// resulting content reports changed=false and does not rewrite the file.
func TestConnectCursorNoOpRerunReportsUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	if changed, err := ConnectCursor(path, "/usr/local/bin/punk", "http://localhost:9090"); err != nil || !changed {
		t.Fatal(changed, err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := ConnectCursor(path, "/usr/local/bin/punk", "http://localhost:9090")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("no-op reconnect must report changed=false")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("no-op reconnect must not rewrite the file")
	}
}

// TestConnectCursorSymlinkedHooksStaysSymlink verifies that when
// hooks.json is a symlink, connecting updates the content the symlink
// points at in place rather than replacing the symlink with a plain file.
func TestConnectCursorSymlinkedHooksStaysSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real-hooks.json")
	if err := os.WriteFile(real, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "hooks.json")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectCursor(link, "/usr/local/bin/punk", "http://localhost:9090"); err != nil {
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
	if !strings.Contains(string(raw), "punk hook --from cursor") {
		t.Fatalf("symlink target missing punk entry: %s", raw)
	}
}

// TestConnectCursorPreservesExistingFileMode verifies connecting against an
// existing hooks.json does not widen its permissions.
func TestConnectCursorPreservesExistingFileMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectCursor(path, "/usr/local/bin/punk", "http://localhost:9090"); err != nil {
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

// TestConnectCursorQuotesPunkPathWithSpaces verifies a punkPath containing
// whitespace is double-quoted in the generated command, and isPunkManaged
// still recognizes its own quoted entry on a re-run.
func TestConnectCursorQuotesPunkPathWithSpaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	punkPath := "/opt/my apps/punk"
	if _, err := ConnectCursor(path, punkPath, "http://localhost:9090"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := `\"` + punkPath + `\" hook --from cursor --url http://localhost:9090`
	if !strings.Contains(string(raw), want) {
		t.Fatalf("expected quoted punk path in command, got: %s", raw)
	}
	if _, err := ConnectCursor(path, punkPath, "http://localhost:9090"); err != nil {
		t.Fatal(err)
	}
	hooks := readCursorHooks(t, path)["hooks"].(map[string]any)
	raw2, _ := json.Marshal(hooks["stop"])
	if strings.Count(string(raw2), "hook --from cursor") != 1 {
		t.Fatalf("re-run with spaced path duplicated entries: %s", raw2)
	}
}

// ---- WriteCursorRules ----

// TestWriteCursorRulesGoldenContent pins the EXACT full byte content
// written for a fresh rules file - frontmatter fencing and the managed
// marker's position (right after the closing `---`, before the body) are
// load-bearing: MDC only honors alwaysApply from a well-formed frontmatter
// block, and the marker must be present verbatim (see cursorRulesMarker)
// for WriteCursorRules to recognize the file as punk-managed on a later
// run. A mere strings.Contains check would pass even if the frontmatter
// fence were malformed or the marker landed in the wrong place.
func TestWriteCursorRulesGoldenContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punk-memory.mdc")
	changed, err := WriteCursorRules(path, "http://localhost:9090")
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "---\n" +
		"alwaysApply: true\n" +
		"description: Load and persist project memory via the punk-records MCP server\n" +
		"---\n" +
		"<!-- managed by punk connect cursor -->\n" +
		"\n" +
		"# Punk memory\n" +
		"\n" +
		"At the start of every session, call the punk-records MCP tool `recall` (or `search` if `recall` is unavailable) for this project's namespace to load prior context before doing anything else.\n" +
		"\n" +
		"When you learn something durable during the session (a decision, a fix, a convention, a gotcha), call the punk-records MCP tool `remember` to persist it so future sessions can find it.\n" +
		"\n" +
		"Punk-records server: http://localhost:9090\n"
	if string(got) != want {
		t.Fatalf("golden mismatch:\n got:  %q\nwant: %q", got, want)
	}
}

// TestWriteCursorRulesIdempotent verifies identical content on a re-run
// reports changed=false and does not rewrite the file.
func TestWriteCursorRulesIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punk-memory.mdc")
	if changed, err := WriteCursorRules(path, "http://localhost:9090"); err != nil || !changed {
		t.Fatal(changed, err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := WriteCursorRules(path, "http://localhost:9090")
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

// TestWriteCursorRulesRefusesUnmanagedExisting verifies that an existing
// rules file without the managed marker is left untouched and refused
// with an error naming the path - overwriting a user's own hand-authored
// punk-memory.mdc would destroy their content silently otherwise.
func TestWriteCursorRulesRefusesUnmanagedExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punk-memory.mdc")
	original := []byte("---\nalwaysApply: true\n---\nmy own hand-written rule\n")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := WriteCursorRules(path, "http://localhost:9090")
	if err == nil {
		t.Fatal("expected error for unmanaged existing rules file")
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("expected error naming the path, got: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatalf("unmanaged file must be left untouched: got %q, want %q", after, original)
	}
}

// TestWriteCursorRulesUpdatesStaleManagedContent verifies that a rules
// file which DOES carry the managed marker (e.g. from a prior punk
// version, or a different serverURL) is updated in place.
func TestWriteCursorRulesUpdatesStaleManagedContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punk-memory.mdc")
	stale := "---\nalwaysApply: true\ndescription: old\n---\n" + cursorRulesMarker + "\nold body mentioning http://old:1\n"
	if err := os.WriteFile(path, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := WriteCursorRules(path, "http://localhost:9090")
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(after), "http://old:1") {
		t.Fatalf("stale content must be replaced: %s", after)
	}
	if !strings.Contains(string(after), "http://localhost:9090") {
		t.Fatalf("expected new server URL: %s", after)
	}
}

// TestWriteCursorRulesPreservesExistingFileMode verifies writing to an
// existing managed rules file does not widen its permissions.
func TestWriteCursorRulesPreservesExistingFileMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punk-memory.mdc")
	if _, err := WriteCursorRules(path, "http://a:1"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteCursorRules(path, "http://b:2"); err != nil {
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

// TestWriteCursorRulesSymlinkPreserved verifies writing through a symlinked
// rules path updates the symlink target rather than replacing the symlink.
func TestWriteCursorRulesSymlinkPreserved(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real-punk-memory.mdc")
	if err := os.WriteFile(real, []byte(cursorRulesContent("http://a:1")), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "punk-memory.mdc")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteCursorRules(link, "http://b:2"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("rules symlink was replaced by a regular file")
	}
	raw, err := os.ReadFile(real)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "http://b:2") {
		t.Fatalf("symlink target missing updated content: %s", raw)
	}
}
