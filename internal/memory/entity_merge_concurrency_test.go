package memory

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/store"
)

// mergeBarrierEmbedder pauses the first two embed calls that mention the
// canonical key, holding each apply/revert between its (already-completed)
// planning reads and its mutation transaction - the exact window in which
// a concurrent merge can commit and stale the plan. Reviewer probe (G02
// revision 3) kept as a permanent descriptive test.
type mergeBarrierEmbedder struct {
	calls   atomic.Int32
	entered [2]chan struct{}
	release [2]chan struct{}
}

func newMergeBarrierEmbedder() *mergeBarrierEmbedder {
	e := &mergeBarrierEmbedder{}
	for i := range e.entered {
		e.entered[i] = make(chan struct{})
		e.release[i] = make(chan struct{})
	}
	return e
}

func (e *mergeBarrierEmbedder) Dims() int { return 3 }

func (e *mergeBarrierEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	for _, s := range texts {
		if strings.Contains(s, "/entities/person/c") {
			n := int(e.calls.Add(1)) - 1
			if n < 2 {
				close(e.entered[n])
				select {
				case <-e.release[n]:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
		}
	}
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{1, 0, 0}
	}
	return out, nil
}

// TestConcurrentEntityMergesCannotLoseLineage: two valid merges A and B
// into ONE canonical prepared concurrently both read the initial
// canonical; the barrier pauses each before its transaction; the first
// commits, then the second must NOT commit its stale planned attrs over
// the first - canonical/alias mutations validate the expected predecessor
// inside the transaction and reject the stale plan
// (ErrEntityMergeStalePlan), leaving the first merge undoable.
func TestConcurrentEntityMergesCannotLoseLineage(t *testing.T) {
	s, _, _ := newTest(t)
	s.now = time.Now
	ctx := t.Context()
	reviewerG02Seed(t, s, "/entities/person/c", "Canonical Person", map[string]any{"entity_type": "person", "mention_count": 100, "aliases": []string{"Alice A", "Alice B"}})
	reviewerG02Seed(t, s, "/entities/person/a", "Alice A", map[string]any{"entity_type": "person"})
	reviewerG02Seed(t, s, "/entities/person/b", "Alice B", map[string]any{"entity_type": "person"})
	p1, p2 := reviewerG02Proposal(t, s, "/entities/person/a", "/entities/person/c"), reviewerG02Proposal(t, s, "/entities/person/b", "/entities/person/c")
	e := newMergeBarrierEmbedder()
	s.SetEmbedder(e)
	done1, done2 := make(chan error, 1), make(chan error, 1)
	go func() { _, err := s.ApplyEntityMerge(ctx, "ns", p1, MergeApplyOptions{}); done1 <- err }()
	select {
	case <-e.entered[0]:
	case <-time.After(3 * time.Second):
		t.Fatal("first merge failed to enter embed barrier")
	}
	go func() { _, err := s.ApplyEntityMerge(ctx, "ns", p2, MergeApplyOptions{}); done2 <- err }()
	// a correct implementation may serialize the second operation before
	// embedding; the barrier tolerates either shape.
	select {
	case <-e.entered[1]:
	case <-time.After(200 * time.Millisecond):
	}
	close(e.release[0])
	if err := <-done1; err != nil {
		close(e.release[1])
		<-done2
		t.Fatal(err)
	}
	close(e.release[1])
	if err := <-done2; err != nil {
		if !errors.Is(err, ErrEntityMergeStalePlan) {
			t.Fatalf("rejected concurrent merge must fail as stale plan, got %v", err)
		}
	} else {
		// serialized-then-replanned is also acceptable, but then the
		// canonical must retain BOTH merge records.
		fs, err := s.liveByKeys(ctx, "ns", []string{"/entities/person/c"})
		if err != nil || len(fs) != 1 {
			t.Fatalf("canonical %v %v", fs, err)
		}
		if !hasMergeRecord(fs[0], p1.ID) || !hasMergeRecord(fs[0], p2.ID) {
			t.Fatal("both concurrent merges succeeded, but canonical lost one merge record; an applied merge can no longer undo")
		}
		return
	}
	// rejected path: the first merge is intact and undoable, the second
	// alias is untouched.
	fs, err := s.liveByKeys(ctx, "ns", []string{"/entities/person/c", "/entities/person/b"})
	if err != nil || len(fs) != 2 {
		t.Fatalf("post-overlap state %v %v", fs, err)
	}
	byKey := map[string]Fact{}
	for _, f := range fs {
		byKey[f.Key] = f
	}
	if !hasMergeRecord(byKey["/entities/person/c"], p1.ID) {
		t.Fatal("committed first merge lost its lineage record")
	}
	if hasMergeRecord(byKey["/entities/person/c"], p2.ID) {
		t.Fatal("rejected stale merge still wrote its lineage record")
	}
	if mi := mergedIntoOf(byKey["/entities/person/b"]); mi != "" {
		t.Fatalf("rejected stale merge still marked alias merged_into %s", mi)
	}
}

// TestRevertApplyOverlapRejectsStalePlan: the revert path plans outside
// its transaction too, so a revert and an apply against the same
// canonical must get the same predecessor guard. Revert of merge A
// commits first; the concurrently planned apply of merge B then holds a
// stale canonical plan and is rejected with ErrEntityMergeStalePlan -
// never silently subtracting or overwriting the other's lineage.
func TestRevertApplyOverlapRejectsStalePlan(t *testing.T) {
	s, _, _ := newTest(t)
	s.now = time.Now
	ctx := t.Context()
	reviewerG02Seed(t, s, "/entities/person/c", "Canonical Person", map[string]any{"entity_type": "person", "mention_count": 100, "aliases": []string{"Alice A", "Alice B"}})
	reviewerG02Seed(t, s, "/entities/person/a", "Alice A", map[string]any{"entity_type": "person"})
	reviewerG02Seed(t, s, "/entities/person/b", "Alice B", map[string]any{"entity_type": "person"})
	p1 := reviewerG02Proposal(t, s, "/entities/person/a", "/entities/person/c")
	if _, err := s.ApplyEntityMerge(ctx, "ns", p1, MergeApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	p2 := reviewerG02Proposal(t, s, "/entities/person/b", "/entities/person/c")
	e := newMergeBarrierEmbedder()
	s.SetEmbedder(e)
	done1, done2 := make(chan error, 1), make(chan error, 1)
	go func() { _, err := s.RevertEntityMerge(ctx, "ns", "/entities/person/a"); done1 <- err }()
	select {
	case <-e.entered[0]:
	case <-time.After(3 * time.Second):
		t.Fatal("revert failed to enter embed barrier")
	}
	go func() { _, err := s.ApplyEntityMerge(ctx, "ns", p2, MergeApplyOptions{}); done2 <- err }()
	select {
	case <-e.entered[1]:
	case <-time.After(200 * time.Millisecond):
	}
	close(e.release[0])
	if err := <-done1; err != nil {
		close(e.release[1])
		<-done2
		t.Fatalf("revert: %v", err)
	}
	close(e.release[1])
	if err := <-done2; !errors.Is(err, ErrEntityMergeStalePlan) {
		t.Fatalf("overlapping apply on a reverted canonical must be rejected as stale plan, got %v", err)
	}
	fs, err := s.liveByKeys(ctx, "ns", []string{"/entities/person/c", "/entities/person/a", "/entities/person/b"})
	if err != nil || len(fs) != 3 {
		t.Fatalf("post-overlap state %v %v", fs, err)
	}
	byKey := map[string]Fact{}
	for _, f := range fs {
		byKey[f.Key] = f
	}
	if hasMergeRecord(byKey["/entities/person/c"], p1.ID) || hasMergeRecord(byKey["/entities/person/c"], p2.ID) {
		t.Fatal("canonical kept lineage of the reverted merge or gained the rejected merge's record")
	}
	if mi := mergedIntoOf(byKey["/entities/person/a"]); mi != "" {
		t.Fatalf("reverted alias still merged_into %s", mi)
	}
	if mi := mergedIntoOf(byKey["/entities/person/b"]); mi != "" {
		t.Fatalf("rejected apply still marked alias merged_into %s", mi)
	}
}

// advanceHeadInTx simulates the concurrent writer from the narrower
// read-committed race window: it commits AFTER the victim's predecessor
// CAS read but BEFORE the victim's invalidating UPDATE. Running inside
// the victim transaction makes the interleaving deterministic on every
// driver (same-transaction writes are visible to later statements), so
// the victim's predicate-less UPDATE would match the interloper's fresh
// live row and commit the stale plan undetected - the exact failure the
// id-bound predicate must prevent.
func advanceHeadInTx(ctx context.Context, s *Store, tx *sql.Tx, nsID int64, prev Fact, now time.Time) error {
	if _, err := tx.ExecContext(ctx, s.db.Rebind(
		`UPDATE memories SET invalid_at = $1 WHERE id = $2`), store.TimeToDB(now), prev.ID); err != nil {
		return err
	}
	attrs := `{"interloper":true}`
	_, err := tx.ExecContext(ctx, s.db.Rebind(
		`INSERT INTO memories (id, namespace_id, key, action, body, attributes, author, created_at,
		                       writer, confidence, valid_at, embedding, content_hash, importance)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`),
		newID(), nsID, prev.Key, prev.Action, prev.Body, attrs, "", store.TimeToDB(now),
		"entity-merge-interloper", 0.0, store.TimeToDB(now), nil, contentHash(prev.Key, prev.Body, attrs), prev.Importance)
	return err
}

// armInterloper installs the commit-window hook once, on the given key
// only, and reports whether it fired.
func armInterloper(s *Store, key string) *atomic.Bool {
	fired := &atomic.Bool{}
	s.mergePreInvalidateHook = func(ctx context.Context, tx *sql.Tx, nsID int64, prev Fact) error {
		if fired.Load() || prev.Key != key {
			return nil
		}
		fired.Store(true)
		return advanceHeadInTx(ctx, s, tx, nsID, prev, s.now())
	}
	return fired
}

// TestWriteMergeRevisionTxBindsInvalidationToPlannedPredecessor is the
// SQL-layer proof for the r5 correction: a head that advances between
// the CAS read and the invalidating UPDATE (hook-driven, deterministic)
// must reject the mutation with ErrEntityMergeStalePlan and roll the
// whole transaction back - the planned predecessor stays the live head,
// neither the interloper nor the stale plan revision survives. With the
// old predicate-less UPDATE the interloper's fresh row matched
// invalid_at IS NULL, RowsAffected was 1, and the stale plan committed.
func TestWriteMergeRevisionTxBindsInvalidationToPlannedPredecessor(t *testing.T) {
	s, _, _ := newTest(t)
	s.now = time.Now
	ctx := t.Context()
	reviewerG02Seed(t, s, "/entities/person/c", "Canonical Person", map[string]any{"entity_type": "person"})
	fs, err := s.liveByKeys(ctx, "ns", []string{"/entities/person/c"})
	if err != nil || len(fs) != 1 {
		t.Fatalf("seed: %v %v", fs, err)
	}
	prev := fs[0]
	fired := armInterloper(s, prev.Key)
	defer func() { s.mergePreInvalidateHook = nil }()
	attrs := copyAttrs(prev.Attributes)
	attrs["merge_marker"] = "stale-plan"
	err = s.db.WithTx(ctx, func(tx *sql.Tx) error {
		return s.writeMergeRevisionTx(ctx, tx, "ns", prev, attrs, nil, s.now())
	})
	if !errors.Is(err, ErrEntityMergeStalePlan) {
		t.Fatalf("head advanced in the commit window must reject as stale plan, got %v", err)
	}
	if !fired.Load() {
		t.Fatal("hook never fired - the commit window was not exercised")
	}
	after, err := s.liveByKeys(ctx, "ns", []string{"/entities/person/c"})
	if err != nil || len(after) != 1 {
		t.Fatalf("post-rollback state %v %v", after, err)
	}
	if after[0].ID != prev.ID {
		t.Fatalf("rollback failed: live head = %s, want planned predecessor %s", after[0].ID, prev.ID)
	}
	if _, ok := after[0].Attributes["interloper"]; ok {
		t.Fatal("interloper revision survived the rollback")
	}
	if _, ok := after[0].Attributes["merge_marker"]; ok {
		t.Fatal("stale plan attributes committed")
	}
}

// TestApplyEntityMergeStaleHeadInCommitWindowStaysUndoable drives the
// same commit-window race through the full Apply path: merge A commits,
// then merge B's apply sees the canonical head advance between its CAS
// read and its invalidating UPDATE. B must be rejected as a stale plan,
// its mention edges/lineage/alias markers must roll back completely,
// and merge A must remain undoable afterwards.
func TestApplyEntityMergeStaleHeadInCommitWindowStaysUndoable(t *testing.T) {
	s, _, _ := newTest(t)
	s.now = time.Now
	ctx := t.Context()
	reviewerG02Seed(t, s, "/entities/person/c", "Canonical Person", map[string]any{"entity_type": "person", "mention_count": 100, "aliases": []string{"Alice A", "Alice B"}})
	reviewerG02Seed(t, s, "/entities/person/a", "Alice A", map[string]any{"entity_type": "person"})
	reviewerG02Seed(t, s, "/entities/person/b", "Alice B", map[string]any{"entity_type": "person"})
	p1 := reviewerG02Proposal(t, s, "/entities/person/a", "/entities/person/c")
	if _, err := s.ApplyEntityMerge(ctx, "ns", p1, MergeApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	p2 := reviewerG02Proposal(t, s, "/entities/person/b", "/entities/person/c")
	fired := armInterloper(s, "/entities/person/c")
	defer func() { s.mergePreInvalidateHook = nil }()
	if _, err := s.ApplyEntityMerge(ctx, "ns", p2, MergeApplyOptions{}); !errors.Is(err, ErrEntityMergeStalePlan) {
		t.Fatalf("apply over a head advanced in the commit window must be stale plan, got %v", err)
	}
	if !fired.Load() {
		t.Fatal("hook never fired - the commit window was not exercised")
	}
	fs, err := s.liveByKeys(ctx, "ns", []string{"/entities/person/c", "/entities/person/b"})
	if err != nil || len(fs) != 2 {
		t.Fatalf("post-rollback state %v %v", fs, err)
	}
	byKey := map[string]Fact{}
	for _, f := range fs {
		byKey[f.Key] = f
	}
	if !hasMergeRecord(byKey["/entities/person/c"], p1.ID) {
		t.Fatal("first merge's lineage record lost to the rejected apply")
	}
	if hasMergeRecord(byKey["/entities/person/c"], p2.ID) {
		t.Fatal("rejected stale apply wrote its lineage record")
	}
	if _, ok := byKey["/entities/person/c"].Attributes["interloper"]; ok {
		t.Fatal("interloper revision survived the rollback")
	}
	if mi := mergedIntoOf(byKey["/entities/person/b"]); mi != "" {
		t.Fatalf("rejected stale apply still marked alias merged_into %s", mi)
	}
	if _, err := s.RevertEntityMerge(ctx, "ns", "/entities/person/a"); err != nil {
		t.Fatalf("first merge no longer undoable after the rejected apply: %v", err)
	}
}

// TestRevertEntityMergeStaleHeadInCommitWindowRollsBack drives the
// commit-window race through the Revert path: the canonical head
// advances between the undo's CAS read and its invalidating UPDATE. The
// undo must be rejected as a stale plan, the merge must stay fully in
// place (alias lineage included), and a retry after the concurrent
// writer's state settles must still undo cleanly.
func TestRevertEntityMergeStaleHeadInCommitWindowRollsBack(t *testing.T) {
	s, _, _ := newTest(t)
	s.now = time.Now
	ctx := t.Context()
	reviewerG02Seed(t, s, "/entities/person/c", "Canonical Person", map[string]any{"entity_type": "person", "mention_count": 100, "aliases": []string{"Alice A"}})
	reviewerG02Seed(t, s, "/entities/person/a", "Alice A", map[string]any{"entity_type": "person"})
	p1 := reviewerG02Proposal(t, s, "/entities/person/a", "/entities/person/c")
	if _, err := s.ApplyEntityMerge(ctx, "ns", p1, MergeApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	fired := armInterloper(s, "/entities/person/c")
	if _, err := s.RevertEntityMerge(ctx, "ns", "/entities/person/a"); !errors.Is(err, ErrEntityMergeStalePlan) {
		t.Fatalf("revert over a head advanced in the commit window must be stale plan, got %v", err)
	}
	s.mergePreInvalidateHook = nil
	if !fired.Load() {
		t.Fatal("hook never fired - the commit window was not exercised")
	}
	fs, err := s.liveByKeys(ctx, "ns", []string{"/entities/person/c", "/entities/person/a"})
	if err != nil || len(fs) != 2 {
		t.Fatalf("post-rollback state %v %v", fs, err)
	}
	byKey := map[string]Fact{}
	for _, f := range fs {
		byKey[f.Key] = f
	}
	if !hasMergeRecord(byKey["/entities/person/c"], p1.ID) {
		t.Fatal("rejected undo still removed the merge's lineage record")
	}
	if _, ok := byKey["/entities/person/c"].Attributes["interloper"]; ok {
		t.Fatal("interloper revision survived the rollback")
	}
	if mi := mergedIntoOf(byKey["/entities/person/a"]); mi != "/entities/person/c" {
		t.Fatalf("rejected undo still cleared alias lineage, merged_into = %q", mi)
	}
	if _, err := s.RevertEntityMerge(ctx, "ns", "/entities/person/a"); err != nil {
		t.Fatalf("retry after the stale-plan rejection must undo cleanly: %v", err)
	}
}

// enrichTargetBarrier pauses the first embed call naming the merge
// canonical - the entity stage's TARGET write preparation - holding the
// stage between its plan reads (the pre-merge target revision) and its
// fenced transaction: the exact window in which a concurrent merge can
// commit. The SOURCE revision never moves, so the source/run fence
// alone cannot see the target staleness. Reviewer probe (G02 revision
// 5) kept as a permanent descriptive test.
type enrichTargetBarrier struct {
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (e *enrichTargetBarrier) Dims() int { return 3 }

func (e *enrichTargetBarrier) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	for _, s := range texts {
		if strings.Contains(s, "/entities/service/mercury") && e.calls.Add(1) == 1 {
			close(e.entered)
			select {
			case <-e.release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{1, 0, 0}
	}
	return out, nil
}

// TestConcurrentEnrichmentCannotEraseMergeLineage: a typed entity-stage
// apply that planned against the PRE-merge target revision must not
// commit over a merge that landed while the stage was held at target
// embedding. The pinned-predecessor invalidation in writeTx rejects the
// stale plan (ErrEntityApplyStaleTarget), the run stays retryable
// (RunFailed, not superseded - no newer SOURCE revision owns the work),
// and recovery re-plans from the merge head: the lineage record
// survives every step and the merge remains undoable at the end.
func TestConcurrentEnrichmentCannotEraseMergeLineage(t *testing.T) {
	s, _, _ := newTest(t)
	s.now = time.Now
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	reviewerG02Seed(t, s, "/entities/service/mercury", "Mercury", map[string]any{"entity_type": "service", "mention_count": 100, "aliases": []string{"Merc"}})
	reviewerG02Seed(t, s, "/entities/service/merc", "Merc", map[string]any{"entity_type": "service"})
	p := reviewerG02Proposal(t, s, "/entities/service/merc", "/entities/service/mercury")
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f", Body: "Mercury service"}); err != nil {
		t.Fatal(err)
	}
	s.SetEntityExtractor(reviewerP01TypedExtractor{})
	e := &enrichTargetBarrier{entered: make(chan struct{}), release: make(chan struct{})}
	s.SetEmbedder(e)
	done := make(chan struct{})
	go func() { defer close(done); s.flushEntityStage(ctx, "ns", []string{"/f"}, testLog()) }()
	select {
	case <-e.entered:
	case <-ctx.Done():
		t.Fatal("enrichment did not reach target embedding")
	}
	// the enrichment plan has read the old target attributes but has
	// not begun its fenced transaction.
	result, err := s.ApplyEntityMerge(ctx, "ns", p, MergeApplyOptions{})
	close(e.release)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("enrichment failed to finish")
	}
	if err != nil || !result.Applied {
		t.Fatalf("merge did not apply: %+v %v", result, err)
	}
	facts, err := s.liveByKeys(ctx, "ns", []string{"/entities/service/mercury"})
	if err != nil || len(facts) != 1 {
		t.Fatalf("canonical %v %v", facts, err)
	}
	if !hasMergeRecord(facts[0], p.ID) {
		t.Fatal("successful merge lineage was erased by a concurrent enrichment plan; source/run fence did not protect the changed target revision")
	}
	runs, err := s.ListPipelineRuns(ctx, "ns", StageEntities, "", 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("entity runs %v %v", runs, err)
	}
	if runs[0].Status != RunFailed {
		t.Fatalf("rejected stale enrichment must leave a retryable failed run, got %s", runs[0].Status)
	}
	// recovery requeues the failed run; the retry re-plans from the
	// merge head and applies over it instead of erasing it.
	s.recoverPipelineRuns(ctx, testLog())
	runs, err = s.ListPipelineRuns(ctx, "ns", StageEntities, "", 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("entity runs after recovery %v %v", runs, err)
	}
	if runs[0].Status != RunSucceeded {
		t.Fatalf("recovered run status = %s, want succeeded", runs[0].Status)
	}
	facts, err = s.liveByKeys(ctx, "ns", []string{"/entities/service/mercury"})
	if err != nil || len(facts) != 1 {
		t.Fatalf("canonical after recovery %v %v", facts, err)
	}
	if !hasMergeRecord(facts[0], p.ID) {
		t.Fatal("recovered enrichment erased the merge lineage it applied over")
	}
	if _, err := s.RevertEntityMerge(ctx, "ns", "/entities/service/merc"); err != nil {
		t.Fatalf("merge no longer undoable after concurrent enrichment completed: %v", err)
	}
}

// TestOriginallyAbsentTargetCannotOverwriteLaterMerge: the dual of the
// existing-target race - the entity stage planned to CREATE the target
// (it had no live revision at plan time), then a different writer
// created Mercury and merged into it while the stage was held at target
// embedding. The pinned-absence check in writeTx must reject the stale
// create (expectAbsent, distinct from unpinned plain writes): the merge
// head stays live and undoable, the run stays retryable, and the
// recovered retry completes the mention without touching the merge.
// Reviewer probe (G02 revision 5, absent-target preflight) kept as a
// permanent descriptive test.
func TestOriginallyAbsentTargetCannotOverwriteLaterMerge(t *testing.T) {
	s, _, _ := newTest(t)
	s.now = time.Now
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	reviewerG02Seed(t, s, "/entities/service/merc", "Merc", map[string]any{"entity_type": "service"})
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f", Body: "Mercury service"}); err != nil {
		t.Fatal(err)
	}
	s.SetEntityExtractor(reviewerP01TypedExtractor{})
	e := &enrichTargetBarrier{entered: make(chan struct{}), release: make(chan struct{})}
	s.SetEmbedder(e)
	done := make(chan struct{})
	go func() { defer close(done); s.flushEntityStage(ctx, "ns", []string{"/f"}, testLog()) }()
	select {
	case <-e.entered:
	case <-ctx.Done():
		t.Fatal("enrichment did not reach target embedding")
	}
	// this target did not exist when enrichment prepared its
	// attributes; a different writer now creates and merges it.
	reviewerG02Seed(t, s, "/entities/service/mercury", "Mercury", map[string]any{"entity_type": "service", "mention_count": 100, "aliases": []string{"Merc"}})
	p := reviewerG02Proposal(t, s, "/entities/service/merc", "/entities/service/mercury")
	result, err := s.ApplyEntityMerge(ctx, "ns", p, MergeApplyOptions{})
	close(e.release)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("enrichment failed to finish")
	}
	if err != nil || !result.Applied {
		t.Fatalf("merge did not apply: %+v %v", result, err)
	}
	facts, err := s.liveByKeys(ctx, "ns", []string{"/entities/service/mercury"})
	if err != nil || len(facts) != 1 {
		t.Fatalf("canonical %v %v", facts, err)
	}
	if !hasMergeRecord(facts[0], p.ID) {
		t.Fatal("merge lineage erased by a stale create from a plan that observed the target absent")
	}
	runs, err := s.ListPipelineRuns(ctx, "ns", StageEntities, "", 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("entity runs %v %v", runs, err)
	}
	if runs[0].Status != RunFailed {
		t.Fatalf("rejected stale create must leave a retryable failed run, got %s", runs[0].Status)
	}
	if _, err := s.RevertEntityMerge(ctx, "ns", "/entities/service/merc"); err != nil {
		t.Fatalf("previously-absent target plan invalidated later merge: %v", err)
	}
	// the retry still completes the enrichment work afterwards,
	// re-planning from the post-revert head.
	s.recoverPipelineRuns(ctx, testLog())
	runs, err = s.ListPipelineRuns(ctx, "ns", StageEntities, "", 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("entity runs after recovery %v %v", runs, err)
	}
	if runs[0].Status != RunSucceeded {
		t.Fatalf("recovered run status = %s, want succeeded", runs[0].Status)
	}
}

// assertMentions pins that fromKey carries a live mentions edge to toKey.
func assertMentions(t *testing.T, s *Store, ctx context.Context, fromKey, toKey string) {
	t.Helper()
	links, err := s.Neighbors(ctx, "ns", fromKey, "out")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range links {
		if l.LinkType == "mentions" && l.ToKey == toKey {
			return
		}
	}
	t.Fatalf("%s must mention %s after the replanned apply completed: %v", fromKey, toKey, links)
}

// TestDirectEnrichmentCannotEraseMergeLineage is the same target race as
// TestConcurrentEnrichmentCannotEraseMergeLineage but through the DIRECT
// public API (EnrichEntitiesBatch, typed extractor - no pipeline run, no
// source fence): the plan reads the PRE-merge target revision, holds at
// target embedding, a merge commits in the window, and the stale derived
// write must not commit over it. The pinned target baseline
// (expectPrevID in writeTx) rejects the stale apply with
// ErrEntityApplyStaleTarget; the direct path's recoverable behavior is
// to re-plan from the merge head and complete its mention over it, the
// lineage riding the new revision via preserveEntityAttrs. Reviewer
// probe (G02 revision 5, direct path) kept as a permanent descriptive
// test.
func TestDirectEnrichmentCannotEraseMergeLineage(t *testing.T) {
	s, _, _ := newTest(t)
	s.now = time.Now
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	reviewerG02Seed(t, s, "/entities/service/mercury", "Mercury", map[string]any{"entity_type": "service", "mention_count": 100, "aliases": []string{"Merc"}})
	reviewerG02Seed(t, s, "/entities/service/merc", "Merc", map[string]any{"entity_type": "service"})
	p := reviewerG02Proposal(t, s, "/entities/service/merc", "/entities/service/mercury")
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f", Body: "Mercury service"}); err != nil {
		t.Fatal(err)
	}
	s.SetEntityExtractor(reviewerP01TypedExtractor{})
	e := &enrichTargetBarrier{entered: make(chan struct{}), release: make(chan struct{})}
	s.SetEmbedder(e)
	done := make(chan struct{})
	go func() { defer close(done); _, _ = s.EnrichEntitiesBatch(ctx, "ns", []string{"/f"}) }()
	select {
	case <-e.entered:
	case <-ctx.Done():
		t.Fatal("enrichment did not reach target embedding")
	}
	// the enrichment plan has read the old target attributes but has
	// not begun its apply transaction.
	result, err := s.ApplyEntityMerge(ctx, "ns", p, MergeApplyOptions{})
	close(e.release)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("enrichment failed to finish")
	}
	if err != nil || !result.Applied {
		t.Fatalf("merge did not apply: %+v %v", result, err)
	}
	facts, err := s.liveByKeys(ctx, "ns", []string{"/entities/service/mercury"})
	if err != nil || len(facts) != 1 {
		t.Fatalf("canonical %v %v", facts, err)
	}
	if !hasMergeRecord(facts[0], p.ID) {
		t.Fatal("successful merge lineage was erased by a concurrent direct enrichment; pinned target writes did not protect the changed target revision")
	}
	// recoverable behavior on the stale rejection: the direct apply
	// replanned from the merge head and completed its mention over it.
	assertMentions(t, s, ctx, "/f", "/entities/service/mercury")
	if _, err := s.RevertEntityMerge(ctx, "ns", "/entities/service/merc"); err != nil {
		t.Fatalf("merge no longer undoable after concurrent direct enrichment completed: %v", err)
	}
}

// TestDirectEnrichmentAbsentTargetCannotOverwriteLaterMerge is the
// direct-path dual of TestOriginallyAbsentTargetCannotOverwriteLaterMerge:
// EnrichEntitiesBatch planned to CREATE the target (no live revision at
// plan time), then a different writer created Mercury and merged into it
// while the plan held at target embedding. The pinned-absence check in
// writeTx (expectAbsent) must reject the stale create, and the replanned
// apply completes over the merge head without touching its lineage.
func TestDirectEnrichmentAbsentTargetCannotOverwriteLaterMerge(t *testing.T) {
	s, _, _ := newTest(t)
	s.now = time.Now
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	reviewerG02Seed(t, s, "/entities/service/merc", "Merc", map[string]any{"entity_type": "service"})
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f", Body: "Mercury service"}); err != nil {
		t.Fatal(err)
	}
	s.SetEntityExtractor(reviewerP01TypedExtractor{})
	e := &enrichTargetBarrier{entered: make(chan struct{}), release: make(chan struct{})}
	s.SetEmbedder(e)
	done := make(chan struct{})
	go func() { defer close(done); _, _ = s.EnrichEntitiesBatch(ctx, "ns", []string{"/f"}) }()
	select {
	case <-e.entered:
	case <-ctx.Done():
		t.Fatal("enrichment did not reach target embedding")
	}
	// this target did not exist when enrichment prepared its
	// attributes; a different writer now creates and merges it.
	reviewerG02Seed(t, s, "/entities/service/mercury", "Mercury", map[string]any{"entity_type": "service", "mention_count": 100, "aliases": []string{"Merc"}})
	p := reviewerG02Proposal(t, s, "/entities/service/merc", "/entities/service/mercury")
	result, err := s.ApplyEntityMerge(ctx, "ns", p, MergeApplyOptions{})
	close(e.release)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("enrichment failed to finish")
	}
	if err != nil || !result.Applied {
		t.Fatalf("merge did not apply: %+v %v", result, err)
	}
	facts, err := s.liveByKeys(ctx, "ns", []string{"/entities/service/mercury"})
	if err != nil || len(facts) != 1 {
		t.Fatalf("canonical %v %v", facts, err)
	}
	if !hasMergeRecord(facts[0], p.ID) {
		t.Fatal("merge lineage erased by a stale direct create from a plan that observed the target absent")
	}
	assertMentions(t, s, ctx, "/f", "/entities/service/mercury")
	if _, err := s.RevertEntityMerge(ctx, "ns", "/entities/service/merc"); err != nil {
		t.Fatalf("previously-absent direct target plan invalidated later merge: %v", err)
	}
}

// TestNewMentionOfTombstonedEntityDoesNotFailStale: a stably TOMBstoned
// entity is a legitimate recreation baseline, not a concurrent creator.
// The plan observes no live entity but must pin the actual mutation head
// (the tombstone revision) as its expected predecessor; pinning absence
// instead would make writeTx's head check mistake the stable tombstone
// for a concurrent create and reject every retry as ErrEntityApplyStaleTarget
// with no writer present. Recreation from genuinely new evidence succeeds
// and the entity is live again. Reviewer probe (G02 revision 6) kept as a
// permanent descriptive test.
func TestNewMentionOfTombstonedEntityDoesNotFailStale(t *testing.T) {
	s, _, _ := newTest(t)
	s.now = time.Now
	ctx := t.Context()
	reviewerG02Seed(t, s, "/entities/service/mercury", "Mercury", map[string]any{"entity_type": "service"})
	if err := s.Forget(ctx, "ns", "/entities/service/mercury", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/new-source", Body: "Mercury service newly mentioned"}); err != nil {
		t.Fatal(err)
	}
	s.SetEntityExtractor(reviewerP01TypedExtractor{})
	if _, err := s.EnrichEntitiesBatch(ctx, "ns", []string{"/new-source"}); err != nil {
		t.Fatalf("new mention with stable tombstone treated as concurrent stale target: %v", err)
	}
	facts, err := s.liveByKeys(ctx, "ns", []string{"/entities/service/mercury"})
	if err != nil || len(facts) != 1 {
		t.Fatalf("recreated entity %v %v", facts, err)
	}
	if mc := facts[0].Attributes["mention_count"]; mc != float64(1) {
		t.Fatalf("recreated mention_count = %v, want 1 (fresh recreation from new evidence, old counts do not revive)", mc)
	}
	assertMentions(t, s, ctx, "/new-source", "/entities/service/mercury")
}

// TestLinkedSourceDoesNotResurrectTombstonedEntity is the dual guard: an
// OLD source whose mentions edge already exists must not recreate a
// tombstoned entity, typed or legacy - recreation is reserved for
// genuinely new evidence.
func TestLinkedSourceDoesNotResurrectTombstonedEntity(t *testing.T) {
	s, _, _ := newTest(t)
	s.now = time.Now
	ctx := t.Context()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/old", Body: "Mercury service"}); err != nil {
		t.Fatal(err)
	}
	s.SetEntityExtractor(reviewerP01TypedExtractor{})
	if _, err := s.EnrichEntities(ctx, "ns", "/old"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.liveByKeys(ctx, "ns", []string{"/entities/service/mercury"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Forget(ctx, "ns", "/entities/service/mercury", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnrichEntities(ctx, "ns", "/old"); err != nil {
		t.Fatalf("re-enrich of already-linked source: %v", err)
	}
	if facts, err := s.liveByKeys(ctx, "ns", []string{"/entities/service/mercury"}); err != nil || len(facts) != 0 {
		t.Fatalf("already-linked old source resurrected the tombstoned entity: %v %v", facts, err)
	}

	// legacy name-only path: the same guarantee via the linked-edge skip.
	s2, _, _ := newTest(t)
	s2.now = time.Now
	if _, err := s2.Write(ctx, WriteInput{Namespace: "ns", Key: "/old", Body: "Alice works"}); err != nil {
		t.Fatal(err)
	}
	s2.SetEntityExtractor(fakeExtractor{names: []string{"Alice"}})
	if _, err := s2.EnrichEntities(ctx, "ns", "/old"); err != nil {
		t.Fatal(err)
	}
	if err := s2.Forget(ctx, "ns", "/entities/alice", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.EnrichEntities(ctx, "ns", "/old"); err != nil {
		t.Fatalf("legacy re-enrich of already-linked source: %v", err)
	}
	if facts, err := s2.liveByKeys(ctx, "ns", []string{"/entities/alice"}); err != nil || len(facts) != 0 {
		t.Fatalf("legacy already-linked old source resurrected the tombstoned entity: %v %v", facts, err)
	}
}

// TestNewMentionOfExpiredEntityDoesNotFailStale is the expired-head
// analogue of the tombstone case: an entity whose head revision is past
// its expiration_date is filtered out of liveByKeys exactly like a
// tombstone, so the plan must pin that expired head as its recreation
// baseline instead of pinning absence. Recreation from new evidence
// succeeds and the new revision carries no expiry.
func TestNewMentionOfExpiredEntityDoesNotFailStale(t *testing.T) {
	s, _, _ := newTest(t)
	s.now = time.Now
	ctx := t.Context()
	past := time.Now().Add(-time.Hour)
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/entities/service/mercury", Body: "Mercury",
		Attributes: map[string]any{"entity_type": "service"}, ExpiresAt: &past}); err != nil {
		t.Fatal(err)
	}
	if facts, err := s.liveByKeys(ctx, "ns", []string{"/entities/service/mercury"}); err != nil || len(facts) != 0 {
		t.Fatalf("expired entity should be filtered from live reads: %v %v", facts, err)
	}
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/new-source", Body: "Mercury service newly mentioned"}); err != nil {
		t.Fatal(err)
	}
	s.SetEntityExtractor(reviewerP01TypedExtractor{})
	if _, err := s.EnrichEntitiesBatch(ctx, "ns", []string{"/new-source"}); err != nil {
		t.Fatalf("new mention with stable expired head treated as concurrent stale target: %v", err)
	}
	facts, err := s.liveByKeys(ctx, "ns", []string{"/entities/service/mercury"})
	if err != nil || len(facts) != 1 {
		t.Fatalf("recreated entity %v %v", facts, err)
	}
	if facts[0].ExpiresAt != nil {
		t.Fatalf("recreated revision must not inherit the expired head's expiry: %v", facts[0].ExpiresAt)
	}
	assertMentions(t, s, ctx, "/new-source", "/entities/service/mercury")
}

// TestDirectLegacyEnrichmentCannotEraseMergeLineage drives the same
// target race through the legacy name-only extractor path (no
// StructuredEntityExtractor): planEntityApply's pinned baseline must
// reject the stale derived write the same way, and the replanned legacy
// apply must carry the merge lineage forward (carryEntityMergeLineage)
// instead of rebuilding the target revision without it.
func TestDirectLegacyEnrichmentCannotEraseMergeLineage(t *testing.T) {
	s, _, _ := newTest(t)
	s.now = time.Now
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	reviewerG02Seed(t, s, "/entities/service/mercury", "Mercury", map[string]any{"entity_type": "service", "mention_count": 100, "aliases": []string{"Merc"}})
	reviewerG02Seed(t, s, "/entities/service/merc", "Merc", map[string]any{"entity_type": "service"})
	p := reviewerG02Proposal(t, s, "/entities/service/merc", "/entities/service/mercury")
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f", Body: "Mercury service"}); err != nil {
		t.Fatal(err)
	}
	s.SetEntityExtractor(fakeExtractor{names: []string{"Mercury"}})
	e := &enrichTargetBarrier{entered: make(chan struct{}), release: make(chan struct{})}
	s.SetEmbedder(e)
	done := make(chan struct{})
	go func() { defer close(done); _, _ = s.EnrichEntitiesBatch(ctx, "ns", []string{"/f"}) }()
	select {
	case <-e.entered:
	case <-ctx.Done():
		t.Fatal("enrichment did not reach target embedding")
	}
	result, err := s.ApplyEntityMerge(ctx, "ns", p, MergeApplyOptions{})
	close(e.release)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("enrichment failed to finish")
	}
	if err != nil || !result.Applied {
		t.Fatalf("merge did not apply: %+v %v", result, err)
	}
	facts, err := s.liveByKeys(ctx, "ns", []string{"/entities/service/mercury"})
	if err != nil || len(facts) != 1 {
		t.Fatalf("canonical %v %v", facts, err)
	}
	if !hasMergeRecord(facts[0], p.ID) {
		t.Fatal("successful merge lineage was erased by a concurrent legacy direct enrichment")
	}
	assertMentions(t, s, ctx, "/f", "/entities/service/mercury")
	if _, err := s.RevertEntityMerge(ctx, "ns", "/entities/service/merc"); err != nil {
		t.Fatalf("merge no longer undoable after concurrent legacy direct enrichment completed: %v", err)
	}
}

func TestEntityPlanHeadQuarantinesBadRowWithoutBlocking(t *testing.T) {
	s, db, _ := newTest(t)
	ctx := t.Context()
	f, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/entities/service/mercury", Body: "Mercury"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, db.Rebind(`UPDATE memories SET attributes = $1 WHERE id = $2`), "{bad", f.ID); err != nil {
		t.Fatal(err)
	}
	deadline, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	head, _, err := s.entityPlanHead(deadline, "ns", "/entities/service/mercury")
	if err != nil {
		t.Fatalf("poisoned head failed quarantine while reader still holds sole DB connection: %v", err)
	}
	if head != nil {
		t.Fatal("poisoned head should be quarantined")
	}
	var n int
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM memories_quarantine").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("quarantined=%d, want1", n)
	}
}
