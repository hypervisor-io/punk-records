package membench

import (
	"strings"
	"testing"
)

// Build-embedded provenance (E01 review r3): the source identity of a
// report comes from the running binary's OWN VCS stamp, so it cannot
// record an unrelated invocation repository's HEAD as the source, and
// it is honest "unknown" when the binary carries no stamp. The
// stamped-binary half of the contract (same binary run from an
// unrelated repo / outside the build tree reports the same build-time
// revision) is proven end-to-end with the real CLI binary in
// cmd/punk/main_test.go (TestMembenchBinaryProvenanceAcrossInvocationRepos)
// because `go test` binaries deliberately carry no VCS stamp.

// TestBuildSourceRevisionHonestUnknownInTestBinary: a `go test` binary
// has no embedded VCS info, so the only honest value here is explicit
// "unknown" - never "", never a build stamp, never a revision borrowed
// from the repository the test happens to run in.
func TestBuildSourceRevisionHonestUnknownInTestBinary(t *testing.T) {
	if got := BuildSourceRevision(); got != SourceRevisionUnknown {
		t.Fatalf("BuildSourceRevision() = %q in a go test binary (no VCS stamp), want explicit %q", got, SourceRevisionUnknown)
	}
	if again := BuildSourceRevision(); again != SourceRevisionUnknown {
		t.Fatalf("second BuildSourceRevision() = %q, want a constant %q: provenance is a function of the binary, not the invocation context", again, SourceRevisionUnknown)
	}
}

// TestSuiteSourceRevisionMatchesBuildInfo: Suite records exactly what
// the binary's build info says - no more (no invocation-repo revision),
// no less (never silently empty or a build stamp like "dev").
func TestSuiteSourceRevisionMatchesBuildInfo(t *testing.T) {
	rep, err := Suite(t.Context(), newBenchStore(t), "bench", suiteFixtureRecs(t), suiteOpts())
	if err != nil {
		t.Fatal(err)
	}
	if want := BuildSourceRevision(); rep.SourceRevision != want {
		t.Fatalf("SourceRevision = %q, want exactly BuildSourceRevision() = %q", rep.SourceRevision, want)
	}
	if rep.SourceRevision == "" || rep.SourceRevision == "dev" {
		t.Fatalf("SourceRevision = %q, want embedded VCS provenance or explicit %q, never a build stamp", rep.SourceRevision, SourceRevisionUnknown)
	}
}

// TestStableRetainsSourceIdentityResultOnlyStripsIt: the r3 finding-2
// contract. A reproducibility manifest must PRESERVE code identity:
// Stable() strips only volatile timing fields, so StableJSON still
// carries source_revision; the provenance-blind projection for
// cross-revision metric comparison is the explicitly named ResultOnly,
// never the silent default.
func TestStableRetainsSourceIdentityResultOnlyStripsIt(t *testing.T) {
	rep, err := Suite(t.Context(), newBenchStore(t), "bench", suiteFixtureRecs(t), suiteOpts())
	if err != nil {
		t.Fatal(err)
	}
	stable := rep.Stable()
	if stable.SourceRevision != rep.SourceRevision {
		t.Fatalf("Stable().SourceRevision = %q, want preserved %q: source identity is part of the reproducibility claim, not volatile", stable.SourceRevision, rep.SourceRevision)
	}
	if stable.GeneratedAt != "" {
		t.Fatalf("Stable().GeneratedAt = %q, want stripped (volatile)", stable.GeneratedAt)
	}
	raw, err := rep.StableJSON()
	if err != nil {
		t.Fatal(err)
	}
	wantField := `"source_revision": "` + rep.SourceRevision + `"`
	if !strings.Contains(string(raw), wantField) {
		t.Fatalf("StableJSON does not carry %s:\n%s", wantField, raw)
	}
	only := rep.ResultOnly()
	if only.SourceRevision != "" {
		t.Fatalf("ResultOnly().SourceRevision = %q, want explicitly stripped for the cross-revision measurement comparison", only.SourceRevision)
	}
	rawOnly, err := rep.ResultOnlyJSON()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawOnly), "source_revision") {
		t.Fatal("ResultOnlyJSON still carries source_revision")
	}
}
