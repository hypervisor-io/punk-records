package hookcli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// codeops:trace repo=punk-records work_item=punk-agent-refresh-20261009 spec=docs/agent-messaging.md plan=plugin-namespace test=TestPluginBindingAddressNamespaces,TestPluginBindingRebindColdState
// F06: execute each real generated plugin against one namespace-aware fake.
// Messages, membership and leases are keyed by namespace AND address, just
// like the server. Gates deliberately return late replies even after abort:
// cancelling a client request cannot undo work the server already accepted.
const pluginBindingHarness = `
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const calls = [], deliveries = [], bindings = new Map(), inboxes = new Map(), members = new Set();
const streams = [], gates = [], failAcks = new Set(), failLookups = new Set(), tools = new Map();
const handlers = new Map();
const cwdNS = "agent-cwd";
const address = (sid) => kind + ":" + sid;
const keyOf = (ns, agent) => JSON.stringify([ns, agent]);
const json = (obj) => new Response(JSON.stringify(obj), { headers: { "Content-Type": "application/json" } });
function must(ok, why) {
  if (!ok) throw new Error(why + "\nrequests=" + JSON.stringify(calls.map((c) => ({ path: c.path, ns: c.ns, agent: c.agent, body: c.body }))));
}
async function until(pred, why) {
  const end = Date.now() + 3500;
  while (Date.now() < end) { if (await pred()) return; await sleep(5); }
  must(false, "timed out: " + why);
}
function hold(match) {
  const gate = { match, entered: false, call: null, release: null };
  gate.promise = new Promise((resolve) => { gate.release = resolve; });
  gates.push(gate);
  return gate;
}
function put(ns, sid, id, body) {
  const agent = address(sid), key = keyOf(ns, agent);
  if (!inboxes.has(key)) inboxes.set(key, []);
  const row = { id, namespace: ns, sender: "planner", recipient: agent, body, created_at: "2026-10-09T00:00:00Z", leasedUntil: 0, leasedBy: "" };
  inboxes.get(key).push(row);
  return row;
}
function hasMember(ns, sid) { return members.has(keyOf(ns, address(sid))); }
function delivered(body) { return deliveries.some((d) => d.text.includes(body)); }
function route(ns, suffix) { return "/v1/namespaces/" + encodeURIComponent(ns) + "/" + suffix; }
function requests(ns, suffix) { return calls.filter((c) => c.path === route(ns, suffix)); }
globalThis.fetch = async (url, init = {}) => {
  const u = new URL(String(url));
  must(u.origin === "http://punk.test", "fake server only");
  const match = u.pathname.match(/^\/v1\/namespaces\/([^/]+)\/(.*)$/);
  const body = init.body ? JSON.parse(init.body) : null;
  const ns = match ? decodeURIComponent(match[1]) : "";
  const agent = u.searchParams.get("agent") || (body && body.agent) || "";
  const c = { path: u.pathname, query: u.searchParams, ns, agent, body, signal: init.signal };
  calls.push(c);
  let response;
  if (u.pathname === "/v1/agent/namespace") {
    // Snapshot now, before the gate: a delayed lookup must not overwrite a
    // newer binding learned on a subsequent supported host event.
    response = failLookups.has(agent) ? new Response("binding unavailable", { status: 500 }) : json({ namespace: bindings.get(agent) || cwdNS });
  } else if (u.pathname === "/v1/agent/hooks") {
    response = json({ status: "stored" });
  } else if (u.pathname === "/v1/agent/context") {
    response = json({ context: "memory for " + (u.searchParams.get("ns") || cwdNS) });
  } else if (match) {
    const suffix = match[2], key = keyOf(ns, agent);
    const rows = inboxes.get(key) || [];
    if (suffix === "members") {
      members.add(key);
      response = json({ namespace: ns, agent, status: "registered" });
    } else if (suffix === "messages/events") {
      must(kind !== "openclaw", "OpenClaw is catch-up only");
      must(members.has(key), "SSE requires membership in its own namespace");
      const stream = { ns, agent, signal: init.signal, closed: false };
      const rs = new ReadableStream({
        start(controller) {
          stream.hint = () => { if (!stream.closed) controller.enqueue(new TextEncoder().encode("event: inbox\ndata: {}\n\n")); };
          stream.close = () => { if (!stream.closed) { stream.closed = true; controller.close(); } };
          stream.hint();
        },
        cancel() { stream.closed = true; },
      });
      streams.push(stream);
      if (init.signal) init.signal.addEventListener("abort", stream.close, { once: true });
      response = new Response(rs, { headers: { "Content-Type": "text/event-stream" } });
    } else if (suffix === "messages") {
      must(members.has(key), "read requires membership in its own namespace");
      const now = Date.now();
      const visible = rows.filter((r) => !r.acked && r.leasedUntil <= now).slice(0, Number(u.searchParams.get("limit") || 100));
      for (const r of visible) {
        r.leasedBy = u.searchParams.get("leased_by");
        r.leasedUntil = now + Number(u.searchParams.get("lease_seconds")) * 1000;
      }
      response = json({ messages: visible });
    } else if (suffix === "messages/ack" || suffix === "messages/release") {
      must(members.has(key), "ACK/release requires membership in its own namespace");
      must(body.ids.length <= 100, "server batch bound");
      let count = 0;
      if (suffix.endsWith("/ack") && failAcks.has(ns)) {
        response = new Response("ACK unavailable", { status: 500 });
      } else {
        for (const r of rows) {
          if (body.ids.includes(r.id) && r.leasedBy === body.leased_by && r.leasedUntil > Date.now()) {
            if (suffix.endsWith("/ack")) r.acked = true;
            r.leasedUntil = 0;
            r.leasedBy = "";
            count++;
          }
        }
        response = json(suffix.endsWith("/ack") ? { acked: count } : { released: count });
      }
    } else if (suffix === "messages/diagnostics") {
      must(members.has(key), "diagnostic requires membership in its own namespace");
      response = json({ status: "recorded" });
    } else {
      must(false, "unexpected namespace route " + suffix);
    }
  } else {
    must(false, "unexpected route " + u.pathname);
  }
  const gate = gates.find((g) => !g.entered && g.match(c));
  if (gate) { gate.entered = true; gate.call = c; await gate.promise; }
  return response;
};
let hooks;
const ctx = (sid, busy = false) => ({ cwd: "/project", sessionId: sid, sessionManager: { getSessionId: () => sid }, isIdle: () => !busy });
async function fire(name, sid, event = {}, busy = false) { return handlers.get(name)(event, ctx(sid, busy)); }
async function initHost() {
  if (kind === "opencode") {
    hooks = await PunkMemoryPlugin({ directory: "/project", client: { session: {
      list: async () => ({ data: [] }), status: async () => ({ data: {} }),
      prompt: async (opts) => {
        const d = { sid: opts.path.id, text: opts.body.parts.map((p) => p.text).join("\n") };
        deliveries.push(d);
        const gate = gates.find((g) => !g.entered && g.match({ path: "prompt", body: d }));
        if (gate) { gate.entered = true; await gate.promise; }
        return { data: { info: { id: "assistant" } } };
      },
    } } });
  } else if (kind === "pi") {
    punkPiExtension({
      on: (name, fn) => handlers.set(name, fn),
      registerTool: (tool) => tools.set(tool.name, tool),
      sendMessage: (msg, opts) => {
        must(opts.deliverAs === "followUp" && opts.triggerTurn, "Pi host enqueue contract");
        deliveries.push({ sid: msg.details.address.slice(3), text: msg.content });
      },
    });
  } else {
    punkOpenClawPlugin.register({ on: (name, fn) => handlers.set(name, fn) });
  }
}
async function start(sid, busy = false) {
  if (kind === "opencode") {
    await hooks.event({ event: { type: "session.created", properties: { info: { id: sid } } } });
    if (busy) await hooks.event({ event: { type: "session.status", properties: { sessionID: sid, status: { type: "busy" } } } });
  } else await fire("session_start", sid, {}, busy);
}
async function refresh(sid) {
  if (kind === "opencode") await hooks.event({ event: { type: "session.idle", properties: { sessionID: sid } } });
  else if (kind === "pi") await fire("agent_settled", sid);
  else await fire("session_start", sid);
}
async function turn(sid) {
  let result;
  if (kind === "opencode") {
    result = { system: [] };
    await hooks["experimental.chat.system.transform"]({ sessionID: sid }, result);
  } else if (kind === "pi") {
    result = await fire("before_agent_start", sid, { systemPrompt: "base" }, true);
    if (result && result.message) deliveries.push({ sid, text: result.message.content });
  } else {
    result = await fire("before_prompt_build", sid, { prompt: "work", messages: [] });
    if (result && result.prependContext && result.prependContext.includes("[PUNK INBOX]")) deliveries.push({ sid, text: result.prependContext });
  }
  return result;
}
async function drive(sid) { return kind === "openclaw" ? turn(sid) : refresh(sid); }
async function stop() {
  if (kind === "opencode") await hooks.dispose();
  else if (kind === "pi") for (const sid of ["s1", "s2", "s3"]) await fire("session_shutdown", sid);
  else await fire("gateway_stop", "s1");
}
async function recoveryRecords() {
  const fs = await import("node:fs/promises");
  const dir = process.env.XDG_STATE_HOME + "/punk/inbox/opencode";
  try {
    const names = await fs.readdir(dir);
    return await Promise.all(names.filter((n) => n.endsWith(".json")).map(async (n) => JSON.parse(await fs.readFile(dir + "/" + n, "utf8"))));
  } catch (_) { return []; }
}
async function main() {
` // drivers close main and exit after stopping the host

func runPluginBindingHarness(t *testing.T, kind, pin string, env map[string]string, driver string) {
	t.Helper()
	output, err := execPluginBindingHarness(t, kind, pin, env, driver, nil)
	if err != nil {
		t.Fatalf("emitted %s plugin: %v\n%s", kind, err, output)
	}
	if !strings.Contains(output, "PASS binding regression") {
		t.Fatalf("missing success assertion: %s", output)
	}
}

// Mutations affect only the generated source in this test's temporary Node
// harness. Concurrent suites always see the unchanged production generators.
func execPluginBindingHarness(t *testing.T, kind, pin string, env map[string]string, driver string, mutate func(string) string) (string, error) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping emitted-plugin binding regression")
	}
	var source string
	switch kind {
	case "opencode":
		source = openCodePluginContent("http://punk.test")
	case "pi":
		source = piExtensionContentNS("http://punk.test", pin)
	case "openclaw":
		source = openClawPluginSource("http://punk.test")
	default:
		t.Fatal("unknown plugin: " + kind)
	}
	if mutate != nil {
		source = mutate(source)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "binding.mjs")
	script := source + "\nconst kind = " + jsStringLiteral(kind) + ";\n" + pluginBindingHarness + driver + `
  await stop();
  console.log("PASS binding regression");
  process.exit(0);
}
main().catch((err) => { console.error(err.stack || err); process.exit(1); });
`
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	defaults := map[string]string{
		"HOME": dir, "XDG_STATE_HOME": filepath.Join(dir, "state"),
		"PUNK_URL": "http://punk.test", "PUNK_MESSAGING": "1",
		"PUNK_MESSAGING_BACKOFF_MS": "10", "PUNK_MESSAGING_LEASE_SECONDS": "1",
	}
	for k, v := range env {
		defaults[k] = v
	}
	cmdEnv := pluginNodeEnv(dir, defaults)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, path)
	cmd.Env = cmdEnv
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("emitted %s plugin exceeded its deadline: %v\n%s", kind, ctx.Err(), output)
	}
	return string(output), err
}

func TestPluginBindingAddressNamespaces(t *testing.T) {
	for _, kind := range []string{"opencode", "pi", "openclaw"} {
		t.Run(kind, func(t *testing.T) {
			runPluginBindingHarness(t, kind, "", nil, `
  await initHost();
  for (const [sid, ns] of [["s1", "coord-a"], ["s2", "coord-b"], ["s3", cwdNS]]) {
    if (sid !== "s3") bindings.set(address(sid), ns);
    put(ns, sid, "same-id", "payload-" + sid);
    await start(sid);
    await until(() => calls.some((c) => c.path.endsWith("/members") && c.agent === address(sid)), "member attempt");
    must(hasMember(ns, sid), "F06: per-address binding must beat cwd, independently for " + sid);
    await drive(sid);
    await until(() => delivered("payload-" + sid), "correct namespace delivered");
    const lookups = calls.filter((c) => c.path === "/v1/agent/namespace" && c.agent === address(sid));
    must(lookups.length > 0 && lookups.every((c) => c.query.get("cwd")), "address and cwd accompany each lookup");
    const d = deliveries.find((d) => d.text.includes("payload-" + sid));
    must(d.sid === sid && d.text.includes(" in " + ns + "."), "envelope is bound to its own namespace/address");
    if (kind === "opencode") {
      const guidance = await turn(sid);
      must(guidance.system.join("\n").includes(address(sid) + " in namespace " + ns), "guidance uses this session's binding");
    }
  }
  must(deliveries.length === 3, "no cross-session duplicate suppression or delivery");
  for (const c of calls.filter((c) => c.path.endsWith("/messages/diagnostics"))) {
    must(c.ns === (bindings.get(c.agent) || cwdNS), "diagnostics use the reporting session namespace");
  }
`)
		})
	}
}

func TestPluginBindingRebindColdState(t *testing.T) {
	for _, kind := range []string{"opencode", "pi", "openclaw"} {
		t.Run(kind, func(t *testing.T) {
			runPluginBindingHarness(t, kind, "", map[string]string{"PUNK_MESSAGING_MAX_CONTINUE": "1"}, `
  await initHost();
  const id = "a".repeat(32);
  failAcks.add(cwdNS);
  const old = put(cwdNS, "s1", id, "old-namespace-payload");
  await start("s1");
  await until(() => hasMember(cwdNS, "s1"), "initial member");
  await drive("s1");
  await until(() => delivered("old-namespace-payload"), "old message handed off");
  if (kind !== "openclaw") await until(() => requests(cwdNS, "messages/ack").length > 0, "old ACK failed");
  const owner = requests(cwdNS, "messages").find((c) => c.query.get("leased_by")).query.get("leased_by");
  const oldStream = streams.find((s) => s.ns === cwdNS);
  bindings.set(address("s1"), "coord-new");
  put("coord-new", "s1", id, "new-namespace-payload");
  await drive("s1");
  await until(() => hasMember("coord-new", "s1"), "F06: next host event refreshes the binding");
  if (kind === "openclaw" && !delivered("new-namespace-payload")) await drive("s1");
  await until(() => delivered("new-namespace-payload"), "cold ACK set and wake budget permit same id in new namespace");
  must(deliveries.filter((d) => d.text.includes("new-namespace-payload")).length === 1, "new message delivered once");
  const newRead = requests("coord-new", "messages").find((c) => c.query.get("leased_by"));
  must(newRead && newRead.query.get("leased_by") !== owner, "new namespace has a fresh lease owner");
  await until(() => requests(cwdNS, "messages/release").some((c) => c.body.ids.includes(id)), "old lease released in OLD namespace");
  must(!old.acked && old.leasedUntil === 0, "old pending ACK was not migrated or marked received");
  if (oldStream) must(oldStream.signal.aborted, "old SSE aborted on rebind");
  const oldReads = requests(cwdNS, "messages").length, oldAcks = requests(cwdNS, "messages/ack").length;
  await sleep(1400);
  must(requests(cwdNS, "messages").length === oldReads && requests(cwdNS, "messages/ack").length === oldAcks, "old retry/reack timers cannot outlive the binding");
  if (kind === "opencode") {
    const records = await recoveryRecords();
    const a = records.find((r) => r.namespace === cwdNS), b = records.find((r) => r.namespace === "coord-new");
    must(a && a.pending_ids.includes(id), "old recovery evidence stays in old namespace");
    must(b && b.wake_times.length === 1 && !b.pending_ids.length, "new recovery record has only new handoff evidence");
  }
`)
		})
	}
}

func TestPluginBindingStaleFetch(t *testing.T) {
	for _, kind := range []string{"opencode", "pi", "openclaw"} {
		t.Run(kind, func(t *testing.T) {
			runPluginBindingHarness(t, kind, "", nil, `
  await initHost();
  await start("s1", true);
  await until(() => hasMember(cwdNS, "s1"), "initial member");
  const old = put(cwdNS, "s1", "stale", "stale-fetch-payload");
  const gate = hold((c) => c.path === route(cwdNS, "messages"));
  const pending = drive("s1");
  await until(() => gate.entered, "old fetch leased a row before responding");
  const owner = gate.call.query.get("leased_by");
  bindings.set(address("s1"), "coord-new");
  put("coord-new", "s1", "stale", "fresh-fetch-payload");
  await refresh("s1");
  await until(() => hasMember("coord-new", "s1"), "rebind while old read is in flight");
  gate.release();
  await pending;
  await drive("s1");
  await until(() => delivered("fresh-fetch-payload"), "new namespace delivered");
  await until(() => requests(cwdNS, "messages/release").some((c) => c.body.leased_by === owner && c.body.ids.includes("stale")), "late old read releases its old owner lease");
  must(!delivered("stale-fetch-payload") && !old.acked && old.leasedUntil === 0, "late old fetch is never delivered or ACKed");
  must(requests("coord-new", "messages/ack").every((c) => c.body.leased_by !== owner), "no old ACK owner in new namespace");
`)
		})
	}
}

func TestPluginBindingStaleAck(t *testing.T) {
	for _, kind := range []string{"opencode", "pi", "openclaw"} {
		t.Run(kind, func(t *testing.T) {
			runPluginBindingHarness(t, kind, "", nil, `
  await initHost();
  const id = "b".repeat(32);
  const gate = hold((c) => c.path === route(cwdNS, "messages/ack"));
  put(cwdNS, "s1", id, "old-ack-payload");
  await start("s1");
  await until(() => hasMember(cwdNS, "s1"), "initial member");
  await drive("s1");
  await until(() => gate.entered, "old ACK response held");
  bindings.set(address("s1"), "coord-new");
  await refresh("s1");
  await until(() => hasMember("coord-new", "s1"), "rebind during old ACK");
  gate.release();
  await sleep(30);
  put("coord-new", "s1", id, "new-ack-payload");
  await drive("s1");
  await until(() => delivered("new-ack-payload"), "late ACK cannot populate new recent-ACK set");
  must(deliveries.filter((d) => d.text.includes("new-ack-payload")).length === 1, "same id delivered exactly once in new namespace");
`)
		})
	}
}

func TestPluginBindingStaleResolutionAndRegistration(t *testing.T) {
	for _, kind := range []string{"opencode", "pi", "openclaw"} {
		for _, phase := range []string{"lookup", "registration"} {
			t.Run(kind+"/"+phase, func(t *testing.T) {
				path := "/v1/agent/namespace"
				if phase == "registration" {
					path = "/v1/namespaces/agent-cwd/members"
				}
				runPluginBindingHarness(t, kind, "", nil, `
  await initHost();
  const gate = hold((c) => c.path === `+jsStringLiteral(path)+`);
  await start("s1");
  await until(() => gate.entered, "old lookup/registration held");
  bindings.set(address("s1"), "coord-new");
  const pending = refresh("s1");
  await sleep(20);
  gate.release();
  await pending;
  await until(() => hasMember("coord-new", "s1"), "newer binding wins over old response");
  must(!streams.some((s) => s.ns === cwdNS), "stale registration cannot start an old SSE stream");
  must(requests(cwdNS, "messages").length === 0, "stale lookup/registration cannot start old delivery");
`)
			})
		}
	}
}

func TestPluginBindingPins(t *testing.T) {
	for _, kind := range []string{"opencode", "pi", "openclaw"} {
		t.Run(kind, func(t *testing.T) {
			runPluginBindingHarness(t, kind, "", map[string]string{"PUNK_NAMESPACE": "pinned"}, pluginBindingPinDriver)
		})
	}
	t.Run("pi-project", func(t *testing.T) {
		runPluginBindingHarness(t, "pi", "pinned", nil, pluginBindingPinDriver)
	})
	t.Run("pi-env-before-project", func(t *testing.T) {
		runPluginBindingHarness(t, "pi", "project-other", map[string]string{"PUNK_NAMESPACE": "pinned"}, pluginBindingPinDriver)
	})
}

const pluginBindingPinDriver = `
  await initHost();
  bindings.set(address("s1"), "other");
  put("pinned", "s1", "pin", "pinned-payload");
  await start("s1");
  await until(() => hasMember("pinned", "s1"), "pin wins");
  await drive("s1");
  await until(() => delivered("pinned-payload"), "pinned delivery");
  bindings.set(address("s1"), "rebound");
  await refresh("s1");
  await sleep(30);
  must(!calls.some((c) => c.path === "/v1/agent/namespace"), "pin skips all binding lookups");
  must(!hasMember("other", "s1") && !hasMember("rebound", "s1"), "server binding cannot override pin");
`

func TestPluginBindingReconnect(t *testing.T) {
	for _, kind := range []string{"opencode", "pi"} {
		t.Run(kind, func(t *testing.T) {
			runPluginBindingHarness(t, kind, "", nil, `
  await initHost();
  await start("s1");
  await until(() => streams.length > 0, "old stream connected");
  bindings.set(address("s1"), "coord-new");
  put("coord-new", "s1", "reconnect", "reconnect-payload");
  streams[0].close();
  await until(() => delivered("reconnect-payload"), "SSE reconnect refreshes binding without a host event");
  must(hasMember("coord-new", "s1"), "new namespace registration precedes reconnect delivery");
`)
		})
	}
}

// F07: zero representable messages is not a host handoff, even when the SDK
// would accept a synthetic text part containing an empty string.
func TestPluginBindingOpenCodeUnrenderable(t *testing.T) {
	runPluginBindingHarness(t, "opencode", "", map[string]string{
		"PUNK_MESSAGING_RENDER_BYTES": "1", "PUNK_MESSAGING_MAX_CONTINUE": "1",
	}, `
  await initHost();
  const row = put(cwdNS, "s1", "c".repeat(32), "render-cap-payload");
  await start("s1");
  await until(() => requests(cwdNS, "messages").length > 0, "inbox read");
  await sleep(100);
  must(deliveries.length === 0, "F07: an unrenderable message must not call session.prompt");
  must(!row.acked && requests(cwdNS, "messages/ack").length === 0, "unrepresented id is never ACKed");
  must(row.leasedUntil === 0 && requests(cwdNS, "messages/release").length > 0, "unrepresented lease is released");
  const diagnostic = requests(cwdNS, "messages/diagnostics").at(-1).body;
  must(diagnostic.state === "delivery_failed" && diagnostic.last_error === "render_cap", "render-cap observation is honest");
  must(diagnostic.wake_count === 0 && diagnostic.pending_ack_count === 0, "empty render spends no wake and has no handoff evidence");
  process.env.PUNK_MESSAGING_RENDER_BYTES = "32768";
  await drive("s1");
  await until(() => row.acked, "restored budget delivers without a spent wake slot");
  must(deliveries.length === 1 && delivered("render-cap-payload"), "only representable text was delivered");
`)
}

// F04: the existing baked project pin already drives Pi's native tools;
// capture and memory recall must target that same project, independently
// of an inbox coordination binding.
func TestPluginBindingPiProjectMemoryPin(t *testing.T) {
	runPluginBindingHarness(t, "pi", "project-pin", map[string]string{"PUNK_MESSAGING": "0"}, `
  await initHost();
  bindings.set(address("s1"), "coord-other");
  await start("s1");
  await fire("input", "s1", { text: "work", source: "interactive" });
  await fire("tool_result", "s1", { toolName: "read", toolCallId: "c1", content: "result" });
  await fire("agent_settled", "s1");
  const result = await turn("s1");
  const who = await tools.get("punk_whoami").execute("tool", {}, undefined, undefined, ctx("s1"));
  must(JSON.parse(who.content[0].text).namespace === "project-pin", "native tool retains existing pin");
  const captures = calls.filter((c) => c.path === "/v1/agent/hooks");
  must(captures.length === 4 && captures.every((c) => c.query.get("ns") === "project-pin"), "F04: all captures honor baked project pin");
  must(calls.filter((c) => c.path === "/v1/agent/context").every((c) => c.query.get("ns") === "project-pin"), "context uses baked project pin");
  must(result.systemPrompt === "base\n\nmemory for project-pin", "pinned memory is injected into the actual Pi return value");
  must(!calls.some((c) => c.path === "/v1/agent/namespace"), "project memory pin needs no inbox binding lookup");
`)
}

func TestPluginBindingLookupOutageIsolation(t *testing.T) {
	for _, kind := range []string{"opencode", "pi", "openclaw"} {
		t.Run(kind, func(t *testing.T) {
			runPluginBindingHarness(t, kind, "", nil, `
  await initHost();
  failLookups.add(address("s1"));
  bindings.set(address("s1"), "coord-a");
  bindings.set(address("s2"), "coord-b");
  await start("s1");
  if (kind === "opencode") await turn("s1");
  put("coord-b", "s2", "healthy", "healthy-session-payload");
  await start("s2");
  await until(() => hasMember("coord-b", "s2"), "one session's failed lookup cannot contaminate another");
  await drive("s2");
  await until(() => delivered("healthy-session-payload"), "healthy session still delivers");
  must(!hasMember(cwdNS, "s1") && !hasMember("coord-b", "s1"), "failed binding lookup cannot guess a namespace");
  if (kind === "opencode") {
    const guidance = await turn("s2");
    must(guidance.system.join("\n").includes("coord-b"), "failed guidance negative cache is per session");
  }
  failLookups.delete(address("s1"));
  await until(() => hasMember("coord-a", "s1"), "failed resolution autonomously recovers in its bound namespace");
`)
		})
	}
}

func TestPluginBindingOpenCodeStalePrompt(t *testing.T) {
	runPluginBindingHarness(t, "opencode", "", map[string]string{"PUNK_MESSAGING_MAX_CONTINUE": "1"}, `
  await initHost();
  const oldId = "d".repeat(32), newId = "e".repeat(32);
  const gate = hold((c) => c.path === "prompt" && c.body.text.includes("old-prompt-payload"));
  const old = put(cwdNS, "s1", oldId, "old-prompt-payload");
  await start("s1");
  await until(() => gate.entered, "old SDK handoff in flight");
  bindings.set(address("s1"), "coord-new");
  const fresh = put("coord-new", "s1", newId, "new-prompt-payload");
  await refresh("s1");
  await until(() => fresh.acked, "new state can hand off while old SDK request is pending");
  gate.release();
  await sleep(50);
  must(!old.acked && old.leasedUntil === 0, "late old SDK success cannot ACK or retain old lease");
  must(requests("coord-new", "messages/ack").every((c) => !c.body.ids.includes(oldId)), "old handoff id cannot ACK in the new namespace");
  const rec = (await recoveryRecords()).find((r) => r.namespace === "coord-new");
  must(rec && !rec.pending_ids.includes(oldId) && rec.wake_times.length === 1, "late old prompt cannot save old evidence into new recovery record");
`)
}

func TestPluginBindingOpenCodeStaleRecovery(t *testing.T) {
	runPluginBindingHarness(t, "opencode", "", map[string]string{
		"PUNK_MESSAGING_RESTORE_DELAY_MS": "120", "PUNK_MESSAGING_MAX_CONTINUE": "1",
	}, `
  const fs = await import("node:fs/promises"), crypto = await import("node:crypto");
  const agent = address("s1"), id = "f".repeat(32);
  const stateDir = process.env.XDG_STATE_HOME + "/punk/inbox/opencode";
  await fs.mkdir(stateDir, { recursive: true });
  const hash = crypto.createHash("sha256").update(["http://punk.test", cwdNS, agent].join(String.fromCharCode(0))).digest("hex").slice(0, 16);
  await fs.writeFile(stateDir + "/opencode_s1-" + hash + ".json", JSON.stringify({
    server: "http://punk.test", namespace: cwdNS, address: agent, pending_ids: [id], wake_times: [Date.now()], saved_at: Date.now(),
  }));
  await initHost();
  await start("s1");
  await until(() => hasMember(cwdNS, "s1"), "old registration before delayed restore");
  bindings.set(agent, "coord-new");
  const row = put("coord-new", "s1", id, "recovery-new-payload");
  await refresh("s1");
  await until(() => row.acked, "new binding restores independently of old delayed recovery");
  must(deliveries.length === 1 && delivered("recovery-new-payload"), "old pending ids and spent wakes do not suppress new message");
  // ACK acceptance precedes the intentionally asynchronous post-ACK save.
  // Wait for that disk boundary, preserving the exact isolation assertions.
  await until(async () => {
    const rec = (await recoveryRecords()).find((r) => r.namespace === "coord-new");
    return rec && rec.wake_times.length === 1 && !rec.pending_ids.length;
  }, "new recovery record has only its own evidence");
`)
}

func TestPluginBindingPiCoordinationDoesNotMoveMemory(t *testing.T) {
	runPluginBindingHarness(t, "pi", "", nil, `
  await initHost();
  bindings.set(address("s1"), "coord-only");
  await start("s1", true);
  await until(() => hasMember("coord-only", "s1"), "coordination bound");
  const result = await turn("s1");
  const who = await tools.get("punk_whoami").execute("tool", {}, undefined, undefined, ctx("s1"));
  must(JSON.parse(who.content[0].text).namespace === cwdNS, "un-pinned native memory retains cwd namespace");
  must(result.systemPrompt === "base\n\nmemory for " + cwdNS, "coordination binding does not redirect memory recall");
  must(calls.filter((c) => c.path === "/v1/agent/hooks").every((c) => !c.query.has("ns")), "un-pinned capture retains cwd routing");
`)
}

func TestPluginBindingOpenCodeMissingSDK(t *testing.T) {
	runPluginBindingHarness(t, "opencode", "", nil, `
  hooks = await PunkMemoryPlugin({ directory: "/project", client: { session: {} } });
  bindings.set(address("s1"), "coord-a");
  const result = await turn("s1");
  await sleep(30);
  must(result.system.some((s) => s.includes("memory for " + cwdNS)), "memory still injects without messaging SDK");
  must(!calls.some((c) => c.ns), "missing SDK must not register or listen through the guidance refresh path");
`)
}

var pluginBindingDisposalCases = []struct {
	kind string
	mode string
}{
	{"opencode", "session_deleted"},
	{"opencode", "dispose"},
	{"pi", "session_shutdown"},
	{"openclaw", "gateway_stop"},
}

// codeops:trace repo=punk-records work_item=punk-agent-refresh-20261009 spec=docs/agent-messaging.md plan=plugin-namespace test=TestPluginBindingDisposedRebind,TestPluginBindingDisposedRebindRejectsRemovedGuards
// QA F3/M7: a fresh binding cannot revive a retired identity through the
// shared resolver. Pi shutdown and OpenCode deletion are session-scoped;
// OpenCode dispose and OpenClaw gateway_stop retire the whole plugin.
const pluginBindingDisposedRebindDriver = `
  await initHost();
  bindings.set(address("s1"), "coord-old");
  failAcks.add("coord-old");
  const old = put("coord-old", "s1", "1".repeat(32), "before-disposal-payload");
  await start("s1");
  await until(() => hasMember("coord-old", "s1"), "original member registered");
  await drive("s1");
  await until(() => delivered("before-disposal-payload"), "original host handoff");
  if (kind !== "openclaw") await until(() => requests("coord-old", "messages/ack").length > 0, "failed ACK leaves a reack timer pending");

  // Keep a real shared-resolver request in flight across disposal. The fake
  // deliberately returns its old response even when its signal is aborted.
  const lookup = hold((c) => c.path === "/v1/agent/namespace" && c.agent === address("s1"));
  await refresh("s1");
  await until(() => lookup.entered, "namespace refresh in flight");
  const oldStreams = streams.slice();
  // Queue stream termination before disposal, exercising any reconnect
  // continuation that was already scheduled. OpenClaw has no SSE contract.
  for (const stream of oldStreams) stream.close();
  if (kind === "opencode") {
    if (shutdownMode === "session_deleted") {
      await hooks.event({ event: { type: "session.deleted", properties: { info: { id: "s1" } } } });
    } else await hooks.dispose();
  } else if (kind === "pi") await fire("session_shutdown", "s1", { reason: "new" });
  else await fire("gateway_stop", "s1");

  const cutoff = calls.length, handoffs = deliveries.length;
  must(lookup.call.signal.aborted, "disposal aborts the in-flight namespace lookup");
  must(oldStreams.every((s) => s.signal.aborted), "disposal aborts old SSE connections");
  const globalShutdown = shutdownMode === "dispose" || shutdownMode === "gateway_stop";
  const retiredIDs = globalShutdown ? ["s1", "s2"] : ["s1"];
  for (const sid of retiredIDs) {
    bindings.set(address(sid), "coord-after-disposal");
    put("coord-after-disposal", sid, "2".repeat(32), "after-disposal-" + sid);
  }
  failAcks.delete("coord-old");
  lookup.release();
  // Replaying every supported binding/content event must not recreate state,
  // including a never-before-seen session after a plugin-wide shutdown.
  for (const sid of retiredIDs) {
    await start(sid);
    await refresh(sid);
    await turn(sid);
    if (kind === "opencode") {
      await hooks["chat.message"]({ sessionID: sid, messageID: "late-user" }, {
        message: { role: "user" }, parts: [{ type: "text", text: "late host event" }],
      });
    }
  }
  // Longer than the test lease plus the shared reack delay (1000 + 250 ms),
  // and the reconnect backoff, so surviving timers cannot hide in the test.
  await sleep(1400);
  const messagingAfter = () => calls.slice(cutoff).filter((c) =>
    c.path === "/v1/agent/namespace" || (c.ns && !c.path.endsWith("/messages/release")));
  must(messagingAfter().length === 0, "disposed session must not restart messaging");
  must(retiredIDs.every((sid) => !hasMember("coord-after-disposal", sid)), "no fresh membership after disposal and rebind");
  must(deliveries.length === handoffs, "no prompt/enqueue/context delivery after disposal");
  must(streams.length === oldStreams.length, "no new SSE stream after disposal");
  must(!old.acked, "retired reack work never acknowledges the old message");
  for (const sid of retiredIDs) {
    must((inboxes.get(keyOf("coord-after-disposal", address(sid))) || []).every((r) => !r.acked && r.leasedUntil === 0), "new binding's messages stay unleased and unacknowledged");
  }
  // Releases are legitimate retirement cleanup, but can only target the
  // old identity. Unrelated memory capture/context requests are permitted.
  must(calls.slice(cutoff).filter((c) => c.path.endsWith("/messages/release")).every((c) => c.ns === "coord-old"), "cleanup never releases in the new binding");
  must(calls.slice(cutoff).some((c) => c.path === "/v1/agent/hooks"), "observational capture is independent of messaging disposal");

  if (!globalShutdown) {
    // Positive control: per-session shutdown must not disable a different
    // session in the same Pi runtime / OpenCode plugin instance.
    bindings.set(address("s2"), "coord-still-live");
    put("coord-still-live", "s2", "3".repeat(32), "other-session-still-live");
    await start("s2");
    await until(() => hasMember("coord-still-live", "s2"), "different session can still register");
    await drive("s2");
    await until(() => delivered("other-session-still-live"), "different session can still receive");
    must(!messagingAfter().some((c) => c.agent === address("s1")), "retired session stays retired while another session runs");
  }
`

func TestPluginBindingDisposedRebind(t *testing.T) {
	for _, tc := range pluginBindingDisposalCases {
		t.Run(tc.kind+"/"+tc.mode, func(t *testing.T) {
			driver := "const shutdownMode = " + jsStringLiteral(tc.mode) + ";\n" + pluginBindingDisposedRebindDriver
			runPluginBindingHarness(t, tc.kind, "", nil, driver)
		})
	}
}

// The same behavioral regression must kill a removed-lifecycle-guard mutant
// for every host. Check the specific runtime assertion: syntax failures,
// timeouts, or unrelated setup failures do not count as mutation evidence.
func TestPluginBindingDisposedRebindRejectsRemovedGuards(t *testing.T) {
	for _, tc := range pluginBindingDisposalCases {
		t.Run(tc.kind+"/"+tc.mode, func(t *testing.T) {
			mutate := func(source string) string {
				old, replacement := " || punkDeletedSessions.has(sessionID)", ""
				switch tc.mode {
				case "dispose":
					old, replacement = "punkDisposed = true", "punkDisposed = false"
				case "gateway_stop":
					old = " || punkDisposed"
				}
				if !strings.Contains(source, old) {
					t.Fatalf("lifecycle mutation target missing: %q", old)
				}
				return strings.ReplaceAll(source, old, replacement)
			}
			driver := "const shutdownMode = " + jsStringLiteral(tc.mode) + ";\n" + pluginBindingDisposedRebindDriver
			output, err := execPluginBindingHarness(t, tc.kind, "", nil, driver, mutate)
			if err == nil || !strings.Contains(output, "Error: disposed session must not restart messaging") {
				t.Fatalf("removed lifecycle guard must fail the post-disposal messaging assertion; err=%v\n%s", err, output)
			}
			t.Log("mutation rejected: disposed session must not restart messaging")
		})
	}
}
