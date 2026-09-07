package api

// The C06 acceptance gate for the Codex 0.153.4 integration, SIMULATED
// half: these tests drive the REAL server (httptest in front of a real
// api.Server over a temporary sqlite store) through the exact hook CLI
// paths Codex 0.153.4 exercises - the same hookcli.RunFrom("codex", ...)
// entry point the installed hooks.json invokes - using the recorded
// native 0.153.4 fixtures (internal/hookcli/testdata/codex-0.153.4/,
// mirroring upstream codex-rs/hooks/src/schema.rs at rust-v0.153.4,
// commit 3d2ee51ca2d5db578f328aa75e20aa22c0197c9a).
//
// What this proves: the hook-to-capture-to-injection lifecycle contracts
// C01/C03/C04/C05 built - idempotent connect, once-per-event context
// delivery, native turn_id capture identity, duplicate transport
// suppression, capture-only stdout silence, fail-open offline/slow
// behavior and foreign-configuration preservation - hold end to end at
// the HTTP boundary.
//
// What this CANNOT prove: native Codex TUI renderer behavior (footer
// composition, terminal-title handling, hook-status surfaces). Those are
// upstream properties only a real `codex` binary run can observe; they are
// covered by the separate NATIVE acceptance procedure documented in
// docs/investigations/codex-0.153.4-terminal-spam.md. No test here claims
// a CLI or simulated result about the TUI renderer.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/hookcli"
	"github.com/hypervisor-io/punk-records/internal/memory"
	"github.com/hypervisor-io/punk-records/internal/store"
)

// codexFixtureCWD is the cwd every recorded native 0.153.4 fixture
// carries; AgentNamespace maps it to agent-punkrecords.
const codexFixtureCWD = "/home/u/punkrecords"

// codexGateRig is one simulated-acceptance environment: a real api.Server
// (temporary sqlite DB, per-turn context budget enabled) over a real
// httptest listener, plus a temporary CODEX_HOME for the connect steps.
// The seed fact is written into the namespace the fixtures' cwd resolves
// to, so session-start and turn injections have durable content to
// deliver.
type codexGateRig struct {
	srv      *Server
	ts       *httptest.Server
	mem      *memory.Store
	ns       string
	codexDir string
}

func newCodexGateRig(t *testing.T) *codexGateRig {
	t.Helper()
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "codex-gate.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.MigrateUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	mem := memory.New(db, nil)
	srv := New(slog.New(slog.DiscardHandler), Deps{Memory: mem, TurnContextTokens: 600})
	ts := httptest.NewServer(srv.Router())
	t.Cleanup(ts.Close)

	ns := AgentNamespace(codexFixtureCWD)
	ctx := context.Background()
	for _, f := range []memory.WriteInput{
		{Namespace: ns, Key: "/decisions/auth", Body: "auth uses jwt via jose", Author: "test", Writer: "test", Importance: 0.8},
		{Namespace: ns, Key: "/notes/turn", Body: "the gate verifies normalize native codex hook events capture", Author: "test", Writer: "test", Importance: 0.8},
	} {
		if _, err := mem.Write(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	return &codexGateRig{srv: srv, ts: ts, mem: mem, ns: ns, codexDir: t.TempDir()}
}

// runHook drives ONE native fixture through the real boundary: the
// fixture's bytes (optionally mutated) go through hookcli.RunFrom("codex")
// - the code path the installed hooks.json command executes - against the
// rig's real listener. RunFrom always returns nil by contract; the error
// return is asserted so a future signature change fails loudly here.
func (rig *codexGateRig) runHook(t *testing.T, fixture string, mutate func(map[string]any)) (stdout, stderr string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "hookcli", "testdata", "codex-0.153.4", fixture))
	if err != nil {
		t.Fatalf("read fixture %s: %v", fixture, err)
	}
	if mutate != nil {
		var native map[string]any
		if err := json.Unmarshal(raw, &native); err != nil {
			t.Fatalf("fixture %s not decodable: %v", fixture, err)
		}
		mutate(native)
		raw, err = json.Marshal(native)
		if err != nil {
			t.Fatal(err)
		}
	}
	var out, errw strings.Builder
	if err := hookcli.RunFrom("codex", strings.NewReader(string(raw)), rig.ts.URL, "", &out, &errw); err != nil {
		t.Fatalf("RunFrom: %v", err)
	}
	return out.String(), errw.String()
}

// codexPunkGroups counts the punk-managed hook groups registered under
// event in the hooks.json at dir, so idempotence checks assert exactly
// one intended entry per event survives repeated connects.
func codexPunkGroups(t *testing.T, dir, event string) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("hooks.json not decodable: %v", err)
	}
	hooks, _ := generic["hooks"].(map[string]any)
	groups, _ := hooks[event].([]any)
	count := 0
	for _, g := range groups {
		gm, _ := g.(map[string]any)
		hooksList, _ := gm["hooks"].([]any)
		for _, h := range hooksList {
			hm, _ := h.(map[string]any)
			if cmd, _ := hm["command"].(string); strings.Contains(cmd, "--from codex") {
				count++
				break
			}
		}
	}
	return count
}

// codexManagedBlocks counts punk-managed block markers in config.toml.
func codexManagedBlocks(t *testing.T, dir string) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(raw), "# punk-managed-start")
}

// TestCodexIntegrationLifecycle is the simulated end-to-end acceptance
// sequence for one Codex 0.153.4 session against the real server, in the
// order a real session experiences them: connect twice, session startup,
// prompt submit/turn flow, a rapid tool burst, stop, then resume. The
// subtests share one rig ON PURPOSE - they are phases of one lifecycle,
// and later phases assert bookkeeping earlier phases wrote - so they run
// sequentially and must stay in order.
func TestCodexIntegrationLifecycle(t *testing.T) {
	rig := newCodexGateRig(t)
	const sid = "sess-codex-0153-1"

	t.Run("connect twice is idempotent", func(t *testing.T) {
		punkPath := "/usr/local/bin/punk"
		opts := hookcli.MCPEntryOpts{ServerURL: rig.ts.URL, Namespace: rig.ns, Agent: "c06-gate"}
		changed, err := hookcli.ConnectCodexConfig(filepath.Join(rig.codexDir, "config.toml"), opts, true, false)
		if err != nil || !changed {
			t.Fatalf("first connect: changed=%v err=%v", changed, err)
		}
		changedHooks, err := hookcli.ConnectCodexHooks(filepath.Join(rig.codexDir, "hooks.json"), punkPath, rig.ts.URL, rig.ns)
		if err != nil || !changedHooks {
			t.Fatalf("first hooks connect: changed=%v err=%v", changedHooks, err)
		}

		// Second connect: byte-identical output, so changed=false on both
		// files and the managed-block/group counts stay at one.
		if changed, err := hookcli.ConnectCodexConfig(filepath.Join(rig.codexDir, "config.toml"), opts, true, false); err != nil || changed {
			t.Fatalf("second config connect must be a no-op: changed=%v err=%v", changed, err)
		}
		if changed, err := hookcli.ConnectCodexHooks(filepath.Join(rig.codexDir, "hooks.json"), punkPath, rig.ts.URL, rig.ns); err != nil || changed {
			t.Fatalf("second hooks connect must be a no-op: changed=%v err=%v", changed, err)
		}
		if n := codexManagedBlocks(t, rig.codexDir); n != 1 {
			t.Fatalf("config.toml carries %d punk-managed blocks, want 1", n)
		}
		for _, ev := range []string{"SessionStart", "UserPromptSubmit", "PostToolUse", "Stop"} {
			if n := codexPunkGroups(t, rig.codexDir, ev); n != 1 {
				t.Fatalf("hooks.json carries %d punk groups under %s, want 1", n, ev)
			}
		}
	})

	t.Run("startup delivers context once", func(t *testing.T) {
		out, errw := rig.runHook(t, "session_start.json", nil)
		if errw != "" {
			t.Fatalf("healthy startup must not note failures: %s", errw)
		}
		if !strings.Contains(out, `"hookSpecificOutput"`) || !strings.Contains(out, "auth uses jwt via jose") {
			t.Fatalf("startup must inject the session block: %s", out)
		}
		facts, err := rig.mem.Recall(context.Background(), rig.ns, "/agent-sessions/"+sid+"/start", 2)
		if err != nil || len(facts) != 1 || facts[0].Key != "/agent-sessions/"+sid+"/start" {
			t.Fatalf("startup capture not stored exactly once: %v %v", facts, err)
		}
		if _, _, state, _ := deliveryMarker(t, rig.srv, rig.ns, sid); state != "issued" {
			t.Fatalf("delivery marker state = %q, want issued", state)
		}
	})

	t.Run("duplicate startup delivery suppressed", func(t *testing.T) {
		_, rev, _, reinf := deliveryMarker(t, rig.srv, rig.ns, sid)
		out, errw := rig.runHook(t, "session_start.json", nil)
		if errw != "" {
			t.Fatalf("duplicate delivery must not error: %s", errw)
		}
		if out != "" {
			t.Fatalf("duplicate startup must inject nothing, got %s", out)
		}
		if _, rev2, _, reinf2 := deliveryMarker(t, rig.srv, rig.ns, sid); rev2 != rev || reinf2 != reinf {
			t.Fatalf("duplicate delivery rewrote the marker: rev %q->%q reinf %d->%d", rev, rev2, reinf, reinf2)
		}
	})

	t.Run("submit turn captures and injects once per turn", func(t *testing.T) {
		// A fact the session start has NOT already injected: the turn
		// path filters what the session was already shown, so the
		// per-turn block only ever carries fresh, prompt-matching
		// content (write one per turn below, mirroring memory that
		// changes between turns).
		if _, err := rig.mem.Write(context.Background(), memory.WriteInput{
			Namespace: rig.ns, Key: "/decisions/turn-one",
			Body:   "turn one verifies normalize native codex hook events delivery",
			Author: "test", Writer: "test", Importance: 0.8,
		}); err != nil {
			t.Fatal(err)
		}
		out, errw := rig.runHook(t, "user_prompt_submit.json", nil)
		if errw != "" {
			t.Fatalf("healthy turn must not note failures: %s", errw)
		}
		if !strings.Contains(out, `"hookSpecificOutput"`) || !strings.Contains(out, "turn one verifies") {
			t.Fatalf("turn must inject the prompt-scoped block: %s", out)
		}
		if strings.Contains(out, "auth uses jwt via jose") {
			t.Fatalf("turn must not re-inject the session-start block: %s", out)
		}
		facts, err := rig.mem.Recall(context.Background(), rig.ns, "/agent-sessions/"+sid+"/prompt-turn-codex-0001", 2)
		if err != nil || len(facts) != 1 {
			t.Fatalf("native turn_id capture not stored: %v %v", facts, err)
		}
		if !strings.Contains(facts[0].Body, "normalize native codex hook events") {
			t.Fatalf("prompt text not captured: %q", facts[0].Body)
		}

		// Same turn delivered again: the pid delivery identity suppresses
		// the injection (C04); the capture key stays single.
		out, _ = rig.runHook(t, "user_prompt_submit.json", nil)
		if out != "" {
			t.Fatalf("duplicate turn delivery must inject nothing, got %s", out)
		}

		// The pid identity is its own boundary, not masked by the
		// injected-ID dedup: even a NEW matching fact must not resurrect
		// an already-delivered pid - a transport replay of turn one stays
		// silent while fresh content waits for a fresh turn.
		if _, err := rig.mem.Write(context.Background(), memory.WriteInput{
			Namespace: rig.ns, Key: "/decisions/turn-replay-bait",
			Body:   "replay bait must not ride a delivered turn for normalize native codex hook events",
			Author: "test", Writer: "test", Importance: 0.8,
		}); err != nil {
			t.Fatal(err)
		}
		out, _ = rig.runHook(t, "user_prompt_submit.json", nil)
		if out != "" {
			t.Fatalf("delivered pid must stay suppressed even with fresh matching facts, got %s", out)
		}

		// A distinct turn with identical prompt text is a distinct capture
		// and stays eligible for injection of NEW facts only (the per-
		// session injected-ID dedup remains effective underneath).
		if _, err := rig.mem.Write(context.Background(), memory.WriteInput{
			Namespace: rig.ns, Key: "/decisions/turn-two",
			Body:   "turn two verifies distinct pid eligibility for normalize native codex hook events",
			Author: "test", Writer: "test", Importance: 0.8,
		}); err != nil {
			t.Fatal(err)
		}
		out, _ = rig.runHook(t, "user_prompt_submit.json", func(m map[string]any) { m["turn_id"] = "turn-codex-0002" })
		if !strings.Contains(out, `"hookSpecificOutput"`) || !strings.Contains(out, "turn two verifies") {
			t.Fatalf("distinct turn must be eligible again: %s", out)
		}
		facts, err = rig.mem.Recall(context.Background(), rig.ns, "/agent-sessions/"+sid+"/delivered-turns", 2)
		if err != nil || len(facts) != 1 {
			t.Fatalf("delivered-turns bookkeeping = %v %v", facts, err)
		}
		if facts[0].Body != "turn-codex-0001 turn-codex-0002" || facts[0].Reinforcements != 0 {
			t.Fatalf("delivered-turns body = %q reinf = %d", facts[0].Body, facts[0].Reinforcements)
		}
	})

	t.Run("tool burst captures every event", func(t *testing.T) {
		// Eight rapid concurrent PostToolUse events (distinct tool_use_id,
		// like Codex firing after every tool call in quick succession):
		// every one is captured under its own key, nothing prints, and no
		// event is lost or crossed.
		const n = 8
		var wg sync.WaitGroup
		stdouts := make([]string, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				id := "call-codex-burst-" + string(rune('a'+i))
				o, _ := rig.runHook(t, "post_tool_use.json", func(m map[string]any) {
					m["tool_use_id"] = id
					m["turn_id"] = "turn-codex-burst-" + string(rune('a'+i))
				})
				stdouts[i] = o
			}(i)
		}
		wg.Wait()
		for i := 0; i < n; i++ {
			if stdouts[i] != "" {
				t.Fatalf("capture-only PostToolUse printed: %s", stdouts[i])
			}
			id := "call-codex-burst-" + string(rune('a'+i))
			facts, err := rig.mem.Recall(context.Background(), rig.ns, "/agent-sessions/"+sid+"/tool-"+id, 2)
			if err != nil || len(facts) != 1 || facts[0].Key != "/agent-sessions/"+sid+"/tool-"+id {
				t.Fatalf("burst capture %d missing: %v %v", i, facts, err)
			}
			if !strings.Contains(facts[0].Body, "shell") {
				t.Fatalf("burst capture %d lost tool content: %q", i, facts[0].Body)
			}
		}
	})

	t.Run("stop captured silently", func(t *testing.T) {
		out, _ := rig.runHook(t, "stop.json", nil)
		if out != "" {
			t.Fatalf("capture-only Stop printed: %s", out)
		}
		facts, err := rig.mem.Recall(context.Background(), rig.ns, "/agent-sessions/"+sid+"/stop", 2)
		if err != nil || len(facts) != 1 {
			t.Fatalf("stop capture missing: %v %v", facts, err)
		}
		if !strings.Contains(facts[0].Body, "Normalized the native Codex hook payloads.") {
			t.Fatalf("stop body missing the assistant message: %q", facts[0].Body)
		}
	})

	t.Run("resume refreshes once per resume event", func(t *testing.T) {
		resume := func(m map[string]any) { m["source"] = "resume" }
		out, errw := rig.runHook(t, "session_start.json", resume)
		if errw != "" {
			t.Fatalf("healthy resume must not note failures: %s", errw)
		}
		if !strings.Contains(out, `"hookSpecificOutput"`) || !strings.Contains(out, "auth uses jwt via jose") {
			t.Fatalf("fresh resume must refresh the block once: %s", out)
		}
		if event, _, state, _ := deliveryMarker(t, rig.srv, rig.ns, sid); event != "resume" || state != "issued" {
			t.Fatalf("marker after resume = %q %q", event, state)
		}
		// A repeated resume delivery with an unchanged revision stays
		// silent - the documented bounded policy (no host delivery ID).
		out, _ = rig.runHook(t, "session_start.json", resume)
		if out != "" {
			t.Fatalf("repeated resume delivery must inject nothing, got %s", out)
		}
	})
}

// TestCodexForeignConfigPreserved pins the foreign-configuration half of
// the gate: connect must preserve pre-existing unmanaged content byte for
// byte (other MCP servers, user settings, user hook groups) and must
// REFUSE - not silently replace - a [mcp_servers.punk] table punk did not
// write, exactly like the real `punk connect codex` does.
func TestCodexForeignConfigPreserved(t *testing.T) {
	rig := newCodexGateRig(t)
	foreign := "model = \"gpt-5.1-codex\"\n\n[mcp_servers.other]\nurl = \"https://other.example.com/mcp\"\n\n[projects.\"/home/u/punkrecords\"]\ntrust_level = \"trusted\"\n"
	configPath := filepath.Join(rig.codexDir, "config.toml")
	if err := os.WriteFile(configPath, []byte(foreign), 0o600); err != nil {
		t.Fatal(err)
	}
	hooksPath := filepath.Join(rig.codexDir, "hooks.json")
	userHooks := `{"hooks":{"PostToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"my-lint.sh"}]}]}}`
	if err := os.WriteFile(hooksPath, []byte(userHooks), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := hookcli.MCPEntryOpts{ServerURL: rig.ts.URL, Namespace: rig.ns, Agent: "c06-gate"}
	changed, err := hookcli.ConnectCodexConfig(configPath, opts, true, false)
	if err != nil || !changed {
		t.Fatalf("connect over foreign config: changed=%v err=%v", changed, err)
	}
	changed, err = hookcli.ConnectCodexHooks(hooksPath, "/usr/local/bin/punk", rig.ts.URL, rig.ns)
	if err != nil || !changed {
		t.Fatalf("connect over foreign hooks: changed=%v err=%v", changed, err)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{
		`model = "gpt-5.1-codex"`,
		"[mcp_servers.other]",
		`url = "https://other.example.com/mcp"`,
		`trust_level = "trusted"`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("foreign config content %q lost:\n%s", want, s)
		}
	}
	if n := codexManagedBlocks(t, rig.codexDir); n != 1 {
		t.Fatalf("config.toml carries %d punk-managed blocks, want 1", n)
	}
	raw, err = os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "my-lint.sh") {
		t.Fatalf("user hook group lost:\n%s", raw)
	}
	if n := codexPunkGroups(t, rig.codexDir, "PostToolUse"); n != 1 {
		t.Fatalf("punk PostToolUse groups = %d, want 1 (the user group must not be duplicated or replaced)", n)
	}

	// A pre-existing [mcp_servers.punk] table punk did not write is
	// foreign: connect refuses it instead of silently replacing it.
	foreignPunk := "model = \"gpt-5.1-codex\"\n\n[mcp_servers.punk]\nurl = \"https://squatter.example.com/mcp\"\n"
	if err := os.WriteFile(configPath, []byte(foreignPunk), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := hookcli.ConnectCodexConfig(configPath, opts, true, false); err == nil {
		t.Fatal("connect must refuse a foreign [mcp_servers.punk] table without --force")
	}
	raw, err = os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "squatter.example.com") {
		t.Fatal("refused connect must leave the foreign table untouched")
	}
}

// TestCodexSlowAndOfflineServerFailOpen pins the degraded-transport half
// of the gate: the hook process must never error, never print garbage and
// never block the session when the punk server is unreachable or too slow
// to answer inside the hook client's own timeout - capture and injection
// are both best-effort by contract.
func TestCodexSlowAndOfflineServerFailOpen(t *testing.T) {
	t.Run("offline server", func(t *testing.T) {
		for _, fixture := range []string{"session_start.json", "post_tool_use.json"} {
			var out, errw strings.Builder
			if err := hookcli.RunFrom("codex", strings.NewReader(string(mustCodexFixture(t, fixture))), "http://127.0.0.1:1", "", &out, &errw); err != nil {
				t.Fatalf("%s: offline server must fail open, got %v", fixture, err)
			}
			if out.Len() != 0 {
				t.Fatalf("%s: offline server must print nothing, got %s", fixture, out.String())
			}
		}
	})

	t.Run("slow server times out without breaking the hook", func(t *testing.T) {
		// The context endpoint stalls past the hook client's own 2s
		// timeout while capture answers fast: the injection is dropped
		// (fail-open, silent) but the capture still lands - a slow server
		// degrades delivery, never the session.
		var captured sync.Map
		slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/agent/context" {
				time.Sleep(3 * time.Second)
				w.Write([]byte(`{"context":"too late"}`)) //nolint:errcheck
				return
			}
			captured.Store(r.URL.Path, true)
			w.Write([]byte(`{"status":"stored"}`)) //nolint:errcheck
		}))
		t.Cleanup(slow.Close)

		var out, errw strings.Builder
		if err := hookcli.RunFrom("codex", strings.NewReader(string(mustCodexFixture(t, "session_start.json"))), slow.URL, "", &out, &errw); err != nil {
			t.Fatalf("slow server must fail open, got %v", err)
		}
		if out.Len() != 0 {
			t.Fatalf("timed-out context fetch must print nothing, got %s", out.String())
		}
		if _, ok := captured.Load("/v1/agent/hooks"); !ok {
			t.Fatal("capture must still be attempted against the fast endpoint")
		}
	})
}

// mustCodexFixture reads one recorded native 0.153.4 fixture.
func mustCodexFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "hookcli", "testdata", "codex-0.153.4", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return raw
}
