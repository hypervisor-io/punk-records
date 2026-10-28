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
    closest: () => null, scrollIntoView() {}, focus() {},
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

test('probeAuth asks /v1/session first and falls back route by route on 404', async () => {
  const seen = [];
  sandbox.fetch = async (path, opts) => {
    seen.push([path, (opts && opts.headers && opts.headers.Authorization) || '']);
    return { ok: true, status: 200 };
  };
  assert.equal(await c.probeAuth('k1'), 200);
  assert.deepEqual(seen, [['/v1/session', 'Bearer k1']], 'a current server answers on the store-free probe alone');
  seen.length = 0;
  sandbox.fetch = async (path, opts) => {
    seen.push([path, (opts && opts.headers && opts.headers.Authorization) || '']);
    return path === '/v1/namespaces' ? { ok: true, status: 200 } : { ok: false, status: 404 };
  };
  assert.equal(await c.probeAuth('k1'), 200);
  assert.deepEqual(seen, [['/v1/session', 'Bearer k1'], ['/v1/overview', 'Bearer k1'], ['/v1/namespaces', 'Bearer k1']]);
  seen.length = 0;
  sandbox.fetch = async path => { seen.push([path, '']); return { ok: false, status: 401 }; };
  assert.equal(await c.probeAuth(''), 401, 'a rejected key is reported, not retried on the fallback');
  assert.deepEqual(seen, [['/v1/session', '']]);
  sandbox.fetch = async () => { throw new Error('offline'); };
  assert.equal(await c.probeAuth('k1'), 0, 'network failure is 0');
});

test('pagerHTML shows the row window and disables the ends', () => {
  assert.equal(c.pagerHTML('board', 0, 50, 30), '', 'one page needs no pager');
  const first = c.pagerHTML('board', 0, 50, 120);
  assert.ok(first.includes('1-50 of 120'));
  assert.match(first, /data-dir="-1" disabled/);
  assert.doesNotMatch(first, /data-dir="1" disabled/);
  const last = c.pagerHTML('board', 100, 50, 120);
  assert.ok(last.includes('101-120 of 120'));
  assert.match(last, /data-dir="1" disabled/);
});

test('pageStep clamps to the first and last page', () => {
  c.pageOffset.board = 0;
  c.pageStep('board', -1, 120);
  assert.equal(c.pageOffset.board, 0);
  c.pageStep('board', 1, 120);
  c.pageStep('board', 1, 120);
  c.pageStep('board', 1, 120);
  assert.equal(c.pageOffset.board, 100, 'never past the last page');
  c.pageStep('board', 1, 0);
  assert.equal(c.pageOffset.board, 0, 'an empty board pins to the start');
});

test('boardURL pages and encodes the state filter', () => {
  assert.equal(c.boardURL('a b', 'open', 50), '/v1/namespaces/a%20b/tasks?limit=' + c.PAGE_SIZE + '&offset=50&state=open');
  assert.equal(c.boardURL('x', '', 0), '/v1/namespaces/x/tasks?limit=' + c.PAGE_SIZE + '&offset=0');
  assert.equal(c.boardURL('x', 'done', 0, 20), '/v1/namespaces/x/tasks?limit=20&offset=0&state=done&sort=recent');
});

test('board state tabs count from server totals and mark the active tab', () => {
  const counts = { total: 10, done: 4, pending: 3, in_progress: 2, review: 1, blocked: 0 };
  assert.equal(c.tabCount('open', counts), 6, 'open is everything not done');
  assert.equal(c.tabCount('', counts), 10);
  assert.equal(c.tabCount('blocked', counts), 0);
  assert.equal(c.tabCount('open', null), null, 'no counts before the first load');
  const html = c.boardTabsHTML('review', counts);
  assert.match(html, /data-board-state="review"[^>]*class="tab-btn on"/);
  assert.match(html, /data-board-state="open"[^>]*class="tab-btn"/);
  assert.doesNotMatch(c.boardTabsHTML('open', null), /font-mono/, 'no count badges without counts');
});

test('kanban shows done only on the all tab and offers more cards per column', () => {
  assert.deepEqual([...c.kanbanStates('open')], ['pending', 'in_progress', 'review', 'blocked']);
  assert.deepEqual([...c.kanbanStates('')], ['pending', 'in_progress', 'review', 'blocked', 'done']);
  const html = c.kanbanHTML([
    { st: 'pending', label: 'Pending', dot: 'bg-line', b: { total: 25, tasks: [{ id: 't<1>', title: 'x', ready: true }] } },
    { st: 'review', label: 'Review', dot: 'bg-wait', b: { total: 0, tasks: [] } },
  ]);
  assert.match(html, /data-task-open="t&lt;1&gt;"/, 'task ids are escaped');
  assert.match(html, /data-kanban-more="pending">show 20 more of 24/);
  assert.doesNotMatch(html, /data-kanban-more="review"/);
  assert.match(html, />none</);
});

test('task modal shows the full task, status, question and answer, escaped', () => {
  assert.equal(c.exactFact([{ key: '/tasks/S10', body: 'x' }, { key: '/tasks/S1', body: 'y' }], '/tasks/S1').body, 'y', 'prefix recall siblings are skipped');
  assert.equal(c.exactFact([{ key: '/tasks/S10' }], '/tasks/S1'), null);
  const row = { id: 'S1', title: 'Do <it>', state: 'blocked', holder: 'codex:a', depends_on: ['S0'], updated_at: new Date().toISOString(), by: 'codex:a' };
  const facts = {
    task: { key: '/tasks/S1', body: 'Do <it>\nfull plan text', author: 'planner', created_at: new Date().toISOString() },
    status: { key: '/tasks/S1/status', body: 'blocked: waiting on keys', created_at: new Date().toISOString() },
    question: { key: '/questions/S1', body: 'which key?', created_at: new Date().toISOString() },
    answer: null,
  };
  const html = c.taskModalHTML('S1', row, facts, ['S2']);
  assert.match(html, /Do &lt;it&gt;/);
  assert.doesNotMatch(html, /<it>/, 'task text is escaped');
  assert.match(html, /full plan text/);
  assert.match(html, /blocked: waiting on keys/);
  assert.match(html, /which key\?/);
  assert.doesNotMatch(html, />Answer</, 'no answer section without an answer');
  assert.match(html, /data-task-open="S0"/, 'dependencies open their own task');
  assert.match(html, /data-task-open="S2"/, 'dependents too');
  // A task not on the loaded board pages still gets its state from the status fact.
  assert.match(c.taskModalHTML('S9', null, { status: { body: 'In progress: phase 2', created_at: '' } }, []), /class="pill [^"]*">in_progress</);
});

test('member chips sort by last seen, shorten addresses and cap the list', () => {
  const now = Date.parse('2026-10-28T10:00:00Z');
  const members = [
    { agent: 'old', last_seen_at: '2026-10-20T10:00:00Z' },
    { agent: 'claude-code:9cf63b43-ab86-4d39-80e9-18c68bf32f00', role: 'planner', last_seen_at: '2026-10-28T09:58:00Z' },
  ];
  const html = c.memberChipsHTML(members, now);
  assert.ok(html.indexOf('claude-code:') < html.indexOf('>old<'), 'most recently seen first');
  assert.match(html, />claude-code:9cf63b43-ab86-…</);
  assert.match(html, /bg-wait/, 'seen within 10 minutes');
  assert.match(html, /title="claude-code:9cf63b43-ab86-4d39-80e9-18c68bf32f00\nplanner/);
  const many = Array.from({ length: 11 }, (_, i) => ({ agent: 'a' + i }));
  assert.match(c.memberChipsHTML(many, now), /\+3 more/);
  assert.equal(c.memberChipsHTML([], now), '');
});

test('nsIsIdle treats missing and old activity as idle', () => {
  assert.equal(c.nsIsIdle({ name: 'a' }), true);
  assert.equal(c.nsIsIdle({ last_activity: new Date(Date.now() - 8 * 86400000).toISOString() }), true);
  assert.equal(c.nsIsIdle({ last_activity: new Date(Date.now() - 3600000).toISOString() }), false);
  assert.equal(c.nsIsIdle({ last_write: new Date().toISOString() }), false, 'last_write is the fallback for old servers');
});

test('taskBar counts open tasks and escapes nothing remote', () => {
  const html = c.taskBar({ done: 2, pending: 1, in_progress: 1 }, 4);
  assert.ok(html.includes('2 open'));
  assert.ok(html.includes('done 2'));
  assert.equal(c.taskBar({}, 0).includes('-'), true, 'no tasks renders a dash');
});

test('consBadge marks archives and consolidated namespaces, escaping nothing remote', () => {
  assert.equal(c.consBadge({ name: 'plain' }), '');
  assert.match(c.consBadge({ name: 'x-archive-20261028', archive: true }), />archive</);
  const at = new Date(Date.now() - 3600000).toISOString();
  const fresh = c.consBadge({ name: 'p', consolidated_at: at, last_write: at });
  assert.match(fresh, /consolidated/);
  assert.doesNotMatch(fresh, /new writes since/);
  const later = new Date(Date.now() - 60000).toISOString();
  assert.match(c.consBadge({ name: 'p', consolidated_at: at, last_write: later }), /new writes since/);
});
