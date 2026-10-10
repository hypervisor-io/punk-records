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

// openCodeMessagingHarnessPrelude is the shared JS prelude appended (after
// the real rendered plugin source) to every messaging-bridge behavioral
// harness: an in-process fake punk-records HTTP surface implementing the
// messaging contract from docs/superpowers/plans/2026-09-25-agent-messaging.md
// (member registration with injectable outages, limit-respecting message
// reads, acks with injectable failures, and the addressed SSE inbox stream
// with injectable non-OK / stalled-connect / stalled-stream responses) plus
// a fake OpenCode SDK client whose session.prompt/list/status record every
// call. Scenarios drive the plugin's real hooks and assert on what the fake
// server/client observed.
//
// Notes on realism:
//   - The fake is wired in by replacing globalThis.fetch BEFORE the plugin
//     is initialized, so the plugin's own punkFetch/SSE calls hit it
//     exactly as they would hit a real server.
//   - SSE responses are real Response objects over real ReadableStreams
//     that emit "event: inbox" blocks and ": keepalive" comments, so the
//     plugin's stream parser, watchdogs, and body-cancel paths run for
//     real; stream cancel() and fetch aborts are recorded so tests can
//     assert on them.
//   - Failing acks/registrations return real non-OK Responses so
//     punkFetch's drain-and-return-null branch is exercised.
//   - PUNK_MESSAGING_BACKOFF_MS / _CONNECT_TIMEOUT_MS / _IDLE_TIMEOUT_MS
//     are passed by the Go harness (milliseconds) so retry, watchdog, and
//     reconnect scenarios run fast; production defaults are 500ms / 10s /
//     45s.
const openCodeMessagingHarnessPrelude = `
// ---- punk-records messaging test harness: fake server + fake SDK client ----
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))

const fetchCalls = []
const promptCalls = []
const punkTestServer = {
  ns: "agent-test-ns",
  registeredAgents: [],
  memberAttempts: 0,
  memberFailNext: 0, // N next registration attempts fail with 500
  inbox: {}, // agent -> array of unacked message objects with lease fields
  ackedIds: [],
  ackFailNext: 0,
  ackZeroNext: false, // one-shot: answer the next ACK {acked:0} (expired lease)
  messageFailAfter: -1, // let N leased reads succeed, then fail one (-1 = never)
  oversizeBatches: 0, // ACK/release calls above the server's 100-id bound (must stay 0)
  ackBatches: [], // ids length per ACK call
  releaseBatches: [], // ids length per release call
  messageFailNext: 0, // one-shot: answer the next leased read with 500
  releaseCalls: [], // {id, owner, mismatch}
  ackCalls: 0,
  sseFetches: 0,
  sseTimes: [], // Date.now() of every SSE fetch attempt
  sseAborted: [], // agents whose fetch got aborted (init.signal)
  sseBodiesCancelled: [], // agents whose non-OK body got cancelled
  sseCancelled: [], // agents whose open stream got reader.cancel()'d
  sseNonOK: 0, // N next SSE connects answer 500 (with a body)
  sseStallConnect: 0, // N next SSE connects never resolve until aborted
  sseStallStream: 0, // N next SSE streams connect but never send a byte
  sessionList: [],
  statusMap: {},
  statusFails: false, // session.status snapshot fails -> unknown busy
  listDelayMs: 0,
  promptMode: "ok", // default outcome once promptPlan is exhausted
  promptPlan: [], // per-attempt outcomes ("ok" | "throw" | "error"), shifted
  contextFetches: 0,
  emitHintFor: {},
  diagCalls: [], // parsed POST bodies to /messages/diagnostics, in order
  diag404: false, // one-shot mode: every diagnostics POST answers 404 (old server)
  diagStarted: 0, // diagnostics POSTs that reached the route handler
  diagOrder: [], // diagnostics POST bodies in COMPLETION order (delay-aware)
  diagDelayFirst: 0, // one-shot: delay the next diagnostics POST by this many ms
}

function jsonResponse(obj) {
  return new Response(JSON.stringify(obj), { status: 200, headers: { "Content-Type": "application/json" } })
}

function encodeChunk(s) {
  return new TextEncoder().encode(s)
}

function makeSSE(agent, opts) {
  let controller = null
  const stream = new ReadableStream({
    start(c) {
      controller = c
      if (!opts || !opts.stall) {
        controller.enqueue(
          encodeChunk("event: inbox\ndata: " + JSON.stringify({ agent: agent }) + "\n\n" + ": keepalive\n\n")
        )
      }
    },
    cancel() {
      punkTestServer.sseCancelled.push(agent)
    },
  })
  punkTestServer.emitHintFor[agent] = () => {
    try {
      controller.enqueue(encodeChunk("event: inbox\ndata: " + JSON.stringify({ agent: agent }) + "\n\n"))
    } catch (err) {}
  }
  return new Response(stream, { status: 200, headers: { "Content-Type": "text/event-stream" } })
}

function nonOKSSE(agent) {
  const stream = new ReadableStream({
    start(c) {
      c.enqueue(encodeChunk("simulated SSE failure body"))
    },
    cancel() {
      punkTestServer.sseBodiesCancelled.push(agent)
    },
  })
  return new Response(stream, { status: 500, headers: { "Content-Type": "text/plain" } })
}

globalThis.fetch = async function (url, init) {
  const u = new URL(String(url))
  const method = (init && init.method) || "GET"
  fetchCalls.push({ method: method, path: u.pathname, query: u.search, body: init && init.body ? String(init.body) : "" })

  const nsMatch = u.pathname.match(/^\/v1\/namespaces\/([^/]+)\/(.*)$/)
  if (nsMatch) {
    const rest = nsMatch[2]
    if (rest === "messages/events") {
      punkTestServer.sseFetches++
      punkTestServer.sseTimes.push(Date.now())
      const agent = u.searchParams.get("agent")
      if (init && init.signal) {
        init.signal.addEventListener("abort", () => punkTestServer.sseAborted.push(agent))
      }
      if (punkTestServer.sseStallConnect > 0) {
        punkTestServer.sseStallConnect--
        return new Promise((resolve, reject) => {
          if (init && init.signal) {
            init.signal.addEventListener("abort", () => reject(new Error("stalled connect aborted")))
          }
        })
      }
      if (punkTestServer.sseNonOK > 0) {
        punkTestServer.sseNonOK--
        return nonOKSSE(agent)
      }
      if (punkTestServer.sseStallStream > 0) {
        punkTestServer.sseStallStream--
        return makeSSE(agent, { stall: true })
      }
      return makeSSE(agent, {})
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
    if (rest === "messages/diagnostics") {
      // The exact shared diagnostics contract
      // (docs/superpowers/specs/2026-09-28-messaging-reliability-design.md):
      // POST body {agent, client, delivery_mode, state, ...optional}; the
      // real server requires an existing registered member, sets
      // updated_at, and answers {"status":"recorded"}. Old servers have no
      // route at all and answer 404 - delivery must survive that.
      // diagDelayFirst holds the FIRST post open so tests can race a
      // newer observation against a slow in-flight one; diagOrder records
      // COMPLETION order, which is what the server would end up storing.
      const body = JSON.parse(init.body)
      punkTestServer.diagStarted++
      let delay = 0
      if (punkTestServer.diagDelayFirst > 0) {
        delay = punkTestServer.diagDelayFirst
        punkTestServer.diagDelayFirst = 0
      }
      if (delay > 0) await sleep(delay)
      punkTestServer.diagCalls.push(body)
      punkTestServer.diagOrder.push(body)
      if (punkTestServer.diag404) {
        return new Response("no diagnostics route on this old server", { status: 404 })
      }
      const member = punkTestServer.registeredAgents.some((r) => r.agent === body.agent)
      if (!member) {
        return new Response("not a registered member", { status: 404 })
      }
      return jsonResponse({ status: "recorded" })
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
        punkTestServer.ackedIds.push(id)
        count++
      }
      return jsonResponse({ acked: count })
    }
    if (rest === "messages") {
      const agent = u.searchParams.get("agent")
      const limit = parseInt(u.searchParams.get("limit") || "100", 10)
      const leaseSeconds = parseInt(u.searchParams.get("lease_seconds") || "0", 10)
      const leasedBy = u.searchParams.get("leased_by") || ""
      const now = Date.now()
      if (punkTestServer.messageFailNext > 0) {
        punkTestServer.messageFailNext--
        return new Response("simulated read failure", { status: 500 })
      }
      if (punkTestServer.messageFailAfter >= 0) {
        if (punkTestServer.messageFailAfter === 0) {
          punkTestServer.messageFailAfter = -1
          return new Response("simulated read failure", { status: 500 })
        }
        punkTestServer.messageFailAfter--
      }
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
  }
  if (u.pathname === "/v1/agent/namespace") {
    return jsonResponse({ namespace: punkTestServer.ns })
  }
  if (u.pathname === "/v1/agent/hooks") {
    return jsonResponse({ ok: true })
  }
  if (u.pathname === "/v1/agent/context") {
    punkTestServer.contextFetches++
    return jsonResponse({ namespace: punkTestServer.ns, context: "" })
  }
  console.error("FAIL: unexpected fetch " + method + " " + u.pathname)
  process.exit(1)
}

const punkTestClient = {
  session: {
    prompt: async (opts) => {
      const sid = opts.path.id
      const firstPart = opts.body.parts && opts.body.parts[0]
      const text = (opts.body.parts || []).map((p) => p.text).join("\n")
      promptCalls.push({ sessionID: sid, text: text, synthetic: !!(firstPart && firstPart.synthetic) })
      let outcome = punkTestServer.promptMode
      if (punkTestServer.promptPlan.length > 0) {
        outcome = punkTestServer.promptPlan.shift()
      }
      if (outcome === "throw") throw new Error("simulated prompt failure")
      if (outcome === "error") return { data: null, error: { message: "simulated prompt error" } }
      return { data: { info: { id: "fake-assistant-msg" } }, error: null }
    },
    list: async () => {
      if (punkTestServer.listDelayMs > 0) await sleep(punkTestServer.listDelayMs)
      return { data: punkTestServer.sessionList }
    },
    status: async () => {
      if (punkTestServer.statusFails) return { data: null, error: { message: "simulated status failure" } }
      return { data: punkTestServer.statusMap }
    },
  },
}

function debugState() {
  return JSON.stringify({
    fetchCalls: fetchCalls.map((c) => c.method + " " + c.path),
    promptCalls: promptCalls.map((p) => ({ sessionID: p.sessionID, synthetic: p.synthetic })),
    registered: punkTestServer.registeredAgents,
    memberAttempts: punkTestServer.memberAttempts,
    ackedIds: punkTestServer.ackedIds,
    ackCalls: punkTestServer.ackCalls,
    sseFetches: punkTestServer.sseFetches,
    sseAborted: punkTestServer.sseAborted,
    sseBodiesCancelled: punkTestServer.sseBodiesCancelled,
    sseCancelled: punkTestServer.sseCancelled,
    diag: punkTestServer.diagCalls.map((d) => d.state),
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

function emitHint(agent) {
  if (punkTestServer.emitHintFor[agent]) punkTestServer.emitHintFor[agent]()
}

async function main() {
`

// openCodeMessagingHarnessEpilogue closes main() and exits; each scenario's
// driver body is spliced between prelude and epilogue by
// runOpenCodeMessagingHarness.
// expectedOpenCodeEnvelope renders the exact envelope the bridge must
// deliver for one message, through the same Go renderer punk hook inbox
// and the pi/OpenClaw bridges use.
func expectedOpenCodeEnvelope(address, id, sender, body string) string {
	return RenderInbox("agent-test-ns", address, []InboxMessage{{
		ID: id, Sender: sender, Recipient: address, Body: body, CreatedAt: "2026-09-25T00:00:00Z",
	}})
}

const openCodeMessagingHarnessEpilogue = `}
main().catch((err) => {
  console.error("FAIL: driver rejected: " + (err && err.stack ? err.stack : err))
  process.exit(1)
})
`

// runOpenCodeMessagingHarness renders the REAL plugin via ConnectOpenCode
// (never a paraphrase), appends the shared prelude + the scenario driver +
// the epilogue into one .mjs file, and executes it under node with the
// messaging bridge enabled. env adds or overrides environment variables
// (PUNK_MESSAGING and friends are set here, not inside the driver, so the
// plugin reads them exactly the way a real OpenCode process would).
//
// Skipped when node is not on PATH, mirroring the other node-driven tests
// in this package. The 25s hard deadline bounds a wedged driver (e.g. an
// SSE watchdog that never fires) instead of hanging ` + "`go test`" + `.
func runOpenCodeMessagingHarness(t *testing.T, env map[string]string, driver string) string {
	t.Helper()
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping opencode messaging behavioral test")
	}

	// PUNK_MESSAGING defaults to on for these harnesses but stays
	// overridable (the disabled-by-default scenario turns it off through
	// env, exactly as a real process would).
	//
	// XDG_STATE_HOME is ALWAYS forced into the harness temp dir: the
	// bridge persists its restart-recovery files under the real punk
	// state home when the variable leaks in from the developer
	// environment, which would both pollute real state and bleed
	// wake/pending state between test runs. env may still override it
	// (restart tests point two plugin lives at one shared temp home).
	defaults := map[string]string{
		"PUNK_URL":                     "http://punk.test",
		"PUNK_MESSAGING":               "1",
		"PUNK_MESSAGING_BACKOFF_MS":    "10",
		"PUNK_MESSAGING_LEASE_SECONDS": "1",
	}
	dir := t.TempDir()
	defaults["XDG_STATE_HOME"] = filepath.Join(dir, "xdg-state")
	for k, v := range env {
		defaults[k] = v
	}
	cmdEnv := pluginNodeEnv(dir, defaults)

	pluginPath := filepath.Join(dir, "punk-memory.js")
	if _, err := ConnectOpenCode(pluginPath, "http://punk.test"); err != nil {
		t.Fatal(err)
	}
	pluginSrc, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatal(err)
	}

	harness := string(pluginSrc) + openCodeMessagingHarnessPrelude + driver + openCodeMessagingHarnessEpilogue
	harnessPath := pluginPath + ".messaging-harness.mjs"
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
	// The driver exits non-zero with a FAIL line on any failed assertion;
	// surface its output verbatim so failures read like Go failures.
	if err != nil {
		t.Fatalf("node harness failed: %v\n%s", err, out)
	}
	return string(out)
}

// TestOpenCodeMessagingIdleWake: a message already queued for a freshly
// created session is delivered to the idle session via client.session.prompt
// (as a synthetic-flagged part) and ACKed only after the prompt succeeds;
// the namespace resolves through GET /v1/agent/namespace; the delivered
// frame carries the explicit namespace/sender/recipient addressing the
// model must use to reply; keepalive comments never duplicate delivery.
func TestOpenCodeMessagingIdleWake(t *testing.T) {
	driver := fmt.Sprintf(`
  putMessage("opencode:s1", "m1", "please review the flaky test")
  const expectedEnvelope = %s
  const hooks = await PunkMemoryPlugin({ directory: "/tmp/punk-messaging-proj", client: punkTestClient })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })

  await until(() => promptCalls.length === 1, "message delivered to idle session")
  await until(() => punkTestServer.ackedIds.indexOf("m1") >= 0, "message m1 acked after successful prompt")

  must(promptCalls[0].sessionID === "s1", "prompt went to session s1, got " + promptCalls[0].sessionID)
  must(promptCalls[0].synthetic === true, "bridge prompt part is synthetic so it is never captured as a human prompt")
  // The delivered text is the shared M5 envelope, byte-identical to what
  // punk hook inbox prints (RenderInbox) and to the pi/OpenClaw bridges.
  must(promptCalls[0].text === expectedEnvelope, "prompt text is the byte-exact shared M5 envelope")
  must(promptCalls[0].text.indexOf("[PUNK INBOX] 1 message(s) for opencode:s1 in agent-test-ns.") === 0, "the synthetic part begins with the inbox marker")
  must(
    promptCalls[0].text.indexOf('send_message(namespace="agent-test-ns", sender="opencode:s1", recipient="planner-agent", reply_to="m1"') >= 0,
    "reply guidance demands EXPLICIT namespace and sender (MCP identity is the host, default workspace ns differs)"
  )
  must(
    promptCalls[0].text.indexOf("do not ack them yourself") >= 0,
    "prompt tells the model the bridge already acknowledged delivery"
  )

  must(
    punkTestServer.registeredAgents.some((r) => r.agent === "opencode:s1"),
    "session registered under the opencode:<sessionID> identity"
  )
  const memberCall = fetchCalls.find((c) => c.path === "/v1/namespaces/agent-test-ns/members")
  must(memberCall && memberCall.method === "POST", "member registration is a POST to the namespace members endpoint")
  must(memberCall.body.indexOf("opencode:s1") >= 0, "registration body carries the agent address")

  must(
    fetchCalls.some(
      (c) => c.path === "/v1/agent/namespace" && c.query.indexOf(encodeURIComponent("/tmp/punk-messaging-proj")) >= 0
    ),
    "namespace resolved through /v1/agent/namespace with the plugin directory as cwd"
  )

  const ackCall = fetchCalls.find((c) => c.path === "/v1/namespaces/agent-test-ns/messages/ack")
  must(ackCall && ackCall.body.indexOf("m1") >= 0, "ack body carries the delivered message id")

  // The SSE stream stays open and sends ": keepalive" comments; none of
  // that may produce a second prompt for the already-acked message.
  await sleep(200)
  must(promptCalls.length === 1, "keepalives and open stream produce no duplicate delivery, prompts=" + promptCalls.length)

  console.log("PASS idle-wake")
  process.exit(0)
`, jsStringLiteral(expectedOpenCodeEnvelope("opencode:s1", "m1", "planner-agent", "please review the flaky test")))
	out := runOpenCodeMessagingHarness(t, nil, driver)
	if !strings.Contains(out, "PASS idle-wake") {
		t.Fatalf("driver did not report PASS idle-wake:\n%s", out)
	}
}

// TestOpenCodeMessagingBusyDefer: a user turn (chat.message) marks the
// session busy; an inbox hint arriving while busy triggers NO prompt and
// NO ack (messages stay queued server-side); the next session.idle
// transition delivers and acks.
func TestOpenCodeMessagingBusyDefer(t *testing.T) {
	driver := `
  const hooks = await PunkMemoryPlugin({ directory: "/tmp/punk-messaging-proj", client: punkTestClient })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s2" } } } })
  await until(
    () => punkTestServer.registeredAgents.some((r) => r.agent === "opencode:s2"),
    "session s2 registered"
  )

  await hooks["chat.message"](
    { sessionID: "s2", messageID: "u1" },
    { message: { role: "user" }, parts: [{ type: "text", text: "user is busy working" }] }
  )
  putMessage("opencode:s2", "m-busy", "urgent question while you work")
  emitHint("opencode:s2")

  await sleep(250)
  must(promptCalls.length === 0, "busy session must not be prompted, prompts=" + promptCalls.length)
  must(punkTestServer.ackedIds.length === 0, "busy session must not ack, acked=" + JSON.stringify(punkTestServer.ackedIds))

  await hooks.event({ event: { type: "session.idle", properties: { sessionID: "s2" } } })
  await until(() => promptCalls.length === 1, "deferred message delivered after idle")
  await until(() => punkTestServer.ackedIds.indexOf("m-busy") >= 0, "deferred message acked after delivery")
  must(promptCalls[0].text.indexOf("urgent question while you work") >= 0, "delivered the queued message body")

  console.log("PASS busy-defer")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, nil, driver)
	if !strings.Contains(out, "PASS busy-defer") {
		t.Fatalf("driver did not report PASS busy-defer:\n%s", out)
	}
}

// TestOpenCodeMessagingMultipleSessions: two live sessions bind to distinct
// opencode:<sessionID> addresses with separate SSE streams, and a message
// for one inbox is delivered only to that session.
func TestOpenCodeMessagingMultipleSessions(t *testing.T) {
	driver := `
  const hooks = await PunkMemoryPlugin({ directory: "/tmp/punk-messaging-proj", client: punkTestClient })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s2" } } } })
  await until(
    () => punkTestServer.registeredAgents.some((r) => r.agent === "opencode:s1") &&
         punkTestServer.registeredAgents.some((r) => r.agent === "opencode:s2"),
    "both sessions registered under distinct addresses"
  )

  putMessage("opencode:s1", "m1", "message for session one")
  emitHint("opencode:s1")
  await until(() => promptCalls.length === 1, "message for s1 delivered")
  await until(() => punkTestServer.ackedIds.indexOf("m1") >= 0, "m1 acked")
  must(promptCalls[0].sessionID === "s1", "only session s1 was prompted for its own inbox")

  await sleep(150)
  must(promptCalls.length === 1, "session s2 was not prompted for s1's message")

  putMessage("opencode:s2", "m2", "message for session two")
  emitHint("opencode:s2")
  await until(() => promptCalls.length === 2, "message for s2 delivered")
  await until(() => punkTestServer.ackedIds.indexOf("m2") >= 0, "m2 acked")
  must(promptCalls[1].sessionID === "s2", "second prompt went to session s2")

  console.log("PASS multiple-sessions")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, nil, driver)
	if !strings.Contains(out, "PASS multiple-sessions") {
		t.Fatalf("driver did not report PASS multiple-sessions:\n%s", out)
	}
}

// TestOpenCodeMessagingPartialAckFlush (orchestrator blocker 5): two
// messages, first prompt succeeds and second fails - the first message's
// ACK must still be flushed (no starvation of delivered ids behind a
// failing sibling), and after recovery the second delivers while the first
// is never re-prompted.
func TestOpenCodeMessagingPartialAckFlush(t *testing.T) {
	driver := `
  putMessage("opencode:s1", "m1", "first message will deliver")
  putMessage("opencode:s1", "m2", "second message will fail once")
  punkTestServer.promptPlan = ["ok", "throw"]
  const hooks = await PunkMemoryPlugin({ directory: "/tmp/punk-messaging-proj", client: punkTestClient })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })

  await until(() => promptCalls.length >= 2, "both prompts attempted (ok then throw)")
  await until(() => punkTestServer.ackedIds.indexOf("m1") >= 0, "m1 acked despite m2 failing after it")
  const firstAck = fetchCalls.find((c) => c.path === "/v1/namespaces/agent-test-ns/messages/ack")
  must(
    firstAck && firstAck.body.indexOf("m1") >= 0 && firstAck.body.indexOf("m2") < 0,
    "the flushed ACK covers exactly the successful subset"
  )

  // The queued drain (or a manual hint) recovers m2; m1 must never be
  // re-prompted while its ACK retry was pending.
  emitHint("opencode:s1")
  await until(() => punkTestServer.ackedIds.indexOf("m2") >= 0, "m2 delivered and acked after recovery")
  must(
    promptCalls.filter((p) => p.text.indexOf("first message will deliver") >= 0).length === 1,
    "m1 must never be re-prompted while its ACK retry was pending"
  )
  must(
    promptCalls.length === 3,
    "exactly three prompts total (m1 ok, m2 throw, m2 retry), prompts=" + promptCalls.length
  )

  console.log("PASS partial-ack-flush")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, nil, driver)
	if !strings.Contains(out, "PASS partial-ack-flush") {
		t.Fatalf("driver did not report PASS partial-ack-flush:\n%s", out)
	}
}

// TestOpenCodeMessagingPromptFailureNoAck: a prompt that throws (or
// resolves with an SDK error result) is a failed delivery - no ACK for that
// message, nothing acked on the session's behalf, no prompt churn while the
// failure mode persists, and a clean delivery once prompts work again.
func TestOpenCodeMessagingPromptFailureNoAck(t *testing.T) {
	driver := `
  const hooks = await PunkMemoryPlugin({ directory: "/tmp/punk-messaging-proj", client: punkTestClient })

  punkTestServer.promptMode = "throw"
  putMessage("opencode:s1", "m1", "first attempt will throw")
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })
  await until(() => promptCalls.length >= 1, "failed prompt was still attempted")
  await sleep(250)
  must(punkTestServer.ackCalls === 0, "no ack after a thrown prompt, ackCalls=" + punkTestServer.ackCalls)
  must(punkTestServer.ackedIds.length === 0, "nothing acked, acked=" + JSON.stringify(punkTestServer.ackedIds))
  must(
    promptCalls.length <= 2,
    "no prompt churn: at most the initial attempt plus one queued-drain retry, prompts=" + promptCalls.length
  )
  const attemptsAfterThrow = promptCalls.length

  punkTestServer.promptMode = "ok"
  emitHint("opencode:s1")
  await until(() => punkTestServer.ackedIds.indexOf("m1") >= 0, "m1 delivered and acked after recovery")
  must(
    promptCalls.length === attemptsAfterThrow + 1,
    "one fresh prompt delivered it, prompts=" + promptCalls.length + " (was " + attemptsAfterThrow + ")"
  )

  punkTestServer.promptMode = "error"
  putMessage("opencode:s1", "m2", "second attempt resolves with an error")
  emitHint("opencode:s1")
  await until(() => promptCalls.length >= attemptsAfterThrow + 2, "error-result prompt was attempted")
  await sleep(250)
  must(punkTestServer.ackedIds.indexOf("m2") < 0, "no ack for an error-result prompt")
  must(
    promptCalls.length <= attemptsAfterThrow + 3,
    "no churn in error mode: at most one attempt plus one queued retry, prompts=" + promptCalls.length
  )
  const attemptsAfterError = promptCalls.length

  punkTestServer.promptMode = "ok"
  emitHint("opencode:s1")
  await until(() => punkTestServer.ackedIds.indexOf("m2") >= 0, "m2 delivered and acked after recovery")
  must(
    promptCalls.length === attemptsAfterError + 1,
    "one fresh prompt delivered m2, prompts=" + promptCalls.length + " (was " + attemptsAfterError + ")"
  )

  console.log("PASS prompt-failure-no-ack")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, nil, driver)
	if !strings.Contains(out, "PASS prompt-failure-no-ack") {
		t.Fatalf("driver did not report PASS prompt-failure-no-ack:\n%s", out)
	}
}

// TestOpenCodeMessagingAckRetryNoDuplicatePrompt: the prompt succeeds but
// the ACK call fails; the retry path (next inbox hint) must re-ACK the
// message WITHOUT prompting the model a second time for the same id.
func TestOpenCodeMessagingAckRetryNoDuplicatePrompt(t *testing.T) {
	driver := `
  punkTestServer.ackFailNext = 1
  putMessage("opencode:s1", "m1", "prompt ok, ack will fail once")
  const hooks = await PunkMemoryPlugin({ directory: "/tmp/punk-messaging-proj", client: punkTestClient })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })

  await until(() => promptCalls.length === 1, "message prompted once")
  await until(() => punkTestServer.ackCalls >= 1, "first ack attempted")
  // The failed ACK is retried by the queued drain or a hint - whichever
  // comes first. The property under test: the retry re-ACKs WITHOUT ever
  // re-prompting the model for the same id.
  emitHint("opencode:s1")
  await until(() => punkTestServer.ackedIds.indexOf("m1") >= 0, "ack retried and succeeded")
  must(promptCalls.length === 1, "ack retry must not re-prompt the model, prompts=" + promptCalls.length)
  must(punkTestServer.ackCalls >= 2, "the ack was actually retried, ackCalls=" + punkTestServer.ackCalls)

  console.log("PASS ack-retry-no-duplicate-prompt")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, nil, driver)
	if !strings.Contains(out, "PASS ack-retry-no-duplicate-prompt") {
		t.Fatalf("driver did not report PASS ack-retry-no-duplicate-prompt:\n%s", out)
	}
}

// TestOpenCodeMessagingBacklogDrainQueue (orchestrator blocker 5): a
// backlog larger than one read batch (>100 messages) drains completely
// across multiple GET rounds, every message is prompted exactly once, and
// hundreds of hints raining down mid-delivery are folded into queued
// drains instead of being lost - with no duplicate delivery.
func TestOpenCodeMessagingBacklogDrainQueue(t *testing.T) {
	driver := `
  const hooks = await PunkMemoryPlugin({ directory: "/tmp/punk-messaging-proj", client: punkTestClient })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })
  await until(() => punkTestServer.sseFetches >= 1, "SSE stream connected")

  for (let i = 0; i < 150; i++) {
    putMessage("opencode:s1", "bk-" + i, "backlog item " + i)
  }
  // Spam far more hints than there are messages: they arrive while drains
  // are mid-flight and must collapse into queued drains, not vanish.
  for (let i = 0; i < 300; i++) {
    emitHint("opencode:s1")
  }

  await until(() => punkTestServer.ackedIds.length === 150, "all 150 backlog messages acked", 8000)
  must(promptCalls.length === 150, "every message prompted exactly once, prompts=" + promptCalls.length)
  must(new Set(promptCalls.map((p) => p.text)).size === 150, "no message was prompted twice")
  const messageGETs = fetchCalls.filter((c) => c.path === "/v1/namespaces/agent-test-ns/messages" && c.method === "GET")
  must(messageGETs.length >= 2, "backlog beyond one batch required multiple read rounds, rounds=" + messageGETs.length)

  await sleep(150)
  must(promptCalls.length === 150, "no post-drain duplicate delivery, prompts=" + promptCalls.length)

  console.log("PASS backlog-drain-queue")
  process.exit(0)
`
	// The wake cap (default 5) would legitimately stop this drain early;
	// cap behavior has its own dedicated test below.
	out := runOpenCodeMessagingHarness(t, map[string]string{"PUNK_MESSAGING_MAX_CONTINUE": "200"}, driver)
	if !strings.Contains(out, "PASS backlog-drain-queue") {
		t.Fatalf("driver did not report PASS backlog-drain-queue:\n%s", out)
	}
}

// TestOpenCodeMessagingSSEReconnectAndDeletion: a dropped SSE stream (server
// closes after the initial hint) is reconnected; session.deleted unbinds
// the session, aborts its stream, allows no further reconnects, delivers
// nothing for late messages - and a duplicate session.created for the
// deleted id must NOT rebind it.
func TestOpenCodeMessagingSSEReconnectAndDeletion(t *testing.T) {
	driver := `
  punkTestServer.sseStallStream = 1 // first stream: connect, zero bytes, then watchdog reconnect
  const hooks = await PunkMemoryPlugin({ directory: "/tmp/punk-messaging-proj", client: punkTestClient })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })

  await until(() => punkTestServer.sseFetches >= 2, "SSE reconnected after the first stream stalled")
  must(
    punkTestServer.sseCancelled.some((a) => a === "opencode:s1"),
    "the stalled stream's reader was cancelled"
  )

  const memberAttemptsBefore = punkTestServer.memberAttempts
  const abortedBeforeDelete = punkTestServer.sseAborted.length
  await hooks.event({ event: { type: "session.deleted", properties: { info: { id: "s1" } } } })
  await until(
    () => punkTestServer.sseAborted.length > abortedBeforeDelete,
    "the live stream's fetch was aborted when the session is deleted"
  )

  const sseCountAfterDelete = punkTestServer.sseFetches
  await sleep(400) // far past the 10ms backoff schedule: no reconnect may occur
  must(
    punkTestServer.sseFetches === sseCountAfterDelete,
    "deleted session must not reconnect, fetches=" + punkTestServer.sseFetches + " (was " + sseCountAfterDelete + ")"
  )

  // A late hint addressed to the deleted session's inbox must not deliver:
  // the session is unbound, so there is nowhere to prompt.
  putMessage("opencode:s1", "m-late", "too late, session is gone")
  await sleep(150)
  must(promptCalls.length === 0, "no delivery for a deleted session, prompts=" + promptCalls.length)

  // No rebind after deletion: a duplicate session.created for the same id
  // must not register, listen, or deliver anything.
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })
  await sleep(200)
  must(
    punkTestServer.memberAttempts === memberAttemptsBefore,
    "deleted session must never re-register, attempts=" + punkTestServer.memberAttempts
  )
  must(punkTestServer.sseFetches === sseCountAfterDelete, "deleted session must never re-listen")

  console.log("PASS sse-reconnect-and-deletion")
  process.exit(0)
`
	// A short idle watchdog makes the zero-byte stream trip quickly.
	out := runOpenCodeMessagingHarness(t, map[string]string{"PUNK_MESSAGING_IDLE_TIMEOUT_MS": "80"}, driver)
	if !strings.Contains(out, "PASS sse-reconnect-and-deletion") {
		t.Fatalf("driver did not report PASS sse-reconnect-and-deletion:\n%s", out)
	}
}

// TestOpenCodeMessagingNonOKSSEBackoff (orchestrator blocker 4): non-OK SSE
// responses (with a body) must have the body cancelled and must retry on
// the same exponential capped backoff as dropped connections - the backoff
// must NOT reset between non-OK attempts - and the stream must work once
// the server recovers.
func TestOpenCodeMessagingNonOKSSEBackoff(t *testing.T) {
	driver := `
  const timerDelays = []
  const realSetTimeout = globalThis.setTimeout
  globalThis.setTimeout = (fn, ms, ...rest) => {
    timerDelays.push(ms)
    return realSetTimeout(fn, ms, ...rest)
  }
  punkTestServer.sseNonOK = 3
  putMessage("opencode:s1", "m1", "delivered once SSE recovers")
  const hooks = await PunkMemoryPlugin({ directory: "/tmp/punk-messaging-proj", client: punkTestClient })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })

  await until(() => punkTestServer.sseFetches >= 4, "three non-OK attempts then a good connect")
  await until(() => punkTestServer.ackedIds.indexOf("m1") >= 0, "message delivered once the stream connects")

  must(
    punkTestServer.sseBodiesCancelled.filter((a) => a === "opencode:s1").length === 3,
    "every non-OK SSE body was cancelled, cancelled=" + JSON.stringify(punkTestServer.sseBodiesCancelled)
  )
  // Wall-clock gaps between attempts are unreliable on a loaded host (a
  // stalled event loop shifts when the mock records an attempt), so the
  // backoff is asserted on the delays the bridge actually requested from
  // setTimeout: the recorded sub-second delays must escalate 40, 80, 160
  // in order. A backoff reset would request 40 again instead.
  const requested = timerDelays.filter((ms) => typeof ms === "number" && ms < 1000)
  const escalation = [40, 80, 160]
  let at = 0
  for (let i = 0; i < requested.length && at < escalation.length; i++) {
    if (requested[i] === escalation[at]) at++
  }
  must(at === escalation.length, "backoff requested 40/80/160 in order, requested=" + JSON.stringify(requested))
  must(requested.filter((ms) => ms === 40).length === 1, "the base backoff was requested exactly once (no reset), requested=" + JSON.stringify(requested))

  console.log("PASS non-ok-sse-backoff")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, map[string]string{"PUNK_MESSAGING_BACKOFF_MS": "40"}, driver)
	if !strings.Contains(out, "PASS non-ok-sse-backoff") {
		t.Fatalf("driver did not report PASS non-ok-sse-backoff:\n%s", out)
	}
}

// TestOpenCodeMessagingStalledConnectWatchdog (orchestrator blocker 4): an
// SSE connect that never resolves (silent server) must be aborted by the
// bounded connect watchdog and retried; the bridge keeps working afterwards.
func TestOpenCodeMessagingStalledConnectWatchdog(t *testing.T) {
	driver := `
  punkTestServer.sseStallConnect = 1
  putMessage("opencode:s1", "m1", "delivered despite a stalled connect")
  const hooks = await PunkMemoryPlugin({ directory: "/tmp/punk-messaging-proj", client: punkTestClient })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })

  await until(() => punkTestServer.sseFetches >= 2, "stalled connect aborted by the watchdog and retried", 6000)
  await until(() => punkTestServer.ackedIds.indexOf("m1") >= 0, "message still delivered")
  must(
    punkTestServer.sseAborted.some((a) => a === "opencode:s1"),
    "the watchdog aborted the stalled fetch"
  )

  console.log("PASS stalled-connect-watchdog")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, map[string]string{"PUNK_MESSAGING_CONNECT_TIMEOUT_MS": "60"}, driver)
	if !strings.Contains(out, "PASS stalled-connect-watchdog") {
		t.Fatalf("driver did not report PASS stalled-connect-watchdog:\n%s", out)
	}
}

// TestOpenCodeMessagingRestoredBusyRetryIdle (orchestrator blocker 3):
// restored sessions bind from the session.list + session.status snapshot -
// busy AND provider-retry statuses count as busy (defer), idle delivers
// immediately, and a busy one delivers only after an authoritative idle.
func TestOpenCodeMessagingRestoredBusyRetryIdle(t *testing.T) {
	driver := `
  punkTestServer.sessionList = [
    { id: "busy-1", directory: "/tmp/punk-messaging-proj" },
    { id: "retry-1", directory: "/tmp/punk-messaging-proj" },
    { id: "idle-1", directory: "/tmp/punk-messaging-proj" },
  ]
  punkTestServer.statusMap = {
    "busy-1": { type: "busy" },
    "retry-1": { type: "retry" },
    "idle-1": { type: "idle" },
  }
  putMessage("opencode:busy-1", "mb", "message for busy restored session")
  putMessage("opencode:retry-1", "mr", "message for retrying restored session")
  putMessage("opencode:idle-1", "mi", "message for idle restored session")

  const hooks = await PunkMemoryPlugin({ directory: "/tmp/punk-messaging-proj", client: punkTestClient })
  await until(() => promptCalls.length === 1, "only the idle restored session is prompted")
  must(promptCalls[0].sessionID === "idle-1", "the prompt went to the idle session, got " + promptCalls[0].sessionID)
  await until(() => punkTestServer.ackedIds.indexOf("mi") >= 0, "idle restored session's message acked")

  emitHint("opencode:busy-1")
  emitHint("opencode:retry-1")
  await sleep(250)
  must(promptCalls.length === 1, "busy and retrying restored sessions stay deferred on hints")

  await hooks.event({ event: { type: "session.idle", properties: { sessionID: "busy-1" } } })
  await until(() => promptCalls.length === 2, "busy restored session delivered after authoritative idle")
  must(promptCalls[1].sessionID === "busy-1", "second prompt went to busy-1")
  must(promptCalls.length === 2, "retry-1 still deferred (no authoritative idle for it yet)")

  console.log("PASS restored-busy-retry-idle")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, nil, driver)
	if !strings.Contains(out, "PASS restored-busy-retry-idle") {
		t.Fatalf("driver did not report PASS restored-busy-retry-idle:\n%s", out)
	}
}

// TestOpenCodeMessagingRestoredUnknownDefers (orchestrator blocker 3): a
// FAILED session.status snapshot must not blindly mark restored sessions
// idle - they bind with UNKNOWN busy state, defer on hints, and deliver
// only after an authoritative idle transition.
func TestOpenCodeMessagingRestoredUnknownDefers(t *testing.T) {
	driver := `
  punkTestServer.statusFails = true
  punkTestServer.sessionList = [{ id: "unk-1", directory: "/tmp/punk-messaging-proj" }]
  putMessage("opencode:unk-1", "mu", "message for unknown-busy restored session")

  const hooks = await PunkMemoryPlugin({ directory: "/tmp/punk-messaging-proj", client: punkTestClient })
  await until(
    () => punkTestServer.registeredAgents.some((r) => r.agent === "opencode:unk-1"),
    "restored session registered despite failed status snapshot"
  )
  await until(() => punkTestServer.sseFetches >= 1, "SSE connected (its initial hint must still defer)")

  await sleep(250)
  must(promptCalls.length === 0, "unknown busy state must defer delivery, prompts=" + promptCalls.length)
  must(punkTestServer.ackedIds.length === 0, "nothing acked while deferred")

  await hooks.event({ event: { type: "session.status", properties: { sessionID: "unk-1", status: { type: "idle" } } } })
  await until(() => promptCalls.length === 1, "delivered after the authoritative idle status")
  await until(() => punkTestServer.ackedIds.indexOf("mu") >= 0, "acked after delivery")

  console.log("PASS restored-unknown-defers")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, nil, driver)
	if !strings.Contains(out, "PASS restored-unknown-defers") {
		t.Fatalf("driver did not report PASS restored-unknown-defers:\n%s", out)
	}
}

// TestOpenCodeMessagingRegistrationOutageRecovery (orchestrator blocker 2):
// a registration outage is retried autonomously on bounded exponential
// backoff until the server confirms; no SSE stream and no prompt before the
// confirmation; and a human chat.message turn DURING the outage marks the
// session busy so the post-recovery hint still defers until idle.
func TestOpenCodeMessagingRegistrationOutageRecovery(t *testing.T) {
	driver := `
  punkTestServer.memberFailNext = 3
  putMessage("opencode:s1", "m1", "delivered once registration recovers")
  const hooks = await PunkMemoryPlugin({ directory: "/tmp/punk-messaging-proj", client: punkTestClient })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })

  await until(() => punkTestServer.memberAttempts >= 3, "three failing registration attempts observed")
  must(promptCalls.length === 0, "no prompt before a confirmed registration")
  must(punkTestServer.sseFetches === 0, "no SSE stream before a confirmed registration")

  // A human turn while binding: must mark busy and survive into the
  // post-registration state.
  await hooks["chat.message"](
    { sessionID: "s1", messageID: "u1" },
    { message: { role: "user" }, parts: [{ type: "text", text: "user got busy during the outage" }] }
  )

  await until(
    () => punkTestServer.registeredAgents.some((r) => r.agent === "opencode:s1"),
    "registration retried until the server confirmed it"
  )
  must(punkTestServer.memberAttempts === 4, "exactly one successful attempt after three failures, attempts=" + punkTestServer.memberAttempts)
  await until(() => punkTestServer.sseFetches >= 1, "SSE starts only after confirmed registration")

  await sleep(250)
  must(promptCalls.length === 0, "busy-while-binding defers delivery even after registration recovers")

  await hooks.event({ event: { type: "session.idle", properties: { sessionID: "s1" } } })
  await until(() => promptCalls.length === 1, "message delivered once idle")
  await until(() => punkTestServer.ackedIds.indexOf("m1") >= 0, "message acked after delivery")

  console.log("PASS registration-outage-recovery")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, nil, driver)
	if !strings.Contains(out, "PASS registration-outage-recovery") {
		t.Fatalf("driver did not report PASS registration-outage-recovery:\n%s", out)
	}
}

// TestOpenCodeMessagingDisposeCancelsPendingWork (orchestrator blocker 2):
// dispose while registration retries are pending stops the retry loop (no
// further attempts, no SSE, no prompts), a session.created after dispose
// binds nothing, and a dispose racing the async restored-session bind
// (list still in flight) prevents the late bind from registering at all.
func TestOpenCodeMessagingDisposeCancelsPendingWork(t *testing.T) {
	pendingRegistrationDriver := `
  punkTestServer.memberFailNext = 100000 // registration never succeeds
  const hooks = await PunkMemoryPlugin({ directory: "/tmp/punk-messaging-proj", client: punkTestClient })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })
  await until(() => punkTestServer.memberAttempts >= 1, "registration retry loop started")

  await hooks.dispose()
  const attemptsAtDispose = punkTestServer.memberAttempts
  await sleep(300) // far past the 10ms backoff schedule
  must(
    punkTestServer.memberAttempts === attemptsAtDispose,
    "dispose must cancel pending registration retries, attempts=" + punkTestServer.memberAttempts + " (was " + attemptsAtDispose + ")"
  )
  must(punkTestServer.sseFetches === 0, "no SSE after dispose")
  must(promptCalls.length === 0, "no prompts after dispose")

  // A late session.created after dispose must bind nothing.
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s2" } } } })
  await sleep(150)
  must(
    punkTestServer.memberAttempts === attemptsAtDispose,
    "session.created after dispose must not register, attempts=" + punkTestServer.memberAttempts
  )
  console.log("PASS dispose-cancels-pending-registration")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, nil, pendingRegistrationDriver)
	if !strings.Contains(out, "PASS dispose-cancels-pending-registration") {
		t.Fatalf("driver did not report PASS dispose-cancels-pending-registration:\n%s", out)
	}

	restoredBindRaceDriver := `
  punkTestServer.listDelayMs = 150 // restored-session list is still in flight when dispose fires
  const hooks = await PunkMemoryPlugin({ directory: "/tmp/punk-messaging-proj", client: punkTestClient })
  await hooks.dispose()
  await sleep(400) // the delayed list resolves well after dispose
  must(
    punkTestServer.memberAttempts === 0,
    "a restored-session bind completing after dispose must register nothing, attempts=" + punkTestServer.memberAttempts
  )
  must(punkTestServer.sseFetches === 0, "no SSE from a late restored bind")
  must(promptCalls.length === 0, "no prompts from a late restored bind")
  console.log("PASS dispose-cancels-late-restored-bind")
  process.exit(0)
`
	// The restored-bind race needs a session that would bind if not disposed.
	restoredBindRaceDriver = `
  punkTestServer.listDelayMs = 150
  punkTestServer.sessionList = [{ id: "late-1", directory: "/tmp/punk-messaging-proj" }]
` + restoredBindRaceDriver
	out = runOpenCodeMessagingHarness(t, nil, restoredBindRaceDriver)
	if !strings.Contains(out, "PASS dispose-cancels-late-restored-bind") {
		t.Fatalf("driver did not report PASS dispose-cancels-late-restored-bind:\n%s", out)
	}
}

// TestOpenCodeMessagingNamespaceOverride: with PUNK_NAMESPACE set (in the
// process env, exactly as a coordination namespace would be), the bridge
// never calls /v1/agent/namespace and every messaging call targets the
// override namespace; the delivered frame and injected guidance both name
// the override namespace and the session's own address as sender.
func TestOpenCodeMessagingNamespaceOverride(t *testing.T) {
	driver := `
  putMessage("opencode:s1", "m1", "coordination namespace message")
  const hooks = await PunkMemoryPlugin({ directory: "/tmp/punk-messaging-proj", client: punkTestClient })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })

  await until(() => punkTestServer.ackedIds.indexOf("m1") >= 0, "message delivered and acked in the override namespace")
  must(
    !fetchCalls.some((c) => c.path === "/v1/agent/namespace"),
    "PUNK_NAMESPACE override must bypass cwd-derived namespace resolution"
  )
  must(
    fetchCalls.some((c) => c.path === "/v1/namespaces/punk-coordination/members"),
    "registration targeted the PUNK_NAMESPACE override"
  )
  must(
    fetchCalls.some((c) => c.path === "/v1/namespaces/punk-coordination/messages/ack"),
    "ack targeted the PUNK_NAMESPACE override"
  )
  must(promptCalls.length === 1, "exactly one delivery, prompts=" + promptCalls.length)
  must(
    promptCalls[0].text.indexOf('send_message(namespace="punk-coordination", sender="opencode:s1", recipient="planner-agent", reply_to="m1"') >= 0,
    "reply guidance names the override namespace and the session's own address as sender"
  )

  // Identity injection carries the override namespace too.
  const out = { system: [] }
  await hooks["experimental.chat.system.transform"]({ sessionID: "s1" }, out)
  must(
    out.system.some((b) => b.indexOf("punk-coordination") >= 0 && b.indexOf("opencode:s1") >= 0),
    "messaging block names the override namespace and the session address"
  )

  console.log("PASS namespace-override")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, map[string]string{"PUNK_NAMESPACE": "punk-coordination"}, driver)
	if !strings.Contains(out, "PASS namespace-override") {
		t.Fatalf("driver did not report PASS namespace-override:\n%s", out)
	}
}

// TestOpenCodeMessagingDisabledByDefault: without PUNK_MESSAGING=1 the
// bridge is completely inert - no member registration, no inbox reads, no
// SSE, no prompts, no messaging block in the system prompt (on ANY turn) -
// while the memory hooks keep firing exactly as before (fail-open around
// the opt-in feature).
func TestOpenCodeMessagingDisabledByDefault(t *testing.T) {
	driver := `
  putMessage("opencode:s1", "m1", "nobody is listening")
  const hooks = await PunkMemoryPlugin({ directory: "/tmp/punk-messaging-proj", client: punkTestClient })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })
  await hooks["chat.message"](
    { sessionID: "s1", messageID: "u1" },
    { message: { role: "user" }, parts: [{ type: "text", text: "plain memory capture" }] }
  )
  await hooks.event({ event: { type: "session.idle", properties: { sessionID: "s1" } } })
  await hooks.event({ event: { type: "session.deleted", properties: { info: { id: "s1" } } } })

  await sleep(200)
  must(promptCalls.length === 0, "no prompts when messaging is disabled")
  must(punkTestServer.sseFetches === 0, "no SSE connections when messaging is disabled")
  must(
    fetchCalls.every((c) => c.path === "/v1/agent/hooks"),
    "only memory-hook calls happen when messaging is disabled, got " + JSON.stringify(fetchCalls.map((c) => c.path))
  )

  const out = { system: [] }
  await hooks["experimental.chat.system.transform"]({ sessionID: "s1" }, out)
  must(out.system.length === 0, "no messaging block injected when disabled, system=" + JSON.stringify(out.system))

  console.log("PASS disabled-by-default")
  process.exit(0)
`
	// PUNK_MESSAGING explicitly off (the default state of a real install).
	out := runOpenCodeMessagingHarness(t, map[string]string{"PUNK_MESSAGING": "0"}, driver)
	if !strings.Contains(out, "PASS disabled-by-default") {
		t.Fatalf("driver did not report PASS disabled-by-default:\n%s", out)
	}
}

// TestOpenCodeMessagingFailOpen: a totally dead punk server (fetch rejects
// everything) must never break the hooks - every hook still resolves, and
// the bridge quietly retries in the background. Also covers the no-SDK-
// client case: without a client there is nothing to deliver through, so
// the bridge stays inert rather than registering sessions it can never
// wake.
func TestOpenCodeMessagingFailOpen(t *testing.T) {
	deadServerDriver := `
  globalThis.fetch = () => Promise.reject(new Error("network down"))
  const hooks = await PunkMemoryPlugin({ directory: "/tmp/punk-messaging-proj", client: punkTestClient })

  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })
  await hooks["chat.message"](
    { sessionID: "s1", messageID: "u1" },
    { message: { role: "user" }, parts: [{ type: "text", text: "capture me anyway" }] }
  )
  const out = { system: [] }
  await hooks["experimental.chat.system.transform"]({ sessionID: "s1" }, out)
  await hooks.event({ event: { type: "session.idle", properties: { sessionID: "s1" } } })
  await hooks.event({ event: { type: "session.deleted", properties: { info: { id: "s1" } } } })
  console.log("PASS fail-open-dead-server")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, nil, deadServerDriver)
	if !strings.Contains(out, "PASS fail-open-dead-server") {
		t.Fatalf("driver did not report PASS fail-open-dead-server:\n%s", out)
	}

	noClientDriver := `
  const hooks = await PunkMemoryPlugin({ directory: "/tmp/punk-messaging-proj" })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })
  await sleep(200)
  must(promptCalls.length === 0, "no prompts without an SDK client")
  must(punkTestServer.sseFetches === 0, "no SSE streams without an SDK client")
  must(
    !fetchCalls.some((c) => c.path.endsWith("/members")),
    "no member registration without an SDK client (nothing could ever be delivered)"
  )
  console.log("PASS fail-open-no-client")
  process.exit(0)
`
	out = runOpenCodeMessagingHarness(t, nil, noClientDriver)
	if !strings.Contains(out, "PASS fail-open-no-client") {
		t.Fatalf("driver did not report PASS fail-open-no-client:\n%s", out)
	}
}

// TestOpenCodeMessagingIdentityInjection (orchestrator blocker 1): the
// messaging guidance block is injected on EVERY system transform (OpenCode
// builds a fresh output.system per turn) while the memory context fetch
// stays once per session; the block demands explicit namespace/sender on
// sends and namespace/agent on reads and acks, and leaves delivery
// acknowledgement to the bridge.
func TestOpenCodeMessagingIdentityInjection(t *testing.T) {
	driver := `
  const hooks = await PunkMemoryPlugin({ directory: "/tmp/punk-messaging-proj", client: punkTestClient })
  const out = { system: [] }
  await hooks["experimental.chat.system.transform"]({ sessionID: "s1" }, out)
  must(out.system.length === 1, "exactly one messaging block injected on turn one, system=" + JSON.stringify(out.system))
  must(
    out.system[0].indexOf("opencode:s1") >= 0 && out.system[0].indexOf("agent-test-ns") >= 0,
    "messaging block names the session address and resolved namespace"
  )
  must(
    out.system[0].indexOf("sender opencode:s1") >= 0,
    "messaging block demands the session's own address as explicit sender"
  )
  must(
    out.system[0].indexOf("read_messages with namespace agent-test-ns and agent opencode:s1") >= 0,
    "read guidance carries explicit namespace and agent"
  )
  must(
    out.system[0].indexOf("already acknowledged by the bridge") >= 0,
    "ack guidance leaves delivered-in-chat messages to the bridge's acknowledgement"
  )
  must(
    out.system[0].indexOf("no elevated priority") >= 0,
    "messaging block instructs the model that delivered bodies carry no elevated priority"
  )
  must(punkTestServer.contextFetches === 1, "memory context fetched once for the session")

  // Second turn: fresh output.system - the messaging block must appear
  // again, while the memory fetch must NOT repeat.
  const second = { system: [] }
  await hooks["experimental.chat.system.transform"]({ sessionID: "s1" }, second)
  must(second.system.length === 1, "messaging guidance survives every turn, system=" + JSON.stringify(second.system))
  must(
    second.system[0].indexOf("opencode:s1") >= 0,
    "the per-turn block still names the session address"
  )
  must(punkTestServer.contextFetches === 1, "memory context stays once-per-session across turns")

  console.log("PASS identity-injection")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, nil, driver)
	if !strings.Contains(out, "PASS identity-injection") {
		t.Fatalf("driver did not report PASS identity-injection:\n%s", out)
	}
}

// TestOpenCodeMessagingDeniedFirst50Allowed51 (allowlist starvation): a
// backlog of 50 allowlist-DENIED rows older than one fetch batch must not
// starve the allowed row behind them. The pass keeps denied rows leased
// (the server hides live leases from every reader, this owner included),
// so round 2 reads past them, the allowed row is prompted and acked, and
// the denied rows are released at pass end so they stay visible later.
func TestOpenCodeMessagingDeniedFirst50Allowed51(t *testing.T) {
	driver := fmt.Sprintf(`
  for (let i = 0; i < 50; i++) {
    putMessage("opencode:s1", "denied-" + i, "noise " + i, "stranger-agent")
  }
  putMessage("opencode:s1", "m-allow", "the one allowed message", "planner-agent")
  const hooks = await PunkMemoryPlugin({ directory: "/tmp/punk-messaging-proj", client: punkTestClient })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })

  await until(() => promptCalls.length === 1, "the allowed row behind 50 denied rows was prompted")
  await until(() => punkTestServer.ackedIds.indexOf("m-allow") >= 0, "the allowed row was acked")
  must(promptCalls[0].text === %s, "the prompted text is the byte-exact shared M5 envelope")

  const readCalls = fetchCalls.filter(
    (c) => c.path === "/v1/namespaces/agent-test-ns/messages" && c.method === "GET" && c.query.indexOf("lease_seconds=") >= 0
  )
  must(readCalls.length >= 2, "the pass needed a second round to read past the denied batch, rounds=" + readCalls.length)
  let deniedRows = 0
  for (const m of punkTestServer.inbox["opencode:s1"] || []) {
    if (m.id.indexOf("denied-") === 0) deniedRows++
  }
  must(deniedRows === 50, "all 50 denied rows stay unread server-side, rows=" + deniedRows)
  let releasedDenied = 0
  for (const r of punkTestServer.releaseCalls) {
    if (r.id.indexOf("denied-") === 0) releasedDenied++
  }
  // >= 50: the delivering pass releases them all; a later queued drain
  // may re-lease and re-release the (now visible) denied rows, which is
  // benign - they simply stay available to the next event.
  must(releasedDenied >= 50, "every denied row was released at pass end, released=" + releasedDenied)
  must(!punkTestServer.ackedIds.some((a) => a.indexOf("denied-") === 0), "no denied row was ever acked")

  console.log("PASS denied-first50-allowed51")
  process.exit(0)
`, jsStringLiteral(expectedOpenCodeEnvelope("opencode:s1", "m-allow", "planner-agent", "the one allowed message")))
	out := runOpenCodeMessagingHarness(t, map[string]string{"PUNK_MESSAGING_FROM": "planner"}, driver)
	if !strings.Contains(out, "PASS denied-first50-allowed51") {
		t.Fatalf("driver did not report PASS denied-first50-allowed51:\n%s", out)
	}
}

// TestOpenCodeMessagingAckZeroOnExpiredLease: the server answers
// {acked:0} (HTTP 200) when the lease expired before the ACK - that is
// NOT success. The id stays pending (never re-prompted) and the scheduled
// post-expiry pass reacquires the row and confirms the ACK.
func TestOpenCodeMessagingAckZeroOnExpiredLease(t *testing.T) {
	driver := `
  punkTestServer.ackZeroNext = true
  putMessage("opencode:s1", "m1", "acked only after reacquire")
  const hooks = await PunkMemoryPlugin({ directory: "/tmp/punk-messaging-proj", client: punkTestClient })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })

  await until(() => promptCalls.length === 1, "message prompted once")
  await until(() => punkTestServer.ackCalls >= 1, "first ack attempted")
  await sleep(200)
  must(punkTestServer.ackedIds.indexOf("m1") < 0, "the {acked:0} answer left the message unacked")
  must(promptCalls.length === 1, "the pending ACK must not re-prompt the model, prompts=" + promptCalls.length)
  must(punkTestServer.inbox["opencode:s1"].length === 1, "the row is still unread server-side after the failed ACK")

  // The scheduled re-acquire pass (after the 1s test lease expires)
  // re-leases the row and re-acks it successfully.
  await until(() => punkTestServer.ackedIds.indexOf("m1") >= 0, "the pending ACK was reacquired and confirmed", 5000)
  must(promptCalls.length === 1, "the re-ack never re-prompts the model, prompts=" + promptCalls.length)
  must(punkTestServer.ackCalls >= 2, "the ack was actually retried, ackCalls=" + punkTestServer.ackCalls)

  console.log("PASS ack-zero-expired-lease")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, nil, driver)
	if !strings.Contains(out, "PASS ack-zero-expired-lease") {
		t.Fatalf("driver did not report PASS ack-zero-expired-lease:\n%s", out)
	}
}

// TestOpenCodeMessagingWakeCapSuppressesNoAck: bridge-triggered prompts
// are capped per sliding window (PUNK_MESSAGING_MAX_CONTINUE, default 5,
// same env defaults as the pi bridge); past the cap the remaining rows
// are suppressed WITHOUT being acked, released, and stay unread for a
// later window.
func TestOpenCodeMessagingWakeCapSuppressesNoAck(t *testing.T) {
	driver := `
  for (let i = 0; i < 6; i++) {
    putMessage("opencode:s1", "w" + i, "wake candidate " + i)
  }
  const hooks = await PunkMemoryPlugin({ directory: "/tmp/punk-messaging-proj", client: punkTestClient })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })

  await until(() => punkTestServer.ackedIds.length === 5, "the first five wake candidates were prompted and acked", 8000)
  await sleep(300)
  must(promptCalls.length === 5, "the wake cap suppressed the sixth prompt, prompts=" + promptCalls.length)
  must(punkTestServer.ackedIds.indexOf("w5") < 0, "the suppressed message was never acked")
  let w5row = (punkTestServer.inbox["opencode:s1"] || []).find((m) => m.id === "w5")
  must(w5row !== undefined, "the suppressed message stays unread server-side")
  must(punkTestServer.releaseCalls.some((r) => r.id === "w5"), "the suppressed message's lease was released, not silently held")

  console.log("PASS wake-cap-suppresses-no-ack")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, nil, driver)
	if !strings.Contains(out, "PASS wake-cap-suppresses-no-ack") {
		t.Fatalf("driver did not report PASS wake-cap-suppresses-no-ack:\n%s", out)
	}
}

// TestOpenCodeMessagingNonOKFetchRetry: a drain whose leased fetch gets
// a non-OK response schedules one bounded retry (same backoff env as the
// SSE reconnects) instead of waiting for the next external hint; the
// message is delivered by the retry.
func TestOpenCodeMessagingNonOKFetchRetry(t *testing.T) {
	driver := `
  punkTestServer.messageFailNext = 1 // the first leased read answers 500
  putMessage("opencode:s1", "m1", "delivered by the fetch retry")
  const hooks = await PunkMemoryPlugin({ directory: "/tmp/punk-messaging-proj", client: punkTestClient })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })

  await until(() => promptCalls.length === 1, "message delivered after the non-OK fetch was retried", 6000)
  await until(() => punkTestServer.ackedIds.indexOf("m1") >= 0, "message acked")
  const readCalls = fetchCalls.filter(
    (c) => c.path === "/v1/namespaces/agent-test-ns/messages" && c.method === "GET" && c.query.indexOf("lease_seconds=") >= 0
  )
  must(readCalls.length >= 2, "the failed fetch was actually retried, reads=" + readCalls.length)

  console.log("PASS non-ok-fetch-retry")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, nil, driver)
	if !strings.Contains(out, "PASS non-ok-fetch-retry") {
		t.Fatalf("driver did not report PASS non-ok-fetch-retry:\n%s", out)
	}
}

// TestOpenCodeMessagingCapZeroZeroPrompts (cap parse):
// PUNK_MESSAGING_MAX_CONTINUE=0 is a VALID value that disables waking
// entirely (non-negative parse - it must not fall back to the default 5):
// zero prompts, the rows are released unread, and no window retry is
// scheduled because no window can ever free a slot.
func TestOpenCodeMessagingCapZeroZeroPrompts(t *testing.T) {
	driver := `
  putMessage("opencode:s1", "m1", "no wakes are allowed")
  putMessage("opencode:s1", "m2", "neither is this one")
  const hooks = await PunkMemoryPlugin({ directory: "/tmp/punk-messaging-proj", client: punkTestClient })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })

  await until(() => punkTestServer.registeredAgents.some((r) => r.agent === "opencode:s1"), "session registered")
  await sleep(400)
  must(promptCalls.length === 0, "cap 0 must disable waking entirely, prompts=" + promptCalls.length)
  must(punkTestServer.ackedIds.length === 0, "nothing was acked")
  for (const id of ["m1", "m2"]) {
    must(punkTestServer.releaseCalls.some((r) => r.id === id), "the suppressed row " + id + " was released, not silently held")
  }
  must(punkTestServer.inbox["opencode:s1"].length === 2, "both rows stay unread server-side")

  // No scheduled wake may fire later: the reads must stay frozen.
  const reads = fetchCalls.filter(
    (c) => c.path === "/v1/namespaces/agent-test-ns/messages" && c.method === "GET" && c.query.indexOf("lease_seconds=") >= 0
  ).length
  await sleep(1500)
  must(
    fetchCalls.filter((c) => c.path === "/v1/namespaces/agent-test-ns/messages" && c.method === "GET" && c.query.indexOf("lease_seconds=") >= 0).length === reads,
    "cap 0 must not schedule a window retry, reads grew past " + reads
  )
  must(promptCalls.length === 0, "still zero prompts after the wait")

  console.log("PASS cap-zero-zero-prompts")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, map[string]string{"PUNK_MESSAGING_MAX_CONTINUE": "0"}, driver)
	if !strings.Contains(out, "PASS cap-zero-zero-prompts") {
		t.Fatalf("driver did not report PASS cap-zero-zero-prompts:\n%s", out)
	}
}

// TestOpenCodeMessagingWakeWindowRoll (cap-exhausted self wake): after
// the cap suppresses a backlog on an idle session (OpenCode's synthetic
// prompts do not mark the session busy), one cancellable wake at the next
// window expiry delivers the remainder by itself - no event, no hint, no
// unrelated trigger. Short window (1s) stands in for the test clock.
func TestOpenCodeMessagingWakeWindowRoll(t *testing.T) {
	driver := `
  putMessage("opencode:s1", "w1", "wake one")
  putMessage("opencode:s1", "w2", "wake two")
  const hooks = await PunkMemoryPlugin({ directory: "/tmp/punk-messaging-proj", client: punkTestClient })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })

  // Cap 1: the drain prompts w1, suppresses w2, releases it, and
  // schedules the single window-expiry wake.
  await until(() => promptCalls.length === 1, "wake one prompted")
  await until(() => punkTestServer.ackedIds.indexOf("w1") >= 0, "wake one acked")
  await sleep(300)
  must(promptCalls.length === 1, "the cap suppressed the second prompt, prompts=" + promptCalls.length)
  must(punkTestServer.ackedIds.indexOf("w2") < 0, "the suppressed row was not acked")
  must(punkTestServer.releaseCalls.some((r) => r.id === "w2"), "the suppressed row was released")

  // Window roll (1s): the scheduled retry prompts w2 with NO further
  // event or hint of any kind.
  await until(() => promptCalls.length === 2, "the window-expiry retry delivered the suppressed row", 6000)
  await until(() => punkTestServer.ackedIds.indexOf("w2") >= 0, "the suppressed row was acked")
  must(promptCalls[1].text.indexOf("wake two") >= 0, "the right message was prompted")

  console.log("PASS wake-window-roll")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, map[string]string{
		"PUNK_MESSAGING_MAX_CONTINUE":            "1",
		"PUNK_MESSAGING_CONTINUE_WINDOW_SECONDS": "1",
	}, driver)
	if !strings.Contains(out, "PASS wake-window-roll") {
		t.Fatalf("driver did not report PASS wake-window-roll:\n%s", out)
	}
}

// TestOpenCodeMessagingRestoredBindsNewestAndLazy: session.list returns
// every stored session of the project, so a restart must not register
// and stream for hundreds of finished conversations. Only busy/retry
// sessions from the status snapshot and the single most recently updated
// session bind at startup; older ones bind lazily on their first event,
// and a human chat.message binds a resumed session that never emitted
// session.created.
func TestOpenCodeMessagingRestoredBindsNewestAndLazy(t *testing.T) {
	driver := `
  const D = "/tmp/punk-messaging-proj"
  punkTestServer.sessionList = [
    { id: "old-1", directory: D, time: { created: 1, updated: 1000 } },
    { id: "newest-1", directory: D, time: { created: 1, updated: 5000 } },
    { id: "old-2", directory: D, time: { created: 1, updated: 2000 } },
    { id: "busy-1", directory: D, time: { created: 1, updated: 3000 } },
    { id: "elsewhere-1", directory: "/tmp/other-proj", time: { created: 1, updated: 9000 } },
  ]
  punkTestServer.statusMap = { "busy-1": { type: "busy" } }
  putMessage("opencode:newest-1", "mn", "for the newest session")
  putMessage("opencode:old-2", "mo", "for an older session")
  putMessage("opencode:old-1", "mr", "for a resumed session")

  const hooks = await PunkMemoryPlugin({ directory: D, client: punkTestClient })
  await until(() => promptCalls.length === 1, "the newest idle stored session is delivered at startup")
  must(promptCalls[0].sessionID === "newest-1", "startup delivery went to newest-1, got " + promptCalls[0].sessionID)
  await sleep(300)
  const bound = () => punkTestServer.registeredAgents.map((r) => r.agent)
  must(bound().indexOf("opencode:busy-1") >= 0, "the busy snapshot session is bound, registered=" + JSON.stringify(bound()))
  must(
    bound().indexOf("opencode:old-1") < 0 && bound().indexOf("opencode:old-2") < 0 && bound().indexOf("opencode:elsewhere-1") < 0,
    "older and foreign stored sessions are not bound at startup, registered=" + JSON.stringify(bound())
  )
  must(punkTestServer.sseFetches === 2, "one stream per bound session (busy-1, newest-1), got " + punkTestServer.sseFetches)

  // First sign of life binds lazily and delivers.
  await hooks.event({ event: { type: "session.status", properties: { sessionID: "old-2", status: { type: "idle" } } } })
  await until(() => promptCalls.length === 2, "old-2 bound on its first status event and delivered")
  must(promptCalls[1].sessionID === "old-2", "lazy delivery went to old-2, got " + promptCalls[1].sessionID)
  await until(() => punkTestServer.ackedIds.indexOf("mo") >= 0, "old-2's message acked")

  // A human turn in a resumed session binds it busy: registered and
  // streaming, but nothing delivered until the turn ends.
  await hooks["chat.message"]({ sessionID: "old-1" }, { message: { role: "user" }, parts: [{ type: "text", text: "resume work" }] })
  await until(() => bound().indexOf("opencode:old-1") >= 0, "chat.message bound the resumed session")
  await sleep(250)
  must(promptCalls.length === 2, "busy resumed session defers delivery, prompts=" + promptCalls.length)
  await hooks.event({ event: { type: "session.idle", properties: { sessionID: "old-1" } } })
  await until(() => promptCalls.length === 3, "resumed session delivered once idle")
  must(promptCalls[2].sessionID === "old-1", "delivery went to old-1, got " + promptCalls[2].sessionID)
  must(bound().indexOf("opencode:elsewhere-1") < 0, "a foreign-directory session is never bound")

  console.log("PASS restored-newest-lazy")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, nil, driver)
	if !strings.Contains(out, "PASS restored-newest-lazy") {
		t.Fatalf("driver did not report PASS restored-newest-lazy:\n%s", out)
	}
}

// TestOpenCodeMessagingDiagnosticsTransitions: the bridge reports the exact
// shared diagnostics contract (spec 2026-09-28) - ready after a confirmed
// registration of an idle session, waiting_for_idle when a human turn marks
// the session busy, ready again once the deferred delivery completes - with
// the required fields (agent, client, delivery_mode, state) and nonnegative
// bounded counts; any timestamp present parses as RFC3339.
func TestOpenCodeMessagingDiagnosticsTransitions(t *testing.T) {
	driver := `
  putMessage("opencode:s1", "m1", "hello diagnostics")
  const hooks = await PunkMemoryPlugin({ directory: "/tmp/punk-messaging-proj", client: punkTestClient })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })

  await until(() => punkTestServer.diagCalls.length >= 1, "a diagnostic was posted after registration")
  const first = punkTestServer.diagCalls[0]
  must(first.agent === "opencode:s1", "agent is the session address, got " + first.agent)
  must(first.client === "opencode", "client identifies the bridge, got " + first.client)
  must(first.delivery_mode === "idle_wake", "delivery_mode is idle_wake, got " + first.delivery_mode)
  must(first.state === "ready", "freshly registered idle session reports ready, got " + first.state)
  must(
    typeof first.pending_ack_count === "number" && first.pending_ack_count >= 0,
    "pending_ack_count is a nonnegative integer"
  )
  must(typeof first.wake_count === "number" && first.wake_count >= 0, "wake_count is a nonnegative integer")

  await hooks["chat.message"](
    { sessionID: "s1", messageID: "u1" },
    { message: { role: "user" }, parts: [{ type: "text", text: "busy now" }] }
  )
  await until(
    () => punkTestServer.diagCalls.some((d) => d.state === "waiting_for_idle"),
    "busy transition reported waiting_for_idle"
  )

  for (const d of punkTestServer.diagCalls) {
    for (const k of ["last_attempt_at", "next_attempt_at"]) {
      if (d[k] !== undefined) {
        must(!isNaN(Date.parse(d[k])), k + " must parse as an RFC3339 timestamp, got " + d[k])
      }
    }
    if (d.last_error !== undefined) {
      must(typeof d.last_error === "string" && d.last_error.length > 0 && d.last_error.length <= 64, "last_error is a short machine reason")
    }
  }

  await hooks.event({ event: { type: "session.idle", properties: { sessionID: "s1" } } })
  await until(() => promptCalls.length === 1, "deferred message delivered after idle")
  await until(() => punkTestServer.ackedIds.indexOf("m1") >= 0, "message acked")
  await until(
    () => punkTestServer.diagCalls.some((d) => d.state === "ready" && d.pending_ack_count === 0),
    "post-delivery ready with zero pending"
  )

  console.log("PASS diagnostics-transitions")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, nil, driver)
	if !strings.Contains(out, "PASS diagnostics-transitions") {
		t.Fatalf("driver did not report PASS diagnostics-transitions:\n%s", out)
	}
}

// TestOpenCodeMessagingDiagnosticsFailureStates: honest observations for
// failure outcomes - a failed prompt reports delivery_failed with the
// machine reason prompt_failed and a last_attempt_at; a successful handoff
// whose ACK fails reports handoff_unconfirmed with ack_failed, the pending
// ACK count, and a future RFC3339 next_attempt_at (the scheduled re-ack).
func TestOpenCodeMessagingDiagnosticsFailureStates(t *testing.T) {
	driver := `
  punkTestServer.promptPlan = ["throw"]
  putMessage("opencode:s1", "m1", "will fail to prompt")
  const hooks = await PunkMemoryPlugin({ directory: "/tmp/punk-messaging-proj", client: punkTestClient })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })

  await until(
    () => punkTestServer.diagCalls.some((d) => d.state === "delivery_failed"),
    "prompt failure reported delivery_failed"
  )
  const df = punkTestServer.diagCalls.find((d) => d.state === "delivery_failed")
  must(df.last_error === "prompt_failed", "last_error is the machine reason prompt_failed, got " + df.last_error)
  must(
    df.last_attempt_at !== undefined && !isNaN(Date.parse(df.last_attempt_at)),
    "delivery_failed carries an RFC3339 last_attempt_at"
  )

  punkTestServer.promptMode = "ok"
  punkTestServer.ackFailNext = 1
  putMessage("opencode:s1", "m2", "handoff ok, ack will fail")
  emitHint("opencode:s1")
  await until(
    () => punkTestServer.diagCalls.some((d) => d.state === "handoff_unconfirmed"),
    "failed ACK reported handoff_unconfirmed"
  )
  const hu = punkTestServer.diagCalls.find((d) => d.state === "handoff_unconfirmed")
  must(hu.last_error === "ack_failed", "last_error is the machine reason ack_failed, got " + hu.last_error)
  must(hu.pending_ack_count >= 1, "the unACKed handoff is counted, got " + hu.pending_ack_count)
  must(
    hu.next_attempt_at !== undefined && !isNaN(Date.parse(hu.next_attempt_at)) && Date.parse(hu.next_attempt_at) > Date.now() - 1000,
    "next_attempt_at is a future RFC3339 retry time, got " + hu.next_attempt_at
  )

  await until(() => punkTestServer.ackedIds.indexOf("m2") >= 0, "the scheduled re-ack confirms the handoff", 6000)
  await until(() => promptCalls.length >= 2, "m1 and m2 were both prompted")

  console.log("PASS diagnostics-failure-states")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, nil, driver)
	if !strings.Contains(out, "PASS diagnostics-failure-states") {
		t.Fatalf("driver did not report PASS diagnostics-failure-states:\n%s", out)
	}
}

// TestOpenCodeMessagingDiagnosticsWakeCap: when the sliding-window wake cap
// suppresses the remaining backlog, the drain outcome reports
// wake_budget_exhausted with the wakes already spent and an RFC3339
// next_attempt_at at the window roll.
func TestOpenCodeMessagingDiagnosticsWakeCap(t *testing.T) {
	driver := `
  putMessage("opencode:s1", "w1", "wake one")
  putMessage("opencode:s1", "w2", "wake two")
  const hooks = await PunkMemoryPlugin({ directory: "/tmp/punk-messaging-proj", client: punkTestClient })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })

  await until(() => punkTestServer.ackedIds.indexOf("w1") >= 0, "w1 delivered within the cap")
  await until(
    () => punkTestServer.diagCalls.some((d) => d.state === "wake_budget_exhausted"),
    "cap suppression reported wake_budget_exhausted"
  )
  const we = punkTestServer.diagCalls.find((d) => d.state === "wake_budget_exhausted")
  must(we.wake_count === 1, "wake_count reports the wakes already spent, got " + we.wake_count)
  must(
    we.next_attempt_at !== undefined && !isNaN(Date.parse(we.next_attempt_at)),
    "next_attempt_at is an RFC3339 window-roll time, got " + we.next_attempt_at
  )
  must(punkTestServer.ackedIds.indexOf("w2") < 0, "the suppressed row was not acked")

  console.log("PASS diagnostics-wake-cap")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, map[string]string{"PUNK_MESSAGING_MAX_CONTINUE": "1"}, driver)
	if !strings.Contains(out, "PASS diagnostics-wake-cap") {
		t.Fatalf("driver did not report PASS diagnostics-wake-cap:\n%s", out)
	}
}

// TestOpenCodeMessagingDiagnosticsDedup: identical hot-loop snapshots are
// suppressed - dozens of inbox hints draining an empty backlog must not
// re-POST the same ready observation.
func TestOpenCodeMessagingDiagnosticsDedup(t *testing.T) {
	driver := `
  const hooks = await PunkMemoryPlugin({ directory: "/tmp/punk-messaging-proj", client: punkTestClient })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })
  await until(
    () => punkTestServer.registeredAgents.some((r) => r.agent === "opencode:s1"),
    "session registered"
  )
  await until(
    () => punkTestServer.diagCalls.filter((d) => d.state === "ready").length >= 1,
    "the initial ready observation was posted"
  )
  const before = punkTestServer.diagCalls.filter((d) => d.state === "ready").length

  for (let i = 0; i < 25; i++) {
    emitHint("opencode:s1")
  }
  await sleep(500)
  const after = punkTestServer.diagCalls.filter((d) => d.state === "ready").length
  must(
    after <= before + 1,
    "identical hot-loop ready reports are suppressed, ready posts=" + after + " (was " + before + ")"
  )

  console.log("PASS diagnostics-dedup")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, nil, driver)
	if !strings.Contains(out, "PASS diagnostics-dedup") {
		t.Fatalf("driver did not report PASS diagnostics-dedup:\n%s", out)
	}
}

// TestOpenCodeMessagingDiagnosticsOldServer404: a server without the
// diagnostics route (every POST answered 404) must not disturb delivery or
// ACK in any way.
func TestOpenCodeMessagingDiagnosticsOldServer404(t *testing.T) {
	driver := `
  punkTestServer.diag404 = true
  putMessage("opencode:s1", "m1", "delivered although the server has no diagnostics route")
  const hooks = await PunkMemoryPlugin({ directory: "/tmp/punk-messaging-proj", client: punkTestClient })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })

  await until(() => punkTestServer.diagCalls.length >= 1, "the diagnostics POST was attempted despite the 404s")
  await until(() => promptCalls.length === 1, "message delivered despite diagnostics 404s")
  await until(() => punkTestServer.ackedIds.indexOf("m1") >= 0, "ack confirmed despite diagnostics 404s")
  must(promptCalls.length === 1, "exactly one delivery, prompts=" + promptCalls.length)

  console.log("PASS diagnostics-old-server-404")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, nil, driver)
	if !strings.Contains(out, "PASS diagnostics-old-server-404") {
		t.Fatalf("driver did not report PASS diagnostics-old-server-404:\n%s", out)
	}
}

// TestOpenCodeMessagingDiagnosticsSenderFiltered: an allowlist-denied
// sender produces an honest sender_filtered observation, no prompt and no
// ACK.
func TestOpenCodeMessagingDiagnosticsSenderFiltered(t *testing.T) {
	driver := `
  putMessage("opencode:s1", "m1", "from a stranger", "stranger-agent")
  const hooks = await PunkMemoryPlugin({ directory: "/tmp/punk-messaging-proj", client: punkTestClient })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })

  await until(
    () => punkTestServer.diagCalls.some((d) => d.state === "sender_filtered"),
    "allowlist denial reported sender_filtered"
  )
  must(promptCalls.length === 0, "denied sender is never prompted, prompts=" + promptCalls.length)
  must(punkTestServer.ackedIds.length === 0, "denied sender is never acked")

  console.log("PASS diagnostics-sender-filtered")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, map[string]string{"PUNK_MESSAGING_FROM": "planner"}, driver)
	if !strings.Contains(out, "PASS diagnostics-sender-filtered") {
		t.Fatalf("driver did not report PASS diagnostics-sender-filtered:\n%s", out)
	}
}

// TestOpenCodeMessagingRestartRecoveryReacksWithoutPrompt: a successful
// prompt handoff whose ACK fails is persisted BEFORE the ACK attempt; after
// a restart (a second plugin life over the same state home) the restored id
// is re-ACKed without ever re-prompting the model, and the restart reports
// the honest handoff_unconfirmed observation until the ACK confirms.
func TestOpenCodeMessagingRestartRecoveryReacksWithoutPrompt(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	driver := `
  const fs = await import("node:fs")
  const D = "/tmp/punk-messaging-proj"
  // Server-shaped id (32 lowercase hex): the restarted life validates
  // restored ids against the real server syntax.
  const MID = "d4" + "0".repeat(30)
  punkTestServer.ackFailNext = 1
  putMessage("opencode:s1", MID, "delivered before the crash")
  const hooks1 = await PunkMemoryPlugin({ directory: D, client: punkTestClient })
  await hooks1.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })

  await until(() => promptCalls.length === 1, "message prompted once in the first life")
  await until(() => punkTestServer.ackCalls >= 1, "first ack attempted and failed")

  // The handoff must already be persisted: the save runs to completion
  // after the successful prompt and BEFORE the ACK attempt.
  const stateDir = process.env.XDG_STATE_HOME + "/punk/inbox/opencode"
  const files = fs.readdirSync(stateDir)
  must(files.length === 1, "one recovery file per identity, got " + JSON.stringify(files))
  const rec = JSON.parse(fs.readFileSync(stateDir + "/" + files[0], "utf8"))
  must(
    rec.pending_ids.indexOf(MID) >= 0,
    "the handed-off-but-unACKed id was persisted, got " + JSON.stringify(rec.pending_ids)
  )
  must(rec.address === "opencode:s1" && rec.namespace === "agent-test-ns", "identity fields isolate the record")
  must(rec.server === "http://punk.test", "the server URL isolates the record")

  // Crash: dispose the first life before its re-ack retry can fire.
  await sleep(150)
  await hooks1.dispose()
  await sleep(1400) // past the 1s test lease: the row is visible to any reader again

  // Restart: the stored session binds through the startup snapshot.
  punkTestServer.sessionList = [{ id: "s1", directory: D }]
  const hooks2 = await PunkMemoryPlugin({ directory: D, client: punkTestClient })
  await until(
    () => punkTestServer.registeredAgents.filter((r) => r.agent === "opencode:s1").length >= 2,
    "the restarted life registered the session again"
  )
  await until(() => punkTestServer.ackedIds.indexOf(MID) >= 0, "restarted bridge re-acked the restored id", 6000)
  must(
    promptCalls.length === 1,
    "restart must re-ACK without re-prompting the model, prompts=" + promptCalls.length
  )
  must(
    punkTestServer.diagCalls.some((d) => d.state === "handoff_unconfirmed" && d.pending_ack_count >= 1),
    "the restarted life reported the honest handoff-unconfirmed observation"
  )

  // The confirmed ACK drains the restored id out of the recovery file.
  await until(() => {
    const r = JSON.parse(fs.readFileSync(stateDir + "/" + files[0], "utf8"))
    return r.pending_ids.indexOf(MID) < 0
  }, "confirmed ACK removed the id from the recovery file", 5000)

  console.log("PASS restart-recovery-reack")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, map[string]string{"XDG_STATE_HOME": stateHome}, driver)
	if !strings.Contains(out, "PASS restart-recovery-reack") {
		t.Fatalf("driver did not report PASS restart-recovery-reack:\n%s", out)
	}
}

// TestOpenCodeMessagingRestartWakeBudgetSurvives: the sliding-window wake
// budget survives a restart - a wake spent in the first life suppresses a
// fresh bridge-triggered prompt in the second life over the same state
// home, with an honest wake_budget_exhausted observation.
func TestOpenCodeMessagingRestartWakeBudgetSurvives(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	driver := `
  const D = "/tmp/punk-messaging-proj"
  putMessage("opencode:s1", "w1", "the only wake of the first life")
  const hooks1 = await PunkMemoryPlugin({ directory: D, client: punkTestClient })
  await hooks1.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })
  await until(() => punkTestServer.ackedIds.indexOf("w1") >= 0, "w1 delivered and acked in the first life")
  await hooks1.dispose()

  punkTestServer.sessionList = [{ id: "s1", directory: D }]
  putMessage("opencode:s1", "w2", "must stay suppressed after the restart")
  const hooks2 = await PunkMemoryPlugin({ directory: D, client: punkTestClient })
  await until(
    () => punkTestServer.registeredAgents.filter((r) => r.agent === "opencode:s1").length >= 2,
    "the restarted life registered the session again"
  )
  await sleep(600)
  must(
    promptCalls.length === 1,
    "restored wake budget suppresses the new bridge-triggered prompt, prompts=" + promptCalls.length
  )
  must(punkTestServer.ackedIds.indexOf("w2") < 0, "the suppressed row was never acked")
  must(punkTestServer.releaseCalls.some((r) => r.id === "w2"), "the suppressed row was released")
  must(
    punkTestServer.diagCalls.some((d) => d.state === "wake_budget_exhausted" && d.wake_count === 1),
    "the restarted life reported the restored exhausted wake budget"
  )

  console.log("PASS restart-wake-budget")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, map[string]string{
		"XDG_STATE_HOME":                         stateHome,
		"PUNK_MESSAGING_MAX_CONTINUE":            "1",
		"PUNK_MESSAGING_CONTINUE_WINDOW_SECONDS": "600",
	}, driver)
	if !strings.Contains(out, "PASS restart-wake-budget") {
		t.Fatalf("driver did not report PASS restart-wake-budget:\n%s", out)
	}
}

// TestOpenCodeMessagingRecoveryCorruptStateFailsOpen: a corrupt recovery
// file must never break the host - delivery and ACK proceed normally and
// the next save rewrites the file cleanly.
func TestOpenCodeMessagingRecoveryCorruptStateFailsOpen(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	driver := `
  const fs = await import("node:fs")
  const crypto = await import("node:crypto")
  const joined = "http://punk.test\u0000agent-test-ns\u0000opencode:s1"
  const h = crypto.createHash("sha256").update(joined).digest("hex").slice(0, 16)
  const dir = process.env.XDG_STATE_HOME + "/punk/inbox/opencode"
  fs.mkdirSync(dir, { recursive: true })
  const file = dir + "/opencode_s1-" + h + ".json"
  fs.writeFileSync(file, "{corrupt not json at all", { mode: 0o600 })

  putMessage("opencode:s1", "m1", "delivered despite corrupt recovery state")
  const hooks = await PunkMemoryPlugin({ directory: "/tmp/punk-messaging-proj", client: punkTestClient })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })
  await until(
    () => punkTestServer.ackedIds.indexOf("m1") >= 0,
    "corrupt state fails open: delivery and ACK complete"
  )
  must(promptCalls.length === 1, "exactly one prompt despite the corrupt file")

  // The post-handoff save rewrites the corrupt file into a clean record.
  const healed = JSON.parse(fs.readFileSync(file, "utf8"))
  must(Array.isArray(healed.pending_ids), "the corrupt file was replaced by a well-formed record")

  console.log("PASS recovery-corrupt-fails-open")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, map[string]string{"XDG_STATE_HOME": stateHome}, driver)
	if !strings.Contains(out, "PASS recovery-corrupt-fails-open") {
		t.Fatalf("driver did not report PASS recovery-corrupt-fails-open:\n%s", out)
	}
}

// TestOpenCodeMessagingRecoveryUnwritableFailsOpen: a state home that
// cannot be created (XDG_STATE_HOME points at a regular file) must fail
// open - the generic write-failure is logged once, delivery and ACK
// complete purely in memory.
func TestOpenCodeMessagingRecoveryUnwritableFailsOpen(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state-file-blocker")
	driver := `
  const fs = await import("node:fs")
  // XDG_STATE_HOME names an existing regular FILE: every mkdir under it
  // fails with ENOTDIR, so every recovery write fails.
  fs.writeFileSync(process.env.XDG_STATE_HOME, "a regular file, not a directory")

  putMessage("opencode:s1", "m1", "delivered despite unwritable recovery state")
  const hooks = await PunkMemoryPlugin({ directory: "/tmp/punk-messaging-proj", client: punkTestClient })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })
  await until(
    () => punkTestServer.ackedIds.indexOf("m1") >= 0,
    "unwritable state fails open: delivery and ACK complete"
  )
  must(promptCalls.length === 1, "exactly one prompt despite the failed saves")

  console.log("PASS recovery-unwritable-fails-open")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, map[string]string{"XDG_STATE_HOME": stateHome}, driver)
	if !strings.Contains(out, "PASS recovery-unwritable-fails-open") {
		t.Fatalf("driver did not report PASS recovery-unwritable-fails-open:\n%s", out)
	}
}

// TestOpenCodeMessagingRecoveryPersistsNoSecretsOrBodies: the recovery
// file carries ONLY identity fields, message ids and timestamps - never a
// message body and never the API key.
func TestOpenCodeMessagingRecoveryPersistsNoSecretsOrBodies(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	driver := `
  const fs = await import("node:fs")
  putMessage("opencode:s1", "m1", "top secret body text must never be persisted")
  const hooks = await PunkMemoryPlugin({ directory: "/tmp/punk-messaging-proj", client: punkTestClient })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })
  await until(() => punkTestServer.ackedIds.indexOf("m1") >= 0, "message delivered and acked")
  await sleep(200)

  const stateDir = process.env.XDG_STATE_HOME + "/punk/inbox/opencode"
  const files = fs.readdirSync(stateDir)
  must(files.length === 1, "one recovery file, got " + JSON.stringify(files))
  const raw = fs.readFileSync(stateDir + "/" + files[0], "utf8")
  const rec = JSON.parse(raw)
  const keys = Object.keys(rec).sort()
  must(
    JSON.stringify(keys) === JSON.stringify(["address", "namespace", "pending_ids", "saved_at", "server", "wake_times"]),
    "only identity, ids and timestamps are persisted, got " + JSON.stringify(keys)
  )
  must(raw.indexOf("top secret body text") < 0, "message bodies are never persisted")
  must(raw.indexOf("sekrit-token") < 0, "credentials are never persisted")
  must(rec.pending_ids.length === 0, "the confirmed ACK drained the pending ids")
  must(rec.wake_times.length === 1, "the wake stamp persists for the budget")

  console.log("PASS recovery-no-secrets")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, map[string]string{
		"XDG_STATE_HOME": stateHome,
		"PUNK_API_KEY":   "sekrit-token-xyz",
	}, driver)
	if !strings.Contains(out, "PASS recovery-no-secrets") {
		t.Fatalf("driver did not report PASS recovery-no-secrets:\n%s", out)
	}
}

// TestOpenCodeMessagingRecoveryConflictGuardSkipsAheadAndResumes: while a
// foreign snapshot is AHEAD of this process's clock (within the skew
// tolerance), this process's saves skip - the ahead record survives
// untouched. A foreign record at or below our clock is merged
// conservatively (its still-pending ids adopted into the delivered set,
// re-ack-or-reconcile against the server) and persistence continues with a
// newer snapshot - recency cannot distinguish a live writer from a dead
// one, so the merge never guesses and never silently drops the other
// writer's handed-off ids.
func TestOpenCodeMessagingRecoveryConflictGuardSkipsAheadAndResumes(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	driver := `
  const fs = await import("node:fs")
  const D = "/tmp/punk-messaging-proj"
  putMessage("opencode:s1", "m1", "first delivery")
  const hooks = await PunkMemoryPlugin({ directory: D, client: punkTestClient })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })
  await until(() => punkTestServer.ackedIds.indexOf("m1") >= 0, "m1 delivered and acked")
  await sleep(150)

  const stateDir = process.env.XDG_STATE_HOME + "/punk/inbox/opencode"
  const file = stateDir + "/" + fs.readdirSync(stateDir)[0]
  // A second live bridge process (honoring the same O_EXCL .lock sibling
  // convention, so the two writers serialize exactly as two real bridge
  // processes would) writes a snapshot AHEAD of our clock, carrying its
  // own still-pending handoff id (server-shaped: 32 lowercase hex, the
  // only syntax adoption accepts).
  const EXT = "c3" + "0".repeat(30)
  const lock = file + ".lock"
  async function withLock(fn) {
    const deadline = Date.now() + 5000
    for (;;) {
      try {
        const fh = await fs.promises.open(lock, "wx")
        await fh.close()
        break
      } catch (err) {}
      if (Date.now() > deadline) throw new Error("could not take the state lock")
      await sleep(25)
    }
    try {
      await fn()
    } finally {
      await fs.promises.unlink(lock)
    }
  }
  const aheadAt = Date.now() + 400
  await withLock(async () => {
    fs.writeFileSync(
      file,
      JSON.stringify({
        server: "http://punk.test",
        namespace: "agent-test-ns",
        address: "opencode:s1",
        pending_ids: [EXT],
        wake_times: [],
        saved_at: aheadAt,
      }) + "\n"
    )
  })

  // Our next handoff must NOT clobber the ahead writer.
  putMessage("opencode:s1", "m2", "delivered while another writer is ahead")
  emitHint("opencode:s1")
  await until(() => punkTestServer.ackedIds.indexOf("m2") >= 0, "m2 delivered and acked")
  await sleep(200)
  const mid = JSON.parse(fs.readFileSync(file, "utf8"))
  must(
    JSON.stringify(mid.pending_ids) === JSON.stringify([EXT]),
    "the ahead writer's record survived our handoff, got " + JSON.stringify(mid.pending_ids)
  )
  must(mid.saved_at === aheadAt, "the record on disk is still exactly the ahead writer's")

  // The foreign record is now at or below our clock (a live writer and a
  // dead one are indistinguishable here). Our next handoff must MERGE it
  // - adopting its pending id - and persist a newer snapshot.
  await sleep(700)
  putMessage("opencode:s1", "m3", "delivered over a foreign record at our clock")
  emitHint("opencode:s1")
  await until(() => punkTestServer.ackedIds.indexOf("m3") >= 0, "m3 delivered and acked")
  await until(() => {
    const r = JSON.parse(fs.readFileSync(file, "utf8"))
    return r.saved_at > aheadAt
  }, "the merged write persisted a newer snapshot", 5000)
  const fin = JSON.parse(fs.readFileSync(file, "utf8"))
  must(
    fin.pending_ids.indexOf(EXT) >= 0,
    "the foreign pending id was adopted into the merged snapshot, got " + JSON.stringify(fin.pending_ids)
  )
  must(
    promptCalls.filter((p) => p.text.indexOf("c3" + "000") >= 0).length === 0,
    "the adopted id is reconciled through re-ack, never prompted"
  )
  must(promptCalls.length === 3, "exactly the three fresh messages were prompted, prompts=" + promptCalls.length)

  console.log("PASS recovery-conflict-guard-resume")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, map[string]string{"XDG_STATE_HOME": stateHome}, driver)
	if !strings.Contains(out, "PASS recovery-conflict-guard-resume") {
		t.Fatalf("driver did not report PASS recovery-conflict-guard-resume:\n%s", out)
	}
}

// TestOpenCodeMessagingDisabledZeroStateEffects: with PUNK_MESSAGING off
// the bridge has ZERO filesystem and zero messaging-network effects - no
// diagnostics POSTs and no punk subtree under the state home - while the
// memory hooks keep working.
func TestOpenCodeMessagingDisabledZeroStateEffects(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	driver := `
  const fs = await import("node:fs")
  const hooks = await PunkMemoryPlugin({ directory: "/tmp/punk-messaging-proj", client: punkTestClient })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })
  await hooks["chat.message"](
    { sessionID: "s1", messageID: "u1" },
    { message: { role: "user" }, parts: [{ type: "text", text: "plain memory capture" }] }
  )
  await hooks.event({ event: { type: "session.idle", properties: { sessionID: "s1" } } })
  await hooks.event({ event: { type: "session.deleted", properties: { info: { id: "s1" } } } })
  const out = { system: [] }
  await hooks["experimental.chat.system.transform"]({ sessionID: "s1" }, out)
  await sleep(200)

  must(punkTestServer.diagCalls.length === 0, "no diagnostics POSTs when messaging is disabled")
  must(
    !fs.existsSync(process.env.XDG_STATE_HOME + "/punk"),
    "no filesystem effects under the state home when messaging is disabled"
  )
  must(promptCalls.length === 0 && punkTestServer.sseFetches === 0, "no delivery when messaging is disabled")

  console.log("PASS disabled-zero-state-effects")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, map[string]string{
		"PUNK_MESSAGING": "0",
		"XDG_STATE_HOME": stateHome,
	}, driver)
	if !strings.Contains(out, "PASS disabled-zero-state-effects") {
		t.Fatalf("driver did not report PASS disabled-zero-state-effects:\n%s", out)
	}
}

// recoveryFileJSHelper mirrors the bridge's recovery filename (slug +
// first 16 hex of sha256(server NUL ns NUL agent)) so drivers can
// pre-write and inspect state files exactly where the bridge puts them.
// Spliced inside the async main() driver body: the crypto import is
// hoisted to the top because a plain function cannot await.
const recoveryFileJSHelper = `
  const cryptoMod = await import("node:crypto")
  function recoveryFileFor(agent) {
    const joined = "http://punk.test\u0000agent-test-ns\u0000" + agent
    const h = cryptoMod.createHash("sha256").update(joined).digest("hex").slice(0, 16)
    const slug = String(agent).replace(/[^A-Za-z0-9_-]+/g, "_").slice(0, 48)
    return process.env.XDG_STATE_HOME + "/punk/inbox/opencode/" + slug + "-" + h + ".json"
  }
`

// TestOpenCodeMessagingRestoreGateBlocksDrainsUntilReady: st.registered
// flips true before the recovery restore's awaits complete, so an idle
// transition or inbox hint arriving during the restore window must NOT
// drain - a drain there would fetch without the restored pending ids and
// re-prompt a handed-off message. The gate folds those triggers into one
// queued drain; once the restore completes, the drain re-acks the restored
// id without ever prompting. PUNK_MESSAGING_RESTORE_DELAY_MS holds the
// window open deterministically.
func TestOpenCodeMessagingRestoreGateBlocksDrainsUntilReady(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	driver := `
  const fs = await import("node:fs")
  const D = "/tmp/punk-messaging-proj"
` + recoveryFileJSHelper + `
  // Server-shaped ids (region.newMessageID mints 32 lowercase hex): the
  // restore validates persisted ids against the real server syntax.
  const M0 = "a1" + "0".repeat(30)
  const file = recoveryFileFor("opencode:s1")
  fs.mkdirSync(file.slice(0, file.lastIndexOf("/")), { recursive: true })
  fs.writeFileSync(
    file,
    JSON.stringify({
      server: "http://punk.test",
      namespace: "agent-test-ns",
      address: "opencode:s1",
      pending_ids: [M0],
      wake_times: [],
      saved_at: Date.now(),
    }),
    { mode: 0o600 }
  )
  putMessage("opencode:s1", M0, "handed off in a previous life, never acked")

  const hooks = await PunkMemoryPlugin({ directory: D, client: punkTestClient })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })
  await until(
    () => punkTestServer.registeredAgents.some((r) => r.agent === "opencode:s1"),
    "session registered (restore window now open)"
  )
  await sleep(150)
  // Idle transition and inbox hint DURING the restore window: both must
  // defer - no prompt, no leased fetch, nothing acked.
  await hooks.event({ event: { type: "session.idle", properties: { sessionID: "s1" } } })
  emitHint("opencode:s1")
  await sleep(200) // still inside the 600ms window
  must(
    promptCalls.length === 0,
    "no drain may run before recovery state is restored, prompts=" + promptCalls.length
  )
  const leasedReads = fetchCalls.filter(
    (c) => c.method === "GET" && c.path === "/v1/namespaces/agent-test-ns/messages" && c.query.indexOf("lease_seconds=") >= 0
  )
  must(leasedReads.length === 0, "no leased fetch before the restore completes, reads=" + leasedReads.length)
  must(punkTestServer.ackedIds.length === 0, "nothing acked before the restore completes")

  // Window closed: the queued drain re-acks m0 without re-prompting.
  await until(() => punkTestServer.ackedIds.indexOf(M0) >= 0, "restored id re-acked after the gate opened", 6000)
  must(
    promptCalls.length === 0,
    "the restored handoff is re-acked, never re-prompted, prompts=" + promptCalls.length
  )
  must(
    punkTestServer.diagCalls.some((d) => d.state === "handoff_unconfirmed" && d.pending_ack_count >= 1),
    "the restart reported the honest handoff-unconfirmed observation"
  )

  console.log("PASS restore-gate-blocks-drains")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, map[string]string{
		"XDG_STATE_HOME":                  stateHome,
		"PUNK_MESSAGING_RESTORE_DELAY_MS": "600",
	}, driver)
	if !strings.Contains(out, "PASS restore-gate-blocks-drains") {
		t.Fatalf("driver did not report PASS restore-gate-blocks-drains:\n%s", out)
	}
}

// TestOpenCodeMessagingRecoveryModulesSharedAcrossSessions: two sessions
// registering in the same tick share ONE module-initialization promise -
// the second session must never observe a half-initialized module set
// (which silently skipped its restore, or persisted under a fallback-hash
// filename no later life would read). Both pre-written pending ids are
// restored and re-acked without a single prompt.
func TestOpenCodeMessagingRecoveryModulesSharedAcrossSessions(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	driver := `
  const fs = await import("node:fs")
  const D = "/tmp/punk-messaging-proj"
` + recoveryFileJSHelper + `
  // Server-shaped ids (32 lowercase hex): the restore validates persisted
  // ids against the real server syntax.
  const M1 = "b1" + "0".repeat(30)
  const M2 = "b2" + "0".repeat(30)
  for (const [sid, id] of [["s1", M1], ["s2", M2]]) {
    const file = recoveryFileFor("opencode:" + sid)
    fs.mkdirSync(file.slice(0, file.lastIndexOf("/")), { recursive: true })
    fs.writeFileSync(
      file,
      JSON.stringify({
        server: "http://punk.test",
        namespace: "agent-test-ns",
        address: "opencode:" + sid,
        pending_ids: [id],
        wake_times: [],
        saved_at: Date.now(),
      }),
      { mode: 0o600 }
    )
    putMessage("opencode:" + sid, id, "handed off before the restart, session " + sid)
  }

  const hooks = await PunkMemoryPlugin({ directory: D, client: punkTestClient })
  // Both sessions register and restore in the same tick, racing the
  // module initialization against each other.
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s2" } } } })

  await until(
    () => punkTestServer.ackedIds.indexOf(M1) >= 0 && punkTestServer.ackedIds.indexOf(M2) >= 0,
    "both sessions restored their pending ids through the shared init",
    6000
  )
  must(
    promptCalls.length === 0,
    "neither restored handoff was re-prompted, prompts=" + promptCalls.length
  )

  console.log("PASS recovery-modules-shared")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, map[string]string{"XDG_STATE_HOME": stateHome}, driver)
	if !strings.Contains(out, "PASS recovery-modules-shared") {
		t.Fatalf("driver did not report PASS recovery-modules-shared:\n%s", out)
	}
}

// TestOpenCodeMessagingDiagnosticsSerializeSlowFirstPost: reports are
// serialized per session with latest-only coalescing. A slow FIRST
// diagnostics POST must never COMPLETE after a newer observation: the
// server ends up holding the newest state, and no older completion lands
// behind a newer one. (Before serialization, the delayed first POST
// resolved last and left stale ready/no-wakes state over the newer
// ready/one-wake observation.)
func TestOpenCodeMessagingDiagnosticsSerializeSlowFirstPost(t *testing.T) {
	driver := `
  punkTestServer.diagDelayFirst = 400
  putMessage("opencode:s1", "m1", "serialize me")
  const hooks = await PunkMemoryPlugin({ directory: "/tmp/punk-messaging-proj", client: punkTestClient })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })
  await until(() => punkTestServer.diagStarted >= 1, "the first (registration) diagnostics POST is in flight")

  // Newer observations arrive while the old POST is still in flight:
  // busy, then idle, then a delivered message spends one wake.
  await hooks["chat.message"](
    { sessionID: "s1", messageID: "u1" },
    { message: { role: "user" }, parts: [{ type: "text", text: "busy now" }] }
  )
  await hooks.event({ event: { type: "session.idle", properties: { sessionID: "s1" } } })
  await until(() => punkTestServer.ackedIds.indexOf("m1") >= 0, "m1 delivered and acked")

  await until(
    () =>
      punkTestServer.diagOrder.some((d) => d.state === "ready" && d.wake_count === 1) &&
      punkTestServer.diagOrder.some((d) => d.state === "ready" && d.wake_count === 0),
    "the newest observation flushed AND the delayed first POST completed",
    6000
  )
  const last = punkTestServer.diagOrder[punkTestServer.diagOrder.length - 1]
  must(
    last.state === "ready" && last.wake_count === 1,
    "the server ends with the NEWEST observation, got " + JSON.stringify(last)
  )
  const newestAt = punkTestServer.diagOrder.findIndex((d) => d.state === "ready" && d.wake_count === 1)
  for (let i = newestAt + 1; i < punkTestServer.diagOrder.length; i++) {
    const d = punkTestServer.diagOrder[i]
    must(
      !(d.state === "ready" && d.wake_count === 0),
      "no older ready observation completed after the newer one, order=" +
        JSON.stringify(punkTestServer.diagOrder.map((x) => x.state + ":" + x.wake_count))
    )
  }
  must(
    punkTestServer.diagOrder.length <= 3,
    "bounded: at most the in-flight report plus coalesced newer ones, completions=" + punkTestServer.diagOrder.length
  )

  console.log("PASS diagnostics-serialize-slow-first-post")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, nil, driver)
	if !strings.Contains(out, "PASS diagnostics-serialize-slow-first-post") {
		t.Fatalf("driver did not report PASS diagnostics-serialize-slow-first-post:\n%s", out)
	}
}

// TestOpenCodeMessagingRecoveryRestoreRejectsOversizedFile: a recovery
// file above the byte bound is untrusted input - the restore rejects it
// wholesale (nothing restored, nothing thrown) and delivery proceeds
// fail-open: the message is prompted FRESH (no restored id to re-ack) and
// acked, and the next save rewrites the oversized file into a clean,
// bounded record.
func TestOpenCodeMessagingRecoveryRestoreRejectsOversizedFile(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	driver := `
  const fs = await import("node:fs")
  const D = "/tmp/punk-messaging-proj"
` + recoveryFileJSHelper + `
  const file = recoveryFileFor("opencode:s1")
  fs.mkdirSync(file.slice(0, file.lastIndexOf("/")), { recursive: true })
  // Valid identity and a real pending id, padded far past the byte bound
  // with wake stamps so the file itself is oversized.
  const wakePad = []
  for (let i = 0; i < 6000; i++) wakePad.push(Date.now() - 5000)
  fs.writeFileSync(
    file,
    JSON.stringify({
      server: "http://punk.test",
      namespace: "agent-test-ns",
      address: "opencode:s1",
      pending_ids: ["m0"],
      wake_times: wakePad,
      saved_at: Date.now(),
    }),
    { mode: 0o600 }
  )
  must(fs.statSync(file).size > 65536, "test precondition: the file is oversized")
  putMessage("opencode:s1", "m0", "a previous life handed this off, but the state file is junk")

  const hooks = await PunkMemoryPlugin({ directory: D, client: punkTestClient })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })
  // Fail-open: the oversized record restored nothing, so the message is
  // delivered FRESH (one prompt) rather than re-acked.
  await until(() => promptCalls.length === 1, "oversized state fails open: the message was delivered fresh")
  await until(() => punkTestServer.ackedIds.indexOf("m0") >= 0, "and acked normally")

  // The next save heals the file into a clean, bounded record.
  await sleep(200)
  const healed = JSON.parse(fs.readFileSync(file, "utf8"))
  must(Array.isArray(healed.pending_ids), "a well-formed record replaced the oversized one")
  must(fs.statSync(file).size <= 65536, "the rewritten file is within the byte bound")

  console.log("PASS recovery-oversized-file-rejected")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, map[string]string{"XDG_STATE_HOME": stateHome}, driver)
	if !strings.Contains(out, "PASS recovery-oversized-file-rejected") {
		t.Fatalf("driver did not report PASS recovery-oversized-file-rejected:\n%s", out)
	}
}

// TestOpenCodeMessagingRecoveryRestoreBoundsIdsAndWakes: within the byte
// bound, the restore still validates every field - over-long or non-string
// pending ids are ignored (the server's own 256-byte id bound), the wake
// array is capped at the explicit stamp maximum with the NEWEST kept, and
// nothing invalid ever reaches an ACK body.
func TestOpenCodeMessagingRecoveryRestoreBoundsIdsAndWakes(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	driver := `
  const fs = await import("node:fs")
  const D = "/tmp/punk-messaging-proj"
` + recoveryFileJSHelper + `
  // Server-shaped id: the restore validates persisted ids against the
  // real server syntax (32 lowercase hex).
  const M0 = "a1" + "0".repeat(30)
  const file = recoveryFileFor("opencode:s1")
  fs.mkdirSync(file.slice(0, file.lastIndexOf("/")), { recursive: true })
  const wakeStamps = []
  for (let i = 0; i < 2000; i++) wakeStamps.push(Date.now() - 5000) // all in-window
  fs.writeFileSync(
    file,
    JSON.stringify({
      server: "http://punk.test",
      namespace: "agent-test-ns",
      address: "opencode:s1",
      pending_ids: [
        M0,
        "junk-" + "x".repeat(300),
        42,
        "",
        "z".repeat(32), // 32 chars but not hex: not server syntax
        "a".repeat(31), // wrong length
        "A".repeat(32), // uppercase: not server syntax
      ],
      wake_times: wakeStamps,
      saved_at: Date.now(),
    }),
    { mode: 0o600 }
  )
  must(fs.statSync(file).size <= 65536, "test precondition: within the byte bound")
  putMessage("opencode:s1", M0, "restorable handoff from a previous life")

  const hooks = await PunkMemoryPlugin({ directory: D, client: punkTestClient })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })
  await until(() => punkTestServer.ackedIds.indexOf(M0) >= 0, "the valid pending id was restored and re-acked", 6000)
  must(promptCalls.length === 0, "the restored handoff was never re-prompted, prompts=" + promptCalls.length)

  // Junk ids never reached an ACK body.
  const ackBodies = fetchCalls.filter((c) => c.path === "/v1/namespaces/agent-test-ns/messages/ack").map((c) => c.body)
  must(
    ackBodies.every((b) => b.indexOf("junk-") < 0 && b.indexOf("zzzz") < 0),
    "over-long or malformed ids never reached an ACK body"
  )

  // The next save writes the bounded window: at most the stamp maximum.
  await sleep(200)
  const rec = JSON.parse(fs.readFileSync(file, "utf8"))
  must(
    rec.wake_times.length <= 1000,
    "restored wake stamps are capped at the explicit maximum, got " + rec.wake_times.length
  )
  must(rec.pending_ids.indexOf(M0) < 0, "the confirmed ACK drained the restored id")
  must(
    rec.pending_ids.every((id) => /^[0-9a-f]{32}$/.test(id)),
    "only server-syntax ids persist back, got " + JSON.stringify(rec.pending_ids)
  )

  console.log("PASS recovery-restore-bounds")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, map[string]string{"XDG_STATE_HOME": stateHome}, driver)
	if !strings.Contains(out, "PASS recovery-restore-bounds") {
		t.Fatalf("driver did not report PASS recovery-restore-bounds:\n%s", out)
	}
}

// TestOpenCodeMessagingDiagnosticsUnknownBusyReportsWaiting: a restored
// session whose status snapshot FAILED binds with UNKNOWN busy state -
// its registration diagnostic must report waiting_for_idle, never ready
// (an unconfirmed snapshot never claims readiness).
func TestOpenCodeMessagingDiagnosticsUnknownBusyReportsWaiting(t *testing.T) {
	driver := `
  punkTestServer.statusFails = true
  punkTestServer.sessionList = [{ id: "unk-1", directory: "/tmp/punk-messaging-proj" }]
  putMessage("opencode:unk-1", "mu", "deferred until an authoritative idle")

  const hooks = await PunkMemoryPlugin({ directory: "/tmp/punk-messaging-proj", client: punkTestClient })
  await until(
    () => punkTestServer.registeredAgents.some((r) => r.agent === "opencode:unk-1"),
    "restored session registered despite the failed status snapshot"
  )
  await until(() => punkTestServer.diagCalls.length >= 1, "the registration diagnostic was posted")
  const first = punkTestServer.diagCalls[0]
  must(
    first.state === "waiting_for_idle",
    "unknown busy state reports waiting_for_idle, never ready, got " + first.state
  )
  await sleep(250)
  must(promptCalls.length === 0, "unknown busy state still defers delivery")

  console.log("PASS diagnostics-unknown-busy-waiting")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, nil, driver)
	if !strings.Contains(out, "PASS diagnostics-unknown-busy-waiting") {
		t.Fatalf("driver did not report PASS diagnostics-unknown-busy-waiting:\n%s", out)
	}
}

// TestOpenCodeMessagingRecoveryIdentityMismatchNotMerged: a foreign record
// whose identity fields do not match this session (filename collision or
// stale rename) is never merged - its pending ids are not adopted, its
// wake evidence is not unioned - while this process's own snapshot still
// persists normally.
func TestOpenCodeMessagingRecoveryIdentityMismatchNotMerged(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	driver := `
  const fs = await import("node:fs")
  const D = "/tmp/punk-messaging-proj"
` + recoveryFileJSHelper + `
  putMessage("opencode:s1", "m1", "first delivery")
  const hooks = await PunkMemoryPlugin({ directory: D, client: punkTestClient })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })
  await until(() => punkTestServer.ackedIds.indexOf("m1") >= 0, "m1 delivered and acked")
  await sleep(150)

  const file = recoveryFileFor("opencode:s1")
  const lock = file + ".lock"
  async function withLock(fn) {
    const deadline = Date.now() + 5000
    for (;;) {
      try {
        const fh = await fs.promises.open(lock, "wx")
        await fh.close()
        break
      } catch (err) {}
      if (Date.now() > deadline) throw new Error("could not take the state lock")
      await sleep(25)
    }
    try {
      await fn()
    } finally {
      await fs.promises.unlink(lock)
    }
  }
  // A record claiming a DIFFERENT identity (another address) lands in this
  // session's file - a hash collision or a stale rename away from it.
  const foreignWake = Date.now() - 1000
  await withLock(async () => {
    fs.writeFileSync(
      file,
      JSON.stringify({
        server: "http://punk.test",
        namespace: "agent-test-ns",
        address: "opencode:s2",
        pending_ids: ["aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"],
        wake_times: [foreignWake],
        saved_at: Date.now() - 10,
      }) + "\n"
    )
  })

  // Our next handoff persists normally, and the mismatched record is
  // neither adopted nor unioned.
  putMessage("opencode:s1", "m2", "delivered over a mismatched on-disk record")
  emitHint("opencode:s1")
  await until(() => punkTestServer.ackedIds.indexOf("m2") >= 0, "m2 delivered and acked")
  await until(() => {
    const r = JSON.parse(fs.readFileSync(file, "utf8"))
    return r.address === "opencode:s1"
  }, "our own snapshot was persisted over the mismatched record", 5000)
  const rec = JSON.parse(fs.readFileSync(file, "utf8"))
  must(
    rec.pending_ids.every((id) => id !== "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" && id !== "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
    "the mismatched record's pending ids were never adopted, got " + JSON.stringify(rec.pending_ids)
  )
  must(
    rec.wake_times.every((t) => t !== foreignWake),
    "the mismatched record's wake evidence was never unioned"
  )
  must(
    promptCalls.filter((p) => p.text.indexOf("m2") >= 0).length === 1 &&
      promptCalls.filter((p) => p.text.indexOf("aaaa") >= 0).length === 0,
    "the mismatched ids were never prompted"
  )

  console.log("PASS recovery-identity-mismatch-not-merged")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, map[string]string{"XDG_STATE_HOME": stateHome}, driver)
	if !strings.Contains(out, "PASS recovery-identity-mismatch-not-merged") {
		t.Fatalf("driver did not report PASS recovery-identity-mismatch-not-merged:\n%s", out)
	}
}

// TestOpenCodeMessagingRecoveryTempCleanupOnRenameFail: when the atomic
// rename fails (the state path is occupied by a directory), the write
// fails open - delivery and ACK continue in memory - and the orphaned
// temp file is cleaned up instead of lingering next to the state file.
func TestOpenCodeMessagingRecoveryTempCleanupOnRenameFail(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	driver := `
  const fs = await import("node:fs")
  const D = "/tmp/punk-messaging-proj"
` + recoveryFileJSHelper + `
  putMessage("opencode:s1", "m1", "first delivery")
  const hooks = await PunkMemoryPlugin({ directory: D, client: punkTestClient })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })
  await until(() => punkTestServer.ackedIds.indexOf("m1") >= 0, "m1 delivered and acked")
  await sleep(150)

  // Replace the state file with a DIRECTORY: writeFile(tmp) succeeds (a
  // sibling), rename(tmp, dir) fails, so the temp-cleanup path runs.
  const file = recoveryFileFor("opencode:s1")
  const lock = file + ".lock"
  const fh = await fs.promises.open(lock, "wx")
  await fh.close()
  fs.rmSync(file)
  fs.mkdirSync(file)
  await fs.promises.unlink(lock)

  putMessage("opencode:s1", "m2", "delivered despite the failed rename")
  emitHint("opencode:s1")
  await until(() => punkTestServer.ackedIds.indexOf("m2") >= 0, "failed rename fails open: m2 delivered and acked")
  await sleep(250)

  const stateDir = process.env.XDG_STATE_HOME + "/punk/inbox/opencode"
  const leftovers = fs.readdirSync(stateDir).filter((n) => n.indexOf(".tmp-") >= 0)
  must(
    leftovers.length === 0,
    "a failed rename must not leave temp files behind, found " + JSON.stringify(leftovers)
  )

  console.log("PASS recovery-temp-cleanup-on-rename-fail")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, map[string]string{"XDG_STATE_HOME": stateHome}, driver)
	if !strings.Contains(out, "PASS recovery-temp-cleanup-on-rename-fail") {
		t.Fatalf("driver did not report PASS recovery-temp-cleanup-on-rename-fail:\n%s", out)
	}
	if !strings.Contains(out, "recovery state write failed; continuing in memory") {
		t.Fatalf("expected the generic write-failure log line:\n%s", out)
	}
}

// TestOpenCodeMessagingRecoverySaveBoundsWakesWithOverflow: with a
// configured wake cap above the persisted stamp bound, the save writes at
// most the bound in stamps plus a conservative overflow summary
// {count, until}; restore turns that summary back into stamps expiring at
// the documented last expiry, so the high-cap budget survives a restart
// and the file never grows past the byte bound the restore itself
// enforces.
func TestOpenCodeMessagingRecoverySaveBoundsWakesWithOverflow(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	driver := `
  const fs = await import("node:fs")
  const D = "/tmp/punk-messaging-proj"
` + recoveryFileJSHelper + `
  const file = recoveryFileFor("opencode:s1")
  fs.mkdirSync(file.slice(0, file.lastIndexOf("/")), { recursive: true })
  // A previous life's record at the stamp bound with live overflow.
  // (overflowUntil, not until: the harness until(cond, label) helper is
  // shadowed otherwise.)
  const overflowUntil = Date.now() + 8000
  const stamps = []
  for (let i = 0; i < 999; i++) stamps.push(Date.now() - 1000)
  fs.writeFileSync(
    file,
    JSON.stringify({
      server: "http://punk.test",
      namespace: "agent-test-ns",
      address: "opencode:s1",
      pending_ids: [],
      wake_times: stamps,
      wake_overflow: { count: 500, until: overflowUntil },
      saved_at: Date.now(),
    }),
    { mode: 0o600 }
  )

  putMessage("opencode:s1", "m1", "one more wake on top of the restored budget")
  const hooks = await PunkMemoryPlugin({ directory: D, client: punkTestClient })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })
  await until(() => punkTestServer.ackedIds.indexOf("m1") >= 0, "m1 delivered and acked (cap 2000 admits it)")
  await sleep(200)

  const rec = JSON.parse(fs.readFileSync(file, "utf8"))
  must(rec.wake_times.length <= 1000, "persisted stamps stay within the bound, got " + rec.wake_times.length)
  must(
    rec.wake_overflow && rec.wake_overflow.count === 500,
    "the overflow summary conserves the stamps beyond the bound, got " + JSON.stringify(rec.wake_overflow)
  )
  must(
    rec.wake_overflow.until === overflowUntil,
    "the overflow expiry round-trips exactly (every dropped stamp has expired by it), got " + rec.wake_overflow.until
  )
  must(fs.statSync(file).size <= 65536, "the bounded record stays far inside the restore's own file bound")

  console.log("PASS recovery-save-bounds-wakes-overflow")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, map[string]string{
		"XDG_STATE_HOME":              stateHome,
		"PUNK_MESSAGING_MAX_CONTINUE": "2000",
	}, driver)
	if !strings.Contains(out, "PASS recovery-save-bounds-wakes-overflow") {
		t.Fatalf("driver did not report PASS recovery-save-bounds-wakes-overflow:\n%s", out)
	}
}

// TestOpenCodeMessagingRestoreDelayCancellableOnDispose: the test-only
// restore pacing delay rides the shared cancellable sleep - a plugin
// disposed mid-restore leaves NO lingering timer behind (observed through
// node's active-resource info: every punk-owned Timeout is gone well
// before the 800ms delay would have elapsed on its own).
func TestOpenCodeMessagingRestoreDelayCancellableOnDispose(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	driver := `
  const timeoutCount = () => process.getActiveResourcesInfo().filter((r) => r === "Timeout").length
  const base = timeoutCount()
  const hooks = await PunkMemoryPlugin({ directory: "/tmp/punk-messaging-proj", client: punkTestClient })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })
  await until(
    () => punkTestServer.registeredAgents.some((r) => r.agent === "opencode:s1"),
    "session registered (the 800ms restore window is open)"
  )
  await sleep(100) // safely inside the restore window
  await hooks.dispose()
  // Every punk-owned timer must be cancelled by dispose well before the
  // 800ms delay would elapse on its own.
  let settled = false
  const deadline = Date.now() + 300
  while (Date.now() < deadline) {
    if (timeoutCount() <= base) {
      settled = true
      break
    }
    await sleep(20)
  }
  must(
    settled,
    "dispose must cancel the in-flight restore delay instead of leaving the timer to run out, timeouts=" +
      timeoutCount() +
      " (base " + base + ")"
  )
  must(promptCalls.length === 0, "nothing was delivered")
  must(punkTestServer.sseFetches === 0, "no SSE stream was ever started")

  console.log("PASS restore-delay-cancellable")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, map[string]string{
		"XDG_STATE_HOME":                  stateHome,
		"PUNK_MESSAGING_RESTORE_DELAY_MS": "800",
	}, driver)
	if !strings.Contains(out, "PASS restore-delay-cancellable") {
		t.Fatalf("driver did not report PASS restore-delay-cancellable:\n%s", out)
	}
}

// TestOpenCodeMessagingRecoveryMergeKeepsForeignWakeMultiplicity: the
// conservative wake-evidence merge takes, per identical timestamp, the MAX
// of the two records' multiplicities - a foreign record carrying three
// same-millisecond wakes keeps all three (never collapsed to 1), and a
// timestamp present on both sides (shared restored lineage) is counted
// once per side's multiplicity (max, never the sum).
func TestOpenCodeMessagingRecoveryMergeKeepsForeignWakeMultiplicity(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	driver := `
  const fs = await import("node:fs")
  const D = "/tmp/punk-messaging-proj"
` + recoveryFileJSHelper + `
  putMessage("opencode:s1", "m1", "first delivery spends one wake")
  const hooks = await PunkMemoryPlugin({ directory: D, client: punkTestClient })
  await hooks.event({ event: { type: "session.created", properties: { info: { id: "s1" } } } })
  await until(() => punkTestServer.ackedIds.indexOf("m1") >= 0, "m1 delivered and acked")
  await sleep(150)

  const file = recoveryFileFor("opencode:s1")
  const ours = JSON.parse(fs.readFileSync(file, "utf8"))
  must(ours.wake_times.length === 1, "one own wake stamp so far, got " + JSON.stringify(ours.wake_times))
  const ownStamp = ours.wake_times[0]

  // A foreign cooperating record (under the same lock convention) at or
  // below our clock: three wakes at one identical millisecond T, plus the
  // timestamp we already carry twice (shared lineage evidence).
  const T = Date.now() - 2000
  const lock = file + ".lock"
  const fh = await fs.promises.open(lock, "wx")
  await fh.close()
  fs.writeFileSync(
    file,
    JSON.stringify({
      server: "http://punk.test",
      namespace: "agent-test-ns",
      address: "opencode:s1",
      pending_ids: [],
      wake_times: [T, T, T, ownStamp, ownStamp],
      saved_at: Date.now() - 10,
    }) + "\n"
  )
  await fs.promises.unlink(lock)

  // Our next handoff merges the foreign wake evidence and persists it.
  putMessage("opencode:s1", "m2", "second delivery triggers the merge")
  emitHint("opencode:s1")
  await until(() => punkTestServer.ackedIds.indexOf("m2") >= 0, "m2 delivered and acked")
  await sleep(200)

  const rec = JSON.parse(fs.readFileSync(file, "utf8"))
  const countT = rec.wake_times.filter((t) => t === T).length
  const countOwn = rec.wake_times.filter((t) => t === ownStamp).length
  must(
    countT === 3,
    "three foreign same-millisecond wakes keep their count (max, not 1), got " + countT
  )
  must(
    countOwn === 2,
    "a timestamp on both sides takes the max multiplicity (2), never the sum (3), got " + countOwn
  )
  must(rec.wake_times.length >= 4, "our own evidence is retained alongside the merged foreign evidence")
  must(promptCalls.length === 2, "exactly the two fresh messages were prompted, prompts=" + promptCalls.length)

  console.log("PASS recovery-merge-foreign-wake-multiplicity")
  process.exit(0)
`
	out := runOpenCodeMessagingHarness(t, map[string]string{"XDG_STATE_HOME": stateHome}, driver)
	if !strings.Contains(out, "PASS recovery-merge-foreign-wake-multiplicity") {
		t.Fatalf("driver did not report PASS recovery-merge-foreign-wake-multiplicity:\n%s", out)
	}
}
