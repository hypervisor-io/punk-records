package bus

import (
	"sync"
	"testing"
	"time"
)

func recv(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case e, ok := <-ch:
		if !ok {
			t.Fatal("channel closed unexpectedly")
		}
		return e
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
	}
	return Event{}
}

func TestPublishFansOutToAllSubscribers(t *testing.T) {
	b := New()
	c1, cancel1 := b.Subscribe()
	defer cancel1()
	c2, cancel2 := b.Subscribe()
	defer cancel2()

	want := Event{Kind: "task_status", Key: "t-1", Data: map[string]string{"status": "working"}}
	b.Publish(want)

	for _, ch := range []<-chan Event{c1, c2} {
		got := recv(t, ch)
		if got.Kind != want.Kind || got.Key != want.Key || got.Data["status"] != "working" {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	}
}

func TestPublishPreservesOrderPerSubscriber(t *testing.T) {
	b := New()
	ch, cancel := b.Subscribe()
	defer cancel()

	for i := 0; i < 10; i++ {
		b.Publish(Event{Kind: "memory", Key: string(rune('a' + i))})
	}
	for i := 0; i < 10; i++ {
		if got := recv(t, ch).Key; got != string(rune('a'+i)) {
			t.Fatalf("event %d: got key %q", i, got)
		}
	}
}

func TestPublishDropsWhenSubscriberBufferFull(t *testing.T) {
	b := New()
	ch, cancel := b.Subscribe()
	defer cancel()

	// Buffer is 64; publishing more must not block the publisher.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			b.Publish(Event{Kind: "memory", Key: "k"})
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a slow subscriber")
	}
	if n := len(ch); n != cap(ch) {
		t.Fatalf("buffered %d events, want a full buffer of %d", n, cap(ch))
	}
}

func TestCancelClosesChannelAndStopsDelivery(t *testing.T) {
	b := New()
	ch, cancel := b.Subscribe()
	cancel()

	if _, ok := <-ch; ok {
		t.Fatal("expected closed channel after cancel")
	}
	// Publishing after cancel must not panic on the closed channel.
	b.Publish(Event{Kind: "memory", Key: "after"})
}

func TestCancelIsIdempotent(t *testing.T) {
	b := New()
	_, cancel := b.Subscribe()
	cancel()
	cancel() // second call must not double-close
}

func TestCancelOnlyRemovesOwnSubscription(t *testing.T) {
	b := New()
	keep, cancelKeep := b.Subscribe()
	defer cancelKeep()
	_, cancelDrop := b.Subscribe()
	cancelDrop()

	b.Publish(Event{Kind: "claim", Key: "ns:key"})
	if got := recv(t, keep).Key; got != "ns:key" {
		t.Fatalf("surviving subscriber got %q", got)
	}
}

func TestConcurrentSubscribePublishCancel(t *testing.T) {
	b := New()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			ch, cancel := b.Subscribe()
			for range 5 {
				select {
				case <-ch:
				case <-time.After(10 * time.Millisecond):
				}
			}
			cancel()
		}()
		go func() {
			defer wg.Done()
			for range 50 {
				b.Publish(Event{Kind: "memory", Key: "race"})
			}
		}()
	}
	wg.Wait()
}
