package itbench

import "strings"

// Score is the result of grading one diagnosis against ground truth. The
// SRE task is faulty-entity identification, so we score by which ground-
// truth entities the diagnosis names. Resolved means the primary faulty
// entity (the first listed) was identified - the ITBench "resolved" bar.
type Score struct {
	Resolved bool     `json:"resolved"`
	Matched  []string `json:"matched"`
	Missed   []string `json:"missed"`
	Recall   float64  `json:"recall"` // matched / total ground-truth entities
}

// ScoreDiagnosis grades a free-text diagnosis (an agent finding) against the
// ground-truth faulty entities.
func ScoreDiagnosis(diagnosis string, gt GroundTruth) Score {
	norm := normalize(diagnosis)
	var sc Score
	for i, e := range gt.FaultyEntities {
		if entityNamed(norm, e) {
			sc.Matched = append(sc.Matched, e)
			if i == 0 {
				sc.Resolved = true
			}
		} else {
			sc.Missed = append(sc.Missed, e)
		}
	}
	if n := len(gt.FaultyEntities); n > 0 {
		sc.Recall = float64(len(sc.Matched)) / float64(n)
	}
	return sc
}

// entityNamed reports whether a normalized diagnosis names the entity. The
// entity is matched both verbatim and with separators (-/_/space) unified,
// so "checkout-service" matches "checkout service" and "checkout_service".
func entityNamed(normDiagnosis, entity string) bool {
	e := normalize(entity)
	if e == "" {
		return false
	}
	if strings.Contains(normDiagnosis, e) {
		return true
	}
	return strings.Contains(unifySep(normDiagnosis), unifySep(e))
}

// normalize lowercases and collapses whitespace.
func normalize(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// unifySep replaces -, _ and spaces with a single space so entity names
// written with any separator compare equal.
func unifySep(s string) string {
	r := strings.NewReplacer("-", " ", "_", " ", "/", " ")
	return strings.Join(strings.Fields(r.Replace(s)), " ")
}

// PassHatK reports whether at least one of k attempts resolved (pass^k in
// the optimistic "any pass" sense used across the eval suite).
func PassHatK(results []bool) bool {
	for _, r := range results {
		if r {
			return true
		}
	}
	return false
}
