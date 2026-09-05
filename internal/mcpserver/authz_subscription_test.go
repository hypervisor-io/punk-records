package mcpserver

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hypervisor-io/punk-records/internal/authz"
	"github.com/hypervisor-io/punk-records/internal/bus"
)

// A02 subscription delivery proofs (round-2 review): the subscription
// registry may never lose permission state while the MCP SDK can still
// deliver, deliveries revalidate the subscribing CREDENTIAL (not just
// the namespace grant), and an MCP session belongs to the verified
// subject that created it - a changed credential reusing the session
// cannot attach to another subject's subscriptions.

// TestMCPSubscriptionRegistrySurvivesSubscriptionFlood: the MCP SDK
// keeps its own per-session subscription set and keeps notifying every
// session that ever subscribed; the local registry carries the gate
// every delivery reauthorizes through. If the registry forgets a
// session - here via the bounded overflow another client's
// subscription flood triggers - the forgotten session silently exits
// reauthorization while the SDK keeps delivering to it: after a
// revocation the revoked client kept receiving updates. The registry
// now bounds SESSIONS (never dropping entries of live SDK sessions),
// revocation prunes and closes the revoked session before delivery,
// and the flooding client still receives its legitimate updates.
func TestMCPSubscriptionRegistrySurvivesSubscriptionFlood(t *testing.T) {
	g := mcpAuthzRigNew(t, true)
	ctx := context.Background()
	aUpdates := make(chan string, 8)
	a := g.connectOpts(t, nil, nil, aUpdates)
	uri := "punk://memory/ns-a/private"
	if err := a.Subscribe(ctx, &mcp.SubscribeParams{URI: uri}); err != nil {
		t.Fatal(err)
	}
	token, err := g.keys.Create(ctx, "bob-key", "bob")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.az.Grant(ctx, "bob", "ns-a", authz.OpRead); err != nil {
		t.Fatal(err)
	}
	g.token = token
	bUpdates := make(chan string, 8)
	b := g.connectOpts(t, nil, nil, bUpdates)
	for i := 0; i < maxCachedSessions; i++ {
		if err := b.Subscribe(ctx, &mcp.SubscribeParams{URI: fmt.Sprintf("punk://memory/ns-a/fill%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Subscribe(ctx, &mcp.SubscribeParams{URI: uri}); err != nil {
		t.Fatal(err)
	}
	if err := g.az.Revoke(ctx, "alice", "ns-a", authz.OpRead); err != nil {
		t.Fatal(err)
	}
	g.bus.Publish(bus.Event{Kind: "memory", Key: "ns-a:/private/change"})
	select {
	case <-bUpdates:
	case <-time.After(3 * time.Second):
		t.Fatal("authorized subscriber did not receive positive-control event")
	}
	select {
	case u := <-aUpdates:
		t.Fatalf("revoked subscriber received %s: the registry lost its gate while SDK delivery survived", u)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestMCPSubscriptionKeyRevocationStopsDelivery: MCP deliveries
// revalidate the credential behind the subscription gate, not just the
// namespace grant (A02 round-2 review): revoking the API key a session
// connected with - while another key stays active, so the server does
// not fall back to bootstrap - stops every later delivery to that
// session even though the subject's grants are untouched. Regression:
// the gate stored only the subject and checked only grants, so a
// revoked key kept its existing subscription alive.
func TestMCPSubscriptionKeyRevocationStopsDelivery(t *testing.T) {
	g := mcpAuthzRigNew(t, true)
	ctx := context.Background()
	updates := make(chan string, 8)
	cs := g.connectOpts(t, nil, nil, updates)
	uri := "punk://memory/ns-a/private"
	if err := cs.Subscribe(ctx, &mcp.SubscribeParams{URI: uri}); err != nil {
		t.Fatal(err)
	}
	g.bus.Publish(bus.Event{Kind: "memory", Key: "ns-a:/private/before"})
	select {
	case <-updates:
	case <-time.After(3 * time.Second):
		t.Fatal("initial allowed event missing")
	}
	if _, err := g.keys.Create(ctx, "other-key", "bob"); err != nil {
		t.Fatal(err)
	}
	if err := g.keys.Revoke(ctx, "alice-key"); err != nil {
		t.Fatal(err)
	}
	g.bus.Publish(bus.Event{Kind: "memory", Key: "ns-a:/private/after"})
	select {
	case u := <-updates:
		t.Fatalf("revoked credential continues to receive %s", u)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestMCPSessionCredentialChangedCannotAttach: an MCP session belongs
// to the verified subject that created it. A different credential -
// even one holding every grant on the namespace - must not reuse the
// session ID to subscribe (attaching to the creator's subscriptions),
// and the creator's own revocation must still end her deliveries: a
// changed-credential reuse of an MCP session cannot attach to another
// subject's subscriptions. Regression context: the gate used to be
// re-recorded from whatever credential last subscribed on the session,
// so an attacker's valid grant could keep a victim's session receiving
// after the victim's own revocation.
func TestMCPSessionCredentialChangedCannotAttach(t *testing.T) {
	g := mcpAuthzRigNew(t, true)
	ctx := context.Background()
	bobToken, err := g.keys.Create(ctx, "bob-key", "bob")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.az.Grant(ctx, "bob", "ns-a", authz.OpRead); err != nil {
		t.Fatal(err)
	}
	endpoint := g.ts.URL + "/mcp"
	uri := "punk://memory/ns-a/private"

	sessionID := rawMCPSession(t, endpoint, g.token)
	if status := rawMCPSubscribe(t, endpoint, sessionID, g.token, uri, 2); status != http.StatusOK {
		t.Fatalf("alice subscribe = %d, want 200", status)
	}
	data, cancel := rawMCPListen(t, endpoint, sessionID, g.token)
	defer cancel()

	// positive control: alice's subscription delivers on her stream
	// (retry-publish until the GET stream registration is live)
	live := false
	deadline := time.Now().Add(3 * time.Second)
	for !live {
		g.bus.Publish(bus.Event{Kind: "memory", Key: "ns-a:/private/x"})
		select {
		case d, open := <-data:
			if !open {
				t.Fatal("alice's stream closed before any delivery")
			}
			if !strings.Contains(d, "resources/updated") {
				t.Fatalf("first frame = %s, want resources/updated", d)
			}
			live = true
		case <-time.After(200 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatal("no notification on alice's stream: subscription never went live")
			}
		}
	}
	// drain any frames the retry-publishes queued behind the first one
	for drained := true; drained; {
		select {
		case <-data:
		case <-time.After(200 * time.Millisecond):
			drained = false
		}
	}

	// bob reuses alice's session ID with his own (fully granted)
	// credential: the boundary must reject the attach
	if status := rawMCPSubscribe(t, endpoint, sessionID, bobToken, uri, 3); status != http.StatusForbidden {
		t.Fatalf("changed-credential subscribe on alice's session = %d, want 403", status)
	}

	// alice's revocation still ends HER deliveries: the rejected attach
	// did not rebind her subscription to bob's credential
	if err := g.az.Revoke(ctx, "alice", "ns-a", authz.OpRead); err != nil {
		t.Fatal(err)
	}
	g.bus.Publish(bus.Event{Kind: "memory", Key: "ns-a:/private/y"})
	select {
	case d, open := <-data:
		if open && strings.Contains(d, "resources/updated") {
			t.Fatalf("alice's session received %s after her revocation: the attach rebound her subscription", d)
		}
	case <-time.After(300 * time.Millisecond):
	}
}

// rawMCPPost sends one JSON-RPC message to the streamable-HTTP endpoint
// and returns the response (caller closes the body). sessionID ""
// targets the session-creation path (initialize).
func rawMCPPost(t *testing.T, endpoint, sessionID, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// rawMCPSession initializes a raw HTTP MCP session (plus the
// initialized notification) and returns its Mcp-Session-Id.
func rawMCPSession(t *testing.T, endpoint, token string) string {
	t.Helper()
	res := rawMCPPost(t, endpoint, "", token,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"a02-raw","version":"0"}}}`)
	_, _ = io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("initialize = %d, want 200", res.StatusCode)
	}
	id := res.Header.Get("Mcp-Session-Id")
	if id == "" {
		t.Fatal("initialize response carried no Mcp-Session-Id")
	}
	res = rawMCPPost(t, endpoint, id, token, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	_, _ = io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("initialized notification = %d, want 202", res.StatusCode)
	}
	return id
}

// rawMCPSubscribe subscribes a session to uri and returns the HTTP
// status of the request.
func rawMCPSubscribe(t *testing.T, endpoint, sessionID, token, uri string, id int) int {
	t.Helper()
	res := rawMCPPost(t, endpoint, sessionID, token, fmt.Sprintf(
		`{"jsonrpc":"2.0","id":%d,"method":"resources/subscribe","params":{"uri":%q}}`, id, uri))
	_, _ = io.ReadAll(res.Body)
	_ = res.Body.Close()
	return res.StatusCode
}

// rawMCPListen opens the session's standalone SSE stream and pushes
// every "data:" payload into the returned channel until the stream
// ends; the channel closes on EOF. The returned cancel ends the
// request.
func rawMCPListen(t *testing.T, endpoint, sessionID, token string) (<-chan string, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Mcp-Session-Id", sessionID)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		_ = res.Body.Close()
		cancel()
		t.Fatalf("GET notification stream = %d, want 200", res.StatusCode)
	}
	data := make(chan string, 16)
	go func() {
		defer close(data)
		defer res.Body.Close()
		r := bufio.NewReader(res.Body)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if d, ok := strings.CutPrefix(strings.TrimRight(line, "\n"), "data: "); ok {
				select {
				case data <- d:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return data, cancel
}

// fakeSubjectGate is a NamespaceGate + api.SubjectGate double for
// registry unit tests: allow is the grant decision, subject is the
// verified credential subject the registry binds.
type fakeSubjectGate struct {
	subject string
	allow   bool
}

func (f fakeSubjectGate) Allow(context.Context, string, authz.Op) bool { return f.allow }
func (f fakeSubjectGate) Subject() string                              { return f.subject }

// TestSubscriptionRegistryBindsSessionSubject: defense in depth behind
// the HTTP boundary's session-user binding - even if a subscribe from a
// different verified subject reaches the registry on an existing
// session, the registry rejects the rebind instead of replacing the
// creating subject's gate, so delivery reauthorization always judges
// the subject that created the subscriptions. Trusted (gate-less)
// sessions never bind, matching the stdio trust model.
func TestSubscriptionRegistryBindsSessionSubject(t *testing.T) {
	subs := newSubscriptions()
	ss := &mcp.ServerSession{} // the registry uses the pointer only as a key here
	if err := subs.add("punk://memory/ns-a/x", ss, fakeSubjectGate{subject: "alice", allow: true}); err != nil {
		t.Fatal(err)
	}
	// the same subject again (e.g. a rotated key): allowed, gate refreshed
	if err := subs.add("punk://memory/ns-a/y", ss, fakeSubjectGate{subject: "alice", allow: true}); err != nil {
		t.Fatal(err)
	}
	// a different subject on the same session: rejected...
	if err := subs.add("punk://memory/ns-a/z", ss, fakeSubjectGate{subject: "bob", allow: true}); err == nil {
		t.Fatal("rebinding a session's subscriptions to a different verified subject must be rejected")
	}
	// ...and it recorded nothing
	if subs.any("punk://memory/ns-a/z") {
		t.Fatal("rejected subscribe must not record a subscription")
	}
	// the creating subject's gate is untouched
	snap := subs.snapshot("punk://memory/ns-a/x")
	gate, ok := snap[ss].(fakeSubjectGate)
	if !ok || gate.subject != "alice" {
		t.Fatalf("creating subject's gate was replaced: %+v", snap[ss])
	}
	// trusted transports (no gate) never bind
	trusted := &mcp.ServerSession{}
	if err := subs.add("punk://tasks/T1", trusted, nil); err != nil {
		t.Fatal(err)
	}
	if err := subs.add("punk://tasks/T2", trusted, nil); err != nil {
		t.Fatal(err)
	}
}
