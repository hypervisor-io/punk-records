package hookcli

import (
	"fmt"
	"strings"
)

// ClaudeMCPRule is the Claude Code permission rule that lets every punk
// tool run without a prompt. Scoped to the server, not to "*".
const ClaudeMCPRule = "mcp__punk"

// mcpEndpoint is the URL an agent session connects to: the lean agent
// toolset, selected per connection by the query string.
func mcpEndpoint(serverURL string) string {
	return strings.TrimRight(serverURL, "/") + "/mcp?toolset=agent"
}

// isPunkMCPEntry reports whether an mcpServers entry looks like one punk
// wrote: an http entry whose url path is /mcp.
func isPunkMCPEntry(entry any) bool {
	m, ok := entry.(map[string]any)
	if !ok {
		return false
	}
	u, _ := m["url"].(string)
	return m["type"] == "http" && strings.Contains(u, "/mcp")
}

// upsertServerEntry sets <section>.punk = entry in the JSON file at path,
// refusing to overwrite a foreign punk entry unless force. isOurs decides
// whether an existing entry was written by punk. seed is merged into a
// freshly created file (schema pointers and the like).
func upsertServerEntry(path, section string, entry map[string]any, isOurs func(any) bool, seed map[string]any, force bool) (bool, error) {
	cfg, existing, err := loadSettings(path)
	if err != nil {
		return false, err
	}
	if existing == nil {
		for k, v := range seed {
			cfg[k] = v
		}
	}
	var servers map[string]any
	if raw, ok := cfg[section]; ok && raw != nil {
		servers, ok = raw.(map[string]any)
		if !ok {
			return false, fmt.Errorf("%s is not an object; refusing to modify %s", section, path)
		}
	} else {
		servers = map[string]any{}
	}
	if prev, ok := servers["punk"]; ok && !isOurs(prev) && !force {
		return false, fmt.Errorf("%s already has a %s.punk entry that punk did not write; rerun with --force to replace it", path, section)
	}
	servers["punk"] = entry
	cfg[section] = servers
	out, err := encodeSettings(cfg)
	if err != nil {
		return false, err
	}
	if existing != nil && string(out) == string(existing) {
		return false, nil
	}
	if err := writePreservingSymlinkAndMode(path, out, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// MCPEntryOpts is everything an MCP server entry can carry: where the
// server is, how to authenticate, and optional per-project identity
// headers the server reads (X-Punk-Namespace, X-Punk-Agent).
type MCPEntryOpts struct {
	ServerURL string
	APIKey    string // literal token, written as-is
	APIKeyEnv string // when set, written as ${NAME} for hosts that expand env vars; wins over APIKey
	Namespace string
	Agent     string
}

func mcpHeaders(o MCPEntryOpts) map[string]any {
	h := map[string]any{}
	switch {
	case o.APIKeyEnv != "":
		h["Authorization"] = "Bearer ${" + o.APIKeyEnv + "}"
	case o.APIKey != "":
		h["Authorization"] = "Bearer " + o.APIKey
	}
	if o.Namespace != "" {
		h["X-Punk-Namespace"] = o.Namespace
	}
	if o.Agent != "" {
		h["X-Punk-Agent"] = o.Agent
	}
	if len(h) == 0 {
		return nil
	}
	return h
}

func withHeaders(entry map[string]any, o MCPEntryOpts) map[string]any {
	if h := mcpHeaders(o); h != nil {
		entry["headers"] = h
	}
	return entry
}

// ConnectClaudeCodeMCP registers punk under mcpServers.punk in a Claude
// Code config file (~/.claude.json globally, .mcp.json per project).
// Same guarantees as ConnectClaudeCode: whole-file parse, refuse on wrong
// shapes, byte-identical no-op detection, atomic mode-preserving write.
// An existing punk entry that punk did not write is refused unless force.
func ConnectClaudeCodeMCP(configPath string, o MCPEntryOpts, force bool) (bool, error) {
	return upsertServerEntry(configPath, "mcpServers",
		withHeaders(map[string]any{"type": "http", "url": mcpEndpoint(o.ServerURL)}, o), isPunkMCPEntry, nil, force)
}

// ConnectCursorMCP registers punk in a Cursor mcp.json ({"mcpServers":{"punk":{"url":...}}}).
func ConnectCursorMCP(mcpPath string, o MCPEntryOpts, force bool) (bool, error) {
	ours := func(e any) bool {
		m, ok := e.(map[string]any)
		if !ok {
			return false
		}
		u, _ := m["url"].(string)
		return strings.Contains(u, "/mcp")
	}
	return upsertServerEntry(mcpPath, "mcpServers", withHeaders(map[string]any{"url": mcpEndpoint(o.ServerURL)}, o), ours, nil, force)
}

// ConnectOpenCodeMCP registers punk in an opencode.json ({"mcp":{"punk":{"type":"remote","url":...,"enabled":true}}}).
func ConnectOpenCodeMCP(configPath string, o MCPEntryOpts, force bool) (bool, error) {
	ours := func(e any) bool {
		m, ok := e.(map[string]any)
		if !ok {
			return false
		}
		u, _ := m["url"].(string)
		return m["type"] == "remote" && strings.Contains(u, "/mcp")
	}
	return upsertServerEntry(configPath, "mcp",
		withHeaders(map[string]any{"type": "remote", "url": mcpEndpoint(o.ServerURL), "enabled": true}, o),
		ours, map[string]any{"$schema": "https://opencode.ai/config.json"}, force)
}

// EnsureClaudePermission appends rule to permissions.allow in a Claude
// Code settings.json when it is not already present.
func EnsureClaudePermission(settingsPath, rule string) (changed bool, err error) {
	settings, existing, err := loadSettings(settingsPath)
	if err != nil {
		return false, err
	}
	var perms map[string]any
	if raw, ok := settings["permissions"]; ok && raw != nil {
		perms, ok = raw.(map[string]any)
		if !ok {
			return false, fmt.Errorf("permissions is not an object; refusing to modify %s", settingsPath)
		}
	} else {
		perms = map[string]any{}
	}
	var allow []any
	if raw, ok := perms["allow"]; ok && raw != nil {
		allow, ok = raw.([]any)
		if !ok {
			return false, fmt.Errorf("permissions.allow is not an array; refusing to modify %s", settingsPath)
		}
	}
	for _, r := range allow {
		if r == rule {
			return false, nil
		}
	}
	perms["allow"] = append(allow, rule)
	settings["permissions"] = perms
	out, err := encodeSettings(settings)
	if err != nil {
		return false, err
	}
	if existing != nil && string(out) == string(existing) {
		return false, nil
	}
	if err := writePreservingSymlinkAndMode(settingsPath, out, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// ConnectCopilotMCP registers punk in a Copilot CLI mcp-config.json:
// {"mcpServers":{"punk":{"type":"http","url":...,"headers":{...},"tools":["*"]}}}.
func ConnectCopilotMCP(configPath string, o MCPEntryOpts, force bool) (bool, error) {
	entry := withHeaders(map[string]any{"type": "http", "url": mcpEndpoint(o.ServerURL), "tools": []any{"*"}}, o)
	return upsertServerEntry(configPath, "mcpServers", entry, isPunkMCPEntry, nil, force)
}

// ConnectAntigravityMCP registers punk in an Antigravity mcp_config.json.
// Antigravity's remote entries use "serverUrl" (its docs state the legacy
// "url" and "httpUrl" keys are not supported), so that is the only URL
// key written.
func ConnectAntigravityMCP(configPath string, o MCPEntryOpts, force bool) (bool, error) {
	ours := func(e any) bool {
		m, ok := e.(map[string]any)
		if !ok {
			return false
		}
		u, _ := m["serverUrl"].(string)
		return strings.Contains(u, "/mcp")
	}
	entry := withHeaders(map[string]any{"serverUrl": mcpEndpoint(o.ServerURL)}, o)
	return upsertServerEntry(configPath, "mcpServers", entry, ours, nil, force)
}
