package memory

import (
	"context"
	"fmt"
	"time"

	"github.com/hypervisor-io/punk-records/internal/store"
)

// Diagnosis reports namespace health counters - deterministic once no
// corrupt rows remain: every number is one query or one scan.
type Diagnosis struct {
	Namespace         string `json:"namespace"`
	Quarantined       int    `json:"quarantined"`
	MissingEmbeddings int    `json:"missing_embeddings"` // -1 when no embedder configured
	// OversizeEmbeddings counts latest live facts whose embedding input
	// (key + body) estimates above the configured model window and is
	// therefore truncated by the provider; -1 when no embedder or no
	// window is configured. Recall caps the scan at 1000 facts.
	OversizeEmbeddings int `json:"oversize_embeddings"`
	OrphanLinks        int `json:"orphan_links"`
	StaleObservations int    `json:"stale_observations"`
	ExpiredClaims     int    `json:"expired_claims"`

	// Consolidation observability. LastConsolidatedAt is when this process last finished a
	// consolidation pass for the namespace (RFC3339; empty when it hasn't
	// - the record is in-process, so a restart clears it).
	// WritesSinceConsolidation counts fact revisions written after that
	// pass; -1 when no pass is recorded, mirroring MissingEmbeddings'
	// "not meaningful" sentinel.
	LastConsolidatedAt       string `json:"last_consolidated_at,omitempty"`
	WritesSinceConsolidation int    `json:"writes_since_consolidation"`
}

// diagnoseLiveKeyChunk bounds the IN clause when resolving link-endpoint
// liveness through liveByKeys.
const diagnoseLiveKeyChunk = 500

// Diagnose reports namespace health counters: quarantined rows, live facts
// missing an embedding (only meaningful when an embedder is configured,
// else -1), links where an endpoint has no live fact, observations
// consolidated before a newer raw fact exists, and expired-but-unreleased
// work claims. Pure SQL/scan, no LLM.
//
// Order matters: read-path scans can quarantine corrupt rows (memory.go's
// scanFacts does this during the observations and link-liveness scans
// below), so every counter that reads memories or memories_quarantine runs
// after all scans - MissingEmbeddings, then Quarantined, then
// ExpiredClaims. A counter run before the scan that quarantines a corrupt
// row would count it once, then not again on the next call; running every
// counter after every scan keeps that count part of this call's own
// result instead of leaking across calls.
//
// Residual caps: StaleObservations scans at most the first 1000
// observation keys, alphabetical by key (Recall capped to the
// /observations/ prefix) - a namespace with more than 1000 live
// observations undercounts. Quarantined, MissingEmbeddings, OrphanLinks,
// and ExpiredClaims are full-table counts, uncapped. StaleObservations
// also issues one EXISTS query per observation considered (bounded by the
// same 1000-observation cap), not a single batched query. There is a
// further source of cross-call nondeterminism the ordering fix above
// cannot close: when a corrupt latest revision shadows an older good
// revision at a link endpoint, quarantining it on call 1 counts the
// endpoint as dead (OrphanLinks inflated for that call), while call 2
// sees the resurfaced older revision as live - quarantine-on-read
// revision shadowing is a package-wide property of memory.go's
// scanFacts, not something specific to Diagnose.
//
// OrphanLinks materializes the namespace's link rows and endpoint key set
// in memory; liveByKeys re-runs the namespace lookup once per 500-key
// chunk rather than resolving it once and reusing it.
func (s *Store) Diagnose(ctx context.Context, ns string) (*Diagnosis, error) {
	// now is a single snapshot used by the MissingEmbeddings and
	// ExpiredClaims queries below only; the helper scans this function
	// calls (Recall, liveByKeys, ObservationStale) read the clock
	// themselves and are not pinned to this snapshot. The snapshot is taken
	// before the scans run, so MissingEmbeddings and ExpiredClaims are
	// evaluated against a timestamp that predates however long the scans
	// take - intentional, it keeps the two counters mutually consistent
	// with each other rather than drifting apart over the scan duration.
	now := s.now()
	d := &Diagnosis{Namespace: ns, MissingEmbeddings: -1, OversizeEmbeddings: -1}
	nsID, ok, err := s.namespaceID(ctx, ns)
	if err != nil {
		return nil, fmt.Errorf("diagnose namespace lookup: %w", err)
	}

	// --- scans first: these can quarantine corrupt rows they encounter,
	// so every counter below that reads memories or memories_quarantine
	// must run after all of them. The order between the scans is also
	// load-bearing: the link scan must precede the observations scan,
	// because a corrupt link-endpoint row quarantined by liveByKeys could
	// otherwise flip ObservationStale's EXISTS between calls. ---

	// orphan links: either endpoint has no live fact. Liveness comes from
	// the actual link keys (resolved via liveByKeys), never from a capped
	// namespace-wide scan - a link to a live fact outside any arbitrary
	// cap must never be miscounted as orphaned. OrphanLinks is a live-edge
	// count: closed links (invalid_at set) are excluded, same as every
	// other default link read.
	rows, err := s.db.QueryContext(ctx, s.db.Rebind(
		`SELECT from_key, to_key FROM memory_links WHERE namespace = $1 AND invalid_at IS NULL`), ns)
	if err != nil {
		return nil, fmt.Errorf("diagnose links: %w", err)
	}
	type linkPair struct{ from, to string }
	var pairs []linkPair
	keySet := map[string]struct{}{}
	for rows.Next() {
		var from, to string
		if err := rows.Scan(&from, &to); err != nil {
			rows.Close()
			return nil, fmt.Errorf("diagnose links scan: %w", err)
		}
		pairs = append(pairs, linkPair{from, to})
		keySet[from] = struct{}{}
		keySet[to] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("diagnose links rows: %w", err)
	}
	rows.Close()

	allKeys := make([]string, 0, len(keySet))
	for k := range keySet {
		allKeys = append(allKeys, k)
	}
	liveKeys := make(map[string]bool, len(allKeys))
	for i := 0; i < len(allKeys); i += diagnoseLiveKeyChunk {
		end := i + diagnoseLiveKeyChunk
		if end > len(allKeys) {
			end = len(allKeys)
		}
		facts, err := s.liveByKeys(ctx, ns, allKeys[i:end])
		if err != nil {
			return nil, fmt.Errorf("diagnose link liveness: %w", err)
		}
		for _, f := range facts {
			liveKeys[f.Key] = true
		}
	}
	for _, p := range pairs {
		if !liveKeys[p.from] || !liveKeys[p.to] {
			d.OrphanLinks++
		}
	}

	// stale observations: prefix-scoped scan, so the 1000-fact cap applies
	// to observations only, not the whole namespace.
	obs, err := s.Recall(ctx, ns, "/observations/", 1000)
	if err != nil {
		return nil, fmt.Errorf("diagnose observations recall: %w", err)
	}
	for _, f := range obs {
		stale, err := s.ObservationStale(ctx, ns, f)
		if err != nil {
			return nil, fmt.Errorf("diagnose observation stale: %w", err)
		}
		if stale {
			d.StaleObservations++
		}
	}

	// --- SQL-side counters: every scan above that can quarantine a corrupt
	// row (liveByKeys and Recall both route through scanFacts) has already
	// run, so these read memories and memories_quarantine post-quarantine
	// for this call - a row quarantined above is reflected in this call's
	// own counts rather than leaking into the next one. ---

	if s.embedder != nil {
		if ok {
			// same latest-live shape as latestLiveSQL (memory.go), without the
			// key-prefix filter: every key in the namespace, not one subtree.
			if err := s.db.QueryRowContext(ctx, s.db.Rebind(`
				SELECT count(*) FROM memories m
				JOIN (
				    SELECT key, MAX(created_at) AS mc
				    FROM memories
				    WHERE namespace_id = $1
				    GROUP BY key
				) latest ON m.key = latest.key AND m.created_at = latest.mc
				WHERE m.namespace_id = $1 AND m.action <> 'tombstone'
				  AND (m.expiration_date IS NULL OR m.expiration_date > $2)
				  AND m.embedding IS NULL`),
				nsID, store.TimeToDB(now)).Scan(&d.MissingEmbeddings); err != nil {
				return nil, fmt.Errorf("diagnose missing embeddings: %w", err)
			}
		} else {
			// no facts to be missing embeddings, but an embedder is
			// configured: 0, not the "no embedder" sentinel.
			d.MissingEmbeddings = 0
		}
	}

	if s.embedder != nil && s.embedMaxTokens > 0 {
		facts, err := s.Recall(ctx, ns, "", 0)
		if err != nil {
			return nil, fmt.Errorf("diagnose oversize scan: %w", err)
		}
		d.OversizeEmbeddings = 0
		for _, f := range facts {
			if EstimateTokens(embedText(f.Key, f.Body)) > s.embedMaxTokens {
				d.OversizeEmbeddings++
			}
		}
	}

	if ok {
		if err := s.db.QueryRowContext(ctx, s.db.Rebind(
			`SELECT count(*) FROM memories_quarantine WHERE namespace_id = $1`), nsID).Scan(&d.Quarantined); err != nil {
			return nil, fmt.Errorf("diagnose quarantine: %w", err)
		}
	}

	if err := s.db.QueryRowContext(ctx, s.db.Rebind(`
		SELECT count(*) FROM work_claims
		WHERE namespace = $1 AND released_at IS NULL AND expires_at < $2`),
		ns, store.TimeToDB(now)).Scan(&d.ExpiredClaims); err != nil {
		return nil, fmt.Errorf("diagnose expired claims: %w", err)
	}

	d.WritesSinceConsolidation = -1
	if last, okc := s.LastConsolidated(ns); okc {
		d.LastConsolidatedAt = last.UTC().Format(time.RFC3339)
		if writes, _, err := s.WriteActivity(ctx, ns, last); err == nil {
			d.WritesSinceConsolidation = writes
		}
	}

	return d, nil
}
