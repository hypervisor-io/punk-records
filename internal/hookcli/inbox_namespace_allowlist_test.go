package hookcli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// P3-D: the optional local PUNK_NAMESPACES allowlist. When set (comma-
// separated namespaces), the hook refuses to FOLLOW a server inbox
// binding whose namespace is outside the list and falls back to the
// cwd-derived namespace the server sent alongside the binding. That is
// the fail-open shape the surrounding code dictates: refusing a binding
// never blocks the event or stops delivery, it only declines the
// rewire - the hook keeps working on the default namespace. Pins
// (--ns, PUNK_NAMESPACE, the connect-baked override) are explicit local
// intent and are never gated. No authz is involved: this is a client-
// local decision on data the server already answered, not server
// enforcement.

// boundNSServer is a /v1/agent/namespace fake answering the P3-D bound
// shape: {"namespace", "bound", "set_by", "set_at", "cwd_namespace"}.
type boundNSServer struct {
	boundNS string // namespace the binding points at
	setBy   string
	setAt   string
	cwdNS   string // cwd-derived fallback sent alongside the binding
	withCwd bool   // false: omit cwd_namespace (the defensive-refusal case)
}

func newBoundNSServer(t *testing.T, b boundNSServer) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/agent/namespace" {
			http.NotFound(w, r)
			return
		}
		out := map[string]any{"namespace": b.boundNS, "bound": true, "set_at": b.setAt}
		if b.setBy != "" {
			out["set_by"] = b.setBy
		}
		if b.withCwd {
			out["cwd_namespace"] = b.cwdNS
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

const boundTestTime = "2026-10-27T12:00:00.000000000Z"

func TestResolveNamespaceReturnsBindingInfo(t *testing.T) {
	resetNamespacePins(t)
	t.Setenv("PUNK_NAMESPACES", "")
	srv := newBoundNSServer(t, boundNSServer{boundNS: "bound-ns", setBy: "codex:operator",
		setAt: boundTestTime, cwdNS: "agent-derived", withCwd: true})
	ns, bound, err := resolveInboxNamespace(InboxOpts{BaseURL: srv.URL}, "/work/x", "fake:s1")
	if err != nil || ns != "bound-ns" {
		t.Fatalf("ns=%q err=%v, want the bound namespace", ns, err)
	}
	if bound == nil || bound.Namespace != "bound-ns" || bound.SetBy != "codex:operator" || bound.SetAt != boundTestTime {
		t.Fatalf("binding info = %+v, want the stored setter and time", bound)
	}
}

// Acceptance: the hook refuses a binding outside the local allowlist
// and falls back to the default (cwd-derived) namespace.
func TestResolveNamespaceAllowlistRefusesNonListedBinding(t *testing.T) {
	resetNamespacePins(t)
	t.Setenv("PUNK_NAMESPACES", "ns-a,ns-b")
	srv := newBoundNSServer(t, boundNSServer{boundNS: "ns-out", setBy: "codex:operator",
		setAt: boundTestTime, cwdNS: "agent-derived", withCwd: true})
	ns, bound, err := resolveInboxNamespace(InboxOpts{BaseURL: srv.URL}, "/work/x", "fake:s1")
	if err != nil {
		t.Fatal(err)
	}
	if ns != "agent-derived" || bound != nil {
		t.Fatalf("ns=%q bound=%+v, want the cwd fallback and no binding followed", ns, bound)
	}
}

func TestResolveNamespaceAllowlistFollowsListedBinding(t *testing.T) {
	resetNamespacePins(t)
	t.Setenv("PUNK_NAMESPACES", " ns-a , ns-b ") // entries are trimmed
	srv := newBoundNSServer(t, boundNSServer{boundNS: "ns-a", setBy: "codex:operator",
		setAt: boundTestTime, cwdNS: "agent-derived", withCwd: true})
	ns, bound, err := resolveInboxNamespace(InboxOpts{BaseURL: srv.URL}, "/work/x", "fake:s1")
	if err != nil || ns != "ns-a" {
		t.Fatalf("ns=%q err=%v, want the listed binding followed", ns, err)
	}
	if bound == nil || bound.SetBy != "codex:operator" || bound.SetAt != boundTestTime {
		t.Fatalf("binding info = %+v, want the stored setter and time", bound)
	}
}

func TestResolveNamespaceAllowlistUnsetFollowsAnyBinding(t *testing.T) {
	resetNamespacePins(t)
	t.Setenv("PUNK_NAMESPACES", "") // unset: no enforcement
	srv := newBoundNSServer(t, boundNSServer{boundNS: "ns-out", setBy: "codex:operator",
		setAt: boundTestTime, cwdNS: "agent-derived", withCwd: true})
	ns, bound, err := resolveInboxNamespace(InboxOpts{BaseURL: srv.URL}, "/work/x", "fake:s1")
	if err != nil || ns != "ns-out" || bound == nil {
		t.Fatalf("ns=%q bound=%+v err=%v, want the binding followed when no allowlist is set", ns, bound, err)
	}
}

// A set-but-empty list (",,") is an explicitly empty allowlist: it
// admits no namespace, so every binding is refused. Garbage never
// widens what the hook follows.
func TestResolveNamespaceAllowlistEmptyEntriesRefuseAll(t *testing.T) {
	resetNamespacePins(t)
	t.Setenv("PUNK_NAMESPACES", ",,")
	srv := newBoundNSServer(t, boundNSServer{boundNS: "ns-out", setBy: "codex:operator",
		setAt: boundTestTime, cwdNS: "agent-derived", withCwd: true})
	ns, bound, err := resolveInboxNamespace(InboxOpts{BaseURL: srv.URL}, "/work/x", "fake:s1")
	if err != nil || ns != "agent-derived" || bound != nil {
		t.Fatalf("ns=%q bound=%+v err=%v, want the cwd fallback", ns, bound, err)
	}
}

// A refused binding whose server answer carries no usable cwd fallback
// is an error, which lands in the hook's existing fail-open path (the
// event is never blocked; the prompt flow continues without punk).
func TestResolveNamespaceAllowlistRefusalWithoutFallbackFails(t *testing.T) {
	resetNamespacePins(t)
	t.Setenv("PUNK_NAMESPACES", "ns-a")
	srv := newBoundNSServer(t, boundNSServer{boundNS: "ns-out", setBy: "codex:operator", setAt: boundTestTime})
	if _, _, err := resolveInboxNamespace(InboxOpts{BaseURL: srv.URL}, "/work/x", "fake:s1"); err == nil {
		t.Fatal("a refused binding with no cwd fallback must error into the hook's fail-open path")
	}
}

func TestResolveNamespaceAllowlistNeverGatesPins(t *testing.T) {
	resetNamespacePins(t)
	t.Setenv("PUNK_NAMESPACES", "ns-a")
	srv := newBoundNSServer(t, boundNSServer{boundNS: "ns-out", setBy: "codex:operator",
		setAt: boundTestTime, cwdNS: "agent-derived", withCwd: true})
	ns, bound, err := resolveInboxNamespace(InboxOpts{Namespace: "pin-ns", BaseURL: srv.URL}, "/work/x", "fake:s1")
	if err != nil || ns != "pin-ns" || bound != nil {
		t.Fatalf("ns=%q bound=%+v err=%v, want the pin ungated and unbound", ns, bound, err)
	}
}

// fakeGuidanceClient registers a minimal client whose reply surfaces
// the hook-authored guidance text as JSON, so an end-to-end assertion
// can read the routing block the whole hook run produced.
func fakeGuidanceClient(t *testing.T, name string) {
	t.Helper()
	withInboxClient(t, InboxClient{
		Name:           name,
		Parse:          parseSessionIDPayload,
		GuidanceEvents: []string{"UserPromptSubmit"},
		MaxRenderBytes: 32 * 1024,
		Reply: func(req InboxReplyRequest) InboxReply {
			raw, _ := json.Marshal(map[string]any{"text": req.Delivery.Guidance + req.Delivery.Rendered})
			return InboxReply{Out: append(raw, '\n'), Delivered: true}
		},
	})
}

// End to end: with PUNK_NAMESPACES set, the WHOLE hook run refuses the
// non-listed binding - registration and fetch stay on the cwd-derived
// namespace and the routing block carries no binding line.
func TestInboxHookRefusesNonAllowlistedBinding(t *testing.T) {
	inboxTestEnv(t)
	t.Setenv("PUNK_NAMESPACES", "ns-a")
	f, srv := newFakeInbox(t)
	f.setBoundNamespace("ns-out", "codex:operator", boundTestTime, "agent-derived")
	fakeGuidanceClient(t, "fakeg")
	out, _ := runInbox(t, InboxOpts{Client: "fakeg", Mode: "context", BaseURL: srv.URL, APIKey: "k"}, fakeStdin)

	f.mu.Lock()
	requests := append([]string(nil), f.requests...)
	f.mu.Unlock()
	var sawFallback, sawRefused bool
	for _, req := range requests {
		if strings.Contains(req, "/v1/namespaces/agent-derived/") {
			sawFallback = true
		}
		if strings.Contains(req, "/v1/namespaces/ns-out/") {
			sawRefused = true
		}
	}
	if !sawFallback || sawRefused {
		t.Fatalf("fallback=%v refused=%v requests=%v, want every call on agent-derived, none on ns-out",
			sawFallback, sawRefused, requests)
	}
	text := jsonText(out)
	if !strings.Contains(text, "in namespace agent-derived") {
		t.Fatalf("routing block missing the fallback namespace: %q", out)
	}
	if strings.Contains(text, "Inbox bound to") {
		t.Fatalf("refused binding leaked a binding line: %q", out)
	}
}

// End to end, listed binding: the hook follows it and the routing block
// states who bound the inbox, to what, and when.
func TestInboxHookFollowsListedBindingAndRendersLine(t *testing.T) {
	inboxTestEnv(t)
	t.Setenv("PUNK_NAMESPACES", "ns-out")
	f, srv := newFakeInbox(t)
	f.setBoundNamespace("ns-out", "codex:operator", boundTestTime, "agent-derived")
	fakeGuidanceClient(t, "fakeg")
	out, _ := runInbox(t, InboxOpts{Client: "fakeg", Mode: "context", BaseURL: srv.URL, APIKey: "k"}, fakeStdin)

	f.mu.Lock()
	requests := append([]string(nil), f.requests...)
	f.mu.Unlock()
	var sawBound bool
	for _, req := range requests {
		if strings.Contains(req, "/v1/namespaces/ns-out/") {
			sawBound = true
		}
	}
	if !sawBound {
		t.Fatalf("listed binding not followed, requests=%v", requests)
	}
	text := jsonText(out)
	want := "Inbox bound to ns-out by codex:operator at " + boundTestTime + "."
	if !strings.Contains(text, want) {
		t.Fatalf("routing block missing %q: %q", want, out)
	}
}
