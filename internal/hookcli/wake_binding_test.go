package hookcli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
)

// Dynamic inbox binding (2026-09-28 design, T2), wake half: ensure
// re-resolves the namespace on EVERY hook event and passes the session
// address, so a moved binding changes the fingerprint, supersedes the
// old generation and respawns the listener on the bound namespace - no
// runner change, no restart.

// wakeBindingServer answers /v1/agent/namespace from a mutable binding
// and records every raw query.
type wakeBindingServer struct {
	mu      sync.Mutex
	ns      string
	queries []string
}

func newWakeBindingServer(t *testing.T, ns string) (*wakeBindingServer, *httptest.Server) {
	t.Helper()
	b := &wakeBindingServer{ns: ns}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/agent/namespace" {
			http.NotFound(w, r)
			return
		}
		b.mu.Lock()
		b.queries = append(b.queries, r.URL.RawQuery)
		cur := b.ns
		b.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"namespace": cur})
	}))
	t.Cleanup(srv.Close)
	return b, srv
}

func (b *wakeBindingServer) move(ns string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ns = ns
}

func (b *wakeBindingServer) rawQueries() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.queries...)
}

func TestWakeEnsureBindingMoveRespawns(t *testing.T) {
	wakeTestEnv(t)
	claudeSocketFixture(t)
	rec := &spawnRecorder{}
	rec.install(t)
	b, srv := newWakeBindingServer(t, "ns-a")

	opts := wakeOpts("claude-code", wakeActionEnsure, "") // no --ns pin
	opts.BaseURL = srv.URL
	wakeRunErr(t, opts, wakeStdin("sess-1", "/work/x"))
	_, first := rec.last(t)
	if first.Namespace != "ns-a" {
		t.Fatalf("first ensure resolved %q, want the bound ns-a", first.Namespace)
	}
	dir := wakeSessionDir("claude-code", "sess-1")

	// The binding moves (the agent re-registered elsewhere); the next
	// hook event must re-resolve, produce a new fingerprint and respawn.
	b.move("ns-b")
	wakeRunErr(t, opts, wakeStdin("sess-1", "/work/x"))
	if rec.calls() != 2 {
		t.Fatalf("a moved binding must respawn the listener, got %d spawns", rec.calls())
	}
	argv, second := rec.last(t)
	if second.Namespace != "ns-b" {
		t.Fatalf("replacement config still on %q", second.Namespace)
	}
	if !strings.Contains(strings.Join(argv, " "), "--ns ns-b") {
		t.Fatalf("replacement worker argv not on the new namespace: %v", argv)
	}
	if first.Generation == second.Generation {
		t.Fatal("a moved binding must mint a fresh generation")
	}
	if _, err := os.Stat(first.ControlPath); !os.IsNotExist(err) {
		t.Fatal("the superseded generation's marker survived the binding move")
	}
	if !wakeMarkerCurrent(dir, second.Generation) {
		t.Fatal("the new generation is not current after the binding move")
	}
	lrec, ok := wakeReadListener(dir)
	if !ok || lrec.Namespace != "ns-b" || lrec.Generation != second.Generation {
		t.Fatalf("listener record after binding move: %+v ok=%v", lrec, ok)
	}

	// Every ensure re-resolved through the server with the session
	// address on the query (never a cached namespace).
	qs := b.rawQueries()
	if len(qs) != 2 {
		t.Fatalf("ensure must re-resolve on every event, saw %d lookups: %v", len(qs), qs)
	}
	for _, q := range qs {
		vals, err := url.ParseQuery(q)
		if err != nil {
			t.Fatalf("lookup query %q: %v", q, err)
		}
		if vals.Get("agent") != "claude-code:sess-1" {
			t.Fatalf("wake lookup must carry the session address: %q", q)
		}
	}

	// An unchanged binding re-resolves again (a third lookup) but dedups
	// on the fingerprint: no respawn.
	wakeRunErr(t, opts, wakeStdin("sess-1", "/work/x"))
	if rec.calls() != 2 {
		t.Fatalf("unchanged binding respawned: %d spawns", rec.calls())
	}
	if qs := b.rawQueries(); len(qs) != 3 {
		t.Fatalf("ensure must look the binding up even when it dedups, saw %v", qs)
	}
}

// A --ns pin keeps wake ensure fully local: no binding query at all.
func TestWakeEnsurePinSkipsBindingQuery(t *testing.T) {
	wakeTestEnv(t)
	claudeSocketFixture(t)
	rec := &spawnRecorder{}
	rec.install(t)
	b, srv := newWakeBindingServer(t, "ns-a")

	opts := wakeOpts("claude-code", wakeActionEnsure, "ns-pin")
	opts.BaseURL = srv.URL
	wakeRunErr(t, opts, wakeStdin("sess-1", "/work/x"))
	_, cc := rec.last(t)
	if cc.Namespace != "ns-pin" {
		t.Fatalf("pinned ensure resolved %q", cc.Namespace)
	}
	if qs := b.rawQueries(); len(qs) != 0 {
		t.Fatalf("a pinned ensure must not spend a binding query, saw %v", qs)
	}
}
