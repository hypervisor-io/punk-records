package api

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/hypervisor-io/punk-records/internal/authz"
	"github.com/hypervisor-io/punk-records/internal/cost"
	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/region"
)

// overviewTTL is how long the cross-namespace read models (namespace
// summaries, the overview) count as fresh. Both scan every namespace's
// facts, which takes seconds on a large store, while the console polls
// them every few seconds; past the TTL the last value is still served
// while one background scan replaces it (see ttlCache).
const overviewTTL = 20 * time.Second

// overviewRecentWindow is the last_seen_at window that makes a member
// "recent" in the overview.
const overviewRecentWindow = 10 * time.Minute

// ttlCache holds one computed value and serves it stale-while-revalidate:
// a value younger than ttl is returned as is; an older one is returned
// at once while a single background computation refreshes it, so after
// the first load no caller waits on the multi-second scan again. Only a
// cache with no value yet makes callers wait, and concurrent misses
// share one computation (single-flight). Errors are never cached: a
// failed refresh keeps serving the last good value and the next caller
// retries. The computation runs detached from the caller's context, so
// one client hanging up does not fail the others waiting on the same
// result.
type ttlCache[T any] struct {
	ttl time.Duration
	now func() time.Time

	mu     sync.Mutex
	val    T
	at     time.Time
	ok     bool
	flight *cacheFlight[T]
}

type cacheFlight[T any] struct {
	done chan struct{}
	val  T
	err  error
}

func newTTLCache[T any](ttl time.Duration, now func() time.Time) *ttlCache[T] {
	return &ttlCache[T]{ttl: ttl, now: now}
}

// get returns the cached value, refreshing it in the background when it
// is older than the TTL, or runs load once for all concurrent callers
// when there is no value yet.
func (c *ttlCache[T]) get(ctx context.Context, load func(context.Context) (T, error)) (T, error) {
	c.mu.Lock()
	if c.ok {
		v := c.val
		if c.now().Sub(c.at) >= c.ttl {
			c.startLocked(ctx, load)
		}
		c.mu.Unlock()
		return v, nil
	}
	f := c.startLocked(ctx, load)
	c.mu.Unlock()
	select {
	case <-f.done:
		return f.val, f.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

// put stores a value computed elsewhere as fresh.
func (c *ttlCache[T]) put(v T) {
	c.mu.Lock()
	c.val, c.at, c.ok = v, c.now(), true
	c.mu.Unlock()
}

// warm starts a computation if none is running and returns without
// waiting, so a server can fill its caches before the first request.
func (c *ttlCache[T]) warm(ctx context.Context, load func(context.Context) (T, error)) {
	c.mu.Lock()
	c.startLocked(ctx, load)
	c.mu.Unlock()
}

// settle waits for the in-flight computation, if any; tests use it to
// observe a background refresh.
func (c *ttlCache[T]) settle() {
	c.mu.Lock()
	f := c.flight
	c.mu.Unlock()
	if f != nil {
		<-f.done
	}
}

// startLocked joins the running computation or starts one. c.mu is held.
func (c *ttlCache[T]) startLocked(ctx context.Context, load func(context.Context) (T, error)) *cacheFlight[T] {
	if c.flight != nil {
		return c.flight
	}
	f := &cacheFlight[T]{done: make(chan struct{})}
	c.flight = f
	go func() {
		lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		f.val, f.err = load(lctx)
		c.mu.Lock()
		if f.err == nil {
			c.val, c.at, c.ok = f.val, c.now(), true
		}
		c.flight = nil
		c.mu.Unlock()
		close(f.done)
	}()
	return f
}

// WarmCaches starts computing the cross-namespace read models (namespace
// summaries, then the overview built on them) in the background, so the
// console's first load after a restart does not wait on a full scan of
// the memory table. It returns at once; serve calls it after New.
func (s *Server) WarmCaches(ctx context.Context) {
	if s.mem == nil {
		return
	}
	s.overview.warm(ctx, s.computeOverview)
}

// namespaceSummaries is memory.NamespaceSummaries behind the shared
// 20s cache. The slice is shared between callers: treat it as read-only.
func (s *Server) namespaceSummaries(ctx context.Context) ([]memory.NamespaceSummary, error) {
	return s.nsSums.get(ctx, s.mem.NamespaceSummaries)
}

// overviewRuntime is the "runtime" block: how much of the domain-agent
// runtime this server actually runs. Dependencies that are not wired
// report zero or false.
type overviewRuntime struct {
	AIEnabled         bool   `json:"ai_enabled"`
	MessagingEnabled  bool   `json:"messaging_enabled"`
	Authz             string `json:"authz"`
	LedgerTasks       int    `json:"ledger_tasks"`
	PendingProposals  int    `json:"pending_proposals"`
	ParkedTasks       int    `json:"parked_tasks"`
	MonthCostMicroUSD int64  `json:"month_cost_microusd"`
	DomainAgents      int    `json:"domain_agents"`
}

type overviewTotals struct {
	Namespaces    int `json:"namespaces"`
	Facts         int `json:"facts"`
	Tasks         int `json:"tasks"`
	TasksOpen     int `json:"tasks_open"`
	Members       int `json:"members"`
	MembersLive   int `json:"members_live"`
	MembersRecent int `json:"members_recent"`
	Unread        int `json:"unread"`
	Messages24h   int `json:"messages_24h"`
	Claims        int `json:"claims"`
}

type overviewNamespace struct {
	Name          string                 `json:"name"`
	Facts         int                    `json:"facts"`
	Tasks         int                    `json:"tasks"`
	TaskCounts    memory.TaskStateCounts `json:"task_counts"`
	Members       int                    `json:"members"`
	MembersLive   int                    `json:"members_live"`
	MembersRecent int                    `json:"members_recent"`
	Unread        int                    `json:"unread"`
	Messages24h   int                    `json:"messages_24h"`
	Claims        int                    `json:"claims"`
	LastWrite     string                 `json:"last_write,omitempty"`
	LastMessageAt string                 `json:"last_message_at,omitempty"`
	LastSeenAt    string                 `json:"last_seen_at,omitempty"`
	LastActivity  string                 `json:"last_activity,omitempty"`
	// ConsolidatedAt is when /punk-consolidate last archived this
	// namespace; Archive marks a namespace that is such an archive.
	ConsolidatedAt string `json:"consolidated_at,omitempty"`
	Archive        bool   `json:"archive,omitempty"`

	lastActivity time.Time // sort key; the string above is its rendering
}

type overviewOut struct {
	Now        string              `json:"now"`
	Version    string              `json:"version"`
	Runtime    overviewRuntime     `json:"runtime"`
	Totals     overviewTotals      `json:"totals"`
	Namespaces []overviewNamespace `json:"namespaces"`
}

// overviewData is the cached, unfiltered computation behind /v1/overview.
type overviewData struct {
	now        time.Time
	runtime    overviewRuntime
	namespaces []overviewNamespace // sorted by last_activity desc, then name
}

func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func laterOf(ts ...time.Time) time.Time {
	var m time.Time
	for _, t := range ts {
		if t.After(m) {
			m = t
		}
	}
	return m
}

// computeOverview builds the unfiltered overview: a fixed handful of
// grouped queries (memory summaries, task states, members, messages,
// claims), never one per namespace.
func (s *Server) computeOverview(ctx context.Context) (*overviewData, error) {
	// Read the summaries directly, not through the stale-while-revalidate
	// cache (a refresh built on a stale summary would lag a full extra
	// cycle), and hand the fresh result to /v1/namespaces as well.
	sums, err := s.mem.NamespaceSummaries(ctx)
	if err != nil {
		return nil, err
	}
	s.nsSums.put(sums)
	states, err := s.mem.NamespaceTaskStates(ctx)
	if err != nil {
		return nil, err
	}
	marks, err := s.mem.NamespaceMarks(ctx)
	if err != nil {
		return nil, err
	}
	var act map[string]*region.NamespaceActivity
	if s.region != nil {
		if act, err = s.region.Activity(ctx, overviewRecentWindow); err != nil {
			return nil, err
		}
	}
	d := &overviewData{now: s.clock().UTC(), namespaces: make([]overviewNamespace, 0, len(sums))}
	seen := make(map[string]bool, len(sums))
	for _, sum := range sums {
		seen[sum.Name] = true
		d.namespaces = append(d.namespaces, overviewNamespace{
			Name: sum.Name, Facts: sum.Facts, Tasks: sum.Tasks, LastWrite: rfc3339(sum.LastWrite),
			TaskCounts: states[sum.Name], lastActivity: sum.LastWrite,
		})
	}
	// A region can exist with no memory namespace yet (members registered,
	// nothing written): list it too.
	for name := range act {
		if !seen[name] {
			d.namespaces = append(d.namespaces, overviewNamespace{Name: name})
		}
	}
	for i := range d.namespaces {
		n := &d.namespaces[i]
		// Tasks counted by summary and by status resolution agree unless
		// a task was written between the two reads; trust the summary.
		if n.TaskCounts.Total() == 0 && n.Tasks > 0 {
			n.TaskCounts.Pending = n.Tasks
		}
		if a := act[n.Name]; a != nil {
			n.Members, n.MembersLive, n.MembersRecent = a.Members, a.Live, a.Recent
			n.Unread, n.Messages24h, n.Claims = a.Unread, a.Messages24h, a.Claims
			n.LastMessageAt, n.LastSeenAt = rfc3339(a.LastMessageAt), rfc3339(a.LastSeenAt)
			n.lastActivity = laterOf(n.lastActivity, a.LastMessageAt, a.LastSeenAt)
		}
		n.LastActivity = rfc3339(n.lastActivity)
		if m, ok := marks[n.Name]; ok {
			n.ConsolidatedAt, n.Archive = rfc3339(m.ConsolidatedAt), m.Archive
		}
	}
	sort.SliceStable(d.namespaces, func(i, j int) bool {
		a, b := d.namespaces[i], d.namespaces[j]
		if !a.lastActivity.Equal(b.lastActivity) {
			return a.lastActivity.After(b.lastActivity)
		}
		return a.Name < b.Name
	})
	d.runtime = s.overviewRuntime(ctx)
	return d, nil
}

// overviewRuntime reads the domain-agent runtime's footprint. Each probe
// is best-effort: an unwired dependency or a failed query reports zero
// and never fails the overview, which is about coordination data.
func (s *Server) overviewRuntime(ctx context.Context) overviewRuntime {
	rt := overviewRuntime{AIEnabled: s.aiEnabled, MessagingEnabled: s.messagingEnabled, Authz: s.authzMode}
	if rt.Authz == "" {
		rt.Authz = "off"
	}
	count := func(query string, dst *int) {
		if s.db == nil {
			return
		}
		if err := s.db.QueryRowContext(ctx, query).Scan(dst); err != nil {
			s.log.Warn("overview runtime probe failed", "err", err)
		}
	}
	if s.ledger != nil {
		count(`SELECT COUNT(*) FROM tasks`, &rt.LedgerTasks)
		count(`SELECT COUNT(*) FROM tasks WHERE status = 'input_required'`, &rt.ParkedTasks)
	}
	if s.props != nil {
		count(`SELECT COUNT(*) FROM proposals WHERE status = 'proposed'`, &rt.PendingProposals)
	}
	if s.db != nil {
		if o, err := cost.Summarize(ctx, s.db, s.clock()); err == nil {
			rt.MonthCostMicroUSD = o.MonthTotalMicroUSD
		} else {
			s.log.Warn("overview cost probe failed", "err", err)
		}
	}
	if s.reg != nil {
		if snap := s.reg.Current(); snap != nil && snap.Bundle != nil {
			rt.DomainAgents = len(snap.Bundle.Agents)
		}
	}
	return rt
}

// handleOverview is GET /v1/overview: one cross-namespace read model for
// the operator console - totals, per-namespace task/member/message
// activity, and the runtime footprint. The computation is cached and
// global; like /v1/namespaces it spans namespaces, so the A01 path hook
// cannot gate it and each row is filtered here against the verified
// subject's read grant, with totals summed from the rows that survive.
func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	d, err := s.overview.get(r.Context(), s.computeOverview)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("overview: %w", err))
		return
	}
	subject := verifiedSubject(r)
	out := overviewOut{
		Now: rfc3339(d.now), Version: s.version, Runtime: d.runtime,
		Namespaces: make([]overviewNamespace, 0, len(d.namespaces)),
	}
	t := &out.Totals
	for _, n := range d.namespaces {
		if !s.allowNS(r.Context(), subject, n.Name, authz.OpRead) {
			continue
		}
		out.Namespaces = append(out.Namespaces, n)
		t.Namespaces++
		t.Facts += n.Facts
		t.Tasks += n.Tasks
		t.TasksOpen += n.TaskCounts.Total() - n.TaskCounts.Done
		t.Members += n.Members
		t.MembersLive += n.MembersLive
		t.MembersRecent += n.MembersRecent
		t.Unread += n.Unread
		t.Messages24h += n.Messages24h
		t.Claims += n.Claims
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRecent is GET /v1/namespaces/{ns}/recent: the newest live fact
// per key, most recent first, for the console's activity feed. ?limit
// defaults to 50 (max 200), ?offset pages further back; ?sessions=1
// includes /agent-sessions/ keys.
// Authorization is the A01 path hook, as for /memories.
func (s *Server) handleRecent(w http.ResponseWriter, r *http.Request) {
	ns := chi.URLParam(r, "ns")
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	facts, err := s.mem.RecentFacts(r.Context(), ns, queryLimit(r), offset, r.URL.Query().Get("sessions") == "1")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"namespace": ns, "facts": facts})
}
