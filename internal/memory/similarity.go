package memory

import "strings"

// nameSimilarity returns a [0,1] closeness of two entity names, using a
// leading-token-prefix + edit-ratio blend. Case/punctuation-insensitive.
//   - prefix containment: if the shorter name's tokens are exactly the
//     LEADING tokens of the longer name, in order (e.g. "Alice" is a prefix
//     of "Alice Chen"), that's a strong alias signal -> 0.9. A trailing or
//     out-of-order match ("Alice" vs "Bob Alice") does NOT count - it falls
//     through to the edit ratio instead, which is why bare first names don't
//     merge into an unrelated "<Other> Alice".
//   - else a normalized edit-distance ratio (Levenshtein / maxlen).
//
// Returns max(containmentScore, editRatio). Identical names (after
// normalizing) return 1.0.
func nameSimilarity(a, b string) float64 {
	return nameSimilarityNorm(normalizeName(a), normalizeName(b))
}

// nameSimilarityNorm is nameSimilarity's shared core, taking inputs that
// are already normalizeName'd. A caller comparing one name against many
// candidates (entityArm's match loop) normalizes each side once up front
// instead of paying normalizeName's cost again per pair.
func nameSimilarityNorm(na, nb string) float64 {
	if na == nb {
		return 1.0
	}
	best := editRatio(na, nb)
	if c := containmentScore(na, nb); c > best {
		best = c
	}
	return best
}

// normalizeName lowercases and collapses runs of non-alphanumeric chars to
// a single space, trimmed - the shared comparison base for both the
// containment check and the edit ratio.
func normalizeName(s string) string {
	var b strings.Builder
	prevSpace := true // trims leading separators
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevSpace = false
		case !prevSpace:
			b.WriteByte(' ')
			prevSpace = true
		}
	}
	return strings.TrimRight(b.String(), " ")
}

// containmentScore returns 0.9 if the shorter name's tokens are a non-empty
// proper PREFIX of the longer name's tokens (order-preserving, leading
// position only), else 0. "Alice" vs "Alice Chen" matches; "Alice" vs
// "Bob Alice" does not, since Alice is trailing there, not leading.
func containmentScore(na, nb string) float64 {
	ta, tb := strings.Fields(na), strings.Fields(nb)
	if len(ta) == 0 || len(tb) == 0 || len(ta) == len(tb) {
		return 0
	}
	small, big := ta, tb
	if len(ta) > len(tb) {
		small, big = tb, ta
	}
	for i, t := range small {
		if big[i] != t {
			return 0
		}
	}
	return 0.9
}

// editRatio is a normalized Levenshtein similarity: 1 - distance/maxlen.
func editRatio(a, b string) float64 {
	maxLen := len(a)
	if len(b) > maxLen {
		maxLen = len(b)
	}
	if maxLen == 0 {
		return 1.0
	}
	return 1 - float64(levenshtein(a, b))/float64(maxLen)
}

// levenshtein computes standard edit distance via DP over rune slices.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	m, n := len(ra), len(rb)
	prev := make([]int, n+1)
	cur := make([]int, n+1)
	for j := 0; j <= n; j++ {
		prev[j] = j
	}
	for i := 1; i <= m; i++ {
		cur[0] = i
		for j := 1; j <= n; j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			min := prev[j] + 1 // deletion
			if v := cur[j-1] + 1; v < min {
				min = v // insertion
			}
			if v := prev[j-1] + cost; v < min {
				min = v // substitution
			}
			cur[j] = min
		}
		prev, cur = cur, prev
	}
	return prev[n]
}
