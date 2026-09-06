package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hypervisor-io/punk-records/internal/authz"
	"github.com/hypervisor-io/punk-records/internal/memory"
)

// Task S01 MCP surface: search_skills returns metadata only (never
// procedure text), load_skill returns the exact versioned body, both
// honor the A02 namespace gate, and the discovery payload stays inside
// a measured size bound.

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s: tool error: %s", name, text(t, res))
	}
	return text(t, res)
}

func TestSearchSkillsToolMetadataOnly(t *testing.T) {
	cs, mem := sessionWithStore(t, nil)
	ctx := context.Background()
	meta := memory.SkillMeta{
		Name:        "db-connection-triage",
		Version:     "0.1.0",
		Description: "Diagnose database connection saturation and pool exhaustion",
		Tools:       []string{"incidents__get_incident"},
		Scope:       "incident",
		Source:      memory.SkillSourceAuthored,
		Active:      true,
	}
	if err := mem.IndexSkill(ctx, "agent-default", meta, "# Triage\n\nprocedure MCP-SENTINEL-7 text"); err != nil {
		t.Fatal(err)
	}

	out := callTool(t, cs, "search_skills", map[string]any{"query": "connection saturation"})
	if strings.Contains(out, "MCP-SENTINEL-7") {
		t.Fatalf("search_skills leaked procedure text: %s", out)
	}
	var parsed struct {
		Skills []memory.SkillMeta `json:"skills"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("parse: %v\n%s", err, out)
	}
	if len(parsed.Skills) != 1 || parsed.Skills[0].Name != meta.Name || parsed.Skills[0].Version != meta.Version {
		t.Fatalf("skills: %+v", parsed.Skills)
	}

	// Missing collection on another namespace is a normal empty result.
	out = callTool(t, cs, "search_skills", map[string]any{"namespace": "elsewhere", "query": "connection saturation"})
	if !strings.Contains(out, `"skills":[]`) && !strings.Contains(out, `"skills": []`) {
		t.Fatalf("empty namespace result: %s", out)
	}

	body := callTool(t, cs, "load_skill", map[string]any{"name": meta.Name, "version": meta.Version})
	if !strings.Contains(body, "MCP-SENTINEL-7") {
		t.Fatalf("load_skill missing body: %s", body)
	}
	var loaded struct {
		Skill memory.SkillMeta `json:"skill"`
		Body  string           `json:"body"`
	}
	if err := json.Unmarshal([]byte(body), &loaded); err != nil {
		t.Fatalf("parse load: %v\n%s", err, body)
	}
	if loaded.Body != "# Triage\n\nprocedure MCP-SENTINEL-7 text" {
		t.Fatalf("loaded body: %q", loaded.Body)
	}
	if loaded.Skill.Name != meta.Name {
		t.Fatalf("loaded meta: %+v", loaded.Skill)
	}
}

// TestSearchSkillsDefaultsToSkillIndexNamespace is the 2b red proof:
// the skill catalog is published into Deps.SkillIndexNamespace()
// (agent-default here), but a session's roots resolve omitted-namespace
// calls to the caller's workspace (agent-x). Before the fix, an omitted
// namespace routed through the same roots resolution as every other
// tool and found nothing; search_skills and load_skill must instead
// target the skill index namespace by default, while an explicit
// namespace argument keeps the normal roots-based resolution.
func TestSearchSkillsDefaultsToSkillIndexNamespace(t *testing.T) {
	cs, mem := sessionWithStore(t, func(c *mcp.Client) {
		c.AddRoots(&mcp.Root{URI: "file:///work/x", Name: "ws"})
	})
	ctx := context.Background()
	meta := memory.SkillMeta{
		Name:        "index-default-skill",
		Version:     "1.0.0",
		Description: "lives in the skill index namespace, not the caller's workspace root",
		Source:      memory.SkillSourceAuthored,
		Active:      true,
	}
	if err := mem.IndexSkill(ctx, "agent-default", meta, "the procedure body"); err != nil {
		t.Fatal(err)
	}

	// Confirm the session's roots really do resolve to agent-x, so a
	// miss below can only be explained by the namespace routing.
	whoamiOut := callTool(t, cs, "whoami", map[string]any{})
	if !strings.Contains(whoamiOut, `"namespace":"agent-x"`) {
		t.Fatalf("whoami = %s, want roots resolving to agent-x", whoamiOut)
	}

	out := callTool(t, cs, "search_skills", map[string]any{"query": "skill index namespace"})
	if !strings.Contains(out, meta.Name) {
		t.Fatalf("empty-namespace search_skills missed the skill index namespace: %s", out)
	}
	loaded := callTool(t, cs, "load_skill", map[string]any{"name": meta.Name, "version": meta.Version})
	if !strings.Contains(loaded, "the procedure body") {
		t.Fatalf("empty-namespace load_skill missed the skill index namespace: %s", loaded)
	}

	// An explicit namespace still resolves normally (here: agent-x, which
	// never had the skill indexed) and must not fall back to the index.
	out = callTool(t, cs, "search_skills", map[string]any{"namespace": "agent-x", "query": "skill index namespace"})
	if strings.Contains(out, meta.Name) {
		t.Fatalf("explicit agent-x search_skills leaked the skill index namespace: %s", out)
	}
}

func TestSkillToolsEnforceNamespaceGrant(t *testing.T) {
	g := mcpAuthzRigNew(t, true)
	ctx := context.Background()
	// A skill exists only in ns-b, which alice (ns-a read+write) must
	// never discover or load through the MCP surface.
	if err := g.mem.IndexSkill(ctx, "ns-b", memory.SkillMeta{
		Name: "secret-procedure", Version: "1.0.0",
		Description: "hidden from alice",
		Source:      memory.SkillSourceAuthored, Active: true,
	}, "classified body"); err != nil {
		t.Fatal(err)
	}
	cs := g.connect(t)

	if out, denied, _ := call(t, cs, "search_skills", map[string]any{"namespace": "ns-b", "query": "hidden"}); !denied {
		t.Fatalf("search_skills on ns-b: denied=%v out=%s", denied, out)
	}
	if out, denied, _ := call(t, cs, "load_skill", map[string]any{"namespace": "ns-b", "name": "secret-procedure", "version": "1.0.0"}); !denied {
		t.Fatalf("load_skill on ns-b: denied=%v out=%s", denied, out)
	}
	// Positive controls on the granted namespace.
	if out, denied, _ := call(t, cs, "search_skills", map[string]any{"namespace": "ns-a", "query": "hidden"}); denied {
		t.Fatalf("search_skills on ns-a: %s", out)
	}
	if out, denied, _ := call(t, cs, "load_skill", map[string]any{"namespace": "ns-a", "name": "nope", "version": "1"}); denied {
		t.Fatalf("load_skill on ns-a: %s", out)
	}
}

// TestSkillToolsEmptyNamespaceRequiresIndexGrant pins finding I1: the
// empty-namespace branch in skillToolNamespace targets
// Deps.SkillIndexNamespace() (agent-default in this rig) and authorizes
// there, so a subject with grants elsewhere (alice: ns-a read+write
// only, nothing on agent-default) must still be denied when it omits
// namespace - the same "namespace grant required" shape as an explicit
// ungranted namespace, not a silent fallback that skips authorization.
func TestSkillToolsEmptyNamespaceRequiresIndexGrant(t *testing.T) {
	g := mcpAuthzRigNew(t, true)
	cs := g.connect(t)

	if out, denied, _ := call(t, cs, "search_skills", map[string]any{"query": "anything"}); !denied {
		t.Fatalf("empty-namespace search_skills without an index grant: denied=%v out=%s", denied, out)
	}
	if out, denied, _ := call(t, cs, "load_skill", map[string]any{"name": "anything", "version": "1"}); !denied {
		t.Fatalf("empty-namespace load_skill without an index grant: denied=%v out=%s", denied, out)
	}
}

// TestSkillToolsEmptyNamespaceGrantIgnoresRoots pins the other half of
// I1: once the subject holds read on the skill index namespace, an
// empty-namespace call succeeds regardless of what the client's
// workspace roots would otherwise resolve to - the roots-derived
// namespace ("agent-ns-b" here) carries no grant at all, proving
// skillToolNamespace never routes an omitted namespace through
// roots-based resolution the way every other tool does.
func TestSkillToolsEmptyNamespaceGrantIgnoresRoots(t *testing.T) {
	g := mcpAuthzRigNew(t, true)
	ctx := context.Background()
	if err := g.az.Grant(ctx, "alice", "agent-default", authz.OpRead); err != nil {
		t.Fatal(err)
	}
	if err := g.mem.IndexSkill(ctx, "agent-default", memory.SkillMeta{
		Name: "index-visible-procedure", Version: "1.0.0",
		Description: "found via the skill index grant",
		Source:      memory.SkillSourceAuthored, Active: true,
	}, "index body"); err != nil {
		t.Fatal(err)
	}
	// Roots resolve to agent-ns-b (see api.AgentNamespace), a namespace
	// alice holds no grant on whatsoever.
	cs := g.connectOpts(t, nil, []string{"file:///work/ns-b"}, nil)

	out, denied, _ := call(t, cs, "search_skills", map[string]any{"query": "index grant"})
	if denied {
		t.Fatalf("empty-namespace search_skills with an index grant was denied: %s", out)
	}
	if !strings.Contains(out, "index-visible-procedure") {
		t.Fatalf("empty-namespace search_skills missed the granted index namespace: %s", out)
	}
}

// The pinned ceiling itself lives with the enforcement in server.go
// (skillDiscoveryPayloadBound); the tests below pin the contract.

func TestSkillDiscoveryPayloadWithinBound(t *testing.T) {
	cs, mem := sessionWithStore(t, nil)
	ctx := context.Background()
	tool := strings.Repeat("t", 120)
	tools := make([]string, 16)
	for i := range tools {
		tools[i] = fmt.Sprintf("%s-%02d", tool, i)
	}
	for i := 0; i < memory.SkillDiscoveryMaxHits+5; i++ {
		if err := mem.IndexSkill(ctx, "agent-default", memory.SkillMeta{
			Name:        fmt.Sprintf("bench-%02d-%s", i, strings.Repeat("n", 50)),
			Version:     strings.Repeat("v", 60) + fmt.Sprintf("%04d", i),
			Description: "benchmark " + strings.Repeat("d", 1014),
			Tools:       tools,
			Scope:       strings.Repeat("s", 128),
			Source:      memory.SkillSourceAuthored,
			Active:      true,
		}, "body"); err != nil {
			t.Fatal(err)
		}
	}
	out := callTool(t, cs, "search_skills", map[string]any{"query": "benchmark", "limit": 100})
	t.Logf("worst-case search_skills payload: %d bytes at cap %d", len(out), memory.SkillDiscoveryMaxHits)
	if len(out) > skillDiscoveryPayloadBound {
		t.Fatalf("discovery payload = %d bytes, bound %d", len(out), skillDiscoveryPayloadBound)
	}
	var parsed struct {
		Skills []memory.SkillMeta `json:"skills"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Skills) != memory.SkillDiscoveryMaxHits {
		t.Fatalf("hits = %d, want cap %d", len(parsed.Skills), memory.SkillDiscoveryMaxHits)
	}
}

// Escaping is part of the bound: every '<' in valid maximal metadata
// becomes six bytes on the wire (encoding/json HTML escaping), which the
// per-field rune caps do not account for. The serialized response must
// still respect the pinned bound - hits are trimmed, never shipped over
// it, and never sliced mid-record. (Round-2 review reproducer, folded
// in.)
func TestSearchSkillsEscapedMetadataWithinPayloadBound(t *testing.T) {
	cs, mem := sessionWithStore(t, nil)
	ctx := context.Background()
	tools := make([]string, 16)
	for i := range tools {
		tools[i] = strings.Repeat("<", 128)
	}
	for i := 0; i < memory.SkillDiscoveryMaxHits; i++ {
		m := memory.SkillMeta{
			Name:        fmt.Sprintf("bench-%02d", i),
			Version:     "1",
			Description: "benchmark " + strings.Repeat("<", 1014),
			Tools:       tools,
			Scope:       strings.Repeat("<", 128),
			Source:      memory.SkillSourceAuthored,
			Active:      true,
		}
		if err := mem.IndexSkill(ctx, "agent-default", m, "procedure"); err != nil {
			t.Fatal(err)
		}
	}
	out := callTool(t, cs, "search_skills", map[string]any{"query": "benchmark", "limit": 20})
	if len(out) > skillDiscoveryPayloadBound {
		t.Fatalf("valid escaped metadata returned %d bytes, above advertised bound %d", len(out), skillDiscoveryPayloadBound)
	}
	var parsed struct {
		Skills []memory.SkillMeta `json:"skills"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("trimmed response must stay well-formed JSON: %v", err)
	}
	if len(parsed.Skills) == 0 || len(parsed.Skills) >= memory.SkillDiscoveryMaxHits {
		t.Fatalf("escaped worst case: hits = %d, want some but fewer than %d (trimmed)", len(parsed.Skills), memory.SkillDiscoveryMaxHits)
	}
	for _, sk := range parsed.Skills {
		if len(sk.Description) != 1024 || len(sk.Tools) != 16 || len(sk.Scope) != 128 {
			t.Fatalf("hit was sliced mid-record: %+v", sk)
		}
	}
}

// An authored SKILL.md loaded into the production registry before the
// MCP server is built is discoverable through search_skills and loadable
// through load_skill: New(Deps) runs the startup index sync, no manual
// IndexSkill seeding involved. (Round-2 review reproducer, folded in.)
func TestLoadedAuthoredSkillIsDiscoverable(t *testing.T) {
	cs, _ := sessionWithStore(t, nil, func(d *Deps) {
		dir := filepath.Join(d.Reg.Dir, "skills", "reviewer-runbook")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := "---\nname: reviewer-runbook\ndescription: Inspect reviewerfixture diagnostics\n---\n\nProcedure sentinel.\n"
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := d.Reg.Load(t.Context()); err != nil {
			t.Fatal(err)
		}
		if d.Reg.Current().Bundle.Skills["reviewer-runbook"] == nil {
			t.Fatal("fixture was not loaded into registry")
		}
	})
	out := callTool(t, cs, "search_skills", map[string]any{"query": "reviewerfixture"})
	if !strings.Contains(out, "reviewer-runbook") {
		t.Fatalf("authored SKILL.md loaded in production registry is invisible to search_skills: %s", out)
	}
	body := callTool(t, cs, "load_skill", map[string]any{"name": "reviewer-runbook"})
	if !strings.Contains(body, "Procedure sentinel.") {
		t.Fatalf("load_skill missing the authored procedure: %s", body)
	}
}

// The spec tree is the source of truth for the authored catalog: a skill
// deleted from the tree (or superseded by a version bump) is swept from
// discovery the next time a server is built on the reloaded registry -
// the same sync the serve loop's reload watcher drives.
func TestAuthoredSkillLifecycleTracksSpecTree(t *testing.T) {
	deps, _ := newTestDeps(t)
	ctx := context.Background()
	connect := func(d Deps) *mcp.ClientSession {
		t.Helper()
		srv := New(d)
		st, ct := mcp.NewInMemoryTransports()
		if _, err := srv.Connect(ctx, st, nil); err != nil {
			t.Fatal(err)
		}
		client := mcp.NewClient(&mcp.Implementation{Name: "sweep", Version: "0"}, nil)
		cs, err := client.Connect(ctx, ct, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cs.Close() })
		return cs
	}
	writeSpec := func(content string) {
		t.Helper()
		dir := filepath.Join(deps.Reg.Dir, "skills", "lifecycle-runbook")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := deps.Reg.Load(ctx); err != nil {
			t.Fatal(err)
		}
	}

	const v1 = "---\nname: lifecycle-runbook\ndescription: Inspect lifecycle sentinel diagnostics\nmetadata:\n  version: 0.1.0\n---\n\nFirst procedure.\n"
	writeSpec(v1)
	cs := connect(deps)
	if out := callTool(t, cs, "search_skills", map[string]any{"query": "lifecycle sentinel"}); !strings.Contains(out, "lifecycle-runbook") {
		t.Fatalf("authored skill not indexed at server build: %s", out)
	}
	if out := callTool(t, cs, "load_skill", map[string]any{"name": "lifecycle-runbook", "version": "0.1.0"}); !strings.Contains(out, "First procedure.") {
		t.Fatalf("v0.1.0 body: %s", out)
	}

	// A same-version source edit is kept pinned and reported as a
	// conflict; the next build still serves the published procedure.
	writeSpec(strings.Replace(v1, "First procedure.", "Quietly rewritten procedure.", 1))
	cs2 := connect(deps)
	if out := callTool(t, cs2, "load_skill", map[string]any{"name": "lifecycle-runbook", "version": "0.1.0"}); !strings.Contains(out, "First procedure.") {
		t.Fatalf("same-version edit must stay pinned: %s", out)
	}

	// A version bump publishes the revision and sweeps the old version.
	writeSpec(strings.Replace(v1, "version: 0.1.0", "version: 0.2.0", 1))
	cs3 := connect(deps)
	if out := callTool(t, cs3, "load_skill", map[string]any{"name": "lifecycle-runbook", "version": "0.2.0"}); !strings.Contains(out, "First procedure.") {
		t.Fatalf("v0.2.0 body: %s", out)
	}
	if out, _, _ := call(t, cs3, "load_skill", map[string]any{"name": "lifecycle-runbook", "version": "0.1.0"}); !strings.Contains(out, "not found") {
		t.Fatalf("superseded v0.1.0 must be swept, got: %s", out)
	}

	// Deleting the skill from the tree unpublishes it entirely.
	if err := os.RemoveAll(filepath.Join(deps.Reg.Dir, "skills", "lifecycle-runbook")); err != nil {
		t.Fatal(err)
	}
	if err := deps.Reg.Load(ctx); err != nil {
		t.Fatal(err)
	}
	cs4 := connect(deps)
	if out := callTool(t, cs4, "search_skills", map[string]any{"query": "lifecycle sentinel"}); strings.Contains(out, "lifecycle-runbook") {
		t.Fatalf("deleted source still discoverable: %s", out)
	}
}
