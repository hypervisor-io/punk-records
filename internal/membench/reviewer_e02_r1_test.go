package membench

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hypervisor-io/punk-records/internal/llm"
	"github.com/hypervisor-io/punk-records/internal/memory"
)

// The E02 round-2 review reproducers, kept as permanent regression tests.
// Each pins one finding against the grading contract: gold-citation
// relevance is not claim support, the token budget gates every model
// call, abstentions/failures cannot inflate accuracy, and a malformed
// judge verdict is a failure - never a successful null grade.

// TestReviewerE02FalseAnswerGoldCitationNotSupport (finding 1): citing
// the GOLD key while contradicting the gold answer must not earn
// support. Membership in ExpectKeys is citation RELEVANCE; the
// deterministic judge verifies support only by exact/structured match
// against the expected answer or a cited gold body, and otherwise leaves
// the semantic case explicitly ungraded - never a pass.
func TestReviewerE02FalseAnswerGoldCitationNotSupport(t *testing.T) {
	v, err := (goldJudge{}).Judge(t.Context(), JudgeInput{
		Q:              "where is primary?",
		Answer:         "primary runs on host9",
		ExpectedAnswer: "primary runs on host7",
		ExpectKeys:     []string{"/db"},
		CitedCount:     1,
		Evidence:       []JudgeEvidence{{ID: "fact1", Key: "/db", Body: "primary runs on host7"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.Support != nil && *v.Support {
		t.Fatal("contradictory answer earned claim support merely by citing gold key")
	}
}

// TestReviewerE02NoJudgeAfterBudgetExhaustion (finding 3): the budget
// gates the judge call too, not just the composer - a composer that
// spends the entire budget means the judge is never called.
func TestReviewerE02NoJudgeAfterBudgetExhaustion(t *testing.T) {
	called := 0
	c := fakeComposer{cfg: ComposerConfig{Provider: "fake-model", ModelBacked: true}, fn: func(_ context.Context, _ string, f []memory.Fact) (ComposeResult, error) {
		return ComposeResult{Text: "primary runs on host7", Tokens: 5}, nil
	}}
	j := fakeJudge{cfg: JudgeConfig{Provider: "fake-judge", ModelBacked: true}, fn: func(context.Context, JudgeInput) (JudgeVerdict, error) {
		called++
		return JudgeVerdict{PromptTokens: 7, Correct: boolPtr(true)}, nil
	}}
	recs := []AnswerRecord{
		{Record: Record{Type: "fact", Key: "/db", Body: "primary runs on host7"}},
		{Record: Record{Type: "query", Q: "primary", Expect: []string{"/db"}}, Answer: "primary runs on host7"},
	}
	_, err := RunAnswers(t.Context(), newBenchStore(t), "bench", recs, AnswerOptions{Composer: c, Judge: j, MaxTokens: 5})
	if err != nil {
		t.Fatal(err)
	}
	if called != 0 {
		t.Fatalf("judge called %d times after composer spent entire budget", called)
	}
}

// TestReviewerE02AbstentionCannotInflateAccuracy (finding 2): the exact
// accuracy rate is end-to-end over every answerable case, so answering
// one of two answerable cases and abstaining on the other reports 50%,
// never a conditional answered-only 100%.
func TestReviewerE02AbstentionCannotInflateAccuracy(t *testing.T) {
	c := fakeComposer{cfg: ComposerConfig{Provider: "scripted"}, fn: func(_ context.Context, q string, _ []memory.Fact) (ComposeResult, error) {
		if q == "host7" {
			return ComposeResult{Abstained: true}, nil
		}
		return ComposeResult{Text: "primary runs on host7"}, nil
	}}
	recs := []AnswerRecord{
		{Record: Record{Type: "fact", Key: "/db", Body: "primary runs on host7"}},
		{Record: Record{Type: "query", Q: "primary", Expect: []string{"/db"}}, Answer: "primary runs on host7"},
		{Record: Record{Type: "query", Q: "host7", Expect: []string{"/db"}}, Answer: "primary runs on host7"},
	}
	rep, err := RunAnswers(t.Context(), newBenchStore(t), "bench", recs, AnswerOptions{Composer: c})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Summary.ExactMatchRate != nil && *rep.Summary.ExactMatchRate == 1 {
		t.Fatal("reported 100% exact accuracy after answering only one of two answerable cases; no end-to-end rate recorded")
	}
}

// TestReviewerE02EmptyJudgeVerdictIsFailure (finding 4): an empty {}
// verdict parses as JSON but carries no grading - it is an explicit
// judge failure, not a successful null grade.
func TestReviewerE02EmptyJudgeVerdictIsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":2}}`))
	}))
	defer srv.Close()
	m := llm.NewManager(true, map[string]llm.Profile{"default": {BaseURL: srv.URL, Model: "fake"}}, nil, nil)
	client, err := m.Client("default")
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewLLMJudge(client, srv.URL, 100).Judge(t.Context(), JudgeInput{Q: "primary?", Answer: "host7"})
	if err == nil {
		t.Fatal("empty verdict accepted as successful judge response")
	}
}
