package hookcli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/api"
	"github.com/hypervisor-io/punk-records/internal/authz"
	"github.com/hypervisor-io/punk-records/internal/bus"
	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/region"
	"github.com/hypervisor-io/punk-records/internal/store"
	"gopkg.in/yaml.v3"
)

// Unlike the direct CLI matrix, this gate installs each target first and
// executes ONLY commands discovered under its native event key. Runtime env
// omits PUNK_MESSAGING so a missing connect-time --messaging flag fails delivery.
// Cline executes its generated native executable, not a reconstructed command.
func TestInstalledInboxHookMatrix(t *testing.T) {
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
	for _, client := range []string{"claude-code", "codex", "cursor", "copilot", "antigravity", "hermes", "cline"} {
		t.Run(client, func(t *testing.T) {
			ctx := context.Background()
			home := t.TempDir()
			db, err := store.Open("sqlite", filepath.Join(home, "db.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.MigrateUp(ctx); err != nil {
				t.Fatal(err)
			}
			reg := region.New(db, nil)
			addr := client + ":native-session"
			for _, ns := range []string{"decoy", "team"} {
				for _, a := range []string{"lead", addr} {
					if err := reg.Register(ctx, ns, a, ""); err != nil {
						t.Fatal(err)
					}
				}
			}
			send := func(ns, body string) *region.Message {
				t.Helper()
				m, e := reg.SendMessage(ctx, region.MessageInput{Namespace: ns, Sender: "lead", Recipient: addr, Body: body})
				if e != nil {
					t.Fatal(e)
				}
				return m
			}
			decoy := send("decoy", "decoy first")
			keys := api.NewKeys(db, nil)
			az := authz.New(db, nil)
			keys.SetAuthorizer(az)
			token, err := keys.Create(ctx, "hook", "hook")
			if err != nil {
				t.Fatal(err)
			}
			for _, op := range []authz.Op{authz.OpRead, authz.OpWrite} {
				if err := az.Grant(ctx, "hook", "team", op); err != nil {
					t.Fatal(err)
				}
			}
			srv := httptest.NewServer(api.New(slog.New(slog.DiscardHandler), api.Deps{Region: reg, Memory: memory.New(db, nil), Bus: bus.New(), Keys: keys}).Router())
			defer srv.Close()
			env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "USERPROFILE=" + home, "CODEX_HOME=" + filepath.Join(home, ".codex"), "XDG_STATE_HOME=" + filepath.Join(home, "state"), "PUNK_API_KEY=" + token, "PUNK_NAMESPACE=team"}
			args := []string{"connect", client, "--messaging", "--no-mcp", "--url", srv.URL}
			if client != "cline" {
				args = append(args, "--no-skill")
			}
			connect := func() {
				t.Helper()
				cmd := exec.Command(bin, args...)
				cmd.Dir = home
				cmd.Env = env
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("connect: %v %s", err, out)
				}
			}
			connect()
			event := "SessionStart"
			stopEvent := "Stop"
			switch client {
			case "cursor":
				event = "sessionStart"
				stopEvent = "stop"
			case "antigravity":
				event = "PreInvocation"
			case "hermes":
				event = "pre_llm_call"
				stopEvent = ""
			case "cline":
				event = "UserPromptSubmit"
				stopEvent = ""
			}
			command := installedInboxCommand(t, home, client, event)
			connect()
			if got := installedInboxCommand(t, home, client, event); got != command {
				t.Fatal("reconnect changed installed command")
			}
			fixture, err := os.ReadFile(filepath.Join(filepath.Dir(here), "testdata", "inbox", client+".json"))
			if err != nil {
				t.Fatal(err)
			}
			run := func(command string, stop bool, extra ...string) string {
				t.Helper()
				raw := fixture
				if stop && client != "antigravity" {
					var p map[string]any
					json.Unmarshal(raw, &p)
					p["hook_event_name"] = stopEvent
					raw, _ = json.Marshal(p)
				}
				deadline, cancel := context.WithTimeout(ctx, 15*time.Second)
				defer cancel()
				var cmd *exec.Cmd
				if client == "cline" {
					cmd = exec.CommandContext(deadline, command)
				} else {
					cmd = exec.CommandContext(deadline, "sh", "-c", command)
				}
				cmd.Dir = home
				cmd.Env = append(append([]string{}, env...), extra...)
				cmd.Stdin = bytes.NewReader(raw)
				var out, errw bytes.Buffer
				cmd.Stdout = &out
				cmd.Stderr = &errw
				if err := cmd.Run(); err != nil {
					t.Fatalf("installed hook %q: %v stderr=%s", command, err, errw.String())
				}
				return out.String()
			}
			empty := nativeExpectedReply(client, "", false)
			if got := run(command, false); got != empty {
				t.Fatalf("empty installed=%q want=%q", got, empty)
			}
			m := send("team", "installed hook delivery")
			if got, want := run(command, false), nativeExpectedReply(client, nativeExpectedEnvelope(addr, m), false); got != want {
				t.Fatalf("installed one got=%q want=%q", got, want)
			}
			if n, err := reg.CountUnreadMessages(ctx, "team", addr); err != nil || n != 0 {
				t.Fatalf("installed ACK=%d %v", n, err)
			}
			if stopEvent != "" {
				stopCmd := installedInboxCommand(t, home, client, stopEvent)
				for i := 0; i < 6; i++ {
					m = send("team", fmt.Sprintf("installed continuation %d", i))
					want := nativeExpectedReply(client, "", true)
					if i < 5 {
						want = nativeExpectedReply(client, nativeExpectedEnvelope(addr, m), true)
					}
					if got := run(stopCmd, true); got != want {
						t.Fatalf("installed cap %d got=%q want=%q", i, got, want)
					}
				}
				if n, err := reg.CountUnreadMessages(ctx, "team", addr); err != nil || n != 1 {
					t.Fatalf("cap sixth must remain unread %d %v", n, err)
				}
			} else {
				m = send("team", "catchup despite cap zero")
				if got, want := run(command, false, "PUNK_MESSAGING_MAX_CONTINUE=0"), nativeExpectedReply(client, nativeExpectedEnvelope(addr, m), false); got != want {
					t.Fatalf("catchup cap got=%q want=%q", got, want)
				}
			}
			rows, err := reg.ReadMessages(ctx, "decoy", addr, 10)
			if err != nil || len(rows) != 1 || rows[0].ID != decoy.ID {
				t.Fatalf("decoy=%+v %v", rows, err)
			}
			srv.Close()
			if got := run(command, false); got != empty {
				t.Fatalf("installed down=%q want=%q", got, empty)
			}
		})
	}
}

func installedInboxCommand(t *testing.T, home, client, event string) string {
	t.Helper()
	if client == "cline" {
		p := filepath.Join(home, "Documents", "Cline", "Hooks", event)
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(p)
		if err != nil || info.Mode().Perm()&0111 == 0 || !strings.Contains(string(b), "--messaging") {
			t.Fatalf("invalid installed Cline executable %s %v", b, err)
		}
		return p
	}
	paths := map[string]string{"claude-code": ".claude/settings.json", "codex": ".codex/hooks.json", "cursor": ".cursor/hooks.json", "copilot": ".copilot/hooks/punk.json", "antigravity": ".gemini/config/hooks.json", "hermes": ".hermes/config.yaml"}
	b, err := os.ReadFile(filepath.Join(home, paths[client]))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if client == "hermes" {
		err = yaml.Unmarshal(b, &doc)
	} else {
		err = json.Unmarshal(b, &doc)
	}
	if err != nil {
		t.Fatal(err)
	}
	section := "hooks"
	if client == "antigravity" {
		section = "punk"
	} // native plugin-name root
	hooks, ok := doc[section].(map[string]any)
	if !ok {
		t.Fatalf("missing hooks in %s", b)
	}
	var commands []string
	var visit func(any)
	visit = func(v any) {
		switch x := v.(type) {
		case []any:
			for _, v := range x {
				visit(v)
			}
		case map[string]any:
			if cmd, ok := x["command"].(string); ok && strings.Contains(cmd, " hook inbox ") {
				commands = append(commands, cmd)
			}
			if nested, ok := x["hooks"]; ok {
				visit(nested)
			}
		}
	}
	visit(hooks[event])
	if len(commands) != 1 || !strings.Contains(commands[0], "--messaging") {
		t.Fatalf("%s %s wants one opted-in command, got %q", client, event, commands)
	}
	return commands[0]
}
