package region

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/store"
)

// diagClock is a settable fake clock so staleness is exercised against
// the server clock without sleeping.
type diagClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *diagClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *diagClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func openDiagStore(t *testing.T, path string, clk *diagClock) *Store {
	t.Helper()
	db, err := store.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.MigrateUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	return New(db, clk.now)
}

func validDiag(ns, agent string) MessageDiagnosticInput {
	return MessageDiagnosticInput{
		Namespace: ns, Agent: agent, Client: "opencode",
		DeliveryMode: "idle_wake", State: "waiting_for_idle",
		LastAttemptAt: "2026-09-28T00:00:00Z", NextAttemptAt: "2026-09-28T02:10:00+02:00",
		LastError: "prompt_failed", PendingAckCount: 1, WakeCount: 2,
	}
}

func TestMessageDiagnosticsRecordReplaceAndReconstruct(t *testing.T) {
	path := filepath.Join(t.TempDir(), "diag.db")
	clk := &diagClock{t: time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)}
	s := openDiagStore(t, path, clk)
	ctx := context.Background()
	if err := s.Register(ctx, "team", "opencode:ses_1", "worker"); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Members(ctx, "team")

	if err := s.RecordMessageDiagnostic(ctx, validDiag("team", "opencode:ses_1")); err != nil {
		t.Fatal(err)
	}
	got, err := s.MessageDiagnostics(ctx, "team", "")
	if err != nil || len(got) != 1 {
		t.Fatalf("list = %+v err=%v", got, err)
	}
	d := got[0]
	if d.Namespace != "team" || d.Agent != "opencode:ses_1" || d.Client != "opencode" || d.DeliveryMode != "idle_wake" ||
		d.State != "waiting_for_idle" || d.LastError != "prompt_failed" || d.PendingAckCount != 1 || d.WakeCount != 2 || d.Stale {
		t.Fatalf("snapshot = %+v", d)
	}
	// timestamps normalize to the portable fixed-width UTC encoding
	if d.LastAttemptAt != "2026-09-28T00:00:00.000000000Z" || d.NextAttemptAt != "2026-09-28T00:10:00.000000000Z" {
		t.Fatalf("timestamps = %q %q", d.LastAttemptAt, d.NextAttemptAt)
	}
	if d.UpdatedAt != store.TimeToDB(clk.now()) {
		t.Fatalf("updated_at = %q", d.UpdatedAt)
	}

	// a diagnostic report is not a liveness sighting
	after, _ := s.Members(ctx, "team")
	if after[0].LastSeenAt != before[0].LastSeenAt {
		t.Fatalf("report touched last_seen_at: %q -> %q", before[0].LastSeenAt, after[0].LastSeenAt)
	}

	// latest wins; absent optional fields clear previous values
	clk.advance(5 * time.Second)
	if err := s.RecordMessageDiagnostic(ctx, MessageDiagnosticInput{Namespace: "team", Agent: "opencode:ses_1",
		Client: "opencode", DeliveryMode: "idle_wake", State: "ready"}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.MessageDiagnostics(ctx, "team", "")
	if len(got) != 1 {
		t.Fatalf("replacement grew storage: %+v", got)
	}
	d = got[0]
	if d.State != "ready" || d.LastAttemptAt != "" || d.NextAttemptAt != "" || d.LastError != "" || d.PendingAckCount != 0 || d.WakeCount != 0 {
		t.Fatalf("replacement kept stale fields: %+v", d)
	}
	var rows int
	if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM agent_message_diagnostics`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rows = %d err=%v, want one latest snapshot", rows, err)
	}

	// store reconstruction over the same database keeps the snapshot
	_ = s.DB().Close()
	s2 := openDiagStore(t, path, clk)
	got, err = s2.MessageDiagnostics(ctx, "team", "opencode:ses_1")
	if err != nil || len(got) != 1 || got[0].State != "ready" {
		t.Fatalf("after reconstruction = %+v err=%v", got, err)
	}
}

func TestMessageDiagnosticsStaleByServerClock(t *testing.T) {
	clk := &diagClock{t: time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)}
	s := openDiagStore(t, filepath.Join(t.TempDir(), "diag.db"), clk)
	ctx := context.Background()
	_ = s.Register(ctx, "team", "a", "")
	if err := s.RecordMessageDiagnostic(ctx, validDiag("team", "a")); err != nil {
		t.Fatal(err)
	}
	clk.advance(MessageDiagnosticStaleAfter)
	got, _ := s.MessageDiagnostics(ctx, "team", "a")
	if got[0].Stale {
		t.Fatal("exactly 120s old must not be stale")
	}
	clk.advance(time.Millisecond)
	got, _ = s.MessageDiagnostics(ctx, "team", "a")
	if !got[0].Stale {
		t.Fatal("older than 120s must be stale")
	}
}

func TestMessageDiagnosticsIsolationAndOrdering(t *testing.T) {
	clk := &diagClock{t: time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)}
	s := openDiagStore(t, filepath.Join(t.TempDir(), "diag.db"), clk)
	ctx := context.Background()
	for _, m := range []struct{ ns, agent string }{{"team", "zed"}, {"team", "amy"}, {"team", "mid"}, {"other", "amy"}} {
		if err := s.Register(ctx, m.ns, m.agent, ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range []struct{ ns, agent, state string }{{"team", "zed", "ready"}, {"team", "amy", "disabled"}, {"team", "mid", "ready"}, {"other", "amy", "delivery_failed"}} {
		in := validDiag(m.ns, m.agent)
		in.State = m.state
		if err := s.RecordMessageDiagnostic(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.MessageDiagnostics(ctx, "team", "")
	if err != nil || len(got) != 3 || got[0].Agent != "amy" || got[1].Agent != "mid" || got[2].Agent != "zed" {
		t.Fatalf("team list = %+v err=%v", got, err)
	}
	if got[0].State != "disabled" {
		t.Fatalf("namespace leak: team/amy = %q", got[0].State)
	}
	got, _ = s.MessageDiagnostics(ctx, "other", "amy")
	if len(got) != 1 || got[0].State != "delivery_failed" || got[0].Namespace != "other" {
		t.Fatalf("other/amy = %+v", got)
	}
	if got, _ := s.MessageDiagnostics(ctx, "team", "nobody"); len(got) != 0 {
		t.Fatalf("unknown agent = %+v", got)
	}
	if got, _ := s.MessageDiagnostics(ctx, "empty", ""); got == nil || len(got) != 0 {
		t.Fatalf("empty namespace = %#v, want empty non-nil", got)
	}
}

func TestMessageDiagnosticsRequireMember(t *testing.T) {
	clk := &diagClock{t: time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)}
	s := openDiagStore(t, filepath.Join(t.TempDir(), "diag.db"), clk)
	ctx := context.Background()
	_ = s.Register(ctx, "other", "a", "")
	err := s.RecordMessageDiagnostic(ctx, validDiag("team", "a"))
	if !errors.Is(err, ErrMessageNotFound) {
		t.Fatalf("unregistered = %v, want ErrMessageNotFound", err)
	}
	if got, _ := s.MessageDiagnostics(ctx, "team", ""); len(got) != 0 {
		t.Fatalf("stored for non-member: %+v", got)
	}
	// the report never creates membership
	if m, _ := s.Members(ctx, "team"); len(m) != 0 {
		t.Fatalf("report created membership: %+v", m)
	}
}

func TestMessageDiagnosticsValidation(t *testing.T) {
	clk := &diagClock{t: time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)}
	s := openDiagStore(t, filepath.Join(t.TempDir(), "diag.db"), clk)
	ctx := context.Background()
	_ = s.Register(ctx, "team", "a", "")
	cases := map[string]func(*MessageDiagnosticInput){
		"no namespace":      func(in *MessageDiagnosticInput) { in.Namespace = "" },
		"colon namespace":   func(in *MessageDiagnosticInput) { in.Namespace = "te:am" },
		"no agent":          func(in *MessageDiagnosticInput) { in.Agent = "" },
		"agent control":     func(in *MessageDiagnosticInput) { in.Agent = "a\n" },
		"agent too long":    func(in *MessageDiagnosticInput) { in.Agent = strings.Repeat("a", MaxMessageIDLen+1) },
		"no client":         func(in *MessageDiagnosticInput) { in.Client = "" },
		"client uppercase":  func(in *MessageDiagnosticInput) { in.Client = "OpenCode" },
		"client too long":   func(in *MessageDiagnosticInput) { in.Client = strings.Repeat("c", 65) },
		"no delivery mode":  func(in *MessageDiagnosticInput) { in.DeliveryMode = "" },
		"bad delivery mode": func(in *MessageDiagnosticInput) { in.DeliveryMode = "push" },
		"no state":          func(in *MessageDiagnosticInput) { in.State = "" },
		"bad state":         func(in *MessageDiagnosticInput) { in.State = "delivered" },
		"bad last attempt":  func(in *MessageDiagnosticInput) { in.LastAttemptAt = "yesterday" },
		"date only":         func(in *MessageDiagnosticInput) { in.NextAttemptAt = "2026-09-28" },
		"long timestamp": func(in *MessageDiagnosticInput) {
			in.LastAttemptAt = "2026-09-28T00:00:00." + strings.Repeat("0", 60) + "Z"
		},
		"raw error text":       func(in *MessageDiagnosticInput) { in.LastError = "Post http://x/y?token=abc failed" },
		"error with url":       func(in *MessageDiagnosticInput) { in.LastError = "https://host" },
		"error too long":       func(in *MessageDiagnosticInput) { in.LastError = strings.Repeat("e", 65) },
		"negative pending":     func(in *MessageDiagnosticInput) { in.PendingAckCount = -1 },
		"pending over bound":   func(in *MessageDiagnosticInput) { in.PendingAckCount = MaxMessageDiagnosticCount + 1 },
		"negative wake":        func(in *MessageDiagnosticInput) { in.WakeCount = -1 },
		"wake over bound":      func(in *MessageDiagnosticInput) { in.WakeCount = MaxMessageDiagnosticCount + 1 },
		"state wrong case":     func(in *MessageDiagnosticInput) { in.State = "READY" },
		"mode with whitespace": func(in *MessageDiagnosticInput) { in.DeliveryMode = " idle_wake" },
	}
	for name, mutate := range cases {
		in := validDiag("team", "a")
		mutate(&in)
		if err := s.RecordMessageDiagnostic(ctx, in); !errors.Is(err, ErrMessageInvalid) {
			t.Errorf("%s: err = %v, want ErrMessageInvalid", name, err)
		}
	}
	if got, _ := s.MessageDiagnostics(ctx, "team", ""); len(got) != 0 {
		t.Fatalf("invalid input stored: %+v", got)
	}
	// every allowed mode and state is accepted, and bounds are inclusive
	for _, mode := range []string{"idle_wake", "hook_continuation", "catch_up"} {
		for _, state := range []string{"ready", "waiting_for_idle", "waiting_for_next_prompt", "wake_budget_exhausted",
			"sender_filtered", "delivery_failed", "handoff_unconfirmed", "disabled"} {
			in := validDiag("team", "a")
			in.DeliveryMode, in.State = mode, state
			in.PendingAckCount, in.WakeCount = MaxMessageDiagnosticCount, 0
			in.LastError = "ack_failed:http_500"
			if err := s.RecordMessageDiagnostic(ctx, in); err != nil {
				t.Errorf("%s/%s rejected: %v", mode, state, err)
			}
		}
	}
	if _, err := s.MessageDiagnostics(ctx, "te:am", ""); !errors.Is(err, ErrMessageInvalid) {
		t.Fatalf("bad list namespace = %v", err)
	}
	if _, err := s.MessageDiagnostics(ctx, "team", " a"); !errors.Is(err, ErrMessageInvalid) {
		t.Fatalf("bad list agent = %v", err)
	}
}

// TestMessageDiagnosticsPostgres runs the store path (member-row lock,
// upsert, listing, cleanup) on Postgres. Skipped, not passed, without
// PUNK_TEST_PG_DSN; the DSN must point at a disposable database.
func TestMessageDiagnosticsPostgres(t *testing.T) {
	dsn := os.Getenv("PUNK_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("PUNK_TEST_PG_DSN not set")
	}
	db, err := store.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	for {
		n, err := db.MigrateDown(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	if _, err := db.MigrateUp(ctx); err != nil {
		t.Fatal(err)
	}
	clk := &diagClock{t: time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)}
	s := New(db, clk.now)
	if err := s.RecordMessageDiagnostic(ctx, validDiag("team", "a")); !errors.Is(err, ErrMessageNotFound) {
		t.Fatalf("unregistered = %v", err)
	}
	_ = s.Register(ctx, "team", "a", "")
	_ = s.Register(ctx, "team", "b", "")
	for i := 0; i < 2; i++ {
		if err := s.RecordMessageDiagnostic(ctx, validDiag("team", "a")); err != nil {
			t.Fatal(err)
		}
	}
	in := validDiag("team", "b")
	in.LastAttemptAt, in.State = "", "ready"
	if err := s.RecordMessageDiagnostic(ctx, in); err != nil {
		t.Fatal(err)
	}
	clk.advance(MessageDiagnosticStaleAfter + time.Second)
	got, err := s.MessageDiagnostics(ctx, "team", "")
	if err != nil || len(got) != 2 || got[0].Agent != "a" || got[1].LastAttemptAt != "" || !got[0].Stale ||
		got[0].NextAttemptAt != "2026-09-28T00:10:00.000000000Z" {
		t.Fatalf("list = %+v err=%v", got, err)
	}
	if removed, err := s.RemoveMember(ctx, "team", "a"); err != nil || !removed {
		t.Fatalf("remove = %v %v", removed, err)
	}
	if got, _ := s.MessageDiagnostics(ctx, "team", ""); len(got) != 1 || got[0].Agent != "b" {
		t.Fatalf("after remove = %+v", got)
	}
}

// TestMessageDiagnosticsMemberCleanup: removing or expiring a member
// deletes its snapshot, so a re-registered address starts clean.
func TestMessageDiagnosticsMemberCleanup(t *testing.T) {
	clk := &diagClock{t: time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)}
	s := openDiagStore(t, filepath.Join(t.TempDir(), "diag.db"), clk)
	ctx := context.Background()
	for _, a := range []string{"removed", "expired", "kept", "listening"} {
		if err := s.Register(ctx, "team", a, ""); err != nil {
			t.Fatal(err)
		}
		if err := s.RecordMessageDiagnostic(ctx, validDiag("team", a)); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.Register(ctx, "other", "removed", "")
	if err := s.RecordMessageDiagnostic(ctx, validDiag("other", "removed")); err != nil {
		t.Fatal(err)
	}

	if removed, err := s.RemoveMember(ctx, "team", "removed"); err != nil || !removed {
		t.Fatalf("remove = %v %v", removed, err)
	}
	if got, _ := s.MessageDiagnostics(ctx, "team", "removed"); len(got) != 0 {
		t.Fatalf("removed member kept diagnostics: %+v", got)
	}
	if got, _ := s.MessageDiagnostics(ctx, "other", "removed"); len(got) != 1 {
		t.Fatalf("cleanup crossed namespaces: %+v", got)
	}

	// age everyone except "kept"; "listening" is protected by its stream
	old := store.TimeToDB(clk.now().Add(-30 * 24 * time.Hour))
	if _, err := s.DB().ExecContext(ctx, s.DB().Rebind(
		`UPDATE region_members SET last_seen_at = $1 WHERE namespace = 'team' AND agent IN ('expired', 'listening')`), old); err != nil {
		t.Fatal(err)
	}
	release := s.Attach("team", "listening")
	defer release()
	if n, err := s.ExpireMembers(ctx, 7*24*time.Hour); err != nil || n != 1 {
		t.Fatalf("expire = %d %v", n, err)
	}
	got, _ := s.MessageDiagnostics(ctx, "team", "")
	agents := []string{}
	for _, d := range got {
		agents = append(agents, d.Agent)
	}
	if strings.Join(agents, ",") != "kept,listening" {
		t.Fatalf("after expiry = %v", agents)
	}

	// re-registration starts without the deleted snapshot
	_ = s.Register(ctx, "team", "removed", "")
	if got, _ := s.MessageDiagnostics(ctx, "team", "removed"); len(got) != 0 {
		t.Fatalf("re-register resurrected snapshot: %+v", got)
	}
}
