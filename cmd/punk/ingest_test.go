package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/store"
)

// ingestConfig writes a minimal config pointing the CLI at a temp SQLite
// database and migrates it via the real CLI path (mirrors authzCLISetup).
func ingestConfig(t *testing.T) (cfgPath, dsn string) {
	t.Helper()
	dir := t.TempDir()
	dsn = filepath.Join(dir, "ingest.db")
	cfgPath = filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("db:\n  driver: sqlite\n  dsn: "+dsn+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"migrate", "--config", cfgPath, "up"}); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	return cfgPath, dsn
}

func recallChunks(t *testing.T, dsn, ns, prefix string) []memory.Fact {
	t.Helper()
	db, err := store.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	facts, err := memory.New(db, nil).Recall(context.Background(), ns, prefix, 100)
	if err != nil {
		t.Fatal(err)
	}
	return facts
}

// TestCmdIngestMarkdownTwice: the CLI loads a Markdown file through the
// I02 loader stack into I01's source-aware write path; a byte-identical
// second run reports zero writes (delta ingest end to end).
func TestCmdIngestMarkdownTwice(t *testing.T) {
	cfg, dsn := ingestConfig(t)
	fixture := filepath.Join("..", "..", "internal", "ingest", "testdata", "runbook.md")
	args := []string{"--config", cfg, "--ns", "ns", "--prefix", "/docs/rb", "--source-id", "runbook", fixture}

	out, err := captureStdout(t, func() error { return cmdIngest(args) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "6 written, 0 unchanged") {
		t.Fatalf("first ingest output = %q, want 6 written, 0 unchanged", out)
	}
	out, err = captureStdout(t, func() error { return cmdIngest(args) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "0 written, 6 unchanged") {
		t.Fatalf("repeat ingest output = %q, want 0 written, 6 unchanged: repeated ingestion must write no unchanged chunks", out)
	}

	facts := recallChunks(t, dsn, "ns", "/docs/rb/chunk-")
	if len(facts) != 6 {
		t.Fatalf("live chunks = %d, want exactly the 6 chunks", len(facts))
	}
	for _, f := range facts {
		if _, ok := f.Attributes["source"].(map[string]any); !ok {
			t.Fatalf("chunk %s has no source provenance: %v", f.Key, f.Attributes)
		}
	}
}

// TestCmdIngestPDFUnavailable: with no external adapter configured a PDF
// ingest fails with the configuration instructions and writes nothing.
func TestCmdIngestPDFUnavailable(t *testing.T) {
	t.Setenv("PUNK_INGEST_PDF_ADAPTER", "")
	cfg, dsn := ingestConfig(t)
	pdf := filepath.Join(t.TempDir(), "report.pdf")
	if err := os.WriteFile(pdf, []byte("%PDF-1.4\n%%EOF\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := cmdIngest([]string{"--config", cfg, "--ns", "ns", "--prefix", "/docs/pdf", pdf})
	if err == nil {
		t.Fatal("pdf ingest without an adapter must fail")
	}
	if !strings.Contains(err.Error(), "--pdf-adapter") || !strings.Contains(err.Error(), "PUNK_INGEST_PDF_ADAPTER") {
		t.Fatalf("err = %v, want the adapter configuration instructions", err)
	}
	if facts := recallChunks(t, dsn, "ns", "/docs/pdf/chunk-"); len(facts) != 0 {
		t.Fatalf("failed pdf ingest wrote %d chunks: no partial successful ingest allowed", len(facts))
	}
}
