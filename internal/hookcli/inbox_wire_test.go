package hookcli

import (
	"strings"
	"testing"
)

// The inbox command carries the opt-in IN the entry (--messaging), per
// the plan's shared design, so delivery never depends on the client's
// environment.
func TestPunkInboxHookCommandShape(t *testing.T) {
	got := punkInboxHookCommand("/usr/local/bin/punk", "cursor", "continue", "", "http://localhost:9090", "proj")
	want := "/usr/local/bin/punk hook inbox --client cursor --mode continue --url http://localhost:9090 --ns proj --messaging"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	got = punkInboxHookCommand("/usr/local/bin/punk", "antigravity", "context", "PreInvocation", "http://localhost:9090", "")
	want = "/usr/local/bin/punk hook inbox --client antigravity --mode context --event PreInvocation --url http://localhost:9090 --messaging"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	// A path with whitespace is double-quoted, same as every other
	// punk-managed command (quotePunkPath).
	got = punkInboxHookCommand("/opt/my tools/punk", "hermes", "context", "", "http://localhost:9090", "")
	want = `"/opt/my tools/punk" hook inbox --client hermes --mode context --url http://localhost:9090 --messaging`
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// The detector must match exactly punk's own inbox entries and nothing
// else: not the capture entries (same client, " hook --from"), not a
// different client's inbox entry, not a hand-rolled hook-inspector
// command that merely contains " hook inbox" as a prefix of a longer
// token.
func TestIsPunkManagedInboxPrecision(t *testing.T) {
	p := "/usr/local/bin/punk"
	own := punkInboxHookCommand(p, "cursor", "context", "", "http://localhost:9090", "")
	if !isPunkManagedInbox(own, p, "cursor") {
		t.Fatal("own command not detected")
	}
	if !isPunkManagedInbox(punkInboxHookCommand(p, "cursor", "continue", "", "http://other:1", "x"), p, "cursor") {
		t.Fatal("a stale mode/url/ns is still ours to replace")
	}
	// Quoted path (whitespace quoting) is tolerated.
	if !isPunkManagedInbox(`"/opt/my tools/punk" hook inbox --client cursor --mode context --url http://x --messaging`, "/opt/my tools/punk", "cursor") {
		t.Fatal("quoted path not detected")
	}
	// Binary relocated: the fallback marker claims any "punk hook inbox
	// --client cursor" invocation regardless of the stale path.
	if !isPunkManagedInbox("/old/bin/punk hook inbox --client cursor --mode context --url http://x --messaging", p, "cursor") {
		t.Fatal("relocated-binary fallback not detected")
	}

	for _, foreign := range []string{
		// The same client's CAPTURE entry: managed by
		// isPunkManagedCursor, never by the inbox detector.
		p + " hook --from cursor --url http://localhost:9090",
		// A different client's inbox entry.
		p + " hook inbox --client copilot --mode context --url http://x --messaging",
		// "--client" present but as a prefix of a longer token.
		p + " hook inbox --client cursorx --mode context --url http://x",
		// " hook inbox" as a prefix of a longer token.
		p + " hook inbox-inspector --client cursor",
		// Unrelated user tooling.
		"/usr/bin/python3 .cursor/hooks/audit.sh",
		"",
	} {
		if isPunkManagedInbox(foreign, p, "cursor") {
			t.Fatalf("foreign command detected as ours: %q", foreign)
		}
	}
	// And the reverse direction: the capture detector must not claim an
	// inbox entry (isPunkManagedCursor requires " hook" + " --from").
	if isPunkManagedCursor(own, p) {
		t.Fatal("capture detector claimed an inbox entry")
	}
}

// The merge removes only stale inbox entries and keeps everything else,
// including hostile non-object elements, in order.
func TestMergeInboxFlatEntriesKeepsForeignAndCapture(t *testing.T) {
	p := "/usr/local/bin/punk"
	stale := map[string]any{"command": punkInboxHookCommand(p, "cursor", "context", "", "http://old", "")}
	capture := map[string]any{"command": p + " hook --from cursor --url http://localhost:9090"}
	user := map[string]any{"command": "./hooks/audit.sh"}
	raw := []any{stale, capture, user, "not-an-object"}
	got := mergeInboxFlatEntries(raw, p, "cursor", map[string]any{"command": punkInboxHookCommand(p, "cursor", "context", "", "http://localhost:9090", "")})
	if len(got) != 4 {
		t.Fatalf("expected 4 entries (stale replaced, capture/user/hostile kept), got %d: %v", len(got), got)
	}
	cmds := make([]string, 0, 4)
	for _, e := range got {
		if m, ok := e.(map[string]any); ok {
			cmds = append(cmds, m["command"].(string))
		} else {
			cmds = append(cmds, "<non-object>")
		}
	}
	if cmds[0] != capture["command"] || cmds[1] != user["command"] || cmds[2] != "<non-object>" {
		t.Fatalf("order/content changed: %v", cmds)
	}
	if !strings.Contains(cmds[3], "hook inbox --client cursor --mode context --url http://localhost:9090") {
		t.Fatalf("fresh inbox entry not appended last: %q", cmds[3])
	}
}
