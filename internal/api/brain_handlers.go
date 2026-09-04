package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/region"
)

// brainNamespace is one region of the brain view: a namespace with the
// counts, people, and leases the page needs to draw and label it.
type brainNamespace struct {
	Name         string            `json:"name"`
	Facts        int               `json:"facts"`
	Observations int               `json:"observations"`
	Models       int               `json:"models"`
	Members      []region.Member   `json:"members"`
	Claims       []region.Claim    `json:"claims"`
	Tasks        memory.TaskCounts `json:"tasks"`
	Writes5m     int               `json:"writes_5m"`
	LastWriteAt  string            `json:"last_write_at"`
}

// brainSnapshot is the whole-server state the brain page loads once and
// then keeps current from the event stream.
type brainSnapshot struct {
	Version    string           `json:"version"`
	Now        string           `json:"now"`
	Namespaces []brainNamespace `json:"namespaces"`
}

// brainActivityWindow is the "recent writes" window reported per
// namespace; the page seeds its glow from it.
const brainActivityWindow = 5 * time.Minute

func (s *Server) buildBrainSnapshot(ctx context.Context) (brainSnapshot, error) {
	now := time.Now().UTC()
	snap := brainSnapshot{Version: s.version, Now: now.Format(time.RFC3339), Namespaces: []brainNamespace{}}
	names, err := s.mem.Namespaces(ctx)
	if err != nil {
		return snap, err
	}
	for _, name := range names {
		n := brainNamespace{Name: name, Members: []region.Member{}, Claims: []region.Claim{}}
		if p, err := s.mem.Profile(ctx, name); err == nil && p != nil {
			n.Facts, n.Observations, n.Models = p.Facts, p.Observations, p.Models
		}
		if writes, last, err := s.mem.WriteActivity(ctx, name, now.Add(-brainActivityWindow)); err == nil {
			n.Writes5m = writes
			if last.IsZero() {
				// The windowed call only sees rows newer than since; a
				// namespace idle longer than the window still has a last
				// write, so fetch it unwindowed.
				_, last, _ = s.mem.WriteActivity(ctx, name, time.Time{})
			}
			if !last.IsZero() {
				n.LastWriteAt = last.UTC().Format(time.RFC3339)
			}
		}
		if tc, err := s.mem.TaskCounts(ctx, name); err == nil {
			n.Tasks = tc
		}
		if s.region != nil {
			if m, err := s.region.Members(ctx, name); err == nil && m != nil {
				n.Members = m
			}
			if c, err := s.region.ListClaims(ctx, name); err == nil && c != nil {
				n.Claims = c
			}
		}
		snap.Namespaces = append(snap.Namespaces, n)
	}
	sort.SliceStable(snap.Namespaces, func(i, j int) bool {
		a, b := snap.Namespaces[i], snap.Namespaces[j]
		if a.LastWriteAt != b.LastWriteAt {
			return a.LastWriteAt > b.LastWriteAt // RFC3339 sorts lexically; "" (never) sinks
		}
		return a.Name < b.Name
	})
	return snap, nil
}

func (s *Server) handleBrainSnapshot(w http.ResponseWriter, r *http.Request) {
	snap, err := s.buildBrainSnapshot(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// brainEvent is the wire envelope for one bus event on /v1/brain/events.
// Namespace and Key come from the bus key "ns:/key"; ledger task ids have
// no namespace and pass through as Key.
type brainEvent struct {
	TS        string            `json:"ts"`
	Kind      string            `json:"kind"`
	Namespace string            `json:"namespace"`
	Key       string            `json:"key"`
	Data      map[string]string `json:"data"`
}

// splitBusKey splits "ns:/key" at the first colon. Memory keys start with
// "/" so a namespace never contains one; a key with no colon is a ledger
// task id and has no namespace.
func splitBusKey(key string) (string, string) {
	i := strings.IndexByte(key, ':')
	if i < 0 {
		return "", key
	}
	return key[:i], key[i+1:]
}

// brainKeepalive is how often an SSE comment is written on an idle stream
// so proxies and browsers do not close it.
const brainKeepalive = 15 * time.Second

func (s *Server) handleBrainEvents(w http.ResponseWriter, r *http.Request) {
	if s.bus == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "event bus not wired"})
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming unsupported"})
		return
	}
	events, cancel := s.bus.Subscribe()
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	write := func(name string, v any) bool {
		raw, err := json.Marshal(v)
		if err != nil {
			return true
		}
		if _, err := w.Write([]byte("event: " + name + "\ndata: " + string(raw) + "\n\n")); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	if !write("hello", map[string]string{"version": s.version, "now": time.Now().UTC().Format(time.RFC3339)}) {
		return
	}

	tick := time.NewTicker(brainKeepalive)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			fl.Flush()
		case e, open := <-events:
			if !open {
				return
			}
			ns, key := splitBusKey(e.Key)
			ev := brainEvent{TS: time.Now().UTC().Format(time.RFC3339), Kind: e.Kind, Namespace: ns, Key: key, Data: e.Data}
			if ev.Data == nil {
				ev.Data = map[string]string{}
			}
			if !write(e.Kind, ev) {
				return
			}
		}
	}
}
