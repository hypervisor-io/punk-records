package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/hypervisor-io/punk-records/internal/hookcli"
)

// P4-F CLI tests: the plugin half of "punk connect hermes". The plugin is
// what makes punk run in the Hermes Desktop backend, which never registers
// shell hooks (P4-D spike). Nothing here touches the operator's real
// ~/.hermes - doctorHomeFixture redirects HOME and HERMES_HOME.

// hermesPluginDirFor returns the plugin directory cmdConnectHermes writes
// for a defaulted config path: <dir of config.yaml>/plugins/<id>.
func hermesPluginDirFor(home string) string {
	return filepath.Join(home, ".hermes", "plugins", hookcli.HermesPluginID)
}

func readEnabledPlugins(t *testing.T, configPath string) []string {
	t.Helper()
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Plugins struct {
			Enabled []string `yaml:"enabled"`
		} `yaml:"plugins"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("config.yaml is not valid YAML: %v\n%s", err, raw)
	}
	return cfg.Plugins.Enabled
}

// TestConnectHermesWritesPlugin pins that a plain connect writes the plugin
// entry + manifest and enables the plugin in plugins.enabled.
func TestConnectHermesWritesPlugin(t *testing.T) {
	home, _ := doctorHomeFixture(t)
	stdout, err := captureStdout(t, func() error {
		return run([]string{"connect", "hermes", "--no-mcp", "--no-skill"})
	})
	if err != nil {
		t.Fatalf("connect hermes: %v\n%s", err, stdout)
	}

	pluginDir := hermesPluginDirFor(home)
	entry, err := os.ReadFile(filepath.Join(pluginDir, "__init__.py"))
	if err != nil {
		t.Fatalf("plugin entry not written at %s: %v\n%s", pluginDir, err, stdout)
	}
	if !strings.HasPrefix(string(entry), "# managed by punk connect hermes") {
		t.Fatalf("plugin entry is missing the managed marker:\n%s", entry)
	}
	manifest, err := os.ReadFile(filepath.Join(pluginDir, "plugin.yaml"))
	if err != nil {
		t.Fatalf("plugin manifest not written: %v", err)
	}
	var m struct {
		Name string `yaml:"name"`
		Kind string `yaml:"kind"`
	}
	if err := yaml.Unmarshal(manifest, &m); err != nil {
		t.Fatalf("plugin.yaml is not valid YAML: %v\n%s", err, manifest)
	}
	if m.Name != hookcli.HermesPluginID || m.Kind != "standalone" {
		t.Fatalf("manifest = %+v, want name=%s kind=standalone", m, hookcli.HermesPluginID)
	}

	if got := readEnabledPlugins(t, filepath.Join(home, ".hermes", "config.yaml")); len(got) != 1 || got[0] != hookcli.HermesPluginID {
		t.Fatalf("plugins.enabled = %v, want [%s]", got, hookcli.HermesPluginID)
	}
	if !strings.Contains(stdout, "wrote the Hermes plugin to "+pluginDir) {
		t.Fatalf("plugin write line missing:\n%s", stdout)
	}
	if !strings.Contains(stdout, "enabled plugin") {
		t.Fatalf("plugin enable line missing:\n%s", stdout)
	}
	// The P4-D coverage note must still promise Desktop coverage.
	if !strings.Contains(stdout, "Hermes CLI and Gateway") || !strings.Contains(stdout, "Desktop") {
		t.Fatalf("coverage note missing:\n%s", stdout)
	}
}

// TestConnectHermesPluginReconnectIdempotent pins that a second connect does
// not rewrite the plugin and does not duplicate the plugins.enabled entry.
func TestConnectHermesPluginReconnectIdempotent(t *testing.T) {
	home, _ := doctorHomeFixture(t)
	if _, err := captureStdout(t, func() error {
		return run([]string{"connect", "hermes", "--no-mcp", "--no-skill"})
	}); err != nil {
		t.Fatalf("first connect: %v", err)
	}
	pluginDir := hermesPluginDirFor(home)
	before, err := os.ReadFile(filepath.Join(pluginDir, "__init__.py"))
	if err != nil {
		t.Fatal(err)
	}

	stdout, err := captureStdout(t, func() error {
		return run([]string{"connect", "hermes", "--no-mcp", "--no-skill"})
	})
	if err != nil {
		t.Fatalf("second connect: %v\n%s", err, stdout)
	}
	after, err := os.ReadFile(filepath.Join(pluginDir, "__init__.py"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("reconnect must not rewrite an up-to-date plugin")
	}
	if !strings.Contains(stdout, "already has punk's Hermes plugin up to date") {
		t.Fatalf("idempotent plugin line missing:\n%s", stdout)
	}
	if !strings.Contains(stdout, "already enables punk's plugin") {
		t.Fatalf("idempotent enable line missing:\n%s", stdout)
	}
	if got := readEnabledPlugins(t, filepath.Join(home, ".hermes", "config.yaml")); len(got) != 1 || got[0] != hookcli.HermesPluginID {
		t.Fatalf("plugins.enabled = %v, want exactly one %s", got, hookcli.HermesPluginID)
	}
}

// TestConnectHermesPluginMessagingFlag pins that --messaging is baked into the
// generated plugin (the inbox catch-up call) and absent without it.
func TestConnectHermesPluginMessagingFlag(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"connect", "hermes", "--no-mcp", "--no-skill"}, "PUNK_MESSAGING = False"},
		{[]string{"connect", "hermes", "--no-mcp", "--no-skill", "--messaging"}, "PUNK_MESSAGING = True"},
	} {
		home, _ := doctorHomeFixture(t)
		stdout, err := captureStdout(t, func() error { return run(tc.args) })
		if err != nil {
			t.Fatalf("connect %v: %v\n%s", tc.args, err, stdout)
		}
		entry, err := os.ReadFile(filepath.Join(hermesPluginDirFor(home), "__init__.py"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(entry), tc.want) {
			t.Fatalf("connect %v: plugin missing %q", tc.args, tc.want)
		}
	}
}

// TestConnectHermesPluginRefusesForeignPlugin pins that a hand-authored
// plugin at punk's directory is never clobbered: connect fails loudly and
// leaves the foreign file untouched.
func TestConnectHermesPluginRefusesForeignPlugin(t *testing.T) {
	home, _ := doctorHomeFixture(t)
	pluginDir := hermesPluginDirFor(home)
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(pluginDir, "__init__.py")
	if err := os.WriteFile(foreign, []byte("# my own plugin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, err := captureStdout(t, func() error {
		return run([]string{"connect", "hermes", "--no-mcp", "--no-skill"})
	})
	if err == nil {
		t.Fatalf("connect must refuse a foreign plugin file:\n%s", stdout)
	}
	got, readErr := os.ReadFile(foreign)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "# my own plugin\n" {
		t.Fatalf("foreign plugin was modified: %q", got)
	}
}
