package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/hypervisor-io/punk-records/internal/hookcli"
)

// P1-D CLI tests: the allowlist half of "punk connect hermes" and the
// "punk doctor <client>" command. Nothing here touches the operator's real
// ~/.hermes, ~/.punk or a live server: HOME/HERMES_HOME/credentials are
// disposable, and every server is an httptest fake on a loopback port
// (dead URLs use a reserved, never-listening port, the wakeHomeFixture
// convention).

func doctorHomeFixture(t *testing.T) (home, hermesHome string) {
	t.Helper()
	home = t.TempDir()
	hermesHome = t.TempDir()
	work := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("HERMES_HOME", hermesHome)
	t.Setenv("PUNK_CREDENTIALS", filepath.Join(t.TempDir(), "credentials.json"))
	t.Setenv("PUNK_API_KEY", "")
	t.Setenv("PUNK_URL", "http://127.0.0.1:19797")
	t.Setenv("PUNK_NAMESPACE", "")
	t.Chdir(work)
	return home, hermesHome
}

// TestConnectHermesWritesAllowlist pins the connect-side wiring: after a
// plain "punk connect hermes", the shell-hook allowlist exists under
// $HERMES_HOME and approves EXACTLY the (event, command) pairs wired into
// config.yaml - Hermes matches by exact string equality, so any drift
// between the two files means the approvals never match.
func TestConnectHermesWritesAllowlist(t *testing.T) {
	home, hermesHome := doctorHomeFixture(t)
	stdout, err := captureStdout(t, func() error {
		return run([]string{"connect", "hermes", "--no-mcp", "--no-skill"})
	})
	if err != nil {
		t.Fatalf("connect hermes: %v\n%s", err, stdout)
	}

	alPath := filepath.Join(hermesHome, "shell-hooks-allowlist.json")
	raw, err := os.ReadFile(alPath)
	if err != nil {
		t.Fatalf("allowlist was not written at %s: %v\n%s", alPath, err, stdout)
	}
	var al struct {
		Approvals []struct {
			Event                 string  `json:"event"`
			Command               string  `json:"command"`
			ApprovedAt            string  `json:"approved_at"`
			ScriptMtimeAtApproval *string `json:"script_mtime_at_approval"`
		} `json:"approvals"`
	}
	if err := json.Unmarshal(raw, &al); err != nil {
		t.Fatalf("allowlist is not valid JSON: %v\n%s", err, raw)
	}
	if len(al.Approvals) != 4 {
		t.Fatalf("%d approvals, want 4 (one per wired event): %s", len(al.Approvals), raw)
	}

	// The wired config.yaml entries, per event.
	cfgRaw, err := os.ReadFile(filepath.Join(home, ".hermes", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Hooks map[string][]struct {
			Command string `yaml:"command"`
		} `yaml:"hooks"`
	}
	if err := yaml.Unmarshal(cfgRaw, &cfg); err != nil {
		t.Fatalf("config.yaml is not valid YAML: %v\n%s", err, cfgRaw)
	}
	want := map[string]string{}
	for ev, entries := range cfg.Hooks {
		for _, e := range entries {
			if strings.Contains(e.Command, " hook --from hermes ") {
				want[ev] = e.Command
			}
		}
	}
	if len(want) != 4 {
		t.Fatalf("wired events = %v, want 4", want)
	}
	got := map[string]string{}
	for _, a := range al.Approvals {
		got[a.Event] = a.Command
		if _, err := time.Parse(time.RFC3339, a.ApprovedAt); err != nil {
			t.Fatalf("approved_at %q is not ISO-8601: %v", a.ApprovedAt, err)
		}
		// The invoked binary is the running test binary, which exists, so
		// its mtime must be recorded rather than null.
		if a.ScriptMtimeAtApproval == nil {
			t.Fatalf("script_mtime_at_approval = null for event %q, want the binary's mtime", a.Event)
		}
	}
	for ev, cmd := range want {
		if got[ev] != cmd {
			t.Fatalf("approval for %s = %q, but config.yaml wires %q - Hermes matches these by exact equality", ev, got[ev], cmd)
		}
	}

	if !strings.Contains(stdout, "pre-approved punk's hook entries in "+alPath) {
		t.Fatalf("allowlist success line missing:\n%s", stdout)
	}
	// The coverage note is the P4-D spike finding: shell hooks run in the
	// CLI and Gateway only, and the output must never promise Desktop.
	if !strings.Contains(stdout, "Hermes CLI and Gateway") || !strings.Contains(stdout, "Desktop") {
		t.Fatalf("CLI/Gateway coverage note missing:\n%s", stdout)
	}
	if strings.Contains(stdout, "hooks_auto_accept") {
		t.Fatalf("hooks_auto_accept guidance must not appear when the allowlist merge succeeded:\n%s", stdout)
	}
}

// TestConnectHermesAllowlistFailurePrintsGuidance pins the fallback half:
// when the allowlist cannot be written, connect still succeeds (the
// config.yaml half is done) and prints the exact hooks_auto_accept /
// --accept-hooks / HERMES_ACCEPT_HOOKS=1 guidance - and never sets
// hooks_auto_accept itself.
func TestConnectHermesAllowlistFailurePrintsGuidance(t *testing.T) {
	home, _ := doctorHomeFixture(t)
	notADir := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERMES_HOME", notADir) // allowlist dir cannot be created

	stdout, err := captureStdout(t, func() error {
		return run([]string{"connect", "hermes", "--no-mcp", "--no-skill"})
	})
	if err != nil {
		t.Fatalf("connect must succeed even when the allowlist cannot be written: %v\n%s", err, stdout)
	}
	for _, want := range []string{
		"could not update the Hermes shell-hook allowlist",
		"hooks_auto_accept: true",
		"--accept-hooks",
		"HERMES_ACCEPT_HOOKS=1",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("guidance %q missing:\n%s", want, stdout)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".hermes", "config.yaml")); err != nil {
		t.Fatalf("config.yaml must still be written: %v", err)
	}
	// punk never flips the user's consent default itself.
	cfgRaw, err := os.ReadFile(filepath.Join(home, ".hermes", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cfgRaw), "hooks_auto_accept") {
		t.Fatalf("punk must never set hooks_auto_accept:\n%s", cfgRaw)
	}
}

func TestCmdDoctorUnknownClient(t *testing.T) {
	doctorHomeFixture(t)
	if err := run([]string{"doctor", "bogus"}); err == nil || !strings.Contains(err.Error(), "hermes") {
		t.Fatalf("err = %v, want an error naming the supported clients", err)
	}
	if err := run([]string{"doctor"}); err == nil {
		t.Fatal("doctor without a client must fail")
	}
}

// TestCmdDoctorHermesAgainstFakeServer is the CLI round trip: a wired
// install against a healthy fake server passes (exit 0) with one line per
// check, and the same install pointed at a dead server fails the
// reachability check and exits non-zero.
func TestCmdDoctorHermesAgainstFakeServer(t *testing.T) {
	home, hermesHome := doctorHomeFixture(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/v1/agent/namespace", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"namespace":"agent-demo"}`))
	})
	mux.HandleFunc("/v1/namespaces/agent-demo/messages/diagnostics", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"diagnostics":[]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	configPath := filepath.Join(home, ".hermes", "config.yaml")
	if _, err := hookcli.ConnectHermes(configPath, "/opt/punk", srv.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := hookcli.ConnectHermesAllowlist(filepath.Join(hermesHome, "shell-hooks-allowlist.json"), "/opt/punk", srv.URL, false); err != nil {
		t.Fatal(err)
	}

	stdout, err := captureStdout(t, func() error {
		return run([]string{"doctor", "hermes", "--url", srv.URL})
	})
	if err != nil {
		t.Fatalf("doctor must pass against the fake server: %v\n%s", err, stdout)
	}
	for _, want := range []string{
		"punk doctor: hermes",
		"PASS config:",
		"PASS hook-entries:",
		"PASS allowlist:",
		"PASS credentials:",
		"PASS server:",
		"PASS messaging:",
		"WARN hook-diagnostic:",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("line %q missing:\n%s", want, stdout)
		}
	}

	// Same install, dead server: FAIL server and a non-zero exit.
	stdout, err = captureStdout(t, func() error {
		return run([]string{"doctor", "hermes", "--url", "http://127.0.0.1:19798"})
	})
	if err == nil {
		t.Fatalf("doctor must exit non-zero when a check fails:\n%s", stdout)
	}
	if !strings.Contains(stdout, "FAIL server:") {
		t.Fatalf("FAIL server line missing:\n%s", stdout)
	}
}

// TestCmdDoctorCodexAgainstFakeServer is the codex CLI round trip (P4-C):
// "punk doctor codex" needs no cmd-side wiring beyond the hookcli registry,
// so this pins that a wired CODEX_HOME install passes against a healthy
// fake server (exit 0 - hook-trust stays a WARN by design) and that a dead
// server still exits non-zero.
func TestCmdDoctorCodexAgainstFakeServer(t *testing.T) {
	doctorHomeFixture(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/v1/agent/namespace", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"namespace":"agent-demo"}`))
	})
	mux.HandleFunc("/v1/namespaces/agent-demo/messages/diagnostics", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"diagnostics":[]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if _, err := hookcli.ConnectCodexHooks(filepath.Join(codexHome, "hooks.json"), "/opt/punk", srv.URL, ""); err != nil {
		t.Fatal(err)
	}

	stdout, err := captureStdout(t, func() error {
		return run([]string{"doctor", "codex", "--url", srv.URL})
	})
	if err != nil {
		t.Fatalf("doctor codex must pass against the fake server: %v\n%s", err, stdout)
	}
	for _, want := range []string{
		"punk doctor: codex",
		"PASS codex-home:",
		"PASS config:",
		"PASS hook-entries:",
		"WARN hook-trust:",
		"PASS app-server:",
		"PASS credentials:",
		"PASS server:",
		"PASS messaging:",
		"WARN hook-diagnostic:",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("line %q missing:\n%s", want, stdout)
		}
	}
	// The Desktop caveat is user-facing text: it must never promise hook
	// execution in Codex Desktop (openai/codex#35863).
	if !strings.Contains(stdout, "openai/codex#35863") || !strings.Contains(stdout, "Codex Desktop") {
		t.Fatalf("Desktop caveat missing:\n%s", stdout)
	}

	// Dead server: FAIL server and a non-zero exit.
	stdout, err = captureStdout(t, func() error {
		return run([]string{"doctor", "codex", "--url", "http://127.0.0.1:19798"})
	})
	if err == nil {
		t.Fatalf("doctor must exit non-zero when a check fails:\n%s", stdout)
	}
	if !strings.Contains(stdout, "FAIL server:") {
		t.Fatalf("FAIL server line missing:\n%s", stdout)
	}
}
