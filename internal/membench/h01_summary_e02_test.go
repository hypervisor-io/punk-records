package membench

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/memory"
)

// This file is the H01 E02-style broad-question off/on comparison. The
// fixtures are SYNTHETIC and the "answer" is an extractive, deterministic
// composition of the evidence actually surfaced under an equal context
// budget. It is mechanism evidence that the summary hierarchy's benefit AND
// additional cost are measurable; it deliberately does NOT claim a real LLM
// quality win (no paid/external model campaign; a neutral or worse result is
// accepted and reported honestly).
//
// Both arms share the same store, corpus and policy: the same retrieval
// route (UnifiedSearch) and the same equal context budget
// (TokenBudgetCompact). The only difference is whether the summary
// hierarchy exists (off) or was generated and is surfaced (on). Reported:
// hierarchy generation work (SummaryTreeResult ModelCalls / InputTokens
// estimate / Built / Fresh), per-arm surfaced evidence token cost, and the
// traceable raw source IDs surfaced.

// h01Summary is a deterministic Summarizer: the emitted body is the join of
// the child bodies, so a topic query that matches the observations also
// matches the summary (same terms, reproducible, no real model call).
type h01Summary struct{}

func (h01Summary) Summarize(_ context.Context, facts []memory.Fact) (string, error) {
	var bodies []string
	for _, f := range facts {
		bodies = append(bodies, f.Body)
	}
	sort.Strings(bodies)
	return strings.Join(bodies, " | "), nil
}

// h01FactSourceIDs returns the raw source fact IDs a fact grounds in: top
// level for an observation, nested under attributes.summary for a summary
// fact; a bare fact with no source_ids contributes nothing.
func h01FactSourceIDs(f memory.Fact) []string {
	attrs := f.Attributes
	if sm, ok := attrs["summary"].(map[string]any); ok {
		attrs = sm
	}
	raw, _ := attrs["source_ids"].([]any)
	ids := []string{}
	for _, v := range raw {
		if id, ok := v.(string); ok && id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func h01WriteObs(t *testing.T, s *memory.Store, ctx context.Context, key, body, sourceID string) memory.Fact {
	t.Helper()
	f, err := s.Write(ctx, memory.WriteInput{Namespace: "ns", Key: key, Body: body,
		Attributes: map[string]any{"source_ids": []any{sourceID}}})
	if err != nil {
		t.Fatal(err)
	}
	return *f
}

func h01WriteRaw(t *testing.T, s *memory.Store, ctx context.Context, key, body string) memory.Fact {
	t.Helper()
	f, err := s.Write(ctx, memory.WriteInput{Namespace: "ns", Key: key, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	return *f
}

// TestH01SummaryOffOnBroadQuestionComparison builds a corpus of
// source-linked observations over several topic groups, then measures a
// broad question with the hierarchy OFF vs ON under an equal context
// budget. It records extra generation work and per-arm surfaced source IDs
// / evidence cost; it does NOT assert a quality win.
func TestH01SummaryOffOnBroadQuestionComparison(t *testing.T) {
	s := newBenchStore(t)
	ctx := t.Context()

	// Three topic groups, each a raw source plus three source-linked
	// observations whose bodies carry the topical terms the broad question
	// uses.
	type grp struct {
		slug string // observation slug segment
		term string // topical body/query term
	}
	groups := []grp{
		{"database", "database"},
		{"networking", "networking"},
		{"storage", "storage"},
	}
	for _, g := range groups {
		src := h01WriteRaw(t, s, ctx, "/facts/source-"+g.slug, "raw source for "+g.slug)
		for i, letter := range []string{"a", "b", "c"} {
			h01WriteObs(t, s, ctx, "/observations/"+g.slug+"/obs-"+letter,
				g.term+" condition "+letter+" on the "+g.slug+" plane", src.ID)
			_ = i
		}
	}
	broad := "database networking storage"
	const budgetTokens = 1600 // equal context budget for both arms

	// surface returns the surfaced evidence keys, the traceable raw source
	// IDs reachable from that evidence, and the evidence token cost under
	// the equal budget. The extraction is deterministic (no model).
	surface := func() (keys []string, sourceIDs []string, tokens int) {
		hits, err := s.UnifiedSearch(ctx, "ns", broad, 60)
		if err != nil {
			t.Fatal(err)
		}
		compact := memory.TokenBudgetCompact(memory.CompactUnified(hits, 0), budgetTokens)
		seenKey := map[string]bool{}
		srcSeen := map[string]bool{}
		for _, h := range compact {
			if seenKey[h.Key] {
				continue
			}
			seenKey[h.Key] = true
			fs, err := s.Recall(ctx, "ns", h.Key, 1)
			if err != nil || len(fs) != 1 {
				continue
			}
			for _, id := range h01FactSourceIDs(fs[0]) {
				srcSeen[id] = true
			}
			tokens += memory.EstimateTokens(h.Body)
		}
		outK := []string{}
		for k := range seenKey {
			outK = append(outK, k)
		}
		outS := []string{}
		for id := range srcSeen {
			outS = append(outS, id)
		}
		sort.Strings(outK)
		sort.Strings(outS)
		return outK, outS, tokens
	}

	// OFF arm (no summaries exist).
	offStart := time.Now()
	offKeys, offSources, offTokens := surface()
	offElapsed := time.Since(offStart)
	offSummaryCount := 0
	for _, k := range offKeys {
		if strings.HasPrefix(k, "/summaries/") {
			offSummaryCount++
		}
	}

	// ON arm: generate the hierarchy, then retrieve; capture generation work.
	genStart := time.Now()
	res, err := s.BuildSummaryTree(ctx, "ns", h01Summary{}, memory.SummaryTreeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	genElapsed := time.Since(genStart)
	onStart := time.Now()
	onKeys, onSources, onTokens := surface()
	onElapsed := time.Since(onStart)
	onSummaryCount := 0
	for _, k := range onKeys {
		if strings.HasPrefix(k, "/summaries/") {
			onSummaryCount++
		}
	}

	t.Logf("=== synthetic broad-question off/on (equal %d-token budget) ===", budgetTokens)
	t.Logf("OFF  surfaced evidence keys=%d (%d summaries) raw-source IDs=%d evidence_tokens=%d", len(offKeys), offSummaryCount, len(offSources), offTokens)
	t.Logf("ON   surfaced evidence keys=%d (%d summaries) raw-source IDs=%d evidence_tokens=%d", len(onKeys), onSummaryCount, len(onSources), onTokens)
	t.Logf("ON   hierarchy generation: model_calls=%d built=%d fresh=%d input_tokens_estimate=%d", res.ModelCalls, res.Built, res.Fresh, res.InputTokens)

	// Measurement validity (not a quality win): the extra generation work is
	// observable, both arms surface traceable raw-source IDs under the equal
	// budget, and the ON arm actually surfaced the summary layer.
	if res.ModelCalls == 0 || res.InputTokens <= 0 {
		t.Fatalf("ON arm reported no generation model work; benefit/cost not measurable")
	}
	if offSummaryCount != 0 {
		t.Fatalf("OFF arm surfaced summaries (%d); baseline not clean", offSummaryCount)
	}
	if onSummaryCount == 0 {
		t.Fatalf("ON arm surfaced no summaries; hierarchy benefit not observable")
	}
	if len(offSources) == 0 || len(onSources) == 0 {
		t.Fatalf("both arms must surface traceable raw-source IDs (off=%d on=%d)", len(offSources), len(onSources))
	}
	if offTokens == 0 || onTokens == 0 {
		t.Fatalf("both arms must report evidence token cost (off=%d on=%d)", offTokens, onTokens)
	}

	// The answer outcome is the deterministic extractive coverage of the
	// expected topic observations surfaced under the equal budget. Reported
	// as measured; never claimed as a positive win.
	coverage := 0
	expectedObs := map[string]bool{}
	for _, g := range groups {
		for _, letter := range []string{"a", "b", "c"} {
			expectedObs["/observations/"+g.slug+"/obs-"+letter] = true
		}
	}
	for _, k := range onKeys {
		if !strings.HasPrefix(k, "/observations/") {
			continue
		}
		if expectedObs[k] {
			coverage++
		}
	}
	offCoverage := 0
	for _, k := range offKeys {
		if !strings.HasPrefix(k, "/observations/") {
			continue
		}
		if expectedObs[k] {
			offCoverage++
		}
	}
	t.Logf("ANSWER outcome (extractive coverage of %d expected topic observations): OFF=%d/%d ON=%d/%d (neutral, synthetic; no quality win claimed)", len(expectedObs), offCoverage, len(expectedObs), coverage, len(expectedObs))
	t.Logf("elapsed: OFF query=%s ON generation=%s ON query=%s", offElapsed, genElapsed, onElapsed)
	// The scripted summarizer reports input estimates from bytes/4 and no
	// provider usage: real provider cost of generation is UNKNOWN, logged as
	// such rather than fabricated.
	t.Logf("generation provider usage: unknown (scripted summarizer reports no measured usage; input_tokens_estimate=%d is bytes/4)", res.InputTokens)

	if !strings.Contains(broad, "networking") {
		t.Fatal("fixture sanity")
	}
}
