package ingest

// Dry-run ingestion preview (task P02): what an ingest would change and
// what enrichment work that would create, sized before anything is
// written.
//
// The preview reuses the real rules instead of restating them: Load runs
// the same loaders and format detection a real ingest runs, and
// memory.Store.PreviewDocumentSource diffs the loaded document with
// WriteDocumentSource's own chunker, chunk identity, ownership boundary
// and defense semantics.
//
// Two wirings are kept apart, because they are two different machines.
// The WRITER is the store this preview runs against: if it has an Embedder
// wired, the write path itself embeds every chunk it stores, and that is
// the write_embedding workload. The PROCESSOR is the configured server
// that later drains the outbox and runs the P01 stages; its stage set
// comes from EstimateOptions.Pipeline, or from
// memory.Store.ConfiguredPipelineStages - persistPipelineWork's own list -
// so predicted stage calls line up with the run rows P01 records. A
// writer with no embedder (the ordinary CLI's openMemory) stores chunks
// unembedded, so the embedding calls belong to the processor's embed_link
// stage, not to write time; predicting them at write time would promise
// work the writer never does. The stage figures are therefore deferred
// work, and the exclusions say that they assume the configured server
// drains the outbox.
//
// A default dry-run makes no memory writes, no model calls and no loader
// subprocess calls: the external PDF adapter is suppressed, which makes an
// expensive unsupported format UNESTIMATED (labeled, with an unknown cost)
// rather than silently counted as cheap text. An explicit --url fetch is
// honoured, because it is explicit and SSRF-guarded, and is disclosed in
// the exclusions.
//
// Every figure is labeled by kind. Byte counts are exact: the bodies that
// would be written and the keyed input an embedder is called with. Token
// counts are estimates over those bytes (memory.EstimateTokens, bytes/4 -
// the same estimator the token budget uses), and output tokens are a
// heuristic on top, because no preview can know what a model will answer.
// Cost is rated locally against llm.PriceTable through internal/cost, and
// a missing price is reported unknown - never as a zero.
//
// Mechanism adapted from Cognee's cognee/modules/cognify/estimator.py
// (commit 78ff576559a7f75f65884c5bd90b22cdc790016e): a dry run that reuses
// the real pipeline pieces, reports per-stage calls/input/output/cost,
// separates estimated input from heuristic output, lists what it excludes,
// and refuses to price what it cannot price. Reimplemented in Go on punk's
// loader, chunker and pipeline boundaries.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hypervisor-io/punk-records/internal/cost"
	"github.com/hypervisor-io/punk-records/internal/llm"
	"github.com/hypervisor-io/punk-records/internal/memory"
)

const (
	// entityOutputTokenRatio and minEntityOutputTokensPerChunk are the
	// heuristic for an extraction call's completion tokens: a ratio of
	// each chunk's estimated tokens with a per-chunk floor. Cognee's
	// estimator uses the same shape (GRAPH_OUTPUT_TOKEN_RATIO with
	// MIN_GRAPH_OUTPUT_TOKENS_PER_CHUNK) for its graph extraction; punk's
	// answer is a much smaller typed-entity list, so the ratio is lower.
	// It is a guess and is reported as one.
	entityOutputTokenRatio        = 0.25
	minEntityOutputTokensPerChunk = 64
)

// EstimateOptions configures a dry-run.
type EstimateOptions struct {
	// Pipeline is the DEFERRED PROCESSOR's stage set: the P01 stages the
	// configured server runs when it drains the outbox after this ingest.
	// Nil derives it from the store (memory.Store.ConfiguredPipelineStages):
	// the stages the enricher bound to this store records work for. A CLI
	// passes the stages its configuration implies instead, because
	// constructing the real dependencies has side effects a preview must
	// not have. This list never decides what the writer itself does at
	// write time - that comes from the store's actual wiring.
	Pipeline []memory.PipelineStage
	// Prices rates the predicted usage. Nil reports every cost unknown.
	Prices *llm.PriceTable
	// AllowAdapter runs the external PDF adapter subprocess. The default
	// dry-run never spawns one: a PDF is reported unestimated instead.
	AllowAdapter bool
}

// StageEstimate is one configured enrichment stage's predicted work.
// Runs counts the pipeline run rows the stage would record (one per
// changed chunk - P01 keys a run by source key and revision); ModelCalls
// counts the model calls those runs make, which is lower when the stage
// batches keys per call. InputBytes is the exact byte count of the text
// this stage actually sends: the stored chunk bodies for the entity
// stage, and - for an embed_link stage that has to embed because the
// writer did not - the stored bodies again, because memory.enrichKey
// backfills from facts[self].Body alone, never the keyed text the write
// path uses. EstimatedPromptTokens and EstimatedCompletionTokens are
// estimates over those bytes (punk counts tokens as bytes/4, and
// completion tokens are a heuristic), so neither is a measurement and
// neither becomes one because the model ID or the price table is known.
type StageEstimate struct {
	Stage                     string      `json:"stage"`
	Version                   int         `json:"stage_version"`
	Model                     string      `json:"model,omitempty"`
	Runs                      int         `json:"runs"`
	ModelCalls                int         `json:"model_calls"`
	InputBytes                int         `json:"input_bytes"`
	EstimatedPromptTokens     int         `json:"estimated_prompt_tokens"`
	EstimatedCompletionTokens int         `json:"estimated_completion_tokens"`
	Cost                      cost.Rating `json:"cost"`
}

// WorkEstimate is a predicted model workload that is not a P01 stage:
// the embedding the WRITER itself performs while writing, when it has an
// Embedder wired. Its InputBytes is the exact keyed input the write path
// sends - embedText(key, body), not the bare body - which is a different
// shape from the deferred embed_link stage's input. The token counts are
// estimates, as in StageEstimate.
type WorkEstimate struct {
	Model                     string      `json:"model,omitempty"`
	Calls                     int         `json:"calls"`
	InputBytes                int         `json:"input_bytes"`
	EstimatedPromptTokens     int         `json:"estimated_prompt_tokens"`
	EstimatedCompletionTokens int         `json:"estimated_completion_tokens"`
	Cost                      cost.Rating `json:"cost"`
}

// Estimate is a dry-run's answer: the chunk delta a real ingest would
// produce, the stage work that delta would trigger, the tokens and cost
// that work implies, and what the estimate does not cover.
type Estimate struct {
	Namespace string                `json:"namespace"`
	Prefix    string                `json:"prefix"`
	Source    memory.DocumentSource `json:"source"`

	// Unestimated is non-empty when the input could not be sized at all -
	// an expensive format whose loader the dry-run refuses to run. Every
	// count below is then absent and the cost unknown: an unestimated
	// input is never reported as a cheap or free one.
	Unestimated string `json:"unestimated,omitempty"`

	Chunks      int      `json:"chunks"`
	Changed     int      `json:"changed"`
	Unchanged   int      `json:"unchanged"`
	Removed     int      `json:"removed"`
	Blocked     int      `json:"blocked"`
	MetaBlocked bool     `json:"meta_blocked,omitempty"`
	ChangedKeys []string `json:"changed_keys,omitempty"`
	RemovedKeys []string `json:"removed_keys,omitempty"`

	// Malformed lists the live chunks under prefix whose stored
	// attributes will not decode. A real ingest quarantines each one
	// before it diffs, so a chunk landing on such a key is counted
	// changed here; the preview itself repairs nothing and says so in
	// Exclusions.
	Malformed []memory.MalformedRow `json:"malformed,omitempty"`

	// Stages lists every configured stage of the deferred processor,
	// including the idle ones: an unchanged document still shows the
	// stages that would have run. This is work the configured server does
	// when it drains the outbox, not work the ingest does.
	Stages []StageEstimate `json:"stages,omitempty"`
	// WriterEmbeds reports whether the store this preview ran against
	// actually has an Embedder wired, which is the only thing that makes
	// the write path embed. When it is false the writer stores chunks
	// unembedded - the ordinary CLI's openMemory shape - Embedding is nil,
	// and the changed-chunk embedding calls are predicted for the
	// processor's embed_link stage instead.
	WriterEmbeds bool `json:"writer_embeds"`
	// Embedding is the writer's own embedding of the changed chunks, and
	// is nil when WriterEmbeds is false. With a wired writer this is where
	// the embedder is really called, and the embed-link stage only
	// re-embeds a chunk whose write-time embedding failed.
	Embedding *WorkEstimate `json:"write_embedding,omitempty"`

	// InputBytes and EmbedInputBytes are exact observed counts: the bytes
	// of the chunk bodies a write would store, and the bytes of the keyed
	// input (key + body) a write-time embedding would send. When
	// WriterEmbeds is false nobody sends that keyed input - the deferred
	// embed_link stage embeds the stored bodies instead - so
	// EmbedInputBytes is then the size of a workload this deployment does
	// not have, reported for comparison and rated nowhere.
	// EstimatedInputTokens and EstimatedOutputTokens are estimates over
	// the bytes that are really sent - bytes/4 for input, a
	// ratio-with-floor heuristic for output - reported separately because
	// a byte count is observed and a token count is not.
	InputBytes            int `json:"input_bytes"`
	EmbedInputBytes       int `json:"embed_input_bytes"`
	EstimatedInputTokens  int `json:"estimated_input_tokens"`
	EstimatedOutputTokens int `json:"estimated_output_tokens"`

	Cost cost.Preview `json:"cost"`
	// Exclusions lists what the estimate does not cover and every
	// approximation it makes, so a reader never mistakes the figure for
	// an invoice.
	Exclusions []string `json:"exclusions,omitempty"`
}

// pdfAdapterRefusal is PDFLoader's own "no external extractor configured"
// message, captured from the loader rather than hardcoded so the dry-run
// cannot drift from it: a default dry-run suppresses Spec.PDFAdapter, so
// that exact error is how a PDF input - by extension, by served content
// type or by byte signature - announces itself.
func pdfAdapterRefusal() string {
	_, err := PDFLoader{}.Load(context.Background(), LoadInput{})
	if err == nil {
		return ""
	}
	return err.Error()
}

// DryRun previews one ingest: it loads spec through the real loaders,
// diffs the result against the live chunks under prefix, and sizes the
// enrichment work the delta would create. It writes nothing, calls no
// model, and - unless AllowAdapter is set - spawns no loader subprocess.
// A load failure is returned as an error, except for a PDF the dry-run
// refused to hand to an external adapter: that is an unestimated input,
// not a failed preview.
func DryRun(ctx context.Context, s *memory.Store, ns, prefix string, spec Spec, opts EstimateOptions) (*Estimate, error) {
	if s == nil {
		return nil, errors.New("ingest: estimate needs a memory store")
	}
	loadSpec := spec
	if !opts.AllowAdapter {
		// PDFLoader refuses an empty command before it would exec, so
		// suppressing the adapter is what keeps the external process out
		// of a default dry-run.
		loadSpec.PDFAdapter = nil
	}
	doc, err := Load(ctx, loadSpec)
	if err != nil {
		if !opts.AllowAdapter && err.Error() == pdfAdapterRefusal() {
			return unestimatedEstimate(ns, prefix, spec), nil
		}
		return nil, err
	}
	prev, err := s.PreviewDocumentSource(ctx, ns, prefix, *doc)
	if err != nil {
		return nil, err
	}
	// Two wirings, two predictions. stages is the deferred processor's
	// stage set; wired is what the store this preview runs against really
	// has, which is the only thing that decides whether the write path
	// itself embeds. A CLI passes opts.Pipeline from its configuration
	// while its writer store has no dependencies wired at all, and the two
	// must not be conflated: attributing the processor's embedding calls
	// to write time would promise work the writer never does.
	stages := opts.Pipeline
	wired := s.ConfiguredPipelineStages()
	if stages == nil {
		stages = wired
	}
	writerEmbeds, writerEmbedModel := false, ""
	for _, st := range wired {
		if st.Name == memory.StageEmbedLink {
			writerEmbeds = true
			writerEmbedModel = st.Model
		}
	}

	est := &Estimate{
		Namespace: ns, Prefix: prefix, Source: doc.Source,
		Chunks: prev.Chunks, Changed: len(prev.Changed), Unchanged: prev.Unchanged,
		Removed: len(prev.Removed), Blocked: prev.Blocked, MetaBlocked: prev.MetaBlocked,
		RemovedKeys:  prev.Removed,
		Malformed:    prev.Malformed,
		Stages:       make([]StageEstimate, len(stages)),
		WriterEmbeds: writerEmbeds,
	}
	for _, c := range prev.Changed {
		est.ChangedKeys = append(est.ChangedKeys, c.Key)
	}

	// exact bytes are observed; token counts are estimates over them
	bodyBytes, embedBytes, bodyTokens, embedTokens := 0, 0, 0, 0
	for _, c := range prev.Changed {
		bodyBytes += c.Bytes
		embedBytes += c.EmbedBytes
		bodyTokens += c.EstimatedTokens
		embedTokens += c.EstimatedEmbedTokens
	}
	est.InputBytes, est.EmbedInputBytes = bodyBytes, embedBytes

	var usage []cost.PreviewUsage
	var ratings []*cost.Rating
	add := func(stage, model string, calls, prompt, completion int, dst *cost.Rating) {
		usage = append(usage, cost.PreviewUsage{
			Stage: stage, Model: model, Calls: calls,
			PromptTokens: prompt, CompletionTokens: completion})
		ratings = append(ratings, dst)
	}

	changed := len(prev.Changed)
	embedStage, entityStage := -1, -1
	for i, st := range stages {
		switch st.Name {
		case memory.StageEmbedLink:
			embedStage = i
		case memory.StageEntities:
			entityStage = i
		}
	}
	// The writer's own embedding: only a store with an Embedder wired
	// embeds while it writes, and then it embeds every changed chunk,
	// keyed by the chunk key.
	if writerEmbeds {
		est.Embedding = &WorkEstimate{
			Model: writerEmbedModel, Calls: changed,
			InputBytes: embedBytes, EstimatedPromptTokens: embedTokens}
		add("write_embedding", est.Embedding.Model, changed, embedTokens, 0, &est.Embedding.Cost)
	}
	for i, st := range stages {
		se := StageEstimate{Stage: st.Name, Version: st.Version, Model: st.Model, Runs: changed}
		switch i {
		case embedStage:
			if writerEmbeds {
				// the writer already embedded every changed chunk; this
				// stage adds similar_to links and re-embeds only a chunk
				// whose write-time embedding failed, so it predicts no
				// model call of its own.
				se.ModelCalls = 0
			} else {
				// nothing was embedded at write time, so this stage embeds
				// the changed chunks itself when the processor drains the
				// outbox. Its input is NOT the keyed text the write path
				// sends: memory.enrichKey backfills from the stored body
				// alone (facts[self].Body), so the forecast is sized over
				// the bodies, one call per chunk.
				se.ModelCalls = changed
				se.InputBytes = bodyBytes
				se.EstimatedPromptTokens = bodyTokens
			}
		case entityStage:
			se.ModelCalls = changed
			if st.BatchKeys > 1 && changed > 0 {
				se.ModelCalls = (changed + st.BatchKeys - 1) / st.BatchKeys
			}
			se.InputBytes = bodyBytes
			se.EstimatedPromptTokens = bodyTokens
			se.EstimatedCompletionTokens = heuristicCompletionTokens(prev.Changed)
		default:
			// a stage this package does not know: predict its run rows and
			// one call per changed chunk over the same bodies, with no
			// output guess, and say so.
			se.ModelCalls = changed
			se.InputBytes = bodyBytes
			se.EstimatedPromptTokens = bodyTokens
		}
		est.Stages[i] = se
		add(st.Name, st.Model, se.ModelCalls, se.EstimatedPromptTokens, se.EstimatedCompletionTokens, &est.Stages[i].Cost)
	}

	total := cost.RatePreview(opts.Prices, usage)
	for i, c := range total.Costs {
		*ratings[i] = c.Rating
	}
	est.Cost = total
	for _, st := range est.Stages {
		est.EstimatedInputTokens += st.EstimatedPromptTokens
		est.EstimatedOutputTokens += st.EstimatedCompletionTokens
	}
	if est.Embedding != nil {
		est.EstimatedInputTokens += est.Embedding.EstimatedPromptTokens
		est.EstimatedOutputTokens += est.Embedding.EstimatedCompletionTokens
	}
	est.Exclusions = exclusions(spec, opts, stages, embedStage, entityStage, prev, est)
	return est, nil
}

// heuristicCompletionTokens guesses the extraction stage's output: a
// ratio of each chunk's estimated tokens with a per-chunk floor. A guess
// stacked on an estimate - the chunk token counts are bytes/4 - and the
// estimate reports it as one.
func heuristicCompletionTokens(chunks []memory.PreviewChunk) int {
	total := 0
	for _, c := range chunks {
		n := int(float64(c.EstimatedTokens) * entityOutputTokenRatio)
		if n < minEntityOutputTokensPerChunk {
			n = minEntityOutputTokensPerChunk
		}
		total += n
	}
	return total
}

// unestimatedEstimate is the answer for an input the dry-run refuses to
// measure: an expensive format whose loader runs an external process. The
// counts stay absent and the cost unknown, so an unsupported document is
// never mistaken for a cheap or free one.
func unestimatedEstimate(ns, prefix string, spec Spec) *Estimate {
	input := spec.Path
	if input == "" {
		input = spec.URL
	}
	est := &Estimate{
		Namespace: ns,
		Prefix:    prefix,
		Source: memory.DocumentSource{
			ID: spec.SourceID, URI: input, Revision: spec.Revision, MediaType: PDFMediaType},
		Unestimated: fmt.Sprintf("%s: the external adapter is not run in a dry-run, so this input's chunks, tokens, stage calls and cost are unestimated (pass --allow-adapter to run it and measure the real output)", PDFMediaType),
		Cost:        cost.Preview{Status: cost.CostUnknown},
		Exclusions: []string{
			"dry-run: no memory writes, no model calls and no loader subprocess were made",
			"unestimated: chunks, tokens, stage calls and cost are unknown, not zero",
			fmt.Sprintf("input %s was read and detected as %s; its bytes were never counted as text", input, PDFMediaType),
		},
	}
	return est
}

// exclusions states what the estimate does not cover and every
// approximation in it.
func exclusions(spec Spec, opts EstimateOptions, stages []memory.PipelineStage,
	embedStage, entityStage int, prev *memory.DocumentPreview, est *Estimate) []string {
	ex := []string{"dry-run: no memory writes and no model calls were made"}
	ex = append(ex, fmt.Sprintf(
		"byte counts are exact (%d chunk body bytes over %d changed chunks); every token count is an estimate: punk counts tokens as bytes/4 (memory.EstimateTokens), not with the model's tokenizer, and a known model ID or price table does not make that exact",
		est.InputBytes, est.Changed))
	if len(prev.Malformed) > 0 {
		keys := make([]string, len(prev.Malformed))
		for i, m := range prev.Malformed {
			keys[i] = m.Key
		}
		ex = append(ex, fmt.Sprintf(
			"%d live chunk(s) under the prefix have undecodable stored attributes (%s): a real ingest quarantines each row - moving it out of the namespace - before diffing, so a chunk landing on such a key is counted changed here; this preview repaired nothing and left every row in place",
			len(prev.Malformed), strings.Join(keys, ", ")))
	}
	if spec.URL != "" {
		ex = append(ex, fmt.Sprintf(
			"the explicit --url input %s was fetched (SSRF-guarded) to measure the real document; the URL string itself was never estimated", spec.URL))
	}
	if opts.AllowAdapter && len(spec.PDFAdapter) > 0 {
		ex = append(ex, fmt.Sprintf(
			"the external adapter %q was run because the dry-run was explicitly allowed to", strings.Join(spec.PDFAdapter, " ")))
	}
	if prev.MetaBlocked {
		ex = append(ex, fmt.Sprintf(
			"source metadata is sensitive under this namespace's block defense: the whole ingest would be blocked, so all %d chunks are counted blocked and nothing would be written", prev.Chunks))
	}
	if prev.Unchanged > 0 {
		ex = append(ex, fmt.Sprintf(
			"%d unchanged chunks would not be rewritten and create no enrichment work", prev.Unchanged))
	}
	if len(prev.Removed) > 0 {
		ex = append(ex, fmt.Sprintf(
			"%d removed chunks would be tombstoned and create no enrichment work: a tombstone records no stage run", len(prev.Removed)))
	}
	if prev.Blocked > 0 && !prev.MetaBlocked {
		ex = append(ex, fmt.Sprintf(
			"%d blocked chunks (namespace defense, or a destination key owned by another source) would not be written and create no enrichment work", prev.Blocked))
	}
	if len(stages) > 0 {
		ex = append(ex, "the stage figures are deferred processor work: they assume the configured server drains the outbox and runs those stages after this ingest")
		if est.WriterEmbeds {
			ex = append(ex, fmt.Sprintf(
				"a real ingest by this writer DOES make model calls: the %d write-time embedding call(s) counted above happen inside the write itself, while the stage figures are work the processor does later",
				est.Changed))
		} else {
			ex = append(ex, "a real ingest by this writer makes no model call at all: it stores every chunk unembedded and leaves all of the counted work to the processor")
		}
	}
	if embedStage >= 0 {
		ex = append(ex, "embedding calls bill no completion tokens")
		if est.WriterEmbeds {
			ex = append(ex,
				fmt.Sprintf("the write-time embedding's exact input is %d bytes of keyed text: the write path embeds embedText(key, body), not the bare body", est.EmbedInputBytes),
				fmt.Sprintf("the %s stage adds similar_to links and predicts no model call of its own: this store has an embedder wired, so the writer already embedded each changed chunk at write time, and only a failed write-time embedding would be re-embedded",
					stages[embedStage].Name))
		} else {
			ex = append(ex,
				fmt.Sprintf("no embedder is wired on the store this preview ran against, so the writer would store every chunk unembedded: the %d embedding call(s) are predicted for the %s stage instead, which runs when the configured server drains the outbox",
					est.Changed, stages[embedStage].Name),
				fmt.Sprintf("that stage's exact input is the %d stored body bytes, not the %d keyed bytes a write-time embedding would send: memory.enrichKey backfills from facts[self].Body alone, so the two shapes are priced separately",
					est.InputBytes, est.EmbedInputBytes))
		}
	} else if !est.WriterEmbeds {
		ex = append(ex, "no embedder is wired on the store this preview ran against and the configured stages include no embed_link stage: no embedding is predicted at all")
	}
	if entityStage >= 0 {
		st := stages[entityStage]
		if st.BatchKeys > 1 {
			ex = append(ex, fmt.Sprintf(
				"the %s stage batches up to %d changed chunks per model call, so its call count is lower than its run count", st.Name, st.BatchKeys))
		}
		ex = append(ex,
			fmt.Sprintf("%s completion tokens are a heuristic (max(%d, %d%% of each chunk's estimated tokens)), not a measurement",
				st.Name, minEntityOutputTokensPerChunk, int(entityOutputTokenRatio*100)),
			"the extraction prompt scaffolding (system prompt and response schema) is not counted; the estimated prompt tokens cover the chunk bodies only",
			"entity merge proposal discovery (memory.ProposeEntityMerges) runs after enrichment, is a pure read and makes no model calls; it is not counted here")
	}
	for i, st := range stages {
		if i != embedStage && i != entityStage {
			ex = append(ex, fmt.Sprintf(
				"stage %q is not one of the pipeline's known stages (%s, %s): predicted as one call per changed chunk over the same chunk bodies, with no completion estimate",
				st.Name, memory.StageEmbedLink, memory.StageEntities))
		}
		if st.Model == "" && est.Stages[i].Cost.Status == cost.CostUnknown {
			ex = append(ex, fmt.Sprintf(
				"cost unknown for stage %s: the configured dependency reports no model ID, so no price entry can be looked up", st.Name))
		}
	}
	if est.Embedding != nil && est.Embedding.Model == "" && est.Embedding.Cost.Status == cost.CostUnknown {
		ex = append(ex, "cost unknown for the write-time embedding: the configured embedder reports no model ID, so no price entry can be looked up")
	}
	switch est.Cost.Status {
	case cost.CostUnknown:
		if len(est.Cost.Unpriced) > 0 {
			ex = append(ex, fmt.Sprintf(
				"cost unknown: the price table has no entry for %s; the reported total covers only the priced workloads and is not a zero",
				strings.Join(est.Cost.Unpriced, ", ")))
		} else {
			ex = append(ex, "cost unknown: no price table was supplied, so nothing was rated (an unknown cost is never reported as zero)")
		}
	case cost.CostPricedZero:
		ex = append(ex, "cost priced at zero by the price table (a local or free model, or no billable tokens): a real zero, distinct from an unknown price")
	case cost.CostRoundedZero:
		ex = append(ex, "cost rated at a nonzero unit price but below one microUSD for this workload, so the integer figure floors to zero: this run is not free, it is below the resolution of the reported number")
	case cost.CostPriced:
		ex = append(ex, "cost is approximate: the sum of the estimated priced workloads, rated locally per estimated token against the configured price table, never quoted by the provider - not an invoice and not a bound on the real figure")
	}
	return ex
}
