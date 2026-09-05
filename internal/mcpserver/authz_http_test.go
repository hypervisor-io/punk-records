package mcpserver

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hypervisor-io/punk-records/internal/authz"
	"github.com/hypervisor-io/punk-records/internal/bus"
	"github.com/hypervisor-io/punk-records/internal/memory"
)

// A02 behavioral proofs over the real HTTP MCP boundary: namespace
// selection inputs never grant access, inherited/forged session
// identity never grants access, subscription delivery is authorized at
// subscribe time AND reauthorized at every delivery (revocation closes
// the session), and the trusted modes (stdio, enforcement off) stay
// compatible.

// TestMCPAuthzSelectionInputsNeverGrant: explicit tool arguments, the
// X-Punk-Namespace header, client roots, and the server default all
// only SELECT the namespace; the grant decision uses the verified
// credential subject alone.
func TestMCPAuthzSelectionInputsNeverGrant(t *testing.T) {
	g := mcpAuthzRigNew(t, true)
	ctx := context.Background()
	if _, err := g.mem.Write(ctx, memory.WriteInput{Namespace: "ns-b", Key: "/secret", Body: "b-only"}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.mem.Write(ctx, memory.WriteInput{Namespace: "agent-ns-b", Key: "/secret", Body: "b-only"}); err != nil {
		t.Fatal(err)
	}

	// explicit argument
	cs := g.connect(t)
	if out, denied, err := call(t, cs, "recall", map[string]any{"namespace": "ns-b"}); !denied {
		t.Fatalf("explicit ns-b: denied=%v err=%v out=%s", denied, err, out)
	}

	// X-Punk-Namespace header (namespace omitted in the call)
	cs = g.connectOpts(t, http.Header{"X-Punk-Namespace": {"ns-b"}}, nil, nil)
	if out, denied, err := call(t, cs, "recall", map[string]any{}); !denied {
		t.Fatalf("header ns-b: denied=%v err=%v out=%s", denied, err, out)
	}

	// client roots mapped to a B namespace (namespace omitted)
	cs = g.connectOpts(t, nil, []string{"file:///work/ns-b"}, nil)
	if out, denied, err := call(t, cs, "recall", map[string]any{}); !denied {
		t.Fatalf("roots agent-ns-b: denied=%v err=%v out=%s", denied, err, out)
	}

	// server default namespace: deny-by-default when ungranted
	cs = g.connectOpts(t, nil, nil, nil)
	if out, denied, err := call(t, cs, "recall", map[string]any{}); !denied {
		t.Fatalf("default agent-default: denied=%v err=%v out=%s", denied, err, out)
	}
}

// TestMCPAuthzSessionIdentityNeverGrants: a forged X-Punk-Subject or an
// agent label must never widen access; only the credential's verified
// subject counts. bob legitimately holds ns-b, alice's token must not
// inherit it through headers.
func TestMCPAuthzSessionIdentityNeverGrants(t *testing.T) {
	g := mcpAuthzRigNew(t, true)
	ctx := context.Background()
	if err := g.az.Grant(ctx, "bob", "ns-b", authz.OpRead); err != nil {
		t.Fatal(err)
	}
	if err := g.az.Grant(ctx, "bob", "ns-b", authz.OpWrite); err != nil {
		t.Fatal(err)
	}
	forged := http.Header{
		"X-Punk-Subject":   {"bob"},
		"X-Punk-Agent":     {"bob"},
		"X-Punk-Namespace": {"ns-b"},
	}
	cs := g.connectOpts(t, forged, nil, nil)
	if out, denied, err := call(t, cs, "recall", map[string]any{}); !denied {
		t.Fatalf("forged subject recall: denied=%v err=%v out=%s", denied, err, out)
	}
	if out, denied, err := call(t, cs, "register", map[string]any{}); !denied {
		t.Fatalf("forged subject register: denied=%v err=%v out=%s", denied, err, out)
	}
	// whoami still reports the (denied) selection and the agent label -
	// it is a diagnostic, and it grants nothing
	out, denied, hadErr := call(t, cs, "whoami", map[string]any{})
	if denied || hadErr {
		t.Fatalf("whoami should stay a diagnostic: denied=%v err=%v out=%s", denied, hadErr, out)
	}
	if !strings.Contains(out, `"namespace":"ns-b"`) {
		t.Fatalf("whoami selection = %s", out)
	}
}

// TestMCPAuthzSubscriptionRevocation: the documented revocation policy
// for MCP resource subscriptions - authorize at subscribe time, then
// reauthorize before EVERY delivery. A session whose grant was revoked
// is closed on the next matching change: it receives no further
// notifications, and its session no longer serves calls.
func TestMCPAuthzSubscriptionRevocation(t *testing.T) {
	g := mcpAuthzRigNew(t, true)
	ctx := context.Background()
	updated := make(chan string, 8)
	cs := g.connectOpts(t, nil, nil, updated)

	uri := "punk://memory/ns-a/tasks"
	if err := cs.Subscribe(ctx, &mcp.SubscribeParams{URI: uri}); err != nil {
		t.Fatalf("subscribe granted ns-a = %v, want ok", err)
	}
	// the subscribe round trip completed, so the server-side record
	// exists; give the notification stream a moment
	time.Sleep(200 * time.Millisecond)

	g.bus.Publish(bus.Event{Kind: "memory", Key: "ns-a:/tasks/T1/status", Data: map[string]string{"action": "add"}})
	select {
	case got := <-updated:
		if got != uri {
			t.Fatalf("updated uri = %s, want %s", got, uri)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no notification for a matching event on a granted subscription")
	}

	// revoke, then publish again: no delivery, and the session is closed
	if err := g.az.Revoke(ctx, "alice", "ns-a", authz.OpRead); err != nil {
		t.Fatal(err)
	}
	g.bus.Publish(bus.Event{Kind: "memory", Key: "ns-a:/tasks/T2/status", Data: map[string]string{"action": "add"}})
	select {
	case got := <-updated:
		t.Fatalf("post-revoke notification %q delivered, want none", got)
	case <-time.After(1500 * time.Millisecond):
	}
	// the closed session no longer serves tool calls
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "whoami", Arguments: map[string]any{}}); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("session still serving calls after subscription revocation, want closed session")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestMCPAuthzListAgentRegionsFiltered: the cross-region enumeration
// tool reports only regions the verified subject can read.
func TestMCPAuthzListAgentRegionsFiltered(t *testing.T) {
	g := mcpAuthzRigNew(t, true)
	ctx := context.Background()
	if err := g.reg.Register(ctx, "ns-a", "worker", ""); err != nil {
		t.Fatal(err)
	}
	if err := g.reg.Register(ctx, "ns-b", "worker", ""); err != nil {
		t.Fatal(err)
	}
	cs := g.connect(t)
	out, denied, hadErr := call(t, cs, "list_agent_regions", map[string]any{"agent": "worker"})
	if denied || hadErr {
		t.Fatalf("list_agent_regions: denied=%v err=%v out=%s", denied, hadErr, out)
	}
	if !strings.Contains(out, `"namespace":"ns-a"`) {
		t.Fatalf("granted region missing: %s", out)
	}
	if strings.Contains(out, `"namespace":"ns-b"`) {
		t.Fatalf("unreadable region leaked: %s", out)
	}
}

// TestMCPAuthzZeroKeyBootstrapDenied: enforcement mode with zero keys
// passes authentication (bootstrap) but the empty subject holds no
// grants: global tools answer, namespaced tools are denied.
func TestMCPAuthzZeroKeyBootstrapDenied(t *testing.T) {
	g := mcpAuthzRigNew(t, true)
	if err := g.keys.Revoke(context.Background(), "alice-key"); err != nil {
		t.Fatal(err)
	}
	g.token = "" // bootstrap: no credential needed, none verified
	cs := g.connect(t)
	if out, denied, hadErr := call(t, cs, "whoami", map[string]any{}); denied || hadErr {
		t.Fatalf("bootstrap whoami: denied=%v err=%v out=%s", denied, hadErr, out)
	}
	if out, denied, err := call(t, cs, "recall", map[string]any{"namespace": "ns-a"}); !denied {
		t.Fatalf("bootstrap recall: denied=%v err=%v out=%s", denied, err, out)
	}
}

// TestMCPAuthzStdioTrustedTransport: the stdio server (punk mcp) has no
// HTTP credential boundary and no injected gate; like direct DB access
// it is a trusted local transport (internal/authz package docs). An
// in-memory session - the exact transport `punk mcp` serves - keeps
// full access even for namespaces no HTTP subject is granted.
func TestMCPAuthzStdioTrustedTransport(t *testing.T) {
	deps, memStore := newTestDeps(t)
	srv := New(deps)
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "stdio", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "remember",
		Arguments: map[string]any{"namespace": "ns-b", "key": "/k", "body": "v"}})
	if err != nil || res.IsError {
		t.Fatalf("stdio remember: err=%v %s", err, text(t, res))
	}
	facts, err := memStore.Recall(ctx, "ns-b", "/k", 1)
	if err != nil || len(facts) != 1 {
		t.Fatalf("stdio write not stored: %v %v", facts, err)
	}
}

// TestMCPAuthzDisabledCompatHTTP: with enforcement off (no authorizer
// wired - the default), any verified key keeps the legacy trusted
// behavior over HTTP MCP, including B access and subscriptions.
func TestMCPAuthzDisabledCompatHTTP(t *testing.T) {
	g := mcpAuthzRigNew(t, false)
	ctx := context.Background()
	token, err := g.keys.Create(ctx, "trusted", "alice")
	if err != nil {
		t.Fatal(err)
	}
	g.token = token
	updated := make(chan string, 8)
	cs := g.connectOpts(t, nil, nil, updated)
	if out, denied, hadErr := call(t, cs, "remember", map[string]any{"namespace": "ns-b", "key": "/k", "body": "v"}); denied || hadErr {
		t.Fatalf("off-mode remember ns-b: denied=%v err=%v out=%s", denied, hadErr, out)
	}
	if out, denied, hadErr := call(t, cs, "recall", map[string]any{"namespace": "ns-b"}); denied || hadErr {
		t.Fatalf("off-mode recall ns-b: denied=%v err=%v out=%s", denied, hadErr, out)
	}
	if err := cs.Subscribe(ctx, &mcp.SubscribeParams{URI: "punk://memory/ns-b/tasks"}); err != nil {
		t.Fatalf("off-mode subscribe ns-b = %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	g.bus.Publish(bus.Event{Kind: "memory", Key: "ns-b:/tasks/T1", Data: map[string]string{"action": "add"}})
	select {
	case <-updated:
	case <-time.After(3 * time.Second):
		t.Fatal("off-mode notification missing")
	}
}
