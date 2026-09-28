package api

import (
	"os"
	"strings"
	"testing"
)

// nodeHarnessEnv is the environment for the Node round-trip harnesses:
// the test process's environment minus every PUNK_* variable, with
// XDG_STATE_HOME pointed at a per-test temp dir. The harnesses exercise
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
		if strings.HasPrefix(name, "PUNK_") || name == "XDG_STATE_HOME" {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "XDG_STATE_HOME="+t.TempDir())
	return append(env, extra...)
}

func TestNodeHarnessEnvIsHermetic(t *testing.T) {
	t.Setenv("PUNK_MESSAGING", "1")
	t.Setenv("PUNK_URL", "http://127.0.0.1:9")
	t.Setenv("PUNK_API_KEY", "secret")
	t.Setenv("XDG_STATE_HOME", "/home/someone/.local/state")
	t.Setenv("PATH", os.Getenv("PATH"))
	env := nodeHarnessEnv(t, "PUNK_NAMESPACE=explicit")
	var state, path int
	for _, kv := range env {
		name, val, _ := strings.Cut(kv, "=")
		switch {
		case name == "PUNK_NAMESPACE":
			if val != "explicit" {
				t.Errorf("explicit extra lost: %q", kv)
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
	if state != 1 || path != 1 {
		t.Fatalf("XDG_STATE_HOME x%d PATH x%d, want one each", state, path)
	}
}
