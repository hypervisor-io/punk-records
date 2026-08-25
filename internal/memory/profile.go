package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// ProfileEntity is one top entity in a namespace digest, ranked by
// mention_count (set by the enricher on /entities/* facts).
type ProfileEntity struct {
	Name     string  `json:"name"`
	Mentions float64 `json:"mentions"`
}

// ProfileKey is one hot key in a namespace digest, ranked by access_count.
type ProfileKey struct {
	Key         string `json:"key"`
	AccessCount int64  `json:"access_count"`
}

// ProfileFact is one recent fact in a namespace digest.
type ProfileFact struct {
	Key  string `json:"key"`
	Body string `json:"body"`
}

// Profile is a deterministic namespace digest: pure SQL - no LLM, no
// cache.
type Profile struct {
	Namespace    string          `json:"namespace"`
	Facts        int             `json:"facts"`
	Observations int             `json:"observations"`
	Models       int             `json:"models"`
	EntityCount  int             `json:"entity_count"`
	Links        int             `json:"links"` // live edges only; closed (invalid_at set) edges are excluded
	TopEntities  []ProfileEntity `json:"top_entities"`
	HotKeys      []ProfileKey    `json:"hot_keys"`
	Recent       []ProfileFact   `json:"recent"`
}

// clipBody truncates a fact body to n runes for the recent-facts digest,
// appending an ellipsis when truncated. Cuts on a rune boundary so a
// multibyte character is never split (which would corrupt the string
// with a stray U+FFFD on re-encode). Iterates the string counting runes
// and byte-slices at the boundary rather than allocating a []rune copy
// of the whole body.
func clipBody(s string, n int) string {
	count := 0
	for i := range s {
		if count == n {
			return s[:i] + "..."
		}
		count++
	}
	return s
}

// Profile computes a deterministic digest of a namespace: counts by
// category, top entities by mention_count, hot keys by access_count, and
// the most recent non-entity, non-agent-session facts. Pure SQL, no LLM
// and no cache, so callers get a stable answer for the same data. An
// unknown namespace yields a zero-valued Profile, not an error.
//
// All counts and rankings are computed over Recall's live set, which caps
// at 1000 facts and orders by key. On a namespace with more than 1000
// live facts, counts saturate at 1000 and the facts considered are the
// first 1000 alphabetically by key - not necessarily the most complete or
// most recent picture of a very large namespace.
//
// HotKeys stops at the first zero-access fact once at least one entry
// exists, so keys tied at zero accesses after the last touched key never
// appear even when they are otherwise eligible. When nothing in the
// namespace has ever been accessed, this still yields exactly one
// zero-access key (the first by the access_count-desc, key-asc sort) -
// never zero and never all of them, provided at least one eligible key
// exists. When the namespace is empty, or every live fact falls under
// /agent-sessions/ or /entities/, there are no eligible keys and HotKeys
// is legitimately empty.
func (s *Store) Profile(ctx context.Context, ns string) (*Profile, error) {
	p := &Profile{Namespace: ns, TopEntities: []ProfileEntity{}, HotKeys: []ProfileKey{}, Recent: []ProfileFact{}}
	facts, err := s.Recall(ctx, ns, "/", 1000)
	if err != nil {
		return nil, err
	}
	p.Facts = len(facts)

	var recents []Fact
	for _, f := range facts {
		switch {
		case strings.HasPrefix(f.Key, "/entities/"):
			p.EntityCount++
			mc, _ := f.Attributes["mention_count"].(float64)
			p.TopEntities = append(p.TopEntities, ProfileEntity{Name: f.Body, Mentions: mc})
		case strings.HasPrefix(f.Key, "/observations/"):
			p.Observations++
		case strings.HasPrefix(f.Key, "/mental-models/"):
			p.Models++
		}
		if !strings.HasPrefix(f.Key, "/agent-sessions/") && !strings.HasPrefix(f.Key, "/entities/") {
			recents = append(recents, f)
		}
	}

	sort.SliceStable(p.TopEntities, func(a, b int) bool {
		if p.TopEntities[a].Mentions != p.TopEntities[b].Mentions {
			return p.TopEntities[a].Mentions > p.TopEntities[b].Mentions
		}
		return p.TopEntities[a].Name < p.TopEntities[b].Name
	})
	if len(p.TopEntities) > 10 {
		p.TopEntities = p.TopEntities[:10]
	}

	sort.SliceStable(recents, func(a, b int) bool {
		if !recents[a].CreatedAt.Equal(recents[b].CreatedAt) {
			return recents[a].CreatedAt.After(recents[b].CreatedAt)
		}
		return recents[a].Key < recents[b].Key
	})
	for i, f := range recents {
		if i >= 10 {
			break
		}
		p.Recent = append(p.Recent, ProfileFact{Key: f.Key, Body: clipBody(f.Body, 200)})
	}

	// hot keys: access_count desc over the same latest-live set, excluding
	// agent-session and entity keys (those aren't user-facing knowledge).
	hot := append([]Fact(nil), facts...)
	sort.SliceStable(hot, func(a, b int) bool {
		if hot[a].AccessCount != hot[b].AccessCount {
			return hot[a].AccessCount > hot[b].AccessCount
		}
		return hot[a].Key < hot[b].Key
	})
	for _, f := range hot {
		if strings.HasPrefix(f.Key, "/agent-sessions/") || strings.HasPrefix(f.Key, "/entities/") {
			continue
		}
		if f.AccessCount == 0 && len(p.HotKeys) > 0 {
			break
		}
		p.HotKeys = append(p.HotKeys, ProfileKey{Key: f.Key, AccessCount: f.AccessCount})
		if len(p.HotKeys) >= 10 {
			break
		}
	}

	// Links is a live-edge count: closed links (invalid_at set) are
	// filtered out, same as every other default link read.
	if err := s.db.QueryRowContext(ctx, s.db.Rebind(
		`SELECT count(*) FROM memory_links WHERE namespace = $1 AND invalid_at IS NULL`), ns).Scan(&p.Links); err != nil {
		return nil, fmt.Errorf("profile links count: %w", err)
	}
	return p, nil
}
