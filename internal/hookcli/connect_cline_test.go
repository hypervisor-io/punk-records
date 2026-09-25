package hookcli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Contract evidence fetched 2026-09-25:
// https://github.com/cline/cline/blob/main/apps/vscode/src/core/hooks/utils.ts
// https://github.com/cline/cline/blob/main/apps/vscode/src/hosts/vscode/mcp-settings-legacy-migration.ts
// https://github.com/cline/cline/blob/main/apps/vscode/src/services/mcp/schemas.ts
func TestConnectClineFilesPreserveUsersAndModes(t *testing.T) {
	dir := t.TempDir()
	foreign := filepath.Join(dir, "UserPromptSubmit")
	body := []byte("#!/bin/sh\n# user's hook\nexit 0\n")
	if err := os.WriteFile(foreign, body, 0700); err != nil {
		t.Fatal(err)
	}
	o := ClineConnectOpts{PunkPath: "/bin/punk", ServerURL: "http://localhost:9090", GOOS: "linux"}
	if _, err := ConnectCline(dir, o); err == nil {
		t.Fatal("foreign executable overwritten")
	}
	if got, _ := os.ReadFile(foreign); !bytes.Equal(got, body) {
		t.Fatal("foreign hook changed")
	}
	if _, err := os.Stat(filepath.Join(dir, "TaskStart")); !os.IsNotExist(err) {
		t.Fatal("partial hooks written before conflict")
	}
	if err := os.Remove(foreign); err != nil {
		t.Fatal(err)
	}
	if changed, err := ConnectCline(dir, o); err != nil || !changed {
		t.Fatalf("connect=%t %v", changed, err)
	}
	path := filepath.Join(dir, "TaskStart")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(got), "#!/bin/sh\n# punk-managed: cline-hook v1\n") || !strings.Contains(string(got), "hook --from cline") || strings.Contains(string(got), "--messaging") {
		t.Fatalf("script=%s", got)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0755 {
		t.Fatalf("new mode=%o", info.Mode().Perm())
	}
	if changed, err := ConnectCline(dir, o); err != nil || changed {
		t.Fatalf("rerun=%t %v", changed, err)
	}
	// Unix execution permission is Cline's enabled switch, preserve disable.
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	o.Messaging = true
	if changed, err := ConnectCline(dir, o); err != nil || !changed {
		t.Fatalf("enable=%t %v", changed, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0600 {
		t.Fatal("rerun enabled disabled hook")
	}
	if got, _ := os.ReadFile(path); !strings.Contains(string(got), "--messaging") {
		t.Fatal("missing flag")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(foreign, path); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectCline(dir, o); err == nil {
		t.Fatal("symlink hook accepted")
	}
}

func TestConnectClineWindowsAndSafeShellArguments(t *testing.T) {
	dir := t.TempDir()
	o := ClineConnectOpts{PunkPath: `C:\O'Brien\punk.exe`, ServerURL: "https://server/", Namespace: "team", Messaging: true, GOOS: "windows"}
	if _, err := ConnectCline(dir, o); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "TaskStart.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `C:\O''Brien\punk.exe`) || !strings.Contains(string(raw), "[Console]::In.ReadToEnd()") || !strings.Contains(string(raw), "--messaging") {
		t.Fatalf("PS script=%s", raw)
	}
	if _, err := os.Stat(filepath.Join(dir, "TaskStart")); !os.IsNotExist(err) {
		t.Fatal("extensionless Windows hook")
	}
	if runtime.GOOS == "windows" {
		return
	}
	// Execute real generated Unix wrapper with metacharacters in every argument.
	punk := filepath.Join(t.TempDir(), "punk ' $file")
	stub := "#!/bin/sh\nprintf '%s\\n' \"$@\"\n"
	if err := os.WriteFile(punk, []byte(stub), 0700); err != nil {
		t.Fatal(err)
	}
	o = ClineConnectOpts{PunkPath: punk, ServerURL: "http://host/?x=$(false)&y='a'", Namespace: "team;false", GOOS: "linux"}
	dir = t.TempDir()
	if _, err := ConnectCline(dir, o); err != nil {
		t.Fatal(err)
	}
	b, err := exec.Command(filepath.Join(dir, "TaskStart")).Output()
	if err != nil {
		t.Fatal(err)
	}
	want := "hook\n--from\ncline\n--url\n" + o.ServerURL + "\n--ns\n" + o.Namespace + "\n"
	if string(b) != want {
		t.Fatalf("args=%q want=%q", b, want)
	}
}

func TestClineMCPPathAndMerge(t *testing.T) {
	home := t.TempDir()
	for _, k := range []string{"CLINE_MCP_SETTINGS_PATH", "CLINE_DATA_DIR", "CLINE_DIR"} {
		t.Setenv(k, "")
	}
	if got := ClineMCPSettingsPath(home); got != filepath.Join(home, ".cline", "data", "settings", "cline_mcp_settings.json") {
		t.Fatal(got)
	}
	t.Setenv("CLINE_DIR", filepath.Join(home, "custom"))
	if got := ClineMCPSettingsPath(home); got != filepath.Join(home, "custom", "data", "settings", "cline_mcp_settings.json") {
		t.Fatal(got)
	}
	t.Setenv("CLINE_DATA_DIR", filepath.Join(home, "data"))
	if got := ClineMCPSettingsPath(home); got != filepath.Join(home, "data", "settings", "cline_mcp_settings.json") {
		t.Fatal(got)
	}
	p := filepath.Join(home, "mcp.json")
	t.Setenv("CLINE_MCP_SETTINGS_PATH", p)
	if got := ClineMCPSettingsPath(home); got != p {
		t.Fatal(got)
	}
	if err := os.WriteFile(p, []byte(`{"custom":1,"mcpServers":{"mine":{"command":"mine"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	o := MCPEntryOpts{ServerURL: "http://localhost:9090", APIKeyEnv: "TOKEN", Namespace: "team"}
	if changed, err := ConnectClineMCP(p, o, false); err != nil || !changed {
		t.Fatalf("MCP=%t %v", changed, err)
	}
	var cfg map[string]any
	raw, _ := os.ReadFile(p)
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	servers := cfg["mcpServers"].(map[string]any)
	entry := servers["punk"].(map[string]any)
	if servers["mine"] == nil || cfg["custom"] != float64(1) || entry["type"] != "streamableHttp" || entry["disabled"] != false || entry["headers"].(map[string]any)["Authorization"] != "Bearer ${env:TOKEN}" {
		t.Fatal(cfg)
	}
	if changed, err := ConnectClineMCP(p, o, false); err != nil || changed {
		t.Fatalf("MCP rerun=%t %v", changed, err)
	}
	if info, _ := os.Stat(p); info.Mode().Perm() != 0600 {
		t.Fatal("MCP permissions widened")
	}
	if err := os.WriteFile(p, []byte(`{"mcpServers":{"punk":{"command":"foreign"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectClineMCP(p, o, false); err == nil {
		t.Fatal("foreign MCP replaced")
	}
}
