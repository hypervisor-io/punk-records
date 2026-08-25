package hookcli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readOpenClawConfig(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("config is not valid JSON: %v\n%s", err, raw)
	}
	return cfg
}

// openClawEntryFlags digs out the four booleans ConnectOpenClaw is
// responsible for. A missing level returns ok=false so a test can tell
// "wrong value" from "never written".
func openClawEntryFlags(cfg map[string]any) (pluginsEnabled, entryEnabled, conversation, injection bool, ok bool) {
	plugins, _ := cfg["plugins"].(map[string]any)
	if plugins == nil {
		return
	}
	entries, _ := plugins["entries"].(map[string]any)
	if entries == nil {
		return
	}
	entry, _ := entries[OpenClawPluginID].(map[string]any)
	if entry == nil {
		return
	}
	hooks, _ := entry["hooks"].(map[string]any)
	if hooks == nil {
		return
	}
	pluginsEnabled, _ = plugins["enabled"].(bool)
	entryEnabled, _ = entry["enabled"].(bool)
	conversation, _ = hooks["allowConversationAccess"].(bool)
	injection, _ = hooks["allowPromptInjection"].(bool)
	return pluginsEnabled, entryEnabled, conversation, injection, true
}

// TestWriteOpenClawPluginCreatesBothFiles pins the two artifacts and the
// link between them: package.json's openclaw.pluginEntry must name the file
// actually written, or OpenClaw loads nothing.
func TestWriteOpenClawPluginCreatesBothFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "plugins", OpenClawPluginID)
	changed, err := WriteOpenClawPlugin(dir, "http://memory.internal:9090")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("a fresh write must report changed")
	}

	entry, err := os.ReadFile(filepath.Join(dir, "index.js"))
	if err != nil {
		t.Fatal(err)
	}
	if !hasOpenClawMarker(string(entry)) {
		t.Fatalf("entry file is missing the managed marker:\n%s", entry[:120])
	}
	if !strings.Contains(string(entry), `"http://memory.internal:9090"`) {
		t.Fatal("the server URL was not rendered into the plugin")
	}
	// The NUL separator must reach the file as the six-character JS escape,
	// never as a raw control byte in the source.
	if strings.ContainsRune(string(entry), 0) {
		t.Fatal("a raw NUL byte landed in the generated JavaScript")
	}
	// Compared against nulSeparator itself rather than a hand-typed
	// escape: typing the escape here is the exact mistake the
	// separator is built programmatically to avoid.
	if !strings.Contains(string(entry), nulSeparator) {
		t.Fatal("the hash separator escape is missing from the generated JavaScript")
	}

	var pkg struct {
		Name     string `json:"name"`
		Type     string `json:"type"`
		Main     string `json:"main"`
		OpenClaw struct {
			PluginEntry string `json:"pluginEntry"`
			Permissions struct {
				Conversation bool `json:"conversation"`
				Sessions     bool `json:"sessions"`
			} `json:"permissions"`
		} `json:"openclaw"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		t.Fatalf("package.json is not valid JSON: %v\n%s", err, raw)
	}
	if pkg.Name != OpenClawPluginID {
		t.Fatalf("package name = %q, want %q (config.json keys the entry by this)", pkg.Name, OpenClawPluginID)
	}
	if pkg.Type != "module" {
		t.Fatalf("type = %q, want module (the entry file uses export default)", pkg.Type)
	}
	if pkg.OpenClaw.PluginEntry != "./index.js" || pkg.Main != "./index.js" {
		t.Fatalf("pluginEntry/main point at %q/%q, not the file that was written", pkg.OpenClaw.PluginEntry, pkg.Main)
	}
	if !pkg.OpenClaw.Permissions.Conversation || !pkg.OpenClaw.Permissions.Sessions {
		t.Fatal("the plugin must declare the conversation and sessions permissions its hooks use")
	}
}

// TestWriteOpenClawPluginIdempotent pins that an unchanged rewrite reports
// changed=false, and that a changed URL does rewrite.
func TestWriteOpenClawPluginIdempotent(t *testing.T) {
	dir := t.TempDir()
	if _, err := WriteOpenClawPlugin(dir, "http://localhost:9090"); err != nil {
		t.Fatal(err)
	}
	changed, err := WriteOpenClawPlugin(dir, "http://localhost:9090")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("an identical rewrite must report changed=false")
	}
	changed, err = WriteOpenClawPlugin(dir, "http://elsewhere:9090")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("a changed URL must rewrite the plugin")
	}
}

// TestWriteOpenClawPluginRefusesForeignFiles pins the "never destroy user
// work" rule for both files, and that the refusal leaves them byte-exact.
func TestWriteOpenClawPluginRefusesForeignFiles(t *testing.T) {
	t.Run("foreign index.js", func(t *testing.T) {
		dir := t.TempDir()
		const body = "// my own plugin\nexport default {};\n"
		if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := WriteOpenClawPlugin(dir, "http://localhost:9090"); err == nil {
			t.Fatal("want a refusal for an unmarked entry file")
		}
		after, err := os.ReadFile(filepath.Join(dir, "index.js"))
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != body {
			t.Fatalf("a refused write modified the file:\n%s", after)
		}
		if _, err := os.Stat(filepath.Join(dir, "package.json")); err == nil {
			t.Fatal("a refused write still created package.json")
		}
	})

	t.Run("package.json for another plugin", func(t *testing.T) {
		dir := t.TempDir()
		const body = `{"name":"someone-elses-plugin","version":"2.0.0"}`
		if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		err := WriteOpenClawPluginErr(t, dir)
		if !strings.Contains(err.Error(), "someone-elses-plugin") {
			t.Fatalf("error %q does not name the conflicting plugin", err)
		}
		after, readErr := os.ReadFile(filepath.Join(dir, "package.json"))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if string(after) != body {
			t.Fatalf("a refused write modified package.json:\n%s", after)
		}
		if _, statErr := os.Stat(filepath.Join(dir, "index.js")); statErr == nil {
			t.Fatal("a refused write still created index.js")
		}
	})

	t.Run("unparseable package.json", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := WriteOpenClawPlugin(dir, "http://localhost:9090"); err == nil {
			t.Fatal("want a refusal for a package.json punk cannot parse")
		}
	})
}

// WriteOpenClawPluginErr is a tiny helper for the cases that must fail: it
// fails the test if the write unexpectedly succeeds, so each caller can
// assert on the message without a nil check.
func WriteOpenClawPluginErr(t *testing.T, dir string) error {
	t.Helper()
	_, err := WriteOpenClawPlugin(dir, "http://localhost:9090")
	if err == nil {
		t.Fatal("want a refusal, got success")
	}
	return err
}

// TestWriteOpenClawPluginRewritesItsOwnFile pins that punk's OWN marked
// file is upgradable - the marker check must not lock punk out of its own
// plugin on the next release.
func TestWriteOpenClawPluginRewritesItsOwnFile(t *testing.T) {
	dir := t.TempDir()
	stale := openClawPluginMarker + "\n// an older punk release\n"
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteOpenClawPlugin(dir, "http://localhost:9090"); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(dir, "index.js"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(after), "an older punk release") {
		t.Fatal("punk failed to replace its own stale plugin")
	}
}

// TestConnectOpenClawCreatesConfig pins all four flags. Each one is
// load-bearing: OpenClaw gates conversation reads and prompt mutation
// separately, so a plugin enabled without them loads and silently does
// nothing.
func TestConnectOpenClawCreatesConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	changed, err := ConnectOpenClaw(path)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("creating a config must report changed")
	}
	pluginsEnabled, entryEnabled, conversation, injection, ok := openClawEntryFlags(readOpenClawConfig(t, path))
	if !ok {
		t.Fatal("the plugin entry was not written at all")
	}
	if !pluginsEnabled || !entryEnabled || !conversation || !injection {
		t.Fatalf("flags: plugins=%v entry=%v conversation=%v injection=%v", pluginsEnabled, entryEnabled, conversation, injection)
	}
}

// TestConnectOpenClawPreservesForeignConfig pins that unrelated keys at
// every level survive - including a sibling plugin's own entry and punk's
// own user-supplied config block.
func TestConnectOpenClawPreservesForeignConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	const body = `{
  "gateway": {"port": 8123},
  "plugins": {
    "load": {"paths": ["~/.openclaw/policies/maintenance.ts"]},
    "entries": {
      "voice-call": {"enabled": true, "config": {"voice": "alloy"}},
      "punk-memory": {"config": {"note": "keep me"}}
    }
  }
}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectOpenClaw(path); err != nil {
		t.Fatal(err)
	}

	cfg := readOpenClawConfig(t, path)
	if gw, _ := cfg["gateway"].(map[string]any); gw == nil || gw["port"] == nil {
		t.Fatalf("an unrelated top-level key was dropped: %v", cfg)
	}
	plugins, _ := cfg["plugins"].(map[string]any)
	if load, _ := plugins["load"].(map[string]any); load == nil {
		t.Fatal("plugins.load was dropped")
	}
	entries, _ := plugins["entries"].(map[string]any)
	if other, _ := entries["voice-call"].(map[string]any); other == nil || other["config"] == nil {
		t.Fatal("another plugin's entry was dropped")
	}
	entry, _ := entries[OpenClawPluginID].(map[string]any)
	inner, _ := entry["config"].(map[string]any)
	if inner == nil || inner["note"] != "keep me" {
		t.Fatalf("the user's own config for punk's entry was dropped: %v", entry)
	}
	if _, _, conversation, injection, _ := openClawEntryFlags(cfg); !conversation || !injection {
		t.Fatal("the hook flags were not added to an existing entry")
	}
}

// TestConnectOpenClawIdempotent pins the no-op contract.
func TestConnectOpenClawIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if _, err := ConnectOpenClaw(path); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := ConnectOpenClaw(path)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("an unchanged reconnect must report changed=false")
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("an unchanged reconnect rewrote the file")
	}
}

// TestConnectOpenClawRefusesWrongShapes pins that every level punk writes
// through is shape-checked by its full dotted path, and that a refusal
// leaves the file untouched. The JSON5/JSONC case matters in practice:
// OpenClaw's own docs show a json5-flavored config, which punk cannot
// re-encode faithfully and must therefore refuse rather than rewrite.
func TestConnectOpenClawRefusesWrongShapes(t *testing.T) {
	cases := map[string]struct{ body, wantErr string }{
		"plugins is a string":  {`{"plugins":"off"}`, "plugins is not an object"},
		"entries is an array":  {`{"plugins":{"entries":[]}}`, "plugins.entries is not an object"},
		"entry is a bool":      {`{"plugins":{"entries":{"punk-memory":true}}}`, "plugins.entries.punk-memory is not an object"},
		"entry hooks is a int": {`{"plugins":{"entries":{"punk-memory":{"hooks":5}}}}`, "plugins.entries.punk-memory.hooks is not an object"},
		"json5 with comments":  {"{\n  // a comment\n  \"plugins\": {}\n}", "parse settings"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			changed, err := ConnectOpenClaw(path)
			if err == nil {
				t.Fatalf("want a refusal, got changed=%v", changed)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not name the problem (%q)", err, tc.wantErr)
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(after) != tc.body {
				t.Fatalf("a refused merge modified the file:\n%s", after)
			}
		})
	}
}
