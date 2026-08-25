package hookcli

import (
	"bytes"
	"fmt"
	"os"
)

// ConnectOpenCode writes an OpenCode plugin file at pluginPath (see
// openCodePluginContent) that talks to a punk-records server directly over
// HTTP at runtime. Unlike ConnectClaudeCode/ConnectCursor, there is no punk
// binary in the runtime path here: OpenCode loads and executes the JS/TS
// plugin file itself (opencode.ai/docs/plugins/, "Use a plugin > From
// local files"), so this function's only job is to place that file - the
// plugin reads PUNK_URL/PUNK_API_KEY from its own process environment at
// runtime, not from anything ConnectOpenCode writes beyond the baked-in
// serverURL fallback.
//
//   - A missing pluginPath is created fresh (parent directories included -
//     see writeAtomic's os.MkdirAll) with mode 0644.
//   - An existing file whose first line is NOT openCodePluginMarker is
//     presumed hand-authored (a real user plugin, or one installed by a
//     different tool) and refused with an error naming pluginPath - the
//     same "never silently discard user content" discipline as
//     WriteCursorRules' cursorRulesMarker check (connect_cursor.go).
//   - An existing file WITH the marker (a prior punk version, or a stale
//     serverURL baked into the PUNK_URL fallback) is overwritten in place.
//   - Idempotent: identical content on a re-run reports changed=false and
//     never touches the file.
//   - The write goes through writePreservingSymlinkAndMode (symlink-
//     resolved target, existing file's mode preserved, atomic temp file +
//     rename) - shared with ConnectClaudeCode/ConnectCursor/
//     WriteCursorRules, see connect.go's doc comment for the exact
//     guarantees.
func ConnectOpenCode(pluginPath, serverURL string) (changed bool, err error) {
	content := openCodePluginContent(serverURL)

	existing, readErr := os.ReadFile(pluginPath)
	switch {
	case readErr == nil:
		if !bytes.HasPrefix(existing, []byte(openCodePluginMarker)) {
			return false, fmt.Errorf("%s exists and is not managed by punk (missing %q marker on its first line); refusing to overwrite", pluginPath, openCodePluginMarker)
		}
	case os.IsNotExist(readErr):
		existing = nil
	default:
		return false, fmt.Errorf("read plugin file %s: %w", pluginPath, readErr)
	}

	if existing != nil && string(existing) == content {
		return false, nil
	}

	if err := writePreservingSymlinkAndMode(pluginPath, []byte(content), 0o644); err != nil {
		return false, err
	}
	return true, nil
}
