package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/authz"
	"github.com/hypervisor-io/punk-records/internal/bus"
	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/region"
	"github.com/hypervisor-io/punk-records/internal/store"
)

// A02 boundary enforcement proofs. Setup: enforcement on (authz.
// enforcement=deny semantics via Keys.SetAuthorizer), one valid
// credential for subject "alice" holding read+write on ns-a ONLY. Every
// attempt to reach namespace B (agent-ns-b, ns-b, or a query/cwd
// override resolving there) must be denied; the trusted disabled mode
// must stay compatible.

type authzBoundaryRig struct {
	s      *Server
	keys   *Keys
	az     *authz.Authorizer
	mem    *memory.Store
	bus    *bus.Bus
	region *region.Store
	token  string // alice: read+write on ns-a only
}

func authzBoundaryServer(t *testing.T, enforce bool) *authzBoundaryRig {
	t.Helper()
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "a02.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.MigrateUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	clk := time.Now().UTC()
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		clk = clk.Add(time.Millisecond)
		return clk
	}
	keys := NewKeys(db, now)
	rig := &authzBoundaryRig{
		keys: keys,
		mem:  memory.New(db, now),
		bus:  bus.New(),
		az:   authz.New(db, nil),
	}
	rig.region = region.New(db, nil)
	if enforce {
		keys.SetAuthorizer(rig.az)
	}
	rig.s = New(testLogger(), Deps{Memory: rig.mem, Keys: keys, Bus: rig.bus, Region: rig.region})
	rig.s.version = "vtest"
	if enforce {
		ctx := context.Background()
		token, err := keys.Create(ctx, "alice-key", "alice")
		if err != nil {
			t.Fatal(err)
		}
		if err := rig.az.Grant(ctx, "alice", "ns-a", authz.OpRead); err != nil {
			t.Fatal(err)
		}
		if err := rig.az.Grant(ctx, "alice", "ns-a", authz.OpWrite); err != nil {
			t.Fatal(err)
		}
		rig.token = token
	}
	return rig
}

func (g *authzBoundaryRig) do(t *testing.T, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if g.token != "" {
		req.Header.Set("Authorization", "Bearer "+g.token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	g.s.Router().ServeHTTP(rec, req)
	return rec
}

const hookSessionStart = `{"hook_event_name":"SessionStart","session_id":"s1","cwd":"/work/ns-b","source":"startup"}`

// TestAuthzBoundaryRESTDenials is the table-driven red proof: a valid
// A-only credential attempts B access on every namespace-scoped REST
// surface, including query-override namespaces. Every entry must be 403
// in enforcement mode.
func TestAuthzBoundaryRESTDenials(t *testing.T) {
	g := authzBoundaryServer(t, true)
	table := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		// path-namespaced routes (A01 middleware enforcement)
		{"recall", http.MethodGet, "/v1/namespaces/ns-b/memories", ""},
		{"remember", http.MethodPost, "/v1/namespaces/ns-b/memories", `{"key":"/k","body":"v"}`},
		{"forget", http.MethodDelete, "/v1/namespaces/ns-b/memories?key=/k", ""},
		{"search", http.MethodGet, "/v1/namespaces/ns-b/memories/search?q=x", ""},
		{"list_keys", http.MethodGet, "/v1/namespaces/ns-b/keys", ""},
		{"memory_events_sse", http.MethodGet, "/v1/namespaces/ns-b/events", ""},
		{"task_board", http.MethodGet, "/v1/namespaces/ns-b/tasks", ""},
		{"task_status", http.MethodPost, "/v1/namespaces/ns-b/tasks/T1/status", `{"state":"done","summary":"x"}`},
		{"profile", http.MethodGet, "/v1/namespaces/ns-b/profile", ""},
		{"diagnose", http.MethodGet, "/v1/namespaces/ns-b/diagnose", ""},
		// request-resolved namespaces (A02): query overrides and cwd
		// derivation must never grant access
		{"hook_query_override", http.MethodPost, "/v1/agent/hooks?ns=ns-b", `{"hook_event_name":"SessionStart","session_id":"s1","cwd":"/work/ok","source":"startup"}`},
		{"hook_cwd_derived", http.MethodPost, "/v1/agent/hooks", hookSessionStart},
		{"context_query_override", http.MethodGet, "/v1/agent/context?ns=ns-b", ""},
		{"context_cwd_derived", http.MethodGet, "/v1/agent/context?cwd=/work/ns-b", ""},
		{"context_turn_mode", http.MethodGet, "/v1/agent/context?ns=ns-b&mode=turn&q=x", ""},
	}
	for _, tc := range table {
		t.Run(tc.name, func(t *testing.T) {
			rec := g.do(t, tc.method, tc.path, tc.body, nil)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s %s = %d, want 403: %s", tc.method, tc.path, rec.Code, rec.Body.String())
			}
		})
	}

	// positive controls: the same credential keeps working on ns-a
	if rec := g.do(t, http.MethodPost, "/v1/agent/hooks?ns=ns-a", `{"hook_event_name":"SessionStart","session_id":"s1","cwd":"/work/ok","source":"startup"}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("hook on granted ns-a = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if rec := g.do(t, http.MethodGet, "/v1/agent/context?ns=ns-a", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("context on granted ns-a = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if rec := g.do(t, http.MethodGet, "/v1/namespaces/ns-a/keys", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("keys on granted ns-a = %d, want 200", rec.Code)
	}
}

// TestAuthzBoundaryHeadersNeverGrant: roots-style selection headers, a
// forged subject header, or an agent label must never widen access.
// Identity comes only from the verified credential.
func TestAuthzBoundaryHeadersNeverGrant(t *testing.T) {
	g := authzBoundaryServer(t, true)
	ctx := context.Background()
	// bob legitimately holds ns-b; alice's token must not inherit it
	// through a forged X-Punk-Subject header.
	if err := g.az.Grant(ctx, "bob", "ns-b", authz.OpRead); err != nil {
		t.Fatal(err)
	}
	forged := map[string]string{
		"X-Punk-Subject":   "bob",
		"X-Punk-Agent":     "bob",
		"X-Punk-Namespace": "ns-a",
	}
	if rec := g.do(t, http.MethodGet, "/v1/namespaces/ns-b/keys", "", forged); rec.Code != http.StatusForbidden {
		t.Fatalf("forged subject on ns-b = %d, want 403", rec.Code)
	}
	if rec := g.do(t, http.MethodGet, "/v1/agent/context?ns=ns-b", "", forged); rec.Code != http.StatusForbidden {
		t.Fatalf("forged subject on resolved ns-b = %d, want 403", rec.Code)
	}
	if rec := g.do(t, http.MethodPost, "/v1/agent/hooks?ns=ns-b", hookSessionStart, forged); rec.Code != http.StatusForbidden {
		t.Fatalf("forged subject on hook ns-b = %d, want 403", rec.Code)
	}
}

// TestAuthzBoundaryBrainSnapshotFiltered: the whole-server snapshot is a
// cross-region enumeration surface; enforcement mode reports only the
// namespaces the verified subject holds read grants on.
func TestAuthzBoundaryBrainSnapshotFiltered(t *testing.T) {
	g := authzBoundaryServer(t, true)
	ctx := context.Background()
	for _, ns := range []string{"ns-a", "ns-b", "ns-c"} {
		if _, err := g.mem.Write(ctx, memory.WriteInput{Namespace: ns, Key: "/k", Body: "v"}); err != nil {
			t.Fatal(err)
		}
	}
	rec := g.do(t, http.MethodGet, "/v1/brain/snapshot", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("snapshot = %d", rec.Code)
	}
	var snap brainSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, n := range snap.Namespaces {
		names = append(names, n.Name)
	}
	if len(names) != 1 || names[0] != "ns-a" {
		t.Fatalf("snapshot namespaces = %v, want [ns-a] only", names)
	}
	// granting read on ns-c widens exactly that namespace
	if err := g.az.Grant(ctx, "alice", "ns-c", authz.OpRead); err != nil {
		t.Fatal(err)
	}
	rec = g.do(t, http.MethodGet, "/v1/brain/snapshot", "", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	names = nil
	for _, n := range snap.Namespaces {
		names = append(names, n.Name)
	}
	if len(names) != 2 || names[0] != "ns-a" || names[1] != "ns-c" {
		t.Fatalf("post-grant snapshot = %v, want [ns-a ns-c]", names)
	}
}

// sseFrame is one parsed SSE frame, or the terminal read error (io.EOF
// when the server closed the stream).
type sseFrame struct {
	event, data string
	err         error
}

// streamHandle is an SSE stream whose frames are drained into a channel
// by a background goroutine, so a handler write never blocks on the
// test's assertion pacing (io.Pipe is synchronous).
type streamHandle struct {
	frames chan sseFrame
	cancel context.CancelFunc
	done   chan struct{}
}

// openStream starts an SSE request against the rig and drains frames in
// the background until the stream ends.
func (g *authzBoundaryRig) openStream(t *testing.T, path string) *streamHandle {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
	if g.token != "" {
		req.Header.Set("Authorization", "Bearer "+g.token)
	}
	pr, pw := io.Pipe()
	rec := &streamRecorder{ResponseRecorder: httptest.NewRecorder(), w: pw}
	done := make(chan struct{})
	go func() {
		g.s.Router().ServeHTTP(rec, req)
		_ = pw.Close()
		close(done)
	}()
	h := &streamHandle{frames: make(chan sseFrame, 64), cancel: cancel, done: done}
	go func() {
		defer close(h.frames)
		r := bufio.NewReader(pr)
		for {
			ev, data, err := readFrameOrEOF(r)
			h.frames <- sseFrame{event: ev, data: data, err: err}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { cancel(); <-done })
	return h
}

// readFrameOrEOF reads one SSE frame; io.EOF means the server closed the
// stream. Unlike the brain test helper it never fails the test itself,
// so it is safe inside goroutines.
func readFrameOrEOF(r *bufio.Reader) (event, data string, err error) {
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return "", "", err
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		case strings.HasPrefix(line, ": "):
			// keepalive comment
		case line == "" && event != "":
			return event, data, nil
		}
	}
}

// nextFrame returns the next delivered frame within d. ok=false means
// no frame arrived (timeout) or the stream ended (f.err carries the
// terminal error, io.EOF for a server-side close).
func (h *streamHandle) nextFrame(d time.Duration) (f sseFrame, ok bool) {
	select {
	case f, open := <-h.frames:
		if !open || f.err != nil {
			return f, false
		}
		return f, true
	case <-time.After(d):
		return sseFrame{}, false
	}
}

// closedWithin reports whether the stream ended with err (io.EOF for a
// server close) within d, without any further frame being delivered.
func (h *streamHandle) closedWithin(d time.Duration) (sseFrame, bool) {
	deadline := time.After(d)
	for {
		select {
		case f, open := <-h.frames:
			if !open {
				return sseFrame{err: io.EOF}, true
			}
			if f.err != nil {
				return f, true
			}
			// a frame arrived before the close: not closed-without-delivery
			return f, false
		case <-deadline:
			return sseFrame{}, false
		}
	}
}

// TestAuthzBoundaryBrainEventsFiltered: the brain event stream delivers
// only events whose source namespace the verified subject can read
// (namespace-less ledger events stay global, matching /v1/tasks), and
// revocation affects subsequent deliveries.
func TestAuthzBoundaryBrainEventsFiltered(t *testing.T) {
	g := authzBoundaryServer(t, true)
	h := g.openStream(t, "/v1/brain/events")

	if f, ok := h.nextFrame(3 * time.Second); !ok || f.event != "hello" {
		t.Fatalf("hello frame missing (ok=%v f=%+v)", ok, f)
	}
	g.bus.Publish(bus.Event{Kind: "memory", Key: "ns-a:/tasks/T1/status", Data: map[string]string{"action": "add"}})
	g.bus.Publish(bus.Event{Kind: "memory", Key: "ns-b:/tasks/T2/status", Data: map[string]string{"action": "add"}})
	g.bus.Publish(bus.Event{Kind: "task_status", Key: "task-9", Data: map[string]string{"status": "completed"}})

	f, ok := h.nextFrame(3 * time.Second)
	if !ok || f.event != "memory" || !strings.Contains(f.data, `"namespace":"ns-a"`) {
		t.Fatalf("first frame = %+v (ok=%v), want memory ns-a", f, ok)
	}
	f, ok = h.nextFrame(3 * time.Second)
	if !ok || f.event != "task_status" || !strings.Contains(f.data, `"key":"task-9"`) {
		t.Fatalf("second frame = %+v (ok=%v), want ledger task-9 (ns-b must be suppressed)", f, ok)
	}
	if f, ok := h.nextFrame(300 * time.Millisecond); ok {
		t.Fatalf("unexpected extra frame %+v: ns-b events must never be delivered", f)
	}

	// revocation takes effect on subsequent deliveries
	if err := g.az.Revoke(context.Background(), "alice", "ns-a", authz.OpRead); err != nil {
		t.Fatal(err)
	}
	g.bus.Publish(bus.Event{Kind: "memory", Key: "ns-a:/tasks/T3/status", Data: map[string]string{"action": "add"}})
	if f, ok := h.nextFrame(300 * time.Millisecond); ok {
		t.Fatalf("post-revoke frame %+v delivered, want none", f)
	}
}

// TestAuthzBoundaryMemoryEventsRevocationClosesStream: a namespace
// subscription (SSE) reauthorizes before every delivery; after revocation
// the stream closes and no further event is delivered.
func TestAuthzBoundaryMemoryEventsRevocationClosesStream(t *testing.T) {
	g := authzBoundaryServer(t, true)
	h := g.openStream(t, "/v1/namespaces/ns-a/events?prefix=/tasks/")

	// the handler subscribes to the bus asynchronously; retry-publish
	// until the first frame proves the subscription is live
	deadline := time.Now().Add(3 * time.Second)
	for {
		g.bus.Publish(bus.Event{Kind: "memory", Key: "ns-a:/tasks/T1/status", Data: map[string]string{"action": "add"}})
		if f, ok := h.nextFrame(200 * time.Millisecond); ok {
			if f.event != "memory" {
				t.Fatalf("pre-revoke frame = %+v, want memory", f)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no pre-revoke frame: subscription never went live")
		}
	}
	if err := g.az.Revoke(context.Background(), "alice", "ns-a", authz.OpRead); err != nil {
		t.Fatal(err)
	}
	g.bus.Publish(bus.Event{Kind: "memory", Key: "ns-a:/tasks/T2/status", Data: map[string]string{"action": "add"}})
	f, closed := h.closedWithin(3 * time.Second)
	if !closed {
		t.Fatalf("stream not closed after revocation (frame %+v): want close on next delivery", f)
	}
	if f.err != io.EOF {
		t.Fatalf("stream ended with err = %v, want io.EOF (server-side close)", f.err)
	}
}

// TestAuthzBoundaryMemoryEventsNoColonNamespaceLeak pins the A02-review
// fix: bus keys are namespace+":"+key and the SSE handler splits on the
// first ':', so a namespace literally named "ns-a:private" would be
// indistinguishable on the bus from "ns-a" under naive prefix matching
// - a subject with read on "ns-a" only would receive its events too.
// Two layers close it: such a namespace can never be created (no write
// to it ever reaches the bus), and the handler itself matches by exact
// namespace via splitBusKey, so even a hand-crafted bus event (standing
// in for a producer that bypasses memory.Write) is denied.
func TestAuthzBoundaryMemoryEventsNoColonNamespaceLeak(t *testing.T) {
	g := authzBoundaryServer(t, true)
	ctx := context.Background()

	if _, err := g.mem.Write(ctx, memory.WriteInput{
		Namespace: "ns-a:private", Key: "/secrets/x", Body: "leak",
	}); err == nil {
		t.Fatal("write to a namespace containing ':' should be rejected")
	}

	h := g.openStream(t, "/v1/namespaces/ns-a/events?prefix=/secrets/")

	// retry-publish until the subscription proves live, then confirm the
	// decoy (crafted directly on the bus, bypassing memory.Write) never
	// arrives: its parsed key "private:/secrets/x" does not start with
	// the requested "/secrets/" prefix.
	deadline := time.Now().Add(3 * time.Second)
	for {
		g.bus.Publish(bus.Event{Kind: "memory", Key: "ns-a:private:/secrets/x", Data: map[string]string{"action": "add"}})
		g.bus.Publish(bus.Event{Kind: "memory", Key: "ns-a:/secrets/y", Data: map[string]string{"action": "add"}})
		if f, ok := h.nextFrame(200 * time.Millisecond); ok {
			if f.event != "memory" || !strings.Contains(f.data, "/secrets/y") {
				t.Fatalf("frame = %+v, want the legitimate ns-a event", f)
			}
			if strings.Contains(f.data, "private") || strings.Contains(f.data, "/secrets/x") {
				t.Fatalf("decoy namespace leaked into ns-a stream: %+v", f)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no frame delivered: subscription never went live")
		}
	}
	if f, ok := h.nextFrame(300 * time.Millisecond); ok {
		t.Fatalf("unexpected extra frame %+v: decoy must never be delivered", f)
	}
}

// TestAuthzBoundaryTaskBoardWaitRevocation: revoking during a board
// long-poll denies the response instead of delivering the board.
func TestAuthzBoundaryTaskBoardWaitRevocation(t *testing.T) {
	g := authzBoundaryServer(t, true)
	recCh := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recCh <- g.do(t, http.MethodGet, "/v1/namespaces/ns-a/tasks?wait=5", "", nil)
	}()
	time.Sleep(200 * time.Millisecond)
	if err := g.az.Revoke(context.Background(), "alice", "ns-a", authz.OpRead); err != nil {
		t.Fatal(err)
	}
	g.bus.Publish(bus.Event{Kind: "memory", Key: "ns-a:/tasks/T1/status", Data: map[string]string{"action": "add"}})
	select {
	case rec := <-recCh:
		if rec.Code != http.StatusForbidden {
			t.Fatalf("board after mid-wait revocation = %d, want 403: %s", rec.Code, rec.Body.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("board request never answered")
	}
}

// TestAuthzBoundaryAgentContextProfileCardGated: the session-start block
// reads the global profile namespace; enforcement mode includes the card
// only when the verified subject holds read on it.
func TestAuthzBoundaryAgentContextProfileCardGated(t *testing.T) {
	g := authzBoundaryServer(t, true)
	ctx := context.Background()
	if _, err := g.mem.Write(ctx, memory.WriteInput{Namespace: ProfileNamespace, Key: "/profile/name", Body: "Alice Example"}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.mem.Write(ctx, memory.WriteInput{Namespace: "ns-a", Key: "/svc/db", Body: "primary is pg-1"}); err != nil {
		t.Fatal(err)
	}
	rec := g.do(t, http.MethodGet, "/v1/agent/context?ns=ns-a", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("context = %d", rec.Code)
	}
	var out agentContextOut
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.Context, "About the user") {
		t.Fatalf("profile card leaked without a user-profile grant: %q", out.Context)
	}
	if err := g.az.Grant(ctx, "alice", ProfileNamespace, authz.OpRead); err != nil {
		t.Fatal(err)
	}
	rec = g.do(t, http.MethodGet, "/v1/agent/context?ns=ns-a", "", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Context, "About the user") {
		t.Fatalf("profile card missing after grant: %q", out.Context)
	}
}

// TestAuthzBoundaryAgentContextReadOnlySkipsInjectedBookkeeping pins the
// A02-review fix: handleAgentContext is gated with OpRead, but it also
// writes /agent-sessions/<sid>/injected bookkeeping - a read-only
// subject must not cause that write. The context itself is still
// returned; only the bookkeeping is skipped.
func TestAuthzBoundaryAgentContextReadOnlySkipsInjectedBookkeeping(t *testing.T) {
	g := authzBoundaryServer(t, true)
	ctx := context.Background()
	if _, err := g.mem.Write(ctx, memory.WriteInput{
		Namespace: "ns-a", Key: "/decisions/x", Body: "auth uses jwt", Importance: 0.8,
	}); err != nil {
		t.Fatal(err)
	}
	bobToken, err := g.keys.Create(ctx, "bob-key", "bob")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.az.Grant(ctx, "bob", "ns-a", authz.OpRead); err != nil {
		t.Fatal(err)
	}
	bobAuth := map[string]string{"Authorization": "Bearer " + bobToken}

	// read-only: context comes back with the fact, but no bookkeeping
	// fact is written.
	rec := g.do(t, http.MethodGet, "/v1/agent/context?ns=ns-a&sid=s1", "", bobAuth)
	if rec.Code != http.StatusOK {
		t.Fatalf("context = %d: %s", rec.Code, rec.Body.String())
	}
	var out agentContextOut
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.FactIDs) == 0 {
		t.Fatalf("context returned no facts: %+v", out)
	}
	if facts, err := g.mem.Recall(ctx, "ns-a", "/agent-sessions/s1/injected", 1); err != nil {
		t.Fatal(err)
	} else if len(facts) != 0 {
		t.Fatalf("read-only subject caused a bookkeeping write: %+v", facts)
	}

	// write grant: the same subject's bookkeeping write now happens.
	if err := g.az.Grant(ctx, "bob", "ns-a", authz.OpWrite); err != nil {
		t.Fatal(err)
	}
	rec = g.do(t, http.MethodGet, "/v1/agent/context?ns=ns-a&sid=s2", "", bobAuth)
	if rec.Code != http.StatusOK {
		t.Fatalf("context = %d: %s", rec.Code, rec.Body.String())
	}
	if facts, err := g.mem.Recall(ctx, "ns-a", "/agent-sessions/s2/injected", 1); err != nil {
		t.Fatal(err)
	} else if len(facts) == 0 {
		t.Fatal("write-granted subject should still get the bookkeeping write")
	}
}

// TestAuthzBoundaryZeroKeyBootstrapDeniedResolvedNS: enforcement mode
// denies the zero-key bootstrap on request-resolved namespace routes too
// (no verified subject, deny-by-default).
func TestAuthzBoundaryZeroKeyBootstrapDeniedResolvedNS(t *testing.T) {
	g := authzBoundaryServer(t, true)
	// revoke alice's key so ZERO active keys remain: the middleware
	// reopens the bootstrap pass-through, but with enforcement on there
	// is no verified subject and deny-by-default applies to the
	// request-resolved namespace routes too
	if err := g.keys.Revoke(context.Background(), "alice-key"); err != nil {
		t.Fatal(err)
	}
	g.token = ""
	if rec := g.do(t, http.MethodPost, "/v1/agent/hooks?ns=ns-b", hookSessionStart, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("bootstrap hook = %d, want 403", rec.Code)
	}
	if rec := g.do(t, http.MethodGet, "/v1/agent/context?ns=ns-b", "", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("bootstrap context = %d, want 403", rec.Code)
	}
}

// TestAuthzBoundaryDisabledCompat: with no authorizer wired (the
// default), every surface keeps its legacy trusted behavior.
func TestAuthzBoundaryDisabledCompat(t *testing.T) {
	g := authzBoundaryServer(t, false)
	ctx := context.Background()
	token, err := g.keys.Create(ctx, "trusted", "alice")
	if err != nil {
		t.Fatal(err)
	}
	g.token = token
	if rec := g.do(t, http.MethodPost, "/v1/agent/hooks?ns=ns-b", `{"hook_event_name":"SessionStart","session_id":"s1","cwd":"/work/ok","source":"startup"}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("off-mode hook ns-b = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if rec := g.do(t, http.MethodGet, "/v1/agent/context?ns=ns-b", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("off-mode context ns-b = %d, want 200", rec.Code)
	}
	for _, ns := range []string{"ns-a", "ns-b"} {
		if _, err := g.mem.Write(ctx, memory.WriteInput{Namespace: ns, Key: "/k", Body: "v"}); err != nil {
			t.Fatal(err)
		}
	}
	rec := g.do(t, http.MethodGet, "/v1/brain/snapshot", "", nil)
	var snap brainSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Namespaces) != 2 {
		t.Fatalf("off-mode snapshot = %d namespaces, want both", len(snap.Namespaces))
	}
	// off-mode brain events deliver every namespace
	h := g.openStream(t, "/v1/brain/events")
	if _, ok := h.nextFrame(3 * time.Second); !ok {
		t.Fatal("off-mode hello missing")
	}
	g.bus.Publish(bus.Event{Kind: "memory", Key: "ns-b:/tasks/T2/status", Data: map[string]string{"action": "add"}})
	if f, ok := h.nextFrame(3 * time.Second); !ok || !strings.Contains(f.data, `"namespace":"ns-b"`) {
		t.Fatalf("off-mode ns-b frame = %+v (ok=%v), want delivery", f, ok)
	}
}

// region store sanity: the rig's region member/claim data participates
// in the snapshot filter (members/claims of unreadable namespaces must
// not leak through the brain surface).
func TestAuthzBoundarySnapshotHidesRegionData(t *testing.T) {
	g := authzBoundaryServer(t, true)
	ctx := context.Background()
	if _, err := g.mem.Write(ctx, memory.WriteInput{Namespace: "ns-b", Key: "/k", Body: "v"}); err != nil {
		t.Fatal(err)
	}
	if err := g.region.Register(ctx, "ns-b", "spy-satellite", "worker"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.region.ClaimWork(ctx, "ns-b", "/tasks/T1", "spy-satellite", time.Minute); err != nil {
		t.Fatal(err)
	}
	rec := g.do(t, http.MethodGet, "/v1/brain/snapshot", "", nil)
	if strings.Contains(rec.Body.String(), "spy-satellite") {
		t.Fatalf("unreadable region data leaked into snapshot: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"name":"ns-b"`) {
		t.Fatalf("unreadable namespace leaked into snapshot: %s", rec.Body.String())
	}
}

// TestAuthzBoundaryMemoryEventsKeyRevocationClosesStream: deliveries on
// a namespace SSE stream revalidate the CREDENTIAL, not just the grant
// (A02 round-2 review): revoking the API key that opened the stream -
// while another key stays active, so the server does not fall back to
// bootstrap - closes the stream on the next delivery even though the
// subject's namespace grants are untouched.
func TestAuthzBoundaryMemoryEventsKeyRevocationClosesStream(t *testing.T) {
	g := authzBoundaryServer(t, true)
	ctx := context.Background()
	if _, err := g.keys.Create(ctx, "other-key", "bob"); err != nil {
		t.Fatal(err)
	}
	h := g.openStream(t, "/v1/namespaces/ns-a/events?prefix=/tasks/")

	// the handler subscribes to the bus asynchronously; retry-publish
	// until the first frame proves the subscription is live
	deadline := time.Now().Add(3 * time.Second)
	for {
		g.bus.Publish(bus.Event{Kind: "memory", Key: "ns-a:/tasks/T1/status", Data: map[string]string{"action": "add"}})
		if f, ok := h.nextFrame(200 * time.Millisecond); ok {
			if f.event != "memory" {
				t.Fatalf("pre-revoke frame = %+v, want memory", f)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no pre-revoke frame: subscription never went live")
		}
	}
	if err := g.keys.Revoke(ctx, "alice-key"); err != nil {
		t.Fatal(err)
	}
	g.bus.Publish(bus.Event{Kind: "memory", Key: "ns-a:/tasks/T2/status", Data: map[string]string{"action": "add"}})
	f, closed := h.closedWithin(3 * time.Second)
	if !closed {
		t.Fatalf("stream not closed after key revocation (frame %+v): want close on next delivery", f)
	}
	if f.err != io.EOF {
		t.Fatalf("stream ended with err = %v, want io.EOF (server-side close)", f.err)
	}
}

// TestAuthzBoundaryBrainEventsKeyRevocationClosesStream: the aggregate
// brain stream revalidates the credential at every delivery too; a
// revoked API key ends the stream even though the subject's namespace
// grants are untouched.
func TestAuthzBoundaryBrainEventsKeyRevocationClosesStream(t *testing.T) {
	g := authzBoundaryServer(t, true)
	ctx := context.Background()
	if _, err := g.keys.Create(ctx, "other-key", "bob"); err != nil {
		t.Fatal(err)
	}
	h := g.openStream(t, "/v1/brain/events")
	if f, ok := h.nextFrame(3 * time.Second); !ok || f.event != "hello" {
		t.Fatalf("hello frame missing (ok=%v f=%+v)", ok, f)
	}
	// positive control: the granted namespace flows while the key is valid
	g.bus.Publish(bus.Event{Kind: "memory", Key: "ns-a:/tasks/T1/status", Data: map[string]string{"action": "add"}})
	if f, ok := h.nextFrame(3 * time.Second); !ok || f.event != "memory" {
		t.Fatalf("pre-revoke frame = %+v (ok=%v), want memory", f, ok)
	}
	if err := g.keys.Revoke(ctx, "alice-key"); err != nil {
		t.Fatal(err)
	}
	g.bus.Publish(bus.Event{Kind: "memory", Key: "ns-a:/tasks/T2/status", Data: map[string]string{"action": "add"}})
	f, closed := h.closedWithin(3 * time.Second)
	if !closed {
		t.Fatalf("stream not closed after key revocation (frame %+v)", f)
	}
	if f.err != io.EOF {
		t.Fatalf("stream ended with err = %v, want io.EOF (server-side close)", f.err)
	}
}

// TestAuthzBoundaryBrainEventsKeyRevocationOnKeepalive pins the A02-
// review fix: handleBrainEvents only revalidated the credential when an
// event arrived, so a revoked key on an otherwise-idle stream kept it
// open indefinitely. The keepalive tick must revalidate too and end the
// stream when the credential is no longer active.
func TestAuthzBoundaryBrainEventsKeyRevocationOnKeepalive(t *testing.T) {
	oldKeepalive := brainKeepalive
	brainKeepalive = 50 * time.Millisecond
	t.Cleanup(func() { brainKeepalive = oldKeepalive })

	g := authzBoundaryServer(t, true)
	ctx := context.Background()
	if _, err := g.keys.Create(ctx, "other-key", "bob"); err != nil {
		t.Fatal(err)
	}
	h := g.openStream(t, "/v1/brain/events")
	if f, ok := h.nextFrame(3 * time.Second); !ok || f.event != "hello" {
		t.Fatalf("hello frame missing (ok=%v f=%+v)", ok, f)
	}
	if err := g.keys.Revoke(ctx, "alice-key"); err != nil {
		t.Fatal(err)
	}
	// idle stream: no event published, only the keepalive ticks - the
	// stream must still close on the next tick.
	f, closed := h.closedWithin(3 * time.Second)
	if !closed {
		t.Fatalf("stream not closed after key revocation on an idle stream (frame %+v)", f)
	}
	if f.err != io.EOF {
		t.Fatalf("stream ended with err = %v, want io.EOF (server-side close)", f.err)
	}
}

// TestAuthzBoundaryTaskBoardWaitKeyRevocation: revoking the API key
// during a board long-poll answers 401 instead of a fresh board, even
// though the namespace grant is untouched (grant revocation answers
// 403; see TestAuthzBoundaryTaskBoardWaitRevocation).
func TestAuthzBoundaryTaskBoardWaitKeyRevocation(t *testing.T) {
	g := authzBoundaryServer(t, true)
	ctx := context.Background()
	if _, err := g.keys.Create(ctx, "other-key", "bob"); err != nil {
		t.Fatal(err)
	}
	recCh := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recCh <- g.do(t, http.MethodGet, "/v1/namespaces/ns-a/tasks?wait=5", "", nil)
	}()
	time.Sleep(200 * time.Millisecond)
	if err := g.keys.Revoke(ctx, "alice-key"); err != nil {
		t.Fatal(err)
	}
	g.bus.Publish(bus.Event{Kind: "memory", Key: "ns-a:/tasks/T1/status", Data: map[string]string{"action": "add"}})
	select {
	case rec := <-recCh:
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("board after mid-wait key revocation = %d, want 401: %s", rec.Code, rec.Body.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("board request never answered")
	}
}
