package main

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/hypervisor-io/punk-records/internal/embedlocal"
	"github.com/hypervisor-io/punk-records/internal/store"
)

// dryRunConfig writes a config that names both enrichment models and
// points the local embedder's model cache at a temp directory. The
// default profile's API key env is deliberately left unset: a dry-run
// reads model IDs from configuration and must never construct the
// dependencies those IDs belong to, so a preview that tried to build an
// LLM client or download the local model would fail here.
func dryRunConfig(t *testing.T) (cfgPath, dsn, cache string) {
	t.Helper()
	dir := t.TempDir()
	dsn = filepath.Join(dir, "dry.db")
	cache = filepath.Join(dir, "models")
	cfgPath = filepath.Join(dir, "config.yaml")
	yaml := strings.Join([]string{
		"db:",
		"  driver: sqlite",
		"  dsn: " + dsn,
		"ai:",
		"  enabled: true",
		"  profiles:",
		"    default:",
		"      model: gpt-5-mini",
		"      api_key_env: PUNK_DRY_RUN_ABSENT_KEY",
		"  embeddings:",
		"    provider: local",
		"    model_cache: " + cache,
		"memory:",
		"  entities: true",
		"",
	}, "\n")
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"migrate", "--config", cfgPath, "up"}); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	return cfgPath, dsn, cache
}

func dryRunFixture() string {
	return filepath.Join("..", "..", "internal", "ingest", "testdata", "runbook.md")
}

// TestCmdIngestDryRunWritesNothing: --dry-run predicts the whole delta and
// the deferred enrichment work it would trigger, naming the configured
// models, and changes nothing - no chunk is written, no model is
// downloaded and no client is built (the configured API key env is unset).
// The CLI's own writer has no embedder wired, so no embedding is
// attributed to write time: the embed_link stage carries it.
func TestCmdIngestDryRunWritesNothing(t *testing.T) {
	cfgPath, dsn, cache := dryRunConfig(t)
	t.Setenv("PUNK_DRY_RUN_ABSENT_KEY", "")
	args := []string{"--config", cfgPath, "--ns", "ns", "--prefix", "/docs/rb",
		"--source-id", "runbook", "--dry-run", dryRunFixture()}

	out, err := captureStdout(t, func() error { return cmdIngest(args) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"6 chunks, 6 would change, 0 unchanged, 0 removed, 0 blocked",
		"write-time embedding: none - no embedder is wired on this store",
		"the configured server makes over the stored bodies",
		"embed_link v1 " + embedlocal.DefaultModel + ": 6 run(s), 6 model call(s)",
		"entities v1 gpt-5-mini: 6 run(s), 1 model call(s)",
		"exact body bytes",
		"exact keyed embedding bytes",
		"estimated input tokens",
		"estimated output tokens",
		"exact input bytes (what the stage sends)",
		"not counted:",
		"bytes/4",
		"deferred processor work",
		"facts[self].Body",
		"a real ingest by this writer makes no model call at all",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry-run output =\n%s\nwant it to contain %q", out, want)
		}
	}
	// the deferred stage is sized over the bodies the processor really
	// sends, so its exact byte count equals the entity stage's and the
	// body total, and is strictly smaller than the keyed total
	bodyBytes, keyedBytes := totalByteCounts(t, out)
	if got := stageInputBytes(t, out, "embed_link"); got != bodyBytes {
		t.Fatalf("embed_link line reports %d exact input bytes, want the %d body bytes the processor sends", got, bodyBytes)
	}
	if got := stageInputBytes(t, out, "entities"); got != bodyBytes {
		t.Fatalf("entities line reports %d exact input bytes, want the %d body bytes", got, bodyBytes)
	}
	if keyedBytes <= bodyBytes {
		t.Fatalf("total line reports %d keyed embedding bytes against %d body bytes: the keyed shape must be larger", keyedBytes, bodyBytes)
	}
	if facts := recallChunks(t, dsn, "ns", "/docs/rb/chunk-"); len(facts) != 0 {
		t.Fatalf("dry-run wrote %d chunks: a preview must write nothing", len(facts))
	}
	if entries, err := os.ReadDir(cache); err == nil && len(entries) > 0 {
		t.Fatalf("dry-run populated the model cache %s: %v; a preview downloads no model", cache, entries)
	}
}

// TestCmdIngestDryRunAfterIngestPredictsNoWork: once the document is in
// memory, the same preview predicts no write and no enrichment work - the
// delta a repeat ingest really produces.
func TestCmdIngestDryRunAfterIngestPredictsNoWork(t *testing.T) {
	cfgPath, dsn, _ := dryRunConfig(t)
	t.Setenv("PUNK_DRY_RUN_ABSENT_KEY", "")
	base := []string{"--config", cfgPath, "--ns", "ns", "--prefix", "/docs/rb", "--source-id", "runbook"}
	if _, err := captureStdout(t, func() error { return cmdIngest(append(base, dryRunFixture())) }); err != nil {
		t.Fatal(err)
	}

	out, err := captureStdout(t, func() error {
		return cmdIngest(append(base, "--dry-run", dryRunFixture()))
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"6 chunks, 0 would change, 6 unchanged",
		"entities v1 gpt-5-mini: 0 run(s), 0 model call(s)",
		"0 exact body bytes + 0 exact keyed embedding bytes, ~0 estimated input tokens, ~0 estimated output tokens",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("repeat dry-run output =\n%s\nwant it to contain %q", out, want)
		}
	}
	if facts := recallChunks(t, dsn, "ns", "/docs/rb/chunk-"); len(facts) != 6 {
		t.Fatalf("live chunks = %d, want the 6 the real ingest wrote", len(facts))
	}
}

// TestCmdIngestDryRunPDFUnestimated: a PDF the preview will not hand to an
// external adapter is reported unestimated rather than as cheap work, and
// the run still succeeds and writes nothing.
func TestCmdIngestDryRunPDFUnestimated(t *testing.T) {
	cfgPath, dsn, _ := dryRunConfig(t)
	t.Setenv("PUNK_INGEST_PDF_ADAPTER", "")
	pdf := filepath.Join(t.TempDir(), "report.pdf")
	if err := os.WriteFile(pdf, []byte("%PDF-1.4\n%%EOF\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error {
		return cmdIngest([]string{"--config", cfgPath, "--ns", "ns", "--prefix", "/docs/pdf", "--dry-run", pdf})
	})
	if err != nil {
		t.Fatalf("pdf dry-run failed: %v; an unmeasurable input is a reported gap, not an error", err)
	}
	for _, want := range []string{"unestimated", "--allow-adapter", "not counted:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("pdf dry-run output =\n%s\nwant it to contain %q", out, want)
		}
	}
	if strings.Contains(out, "would change") {
		t.Fatalf("pdf dry-run output =\n%s\nwant no chunk counts for an unestimated input", out)
	}
	if facts := recallChunks(t, dsn, "ns", "/docs/pdf/chunk-"); len(facts) != 0 {
		t.Fatalf("pdf dry-run wrote %d chunks", len(facts))
	}
}

// TestCmdIngestAllowAdapterNeedsDryRun: --allow-adapter loosens a preview's
// no-subprocess rule, so passing it to a real ingest is a usage error
// rather than a silently ignored flag.
func TestCmdIngestAllowAdapterNeedsDryRun(t *testing.T) {
	cfgPath, _, _ := dryRunConfig(t)
	err := cmdIngest([]string{"--config", cfgPath, "--ns", "ns", "--prefix", "/docs/rb",
		"--allow-adapter", dryRunFixture()})
	if err == nil || !strings.Contains(err.Error(), "--allow-adapter only applies to --dry-run") {
		t.Fatalf("err = %v, want the --allow-adapter usage error", err)
	}
}

// embeddedChunks counts the chunks a real ingest actually stored with a
// vector: the writer's own embedding work, not what a background processor
// would do later.
func embeddedChunks(t *testing.T, dsn, prefix string) int {
	t.Helper()
	db, err := store.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM memories WHERE key LIKE '`+prefix+`/chunk-%' AND embedding IS NOT NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// totalByteCounts reads the total line's two exact byte counts: the chunk
// body bytes and the keyed embedding bytes (key + body). They differ, and
// the deferred embed_link stage is sized over the first, not the second.
func totalByteCounts(t *testing.T, out string) (body, keyed int) {
	t.Helper()
	m := regexp.MustCompile(`total: ([0-9]+) exact body bytes \+ ([0-9]+) exact keyed embedding bytes`).FindStringSubmatch(out)
	if len(m) != 3 {
		t.Fatalf("dry-run output =\n%s\nwant a total line with both exact byte counts", out)
	}
	body, _ = strconv.Atoi(m[1])
	keyed, _ = strconv.Atoi(m[2])
	return body, keyed
}

// stageInputBytes reads one stage line's exact input byte count: the bytes
// that stage really sends.
func stageInputBytes(t *testing.T, out, stage string) int {
	t.Helper()
	m := regexp.MustCompile(`  ` + stage + ` v[0-9]+ [^\n]+: [0-9]+ run\(s\), [0-9]+ model call\(s\), ([0-9]+) exact input bytes`).
		FindStringSubmatch(out)
	if len(m) != 2 {
		t.Fatalf("dry-run output =\n%s\nwant a %s stage line with an exact input byte count", out, stage)
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// TestCmdIngestDryRunWriteEmbeddingMatchesWriter: the ordinary CLI writer
// opens memory with no embedder, so it stores chunks unembedded and the
// configured server's embed_link stage embeds them later, when it drains
// the outbox. A preview must attribute embedding work the same way: zero
// write-time calls for this writer, and the changed-chunk embedding calls
// predicted for the deferred stage - never write-time calls the writer
// will not make. Folded from the reviewer's reproduced
// /tmp/punk-P02-review-cli-embedding_test.go.
func TestCmdIngestDryRunWriteEmbeddingMatchesWriter(t *testing.T) {
	cfgPath, dsn, _ := dryRunConfig(t)
	t.Setenv("PUNK_DRY_RUN_ABSENT_KEY", "")
	base := []string{"--config", cfgPath, "--ns", "ns", "--prefix", "/docs/rb", "--source-id", "runbook"}

	preview, err := captureStdout(t, func() error {
		return cmdIngest(append(append([]string{}, base...), "--dry-run", dryRunFixture()))
	})
	if err != nil {
		t.Fatal(err)
	}
	predicted := 0
	if m := regexp.MustCompile(`write-time embedding [^\n]+: ([0-9]+) call\(s\)`).FindStringSubmatch(preview); len(m) > 0 {
		predicted, _ = strconv.Atoi(m[1])
	}
	if _, err := captureStdout(t, func() error {
		return cmdIngest(append(append([]string{}, base...), dryRunFixture()))
	}); err != nil {
		t.Fatal(err)
	}
	embedded := embeddedChunks(t, dsn, "/docs/rb")
	if predicted != embedded {
		t.Fatalf("preview attributes %d calls to CLI write-time embedding, but real CLI wrote %d embedded chunks; background embedding is a separate stage",
			predicted, embedded)
	}
	if embedded != 0 {
		t.Fatalf("real CLI ingest embedded %d chunks, want 0: the ordinary writer has no embedder wired", embedded)
	}
	// the deferred stage carries the embedding work instead, and the
	// preview says whose work it assumes
	if !strings.Contains(preview, "embed_link v1 "+embedlocal.DefaultModel+": 6 run(s), 6 model call(s)") {
		t.Fatalf("dry-run output =\n%s\nwant the embed_link stage to predict the 6 deferred embedding calls", preview)
	}
	for _, want := range []string{"no embedder is wired", "drains the outbox"} {
		if !strings.Contains(preview, want) {
			t.Fatalf("dry-run output =\n%s\nwant it to disclose %q", preview, want)
		}
	}
}
