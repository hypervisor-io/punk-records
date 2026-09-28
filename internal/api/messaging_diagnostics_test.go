package api

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/authz"
	"github.com/hypervisor-io/punk-records/internal/bus"
	"github.com/hypervisor-io/punk-records/internal/region"
	"github.com/hypervisor-io/punk-records/internal/store"
)

const diagPath = "/v1/namespaces/team/messages/diagnostics"

// diagnosticsRig is messagingRig with a settable clock, so staleness is
// driven by the server clock without sleeping, plus the db path so the
// server can be rebuilt over the same storage.
type diagnosticsRig struct {
	*messagingRig
	path string
	mu   sync.Mutex
	clk  time.Time
}

func (g *diagnosticsRig) now() time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.clk
}

func (g *diagnosticsRig) advance(d time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.clk = g.clk.Add(d)
}

// build (re)opens the database and server; a second call reconstructs
// both over the same file.
func (g *diagnosticsRig) build(t *testing.T) {
	t.Helper()
	db, err := store.Open("sqlite", g.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.MigrateUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	rig := &messagingRig{region: region.New(db, g.now), bus: bus.New()}
	rig.s = New(testLogger(), Deps{Region: rig.region, Bus: rig.bus})
	g.messagingRig = rig
}

func diagnosticsServer(t *testing.T) *diagnosticsRig {
	t.Helper()
	g := &diagnosticsRig{path: filepath.Join(t.TempDir(), "diag.db"), clk: time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)}
	g.build(t)
	return g
}

type diagList struct {
	Diagnostics []map[string]any `json:"diagnostics"`
}

func decodeDiagList(t *testing.T, body []byte) diagList {
	t.Helper()
	var out diagList
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if out.Diagnostics == nil {
		t.Fatalf("diagnostics must be a JSON array, got %s", body)
	}
	return out
}

// specExample is the exact request body from the shared contract.
const specExample = `{"agent":"opencode:session","client":"opencode","delivery_mode":"idle_wake","state":"waiting_for_idle","last_attempt_at":"2026-09-28T00:00:00Z","next_attempt_at":"2026-09-28T00:10:00Z","last_error":"prompt_failed","pending_ack_count":1,"wake_count":2}`

func TestMessageDiagnosticsHTTPContract(t *testing.T) {
	g := diagnosticsServer(t)
	g.register(t, "team", "opencode:session")

	rec := g.do(t, http.MethodPost, diagPath, specExample)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"status":"recorded"}` {
		t.Fatalf("post = %d %s", rec.Code, rec.Body)
	}
	rec = g.do(t, http.MethodGet, diagPath, "")
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("get = %d %s", rec.Code, rec.Body)
	}
	got := decodeDiagList(t, rec.Body.Bytes())
	if len(got.Diagnostics) != 1 {
		t.Fatalf("list = %+v", got)
	}
	d := got.Diagnostics[0]
	want := map[string]any{
		"namespace": "team", "agent": "opencode:session", "client": "opencode", "delivery_mode": "idle_wake",
		"state": "waiting_for_idle", "last_attempt_at": "2026-09-28T00:00:00.000000000Z",
		"next_attempt_at": "2026-09-28T00:10:00.000000000Z", "last_error": "prompt_failed",
		"pending_ack_count": float64(1), "wake_count": float64(2),
		"updated_at": "2026-09-28T00:00:00.000000000Z", "stale": false,
	}
	for k, v := range want {
		if d[k] != v {
			t.Errorf("%s = %#v, want %#v", k, d[k], v)
		}
	}
	if len(d) != len(want) {
		t.Errorf("unexpected fields: %v", d)
	}

	// replacement: absent optional values clear, counts default to zero
	g.advance(time.Second)
	rec = g.do(t, http.MethodPost, diagPath, `{"agent":"opencode:session","client":"opencode","delivery_mode":"idle_wake","state":"ready","last_attempt_at":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("replace = %d %s", rec.Code, rec.Body)
	}
	d = decodeDiagList(t, g.do(t, http.MethodGet, diagPath+"?agent=opencode:session", "").Body.Bytes()).Diagnostics[0]
	for _, k := range []string{"last_attempt_at", "next_attempt_at", "last_error"} {
		if _, ok := d[k]; ok {
			t.Errorf("%s survived replacement: %v", k, d)
		}
	}
	if d["state"] != "ready" || d["pending_ack_count"] != float64(0) || d["wake_count"] != float64(0) || d["updated_at"] != "2026-09-28T00:00:01.000000000Z" {
		t.Fatalf("replacement = %v", d)
	}

	// staleness by server clock, then reconstruction keeps the snapshot
	g.advance(121 * time.Second)
	g.build(t)
	d = decodeDiagList(t, g.do(t, http.MethodGet, diagPath, "").Body.Bytes()).Diagnostics[0]
	if d["stale"] != true || d["state"] != "ready" {
		t.Fatalf("after reconstruction and 121s = %v", d)
	}
	// a fresh report clears staleness
	g.do(t, http.MethodPost, diagPath, specExample)
	d = decodeDiagList(t, g.do(t, http.MethodGet, diagPath, "").Body.Bytes()).Diagnostics[0]
	if d["stale"] != false {
		t.Fatalf("fresh report still stale: %v", d)
	}
}

func TestMessageDiagnosticsHTTPListingAndIsolation(t *testing.T) {
	g := diagnosticsServer(t)
	for _, a := range []string{"zed", "amy", "opencode:ses_x"} {
		g.register(t, "team", a)
		body := `{"agent":"` + a + `","client":"opencode","delivery_mode":"catch_up","state":"ready"}`
		if rec := g.do(t, http.MethodPost, diagPath, body); rec.Code != http.StatusOK {
			t.Fatalf("post %s = %d %s", a, rec.Code, rec.Body)
		}
	}
	g.register(t, "other", "amy")
	g.do(t, http.MethodPost, "/v1/namespaces/other/messages/diagnostics", `{"agent":"amy","client":"claude","delivery_mode":"hook_continuation","state":"waiting_for_next_prompt"}`)

	list := decodeDiagList(t, g.do(t, http.MethodGet, diagPath, "").Body.Bytes()).Diagnostics
	var agents []string
	for _, d := range list {
		agents = append(agents, d["agent"].(string))
		if d["namespace"] != "team" || d["delivery_mode"] != "catch_up" {
			t.Fatalf("namespace leak: %v", d)
		}
	}
	if strings.Join(agents, ",") != "amy,opencode:ses_x,zed" {
		t.Fatalf("order = %v", agents)
	}
	one := decodeDiagList(t, g.do(t, http.MethodGet, "/v1/namespaces/other/messages/diagnostics?agent=amy", "").Body.Bytes()).Diagnostics
	if len(one) != 1 || one[0]["client"] != "claude" || one[0]["state"] != "waiting_for_next_prompt" {
		t.Fatalf("other/amy = %v", one)
	}
	// colon addresses round-trip through the query string
	one = decodeDiagList(t, g.do(t, http.MethodGet, diagPath+"?agent=opencode%3Ases_x", "").Body.Bytes()).Diagnostics
	if len(one) != 1 || one[0]["agent"] != "opencode:ses_x" {
		t.Fatalf("encoded agent filter = %v", one)
	}
	for _, path := range []string{diagPath + "?agent=nobody", "/v1/namespaces/empty/messages/diagnostics"} {
		rec := g.do(t, http.MethodGet, path, "")
		if rec.Code != http.StatusOK || len(decodeDiagList(t, rec.Body.Bytes()).Diagnostics) != 0 {
			t.Fatalf("%s = %d %s, want empty list", path, rec.Code, rec.Body)
		}
	}
}

func TestMessageDiagnosticsHTTPValidation(t *testing.T) {
	g := diagnosticsServer(t)
	g.register(t, "team", "a")
	base := `"agent":"a","client":"opencode","delivery_mode":"idle_wake","state":"ready"`
	bad := map[string]string{
		"not json":           `{`,
		"trailing data":      `{` + base + `}{}`,
		"array":              `[]`,
		"missing agent":      `{"client":"opencode","delivery_mode":"idle_wake","state":"ready"}`,
		"missing client":     `{"agent":"a","delivery_mode":"idle_wake","state":"ready"}`,
		"missing mode":       `{"agent":"a","client":"opencode","state":"ready"}`,
		"missing state":      `{"agent":"a","client":"opencode","delivery_mode":"idle_wake"}`,
		"unknown mode":       `{"agent":"a","client":"opencode","delivery_mode":"push","state":"ready"}`,
		"unknown state":      `{"agent":"a","client":"opencode","delivery_mode":"idle_wake","state":"delivered"}`,
		"bad timestamp":      `{` + base + `,"next_attempt_at":"soon"}`,
		"number timestamp":   `{` + base + `,"last_attempt_at":17}`,
		"raw error":          `{` + base + `,"last_error":"dial tcp http://127.0.0.1:9090/v1?key=x: refused"}`,
		"negative count":     `{` + base + `,"pending_ack_count":-1}`,
		"fractional count":   `{` + base + `,"wake_count":1.5}`,
		"string count":       `{` + base + `,"wake_count":"2"}`,
		"huge count":         `{` + base + `,"wake_count":10001}`,
		"overflow count":     `{` + base + `,"wake_count":99999999999999999999999}`,
		"namespace conflict": `{` + base + `,"namespace":"other"}`,
		"oversized body":     `{` + base + `,"pad":"` + strings.Repeat("x", maxDiagnosticBody) + `"}`,
	}
	for name, body := range bad {
		if rec := g.do(t, http.MethodPost, diagPath, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d %s, want 400", name, rec.Code, rec.Body)
		}
	}
	if rec := g.do(t, http.MethodPost, "/v1/namespaces/te:am/messages/diagnostics", `{`+base+`}`); rec.Code != http.StatusBadRequest {
		t.Errorf("colon namespace = %d", rec.Code)
	}
	if list := decodeDiagList(t, g.do(t, http.MethodGet, diagPath, "").Body.Bytes()).Diagnostics; len(list) != 0 {
		t.Fatalf("invalid reports stored: %v", list)
	}
	// unknown fields are additive-compatible; a matching namespace is fine
	if rec := g.do(t, http.MethodPost, diagPath, `{`+base+`,"namespace":"team","future_field":true}`); rec.Code != http.StatusOK {
		t.Fatalf("additive fields = %d %s", rec.Code, rec.Body)
	}
	// unregistered member: 404, nothing stored, membership not created
	if rec := g.do(t, http.MethodPost, diagPath, `{"agent":"ghost","client":"opencode","delivery_mode":"idle_wake","state":"ready"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unregistered = %d %s, want 404", rec.Code, rec.Body)
	}
	members, _ := g.region.Members(context.Background(), "team")
	if len(members) != 1 {
		t.Fatalf("report created membership: %v", members)
	}
	if rec := g.do(t, http.MethodGet, diagPath+"?agent=%20a", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad agent filter = %d", rec.Code)
	}
}

// TestMessageDiagnosticsHTTPNoSideEffects: GET and POST diagnostics never
// touch liveness, leases, unread or ACK state; the existing messaging
// endpoints behave the same with snapshots present.
func TestMessageDiagnosticsHTTPNoSideEffects(t *testing.T) {
	g := diagnosticsServer(t)
	ctx := context.Background()
	g.register(t, "team", "alice")
	g.register(t, "team", "bob")
	if rec := g.do(t, http.MethodPost, "/v1/namespaces/team/messages", `{"sender":"alice","recipient":"bob","body":"hi"}`); rec.Code != http.StatusCreated {
		t.Fatalf("send = %d %s", rec.Code, rec.Body)
	}
	snapshot := func() (string, int64, []region.Message) {
		members, _ := g.region.Members(ctx, "team")
		seen := ""
		for _, m := range members {
			seen += m.Agent + "=" + m.LastSeenAt + ";"
		}
		n, _ := g.region.CountUnreadMessages(ctx, "team", "bob")
		all, _ := g.region.ListMessages(ctx, "team", region.MessageLogOptions{})
		return seen, n, all
	}
	seen0, unread0, log0 := snapshot()
	g.advance(time.Minute)
	g.do(t, http.MethodPost, diagPath, `{"agent":"bob","client":"opencode","delivery_mode":"idle_wake","state":"handoff_unconfirmed","pending_ack_count":1}`)
	g.do(t, http.MethodGet, diagPath, "")
	g.do(t, http.MethodGet, diagPath+"?agent=bob", "")
	seen1, unread1, log1 := snapshot()
	if seen0 != seen1 || unread0 != unread1 || unread1 != 1 || len(log1) != len(log0) || log1[0].AckedAt != "" || log1[0].LeasedBy != "" {
		t.Fatalf("diagnostics changed delivery state: %q/%d -> %q/%d %+v", seen0, unread0, seen1, unread1, log1)
	}
	// legacy delivery still works and ACK truth is independent of the
	// reported snapshot
	msgs := deliveryHTTPMessages(t, g.do(t, http.MethodGet, "/v1/namespaces/team/messages?agent=bob", ""))
	if len(msgs) != 1 {
		t.Fatalf("read = %+v", msgs)
	}
	if rec := g.do(t, http.MethodPost, "/v1/namespaces/team/messages/ack", `{"agent":"bob","ids":["`+msgs[0].ID+`"]}`); rec.Code != http.StatusOK {
		t.Fatalf("ack = %d", rec.Code)
	}
	d := decodeDiagList(t, g.do(t, http.MethodGet, diagPath+"?agent=bob", "").Body.Bytes()).Diagnostics[0]
	if d["state"] != "handoff_unconfirmed" || d["pending_ack_count"] != float64(1) {
		t.Fatalf("server rewrote a reported snapshot: %v", d)
	}
	// deleting the member over HTTP cleans its snapshot
	if rec := g.do(t, http.MethodDelete, "/v1/namespaces/team/members/bob", ""); rec.Code != http.StatusOK {
		t.Fatalf("delete = %d", rec.Code)
	}
	if list := decodeDiagList(t, g.do(t, http.MethodGet, diagPath, "").Body.Bytes()).Diagnostics; len(list) != 0 {
		t.Fatalf("snapshot survived member deletion: %v", list)
	}
}

func TestMessageDiagnosticsHTTPAuthorization(t *testing.T) {
	g := authzBoundaryServer(t, true)
	ctx := context.Background()
	for _, ns := range []string{"ns-a", "ns-b"} {
		if err := g.region.Register(ctx, ns, "bob", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.region.RecordMessageDiagnostic(ctx, region.MessageDiagnosticInput{Namespace: "ns-b", Agent: "bob",
		Client: "opencode", DeliveryMode: "idle_wake", State: "delivery_failed"}); err != nil {
		t.Fatal(err)
	}
	body := `{"agent":"bob","client":"opencode","delivery_mode":"idle_wake","state":"ready"}`
	// no grant on B: neither read nor write
	if rec := g.do(t, http.MethodGet, "/v1/namespaces/ns-b/messages/diagnostics", "", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("cross read = %d %s", rec.Code, rec.Body)
	}
	if rec := g.do(t, http.MethodPost, "/v1/namespaces/ns-b/messages/diagnostics", body, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("cross write = %d", rec.Code)
	}
	b, _ := g.region.MessageDiagnostics(ctx, "ns-b", "bob")
	if len(b) != 1 || b[0].State != "delivery_failed" {
		t.Fatalf("denied write changed B: %+v", b)
	}
	// read+write on A
	if rec := g.do(t, http.MethodPost, "/v1/namespaces/ns-a/messages/diagnostics", body, nil); rec.Code != http.StatusOK {
		t.Fatalf("own write = %d %s", rec.Code, rec.Body)
	}
	// read-only on A: GET allowed, POST denied
	if err := g.az.Revoke(ctx, "alice", "ns-a", authz.OpWrite); err != nil {
		t.Fatal(err)
	}
	if rec := g.do(t, http.MethodPost, "/v1/namespaces/ns-a/messages/diagnostics", body, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("read-only write = %d", rec.Code)
	}
	rec := g.do(t, http.MethodGet, "/v1/namespaces/ns-a/messages/diagnostics", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("read-only read = %d %s", rec.Code, rec.Body)
	}
	if list := decodeDiagList(t, rec.Body.Bytes()).Diagnostics; len(list) != 1 || list[0]["namespace"] != "ns-a" {
		t.Fatalf("read-only list = %v", list)
	}
	// missing credential
	g.token = ""
	if rec := g.do(t, http.MethodGet, "/v1/namespaces/ns-a/messages/diagnostics", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d", rec.Code)
	}
}
