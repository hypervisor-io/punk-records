package cost

import (
	"github.com/hypervisor-io/punk-records/internal/llm"
)

// Dry-run cost rating (task P02): a preview rates the model work it
// predicts against the same local price table the call ledger uses, and
// reports a MISSING price as unknown instead of reporting a confident
// zero. Four states are kept apart on purpose:
//
//   - priced: the table matched the model and the cost is nonzero.
//   - priced_zero: the table matched and the unit price really is zero (a
//     local model), or the workload has no billable tokens at all. A
//     deployment that pays nothing is not a deployment whose price we do
//     not know.
//   - priced_rounded_zero: the table matched at a NONZERO unit price, but
//     the workload is small enough that llm.PriceTable.Rate - which
//     truncates to whole microUSD per million tokens - floors it to zero.
//     That work is not free; it is below the resolution of the figure.
//   - unknown: no table, no matching entry, or no model ID at all. Never
//     presented as a zero cost.
//
// A fifth state, no_work, covers a prediction with no model call in it:
// there the zero is exact, not a pricing gap.
//
// Mechanism adapted from Cognee's cognee/modules/cognify/estimator.py
// (commit 78ff576559a7f75f65884c5bd90b22cdc790016e), which rates each
// predicted stage locally and appends the warning "Cost unavailable: no
// pricing entry for model X" when a predicted workload totals $0 - never
// reporting zero-cost certainty. Reimplemented in Go on punk's own
// llm.PriceTable, which rates in microUSD and answers ok=false when no
// entry matches.
const (
	CostPriced      = "priced"
	CostPricedZero  = "priced_zero"
	CostRoundedZero = "priced_rounded_zero"
	CostUnknown     = "unknown"
	CostNoWork      = "no_work"
)

// Rating is one workload's cost and whether that cost is known.
type Rating struct {
	MicroUSD int64  `json:"micro_usd"`
	Status   string `json:"status"`
}

// PreviewUsage is one predicted model workload: the stage that would run
// it, the model it would call, and the token counts the prediction
// estimated for it. Both counts are estimates - punk counts tokens as
// bytes/4 rather than running the model's tokenizer, and completion
// tokens are a heuristic - so what comes back is a rating of an estimate,
// never an invoice.
type PreviewUsage struct {
	Stage            string `json:"stage"`
	Model            string `json:"model"`
	Calls            int    `json:"calls"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
}

// PreviewCost is one rated workload, in the order it was predicted.
type PreviewCost struct {
	Stage string `json:"stage"`
	Model string `json:"model,omitempty"`
	Calls int    `json:"calls"`
	Rating
}

// Preview is the rated total of a prediction: the sum of the workloads a
// price entry covered, the honesty state of that total, and the models no
// entry covered.
type Preview struct {
	// MicroUSD sums the estimated priced workloads. It is neither an
	// invoice nor a guaranteed bound on the real figure: the token counts
	// behind it are themselves estimates (bytes/4 input, heuristic
	// output), so a real run can land above or below it. When Status is
	// unknown the sum also covers only the workloads that had a price, so
	// it is not the whole predicted bill either.
	MicroUSD int64         `json:"micro_usd"`
	Status   string        `json:"status"`
	Unpriced []string      `json:"unpriced_models,omitempty"`
	Costs    []PreviewCost `json:"costs,omitempty"`
}

// Rating views the total as a single rating, so a caller can render a
// whole prediction with the same honesty state it renders one workload
// with.
func (p Preview) Rating() Rating {
	return Rating{MicroUSD: p.MicroUSD, Status: p.Status}
}

// unitRates asks a table what it charges per million tokens of each kind.
// Rating exactly one million tokens of one kind returns that kind's unit
// price, because Rate is integer microUSD per million: this reads the
// table's own numbers through its public API instead of adding an
// accessor, and it is what tells a genuinely free model from a paid one
// whose small workload floored to zero.
func unitRates(prices *llm.PriceTable, model string) (in, out int64, ok bool) {
	in, ok = prices.Rate(model, 1_000_000, 0)
	if !ok {
		return 0, 0, false
	}
	out, ok = prices.Rate(model, 0, 1_000_000)
	if !ok {
		return 0, 0, false
	}
	return in, out, true
}

// RateOne rates a single predicted workload. An empty model ID is
// unknown, not cheap: llm.PriceTable always ends with a "*" catch-all, so
// rating an unnamed model would invent a price for a model the
// configuration never reported. A workload that rates to zero is then
// split by its unit price: zero rates are free, nonzero rates that floored
// are priced_rounded_zero, because truncation is not a discount.
func RateOne(prices *llm.PriceTable, model string, calls, promptTokens, completionTokens int) Rating {
	if calls <= 0 && promptTokens <= 0 && completionTokens <= 0 {
		return Rating{Status: CostNoWork}
	}
	if prices == nil || model == "" {
		return Rating{Status: CostUnknown}
	}
	micro, ok := prices.Rate(model, promptTokens, completionTokens)
	if !ok {
		return Rating{Status: CostUnknown}
	}
	if micro > 0 {
		return Rating{MicroUSD: micro, Status: CostPriced}
	}
	in, out, ok := unitRates(prices, model)
	if !ok {
		return Rating{Status: CostUnknown}
	}
	if int64(promptTokens)*in+int64(completionTokens)*out == 0 {
		return Rating{Status: CostPricedZero}
	}
	return Rating{Status: CostRoundedZero}
}

// RatePreview rates every predicted workload in order and totals them.
// Costs has one entry per usage entry, in the same order. The total is
// unknown if any workload is unknown: a single missing price means the
// prediction cannot promise a figure, however large the rated part is. A
// total of zero is reported as free only when every priced workload
// really rated at zero; if any of them floored from a nonzero unit price,
// the total says so instead of claiming a free run.
func RatePreview(prices *llm.PriceTable, usage []PreviewUsage) Preview {
	out := Preview{Costs: make([]PreviewCost, 0, len(usage))}
	unknown, priced, rounded, pricedZero := false, false, false, false
	seen := map[string]bool{}
	for _, u := range usage {
		r := RateOne(prices, u.Model, u.Calls, u.PromptTokens, u.CompletionTokens)
		out.Costs = append(out.Costs, PreviewCost{Stage: u.Stage, Model: u.Model, Calls: u.Calls, Rating: r})
		switch r.Status {
		case CostUnknown:
			unknown = true
			if u.Model != "" && !seen[u.Model] {
				seen[u.Model] = true
				out.Unpriced = append(out.Unpriced, u.Model)
			}
		case CostPriced:
			priced = true
			out.MicroUSD += r.MicroUSD
		case CostRoundedZero:
			rounded = true
		case CostPricedZero:
			pricedZero = true
		}
	}
	switch {
	case unknown:
		out.Status = CostUnknown
	case priced:
		out.Status = CostPriced
	case rounded:
		out.Status = CostRoundedZero
	case pricedZero:
		out.Status = CostPricedZero
	default:
		out.Status = CostNoWork
	}
	return out
}
