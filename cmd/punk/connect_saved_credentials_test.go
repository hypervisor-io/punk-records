package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

type installedHookRequest struct {
	method        string
	path          string
	authenticated bool
}

func collectInstalledHookCommands(v any, out *[]string) {
	switch n := v.(type) {
	case map[string]any:
		for key, value := range n {
			if key == "command" {
				if command, ok := value.(string); ok && strings.Contains(command, " hook ") {
					*out = append(*out, command)
				}
			}
			collectInstalledHookCommands(value, out)
		}
	case []any:
		for _, value := range n {
			collectInstalledHookCommands(value, out)
		}
	}
}

func readInstalledHookCommands(t *testing.T, path string, script bool) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if script {
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.Contains(line, " hook ") {
				return []string{line}
			}
		}
		t.Fatalf("no managed hook command in %s", path)
	}
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse installed hooks %s: %v", path, err)
	}
	var commands []string
	collectInstalledHookCommands(doc, &commands)
	if len(commands) == 0 {
		t.Fatalf("no managed hook commands in %s", path)
	}
	return commands
}

func writeSyntheticCredentials(t *testing.T, path, serverURL, apiKey string) {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"url": serverURL, "api_key": apiKey})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// codeops:trace repo=punk-records work_item=punk-connect-remote-url-review-20261009 spec=docs/client-credentials.md plan=phase-2/task-2-1 test=TestConnectSavedCredentialsSevenClientCommandMatrix evidence=docs/superpowers/reports/2026-10-09-client-credentials/builder-a.md
func TestConnectSavedCredentialsSevenClientCommandMatrix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("installed shell command execution requires the Windows client gate")
	}
	_, here, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(here), "..", "..")
	bin := filepath.Join(t.TempDir(), "punk")
	build := exec.Command("go", "build", "-o", bin, "./cmd/punk")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build disposable punk: %v\n%s", err, out)
	}

	type clientCase struct {
		name      string
		firstFlag string
		hookPath  func(string) string
		fixture   string
		script    bool
		wantWake  bool
	}
	cases := []clientCase{
		{name: "claude-code", firstFlag: "--wake", hookPath: func(home string) string { return filepath.Join(home, ".claude", "settings.json") }, fixture: "claude-code.json", wantWake: true},
		{name: "cursor", firstFlag: "--messaging", hookPath: func(home string) string { return filepath.Join(home, ".cursor", "hooks.json") }, fixture: "cursor.json"},
		{name: "antigravity", firstFlag: "--messaging", hookPath: func(home string) string { return filepath.Join(home, ".gemini", "config", "hooks.json") }, fixture: "antigravity.json"},
		{name: "copilot", firstFlag: "--messaging", hookPath: func(home string) string { return filepath.Join(home, ".copilot", "hooks", "punk.json") }, fixture: "copilot.json"},
		{name: "hermes", firstFlag: "--messaging", hookPath: func(home string) string { return filepath.Join(home, ".hermes", "config.yaml") }, fixture: "hermes.json"},
		{name: "codex", firstFlag: "--wake", hookPath: func(home string) string { return filepath.Join(home, ".codex", "hooks.json") }, fixture: "codex.json", wantWake: true},
		{name: "cline", firstFlag: "--messaging", hookPath: func(home string) string {
			return filepath.Join(home, "Documents", "Cline", "Hooks", "UserPromptSubmit")
		}, fixture: "cline.json", script: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			work := t.TempDir()
			credentials := filepath.Join(home, "credentials.json")
			oldURL := "http://127.0.0.1:1/old"
			writeSyntheticCredentials(t, credentials, oldURL, "synthetic-old-key")
			env := []string{
				"PATH=" + os.Getenv("PATH"),
				"HOME=" + home,
				"USERPROFILE=" + home,
				"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
				"XDG_STATE_HOME=" + filepath.Join(home, ".state"),
				"CODEX_HOME=" + filepath.Join(home, ".codex"),
				"COPILOT_HOME=" + filepath.Join(home, ".copilot"),
				"CLINE_MCP_SETTINGS_PATH=" + filepath.Join(home, ".cline", "mcp.json"),
				"PUNK_CREDENTIALS=" + credentials,
				"PUNK_URL=",
				"PUNK_API_KEY=",
				"PUNK_NAMESPACE=",
				"PUNK_MESSAGING=",
			}
			connect := func(extra ...string) {
				args := []string{"connect", tc.name}
				if tc.name != "cline" {
					args = append(args, "--no-skill")
				}
				args = append(args, extra...)
				cmd := exec.Command(bin, args...)
				cmd.Dir, cmd.Env = work, env
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("saved-only connect %v: %v\n%s", args, err, out)
				}
			}
			connect(tc.firstFlag)

			const currentKey = "synthetic-current-key"
			var mu sync.Mutex
			var requests []installedHookRequest
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				requests = append(requests, installedHookRequest{
					method: r.Method, path: r.URL.Path,
					authenticated: r.Header.Get("Authorization") == "Bearer "+currentKey,
				})
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/context"):
					_, _ = io.WriteString(w, `{"namespace":"synthetic-ns","context":""}`)
				case strings.HasSuffix(r.URL.Path, "/namespace"):
					_, _ = io.WriteString(w, `{"namespace":"synthetic-ns"}`)
				case strings.HasSuffix(r.URL.Path, "/members"):
					_, _ = io.WriteString(w, `{"status":"registered","namespace":"synthetic-ns"}`)
				case strings.HasSuffix(r.URL.Path, "/messages"):
					_, _ = io.WriteString(w, `{"messages":[]}`)
				default:
					_, _ = io.WriteString(w, `{"status":"stored"}`)
				}
			}))
			defer srv.Close()
			currentURL := srv.URL + "/current"
			writeSyntheticCredentials(t, credentials, currentURL, currentKey)
			connect() // Omitted opt-in must refresh, not remove, existing groups.

			commands := readInstalledHookCommands(t, tc.hookPath(home), tc.script)
			inboxCount, wakeCount := 0, 0
			var capture string
			for _, command := range commands {
				managedOptional := strings.Contains(command, " hook inbox ") || strings.Contains(command, " hook wake ") ||
					(tc.script && strings.Contains(command, "--messaging"))
				if managedOptional && !strings.Contains(command, currentURL) {
					t.Fatalf("preserved inbox/wake command did not refresh URL: %q", command)
				}
				if strings.Contains(command, " hook inbox ") || (tc.script && strings.Contains(command, "--messaging")) {
					inboxCount++
				}
				if strings.Contains(command, " hook wake ") {
					wakeCount++
				}
				captureCandidate := !strings.Contains(command, " hook inbox ") && !strings.Contains(command, " hook wake ")
				if tc.name == "antigravity" {
					captureCandidate = captureCandidate && strings.Contains(command, "--event PostToolUse")
				}
				if capture == "" && captureCandidate {
					capture = command
				}
			}
			if inboxCount == 0 || (tc.wantWake && wakeCount == 0) {
				t.Fatalf("reconnect removed opt-ins: inbox=%d wake=%d commands=%v", inboxCount, wakeCount, commands)
			}
			if capture == "" || !strings.Contains(capture, currentURL) {
				t.Fatalf("missing refreshed capture command: %q", capture)
			}

			fixture, err := os.ReadFile(filepath.Join(root, "internal", "hookcli", "testdata", "inbox", tc.fixture))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			runHook := exec.CommandContext(ctx, "sh", "-c", capture)
			runHook.Dir, runHook.Env, runHook.Stdin = work, env, bytes.NewReader(fixture)
			if out, err := runHook.CombinedOutput(); err != nil {
				t.Fatalf("execute installed hook: %v\n%s", err, out)
			}
			mu.Lock()
			observed := append([]installedHookRequest(nil), requests...)
			mu.Unlock()
			hookAuthenticated := false
			for _, request := range observed {
				if !request.authenticated {
					t.Fatalf("installed hook request lacked selected saved authentication: %s %s", request.method, request.path)
				}
				if request.method == http.MethodPost && request.path == "/current/v1/agent/hooks" {
					hookAuthenticated = true
				}
			}
			if !hookAuthenticated {
				t.Fatalf("installed hook made no authenticated capture request; observed=%v", observed)
			}
			t.Logf("%s: actual POST /current/v1/agent/hooks authenticated=true; inbox=%d wake=%d", tc.name, inboxCount, wakeCount)
		})
	}
}

func (r installedHookRequest) String() string {
	return fmt.Sprintf("%s %s authenticated=%t", r.method, r.path, r.authenticated)
}
