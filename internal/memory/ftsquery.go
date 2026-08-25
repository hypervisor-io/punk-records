package memory

import (
	"regexp"
	"strings"
)

// ftsToken matches a run of alnum chars. Apostrophes are deliberately
// NOT kept inside tokens: FTS5's own unicode61 tokenizer splits on them
// too (both when it indexes a stored body and when it parses a quoted
// query string), so "Caroline's" already becomes two adjacent document
// tokens ("caroline", "s"). Quoting a merged "caroline's" query token
// would turn it into an FTS5 phrase requiring that exact adjacency,
// which is stricter than plain content matching and was observed to
// silently drop otherwise-correct hits; splitting on the apostrophe like
// the tokenizer does keeps the two independent AND terms it actually
// indexes.
var ftsToken = regexp.MustCompile(`[\p{L}\p{N}]+`)

// ftsStopwords is a compact closed-class English stopword set that gates
// production sqlite Search token filtering (memory.go), originally
// derived from the LoCoMo FTS baseline: dropping these before OR-joining
// the rest keeps BM25 ranking driven by content words instead of function
// words (the/is/what/...) that appear in nearly every turn and would
// otherwise dilute the score. SanitizeFTS falls back to the unfiltered
// token set when stopword removal would leave zero tokens, so a
// legitimate single-word query like "go" still searches instead of
// silently sanitizing to "". ponytail: hand-picked ~60-word list, not a
// full stopword corpus -- extend it if a category shows it's too thin.
var ftsStopwords = map[string]bool{
	"a": true, "an": true, "the": true, "is": true, "are": true, "was": true, "were": true,
	"be": true, "been": true, "being": true, "to": true, "of": true, "in": true, "on": true,
	"at": true, "for": true, "and": true, "or": true, "but": true, "with": true, "as": true,
	"by": true, "from": true, "into": true, "about": true, "when": true, "where": true,
	"what": true, "who": true, "whom": true, "which": true, "why": true, "how": true,
	"did": true, "do": true, "does": true, "done": true, "go": true, "went": true, "it": true,
	"its": true, "this": true, "that": true, "these": true, "those": true, "i": true, "you": true,
	"he": true, "she": true, "they": true, "we": true, "my": true, "your": true, "his": true,
	"her": true, "their": true, "our": true, "me": true, "him": true, "them": true, "us": true,
}

// SanitizeFTS turns a natural-language question into an FTS5 MATCH-safe
// keyword-retrieval query. Real questions are full sentences ("When did
// Caroline go to the LGBTQ support group?") and FTS5 treats "?", ":",
// quotes, and other punctuation as query syntax, so passing them through
// raw is a SQL logic error, not just a bad match -- so tokens are
// extracted first. Stopwords (closed-class function/question words) are
// then dropped, and the surviving content tokens are double-quoted (an
// FTS5 string literal, immune to operator keywords) and joined with
// " OR ". This is the standard keyword-retrieval shape: FTS5 implicit-AND
// over every raw word requires a single turn to contain the ENTIRE
// question, which on real free-form questions is almost never true
// (hit@k near zero) and isn't how keyword baselines are run in practice;
// OR lets BM25 rank turns by how many/well-matching content terms they
// contain. If stopword filtering would leave zero tokens but the input
// had tokens at all, the unfiltered token set is used instead -- this
// keeps a legitimate single-word (or all-stopword) query like "go"
// searchable rather than silently sanitizing it to "". Returns "" only
// when no tokens survive at all (punctuation-only or empty input) -- an
// empty MATCH string is itself a syntax error, so callers must skip the
// query entirely in that case.
func SanitizeFTS(q string) string {
	toks := ftsToken.FindAllString(q, -1)
	if len(toks) == 0 {
		return ""
	}
	kept := make([]string, 0, len(toks))
	for _, t := range toks {
		lower := strings.ToLower(t)
		if !ftsStopwords[lower] {
			kept = append(kept, lower)
		}
	}
	if len(kept) == 0 {
		// Fallback: stopwords consumed every token (e.g. "go", "the a
		// is"). Use the unfiltered tokens rather than returning "", which
		// would make the query silently match nothing.
		for _, t := range toks {
			kept = append(kept, strings.ToLower(t))
		}
	}
	quoted := make([]string, len(kept))
	for i, t := range kept {
		quoted[i] = `"` + t + `"`
	}
	return strings.Join(quoted, " OR ")
}

// SanitizeTSQuery is the Postgres twin of SanitizeFTS: same tokenization,
// same stopword filtering, same all-stopwords fallback, but emitted as a
// to_tsquery('english', ...) input joined with the OR operator "|".
// plainto_tsquery cannot be used for this: it ANDs every lexeme, so a
// natural-language question ("what's wrong with the pool?") only matches
// bodies containing EVERY content word, which is the exact implicit-AND
// failure mode SanitizeFTS exists to avoid on sqlite. Tokens come from
// ftsToken ([\p{L}\p{N}]+ runs), so they cannot contain quotes,
// backslashes, or tsquery operator characters; no further escaping is
// needed. Returns "" when no tokens survive (punctuation-only input),
// which callers must treat as match-nothing, same as SanitizeFTS.
func SanitizeTSQuery(q string) string {
	toks := ftsToken.FindAllString(q, -1)
	if len(toks) == 0 {
		return ""
	}
	kept := make([]string, 0, len(toks))
	for _, t := range toks {
		lower := strings.ToLower(t)
		if !ftsStopwords[lower] {
			kept = append(kept, lower)
		}
	}
	if len(kept) == 0 {
		for _, t := range toks {
			kept = append(kept, strings.ToLower(t))
		}
	}
	return strings.Join(kept, " | ")
}
