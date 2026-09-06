package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// The membench answer-stage CLI tests (E02): --answers attaches the
// answer/citation/abstention report to the versioned artifact with the
// deterministic offline defaults; model-backed judging requires explicit
// endpoint/model/budget configuration and only ever runs against the
// throwaway in-memory store - never the live namespace.

func answersScenarioPath() string {
	return filepath.Join("..", "..", "scenarios", "membench", "answers.jsonl")
}

// TestMembenchAnswersReportGoldDefaults: one command writes the report
// with the answer section: extractive composer + gold judge, per-case
// artifacts (answers, retrieved/cited IDs, abstention), separate
// accuracy/evidence/cost summary, and the judge configuration recorded.
func TestMembenchAnswersReportGoldDefaults(t *testing.T) {
	rep := runMembenchReport(t, answersScenarioPath(), "--answers")
	if rep.Answers == nil {
		t.Fatal("report has no answers section with --answers")
	}
	ans := rep.Answers
	if ans.Composer.Provider != "extractive" || ans.Composer.ModelBacked {
		t.Fatalf("composer = %+v, want the deterministic offline extractive default", ans.Composer)
	}
	if ans.Judge.Provider != "gold" || ans.Judge.ModelBacked {
		t.Fatalf("judge = %+v, want the deterministic offline gold default", ans.Judge)
	}
	sum := ans.Summary
	if sum.Queries != 3 || sum.Answerable != 2 || sum.Unanswerable != 1 {
		t.Fatalf("summary = %+v, want queries=3 answerable=2 unanswerable=1", sum)
	}
	if sum.ExactMatchRate == nil || *sum.ExactMatchRate != 1.0 {
		t.Fatalf("ExactMatchRate = %v, want 1.0", sum.ExactMatchRate)
	}
	if sum.SupportRate == nil || *sum.SupportRate != 1.0 {
		t.Fatalf("SupportRate = %v, want 1.0", sum.SupportRate)
	}
	if sum.AbstainedUnanswerable != 1 || sum.ConfidentUnanswerable != 0 {
		t.Fatalf("summary = %+v, want the unanswerable query abstained", sum)
	}
	if sum.Tokens != 0 || sum.JudgeTokens != 0 {
		t.Fatalf("summary tokens = %d/%d, want 0/0 (no model calls by default)", sum.Tokens, sum.JudgeTokens)
	}
	if len(ans.Cases) != 3 {
		t.Fatalf("len(Cases) = %d, want 3 per-case artifacts", len(ans.Cases))
	}
	// The retrieval suite is unaffected.
	if len(rep.Runs) != 4 {
		t.Fatalf("len(Runs) = %d, want 4 retrieval runs alongside the answers section", len(rep.Runs))
	}
}

// TestMembenchAnswersRequiresReport: the answer stage's artifact is the
// versioned report; --answers without --report is a usage error.
func TestMembenchAnswersRequiresReport(t *testing.T) {
	if err := cmdMembench([]string{"--file", answersScenarioPath(), "--answers"}); err == nil {
		t.Fatal("--answers without --report must fail")
	}
}

// TestMembenchAnswersModelJudgeNeedsExplicitConfig: a model-backed judge
// without the full endpoint+model+budget triple is a configuration error
// - no partial wiring, no unbounded spend, no default paid calls.
func TestMembenchAnswersModelJudgeNeedsExplicitConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the judge endpoint must never be called without complete explicit config")
	}))
	t.Cleanup(srv.Close)
	// URL without model.
	if err := cmdMembench([]string{"--file", answersScenarioPath(), "--answers",
		"--report", filepath.Join(t.TempDir(), "r.json"), "--judge-url", srv.URL}); err == nil {
		t.Fatal("--judge-url without --judge-model must fail")
	}
	// URL + model without budget.
	if err := cmdMembench([]string{"--file", answersScenarioPath(), "--answers",
		"--report", filepath.Join(t.TempDir(), "r.json"), "--judge-url", srv.URL, "--judge-model", "m"}); err == nil {
		t.Fatal("model judge without --answer-budget must fail")
	}
	// reflect composer without endpoint/model.
	if err := cmdMembench([]string{"--file", answersScenarioPath(), "--answers",
		"--report", filepath.Join(t.TempDir(), "r.json"), "--composer", "reflect", "--answer-budget", "100"}); err == nil {
		t.Fatal("--composer reflect without endpoint/model must fail")
	}
	// Answer-stage flags without --answers.
	if err := cmdMembench([]string{"--file", answersScenarioPath(), "--report", filepath.Join(t.TempDir(), "r.json"),
		"--judge-url", srv.URL, "--judge-model", "m", "--answer-budget", "100"}); err == nil {
		t.Fatal("answer-stage flags without --answers must fail")
	}
}

// TestMembenchAnswersModelJudgeAgainstExplicitEndpoint: with the full
// explicit configuration the judge runs against exactly the configured
// endpoint (here a fixed httptest double), its verdicts and tokens land
// in the report, and the run still happens over the throwaway in-memory
// store - the live namespace is never involved.
func TestMembenchAnswersModelJudgeAgainstExplicitEndpoint(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
		  "id": "cmpl-1", "object": "chat.completion", "created": 1,
		  "model": "judge-test",
		  "choices": [{"index": 0, "finish_reason": "stop",
		    "message": {"role": "assistant", "content": "{\"correct\":true,\"support\":true,\"justification\":\"ok\"}"}}],
		  "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
		}`))
	}))
	t.Cleanup(srv.Close)

	rep := runMembenchReport(t, answersScenarioPath(), "--answers",
		"--judge-url", srv.URL, "--judge-model", "judge-test", "--answer-budget", "1000")
	if calls == 0 {
		t.Fatal("the explicitly configured judge endpoint was never called")
	}
	ans := rep.Answers
	if ans == nil {
		t.Fatal("no answers section")
	}
	if ans.Judge.Provider != "openai-compatible" || !ans.Judge.ModelBacked || ans.Judge.Model != "judge-test" || ans.Judge.Endpoint != srv.URL || ans.Judge.BudgetTokens != 1000 {
		t.Fatalf("judge config = %+v, want the explicit endpoint/model/budget recorded", ans.Judge)
	}
	if ans.Summary.JudgeTokens == 0 {
		t.Fatal("judge token usage not recorded")
	}
	if ans.Summary.JudgeCorrectRate == nil || *ans.Summary.JudgeCorrectRate != 1.0 {
		t.Fatalf("JudgeCorrectRate = %v, want 1.0 from the fixed double", ans.Summary.JudgeCorrectRate)
	}
	for _, c := range ans.Cases {
		if strings.Contains(c.Error, "budget") {
			t.Fatalf("case %q hit the budget at 1000 tokens: %+v", c.Q, c)
		}
	}
}
