package hookcli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
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

// stripManagedBlock removes every previous punk block, returning the
// rest. A config.toml can carry more than one (a paste accident, or a
// block written before the strip existed): they are all punk-managed, so
// all of them go - leaving a second [mcp_servers.punk] table behind would
// be exactly the duplicate-MCP-alias defect this package reconciles.
func stripManagedBlock(s string) string {
	for {
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
		s = s[:i] + s[end:]
	}
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
	// Codex runs on Windows too; edit in LF and restore the file's own
	// line endings on the way out so a CRLF config.toml stays CRLF and
	// never ends up with mixed endings.
	crlf := strings.Contains(existing, "\r\n")
	body := stripManagedBlock(strings.ReplaceAll(existing, "\r\n", "\n"))

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
	if crlf {
		out = strings.ReplaceAll(out, "\n", "\r\n")
	}
	if out == existing {
		return false, nil
	}
	if err := writePreservingSymlinkAndMode(configPath, []byte(out), 0o600); err != nil {
		return false, err
	}
	return true, nil
}

// canonicalGroupJSON renders one hook group for semantic comparison.
// encoding/json marshals map keys in sorted order, so two groups that are
// semantically identical - matcher, the ordered hook list, and every
// unknown/custom field a future Codex or a user added - produce identical
// bytes, while ANY difference (a different matcher, a reordered hooks
// list, a changed timeout) produces different bytes. Numbers survive as
// their original literals because loadSettings decodes with UseNumber.
func canonicalGroupJSON(group any) []byte {
	b, err := json.Marshal(group)
	if err != nil {
		return nil
	}
	return b
}

// DedupeCodexHookScopes removes punk-managed hook groups from the
// project-scope hooks file (projectPath) that are semantically identical
// to a punk-managed group in the global hooks file (globalPath) for the
// same event. Codex merges both scopes, so the same group registered in
// both executes the same punk hook command twice per event for that repo
// (the double-capture half of the C03 duplicate-integration defect). The
// global registration already fires for every repo, so the project copy of
// an identical group is the redundant one.
//
// Equivalence is whole-group, not command-string: a project group is
// removed only when its canonical JSON (matcher, ordered hooks list,
// type/timeout and any unknown fields) byte-matches ONE punk-managed
// global group for the same event. Matching on the command alone would be
// matcher-blind - a global SessionStart matcher=startup group must never
// cause a project matcher=resume group to be deleted - and pooling
// commands across unrelated global groups would claim equivalences no
// single global group actually has. A project punk group that matches no
// global group exactly (e.g. carries a --ns pin, a different matcher, or
// different timeouts) is kept, and the returned notes name the event and
// both files so the user sees the double execution and can act. User
// groups and non-punk groups are never touched. Either file missing means
// there is nothing to duplicate: nil notes, changed=false. A
// present-but-wrongly-shaped "hooks" value is refused with an error naming
// the file, matching ConnectCodexHooks' own discipline for live user
// config.
func DedupeCodexHookScopes(globalPath, projectPath, punkPath string) (notes []string, changed bool, err error) {
	// Same-file guard, before any loading: a custom CODEX_HOME can make the
	// "global" and "project" paths name ONE file (directly, or through a
	// symlink/parent alias - e.g. punk connect codex --project with
	// CODEX_HOME pointed at the project .codex). Deduping a file against
	// itself would delete its own - and only - punk registrations.
	if sameFile(globalPath, projectPath) {
		return nil, false, nil
	}
	global, _, err := loadSettings(globalPath)
	if err != nil {
		return nil, false, err
	}
	project, existing, err := loadSettings(projectPath)
	if err != nil {
		return nil, false, err
	}
	if existing == nil {
		// No project file at all: nothing to dedupe.
		return nil, false, nil
	}

	globalHooks, _ := global["hooks"].(map[string]any)
	projectHooks, _ := project["hooks"].(map[string]any)
	if raw, ok := project["hooks"]; ok && raw != nil && projectHooks == nil {
		return nil, false, fmt.Errorf("hooks is not an object; refusing to modify %s", projectPath)
	}
	if len(globalHooks) == 0 || len(projectHooks) == 0 {
		return nil, false, nil
	}

	for _, ev := range codexHookEvents {
		groups, _ := projectHooks[ev].([]any)
		if len(groups) == 0 {
			continue
		}
		globalGroups, _ := globalHooks[ev].([]any)
		var globalPunkCanon [][]byte
		for _, g := range globalGroups {
			if isPunkManagedGroup(g, punkPath) {
				globalPunkCanon = append(globalPunkCanon, canonicalGroupJSON(g))
			}
		}

		var keptGroups []any
		for _, g := range groups {
			if !isPunkManagedGroup(g, punkPath) {
				keptGroups = append(keptGroups, g)
				continue
			}
			canon := canonicalGroupJSON(g)
			equivalent := false
			for _, gc := range globalPunkCanon {
				if string(gc) == string(canon) {
					equivalent = true
					break
				}
			}
			if equivalent {
				changed = true
				notes = append(notes, fmt.Sprintf("%s: removed identical project-scope punk hook group from %s (the global registration in %s already fires for this repo)", ev, projectPath, globalPath))
				continue
			}
			keptGroups = append(keptGroups, g)
			notes = append(notes, fmt.Sprintf("%s: both %s and %s register a punk hook; both fire for this repo (the project group differs - for example a namespace pin or a different matcher) - kept, remove one scope by hand if unintended", ev, globalPath, projectPath))
		}
		if changed {
			if len(keptGroups) == 0 {
				delete(projectHooks, ev)
			} else {
				projectHooks[ev] = keptGroups
			}
		}
	}
	if !changed {
		return notes, false, nil
	}

	project["hooks"] = projectHooks
	out, err := encodeSettings(project)
	if err != nil {
		return nil, false, fmt.Errorf("encode settings: %w", err)
	}
	if string(out) == string(existing) {
		return notes, false, nil
	}
	if err := writePreservingSymlinkAndMode(projectPath, out, 0o644); err != nil {
		return nil, false, err
	}
	return notes, true, nil
}

// codexMCPTableRe matches an [mcp_servers.<alias>] table header; the
// alias group is everything between the dots.
var codexMCPTableRe = regexp.MustCompile(`(?m)^\s*\[mcp_servers\.([^\]]+)\]\s*$`)

// codexMCPURLRe matches a url = "..." line inside one mcp_servers table.
var codexMCPURLRe = regexp.MustCompile(`(?m)^\s*url\s*=\s*("(?:[^"\\]|\\.)*")`)

// DetectCodexMCPAliases reports every mcp_servers alias in configPath
// whose url is exactly punk's MCP endpoint for serverURL. More than one
// such alias means Codex opens duplicate connections to the same punk
// server under different names (the duplicate-MCP-alias half of C03's
// duplicate-integration defect). Detection only: aliases punk did not
// write are foreign config and are never edited here - the caller prints
// the list so the user can resolve them. A missing file or a config with
// at most one matching alias returns nil. CRLF content is normalized
// before scanning, matching ConnectCodexConfig's own line-ending
// tolerance.
func DetectCodexMCPAliases(configPath, serverURL string) ([]string, error) {
	raw, err := os.ReadFile(configPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	body := strings.ReplaceAll(string(raw), "\r\n", "\n")
	want := mcpEndpoint(serverURL)

	headers := codexMCPTableRe.FindAllStringSubmatchIndex(body, -1)
	var aliases []string
	for i, h := range headers {
		end := len(body)
		if i+1 < len(headers) {
			end = headers[i+1][0]
		}
		block := body[h[1]:end]
		m := codexMCPURLRe.FindStringSubmatch(block)
		if m == nil {
			continue
		}
		var u string
		if err := json.Unmarshal([]byte(m[1]), &u); err != nil {
			continue
		}
		if u == want {
			aliases = append(aliases, body[h[2]:h[3]])
		}
	}
	if len(aliases) < 2 {
		return nil, nil
	}
	sort.Strings(aliases)
	return aliases, nil
}
