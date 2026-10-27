package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hypervisor-io/punk-records/internal/store"
)

// Namespace consolidation (/punk-consolidate). An agent reads a namespace
// through the digest pages below, writes one structured summary, and the
// apply step moves everything else into a sibling archive namespace in
// one transaction: the live namespace keeps the summary, its open tasks
// and anything under the caller's keep prefixes. Moving rows (rather than
// copying or tombstoning them) keeps every revision, author and
// embedding intact in the archive, where recall, search and as-of reads
// work as before, and makes RestoreArchive a move back.
//
// This is distinct from Consolidate (the nightly compaction and optional
// server-side /consolidated/region rollup): that pass never moves facts
// between namespaces.

// Keys the consolidation flow owns.
const (
	// ConsolidatedSummaryKey holds the agent-written summary in the live
	// namespace.
	ConsolidatedSummaryKey = "/consolidated/summary"
	// ConsolidatedStatusKey records a finished apply: when, by whom and
	// into which archive. Its presence is what marks a namespace
	// consolidated (the summary alone may be a draft of a failed apply).
	ConsolidatedStatusKey = "/consolidated/status"
	// ArchiveSourceKey marks an archive namespace and names its source.
	ArchiveSourceKey = "/archive/source"
)

// consolidatedPrefix is always kept live: the summary and status, and the
// nightly /consolidated/region rollup.
const consolidatedPrefix = "/consolidated/"

// moveChunk bounds the IN list of one move-back statement.
const moveChunk = 400

// ConsolidationPlan is what an apply would do, computed from the live
// namespace before anything moves.
type ConsolidationPlan struct {
	Namespace string `json:"namespace"`
	// Archive is the namespace the moved facts land in.
	Archive string `json:"archive"`
	// KeepKeys are the keys that stay live (every revision of each).
	KeepKeys []string `json:"keep_keys"`
	// MoveKeys counts the keys (live or not) whose revisions move.
	MoveKeys int `json:"move_keys"`
	// OpenTasks are the ids of tasks not yet done; they stay live.
	OpenTasks []string `json:"open_tasks"`
}

// ArchiveName returns the archive namespace for ns on day now:
// <ns>-archive-YYYYMMDD, suffixed -2, -3 ... when that name already holds
// facts. Dashes, not dots: hook namespace overrides only accept
// lowercase letters, digits and dashes.
func (s *Store) ArchiveName(ctx context.Context, ns string, now time.Time) (string, error) {
	base := ns + "-archive-" + now.UTC().Format("20060102")
	for i := 1; i <= 99; i++ {
		name := base
		if i > 1 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		used, err := s.namespaceHasRows(ctx, name)
		if err != nil {
			return "", err
		}
		if !used {
			return name, nil
		}
	}
	return "", fmt.Errorf("consolidate: no free archive name for %s on %s", ns, now.UTC().Format("2006-01-02"))
}

func (s *Store) namespaceHasRows(ctx context.Context, name string) (bool, error) {
	id, ok, err := s.namespaceID(ctx, name)
	if err != nil || !ok {
		return false, err
	}
	var one int
	err = s.db.QueryRowContext(ctx, s.db.Rebind(`SELECT 1 FROM memories WHERE namespace_id = $1 LIMIT 1`), id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// PlanConsolidation decides which keys stay live in ns: everything under
// /consolidated/, every key under one of keepPrefixes, and each open
// task's /tasks/<id> fact with its children (status, notes). Every other
// key - live, superseded or tombstoned - moves to the archive.
func (s *Store) PlanConsolidation(ctx context.Context, ns string, keepPrefixes []string, now time.Time) (*ConsolidationPlan, error) {
	nsID, ok, err := s.namespaceID(ctx, ns)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("consolidate: namespace %q does not exist", ns)
	}
	for _, p := range keepPrefixes {
		if !strings.HasPrefix(p, "/") {
			return nil, fmt.Errorf("consolidate: keep prefix %q must start with /", p)
		}
	}
	tasks, err := s.ListTasks(ctx, ns)
	if err != nil {
		return nil, err
	}
	plan := &ConsolidationPlan{Namespace: ns, KeepKeys: []string{}, OpenTasks: []string{}}
	open := map[string]bool{}
	for _, t := range tasks {
		if t.State != "done" {
			open[t.ID] = true
			plan.OpenTasks = append(plan.OpenTasks, t.ID)
		}
	}
	rows, err := s.db.QueryContext(ctx, s.db.Rebind(`SELECT DISTINCT key FROM memories WHERE namespace_id = $1 ORDER BY key`), nsID)
	if err != nil {
		return nil, fmt.Errorf("consolidate: list keys: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		if keepKey(k, keepPrefixes, open) {
			plan.KeepKeys = append(plan.KeepKeys, k)
		} else {
			plan.MoveKeys++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if plan.Archive, err = s.ArchiveName(ctx, ns, now); err != nil {
		return nil, err
	}
	return plan, nil
}

func keepKey(k string, keepPrefixes []string, open map[string]bool) bool {
	if strings.HasPrefix(k, consolidatedPrefix) {
		return true
	}
	for _, p := range keepPrefixes {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	if rest, ok := strings.CutPrefix(k, "/tasks/"); ok {
		id, _, _ := strings.Cut(rest, "/")
		return id != "" && open[id]
	}
	return false
}

// ArchiveNamespace moves every revision of every key in ns except keep
// into archive, together with the memory links that start at a moved key,
// in one transaction. archive must not hold facts yet. It returns the
// number of fact revisions moved.
func (s *Store) ArchiveNamespace(ctx context.Context, ns, archive string, keep []string) (int64, error) {
	if archive == ns {
		return 0, errors.New("consolidate: archive must differ from the namespace")
	}
	nsID, ok, err := s.namespaceID(ctx, ns)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("consolidate: namespace %q does not exist", ns)
	}
	var moved int64
	err = s.db.WithTx(ctx, func(tx *sql.Tx) error {
		archID, err := s.ensureNamespace(ctx, tx, archive)
		if err != nil {
			return err
		}
		var one int
		switch err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT 1 FROM memories WHERE namespace_id = $1 LIMIT 1`), archID).Scan(&one); {
		case err == nil:
			return fmt.Errorf("consolidate: archive %q already holds facts", archive)
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		// Move everything, then move the kept keys back: one statement
		// for the bulk plus one per chunk of a (short) keep list.
		r, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE memories SET namespace_id = $1 WHERE namespace_id = $2`), archID, nsID)
		if err != nil {
			return fmt.Errorf("consolidate: move facts: %w", err)
		}
		all, _ := r.RowsAffected()
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE memory_links SET namespace = $1 WHERE namespace = $2`), archive, ns); err != nil {
			return fmt.Errorf("consolidate: move links: %w", err)
		}
		var back int64
		for start := 0; start < len(keep); start += moveChunk {
			chunk := keep[start:min(start+moveChunk, len(keep))]
			ph, args := inList(chunk, 3)
			r, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE memories SET namespace_id = $1 WHERE namespace_id = $2 AND key IN (`+ph+`)`),
				append([]any{nsID, archID}, args...)...)
			if err != nil {
				return fmt.Errorf("consolidate: keep facts: %w", err)
			}
			n, _ := r.RowsAffected()
			back += n
			if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE memory_links SET namespace = $1 WHERE namespace = $2 AND from_key IN (`+ph+`)`),
				append([]any{ns, archive}, args...)...); err != nil {
				return fmt.Errorf("consolidate: keep links: %w", err)
			}
		}
		moved = all - back
		return nil
	})
	if err != nil {
		return 0, err
	}
	s.dropIVF(ns)
	return moved, nil
}

// RestoreArchive moves every fact revision and link of archive back into
// the namespace its ArchiveSourceKey names, except that marker itself,
// which is tombstoned so the archive reads as restored, as is the
// namespace's ConsolidatedStatusKey when it names this archive. Revisions merge by
// time: a key written again after the consolidation keeps its newest
// revision as the live one. It returns the source namespace and the
// number of revisions moved.
func (s *Store) RestoreArchive(ctx context.Context, archive, by string) (string, int64, error) {
	src, err := s.Recall(ctx, archive, ArchiveSourceKey, 1)
	if err != nil {
		return "", 0, err
	}
	if len(src) == 0 {
		return "", 0, fmt.Errorf("restore: %q is not an archive (no %s fact)", archive, ArchiveSourceKey)
	}
	ns, _ := src[0].Attributes["source"].(string)
	if ns == "" {
		return "", 0, fmt.Errorf("restore: %s in %q names no source namespace", ArchiveSourceKey, archive)
	}
	archID, _, err := s.namespaceID(ctx, archive)
	if err != nil {
		return "", 0, err
	}
	var moved int64
	err = s.db.WithTx(ctx, func(tx *sql.Tx) error {
		nsID, err := s.ensureNamespace(ctx, tx, ns)
		if err != nil {
			return err
		}
		r, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE memories SET namespace_id = $1 WHERE namespace_id = $2 AND key <> $3`), nsID, archID, ArchiveSourceKey)
		if err != nil {
			return fmt.Errorf("restore: move facts: %w", err)
		}
		moved, _ = r.RowsAffected()
		// A link the namespace already has (same endpoints and type)
		// stays where it is; the archive copy is dropped with the rest.
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`
			UPDATE memory_links SET namespace = $1 WHERE namespace = $2
			  AND NOT EXISTS (SELECT 1 FROM memory_links l2 WHERE l2.namespace = $1
			      AND l2.from_key = memory_links.from_key AND l2.to_key = memory_links.to_key
			      AND l2.link_type = memory_links.link_type)`), ns, archive); err != nil {
			return fmt.Errorf("restore: move links: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", 0, err
	}
	s.dropIVF(archive)
	s.dropIVF(ns)
	if err := s.Forget(ctx, archive, ArchiveSourceKey, by); err != nil {
		return ns, moved, fmt.Errorf("restore: facts moved back, but clearing %s failed: %w", ArchiveSourceKey, err)
	}
	// The namespace no longer reads as consolidated when the status it
	// carries is this archive's; a later consolidation's status stays.
	if st, err := s.Recall(ctx, ns, ConsolidatedStatusKey, 1); err == nil && len(st) == 1 {
		if a, _ := st[0].Attributes["archive"].(string); a == archive {
			if err := s.Forget(ctx, ns, ConsolidatedStatusKey, by); err != nil {
				return ns, moved, fmt.Errorf("restore: facts moved back, but clearing %s failed: %w", ConsolidatedStatusKey, err)
			}
		}
	}
	return ns, moved, nil
}

// inList renders n placeholders $first.. for an IN list and their args.
func inList(vals []string, first int) (string, []any) {
	ph := make([]string, len(vals))
	args := make([]any, len(vals))
	for i, v := range vals {
		ph[i] = fmt.Sprintf("$%d", first+i)
		args[i] = v
	}
	return strings.Join(ph, ", "), args
}

// dropIVF forgets a namespace's in-process vector index after its rows
// moved; VectorSearch already ignores stale postings, this frees them.
func (s *Store) dropIVF(ns string) {
	s.ivfMu.Lock()
	delete(s.ivf, ns)
	s.ivfMu.Unlock()
}

// PrefixCount is one first-level key prefix and how many live keys it
// holds.
type PrefixCount struct {
	Prefix string `json:"prefix"`
	Keys   int    `json:"keys"`
}

// LivePrefixCounts groups the live keys of ns by their first path
// segment ("/tasks/", "/decisions/", ...), largest first: the map an
// agent pages the digest by.
func (s *Store) LivePrefixCounts(ctx context.Context, ns string) ([]PrefixCount, int, error) {
	keys, err := s.ListKeys(ctx, ns, "/")
	if err != nil {
		return nil, 0, err
	}
	counts := map[string]int{}
	for _, k := range keys {
		seg, _, nested := strings.Cut(strings.TrimPrefix(k, "/"), "/")
		p := "/" + seg
		if nested {
			p += "/"
		}
		counts[p]++
	}
	out := make([]PrefixCount, 0, len(counts))
	for p, n := range counts {
		out = append(out, PrefixCount{Prefix: p, Keys: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Keys != out[j].Keys {
			return out[i].Keys > out[j].Keys
		}
		return out[i].Prefix < out[j].Prefix
	})
	return out, len(keys), nil
}

// LiveFactsPage returns up to limit live facts under prefix with keys
// after cursor, in key order, skipping /agent-sessions/ unless
// includeSessions. next is the cursor for the following page, "" at the
// end.
func (s *Store) LiveFactsPage(ctx context.Context, ns, prefix, cursor string, limit int, includeSessions bool) ([]Fact, string, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	nsID, ok, err := s.namespaceID(ctx, ns)
	if err != nil {
		return nil, "", err
	}
	if !ok {
		return []Fact{}, "", nil
	}
	sessionsEnd, _ := prefixUpper(sessionKeyPrefix)
	out := []Fact{}
	batch := limit + 1 // one extra row tells whether a next page exists
query:
	for {
		rows, err := s.db.QueryContext(ctx, s.db.Rebind(latestLivePageSQL),
			nsID, likePrefix(prefix), store.TimeToDB(s.now()), cursor, batch)
		if err != nil {
			return nil, "", fmt.Errorf("facts page: %w", err)
		}
		facts, err := s.scanFacts(ctx, ns, nsID, rows)
		rows.Close()
		if err != nil {
			return nil, "", err
		}
		for _, f := range facts {
			if !includeSessions && strings.HasPrefix(f.Key, sessionKeyPrefix) {
				cursor = sessionsEnd // skip the whole session block in one hop
				continue query
			}
			if len(out) == limit {
				return out, out[limit-1].Key, nil
			}
			out = append(out, f)
			cursor = f.Key
		}
		if len(facts) < batch {
			return out, "", nil
		}
	}
}

// ConsolidationStatus is the recorded outcome of an apply.
type ConsolidationStatus struct {
	At               time.Time `json:"at"`
	By               string    `json:"by"`
	Archive          string    `json:"archive"`
	MovedFacts       int64     `json:"moved_facts"`
	MovedMessages    int64     `json:"moved_messages"`
	KeptKeys         int       `json:"kept_keys"`
	OpenTasks        int       `json:"open_tasks"`
	MessagesArchived bool      `json:"messages_archived"`
}

// RecordConsolidation writes the status fact that marks ns consolidated
// and the marker that makes archive restorable.
func (s *Store) RecordConsolidation(ctx context.Context, ns string, st ConsolidationStatus) error {
	at := st.At.UTC().Format(time.RFC3339)
	if _, err := s.Write(ctx, WriteInput{
		Namespace: st.Archive, Key: ArchiveSourceKey,
		Body: fmt.Sprintf("Archive of namespace %s, consolidated %s by %s. The summary lives at %s in %s; restore with: punk archive-restore -archive %s",
			ns, at, st.By, ConsolidatedSummaryKey, ns, st.Archive),
		Attributes: map[string]any{"source": ns, "archived_at": at},
		Author:     st.By, Writer: st.By, Importance: 0.5,
	}); err != nil {
		return fmt.Errorf("consolidate: archive marker: %w", err)
	}
	_, err := s.Write(ctx, WriteInput{
		Namespace: ns, Key: ConsolidatedStatusKey,
		Body: fmt.Sprintf("consolidated %s by %s: %d fact revisions and %d read messages archived to %s; %d keys kept live (%d open tasks).",
			at, st.By, st.MovedFacts, st.MovedMessages, st.Archive, st.KeptKeys, st.OpenTasks),
		Attributes: map[string]any{
			"consolidated_at": at, "by": st.By, "archive": st.Archive,
			"moved_facts": st.MovedFacts, "moved_messages": st.MovedMessages,
			"kept_keys": st.KeptKeys, "open_tasks": st.OpenTasks,
		},
		Author: st.By, Writer: st.By, Importance: 0.8,
	})
	if err != nil {
		return fmt.Errorf("consolidate: status: %w", err)
	}
	return nil
}

// NamespaceMarks reports, per namespace, when it was last consolidated
// (its newest live ConsolidatedStatusKey) and whether it is an archive
// (holds a live ArchiveSourceKey). One grouped read over the key index
// on the read pool; namespaces with neither are absent.
func (s *Store) NamespaceMarks(ctx context.Context) (map[string]NamespaceMark, error) {
	rows, err := s.db.Reader().QueryContext(ctx, s.db.Rebind(`
		SELECT n.name, m.key, m.action, m.created_at
		FROM memories m
		JOIN namespaces n ON n.id = m.namespace_id
		WHERE m.key IN ($1, $2)
		  AND NOT EXISTS (SELECT 1 FROM memories newer
		      WHERE newer.namespace_id = m.namespace_id AND newer.key = m.key
		        AND newer.created_at > m.created_at)`),
		ConsolidatedStatusKey, ArchiveSourceKey)
	if err != nil {
		return nil, fmt.Errorf("namespace marks: %w", err)
	}
	defer rows.Close()
	out := map[string]NamespaceMark{}
	for rows.Next() {
		var name, key, action, created string
		if err := rows.Scan(&name, &key, &action, &created); err != nil {
			return nil, err
		}
		if action == "tombstone" {
			continue
		}
		m := out[name]
		if key == ConsolidatedStatusKey {
			if t, err := store.TimeFromDB(created); err == nil {
				m.ConsolidatedAt = t
			}
		} else {
			m.Archive = true
		}
		out[name] = m
	}
	return out, rows.Err()
}

// NamespaceMark is one namespace's consolidation state.
type NamespaceMark struct {
	ConsolidatedAt time.Time
	Archive        bool
}
