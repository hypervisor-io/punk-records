package hookcli

import "fmt"

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
