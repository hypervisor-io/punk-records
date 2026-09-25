package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// M6 end to end with the BUILT punk binary: `punk connect claude-code
// --messaging` and `punk connect codex --messaging` write the hook
// entries into a throwaway HOME/CODEX_HOME, then the exact command
// strings those files contain are executed through a shell (as the
// clients execute them) with each client's native stdin payload,
// against the real messaging handlers. Nothing touches the live server
// or the real user config.
func TestBuiltPunkClaudeCodexInboxHooksDeliver(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-form hook commands")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..")
	bin := filepath.Join(t.TempDir(), "punk")
	build := exec.Command("go", "build", "-o", bin, "./cmd/punk")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	g := messagingServer(t)
	srv := httptest.NewServer(g.s.Router())
	defer srv.Close()

	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	state := t.TempDir()
	baseEnv := []string{"HOME=" + home, "USERPROFILE=" + home, "CODEX_HOME=" + codexHome,
		"XDG_STATE_HOME=" + state, "PATH=" + os.Getenv("PATH"), "PUNK_API_KEY="}
	run := func(env []string, stdin string, args ...string) (string, string) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env = append(append([]string{}, baseEnv...), env...)
		cmd.Stdin = strings.NewReader(stdin)
		var out, errb strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			t.Fatalf("punk %v: %v\n%s", args, err, errb.String())
		}
		return out.String(), errb.String()
	}
	for _, target := range []string{"claude-code", "codex"} {
		out, errs := run(nil, "", "connect", target, "--messaging", "--no-mcp", "--no-skill", "--url", srv.URL)
		if strings.Contains(errs, "error") {
			t.Fatalf("connect %s: %s %s", target, out, errs)
		}
	}
	// Without --messaging a second connect keeps the inbox groups (the
	// capture merge never claims them) and changes nothing else.
	claudePath := filepath.Join(home, ".claude", "settings.json")
	before, _ := os.ReadFile(claudePath)
	run(nil, "", "connect", "claude-code", "--no-mcp", "--no-skill", "--url", srv.URL)
	after, _ := os.ReadFile(claudePath)
	if string(before) != string(after) {
		t.Fatalf("plain reconnect changed a messaging install:\n%s\n---\n%s", before, after)
	}

	// commandFor reads the installed inbox command for event.
	commandFor := func(path, event string) string {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Hooks map[string][]struct {
				Hooks []struct{ Command string } `json:"hooks"`
			} `json:"hooks"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		for _, g := range doc.Hooks[event] {
			for _, h := range g.Hooks {
				if strings.Contains(h.Command, " hook inbox ") {
					return h.Command
				}
			}
		}
		t.Fatalf("no inbox command for %s in %s", event, path)
		return ""
	}
	shell := func(env []string, command, stdin string) string {
		t.Helper()
		cmd := exec.Command("sh", "-c", command)
		cmd.Env = append(append([]string{}, baseEnv...), env...)
		cmd.Stdin = strings.NewReader(stdin)
		var out, errb strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			t.Fatalf("hook %q: %v\n%s", command, err, errb.String())
		}
		return out.String()
	}

	const ns = "m6-built"
	g.register(t, ns, "lead")
	send := func(recipient, body string) string {
		rec := g.do(t, http.MethodPost, "/v1/namespaces/"+ns+"/messages",
			`{"sender":"lead","recipient":"`+recipient+`","body":"`+body+`"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("send: %d %s", rec.Code, rec.Body)
		}
		var m struct{ ID string }
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return m.ID
	}
	nsEnv := []string{"PUNK_NAMESPACE=" + ns}

	for _, c := range []struct{ client, path string }{
		{"claude-code", claudePath},
		{"codex", filepath.Join(codexHome, "hooks.json")},
	} {
		addr := c.client + ":sess-" + c.client
		startCmd := commandFor(c.path, "SessionStart")
		promptCmd := commandFor(c.path, "UserPromptSubmit")
		stopCmd := commandFor(c.path, "Stop")
		payload := func(event, extra string) string {
			return `{"session_id":"sess-` + c.client + `","cwd":"/work/m6","hook_event_name":"` + event + `"` + extra + `}`
		}

		// SessionStart registers the address (empty inbox: no output).
		if out := shell(nsEnv, startCmd, payload("SessionStart", `,"source":"startup"`)); out != "" {
			t.Fatalf("%s empty SessionStart printed %q", c.client, out)
		}
		id1 := send(addr, "first task")
		out := shell(nsEnv, promptCmd, payload("UserPromptSubmit", `,"prompt":"hi"`))
		var ctx struct {
			HookSpecificOutput struct {
				HookEventName     string `json:"hookEventName"`
				AdditionalContext string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal([]byte(out), &ctx); err != nil || ctx.HookSpecificOutput.HookEventName != "UserPromptSubmit" {
			t.Fatalf("%s prompt reply %q", c.client, out)
		}
		wantReply := `send_message(namespace="` + ns + `", sender="` + addr + `", recipient="lead", reply_to="` + id1 + `"`
		if !strings.Contains(ctx.HookSpecificOutput.AdditionalContext, "first task") ||
			!strings.Contains(ctx.HookSpecificOutput.AdditionalContext, wantReply) {
			t.Fatalf("%s context: %s", c.client, ctx.HookSpecificOutput.AdditionalContext)
		}

		// stop_hook_active: prints nothing, the message stays unread.
		send(addr, "second task")
		if out := shell(nsEnv, stopCmd, payload("Stop", `,"stop_hook_active":true`)); out != "" {
			t.Fatalf("%s stop_hook_active printed %q", c.client, out)
		}
		rec := g.do(t, http.MethodGet, "/v1/namespaces/"+ns+"/messages/count?agent="+addr, "")
		if strings.TrimSpace(rec.Body.String()) != `{"unread":1}` {
			t.Fatalf("%s: %s", c.client, rec.Body.String())
		}
		out = shell(nsEnv, stopCmd, payload("Stop", `,"stop_hook_active":false`))
		var blk struct{ Decision, Reason string }
		if err := json.Unmarshal([]byte(out), &blk); err != nil || blk.Decision != "block" || !strings.Contains(blk.Reason, "second task") {
			t.Fatalf("%s stop reply %q", c.client, out)
		}
		rec = g.do(t, http.MethodGet, "/v1/namespaces/"+ns+"/messages/count?agent="+addr, "")
		if strings.TrimSpace(rec.Body.String()) != `{"unread":0}` {
			t.Fatalf("%s not acked: %s", c.client, rec.Body.String())
		}
		// PUNK_MESSAGING=0 kills delivery even though the entry opts in.
		send(addr, "third")
		if out := shell(append(nsEnv, "PUNK_MESSAGING=0"), promptCmd, payload("UserPromptSubmit", `,"prompt":"x"`)); out != "" {
			t.Fatalf("%s PUNK_MESSAGING=0 printed %q", c.client, out)
		}
	}

	// A dead server fails open: exit 0, no stdout (Codex Stop accepts
	// empty output; plain text would be invalid).
	srv.Close()
	stop := commandFor(filepath.Join(codexHome, "hooks.json"), "Stop")
	if out := shell(nsEnv, stop, `{"session_id":"x","cwd":"/w","hook_event_name":"Stop"}`); out != "" {
		t.Fatalf("dead server printed %q", out)
	}
}
