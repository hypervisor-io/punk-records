// Package memory (defense.go): write-time secret scrubbing. Scrub runs
// inside Write, after ValidateKey and before contentHash, so dedup keys
// on scrubbed content and the raw secret never reaches the database, the
// outbox, or a caller's error path.
package memory

import (
	"errors"
	"regexp"
)

// ErrSensitiveContent is returned by Write in block mode.
var ErrSensitiveContent = errors.New("memory: body contains sensitive content")

// redaction patterns, most-specific-first (sk-ant- before sk-).
// Deliberately small set, trimmed to high-confidence infra secrets.
// ponytail: 11 patterns not 45; extend when a real leak class appears.
var redactions = []struct {
	label string
	re    *regexp.Regexp
}{
	{"private_key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`)},
	{"anthropic_key", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{16,}`)},
	{"openai_key", regexp.MustCompile(`\bsk-[A-Za-z0-9]{20,}`)},
	{"github_token", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}`)},
	{"slack_token", regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}`)},
	{"aws_access_key", regexp.MustCompile(`\b(?:AKIA|ASIA|AGPA|AIDA|AROA|AIPA|ANPA|ANVA)[A-Z0-9]{16}\b`)},
	{"google_api_key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}`)},
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)},
	{"dsn_credentials", regexp.MustCompile(`\b[a-zA-Z][a-zA-Z0-9+.-]*://[^:/\s]+:([^@\s/]+)@`)},
	{"bearer_token", regexp.MustCompile(`(?i)\bbearer\s+([A-Za-z0-9._~+/-]{16,}=*)`)},
	{"password", regexp.MustCompile(`(?i)\b(?:password|passwd|pwd)\s*[=:]\s*(\S{6,})`)},
}

// Fingerprint returns a length-aware, non-reversible preview of a secret
// for audit: first 4 and last 4 chars around an ellipsis, or "[redacted]"
// when the value is shorter than 12 chars. The raw value never survives.
func Fingerprint(raw string) string {
	if len(raw) < 12 {
		return "[redacted]"
	}
	return raw[:4] + "…" + raw[len(raw)-4:]
}

// ScrubAudited scrubs like Scrub but also returns a fingerprint per match
// (parallel to labels, same pattern order). Two-pass per pattern: find
// raw matches, fingerprint them, THEN replace — so a raw secret is read
// only to compute its fingerprint and never assembled into the returned
// scrubbed body.
//
// For compound "label+value" patterns (password, dsn_credentials,
// bearer_token) the secret sits at the tail of the match, not the whole
// match — those three patterns carry a capture group around just the
// secret, and the fingerprint is taken from that submatch so the audit
// event never previews boundary bytes of the real credential. Patterns
// with no capture group (the token-shaped secrets) fingerprint the whole
// match as before.
func ScrubAudited(body string) (string, []string, []string) {
	var labels, fingerprints []string
	for _, r := range redactions {
		matches := r.re.FindAllStringSubmatch(body, -1)
		if len(matches) == 0 {
			continue
		}
		labels = append(labels, r.label)
		secret := matches[0][0]
		if r.re.NumSubexp() > 0 {
			secret = matches[0][1]
		}
		fingerprints = append(fingerprints, Fingerprint(secret))
		body = r.re.ReplaceAllString(body, "[REDACTED:"+r.label+"]")
	}
	return body, labels, fingerprints
}

// Scrub replaces recognized secrets with [REDACTED:<label>] markers and
// returns the labels matched, in pattern order. The raw secret never
// leaves this function. Kept for existing callers/tests; wraps ScrubAudited.
func Scrub(body string) (string, []string) {
	scrubbed, labels, _ := ScrubAudited(body)
	return scrubbed, labels
}

// SetDefense sets the write-time scrub mode: "off"/"", "redact", "block".
func (s *Store) SetDefense(mode string) { s.defense = mode }

// SetDefensePolicy sets a per-namespace mode override; SetDefense remains
// the global fallback for namespaces not in the map.
func (s *Store) SetDefensePolicy(ns, mode string) {
	if s.defensePolicies == nil {
		s.defensePolicies = make(map[string]string)
	}
	s.defensePolicies[ns] = mode
}

// defenseMode resolves the effective write-time scrub mode for a
// namespace: the per-namespace override if set, else the global default.
func (s *Store) defenseMode(ns string) string {
	if m, ok := s.defensePolicies[ns]; ok {
		return m
	}
	return s.defense
}
