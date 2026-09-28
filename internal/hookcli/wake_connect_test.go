package hookcli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Wake connect tests (W5, 2026-09-28 native wake plan). The wake groups
// are a third managed group kind beside capture and inbox; these tests
// pin the command shape, the detector distinctness, the install layout
// (ensure on SessionStart/UserPromptSubmit/Stop, stop on SessionEnd),
// idempotent and cross-mode reconnect byte-stability, foreign-group
// preservation, and the Codex scope dedupe.

const wakeUserSettings = `{"model":"opus","hooks":{"Stop":[{"hooks":[{"type":"command","command":"my-stop.sh"}]}],"SessionStart":[{"matcher":"startup","hooks":[{"type":"command","command":"echo hi && date"}]}],"SessionEnd":[{"hooks":[{"type":"command","command":"my-session-end.sh"}]}]}}`

func wakeGroups(t *testing.T, hooks map[string]any, event, client string) []map[string]any {
	t.Helper()
	var out []map[string]any
	arr, _ := hooks[event].([]any)
	for _, g := range arr {
		if isPunkManagedWakeGroup(g, "/usr/local/bin/punk", client) {
			out = append(out, g.(map[string]any))
		}
	}
	return out
}

func wakeGroupCommand(t *testing.T, g map[string]any) string {
	t.Helper()
	h := g["hooks"].([]any)[0].(map[string]any)
	if h["type"] != "command" {
		t.Fatalf("wake handler type %v", h)
	}
	cmd, _ := h["command"].(string)
	return cmd
}

func TestPunkWakeHookCommandAndDetector(t *testing.T) {
	p := "/usr/local/bin/punk"
	cmd := punkWakeHookCommand(p, "claude-code", "ensure", "http://localhost:9090", "proj-ns")
	want := p + " hook wake --client claude-code --action ensure --url http://localhost:9090 --ns proj-ns --messaging"
	if cmd != want {
		t.Fatalf("got %q want %q", cmd, want)
	}
	if !isPunkManagedWake(cmd, p, "claude-code") {
		t.Fatal("own command not detected")
	}
	if isPunkManagedWake(cmd, p, "codex") {
		t.Fatal("client scoping leaked")
	}
	// A stale path is still ours (binary relocation fallback).
	stale := "/old/bin/punk hook wake --client claude-code --action stop --url http://x --messaging"
	if !isPunkManagedWake(stale, p, "claude-code") {
		t.Fatal("stale path fallback lost")
	}
	// Quoted path with whitespace, like quotePunkPath emits.
	quoted := punkWakeHookCommand("/opt/my tools/punk", "codex", "stop", "http://x", "")
	if !strings.HasPrefix(quoted, `"/opt/my tools/punk"`) || !isPunkManagedWake(quoted, "/opt/my tools/punk", "codex") {
		t.Fatalf("quoted command %q", quoted)
	}
	// No secret material ever enters the command.
	for _, c := range []string{cmd, quoted, stale} {
		if strings.Contains(c, "token") || strings.Contains(c, "PUNK_API_KEY") || strings.Contains(c, "sock") {
			t.Fatalf("command carries endpoint/secret material: %q", c)
		}
	}
}

// The three managed detectors must be mutually exclusive: capture,
// inbox and wake entries never claim one another, in either direction.
func TestWakeDetectorDistinctFromCaptureAndInbox(t *testing.T) {
	p := "/usr/local/bin/punk"
	wake := punkWakeHookCommand(p, "claude-code", "ensure", "http://x", "")
	inbox := punkInboxHookCommand(p, "claude-code", "context", "", "http://x", "")
	capture := punkHookCommand(p, "http://x", "")
	if isPunkManaged(wake, p) {
		t.Error("capture detector claimed wake command")
	}
	if isPunkManagedInbox(wake, p, "claude-code") {
		t.Error("inbox detector claimed wake command")
	}
	if isPunkManagedWake(inbox, p, "claude-code") {
		t.Error("wake detector claimed inbox command")
	}
	if isPunkManagedWake(capture, p, "claude-code") {
		t.Error("wake detector claimed capture command")
	}
	if !isPunkManaged(capture, p) || !isPunkManagedInbox(inbox, p, "claude-code") {
		t.Error("existing detectors lost their own commands")
	}
	// Group-level exclusivity too.
	wg := map[string]any{"hooks": []any{map[string]any{"type": "command", "command": wake}}}
	if isPunkManagedGroup(wg, p) || isPunkManagedInboxGroup(wg, p, "claude-code") {
		t.Error("wake group claimed by capture/inbox group detector")
	}
	if !isAnyPunkWakeGroup(wg) || isAnyPunkInboxGroup(wg) {
		t.Error("any-group detectors misclassified the wake group")
	}
}

func TestConnectClaudeCodeWakeAddsWakeGroups(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(p, []byte(wakeUserSettings), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := ConnectClaudeCodeWake(p, "/usr/local/bin/punk", "http://localhost:9090", "proj-ns")
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	m := readSettings(t, p)
	hooks := m["hooks"].(map[string]any)
	for _, ev := range claudeWakeEnsureEvents {
		gs := wakeGroups(t, hooks, ev, "claude-code")
		if len(gs) != 1 {
			t.Fatalf("%s: %d wake groups", ev, len(gs))
		}
		want := "/usr/local/bin/punk hook wake --client claude-code --action ensure --url http://localhost:9090 --ns proj-ns --messaging"
		if cmd := wakeGroupCommand(t, gs[0]); cmd != want {
			t.Fatalf("%s wake command %q", ev, cmd)
		}
		if _, has := gs[0]["matcher"]; has {
			t.Fatalf("%s: claude wake group must fire on every source: %v", ev, gs[0])
		}
		// Capture and inbox groups still present, separately.
		if len(inboxGroups(t, hooks, ev, "claude-code")) != 1 {
			t.Fatalf("%s lost the inbox group", ev)
		}
		raw, _ := json.Marshal(hooks[ev])
		if !strings.Contains(string(raw), `/usr/local/bin/punk hook --url http://localhost:9090 --ns proj-ns"`) {
			t.Fatalf("%s lost the capture group: %s", ev, raw)
		}
	}
	gs := wakeGroups(t, hooks, "SessionEnd", "claude-code")
	if len(gs) != 1 {
		t.Fatalf("SessionEnd: %d wake groups", len(gs))
	}
	wantStop := "/usr/local/bin/punk hook wake --client claude-code --action stop --url http://localhost:9090 --ns proj-ns --messaging"
	if cmd := wakeGroupCommand(t, gs[0]); cmd != wantStop {
		t.Fatalf("SessionEnd wake command %q", cmd)
	}
	// No wake group on PostToolUse; user entries and settings survive.
	if len(wakeGroups(t, hooks, "PostToolUse", "claude-code")) != 0 {
		t.Fatal("PostToolUse must not get a wake group")
	}
	raw, _ := os.ReadFile(p)
	for _, keep := range []string{"my-stop.sh", "echo hi && date", "my-session-end.sh"} {
		if !strings.Contains(string(raw), keep) {
			t.Fatalf("user entry %q lost: %s", keep, raw)
		}
	}
	if m["model"] != "opus" {
		t.Fatal("unrelated settings lost")
	}

	// Idempotent: rerun reports unchanged and rewrites nothing.
	before, _ := os.ReadFile(p)
	if changed, err := ConnectClaudeCodeWake(p, "/usr/local/bin/punk", "http://localhost:9090", "proj-ns"); err != nil || changed {
		t.Fatalf("rerun changed=%v err=%v", changed, err)
	}
	if after, _ := os.ReadFile(p); !bytes.Equal(before, after) {
		t.Fatal("rerun rewrote the file")
	}

	// A messaging (no-wake) reconnect over a wake install is a
	// byte-identical no-op and keeps the wake groups; so is a plain
	// capture-only reconnect.
	if changed, err := ConnectClaudeCodeMessaging(p, "/usr/local/bin/punk", "http://localhost:9090", "proj-ns"); err != nil || changed {
		t.Fatalf("messaging reconnect changed=%v err=%v", changed, err)
	}
	if after, _ := os.ReadFile(p); !bytes.Equal(before, after) {
		b, _ := os.ReadFile(p)
		t.Fatalf("messaging reconnect reordered:\n%s", b)
	}
	if changed, err := ConnectClaudeCodeNS(p, "/usr/local/bin/punk", "http://localhost:9090", "proj-ns"); err != nil || changed {
		t.Fatalf("plain reconnect changed=%v err=%v", changed, err)
	}
	if after, _ := os.ReadFile(p); !bytes.Equal(before, after) {
		t.Fatal("plain reconnect rewrote the file")
	}
	hooks = readSettings(t, p)["hooks"].(map[string]any)
	if len(wakeGroups(t, hooks, "Stop", "claude-code")) != 1 || len(wakeGroups(t, hooks, "SessionEnd", "claude-code")) != 1 {
		t.Fatal("no-wake reconnect deleted wake groups")
	}

	// URL/ns change replaces the wake group in place: still exactly
	// one, carrying the new values.
	if _, err := ConnectClaudeCodeWake(p, "/usr/local/bin/punk", "http://other:1", "ns2"); err != nil {
		t.Fatal(err)
	}
	hooks = readSettings(t, p)["hooks"].(map[string]any)
	for _, ev := range append(append([]string{}, claudeWakeEnsureEvents...), "SessionEnd") {
		gs := wakeGroups(t, hooks, ev, "claude-code")
		if len(gs) != 1 || !strings.Contains(fmt.Sprint(gs[0]), "http://other:1 --ns ns2") {
			t.Fatalf("%s: wake group not replaced in place: %v", ev, gs)
		}
		raw, _ := json.Marshal(hooks[ev])
		if strings.Count(string(raw), "hook wake") != 1 {
			t.Fatalf("%s: wake group duplicated: %s", ev, raw)
		}
	}
}

// A SessionEnd key holding a non-array is user config punk must not
// clobber; the wake install refuses and leaves the file untouched,
// exactly like the capture merge does for the canonical events.
func TestConnectClaudeCodeWakeRefusesNonArraySessionEnd(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	original := []byte(`{"hooks":{"SessionEnd":"oops"}}`)
	if err := os.WriteFile(p, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectClaudeCodeWake(p, "/usr/local/bin/punk", "http://localhost:9090", ""); err == nil {
		t.Fatal("expected refusal")
	} else if !strings.Contains(err.Error(), "hooks.SessionEnd") {
		t.Fatalf("error must name the key: %v", err)
	}
	after, _ := os.ReadFile(p)
	if !bytes.Equal(original, after) {
		t.Fatal("refused install touched the file")
	}
}

// Without --wake the output must carry no wake entries: the messaging
// and plain outputs are byte-identical to what they were before wake
// existed (the pre-M6 golden already pins the plain output).
func TestConnectWithoutWakeHasNoWakeGroups(t *testing.T) {
	for _, fn := range []func(string) (bool, error){
		func(p string) (bool, error) {
			return ConnectClaudeCodeNS(p, "/usr/local/bin/punk", "http://localhost:9090", "proj-ns")
		},
		func(p string) (bool, error) {
			return ConnectClaudeCodeMessaging(p, "/usr/local/bin/punk", "http://localhost:9090", "proj-ns")
		},
		func(p string) (bool, error) {
			return ConnectCodexHooks(p, "/usr/local/bin/punk", "http://localhost:9090", "proj-ns")
		},
		func(p string) (bool, error) {
			return ConnectCodexHooksMessaging(p, "/usr/local/bin/punk", "http://localhost:9090", "proj-ns")
		},
	} {
		p := filepath.Join(t.TempDir(), "s.json")
		if err := os.WriteFile(p, []byte(wakeUserSettings), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := fn(p); err != nil {
			t.Fatal(err)
		}
		raw, _ := os.ReadFile(p)
		if strings.Contains(string(raw), "hook wake") {
			t.Fatalf("no-wake output gained wake entries:\n%s", raw)
		}
		// The user's own SessionEnd entry is foreign config and stays.
		if !strings.Contains(string(raw), "my-session-end.sh") {
			t.Fatal("user SessionEnd entry lost")
		}
	}
}

func TestConnectCodexHooksWakeAddsWakeGroups(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hooks.json")
	if err := os.WriteFile(p, []byte(wakeUserSettings), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectCodexHooksWake(p, "/usr/local/bin/punk", "http://localhost:9090", ""); err != nil {
		t.Fatal(err)
	}
	hooks := readSettings(t, p)["hooks"].(map[string]any)
	for _, ev := range claudeWakeEnsureEvents {
		gs := wakeGroups(t, hooks, ev, "codex")
		if len(gs) != 1 {
			t.Fatalf("%s: %d codex wake groups", ev, len(gs))
		}
		want := "/usr/local/bin/punk hook wake --client codex --action ensure --url http://localhost:9090 --messaging"
		if cmd := wakeGroupCommand(t, gs[0]); cmd != want {
			t.Fatalf("%s: %q", ev, cmd)
		}
		if ev == "SessionStart" && gs[0]["matcher"] != codexSessionStartMatcher {
			t.Fatalf("codex SessionStart wake matcher %v", gs[0]["matcher"])
		}
		if ev != "SessionStart" && gs[0]["matcher"] != nil {
			t.Fatalf("%s must carry no matcher", ev)
		}
		// Claude-scoped wake detection must not claim codex entries.
		if len(wakeGroups(t, hooks, ev, "claude-code")) != 0 {
			t.Fatal("client-scoped wake detection leaked")
		}
	}
	gs := wakeGroups(t, hooks, "SessionEnd", "codex")
	if len(gs) != 1 || gs[0]["matcher"] != nil {
		t.Fatalf("SessionEnd wake group %v", gs)
	}
	before, _ := os.ReadFile(p)
	if changed, err := ConnectCodexHooksWake(p, "/usr/local/bin/punk", "http://localhost:9090", ""); err != nil || changed {
		t.Fatalf("rerun changed=%v err=%v", changed, err)
	}
	if after, _ := os.ReadFile(p); !bytes.Equal(before, after) {
		t.Fatal("rerun rewrote")
	}
	// Scope inspection counts only capture registrations: wake groups
	// are not capture destinations and must not confuse verify.
	sc := inspectCodexHookScope(p, "/usr/local/bin/punk")
	if sc.Err != nil || len(sc.Registrations) != 4 {
		t.Fatalf("scope inspection: %+v", sc)
	}
}

// A wake group identical in both scopes fires the wake hook twice per
// event for that repo; DedupeCodexHookScopes must remove the project
// copy, SessionEnd's stop group included.
func TestDedupeCodexHookScopesRemovesDuplicateWakeGroups(t *testing.T) {
	dir := t.TempDir()
	global := filepath.Join(dir, "global.json")
	project := filepath.Join(dir, "project.json")
	if _, err := ConnectCodexHooksWake(global, "/usr/local/bin/punk", "http://localhost:9090", ""); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(global)
	if err := os.WriteFile(project, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	notes, changed, err := DedupeCodexHookScopes(global, project, "/usr/local/bin/punk")
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	hooks := readSettings(t, project)["hooks"].(map[string]any)
	for _, ev := range append(append([]string{}, claudeWakeEnsureEvents...), "SessionEnd") {
		if len(wakeGroups(t, hooks, ev, "codex")) != 0 {
			t.Fatalf("%s: duplicate project wake group kept: %v", ev, hooks[ev])
		}
	}
	var sawSessionEnd bool
	for _, n := range notes {
		if strings.HasPrefix(n, "SessionEnd:") {
			sawSessionEnd = true
		}
	}
	if !sawSessionEnd {
		t.Fatalf("no SessionEnd dedupe note: %v", notes)
	}
	// The global file keeps its wake groups.
	gHooks := readSettings(t, global)["hooks"].(map[string]any)
	if len(wakeGroups(t, gHooks, "SessionEnd", "codex")) != 1 {
		t.Fatal("global wake group lost")
	}
}
