// Focused tests for the operator-console delivery diagnostics projection
// in console.js: remote-string escaping, fresh/stale rendering, the
// "last reported" (non-authoritative) wording, and the legacy-server
// (404) fallback. console.js is a plain browser script, so it is
// evaluated once in a vm sandbox with minimal DOM stubs; the CommonJS-ish
// seam at the bottom of console.js re-exports the helpers under test.

import { test, before } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import vm from 'node:vm';

const here = dirname(fileURLToPath(import.meta.url));
const src = readFileSync(join(here, 'console.js'), 'utf8');

function makeEl() {
  return {
    innerHTML: '', textContent: '', value: '', checked: false, disabled: false,
    dataset: {}, style: {},
    classList: { add() {}, remove() {}, toggle() {}, contains: () => false },
    addEventListener() {}, appendChild() {}, setAttribute() {},
    querySelector: () => makeEl(), querySelectorAll: () => [],
    closest: () => null, scrollIntoView() {},
  };
}

let sandbox;
let c; // console.js test-seam exports
let fetchCalls; // paths the stubbed fetch was asked for

function okJSON(body) {
  return { ok: true, status: 200, json: async () => body, text: async () => JSON.stringify(body) };
}

before(async () => {
  fetchCalls = [];
  const store = new Map();
  sandbox = {
    module: { exports: {} },
    console,
    AbortController,
    TextDecoder,
    document: {
      querySelector: () => makeEl(),
      querySelectorAll: () => [],
      createElement: () => makeEl(),
      getElementById: () => makeEl(),
      addEventListener() {},
    },
    window: { innerWidth: 1280, matchMedia: () => ({ matches: true }) },
    localStorage: {
      getItem: k => (store.has(k) ? store.get(k) : null),
      setItem: (k, v) => store.set(k, String(v)),
    },
    location: { hash: '' },
    history: { replaceState() {} },
    setInterval: () => 1,
    clearInterval() {},
    setTimeout: () => 1,
    clearTimeout() {},
    fetch: async path => { fetchCalls.push(path); return okJSON({ namespaces: [] }); },
  };
  vm.runInNewContext(src, sandbox, { filename: 'console.js' });
  c = sandbox.module.exports;
  assert.ok(c && c.diagnosticsHTML, 'console.js test seam did not populate module.exports');
  // Let the initial refreshAll() microtasks settle before tests run.
  await new Promise(r => setImmediate(r));
  fetchCalls.length = 0;
});

test('esc escapes markup metacharacters and stringifies nullish', () => {
  assert.equal(c.esc('<img src="x">&'), '&lt;img src=&quot;x&quot;&gt;&amp;');
  assert.equal(c.esc(null), '');
  assert.equal(c.esc(42), '42');
});

test('diagWords renders machine tokens for operators', () => {
  assert.equal(c.diagWords('waiting_for_idle'), 'waiting for idle');
  assert.equal(c.diagWords(''), '');
});

test('relEta projects future times and rejects junk', () => {
  assert.equal(c.relEta(new Date(Date.now() + 330000).toISOString()), 'in 5m');
  assert.equal(c.relEta(new Date(Date.now() - 60000).toISOString()), 'now');
  assert.equal(c.relEta('not a date'), null);
  assert.equal(c.relEta(''), null);
});

test('diagnosticsHTML escapes every remote string field', () => {
  const html = c.diagnosticsHTML({
    agent: 'opencode:evil',
    client: '<img src=x onerror=alert(1)>',
    delivery_mode: 'idle_wake',
    state: 'waiting_for_idle',
    last_error: 'prompt_failed"><script>alert(2)</script>',
    next_attempt_at: new Date(Date.now() + 330000).toISOString(),
    pending_ack_count: 2,
    updated_at: new Date().toISOString(),
    stale: false,
  });
  assert.ok(!html.includes('<img'), 'client name must be escaped');
  assert.ok(!html.includes('<script>'), 'last_error must be escaped');
  assert.ok(html.includes('&lt;img'), 'client name appears escaped');
  assert.ok(html.includes('waiting for idle'));
  assert.ok(html.includes('idle wake'), 'capability shows client + delivery mode');
  assert.ok(html.includes('retry in 5m'));
  assert.ok(html.includes('2 unacked'));
  assert.ok(html.includes('last reported'));
  assert.ok(!html.includes('(stale)'));
});

test('diagnosticsHTML marks stale snapshots as last-reported observations', () => {
  const html = c.diagnosticsHTML({
    agent: 'claude-code:s1',
    client: 'claude-code',
    delivery_mode: 'catch_up',
    state: 'delivery_failed',
    last_error: 'prompt_failed',
    updated_at: new Date(Date.now() - 300000).toISOString(),
    stale: true,
  });
  assert.ok(html.includes('(stale)'));
  assert.ok(html.includes('last reported'));
  assert.ok(html.includes('delivery failed'));
  // Stale observations dim rather than keep the live failure colour.
  assert.ok(!html.includes('text-brand'), 'stale failure must not look like a fresh alert');
});

test('diagnosticsHTML never claims liveness or ACK authority', () => {
  const html = c.diagnosticsHTML({ agent: 'a:b', state: 'ready', client: 'opencode', delivery_mode: 'idle_wake', updated_at: new Date().toISOString() });
  assert.ok(!html.includes('listening'), 'diagnostics never render liveness words');
  assert.ok(!/\backed\b/.test(html.replace('unacked', '')), 'diagnostics never claim ACK state');
});

test('diagnosticsHTML renders a benign fallback without a snapshot', () => {
  assert.ok(c.diagnosticsHTML(null).includes('no reports'));
  assert.ok(c.diagnosticsHTML(undefined).includes('no reports'));
});

test('fetchDiagnostics maps snapshots by agent and skips malformed rows', async () => {
  c.setNS('proj ns');
  c.setDiagUnsupported(false);
  sandbox.fetch = async path => {
    fetchCalls.push(path);
    return okJSON({ diagnostics: [{ agent: 'opencode:s1', state: 'ready' }, { state: 'broken' }, null] });
  };
  const map = await c.fetchDiagnostics();
  assert.deepEqual(Object.keys(map), ['opencode:s1']);
  assert.equal(map['opencode:s1'].state, 'ready');
  assert.equal(fetchCalls[0], '/v1/namespaces/proj%20ns/messages/diagnostics');
});

test('fetchDiagnostics degrades to empty on a legacy 404 and stops asking', async () => {
  c.setNS('ns1');
  c.setDiagUnsupported(false);
  sandbox.fetch = async () => ({ ok: false, status: 404, text: async () => 'not found' });
  const map = await c.fetchDiagnostics();
  assert.deepEqual(Object.keys(map), []);
  assert.equal(c.getDiagUnsupported(), true);
  const calls = fetchCalls.length;
  assert.deepEqual(Object.keys(await c.fetchDiagnostics()), [], 'unsupported server keeps returning empty');
  assert.equal(fetchCalls.length, calls, 'no further requests once the route is known missing');
});

test('fetchDiagnostics returns null on a transient failure so callers keep old data', async () => {
  c.setNS('ns1');
  c.setDiagUnsupported(false);
  sandbox.fetch = async () => ({ ok: false, status: 500, text: async () => 'boom' });
  assert.equal(await c.fetchDiagnostics(), null);
  assert.equal(c.getDiagUnsupported(), false, 'a 500 never marks the server legacy');
});

test('fetchDiagnostics is inert without a selected namespace', async () => {
  c.setNS('');
  c.setDiagUnsupported(false);
  const calls = fetchCalls.length;
  assert.deepEqual(Object.keys(await c.fetchDiagnostics()), []);
  assert.equal(fetchCalls.length, calls, 'no request without a namespace');
});

test('a namespace switch mid-flight discards the late response', async () => {
  c.setNS('old-ns');
  c.setDiagUnsupported(false);
  let release;
  sandbox.fetch = () => new Promise(res => { release = () => res(okJSON({ diagnostics: [{ agent: 'a:1', state: 'ready' }] })); });
  const pending = c.fetchDiagnostics();
  c.setNS('new-ns');
  release();
  assert.equal(await pending, null, 'late data for old-ns must not land in new-ns');
});

test('a namespace switch mid-flight discards the late failure too', async () => {
  c.setNS('old-ns');
  c.setDiagUnsupported(false);
  let release;
  sandbox.fetch = () => new Promise(res => { release = () => res({ ok: false, status: 404, text: async () => 'gone' }); });
  const pending = c.fetchDiagnostics();
  c.setNS('new-ns');
  release();
  assert.equal(await pending, null);
  assert.equal(c.getDiagUnsupported(), false, 'old namespace 404 must not mark the route missing for the new one');
});

test('diagFor never shows one namespace\u2019s report under another', () => {
  c.setNS('ns1');
  const map = Object.create(null);
  map['a:1'] = { agent: 'a:1', state: 'ready' };
  assert.equal(c.diagFor(map, 'ns1', 'a:1').state, 'ready');
  c.setNS('ns2');
  assert.equal(c.diagFor(map, 'ns1', 'a:1'), undefined, 'stale ns1 map must read as no-data in ns2');
  assert.equal(c.diagFor(null, 'ns2', 'a:1'), undefined);
});

test('fetchDiagnostics is safe against prototype-polluting addresses', async () => {
  c.setNS('ns1');
  c.setDiagUnsupported(false);
  sandbox.fetch = async () => okJSON({
    diagnostics: [
      { agent: '__proto__', state: 'delivery_failed' },
      { agent: 'constructor', state: 'ready' },
      { agent: 42, state: 'ready' },      // non-string agent: skipped
      { agent: '', state: 'ready' },      // empty agent: skipped
      { agent: 'a:b', state: 'ready' },
    ],
  });
  const map = await c.fetchDiagnostics();
  assert.equal(Object.getPrototypeOf(map), null, 'map must be prototypeless');
  assert.equal(map['__proto__'].state, 'delivery_failed', '__proto__ is an own data key, not a prototype write');
  assert.equal(map.constructor.state, 'ready', 'constructor is an own data key, not Object');
  assert.deepEqual(Object.keys(map).sort(), ['__proto__', 'a:b', 'constructor']);
  assert.equal({}.polluted, undefined, 'no prototype pollution');
  c.setNS('ns1');
  assert.equal(c.diagFor(map, 'ns1', 'hasOwnProperty'), undefined, 'unknown special keys stay undefined');
});
