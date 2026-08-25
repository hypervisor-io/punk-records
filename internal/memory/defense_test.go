package memory

import (
	"errors"
	"strings"
	"testing"
)

func TestScrubPatterns(t *testing.T) {
	cases := []struct{ in, wantLabel string }{
		{"key is sk-ant-api03-AAAABBBBCCCCDDDDEEEEFFFF1234 ok", "anthropic_key"},
		{"openai sk-AbCdEfGhIjKlMnOpQrStUvWx1234", "openai_key"},
		{"token ghp_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789", "github_token"},
		{"slack xoxb-123456789012-abcdefghijklmnop", "slack_token"},
		{"aws AKIAIOSFODNN7EXAMPLE", "aws_access_key"},
		{"gcp AIzaSyA-1234567890abcdefghijklmnopqrstu", "google_api_key"},
		{"jwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U", "jwt"},
		{"dsn postgres://admin:hunter22secret@db.internal:5432/prod", "dsn_credentials"},
		{"dsn mssql://admin:hunter22secret@db.internal:1433/prod", "dsn_credentials"},
		{"dsn oracle://admin:hunter22secret@db.internal:1521/prod", "dsn_credentials"},
		{"url http://admin:hunter22secret@h/", "dsn_credentials"},
		{"Authorization: Bearer AbCdEf0123456789.AbCdEf0123456789", "bearer_token"},
		{"password = supersecret99", "password"},
		{"-----BEGIN RSA PRIVATE KEY-----\nMIIEow\n-----END RSA PRIVATE KEY-----", "private_key"},
	}
	for _, c := range cases {
		out, labels := Scrub(c.in)
		if len(labels) == 0 || labels[0] != c.wantLabel {
			t.Errorf("Scrub(%q) labels = %v, want [%s]", c.in, labels, c.wantLabel)
		}
		if !strings.Contains(out, "[REDACTED:"+c.wantLabel+"]") {
			t.Errorf("Scrub(%q) = %q, missing marker", c.in, out)
		}
		if strings.Contains(out, "hunter22secret") || strings.Contains(out, "AKIAIOSFODNN7EXAMPLE") {
			t.Errorf("Scrub(%q) leaked raw secret: %q", c.in, out)
		}
	}
	clean, labels := Scrub("postgres primary lives on ceph-node-07 vlan 40")
	if len(labels) != 0 || clean != "postgres primary lives on ceph-node-07 vlan 40" {
		t.Errorf("clean text mangled: %q %v", clean, labels)
	}
}

// TestScrubDSNPasswordExcludesSlash covers the finding that the
// dsn_credentials password class [^@\s]+ crossed "/", so an ordinary URL
// with a path and an "@" further along (e.g. an email in a query string)
// was falsely treated as embedded credentials and irreversibly redacted.
// The class must stop at "/" (RFC 3986 userinfo excludes raw "/").
func TestScrubDSNPasswordExcludesSlash(t *testing.T) {
	clean := "see https://mail.example.com:8080/compose?to=ops@example.com for the runbook"
	out, labels := Scrub(clean)
	if len(labels) != 0 || out != clean {
		t.Fatalf("Scrub(%q) = %q, labels %v; want unchanged, no match", clean, out, labels)
	}

	// existing case must keep passing: a real DSN password still matches.
	dsn := "dsn postgres://u:pw12345678@h/db"
	out, labels = Scrub(dsn)
	if len(labels) == 0 || labels[0] != "dsn_credentials" {
		t.Fatalf("Scrub(%q) labels = %v, want [dsn_credentials]", dsn, labels)
	}
	if !strings.Contains(out, "[REDACTED:dsn_credentials]") || strings.Contains(out, "pw12345678") {
		t.Fatalf("Scrub(%q) = %q, want password redacted", dsn, out)
	}
}

// TestScrubMultipleSecrets covers finding #4a: a body with several
// distinct secret types must have every match replaced, and the
// returned labels must list one entry per matched pattern in the
// fixed pattern order (redactions slice order), not match order.
func TestScrubMultipleSecrets(t *testing.T) {
	body := "password = supersecret99 and token ghp_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789 " +
		"and dsn postgres://admin:hunter22secret@db.internal:5432/prod"
	out, labels := Scrub(body)
	wantLabels := []string{"github_token", "dsn_credentials", "password"}
	if len(labels) != len(wantLabels) {
		t.Fatalf("labels = %v, want %v", labels, wantLabels)
	}
	for i, want := range wantLabels {
		if labels[i] != want {
			t.Errorf("labels[%d] = %q, want %q (pattern order)", i, labels[i], want)
		}
	}
	for _, raw := range []string{"supersecret99", "ghp_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789", "hunter22secret"} {
		if strings.Contains(out, raw) {
			t.Errorf("Scrub multi-secret leaked %q in %q", raw, out)
		}
	}
	for _, marker := range []string{"[REDACTED:password]", "[REDACTED:github_token]", "[REDACTED:dsn_credentials]"} {
		if !strings.Contains(out, marker) {
			t.Errorf("Scrub multi-secret missing marker %q in %q", marker, out)
		}
	}
}

func TestWriteDefenseModes(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	s.SetDefense("redact")
	f, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/dsn", Body: "conn postgres://u:pw12345678@h/db"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.Body, "[REDACTED:dsn_credentials]") || strings.Contains(f.Body, "pw12345678") {
		t.Fatalf("redact mode stored %q", f.Body)
	}
	s.SetDefense("block")
	_, err = s.Write(ctx, WriteInput{Namespace: "ns", Key: "/k2", Body: "token ghp_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"})
	if !errors.Is(err, ErrSensitiveContent) {
		t.Fatalf("block mode err = %v, want ErrSensitiveContent", err)
	}
	// finding #4b: the error string itself must never carry the raw secret
	if strings.Contains(err.Error(), "ghp_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789") {
		t.Fatalf("block mode error leaked raw secret: %v", err)
	}
	s.SetDefense("off")
	f, err = s.Write(ctx, WriteInput{Namespace: "ns", Key: "/k3", Body: "password = launchcode1"})
	if err != nil || strings.Contains(f.Body, "REDACTED") {
		t.Fatalf("off mode altered body: %q err %v", f.Body, err)
	}
}

func TestFingerprint(t *testing.T) {
	fp := Fingerprint("ghp_AbCdEfGhIjKlMnOpQr")
	if !strings.HasPrefix(fp, "ghp_") || !strings.HasSuffix(fp, "OpQr") || !strings.Contains(fp, "…") {
		t.Fatalf("Fingerprint(long) = %q, want ghp_…OpQr shape", fp)
	}
	if strings.Contains(fp, "IjKlMn") {
		t.Fatalf("Fingerprint(long) = %q leaked middle substring", fp)
	}
	if got := Fingerprint("short"); got != "[redacted]" {
		t.Fatalf("Fingerprint(short) = %q, want [redacted]", got)
	}
}

// TestScrubAuditedNoSecretTailLeak covers the fingerprint-tail-leak defect:
// for the three compound "label+value" patterns (password, dsn_credentials,
// bearer_token) the real secret sits at the tail of the whole regex match,
// so fingerprinting the WHOLE match let a short secret dodge Fingerprint's
// "<12 chars -> [redacted]" guard purely because the surrounding label or
// scheme text padded the match past 12 chars. ScrubAudited must fingerprint
// the captured secret submatch instead, so a short secret is judged on its
// own length, not the length of the label/prefix wrapped around it.
func TestScrubAuditedNoSecretTailLeak(t *testing.T) {
	// dsn password "hunter2pass" is 11 raw chars (<12). Under the old
	// whole-match fingerprinting, the match "postgres://admin:hunter2pass@"
	// (30 chars) cleared the 12-char floor and the tail-4 window ("ass@")
	// spilled real trailing bytes of the password into the audit event.
	// Fingerprinting the submatch alone must now fully redact it.
	_, _, fps := ScrubAudited("conn postgres://admin:hunter2pass@h/db")
	if len(fps) != 1 {
		t.Fatalf("dsn fps = %v, want 1 entry", fps)
	}
	if fps[0] != "[redacted]" || strings.Contains(fps[0], "hunter2pass") || strings.Contains(fps[0], "pass") {
		t.Errorf("dsn fingerprint = %q, want [redacted] (11-char password must not clear the 12-char floor via the surrounding match)", fps[0])
	}

	// password value shorter than 12 chars: the whole match
	// "password=abc123" is 16 chars (>=12) so the OLD code treated it as
	// long enough for a real preview and leaked "c123" - the tail of a
	// 6-char password that, judged on its own, must be fully redacted.
	_, _, fps = ScrubAudited("password=abc123")
	if len(fps) != 1 || fps[0] != "[redacted]" {
		t.Fatalf("short password fps = %v, want [[redacted]]", fps)
	}

	// Secrets >=12 chars legitimately get a first4...last4 preview per
	// Fingerprint's documented contract. Verify that preview is anchored
	// to the SECRET itself (never the surrounding label/scheme text) and
	// never exposes the untruncated raw value or its middle.
	longCases := []struct{ body, secret string }{
		{"password = SuperSecret123", "SuperSecret123"},
		{"bearer abcdef0123456789ghij", "abcdef0123456789ghij"},
	}
	for _, c := range longCases {
		_, _, fps = ScrubAudited(c.body)
		if len(fps) != 1 {
			t.Fatalf("ScrubAudited(%q) fps = %v, want 1 entry", c.body, fps)
		}
		fp := fps[0]
		if strings.Contains(fp, c.secret) {
			t.Errorf("ScrubAudited(%q) fingerprint %q leaked the full raw secret", c.body, fp)
		}
		if middle := c.secret[4 : len(c.secret)-4]; strings.Contains(fp, middle) {
			t.Errorf("ScrubAudited(%q) fingerprint %q leaked secret middle %q", c.body, fp, middle)
		}
	}
}

func TestScrubAuditedParallelArrays(t *testing.T) {
	body := "token ghp_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789 " +
		"and dsn postgres://admin:hunter22secret@db.internal:5432/prod"
	scrubbed, labels, fps := ScrubAudited(body)
	if len(labels) != 2 || len(fps) != 2 {
		t.Fatalf("labels=%v fps=%v, want 2 of each", labels, fps)
	}
	wantLabels := []string{"github_token", "dsn_credentials"}
	for i, want := range wantLabels {
		if labels[i] != want {
			t.Errorf("labels[%d] = %q, want %q (pattern order)", i, labels[i], want)
		}
	}
	for _, raw := range []string{"ghp_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789", "hunter22secret"} {
		if strings.Contains(scrubbed, raw) {
			t.Errorf("ScrubAudited leaked raw secret %q in scrubbed body", raw)
		}
		for _, fp := range fps {
			if strings.Contains(fp, raw) {
				t.Errorf("ScrubAudited leaked raw secret %q in fingerprint %q", raw, fp)
			}
		}
	}
}

func TestPerNamespaceDefense(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	s.SetDefense("off")
	s.SetDefensePolicy("secure-ns", "redact")

	f, err := s.Write(ctx, WriteInput{Namespace: "secure-ns", Key: "/dsn", Body: "conn postgres://u:pw12345678@h/db"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.Body, "[REDACTED:dsn_credentials]") || strings.Contains(f.Body, "pw12345678") {
		t.Fatalf("per-ns redact stored %q", f.Body)
	}

	f, err = s.Write(ctx, WriteInput{Namespace: "other-ns", Key: "/dsn", Body: "conn postgres://u:pw12345678@h/db"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.Body, "pw12345678") {
		t.Fatalf("other-ns (no policy, global off) got scrubbed: %q", f.Body)
	}
}

func TestDefenseAuditEvent(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	s.SetDefense("redact")

	const secret = "ghp_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"
	if _, err := s.Write(ctx, WriteInput{Namespace: "ns", Key: "/secret", Body: "token " + secret}); err != nil {
		t.Fatal(err)
	}

	var got []OutboxEvent
	n, err := s.DrainOutbox(ctx, 10, func(e OutboxEvent) error { got = append(got, e); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("drained n=%d, want 2 (memory + defense)", n)
	}
	var defenseEvent *OutboxEvent
	for i := range got {
		if got[i].Kind == "defense" {
			defenseEvent = &got[i]
		}
	}
	if defenseEvent == nil {
		t.Fatalf("no defense event among %+v", got)
	}
	if defenseEvent.Payload["labels"] != "github_token" {
		t.Fatalf("defense event labels = %q, want github_token", defenseEvent.Payload["labels"])
	}
	if defenseEvent.Payload["fingerprints"] == "" {
		t.Fatalf("defense event missing fingerprints")
	}
	for _, v := range defenseEvent.Payload {
		if strings.Contains(v, secret) {
			t.Fatalf("defense event leaked raw secret in field: %+v", defenseEvent.Payload)
		}
	}
}
