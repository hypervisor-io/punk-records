package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hypervisor-io/punk-records/internal/store"
)

// SessionSummarizer folds a session's new capture events into a rolling
// narrative, given the prior summary (empty on the first pass). The
// recursion is the point: each pass sees the previous summary
// plus only the events after it, which biases the narrative toward
// recent activity while still covering the whole session.
type SessionSummarizer interface {
	Summarize(ctx context.Context, prior string, facts []Fact) (string, error)
}

// sessionSummaryKeySuffix names the rolling-summary fact inside a
// session's capture subtree: /agent-sessions/<sid>/summary.
const sessionSummaryKeySuffix = "/summary"

// sessionBookkeepingSuffixes are per-session facts that are metadata, not
// capture: they never count toward the summarization threshold and are
// never fed to the summarizer.
func sessionBookkeepingKey(rest string) bool {
	return rest == "summary" || rest == "injected"
}

// SummarizeSessions maintains one rolling summary per captured agent
// session in ns.
// For each session under /agent-sessions/<sid>/, when the capture
// events written after the session's last summary weigh at least
// thresholdTokens, the summarizer receives the prior summary plus those
// new events and its answer replaces /agent-sessions/<sid>/summary
// (same-key supersede - the revision chain is the summary history).
// The summary fact carries summarized_at and expires with ttl, like the
// capture it compresses. Deterministic-first: a nil summarizer or a
// non-positive threshold is a no-op. Returns summaries written; the
// first summarizer or store failure aborts the remaining sessions in
// this namespace (fail-fast within a namespace, same as the
// consolidation pass) - the next tick retries them.
func (s *Store) SummarizeSessions(ctx context.Context, ns string, thresholdTokens int, sum SessionSummarizer, ttl time.Duration) (int, error) {
	if sum == nil || thresholdTokens <= 0 {
		return 0, nil
	}
	facts, err := s.Recall(ctx, ns, "/agent-sessions/", 1000)
	if err != nil {
		return 0, err
	}
	type sess struct {
		prior        string
		summarizedAt time.Time
		events       []Fact
	}
	sessions := map[string]*sess{}
	get := func(sid string) *sess {
		if sessions[sid] == nil {
			sessions[sid] = &sess{}
		}
		return sessions[sid]
	}
	for _, f := range facts {
		rest := strings.TrimPrefix(f.Key, "/agent-sessions/")
		sid, sub, found := strings.Cut(rest, "/")
		if !found || sid == "" {
			continue
		}
		if sub == "summary" {
			st := get(sid)
			st.prior = f.Body
			st.summarizedAt = f.CreatedAt
			if raw, ok := f.Attributes["summarized_at"].(string); ok {
				if t, err := store.TimeFromDB(raw); err == nil {
					st.summarizedAt = t
				}
			}
			continue
		}
		if sessionBookkeepingKey(sub) {
			continue
		}
		get(sid).events = append(get(sid).events, f)
	}

	written := 0
	sids := make([]string, 0, len(sessions))
	for sid := range sessions {
		sids = append(sids, sid)
	}
	sort.Strings(sids) // deterministic iteration for tests and logs
	for _, sid := range sids {
		st := sessions[sid]
		var fresh []Fact
		tokens := 0
		for _, f := range st.events {
			if !st.summarizedAt.IsZero() && !f.CreatedAt.After(st.summarizedAt) {
				continue
			}
			fresh = append(fresh, f)
			tokens += EstimateTokens(f.Body)
		}
		if len(fresh) == 0 || tokens < thresholdTokens {
			continue
		}
		// Chronological order for the summarizer: capture keys are
		// prompt-/tool-id based, so Recall's key order is not time order.
		sort.Slice(fresh, func(a, b int) bool { return fresh[a].CreatedAt.Before(fresh[b].CreatedAt) })
		body, err := sum.Summarize(ctx, st.prior, fresh)
		if err != nil {
			return written, fmt.Errorf("summarize session %s: %w", sid, err)
		}
		body = strings.TrimSpace(body)
		if body == "" {
			continue // a refusal/empty answer must not blank the summary
		}
		var expiresAt *time.Time
		if ttl > 0 {
			t := s.now().Add(ttl)
			expiresAt = &t
		}
		if _, err := s.Write(ctx, WriteInput{
			Namespace: ns, Key: "/agent-sessions/" + sid + sessionSummaryKeySuffix, Body: body,
			Attributes: map[string]any{
				"summarized_at": store.TimeToDB(s.now()),
				"event_count":   float64(len(fresh)),
			},
			Writer: "session-summary", ExpiresAt: expiresAt,
		}); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}

// LatestSessionSummary returns the most recently written session summary
// in ns (the "Last session" narrative for context injection), or nil
// when none exists.
func (s *Store) LatestSessionSummary(ctx context.Context, ns string) (*Fact, error) {
	facts, err := s.Recall(ctx, ns, "/agent-sessions/", 1000)
	if err != nil {
		return nil, err
	}
	var latest *Fact
	for i := range facts {
		f := &facts[i]
		rest := strings.TrimPrefix(f.Key, "/agent-sessions/")
		if _, sub, found := strings.Cut(rest, "/"); !found || sub != "summary" {
			continue
		}
		if latest == nil || f.CreatedAt.After(latest.CreatedAt) {
			latest = f
		}
	}
	return latest, nil
}
