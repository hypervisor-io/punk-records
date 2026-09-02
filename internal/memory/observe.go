package memory

import (
	"context"
	"fmt"

	"github.com/hypervisor-io/punk-records/internal/store"
)

// Observation is one consolidated belief with provenance.
type Observation struct {
	Slug      string   `json:"slug"` // key segment: /observations/<slug>
	Body      string   `json:"body"`
	SourceIDs []string `json:"source_ids"` // fact IDs this belief is grounded in

	// Kind labels how the belief was reasoned: "explicit" (stated
	// outright in a
	// source fact), "deductive" (follows necessarily from sources),
	// "inductive" (a pattern across sources - requires >= 2 of them, see
	// ConsolidateObservations), or "abductive" (the simplest explanation
	// of the sources). Empty is allowed and means unlabeled; any other
	// value is dropped to empty at write time.
	Kind string `json:"kind,omitempty"`
}

// observationKinds is the allowlist for Observation.Kind.
var observationKinds = map[string]bool{
	"explicit": true, "deductive": true, "inductive": true, "abductive": true,
}

// ObservationSummarizer folds raw facts into consolidated beliefs.
type ObservationSummarizer interface {
	Observe(ctx context.Context, facts []Fact) ([]Observation, error)
}

// ConsolidateObservations folds live facts into consolidated beliefs.
// Deterministic-first rule: without a summarizer this is a no-op, and
// observation slugs are validated like any key.
func (s *Store) ConsolidateObservations(ctx context.Context, ns string, obs ObservationSummarizer) (int, error) {
	if obs == nil {
		return 0, nil
	}
	facts, err := s.Recall(ctx, ns, "/", 1000)
	if err != nil {
		return 0, err
	}
	// never consolidate the consolidated, and never feed a curated model
	// back in as raw evidence — models are syntheses, not raw evidence.
	raw := facts[:0]
	for _, f := range facts {
		if !hasPrefix(f.Key, "/observations/") && !hasPrefix(f.Key, "/consolidated/") && !hasPrefix(f.Key, "/mental-models/") && !hasPrefix(f.Key, "/entities/") {
			raw = append(raw, f)
		}
	}
	if len(raw) == 0 {
		return 0, nil
	}
	found, err := obs.Observe(ctx, raw)
	if err != nil {
		return 0, fmt.Errorf("observe: %w", err)
	}
	written := 0
	for _, o := range found {
		key := "/observations/" + o.Slug
		if err := ValidateKey(key); err != nil {
			continue // bad slug from the model: drop, don't fail the run
		}
		if len(o.SourceIDs) == 0 {
			continue // evidence or it did not happen — same contract as findings
		}
		if !observationKinds[o.Kind] {
			o.Kind = "" // unknown label from the model: unlabeled, not failed
		}
		if o.Kind == "inductive" && len(o.SourceIDs) < 2 {
			// A pattern claim needs at least two data points.
			// One source can state or imply a fact, but it cannot exhibit a
			// pattern - drop the belief, same as missing evidence.
			continue
		}
		ids := make([]any, len(o.SourceIDs))
		for i, id := range o.SourceIDs {
			ids[i] = id
		}
		attrs := map[string]any{"source_ids": ids, "proof_count": float64(len(o.SourceIDs)),
			"consolidated_at": store.TimeToDB(s.now())}
		if o.Kind != "" {
			attrs["kind"] = o.Kind
		}
		if _, err := s.Write(ctx, WriteInput{
			Namespace: ns, Key: key, Body: o.Body,
			Attributes: attrs,
			Writer:     "consolidation", Importance: 0.5,
		}); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}

// ObservationStale reports whether any live raw fact in ns was created
// after the observation was consolidated — i.e. the belief may no longer
// reflect current evidence. Cheap: one existence query.
func (s *Store) ObservationStale(ctx context.Context, ns string, obs Fact) (bool, error) {
	consolidatedAt := obs.CreatedAt
	if raw, ok := obs.Attributes["consolidated_at"].(string); ok {
		if t, err := store.TimeFromDB(raw); err == nil {
			consolidatedAt = t
		}
	}
	var stale bool
	err := s.db.QueryRowContext(ctx, s.db.Rebind(`
		SELECT EXISTS(
			SELECT 1 FROM memories m
			JOIN namespaces n ON n.id = m.namespace_id
			WHERE n.name = $1 AND m.action <> 'tombstone' AND m.invalid_at IS NULL
			  AND m.key NOT LIKE '/observations/%' AND m.key NOT LIKE '/consolidated/%' AND m.key NOT LIKE '/mental-models/%' AND m.key NOT LIKE '/entities/%'
			  AND (m.expiration_date IS NULL OR m.expiration_date > $2)
			  AND m.created_at > $3
		)`), ns, store.TimeToDB(s.now()), store.TimeToDB(consolidatedAt)).Scan(&stale)
	if err != nil {
		return false, fmt.Errorf("observation stale query: %w", err)
	}
	return stale, nil
}
