package hookcli

import (
	"fmt"
	"os"
	"strings"
)

// cursorHookEvents lists the six Cursor native hook events T1.1's
// translator (normalize.go's translateCursor) maps to a Claude-shaped
// envelope: sessionStart, beforeSubmitPrompt, postToolUse, afterFileEdit,
// stop, sessionEnd. ConnectCursor wires a punk entry into each of these
// event arrays; any other Cursor event (workspaceOpen, preToolUse, ...)
// has no server-side mapping yet and is left completely alone.
var cursorHookEvents = []string{"sessionStart", "beforeSubmitPrompt", "postToolUse", "afterFileEdit", "stop", "sessionEnd"}

// ConnectCursor merges punk hook entries into a Cursor hooks.json,
// preserving user entries and any events punk does not manage. It mirrors
// ConnectClaudeCode's guarantees (see connect.go's doc comment) applied to
// Cursor's shape: {"version":1,"hooks":{"<event>":[{"command":"..."}]}}
// (cursor.com/docs/agent/hooks) rather than Claude Code's nested
// matcher/hooks-array groups. Concretely:
//
//   - hooksPath is read and parsed whole via loadSettings (shared with
//     ConnectClaudeCode): a `null` body normalizes to an empty map, a
//     missing file is treated as absent (not an error), and numbers decode
//     with json.Number so they round-trip byte-exact.
//   - "hooks" or one of the six mapped events holding a JSON value of the
//     wrong shape (a string instead of an object, an object instead of an
//     array) is refused with an error naming the offending key; the file
//     is left completely untouched.
//   - An existing "version" value is preserved; a freshly created file
//     gets "version":1.
//   - Idempotent: an existing punk-managed entry for an event (see
//     isPunkManagedCursor) is replaced in place, never duplicated; foreign
//     entries (including non-object hostile array elements) are kept
//     as-is. A re-run whose resulting bytes are identical to what's on
//     disk reports changed=false without writing.
//   - The write goes through writePreservingSymlinkAndMode (symlink-
//     resolved target, existing file's mode preserved, atomic temp file +
//     rename) - shared with ConnectClaudeCode and WriteCursorRules, see
//     connect.go's doc comment for the exact guarantees.
func ConnectCursor(hooksPath, punkPath, serverURL string) (changed bool, err error) {
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

	command := punkCursorHookCommand(punkPath, serverURL)
	for _, ev := range cursorHookEvents {
		if raw, ok := hooksAny[ev]; ok && raw != nil {
			if _, isArr := raw.([]any); !isArr {
				return false, fmt.Errorf("hooks.%s is not an array; refusing to modify %s", ev, hooksPath)
			}
		}
		hooksAny[ev] = mergeCursorEntries(hooksAny[ev], punkPath, command)
	}
	settings["hooks"] = hooksAny

	if _, ok := settings["version"]; !ok {
		settings["version"] = 1
	}

	out, err := encodeSettings(settings)
	if err != nil {
		return false, fmt.Errorf("encode hooks: %w", err)
	}

	// Same byte-compare no-op skip ConnectClaudeCode relies on: map keys
	// marshal in sorted order, so an unchanged document re-encodes
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

// punkCursorHookCommand builds the command string punk's managed Cursor
// hook entry invokes: "<punkPath> hook --from cursor --url <serverURL>",
// sharing quotePunkPath's whitespace-quoting with punkHookCommand (see
// connect.go) so isPunkManagedCursor's optional-quote tolerance on a later
// run applies identically.
func punkCursorHookCommand(punkPath, serverURL string) string {
	return fmt.Sprintf("%s hook --from cursor --url %s", quotePunkPath(punkPath), serverURL)
}

// mergeCursorEntries returns one event's entry list with any stale
// punk-managed entry (see isPunkManagedCursor) removed and a fresh one
// carrying command appended last. Non-punk entries, including hostile
// array elements that are not JSON objects at all, are kept untouched and
// in their original order - isPunkManagedCursorEntry only ever returns
// true for a well-formed {"command": "..."} entry that is ours.
func mergeCursorEntries(raw any, punkPath, command string) []any {
	var entries []any
	if arr, ok := raw.([]any); ok {
		for _, e := range arr {
			if isPunkManagedCursorEntry(e, punkPath) {
				continue
			}
			entries = append(entries, e)
		}
	}
	return append(entries, map[string]any{"command": command})
}

// isPunkManagedCursorEntry reports whether entry is a punk-managed Cursor
// hook entry: a JSON object whose "command" field is punk-managed per
// isPunkManagedCursor. Anything else (a bare string, number, null, or an
// object with a non-string/absent "command") is never treated as ours,
// so a hostile or merely differently-shaped array element is left alone
// rather than causing a panic or being silently dropped.
func isPunkManagedCursorEntry(entry any, punkPath string) bool {
	m, ok := entry.(map[string]any)
	if !ok {
		return false
	}
	cmd, ok := m["command"].(string)
	if !ok || cmd == "" {
		return false
	}
	return isPunkManagedCursor(cmd, punkPath)
}

// isPunkManagedCursor reports whether cmd is a punk-generated Cursor hook
// invocation. Its primary/fallback rule pair mirrors isPunkManaged's
// structure (connect.go) line for line, adapted for the Cursor command
// shape, with one addition: the literal "--from cursor" marker.
//
// What actually keeps ConnectClaudeCode and ConnectCursor from stepping on
// each other's entries is that they write to two entirely separate files
// (settings.json vs hooks.json) - a Cursor-managed command string never
// appears inside settings.json's "hooks" tree at all, and vice versa, so
// in normal operation neither isPunkManaged* function ever even sees the
// other's command string. On the Claude Code side specifically, there is a
// second, independent backstop: isPunkManagedGroup only treats a
// settings.json array element as punk-managed if it has Claude's nested
// {"hooks":[{"type":...,"command":...}]} group shape - a flat Cursor
// {"command":"..."} entry (or a bare Cursor command string hand-pasted
// into the wrong file) does not have that shape and is left alone as a
// foreign entry, regardless of what its command string contains.
//
// The "--from cursor" marker required here is NOT a symmetric,
// isPunkManaged-provided guarantee, though: isPunkManaged itself has no
// awareness of "--from cursor" and would in fact match a Cursor-managed
// command string (it only checks for punkPath/marker followed by a
// " hook" word boundary, which "<punkPath> hook --from cursor --url ..."
// satisfies) if one ever ended up inside a Claude-shaped hooks array
// alongside the right group shape. That combination does not happen in
// practice given the separate-files/group-shape backstops above, but this
// function's own "--from cursor" requirement is what stops the reverse
// case - a plain Claude Code command string (no --from flag) hand-pasted
// into hooks.json - from being misattributed to Cursor by this function.
// It is a one-sided check, not a mirrored invariant the two functions
// jointly enforce.
//
// Primary rule: cmd contains punkPath (optionally followed by a closing
// double-quote, matching punkCursorHookCommand's quoting), then the
// literal token " hook" with a word boundary, then immediately
// " --from cursor" with its own word boundary (end of string or
// whitespace) right after - the exact shape punkCursorHookCommand
// generates, quoted path or not.
//
// Fallback rule (deliberate false-positive tradeoff, mirroring
// isPunkManaged's binary-relocation fallback): if the primary rule
// doesn't match because punkPath itself changed, cmd is still treated as
// ours when it contains the literal substring "punk hook --from cursor"
// immediately preceded by the start of the command, a path separator, or
// a double-quote, and immediately followed by end-of-string or
// whitespace. This intentionally claims ANY "punk hook --from cursor"
// entry regardless of stale path, so relocating the punk binary replaces
// the old entry with the new one instead of leaving two Cursor-managed
// entries (one permanently pointing at a dead path) side by side forever.
// The primary/fallback rules described above live in
// isPunkManagedFromAgent (connect.go), shared with every other --from
// agent's own detector; this function is the "cursor" binding of it.
func isPunkManagedCursor(cmd, punkPath string) bool {
	return isPunkManagedFromAgent(cmd, punkPath, "cursor")
}

// cursorRulesMarker identifies a .cursor/rules/punk-memory.mdc file as
// owned by punk. WriteCursorRules refuses to overwrite an existing file
// that lacks this marker (a user's own hand-authored rules file), the
// same "never silently discard user config" discipline ConnectClaudeCode
// and ConnectCursor apply to hooks.json.
const cursorRulesMarker = "<!-- managed by punk connect cursor -->"

// cursorRulesContent renders the full MDC rules file content punk writes:
// frontmatter with alwaysApply:true and a description (MDC files are only
// applied by Cursor when alwaysApply is set, or their glob/description
// match - alwaysApply:true is the unconditional case, appropriate for a
// standing "load your memory" instruction), the managed marker, and a body
// telling the agent to call the punk-records MCP tool recall (or search)
// for this project's namespace at session start, and remember to persist
// durable findings.
func cursorRulesContent(serverURL string) string {
	return fmt.Sprintf(`---
alwaysApply: true
description: Load and persist project memory via the punk-records MCP server
---
%s

# Punk memory

At the start of every session, call the punk-records MCP tool `+"`recall`"+` (or `+"`search`"+` if `+"`recall`"+` is unavailable) for this project's namespace to load prior context before doing anything else.

When you learn something durable during the session (a decision, a fix, a convention, a gotcha), call the punk-records MCP tool `+"`remember`"+` to persist it so future sessions can find it.

Punk-records server: %s
`, cursorRulesMarker, serverURL)
}

// WriteCursorRules writes .cursor/rules/punk-memory.mdc, the MDC rules
// file that instructs a Cursor agent to load and persist project memory
// via punk's MCP tools. Unlike ConnectCursor's hooks.json (structured
// JSON merged key-by-key), this file is plain text punk owns outright:
// content is either punk's current rendering or nothing at all, so there
// is no per-field merge - only a marker-gated overwrite.
//
//   - A missing file is created fresh with mode 0644.
//   - An existing file WITHOUT cursorRulesMarker is refused with an error
//     naming rulesPath: it is presumed hand-authored, and silently
//     replacing it would destroy the user's own content.
//   - An existing file WITH the marker (a prior punk version, or a stale
//     serverURL) is overwritten in place, mode and symlink target
//     preserved exactly like ConnectClaudeCode/ConnectCursor (see
//     connect.go's writePreservingSymlinkAndMode).
//   - Idempotent: identical content on a re-run reports changed=false and
//     never touches the file.
func WriteCursorRules(rulesPath, serverURL string) (changed bool, err error) {
	content := cursorRulesContent(serverURL)

	existing, readErr := os.ReadFile(rulesPath)
	switch {
	case readErr == nil:
		if !strings.Contains(string(existing), cursorRulesMarker) {
			return false, fmt.Errorf("%s exists and is not managed by punk (missing %q marker); refusing to overwrite", rulesPath, cursorRulesMarker)
		}
	case os.IsNotExist(readErr):
		existing = nil
	default:
		return false, fmt.Errorf("read rules file %s: %w", rulesPath, readErr)
	}

	if existing != nil && string(existing) == content {
		return false, nil
	}

	if err := writePreservingSymlinkAndMode(rulesPath, []byte(content), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// CursorRulesText returns the exact rules content WriteCursorRules would
// write to a project's .cursor/rules/punk-memory.mdc, without writing
// anything. cmdConnectCursor's non---project path uses this: Cursor has no
// native support for a user-level ~/.cursor/rules directory (rules either
// live in Cursor's Settings -> Rules UI or a project's own .cursor/rules/,
// per cursor.com/docs and the Cursor forum), so writing a global rules
// file there would silently never be read by Cursor. Printing the same
// content for the user to paste into Settings -> Rules (or to rerun with
// --project inside a repo, which does write the file) is the load-bearing
// action instead.
func CursorRulesText(serverURL string) string {
	return cursorRulesContent(serverURL)
}
