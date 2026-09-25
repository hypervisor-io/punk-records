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
	return connectCodexHooks(hooksPath, punkPath, serverURL, ns, false)
}

// ConnectCodexHooksMessaging is ConnectCodexHooks plus the opt-in inbox
// groups (punk connect codex --messaging): SessionStart (matcher
// startup|resume, same as capture) and UserPromptSubmit in --mode
// context, Stop in --mode continue. Codex hooks mirror Claude Code's
// shape and reply contract (developers.openai.com/codex/hooks, fetched
// 2026-09-25), so the Claude-shaped merge and reply writer are shared.
func ConnectCodexHooksMessaging(hooksPath, punkPath, serverURL, ns string) (changed bool, err error) {
	return connectCodexHooks(hooksPath, punkPath, serverURL, ns, true)
}

func connectCodexHooks(hooksPath, punkPath, serverURL, ns string, messaging bool) (changed bool, err error) {
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
	if messaging {
		addClaudeShapedInbox(hooksAny, punkPath, "codex", serverURL, ns, codexSessionStartMatcher)
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
// be exactly the duplicate-MCP-alias defect this package reconciles. A
// start marker with no matching end marker is not a block this function
// can safely remove - the content after the start marker could be
// anything, including a real table - so it reports an error naming the
// marker instead of truncating the file at the start marker.
func stripManagedBlock(s string) (string, error) {
	for {
		i := strings.Index(s, codexBlockStart)
		if i < 0 {
			return s, nil
		}
		j := strings.Index(s[i:], codexBlockEnd)
		if j < 0 {
			return "", fmt.Errorf("found %q with no matching %q", codexBlockStart, codexBlockEnd)
		}
		end := i + j + len(codexBlockEnd)
		if end < len(s) && s[end] == '\n' {
			end++
		}
		s = s[:i] + s[end:]
	}
}

// isPunkMCPTableHeader reports whether line is a [mcp_servers.punk]
// table header, in any TOML-equivalent quoting - bare
// ([mcp_servers.punk]), basic-quoted ([mcp_servers."punk"]) or
// literal-quoted ([mcp_servers.'punk']) all name the same table. An
// array-of-tables header ([[mcp_servers.punk]]) is a different
// construct entirely and does not count.
func isPunkMCPTableHeader(line string) bool {
	path, array, ok := parseTOMLTableHeader(line)
	return ok && !array && len(path) == 2 && path[0] == "mcp_servers" && path[1] == "punk"
}

// hasPunkMCPTableHeader reports whether body declares a
// [mcp_servers.punk] table, in any TOML-equivalent quoted spelling.
func hasPunkMCPTableHeader(body string) bool {
	for _, ln := range strings.Split(body, "\n") {
		if isPunkMCPTableHeader(strings.TrimSpace(ln)) {
			return true
		}
	}
	return false
}

// removeTable deletes the TOML table whose header is exactly hdr, from
// the header line through the line before the next header (or EOF). hdr
// naming the punk table (codexPunkTable) matches every TOML-equivalent
// quoted spelling of [mcp_servers.punk], not just hdr's own bytes -
// removeTable is called with the bare spelling but a config can declare
// the foreign table it replaces with a quoted one.
func removeTable(s, hdr string) string {
	punk := hdr == codexPunkTable
	lines := strings.Split(s, "\n")
	out := lines[:0]
	skipping := false
	for _, ln := range lines {
		trim := strings.TrimSpace(ln)
		if trim == hdr || (punk && isPunkMCPTableHeader(trim)) {
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
	body, stripErr := stripManagedBlock(strings.ReplaceAll(existing, "\r\n", "\n"))
	if stripErr != nil {
		return false, fmt.Errorf("%s: %w", configPath, stripErr)
	}

	if hasPunkMCPTableHeader(body) {
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
		// Inbox groups (--messaging) dedupe exactly like capture groups:
		// the same group in both scopes would run the inbox hook twice.
		managed := func(g any) bool {
			return isPunkManagedGroup(g, punkPath) || isPunkManagedInboxGroup(g, punkPath, "codex")
		}
		for _, g := range globalGroups {
			if managed(g) {
				globalPunkCanon = append(globalPunkCanon, canonicalGroupJSON(g))
			}
		}

		var keptGroups []any
		for _, g := range groups {
			if !managed(g) {
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
			// "Both fire" is only true when the global file actually
			// registers a punk group for THIS event too; a project punk
			// group for an event the global file has no punk group for at
			// all is the only registration, so no double-execution note
			// applies.
			if len(globalPunkCanon) > 0 {
				notes = append(notes, fmt.Sprintf("%s: both %s and %s register a punk hook; both fire for this repo (the project group differs - for example a namespace pin or a different matcher) - kept, remove one scope by hand if unintended", ev, globalPath, projectPath))
			}
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

// hookNSFlagRe extracts the value of the --ns flag from a punk hook
// command. Namespaces never contain whitespace (nsSlug in namespace.go
// strips everything but letters and digits), so a bare token match is
// exact; the flag is the last token punkHookCommandFrom emits, but a
// hand-edited command may append further flags, so no end anchor.
var hookNSFlagRe = regexp.MustCompile(`(?:^|\s)--ns(?:=|\s)(\S+)`)

// hookURLFlagRe extracts the value of the --url flag from a punk hook
// command: the server endpoint the command forwards captures to. URLs
// never contain whitespace, so the bare token match is exact, and no
// end anchor for the same reason --ns has none.
var hookURLFlagRe = regexp.MustCompile(`(?:^|\s)--url(?:=|\s)(\S+)`)

// CodexHookRegistration is one punk-managed hook command read back from
// an installed hooks.json: the event it is registered under, the
// namespace pin its command carries (the --ns flag; "" = unpinned, the
// capture derives from the hook payload's cwd) and the server endpoint
// it forwards captures to (the --url flag; "" when the command carries
// no --url and the endpoint cannot be established from the file).
type CodexHookRegistration struct {
	Event    string
	Pin      string
	Endpoint string
}

// CodexHookScope is what one hooks.json scope file holds: every
// punk-managed hook registration installed in it. Installed is false
// when the file is missing or holds no punk-managed group - nothing
// punk wrote will capture from it. Err is set when the file exists but
// could not be inspected; callers must treat that scope as unknown,
// never as absent or unpinned.
type CodexHookScope struct {
	File          string
	Installed     bool
	Registrations []CodexHookRegistration
	Err           error
}

// inspectCodexHookScope reads back every punk-managed hook command from
// one hooks.json. EVERY punk-managed group under EVERY event key
// contributes a registration, not just the first one found: Codex
// appends hooks from all applicable config layers and a hand-edited
// file can pin different namespaces per group, so a first-group-wins
// view would hide capture destinations that fire just the same.
func inspectCodexHookScope(hooksPath, punkPath string) CodexHookScope {
	sc := CodexHookScope{File: hooksPath}
	settings, existing, err := loadSettings(hooksPath)
	if err != nil {
		sc.Err = err
		return sc
	}
	if existing == nil {
		return sc
	}
	var hooksAny map[string]any
	if raw, ok := settings["hooks"]; ok && raw != nil {
		var isMap bool
		hooksAny, isMap = raw.(map[string]any)
		if !isMap {
			sc.Err = fmt.Errorf("hooks is not an object; cannot inspect %s", hooksPath)
			return sc
		}
	}
	// Canonical events first, then any further keys the file holds in
	// sorted order: a punk-managed group registered under an unexpected
	// event name is still a capture destination.
	known := map[string]bool{}
	events := append([]string{}, codexHookEvents...)
	for _, ev := range codexHookEvents {
		known[ev] = true
	}
	var extra []string
	for ev := range hooksAny {
		if !known[ev] {
			extra = append(extra, ev)
		}
	}
	sort.Strings(extra)
	events = append(events, extra...)
	for _, ev := range events {
		groups, _ := hooksAny[ev].([]any)
		for _, g := range groups {
			if !isPunkManagedGroup(g, punkPath) {
				continue
			}
			gm, _ := g.(map[string]any)
			hooks, _ := gm["hooks"].([]any)
			for _, h := range hooks {
				cmd, _ := h.(map[string]any)["command"].(string)
				reg := CodexHookRegistration{Event: ev}
				if m := hookNSFlagRe.FindStringSubmatch(cmd); m != nil {
					reg.Pin = strings.Trim(m[1], `"'`)
				}
				if m := hookURLFlagRe.FindStringSubmatch(cmd); m != nil {
					reg.Endpoint = strings.Trim(m[1], `"'`)
				}
				sc.Registrations = append(sc.Registrations, reg)
			}
		}
	}
	sc.Installed = len(sc.Registrations) > 0
	return sc
}

// distinctScopePaths drops every path that names the same file as an
// earlier path in the list (sameFile: identical absolute paths,
// symlinks and hardlinks all count). A custom CODEX_HOME can alias the
// global scope onto a project's .codex directory, and an aliased file
// must contribute its registrations or entries exactly once.
func distinctScopePaths(paths []string) []string {
	var out []string
	for i, p := range paths {
		aliased := false
		for _, q := range paths[:i] {
			if sameFile(p, q) {
				aliased = true
				break
			}
		}
		if !aliased {
			out = append(out, p)
		}
	}
	return out
}

// EnumerateCodexHookScopes inspects the punk hook registrations of
// every applicable hooks.json scope, in the order given (highest layer
// first). Native Codex APPENDS hook registrations from all applicable
// config layers (upstream codex-rs discovery loops the layers and
// appends hook events), so a project-scope registration does NOT
// override a global one - both fire, and a caller proving where
// captures land must consider every scope, never just the one a
// particular invocation wrote. Paths aliasing the same file are
// deduped via sameFile; per-file inspection failures are reported on
// that scope's Err.
func EnumerateCodexHookScopes(punkPath string, hooksPaths ...string) []CodexHookScope {
	var out []CodexHookScope
	for _, p := range distinctScopePaths(hooksPaths) {
		out = append(out, inspectCodexHookScope(p, punkPath))
	}
	return out
}

// CodexHookPin reports the namespace pin the punk-managed hook
// registrations of ONE hooks.json carry (the --ns flag; "" when the
// commands pin nothing and capture derives from the hook payload's
// cwd). installed is false when the file is missing or holds no
// punk-managed group - nothing punk wrote will capture from it, and
// callers must treat the hook side as absent rather than unpinned.
// When the file's punk registrations disagree about the pin (a layered
// or hand-edited install) there is no single honest answer, and
// CodexHookPin returns an error instead of hiding a capture namespace
// behind the first group found; alignment checks use
// EnumerateCodexHookScopes, which reports every registration with its
// own pin and endpoint.
func CodexHookPin(hooksPath, punkPath string) (pin string, installed bool, err error) {
	sc := inspectCodexHookScope(hooksPath, punkPath)
	if sc.Err != nil {
		return "", false, sc.Err
	}
	if !sc.Installed {
		return "", false, nil
	}
	var distinct []string
	seen := map[string]bool{}
	for _, r := range sc.Registrations {
		if !seen[r.Pin] {
			seen[r.Pin] = true
			distinct = append(distinct, r.Pin)
		}
	}
	if len(distinct) > 1 {
		for i, p := range distinct {
			if p == "" {
				distinct[i] = "unpinned"
			}
		}
		return "", true, fmt.Errorf("%s carries punk hook registrations with more than one namespace pin (%s); it has no single pin", hooksPath, strings.Join(distinct, ", "))
	}
	return distinct[0], true, nil
}

// parseTOMLString parses a basic ("...") or literal ('...') string at
// the start of s, returning the decoded value and the remainder after
// the closing quote. Multi-line (triple-quoted) strings, unterminated
// strings and escapes encoding/json cannot decode report ok=false -
// callers must return unknown rather than guess at a value.
func parseTOMLString(s string) (val, rest string, ok bool) {
	switch {
	case strings.HasPrefix(s, `'''`), strings.HasPrefix(s, `"""`):
		return "", s, false
	case strings.HasPrefix(s, `"`):
		// Scan to the closing quote respecting escapes, then decode
		// with encoding/json: TOML basic strings share escapes with
		// JSON for everything that appears in punk-written config.
		for i := 1; i < len(s); i++ {
			switch s[i] {
			case '\\':
				i++
			case '"':
				var v string
				if err := json.Unmarshal([]byte(s[:i+1]), &v); err != nil {
					return "", s, false
				}
				return v, s[i+1:], true
			}
		}
		return "", s, false
	case strings.HasPrefix(s, `'`):
		// Literal strings carry no escapes: the value runs verbatim
		// to the next single quote.
		if i := strings.Index(s[1:], `'`); i >= 0 {
			return s[1 : i+1], s[i+2:], true
		}
		return "", s, false
	}
	return "", s, false
}

// parseTOMLKey parses a bare, basic-quoted or literal-quoted key at the
// start of s, returning the key and the remainder after it.
func parseTOMLKey(s string) (key, rest string, ok bool) {
	if strings.HasPrefix(s, `"`) || strings.HasPrefix(s, `'`) {
		return parseTOMLString(s)
	}
	i := 0
	for i < len(s) {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' {
			i++
			continue
		}
		break
	}
	if i == 0 {
		return "", s, false
	}
	return s[:i], s[i:], true
}

// tomlTrailingOK reports whether the remainder of a line after a
// complete value holds only whitespace and/or a # comment - the only
// tails TOML allows there.
func tomlTrailingOK(rest string) bool {
	rest = strings.TrimSpace(rest)
	return rest == "" || strings.HasPrefix(rest, "#")
}

// cutTOMLInlineTable splits s, which must start with '{', at its
// matching closing brace (respecting basic and literal strings),
// returning the body between the braces and the remainder after the
// closing brace. Unterminated braces or strings report ok=false; the
// caller then folds in further lines (TOML inline tables may span
// multiple lines) or gives up as unknown.
func cutTOMLInlineTable(s string) (body, rest string, ok bool) {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"', '\'':
			q := s[i]
			i++
			for i < len(s) {
				if s[i] == '\\' && q == '"' {
					i += 2
					continue
				}
				if s[i] == q {
					break
				}
				i++
			}
			if i >= len(s) {
				return "", s, false
			}
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[1:i], s[i+1:], true
			}
		}
	}
	return "", s, false
}

// parseTOMLInlinePairs parses an inline table's body (the text between
// its braces) into key/value pairs. Keys may be bare, basic-quoted or
// literal-quoted; values must be basic or literal strings. Anything
// else - nested tables, numbers, duplicate keys, malformed separators
// - reports ok=false so the caller returns unknown rather than guesses.
func parseTOMLInlinePairs(body string) (map[string]string, bool) {
	pairs := map[string]string{}
	s := strings.TrimSpace(body)
	for s != "" {
		key, rest, ok := parseTOMLKey(s)
		if !ok {
			return nil, false
		}
		s = strings.TrimSpace(rest)
		if !strings.HasPrefix(s, "=") {
			return nil, false
		}
		val, rest, ok := parseTOMLString(strings.TrimSpace(s[1:]))
		if !ok {
			return nil, false
		}
		if _, dup := pairs[key]; dup {
			return nil, false
		}
		pairs[key] = val
		s = strings.TrimSpace(rest)
		switch {
		case s == "":
			return pairs, true
		case strings.HasPrefix(s, ","):
			s = strings.TrimSpace(s[1:])
		default:
			return nil, false
		}
	}
	return pairs, true
}

// cutTOMLArray splits s, which must start with '[', at its matching
// closing bracket (respecting basic and literal strings, nested
// brackets and the '#' comments TOML allows inside arrays), returning
// the body between the brackets and the remainder after the closing
// bracket. Unterminated brackets, strings or comments report ok=false;
// the caller folds in further lines (TOML arrays may span multiple
// lines) or gives up as unknown.
func cutTOMLArray(s string) (body, rest string, ok bool) {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '#':
			j := strings.Index(s[i:], "\n")
			if j < 0 {
				return "", s, false
			}
			i += j
		case '"', '\'':
			q := s[i]
			i++
			for i < len(s) {
				if s[i] == '\\' && q == '"' {
					i += 2
					continue
				}
				if s[i] == q {
					break
				}
				i++
			}
			if i >= len(s) {
				return "", s, false
			}
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return s[1:i], s[i+1:], true
			}
		}
	}
	return "", s, false
}

// foldTOMLValue cuts the array or inline table whose opener starts
// value, folding in further lines (advancing *i) when the construct
// spans them - TOML arrays and inline tables may span multiple lines.
// ok=false means it never closes (or holds an unterminated string): an
// uninspectable construct, conservatively unknown to the caller.
func foldTOMLValue(lines []string, i *int, value string, cut func(string) (string, string, bool)) (body, rest string, ok bool) {
	joined := value
	body, rest, ok = cut(joined)
	for !ok {
		*i++
		if *i >= len(lines) {
			return "", "", false
		}
		joined += "\n" + strings.TrimSpace(lines[*i])
		body, rest, ok = cut(joined)
	}
	return body, rest, true
}

// splitTOMLElems splits a TOML array body at its top-level commas,
// respecting strings, nested arrays, inline tables and '#' comments
// (which TOML allows inside arrays). Elements come back trimmed with
// comments stripped; an empty element or a construct the scan cannot
// follow reports ok=false. A trailing comma is legal TOML and
// contributes no element.
func splitTOMLElems(body string) ([]string, bool) {
	// Strip comments first, tracking strings so a '#' inside one stays
	// content.
	var clean strings.Builder
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '#':
			j := strings.Index(body[i:], "\n")
			if j < 0 {
				i = len(body) // a comment running to the end of the body
				continue
			}
			i += j - 1 // the loop's i++ lands on the newline, which is kept
		case '"', '\'':
			q := body[i]
			clean.WriteByte(q)
			i++
			for i < len(body) {
				clean.WriteByte(body[i])
				if body[i] == '\\' && q == '"' {
					i++
					if i < len(body) {
						clean.WriteByte(body[i])
					}
					continue
				}
				if body[i] == q {
					break
				}
				i++
			}
			if i >= len(body) {
				return nil, false
			}
		default:
			clean.WriteByte(body[i])
		}
	}
	s := clean.String()
	var elems []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"', '\'':
			q := s[i]
			i++
			for i < len(s) {
				if s[i] == '\\' && q == '"' {
					i += 2
					continue
				}
				if s[i] == q {
					break
				}
				i++
			}
			if i >= len(s) {
				return nil, false
			}
		case '[', '{':
			depth++
		case ']', '}':
			depth--
			if depth < 0 {
				return nil, false
			}
		case ',':
			if depth == 0 {
				el := strings.TrimSpace(s[start:i])
				if el == "" {
					return nil, false
				}
				elems = append(elems, el)
				start = i + 1
			}
		}
	}
	if depth != 0 {
		return nil, false
	}
	if last := strings.TrimSpace(s[start:]); last != "" {
		elems = append(elems, last)
	}
	return elems, true
}

// tomlStringElemOK reports whether elem is exactly one basic or literal
// string and nothing else.
func tomlStringElemOK(elem string) bool {
	_, rest, ok := parseTOMLString(strings.TrimSpace(elem))
	return ok && strings.TrimSpace(rest) == ""
}

// tomlEnvVarElemOK reports whether elem is a valid env_vars element:
// either a plain string (a variable name) or an inline table (the
// name/source form upstream McpServerEnvVar accepts).
func tomlEnvVarElemOK(elem string) bool {
	elem = strings.TrimSpace(elem)
	if tomlStringElemOK(elem) {
		return true
	}
	if strings.HasPrefix(elem, "{") {
		_, rest, ok := cutTOMLInlineTable(elem)
		return ok && strings.TrimSpace(rest) == ""
	}
	return false
}

// scanTOMLInlineKeys walks the top-level keys of an inline table body
// (the text between its braces), reporting whether want is among them.
// Values are skipped generically - strings, nested inline tables,
// arrays and bare scalars - because the walk only establishes key
// PRESENCE. A body that cannot be walked faithfully reports ok=false:
// the caller cannot prove want absent and must answer unknown.
func scanTOMLInlineKeys(body, want string) (found, ok bool) {
	s := strings.TrimSpace(body)
	for s != "" {
		key, rest, ok := parseTOMLKey(s)
		if !ok {
			return false, false
		}
		if key == want {
			return true, true
		}
		s = strings.TrimSpace(rest)
		if !strings.HasPrefix(s, "=") {
			return false, false
		}
		s = strings.TrimSpace(s[1:])
		switch {
		case strings.HasPrefix(s, `"`), strings.HasPrefix(s, `'`):
			_, rest, ok := parseTOMLString(s)
			if !ok {
				return false, false
			}
			s = strings.TrimSpace(rest)
		case strings.HasPrefix(s, "{"):
			_, rest, ok := cutTOMLInlineTable(s)
			if !ok {
				return false, false
			}
			s = strings.TrimSpace(rest)
		case strings.HasPrefix(s, "["):
			_, rest, ok := cutTOMLArray(s)
			if !ok {
				return false, false
			}
			s = strings.TrimSpace(rest)
		default:
			// A bare scalar runs to the next comma or the end of the
			// body; quote and bracket characters inside it are invalid
			// TOML this walk refuses to guess at.
			j := strings.Index(s, ",")
			scalar := s
			if j >= 0 {
				scalar = s[:j]
			}
			scalar = strings.TrimSpace(scalar)
			if scalar == "" || strings.ContainsAny(scalar, `"'{}[]`) {
				return false, false
			}
			if j < 0 {
				return false, true
			}
			s = strings.TrimSpace(s[j+1:])
			continue
		}
		switch {
		case s == "":
			return false, true
		case strings.HasPrefix(s, ","):
			s = strings.TrimSpace(s[1:])
		default:
			return false, false
		}
	}
	return false, true
}

// parseTOMLKeyPath parses a dotted key path (bare, basic-quoted or
// literal-quoted segments joined by '.', whitespace allowed around the
// dots) at the start of s, returning the decoded segments and the
// remainder after the last segment.
func parseTOMLKeyPath(s string) (segs []string, rest string, ok bool) {
	rest = s
	for {
		seg, r, segOK := parseTOMLKey(strings.TrimSpace(rest))
		if !segOK {
			return nil, s, false
		}
		segs = append(segs, seg)
		r = strings.TrimSpace(r)
		if !strings.HasPrefix(r, ".") {
			return segs, r, true
		}
		rest = r[1:]
	}
}

// splitTOMLKeyPath splits a dotted key path (a table header's inner
// text) into its normalized segments, unquoting quoted segments - so
// [mcp_servers."punk"] names the same table as [mcp_servers.punk].
func splitTOMLKeyPath(s string) ([]string, bool) {
	var segs []string
	var cur strings.Builder
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"', '\'':
			val, rest, ok := parseTOMLString(s[i:])
			if !ok {
				return nil, false
			}
			cur.WriteString(val)
			i += len(s[i:]) - len(rest) - 1
		case '.':
			segs = append(segs, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(s[i])
		}
	}
	segs = append(segs, cur.String())
	for i := range segs {
		segs[i] = strings.TrimSpace(segs[i])
		if segs[i] == "" {
			return nil, false
		}
	}
	return segs, true
}

// parseTOMLTableHeader parses a [a.b."c"] or [[a.b]] header line into
// its normalized dotted key path, plus whether it is an array-of-tables
// header. The closing bracket scan respects quoted segments, and only
// whitespace or a comment may follow the header.
func parseTOMLTableHeader(line string) (path []string, array bool, ok bool) {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, "[") {
		return nil, false, false
	}
	s = s[1:]
	if strings.HasPrefix(s, "[") {
		array = true
		s = s[1:]
	}
	end := -1
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"', '\'':
			q := s[i]
			i++
			for i < len(s) {
				if s[i] == '\\' && q == '"' {
					i += 2
					continue
				}
				if s[i] == q {
					break
				}
				i++
			}
			if i >= len(s) {
				return nil, false, false
			}
		case ']':
			end = i
			i = len(s)
		}
	}
	if end < 0 {
		return nil, false, false
	}
	inner, rest := s[:end], s[end+1:]
	if array {
		rest = strings.TrimLeft(rest, " \t")
		if !strings.HasPrefix(rest, "]") {
			return nil, false, false
		}
		rest = rest[1:]
	}
	if !tomlTrailingOK(rest) {
		return nil, false, false
	}
	path, ok = splitTOMLKeyPath(inner)
	return path, array, ok
}

// tomlCodeLineStarts scans the whole document line by line, tracking
// basic ("...") and literal ('...') strings, their triple-quoted
// multi-line forms and # comments, and reports for every line whether
// the line BEGINS in code context. A line beginning inside a
// multi-line string is string content, never document structure: a
// [table] header written inside developer_instructions triple-quoted
// documentation is not config, and the line scanner must not see it. A
// multi-line closing delimiter may carry up to two quote characters of
// content (the closing run may be three to five quotes); a longer
// quote run is ambiguous. Unterminated single-line strings, over-long
// closing quote runs and unterminated multi-line strings are invalid
// or unsupported TOML and report ok=false, so the caller returns
// unknown instead of risking a misread of the document's structure.
func tomlCodeLineStarts(lines []string) ([]bool, bool) {
	const (
		stCode = iota
		stBasic
		stLiteral
		stMLBasic
		stMLLiteral
	)
	state := stCode
	starts := make([]bool, len(lines))
	for i, ln := range lines {
		starts[i] = state == stCode
	scan:
		for j := 0; j < len(ln); j++ {
			c := ln[j]
			switch state {
			case stCode:
				switch c {
				case '#':
					break scan // the rest of the line is a comment
				case '"':
					if strings.HasPrefix(ln[j:], `"""`) {
						state = stMLBasic
						j += 2
					} else {
						state = stBasic
					}
				case '\'':
					if strings.HasPrefix(ln[j:], `'''`) {
						state = stMLLiteral
						j += 2
					} else {
						state = stLiteral
					}
				}
			case stBasic:
				switch c {
				case '\\':
					j++
				case '"':
					state = stCode
				}
			case stLiteral:
				if c == '\'' {
					state = stCode
				}
			case stMLBasic, stMLLiteral:
				quote := byte('"')
				if state == stMLLiteral {
					quote = '\''
				}
				if state == stMLBasic && c == '\\' {
					j++ // an escaped character is content, never a delimiter
					continue
				}
				if c != quote {
					continue
				}
				run := 1
				for j+run < len(ln) && ln[j+run] == quote {
					run++
				}
				if run >= 3 {
					if run > 5 {
						return nil, false
					}
					state = stCode // the last three quotes close; any before them are content
				}
				j += run - 1
			}
		}
		if state == stBasic || state == stLiteral {
			return nil, false // a single-line string still open at end of line is invalid TOML
		}
	}
	if state != stCode {
		return nil, false // an unterminated multi-line string at EOF is invalid TOML
	}
	return starts, true
}

// CodexMCPScope is what one config.toml scope file CONTRIBUTES to the
// punk MCP entry, read back from the file. Codex recursively merges
// MCP table FIELDS across config layers rather than replacing whole
// entries (upstream config/src/merge.rs:58-132, state.rs:343), so when
// more than one layer contributes, one file's fields are never the
// effective entry on their own: a field the higher layer's entry lacks
// inherits from the lower layers, and a header-subtable fragment in a
// layer that defines no [mcp_servers.punk] table of its own still
// contributes to the merged entry. MergeCodexMCPScopes computes that
// effective entry; the exported fields below describe THIS file's own
// contribution. Pin is the entry's
// effective X-Punk-Namespace header ("" = the entry pins nothing):
// static http_headers overlaid with env_http_headers the way Codex's
// rmcp client applies them, header names matched case-insensitively
// and env-resolved headers winning on conflict. Endpoint is the url
// Codex connects through ("" when the entry carries no url, e.g. a
// command-launched stdio entry, and the connection cannot be
// established from the file) and BearerEnv the NAME of the env var the
// entry reads its bearer token from - the name only, a token value is
// never read into this struct. HeaderAuth is true when the entry's
// effective headers carry an Authorization header (static or
// env-resolved): the connection then authenticates from a header
// credential whose provenance verify cannot compare safely, so the
// credential must be treated as unestablished. Disabled is true when
// the entry sets enabled = false: Codex filters disabled entries
// (upstream codex-rs connection_manager.rs), so the entry is installed
// but is not a working connection. HeadersHelper is true when the
// entry sets http_headers_helper (upstream codex-rs
// config/src/mcp_types.rs): part of the entry's headers are then
// resolved by running an external command, so the effective headers -
// namespace pin and credentials alike - are unverified; the helper is
// NEVER executed here. Err is set when the
// file exists but its punk entry uses a TOML representation this
// inspector does not parse faithfully; callers must treat that entry
// as unknown, never as absent or unpinned.
type CodexMCPScope struct {
	File          string
	Installed     bool
	Pin           string
	Endpoint      string
	BearerEnv     string
	HeaderAuth    bool
	Disabled      bool
	HeadersHelper bool
	Err           error

	// Raw material for the cross-layer field-wise inheritance merge
	// (MergeCodexMCPScopes), unexported so a single scope is never
	// mistaken for the effective entry. The presence bits distinguish a
	// field THIS layer defines from one it leaves to lower layers: an
	// absent field must inherit, never default to false/empty. The
	// header material is secret-safe: namespace pin values (safe to
	// report) and env var NAMES are kept, an Authorization header is
	// kept as presence only - its value is never stored.
	hasURL       bool              // this layer defines url
	hasBearerEnv bool              // this layer defines bearer_token_env_var
	enabledSet   bool              // this layer defines enabled
	enabledVal   bool              // this layer's enabled value (meaningful when enabledSet)
	fragment     bool              // punk subtables (headers, env, oauth) without a [mcp_servers.punk] table of their own
	nsStatic     map[string]string // raw x-punk-namespace spellings in http_headers -> pin value
	nsEnv        map[string]string // raw x-punk-namespace spellings in env_http_headers -> env var name
	authStatic   map[string]bool   // raw authorization spellings in http_headers (presence; value never kept)
	authEnv      map[string]string // raw authorization spellings in env_http_headers -> env var name
	// tpFields records the PRESENCE of every upstream transport-shaped
	// field this layer defines (config/src/mcp_types.rs
	// RawMcpServerConfig: command/args/env/env_vars/cwd shape a stdio
	// transport, url/bearer_token(_env_var)/the header fields/
	// http_headers_helper/oauth/oauth_resource/auth a streamable-HTTP
	// one). Codex recursively merges fields across layers and then
	// REJECTS an entry mixing the two shapes, so the merge validates
	// the combined presence - values are never kept, bearer_token above
	// all.
	tpFields map[string]bool
}

// markTransport records the presence of an upstream transport-shaped
// field on this scope. Presence is all the cross-layer merge validates;
// the field's value is never kept.
func (sc *CodexMCPScope) markTransport(field string) {
	if sc.tpFields == nil {
		sc.tpFields = map[string]bool{}
	}
	sc.tpFields[field] = true
}

// auditCodexForeignLine examines one key line sitting OUTSIDE the punk
// tables for punk-identity representations the table-scoped parse would
// otherwise skip - the class of TOML spellings that still define punk
// MCP entry fields: dotted keys under the parent table
// ([mcp_servers] punk.url = ...), dotted keys at the document root
// (mcp_servers.punk.url = ...) and inline tables
// (mcp_servers = { punk = {...} } or [mcp_servers] punk = {...}).
// Codex reads all of them into the same merged punk entry, so skipping
// one would report inherited connection identity as absent; every one
// of them fails the scope as unknown instead. Lines that cannot reach
// the punk entry (other servers, other tables, text this inspector
// cannot even parse as a key) keep being ignored - but when such a
// line's value is an array or inline table, it is still folded (i
// advances across the lines it spans) so a continuation line is never
// left for the caller's line loop to misread as document structure (in
// particular, a nested array element line beginning with '[' would
// otherwise be read as a table header). Diagnostics name the file, the
// line and the representation - never a value, which can carry a
// secret.
func auditCodexForeignLine(lines []string, i *int, cur []string, trim, configPath string) error {
	segs, rest, ok := parseTOMLKeyPath(trim)
	if !ok || len(segs) == 0 {
		return nil
	}
	dotted := len(segs) > 1
	switch {
	case len(cur) == 1 && cur[0] == "mcp_servers" && segs[0] == "punk":
		return fmt.Errorf("%s line %d defines punk MCP entry fields through a dotted key under [mcp_servers], which this inspector does not support; the punk MCP entry cannot be inspected faithfully", configPath, *i+1)
	case len(cur) == 0 && len(segs) >= 2 && segs[0] == "mcp_servers" && segs[1] == "punk":
		return fmt.Errorf("%s line %d defines punk MCP entry fields through a dotted key at the document root, which this inspector does not support; the punk MCP entry cannot be inspected faithfully", configPath, *i+1)
	}

	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(rest, "=") {
		return nil
	}
	value := strings.TrimSpace(rest[1:])

	// A root-level inline mcp_servers table: its top-level keys are the
	// server aliases, so a punk key among them is the punk entry in a
	// representation this inspector does not parse.
	if !dotted && len(cur) == 0 && segs[0] == "mcp_servers" && strings.HasPrefix(value, "{") {
		body, _, ok := foldTOMLValue(lines, i, value, cutTOMLInlineTable)
		if !ok {
			return fmt.Errorf("%s line %d holds an mcp_servers inline table this inspector does not support; whether it defines a punk entry cannot be established", configPath, *i+1)
		}
		found, ok := scanTOMLInlineKeys(body, "punk")
		if !ok {
			return fmt.Errorf("%s line %d holds an mcp_servers inline table this inspector does not support; whether it defines a punk entry cannot be established", configPath, *i+1)
		}
		if found {
			return fmt.Errorf("%s line %d defines the punk MCP entry inside an inline mcp_servers table, which this inspector does not support; the punk MCP entry cannot be inspected faithfully", configPath, *i+1)
		}
		return nil
	}

	// Any other key line's array or inline-table value carries no punk
	// identity (it was already ruled out above), but TOML arrays and
	// inline tables can span multiple lines, and the value must still be
	// folded so those continuation lines are consumed here rather than
	// examined as fresh lines by the caller.
	switch {
	case strings.HasPrefix(value, "["):
		if _, _, ok := foldTOMLValue(lines, i, value, cutTOMLArray); !ok {
			return fmt.Errorf("%s line %d holds an array this inspector cannot follow to its end; the punk MCP entry cannot be inspected faithfully", configPath, *i+1)
		}
	case strings.HasPrefix(value, "{"):
		if _, _, ok := foldTOMLValue(lines, i, value, cutTOMLInlineTable); !ok {
			return fmt.Errorf("%s line %d holds an inline table this inspector cannot follow to its end; the punk MCP entry cannot be inspected faithfully", configPath, *i+1)
		}
	}
	return nil
}

// codexPunkEntryKeys enumerates exactly the [mcp_servers.punk] keys the
// inspector recognizes - the supported boundary of the punk entry: the
// identity fields it parses values for (url, enabled,
// bearer_token_env_var, http_headers, env_http_headers,
// http_headers_helper, auth), the benign punk-written knobs
// (startup_timeout_sec, tool_timeout_sec, default_tools_approval_mode)
// and the upstream transport-shaped fields it records as PRESENCE for
// the cross-layer transport validation (command, args, env, env_vars,
// cwd, bearer_token, oauth, oauth_resource). A recognized key defined
// more than once in the table is invalid TOML (toml.io/v1.0.0 keys:
// duplicate definitions are rejected, quoted and bare spellings of the
// same key included), so Codex cannot load the document at all and the
// entry must be reported unknown rather than read last-wins - a
// last-wins reading reports a value Codex never sees. Keys outside the
// enumeration carry neither transport nor punk identity and stay
// ignored; every identity-relevant representation outside the
// recognized shape fails the scope as unknown.
var codexPunkEntryKeys = map[string]bool{
	"url": true, "enabled": true, "bearer_token_env_var": true,
	"http_headers": true, "env_http_headers": true, "http_headers_helper": true, "auth": true,
	"startup_timeout_sec": true, "tool_timeout_sec": true, "default_tools_approval_mode": true,
	"command": true, "args": true, "env": true, "env_vars": true, "cwd": true,
	"bearer_token": true, "oauth": true, "oauth_resource": true,
}

// codexPunkAuthKinds are the auth values upstream Codex's McpServerAuth
// enum supports (config/src/mcp_types.rs:177-187): oauth and chatgpt,
// nothing else. An explicit auth value outside the enum is an auth
// identity Codex cannot load and this inspector cannot establish -
// conservatively unknown, never a guess.
var codexPunkAuthKinds = map[string]bool{"oauth": true, "chatgpt": true}

// inspectCodexMCPScope parses one config.toml for the punk MCP entry.
// Only real punk entries count: the [mcp_servers.punk] table (including
// quoted spellings like [mcp_servers."punk"]) and its
// [mcp_servers.punk.http_headers] / [mcp_servers.punk.env_http_headers]
// subtables - commented-out lines, other tables' text and STRING
// CONTENT are never attributed to punk (the whole document is scanned
// with multi-line basic/literal string tracking first, so a
// [mcp_servers.punk] example inside developer_instructions = """ ...
// """ documentation is not a table header). The supported TOML is
// parsed faithfully: basic
// AND literal strings for url, bearer_token_env_var and header
// keys/values, http_headers and env_http_headers as inline tables
// (single- or multi-line) or as subtables, comments anywhere, CRLF
// endings. env_http_headers (Codex 0.153.4, upstream codex-rs
// rmcp-client/src/utils.rs) maps header names to env var NAMES; each
// named variable that is set and non-blank overlays its value onto the
// static headers case-insensitively, so an env-resolved
// X-Punk-Namespace wins over a static one and ignoring it would report
// the static pin as effective when Codex sends another. enabled is
// parsed as a TOML boolean (enabled = false marks the entry Disabled:
// Codex does not open it), and the mere presence of
// http_headers_helper marks HeadersHelper (the helper is never
// executed). The transport-shaped fields upstream Codex validates
// (config/src/mcp_types.rs RawMcpServerConfig) are recorded as
// PRESENCE for the cross-layer merge: command, args, env (inline table
// of strings or subtable), env_vars, cwd, bearer_token, oauth (inline
// table or subtable), oauth_resource and auth - their values are
// parsed only to prove well-formedness and are then discarded,
// bearer_token's above all; auth additionally must name one of the two
// kinds upstream McpServerAuth supports (oauth, chatgpt -
// config/src/mcp_types.rs:177-187), an explicit value outside the enum
// being an identity this inspector cannot establish. The recognized
// [mcp_servers.punk] keys are exactly codexPunkEntryKeys, and any
// other representation of the punk entry
// (a recognized key, header entry or punk subtable defined more than
// once - TOML rejects duplicate definitions, so Codex cannot load the
// document and a last-wins reading would report a value Codex never
// sees - a field declared BOTH as an inline table and as a subtable,
// dotted keys reaching punk identity fields - inside the punk table,
// under the parent [mcp_servers] table or at the document root - an
// inline mcp_servers table carrying a punk entry, dotted keys inside
// the header/env subtables, multi-line strings, non-string header
// values, a non-boolean enabled, a duplicated table, an unparseable
// header line, an unterminated or ambiguous string anywhere in the
// document, same-named headers with disagreeing values that cannot be
// ordered faithfully) sets Err - conservatively unknown, never a guess
// that a pinned entry is unpinned, because a representation the parser
// skipped would read inherited connection identity as absent.
// Diagnostics name the file, the line number and the field identity
// only: config VALUES (above all header values, which can carry an
// Authorization secret) never appear in an error.
func inspectCodexMCPScope(configPath string) CodexMCPScope {
	sc := CodexMCPScope{File: configPath}
	raw, err := os.ReadFile(configPath)
	if errors.Is(err, os.ErrNotExist) {
		return sc
	}
	if err != nil {
		sc.Err = err
		return sc
	}
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	fail := func(format string, args ...any) CodexMCPScope {
		sc.Err = fmt.Errorf(format, args...)
		return sc
	}
	codeStart, lexOK := tomlCodeLineStarts(lines)
	if !lexOK {
		return fail("%s holds a string construct this inspector does not support; the punk MCP entry cannot be inspected faithfully", configPath)
	}

	var cur []string // normalized key path of the table in scope
	inPunk := func() bool {
		return len(cur) == 2 && cur[0] == "mcp_servers" && cur[1] == "punk"
	}
	inPunkHeaders := func() bool {
		return len(cur) == 3 && cur[0] == "mcp_servers" && cur[1] == "punk" && cur[2] == "http_headers"
	}
	inPunkEnvHeaders := func() bool {
		return len(cur) == 3 && cur[0] == "mcp_servers" && cur[1] == "punk" && cur[2] == "env_http_headers"
	}
	inPunkEnv := func() bool {
		return len(cur) == 3 && cur[0] == "mcp_servers" && cur[1] == "punk" && cur[2] == "env"
	}
	inPunkOauth := func() bool {
		return len(cur) == 3 && cur[0] == "mcp_servers" && cur[1] == "punk" && cur[2] == "oauth"
	}
	underPunk := func() bool {
		return len(cur) >= 2 && cur[0] == "mcp_servers" && cur[1] == "punk"
	}

	punkTables := 0
	headers := map[string]string{}
	envHeaders := map[string]string{}
	inlineHeaders, subtableHeaders := false, false
	inlineEnvHeaders, subtableEnvHeaders := false, false
	subtableEnv, subtableOauth := false, false
	inlineEnv, inlineOauth := false, false
	// Duplicate-definition tracking inside the punk subtree: TOML
	// rejects a key or table defined more than once (quoted and bare
	// spellings of the same key included), so Codex cannot load a
	// document carrying one - every occurrence is reported unknown
	// instead of reading the last value as effective.
	seenPunkSubtables := map[string]bool{}
	seenPunkFields := map[string]bool{}
	envKeys := map[string]bool{}

	for i := 0; i < len(lines); i++ {
		if !codeStart[i] {
			continue // a line inside a multi-line string is string content, never structure
		}
		trim := strings.TrimSpace(lines[i])
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue // blank or comment: never contributes to any table
		}
		if strings.HasPrefix(trim, "[") {
			path, array, ok := parseTOMLTableHeader(trim)
			if !ok {
				return fail("cannot parse the table header on line %d of %s; the punk MCP entry cannot be inspected faithfully", i+1, configPath)
			}
			cur = path
			if underPunk() {
				if array {
					return fail("%s line %d declares punk as an array of tables, which this inspector does not support", configPath, i+1)
				}
				if inPunk() {
					punkTables++
					if punkTables > 1 {
						return fail("%s declares [mcp_servers.punk] more than once; which entry Codex opens cannot be established", configPath)
					}
				} else {
					sub := strings.Join(cur, ".")
					if seenPunkSubtables[sub] {
						return fail("%s declares the [%s] table more than once; TOML rejects duplicate definitions, so Codex cannot load the punk MCP entry and it cannot be inspected faithfully", configPath, sub)
					}
					seenPunkSubtables[sub] = true
				}
				if inPunkHeaders() {
					subtableHeaders = true
				}
				if inPunkEnvHeaders() {
					subtableEnvHeaders = true
				}
				if inPunkEnv() {
					subtableEnv = true
					sc.markTransport("env") // the subtable form of the env field
				}
				if inPunkOauth() {
					subtableOauth = true
					sc.markTransport("oauth")
				}
			}
			continue
		}
		if !underPunk() {
			// A key line outside the punk tables: still audited for the
			// dotted and inline representations that reach the punk entry
			// from the parent table or the document root - skipped, they
			// would read inherited connection identity as absent.
			if err := auditCodexForeignLine(lines, &i, cur, trim, configPath); err != nil {
				sc.Err = err
				return sc
			}
			continue
		}
		key, rest, ok := parseTOMLKey(trim)
		if !ok {
			return fail("cannot parse line %d of %s; the punk MCP entry cannot be inspected faithfully", i+1, configPath)
		}
		rest = strings.TrimSpace(rest)
		if strings.HasPrefix(rest, ".") {
			// A dotted key REDEFINES its first segment as a table. Inside
			// the header and env subtables every value must be a string,
			// and inside the punk table any identity- or transport-shaped
			// field redefined this way is a representation this inspector
			// does not support: conservatively unknown, never silently
			// skipped. Other dotted keys are ignored like any untracked key.
			if inPunkHeaders() || inPunkEnvHeaders() || inPunkEnv() || inPunkOauth() {
				return fail("%s line %d defines %s with a dotted key, which this inspector does not support; the punk MCP entry cannot be inspected faithfully", configPath, i+1, key)
			}
			if inPunk() && codexPunkEntryKeys[key] {
				return fail("%s defines %s with a dotted key, which this inspector does not support", configPath, key)
			}
			continue
		}
		if !strings.HasPrefix(rest, "=") {
			return fail("cannot parse line %d of %s; the punk MCP entry cannot be inspected faithfully", i+1, configPath)
		}
		value := strings.TrimSpace(rest[1:])
		if inPunkHeaders() || inPunkEnvHeaders() {
			val, tail, ok := parseTOMLString(value)
			if !ok || !tomlTrailingOK(tail) {
				return fail("%s line %d holds a header entry this inspector does not support; the entry's headers cannot be read faithfully", configPath, i+1)
			}
			dst := envHeaders
			if inPunkHeaders() {
				dst = headers
			}
			if _, dup := dst[key]; dup {
				return fail("%s line %d redefines the %s header; TOML rejects duplicate definitions, so Codex cannot load the punk MCP entry and it cannot be inspected faithfully", configPath, i+1, key)
			}
			dst[key] = val
			continue
		}
		if inPunkEnv() {
			// env maps variable names to string VALUES; anything else Codex
			// cannot decode, and the entry cannot be established.
			if _, tail, ok := parseTOMLString(value); !ok || !tomlTrailingOK(tail) {
				return fail("%s line %d holds an env entry this inspector does not support; the field cannot be read faithfully", configPath, i+1)
			}
			if envKeys[key] {
				return fail("%s line %d redefines the %s env entry; TOML rejects duplicate definitions, so Codex cannot load the punk MCP entry and it cannot be inspected faithfully", configPath, i+1, key)
			}
			envKeys[key] = true
			continue
		}
		if inPunkOauth() {
			continue // presence was recorded at the subtable header; oauth's inner fields are not punk identity
		}
		if !inPunk() {
			continue // a deeper punk subtable's key: not a top-level field
		}
		if !codexPunkEntryKeys[key] {
			// A key outside codexPunkEntryKeys (tool filters and any
			// further fields upstream adds) carries neither transport nor
			// punk identity and is ignored - but when its value is an
			// array or inline table, it may still SPAN further lines
			// (TOML arrays and inline tables can be multi-line), and those
			// continuation lines must be consumed here or the main loop
			// will read them as fresh, unparseable lines (or, for a line
			// beginning with '[', misread them as a table header).
			switch {
			case strings.HasPrefix(value, "["):
				if _, _, ok := foldTOMLValue(lines, &i, value, cutTOMLArray); !ok {
					return fail("%s has an unterminated array on line %d; the punk MCP entry cannot be inspected faithfully", configPath, i+1)
				}
			case strings.HasPrefix(value, "{"):
				if _, _, ok := foldTOMLValue(lines, &i, value, cutTOMLInlineTable); !ok {
					return fail("%s has an unterminated inline table on line %d; the punk MCP entry cannot be inspected faithfully", configPath, i+1)
				}
			}
			continue
		}
		if seenPunkFields[key] {
			return fail("%s line %d redefines %s in the [mcp_servers.punk] table; TOML rejects duplicate definitions, so Codex cannot load the entry and it cannot be inspected faithfully", configPath, i+1, key)
		}
		seenPunkFields[key] = true
		switch key {
		case "url", "bearer_token_env_var":
			val, tail, ok := parseTOMLString(value)
			if !ok || !tomlTrailingOK(tail) {
				return fail("%s line %d holds a %s value this inspector does not support; the field cannot be read faithfully", configPath, i+1, key)
			}
			if key == "url" {
				sc.Endpoint = val
				sc.hasURL = true
			} else {
				sc.BearerEnv = val
				sc.hasBearerEnv = true
			}
			sc.markTransport(key)
		case "enabled":
			// Codex filters enabled = false entries (upstream codex-rs
			// connection_manager.rs): installed, but not a working
			// connection. A non-boolean activation state cannot be read
			// faithfully and is conservatively unknown.
			switch {
			case strings.HasPrefix(value, "false") && tomlTrailingOK(value[len("false"):]):
				sc.Disabled = true
				sc.enabledSet = true
			case strings.HasPrefix(value, "true") && tomlTrailingOK(value[len("true"):]):
				sc.enabledSet, sc.enabledVal = true, true
			default:
				return fail("%s line %d holds an enabled value this inspector does not support; whether Codex opens the entry cannot be established", configPath, i+1)
			}
		case "http_headers_helper":
			// The helper command resolves part of the entry's headers at
			// connect time (upstream codex-rs config/src/mcp_types.rs);
			// its presence marks the effective headers unverified. The
			// value is parsed only to prove the line is well-formed -
			// the helper is NEVER executed.
			if _, tail, ok := parseTOMLString(value); !ok || !tomlTrailingOK(tail) {
				return fail("%s line %d holds an http_headers_helper value this inspector does not support; the field cannot be read faithfully", configPath, i+1)
			}
			sc.HeadersHelper = true
			sc.markTransport(key)
		case "http_headers", "env_http_headers":
			if !strings.HasPrefix(value, "{") {
				return fail("%s line %d holds an %s value this inspector does not support; the entry's headers cannot be read faithfully", configPath, i+1, key)
			}
			pairs, perr := parseCodexInlineHeaders(lines, &i, value, configPath, key)
			if perr != nil {
				sc.Err = perr
				return sc
			}
			if key == "http_headers" {
				inlineHeaders = true
				for k, v := range pairs {
					headers[k] = v
				}
			} else {
				inlineEnvHeaders = true
				for k, v := range pairs {
					envHeaders[k] = v
				}
			}
			sc.markTransport(key)
		case "command", "cwd", "bearer_token", "oauth_resource":
			// Presence only: the value is parsed to prove the line is
			// well-formed and then DISCARDED - bearer_token above all is a
			// secret and is never kept. What the merged entry needs is that
			// the field is SET, because upstream Codex rejects
			// transport-incompatible combinations of these fields
			// (config/src/mcp_types.rs RawMcpServerConfig::try_from).
			if _, tail, ok := parseTOMLString(value); !ok || !tomlTrailingOK(tail) {
				return fail("%s line %d holds a %s value this inspector does not support; the field cannot be read faithfully", configPath, i+1, key)
			}
			sc.markTransport(key)
		case "auth":
			// The upstream McpServerAuth enum (config/src/mcp_types.rs:177-187)
			// supports exactly oauth and chatgpt: an explicit auth value
			// outside the enum is one Codex cannot load and an auth identity
			// this inspector cannot establish - conservatively unknown,
			// never a guess. The value is compared against the enum and then
			// DISCARDED; it is never kept and never echoed.
			val, tail, ok := parseTOMLString(value)
			if !ok || !tomlTrailingOK(tail) {
				return fail("%s line %d holds an auth value this inspector does not support; the field cannot be read faithfully", configPath, i+1)
			}
			if !codexPunkAuthKinds[val] {
				return fail("%s line %d holds an auth kind outside the enum upstream Codex supports (oauth, chatgpt); the punk MCP entry cannot be inspected faithfully", configPath, i+1)
			}
			sc.markTransport(key)
		case "args", "env_vars":
			// Arrays (args of strings; env_vars of strings or name/source
			// inline tables), possibly spanning lines. Anything else Codex
			// cannot decode, and the entry cannot be established.
			if !strings.HasPrefix(value, "[") {
				return fail("%s line %d holds an %s value this inspector does not support; the field cannot be read faithfully", configPath, i+1, key)
			}
			body, tail, ok := foldTOMLValue(lines, &i, value, cutTOMLArray)
			if !ok {
				return fail("%s has an unterminated %s array; the field cannot be read faithfully", configPath, key)
			}
			if !tomlTrailingOK(tail) {
				return fail("%s line %d holds an %s array with trailing content this inspector does not support; the field cannot be read faithfully", configPath, i+1, key)
			}
			elems, ok := splitTOMLElems(body)
			if !ok {
				return fail("%s line %d holds %s entries this inspector does not support; the field cannot be read faithfully", configPath, i+1, key)
			}
			for _, el := range elems {
				if key == "args" {
					if !tomlStringElemOK(el) {
						return fail("%s line %d holds a non-string args element; the field cannot be read faithfully", configPath, i+1)
					}
				} else if !tomlEnvVarElemOK(el) {
					return fail("%s line %d holds an env_vars element this inspector does not support; the field cannot be read faithfully", configPath, i+1)
				}
			}
			sc.markTransport(key)
		case "env":
			// The inline-table form of env maps variable names to string
			// values (the subtable form is handled at its header).
			if !strings.HasPrefix(value, "{") {
				return fail("%s line %d holds an env value this inspector does not support; the field cannot be read faithfully", configPath, i+1)
			}
			body, tail, ok := foldTOMLValue(lines, &i, value, cutTOMLInlineTable)
			if !ok {
				return fail("%s has an unterminated env inline table; the field cannot be read faithfully", configPath)
			}
			if !tomlTrailingOK(tail) {
				return fail("%s line %d holds an env inline table with trailing content this inspector does not support; the field cannot be read faithfully", configPath, i+1)
			}
			if _, ok := parseTOMLInlinePairs(body); !ok {
				return fail("%s line %d holds env entries this inspector does not support; the field cannot be read faithfully", configPath, i+1)
			}
			inlineEnv = true
			sc.markTransport(key)
		case "oauth":
			// Presence only: the inline-table form is cut to prove it is
			// well-formed; oauth's inner fields are not punk identity.
			if !strings.HasPrefix(value, "{") {
				return fail("%s line %d holds an oauth value this inspector does not support; the field cannot be read faithfully", configPath, i+1)
			}
			if _, tail, ok := foldTOMLValue(lines, &i, value, cutTOMLInlineTable); !ok || !tomlTrailingOK(tail) {
				return fail("%s line %d holds an oauth inline table this inspector does not support; the field cannot be read faithfully", configPath, i+1)
			}
			inlineOauth = true
			sc.markTransport(key)
		case "startup_timeout_sec", "tool_timeout_sec", "default_tools_approval_mode":
			// Recognized benign punk-entry knobs (upstream
			// config/src/mcp_types.rs): their values shape neither the
			// transport nor the identity. Enumerating them keeps the
			// supported boundary explicit, and a duplicate definition is
			// rejected above exactly like any other recognized key's.
		}
	}

	if inlineHeaders && subtableHeaders {
		return fail("%s defines http_headers both as an inline table and as a subtable; which headers Codex sends cannot be established", configPath)
	}
	if inlineEnvHeaders && subtableEnvHeaders {
		return fail("%s defines env_http_headers both as an inline table and as a subtable; which headers Codex sends cannot be established", configPath)
	}
	if inlineEnv && subtableEnv {
		return fail("%s defines env both as an inline table and as a subtable; TOML rejects the redefinition, so Codex cannot load the punk MCP entry and it cannot be inspected faithfully", configPath)
	}
	if inlineOauth && subtableOauth {
		return fail("%s defines oauth both as an inline table and as a subtable; TOML rejects the redefinition, so Codex cannot load the punk MCP entry and it cannot be inspected faithfully", configPath)
	}
	// The subtable spellings of the header fields carry the same
	// transport shape as their inline forms (the inline forms marked
	// their presence at the key line already).
	if subtableHeaders {
		sc.markTransport("http_headers")
	}
	if subtableEnvHeaders {
		sc.markTransport("env_http_headers")
	}
	sc.Installed = punkTables == 1
	// A punk subtable fragment without a [mcp_servers.punk] table of
	// its own is not an installed entry, but it is NOT nothing either:
	// Codex's layer merge (upstream config/src/merge.rs:58-132) folds
	// its fields into the entry another layer defines, so the merge
	// must see this scope contribute - a [mcp_servers.punk.env]-only
	// layer shapes the merged entry's transport exactly like a header
	// fragment shapes its headers.
	sc.fragment = punkTables == 0 && (subtableHeaders || subtableEnvHeaders || subtableEnv || subtableOauth)
	// Identity-relevant header families for the cross-layer merge,
	// kept secret-safe: pin values are namespaces (safe to report),
	// env entries hold var NAMES, and an Authorization header is
	// presence only - its value is discarded here, never stored.
	for k, v := range headers {
		switch strings.ToLower(k) {
		case "x-punk-namespace":
			if sc.nsStatic == nil {
				sc.nsStatic = map[string]string{}
			}
			sc.nsStatic[k] = v
		case "authorization":
			if sc.authStatic == nil {
				sc.authStatic = map[string]bool{}
			}
			sc.authStatic[k] = true
		}
	}
	for k, v := range envHeaders {
		switch strings.ToLower(k) {
		case "x-punk-namespace":
			if sc.nsEnv == nil {
				sc.nsEnv = map[string]string{}
			}
			sc.nsEnv[k] = v
		case "authorization":
			if sc.authEnv == nil {
				sc.authEnv = map[string]string{}
			}
			sc.authEnv[k] = v
		}
	}
	eff, ok := effectiveCodexHeaders(headers, envHeaders)
	if !ok {
		return fail("%s carries headers whose names differ only by case with disagreeing values; which header Codex sends cannot be established", configPath)
	}
	sc.Pin = eff["x-punk-namespace"]
	_, sc.HeaderAuth = eff["authorization"]
	return sc
}

// parseCodexInlineHeaders parses the value of an http_headers or
// env_http_headers assignment that begins on lines[*i] as an inline
// table, folding in further lines when the table spans them (TOML
// inline tables may span multiple lines). Errors name the file, the
// line and the field identity only - never a raw line or value, which
// can carry an Authorization secret.
func parseCodexInlineHeaders(lines []string, i *int, value, configPath, field string) (map[string]string, error) {
	joined := value
	body, tail, ok := cutTOMLInlineTable(joined)
	for !ok {
		*i++
		if *i >= len(lines) {
			return nil, fmt.Errorf("%s has an unterminated %s inline table; the entry's headers cannot be read faithfully", configPath, field)
		}
		joined += "\n" + strings.TrimSpace(lines[*i])
		body, tail, ok = cutTOMLInlineTable(joined)
	}
	if !tomlTrailingOK(tail) {
		return nil, fmt.Errorf("%s line %d holds an %s inline table with trailing content this inspector does not support; the entry's headers cannot be read faithfully", configPath, *i+1, field)
	}
	pairs, ok := parseTOMLInlinePairs(body)
	if !ok {
		return nil, fmt.Errorf("%s line %d holds %s entries this inspector does not support; the entry's headers cannot be read faithfully", configPath, *i+1, field)
	}
	return pairs, nil
}

// effectiveCodexHeaders merges the punk entry's static http_headers
// with its env_http_headers exactly the way Codex's rmcp client
// applies them (upstream codex-rs rmcp-client/src/utils.rs): static
// headers first, then every env_http_headers entry whose named env var
// is set AND resolves to a non-blank value (Codex SKIPS an
// env-resolved header whose value is empty or whitespace-only - the
// static header stays effective and must never be reported as replaced
// by a blank pin Codex does not send), into one case-insensitive
// header map - so an env-resolved header wins over a same-named static
// one, and header names match case-insensitively. Static entries whose names differ only by case,
// and env-resolved entries that collide the same way, report ok=false
// when their values disagree: Codex inserts them from an unordered
// map, so which one wins cannot be established faithfully and the
// entry is conservatively unknown. Env var VALUES are read only to
// derive the effective headers; the resolved map holds header values
// (namespace pins are safe to report, and the caller records only the
// PRESENCE of an Authorization header, never its value).
func effectiveCodexHeaders(static, env map[string]string) (map[string]string, bool) {
	sorted := func(m map[string]string) []string {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return keys
	}
	eff := map[string]string{}
	for _, k := range sorted(static) {
		lk := strings.ToLower(k)
		if prev, dup := eff[lk]; dup && prev != static[k] {
			return nil, false
		}
		eff[lk] = static[k]
	}
	fromEnv := map[string]bool{}
	for _, k := range sorted(env) {
		v, ok := os.LookupEnv(env[k])
		if !ok || strings.TrimSpace(v) == "" {
			continue // Codex skips an env header whose variable is not set or resolves empty/blank
		}
		lk := strings.ToLower(k)
		if fromEnv[lk] && eff[lk] != v {
			return nil, false
		}
		eff[lk] = v
		fromEnv[lk] = true
	}
	return eff, true
}

// EnumerateCodexMCPScopes inspects every applicable config.toml scope,
// in the order given (highest layer first). One scope is never the
// effective entry on its own: Codex recursively merges MCP table
// FIELDS across config layers rather than replacing whole entries
// (upstream config/src/merge.rs:58-132, state.rs:343), so callers
// proving what connection Codex opens fold the enumerated scopes with
// MergeCodexMCPScopes instead of reading the topmost installed entry.
// Paths aliasing the same file are deduped via sameFile; per-file
// inspection failures are reported on that scope's Err, which callers
// must treat as unknown, never as absent or unpinned.
func EnumerateCodexMCPScopes(configPaths ...string) []CodexMCPScope {
	var out []CodexMCPScope
	for _, p := range distinctScopePaths(configPaths) {
		out = append(out, inspectCodexMCPScope(p))
	}
	return out
}

// CodexMCPEffective is the punk MCP entry Codex actually opens: the
// per-scope contributions EnumerateCodexMCPScopes reports, folded
// field-wise the way upstream Codex merges config layers
// (config/src/merge.rs:58-132, state.rs:343) - higher layers win per
// FIELD, fields no higher layer defines stay inherited from lower
// ones, and header-subtable fragments contribute even from layers
// defining no [mcp_servers.punk] table of their own. From names the
// config layer each field's effective value came from. Unknown is
// non-empty when the effective entry cannot be established safely
// (an uninspectable layer, a cross-layer case-variant pin conflict,
// a transport shape upstream Codex rejects - stdio fields mixed with
// HTTP fields across layers - fragment fields with no layer defining
// the table, or no punk config
// at all); callers must then report the connection unverified, never
// default the missing fields to false/empty.
type CodexMCPEffective struct {
	Contributing      []string // files contributing punk fields, highest layer first
	Defined           bool     // at least one layer defines [mcp_servers.punk] itself
	Endpoint          string   // effective url ("" = no layer defines one)
	EndpointFrom      string
	BearerEnv         string // effective bearer_token_env_var NAME ("" = no layer defines one); a token value is never read
	BearerEnvFrom     string
	Disabled          bool // effective enabled = false: the highest layer defining enabled wins
	DisabledFrom      string
	HeadersHelper     bool // a layer defines http_headers_helper; a merge can replace but never remove it
	HeadersHelperFrom string
	Pin               string // effective X-Punk-Namespace of the merged headers ("" = unpinned)
	HeaderAuth        bool   // the merged headers carry an effective Authorization entry (presence; the value is never held)
	HeaderAuthFrom    string // layers contributing effective Authorization entries, highest first
	Unknown           string
}

// MergeCodexMCPScopes folds the enumerated config scopes - highest
// layer first, the order EnumerateCodexMCPScopes returns - into the
// effective punk MCP entry. Codex recursively merges MCP table FIELDS
// across config layers rather than replacing whole entries (upstream
// config/src/merge.rs:58-132, state.rs:343): a field the project
// layer's entry lacks inherits from the global layer, and a
// header-subtable fragment in a layer defining no [mcp_servers.punk]
// table still contributes to the merged entry when another layer
// defines the table. A top-entry-only view therefore misses an
// inherited enabled = false, an inherited http_headers_helper, an
// inherited bearer_token_env_var, an inherited Authorization header
// and an inherited TRANSPORT field - false alignment claims all. The
// merged entry's transport shape is validated exactly the way upstream
// Codex validates it (config/src/mcp_types.rs
// RawMcpServerConfig::try_from): a stdio command mixed with any HTTP
// field, or a url mixed with any stdio field, is an entry Codex
// refuses to load, and Unknown says so - cross-layer inheritance is
// exactly how such a mix arises without any single layer looking
// wrong. When NO layer defines the
// table itself, the fragment fields merge into an entry that does not
// exist; whether Codex opens a punk connection is then honestly
// unknown, and Unknown says so. Headers merge per key (the higher
// layer's spelling wins), then resolve through effectiveCodexHeaders
// exactly the way Codex's rmcp client applies them, so the reported
// pin is the one Codex actually sends and a cross-layer case-variant
// conflict with disagreeing values is unknown, never a guess.
// Diagnostics stay secret-safe: Authorization travels as presence
// only and no header value is ever rendered. The helper is NEVER
// executed.
func MergeCodexMCPScopes(scopes []CodexMCPScope) CodexMCPEffective {
	m := CodexMCPEffective{}
	files := make([]string, 0, len(scopes))
	for _, sc := range scopes {
		files = append(files, sc.File)
		if sc.Err != nil {
			m.Unknown = fmt.Sprintf("could not inspect %s: %v", sc.File, sc.Err)
			return m
		}
	}
	// Accumulate lowest layer to highest so each higher layer's DEFINED
	// fields override; a field no higher layer defines keeps the
	// inherited lower value instead of defaulting to false/empty.
	var contributing []string
	staticNS := map[string]string{}
	envNS := map[string]string{}
	staticAuth := map[string]bool{}
	envAuth := map[string]string{}
	tpFrom := map[string]string{} // transport-shaped field -> highest layer defining it
	for i := len(scopes) - 1; i >= 0; i-- {
		sc := scopes[i]
		if !sc.Installed && !sc.fragment {
			continue
		}
		contributing = append(contributing, sc.File)
		if sc.Installed {
			m.Defined = true
		}
		if sc.hasURL {
			m.Endpoint, m.EndpointFrom = sc.Endpoint, sc.File
		}
		if sc.hasBearerEnv {
			m.BearerEnv, m.BearerEnvFrom = sc.BearerEnv, sc.File
		}
		if sc.enabledSet {
			m.Disabled, m.DisabledFrom = !sc.enabledVal, sc.File
		}
		if sc.HeadersHelper {
			m.HeadersHelper, m.HeadersHelperFrom = true, sc.File
		}
		for k, v := range sc.nsStatic {
			staticNS[k] = v
		}
		for k, v := range sc.nsEnv {
			envNS[k] = v
		}
		for k := range sc.authStatic {
			staticAuth[k] = true
		}
		for k, v := range sc.authEnv {
			envAuth[k] = v
		}
		for f := range sc.tpFields {
			tpFrom[f] = sc.File
		}
	}
	for i, j := 0, len(contributing)-1; i < j; i, j = i+1, j-1 {
		contributing[i], contributing[j] = contributing[j], contributing[i]
	}
	m.Contributing = contributing
	if !m.Defined {
		if len(contributing) == 0 {
			m.Unknown = "no [mcp_servers.punk] entry found in " + strings.Join(files, " or ")
		} else {
			m.Unknown = fmt.Sprintf("%s carry punk MCP entry fields but no config layer defines [mcp_servers.punk] itself; the fragments merge into an entry that does not exist, so whether Codex opens a punk connection cannot be established", strings.Join(contributing, " and "))
		}
		return m
	}
	// The transport shape of the MERGED entry, validated exactly the way
	// upstream Codex validates it (config/src/mcp_types.rs
	// RawMcpServerConfig::try_from): a stdio entry (command) rejects
	// every HTTP field, a streamable-HTTP entry (url) rejects every
	// stdio field, and bearer_token is rejected either way. Codex
	// merges fields across layers BEFORE this validation, so the mixed
	// shape arises precisely when no single layer looks wrong - a
	// global command inherited next to a project url is an entry Codex
	// refuses to load, never a connection whose alignment verify can
	// prove. Diagnostics name fields and files only, never values.
	if from, ok := tpFrom["command"]; ok {
		for _, f := range []string{"url", "bearer_token_env_var", "bearer_token", "http_headers_helper", "http_headers", "env_http_headers", "oauth", "oauth_resource", "auth"} {
			if ffrom, bad := tpFrom[f]; bad {
				m.Unknown = fmt.Sprintf("the merged punk MCP entry sets %s (from %s), which upstream Codex rejects for the stdio command transport (from %s): an entry Codex refuses to load is not a connection whose alignment can be verified", f, ffrom, from)
				return m
			}
		}
	} else if from, ok := tpFrom["url"]; ok {
		for _, f := range []string{"args", "env", "env_vars", "cwd", "bearer_token"} {
			if ffrom, bad := tpFrom[f]; bad {
				m.Unknown = fmt.Sprintf("the merged punk MCP entry sets %s (from %s), which upstream Codex rejects for the streamable HTTP url transport (from %s): an entry Codex refuses to load is not a connection whose alignment can be verified", f, ffrom, from)
				return m
			}
		}
	}
	eff, ok := effectiveCodexHeaders(staticNS, envNS)
	if !ok {
		m.Unknown = fmt.Sprintf("the merged punk MCP entry carries X-Punk-Namespace headers whose names differ only by case with disagreeing values across %s; which header Codex sends cannot be established", strings.Join(contributing, " and "))
		return m
	}
	m.Pin = eff["x-punk-namespace"]
	// Authorization presence across layers, values never kept: a static
	// entry survives any merge (a higher layer can replace its value
	// but never remove the header), and an env entry is effective when
	// the merged spelling's variable resolves set and non-blank - Codex
	// skips a blank one, the way effectiveCodexHeaders skips it.
	var authFrom []string
	for _, sc := range scopes {
		entry := len(sc.authStatic) > 0
		for k, v := range sc.authEnv {
			if envAuth[k] != v {
				continue // a higher layer replaced this spelling's variable
			}
			if resolved, set := os.LookupEnv(v); set && strings.TrimSpace(resolved) != "" {
				entry = true
			}
		}
		if entry {
			m.HeaderAuth = true
			authFrom = append(authFrom, sc.File)
		}
	}
	m.HeaderAuthFrom = strings.Join(authFrom, " and ")
	return m
}

// CodexMCPPin inspects the config.toml at configPath for the punk MCP
// entry and reports its X-Punk-Namespace header ("" when the entry
// pins nothing). installed is false when the file is missing or has no
// [mcp_servers.punk] table at all. The parse is faithful to the
// supported TOML - basic AND literal strings, inline-table or subtable
// http_headers, comments, quoted table spellings - and a punk entry
// written in a representation this inspector does not support returns
// an error: conservatively unknown, never a guess that the entry is
// unpinned. Callers that also need the installed endpoint and
// credential identity use EnumerateCodexMCPScopes with
// MergeCodexMCPScopes: a single file is one layer's contribution, not
// the effective entry Codex merges across layers.
func CodexMCPPin(configPath string) (pin string, installed bool, err error) {
	sc := inspectCodexMCPScope(configPath)
	if sc.Err != nil {
		return "", false, sc.Err
	}
	return sc.Pin, sc.Installed, nil
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
// duplicate-integration defect). Aliases are normalized through
// parseTOMLTableHeader before comparison, so quoted spellings of the
// same table ([mcp_servers.punk] and [mcp_servers."punk"]) count as ONE
// alias rather than two - they name the identical table, not a
// duplicate. Detection only: aliases punk did not write are foreign
// config and are never edited here - the caller prints the list so the
// user can resolve them. A missing file or a config with at most one
// distinct matching alias returns nil. CRLF content is normalized
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
	seen := map[string]bool{}
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
		if u != want {
			continue
		}
		alias := body[h[2]:h[3]]
		if path, array, ok := parseTOMLTableHeader(body[h[0]:h[1]]); ok && !array && len(path) >= 2 {
			alias = strings.Join(path[1:], ".")
		}
		if seen[alias] {
			continue
		}
		seen[alias] = true
		aliases = append(aliases, alias)
	}
	if len(aliases) < 2 {
		return nil, nil
	}
	sort.Strings(aliases)
	return aliases, nil
}
