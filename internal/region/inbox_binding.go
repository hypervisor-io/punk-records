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
//
// This is the setter-less form: the binding records an honest empty
// set_by = unknown (P3-D visibility). Call sites that can name who
// performed the bind use SetInboxBindingAs instead; migrating the
// server call sites (the MCP register tool, and the unmerged join tool
// on coord/P4-A) onto it is a follow-up, and this signature stays so
// they keep compiling unchanged.
func (s *Store) SetInboxBinding(ctx context.Context, ns, agent string) error {
	return s.SetInboxBindingAs(ctx, ns, agent, "")
}

// SetInboxBindingAs is SetInboxBinding with the setter named for
// binding visibility (P3-D, migration 0028): set_by stores who
// performed the bind - typically the agent address or API-key subject
// the connection pins - and set_at when it happened. Same membership
// gate, same latest-bind-wins upsert; a rebind replaces namespace,
// setter and time together. An empty setBy stores the unknown default
// rather than a fabricated name.
func (s *Store) SetInboxBindingAs(ctx context.Context, ns, agent, setBy string) error {
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
		INSERT INTO agent_inbox_bindings (agent, namespace, set_by, set_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (agent) DO UPDATE SET namespace = excluded.namespace, set_by = excluded.set_by, set_at = excluded.set_at`),
		agent, ns, setBy, store.TimeToDB(s.now()))
	return err
}

// InboxBindingInfo is the full stored binding (P3-D visibility): where
// the inbox is bound, who set it and when. SetBy is empty when the
// setter is unknown - a binding written before migration 0028, or by a
// setter-less call site.
type InboxBindingInfo struct {
	Namespace string
	SetBy     string
	SetAt     string
}

// InboxBinding reports the namespace agent's inbox is bound to. It is
// strictly read-only: a pure lookup with no membership side effects, so
// resolution paths can call it fresh on every hook event. ok is false
// for an unbound address (the caller then falls back to its local
// resolution order, e.g. the cwd-derived namespace).
func (s *Store) InboxBinding(ctx context.Context, agent string) (ns string, ok bool, err error) {
	info, ok, err := s.InboxBindingInfo(ctx, agent)
	return info.Namespace, ok, err
}

// InboxBindingInfo reports the full binding row - namespace, setter and
// time - under exactly InboxBinding's read-only contract: a pure
// lookup, ok false for an unbound address.
func (s *Store) InboxBindingInfo(ctx context.Context, agent string) (InboxBindingInfo, bool, error) {
	if agent == "" {
		return InboxBindingInfo{}, false, nil
	}
	var info InboxBindingInfo
	err := s.db.QueryRowContext(ctx, s.db.Rebind(
		`SELECT namespace, set_by, set_at FROM agent_inbox_bindings WHERE agent = $1`), agent).
		Scan(&info.Namespace, &info.SetBy, &info.SetAt)
	if errors.Is(err, sql.ErrNoRows) {
		return InboxBindingInfo{}, false, nil
	}
	if err != nil {
		return InboxBindingInfo{}, false, err
	}
	return info, true, nil
}
