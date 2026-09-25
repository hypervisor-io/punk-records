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

// piMessagingHarnessPrelude is the shared JS prelude appended (after the
// real rendered extension source, with its default export rewritten to a
// plain named declaration so the driver can call it) to every Pi
// messaging-bridge behavioral harness: a lease-aware fake punk-records
// HTTP surface implementing the M5/M10 contract (member registration with
// injectable outages, leased reads where a row leased to one owner is
// invisible to another owner inside the lease, owner-recorded ACKs with
// injectable failures, owner-scoped release, and the addressed SSE inbox
// stream with injectable non-OK / stalled-connect / stalled-stream
// responses) plus a fake pi ExtensionAPI whose on/sendMessage/registerTool
// record every call. Scenarios drive the extension's real handlers and
// assert on what the fake server/API observed.
//
// The verified contract this harness fakes (2026-09-25,
// /research/extension-clients/pi in the punk-agent-messaging namespace):
// pi.sendMessage(message, options) where message is
// Pick<CustomMessage, "customType"|"content"|"display"|"details"> and
// options are {triggerTurn?, deliverAs?: "steer"|"followUp"|"nextTurn"};
// the call is synchronous and void (host handoff, no completion signal);
// events session_start/turn_start/turn_end/agent_settled/session_shutdown
// and before_agent_start's {message, systemPrompt} return shape;
// ctx.sessionManager.getSessionId() and ctx.isIdle().
const piMessagingHarnessPrelude = `
// ---- punk-records pi messaging test harness: fake server + fake pi ----
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))

const fetchCalls = []
const sendMessageCalls = []
const punkHandlers = {}
let sendMessagePlan = [] // per-call outcomes ("ok" | "throw"), shifted
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
  messageFailNext: 0, // one-shot: answer the next leased read with 500
  releaseCalls: [], // {id, owner, mismatch}
  memberAttempts: 0,
  memberFailNext: 0,
  registeredAgents: [],
  sseFetches: 0,
  sseTimes: [],
  sseAborted: [],
  sseBodiesCancelled: [],
  sseCancelled: [],
  sseNonOK: 0,
  sseStallConnect: 0,
  sseStallStream: 0,
  emitHintFor: {},
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
      // actually RETURNED (after the limit) are leased - the real server
      // leases the LIMIT'd query rows, not the whole backlog.
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

// Fake pi ExtensionAPI: the verified surface the bridge touches.
const fakePi = {
  on(name, handler) {
    punkHandlers[name] = handler
    return () => delete punkHandlers[name]
  },
  sendMessage(message, options) {
    sendMessageCalls.push({ msg: message, opts: options })
    const outcome = sendMessagePlan.length > 0 ? sendMessagePlan.shift() : "ok"
    if (outcome === "throw") throw new Error("simulated sendMessage failure")
  },
  registerTool() {},
}

function makeCtx(sid, opts) {
  opts = opts || {}
  const ctx = { cwd: "/tmp/punk-pi-proj" }
  if (!opts.noSessionManager) {
    ctx.sessionManager = { getSessionId: () => sid }
  }
  if (!opts.noIdle) {
    ctx.isIdle = () => (opts.idle === undefined ? true : opts.idle)
  }
  return ctx
}

async function fire(name, event, ctx) {
  if (!punkHandlers[name]) throw new Error("no handler wired for " + name)
  return punkHandlers[name](event || {}, ctx)
}

function emitHint(agent) {
  if (punkTestServer.emitHintFor[agent]) punkTestServer.emitHintFor[agent]()
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

// consumeElsewhere simulates another consumer taking the row: leased
// and ACKed server-side, so it disappears from this owner's view.
function consumeElsewhere(agent, id) {
  punkTestServer.inbox[agent] = (punkTestServer.inbox[agent] || []).filter((m) => m.id !== id)
}

function acked(id) {
  return punkTestServer.ackedIds.some((a) => a.id === id)
}

function inboxCount(agent) {
  return (punkTestServer.inbox[agent] || []).length
}

function debugState() {
  return JSON.stringify({
    fetchCalls: fetchCalls.map((c) => c.method + " " + c.path + c.query),
    sendMessageCalls: sendMessageCalls.map((c) => ({ customType: c.msg && c.msg.customType, opts: c.opts })),
    registered: punkTestServer.registeredAgents,
    memberAttempts: punkTestServer.memberAttempts,
    ackedIds: punkTestServer.ackedIds,
    ackCalls: punkTestServer.ackCalls,
    releaseCalls: punkTestServer.releaseCalls,
    sseFetches: punkTestServer.sseFetches,
    sseAborted: punkTestServer.sseAborted,
    sseBodiesCancelled: punkTestServer.sseBodiesCancelled,
    sseCancelled: punkTestServer.sseCancelled,
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

const piMessagingHarnessEpilogue = `}
main().catch((err) => {
  console.error("FAIL: driver rejected: " + (err && err.stack ? err.stack : err))
  process.exit(1)
})
`

// runPiMessagingHarness renders the REAL extension via ConnectPi (never a
// paraphrase), rewrites its named default export into a plain declaration,
// appends the shared prelude + the scenario driver + the epilogue into one
// .mjs file, and executes it under node with the messaging bridge enabled.
// env adds or overrides environment variables (PUNK_MESSAGING and friends
// are set here, not inside the driver, so the extension reads them exactly
// the way a real pi process would). Skipped when node is not on PATH; the
// 25s hard deadline bounds a wedged driver instead of hanging go test.
func runPiMessagingHarness(t *testing.T, env map[string]string, driver string) string {
	t.Helper()
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping pi messaging behavioral test")
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
	extPath := filepath.Join(dir, "punk-memory.ts")
	if _, err := ConnectPi(extPath, "http://punk.test", PiOpts{}); err != nil {
		t.Fatal(err)
	}
	extSrc, err := os.ReadFile(extPath)
	if err != nil {
		t.Fatal(err)
	}
	const exportDecl = "export default function punkPiExtension(pi) {"
	rewritten := strings.Replace(string(extSrc), exportDecl, "function punkPiExtension(pi) {", 1)
	if rewritten == string(extSrc) {
		t.Fatal("expected to find and rewrite the extension's default export declaration")
	}

	harness := rewritten + piMessagingHarnessPrelude + driver + piMessagingHarnessEpilogue
	harnessPath := extPath + ".messaging-harness.mjs"
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

// expectedPiEnvelope renders the exact envelope the bridge must deliver
// for one message, through the same Go renderer punk hook inbox uses.
func expectedPiEnvelope(address, id, sender, body string) string {
	return RenderInbox("agent-test-ns", address, []InboxMessage{{
		ID: id, Sender: sender, Recipient: address, Body: body, CreatedAt: "2026-09-25T00:00:00Z",
	}})
}

// TestPiMessagingIdleWake: a message already queued for a freshly bound
// idle session is delivered through the VERIFIED object form of
// pi.sendMessage ({customType, content, display, details} plus
// {deliverAs:"followUp", triggerTurn:true}), the content is byte-identical
// to the M5 envelope (hookcli.RenderInbox), the ACK carries the same lease
// owner as the fetch, and keepalives never duplicate delivery.
func TestPiMessagingIdleWake(t *testing.T) {
	driver := fmt.Sprintf(`
  putMessage("pi:s1", "m1", "please review the flaky test")
  punkPiExtension(fakePi)
  await fire("session_start", { reason: "new" }, makeCtx("s1", { idle: true }))

  await until(() => sendMessageCalls.length === 1, "message enqueued to the idle session")
  await until(() => acked("m1"), "message m1 acked after the enqueue call")

  const call = sendMessageCalls[0]
  must(call.opts && call.opts.deliverAs === "followUp" && call.opts.triggerTurn === true, "options are deliverAs followUp + triggerTurn true")
  must(call.msg && call.msg.customType === "punk-inbox", "sendMessage takes the object message form, got " + JSON.stringify(call.msg && call.msg.customType))
  must(call.msg && typeof call.msg.content === "string", "content is a string (CustomMessage.content accepts a plain string)")
  must(call.msg && call.msg.display === true, "display flag set so the delivery renders in the TUI")
  must(call.msg.content === %s, "delivered content is byte-identical to the shared M5 envelope")

  must(punkTestServer.registeredAgents.some((r) => r.agent === "pi:s1"), "session registered under the pi:<session_id> identity")
  const memberCall = fetchCalls.find((c) => c.path === "/v1/namespaces/agent-test-ns/members")
  must(memberCall && memberCall.method === "POST", "member registration is a POST to the namespace members endpoint")

  const readCall = fetchCalls.find((c) => c.path === "/v1/namespaces/agent-test-ns/messages" && c.query.indexOf("lease_seconds=") >= 0)
  must(readCall, "the inbox fetch carries the M10 lease (lease_seconds)")
  must(readCall.query.indexOf("leased_by=pi-ext-") >= 0, "the inbox fetch carries the bridge lease owner")
  must(punkTestServer.ackedIds.some((a) => a.id === "m1" && a.owner && a.owner.indexOf("pi-ext-") === 0), "the ACK carries the same lease owner as the fetch")

  await until(() => punkTestServer.sseFetches >= 1, "SSE stream connected after confirmed registration")

  // The stream stays open sending keepalives; nothing may duplicate.
  await sleep(200)
  must(sendMessageCalls.length === 1, "keepalives and the open stream produce no duplicate delivery, calls=" + sendMessageCalls.length)

  console.log("PASS idle-wake")
  process.exit(0)
`, jsStringLiteral(expectedPiEnvelope("pi:s1", "m1", "planner-agent", "please review the flaky test")))
	out := runPiMessagingHarness(t, nil, driver)
	if !strings.Contains(out, "PASS idle-wake") {
		t.Fatalf("driver did not report PASS idle-wake:\n%s", out)
	}
}

// TestPiMessagingBusyDefer: a turn in flight (turn_start) marks the
// session busy; an inbox hint while busy triggers NO enqueue and NO ACK;
// the authoritative agent_settled marks idle and flushes the delivery.
// agent_settled (not agent_end) is the idle marker per the verified docs.
func TestPiMessagingBusyDefer(t *testing.T) {
	driver := `
  punkPiExtension(fakePi)
  await fire("session_start", { reason: "new" }, makeCtx("s1", { idle: true }))
  await until(() => punkTestServer.registeredAgents.some((r) => r.agent === "pi:s1"), "session registered")

  await fire("turn_start", { turnIndex: 0 }, makeCtx("s1", { idle: false }))
  putMessage("pi:s1", "m-busy", "urgent question while you work")
  emitHint("pi:s1")

  await sleep(250)
  must(sendMessageCalls.length === 0, "busy session must not be enqueued, calls=" + sendMessageCalls.length)
  must(punkTestServer.ackedIds.length === 0, "busy session must not ack, acked=" + JSON.stringify(punkTestServer.ackedIds))

  await fire("agent_settled", {}, makeCtx("s1", { idle: true }))
  await until(() => sendMessageCalls.length === 1, "deferred message enqueued after agent_settled")
  await until(() => acked("m-busy"), "deferred message acked after delivery")
  must(sendMessageCalls[0].msg.content.indexOf("urgent question while you work") >= 0, "delivered the queued message body")

  console.log("PASS busy-defer")
  process.exit(0)
`
	out := runPiMessagingHarness(t, nil, driver)
	if !strings.Contains(out, "PASS busy-defer") {
		t.Fatalf("driver did not report PASS busy-defer:\n%s", out)
	}
}

// TestPiMessagingUnknownDefers: a host ctx without isIdle (or one whose
// isIdle call fails) leaves the busy state UNKNOWN at bind time - the
// bridge defers delivery on hints rather than guessing idle, until an
// authoritative agent_settled flushes it.
func TestPiMessagingUnknownDefers(t *testing.T) {
	driver := `
  punkPiExtension(fakePi)
  await fire("session_start", { reason: "resume" }, makeCtx("s1", { noIdle: true }))
  await until(() => punkTestServer.registeredAgents.some((r) => r.agent === "pi:s1"), "session registered")

  putMessage("pi:s1", "m-unk", "message while busy state is unknown")
  emitHint("pi:s1")
  await sleep(250)
  must(sendMessageCalls.length === 0, "unknown busy state must defer delivery, calls=" + sendMessageCalls.length)
  must(punkTestServer.ackedIds.length === 0, "nothing acked while deferred")

  await fire("agent_settled", {}, makeCtx("s1", { idle: true }))
  await until(() => sendMessageCalls.length === 1, "delivered after the authoritative settle")
  await until(() => acked("m-unk"), "acked after delivery")

  console.log("PASS unknown-defers")
  process.exit(0)
`
	out := runPiMessagingHarness(t, nil, driver)
	if !strings.Contains(out, "PASS unknown-defers") {
		t.Fatalf("driver did not report PASS unknown-defers:\n%s", out)
	}
}

// TestPiMessagingCatchUpInjection: when a user turn is already starting,
// the unread set is injected INTO that turn through before_agent_start's
// documented {message} return (no wake is enqueued), combined with the
// once-per-session memory systemPrompt injection. The injection's ACK is
// deferred: nothing is acked at injection time, and the next observed
// fetch (agent_settled's drain) re-acks the id without ever enqueueing a
// separate turn for it.
func TestPiMessagingCatchUpInjection(t *testing.T) {
	driver := fmt.Sprintf(`
  punkTestServer.memoryContext = "## Project memory\n- rule one"
  putMessage("pi:s1", "m1", "catch me up please")
  punkPiExtension(fakePi)

  // Busy from the start (a run is active), so the wake path defers and
  // the inbox can only ride the turn that is about to start.
  await fire("session_start", { reason: "resume" }, makeCtx("s1", { idle: false }))
  await until(() => punkTestServer.registeredAgents.some((r) => r.agent === "pi:s1"), "session registered")
  await sleep(150)
  must(sendMessageCalls.length === 0, "busy session was not woken, calls=" + sendMessageCalls.length)

  const res = await fire("before_agent_start", { prompt: "do the work", systemPrompt: "BASE PROMPT" }, makeCtx("s1", { idle: false }))
  must(res && res.systemPrompt === "BASE PROMPT\n\n## Project memory\n- rule one", "memory context still appended once per session, got " + JSON.stringify(res && res.systemPrompt))
  must(res && res.message && res.message.customType === "punk-inbox", "inbox rides the turn as the before_agent_start message return")
  must(res && res.message && res.message.content === %s, "injected content is byte-identical to the shared M5 envelope")
  must(sendMessageCalls.length === 0, "in-turn catch-up never enqueues a wake, calls=" + sendMessageCalls.length)
  must(!acked("m1"), "nothing is acked at injection time (the ACK rides the next observed fetch)")

  // The turn runs and settles. The injected id is pending, NOT acked:
  // its ACK rides the next observed fetch, and the real server hides a
  // live-leased row from every reader (this owner included) until the
  // lease expires - so the settle drain here sees nothing yet.
  await fire("agent_settled", {}, makeCtx("s1", { idle: true }))
  await sleep(150)
  must(!acked("m1"), "no ACK before an observed fetch sees the row again (deferred host handoff)")

  // A later quiet turn inside the lease window: still hidden, nothing
  // re-delivered, no memory re-fetch.
  const res2 = await fire("before_agent_start", { prompt: "next turn", systemPrompt: "BASE PROMPT" }, makeCtx("s1", { idle: false }))
  must(res2 === undefined, "a quiet later turn returns undefined, got " + JSON.stringify(res2))
  must(punkTestServer.contextFetches === 1, "memory context stays once-per-session across turns")

  // After the (1s test) lease expires, the next observed fetch re-leases
  // the row, re-acks it silently, and never prompts the model for it.
  await fire("agent_settled", {}, makeCtx("s1", { idle: true }))
  await sleep(1100)
  const res3 = await fire("before_agent_start", { prompt: "turn three", systemPrompt: "BASE PROMPT" }, makeCtx("s1", { idle: false }))
  must(res3 === undefined, "the re-ack turn injects nothing new, got " + JSON.stringify(res3))
  await until(() => acked("m1"), "injected id re-acked by the first observed fetch after lease expiry")
  must(sendMessageCalls.length === 0, "the re-ack never enqueues another turn, calls=" + sendMessageCalls.length)
  must(punkTestServer.contextFetches === 1, "memory context stays once-per-session across all turns")

  console.log("PASS catch-up-injection")
  process.exit(0)
`, jsStringLiteral(expectedPiEnvelope("pi:s1", "m1", "planner-agent", "catch me up please")))
	out := runPiMessagingHarness(t, nil, driver)
	if !strings.Contains(out, "PASS catch-up-injection") {
		t.Fatalf("driver did not report PASS catch-up-injection:\n%s", out)
	}
}

// TestPiMessagingLeaseExclusivityAndAckRetry (M10 lease): while a row is
// leased to the bridge's owner, a DIFFERENT consumer's leased read sees
// nothing (two overlapping consumers never double-deliver), the same
// owner can always re-read, and a failed ACK is retried by the next drain
// WITHOUT enqueueing the message again.
func TestPiMessagingLeaseExclusivityAndAckRetry(t *testing.T) {
	driver := `
  punkTestServer.ackFailNext = 1
  putMessage("pi:s1", "m1", "leased while ack fails once")
  punkPiExtension(fakePi)
  await fire("session_start", { reason: "new" }, makeCtx("s1", { idle: true }))

  await until(() => sendMessageCalls.length === 1, "message enqueued once")
  await until(() => punkTestServer.ackCalls >= 1, "first ack attempted")
  await sleep(200)
  must(!acked("m1"), "the failed ACK left the message unacked")

  // The row is now leased to the bridge's owner: another consumer's
  // leased read inside the window gets nothing.
  const foreign = await punkTestServer.inbox["pi:s1"].find(() => true)
  const otherRead = await fetch(
    "http://punk.test/v1/namespaces/agent-test-ns/messages?agent=" + encodeURIComponent("pi:s1") +
      "&limit=50&lease_seconds=15&leased_by=other-consumer"
  )
  const otherBody = await otherRead.json()
  must(otherBody.messages.length === 0, "a different lease owner must not see the leased row, saw " + otherBody.messages.length)
  must(punkTestServer.inbox["pi:s1"].length === 1, "the row is still unread server-side after the failed ACK")

  // The turn the enqueue started settles: the drain re-acks the delivered
  // id (same owner re-reads fine) without a second enqueue.
  await fire("agent_settled", {}, makeCtx("s1", { idle: true }))
  await until(() => acked("m1"), "ack retried and succeeded after recovery")
  must(sendMessageCalls.length === 1, "ack retry must never re-enqueue the model, calls=" + sendMessageCalls.length)
  must(punkTestServer.ackCalls >= 2, "the ack was actually retried, ackCalls=" + punkTestServer.ackCalls)

  console.log("PASS lease-exclusivity-ack-retry")
  process.exit(0)
`
	out := runPiMessagingHarness(t, nil, driver)
	if !strings.Contains(out, "PASS lease-exclusivity-ack-retry") {
		t.Fatalf("driver did not report PASS lease-exclusivity-ack-retry:\n%s", out)
	}
}

// TestPiMessagingRegistrationOutageRecovery: a registration outage is
// retried autonomously on bounded exponential backoff until the server
// confirms; no SSE stream and no enqueue before the confirmation.
func TestPiMessagingRegistrationOutageRecovery(t *testing.T) {
	driver := `
  punkTestServer.memberFailNext = 3
  putMessage("pi:s1", "m1", "delivered once registration recovers")
  punkPiExtension(fakePi)
  await fire("session_start", { reason: "new" }, makeCtx("s1", { idle: true }))

  await until(() => punkTestServer.memberAttempts >= 3, "three failing registration attempts observed")
  must(sendMessageCalls.length === 0, "no enqueue before a confirmed registration")
  must(punkTestServer.sseFetches === 0, "no SSE stream before a confirmed registration")

  await until(() => punkTestServer.registeredAgents.some((r) => r.agent === "pi:s1"), "registration retried until the server confirmed it")
  must(punkTestServer.memberAttempts === 4, "exactly one successful attempt after three failures, attempts=" + punkTestServer.memberAttempts)
  await until(() => punkTestServer.sseFetches >= 1, "SSE starts only after confirmed registration")
  await until(() => sendMessageCalls.length === 1, "message enqueued once registration recovered")
  await until(() => acked("m1"), "message acked after delivery")

  console.log("PASS registration-outage-recovery")
  process.exit(0)
`
	out := runPiMessagingHarness(t, nil, driver)
	if !strings.Contains(out, "PASS registration-outage-recovery") {
		t.Fatalf("driver did not report PASS registration-outage-recovery:\n%s", out)
	}
}

// TestPiMessagingSSEReconnectAndDelivery: a stream that connects but
// never sends a byte is aborted by the idle heartbeat watchdog and
// reconnected; a message arriving afterwards is delivered.
func TestPiMessagingSSEReconnectAndDelivery(t *testing.T) {
	driver := `
  punkTestServer.sseStallStream = 1 // first stream: connect, zero bytes, watchdog reconnect
  punkPiExtension(fakePi)
  await fire("session_start", { reason: "new" }, makeCtx("s1", { idle: true }))

  await until(() => punkTestServer.sseFetches >= 2, "SSE reconnected after the first stream stalled", 6000)
  must(punkTestServer.sseCancelled.some((a) => a === "pi:s1"), "the stalled stream's reader was cancelled")

  putMessage("pi:s1", "m1", "delivered after the reconnect")
  emitHint("pi:s1")
  await until(() => sendMessageCalls.length === 1, "message delivered over the reconnected stream")
  await until(() => acked("m1"), "message acked")

  console.log("PASS sse-reconnect-delivery")
  process.exit(0)
`
	out := runPiMessagingHarness(t, map[string]string{"PUNK_MESSAGING_IDLE_TIMEOUT_MS": "80"}, driver)
	if !strings.Contains(out, "PASS sse-reconnect-delivery") {
		t.Fatalf("driver did not report PASS sse-reconnect-delivery:\n%s", out)
	}
}

// TestPiMessagingNonOKSSEBackoff: non-OK SSE responses (with a body) have
// the body cancelled and retry on the same exponential capped backoff as
// dropped connections - the backoff must NOT reset between non-OK
// attempts - and the stream works once the server recovers.
func TestPiMessagingNonOKSSEBackoff(t *testing.T) {
	driver := `
  punkTestServer.sseNonOK = 3
  putMessage("pi:s1", "m1", "delivered once SSE recovers")
  punkPiExtension(fakePi)
  await fire("session_start", { reason: "new" }, makeCtx("s1", { idle: true }))

  await until(() => punkTestServer.sseFetches >= 4, "three non-OK attempts then a good connect")
  await until(() => acked("m1"), "message delivered once the stream connects")

  must(
    punkTestServer.sseBodiesCancelled.filter((a) => a === "pi:s1").length === 3,
    "every non-OK SSE body was cancelled, cancelled=" + JSON.stringify(punkTestServer.sseBodiesCancelled)
  )
  const t = punkTestServer.sseTimes
  must(t.length >= 4, "four SSE attempts recorded, got " + t.length)
  const gap1 = t[1] - t[0]
  const gap2 = t[2] - t[1]
  const gap3 = t[3] - t[2]
  must(gap1 >= 40, "first retry waited the base backoff, gap=" + gap1)
  must(gap2 >= 80, "second retry doubled the backoff, gap=" + gap2)
  must(gap3 >= 160, "third retry doubled again, gap=" + gap3)
  must(gap3 > gap1, "backoff escalated across non-OK attempts (no reset), gaps=" + gap1 + "/" + gap2 + "/" + gap3)

  console.log("PASS non-ok-sse-backoff")
  process.exit(0)
`
	out := runPiMessagingHarness(t, map[string]string{"PUNK_MESSAGING_BACKOFF_MS": "40"}, driver)
	if !strings.Contains(out, "PASS non-ok-sse-backoff") {
		t.Fatalf("driver did not report PASS non-ok-sse-backoff:\n%s", out)
	}
}

// TestPiMessagingSendMessageThrows: a sendMessage that throws (or a host
// without the API surface failing synchronously) is a failed delivery -
// no ACK for that message, no enqueue churn while the failure mode
// persists, and a clean delivery once it recovers.
func TestPiMessagingSendMessageThrows(t *testing.T) {
	driver := `
  // Enough throws to cover the initial drain plus the queued follow-up
  // drain, so the failure mode genuinely persists until the driver turns
  // it off.
  sendMessagePlan = ["throw", "throw", "throw", "throw", "throw"]
  putMessage("pi:s1", "m1", "first attempt will throw")
  punkPiExtension(fakePi)
  await fire("session_start", { reason: "new" }, makeCtx("s1", { idle: true }))

  await until(() => sendMessageCalls.length >= 1, "failed enqueue was still attempted")
  await sleep(250)
  must(punkTestServer.ackCalls === 0, "no ack while the enqueue keeps failing, ackCalls=" + punkTestServer.ackCalls)
  must(!acked("m1"), "nothing acked for the failed enqueues")
  must(sendMessageCalls.length <= 3, "no churn: initial attempt plus at most the queued follow-up drain and one hint retry, calls=" + sendMessageCalls.length)
  const attemptsAfterThrow = sendMessageCalls.length

  sendMessagePlan = []
  emitHint("pi:s1")
  await until(() => acked("m1"), "m1 enqueued and acked after recovery")
  must(sendMessageCalls.length === attemptsAfterThrow + 1, "one fresh enqueue delivered it, calls=" + sendMessageCalls.length + " (was " + attemptsAfterThrow + ")")

  console.log("PASS sendmessage-throws")
  process.exit(0)
`
	out := runPiMessagingHarness(t, nil, driver)
	if !strings.Contains(out, "PASS sendmessage-throws") {
		t.Fatalf("driver did not report PASS sendmessage-throws:\n%s", out)
	}
}

// TestPiMessagingShutdownDisposal: session_shutdown unbinds the session,
// aborts its SSE stream, allows no reconnect, delivers nothing for late
// messages, and a duplicate session_start for the same id must NOT rebind
// it.
func TestPiMessagingShutdownDisposal(t *testing.T) {
	driver := `
  punkPiExtension(fakePi)
  await fire("session_start", { reason: "new" }, makeCtx("s1", { idle: true }))
  await until(() => punkTestServer.sseFetches >= 1, "SSE stream connected")

  const memberAttemptsBefore = punkTestServer.memberAttempts
  await fire("session_shutdown", { reason: "quit" }, makeCtx("s1", { idle: true }))
  await until(() => punkTestServer.sseAborted.length > 0, "the live stream's fetch was aborted on shutdown")

  const sseCountAfter = punkTestServer.sseFetches
  await sleep(400) // far past the 10ms backoff schedule: no reconnect may occur
  must(punkTestServer.sseFetches === sseCountAfter, "unbound session must not reconnect, fetches=" + punkTestServer.sseFetches + " (was " + sseCountAfter + ")")

  putMessage("pi:s1", "m-late", "too late, session is gone")
  emitHint("pi:s1")
  await sleep(150)
  must(sendMessageCalls.length === 0, "no delivery for an unbound session, calls=" + sendMessageCalls.length)

  await fire("session_start", { reason: "new" }, makeCtx("s1", { idle: true }))
  await sleep(200)
  must(punkTestServer.memberAttempts === memberAttemptsBefore, "unbound session must never re-register, attempts=" + punkTestServer.memberAttempts)
  must(punkTestServer.sseFetches === sseCountAfter, "unbound session must never re-listen")

  console.log("PASS shutdown-disposal")
  process.exit(0)
`
	out := runPiMessagingHarness(t, nil, driver)
	if !strings.Contains(out, "PASS shutdown-disposal") {
		t.Fatalf("driver did not report PASS shutdown-disposal:\n%s", out)
	}
}

// TestPiMessagingDisabledBaseline: without PUNK_MESSAGING=1 the bridge is
// completely inert - no registration, no inbox reads, no SSE, no enqueue,
// no message key on the before_agent_start return - while the memory
// hooks keep firing exactly as before (zero-network fail-open baseline
// around the opt-in feature).
func TestPiMessagingDisabledBaseline(t *testing.T) {
	driver := `
  punkTestServer.memoryContext = "## Project memory"
  putMessage("pi:s1", "m1", "nobody is listening")
  punkPiExtension(fakePi)

  await fire("session_start", { reason: "new" }, makeCtx("s1", { idle: true }))
  await fire("input", { text: "plain memory capture", source: "interactive" }, makeCtx("s1", { idle: false }))
  await fire("turn_start", { turnIndex: 0 }, makeCtx("s1", { idle: false }))
  const res = await fire("before_agent_start", { prompt: "work", systemPrompt: "BASE" }, makeCtx("s1", { idle: false }))
  await fire("turn_end", { message: { role: "assistant", content: [{ type: "text", text: "done" }] } }, makeCtx("s1", { idle: false }))
  await fire("agent_settled", {}, makeCtx("s1", { idle: true }))
  await fire("session_shutdown", { reason: "quit" }, makeCtx("s1", { idle: true }))

  await sleep(200)
  must(sendMessageCalls.length === 0, "no enqueue when messaging is disabled")
  must(punkTestServer.sseFetches === 0, "no SSE connections when messaging is disabled")
  must(!punkTestServer.registeredAgents.some((r) => r.agent === "pi:s1"), "no member registration when messaging is disabled")
  must(
    fetchCalls.every((c) => c.path === "/v1/agent/hooks" || c.path === "/v1/agent/context"),
    "only memory-hook calls happen when messaging is disabled, got " + JSON.stringify(fetchCalls.map((c) => c.path))
  )
  must(res && res.systemPrompt === "BASE\n\n## Project memory" && !("message" in res), "memory injection works and carries no message key, got " + JSON.stringify(res))

  console.log("PASS disabled-baseline")
  process.exit(0)
`
	// PUNK_MESSAGING explicitly off (the default state of a real install).
	out := runPiMessagingHarness(t, map[string]string{"PUNK_MESSAGING": "0"}, driver)
	if !strings.Contains(out, "PASS disabled-baseline") {
		t.Fatalf("driver did not report PASS disabled-baseline:\n%s", out)
	}
}

// TestPiMessagingWakeCap: sendMessage-triggered wakes are capped per
// sliding window (same env keys and defaults as punk hook inbox's
// continuation cap); past the cap the remaining messages stay unread and
// are released for a later window instead of stacking turns.
func TestPiMessagingWakeCap(t *testing.T) {
	driver := `
  punkPiExtension(fakePi)
  await fire("session_start", { reason: "new" }, makeCtx("s1", { idle: true }))
  await until(() => punkTestServer.registeredAgents.some((r) => r.agent === "pi:s1"), "session registered")

  putMessage("pi:s1", "w1", "wake one")
  putMessage("pi:s1", "w2", "wake two")
  putMessage("pi:s1", "w3", "wake three")
  emitHint("pi:s1")

  await until(() => sendMessageCalls.length === 1, "first wake enqueued")
  await until(() => acked("w1"), "first wake acked")

  // Each settled turn drains the next message until the cap (2) is hit.
  await fire("agent_settled", {}, makeCtx("s1", { idle: true }))
  await until(() => sendMessageCalls.length === 2, "second wake enqueued after settle")
  await until(() => acked("w2"), "second wake acked")

  await fire("agent_settled", {}, makeCtx("s1", { idle: true }))
  await sleep(300)
  must(sendMessageCalls.length === 2, "the wake cap stops further enqueues, calls=" + sendMessageCalls.length)
  must(!acked("w3"), "the capped message stays unacked")
  must(inboxCount("pi:s1") === 1, "the capped message stays unread server-side, rows=" + inboxCount("pi:s1"))
  must(punkTestServer.releaseCalls.some((r) => r.id === "w3"), "the suppressed message's lease was released, not silently held")

  console.log("PASS wake-cap")
  process.exit(0)
`
	out := runPiMessagingHarness(t, map[string]string{"PUNK_MESSAGING_MAX_CONTINUE": "2"}, driver)
	if !strings.Contains(out, "PASS wake-cap") {
		t.Fatalf("driver did not report PASS wake-cap:\n%s", out)
	}
}

// TestPiMessagingAllowlist: senders outside PUNK_MESSAGING_FROM are held
// back - released unread and unacked - while an allowed sender's message
// delivers normally.
func TestPiMessagingAllowlist(t *testing.T) {
	driver := fmt.Sprintf(`
  putMessage("pi:s1", "m-allow", "from the planner", "planner-agent")
  putMessage("pi:s1", "m-hold", "from a stranger", "random-stranger")
  punkPiExtension(fakePi)
  await fire("session_start", { reason: "new" }, makeCtx("s1", { idle: true }))

  await until(() => sendMessageCalls.length === 1, "the allowed message was enqueued")
  await until(() => acked("m-allow"), "the allowed message was acked")
  must(sendMessageCalls[0].msg.content === %s, "the delivered envelope is exactly the allowed message")
  must(!acked("m-hold"), "the held-back message was never acked")
  must(inboxCount("pi:s1") === 1, "the held-back message stays unread server-side, rows=" + inboxCount("pi:s1"))
  must(
    punkTestServer.releaseCalls.some((r) => r.id === "m-hold"),
    "the held-back message's lease was released"
  )

  console.log("PASS allowlist")
  process.exit(0)
`, jsStringLiteral(expectedPiEnvelope("pi:s1", "m-allow", "planner-agent", "from the planner")))
	out := runPiMessagingHarness(t, map[string]string{"PUNK_MESSAGING_FROM": "planner"}, driver)
	if !strings.Contains(out, "PASS allowlist") {
		t.Fatalf("driver did not report PASS allowlist:\n%s", out)
	}
}

// TestPiMessagingFailOpenDeadServer: a totally dead punk server must
// never break the hooks - every handler still resolves (the memory path
// and the bridge both swallow their own errors), and the session simply
// never binds.
func TestPiMessagingFailOpenDeadServer(t *testing.T) {
	driver := `
  globalThis.fetch = () => Promise.reject(new Error("network down"))
  punkPiExtension(fakePi)

  await fire("session_start", { reason: "new" }, makeCtx("s1", { idle: true }))
  await fire("input", { text: "capture me anyway", source: "interactive" }, makeCtx("s1", { idle: false }))
  await fire("turn_start", { turnIndex: 0 }, makeCtx("s1", { idle: false }))
  const res = await fire("before_agent_start", { prompt: "work", systemPrompt: "BASE" }, makeCtx("s1", { idle: false }))
  await fire("agent_settled", {}, makeCtx("s1", { idle: true }))
  await fire("session_shutdown", { reason: "quit" }, makeCtx("s1", { idle: true }))
  must(res === undefined, "a dead server yields the plain undefined return, got " + JSON.stringify(res))

  console.log("PASS fail-open-dead-server")
  process.exit(0)
`
	out := runPiMessagingHarness(t, nil, driver)
	if !strings.Contains(out, "PASS fail-open-dead-server") {
		t.Fatalf("driver did not report PASS fail-open-dead-server:\n%s", out)
	}
}

// TestPiMessagingDeniedFirst50Allowed51 (allowlist starvation): a backlog
// of 50 allowlist-DENIED rows older than one fetch batch must not starve
// the allowed row behind them. The pass keeps denied rows leased (the
// server hides live leases from every reader, this owner included), so
// round 2 reads past them, delivers the allowed row, and releases the
// denied rows at pass end so they stay visible to later events.
func TestPiMessagingDeniedFirst50Allowed51(t *testing.T) {
	driver := fmt.Sprintf(`
  for (let i = 0; i < 50; i++) {
    putMessage("pi:s1", "denied-" + i, "noise " + i, "stranger-agent")
  }
  putMessage("pi:s1", "m-allow", "the one allowed message", "planner-agent")
  punkPiExtension(fakePi)
  await fire("session_start", { reason: "new" }, makeCtx("s1", { idle: true }))

  await until(() => sendMessageCalls.length === 1, "the allowed row behind 50 denied rows was delivered")
  await until(() => acked("m-allow"), "the allowed row was acked")
  must(sendMessageCalls[0].msg.content === %s, "the delivered envelope is exactly the allowed message")

  const readCalls = fetchCalls.filter(
    (c) => c.path === "/v1/namespaces/agent-test-ns/messages" && c.method === "GET" && c.query.indexOf("lease_seconds=") >= 0
  )
  must(readCalls.length >= 2, "the pass needed a second round to read past the denied batch, rounds=" + readCalls.length)
  must(inboxCount("pi:s1") === 50, "all 50 denied rows stay unread server-side, rows=" + inboxCount("pi:s1"))
  let releasedDenied = 0
  for (const r of punkTestServer.releaseCalls) {
    if (r.id.indexOf("denied-") === 0) releasedDenied++
  }
  must(releasedDenied === 50, "every denied row was released at pass end, released=" + releasedDenied)
  must(!punkTestServer.ackedIds.some((a) => a.id.indexOf("denied-") === 0), "no denied row was ever acked")

  console.log("PASS denied-first50-allowed51")
  process.exit(0)
`, jsStringLiteral(expectedPiEnvelope("pi:s1", "m-allow", "planner-agent", "the one allowed message")))
	out := runPiMessagingHarness(t, map[string]string{"PUNK_MESSAGING_FROM": "planner"}, driver)
	if !strings.Contains(out, "PASS denied-first50-allowed51") {
		t.Fatalf("driver did not report PASS denied-first50-allowed51:\n%s", out)
	}
}

// TestPiMessagingAckZeroOnExpiredLease: the server answers {acked:0}
// (HTTP 200) when the lease expired before the ACK - that is NOT success.
// The bridge keeps the id pending (no second enqueue for it) and
// schedules the post-expiry re-acquire/re-ack, which re-leases the row
// and confirms the ACK without ever prompting the model again.
func TestPiMessagingAckZeroOnExpiredLease(t *testing.T) {
	driver := `
  punkTestServer.ackZeroNext = true
  putMessage("pi:s1", "m1", "acked only after reacquire")
  punkPiExtension(fakePi)
  await fire("session_start", { reason: "new" }, makeCtx("s1", { idle: true }))

  await until(() => sendMessageCalls.length === 1, "message enqueued once")
  await until(() => punkTestServer.ackCalls >= 1, "first ack attempted")
  await sleep(200)
  must(!acked("m1"), "the {acked:0} answer left the message unacked")
  must(sendMessageCalls.length === 1, "the pending ACK must not re-enqueue the model, calls=" + sendMessageCalls.length)
  must(punkTestServer.inbox["pi:s1"].length === 1, "the row is still unread server-side after the failed ACK")

  // The scheduled re-acquire pass (after the 1s test lease expires)
  // re-leases the row and re-acks it successfully.
  await until(() => acked("m1"), "the pending ACK was reacquired and confirmed", 5000)
  must(sendMessageCalls.length === 1, "the re-ack never re-enqueues the model, calls=" + sendMessageCalls.length)
  must(punkTestServer.ackCalls >= 2, "the ack was actually retried, ackCalls=" + punkTestServer.ackCalls)

  console.log("PASS ack-zero-expired-lease")
  process.exit(0)
`
	out := runPiMessagingHarness(t, nil, driver)
	if !strings.Contains(out, "PASS ack-zero-expired-lease") {
		t.Fatalf("driver did not report PASS ack-zero-expired-lease:\n%s", out)
	}
}

// TestPiMessagingMultipleSessions: two live sessions bind to distinct
// pi:<session_id> addresses with separate leases, and a message for one
// inbox is enqueued only to that session.
func TestPiMessagingMultipleSessions(t *testing.T) {
	driver := `
  punkPiExtension(fakePi)
  await fire("session_start", { reason: "new" }, makeCtx("s1", { idle: true }))
  await fire("session_start", { reason: "new" }, makeCtx("s2", { idle: true }))
  await until(
    () => punkTestServer.registeredAgents.some((r) => r.agent === "pi:s1") &&
         punkTestServer.registeredAgents.some((r) => r.agent === "pi:s2"),
    "both sessions registered under distinct addresses"
  )

  putMessage("pi:s1", "m1", "message for session one")
  emitHint("pi:s1")
  await until(() => sendMessageCalls.length === 1, "message for s1 enqueued")
  await until(() => acked("m1"), "m1 acked")

  await sleep(150)
  must(sendMessageCalls.length === 1, "session s2 was not enqueued for s1's message")

  // s1 is busy pending its settle; s2 is still idle and gets its own
  // message independently.
  putMessage("pi:s2", "m2", "message for session two")
  emitHint("pi:s2")
  await until(() => sendMessageCalls.length === 2, "message for s2 enqueued")
  await until(() => acked("m2"), "m2 acked")
  must(sendMessageCalls[1].msg.details.address === "pi:s2", "second enqueue went to session s2")

  console.log("PASS multiple-sessions")
  process.exit(0)
`
	out := runPiMessagingHarness(t, nil, driver)
	if !strings.Contains(out, "PASS multiple-sessions") {
		t.Fatalf("driver did not report PASS multiple-sessions:\n%s", out)
	}
}

// TestPiMessagingNonOKFetchRetry: a drain whose leased fetch gets a
// non-OK response schedules one bounded retry (same backoff env as SSE)
// instead of waiting silently for the next external hint; the message is
// delivered by the retry.
func TestPiMessagingNonOKFetchRetry(t *testing.T) {
	driver := `
  punkTestServer.messageFailNext = 1 // the first leased read answers 500
  putMessage("pi:s1", "m1", "delivered by the fetch retry")
  punkPiExtension(fakePi)
  await fire("session_start", { reason: "new" }, makeCtx("s1", { idle: true }))

  await until(() => sendMessageCalls.length === 1, "message delivered after the non-OK fetch was retried", 6000)
  await until(() => acked("m1"), "message acked")
  const readCalls = fetchCalls.filter(
    (c) => c.path === "/v1/namespaces/agent-test-ns/messages" && c.method === "GET" && c.query.indexOf("lease_seconds=") >= 0
  )
  must(readCalls.length >= 2, "the failed fetch was actually retried, reads=" + readCalls.length)

  console.log("PASS non-ok-fetch-retry")
  process.exit(0)
`
	out := runPiMessagingHarness(t, nil, driver)
	if !strings.Contains(out, "PASS non-ok-fetch-retry") {
		t.Fatalf("driver did not report PASS non-ok-fetch-retry:\n%s", out)
	}
}

// TestPiMessagingDenied130BatchedRelease (batch bound): a backlog of 130
// allowlist-denied rows older than one pass (up to 250 acquired rows)
// exercises the server's 100-id ACK/release batch bound - the bridge must
// deduplicate and batch every request under it, deliver the eligible row
// behind the denied backlog, and release all denied rows.
func TestPiMessagingDenied130BatchedRelease(t *testing.T) {
	driver := fmt.Sprintf(`
  for (let i = 0; i < 130; i++) {
    putMessage("pi:s1", "denied-" + i, "noise " + i, "stranger-agent")
  }
  putMessage("pi:s1", "m-allow", "the one allowed message", "planner-agent")
  punkPiExtension(fakePi)
  await fire("session_start", { reason: "new" }, makeCtx("s1", { idle: true }))

  await until(() => sendMessageCalls.length === 1, "the allowed row behind 130 denied rows was delivered", 8000)
  await until(() => acked("m-allow"), "the allowed row was acked")
  must(sendMessageCalls[0].msg.content === %s, "the delivered envelope is exactly the allowed message")

  must(punkTestServer.oversizeBatches === 0, "no ACK or release request ever exceeded the server's 100-id batch bound")
  for (const n of punkTestServer.ackBatches) must(n <= 100, "every ACK batch is within the bound, saw " + n)
  for (const n of punkTestServer.releaseBatches) must(n <= 100, "every release batch is within the bound, saw " + n)
  let deniedRows = 0
  for (const m of punkTestServer.inbox["pi:s1"] || []) {
    if (m.id.indexOf("denied-") === 0) deniedRows++
  }
  must(deniedRows === 130, "all 130 denied rows stay unread server-side, rows=" + deniedRows)
  let releasedDenied = 0
  for (const r of punkTestServer.releaseCalls) {
    if (r.id.indexOf("denied-") === 0) releasedDenied++
  }
  must(releasedDenied >= 130, "every denied row was released, released=" + releasedDenied)
  must(!punkTestServer.ackedIds.some((a) => a.id.indexOf("denied-") === 0), "no denied row was ever acked")

  console.log("PASS denied-130-batched-release")
  process.exit(0)
`, jsStringLiteral(expectedPiEnvelope("pi:s1", "m-allow", "planner-agent", "the one allowed message")))
	out := runPiMessagingHarness(t, map[string]string{"PUNK_MESSAGING_FROM": "planner"}, driver)
	if !strings.Contains(out, "PASS denied-130-batched-release") {
		t.Fatalf("driver did not report PASS denied-130-batched-release:\n%s", out)
	}
}

// TestPiMessagingCapZeroDisablesWakes (cap parse): PUNK_MESSAGING_MAX_CONTINUE=0
// is a VALID value that disables waking entirely (a non-negative parse -
// it must not fall back to the default 5). Nothing is ever enqueued, the
// row is released unread, no wake retry is scheduled (no window can ever
// free a slot), and in-turn catch-up still works because it is not a wake.
func TestPiMessagingCapZeroDisablesWakes(t *testing.T) {
	driver := fmt.Sprintf(`
  putMessage("pi:s1", "m1", "no wakes are allowed")
  punkPiExtension(fakePi)
  await fire("session_start", { reason: "new" }, makeCtx("s1", { idle: true }))
  await until(() => punkTestServer.registeredAgents.some((r) => r.agent === "pi:s1"), "session registered")

  await sleep(400)
  must(sendMessageCalls.length === 0, "cap 0 must disable waking entirely, calls=" + sendMessageCalls.length)
  must(!acked("m1"), "nothing was acked")
  must(punkTestServer.releaseCalls.some((r) => r.id === "m1"), "the suppressed row was released, not silently held")
  must(inboxCount("pi:s1") === 1, "the row stays unread server-side")

  // In-turn catch-up is NOT a wake and is never capped.
  const res = await fire("before_agent_start", { prompt: "work", systemPrompt: "BASE" }, makeCtx("s1", { idle: false }))
  must(res && res.message && res.message.content === %s, "in-turn catch-up still delivers with cap 0")
  must(sendMessageCalls.length === 0, "the injection is not a wake, calls=" + sendMessageCalls.length)

  // The injection's pending mark resolves through the recurrent expiry
  // pass with no further event at all.
  await until(() => acked("m1"), "the injected id was acked by the recurrent expiry pass", 6000)

  console.log("PASS cap-zero-disables-wakes")
  process.exit(0)
`, jsStringLiteral(expectedPiEnvelope("pi:s1", "m1", "planner-agent", "no wakes are allowed")))
	out := runPiMessagingHarness(t, map[string]string{"PUNK_MESSAGING_MAX_CONTINUE": "0"}, driver)
	if !strings.Contains(out, "PASS cap-zero-disables-wakes") {
		t.Fatalf("driver did not report PASS cap-zero-disables-wakes:\n%s", out)
	}
}

// TestPiMessagingFailedSecondPageCleanup (mid-pass failure): when the
// pass's SECOND fetch fails after the first round already leased its
// denied rows, the pass must release everything it acquired before
// returning null - no lease stays stranded hidden until expiry - and the
// bounded drain retry then completes the delivery.
func TestPiMessagingFailedSecondPageCleanup(t *testing.T) {
	driver := fmt.Sprintf(`
  for (let i = 0; i < 50; i++) {
    putMessage("pi:s1", "denied-" + i, "noise " + i, "stranger-agent")
  }
  putMessage("pi:s1", "m-allow", "delivered after the failed page", "planner-agent")
  // Let exactly ONE leased read succeed, then fail the next (the pass's
  // round-2 fetch).
  punkTestServer.messageFailAfter = 1
  punkPiExtension(fakePi)
  await fire("session_start", { reason: "new" }, makeCtx("s1", { idle: true }))

  await until(() => sendMessageCalls.length === 1, "the allowed row was delivered by the retried drain", 8000)
  await until(() => acked("m-allow"), "the allowed row was acked")
  must(sendMessageCalls[0].msg.content === %s, "the delivered envelope is exactly the allowed message")

  const reads = fetchCalls.filter(
    (c) => c.path === "/v1/namespaces/agent-test-ns/messages" && c.method === "GET" && c.query.indexOf("lease_seconds=") >= 0
  ).length
  must(reads >= 3, "the failed round-2 fetch was followed by a retry pass, reads=" + reads)
  // The round-1 leases were released best effort on the failure path.
  let releasedDenied = 0
  for (const r of punkTestServer.releaseCalls) {
    if (r.id.indexOf("denied-") === 0) releasedDenied++
  }
  must(releasedDenied >= 50, "the failed pass released its acquired rows, released=" + releasedDenied)
  let deniedRows = 0
  for (const m of punkTestServer.inbox["pi:s1"] || []) {
    if (m.id.indexOf("denied-") === 0) deniedRows++
  }
  must(deniedRows === 50, "the denied rows stay unread server-side, rows=" + deniedRows)
  must(punkTestServer.oversizeBatches === 0, "no oversize batch was ever sent")

  console.log("PASS failed-second-page-cleanup")
  process.exit(0)
`, jsStringLiteral(expectedPiEnvelope("pi:s1", "m-allow", "planner-agent", "delivered after the failed page")))
	out := runPiMessagingHarness(t, map[string]string{"PUNK_MESSAGING_FROM": "planner"}, driver)
	if !strings.Contains(out, "PASS failed-second-page-cleanup") {
		t.Fatalf("driver did not report PASS failed-second-page-cleanup:\n%s", out)
	}
}

// TestPiMessagingFreshMessageDuringReack (housekeeping must not hide new
// work): while a pending ACK waits for its expiry pass, a NEW eligible
// message arrives; the reack pass acquires it with its fetch but must
// release it (it delivers nothing) so the fresh row is never hidden
// behind the housekeeping lease - the next trigger delivers it.
func TestPiMessagingFreshMessageDuringReack(t *testing.T) {
	driver := `
  punkTestServer.ackFailNext = 1
  putMessage("pi:s1", "m1", "delivered, ack fails once")
  punkPiExtension(fakePi)
  await fire("session_start", { reason: "new" }, makeCtx("s1", { idle: true }))

  await until(() => sendMessageCalls.length === 1, "the first message was enqueued")
  await until(() => punkTestServer.ackCalls >= 1, "the first ack failed")
  must(!acked("m1"), "m1 is pending")

  // A fresh eligible message arrives while the reack pass is scheduled.
  putMessage("pi:s1", "m2", "fresh work during housekeeping")

  // The scheduled reack pass runs: it re-acks m1 and must RELEASE the
  // fresh m2 it accidentally acquired.
  await until(() => punkTestServer.releaseCalls.some((r) => r.id === "m2"), "the reack pass released the fresh row", 6000)
  await until(() => acked("m1"), "the pending ack was confirmed")
  must(sendMessageCalls.length === 1, "the reack pass never enqueues, calls=" + sendMessageCalls.length)
  must(inboxCount("pi:s1") === 1, "the fresh row is still unread and visible, rows=" + inboxCount("pi:s1"))

  // The turn the first wake started settles, then the next trigger
  // delivers the fresh row normally.
  await fire("agent_settled", {}, makeCtx("s1", { idle: true }))
  emitHint("pi:s1")
  await until(() => sendMessageCalls.length === 2, "the fresh row was delivered by the next trigger")
  await until(() => acked("m2"), "the fresh row was acked")
  must(sendMessageCalls[1].msg.content.indexOf("fresh work during housekeeping") >= 0, "the fresh body was delivered")

  console.log("PASS fresh-message-during-reack")
  process.exit(0)
`
	out := runPiMessagingHarness(t, nil, driver)
	if !strings.Contains(out, "PASS fresh-message-during-reack") {
		t.Fatalf("driver did not report PASS fresh-message-during-reack:\n%s", out)
	}
}

// TestPiMessagingRecurrentReackReconciles (no unbounded retry): when a
// pending ACK's row is taken by another consumer (or its reply ACK is
// lost), the recurrent pass never sees the id again; after two
// consecutive absences the pending mark is reconciled away and the
// scheduling stops - the loop and the delivered set cannot run or grow
// forever on rows that never come back.
func TestPiMessagingRecurrentReackReconciles(t *testing.T) {
	driver := `
  punkTestServer.ackZeroNext = true
  putMessage("pi:s1", "m1", "pending, then consumed elsewhere")
  punkPiExtension(fakePi)
  await fire("session_start", { reason: "new" }, makeCtx("s1", { idle: true }))

  await until(() => sendMessageCalls.length === 1, "the message was enqueued once")
  await until(() => punkTestServer.ackCalls >= 1, "the ack answered {acked:0}")
  must(!acked("m1"), "the id is pending")

  // Another consumer takes the row: leased and ACKed server-side, so it
  // disappears from this owner's view entirely.
  consumeElsewhere("pi:s1", "m1")

  const reads = () =>
    fetchCalls.filter(
      (c) => c.path === "/v1/namespaces/agent-test-ns/messages" && c.method === "GET" && c.query.indexOf("lease_seconds=") >= 0
    ).length

  // Drain read + two absent reack passes (paced by the lease window).
  await until(() => reads() >= 3, "the recurrent pass ran twice without seeing the row", 8000)
  await sleep(2600) // past two more pacing intervals: the loop must have stopped
  const n = reads()
  await sleep(2000)
  must(reads() === n, "the reconciled pending id stopped the recurrent pass, reads=" + reads() + " (was " + n + ")")
  must(sendMessageCalls.length === 1, "no re-delivery happened, calls=" + sendMessageCalls.length)

  console.log("PASS recurrent-reack-reconciles")
  process.exit(0)
`
	out := runPiMessagingHarness(t, nil, driver)
	if !strings.Contains(out, "PASS recurrent-reack-reconciles") {
		t.Fatalf("driver did not report PASS recurrent-reack-reconciles:\n%s", out)
	}
}

// TestPiMessagingWakeWindowRoll (cap-exhausted self wake): after the cap
// suppresses a backlog on an idle session, one cancellable wake at the
// next window expiry delivers the remainder by itself - no hint, no
// unrelated event. Short window (1s) stands in for the test clock.
func TestPiMessagingWakeWindowRoll(t *testing.T) {
	driver := `
  putMessage("pi:s1", "w1", "wake one")
  putMessage("pi:s1", "w2", "wake two")
  punkPiExtension(fakePi)
  await fire("session_start", { reason: "new" }, makeCtx("s1", { idle: true }))

  // Cap 1: the first drain enqueues w1 and defers w2 to the settle.
  await until(() => sendMessageCalls.length === 1, "wake one enqueued")
  await until(() => acked("w1"), "wake one acked")

  // The settle drains again: the cap suppresses w2 (no second enqueue)
  // and schedules the single window-expiry wake.
  await fire("agent_settled", {}, makeCtx("s1", { idle: true }))
  await sleep(300)
  must(sendMessageCalls.length === 1, "the cap suppressed the second wake, calls=" + sendMessageCalls.length)
  must(!acked("w2"), "the suppressed row was not acked")
  must(punkTestServer.releaseCalls.some((r) => r.id === "w2"), "the suppressed row was released")

  // Window roll (1s): the scheduled retry delivers w2 with NO further
  // event or hint of any kind.
  await until(() => sendMessageCalls.length === 2, "the window-expiry retry delivered the suppressed row", 6000)
  await until(() => acked("w2"), "the suppressed row was acked")
  must(sendMessageCalls[1].msg.content.indexOf("wake two") >= 0, "the right message was delivered")

  console.log("PASS wake-window-roll")
  process.exit(0)
`
	out := runPiMessagingHarness(t, map[string]string{
		"PUNK_MESSAGING_MAX_CONTINUE":            "1",
		"PUNK_MESSAGING_CONTINUE_WINDOW_SECONDS": "1",
	}, driver)
	if !strings.Contains(out, "PASS wake-window-roll") {
		t.Fatalf("driver did not report PASS wake-window-roll:\n%s", out)
	}
}
