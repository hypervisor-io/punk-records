// Package region is the brain-region registry: which agents (satellites)
// are registered to which namespace (Punk Records section), and the
// declarative membership loaded from agent specs. Registration is the
// wiring that lets a group of agents coordinate through a shared region.
package region

import (
	"context"
	"fmt"
	"time"

	"github.com/hypervisor-io/punk-records/internal/store"
)

type Store struct {
	db  *store.DB
	now func() time.Time
}

func New(db *store.DB, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{db: db, now: now}
}

// Member is one satellite registered to a region.
type Member struct {
	Namespace string `json:"namespace"`
	Agent     string `json:"agent"`
	Role      string `json:"role"`
	JoinedAt  string `json:"joined_at"`
}

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
	_, err := s.db.ExecContext(ctx, s.db.Rebind(`
		INSERT INTO region_members (namespace, agent, role, joined_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (namespace, agent) DO UPDATE SET role = excluded.role`),
		ns, agent, role, store.TimeToDB(s.now()))
	return err
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
		`SELECT namespace, agent, role, joined_at FROM region_members `+where), arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Member{}
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.Namespace, &m.Agent, &m.Role, &m.JoinedAt); err != nil {
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
