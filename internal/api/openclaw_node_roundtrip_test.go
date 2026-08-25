package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hypervisor-io/punk-records/internal/hookcli"
	"github.com/hypervisor-io/punk-records/internal/memory"
)

// openClawNodeRTCWD and openClawNodeRTSessionID are spliced into the JS
// driver below as Go string concatenation, not retyped as a second JS-side
// literal, so the two sides cannot drift.
const (
	openClawNodeRTCWD       = "/tmp/punk-openclaw-node-rt-project"
	openClawNodeRTSessionID = "oclaw-node-rt-session-1"
)

// openClawNodeRTContextFactBody is the sentinel seeded into memory before
// the driver runs, so before_prompt_build's injection has real project
// memory to fetch. Distinctive enough that a substring match cannot
// accidentally hit anything else this test writes.
const openClawNodeRTContextFactBody = "openclaw node round trip context injection sentinel fact 41d9ab"

// TestOpenClawPluginNodeRoundTripsThroughRealServer drives the REAL
// rendered OpenClaw plugin (hookcli.WriteOpenClawPlugin's output, not a
// paraphrase of it) under node against a live httptest.Server wrapping this
// package's own Server.Router() - the exact handleAgentHook and
// handleAgentContext a production deployment runs.
//
// This is the only test that can catch drift in the plugin TEMPLATE
// itself: a renamed envelope field, a dropped tool_use_id, a broken
// injection field read. A Go-side test that posts hand-written envelope
// literals proves only that the SERVER accepts them, and a byte-golden of
// the template survives any mutation that edits template and golden
// together (see .claude/rules/testing.md).
//
// The driver imports the plugin as a real ES module through the package.json
// the writer emits alongside it (type: module, main/pluginEntry ./index.js),
// so module resolution is exercised too rather than assumed. It then
// registers the plugin against a stub api that records handlers by hook
// name, and invokes: session_start, before_prompt_build (with a real
// prompt, whose return value is printed for the injection assertion), a
// SECOND before_prompt_build for the same session (the once-per-session
// injection gate), after_tool_call, and agent_end.
//
// Skipped when node is not on PATH, mirroring every other node-driven test
// in this repo.
func TestOpenClawPluginNodeRoundTripsThroughRealServer(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping openclaw plugin node round-trip test")
	}

	srv := testServer(t)
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()

	// Seeded directly through srv.mem (not through a hook) so it is real
	// project knowledge with high importance, not the ephemeral
	// /agent-sessions/ capture context assembly deliberately excludes.
	if _, err := srv.mem.Write(context.Background(), memory.WriteInput{
		Namespace:  AgentNamespace(openClawNodeRTCWD),
		Key:        "/decisions/openclaw-node-rt-context",
		Body:       openClawNodeRTContextFactBody,
		Author:     "test",
		Writer:     "test",
		Importance: 0.8,
	}); err != nil {
		t.Fatal(err)
	}

	pluginDir := filepath.Join(t.TempDir(), "plugins", hookcli.OpenClawPluginID)
	if _, err := hookcli.WriteOpenClawPlugin(pluginDir, httpSrv.URL); err != nil {
		t.Fatal(err)
	}

	// The plugin's own hooks read cwd from process.cwd(), so the driver
	// runs with the harness's working directory set to the namespace this
	// test seeded - the same way a real OpenClaw gateway's cwd determines
	// which project's memory a session belongs to.
	if err := os.MkdirAll(openClawNodeRTCWD, 0o755); err != nil {
		t.Fatal(err)
	}

	driver := `
import plugin from "./index.js"

// Every observation hook fires its POST without awaiting it (so punk never
// adds latency to a turn), and an async function runs synchronously up to
// its first await - so by the time each handler call returns, fetch has
// already been invoked and captured here. Awaiting these before the driver
// exits is what guarantees the POSTs actually reached the server before Go
// reads the stored facts back.
const pending = []
const realFetch = fetch
globalThis.fetch = (...args) => {
  const p = realFetch(...args)
  pending.push(p.catch(() => {}))
  return p
}

const handlers = new Map()
plugin.register({ on: (name, handler) => { handlers.set(name, handler) } })

function hook(name) {
  const h = handlers.get(name)
  if (!h) {
    console.error("FAIL: plugin never registered hook " + name)
    process.exit(1)
  }
  return h
}

async function main() {
  const sessionId = "` + openClawNodeRTSessionID + `"
  const ctx = { sessionId, sessionKey: "cli:" + sessionId }

  await hook("session_start")({ sessionKey: ctx.sessionKey, sessionId, reason: "new" }, ctx)

  const first = await hook("before_prompt_build")(
    { prompt: "why did the migration fail", messages: [] },
    ctx,
  )
  console.log("FIRST_PROMPT_BUILD_START" + JSON.stringify(first === undefined ? null : first) + "FIRST_PROMPT_BUILD_END")

  // Same session again: the once-per-session gate must return nothing.
  const second = await hook("before_prompt_build")(
    { prompt: "and what about the index", messages: [] },
    ctx,
  )
  console.log("SECOND_PROMPT_BUILD_START" + JSON.stringify(second === undefined ? null : second) + "SECOND_PROMPT_BUILD_END")

  await hook("after_tool_call")(
    { toolName: "shell", params: { command: "ls -la" }, result: "file1\nfile2", durationMs: 12 },
    ctx,
  )

  await hook("agent_end")(
    {
      runId: "run-1",
      success: true,
      durationMs: 900,
      messages: [
        { role: "user", content: "why did the migration fail" },
        { role: "assistant", content: "the 0020 migration was never applied" },
      ],
    },
    ctx,
  )

  await Promise.allSettled(pending)
  console.log("driver done, requests=" + pending.length)
}
main().catch((err) => {
  console.error("FAIL: driver rejected:", err)
  process.exit(1)
})
`
	// Written INSIDE the plugin directory on purpose: node resolves a
	// module's type from the nearest package.json, so this is what proves
	// the emitted package.json really marks index.js as ESM.
	harnessPath := filepath.Join(pluginDir, "roundtrip-harness.mjs")
	if err := os.WriteFile(harnessPath, []byte(driver), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, nodePath, harnessPath)
	cmd.Dir = openClawNodeRTCWD
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("node harness did not complete within 15s: %s", out)
	}
	if err != nil {
		t.Fatalf("node harness failed: %v\n%s", err, out)
	}
	t.Logf("node harness output:\n%s", out)

	ns := AgentNamespace(openClawNodeRTCWD)
	sid := sanitizeID(openClawNodeRTSessionID)
	recall := func(prefix string, limit int) []memory.Fact {
		t.Helper()
		facts, err := srv.mem.Recall(context.Background(), ns, prefix, limit)
		if err != nil {
			t.Fatalf("Recall(%s, %s): %v", ns, prefix, err)
		}
		return facts
	}

	// SessionStart. Limits stay above the expected count everywhere below
	// so a duplicate-fire regression shows up as a count mismatch instead
	// of being silently capped away.
	starts := recall("/agent-sessions/"+sid+"/start", 5)
	if len(starts) != 1 {
		t.Fatalf("expected exactly one SessionStart fact, got %d", len(starts))
	}
	if starts[0].SourceRef != "openclaw" {
		t.Fatalf("SessionStart SourceRef = %q, want openclaw", starts[0].SourceRef)
	}

	// Both prompts are captured even though only the first one injects -
	// gating capture on the injection gate would silently lose every
	// prompt after a session's first.
	prompts := recall("/agent-sessions/"+sid+"/prompt-", 10)
	if len(prompts) != 2 {
		t.Fatalf("expected 2 UserPromptSubmit facts (capture is not gated by the injection gate), got %d: %+v", len(prompts), prompts)
	}
	var sawFirst, sawSecond bool
	for _, p := range prompts {
		if p.SourceRef != "openclaw" {
			t.Fatalf("prompt fact %s SourceRef = %q, want openclaw", p.Key, p.SourceRef)
		}
		switch {
		case strings.Contains(p.Body, "why did the migration fail"):
			sawFirst = true
		case strings.Contains(p.Body, "and what about the index"):
			sawSecond = true
		default:
			t.Fatalf("unexpected prompt fact body: %q", p.Body)
		}
	}
	if !sawFirst || !sawSecond {
		t.Fatalf("missing prompt facts: first=%v second=%v", sawFirst, sawSecond)
	}

	// PostToolUse. The server drops any PostToolUse whose tool_use_id
	// sanitizes to empty, so a template that stopped synthesizing one
	// leaves nothing at all here.
	tools := recall("/agent-sessions/"+sid+"/tool-call-", 5)
	if len(tools) != 1 {
		t.Fatalf("expected exactly one PostToolUse fact under the synthesized call- id, got %d", len(tools))
	}
	if !strings.Contains(tools[0].Body, "shell") || !strings.Contains(tools[0].Body, "file1") {
		t.Fatalf("PostToolUse body missing tool name or result: %q", tools[0].Body)
	}

	// Stop, from agent_end: the real assistant text, not the
	// success/duration fallback.
	stops := recall("/agent-sessions/"+sid+"/stop", 5)
	if len(stops) != 1 {
		t.Fatalf("expected exactly one Stop fact, got %d", len(stops))
	}
	if !strings.Contains(stops[0].Body, "the 0020 migration was never applied") {
		t.Fatalf("Stop body = %q, want the last assistant message", stops[0].Body)
	}

	// Injection: the first before_prompt_build must return prependContext
	// holding the seeded fact. This is the assertion that dies if the
	// template's `data.context` read is renamed, or if prependContext is
	// spelled anything else.
	first := driverJSON(t, out, "FIRST_PROMPT_BUILD_START", "FIRST_PROMPT_BUILD_END")
	if first == nil {
		t.Fatal("first before_prompt_build returned nothing; no context was injected")
	}
	prepend, _ := first["prependContext"].(string)
	if prepend == "" {
		t.Fatalf("first before_prompt_build returned %v, want a prependContext string", first)
	}
	if !strings.Contains(prepend, openClawNodeRTContextFactBody) {
		t.Fatalf("injected context missing the seeded fact: %q", prepend)
	}

	// The once-per-session gate: same session, no second injection.
	if second := driverJSON(t, out, "SECOND_PROMPT_BUILD_START", "SECOND_PROMPT_BUILD_END"); second != nil {
		t.Fatalf("second before_prompt_build injected again in the same session: %v", second)
	}
}

// driverJSON pulls one delimited JSON object out of the harness's combined
// output. A JSON null (what the driver prints for an undefined return)
// decodes to a nil map, which is how callers distinguish "returned nothing"
// from "returned an object".
func driverJSON(t *testing.T, out []byte, startMarker, endMarker string) map[string]any {
	t.Helper()
	s := string(out)
	start := strings.Index(s, startMarker)
	end := strings.Index(s, endMarker)
	if start < 0 || end < 0 || end < start {
		t.Fatalf("driver output missing %s/%s markers: %s", startMarker, endMarker, out)
	}
	body := s[start+len(startMarker) : end]
	var decoded map[string]any
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("driver payload between %s and %s is not JSON: %v: %s", startMarker, endMarker, err, body)
	}
	return decoded
}
