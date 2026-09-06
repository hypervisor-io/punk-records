package membench

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hypervisor-io/punk-records/internal/llm"
	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/reflect"
)

// Round-2 grading-contract tests (E02): the gold judge's three-way
// support verdict, end-to-end vs conditional denominators, the budget's
// per-call gate and bounded-overshoot contract, usage retained across
// grading failures, unknown usage reported distinctly from zero, and the
// reflect composer resolving the evidence it was actually shown - not
// only what the initial top-k retrieval happened to return.

// TestGoldJudgeSupportVerdictContract pins the deterministic judge's
// three-way support verdict: label-decidable non-support (false),
// deterministically verified support (true), and explicitly ungraded
// semantic divergence (nil, never a pass).
func TestGoldJudgeSupportVerdictContract(t *testing.T) {
	gold := []string{"/db"}
	goldEv := []JudgeEvidence{{ID: "f1", Key: "/db", Body: "primary runs on host7"}}
	cases := []struct {
		name     string
		in       JudgeInput
		want     *bool // nil = explicitly ungraded
		wantNote bool
	}{
		{"unanswerable is never support-graded", JudgeInput{Unanswerable: true, Answer: "x"}, nil, false},
		{"no citations is not support", JudgeInput{Answer: "primary runs on host7", ExpectedAnswer: "primary runs on host7", ExpectKeys: gold}, boolPtr(false), true},
		{"unresolved citation is not support", JudgeInput{Answer: "primary runs on host7", ExpectedAnswer: "primary runs on host7", ExpectKeys: gold, CitedCount: 2, Evidence: goldEv}, boolPtr(false), true},
		{"unrelated real citation is not support", JudgeInput{Answer: "lag threshold is 200ms", ExpectKeys: gold, CitedCount: 1, Evidence: []JudgeEvidence{{ID: "f2", Key: "/lag", Body: "lag threshold is 200ms"}}}, boolPtr(false), true},
		{"mixed relevant and unrelated is not support", JudgeInput{Answer: "primary runs on host7", ExpectedAnswer: "primary runs on host7", ExpectKeys: gold, CitedCount: 2, Evidence: append(append([]JudgeEvidence{}, goldEv...), JudgeEvidence{ID: "f2", Key: "/lag", Body: "lag"})}, boolPtr(false), true},
		{"gold citation plus expected-answer match is support", JudgeInput{Answer: "primary runs on host7", ExpectedAnswer: "primary runs on host7", ExpectKeys: gold, CitedCount: 1, Evidence: goldEv}, boolPtr(true), false},
		{"gold citation plus verbatim cited body is support", JudgeInput{Answer: "primary runs on host7", ExpectKeys: gold, CitedCount: 1, Evidence: goldEv}, boolPtr(true), false},
		{"gold citation with a contradictory answer is ungraded", JudgeInput{Answer: "primary runs on host9", ExpectedAnswer: "primary runs on host7", ExpectKeys: gold, CitedCount: 1, Evidence: goldEv}, nil, true},
		{"gold citation with a paraphrase and no expected answer is ungraded", JudgeInput{Answer: "the primary lives on host7", ExpectKeys: gold, CitedCount: 1, Evidence: goldEv}, nil, true},
	}
	for _, tc := range cases {
		v, err := (goldJudge{}).Judge(t.Context(), tc.in)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		switch {
		case tc.want == nil && v.Support != nil:
			t.Errorf("%s: Support = %v, want explicitly ungraded (nil)", tc.name, *v.Support)
		case tc.want != nil && (v.Support == nil || *v.Support != *tc.want):
			t.Errorf("%s: Support = %v, want %v", tc.name, v.Support, *tc.want)
		}
		if tc.wantNote && v.Justification == "" {
			t.Errorf("%s: no justification recorded for a false/ungraded verdict", tc.name)
		}
	}
}

// TestRunAnswersEndToEndExactDenominator: ExactMatchRate divides by every
// answerable labeled case - an abstention on retrieved gold and a
// retrieval-miss abstention both count as not matched - while
// AnsweredExactMatchRate keeps the conditional answered-only view
// explicitly labeled.
func TestRunAnswersEndToEndExactDenominator(t *testing.T) {
	s := newBenchStore(t)
	recs := []AnswerRecord{
		{Record: Record{Type: "fact", Key: "/svc/db", Body: "postgres primary runs on host7"}},
		{Record: Record{Type: "query", Q: "postgres primary", Expect: []string{"/svc/db"}}, Answer: "postgres primary runs on host7"}, // answered
		{Record: Record{Type: "query", Q: "host7", Expect: []string{"/svc/db"}}, Answer: "postgres primary runs on host7"},            // gold retrieved, composer abstains
		{Record: Record{Type: "query", Q: "zzzq no match", Expect: []string{"/svc/db"}}, Answer: "postgres primary runs on host7"},    // retrieval miss, abstains
	}
	composer := fakeComposer{
		cfg: ComposerConfig{Provider: "scripted"},
		fn: func(_ context.Context, q string, facts []memory.Fact) (ComposeResult, error) {
			if q != "postgres primary" {
				return ComposeResult{Abstained: true}, nil
			}
			db := findFact(t, facts, "/svc/db")
			return ComposeResult{Text: "postgres primary runs on host7", CitedIDs: []string{db.ID}, KnownIDs: []string{db.ID}}, nil
		},
	}
	rep, err := RunAnswers(t.Context(), s, "bench", recs, AnswerOptions{K: 5, Composer: composer})
	if err != nil {
		t.Fatal(err)
	}
	sum := rep.Summary
	if sum.ExactAnswerable != 3 || sum.ExactGraded != 1 {
		t.Fatalf("denominators = %d answerable / %d graded, want 3/1", sum.ExactAnswerable, sum.ExactGraded)
	}
	wantEnd := 1.0 / 3.0
	if sum.ExactMatchRate == nil || *sum.ExactMatchRate != wantEnd {
		t.Fatalf("ExactMatchRate = %v, want %v (end-to-end over every answerable case)", sum.ExactMatchRate, wantEnd)
	}
	if sum.AnsweredExactMatchRate == nil || *sum.AnsweredExactMatchRate != 1.0 {
		t.Fatalf("AnsweredExactMatchRate = %v, want 1.0 (conditional answered-only)", sum.AnsweredExactMatchRate)
	}
	if sum.RetrievalMisses != 1 || sum.AbstainedAnswerable != 2 || sum.AbstainedUnanswerable != 0 {
		t.Fatalf("summary = %+v, want retrieval_misses=1 abstained_answerable=2: a miss is not a correct unanswerable classification", sum)
	}
}

// TestRunAnswersBudgetBoundsOvershootToInflightCall pins the
// bounded-overshoot contract: the budget gates before every model call,
// so a judge call that starts under budget may land over it, and no call
// starts afterwards.
func TestRunAnswersBudgetBoundsOvershootToInflightCall(t *testing.T) {
	s := newBenchStore(t)
	recs := []AnswerRecord{
		{Record: Record{Type: "fact", Key: "/svc/db", Body: "postgres primary runs on host7"}},
		{Record: Record{Type: "query", Q: "postgres primary", Expect: []string{"/svc/db"}}, Answer: "postgres primary runs on host7"},
		{Record: Record{Type: "query", Q: "postgres host7", Expect: []string{"/svc/db"}}, Answer: "postgres primary runs on host7"},
	}
	composer := fakeComposer{
		cfg: ComposerConfig{Provider: "scripted-model", ModelBacked: true, Model: "m"},
		fn: func(_ context.Context, _ string, facts []memory.Fact) (ComposeResult, error) {
			db := findFact(t, facts, "/svc/db")
			return ComposeResult{Text: "postgres primary runs on host7", CitedIDs: []string{db.ID}, KnownIDs: []string{db.ID}, Tokens: 6}, nil
		},
	}
	judgeCalls := 0
	judge := fakeJudge{
		cfg: JudgeConfig{Provider: "scripted-judge", ModelBacked: true, Model: "m", GradesCorrectness: true, GradesSupport: true},
		fn: func(context.Context, JudgeInput) (JudgeVerdict, error) {
			judgeCalls++
			return JudgeVerdict{Correct: boolPtr(true), Support: boolPtr(true), PromptTokens: 40}, nil
		},
	}
	// Budget 10: case 1 composes (6) and judges (6<10 gate, lands 46:
	// the in-flight call's overshoot); case 2 never composes (46>=10).
	rep, err := RunAnswers(t.Context(), s, "bench", recs, AnswerOptions{K: 5, Composer: composer, Judge: judge, MaxTokens: 10})
	if err != nil {
		t.Fatal(err)
	}
	if judgeCalls != 1 {
		t.Fatalf("judge called %d times, want 1 (gated after the first case's spend)", judgeCalls)
	}
	first, second := rep.Cases[0], rep.Cases[1]
	if first.Error != "" || first.JudgeCorrect == nil || !*first.JudgeCorrect {
		t.Fatalf("first case = %+v, want composed and judged within the gate", first)
	}
	if !strings.Contains(second.Error, "budget") || second.Answer != "" {
		t.Fatalf("second case = %+v, want an explicit budget error and no composition", second)
	}
	sum := rep.Summary
	if sum.Tokens != 6 || sum.JudgeTokens != 40 {
		t.Fatalf("tokens = %d/%d, want 6/40: spend stops after the in-flight overshoot", sum.Tokens, sum.JudgeTokens)
	}
	if sum.Failed != 1 {
		t.Fatalf("Failed = %d, want 1 (the budget-blocked case is counted, not dropped)", sum.Failed)
	}
}

// TestRunAnswersJudgeFailureRetainsUsageAndRates: a judge call whose
// response fails grading keeps its consumed usage in the per-case and
// summary token fields, is counted as an unavailable grading
// (JudgeErrors + Failed + per-case reason), and depresses the judge
// rates it was responsible for instead of vanishing from their
// denominators - while the deterministic exact grading it preceded
// stands.
func TestRunAnswersJudgeFailureRetainsUsageAndRates(t *testing.T) {
	s := newBenchStore(t)
	recs := []AnswerRecord{
		{Record: Record{Type: "fact", Key: "/svc/db", Body: "postgres primary runs on host7"}},
		{Record: Record{Type: "query", Q: "postgres primary", Expect: []string{"/svc/db"}}, Answer: "postgres primary runs on host7"},
		{Record: Record{Type: "query", Q: "postgres host7", Expect: []string{"/svc/db"}}, Answer: "postgres primary runs on host7"},
	}
	composer := fakeComposer{
		cfg: ComposerConfig{Provider: "scripted"},
		fn: func(_ context.Context, _ string, facts []memory.Fact) (ComposeResult, error) {
			db := findFact(t, facts, "/svc/db")
			return ComposeResult{Text: "postgres primary runs on host7", CitedIDs: []string{db.ID}, KnownIDs: []string{db.ID}}, nil
		},
	}
	judge := fakeJudge{
		cfg: JudgeConfig{Provider: "scripted-judge", ModelBacked: true, Model: "m", GradesCorrectness: true, GradesSupport: true},
		fn: func(_ context.Context, in JudgeInput) (JudgeVerdict, error) {
			if in.Q == "postgres host7" {
				// Malformed grading response, but the call consumed 7 tokens.
				return JudgeVerdict{PromptTokens: 7}, errors.New("verdict is not verdict JSON")
			}
			return JudgeVerdict{Correct: boolPtr(true), Support: boolPtr(true), PromptTokens: 11}, nil
		},
	}
	rep, err := RunAnswers(t.Context(), s, "bench", recs, AnswerOptions{K: 5, Composer: composer, Judge: judge, MaxTokens: 1000})
	if err != nil {
		t.Fatal(err)
	}
	failed := answerCase(t, rep, "postgres host7")
	if !strings.Contains(failed.Error, "judge:") {
		t.Fatalf("failed case error = %q, want a per-case judge reason", failed.Error)
	}
	if failed.JudgeTokens != 7 {
		t.Fatalf("failed case JudgeTokens = %d, want 7 retained from the malformed response", failed.JudgeTokens)
	}
	if failed.ExactMatch == nil || !*failed.ExactMatch {
		t.Fatalf("failed case ExactMatch = %v, want true: the judge failure does not erase deterministic grading", failed.ExactMatch)
	}
	if failed.Support != nil || failed.JudgeCorrect != nil {
		t.Fatalf("failed case = %+v, want no verdicts from a failed judge call - never a successful null grade", failed)
	}
	sum := rep.Summary
	if sum.JudgeTokens != 18 || sum.Tokens != 0 {
		t.Fatalf("tokens = composer %d / judge %d, want 0/18 (failed-call usage retained)", sum.Tokens, sum.JudgeTokens)
	}
	if sum.Failed != 1 || sum.JudgeErrors != 1 {
		t.Fatalf("Failed = %d JudgeErrors = %d, want 1/1", sum.Failed, sum.JudgeErrors)
	}
	if sum.JudgeCorrectRate == nil || *sum.JudgeCorrectRate != 0.5 {
		t.Fatalf("JudgeCorrectRate = %v, want 0.5: the unavailable grading stays in the denominator", sum.JudgeCorrectRate)
	}
	if sum.SupportRate == nil || *sum.SupportRate != 0.5 {
		t.Fatalf("SupportRate = %v, want 0.5: the unavailable grading stays in the denominator", sum.SupportRate)
	}
	if sum.ExactMatchRate == nil || *sum.ExactMatchRate != 1.0 {
		t.Fatalf("ExactMatchRate = %v, want 1.0 (both answers composed and exact)", sum.ExactMatchRate)
	}
}

// TestRunAnswersUnknownUsageReportedDistinctly: a model-backed call whose
// endpoint reports no usage is unknown usage, not zero usage - counted
// in UsageUnknown so an under-measured budget is visible, while measured
// spend stays 0 and the (measured) budget is not tripped.
func TestRunAnswersUnknownUsageReportedDistinctly(t *testing.T) {
	s := newBenchStore(t)
	recs := []AnswerRecord{
		{Record: Record{Type: "fact", Key: "/svc/db", Body: "postgres primary runs on host7"}},
		{Record: Record{Type: "query", Q: "postgres primary", Expect: []string{"/svc/db"}}, Answer: "postgres primary runs on host7"},
	}
	composer := fakeComposer{
		cfg: ComposerConfig{Provider: "scripted-model", ModelBacked: true, Model: "m"},
		fn: func(_ context.Context, _ string, facts []memory.Fact) (ComposeResult, error) {
			db := findFact(t, facts, "/svc/db")
			return ComposeResult{Text: "postgres primary runs on host7", CitedIDs: []string{db.ID}, KnownIDs: []string{db.ID}}, nil // no usage reported
		},
	}
	judgeCalled := 0
	judge := fakeJudge{
		cfg: JudgeConfig{Provider: "scripted-judge", ModelBacked: true, Model: "m", GradesCorrectness: true, GradesSupport: true},
		fn: func(context.Context, JudgeInput) (JudgeVerdict, error) {
			judgeCalled++
			return JudgeVerdict{Correct: boolPtr(true), Support: boolPtr(true)}, nil // no usage reported
		},
	}
	rep, err := RunAnswers(t.Context(), s, "bench", recs, AnswerOptions{K: 5, Composer: composer, Judge: judge, MaxTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	sum := rep.Summary
	if sum.UsageUnknown != 2 {
		t.Fatalf("UsageUnknown = %d, want 2 (composer + judge calls reported no usage)", sum.UsageUnknown)
	}
	if sum.Tokens != 0 || sum.JudgeTokens != 0 {
		t.Fatalf("tokens = %d/%d, want 0/0 measured (unknown is not measured spend)", sum.Tokens, sum.JudgeTokens)
	}
	if judgeCalled != 1 {
		t.Fatalf("judge called %d times, want 1: unknown usage must not trip the measured budget gate", judgeCalled)
	}
	c := answerCase(t, rep, "postgres primary")
	if c.Error != "" || c.Support == nil || !*c.Support {
		t.Fatalf("case = %+v, want graded normally despite unreported usage", c)
	}
}

// scriptedLLM is a fixed llm.Client double replaying queued responses in
// order (modeled on internal/reflect's scriptedLLM): the reflect
// composer tests drive a real reflect.Engine over the bench store with
// no network and no paid endpoint.
type scriptedLLM struct {
	t     *testing.T
	queue []func(turns []llm.Turn) (*llm.Result, error)
	calls int
}

func (s *scriptedLLM) Chat(_ context.Context, turns []llm.Turn, _ []llm.Tool) (*llm.Result, error) {
	if s.calls >= len(s.queue) {
		s.t.Fatalf("scripted LLM exhausted after %d calls", s.calls)
	}
	fn := s.queue[s.calls]
	s.calls++
	return fn(turns)
}
func (s *scriptedLLM) Model() string { return "scripted" }

func scriptedCall(id, name string, args json.RawMessage) func([]llm.Turn) (*llm.Result, error) {
	return func([]llm.Turn) (*llm.Result, error) {
		return &llm.Result{ToolCalls: []llm.ToolCall{{ID: id, Name: name, Args: args}}, PromptTokens: 10, CompletionTokens: 5}, nil
	}
}

// TestReflectComposerResolvesCitationsOutsideInitialTopK (finding 5): the
// reflect composer gathers its own hierarchical evidence, so a citation
// can name a mental model the answer stage's initial HybridSearch never
// returned. The runner must resolve that actually-shown evidence against
// the isolated namespace and let the gold judge grade it - never mark a
// valid higher-layer citation unsupported because the initial top-k
// missed it.
func TestReflectComposerResolvesCitationsOutsideInitialTopK(t *testing.T) {
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
		t.Fatalf("Support = %v, want true: the shown model evidence resolves in the isolated namespace and its key is gold", c.Support)
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

// TestRunAnswersReflectComposerBudgetStopsLoop: the stage's remaining
// budget is passed into the reflect loop, which checks before EVERY one
// of its model calls - the loop stops with the budget error before the
// next call, and the usage consumed so far is retained on the failed
// case instead of vanishing with the error.
func TestRunAnswersReflectComposerBudgetStopsLoop(t *testing.T) {
	s := newBenchStore(t)
	recs := []AnswerRecord{
		{Record: Record{Type: "fact", Key: "/svc/db", Body: "postgres primary runs on host7"}},
		{Record: Record{Type: "query", Q: "postgres primary", Expect: []string{"/svc/db"}}, Answer: "postgres primary runs on host7"},
	}
	script := &scriptedLLM{t: t}
	script.queue = []func([]llm.Turn) (*llm.Result, error){
		scriptedCall("c1", "recall", json.RawMessage(`{"query":"postgres"}`)),
		scriptedCall("c2", "recall", json.RawMessage(`{"query":"host7"}`)),
		// No third entry: the budget gate must stop the loop before it.
	}
	composer := NewReflectComposer(reflect.New(s, script), "bench", "http://scripted", "scripted-model")
	// Budget 20: call 1 lands 15 (15<20 gate passes), call 2 lands 30
	// (bounded overshoot), the loop's pre-call gate stops call 3.
	rep, err := RunAnswers(t.Context(), s, "bench", recs, AnswerOptions{K: 5, Composer: composer, MaxTokens: 20})
	if err != nil {
		t.Fatal(err)
	}
	if script.calls != 2 {
		t.Fatalf("scripted LLM calls = %d, want 2: the loop's per-call budget gate stopped the third call", script.calls)
	}
	c := answerCase(t, rep, "postgres primary")
	if !strings.Contains(c.Error, "budget") {
		t.Fatalf("case error = %q, want the reflect budget stop recorded", c.Error)
	}
	if c.Tokens != 30 {
		t.Fatalf("case Tokens = %d, want 30: usage consumed before the budget stop is retained", c.Tokens)
	}
	sum := rep.Summary
	if sum.Tokens != 30 || sum.Failed != 1 {
		t.Fatalf("summary tokens = %d failed = %d, want 30/1", sum.Tokens, sum.Failed)
	}
	if rep.BudgetTokens != 20 {
		t.Fatalf("BudgetTokens = %d, want the configured 20 recorded", rep.BudgetTokens)
	}
}
