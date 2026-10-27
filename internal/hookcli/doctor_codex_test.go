package hookcli

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Codex doctor tests (P4-C). Same hermetic discipline as the hermes set:
// fake CODEX_HOME temp dirs (never the real ~/.codex), httptest servers
// (never the real :9090), the MCP tools/list round trip stubbed through
// doctorListTools, and every environment input doctor resolves isolated
// so the developer's machine can never leak into an assertion.

// doctorCodexEnvFixture is doctorEnvFixture plus CODEX_HOME isolation.
func doctorCodexEnvFixture(t *testing.T) string {
	t.Helper()
	doctorEnvFixture(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	return codexHome
}

// doctorCodexInstall wires a codex home the way punk connect codex
// [--messaging] does: capture (and inbox) groups in hooks.json plus the
// punk MCP block and the [features] hooks flag in config.toml.
func doctorCodexInstall(t *testing.T, home, punkPath, serverURL string, messaging bool) {
	t.Helper()
	hooksPath := filepath.Join(home, "hooks.json")
	var err error
	if messaging {
		_, err = ConnectCodexHooksMessaging(hooksPath, punkPath, serverURL, "")
	} else {
		_, err = ConnectCodexHooks(hooksPath, punkPath, serverURL, "")
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectCodexConfig(filepath.Join(home, "config.toml"),
		MCPEntryOpts{ServerURL: serverURL, Agent: "codex-agent@example"}, true, false); err != nil {
		t.Fatal(err)
	}
}

// doctorCodexGolden is the byte-exact report for the healthy CLI scenario
// in TestDoctorCodexGolden, with run-specific values replaced by
// placeholders ($SERVER, $CODEX_HOME, $DIR). The steady state of a healthy
// CLI install carries exactly one WARN: hook-trust, because Codex grants
// hook trust interactively (no disk state punk can read) and trusted
// SessionStart command hooks never execute in Codex Desktop
// (openai/codex#35863) - doctor must never promise Desktop hook execution.
const doctorCodexGolden = `punk doctor: codex
PASS codex-home: $CODEX_HOME resolved from the CODEX_HOME environment variable, directory present
PASS config: $CODEX_HOME/hooks.json present and parseable
PASS hook-entries: punk capture groups wired on all 4 events (SessionStart, UserPromptSubmit, PostToolUse, Stop)
WARN hook-trust: hook trust is granted interactively (Codex "Hooks need review" prompt, once) and is not readable from disk; [features] hooks = true is set in $CODEX_HOME/config.toml; trusted SessionStart command hooks never execute in Codex Desktop (openai/codex#35863) - Desktop delivery uses the scheduled punk-inbox recipe (docs/agent-messaging.md#codex-desktop)
PASS app-server: no app-server control socket at $CODEX_HOME/app-server-control/app-server-control.sock - no shared app-server daemon at this CODEX_HOME right now (direct CLI launches never create it, and Codex Desktop may use a different CODEX_HOME); native wake is not wired
PASS credentials: server $SERVER resolved from --url flag, no API key, agent codex-agent@example (config.toml punk block X-Punk-Agent)
PASS server: $SERVER reachable (GET /healthz -> 200)
PASS messaging: punk inbox groups wired on SessionStart, UserPromptSubmit, Stop; server advertises send_message, read_messages, ack_messages, await_messages at $SERVER/mcp?toolset=agent
PASS hook-diagnostic: state delivered at 2026-10-27T10:29:00Z (agent codex-cli-1, namespace agent-demo via server lookup for $DIR, updated 2026-10-27T10:30:00Z)
`

// TestDoctorCodexGolden is the output golden test: a fully wired
// --messaging CLI install against a healthy fake server renders exactly
// doctorCodexGolden. The diagnostics body holds a newer claude-code entry
// and an older codex entry, so the reported line proves the check filters
// by client and picks the latest codex snapshot.
func TestDoctorCodexGolden(t *testing.T) {
	home := doctorCodexEnvFixture(t)
	diagnostics := `{"diagnostics":[
		{"namespace":"agent-demo","agent":"claude-1","client":"claude-code","delivery_mode":"hook_continuation","state":"delivered","last_attempt_at":"2026-10-27T11:00:00Z","updated_at":"2026-10-27T11:00:00Z","pending_ack_count":0,"wake_count":0,"stale":false},
		{"namespace":"agent-demo","agent":"codex-cli-1","client":"codex","delivery_mode":"hook_continuation","state":"waiting_for_next_prompt","last_attempt_at":"2026-10-27T10:00:00Z","updated_at":"2026-10-27T10:00:00Z","pending_ack_count":0,"wake_count":0,"stale":false},
		{"namespace":"agent-demo","agent":"codex-cli-1","client":"codex","delivery_mode":"hook_continuation","state":"delivered","last_attempt_at":"2026-10-27T10:29:00Z","updated_at":"2026-10-27T10:30:00Z","pending_ack_count":0,"wake_count":1,"stale":false}
	]}`
	srv := doctorFakeServer(t, diagnostics)
	doctorCodexInstall(t, home, "/opt/punk", srv.URL, true)
	dir := t.TempDir()
	defer stubDoctorListTools(t, []string{"whoami", "send_message", "read_messages", "ack_messages", "await_messages"}, nil)()

	rep, err := Doctor(context.Background(), "codex", DoctorOptions{URLFlag: srv.URL, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failed() {
		t.Fatalf("golden scenario must have no FAIL:\n%s", rep.Render())
	}
	got := rep.Render()
	for _, sub := range [][2]string{
		{srv.URL, "$SERVER"},
		{home, "$CODEX_HOME"},
		{dir, "$DIR"},
	} {
		got = strings.ReplaceAll(got, sub[0], sub[1])
	}
	if got != doctorCodexGolden {
		t.Fatalf("report does not match golden:\n--- got ---\n%s\n--- want ---\n%s", got, doctorCodexGolden)
	}
}

// TestDoctorCodexMissingHomeFails pins the broken-install report: a
// CODEX_HOME that does not exist fails the home, config and hook-entry
// checks, degrades the config-derived checks, and a dead server URL fails
// the reachability check - so Failed() reports a non-zero-exit condition.
func TestDoctorCodexMissingHomeFails(t *testing.T) {
	doctorEnvFixture(t)
	missing := filepath.Join(t.TempDir(), "no-such-codex-home")
	t.Setenv("CODEX_HOME", missing)
	rep, err := Doctor(context.Background(), "codex", DoctorOptions{
		URLFlag: "http://127.0.0.1:19798", // reserved, never-listening loopback port
		Dir:     t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Failed() {
		t.Fatalf("missing CODEX_HOME must produce failures:\n%s", rep.Render())
	}
	checks := doctorCheckByName(rep)
	want := map[string]string{
		"codex-home":      DoctorFail,
		"config":          DoctorFail,
		"hook-entries":    DoctorFail,
		"hook-trust":      DoctorWarn,
		"app-server":      DoctorPass,
		"credentials":     DoctorPass,
		"server":          DoctorFail,
		"messaging":       DoctorWarn,
		"hook-diagnostic": DoctorWarn,
	}
	for name, status := range want {
		c, ok := checks[name]
		if !ok {
			t.Fatalf("check %q missing from report:\n%s", name, rep.Render())
		}
		if c.Status != status {
			t.Fatalf("check %q = %s, want %s:\n%s", name, c.Status, status, rep.Render())
		}
	}
	if !strings.Contains(checks["codex-home"].Message, "CODEX_HOME") ||
		!strings.Contains(checks["codex-home"].Message, "punk connect codex") {
		t.Fatalf("codex-home failure must name CODEX_HOME and the remedy: %q", checks["codex-home"].Message)
	}
}

// TestDoctorCodexFeaturesHooksFalseFails pins the one hook-trust state
// punk CAN read from disk: [features] hooks = false in config.toml
// disables every hook, so it fails the report and names the hand edit
// (punk never changes that key itself - ConnectCodexConfig refuses too).
func TestDoctorCodexFeaturesHooksFalseFails(t *testing.T) {
	home := doctorCodexEnvFixture(t)
	doctorCodexInstall(t, home, "/opt/punk", "http://127.0.0.1:19797", false)
	// The user flips the feature flag off by hand above the punk block.
	body, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"),
		append([]byte("[features]\nhooks = false\n\n"), body...), 0o600); err != nil {
		t.Fatal(err)
	}

	rep, err := Doctor(context.Background(), "codex", DoctorOptions{
		URLFlag: "http://127.0.0.1:19797",
		Dir:     t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	checks := doctorCheckByName(rep)
	if checks["hook-trust"].Status != DoctorFail {
		t.Fatalf("hook-trust = %s %q, want FAIL", checks["hook-trust"].Status, checks["hook-trust"].Message)
	}
	for _, want := range []string{"hooks = false", "[features]"} {
		if !strings.Contains(checks["hook-trust"].Message, want) {
			t.Fatalf("hook-trust failure must mention %q: %q", want, checks["hook-trust"].Message)
		}
	}
	if !rep.Failed() {
		t.Fatal("hooks = false must fail the report")
	}
}

// shortCodexHome returns a CODEX_HOME short enough for a Unix socket path
// under it (the ~108-byte sun_path bound): t.TempDir() names can carry the
// full test name and overflow once /app-server-control/
// app-server-control.sock is appended.
func shortCodexHome(t *testing.T) string {
	t.Helper()
	base := filepath.Join(os.TempDir(), "p4c")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(base, "codex")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// TestDoctorCodexAppServerSocketPresent pins the Desktop-shape branch: a
// live Unix socket at $CODEX_HOME/app-server-control/app-server-control.sock
// (the path wake.go's codexControlSocket resolves) reports the shared
// app-server daemon - the launch mode Codex Desktop uses and the native
// wake target.
func TestDoctorCodexAppServerSocketPresent(t *testing.T) {
	doctorEnvFixture(t)
	home := shortCodexHome(t)
	t.Setenv("CODEX_HOME", home)
	sockDir := filepath.Join(home, "app-server-control")
	if err := os.MkdirAll(sockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(sockDir, "app-server-control.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	doctorCodexInstall(t, home, "/opt/punk", "http://127.0.0.1:19797", false)
	rep, err := Doctor(context.Background(), "codex", DoctorOptions{
		URLFlag: "http://127.0.0.1:19797",
		Dir:     t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	checks := doctorCheckByName(rep)
	if checks["app-server"].Status != DoctorPass {
		t.Fatalf("app-server = %s %q, want PASS", checks["app-server"].Status, checks["app-server"].Message)
	}
	if !strings.Contains(checks["app-server"].Message, "control socket present") {
		t.Fatalf("app-server message = %q, want the socket-present report", checks["app-server"].Message)
	}
}

// TestDoctorCodexAppServerSocketWrongType pins that a regular file at the
// socket path is not mistaken for a daemon endpoint.
func TestDoctorCodexAppServerSocketWrongType(t *testing.T) {
	doctorEnvFixture(t)
	home := shortCodexHome(t)
	t.Setenv("CODEX_HOME", home)
	sockDir := filepath.Join(home, "app-server-control")
	if err := os.MkdirAll(sockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sockDir, "app-server-control.sock"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	rep, err := Doctor(context.Background(), "codex", DoctorOptions{
		URLFlag: "http://127.0.0.1:19797",
		Dir:     t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	checks := doctorCheckByName(rep)
	if checks["app-server"].Status != DoctorWarn ||
		!strings.Contains(checks["app-server"].Message, "not a Unix socket") {
		t.Fatalf("app-server = %s %q, want a WARN about a non-socket path", checks["app-server"].Status, checks["app-server"].Message)
	}
}

// TestDoctorCodexWakeWiredSocketAbsentWarns pins the wake-half failure:
// --wake entries are installed, but no shared-daemon session has run at
// this CODEX_HOME, so native wake reports unavailable until one does.
func TestDoctorCodexWakeWiredSocketAbsentWarns(t *testing.T) {
	home := doctorCodexEnvFixture(t)
	hooksPath := filepath.Join(home, "hooks.json")
	if _, err := ConnectCodexHooksWake(hooksPath, "/opt/punk", "http://127.0.0.1:19797", ""); err != nil {
		t.Fatal(err)
	}

	rep, err := Doctor(context.Background(), "codex", DoctorOptions{
		URLFlag: "http://127.0.0.1:19797",
		Dir:     t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	checks := doctorCheckByName(rep)
	if checks["app-server"].Status != DoctorWarn ||
		!strings.Contains(checks["app-server"].Message, "wake entries are wired") {
		t.Fatalf("app-server = %s %q, want a WARN naming the wired wake entries", checks["app-server"].Status, checks["app-server"].Message)
	}
}

// TestDoctorCodexMessagingToolsMissingFails pins the delivery-side
// failure: inbox groups are wired, but the server does not advertise the
// messaging tools - a FAIL naming every missing tool.
func TestDoctorCodexMessagingToolsMissingFails(t *testing.T) {
	home := doctorCodexEnvFixture(t)
	srv := doctorFakeServer(t, `{"diagnostics":[]}`)
	doctorCodexInstall(t, home, "/opt/punk", srv.URL, true)
	defer stubDoctorListTools(t, []string{"whoami", "recall"}, nil)()

	rep, err := Doctor(context.Background(), "codex", DoctorOptions{URLFlag: srv.URL, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	checks := doctorCheckByName(rep)
	if checks["messaging"].Status != DoctorFail {
		t.Fatalf("messaging = %s %q, want FAIL", checks["messaging"].Status, checks["messaging"].Message)
	}
	for _, tool := range []string{"send_message", "read_messages", "ack_messages", "await_messages"} {
		if !strings.Contains(checks["messaging"].Message, tool) {
			t.Fatalf("messaging failure must name %q: %q", tool, checks["messaging"].Message)
		}
	}
	if !rep.Failed() {
		t.Fatal("a missing messaging tool must fail the report")
	}
}

// TestDoctorCodexMessagingNotConfiguredSkipsToolsCheck pins the plain
// install: no inbox groups, no MCP round trip, an honest PASS skip.
func TestDoctorCodexMessagingNotConfiguredSkipsToolsCheck(t *testing.T) {
	home := doctorCodexEnvFixture(t)
	srv := doctorFakeServer(t, `{"diagnostics":[]}`)
	doctorCodexInstall(t, home, "/opt/punk", srv.URL, false)
	old := doctorListTools
	doctorListTools = func(_ context.Context, _, _ string) ([]string, error) {
		t.Error("tools/list must not run when messaging is not configured")
		return nil, nil
	}
	defer func() { doctorListTools = old }()

	rep, err := Doctor(context.Background(), "codex", DoctorOptions{URLFlag: srv.URL, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failed() {
		t.Fatalf("healthy non-messaging install must have no FAIL:\n%s", rep.Render())
	}
	checks := doctorCheckByName(rep)
	if checks["messaging"].Status != DoctorPass || !strings.Contains(checks["messaging"].Message, "skipped") {
		t.Fatalf("messaging = %s %q, want a PASS skip", checks["messaging"].Status, checks["messaging"].Message)
	}
	// No codex snapshot recorded yet: an honest WARN, not a failure.
	if checks["hook-diagnostic"].Status != DoctorWarn || !strings.Contains(checks["hook-diagnostic"].Message, "no diagnostic") {
		t.Fatalf("hook-diagnostic = %s %q, want a WARN about no recorded diagnostic", checks["hook-diagnostic"].Status, checks["hook-diagnostic"].Message)
	}
}

// TestDoctorCodexPinnedNamespace pins the --project shape: the capture
// command's --ns flag is the namespace the diagnostic check queries, and
// the server's cwd lookup never runs.
func TestDoctorCodexPinnedNamespace(t *testing.T) {
	home := doctorCodexEnvFixture(t)
	diagnostics := `{"diagnostics":[
		{"namespace":"agent-pinned","agent":"codex-cli-1","client":"codex","delivery_mode":"catch_up","state":"delivered","last_attempt_at":"2026-10-27T09:00:00Z","updated_at":"2026-10-27T09:00:00Z","pending_ack_count":0,"wake_count":0,"stale":false}
	]}`
	srv := doctorFakeServerNS(t, "agent-pinned", diagnostics, false)
	hooksPath := filepath.Join(home, "hooks.json")
	if _, err := ConnectCodexHooksMessaging(hooksPath, "/opt/punk", srv.URL, "agent-pinned"); err != nil {
		t.Fatal(err)
	}
	defer stubDoctorListTools(t, []string{"send_message", "read_messages", "ack_messages", "await_messages"}, nil)()

	rep, err := Doctor(context.Background(), "codex", DoctorOptions{URLFlag: srv.URL, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	checks := doctorCheckByName(rep)
	if checks["hook-diagnostic"].Status != DoctorPass {
		t.Fatalf("hook-diagnostic = %s %q, want PASS", checks["hook-diagnostic"].Status, checks["hook-diagnostic"].Message)
	}
	for _, want := range []string{"agent-pinned", "--ns hook flag"} {
		if !strings.Contains(checks["hook-diagnostic"].Message, want) {
			t.Fatalf("hook-diagnostic message must mention %q: %q", want, checks["hook-diagnostic"].Message)
		}
	}
}

// TestDoctorCodexDefaultHome pins the CODEX_HOME-unset resolution: doctor
// inspects ~/.codex exactly the way connect and codexControlSocket do, and
// says so - Codex CLI and Desktop may not share one home.
func TestDoctorCodexDefaultHome(t *testing.T) {
	doctorEnvFixture(t)
	t.Setenv("CODEX_HOME", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	rep, err := Doctor(context.Background(), "codex", DoctorOptions{
		URLFlag: "http://127.0.0.1:19797",
		Dir:     t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	checks := doctorCheckByName(rep)
	// ~/.codex does not exist in the fixture home: an honest FAIL naming
	// the default resolution and its CLI-vs-Desktop caveat.
	if checks["codex-home"].Status != DoctorFail {
		t.Fatalf("codex-home = %s %q, want FAIL for a missing default home", checks["codex-home"].Status, checks["codex-home"].Message)
	}
	for _, want := range []string{filepath.Join(home, ".codex"), "punk connect codex"} {
		if !strings.Contains(checks["codex-home"].Message, want) {
			t.Fatalf("codex-home message must mention %q: %q", want, checks["codex-home"].Message)
		}
	}
}
