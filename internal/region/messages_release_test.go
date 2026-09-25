package region

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestReleaseMessageLeasesScopedOwner(t *testing.T) {
	s, _, decoy := deliveryFixture(t)
	ctx := context.Background()
	now := time.Now()
	s.now = func() time.Time { return now }
	m := send(t, s, MessageInput{Namespace: "team", Sender: "alice", Recipient: "bob", Body: "held back"})
	for _, ns := range []string{"other", "team"} {
		if _, err := s.ReadMessagesWithOptions(ctx, ns, "bob", MessageReadOptions{LeaseSeconds: 60, LeasedBy: "hook"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		ns, agent, owner string
		want             int64
	}{{"team", "bob", "wrong", 0}, {"team", "carol", "hook", 0}, {"team", "bob", "hook", 1}, {"team", "bob", "hook", 0}} {
		n, err := s.ReleaseMessageLeases(ctx, tc.ns, tc.agent, []string{m.ID, decoy.ID, m.ID}, tc.owner)
		if err != nil || n != tc.want {
			t.Fatalf("release %+v = %d %v", tc, n, err)
		}
	}
	rows, err := s.ReadMessages(ctx, "team", "bob", 10)
	if err != nil || len(rows) != 1 || rows[0].ID != m.ID || rows[0].LeasedBy != "" {
		t.Fatalf("not immediately available: %+v %v", rows, err)
	}
	if rows, err := s.ReadMessages(ctx, "other", "bob", 10); err != nil || len(rows) != 0 {
		t.Fatalf("decoy lease cleared: %+v %v", rows, err)
	}
	now = now.Add(time.Minute)
	if n, err := s.ReleaseMessageLeases(ctx, "other", "bob", []string{decoy.ID}, "hook"); err != nil || n != 0 {
		t.Fatalf("expired release = %d %v", n, err)
	}
	if _, err := s.ReleaseMessageLeases(ctx, "team", "bob", []string{m.ID}, ""); !errors.Is(err, ErrMessageInvalid) {
		t.Fatalf("owner required: %v", err)
	}
	if _, err := s.ReleaseMessageLeases(ctx, "team", "bob", nil, "hook"); !errors.Is(err, ErrMessageInvalid) {
		t.Fatalf("ids required: %v", err)
	}
}
