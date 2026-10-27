package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/authz"
	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/region"
	"github.com/hypervisor-io/punk-records/internal/store"
)

func getOverview(t *testing.T, s *Server) overviewOut {
	t.Helper()
	rr := do(t, s, http.MethodGet, "/v1/overview", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("overview = %d: %s", rr.Code, rr.Body)
	}
	var out overviewOut
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func overviewNS(t *testing.T, o overviewOut, name string) overviewNamespace {
	t.Helper()
	for _, n := range o.Namespaces {
		if n.Name == name {
			return n
		}
	}
	t.Fatalf("namespace %q missing from overview: %+v", name, o.Namespaces)
	return overviewNamespace{}
}

// seedOverview writes a small coordination world: "board" has tasks in
// every state plus members, mail and a claim; "notes" is plain memory;
// "quiet" has only a registered member.
func seedOverview(t *testing.T, mem *memory.Store, reg *region.Store) {
	t.Helper()
	ctx := context.Background()
	for _, w := range []struct{ ns, key, body string }{
		{"notes", "/notes/a", "a note"},
		{"board", "/tasks/T1", "pending one (no status)"},
		{"board", "/tasks/T2", "in progress one"},
		{"board", "/tasks/T3", "review one"},
		{"board", "/tasks/T4", "blocked one"},
		{"board", "/tasks/T5", "done one"},
		{"board", "/tasks/T6", "done with prose status"},
		{"board", "/tasks/T2/status", "in progress (half way)"},
		{"board", "/tasks/T3/status", "review: please look"},
		{"board", "/tasks/T4/status", "blocked - waiting"},
		{"board", "/tasks/T5/status", "done: shipped"},
		{"board", "/tasks/T6/status", "done (benchmark 12ms)"},
		{"board", "/tasks/T9/status", "done: orphan status, no task fact"},
	} {
		if _, err := mem.Write(ctx, memory.WriteInput{Namespace: w.ns, Key: w.key, Body: w.body}); err != nil {
			t.Fatal(err)
		}
	}
	// A status that was superseded: only the latest revision counts.
	for _, body := range []string{"blocked: first", "done: second"} {
		if _, err := mem.Write(ctx, memory.WriteInput{Namespace: "board", Key: "/tasks/T1/status", Body: body}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := mem.Write(ctx, memory.WriteInput{Namespace: "board", Key: "/tasks/T1/status", Body: "pending: back again"}); err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"alice", "bob"} {
		if err := reg.Register(ctx, "board", a, "worker"); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.Register(ctx, "quiet", "carol", "worker"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := reg.SendMessage(ctx, region.MessageInput{Namespace: "board", Sender: "alice", Recipient: "bob", Body: "hello"}); err != nil {
			t.Fatal(err)
		}
	}
	msgs, err := reg.ReadMessages(ctx, "board", "bob", 10)
	if err != nil || len(msgs) != 3 {
		t.Fatalf("read = %d %v", len(msgs), err)
	}
	if _, err := reg.AckMessages(ctx, "board", "bob", []string{msgs[0].ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.ClaimWork(ctx, "board", "/tasks/T2", "alice", time.Hour); err != nil {
		t.Fatal(err)
	}
}

func TestOverview(t *testing.T) {
	s, mem, reg := brainTestServer(t)
	seedOverview(t, mem, reg)
	release := reg.Attach("board", "alice")
	defer release()

	o := getOverview(t, s)
	if o.Version != "vtest" || o.Now == "" {
		t.Fatalf("header = %q %q", o.Version, o.Now)
	}
	if o.Runtime.Authz != "off" || o.Runtime.AIEnabled || o.Runtime.LedgerTasks != 0 || o.Runtime.DomainAgents != 0 {
		t.Fatalf("runtime = %+v; want zeros with authz off", o.Runtime)
	}
	if len(o.Namespaces) != 3 {
		t.Fatalf("namespaces = %+v; want board, notes, quiet", o.Namespaces)
	}

	b := overviewNS(t, o, "board")
	want := memory.TaskStateCounts{Pending: 1, InProgress: 1, Review: 1, Blocked: 1, Done: 2}
	if b.TaskCounts != want || b.Tasks != 6 {
		t.Fatalf("board tasks = %d %+v; want 6 %+v", b.Tasks, b.TaskCounts, want)
	}
	if b.Members != 2 || b.MembersLive != 1 || b.MembersRecent != 2 {
		t.Fatalf("board members = %d live %d recent %d; want 2/1/2", b.Members, b.MembersLive, b.MembersRecent)
	}
	if b.Unread != 2 || b.Messages24h != 3 || b.Claims != 1 {
		t.Fatalf("board unread/24h/claims = %d/%d/%d; want 2/3/1", b.Unread, b.Messages24h, b.Claims)
	}
	if b.LastWrite == "" || b.LastMessageAt == "" || b.LastSeenAt == "" || b.LastActivity == "" {
		t.Fatalf("board timestamps = %+v", b)
	}
	n := overviewNS(t, o, "notes")
	if n.Facts != 1 || n.Tasks != 0 || n.TaskCounts != (memory.TaskStateCounts{}) || n.LastMessageAt != "" || n.LastSeenAt != "" {
		t.Fatalf("notes = %+v", n)
	}
	q := overviewNS(t, o, "quiet")
	if q.Facts != 0 || q.Members != 1 || q.MembersRecent != 1 || q.LastWrite != "" || q.LastActivity == "" {
		t.Fatalf("quiet = %+v", q)
	}

	tot := o.Totals
	if tot.Namespaces != 3 || tot.Facts != b.Facts+1 || tot.Tasks != 6 || tot.TasksOpen != 4 ||
		tot.Members != 3 || tot.MembersLive != 1 || tot.MembersRecent != 3 ||
		tot.Unread != 2 || tot.Messages24h != 3 || tot.Claims != 1 {
		t.Fatalf("totals = %+v", tot)
	}
}

// clockedServer is a Server over a fresh SQLite store whose memory,
// region and cache clocks all read one settable instant.
type clockedServer struct {
	*Server
	mem *memory.Store
	reg *region.Store
	cur time.Time
}

func newClockedServer(t *testing.T) *clockedServer {
	t.Helper()
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "overview.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.MigrateUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := &clockedServer{cur: time.Now().UTC().Add(-time.Hour)}
	now := func() time.Time { return c.cur }
	c.mem, c.reg = memory.New(db, now), region.New(db, now)
	c.Server = New(testLogger(), Deps{Memory: c.mem, Region: c.reg})
	c.clock = now
	return c
}

func (c *clockedServer) write(t *testing.T, at time.Duration, ns, key, body string) {
	t.Helper()
	c.cur = c.cur.Add(at)
	defer func() { c.cur = c.cur.Add(-at) }()
	if _, err := c.mem.Write(context.Background(), memory.WriteInput{Namespace: ns, Key: key, Body: body}); err != nil {
		t.Fatal(err)
	}
}

// TestOverviewSortOrder: last_activity (the max of write, message and
// member sighting) descending, then name.
func TestOverviewSortOrder(t *testing.T) {
	c := newClockedServer(t)
	ctx := context.Background()
	min := time.Minute
	c.write(t, 1*min, "zeta", "/k", "v")
	c.write(t, 2*min, "alpha", "/k", "v")
	c.write(t, 5*min, "tie-b", "/k", "v")
	c.write(t, 5*min, "tie-a", "/k", "v")
	// An old write with a fresh member sighting: activity is the max.
	c.write(t, 0, "seen", "/k", "v")
	c.cur = c.cur.Add(10 * min)
	if err := c.reg.Register(ctx, "seen", "agent", ""); err != nil {
		t.Fatal(err)
	}
	c.cur = c.cur.Add(-10 * min)
	// Mail is activity too.
	for _, a := range []string{"alice", "bob"} {
		if err := c.reg.Register(ctx, "mail", a, ""); err != nil {
			t.Fatal(err)
		}
	}
	c.cur = c.cur.Add(7 * min)
	if _, err := c.reg.SendMessage(ctx, region.MessageInput{Namespace: "mail", Sender: "alice", Recipient: "bob", Body: "hi"}); err != nil {
		t.Fatal(err)
	}
	c.cur = c.cur.Add(-7 * min)
	// Fully tombstoned namespaces have no live facts and no activity: they
	// sort last, by name.
	for _, ns := range []string{"nothing-b", "nothing-a"} {
		c.write(t, 0, ns, "/k", "v")
		if err := c.mem.Forget(ctx, ns, "/k", "t"); err != nil {
			t.Fatal(err)
		}
	}

	o := getOverview(t, c.Server)
	var got []string
	for _, n := range o.Namespaces {
		got = append(got, n.Name)
	}
	// "mail" registered its members at the base instant, so its newest
	// activity is the 7 minute message; "seen" is the 10 minute sighting.
	want := "seen,mail,tie-a,tie-b,alpha,zeta,nothing-a,nothing-b"
	if strings.Join(got, ",") != want {
		t.Fatalf("order = %v; want %s", got, want)
	}
}

// TestOverviewCache: a second call inside the TTL is served from the
// cache (new writes are not visible), and one past the TTL recomputes.
func TestOverviewCache(t *testing.T) {
	c := newClockedServer(t)
	c.write(t, 0, "one", "/k", "v")
	if n := getOverview(t, c.Server).Totals.Facts; n != 1 {
		t.Fatalf("first facts = %d, want 1", n)
	}
	c.write(t, 0, "one", "/k2", "v")
	c.cur = c.cur.Add(overviewTTL - time.Second)
	if n := getOverview(t, c.Server).Totals.Facts; n != 1 {
		t.Fatalf("within TTL facts = %d, want the cached 1", n)
	}
	c.cur = c.cur.Add(2 * time.Second)
	if n := getOverview(t, c.Server).Totals.Facts; n != 2 {
		t.Fatalf("past TTL facts = %d, want recomputed 2", n)
	}
}

// TestNamespaceIndexCached: /v1/namespaces shares the 20s summaries cache.
func TestNamespaceIndexCached(t *testing.T) {
	c := newClockedServer(t)
	c.write(t, 0, "one", "/k", "v")
	count := func() int {
		var out nsIndexOut
		rr := do(t, c.Server, http.MethodGet, "/v1/namespaces", "")
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil || rr.Code != http.StatusOK {
			t.Fatalf("index = %d %v", rr.Code, err)
		}
		return len(out.Namespaces)
	}
	if n := count(); n != 1 {
		t.Fatalf("namespaces = %d, want 1", n)
	}
	c.write(t, 0, "two", "/k", "v")
	if n := count(); n != 1 {
		t.Fatalf("within TTL namespaces = %d, want the cached 1", n)
	}
	c.cur = c.cur.Add(overviewTTL + time.Second)
	if n := count(); n != 2 {
		t.Fatalf("past TTL namespaces = %d, want recomputed 2", n)
	}
}

// TestTTLCacheSingleFlight: concurrent misses share one computation and
// an error is not cached.
func TestTTLCacheSingleFlight(t *testing.T) {
	now := time.Now()
	c := newTTLCache[int](time.Minute, func() time.Time { return now })
	release := make(chan struct{})
	calls := 0
	load := func(context.Context) (int, error) {
		calls++ // only the one flight touches it
		<-release
		return 7, nil
	}
	results := make(chan int, 5)
	for i := 0; i < 5; i++ {
		go func() {
			v, err := c.get(context.Background(), load)
			if err != nil {
				v = -1
			}
			results <- v
		}()
	}
	// Let the callers pile onto the flight before it finishes.
	time.Sleep(50 * time.Millisecond)
	close(release)
	for i := 0; i < 5; i++ {
		if v := <-results; v != 7 {
			t.Fatalf("result = %d, want 7", v)
		}
	}
	if calls != 1 {
		t.Fatalf("load ran %d times, want 1", calls)
	}

	failing := newTTLCache[int](time.Minute, func() time.Time { return now })
	boom := context.DeadlineExceeded
	if _, err := failing.get(context.Background(), func(context.Context) (int, error) { return 0, boom }); err != boom {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if v, err := failing.get(context.Background(), func(context.Context) (int, error) { return 3, nil }); err != nil || v != 3 {
		t.Fatalf("retry = %d %v, want 3 (errors are not cached)", v, err)
	}
}

// TestOverviewAuthz: the cache is global but each response keeps only the
// namespaces the verified subject may read, and totals follow the rows.
func TestOverviewAuthz(t *testing.T) {
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "ovauthz.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if _, err := db.MigrateUp(ctx); err != nil {
		t.Fatal(err)
	}
	mem, reg := memory.New(db, nil), region.New(db, nil)
	keys := NewKeys(db, nil)
	az := authz.New(db, nil)
	keys.SetAuthorizer(az)
	s := New(testLogger(), Deps{Memory: mem, Region: reg, Keys: keys, AuthzMode: "deny"})
	for _, ns := range []string{"ns-a", "ns-b"} {
		if _, err := mem.Remember(ctx, ns, "/tasks/A", "a task", nil, "tester"); err != nil {
			t.Fatal(err)
		}
	}
	alice, err := keys.Create(ctx, "alice-key", "alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := keys.Create(ctx, "bob-key", "bob")
	if err != nil {
		t.Fatal(err)
	}
	if err := az.Grant(ctx, "alice", "ns-a", authz.OpRead); err != nil {
		t.Fatal(err)
	}
	if err := az.Grant(ctx, "bob", "ns-a", authz.OpRead); err != nil {
		t.Fatal(err)
	}
	if err := az.Grant(ctx, "bob", "ns-b", authz.OpRead); err != nil {
		t.Fatal(err)
	}
	get := func(tok string) (int, overviewOut) {
		r := httptest.NewRequest(http.MethodGet, "/v1/overview", nil)
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, r)
		var out overviewOut
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	if code, _ := get(""); code != http.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401", code)
	}
	code, a := get(alice)
	if code != http.StatusOK || len(a.Namespaces) != 1 || a.Namespaces[0].Name != "ns-a" || a.Totals.Namespaces != 1 || a.Totals.Tasks != 1 {
		t.Fatalf("alice = %d %+v; want ns-a only", code, a)
	}
	if a.Runtime.Authz != "deny" {
		t.Fatalf("authz = %q, want deny", a.Runtime.Authz)
	}
	// Bob, served from the same cache entry, sees both.
	if code, b := get(bob); code != http.StatusOK || len(b.Namespaces) != 2 || b.Totals.Tasks != 2 {
		t.Fatalf("bob = %d %+v; want both namespaces", code, b)
	}
}

type recentOut struct {
	Namespace string `json:"namespace"`
	Facts     []struct {
		Key        string  `json:"key"`
		Body       string  `json:"body"`
		Author     string  `json:"author"`
		Action     string  `json:"action"`
		CreatedAt  string  `json:"created_at"`
		Importance float64 `json:"importance"`
	} `json:"facts"`
}

func getRecent(t *testing.T, s *Server, path string) recentOut {
	t.Helper()
	rr := do(t, s, http.MethodGet, path, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("%s = %d: %s", path, rr.Code, rr.Body)
	}
	var out recentOut
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRecent(t *testing.T) {
	c := newClockedServer(t)
	ctx := context.Background()
	min := time.Minute
	c.write(t, 1*min, "feed", "/a", "first a")
	c.write(t, 2*min, "feed", "/b", "b")
	c.write(t, 3*min, "feed", "/a", "second a") // newest revision wins, a moves up
	c.write(t, 4*min, "feed", "/gone", "soon tombstoned")
	c.write(t, 5*min, "feed", "/agent-sessions/s1/summary", "session scratch")
	c.write(t, 6*min, "feed", "/long", strings.Repeat("x", 2000))
	c.cur = c.cur.Add(7 * min)
	if err := c.mem.Forget(ctx, "feed", "/gone", "tester"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.mem.Write(ctx, memory.WriteInput{Namespace: "other", Key: "/x", Body: "not in feed"}); err != nil {
		t.Fatal(err)
	}

	keys := func(o recentOut) string {
		var ks []string
		for _, f := range o.Facts {
			ks = append(ks, f.Key)
		}
		return strings.Join(ks, ",")
	}
	got := getRecent(t, c.Server, "/v1/namespaces/feed/recent")
	if got.Namespace != "feed" || keys(got) != "/long,/a,/b" {
		t.Fatalf("recent = %s %s; want /long,/a,/b (no tombstone, no sessions)", got.Namespace, keys(got))
	}
	if got.Facts[1].Body != "second a" || got.Facts[1].Action != "update" || got.Facts[1].CreatedAt == "" {
		t.Fatalf("/a = %+v; want its newest revision", got.Facts[1])
	}
	if n := len([]rune(got.Facts[0].Body)); n != memory.RecentBodyChars {
		t.Fatalf("long body = %d chars, want clipped to %d", n, memory.RecentBodyChars)
	}
	if k := keys(getRecent(t, c.Server, "/v1/namespaces/feed/recent?sessions=1")); k != "/long,/agent-sessions/s1/summary,/a,/b" {
		t.Fatalf("sessions=1 = %s", k)
	}
	if k := keys(getRecent(t, c.Server, "/v1/namespaces/feed/recent?limit=2")); k != "/long,/a" {
		t.Fatalf("limit=2 = %s", k)
	}
	if o := getRecent(t, c.Server, "/v1/namespaces/nope/recent"); o.Namespace != "nope" || o.Facts == nil || len(o.Facts) != 0 {
		t.Fatalf("unknown namespace = %+v; want empty facts array", o)
	}
}

// TestRecentLimitCap: limit above 200 is clamped.
func TestRecentLimitCap(t *testing.T) {
	c := newClockedServer(t)
	for i := 0; i < 205; i++ {
		c.write(t, time.Duration(i)*time.Second, "big", fmt.Sprintf("/k/%03d", i), "v")
	}
	if n := len(getRecent(t, c.Server, "/v1/namespaces/big/recent?limit=9999").Facts); n != 200 {
		t.Fatalf("limit=9999 returned %d, want 200", n)
	}
	if n := len(getRecent(t, c.Server, "/v1/namespaces/big/recent").Facts); n != 50 {
		t.Fatalf("default limit returned %d, want 50", n)
	}
}

// TestRecentAuthz: the route is classNSPath, so a key without a read
// grant on the namespace gets 403 from the path hook.
func TestRecentAuthz(t *testing.T) {
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "recentauthz.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if _, err := db.MigrateUp(ctx); err != nil {
		t.Fatal(err)
	}
	mem := memory.New(db, nil)
	keys := NewKeys(db, nil)
	az := authz.New(db, nil)
	keys.SetAuthorizer(az)
	s := New(testLogger(), Deps{Memory: mem, Keys: keys})
	if _, err := mem.Remember(ctx, "ns-b", "/k", "v", nil, "tester"); err != nil {
		t.Fatal(err)
	}
	tok, err := keys.Create(ctx, "alice-key", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := az.Grant(ctx, "alice", "ns-a", authz.OpRead); err != nil {
		t.Fatal(err)
	}
	for ns, want := range map[string]int{"ns-a": http.StatusOK, "ns-b": http.StatusForbidden} {
		r := httptest.NewRequest(http.MethodGet, "/v1/namespaces/"+ns+"/recent", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, r)
		if rec.Code != want {
			t.Fatalf("recent %s = %d, want %d", ns, rec.Code, want)
		}
	}
}
