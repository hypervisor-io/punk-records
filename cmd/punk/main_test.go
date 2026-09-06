package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/membench"
	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/registry"
	"github.com/hypervisor-io/punk-records/internal/store"
)

func TestOpenCodePathsGlobalUsesOpencodeDir(t *testing.T) {
	plugin, cfg := openCodePaths(false, "", "/home/u")
	if cfg != "/home/u/.config/opencode/opencode.json" {
		t.Fatalf("config path = %s", cfg)
	}
	if plugin != "/home/u/.config/opencode/plugins/punk-memory.js" {
		t.Fatalf("plugin path = %s", plugin)
	}
	_, cfg = openCodePaths(false, "/xdg", "/home/u")
	if cfg != "/xdg/opencode/opencode.json" {
		t.Fatalf("xdg config path = %s", cfg)
	}
	plugin, cfg = openCodePaths(true, "/xdg", "/home/u")
	if plugin != ".opencode/plugins/punk-memory.js" || cfg != "opencode.json" {
		t.Fatalf("project paths = %s %s", plugin, cfg)
	}
}

// The membench CLI tests below cover the E01 review fixes at the
// command layer: --report must record honest code-state provenance
// (source_revision: the binary's own embedded VCS revision, "+modified"
// when the build tree was dirty, or explicit "unknown" - never the
// "dev" build stamp, and never an unrelated invocation repository's
// HEAD), separate from the --commit build label; and a failing
// --reranker-url must degrade the rerank ablation to recorded baseline
// fallback instead of reporting an available successful rerank.

// sourceRevisionRe matches a resolved provenance value: a 40-hex git
// SHA, optionally suffixed with the +modified build-tree marker.
var sourceRevisionRe = regexp.MustCompile(`^[0-9a-f]{40}(\+modified)?$`)

func baselineScenarioPath() string {
	return filepath.Join("..", "..", "scenarios", "membench", "baseline.jsonl")
}

// runMembenchReport runs "punk membench --file <file> --report <tmp>"
// with extra args and returns the parsed report artifact.
func runMembenchReport(t *testing.T, file string, extraArgs ...string) membench.Report {
	t.Helper()
	reportPath := filepath.Join(t.TempDir(), "report.json")
	args := append([]string{"--file", file, "--report", reportPath}, extraArgs...)
	if err := cmdMembench(args); err != nil {
		t.Fatalf("cmdMembench(%v): %v", args, err)
	}
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	var rep membench.Report
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatalf("report artifact does not parse: %v", err)
	}
	return rep
}

func findReportRun(t *testing.T, rep membench.Report, name string) membench.RunReport {
	t.Helper()
	for _, r := range rep.Runs {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("run %q missing from the report", name)
	return membench.RunReport{}
}

// TestMembenchReportRecordsSourceProvenance: the one-command report must
// carry exactly the binary's own build-embedded provenance - never a
// generic build stamp like "dev" presented as the reproducibility
// identity, and (r3) never a revision resolved from the invocation
// working directory. In a go test binary there is no VCS stamp, so the
// only honest value is explicit "unknown"; the stamped-binary contract
// is proven by TestMembenchBinaryProvenanceAcrossInvocationRepos below.
// The manifest's commit field stays the caller/build label, recorded
// separately from source_revision.
func TestMembenchReportRecordsSourceProvenance(t *testing.T) {
	rep := runMembenchReport(t, baselineScenarioPath())
	if rep.SourceRevision == "" || rep.SourceRevision == "dev" || rep.SourceRevision == version {
		t.Fatalf("source_revision = %q, want embedded VCS provenance or explicit \"unknown\", never a build stamp", rep.SourceRevision)
	}
	if rep.SourceRevision != membench.SourceRevisionUnknown && !sourceRevisionRe.MatchString(rep.SourceRevision) {
		t.Fatalf("source_revision = %q, want <40-hex>[+modified] or %q", rep.SourceRevision, membench.SourceRevisionUnknown)
	}
	if want := membench.BuildSourceRevision(); rep.SourceRevision != want {
		t.Fatalf("source_revision = %q, want exactly the binary's build info %q: the invocation repository must never be recorded as the source", rep.SourceRevision, want)
	}
	if rep.SourceRevision != membench.SourceRevisionUnknown {
		t.Fatalf("source_revision = %q in an unstamped go test binary, want explicit %q", rep.SourceRevision, membench.SourceRevisionUnknown)
	}
	if len(rep.Runs) == 0 || rep.Runs[0].Manifest == nil {
		t.Fatal("report has no baseline run manifest")
	}
	if rep.Runs[0].Manifest.Commit != version {
		t.Fatalf("manifest commit = %q, want the default build label %q, recorded separately from source_revision", rep.Runs[0].Manifest.Commit, version)
	}
}

// TestMembenchBinaryProvenanceAcrossInvocationRepos is the r3 runtime
// regression, mirroring the reviewer's proof: the SAME compiled binary
// is run from (a) an unrelated git repository, (b) a directory with no
// repository at all, and (c) the build tree itself. In every case the
// report's source_revision must be exactly the revision embedded in the
// binary at build time - constant across invocation contexts and never
// the unrelated repository's HEAD - and the stable serialization must
// retain that identity. Builds the real binary with `go build` into a
// temp dir (go test binaries carry no VCS stamp); skipped in -short.
//
// The go tool stamps VCS info only when it detects the checkout by a
// .git DIRECTORY, so a binary built from a git *worktree* (where .git
// is a file) honestly carries no stamp. The test therefore reads the
// built binary's own stamp with `go version -m` and expects exactly
// that value - a real build-tree revision when stamped, explicit
// "unknown" when not - in every invocation context.
func TestMembenchBinaryProvenanceAcrossInvocationRepos(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the punk binary: skipped in -short")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available: cannot create the unrelated-repo fixture or read the build revision")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	gitEnv := append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	gitIn := func(dir string, args ...string) string {
		t.Helper()
		argv := append([]string{"-c", "safe.directory=*"}, args...)
		cmd := exec.Command("git", argv...)
		cmd.Dir = dir
		cmd.Env = gitEnv
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	bin := filepath.Join(t.TempDir(), "punk")
	build := exec.Command("go", "build", "-buildvcs=true", "-o", bin, "./cmd/punk")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/punk: %v\n%s", err, out)
	}

	// The expected source_revision is whatever the build actually and
	// verifiably embedded: vcs.revision (+ "+modified" when the build
	// tree was dirty), or explicit "unknown" when the go tool could not
	// stamp (e.g. worktree checkouts). Never derive the expectation
	// from the invocation directory.
	want := membench.SourceRevisionUnknown
	var stampRev string
	out, err := exec.Command("go", "version", "-m", bin).Output()
	if err != nil {
		t.Fatalf("go version -m %s: %v", bin, err)
	}
	stampModified := false
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "build\tvcs.revision="); ok {
			stampRev = v
		}
		if v, ok := strings.CutPrefix(line, "build\tvcs.modified="); ok {
			stampModified = v == "true"
		}
	}
	if stampRev != "" {
		want = stampRev
		if stampModified {
			want += "+modified"
		}
		buildHead := gitIn(root, "rev-parse", "HEAD")
		if len(buildHead) != 40 {
			t.Fatalf("build tree HEAD = %q, want a 40-hex SHA to cross-check the embedded stamp", buildHead)
		}
		if stampRev != buildHead {
			t.Fatalf("embedded vcs.revision = %q, want the build tree HEAD %q", stampRev, buildHead)
		}
	} else {
		t.Logf("binary carries no VCS stamp (the go tool cannot stamp worktree checkouts, whose .git is a file): expecting explicit %q everywhere", want)
	}

	// The reviewer's fixture: a git repository with no Punk source.
	unrelated := t.TempDir()
	gitIn(unrelated, "init", "-q")
	if err := os.WriteFile(filepath.Join(unrelated, "README"), []byte("unrelated repository without Punk source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(unrelated, "add", "README")
	gitIn(unrelated, "commit", "-qm", "unrelated fixture")
	unrelatedHead := gitIn(unrelated, "rev-parse", "HEAD")
	if unrelatedHead == stampRev {
		t.Fatal("fixture collision: unrelated repo HEAD equals the embedded revision")
	}

	fixture := filepath.Join(root, "scenarios", "membench", "baseline.jsonl")
	runReport := func(cwd string) membench.Report {
		t.Helper()
		outPath := filepath.Join(t.TempDir(), "out.json")
		cmd := exec.Command(bin, "membench", "--file", fixture, "--report", outPath)
		cmd.Dir = cwd
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("membench --report from %s: %v\n%s", cwd, err, out)
		}
		raw, err := os.ReadFile(outPath)
		if err != nil {
			t.Fatal(err)
		}
		var rep membench.Report
		if err := json.Unmarshal(raw, &rep); err != nil {
			t.Fatalf("report from %s does not parse: %v", cwd, err)
		}
		return rep
	}

	reps := map[string]membench.Report{
		"unrelated-repo": runReport(unrelated),
		"no-repo":        runReport(t.TempDir()),
		"build-tree":     runReport(root),
	}
	for name, rep := range reps {
		if rep.SourceRevision != want {
			t.Fatalf("%s: source_revision = %q, want exactly the binary's embedded stamp %q: the invocation repository must never be recorded as the source", name, rep.SourceRevision, want)
		}
		if stampRev == "" && rep.SourceRevision != membench.SourceRevisionUnknown {
			t.Fatalf("%s: source_revision = %q from an unstamped binary, want explicit %q", name, rep.SourceRevision, membench.SourceRevisionUnknown)
		}
		if strings.HasPrefix(rep.SourceRevision, unrelatedHead) {
			t.Fatalf("%s: source_revision = %q records the unrelated invocation repository's HEAD as the source (r3 finding)", name, rep.SourceRevision)
		}
	}

	stable, err := reps["unrelated-repo"].StableJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stable), `"source_revision": "`+want+`"`) {
		t.Fatalf("stable serialization lost the source identity %q: provenance is part of the reproducibility claim", want)
	}
	if strings.Contains(string(stable), unrelatedHead) {
		t.Fatal("stable serialization contains the unrelated invocation repository's HEAD")
	}
}

// TestMembenchReportRerankerFailureIsDegraded: --report with a failing
// --reranker-url must record the rerank ablation as degraded baseline
// fallback (r1 finding 1 at the CLI layer): memory.applyRerank swallows
// the reranker error and serves the baseline ranking, so the report must
// expose the fallback per query, in the summary count and in the run
// reason - never as an available successful rerank with Failed=0 and no
// failure/fallback evidence.
func TestMembenchReportRerankerFailureIsDegraded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "reranker down", http.StatusInternalServerError)
	}))
	defer srv.Close()

	rep := runMembenchReport(t, baselineScenarioPath(), "--reranker-url", srv.URL)
	rr := findReportRun(t, rep, "rerank")
	if !rr.Available || rr.Summary == nil || rr.Manifest == nil {
		t.Fatalf("rerank run = %+v, want available with manifest and summary", rr)
	}
	if rr.Manifest.RerankerID != srv.URL || rr.Manifest.Strategy != "hybrid-reranked" {
		t.Fatalf("manifest = %+v, want reranker_id=%s strategy=hybrid-reranked", *rr.Manifest, srv.URL)
	}
	if rr.Reason == "" {
		t.Fatal("Reason = empty: a failed reranker must produce an explicit degradation/fallback reason")
	}
	if rr.Summary.Failed != 1 {
		t.Fatalf("Failed = %d, want 1 (only the whitespace query is a retrieval failure, orthogonal to degradation)", rr.Summary.Failed)
	}
	// Every query that returned candidates fell back; the no-match query
	// had nothing to rerank (neutral) and the whitespace query errored.
	if rr.Summary.RerankDegraded != 7 {
		t.Fatalf("RerankDegraded = %d, want 7 (every query with candidates served baseline fallback)", rr.Summary.RerankDegraded)
	}
	flagged := 0
	for _, q := range rr.Queries {
		if q.RerankDegraded {
			flagged++
		}
	}
	if flagged != rr.Summary.RerankDegraded {
		t.Fatalf("per-query degraded flags = %d, want the summary count %d", flagged, rr.Summary.RerankDegraded)
	}
}

// TestMembenchReportRerankerSuccessApplies is the other direction at the
// CLI layer: a working TEI-shape --reranker-url endpoint must observably
// apply - the rerank ablation reports no degradation and its ranking is
// the cross-encoder's order (ascending scores reverse the candidate
// order), not the baseline's.
func TestMembenchReportRerankerSuccessApplies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		out := make([]struct {
			Index int     `json:"index"`
			Score float64 `json:"score"`
		}, 50) // applyRerank sends at most rerankTopK=50 bodies; extra indices are ignored
		for i := range out {
			out[i].Index, out[i].Score = i, float64(i)
		}
		if err := json.NewEncoder(w).Encode(out); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()

	// Lopsided token frequency keeps the pre-rerank candidate order
	// stable across the cold baseline and the warm rerank run (the
	// access-count salience drift is far below the bm25 gap), so the
	// reversal is exact.
	scenario := filepath.Join(t.TempDir(), "scenario.jsonl")
	content := `{"type":"fact","key":"/svc/db","body":"postgres postgres postgres primary host7"}
{"type":"fact","key":"/svc/cache","body":"postgres client connections redis cache"}
{"type":"query","q":"postgres","expect":["/svc/db"]}
`
	if err := os.WriteFile(scenario, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	rep := runMembenchReport(t, scenario, "--reranker-url", srv.URL)
	base := findReportRun(t, rep, "baseline")
	if got := base.Queries[0].Ranking; len(got) != 2 || got[0] != "/svc/db" || got[1] != "/svc/cache" {
		t.Fatalf("baseline ranking = %v, want [/svc/db /svc/cache]", got)
	}
	rr := findReportRun(t, rep, "rerank")
	if !rr.Available || rr.Summary == nil {
		t.Fatalf("rerank run = %+v, want available with a summary", rr)
	}
	if rr.Reason != "" {
		t.Fatalf("Reason = %q, want empty: the reranker demonstrably ran", rr.Reason)
	}
	if rr.Summary.RerankDegraded != 0 || rr.Queries[0].RerankDegraded {
		t.Fatalf("summary %+v / query %+v, want no degradation on a successful rerank", *rr.Summary, rr.Queries[0])
	}
	if got := rr.Queries[0].Ranking; len(got) != 2 || got[0] != "/svc/cache" || got[1] != "/svc/db" {
		t.Fatalf("rerank ranking = %v, want [/svc/cache /svc/db]: ascending cross-encoder scores must reverse the baseline order", got)
	}
}

// The serve loop's skill-index reload lifecycle: while
// syncSkillsOnSpecChange runs, every spec snapshot the registry
// activates is republished into the skill namespace - new skills become
// discoverable and deleted ones are swept, with no server restart.
func TestSyncSkillsOnSpecChangeTracksRegistry(t *testing.T) {
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "skillsync.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if _, err := db.MigrateUp(ctx); err != nil {
		t.Fatal(err)
	}
	specDir := t.TempDir()
	reg := registry.New(specDir, db, slog.New(slog.DiscardHandler))
	if err := reg.Load(ctx); err != nil {
		t.Fatal(err)
	}
	mem := memory.New(db, nil)
	log := slog.New(slog.DiscardHandler)

	runCtx, stop := context.WithCancel(ctx)
	t.Cleanup(stop)
	done := make(chan struct{})
	go func() {
		defer close(done)
		syncSkillsOnSpecChange(runCtx, log, mem, reg, "agent-default", 5*time.Millisecond)
	}()

	writeSkill := func(content string) {
		t.Helper()
		dir := filepath.Join(specDir, "skills", "reload-runbook")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := reg.Load(ctx); err != nil {
			t.Fatal(err)
		}
	}
	eventually := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if cond() {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	discoverable := func() bool {
		hits, err := mem.SearchSkills(ctx, "agent-default", "reload sentinel", 0)
		return err == nil && len(hits) == 1 && hits[0].Name == "reload-runbook"
	}

	// A skill added to the tree and loaded (the hot-reload path) is
	// indexed into the mapped namespace without a restart.
	writeSkill("---\nname: reload-runbook\ndescription: Inspect reload sentinel diagnostics\nmetadata:\n  version: 0.1.0\n---\n\nFirst body.\n")
	eventually("authored skill to become discoverable after snapshot advance", discoverable)

	// Deleting it from the tree sweeps it on the next snapshot advance.
	if err := os.RemoveAll(filepath.Join(specDir, "skills", "reload-runbook")); err != nil {
		t.Fatal(err)
	}
	if err := reg.Load(ctx); err != nil {
		t.Fatal(err)
	}
	eventually("deleted skill to be swept after snapshot advance", func() bool {
		hits, err := mem.SearchSkills(ctx, "agent-default", "reload sentinel", 0)
		return err == nil && len(hits) == 0
	})

	stop()
	<-done
}
