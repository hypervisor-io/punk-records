package region

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/hypervisor-io/punk-records/internal/store"
)

// ErrNotMember is returned by SetInboxBinding when the agent is not a
// registered member of the namespace at bind time. Bindings are only
// ever created by an explicit register (see the MCP register tool's
// inbox flag), so membership is the gate that keeps a binding honest:
// an address can only bind its inbox to a region it actually joined.
var ErrNotMember = errors.New("region: agent is not a member of namespace")

// SetInboxBinding binds an agent address's inbox to ns: after this,
// namespace-resolution reads (GET /v1/agent/namespace with agent=) for
// that address answer ns instead of the cwd-derived namespace, until a
// later explicit bind replaces it. The agent must be a registered
// member of ns at bind time (ErrNotMember otherwise); binding happens
// after a successful register, never instead of one. The row is
// upserted by agent - one binding per address, latest explicit
// registration wins - and is independent of region_members: member
// expiry or deregistration never deletes it (see migration 0027).
func (s *Store) SetInboxBinding(ctx context.Context, ns, agent string) error {
	if ns == "" || agent == "" {
		return fmt.Errorf("region: namespace and agent required")
	}
	var one int
	err := s.db.QueryRowContext(ctx, s.db.Rebind(
		`SELECT 1 FROM region_members WHERE namespace = $1 AND agent = $2`), ns, agent).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("region: bind %s to %s: %w", agent, ns, ErrNotMember)
	}
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, s.db.Rebind(`
		INSERT INTO agent_inbox_bindings (agent, namespace, set_at) VALUES ($1, $2, $3)
		ON CONFLICT (agent) DO UPDATE SET namespace = excluded.namespace, set_at = excluded.set_at`),
		agent, ns, store.TimeToDB(s.now()))
	return err
}

// InboxBinding reports the namespace agent's inbox is bound to. It is
// strictly read-only: a pure lookup with no membership side effects, so
// resolution paths can call it fresh on every hook event. ok is false
// for an unbound address (the caller then falls back to its local
// resolution order, e.g. the cwd-derived namespace).
func (s *Store) InboxBinding(ctx context.Context, agent string) (ns string, ok bool, err error) {
	if agent == "" {
		return "", false, nil
	}
	err = s.db.QueryRowContext(ctx, s.db.Rebind(
		`SELECT namespace FROM agent_inbox_bindings WHERE agent = $1`), agent).Scan(&ns)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return ns, true, nil
}
