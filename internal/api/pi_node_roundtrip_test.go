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

// piNodeRTCWD and piNodeRTSessionID are the fixed test constants the JS
// driver below is built from, mirroring
// opencode_node_roundtrip_test.go's own openCodeNodeRT* constants.
const (
	piNodeRTCWD       = "/tmp/punk-pi-node-rt-project"
	piNodeRTSessionID = "pi-node-rt-session-1"
)

// piNodeRTContextFactBody is the sentinel content seeded into memory (see
// the srv.mem.Write call below) before the driver runs, so
// before_agent_start's injection has real project memory to fetch and
// inject. Distinct enough that a substring match against it can't
// accidentally hit anything else this test writes.
const piNodeRTContextFactBody = "pi node round trip context injection sentinel fact 5e2a90"

// TestPiExtensionNodeRoundTripsThroughRealServer is round-1 finding #1's
// fix: internal/hookcli's own node-driven pi test used to assert only
// against a hand-typed local mux (connect_pi_test.go) that re-typed the
// server's envelope contract from scratch - it could never detect drift
// between that hand-typed contract and the REAL
// handleAgentHook/handleAgentContext handlers (a renamed field, a status
// code change, a dropped filter), the same gap
// opencode_node_roundtrip_test.go's own doc comment identifies and closes
// for OpenCode. That hand-typed mux test has since been removed;
// connect_pi_test.go now only pins byte-exact golden content, file-write
// semantics, and syntax validity, and this test is the sole node-driven
// check against the real server contract.
//
// This test closes that gap for pi by driving the ACTUAL rendered
// extension (hookcli.ConnectPi's output, not a paraphrase of it) under
// node against a live httptest.Server wrapping this package's own real
// Server.Router() - the exact handleAgentHook and GET /v1/agent/context
// handlers a production deployment would run. Unlike OpenCode's plugin (a
// factory that RETURNS a Hooks object, straightforward to invoke
// standalone), a pi extension's default export registers handlers as a
// SIDE EFFECT of calling pi.on(...) inside the factory - there is no real
// pi runtime available in this Go test environment to host it, so a
// minimal fake "pi" facade (matching pi's documented
// ExtensionAPI/ExtensionContext shapes: pi.on(eventName, handler);
// ctx.cwd, ctx.sessionManager.getSessionId()) collects the registrations,
// exactly as internal/hookcli's own node driver does. What's different
// here, and what makes this the real round trip: every POST the
// extension issues (postHook -> punkFetch -> fetch) lands on the REAL
// server, driven by ConnectPi baking httpSrv.URL in as the PUNK_URL
// fallback the same way a real deployment's serverURL flag would; nothing
// about the server side is re-implemented by hand.
//
// A fact is seeded through the real server BEFORE the driver runs (see
// srv.mem.Write below), under the same namespace AgentNamespace(cwd)
// derives; the driver's before_agent_start call must come back with that
// fact's content appended to systemPrompt. This is what would catch a
// mutation renaming the field the extension reads off the parsed context
// response (data.context -> data.contextx): nothing in
// internal/hookcli's own test suite ever drives that check against a real
// server response body.
//
// Skipped when node is not on PATH (mirrors every other node-driven test
// in this repo).
func TestPiExtensionNodeRoundTripsThroughRealServer(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping pi extension node round-trip test")
	}

	srv := testServer(t)
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()

	// Seed a durable, high-importance fact under the namespace
	// AgentNamespace(piNodeRTCWD) derives, BEFORE the driver runs -
	// mirroring TestOpenCodePluginNodeRoundTripsThroughRealServer's own
	// pattern - so before_agent_start's GET /v1/agent/context has real
	// project memory to fetch and inject. Written directly via srv.mem
	// (not through a hook), so it's real project knowledge, not the
	// ephemeral /agent-sessions/ capture the driver's other hooks produce
	// and that context assembly deliberately excludes.
	seedCtx := context.Background()
	if _, err := srv.mem.Write(seedCtx, memory.WriteInput{
		Namespace:  AgentNamespace(piNodeRTCWD),
		Key:        "/decisions/pi-node-rt-context",
		Body:       piNodeRTContextFactBody,
		Author:     "test",
		Writer:     "test",
		Importance: 0.8,
	}); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	extPath := filepath.Join(dir, "punk-memory.ts")
	if _, err := hookcli.ConnectPi(extPath, httpSrv.URL); err != nil {
		t.Fatal(err)
	}
	extSrc, err := os.ReadFile(extPath)
	if err != nil {
		t.Fatal(err)
	}

	// The driver is appended to the ACTUAL rendered extension source in
	// one .mjs file (unambiguously ESM by extension - node's
	// CommonJS-by-default loader otherwise rejects top-level "export"
	// under direct execution, see internal/hookcli/connect_pi_test.go's
	// runPiSyntaxCheck doc comment for the same reasoning), so this
	// exercises the exact bytes ConnectPi writes. The extension's own
	// fetch() already targets httpSrv.URL (baked in by ConnectPi above),
	// so fetch is monkey-patched here only to collect in-flight promises,
	// not to redirect requests elsewhere - the OBSERVATIONAL hooks (which
	// deliberately do not await postHook internally, per pi_extension.go's
	// "Hook classification" doc comment) can then be waited on before the
	// driver exits and Go reads the stored facts back out.
	driver := `
const handlers = {}
const fakePi = {
  on(name, handler) { handlers[name] = handler },
}
const fakeCtx = {
  cwd: "` + piNodeRTCWD + `",
  sessionManager: { getSessionId: () => "` + piNodeRTSessionID + `" },
}

const pending = []
const realFetch = fetch
globalThis.fetch = (...args) => {
  const p = realFetch(...args)
  pending.push(p.catch(() => {}))
  return p
}

async function main() {
  extensionFactory(fakePi)

  await handlers.session_start({ reason: "new" }, fakeCtx)

  await handlers.input({ text: "real pi prompt text", source: "interactive" }, fakeCtx)
  await handlers.input({ text: "synthetic distractor from another extension", source: "extension" }, fakeCtx)

  await handlers.tool_result(
    { toolName: "bash", toolCallId: "rt-call-1", input: { command: "ls -la" }, content: "file1\nfile2" },
    fakeCtx
  )

  handlers.turn_end(
    { turnIndex: 0, message: { role: "assistant", content: [{ type: "text", text: "final answer text" }] } },
    fakeCtx
  )
  await handlers.agent_settled({}, fakeCtx)

  const result = await handlers.before_agent_start({ systemPrompt: "BASE PROMPT" }, fakeCtx)
  console.log("BEFORE_AGENT_START_RESULT_JSON_START" + JSON.stringify(result || null) + "BEFORE_AGENT_START_RESULT_JSON_END")

  // Once-per-session injection gate: a second before_agent_start call for
  // the SAME session (fakeCtx, same sessionID as the call directly above)
  // must return undefined even though systemPrompt is present and valid -
  // the extension's own injectedSessions Set already marked this
  // sessionID injected on the call above, so this proves the gate is
  // keyed per session, not re-armed on every submitted prompt.
  const secondCallResult = await handlers.before_agent_start({ systemPrompt: "BASE PROMPT AGAIN" }, fakeCtx)
  console.log("SECOND_CALL_RESULT_JSON_START" + JSON.stringify(secondCallResult || null) + "SECOND_CALL_RESULT_JSON_END")

  // Finding #3: event.systemPrompt absent must never ship a punk-only
  // system prompt. Fresh session id (fakeCtx2) so the injectedSessions
  // gate doesn't just short-circuit this to undefined for the wrong
  // reason - same cwd, so the context fetch still finds the seeded fact
  // and reaches the base-prompt guard.
  const fakeCtx2 = {
    cwd: "` + piNodeRTCWD + `",
    sessionManager: { getSessionId: () => "` + piNodeRTSessionID + `-no-system-prompt" },
  }
  const noPromptResult = await handlers.before_agent_start({}, fakeCtx2)
  console.log("NO_SYSTEM_PROMPT_RESULT_JSON_START" + JSON.stringify(noPromptResult || null) + "NO_SYSTEM_PROMPT_RESULT_JSON_END")

  // Finding #4: ctx.sessionManager.getSessionId() resolving empty logs a
  // diagnostic exactly once per extension instance, not once per call.
  // The server ignores (200, no fact written) any hook whose session_id
  // sanitizes to empty, so these two calls don't disturb the cardinality
  // assertions above.
  const noSessionManagerCtx = { cwd: "` + piNodeRTCWD + `" }
  await handlers.session_start({ reason: "new" }, noSessionManagerCtx)
  await handlers.session_start({ reason: "new" }, noSessionManagerCtx)

  await Promise.allSettled(pending)
  console.log("driver done, requests=" + pending.length)
}
main().catch((err) => {
  console.error("FAIL: driver rejected:", err)
  process.exit(1)
})
`

	rewritten := strings.Replace(string(extSrc), "export default function (pi) {", "function extensionFactory(pi) {", 1)
	if rewritten == string(extSrc) {
		t.Fatal("expected to find and rewrite the extension's default export declaration")
	}

	harnessPath := extPath + ".roundtrip-harness.mjs"
	if err := os.WriteFile(harnessPath, []byte(rewritten+driver), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, nodePath, harnessPath).CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("node harness did not complete within 15s: %s", out)
	}
	if err != nil {
		t.Fatalf("node harness failed: %v\n%s", err, out)
	}
	t.Logf("node harness output:\n%s", out)

	ns := AgentNamespace(piNodeRTCWD)
	sid := sanitizeID(piNodeRTSessionID)
	recall := func(prefix string, limit int) []struct {
		Key       string
		Body      string
		SourceRef string
	} {
		t.Helper()
		facts, err := srv.mem.Recall(context.Background(), ns, prefix, limit)
		if err != nil {
			t.Fatalf("Recall(%s, %s): %v", ns, prefix, err)
		}
		out := make([]struct {
			Key       string
			Body      string
			SourceRef string
		}, len(facts))
		for i, f := range facts {
			out[i].Key = f.Key
			out[i].Body = f.Body
			out[i].SourceRef = f.SourceRef
		}
		return out
	}

	// SessionStart landed, sourced from pi. Recall limit deliberately well
	// above the one fact actually expected - see
	// opencode_node_roundtrip_test.go's own comment on the same pattern.
	starts := recall("/agent-sessions/"+sid+"/start", 5)
	if len(starts) != 1 {
		t.Fatalf("expected exactly one SessionStart fact, got %d", len(starts))
	}
	if starts[0].SourceRef != "pi" {
		t.Fatalf("SessionStart SourceRef = %q, want pi", starts[0].SourceRef)
	}

	// Exactly one prompt fact: the "extension"-sourced input must never be
	// captured (pi_extension.go's "input" handler excludes it the same way
	// OpenCode's chat.message translation excludes synthetic/ignored
	// parts).
	prompts := recall("/agent-sessions/"+sid+"/prompt-", 10)
	if len(prompts) != 1 {
		t.Fatalf("expected exactly one UserPromptSubmit fact (extension-sourced input must be excluded), got %d: %+v", len(prompts), prompts)
	}
	if prompts[0].SourceRef != "pi" {
		t.Fatalf("prompt fact SourceRef = %q, want pi", prompts[0].SourceRef)
	}
	if !strings.Contains(prompts[0].Body, "real pi prompt text") {
		t.Fatalf("expected the real interactive-source prompt to be captured, got: %q", prompts[0].Body)
	}
	if strings.Contains(prompts[0].Body, "synthetic distractor") {
		t.Fatalf("extension-sourced input must never be captured, found in prompt body: %q", prompts[0].Body)
	}
	if !strings.HasPrefix(prompts[0].Key, "/agent-sessions/"+sid+"/prompt-msg-") {
		t.Fatalf("prompt fact key = %q, want the fnv1aHex-derived \"msg-\"-prefixed fallback id shape (pi's input event carries no real id)", prompts[0].Key)
	}

	// PostToolUse: this is where an empty/mutated tool_use_id would fail -
	// the server drops any PostToolUse whose tool_use_id sanitizes to
	// empty as 200 "ignored" rather than storing it, so a mutated template
	// leaves no fact at this key at all.
	tools := recall("/agent-sessions/"+sid+"/tool-rt-call-1", 5)
	if len(tools) != 1 {
		t.Fatalf("expected exactly one PostToolUse fact at tool-rt-call-1 (tool_use_id must have reached the server intact), got %d", len(tools))
	}
	if !strings.Contains(tools[0].Body, "bash") || !strings.Contains(tools[0].Body, "file1") {
		t.Fatalf("PostToolUse fact body missing expected content: %q", tools[0].Body)
	}
	if tools[0].SourceRef != "pi" {
		t.Fatalf("PostToolUse SourceRef = %q, want pi", tools[0].SourceRef)
	}

	// Stop, from agent_settled: body must be non-empty and equal to
	// turn_end's cached assistant text, not the "status=settled" fallback
	// this test's turn_end call gives it something real to cache first.
	stops := recall("/agent-sessions/"+sid+"/stop", 5)
	if len(stops) != 1 {
		t.Fatalf("expected exactly one Stop fact, got %d", len(stops))
	}
	if stops[0].Body == "" {
		t.Fatal("Stop fact body must not be empty")
	}
	if stops[0].Body != "final answer text" {
		t.Fatalf("Stop fact body = %q, want %q", stops[0].Body, "final answer text")
	}
	if stops[0].SourceRef != "pi" {
		t.Fatalf("Stop SourceRef = %q, want pi", stops[0].SourceRef)
	}

	// Injection success path: the driver's before_agent_start call printed
	// the JSON-encoded result it got back, delimited so it can be pulled
	// out of the harness's combined stdout/stderr without caring about
	// ordering against console.error noise from the earlier hooks.
	const startMarker = "BEFORE_AGENT_START_RESULT_JSON_START"
	const endMarker = "BEFORE_AGENT_START_RESULT_JSON_END"
	startIdx := strings.Index(string(out), startMarker)
	endIdx := strings.Index(string(out), endMarker)
	if startIdx < 0 || endIdx < 0 || endIdx < startIdx {
		t.Fatalf("driver output missing before_agent_start result markers: %s", out)
	}
	resultJSON := string(out)[startIdx+len(startMarker) : endIdx]
	var result struct {
		SystemPrompt string `json:"systemPrompt"`
	}
	if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
		t.Fatalf("before_agent_start result not decodable: %v: %s", err, resultJSON)
	}
	if !strings.Contains(result.SystemPrompt, "BASE PROMPT") {
		t.Fatalf("expected base systemPrompt preserved, got: %q", result.SystemPrompt)
	}
	if !strings.Contains(result.SystemPrompt, piNodeRTContextFactBody) {
		t.Fatalf("expected injected context in systemPrompt, got: %q", result.SystemPrompt)
	}

	// Once-per-session injection gate: the driver's second
	// before_agent_start call, same sessionID (fakeCtx) as the call above,
	// must return undefined - the extension's injectedSessions Set already
	// marked this session injected, so a second submitted prompt in the
	// same session must not re-inject context.
	const secondStartMarker = "SECOND_CALL_RESULT_JSON_START"
	const secondEndMarker = "SECOND_CALL_RESULT_JSON_END"
	secondStartIdx := strings.Index(string(out), secondStartMarker)
	secondEndIdx := strings.Index(string(out), secondEndMarker)
	if secondStartIdx < 0 || secondEndIdx < 0 || secondEndIdx < secondStartIdx {
		t.Fatalf("driver output missing second-call result markers: %s", out)
	}
	secondResultJSON := strings.TrimSpace(string(out)[secondStartIdx+len(secondStartMarker) : secondEndIdx])
	if secondResultJSON != "null" {
		t.Fatalf("expected before_agent_start to return undefined on a second call for the same session (once-per-session gate), got: %s", secondResultJSON)
	}

	// Finding #3: event.systemPrompt absent (or empty) must never ship a
	// punk-only system prompt - before_agent_start must return undefined,
	// never { systemPrompt: "\n\n" + context } with no real base prompt.
	const noPromptStartMarker = "NO_SYSTEM_PROMPT_RESULT_JSON_START"
	const noPromptEndMarker = "NO_SYSTEM_PROMPT_RESULT_JSON_END"
	noPromptStartIdx := strings.Index(string(out), noPromptStartMarker)
	noPromptEndIdx := strings.Index(string(out), noPromptEndMarker)
	if noPromptStartIdx < 0 || noPromptEndIdx < 0 || noPromptEndIdx < noPromptStartIdx {
		t.Fatalf("driver output missing no-system-prompt result markers: %s", out)
	}
	noPromptResultJSON := strings.TrimSpace(string(out)[noPromptStartIdx+len(noPromptStartMarker) : noPromptEndIdx])
	if noPromptResultJSON != "null" {
		t.Fatalf("expected before_agent_start to return undefined when event.systemPrompt is absent (never a punk-only system prompt), got: %s", noPromptResultJSON)
	}

	// Finding #4: ctx.sessionManager.getSessionId() resolving empty logs a
	// diagnostic exactly once per extension instance (not once per call) -
	// the driver triggered it twice above via noSessionManagerCtx.
	const emptySessionIDDiagnostic = "ctx.sessionManager.getSessionId() resolved empty"
	if got := strings.Count(string(out), emptySessionIDDiagnostic); got != 1 {
		t.Fatalf("expected the empty-session-id diagnostic exactly once across the whole extension instance, got %d: %s", got, out)
	}
}
