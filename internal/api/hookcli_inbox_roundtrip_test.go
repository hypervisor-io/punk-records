package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hypervisor-io/punk-records/internal/hookcli"
)

// The M5 inbox hook against the real M3/M10 messaging handlers (not a
// fake): self-registration, leased read, ACK scoped to the invocation's
// lease owner, and full-body recovery of a truncated, ACKed message by id.
func TestHookInboxRoundTripsThroughRealServer(t *testing.T) {
	g := messagingServer(t)
	httpSrv := httptest.NewServer(g.s.Router())
	defer httpSrv.Close()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("PUNK_MESSAGING", "1")
	t.Setenv("PUNK_MESSAGING_FROM", "")
	t.Setenv("PUNK_MESSAGING_RENDER_BYTES", "")
	t.Setenv("PUNK_NAMESPACE", "")

	hookcli.RegisterInboxClient(hookcli.InboxClient{Name: "rt-inbox", Reply: func(req hookcli.InboxReplyRequest) hookcli.InboxReply {
		if req.Delivery.Rendered == "" {
			return hookcli.InboxReply{}
		}
		return hookcli.InboxReply{Out: []byte(req.Delivery.Rendered + "\n"), Delivered: true}
	}})
	const ns = "rt-inbox-ns"
	const addr = "rt-inbox:sess-1"
	stdin := `{"session_id":"sess-1","cwd":"/work/rt","hook_event_name":"UserPromptSubmit"}`
	run := func() string {
		var out, errw bytes.Buffer
		if err := hookcli.Inbox(hookcli.InboxOpts{Client: "rt-inbox", Mode: "context", BaseURL: httpSrv.URL, Namespace: ns},
			strings.NewReader(stdin), &out, &errw); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}

	// First call: registers the session address, empty inbox, no output.
	if out := run(); out != "" {
		t.Fatalf("empty inbox printed %q", out)
	}
	rec := g.do(t, http.MethodGet, "/v1/namespaces/"+ns+"/members", "")
	if !strings.Contains(rec.Body.String(), `"agent":"`+addr+`"`) {
		t.Fatalf("hook did not self-register: %s", rec.Body.String())
	}

	g.register(t, ns, "lead")
	long := strings.Repeat("x", 9000) // over the 8 KiB per-message budget
	rec = g.do(t, http.MethodPost, "/v1/namespaces/"+ns+"/messages",
		`{"sender":"lead","recipient":"`+addr+`","body":"`+long+`","task_id":"T9"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("send: %d %s", rec.Code, rec.Body.String())
	}
	var sent struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &sent)

	out := run()
	if !strings.Contains(out, "--- punk message "+sent.ID+" from lead at ") || !strings.Contains(out, "task=T9") ||
		!strings.Contains(out, "[truncated 808 bytes; full text: read_messages(namespace=\""+ns+"\", agent=\""+addr+"\", id=\""+sent.ID+"\")]") ||
		!strings.Contains(out, `send_message(namespace="`+ns+`", sender="`+addr+`", recipient="lead", reply_to="`+sent.ID+`"`) {
		t.Fatalf("delivery envelope:\n%s", out)
	}

	// ACKed through the lease owner: unread is empty, a rerun prints nothing.
	rec = g.do(t, http.MethodGet, "/v1/namespaces/"+ns+"/messages/count?agent="+addr, "")
	if strings.TrimSpace(rec.Body.String()) != `{"unread":0}` {
		t.Fatalf("message not acked: %s", rec.Body.String())
	}
	if out := run(); out != "" {
		t.Fatalf("acked message redelivered: %q", out)
	}

	// The truncated body is recoverable by id after ACK.
	rec = g.do(t, http.MethodGet, "/v1/namespaces/"+ns+"/messages?agent="+addr+"&id="+sent.ID, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), long) {
		t.Fatalf("full-body recovery by id: %d %.200s", rec.Code, rec.Body.String())
	}

	// Server-side registration happened once per state file.
	for i := 0; i < 2; i++ {
		run()
	}
	rec = g.do(t, http.MethodGet, "/v1/namespaces/"+ns+"/members", "")
	if regs := strings.Count(rec.Body.String(), `"agent":"`+addr+`"`); regs != 1 {
		t.Fatalf("member rows %d", regs)
	}

	// A sender outside PUNK_MESSAGING_FROM is never printed and never
	// acked on the real server (the unsupported release route is
	// tolerated silently; the lease simply expires).
	g.register(t, ns, "stranger")
	rec = g.do(t, http.MethodPost, "/v1/namespaces/"+ns+"/messages", `{"sender":"stranger","recipient":"`+addr+`","body":"nope"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("send: %d", rec.Code)
	}
	t.Setenv("PUNK_MESSAGING_FROM", "lead")
	var outb, errw bytes.Buffer
	_ = hookcli.Inbox(hookcli.InboxOpts{Client: "rt-inbox", Mode: "context", BaseURL: httpSrv.URL, Namespace: ns},
		strings.NewReader(stdin), &outb, &errw)
	if outb.String() != "" || !strings.Contains(errw.String(), "held back 1 message(s)") || strings.Contains(errw.String(), "release") {
		t.Fatalf("held-back: out=%q err=%q", outb.String(), errw.String())
	}
	rec = g.do(t, http.MethodGet, "/v1/namespaces/"+ns+"/messages/count?agent="+addr, "")
	if strings.TrimSpace(rec.Body.String()) != `{"unread":1}` {
		t.Fatalf("held-back message was acked: %s", rec.Body.String())
	}
}
