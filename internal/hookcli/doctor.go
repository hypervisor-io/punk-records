package hookcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// punk doctor <client>: one honest health report for an installed punk
// connection, rendered as one PASS/FAIL/WARN line per check so an
// operator (or an agent) can see exactly which half of the wiring is
// broken. Read-only by design - doctor diagnoses, it never repairs; the
// failing lines name the command that fixes them.
//
// The check set for a client is the P1-D contract:
//
//   - config: the client's config file is present and parseable.
//   - hook-entries: punk's own hook entries are wired in it.
//   - allowlist: (hermes) the shell-hook allowlist approves every wired
//     punk entry, the way Hermes matches - exact (event, command)
//     equality.
//   - credentials: the server URL and API key punk resolves for this
//     client are usable, and the configured agent identity is reported.
//   - server: the resolved server answers GET /healthz.
//   - messaging: when a punk inbox entry is wired (--messaging), the
//     server advertises the messaging tools the model needs to answer
//     delivery; without an inbox entry the check is an honest skip.
//   - hook-diagnostic: the last delivery diagnostic the server recorded
//     for this client (time + status), or why none can be shown.
//
// Every network probe goes to the RESOLVED server URL (flag, PUNK_URL,
// credentials file, default) and nothing else; tests point it at
// httptest fakes.

// Doctor check statuses. All four characters wide, so Render's lines
// align without padding.
const (
	DoctorPass = "PASS"
	DoctorFail = "FAIL"
	DoctorWarn = "WARN"
)

// DoctorCheck is one rendered report line.
type DoctorCheck struct {
	Name    string // stable machine-ish label: "config", "hook-entries", ...
	Status  string // DoctorPass | DoctorFail | DoctorWarn
	Message string
}

// DoctorReport is the whole run: one report per client, checks in a
// fixed order.
type DoctorReport struct {
	Client string
	Checks []DoctorCheck
}

// Failed reports whether any check failed - the caller's non-zero exit
// condition. WARN lines never fail the report: they are honest
// degradations (no diagnostic recorded yet, a consent prompt to expect),
// not broken wiring.
func (r DoctorReport) Failed() bool {
	for _, c := range r.Checks {
		if c.Status == DoctorFail {
			return true
		}
	}
	return false
}

// FailCount counts the FAIL lines, for the caller's error message.
func (r DoctorReport) FailCount() int {
	n := 0
	for _, c := range r.Checks {
		if c.Status == DoctorFail {
			n++
		}
	}
	return n
}

// Render renders the human-readable report: one header line, then one
// line per check with its PASS/FAIL/WARN prefix.
func (r DoctorReport) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "punk doctor: %s\n", r.Client)
	for _, c := range r.Checks {
		fmt.Fprintf(&b, "%s %s: %s\n", c.Status, c.Name, c.Message)
	}
	return b.String()
}

// DoctorOptions configures one Doctor run. The zero value resolves
// everything the way the connect commands do: the client's own default
// config path, and the server from ResolveServer.
type DoctorOptions struct {
	ConfigPath    string // client config file; "" = the client's default (hermes: ~/.hermes/config.yaml; codex: $CODEX_HOME/hooks.json - a set path's DIRECTORY becomes the CODEX_HOME scope inspected for config.toml and the app-server socket)
	AllowlistPath string // hermes only; "" = HermesAllowlistPath()
	URLFlag       string // explicit server URL, same precedence as everywhere else in the CLI
	Dir           string // working directory the namespace is resolved for; "" = os.Getwd()
}

// doctorClients lists the clients Doctor supports, for the refusal
// message. A new client adds its case to Doctor's switch and its name
// here.
var doctorClients = []string{"hermes", "codex"}

// Doctor runs one client's health report. It returns an error only when
// the report itself cannot run (unknown client, unresolvable punk path);
// broken wiring is reported as FAIL checks inside the report, never as
// an error.
func Doctor(ctx context.Context, client string, o DoctorOptions) (DoctorReport, error) {
	switch client {
	case "hermes":
		return doctorHermes(ctx, o)
	case "codex":
		return doctorCodex(ctx, o)
	}
	return DoctorReport{}, fmt.Errorf("unknown doctor client %q, only %s supported", client, strings.Join(quoteList(doctorClients), ", "))
}

// quoteList renders each name double-quoted, for refusal messages.
func quoteList(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = `"` + n + `"`
	}
	return out
}

// doctorMessagingTools are the MCP tools a messaging client needs the
// server to advertise: the four delivery tools (internal/mcpserver only
// registers them when messaging is enabled server-side).
// list_region_members is deliberately not checked: the agent toolset
// gates it separately, and delivery does not depend on it.
var doctorMessagingTools = []string{"send_message", "read_messages", "ack_messages", "await_messages"}

// doctorListTools lists the tool names one MCP endpoint advertises. A
// package var so tests can stub the MCP round trip; the real probe is a
// roots-less verifyMCP session, the same posture VerifyMCP uses.
var doctorListTools = func(ctx context.Context, endpoint, apiKey string) ([]string, error) {
	rep, err := verifyMCP(ctx, endpoint, apiKey, "", "")
	if err != nil {
		return nil, err
	}
	return rep.Tools, nil
}

// doctorDiagnostic is the subset of the server's diagnostic snapshot
// (region.MessageDiagnostic) the report shows.
type doctorDiagnostic struct {
	Agent         string `json:"agent"`
	Client        string `json:"client"`
	State         string `json:"state"`
	LastAttemptAt string `json:"last_attempt_at"`
	UpdatedAt     string `json:"updated_at"`
	Stale         bool   `json:"stale"`
}

func doctorHermes(ctx context.Context, o DoctorOptions) (DoctorReport, error) {
	rep := DoctorReport{Client: "hermes"}
	add := func(status, name, format string, args ...any) {
		rep.Checks = append(rep.Checks, DoctorCheck{Name: name, Status: status, Message: fmt.Sprintf(format, args...)})
	}

	punkPath, err := os.Executable()
	if err != nil {
		return DoctorReport{}, fmt.Errorf("resolve punk executable path: %w", err)
	}

	configPath := o.ConfigPath
	if configPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return DoctorReport{}, fmt.Errorf("resolve home directory: %w", err)
		}
		configPath = filepath.Join(home, ".hermes", "config.yaml")
	}
	allowlistPath := o.AllowlistPath
	if allowlistPath == "" {
		allowlistPath = HermesAllowlistPath()
	}
	dir := o.Dir
	if dir == "" {
		if dir, err = os.Getwd(); err != nil {
			return DoctorReport{}, fmt.Errorf("resolve working directory: %w", err)
		}
	}
	serverURL, apiKey := ResolveServer(o.URLFlag)

	// config: present and parseable. loadHermesConfig treats a missing
	// file as an empty document, so existence is checked first - doctor
	// must report "not installed", not "installed and empty".
	var root *yaml.Node
	if _, statErr := os.Stat(configPath); statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			add(DoctorFail, "config", "no Hermes config at %s - run punk connect hermes first", configPath)
		} else {
			add(DoctorFail, "config", "cannot read %s: %v", configPath, statErr)
		}
	} else if doc, _, cfgErr := loadHermesConfig(configPath); cfgErr != nil {
		add(DoctorFail, "config", "%s is not usable: %v", configPath, cfgErr)
	} else {
		root = doc.Content[0]
		add(DoctorPass, "config", "%s present and parseable", configPath)
	}

	// hook-entries: punk's capture entries on every wired event, detected
	// with the same managed-entry rules a reconnect dedups by (the
	// fallback rules tolerate a relocated punk binary).
	var wiring doctorHermesWiring
	if root == nil {
		add(DoctorFail, "hook-entries", "cannot check hook entries: config missing or unparseable")
	} else {
		wiring = doctorReadHermesWiring(root, punkPath)
		var missing []string
		for _, ev := range hermesHookEvents {
			if len(wiring.capture[ev]) == 0 {
				missing = append(missing, ev)
			}
		}
		switch {
		case len(missing) == 0:
			add(DoctorPass, "hook-entries", "punk capture entries wired on all %d events (%s)",
				len(hermesHookEvents), strings.Join(hermesHookEvents, ", "))
		case len(missing) == len(hermesHookEvents):
			add(DoctorFail, "hook-entries", "no punk hook entries in %s - run punk connect hermes", configPath)
		default:
			add(DoctorFail, "hook-entries", "punk capture entries missing on %s - rerun punk connect hermes",
				strings.Join(missing, ", "))
		}
	}

	// allowlist: every wired punk entry approved the way Hermes matches -
	// exact (event, command) equality against the commands ACTUALLY wired
	// (not freshly built ones, so a relocated-binary install is checked
	// as installed). Missing approvals are a WARN, never a FAIL: the
	// hooks still run, Hermes just prompts for consent first.
	doctorHermesAllowlistCheck(add, configPath, allowlistPath, root, wiring)

	// credentials: the server URL and API key punk resolves, where they
	// came from, and the agent identity the connection pins.
	urlOK := doctorCredentialsCheck(add, o.URLFlag, serverURL, apiKey, doctorHermesAgent(root), "mcp_servers.punk X-Punk-Agent")

	// server: a cheap reachability probe against the resolved URL only.
	doctorServerCheck(ctx, add, serverURL, apiKey, urlOK)

	// messaging: the inbox entry's server-side half. Only checked when
	// --messaging is actually wired; a roots-less MCP session proves the
	// delivery tools are advertised.
	switch {
	case root == nil:
		add(DoctorWarn, "messaging", "cannot check messaging: config missing or unparseable")
	case len(wiring.inbox) == 0:
		add(DoctorPass, "messaging", "not configured (no punk inbox entry in %s); messaging tools check skipped", configPath)
	case !urlOK:
		add(DoctorFail, "messaging", "punk inbox entry is wired on pre_llm_call, but server URL %q is not usable", serverURL)
	default:
		endpoint := mcpEndpoint(serverURL)
		tools, toolsErr := doctorListTools(ctx, endpoint, apiKey)
		switch {
		case toolsErr != nil:
			add(DoctorFail, "messaging", "punk inbox entry is wired on pre_llm_call, but listing tools at %s failed: %v", endpoint, toolsErr)
		default:
			missing := doctorMissingMessagingTools(tools)
			if len(missing) > 0 {
				add(DoctorFail, "messaging", "punk inbox entry is wired on pre_llm_call, but the server does not advertise %s at %s - enable messaging server-side (docs/agent-messaging.md)",
					strings.Join(missing, ", "), endpoint)
			} else {
				add(DoctorPass, "messaging", "punk inbox entry wired on pre_llm_call; server advertises %s at %s",
					strings.Join(doctorMessagingTools, ", "), endpoint)
			}
		}
	}

	// hook-diagnostic: the last delivery observation the server recorded
	// for client hermes. Absence or staleness is a WARN - a snapshot is
	// an observation, never delivery truth, and a fresh install honestly
	// has none yet.
	doctorDiagnosticCheck(ctx, add, serverURL, apiKey, dir, urlOK, "hermes", "")

	return rep, nil
}

// doctorHermesWiring is what doctor reads out of a parsed Hermes config:
// the punk commands actually wired per event (captures) and the punk
// inbox commands on pre_llm_call.
type doctorHermesWiring struct {
	capture map[string][]string // wired event -> punk capture commands on it
	inbox   []string            // punk inbox commands on pre_llm_call
}

// doctorReadHermesWiring collects the punk-managed commands from a
// parsed config, read-only: unlike connect's hermesChildSequence it
// never creates missing nodes, it only reports what is installed.
func doctorReadHermesWiring(root *yaml.Node, punkPath string) doctorHermesWiring {
	w := doctorHermesWiring{capture: map[string][]string{}}
	hooks := yamlMapValue(root, "hooks")
	if hooks == nil || hooks.Kind != yaml.MappingNode {
		return w
	}
	for _, ev := range hermesHookEvents {
		seq := yamlMapValue(hooks, ev)
		if seq == nil || seq.Kind != yaml.SequenceNode {
			continue
		}
		for _, e := range seq.Content {
			cmd := doctorHermesEntryCommand(e)
			if cmd == "" {
				continue
			}
			switch {
			case isPunkManagedInbox(cmd, punkPath, "hermes"):
				w.inbox = append(w.inbox, cmd)
			case isPunkManagedHermes(cmd, punkPath):
				w.capture[ev] = append(w.capture[ev], cmd)
			}
		}
	}
	return w
}

// doctorHermesEntryCommand returns a hook entry's command string, or ""
// for any element that is not a mapping with a scalar command - the same
// tolerance the merge detectors apply to hostile shapes.
func doctorHermesEntryCommand(e *yaml.Node) string {
	if e == nil || e.Kind != yaml.MappingNode {
		return ""
	}
	cmd := yamlMapValue(e, "command")
	if cmd == nil || cmd.Kind != yaml.ScalarNode {
		return ""
	}
	return cmd.Value
}

// doctorHermesAgent reads the agent identity the connection pins: the
// X-Punk-Agent header punk connect writes into mcp_servers.punk. "" when
// no MCP entry (or no header) is installed - hook sessions then
// self-register their address from the native payload, and there is
// nothing pinned to report.
func doctorHermesAgent(root *yaml.Node) string {
	if root == nil {
		return ""
	}
	servers := yamlMapValue(root, "mcp_servers")
	if servers == nil || servers.Kind != yaml.MappingNode {
		return ""
	}
	punk := yamlMapValue(servers, "punk")
	if punk == nil || punk.Kind != yaml.MappingNode {
		return ""
	}
	headers := yamlMapValue(punk, "headers")
	if headers == nil || headers.Kind != yaml.MappingNode {
		return ""
	}
	agent := yamlMapValue(headers, "X-Punk-Agent")
	if agent == nil || agent.Kind != yaml.ScalarNode {
		return ""
	}
	return agent.Value
}

// doctorHermesAllowlistCheck renders the allowlist line: every wired
// punk entry approved in the shell-hook allowlist, matched the way
// Hermes matches (exact (event, command) equality).
func doctorHermesAllowlistCheck(add func(status, name, format string, args ...any), configPath, allowlistPath string, root *yaml.Node, wiring doctorHermesWiring) {
	type approval struct{ event, command string }
	var wants []approval
	if root != nil {
		for _, ev := range hermesHookEvents {
			for _, cmd := range wiring.capture[ev] {
				wants = append(wants, approval{ev, cmd})
			}
		}
		for _, cmd := range wiring.inbox {
			wants = append(wants, approval{"pre_llm_call", cmd})
		}
	}
	guidance := fmt.Sprintf("rerun punk connect hermes, set the top-level key hooks_auto_accept: true in %s, or run hermes with --accept-hooks / HERMES_ACCEPT_HOOKS=1", configPath)
	switch {
	case root == nil:
		add(DoctorWarn, "allowlist", "cannot check the shell-hook allowlist: config missing or unparseable")
	case len(wants) == 0:
		add(DoctorWarn, "allowlist", "no punk hook entries are wired, so nothing to approve in %s", allowlistPath)
	default:
		doc, existing, err := loadHermesAllowlist(allowlistPath)
		switch {
		case err != nil:
			add(DoctorWarn, "allowlist", "%s is not usable (%v) - Hermes treats it as empty and prompts for consent; %s", allowlistPath, err, guidance)
		case existing == nil:
			add(DoctorWarn, "allowlist", "no shell-hook allowlist at %s - Hermes will prompt for consent the first time each punk hook runs; %s", allowlistPath, guidance)
		default:
			have := map[approval]bool{}
			for _, raw := range doc.Approvals {
				var a hermesApproval
				if json.Unmarshal(raw, &a) == nil {
					have[approval{a.Event, a.Command}] = true
				}
			}
			var missing []string
			for _, w := range wants {
				if !have[w] {
					missing = append(missing, w.event)
				}
			}
			if len(missing) == 0 {
				add(DoctorPass, "allowlist", "all %d punk hook approvals present in %s", len(wants), allowlistPath)
			} else {
				add(DoctorWarn, "allowlist", "%d of %d punk hook approvals missing in %s (%s) - Hermes will prompt for consent on those hooks; %s",
					len(missing), len(wants), allowlistPath, strings.Join(missing, ", "), guidance)
			}
		}
	}
}

// doctorServerSource names where ResolveServer's URL came from, in the
// same precedence order ResolveServer applies.
func doctorServerSource(flagURL string) string {
	switch {
	case strings.TrimRight(flagURL, "/") != "":
		return "--url flag"
	case os.Getenv("PUNK_URL") != "":
		return "PUNK_URL"
	default:
		if creds, found, err := LoadCredentials(CredentialsPath()); err == nil && found && creds.URL != "" {
			return "credentials file " + CredentialsPath()
		}
		return "built-in default"
	}
}

// doctorCredentialsCheck renders the credentials line shared by every
// client: the resolved server URL and where it came from, whether an API
// key resolved, and the agent identity the connection pins (agent ""
// means unpinned - hook sessions then self-register their address from
// the native payload, and there is nothing pinned to report). It returns
// whether the resolved URL is usable, the gate the server, messaging and
// diagnostic checks apply before probing.
func doctorCredentialsCheck(add func(status, name, format string, args ...any), urlFlag, serverURL, apiKey, agent, agentSource string) bool {
	credsPath := CredentialsPath()
	if _, _, credErr := LoadCredentials(credsPath); credErr != nil {
		add(DoctorFail, "credentials", "credentials file %s is unreadable: %v", credsPath, credErr)
		return false
	}
	if u, urlErr := url.Parse(serverURL); urlErr != nil || u.Scheme == "" || u.Host == "" {
		add(DoctorFail, "credentials", "server URL %q (from %s) is not a valid URL", serverURL, doctorServerSource(urlFlag))
		return false
	}
	key := "no API key"
	if apiKey != "" {
		key = "API key resolved"
	}
	who := "agent identity not pinned (hook sessions self-register their address)"
	if agent != "" {
		who = fmt.Sprintf("agent %s (%s)", agent, agentSource)
	}
	add(DoctorPass, "credentials", "server %s resolved from %s, %s, %s", serverURL, doctorServerSource(urlFlag), key, who)
	return true
}

// doctorServerCheck renders the server line shared by every client: a
// cheap reachability probe against the resolved URL only.
func doctorServerCheck(ctx context.Context, add func(status, name, format string, args ...any), serverURL, apiKey string, urlOK bool) {
	switch {
	case !urlOK:
		add(DoctorFail, "server", "check skipped: server URL %q is not usable", serverURL)
	default:
		status, probeErr := doctorProbeHealthz(ctx, serverURL, apiKey)
		switch {
		case probeErr != nil:
			add(DoctorFail, "server", "%s unreachable: %v", serverURL, probeErr)
		case status != http.StatusOK:
			add(DoctorFail, "server", "%s answered GET /healthz with status %d", serverURL, status)
		default:
			add(DoctorPass, "server", "%s reachable (GET /healthz -> 200)", serverURL)
		}
	}
}

// doctorMissingMessagingTools returns the delivery tools the advertised
// set does not cover, in doctorMessagingTools order.
func doctorMissingMessagingTools(tools []string) []string {
	advertised := make(map[string]bool, len(tools))
	for _, t := range tools {
		advertised[t] = true
	}
	var missing []string
	for _, t := range doctorMessagingTools {
		if !advertised[t] {
			missing = append(missing, t)
		}
	}
	return missing
}

// doctorProbeHealthz answers the reachability check: the HTTP status of
// GET <server>/healthz, or an error when the server cannot be reached at
// all. Bounded by httpClient's 2s timeout, so a hung server adds at most
// that much to the report.
func doctorProbeHealthz(ctx context.Context, serverURL, apiKey string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, serverURL+"/healthz", nil)
	if err != nil {
		return 0, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxRespBytes))
	return resp.StatusCode, nil
}

// doctorDiagnosticCheck renders the hook-diagnostic line for one client:
// the last delivery snapshot the server recorded for it. The namespace
// resolves the way the hooks' own delivery does: the hook command's --ns
// pin when it carries one (punk connect --project), else PUNK_NAMESPACE
// when set, else the server's own lookup for the working directory (the
// same /v1/agent/namespace answer the server derives a hook payload's cwd
// to).
func doctorDiagnosticCheck(ctx context.Context, add func(status, name, format string, args ...any), serverURL, apiKey, dir string, urlOK bool, client, pinnedNS string) {
	if !urlOK {
		add(DoctorWarn, "hook-diagnostic", "skipped: server URL %q is not usable", serverURL)
		return
	}
	var ns, nsSource string
	switch {
	case pinnedNS != "":
		ns, nsSource = pinnedNS, "the --ns hook flag"
	case strings.TrimSpace(os.Getenv("PUNK_NAMESPACE")) != "":
		ns, nsSource = strings.TrimSpace(os.Getenv("PUNK_NAMESPACE")), "PUNK_NAMESPACE"
	default:
		resolved, err := VerifyHTTP(ctx, serverURL, apiKey, dir)
		if err != nil {
			add(DoctorWarn, "hook-diagnostic", "namespace lookup for %s failed (%v) - cannot locate hook diagnostics", dir, err)
			return
		}
		ns, nsSource = resolved, "server lookup for "+dir
	}
	diags, status, err := doctorFetchDiagnostics(ctx, serverURL, apiKey, ns)
	switch {
	case err != nil:
		add(DoctorWarn, "hook-diagnostic", "diagnostics lookup in namespace %s failed: %v", ns, err)
	case status != http.StatusOK:
		add(DoctorWarn, "hook-diagnostic", "server answered GET /v1/namespaces/%s/messages/diagnostics with status %d", ns, status)
	default:
		// The latest snapshot for this client: UpdatedAt uses the store's
		// fixed-width UTC encoding, so it orders lexicographically.
		var best *doctorDiagnostic
		for i := range diags {
			d := &diags[i]
			if d.Client != client {
				continue
			}
			if best == nil || d.UpdatedAt > best.UpdatedAt {
				best = d
			}
		}
		switch {
		case best == nil:
			add(DoctorWarn, "hook-diagnostic", "no diagnostic recorded for client %s in namespace %s (%s) yet - hooks report one per inbox run (punk connect %s --messaging)", client, ns, nsSource, client)
		case best.Stale:
			add(DoctorWarn, "hook-diagnostic", "last report is stale: state %s at %s (agent %s, namespace %s, updated %s)",
				best.State, doctorOrDash(best.LastAttemptAt), best.Agent, ns, best.UpdatedAt)
		default:
			add(DoctorPass, "hook-diagnostic", "state %s at %s (agent %s, namespace %s via %s, updated %s)",
				best.State, doctorOrDash(best.LastAttemptAt), best.Agent, ns, nsSource, best.UpdatedAt)
		}
	}
}

// doctorOrDash renders an optional timestamp for the report.
func doctorOrDash(ts string) string {
	if ts == "" {
		return "-"
	}
	return ts
}

// doctorFetchDiagnostics reads the server's diagnostic snapshots for one
// namespace: GET /v1/namespaces/<ns>/messages/diagnostics. It returns
// the decoded list on a 200, the HTTP status on any answered response
// (so a 404 from a pre-diagnostics server is distinguishable from a
// transport failure), and an error only for transport/decode failures.
func doctorFetchDiagnostics(ctx context.Context, serverURL, apiKey, ns string) ([]doctorDiagnostic, int, error) {
	endpoint := fmt.Sprintf("%s/v1/namespaces/%s/messages/diagnostics", serverURL, url.PathEscape(ns))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxRespBytes))
		return nil, resp.StatusCode, nil
	}
	var out struct {
		Diagnostics []doctorDiagnostic `json:"diagnostics"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxRespBytes)).Decode(&out); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("decode response: %w", err)
	}
	return out.Diagnostics, resp.StatusCode, nil
}
