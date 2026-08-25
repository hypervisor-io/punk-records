package memory

import (
	"context"

	"github.com/hypervisor-io/punk-records/internal/store"
)

// RememberModel writes or pins a curated mental model — a durable, top-tier
// synthesis under /mental-models/<slug>. Higher importance than an auto
// observation (0.7) so it outranks beliefs; carries optional source_ids and
// a pinned flag. All models (pinned or not) are already exempt from
// staleness demotion — ObservationStale skips /mental-models/* raw evidence
// regardless. pinned has no behavioral effect today; it's stored for
// future refresh/curation logic.
func (s *Store) RememberModel(ctx context.Context, ns, slug, body string, sourceIDs []string, pinned bool) (*Fact, error) {
	key := "/mental-models/" + slug
	if err := ValidateKey(key); err != nil {
		return nil, err
	}
	attrs := map[string]any{
		"pinned":       pinned,
		"refreshed_at": store.TimeToDB(s.now()),
	}
	if len(sourceIDs) > 0 {
		ids := make([]any, len(sourceIDs))
		for i, id := range sourceIDs {
			ids[i] = id
		}
		attrs["source_ids"] = ids
	}
	return s.Write(ctx, WriteInput{
		Namespace: ns, Key: key, Body: body,
		Attributes: attrs, Writer: "curator", Importance: 0.7,
	})
}

// ListModels returns live mental models under ns, newest revision per key.
func (s *Store) ListModels(ctx context.Context, ns string) ([]Fact, error) {
	return s.Recall(ctx, ns, "/mental-models", 1000)
}
