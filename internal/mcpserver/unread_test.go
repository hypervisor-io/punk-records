package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hypervisor-io/punk-records/internal/region"
)

// unreadSession mounts the real server over the real streamable-HTTP
// transport with per-request identity headers (the shape punk connect
// bakes into the MCP entry), messaging enabled unless a tweak says
// otherwise, and returns the region store for direct seeding.
func unreadSession(t *testing.T, agent, ns string, tweaks ...func(*Deps)) (*mcp.ClientSession, *region.Store) {
	t.Helper()
	deps, _ := newTestDeps(t)
	deps.MessagingEnabled = true
	for _, tweak := range tweaks {
		if tweak != nil {
			tweak(&deps)
		}
	}
	srv := New(deps)
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	hdr := http.Header{}
	if agent != "" {
		hdr.Set("X-Punk-Agent", agent)
	}
	if ns != "" {
		hdr.Set("X-Punk-Namespace", ns)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "unread", Version: "0"}, nil)
	cs, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:   ts.URL,
		HTTPClient: &http.Client{Transport: headerRT{h: hdr}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, deps.Region
}

// seedUnread registers both addresses and leaves n unread messages for
// to in ns, written through storage directly like the delivery tests.
func seedUnread(t *testing.T, reg *region.Store, ns, from, to string, n int) {
	t.Helper()
	ctx := context.Background()
	for _, a := range []string{from, to} {
		if err := reg.Register(ctx, ns, a, ""); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < n; i++ {
		if _, err := reg.SendMessage(ctx, region.MessageInput{
			Namespace: ns, Sender: from, Recipient: to, Body: fmt.Sprintf("m%d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// textItems renders the result's text content items in wire order: the
// trailer is an appended item, so tests assert on item boundaries, not
// on substring concatenations.
func textItems(res *mcp.CallToolResult) []string {
	var out []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			out = append(out, tc.Text)
		}
	}
	return out
}

func TestUnreadTrailerOnNonMessageToolResult(t *testing.T) {
	cs, reg := unreadSession(t, "two", "ns-a")
	seedUnread(t, reg, "ns-a", "one", "two", 2)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "list_claims", Arguments: map[string]any{"namespace": "ns-a"}})
	if err != nil || res.IsError {
		t.Fatalf("list_claims: %v %s", err, text(t, res))
	}
	items := textItems(res)
	if len(items) != 2 {
		t.Fatalf("items = %q, want the tool payload plus one trailer", items)
	}
	if !json.Valid([]byte(items[0])) {
		t.Fatalf("tool payload is not intact JSON: %q", items[0])
	}
	if want := "2 unread for two in ns-a: call read_messages"; items[1] != want {
		t.Fatalf("trailer = %q, want %q", items[1], want)
	}
}

// TestUnreadTrailerExplicitAgentArg: the call's own agent argument names
// the address the trailer reports, even when the session identity is a
// different (empty-mailbox) address.
func TestUnreadTrailerExplicitAgentArg(t *testing.T) {
	cs, reg := unreadSession(t, "one", "ns-a")
	seedUnread(t, reg, "ns-a", "one", "two", 3)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "list_agent_regions", Arguments: map[string]any{"agent": "two"}})
	if err != nil || res.IsError {
		t.Fatalf("list_agent_regions: %v %s", err, text(t, res))
	}
	items := textItems(res)
	if len(items) != 2 || items[1] != "3 unread for two in ns-a: call read_messages" {
		t.Fatalf("items = %q, want the trailer for the explicit agent arg", items)
	}
}

func TestUnreadTrailerAbsent(t *testing.T) {
	t.Run("no unread", func(t *testing.T) {
		cs, _ := unreadSession(t, "two", "ns-a")
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name: "list_claims", Arguments: map[string]any{"namespace": "ns-a"}})
		if err != nil || res.IsError {
			t.Fatalf("list_claims: %v %s", err, text(t, res))
		}
		if items := textItems(res); len(items) != 1 || !json.Valid([]byte(items[0])) {
			t.Fatalf("zero-unread result grew a trailer: %q", items)
		}
	})

	t.Run("message tool excluded", func(t *testing.T) {
		cs, reg := unreadSession(t, "two", "ns-a")
		seedUnread(t, reg, "ns-a", "one", "two", 1)
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name: "read_messages", Arguments: map[string]any{"namespace": "ns-a"}})
		if err != nil || res.IsError {
			t.Fatalf("read_messages: %v %s", err, text(t, res))
		}
		if items := textItems(res); len(items) != 1 || !json.Valid([]byte(items[0])) ||
			strings.Contains(items[0], "call read_messages") {
			t.Fatalf("message tool result grew a trailer: %q", items)
		}
	})

	t.Run("anonymous caller", func(t *testing.T) {
		// In-memory transport carries no identity headers: the caller
		// resolves to anonymous "mcp" and gets no trailer even with
		// unread mail sitting in the namespace it calls into.
		var reg *region.Store
		cs := messagingSession(t, func(d *Deps) { reg = d.Region })
		seedUnread(t, reg, "ns", "one", "two", 2)
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name: "list_claims", Arguments: map[string]any{"namespace": "ns"}})
		if err != nil || res.IsError {
			t.Fatalf("list_claims: %v %s", err, text(t, res))
		}
		if items := textItems(res); len(items) != 1 {
			t.Fatalf("anonymous caller got a trailer: %q", items)
		}
	})

	t.Run("messaging disabled", func(t *testing.T) {
		cs, reg := unreadSession(t, "two", "ns-a", func(d *Deps) { d.MessagingEnabled = false })
		seedUnread(t, reg, "ns-a", "one", "two", 2)
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name: "list_claims", Arguments: map[string]any{"namespace": "ns-a"}})
		if err != nil || res.IsError {
			t.Fatalf("list_claims: %v %s", err, text(t, res))
		}
		if items := textItems(res); len(items) != 1 {
			t.Fatalf("disabled messaging grew a trailer: %q", items)
		}
	})

	t.Run("count error swallowed", func(t *testing.T) {
		// An over-long identity address passes header transport and
		// tool handling but fails the store's inbox validation, so the
		// trailer count errors: the tool call must still succeed and
		// carry no trailer.
		cs, reg := unreadSession(t, strings.Repeat("x", region.MaxMessageIDLen+44), "ns-a")
		seedUnread(t, reg, "ns-a", "one", "two", 2)
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name: "list_claims", Arguments: map[string]any{"namespace": "ns-a"}})
		if err != nil || res.IsError {
			t.Fatalf("count error failed the tool call: %v %s", err, text(t, res))
		}
		if items := textItems(res); len(items) != 1 {
			t.Fatalf("errored count grew a trailer: %q", items)
		}
	})
}

func TestUnreadTrailerCountErrorSwallowed(t *testing.T) {
	nsr := newNSResolver(nil, "agent-default")
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{
		Name: "list_claims", Arguments: json.RawMessage(`{"namespace":"ns-a","agent":"two"}`)}}
	tr := newUnreadTrailer(func(context.Context, string, string) (int64, error) {
		return 0, errors.New("store down")
	}, nsr)
	if line := tr.line(context.Background(), req); line != "" {
		t.Fatalf("count error surfaced as trailer %q", line)
	}
	// Message tools never reach the counter at all.
	counted := false
	tr = newUnreadTrailer(func(context.Context, string, string) (int64, error) {
		counted = true
		return 5, nil
	}, nsr)
	req.Params.Name = "read_messages"
	if line := tr.line(context.Background(), req); line != "" || counted {
		t.Fatalf("message tool trailer = %q, counted = %v", line, counted)
	}
}

// TestUnreadTrailerCache: within the TTL a repeated call reuses the
// cached count (asserted through a counting fake), a different address
// counts separately, and expiry past the TTL recounts.
func TestUnreadTrailerCache(t *testing.T) {
	nsr := newNSResolver(nil, "agent-default")
	clk := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	counts := map[string]int{}
	tr := newUnreadTrailer(func(_ context.Context, ns, agent string) (int64, error) {
		counts[ns+"/"+agent]++
		return 2, nil
	}, nsr)
	tr.now = func() time.Time { return clk }
	line := func(agent string) string {
		t.Helper()
		req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{
			Name: "list_claims",
			Arguments: json.RawMessage(fmt.Sprintf(
				`{"namespace":"ns-a","agent":%q}`, agent))}}
		return tr.line(context.Background(), req)
	}
	if want := "2 unread for two in ns-a: call read_messages"; line("two") != want || line("two") != want {
		t.Fatalf("trailer = %q, want %q", line("two"), want)
	}
	if counts["ns-a/two"] != 1 {
		t.Fatalf("cache hit within TTL recounted: counts = %v", counts)
	}
	if line("three") == "" || counts["ns-a/three"] != 1 || counts["ns-a/two"] != 1 {
		t.Fatalf("per-address cache wrong: counts = %v", counts)
	}
	clk = clk.Add(unreadTrailerTTL + time.Second)
	line("two")
	if counts["ns-a/two"] != 2 {
		t.Fatalf("expiry past TTL did not recount: counts = %v", counts)
	}
}

// TestUnreadTrailerFollowsJoinedInbox: a session that joined with a
// minted address (no hook, no agent argument on ordinary tools) gets the
// trailer for that joined inbox, in the joined namespace - not for the
// host-wide X-Punk-Agent identity, which owns no mailbox.
func TestUnreadTrailerFollowsJoinedInbox(t *testing.T) {
	cs, reg := unreadSession(t, "user@host", "")
	ctx := context.Background()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "join", Arguments: map[string]any{"namespace": "ns-j"}})
	if err != nil || res.IsError {
		t.Fatalf("join: %v %s", err, text(t, res))
	}
	var joined struct {
		Address string `json:"address"`
	}
	if b, _ := json.Marshal(res.StructuredContent); json.Unmarshal(b, &joined) != nil || joined.Address == "" {
		t.Fatalf("join output %v", res.StructuredContent)
	}
	seedUnread(t, reg, "ns-j", "peer", joined.Address, 2)
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "whoami", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("whoami: %v %s", err, text(t, res))
	}
	items := textItems(res)
	want := "2 unread for " + joined.Address + " in ns-j: call read_messages"
	if len(items) != 2 || items[1] != want {
		t.Fatalf("items = %q, want trailer %q", items, want)
	}
}
