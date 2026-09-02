package hookcli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestConnectClaudeCodeMCPAddsEntryAndPreservesOthers(t *testing.T) {
	p := filepath.Join(t.TempDir(), "claude.json")
	if err := os.WriteFile(p, []byte(`{"numStartups": 42, "mcpServers": {"other": {"type":"stdio","command":"x"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := ConnectClaudeCodeMCP(p, MCPEntryOpts{ServerURL: "http://127.0.0.1:9090"}, false)
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	m := readSettings(t, p)
	servers := m["mcpServers"].(map[string]any)
	punk := servers["punk"].(map[string]any)
	if punk["type"] != "http" || punk["url"] != "http://127.0.0.1:9090/mcp?toolset=agent" {
		t.Fatalf("punk entry = %v", punk)
	}
	if _, ok := servers["other"]; !ok || m["numStartups"] == nil {
		t.Fatal("unrelated config must survive")
	}
	if info, _ := os.Stat(p); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode widened to %v", info.Mode().Perm())
	}
	if changed, err := ConnectClaudeCodeMCP(p, MCPEntryOpts{ServerURL: "http://127.0.0.1:9090"}, false); err != nil || changed {
		t.Fatalf("second run: changed=%v err=%v", changed, err)
	}
}

func TestConnectClaudeCodeMCPRefusesForeignEntry(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".mcp.json")
	if err := os.WriteFile(p, []byte(`{"mcpServers": {"punk": {"type":"stdio","command":"something-else"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectClaudeCodeMCP(p, MCPEntryOpts{ServerURL: "http://127.0.0.1:9090"}, false); err == nil {
		t.Fatal("must refuse to replace an entry punk did not write")
	}
	if changed, err := ConnectClaudeCodeMCP(p, MCPEntryOpts{ServerURL: "http://127.0.0.1:9090"}, true); err != nil || !changed {
		t.Fatalf("force: %v %v", changed, err)
	}
}

func TestConnectClaudeCodeMCPCreatesProjectFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".mcp.json")
	if _, err := ConnectClaudeCodeMCP(p, MCPEntryOpts{ServerURL: "http://localhost:9090"}, false); err != nil {
		t.Fatal(err)
	}
	m := readSettings(t, p)
	if _, ok := m["mcpServers"].(map[string]any)["punk"]; !ok {
		t.Fatalf("created file = %v", m)
	}
}

func TestEnsureClaudePermission(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(p, []byte(`{"permissions":{"allow":["Bash(ls)"],"deny":[]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := EnsureClaudePermission(p, ClaudeMCPRule)
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	m := readSettings(t, p)
	raw, _ := json.Marshal(m["permissions"].(map[string]any)["allow"])
	if string(raw) != `["Bash(ls)","mcp__punk"]` {
		t.Fatalf("allow = %s", raw)
	}
	if changed, _ := EnsureClaudePermission(p, ClaudeMCPRule); changed {
		t.Fatal("must be idempotent")
	}
	// missing permissions object is created
	p2 := filepath.Join(t.TempDir(), "settings.json")
	if _, err := EnsureClaudePermission(p2, ClaudeMCPRule); err != nil {
		t.Fatal(err)
	}
	if raw, _ := json.Marshal(readSettings(t, p2)["permissions"]); string(raw) != `{"allow":["mcp__punk"]}` {
		t.Fatalf("created permissions = %s", raw)
	}
}

func TestConnectCursorMCP(t *testing.T) {
	p := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(p, []byte(`{"mcpServers":{"gh":{"command":"gh-mcp"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if changed, err := ConnectCursorMCP(p, MCPEntryOpts{ServerURL: "http://localhost:9090"}, false); err != nil || !changed {
		t.Fatal(changed, err)
	}
	m := readSettings(t, p)["mcpServers"].(map[string]any)
	if m["punk"].(map[string]any)["url"] != "http://localhost:9090/mcp?toolset=agent" || m["gh"] == nil {
		t.Fatalf("cursor mcp.json = %v", m)
	}
	if changed, _ := ConnectCursorMCP(p, MCPEntryOpts{ServerURL: "http://localhost:9090"}, false); changed {
		t.Fatal("idempotent")
	}
}

func TestConnectOpenCodeMCP(t *testing.T) {
	p := filepath.Join(t.TempDir(), "opencode.json")
	if changed, err := ConnectOpenCodeMCP(p, MCPEntryOpts{ServerURL: "http://localhost:9090"}, false); err != nil || !changed {
		t.Fatal(changed, err)
	}
	m := readSettings(t, p)
	if m["$schema"] != "https://opencode.ai/config.json" {
		t.Fatalf("created file must carry the schema: %v", m)
	}
	punk := m["mcp"].(map[string]any)["punk"].(map[string]any)
	if punk["type"] != "remote" || punk["enabled"] != true || punk["url"] != "http://localhost:9090/mcp?toolset=agent" {
		t.Fatalf("opencode entry = %v", punk)
	}
	if err := os.WriteFile(p, []byte(`{"mcp":{"punk":{"type":"local","command":["node","x"]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectOpenCodeMCP(p, MCPEntryOpts{ServerURL: "http://localhost:9090"}, false); err == nil {
		t.Fatal("foreign entry must be refused without force")
	}
}

func TestMCPEntriesCarryHeaders(t *testing.T) {
	o := MCPEntryOpts{ServerURL: "https://punk.example.com", APIKey: "prk_x", Namespace: "agent-billing-1a2b3c", Agent: "alice@laptop"}
	h := mcpHeaders(o)
	if h["Authorization"] != "Bearer prk_x" || h["X-Punk-Namespace"] != "agent-billing-1a2b3c" || h["X-Punk-Agent"] != "alice@laptop" {
		t.Fatalf("headers = %v", h)
	}
	if mcpHeaders(MCPEntryOpts{ServerURL: "http://localhost:9090"}) != nil {
		t.Fatal("no credentials, no headers")
	}
	if h := mcpHeaders(MCPEntryOpts{APIKeyEnv: "PUNK_API_KEY"}); h["Authorization"] != "Bearer ${PUNK_API_KEY}" {
		t.Fatalf("env form = %v", h)
	}

	p := filepath.Join(t.TempDir(), ".mcp.json")
	if _, err := ConnectClaudeCodeMCP(p, o, false); err != nil {
		t.Fatal(err)
	}
	entry := readSettings(t, p)["mcpServers"].(map[string]any)["punk"].(map[string]any)
	if entry["headers"].(map[string]any)["Authorization"] != "Bearer prk_x" {
		t.Fatalf("claude entry = %v", entry)
	}
	pc := filepath.Join(t.TempDir(), "mcp.json")
	if _, err := ConnectCursorMCP(pc, o, false); err != nil {
		t.Fatal(err)
	}
	if readSettings(t, pc)["mcpServers"].(map[string]any)["punk"].(map[string]any)["headers"] == nil {
		t.Fatal("cursor entry missing headers")
	}
	po := filepath.Join(t.TempDir(), "opencode.json")
	if _, err := ConnectOpenCodeMCP(po, o, false); err != nil {
		t.Fatal(err)
	}
	if readSettings(t, po)["mcp"].(map[string]any)["punk"].(map[string]any)["headers"] == nil {
		t.Fatal("opencode entry missing headers")
	}
}
