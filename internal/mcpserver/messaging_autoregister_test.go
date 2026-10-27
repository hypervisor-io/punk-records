package mcpserver

import (
	"context"
	"net/http"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hypervisor-io/punk-records/internal/region"
)

// P4-B: auto-register (plain, no bind) on the first
// send_message/read_messages/await_messages/ack_messages call from an
// unknown member. The registered identity is the explicit agent/sender
// argument, else the connection-pinned identity - membership metadata
// only: NO inbox binding is ever written (binding stays join's explicit
// job, and the reworked-P4-A rule that a host-pinned identity never
// becomes an inbox address stays intact). Known members are never
// re-registered (no extra writes), and a failing registration is
// swallowed without failing the tool call.

// autoRegSession is a session on the standard test Deps with the
// messaging opt-in on (the four message tools require
// Deps.MessagingEnabled), plus the server's region store for membership
// and binding assertions.
func autoRegSession(t *testing.T) (*mcp.ClientSession, *region.Store) {
	t.Helper()
	var reg *region.Store
	cs := sessionOpts(t, nil, func(d *Deps) {
		d.MessagingEnabled = true
		reg = d.Region
	})
	return cs, reg
}

func TestSendMessageAutoRegistersUnknownSenderNoBinding(t *testing.T) {
	cs, reg := autoRegSession(t)
	ctx := context.Background()

	// The recipient must already be a member: auto-register covers the
	// CALLER side only, never the recipient.
	if err := reg.Register(ctx, "ns-ar", "known:r1", ""); err != nil {
		t.Fatal(err)
	}

	var msg struct {
		ID        string `json:"id"`
		Namespace string `json:"namespace"`
		Sender    string `json:"sender"`
		Recipient string `json:"recipient"`
	}
	// First send from an unknown sender succeeds: the tool auto-registers
	// the sender (plain) before the region's membership check runs.
	callJSON(t, cs, "send_message", map[string]any{
		"namespace": "ns-ar", "sender": "fresh:s1", "recipient": "known:r1", "body": "hi"}, &msg)
	if msg.Sender != "fresh:s1" || msg.Recipient != "known:r1" {
		t.Fatalf("message = %+v, want the fresh sender's message stored", msg)
	}
	members, err := reg.Members(ctx, "ns-ar")
	if err != nil {
		t.Fatal(err)
	}
	roles := map[string]string{}
	for _, m := range members {
		roles[m.Agent] = m.Role
	}
	if role, ok := roles["fresh:s1"]; !ok || role != "" {
		t.Fatalf("members = %v, want fresh:s1 auto-registered PLAIN (empty role)", roles)
	}
	// Auto-register is membership metadata only: never a binding.
	if ns, ok, err := reg.InboxBinding(ctx, "fresh:s1"); err != nil || ok {
		t.Fatalf("binding = (%q, %v, %v), want NO binding for an auto-registered member", ns, ok, err)
	}
}

func TestReadAckAwaitAutoRegisterUnknownAgentNoBinding(t *testing.T) {
	cs, reg := autoRegSession(t)
	ctx := context.Background()

	// read_messages from an unknown agent registers it.
	var rd struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	callJSON(t, cs, "read_messages", map[string]any{"namespace": "ns-rd", "agent": "reader:r1"}, &rd)
	if len(rd.Messages) != 0 {
		t.Fatalf("read = %+v, want an empty inbox", rd)
	}

	// ack_messages from an unknown agent registers it (acking nothing).
	var ack struct {
		Acked int64 `json:"acked"`
	}
	callJSON(t, cs, "ack_messages", map[string]any{"namespace": "ns-rd", "agent": "acker:a1", "ids": []string{"nope"}}, &ack)
	if ack.Acked != 0 {
		t.Fatalf("ack = %+v, want 0", ack)
	}

	// await_messages from an unknown agent registers it (empty timeout).
	callJSON(t, cs, "await_messages", map[string]any{"namespace": "ns-rd", "agent": "waiter:w1", "timeout_seconds": 1}, &rd)

	// The sent-box view registers the sender address it reads under.
	callJSON(t, cs, "read_messages", map[string]any{"namespace": "ns-rd", "box": "sent", "sender": "view:v1"}, &rd)

	members, err := reg.Members(ctx, "ns-rd")
	if err != nil {
		t.Fatal(err)
	}
	roles := map[string]string{}
	for _, m := range members {
		roles[m.Agent] = m.Role
	}
	for _, want := range []string{"reader:r1", "acker:a1", "waiter:w1", "view:v1"} {
		if role, ok := roles[want]; !ok || role != "" {
			t.Fatalf("members = %v, want %s auto-registered PLAIN (empty role)", roles, want)
		}
	}
	for agent := range roles {
		if ns, ok, err := reg.InboxBinding(ctx, agent); err != nil || ok {
			t.Fatalf("binding for %s = (%q, %v, %v), want NO binding", agent, ns, ok, err)
		}
	}
}

func TestAutoRegisterPinnedIdentityIsMetadataOnly(t *testing.T) {
	// The authz rig serves real HTTP, so the X-Punk-Agent header punk
	// connect bakes into MCP entries rides every call. With no explicit
	// sender, the pinned identity is the auto-registered membership
	// metadata - and the reworked-P4-A invariant holds: it NEVER gets an
	// inbox binding (that stays join's explicit, per-session decision).
	g := mcpAuthzRigNew(t, true)
	cs := g.connectOpts(t, http.Header{"X-Punk-Agent": {"codex:host-9"}, "X-Punk-Namespace": {"ns-a"}}, nil, nil)
	ctx := context.Background()

	if err := g.reg.Register(ctx, "ns-a", "peer:p1", ""); err != nil {
		t.Fatal(err)
	}
	var msg struct {
		Sender string `json:"sender"`
	}
	callJSON(t, cs, "send_message", map[string]any{"recipient": "peer:p1", "body": "hi"}, &msg)
	if msg.Sender != "codex:host-9" {
		t.Fatalf("message sender = %q, want the pinned identity", msg.Sender)
	}
	members, err := g.reg.Members(ctx, "ns-a")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range members {
		if m.Agent == "codex:host-9" {
			found = true
			if m.Role != "" {
				t.Fatalf("pinned identity registered with role %q, want plain", m.Role)
			}
		}
	}
	if !found {
		t.Fatalf("members = %+v, want the pinned identity auto-registered", members)
	}
	if ns, ok, err := g.reg.InboxBinding(ctx, "codex:host-9"); err != nil || ok {
		t.Fatalf("binding = (%q, %v, %v), want NO binding for the pinned host identity", ns, ok, err)
	}
}

func TestAutoRegisterKnownMemberWritesNothing(t *testing.T) {
	cs, reg := autoRegSession(t)
	ctx := context.Background()

	if err := reg.Register(ctx, "ns-k", "known:k1", "worker"); err != nil {
		t.Fatal(err)
	}
	before, err := reg.Members(ctx, "ns-k")
	if err != nil || len(before) != 1 {
		t.Fatalf("members = %+v (%v), want the one known member", before, err)
	}

	var rd struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	callJSON(t, cs, "read_messages", map[string]any{"namespace": "ns-k", "agent": "known:k1"}, &rd)

	// A duplicate Register would rewrite the role to "" and re-stamp
	// joined_at (the test clock advances on every call, so both are
	// observable); both surviving proves no re-registration ran. The
	// last_seen heartbeat (Touch) is the pre-existing ride-along and may
	// legitimately move.
	after, err := reg.Members(ctx, "ns-k")
	if err != nil || len(after) != 1 {
		t.Fatalf("members = %+v (%v), want still exactly one member", after, err)
	}
	if after[0].Role != "worker" || after[0].JoinedAt != before[0].JoinedAt {
		t.Fatalf("member after = %+v, want role and joined_at untouched (%+v)", after[0], before[0])
	}
}

func TestAutoRegisterFailureNeverFailsTool(t *testing.T) {
	cs, reg := autoRegSession(t)
	ctx := context.Background()

	// Drop the regions table so Register's Ensure step fails while the
	// membership check (region_members) and the whole message read path
	// (agent_messages) stay healthy: the auto-register failure must be
	// swallowed and the tool call must still succeed.
	if _, err := reg.DB().ExecContext(ctx, `DROP TABLE regions`); err != nil {
		t.Fatal(err)
	}
	var rd struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	callJSON(t, cs, "read_messages", map[string]any{"namespace": "ns-f", "agent": "ghost:g1"}, &rd)
	if len(rd.Messages) != 0 {
		t.Fatalf("read = %+v, want an empty inbox despite the failed registration", rd)
	}
	members, err := reg.Members(ctx, "ns-f")
	if err != nil || len(members) != 0 {
		t.Fatalf("members = %+v (%v), want none (the registration failed)", members, err)
	}
}

func TestAnonymousFallbackIdentityNotAutoRegistered(t *testing.T) {
	// No explicit agent and no pinned identity: the tool falls back to
	// the shared anonymous "mcp" name. Like the touch heartbeat, the
	// auto-register skips it - a shared placeholder never becomes a
	// member; the model must pass an explicit address or join.
	cs, reg := autoRegSession(t)
	ctx := context.Background()

	var rd struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	callJSON(t, cs, "read_messages", map[string]any{"namespace": "ns-an"}, &rd)
	members, err := reg.Members(ctx, "ns-an")
	if err != nil || len(members) != 0 {
		t.Fatalf("members = %+v (%v), want the anonymous fallback NOT registered", members, err)
	}
}
