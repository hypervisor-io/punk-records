package memory

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/bus"
)

// failingEmbedder fails every embed call; its message carries a fake
// credential to prove the run row stores only the sanitized class.
type failingEmbedder struct{}

func (failingEmbedder) Dims() int { return 3 }
func (failingEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, errors.New("upstream 401: key sk-live-secretabc rejected")
}

// fenceBlockingExtractor blocks inside Extract on the old-revision body
// so the test can run a newer revision's complete stage between the
// stale worker's read and its apply; any other body extracts normally.
type fenceBlockingExtractor struct {
	started, release chan struct{}
}

func (e *fenceBlockingExtractor) Extract(ctx context.Context, body string) ([]string, error) {
	if body == "old-revision" {
		close(e.started)
		select {
		case <-e.release:
			return []string{"OldWarehouse"}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return []string{"NewGateway"}, nil
}

// fenceBlockingEmbedder blocks inside Embed on one exact text so the
// test can interleave a newer revision's complete stage run between the
// stale worker's read and its derived writes; other texts (write-time
// embedding uses the "key: <key>\n<body>" form) resolve immediately.
type fenceBlockingEmbedder struct {
	blockOn string
	vec     map[string][]float32
	started chan struct{}
	release chan struct{}
}

func (e *fenceBlockingEmbedder) Dims() int { return 3 }

func (e *fenceBlockingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 1 && texts[0] == e.blockOn {
		close(e.started)
		select {
		case <-e.release:
			return [][]float32{e.vec[e.blockOn]}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		key := t
		if strings.HasPrefix(t, "key: ") {
			key = t[strings.IndexByte(t, '\n')+1:]
		}
		out[i] = e.vec[key]
	}
	return out, nil
}

// TestEmbeddingFailureFailsRunForRetry pins the truthful-outcome rule
// for the embed-link stage: a swallowed embedder failure used to record
// the run as succeeded, and a terminal success blocks recovery from
// ever retrying. The failure must land as a failed run with the
// sanitized model error class, and the retry budget must pick it up
// once the embedder recovers.
func TestEmbeddingFailureFailsRunForRetry(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := t.Context()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f", Body: "needs embedding"}); err != nil {
		t.Fatal(err)
	}
	s.SetEmbedder(failingEmbedder{})
	s.runEmbedLinkStage(ctx, "ns", "/f", testLog())

	r := findRun(t, s, "ns", StageEmbedLink, "/f")
	if r.Status == RunSucceeded {
		t.Fatal("embedding failure recorded as succeeded; a terminal success blocks recovery retry")
	}
	if r.Status != RunFailed || r.ErrorClass != "model" {
		t.Fatalf("run = status %q class %q, want failed with the model error class", r.Status, r.ErrorClass)
	}
	if strings.Contains(r.ErrorClass, "sk-live-secretabc") || strings.Contains(r.ErrorClass, "401") {
		t.Fatalf("error_class %q leaked raw error text", r.ErrorClass)
	}

	// the embedder recovers: the retry budget picks the failed run up
	s.SetEmbedder(&fakeEmbedder{m: map[string][]float32{"needs embedding": {1, 0, 0}}})
	s.recoverPipelineRuns(ctx, testLog())
	r = findRun(t, s, "ns", StageEmbedLink, "/f")
	if r.Status != RunSucceeded || r.Attempts != 2 {
		t.Fatalf("run after retry = status %q attempts %d, want succeeded attempts 2", r.Status, r.Attempts)
	}
}

// TestDrainedOutboxEventPersistsDurableWorkBeforeAck pins the durable
// delivery boundary: DrainOutbox publishes to an in-memory bus and
// marks the row delivered, and a crash before any consumer claims
// leaves nothing behind unless the stage work was persisted BEFORE the
// acknowledgment. The persisted pending run - not the ephemeral bus
// event - is what restart recovery drains.
func TestDrainedOutboxEventPersistsDurableWorkBeforeAck(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := t.Context()
	s.SetEntityExtractor(fakeExtractor{names: []string{"Mercury"}})
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f", Body: "Mercury"}); err != nil {
		t.Fatal(err)
	}
	b := bus.New()
	_, cancel := b.Subscribe()
	if _, err := s.DrainOutbox(ctx, 100, func(e OutboxEvent) error {
		b.Publish(bus.Event{Kind: e.Kind, Key: e.Key, Data: e.Payload})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cancel() // crash after the durable delivery acknowledgment, before the in-memory event is consumed

	runs, err := s.ListPipelineRuns(ctx, "ns", StageEntities, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Status != RunPending {
		t.Fatalf("runs after acknowledged delivery = %v, want exactly one pending entities run (durable work persisted before ack)", runs)
	}

	s.recoverPipelineRuns(ctx, testLog())
	r := findRun(t, s, "ns", StageEntities, "/f")
	if r.Status != RunSucceeded {
		t.Fatalf("recovered run status = %q, want succeeded (durable pending work drained)", r.Status)
	}
}

// TestTombstoneRetiresPendingPipelineWork pins the other half of the
// durable pending lifecycle: when a key's tombstone drains before any
// claim, its pending work is retired instead of sitting pending where
// every recovery pass would retry it forever.
func TestTombstoneRetiresPendingPipelineWork(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := t.Context()
	s.SetEntityExtractor(fakeExtractor{names: []string{"Mercury"}})
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f", Body: "Mercury"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DrainOutbox(ctx, 100, func(OutboxEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if r := findRun(t, s, "ns", StageEntities, "/f"); r.Status != RunPending {
		t.Fatalf("run after drain = %q, want pending", r.Status)
	}
	if err := s.Forget(ctx, "ns", "/f", "tester"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DrainOutbox(ctx, 100, func(OutboxEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	r := findRun(t, s, "ns", StageEntities, "/f")
	if r.Status != RunSuperseded {
		t.Fatalf("run after tombstone drain = %q, want superseded (a dead key has nothing to derive)", r.Status)
	}
	s.recoverPipelineRuns(ctx, testLog())
	if r := findRun(t, s, "ns", StageEntities, "/f"); r.Status != RunSuperseded {
		t.Fatalf("run after recovery = %q, want superseded", r.Status)
	}
}

// TestRequeuedPendingWorkDrainedByRecovery pins recovery against a
// second crash: a stale run requeued to pending whose processing then
// crashed must be drained by the next recovery pass. Recovery that
// only looked at running and failed rows stranded requeued work
// forever.
func TestRequeuedPendingWorkDrainedByRecovery(t *testing.T) {
	s, _, clk := newTest(t)
	ctx := t.Context()
	s.SetEntityExtractor(fakeExtractor{names: []string{"Mercury"}})
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f", Body: "Mercury"}); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := s.claimPipelineWork(ctx, "ns", StageEntities, entitiesStageVersion, "/f"); err != nil || !ok {
		t.Fatalf("claim = claimed %v, err %v", ok, err)
	}
	clk.Set(s.now().Add(10 * time.Minute))
	requeued, err := s.requeueStalePipelineRuns(ctx, s.now().Add(-pipelineRunLease))
	if err != nil || len(requeued) != 1 {
		t.Fatalf("requeued = %v, %v; want 1 run", requeued, err)
	}
	// crash after the requeue persisted pending but before processing it

	s.recoverPipelineRuns(ctx, testLog())
	r := findRun(t, s, "ns", StageEntities, "/f")
	if r.Status != RunSucceeded || r.Attempts != 2 {
		t.Fatalf("run = status %q attempts %d, want succeeded attempts 2 (pending work drained)", r.Status, r.Attempts)
	}
}

// TestStaleEntityWorkerFencedAtApplyBoundary is the entity-path
// interleaving proof for the apply-boundary fence: an old-revision
// extraction still in flight while a newer revision's run writes and
// finishes must not emit the old revision's derived state afterwards.
// finishPipelineRun's attempt guard protects only the status row; the
// revision fence protects the mentions edges and entity facts
// themselves, and the stale run finishes superseded.
func TestStaleEntityWorkerFencedAtApplyBoundary(t *testing.T) {
	s, _, clk := newTest(t)
	ctx := t.Context()
	ex := &fenceBlockingExtractor{started: make(chan struct{}), release: make(chan struct{})}
	s.SetEntityExtractor(ex)
	released := false
	defer func() {
		if !released {
			close(ex.release)
		}
	}()
	f1, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f", Body: "old-revision"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.flushEntityStage(ctx, "ns", []string{"/f"}, testLog())
	}()
	select {
	case <-ex.started:
	case <-time.After(5 * time.Second):
		t.Fatal("old-revision extraction did not start")
	}
	clk.Set(s.now().Add(time.Second))
	f2, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f", Body: "new-revision"})
	if err != nil {
		t.Fatal(err)
	}
	s.flushEntityStage(ctx, "ns", []string{"/f"}, testLog())
	close(ex.release)
	released = true
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("old-revision extraction did not finish")
	}

	links, err := s.Neighbors(ctx, "ns", "/f", "out")
	if err != nil {
		t.Fatal(err)
	}
	var sawOld, sawNew bool
	for _, l := range links {
		if l.LinkType != "mentions" {
			continue
		}
		switch l.ToKey {
		case "/entities/oldwarehouse":
			sawOld = true
		case "/entities/newgateway":
			sawNew = true
		}
	}
	if sawOld {
		t.Fatal("stale worker wrote old-revision derived state after the newer revision's run finished")
	}
	if !sawNew {
		t.Fatal("newer revision's run did not write its mentions edge")
	}
	ents, err := s.ListEntities(ctx, "ns")
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Key != "/entities/newgateway" {
		t.Fatalf("entities = %v, want only /entities/newgateway (the fenced extraction must not create entities)", ents)
	}
	runs, err := s.ListPipelineRuns(ctx, "ns", StageEntities, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	byRev := map[string]PipelineRun{}
	for _, r := range runs {
		if r.SourceKey == "/f" {
			byRev[r.SourceRevision] = r
		}
	}
	if got := byRev[f1.ID].Status; got != RunSuperseded {
		t.Fatalf("stale run status = %q, want superseded", got)
	}
	if got := byRev[f2.ID].Status; got != RunSucceeded {
		t.Fatalf("newer run status = %q, want succeeded", got)
	}
}

// TestStaleEmbedWorkerFencedAtWriteBoundary is the embed-link-path
// interleaving proof: an old-revision backfill still inside the
// embedder call while a newer revision's run links and finishes must
// not write similar_to links derived from the superseded revision's
// vector afterwards, and its run finishes superseded.
func TestStaleEmbedWorkerFencedAtWriteBoundary(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := t.Context()
	// the first revision is written without an embedder, so the stage
	// must backfill - that embedder call is where the stale worker blocks
	f1, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f", Body: "old-body"})
	if err != nil {
		t.Fatal(err)
	}
	emb := &fenceBlockingEmbedder{
		blockOn: "old-body",
		vec: map[string][]float32{
			"old-body": {1, 0, 0},
			"new-body": {0, 1, 0},
			"x-body":   {1, 0, 0},
			"y-body":   {0, 1, 0},
		},
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	s.SetEmbedder(emb)
	released := false
	defer func() {
		if !released {
			close(emb.release)
		}
	}()
	// a neighbor matching the OLD body's vector, embedded at write time
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/x", Body: "x-body"}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runEmbedLinkStage(ctx, "ns", "/f", testLog())
	}()
	select {
	case <-emb.started:
	case <-time.After(5 * time.Second):
		t.Fatal("old-revision embedding did not start")
	}
	f2, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f", Body: "new-body"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/y", Body: "y-body"}); err != nil {
		t.Fatal(err)
	}
	s.runEmbedLinkStage(ctx, "ns", "/f", testLog())
	close(emb.release)
	released = true
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("old-revision embedding did not finish")
	}

	links, err := s.Neighbors(ctx, "ns", "/f", "out")
	if err != nil {
		t.Fatal(err)
	}
	var sawX, sawY bool
	for _, l := range links {
		if l.LinkType != "similar_to" {
			continue
		}
		switch l.ToKey {
		case "/x":
			sawX = true
		case "/y":
			sawY = true
		}
	}
	if sawX {
		t.Fatal("stale worker wrote an old-revision similar_to link after the newer revision's run finished")
	}
	if !sawY {
		t.Fatal("newer revision's run did not link /f -> /y")
	}
	runs, err := s.ListPipelineRuns(ctx, "ns", StageEmbedLink, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	byRev := map[string]PipelineRun{}
	for _, r := range runs {
		if r.SourceKey == "/f" {
			byRev[r.SourceRevision] = r
		}
	}
	if got := byRev[f1.ID].Status; got != RunSuperseded {
		t.Fatalf("stale run status = %q, want superseded", got)
	}
	if got := byRev[f2.ID].Status; got != RunSucceeded {
		t.Fatalf("newer run status = %q, want succeeded", got)
	}
}

// TestFencedStageSerializesConcurrentSourceRevisionPostgres is the
// PostgreSQL interleaving proof for the source fence's commit-time
// guarantee: an old revision's fenced stage transaction pauses after
// its claim check and head-row lock, then a newer source Write and ITS
// run try to complete. Two outcomes are correct - the new work finishes
// first and the old transaction's fence trips (no stale derived fact
// commits), or the new work serializes behind the old transaction's
// head-row lock and runs after it. The one outcome that must never
// happen is the old transaction committing derived state over a newer
// revision's finished run. SQLite cannot express the interleaving (its
// single-connection store serializes writers in-process), so this proof
// requires PUNK_TEST_PG_DSN.
func TestFencedStageSerializesConcurrentSourceRevisionPostgres(t *testing.T) {
	if os.Getenv("PUNK_TEST_PG_DSN") == "" {
		t.Skip("requires isolated postgres")
	}
	s, _, _ := newTest(t)
	ctx := t.Context()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f", Body: "old source"}); err != nil {
		t.Fatal(err)
	}
	old, attempt, claimed, err := s.claimPipelineWork(ctx, "ns", StageEntities, entitiesStageVersion, "/f")
	if err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	oldPlan, err := s.writePrepare(ctx, WriteInput{Namespace: "ns", Key: "/entities/stale-after-new", Body: "stale entity"})
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	type outcome struct {
		applied bool
		err     error
	}
	oldDone := make(chan outcome, 1)
	go func() {
		applied, err := s.fencedStageTx(ctx, "ns", "/f", runFence{old.ID, attempt, old.SourceRevision}, func(tx *sql.Tx) error {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			_, err := s.writeTx(ctx, tx, oldPlan, false)
			return err
		})
		oldDone <- outcome{applied, err}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("old transaction did not enter")
	}
	newDone := make(chan error, 1)
	go func() {
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f", Body: "new source"}); err != nil {
			newDone <- err
			return
		}
		fresh, n, ok, err := s.claimPipelineWork(ctx, "ns", StageEntities, entitiesStageVersion, "/f")
		if err != nil {
			newDone <- err
			return
		}
		if !ok {
			newDone <- ErrNotFound
			return
		}
		plan, err := s.writePrepare(ctx, WriteInput{Namespace: "ns", Key: "/entities/fresh", Body: "fresh entity"})
		if err != nil {
			newDone <- err
			return
		}
		if _, err := s.fencedStageTx(ctx, "ns", "/f", runFence{fresh.ID, n, fresh.SourceRevision}, func(tx *sql.Tx) error {
			_, err := s.writeTx(ctx, tx, plan, false)
			return err
		}); err != nil {
			newDone <- err
			return
		}
		_, err = s.finishPipelineRun(ctx, fresh.ID, n, RunSucceeded, nil, "")
		newDone <- err
	}()
	select {
	case err := <-newDone:
		if err != nil {
			close(release)
			<-oldDone
			t.Fatal(err)
		}
		close(release)
		result := <-oldDone
		if result.err != nil {
			t.Fatal(result.err)
		}
		facts, err := s.liveByKeys(ctx, "ns", []string{"/entities/stale-after-new"})
		if err != nil {
			t.Fatal(err)
		}
		if result.applied || len(facts) > 0 {
			t.Fatal("old fenced transaction committed stale derived state after a newer source revision's run succeeded")
		}
	case <-time.After(time.Second):
		// the new work serialized behind the old transaction's head-row
		// lock: it must resume once the old transaction finishes.
		close(release)
		result := <-oldDone
		if result.err != nil {
			t.Fatal(result.err)
		}
		select {
		case err := <-newDone:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("new revision failed to resume after old transaction finished")
		}
	}
}

// staleAttemptExtractor blocks inside its first Extract call until
// released, then answers "StaleAttempt"; every later call answers
// "FreshAttempt". It reproduces a worker whose lease expires mid-extraction:
// the run is reclaimed and completed by a replacement attempt on the SAME
// source revision before the stale attempt's extraction returns.
type staleAttemptExtractor struct {
	calls            atomic.Int32
	started, release chan struct{}
}

func (e *staleAttemptExtractor) Extract(ctx context.Context, _ string) ([]string, error) {
	if e.calls.Add(1) == 1 {
		close(e.started)
		select {
		case <-e.release:
			return []string{"StaleAttempt"}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return []string{"FreshAttempt"}, nil
}

// TestExpiredEntityAttemptFencedAtWriteBoundary is the same-revision
// reclaim proof for the entity stage's transactional fence: attempt 1 is
// still inside the extractor when its lease expires, the requeued run is
// claimed and completed by attempt 2, and only then does attempt 1's
// extraction return. revisionStillLive alone cannot fence this (the
// source revision never changed) and finishPipelineRun's attempt guard
// protects only the status row - attempt 1's derived writes must be
// fenced off transactionally, by the active-claim check at the write
// boundary.
func TestExpiredEntityAttemptFencedAtWriteBoundary(t *testing.T) {
	s, _, clk := newTest(t)
	ctx := t.Context()
	ex := &staleAttemptExtractor{started: make(chan struct{}), release: make(chan struct{})}
	s.SetEntityExtractor(ex)
	released := false
	defer func() {
		if !released {
			close(ex.release)
		}
	}()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f", Body: "same revision"}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.flushEntityStage(ctx, "ns", []string{"/f"}, testLog())
	}()
	select {
	case <-ex.started:
	case <-time.After(5 * time.Second):
		t.Fatal("attempt 1 extraction did not start")
	}
	// attempt 1's lease expires; recovery requeues and attempt 2 runs the
	// same source revision to completion.
	clk.Set(s.now().Add(10 * time.Minute))
	if _, err := s.requeueStalePipelineRuns(ctx, s.now().Add(-pipelineRunLease)); err != nil {
		t.Fatal(err)
	}
	s.flushEntityStage(ctx, "ns", []string{"/f"}, testLog())
	close(ex.release)
	released = true
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("attempt 1 did not finish")
	}

	links, err := s.Neighbors(ctx, "ns", "/f", "out")
	if err != nil {
		t.Fatal(err)
	}
	var sawStale, sawFresh bool
	for _, l := range links {
		if l.LinkType != "mentions" {
			continue
		}
		switch l.ToKey {
		case "/entities/staleattempt":
			sawStale = true
		case "/entities/freshattempt":
			sawFresh = true
		}
	}
	if sawStale {
		t.Fatal("expired attempt wrote derived state after the replacement attempt succeeded on the same source revision")
	}
	if !sawFresh {
		t.Fatal("replacement attempt's mentions edge missing")
	}
	ents, err := s.ListEntities(ctx, "ns")
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Key != "/entities/freshattempt" {
		t.Fatalf("entities = %v, want only /entities/freshattempt (the expired attempt's writes must be fenced off)", ents)
	}
	r := findRun(t, s, "ns", StageEntities, "/f")
	if r.Status != RunSucceeded || r.Attempts != 2 {
		t.Fatalf("run = status %q attempts %d, want succeeded attempts 2 (replacement owns the terminal state)", r.Status, r.Attempts)
	}
}

// TestOldTombstoneCannotRetireRecreatedWork is the durable-work proof for
// the tombstone drain fence: an add, a forget, and a re-add drain in one
// batch. The old add persists pending work at the CURRENT live revision
// (the recreated fact); the old tombstone must retire only work whose
// revision is no longer live, because the re-add's own drained add cannot
// reactivate a superseded row (ON CONFLICT DO NOTHING). Without the fence
// the recreated fact's durable work is permanently superseded while the
// fact itself lives.
func TestOldTombstoneCannotRetireRecreatedWork(t *testing.T) {
	s, _, clk := newTest(t)
	ctx := t.Context()
	s.SetEntityExtractor(fakeExtractor{names: []string{"Mercury"}})
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f", Body: "old Mercury"}); err != nil {
		t.Fatal(err)
	}
	clk.Set(s.now().Add(time.Second))
	if err := s.Forget(ctx, "ns", "/f", "test"); err != nil {
		t.Fatal(err)
	}
	clk.Set(s.now().Add(time.Second))
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f", Body: "new Mercury"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DrainOutbox(ctx, 100, func(OutboxEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	// crash after delivery, before any ephemeral consumer runs: recovery
	// must find the recreated fact's durable work and run it.
	s.recoverPipelineRuns(ctx, testLog())
	r := findRun(t, s, "ns", StageEntities, "/f")
	if r.Status != RunSucceeded {
		t.Fatalf("recreated fact's run = %q, want succeeded (the old tombstone must not retire live-revision work)", r.Status)
	}
	links, err := s.Neighbors(ctx, "ns", "/f", "out")
	if err != nil {
		t.Fatal(err)
	}
	sawMention := false
	for _, l := range links {
		if l.LinkType == "mentions" && l.ToKey == "/entities/mercury" {
			sawMention = true
		}
	}
	if !sawMention {
		t.Fatal("recreated fact never enriched: its durable work was retired by the old tombstone")
	}
}

// pipelineTypedExtractor is a StructuredEntityExtractor: typed mode must
// survive the pipeline's staged flush, producing canonical
// /entities/<type>/<slug> keys with revision-precise provenance.
type pipelineTypedExtractor struct{}

func (pipelineTypedExtractor) Extract(context.Context, string) ([]string, error) {
	return []string{"Mercury"}, nil
}

func (pipelineTypedExtractor) ExtractStructured(_ context.Context, src []EntitySource) ([]ExtractedEntity, error) {
	return []ExtractedEntity{{Name: "Mercury", Type: "service", SourceFacts: []string{src[0].ID}}}, nil
}

// TestPipelineEntityStagePreservesTypedExtraction pins the G01 dispatch
// inside the pipeline's entity stage: flushEntityStage must route a
// structured extractor through the typed apply path, not the legacy
// Extract/applyEntities path (which would emit untyped /entities/<slug>
// keys and drop the type and revision provenance).
func TestPipelineEntityStagePreservesTypedExtraction(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := t.Context()
	s.SetEntityExtractor(pipelineTypedExtractor{})
	f, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/f", Body: "Mercury service"})
	if err != nil {
		t.Fatal(err)
	}
	s.flushEntityStage(ctx, "ns", []string{"/f"}, testLog())

	links, err := s.Neighbors(ctx, "ns", "/f", "out")
	if err != nil {
		t.Fatal(err)
	}
	sawTyped := false
	for _, l := range links {
		if l.LinkType == "mentions" && l.ToKey == "/entities/service/mercury" {
			sawTyped = true
		}
		if l.LinkType == "mentions" && l.ToKey == "/entities/mercury" {
			t.Fatal("pipeline used the legacy untyped apply path for a structured extractor")
		}
	}
	if !sawTyped {
		t.Fatalf("typed mentions edge missing: links=%v", links)
	}
	ents, err := s.liveByKeys(ctx, "ns", []string{"/entities/service/mercury"})
	if err != nil || len(ents) != 1 {
		t.Fatalf("typed entity = %v, %v; want exactly 1", ents, err)
	}
	if got := ents[0].Attributes["entity_type"]; got != "service" {
		t.Fatalf("entity_type = %v, want service", got)
	}
	if got := attrStringList(ents[0], "source_facts"); len(got) != 1 || got[0] != f.ID {
		t.Fatalf("source_facts = %v, want [%s] (revision-precise provenance)", got, f.ID)
	}
	r := findRun(t, s, "ns", StageEntities, "/f")
	if r.Status != RunSucceeded {
		t.Fatalf("run status = %q, want succeeded", r.Status)
	}
}
