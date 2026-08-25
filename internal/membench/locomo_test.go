package membench

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hypervisor-io/punk-records/internal/memory"
)

// TestLoCoMoQuerySanitize checks that a real LoCoMo question with FTS5-
// hostile punctuation ("?") sanitizes to a MATCH-safe string carrying the
// content words OR-joined (the fair keyword-retrieval baseline -- FTS5's
// implicit AND over every raw word would require a single turn to
// contain the entire question), with stopwords and question words
// dropped, and that punctuation-only/all-stopword/empty input sanitizes
// to "" (RunLoCoMo's signal to skip the query rather than send an empty
// MATCH, itself a syntax error). sanitizeFTS itself now lives in
// internal/memory (memory.SanitizeFTS) so production Search can share it;
// this test stays here as RunLoCoMo's regression guard on that dependency.
func TestLoCoMoQuerySanitize(t *testing.T) {
	got := memory.SanitizeFTS("When did Caroline go to the LGBTQ support group?")
	if strings.ContainsAny(got, "?:") {
		t.Fatalf("SanitizeFTS(...) = %q, want no ? or : characters", got)
	}
	want := `"caroline" OR "lgbtq" OR "support" OR "group"`
	if got != want {
		t.Fatalf("SanitizeFTS(...) = %q, want %q", got, want)
	}
	for _, dropped := range []string{"when", "did", "the", "go", "to"} {
		if strings.Contains(got, `"`+dropped+`"`) {
			t.Fatalf("SanitizeFTS(...) = %q, want stopword %q dropped", got, dropped)
		}
	}

	for _, in := range []string{"???", "", "   ", "::--"} {
		if got := memory.SanitizeFTS(in); got != "" {
			t.Fatalf("SanitizeFTS(%q) = %q, want empty (no tokens at all)", in, got)
		}
	}
	// All-stopword-but-nonempty input ("When is the What who") no longer
	// sanitizes to "": production sqlite Search (memory.go) now falls
	// back to the unfiltered token set when stopword filtering would
	// leave zero tokens, so a legitimate all-stopword query still
	// searches instead of silently matching nothing. RunLoCoMo callers
	// only skip on a truly empty sanitized query (see the "" case above).
	for _, in := range []string{"When is the What who", "Who did what to whom"} {
		if got := memory.SanitizeFTS(in); got == "" {
			t.Fatalf("SanitizeFTS(%q) = %q, want non-empty fallback (all-stopword input still has tokens)", in, got)
		}
	}
}

// TestRunLoCoMoAllStopwordQuestionNotDoubleSanitized guards against
// RunLoCoMo pre-sanitizing qa.Question with memory.SanitizeFTS before
// handing it to HybridSearch: production sqlite Search (memory.go) now
// sanitizes internally, so a pre-sanitized query gets tokenized a SECOND
// time. On an all-stopword question ("Who did what to whom"), pass 1's
// stopword-fallback emits `"who" OR "did" OR "what" OR "to" OR "whom"`;
// pass 2 tokenizes that literal string and its own " OR " separators
// become tokens too, so the query grows spurious `"or"` clauses that
// weren't in the original question at all.
//
// D1:1 is the real evidence turn: it shares stopword tokens ("to",
// "whom") with the all-stopword-fallback query, so it matches under
// either single- or double-sanitization. D1:2 is a decoy that shares NO
// content with the question but is packed with the standalone token
// "or" -- it only matches once double-sanitization has injected spurious
// "or" OR-clauses, and being short and or-dense it outscores D1:1 on
// bm25, pushing D1:1 out of the k=1 result. So HitAtK is 1.0 only when
// the question reaches Search exactly once (RunLoCoMo passes the raw
// question straight through); it drops to 0 the moment a redundant
// memory.SanitizeFTS call in locomo.go double-wraps it.
func TestRunLoCoMoAllStopwordQuestionNotDoubleSanitized(t *testing.T) {
	dataset := []map[string]any{
		{
			"sample_id": "convA",
			"conversation": map[string]any{
				"session_1": []map[string]string{
					{"speaker": "Alice", "dia_id": "D1:1", "text": "I don't know to whom the package belongs."},
					{"speaker": "Bob", "dia_id": "D1:2", "text": "or or or or"},
				},
			},
			"qa": []map[string]any{
				{"question": "Who did what to whom", "evidence": []string{"D1:1"}, "category": 1},
			},
		},
	}
	raw, err := json.Marshal(dataset)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "locomo.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	s := newBenchStore(t)
	res, err := RunLoCoMo(t.Context(), s, path, 1)
	if err != nil {
		t.Fatal(err)
	}
	if res.Questions != 1 {
		t.Fatalf("Questions = %d, want 1", res.Questions)
	}
	if res.HitAtK != 1.0 {
		t.Fatalf("HitAtK = %v, want 1.0 (D1:1 must win top-1 over the or-decoy D1:2; a HitAtK of 0 means the question was double-sanitized and the decoy's spurious \"or\" tokens won)", res.HitAtK)
	}
}

// TestLoCoMoParse checks that the real locomo10.json shape decodes: turns
// live under dynamically-named "session_N" keys (siblings of
// "session_N_date_time", speaker_a, speaker_b), and QA carries evidence
// dia_ids + a category int.
func TestLoCoMoParse(t *testing.T) {
	const raw = `[
		{
			"sample_id": "convA",
			"conversation": {
				"speaker_a": "Alice",
				"speaker_b": "Bob",
				"session_1_date_time": "1:00 pm on July 1, 2023",
				"session_1": [
					{"speaker": "Alice", "dia_id": "D1:1", "text": "My favorite hobby is painting landscapes."},
					{"speaker": "Bob", "dia_id": "D1:2", "text": "That sounds relaxing."}
				]
			},
			"qa": [
				{"question": "Alice favorite hobby painting", "answer": "painting", "evidence": ["D1:1"], "category": 1},
				{"question": "unanswerable, no evidence", "category": 5}
			]
		}
	]`
	var convs []locomoConversation
	if err := json.Unmarshal([]byte(raw), &convs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(convs) != 1 {
		t.Fatalf("len(convs) = %d, want 1", len(convs))
	}
	c := convs[0]
	if c.SampleID != "convA" {
		t.Fatalf("SampleID = %q, want convA", c.SampleID)
	}
	turns, err := c.turns()
	if err != nil {
		t.Fatalf("turns: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("len(turns) = %d, want 2 (session_1_date_time must be skipped)", len(turns))
	}
	if len(c.QA) != 2 {
		t.Fatalf("len(QA) = %d, want 2", len(c.QA))
	}
	if got := c.QA[0].Evidence; len(got) != 1 || got[0] != "D1:1" {
		t.Fatalf("QA[0].Evidence = %v, want [D1:1]", got)
	}
	if c.QA[0].Category != 1 {
		t.Fatalf("QA[0].Category = %d, want 1", c.QA[0].Category)
	}
	if len(c.QA[1].Evidence) != 0 {
		t.Fatalf("QA[1].Evidence = %v, want empty (adversarial QA)", c.QA[1].Evidence)
	}
}

// TestRunLoCoMo verifies conversation isolation as well as scoring.
// convB's turn uses a dia_id ("D2:1") distinct from convA's ("D1:1") so a
// shared-namespace bug wouldn't just overwrite-and-hide convA's turn under
// a colliding key (RunLoCoMo's loop scores each conversation right after
// its own ingest, so a same-key overwrite is invisible to convA, which is
// always scored first — that made an earlier version of this fixture,
// which reused "D1:1" for both convs, pass identically whether isolation
// worked or was completely broken).
//
// convB's question is an exact copy of convA's question, itself a
// verbatim subset of convA's evidence-turn body (FTS ANDs every query
// token, no stopword removal, guaranteeing a match). convB's *evidence*
// deliberately points at convA's key ("D1:1"), not its own turn — this is
// the isolation probe: under correct isolation convB's namespace never
// contains "/turn/D1:1" at all (it only holds "/turn/D2:1"), so the
// lookup structurally cannot hit. Under a shared-namespace bug, convA's
// still-live "/turn/D1:1" (distinct key => never overwritten) leaks into
// convB's search, its text matches convB's question, and its key matches
// convB's (deliberately cross-referenced) evidence => spurious hit. So
// HitAtK is 0.5 only when isolation holds, and rises to 1.0 the moment it
// doesn't — unlike the old fixture, this is a real regression guard
// (verified via mutation: hardcoding a shared namespace in RunLoCoMo
// flips this assertion from pass to fail).
func TestRunLoCoMo(t *testing.T) {
	dataset := []map[string]any{
		{
			"sample_id": "convA",
			"conversation": map[string]any{
				"session_1": []map[string]string{
					{"speaker": "Alice", "dia_id": "D1:1", "text": "My favorite hobby is painting landscapes."},
				},
			},
			"qa": []map[string]any{
				{"question": "Alice favorite hobby: painting?", "evidence": []string{"D1:1"}, "category": 1},
			},
		},
		{
			"sample_id": "convB",
			"conversation": map[string]any{
				"session_1": []map[string]string{
					{"speaker": "Carol", "dia_id": "D2:1", "text": "My favorite drink is coffee."},
				},
			},
			"qa": []map[string]any{
				// Same question text as convA (the textual trap), but
				// "evidence" is convA's key, not convB's own "D2:1" — the
				// isolation probe described above.
				{"question": "Alice favorite hobby: painting?", "evidence": []string{"D1:1"}, "category": 1},
			},
		},
	}
	raw, err := json.Marshal(dataset)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "locomo.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	s := newBenchStore(t)
	res, err := RunLoCoMo(t.Context(), s, path, 5)
	if err != nil {
		t.Fatal(err)
	}
	if res.Conversations != 2 {
		t.Fatalf("Conversations = %d, want 2", res.Conversations)
	}
	if res.Questions != 2 {
		t.Fatalf("Questions = %d, want 2", res.Questions)
	}
	// Exactly one of the two questions should hit: convA's (its own
	// evidence turn matches), not convB's (isolation holds).
	if res.HitAtK != 0.5 {
		t.Fatalf("HitAtK = %v, want 0.5 (convA hits, convB isolated-miss)", res.HitAtK)
	}
	if res.ByCategory[1] != 0.5 {
		t.Fatalf("ByCategory[1] = %v, want 0.5", res.ByCategory[1])
	}
}
