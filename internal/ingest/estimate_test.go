package ingest

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hypervisor-io/punk-records/internal/cost"
	"github.com/hypervisor-io/punk-records/internal/llm"
	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/store"
)

// The P02 dry-run proofs. Every estimate here is taken against a store
// whose model dependencies count calls and refuse them while a preview
// runs, so "no model calls" is an assertion, not a comment.

// previewEmbedder is a counting Embedder that also reports its model ID
// (memory.ModelNamer), so an estimate can name the configured model. It
// records the exact texts it was called with, because a forecast is only
// honest if it sizes the string the model really receives: the write path
// sends embedText(key, body) while the deferred embed_link stage sends the
// stored body alone. While forbid is set, any call fails the test.
type previewEmbedder struct {
	t      *testing.T
	model  string
	forbid bool
	calls  int
	texts  int
	seen   []string
}

func (f *previewEmbedder) Dims() int     { return 3 }
func (f *previewEmbedder) Model() string { return f.model }

func (f *previewEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.calls++
	f.texts += len(texts)
	f.seen = append(f.seen, texts...)
	if f.forbid {
		f.t.Errorf("dry-run called the embedder with %d texts: a preview must make no model calls", len(texts))
	}
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{0.1, 0.2, 0.3}
	}
	return out, nil
}

// previewExtractor is a counting batched extractor that also reports its
// model ID (it implements StructuredEntityExtractor, so the entity stage
// batches like the wired one does). While forbid is set, any call fails
// the test.
type previewExtractor struct {
	t      *testing.T
	model  string
	names  []string
	forbid bool
	calls  int
	batch  []int
}

func (f *previewExtractor) Model() string { return f.model }

func (f *previewExtractor) Extract(_ context.Context, _ string) ([]string, error) {
	f.calls++
	if f.forbid {
		f.t.Error("dry-run called the entity extractor: a preview must make no model calls")
	}
	return f.names, nil
}

func (f *previewExtractor) ExtractStructured(_ context.Context, sources []memory.EntitySource) ([]memory.ExtractedEntity, error) {
	f.calls++
	f.batch = append(f.batch, len(sources))
	if f.forbid {
		f.t.Error("dry-run called the entity extractor: a preview must make no model calls")
	}
	var out []memory.ExtractedEntity
	for _, n := range f.names {
		for _, src := range sources {
			out = append(out, memory.ExtractedEntity{
				Name: n, Type: memory.EntityTypeService, SourceFacts: []string{src.ID}})
		}
	}
	return out, nil
}

// previewStore wires a temp store with both counting dependencies.
func previewStore(t *testing.T) (*memory.Store, *previewEmbedder, *previewExtractor) {
	t.Helper()
	s, _, emb, ext := previewStoreDB(t)
	return s, emb, ext
}

// previewStoreDB is previewStore plus the database handle, for the tests
// that corrupt a stored row the way a partial write or a bad migration
// would and then prove the preview left it exactly where it was.
func previewStoreDB(t *testing.T) (*memory.Store, *store.DB, *previewEmbedder, *previewExtractor) {
	t.Helper()
	return writerStoreDB(t, true, true)
}

// writerStoreDB builds a store over its own database with exactly the
// wiring asked for. The distinction matters: the ordinary CLI writer opens
// memory with no embedder at all, so it stores chunks unembedded and the
// configured server's embed_link stage embeds them later. A preview must
// attribute embedding work by this wiring, not by the configured stages.
func writerStoreDB(t *testing.T, wireEmbedder, wireExtractor bool) (*memory.Store, *store.DB, *previewEmbedder, *previewExtractor) {
	t.Helper()
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "mem.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.MigrateUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := memory.New(db, nil)
	emb := &previewEmbedder{t: t, model: "nomic-embed-text"}
	ext := &previewExtractor{t: t, model: "gpt-5-mini", names: []string{"Acme"}}
	if wireEmbedder {
		s.SetEmbedder(emb)
	}
	if wireExtractor {
		s.SetEntityExtractor(ext)
	}
	return s, db, emb, ext
}

// embeddedChunkCount counts the chunks actually stored with a vector: the
// writer's own embedding work, not what a deferred stage would do later.
func embeddedChunkCount(t *testing.T, db *store.DB, prefix string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM memories WHERE key LIKE '`+prefix+`/chunk-%' AND embedding IS NOT NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// forbidModelCalls fails the test on any model call until the returned
// release func runs.
func forbidModelCalls(emb *previewEmbedder, ext *previewExtractor) func() {
	emb.forbid, ext.forbid = true, true
	return func() { emb.forbid, ext.forbid = false, false }
}

// inputTotals sums the exact bytes and the estimated tokens of the texts a
// model really received, so a forecast is checked against the string the
// model was actually called with instead of a bare call count.
func inputTotals(texts []string) (bytes, tokens int) {
	for _, s := range texts {
		bytes += len(s)
		tokens += memory.EstimateTokens(s)
	}
	return bytes, tokens
}

// chunkBodyCounts maps each stored chunk body under prefix to the number of
// live chunks carrying it: that body is the exact text the deferred
// embed_link stage sends, because memory.enrichKey backfills from
// facts[self].Body alone and never from the keyed write-path text.
func chunkBodyCounts(t *testing.T, s *memory.Store, prefix string) map[string]int {
	t.Helper()
	facts, err := s.Recall(context.Background(), "ns", prefix, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]int, len(facts))
	for _, f := range facts {
		out[f.Body]++
	}
	return out
}

func stageByName(t *testing.T, est *Estimate, stage string) StageEstimate {
	t.Helper()
	for _, st := range est.Stages {
		if st.Stage == stage {
			return st
		}
	}
	t.Fatalf("estimate has no %s stage: %+v", stage, est.Stages)
	return StageEstimate{}
}

// TestEstimateZeroWritesZeroModelCalls is the P02 red proof: a preview
// reports the work a real ingest would create while the store sees zero
// writes and the model dependencies see zero calls.
func TestEstimateZeroWritesZeroModelCalls(t *testing.T) {
	s, emb, ext := previewStore(t)
	ctx := context.Background()

	release := forbidModelCalls(emb, ext)
	est, err := DryRun(ctx, s, "ns", "/docs/rb",
		Spec{Path: "testdata/runbook.md", SourceID: "runbook"}, EstimateOptions{})
	release()
	if err != nil {
		t.Fatal(err)
	}
	if emb.calls != 0 || ext.calls != 0 {
		t.Fatalf("model calls during preview: embedder %d, extractor %d; want 0", emb.calls, ext.calls)
	}
	if est.Chunks != 6 || est.Changed != 6 || est.Unchanged != 0 {
		t.Fatalf("estimate = %d chunks, %d changed, %d unchanged; want 6/6/0", est.Chunks, est.Changed, est.Unchanged)
	}
	if len(est.ChangedKeys) != 6 {
		t.Fatalf("changed keys = %d, want the 6 keys a real ingest would write", len(est.ChangedKeys))
	}

	// spy store: no facts, no pipeline run rows, and an empty outbox
	// (every write path enqueues its event in the same transaction).
	if facts, err := s.Recall(ctx, "ns", "/", 0); err != nil || len(facts) != 0 {
		t.Fatalf("preview left %d live facts (err %v): a dry-run must write nothing", len(facts), err)
	}
	if runs, err := s.ListPipelineRuns(ctx, "ns", "", "", 0); err != nil || len(runs) != 0 {
		t.Fatalf("preview recorded %d pipeline runs (err %v): a dry-run must record no stage work", len(runs), err)
	}
	delivered := 0
	n, err := s.DrainOutbox(ctx, 100, func(memory.OutboxEvent) error { delivered++; return nil })
	if err != nil || n != 0 || delivered != 0 {
		t.Fatalf("preview enqueued %d outbox events, delivered %d (err %v): want none", n, delivered, err)
	}
}

// TestEstimateUnchangedDocZeroEnrichment: re-previewing an already
// ingested document predicts zero changed-chunk enrichment - no stage
// runs, no model calls, no tokens and no cost.
func TestEstimateUnchangedDocZeroEnrichment(t *testing.T) {
	s, emb, ext := previewStore(t)
	ctx := context.Background()
	spec := Spec{Path: "testdata/runbook.md", SourceID: "runbook"}
	if _, _, _, _, err := Ingest(ctx, s, "ns", "/docs/rb", "t", spec); err != nil {
		t.Fatal(err)
	}

	embBefore, extBefore := emb.calls, ext.calls
	release := forbidModelCalls(emb, ext)
	est, err := DryRun(ctx, s, "ns", "/docs/rb", spec, EstimateOptions{})
	release()
	if err != nil {
		t.Fatal(err)
	}
	if emb.calls != embBefore || ext.calls != extBefore {
		t.Fatalf("model calls during preview: embedder %d, extractor %d; want 0",
			emb.calls-embBefore, ext.calls-extBefore)
	}
	if est.Changed != 0 || est.Unchanged != 6 {
		t.Fatalf("estimate = %d changed, %d unchanged; want 0/6", est.Changed, est.Unchanged)
	}
	if est.EstimatedInputTokens != 0 || est.EstimatedOutputTokens != 0 {
		t.Fatalf("tokens = %d estimated input, %d estimated output; want 0/0 for an unchanged document",
			est.EstimatedInputTokens, est.EstimatedOutputTokens)
	}
	if len(est.Stages) != 2 {
		t.Fatalf("stages = %+v, want both configured stages listed even when idle", est.Stages)
	}
	for _, st := range est.Stages {
		if st.Runs != 0 || st.ModelCalls != 0 || st.EstimatedPromptTokens != 0 || st.EstimatedCompletionTokens != 0 {
			t.Fatalf("stage %s = %+v, want zero predicted work", st.Stage, st)
		}
		if st.Cost.Status != cost.CostNoWork {
			t.Fatalf("stage %s cost status = %q, want %q (zero predicted calls is exact, not unknown)",
				st.Stage, st.Cost.Status, cost.CostNoWork)
		}
	}
	if est.Embedding == nil || est.Embedding.Calls != 0 {
		t.Fatalf("write-time embedding = %+v, want zero calls", est.Embedding)
	}
	if est.Cost.Status != cost.CostNoWork || est.Cost.MicroUSD != 0 {
		t.Fatalf("total cost = %+v, want no_work at zero", est.Cost)
	}
}

// TestEstimateParityWithWriteDocumentSource is the acceptance proof that
// the preview reuses the real change-detection rules: predicted counters
// and keys equal what WriteDocumentSource then actually does, for a first
// ingest, an edited revision and a shrunk document.
func TestEstimateParityWithWriteDocumentSource(t *testing.T) {
	s, emb, ext := previewStore(t)
	ctx := context.Background()
	spec := Spec{Path: "testdata/runbook.md", SourceID: "runbook"}

	check := func(what string, spec Spec) (written, unchanged, removed, blocked int) {
		t.Helper()
		release := forbidModelCalls(emb, ext)
		est, err := DryRun(ctx, s, "ns", "/docs/rb", spec, EstimateOptions{})
		release()
		if err != nil {
			t.Fatal(err)
		}
		w, u, r, b, err := Ingest(ctx, s, "ns", "/docs/rb", "t", spec)
		if err != nil {
			t.Fatal(err)
		}
		if est.Changed != w || est.Unchanged != u || est.Removed != r || est.Blocked != b {
			t.Fatalf("%s: predicted %d changed/%d unchanged/%d removed/%d blocked, real ingest did %d/%d/%d/%d",
				what, est.Changed, est.Unchanged, est.Removed, est.Blocked, w, u, r, b)
		}
		live := map[string]bool{}
		for _, f := range liveChunks(t, s, "ns", "/docs/rb") {
			live[f.Key] = true
		}
		for _, k := range est.ChangedKeys {
			if !live[k] {
				t.Fatalf("%s: predicted changed key %s was not written", what, k)
			}
		}
		for _, k := range est.RemovedKeys {
			if live[k] {
				t.Fatalf("%s: predicted removed key %s is still live", what, k)
			}
		}
		return w, u, r, b
	}

	if w, _, _, _ := check("first ingest", spec); w != 6 {
		t.Fatalf("first ingest wrote %d chunks, want 6", w)
	}

	raw, err := os.ReadFile("testdata/runbook.md")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	edited := strings.Replace(string(raw),
		"This runbook covers postgres failover.",
		"New intro paragraph.\n\nThis runbook covers postgres failover.", 1)
	if edited == string(raw) {
		t.Fatal("fixture edit did not apply")
	}
	editPath := filepath.Join(dir, "runbook.md")
	if err := os.WriteFile(editPath, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	if w, u, _, _ := check("early insertion", Spec{Path: editPath, SourceID: "runbook"}); w != 1 || u != 6 {
		t.Fatalf("early insertion = %d written/%d unchanged, want 1/6", w, u)
	}

	// a shrunk document: the dropped section's chunks are predicted as
	// removals, and removals create no enrichment work
	shrunk := filepath.Join(dir, "shrunk.md")
	if err := os.WriteFile(shrunk, []byte("# Runbook: Database Failover\n\nThis runbook covers postgres failover.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	release := forbidModelCalls(emb, ext)
	est, err := DryRun(ctx, s, "ns", "/docs/rb", Spec{Path: shrunk, SourceID: "runbook"}, EstimateOptions{})
	release()
	if err != nil {
		t.Fatal(err)
	}
	if est.Removed == 0 {
		t.Fatalf("shrunk document predicted %d removals, want the dropped section's chunks", est.Removed)
	}
	for _, st := range est.Stages {
		if st.Runs != est.Changed {
			t.Fatalf("stage %s predicted %d runs for %d changed chunks: removals must create no stage work",
				st.Stage, st.Runs, est.Changed)
		}
	}
	if _, _, r, _, err := Ingest(ctx, s, "ns", "/docs/rb", "t", Spec{Path: shrunk, SourceID: "runbook"}); err != nil || r != est.Removed {
		t.Fatalf("shrunk ingest removed %d (err %v), predicted %d", r, err, est.Removed)
	}
}

// TestEstimateUnsupportedNotCheap: an input the built-in loaders cannot
// read as text is labeled unestimated - never counted as a cheap URL or
// string - and an unsupported format stays a visible error.
func TestEstimateUnsupportedNotCheap(t *testing.T) {
	s, emb, ext := previewStore(t)
	ctx := context.Background()

	assertUnestimated := func(what string, est *Estimate) {
		t.Helper()
		if !strings.Contains(est.Unestimated, PDFMediaType) {
			t.Fatalf("%s: unestimated = %q, want it to name %s", what, est.Unestimated, PDFMediaType)
		}
		if est.Chunks != 0 || est.Changed != 0 || est.Unchanged != 0 {
			t.Fatalf("%s: chunks/changed/unchanged = %d/%d/%d, want 0/0/0: unsupported input must never be counted as text",
				what, est.Chunks, est.Changed, est.Unchanged)
		}
		if est.EstimatedInputTokens != 0 || est.EstimatedOutputTokens != 0 {
			t.Fatalf("%s: tokens = %d/%d, want 0/0", what, est.EstimatedInputTokens, est.EstimatedOutputTokens)
		}
		if len(est.Stages) != 0 {
			t.Fatalf("%s: stages = %+v, want none: an unestimated input predicts no stage work", what, est.Stages)
		}
		if est.Cost.Status != cost.CostUnknown || est.Cost.MicroUSD != 0 {
			t.Fatalf("%s: cost = %+v, want unknown at zero (never zero-cost certainty)", what, est.Cost)
		}
	}

	// a PDF by extension: the default dry-run does not run the adapter
	release := forbidModelCalls(emb, ext)
	est, err := DryRun(ctx, s, "ns", "/docs/pdf", Spec{Path: "testdata/sample.pdf"}, EstimateOptions{})
	release()
	if err != nil {
		t.Fatalf("pdf preview must report unestimated, not fail: %v", err)
	}
	assertUnestimated("pdf by extension", est)

	// a PDF by served content type over an explicit URL: same answer, so a
	// remote document is never estimated as the URL string
	pdfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", PDFMediaType)
		_, _ = io.WriteString(w, "%PDF-1.7\n1 0 obj\n%%EOF\n")
	}))
	defer pdfSrv.Close()
	release = forbidModelCalls(emb, ext)
	est, err = DryRun(ctx, s, "ns", "/docs/pdf-url", Spec{
		URL: pdfSrv.URL + "/report", fetcher: &Fetcher{allowPrivate: true}}, EstimateOptions{})
	release()
	if err != nil {
		t.Fatalf("pdf url preview must report unestimated, not fail: %v", err)
	}
	assertUnestimated("pdf by content type", est)

	// an unsupported explicit format is a loud error, never an estimate
	if _, err := DryRun(ctx, s, "ns", "/docs/x",
		Spec{Path: "testdata/notes.txt", Format: "docx"}, EstimateOptions{}); err == nil {
		t.Fatal("unsupported format must fail loudly instead of estimating")
	}

	// a text URL is sized as the fetched document, not as the URL string
	body := strings.Repeat("Postgres failover notes for the payments cluster.\n\n", 30)
	textSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, body)
	}))
	defer textSrv.Close()
	url := textSrv.URL + "/notes.txt"
	release = forbidModelCalls(emb, ext)
	est, err = DryRun(ctx, s, "ns", "/docs/url", Spec{URL: url, fetcher: &Fetcher{allowPrivate: true}}, EstimateOptions{})
	release()
	if err != nil {
		t.Fatal(err)
	}
	if est.Unestimated != "" {
		t.Fatalf("text url reported unestimated: %q", est.Unestimated)
	}
	if est.EstimatedInputTokens <= memory.EstimateTokens(url) {
		t.Fatalf("estimated input = %d tokens, want the fetched document (%d), not the URL string (%d)",
			est.EstimatedInputTokens, memory.EstimateTokens(body), memory.EstimateTokens(url))
	}
	if !strings.Contains(strings.Join(est.Exclusions, "\n"), "fetched") {
		t.Fatalf("exclusions %v must disclose the explicit URL fetch", est.Exclusions)
	}
}

// TestEstimateNoAdapterSubprocessDefault: the default dry-run never spawns
// the external adapter (proven with an adapter command that cannot run: a
// subprocess attempt would surface as an exec error), and an explicit
// opt-in does run it and measures the result.
func TestEstimateNoAdapterSubprocessDefault(t *testing.T) {
	s, _, _ := previewStore(t)
	ctx := context.Background()
	broken := Spec{Path: "testdata/sample.pdf",
		PDFAdapter: []string{"punk-estimate-adapter-must-not-run"}}

	est, err := DryRun(ctx, s, "ns", "/docs/pdf", broken, EstimateOptions{})
	if err != nil {
		t.Fatalf("default dry-run attempted the adapter subprocess: %v", err)
	}
	if est.Unestimated == "" {
		t.Fatalf("estimate = %+v, want the pdf labeled unestimated", est)
	}

	// explicit opt-in runs the real helper adapter and measures its output
	allowed := Spec{Path: "testdata/sample.pdf", PDFAdapter: helperAdapter(t, "ok")}
	est, err = DryRun(ctx, s, "ns", "/docs/pdf", allowed, EstimateOptions{AllowAdapter: true})
	if err != nil {
		t.Fatal(err)
	}
	if est.Unestimated != "" {
		t.Fatalf("allowed adapter reported unestimated: %q", est.Unestimated)
	}
	if est.Chunks != 2 || est.Changed != 2 {
		t.Fatalf("allowed adapter estimate = %d chunks/%d changed, want 2/2 (the adapter's two sections)",
			est.Chunks, est.Changed)
	}
	if est.Source.Revision != "pdf-r1" {
		t.Fatalf("source revision = %q, want the adapter's pdf-r1", est.Source.Revision)
	}

	// an explicitly allowed adapter that cannot run stays a loud failure
	if _, err := DryRun(ctx, s, "ns", "/docs/pdf", broken, EstimateOptions{AllowAdapter: true}); err == nil {
		t.Fatal("an explicitly allowed adapter that cannot run must fail loudly, not estimate zero")
	}
}

// TestEstimateMissingPriceUnknown: a missing price - no table, a table
// with no matching entry, or a dependency that reports no model ID - is
// reported unknown, never as a zero cost. A table that really prices the
// model at zero is a distinct priced_zero.
func TestEstimateMissingPriceUnknown(t *testing.T) {
	s, emb, ext := previewStore(t)
	ctx := context.Background()
	spec := Spec{Path: "testdata/runbook.md", SourceID: "runbook"}
	estimate := func(opts EstimateOptions) *Estimate {
		t.Helper()
		release := forbidModelCalls(emb, ext)
		defer release()
		est, err := DryRun(ctx, s, "ns", "/docs/price", spec, opts)
		if err != nil {
			t.Fatal(err)
		}
		return est
	}

	if est := estimate(EstimateOptions{}); est.Cost.Status != cost.CostUnknown || est.Cost.MicroUSD != 0 {
		t.Fatalf("nil price table = %+v, want unknown with no zero-cost certainty", est.Cost)
	} else {
		if ent := stageByName(t, est, memory.StageEntities); ent.Cost.Status != cost.CostUnknown {
			t.Fatalf("entities stage cost = %+v, want unknown", ent.Cost)
		}
		// the embed-link stage predicts no model call of its own (the write
		// path does the embedding), so its zero is exact, not a pricing gap
		if el := stageByName(t, est, memory.StageEmbedLink); el.Cost.Status != cost.CostNoWork {
			t.Fatalf("embed_link stage cost = %+v, want no_work", el.Cost)
		}
		if est.Embedding == nil || est.Embedding.Cost.Status != cost.CostUnknown {
			t.Fatalf("write-time embedding cost = %+v, want unknown", est.Embedding)
		}
	}

	// an empty table has no catch-all entry: unknown, naming the models
	est := estimate(EstimateOptions{Prices: &llm.PriceTable{AsOf: "2026-09-07"}})
	if est.Cost.Status != cost.CostUnknown {
		t.Fatalf("empty price table = %+v, want unknown", est.Cost)
	}
	joined := strings.Join(est.Cost.Unpriced, ",")
	if !strings.Contains(joined, "gpt-5-mini") || !strings.Contains(joined, "nomic-embed-text") {
		t.Fatalf("unpriced models = %v, want both configured models named", est.Cost.Unpriced)
	}

	// a real zero price (local model) is priced_zero, not unknown
	zero := estimate(EstimateOptions{Prices: &llm.PriceTable{
		Models: []llm.ModelPrice{{Match: "*", InputPerMTokUSD: 0, OutputPerMTok: 0}}}})
	if zero.Cost.Status != cost.CostPricedZero || zero.Cost.MicroUSD != 0 {
		t.Fatalf("zero-priced models = %+v, want priced_zero at zero", zero.Cost)
	}

	// the shipped table prices the work: a nonzero, explicitly approximate cost
	prices, err := llm.LoadPrices("")
	if err != nil {
		t.Fatal(err)
	}
	priced := estimate(EstimateOptions{Prices: prices})
	if priced.Cost.Status != cost.CostPriced || priced.Cost.MicroUSD <= 0 {
		t.Fatalf("shipped price table = %+v, want a nonzero priced total", priced.Cost)
	}
	ent := stageByName(t, priced, memory.StageEntities)
	if ent.Cost.MicroUSD <= 0 || ent.Model != "gpt-5-mini" {
		t.Fatalf("entities stage = %+v, want a priced cost and the configured model ID", ent)
	}
}

// TestEstimateNoModelIDUnknown: a configured dependency that does not
// report a model ID leaves its cost unknown instead of being rated
// through the table's catch-all entry.
func TestEstimateNoModelIDUnknown(t *testing.T) {
	s := newTestStore(t)
	s.SetEmbedder(&anonymousEmbedder{})
	s.SetEntityExtractor(&anonymousExtractor{})
	ctx := context.Background()
	prices, err := llm.LoadPrices("")
	if err != nil {
		t.Fatal(err)
	}
	est, err := DryRun(ctx, s, "ns", "/docs/anon",
		Spec{Path: "testdata/runbook.md", SourceID: "runbook"}, EstimateOptions{Prices: prices})
	if err != nil {
		t.Fatal(err)
	}
	if est.Cost.Status != cost.CostUnknown {
		t.Fatalf("cost = %+v, want unknown: an unnamed model cannot be rated", est.Cost)
	}
	for _, st := range est.Stages {
		if st.Model != "" {
			t.Fatalf("stage %s reports model %q, want an empty model ID", st.Stage, st.Model)
		}
	}
	if ent := stageByName(t, est, memory.StageEntities); ent.Cost.Status != cost.CostUnknown {
		t.Fatalf("entities stage cost = %+v, want unknown: an unnamed model cannot be rated", ent.Cost)
	}
	if est.Embedding == nil || est.Embedding.Cost.Status != cost.CostUnknown {
		t.Fatalf("write-time embedding cost = %+v, want unknown", est.Embedding)
	}
	if !strings.Contains(strings.Join(est.Exclusions, "\n"), "model") {
		t.Fatalf("exclusions %v must disclose the missing model IDs", est.Exclusions)
	}
}

// anonymousEmbedder / anonymousExtractor are configured dependencies that
// do not implement memory.ModelNamer.
type anonymousEmbedder struct{}

func (anonymousEmbedder) Dims() int { return 3 }
func (anonymousEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{0.1, 0.2, 0.3}
	}
	return out, nil
}

type anonymousExtractor struct{}

func (anonymousExtractor) Extract(context.Context, string) ([]string, error) { return nil, nil }

// TestEstimateDefenseBlockExcluded: chunks the namespace defense blocks
// are counted and excluded from enrichment - never written, never
// tokenized, never staged - and sensitive source metadata blocks the whole
// ingest exactly as the write path does.
func TestEstimateDefenseBlockExcluded(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	p := filepath.Join(dir, "secrets.md")
	body := "# Secrets\n\nThe db password=hunter2x is rotated monthly.\n\nPlain paragraph about failover.\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	s, emb, ext := previewStore(t)
	s.SetDefense("block")
	spec := Spec{Path: p, SourceID: "secrets"}
	release := forbidModelCalls(emb, ext)
	est, err := DryRun(ctx, s, "ns", "/docs/sec", spec, EstimateOptions{})
	release()
	if err != nil {
		t.Fatal(err)
	}
	if est.Blocked != 1 || est.Changed != 2 || est.Chunks != 3 {
		t.Fatalf("block-mode estimate = %d chunks/%d changed/%d blocked, want 3/2/1", est.Chunks, est.Changed, est.Blocked)
	}
	wantTokens := memory.EstimateTokens("# Secrets") + memory.EstimateTokens("Plain paragraph about failover.")
	ent := stageByName(t, est, memory.StageEntities)
	if ent.EstimatedPromptTokens != wantTokens {
		t.Fatalf("entities prompt tokens = %d, want %d: a blocked chunk contributes no tokens", ent.EstimatedPromptTokens, wantTokens)
	}
	if ent.Runs != est.Changed {
		t.Fatalf("entities runs = %d for %d changed chunks: a blocked chunk creates no stage work", ent.Runs, est.Changed)
	}
	w, _, _, b, err := Ingest(ctx, s, "ns", "/docs/sec", "t", spec)
	if err != nil || w != est.Changed || b != est.Blocked {
		t.Fatalf("real block-mode ingest = %d written/%d blocked (err %v), predicted %d/%d",
			w, b, err, est.Changed, est.Blocked)
	}

	// sensitive source metadata blocks every chunk: metadata is
	// document-wide, so nothing can carry it
	s2, emb2, ext2 := previewStore(t)
	s2.SetDefense("block")
	release = forbidModelCalls(emb2, ext2)
	meta, err := DryRun(ctx, s2, "ns", "/docs/meta",
		Spec{Path: "testdata/runbook.md", SourceID: "AKIAIOSFODNN7EXAMPLE"}, EstimateOptions{})
	release()
	if err != nil {
		t.Fatal(err)
	}
	if !meta.MetaBlocked || meta.Changed != 0 || meta.Blocked != meta.Chunks || meta.Chunks == 0 {
		t.Fatalf("metadata-blocked estimate = %+v, want every chunk blocked and nothing changed", meta)
	}
	for _, st := range meta.Stages {
		if st.Runs != 0 || st.ModelCalls != 0 {
			t.Fatalf("stage %s = %+v, want no work for a blocked ingest", st.Stage, st)
		}
	}
	if _, _, _, b2, err := Ingest(ctx, s2, "ns", "/docs/meta", "t",
		Spec{Path: "testdata/runbook.md", SourceID: "AKIAIOSFODNN7EXAMPLE"}); err != nil || b2 != meta.Blocked {
		t.Fatalf("real metadata-blocked ingest blocked %d (err %v), predicted %d", b2, err, meta.Blocked)
	}

	// redact mode measures the scrubbed body that would actually be written
	s3, emb3, ext3 := previewStore(t)
	s3.SetDefense("redact")
	release = forbidModelCalls(emb3, ext3)
	red, err := DryRun(ctx, s3, "ns", "/docs/sec", spec, EstimateOptions{})
	release()
	if err != nil {
		t.Fatal(err)
	}
	if red.Blocked != 0 || red.Changed != 3 {
		t.Fatalf("redact estimate = %d changed/%d blocked, want 3/0", red.Changed, red.Blocked)
	}
	if _, _, _, _, err := Ingest(ctx, s3, "ns", "/docs/sec", "t", spec); err != nil {
		t.Fatal(err)
	}
	stored := 0
	for _, f := range liveChunks(t, s3, "ns", "/docs/sec") {
		if strings.Contains(f.Body, "hunter2x") {
			t.Fatalf("secret survived the predicted redaction: %q", f.Body)
		}
		stored += memory.EstimateTokens(f.Body)
	}
	if redEnt := stageByName(t, red, memory.StageEntities); redEnt.EstimatedPromptTokens != stored {
		t.Fatalf("redact prompt tokens = %d, want %d (the scrubbed bodies that would be written)",
			redEnt.EstimatedPromptTokens, stored)
	}
}

// TestEstimateExactBytesVersusEstimatedTokens: the estimate reports the
// bytes it actually observed separately from the token counts it
// estimates, keeps the guessed output apart from the estimated input, and
// says so in its exclusions.
func TestEstimateExactBytesVersusEstimatedTokens(t *testing.T) {
	s, emb, ext := previewStore(t)
	ctx := context.Background()
	release := forbidModelCalls(emb, ext)
	est, err := DryRun(ctx, s, "ns", "/docs/rb",
		Spec{Path: "testdata/runbook.md", SourceID: "runbook"}, EstimateOptions{})
	release()
	if err != nil {
		t.Fatal(err)
	}

	doc, err := Load(ctx, Spec{Path: "testdata/runbook.md", SourceID: "runbook"})
	if err != nil {
		t.Fatal(err)
	}
	wantChunkTokens, wantBodyBytes := 0, 0
	for _, sec := range doc.Sections {
		for _, para := range strings.Split(sec.Text, "\n\n") {
			if p := strings.TrimSpace(para); p != "" {
				wantChunkTokens += memory.EstimateTokens(p)
				wantBodyBytes += len(p)
			}
		}
	}
	ent := stageByName(t, est, memory.StageEntities)
	if ent.EstimatedPromptTokens != wantChunkTokens {
		t.Fatalf("entities estimated prompt tokens = %d, want the chunk bodies' bytes/4 estimate %d",
			ent.EstimatedPromptTokens, wantChunkTokens)
	}
	if ent.InputBytes != wantBodyBytes {
		t.Fatalf("entities input bytes = %d, want the exact chunk body bytes %d", ent.InputBytes, wantBodyBytes)
	}
	if est.InputBytes != wantBodyBytes {
		t.Fatalf("estimate input bytes = %d, want the exact chunk body bytes %d", est.InputBytes, wantBodyBytes)
	}
	if est.EmbedInputBytes <= est.InputBytes {
		t.Fatalf("embedding input bytes = %d, want the keyed input above the bare bodies %d",
			est.EmbedInputBytes, est.InputBytes)
	}
	// both token figures are bytes/4 estimates over the exact byte counts -
	// the chunk bodies plus the keyed embedding input - so the total stays
	// within a rounding token per counted string of bytes/4: an estimate,
	// not a tokenizer
	exact := est.InputBytes + est.EmbedInputBytes
	if lo, hi := exact/4, exact/4+2*est.Changed; est.EstimatedInputTokens < lo || est.EstimatedInputTokens > hi {
		t.Fatalf("estimated input tokens = %d, want a bytes/4 estimate near %d exact bytes", est.EstimatedInputTokens, exact)
	}
	if ent.EstimatedCompletionTokens <= 0 {
		t.Fatalf("entities completion tokens = %d, want a heuristic above zero", ent.EstimatedCompletionTokens)
	}
	if ent.Runs != 6 || ent.ModelCalls != 1 {
		t.Fatalf("entities stage = %d runs/%d model calls, want 6 runs batched into 1 call", ent.Runs, ent.ModelCalls)
	}
	// write-time embedding sizes the keyed embedder input, not the bare body
	if est.Embedding == nil || est.Embedding.Calls != 6 {
		t.Fatalf("write-time embedding = %+v, want 6 calls", est.Embedding)
	}
	if est.Embedding.EstimatedPromptTokens <= wantChunkTokens {
		t.Fatalf("embedding estimated prompt tokens = %d, want the keyed input above the bare bodies %d",
			est.Embedding.EstimatedPromptTokens, wantChunkTokens)
	}
	if est.Embedding.EstimatedCompletionTokens != 0 {
		t.Fatalf("embedding completion tokens = %d, want 0 (embedding bills no output)", est.Embedding.EstimatedCompletionTokens)
	}
	if est.EstimatedInputTokens != ent.EstimatedPromptTokens+est.Embedding.EstimatedPromptTokens {
		t.Fatalf("estimated input = %d, want the sum of the estimated stage inputs %d",
			est.EstimatedInputTokens, ent.EstimatedPromptTokens+est.Embedding.EstimatedPromptTokens)
	}
	if est.EstimatedOutputTokens != ent.EstimatedCompletionTokens {
		t.Fatalf("estimated output = %d, want the extraction heuristic %d", est.EstimatedOutputTokens, ent.EstimatedCompletionTokens)
	}
	// the embed-link stage adds links; the write already embedded the chunk
	if el := stageByName(t, est, memory.StageEmbedLink); el.ModelCalls != 0 || el.Runs != 6 {
		t.Fatalf("embed_link stage = %d runs/%d model calls, want 6 runs and no extra model call", el.Runs, el.ModelCalls)
	}
	joined := strings.Join(est.Exclusions, "\n")
	for _, want := range []string{"heuristic", "bytes/4", "byte counts are exact", "no model calls"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("exclusions %v must disclose %q", est.Exclusions, want)
		}
	}
}

// TestPredictedVsRecordedPipelineRuns compares the prediction against the
// P01 work the real pipeline records: one run row per changed chunk per
// configured stage, at the predicted stage version, and nothing at all for
// an unchanged re-ingest.
func TestPredictedVsRecordedPipelineRuns(t *testing.T) {
	s, emb, ext := previewStore(t)
	ctx := context.Background()
	spec := Spec{Path: "testdata/runbook.md", SourceID: "runbook"}

	release := forbidModelCalls(emb, ext)
	est, err := DryRun(ctx, s, "ns", "/docs/rb", spec, EstimateOptions{})
	release()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]StageEstimate{}
	for _, st := range est.Stages {
		want[st.Stage] = st
	}
	if len(want) != 2 {
		t.Fatalf("predicted stages = %+v, want embed_link and entities", est.Stages)
	}

	if _, _, _, _, err := Ingest(ctx, s, "ns", "/docs/rb", "t", spec); err != nil {
		t.Fatal(err)
	}
	// the outbox drain is P01's delivery boundary: it persists the durable
	// stage work for every written chunk before acknowledging the event.
	n, err := s.DrainOutbox(ctx, 100, func(memory.OutboxEvent) error { return nil })
	if err != nil || n != est.Changed {
		t.Fatalf("drained %d outbox events (err %v), want the %d predicted chunk writes", n, err, est.Changed)
	}
	for stage, predicted := range want {
		runs, err := s.ListPipelineRuns(ctx, "ns", stage, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) != predicted.Runs {
			t.Fatalf("recorded %s runs = %d, predicted %d", stage, len(runs), predicted.Runs)
		}
		seen := map[string]bool{}
		for _, r := range runs {
			if r.StageVersion != predicted.Version {
				t.Fatalf("%s run for %s has stage_version %d, predicted %d", stage, r.SourceKey, r.StageVersion, predicted.Version)
			}
			if r.Status != memory.RunPending {
				t.Fatalf("%s run for %s status = %q, want pending (durable work recorded, not executed)",
					stage, r.SourceKey, r.Status)
			}
			seen[r.SourceKey] = true
		}
		for _, k := range est.ChangedKeys {
			if !seen[k] {
				t.Fatalf("predicted changed key %s has no recorded %s run", k, stage)
			}
		}
	}

	// an unchanged re-ingest records no new work: the prediction and the
	// recorded rows both stay put
	release = forbidModelCalls(emb, ext)
	again, err := DryRun(ctx, s, "ns", "/docs/rb", spec, EstimateOptions{})
	release()
	if err != nil {
		t.Fatal(err)
	}
	if again.Changed != 0 {
		t.Fatalf("re-preview = %d changed, want 0", again.Changed)
	}
	if _, _, _, _, err := Ingest(ctx, s, "ns", "/docs/rb", "t", spec); err != nil {
		t.Fatal(err)
	}
	if n, err := s.DrainOutbox(ctx, 100, func(memory.OutboxEvent) error { return nil }); err != nil || n != 0 {
		t.Fatalf("unchanged re-ingest drained %d events (err %v), want 0", n, err)
	}
	for stage, predicted := range want {
		runs, err := s.ListPipelineRuns(ctx, "ns", stage, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) != predicted.Runs {
			t.Fatalf("recorded %s runs after an unchanged re-ingest = %d, want the predicted %d",
				stage, len(runs), predicted.Runs)
		}
	}
	if emb.calls == 0 {
		t.Fatal("the real ingest never embedded: the fake embedder is not wired")
	}
	if ext.calls != 0 {
		t.Fatalf("the extractor ran %d times: this test records durable work only, it never executes stages", ext.calls)
	}
}

// TestEstimateStagesFromOptions: an explicit stage list (what the CLI
// derives from configuration, without constructing any client) is used
// verbatim, including its model IDs and batching. The store here has
// nothing wired, which is the ordinary CLI writer's shape: the configured
// stages are still predicted, as deferred processor work, and no embedding
// is attributed to write time.
func TestEstimateStagesFromOptions(t *testing.T) {
	s := newTestStore(t) // no dependencies wired at all
	ctx := context.Background()
	stages := memory.PipelineStagesForModels("nomic-embed-text", "gpt-5-mini", true)
	if len(stages) != 2 {
		t.Fatalf("PipelineStagesForModels = %+v, want the embed-link and entity stages", stages)
	}
	est, err := DryRun(ctx, s, "ns", "/docs/rb",
		Spec{Path: "testdata/runbook.md", SourceID: "runbook"}, EstimateOptions{Pipeline: stages})
	if err != nil {
		t.Fatal(err)
	}
	if len(est.Stages) != 2 {
		t.Fatalf("stages = %+v, want the two configured stages", est.Stages)
	}
	ent := stageByName(t, est, memory.StageEntities)
	if ent.Model != "gpt-5-mini" || ent.ModelCalls != 1 || ent.Runs != 6 {
		t.Fatalf("entities stage = %+v, want gpt-5-mini, 6 runs in 1 batched call", ent)
	}
	el := stageByName(t, est, memory.StageEmbedLink)
	if el.Model != "nomic-embed-text" || el.ModelCalls != 6 {
		t.Fatalf("embed_link stage = %+v, want the configured embedding model carrying the 6 deferred calls", el)
	}
	if est.WriterEmbeds || est.Embedding != nil {
		t.Fatalf("write-time embedding = %+v (WriterEmbeds %v), want none: this store has no embedder wired",
			est.Embedding, est.WriterEmbeds)
	}
}

// TestEstimateSurfacesMalformedLiveChunk: a live chunk whose stored
// attributes will not decode is reported in the estimate and in its
// exclusions. The dry-run neither repairs the row - an ordinary read
// quarantines it, which is a write - nor reads the corruption as absence:
// the chunk it would rewrite is still predicted, and the storage counts do
// not move.
func TestEstimateSurfacesMalformedLiveChunk(t *testing.T) {
	s, db, emb, ext := previewStoreDB(t)
	ctx := context.Background()
	spec := Spec{Path: "testdata/runbook.md", SourceID: "runbook"}
	if _, _, _, _, err := Ingest(ctx, s, "ns", "/docs/rb", "t", spec); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE memories SET attributes = '{bad' WHERE key LIKE '/docs/rb/chunk-%'`); err != nil {
		t.Fatal(err)
	}
	counts := func() map[string]int {
		t.Helper()
		out := map[string]int{}
		for _, table := range []string{"memories", "memories_quarantine"} {
			var n int
			if err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
				t.Fatal(err)
			}
			out[table] = n
		}
		return out
	}
	before := counts()

	release := forbidModelCalls(emb, ext)
	est, err := DryRun(ctx, s, "ns", "/docs/rb", spec, EstimateOptions{})
	release()
	if err != nil {
		t.Fatal(err)
	}
	after := counts()
	for table, n := range before {
		if after[table] != n {
			t.Errorf("dry-run wrote storage: %s before=%d after=%d", table, n, after[table])
		}
	}
	if len(est.Malformed) != 6 {
		t.Fatalf("malformed = %+v, want all 6 corrupted chunks reported", est.Malformed)
	}
	if est.Changed != 6 || est.Unchanged != 0 {
		t.Fatalf("estimate = %d changed, %d unchanged; want the 6 chunks a real ingest would rewrite after quarantining",
			est.Changed, est.Unchanged)
	}
	joined := strings.Join(est.Exclusions, "\n")
	for _, want := range []string{"undecodable stored attributes", "quarantines", "repaired nothing"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("exclusions %v must disclose %q", est.Exclusions, want)
		}
	}
}

// TestEstimateWriteEmbeddingFollowsStoreWiring: embedding work is
// attributed by the wiring the preview actually ran against, not by the
// configured processor's stage list. An unwired writer - the ordinary CLI
// shape - stores chunks unembedded, so it gets zero write-time calls and
// the changed-chunk calls are predicted for the deferred embed_link stage;
// a library store with an embedder keeps the write-time forecast. Both
// halves are checked against what the real write then does.
func TestEstimateWriteEmbeddingFollowsStoreWiring(t *testing.T) {
	ctx := context.Background()
	spec := Spec{Path: "testdata/runbook.md", SourceID: "runbook"}
	// the configured server's stages: what a deployment would run when it
	// drains the outbox, independent of what the writer has wired
	pipeline := memory.PipelineStagesForModels("nomic-embed-text", "gpt-5-mini", true)

	t.Run("unwired writer", func(t *testing.T) {
		s, db, emb, ext := writerStoreDB(t, false, true)
		release := forbidModelCalls(emb, ext)
		est, err := DryRun(ctx, s, "ns", "/docs/rb", spec, EstimateOptions{Pipeline: pipeline})
		release()
		if err != nil {
			t.Fatal(err)
		}
		if est.WriterEmbeds {
			t.Fatal("WriterEmbeds = true, want false: this store has no embedder wired")
		}
		if est.Embedding != nil {
			t.Fatalf("write-time embedding = %+v, want none: this writer embeds nothing", est.Embedding)
		}
		el := stageByName(t, est, memory.StageEmbedLink)
		if el.Runs != 6 || el.ModelCalls != 6 {
			t.Fatalf("embed_link stage = %d runs/%d model calls, want the 6 deferred embedding calls", el.Runs, el.ModelCalls)
		}
		if el.Model != "nomic-embed-text" {
			t.Fatalf("embed_link model = %q, want the configured embedding model", el.Model)
		}
		joined := strings.Join(est.Exclusions, "\n")
		for _, want := range []string{"no embedder is wired", "drains the outbox", "deferred processor work"} {
			if !strings.Contains(joined, want) {
				t.Fatalf("exclusions %v must disclose %q", est.Exclusions, want)
			}
		}

		// parity with the real write: no embedder call, no stored vector
		before := emb.calls
		if _, _, _, _, err := Ingest(ctx, s, "ns", "/docs/rb", "t", spec); err != nil {
			t.Fatal(err)
		}
		if made := emb.calls - before; made != 0 {
			t.Fatalf("real ingest made %d embedder calls, want the 0 the preview predicted", made)
		}
		if n := embeddedChunkCount(t, db, "/docs/rb"); n != 0 {
			t.Fatalf("real ingest stored %d embedded chunks, want 0: the deferred stage embeds later", n)
		}
		if ext.calls != 0 {
			t.Fatalf("real ingest made %d extractor calls: stage work is deferred, not write-time", ext.calls)
		}

		// parity with the real deferred stage: run the processor's own
		// enrich pass over the chunks this writer left unembedded and diff
		// the exact text the model receives against the stage forecast.
		proc := memory.New(db, nil)
		proc.SetEmbedder(emb)
		from := len(emb.seen)
		for _, k := range est.ChangedKeys {
			if _, err := proc.EnrichKey(ctx, "ns", k, 0, 0); err != nil {
				t.Fatal(err)
			}
		}
		sent := emb.seen[from:]
		if len(sent) != el.ModelCalls {
			t.Fatalf("the real embed_link stage sent %d embedding inputs, want the %d model calls the preview predicted",
				len(sent), el.ModelCalls)
		}
		bodies := chunkBodyCounts(t, s, "/docs/rb")
		for _, text := range sent {
			if bodies[text] == 0 {
				t.Fatalf("the real embed_link stage sent %q, which is no stored chunk body: memory.enrichKey embeds facts[self].Body alone, so the forecast must be sized over bodies", text)
			}
			bodies[text]--
		}
		gotBytes, gotTokens := inputTotals(sent)
		if gotBytes != el.InputBytes {
			t.Fatalf("embed_link stage forecasts %d exact input bytes, but the real stage sends %d", el.InputBytes, gotBytes)
		}
		if gotTokens != el.EstimatedPromptTokens {
			t.Fatalf("embed_link stage forecasts ~%d prompt tokens, but bytes/4 over the real input is %d",
				el.EstimatedPromptTokens, gotTokens)
		}
		if gotBytes == est.EmbedInputBytes {
			t.Fatalf("the real deferred input (%d bytes) equals the keyed write-time input: the two shapes must be priced separately", gotBytes)
		}
		if n := embeddedChunkCount(t, db, "/docs/rb"); n != el.ModelCalls {
			t.Fatalf("the real embed_link stage left %d chunks embedded, want the %d calls it was forecast to make", n, el.ModelCalls)
		}
	})

	t.Run("wired writer", func(t *testing.T) {
		s, db, emb, ext := writerStoreDB(t, true, true)
		release := forbidModelCalls(emb, ext)
		est, err := DryRun(ctx, s, "ns", "/docs/rb", spec, EstimateOptions{Pipeline: pipeline})
		release()
		if err != nil {
			t.Fatal(err)
		}
		if !est.WriterEmbeds {
			t.Fatal("WriterEmbeds = false, want true: this store has an embedder wired")
		}
		if est.Embedding == nil || est.Embedding.Calls != 6 || est.Embedding.Model != "nomic-embed-text" {
			t.Fatalf("write-time embedding = %+v, want 6 calls on the wired embedder", est.Embedding)
		}
		if el := stageByName(t, est, memory.StageEmbedLink); el.ModelCalls != 0 || el.Runs != 6 {
			t.Fatalf("embed_link stage = %d runs/%d model calls, want 6 runs and no call of its own: the writer already embedded",
				el.Runs, el.ModelCalls)
		}

		// parity with the real write: exactly the predicted write-time calls,
		// over exactly the predicted keyed text
		before := emb.calls
		from := len(emb.seen)
		if _, _, _, _, err := Ingest(ctx, s, "ns", "/docs/rb", "t", spec); err != nil {
			t.Fatal(err)
		}
		if made := emb.calls - before; made != est.Embedding.Calls {
			t.Fatalf("real ingest made %d embedder calls, want the %d the preview predicted", made, est.Embedding.Calls)
		}
		sent := emb.seen[from:]
		facts, err := s.Recall(ctx, "ns", "/docs/rb", 0)
		if err != nil {
			t.Fatal(err)
		}
		keyed := make(map[string]int, len(facts))
		for _, f := range facts {
			keyed["key: "+f.Key+"\n"+f.Body]++ // memory.embedText, the write path's input
		}
		for _, text := range sent {
			if keyed[text] == 0 {
				t.Fatalf("the real write sent %q, which is no keyed embedText(key, body) of a stored chunk", text)
			}
			keyed[text]--
		}
		gotBytes, gotTokens := inputTotals(sent)
		if gotBytes != est.Embedding.InputBytes {
			t.Fatalf("write-time embedding forecasts %d exact input bytes, but the real write sends %d",
				est.Embedding.InputBytes, gotBytes)
		}
		if gotTokens != est.Embedding.EstimatedPromptTokens {
			t.Fatalf("write-time embedding forecasts ~%d prompt tokens, but bytes/4 over the real input is %d",
				est.Embedding.EstimatedPromptTokens, gotTokens)
		}
		if gotBytes != est.EmbedInputBytes {
			t.Fatalf("Estimate.EmbedInputBytes = %d, want the %d keyed bytes the real write sent", est.EmbedInputBytes, gotBytes)
		}
		if n := embeddedChunkCount(t, db, "/docs/rb"); n != est.Embedding.Calls {
			t.Fatalf("real ingest stored %d embedded chunks, want the %d predicted", n, est.Embedding.Calls)
		}

		// the deferred stage adds links but embeds nothing again: the writer
		// already embedded every chunk, which is why its forecast is 0 calls
		el := stageByName(t, est, memory.StageEmbedLink)
		proc := memory.New(db, nil)
		proc.SetEmbedder(emb)
		from = len(emb.seen)
		for _, k := range est.ChangedKeys {
			if _, err := proc.EnrichKey(ctx, "ns", k, 0, 0); err != nil {
				t.Fatal(err)
			}
		}
		if extra := emb.seen[from:]; len(extra) != el.ModelCalls {
			t.Fatalf("the real embed_link stage sent %d embedding inputs after a wired write, want the %d forecast",
				len(extra), el.ModelCalls)
		}
	})
}

// TestEstimateNoEmbeddingAnywhereIsDisclosed: a writer with no embedder and
// a configured stage list with no embed_link stage predicts no embedding at
// all, and says so instead of leaving the reader to assume one.
func TestEstimateNoEmbeddingAnywhereIsDisclosed(t *testing.T) {
	s, _, emb, ext := writerStoreDB(t, false, true)
	release := forbidModelCalls(emb, ext)
	est, err := DryRun(context.Background(), s, "ns", "/docs/rb",
		Spec{Path: "testdata/runbook.md", SourceID: "runbook"},
		EstimateOptions{Pipeline: memory.PipelineStagesForModels("", "gpt-5-mini", true)})
	release()
	if err != nil {
		t.Fatal(err)
	}
	if est.WriterEmbeds || est.Embedding != nil {
		t.Fatalf("embedding = %+v (WriterEmbeds %v), want none", est.Embedding, est.WriterEmbeds)
	}
	if estimateHasEmbedLink(est) {
		t.Fatalf("stages = %+v, want no embed_link stage", est.Stages)
	}
	if !strings.Contains(strings.Join(est.Exclusions, "\n"), "no embedding is predicted at all") {
		t.Fatalf("exclusions %v must disclose that nothing embeds", est.Exclusions)
	}
}

func estimateHasEmbedLink(est *Estimate) bool {
	for _, st := range est.Stages {
		if st.Stage == memory.StageEmbedLink {
			return true
		}
	}
	return false
}
