package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/bus"
	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/store"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.MigrateUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The clock closure mutates clk, and the node round-trip tests drive the
	// router over a real httptest server whose handlers run concurrently, so
	// the bump must be locked or `go test -race` reports the write.
	var clkMu sync.Mutex
	clk := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	now := func() time.Time {
		clkMu.Lock()
		defer clkMu.Unlock()
		clk = clk.Add(time.Millisecond)
		return clk
	}
	return New(testLogger(), Deps{Memory: memory.New(db, now)})
}

func do(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	rr := httptest.NewRecorder()
	s.Router().ServeHTTP(rr, r)
	return rr
}

func TestMemoryEndpoints(t *testing.T) {
	s := testServer(t)

	rr := do(t, s, http.MethodPost, "/v1/namespaces/susanoo/memories",
		`{"key":"/svc/db","body":"primary is pg-1","author":"tester"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("remember = %d: %s", rr.Code, rr.Body)
	}

	rr = do(t, s, http.MethodGet, "/v1/namespaces/susanoo/memories?prefix=/svc", "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "pg-1") {
		t.Fatalf("recall = %d: %s", rr.Code, rr.Body)
	}

	rr = do(t, s, http.MethodGet, "/v1/namespaces/susanoo/keys?prefix=/", "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "/svc/db") {
		t.Fatalf("keys = %d: %s", rr.Code, rr.Body)
	}

	rr = do(t, s, http.MethodGet, "/v1/namespaces/susanoo/memories/search?q=primary", "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "/svc/db") {
		t.Fatalf("search = %d: %s", rr.Code, rr.Body)
	}

	rr = do(t, s, http.MethodGet, "/v1/namespaces/susanoo/memories/search?q=primary&mode=hybrid&scored=1", "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"score_components"`) {
		t.Fatalf("scored search = %d: %s", rr.Code, rr.Body)
	}

	rr = do(t, s, http.MethodDelete, "/v1/namespaces/susanoo/memories?key=/svc/db", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("forget = %d: %s", rr.Code, rr.Body)
	}
	rr = do(t, s, http.MethodDelete, "/v1/namespaces/susanoo/memories?key=/svc/db", "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("double forget = %d, want 404", rr.Code)
	}

	rr = do(t, s, http.MethodPost, "/v1/namespaces/susanoo/memories", `{"key":"no-slash","body":"x"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bad key = %d, want 400", rr.Code)
	}
}

func TestSearchWindowed(t *testing.T) {
	s := testServer(t)
	do(t, s, http.MethodPost, "/v1/namespaces/susanoo/memories",
		`{"key":"/svc/db","body":"primary is pg-1","author":"tester"}`)

	since := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	until := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	rr := do(t, s, http.MethodGet,
		"/v1/namespaces/susanoo/memories/search?q=primary&since="+since+"&until="+until, "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "pg-1") {
		t.Fatalf("windowed search = %d: %s", rr.Code, rr.Body)
	}

	// since without until: 400
	rr = do(t, s, http.MethodGet, "/v1/namespaces/susanoo/memories/search?q=primary&since="+since, "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("since without until = %d, want 400", rr.Code)
	}

	// until strictly after since is required
	rr = do(t, s, http.MethodGet,
		"/v1/namespaces/susanoo/memories/search?q=primary&since="+until+"&until="+since, "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("until before since = %d, want 400", rr.Code)
	}
}

// TestSearchTemporalDoesNotHijackHybrid is the regression test for the
// auto-route bug: a query containing a recognized temporal phrase (here a
// bare 4-digit year) must not silently fall through to plain FTS
// WindowedSearch when the caller asked for mode=hybrid or fusion=interleave
// — those callers keep their chosen (scored/vector-fused) path.
func TestSearchTemporalDoesNotHijackHybrid(t *testing.T) {
	s := testServer(t)
	do(t, s, http.MethodPost, "/v1/namespaces/susanoo/memories",
		`{"key":"/svc/db","body":"primary is pg-1 in 2026","author":"tester"}`)

	rr := do(t, s, http.MethodGet,
		"/v1/namespaces/susanoo/memories/search?q=primary+2026&mode=hybrid&scored=1", "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"score_components"`) {
		t.Fatalf("hybrid search with year in query = %d: %s (want scored hybrid path, not plain WindowedSearch)", rr.Code, rr.Body)
	}
}

// TestSearchStrategyEnvelope covers R01's routed surface: strategy=auto
// routes identifier text (a version year) to exact instead of the legacy
// temporal window, an explicit strategy wins over temporal language, and
// an unknown strategy is a 400. With no strategy param the legacy path
// must stay byte-identical: a bare fact array, no envelope.
func TestSearchStrategyEnvelope(t *testing.T) {
	s := testServer(t)
	do(t, s, http.MethodPost, "/v1/namespaces/susanoo/memories",
		`{"key":"/svc/api","body":"release v2024.1 shipped fixes","author":"tester"}`)

	// Legacy default: plain array of facts, no envelope keys.
	rr := do(t, s, http.MethodGet, "/v1/namespaces/susanoo/memories/search?q=release+v2024.1", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("legacy search = %d: %s", rr.Code, rr.Body)
	}
	var legacy []memory.Fact
	if err := json.Unmarshal(rr.Body.Bytes(), &legacy); err != nil || len(legacy) != 1 {
		t.Fatalf("legacy search body = %s (err %v), want a bare 1-fact array", rr.Body, err)
	}

	// strategy=auto: the identifier guard keeps "v2024.1" out of temporal.
	rr = do(t, s, http.MethodGet, "/v1/namespaces/susanoo/memories/search?q=release+v2024.1&strategy=auto", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("routed search = %d: %s", rr.Code, rr.Body)
	}
	var env memory.RouteResult
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("routed envelope decode: %v (%s)", err, rr.Body)
	}
	if env.Mode != memory.RouteExact || env.RequestedMode != memory.RouteAuto {
		t.Fatalf("envelope mode=%s requested=%s, want exact/auto", env.Mode, env.RequestedMode)
	}
	if len(env.Reasons) == 0 || env.Reasons[0] != "identifier:version" {
		t.Fatalf("envelope reasons = %v, want identifier:version first", env.Reasons)
	}
	if len(env.Hits) != 1 || env.Hits[0].Fact == nil || env.Hits[0].Fact.Key != "/svc/api" {
		t.Fatalf("envelope hits = %+v, want /svc/api", env.Hits)
	}

	// Explicit mode wins over temporal language: written at the server's
	// now, "last week" would window this fact out; exact must not window.
	rr = do(t, s, http.MethodGet, "/v1/namespaces/susanoo/memories/search?q=release+last+week&strategy=exact", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("explicit exact = %d: %s", rr.Code, rr.Body)
	}
	env = memory.RouteResult{}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Mode != memory.RouteExact || len(env.Hits) != 1 {
		t.Fatalf("explicit exact: mode=%s hits=%d, want exact with the fact", env.Mode, len(env.Hits))
	}

	// Unknown strategy: 400, not a silent legacy fallback.
	rr = do(t, s, http.MethodGet, "/v1/namespaces/susanoo/memories/search?q=release&strategy=bogus", "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown strategy = %d: %s, want 400", rr.Code, rr.Body)
	}
}

// TestSearchStrategyWindowContract: an explicit since/until window with
// an explicit strategy must never be silently ignored (reviewer
// preflight 3). historical carries the window; auto resolves to
// historical; modes without window support reject the combination with a
// clear 400.
func TestSearchStrategyWindowContract(t *testing.T) {
	s := testServer(t)
	do(t, s, http.MethodPost, "/v1/namespaces/susanoo/memories",
		`{"key":"/svc/rollout","body":"rollout checklist v2024.1","author":"tester"}`)
	sinceAll := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	untilAll := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	untilNone := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)

	// historical carries the explicit window: all-inclusive -> hit,
	// window ending 2001 (fact written at the server's 2026 clock) -> no hit.
	rr := do(t, s, http.MethodGet,
		"/v1/namespaces/susanoo/memories/search?q=rollout&strategy=historical&since="+sinceAll+"&until="+untilAll, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("historical+window = %d: %s", rr.Code, rr.Body)
	}
	var env memory.RouteResult
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Mode != memory.RouteHistorical || len(env.Hits) != 1 {
		t.Fatalf("historical+window: mode=%s hits=%d, want historical with the fact", env.Mode, len(env.Hits))
	}
	rr = do(t, s, http.MethodGet,
		"/v1/namespaces/susanoo/memories/search?q=rollout&strategy=historical&since="+sinceAll+"&until="+untilNone, "")
	env = memory.RouteResult{}
	if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &env) != nil || len(env.Hits) != 0 {
		t.Fatalf("historical+narrow window = %d hits=%d: window must filter, not be ignored", rr.Code, len(env.Hits))
	}

	// auto with an explicit window resolves to historical.
	rr = do(t, s, http.MethodGet,
		"/v1/namespaces/susanoo/memories/search?q=rollout&strategy=auto&since="+sinceAll+"&until="+untilAll, "")
	env = memory.RouteResult{}
	if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &env) != nil || env.Mode != memory.RouteHistorical {
		t.Fatalf("auto+window = %d mode=%s, want 200 historical", rr.Code, env.Mode)
	}

	// exact has no window semantics: clear 400, not silent ignorance.
	rr = do(t, s, http.MethodGet,
		"/v1/namespaces/susanoo/memories/search?q=rollout&strategy=exact&since="+sinceAll+"&until="+untilAll, "")
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "window") {
		t.Fatalf("exact+window = %d: %s, want a 400 naming the window conflict", rr.Code, rr.Body)
	}

	// since without until stays a 400 on the strategy path too.
	rr = do(t, s, http.MethodGet,
		"/v1/namespaces/susanoo/memories/search?q=rollout&strategy=historical&since="+sinceAll, "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("historical+since-only = %d, want 400", rr.Code)
	}
}

func TestProfileAndDiagnoseEndpoints(t *testing.T) {
	s := testServer(t)
	rr := do(t, s, http.MethodPost, "/v1/namespaces/ns/memories",
		`{"key":"/a","body":"x","author":"t"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("remember = %d: %s", rr.Code, rr.Body)
	}

	rr = do(t, s, http.MethodGet, "/v1/namespaces/ns/profile", "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"facts":1`) {
		t.Fatalf("profile -> %d: %s", rr.Code, rr.Body.String())
	}

	rr = do(t, s, http.MethodGet, "/v1/namespaces/ns/diagnose", "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"namespace":"ns"`) {
		t.Fatalf("diagnose -> %d: %s", rr.Code, rr.Body.String())
	}
}

// fakeExpander is a stub memory.QueryExpander for TestSearchExpandParam: it
// always returns the fixed reformulation list it was built with.
type fakeExpander struct{ refs []string }

func (f fakeExpander) Expand(context.Context, string) ([]string, error) { return f.refs, nil }

// TestSearchExpandParam proves the REST search endpoint's expand=1 param.
// testServer(t) hardcodes Deps{Memory: ...} with no way to inject an
// Expander, so this builds the server directly (mirrors TestMemorySSEAndAsOf),
// seeding the same two-fact fixture as memory.TestHybridSearchExpanded and
// the mcpserver TestSearchExpandFlag: /a only matches the direct query
// "primary", /b only matches the reformulation "login tokens".
func TestSearchExpandParam(t *testing.T) {
	newExpandServer := func(t *testing.T, exp memory.QueryExpander) *Server {
		t.Helper()
		db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "expandapi.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if _, err := db.MigrateUp(context.Background()); err != nil {
			t.Fatal(err)
		}
		clk := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
		now := func() time.Time { clk = clk.Add(time.Millisecond); return clk }
		s := New(testLogger(), Deps{Memory: memory.New(db, now), Expander: exp})
		for _, kv := range [][2]string{
			{"/a", "authentication uses jwt"},
			{"/b", "login tokens rotate hourly"},
		} {
			rr := do(t, s, http.MethodPost, "/v1/namespaces/ns/memories",
				`{"key":"`+kv[0]+`","body":"`+kv[1]+`","author":"t"}`)
			if rr.Code != http.StatusCreated {
				t.Fatalf("seed %s = %d: %s", kv[0], rr.Code, rr.Body)
			}
		}
		return s
	}

	s := newExpandServer(t, fakeExpander{refs: []string{"login tokens"}})
	rr := do(t, s, http.MethodGet,
		"/v1/namespaces/ns/memories/search?q=authentication&mode=hybrid&scored=1&expand=1", "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "/a") || !strings.Contains(rr.Body.String(), "/b") {
		t.Fatalf("search expand=1 = %d: %s, want both /a and /b", rr.Code, rr.Body)
	}

	rr = do(t, s, http.MethodGet,
		"/v1/namespaces/ns/memories/search?q=authentication&mode=hybrid&scored=1", "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "/a") || strings.Contains(rr.Body.String(), "/b") {
		t.Fatalf("search without expand = %d: %s, want only /a", rr.Code, rr.Body)
	}

	sNoExpander := newExpandServer(t, nil)
	rr = do(t, sNoExpander, http.MethodGet,
		"/v1/namespaces/ns/memories/search?q=authentication&mode=hybrid&scored=1&expand=1", "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "/a") || strings.Contains(rr.Body.String(), "/b") {
		t.Fatalf("search expand=1 no expander = %d: %s, want only /a (flag ignored, no error)", rr.Code, rr.Body)
	}
}

func TestMemorySSEAndAsOf(t *testing.T) {
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "sse.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if _, err := db.MigrateUp(ctx); err != nil {
		t.Fatal(err)
	}
	clk := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	now := func() time.Time { clk = clk.Add(time.Second); return clk }
	mem := memory.New(db, now)
	b := bus.New()
	s := New(testLogger(), Deps{Memory: mem, Bus: b})

	// SSE: subscribe, publish through the bus, read one frame
	srv := httptest.NewServer(s.Router())
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/v1/namespaces/ns/events?prefix=/svc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	time.Sleep(100 * time.Millisecond) // subscription arming
	b.Publish(bus.Event{Kind: "memory", Key: "ns:/svc/db", Data: map[string]string{"action": "add"}})
	b.Publish(bus.Event{Kind: "memory", Key: "other:/x", Data: map[string]string{"action": "add"}})

	buf := make([]byte, 512)
	type readResult struct {
		n   int
		err error
	}
	ch := make(chan readResult, 1)
	go func() { n, err := resp.Body.Read(buf); ch <- readResult{n, err} }()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatal(r.err)
		}
		body := string(buf[:r.n])
		if !strings.Contains(body, "ns:/svc/db") {
			t.Fatalf("sse frame = %q", body)
		}
		if strings.Contains(body, "other:/x") {
			t.Fatal("prefix filter leaked foreign namespace")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no SSE frame")
	}

	// as_of read through the API
	if _, err := mem.Remember(ctx, "ns", "/svc/db", "v1", nil, "w"); err != nil {
		t.Fatal(err)
	}
	mid := clk.Add(time.Minute)
	clk = clk.Add(time.Hour)
	if _, err := mem.Remember(ctx, "ns", "/svc/db", "v2", nil, "w"); err != nil {
		t.Fatal(err)
	}
	rr := do(t, s, http.MethodGet, "/v1/namespaces/ns/memories?prefix=/svc&as_of="+mid.Format(time.RFC3339), "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"v1"`) || strings.Contains(rr.Body.String(), `"v2"`) {
		t.Fatalf("as_of = %d: %s", rr.Code, rr.Body)
	}
}

func TestSearchCompactParam(t *testing.T) {
	srv := testServer(t)
	ctx := context.Background()
	if _, err := srv.mem.Write(ctx, memory.WriteInput{Namespace: "ns", Key: "/k", Body: "disk full " + strings.Repeat("q", 700)}); err != nil {
		t.Fatal(err)
	}
	rec := do(t, srv, http.MethodGet, "/v1/namespaces/ns/memories/search?q=disk&format=compact", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var hits []struct {
		Key  string `json:"key"`
		Body string `json:"body"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &hits); err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Key != "/k" || len([]rune(hits[0].Body)) != 603 {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestSearchAnchorsParam(t *testing.T) {
	srv := testServer(t)
	ctx := context.Background()
	for k, b := range map[string]string{
		"/incident/1":   "database outage traced to connection saturation",
		"/runbook/pool": "when ERR_POOL_EXHAUSTED appears, raise max_connections",
	} {
		if _, err := srv.mem.Write(ctx, memory.WriteInput{Namespace: "ns", Key: k, Body: b}); err != nil {
			t.Fatal(err)
		}
	}
	rec := do(t, srv, http.MethodGet,
		"/v1/namespaces/ns/memories/search?q=database+outage&mode=hybrid&scored=1&anchor=ERR_POOL_EXHAUSTED&format=compact", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var hits []struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &hits); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range hits {
		found = found || h.Key == "/runbook/pool"
	}
	if !found {
		t.Fatalf("anchored runbook missing: %+v", hits)
	}
}

func TestAgentNamespaceEndpoint(t *testing.T) {
	srv := testServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/agent/namespace?cwd=/home/dev/My_Project", nil)
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"namespace":"agent-my-project"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/agent/namespace", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing cwd must 400, got %d", rec.Code)
	}
}
