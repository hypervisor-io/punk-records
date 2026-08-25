package hookcli

import (
	"bytes"
	"fmt"
	"os"
)

// ConnectPi writes a pi coding-agent extension file at extensionPath (see
// piExtensionContent) that talks to a punk-records server directly over
// HTTP at runtime. Like ConnectOpenCode, there is no punk binary in the
// runtime path here: pi loads and executes the TypeScript/JavaScript
// extension file itself (pi's docs: extensions auto-discover from
// ".pi/extensions/*.ts" project-local or "~/.pi/agent/extensions/*.ts"
// global), so this function's only job is to place that file - the
// extension reads PUNK_URL/PUNK_API_KEY from its own process environment
// at runtime, not from anything ConnectPi writes beyond the baked-in
// serverURL fallback.
//
//   - A missing extensionPath is created fresh (parent directories
//     included - see writeAtomic's os.MkdirAll) with mode 0644.
//   - An existing file whose first line is NOT piExtensionMarker is
//     presumed hand-authored (a real user extension, or one installed by
//     a different tool) and refused with an error naming extensionPath -
//     the same "never silently discard user content" discipline as
//     ConnectOpenCode's openCodePluginMarker check and WriteCursorRules'
//     cursorRulesMarker check.
//   - An existing file WITH the marker (a prior punk version, or a stale
//     serverURL baked into the PUNK_URL fallback) is overwritten in
//     place.
//   - Idempotent: identical content on a re-run reports changed=false and
//     never touches the file.
//   - The write goes through writePreservingSymlinkAndMode (symlink-
//     resolved target, existing file's mode preserved, atomic temp file +
//     rename) - shared with ConnectClaudeCode/ConnectCursor/
//     ConnectOpenCode/WriteCursorRules, see connect.go's doc comment for
//     the exact guarantees.
func ConnectPi(extensionPath, serverURL string) (changed bool, err error) {
	content := piExtensionContent(serverURL)

	existing, readErr := os.ReadFile(extensionPath)
	switch {
	case readErr == nil:
		if !bytes.HasPrefix(existing, []byte(piExtensionMarker)) {
			return false, fmt.Errorf("%s exists and is not managed by punk (missing %q marker on its first line); refusing to overwrite", extensionPath, piExtensionMarker)
		}
	case os.IsNotExist(readErr):
		existing = nil
	default:
		return false, fmt.Errorf("read extension file %s: %w", extensionPath, readErr)
	}

	if existing != nil && string(existing) == content {
		return false, nil
	}

	if err := writePreservingSymlinkAndMode(extensionPath, []byte(content), 0o644); err != nil {
		return false, err
	}
	return true, nil
}
