package hookcli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"gopkg.in/yaml.v3"
)

// Hermes' plugin system: the in-process hook surface punk installs a plugin
// into so it also runs in the Desktop backend. Verified against
// hermes-agent's source (8301654), hermes_cli/plugins.py:
//
//   - User plugins live at ``$HERMES_HOME/plugins/<name>/``
//     (``_discover_and_load_inner``). Each directory holds a ``plugin.yaml``
//     manifest and an ``__init__.py`` with a ``register(ctx)`` function
//     (module docstring + ``_load_plugin``).
//   - A flat plugin's registry key is its manifest ``name`` (falling back to
//     the directory basename), and the loader only loads a user plugin whose
//     key or name appears in ``plugins.enabled`` (``_get_enabled_plugins``
//     plus the ``is_enabled`` gate in ``_discover_and_load_inner``). Opt-in
//     is the default, so connect must add the key explicitly.
//
// HermesPluginsDir resolves the plugins root the same way Hermes'
// get_hermes_home() does, sharing HermesAllowlistPath's resolution rules.

// HermesPluginsDir returns the directory Hermes scans for user plugins:
// $HERMES_HOME/plugins when HERMES_HOME is set, else
// %LOCALAPPDATA%\hermes\plugins on Windows and ~/.hermes/plugins everywhere
// else. An unresolvable home falls back to a relative .hermes path, the same
// degradation HermesAllowlistPath applies.
func HermesPluginsDir() string {
	if h := os.Getenv("HERMES_HOME"); h != "" {
		return filepath.Join(h, "plugins")
	}
	if runtime.GOOS == "windows" {
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			return filepath.Join(local, "hermes", "plugins")
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".hermes", "plugins")
	}
	return filepath.Join(home, ".hermes", "plugins")
}

// ConnectHermesPluginEnable adds HermesPluginID to the “plugins.enabled“
// allow-list in a Hermes config.yaml, so Hermes' loader loads punk's plugin.
// It gives the same guarantees ConnectHermes gives, applied to this key:
//
//   - configPath is read and parsed whole via loadHermesConfig (missing file
//     treated as empty, a non-mapping root or a multi-document file refused),
//     so a failed merge never clobbers a live user config.
//   - A wrong-shaped value on the path punk writes (“plugins“ not a
//     mapping, “plugins.enabled“ not a sequence, an alias node at either
//     position) is refused with an error naming the key, leaving the file
//     untouched, rather than being replaced with punk's own shape.
//   - Idempotent: when the key is already present (matched by scalar value)
//     the document is left byte-identical and changed=false; every other
//     entry, including non-string elements and foreign plugin names, is kept
//     as-is and in order, with punk's name appended only when absent.
//   - The write goes through writePreservingSymlinkAndMode (symlink
//     resolved, existing mode preserved, atomic temp file + rename).
//
// Like ConnectHermes, the merge round-trips the whole document through
// yaml.v3's Node API, so the FIRST run against a hand-formatted file may
// report changed=true even when it is semantically a no-op; subsequent runs
// re-encode byte-identically and report changed=false.
func ConnectHermesPluginEnable(configPath string) (changed bool, err error) {
	doc, existing, err := loadHermesConfig(configPath)
	if err != nil {
		return false, err
	}
	root := doc.Content[0]

	plugins, err := hermesChildMapping(root, "plugins", configPath)
	if err != nil {
		return false, err
	}
	enabled, err := hermesChildSequence(plugins, "enabled", configPath)
	if err != nil {
		return false, err
	}
	enabled.Content = mergeHermesEnabledPlugins(enabled.Content)

	out, err := encodeHermesConfig(doc)
	if err != nil {
		return false, fmt.Errorf("encode config: %w", err)
	}
	if existing != nil && bytes.Equal(out, existing) {
		return false, nil
	}
	if err := writePreservingSymlinkAndMode(configPath, out, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// mergeHermesEnabledPlugins returns a plugins.enabled list with
// HermesPluginID appended when it is not already present. Matching is by
// scalar value, so a differently-shaped element (a mapping, a null, a
// nested sequence) is never mistaken for punk's name and is preserved
// untouched and in its original order.
func mergeHermesEnabledPlugins(entries []*yaml.Node) []*yaml.Node {
	for _, e := range entries {
		if e != nil && e.Kind == yaml.ScalarNode && e.Value == HermesPluginID {
			return entries
		}
	}
	return append(entries, &yaml.Node{
		Kind:  yaml.ScalarNode,
		Tag:   "!!str",
		Value: HermesPluginID,
	})
}
