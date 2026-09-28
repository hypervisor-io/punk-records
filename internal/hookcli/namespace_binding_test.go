package hookcli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// Dynamic inbox binding (2026-09-28 design, T2): hook-side namespace
// resolution order is --ns > PUNK_NAMESPACE > the server's inbox binding
// for the session address > cwd-derived. Pins never spend a request;
// without pins the session address rides the /v1/agent/namespace query
// so the server can apply a binding, and a server without one answers
// exactly what it answers today.

// bindingNSServer is a fake /v1/agent/namespace endpoint that answers a
// per-agent binding when the query carries a known agent, else the
// cwd-derived namespace, and records every raw query it saw.
type bindingNSServer struct {
	mu       sync.Mutex
	bindings map[string]string
	queries  []string // raw query strings, in order
}

func newBindingNSServer(t *testing.T, bindings map[string]string) (*bindingNSServer, *httptest.Server) {
	t.Helper()
	b := &bindingNSServer{bindings: bindings}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/agent/namespace" {
			http.NotFound(w, r)
			return
		}
		b.mu.Lock()
		b.queries = append(b.queries, r.URL.RawQuery)
		b.mu.Unlock()
		ns := "agent-cwdderived"
		if bound, ok := b.bindings[r.URL.Query().Get("agent")]; ok {
			ns = bound
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"namespace": ns})
	}))
	t.Cleanup(srv.Close)
	return b, srv
}

func (b *bindingNSServer) rawQueries() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.queries...)
}

// resetNamespacePins neutralises every pin so resolution must consult
// the server.
func resetNamespacePins(t *testing.T) {
	t.Helper()
	t.Setenv("PUNK_NAMESPACE", "")
	SetNamespaceOverride("")
	t.Cleanup(func() { SetNamespaceOverride("") })
}

func TestResolveNamespaceFlagPinWinsAndSkipsQuery(t *testing.T) {
	resetNamespacePins(t)
	b, srv := newBindingNSServer(t, map[string]string{"fake:s1": "bound-ns"})
	ns, err := resolveInboxNamespace(InboxOpts{Namespace: "pin-ns", BaseURL: srv.URL}, "/work/x", "fake:s1")
	if err != nil || ns != "pin-ns" {
		t.Fatalf("--ns pin: ns=%q err=%v", ns, err)
	}
	if q := b.rawQueries(); len(q) != 0 {
		t.Fatalf("a pinned namespace must not spend a binding query, saw %v", q)
	}
}

func TestResolveNamespaceEnvPinWinsAndSkipsQuery(t *testing.T) {
	resetNamespacePins(t)
	t.Setenv("PUNK_NAMESPACE", "env-ns")
	b, srv := newBindingNSServer(t, map[string]string{"fake:s1": "bound-ns"})
	ns, err := resolveInboxNamespace(InboxOpts{BaseURL: srv.URL}, "/work/x", "fake:s1")
	if err != nil || ns != "env-ns" {
		t.Fatalf("PUNK_NAMESPACE pin: ns=%q err=%v", ns, err)
	}
	if q := b.rawQueries(); len(q) != 0 {
		t.Fatalf("PUNK_NAMESPACE must not spend a binding query, saw %v", q)
	}
}

func TestResolveNamespaceOverridePinWinsAndSkipsQuery(t *testing.T) {
	resetNamespacePins(t)
	SetNamespaceOverride("override-ns")
	b, srv := newBindingNSServer(t, map[string]string{"fake:s1": "bound-ns"})
	ns, err := resolveInboxNamespace(InboxOpts{BaseURL: srv.URL}, "/work/x", "fake:s1")
	if err != nil || ns != "override-ns" {
		t.Fatalf("namespace override pin: ns=%q err=%v", ns, err)
	}
	if q := b.rawQueries(); len(q) != 0 {
		t.Fatalf("the namespace override must not spend a binding query, saw %v", q)
	}
}

func TestResolveNamespaceBindingBeatsCwd(t *testing.T) {
	resetNamespacePins(t)
	b, srv := newBindingNSServer(t, map[string]string{"fake:s1": "bound-ns"})
	ns, err := resolveInboxNamespace(InboxOpts{BaseURL: srv.URL}, "/work/x", "fake:s1")
	if err != nil || ns != "bound-ns" {
		t.Fatalf("server binding must beat cwd derivation: ns=%q err=%v", ns, err)
	}
	q := b.rawQueries()
	if len(q) != 1 {
		t.Fatalf("expected exactly one binding query, saw %v", q)
	}
	vals, verr := url.ParseQuery(q[0])
	if verr != nil {
		t.Fatalf("query %q does not parse: %v", q[0], verr)
	}
	if vals.Get("agent") != "fake:s1" || vals.Get("cwd") != "/work/x" {
		t.Fatalf("binding query must carry agent and cwd: %q", q[0])
	}
}

func TestResolveNamespaceNoBindingFallsBackToCwd(t *testing.T) {
	resetNamespacePins(t)
	// The server has no binding for this address: resolution must land
	// on the same cwd-derived answer it produces today.
	b, srv := newBindingNSServer(t, map[string]string{"fake:other": "bound-ns"})
	withAgent, err := resolveInboxNamespace(InboxOpts{BaseURL: srv.URL}, "/work/x", "fake:s1")
	if err != nil {
		t.Fatal(err)
	}
	withoutAgent, err := resolveInboxNamespace(InboxOpts{BaseURL: srv.URL}, "/work/x", "")
	if err != nil {
		t.Fatal(err)
	}
	if withAgent != "agent-cwdderived" || withAgent != withoutAgent {
		t.Fatalf("no binding must be identical to today: with agent %q, without %q", withAgent, withoutAgent)
	}
	if q := b.rawQueries(); len(q) != 2 {
		t.Fatalf("expected both lookups to hit the server, saw %v", q)
	}
}

func TestResolveNamespaceEmptyAgentQueryByteIdentical(t *testing.T) {
	resetNamespacePins(t)
	b, srv := newBindingNSServer(t, nil)
	cwd := "/work/my proj"
	if _, err := resolveInboxNamespace(InboxOpts{BaseURL: srv.URL}, cwd, ""); err != nil {
		t.Fatal(err)
	}
	q := b.rawQueries()
	want := "cwd=" + url.QueryEscape(cwd)
	if len(q) != 1 || q[0] != want {
		t.Fatalf("an empty agent must leave the query byte-identical to today: got %v want %q", q, want)
	}
}

// The inbox hook passes its parsed session address on the lookup when no
// pin fired, so the server can apply that address's binding.
func TestInboxHookSendsSessionAddressForBinding(t *testing.T) {
	inboxTestEnv(t)
	f, srv := newFakeInbox(t)
	fakeReplyClient(t, "fake", false, "MIN")
	runInbox(t, InboxOpts{Client: "fake", BaseURL: srv.URL, APIKey: "k"}, fakeStdin)
	f.mu.Lock()
	defer f.mu.Unlock()
	var lookup string
	for _, req := range f.requests {
		if strings.HasPrefix(req, "GET /v1/agent/namespace?") {
			lookup = req
		}
	}
	if lookup == "" {
		t.Fatalf("no namespace lookup among %v", f.requests)
	}
	raw := strings.TrimPrefix(lookup, "GET /v1/agent/namespace?")
	vals, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("lookup query %q: %v", raw, err)
	}
	if vals.Get("agent") != "fake:s1" {
		t.Fatalf("lookup must carry the session address: %q", raw)
	}
	if vals.Get("cwd") != "/work/proj" {
		t.Fatalf("lookup lost the cwd: %q", raw)
	}
}

// A pinned inbox hook never spends the binding query.
func TestInboxHookPinSkipsBindingQuery(t *testing.T) {
	inboxTestEnv(t)
	f, srv := newFakeInbox(t)
	fakeReplyClient(t, "fake", false, "MIN")
	runInbox(t, InboxOpts{Client: "fake", BaseURL: srv.URL, APIKey: "k", Namespace: "pin-ns"}, fakeStdin)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, req := range f.requests {
		if strings.HasPrefix(req, "GET /v1/agent/namespace") {
			t.Fatalf("a pinned hook must not resolve through the server, saw %q", req)
		}
	}
}
