package skillmine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/policy"
	"github.com/hypervisor-io/punk-records/internal/task"
)

// Cognee provenance: proposal-first skill improvement adapts
// cognee/modules/memify/skill_improvement.py (commit
// 78ff576559a7f75f65884c5bd90b22cdc790016e): failed runs select the
// candidates, a draft is generated from them, and activation happens
// only through an explicit, separately-approved apply step. Two
// mechanisms are deliberately different from cognee, which mutates its
// procedure store in place and applies improvements directly:
//
//   - A proposal never becomes the active procedure by itself. The
//     draft waits under /skill-drafts/ (disjoint from /skills/, so it
//     is invisible to discovery and unloadable) and only an approved,
//     outcome-checked apply publishes it - as a NEW content-derived
//     version, never as an overwrite.
//   - The base version stays published and immutable when the
//     improvement activates: runs recorded against it keep citing it
//     (identity digest, exact body), and a regression can revert to it.

const (
	// ActionClassSkillImprove marks proposals that would publish an
	// improved skill version; they ride the existing proposal lifecycle
	// (proposed -> approved -> applied marker), no schema changes.
	ActionClassSkillImprove = "skill_improve"
	// proposalMaxRuns caps how many runs one proposal cites; evidence
	// stays a bounded digest, never a trajectory dump.
	proposalMaxRuns = 20
	// skillImproveBodyMaxRunes bounds the proposed procedure the
	// outcome check accepts (metadata caps come from the store's
	// validation; this covers the body itself).
	skillImproveBodyMaxRunes = 64 * 1024
	// taskKindSkillImprovement is the ledger kind of a proposal's
	// anchoring task.
	taskKindSkillImprovement = "skill_improvement"
	// appliedRefPrefix marks an applied proposal's external_ref; the
	// proposals status CHECK has no "applied" state, so the marker rides
	// external_ref and keeps the apply idempotent with no migration.
	appliedRefPrefix = "applied:"
)

var (
	// ErrProposalNotApproved: activation refused because the proposal is
	// not in the approved state (proposed, submitted, rejected and
	// expired all fail here) - a skill draft has no authority to edit
	// active procedures on its own.
	ErrProposalNotApproved = errors.New("skillmine: improvement proposal is not approved")
	// ErrOutcomeCheckFailed: the outcome check vetoed activation; the
	// proposal stays approved-but-unapplied and may be retried.
	ErrOutcomeCheckFailed = errors.New("skillmine: outcome check blocked activation")
	// ErrUnresolvedRun: the proposal cites a run id with no live run
	// record - proposals must cite actual recorded outcomes.
	ErrUnresolvedRun = errors.New("skillmine: proposal cites unknown skill run")
	// ErrDraftDigestMismatch: the draft under /skill-drafts/ no longer
	// matches the content the proposal was approved for. Approval binds
	// to the exact draft digest, so the draft cannot be swapped after
	// approval to publish a different procedure under the approved
	// content-derived version.
	ErrDraftDigestMismatch = errors.New("skillmine: draft content does not match the approved proposal")
)

// ImprovementDrafter turns a skill's observed runs into a proposed next
// revision. Implementations may be model-backed; the model adapter
// lives with the other LLM adapters in cmd and is constructed only with
// explicit budget configuration. Tests use deterministic fakes.
type ImprovementDrafter interface {
	Draft(ctx context.Context, meta memory.SkillMeta, body string, runs []memory.SkillRunRecord) (proposedBody, rationale string, confidence float64, err error)
}

// ProposeOptions tunes one ProposeImprovement call.
type ProposeOptions struct {
	// BaseVersion is the exact version to improve; empty resolves the
	// single active version (ambiguous actives are an error).
	BaseVersion string
	// MinSuccessScore lets underperforming successes count as evidence
	// too: successful runs scoring below it qualify. 0 keeps only
	// failed and error runs.
	MinSuccessScore float64
	// MaxRuns caps the cited evidence; 0 defaults to proposalMaxRuns.
	MaxRuns int
	// Actor is the ledger actor; empty defaults to "skillmine".
	Actor string
}

// SkillImprovement is one generated, unapplied proposal: a draft under
// /skill-drafts/, its citing evidence, and the policy proposal that
// carries it through approval.
type SkillImprovement struct {
	Proposal        *policy.Proposal
	TaskID          string
	SkillName       string
	BaseVersion     string
	ProposedVersion string
	RunIDs          []string
	Rationale       string
	Confidence      float64
}

// skillImprovementParams is the JSON payload stored on the proposal row
// (params carries everything apply needs, including a snapshot of the
// base metadata, so activation never depends on the base version still
// being active). Namespace binds the proposal to the namespace whose
// runs generated it (the ledger's open-task dedup is global, so the
// external ref and every apply-time check are namespace-scoped too);
// DraftDigest pins the approval to the exact draft content, verified at
// application time before any checker runs.
type skillImprovementParams struct {
	Namespace       string   `json:"namespace"`
	DraftKey        string   `json:"draft_key"`
	DraftDigest     string   `json:"draft_digest"`
	BaseVersion     string   `json:"base_version"`
	ProposedVersion string   `json:"proposed_version"`
	BaseIdentity    string   `json:"base_identity"`
	RunIDs          []string `json:"run_ids"`
	Rationale       string   `json:"rationale,omitempty"`
	Confidence      float64  `json:"confidence,omitempty"`
	Description     string   `json:"description"`
	Tools           []string `json:"tools,omitempty"`
	Scope           string   `json:"scope,omitempty"`
}

// skillDraftKey is where an unapplied draft body waits for approval.
// The /skill-drafts/ prefix is deliberately disjoint from /skills/:
// discovery reads only /skills/%, so an unapproved draft can never
// surface in SearchSkills/ListSkills or load through LoadSkill.
func skillDraftKey(name, version string) string {
	return "/skill-drafts/" + name + "/" + version
}

// improvementVersion pins a proposed revision to its content, the same
// derivation draftVersion uses for mined drafts ("r" + 12 hex of
// SHA-256): regenerating an identical draft resolves to the same
// version (idempotent proposals), changed evidence producing a changed
// draft resolves to a new one, and a proposal can never collide with a
// shipped semantic version.
func improvementVersion(body string) string {
	sum := sha256.Sum256([]byte(body))
	return "r" + hex.EncodeToString(sum[:])[:12]
}

// selectImprovementRuns picks the evidence for one proposal: failed and
// error runs always qualify, successful runs qualify when their score
// is below MinSuccessScore. Newest first, capped at MaxRuns (0 ->
// proposalMaxRuns).
func selectImprovementRuns(runs []memory.SkillRunRecord, opts ProposeOptions) []memory.SkillRunRecord {
	max := opts.MaxRuns
	if max <= 0 {
		max = proposalMaxRuns
	}
	out := make([]memory.SkillRunRecord, 0, max)
	for i := len(runs) - 1; i >= 0 && len(out) < max; i-- {
		r := runs[i]
		if r.Outcome == memory.RunOutcomeSuccess && r.SuccessScore >= opts.MinSuccessScore {
			continue
		}
		out = append(out, r)
	}
	return out
}

// ProposeImprovement generates an improvement proposal for one skill
// from its recorded run outcomes. Runs are selected (failed/error, or
// successful below MinSuccessScore, newest first, capped); no
// qualifying runs is a normal nil,nil result, not an error. The drafter
// produces the proposed procedure from the base version and the
// selected evidence; every cited run is a real /skill-runs/ row the
// drafter actually saw. The draft waits under /skill-drafts/ keyed by
// its content-derived version, the anchoring task is submitted with a
// skill-improve external ref (one open task per proposed version, so a
// duplicate propose returns the existing work), and the policy proposal
// is created in status proposed - generation is fully separate from
// approval and application, and nothing here touches an active
// procedure.
func ProposeImprovement(ctx context.Context, mem *memory.Store, ledger *task.Ledger, props *policy.Proposals, ns, name string, d ImprovementDrafter, opts ProposeOptions) (*SkillImprovement, error) {
	if d == nil {
		return nil, errors.New("skillmine: propose improvement requires a drafter")
	}
	base, body, err := mem.LoadSkill(ctx, ns, name, opts.BaseVersion)
	if err != nil {
		return nil, fmt.Errorf("propose improvement for %s: %w", name, err)
	}
	runs, err := mem.ListSkillRuns(ctx, ns, name, base.Version)
	if err != nil {
		return nil, fmt.Errorf("propose improvement for %s: %w", name, err)
	}
	selected := selectImprovementRuns(runs, opts)
	if len(selected) == 0 {
		return nil, nil // no qualifying outcomes: nothing to propose
	}
	proposedBody, rationale, confidence, err := d.Draft(ctx, base, body, selected)
	if err != nil {
		return nil, fmt.Errorf("draft improvement for %s: %w", name, err)
	}
	if strings.TrimSpace(proposedBody) == "" {
		return nil, fmt.Errorf("draft improvement for %s: proposed procedure is empty", name)
	}
	if proposedBody == body {
		return nil, fmt.Errorf("draft improvement for %s: proposed procedure does not differ from %s", name, base.Version)
	}
	baseIdentity, err := mem.SkillIdentity(ctx, ns, name, base.Version)
	if err != nil {
		return nil, fmt.Errorf("propose improvement for %s: %w", name, err)
	}
	proposedVersion := improvementVersion(proposedBody)
	runIDs := make([]string, len(selected))
	for i, r := range selected {
		runIDs[i] = r.RunID
	}
	actor := opts.Actor
	if actor == "" {
		actor = "skillmine"
	}
	params := skillImprovementParams{
		Namespace:       ns,
		DraftKey:        skillDraftKey(name, proposedVersion),
		DraftDigest:     draftDigest(proposedBody),
		BaseVersion:     base.Version,
		ProposedVersion: proposedVersion,
		BaseIdentity:    baseIdentity,
		RunIDs:          runIDs,
		Rationale:       rationale,
		Confidence:      confidence,
		Description:     base.Description,
		Tools:           base.Tools,
		Scope:           base.Scope,
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("propose improvement for %s: %w", name, err)
	}

	// The draft ships before the task and proposal rows: whenever a
	// proposal exists, its draft exists. A failure here or between the
	// writes leaves at most an invisible orphan draft that the next
	// identical propose overwrites.
	if _, err := mem.Write(ctx, memory.WriteInput{
		Namespace: ns, Key: params.DraftKey, Body: proposedBody,
		Attributes: map[string]any{
			"skill_name":         name,
			"skill_version":      proposedVersion,
			"skill_base_version": base.Version,
			"skill_draft_digest": params.DraftDigest,
		},
		Writer: "skill-improve", Author: actor,
	}); err != nil {
		return nil, fmt.Errorf("store improvement draft: %w", err)
	}

	// The external ref carries the namespace: the ledger's open-task
	// dedup is global, and the same skill/body improving in two
	// namespaces are two distinct proposals citing distinct runs.
	externalRef := fmt.Sprintf("skill-improve:%s:%s@%s", ns, name, proposedVersion)
	tk, created, err := ledger.Submit(ctx, task.SubmitInput{
		Source: "skillmine", Kind: taskKindSkillImprovement,
		ExternalRef: externalRef, Actor: actor,
	})
	if err != nil {
		return nil, fmt.Errorf("anchor improvement task: %w", err)
	}
	if !created {
		// Open-task dedup: an identical proposal is already anchored to
		// this external ref - return it instead of stacking a duplicate.
		if prior := findSkillProposal(ctx, props, tk.ID, ns, proposedVersion); prior != nil {
			return skillImprovementFromProposal(prior), nil
		}
	}
	pr, err := props.Create(ctx, tk.ID, ActionClassSkillImprove, name, string(raw))
	if err != nil {
		return nil, fmt.Errorf("create improvement proposal: %w", err)
	}
	return &SkillImprovement{
		Proposal:        pr,
		TaskID:          tk.ID,
		SkillName:       name,
		BaseVersion:     base.Version,
		ProposedVersion: proposedVersion,
		RunIDs:          runIDs,
		Rationale:       rationale,
		Confidence:      confidence,
	}, nil
}

// findSkillProposal returns the task's proposal carrying the given
// proposed version for the namespace, if any.
func findSkillProposal(ctx context.Context, props *policy.Proposals, taskID, ns, proposedVersion string) *policy.Proposal {
	list, err := props.ListByTask(ctx, taskID)
	if err != nil {
		return nil
	}
	for i := range list {
		var p skillImprovementParams
		if json.Unmarshal([]byte(list[i].Params), &p) == nil &&
			p.Namespace == ns && p.ProposedVersion == proposedVersion {
			return &list[i]
		}
	}
	return nil
}

func skillImprovementFromProposal(pr *policy.Proposal) *SkillImprovement {
	var p skillImprovementParams
	_ = json.Unmarshal([]byte(pr.Params), &p)
	return &SkillImprovement{
		Proposal:        pr,
		TaskID:          pr.TaskID,
		SkillName:       pr.Target,
		BaseVersion:     p.BaseVersion,
		ProposedVersion: p.ProposedVersion,
		RunIDs:          p.RunIDs,
		Rationale:       p.Rationale,
		Confidence:      p.Confidence,
	}
}

// OutcomeCitation is one cited run resolved for the outcome check; Run
// is nil when the citation does not resolve to a live run record.
type OutcomeCitation struct {
	RunID string
	Run   *memory.SkillRunRecord
}

// OutcomeInput is everything an outcome check may use to decide whether
// an approved proposal may activate. The default check is
// deterministic; a model-backed check plugs in through the same
// interface and is only ever constructed with explicit budget
// configuration.
type OutcomeInput struct {
	SkillName       string
	BaseVersion     string
	ProposedVersion string
	BaseBody        string
	ProposedBody    string
	BaseIdentity    string                // identity digest recorded in the proposal
	CurrentIdentity string                // live identity digest of the base version now
	Runs            []OutcomeCitation     // the proposal's cited evidence
	CandidateEval   *CandidateEvaluation  // stored, digest-bound evaluation of the proposed procedure (nil = none)
	Acceptance      []AcceptanceAssertion // recorded acceptance fixture for the proposed version (nil = none)
}

// Candidate evaluation statuses for CandidateEvaluation.Status.
const (
	CandidateEvalPassed   = "passed"   // an executed evaluation found the candidate supported
	CandidateEvalFailed   = "failed"   // an executed evaluation found the candidate unsupported
	CandidateEvalUngraded = "ungraded" // the candidate's own behavior was never evaluated; support unknown
)

// CandidateEvaluation is one explicit evaluation of a proposed
// procedure's OWN behavior, recorded in the memory plane and pinned to
// the exact draft digest it graded: Status is the verdict (passed,
// failed or ungraded), Check names the check that produced it, and
// Sources cites the evidence the check used (the ids of the
// acceptance assertions it executed). Support never comes from form or
// citation validity: a passed verdict requires assertions actually
// executed against the candidate body, an absent or unexecutable
// evaluation stays ungraded, and the apply gate re-runs the recorded
// fixture itself - the persisted flag alone is never the evaluation.
// A proposal whose candidate was never evaluated, or whose evaluation
// does not match the approved draft digest, cannot activate.
type CandidateEvaluation struct {
	DraftDigest string
	Status      string
	Check       string
	Sources     []string
}

func skillEvalKey(name, version string) string {
	return "/skill-evals/" + name + "/" + version
}

// RecordCandidateEvaluation pins an explicit candidate evaluation to the
// proposed version, bound to the exact draft digest it graded. The
// artifact is the auditable evidence the activation gate requires:
// status, the check that decided it and the sources it cites. A passed
// evaluation must cite at least one source - and even then the flag is
// not self-sufficient: activation additionally re-runs the acceptance
// fixture recorded for the version against the candidate body, so a
// hand-recorded passed label without a re-runnable evaluation never
// activates anything.
func RecordCandidateEvaluation(ctx context.Context, mem *memory.Store, ns, name, version string, ev CandidateEvaluation) error {
	if name == "" || version == "" {
		return errors.New("skillmine: candidate evaluation requires a skill name and version")
	}
	if len([]rune(ev.DraftDigest)) != 64 {
		return errors.New("skillmine: candidate evaluation requires the 64-hex draft digest it graded")
	}
	switch ev.Status {
	case CandidateEvalPassed, CandidateEvalFailed, CandidateEvalUngraded:
	default:
		return fmt.Errorf("skillmine: candidate evaluation status %q: want passed, failed or ungraded", ev.Status)
	}
	if ev.Check == "" {
		return errors.New("skillmine: candidate evaluation requires the check that produced it")
	}
	for _, s := range ev.Sources {
		if s == "" {
			return errors.New("skillmine: candidate evaluation sources must be non-empty")
		}
	}
	if ev.Status == CandidateEvalPassed && len(ev.Sources) == 0 {
		return errors.New("skillmine: a passed candidate evaluation must cite its sources")
	}
	sources, err := json.Marshal(ev.Sources)
	if err != nil {
		return fmt.Errorf("record candidate evaluation: %w", err)
	}
	if _, err := mem.Write(ctx, memory.WriteInput{
		Namespace: ns, Key: skillEvalKey(name, version),
		Body: fmt.Sprintf("candidate %s: %s (sources: %s)", ev.Status, ev.Check, strings.Join(ev.Sources, ", ")),
		Attributes: map[string]any{
			"skill_name":    name,
			"skill_version": version,
			"eval_digest":   ev.DraftDigest,
			"eval_status":   ev.Status,
			"eval_check":    ev.Check,
			"eval_sources":  string(sources),
		},
		Writer: "skill-eval",
	}); err != nil {
		return fmt.Errorf("record candidate evaluation: %w", err)
	}
	return nil
}

// loadCandidateEvaluation projects the recorded evaluation for one
// proposed version, if any; nil when none is recorded or the artifact
// is malformed.
func loadCandidateEvaluation(ctx context.Context, mem *memory.Store, ns, name, version string) *CandidateEvaluation {
	facts, err := mem.Recall(ctx, ns, skillEvalKey(name, version), 1)
	if err != nil || len(facts) == 0 || facts[0].Key != skillEvalKey(name, version) {
		return nil
	}
	a := facts[0].Attributes
	if a == nil {
		return nil
	}
	digest, _ := a["eval_digest"].(string)
	status, _ := a["eval_status"].(string)
	check, _ := a["eval_check"].(string)
	rawSources, _ := a["eval_sources"].(string)
	var sources []string
	if json.Unmarshal([]byte(rawSources), &sources) != nil {
		sources = nil
	}
	if digest == "" || status == "" || check == "" {
		return nil
	}
	return &CandidateEvaluation{DraftDigest: digest, Status: status, Check: check, Sources: sources}
}

// Acceptance assertion kinds for AcceptanceAssertion.Kind: the only
// deterministic content expectations the model-free evaluator executes.
const (
	AssertionContains = "contains" // the candidate body must contain Value
	AssertionAbsent   = "absent"   // the candidate body must NOT contain Value
)

// Per-fixture caps, mirroring the run digest caps so a recorded
// acceptance fixture stays a closed-form bound.
const (
	acceptanceMaxAssertions = 64
	acceptanceIDMaxRunes    = 128
	acceptanceValueMaxRunes = 1024
)

// AcceptanceAssertion is one explicit, deterministic expectation of a
// proposed procedure's CONTENT. The fixture of assertions recorded for
// a proposed version is what a genuine evaluation executes against the
// candidate body: form checks (digest match, difference from the base,
// resolvable citations of old base-run failures) are integrity vetoes,
// never outcome support, and support requires running real assertions
// like these. Fixtures are supplied independently of the drafter that
// generated the candidate.
type AcceptanceAssertion struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// skillAcceptanceKey is where a proposed version's acceptance fixture
// waits. The /skill-acceptance/ prefix is disjoint from /skills/,
// /skill-drafts/ and /skill-evals/ so prefix recalls stay unambiguous.
func skillAcceptanceKey(name, version string) string {
	return "/skill-acceptance/" + name + "/" + version
}

// RecordCandidateAcceptance records the explicit acceptance fixture for
// one proposed version: the deterministic assertions an evaluation
// executes against the candidate body. It is deliberately separate from
// proposal generation and from the evaluation artifact - whoever demands
// activation states what the proposed procedure must contain or must
// not contain, the evaluator runs it, and the activation gate re-runs
// it before trusting a passed verdict. Recording replaces any prior
// fixture for the version (latest wins, revisions stay auditable).
// Validation rejects an empty fixture, duplicate or empty ids, unknown
// kinds and empty values, so a recorded fixture is always at least one
// real expectation of the content, never a vacuous pass.
func RecordCandidateAcceptance(ctx context.Context, mem *memory.Store, ns, name, version string, assertions []AcceptanceAssertion) error {
	if name == "" || version == "" {
		return errors.New("skillmine: acceptance fixture requires a skill name and version")
	}
	if len(assertions) == 0 {
		return errors.New("skillmine: acceptance fixture requires at least one assertion")
	}
	if len(assertions) > acceptanceMaxAssertions {
		return fmt.Errorf("skillmine: acceptance fixture: want at most %d assertions", acceptanceMaxAssertions)
	}
	seen := make(map[string]bool, len(assertions))
	for _, a := range assertions {
		if a.ID == "" || len([]rune(a.ID)) > acceptanceIDMaxRunes {
			return fmt.Errorf("skillmine: acceptance assertion id %q: 1-%d chars", a.ID, acceptanceIDMaxRunes)
		}
		if seen[a.ID] {
			return fmt.Errorf("skillmine: acceptance assertion id %q: duplicate", a.ID)
		}
		seen[a.ID] = true
		switch a.Kind {
		case AssertionContains, AssertionAbsent:
		default:
			return fmt.Errorf("skillmine: acceptance assertion %s kind %q: want %s or %s",
				a.ID, a.Kind, AssertionContains, AssertionAbsent)
		}
		if a.Value == "" || len([]rune(a.Value)) > acceptanceValueMaxRunes {
			return fmt.Errorf("skillmine: acceptance assertion %s value: 1-%d chars", a.ID, acceptanceValueMaxRunes)
		}
	}
	raw, err := json.Marshal(assertions)
	if err != nil {
		return fmt.Errorf("record acceptance fixture: %w", err)
	}
	ids := make([]string, len(assertions))
	for i, a := range assertions {
		ids[i] = a.ID
	}
	if _, err := mem.Write(ctx, memory.WriteInput{
		Namespace: ns, Key: skillAcceptanceKey(name, version),
		Body: fmt.Sprintf("acceptance fixture for %s@%s: %d assertions (%s)",
			name, version, len(assertions), strings.Join(ids, ", ")),
		Attributes: map[string]any{
			"skill_name":            name,
			"skill_version":         version,
			"acceptance_assertions": string(raw),
		},
		Writer: "skill-acceptance",
	}); err != nil {
		return fmt.Errorf("record acceptance fixture: %w", err)
	}
	return nil
}

// loadCandidateAcceptance projects the recorded acceptance fixture for
// one proposed version; nil when none is recorded or the artifact is
// malformed. The recall window is scanned for the EXACT key so a longer
// sibling version sharing the prefix cannot shadow the fixture.
func loadCandidateAcceptance(ctx context.Context, mem *memory.Store, ns, name, version string) []AcceptanceAssertion {
	key := skillAcceptanceKey(name, version)
	facts, err := mem.Recall(ctx, ns, key, 8)
	if err != nil {
		return nil
	}
	for _, f := range facts {
		if f.Key != key || f.Attributes == nil {
			continue
		}
		raw, _ := f.Attributes["acceptance_assertions"].(string)
		var assertions []AcceptanceAssertion
		if json.Unmarshal([]byte(raw), &assertions) != nil || len(assertions) == 0 {
			return nil
		}
		return assertions
	}
	return nil
}

// runAcceptanceAssertions executes one acceptance fixture against a
// candidate body and returns the ids of the assertions that held and
// that failed. An unknown kind fails: a fixture the evaluator cannot
// execute is not support.
func runAcceptanceAssertions(body string, assertions []AcceptanceAssertion) (held, failed []string) {
	for _, a := range assertions {
		ok := false
		switch a.Kind {
		case AssertionContains:
			ok = strings.Contains(body, a.Value)
		case AssertionAbsent:
			ok = !strings.Contains(body, a.Value)
		}
		if ok {
			held = append(held, a.ID)
		} else {
			failed = append(failed, a.ID)
		}
	}
	return held, failed
}

// OutcomeVerdict grades one proposal along four nil-able dimensions,
// shaped like the membench Judge's verdict (E02 grades citation
// existence separately from support): nil means "not graded". The
// mandatory integrity checks (citations, identity, draft form and the
// candidate evaluation) always run and veto independently of this
// verdict; a checker's graded false only adds a veto, and a graded true
// never disarms a deterministically false integrity grade.
// Implementations that make model calls MUST return the usage they
// consumed in PromptTokens/CompletionTokens even when they return an
// error.
type OutcomeVerdict struct {
	CitationsResolve *bool // every cited run resolves to a live record
	IdentityMatch    *bool // the base version still carries the recorded identity
	DraftWellFormed  *bool // proposed body non-empty, differing, within caps
	EvidenceSupports *bool // the candidate's own evaluation supports activation
	Justification    string
	PromptTokens     int
	CompletionTokens int
}

// OutcomeChecker decides whether an approved improvement may activate.
type OutcomeChecker interface {
	CheckOutcome(ctx context.Context, in OutcomeInput) (OutcomeVerdict, error)
}

// DefaultOutcomeChecker is the deterministic, model-free gate: the
// citations resolve, the base identity is unchanged, the draft is
// non-empty, different from the base procedure and within the body cap,
// and the candidate itself carries a passed, digest-bound evaluation
// that the check RE-VERIFIES by executing the recorded acceptance
// fixture against the proposed body. The motivation for proposing a
// change (the cited base runs) never substitutes for an evaluation of
// the proposed procedure, and a persisted passed flag without a
// re-runnable fixture is not support. E02-style: citation existence and
// candidate support are graded separately.
type DefaultOutcomeChecker struct{}

func (DefaultOutcomeChecker) CheckOutcome(_ context.Context, in OutcomeInput) (OutcomeVerdict, error) {
	citations := true
	for _, c := range in.Runs {
		if c.Run == nil {
			citations = false
			break
		}
	}
	identity := in.BaseIdentity != "" && in.CurrentIdentity == in.BaseIdentity
	wellFormed := in.ProposedBody != "" && in.BaseBody != "" &&
		in.ProposedBody != in.BaseBody &&
		utf8.RuneCountInString(in.ProposedBody) <= skillImproveBodyMaxRunes
	var supports bool
	var evalNote string
	switch {
	case in.CandidateEval == nil:
		evalNote = "candidate evaluation: not graded (no recorded evaluation matching the approved draft digest)"
	case in.CandidateEval.Status == CandidateEvalUngraded:
		evalNote = fmt.Sprintf("candidate evaluation: not graded (%s)", in.CandidateEval.Check)
	case in.CandidateEval.Status != CandidateEvalPassed:
		evalNote = fmt.Sprintf("candidate evaluation: unsupported (failed via %q, %d sources)",
			in.CandidateEval.Check, len(in.CandidateEval.Sources))
	default:
		// A recorded passed verdict is never trusted as a bare flag:
		// the gate itself executes the recorded acceptance fixture
		// against the proposed body before grading support.
		held, failed := runAcceptanceAssertions(in.ProposedBody, in.Acceptance)
		if len(in.Acceptance) == 0 || len(failed) > 0 {
			evalNote = fmt.Sprintf("candidate evaluation: unsupported (recorded passed via %q, but no acceptance fixture re-verifies against the candidate body)",
				in.CandidateEval.Check)
		} else {
			supports = true
			evalNote = fmt.Sprintf("candidate evaluation: passed via %q, re-verified (%d/%d acceptance assertions hold)",
				in.CandidateEval.Check, len(held), len(in.Acceptance))
		}
	}
	return OutcomeVerdict{
		CitationsResolve: &citations,
		IdentityMatch:    &identity,
		DraftWellFormed:  &wellFormed,
		EvidenceSupports: &supports,
		Justification: fmt.Sprintf("%d/%d citations resolved, identity match %t, draft %d runes, %s",
			len(in.Runs), len(in.Runs), identity, utf8.RuneCountInString(in.ProposedBody), evalNote),
	}, nil
}

// draftDigest pins a proposal to the exact draft content: the hex
// SHA-256 of the procedure body, stored in the approved params and
// verified against the live draft at application time.
func draftDigest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// EvaluateCandidateDeterministic grades a proposed candidate with a
// model-free evaluation of the candidate's OWN content: the acceptance
// fixture recorded for the proposed version - explicit assertions
// supplied independently of the drafter - is executed against the live
// draft body. Form and integrity stay separate from support: the draft
// must still hash to the approved digest, resolve to its content-derived
// version, differ from the base procedure and fit the body cap before
// any evaluation is possible, and failing that records a FAILED
// evaluation (an integrity verdict, never support). A candidate with no
// recorded acceptance fixture is not evaluated at all and stays
// UNGRADED - resolvable citations of the base version's old runs
// motivate a proposal, they never prove the candidate, and unknown
// support is never passed. The returned artifact names the check and
// cites the executed assertion ids as its sources, so the audit trail
// shows exactly what was asserted and what held; the activation gate
// re-runs the fixture itself before trusting a passed verdict.
func EvaluateCandidateDeterministic(ctx context.Context, mem *memory.Store, ns string, pr *policy.Proposal) (*CandidateEvaluation, error) {
	var params skillImprovementParams
	if err := json.Unmarshal([]byte(pr.Params), &params); err != nil {
		return nil, fmt.Errorf("evaluate candidate %s: bad params: %w", pr.ID, err)
	}
	draftFacts, err := mem.Recall(ctx, ns, params.DraftKey, 1)
	if err != nil || len(draftFacts) == 0 || draftFacts[0].Key != params.DraftKey {
		return nil, fmt.Errorf("evaluate candidate %s: draft %s not found", pr.ID, params.DraftKey)
	}
	draftBody := draftFacts[0].Body
	baseBody, err := mem.SkillBody(ctx, ns, pr.Target, params.BaseVersion)
	if err != nil && !errors.Is(err, memory.ErrSkillNotFound) {
		return nil, fmt.Errorf("evaluate candidate %s: %w", pr.ID, err)
	}
	// Integrity (form) gate: a candidate that is not the approved,
	// well-formed revision of the base cannot be supported by anything.
	// This grade is integrity, never outcome support.
	if draftDigest(draftBody) != params.DraftDigest ||
		improvementVersion(draftBody) != params.ProposedVersion ||
		baseBody == "" || draftBody == "" || draftBody == baseBody ||
		utf8.RuneCountInString(draftBody) > skillImproveBodyMaxRunes {
		ev := CandidateEvaluation{
			DraftDigest: params.DraftDigest,
			Status:      CandidateEvalFailed,
			Check:       "integrity: candidate is not the approved, well-formed revision of the base procedure",
		}
		if err := RecordCandidateEvaluation(ctx, mem, ns, pr.Target, params.ProposedVersion, ev); err != nil {
			return nil, err
		}
		return &ev, nil
	}
	// The support verdict comes only from executing the recorded
	// acceptance fixture against the candidate body. No fixture: the
	// proposed behavior was never evaluated and support stays unknown.
	assertions := loadCandidateAcceptance(ctx, mem, ns, pr.Target, params.ProposedVersion)
	ev := CandidateEvaluation{DraftDigest: params.DraftDigest}
	switch {
	case len(assertions) == 0:
		ev.Status = CandidateEvalUngraded
		ev.Check = "no acceptance fixture recorded for the candidate; its proposed behavior was never evaluated"
	default:
		held, failed := runAcceptanceAssertions(draftBody, assertions)
		if len(failed) == 0 {
			ev.Status = CandidateEvalPassed
			ev.Check = fmt.Sprintf("deterministic acceptance check: %d/%d recorded assertions hold against the candidate body",
				len(held), len(assertions))
			ev.Sources = held
		} else {
			ev.Status = CandidateEvalFailed
			ev.Check = "acceptance assertions failed against the candidate body: " + strings.Join(failed, ", ")
			ev.Sources = failed
		}
	}
	if err := RecordCandidateEvaluation(ctx, mem, ns, pr.Target, params.ProposedVersion, ev); err != nil {
		return nil, err
	}
	return &ev, nil
}

// ApplySkillProposal activates an approved skill improvement. Gates, in
// order: the proposal must carry the skill_improve action class; an
// already-applied proposal is a no-op (idempotent by the applied
// external_ref marker); anything but approved is ErrProposalNotApproved
// (a draft has no authority over active procedures); the proposal must
// belong to the applying namespace (the ledger dedup is global, the
// proposal and its citations are not); the draft and every cited run
// must still resolve; the draft must still be byte-for-byte the content
// the approval was granted for (digest-bound, checked before any
// checker runs); and the outcome check must not veto. The mandatory
// integrity checks - citations, base identity, draft form and the
// candidate's own digest-bound evaluation, which the gate re-verifies by
// executing the recorded acceptance fixture against the draft body - are
// graded deterministically and veto on their own; an optional checker
// can only add vetoes (a graded-false dimension), never disarm them, and
// a candidate that was never evaluated cannot pass on the strength of
// the base version's old failures. Activation then stages the publish:
// the draft is indexed as a new content-derived version, the base
// version is deactivated but stays published and immutable (prior runs
// keep citing it, a regression can revert to it), and the proposal is
// marked applied.
//
// Crash windows mirror the S01 staged publish and are healed by simply
// running the apply again: a failure after the index but before the
// deactivation leaves two active versions (exact-version loads stay
// correct; the re-apply deactivates the base), and a failure after the
// deactivation but before the marker leaves the state applied but
// unmarked (the re-apply marks it; the index and flip are no-ops).
func ApplySkillProposal(ctx context.Context, mem *memory.Store, props *policy.Proposals, ns, proposalID string, checker OutcomeChecker) error {
	pr, err := props.Get(ctx, proposalID)
	if err != nil {
		return fmt.Errorf("apply skill proposal: %w", err)
	}
	if pr.ActionClass != ActionClassSkillImprove {
		return fmt.Errorf("apply skill proposal %s: action class %q, want %s", pr.ID, pr.ActionClass, ActionClassSkillImprove)
	}
	if strings.HasPrefix(pr.ExternalRef, appliedRefPrefix) {
		return nil // already applied: one effect, however often it runs
	}
	if pr.Status != "approved" {
		return fmt.Errorf("apply skill proposal %s: status %s: %w", pr.ID, pr.Status, ErrProposalNotApproved)
	}
	var params skillImprovementParams
	if err := json.Unmarshal([]byte(pr.Params), &params); err != nil {
		return fmt.Errorf("apply skill proposal %s: bad params: %w", pr.ID, err)
	}
	if params.Namespace != ns {
		return fmt.Errorf("apply skill proposal %s: proposal belongs to namespace %q, not %q", pr.ID, params.Namespace, ns)
	}
	if !strings.HasPrefix(params.DraftKey, "/skill-drafts/"+pr.Target+"/") || params.BaseVersion == "" || params.ProposedVersion == "" {
		return fmt.Errorf("apply skill proposal %s: params name no draft of %s", pr.ID, pr.Target)
	}

	// Revalidate the draft: it must still be live, exactly where the
	// proposal points, and carrying a procedure.
	facts, err := mem.Recall(ctx, ns, params.DraftKey, 1)
	if err != nil {
		return fmt.Errorf("apply skill proposal %s: %w", pr.ID, err)
	}
	if len(facts) == 0 || facts[0].Key != params.DraftKey {
		return fmt.Errorf("apply skill proposal %s: draft %s: %w", pr.ID, params.DraftKey, memory.ErrNotFound)
	}
	draftBody := facts[0].Body

	// The approval binds to the exact draft content: the live draft must
	// still hash to the digest the proposal was approved with (and so
	// still resolve to its content-derived version). This runs before
	// any checker, so post-approval draft swaps cannot publish a
	// different procedure under the approved version.
	if params.DraftDigest == "" || draftDigest(draftBody) != params.DraftDigest {
		return fmt.Errorf("apply skill proposal %s: draft %s: %w", pr.ID, params.DraftKey, ErrDraftDigestMismatch)
	}

	// Revalidate the citations: every cited run must still resolve to a
	// live run record of the base version (proposals cite actual runs).
	runs, err := mem.ListSkillRuns(ctx, ns, pr.Target, params.BaseVersion)
	if err != nil {
		return fmt.Errorf("apply skill proposal %s: %w", pr.ID, err)
	}
	byID := make(map[string]memory.SkillRunRecord, len(runs))
	for _, r := range runs {
		byID[r.RunID] = r
	}
	citations := make([]OutcomeCitation, 0, len(params.RunIDs))
	for _, id := range params.RunIDs {
		c := OutcomeCitation{RunID: id}
		if r, ok := byID[id]; ok {
			run := r
			c.Run = &run
		}
		if c.Run == nil {
			return fmt.Errorf("apply skill proposal %s: %w: %s", pr.ID, ErrUnresolvedRun, id)
		}
		citations = append(citations, c)
	}

	currentIdentity, err := mem.SkillIdentity(ctx, ns, pr.Target, params.BaseVersion)
	if err != nil {
		if !errors.Is(err, memory.ErrSkillNotFound) {
			return fmt.Errorf("apply skill proposal %s: %w", pr.ID, err)
		}
		currentIdentity = ""
	}
	baseBody, err := mem.SkillBody(ctx, ns, pr.Target, params.BaseVersion)
	if err != nil && !errors.Is(err, memory.ErrSkillNotFound) {
		return fmt.Errorf("apply skill proposal %s: %w", pr.ID, err)
	}

	// The candidate's own evaluation is part of the mandatory gate: the
	// stored artifact must exist, match the approved draft digest and
	// have passed - and the recorded acceptance fixture rides the input
	// so the deterministic gate RE-RUNS it against the draft body: a
	// persisted passed flag alone never activates. The motivation for
	// proposing a change - the base version's recorded failures - never
	// substitutes for any of it.
	var candEval *CandidateEvaluation
	if ev := loadCandidateEvaluation(ctx, mem, ns, pr.Target, params.ProposedVersion); ev != nil && ev.DraftDigest == params.DraftDigest {
		candEval = ev
	}
	acceptance := loadCandidateAcceptance(ctx, mem, ns, pr.Target, params.ProposedVersion)

	// The E02-style outcome check gates activation; the deterministic
	// default needs no model. The mandatory integrity checks are graded
	// deterministically and veto independently; the optional checker's
	// grades only add vetoes - a graded true can never disarm a
	// deterministically false integrity grade.
	if checker == nil {
		checker = DefaultOutcomeChecker{}
	}
	in := OutcomeInput{
		SkillName:       pr.Target,
		BaseVersion:     params.BaseVersion,
		ProposedVersion: params.ProposedVersion,
		BaseBody:        baseBody,
		ProposedBody:    draftBody,
		BaseIdentity:    params.BaseIdentity,
		CurrentIdentity: currentIdentity,
		Runs:            citations,
		CandidateEval:   candEval,
		Acceptance:      acceptance,
	}
	verdict, err := checker.CheckOutcome(ctx, in)
	if err != nil {
		return fmt.Errorf("apply skill proposal %s: outcome check: %w", pr.ID, err)
	}
	def, err := DefaultOutcomeChecker{}.CheckOutcome(ctx, in)
	if err != nil {
		return fmt.Errorf("apply skill proposal %s: default outcome check: %w", pr.ID, err)
	}
	for _, dim := range []struct {
		name   string
		def    *bool
		custom *bool
	}{
		{"citations_resolve", def.CitationsResolve, verdict.CitationsResolve},
		{"identity_match", def.IdentityMatch, verdict.IdentityMatch},
		{"draft_well_formed", def.DraftWellFormed, verdict.DraftWellFormed},
		{"candidate_evaluated", def.EvidenceSupports, verdict.EvidenceSupports},
	} {
		if dim.def != nil && !*dim.def {
			return fmt.Errorf("apply skill proposal %s: %w (%s: %s)", pr.ID, ErrOutcomeCheckFailed, dim.name, def.Justification)
		}
		if dim.custom != nil && !*dim.custom {
			return fmt.Errorf("apply skill proposal %s: %w (%s: %s)", pr.ID, ErrOutcomeCheckFailed, dim.name, verdict.Justification)
		}
	}

	// Staged activation: publish the draft as a new immutable version,
	// then deactivate the base. An identical re-apply (crash-heal path)
	// finds the index a no-op.
	draftMeta := memory.SkillMeta{
		Name:        pr.Target,
		Version:     params.ProposedVersion,
		Description: params.Description,
		Tools:       params.Tools,
		Scope:       params.Scope,
		Source:      memory.SkillSourceMined,
		Active:      true,
	}
	if err := mem.IndexSkill(ctx, ns, draftMeta, draftBody); err != nil {
		return fmt.Errorf("apply skill proposal %s: publish %s@%s: %w", pr.ID, pr.Target, params.ProposedVersion, err)
	}
	if err := mem.SetSkillActive(ctx, ns, pr.Target, params.BaseVersion, false); err != nil {
		return fmt.Errorf("apply skill proposal %s: deactivate base %s: %w", pr.ID, params.BaseVersion, err)
	}
	if _, err := props.UpdateStatus(ctx, pr.ID, "approved", appliedRefPrefix+pr.Target+"@"+params.ProposedVersion); err != nil {
		return fmt.Errorf("apply skill proposal %s: mark applied: %w", pr.ID, err)
	}
	return nil
}
