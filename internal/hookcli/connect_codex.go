package hookcli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// codexHookEvents are the Codex hook events punk captures from. Codex's
// hooks.json is shaped like Claude Code's settings.json "hooks" object
// and its stdin payloads carry the same field names, so the Claude merge
// and the Claude passthrough are reused rather than re-implemented.
var codexHookEvents = []string{"SessionStart", "UserPromptSubmit", "PostToolUse", "Stop"}

// codexSessionStartMatcher limits injection to fresh and resumed
// sessions; Codex also fires SessionStart on clear and compact, where
// re-injecting the project block would be noise mid-session.
const codexSessionStartMatcher = "startup|resume"

// ConnectCodexHooks merges punk hook entries into a Codex hooks.json
// (~/.codex/hooks.json, or <repo>/.codex/hooks.json for --project).
func ConnectCodexHooks(hooksPath, punkPath, serverURL, ns string) (changed bool, err error) {
	settings, existing, err := loadSettings(hooksPath)
	if err != nil {
		return false, err
	}
	var hooksAny map[string]any
	if raw, ok := settings["hooks"]; ok && raw != nil {
		hooksAny, ok = raw.(map[string]any)
		if !ok {
			return false, fmt.Errorf("hooks is not an object; refusing to modify %s", hooksPath)
		}
	} else {
		hooksAny = map[string]any{}
	}
	command := punkHookCommandFrom(punkPath, serverURL, ns, "codex")
	for _, ev := range codexHookEvents {
		if raw, ok := hooksAny[ev]; ok && raw != nil {
			if _, isArr := raw.([]any); !isArr {
				return false, fmt.Errorf("hooks.%s is not an array; refusing to modify %s", ev, hooksPath)
			}
		}
		groups := mergeEventGroups(hooksAny[ev], ev, punkPath, command)
		if ev == "SessionStart" {
			setPunkGroupMatcher(groups, punkPath, codexSessionStartMatcher)
		}
		hooksAny[ev] = groups
	}
	settings["hooks"] = hooksAny
	out, err := encodeSettings(settings)
	if err != nil {
		return false, err
	}
	if existing != nil && string(out) == string(existing) {
		return false, nil
	}
	if err := writePreservingSymlinkAndMode(hooksPath, out, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// setPunkGroupMatcher stamps matcher onto the punk-managed group in groups.
func setPunkGroupMatcher(groups []any, punkPath, matcher string) {
	for _, g := range groups {
		if gm, ok := g.(map[string]any); ok && isPunkManagedGroup(gm, punkPath) {
			gm["matcher"] = matcher
		}
	}
}

const (
	codexBlockStart = "# punk-managed-start (punk connect codex; edit outside this block only)"
	codexBlockEnd   = "# punk-managed-end"
	codexPunkTable  = "[mcp_servers.punk]"
)

var (
	tomlHeaderRe    = regexp.MustCompile(`(?m)^\s*\[[^\]]+\]\s*$`)
	featuresHdrRe   = regexp.MustCompile(`(?m)^\s*\[features\]\s*$`)
	featuresHooksRe = regexp.MustCompile(`(?m)^\s*hooks\s*=\s*(true|false)\s*$`)
)

// tomlString quotes s as a TOML basic string.
func tomlString(s string) string {
	b, _ := json.Marshal(s) // JSON and TOML basic strings share escapes for everything punk writes
	return string(b)
}

// codexManagedBlock renders the punk block for config.toml.
func codexManagedBlock(o MCPEntryOpts, includeFeatures bool) string {
	var b strings.Builder
	b.WriteString(codexBlockStart + "\n")
	if includeFeatures {
		b.WriteString("[features]\nhooks = true\n\n")
	}
	b.WriteString(codexPunkTable + "\n")
	b.WriteString("url = " + tomlString(mcpEndpoint(o.ServerURL)) + "\n")
	switch {
	case o.APIKeyEnv != "":
		b.WriteString("bearer_token_env_var = " + tomlString(o.APIKeyEnv) + "\n")
	case o.APIKey != "":
		b.WriteString("bearer_token_env_var = \"PUNK_API_KEY\"\n")
	}
	var hdrs []string
	if o.Namespace != "" {
		hdrs = append(hdrs, tomlString("X-Punk-Namespace")+" = "+tomlString(o.Namespace))
	}
	if o.Agent != "" {
		hdrs = append(hdrs, tomlString("X-Punk-Agent")+" = "+tomlString(o.Agent))
	}
	if len(hdrs) > 0 {
		b.WriteString("http_headers = { " + strings.Join(hdrs, ", ") + " }\n")
	}
	b.WriteString("startup_timeout_sec = 10\ntool_timeout_sec = 60\ndefault_tools_approval_mode = \"approve\"\n")
	b.WriteString(codexBlockEnd + "\n")
	return b.String()
}

// stripManagedBlock removes a previous punk block, returning the rest.
func stripManagedBlock(s string) string {
	i := strings.Index(s, codexBlockStart)
	if i < 0 {
		return s
	}
	j := strings.Index(s[i:], codexBlockEnd)
	if j < 0 {
		return s[:i]
	}
	end := i + j + len(codexBlockEnd)
	if end < len(s) && s[end] == '\n' {
		end++
	}
	return s[:i] + s[end:]
}

// removeTable deletes the TOML table whose header is exactly hdr, from
// the header line through the line before the next header (or EOF).
func removeTable(s, hdr string) string {
	lines := strings.Split(s, "\n")
	out := lines[:0]
	skipping := false
	for _, ln := range lines {
		trim := strings.TrimSpace(ln)
		if trim == hdr {
			skipping = true
			continue
		}
		if skipping && tomlHeaderRe.MatchString(ln) {
			skipping = false
		}
		if !skipping {
			out = append(out, ln)
		}
	}
	return strings.Join(out, "\n")
}

// ConnectCodexConfig writes the punk MCP entry (and the hooks feature
// flag) into a Codex config.toml as a marker-fenced block, preserving
// everything else byte for byte.
func ConnectCodexConfig(configPath string, o MCPEntryOpts, enableHooks, force bool) (changed bool, err error) {
	raw, readErr := os.ReadFile(configPath)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return false, readErr
	}
	existing := string(raw)
	body := stripManagedBlock(existing)

	if strings.Contains(body, codexPunkTable) {
		if !force {
			return false, fmt.Errorf("%s already has a %s table that punk did not write; rerun with --force to replace it", configPath, codexPunkTable)
		}
		body = removeTable(body, codexPunkTable)
	}

	includeFeatures := false
	if enableHooks {
		if loc := featuresHdrRe.FindStringIndex(body); loc != nil {
			// Scope the search to the [features] table: from its header to the next header.
			rest := body[loc[1]:]
			next := len(rest)
			if nl := tomlHeaderRe.FindStringIndex(rest); nl != nil {
				next = nl[0]
			}
			table := rest[:next]
			if m := featuresHooksRe.FindStringSubmatch(table); m != nil {
				if m[1] == "false" {
					return false, fmt.Errorf("%s sets hooks = false under [features]; punk hooks need hooks = true, change it by hand", configPath)
				}
			} else {
				body = body[:loc[1]] + "\nhooks = true" + body[loc[1]:]
			}
		} else {
			includeFeatures = true
		}
	}

	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	if body != "" && !strings.HasSuffix(body, "\n\n") {
		body += "\n"
	}
	out := body + codexManagedBlock(o, includeFeatures)
	if out == existing {
		return false, nil
	}
	if err := writePreservingSymlinkAndMode(configPath, []byte(out), 0o600); err != nil {
		return false, err
	}
	return true, nil
}
