package hookcli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// hookEvents lists the Claude Code hook events ConnectClaudeCode wires a
// punk entry into. PostToolUse additionally gets a "*" matcher (see
// mergeEventGroups); the others fire unconditionally.
var hookEvents = []string{"SessionStart", "UserPromptSubmit", "PostToolUse", "Stop"}

// ConnectClaudeCode merges punk hook entries into a Claude Code
// settings.json, preserving user entries. Idempotent: prior punk-managed
// entries (command invokes punkPath's "hook" subcommand - see
// isPunkManaged) are replaced in place, never duplicated, and a rerun
// with unchanged inputs reports changed=false without writing.
//
// settingsPath is read and parsed whole; on any read/parse failure past
// "file does not exist" (a missing file is treated as an empty {}
// document, not an error), settingsPath is left completely untouched -
// this is a live user config file and a failed merge attempt must never
// clobber it. The same refusal applies if "hooks" or one of its event
// keys is present but holds a JSON value of the wrong shape (e.g. a
// string instead of an object, or an object instead of an array):
// silently discarding it and replacing it with punk's own shape would
// destroy user config, so ConnectClaudeCode returns an error naming the
// offending key and leaves the file untouched instead.
//
// The write itself goes to a temp file beside the resolved target
// (settingsPath itself, or the file a settingsPath symlink points at -
// see writePreservingSymlinkAndMode below) followed by os.Rename, so a
// crash mid-write cannot leave a half-written settings.json (atomic on POSIX
// in the sense that a reader never observes a torn write; this does not
// fsync, so on some filesystems a power loss between the rename and the
// next fsync of the directory entry could still expose an empty or
// missing file - not exercised by tests, which can't easily observe a
// torn write, but implemented and relied on for the "no torn write"
// guarantee).
func ConnectClaudeCode(settingsPath, punkPath, serverURL string) (changed bool, err error) {
	return ConnectClaudeCodeNS(settingsPath, punkPath, serverURL, "")
}

// ConnectClaudeCodeNS is ConnectClaudeCode with a namespace override
// baked into the generated hook commands (from punk connect --project).
func ConnectClaudeCodeNS(settingsPath, punkPath, serverURL, ns string) (changed bool, err error) {
	settings, existing, err := loadSettings(settingsPath)
	if err != nil {
		return false, err
	}

	var hooksAny map[string]any
	if raw, ok := settings["hooks"]; ok && raw != nil {
		hooksAny, ok = raw.(map[string]any)
		if !ok {
			return false, fmt.Errorf("hooks is not an object; refusing to modify %s", settingsPath)
		}
	} else {
		hooksAny = map[string]any{}
	}

	command := punkHookCommand(punkPath, serverURL, ns)
	for _, ev := range hookEvents {
		if raw, ok := hooksAny[ev]; ok && raw != nil {
			if _, isArr := raw.([]any); !isArr {
				return false, fmt.Errorf("hooks.%s is not an array; refusing to modify %s", ev, settingsPath)
			}
		}
		hooksAny[ev] = mergeEventGroups(hooksAny[ev], ev, punkPath, command)
	}
	settings["hooks"] = hooksAny

	out, err := encodeSettings(settings)
	if err != nil {
		return false, fmt.Errorf("encode settings: %w", err)
	}

	// Go's encoding/json marshals map[string]any keys in sorted order, so
	// re-marshaling unchanged content is byte-for-byte deterministic:
	// comparing against the bytes we just read lets a no-op reconnect
	// skip the write (and report changed=false) instead of touching the
	// file's mtime for nothing.
	if existing != nil && string(out) == string(existing) {
		return false, nil
	}

	// writePreservingSymlinkAndMode resolves settingsPath through any
	// symlink (e.g. into a dotfiles repo) and writes through the real
	// target rather than replacing the symlink itself, and reuses the
	// existing file's mode (a settings.json a user locked down to 0600,
	// since it can hold secrets: env vars, apiKeyHelper, isn't silently
	// widened to 0644 - only a freshly created file gets 0644) - see its
	// doc comment below for the full guarantee, shared with ConnectCursor
	// and WriteCursorRules.
	if err := writePreservingSymlinkAndMode(settingsPath, out, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// loadSettings reads and parses settingsPath. A missing file yields an
// empty settings map and nil existing bytes (not an error) so
// ConnectClaudeCode can create settings.json fresh. Any other read
// failure or a parse failure returns a nil map and nil bytes alongside
// the error - callers must treat that as "do not write", since existing
// being nil in the error path is never mistaken for "file absent" by
// ConnectClaudeCode (it returns before reaching the write).
//
// Numbers are decoded with json.Number (via Decoder.UseNumber) rather
// than float64, so integers beyond float64's exact range and literals
// like "1.0" survive a round-trip byte-for-byte instead of being
// silently reformatted ("1.0" -> "1") or losing precision.
//
// A body of the JSON literal `null` is valid JSON and decodes without
// error, but leaves the destination map nil (the same as an absent
// key would) - that nil map is normalized to an empty map here so
// callers never have to special-case it; using it as-is would panic
// ("assignment to entry in nil map") the moment ConnectClaudeCode wrote
// a "hooks" key into it.
func loadSettings(path string) (settings map[string]any, existing []byte, err error) {
	raw, readErr := os.ReadFile(path)
	if errors.Is(readErr, os.ErrNotExist) {
		return map[string]any{}, nil, nil
	}
	if readErr != nil {
		return nil, nil, fmt.Errorf("read settings %s: %w", path, readErr)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return map[string]any{}, raw, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, nil, fmt.Errorf("parse settings %s: %w", path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, raw, nil
}

// encodeSettings marshals settings the way ConnectClaudeCode wants it
// persisted: 2-space indent, keys sorted (encoding/json's map ordering),
// trailing newline, and HTML escaping disabled. By default the standard
// encoder rewrites <, >, and & to their \u003c-style unicode escapes;
// SetEscapeHTML(false) keeps user command strings readable (e.g. inside
// a hook command like "echo x && date").
func encodeSettings(settings map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(settings); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// quotePunkPath double-quotes punkPath when it contains whitespace, so the
// shell doesn't split it into multiple words; isPunkManaged/
// isPunkManagedCursor both tolerate that optional quoting when recognizing
// the command on a later run. Shared by punkHookCommand and
// punkCursorHookCommand - the only difference between the two commands is
// the subcommand/flags that follow the (possibly quoted) path.
func quotePunkPath(punkPath string) string {
	if strings.ContainsAny(punkPath, " \t") {
		return `"` + punkPath + `"`
	}
	return punkPath
}

// punkHookCommand builds the shell command punk's managed hook group
// invokes.
func punkHookCommand(punkPath, serverURL, ns string) string {
	cmd := fmt.Sprintf("%s hook --url %s", quotePunkPath(punkPath), serverURL)
	if ns != "" {
		cmd += " --ns " + ns
	}
	return cmd
}

// mergeEventGroups returns the hook-group list for one event with any
// stale punk-managed group(s) removed and a fresh one carrying command
// appended. Non-punk groups (including a group that merely mentions
// punkPath under an unrelated subcommand, see isPunkManaged) are kept
// untouched and in their original order; the punk group is always last.
func mergeEventGroups(raw any, event, punkPath, command string) []any {
	var groups []any
	if arr, ok := raw.([]any); ok {
		for _, g := range arr {
			if isPunkManagedGroup(g, punkPath) {
				continue
			}
			groups = append(groups, g)
		}
	}

	punkGroup := map[string]any{
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": command,
				"timeout": 10,
			},
		},
	}
	if event == "PostToolUse" {
		punkGroup["matcher"] = "*"
	}
	return append(groups, punkGroup)
}

// isPunkManagedGroup reports whether every hook command in group's
// "hooks" list is punk-managed (see isPunkManaged). A group is only
// ever treated as ours, and therefore eligible for silent
// replacement/removal, when ALL of its commands are ours - a group
// that mixes a punk command with an unrelated one is left alone.
func isPunkManagedGroup(group any, punkPath string) bool {
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
		if !isPunkManaged(cmd, punkPath) {
			return false
		}
	}
	return true
}

// isPunkManaged reports whether cmd is a punk-generated hook invocation.
//
// Primary rule: cmd contains punkPath (optionally immediately followed
// by a closing double-quote, see punkHookCommand's quoting of paths with
// whitespace) and then the literal token " hook" with a word boundary
// after it (end of string or whitespace). A plain
// strings.Contains(cmd, punkPath) && strings.Contains(cmd, " hook") check
// is NOT precise enough: a user's own tool invoked as
// "/path/to/punk hook-inspector --verbose" contains both substrings
// (" hook" matches the start of "hook-inspector") and would be
// misdetected as punk-managed, silently deleting an unrelated user hook
// on the next connect. Requiring a boundary right after "hook" rejects
// that case while still matching our own generated
// "<punkPath> hook --url <serverURL>" command (quoted or not).
//
// Fallback rule (deliberate false-positive tradeoff): if the primary
// rule doesn't match - because punkPath itself changed, e.g. the punk
// binary was relocated from /old/bin/punk to /new/bin/punk - cmd is
// still treated as punk-managed when it contains the literal substring
// "punk hook --url " immediately preceded by either the start of the
// command, a path separator ('/' or '\'), or a double-quote (an
// invocation of a binary literally named "punk", from any directory,
// quoted or not, calling our "hook --url" subcommand+flag). This
// intentionally treats ANY "punk hook --url" entry as ours to manage,
// even one from a stale path, so that relocating the punk binary
// replaces the old group with the new one instead of leaving two
// punk-managed groups (one permanently failing) side by side forever.
// The fallback's marker phrase is specific enough ("hook --url ", not
// just "hook") that it does not defeat the primary rule's precision: the
// hook-inspector example above never contains "punk hook --url ", so it
// is never caught by either rule.
func isPunkManaged(cmd, punkPath string) bool {
	if idx := strings.Index(cmd, punkPath); idx >= 0 {
		rest := cmd[idx+len(punkPath):]
		rest = strings.TrimPrefix(rest, `"`)
		const tok = " hook"
		if strings.HasPrefix(rest, tok) {
			after := rest[len(tok):]
			if after == "" {
				return true
			}
			switch after[0] {
			case ' ', '\t':
				return true
			}
		}
	}

	const marker = "punk hook --url "
	idx := strings.Index(cmd, marker)
	if idx < 0 {
		return false
	}
	if idx == 0 {
		return true
	}
	switch cmd[idx-1] {
	case '/', '\\', '"':
		return true
	default:
		return false
	}
}

// isPunkManagedFromAgent reports whether cmd is a punk-generated hook
// invocation for the agent named by agent (the value of punk hook's own
// --from flag: "cursor", "copilot", "hermes", ...). It is the shared
// implementation behind isPunkManagedCursor (connect_cursor.go),
// isPunkManagedCopilot (connect_copilot.go) and isPunkManagedHermes
// (connect_hermes.go), which had been three byte-identical copies differing
// only in that agent name - extracted here at the point a third agent
// needed it, per .claude/rules/ai.md's "extract a shared helper when any of
// them next changes" rule. isPunkManaged (above) deliberately stays
// separate: Claude Code's own command carries no --from flag at all, so it
// has a genuinely different primary rule and a different fallback marker,
// not just a different agent name.
//
// Primary rule: cmd contains punkPath (optionally followed by a closing
// double-quote, matching quotePunkPath's quoting of paths with whitespace),
// then the literal token " hook", then immediately " --from <agent>" with a
// word boundary (end of string or whitespace) right after - the exact shape
// every punk*HookCommand generates, quoted path or not.
//
// Fallback rule (deliberate false-positive tradeoff, mirroring
// isPunkManaged's own binary-relocation fallback): if the primary rule
// doesn't match because punkPath itself changed, cmd is still treated as
// ours when it contains the literal substring "punk hook --from <agent>"
// immediately preceded by the start of the command, a path separator, or a
// double-quote, and immediately followed by end-of-string or whitespace.
// This intentionally claims ANY such entry regardless of a stale path, so
// relocating the punk binary replaces the old entry instead of leaving two
// managed entries (one permanently pointing at a dead path) side by side
// forever.
//
// Requiring the agent name is what keeps a plain Claude Code command string
// (no --from flag) hand-pasted into another agent's config from being
// misattributed here. It is a one-sided check, not a mirrored invariant:
// isPunkManaged has no awareness of --from and would match any of these
// commands if one ever landed inside a Claude-shaped hooks group. See
// isPunkManagedCursor's doc comment for why that combination does not arise
// in practice.
func isPunkManagedFromAgent(cmd, punkPath, agent string) bool {
	from := " --from " + agent
	if idx := strings.Index(cmd, punkPath); idx >= 0 {
		rest := cmd[idx+len(punkPath):]
		rest = strings.TrimPrefix(rest, `"`)
		const tok = " hook"
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

	marker := "punk hook" + from
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

// writePreservingSymlinkAndMode is the shared write tail for every
// punk-owned user file (ConnectClaudeCode's settings.json, ConnectCursor's
// hooks.json, WriteCursorRules' punk-memory.mdc): resolve path through any
// symlink and write through the resolved target - not onto the symlink
// path itself, which would replace the symlink with a plain file and
// silently detach a dotfiles repo from the real config (a missing path or
// a non-symlink both fail/no-op EvalSymlinks harmlessly and fall back to
// path as-is) - then reuse the existing file's permission mode (only a
// brand-new file gets defaultMode) so a file a user locked down (e.g.
// 0600, since settings.json can hold secrets: env vars, apiKeyHelper)
// isn't silently widened. The actual write goes through writeAtomic (temp
// file + rename) so a crash never leaves a torn file.
func writePreservingSymlinkAndMode(path string, data []byte, defaultMode os.FileMode) error {
	writePath := path
	if resolved, evalErr := filepath.EvalSymlinks(path); evalErr == nil {
		writePath = resolved
	}
	mode := defaultMode
	if fi, statErr := os.Stat(path); statErr == nil {
		mode = fi.Mode().Perm()
	}
	return writeAtomic(writePath, data, mode)
}

// writeAtomic writes data to path via a temp file in the same directory
// followed by os.Rename, so a crash or concurrent reader never observes a
// partially written file. The temp file is created with CreateTemp's
// default 0600 mode and then chmod'd to mode (the caller passes the
// existing file's mode, or a default for a brand-new file); it is removed
// on any failure before the rename. Error strings and the temp-file
// pattern name are deliberately generic ("file", not "settings"): this
// helper backs settings.json, hooks.json, and punk-memory.mdc alike (via
// writePreservingSymlinkAndMode above), so a failure writing the Cursor
// rules file must never be misreported as a "settings" failure.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create dir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".punk-config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename temp file into place: %w", err)
	}
	ok = true
	return nil
}
