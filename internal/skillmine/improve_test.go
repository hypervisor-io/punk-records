package skillmine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/policy"
	"github.com/hypervisor-io/punk-records/internal/store"
	"github.com/hypervisor-io/punk-records/internal/task"
)

// Task S02 red proofs and acceptance: run outcomes stay pinned to the
// exact version they ran, improvement proposals are separate from
// approval and application, an unapproved draft has no authority over
// active procedures, activation passes an outcome check, and applying a
// proposal is idempotent.

func newImproveTest(t *testing.T) (*memory.Store, *store.DB, *task.Ledger, *policy.Proposals, *fakeClock) {
	t.Helper()
	db, err := store.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.MigrateUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	clk := &fakeClock{t: time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)}
	mem := memory.New(db, clk.Now)
	led := task.NewLedger(db, clk.Now)
	props := policy.NewProposals(db, clk.Now)
	return mem, db, led, props, clk
}

func indexImproveSkill(t *testing.T, mem *memory.Store, ns, name, version, body string) memory.SkillMeta {
	t.Helper()
	meta := memory.SkillMeta{
		Name:        name,
		Version:     version,
		Description: "Diagnose database connection saturation and pool exhaustion after deploys",
		Tools:       []string{"incidents__get_incident"},
		Scope:       "incident",
		Source:      memory.SkillSourceAuthored,
		Active:      true,
	}
	if err := mem.IndexSkill(context.Background(), ns, meta, body); err != nil {
		t.Fatal(err)
	}
	return meta
}

// recordFailedRun drives the full path: ledger event first, then the
// memory-plane run record pinned to the exact version. The execution id
// is derived from the task, so helper calls across tests never collide
// within a task's lineage.
func recordFailedRun(t *testing.T, mem *memory.Store, led *task.Ledger, ns, name, version, taskID string) *memory.SkillRunRecord {
	t.Helper()
	rec, err := RecordRunOutcome(context.Background(), mem, led, ns, RunOutcomeInput{
		SkillName:    name,
		SkillVersion: version,
		TaskID:       taskID,
		ExecutionID:  "exec-" + taskID,
		Outcome:      memory.RunOutcomeFailed,
		SuccessScore: 0.1,
		ErrorType:    "tool_failure",
		ErrorMessage: "pool exhausted after 3 retries",
		Actor:        "agent:db",
	})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// evalCandidateDeterministically records an explicit acceptance fixture
// for the proposal's candidate - independent expectations the evaluator
// executes against the candidate body - and then runs the package's
// deterministic evaluator. Support requires assertions actually
// executed against the content, so the passing path always supplies a
// real fixture first; form checks alone stay ungraded.
func evalCandidateDeterministically(t *testing.T, mem *memory.Store, ns string, pr *policy.Proposal) {
	t.Helper()
	var params skillImprovementParams
	if err := json.Unmarshal([]byte(pr.Params), &params); err != nil {
		t.Fatal(err)
	}
	if err := RecordCandidateAcceptance(context.Background(), mem, ns, pr.Target, params.ProposedVersion, []AcceptanceAssertion{
		{ID: "states-improvement", Kind: AssertionContains, Value: "improved"},
		{ID: "no-success-shortcut", Kind: AssertionAbsent, Value: "declare success"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := EvaluateCandidateDeterministic(context.Background(), mem, ns, pr); err != nil {
		t.Fatal(err)
	}
}

type stubImprover struct {
	body       string
	rationale  string
	confidence float64
	seenRuns   []memory.SkillRunRecord
	seenBody   string
}

func (s *stubImprover) Draft(_ context.Context, _ memory.SkillMeta, body string, runs []memory.SkillRunRecord) (string, string, float64, error) {
	s.seenRuns = runs
	s.seenBody = body
	return s.body, s.rationale, s.confidence, nil
}

type staticChecker struct{ verdict OutcomeVerdict }

func (c staticChecker) CheckOutcome(context.Context, OutcomeInput) (OutcomeVerdict, error) {
	return c.verdict, nil
}

func boolPtr(b bool) *bool { return &b }

const improveSkillName = "db-connection-triage"

// countRows counts stored revisions under a key prefix, so the double-
// apply proof can show the second apply writes nothing at all.
func countRows(t *testing.T, db *store.DB, prefix string) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRowContext(context.Background(), db.Rebind(
		`SELECT count(*) FROM memories WHERE key LIKE ?`), prefix+"%").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Red proof (a): a run recorded against v1 still references v1 - its
// key, its identity digest and the loadable procedure - after a new
// version has been proposed from its outcome.
func TestSkillRunPinsOriginalVersionAfterProposal(t *testing.T) {
	mem, db, led, props, clk := newImproveTest(t)
	ctx := context.Background()
	ns := "ns"
	indexImproveSkill(t, mem, ns, improveSkillName, "0.1.0", "original procedure")
	taskID := mkTask(t, led, db, clk, "db", "investigate", nil, "triage run")
	rec := recordFailedRun(t, mem, led, ns, improveSkillName, "0.1.0", taskID)

	imp, err := ProposeImprovement(ctx, mem, led, props, ns, improveSkillName,
		&stubImprover{body: "improved procedure", rationale: "runs keep failing", confidence: 0.8}, ProposeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if imp == nil || imp.ProposedVersion == "" || imp.ProposedVersion == "0.1.0" {
		t.Fatalf("proposal = %+v, want a new proposed version", imp)
	}
	if len(imp.RunIDs) != 1 || imp.RunIDs[0] != rec.RunID {
		t.Fatalf("cited runs = %v, want [%s]", imp.RunIDs, rec.RunID)
	}

	// The run still references the original version: same key, same
	// version attribution, same identity digest as the published v1.
	runs, err := mem.ListSkillRuns(ctx, ns, improveSkillName, "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].RunID != rec.RunID || runs[0].SkillVersion != "0.1.0" {
		t.Fatalf("run lineage changed by the proposal: %+v", runs)
	}
	ident, err := mem.SkillIdentity(ctx, ns, improveSkillName, "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if runs[0].SkillIdentity != ident {
		t.Fatalf("run identity %q != published v1 identity %q", runs[0].SkillIdentity, ident)
	}
	if _, body, err := mem.LoadSkill(ctx, ns, improveSkillName, "0.1.0"); err != nil || body != "original procedure" {
		t.Fatalf("v1 body after proposal: %q err=%v", body, err)
	}

	// The proposed version is a draft, not a publication: nothing under
	// /skill-drafts/ is discoverable or loadable.
	if listed, err := mem.ListSkillsAll(ctx, ns); err != nil || len(listed) != 1 {
		t.Fatalf("draft leaked into the skill listing: %+v err=%v", listed, err)
	}
	if keys, err := mem.ListKeys(ctx, ns, "/skill-drafts/"+improveSkillName+"/"); err != nil ||
		len(keys) != 1 || !strings.HasPrefix(keys[0], "/skill-drafts/"+improveSkillName+"/"+imp.ProposedVersion) {
		t.Fatalf("draft keys = %v err=%v", keys, err)
	}
}

// Red proof (b): an unapproved draft cannot replace the active
// procedure. Applying at every non-approved lifecycle state fails with
// ErrProposalNotApproved and leaves discovery, load and the draft
// untouched.
func TestUnapprovedDraftCannotReplaceActiveProcedure(t *testing.T) {
	mem, db, led, props, clk := newImproveTest(t)
	ctx := context.Background()
	ns := "ns"
	indexImproveSkill(t, mem, ns, improveSkillName, "0.1.0", "original procedure")
	taskID := mkTask(t, led, db, clk, "db", "investigate", nil, "triage run")
	recordFailedRun(t, mem, led, ns, improveSkillName, "0.1.0", taskID)

	bodies := []string{"improved procedure a", "improved procedure b", "improved procedure c", "improved procedure d"}
	proposals := make([]*policy.Proposal, 0, len(bodies))
	for _, body := range bodies {
		imp, err := ProposeImprovement(ctx, mem, led, props, ns, improveSkillName,
			&stubImprover{body: body, rationale: "runs", confidence: 0.5}, ProposeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		proposals = append(proposals, imp.Proposal)
	}

	// proposed (as created), submitted, rejected, expired: none may apply.
	states := []string{"proposed", "submitted", "rejected", "expired"}
	for i, state := range states {
		if state != "proposed" {
			if _, err := props.UpdateStatus(ctx, proposals[i].ID, state, ""); err != nil {
				t.Fatal(err)
			}
		}
		err := ApplySkillProposal(ctx, mem, props, ns, proposals[i].ID, nil)
		if !errors.Is(err, ErrProposalNotApproved) {
			t.Fatalf("apply at %s: err = %v, want ErrProposalNotApproved", state, err)
		}
		if got, _ := props.Get(ctx, proposals[i].ID); got.ExternalRef != "" {
			t.Fatalf("apply at %s marked the proposal applied: %q", state, got.ExternalRef)
		}
	}

	// The active procedure is untouched and no draft is discoverable.
	hits, err := mem.SearchSkills(ctx, ns, "connection saturation", 10)
	if err != nil || len(hits) != 1 || hits[0].Version != "0.1.0" {
		t.Fatalf("search after refused applies: %+v err=%v", hits, err)
	}
	if _, body, err := mem.LoadSkill(ctx, ns, improveSkillName, ""); err != nil || body != "original procedure" {
		t.Fatalf("active body after refused applies: %q err=%v", body, err)
	}
	if listed, err := mem.ListSkillsAll(ctx, ns); err != nil || len(listed) != 1 {
		t.Fatalf("drafts leaked into the skill listing: %+v err=%v", listed, err)
	}
	if n := countRows(t, db, "/skills/"+improveSkillName+"/"); n != 1 {
		t.Fatalf("%d published discovery rows after refused applies, want 1", n)
	}
}

// Red proof (c): applying the same approved proposal twice has one
// effect - the second apply is a no-op that writes nothing.
func TestApplyApprovedProposalTwiceHasOneEffect(t *testing.T) {
	mem, db, led, props, clk := newImproveTest(t)
	ctx := context.Background()
	ns := "ns"
	indexImproveSkill(t, mem, ns, improveSkillName, "0.1.0", "original procedure")
	taskID := mkTask(t, led, db, clk, "db", "investigate", nil, "triage run")
	recordFailedRun(t, mem, led, ns, improveSkillName, "0.1.0", taskID)

	imp, err := ProposeImprovement(ctx, mem, led, props, ns, improveSkillName,
		&stubImprover{body: "improved procedure", rationale: "runs keep failing", confidence: 0.8}, ProposeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := props.UpdateStatus(ctx, imp.Proposal.ID, "approved", ""); err != nil {
		t.Fatal(err)
	}
	evalCandidateDeterministically(t, mem, ns, imp.Proposal)

	prefixes := []string{
		"/skills/" + improveSkillName + "/",
		"/skill-bodies/" + improveSkillName + "/",
		"/skill-identities/" + improveSkillName + "/",
		"/skill-drafts/" + improveSkillName + "/",
		"/skill-runs/" + improveSkillName + "/",
	}
	before := map[string]int64{}
	for _, p := range prefixes {
		before[p] = countRows(t, db, p)
	}

	if err := ApplySkillProposal(ctx, mem, props, ns, imp.Proposal.ID, nil); err != nil {
		t.Fatal(err)
	}

	// First apply: the proposed version is published and active, the
	// base version deactivated but still published, and the proposal is
	// marked applied.
	listed, err := mem.ListSkillsAll(ctx, ns)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 {
		t.Fatalf("after apply: %+v, want both versions published", listed)
	}
	for _, m := range listed {
		if m.Version == imp.ProposedVersion && !m.Active {
			t.Fatalf("proposed version not active: %+v", m)
		}
		if m.Version == "0.1.0" && m.Active {
			t.Fatalf("base version still active: %+v", m)
		}
	}
	if _, body, err := mem.LoadSkill(ctx, ns, improveSkillName, ""); err != nil || body != "improved procedure" {
		t.Fatalf("active body after apply: %q err=%v", body, err)
	}
	pr, err := props.Get(ctx, imp.Proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pr.ExternalRef != "applied:"+improveSkillName+"@"+imp.ProposedVersion {
		t.Fatalf("external_ref = %q, want the applied marker", pr.ExternalRef)
	}

	after := map[string]int64{}
	for _, p := range prefixes {
		after[p] = countRows(t, db, p)
	}
	// The first apply had exactly the one effect: the proposed
	// version's discovery, body and identity rows, plus the base
	// discovery document's deactivation revision. Drafts and runs are
	// untouched.
	for _, p := range prefixes {
		want := before[p]
		switch p {
		case "/skill-bodies/" + improveSkillName + "/", "/skill-identities/" + improveSkillName + "/":
			want++
		case "/skills/" + improveSkillName + "/":
			want += 2
		}
		if after[p] != want {
			t.Fatalf("first apply changed %s: %d -> %d, want %d", p, before[p], after[p], want)
		}
	}

	// Second apply: no-op.
	if err := ApplySkillProposal(ctx, mem, props, ns, imp.Proposal.ID, nil); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	for _, p := range prefixes {
		if n := countRows(t, db, p); n != after[p] {
			t.Fatalf("second apply wrote rows under %s: %d -> %d", p, after[p], n)
		}
	}
	pr2, err := props.Get(ctx, imp.Proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pr2.ExternalRef != pr.ExternalRef || pr2.Status != "approved" {
		t.Fatalf("second apply moved the proposal: %+v", pr2)
	}
	if _, body, err := mem.LoadSkill(ctx, ns, improveSkillName, ""); err != nil || body != "improved procedure" {
		t.Fatalf("active body after second apply: %q err=%v", body, err)
	}
}

// Proposals cite actual runs: with no qualifying outcome there is no
// proposal at all, and a proposal whose citations do not resolve to
// live run records is refused at apply time.
func TestProposalRequiresRealRuns(t *testing.T) {
	mem, db, led, props, clk := newImproveTest(t)
	ctx := context.Background()
	ns := "ns"
	indexImproveSkill(t, mem, ns, improveSkillName, "0.1.0", "original procedure")
	taskID := mkTask(t, led, db, clk, "db", "investigate", nil, "triage run")

	// No runs recorded: nothing to improve on, a normal nil result.
	imp, err := ProposeImprovement(ctx, mem, led, props, ns, improveSkillName,
		&stubImprover{body: "improved procedure"}, ProposeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if imp != nil {
		t.Fatalf("proposal without runs: %+v, want nil", imp)
	}

	recordFailedRun(t, mem, led, ns, improveSkillName, "0.1.0", taskID)
	real, err := ProposeImprovement(ctx, mem, led, props, ns, improveSkillName,
		&stubImprover{body: "improved procedure", rationale: "runs", confidence: 0.5}, ProposeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The drafter saw the real recorded run.
	stub := &stubImprover{body: "second improved procedure"}
	if _, err := ProposeImprovement(ctx, mem, led, props, ns, improveSkillName, stub, ProposeOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(stub.seenRuns) != 1 || stub.seenRuns[0].RunID == "" || stub.seenBody != "original procedure" {
		t.Fatalf("drafter evidence: runs=%+v body=%q", stub.seenRuns, stub.seenBody)
	}

	// A hand-forged proposal citing a fabricated run id: refused at
	// apply even though its draft exists.
	var params skillImprovementParams
	if err := json.Unmarshal([]byte(real.Proposal.Params), &params); err != nil {
		t.Fatal(err)
	}
	params.RunIDs = []string{"fabricated-task-99"}
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	forged, err := props.Create(ctx, taskID, ActionClassSkillImprove, improveSkillName, string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := props.UpdateStatus(ctx, forged.ID, "approved", ""); err != nil {
		t.Fatal(err)
	}
	if err := ApplySkillProposal(ctx, mem, props, ns, forged.ID, nil); !errors.Is(err, ErrUnresolvedRun) {
		t.Fatalf("apply with fabricated citation: err = %v, want ErrUnresolvedRun", err)
	}
	if listed, err := mem.ListSkillsAll(ctx, ns); err != nil || len(listed) != 1 {
		t.Fatalf("forged apply published something: %+v err=%v", listed, err)
	}
}

// Activation requires the E02-style outcome check: a vetoing checker
// blocks the apply (and the proposal stays retryable), and the
// deterministic default check independently refuses a proposal whose
// recorded base identity no longer matches the published version.
func TestActivationRequiresOutcomeCheck(t *testing.T) {
	mem, db, led, props, clk := newImproveTest(t)
	ctx := context.Background()
	ns := "ns"
	indexImproveSkill(t, mem, ns, improveSkillName, "0.1.0", "original procedure")
	taskID := mkTask(t, led, db, clk, "db", "investigate", nil, "triage run")
	recordFailedRun(t, mem, led, ns, improveSkillName, "0.1.0", taskID)

	imp, err := ProposeImprovement(ctx, mem, led, props, ns, improveSkillName,
		&stubImprover{body: "improved procedure", rationale: "runs", confidence: 0.5}, ProposeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := props.UpdateStatus(ctx, imp.Proposal.ID, "approved", ""); err != nil {
		t.Fatal(err)
	}
	evalCandidateDeterministically(t, mem, ns, imp.Proposal)

	veto := staticChecker{verdict: OutcomeVerdict{
		IdentityMatch: boolPtr(false), Justification: "identity drifted",
	}}
	if err := ApplySkillProposal(ctx, mem, props, ns, imp.Proposal.ID, veto); !errors.Is(err, ErrOutcomeCheckFailed) {
		t.Fatalf("vetoed apply: err = %v, want ErrOutcomeCheckFailed", err)
	}
	if listed, err := mem.ListSkillsAll(ctx, ns); err != nil || len(listed) != 1 {
		t.Fatalf("vetoed apply published something: %+v err=%v", listed, err)
	}
	pr, err := props.Get(ctx, imp.Proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pr.ExternalRef != "" {
		t.Fatalf("vetoed apply marked the proposal applied: %q", pr.ExternalRef)
	}

	// The deterministic default check passes a coherent proposal.
	if err := ApplySkillProposal(ctx, mem, props, ns, imp.Proposal.ID, nil); err != nil {
		t.Fatal(err)
	}

	// A proposal whose params claim a different base identity than the
	// published version carries is refused by the default check alone.
	// Its draft exists, so the refusal is the identity dimension itself.
	var params skillImprovementParams
	if err := json.Unmarshal([]byte(imp.Proposal.Params), &params); err != nil {
		t.Fatal(err)
	}
	params.BaseIdentity = "deadbeef"
	params.ProposedVersion = "rtampered0001"
	params.DraftKey = skillDraftKey(improveSkillName, params.ProposedVersion)
	if _, err := mem.Write(ctx, memory.WriteInput{
		Namespace: ns, Key: params.DraftKey, Body: "improved procedure",
		Attributes: map[string]any{"skill_name": improveSkillName, "skill_version": params.ProposedVersion},
		Writer:     "skill-improve",
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	tampered, err := props.Create(ctx, taskID, ActionClassSkillImprove, improveSkillName, string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := props.UpdateStatus(ctx, tampered.ID, "approved", ""); err != nil {
		t.Fatal(err)
	}
	if err := ApplySkillProposal(ctx, mem, props, ns, tampered.ID, nil); !errors.Is(err, ErrOutcomeCheckFailed) {
		t.Fatalf("tampered identity apply: err = %v, want ErrOutcomeCheckFailed", err)
	}
}
