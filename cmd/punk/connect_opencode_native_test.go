package main

import (
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
