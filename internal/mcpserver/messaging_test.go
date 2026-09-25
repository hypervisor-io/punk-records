package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/api"
	"github.com/hypervisor-io/punk-records/internal/authz"
	"github.com/hypervisor-io/punk-records/internal/bus"
	"github.com/hypervisor-io/punk-records/internal/region"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Decode the public wire shape rather than relying on the storage types.
type messageWire struct {
	ID, Namespace, Sender, Recipient, Body string
	TaskID                                 string `json:"task_id"`
	ReplyTo                                string `json:"reply_to"`
	CreatedAt                              string `json:"created_at"`
}

type inboxWire struct {
	Messages []messageWire `json:"messages"`
}

// Existing messaging behavior tests explicitly opt in. General session helpers
// keep the real default (disabled), including default schema/budget tests.
func messagingSession(t *testing.T, tweaks ...func(*Deps)) *mcp.ClientSession {
	t.Helper()
	opts := []func(*Deps){func(d *Deps) { d.MessagingEnabled = true }}
	return sessionOpts(t, nil, append(opts, tweaks...)...)
}

func registerMessageMembers(t *testing.T, cs *mcp.ClientSession, ns string, agents ...string) {
	t.Helper()
	for _, agent := range agents {
		callJSON(t, cs, "register", map[string]any{"namespace": ns, "agent": agent}, nil)
	}
}

func TestMessagingRoundTripAndIsolation(t *testing.T) {
	cs := messagingSession(t, func(d *Deps) { d.Toolset = "agent" })
	registerMessageMembers(t, cs, "ns", "one", "two")
	registerMessageMembers(t, cs, "other", "one", "two")
	var members struct{ Members []struct{ Agent string } }
	callJSON(t, cs, "list_region_members", map[string]any{"namespace": "ns"}, &members)
	if len(members.Members) != 2 || members.Members[0].Agent != "one" || members.Members[1].Agent != "two" {
		t.Fatalf("lean member discovery = %+v", members)
	}
	args := map[string]any{"namespace": "ns", "sender": "one", "recipient": "two", "body": "review task", "task_id": "M2", "idempotency_key": "first"}
	var sent, retry messageWire
	callJSON(t, cs, "send_message", args, &sent)
	callJSON(t, cs, "send_message", args, &retry)
	if sent.ID == "" || retry.ID != sent.ID || sent.Sender != "one" || sent.Recipient != "two" || sent.Namespace != "ns" || sent.TaskID != "M2" || sent.CreatedAt == "" {
		t.Fatalf("send/retry = %+v / %+v", sent, retry)
	}
	for _, tc := range []struct {
		ns, agent string
		want      int
	}{{"ns", "two", 1}, {"ns", "one", 0}, {"other", "two", 0}, {"ns", "two", 1}} {
		var inbox inboxWire
		callJSON(t, cs, "read_messages", map[string]any{"namespace": tc.ns, "agent": tc.agent}, &inbox)
		if inbox.Messages == nil || len(inbox.Messages) != tc.want {
			t.Fatalf("%s/%s inbox = %+v; want %d (reads never ACK)", tc.ns, tc.agent, inbox, tc.want)
		}
	}
	var reply messageWire
	callJSON(t, cs, "send_message", map[string]any{"namespace": "ns", "sender": "two", "recipient": "one", "body": "reviewed", "reply_to": sent.ID}, &reply)
	if reply.ReplyTo != sent.ID {
		t.Fatalf("reply = %+v", reply)
	}
	for _, tc := range []struct {
		ns, agent string
		want      int
	}{{"other", "two", 0}, {"ns", "one", 0}, {"ns", "two", 1}, {"ns", "two", 0}} {
		var ack struct{ Acked int }
		callJSON(t, cs, "ack_messages", map[string]any{"namespace": tc.ns, "agent": tc.agent, "ids": []string{sent.ID}}, &ack)
		if ack.Acked != tc.want {
			t.Fatalf("%s/%s ACK = %d, want %d", tc.ns, tc.agent, ack.Acked, tc.want)
		}
	}
	var inbox inboxWire
	callJSON(t, cs, "read_messages", map[string]any{"namespace": "ns", "agent": "two"}, &inbox)
	if len(inbox.Messages) != 0 {
		t.Fatalf("ACKed message still unread: %+v", inbox)
	}
	callJSON(t, cs, "read_messages", map[string]any{"namespace": "ns", "agent": "one"}, &inbox)
	if len(inbox.Messages) != 1 || inbox.Messages[0].ID != reply.ID {
		t.Fatalf("ACK must preserve unspecified reply: %+v", inbox)
	}
}

func TestMessagingDefaultsAndDistinctSessionAddresses(t *testing.T) {
	g := mcpAuthzRigNew(t, true)
	one := g.connectOpts(t, http.Header{"X-Punk-Agent": {"opencode:one"}, "X-Punk-Namespace": {"ns-a"}}, nil, nil)
	two := g.connectOpts(t, http.Header{"X-Punk-Agent": {"opencode:two"}, "X-Punk-Namespace": {"ns-a"}}, nil, nil)
	callJSON(t, one, "register", map[string]any{}, nil)
	callJSON(t, two, "register", map[string]any{}, nil)
	var sent messageWire
	callJSON(t, one, "send_message", map[string]any{"recipient": "opencode:two", "body": "hello"}, &sent)
	if sent.Sender != "opencode:one" || sent.Namespace != "ns-a" {
		t.Fatalf("resolved send = %+v", sent)
	}
	var inbox inboxWire
	callJSON(t, one, "read_messages", map[string]any{}, &inbox)
	if len(inbox.Messages) != 0 {
		t.Fatalf("same-machine sender received other's message: %+v", inbox)
	}
	callJSON(t, two, "await_messages", map[string]any{"timeout_seconds": 1}, &inbox)
	if len(inbox.Messages) != 1 || inbox.Messages[0].ID != sent.ID {
		t.Fatalf("default await inbox = %+v", inbox)
	}
	callJSON(t, two, "ack_messages", map[string]any{"ids": []string{sent.ID}}, nil)
	callJSON(t, two, "read_messages", map[string]any{}, &inbox)
	if len(inbox.Messages) != 0 {
		t.Fatalf("default ACK failed: %+v", inbox)
	}
}

func TestMessagingAbsentWithoutRegion(t *testing.T) {
	cs := messagingSession(t, func(d *Deps) { d.Region = nil })
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		switch tool.Name {
		case "send_message", "read_messages", "ack_messages", "await_messages", "list_region_members":
			t.Fatalf("%s exposed without region storage", tool.Name)
		}
	}
}

func TestMessagingLimitsPreserveUnread(t *testing.T) {
	cs := messagingSession(t)
	registerMessageMembers(t, cs, "ns", "one", "two")
	var first messageWire
	callJSON(t, cs, "send_message", map[string]any{"namespace": "ns", "sender": "one", "recipient": "two", "body": "first"}, &first)
	callJSON(t, cs, "send_message", map[string]any{"namespace": "ns", "sender": "one", "recipient": "two", "body": "second"}, nil)
	for _, tool := range []string{"read_messages", "await_messages"} {
		var inbox inboxWire
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: map[string]any{"namespace": "ns", "agent": "two", "limit": 1}})
		cancel()
		if err != nil || res.IsError {
			t.Fatalf("%s with existing unread: %v %s", tool, err, text(t, res))
		}
		if err := json.Unmarshal([]byte(text(t, res)), &inbox); err != nil {
			t.Fatal(err)
		}
		if len(inbox.Messages) != 1 || inbox.Messages[0].ID != first.ID {
			t.Fatalf("%s did not return oldest limited batch: %+v", tool, inbox)
		}
	}
	var inbox inboxWire
	callJSON(t, cs, "read_messages", map[string]any{"namespace": "ns", "agent": "two"}, &inbox)
	if len(inbox.Messages) != 2 {
		t.Fatalf("limited reads/await must not ACK or skip messages: %+v", inbox)
	}
}

func TestMessagingPublishesOnlyDurableHints(t *testing.T) {
	b := bus.New()
	cs := messagingSession(t, func(d *Deps) { d.Bus = b })
	registerMessageMembers(t, cs, "ns", "one", "two")
	events, cancel := b.Subscribe()
	defer cancel()
	var sent messageWire
	callJSON(t, cs, "send_message", map[string]any{"namespace": "ns", "sender": "one", "recipient": "two", "body": "private"}, &sent)
	select {
	case e := <-events:
		if e.Kind != region.MessageEventKind || e.Key != region.MessageEventKey("ns", "two") || len(e.Data) != 1 || e.Data["id"] != sent.ID {
			t.Fatalf("hint must match region.MessageEvent, with ID but no body: %+v", e)
		}
		var inbox inboxWire
		callJSON(t, cs, "read_messages", map[string]any{"namespace": "ns", "agent": "two"}, &inbox)
		if len(inbox.Messages) != 1 || inbox.Messages[0].ID != sent.ID {
			t.Fatalf("hint preceded durable send: %+v", inbox)
		}
	case <-time.After(time.Second):
		t.Fatal("no send hint")
	}
	_, _, failed := call(t, cs, "send_message", map[string]any{"namespace": "ns", "sender": "one", "recipient": "unregistered", "body": "no"})
	if !failed {
		t.Fatal("unregistered recipient accepted")
	}
	select {
	case e := <-events:
		t.Fatalf("failed send published hint: %+v", e)
	default:
	}
}

func TestMessagingValidation(t *testing.T) {
	cs := messagingSession(t)
	registerMessageMembers(t, cs, "ns", "one", "two")
	base := map[string]any{"namespace": "ns", "sender": "one", "recipient": "two", "body": "ok", "idempotency_key": "retry"}
	callJSON(t, cs, "send_message", base, nil)
	for _, tc := range []struct {
		name, field string
		value       any
	}{
		{"empty body", "body", ""}, {"large body", "body", strings.Repeat("x", 16*1024+1)},
		{"long identifier", "task_id", strings.Repeat("x", 257)}, {"unknown sender", "sender", "unknown"},
		{"unknown reply", "reply_to", "missing"}, {"conflicting retry", "body", "different"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]any{}
			for k, v := range base {
				args[k] = v
			}
			if tc.name != "conflicting retry" {
				delete(args, "idempotency_key")
			}
			args[tc.field] = tc.value
			if out, _, failed := call(t, cs, "send_message", args); !failed {
				t.Fatalf("accepted invalid send: %s", out)
			}
		})
	}
	ids := make([]string, 101)
	for i := range ids {
		ids[i] = "id"
	}
	if out, _, failed := call(t, cs, "ack_messages", map[string]any{"namespace": "ns", "agent": "two", "ids": ids}); !failed {
		t.Fatalf("accepted oversized ACK: %s", out)
	}
}

func TestMessagingReadOnlyGrantCannotMutate(t *testing.T) {
	g := mcpAuthzRigNew(t, true)
	cs := g.connect(t)
	registerMessageMembers(t, cs, "ns-a", "one", "two")
	var sent messageWire
	callJSON(t, cs, "send_message", map[string]any{"namespace": "ns-a", "sender": "one", "recipient": "two", "body": "pending"}, &sent)
	if err := g.az.Revoke(context.Background(), "alice", "ns-a", authz.OpWrite); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string]map[string]any{
		"send_message": {"namespace": "ns-a", "sender": "one", "recipient": "two", "body": "denied"},
		"ack_messages": {"namespace": "ns-a", "agent": "two", "ids": []string{sent.ID}},
	} {
		if out, denied, _ := call(t, cs, name, args); !denied {
			t.Fatalf("%s not write-gated: %s", name, out)
		}
	}
	var inbox inboxWire
	callJSON(t, cs, "read_messages", map[string]any{"namespace": "ns-a", "agent": "two"}, &inbox)
	if len(inbox.Messages) != 1 {
		t.Fatalf("denied writes changed inbox: %+v", inbox)
	}
}

func TestMessagingNamespaceSelectorsCannotGrantAccess(t *testing.T) {
	g := mcpAuthzRigNew(t, true)
	ctx := context.Background()
	for _, agent := range []string{"one", "two"} {
		if err := g.reg.Register(ctx, "ns-b", agent, "worker"); err != nil {
			t.Fatal(err)
		}
	}
	sent, err := g.reg.SendMessage(ctx, region.MessageInput{Namespace: "ns-b", Sender: "one", Recipient: "two", Body: "private-ns-b"})
	if err != nil {
		t.Fatal(err)
	}
	for _, selector := range []struct {
		name      string
		namespace string
		headers   http.Header
		roots     []string
	}{
		{"explicit overrides granted header", "ns-b", http.Header{"X-Punk-Namespace": {"ns-a"}}, nil},
		{"header", "", http.Header{"X-Punk-Namespace": {"ns-b"}}, nil},
		// AgentNamespace("/work/b") is agent-b, also outside alice's grant.
		{"roots", "", nil, []string{"file:///work/b"}},
	} {
		t.Run(selector.name, func(t *testing.T) {
			cs := g.connectOpts(t, selector.headers, selector.roots, nil)
			for tool, args := range map[string]map[string]any{
				"send_message":        {"sender": "one", "recipient": "two", "body": "unauthorized-send"},
				"read_messages":       {"agent": "two"},
				"await_messages":      {"agent": "two", "timeout_seconds": 1},
				"ack_messages":        {"agent": "two", "ids": []string{sent.ID}},
				"list_region_members": {},
			} {
				if selector.namespace != "" {
					args["namespace"] = selector.namespace
				}
				out, denied, _ := call(t, cs, tool, args)
				if !denied || strings.Contains(out, sent.Body) {
					t.Fatalf("%s bypassed namespace grant: %s", tool, out)
				}
			}
		})
	}
	inbox, err := g.reg.ReadMessages(ctx, "ns-b", "two", 100)
	if err != nil || len(inbox) != 1 || inbox[0].ID != sent.ID {
		t.Fatalf("denied sends/ACKs changed foreign inbox: %+v %v", inbox, err)
	}
}

func TestAwaitMessagesWakesAndTimesOut(t *testing.T) {
	cs := messagingSession(t, func(d *Deps) { d.Bus = bus.New() })
	registerMessageMembers(t, cs, "ns", "one", "two")
	var inbox inboxWire
	callJSON(t, cs, "await_messages", map[string]any{"namespace": "ns", "agent": "two", "timeout_seconds": 1}, &inbox)
	if inbox.Messages == nil || len(inbox.Messages) != 0 {
		t.Fatalf("quiet wait = %+v", inbox)
	}
	type result struct {
		res *mcp.CallToolResult
		err error
	}
	done := make(chan result, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "await_messages", Arguments: map[string]any{"namespace": "ns", "agent": "two", "timeout_seconds": 10}})
		done <- result{r, err}
	}()
	time.Sleep(100 * time.Millisecond)
	var sent messageWire
	callJSON(t, cs, "send_message", map[string]any{"namespace": "ns", "sender": "one", "recipient": "two", "body": "wake"}, &sent)
	select {
	case got := <-done:
		if got.err != nil || got.res.IsError {
			t.Fatalf("wait failed: %v %s", got.err, text(t, got.res))
		}
		if err := json.Unmarshal([]byte(text(t, got.res)), &inbox); err != nil {
			t.Fatal(err)
		}
		if len(inbox.Messages) != 1 || inbox.Messages[0].ID != sent.ID {
			t.Fatalf("wake result = %+v", inbox)
		}
	case <-ctx.Done():
		t.Fatal("wait failed to wake")
	}
}

// Observe the real HTTP gate's decision, not just receipt of the request.
// This proves revocation happens AFTER initial authorization succeeded.
type messageGateObserver struct {
	NamespaceGate
	decisions chan bool
}

func (g messageGateObserver) Allow(ctx context.Context, ns string, op authz.Op) bool {
	allowed := g.NamespaceGate.Allow(ctx, ns, op)
	g.decisions <- allowed
	return allowed
}

func TestAwaitMessagesRechecksRevocation(t *testing.T) {
	for _, credential := range []bool{false, true} {
		name := "grant"
		if credential {
			name = "credential"
		}
		t.Run(name, func(t *testing.T) {
			decisions := make(chan bool, 2)
			g := mcpAuthzRigNew(t, true, func(s *mcp.Server) {
				s.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
					return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
						if call, ok := req.(*mcp.CallToolRequest); ok && call.Params.Name == "await_messages" {
							ctx = api.ContextWithNamespaceGate(ctx, messageGateObserver{namespaceGateFrom(ctx), decisions})
						}
						return next(ctx, method, req)
					}
				})
			})
			cs := g.connect(t)
			registerMessageMembers(t, cs, "ns-a", "one", "two")
			type result struct {
				res *mcp.CallToolResult
				err error
			}
			done := make(chan result, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			go func() {
				r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "await_messages", Arguments: map[string]any{"namespace": "ns-a", "agent": "two", "timeout_seconds": 10}})
				done <- result{r, err}
			}()
			select {
			case allowed := <-decisions:
				if !allowed {
					t.Fatal("initial await authorization failed before revocation")
				}
			case <-ctx.Done():
				t.Fatal("await never reached initial authorization")
			}
			if credential {
				if err := g.keys.Revoke(context.Background(), "alice-key"); err != nil {
					t.Fatal(err)
				}
			} else if err := g.az.Revoke(context.Background(), "alice", "ns-a", authz.OpRead); err != nil {
				t.Fatal(err)
			}
			// Deliver durably while the original call is blocked. Its captured
			// gate must deny even though the waiter now has a body to return.
			sent, err := g.reg.SendMessage(context.Background(), region.MessageInput{
				Namespace: "ns-a", Sender: "one", Recipient: "two", Body: "revoked-private-body",
			})
			if err != nil {
				t.Fatal(err)
			}
			g.bus.Publish(region.MessageEvent(sent))
			select {
			case got := <-done:
				out := text(t, got.res)
				if got.err != nil {
					out = got.err.Error()
				}
				if !strings.Contains(out, "namespace grant required") || strings.Contains(out, "revoked-private-body") {
					t.Fatalf("revoked wait leaked success: %v %s", got.err, out)
				}
				select {
				case allowed := <-decisions:
					if allowed {
						t.Fatal("post-wait authorization allowed revoked caller")
					}
				default:
					t.Fatal("await skipped post-wait authorization")
				}
			case <-ctx.Done():
				t.Fatal("revoked wait did not finish")
			}
		})
	}
}

func TestAwaitMessagesCancellationFreesWaiterAndSession(t *testing.T) {
	cs, _, gate := streamableBoardSession(t, func(d *Deps) { d.MessagingEnabled = true })
	registerMessageMembers(t, cs, "ns", "one", "two")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "await_messages", Arguments: map[string]any{"namespace": "ns", "agent": "two"}})
		done <- err
	}()
	time.Sleep(300 * time.Millisecond)
	cut := time.Now()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled await returned success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled await did not return")
	}
	by := cut.Add(5 * time.Second)
	for !gate.finishedInFlight(cut, by) {
		if time.Now().After(by) {
			t.Fatalf("server kept cancelled wait open: %v", gate.dump())
		}
		time.Sleep(50 * time.Millisecond)
	}
	callJSON(t, cs, "read_messages", map[string]any{"namespace": "ns", "agent": "two"}, nil)
}

func TestAwaitMessagesReconcilesWithoutBus(t *testing.T) {
	var store *region.Store
	cs := messagingSession(t, func(d *Deps) { store = d.Region })
	registerMessageMembers(t, cs, "ns", "one", "two")
	type result struct {
		res *mcp.CallToolResult
		err error
	}
	done := make(chan result, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "await_messages", Arguments: map[string]any{"namespace": "ns", "agent": "two", "timeout_seconds": 10}})
		done <- result{r, err}
	}()
	time.Sleep(100 * time.Millisecond)
	// Simulate another process: write storage without any local hint.
	sent, err := store.SendMessage(context.Background(), region.MessageInput{Namespace: "ns", Sender: "one", Recipient: "two", Body: "catch up"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != nil || got.res.IsError {
			t.Fatalf("wait failed: %v %s", got.err, text(t, got.res))
		}
		var inbox inboxWire
		if err := json.Unmarshal([]byte(text(t, got.res)), &inbox); err != nil {
			t.Fatal(err)
		}
		if len(inbox.Messages) != 1 || inbox.Messages[0].ID != sent.ID {
			t.Fatalf("durable reconciliation = %+v", inbox)
		}
	case <-ctx.Done():
		t.Fatal("bus-less wait did not reconcile storage")
	}
}
