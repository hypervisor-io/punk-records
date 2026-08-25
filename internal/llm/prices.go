package llm

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"time"
)

// PriceTable rates model usage locally: per-request cost attribution
// from vendor APIs is impossible (1-minute usage buckets, daily cost
// buckets), so we rate every response ourselves and reconcile against
// vendor cost APIs out of band (GAPS2 C2#4).
type PriceTable struct {
	AsOf   string       `json:"as_of"` // YYYY-MM-DD; staleness guard
	Models []ModelPrice `json:"models"`
}

type ModelPrice struct {
	Match           string `json:"match"`             // glob on model id
	InputPerMTokUSD int64  `json:"input_per_mtok_us"` // microUSD per 1M input tokens... unit: microUSD
	OutputPerMTok   int64  `json:"output_per_mtok_us"`
}

// defaultPrices ships a conservative table; deployments override with
// budgets.price_table_path. Values are microUSD per 1M tokens.
var defaultPrices = PriceTable{
	AsOf: "2026-07-06",
	Models: []ModelPrice{
		{Match: "gpt-5*", InputPerMTokUSD: 1_250_000, OutputPerMTok: 10_000_000},
		{Match: "claude-*", InputPerMTokUSD: 3_000_000, OutputPerMTok: 15_000_000},
		{Match: "llama*", InputPerMTokUSD: 0, OutputPerMTok: 0},
		{Match: "qwen*", InputPerMTokUSD: 0, OutputPerMTok: 0},
		{Match: "*", InputPerMTokUSD: 1_000_000, OutputPerMTok: 4_000_000},
	},
}

// LoadPrices reads a table from disk, or returns the shipped default.
func LoadPrices(pathOverride string) (*PriceTable, error) {
	if pathOverride == "" {
		p := defaultPrices
		return &p, nil
	}
	raw, err := os.ReadFile(pathOverride)
	if err != nil {
		return nil, err
	}
	var t PriceTable
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("price table: %w", err)
	}
	return &t, nil
}

// Stale reports whether the table is older than maxAge.
func (t *PriceTable) Stale(now time.Time, maxAge time.Duration) bool {
	asOf, err := time.Parse("2006-01-02", t.AsOf)
	if err != nil {
		return true
	}
	return now.Sub(asOf) > maxAge
}

// Rate returns the cost in microUSD for one call. First matching glob
// wins; a table always ends with "*" so ok=false means an empty table.
func (t *PriceTable) Rate(model string, promptTokens, completionTokens int) (int64, bool) {
	for _, m := range t.Models {
		if ok, _ := path.Match(m.Match, model); ok {
			in := int64(promptTokens) * m.InputPerMTokUSD / 1_000_000
			out := int64(completionTokens) * m.OutputPerMTok / 1_000_000
			return in + out, true
		}
	}
	return 0, false
}
