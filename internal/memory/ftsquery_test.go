package memory

import (
	"context"
	"fmt"
	"testing"
)

func TestSanitizeFTS(t *testing.T) {
	cases := []struct{ in, want string }{
		{"When did Caroline go to the group?", `"caroline" OR "group"`},
		// The stray "s" is the intended tokenizer artifact, not a bug:
		// FTS5's unicode61 tokenizer splits "what's" into "what" + "s" on
		// both the index side and the query side (see the ftsToken doc
		// comment), so "s" is a real, matchable content token here. Do not
		// "fix" this case by re-merging the apostrophe.
		{"what's broken", `"s" OR "broken"`},
		// All-stopword query: no content tokens survive, but the
		// unfiltered token set is non-empty, so the fallback rule (finding
		// 3) uses the unfiltered tokens instead of returning "".
		{"the a is", `"the" OR "a" OR "is"`},
		// Single stopword query: same fallback -- "go" alone would
		// otherwise silently sanitize to "", making Search(ns, "go")
		// return empty with no error for a perfectly legitimate query.
		{"go", `"go"`},
		{"", ""},
		// No tokens at all (punctuation only): fallback has nothing to
		// fall back to, so this must still be "".
		{"???", ""},
		{"pool-exhaustion!!", `"pool" OR "exhaustion"`},
		// Unicode-aware tokenization: query-side tokenization must match
		// FTS5's unicode61 index-side tokenizer, which is not ASCII-only.
		{"日本語 メモ", `"日本語" OR "メモ"`},
		{"café", `"café"`},
	}
	for _, c := range cases {
		if got := SanitizeFTS(c.in); got != c.want {
			t.Errorf("SanitizeFTS(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSearchPunctuatedQuery(t *testing.T) {
	s := newTestStore(t) // existing helper in memory_test.go
	ctx := context.Background()
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/a", Body: "connection pool exhausted", Writer: "t"}); err != nil {
		t.Fatal(err)
	}
	facts, err := s.Search(ctx, "ns", "what's wrong with the pool?", 10)
	if err != nil {
		t.Fatalf("punctuated query must not error: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("want 1 hit, got %d", len(facts))
	}
	// "the ??? a" sanitizes to `"the" OR "a"` under the fallback rule
	// (finding 3: both "the" and "a" are stopwords, but with nothing else
	// surviving, the fallback keeps them rather than returning ""). The
	// stored fact body "connection pool exhausted" contains neither token,
	// so the expected result is still empty -- this is "no body matches",
	// not "sanitizer returns empty query".
	facts, err = s.Search(ctx, "ns", "the ??? a", 10)
	if err != nil || len(facts) != 0 {
		t.Fatalf("stopword-only: want empty no error, got %v %v", facts, err)
	}
}

// TestSearchRankOrdering is the regression guard for the missing ORDER BY
// on the sqlite FTS query: without it, FTS5 returns OR-widened matches in
// rowid (insertion) order, not relevance order, so LIMIT truncates to the
// oldest matches of any single common term and an exact multi-token match
// can be pushed out of the result set entirely. score.go's RRF fusion uses
// each hit's slice position as its rank, so this ordering is load-bearing,
// not cosmetic.
func TestSearchRankOrdering(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Five distractors, each sharing exactly ONE of the three query tokens
	// ("swimming"/"pool"/"schedule") -- written FIRST, so under the old
	// rowid-ordered code they occupy every slot in a LIMIT-3 result.
	distractors := []string{
		"swimming lessons start Monday",
		"pool cleaning notice posted",
		"schedule a dentist appointment",
		"community center swimming lanes",
		"pool hours extended this summer",
	}
	for i, body := range distractors {
		if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: fmt.Sprintf("/distractor/%d", i), Body: body, Writer: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	// Written LAST (highest rowid), so it only surfaces at all if ranking
	// -- not insertion order -- drives result selection and ordering. It
	// matches all three query tokens and is kept short (bm25's length
	// normalization otherwise favors short single-term distractors enough
	// to outweigh matching more distinct terms; verified empirically
	// against this schema).
	const exactKey = "/exact"
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: exactKey, Body: "swimming pool schedule", Writer: "t"}); err != nil {
		t.Fatal(err)
	}

	facts, err := s.Search(ctx, "ns", "swimming pool schedule", 3)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(facts) == 0 {
		t.Fatal("want non-empty results")
	}
	if facts[0].Key != exactKey {
		t.Fatalf("top hit = %s, want %s (best bm25 match ranked first, not oldest-by-rowid)", facts[0].Key, exactKey)
	}
}
