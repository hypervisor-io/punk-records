package membench

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/hypervisor-io/punk-records/internal/llm"
	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/reflect"
)

// The round-3 review reproducers (E02), kept as permanent regression
// tests. Each pins one finding against the evidence/usage contract:
// shown evidence resolves through the bodies the composer carried out
// (complete no matter how large the namespace is - a capped namespace
// re-scan silently omits shown facts past the cap), resolution never
// crosses namespaces, and a composer's unreported usage is counted per
// model call instead of collapsing a multi-call loop into at most one
// unknown call per composition.

// TestReflectShownEvidenceResolvesBeyondRecallCap (finding 1): a real
// reflect mental-model citation must resolve to its shown body even when
// the namespace holds more facts than any single capped Recall could
// return (here 1001 /aaa/* fillers). The earlier evidence cache scanned
// Recall(ns, "", 1000), which returned only the alphabetically-first
// 1000 keys and silently omitted the shown /mental-models/* fact: the
// citation passed existence but the gold judge failed support on an
// unresolved citation. Resolution now uses the shown evidence bodies the
// reflect loop carried out (ComposeResult.ShownFacts), which are
// complete by construction.
func TestReflectShownEvidenceResolvesBeyondRecallCap(t *testing.T) {
	s := newBenchStore(t)
	ctx := t.Context()
	model, err := s.RememberModel(ctx, "bench", "fleet-posture", "fleet posture: two primaries per region", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	recs := []AnswerRecord{
		{Record: Record{Type: "fact", Key: "/svc/db", Body: "postgres primary runs on host7"}},
		// The query terms match nothing in the corpus: the initial
		// retrieval returns empty, so the mental-model citation is
		// definitionally outside the initial top-k.
		{Record: Record{Type: "query", Q: "antarctic flamingo census", Expect: []string{"/mental-models/fleet-posture"}}, Answer: "fleet posture: two primaries per region"},
	}
	// Push the namespace past any single capped recall: the shown model
	// sorts after all 1001 fillers, so a capped scan cannot return it.
	for i := 0; i < 1001; i++ {
		if _, err := s.Write(ctx, memory.WriteInput{Namespace: "bench", Key: fmt.Sprintf("/aaa/%04d", i), Body: "unrelated filler"}); err != nil {
			t.Fatal(err)
		}
	}
	script := &scriptedLLM{t: t}
	script.queue = []func([]llm.Turn) (*llm.Result, error){
		scriptedCall("c1", "list_models", json.RawMessage(`{}`)),
		func([]llm.Turn) (*llm.Result, error) {
			args, _ := json.Marshal(map[string]any{
				"answer":    "fleet posture: two primaries per region",
				"citations": []string{model.ID},
			})
			return &llm.Result{ToolCalls: []llm.ToolCall{{ID: "c2", Name: "done", Args: args}}, PromptTokens: 10, CompletionTokens: 5}, nil
		},
	}
	composer := NewReflectComposer(reflect.New(s, script), "bench", "http://scripted", "scripted-model")
	rep, err := RunAnswers(ctx, s, "bench", recs, AnswerOptions{K: 5, Composer: composer, MaxTokens: 1000})
	if err != nil {
		t.Fatal(err)
	}
	c := answerCase(t, rep, "antarctic flamingo census")
	if len(c.RetrievedIDs) != 0 {
		t.Fatalf("retrieved IDs = %v, want empty: the citation must lie outside the initial top-k", c.RetrievedIDs)
	}
	if len(c.CitedIDs) != 1 || c.CitedIDs[0] != model.ID {
		t.Fatalf("cited IDs = %v, want [%s] (the reflect-shown mental model)", c.CitedIDs, model.ID)
	}
	if !c.CitationExist {
		t.Fatal("CitationExist = false for a citation the composer was actually shown")
	}
	if c.Error != "" {
		t.Fatalf("case error = %q, want none", c.Error)
	}
	if c.Support == nil || !*c.Support {
		t.Fatalf("Support = %v, want true: the shown model evidence resolves from the carried bodies and its key is gold", c.Support)
	}
	if c.ExactMatch == nil || !*c.ExactMatch {
		t.Fatalf("ExactMatch = %v, want true", c.ExactMatch)
	}
	sum := rep.Summary
	if sum.RetrievalMisses != 1 {
		t.Fatalf("RetrievalMisses = %d, want 1: the initial retrieval genuinely missed the gold model", sum.RetrievalMisses)
	}
	if sum.SupportRate == nil || *sum.SupportRate != 1.0 {
		t.Fatalf("SupportRate = %v, want 1.0", sum.SupportRate)
	}
}

// TestCitationKnownOnlyInAnotherNamespaceNeverResolves is the
// cross-namespace denial control for the finding-1 fix: a cited ID that
// exists ONLY in another namespace may pass citation existence (the
// composer claimed it as known) but must never resolve to evidence -
// resolution stays inside the stage's isolated namespace, so the gold
// judge fails support on the unresolved citation.
func TestCitationKnownOnlyInAnotherNamespaceNeverResolves(t *testing.T) {
	s := newBenchStore(t)
	ctx := t.Context()
	// The model lives in namespace "other", never in the stage's "bench".
	other, err := s.RememberModel(ctx, "other", "fleet-posture", "fleet posture: two primaries per region", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	recs := []AnswerRecord{
		{Record: Record{Type: "fact", Key: "/svc/db", Body: "postgres primary runs on host7"}},
		{Record: Record{Type: "query", Q: "postgres primary", Expect: []string{"/svc/db"}}, Answer: "postgres primary runs on host7"},
	}
	composer := fakeComposer{
		cfg: ComposerConfig{Provider: "scripted"},
		fn: func(_ context.Context, _ string, facts []memory.Fact) (ComposeResult, error) {
			db := findFact(t, facts, "/svc/db")
			// The composer claims the cross-namespace ID as known and
			// cites it alongside the real retrieved gold fact.
			return ComposeResult{
				Text:       "postgres primary runs on host7",
				CitedIDs:   []string{db.ID, other.ID},
				KnownIDs:   []string{db.ID, other.ID},
				ShownFacts: []memory.Fact{db},
			}, nil
		},
	}
	rep, err := RunAnswers(ctx, s, "bench", recs, AnswerOptions{K: 5, Composer: composer})
	if err != nil {
		t.Fatal(err)
	}
	c := answerCase(t, rep, "postgres primary")
	if !c.CitationExist {
		t.Fatal("CitationExist = false, want true: existence is scored against the composer's known set")
	}
	if c.Support == nil || *c.Support {
		t.Fatalf("Support = %v, want explicit false: a cross-namespace citation must never resolve to evidence", c.Support)
	}
	if !strings.Contains(c.Note, "did not resolve") {
		t.Fatalf("case note = %q, want the unresolved-citation reason", c.Note)
	}
	if c.ExactMatch == nil || !*c.ExactMatch {
		t.Fatalf("ExactMatch = %v, want true (the answer text is right; support is the failed dimension)", c.ExactMatch)
	}
	sum := rep.Summary
	if sum.SupportRate == nil || *sum.SupportRate != 0.0 {
		t.Fatalf("SupportRate = %v, want 0.0", sum.SupportRate)
	}
}

// TestForeignNamespaceShownEvidenceNeverEarnsSupport (round-4 finding):
// a REAL reflect composer bound to namespace "other" but evaluated by
// RunAnswers in namespace "bench" carries a foreign mental-model fact in
// its shown evidence (the round-3 control only covered a bench fact in
// ShownFacts with the foreign ID merely claimed). The carried body must
// never be admissible: resolution requires the shown fact's namespace to
// match the stage namespace, so the citation goes unresolved and the
// gold judge fails support. Citation existence still passes - the
// composer did report the ID as known - and exact match still grades the
// answer text; support is the denied dimension.
func TestForeignNamespaceShownEvidenceNeverEarnsSupport(t *testing.T) {
	s := newBenchStore(t)
	ctx := t.Context()
	// The model lives in namespace "other"; the stage runs in "bench".
	model, err := s.RememberModel(ctx, "other", "fleet-posture", "fleet posture: two primaries per region", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	script := &scriptedLLM{t: t}
	script.queue = []func([]llm.Turn) (*llm.Result, error){
		scriptedCall("c1", "list_models", json.RawMessage(`{}`)),
		func([]llm.Turn) (*llm.Result, error) {
			args, _ := json.Marshal(map[string]any{
				"answer":    model.Body,
				"citations": []string{model.ID},
			})
			return &llm.Result{ToolCalls: []llm.ToolCall{{ID: "c2", Name: "done", Args: args}}, PromptTokens: 10, CompletionTokens: 5}, nil
		},
	}
	composer := NewReflectComposer(reflect.New(s, script), "other", "http://scripted", "scripted-model")
	recs := []AnswerRecord{
		{Record: Record{Type: "query", Q: "antarctic flamingo census", Expect: []string{model.Key}}, Answer: model.Body},
	}
	rep, err := RunAnswers(ctx, s, "bench", recs, AnswerOptions{Composer: composer, MaxTokens: 1000})
	if err != nil {
		t.Fatal(err)
	}
	c := answerCase(t, rep, "antarctic flamingo census")
	if len(c.CitedIDs) != 1 || c.CitedIDs[0] != model.ID {
		t.Fatalf("cited IDs = %v, want [%s] (the foreign reflect-shown model)", c.CitedIDs, model.ID)
	}
	if !c.CitationExist {
		t.Fatal("CitationExist = false, want true: existence is scored against the composer's known set")
	}
	if c.Support == nil || *c.Support {
		t.Fatalf("Support = %v, want explicit false: foreign-namespace shown evidence must never resolve", c.Support)
	}
	if !strings.Contains(c.Note, "did not resolve") {
		t.Fatalf("case note = %q, want the unresolved-citation reason", c.Note)
	}
	if c.ExactMatch == nil || !*c.ExactMatch {
		t.Fatalf("ExactMatch = %v, want true (the answer text is right; support is the failed dimension)", c.ExactMatch)
	}
	sum := rep.Summary
	if sum.RetrievalMisses != 1 {
		t.Fatalf("RetrievalMisses = %d, want 1: the gold key lives only in the foreign namespace", sum.RetrievalMisses)
	}
	if sum.SupportRate == nil || *sum.SupportRate != 0.0 {
		t.Fatalf("SupportRate = %v, want 0.0", sum.SupportRate)
	}
}

// TestReflectComposerPartialUnknownUsageCountedPerCall (finding 2): a
// real reflect two-call loop whose FIRST response reports no usage and
// whose second (done) reports 15 tokens must record UsageUnknown=1
// alongside the measured 15. The earlier runner only asked whether the
// whole composition measured zero, so a partly unreported multi-call
// loop looked fully measured. The per-call count now propagates
// reflect Answer -> ComposeResult -> summary, the measured spend stays
// intact, and the pre-call budget gates are unchanged (unknown usage
// never trips the measured gate).
func TestReflectComposerPartialUnknownUsageCountedPerCall(t *testing.T) {
	s := newBenchStore(t)
	ctx := t.Context()
	model, err := s.RememberModel(ctx, "bench", "fleet-posture", "fleet posture: two primaries per region", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	recs := []AnswerRecord{
		{Record: Record{Type: "fact", Key: "/svc/db", Body: "postgres primary runs on host7"}},
		// The query terms match nothing in the corpus: the initial
		// retrieval returns empty, so the mental-model citation is
		// definitionally outside the initial top-k.
		{Record: Record{Type: "query", Q: "antarctic flamingo census", Expect: []string{"/mental-models/fleet-posture"}}, Answer: "fleet posture: two primaries per region"},
	}
	script := &scriptedLLM{t: t}
	script.queue = []func([]llm.Turn) (*llm.Result, error){
		// First call: no usage reported (unknown, not zero).
		func([]llm.Turn) (*llm.Result, error) {
			return &llm.Result{ToolCalls: []llm.ToolCall{{ID: "c1", Name: "list_models", Args: json.RawMessage(`{}`)}}}, nil
		},
		// Second call: done, reporting 15 measured tokens.
		func([]llm.Turn) (*llm.Result, error) {
			args, _ := json.Marshal(map[string]any{
				"answer":    "fleet posture: two primaries per region",
				"citations": []string{model.ID},
			})
			return &llm.Result{ToolCalls: []llm.ToolCall{{ID: "c2", Name: "done", Args: args}}, PromptTokens: 10, CompletionTokens: 5}, nil
		},
	}
	composer := NewReflectComposer(reflect.New(s, script), "bench", "http://scripted", "scripted-model")
	rep, err := RunAnswers(ctx, s, "bench", recs, AnswerOptions{K: 5, Composer: composer, MaxTokens: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if script.calls != 2 {
		t.Fatalf("scripted LLM calls = %d, want 2: unknown usage must not trip the measured pre-call budget gates", script.calls)
	}
	c := answerCase(t, rep, "antarctic flamingo census")
	if len(c.RetrievedIDs) != 0 {
		t.Fatalf("retrieved IDs = %v, want empty: the citation must lie outside the initial top-k", c.RetrievedIDs)
	}
	if len(c.CitedIDs) != 1 || c.CitedIDs[0] != model.ID {
		t.Fatalf("cited IDs = %v, want [%s] (the reflect-shown mental model)", c.CitedIDs, model.ID)
	}
	if !c.CitationExist {
		t.Fatal("CitationExist = false for a citation the composer was actually shown")
	}
	if c.Error != "" {
		t.Fatalf("case error = %q, want none", c.Error)
	}
	if c.Support == nil || !*c.Support {
		t.Fatalf("Support = %v, want true: the shown model evidence resolves from the carried bodies and its key is gold", c.Support)
	}
	if c.ExactMatch == nil || !*c.ExactMatch {
		t.Fatalf("ExactMatch = %v, want true", c.ExactMatch)
	}
	if c.Tokens != 15 {
		t.Fatalf("case Tokens = %d, want 15: the measured second call is preserved alongside the unknown first", c.Tokens)
	}
	sum := rep.Summary
	if sum.RetrievalMisses != 1 {
		t.Fatalf("RetrievalMisses = %d, want 1: the initial retrieval genuinely missed the gold model", sum.RetrievalMisses)
	}
	if sum.SupportRate == nil || *sum.SupportRate != 1.0 {
		t.Fatalf("SupportRate = %v, want 1.0", sum.SupportRate)
	}
	if sum.Tokens != 15 {
		t.Fatalf("summary Tokens = %d, want 15 measured", sum.Tokens)
	}
	if sum.UsageUnknown != 1 {
		t.Fatalf("UsageUnknown = %d, want 1: the first reflect call reported no usage", sum.UsageUnknown)
	}
}
