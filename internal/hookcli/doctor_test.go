package hookcli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// Doctor tests. Everything hermetic: the server side is an httptest fake
// (never the real :9090), the MCP tools/list round trip is stubbed through
// doctorListTools, config and allowlist live in t.TempDir(), and the
// environment is isolated so the developer's real ~/.punk, PUNK_URL or
// PUNK_NAMESPACE can never leak into an assertion.

// doctorEnvFixture isolates every environment input doctor resolves.
func doctorEnvFixture(t *testing.T) {
	t.Helper()
	t.Setenv("PUNK_URL", "")
	t.Setenv("PUNK_API_KEY", "")
	t.Setenv("PUNK_CREDENTIALS", filepath.Join(t.TempDir(), "credentials.json"))
	t.Setenv("PUNK_NAMESPACE", "")
	t.Setenv("HERMES_HOME", t.TempDir())
}

// doctorFakeServer stands in for a punk server: /healthz for the
// reachability check, /v1/agent/namespace for the namespace lookup and one
// fixed diagnostics body for the hook-diagnostic check. Anything else 404s
// loudly so a wrong request path fails the test instead of silently
// passing a check.
func doctorFakeServer(t *testing.T, diagnosticsBody string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/v1/agent/namespace":
			_, _ = w.Write([]byte(`{"namespace":"agent-demo"}`))
		case "/v1/namespaces/agent-demo/messages/diagnostics":
			_, _ = w.Write([]byte(diagnosticsBody))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// stubDoctorListTools replaces the MCP tools/list probe for one test and
// returns the restore function.
func stubDoctorListTools(t *testing.T, tools []string, err error) func() {
	t.Helper()
	old := doctorListTools
	doctorListTools = func(_ context.Context, _, _ string) ([]string, error) { return tools, err }
	return func() { doctorListTools = old }
}

func doctorCheckByName(rep DoctorReport) map[string]DoctorCheck {
	out := map[string]DoctorCheck{}
	for _, c := range rep.Checks {
		out[c.Name] = c
	}
	return out
}

// doctorHermesGolden is the byte-exact report for the healthy scenario in
// TestDoctorHermesGolden, with the run-specific values replaced by
// placeholders ($SERVER, $CONFIG, $ALLOWLIST, $DIR).
const doctorHermesGolden = `punk doctor: hermes
PASS config: $CONFIG present and parseable
PASS hook-entries: punk capture entries wired on all 4 events (on_session_start, pre_llm_call, post_tool_call, post_llm_call)
PASS allowlist: all 5 punk hook approvals present in $ALLOWLIST
PASS credentials: server $SERVER resolved from --url flag, no API key, agent identity not pinned (hook sessions self-register their address)
PASS server: $SERVER reachable (GET /healthz -> 200)
PASS messaging: punk inbox entry wired on pre_llm_call; server advertises send_message, read_messages, ack_messages, await_messages at $SERVER/mcp?toolset=agent
PASS hook-diagnostic: state delivered at 2026-10-27T10:29:00Z (agent hermes-cli-1, namespace agent-demo via server lookup for $DIR, updated 2026-10-27T10:30:00Z)
`

// TestDoctorHermesGolden is the output golden test: a fully wired
// --messaging install against a healthy fake server renders exactly
// doctorHermesGolden. The diagnostics body holds a newer claude-code entry
// and an older hermes entry, so the reported line proves the check filters
// by client and picks the latest hermes snapshot.
func TestDoctorHermesGolden(t *testing.T) {
	doctorEnvFixture(t)
	diagnostics := `{"diagnostics":[
		{"namespace":"agent-demo","agent":"claude-1","client":"claude-code","delivery_mode":"hook_continuation","state":"delivered","last_attempt_at":"2026-10-27T11:00:00Z","updated_at":"2026-10-27T11:00:00Z","pending_ack_count":0,"wake_count":0,"stale":false},
		{"namespace":"agent-demo","agent":"hermes-cli-1","client":"hermes","delivery_mode":"catch_up","state":"waiting_for_next_prompt","last_attempt_at":"2026-10-27T10:00:00Z","updated_at":"2026-10-27T10:00:00Z","pending_ack_count":0,"wake_count":0,"stale":false},
		{"namespace":"agent-demo","agent":"hermes-cli-1","client":"hermes","delivery_mode":"catch_up","state":"delivered","last_attempt_at":"2026-10-27T10:29:00Z","updated_at":"2026-10-27T10:30:00Z","pending_ack_count":0,"wake_count":0,"stale":false}
	]}`
	srv := doctorFakeServer(t, diagnostics)

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	allowlistPath := filepath.Join(dir, hermesAllowlistFile)
	if _, err := ConnectHermesMessaging(configPath, "/opt/punk", srv.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectHermesAllowlist(allowlistPath, "/opt/punk", srv.URL, true); err != nil {
		t.Fatal(err)
	}
	defer stubDoctorListTools(t, []string{"whoami", "send_message", "read_messages", "ack_messages", "await_messages"}, nil)()

	rep, err := Doctor(context.Background(), "hermes", DoctorOptions{
		ConfigPath:    configPath,
		AllowlistPath: allowlistPath,
		URLFlag:       srv.URL,
		Dir:           dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failed() {
		t.Fatalf("golden scenario must have no FAIL:\n%s", rep.Render())
	}
	got := rep.Render()
	// Longest run-specific values first: the paths live under dir.
	for _, sub := range [][2]string{
		{configPath, "$CONFIG"},
		{allowlistPath, "$ALLOWLIST"},
		{srv.URL, "$SERVER"},
		{dir, "$DIR"},
	} {
		got = strings.ReplaceAll(got, sub[0], sub[1])
	}
	if got != doctorHermesGolden {
		t.Fatalf("report does not match golden:\n--- got ---\n%s\n--- want ---\n%s", got, doctorHermesGolden)
	}
}

// TestDoctorHermesMissingConfigFails pins the broken-install report: a
// missing config fails the config and hook-entry checks, degrades the
// config-derived checks to WARN, and a dead server URL fails the
// reachability check - so Failed() reports a non-zero-exit condition.
func TestDoctorHermesMissingConfigFails(t *testing.T) {
	doctorEnvFixture(t)
	dir := t.TempDir()
	rep, err := Doctor(context.Background(), "hermes", DoctorOptions{
		ConfigPath:    filepath.Join(dir, "config.yaml"), // never created
		AllowlistPath: filepath.Join(dir, hermesAllowlistFile),
		URLFlag:       "http://127.0.0.1:19798", // reserved, never-listening loopback port
		Dir:           dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Failed() {
		t.Fatalf("missing config must produce failures:\n%s", rep.Render())
	}
	checks := doctorCheckByName(rep)
	want := map[string]string{
		"config":          DoctorFail,
		"hook-entries":    DoctorFail,
		"allowlist":       DoctorWarn,
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
	if !strings.Contains(checks["config"].Message, "punk connect hermes") {
		t.Fatalf("config failure must name the remedy: %q", checks["config"].Message)
	}
}

// TestDoctorHermesMessagingNotConfiguredSkipsToolsCheck pins that a plain
// (non-messaging) install never opens an MCP session and reports the
// messaging check as a skip, and that an allowlist matching the wired
// entries passes.
func TestDoctorHermesMessagingNotConfiguredSkipsToolsCheck(t *testing.T) {
	doctorEnvFixture(t)
	srv := doctorFakeServer(t, `{"diagnostics":[]}`)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	allowlistPath := filepath.Join(dir, hermesAllowlistFile)
	if _, err := ConnectHermes(configPath, "/opt/punk", srv.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectHermesAllowlist(allowlistPath, "/opt/punk", srv.URL, false); err != nil {
		t.Fatal(err)
	}
	old := doctorListTools
	doctorListTools = func(_ context.Context, _, _ string) ([]string, error) {
		t.Error("tools/list must not run when messaging is not configured")
		return nil, nil
	}
	defer func() { doctorListTools = old }()

	rep, err := Doctor(context.Background(), "hermes", DoctorOptions{
		ConfigPath:    configPath,
		AllowlistPath: allowlistPath,
		URLFlag:       srv.URL,
		Dir:           dir,
	})
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
	if checks["allowlist"].Status != DoctorPass {
		t.Fatalf("allowlist = %s %q, want PASS", checks["allowlist"].Status, checks["allowlist"].Message)
	}
	// No hermes snapshot recorded yet: an honest WARN, not a failure.
	if checks["hook-diagnostic"].Status != DoctorWarn || !strings.Contains(checks["hook-diagnostic"].Message, "no diagnostic") {
		t.Fatalf("hook-diagnostic = %s %q, want a WARN about no recorded diagnostic", checks["hook-diagnostic"].Status, checks["hook-diagnostic"].Message)
	}
}

// TestDoctorHermesMessagingToolsMissingFails pins the delivery-side
// failure that matters: the inbox hook is wired, but the server does not
// advertise the messaging tools (messaging disabled server-side), so the
// model can never send or read - a FAIL, naming what is missing.
func TestDoctorHermesMessagingToolsMissingFails(t *testing.T) {
	doctorEnvFixture(t)
	srv := doctorFakeServer(t, `{"diagnostics":[]}`)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	allowlistPath := filepath.Join(dir, hermesAllowlistFile)
	if _, err := ConnectHermesMessaging(configPath, "/opt/punk", srv.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectHermesAllowlist(allowlistPath, "/opt/punk", srv.URL, true); err != nil {
		t.Fatal(err)
	}
	defer stubDoctorListTools(t, []string{"whoami", "recall"}, nil)()

	rep, err := Doctor(context.Background(), "hermes", DoctorOptions{
		ConfigPath:    configPath,
		AllowlistPath: allowlistPath,
		URLFlag:       srv.URL,
		Dir:           dir,
	})
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

// TestDoctorHermesMessagingToolsProbeErrorFails pins that a tools/list
// round trip that cannot even complete is a FAIL, not a silent skip: the
// inbox entry is wired, so messaging was asked for and cannot be proven.
func TestDoctorHermesMessagingToolsProbeErrorFails(t *testing.T) {
	doctorEnvFixture(t)
	srv := doctorFakeServer(t, `{"diagnostics":[]}`)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if _, err := ConnectHermesMessaging(configPath, "/opt/punk", srv.URL); err != nil {
		t.Fatal(err)
	}
	defer stubDoctorListTools(t, nil, context.DeadlineExceeded)()

	rep, err := Doctor(context.Background(), "hermes", DoctorOptions{
		ConfigPath:    configPath,
		AllowlistPath: filepath.Join(dir, hermesAllowlistFile),
		URLFlag:       srv.URL,
		Dir:           dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	checks := doctorCheckByName(rep)
	if checks["messaging"].Status != DoctorFail || !strings.Contains(checks["messaging"].Message, "listing tools") {
		t.Fatalf("messaging = %s %q, want a FAIL naming the failed tools/list", checks["messaging"].Status, checks["messaging"].Message)
	}
}

// TestDoctorHermesStaleDiagnosticWarns pins the staleness branch: the
// server marks the snapshot stale, so doctor reports it as a WARN with the
// recorded state and timestamps instead of a clean PASS.
func TestDoctorHermesStaleDiagnosticWarns(t *testing.T) {
	doctorEnvFixture(t)
	diagnostics := `{"diagnostics":[
		{"namespace":"agent-demo","agent":"hermes-cli-1","client":"hermes","delivery_mode":"catch_up","state":"waiting_for_next_prompt","last_attempt_at":"2026-10-20T08:00:00Z","updated_at":"2026-10-20T08:00:00Z","pending_ack_count":2,"wake_count":0,"stale":true}
	]}`
	srv := doctorFakeServer(t, diagnostics)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if _, err := ConnectHermesMessaging(configPath, "/opt/punk", srv.URL); err != nil {
		t.Fatal(err)
	}
	defer stubDoctorListTools(t, []string{"send_message", "read_messages", "ack_messages", "await_messages"}, nil)()

	rep, err := Doctor(context.Background(), "hermes", DoctorOptions{
		ConfigPath:    configPath,
		AllowlistPath: filepath.Join(dir, hermesAllowlistFile),
		URLFlag:       srv.URL,
		Dir:           dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	checks := doctorCheckByName(rep)
	if checks["hook-diagnostic"].Status != DoctorWarn {
		t.Fatalf("hook-diagnostic = %s %q, want WARN", checks["hook-diagnostic"].Status, checks["hook-diagnostic"].Message)
	}
	for _, want := range []string{"stale", "2026-10-20T08:00:00Z", "hermes-cli-1"} {
		if !strings.Contains(checks["hook-diagnostic"].Message, want) {
			t.Fatalf("hook-diagnostic message must mention %q: %q", want, checks["hook-diagnostic"].Message)
		}
	}
	if rep.Failed() {
		t.Fatalf("a stale diagnostic is a WARN, not a FAIL:\n%s", rep.Render())
	}
}

// TestDoctorUnknownClient pins the registry's refusal path.
func TestDoctorUnknownClient(t *testing.T) {
	if _, err := Doctor(context.Background(), "bogus", DoctorOptions{}); err == nil ||
		!strings.Contains(err.Error(), "hermes") {
		t.Fatalf("err = %v, want an error naming the supported clients", err)
	}
}
