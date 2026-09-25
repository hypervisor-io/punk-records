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

// openClawMessagingHarnessPrelude is the shared JS prelude appended (after
// the real rendered plugin source, with its `export default` line stripped
// so the driver can call register() directly) to every OpenClaw
// messaging-bridge behavioral harness: the same lease-aware fake
// punk-records HTTP surface the Pi harness uses (minus the SSE stream -
// any attempt to open one is RECORDED as a violation, because the verified
// OpenClaw contract is catch-up only and the bridge must never hold a
// stream or start a turn) plus a fake plugin-api object whose on()
// records every registered hook and handler.
//
// The verified contract this harness fakes (2026-09-25,
// /research/extension-clients/openclaw): api.on(<hook>, handler) with
// before_prompt_build receiving {prompt, messages} (+ currentUserMessage /
// currentUserMessageId on harnesses that supply them) and returning
// {prependContext} among other fields; session_start carrying
// {sessionKey, sessionId, reason}; ctx.sessionId / ctx.sessionKey / ctx.runId
// optional on many hooks; gateway_stop as the lifecycle teardown hook.
const openClawMessagingHarnessPrelude = `
// ---- punk-records openclaw messaging test harness: fake server + fake api ----
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))

const fetchCalls = []
const registeredHooks = {}
const punkTestServer = {
  ns: "agent-test-ns",
  memoryContext: "",
  contextFetches: 0,
  inbox: {}, // agent -> [{id, sender, recipient, body, created_at, leasedUntil, leasedBy}]
  ackedIds: [], // {id, owner}
  ackCalls: 0,
  ackFailNext: 0,
  ackZeroNext: false, // one-shot: answer the next ACK {acked:0} (expired lease)
  messageFailAfter: -1, // let N leased reads succeed, then fail one (-1 = never)
  oversizeBatches: 0, // ACK/release calls above the server's 100-id bound (must stay 0)
  ackBatches: [], // ids length per ACK call
  releaseBatches: [], // ids length per release call
  releaseCalls: [], // {id, owner, mismatch}
  memberAttempts: 0,
  memberFailNext: 0,
  registeredAgents: [],
  sseAttempts: 0, // any /messages/events fetch is a catch-up-only violation
}

function jsonResponse(obj) {
  return new Response(JSON.stringify(obj), { status: 200, headers: { "Content-Type": "application/json" } })
}

globalThis.fetch = async function (url, init) {
  const u = new URL(String(url))
  const method = (init && init.method) || "GET"
  fetchCalls.push({ method: method, path: u.pathname, query: u.search, body: init && init.body ? String(init.body) : "" })

  const nsMatch = u.pathname.match(/^\/v1\/namespaces\/([^/]+)\/(.*)$/)
  if (nsMatch) {
    const rest = nsMatch[2]
    if (rest === "messages/events") {
      punkTestServer.sseAttempts++
      return new Response("catch-up only: the openclaw bridge must never open an SSE stream", { status: 500 })
    }
    if (rest === "members") {
      punkTestServer.memberAttempts++
      if (punkTestServer.memberFailNext > 0) {
        punkTestServer.memberFailNext--
        return new Response("simulated registration failure", { status: 500 })
      }
      const body = JSON.parse(init.body)
      punkTestServer.registeredAgents.push(body)
      return jsonResponse({ namespace: nsMatch[1], agent: body.agent, status: "registered" })
    }
    if (rest === "messages/ack") {
      punkTestServer.ackCalls++
      const ackBody = JSON.parse(init.body)
      punkTestServer.ackBatches.push(ackBody.ids.length)
      if (ackBody.ids.length > 100) {
        // Faithful to the real server: the id list is rejected whole.
        punkTestServer.oversizeBatches++
        return new Response("ids exceed batch bound", { status: 400 })
      }
      if (punkTestServer.ackFailNext > 0) {
        punkTestServer.ackFailNext--
        return new Response("simulated ack failure", { status: 500 })
      }
      const body = ackBody
      if (punkTestServer.ackZeroNext) {
        // The real server answers {acked:0} (HTTP 200) when the lease
        // expired before the ACK: only a LIVE lease of THIS owner acks.
        punkTestServer.ackZeroNext = false
        return jsonResponse({ acked: 0 })
      }
      let count = 0
      const now = Date.now()
      for (const id of body.ids) {
        const row = (punkTestServer.inbox[body.agent] || []).find((m) => m.id === id)
        if (!row || row.leasedBy !== body.leased_by || !(row.leasedUntil > now)) continue
        punkTestServer.inbox[body.agent] = (punkTestServer.inbox[body.agent] || []).filter((m) => m.id !== id)
        punkTestServer.ackedIds.push({ id: id, owner: body.leased_by })
        count++
      }
      return jsonResponse({ acked: count })
    }
    if (rest === "messages/release") {
      const body = JSON.parse(init.body)
      punkTestServer.releaseBatches.push(body.ids.length)
      if (body.ids.length > 100) {
        punkTestServer.oversizeBatches++
        return new Response("ids exceed batch bound", { status: 400 })
      }
      let cleared = 0
      for (const id of body.ids) {
        const row = (punkTestServer.inbox[body.agent] || []).find((m) => m.id === id)
        const mismatch = !row || row.leasedBy !== body.leased_by
        punkTestServer.releaseCalls.push({ id: id, owner: body.leased_by, mismatch: mismatch })
        if (row && !mismatch) {
          row.leasedUntil = 0
          row.leasedBy = null
          cleared++
        }
      }
      return jsonResponse({ released: cleared })
    }
    if (rest === "messages") {
      const agent = u.searchParams.get("agent")
      const limit = parseInt(u.searchParams.get("limit") || "100", 10)
      const leaseSeconds = parseInt(u.searchParams.get("lease_seconds") || "0", 10)
      const leasedBy = u.searchParams.get("leased_by") || ""
      const now = Date.now()
      // M10 lease semantics, faithful to ReadMessagesWithOptions: a row
      // with a LIVE lease is invisible to EVERY reader, including the
      // lease's own owner, until the lease expires; and only the rows
      // actually RETURNED (after the limit) are leased.
      const visible = (punkTestServer.inbox[agent] || []).filter((m) => !(m.leasedUntil > now))
      const rows = visible.slice(0, limit)
      for (const m of rows) {
        m.leasedUntil = now + leaseSeconds * 1000
        m.leasedBy = leasedBy
      }
      return jsonResponse({ messages: rows })
    }
  }
  if (u.pathname === "/v1/agent/namespace") {
    return jsonResponse({ namespace: punkTestServer.ns })
  }
  if (u.pathname === "/v1/agent/hooks") {
    return jsonResponse({ ok: true })
  }
  if (u.pathname === "/v1/agent/context") {
    punkTestServer.contextFetches++
    return jsonResponse({ namespace: punkTestServer.ns, context: punkTestServer.memoryContext })
  }
  console.error("FAIL: unexpected fetch " + method + " " + u.pathname)
  process.exit(1)
}

// Fake plugin api: every registration is recorded so tests can pin the
// exact hook set (and that nothing turn-starting is ever registered).
const fakeApi = {
  on(name, handler, opts) {
    registeredHooks[name] = { handler: handler, opts: opts }
  },
}

function putMessage(agent, id, body, sender) {
  punkTestServer.inbox[agent] = punkTestServer.inbox[agent] || []
  punkTestServer.inbox[agent].push({
    id: id,
    sender: sender || "planner-agent",
    recipient: agent,
    body: body,
    created_at: "2026-09-25T00:00:00Z",
    leasedUntil: 0,
    leasedBy: null,
  })
}

function acked(id) {
  return punkTestServer.ackedIds.some((a) => a.id === id)
}

function inboxCount(agent) {
  return (punkTestServer.inbox[agent] || []).length
}

function ocCtx(sid) {
  return { sessionId: sid, sessionKey: "chan:" + sid, runId: "r-" + sid }
}

async function fire(name, event, ctx) {
  if (!registeredHooks[name]) throw new Error("no handler wired for " + name)
  return registeredHooks[name].handler(event || {}, ctx)
}

function debugState() {
  return JSON.stringify({
    fetchCalls: fetchCalls.map((c) => c.method + " " + c.path + c.query),
    registeredHooks: Object.keys(registeredHooks),
    registered: punkTestServer.registeredAgents,
    memberAttempts: punkTestServer.memberAttempts,
    ackedIds: punkTestServer.ackedIds,
    ackCalls: punkTestServer.ackCalls,
    releaseCalls: punkTestServer.releaseCalls,
    sseAttempts: punkTestServer.sseAttempts,
  })
}

async function until(cond, label, timeoutMs) {
  const deadline = Date.now() + (timeoutMs || 4000)
  while (Date.now() < deadline) {
    if (cond()) return
    await sleep(10)
  }
  console.error("FAIL: timed out waiting for: " + label)
  console.error("state: " + debugState())
  process.exit(1)
}

function must(cond, label) {
  if (!cond) {
    console.error("FAIL: " + label)
    console.error("state: " + debugState())
    process.exit(1)
  }
}

async function main() {
`

const openClawMessagingHarnessEpilogue = `}
main().catch((err) => {
  console.error("FAIL: driver rejected: " + (err && err.stack ? err.stack : err))
  process.exit(1)
})
`

// runOpenClawMessagingHarness renders the REAL plugin via WriteOpenClawPlugin
// (never a paraphrase), strips its `export default` line so the driver can
// call register() on the named plugin object, appends the shared prelude +
// the scenario driver + the epilogue into one .mjs file, and executes it
// under node with the messaging bridge enabled. Skipped when node is not on
// PATH; the 25s hard deadline bounds a wedged driver.
func runOpenClawMessagingHarness(t *testing.T, env map[string]string, driver string) string {
	t.Helper()
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping openclaw messaging behavioral test")
	}

	defaults := map[string]string{
		"PUNK_URL":                     "http://punk.test",
		"PUNK_MESSAGING":               "1",
		"PUNK_MESSAGING_BACKOFF_MS":    "10",
		"PUNK_MESSAGING_LEASE_SECONDS": "1",
	}
	for k, v := range env {
		defaults[k] = v
	}
	cmdEnv := os.Environ()
	for k, v := range defaults {
		cmdEnv = append(cmdEnv, k+"="+v)
	}

	dir := t.TempDir()
	pluginDir := filepath.Join(dir, "plugins", OpenClawPluginID)
	if _, err := WriteOpenClawPlugin(pluginDir, "http://punk.test"); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join(pluginDir, "index.js"))
	if err != nil {
		t.Fatal(err)
	}
	rewritten := strings.Replace(string(src), "export default punkOpenClawPlugin;\n", "", 1)
	if rewritten == string(src) {
		t.Fatal("expected to strip the plugin's default export line")
	}

	harness := rewritten + openClawMessagingHarnessPrelude + driver + openClawMessagingHarnessEpilogue
	harnessPath := filepath.Join(pluginDir, "index.js.messaging-harness.mjs")
	if err := os.WriteFile(harnessPath, []byte(harness), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, nodePath, harnessPath)
	cmd.Env = cmdEnv
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("node harness did not complete within 25s:\n%s", out)
	}
	if err != nil {
		t.Fatalf("node harness failed: %v\n%s", err, out)
	}
	return string(out)
}

// expectedOpenClawEnvelope renders the exact envelope the bridge must
// prepend, through the same Go renderer punk hook inbox uses.
func expectedOpenClawEnvelope(address, id, sender, body string) string {
	return RenderInbox("agent-test-ns", address, []InboxMessage{{
		ID: id, Sender: sender, Recipient: address, Body: body, CreatedAt: "2026-09-25T00:00:00Z",
	}})
}

// TestOpenClawMessagingCatchUpPrependsInbox: the leased unread set rides
// before_prompt_build's prependContext (the one documented content seam),
// combined with the once-per-session memory recall; the reply carries
// ONLY prependContext (never any stop/continue decision - OpenClaw is
// catch-up only); the fetch carries the M10 lease; the ACK is deferred to
// the next observed fetch; and no SSE stream is ever opened.
func TestOpenClawMessagingCatchUpPrependsInbox(t *testing.T) {
	driver := fmt.Sprintf(`
  punkTestServer.memoryContext = "## Project memory"
  putMessage("openclaw:s1", "m1", "catch-up please")
  punkOpenClawPlugin.register(fakeApi)

  await fire("session_start", { sessionId: "s1", sessionKey: "chan:s1", reason: "new" }, ocCtx("s1"))
  await until(() => punkTestServer.registeredAgents.some((r) => r.agent === "openclaw:s1"), "session registered")

  const res = await fire("before_prompt_build", { prompt: "do the work", messages: [] }, ocCtx("s1"))
  must(res && typeof res.prependContext === "string", "the reply carries prependContext, got " + JSON.stringify(res))
  must(res.prependContext === "## Project memory\n\n" + %s, "memory recall and the byte-exact M5 envelope are prepended together, got " + JSON.stringify(res.prependContext))
  must(Object.keys(res).length === 1 && Object.keys(res)[0] === "prependContext", "catch-up only: the reply never carries a stop/continue decision, keys=" + JSON.stringify(Object.keys(res)))
  must(!acked("m1"), "nothing is acked at injection time (the ACK rides the next observed fetch)")

  const readCall = fetchCalls.find((c) => c.path === "/v1/namespaces/agent-test-ns/messages" && c.query.indexOf("lease_seconds=") >= 0)
  must(readCall, "the inbox fetch carries the M10 lease (lease_seconds)")
  must(readCall.query.indexOf("leased_by=openclaw-ext-") >= 0, "the inbox fetch carries the bridge lease owner")

  // A row leased to the bridge's owner is invisible to a different
  // consumer inside the lease window.
  const foreign = await fetch(
    "http://punk.test/v1/namespaces/agent-test-ns/messages?agent=" + encodeURIComponent("openclaw:s1") +
      "&limit=50&lease_seconds=15&leased_by=other-consumer"
  )
  const foreignBody = await foreign.json()
  must(foreignBody.messages.length === 0, "a different lease owner must not see the leased row, saw " + foreignBody.messages.length)

  // The next prompt build is INSIDE the lease window: the row is hidden
  // from every reader (this owner included), so nothing re-delivers and
  // nothing is acked yet - no ACK before the returned context was
  // consumed and the row became visible again.
  const res2 = await fire("before_prompt_build", { prompt: "next turn", messages: [] }, ocCtx("s1"))
  must(res2 === undefined, "a quiet later build returns undefined, got " + JSON.stringify(res2))
  must(!acked("m1"), "no ACK while the delivered row is still lease-hidden")

  // After the (1s test) lease expires, the next observed fetch re-leases
  // the row and re-acks it silently.
  await sleep(1100)
  const res3 = await fire("before_prompt_build", { prompt: "turn three", messages: [] }, ocCtx("s1"))
  must(res3 === undefined, "the re-ack turn injects nothing new, got " + JSON.stringify(res3))
  await until(() => acked("m1"), "the delivered id was re-acked by the first observed fetch after lease expiry")
  must(punkTestServer.ackedIds.some((a) => a.id === "m1" && a.owner && a.owner.indexOf("openclaw-ext-") === 0), "the ACK carries the same lease owner as the fetch")
  must(punkTestServer.contextFetches === 1, "memory recall stays once-per-session across builds")
  must(punkTestServer.sseAttempts === 0, "catch-up only: the bridge never opens an SSE stream")

  console.log("PASS catch-up")
  process.exit(0)
`, jsStringLiteral(expectedOpenClawEnvelope("openclaw:s1", "m1", "planner-agent", "catch-up please")))
	out := runOpenClawMessagingHarness(t, nil, driver)
	if !strings.Contains(out, "PASS catch-up") {
		t.Fatalf("driver did not report PASS catch-up:\n%s", out)
	}
}

// TestOpenClawMessagingAckRetryNoReRender: a failed ACK never re-renders
// the message into a later prompt - the next build attempts only the
// silent re-ack, and the one after that confirms it.
func TestOpenClawMessagingAckRetryNoReRender(t *testing.T) {
	driver := fmt.Sprintf(`
  punkTestServer.ackFailNext = 1
  putMessage("openclaw:s1", "m1", "delivered once, acked later")
  punkOpenClawPlugin.register(fakeApi)
  await fire("session_start", { sessionId: "s1", sessionKey: "chan:s1", reason: "new" }, ocCtx("s1"))
  await until(() => punkTestServer.registeredAgents.some((r) => r.agent === "openclaw:s1"), "session registered")

  const res1 = await fire("before_prompt_build", { prompt: "turn one", messages: [] }, ocCtx("s1"))
  must(res1 && res1.prependContext.indexOf(%s) >= 0, "the message was rendered on the first build")

  // The immediate second build is inside the lease window: the row is
  // hidden, nothing re-renders, and the re-ack has not even fired yet.
  const res2 = await fire("before_prompt_build", { prompt: "turn two", messages: [] }, ocCtx("s1"))
  must(res2 === undefined, "a quiet build inside the lease window renders nothing, got " + JSON.stringify(res2))
  must(punkTestServer.ackCalls === 0, "no ACK attempt before the row is visible again, ackCalls=" + punkTestServer.ackCalls)

  // After lease expiry the next build fetches the row and the re-ack
  // FAILS (injected once); the message must NOT be rendered again, and
  // the scheduled post-expiry retry must confirm the ACK.
  await sleep(1100)
  const res3 = await fire("before_prompt_build", { prompt: "turn three", messages: [] }, ocCtx("s1"))
  must(res3 === undefined, "a failed ACK must not re-render the message, got " + JSON.stringify(res3))
  must(!acked("m1"), "the re-ack genuinely failed")
  must(inboxCount("openclaw:s1") === 1, "the row is still unread server-side after the failed ACK")

  // The scheduled re-acquire pass (after the fresh lease expires)
  // re-leases the row and confirms the ACK, still without re-rendering.
  await until(() => acked("m1"), "the pending ACK was reacquired and confirmed", 6000)
  must(punkTestServer.ackCalls >= 2, "the ack was actually retried, ackCalls=" + punkTestServer.ackCalls)

  console.log("PASS ack-retry-no-rerender")
  process.exit(0)
`, jsStringLiteral(expectedOpenClawEnvelope("openclaw:s1", "m1", "planner-agent", "delivered once, acked later")))
	out := runOpenClawMessagingHarness(t, nil, driver)
	if !strings.Contains(out, "PASS ack-retry-no-rerender") {
		t.Fatalf("driver did not report PASS ack-retry-no-rerender:\n%s", out)
	}
}

// TestOpenClawMessagingRegistrationOutage: a registration outage retries
// on bounded backoff until confirmed; no inbox fetch happens before the
// confirmation, and catch-up starts on the first build after it.
func TestOpenClawMessagingRegistrationOutage(t *testing.T) {
	driver := `
  punkTestServer.memberFailNext = 3
  putMessage("openclaw:s1", "m1", "delivered once registration recovers")
  punkOpenClawPlugin.register(fakeApi)
  await fire("session_start", { sessionId: "s1", sessionKey: "chan:s1", reason: "new" }, ocCtx("s1"))

  await until(() => punkTestServer.memberAttempts >= 3, "three failing registration attempts observed")
  must(!fetchCalls.some((c) => c.path.endsWith("/messages") && c.method === "GET"), "no inbox read before a confirmed registration")
  const res1 = await fire("before_prompt_build", { prompt: "too early", messages: [] }, ocCtx("s1"))
  must(res1 === undefined, "an unregistered session injects nothing, got " + JSON.stringify(res1))

  await until(() => punkTestServer.registeredAgents.some((r) => r.agent === "openclaw:s1"), "registration retried until the server confirmed it")
  const res2 = await fire("before_prompt_build", { prompt: "now", messages: [] }, ocCtx("s1"))
  must(res2 && res2.prependContext && res2.prependContext.indexOf("delivered once registration recovers") >= 0, "catch-up starts on the first build after confirmation")

  console.log("PASS registration-outage")
  process.exit(0)
`
	out := runOpenClawMessagingHarness(t, nil, driver)
	if !strings.Contains(out, "PASS registration-outage") {
		t.Fatalf("driver did not report PASS registration-outage:\n%s", out)
	}
}

// TestOpenClawMessagingAllowlist: senders outside PUNK_MESSAGING_FROM are
// held back (released unread, unacked) while an allowed sender's message
// rides the prompt.
func TestOpenClawMessagingAllowlist(t *testing.T) {
	driver := fmt.Sprintf(`
  putMessage("openclaw:s1", "m-allow", "from the planner", "planner-agent")
  putMessage("openclaw:s1", "m-hold", "from a stranger", "random-stranger")
  punkOpenClawPlugin.register(fakeApi)
  await fire("session_start", { sessionId: "s1", sessionKey: "chan:s1", reason: "new" }, ocCtx("s1"))
  await until(() => punkTestServer.registeredAgents.some((r) => r.agent === "openclaw:s1"), "session registered")

  const res = await fire("before_prompt_build", { prompt: "work", messages: [] }, ocCtx("s1"))
  must(res && res.prependContext === %s, "only the allowed sender's message is prepended")
  must(!acked("m-hold"), "the held-back message was never acked")
  // The allowed message is delivered-pending-ack; the first observed
  // fetch AFTER lease expiry re-acks it, after which only the held-back
  // row remains unread.
  await sleep(1100)
  await fire("before_prompt_build", { prompt: "next", messages: [] }, ocCtx("s1"))
  await until(() => acked("m-allow"), "the allowed message was re-acked by the first post-expiry fetch")
  must(!acked("m-hold"), "the held-back message is still never acked")
  must(inboxCount("openclaw:s1") === 1, "only the held-back message stays unread server-side, rows=" + inboxCount("openclaw:s1"))
  must(punkTestServer.releaseCalls.some((r) => r.id === "m-hold"), "the held-back message's lease was released")

  console.log("PASS allowlist")
  process.exit(0)
`, jsStringLiteral(expectedOpenClawEnvelope("openclaw:s1", "m-allow", "planner-agent", "from the planner")))
	out := runOpenClawMessagingHarness(t, map[string]string{"PUNK_MESSAGING_FROM": "planner"}, driver)
	if !strings.Contains(out, "PASS allowlist") {
		t.Fatalf("driver did not report PASS allowlist:\n%s", out)
	}
}

// TestOpenClawMessagingGatewayStopTeardown: gateway_stop aborts every
// pending registration retry; no further attempts happen afterwards.
func TestOpenClawMessagingGatewayStopTeardown(t *testing.T) {
	driver := `
  punkTestServer.memberFailNext = 100000 // registration never succeeds
  punkOpenClawPlugin.register(fakeApi)
  await fire("session_start", { sessionId: "s1", sessionKey: "chan:s1", reason: "new" }, ocCtx("s1"))
  await until(() => punkTestServer.memberAttempts >= 2, "registration retry loop started")

  await fire("gateway_stop", {}, {})
  const attemptsAtStop = punkTestServer.memberAttempts
  await sleep(300) // far past the 10ms backoff schedule
  must(punkTestServer.memberAttempts === attemptsAtStop, "teardown must cancel pending registration retries, attempts=" + punkTestServer.memberAttempts + " (was " + attemptsAtStop + ")")

  console.log("PASS gateway-stop-teardown")
  process.exit(0)
`
	out := runOpenClawMessagingHarness(t, nil, driver)
	if !strings.Contains(out, "PASS gateway-stop-teardown") {
		t.Fatalf("driver did not report PASS gateway-stop-teardown:\n%s", out)
	}
}

// TestOpenClawMessagingDisabledBaseline: without PUNK_MESSAGING=1 the
// bridge is completely inert - no registration, no inbox reads, no SSE -
// while the memory hooks keep firing exactly as before, and the hook set
// is exactly the five documented registrations (nothing turn-starting).
func TestOpenClawMessagingDisabledBaseline(t *testing.T) {
	driver := `
  punkTestServer.memoryContext = "## Project memory"
  putMessage("openclaw:s1", "m1", "nobody is listening")
  punkOpenClawPlugin.register(fakeApi)

  await fire("session_start", { sessionId: "s1", sessionKey: "chan:s1", reason: "new" }, ocCtx("s1"))
  const res = await fire("before_prompt_build", { prompt: "work", messages: [] }, ocCtx("s1"))
  await fire("after_tool_call", { toolName: "terminal", params: {}, result: "ok" }, ocCtx("s1"))
  await fire("agent_end", { runId: "r1", messages: [], success: true, durationMs: 5 }, ocCtx("s1"))
  await fire("gateway_stop", {}, {})

  const names = Object.keys(registeredHooks).sort()
  must(JSON.stringify(names) === JSON.stringify(["after_tool_call", "agent_end", "before_prompt_build", "gateway_stop", "session_start"]), "exact hook set, got " + JSON.stringify(names))
  must(res && res.prependContext === "## Project memory", "memory recall works untouched, got " + JSON.stringify(res))
  must(!punkTestServer.registeredAgents.some((r) => r.agent === "openclaw:s1"), "no member registration when messaging is disabled")
  must(
    !fetchCalls.some((c) => c.path.startsWith("/v1/namespaces/")),
    "no namespace-scoped messaging calls happen when messaging is disabled, got " + JSON.stringify(fetchCalls.map((c) => c.path))
  )
  must(punkTestServer.sseAttempts === 0, "no SSE attempts when messaging is disabled")

  console.log("PASS disabled-baseline")
  process.exit(0)
`
	// PUNK_MESSAGING explicitly off (the default state of a real install).
	out := runOpenClawMessagingHarness(t, map[string]string{"PUNK_MESSAGING": "0"}, driver)
	if !strings.Contains(out, "PASS disabled-baseline") {
		t.Fatalf("driver did not report PASS disabled-baseline:\n%s", out)
	}
}

// TestOpenClawMessagingFailOpenDeadServer: a totally dead punk server
// must never break the hooks - every handler resolves, the prompt build
// returns only what memory could produce (here: nothing), and the session
// simply never binds.
func TestOpenClawMessagingFailOpenDeadServer(t *testing.T) {
	driver := `
  globalThis.fetch = () => Promise.reject(new Error("network down"))
  punkOpenClawPlugin.register(fakeApi)

  await fire("session_start", { sessionId: "s1", sessionKey: "chan:s1", reason: "new" }, ocCtx("s1"))
  const res = await fire("before_prompt_build", { prompt: "work", messages: [] }, ocCtx("s1"))
  await fire("after_tool_call", { toolName: "terminal", params: {}, result: "ok" }, ocCtx("s1"))
  await fire("agent_end", { runId: "r1", messages: [], success: true, durationMs: 5 }, ocCtx("s1"))
  await fire("gateway_stop", {}, {})
  must(res === undefined, "a dead server yields the plain undefined return, got " + JSON.stringify(res))

  console.log("PASS fail-open-dead-server")
  process.exit(0)
`
	out := runOpenClawMessagingHarness(t, nil, driver)
	if !strings.Contains(out, "PASS fail-open-dead-server") {
		t.Fatalf("driver did not report PASS fail-open-dead-server:\n%s", out)
	}
}

// TestOpenClawMessagingDeniedFirst50Allowed51 (allowlist starvation): a
// backlog of 50 allowlist-DENIED rows older than one fetch batch must not
// starve the allowed row behind them - the pass's second round reads past
// the leased-denied rows and the catch-up delivers the allowed message.
func TestOpenClawMessagingDeniedFirst50Allowed51(t *testing.T) {
	driver := fmt.Sprintf(`
  for (let i = 0; i < 50; i++) {
    putMessage("openclaw:s1", "denied-" + i, "noise " + i, "stranger-agent")
  }
  putMessage("openclaw:s1", "m-allow", "the one allowed message", "planner-agent")
  punkOpenClawPlugin.register(fakeApi)
  await fire("session_start", { sessionId: "s1", sessionKey: "chan:s1", reason: "new" }, ocCtx("s1"))
  await until(() => punkTestServer.registeredAgents.some((r) => r.agent === "openclaw:s1"), "session registered")

  const res = await fire("before_prompt_build", { prompt: "work", messages: [] }, ocCtx("s1"))
  must(res && res.prependContext === %s, "the allowed row behind 50 denied rows was prepended byte-exactly")

  const readCalls = fetchCalls.filter(
    (c) => c.path === "/v1/namespaces/agent-test-ns/messages" && c.method === "GET" && c.query.indexOf("lease_seconds=") >= 0
  )
  must(readCalls.length >= 2, "the pass needed a second round to read past the denied batch, rounds=" + readCalls.length)
  let deniedRows = 0
  for (const m of punkTestServer.inbox["openclaw:s1"] || []) {
    if (m.id.indexOf("denied-") === 0) deniedRows++
  }
  must(deniedRows === 50, "all 50 denied rows stay unread server-side, rows=" + deniedRows)
  must(
    inboxCount("openclaw:s1") === 51,
    "the allowed row is delivered-pending-ack (deferred host handoff: unread until the first post-expiry fetch), rows=" + inboxCount("openclaw:s1")
  )
  let releasedDenied = 0
  for (const r of punkTestServer.releaseCalls) {
    if (r.id.indexOf("denied-") === 0) releasedDenied++
  }
  must(releasedDenied === 50, "every denied row was released at pass end, released=" + releasedDenied)
  must(!punkTestServer.ackedIds.some((a) => a.id.indexOf("denied-") === 0), "no denied row was ever acked")

  console.log("PASS denied-first50-allowed51")
  process.exit(0)
`, jsStringLiteral(expectedOpenClawEnvelope("openclaw:s1", "m-allow", "planner-agent", "the one allowed message")))
	out := runOpenClawMessagingHarness(t, map[string]string{"PUNK_MESSAGING_FROM": "planner"}, driver)
	if !strings.Contains(out, "PASS denied-first50-allowed51") {
		t.Fatalf("driver did not report PASS denied-first50-allowed51:\n%s", out)
	}
}

// TestOpenClawMessagingAckZeroOnExpiredLease: the server answers
// {acked:0} (HTTP 200) when the lease expired before the ACK - that is
// NOT success. The pending mark is kept (never thrown away), nothing is
// ever re-rendered for it, and the scheduled post-expiry re-acquire
// confirms the ACK.
func TestOpenClawMessagingAckZeroOnExpiredLease(t *testing.T) {
	driver := fmt.Sprintf(`
  punkTestServer.memoryContext = "## Project memory"
  putMessage("openclaw:s1", "m1", "acked only after reacquire")
  punkOpenClawPlugin.register(fakeApi)
  await fire("session_start", { sessionId: "s1", sessionKey: "chan:s1", reason: "new" }, ocCtx("s1"))
  await until(() => punkTestServer.registeredAgents.some((r) => r.agent === "openclaw:s1"), "session registered")

  const res1 = await fire("before_prompt_build", { prompt: "turn one", messages: [] }, ocCtx("s1"))
  must(res1 && res1.prependContext.indexOf(%s) >= 0, "the message was rendered on the first build")
  must(!acked("m1"), "no ACK at injection time (deferred host handoff)")

  // After lease expiry the next build re-leases the row and the re-ack
  // answers {acked:0} - not success: the pending mark is kept and the
  // post-expiry retry confirms it, without any re-render.
  await sleep(1100)
  punkTestServer.ackZeroNext = true
  const res2 = await fire("before_prompt_build", { prompt: "turn two", messages: [] }, ocCtx("s1"))
  must(res2 === undefined, "the re-ack build renders nothing new, got " + JSON.stringify(res2))
  await sleep(200)
  must(!acked("m1"), "the {acked:0} answer left the message unacked")
  must(inboxCount("openclaw:s1") === 1, "the row is still unread server-side after the {acked:0}")

  await until(() => acked("m1"), "the pending ACK was reacquired and confirmed", 6000)
  must(punkTestServer.ackCalls >= 2, "the ack was actually retried, ackCalls=" + punkTestServer.ackCalls)
  must(punkTestServer.contextFetches === 1, "memory recall stays once-per-session across builds")

  console.log("PASS ack-zero-expired-lease")
  process.exit(0)
`, jsStringLiteral(expectedOpenClawEnvelope("openclaw:s1", "m1", "planner-agent", "acked only after reacquire")))
	out := runOpenClawMessagingHarness(t, nil, driver)
	if !strings.Contains(out, "PASS ack-zero-expired-lease") {
		t.Fatalf("driver did not report PASS ack-zero-expired-lease:\n%s", out)
	}
}

// TestOpenClawMessagingDenied130BatchedRelease (batch bound): a backlog
// of 130 allowlist-denied rows older than one pass exercises the server's
// 100-id ACK/release batch bound - the catch-up must deliver the eligible
// row behind the denied backlog, batch every request under the bound, and
// release all denied rows.
func TestOpenClawMessagingDenied130BatchedRelease(t *testing.T) {
	driver := fmt.Sprintf(`
  for (let i = 0; i < 130; i++) {
    putMessage("openclaw:s1", "denied-" + i, "noise " + i, "stranger-agent")
  }
  putMessage("openclaw:s1", "m-allow", "the one allowed message", "planner-agent")
  punkOpenClawPlugin.register(fakeApi)
  await fire("session_start", { sessionId: "s1", sessionKey: "chan:s1", reason: "new" }, ocCtx("s1"))
  await until(() => punkTestServer.registeredAgents.some((r) => r.agent === "openclaw:s1"), "session registered")

  const res = await fire("before_prompt_build", { prompt: "work", messages: [] }, ocCtx("s1"))
  must(res && res.prependContext === %s, "the allowed row behind 130 denied rows was prepended byte-exactly")

  must(punkTestServer.oversizeBatches === 0, "no ACK or release request ever exceeded the server's 100-id batch bound")
  for (const n of punkTestServer.ackBatches) must(n <= 100, "every ACK batch is within the bound, saw " + n)
  for (const n of punkTestServer.releaseBatches) must(n <= 100, "every release batch is within the bound, saw " + n)
  // The release is deliberately fire-and-forget in the blocking hook:
  // wait for both batches (100 + 30) to land.
  await until(() => {
    let releasedDenied = 0
    for (const r of punkTestServer.releaseCalls) {
      if (r.id.indexOf("denied-") === 0) releasedDenied++
    }
    return releasedDenied >= 130
  }, "every denied row was released in bounded batches")
  let deniedRows = 0
  for (const m of punkTestServer.inbox["openclaw:s1"] || []) {
    if (m.id.indexOf("denied-") === 0) deniedRows++
  }
  must(deniedRows === 130, "all 130 denied rows stay unread server-side, rows=" + deniedRows)
  must(!punkTestServer.ackedIds.some((a) => a.id.indexOf("denied-") === 0), "no denied row was ever acked")

  console.log("PASS denied-130-batched-release")
  process.exit(0)
`, jsStringLiteral(expectedOpenClawEnvelope("openclaw:s1", "m-allow", "planner-agent", "the one allowed message")))
	out := runOpenClawMessagingHarness(t, map[string]string{"PUNK_MESSAGING_FROM": "planner"}, driver)
	if !strings.Contains(out, "PASS denied-130-batched-release") {
		t.Fatalf("driver did not report PASS denied-130-batched-release:\n%s", out)
	}
}
