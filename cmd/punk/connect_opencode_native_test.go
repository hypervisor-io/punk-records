package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hypervisor-io/punk-records/internal/hookcli"
)

func withWorkingDir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
}

// codeops:trace repo=punk-records work_item=punk-connect-remote-url-review-20261009 spec=docs/client-credentials.md plan=phase-0/task-0-A test=TestConnectOpenCodeAdoptsJSONCBeforePluginWrite evidence=docs/superpowers/reports/2026-10-09-client-credentials/builder-a.md
func TestConnectOpenCodeAdoptsJSONCBeforePluginWrite(t *testing.T) {
	dir := t.TempDir()
	withWorkingDir(t, dir)
	jsonc := `{
  // retained
  "mcp": {"foreign":{"type":"local","command":["x"]},},
}
`
	if err := os.WriteFile("opencode.jsonc", []byte(jsonc), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmdConnectOpenCode([]string{"--project", "--no-skill", "--url", "http://127.0.0.1:19090"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat("opencode.json"); !os.IsNotExist(err) {
		t.Fatalf("competing opencode.json created: %v", err)
	}
	raw, err := os.ReadFile("opencode.jsonc")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "// retained") || !strings.Contains(string(raw), `"foreign"`) || !strings.Contains(string(raw), `"punk"`) {
		t.Fatalf("JSONC not adopted faithfully:\n%s", raw)
	}
}

func TestConnectOpenCodeRefusesAmbiguousConfigBeforePluginWrite(t *testing.T) {
	dir := t.TempDir()
	withWorkingDir(t, dir)
	if err := os.WriteFile("opencode.json", []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("opencode.jsonc", []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := cmdConnectOpenCode([]string{"--project", "--no-skill", "--url", "http://127.0.0.1:19090"})
	if err == nil || !strings.Contains(err.Error(), "both") {
		t.Fatalf("want ambiguity error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(".opencode", "plugins", "punk-memory.js")); !os.IsNotExist(err) {
		t.Fatalf("plugin changed before preflight: %v", err)
	}
}

func TestConnectOpenCodeInvalidConfigLeavesManagedPluginUntouched(t *testing.T) {
	dir := t.TempDir()
	withWorkingDir(t, dir)
	plugin := filepath.Join(".opencode", "plugins", "punk-memory.js")
	if _, err := hookcli.ConnectOpenCode(plugin, "http://old.example.test:9090"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(plugin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("opencode.json", []byte(`{"mcp":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmdConnectOpenCode([]string{"--project", "--no-skill", "--url", "http://127.0.0.1:19090"}); err == nil {
		t.Fatal("malformed config must fail before plugin write")
	}
	after, err := os.ReadFile(plugin)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("managed plugin changed before config preflight completed")
	}
}

func TestConnectOpenCodeInvalidCredentialsFailBeforeAnyWrite(t *testing.T) {
	dir := t.TempDir()
	withWorkingDir(t, dir)
	credentials := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(credentials, []byte(`{"url":"https://user:pass@example.test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PUNK_CREDENTIALS", credentials)
	t.Setenv("PUNK_URL", "")
	t.Setenv("PUNK_API_KEY", "")
	if err := cmdConnectOpenCode([]string{"--project", "--no-skill"}); err == nil {
		t.Fatal("invalid present credentials must fail")
	}
	for _, path := range []string{"opencode.json", "opencode.jsonc", filepath.Join(".opencode", "plugins", "punk-memory.js")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("invalid credentials wrote %s: %v", path, err)
		}
	}
}

func TestConnectOpenCodeForeignConfigLeavesPluginUntouched(t *testing.T) {
	dir := t.TempDir()
	withWorkingDir(t, dir)
	if err := os.WriteFile("opencode.jsonc", []byte(`{
  // foreign owner
  "mcp": {"punk":{"type":"local","command":["foreign"]},},
}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := cmdConnectOpenCode([]string{"--project", "--no-skill", "--url", "http://127.0.0.1:19090"})
	if err == nil || !strings.Contains(err.Error(), "foreign") {
		t.Fatalf("want foreign ownership refusal, got %v", err)
	}
	if _, err := os.Lstat(filepath.Join(".opencode", "plugins", "punk-memory.js")); !os.IsNotExist(err) {
		t.Fatalf("foreign config allowed plugin write: %v", err)
	}
}

// TestConnectOpenCodeKeepsPluginLoadedWhenIdle: connect sets
// experimental.location_idle_timeout=false beside the MCP entry so a
// headless opencode serve does not unload the plugin (and its inbox
// wake) after 60 idle minutes; --keep-idle-unload leaves it unset.
func TestConnectOpenCodeKeepsPluginLoadedWhenIdle(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{"default", nil, true},
		{"opt out", []string{"--keep-idle-unload"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withWorkingDir(t, t.TempDir())
			args := append([]string{"--project", "--no-skill", "--url", "http://127.0.0.1:19090"}, tc.args...)
			if err := cmdConnectOpenCode(args); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile("opencode.json")
			if err != nil {
				t.Fatal(err)
			}
			var cfg struct {
				MCP          map[string]any `json:"mcp"`
				Experimental map[string]any `json:"experimental"`
			}
			if err := json.Unmarshal(raw, &cfg); err != nil {
				t.Fatalf("opencode.json: %v\n%s", err, raw)
			}
			if cfg.MCP["punk"] == nil {
				t.Fatalf("mcp.punk missing:\n%s", raw)
			}
			v, set := cfg.Experimental["location_idle_timeout"]
			if got := set && v == false; got != tc.want {
				t.Fatalf("location_idle_timeout set=%v value=%v, want %v:\n%s", set, v, tc.want, raw)
			}
		})
	}
}

// TestConnectOpenCodeMessagingFlag: --messaging bakes messaging on into
// the plugin, a later run without the flag keeps it, and
// --messaging=false turns it back off.
func TestConnectOpenCodeMessagingFlag(t *testing.T) {
	withWorkingDir(t, t.TempDir())
	plugin := filepath.Join(".opencode", "plugins", "punk-memory.js")
	base := []string{"--project", "--no-skill", "--no-mcp", "--url", "http://127.0.0.1:19090"}
	for _, step := range []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{"--messaging"}, true},
		{nil, true},
		{[]string{"--messaging=false"}, false},
	} {
		if err := cmdConnectOpenCode(append(append([]string{}, base...), step.args...)); err != nil {
			t.Fatal(err)
		}
		if got := hookcli.InstalledOpenCodeMessaging(plugin); got != step.want {
			t.Fatalf("after %v: messaging=%v, want %v", step.args, got, step.want)
		}
	}
}
