// Package membench is a deterministic recall/MRR regression harness for
// punk's own memory retrieval. It is deliberately NOT LongMemEval: no
// dataset download, no LLM judge — just JSONL facts + queries scored
// against Store.HybridSearch. A LongMemEval adapter can convert into this
// format later.
package membench

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/hypervisor-io/punk-records/internal/memory"
)

// Record is one line of a membench JSONL scenario: either a fact to
// ingest or a query to score against the facts already ingested.
type Record struct {
	Type   string   `json:"type"` // "fact" | "query"
	Key    string   `json:"key,omitempty"`
	Body   string   `json:"body,omitempty"`
	Q      string   `json:"q,omitempty"`
	Expect []string `json:"expect,omitempty"` // keys that answer the query
}

// Result is the aggregate score over every query record in a scenario.
type Result struct {
	Queries   int     `json:"queries"`
	RecallAtK float64 `json:"recall_at_k"`
	MRR       float64 `json:"mrr"`
}

// Load reads a JSONL scenario file, skipping blank lines.
func Load(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var recs []Record
	scanner := bufio.NewScanner(f)
	line := 0
	for scanner.Scan() {
		line++
		text := scanner.Text()
		if len(text) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal([]byte(text), &r); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		recs = append(recs, r)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return recs, nil
}

// Run ingests every fact record, then scores each query record against
// Store.HybridSearch: a hit if any expected key is in the top-k, MRR is
// 1/rank of the first expected key found (0 if absent). rerank routes
// queries through HybridSearchReranked instead — the acceptance harness
// for measuring a configured cross-encoder's recall@k delta.
func Run(ctx context.Context, s *memory.Store, ns string, recs []Record, k int, rerank bool) (Result, error) {
	if k <= 0 {
		k = 5
	}
	var res Result
	for _, r := range recs {
		if r.Type != "fact" {
			continue
		}
		if _, err := s.Write(ctx, memory.WriteInput{Namespace: ns, Key: r.Key, Body: r.Body, Writer: "membench"}); err != nil {
			return res, fmt.Errorf("ingest %s: %w", r.Key, err)
		}
	}
	var recallHits, mrrSum float64
	for _, r := range recs {
		if r.Type != "query" {
			continue
		}
		res.Queries++
		var facts []memory.Fact
		if rerank {
			scored, err := s.HybridSearchReranked(ctx, ns, r.Q, k, 0)
			if err != nil {
				return res, fmt.Errorf("query %q: %w", r.Q, err)
			}
			facts = make([]memory.Fact, len(scored))
			for i, sf := range scored {
				facts[i] = sf.Fact
			}
		} else {
			var err error
			facts, err = s.HybridSearch(ctx, ns, r.Q, k, 0)
			if err != nil {
				return res, fmt.Errorf("query %q: %w", r.Q, err)
			}
		}
		expect := map[string]bool{}
		for _, e := range r.Expect {
			expect[e] = true
		}
		for rank, f := range facts {
			if expect[f.Key] {
				recallHits++
				mrrSum += 1.0 / float64(rank+1)
				break
			}
		}
	}
	if res.Queries > 0 {
		res.RecallAtK = recallHits / float64(res.Queries)
		res.MRR = mrrSum / float64(res.Queries)
	}
	return res, nil
}
