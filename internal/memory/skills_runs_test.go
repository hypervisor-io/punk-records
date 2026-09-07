package memory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Task S02 red proofs and acceptance for run outcomes, version lineage
// and regression revert: a run outcome is pinned to the exact published
// version through its identity record, versions and run lineage are
// queryable, a regression can revert to a prior version, and run records
// are retention-exempt like the identity rows they are bound to.

func skillRunFixture(name, version, taskID string, seq int64) SkillRunInput {
	return SkillRunInput{
		SkillName:     name,
		SkillVersion:  version,
		TaskID:        taskID,
		TaskSeq:       seq,
		Outcome:       RunOutcomeFailed,
		SuccessScore:  0.2,
		ErrorType:     "tool_failure",
		ErrorMessage:  "pool exhausted after 3 retries",
		ResultSummary: "2 of 5 triage checks failed",
		Actor:         "agent:db",
	}
}

// Recording a run outcome REQUIRES the published identity row: the
// outcome is bound to the exact shipped content, and the stored record
// carries the identity digest, never procedure bytes.
func TestRecordSkillRunRequiresPublishedIdentity(t *testing.T) {
	s, db, _ := newTest(t)
	ctx := context.Background()
	meta := skillMetaFixture()

	in := skillRunFixture(meta.Name, meta.Version, "task-1", 3)
	if _, err := s.RecordSkillRun(ctx, "ns", in); !errors.Is(err, ErrSkillIdentityMissing) {
		t.Fatalf("record run before publish: err = %v, want ErrSkillIdentityMissing", err)
	}

	body := "# Triage\n\n" + skillBodySentinel
	indexFixture(t, s, "ns", meta, body)
	rec, err := s.RecordSkillRun(ctx, "ns", in)
	if err != nil {
		t.Fatal(err)
	}
	if rec.RunID != "task-1-3" {
		t.Fatalf("run id = %q, want task-1-3", rec.RunID)
	}
	ident, err := s.SkillIdentity(ctx, "ns", meta.Name, meta.Version)
	if err != nil {
		t.Fatal(err)
	}
	if rec.SkillIdentity != ident || ident == "" {
		t.Fatalf("run identity = %q, want the published identity %q", rec.SkillIdentity, ident)
	}
	if rec.TaskID != "task-1" || rec.TaskSeq != 3 || rec.Outcome != RunOutcomeFailed ||
		rec.SuccessScore != 0.2 || rec.ErrorMessage != "pool exhausted after 3 retries" ||
		rec.ResultSummary != "2 of 5 triage checks failed" || rec.Actor != "agent:db" {
		t.Fatalf("record mismatch: %+v", rec)
	}

	// The run record is digest-only: the stored body never carries the
	// procedure text, exactly like the identity row it cites.
	var stored string
	if err := db.QueryRowContext(ctx, db.Rebind(`SELECT body FROM memories WHERE key = $1`),
		skillRunKey(meta.Name, meta.Version, rec.RunID)).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, skillBodySentinel) || strings.Contains(stored, "Pull the incident") {
		t.Fatalf("run record leaked procedure bytes: %s", stored)
	}
}

func TestSkillVersionsAndRunLineageQueryable(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := context.Background()
	v1 := skillMetaFixture()
	indexFixture(t, s, "ns", v1, "original procedure")
	v2 := v1
	v2.Version = "0.2.0"
	indexFixture(t, s, "ns", v2, "revised procedure")

	runA, err := s.RecordSkillRun(ctx, "ns", skillRunFixture(v1.Name, "0.1.0", "task-1", 1))
	if err != nil {
		t.Fatal(err)
	}
	inB := skillRunFixture(v1.Name, "0.1.0", "task-2", 5)
	inB.Outcome = RunOutcomeSuccess
	inB.SuccessScore = 0.95
	runB, err := s.RecordSkillRun(ctx, "ns", inB)
	if err != nil {
		t.Fatal(err)
	}
	// Re-recording the same run is idempotent: the deterministic run id
	// and identical content collapse to the single live revision.
	if _, err := s.RecordSkillRun(ctx, "ns", inB); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordSkillRun(ctx, "ns", skillRunFixture(v1.Name, "0.2.0", "task-3", 2)); err != nil {
		t.Fatal(err)
	}
	// Namespace scoping: the same skill elsewhere is a separate lineage
	// with its own published version and its own runs.
	indexFixture(t, s, "other-ns", v1, "another namespace's procedure")
	other := skillRunFixture(v1.Name, "0.1.0", "task-9", 1)
	if _, err := s.RecordSkillRun(ctx, "other-ns", other); err != nil {
		t.Fatal(err)
	}

	versions, err := s.ListSkillVersions(ctx, "ns", v1.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 2 || versions[0].Version != "0.1.0" || versions[1].Version != "0.2.0" {
		t.Fatalf("versions = %+v, want 0.1.0 then 0.2.0", versions)
	}
	if ghost, err := s.ListSkillVersions(ctx, "ns", "ghost-skill"); err != nil || len(ghost) != 0 {
		t.Fatalf("ghost versions = %+v err=%v", ghost, err)
	}

	all, err := s.ListSkillRuns(ctx, "ns", v1.Name, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("all runs = %d, want 3: %+v", len(all), all)
	}
	// Lineage order is chronological (recorded at, then run id).
	if all[0].RunID != runA.RunID || all[1].RunID != runB.RunID {
		t.Fatalf("lineage order: %s, %s, ...", all[0].RunID, all[1].RunID)
	}
	v1runs, err := s.ListSkillRuns(ctx, "ns", v1.Name, "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(v1runs) != 2 {
		t.Fatalf("v1 runs = %d, want 2", len(v1runs))
	}
	v1ident, err := s.SkillIdentity(ctx, "ns", v1.Name, "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range v1runs {
		if r.SkillVersion != "0.1.0" || r.SkillIdentity != v1ident {
			t.Fatalf("run %s does not cite the original version: %+v", r.RunID, r)
		}
	}
	if none, err := s.ListSkillRuns(ctx, "ns", v1.Name, "9.9.9"); err != nil || len(none) != 0 {
		t.Fatalf("unknown version runs = %+v err=%v", none, err)
	}
	if foreign, err := s.ListSkillRuns(ctx, "other-ns", v1.Name, ""); err != nil || len(foreign) != 1 {
		t.Fatalf("other-ns runs = %+v err=%v", foreign, err)
	}

	// Run rows live under /skill-runs/, disjoint from the /skills/
	// discovery collection: the two indexed versions are all that the
	// skill listing ever projects.
	listed, err := s.ListSkillsAll(ctx, "ns")
	if err != nil || len(listed) != 2 {
		t.Fatalf("ListSkillsAll leaked run rows: %+v err=%v", listed, err)
	}
}

func TestRegressionRevertsToPriorVersion(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := context.Background()
	v1 := skillMetaFixture()
	indexFixture(t, s, "ns", v1, "original procedure")
	v2 := v1
	v2.Version = "0.2.0"
	indexFixture(t, s, "ns", v2, "revised procedure")

	if err := s.RevertSkillVersion(ctx, "ns", v1.Name, "0.2.0", "0.1.0"); err != nil {
		t.Fatal(err)
	}
	_, body, err := s.LoadSkill(ctx, "ns", v1.Name, "")
	if err != nil {
		t.Fatal(err)
	}
	if body != "original procedure" {
		t.Fatalf("after revert the active procedure is %q, want the prior version", body)
	}
	all, err := s.ListSkillsAll(ctx, "ns")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("ListSkillsAll = %d entries, want both versions still published", len(all))
	}
	for _, m := range all {
		if m.Version == "0.2.0" && m.Active {
			t.Fatalf("regressed version still active: %+v", m)
		}
		if m.Version == "0.1.0" && !m.Active {
			t.Fatalf("prior version not active after revert: %+v", m)
		}
	}
	// The regressed version stays published: an exact load fails only
	// because it is inactive, not because it is gone.
	if _, _, err := s.LoadSkill(ctx, "ns", v1.Name, "0.2.0"); !errors.Is(err, ErrSkillInactive) {
		t.Fatalf("load regressed version: err = %v, want ErrSkillInactive", err)
	}

	if err := s.RevertSkillVersion(ctx, "ns", v1.Name, "9.9.9", "0.1.0"); !errors.Is(err, ErrSkillNotFound) {
		t.Fatalf("revert from unknown version: err = %v, want ErrSkillNotFound", err)
	}
	if err := s.RevertSkillVersion(ctx, "ns", v1.Name, "0.1.0", "0.1.0"); err == nil {
		t.Fatal("revert to the same version accepted")
	}
}

// Run records are retention-exempt by the same construction as the
// identity rows: single live revision, never tombstoned or expired by
// the skill lifecycle, kept by both purge paths - so the lineage
// outlives the unpublish and sweep that remove the version's own rows.
func TestSkillRunOutcomeSurvivesRetentionSweep(t *testing.T) {
	s, db, clk := newTest(t)
	ctx := context.Background()
	v1 := skillMetaFixture()
	indexFixture(t, s, "ns", v1, "original procedure")
	rec, err := s.RecordSkillRun(ctx, "ns", skillRunFixture(v1.Name, "0.1.0", "task-7", 1))
	if err != nil {
		t.Fatal(err)
	}

	clk.Set(s.now().Add(time.Second))
	if err := s.UnpublishSkill(ctx, "ns", v1.Name, v1.Version); err != nil {
		t.Fatal(err)
	}
	// The sweep must actually purge the unpublished chains, or survival
	// would be vacuous.
	clk.Set(s.now().Add(48 * time.Hour))
	n, err := s.SweepRetention(ctx, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("sweep removed nothing")
	}
	var versionRows int
	if err := db.QueryRowContext(ctx, db.Rebind(`SELECT count(*) FROM memories WHERE key IN ($1, $2)`),
		skillDocKey(v1.Name, v1.Version), skillBodyKey(v1.Name, v1.Version)).Scan(&versionRows); err != nil {
		t.Fatal(err)
	}
	if versionRows != 0 {
		t.Fatalf("unpublished version rows survived the sweep: %d", versionRows)
	}

	runs, err := s.ListSkillRuns(ctx, "ns", v1.Name, "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].RunID != rec.RunID || runs[0].SkillIdentity != rec.SkillIdentity {
		t.Fatalf("run lineage did not survive the sweep: %+v (want %s)", runs, rec.RunID)
	}
	// The identity pin survives with it: changed content under the swept
	// version stays a conflict.
	if err := s.IndexSkill(ctx, "ns", v1, "changed procedure"); !errors.Is(err, ErrSkillVersionConflict) {
		t.Fatalf("re-publish changed body after the sweep: err = %v, want ErrSkillVersionConflict", err)
	}
}
