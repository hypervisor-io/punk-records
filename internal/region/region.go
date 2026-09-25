// Package region is the brain-region registry: which agents (satellites)
// are registered to which namespace (Punk Records section), and the
// declarative membership loaded from agent specs. Registration is the
// wiring that lets a group of agents coordinate through a shared region.
package region

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/hypervisor-io/punk-records/internal/store"
)

type Store struct {
	db  *store.DB
	now func() time.Time

	// presence counts open inbox event streams per namespace and agent.
	// It is process-local runtime state, never stored: a stream that is
	// open right now is the strongest liveness signal a member can have.
	pmu      sync.Mutex
	presence map[string]map[string]int
	// Configure before serving requests. Nonpositive cap uses the default;
	// nonpositive retention disables deletion, never unread delivery.
	MaxUnreadPerRecipient int
	MessageRetention      time.Duration
}

func New(db *store.DB, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{db: db, now: now, MaxUnreadPerRecipient: 200, MessageRetention: 30 * 24 * time.Hour, presence: map[string]map[string]int{}}
}

// Member is one satellite registered to a region.
type Member struct {
	Namespace  string `json:"namespace"`
	Agent      string `json:"agent"`
	Role       string `json:"role"`
	JoinedAt   string `json:"joined_at"`
	LastSeenAt string `json:"last_seen_at,omitempty"`
}

// MemberStatus is a Member plus process-local liveness for discovery
// surfaces. It is a separate type so the runtime flag never widens the
// Member schema that other tools embed.
type MemberStatus struct {
	Member
	// Listening: an inbox event stream is open right now.
	Listening bool `json:"listening"`
}

// DB exposes the underlying handle for callers that must adjust rows
// outside the store's own API, such as tests that age a sighting.
func (s *Store) DB() *store.DB { return s.db }

// Ensure creates a region if absent.
func (s *Store) Ensure(ctx context.Context, ns, title string) error {
	_, err := s.db.ExecContext(ctx, s.db.Rebind(`
		INSERT INTO regions (namespace, title, created_at) VALUES ($1, $2, $3)
		ON CONFLICT (namespace) DO NOTHING`),
		ns, title, store.TimeToDB(s.now()))
	return err
}

// Register joins an agent to a region (idempotent; updates role).
func (s *Store) Register(ctx context.Context, ns, agent, role string) error {
	if ns == "" || agent == "" {
		return fmt.Errorf("region: namespace and agent required")
	}
	if err := s.Ensure(ctx, ns, ""); err != nil {
		return err
	}
	now := store.TimeToDB(s.now())
	_, err := s.db.ExecContext(ctx, s.db.Rebind(`
		INSERT INTO region_members (namespace, agent, role, joined_at, last_seen_at) VALUES ($1, $2, $3, $4, $4)
		ON CONFLICT (namespace, agent) DO UPDATE SET role = excluded.role, last_seen_at = excluded.last_seen_at`),
		ns, agent, role, now)
	return err
}

// Touch records that an agent is alive in a region. Unregistered agents
// are ignored: the heartbeat never creates membership by itself.
func (s *Store) Touch(ctx context.Context, ns, agent string) error {
	if ns == "" || agent == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, s.db.Rebind(
		`UPDATE region_members SET last_seen_at = $1 WHERE namespace = $2 AND agent = $3`),
		store.TimeToDB(s.now()), ns, agent)
	return err
}

// Attach records an open inbox event stream for an agent and returns the
// release to call when the stream ends. Streams are counted, so two
// concurrent streams for one address stay "listening" until both close.
func (s *Store) Attach(ns, agent string) func() {
	if ns == "" || agent == "" {
		return func() {}
	}
	s.pmu.Lock()
	byAgent := s.presence[ns]
	if byAgent == nil {
		byAgent = map[string]int{}
		s.presence[ns] = byAgent
	}
	byAgent[agent]++
	s.pmu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.pmu.Lock()
			defer s.pmu.Unlock()
			byAgent := s.presence[ns]
			if byAgent == nil {
				return
			}
			if byAgent[agent] <= 1 {
				delete(byAgent, agent)
			} else {
				byAgent[agent]--
			}
			if len(byAgent) == 0 {
				delete(s.presence, ns)
			}
		})
	}
}

// Listening reports whether the agent holds an open inbox event stream.
func (s *Store) Listening(ns, agent string) bool {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	return s.presence[ns][agent] > 0
}

// MemberStatuses lists a region's members with their liveness flag.
func (s *Store) MemberStatuses(ctx context.Context, ns string) ([]MemberStatus, error) {
	members, err := s.Members(ctx, ns)
	if err != nil {
		return nil, err
	}
	out := make([]MemberStatus, 0, len(members))
	for _, m := range members {
		out = append(out, MemberStatus{Member: m, Listening: s.Listening(m.Namespace, m.Agent)})
	}
	return out, nil
}

// Active reports whether a member is a live delivery target: it is
// listening now, or it was seen (registered, read, or heartbeat) within
// the window. Members whose last_seen_at cannot be parsed count as
// inactive rather than as alive.
func (s *Store) Active(m MemberStatus, within time.Duration) bool {
	if m.Listening {
		return true
	}
	if m.LastSeenAt == "" {
		return false
	}
	seen, err := store.TimeFromDB(m.LastSeenAt)
	if err != nil {
		return false
	}
	return !seen.Before(s.now().Add(-within))
}

// Deregister removes an agent from a region.
func (s *Store) Deregister(ctx context.Context, ns, agent string) error {
	_, err := s.db.ExecContext(ctx, s.db.Rebind(
		`DELETE FROM region_members WHERE namespace = $1 AND agent = $2`), ns, agent)
	return err
}

// Members lists a region's satellites.
func (s *Store) Members(ctx context.Context, ns string) ([]Member, error) {
	return s.query(ctx, `WHERE namespace = $1 ORDER BY agent`, ns)
}

// Regions lists the regions an agent is registered to.
func (s *Store) Regions(ctx context.Context, agent string) ([]Member, error) {
	return s.query(ctx, `WHERE agent = $1 ORDER BY namespace`, agent)
}

func (s *Store) query(ctx context.Context, where string, arg string) ([]Member, error) {
	rows, err := s.db.QueryContext(ctx, s.db.Rebind(
		`SELECT namespace, agent, role, joined_at, COALESCE(last_seen_at, '') FROM region_members `+where), arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Member{}
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.Namespace, &m.Agent, &m.Role, &m.JoinedAt, &m.LastSeenAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AllNamespaces lists every region that exists (for consolidation).
func (s *Store) AllNamespaces(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT namespace FROM regions ORDER BY namespace`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// SyncFromSpecs reconciles declarative membership: every agent spec's
// regions[] becomes a registration; specs are the source of truth for
// declared membership (dynamic MCP registration coexists with role="").
func (s *Store) SyncFromSpecs(ctx context.Context, specRegions map[string][]string) error {
	for agent, nss := range specRegions {
		for _, ns := range nss {
			if err := s.Register(ctx, ns, agent, "declared"); err != nil {
				return err
			}
		}
	}
	return nil
}
