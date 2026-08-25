package hookcli

import (
	"fmt"
)

// copilotHookEvents lists the five GitHub Copilot CLI hook events
// ConnectCopilot wires a punk entry into: SessionStart, UserPromptSubmit,
// PostToolUse, Stop, SessionEnd - the PascalCase spelling of each, which is
// what selects the snake_case "VS Code compatible" payload shape
// translateCopilot (normalize.go) decodes (docs.github.com/en/copilot/
// reference/hooks-reference: "VS Code compatible format - Configure the
// event name in PascalCase ... Fields use snake_case to match the VS Code
// Copilot extension format"). PreToolUse, permissionRequest, preCompact,
// notification, subagentStart/Stop, and postToolUseFailure are all
// documented events too but are deliberately never wired: PreToolUse/
// permissionRequest gate tool execution (punk's job is passive memory
// capture, not permission gating - the same reasoning Antigravity's
// PreToolUse omission documents, connect_antigravity.go), and the rest have
// no Claude Code hook equivalent in this package's existing four-event
// capture set (SessionStart/UserPromptSubmit/PostToolUse/Stop, connect.go's
// hookEvents) for translateCopilot to map into.
var copilotHookEvents = []string{"SessionStart", "UserPromptSubmit", "PostToolUse", "Stop", "SessionEnd"}

// ConnectCopilot merges punk hook entries into a GitHub Copilot CLI
// hooks.json, preserving user entries and any events punk does not manage.
// It mirrors ConnectCursor's guarantees (see connect_cursor.go's own doc
// comment) applied to Copilot's shape:
// {"version":1,"hooks":{"<PascalEvent>":[{"type":"command","command":...,
// "timeoutSec":...}]}} (docs.github.com/en/copilot/reference/
// hooks-reference: "Hook configuration format" and each event's own flat
// array example - unlike Claude Code/Antigravity's PostToolUse, Copilot has
// no matcher+hooks GROUP wrapper for any event; "matcher" is an optional
// sibling field directly on the handler object, and is simply omitted here
// - the docs' own words for that field: "Omit matcher to receive results
// from all tools").
//
//   - hooksPath is read and parsed whole via loadSettings (shared with
//     ConnectClaudeCode/ConnectCursor/ConnectAntigravity): a `null` body
//     normalizes to an empty map, a missing file is treated as absent (not
//     an error), and numbers decode with json.Number so they round-trip
//     byte-exact.
//   - "hooks" or one of the five mapped events holding a JSON value of the
//     wrong shape (a string instead of an object, an object instead of an
//     array) is refused with an error naming the offending key; the file
//     is left completely untouched.
//   - An existing "version" value is preserved; a freshly created file
//     gets "version":1.
//   - Idempotent: an existing punk-managed entry for an event (see
//     isPunkManagedCopilot) is replaced in place, never duplicated; foreign
//     entries (including hostile non-object array elements) are kept as-is.
//     A re-run whose resulting bytes are identical to what's on disk
//     reports changed=false without writing.
//   - The write goes through writePreservingSymlinkAndMode (symlink-
//     resolved target, existing file's mode preserved, atomic temp file +
//     rename) - shared with every other Connect* writer in this package,
//     see connect.go's doc comment for the exact guarantees.
//
// hooksPath is punk's OWN dedicated hook file (cmdConnectCopilot picks the
// name), not a single shared config Copilot itself expects by a fixed
// name: the docs document that Copilot CLI loads and combines EVERY
// *.json file under a hooks directory ("Hooks are loaded from the
// following sources ... and combined. When the same event appears in
// multiple sources, all hook entries from all sources are run."), so a
// punk-owned file sitting alongside any number of a user's own hook files
// in the same directory never needs to merge INTO them - only its own
// content ever needs the merge/dedup discipline above, applied here purely
// as defense-in-depth (a user hand-editing punk's own file) rather than
// because Copilot requires one shared file the way Claude Code/Cursor/
// Antigravity do.
func ConnectCopilot(hooksPath, punkPath, serverURL string) (changed bool, err error) {
	settings, existing, err := loadSettings(hooksPath)
	if err != nil {
		return false, err
	}

	var hooksAny map[string]any
	if raw, ok := settings["hooks"]; ok && raw != nil {
		hooksAny, ok = raw.(map[string]any)
		if !ok {
			return false, fmt.Errorf("hooks is not an object; refusing to modify %s", hooksPath)
		}
	} else {
		hooksAny = map[string]any{}
	}

	command := punkCopilotHookCommand(punkPath, serverURL)
	for _, ev := range copilotHookEvents {
		if raw, ok := hooksAny[ev]; ok && raw != nil {
			if _, isArr := raw.([]any); !isArr {
				return false, fmt.Errorf("hooks.%s is not an array; refusing to modify %s", ev, hooksPath)
			}
		}
		hooksAny[ev] = mergeCopilotEntries(hooksAny[ev], punkPath, command)
	}
	settings["hooks"] = hooksAny

	if _, ok := settings["version"]; !ok {
		settings["version"] = 1
	}

	out, err := encodeSettings(settings)
	if err != nil {
		return false, fmt.Errorf("encode hooks: %w", err)
	}

	// Same byte-compare no-op skip every other Connect* writer relies on:
	// map keys marshal in sorted order, so an unchanged document re-encodes
	// identically and a no-op reconnect can report changed=false without
	// touching the file's mtime.
	if existing != nil && string(out) == string(existing) {
		return false, nil
	}

	if err := writePreservingSymlinkAndMode(hooksPath, out, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// punkCopilotHookCommand builds the command string punk's managed Copilot
// hook entry invokes: "<punkPath> hook --from copilot --url <serverURL>",
// sharing quotePunkPath's whitespace-quoting with every other agent's
// command-building helper (connect.go) so isPunkManagedCopilot's
// optional-quote tolerance on a later run applies identically.
func punkCopilotHookCommand(punkPath, serverURL string) string {
	return fmt.Sprintf("%s hook --from copilot --url %s", quotePunkPath(punkPath), serverURL)
}

// mergeCopilotEntries returns one event's entry list with any stale
// punk-managed entry (see isPunkManagedCopilotEntry) removed and a fresh
// one carrying command appended last. Non-punk entries, including hostile
// array elements that are not JSON objects at all, are kept untouched and
// in their original order. timeoutSec (not Claude/Cursor/Antigravity's
// "timeout") is Copilot's own canonical field name for this - see the
// docs' Command hooks field table: "timeoutSec number No Timeout in
// seconds. Default: 30." ("timeout" is documented as an alias, honored
// only when timeoutSec is absent).
func mergeCopilotEntries(raw any, punkPath, command string) []any {
	var entries []any
	if arr, ok := raw.([]any); ok {
		for _, e := range arr {
			if isPunkManagedCopilotEntry(e, punkPath) {
				continue
			}
			entries = append(entries, e)
		}
	}
	return append(entries, map[string]any{
		"type":       "command",
		"command":    command,
		"timeoutSec": 10,
	})
}

// isPunkManagedCopilotEntry reports whether entry is a punk-managed
// Copilot hook entry: a JSON object whose "command" field is punk-managed
// per isPunkManagedCopilot. Anything else (a bare string, number, null, or
// an object with a non-string/absent "command" - e.g. a foreign entry
// using "bash"/"powershell" instead of the cross-platform "command" field)
// is never treated as ours, so a hostile or merely differently-shaped
// array element is left alone rather than causing a panic or being
// silently dropped.
func isPunkManagedCopilotEntry(entry any, punkPath string) bool {
	m, ok := entry.(map[string]any)
	if !ok {
		return false
	}
	cmd, ok := m["command"].(string)
	if !ok || cmd == "" {
		return false
	}
	return isPunkManagedCopilot(cmd, punkPath)
}

// isPunkManagedCopilot reports whether cmd is a punk-generated Copilot hook
// invocation. Its primary/fallback rule pair mirrors isPunkManagedCursor's
// structure (connect_cursor.go) line for line, adapted for the Copilot
// command shape via the "--from copilot" marker in place of "--from
// cursor". See isPunkManagedCursor's own doc comment for the full
// rationale behind the primary/fallback split and the one-sided-check
// caveat; it applies here unchanged.
// Like isPunkManagedCursor, this is a thin binding of the shared
// isPunkManagedFromAgent (connect.go) to this agent's --from name.
func isPunkManagedCopilot(cmd, punkPath string) bool {
	return isPunkManagedFromAgent(cmd, punkPath, "copilot")
}
