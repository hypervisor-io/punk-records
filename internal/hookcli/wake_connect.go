package hookcli

import (
	"fmt"
	"strings"
)

// wake_connect.go holds the connect-side wiring for the opt-in native
// wake bridge (punk connect claude-code|codex --wake, which implies
// --messaging). It installs a THIRD kind of managed hook group beside
// the capture group (mergeEventGroups, isPunkManaged) and the inbox
// group (mergeInboxGroups, isPunkManagedInbox): a "punk hook wake"
// group that runs the wake lifecycle's ensure action on
// SessionStart/UserPromptSubmit/Stop and the stop action on SessionEnd.
// The three detectors are mutually exclusive by marker token (" hook"
// + boundary vs " hook inbox" vs " hook wake"), so one kind's merge
// never claims another kind's group. A no-wake reconnect preserves an
// existing wake opt-in and refreshes its URL; it never enables wake anew.
//
// The command deliberately ends with --messaging, like the inbox
// command (see inbox_wire.go): --wake implies --messaging, so delivery
// and wake share the one opt-in baked into the entry; PUNK_MESSAGING=0
// still disables and tears down at hook time. No token, socket path or
// other secret ever appears in the command: the wake hook resolves the
// native endpoint from the session's own environment at runtime.

// claudeWakeEnsureEvents are the Claude-shaped events whose wake group
// runs the ensure action (recheck runtime availability, ensure one
// listener for this session). SessionEnd is separate: its group runs
// the stop action.
var claudeWakeEnsureEvents = []string{"SessionStart", "UserPromptSubmit", "Stop"}

// wakeStopEvent is the Claude-shaped event whose wake group runs the
// stop action, tearing the session's listener down.
const wakeStopEvent = "SessionEnd"

// wakeHookTimeout is the wake group's handler timeout in seconds.
// Ensure exits promptly (it only rechecks availability and spawns or
// confirms a detached listener), so the inbox group's bound is reused.
const wakeHookTimeout = claudeInboxTimeout

// punkWakeHookCommand builds the shell command one managed wake hook
// entry invokes: "<punkPath> hook wake --client <client> --action
// <ensure|stop> --url <serverURL> [--ns <ns>] --messaging". Flag order
// mirrors punkInboxHookCommand so the two entries read alike in a
// settings file; action is the only wake-specific token.
func punkWakeHookCommand(punkPath, client, action, serverURL, ns string) string {
	cmd := fmt.Sprintf("%s hook wake --client %s --action %s", quotePunkPath(punkPath), client, action)
	cmd += " --url " + serverURL
	if ns != "" {
		cmd += " --ns " + ns
	}
	return cmd + " --messaging"
}

// isPunkManagedWake reports whether cmd is a punk-generated wake hook
// invocation for client. Its primary/fallback rule pair mirrors
// isPunkManagedInbox (inbox_wire.go), with " hook wake" in place of "
// hook inbox": the distinct marker tokens are what keep capture, inbox
// and wake entries from ever deduping each other in either direction.
// The --action/--url/--ns values are deliberately NOT pinned: a stale
// entry from an older punk version is still ours and gets replaced.
func isPunkManagedWake(cmd, punkPath, client string) bool {
	from := " --client " + client
	if idx := strings.Index(cmd, punkPath); idx >= 0 {
		rest := cmd[idx+len(punkPath):]
		rest = strings.TrimPrefix(rest, `"`)
		const tok = " hook wake"
		if strings.HasPrefix(rest, tok) {
			after := rest[len(tok):]
			if strings.HasPrefix(after, from) {
				tail := after[len(from):]
				if tail == "" {
					return true
				}
				switch tail[0] {
				case ' ', '\t':
					return true
				}
			}
		}
	}

	marker := "punk hook wake" + from
	idx := strings.Index(cmd, marker)
	if idx < 0 {
		return false
	}
	if idx != 0 {
		switch cmd[idx-1] {
		case '/', '\\', '"':
		default:
			return false
		}
	}
	tail := cmd[idx+len(marker):]
	if tail != "" {
		switch tail[0] {
		case ' ', '\t':
		default:
			return false
		}
	}
	return true
}

// isPunkManagedWakeGroup reports whether every handler in group is a
// punk wake command for client (see isPunkManagedWake). Same all-or-
// nothing discipline as isPunkManagedInboxGroup: a group mixing a wake
// command with anything else is left alone.
func isPunkManagedWakeGroup(group any, punkPath, client string) bool {
	m, ok := group.(map[string]any)
	if !ok {
		return false
	}
	hooks, ok := m["hooks"].([]any)
	if !ok || len(hooks) == 0 {
		return false
	}
	for _, h := range hooks {
		hm, ok := h.(map[string]any)
		if !ok {
			return false
		}
		cmd, _ := hm["command"].(string)
		if !isPunkManagedWake(cmd, punkPath, client) {
			return false
		}
	}
	return true
}

// isAnyPunkWakeGroup reports whether group holds only "punk hook wake"
// handlers (any path, any client). It is the wake counterpart of
// isAnyPunkInboxGroup (connect.go) and exists so the inbox merge can
// re-insert its fresh group AHEAD of any wake groups: without that,
// a reconnect would move the inbox group past the wake group and the
// file would churn on every run.
func isAnyPunkWakeGroup(group any) bool {
	m, ok := group.(map[string]any)
	if !ok {
		return false
	}
	hooks, ok := m["hooks"].([]any)
	if !ok || len(hooks) == 0 {
		return false
	}
	for _, h := range hooks {
		hm, ok := h.(map[string]any)
		if !ok {
			return false
		}
		cmd, _ := hm["command"].(string)
		if !strings.Contains(cmd, " hook wake --client ") {
			return false
		}
	}
	return true
}

// isWakeSubcommand reports whether after (the text following "<punk>
// hook") starts the "wake" subcommand as a whole word - the same shape
// of check isInboxSubcommand (connect.go) makes for "inbox", so the
// capture detector can refuse wake entries the way it refuses inbox
// entries.
func isWakeSubcommand(after string) bool {
	t := strings.TrimLeft(after, " \t")
	if len(t) == len(after) || !strings.HasPrefix(t, "wake") {
		return false
	}
	t = t[len("wake"):]
	return t == "" || t[0] == ' ' || t[0] == '\t'
}

// mergeWakeGroups returns one event's group list with any stale
// punk-managed wake group for client removed and a fresh one appended
// last. Capture groups, inbox groups and user groups are kept
// untouched, in order.
func mergeWakeGroups(raw any, punkPath, client, command string, matcher string) []any {
	var groups []any
	if arr, ok := raw.([]any); ok {
		for _, g := range arr {
			if isPunkManagedWakeGroup(g, punkPath, client) {
				continue
			}
			groups = append(groups, g)
		}
	}
	group := map[string]any{
		"hooks": []any{map[string]any{"type": "command", "command": command, "timeout": wakeHookTimeout}},
	}
	if matcher != "" {
		group["matcher"] = matcher
	}
	return append(groups, group)
}

// addClaudeShapedWake merges the wake groups for client into hooksAny
// (a Claude-shaped "hooks" object whose canonical event values were
// already validated as arrays or absent by the caller - SessionEnd is
// validated here, since the capture merge never touches it). An ensure
// group goes onto each of claudeWakeEnsureEvents (SessionStart carries
// sessionMatcher when non-empty, matching the inbox group's Codex
// matcher) and a stop group onto SessionEnd.
func addClaudeShapedWake(hooksAny map[string]any, hooksPath, punkPath, client, serverURL, ns, sessionMatcher string) error {
	for _, ev := range append(append([]string{}, claudeWakeEnsureEvents...), wakeStopEvent) {
		if raw, ok := hooksAny[ev]; ok && raw != nil {
			if _, isArr := raw.([]any); !isArr {
				return fmt.Errorf("hooks.%s is not an array; refusing to modify %s", ev, hooksPath)
			}
		}
	}
	for _, ev := range claudeWakeEnsureEvents {
		matcher := ""
		if ev == "SessionStart" {
			matcher = sessionMatcher
		}
		cmd := punkWakeHookCommand(punkPath, client, "ensure", serverURL, ns)
		hooksAny[ev] = mergeWakeGroups(hooksAny[ev], punkPath, client, cmd, matcher)
	}
	cmd := punkWakeHookCommand(punkPath, client, "stop", serverURL, ns)
	hooksAny[wakeStopEvent] = mergeWakeGroups(hooksAny[wakeStopEvent], punkPath, client, cmd, "")
	return nil
}
