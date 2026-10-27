package hookcli

// P1-A: every context-capable client gets the [PUNK ROUTING] identity
// block on an empty inbox, on exactly the events whose reply can carry
// context, and on no other event. Wake wording stays with the clients
// that have a native wake transport (claude-code, codex); the rest are
// told nothing about wake. Each empty-inbox reply must stay valid JSON
// in the client's own envelope, and guidance is never peer delivery: it
// ACKs nothing and is never stacked onto a delivered envelope.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestInboxRoutingGuidanceEveryContextClient(t *testing.T) {
	type check struct {
		stdin string
		event string // --event, for antigravity (its payload does not name one)
		mode  string
		addr  string // expected address when a routing block is wanted
		want  bool   // routing block expected on an empty inbox
	}
	for _, tc := range []struct {
		client string
		wake   bool
		checks []check
	}{
		{
			client: "claude-code",
			wake:   true,
			checks: []check{
				{`{"session_id":"s1","cwd":"/w","hook_event_name":"SessionStart"}`, "", "context", "claude-code:s1", true},
				{`{"session_id":"s1","cwd":"/w","hook_event_name":"UserPromptSubmit"}`, "", "context", "claude-code:s1", true},
				{`{"session_id":"s1","cwd":"/w","hook_event_name":"Stop"}`, "", "continue", "", false},
			},
		},
		{
			client: "codex",
			wake:   true,
			checks: []check{
				{`{"session_id":"s1","cwd":"/w","hook_event_name":"SessionStart"}`, "", "context", "codex:s1", true},
				{`{"session_id":"s1","cwd":"/w","hook_event_name":"UserPromptSubmit"}`, "", "context", "codex:s1", true},
				{`{"session_id":"s1","cwd":"/w","hook_event_name":"Stop"}`, "", "continue", "", false},
			},
		},
		{
			client: "hermes",
			checks: []check{
				{hermesPreLLMCallPayload, "", "context", "hermes:h1", true},
				{`{"hook_event_name":"post_llm_call","session_id":"h1","cwd":"/w"}`, "", "context", "", false},
			},
		},
		{
			client: "cursor",
			checks: []check{
				{cursorSessionStartPayload, "", "context", "cursor:c1", true},
				{cursorStopPayload, "", "continue", "", false},
				{`{"conversation_id":"c1","hook_event_name":"beforeSubmitPrompt","workspace_roots":["/w"],"prompt":"hi"}`, "", "context", "", false},
			},
		},
		{
			client: "copilot",
			checks: []check{
				{copilotSessionStartPayload, "", "context", "copilot:s1", true},
				{copilotStopPayload, "", "continue", "", false},
				{`{"hook_event_name":"UserPromptSubmit","session_id":"s1","cwd":"/w","prompt":"hi"}`, "", "context", "", false},
			},
		},
		{
			client: "antigravity",
			checks: []check{
				{antigravityPreInvocationPayload, "PreInvocation", "context", "antigravity:a1", true},
				{antigravityStopPayload, "Stop", "continue", "", false},
			},
		},
		{
			client: "cline",
			checks: []check{
				{`{"hookName":"TaskStart","taskId":"t1","workspaceRoots":["/w"]}`, "", "context", "cline:t1", true},
				{`{"hookName":"UserPromptSubmit","taskId":"t1","workspaceRoots":["/w"]}`, "", "context", "cline:t1", true},
				{`{"hookName":"TaskComplete","taskId":"t1","workspaceRoots":["/w"]}`, "", "context", "", false},
			},
		},
	} {
		t.Run(tc.client, func(t *testing.T) {
			inboxTestEnv(t)
			f, srv := newFakeInbox(t)
			run := func(c check) string {
				out, _ := runInbox(t, InboxOpts{Client: tc.client, Mode: c.mode, Event: c.event,
					BaseURL: srv.URL, Namespace: "ns1"}, c.stdin)
				return out
			}
			for _, c := range tc.checks {
				out := run(c)
				if got := strings.Contains(out, "[PUNK ROUTING]"); got != c.want {
					t.Fatalf("event=%q mode=%q routing=%v want=%v out=%q", c.event, c.mode, got, c.want, out)
				}
				if out != "" && !json.Valid([]byte(out)) {
					t.Fatalf("event=%q empty-inbox reply is not valid JSON: %q", c.event, out)
				}
				if !c.want {
					continue
				}
				// The routing block rides inside the client's own envelope,
				// so decode the JSON and assert on the carried text.
				text := jsonText(out)
				for _, want := range []string{
					"[PUNK ROUTING] Your punk messaging address is " + c.addr + " in namespace ns1",
					`send_message(namespace="ns1", sender="` + c.addr + `"`,
					"never invent aliases or namespaces",
				} {
					if !strings.Contains(text, want) {
						t.Fatalf("event=%q routing missing %q: %q", c.event, want, out)
					}
				}
				// Wake wording exists only where a wake transport does.
				if got := strings.Contains(text, "idle wake"); got != tc.wake {
					t.Fatalf("event=%q wake wording=%v want=%v out=%q", c.event, got, tc.wake, out)
				}
			}
			if ids := f.ackedIDs(); len(ids) != 0 {
				t.Fatalf("routing guidance is not peer delivery; acked=%v", ids)
			}
			// A delivered envelope already names address and namespace: the
			// routing block is never stacked onto one.
			first := tc.checks[0]
			f.add(InboxMessage{ID: "m1", Namespace: "ns1", Sender: "lead", Recipient: first.addr, Body: "hi", CreatedAt: "t"})
			out := run(first)
			if strings.Contains(out, "[PUNK ROUTING]") {
				t.Fatalf("routing stacked onto a delivered envelope: %q", out)
			}
			if !strings.Contains(out, "--- punk message m1 from lead") {
				t.Fatalf("delivered envelope missing: %q", out)
			}
			if ids := f.ackedIDs(); len(ids) != 1 || ids[0] != "m1" {
				t.Fatalf("delivery acked=%v", ids)
			}
		})
	}
}

// jsonText decodes one JSON reply and concatenates every string it
// carries, so an assertion on the routing text does not have to know the
// client's envelope shape.
func jsonText(out string) string {
	var v any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return ""
	}
	var b strings.Builder
	var walk func(any)
	walk = func(x any) {
		switch x := x.(type) {
		case string:
			b.WriteString(x)
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(v)
	return b.String()
}

// The routing block is the longest form (identity + wake line); it must
// fit every client's render budget so an empty-inbox reply never exceeds
// the per-client cap.
func TestInboxRoutingGuidanceFitsClientBudgets(t *testing.T) {
	for _, name := range []string{"claude-code", "codex", "cursor", "copilot", "antigravity", "hermes", "cline"} {
		c, ok := lookupInboxClient(name)
		if !ok {
			t.Fatalf("%s: not registered", name)
		}
		g := inboxRoutingGuidance("ns1", name+":s1", false, false)
		if total := inboxBudget(c).Total; total > 0 && len(g) > total {
			t.Fatalf("%s: routing block %d bytes exceeds the client render budget %d", name, len(g), total)
		}
	}
}
