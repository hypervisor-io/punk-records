package skillmine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hypervisor-io/punk-records/internal/memory"
)

// InsightDrafter turns a namespace's distilled memory into proposed
// CLAUDE.md additions. The LLM adapter lives
// in cmd; tests use fakes.
type InsightDrafter interface {
	DraftInsights(ctx context.Context, ns string, facts []memory.Fact) (string, error)
}

// insightFactCap bounds how many facts feed one insights draft: mental
// models and observations all qualify, raw facts only when important.
const insightFactCap = 120

// GatherInsightFacts collects the evidence an insights draft reasons
// over, hierarchy-ordered exactly like reflect: mental models first,
// then observations, then important raw facts (importance >= 0.5),
// capped at insightFactCap. /agent-sessions/ and /entities/ never
// qualify - ephemeral capture and entity nodes are not rules.
func GatherInsightFacts(ctx context.Context, mem *memory.Store, ns string) ([]memory.Fact, error) {
	var out []memory.Fact
	models, err := mem.ListModels(ctx, ns)
	if err != nil {
		return nil, err
	}
	out = append(out, models...)
	obs, err := mem.Recall(ctx, ns, "/observations/", insightFactCap)
	if err != nil {
		return nil, err
	}
	out = append(out, obs...)
	raw, err := mem.Recall(ctx, ns, "/", 1000)
	if err != nil {
		return nil, err
	}
	for _, f := range raw {
		if len(out) >= insightFactCap {
			break
		}
		if f.Importance < 0.5 {
			continue
		}
		k := f.Key
		if strings.HasPrefix(k, "/agent-sessions/") || strings.HasPrefix(k, "/entities/") ||
			strings.HasPrefix(k, "/observations/") || strings.HasPrefix(k, "/mental-models/") {
			continue
		}
		out = append(out, f)
	}
	if len(out) > insightFactCap {
		out = out[:insightFactCap]
	}
	return out, nil
}

// WriteInsights drafts proposed CLAUDE.md additions for ns into
// outDir/<ns>/CLAUDE-additions.md and returns the path ("" when the
// namespace had nothing to distill). Like WriteDrafts, this writes
// PROPOSALS for a human to review and merge - it never touches an
// actual CLAUDE.md. Re-running overwrites the previous proposal.
func WriteInsights(ctx context.Context, mem *memory.Store, ns string, d InsightDrafter, outDir string) (string, error) {
	facts, err := GatherInsightFacts(ctx, mem, ns)
	if err != nil {
		return "", err
	}
	if len(facts) == 0 {
		return "", nil
	}
	body, err := d.DraftInsights(ctx, ns, facts)
	if err != nil {
		return "", fmt.Errorf("draft insights for %s: %w", ns, err)
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return "", nil
	}
	slug := slugify(ns)
	if slug == "" {
		slug = "namespace"
	}
	dir := filepath.Join(outDir, slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("write insights %s: %w", slug, err)
	}
	content := fmt.Sprintf(`<!-- Proposed CLAUDE.md additions distilled from punk-records memory.
     Namespace: %s | facts considered: %d | generated: %s
     Review each rule, keep what's right, merge by hand - punk never edits CLAUDE.md itself. -->

%s
`, ns, len(facts), time.Now().UTC().Format(time.RFC3339), body)
	path := filepath.Join(dir, "CLAUDE-additions.md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write insights %s: %w", slug, err)
	}
	return path, nil
}
