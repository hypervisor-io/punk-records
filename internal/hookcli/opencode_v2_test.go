package hookcli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// openCodeV2FakeCtx is a minimal OpenCode 2.x plugin Context
// (@opencode/plugin@2.2.0 promise/plugin.d.ts): location, an event domain
// whose subscribe() returns an async iterator the test drives, tool.hook /
// session.hook registries returning Registration objects with dispose(),
// and a session domain with prompt/list/wait spies. Spliced into drivers
// that run alongside the real generated plugin source.
const openCodeV2FakeCtx = `
function makeFakeCtx(opts) {
  opts = opts || {}
  const ctx = {
    location: { directory: "/tmp/punk-v2-proj", project: { id: "p1" } },
    hooks: { tool: {}, session: {} },
    disposed: [],
    subscribeCalls: [],
    pushed: [],
    promptSpy: [],
    iterReturned: 0,
  }
  const queue = []
  let wake = null
  ctx.emit = (ev) => {
    queue.push(ev)
    if (wake) {
      const w = wake
      wake = null
      w()
    }
  }
  ctx.event = {
    subscribe(o) {
      ctx.subscribeCalls.push(o)
      const signal = o && o.signal
      return {
        [Symbol.asyncIterator]() {
          return {
            async next() {
              for (;;) {
                if (signal && signal.aborted) return { done: true, value: undefined }
                if (queue.length) return { done: false, value: queue.shift() }
                await new Promise((resolve) => {
                  wake = resolve
                  if (signal) signal.addEventListener("abort", resolve, { once: true })
                })
              }
            },
            async return() {
              ctx.iterReturned++
              return { done: true, value: undefined }
            },
          }
        },
      }
    },
  }
  const reg = (domain) => async (name, cb) => {
    if (opts.rejectHook === domain + ":" + name) throw new Error("registration refused")
    ctx.hooks[domain][name] = cb
    return { dispose: async () => { ctx.disposed.push(domain + ":" + name); delete ctx.hooks[domain][name] } }
  }
  ctx.tool = { hook: reg("tool") }
  ctx.session = {
    hook: reg("session"),
    prompt: async (input) => {
      ctx.promptSpy.push(input)
      return { id: "msg-1", sessionID: input.sessionID, type: "user", payload: { text: input.text }, delivery: "queue" }
    },
    list: async () => ({ data: opts.sessions || [], cursor: {} }),
    wait: async () => {},
  }
  return ctx
}
`

// runNodeESM writes src to a temp .mjs, runs it under node with env and a
// 25s deadline, and returns combined output (t.Fatal on failure).
func runNodeESM(t *testing.T, dir, name, src string, env map[string]string) string {
	t.Helper()
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping opencode v2 test")
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, nodePath, p)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("node did not complete within 25s:\n%s", out)
	}
	if err != nil {
		t.Fatalf("node failed: %v\n%s", err, out)
	}
	return string(out)
}

// TestOpenCodePluginDefaultDefinitionShape imports the REAL generated
// module (no concatenation) and checks the contract both OpenCode majors
// read: a default definition with a string id and setup() (2.x) and
// server() (1.x >= 1.18), plus the legacy named export still present.
// default.server must produce the same four v1 hooks plus dispose.
func TestOpenCodePluginDefaultDefinitionShape(t *testing.T) {
	dir := t.TempDir()
	pluginPath := filepath.Join(dir, "punk-memory.mjs")
	if _, err := ConnectOpenCode(pluginPath, "http://punk.test"); err != nil {
		t.Fatal(err)
	}
	src := fmt.Sprintf(`
import def, * as mod from %s
const fail = (m) => { console.error("FAIL: " + m); process.exit(1) }
if (!def || typeof def !== "object") fail("default export must be an object")
if (def.id !== "punk-memory") fail("default.id = " + def.id)
if (typeof def.setup !== "function") fail("default.setup must be a function")
if (typeof def.server !== "function") fail("default.server must be a function")
if (typeof mod.PunkMemoryPlugin !== "function") fail("named PunkMemoryPlugin export must remain")
const hooks = await def.server({ directory: "/tmp/x", client: {} })
for (const k of ["event", "tool.execute.after", "chat.message", "experimental.chat.system.transform", "dispose"]) {
  if (typeof hooks[k] !== "function") fail("v1 hook missing via default.server: " + k)
}
console.log("PASS shape")
process.exit(0)
`, jsStringLiteral("file://"+pluginPath))
	out := runNodeESM(t, dir, "shape.mjs", src, map[string]string{"PUNK_URL": "http://127.0.0.1:1"})
	if !strings.Contains(out, "PASS shape") {
		t.Fatalf("missing PASS:\n%s", out)
	}
}

// TestOpenCodeV2SetupHooksAndCleanup drives default.setup's implementation
// (punkSetupV2, same module) against the fake 2.x context: events become
// hook POSTs, the tool/prompt hooks forward, the context hook injects the
// stored memory once per session as a SystemPart, a registration refusal
// is fail-open, and the cleanup function unregisters every hook, aborts
// the event subscription and closes the iterator.
func TestOpenCodeV2SetupHooksAndCleanup(t *testing.T) {
	driver := openCodeV2FakeCtx + `
  const hookPosts = []
  const realFetch = globalThis.fetch
  globalThis.fetch = async (url, init) => {
    const u = new URL(String(url))
    if (u.pathname === "/v1/agent/hooks") {
      hookPosts.push({ body: JSON.parse(init.body), auth: init.headers && init.headers.Authorization, signal: !!init.signal })
      return jsonResponse({ ok: true })
    }
    if (u.pathname === "/v1/agent/context") {
      punkTestServer.contextFetches++
      must(u.searchParams.get("cwd") === "/tmp/punk-v2-proj", "context fetched with ctx.location.directory as cwd, got " + u.search)
      return jsonResponse({ namespace: "ns", context: "STORED MEMORY" })
    }
    return realFetch(url, init)
  }

  const ctx = makeFakeCtx({ rejectHook: "session:compaction" })
  const cleanup = await punkSetupV2(ctx)
  must(typeof cleanup === "function", "setup returns a cleanup function")
  must(typeof ctx.hooks.tool["execute.after"] === "function", "tool execute.after hook registered")
  must(typeof ctx.hooks.session["prompt"] === "function", "session prompt hook registered")
  must(typeof ctx.hooks.session["context"] === "function", "session context hook registered")
  must(ctx.subscribeCalls.length === 1, "one event subscription")
  must(ctx.subscribeCalls[0].signal instanceof AbortSignal, "subscription is abortable")

  // events -> hook POSTs
  ctx.emit({ type: "session.created", data: { sessionID: "s1" } })
  ctx.emit({ type: "session.idle", data: { sessionID: "s1" } })
  await until(() => hookPosts.length === 2, "two event hook posts")
  must(hookPosts[0].body.hook_event_name === "SessionStart" && hookPosts[0].body.session_id === "s1", "SessionStart from session.created")
  must(hookPosts[0].body.cwd === "/tmp/punk-v2-proj" && hookPosts[0].body.source === "opencode", "SessionStart cwd/source")
  must(hookPosts[1].body.hook_event_name === "Stop" && hookPosts[1].body.last_assistant_message === "status=idle", "Stop from session.idle")
  must(hookPosts[0].signal === true, "hook POST carries the 2s abort signal")

  // tool.execute.after
  ctx.hooks.tool["execute.after"]({ tool: "bash", sessionID: "s1", id: "call1", input: { cmd: "ls" }, status: "completed", result: { content: "file.txt", output: undefined } })
  await until(() => hookPosts.length === 3, "tool hook post")
  const tp = hookPosts[2].body
  must(tp.hook_event_name === "PostToolUse" && tp.tool_name === "bash" && tp.tool_use_id === "call1" && tp.tool_response === "file.txt" && tp.tool_input.cmd === "ls", "PostToolUse fields: " + JSON.stringify(tp))

  // prompt hook: human prompt forwarded, bridge-marked prompt ignored
  ctx.hooks.session["prompt"]({ sessionID: "s1", messageID: "m9", prompt: { text: "hello there" }, metadata: {}, delivery: "queue" })
  ctx.hooks.session["prompt"]({ sessionID: "s1", messageID: "m10", prompt: { text: "bridge text" }, metadata: { punk_bridge: true }, delivery: "queue" })
  await until(() => hookPosts.length === 4, "prompt hook post")
  await sleep(50)
  must(hookPosts.length === 4, "bridge-marked prompt is not captured, posts=" + hookPosts.length)
  must(hookPosts[3].body.hook_event_name === "UserPromptSubmit" && hookPosts[3].body.prompt === "hello there" && hookPosts[3].body.prompt_id === "m9", "UserPromptSubmit fields: " + JSON.stringify(hookPosts[3].body))

  // context hook: injected once per session, as a SystemPart
  const ev1 = { sessionID: "s1", system: [{ type: "text", text: "base" }] }
  await ctx.hooks.session["context"](ev1)
  must(ev1.system.length === 2 && ev1.system[1].type === "text" && ev1.system[1].text === "STORED MEMORY", "context injected as SystemPart: " + JSON.stringify(ev1.system))
  const ev2 = { sessionID: "s1", system: [] }
  await ctx.hooks.session["context"](ev2)
  must(ev2.system.length === 0 && punkTestServer.contextFetches === 1, "context fetched once per session")

  // cleanup
  await cleanup()
  must(ctx.subscribeCalls[0].signal.aborted, "cleanup aborts the event subscription")
  must(ctx.disposed.indexOf("tool:execute.after") >= 0 && ctx.disposed.indexOf("session:prompt") >= 0 && ctx.disposed.indexOf("session:context") >= 0, "cleanup disposes every registration: " + ctx.disposed.join(","))
  must(Object.keys(ctx.hooks.tool).length === 0 && Object.keys(ctx.hooks.session).length === 0, "no hooks left registered")
  const before = hookPosts.length
  ctx.emit({ type: "session.idle", data: { sessionID: "s1" } })
  await sleep(100)
  must(hookPosts.length === before, "events after cleanup are ignored")

  console.log("PASS v2-setup")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, map[string]string{"PUNK_MESSAGING": "0"}, driver)
	if !strings.Contains(out, "PASS v2-setup") {
		t.Fatalf("driver did not report PASS v2-setup:\n%s", out)
	}
}

// TestOpenCodeV2EventStreamResubscribes: an event stream that ends while
// the plugin is live is re-subscribed instead of leaving the plugin deaf.
func TestOpenCodeV2EventStreamResubscribes(t *testing.T) {
	driver := openCodeV2FakeCtx + `
  const ctx = makeFakeCtx()
  let n = 0
  const orig = ctx.event.subscribe
  ctx.event.subscribe = (o) => {
    n++
    if (n === 1) return { [Symbol.asyncIterator]() { return { next: async () => ({ done: true, value: undefined }) } } }
    return orig(o)
  }
  const cleanup = await punkSetupV2(ctx)
  await until(() => n >= 2, "re-subscribe after stream end", 6000)
  await cleanup()
  const settled = n
  await sleep(1300)
  must(n === settled, "no re-subscribe after cleanup")
  console.log("PASS v2-resubscribe")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, map[string]string{"PUNK_MESSAGING": "0"}, driver)
	if !strings.Contains(out, "PASS v2-resubscribe") {
		t.Fatalf("driver did not report PASS v2-resubscribe:\n%s", out)
	}
}

// TestOpenCodeV2MessagingIdleWake: the opt-in bridge on the 2.x path binds
// a created session, delivers a queued message via ctx.session.prompt
// (bridge-marked, resume), ACKs after the successful handoff, and does not
// capture its own delivery as a human prompt.
func TestOpenCodeV2MessagingIdleWake(t *testing.T) {
	driver := fmt.Sprintf(`
  putMessage("opencode:s1", "m1", "please review the flaky test")
  const expectedEnvelope = %s
  const ctx = makeFakeCtx()
  const cleanup = await punkSetupV2(ctx)
  ctx.emit({ type: "session.created", data: { sessionID: "s1" } })
  await until(() => ctx.promptSpy.length === 1, "message delivered via ctx.session.prompt")
  await until(() => punkTestServer.ackedIds.indexOf("m1") >= 0, "m1 acked after successful prompt")
  const p = ctx.promptSpy[0]
  must(p.sessionID === "s1" && p.text === expectedEnvelope, "prompt carries the byte-exact shared envelope")
  must(p.metadata && p.metadata.punk_bridge === true && p.resume === true, "prompt is bridge-marked and resumes the session")
  must(punkTestServer.registeredAgents.some((r) => r.agent === "opencode:s1"), "registered as opencode:<sessionID>")

  // The prompt hook sees the bridge's own text (as the real host would)
  // and must not treat it as a human turn: no UserPromptSubmit, session
  // stays idle so a later message is delivered too.
  const before = fetchCalls.filter((c) => c.path === "/v1/agent/hooks" && c.body.indexOf("UserPromptSubmit") >= 0).length
  ctx.hooks.session["prompt"]({ sessionID: "s1", messageID: "mm", prompt: { text: p.text }, metadata: { punk_bridge: true }, delivery: "queue" })
  await sleep(50)
  const after = fetchCalls.filter((c) => c.path === "/v1/agent/hooks" && c.body.indexOf("UserPromptSubmit") >= 0).length
  must(after === before, "bridge delivery not captured as a human prompt")

  // A human prompt marks busy: the next message defers until idle.
  ctx.hooks.session["prompt"]({ sessionID: "s1", messageID: "h1", prompt: { text: "human" }, metadata: {}, delivery: "queue" })
  await sleep(30)
  putMessage("opencode:s1", "m2", "second")
  emitHint("opencode:s1")
  await sleep(200)
  must(ctx.promptSpy.length === 1, "busy session defers delivery")
  ctx.emit({ type: "session.idle", data: { sessionID: "s1" } })
  await until(() => ctx.promptSpy.length === 2, "delivered after session.idle")
  await until(() => punkTestServer.ackedIds.indexOf("m2") >= 0, "m2 acked")

  await cleanup()
  await until(() => punkTestServer.sseAborted.length > 0, "cleanup aborts the SSE stream")
  console.log("PASS v2-messaging")
  process.exit(0)
`, jsStringLiteral(expectedOpenCodeEnvelope("opencode:s1", "m1", "planner-agent", "please review the flaky test")))
	driver = openCodeV2FakeCtx + driver
	out := runOpenCodeMessagingHarness(t, nil, driver)
	if !strings.Contains(out, "PASS v2-messaging") {
		t.Fatalf("driver did not report PASS v2-messaging:\n%s", out)
	}
}
