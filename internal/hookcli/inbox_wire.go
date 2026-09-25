package hookcli

import (
	"fmt"
	"strings"
)

// inbox_wire.go holds the connect-side helpers every client's
// --messaging wiring shares: the command string a managed inbox hook
// entry invokes, the managed-entry detector a reconnect dedups with,
// and the flat-entry merge for the clients whose hook arrays are flat
// (Cursor, Copilot, Antigravity's flat events, Hermes). M6's Claude
// Code/Codex group-shape events reuse the detector and command builder
// with their own group merge (connect.go's mergeEventGroups shape).
//
// The command deliberately ends with --messaging: punk connect
// --messaging writes the opt-in INTO the hook entry (plan shared
// design), so delivery does not depend on PUNK_MESSAGING=1 being in the
// client's environment (PUNK_MESSAGING=0 still kills it - see
// inboxEnabled in inbox.go).

// punkInboxHookCommand builds the shell command one managed inbox hook
// entry invokes: "<punkPath> hook inbox --client <client> --mode <mode>
// [--event <event>] --url <serverURL> [--ns <ns>] --messaging". event is
// non-empty only for clients whose native payload does not name the
// fired event (Antigravity - same reason cmdHook carries --event there,
// see connect_antigravity.go). quotePunkPath (connect.go) is shared with
// every other command builder so isPunkManagedInbox's optional-quote
// tolerance applies identically on a later run.
func punkInboxHookCommand(punkPath, client, mode, event, serverURL, ns string) string {
	cmd := fmt.Sprintf("%s hook inbox --client %s --mode %s", quotePunkPath(punkPath), client, mode)
	if event != "" {
		cmd += " --event " + event
	}
	cmd += " --url " + serverURL
	if ns != "" {
		cmd += " --ns " + ns
	}
	return cmd + " --messaging"
}

// isPunkManagedInbox reports whether cmd is a punk-generated inbox hook
// invocation for client. Its primary/fallback rule pair mirrors
// isPunkManagedFromAgent (connect.go), with " hook inbox" + " --client
// <client>" in place of " hook" + " --from <agent>": a capture entry
// ("punk hook --from cursor ...") and an inbox entry ("punk hook inbox
// --client cursor ...") for the same client must never dedup each other,
// which the distinct marker tokens guarantee in both directions (" hook
// inbox" does not satisfy isPunkManagedFromAgent's " hook" + "--from"
// sequence, and " hook --from" does not satisfy the " hook inbox" +
// "--client" sequence here). The --mode/--event/--url/--ns values are
// deliberately NOT pinned: a stale entry from an older punk version is
// still ours and gets replaced.
func isPunkManagedInbox(cmd, punkPath, client string) bool {
	from := " --client " + client
	if idx := strings.Index(cmd, punkPath); idx >= 0 {
		rest := cmd[idx+len(punkPath):]
		rest = strings.TrimPrefix(rest, `"`)
		const tok = " hook inbox"
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

	marker := "punk hook inbox" + from
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

// mergeInboxFlatEntries returns one flat-shape event's entry list with
// any stale punk-managed INBOX entry (see isPunkManagedInbox) removed and
// entry appended last. Non-punk entries, capture entries ("punk hook
// --from ...", managed by the client's own isPunkManaged* detector), and
// hostile non-object elements are kept untouched and in their original
// order. Used by Cursor's {"command":...} arrays, Copilot's
// {"type","command","timeoutSec"} arrays and Antigravity's flat
// {"type","command","timeout"} events alike - only the entry constructor
// differs per client.
func mergeInboxFlatEntries(raw any, punkPath, client string, entry map[string]any) []any {
	var entries []any
	if arr, ok := raw.([]any); ok {
		for _, e := range arr {
			if m, ok := e.(map[string]any); ok {
				if cmd, ok := m["command"].(string); ok && isPunkManagedInbox(cmd, punkPath, client) {
					continue
				}
			}
			entries = append(entries, e)
		}
	}
	return append(entries, entry)
}
