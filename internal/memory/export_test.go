package memory

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestExportImportRoundTrip(t *testing.T) {
	s, db, _ := newTest(t)
	ctx := context.Background()

	if _, err := s.Remember(ctx, "ns", "/k1", "v1", map[string]any{"env": "prod"}, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Remember(ctx, "ns", "/k1", "v2", nil, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Remember(ctx, "ns", "/k2", "x", nil, "b"); err != nil {
		t.Fatal(err)
	}
	if err := s.Forget(ctx, "ns", "/k2", "b"); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := s.ExportJSONL(ctx, "ns", &buf); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(buf.String(), "\n"); n != 4 {
		t.Fatalf("export lines = %d, want 4 (full history incl tombstone)", n)
	}

	// restore flow: wipe the instance (fresh deployment), then import
	if _, err := db.ExecContext(ctx, `DELETE FROM memories`); err != nil {
		t.Fatal(err)
	}
	imported, skipped, blocked, err := s.ImportJSONL(ctx, "ns", bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if imported != 4 || skipped != 0 || blocked != 0 {
		t.Fatalf("import = %d/%d/%d, want 4/0/0", imported, skipped, blocked)
	}

	facts, err := s.Recall(ctx, "ns", "/", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 || facts[0].Key != "/k1" || facts[0].Body != "v2" {
		t.Fatalf("imported recall = %+v, want /k1=v2 only (k2 tombstoned)", facts)
	}

	// idempotent: importing again skips everything already present
	imported, skipped, blocked, err = s.ImportJSONL(ctx, "ns", bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if imported != 0 || skipped != 4 || blocked != 0 {
		t.Fatalf("re-import = %d/%d/%d, want 0/4/0", imported, skipped, blocked)
	}
}

func TestImportRejectsMalformedLine(t *testing.T) {
	s, _, _ := newTest(t)
	bad := `{"id":"x1","key":"/k","action":"add","body":"v","created_at":"2026-07-06T00:00:00.000000000Z"}
{"id":"x2","key":"/k","action":"EXPLODE","body":"v","created_at":"2026-07-06T00:00:01.000000000Z"}`
	_, _, _, err := s.ImportJSONL(context.Background(), "ns", strings.NewReader(bad))
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("err = %v, want line 2 rejection", err)
	}
	// whole import rolled back
	facts, ferr := s.Recall(context.Background(), "ns", "/", 0)
	if ferr != nil {
		t.Fatal(ferr)
	}
	if len(facts) != 0 {
		t.Fatalf("partial import survived rollback: %+v", facts)
	}
}

// TestImportDefenseRedact covers finding #1 (CRITICAL): ImportJSONL used
// to INSERT rec.Body via raw SQL, bypassing write-time defense entirely
// (reachable from `punk import`, MergeBranch, PullRegion). In redact
// mode the imported body must be scrubbed, never the raw secret.
func TestImportDefenseRedact(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := context.Background()
	s.SetDefense("redact")
	line := `{"id":"i1","key":"/dsn","action":"add","body":"conn postgres://u:pw12345678@h/db","created_at":"2026-07-06T00:00:00.000000000Z"}` + "\n"
	imported, skipped, blocked, err := s.ImportJSONL(ctx, "ns", strings.NewReader(line))
	if err != nil {
		t.Fatal(err)
	}
	if imported != 1 || skipped != 0 || blocked != 0 {
		t.Fatalf("import = %d/%d/%d, want 1/0/0", imported, skipped, blocked)
	}
	facts, err := s.Recall(ctx, "ns", "/", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 || strings.Contains(facts[0].Body, "pw12345678") {
		t.Fatalf("imported body leaked raw secret: %+v", facts)
	}
	if !strings.Contains(facts[0].Body, "[REDACTED:dsn_credentials]") {
		t.Fatalf("imported body not redacted: %q", facts[0].Body)
	}
}

// TestImportDefenseBlock covers finding #1: block mode must skip only the
// offending record (counted in blocked), not abort the whole import.
func TestImportDefenseBlock(t *testing.T) {
	s, _, _ := newTest(t)
	ctx := context.Background()
	s.SetDefense("block")
	lines := `{"id":"b1","key":"/clean","action":"add","body":"nothing sensitive here","created_at":"2026-07-06T00:00:00.000000000Z"}
{"id":"b2","key":"/secret","action":"add","body":"token ghp_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789","created_at":"2026-07-06T00:00:01.000000000Z"}
`
	imported, skipped, blocked, err := s.ImportJSONL(ctx, "ns", strings.NewReader(lines))
	if err != nil {
		t.Fatal(err)
	}
	if imported != 1 || skipped != 0 || blocked != 1 {
		t.Fatalf("import = %d/%d/%d, want 1/0/1 (secret record blocked, not the whole import)", imported, skipped, blocked)
	}
	facts, err := s.Recall(ctx, "ns", "/", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 || facts[0].Key != "/clean" {
		t.Fatalf("recall = %+v, want only /clean (blocked record never landed)", facts)
	}
}
