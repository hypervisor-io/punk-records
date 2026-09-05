package membench

import (
	"context"
	"errors"
	"testing"
)

type reviewerFailedReranker struct{}

func (reviewerFailedReranker) Rerank(context.Context, string, []string) ([]float64, error) {
	return nil, errors.New("reviewer simulated reranker failure")
}
func TestReviewerE01RerankerFailureCannotBeSuccessfulAblation(t *testing.T) {
	s := newBenchStore(t)
	s.SetReranker(reviewerFailedReranker{})
	records := []Record{{Type: "fact", Key: "/fact", Body: "postgres primary"}, {Type: "query", Q: "postgres", Expect: []string{"/fact"}}}
	rep, err := Suite(t.Context(), s, "bench", records, SuiteOptions{K: 5, Seed: 1, Commit: "review-fixture", RerankerID: "reviewer-failing"})
	if err != nil {
		return
	}
	for _, run := range rep.Runs {
		if run.Name == "rerank" && run.Available && run.Reason == "" && run.Summary != nil && run.Summary.Failed == 0 {
			t.Fatal("failed reranker reported as available successful ablation with no failure/fallback evidence")
		}
	}
}
