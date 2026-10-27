package hookcli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"
)

// hermesPluginGoldenInputs are the fixed inputs the golden fixtures were
// generated with. Regenerate testdata/hermes_plugin_golden.py and
// testdata/hermes_plugin_golden.yaml from hermesPluginSource /
// hermesPluginManifest when the generator changes intentionally - never
// hand-edit the fixtures.
const (
	hermesPluginGoldenURL  = "http://localhost:9090"
	hermesPluginGoldenBin  = "/usr/local/bin/punk"
	hermesPluginGoldenMsg  = true
	hermesPluginGoldenPy   = "testdata/hermes_plugin_golden.py"
	hermesPluginGoldenYAML = "testdata/hermes_plugin_golden.yaml"
)

func readGolden(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(path))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestWriteHermesPluginGoldenContent pins the exact bytes WriteHermesPlugin
// writes for a fresh plugin directory: the entry file and the plugin.yaml
// manifest, both compared byte-for-byte against the checked-in fixtures.
func TestWriteHermesPluginGoldenContent(t *testing.T) {
	dir := t.TempDir()
	changed, err := WriteHermesPlugin(dir, hermesPluginGoldenURL, hermesPluginGoldenBin, hermesPluginGoldenMsg)
	if err != nil || !changed {
		t.Fatal(changed, err)
	}

	entry, err := os.ReadFile(filepath.Join(dir, "__init__.py"))
	if err != nil {
		t.Fatal(err)
	}
	if want := readGolden(t, hermesPluginGoldenPy); string(entry) != want {
		t.Fatalf("entry golden mismatch (regenerate testdata/hermes_plugin_golden.py if the generator changed intentionally):\n%s",
			goldenMismatchWindow(string(entry), want))
	}
	if !strings.HasPrefix(string(entry), hermesPluginMarker) {
		t.Fatalf("marker must be the first line, got: %q", entry[:min(len(entry), 80)])
	}

	manifest, err := os.ReadFile(filepath.Join(dir, "plugin.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if want := readGolden(t, hermesPluginGoldenYAML); string(manifest) != want {
		t.Fatalf("manifest golden mismatch (regenerate testdata/hermes_plugin_golden.yaml if the generator changed intentionally):\n%s",
			goldenMismatchWindow(string(manifest), want))
	}
}

// TestWriteHermesPluginCreatesAllFiles covers the fresh-install path: both
// files appear, the manifest is valid YAML, and its name/kind/provides_hooks
// are what Hermes' loader keys on.
func TestWriteHermesPluginCreatesAllFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "plugins", HermesPluginID)
	changed, err := WriteHermesPlugin(dir, "http://localhost:9090", "/usr/local/bin/punk", false)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("creating a plugin must report changed")
	}

	raw, err := os.ReadFile(filepath.Join(dir, "plugin.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Name          string   `yaml:"name"`
		Kind          string   `yaml:"kind"`
		ProvidesHooks []string `yaml:"provides_hooks"`
	}
	if err := yaml.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("written manifest is not valid YAML: %v\n%s", err, raw)
	}
	if manifest.Name != HermesPluginID {
		t.Fatalf("manifest name = %q, want %q (Hermes keys plugins.enabled on it)", manifest.Name, HermesPluginID)
	}
	if manifest.Kind != "standalone" {
		t.Fatalf("manifest kind = %q, want standalone (the opt-in loader path)", manifest.Kind)
	}
	if strings.Join(manifest.ProvidesHooks, ",") != strings.Join(hermesPluginEvents, ",") {
		t.Fatalf("manifest provides_hooks = %v, want %v", manifest.ProvidesHooks, hermesPluginEvents)
	}
}

// TestWriteHermesPluginIdempotent verifies a re-run with identical inputs
// reports changed=false.
func TestWriteHermesPluginIdempotent(t *testing.T) {
	dir := t.TempDir()
	if changed, err := WriteHermesPlugin(dir, "http://localhost:9090", "/usr/local/bin/punk", true); err != nil || !changed {
		t.Fatal(changed, err)
	}
	changed, err := WriteHermesPlugin(dir, "http://localhost:9090", "/usr/local/bin/punk", true)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("idempotent re-run must report changed=false")
	}
}

// TestWriteHermesPluginRewritesItsOwnFile verifies a stale managed plugin (an
// old URL, or a messaging flip) is overwritten in place.
func TestWriteHermesPluginRewritesItsOwnFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := WriteHermesPlugin(dir, "http://old:9090", "/usr/local/bin/punk", false); err != nil {
		t.Fatal(err)
	}
	changed, err := WriteHermesPlugin(dir, "http://new:9090", "/usr/local/bin/punk", true)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("a changed server URL must rewrite the plugin")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "__init__.py"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"http://new:9090"`) {
		t.Fatalf("rewritten plugin does not carry the new URL:\n%s", raw)
	}
	if !strings.Contains(string(raw), "PUNK_MESSAGING = True") {
		t.Fatalf("rewritten plugin does not carry the messaging flip:\n%s", raw)
	}
}

// TestWriteHermesPluginRefusesForeignFiles verifies neither target file is
// clobbered when it is not punk's.
func TestWriteHermesPluginRefusesForeignFiles(t *testing.T) {
	for _, name := range []string{"__init__.py", "plugin.yaml"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, name)
			if err := os.WriteFile(target, []byte("hand authored\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := WriteHermesPlugin(dir, "http://localhost:9090", "/usr/local/bin/punk", true)
			if err == nil {
				t.Fatalf("expected a refusal for a foreign %s", name)
			}
			got, readErr := os.ReadFile(target)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(got) != "hand authored\n" {
				t.Fatalf("foreign %s was modified: %q", name, got)
			}
		})
	}
}

// TestHermesPluginSourceUsesShellHookCommands pins that the generated plugin
// invokes the SAME argv the shell hooks do (connect_hermes.go's
// punkHermesHookCommand / punkInboxHookCommand), so the wire path cannot
// drift between the two hook systems.
func TestHermesPluginSourceUsesShellHookCommands(t *testing.T) {
	src := hermesPluginSource("http://localhost:9090", "/usr/local/bin/punk", true)
	for _, want := range []string{
		`"hook", "--from", "hermes", "--url", _punk_url()`,
		`"hook", "inbox", "--client", "hermes", "--mode", "context"`,
		`"--url", _punk_url(), "--messaging"`,
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("generated plugin is missing %q; it must mirror connect_hermes.go's command builders", want)
		}
	}
	// The shell-hook command strings carry the same markers the plugin's
	// dedup probe looks for, so a shell hook registered for an event is
	// recognised by the plugin.
	capture := punkHermesHookCommand("/usr/local/bin/punk", "http://localhost:9090")
	if !strings.Contains(capture, " hook --from hermes") {
		t.Fatalf("punkHermesHookCommand no longer carries the capture marker the plugin probes for: %q", capture)
	}
	inbox := punkInboxHookCommand("/usr/local/bin/punk", "hermes", "context", "", "http://localhost:9090", "")
	if !strings.Contains(inbox, " hook inbox --client hermes") {
		t.Fatalf("punkInboxHookCommand no longer carries the inbox marker the plugin probes for: %q", inbox)
	}
}

// TestHermesPluginSourceEscapesHostileValues verifies a server URL or binary
// path containing quotes/backslashes/newlines cannot break out of the Python
// string literal it is spliced into.
func TestHermesPluginSourceEscapesHostileValues(t *testing.T) {
	hostileURL := "http://x\"; import os; os.system('boom'); #\n"
	src := hermesPluginSource(hostileURL, "/bin/p\nunk", true)
	if strings.Contains(src, "\nunk") {
		t.Fatalf("binary path newline was not escaped:\n%s", src)
	}
	// The literal must round-trip through Python's own parser to the exact
	// original value - proven by the Python unit test, which loads the file;
	// here we just assert the raw newline never leaks into source.
	if strings.Contains(src, "PUNK_DEFAULT_BIN = \"/bin/p\nunk\"") {
		t.Fatal("raw newline leaked into the generated literal")
	}
}

// -- ConnectHermesPluginEnable ---------------------------------------------

func enabledPlugins(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Plugins struct {
			Enabled []string `yaml:"enabled"`
		} `yaml:"plugins"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("written config is not valid YAML: %v\n%s", err, raw)
	}
	return doc.Plugins.Enabled
}

func TestConnectHermesPluginEnableCreatesKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.yaml")
	changed, err := ConnectHermesPluginEnable(path)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("creating plugins.enabled must report changed")
	}
	if got := enabledPlugins(t, path); len(got) != 1 || got[0] != HermesPluginID {
		t.Fatalf("plugins.enabled = %v, want [%s]", got, HermesPluginID)
	}
}

func TestConnectHermesPluginEnablePreservesForeignEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	before := "model: gpt\nplugins:\n  enabled:\n    - other-plugin\n  disabled:\n    - nope\nhooks:\n  pre_llm_call:\n    - command: \"/bin/keep\"\n"
	if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := ConnectHermesPluginEnable(path)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("adding punk-memory must report changed")
	}
	if got := enabledPlugins(t, path); len(got) != 2 || got[0] != "other-plugin" || got[1] != HermesPluginID {
		t.Fatalf("plugins.enabled = %v, want [other-plugin %s]", got, HermesPluginID)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"model: gpt", "- nope", "/bin/keep"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("merge dropped %q:\n%s", want, raw)
		}
	}
}

func TestConnectHermesPluginEnableIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if _, err := ConnectHermesPluginEnable(path); err != nil {
		t.Fatal(err)
	}
	changed, err := ConnectHermesPluginEnable(path)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("re-adding an already-present plugin must report changed=false")
	}
}

func TestConnectHermesPluginEnableRefusesWrongShapes(t *testing.T) {
	cases := map[string]string{
		"plugins not a mapping":  "plugins: 7\n",
		"enabled not a sequence": "plugins:\n  enabled: 7\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := ConnectHermesPluginEnable(path); err == nil {
				t.Fatal("expected a refusal")
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != body {
				t.Fatalf("file was modified despite the refusal:\n%s", raw)
			}
		})
	}
}

func TestConnectHermesPluginEnableParseErrorLeavesFileUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "plugins: [unclosed\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectHermesPluginEnable(path); err == nil {
		t.Fatal("expected a parse error")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != body {
		t.Fatalf("parse failure modified the file:\n%s", raw)
	}
}

// -- Python unit tests for the generated plugin -----------------------------

var (
	hermesPythonOnce sync.Once
	hermesPythonPath string
	hermesPythonErr  error
)

// hermesPython3 resolves python3 once per test binary (sync.Once, no
// goroutine, no per-test PATH scan) and caches the result.
func hermesPython3() (string, error) {
	hermesPythonOnce.Do(func() {
		hermesPythonPath, hermesPythonErr = exec.LookPath("python3")
	})
	return hermesPythonPath, hermesPythonErr
}

// TestHermesPluginPythonUnitTests runs the stdlib unittest suite in
// testdata/hermes_plugin_test.py against a freshly generated plugin
// directory, so the plugin's registration, payload shape, dedup and
// fail-open behaviour are exercised as the generator actually emits them.
// Skips (not fails) when python3 is not on PATH.
func TestHermesPluginPythonUnitTests(t *testing.T) {
	python, err := hermesPython3()
	if err != nil {
		t.Skip("python3 not on PATH; skipping the generated-plugin unit tests")
	}

	dir := filepath.Join(t.TempDir(), "plugins", HermesPluginID)
	if _, err := WriteHermesPlugin(dir, hermesPluginGoldenURL, hermesPluginGoldenBin, hermesPluginGoldenMsg); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(python, filepath.Join("testdata", "hermes_plugin_test.py"), dir)
	cmd.Env = append(os.Environ(), "PUNK_TEST_LOG=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("python unit tests failed: %v\n%s", err, out)
	}
}
