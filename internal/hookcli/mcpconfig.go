package hookcli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
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
	APIKey    string // literal token, written as-is unless ProjectScoped
	APIKeyEnv string // when set, written in the target host's env-reference syntax; wins over APIKey
	Namespace string
	Agent     string

	// ProjectScoped marks a target config that lives INSIDE the user's
	// repository (the --project scope: .mcp.json, ./.cursor/mcp.json,
	// ./opencode.json, ./.agents/mcp_config.json, ./.copilot/mcp-config.json,
	// ./.codex/config.toml). Such a file is routinely committed, so a
	// literal token must never be embedded in it: a literal key degrades to
	// the host's env-var reference (defaultMCPKeyEnv) and the file is never
	// tightened to owner-only for a token it does not hold.
	ProjectScoped bool
}

// defaultMCPKeyEnv is the environment variable a project-scoped config
// references when punk refuses to embed the selected literal key.
const defaultMCPKeyEnv = "PUNK_API_KEY"

// RepoScopedTarget reports whether path is a target config that lives
// inside a git work tree and is NOT gitignored - a file the user's
// repository may commit, so the project-scoped credential rule
// (MCPEntryOpts.ProjectScoped) has to apply even though the caller has no
// --project flag to say so. Hermes (--config) and OpenClaw (--home) take
// arbitrary paths, so they ask git instead of a flag.
//
// Failure-tolerant by design: a missing git binary, no repository, an
// unreadable directory or any git error means "not repo-scoped" - the
// conservative direction, because it leaves the ordinary global behaviour
// (embed the key, write the file owner-only) untouched. Only a definitive
// "inside a work tree AND not ignored" flips it.
func RepoScopedTarget(path string) bool {
	if path == "" {
		return false
	}
	// The target's own directory may not exist yet (a fresh --home or
	// --config), so probe from the nearest existing ancestor: git can only
	// be run from a directory that is there.
	dir := nearestExistingDir(filepath.Dir(path))
	if dir == "" {
		return false
	}
	// Inside a work tree? git exits non-zero for "not a git repository" and
	// for a missing binary, both of which mean no.
	if err := runGitQuiet(dir, "rev-parse", "--show-toplevel"); err != nil {
		return false
	}
	// A gitignored file is never committed, so it is the ordinary global
	// case. check-ignore exits 0 when the path IS ignored and 1 when it is
	// not; any other exit is an error, not a verdict, and stays "not
	// repo-scoped". The path is passed absolute so the answer cannot depend
	// on how it was spelled relative to dir.
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	switch err := runGitQuiet(dir, "check-ignore", "-q", "--", abs); {
	case err == nil:
		return false // ignored: never committed
	case gitExitCode(err) == 1:
		return true // inside the work tree and not ignored
	default:
		return false
	}
}

// nearestExistingDir returns dir itself when it is an existing directory,
// else the closest existing ancestor, else "".
func nearestExistingDir(dir string) string {
	for {
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// runGitQuiet runs one git probe with both streams discarded: the guard
// only cares about the exit status, and git's diagnostics must never reach
// the user's terminal as noise.
func runGitQuiet(dir string, args ...string) error {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run()
}

// gitExitCode returns the process exit status, or -1 when err is not an
// ExitError (a missing binary, for instance).
func gitExitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// mcpKeyEnv is the ONE rule behind "never embed a literal key in a
// project-scoped config": it returns the environment variable name an
// entry's Authorization header must reference, and true, whenever the
// credential has to be an env reference rather than a literal. An explicit
// --api-key-env always wins (the operator chose it); otherwise a
// project-scoped entry degrades a literal key to defaultMCPKeyEnv.
// Callers write the returned name in their own host's env syntax.
func mcpKeyEnv(o MCPEntryOpts) (string, bool) {
	if o.APIKeyEnv != "" {
		return o.APIKeyEnv, true
	}
	if o.ProjectScoped && o.APIKey != "" {
		return defaultMCPKeyEnv, true
	}
	return "", false
}

// hasLiteralMCPToken reports whether this entry will actually carry the
// literal token, which is what decides whether the written file must be
// owner-only. A project-scoped entry never does.
func hasLiteralMCPToken(o MCPEntryOpts) bool {
	return !o.ProjectScoped && o.APIKeyEnv == "" && o.APIKey != ""
}

func mcpHeaders(o MCPEntryOpts) map[string]any {
	h := map[string]any{}
	if env, ok := mcpKeyEnv(o); ok {
		h["Authorization"] = "Bearer ${" + env + "}"
	} else if o.APIKey != "" {
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
	if env, ok := mcpKeyEnv(o); ok {
		entry["headers"].(map[string]any)["Authorization"] = "Bearer ${env:" + env + "}"
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
	if env, ok := mcpKeyEnv(o); ok {
		entry["headers"].(map[string]any)["Authorization"] = "Bearer {env:" + env + "}"
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

// openCodeIdleTimeoutKey is OpenCode's per-directory unload timer
// (experimental.location_idle_timeout, default "60 minutes" in 2.2.0's
// packages/core/src/location-activity.ts). OpenCode 2.x runs plugins
// inside per-directory location services and evicts them after that much
// time without session activity; a headless "opencode serve" with no TUI
// touching the directory then unloads the punk plugin and its inbox
// stream, so mail stops waking the session.
const openCodeIdleTimeoutKey = "location_idle_timeout"

// EnsureOpenCodeLocationIdleTimeout sets experimental.location_idle_timeout
// to false in an OpenCode opencode.json or opencode.jsonc so the punk
// plugin stays loaded on an idle server. A value the user already set -
// any value, including a duration - is left alone, as is a non-object
// experimental setting. JSONC comments, trailing commas and foreign keys
// survive: the member is inserted into the existing text, never
// re-encoded. A missing or empty file gets a minimal config.
func EnsureOpenCodeLocationIdleTimeout(configPath string) (bool, error) {
	cfg, original, normalized, err := loadOpenCodeSettings(configPath)
	if err != nil {
		return false, err
	}
	if original == nil || len(strings.TrimSpace(string(original))) == 0 {
		out, err := json.MarshalIndent(map[string]any{
			"$schema":      "https://opencode.ai/config.json",
			"experimental": map[string]any{openCodeIdleTimeoutKey: false},
		}, "", "  ")
		if err != nil {
			return false, err
		}
		return true, writePreservingSymlinkAndMode(configPath, append(out, '\n'), 0o644)
	}
	var out []byte
	switch exp, ok := cfg["experimental"]; {
	case !ok || exp == nil:
		if ok { // "experimental": null - replace the null
			cur := gjson.GetBytes(normalized, "experimental")
			start, end := cur.Index, cur.Index+len(cur.Raw)
			if len(cur.Raw) == 0 || start < 0 || end > len(original) {
				return false, errors.New("invalid OpenCode configuration")
			}
			out = append(append(append([]byte{}, original[:start]...), `{"`+openCodeIdleTimeoutKey+`": false}`...), original[end:]...)
		} else {
			out, err = jsoncObjectInsert(original, normalized, gjson.ParseBytes(normalized), "experimental",
				[]byte(`{"`+openCodeIdleTimeoutKey+`": false}`))
		}
	default:
		m, isObj := exp.(map[string]any)
		if !isObj {
			return false, nil // not ours to rewrite
		}
		if _, set := m[openCodeIdleTimeoutKey]; set {
			return false, nil // the user's choice stands
		}
		out, err = jsoncObjectInsert(original, normalized, gjson.GetBytes(normalized, "experimental"), openCodeIdleTimeoutKey, []byte("false"))
	}
	if err != nil {
		return false, fmt.Errorf("merge OpenCode experimental setting: %w", err)
	}
	return true, writePreservingSymlinkAndMode(configPath, out, 0o644)
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
