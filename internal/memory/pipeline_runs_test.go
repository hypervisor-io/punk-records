package memory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// errExtractor fails entity extraction with a fixed error.
type errExtractor struct{ err error }

func (e errExtractor) Extract(_ context.Context, _ string) ([]string, error) {
	return nil, e.err
}

// countingStructuredFake implements StructuredEntityExtractor and
// records the size of every ExtractStructured call, in order, so a test
// can pin exactly how recovery chunked its batches (see structuredFake
// in entity_typed_test.go for the same shape without the size history).
type countingStructuredFake struct {
	calls []int
}

func (f *countingStructuredFake) Extract(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (f *countingStructuredFake) ExtractStructured(_ context.Context, sources []EntitySource) ([]ExtractedEntity, error) {
	f.calls = append(f.calls, len(sources))
	return nil, nil
}

func testLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

// findRun returns the single run matching stage+key, or fails.
func findRun(t *testing.T, s *Store, ns, stage, key string) PipelineRun {
	t.Helper()
	runs, err := s.ListPipelineRuns(t.Context(), ns, stage, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		if r.SourceKey == key {
			return r
		}
	}
	t.Fatalf("no %s run for %s in %v", stage, key, runs)
	return PipelineRun{}
}

func TestPipelineRunLifecycleAndDedup(t *testing.T) {
	s, _, _ := newTest(t)
	s.SetEmbedder(&fakeEmbedder{m: map[string][]float32{
		"ceph osd flapping on node 3":  {1, 0, 0},
		"osd flap traced to nic reset": {0.98, 0.15, 0},
	}})
	ctx := t.Context()
	for k, b := range map[string]string{
		"/a": "ceph osd flapping on node 3",
		"/b": "osd flap traced to nic reset",
	} {
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: k, Body: b}); err != nil {
			t.Fatal(err)
		}
	}

	run, attempt, claimed, err := s.claimPipelineWork(ctx, "ns", StageEmbedLink, embedLinkStageVersion, "/a")
	if err != nil || !claimed {
		t.Fatalf("claim = claimed %v, err %v; want claimed", claimed, err)
	}
	if run.Status != RunRunning || attempt != 1 || run.Attempts != 1 {
		t.Fatalf("run = %+v attempt %d, want running attempt 1", run, attempt)
	}
	if run.StartedAt == nil {
		t.Fatal("claimed run has no started_at")
	}
	facts, err := s.Recall(ctx, "ns", "/a", 1)
	if err != nil || len(facts) != 1 {
		t.Fatalf("recall: %v %v", facts, err)
	}
	if run.SourceRevision != facts[0].ID {
		t.Fatalf("source_revision = %q, want live revision %q", run.SourceRevision, facts[0].ID)
	}

	// at-least-once redelivery while running: not claimed again
	if _, _, again, err := s.claimPipelineWork(ctx, "ns", StageEmbedLink, embedLinkStageVersion, "/a"); err != nil || again {
		t.Fatalf("re-claim while running = claimed %v, err %v; want not claimed", again, err)
	}

	added, err := s.EnrichKey(ctx, "ns", "/a", 0.75, 2)
	if err != nil || added != 1 {
		t.Fatalf("enrich = %d, %v; want 1", added, err)
	}
	items := int64(added)
	applied, err := s.finishPipelineRun(ctx, run.ID, attempt, RunSucceeded, &items, "")
	if err != nil || !applied {
		t.Fatalf("finish = applied %v, err %v; want applied", applied, err)
	}

	// terminal run: a duplicate delivery must not re-execute
	if _, _, again, err := s.claimPipelineWork(ctx, "ns", StageEmbedLink, embedLinkStageVersion, "/a"); err != nil || again {
		t.Fatalf("claim after success = claimed %v, err %v; want not claimed", again, err)
	}

	r := findRun(t, s, "ns", StageEmbedLink, "/a")
	if r.Status != RunSucceeded || r.Attempts != 1 {
		t.Fatalf("run = %+v, want succeeded attempts 1", r)
	}
	if r.Items == nil || *r.Items != 1 {
		t.Fatalf("items = %v, want 1", r.Items)
	}
	if r.FinishedAt == nil || r.ErrorClass != "" {
		t.Fatalf("run = %+v, want finished with no error class", r)
	}
}

// TestPipelineRunCrashBeforeAckRetryIdempotent is the P01 red proof for
// the embed-link stage: the worker writes stage output (a similar_to
// link) and crashes before acknowledging the run. Restart recovery must
// retry the run without duplicating derived state, then record success
// with the retry visible in attempts.
func TestPipelineRunCrashBeforeAckRetryIdempotent(t *testing.T) {
	s, _, clk := newTest(t)
	s.SetEmbedder(&fakeEmbedder{m: map[string][]float32{
		"ceph osd flapping on node 3":  {1, 0, 0},
		"osd flap traced to nic reset": {0.98, 0.15, 0},
		"tuesday lunch menu":           {0, 1, 0},
	}})
	ctx := t.Context()
	for k, b := range map[string]string{
		"/a": "ceph osd flapping on node 3",
		"/b": "osd flap traced to nic reset",
		"/c": "tuesday lunch menu",
	} {
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: k, Body: b}); err != nil {
			t.Fatal(err)
		}
	}

	run, attempt, claimed, err := s.claimPipelineWork(ctx, "ns", StageEmbedLink, embedLinkStageVersion, "/a")
	if err != nil || !claimed {
		t.Fatalf("claim = claimed %v, err %v", claimed, err)
	}
	added, err := s.EnrichKey(ctx, "ns", "/a", 0.75, 2)
	if err != nil || added != 1 {
		t.Fatalf("stage output = %d links, %v; want 1", added, err)
	}
	// CRASH: process dies here, before finishPipelineRun (the ack).

	// restart: the clock has moved past the lease; recovery requeues the
	// interrupted run and processes it again.
	clk.Set(time.Date(2026, 7, 6, 1, 0, 0, 0, time.UTC))
	s.recoverPipelineRuns(ctx, testLog())

	r := findRun(t, s, "ns", StageEmbedLink, "/a")
	if r.ID != run.ID || r.Attempts != 2 {
		t.Fatalf("run = id %d attempts %d, want id %d attempts 2 (retry lineage)", r.ID, r.Attempts, run.ID)
	}
	if r.Status != RunSucceeded {
		t.Fatalf("run status = %q, want succeeded after recovery retry", r.Status)
	}
	if r.Items == nil || *r.Items != 0 {
		t.Fatalf("retry items = %v, want 0 (idempotent re-run added nothing)", r.Items)
	}
	links, err := s.Neighbors(ctx, "ns", "/a", "out")
	if err != nil {
		t.Fatal(err)
	}
	similar := 0
	for _, l := range links {
		if l.LinkType == "similar_to" && l.ToKey == "/b" {
			similar++
		}
	}
	if similar != 1 {
		t.Fatalf("similar_to /a->/b edges = %d, want exactly 1 (no duplicate derived links)", similar)
	}
	_ = attempt
}

// TestPipelineEntityCrashBeforeAckRetry is the same red proof for the
// batched entity stage: extraction output (entity facts, mention counts,
// mentions edges) lands, the ack does not; recovery must not
// double-count.
func TestPipelineEntityCrashBeforeAckRetry(t *testing.T) {
	s, _, clk := newTest(t)
	s.SetEntityExtractor(fakeExtractor{names: []string{"Alice Chen", "Acme"}})
	ctx := t.Context()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/a", Body: "Alice Chen joined Acme"}); err != nil {
		t.Fatal(err)
	}

	_, _, claimed, err := s.claimPipelineWork(ctx, "ns", StageEntities, entitiesStageVersion, "/a")
	if err != nil || !claimed {
		t.Fatalf("claim = claimed %v, err %v", claimed, err)
	}
	n, err := s.EnrichEntitiesBatch(ctx, "ns", []string{"/a"})
	if err != nil || n != 2 {
		t.Fatalf("stage output = %d entities, %v; want 2", n, err)
	}
	// CRASH before ack.

	clk.Set(time.Date(2026, 7, 6, 1, 0, 0, 0, time.UTC))
	s.recoverPipelineRuns(ctx, testLog())

	r := findRun(t, s, "ns", StageEntities, "/a")
	if r.Status != RunSucceeded || r.Attempts != 2 {
		t.Fatalf("run = %+v, want succeeded attempts 2", r)
	}
	if r.Items == nil || *r.Items != 0 {
		t.Fatalf("retry items = %v, want 0 (idempotent re-run added nothing)", r.Items)
	}
	ents, err := s.ListEntities(ctx, "ns")
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 2 {
		t.Fatalf("entities = %d, want 2 (no duplicate derived entities)", len(ents))
	}
	for _, e := range ents {
		if mc, _ := e.Attributes["mention_count"].(float64); mc != 1 {
			t.Fatalf("%s mention_count = %v, want 1 (retry must not double-count)", e.Key, mc)
		}
	}
	links, err := s.Neighbors(ctx, "ns", "/a", "out")
	if err != nil {
		t.Fatal(err)
	}
	mentions := 0
	for _, l := range links {
		if l.LinkType == "mentions" {
			mentions++
		}
	}
	if mentions != 2 {
		t.Fatalf("mentions edges = %d, want 2", mentions)
	}
}

// TestRecoverPipelineRunsChunksEntityBatch is the 2c red proof: recovery
// used to hand every pending entity key of a namespace to a single
// flushEntityStage call, so the model saw one unbounded batch instead of
// the entityBatchKeys-sized batches the normal live path always
// produces. 20 stale (crashed, never acknowledged) entity runs in one
// namespace must recover as three batches of 8, 8 and 4.
func TestRecoverPipelineRunsChunksEntityBatch(t *testing.T) {
	s, _, clk := newTest(t)
	fake := &countingStructuredFake{}
	s.SetEntityExtractor(fake)
	ctx := t.Context()

	const n = 20
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("/k%02d", i)
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: key, Body: fmt.Sprintf("fact %d", i)}); err != nil {
			t.Fatal(err)
		}
		if _, _, claimed, err := s.claimPipelineWork(ctx, "ns", StageEntities, entitiesStageVersion, key); err != nil || !claimed {
			t.Fatalf("claim %s: claimed %v err %v", key, claimed, err)
		}
		// CRASH: none of these runs are ever finished, so they stay
		// "running" and become recoverable once their lease expires.
	}

	clk.Set(time.Date(2026, 7, 6, 1, 0, 0, 0, time.UTC)) // past pipelineRunLease
	s.recoverPipelineRuns(ctx, testLog())

	if len(fake.calls) != 3 {
		t.Fatalf("ExtractStructured calls = %d %v, want 3 batches", len(fake.calls), fake.calls)
	}
	if fake.calls[0] != 8 || fake.calls[1] != 8 || fake.calls[2] != 4 {
		t.Fatalf("batch sizes = %v, want [8 8 4]", fake.calls)
	}
}

// TestPipelineRunFailureVisibleSanitized: a failing stage leaves a failed
// run whose error_class is a fixed vocabulary word - the raw error text
// (which may carry credentials from an upstream API) is never stored.
func TestPipelineRunFailureVisibleSanitized(t *testing.T) {
	s, _, _ := newTest(t)
	s.SetEntityExtractor(errExtractor{err: errors.New("anthropic 401: key sk-live-secretabc rejected")})
	ctx := t.Context()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/a", Body: "x"}); err != nil {
		t.Fatal(err)
	}
	s.flushEntityStage(ctx, "ns", []string{"/a"}, testLog())

	r := findRun(t, s, "ns", StageEntities, "/a")
	if r.Status != RunFailed {
		t.Fatalf("status = %q, want failed", r.Status)
	}
	if r.ErrorClass != "model" {
		t.Fatalf("error_class = %q, want model", r.ErrorClass)
	}
	if strings.Contains(r.ErrorClass, "sk-live-secretabc") || strings.Contains(r.ErrorClass, "401") {
		t.Fatalf("error_class %q leaked raw error text", r.ErrorClass)
	}
	if r.FinishedAt == nil || r.Attempts != 1 {
		t.Fatalf("run = %+v, want finished, attempts 1", r)
	}

	// context deadline errors classify as timeout regardless of fallback
	s2, _, _ := newTest(t)
	s2.SetEntityExtractor(errExtractor{err: fmt.Errorf("extract: %w", context.DeadlineExceeded)})
	if _, err := s2.Write(ctx, WriteInput{Namespace: "ns", Key: "/b", Body: "y"}); err != nil {
		t.Fatal(err)
	}
	s2.flushEntityStage(ctx, "ns", []string{"/b"}, testLog())
	r2 := findRun(t, s2, "ns", StageEntities, "/b")
	if r2.ErrorClass != "timeout" {
		t.Fatalf("error_class = %q, want timeout", r2.ErrorClass)
	}
}

// TestPipelineRunRetryBudgetThenParked: recovery retries a failed run
// while attempts remain, then leaves it failed and visible.
func TestPipelineRunRetryBudgetThenParked(t *testing.T) {
	s, _, _ := newTest(t)
	s.SetEntityExtractor(errExtractor{err: errors.New("model down")})
	ctx := t.Context()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/a", Body: "x"}); err != nil {
		t.Fatal(err)
	}
	s.flushEntityStage(ctx, "ns", []string{"/a"}, testLog()) // attempts=1 failed
	for want := 2; want <= pipelineMaxAttempts; want++ {
		s.recoverPipelineRuns(ctx, testLog())
		r := findRun(t, s, "ns", StageEntities, "/a")
		if r.Status != RunFailed || r.Attempts != want {
			t.Fatalf("after recovery: run = status %q attempts %d, want failed attempts %d", r.Status, r.Attempts, want)
		}
	}
	// budget exhausted: recovery must not requeue again
	s.recoverPipelineRuns(ctx, testLog())
	r := findRun(t, s, "ns", StageEntities, "/a")
	if r.Attempts != pipelineMaxAttempts || r.Status != RunFailed {
		t.Fatalf("parked run = status %q attempts %d, want failed attempts %d (visible, not retried forever)",
			r.Status, r.Attempts, pipelineMaxAttempts)
	}
}

// TestPipelineRunNewRevisionGetsNewRun: editing the source fact starts a
// new run keyed by the new revision; a pending run for a stale revision
// is superseded instead of executing against newer state.
func TestPipelineRunNewRevisionGetsNewRun(t *testing.T) {
	s, _, clk := newTest(t)
	s.SetEmbedder(&fakeEmbedder{m: map[string][]float32{
		"ceph osd flapping on node 3":  {1, 0, 0},
		"osd flap traced to nic reset": {0.98, 0.15, 0},
		"ceph osd flapping on node 4":  {0.97, 0.1, 0},
		"ceph osd flapping on node 5":  {0.96, 0.2, 0},
	}})
	ctx := t.Context()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/a", Body: "ceph osd flapping on node 3"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/b", Body: "osd flap traced to nic reset"}); err != nil {
		t.Fatal(err)
	}

	run1, att1, claimed, err := s.claimPipelineWork(ctx, "ns", StageEmbedLink, embedLinkStageVersion, "/a")
	if err != nil || !claimed {
		t.Fatalf("claim rev1 = %v, %v", claimed, err)
	}
	if _, err := s.EnrichKey(ctx, "ns", "/a", 0.75, 2); err != nil {
		t.Fatal(err)
	}
	items := int64(1)
	if ok, err := s.finishPipelineRun(ctx, run1.ID, att1, RunSucceeded, &items, ""); err != nil || !ok {
		t.Fatalf("finish rev1 = %v, %v", ok, err)
	}

	// a changed source revision is a NEW run row, never an overwrite
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/a", Body: "ceph osd flapping on node 4"}); err != nil {
		t.Fatal(err)
	}
	run2, _, claimed, err := s.claimPipelineWork(ctx, "ns", StageEmbedLink, embedLinkStageVersion, "/a")
	if err != nil || !claimed {
		t.Fatalf("claim rev2 = %v, %v", claimed, err)
	}
	if run2.SourceRevision == run1.SourceRevision || run2.ID == run1.ID {
		t.Fatalf("rev2 run = %+v, want a new row with a new source_revision (rev1 %q)", run2, run1.SourceRevision)
	}
	if ok, err := s.finishPipelineRun(ctx, run2.ID, 1, RunSucceeded, &items, ""); err != nil || !ok {
		t.Fatalf("finish rev2 = %v, %v", ok, err)
	}
	runs, err := s.ListPipelineRuns(ctx, "ns", StageEmbedLink, "", 0)
	if err != nil || len(runs) != 2 {
		t.Fatalf("runs = %v, %v; want 2 rows (one per revision)", runs, err)
	}

	// stale pending revision is superseded, never executed against newer state
	write := func(body string) {
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/m", Body: body}); err != nil {
			t.Fatal(err)
		}
	}
	write("ceph osd flapping on node 3") // revM1
	runM1, attM1, _, err := s.claimPipelineWork(ctx, "ns", StageEmbedLink, embedLinkStageVersion, "/m")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.finishPipelineRun(ctx, runM1.ID, attM1, RunSucceeded, &items, ""); err != nil || !ok {
		t.Fatalf("finish revM1 = %v, %v", ok, err)
	}
	write("ceph osd flapping on node 4") // revM2
	runM2, _, claimed, err := s.claimPipelineWork(ctx, "ns", StageEmbedLink, embedLinkStageVersion, "/m")
	if err != nil || !claimed {
		t.Fatalf("claim revM2 = %v, %v", claimed, err)
	}
	// worker dies after claim: revM2 run left running; lease expires
	clk.Set(time.Date(2026, 7, 6, 1, 0, 0, 0, time.UTC))
	requeued, err := s.requeueStalePipelineRuns(ctx, s.now().Add(-pipelineRunLease))
	if err != nil || len(requeued) != 1 {
		t.Fatalf("requeued = %v, %v; want 1 run", requeued, err)
	}
	write("ceph osd flapping on node 5") // revM3 lands while revM2's run is pending
	runM3, _, claimed, err := s.claimPipelineWork(ctx, "ns", StageEmbedLink, embedLinkStageVersion, "/m")
	if err != nil || !claimed {
		t.Fatalf("claim revM3 = %v, %v", claimed, err)
	}
	runs, err = s.ListPipelineRuns(ctx, "ns", StageEmbedLink, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	byRev := map[string]PipelineRun{}
	for _, r := range runs {
		if r.SourceKey == "/m" {
			byRev[r.SourceRevision] = r
		}
	}
	if got := byRev[runM2.SourceRevision].Status; got != RunSuperseded {
		t.Fatalf("stale revM2 run status = %q, want superseded", got)
	}
	if runM3.SourceRevision == runM2.SourceRevision {
		t.Fatal("revM3 run reused the stale revision")
	}
	if got := byRev[runM3.SourceRevision].Status; got != RunRunning {
		t.Fatalf("revM3 run status = %q, want running (claimed)", got)
	}
	// status filter: only the running one matches
	running, err := s.ListPipelineRuns(ctx, "ns", StageEmbedLink, RunRunning, 0)
	if err != nil || len(running) != 1 || running[0].ID != runM3.ID {
		t.Fatalf("status filter = %v, %v; want only revM3 running", running, err)
	}
}

// TestPipelineRunStaleWorkerCannotClobber: a worker whose lease expired
// and whose run was reclaimed must not overwrite the new owner's
// terminal state when it finishes late.
func TestPipelineRunStaleWorkerCannotClobber(t *testing.T) {
	s, _, clk := newTest(t)
	s.SetEmbedder(&fakeEmbedder{m: map[string][]float32{"x": {1, 0, 0}}})
	ctx := t.Context()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/a", Body: "x"}); err != nil {
		t.Fatal(err)
	}
	run, att1, claimed, err := s.claimPipelineWork(ctx, "ns", StageEmbedLink, embedLinkStageVersion, "/a")
	if err != nil || !claimed {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	// worker 1 stalls past the lease; recovery requeues and worker 2 claims
	clk.Set(time.Date(2026, 7, 6, 1, 0, 0, 0, time.UTC))
	if _, err := s.requeueStalePipelineRuns(ctx, s.now().Add(-pipelineRunLease)); err != nil {
		t.Fatal(err)
	}
	run2, att2, claimed, err := s.claimPipelineWork(ctx, "ns", StageEmbedLink, embedLinkStageVersion, "/a")
	if err != nil || !claimed {
		t.Fatalf("worker 2 claim = %v, %v", claimed, err)
	}
	if run2.ID != run.ID || att2 != 2 {
		t.Fatalf("worker 2 claim = id %d attempt %d, want id %d attempt 2", run2.ID, att2, run.ID)
	}
	items := int64(0)
	if ok, err := s.finishPipelineRun(ctx, run2.ID, att2, RunSucceeded, &items, ""); err != nil || !ok {
		t.Fatalf("worker 2 finish = %v, %v", ok, err)
	}
	// worker 1 wakes up and reports a failure for its dead claim
	if ok, err := s.finishPipelineRun(ctx, run.ID, att1, RunFailed, nil, "model"); err != nil || ok {
		t.Fatalf("stale worker finish = applied %v, err %v; want not applied", ok, err)
	}
	r := findRun(t, s, "ns", StageEmbedLink, "/a")
	if r.Status != RunSucceeded || r.ErrorClass != "" || r.Attempts != 2 {
		t.Fatalf("run = %+v, want succeeded attempts 2, no error class (stale write rejected)", r)
	}
}

// TestPipelineClaimSkipsGoneKey: no live revision means no run row -
// there is no revision to key work on.
func TestPipelineClaimSkipsGoneKey(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := t.Context()
	if _, _, claimed, err := s.claimPipelineWork(ctx, "ns", StageEmbedLink, embedLinkStageVersion, "/nope"); err != nil || claimed {
		t.Fatalf("claim on unknown key = %v, %v; want not claimed, nil", claimed, err)
	}
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/t", Body: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Forget(ctx, "ns", "/t", "tester"); err != nil {
		t.Fatal(err)
	}
	if _, _, claimed, err := s.claimPipelineWork(ctx, "ns", StageEmbedLink, embedLinkStageVersion, "/t"); err != nil || claimed {
		t.Fatalf("claim on tombstoned key = %v, %v; want not claimed, nil", claimed, err)
	}
	runs, err := s.ListPipelineRuns(ctx, "ns", "", "", 0)
	if err != nil || len(runs) != 0 {
		t.Fatalf("runs = %v, %v; want none", runs, err)
	}
}
