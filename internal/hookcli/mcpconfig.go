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

// ConnectClaudeCodeMCP registers punk under mcpServers.punk in a Claude
// Code config file (~/.claude.json globally, .mcp.json per project).
// Same guarantees as ConnectClaudeCode: whole-file parse, refuse on wrong
// shapes, byte-identical no-op detection, atomic mode-preserving write.
// An existing punk entry that punk did not write is refused unless force.
func ConnectClaudeCodeMCP(configPath, serverURL string, force bool) (changed bool, err error) {
	cfg, existing, err := loadSettings(configPath)
	if err != nil {
		return false, err
	}
	var servers map[string]any
	if raw, ok := cfg["mcpServers"]; ok && raw != nil {
		servers, ok = raw.(map[string]any)
		if !ok {
			return false, fmt.Errorf("mcpServers is not an object; refusing to modify %s", configPath)
		}
	} else {
		servers = map[string]any{}
	}
	if prev, ok := servers["punk"]; ok && !isPunkMCPEntry(prev) && !force {
		return false, fmt.Errorf("%s already has an mcpServers.punk entry that punk did not write; rerun with --force to replace it", configPath)
	}
	servers["punk"] = map[string]any{"type": "http", "url": mcpEndpoint(serverURL)}
	cfg["mcpServers"] = servers
	out, err := encodeSettings(cfg)
	if err != nil {
		return false, err
	}
	if existing != nil && string(out) == string(existing) {
		return false, nil
	}
	if err := writePreservingSymlinkAndMode(configPath, out, 0o644); err != nil {
		return false, err
	}
	return true, nil
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
