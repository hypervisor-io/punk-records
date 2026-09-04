package api

import (
	"context"
	"net/http"
	"sort"
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
