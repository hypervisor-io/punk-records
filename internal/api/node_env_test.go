package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// nodeHarnessEnv is the environment for the Node round-trip harnesses:
// the test process's environment minus every PUNK_* variable, with
// HOME, USERPROFILE, PUNK_CREDENTIALS and XDG_STATE_HOME pointed at a per-test
// temp dir. The harnesses exercise
// the generated plugins' memory hooks against an in-process server whose
// URL is baked into the plugin. An inherited PUNK_MESSAGING=1 (common on
// a developer box running messaging) would turn on the messaging bridge
// the fixtures do not wire, and an inherited PUNK_URL/PUNK_API_KEY would
// point the plugin at a real server instead. Each harness opts into
// anything it needs explicitly, so results never depend on the
// developer's shell and never touch live servers or user state.
func nodeHarnessEnv(t *testing.T, extra ...string) []string {
	t.Helper()
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "PUNK_") || name == "XDG_STATE_HOME" || name == "HOME" || name == "USERPROFILE" || name == "NODE_OPTIONS" {
			continue
		}
		env = append(env, kv)
	}
	home := t.TempDir()
	env = append(env, "HOME="+home, "USERPROFILE="+home,
		"PUNK_CREDENTIALS="+filepath.Join(home, "missing-credentials.json"),
		"XDG_STATE_HOME="+filepath.Join(home, "state"))
	return append(env, extra...)
}

func TestNodeHarnessEnvIsHermetic(t *testing.T) {
	t.Setenv("PUNK_MESSAGING", "1")
	t.Setenv("PUNK_URL", "http://127.0.0.1:9")
	t.Setenv("PUNK_API_KEY", "secret")
	t.Setenv("PUNK_CREDENTIALS", "/operator/credentials.json")
	t.Setenv("HOME", "/operator")
	t.Setenv("USERPROFILE", "/operator")
	t.Setenv("XDG_STATE_HOME", "/home/someone/.local/state")
	t.Setenv("PATH", os.Getenv("PATH"))
	env := nodeHarnessEnv(t, "PUNK_NAMESPACE=explicit")
	var state, path, isolated int
	for _, kv := range env {
		name, val, _ := strings.Cut(kv, "=")
		switch {
		case name == "PUNK_NAMESPACE":
			if val != "explicit" {
				t.Errorf("explicit extra lost: %q", kv)
			}
		case name == "HOME" || name == "USERPROFILE" || name == "PUNK_CREDENTIALS":
			isolated++
			if strings.HasPrefix(val, "/operator") {
				t.Errorf("inherited %s escaped isolation", name)
			}
		case strings.HasPrefix(name, "PUNK_"):
			t.Errorf("inherited %s leaked into the harness", name)
		case name == "XDG_STATE_HOME":
			state++
			if val == "/home/someone/.local/state" {
				t.Error("harness would write the user's state home")
			}
		case name == "PATH":
			path++
		}
	}
	if state != 1 || path != 1 || isolated != 3 {
		t.Fatalf("XDG_STATE_HOME x%d PATH x%d isolated credential/home fields x%d, want 1/1/3", state, path, isolated)
	}
}
