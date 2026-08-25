package hookcli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// WriteOpenClawPlugin writes punk's OpenClaw plugin into pluginDir: the
// entry file (index.js) plus the package.json that points OpenClaw at it.
// changed reports whether anything on disk actually differs afterwards, so
// a no-op reconnect can say so instead of touching mtimes.
//
// Both files are refused rather than overwritten when they exist and are
// not punk's:
//
//   - index.js must carry openClawPluginMarker as its first line. A file
//     without it is a hand-authored plugin (or another tool's), and
//     silently replacing it would destroy user work.
//   - package.json must parse and declare "name" equal to OpenClawPluginID.
//     A package.json naming a different plugin means pluginDir belongs to
//     someone else even if index.js happens to be absent.
//
// Each write goes through writePreservingSymlinkAndMode (symlink resolved,
// existing mode preserved, atomic temp file + rename), shared with every
// other file this package writes.
func WriteOpenClawPlugin(pluginDir, serverURL string) (changed bool, err error) {
	entryPath := filepath.Join(pluginDir, "index.js")
	pkgPath := filepath.Join(pluginDir, "package.json")

	existingEntry, err := readIfExists(entryPath)
	if err != nil {
		return false, err
	}
	if existingEntry != nil && !hasOpenClawMarker(string(existingEntry)) {
		return false, fmt.Errorf("%s exists and is not managed by punk (missing %q marker); refusing to overwrite it",
			entryPath, openClawPluginMarker)
	}

	existingPkg, err := readIfExists(pkgPath)
	if err != nil {
		return false, err
	}
	if existingPkg != nil {
		var pkg struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(existingPkg, &pkg); err != nil {
			return false, fmt.Errorf("parse %s: %w; refusing to overwrite it", pkgPath, err)
		}
		if pkg.Name != OpenClawPluginID {
			return false, fmt.Errorf("%s declares plugin %q, not %q; refusing to overwrite it",
				pkgPath, pkg.Name, OpenClawPluginID)
		}
	}

	entry := []byte(openClawPluginSource(serverURL))
	pkgBody := []byte(openClawPackageJSON())

	if existingEntry == nil || string(existingEntry) != string(entry) {
		if err := writePreservingSymlinkAndMode(entryPath, entry, 0o644); err != nil {
			return false, err
		}
		changed = true
	}
	if existingPkg == nil || string(existingPkg) != string(pkgBody) {
		if err := writePreservingSymlinkAndMode(pkgPath, pkgBody, 0o644); err != nil {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}

// readIfExists returns path's bytes, or a nil slice and nil error when the
// file does not exist. Any other read failure is returned as an error -
// callers must treat that as "do not write", never as "absent".
func readIfExists(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return raw, nil
}

// ConnectOpenClaw enables punk's plugin in an OpenClaw config.json,
// preserving every other key. It writes exactly four things, all under the
// plugin's own entry except the first:
//
//	plugins.enabled                                       = true
//	plugins.entries.punk-memory.enabled                   = true
//	plugins.entries.punk-memory.hooks.allowConversationAccess = true
//	plugins.entries.punk-memory.hooks.allowPromptInjection    = true
//
// The two hook flags are not optional extras: docs.openclaw.ai/plugins/hooks
// documents allowConversationAccess as the gate on a non-bundled plugin
// reading raw conversation content from hooks including before_prompt_build,
// and allowPromptInjection (when false) as blocking prompt-mutating hooks,
// before_prompt_build named among them. Without both, punk's plugin loads
// but its capture and injection hook does nothing. Writing them is
// therefore part of connecting, not a silent privilege grab - and
// cmdConnectOpenClaw prints each one it set.
//
// Guarantees mirror ConnectClaudeCode's (see connect.go): the file is read
// and parsed whole via loadSettings (missing file treated as empty, `null`
// body normalized, numbers decoded as json.Number so unrelated values
// round-trip byte-exact); any key on the path punk writes that holds a
// wrong-shaped value is refused with an error naming it and the file is
// left completely untouched; unchanged content reports changed=false
// without writing; the write itself is symlink-preserving, mode-preserving
// and atomic.
//
// A config.json that is really JSON5/JSONC (OpenClaw's docs show a
// json5-flavored example) fails the parse above, so punk refuses and
// prints the snippet to add by hand rather than reformatting a file it
// cannot faithfully re-encode.
func ConnectOpenClaw(configPath string) (changed bool, err error) {
	settings, existing, err := loadSettings(configPath)
	if err != nil {
		return false, err
	}

	plugins, err := childObject(settings, "plugins", "plugins", configPath)
	if err != nil {
		return false, err
	}
	entries, err := childObject(plugins, "entries", "plugins.entries", configPath)
	if err != nil {
		return false, err
	}
	entry, err := childObject(entries, OpenClawPluginID, "plugins.entries."+OpenClawPluginID, configPath)
	if err != nil {
		return false, err
	}
	hooks, err := childObject(entry, "hooks", "plugins.entries."+OpenClawPluginID+".hooks", configPath)
	if err != nil {
		return false, err
	}

	plugins["enabled"] = true
	entry["enabled"] = true
	hooks["allowConversationAccess"] = true
	hooks["allowPromptInjection"] = true

	out, err := encodeSettings(settings)
	if err != nil {
		return false, fmt.Errorf("encode config: %w", err)
	}
	if existing != nil && string(out) == string(existing) {
		return false, nil
	}
	if err := writePreservingSymlinkAndMode(configPath, out, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// childObject returns the JSON object stored at key inside parent, creating
// and inserting an empty one when the key is absent or null. A present
// value of any other type is an error naming the key with its full dotted
// path (so "plugins.entries is not an object" points at the right place in
// a nested document), leaving the caller to abandon the write - silently
// replacing user config with punk's own shape is exactly what every writer
// in this package refuses to do.
func childObject(parent map[string]any, key, path, file string) (map[string]any, error) {
	if raw, ok := parent[key]; ok && raw != nil {
		obj, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s is not an object; refusing to modify %s", path, file)
		}
		return obj, nil
	}
	created := map[string]any{}
	parent[key] = created
	return created, nil
}
