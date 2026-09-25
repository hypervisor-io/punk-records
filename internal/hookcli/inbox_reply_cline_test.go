package hookcli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestClineInboxReplyCatchupOnly(t *testing.T) {
	c, ok := lookupInboxClient("cline")
	if !ok || c.Reply == nil || c.AllowContinue || c.CanCarry == nil {
		t.Fatalf("Cline adapter overwritten/missing: %+v", c)
	}
	for _, event := range []string{"TaskStart", "UserPromptSubmit", "TaskResume", "TaskComplete", "Notification", "Stop"} {
		p := InboxPayload{Event: event}
		want := event == "TaskStart" || event == "UserPromptSubmit"
		if c.CanCarry(p, false) != want {
			t.Fatalf("CanCarry %s", event)
		}
		r := c.Reply(InboxReplyRequest{Mode: "continue", Payload: p, Delivery: Delivery{Rendered: "peer text", Continue: true}})
		var out map[string]any
		if err := json.Unmarshal(r.Out, &out); err != nil {
			t.Fatal(err)
		}
		if out["cancel"] != false || out["decision"] != nil || out["followup_message"] != nil || r.Delivered != want {
			t.Fatalf("reply=%s delivered=%v", r.Out, r.Delivered)
		}
		if want && out["contextModification"] != "peer text" {
			t.Fatal(out)
		}
	}
	var out, errw bytes.Buffer
	t.Setenv("PUNK_MESSAGING", "0")
	if err := Inbox(InboxOpts{Client: "cline", Mode: "context", BaseURL: "http://127.0.0.1:1"}, strings.NewReader(`{"hookName":"TaskStart","taskId":"t"}`), &out, &errw); err != nil {
		t.Fatal(err)
	}
	if out.String() != `{"cancel":false}`+"\n" {
		t.Fatalf("disabled reply %q", out.String())
	}
}
