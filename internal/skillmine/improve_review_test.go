package skillmine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/task"
)

// Reviewer regression suites (rounds 1-3), folded in as permanent
// descriptive tests. Round 1: a rejected revert preserves the active
// skill, a rejected run recording appends no ledger evidence, an
// approval binds the exact draft digest, and proposal dedup is
// namespace-scoped. Round 2: mandatory integrity checks veto
// independently of an optional checker's grades, and the candidate's
// own outcome evaluation - not the base version's old failures - gates
// activation. Round 3: execution identity is an explicit id (never
// outcome content), retries heal once, and concurrent duplicates append
// once. Round 3 rejection: a form-only evaluation never proves a
// candidate - support comes only from acceptance assertions actually
// executed against the candidate body, unknown support stays ungraded,
// and a persisted passed flag alone does not activate - and an orphan
// retry cannot change a recorded outcome: anchor reuse requires exact
// identity and semantic payload consistency, the persisted execution
// identity carries the namespace, and the dedup is atomic at the
// ledger's transaction boundary. Companion tests pin the supporting
// semantics: distinct executions with identical outputs stay separate,
// and unsupported vs not-graded candidates are distinguishably refused.

// A revert to a missing target must be refused without touching the
// currently working version: both sides are validated before any
// visibility flip, so a rejected revert leaves the active state active.
func TestReviewerS02InvalidRevertPreservesActive(t *testing.T) {
	mem, _, _, _, _ := newImproveTest(t)
	ctx := context.Background()
	indexImproveSkill(t, mem, "ns", improveSkillName, "0.1.0", "working procedure")
	if err := mem.RevertSkillVersion(ctx, "ns", improveSkillName, "0.1.0", "missing"); err == nil {
		t.Fatal("expected missing target error")
	}
	if _, _, err := mem.LoadSkill(ctx, "ns", improveSkillName, ""); err != nil {
		t.Fatalf("failed revert disabled working skill: %v", err)
	}
}

// A recording rejected for a never-published version must append no
// ledger event: input and identity are validated before the append, so
// the ledger never claims an outcome that was never recordable.
func TestReviewerS02RejectedRunDoesNotAppendSuccessEvidence(t *testing.T) {
	mem, db, led, _, clk := newImproveTest(t)
	ctx := context.Background()
	id := mkTask(t, led, db, clk, "db", "investigate", nil, "run")
	_, before, err := led.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	_, err = RecordRunOutcome(ctx, mem, led, "ns", RunOutcomeInput{
		SkillName: improveSkillName, SkillVersion: "never-published",
		TaskID: id, ExecutionID: "exec-1", Outcome: memory.RunOutcomeSuccess,
	})
	if err == nil {
		t.Fatal("expected unknown version error")
	}
	_, after, err := led.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("rejected run appended %d ledger events claiming an unrecordable outcome", len(after)-len(before))
	}
}

// Approval binds the exact draft digest: swapping the /skill-drafts/
// content after approval cannot publish a different, unapproved
// procedure under the approved content-derived version. The digest is
// verified at application time, independent of the optional checker.
func TestReviewerS02ApprovalBindsDraftContent(t *testing.T) {
	mem, db, led, props, clk := newImproveTest(t)
	ctx := context.Background()
	indexImproveSkill(t, mem, "ns", improveSkillName, "0.1.0", "original procedure")
	id := mkTask(t, led, db, clk, "db", "investigate", nil, "run")
	recordFailedRun(t, mem, led, "ns", improveSkillName, "0.1.0", id)
	imp, err := ProposeImprovement(ctx, mem, led, props, "ns", improveSkillName, &stubImprover{body: "reviewed improved procedure"}, ProposeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := props.UpdateStatus(ctx, imp.Proposal.ID, "approved", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Write(ctx, memory.WriteInput{
		Namespace: "ns", Key: skillDraftKey(improveSkillName, imp.ProposedVersion),
		Body: "different unapproved procedure", Writer: "draft-editor",
	}); err != nil {
		t.Fatal(err)
	}
	err = ApplySkillProposal(ctx, mem, props, "ns", imp.Proposal.ID, nil)
	if !errors.Is(err, ErrDraftDigestMismatch) {
		t.Fatalf("applied changed draft: err = %v, want ErrDraftDigestMismatch", err)
	}
}

// The same skill and body improving in two namespaces are two distinct
// proposals: the ledger's open-task dedup key carries the namespace, so
// team-b never receives team-a's proposal or its run citations.
func TestReviewerS02ProposalDedupIsNamespaceScoped(t *testing.T) {
	mem, db, led, props, clk := newImproveTest(t)
	ctx := context.Background()
	var first string
	for _, ns := range []string{"team-a", "team-b"} {
		indexImproveSkill(t, mem, ns, improveSkillName, "0.1.0", "original procedure")
		id := mkTask(t, led, db, clk, "db", "investigate", nil, "run "+ns)
		recordFailedRun(t, mem, led, ns, improveSkillName, "0.1.0", id)
		imp, err := ProposeImprovement(ctx, mem, led, props, ns, improveSkillName, &stubImprover{body: "same improved procedure"}, ProposeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if imp == nil {
			t.Fatal("missing proposal")
		}
		if ns == "team-a" {
			first = imp.Proposal.ID
		} else if imp.Proposal.ID == first {
			t.Fatal("different namespaces returned same proposal and first namespace run evidence")
		}
	}
}

// Mandatory integrity checks veto independently: an optional checker
// grading every dimension true cannot disarm the deterministically
// false base-identity grade. The checker may add vetoes, never remove
// them.
func TestReviewerS02CustomVerdictCannotOverrideIdentityInvariant(t *testing.T) {
	mem, db, led, props, clk := newImproveTest(t)
	ctx := context.Background()
	indexImproveSkill(t, mem, "ns", improveSkillName, "0.1.0", "original procedure")
	id := mkTask(t, led, db, clk, "db", "investigate", nil, "run")
	recordFailedRun(t, mem, led, "ns", improveSkillName, "0.1.0", id)
	imp, err := ProposeImprovement(ctx, mem, led, props, "ns", improveSkillName, &stubImprover{body: "improved procedure"}, ProposeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = props.UpdateStatus(ctx, imp.Proposal.ID, "approved", ""); err != nil {
		t.Fatal(err)
	}
	evalCandidateDeterministically(t, mem, "ns", imp.Proposal)
	if _, err = mem.Write(ctx, memory.WriteInput{Namespace: "ns", Key: "/skill-identities/" + improveSkillName + "/0.1.0", Body: "different identity", Writer: "test"}); err != nil {
		t.Fatal(err)
	}
	yes := true
	checker := staticChecker{verdict: OutcomeVerdict{CitationsResolve: &yes, IdentityMatch: &yes, DraftWellFormed: &yes, EvidenceSupports: &yes}}
	if err = ApplySkillProposal(ctx, mem, props, "ns", imp.Proposal.ID, checker); err == nil {
		t.Fatal("custom verdict overrode a deterministically false mandatory base-identity check")
	}
	if listed, err := mem.ListSkillsAll(ctx, "ns"); err != nil || len(listed) != 1 {
		t.Fatalf("overridden apply published something: %+v err=%v", listed, err)
	}
}

// The base version's old failures motivate a proposal; they do not
// evaluate the candidate. Activation requires an explicit, digest-bound
// candidate evaluation: an arbitrary unevaluated draft stays inactive
// even with the default checker.
func TestReviewerS02PriorFailureDoesNotEvaluateCandidate(t *testing.T) {
	mem, db, led, props, clk := newImproveTest(t)
	ctx := context.Background()
	indexImproveSkill(t, mem, "ns", improveSkillName, "0.1.0", "original procedure")
	id := mkTask(t, led, db, clk, "db", "investigate", nil, "run")
	recordFailedRun(t, mem, led, "ns", improveSkillName, "0.1.0", id)
	imp, err := ProposeImprovement(ctx, mem, led, props, "ns", improveSkillName, &stubImprover{body: "Unvalidated candidate: declare success without investigating."}, ProposeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = props.UpdateStatus(ctx, imp.Proposal.ID, "approved", ""); err != nil {
		t.Fatal(err)
	}
	err = ApplySkillProposal(ctx, mem, props, "ns", imp.Proposal.ID, nil)
	if !errors.Is(err, ErrOutcomeCheckFailed) {
		t.Fatalf("activated candidate without any candidate evaluation: err = %v, want ErrOutcomeCheckFailed", err)
	}
	if !strings.Contains(err.Error(), "not graded") {
		t.Fatalf("unevaluated candidate refusal is not auditable as not-graded: %v", err)
	}
	if listed, err := mem.ListSkillsAll(ctx, "ns"); err != nil || len(listed) != 1 {
		t.Fatalf("unevaluated apply published something: %+v err=%v", listed, err)
	}
}

// An optional checker cannot conjure a candidate evaluation either: an
// all-true verdict on an unevaluated candidate still vetoes, because
// the mandatory evaluation artifact is missing.
func TestAllTrueVerdictCannotConjureCandidateEvaluation(t *testing.T) {
	mem, db, led, props, clk := newImproveTest(t)
	ctx := context.Background()
	indexImproveSkill(t, mem, "ns", improveSkillName, "0.1.0", "original procedure")
	id := mkTask(t, led, db, clk, "db", "investigate", nil, "run")
	recordFailedRun(t, mem, led, "ns", improveSkillName, "0.1.0", id)
	imp, err := ProposeImprovement(ctx, mem, led, props, "ns", improveSkillName, &stubImprover{body: "improved procedure"}, ProposeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := props.UpdateStatus(ctx, imp.Proposal.ID, "approved", ""); err != nil {
		t.Fatal(err)
	}
	yes := true
	checker := staticChecker{verdict: OutcomeVerdict{CitationsResolve: &yes, IdentityMatch: &yes, DraftWellFormed: &yes, EvidenceSupports: &yes}}
	if err := ApplySkillProposal(ctx, mem, props, "ns", imp.Proposal.ID, checker); !errors.Is(err, ErrOutcomeCheckFailed) {
		t.Fatalf("all-true verdict bypassed the candidate evaluation gate: err = %v", err)
	}
	if listed, err := mem.ListSkillsAll(ctx, "ns"); err != nil || len(listed) != 1 {
		t.Fatalf("unevaluated apply published something: %+v err=%v", listed, err)
	}
}

// Unsupported and not-graded are distinguishably auditable: a recorded
// failed evaluation vetoes with an unsupported verdict, while a missing
// evaluation vetoes as not graded.
func TestCandidateEvalUnsupportedVsNotGraded(t *testing.T) {
	mem, db, led, props, clk := newImproveTest(t)
	ctx := context.Background()
	indexImproveSkill(t, mem, "ns", improveSkillName, "0.1.0", "original procedure")
	id := mkTask(t, led, db, clk, "db", "investigate", nil, "run")
	recordFailedRun(t, mem, led, "ns", improveSkillName, "0.1.0", id)
	imp, err := ProposeImprovement(ctx, mem, led, props, "ns", improveSkillName, &stubImprover{body: "improved procedure"}, ProposeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := props.UpdateStatus(ctx, imp.Proposal.ID, "approved", ""); err != nil {
		t.Fatal(err)
	}
	if err := ApplySkillProposal(ctx, mem, props, "ns", imp.Proposal.ID, nil); !strings.Contains(err.Error(), "not graded") {
		t.Fatalf("missing evaluation: err = %v, want a not-graded refusal", err)
	}
	var params skillImprovementParams
	if err := json.Unmarshal([]byte(imp.Proposal.Params), &params); err != nil {
		t.Fatal(err)
	}
	if err := RecordCandidateEvaluation(ctx, mem, "ns", improveSkillName, params.ProposedVersion, CandidateEvaluation{
		DraftDigest: params.DraftDigest, Status: CandidateEvalFailed,
		Check: "fixture check: candidate does not address the cited failure", Sources: params.RunIDs,
	}); err != nil {
		t.Fatal(err)
	}
	err = ApplySkillProposal(ctx, mem, props, "ns", imp.Proposal.ID, nil)
	if !errors.Is(err, ErrOutcomeCheckFailed) || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("failed evaluation: err = %v, want an unsupported refusal", err)
	}
	if listed, err := mem.ListSkillsAll(ctx, "ns"); err != nil || len(listed) != 1 {
		t.Fatalf("unsupported apply published something: %+v err=%v", listed, err)
	}
}

// The deterministic evaluator's passing artifact lets the default gate
// activate: an evaluated, digest-bound candidate applies without a model.
func TestEvaluatedCandidateApplies(t *testing.T) {
	mem, db, led, props, clk := newImproveTest(t)
	ctx := context.Background()
	indexImproveSkill(t, mem, "ns", improveSkillName, "0.1.0", "original procedure")
	id := mkTask(t, led, db, clk, "db", "investigate", nil, "run")
	recordFailedRun(t, mem, led, "ns", improveSkillName, "0.1.0", id)
	imp, err := ProposeImprovement(ctx, mem, led, props, "ns", improveSkillName, &stubImprover{body: "improved procedure"}, ProposeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := props.UpdateStatus(ctx, imp.Proposal.ID, "approved", ""); err != nil {
		t.Fatal(err)
	}
	evalCandidateDeterministically(t, mem, "ns", imp.Proposal)
	if err := ApplySkillProposal(ctx, mem, props, "ns", imp.Proposal.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, body, err := mem.LoadSkill(ctx, "ns", improveSkillName, ""); err != nil || body != "improved procedure" {
		t.Fatalf("active body after evaluated apply: %q err=%v", body, err)
	}
}

// A proposal stored in one namespace cannot be applied through another:
// the stored proposal is namespace-bound and apply rejects the
// wrong-namespace caller.
func TestApplyRejectsWrongNamespace(t *testing.T) {
	mem, db, led, props, clk := newImproveTest(t)
	ctx := context.Background()
	indexImproveSkill(t, mem, "ns", improveSkillName, "0.1.0", "original procedure")
	id := mkTask(t, led, db, clk, "db", "investigate", nil, "run")
	recordFailedRun(t, mem, led, "ns", improveSkillName, "0.1.0", id)
	imp, err := ProposeImprovement(ctx, mem, led, props, "ns", improveSkillName, &stubImprover{body: "improved procedure"}, ProposeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := props.UpdateStatus(ctx, imp.Proposal.ID, "approved", ""); err != nil {
		t.Fatal(err)
	}
	if err := ApplySkillProposal(ctx, mem, props, "other", imp.Proposal.ID, nil); err == nil {
		t.Fatal("wrong-namespace apply accepted")
	}
	if listed, err := mem.ListSkillsAll(ctx, "other"); err != nil || len(listed) != 0 {
		t.Fatalf("wrong-namespace apply published something: %+v err=%v", listed, err)
	}
}

// Stable execution identity: a retry of the same execution (same
// explicit execution id) returns the first pinned record and appends no
// second skill_run event, so retries cannot inflate evidence counts.
func TestRunRecordingRetryHasStableIdentity(t *testing.T) {
	mem, db, led, _, clk := newImproveTest(t)
	ctx := context.Background()
	indexImproveSkill(t, mem, "ns", improveSkillName, "0.1.0", "original procedure")
	id := mkTask(t, led, db, clk, "db", "investigate", nil, "run")
	in := RunOutcomeInput{
		SkillName: improveSkillName, SkillVersion: "0.1.0", TaskID: id,
		ExecutionID: "execution-7",
		Outcome:     memory.RunOutcomeFailed, SuccessScore: 0.1,
		ErrorType: "tool_failure", ErrorMessage: "pool exhausted after 3 retries",
		Actor: "agent:db",
	}
	first, err := RecordRunOutcome(ctx, mem, led, "ns", in)
	if err != nil {
		t.Fatal(err)
	}
	_, before, err := led.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	again, err := RecordRunOutcome(ctx, mem, led, "ns", in)
	if err != nil {
		t.Fatal(err)
	}
	if again.RunID != first.RunID || again.RunID != fmt.Sprintf("%s-%s", id, "execution-7") {
		t.Fatalf("retry run id %q != first %q, want the execution-derived id", again.RunID, first.RunID)
	}
	_, after, err := led.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("retry appended %d ledger events", len(after)-len(before))
	}
	// A retry carrying different content for a recorded execution is
	// refused: the execution's outcome is fixed by its first recording.
	conflicting := in
	conflicting.Outcome = memory.RunOutcomeSuccess
	if _, err := RecordRunOutcome(ctx, mem, led, "ns", conflicting); err == nil {
		t.Fatal("conflicting retry for a recorded execution accepted")
	}
}

// Execution identity is the explicit id, never outcome content: two
// real executions of one task with identical outputs remain two
// separate runs and two separate evidence events.
func TestDistinctExecutionsWithIdenticalOutputsStaySeparate(t *testing.T) {
	mem, db, led, _, clk := newImproveTest(t)
	ctx := context.Background()
	indexImproveSkill(t, mem, "ns", improveSkillName, "0.1.0", "original procedure")
	id := mkTask(t, led, db, clk, "db", "investigate", nil, "run")
	mk := func(exec string) RunOutcomeInput {
		return RunOutcomeInput{
			SkillName: improveSkillName, SkillVersion: "0.1.0", TaskID: id,
			ExecutionID: exec, Outcome: memory.RunOutcomeFailed, SuccessScore: 0.1,
			ErrorType: "tool_failure", ErrorMessage: "pool exhausted after 3 retries",
			Actor: "agent:db",
		}
	}
	first, err := RecordRunOutcome(ctx, mem, led, "ns", mk("exec-a"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := RecordRunOutcome(ctx, mem, led, "ns", mk("exec-b"))
	if err != nil {
		t.Fatal(err)
	}
	if first.RunID == second.RunID {
		t.Fatalf("identical outputs collapsed to one run id %q", first.RunID)
	}
	runs, err := mem.ListSkillRuns(ctx, "ns", improveSkillName, "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("run lineage = %d records, want 2 distinct executions", len(runs))
	}
	_, events, err := led.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range events {
		if e.Type == task.EventSkillRun {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("%d skill_run events for 2 distinct executions", n)
	}
}

// Partial-write heal: an appended-but-unpinned skill_run event from a
// failed prior attempt is reused (same sequence, same run id) instead of
// stacking a duplicate event, so the execution keeps one identity.
func TestRunRecordingHealsPartialWrite(t *testing.T) {
	mem, db, led, _, clk := newImproveTest(t)
	ctx := context.Background()
	indexImproveSkill(t, mem, "ns", improveSkillName, "0.1.0", "original procedure")
	id := mkTask(t, led, db, clk, "db", "investigate", nil, "run")
	in := RunOutcomeInput{
		SkillName: improveSkillName, SkillVersion: "0.1.0", TaskID: id,
		ExecutionID: "execution-9",
		Outcome:     memory.RunOutcomeFailed, SuccessScore: 0.1,
		ErrorType: "tool_failure", ErrorMessage: "pool exhausted after 3 retries",
		Actor: "agent:db",
	}
	// Simulate the orphan: the ledger append of a prior attempt whose
	// memory-plane pin never landed.
	orphanSeq, err := led.AppendSkillRun(ctx, id, "agent:db", task.SkillRunPayload{
		SkillName: in.SkillName, SkillVersion: in.SkillVersion, ExecutionID: in.ExecutionID,
		Outcome: in.Outcome, SuccessScore: in.SuccessScore,
		ErrorType: in.ErrorType, ErrorMessage: in.ErrorMessage,
	})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := RecordRunOutcome(ctx, mem, led, "ns", in)
	if err != nil {
		t.Fatal(err)
	}
	if rec.RunID != fmt.Sprintf("%s-%s", id, in.ExecutionID) {
		t.Fatalf("run id %q does not reuse the orphan event's identity", rec.RunID)
	}
	if rec.TaskSeq != orphanSeq {
		t.Fatalf("healed record cites seq %d, want the orphan's %d", rec.TaskSeq, orphanSeq)
	}
	_, events, err := led.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Type == task.EventSkillRun && e.Seq != orphanSeq {
			t.Fatalf("recording appended a second skill_run event (seq %d)", e.Seq)
		}
	}
}

// Concurrent duplicates of one execution serialize on the run key: only
// the first appends evidence, the rest return the first recording.
func TestConcurrentDuplicateRecordingsAppendOnce(t *testing.T) {
	mem, db, led, _, clk := newImproveTest(t)
	ctx := context.Background()
	indexImproveSkill(t, mem, "ns", improveSkillName, "0.1.0", "original procedure")
	id := mkTask(t, led, db, clk, "db", "investigate", nil, "run")
	in := RunOutcomeInput{
		SkillName: improveSkillName, SkillVersion: "0.1.0", TaskID: id,
		ExecutionID: "execution-race",
		Outcome:     memory.RunOutcomeFailed, SuccessScore: 0.1,
		ErrorType: "tool_failure", ErrorMessage: "pool exhausted after 3 retries",
		Actor: "agent:db",
	}
	const workers = 6
	var wg sync.WaitGroup
	errs := make([]error, workers)
	ids := make([]string, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec, err := RecordRunOutcome(ctx, mem, led, "ns", in)
			errs[i] = err
			if rec != nil {
				ids[i] = rec.RunID
			}
		}(i)
	}
	wg.Wait()
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
	}
	for i := 1; i < workers; i++ {
		if ids[i] != ids[0] {
			t.Fatalf("worker %d run id %q != worker 0 %q", i, ids[i], ids[0])
		}
	}
	_, events, err := led.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range events {
		if e.Type == task.EventSkillRun {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d skill_run events for one concurrent execution", n)
	}
	runs, err := mem.ListSkillRuns(ctx, "ns", improveSkillName, "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("%d run records for one concurrent execution", len(runs))
	}
}

// An all-nil custom verdict grades nothing, but the mandatory integrity
// checks still run: the deterministic grades veto on their own.
func TestAllNilVerdictCannotBypassIntegrityChecks(t *testing.T) {
	mem, db, led, props, clk := newImproveTest(t)
	ctx := context.Background()
	indexImproveSkill(t, mem, "ns", improveSkillName, "0.1.0", "original procedure")
	id := mkTask(t, led, db, clk, "db", "investigate", nil, "run")
	recordFailedRun(t, mem, led, "ns", improveSkillName, "0.1.0", id)
	real, err := ProposeImprovement(ctx, mem, led, props, "ns", improveSkillName, &stubImprover{body: "improved procedure"}, ProposeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := props.UpdateStatus(ctx, real.Proposal.ID, "approved", ""); err != nil {
		t.Fatal(err)
	}
	evalCandidateDeterministically(t, mem, "ns", real.Proposal)
	// Tamper the approved identity, then apply through a checker that
	// grades nothing at all: the default identity grade must still veto.
	var params skillImprovementParams
	if err := json.Unmarshal([]byte(real.Proposal.Params), &params); err != nil {
		t.Fatal(err)
	}
	params.BaseIdentity = "deadbeef"
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	tampered, err := props.Create(ctx, id, ActionClassSkillImprove, improveSkillName, string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := props.UpdateStatus(ctx, tampered.ID, "approved", ""); err != nil {
		t.Fatal(err)
	}
	err = ApplySkillProposal(ctx, mem, props, "ns", tampered.ID, staticChecker{verdict: OutcomeVerdict{}})
	if !errors.Is(err, ErrOutcomeCheckFailed) {
		t.Fatalf("all-nil verdict bypassed integrity checks: err = %v, want ErrOutcomeCheckFailed", err)
	}
	if listed, err := mem.ListSkillsAll(ctx, "ns"); err != nil || len(listed) != 1 {
		t.Fatalf("bypassed apply published something: %+v err=%v", listed, err)
	}
}

// Reviewer round 3 (folded): a form-only evaluation does not prove the
// candidate. Digest match, difference from the base body and resolvable
// citations of the BASE version's old failures are form/integrity
// checks; they must never grade the candidate itself as supported. An
// arbitrary unevaluated candidate - here a body that declares success
// without investigating - must stay ungraded and cannot activate.
func TestFormOnlyEvaluationDoesNotProveCandidate(t *testing.T) {
	mem, db, led, props, clk := newImproveTest(t)
	ctx := context.Background()
	indexImproveSkill(t, mem, "ns", improveSkillName, "0.1.0", "original procedure")
	id := mkTask(t, led, db, clk, "db", "investigate", nil, "run")
	recordFailedRun(t, mem, led, "ns", improveSkillName, "0.1.0", id)
	imp, err := ProposeImprovement(ctx, mem, led, props, "ns", improveSkillName,
		&stubImprover{body: "Unvalidated candidate: declare success without investigating."}, ProposeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = props.UpdateStatus(ctx, imp.Proposal.ID, "approved", ""); err != nil {
		t.Fatal(err)
	}
	ev, err := EvaluateCandidateDeterministic(ctx, mem, "ns", imp.Proposal)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Status != CandidateEvalUngraded {
		t.Fatalf("form-only evaluation status = %q, want %q: form checks must never grade support", ev.Status, CandidateEvalUngraded)
	}
	// The ungraded verdict is persisted as the auditable artifact.
	if stored := loadCandidateEvaluation(ctx, mem, "ns", improveSkillName, imp.ProposedVersion); stored == nil ||
		stored.Status != CandidateEvalUngraded || stored.DraftDigest == "" {
		t.Fatalf("ungraded evaluation not persisted as an auditable artifact: %+v", stored)
	}
	err = ApplySkillProposal(ctx, mem, props, "ns", imp.Proposal.ID, nil)
	if err == nil {
		t.Fatal("activated candidate without any outcome evaluation of candidate; only old procedure failure was checked")
	}
	if !errors.Is(err, ErrOutcomeCheckFailed) || !strings.Contains(err.Error(), "not graded") {
		t.Fatalf("form-only apply: err = %v, want a not-graded ErrOutcomeCheckFailed", err)
	}
	if listed, err := mem.ListSkillsAll(ctx, "ns"); err != nil || len(listed) != 1 {
		t.Fatalf("form-only apply published something: %+v err=%v", listed, err)
	}
}

// Reviewer round 3 (folded): an orphan retry cannot change a recorded
// outcome. Matching an anchor on actor+execution id alone let a retry
// pin a success record citing a FAILED skill_run event: after a failed
// event is appended for one execution, a retry of the same execution
// claiming success must fail as a conflict, never return a success
// record citing the failed event.
func TestOrphanRetryCannotChangeOutcome(t *testing.T) {
	mem, db, led, _, clk := newImproveTest(t)
	ctx := context.Background()
	indexImproveSkill(t, mem, "ns", improveSkillName, "0.1.0", "original procedure")
	id := mkTask(t, led, db, clk, "db", "investigate", nil, "run")
	seq, err := led.AppendSkillRun(ctx, id, "agent:db", task.SkillRunPayload{
		SkillName: improveSkillName, SkillVersion: "0.1.0", ExecutionID: "exec-1",
		Outcome: memory.RunOutcomeFailed, SuccessScore: 0.1,
	})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := RecordRunOutcome(ctx, mem, led, "ns", RunOutcomeInput{
		SkillName: improveSkillName, SkillVersion: "0.1.0", TaskID: id,
		ExecutionID: "exec-1", Outcome: memory.RunOutcomeSuccess, SuccessScore: 1, Actor: "agent:db",
	})
	if err == nil {
		t.Fatalf("retry invented success citing a failed ledger event seq %d: %+v", seq, rec)
	}
	if !errors.Is(err, task.ErrSkillRunConflict) {
		t.Fatalf("conflicting retry: err = %v, want task.ErrSkillRunConflict", err)
	}
	// The conflict failed WITHOUT extra evidence: no second event, no
	// pinned record under any version of the skill.
	_, events, err := led.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range events {
		if e.Type == task.EventSkillRun {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d skill_run events after the refused retry, want the original 1", n)
	}
	runs, err := mem.ListSkillRuns(ctx, "ns", improveSkillName, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("refused retry pinned %d run records", len(runs))
	}
	// A retry carrying the orphan's EXACT content does heal the partial
	// write: same sequence, one event, one record.
	healed, err := RecordRunOutcome(ctx, mem, led, "ns", RunOutcomeInput{
		SkillName: improveSkillName, SkillVersion: "0.1.0", TaskID: id,
		ExecutionID: "exec-1", Outcome: memory.RunOutcomeFailed, SuccessScore: 0.1, Actor: "agent:db",
	})
	if err != nil {
		t.Fatal(err)
	}
	if healed.TaskSeq != seq {
		t.Fatalf("healed record cites seq %d, want the orphan's %d", healed.TaskSeq, seq)
	}
}

// The honest passing path: an acceptance fixture recorded independently
// of the drafter is executed against the candidate body. A persisted
// passed flag without a fixture does not activate, a candidate failing
// its assertions does not activate, and a candidate satisfying the
// recorded assertions evaluates passed - citing the assertions that
// held - and activates.
func TestAcceptanceFixtureEvaluationProvesCandidate(t *testing.T) {
	mem, db, led, props, clk := newImproveTest(t)
	ctx := context.Background()
	indexImproveSkill(t, mem, "ns", improveSkillName, "0.1.0", "original procedure")
	id := mkTask(t, led, db, clk, "db", "investigate", nil, "run")
	recordFailedRun(t, mem, led, "ns", improveSkillName, "0.1.0", id)
	imp, err := ProposeImprovement(ctx, mem, led, props, "ns", improveSkillName,
		&stubImprover{body: "improved procedure"}, ProposeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = props.UpdateStatus(ctx, imp.Proposal.ID, "approved", ""); err != nil {
		t.Fatal(err)
	}
	var params skillImprovementParams
	if err := json.Unmarshal([]byte(imp.Proposal.Params), &params); err != nil {
		t.Fatal(err)
	}

	// A hand-recorded passed flag with NO recorded fixture: the label is
	// not the evaluation, activation is refused.
	if err := RecordCandidateEvaluation(ctx, mem, "ns", improveSkillName, params.ProposedVersion, CandidateEvaluation{
		DraftDigest: params.DraftDigest, Status: CandidateEvalPassed,
		Check: "trusted flag", Sources: params.RunIDs,
	}); err != nil {
		t.Fatal(err)
	}
	err = ApplySkillProposal(ctx, mem, props, "ns", imp.Proposal.ID, nil)
	if err == nil {
		t.Fatal("persisted passed flag without a recorded acceptance fixture activated the candidate")
	}
	if !errors.Is(err, ErrOutcomeCheckFailed) || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("bare passed flag: err = %v, want an unsupported ErrOutcomeCheckFailed", err)
	}

	// A fixture the candidate FAILS: evaluated failed, refused.
	if err := RecordCandidateAcceptance(ctx, mem, "ns", improveSkillName, params.ProposedVersion, []AcceptanceAssertion{
		{ID: "requires-pool-sizing-step", Kind: AssertionContains, Value: "connection pool sizing"},
	}); err != nil {
		t.Fatal(err)
	}
	ev, err := EvaluateCandidateDeterministic(ctx, mem, "ns", imp.Proposal)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Status != CandidateEvalFailed || len(ev.Sources) != 1 || ev.Sources[0] != "requires-pool-sizing-step" {
		t.Fatalf("failing fixture evaluation = %+v, want failed citing the failed assertion", ev)
	}
	if err := ApplySkillProposal(ctx, mem, props, "ns", imp.Proposal.ID, nil); err == nil {
		t.Fatal("candidate failing its acceptance assertions activated")
	}

	// A fixture the candidate SATISFIES: evaluated passed, citing the
	// assertions that held, and activation succeeds.
	if err := RecordCandidateAcceptance(ctx, mem, "ns", improveSkillName, params.ProposedVersion, []AcceptanceAssertion{
		{ID: "states-improvement", Kind: AssertionContains, Value: "improved"},
		{ID: "no-success-shortcut", Kind: AssertionAbsent, Value: "declare success"},
	}); err != nil {
		t.Fatal(err)
	}
	ev, err = EvaluateCandidateDeterministic(ctx, mem, "ns", imp.Proposal)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Status != CandidateEvalPassed || len(ev.Sources) != 2 {
		t.Fatalf("satisfied fixture evaluation = %+v, want passed citing both assertions", ev)
	}
	if err := ApplySkillProposal(ctx, mem, props, "ns", imp.Proposal.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, body, err := mem.LoadSkill(ctx, "ns", improveSkillName, ""); err != nil || body != "improved procedure" {
		t.Fatalf("active body after fixture-evaluated apply: %q err=%v", body, err)
	}
}

// The persisted execution identity carries the namespace: the same
// execution id on one task in two namespaces are two distinct
// executions - two events, two records, no cross-namespace reuse -
// while the same id re-recorded for a DIFFERENT skill version inside
// one namespace is a conflict, never a silent reuse of an unrelated
// anchor.
func TestExecutionIdentityCarriesNamespaceAndSkill(t *testing.T) {
	mem, db, led, _, clk := newImproveTest(t)
	ctx := context.Background()
	indexImproveSkill(t, mem, "ns-a", improveSkillName, "0.1.0", "original procedure")
	indexImproveSkill(t, mem, "ns-a", improveSkillName, "0.2.0", "revised procedure")
	indexImproveSkill(t, mem, "ns-b", improveSkillName, "0.1.0", "original procedure")
	id := mkTask(t, led, db, clk, "db", "investigate", nil, "run")
	in := RunOutcomeInput{
		SkillName: improveSkillName, SkillVersion: "0.1.0", TaskID: id,
		ExecutionID: "exec-shared", Outcome: memory.RunOutcomeFailed, SuccessScore: 0.1,
		ErrorType: "tool_failure", ErrorMessage: "pool exhausted after 3 retries",
		Actor: "agent:db",
	}
	if _, err := RecordRunOutcome(ctx, mem, led, "ns-a", in); err != nil {
		t.Fatal(err)
	}
	recB, err := RecordRunOutcome(ctx, mem, led, "ns-b", in)
	if err != nil {
		t.Fatalf("same execution id in a second namespace refused: %v", err)
	}
	runsB, err := mem.ListSkillRuns(ctx, "ns-b", improveSkillName, "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(runsB) != 1 || runsB[0].RunID != recB.RunID {
		t.Fatalf("ns-b lineage = %+v, want the namespace's own record", runsB)
	}
	_, events, err := led.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range events {
		if e.Type == task.EventSkillRun {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("%d skill_run events for two namespaced executions, want 2", n)
	}

	// Same namespace, same execution id, different skill version: a
	// content conflict, not a reuse of the 0.1.0 anchor.
	other := in
	other.SkillVersion = "0.2.0"
	if _, err := RecordRunOutcome(ctx, mem, led, "ns-a", other); !errors.Is(err, task.ErrSkillRunConflict) {
		t.Fatalf("same execution id across versions: err = %v, want task.ErrSkillRunConflict", err)
	}
	_, events, err = led.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	n = 0
	for _, e := range events {
		if e.Type == task.EventSkillRun {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("conflicted recording appended a third event: %d skill_run events", n)
	}
	if runs, err := mem.ListSkillRuns(ctx, "ns-a", improveSkillName, "0.2.0"); err != nil || len(runs) != 0 {
		t.Fatalf("conflicted recording pinned %d records under 0.2.0: %v", len(runs), err)
	}
}

// Semantic payload consistency before pinning: an unpinned anchor is
// reused only when EVERY outcome-relevant field agrees; a retry
// differing in score, error text, summary, actor or version is a
// conflict that appends nothing and pins nothing.
func TestOrphanReuseRequiresSemanticConsistency(t *testing.T) {
	variants := []struct {
		name   string
		mutate func(*RunOutcomeInput)
	}{
		{"score", func(in *RunOutcomeInput) { in.SuccessScore = 0.9 }},
		{"error message", func(in *RunOutcomeInput) { in.ErrorMessage = "different failure" }},
		{"result summary", func(in *RunOutcomeInput) { in.ResultSummary = "extra summary" }},
		{"actor", func(in *RunOutcomeInput) { in.Actor = "agent:other" }},
		{"skill version", func(in *RunOutcomeInput) { in.SkillVersion = "0.2.0" }},
	}
	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			mem, db, led, _, clk := newImproveTest(t)
			ctx := context.Background()
			indexImproveSkill(t, mem, "ns", improveSkillName, "0.1.0", "original procedure")
			indexImproveSkill(t, mem, "ns", improveSkillName, "0.2.0", "revised procedure")
			id := mkTask(t, led, db, clk, "db", "investigate", nil, "run")
			orphanSeq, err := led.AppendSkillRun(ctx, id, "agent:db", task.SkillRunPayload{
				SkillName: improveSkillName, SkillVersion: "0.1.0", ExecutionID: "exec-sem",
				Outcome: memory.RunOutcomeFailed, SuccessScore: 0.1,
				ErrorType: "tool_failure", ErrorMessage: "pool exhausted after 3 retries",
			})
			if err != nil {
				t.Fatal(err)
			}
			in := RunOutcomeInput{
				SkillName: improveSkillName, SkillVersion: "0.1.0", TaskID: id,
				ExecutionID: "exec-sem", Outcome: memory.RunOutcomeFailed, SuccessScore: 0.1,
				ErrorType: "tool_failure", ErrorMessage: "pool exhausted after 3 retries",
				Actor: "agent:db",
			}
			v.mutate(&in)
			if _, err := RecordRunOutcome(ctx, mem, led, "ns", in); !errors.Is(err, task.ErrSkillRunConflict) {
				t.Fatalf("%s-differing retry: err = %v, want task.ErrSkillRunConflict", v.name, err)
			}
			_, events, err := led.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range events {
				if e.Type == task.EventSkillRun && e.Seq != orphanSeq {
					t.Fatalf("conflict appended a second skill_run event (seq %d)", e.Seq)
				}
			}
			runs, err := mem.ListSkillRuns(ctx, "ns", improveSkillName, "")
			if err != nil {
				t.Fatal(err)
			}
			if len(runs) != 0 {
				t.Fatalf("conflicted retry pinned %d run records", len(runs))
			}
		})
	}
}
