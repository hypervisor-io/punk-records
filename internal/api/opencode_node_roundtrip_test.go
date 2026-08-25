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

// openCodeNodeRTCWD and openCodeNodeRTSessionID are the fixed test
// constants the JS driver below is built from: both are spliced into the
// driver source as Go string concatenation (`+ openCodeNodeRTCWD +`, see
// the driver template), not retyped by hand as a second JS-side literal -
// there is nothing to fall "out of sync", since the driver's copy IS these
// constants, produced by the Go compiler at build time from the same
// identifier. Changing either constant changes both sides automatically.
const (
	openCodeNodeRTCWD       = "/tmp/punk-opencode-node-rt-project"
	openCodeNodeRTSessionID = "oc-node-rt-session-1"
)

// openCodeNodeRTContextFactBody is the sentinel content seeded into memory
// (see the srv.mem.Write call below) before the driver runs, so
// experimental.chat.system.transform's injection has real project memory
// to fetch and inject. Distinct enough that a substring match against it
// can't accidentally hit anything else this test writes.
const openCodeNodeRTContextFactBody = "opencode node round trip context injection sentinel fact 8f3c21"

// TestOpenCodePluginNodeRoundTripsThroughRealServer is finding #1's fix:
// TestOpenCodeHookRoundTripsThroughServerContract (hookcli_roundtrip_test.go)
// only proves the SERVER accepts hand-written envelope literals - it can
// never detect drift in the JS plugin TEMPLATE itself (a renamed field, a
// removed role gate, a broken tool_use_id) because nothing in that test
// ever executes the template's actual JS.
//
// This test closes that gap by driving the REAL rendered plugin
// (hookcli.ConnectOpenCode's output, not a paraphrase of it) under node
// against a live httptest.Server wrapping this package's own real
// Server.Router() - the exact handleAgentHook (and, for injection, the
// exact GET /v1/agent/context handler) a production deployment would run.
// The node driver invokes: event(session.created), chat.message with a
// real user prompt (one normal text part plus a synthetic and an ignored
// distractor part, per @opencode-ai/sdk's TextPart), chat.message with an
// assistant-role distractor message (must produce no capture at all - the
// role gate), chat.message with NO messageID (covers the fnv1aHex
// fallback prompt_id path for real, finding #2), tool.execute.after,
// event(session.idle), and experimental.chat.system.transform (the
// injection path: a fact is seeded through the real server BEFORE the
// driver runs, and the driver's call must come back with that fact's
// content pushed onto output.system). Every POST the plugin issues lands
// on the real server; assertions below read the stored facts back out, or
// the driver's own captured output.system array, to prove the whole
// chain - JS hook body -> HTTP envelope -> handleAgentHook/handleAgentContext
// -> stored/recalled fact - actually holds in both directions (capture and
// injection).
//
// The injection assertion is what kills the surviving "rename data.context"
// mutation: nothing else in this package's test suite ever executes the
// plugin's own `if (data && typeof data.context === "string" ...)` check
// in opencode_plugin.go's "experimental.chat.system.transform" hook, so a
// mutation that renames the field the plugin reads off the parsed response
// (e.g. data.context -> data.contextx) previously left every test green:
// TestOpenCodeHookRoundTripsThroughServerContract only proves the SERVER
// accepts hand-typed envelope literals on the CAPTURE path, and nothing
// before this addition ever drove the injection hook itself. Verified by
// mutation: temporarily renaming that field access in opencode_plugin.go
// makes this test's injection assertions below fail, and reverting it
// restores a pass.
//
// Skipped when node is not on PATH (mirrors every other node-driven test
// in internal/hookcli).
func TestOpenCodePluginNodeRoundTripsThroughRealServer(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping opencode plugin node round-trip test")
	}

	srv := testServer(t)
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()

	// Seed a durable, high-importance fact under the same namespace
	// AgentNamespace(openCodeNodeRTCWD) derives, BEFORE the driver runs -
	// mirroring TestAgentContext's own pattern (agent_handlers_test.go) -
	// so experimental.chat.system.transform's GET /v1/agent/context has
	// real project memory to fetch and inject. High importance keeps it
	// above the assembly threshold; it is written directly via srv.mem
	// (not through a hook), so it's real project knowledge, not the
	// ephemeral /agent-sessions/ capture the driver's other hooks
	// produce and that context assembly deliberately excludes.
	seedCtx := context.Background()
	if _, err := srv.mem.Write(seedCtx, memory.WriteInput{
		Namespace:  AgentNamespace(openCodeNodeRTCWD),
		Key:        "/decisions/opencode-node-rt-context",
		Body:       openCodeNodeRTContextFactBody,
		Author:     "test",
		Writer:     "test",
		Importance: 0.8,
	}); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	pluginPath := filepath.Join(dir, "punk-memory.js")
	if _, err := hookcli.ConnectOpenCode(pluginPath, httpSrv.URL); err != nil {
		t.Fatal(err)
	}
	pluginSrc, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatal(err)
	}

	// The driver is appended to the ACTUAL rendered plugin source in one
	// .mjs file (unambiguously ESM by extension, node's CommonJS-by-default
	// loader otherwise rejects top-level "export" under --check/execution -
	// see internal/hookcli/connect_opencode_test.go's runJSSyntaxCheck doc
	// comment for the same reasoning), so this exercises the exact bytes
	// ConnectOpenCode writes.
	//
	// fetch is monkey-patched to collect every in-flight request promise:
	// the OBSERVATIONAL hooks (event, tool.execute.after, chat.message)
	// deliberately do not await postHook(...) internally (see
	// opencode_plugin.go's "Hook classification" doc comment), so calling
	// e.g. `await hooks.event(...)` resolves almost immediately without
	// waiting for the underlying HTTP request to complete. Calling an
	// async function runs it synchronously up to its first await though,
	// and every hook body here calls postHook (and therefore the global
	// fetch) before any await of its own - so by the time each hook call
	// returns, the real fetch() has already been invoked and captured into
	// `pending`. Awaiting Promise.allSettled(pending) before the driver
	// exits is what guarantees every POST has actually reached the server
	// before Go reads the stored facts back out.
	driver := `
const pending = []
const realFetch = fetch
globalThis.fetch = (...args) => {
  const p = realFetch(...args)
  pending.push(p.catch(() => {}))
  return p
}

async function main() {
  const directory = "` + openCodeNodeRTCWD + `"
  const sessionID = "` + openCodeNodeRTSessionID + `"
  const hooks = await PunkMemoryPlugin({ directory })

  await hooks.event({ event: { type: "session.created", properties: { info: { id: sessionID } } } })

  // Real user prompt: one genuine text part plus a synthetic and an
  // ignored distractor part (@opencode-ai/sdk TextPart's own optional
  // booleans) that must NOT appear in the captured prompt text.
  await hooks["chat.message"](
    { sessionID, messageID: "rt-msg-real" },
    {
      message: { role: "user" },
      parts: [
        { type: "text", text: "real prompt part one" },
        { type: "text", text: "synthetic distractor part", synthetic: true },
        { type: "text", text: "ignored distractor part", ignored: true },
      ],
    }
  )

  // Assistant-role distractor: the role gate must drop this before it
  // ever reaches postHook/fetch - if the gate is removed, this becomes a
  // THIRD stored prompt fact and the Go-side "exactly two" assertion
  // below fails.
  await hooks["chat.message"](
    { sessionID, messageID: "rt-msg-assistant" },
    {
      message: { role: "assistant" },
      parts: [{ type: "text", text: "assistant distractor text should never be stored" }],
    }
  )

  // No messageID: exercises the fnv1aHex-derived fallback prompt_id for
  // real (finding #2) rather than a hand-hardcoded Go-side guess at it.
  await hooks["chat.message"](
    { sessionID },
    {
      message: { role: "user" },
      parts: [{ type: "text", text: "fallback id prompt text" }],
    }
  )

  await hooks["tool.execute.after"](
    { sessionID, callID: "rt-call-1", tool: "bash", args: { command: "ls -la" } },
    { output: "file1\nfile2" }
  )

  await hooks.event({ event: { type: "session.idle", properties: { sessionID } } })

  // Injection success path (finding #1's fix): a fact was seeded through
  // the real server, under this same directory's namespace, before this
  // driver started (see the Go-side srv.mem.Write above). This hook
  // AWAITS its network call (it is the one BLOCKING hook, per
  // opencode_plugin.go's "Hook classification" comment), so by the time
  // it returns, output.system has already been mutated in place if
  // injection happened - no need to wait on "pending" for this one.
  const contextOutput = { system: [] }
  await hooks["experimental.chat.system.transform"]({ sessionID }, contextOutput)
  console.log("SYSTEM_CONTEXT_JSON_START" + JSON.stringify(contextOutput.system) + "SYSTEM_CONTEXT_JSON_END")

  // Once-per-session injection gate: a second experimental.chat.system.transform
  // call for the SAME sessionID must push nothing onto output.system - the
  // plugin's own injectedSessions Set already marked this sessionID
  // injected on the call above.
  const secondContextOutput = { system: [] }
  await hooks["experimental.chat.system.transform"]({ sessionID }, secondContextOutput)
  console.log("SECOND_TRANSFORM_RESULT_START" + JSON.stringify(secondContextOutput.system) + "SECOND_TRANSFORM_RESULT_END")

  await Promise.allSettled(pending)
  console.log("driver done, requests=" + pending.length)
}
main().catch((err) => {
  console.error("FAIL: driver rejected:", err)
  process.exit(1)
})
`
	harnessPath := pluginPath + ".roundtrip-harness.mjs"
	if err := os.WriteFile(harnessPath, append(pluginSrc, []byte(driver)...), 0o644); err != nil {
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

	ns := AgentNamespace(openCodeNodeRTCWD)
	sid := sanitizeID(openCodeNodeRTSessionID)
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

	// SessionStart landed, sourced from opencode. The recall limit here is
	// deliberately well above the one fact actually expected (mirroring
	// the prompts query below, which already used limit=10 for its own
	// "exactly 2" check): a recall limit of exactly 1 would cap the
	// result at 1 even if the driver's "event" hook regressed and fired
	// session.created twice, silently turning a genuine "exactly one"
	// assertion into a no-op that always passes at the cap.
	starts := recall("/agent-sessions/"+sid+"/start", 5)
	if len(starts) != 1 {
		t.Fatalf("expected exactly one SessionStart fact, got %d", len(starts))
	}
	if starts[0].SourceRef != "opencode" {
		t.Fatalf("SessionStart SourceRef = %q, want opencode", starts[0].SourceRef)
	}

	// Exactly two prompt facts: the real one and the fallback-id one. If
	// the role gate regresses (finding surviving mutation #2), the
	// assistant distractor becomes a third fact here and this fails.
	prompts := recall("/agent-sessions/"+sid+"/prompt-", 10)
	if len(prompts) != 2 {
		t.Fatalf("expected exactly 2 UserPromptSubmit facts (real + fallback-id; assistant message must be excluded), got %d: %+v", len(prompts), prompts)
	}
	var sawReal, sawFallback bool
	for _, p := range prompts {
		if p.SourceRef != "opencode" {
			t.Fatalf("prompt fact %s SourceRef = %q, want opencode", p.Key, p.SourceRef)
		}
		if strings.Contains(p.Body, "assistant distractor") {
			t.Fatalf("assistant-role message must never be captured, found in %s: %q", p.Key, p.Body)
		}
		if strings.Contains(p.Body, "synthetic distractor") || strings.Contains(p.Body, "ignored distractor") {
			t.Fatalf("synthetic/ignored text parts must be excluded from the captured prompt, found in %s: %q", p.Key, p.Body)
		}
		switch {
		case strings.Contains(p.Body, "real prompt part one"):
			sawReal = true
			if p.Key != "/agent-sessions/"+sid+"/prompt-rt-msg-real" {
				t.Fatalf("real prompt fact key = %q, want the verbatim messageID-derived key", p.Key)
			}
		case strings.Contains(p.Body, "fallback id prompt text"):
			sawFallback = true
			if p.Key == "/agent-sessions/"+sid+"/prompt-rt-msg-real" {
				t.Fatalf("fallback-id prompt must not reuse the real prompt's key")
			}
			if !strings.HasPrefix(p.Key, "/agent-sessions/"+sid+"/prompt-msg-") {
				t.Fatalf("fallback-id prompt key = %q, want the fnv1aHex-derived \"msg-\"-prefixed fallback id shape", p.Key)
			}
		default:
			t.Fatalf("unexpected prompt fact body: %q", p.Body)
		}
	}
	if !sawReal || !sawFallback {
		t.Fatalf("missing expected prompt facts: sawReal=%v sawFallback=%v: %+v", sawReal, sawFallback, prompts)
	}

	// PostToolUse: this is where an empty/mutated tool_use_id (surviving
	// mutation #1) would fail - the server drops any PostToolUse whose
	// tool_use_id sanitizes to empty as 200 "ignored" rather than storing
	// it, so a mutated template leaves no fact at this key at all. Limit
	// raised above 1 for the same "exactly one" cardinality reason as
	// starts/stops above.
	tools := recall("/agent-sessions/"+sid+"/tool-rt-call-1", 5)
	if len(tools) != 1 {
		t.Fatalf("expected exactly one PostToolUse fact at tool-rt-call-1 (tool_use_id must have reached the server intact), got %d", len(tools))
	}
	if !strings.Contains(tools[0].Body, "bash") || !strings.Contains(tools[0].Body, "file1") {
		t.Fatalf("PostToolUse fact body missing expected content: %q", tools[0].Body)
	}
	if tools[0].SourceRef != "opencode" {
		t.Fatalf("PostToolUse SourceRef = %q, want opencode", tools[0].SourceRef)
	}

	// Stop, from session.idle: body must be non-empty ("status=idle"),
	// not the empty-body regression the plugin's own doc comment warns
	// about. Limit raised above 1 for the same reason as starts/tools
	// above.
	stops := recall("/agent-sessions/"+sid+"/stop", 5)
	if len(stops) != 1 {
		t.Fatalf("expected exactly one Stop fact, got %d", len(stops))
	}
	if stops[0].Body == "" {
		t.Fatal("Stop fact body must not be empty")
	}
	if stops[0].Body != "status=idle" {
		t.Fatalf("Stop fact body = %q, want %q", stops[0].Body, "status=idle")
	}
	if stops[0].SourceRef != "opencode" {
		t.Fatalf("Stop SourceRef = %q, want opencode", stops[0].SourceRef)
	}

	// Injection success path (finding #1's fix, and the assertion that
	// kills the surviving "rename data.context" mutation - see this
	// test's own doc comment above): the driver's
	// experimental.chat.system.transform call printed the JSON-encoded
	// output.system array it ended up with, delimited so it can be pulled
	// out of the harness's combined stdout/stderr without caring about
	// ordering against console.error noise from the earlier hooks.
	const startMarker = "SYSTEM_CONTEXT_JSON_START"
	const endMarker = "SYSTEM_CONTEXT_JSON_END"
	startIdx := strings.Index(string(out), startMarker)
	endIdx := strings.Index(string(out), endMarker)
	if startIdx < 0 || endIdx < 0 || endIdx < startIdx {
		t.Fatalf("driver output missing system-context markers: %s", out)
	}
	systemJSON := string(out)[startIdx+len(startMarker) : endIdx]
	var system []string
	if err := json.Unmarshal([]byte(systemJSON), &system); err != nil {
		t.Fatalf("driver's captured output.system not decodable as a JSON string array: %v: %s", err, systemJSON)
	}
	if len(system) != 1 {
		t.Fatalf("expected experimental.chat.system.transform to push exactly one context block onto output.system, got %d: %v", len(system), system)
	}
	if !strings.Contains(system[0], openCodeNodeRTContextFactBody) {
		t.Fatalf("injected system context missing the seeded fact's content: %q", system[0])
	}

	// Once-per-session injection gate: the driver's second
	// experimental.chat.system.transform call, same sessionID as above,
	// must gain nothing - the plugin's injectedSessions Set already marked
	// this session injected, so a second idle/context-transform call in
	// the same session must not push a second context block.
	const secondStartMarker = "SECOND_TRANSFORM_RESULT_START"
	const secondEndMarker = "SECOND_TRANSFORM_RESULT_END"
	secondStartIdx := strings.Index(string(out), secondStartMarker)
	secondEndIdx := strings.Index(string(out), secondEndMarker)
	if secondStartIdx < 0 || secondEndIdx < 0 || secondEndIdx < secondStartIdx {
		t.Fatalf("driver output missing second-transform-call markers: %s", out)
	}
	secondSystemJSON := string(out)[secondStartIdx+len(secondStartMarker) : secondEndIdx]
	var secondSystem []string
	if err := json.Unmarshal([]byte(secondSystemJSON), &secondSystem); err != nil {
		t.Fatalf("second transform call's captured output.system not decodable as a JSON string array: %v: %s", err, secondSystemJSON)
	}
	if len(secondSystem) != 0 {
		t.Fatalf("expected a second experimental.chat.system.transform call for the same session (once-per-session gate) to push nothing onto output.system, got %d: %v", len(secondSystem), secondSystem)
	}
}
