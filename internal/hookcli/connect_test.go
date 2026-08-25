package hookcli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readSettings(t *testing.T, path string) map[string]any {
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

func TestConnectCreatesAndMerges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	// pre-existing user hook must survive
	if err := os.WriteFile(path, []byte(`{"model":"opus","hooks":{"PostToolUse":[{"matcher":"Edit","hooks":[{"type":"command","command":"my-linter.sh"}]}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := ConnectClaudeCode(path, "/usr/local/bin/punk", "http://localhost:9090")
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	m := readSettings(t, path)
	if m["model"] != "opus" {
		t.Fatal("unrelated settings must survive")
	}
	hooks := m["hooks"].(map[string]any)
	for _, ev := range []string{"SessionStart", "UserPromptSubmit", "PostToolUse", "Stop"} {
		raw, _ := json.Marshal(hooks[ev])
		if !strings.Contains(string(raw), "/usr/local/bin/punk hook --url http://localhost:9090") {
			t.Fatalf("%s missing punk entry: %s", ev, raw)
		}
	}
	if raw, _ := json.Marshal(hooks["PostToolUse"]); !strings.Contains(string(raw), "my-linter.sh") {
		t.Fatal("user PostToolUse entry must survive")
	}
	// idempotent: re-run changes nothing structurally (still exactly one punk entry per event)
	if _, err := ConnectClaudeCode(path, "/usr/local/bin/punk", "http://localhost:9090"); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(readSettings(t, path)["hooks"].(map[string]any)["Stop"])
	if strings.Count(string(raw), "punk hook") != 1 {
		t.Fatalf("re-run duplicated entries: %s", raw)
	}
	// fresh file
	fresh := filepath.Join(dir, "sub", "settings.json")
	if changed, err := ConnectClaudeCode(fresh, "/bin/punk", "http://h:1"); err != nil || !changed {
		t.Fatal(changed, err)
	}
}

// TestConnectURLChangeReplaces verifies that reconnecting with a new
// server URL replaces the punk-managed group (still exactly one punk
// entry per event) rather than appending a second one alongside it.
func TestConnectURLChangeReplaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if _, err := ConnectClaudeCode(path, "/usr/local/bin/punk", "http://a:1"); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectClaudeCode(path, "/usr/local/bin/punk", "http://b:2"); err != nil {
		t.Fatal(err)
	}
	hooks := readSettings(t, path)["hooks"].(map[string]any)
	for _, ev := range []string{"SessionStart", "UserPromptSubmit", "PostToolUse", "Stop"} {
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

// TestConnectCorruptedSettingsNotTouched verifies that an unparseable
// settings.json produces an error and the file is left byte-identical -
// a live user config file must never be clobbered on a failed parse.
func TestConnectCorruptedSettingsNotTouched(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	original := []byte(`{not valid json`)
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectClaudeCode(path, "/usr/local/bin/punk", "http://localhost:9090"); err == nil {
		t.Fatal("expected error for corrupted settings.json")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatalf("corrupted file must be left untouched: got %q, want %q", after, original)
	}
}

// TestConnectUserSessionStartEntrySurvives verifies a user-authored
// SessionStart entry survives alongside the punk-managed group, mirroring
// the PostToolUse survival check but for a different event to catch a
// fix that special-cased PostToolUse alone.
func TestConnectUserSessionStartEntrySurvives(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"my-session-init.sh"}]}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectClaudeCode(path, "/usr/local/bin/punk", "http://localhost:9090"); err != nil {
		t.Fatal(err)
	}
	hooks := readSettings(t, path)["hooks"].(map[string]any)
	raw, _ := json.Marshal(hooks["SessionStart"])
	s := string(raw)
	if !strings.Contains(s, "my-session-init.sh") {
		t.Fatalf("user SessionStart entry must survive: %s", s)
	}
	if !strings.Contains(s, "/usr/local/bin/punk hook --url http://localhost:9090") {
		t.Fatalf("punk SessionStart entry must be added: %s", s)
	}
}

// TestConnectNoOpRerunReportsUnchanged verifies that reconnecting with
// identical inputs (same punkPath, same serverURL) against a
// settings.json that already carries the resulting content reports
// changed=false and does not rewrite the file (mtime/content untouched).
// This pins the "write only when different" behavior called for beyond
// the plan's literal test, which only checks structural non-duplication.
func TestConnectNoOpRerunReportsUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if changed, err := ConnectClaudeCode(path, "/usr/local/bin/punk", "http://localhost:9090"); err != nil || !changed {
		t.Fatal(changed, err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := ConnectClaudeCode(path, "/usr/local/bin/punk", "http://localhost:9090")
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

// TestIsPunkManagedPrecision verifies punk-managed group detection
// requires the exact "<punkPath> hook" invocation, not merely a command
// that mentions the punk binary path with an unrelated subcommand. A
// user-authored "punk hook-inspector" entry must never be treated as
// punk-managed (and therefore silently replaced/removed) by a
// substring-only match.
func TestIsPunkManagedPrecision(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	// user has their own tool literally named "punk" with a "hook-inspector"
	// subcommand at the same path our punk binary would occupy - a naive
	// `strings.Contains(cmd, punkPath) && strings.Contains(cmd, " hook")`
	// check false-positives on this.
	settings := `{"hooks":{"PostToolUse":[{"matcher":"*","hooks":[{"type":"command","command":"/usr/local/bin/punk hook-inspector --verbose"}]}]}}`
	if err := os.WriteFile(path, []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectClaudeCode(path, "/usr/local/bin/punk", "http://localhost:9090"); err != nil {
		t.Fatal(err)
	}
	hooks := readSettings(t, path)["hooks"].(map[string]any)
	raw, _ := json.Marshal(hooks["PostToolUse"])
	s := string(raw)
	if !strings.Contains(s, "hook-inspector") {
		t.Fatalf("unrelated punk hook-inspector entry must survive untouched: %s", s)
	}
	if strings.Count(s, "punk hook") != 2 {
		// "punk hook-inspector" contains "punk hook" as a substring, plus
		// our real "punk hook --url ..." entry - exactly two textual
		// occurrences of "punk hook" is expected once both groups
		// coexist. This pins that we did NOT dedupe/replace the
		// hook-inspector entry as if it were punk-managed.
		t.Fatalf("expected both the unrelated entry and our managed entry to coexist: %s", s)
	}
	// run again - must remain stable (still coexisting, no runaway duplication)
	if _, err := ConnectClaudeCode(path, "/usr/local/bin/punk", "http://localhost:9090"); err != nil {
		t.Fatal(err)
	}
	hooks2 := readSettings(t, path)["hooks"].(map[string]any)
	raw2, _ := json.Marshal(hooks2["PostToolUse"])
	s2 := string(raw2)
	if strings.Count(s2, "hook-inspector") != 1 {
		t.Fatalf("re-run must not duplicate the unrelated entry: %s", s2)
	}
	if strings.Count(s2, "--url http://localhost:9090") != 1 {
		t.Fatalf("re-run must not duplicate punk's own managed entry: %s", s2)
	}
}

// TestConnectNullSettingsBodyNoPanic verifies that a settings.json whose
// entire body is the JSON literal `null` (valid JSON, decodes to a nil
// map) does not panic ("assignment to entry in nil map") and instead
// connects successfully, producing a valid settings document.
func TestConnectNullSettingsBodyNoPanic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`null`), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := ConnectClaudeCode(path, "/usr/local/bin/punk", "http://localhost:9090")
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	m := readSettings(t, path)
	hooks, ok := m["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("expected hooks object in output, got: %v", m)
	}
	raw, _ := json.Marshal(hooks["Stop"])
	if !strings.Contains(string(raw), "punk hook --url http://localhost:9090") {
		t.Fatalf("Stop missing punk entry: %s", raw)
	}
}

// TestConnectRefusesWrongTypedHooksValue verifies that when "hooks" is
// present but not a JSON object (e.g. a string), ConnectClaudeCode
// refuses to modify the file rather than silently discarding it, and
// leaves the file byte-identical.
func TestConnectRefusesWrongTypedHooksValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	original := []byte(`{"hooks":"not-an-object"}`)
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ConnectClaudeCode(path, "/usr/local/bin/punk", "http://localhost:9090")
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

// TestConnectRefusesWrongTypedEventValue verifies that when an event's
// value under "hooks" is present but not a JSON array (e.g. an object),
// ConnectClaudeCode refuses to modify the file rather than silently
// discarding the offending value, and leaves the file byte-identical.
func TestConnectRefusesWrongTypedEventValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	original := []byte(`{"hooks":{"Stop":{"not":"array"}}}`)
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ConnectClaudeCode(path, "/usr/local/bin/punk", "http://localhost:9090")
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

// TestConnectPreservesNumberPrecision verifies that numbers outside
// float64's exact-integer range and numbers with a trailing ".0" survive
// a connect round-trip byte-for-byte, rather than losing precision or
// being reformatted by a naive float64 decode/re-encode.
func TestConnectPreservesNumberPrecision(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	original := []byte(`{"bigNum":12345678901234567890,"one":1.0}`)
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectClaudeCode(path, "/usr/local/bin/punk", "http://localhost:9090"); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(after)
	if !strings.Contains(s, "12345678901234567890") {
		t.Fatalf("big integer lost precision: %s", s)
	}
	if !strings.Contains(s, `"one": 1.0`) {
		t.Fatalf("1.0 was reformatted: %s", s)
	}
}

// TestConnectDoesNotHTMLEscapeCommands verifies that shell metacharacters
// like && in an existing hook command survive verbatim rather than being
// HTML-escaped (&&) by the default json.Marshal behavior.
func TestConnectDoesNotHTMLEscapeCommands(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	original := []byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"echo x && date"}]}]}}`)
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectClaudeCode(path, "/usr/local/bin/punk", "http://localhost:9090"); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(after)
	if !strings.Contains(s, "echo x && date") {
		t.Fatalf("&& was mangled: %s", s)
	}
	if strings.Contains(s, "\\u0026") {
		t.Fatalf("command was HTML-escaped: %s", s)
	}
}

// TestConnectSymlinkedSettingsStaysSymlink verifies that when
// settings.json is a symlink into a dotfiles repo, connecting updates the
// content the symlink points at in place rather than replacing the
// symlink itself with a regular file (which would silently detach the
// dotfiles repo from Claude Code's config).
func TestConnectSymlinkedSettingsStaysSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real-settings.json")
	if err := os.WriteFile(real, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "settings.json")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectClaudeCode(link, "/usr/local/bin/punk", "http://localhost:9090"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("settings.json symlink was replaced by a regular file")
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
	if !strings.Contains(string(raw), "punk hook --url http://localhost:9090") {
		t.Fatalf("symlink target missing punk entry: %s", raw)
	}
}

// TestConnectPreservesExistingFileMode verifies that connecting against
// an existing settings.json does not widen its permissions (e.g. a
// user-set 0600 because settings.json can hold env secrets or
// apiKeyHelper) - only a freshly created file gets the default 0644.
func TestConnectPreservesExistingFileMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectClaudeCode(path, "/usr/local/bin/punk", "http://localhost:9090"); err != nil {
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

// TestConnectQuotesPunkPathWithSpaces verifies that a punkPath containing
// whitespace is double-quoted in the generated hook command (otherwise
// the shell splits it into multiple words), and that isPunkManaged still
// recognizes its own quoted entry on a re-run (idempotent: single entry).
func TestConnectQuotesPunkPathWithSpaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	punkPath := "/opt/my apps/punk"
	if _, err := ConnectClaudeCode(path, punkPath, "http://localhost:9090"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := `\"` + punkPath + `\" hook --url http://localhost:9090`
	if !strings.Contains(string(raw), want) {
		t.Fatalf("expected quoted punk path in command, got: %s", raw)
	}
	// idempotent re-run: still exactly one punk entry
	if _, err := ConnectClaudeCode(path, punkPath, "http://localhost:9090"); err != nil {
		t.Fatal(err)
	}
	hooks := readSettings(t, path)["hooks"].(map[string]any)
	raw2, _ := json.Marshal(hooks["Stop"])
	if strings.Count(string(raw2), "hook --url") != 1 {
		t.Fatalf("re-run with spaced path duplicated entries: %s", raw2)
	}
}

// TestConnectRelocatedBinaryReplacesStaleGroup verifies that when the
// punk binary moves (e.g. /old/bin/punk -> /new/bin/punk), reconnecting
// with the new path replaces the stale group left by the old path
// instead of leaving two punk-managed groups (one permanently failing)
// side by side.
func TestConnectRelocatedBinaryReplacesStaleGroup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if _, err := ConnectClaudeCode(path, "/old/bin/punk", "http://localhost:9090"); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectClaudeCode(path, "/new/bin/punk", "http://localhost:9090"); err != nil {
		t.Fatal(err)
	}
	hooks := readSettings(t, path)["hooks"].(map[string]any)
	for _, ev := range []string{"SessionStart", "UserPromptSubmit", "PostToolUse", "Stop"} {
		raw, _ := json.Marshal(hooks[ev])
		s := string(raw)
		if strings.Count(s, "punk hook") != 1 {
			t.Fatalf("%s: expected exactly one punk group after relocation, got: %s", ev, s)
		}
		if !strings.Contains(s, "/new/bin/punk") {
			t.Fatalf("%s: expected new path present: %s", ev, s)
		}
		if strings.Contains(s, "/old/bin/punk") {
			t.Fatalf("%s: expected old path removed: %s", ev, s)
		}
	}
}
