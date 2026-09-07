package cost

import (
	"testing"

	"github.com/hypervisor-io/punk-records/internal/llm"
)

// The P02 dry-run rating proofs: a predicted workload whose price is
// missing is reported unknown - never as a confident zero - while a price
// that really is zero stays a distinct priced_zero, and a prediction with
// no model call at all is an exact zero (no_work).
//
// Mechanism adapted from Cognee's cognee/modules/cognify/estimator.py
// (commit 78ff576559a7f75f65884c5bd90b22cdc790016e), which rates each
// predicted stage locally and warns "Cost unavailable: no pricing entry
// for model X" rather than reporting $0.

func TestRateOneMissingPriceUnknown(t *testing.T) {
	if got := RateOne(nil, "gpt-5-mini", 1, 1000, 200); got.Status != CostUnknown || got.MicroUSD != 0 {
		t.Fatalf("nil table = %+v, want unknown at zero", got)
	}
	empty := &llm.PriceTable{AsOf: "2026-09-07"}
	if got := RateOne(empty, "gpt-5-mini", 1, 1000, 200); got.Status != CostUnknown {
		t.Fatalf("table with no matching entry = %+v, want unknown", got)
	}
	catchAll := &llm.PriceTable{Models: []llm.ModelPrice{
		{Match: "*", InputPerMTokUSD: 1_000_000, OutputPerMTok: 4_000_000}}}
	if got := RateOne(catchAll, "", 1, 1000, 200); got.Status != CostUnknown {
		t.Fatalf("unnamed model = %+v, want unknown: a catch-all must not price a model we cannot name", got)
	}
	if got := RateOne(catchAll, "gpt-5-mini", 1, 1_000_000, 500_000); got.Status != CostPriced ||
		got.MicroUSD != 1_000_000+2_000_000 {
		t.Fatalf("priced workload = %+v, want 3 USD in microUSD", got)
	}
	free := &llm.PriceTable{Models: []llm.ModelPrice{{Match: "qwen*", InputPerMTokUSD: 0, OutputPerMTok: 0}}}
	if got := RateOne(free, "qwen3-30b", 1, 1000, 200); got.Status != CostPricedZero || got.MicroUSD != 0 {
		t.Fatalf("zero-priced model = %+v, want priced_zero at zero", got)
	}
	if got := RateOne(nil, "gpt-5-mini", 0, 0, 0); got.Status != CostNoWork || got.MicroUSD != 0 {
		t.Fatalf("no predicted call = %+v, want no_work: zero work is exact, not a pricing gap", got)
	}
}

func TestRatePreviewTotals(t *testing.T) {
	prices, err := llm.LoadPrices("")
	if err != nil {
		t.Fatal(err)
	}
	usage := []PreviewUsage{
		{Stage: "entities", Model: "gpt-5-mini", Calls: 2, PromptTokens: 1000, CompletionTokens: 200},
		{Stage: "write_embedding", Model: "nomic-embed-text", Calls: 6, PromptTokens: 900},
	}

	got := RatePreview(prices, usage)
	if got.Status != CostPriced || got.MicroUSD <= 0 {
		t.Fatalf("shipped table = %+v, want a nonzero priced total", got)
	}
	if len(got.Costs) != 2 || got.Costs[0].Stage != "entities" || got.Costs[1].Stage != "write_embedding" {
		t.Fatalf("per-workload costs = %+v, want one entry per workload in usage order", got.Costs)
	}
	sum := got.Costs[0].MicroUSD + got.Costs[1].MicroUSD
	if sum != got.MicroUSD {
		t.Fatalf("total = %d, want the sum of the workloads %d", got.MicroUSD, sum)
	}

	// one unpriceable workload makes the whole total unknown, while the
	// rated part stays visible
	partial := &llm.PriceTable{Models: []llm.ModelPrice{
		{Match: "gpt-5*", InputPerMTokUSD: 1_250_000, OutputPerMTok: 10_000_000}}}
	mixed := RatePreview(partial, usage)
	if mixed.Status != CostUnknown {
		t.Fatalf("mixed table = %+v, want unknown: a missing price is never zero-cost certainty", mixed)
	}
	if len(mixed.Unpriced) != 1 || mixed.Unpriced[0] != "nomic-embed-text" {
		t.Fatalf("unpriced models = %v, want [nomic-embed-text]", mixed.Unpriced)
	}
	if mixed.MicroUSD != mixed.Costs[0].MicroUSD || mixed.MicroUSD <= 0 {
		t.Fatalf("mixed total = %+v, want the rated workload's cost with an unknown status", mixed)
	}

	// no predicted work at all: an exact zero, not an unknown
	if none := RatePreview(nil, nil); none.Status != CostNoWork || none.MicroUSD != 0 {
		t.Fatalf("empty usage = %+v, want no_work at zero", none)
	}
	idle := RatePreview(nil, []PreviewUsage{{Stage: "entities", Model: "gpt-5-mini", Calls: 0}})
	if idle.Status != CostNoWork || len(idle.Costs) != 1 || idle.Costs[0].Status != CostNoWork {
		t.Fatalf("idle stage = %+v, want no_work", idle)
	}

	// a zero-priced deployment is priced_zero, distinct from both
	free := RatePreview(&llm.PriceTable{Models: []llm.ModelPrice{
		{Match: "*", InputPerMTokUSD: 0, OutputPerMTok: 0}}}, usage)
	if free.Status != CostPricedZero || free.MicroUSD != 0 || len(free.Unpriced) != 0 {
		t.Fatalf("zero-priced deployment = %+v, want priced_zero at zero with nothing unpriced", free)
	}
}

// TestRateOneRoundedPaidWorkIsNotFree: llm.PriceTable.Rate truncates to
// whole microUSD per million tokens, so a NONZERO unit price on a small
// workload lands at 0 microUSD. That work is not free. Folded from the
// reviewer's reproduced /tmp/punk-P02-review-rounding_test.go.
func TestRateOneRoundedPaidWorkIsNotFree(t *testing.T) {
	prices := &llm.PriceTable{Models: []llm.ModelPrice{{Match: "tiny", InputPerMTokUSD: 20000, OutputPerMTok: 20000}}}
	got := RateOne(prices, "tiny", 1, 1, 0)
	if got.Status == CostPricedZero {
		t.Fatalf("positive unit price labeled free because cost rounded to %d microUSD", got.MicroUSD)
	}
	if got.Status != CostRoundedZero || got.MicroUSD != 0 {
		t.Fatalf("rounded workload = %+v, want %s at zero", got, CostRoundedZero)
	}

	// a genuinely zero unit rate stays free
	free := &llm.PriceTable{Models: []llm.ModelPrice{{Match: "tiny", InputPerMTokUSD: 0, OutputPerMTok: 0}}}
	if got := RateOne(free, "tiny", 1, 1000, 100); got.Status != CostPricedZero {
		t.Fatalf("zero unit rate = %+v, want priced_zero", got)
	}
	// a nonzero rate with no billable tokens is exactly zero, not rounded
	outOnly := &llm.PriceTable{Models: []llm.ModelPrice{{Match: "tiny", InputPerMTokUSD: 0, OutputPerMTok: 20000}}}
	if got := RateOne(outOnly, "tiny", 1, 500, 0); got.Status != CostPricedZero {
		t.Fatalf("no billable tokens = %+v, want priced_zero: nothing was rounded away", got)
	}
	// the same unit price at scale rates normally
	if got := RateOne(prices, "tiny", 1, 1_000_000, 0); got.Status != CostPriced || got.MicroUSD != 20000 {
		t.Fatalf("one million tokens = %+v, want priced at 20000 microUSD", got)
	}
	// a mixed rate floors on the billed kind only
	mixed := &llm.PriceTable{Models: []llm.ModelPrice{{Match: "tiny", InputPerMTokUSD: 20000, OutputPerMTok: 0}}}
	if got := RateOne(mixed, "tiny", 1, 10, 1000); got.Status != CostRoundedZero {
		t.Fatalf("small paid input with free output = %+v, want %s", got, CostRoundedZero)
	}
}

// TestRatePreviewRoundedTotalIsNotFree: a total whose workloads all priced
// below microUSD resolution reports the rounding rather than a free run,
// and any workload that rates nonzero outranks it.
func TestRatePreviewRoundedTotalIsNotFree(t *testing.T) {
	prices := &llm.PriceTable{Models: []llm.ModelPrice{{Match: "*", InputPerMTokUSD: 20000, OutputPerMTok: 20000}}}
	got := RatePreview(prices, []PreviewUsage{{Stage: "entities", Model: "tiny", Calls: 1, PromptTokens: 3}})
	if got.Status != CostRoundedZero || got.MicroUSD != 0 {
		t.Fatalf("tiny priced total = %+v, want %s at zero", got, CostRoundedZero)
	}
	scaled := RatePreview(prices, []PreviewUsage{
		{Stage: "entities", Model: "tiny", Calls: 1, PromptTokens: 3},
		{Stage: "entities", Model: "tiny", Calls: 1, PromptTokens: 1_000_000}})
	if scaled.Status != CostPriced || scaled.MicroUSD != 20000 {
		t.Fatalf("mixed-scale total = %+v, want priced at 20000 microUSD", scaled)
	}
}
