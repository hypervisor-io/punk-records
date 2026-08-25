package hookcli

import (
	"fmt"
	"strings"
)

// antigravityHookName is the top-level hooks.json key ConnectAntigravity
// owns outright. Unlike Claude Code's settings.json or Cursor's hooks.json
// (where every hook source shares the SAME per-event array and punk must
// dedup its own entries out of a mix of foreign ones - see connect.go's
// mergeEventGroups/isPunkManagedGroup and connect_cursor.go's
// mergeCursorEntries/isPunkManagedCursorEntry), Antigravity's hooks.json
// namespaces every hook definition under its own top-level "hook name" key
// (antigravity.google/docs/hooks: "The hooks.json file maps hook names to
// their event configurations" - the docs' own example has three sibling
// top-level keys, "my-linter-hook", "safety-gate", "reminder", each with
// its own independent event lists). That means punk can own ONE top-level
// key outright and never touch any of a user's own hook-name entries at
// all - a coarser, simpler ownership boundary than Claude/Cursor's
// per-command-string dedup. Inside punk's own "punk" key, the SAME
// per-command-string dedup discipline is still applied one level deeper
// (see mergeAntigravityGroupEntries/mergeAntigravityFlatEntries below), in
// case a user ever hand-edits inside it.
const antigravityHookName = "punk"

// antigravityGroupEvents lists the Antigravity hooks.json events that use
// the matcher+hooks GROUP shape - {"matcher":"...","hooks":[{"type":
// "command","command":...,"timeout":...}]} - byte-for-byte the same group
// shape Claude Code's own settings.json hooks use (connect.go's
// mergeEventGroups). Per antigravity.google/docs/hooks' documented
// "my-linter-hook"/"safety-gate" examples, PreToolUse and PostToolUse are
// the only two events using this shape.
//
// Punk only wires PostToolUse here. PreToolUse is deliberately never
// wired: it is documented as gating tool execution (stdout contract:
// `decision` is Required - "allow", "deny", "ask", or "force_ask", with no
// documented safe default for an absent/malformed reply, unlike Stop's
// explicit "any other value allows the stop" fallback - see
// RunFromAntigravity's own doc comment in hookcli.go). Punk's job is
// passive memory capture, not permission gating; wiring PreToolUse would
// mean a slow or dead punk-records server could stall or outright block a
// user's real tool calls, which is unacceptable for a capture-only
// feature. Claude Code's own hookEvents list (connect.go) never wires an
// equivalent pre-execution gate either, for the same reason.
var antigravityGroupEvents = []string{"PostToolUse"}

// antigravityFlatEvents lists the events using the FLAT handler-list shape
// - array elements are {"type":"command","command":...,"timeout":...}
// handler objects directly, no matcher/hooks wrapper - per the docs'
// "reminder" example (`"PreInvocation": [{"type":"command","command":
// "./scripts/reminder.sh"}]`) and the "Note" that "For PreInvocation,
// PostInvocation, and Stop, the structure is simpler ... and the matcher
// is ignored".
//
// Punk wires PreInvocation (doubles as session-start capture AND
// context-injection, gated to the conversation's first model call - see
// translateAntigravity in normalize.go and RunFromAntigravity in
// hookcli.go) and Stop (terminal capture, with its own required-decision
// reply - see RunFromAntigravity). PostInvocation is deliberately left
// unwired: antigravity.google/docs/hooks documents its input schema as
// IDENTICAL to PreInvocation's (invocationNum, initialNumSteps, plus the
// common fields) with no error/result/output field of its own, so it
// offers no capturable signal PreInvocation doesn't already give punk one
// invocation earlier in the same loop - wiring it would only double the
// hook-call overhead per model invocation for zero new information.
var antigravityFlatEvents = []string{"PreInvocation", "Stop"}

// punkAntigravityHookCommand builds the command string one of punk's
// managed Antigravity hook entries invokes: "<punkPath> hook --from
// antigravity --event <event> --url <serverURL>". The --event flag exists
// because none of Antigravity's documented hook payloads carry a field
// identifying which event fired (verified exhaustively against
// antigravity.google/docs/hooks - see translateAntigravity's doc comment
// in normalize.go for the full "why"); baking a distinct --event value
// into each wired command is what lets a single generic "punk hook"
// invocation know which event it is handling. quotePunkPath (connect.go)
// is shared with every other agent's command-building helper so
// isPunkManagedAntigravity's optional-quote tolerance on a later run
// applies identically.
func punkAntigravityHookCommand(punkPath, event, serverURL string) string {
	return fmt.Sprintf("%s hook --from antigravity --event %s --url %s", quotePunkPath(punkPath), event, serverURL)
}

// isPunkManagedAntigravity reports whether cmd is a punk-generated
// Antigravity hook invocation. Its primary/fallback rule pair mirrors
// isPunkManagedCursor's structure (connect_cursor.go) line for line,
// adapted for the Antigravity command shape via the "--from antigravity"
// marker in place of "--from cursor". Unlike isPunkManagedCursor, the
// marker check does NOT also pin the trailing "--event <name>" value -
// ownership here is "is this command one of punk's Antigravity hook
// invocations at all", not "which specific event"; a stale command whose
// --event value doesn't match what punkAntigravityHookCommand would
// generate today (e.g. from an older punk version that wired different
// events) is still correctly recognized and replaced, the same way
// isPunkManagedCursor doesn't pin --url.
func isPunkManagedAntigravity(cmd, punkPath string) bool {
	if idx := strings.Index(cmd, punkPath); idx >= 0 {
		rest := cmd[idx+len(punkPath):]
		rest = strings.TrimPrefix(rest, `"`)
		const tok = " hook"
		if strings.HasPrefix(rest, tok) {
			after := rest[len(tok):]
			const from = " --from antigravity"
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

	const marker = "punk hook --from antigravity"
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

// mergeAntigravityGroupEntries returns one GROUP-shape event's entry list
// (see antigravityGroupEvents) with any stale punk-managed group(s)
// removed and a fresh one carrying command appended last. Mirrors
// mergeEventGroups (connect.go) exactly, but dedups via
// isPunkManagedAntigravity/isPunkManagedAntigravityGroup instead of the
// agent-agnostic isPunkManaged/isPunkManagedGroup, and always sets
// matcher "*" (all tools) - PostToolUse is the only group-shape event punk
// wires, and it wants every tool's completion observed, the same "*"
// convention Claude Code's own PostToolUse group already uses.
func mergeAntigravityGroupEntries(raw any, punkPath, command string) []any {
	var groups []any
	if arr, ok := raw.([]any); ok {
		for _, g := range arr {
			if isPunkManagedAntigravityGroup(g, punkPath) {
				continue
			}
			groups = append(groups, g)
		}
	}
	punkGroup := map[string]any{
		"matcher": "*",
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": command,
				"timeout": 10,
			},
		},
	}
	return append(groups, punkGroup)
}

// isPunkManagedAntigravityGroup mirrors isPunkManagedGroup (connect.go):
// a group is only treated as punk's own, and therefore eligible for
// silent replacement, when EVERY command in its nested "hooks" list is
// punk-managed per isPunkManagedAntigravity.
func isPunkManagedAntigravityGroup(group any, punkPath string) bool {
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
		if !isPunkManagedAntigravity(cmd, punkPath) {
			return false
		}
	}
	return true
}

// mergeAntigravityFlatEntries returns one FLAT-shape event's entry list
// (see antigravityFlatEvents) with any stale punk-managed entry removed
// and a fresh {"type":"command","command":...,"timeout":10} handler object
// appended last. Mirrors mergeCursorEntries (connect_cursor.go), but each
// entry carries "type"/"timeout" fields too (Cursor's flat entries are
// just {"command":"..."}) - Antigravity's own Hook Handler Configuration
// table documents type/timeout as valid on every handler object regardless
// of which event it's under.
func mergeAntigravityFlatEntries(raw any, punkPath, command string) []any {
	var entries []any
	if arr, ok := raw.([]any); ok {
		for _, e := range arr {
			if isPunkManagedAntigravityFlatEntry(e, punkPath) {
				continue
			}
			entries = append(entries, e)
		}
	}
	return append(entries, map[string]any{
		"type":    "command",
		"command": command,
		"timeout": 10,
	})
}

// isPunkManagedAntigravityFlatEntry mirrors isPunkManagedCursorEntry
// (connect_cursor.go): only a well-formed object whose "command" field is
// punk-managed per isPunkManagedAntigravity counts. A bare string, number,
// null, or an object with a non-string/absent "command" is left alone
// rather than causing a panic or being silently dropped.
func isPunkManagedAntigravityFlatEntry(entry any, punkPath string) bool {
	m, ok := entry.(map[string]any)
	if !ok {
		return false
	}
	cmd, ok := m["command"].(string)
	if !ok || cmd == "" {
		return false
	}
	return isPunkManagedAntigravity(cmd, punkPath)
}

// ConnectAntigravity merges punk hook entries into an Antigravity CLI
// hooks.json, preserving every other top-level hook-name key untouched
// (see antigravityHookName's doc comment for why that's a safe, coarse
// ownership boundary here, unlike Claude Code/Cursor). It mirrors
// ConnectCursor's guarantees (see connect_cursor.go's own doc comment)
// applied to Antigravity's shape:
//
//   - hooksPath is read and parsed whole via loadSettings (shared with
//     ConnectClaudeCode/ConnectCursor): a `null` body normalizes to an
//     empty map, a missing file is treated as absent (not an error), and
//     numbers decode with json.Number so they round-trip byte-exact.
//   - The top-level "punk" key, or one of its three managed event keys
//     (PostToolUse, PreInvocation, Stop), holding a JSON value of the
//     wrong shape (a string instead of an object, an object instead of an
//     array) is refused with an error naming the offending key; the file
//     is left completely untouched. Every OTHER top-level hook-name key
//     (a user's own "my-linter-hook", "safety-gate", ...) is never even
//     inspected, let alone shape-checked.
//   - Idempotent: an existing punk-managed entry for one of the three
//     managed events (see isPunkManagedAntigravity) is replaced in place,
//     never duplicated; foreign entries within "punk"'s own event arrays
//     (including hostile non-object array elements) are kept as-is. A
//     re-run whose resulting bytes are identical to what's on disk reports
//     changed=false without writing.
//   - The write goes through writePreservingSymlinkAndMode (symlink-
//     resolved target, existing file's mode preserved, atomic temp file +
//     rename) - shared with every other Connect* writer in this package,
//     see connect.go's doc comment for the exact guarantees.
func ConnectAntigravity(hooksPath, punkPath, serverURL string) (changed bool, err error) {
	settings, existing, err := loadSettings(hooksPath)
	if err != nil {
		return false, err
	}

	var punkEntry map[string]any
	if raw, ok := settings[antigravityHookName]; ok && raw != nil {
		punkEntry, ok = raw.(map[string]any)
		if !ok {
			return false, fmt.Errorf("%q is not an object; refusing to modify %s", antigravityHookName, hooksPath)
		}
	} else {
		punkEntry = map[string]any{}
	}

	for _, ev := range antigravityGroupEvents {
		if raw, ok := punkEntry[ev]; ok && raw != nil {
			if _, isArr := raw.([]any); !isArr {
				return false, fmt.Errorf("%s.%s is not an array; refusing to modify %s", antigravityHookName, ev, hooksPath)
			}
		}
		command := punkAntigravityHookCommand(punkPath, ev, serverURL)
		punkEntry[ev] = mergeAntigravityGroupEntries(punkEntry[ev], punkPath, command)
	}
	for _, ev := range antigravityFlatEvents {
		if raw, ok := punkEntry[ev]; ok && raw != nil {
			if _, isArr := raw.([]any); !isArr {
				return false, fmt.Errorf("%s.%s is not an array; refusing to modify %s", antigravityHookName, ev, hooksPath)
			}
		}
		command := punkAntigravityHookCommand(punkPath, ev, serverURL)
		punkEntry[ev] = mergeAntigravityFlatEntries(punkEntry[ev], punkPath, command)
	}
	settings[antigravityHookName] = punkEntry

	out, err := encodeSettings(settings)
	if err != nil {
		return false, fmt.Errorf("encode hooks: %w", err)
	}

	// Same byte-compare no-op skip ConnectClaudeCode/ConnectCursor rely on:
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
