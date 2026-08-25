package task

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/store"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time {
	c.t = c.t.Add(time.Millisecond)
	return c.t
}

func newTest(t *testing.T) (*Ledger, *store.DB, *fakeClock) {
	t.Helper()
	driver, dsn := "sqlite", filepath.Join(t.TempDir(), "task.db")
	if pg := os.Getenv("PUNK_TEST_PG_DSN"); pg != "" {
		driver, dsn = "postgres", pg
	}
	db, err := store.Open(driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if driver == "postgres" {
		for {
			n, err := db.MigrateDown(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				break
			}
		}
	}
	if _, err := db.MigrateUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	clk := &fakeClock{t: time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)}
	return NewLedger(db, clk.Now), db, clk
}

func TestTransitions(t *testing.T) {
	legal := [][2]string{
		{StatusSubmitted, StatusWorking}, {StatusSubmitted, StatusCanceled},
		{StatusWorking, StatusCompleted}, {StatusWorking, StatusFailed},
		{StatusWorking, StatusInputRequired}, {StatusWorking, StatusCanceled},
		{StatusWorking, StatusSubmitted},
		{StatusInputRequired, StatusWorking}, {StatusInputRequired, StatusCanceled},
	}
	for _, c := range legal {
		if err := CheckTransition(c[0], c[1]); err != nil {
			t.Errorf("legal %s->%s rejected: %v", c[0], c[1], err)
		}
	}
	illegal := [][2]string{
		{StatusCompleted, StatusWorking}, {StatusFailed, StatusWorking},
		{StatusCanceled, StatusSubmitted}, {StatusSubmitted, StatusCompleted},
		{StatusSubmitted, StatusInputRequired},
	}
	for _, c := range illegal {
		err := CheckTransition(c[0], c[1])
		var bad *ErrBadTransition
		if !errors.As(err, &bad) {
			t.Errorf("illegal %s->%s: err = %v, want ErrBadTransition", c[0], c[1], err)
		}
	}
	if err := CheckTransition("nonsense", StatusWorking); err == nil {
		t.Error("unknown status accepted")
	}
}

func TestLedgerNoDriftBetweenStatusAndEvents(t *testing.T) {
	l, db, _ := newTest(t)
	ctx := context.Background()

	tk, created, err := l.Submit(ctx, SubmitInput{Source: "test", Budget: Budget{Tokens: 10}})
	if err != nil || !created {
		t.Fatalf("submit: %v created=%v", err, created)
	}
	if err := l.SetStatus(ctx, tk.ID, StatusWorking, "test", "go"); err != nil {
		t.Fatal(err)
	}
	if err := l.SetStatus(ctx, tk.ID, StatusCompleted, "test", "done"); err != nil {
		t.Fatal(err)
	}
	// illegal after terminal
	if err := l.SetStatus(ctx, tk.ID, StatusWorking, "test", "zombie"); err == nil {
		t.Fatal("terminal task accepted transition")
	}

	got, events, err := l.Get(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	// last status_change event must equal the row status
	var last StatusChangePayload
	for _, e := range events {
		if e.Type == EventStatusChange {
			if err := json.Unmarshal(e.Payload, &last); err != nil {
				t.Fatal(err)
			}
		}
	}
	if last.To != got.Status {
		t.Fatalf("drift: row status %q, last event to %q", got.Status, last.To)
	}
	// seq strictly monotonic from 1
	for i, e := range events {
		if e.Seq != int64(i+1) {
			t.Fatalf("seq gap at %d: %d", i, e.Seq)
		}
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM task_events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != len(events) {
		t.Fatalf("events count mismatch: table %d, read %d", n, len(events))
	}
}

func TestSubmitDedup(t *testing.T) {
	l, _, _ := newTest(t)
	ctx := context.Background()

	t1, created, err := l.Submit(ctx, SubmitInput{ExternalRef: "incident:42", Source: "acme"})
	if err != nil || !created {
		t.Fatalf("first: %v %v", err, created)
	}
	t2, created, err := l.Submit(ctx, SubmitInput{ExternalRef: "incident:42", Source: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	if created || t2.ID != t1.ID {
		t.Fatalf("dedup failed: created=%v id=%s want %s", created, t2.ID, t1.ID)
	}

	// closing the task frees the ref
	if err := l.SetStatus(ctx, t1.ID, StatusCanceled, "test", ""); err != nil {
		t.Fatal(err)
	}
	t3, created, err := l.Submit(ctx, SubmitInput{ExternalRef: "incident:42", Source: "acme"})
	if err != nil || !created || t3.ID == t1.ID {
		t.Fatalf("closed ref not reusable: %v created=%v", err, created)
	}

	// tasks without refs never dedup
	a, _, err := l.Submit(ctx, SubmitInput{Source: "x"})
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := l.Submit(ctx, SubmitInput{Source: "x"})
	if err != nil || a.ID == b.ID {
		t.Fatalf("ref-less dedup happened: %v", err)
	}
}

func TestClaimAndLeaseReap(t *testing.T) {
	l, _, clk := newTest(t)
	ctx := context.Background()

	var verID int64
	if err := l.db.QueryRowContext(ctx, l.db.Rebind(`
		INSERT INTO agent_versions (name, version, content_hash, loaded_at, active)
		VALUES ('database', '0.1.0', 'testhash', $1, TRUE) RETURNING id`),
		store.TimeToDB(clk.Now())).Scan(&verID); err != nil {
		t.Fatal(err)
	}

	tk, _, err := l.Submit(ctx, SubmitInput{Source: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Claim(ctx, tk.ID, "database", verID, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	got, _, err := l.Get(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusWorking || got.AgentName != "database" || got.AgentVersionID != verID || got.LeaseExpiresAt == nil {
		t.Fatalf("claim state: %+v", got)
	}

	// nothing to reap before expiry
	if n, err := l.ReapExpired(ctx); err != nil || n != 0 {
		t.Fatalf("premature reap: n=%d err=%v", n, err)
	}
	clk.t = clk.t.Add(10 * time.Minute)
	n, err := l.ReapExpired(ctx)
	if err != nil || n != 1 {
		t.Fatalf("reap: n=%d err=%v", n, err)
	}
	got, events, err := l.Get(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusSubmitted {
		t.Fatalf("reaped status = %s, want submitted", got.Status)
	}
	// history preserved: error event present
	foundErr := false
	for _, e := range events {
		if e.Type == EventError {
			foundErr = true
		}
	}
	if !foundErr {
		t.Fatal("reap left no error event")
	}
}

func TestEventPayloadRoundTrip(t *testing.T) {
	p := StatusChangePayload{From: "working", To: "completed", Reason: "ok"}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var back StatusChangePayload
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back != p {
		t.Fatalf("round trip: %+v != %+v", back, p)
	}
}

func TestLeaseExtendKeepsReaperAway(t *testing.T) {
	l, _, clk := newTest(t)
	ctx := context.Background()
	var verID int64
	if err := l.db.QueryRowContext(ctx, l.db.Rebind(`
		INSERT INTO agent_versions (name, version, content_hash, loaded_at, active)
		VALUES ('database', '0.1.0', 'h2', $1, TRUE) RETURNING id`),
		store.TimeToDB(clk.Now())).Scan(&verID); err != nil {
		t.Fatal(err)
	}
	tk, _, err := l.Submit(ctx, SubmitInput{Source: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Claim(ctx, tk.ID, "database", verID, time.Minute); err != nil {
		t.Fatal(err)
	}
	// past original lease, but extended in time
	clk.t = clk.t.Add(50 * time.Second)
	if err := l.ExtendLease(ctx, tk.ID, time.Minute); err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(30 * time.Second) // 80s after claim, 30s after extend
	if n, err := l.ReapExpired(ctx); err != nil || n != 0 {
		t.Fatalf("extended lease reaped: n=%d err=%v", n, err)
	}
	// extension only works on working tasks
	if err := l.SetStatus(ctx, tk.ID, StatusCompleted, "test", ""); err != nil {
		t.Fatal(err)
	}
	if err := l.ExtendLease(ctx, tk.ID, time.Minute); err == nil {
		t.Fatal("extend on completed task accepted")
	}
}
