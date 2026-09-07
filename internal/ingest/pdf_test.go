package ingest

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// TestPDFHelperProcess is re-executed as the fake external adapter by
// the PDF loader tests (helper-process pattern: the child is this same
// test binary, guarded by env + a "--" mode arg so a normal run is a
// no-op). The child os.Exit(0)s before the testing framework can print
// trailing PASS noise into what the loader parses as adapter stdout.
func TestPDFHelperProcess(t *testing.T) {
	mode := ""
	for i, a := range os.Args {
		if a == "--" && i+1 < len(os.Args) {
			mode = os.Args[i+1]
		}
	}
	if os.Getenv("PUNK_PDF_HELPER") != "1" || mode == "" {
		return
	}
	switch mode {
	case "ok":
		_, _ = io.WriteString(os.Stdout, `{"revision":"pdf-r1","sections":[{"name":"Page 1","page":1,"text":"extracted page one"},{"name":"Page 2","page":2,"text":"extracted page two"}]}`)
		os.Exit(0)
	case "empty":
		_, _ = io.WriteString(os.Stdout, `{"sections":[]}`)
		os.Exit(0)
	case "badjson":
		_, _ = io.WriteString(os.Stdout, "not json at all")
		os.Exit(0)
	case "fail":
		_, _ = io.WriteString(os.Stderr, "corrupt pdf stream")
		os.Exit(3)
	case "noisystderr":
		_, _ = io.WriteString(os.Stderr, strings.Repeat("e", 6000)) // >4KiB stderr, no newline
		_, _ = io.WriteString(os.Stdout, `{"revision":"noisy","sections":[{"name":"Page 1","page":1,"text":"extracted despite noise"}]}`)
		os.Exit(0)
	case "sleep":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "toobig":
		_, _ = io.WriteString(os.Stdout, strings.Repeat("x", 200))
		os.Exit(0)
	}
	os.Exit(2)
}

func helperAdapter(t *testing.T, mode string) []string {
	t.Helper()
	t.Setenv("PUNK_PDF_HELPER", "1")
	return []string{os.Args[0], "-test.run=^TestPDFHelperProcess$", "--", mode}
}

// TestPDFAdapterUnavailableActionable is the PDF half of the I02 red
// proof: with no adapter configured the error says exactly how to
// configure one, and not a single chunk lands in memory.
func TestPDFAdapterUnavailableActionable(t *testing.T) {
	s := newTestStore(t)
	_, err := Load(context.Background(), Spec{Path: "testdata/sample.pdf"})
	if err == nil {
		t.Fatal("unconfigured pdf load must fail")
	}
	for _, want := range []string{"--pdf-adapter", "PUNK_INGEST_PDF_ADAPTER", "does not bundle"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q must name %q", err, want)
		}
	}
	if _, _, _, _, err := Ingest(context.Background(), s, "ns", "/docs/pdf", "t",
		Spec{Path: "testdata/sample.pdf"}); err == nil {
		t.Fatal("unconfigured pdf ingest must fail")
	}
	if got := liveChunks(t, s, "ns", "/docs/pdf"); len(got) != 0 {
		t.Fatalf("failed pdf ingest left %d chunks: no partial successful ingest allowed", len(got))
	}
}

// TestPDFAdapterContractSuccess: a compliant adapter's JSON sections
// become source-aware sections with page provenance, and the adapter's
// revision label flows into the source when the caller gave none.
func TestPDFAdapterContractSuccess(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	spec := Spec{Path: "testdata/sample.pdf", SourceID: "pdf1",
		PDFAdapter: helperAdapter(t, "ok")}

	doc, err := Load(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Source.MediaType != PDFMediaType {
		t.Fatalf("media type = %q, want %q", doc.Source.MediaType, PDFMediaType)
	}
	if doc.Source.Revision != "pdf-r1" {
		t.Fatalf("revision = %q, want the adapter-reported pdf-r1", doc.Source.Revision)
	}
	if len(doc.Sections) != 2 || doc.Sections[0].Page != 1 || doc.Sections[1].Page != 2 {
		t.Fatalf("sections = %+v, want two pages numbered 1 and 2", doc.Sections)
	}

	w, u, r, b, err := Ingest(ctx, s, "ns", "/docs/pdf", "t", spec)
	if err != nil || w != 2 || u+r+b != 0 {
		t.Fatalf("ingest = %d/%d/%d/%d (err %v), want 2/0/0/0", w, u, r, b, err)
	}
	f := mustFact(t, liveChunks(t, s, "ns", "/docs/pdf"), "extracted page two")
	src := chunkSource(t, f)
	if src["page"] != float64(2) {
		t.Fatalf("page provenance = %v, want 2", src["page"])
	}
	if src["revision"] != "pdf-r1" {
		t.Fatalf("revision provenance = %v, want pdf-r1", src["revision"])
	}

	// A caller-supplied revision wins over the adapter's label.
	spec.Revision = "cli-rev"
	doc, err = Load(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Source.Revision != "cli-rev" {
		t.Fatalf("revision = %q, want caller override cli-rev", doc.Source.Revision)
	}
}

// TestPDFAdapterNoisyStderrSucceedsBounded: an adapter that emits >4KiB
// on stderr and a valid JSON document on stdout still succeeds, and the
// retained error text stays bounded (capWriter) while the stream is fully
// consumed. This is the real-adapter regression for the r1 rejection: an
// unbounded-inherited-pipe / short-write adapter must not break ingest.
func TestPDFAdapterNoisyStderrSucceedsBounded(t *testing.T) {
	spec := Spec{Path: "testdata/sample.pdf", SourceID: "pdf1",
		PDFAdapter: helperAdapter(t, "noisystderr")}
	doc, err := Load(context.Background(), spec)
	if err != nil {
		t.Fatalf("adapter with >4KiB stderr and valid stdout must succeed: %v", err)
	}
	if len(doc.Sections) != 1 || doc.Sections[0].Text != "extracted despite noise" {
		t.Fatalf("sections = %+v, want the one noisy-source section", doc.Sections)
	}
	// The error path must retain only a bounded prefix of stderr.
	cw := &capWriter{max: 4 << 10}
	if _, err := io.Copy(cw, strings.NewReader(strings.Repeat("e", 6000))); err != nil {
		t.Fatalf("capWriter io.Copy errored: %v", err)
	}
	if cw.buf.Len() != 4<<10 {
		t.Fatalf("retained stderr = %d bytes, want bounded %d", cw.buf.Len(), 4<<10)
	}
}

// TestPDFAdapterFailuresVisible: adapter failures (nonzero exit, junk
// output, empty extraction, timeout) surface as actionable errors and
// never write a partial document.
func TestPDFAdapterFailuresVisible(t *testing.T) {
	cases := []struct {
		name    string
		mode    string
		timeout time.Duration
		wantErr string
	}{
		{"exit-nonzero", "fail", 0, "exit status 3"},
		{"stderr-captured", "fail", 0, "corrupt pdf stream"},
		{"junk-output", "badjson", 0, "invalid"},
		{"no-sections", "empty", 0, "no sections"},
		{"timeout", "sleep", 100 * time.Millisecond, "timed out"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newTestStore(t)
			spec := Spec{Path: "testdata/sample.pdf", SourceID: "pdf1",
				PDFAdapter: helperAdapter(t, c.mode), Timeout: c.timeout}
			_, err := Load(context.Background(), spec)
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want substring %q", err, c.wantErr)
			}
			if _, _, _, _, ierr := Ingest(context.Background(), s, "ns", "/docs/pdf", "t", spec); ierr == nil {
				t.Fatal("failed adapter must abort the ingest")
			}
			if got := liveChunks(t, s, "ns", "/docs/pdf"); len(got) != 0 {
				t.Fatalf("failed adapter ingest left %d chunks", len(got))
			}
		})
	}
}

// TestPDFAdapterStdoutExceedsMaxBytesReportsExceeded: an adapter whose
// stdout overruns max_bytes must report the actionable max_bytes error,
// not a generic "failed" wrapping the incidental write error the
// overrun also triggers on the stdout pipe copy (the switch order bug:
// runErr != nil must not shadow stdout.exceeded). Drives PDFLoader.Load
// directly (a small input body, not the sample.pdf fixture) so the
// small MaxBytes bounds only the adapter's stdout, not Spec's separate
// input-size check.
func TestPDFAdapterStdoutExceedsMaxBytesReportsExceeded(t *testing.T) {
	loader := PDFLoader{Command: helperAdapter(t, "toobig"), MaxBytes: 64}
	_, err := loader.Load(context.Background(), LoadInput{Name: "x.pdf", Body: []byte("small pdf body")})
	if err == nil || !strings.Contains(err.Error(), "output exceeds max_bytes") {
		t.Fatalf("err = %v, want substring %q", err, "output exceeds max_bytes")
	}
	// The overrun also makes the stdout pipe-copy fail, so runErr is
	// non-nil too; that must not shadow the specific max_bytes message
	// with the generic "failed: <runErr>" wrapper (which happens to
	// still embed the same substring via %w, so the check above alone
	// cannot tell the two apart).
	if strings.Contains(err.Error(), "failed") {
		t.Fatalf("err = %v, want the clean max_bytes message, not the generic runErr-wrapped \"failed\" message", err)
	}
}
