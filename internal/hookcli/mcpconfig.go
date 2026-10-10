package hookcli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/pretty"
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
func upsertServerEntry(path, section string, entry map[string]any, isOurs func(any) bool, seed map[string]any, force, private bool) (bool, error) {
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
	if existing != nil && string(out) == string(existing) && (!private || !privateModeNeedsRepair(path)) {
		return false, nil
	}
	var writeErr error
	if private {
		writeErr = writePrivatePreservingSymlinkAndMode(path, out)
	} else {
		writeErr = writePreservingSymlinkAndMode(path, out, 0o644)
	}
	if writeErr != nil {
		return false, writeErr
	}
	return true, nil
}

// MCPEntryOpts is everything an MCP server entry can carry: where the
// server is, how to authenticate, and optional per-project identity
// headers the server reads (X-Punk-Namespace, X-Punk-Agent).
type MCPEntryOpts struct {
	ServerURL string
	APIKey    string // literal token, written as-is
	APIKeyEnv string // when set, written in the target host's env-reference syntax; wins over APIKey
	Namespace string
	Agent     string
}

func hasLiteralMCPToken(o MCPEntryOpts) bool {
	return o.APIKeyEnv == "" && o.APIKey != ""
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
		withHeaders(map[string]any{"type": "http", "url": mcpEndpoint(o.ServerURL)}, o), isPunkMCPEntry, nil, force, hasLiteralMCPToken(o))
}

// ConnectClineMCP uses the extension's explicit streamableHttp transport and
// ${env:NAME} expansion syntax, not the default SSE or other hosts' ${NAME}.
func ConnectClineMCP(configPath string, o MCPEntryOpts, force bool) (bool, error) {
	ours := func(e any) bool {
		m, ok := e.(map[string]any)
		if !ok {
			return false
		}
		u, _ := m["url"].(string)
		return m["type"] == "streamableHttp" && strings.Contains(u, "/mcp")
	}
	entry := withHeaders(map[string]any{"type": "streamableHttp", "url": mcpEndpoint(o.ServerURL), "disabled": false, "autoApprove": []string{}}, o)
	if o.APIKeyEnv != "" {
		entry["headers"].(map[string]any)["Authorization"] = "Bearer ${env:" + o.APIKeyEnv + "}"
	}
	return upsertServerEntry(configPath, "mcpServers", entry, ours, nil, force, hasLiteralMCPToken(o))
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
	return upsertServerEntry(mcpPath, "mcpServers", withHeaders(map[string]any{"url": mcpEndpoint(o.ServerURL)}, o), ours, nil, force, hasLiteralMCPToken(o))
}

func isPunkOpenCodeMCPEntry(e any) bool {
	m, ok := e.(map[string]any)
	if !ok {
		return false
	}
	u, _ := m["url"].(string)
	return m["type"] == "remote" && strings.Contains(u, "/mcp")
}

// ResolveOpenCodeConfigPath adopts the one existing opencode.jsonc or
// opencode.json at a scope. Both is ambiguous and is refused before writes.
func ResolveOpenCodeConfigPath(defaultJSONPath string) (string, error) {
	// A caller that already passes the .jsonc form must not collide with
	// itself: deriving both candidates from one base keeps the two paths
	// distinct, so the ambiguity check below can never compare a path to
	// itself and report a false "both exist".
	jsonPath := defaultJSONPath
	if strings.EqualFold(filepath.Ext(defaultJSONPath), ".jsonc") {
		jsonPath = strings.TrimSuffix(defaultJSONPath, filepath.Ext(defaultJSONPath)) + ".json"
	}
	jsoncPath := strings.TrimSuffix(jsonPath, filepath.Ext(jsonPath)) + ".jsonc"
	exists := func(path string) (bool, error) {
		_, err := os.Lstat(path)
		if err == nil {
			return true, nil
		}
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, errors.New("cannot inspect OpenCode configuration")
	}
	hasJSON, err := exists(jsonPath)
	if err != nil {
		return "", err
	}
	hasJSONC, err := exists(jsoncPath)
	if err != nil {
		return "", err
	}
	if hasJSON && hasJSONC {
		return "", errors.New("both opencode.json and opencode.jsonc exist at the selected scope; refusing ambiguous configuration")
	}
	if hasJSONC {
		return jsoncPath, nil
	}
	return jsonPath, nil
}

func loadOpenCodeSettings(path string) (map[string]any, []byte, []byte, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if _, linkErr := os.Lstat(path); linkErr == nil {
			return nil, nil, nil, errors.New("cannot read OpenCode configuration")
		}
		return map[string]any{}, nil, nil, nil
	}
	if err != nil {
		return nil, nil, nil, errors.New("cannot read OpenCode configuration")
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return map[string]any{}, raw, raw, nil
	}
	normalized := raw
	if filepath.Ext(path) == ".jsonc" {
		normalized = pretty.Spec(raw)
	}
	dec := json.NewDecoder(bytes.NewReader(normalized))
	dec.UseNumber()
	var cfg map[string]any
	if err := dec.Decode(&cfg); err != nil || cfg == nil {
		return nil, nil, nil, errors.New("invalid OpenCode configuration")
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, nil, nil, errors.New("invalid OpenCode configuration")
	}
	return cfg, raw, normalized, nil
}

// PreflightOpenCodeMCP validates the selected config and ownership without
// writing it. The CLI runs this before replacing its managed plugin.
func PreflightOpenCodeMCP(configPath string, force bool) error {
	cfg, _, _, err := loadOpenCodeSettings(configPath)
	if err != nil {
		return err
	}
	if raw, ok := cfg["mcp"]; ok && raw != nil {
		mcp, ok := raw.(map[string]any)
		if !ok {
			return errors.New("OpenCode mcp setting is not an object; refusing to modify configuration")
		}
		if prev, ok := mcp["punk"]; ok && !isPunkOpenCodeMCPEntry(prev) && !force {
			return errors.New("OpenCode already has a foreign mcp.punk entry; rerun with --force to replace it")
		}
	}
	return nil
}

func jsoncObjectInsert(original, normalized []byte, object gjson.Result, key string, value []byte) ([]byte, error) {
	start := object.Index
	end := start + len(object.Raw)
	if start < 0 || end > len(original) || end <= start {
		return nil, errors.New("invalid OpenCode configuration object span")
	}
	closeAt := end - 1
	for closeAt > start && normalized[closeAt] <= ' ' {
		closeAt--
	}
	if normalized[closeAt] != '}' {
		return nil, errors.New("invalid OpenCode configuration object boundary")
	}
	last := closeAt - 1
	for last > start && normalized[last] <= ' ' {
		last--
	}
	prefix := ""
	if normalized[last] != '{' {
		hasTrailingComma := false
		for i := last + 1; i < closeAt; i++ {
			if original[i] == ',' && normalized[i] == ' ' {
				hasTrailingComma = true
				break
			}
		}
		if !hasTrailingComma {
			prefix = ","
		}
	}
	member := []byte(prefix + "\n    " + strconvQuote(key) + ": ")
	member = append(member, value...)
	out := make([]byte, 0, len(original)+len(member))
	out = append(out, original[:closeAt]...)
	out = append(out, member...)
	out = append(out, original[closeAt:]...)
	return out, nil
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func connectOpenCodeMCPJSONC(configPath string, o MCPEntryOpts, entry map[string]any, force bool) (bool, error) {
	cfg, original, normalized, err := loadOpenCodeSettings(configPath)
	if err != nil {
		return false, fmt.Errorf("load OpenCode JSONC: %w", err)
	}
	if err := PreflightOpenCodeMCP(configPath, force); err != nil {
		return false, fmt.Errorf("preflight OpenCode JSONC: %w", err)
	}
	if original == nil || len(strings.TrimSpace(string(original))) == 0 {
		return upsertServerEntry(configPath, "mcp", entry, isPunkOpenCodeMCPEntry,
			map[string]any{"$schema": "https://opencode.ai/config.json"}, force, hasLiteralMCPToken(o))
	}
	entryRaw, err := json.Marshal(entry)
	if err != nil {
		return false, fmt.Errorf("merge OpenCode JSONC: %w", err)
	}
	out := original
	punk := gjson.GetBytes(normalized, "mcp.punk")
	if punk.Exists() {
		start, end := punk.Index, punk.Index+len(punk.Raw)
		out = append(append(append([]byte{}, original[:start]...), entryRaw...), original[end:]...)
	} else if currentMCP, ok := cfg["mcp"]; ok && currentMCP != nil {
		out, err = jsoncObjectInsert(original, normalized, gjson.GetBytes(normalized, "mcp"), "punk", entryRaw)
	} else if ok {
		mcpRaw, marshalErr := json.Marshal(map[string]any{"punk": entry})
		if marshalErr != nil {
			return false, marshalErr
		}
		current := gjson.GetBytes(normalized, "mcp")
		start, end := current.Index, current.Index+len(current.Raw)
		if len(current.Raw) == 0 || start < 0 || end > len(original) {
			return false, errors.New("invalid OpenCode configuration")
		}
		out = append(append(append([]byte{}, original[:start]...), mcpRaw...), original[end:]...)
	} else {
		mcpRaw, marshalErr := json.Marshal(map[string]any{"punk": entry})
		if marshalErr != nil {
			return false, marshalErr
		}
		out, err = jsoncObjectInsert(original, normalized, gjson.ParseBytes(normalized), "mcp", mcpRaw)
	}
	if err != nil {
		return false, fmt.Errorf("merge OpenCode JSONC: %w", err)
	}
	if bytes.Equal(out, original) && (!hasLiteralMCPToken(o) || !privateModeNeedsRepair(configPath)) {
		return false, nil
	}
	if hasLiteralMCPToken(o) {
		err = writePrivatePreservingSymlinkAndMode(configPath, out)
	} else {
		err = writePreservingSymlinkAndMode(configPath, out, 0o644)
	}
	return err == nil, err
}

// ConnectOpenCodeMCP registers punk in opencode.json or opencode.jsonc while
// preserving JSONC comments/trailing commas and foreign configuration.
// codeops:trace repo=punk-records work_item=punk-connect-remote-url-review-20261009 spec=docs/client-credentials.md plan=phase-1/task-1-A test=internal/hookcli/mcpconfig_opencode_native_test.go evidence=docs/superpowers/reports/2026-10-09-client-credentials/builder-a.md
func ConnectOpenCodeMCP(configPath string, o MCPEntryOpts, force bool) (bool, error) {
	entry := withHeaders(map[string]any{"type": "remote", "url": mcpEndpoint(o.ServerURL), "enabled": true}, o)
	// codeops:trace repo=punk-records work_item=punk-agent-refresh-20261009 spec=docs/CONFIG.md plan=client-auth test=TestConnectOpenCodeMCPUsesOpenCodeEnvSyntax
	if o.APIKeyEnv != "" {
		entry["headers"].(map[string]any)["Authorization"] = "Bearer {env:" + o.APIKeyEnv + "}"
	}
	if err := PreflightOpenCodeMCP(configPath, force); err != nil {
		return false, err
	}
	if filepath.Ext(configPath) == ".jsonc" {
		return connectOpenCodeMCPJSONC(configPath, o, entry, force)
	}
	return upsertServerEntry(configPath, "mcp", entry,
		isPunkOpenCodeMCPEntry, map[string]any{"$schema": "https://opencode.ai/config.json"}, force, hasLiteralMCPToken(o))
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
	return upsertServerEntry(configPath, "mcpServers", entry, isPunkMCPEntry, nil, force, hasLiteralMCPToken(o))
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
	return upsertServerEntry(configPath, "mcpServers", entry, ours, nil, force, hasLiteralMCPToken(o))
}

// ConnectOpenClawMCP registers punk under mcp.servers.punk in OpenClaw's
// config.json as a streamable-http remote server.
func ConnectOpenClawMCP(configPath string, o MCPEntryOpts, force bool) (bool, error) {
	settings, existing, err := loadSettings(configPath)
	if err != nil {
		return false, err
	}
	mcpObj, err := childObject(settings, "mcp", "mcp", configPath)
	if err != nil {
		return false, err
	}
	servers, err := childObject(mcpObj, "servers", "mcp.servers", configPath)
	if err != nil {
		return false, err
	}
	if prev, ok := servers["punk"]; ok && !force {
		m, isMap := prev.(map[string]any)
		u, _ := m["url"].(string)
		if !isMap || m["transport"] != "streamable-http" || !strings.Contains(u, "/mcp") {
			return false, fmt.Errorf("%s already has an mcp.servers.punk entry that punk did not write; rerun with --force to replace it", configPath)
		}
	}
	servers["punk"] = withHeaders(map[string]any{"url": mcpEndpoint(o.ServerURL), "transport": "streamable-http"}, o)
	out, err := encodeSettings(settings)
	if err != nil {
		return false, err
	}
	private := hasLiteralMCPToken(o)
	if existing != nil && string(out) == string(existing) && (!private || !privateModeNeedsRepair(configPath)) {
		return false, nil
	}
	var writeErr error
	if private {
		writeErr = writePrivatePreservingSymlinkAndMode(configPath, out)
	} else {
		writeErr = writePreservingSymlinkAndMode(configPath, out, 0o644)
	}
	if writeErr != nil {
		return false, writeErr
	}
	return true, nil
}
