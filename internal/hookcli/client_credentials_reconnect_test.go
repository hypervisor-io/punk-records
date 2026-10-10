package hookcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// codeops:trace repo=punk-records work_item=punk-connect-remote-url-review-20261009 spec=docs/client-credentials.md plan=phase-0/task-0-A test=TestReconnectRefreshesAllExistingManagedHookGroups evidence=docs/superpowers/reports/2026-10-09-client-credentials/builder-a.md
func TestReconnectRefreshesAllExistingManagedHookGroups(t *testing.T) {
	const oldURL = "http://old.example.test:9090/base"
	const newURL = "https://new.example.test/punk"
	const punkPath = "/opt/punk/bin/punk"
	type reconnectCase struct {
		name       string
		path       func(string) string
		seed       string
		install    func(string) error
		reconnect  func(string) error
		wantInbox  bool
		wantWake   bool
		checkExtra func(*testing.T, string)
	}
	cases := []reconnectCase{
		{
			name: "claude-code", path: func(d string) string { return filepath.Join(d, "settings.json") }, seed: `{"foreign":"keep"}`,
			install:   func(p string) error { _, err := ConnectClaudeCodeWake(p, punkPath, oldURL, "synthetic-ns"); return err },
			reconnect: func(p string) error { _, err := ConnectClaudeCodeNS(p, punkPath, newURL, "synthetic-ns"); return err }, wantInbox: true, wantWake: true,
		},
		{
			name: "cursor", path: func(d string) string { return filepath.Join(d, "hooks.json") }, seed: `{"foreign":"keep"}`,
			install: func(p string) error {
				_, err := ConnectCursorMessaging(p, punkPath, oldURL, "synthetic-ns")
				return err
			},
			reconnect: func(p string) error { _, err := ConnectCursorNS(p, punkPath, newURL, "synthetic-ns"); return err }, wantInbox: true,
		},
		{
			name: "antigravity", path: func(d string) string { return filepath.Join(d, "hooks.json") }, seed: `{"foreign":"keep"}`,
			install:   func(p string) error { _, err := ConnectAntigravityMessaging(p, punkPath, oldURL); return err },
			reconnect: func(p string) error { _, err := ConnectAntigravity(p, punkPath, newURL); return err }, wantInbox: true,
		},
		{
			name: "copilot", path: func(d string) string { return filepath.Join(d, "punk.json") }, seed: `{"foreign":"keep"}`,
			install:   func(p string) error { _, err := ConnectCopilotMessaging(p, punkPath, oldURL); return err },
			reconnect: func(p string) error { _, err := ConnectCopilot(p, punkPath, newURL); return err }, wantInbox: true,
		},
		{
			name: "hermes", path: func(d string) string { return filepath.Join(d, "config.yaml") }, seed: "foreign: keep\n",
			install:   func(p string) error { _, err := ConnectHermesMessaging(p, punkPath, oldURL); return err },
			reconnect: func(p string) error { _, err := ConnectHermes(p, punkPath, newURL); return err }, wantInbox: true,
		},
		{
			name: "codex", path: func(d string) string { return filepath.Join(d, "hooks.json") }, seed: `{"foreign":"keep"}`,
			install:   func(p string) error { _, err := ConnectCodexHooksWake(p, punkPath, oldURL, "synthetic-ns"); return err },
			reconnect: func(p string) error { _, err := ConnectCodexHooks(p, punkPath, newURL, "synthetic-ns"); return err }, wantInbox: true, wantWake: true,
		},
		{
			name: "cline", path: func(d string) string { return d },
			install: func(p string) error {
				_, err := ConnectCline(p, ClineConnectOpts{PunkPath: punkPath, ServerURL: oldURL, Namespace: "synthetic-ns", Messaging: true, GOOS: "linux"})
				return err
			},
			reconnect: func(p string) error {
				_, err := ConnectCline(p, ClineConnectOpts{PunkPath: punkPath, ServerURL: newURL, Namespace: "synthetic-ns", GOOS: "linux"})
				return err
			}, wantInbox: true,
			checkExtra: func(t *testing.T, p string) {
				foreign := filepath.Join(p, "ForeignHook")
				if got, err := os.ReadFile(foreign); err != nil || string(got) != "foreign keep\n" {
					t.Fatalf("foreign Cline hook changed: %q %v", got, err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p := tc.path(dir)
			if tc.name == "cline" {
				if err := os.WriteFile(filepath.Join(p, "ForeignHook"), []byte("foreign keep\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(p, []byte(tc.seed), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := tc.install(p); err != nil {
				t.Fatal(err)
			}
			if err := tc.reconnect(p); err != nil {
				t.Fatal(err)
			}

			var all string
			if tc.name == "cline" {
				for _, ev := range clineHookEvents {
					raw, err := os.ReadFile(filepath.Join(p, ev))
					if err != nil {
						t.Fatal(err)
					}
					all += string(raw)
				}
			} else {
				raw, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				all = string(raw)
			}
			if strings.Contains(all, oldURL) || !strings.Contains(all, newURL) {
				t.Fatalf("stale or missing URL after reconnect:\n%s", all)
			}
			if tc.wantInbox && !strings.Contains(all, "hook inbox") && !strings.Contains(all, "--messaging") {
				t.Fatalf("reconnect removed messaging opt-in:\n%s", all)
			}
			if tc.wantWake && !strings.Contains(all, "hook wake") {
				t.Fatalf("reconnect removed wake opt-in:\n%s", all)
			}
			for _, line := range strings.Split(all, "\n") {
				isPreservedOptional := strings.Contains(line, "hook inbox") || strings.Contains(line, "hook wake") ||
					(tc.name == "cline" && strings.Contains(line, "--messaging"))
				if isPreservedOptional && !strings.Contains(line, newURL) {
					t.Fatalf("preserved inbox/wake command did not refresh URL: %q", line)
				}
			}
			if tc.name != "cline" && !strings.Contains(all, "foreign") {
				t.Fatalf("reconnect removed foreign config:\n%s", all)
			}
			if tc.checkExtra != nil {
				tc.checkExtra(t, p)
			}
		})
	}
}
