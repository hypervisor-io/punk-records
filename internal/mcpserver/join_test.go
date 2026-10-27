package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hypervisor-io/punk-records/internal/authz"
	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/region"
	"github.com/hypervisor-io/punk-records/internal/store"
)

// P4-A: the join tool - one-call register + inbox bind + who-am-I with
// the active peers and their delivery tiers. Acceptance: explicit
// address, minted address stable within the MCP session, bind recorded.

// joinWire decodes the public output shape.
type joinWire struct {
	Address   string `json:"address"`
	Namespace string `json:"namespace"`
	Minted    bool   `json:"minted"`
	Peers     []struct {
		Agent      string `json:"agent"`
		Role       string `json:"role"`
		Listening  bool   `json:"listening"`
		LastSeenAt string `json:"last_seen_at"`
		Delivery   string `json:"delivery"`
	} `json:"peers"`
	PeersNote  string `json:"peers_note"`
	Delivery   string `json:"delivery"`
	MintedNote string `json:"minted_note"`
}

// joinRig is the register_test-shaped minimal server: one sqlite db, a
// region store and a memory store, exposed over an in-memory transport.
// It returns the server so a test can attach a SECOND session (the
// minted-address isolation case) and the db for last_seen surgery (the
// active-window case).
func joinRig(t *testing.T) (*mcp.Server, *region.Store, *store.DB) {
	t.Helper()
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "join.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if _, err := db.MigrateUp(ctx); err != nil {
		t.Fatal(err)
	}
	reg := region.New(db, nil)
	srv := New(Deps{Mem: memory.New(db, nil), Region: reg})
	return srv, reg, db
}

// joinSession attaches one client session (clientInfo name as given) to
// the rig server over a fresh in-memory transport.
func joinSession(t *testing.T, srv *mcp.Server, clientName string) *mcp.ClientSession {
	t.Helper()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: clientName, Version: "0"}, nil)
	cs, err := client.Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

var mintedAddressRe = regexp.MustCompile(`^[^:]+:[0-9a-f]{16}$`)

func TestJoinExplicitAddressRegistersAndBinds(t *testing.T) {
	srv, reg, _ := joinRig(t)
	cs := joinSession(t, srv, "t")
	ctx := context.Background()

	var out joinWire
	callJSON(t, cs, "join", map[string]any{"namespace": "ns-j", "address": "claude-code:s9", "role": "worker"}, &out)
	if out.Address != "claude-code:s9" || out.Namespace != "ns-j" || out.Minted {
		t.Fatalf("join = %+v, want the explicit address in ns-j, not minted", out)
	}
	if out.Delivery == "" {
		t.Fatal("join must state the delivery rule for this address")
	}
	// Bind recorded: the inbox of the explicit address now routes to ns-j.
	if ns, ok, err := reg.InboxBinding(ctx, "claude-code:s9"); err != nil || !ok || ns != "ns-j" {
		t.Fatalf("binding = (%q, %v, %v), want ns-j recorded", ns, ok, err)
	}
	// Membership recorded, role kept.
	members, err := reg.Members(ctx, "ns-j")
	if err != nil || len(members) != 1 || members[0].Agent != "claude-code:s9" || members[0].Role != "worker" {
		t.Fatalf("members = %+v (%v), want the registered worker", members, err)
	}
	// The joined address is its own first peer: active (just registered),
	// not listening (no bridge stream in this test), tier catch_up.
	if len(out.Peers) != 1 || out.Peers[0].Agent != "claude-code:s9" || out.Peers[0].Delivery != "catch_up" || out.Peers[0].Listening {
		t.Fatalf("peers = %+v, want the fresh member on the catch_up tier", out.Peers)
	}
	if out.PeersNote != "" {
		t.Fatalf("unexpected peers_note %q", out.PeersNote)
	}
	// An explicit address is never minted, so the no-hook-reader hint
	// stays empty: this address may well have a hook reader.
	if out.MintedNote != "" {
		t.Fatalf("minted_note = %q, want it empty for an explicit address", out.MintedNote)
	}
}

func TestJoinMintedAddressStableWithinSession(t *testing.T) {
	srv, reg, _ := joinRig(t)
	cs := joinSession(t, srv, "codex")
	ctx := context.Background()

	var first, second joinWire
	callJSON(t, cs, "join", map[string]any{"namespace": "ns-m"}, &first)
	if !first.Minted || !mintedAddressRe.MatchString(first.Address) || !strings.HasPrefix(first.Address, "codex:") {
		t.Fatalf("first join = %+v, want a minted codex:<16 hex> address", first)
	}
	if !strings.Contains(first.MintedNote, "no hook reader") || !strings.Contains(first.MintedNote, "await_messages/read_messages") {
		t.Fatalf("minted_note = %q, want the no-hook-reader receive hint", first.MintedNote)
	}
	callJSON(t, cs, "join", map[string]any{"namespace": "ns-m"}, &second)
	if second.Address != first.Address || !second.Minted || second.MintedNote != first.MintedNote {
		t.Fatalf("repeat join = %+v, want the SAME minted address %q and the same hint", second, first.Address)
	}
	// Bind recorded for the minted address.
	if ns, ok, err := reg.InboxBinding(ctx, first.Address); err != nil || !ok || ns != "ns-m" {
		t.Fatalf("binding = (%q, %v, %v), want ns-m recorded for %s", ns, ok, err, first.Address)
	}

	// A DIFFERENT session on the same server mints a DIFFERENT address:
	// the mint is per MCP session, never server-global.
	other := joinSession(t, srv, "codex")
	var third joinWire
	callJSON(t, other, "join", map[string]any{"namespace": "ns-m"}, &third)
	if !third.Minted || third.Address == first.Address {
		t.Fatalf("second session = %+v, want a fresh minted address distinct from %q", third, first.Address)
	}
}

func TestJoinSameHostIdentityMintsDistinctAddresses(t *testing.T) {
	// The verdict defect: punk connect writes X-Punk-Agent as one
	// user@host value (e.g. ubuntu@SGITSERVER) for EVERY client on the
	// host, so every HTTP MCP session on the machine rides the same
	// header. Two sessions with that same header and no address must
	// mint two distinct per-session addresses with two bindings - never
	// join as, and bind the inbox of, one shared host identity that
	// steals everyone's mail and whose inbox the last join moves.
	g := mcpAuthzRigNew(t, true)
	hdr := http.Header{"X-Punk-Agent": {"codex:hook-1"}, "X-Punk-Namespace": {"ns-a"}}
	firstCS := g.connectOpts(t, hdr, nil, nil)
	secondCS := g.connectOpts(t, hdr, nil, nil)
	ctx := context.Background()

	var a, b joinWire
	callJSON(t, firstCS, "join", map[string]any{}, &a)
	callJSON(t, secondCS, "join", map[string]any{}, &b)
	for i, out := range []joinWire{a, b} {
		if !out.Minted || !mintedAddressRe.MatchString(out.Address) || !strings.HasPrefix(out.Address, "a02:") {
			t.Fatalf("session %d join = %+v, want a minted a02:<16 hex> address, never the host header", i+1, out)
		}
		if out.Address == "codex:hook-1" {
			t.Fatal("the host-level X-Punk-Agent header must never become an inbox address")
		}
		if !strings.Contains(out.MintedNote, "no hook reader") || !strings.Contains(out.MintedNote, "await_messages/read_messages") {
			t.Fatalf("session %d minted_note = %q, want the no-hook-reader receive hint", i+1, out.MintedNote)
		}
	}
	if a.Address == b.Address {
		t.Fatalf("both sessions minted the SAME address %q, want two distinct addresses", a.Address)
	}
	// Two bindings: one per minted address, both to the header
	// namespace; the host identity itself never gets one.
	for _, addr := range []string{a.Address, b.Address} {
		if ns, ok, err := g.reg.InboxBinding(ctx, addr); err != nil || !ok || ns != "ns-a" {
			t.Fatalf("binding for %s = (%q, %v, %v), want ns-a", addr, ns, ok, err)
		}
	}
	if _, ok, err := g.reg.InboxBinding(ctx, "codex:hook-1"); err != nil || ok {
		t.Fatalf("the host identity itself got a binding (%v, %v); it must never bind", ok, err)
	}
	// The pinned host identity survives as role metadata only.
	members, err := g.reg.Members(ctx, "ns-a")
	if err != nil || len(members) != 2 {
		t.Fatalf("members = %+v (%v), want exactly the two minted addresses", members, err)
	}
	for _, m := range members {
		if !strings.Contains(m.Role, "(host identity codex:hook-1)") {
			t.Fatalf("member %s role = %q, want the host identity kept as metadata", m.Agent, m.Role)
		}
	}
	// Minted stays stable across repeat calls in one session.
	var again joinWire
	callJSON(t, firstCS, "join", map[string]any{}, &again)
	if again.Address != a.Address || !again.Minted {
		t.Fatalf("repeat join = %+v, want the SAME minted address %q", again, a.Address)
	}
}

func TestJoinExplicitAddressBeatsHostIdentity(t *testing.T) {
	// An explicit address wins over everything, the host header
	// included: the model passes the [PUNK ROUTING] block's address so
	// the hook keeps reading its <client>:<session> inbox, and the
	// pinned host identity rides along as role metadata.
	g := mcpAuthzRigNew(t, true)
	cs := g.connectOpts(t, http.Header{"X-Punk-Agent": {"codex:hook-1"}, "X-Punk-Namespace": {"ns-a"}}, nil, nil)
	ctx := context.Background()

	var out joinWire
	callJSON(t, cs, "join", map[string]any{"address": "codex:routed-9", "role": "worker"}, &out)
	if out.Address != "codex:routed-9" || out.Namespace != "ns-a" || out.Minted {
		t.Fatalf("join = %+v, want the explicit address in ns-a, never minted", out)
	}
	if out.MintedNote != "" {
		t.Fatalf("minted_note = %q, want it empty for an explicit address", out.MintedNote)
	}
	if ns, ok, err := g.reg.InboxBinding(ctx, "codex:routed-9"); err != nil || !ok || ns != "ns-a" {
		t.Fatalf("binding = (%q, %v, %v), want ns-a recorded for the explicit address", ns, ok, err)
	}
	if _, ok, err := g.reg.InboxBinding(ctx, "codex:hook-1"); err != nil || ok {
		t.Fatalf("the host identity got a binding (%v, %v); it must never bind", ok, err)
	}
	members, err := g.reg.Members(ctx, "ns-a")
	if err != nil || len(members) != 1 || members[0].Agent != "codex:routed-9" || members[0].Role != "worker (host identity codex:hook-1)" {
		t.Fatalf("members = %+v (%v), want the explicit address with the host identity as role metadata", members, err)
	}
}

func TestJoinPeersAreActiveOnly(t *testing.T) {
	srv, reg, db := joinRig(t)
	cs := joinSession(t, srv, "t")
	ctx := context.Background()

	// A fresh member (active by registration) and a stale one (last seen
	// 20 minutes ago, past the 10-minute active window), plus a member
	// of another namespace that must never surface.
	if err := reg.Register(ctx, "ns-p", "peer-fresh", "worker"); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(ctx, "ns-p", "peer-stale", "worker"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, db.Rebind(`UPDATE region_members SET last_seen_at = $1 WHERE namespace = $2 AND agent = $3`),
		store.TimeToDB(time.Now().Add(-20*time.Minute)), "ns-p", "peer-stale"); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(ctx, "ns-other", "outsider", "worker"); err != nil {
		t.Fatal(err)
	}

	var out joinWire
	callJSON(t, cs, "join", map[string]any{"namespace": "ns-p", "address": "me:s1"}, &out)
	got := map[string]string{}
	for _, p := range out.Peers {
		got[p.Agent] = p.Delivery
	}
	if len(got) != 2 || got["peer-fresh"] != "catch_up" || got["me:s1"] != "catch_up" {
		t.Fatalf("peers = %+v, want exactly the active ns-p members (peer-fresh, me:s1) on catch_up", out.Peers)
	}
}

func TestJoinUnderEnforcedAuthz(t *testing.T) {
	g := mcpAuthzRigNew(t, true)
	ctx := context.Background()

	// alice holds read+write on ns-a: the full one-call flow.
	alice := g.connectOpts(t, nil, nil, nil)
	var out joinWire
	callJSON(t, alice, "join", map[string]any{"namespace": "ns-a", "address": "alice:s1"}, &out)
	if out.Address != "alice:s1" || out.PeersNote != "" || len(out.Peers) != 1 {
		t.Fatalf("alice join = %+v, want the full flow with peers listed", out)
	}

	// bob holds WRITE only on ns-b: register+bind succeed, the peers
	// listing degrades to a note instead of failing the whole call or
	// leaking read-classified data past the grant.
	token, err := g.keys.Create(ctx, "bob-key", "bob")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.az.Grant(ctx, "bob", "ns-b", authz.OpWrite); err != nil {
		t.Fatal(err)
	}
	g.token = token // the rig dials every connectOpts session with this bearer
	bob := g.connectOpts(t, nil, nil, nil)
	var bobOut joinWire
	callJSON(t, bob, "join", map[string]any{"namespace": "ns-b", "address": "bob:s1"}, &bobOut)
	if bobOut.Address != "bob:s1" || bobOut.Namespace != "ns-b" {
		t.Fatalf("bob join = %+v, want register+bind to succeed on the write grant", bobOut)
	}
	if len(bobOut.Peers) != 0 || !strings.Contains(bobOut.PeersNote, "read") {
		t.Fatalf("bob join = %+v, want empty peers and a read-grant note", bobOut)
	}
	if ns, ok, err := g.reg.InboxBinding(ctx, "bob:s1"); err != nil || !ok || ns != "ns-b" {
		t.Fatalf("bob binding = (%q, %v, %v), want ns-b recorded", ns, ok, err)
	}

	// No grant at all: the whole call is refused (write denied) and
	// nothing is registered.
	res, err := bob.CallTool(ctx, &mcp.CallToolParams{Name: "join", Arguments: map[string]any{"namespace": "ns-c", "address": "bob:s2"}})
	if err == nil && !res.IsError {
		t.Fatalf("join without a grant must fail, got %s", text(t, res))
	}
	if members, merr := g.reg.Members(ctx, "ns-c"); merr == nil && len(members) > 0 {
		t.Fatalf("denied join registered members: %+v", members)
	}
}

func TestJoinInAgentToolset(t *testing.T) {
	cs := sessionOpts(t, nil, func(d *Deps) { d.Toolset = "agent" })
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range res.Tools {
		if tool.Name == "join" {
			found = true
		}
	}
	if !found {
		t.Fatal("lean agent toolset must serve join")
	}
	// Sanity: the output shape survives the wire.
	var out joinWire
	callJSON(t, cs, "join", map[string]any{"namespace": "ns-t"}, &out)
	if out.Address == "" || out.Namespace != "ns-t" || !out.Minted {
		raw, _ := json.Marshal(out)
		t.Fatalf("lean join = %s, want a minted address in ns-t", raw)
	}
}
