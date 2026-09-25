package mcpserver

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/hypervisor-io/punk-records/internal/authz"
	"github.com/hypervisor-io/punk-records/internal/region"
)

func TestMessageDeliveryMCPLeaseSentCountRecovery(t *testing.T) {
	cs := messagingSession(t)
	var decoy, target region.Message
	for i, ns := range []string{"other", "ns"} {
		registerMessageMembers(t, cs, ns, "one", "two")
		dst := &decoy
		if i == 1 {
			dst = &target
		}
		callJSON(t, cs, "send_message", map[string]any{"namespace": ns, "sender": "one", "recipient": "two", "body": "full body"}, dst)
	}
	var out struct {
		Messages []region.Message
		Unread   int
	}
	lease := map[string]any{"namespace": "ns", "agent": "two", "lease_seconds": 60, "leased_by": "hook-1"}
	callJSON(t, cs, "read_messages", lease, &out)
	if len(out.Messages) != 1 || out.Messages[0].ID != target.ID || out.Messages[0].LeasedBy != "hook-1" {
		t.Fatalf("lease = %+v", out)
	}
	lease["leased_by"] = "hook-2"
	callJSON(t, cs, "read_messages", lease, &out)
	if len(out.Messages) != 0 {
		t.Fatalf("second delivery = %+v", out)
	}
	callJSON(t, cs, "read_messages", map[string]any{"namespace": "ns", "agent": "two", "count_only": true}, &out)
	if out.Unread != 1 {
		t.Fatalf("count leased = %+v", out)
	}
	for _, tc := range []struct {
		owner string
		n     int
	}{{"hook-2", 0}, {"hook-1", 1}} {
		var ack struct{ Acked int }
		callJSON(t, cs, "ack_messages", map[string]any{"namespace": "ns", "agent": "two", "ids": []string{target.ID, decoy.ID}, "leased_by": tc.owner}, &ack)
		if ack.Acked != tc.n {
			t.Fatalf("ack = %+v", ack)
		}
	}
	callJSON(t, cs, "read_messages", map[string]any{"namespace": "ns", "agent": "two", "id": target.ID}, &out)
	if len(out.Messages) != 1 || out.Messages[0].Body != "full body" || out.Messages[0].AckedAt == "" || out.Messages[0].LeasedBy != "" {
		t.Fatalf("recovery = %+v", out)
	}
	callJSON(t, cs, "read_messages", map[string]any{"namespace": "ns", "box": "sent", "sender": "one"}, &out)
	if len(out.Messages) != 1 || out.Messages[0].ID != target.ID || out.Messages[0].AckedAt == "" {
		t.Fatalf("sent = %+v", out)
	}
	callJSON(t, cs, "read_messages", map[string]any{"namespace": "other", "agent": "two", "id": target.ID}, &out)
	if len(out.Messages) != 0 {
		t.Fatalf("foreign recovery = %+v", out)
	}
	callJSON(t, cs, "read_messages", map[string]any{"namespace": "other", "agent": "two"}, &out)
	if len(out.Messages) != 1 || out.Messages[0].ID != decoy.ID {
		t.Fatalf("decoy = %+v", out)
	}
	callJSON(t, cs, "read_messages", map[string]any{"namespace": "ns", "agent": "two", "count_only": true}, &out)
	if out.Unread != 0 {
		t.Fatalf("count after ack = %+v", out)
	}
}

func TestMessageDeliveryMCPBacklog(t *testing.T) {
	cs := messagingSession(t, func(d *Deps) { d.Region.MaxUnreadPerRecipient = 1 })
	for _, ns := range []string{"other", "ns"} {
		registerMessageMembers(t, cs, ns, "one", "two")
		callJSON(t, cs, "send_message", map[string]any{"namespace": ns, "sender": "one", "recipient": "two", "body": "body", "idempotency_key": "key"}, nil)
	}
	args := map[string]any{"namespace": "ns", "sender": "one", "recipient": "two", "body": "new"}
	if out, _, failed := call(t, cs, "send_message", args); !failed || !strings.Contains(out, "backlog") {
		t.Fatalf("backlog error = %q, failed=%v", out, failed)
	}
	args["body"], args["idempotency_key"] = "body", "key"
	callJSON(t, cs, "send_message", args, nil)
}

func TestMessageDeliveryMCPModesRequireGrants(t *testing.T) {
	g := mcpAuthzRigNew(t, true)
	cs := g.connectOpts(t, http.Header{"X-Punk-Agent": {"one"}, "X-Punk-Namespace": {"ns-a"}}, nil, nil)
	// Seed forbidden namespace first using direct storage, not unauthorized tool.
	for _, ns := range []string{"ns-b", "ns-a"} {
		for _, a := range []string{"one", "two"} {
			if err := g.reg.Register(context.Background(), ns, a, ""); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := g.reg.SendMessage(context.Background(), region.MessageInput{Namespace: ns, Sender: "one", Recipient: "two", Body: ns}); err != nil {
			t.Fatal(err)
		}
	}
	for _, extra := range []map[string]any{{"id": "id"}, {"box": "sent", "sender": "one"}, {"count_only": true}, {"lease_seconds": 60, "leased_by": "h"}} {
		extra["namespace"], extra["agent"] = "ns-b", "two"
		if out, denied, _ := call(t, cs, "read_messages", extra); !denied {
			t.Fatalf("no grant accepted: %s", out)
		}
	}
	if err := g.az.Revoke(context.Background(), "alice", "ns-a", authz.OpWrite); err != nil {
		t.Fatal(err)
	}
	if out, denied, _ := call(t, cs, "read_messages", map[string]any{"lease_seconds": 60, "leased_by": "h", "agent": "two"}); !denied {
		t.Fatalf("read-only lease = %s", out)
	}
	var out struct{ Messages []region.Message }
	callJSON(t, cs, "read_messages", map[string]any{"box": "sent"}, &out)
	if len(out.Messages) != 1 || out.Messages[0].Sender != "one" {
		t.Fatalf("default sender sent = %+v", out)
	}
}
