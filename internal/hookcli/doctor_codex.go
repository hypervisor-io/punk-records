package hookcli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// punk doctor codex (P4-C): the codex half of the Doctor registry, with
// the same PASS/FAIL/WARN line contract and read-only discipline the
// hermes report established. The codex-specific checks come from what the
// repo already knows about Codex:
//
//   - CODEX_HOME: $CODEX_HOME else ~/.codex - the same resolution
//     cmdConnectCodex and wake.go's codexControlSocket apply. Codex CLI
//     and Codex Desktop may not share one CODEX_HOME, so the report names
//     where the home came from.
//   - hooks.json (Claude-shaped, see connect_codex.go): punk's capture
//     groups per wired event, plus the inbox (--messaging) and wake
//     (--wake) groups detected by their own markers.
//   - config.toml: the [features] hooks flag - the one hook-gate state
//     punk can read from disk. Hook COMMAND trust itself is granted
//     interactively (Codex' "Hooks need review" prompt, once) and is not
//     readable from disk, so hook-trust is a standing WARN, never a
//     promise that hooks execute - and per openai/codex#35863 trusted
//     SessionStart command hooks never execute in Codex Desktop at all.
//     The Desktop delivery path is the scheduled punk-inbox recipe in
//     docs/agent-messaging.md#codex-desktop.
//   - the shared app-server daemon's control socket
//     ($CODEX_HOME/app-server-control/app-server-control.sock, the path
//     wake.go resolves and internal/nativewake targets): present means a
//     shared-daemon launch - the mode Codex Desktop uses - owns sessions
//     at this CODEX_HOME and native wake can target them. Direct CLI
//     launches never create it, so absence is only a WARN when wake
//     entries are actually wired.
//
// The remaining checks (credentials, server, messaging tools, last hook
// diagnostic) are the shared P1-D implementations.

// doctorCodexAgentRe finds the agent identity the punk config.toml block
// pins: codexManagedBlock writes it as "X-Punk-Agent" = "<agent>" inside
// http_headers. Only punk writes that header, so scanning the whole body
// is safe; the match is the raw TOML basic string (escapes are not
// decoded - an agent name with escaped quotes is an operator choice this
// report renders verbatim).
var doctorCodexAgentRe = regexp.MustCompile(`"X-Punk-Agent"\s*=\s*"([^"]*)"`)

func doctorCodex(ctx context.Context, o DoctorOptions) (DoctorReport, error) {
	rep := DoctorReport{Client: "codex"}
	add := func(status, name, format string, args ...any) {
		rep.Checks = append(rep.Checks, DoctorCheck{Name: name, Status: status, Message: fmt.Sprintf(format, args...)})
	}

	punkPath, err := os.Executable()
	if err != nil {
		return DoctorReport{}, fmt.Errorf("resolve punk executable path: %w", err)
	}

	home, homeSource, homeFromEnv, hooksPath := doctorCodexHome(o)
	configToml := filepath.Join(home, "config.toml")
	// The same path wake.go's codexControlSocket resolves; duplicated here
	// (not called) because doctor must report the path even when nothing
	// is listening, and wake.go's helper is wake-side scope.
	socketPath := filepath.Join(home, "app-server-control", "app-server-control.sock")
	dir := o.Dir
	if dir == "" {
		if dir, err = os.Getwd(); err != nil {
			return DoctorReport{}, fmt.Errorf("resolve working directory: %w", err)
		}
	}
	// ResolveServer can now fail (a malformed saved-credentials file fails
	// closed); doctor carries it into the credentials check instead of
	// aborting (see Doctor's contract).
	resolved, resolveErr := ResolveServer(o.URLFlag)
	serverURL, apiKey := resolved.URL, resolved.APIKey

	// codex-home: where the Codex home came from and whether it exists.
	if _, statErr := os.Stat(home); statErr != nil {
		switch {
		case homeFromEnv:
			add(DoctorFail, "codex-home", "CODEX_HOME %s does not exist - run punk connect codex, or unset CODEX_HOME to fall back to ~/.codex", home)
		default:
			add(DoctorFail, "codex-home", "no Codex home at %s - run punk connect codex first", home)
		}
	} else {
		add(DoctorPass, "codex-home", "%s", homeSource)
	}

	// config: hooks.json present and parseable. loadSettings treats a
	// missing file as an empty document, so existence is checked first -
	// doctor must report "not installed", not "installed and empty".
	var settings map[string]any
	if _, statErr := os.Stat(hooksPath); statErr != nil {
		if os.IsNotExist(statErr) {
			add(DoctorFail, "config", "no Codex hooks file at %s - run punk connect codex first", hooksPath)
		} else {
			add(DoctorFail, "config", "cannot read %s: %v", hooksPath, statErr)
		}
	} else if loaded, _, loadErr := loadSettings(hooksPath); loadErr != nil {
		add(DoctorFail, "config", "%s is not usable: %v", hooksPath, loadErr)
	} else {
		settings = loaded
		add(DoctorPass, "config", "%s present and parseable", hooksPath)
	}

	// hook-entries: punk's capture groups on every wired event, detected
	// with the same managed-group rules a reconnect dedups by (the
	// fallback rules tolerate a relocated punk binary). The inbox and
	// wake groups are collected on the same pass for the messaging and
	// app-server checks.
	var wiring doctorCodexWiring
	if settings == nil {
		add(DoctorFail, "hook-entries", "cannot check hook entries: hooks.json missing or unparseable")
	} else {
		wiring = doctorReadCodexWiring(settings, punkPath)
		var missing []string
		for _, ev := range codexHookEvents {
			if !wiring.capture[ev] {
				missing = append(missing, ev)
			}
		}
		switch {
		case len(missing) == 0:
			add(DoctorPass, "hook-entries", "punk capture groups wired on all %d events (%s)",
				len(codexHookEvents), strings.Join(codexHookEvents, ", "))
		case len(missing) == len(codexHookEvents):
			add(DoctorFail, "hook-entries", "no punk hook entries in %s - run punk connect codex", hooksPath)
		default:
			add(DoctorFail, "hook-entries", "punk capture groups missing on %s - rerun punk connect codex",
				strings.Join(missing, ", "))
		}
	}

	// hook-trust: the [features] hooks flag is the only hook-gate state
	// readable from disk; hooks = false fails the report. Command trust
	// itself is interactive and unverifiable, and Desktop never runs
	// SessionStart command hooks (openai/codex#35863), so the healthy
	// state is a WARN that promises nothing.
	tomBody, tomErr := os.ReadFile(configToml)
	doctorCodexHookTrustCheck(add, configToml, tomBody, tomErr)

	// app-server: the shared daemon's control socket. Present means
	// Desktop-style (shared-daemon) sessions run at this CODEX_HOME and
	// native wake has a target; absence is normal for direct CLI launches
	// and only degrades to WARN when wake entries are wired.
	doctorCodexAppServerCheck(add, socketPath, wiring.wake)

	// credentials: the shared check, with the agent identity read from
	// the punk config.toml block.
	urlOK := doctorCredentialsCheck(add, o.URLFlag, resolved, resolveErr, doctorCodexAgent(tomBody), "config.toml punk block X-Punk-Agent")

	// server: the shared reachability probe.
	doctorServerCheck(ctx, add, serverURL, apiKey, urlOK)

	// messaging: the inbox groups' server-side half, checked only when
	// --messaging is actually wired.
	switch {
	case settings == nil:
		add(DoctorWarn, "messaging", "cannot check messaging: hooks.json missing or unparseable")
	case len(wiring.inboxEvents) == 0:
		add(DoctorPass, "messaging", "not configured (no punk inbox group in %s); messaging tools check skipped", hooksPath)
	case !urlOK:
		add(DoctorFail, "messaging", "punk inbox groups are wired (%s), but server URL %q is not usable",
			strings.Join(wiring.inboxEvents, ", "), serverURL)
	default:
		endpoint := mcpEndpoint(serverURL)
		wired := strings.Join(wiring.inboxEvents, ", ")
		tools, toolsErr := doctorListTools(ctx, endpoint, apiKey)
		switch {
		case toolsErr != nil:
			add(DoctorFail, "messaging", "punk inbox groups are wired (%s), but listing tools at %s failed: %v", wired, endpoint, toolsErr)
		default:
			if missing := doctorMissingMessagingTools(tools); len(missing) > 0 {
				add(DoctorFail, "messaging", "punk inbox groups are wired (%s), but the server does not advertise %s at %s - enable messaging server-side (docs/agent-messaging.md)",
					wired, strings.Join(missing, ", "), endpoint)
			} else {
				add(DoctorPass, "messaging", "punk inbox groups wired on %s; server advertises %s at %s",
					wired, strings.Join(doctorMessagingTools, ", "), endpoint)
			}
		}
	}

	// hook-diagnostic: the shared check, on the namespace the capture
	// command pins (--ns, from punk connect codex --project) when it
	// pins one.
	doctorDiagnosticCheck(ctx, add, serverURL, apiKey, dir, urlOK, "codex", wiring.pinnedNS)

	return rep, nil
}

// doctorCodexHome resolves the Codex home to inspect, its hooks.json and
// a human-readable source phrase for the codex-home line. Precedence:
// --config names the hooks.json directly (its DIRECTORY becomes the
// CODEX_HOME scope, which also covers a --project ./.codex install), else
// $CODEX_HOME, else ~/.codex - the resolution cmdConnectCodex and
// codexControlSocket apply. homeFromEnv reports whether CODEX_HOME named
// the home, for the failure-message split.
func doctorCodexHome(o DoctorOptions) (home, source string, homeFromEnv bool, hooksPath string) {
	if o.ConfigPath != "" {
		return filepath.Dir(o.ConfigPath),
			fmt.Sprintf("%s resolved from the directory of --config %s, directory present", filepath.Dir(o.ConfigPath), o.ConfigPath),
			false, o.ConfigPath
	}
	if env := strings.TrimSpace(os.Getenv("CODEX_HOME")); env != "" {
		return env,
			fmt.Sprintf("%s resolved from the CODEX_HOME environment variable, directory present", env),
			true, filepath.Join(env, "hooks.json")
	}
	home = filepath.Join(".codex")
	if h, err := os.UserHomeDir(); err == nil {
		home = filepath.Join(h, ".codex")
	}
	return home,
		fmt.Sprintf("%s resolved as the default (CODEX_HOME unset - Codex CLI and Desktop may use different homes), directory present", home),
		false, filepath.Join(home, "hooks.json")
}

// doctorCodexWiring is what doctor reads out of a parsed hooks.json: the
// events carrying punk capture groups, the events carrying punk inbox
// groups (in codexHookEvents order), whether any punk wake group is
// installed, and the --ns pin of the first capture command.
type doctorCodexWiring struct {
	capture     map[string]bool
	inboxEvents []string
	wake        bool
	pinnedNS    string
}

// doctorReadCodexWiring collects the punk-managed groups from a parsed
// hooks.json, read-only. SessionEnd is scanned too - punk never wires a
// capture or inbox group there, but --wake installs its stop entry on it.
func doctorReadCodexWiring(settings map[string]any, punkPath string) doctorCodexWiring {
	w := doctorCodexWiring{capture: map[string]bool{}}
	hooksAny, ok := settings["hooks"].(map[string]any)
	if !ok {
		return w
	}
	events := append(append([]string{}, codexHookEvents...), "SessionEnd")
	for _, ev := range events {
		arr, ok := hooksAny[ev].([]any)
		if !ok {
			continue
		}
		for _, g := range arr {
			switch {
			case isPunkManagedInboxGroup(g, punkPath, "codex"):
				w.inboxEvents = append(w.inboxEvents, ev)
			case isPunkManagedWakeGroup(g, punkPath, "codex"):
				w.wake = true
			case isPunkManagedGroup(g, punkPath):
				w.capture[ev] = true
				if w.pinnedNS == "" {
					w.pinnedNS = doctorHookCommandNS(doctorGroupFirstCommand(g))
				}
			}
		}
	}
	return w
}

// doctorGroupFirstCommand returns the first handler command of a
// Claude-shaped hook group, or "" for any shape that is not one.
func doctorGroupFirstCommand(group any) string {
	m, ok := group.(map[string]any)
	if !ok {
		return ""
	}
	hooks, ok := m["hooks"].([]any)
	if !ok || len(hooks) == 0 {
		return ""
	}
	hm, ok := hooks[0].(map[string]any)
	if !ok {
		return ""
	}
	cmd, _ := hm["command"].(string)
	return cmd
}

// doctorHookCommandNS extracts the --ns pin from a punk hook command
// ("... hook --url <URL> --from codex --ns <ns>"), or "" when unpinned.
// Namespace values are slugs (no whitespace), so the token ends at the
// next space.
func doctorHookCommandNS(cmd string) string {
	i := strings.Index(cmd, " --ns ")
	if i < 0 {
		return ""
	}
	rest := cmd[i+len(" --ns "):]
	if j := strings.IndexAny(rest, " \t"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// doctorCodexHookTrustCheck renders the hook-trust line. The [features]
// hooks flag is scoped to its TOML table exactly the way
// ConnectCodexConfig scopes it (header to next header): hooks = false
// disables every hook and fails the report; anything else is the standing
// WARN - command trust is granted interactively and never readable from
// disk, and openai/codex#35863 means trusted SessionStart command hooks
// never execute in Codex Desktop, so this line must never promise hook
// execution there.
func doctorCodexHookTrustCheck(add func(status, name, format string, args ...any), configToml string, tomBody []byte, tomErr error) {
	const desktop = `trusted SessionStart command hooks never execute in Codex Desktop (openai/codex#35863) - Desktop delivery uses the scheduled punk-inbox recipe (docs/agent-messaging.md#codex-desktop)`
	const interactive = `hook trust is granted interactively (Codex "Hooks need review" prompt, once) and is not readable from disk`
	var body string
	if tomErr == nil {
		body = strings.ReplaceAll(string(tomBody), "\r\n", "\n")
	}
	flag := doctorCodexFeaturesHooks(body)
	switch flag {
	case "false":
		add(DoctorFail, "hook-trust", "%s sets hooks = false under [features] - Codex runs no hooks at all; punk hooks need hooks = true (change it by hand; punk never edits that key)", configToml)
		return
	}
	var flagNote string
	switch {
	case tomErr != nil && !os.IsNotExist(tomErr):
		flagNote = fmt.Sprintf("the [features] hooks flag cannot be checked (%s is unreadable: %v)", configToml, tomErr)
	case body == "":
		flagNote = fmt.Sprintf("no config.toml at %s to check the [features] hooks flag (punk connect codex writes hooks = true)", configToml)
	case flag == "true":
		flagNote = fmt.Sprintf("[features] hooks = true is set in %s", configToml)
	default:
		flagNote = fmt.Sprintf("no hooks flag found under [features] in %s (punk connect codex writes hooks = true)", configToml)
	}
	add(DoctorWarn, "hook-trust", "%s; %s; %s", interactive, flagNote, desktop)
}

// doctorCodexFeaturesHooks reports the hooks flag under the config.toml
// body's [features] table: "true", "false", or "" when the table or the
// flag is absent. The scoping mirrors ConnectCodexConfig's: from the
// [features] header to the next table header, so a hooks key in another
// table never counts.
func doctorCodexFeaturesHooks(body string) string {
	if body == "" {
		return ""
	}
	loc := featuresHdrRe.FindStringIndex(body)
	if loc == nil {
		return ""
	}
	rest := body[loc[1]:]
	next := len(rest)
	if nl := tomlHeaderRe.FindStringIndex(rest); nl != nil {
		next = nl[0]
	}
	if m := featuresHooksRe.FindStringSubmatch(rest[:next]); m != nil {
		return m[1]
	}
	return ""
}

// doctorCodexAgent reads the agent identity the punk config.toml block
// pins, or "" when no block (or no X-Punk-Agent header) is installed.
func doctorCodexAgent(tomBody []byte) string {
	if m := doctorCodexAgentRe.FindSubmatch(tomBody); m != nil {
		return string(m[1])
	}
	return ""
}

// doctorCodexAppServerCheck renders the app-server line: presence of the
// shared daemon's control socket. A live socket means sessions at this
// CODEX_HOME are owned by a shared app-server daemon - the launch mode
// Codex Desktop uses and the only kind internal/nativewake's codex
// transport can target. Absence is normal for direct CLI launches (they
// never create it) and honest as a PASS while no wake entries are wired;
// wired wake entries with no socket degrade to WARN because native wake
// will report unavailable until a shared-daemon session runs here.
func doctorCodexAppServerCheck(add func(status, name, format string, args ...any), socketPath string, wakeWired bool) {
	st, err := os.Stat(socketPath)
	switch {
	case err == nil && st.Mode()&os.ModeSocket != 0:
		add(DoctorPass, "app-server", "app-server control socket present at %s - a shared app-server daemon owns sessions at this CODEX_HOME (the launch mode Codex Desktop uses); native wake can target them", socketPath)
	case err == nil:
		add(DoctorWarn, "app-server", "%s exists but is not a Unix socket - not a usable app-server control endpoint", socketPath)
	case wakeWired:
		add(DoctorWarn, "app-server", "wake entries are wired, but no app-server control socket at %s right now - native wake reports unavailable until a session launched by a shared app-server daemon (Codex Desktop, codex app-server) runs at this CODEX_HOME; direct CLI launches never create it", socketPath)
	default:
		add(DoctorPass, "app-server", "no app-server control socket at %s - no shared app-server daemon at this CODEX_HOME right now (direct CLI launches never create it, and Codex Desktop may use a different CODEX_HOME); native wake is not wired", socketPath)
	}
}
