// Package bus is the in-process event fan-out: ledger status changes and
// memory writes flow to MCP resource subscriptions and (P12.3) the
// durable outbox tailer swaps in behind the same shape. Slow consumers
// drop events rather than block writers; durable delivery is the
// outbox's job, not this bus's.
package bus

import "sync"

// Event is one change notification.
type Event struct {
	Kind string // task_status | memory
	Key  string // task id, or namespace-prefixed memory key
	Data map[string]string
}

type Bus struct {
	mu   sync.RWMutex
	subs map[chan Event]struct{}
}

func New() *Bus { return &Bus{subs: map[chan Event]struct{}{}} }

// Subscribe returns a buffered channel of events plus a cancel func.
func (b *Bus) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 64)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		if _, ok := b.subs[ch]; ok {
			delete(b.subs, ch)
			close(ch)
		}
		b.mu.Unlock()
	}
}

// Publish fans out without blocking: full subscriber buffers drop.
func (b *Bus) Publish(e Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.subs {
		select {
		case ch <- e:
		default:
		}
	}
}
