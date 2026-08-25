package api

import (
	"context"
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
