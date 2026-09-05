package hookcli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The tests in this file pin the C05 namespace-alignment contract: the
// namespace the hooks capture into for a repository and the namespace an
// MCP session resolves must agree, for BOTH client postures - clients
// that advertise workspace roots (editors) and clients that cannot
// (Codex's native MCP client). The production mismatch that motivated
// this was a checkout whose hooks captured into the remote-derived
// namespace while a roots-less MCP session read the server default, so
// everything captured was invisible to the agent. Fixtures here are
// sanitized stand-ins (acme/punk-records), never real user paths.

// alignmentWhoamiServer fakes the MCP server's namespace resolution with
// the same precedence internal/mcpserver implements: an X-Punk-Namespace
// header wins (punk connect --project bakes it into the MCP entry), then
// the first file:// workspace root mapped with the hook-side basename
// rule, then the server default. It answers over streamable HTTP so the
// real VerifyMCP client path - including its roots advertisement - is
// exercised without a database.
func alignmentWhoamiServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "punk-records", Version: "test"}, &mcp.ServerOptions{Instructions: "inst"})
	type out struct {
		Namespace string `json:"namespace"`
		Source    string `json:"source"`
		Root      string `json:"root,omitempty"`
	}
	mcp.AddTool(srv, &mcp.Tool{Name: "whoami", Description: "who"},
		func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, out, error) {
			if req.Extra != nil && req.Extra.Header != nil {
				if h := strings.TrimSpace(req.Extra.Header.Get("X-Punk-Namespace")); h != "" {
					return nil, out{Namespace: h, Source: "header"}, nil
				}
			}
			if res, err := req.Session.ListRoots(ctx, nil); err == nil {
				for _, r := range res.Roots {
					if u, perr := url.Parse(r.URI); perr == nil && u.Scheme == "file" && u.Path != "" {
						p := filepath.Clean(u.Path)
						return nil, out{Namespace: agentNamespaceForPath(p), Source: "roots", Root: p}, nil
					}
				}
			}
			return nil, out{Namespace: "agent-default", Source: "default"}, nil
		})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts
}

// gitRepoWithRemote returns a temporary git checkout with origin set, so
// ProjectNamespace derives from the remote exactly like a real clone.
func gitRepoWithRemote(t *testing.T, remote string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init", "-q")
	run("remote", "add", "origin", remote)
	return dir
}

func absPath(t *testing.T, dir string) string {
	t.Helper()
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// TestCheckNamespaceAlignmentConflictingPinsAreNotServerFailure is the
// C05 review fix: when the hook command and the MCP entry pin DIFFERENT
// namespaces and whoami answers with exactly the MCP pin via the header,
// the server applied that pin - the mismatch is a local conflict between
// the two installed pins, so verify must name both pins and must not
// blame the server (no "does not honor X-Punk-Namespace", no upgrade
// advice). Only a pin the server provably ignored justifies that.
func TestCheckNamespaceAlignmentConflictingPinsAreNotServerFailure(t *testing.T) {
	ts := alignmentWhoamiServer(t)
	al, err := CheckNamespaceAlignment(context.Background(), ts.URL, "", t.TempDir(), "agent-hook", "agent-mcp")
	if err != nil {
		t.Fatal(err)
	}
	if !al.Mismatch() {
		t.Fatal("distinct explicit pins must mismatch")
	}
	warnings := strings.Join(al.Warnings(), "\n")
	if strings.Contains(warnings, "server does not honor") || strings.Contains(warnings, "upgrade it") {
		t.Fatalf("server correctly honored the agent-mcp header; diagnostics must not blame it:\n%s", warnings)
	}
	for _, want := range []string{"agent-hook", "agent-mcp"} {
		if !strings.Contains(warnings, want) {
			t.Fatalf("the conflicting-pins remediation must name %q:\n%s", want, warnings)
		}
	}
}

// TestCheckNamespaceAlignmentIgnoredPinBlamesTheServer is the complement:
// when the MCP entry pins a namespace the server never resolves (whoami
// falls back to the default even with the header), the server ignored
// X-Punk-Namespace - the remediation names the MCP pin (kept on the
// alignment as MCPPin, distinct from the hook-side Capture) and
// recommends the upgrade.
func TestCheckNamespaceAlignmentIgnoredPinBlamesTheServer(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "punk-records", Version: "test"}, nil)
	type out struct {
		Namespace string `json:"namespace"`
		Source    string `json:"source"`
	}
	mcp.AddTool(srv, &mcp.Tool{Name: "whoami", Description: "who"},
		func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, out, error) {
			// A server without header support: always the default.
			return nil, out{Namespace: "agent-default", Source: "default"}, nil
		})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	ts := httptest.NewServer(h)
	defer ts.Close()

	al, err := CheckNamespaceAlignment(context.Background(), ts.URL, "", t.TempDir(), "agent-pinned-1a2b3c", "agent-pinned-1a2b3c")
	if err != nil {
		t.Fatal(err)
	}
	if !al.Mismatch() {
		t.Fatal("a pin the server ignores must mismatch")
	}
	warnings := strings.Join(al.Warnings(), "\n")
	if !strings.Contains(warnings, "server does not honor X-Punk-Namespace") || !strings.Contains(warnings, "agent-pinned-1a2b3c") {
		t.Fatalf("an ignored pin must be named and blamed on the server:\n%s", warnings)
	}
}

// TestCheckNamespaceAlignmentNoRootsMismatch is the red proof: hooks
// pinned to the remote-derived namespace (a project hook install) while
// the MCP entry is global and the client cannot advertise roots. whoami
// falls back to the server default, so captures and retrieval disagree;
// verify must detect and report exactly that pair.
func TestCheckNamespaceAlignmentNoRootsMismatch(t *testing.T) {
	ts := alignmentWhoamiServer(t)
	repo := gitRepoWithRemote(t, "git@github.com:acme/punk-records.git")
	hookNS, src := ProjectNamespace(repo)
	if src != "remote" || !strings.HasPrefix(hookNS, "agent-punk-records-") {
		t.Fatalf("fixture must derive from the remote: %s (%s)", hookNS, src)
	}

	// Mixed install: hook side pinned, MCP side unpinned.
	al, err := CheckNamespaceAlignment(context.Background(), ts.URL, "", repo, hookNS, "")
	if err != nil {
		t.Fatal(err)
	}
	if al.Capture != hookNS || al.CaptureSource != "pin" {
		t.Fatalf("capture = %s (%s), want the pinned %s", al.Capture, al.CaptureSource, hookNS)
	}
	if al.NoRoots.Namespace != "agent-default" || al.NoRoots.Source != "default" {
		t.Fatalf("roots-less whoami must fall back to the default: %+v", al.NoRoots)
	}
	if want := agentNamespaceForPath(absPath(t, repo)); al.Roots.Namespace != want || al.Roots.Source != "roots" {
		t.Fatalf("roots whoami = %+v, want %s from roots", al.Roots, want)
	}
	if !al.Mismatch() {
		t.Fatalf("capture %s vs MCP %s/%s must be flagged: %+v", al.Capture, al.Roots.Namespace, al.NoRoots.Namespace, al)
	}
	warnings := strings.Join(al.Warnings(), "\n")
	for _, want := range []string{hookNS, "agent-default", "--project", "nothing is copied or deleted"} {
		if !strings.Contains(warnings, want) {
			t.Fatalf("warnings must name %q:\n%s", want, warnings)
		}
	}
	if al.Pinned {
		t.Fatal("an unpinned MCP entry must be reported as unpinned")
	}
}

// TestCheckNamespaceAlignmentProjectConnection: a project connection
// bakes the same remote-derived namespace into the hook command and the
// MCP entry header, so whoami resolves it identically with and without
// roots - this is the state the mismatch warning sends the user to.
func TestCheckNamespaceAlignmentProjectConnection(t *testing.T) {
	ts := alignmentWhoamiServer(t)
	repo := gitRepoWithRemote(t, "git@github.com:acme/punk-records.git")
	pin, src := ProjectNamespace(repo)
	if src != "remote" {
		t.Fatalf("fixture must derive from the remote: %s (%s)", pin, src)
	}
	al, err := CheckNamespaceAlignment(context.Background(), ts.URL, "", repo, pin, pin)
	if err != nil {
		t.Fatal(err)
	}
	if al.Mismatch() {
		t.Fatalf("project connection must align both postures: %+v\n%s", al, strings.Join(al.Warnings(), "\n"))
	}
	for _, rep := range []VerifyReport{al.Roots, al.NoRoots} {
		if rep.Namespace != pin || rep.Source != "header" {
			t.Fatalf("pinned whoami = %+v, want %s via header", rep, pin)
		}
	}
	if !al.Pinned {
		t.Fatal("a connection carrying X-Punk-Namespace is pinned")
	}
	if len(al.Warnings()) != 0 {
		t.Fatalf("aligned connection must not warn: %s", strings.Join(al.Warnings(), "\n"))
	}
}

// TestCheckNamespaceAlignmentGlobalConnection: a global connection pins
// nothing. Roots-capable clients resolve the same path-derived namespace
// the unpinned hooks capture into, so that posture aligns; roots-less
// clients (Codex native MCP) fall back to the default and must be
// flagged - with guidance towards a per-repository project connection,
// never towards pinning every repository to the connector's cwd.
func TestCheckNamespaceAlignmentGlobalConnection(t *testing.T) {
	ts := alignmentWhoamiServer(t)
	for _, remote := range []string{"git@github.com:acme/billing.git", "git@github.com:acme/punk-records.git"} {
		repo := gitRepoWithRemote(t, remote)
		al, err := CheckNamespaceAlignment(context.Background(), ts.URL, "", repo, "", "")
		if err != nil {
			t.Fatal(err)
		}
		wantCapture := agentNamespaceForPath(absPath(t, repo))
		if al.Capture != wantCapture || al.CaptureSource != "path" {
			t.Fatalf("unpinned hooks capture the path-derived namespace: %s (%s), want %s (path)", al.Capture, al.CaptureSource, wantCapture)
		}
		if al.Roots.Namespace != al.Capture || al.Roots.Source != "roots" {
			t.Fatalf("roots clients agree with unpinned hooks: %+v vs capture %s", al.Roots, al.Capture)
		}
		if al.NoRoots.Namespace != "agent-default" || !al.Mismatch() {
			t.Fatalf("roots-less fallback must be flagged: %+v", al)
		}
		warnings := strings.Join(al.Warnings(), "\n")
		if !strings.Contains(warnings, "--project") || !strings.Contains(warnings, "agent-default") {
			t.Fatalf("global-connection guidance must point at a per-repo project pin:\n%s", warnings)
		}
		if strings.Contains(warnings, "PUNK_NAMESPACE") {
			t.Fatalf("guidance must not pin every repo to one namespace via the environment:\n%s", warnings)
		}
	}
}

// TestProjectNamespaceClonesAndUnrelatedRepos: two clones of one
// repository derive the same namespace however their remote URLs are
// written (that is what makes the remote-derived pin stable across
// machines), and two unrelated repositories never collide.
func TestProjectNamespaceClonesAndUnrelatedRepos(t *testing.T) {
	clone1 := gitRepoWithRemote(t, "git@github.com:acme/billing.git")
	clone2 := gitRepoWithRemote(t, "https://github.com/Acme/Billing")
	ns1, src1 := ProjectNamespace(clone1)
	ns2, src2 := ProjectNamespace(clone2)
	if src1 != "remote" || src2 != "remote" || ns1 != ns2 {
		t.Fatalf("two clones of one repo must agree: %s (%s) vs %s (%s)", ns1, src1, ns2, src2)
	}
	other := gitRepoWithRemote(t, "git@github.com:acme/other.git")
	if ns3, _ := ProjectNamespace(other); ns3 == ns1 {
		t.Fatalf("unrelated repos must not share a namespace: %s", ns3)
	}

	// A pinned connection resolves both clones to the identical
	// namespace, which is the whole point of the remote-derived pin.
	ts := alignmentWhoamiServer(t)
	for _, clone := range []string{clone1, clone2} {
		al, err := CheckNamespaceAlignment(context.Background(), ts.URL, "", clone, ns1, ns1)
		if err != nil {
			t.Fatal(err)
		}
		if al.Mismatch() || al.NoRoots.Namespace != ns1 || al.Roots.Namespace != ns1 {
			t.Fatalf("clone %s must resolve the pinned namespace on both postures: %+v", clone, al)
		}
	}
}

// TestVerifyMCPNamespaceHeader: verifyMCP carries the pinned namespace
// header for project connections, and whoami's source distinguishes that
// explicit override (header) from an accidental fallback (default) and
// from roots-derived resolution.
func TestVerifyMCPNamespaceHeader(t *testing.T) {
	ts := alignmentWhoamiServer(t)
	repo := t.TempDir()
	for _, c := range []struct {
		name       string
		cwd, pin   string
		wantNS     string
		wantSource string
	}{
		{"pinned without roots", "", "agent-billing-1a2b3c", "agent-billing-1a2b3c", "header"},
		{"pinned with roots", repo, "agent-billing-1a2b3c", "agent-billing-1a2b3c", "header"},
		{"unpinned with roots", repo, "", agentNamespaceForPath(absPath(t, repo)), "roots"},
		{"unpinned without roots", "", "", "agent-default", "default"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rep, err := verifyMCP(context.Background(), ts.URL, "", c.cwd, c.pin)
			if err != nil {
				t.Fatal(err)
			}
			if rep.Namespace != c.wantNS || rep.Source != c.wantSource {
				t.Fatalf("whoami = %s (%s), want %s (%s)", rep.Namespace, rep.Source, c.wantNS, c.wantSource)
			}
		})
	}
}

// TestCheckNamespaceAlignmentCapturesLayeredScopes is the round-3 fix
// at the alignment boundary: Codex appends hook registrations from
// every applicable config layer, so a layered install (a global
// UNPINNED registration next to a pinned project one) captures into
// TWO namespaces and both must surface. The alignment is a mismatch
// even though the MCP pin matches ONE of the destinations - the other
// destination's memories land where retrieval never reads - and the
// warnings name every namespace and every file. Identical destinations
// from two files, by contrast, merge into one and stay aligned.
func TestCheckNamespaceAlignmentCapturesLayeredScopes(t *testing.T) {
	ts := alignmentWhoamiServer(t)
	repo := gitRepoWithRemote(t, "git@github.com:acme/punk-records.git")
	pin, src := ProjectNamespace(repo)
	if src != "remote" {
		t.Fatalf("fixture must derive from the remote: %s (%s)", pin, src)
	}
	captures := []CaptureDestination{
		{Source: "path", Origin: "global hooks.json"},
		{Namespace: pin, Source: "pin", Origin: "project hooks.json"},
	}
	al, err := CheckNamespaceAlignmentCaptures(context.Background(), ts.URL, "", repo, captures, pin)
	if err != nil {
		t.Fatal(err)
	}
	if !al.Mismatch() {
		t.Fatalf("a split capture must mismatch even though one destination matches retrieval: %+v", al)
	}
	if al.Capture != "" {
		t.Fatalf("disagreeing destinations have no single capture namespace: %q", al.Capture)
	}
	warnings := strings.Join(al.Warnings(), "\n")
	for _, want := range []string{pin, agentNamespaceForPath(absPath(t, repo)), "global hooks.json", "project hooks.json", "nothing is copied or deleted"} {
		if !strings.Contains(warnings, want) {
			t.Fatalf("warnings must name %q:\n%s", want, warnings)
		}
	}
	if strings.Contains(warnings, "server does not honor") || strings.Contains(warnings, "upgrade it") {
		t.Fatalf("the server honored the pin; the split is local to the installed layers:\n%s", warnings)
	}

	// Two registrations capturing identically (same pin, two files) are
	// ONE destination: aligned, with both origins named.
	al, err = CheckNamespaceAlignmentCaptures(context.Background(), ts.URL, "", repo, []CaptureDestination{
		{Namespace: pin, Source: "pin", Origin: "scope-a hooks.json"},
		{Namespace: pin, Source: "pin", Origin: "scope-b hooks.json"},
	}, pin)
	if err != nil {
		t.Fatal(err)
	}
	if al.Mismatch() || al.Capture != pin || al.CaptureSource != "pin" {
		t.Fatalf("identical destinations must merge and align: %+v", al)
	}
	if len(al.Captures) != 1 || al.Captures[0].Origin != "scope-a hooks.json and scope-b hooks.json" {
		t.Fatalf("merged destination = %+v", al.Captures)
	}
	if len(al.Warnings()) != 0 {
		t.Fatalf("merged aligned destinations must not warn: %s", strings.Join(al.Warnings(), "\n"))
	}
}
