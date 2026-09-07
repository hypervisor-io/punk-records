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

// R02 acceptance: the reflect composer's opt-in evidence expansion runs
// the SAME answer stage on the SAME fixtures under the SAME token
// budget, and the paired reports compare answer quality against added
// cost through CompareAnswerReports. The flag is opt-in: the plain
// reflect composer's config and behavior are untouched.

// newMultiHopFixture writes the R02 graph into the bench namespace: a
// db incident (a) and an outage (b) both match the query tokens; the
// firmware fact (c) matches neither and is reachable only through the
// described b->c relation, so the answer needs a second evidence hop.
// The pointer variant lets the caller alter the pointer fact's body
// (the altered-evidence control below).
func newMultiHopFixture(t *testing.T, s *memory.Store, pointerBody string) (c memory.Fact) {
	t.Helper()
	ctx := t.Context()
	for _, f := range []struct{ key, body string }{
		{"/incidents/db", "the db degraded overnight after the /net/outage"},
		{"/net/outage", pointerBody},
		{"/net/firmware", "firmware 2.3.1 rollback restored the ports"},
	} {
		if _, err := s.Write(ctx, memory.WriteInput{Namespace: "bench", Key: f.key, Body: f.body}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddLinkDescribed(ctx, "bench", "/net/outage", "/net/firmware", "relates_to", 1.0, "remedied by"); err != nil {
		t.Fatal(err)
	}
	f, err := s.Recall(ctx, "bench", "/net/firmware", 1)
	if err != nil || len(f) != 1 {
		t.Fatalf("recall firmware: %v %v", f, err)
	}
	return f[0]
}

// pointerTerm / pointerMarker are the policy's evidence markers: a shown
// fact whose body carries the marker names the follow-up term the policy
// recalls (baseline) or the fact whose key the policy expands
// (expanding arm). The policy READS these from the actual tool payloads;
// nothing about them is predetermined.
const (
	pointerMarker = "remedied by the "
	pointerTerm   = "firmware"
)

// pointerPolicyClient is ONE shared evidence-responsive policy driving
// BOTH comparison arms. Every turn it parses the facts its prior tool
// results actually showed, then decides:
//  1. the gold fact is in evidence -> answer citing it;
//  2. some shown fact carries the remediation pointer -> follow it:
//     recall the pointer term (baseline action space) or expand the
//     pointer fact's key (expanding action space);
//  3. neither -> abstain honestly.
//
// With altered evidence (no marker in any shown body) the policy
// abstains - the control proving it responds to evidence rather than
// replaying a predetermined sequence.
type pointerPolicyClient struct {
	expand  bool // action space: expand tool available (expanding arm)
	goldID  string
	answer  string
	recalls int
}

func (c *pointerPolicyClient) Model() string { return "pointer-policy" }

// shownFacts extracts every fact the tool results have shown so far.
// Recall returns a bare fact array; expand returns an object with a
// facts field.
func shownFacts(turns []llm.Turn) []memory.Fact {
	var out []memory.Fact
	for _, turn := range turns {
		if turn.Role != "tool" {
			continue
		}
		var facts []memory.Fact
		if json.Unmarshal([]byte(turn.Content), &facts) == nil {
			out = append(out, facts...)
			continue
		}
		var obj struct {
			Facts []memory.Fact `json:"facts"`
		}
		if json.Unmarshal([]byte(turn.Content), &obj) == nil {
			out = append(out, obj.Facts...)
		}
	}
	return out
}

func (c *pointerPolicyClient) call(tc llm.ToolCall) (*llm.Result, error) {
	return &llm.Result{ToolCalls: []llm.ToolCall{tc}, PromptTokens: 10, CompletionTokens: 5}, nil
}

func (c *pointerPolicyClient) Chat(_ context.Context, turns []llm.Turn, _ []llm.Tool) (*llm.Result, error) {
	c.recalls++
	id := fmt.Sprintf("c%d", c.recalls)
	shown := shownFacts(turns)

	// Step 0: nothing shown yet - recall the user's question.
	if len(shown) == 0 {
		question := ""
		for _, turn := range turns {
			if turn.Role == "user" {
				question = turn.Content
			}
		}
		args, _ := json.Marshal(map[string]any{"query": question})
		return c.call(llm.ToolCall{ID: id, Name: "recall", Args: args})
	}

	for _, f := range shown {
		if f.ID == c.goldID {
			args, _ := json.Marshal(map[string]any{"answer": c.answer, "citations": []string{c.goldID}})
			return c.call(llm.ToolCall{ID: id, Name: "done", Args: args})
		}
	}
	for _, f := range shown {
		if strings.Contains(f.Body, pointerMarker) {
			if c.expand {
				args, _ := json.Marshal(map[string]any{"keys": []string{f.Key}})
				return c.call(llm.ToolCall{ID: id, Name: "expand", Args: args})
			}
			args, _ := json.Marshal(map[string]any{"query": pointerTerm})
			return c.call(llm.ToolCall{ID: id, Name: "recall", Args: args})
		}
	}
	args, _ := json.Marshal(map[string]any{"answer": "", "abstain": true})
	return c.call(llm.ToolCall{ID: id, Name: "done", Args: args})
}

// TestExpandingComparisonSameEvidenceResponsivePolicy is the E02/R02
// comparison harness, built to the fairness contract: BOTH arms run the
// SAME shared callback policy (see pointerPolicyClient) that inspects
// the facts actually shown before choosing pointer-follow, answer, or
// abstain, under the SAME budget. The arms differ only in action space
// (baseline recalls the pointer term; the expanding arm expands the
// pointer fact's key).
//
// SYNTHETIC FIXTURE, MECHANISM + FAIRNESS EVIDENCE ONLY: the corpus
// deliberately exposes the pointer term ("firmware") to plain recall, so
// the baseline is NOT inherently unable to reach the answer - a real
// model could find it either way. The expected honest outcome is a
// NEUTRAL quality delta at equal measured cost. The 15-token call costs
// are synthetic accounting values, not measured provider spend. This
// proves the comparison harness measures both arms fairly; it establishes
// no real-model or general quality superiority, and no paid
// external-model campaign is run here.
func TestExpandingComparisonSameEvidenceResponsivePolicy(t *testing.T) {
	s := newBenchStore(t)
	c := newMultiHopFixture(t, s, "switch outage affected the db; remedied by the firmware change")
	recs := []AnswerRecord{
		{Record: Record{Type: "fact", Key: "/incidents/db", Body: "the db degraded overnight after the /net/outage"}},
		{Record: Record{Type: "fact", Key: "/net/outage", Body: "switch outage affected the db; remedied by the firmware change"}},
		{Record: Record{Type: "fact", Key: "/net/firmware", Body: "firmware 2.3.1 rollback restored the ports"}},
		{Record: Record{Type: "query", Q: "db outage", Expect: []string{"/net/firmware"}}, Answer: "firmware 2.3.1 rollback restored the ports"},
	}

	run := func(client *pointerPolicyClient, expand bool) AnswerReport {
		var composer Composer
		if expand {
			composer = NewReflectExpandingComposer(reflect.New(s, client), "bench", "http://policy", "pointer-policy")
		} else {
			composer = NewReflectComposer(reflect.New(s, client), "bench", "http://policy", "pointer-policy")
		}
		rep, err := RunAnswers(t.Context(), s, "bench", recs, AnswerOptions{K: 5, Composer: composer, MaxTokens: 1000})
		if err != nil {
			t.Fatal(err)
		}
		return rep
	}

	// Control first: with the pointer ALTERED out of the evidence, the
	// shared policy must abstain on both arms - it responds to the
	// evidence, it does not replay a predetermined sequence.
	alt := newBenchStore(t)
	altGold := newMultiHopFixture(t, alt, "switch outage affected the db; root cause never written down")
	altRecs := []AnswerRecord{
		{Record: Record{Type: "fact", Key: "/incidents/db", Body: "the db degraded overnight after the /net/outage"}},
		{Record: Record{Type: "fact", Key: "/net/outage", Body: "switch outage affected the db; root cause never written down"}},
		{Record: Record{Type: "fact", Key: "/net/firmware", Body: "firmware 2.3.1 rollback restored the ports"}},
		{Record: Record{Type: "query", Q: "db outage", Expect: []string{"/net/firmware"}}, Answer: "firmware 2.3.1 rollback restored the ports"},
	}
	for _, arm := range []struct {
		name   string
		expand bool
	}{{"baseline", false}, {"expanded", true}} {
		var composer Composer
		client := &pointerPolicyClient{expand: arm.expand, goldID: altGold.ID, answer: "firmware 2.3.1 rollback restored the ports"}
		if arm.expand {
			composer = NewReflectExpandingComposer(reflect.New(alt, client), "bench", "http://policy", "pointer-policy")
		} else {
			composer = NewReflectComposer(reflect.New(alt, client), "bench", "http://policy", "pointer-policy")
		}
		rep, err := RunAnswers(t.Context(), alt, "bench", altRecs, AnswerOptions{K: 5, Composer: composer, MaxTokens: 1000})
		if err != nil {
			t.Fatal(err)
		}
		ac := answerCase(t, rep, "db outage")
		if !ac.Abstained {
			t.Fatalf("altered-evidence control (%s arm) = %+v, want abstain: the policy must follow evidence, not a script", arm.name, ac)
		}
	}

	// Baseline arm: plain reflect composer (no expand tool), same
	// policy, same K, same token budget.
	base := run(&pointerPolicyClient{goldID: c.ID, answer: "firmware 2.3.1 rollback restored the ports"}, false)
	if base.Composer.ExpandEvidence {
		t.Fatal("baseline composer config claims expansion, want opt-in off")
	}
	baseCase := answerCase(t, base, "db outage")
	if baseCase.Abstained || baseCase.Error != "" {
		t.Fatalf("baseline case = %+v, want answered: the evidence-responsive baseline follows the pointer with a second recall", baseCase)
	}
	if len(baseCase.CitedIDs) != 1 || baseCase.CitedIDs[0] != c.ID {
		t.Fatalf("baseline cited IDs = %v, want [%s]", baseCase.CitedIDs, c.ID)
	}
	if baseCase.ExactMatch == nil || !*baseCase.ExactMatch || !baseCase.CitationExist {
		t.Fatalf("baseline grading = %+v, want exact match and citation existence", baseCase)
	}

	// Expanded arm: same fixtures, same K, same budget, expansion on;
	// same policy with the pointer followed through the expand tool.
	expanded := run(&pointerPolicyClient{expand: true, goldID: c.ID, answer: "firmware 2.3.1 rollback restored the ports"}, true)
	if !expanded.Composer.ExpandEvidence || !expanded.Composer.ModelBacked {
		t.Fatalf("expanded composer config = %+v, want model-backed expansion", expanded.Composer)
	}
	ec := answerCase(t, expanded, "db outage")
	if ec.Abstained || ec.Error != "" {
		t.Fatalf("expanded case = %+v, want an answered case", ec)
	}
	if len(ec.CitedIDs) != 1 || ec.CitedIDs[0] != c.ID {
		t.Fatalf("cited IDs = %v, want [%s]", ec.CitedIDs, c.ID)
	}
	if !ec.CitationExist || ec.ExactMatch == nil || !*ec.ExactMatch {
		t.Fatalf("expanded case grading = exist %v exact %v, want both true", ec.CitationExist, ec.ExactMatch)
	}

	d := CompareAnswerReports(base, expanded)
	if d.Queries != 1 {
		t.Fatalf("delta queries = %d, want 1 (the shared scenario size)", d.Queries)
	}
	// The honest result for a fair comparison on this synthetic fixture
	// is NEUTRAL quality at equal measured cost: both evidence-responsive
	// arms reach and cite the gold fact in three 15-token calls. Actual
	// deltas are asserted as data - including when they are zero - and
	// never massaged into a manufactured win.
	if d.AnsweredDelta != 0 || d.AbstainedAnswerableDelta != 0 || d.FailedDelta != 0 {
		t.Fatalf("quality deltas = answered %+d abstained %+d failed %+d, want all 0 (equal policy, equal budget)", d.AnsweredDelta, d.AbstainedAnswerableDelta, d.FailedDelta)
	}
	if d.ExactMatchRateDelta == nil || *d.ExactMatchRateDelta != 0.0 {
		t.Fatalf("exact match rate delta = %v, want 0.0 (both arms exact-match)", d.ExactMatchRateDelta)
	}
	if d.TokensDelta != 0 || d.JudgeTokensDelta != 0 {
		t.Fatalf("cost deltas = tokens %+d judge %+d, want 0 (three calls on both sides)", d.TokensDelta, d.JudgeTokensDelta)
	}
}

// TestCompareAnswerReportsNilRatesStayNil: a rate missing on either side
// (empty denominator) yields a nil delta, never a vacuous number.
func TestCompareAnswerReportsNilRatesStayNil(t *testing.T) {
	base := AnswerReport{Summary: AnswerSummary{Queries: 2}}
	expanded := AnswerReport{Summary: AnswerSummary{Queries: 2, ExactAnswerable: 1, ExactGraded: 1}}
	emr := 1.0
	expanded.Summary.ExactMatchRate = &emr
	d := CompareAnswerReports(base, expanded)
	if d.ExactMatchRateDelta != nil {
		t.Fatalf("exact match rate delta = %v, want nil (baseline denominator empty)", d.ExactMatchRateDelta)
	}
	if d.Queries != 2 || d.TokensDelta != 0 {
		t.Fatalf("delta = %+v", d)
	}
}
