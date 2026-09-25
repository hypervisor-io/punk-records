package hookcli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// M6 tests. Contracts verified 2026-09-25 against
// https://code.claude.com/docs/en/hooks (Claude Code 2.1.282 installed)
// and https://developers.openai.com/codex/hooks (codex-cli 0.156.1
// installed): SessionStart/UserPromptSubmit take nested
// hookSpecificOutput.additionalContext; Stop takes
// {"decision":"block","reason":...}; stop_hook_active is set on re-entry;
// Codex Stop rejects plain text but accepts empty output. The Claude Code
// asyncRewake field has no documented minimum version and is not used.

const m6UserSettings = `{"model":"opus","hooks":{"Stop":[{"hooks":[{"type":"command","command":"my-stop.sh"}]}],"SessionStart":[{"matcher":"startup","hooks":[{"type":"command","command":"echo hi && date"}]}]}}`

// Without --messaging the connect output is byte-identical to the
// pre-M6 output (golden files produced by running HEAD's
// ConnectClaudeCodeNS/ConnectCodexHooks on the same input).
func TestConnectWithoutMessagingIsByteIdenticalToPreM6(t *testing.T) {
	for _, c := range []struct {
		golden string
		fn     func(p string) (bool, error)
	}{
		{"claude-settings-pre-m6.json", func(p string) (bool, error) {
			return ConnectClaudeCodeNS(p, "/usr/local/bin/punk", "http://localhost:9090", "proj-ns")
		}},
		{"codex-hooks-pre-m6.json", func(p string) (bool, error) {
			return ConnectCodexHooks(p, "/usr/local/bin/punk", "http://localhost:9090", "proj-ns")
		}},
	} {
		p := filepath.Join(t.TempDir(), "s.json")
		if err := os.WriteFile(p, []byte(m6UserSettings), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := c.fn(p); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(p)
		want, err := os.ReadFile(filepath.Join("testdata", "inbox_m6", c.golden))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s: output drifted without --messaging\n got: %s\nwant: %s", c.golden, got, want)
		}
	}
}

func inboxGroups(t *testing.T, hooks map[string]any, event, client string) []map[string]any {
	t.Helper()
	var out []map[string]any
	arr, _ := hooks[event].([]any)
	for _, g := range arr {
		if isPunkManagedInboxGroup(g, "/usr/local/bin/punk", client) {
			out = append(out, g.(map[string]any))
		}
	}
	return out
}

func TestConnectClaudeCodeMessagingAddsInboxGroups(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(p, []byte(m6UserSettings), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := ConnectClaudeCodeMessaging(p, "/usr/local/bin/punk", "http://localhost:9090", "proj-ns")
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	m := readSettings(t, p)
	hooks := m["hooks"].(map[string]any)
	want := map[string]string{
		"SessionStart":     "/usr/local/bin/punk hook inbox --client claude-code --mode context --url http://localhost:9090 --ns proj-ns --messaging",
		"UserPromptSubmit": "/usr/local/bin/punk hook inbox --client claude-code --mode context --url http://localhost:9090 --ns proj-ns --messaging",
		"Stop":             "/usr/local/bin/punk hook inbox --client claude-code --mode continue --url http://localhost:9090 --ns proj-ns --messaging",
	}
	for ev, cmd := range want {
		gs := inboxGroups(t, hooks, ev, "claude-code")
		if len(gs) != 1 {
			t.Fatalf("%s: %d inbox groups", ev, len(gs))
		}
		h := gs[0]["hooks"].([]any)[0].(map[string]any)
		if h["command"] != cmd || h["type"] != "command" {
			t.Fatalf("%s inbox handler %v", ev, h)
		}
		if _, has := gs[0]["matcher"]; has {
			t.Fatalf("%s: claude inbox group must fire on every source: %v", ev, gs[0])
		}
		// Capture group still present, separately.
		raw, _ := json.Marshal(hooks[ev])
		if !strings.Contains(string(raw), `/usr/local/bin/punk hook --url http://localhost:9090 --ns proj-ns"`) {
			t.Fatalf("%s lost the capture group: %s", ev, raw)
		}
	}
	if len(inboxGroups(t, hooks, "PostToolUse", "claude-code")) != 0 {
		t.Fatal("PostToolUse must not get an inbox group")
	}
	raw, _ := os.ReadFile(p)
	for _, keep := range []string{"my-stop.sh", "echo hi && date"} {
		if !strings.Contains(string(raw), keep) {
			t.Fatalf("user entry %q lost: %s", keep, raw)
		}
	}
	if m["model"] != "opus" {
		t.Fatal("unrelated settings lost")
	}

	// Idempotent: rerun reports unchanged; changing url/ns replaces in place.
	before, _ := os.ReadFile(p)
	if changed, err := ConnectClaudeCodeMessaging(p, "/usr/local/bin/punk", "http://localhost:9090", "proj-ns"); err != nil || changed {
		t.Fatalf("rerun changed=%v err=%v", changed, err)
	}
	after, _ := os.ReadFile(p)
	if !bytes.Equal(before, after) {
		t.Fatal("rerun rewrote the file")
	}
	if _, err := ConnectClaudeCodeMessaging(p, "/usr/local/bin/punk", "http://other:1", "ns2"); err != nil {
		t.Fatal(err)
	}
	hooks = readSettings(t, p)["hooks"].(map[string]any)
	for ev := range want {
		gs := inboxGroups(t, hooks, ev, "claude-code")
		if len(gs) != 1 || !strings.Contains(fmt.Sprint(gs[0]), "http://other:1 --ns ns2") {
			t.Fatalf("%s: not replaced in place: %v", ev, gs)
		}
		raw, _ := json.Marshal(hooks[ev])
		if strings.Count(string(raw), "punk hook --url") != 1 {
			t.Fatalf("%s: capture group duplicated: %s", ev, raw)
		}
	}

	// A plain (non-messaging) reconnect leaves the inbox groups alone: the
	// capture merge never claims "punk hook inbox", and it is a
	// byte-identical no-op over a messaging install.
	before, _ = os.ReadFile(p)
	if changed, err := ConnectClaudeCodeNS(p, "/usr/local/bin/punk", "http://other:1", "ns2"); err != nil || changed {
		t.Fatalf("plain reconnect changed=%v err=%v", changed, err)
	}
	after, _ = os.ReadFile(p)
	if !bytes.Equal(before, after) {
		t.Fatalf("plain reconnect reordered:\n%s\n---\n%s", before, after)
	}
	hooks = readSettings(t, p)["hooks"].(map[string]any)
	if len(inboxGroups(t, hooks, "Stop", "claude-code")) != 1 {
		t.Fatal("capture reconnect deleted the inbox group")
	}
}

func TestIsPunkManagedIgnoresInboxCommands(t *testing.T) {
	p := "/usr/local/bin/punk"
	for _, cmd := range []string{
		p + " hook inbox --client claude-code --mode context --url http://x --messaging",
		`"/opt/my tools/punk" hook inbox --client codex --mode continue --url http://x`,
		p + " hook  inbox --client x",
	} {
		pp := p
		if strings.HasPrefix(cmd, `"`) {
			pp = "/opt/my tools/punk"
		}
		if isPunkManaged(cmd, pp) {
			t.Errorf("capture detector claimed inbox command %q", cmd)
		}
	}
	for _, cmd := range []string{p + " hook --url http://x", p + " hook --url http://x --ns inboxes", p + " hook --from codex --url x"} {
		if !isPunkManaged(cmd, p) {
			t.Errorf("capture detector lost %q", cmd)
		}
	}
}

func TestConnectCodexHooksMessagingAddsInboxGroups(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hooks.json")
	if err := os.WriteFile(p, []byte(m6UserSettings), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectCodexHooksMessaging(p, "/usr/local/bin/punk", "http://localhost:9090", ""); err != nil {
		t.Fatal(err)
	}
	hooks := readSettings(t, p)["hooks"].(map[string]any)
	for ev, mode := range map[string]string{"SessionStart": "context", "UserPromptSubmit": "context", "Stop": "continue"} {
		gs := inboxGroups(t, hooks, ev, "codex")
		if len(gs) != 1 {
			t.Fatalf("%s: %d codex inbox groups", ev, len(gs))
		}
		cmd := gs[0]["hooks"].([]any)[0].(map[string]any)["command"]
		if cmd != "/usr/local/bin/punk hook inbox --client codex --mode "+mode+" --url http://localhost:9090 --messaging" {
			t.Fatalf("%s: %v", ev, cmd)
		}
		if ev == "SessionStart" && gs[0]["matcher"] != codexSessionStartMatcher {
			t.Fatalf("codex SessionStart inbox matcher %v", gs[0]["matcher"])
		}
		if ev != "SessionStart" && gs[0]["matcher"] != nil {
			t.Fatalf("%s must carry no matcher", ev)
		}
		// Claude-code inbox detector must not claim codex entries.
		if len(inboxGroups(t, hooks, ev, "claude-code")) != 0 {
			t.Fatal("client-scoped inbox detection leaked")
		}
	}
	before, _ := os.ReadFile(p)
	if changed, err := ConnectCodexHooksMessaging(p, "/usr/local/bin/punk", "http://localhost:9090", ""); err != nil || changed {
		t.Fatalf("rerun changed=%v err=%v", changed, err)
	}
	after, _ := os.ReadFile(p)
	if !bytes.Equal(before, after) {
		t.Fatal("rerun rewrote")
	}
	// Scope inspection counts only capture registrations: inbox groups
	// are not capture destinations and must not confuse verify.
	sc := inspectCodexHookScope(p, "/usr/local/bin/punk")
	if sc.Err != nil || len(sc.Registrations) != 4 {
		t.Fatalf("scope inspection: %+v", sc)
	}
}

func claudeReq(event string, cont bool, rendered string, stopActive bool) InboxReplyRequest {
	return InboxReplyRequest{Mode: "context", Payload: InboxPayload{Event: event, StopHookActive: stopActive},
		Delivery: Delivery{Namespace: "ns1", Address: "claude-code:s1", Rendered: rendered, Continue: cont}}
}

func TestClaudeInboxReplyShapes(t *testing.T) {
	text := "[PUNK INBOX] 1 message(s)\nline \"two\" <b>"
	for _, ev := range []string{"SessionStart", "UserPromptSubmit"} {
		r := claudeInboxReply(claudeReq(ev, false, text, false))
		want := `{"hookSpecificOutput":{"additionalContext":"[PUNK INBOX] 1 message(s)\nline \"two\" \u003cb\u003e","hookEventName":"` + ev + `"}}` + "\n"
		if string(r.Out) != want || !r.Delivered {
			t.Fatalf("%s: %q delivered=%v", ev, r.Out, r.Delivered)
		}
	}
	r := claudeInboxReply(claudeReq("Stop", true, text, false))
	if string(r.Out) != `{"decision":"block","reason":"[PUNK INBOX] 1 message(s)\nline \"two\" \u003cb\u003e"}`+"\n" || !r.Delivered {
		t.Fatalf("stop: %q", r.Out)
	}
	for name, req := range map[string]InboxReplyRequest{
		"stop without continuation": claudeReq("Stop", false, text, false),
		"empty":                     claudeReq("UserPromptSubmit", false, "", false),
		"empty stop":                claudeReq("Stop", true, "", false),
		"unknown event":             claudeReq("PostToolUse", false, text, false),
	} {
		if r := claudeInboxReply(req); len(r.Out) != 0 || r.Delivered {
			t.Fatalf("%s: printed %q delivered=%v", name, r.Out, r.Delivered)
		}
	}
	if !claudeCanCarry(InboxPayload{Event: "Stop"}, true) || claudeCanCarry(InboxPayload{Event: "Stop"}, false) ||
		!claudeCanCarry(InboxPayload{Event: "SessionStart"}, false) || claudeCanCarry(InboxPayload{Event: ""}, false) {
		t.Fatal("CanCarry gate wrong")
	}
}

// Registration order must not matter: the built-in parser survives an
// adapter registration and the adapter's fields survive any later
// registration that sets other fields.
func TestInboxRegistryComposesRegistrations(t *testing.T) {
	c, ok := lookupInboxClient("claude")
	if !ok || c.Name != "claude-code" || c.Reply == nil || !c.AllowContinue || c.MaxRenderBytes != claudeInboxMaxRender || c.Parse == nil {
		t.Fatalf("claude-code registration incomplete: ok=%v %+v", ok, c)
	}
	p, err := c.Parse("", []byte(`{"session_id":"s","hook_event_name":"Stop","stop_hook_active":true}`))
	if err != nil || !p.StopHookActive || p.Event != "Stop" {
		t.Fatalf("built-in parser lost: %+v %v", p, err)
	}
	// A later partial registration keeps earlier fields.
	restore := swapInboxClient(InboxClient{Name: "codex", MaxRenderBytes: 1234})
	c, _ = lookupInboxClient("codex")
	if c.Reply == nil || c.CanCarry == nil || c.MaxRenderBytes != 1234 || c.Parse == nil {
		t.Fatalf("partial registration overwrote fields: %+v", c)
	}
	restore()
	c, _ = lookupInboxClient("codex")
	if c.MaxRenderBytes != codexInboxMaxRender {
		t.Fatalf("restore failed: %+v", c)
	}
	// Every client registers a writer somewhere in the package by now,
	// and none of those registrations lost its built-in parser.
	for _, name := range []string{"claude-code", "codex", "copilot", "cursor", "antigravity", "cline", "hermes"} {
		c, ok := lookupInboxClient(name)
		if !ok || c.Parse == nil {
			t.Errorf("%s: parser missing", name)
		}
	}
}

// End to end through Inbox with the fake M3/M10 server.
func TestClaudeInboxEndToEnd(t *testing.T) {
	inboxTestEnv(t)
	f, srv := newFakeInbox(t)
	run := func(client, mode, stdin string) string {
		out, _ := runInbox(t, InboxOpts{Client: client, Mode: mode, BaseURL: srv.URL, Namespace: "ns1"}, stdin)
		return out
	}
	add := func(id, recipient string) {
		f.add(InboxMessage{ID: id, Namespace: "ns1", Sender: "lead", Recipient: recipient, Body: "do " + id, CreatedAt: "t"})
	}
	for _, client := range []string{"claude-code", "codex"} {
		addr := client + ":s-" + client
		ups := `{"session_id":"s-` + client + `","cwd":"/w","hook_event_name":"UserPromptSubmit","prompt":"x"}`
		stop := `{"session_id":"s-` + client + `","cwd":"/w","hook_event_name":"Stop","stop_hook_active":false}`
		stopActive := `{"session_id":"s-` + client + `","cwd":"/w","hook_event_name":"Stop","stop_hook_active":true}`

		add(client+"-1", addr)
		out := run(client, "context", ups)
		var ctx struct {
			HookSpecificOutput struct{ HookEventName, AdditionalContext string }
		}
		if err := json.Unmarshal([]byte(out), &ctx); err != nil || ctx.HookSpecificOutput.HookEventName != "UserPromptSubmit" {
			t.Fatalf("%s context reply: %q", client, out)
		}
		ac := ctx.HookSpecificOutput.AdditionalContext
		if !strings.Contains(ac, "--- punk message "+client+"-1 from lead") ||
			!strings.Contains(ac, `send_message(namespace="ns1", sender="`+addr+`", recipient="lead", reply_to="`+client+`-1"`) {
			t.Fatalf("%s envelope missing explicit namespace/sender: %s", client, ac)
		}

		// stop_hook_active: nothing printed, nothing fetched or acked.
		add(client+"-2", addr)
		readsBefore := len(f.reads)
		if out := run(client, "continue", stopActive); out != "" {
			t.Fatalf("%s stop_hook_active printed %q", client, out)
		}
		if len(f.reads) != readsBefore {
			t.Fatalf("%s stop_hook_active fetched (leased) messages", client)
		}

		// Normal Stop: block with the rendered inbox, then acked.
		out = run(client, "continue", stop)
		var blk struct{ Decision, Reason string }
		if err := json.Unmarshal([]byte(out), &blk); err != nil || blk.Decision != "block" || !strings.Contains(blk.Reason, client+"-2") {
			t.Fatalf("%s stop reply: %q", client, out)
		}
		// Empty inbox Stop: nothing, no continuation used.
		if out := run(client, "continue", stop); out != "" {
			t.Fatalf("%s empty stop printed %q", client, out)
		}
	}
	acked := strings.Join(f.ackedIDs(), ",")
	if acked != "claude-code-1,claude-code-2,codex-1,codex-2" {
		t.Fatalf("acked %s", acked)
	}
}

// Cap hit on Stop: no false delivery (nothing printed, nothing acked,
// nothing leased) and the message reaches the next UserPromptSubmit.
func TestClaudeInboxCapHitDeliversOnNextPrompt(t *testing.T) {
	inboxTestEnv(t)
	t.Setenv("PUNK_MESSAGING_MAX_CONTINUE", "1")
	f, srv := newFakeInbox(t)
	stop := `{"session_id":"s1","cwd":"/w","hook_event_name":"Stop"}`
	m := func(id string) InboxMessage {
		return InboxMessage{ID: id, Namespace: "ns1", Sender: "lead", Recipient: "claude-code:s1", Body: id, CreatedAt: "t"}
	}
	f.add(m("a"))
	out, _ := runInbox(t, InboxOpts{Client: "claude-code", Mode: "continue", BaseURL: srv.URL, Namespace: "ns1"}, stop)
	if !strings.Contains(out, `"decision":"block"`) {
		t.Fatalf("first stop: %q", out)
	}
	f.add(m("b"))
	reads := len(f.reads)
	out, errs := runInbox(t, InboxOpts{Client: "claude-code", Mode: "continue", BaseURL: srv.URL, Namespace: "ns1"}, stop)
	if out != "" || len(f.reads) != reads || !strings.Contains(errs, "continuation cap reached") {
		t.Fatalf("cap-hit stop: out=%q reads %d->%d errs=%q", out, reads, len(f.reads), errs)
	}
	if strings.Join(f.ackedIDs(), ",") != "a" {
		t.Fatalf("cap hit acked something: %v", f.ackedIDs())
	}
	out, _ = runInbox(t, InboxOpts{Client: "claude-code", Mode: "continue", BaseURL: srv.URL, Namespace: "ns1"},
		`{"session_id":"s1","cwd":"/w","hook_event_name":"UserPromptSubmit","prompt":"hi"}`)
	if !strings.Contains(out, `"hookEventName":"UserPromptSubmit"`) || !strings.Contains(out, "--- punk message b ") {
		t.Fatalf("held message not delivered on next prompt: %q", out)
	}
	if strings.Join(f.ackedIDs(), ",") != "a,b" {
		t.Fatalf("acked %v", f.ackedIDs())
	}
}

// Render budget per client keeps the reply under the documented caps.
func TestClaudeInboxRespectsClientRenderCap(t *testing.T) {
	inboxTestEnv(t)
	f, srv := newFakeInbox(t)
	for i := 0; i < 6; i++ {
		f.add(InboxMessage{ID: fmt.Sprintf("m%d", i), Namespace: "ns1", Sender: "lead", Recipient: "claude-code:s1",
			Body: strings.Repeat("x", 3000), CreatedAt: "t"})
	}
	out, _ := runInbox(t, InboxOpts{Client: "claude-code", Mode: "context", BaseURL: srv.URL, Namespace: "ns1"},
		`{"session_id":"s1","cwd":"/w","hook_event_name":"SessionStart"}`)
	var ctx struct {
		HookSpecificOutput struct{ AdditionalContext string }
	}
	if err := json.Unmarshal([]byte(out), &ctx); err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(ctx.HookSpecificOutput.AdditionalContext)); n > 10000 {
		t.Fatalf("additionalContext %d chars exceeds Claude Code's 10,000 cap", n)
	}
	if !strings.Contains(ctx.HookSpecificOutput.AdditionalContext, "more message(s) are waiting") {
		t.Fatal("deferred messages not announced")
	}
	if n := len(f.ackedIDs()); n == 0 || n >= 6 {
		t.Fatalf("acked %d of 6", n)
	}
}

// Codex merges global and project hook scopes; an identical inbox group
// in both would run the inbox hook twice per event.
func TestDedupeCodexHookScopesRemovesIdenticalInboxGroups(t *testing.T) {
	d := t.TempDir()
	global, project := filepath.Join(d, "g.json"), filepath.Join(d, "p.json")
	for _, p := range []string{global, project} {
		if _, err := ConnectCodexHooksMessaging(p, "/usr/local/bin/punk", "http://localhost:9090", ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, changed, err := DedupeCodexHookScopes(global, project, "/usr/local/bin/punk"); err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	hooks, _ := readSettings(t, project)["hooks"].(map[string]any)
	for _, ev := range []string{"SessionStart", "UserPromptSubmit", "Stop"} {
		if len(inboxGroups(t, hooks, ev, "codex")) != 0 {
			t.Fatalf("%s: project inbox duplicate kept", ev)
		}
	}
	if len(inboxGroups(t, readSettings(t, global)["hooks"].(map[string]any), "Stop", "codex")) != 1 {
		t.Fatal("global inbox group must stay")
	}
}
