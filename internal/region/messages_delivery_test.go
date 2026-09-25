package region

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func deliveryFixture(t *testing.T) (*Store, string, *Message) {
	t.Helper()
	s, path := newMessageTest(t)
	if err := s.Register(context.Background(), "other", "alice", ""); err != nil {
		t.Fatal(err)
	}
	// Decoy is older than every target row, so a missing namespace filter fails.
	decoy := send(t, s, MessageInput{Namespace: "other", Sender: "alice", Recipient: "bob", Body: "decoy"})
	return s, path, decoy
}

func TestMessageDeliveryLeaseExpiryOwnershipAndRecovery(t *testing.T) {
	s, _, decoy := deliveryFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	m := send(t, s, MessageInput{Namespace: "team", Sender: "alice", Recipient: "bob", Body: "full body"})
	opts := MessageReadOptions{LeaseSeconds: 60, LeasedBy: "hook-1"}
	got, err := s.ReadMessagesWithOptions(ctx, "team", "bob", opts)
	if err != nil || len(got) != 1 || got[0].ID != m.ID || got[0].LeasedBy != "hook-1" || got[0].LeasedUntil == "" {
		t.Fatalf("lease = %+v, %v", got, err)
	}
	for _, owner := range []string{"hook-1", "hook-2"} {
		opts.LeasedBy = owner
		if got, err := s.ReadMessagesWithOptions(ctx, "team", "bob", opts); err != nil || len(got) != 0 {
			t.Fatalf("second lease = %+v, %v", got, err)
		}
	}
	if got, err := s.ReadMessages(ctx, "team", "bob", 10); err != nil || len(got) != 0 {
		t.Fatalf("legacy read exposed leased row: %+v %v", got, err)
	}
	if n, err := s.CountUnreadMessages(ctx, "team", "bob"); err != nil || n != 1 {
		t.Fatalf("leased unread count = %d %v", n, err)
	}
	if n, err := s.AckMessagesWithLease(ctx, "team", "bob", []string{m.ID, decoy.ID}, "hook-2"); err != nil || n != 0 {
		t.Fatalf("wrong owner ACK = %d %v", n, err)
	}
	now = now.Add(time.Minute)
	if n, err := s.AckMessagesWithLease(ctx, "team", "bob", []string{m.ID}, "hook-1"); err != nil || n != 0 {
		t.Fatalf("expired owner ACK = %d %v", n, err)
	}
	got, err = s.ReadMessagesWithOptions(ctx, "team", "bob", opts)
	if err != nil || len(got) != 1 || got[0].LeasedBy != "hook-2" {
		t.Fatalf("expired lease not recovered: %+v %v", got, err)
	}
	if n, err := s.AckMessagesWithLease(ctx, "team", "bob", []string{m.ID, decoy.ID}, "hook-2"); err != nil || n != 1 {
		t.Fatalf("owner ACK = %d %v", n, err)
	}
	got, err = s.ReadMessagesWithOptions(ctx, "team", "bob", MessageReadOptions{ID: m.ID})
	if err != nil || len(got) != 1 || got[0].Body != m.Body || got[0].AckedAt == "" || got[0].LeasedBy != "" || got[0].LeasedUntil != "" {
		t.Fatalf("full ACKed body recovery / lease clear = %+v %v", got, err)
	}
	if got, err := s.ReadMessagesWithOptions(ctx, "other", "bob", MessageReadOptions{ID: m.ID}); err != nil || len(got) != 0 {
		t.Fatalf("ID lookup crossed namespace: %+v %v", got, err)
	}
	if got, err := s.ReadMessagesWithOptions(ctx, "team", "carol", MessageReadOptions{ID: m.ID}); err != nil || len(got) != 0 {
		t.Fatalf("ID lookup crossed recipient: %+v %v", got, err)
	}
	if got, err := s.ReadMessages(ctx, "other", "bob", 10); err != nil || len(got) != 1 || got[0].ID != decoy.ID {
		t.Fatalf("decoy changed: %+v %v", got, err)
	}
	// Legacy ACK remains valid even for leased rows.
	if _, err := s.ReadMessagesWithOptions(ctx, "other", "bob", opts); err != nil {
		t.Fatal(err)
	}
	if n, err := s.AckMessages(ctx, "other", "bob", []string{decoy.ID}); err != nil || n != 1 {
		t.Fatalf("legacy ACK = %d %v", n, err)
	}
}

func TestMessageDeliveryConcurrentDBHandles(t *testing.T) {
	s, path, decoy := deliveryFixture(t)
	ctx := context.Background()
	s2 := openMessageStore(t, path)
	// Immutable clocks, safe under concurrent calls.
	now := time.Now()
	s.now = func() time.Time { return now }
	s2.now = s.now
	for i := 0; i < 40; i++ {
		send(t, s, MessageInput{Namespace: "team", Sender: "alice", Recipient: "bob", Body: fmt.Sprint(i)})
	}
	start := make(chan struct{})
	results := make(chan []Message, 2)
	errs := make(chan error, 2)
	for i, db := range []*Store{s, s2} {
		go func(s *Store, owner string) {
			<-start
			rows, err := s.ReadMessagesWithOptions(ctx, "team", "bob", MessageReadOptions{Limit: 20, LeaseSeconds: 60, LeasedBy: owner})
			results <- rows
			errs <- err
		}(db, fmt.Sprint("reader-", i))
	}
	close(start)
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		rows := <-results
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		if len(rows) != 20 {
			t.Fatalf("batch = %d, want 20", len(rows))
		}
		for _, m := range rows {
			if seen[m.ID] || m.ID == decoy.ID {
				t.Fatalf("duplicate/decoy lease: %+v", m)
			}
			seen[m.ID] = true
		}
	}
	if got, err := s2.ReadMessages(ctx, "other", "bob", 100); err != nil || len(got) != 1 || got[0].ID != decoy.ID {
		t.Fatalf("decoy = %+v %v", got, err)
	}
}

func TestMessageDeliveryBacklogAtomicAndIdempotent(t *testing.T) {
	s, path, _ := deliveryFixture(t)
	s2 := openMessageStore(t, path)
	s.MaxUnreadPerRecipient, s2.MaxUnreadPerRecipient = 2, 2
	now := time.Now()
	s.now = func() time.Time { return now }
	s2.now = s.now
	ctx := context.Background()
	in := MessageInput{Namespace: "team", Sender: "alice", Recipient: "bob", Body: "retry", IdempotencyKey: "key"}
	first := send(t, s, in)
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			db := []*Store{s, s2}[i%2]
			_, err := db.SendMessage(ctx, MessageInput{Namespace: "team", Sender: "alice", Recipient: "bob", Body: fmt.Sprint(i)})
			errs <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	accepted := 0
	for err := range errs {
		if err == nil {
			accepted++
		} else if !errors.Is(err, ErrMessageBacklog) {
			t.Fatal(err)
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted %d extra, want 1", accepted)
	}
	if n, err := s.CountUnreadMessages(ctx, "team", "bob"); err != nil || n != 2 {
		t.Fatalf("count = %d %v", n, err)
	}
	if again := send(t, s2, in); again.ID != first.ID {
		t.Fatal("retry did not bypass cap")
	}
	in.Body = "conflict"
	if _, err := s.SendMessage(ctx, in); !errors.Is(err, ErrMessageConflict) {
		t.Fatalf("conflict hidden by cap: %v", err)
	}
	if n, err := s.CountUnreadMessages(ctx, "other", "bob"); err != nil || n != 1 {
		t.Fatalf("decoy count = %d %v", n, err)
	}
	if _, err := s.AckMessages(ctx, "team", "bob", []string{first.ID}); err != nil {
		t.Fatal(err)
	}
	send(t, s, MessageInput{Namespace: "team", Sender: "alice", Recipient: "bob", Body: "space after ACK"})
}

func TestMessageDeliverySentAndCount(t *testing.T) {
	s, _, decoy := deliveryFixture(t)
	ctx := context.Background()
	first := send(t, s, MessageInput{Namespace: "team", Sender: "alice", Recipient: "bob", Body: "one"})
	second := send(t, s, MessageInput{Namespace: "team", Sender: "alice", Recipient: "carol", Body: "two"})
	if _, err := s.AckMessages(ctx, "team", "bob", []string{first.ID}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadMessagesWithOptions(ctx, "team", "alice", MessageReadOptions{Box: "sent"})
	if err != nil || len(got) != 2 || got[0].ID != first.ID || got[0].AckedAt == "" || got[1].ID != second.ID || got[1].AckedAt != "" {
		t.Fatalf("sent = %+v %v", got, err)
	}
	for _, tc := range []struct {
		ns, agent string
		want      int64
	}{{"team", "bob", 0}, {"team", "carol", 1}, {"other", "bob", 1}} {
		if n, err := s.CountUnreadMessages(ctx, tc.ns, tc.agent); err != nil || n != tc.want {
			t.Fatalf("count %s/%s = %d %v", tc.ns, tc.agent, n, err)
		}
	}
	got, err = s.ReadMessagesWithOptions(ctx, "other", "alice", MessageReadOptions{Box: "sent"})
	if err != nil || len(got) != 1 || got[0].ID != decoy.ID {
		t.Fatalf("decoy sent = %+v %v", got, err)
	}
}

func TestMessageDeliveryOptionsValidation(t *testing.T) {
	s, _, _ := deliveryFixture(t)
	for _, opts := range []MessageReadOptions{{LeaseSeconds: -1}, {LeaseSeconds: 301, LeasedBy: "x"}, {LeaseSeconds: 60}, {LeasedBy: "x"}, {Box: "wrong"}, {Box: "sent", LeaseSeconds: 1, LeasedBy: "x"}, {ID: "id", LeaseSeconds: 1, LeasedBy: "x"}} {
		if _, err := s.ReadMessagesWithOptions(context.Background(), "team", "bob", opts); !errors.Is(err, ErrMessageInvalid) {
			t.Fatalf("accepted %+v: %v", opts, err)
		}
	}
}

func TestMessageDeliveryDefaultBacklogCap(t *testing.T) {
	s, _, _ := deliveryFixture(t)
	if s.MaxUnreadPerRecipient != 200 || s.MessageRetention != 30*24*time.Hour {
		t.Fatalf("defaults: cap=%d retention=%v", s.MaxUnreadPerRecipient, s.MessageRetention)
	}
	for i := 0; i < 200; i++ {
		send(t, s, MessageInput{Namespace: "team", Sender: "alice", Recipient: "bob", Body: fmt.Sprint(i)})
	}
	if _, err := s.SendMessage(context.Background(), MessageInput{Namespace: "team", Sender: "alice", Recipient: "bob", Body: "201st"}); !errors.Is(err, ErrMessageBacklog) {
		t.Fatalf("default cap not enforced: %v", err)
	}
	if n, err := s.CountUnreadMessages(context.Background(), "other", "bob"); err != nil || n != 1 {
		t.Fatalf("decoy=%d %v", n, err)
	}
}
