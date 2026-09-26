package region

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/bus"
	"github.com/hypervisor-io/punk-records/internal/store"
)

// newMessageTest opens a migrated temp SQLite store with a monotonic fake
// clock and registers alice, bob and carol in "team" plus dave in "other".
func newMessageTest(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "messages.db")
	s := openMessageStore(t, path)
	ctx := context.Background()
	for _, m := range []struct{ ns, agent string }{
		{"team", "alice"}, {"team", "bob"}, {"team", "carol"}, {"other", "dave"}, {"other", "bob"},
	} {
		if err := s.Register(ctx, m.ns, m.agent, "worker"); err != nil {
			t.Fatal(err)
		}
	}
	return s, path
}

func openMessageStore(t *testing.T, path string) *Store {
	t.Helper()
	db, err := store.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.MigrateUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	clk := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	return New(db, func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		clk = clk.Add(time.Millisecond)
		return clk
	})
}

func send(t *testing.T, s *Store, in MessageInput) *Message {
	t.Helper()
	m, err := s.SendMessage(context.Background(), in)
	if err != nil {
		t.Fatalf("send %+v: %v", in, err)
	}
	return m
}

func ids(ms []Message) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}

func TestSendMessagePersistsAcrossReopen(t *testing.T) {
	s, path := newMessageTest(t)
	ctx := context.Background()
	m := send(t, s, MessageInput{Namespace: "team", Sender: "alice", Recipient: "bob", Body: "hello", TaskID: "M1"})
	if m.ID == "" || m.Namespace != "team" || m.Sender != "alice" || m.Recipient != "bob" ||
		m.Body != "hello" || m.TaskID != "M1" || m.CreatedAt == "" || m.AckedAt != "" {
		t.Fatalf("sent message = %+v", m)
	}
	if _, err := store.TimeFromDB(m.CreatedAt); err != nil {
		t.Fatalf("created_at not db time: %v", err)
	}
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}

	// a restarted recipient recovers the unread message from storage
	s2 := openMessageStore(t, path)
	got, err := s2.ReadMessages(ctx, "team", "bob", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != *m {
		t.Fatalf("after reopen = %+v, want [%+v]", got, *m)
	}
}

func TestReadMessagesScopedAndNonAcking(t *testing.T) {
	s, _ := newMessageTest(t)
	ctx := context.Background()
	m1 := send(t, s, MessageInput{Namespace: "team", Sender: "alice", Recipient: "bob", Body: "one"})
	m2 := send(t, s, MessageInput{Namespace: "team", Sender: "carol", Recipient: "bob", Body: "two"})
	send(t, s, MessageInput{Namespace: "team", Sender: "alice", Recipient: "carol", Body: "for carol"})
	send(t, s, MessageInput{Namespace: "other", Sender: "dave", Recipient: "bob", Body: "other ns"})

	for i := 0; i < 2; i++ { // read never acknowledges: same result twice
		got, err := s.ReadMessages(ctx, "team", "bob", 10)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(ids(got)) != fmt.Sprint([]string{m1.ID, m2.ID}) {
			t.Fatalf("read %d = %v, want oldest-first [%s %s]", i, ids(got), m1.ID, m2.ID)
		}
	}
	if got, _ := s.ReadMessages(ctx, "team", "alice", 10); len(got) != 0 {
		t.Fatalf("sender inbox not empty: %+v", got)
	}
	if got, _ := s.ReadMessages(ctx, "other", "bob", 10); len(got) != 1 || got[0].Body != "other ns" {
		t.Fatalf("bob@other = %+v, want only the other-namespace message", got)
	}
	if got, _ := s.ReadMessages(ctx, "nowhere", "bob", 10); got == nil || len(got) != 0 {
		t.Fatalf("unknown namespace = %#v, want empty non-nil slice", got)
	}
}

func TestReadMessagesLimit(t *testing.T) {
	s, _ := newMessageTest(t)
	ctx := context.Background()
	var sent []string
	for i := 0; i < MaxMessageBatch+3; i++ {
		sent = append(sent, send(t, s, MessageInput{Namespace: "team", Sender: "alice", Recipient: "bob", Body: fmt.Sprint(i)}).ID)
	}
	got, err := s.ReadMessages(ctx, "team", "bob", 2)
	if err != nil || fmt.Sprint(ids(got)) != fmt.Sprint(sent[:2]) {
		t.Fatalf("limit 2 = %v err=%v, want %v", ids(got), err, sent[:2])
	}
	for _, lim := range []int{0, -1, MaxMessageBatch + 50} {
		got, err := s.ReadMessages(ctx, "team", "bob", lim)
		if err != nil || len(got) != MaxMessageBatch {
			t.Fatalf("limit %d returned %d err=%v, want clamp/default %d", lim, len(got), err, MaxMessageBatch)
		}
	}
}

func TestAckMessagesScopedIdempotent(t *testing.T) {
	s, _ := newMessageTest(t)
	ctx := context.Background()
	m1 := send(t, s, MessageInput{Namespace: "team", Sender: "alice", Recipient: "bob", Body: "one"})
	m2 := send(t, s, MessageInput{Namespace: "team", Sender: "alice", Recipient: "bob", Body: "two"})
	mc := send(t, s, MessageInput{Namespace: "team", Sender: "alice", Recipient: "carol", Body: "carol"})

	// another recipient, or the right recipient in the wrong namespace,
	// cannot acknowledge bob's message
	if n, err := s.AckMessages(ctx, "team", "carol", []string{m1.ID}); err != nil || n != 0 {
		t.Fatalf("carol acking bob's message = %d err=%v, want 0", n, err)
	}
	if n, err := s.AckMessages(ctx, "other", "bob", []string{m1.ID}); err != nil || n != 0 {
		t.Fatalf("cross-namespace ack = %d err=%v, want 0", n, err)
	}
	// unknown ids are ignored, duplicates count once, only supplied ids ack
	n, err := s.AckMessages(ctx, "team", "bob", []string{m1.ID, m1.ID, "no-such-id", mc.ID})
	if err != nil || n != 1 {
		t.Fatalf("ack = %d err=%v, want 1", n, err)
	}
	if n, err := s.AckMessages(ctx, "team", "bob", []string{m1.ID}); err != nil || n != 0 {
		t.Fatalf("re-ack = %d err=%v, want idempotent 0", n, err)
	}
	got, _ := s.ReadMessages(ctx, "team", "bob", 10)
	if fmt.Sprint(ids(got)) != fmt.Sprint([]string{m2.ID}) {
		t.Fatalf("after ack = %v, want only %s", ids(got), m2.ID)
	}
	if got, _ := s.ReadMessages(ctx, "team", "carol", 10); len(got) != 1 {
		t.Fatalf("carol's message acked by bob's call: %+v", got)
	}
	// an idempotent resend of an acked message reports acked_at
	k := send(t, s, MessageInput{Namespace: "team", Sender: "alice", Recipient: "bob", Body: "k", IdempotencyKey: "k1"})
	if _, err := s.AckMessages(ctx, "team", "bob", []string{k.ID}); err != nil {
		t.Fatal(err)
	}
	again := send(t, s, MessageInput{Namespace: "team", Sender: "alice", Recipient: "bob", Body: "k", IdempotencyKey: "k1"})
	if again.ID != k.ID || again.AckedAt == "" {
		t.Fatalf("resend after ack = %+v, want same id with acked_at", again)
	}
}

func TestSendMessageRequiresMembership(t *testing.T) {
	s, _ := newMessageTest(t)
	ctx := context.Background()
	cases := []MessageInput{
		{Namespace: "team", Sender: "mallory", Recipient: "bob", Body: "x"},  // sender unregistered
		{Namespace: "team", Sender: "alice", Recipient: "nobody", Body: "x"}, // recipient unregistered
		{Namespace: "team", Sender: "alice", Recipient: "dave", Body: "x"},   // dave only in other
		{Namespace: "other", Sender: "alice", Recipient: "dave", Body: "x"},  // alice only in team
		{Namespace: "ghost", Sender: "alice", Recipient: "bob", Body: "x"},   // no such region
	}
	for _, in := range cases {
		if _, err := s.SendMessage(ctx, in); !errors.Is(err, ErrMessageNotFound) {
			t.Fatalf("send %+v err=%v, want ErrMessageNotFound", in, err)
		}
	}
	// a deregistered recipient no longer receives new messages
	if err := s.Deregister(ctx, "team", "carol"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SendMessage(ctx, MessageInput{Namespace: "team", Sender: "alice", Recipient: "carol", Body: "x"}); !errors.Is(err, ErrMessageNotFound) {
		t.Fatalf("send to deregistered err=%v, want ErrMessageNotFound", err)
	}
	// a self-message is allowed (a member can leave itself a note)
	send(t, s, MessageInput{Namespace: "team", Sender: "alice", Recipient: "alice", Body: "note"})

	// same-machine sessions register distinct addresses and get distinct
	// inboxes; ':' is legal inside agent addresses
	for _, a := range []string{"opencode:ses_1", "opencode:ses_2"} {
		if err := s.Register(ctx, "team", a, ""); err != nil {
			t.Fatal(err)
		}
	}
	m := send(t, s, MessageInput{Namespace: "team", Sender: "opencode:ses_1", Recipient: "opencode:ses_2", Body: "hi"})
	if got, _ := s.ReadMessages(ctx, "team", "opencode:ses_2", 10); len(got) != 1 || got[0].ID != m.ID {
		t.Fatalf("ses_2 inbox = %+v", got)
	}
	if got, _ := s.ReadMessages(ctx, "team", "opencode:ses_1", 10); len(got) != 0 {
		t.Fatalf("ses_1 inbox = %+v, want empty", got)
	}
	if e := MessageEvent(m); e.Key != "team:opencode:ses_2" {
		t.Fatalf("event key = %q", e.Key)
	}
}

func TestMessageValidation(t *testing.T) {
	s, _ := newMessageTest(t)
	ctx := context.Background()
	long := strings.Repeat("a", MaxMessageIDLen+1)
	ok := func() MessageInput {
		return MessageInput{Namespace: "team", Sender: "alice", Recipient: "bob", Body: "hi"}
	}
	mut := func(f func(*MessageInput)) MessageInput { in := ok(); f(&in); return in }
	bad := map[string]MessageInput{
		"empty namespace":  mut(func(in *MessageInput) { in.Namespace = "" }),
		"blank sender":     mut(func(in *MessageInput) { in.Sender = "  " }),
		"empty recipient":  mut(func(in *MessageInput) { in.Recipient = "" }),
		"empty body":       mut(func(in *MessageInput) { in.Body = "" }),
		"blank body":       mut(func(in *MessageInput) { in.Body = " \n\t" }),
		"body too large":   mut(func(in *MessageInput) { in.Body = strings.Repeat("x", MaxMessageBody+1) }),
		"invalid utf8":     mut(func(in *MessageInput) { in.Body = "bad \xff" }),
		"nul in body":      mut(func(in *MessageInput) { in.Body = "a\x00b" }),
		"long namespace":   mut(func(in *MessageInput) { in.Namespace = long }),
		"long sender":      mut(func(in *MessageInput) { in.Sender = long }),
		"long recipient":   mut(func(in *MessageInput) { in.Recipient = long }),
		"long task":        mut(func(in *MessageInput) { in.TaskID = long }),
		"long reply":       mut(func(in *MessageInput) { in.ReplyTo = long }),
		"long idempotency": mut(func(in *MessageInput) { in.IdempotencyKey = long }),
		"control in agent": mut(func(in *MessageInput) { in.Recipient = "bob\n" }),
		// the bus hint key is ns+":"+recipient and consumers split on
		// the first colon, so a colon in the namespace would misroute
		"colon in namespace": mut(func(in *MessageInput) { in.Namespace = "te:am" }),
	}
	for name, in := range bad {
		if _, err := s.SendMessage(ctx, in); !errors.Is(err, ErrMessageInvalid) {
			t.Errorf("%s: err=%v, want ErrMessageInvalid", name, err)
		}
	}
	// exact bounds are accepted
	edge := ok()
	edge.Body = strings.Repeat("x", MaxMessageBody)
	edge.TaskID = strings.Repeat("t", MaxMessageIDLen)
	edge.IdempotencyKey = strings.Repeat("k", MaxMessageIDLen)
	send(t, s, edge)

	if _, err := s.ReadMessages(ctx, "", "bob", 1); !errors.Is(err, ErrMessageInvalid) {
		t.Errorf("read empty ns err=%v", err)
	}
	if _, err := s.ReadMessages(ctx, "team", long, 1); !errors.Is(err, ErrMessageInvalid) {
		t.Errorf("read long agent err=%v", err)
	}
	if _, err := s.AckMessages(ctx, "team", "bob", nil); !errors.Is(err, ErrMessageInvalid) {
		t.Errorf("ack no ids err=%v", err)
	}
	if _, err := s.AckMessages(ctx, "team", "bob", []string{""}); !errors.Is(err, ErrMessageInvalid) {
		t.Errorf("ack empty id err=%v", err)
	}
	tooMany := make([]string, MaxMessageBatch+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprint("id-", i)
	}
	if _, err := s.AckMessages(ctx, "team", "bob", tooMany); !errors.Is(err, ErrMessageInvalid) {
		t.Errorf("ack over batch err=%v", err)
	}
	if _, err := s.AckMessages(ctx, "team", "", []string{"x"}); !errors.Is(err, ErrMessageInvalid) {
		t.Errorf("ack empty agent err=%v", err)
	}
	if _, err := s.WaitMessages(ctx, nil, "team", "", 1, 0); !errors.Is(err, ErrMessageInvalid) {
		t.Errorf("wait empty agent err=%v", err)
	}
}

func TestSendMessageIdempotency(t *testing.T) {
	s, _ := newMessageTest(t)
	ctx := context.Background()
	in := MessageInput{Namespace: "team", Sender: "alice", Recipient: "bob", Body: "do M1", TaskID: "M1", IdempotencyKey: "retry-1"}
	first := send(t, s, in)
	retry := send(t, s, in)
	if *retry != *first {
		t.Fatalf("retry = %+v, want identical %+v", retry, first)
	}
	if got, _ := s.ReadMessages(ctx, "team", "bob", 10); len(got) != 1 {
		t.Fatalf("retry duplicated delivery: %+v", got)
	}

	// conflicting reuse of the key is rejected, for every content field
	for name, f := range map[string]func(*MessageInput){
		"body":      func(m *MessageInput) { m.Body = "different" },
		"recipient": func(m *MessageInput) { m.Recipient = "carol" },
		"task":      func(m *MessageInput) { m.TaskID = "M2" },
		"reply":     func(m *MessageInput) { m.ReplyTo = first.ID },
	} {
		c := in
		f(&c)
		if _, err := s.SendMessage(ctx, c); !errors.Is(err, ErrMessageConflict) {
			t.Errorf("conflicting %s reuse err=%v, want ErrMessageConflict", name, err)
		}
	}

	// the key is scoped per (namespace, sender): other senders and other
	// namespaces reuse it freely
	c := in
	c.Sender = "carol"
	if m := send(t, s, c); m.ID == first.ID {
		t.Fatal("other sender with same key deduplicated onto alice's message")
	}
	if err := s.Register(ctx, "other", "alice", ""); err != nil {
		t.Fatal(err)
	}
	o := in
	o.Namespace = "other"
	if m := send(t, s, o); m.ID == first.ID || m.Namespace != "other" {
		t.Fatalf("other namespace with same key = %+v", m)
	}
	// no key: every send is a new message
	nk := in
	nk.IdempotencyKey = ""
	a, b := send(t, s, nk), send(t, s, nk)
	if a.ID == b.ID {
		t.Fatal("keyless sends deduplicated")
	}
}

func TestSendMessageReplyLinks(t *testing.T) {
	s, _ := newMessageTest(t)
	ctx := context.Background()
	q := send(t, s, MessageInput{Namespace: "team", Sender: "alice", Recipient: "bob", Body: "question", TaskID: "M1"})
	r := send(t, s, MessageInput{Namespace: "team", Sender: "bob", Recipient: "alice", Body: "answer", TaskID: "M1", ReplyTo: q.ID})
	if r.ReplyTo != q.ID || r.TaskID != "M1" {
		t.Fatalf("reply = %+v", r)
	}
	got, _ := s.ReadMessages(ctx, "team", "alice", 10)
	if len(got) != 1 || got[0].ReplyTo != q.ID {
		t.Fatalf("alice inbox = %+v, want threaded reply", got)
	}
	// replies to a missing id, or to a message in another namespace, fail
	if _, err := s.SendMessage(ctx, MessageInput{Namespace: "team", Sender: "bob", Recipient: "alice", Body: "x", ReplyTo: "missing"}); !errors.Is(err, ErrMessageNotFound) {
		t.Fatalf("reply to missing err=%v, want ErrMessageNotFound", err)
	}
	od := send(t, s, MessageInput{Namespace: "other", Sender: "dave", Recipient: "bob", Body: "other"})
	if _, err := s.SendMessage(ctx, MessageInput{Namespace: "team", Sender: "bob", Recipient: "alice", Body: "x", ReplyTo: od.ID}); !errors.Is(err, ErrMessageNotFound) {
		t.Fatalf("cross-namespace reply err=%v, want ErrMessageNotFound", err)
	}
}

func TestMessageEventHelpers(t *testing.T) {
	m := &Message{ID: "abc", Namespace: "team", Recipient: "bob", Sender: "alice", Body: "secret"}
	e := MessageEvent(m)
	if MessageEventKind != "agent_message" || e.Kind != MessageEventKind || e.Key != "team:bob" || e.Key != MessageEventKey("team", "bob") {
		t.Fatalf("event = %+v", e)
	}
	for k, v := range e.Data {
		if strings.Contains(v, "secret") {
			t.Fatalf("event data %s leaks body", k)
		}
	}
}

// setPollInterval shortens or lengthens the server-side reconciliation poll
// for one test and restores it afterwards.
func setPollInterval(t *testing.T, d time.Duration) {
	t.Helper()
	old := messagePollInterval
	messagePollInterval = d
	t.Cleanup(func() { messagePollInterval = old })
}

type waitResult struct {
	msgs []Message
	err  error
	took time.Duration
}

func startWait(s *Store, ctx context.Context, b *bus.Bus, ns, agent string, timeout time.Duration) <-chan waitResult {
	out := make(chan waitResult, 1)
	go func() {
		start := time.Now()
		ms, err := s.WaitMessages(ctx, b, ns, agent, 10, timeout)
		out <- waitResult{ms, err, time.Since(start)}
	}()
	return out
}

func recvWait(t *testing.T, ch <-chan waitResult, within time.Duration) waitResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(within):
		t.Fatalf("WaitMessages did not return within %v", within)
		return waitResult{}
	}
}

func TestWaitMessagesReturnsExistingUnreadImmediately(t *testing.T) {
	s, _ := newMessageTest(t)
	setPollInterval(t, time.Hour)
	m := send(t, s, MessageInput{Namespace: "team", Sender: "alice", Recipient: "bob", Body: "queued"})
	r := recvWait(t, startWait(s, context.Background(), bus.New(), "team", "bob", time.Hour), 2*time.Second)
	if r.err != nil || len(r.msgs) != 1 || r.msgs[0].ID != m.ID {
		t.Fatalf("wait = %+v err=%v", r.msgs, r.err)
	}
}

func TestWaitMessagesWakesOnBusHint(t *testing.T) {
	s, _ := newMessageTest(t)
	setPollInterval(t, time.Hour) // polling cannot be what wakes it
	b := bus.New()
	ch := startWait(s, context.Background(), b, "team", "bob", time.Hour)
	time.Sleep(50 * time.Millisecond) // let the waiter subscribe and find nothing

	// unrelated hints (other agent, other kind) do not end the wait
	other := send(t, s, MessageInput{Namespace: "team", Sender: "alice", Recipient: "carol", Body: "not bob"})
	b.Publish(MessageEvent(other))
	b.Publish(bus.Event{Kind: "memory", Key: "team:bob"})
	select {
	case r := <-ch:
		t.Fatalf("woke for unrelated event: %+v err=%v", r.msgs, r.err)
	case <-time.After(50 * time.Millisecond):
	}

	m := send(t, s, MessageInput{Namespace: "team", Sender: "alice", Recipient: "bob", Body: "wake"})
	b.Publish(MessageEvent(m))
	r := recvWait(t, ch, 2*time.Second)
	if r.err != nil || len(r.msgs) != 1 || r.msgs[0].ID != m.ID {
		t.Fatalf("wait = %+v err=%v", r.msgs, r.err)
	}
}

func TestWaitMessagesRecoversLostEventByPolling(t *testing.T) {
	s, _ := newMessageTest(t)
	setPollInterval(t, 20*time.Millisecond)
	for _, b := range []*bus.Bus{bus.New(), nil} { // lost hint, and no bus at all
		ch := startWait(s, context.Background(), b, "team", "bob", time.Hour)
		time.Sleep(30 * time.Millisecond)
		m := send(t, s, MessageInput{Namespace: "team", Sender: "alice", Recipient: "bob", Body: "no hint"})
		r := recvWait(t, ch, 2*time.Second)
		if r.err != nil || len(r.msgs) != 1 || r.msgs[0].ID != m.ID {
			t.Fatalf("bus=%v wait = %+v err=%v", b != nil, r.msgs, r.err)
		}
		if _, err := s.AckMessages(context.Background(), "team", "bob", []string{m.ID}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWaitMessagesHintForAckedMessageKeepsWaiting(t *testing.T) {
	s, _ := newMessageTest(t)
	setPollInterval(t, time.Hour)
	b := bus.New()
	m := send(t, s, MessageInput{Namespace: "team", Sender: "alice", Recipient: "bob", Body: "x"})
	if _, err := s.AckMessages(context.Background(), "team", "bob", []string{m.ID}); err != nil {
		t.Fatal(err)
	}
	ch := startWait(s, context.Background(), b, "team", "bob", 150*time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	b.Publish(MessageEvent(m)) // stale hint: storage says nothing unread
	r := recvWait(t, ch, 2*time.Second)
	if r.err != nil || r.msgs == nil || len(r.msgs) != 0 || r.took < 140*time.Millisecond {
		t.Fatalf("stale hint wait = %#v err=%v took=%v, want empty after timeout", r.msgs, r.err, r.took)
	}
}

func TestWaitMessagesTimeoutAndCancellation(t *testing.T) {
	s, _ := newMessageTest(t)
	setPollInterval(t, 10*time.Millisecond)

	// zero timeout: one non-blocking check
	r := recvWait(t, startWait(s, context.Background(), bus.New(), "team", "bob", 0), time.Second)
	if r.err != nil || r.msgs == nil || len(r.msgs) != 0 {
		t.Fatalf("zero timeout = %#v err=%v", r.msgs, r.err)
	}
	// bounded timeout with nothing arriving returns empty, not an error
	r = recvWait(t, startWait(s, context.Background(), bus.New(), "team", "bob", 60*time.Millisecond), 2*time.Second)
	if r.err != nil || len(r.msgs) != 0 || r.took < 50*time.Millisecond {
		t.Fatalf("timeout = %+v err=%v took=%v", r.msgs, r.err, r.took)
	}
	// caller cancellation ends the wait promptly with the context error
	ctx, cancel := context.WithCancel(context.Background())
	ch := startWait(s, ctx, bus.New(), "team", "bob", time.Hour)
	time.Sleep(30 * time.Millisecond)
	cancel()
	r = recvWait(t, ch, time.Second)
	if !errors.Is(r.err, context.Canceled) {
		t.Fatalf("cancel err=%v, want context.Canceled", r.err)
	}
	// an expired deadline is honoured even with a longer timeout
	dctx, dcancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer dcancel()
	r = recvWait(t, startWait(s, dctx, nil, "team", "bob", time.Hour), time.Second)
	if !errors.Is(r.err, context.DeadlineExceeded) {
		t.Fatalf("deadline err=%v, want context.DeadlineExceeded", r.err)
	}
}

func TestListMessagesOrderingAndAckedIncluded(t *testing.T) {
	s, _ := newMessageTest(t)
	ctx := context.Background()
	m1 := send(t, s, MessageInput{Namespace: "team", Sender: "alice", Recipient: "bob", Body: "one"})
	m2 := send(t, s, MessageInput{Namespace: "team", Sender: "bob", Recipient: "alice", Body: "two"})
	m3 := send(t, s, MessageInput{Namespace: "team", Sender: "alice", Recipient: "carol", Body: "three"})
	send(t, s, MessageInput{Namespace: "other", Sender: "dave", Recipient: "bob", Body: "other ns"})

	if _, err := s.AckMessages(ctx, "team", "alice", []string{m2.ID}); err != nil {
		t.Fatalf("ack: %v", err)
	}

	got, err := s.ListMessages(ctx, "team", MessageLogOptions{})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if fmt.Sprint(ids(got)) != fmt.Sprint([]string{m3.ID, m2.ID, m1.ID}) {
		t.Fatalf("order = %v, want newest first [%s %s %s]", ids(got), m3.ID, m2.ID, m1.ID)
	}
	if got[0].Seq <= got[1].Seq || got[1].Seq <= got[2].Seq {
		t.Fatalf("seq not descending: %+v", got)
	}
	// the acked row (m2) is still present, with acked_at set
	if got[1].ID != m2.ID || got[1].AckedAt == "" {
		t.Fatalf("acked message missing from log or acked_at empty: %+v", got[1])
	}
	// unacked rows carry no acked_at
	if got[0].AckedAt != "" || got[2].AckedAt != "" {
		t.Fatalf("unacked rows should have empty acked_at: %+v %+v", got[0], got[2])
	}
}

func TestListMessagesAgentFilterMatchesEitherSide(t *testing.T) {
	s, _ := newMessageTest(t)
	ctx := context.Background()
	m1 := send(t, s, MessageInput{Namespace: "team", Sender: "alice", Recipient: "bob", Body: "one"})
	m2 := send(t, s, MessageInput{Namespace: "team", Sender: "carol", Recipient: "alice", Body: "two"})
	send(t, s, MessageInput{Namespace: "team", Sender: "bob", Recipient: "carol", Body: "three"})

	got, err := s.ListMessages(ctx, "team", MessageLogOptions{Agent: "alice"})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if fmt.Sprint(ids(got)) != fmt.Sprint([]string{m2.ID, m1.ID}) {
		t.Fatalf("agent filter (recipient+sender) = %v, want [%s %s]", ids(got), m2.ID, m1.ID)
	}
}

func TestListMessagesLimitClamp(t *testing.T) {
	cases := []struct {
		in, want int
	}{
		{0, DefaultMessageLogBatch},
		{-5, DefaultMessageLogBatch},
		{50, 50},
		{MaxMessageLogBatch, MaxMessageLogBatch},
		{MaxMessageLogBatch + 1, MaxMessageLogBatch},
		{100000, MaxMessageLogBatch},
	}
	for _, c := range cases {
		if got := clampMessageLogLimit(c.in); got != c.want {
			t.Errorf("clampMessageLogLimit(%d) = %d, want %d", c.in, got, c.want)
		}
	}

	// exercised end-to-end too: an explicit limit is honoured against real rows
	s, _ := newMessageTest(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		send(t, s, MessageInput{Namespace: "team", Sender: "alice", Recipient: "bob", Body: fmt.Sprint(i)})
	}
	got, err := s.ListMessages(ctx, "team", MessageLogOptions{Limit: 2})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("limited len = %d, want 2", len(got))
	}
}

func TestListMessagesPagingWithBefore(t *testing.T) {
	s, _ := newMessageTest(t)
	ctx := context.Background()
	var sent []*Message
	for i := 0; i < 5; i++ {
		sent = append(sent, send(t, s, MessageInput{Namespace: "team", Sender: "alice", Recipient: "bob", Body: fmt.Sprint(i)}))
	}

	page1, err := s.ListMessages(ctx, "team", MessageLogOptions{Limit: 2})
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if fmt.Sprint(ids(page1)) != fmt.Sprint([]string{sent[4].ID, sent[3].ID}) {
		t.Fatalf("page1 = %v, want [%s %s]", ids(page1), sent[4].ID, sent[3].ID)
	}

	page2, err := s.ListMessages(ctx, "team", MessageLogOptions{Limit: 2, BeforeSeq: page1[len(page1)-1].Seq})
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if fmt.Sprint(ids(page2)) != fmt.Sprint([]string{sent[2].ID, sent[1].ID}) {
		t.Fatalf("page2 = %v, want [%s %s]", ids(page2), sent[2].ID, sent[1].ID)
	}

	page3, err := s.ListMessages(ctx, "team", MessageLogOptions{Limit: 2, BeforeSeq: page2[len(page2)-1].Seq})
	if err != nil {
		t.Fatalf("page3: %v", err)
	}
	if fmt.Sprint(ids(page3)) != fmt.Sprint([]string{sent[0].ID}) {
		t.Fatalf("page3 = %v, want [%s]", ids(page3), sent[0].ID)
	}
}
