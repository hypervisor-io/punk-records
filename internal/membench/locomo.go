package membench

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/hypervisor-io/punk-records/internal/memory"
)

// locomoTurn is one dialog line inside a LoCoMo conversation's session_N
// array.
type locomoTurn struct {
	Speaker string `json:"speaker"`
	DiaID   string `json:"dia_id"`
	Text    string `json:"text"`
}

// locomoQA is one question over a conversation. evidence lists dia_ids;
// ~4 of ~1986 QA in locomo10.json have none (adversarial/unanswerable) and
// are skipped since there's nothing to score retrieval against.
type locomoQA struct {
	Question string   `json:"question"`
	Evidence []string `json:"evidence"`
	Category int      `json:"category"`
}

// locomoConversation is one entry of the top-level array. Conversation
// turns live under dynamically-named keys ("session_1", "session_2", ...)
// alongside sibling "session_N_date_time" strings and speaker_a/speaker_b
// fields, so it's decoded as raw messages and filtered in turns().
type locomoConversation struct {
	SampleID     string                     `json:"sample_id"`
	Conversation map[string]json.RawMessage `json:"conversation"`
	QA           []locomoQA                 `json:"qa"`
}

// turns extracts every session's dialog turns, in no particular
// cross-session order: dia_ids are unique within a conversation regardless
// of session, and this harness always calls HybridSearch with recency
// disabled (half-life 0), so ingestion order can't affect scoring.
func (c locomoConversation) turns() ([]locomoTurn, error) {
	var out []locomoTurn
	for key, raw := range c.Conversation {
		if !strings.HasPrefix(key, "session_") || strings.HasSuffix(key, "_date_time") {
			continue
		}
		var ts []locomoTurn
		if err := json.Unmarshal(raw, &ts); err != nil {
			return nil, fmt.Errorf("%s: %s: %w", c.SampleID, key, err)
		}
		out = append(out, ts...)
	}
	return out, nil
}

func loadLoCoMo(path string) ([]locomoConversation, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var convs []locomoConversation
	if err := json.Unmarshal(raw, &convs); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return convs, nil
}

// LoCoMoResult is the aggregate over all conversations, with a per-
// category breakdown. HitAtK = fraction of questions with >=1 evidence
// turn in the top-k; EvidenceRecall = mean fraction of a question's
// evidence turns retrieved in the top-k. This is RETRIEVAL recall over
// gold evidence dia_ids, not an answer-accuracy LoCoMo score.
type LoCoMoResult struct {
	Conversations  int             `json:"conversations"`
	Questions      int             `json:"questions"`
	HitAtK         float64         `json:"hit_at_k"`
	EvidenceRecall float64         `json:"evidence_recall"`
	MRR            float64         `json:"mrr"`
	ByCategory     map[int]float64 `json:"hit_at_k_by_category"`
}

// RunLoCoMo loads locomo10.json and, for EACH conversation, ingests its
// dialog turns into a fresh namespace ("loco-"+sample_id, key
// "/turn/"+dia_id, body "speaker: text") then scores every QA that has
// evidence by retrieving top-k via HybridSearch (which itself degrades to
// FTS+recency when the store has no embedder — same as Run). A fresh
// namespace per conversation is what keeps conversations isolated: each
// QA is only ever scored against its own conversation's namespace, so a
// turn from one conversation can never satisfy another's question.
func RunLoCoMo(ctx context.Context, s *memory.Store, path string, k int) (LoCoMoResult, error) {
	if k <= 0 {
		k = 5
	}
	convs, err := loadLoCoMo(path)
	if err != nil {
		return LoCoMoResult{}, err
	}

	var res LoCoMoResult
	var hitSum, evidenceRecallSum, mrrSum float64
	catHits := map[int]int{}
	catTotal := map[int]int{}

	for _, conv := range convs {
		res.Conversations++
		ns := "loco-" + conv.SampleID

		turns, err := conv.turns()
		if err != nil {
			return res, err
		}
		for _, t := range turns {
			if _, err := s.Write(ctx, memory.WriteInput{
				Namespace: ns,
				Key:       "/turn/" + t.DiaID,
				Body:      t.Speaker + ": " + t.Text,
				Writer:    "membench",
			}); err != nil {
				return res, fmt.Errorf("ingest %s %s: %w", conv.SampleID, t.DiaID, err)
			}
		}

		for _, qa := range conv.QA {
			if len(qa.Evidence) == 0 {
				continue
			}
			if strings.TrimSpace(qa.Question) == "" {
				continue
			}
			res.Questions++
			facts, err := s.HybridSearch(ctx, ns, qa.Question, k, 0)
			if err != nil {
				return res, fmt.Errorf("query %q: %w", qa.Question, err)
			}
			evidence := map[string]bool{}
			for _, e := range qa.Evidence {
				evidence["/turn/"+e] = true
			}
			retrieved := 0
			hit := false
			rank1 := 0
			for rank, f := range facts {
				if evidence[f.Key] {
					retrieved++
					if !hit {
						hit, rank1 = true, rank+1
					}
				}
			}
			if hit {
				hitSum++
				mrrSum += 1.0 / float64(rank1)
				catHits[qa.Category]++
			}
			evidenceRecallSum += float64(retrieved) / float64(len(qa.Evidence))
			catTotal[qa.Category]++
		}
	}

	if res.Questions > 0 {
		res.HitAtK = hitSum / float64(res.Questions)
		res.EvidenceRecall = evidenceRecallSum / float64(res.Questions)
		res.MRR = mrrSum / float64(res.Questions)
	}
	res.ByCategory = make(map[int]float64, len(catTotal))
	for cat, total := range catTotal {
		res.ByCategory[cat] = float64(catHits[cat]) / float64(total)
	}
	return res, nil
}
