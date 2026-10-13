// CREDS-1..5 runtime regression driver. Only synthetic files and loopback IO.
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { syncBuiltinESMExports } from "node:module";

const { kind, scenario, server, plugin } = JSON.parse(process.env.PUNK_TEST_CASE);
const home = process.env.HOME;
assert.equal(process.env.USERPROFILE, home);
assert.equal(path.dirname(plugin), home);
const savedKey = "synthetic-saved-key", envKey = "synthetic-env-key";
const installed = server + "/installed", alternate = server + "/alternate";
let expectedURL = installed, expectedKey = savedKey, disabled = false, bypass = false, mismatch = false;
let selected = path.join(home, ".punk", "credentials.json");
fs.mkdirSync(path.dirname(selected), { recursive: true });
let saved = { url: installed + "///", api_key: savedKey };
let raw;
delete process.env.PUNK_CREDENTIALS;
process.env.PUNK_MESSAGING = "1";
process.env.PUNK_MESSAGING_BACKOFF_MS = "10";
process.env.PUNK_MESSAGING_CONNECT_TIMEOUT_MS = "1500";
process.env.PUNK_MESSAGING_IDLE_TIMEOUT_MS = "5000";

switch (scenario) {
  case "custom-file": case "custom-missing":
    fs.writeFileSync(selected, "default-file-must-not-be-read");
    selected = path.join(home, "custom.json");
    process.env.PUNK_CREDENTIALS = selected;
    if (scenario === "custom-missing") { saved = undefined; expectedKey = ""; }
    break;
  case "saved-url-fallback": saved.url = alternate; expectedURL = alternate; break;
  case "missing-installed": saved = undefined; expectedKey = ""; break;
  case "missing-local": saved = undefined; expectedURL = "http://localhost:9090"; expectedKey = ""; break;
  case "empty-env": process.env.PUNK_URL = ""; process.env.PUNK_API_KEY = ""; process.env.PUNK_CREDENTIALS = ""; break;
  case "env-key": process.env.PUNK_API_KEY = envKey; expectedKey = envKey; break;
  case "runtime-url-match": saved.url = alternate; process.env.PUNK_URL = alternate + "///"; expectedURL = alternate; break;
  case "runtime-url-mismatch": process.env.PUNK_URL = alternate; expectedURL = alternate; expectedKey = ""; mismatch = true; break;
  case "installed-url-mismatch": saved.url = alternate; expectedKey = ""; mismatch = true; break;
  case "explicit-bypass":
    process.env.PUNK_URL = alternate; process.env.PUNK_API_KEY = envKey;
    expectedURL = alternate; expectedKey = envKey; raw = "invalid-unused-synthetic-secret"; bypass = true; break;
  case "env-key-invalid-file": process.env.PUNK_API_KEY = envKey; raw = "invalid-synthetic-secret"; disabled = true; break;
  case "snapshot": case "two-instances":
    process.env.PUNK_URL = installed; process.env.PUNK_API_KEY = envKey; expectedKey = envKey; bypass = true; break;
  case "canonical-host":
    saved.url = installed.replace("127.0.0.1", "LOCALHOST");
    expectedURL = installed.replace("127.0.0.1", "localhost"); process.env.PUNK_URL = expectedURL + "///"; break;
  case "canonical-http-port": saved.url = "HTTP://LOCALHOST:80/mounted///"; expectedURL = "http://localhost/mounted"; process.env.PUNK_URL = expectedURL; break;
  case "canonical-https-port": saved.url = "HTTPS://LOCALHOST:443/mounted///"; expectedURL = "https://localhost/mounted"; process.env.PUNK_URL = expectedURL; break;
  case "different-scheme": saved.url = "https://localhost/mounted"; expectedURL = "http://localhost/mounted"; process.env.PUNK_URL = expectedURL; expectedKey = ""; mismatch = true; break;
  case "different-port": saved.url = "http://localhost:81/mounted"; expectedURL = "http://localhost/mounted"; process.env.PUNK_URL = expectedURL; expectedKey = ""; mismatch = true; break;
  case "invalid-json": raw = '{"api_key":"invalid-synthetic-secret",'; break;
  case "invalid-null": raw = "null"; break;
  case "invalid-array": raw = "[]"; break;
  case "invalid-scalar": raw = '"invalid-synthetic-secret"'; break;
  case "invalid-url-missing": delete saved.url; break;
  case "invalid-url-empty": saved.url = ""; break;
  case "invalid-url-type": saved.url = 7; break;
  case "invalid-key-type": saved.api_key = ["invalid-synthetic-secret"]; break;
  case "invalid-key-null": saved.api_key = null; break;
  case "invalid-unreadable": fs.mkdirSync(selected); saved = undefined; break;
  case "invalid-userinfo": saved.url = "http://name:invalid-synthetic-secret@localhost/"; break;
  case "invalid-query": saved.url = installed + "?token=invalid-synthetic-secret"; break;
  case "invalid-fragment": saved.url = installed + "#invalid-synthetic-secret"; break;
  case "invalid-empty-query": saved.url = installed + "?"; break;
  case "invalid-empty-fragment": saved.url = installed + "#"; break;
  case "invalid-relative": saved.url = "/relative"; break;
  case "invalid-scheme": saved.url = "ftp://localhost/memory"; break;
  case "invalid-host": saved.url = "http:///missing-host"; break;
  case "invalid-port": saved.url = "http://localhost:65536"; break;
  case "invalid-backslash": saved.url = "http://localhost\\other/memory"; break;
  case "invalid-whitespace": saved.url = " " + installed; break;
  case "invalid-escape": saved.url = installed + "/%zz"; break;
  case "invalid-selected-url": process.env.PUNK_URL = "http://name:invalid-synthetic-secret@localhost"; break;
}
if (scenario.startsWith("invalid-")) disabled = true;
if (raw !== undefined || saved !== undefined) fs.writeFileSync(selected, raw ?? JSON.stringify(saved));

// Guard exported builtin reads before importing the emitted ESM. The loader
// reads the plugin in this temp home too. Recovery IO stays under this home.
const reads = [];
function guardRead(file) {
  const p = file instanceof URL ? fileURLToPath(file) : String(file);
  assert.ok(path.resolve(p).startsWith(home + path.sep), "file read escaped synthetic HOME");
  if (p === selected || p.endsWith("credentials.json") || p.endsWith("custom.json")) reads.push(p);
}
const readSync = fs.readFileSync.bind(fs), readAsync = fs.promises.readFile.bind(fs.promises);
fs.readFileSync = (file, ...args) => { guardRead(file); return readSync(file, ...args); };
fs.promises.readFile = (file, ...args) => { guardRead(file); return readAsync(file, ...args); };
syncBuiltinESMExports();
const errors = [];
console.error = (...args) => errors.push(args.map(String).join(" "));
const calls = [], violations = [];
let pendingRequests = 0;
const realFetch = globalThis.fetch;
// Default-port/local-fallback checks preserve the requested URL in calls but
// forward it to the ephemeral loopback listener (never bind to a live :80/:9090).
const routed = ["missing-local", "canonical-host", "canonical-http-port", "canonical-https-port", "different-scheme", "different-port"].includes(scenario);
globalThis.fetch = async (url, init = {}) => {
  const u = new URL(String(url));
  const auth = new Headers(init.headers).get("authorization") || "";
  calls.push({ url: u.href, path: u.pathname, auth });
  if (disabled) violations.push("invalid credentials attempted Punk IO");
  if (!u.href.startsWith(expectedURL + "/v1/")) violations.push("request did not use the selected startup URL");
  if (auth !== (expectedKey ? "Bearer " + expectedKey : "")) violations.push("request did not use the selected startup key");
  if ((!routed && u.origin !== server) || (routed && u.hostname !== "localhost")) {
    violations.push("non-loopback destination rejected");
    throw new Error("test destination guard");
  }
  if (u.pathname.endsWith("/events")) return realFetch(routed ? server + u.pathname + u.search : u.href, init);
  pendingRequests++;
  try {
    const res = await realFetch(routed ? server + u.pathname + u.search : u.href, init);
    // Buffer this tiny fixture response before returning it, so the harness
    // can wait for observational requests to finish before host teardown.
    return new Response(await res.arrayBuffer(), { status: res.status, headers: res.headers });
  } finally { pendingRequests--; }
};
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
async function until(pred, label) {
  const end = Date.now() + 2500;
  while (Date.now() < end) { if (pred()) return; await sleep(5); }
  assert.fail("timed out: " + label);
}
async function settled() {
  await until(() => pendingRequests === 0, "observational requests completed");
  await sleep(20);
  await until(() => pendingRequests === 0, "follow-up requests completed");
}
const ctx = (sid) => ({ cwd: home, sessionId: sid, sessionManager: { getSessionId: () => sid }, isIdle: () => true });
let mod;
const hosts = [];
async function startHost() {
  const handlers = new Map(), tools = new Map();
  let hooks;
  if (kind === "opencode") {
    hooks = await mod.PunkMemoryPlugin({ directory: home, client: { session: {
      list: async () => ({ data: [] }), status: async () => ({ data: {} }), prompt: async () => ({ data: {} }),
    } } });
  } else if (kind === "pi") {
    await mod.default({ on: (n, f) => handlers.set(n, f), registerTool: (t) => tools.set(t.name, t), sendMessage: () => {} });
  } else {
    await mod.default.register({ on: (n, f) => handlers.set(n, f) });
  }
  const sids = [];
  const fire = (name, sid, e = {}) => handlers.get(name)(e, ctx(sid));
  const host = {
    async exercise(sid) {
      sids.push(sid);
      const before = calls.length;
      if (kind === "opencode") await hooks.event({ event: { type: "session.created", properties: { info: { id: sid } } } });
      else await fire("session_start", sid);
      if (!disabled) {
        await until(() => calls.slice(before).some((c) => c.path.endsWith("/members")), "membership request");
        await sleep(15);
      }
      let result;
      if (kind === "opencode") {
        result = { system: ["host prompt"] };
        await hooks["experimental.chat.system.transform"]({ sessionID: sid }, result);
        await hooks["tool.execute.after"]({ sessionID: sid, tool: "read", callID: "call", args: {} }, { output: "fixture" });
      } else if (kind === "pi") {
        result = await fire("before_agent_start", sid, { systemPrompt: "host prompt" });
        await fire("tool_result", sid, { toolName: "read", toolCallId: "call", input: {}, content: [] });
        for (const [name, params] of [["punk_whoami", {}], ["punk_recall", { prefix: "/" }], ["punk_search", { query: "fixture" }], ["punk_remember", { key: "/fixture", body: "test" }]]) {
          const run = () => tools.get(name).execute("call", params, undefined, undefined, ctx(sid));
          if (disabled) await assert.rejects(run, /credential|disabled/i, "disabled Pi tools report a sanitized error");
          else assert.ok((await run()).content.length > 0, "Pi native tool returns a host result");
        }
      } else {
        result = await fire("before_prompt_build", sid, { prompt: "fixture", messages: [] });
        await fire("after_tool_call", sid, { toolName: "read", params: {}, result: "fixture" });
      }
      if (disabled) {
        assert.ok(!JSON.stringify(result)?.includes("fixture memory context"), "invalid credentials do not inject memory");
        await sleep(40);
      } else {
        assert.ok(JSON.stringify(result).includes("fixture memory context"), "context reached the host");
        await until(() => calls.slice(before).some((c) => c.path.endsWith("/messages")), "inbox request");
        if (kind !== "openclaw") await until(() => calls.slice(before).some((c) => c.path.endsWith("/events")), "SSE request");
        else assert.ok(!calls.some((c) => c.path.endsWith("/events")), "OpenClaw remains catch-up only");
      }
    },
    async stop() {
      if (kind === "opencode") await hooks.dispose();
      else if (kind === "pi") for (const sid of sids) await fire("session_shutdown", sid);
      else await fire("gateway_stop", sids[0]);
    },
  };
  hosts.push(host);
  return host;
}

async function main() {
  const generated = readSync(plugin, "utf8");
  assert.ok(![savedKey, envKey, "invalid-synthetic-secret"].some((key) => generated.includes(key)), "generated plugin must not embed credentials");
  mod = await import(pathToFileURL(plugin));
  const host = await startHost();
  await host.exercise("first");
  if (["snapshot", "saved-snapshot", "two-instances"].includes(scenario)) {
    fs.writeFileSync(selected, JSON.stringify({ url: alternate, api_key: "changed-synthetic-key" }));
    if (scenario !== "saved-snapshot") {
      process.env.PUNK_URL = alternate;
      process.env.PUNK_API_KEY = "changed-synthetic-key";
      process.env.PUNK_CREDENTIALS = path.join(home, "later-file-must-not-be-read.json");
    }
    await host.exercise("second");
    if (scenario === "two-instances") {
      // Tear down old host IO before checking a new registration's pair. The
      // module is cached: each factory/register invocation must resolve anew.
      await settled();
      await host.stop();
      expectedURL = alternate; expectedKey = "changed-synthetic-key";
      const second = await startHost();
      await second.exercise("third");
    }
  }
  await settled();
  for (const host of hosts) await host.stop();
  await sleep(20);
  assert.deepEqual([...new Set(violations)], [], "all capture/context/inbox/SSE/tools use one valid pair");
  if (disabled) {
    assert.equal(calls.length, 0, "invalid credentials disable every Punk request");
    assert.equal(errors.length, 1, "one actionable startup diagnostic, no per-hook noise");
    assert.match(errors[0], /credential|URL/i);
  } else {
    assert.ok(calls.some((c) => c.path.endsWith("/hooks")), "capture reached the server");
    if (kind === "pi") assert.ok(calls.some((c) => c.path.endsWith("/memories")), "Pi tools reached the server");
    if (mismatch) assert.ok(errors.some((s) => /match|different|saved key/i.test(s)), "saved-key mismatch is diagnosed");
    else assert.deepEqual(errors, [], "valid startup has no credential/runtime errors");
  }
  assert.ok(!errors.some((s) => /synthetic-|http[s]?:|credentials\.json|custom\.json/.test(s)), "diagnostics must not expose keys, URLs, contents or paths");
  if (bypass) assert.equal(reads.length, 0, "fully explicit runtime pair bypasses the unused file");
  else if (scenario !== "invalid-selected-url") assert.deepEqual(reads, [selected], "read only the selected credential file, exactly once");
  console.log("PASS credentials " + kind + "/" + scenario + " requests=" + calls.length + " selected-file-reads=" + reads.length + " isolated-home=true");
}
main().then(() => process.exit(0)).catch(async (err) => {
  for (const host of hosts) { try { await host.stop(); } catch {} }
  process.stderr.write(String(err.stack || err) + "\n");
  process.exit(1);
});
