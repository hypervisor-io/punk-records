package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// W5 native wake connect tests (2026-09-28 plan): the --wake flag on
// connect claude-code / connect codex, the codex --no-hooks conflict,
// and the generated installed wake commands executed in disposable
// homes. Nothing here touches the operator's real ~/.claude or
// ~/.codex: every test sets HOME/CODEX_HOME into t.TempDir().

// wakeHomeFixture isolates HOME, CODEX_HOME, credentials and the server
// URL into disposable locations, chdir'd into a temp working directory.
// PUNK_URL points at a reserved, never-listening loopback port so a
// misparsed installed command can never reach a live server.
func wakeHomeFixture(t *testing.T) (home, work string) {
	t.Helper()
	home = t.TempDir()
	work = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("PUNK_CREDENTIALS", filepath.Join(t.TempDir(), "credentials.json"))
	t.Setenv("PUNK_API_KEY", "")
	t.Setenv("PUNK_URL", "http://127.0.0.1:19797")
	t.Chdir(work)
	return home, work
}

// wakeCommandsByEvent reads one installed hooks file and returns every
// "punk hook wake" command under each event key.
func wakeCommandsByEvent(t *testing.T, hooksFile string) map[string][]string {
	t.Helper()
	raw, err := os.ReadFile(hooksFile)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	hooks, ok := doc["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("missing hooks in %s", raw)
	}
	out := map[string][]string{}
	for ev, v := range hooks {
		var visit func(any)
		visit = func(x any) {
			switch n := x.(type) {
			case []any:
				for _, e := range n {
					visit(e)
				}
			case map[string]any:
				if cmd, ok := n["command"].(string); ok && strings.Contains(cmd, " hook wake ") {
					out[ev] = append(out[ev], cmd)
				}
				if nested, ok := n["hooks"]; ok {
					visit(nested)
				}
			}
		}
		visit(v)
	}
	return out
}

func TestConnectClaudeCodeWakeFlagInstallsWakeHooks(t *testing.T) {
	home, _ := wakeHomeFixture(t)
	if err := cmdConnectClaudeCode([]string{"--wake", "--no-mcp", "--no-skill", "--url", "http://127.0.0.1:19797"}); err != nil {
		t.Fatal(err)
	}
	cmds := wakeCommandsByEvent(t, filepath.Join(home, ".claude", "settings.json"))
	for _, ev := range []string{"SessionStart", "UserPromptSubmit", "Stop"} {
		if len(cmds[ev]) != 1 || !strings.Contains(cmds[ev][0], "--action ensure") {
			t.Fatalf("%s wake commands %v", ev, cmds[ev])
		}
	}
	if len(cmds["SessionEnd"]) != 1 || !strings.Contains(cmds["SessionEnd"][0], "--action stop") {
		t.Fatalf("SessionEnd wake commands %v", cmds["SessionEnd"])
	}
	for ev, list := range cmds {
		for _, c := range list {
			if !strings.HasSuffix(c, "--messaging") {
				t.Fatalf("%s wake entry must carry the implied opt-in: %q", ev, c)
			}
		}
	}
	// Reconnect is a no-op byte for byte.
	settings := filepath.Join(home, ".claude", "settings.json")
	before, _ := os.ReadFile(settings)
	if err := cmdConnectClaudeCode([]string{"--wake", "--no-mcp", "--no-skill", "--url", "http://127.0.0.1:19797"}); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(settings); !bytes.Equal(before, after) {
		t.Fatal("wake reconnect rewrote settings")
	}
}

func TestConnectCodexWakeFlagInstallsAndNoHooksConflict(t *testing.T) {
	home, _ := wakeHomeFixture(t)
	if err := cmdConnectCodex([]string{"--wake", "--no-hooks", "--no-skill", "--url", "http://127.0.0.1:19797"}); err == nil {
		t.Fatal("--wake --no-hooks must be rejected")
	} else if !strings.Contains(err.Error(), "--wake needs hooks") {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := cmdConnectCodex([]string{"--wake", "--no-mcp", "--no-skill", "--url", "http://127.0.0.1:19797"}); err != nil {
		t.Fatal(err)
	}
	cmds := wakeCommandsByEvent(t, filepath.Join(home, ".codex", "hooks.json"))
	for _, ev := range []string{"SessionStart", "UserPromptSubmit", "Stop"} {
		if len(cmds[ev]) != 1 || !strings.Contains(cmds[ev][0], "--client codex --action ensure") {
			t.Fatalf("%s wake commands %v", ev, cmds[ev])
		}
	}
	if len(cmds["SessionEnd"]) != 1 || !strings.Contains(cmds["SessionEnd"][0], "--action stop") {
		t.Fatalf("SessionEnd wake commands %v", cmds["SessionEnd"])
	}
}

// The generated wake commands are executed against a disposable home
// with the server unreachable: a native hook must fail open - exit 0,
// promptly, with no stdout - whether the wake capability exists on this
// host or not. No real session is ever nudged.
func TestInstalledWakeCommandsFailOpen(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("installed shell command execution; PowerShell runtime needs Windows gate")
	}
	_, here, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(here), "..", "..")
	bin := filepath.Join(t.TempDir(), "punk")
	build := exec.Command("go", "build", "-o", bin, "./cmd/punk")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	for _, client := range []string{"claude-code", "codex"} {
		t.Run(client, func(t *testing.T) {
			home := t.TempDir()
			env := []string{
				"PATH=" + os.Getenv("PATH"),
				"HOME=" + home,
				"USERPROFILE=" + home,
				"CODEX_HOME=" + filepath.Join(home, ".codex"),
				"PUNK_CREDENTIALS=" + filepath.Join(home, "credentials.json"),
				"PUNK_URL=http://127.0.0.1:19797",
				"PUNK_API_KEY=",
				"XDG_STATE_HOME=" + filepath.Join(home, "state"),
			}
			connect := exec.Command(bin, "connect", client, "--wake", "--no-mcp", "--no-skill", "--url", "http://127.0.0.1:19797")
			connect.Dir = home
			connect.Env = env
			if out, err := connect.CombinedOutput(); err != nil {
				t.Fatalf("connect: %v %s", err, out)
			}
			hooksFile := filepath.Join(home, ".claude", "settings.json")
			if client == "codex" {
				hooksFile = filepath.Join(home, ".codex", "hooks.json")
			}
			fixture, err := os.ReadFile(filepath.Join(root, "internal", "hookcli", "testdata", "inbox", client+".json"))
			if err != nil {
				t.Fatal(err)
			}
			payloadFor := func(event string) []byte {
				var p map[string]any
				if err := json.Unmarshal(fixture, &p); err != nil {
					t.Fatal(err)
				}
				p["hook_event_name"] = event
				raw, _ := json.Marshal(p)
				return raw
			}
			for ev, list := range wakeCommandsByEvent(t, hooksFile) {
				for _, command := range list {
					deadline, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					cmd := exec.CommandContext(deadline, "sh", "-c", command)
					cmd.Dir = home
					cmd.Env = env
					cmd.Stdin = bytes.NewReader(payloadFor(ev))
					var out, errw bytes.Buffer
					cmd.Stdout = &out
					cmd.Stderr = &errw
					runErr := cmd.Run()
					cancel()
					if runErr != nil {
						t.Fatalf("installed wake hook %q: %v stderr=%s", command, runErr, errw.String())
					}
					if out.Len() != 0 {
						t.Fatalf("wake hook must print no stdout, got %q", out.String())
					}
				}
			}
		})
	}
}
