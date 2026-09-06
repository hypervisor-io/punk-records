package membench

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hypervisor-io/punk-records/internal/llm"
	"github.com/hypervisor-io/punk-records/internal/memory"
)

// Tests for the E02 answer stage (answers.go): exact/structured grading,
// the pluggable judge, citation EXISTENCE scored separately from claim
// SUPPORT, and abstention accounting that keeps unanswerable examples
// distinct from retrieval misses. Every test double here is fixed and
// offline; the only model-backed path exercised is the OpenAI-compatible
// judge against an httptest server with explicit endpoint/model/budget.

// fakeComposer is a fixed composer double: the response is a function of
// the query and the actually-retrieved facts, so tests can cite real fact
// IDs without knowing them upfront.
type fakeComposer struct {
	cfg ComposerConfig
	fn  func(context.Context, string, []memory.Fact) (ComposeResult, error)
}

func (f fakeComposer) Compose(ctx context.Context, q string, facts []memory.Fact) (ComposeResult, error) {
	return f.fn(ctx, q, facts)
}
func (f fakeComposer) ComposerConfig() ComposerConfig { return f.cfg }

// fakeJudge is a fixed judge double.
type fakeJudge struct {
	cfg JudgeConfig
	fn  func(context.Context, JudgeInput) (JudgeVerdict, error)
}

func (f fakeJudge) Judge(ctx context.Context, in JudgeInput) (JudgeVerdict, error) {
	return f.fn(ctx, in)
}
func (f fakeJudge) JudgeConfig() JudgeConfig { return f.cfg }

// findFact locates a retrieved fact by key so a scripted composer can
// cite its real (run-generated) ID.
func findFact(t *testing.T, facts []memory.Fact, key string) memory.Fact {
	t.Helper()
	for _, f := range facts {
		if f.Key == key {
			return f
		}
	}
	t.Fatalf("fact %q not among retrieved facts", key)
	return memory.Fact{}
}

func answerCase(t *testing.T, rep AnswerReport, q string) AnswerCaseResult {
	t.Helper()
	for _, c := range rep.Cases {
		if c.Q == q {
			return c
		}
	}
	t.Fatalf("case %q missing from the answer report", q)
	return AnswerCaseResult{}
}

// TestRunAnswersUnrelatedCitationPassesExistenceFailsSupport is the E02
// red proof, first half: an answer citing a REAL but UNRELATED retrieved
// fact must pass citation ID-existence (the ID genuinely exists among the
// retrieved/known facts) and FAIL claim support (the cited evidence does
// not support the claim: under gold grading the cited key is outside the
// query's gold evidence set).
func TestRunAnswersUnrelatedCitationPassesExistenceFailsSupport(t *testing.T) {
	s := newBenchStore(t)
	recs := []AnswerRecord{
		{Record: Record{Type: "fact", Key: "/svc/db", Body: "postgres primary runs on host7"}},
		{Record: Record{Type: "fact", Key: "/svc/lag", Body: "primary replica lag alert threshold is 200ms"}},
		{Record: Record{Type: "query", Q: "primary", Expect: []string{"/svc/db"}}, Answer: "postgres primary runs on host7"},
	}
	// The composer answers the "primary" question with the replica-lag
	// claim and cites the real (retrieved) but unrelated lag fact.
	composer := fakeComposer{
		cfg: ComposerConfig{Provider: "scripted"},
		fn: func(_ context.Context, _ string, facts []memory.Fact) (ComposeResult, error) {
			lag := findFact(t, facts, "/svc/lag")
			ids := make([]string, 0, len(facts))
			for _, f := range facts {
				ids = append(ids, f.ID)
			}
			return ComposeResult{Text: "primary replica lag alert threshold is 200ms", CitedIDs: []string{lag.ID}, KnownIDs: ids}, nil
		},
	}
	rep, err := RunAnswers(t.Context(), s, "bench", recs, AnswerOptions{K: 5, Composer: composer})
	if err != nil {
		t.Fatal(err)
	}
	c := answerCase(t, rep, "primary")
	if !c.CitationExist {
		t.Fatalf("CitationExist = false, want true: the cited ID %v is a real retrieved fact", c.CitedIDs)
	}
	if c.Support == nil || *c.Support {
		t.Fatalf("Support = %v, want explicit false: the cited fact is real but unrelated to the gold claim", c.Support)
	}
	if c.ExactMatch == nil || *c.ExactMatch {
		t.Fatalf("ExactMatch = %v, want explicit false (the answer is the unrelated claim)", c.ExactMatch)
	}
	sum := rep.Summary
	if sum.CitationExistenceRate == nil || *sum.CitationExistenceRate != 1.0 {
		t.Fatalf("CitationExistenceRate = %v, want 1.0 (existence passed)", sum.CitationExistenceRate)
	}
	if sum.SupportRate == nil || *sum.SupportRate != 0.0 {
		t.Fatalf("SupportRate = %v, want 0.0 (support failed)", sum.SupportRate)
	}
}

// TestRunAnswersConfidentUnanswerableFailsAbstention is the E02 red
// proof, second half: an UNANSWERABLE question (empty gold set) answered
// confidently must fail abstention - counted as ConfidentUnanswerable,
// never graded as a correct answer, and kept out of the answerable
// denominators.
func TestRunAnswersConfidentUnanswerableFailsAbstention(t *testing.T) {
	s := newBenchStore(t)
	recs := []AnswerRecord{
		{Record: Record{Type: "fact", Key: "/svc/db", Body: "postgres primary runs on host7"}},
		{Record: Record{Type: "query", Q: "quantum flux capacitor calibration"}}, // no expect: unanswerable
	}
	composer := fakeComposer{
		cfg: ComposerConfig{Provider: "scripted"},
		fn: func(_ context.Context, _ string, _ []memory.Fact) (ComposeResult, error) {
			return ComposeResult{Text: "the flux capacitor calibrates at 88 mph"}, nil
		},
	}
	rep, err := RunAnswers(t.Context(), s, "bench", recs, AnswerOptions{K: 5, Composer: composer})
	if err != nil {
		t.Fatal(err)
	}
	c := answerCase(t, rep, "quantum flux capacitor calibration")
	if c.Answer == "" || c.Abstained {
		t.Fatalf("case = %+v, want a confident non-abstained answer (the fixture violation)", c)
	}
	if !c.Unanswerable {
		t.Fatal("Unanswerable = false, want true: empty gold set")
	}
	if c.Support != nil || c.ExactMatch != nil || c.JudgeCorrect != nil {
		t.Fatalf("case = %+v, want no answer grading on an unanswerable question: abstention is the graded signal", c)
	}
	sum := rep.Summary
	if sum.Unanswerable != 1 || sum.ConfidentUnanswerable != 1 || sum.AbstainedUnanswerable != 0 {
		t.Fatalf("summary = %+v, want unanswerable=1 confident_unanswerable=1 abstained_unanswerable=0", sum)
	}
	if sum.Answerable != 0 || sum.Answered != 0 {
		t.Fatalf("summary = %+v, want answerable=0 answered=0: a confident non-answer is not an answer", sum)
	}
}

// TestRunAnswersAbstentionOnUnanswerableCountsCorrect is the red-proof
// control: the default extractive composer abstains when retrieval finds
// nothing, and the abstention is counted as the CORRECT behavior on an
// unanswerable question.
func TestRunAnswersAbstentionOnUnanswerableCountsCorrect(t *testing.T) {
	s := newBenchStore(t)
	recs := []AnswerRecord{
		{Record: Record{Type: "fact", Key: "/svc/db", Body: "postgres primary runs on host7"}},
		{Record: Record{Type: "query", Q: "quantum flux capacitor calibration"}},
	}
	rep, err := RunAnswers(t.Context(), s, "bench", recs, AnswerOptions{K: 5}) // default extractive composer + gold judge
	if err != nil {
		t.Fatal(err)
	}
	c := answerCase(t, rep, "quantum flux capacitor calibration")
	if !c.Abstained {
		t.Fatalf("Abstained = false, want true: nothing retrieved, nothing to claim")
	}
	sum := rep.Summary
	if sum.AbstainedUnanswerable != 1 || sum.ConfidentUnanswerable != 0 {
		t.Fatalf("summary = %+v, want abstained_unanswerable=1 confident_unanswerable=0", sum)
	}
}

// TestRunAnswersExactAndStructuredGrading pins the deterministic grader:
// normalized exact match (case/whitespace-insensitive) and structured
// JSON match (key order-insensitive), each recorded per case.
func TestRunAnswersExactAndStructuredGrading(t *testing.T) {
	s := newBenchStore(t)
	recs := []AnswerRecord{
		{Record: Record{Type: "fact", Key: "/svc/db", Body: "postgres primary runs on host7"}},
		{Record: Record{Type: "query", Q: "postgres primary", Expect: []string{"/svc/db"}}, Answer: "Postgres   Primary runs on host7"},
		{Record: Record{Type: "query", Q: "postgres host7", Expect: []string{"/svc/db"}}, Answer: `{"host":"host7","role":"primary"}`},
	}
	// Composer 1: exact answer with different case/extra whitespace.
	// Composer 2: structured JSON answer with reordered keys.
	composer := fakeComposer{
		cfg: ComposerConfig{Provider: "scripted"},
		fn: func(_ context.Context, q string, facts []memory.Fact) (ComposeResult, error) {
			db := findFact(t, facts, "/svc/db")
			if q == "postgres primary" {
				return ComposeResult{Text: "postgres primary runs on host7", CitedIDs: []string{db.ID}, KnownIDs: []string{db.ID}}, nil
			}
			return ComposeResult{Text: `{"role":"primary","host":"host7"}`, CitedIDs: []string{db.ID}, KnownIDs: []string{db.ID}}, nil
		},
	}
	rep, err := RunAnswers(t.Context(), s, "bench", recs, AnswerOptions{K: 5, Composer: composer})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"postgres primary", "postgres host7"} {
		c := answerCase(t, rep, q)
		if c.ExactMatch == nil || !*c.ExactMatch {
			t.Fatalf("%q: ExactMatch = %v, want explicit true", q, c.ExactMatch)
		}
	}
	sum := rep.Summary
	if sum.ExactMatchRate == nil || *sum.ExactMatchRate != 1.0 {
		t.Fatalf("ExactMatchRate = %v, want 1.0", sum.ExactMatchRate)
	}
	if sum.SupportRate == nil || *sum.SupportRate != 1.0 {
		t.Fatalf("SupportRate = %v, want 1.0 (gold citations support gold claims)", sum.SupportRate)
	}
}

// TestRunAnswersUncitedAnswerFailsSupport: an answered answerable
// question with NO citations has claims without cited evidence: support
// must fail explicitly, while the existence rate (defined over answers
// with >=1 citation) stays undefined rather than vacuously perfect.
func TestRunAnswersUncitedAnswerFailsSupport(t *testing.T) {
	s := newBenchStore(t)
	recs := []AnswerRecord{
		{Record: Record{Type: "fact", Key: "/svc/db", Body: "postgres primary runs on host7"}},
		{Record: Record{Type: "query", Q: "postgres primary", Expect: []string{"/svc/db"}}, Answer: "postgres primary runs on host7"},
	}
	composer := fakeComposer{
		cfg: ComposerConfig{Provider: "scripted"},
		fn: func(_ context.Context, _ string, _ []memory.Fact) (ComposeResult, error) {
			return ComposeResult{Text: "postgres primary runs on host7"}, nil // right answer, zero citations
		},
	}
	rep, err := RunAnswers(t.Context(), s, "bench", recs, AnswerOptions{K: 5, Composer: composer})
	if err != nil {
		t.Fatal(err)
	}
	c := answerCase(t, rep, "postgres primary")
	if c.ExactMatch == nil || !*c.ExactMatch {
		t.Fatalf("ExactMatch = %v, want true (the answer text is right)", c.ExactMatch)
	}
	if c.Support == nil || *c.Support {
		t.Fatalf("Support = %v, want explicit false: zero citations means zero cited evidence", c.Support)
	}
	sum := rep.Summary
	if sum.CitationExistenceRate != nil {
		t.Fatalf("CitationExistenceRate = %v, want nil (no answer carried a citation)", *sum.CitationExistenceRate)
	}
	if sum.SupportRate == nil || *sum.SupportRate != 0.0 {
		t.Fatalf("SupportRate = %v, want 0.0", sum.SupportRate)
	}
}

// TestRunAnswersFabricatedCitationFailsExistence: a cited ID that was
// never retrieved and never known to the composer fails existence - the
// answer-stage analogue of reflect's citation validation.
func TestRunAnswersFabricatedCitationFailsExistence(t *testing.T) {
	s := newBenchStore(t)
	recs := []AnswerRecord{
		{Record: Record{Type: "fact", Key: "/svc/db", Body: "postgres primary runs on host7"}},
		{Record: Record{Type: "query", Q: "postgres primary", Expect: []string{"/svc/db"}}, Answer: "postgres primary runs on host7"},
	}
	composer := fakeComposer{
		cfg: ComposerConfig{Provider: "scripted"},
		fn: func(_ context.Context, _ string, _ []memory.Fact) (ComposeResult, error) {
			return ComposeResult{Text: "postgres primary runs on host7", CitedIDs: []string{"00000000-fabricated-id"}}, nil
		},
	}
	rep, err := RunAnswers(t.Context(), s, "bench", recs, AnswerOptions{K: 5, Composer: composer})
	if err != nil {
		t.Fatal(err)
	}
	c := answerCase(t, rep, "postgres primary")
	if c.CitationExist {
		t.Fatal("CitationExist = true for a fabricated ID")
	}
	if c.Support == nil || *c.Support {
		t.Fatalf("Support = %v, want explicit false (an unresolvable citation is not evidence)", c.Support)
	}
	sum := rep.Summary
	if sum.CitationExistenceRate == nil || *sum.CitationExistenceRate != 0.0 {
		t.Fatalf("CitationExistenceRate = %v, want 0.0", sum.CitationExistenceRate)
	}
}

// TestRunAnswersRetrievalMissDistinctFromUnanswerable: an answerable
// question whose gold evidence was not retrieved is a RETRIEVAL MISS, not
// an unanswerable case; the summary must keep the two categories in
// separate counts.
func TestRunAnswersRetrievalMissDistinctFromUnanswerable(t *testing.T) {
	s := newBenchStore(t)
	recs := []AnswerRecord{
		{Record: Record{Type: "fact", Key: "/svc/db", Body: "postgres primary runs on host7"}},
		{Record: Record{Type: "query", Q: "flamingo census antarctica", Expect: []string{"/svc/db"}}, Answer: "postgres primary runs on host7"}, // answerable, gold unretrievable
		{Record: Record{Type: "query", Q: "quantum flux capacitor calibration"}},                                                                // unanswerable
	}
	rep, err := RunAnswers(t.Context(), s, "bench", recs, AnswerOptions{K: 5}) // extractive: abstains on the empty retrieval
	if err != nil {
		t.Fatal(err)
	}
	sum := rep.Summary
	if sum.Answerable != 1 || sum.Unanswerable != 1 || sum.RetrievalMisses != 1 {
		t.Fatalf("summary = %+v, want answerable=1 unanswerable=1 retrieval_misses=1", sum)
	}
	miss := answerCase(t, rep, "flamingo census antarctica")
	if miss.Unanswerable {
		t.Fatal("retrieval miss marked unanswerable: the categories must stay distinct")
	}
	if !miss.Abstained {
		t.Fatal("extractive composer should abstain on an empty retrieval")
	}
	if sum.AbstainedAnswerable != 1 {
		t.Fatalf("AbstainedAnswerable = %d, want 1 (abstention on an answerable-but-missed query is recorded separately)", sum.AbstainedAnswerable)
	}
}

// TestRunAnswersJudgeVerdictAndTokensRecorded: a pluggable judge's
// correctness/support verdicts land on the case, its token usage is
// summed separately from composer tokens, and its configuration is saved
// in the report.
func TestRunAnswersJudgeVerdictAndTokensRecorded(t *testing.T) {
	s := newBenchStore(t)
	recs := []AnswerRecord{
		{Record: Record{Type: "fact", Key: "/svc/db", Body: "postgres primary runs on host7"}},
		{Record: Record{Type: "query", Q: "postgres primary", Expect: []string{"/svc/db"}}, Answer: "postgres primary runs on host7"},
	}
	composer := fakeComposer{
		cfg: ComposerConfig{Provider: "scripted"},
		fn: func(_ context.Context, _ string, facts []memory.Fact) (ComposeResult, error) {
			db := findFact(t, facts, "/svc/db")
			return ComposeResult{Text: "postgres primary runs on host7", CitedIDs: []string{db.ID}, KnownIDs: []string{db.ID}, Tokens: 7}, nil
		},
	}
	judge := fakeJudge{
		cfg: JudgeConfig{Provider: "scripted"},
		fn: func(_ context.Context, in JudgeInput) (JudgeVerdict, error) {
			if len(in.Evidence) != 1 || in.Evidence[0].Key != "/svc/db" {
				t.Fatalf("judge evidence = %+v, want the cited gold fact resolved with its key", in.Evidence)
			}
			return JudgeVerdict{Correct: boolPtr(true), Support: boolPtr(true), Justification: "exact", PromptTokens: 30, CompletionTokens: 10}, nil
		},
	}
	rep, err := RunAnswers(t.Context(), s, "bench", recs, AnswerOptions{K: 5, Composer: composer, Judge: judge, MaxTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	c := answerCase(t, rep, "postgres primary")
	if c.JudgeCorrect == nil || !*c.JudgeCorrect {
		t.Fatalf("JudgeCorrect = %v, want explicit true", c.JudgeCorrect)
	}
	if c.Support == nil || !*c.Support {
		t.Fatalf("Support = %v, want explicit true (judge verdict overrides the gold default)", c.Support)
	}
	if c.Tokens != 7 || c.JudgeTokens != 40 {
		t.Fatalf("tokens = %d judge %d, want 7/40", c.Tokens, c.JudgeTokens)
	}
	sum := rep.Summary
	if sum.JudgeCorrectRate == nil || *sum.JudgeCorrectRate != 1.0 {
		t.Fatalf("JudgeCorrectRate = %v, want 1.0", sum.JudgeCorrectRate)
	}
	if sum.Tokens != 7 || sum.JudgeTokens != 40 {
		t.Fatalf("summary tokens = %d judge %d, want 7/40 (cost kept separate from accuracy/evidence)", sum.Tokens, sum.JudgeTokens)
	}
	if rep.Judge.Provider != "scripted" {
		t.Fatalf("report judge config = %+v, want provider scripted", rep.Judge)
	}
}

// TestRunAnswersModelBackedRequiresBudget: wiring a model-backed composer
// or judge without an explicit token budget is a configuration error,
// never a silent unbounded spend.
func TestRunAnswersModelBackedRequiresBudget(t *testing.T) {
	s := newBenchStore(t)
	recs := []AnswerRecord{
		{Record: Record{Type: "query", Q: "anything", Expect: []string{"/x"}}, Answer: "x"},
	}
	composer := fakeComposer{
		cfg: ComposerConfig{Provider: "scripted-model", ModelBacked: true, Model: "m", Endpoint: "http://test"},
		fn: func(context.Context, string, []memory.Fact) (ComposeResult, error) {
			return ComposeResult{Text: "x"}, nil
		},
	}
	_, err := RunAnswers(t.Context(), s, "bench", recs, AnswerOptions{K: 5, Composer: composer})
	if err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("err = %v, want an explicit-budget-required error", err)
	}
	judge := fakeJudge{
		cfg: JudgeConfig{Provider: "scripted-model", ModelBacked: true, Model: "m"},
		fn: func(context.Context, JudgeInput) (JudgeVerdict, error) {
			return JudgeVerdict{}, nil
		},
	}
	_, err = RunAnswers(t.Context(), s, "bench", recs, AnswerOptions{K: 5, Judge: judge})
	if err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("err = %v, want an explicit-budget-required error for a model-backed judge", err)
	}
}

// TestRunAnswersBudgetExhaustion: once the configured token budget is
// spent, remaining cases must be recorded with an explicit budget error
// and counted in Failed - never silently dropped, never silently over
// budget.
func TestRunAnswersBudgetExhaustion(t *testing.T) {
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
	rep, err := RunAnswers(t.Context(), s, "bench", recs, AnswerOptions{K: 5, Composer: composer, MaxTokens: 6})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Cases) != 2 {
		t.Fatalf("len(Cases) = %d, want 2 (budget-exhausted cases are recorded, not dropped)", len(rep.Cases))
	}
	first, second := rep.Cases[0], rep.Cases[1]
	if first.Error != "" || first.Answer == "" {
		t.Fatalf("first case = %+v, want composed within budget", first)
	}
	if !strings.Contains(second.Error, "budget") {
		t.Fatalf("second case error = %q, want an explicit budget-exhaustion error", second.Error)
	}
	if rep.Summary.Failed != 1 {
		t.Fatalf("Failed = %d, want 1", rep.Summary.Failed)
	}
	if rep.Summary.Tokens != 6 {
		t.Fatalf("Tokens = %d, want 6 (spend stops at the budget)", rep.Summary.Tokens)
	}
	if rep.BudgetTokens != 6 {
		t.Fatalf("BudgetTokens = %d, want the configured 6 recorded", rep.BudgetTokens)
	}
}

// TestSuiteWithAnswersIntegratesAndStableStripsCost: the answer stage
// attaches to the versioned report as Report.Answers without disturbing
// the retrieval runs; measured token cost is present in the full report
// but stripped by Stable (measured cost is volatile for stable-manifest
// comparisons), and two identical suite runs stay byte-identical.
func TestSuiteWithAnswersIntegratesAndStableStripsCost(t *testing.T) {
	arecs := []AnswerRecord{
		{Record: Record{Type: "fact", Key: "/svc/db", Body: "postgres primary runs on host7"}},
		{Record: Record{Type: "query", Q: "postgres primary", Expect: []string{"/svc/db"}}, Answer: "postgres primary runs on host7"},
	}
	composer := func() fakeComposer {
		return fakeComposer{
			cfg: ComposerConfig{Provider: "scripted"},
			fn: func(_ context.Context, _ string, facts []memory.Fact) (ComposeResult, error) {
				db := findFact(t, facts, "/svc/db")
				return ComposeResult{Text: "postgres primary runs on host7", CitedIDs: []string{db.ID}, KnownIDs: []string{db.ID}, Tokens: 3}, nil
			},
		}
	}
	opts := func() SuiteOptions {
		o := suiteOpts()
		o.Answers = &AnswerOptions{K: 5, Composer: composer()}
		return o
	}
	rep1, err := SuiteWithAnswers(t.Context(), newBenchStore(t), "bench", arecs, opts())
	if err != nil {
		t.Fatal(err)
	}
	rep2, err := SuiteWithAnswers(t.Context(), newBenchStore(t), "bench", arecs, opts())
	if err != nil {
		t.Fatal(err)
	}
	if rep1.Answers == nil {
		t.Fatal("Report.Answers = nil with the answer stage configured")
	}
	if rep1.Answers.Summary.Tokens != 3 {
		t.Fatalf("Answers.Summary.Tokens = %d, want 3 (measured cost reported)", rep1.Answers.Summary.Tokens)
	}
	stripped := rep1.Stable()
	if stripped.Answers == nil || stripped.Answers.Summary.Tokens != 0 || stripped.Answers.Cases[0].Tokens != 0 {
		t.Fatalf("Stable answers = %+v, want token cost stripped (volatile), structure preserved", stripped.Answers)
	}
	a, err := rep1.StableJSON()
	if err != nil {
		t.Fatal(err)
	}
	b, err := rep2.StableJSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatalf("stable report with answers differs across identical runs:\n%s\n--- vs ---\n%s", a, b)
	}
	// Retrieval runs are untouched by the answer stage.
	base := findRun(t, rep1, "baseline")
	if base.Summary == nil || base.Summary.HitAtK != 1.0 {
		t.Fatalf("baseline summary = %+v, want hit_at_k=1.0 (retrieval unaffected)", base.Summary)
	}
}

// TestSuiteWithoutAnswersKeepsReportShape: without AnswerOptions the
// report carries no answers section at all, so the E01 committed-artifact
// comparison and consumers of the v1 schema are undisturbed.
func TestSuiteWithoutAnswersKeepsReportShape(t *testing.T) {
	rep, err := Suite(t.Context(), newBenchStore(t), "bench", suiteFixtureRecs(t), suiteOpts())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Answers != nil {
		t.Fatalf("Answers = %+v, want nil when the answer stage is not configured", rep.Answers)
	}
	raw, err := rep.ResultOnlyJSON()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "answers") {
		t.Fatal("result-only serialization mentions answers without a configured answer stage")
	}
}

// TestLoadAnswersParsesExpectedAnswers: the answer-aware loader keeps the
// base record semantics identical (corpus hash compatibility) while
// carrying the expected answer for exact/structured grading.
func TestLoadAnswersParsesExpectedAnswers(t *testing.T) {
	arecs, err := LoadAnswers(filepath.Join("..", "..", "scenarios", "membench", "answers.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(arecs) != 6 {
		t.Fatalf("len(arecs) = %d, want 6", len(arecs))
	}
	q := arecs[3]
	if q.Type != "query" || q.Answer == "" || len(q.Expect) != 1 {
		t.Fatalf("query record = %+v, want expected answer and gold keys", q)
	}
	base := BaseRecords(arecs)
	h1, err := CorpusHash(base)
	if err != nil {
		t.Fatal(err)
	}
	recs := make([]Record, len(arecs))
	for i, a := range arecs {
		recs[i] = a.Record
	}
	h2, err := CorpusHash(recs)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatal("base-record extraction changed the corpus hash")
	}
}

// TestAnswersFixtureGoldRun pins the committed answer fixture end to end
// with the deterministic defaults (extractive composer + gold judge): the
// two answerable queries exact-match with supported citations, the
// unanswerable query is abstained, and accuracy/evidence/cost land in
// their separate summary fields.
func TestAnswersFixtureGoldRun(t *testing.T) {
	arecs, err := LoadAnswers(filepath.Join("..", "..", "scenarios", "membench", "answers.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	rep, err := SuiteWithAnswers(t.Context(), newBenchStore(t), "bench", arecs, SuiteOptions{
		K: 5, Seed: 1, Commit: "test", EmbedderID: "none", RerankerID: "none", Fixture: "answers.jsonl",
		Answers: &AnswerOptions{K: 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	ans := rep.Answers
	if ans == nil {
		t.Fatal("Answers = nil")
	}
	if ans.Composer.Provider != "extractive" || ans.Judge.Provider != "gold" {
		t.Fatalf("composer/judge = %+v / %+v, want extractive/gold defaults", ans.Composer, ans.Judge)
	}
	sum := ans.Summary
	if sum.Queries != 3 || sum.Answerable != 2 || sum.Unanswerable != 1 {
		t.Fatalf("summary = %+v, want queries=3 answerable=2 unanswerable=1", sum)
	}
	if sum.ExactMatchRate == nil || *sum.ExactMatchRate != 1.0 {
		t.Fatalf("ExactMatchRate = %v, want 1.0 (extractive answer is the gold body verbatim)", sum.ExactMatchRate)
	}
	if sum.CitationExistenceRate == nil || *sum.CitationExistenceRate != 1.0 {
		t.Fatalf("CitationExistenceRate = %v, want 1.0", sum.CitationExistenceRate)
	}
	if sum.SupportRate == nil || *sum.SupportRate != 1.0 {
		t.Fatalf("SupportRate = %v, want 1.0", sum.SupportRate)
	}
	if sum.AbstainedUnanswerable != 1 || sum.ConfidentUnanswerable != 0 {
		t.Fatalf("summary = %+v, want the unanswerable query abstained, none confident", sum)
	}
	if sum.Tokens != 0 || sum.JudgeTokens != 0 {
		t.Fatalf("summary tokens = %d/%d, want 0/0 for the deterministic offline defaults", sum.Tokens, sum.JudgeTokens)
	}
	if len(ans.Cases) != 3 {
		t.Fatalf("len(Cases) = %d, want 3 per-case artifacts", len(ans.Cases))
	}
	for _, c := range ans.Cases[:2] {
		if len(c.RetrievedIDs) == 0 || len(c.CitedIDs) == 0 {
			t.Fatalf("case %q missing retrieved/cited IDs: %+v", c.Q, c)
		}
	}
}

// TestLLMJudgeParsesVerdictAndRecordsTokens exercises the model-backed
// judge against a fixed httptest OpenAI-compatible endpoint: explicit
// endpoint/model/budget configuration, verdict JSON parsed, token usage
// recorded. No paid or live endpoint is involved.
func TestLLMJudgeParsesVerdictAndRecordsTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
		  "id": "cmpl-1", "object": "chat.completion", "created": 1,
		  "model": "judge-test",
		  "choices": [{"index": 0, "finish_reason": "stop",
		    "message": {"role": "assistant", "content": "{\"correct\":true,\"support\":false,\"justification\":\"cited fact is unrelated\"}"}}],
		  "usage": {"prompt_tokens": 42, "completion_tokens": 7, "total_tokens": 49}
		}`))
	}))
	t.Cleanup(srv.Close)

	m := llm.NewManager(true, map[string]llm.Profile{
		"default": {BaseURL: srv.URL, Model: "judge-test"},
	}, nil, nil)
	client, err := m.Client("default")
	if err != nil {
		t.Fatal(err)
	}
	j := NewLLMJudge(client, srv.URL, 1000)
	cfg := j.JudgeConfig()
	if cfg.Provider != "openai-compatible" || !cfg.ModelBacked || cfg.Model != "judge-test" || cfg.Endpoint != srv.URL || cfg.BudgetTokens != 1000 {
		t.Fatalf("judge config = %+v, want explicit endpoint/model/budget, model-backed", cfg)
	}
	v, err := j.Judge(t.Context(), JudgeInput{
		Q:              "where does postgres run?",
		Answer:         "on ceph-node-07",
		ExpectedAnswer: "postgres 16 primary runs on ceph-node-07",
		Evidence:       []JudgeEvidence{{ID: "id1", Key: "/svc/db", Body: "postgres 16 primary runs on ceph-node-07"}},
		ExpectKeys:     []string{"/svc/db"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.Correct == nil || !*v.Correct {
		t.Fatalf("Correct = %v, want explicit true", v.Correct)
	}
	if v.Support == nil || *v.Support {
		t.Fatalf("Support = %v, want explicit false", v.Support)
	}
	if v.PromptTokens != 42 || v.CompletionTokens != 7 {
		t.Fatalf("tokens = %d/%d, want 42/7 recorded", v.PromptTokens, v.CompletionTokens)
	}
}

// TestLLMJudgeUnparseableVerdictIsAnError: a judge response that is not
// the verdict JSON is a per-case judge error (recorded, counted), never a
// silently passing grade.
func TestLLMJudgeUnparseableVerdictIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
		  "id": "cmpl-1", "object": "chat.completion", "created": 1,
		  "model": "judge-test",
		  "choices": [{"index": 0, "finish_reason": "stop",
		    "message": {"role": "assistant", "content": "looks fine to me"}}],
		  "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}
		}`))
	}))
	t.Cleanup(srv.Close)
	m := llm.NewManager(true, map[string]llm.Profile{
		"default": {BaseURL: srv.URL, Model: "judge-test"},
	}, nil, nil)
	client, err := m.Client("default")
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewLLMJudge(client, srv.URL, 100).Judge(t.Context(), JudgeInput{Q: "q", Answer: "a"})
	if err == nil {
		t.Fatal("unparseable judge verdict must be an error, not a passing grade")
	}
}

// TestAnswerMatchUnit pins the deterministic grader's normalization and
// structured comparison directly.
func TestAnswerMatchUnit(t *testing.T) {
	cases := []struct {
		expected, got string
		want          bool
	}{
		{"postgres primary runs on host7", "postgres primary runs on host7", true},
		{"Postgres  Primary", "postgres primary", true},   // case + whitespace normalized
		{`{"a":1,"b":2}`, `{"b":2,"a":1}`, true},          // structured: key order free
		{`{"a":1}`, `{"a":2}`, false},                     // structured mismatch
		{"postgres on host7", "postgres on host9", false}, // plain mismatch
		{`{"a":1}`, "not json", false},                    // one side structured, other not
	}
	for _, tc := range cases {
		if got := answerMatch(tc.expected, tc.got); got != tc.want {
			t.Errorf("answerMatch(%q, %q) = %v, want %v", tc.expected, tc.got, got, tc.want)
		}
	}
}

// TestRunAnswersJSONLRoundTrip guards the loader's error surface: a
// malformed line reports path and line number like the base loader.
func TestLoadAnswersMalformedLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.jsonl")
	if err := os.WriteFile(path, []byte("{\"type\":\"fact\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadAnswers(path)
	if err == nil || !strings.Contains(err.Error(), ":1") {
		t.Fatalf("err = %v, want a path:line-wrapped parse error", err)
	}
}
