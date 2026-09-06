package skillmine

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/task"
)

// Cognee provenance: run-outcome recording adapts the mechanism of
// cognee/modules/memify/skill_improvement.py (commit
// 78ff576559a7f75f65884c5bd90b22cdc790016e), where skill runs are logged
// and their outcomes feed improvement proposals. The adaptation keeps
// the two-anchor binding - a run outcome points at both the exact
// procedure version it executed and the ledger task that ran it - while
// replacing cognee's in-place procedure mutation with the S01
// immutable-version publish (outcomes are pinned to a version's content
// address, never to mutable rows).

// RunOutcomeInput is one procedural-skill execution to record. The
// outcome is anchored to TaskID (the originating ledger task), and
// ExecutionID is the caller's stable identity for that one real
// execution, scoped to the task: the run id is derived as
// <taskID>-<executionID> and the id rides the skill_run ledger event.
// Two executions of one task are two recordings even when their outputs
// are identical; retries of one execution stay one recording.
type RunOutcomeInput struct {
	SkillName     string
	SkillVersion  string
	TaskID        string
	ExecutionID   string
	Outcome       string
	SuccessScore  float64
	ErrorType     string
	ErrorMessage  string
	ResultSummary string
	Actor         string
}

// runRecordLocks serializes recordings per run key WITHIN this process,
// so concurrent in-process duplicates of one execution cannot race past
// the existence check. It is not the durability boundary and protects
// nothing across processes: cross-writer atomicity lives in the
// ledger's AppendSkillRunOnce transaction (postgres locks the task row,
// sqlite runs one writer connection per process and WAL surfaces
// cross-process write conflicts as errors), and the memory-plane pin is
// latest-wins per run key, so even an unserialized cross-process
// duplicate of one execution converges on one ledger event and one
// record instead of double-appending.
var (
	runRecordLocksMu sync.Mutex
	runRecordLocks   = map[string]*sync.Mutex{}
)

func lockRunRecord(key string) func() {
	runRecordLocksMu.Lock()
	mu, ok := runRecordLocks[key]
	if !ok {
		mu = &sync.Mutex{}
		runRecordLocks[key] = mu
	}
	runRecordLocksMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// RecordRunOutcome anchors one run outcome to its originating ledger
// evidence and the exact skill version: the skill_run ledger event is
// appended (its per-task sequence number is the evidence anchor), then
// the outcome is pinned to the version in the memory plane under
// /skill-runs/<name>/<version>/<taskID>-<executionID>.
//
// Every gate runs before the first side effect, so a rejected recording
// leaves no ledger event claiming an outcome that was never recorded:
// the input must be well-formed (including an explicit execution id),
// the ledger task must exist, and the version must carry a published
// identity record - an outcome can only bind to content that actually
// shipped.
//
// Execution identity and retry handling: one execution is identified by
// (namespace, task, execution id) - never by outcome content, since two
// real executions of a task can legitimately produce identical outputs.
// The namespace rides the skill_run event payload, so an anchor from
// another namespace is never reused. A retry of the same execution
// resolves to the first recording instead of inflating evidence: an
// already-pinned record is returned as-is (a retry carrying different
// content for a recorded execution is refused), and an appended-but-
// unpinned event from a failed prior attempt is healed by pinning the
// record under that event's sequence number - but only when the event
// carries this execution's EXACT semantic content (skill, version,
// outcome, score, error and summary text, actor). Any difference is a
// hard conflict (task.ErrSkillRunConflict): a retry can heal a partial
// write, never change a recorded outcome, and the conflict is refused
// without appending new evidence. Anchor resolution and the append run
// in one ledger transaction, so concurrent writers cannot both append
// for one execution. A failed attempt can therefore leave at most one
// dangling ledger event, and the lineage keeps exactly one record per
// distinct execution.
func RecordRunOutcome(ctx context.Context, mem *memory.Store, ledger *task.Ledger, ns string, in RunOutcomeInput) (*memory.SkillRunRecord, error) {
	if in.TaskID == "" {
		return nil, errors.New("skillmine: run outcome requires a task id")
	}
	if in.ExecutionID == "" {
		return nil, errors.New("skillmine: run outcome requires an execution id")
	}
	actor := in.Actor
	if actor == "" {
		actor = "skillmine"
	}
	score := in.SuccessScore
	if score < 0 {
		score = 0
	} else if score > 1 {
		score = 1
	}
	run := memory.SkillRunInput{
		SkillName:     in.SkillName,
		SkillVersion:  in.SkillVersion,
		TaskID:        in.TaskID,
		ExecutionID:   in.ExecutionID,
		Outcome:       in.Outcome,
		SuccessScore:  score,
		ErrorType:     in.ErrorType,
		ErrorMessage:  in.ErrorMessage,
		ResultSummary: in.ResultSummary,
		Actor:         actor,
	}
	// Input validation first: nothing is appended for a malformed run.
	if err := memory.ValidateSkillRunInput(run); err != nil {
		return nil, fmt.Errorf("record run outcome: %w", err)
	}
	// The anchor must exist: evidence points at a real ledger task.
	if _, _, err := ledger.Get(ctx, in.TaskID); err != nil {
		return nil, fmt.Errorf("record run outcome: %w", err)
	}
	// The identity must exist: an outcome can only bind to shipped
	// content, so a never-published version is rejected before any
	// event is appended.
	if _, err := mem.SkillIdentity(ctx, ns, in.SkillName, in.SkillVersion); err != nil {
		return nil, fmt.Errorf("record run outcome: %w", err)
	}

	runID := fmt.Sprintf("%s-%s", in.TaskID, in.ExecutionID)
	unlock := lockRunRecord(ns + "\x00" + skillRunKeyPath(in.SkillName, in.SkillVersion, runID))
	defer unlock()

	// Retry resolution: this execution's record is the recording.
	runs, err := mem.ListSkillRuns(ctx, ns, in.SkillName, in.SkillVersion)
	if err != nil {
		return nil, fmt.Errorf("record run outcome: %w", err)
	}
	for i := range runs {
		if runs[i].TaskID == in.TaskID && runs[i].ExecutionID == in.ExecutionID {
			if !sameExecutionContent(&runs[i], &run) {
				return nil, fmt.Errorf("record run outcome: execution %s already recorded with a different outcome", in.ExecutionID)
			}
			return &runs[i], nil
		}
	}

	// Anchor resolution and the append are ONE transaction at the
	// ledger's writer boundary: an appended-but-unpinned event of a
	// failed prior attempt of THIS execution identity is reused (same
	// seq, same run id), a stored anchor with different semantic
	// content is a hard conflict that appends nothing, and only a
	// truly absent anchor appends a new event.
	seq, _, err := ledger.AppendSkillRunOnce(ctx, in.TaskID, actor, task.SkillRunPayload{
		SkillName:     in.SkillName,
		SkillVersion:  in.SkillVersion,
		Namespace:     ns,
		ExecutionID:   in.ExecutionID,
		Outcome:       run.Outcome,
		SuccessScore:  run.SuccessScore,
		ErrorType:     run.ErrorType,
		ErrorMessage:  run.ErrorMessage,
		ResultSummary: run.ResultSummary,
	})
	if err != nil {
		return nil, fmt.Errorf("record run outcome: %w", err)
	}
	run.TaskSeq = seq
	rec, err := mem.RecordSkillRun(ctx, ns, run)
	if err != nil {
		return nil, fmt.Errorf("record run outcome: %w", err)
	}
	return rec, nil
}

// skillRunKeyPath is the memory-plane key of a run record (the lock and
// lineage path of one execution's evidence).
func skillRunKeyPath(name, version, runID string) string {
	return "/skill-runs/" + name + "/" + version + "/" + runID
}

// sameExecutionContent reports whether a pinned run record carries the
// same outcome content as a run input - the consistency check for a
// retry of one recorded execution, never the identity itself.
func sameExecutionContent(r *memory.SkillRunRecord, in *memory.SkillRunInput) bool {
	return r.Outcome == in.Outcome && r.SuccessScore == in.SuccessScore &&
		r.ErrorType == in.ErrorType && r.ErrorMessage == in.ErrorMessage &&
		r.ResultSummary == in.ResultSummary && r.Actor == in.Actor
}
