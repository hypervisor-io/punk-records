'use strict';

// Punk Records operator console.
//
// Single-file vanilla JS app. Views are plain functions registered in
// the VIEWS map at the bottom of this file; each view mounts its own
// static skeleton once into #view and refresh()es its data portions on
// tab switch and on the 5s poll, so controls inside a view (selects,
// filters, the open task ledger) keep their state while another view is
// showing. localStorage key `amk` (bearer token) is shared with the
// brain view (internal/api/ui/brain.js reads only that key and has no
// namespace rail); `ns` (selected namespace) is this console's own key,
// kept under its previous name for continuity with the console it
// replaces, and must keep working.

const $ = (q, root) => (root || document).querySelector(q);
const $$ = (q, root) => Array.from((root || document).querySelectorAll(q));

const token = () => localStorage.getItem('amk') || '';

const esc = s => String(s ?? '').replace(/[&<>"]/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));
const usd = m => '$' + (m / 1e6).toFixed(2);

async function api(path, opts) {
  opts = opts || {};
  opts.headers = Object.assign({ 'Content-Type': 'application/json' }, opts.headers || {});
  if (token()) opts.headers['Authorization'] = 'Bearer ' + token();
  const r = await fetch(path, opts);
  if (r.status === 401 && authed) {
    // The key was revoked or the server gained keys mid-session: back to
    // the login screen instead of every view rendering the same error.
    showLogin('Your API key was rejected. Sign in again.');
  }
  if (!r.ok) throw new Error(path + ': ' + r.status + ' ' + await r.text());
  return r.json();
}

// ---------------------------------------------------------------------
// Login gate. Nothing but the login screen renders until a key (or an
// open, keyless server) is confirmed against an authenticated route.
// ---------------------------------------------------------------------

let authed = false;

// probeAuth reports the HTTP status an authenticated route answers with
// for the given key (empty key = no Authorization header). /v1/session
// reads no store, so signing in never waits on a read model; servers
// that predate it answer 404 there, and each older route is tried in
// turn (/v1/overview, then /v1/namespaces).
async function probeAuth(key) {
  const h = key ? { Authorization: 'Bearer ' + key } : {};
  try {
    let r = await fetch('/v1/session', { headers: h });
    if (r.status === 404) r = await fetch('/v1/overview', { headers: h });
    if (r.status === 404) r = await fetch('/v1/namespaces', { headers: h });
    return r.status;
  } catch (e) {
    return 0; // network failure
  }
}

function loginError(msg) {
  const el = $('#loginError');
  el.textContent = msg || '';
  el.classList.toggle('hidden', !msg);
}

function showLogin(msg) {
  authed = false;
  $('#loginSubmit').disabled = false;
  $('#loginSubmit').textContent = 'Sign in';
  if (VIEWS[current] && VIEWS[current].leave) VIEWS[current].leave();
  $('#app').classList.add('hidden');
  $('#app').classList.remove('flex');
  $('#login').classList.remove('hidden');
  loginError(msg);
  $('#loginKey').value = '';
  $('#loginKey').focus();
}

function enterApp() {
  authed = true;
  $('#login').classList.add('hidden');
  $('#app').classList.remove('hidden');
  $('#app').classList.add('flex');
  loginError('');
  namespaces = null;
  overviewMissing = false;
  refreshAll();
}

async function boot() {
  const saved = token();
  const st = await probeAuth(saved);
  if (st >= 200 && st < 300) {
    // A saved key that works, or no saved key on a keyless server: the
    // latter still shows the login screen once so the open state is
    // stated, not silently assumed.
    if (saved) { enterApp(); return; }
    showLogin('');
    $('#loginOpen').classList.remove('hidden');
    $('#loginOpen').classList.add('flex');
    return;
  }
  if (saved) localStorage.removeItem('amk');
  showLogin(st === 0 ? 'Cannot reach the server.' : saved ? 'Your saved API key was rejected. Sign in again.' : '');
}

$('#loginForm').addEventListener('submit', async e => {
  e.preventDefault();
  const key = $('#loginKey').value.trim();
  if (!key) { loginError('Enter an API key.'); return; }
  $('#loginSubmit').disabled = true;
  loginError('');
  $('#loginSubmit').textContent = 'Checking…';
  const st = await probeAuth(key);
  $('#loginSubmit').disabled = false;
  $('#loginSubmit').textContent = 'Sign in';
  if (st >= 200 && st < 300) {
    localStorage.setItem('amk', key);
    enterApp();
  } else if (st === 401 || st === 403) {
    loginError('That key was rejected.');
  } else {
    loginError(st ? 'Server error (' + st + '). Try again.' : 'Cannot reach the server.');
  }
});

$('#loginContinue').addEventListener('click', () => {
  localStorage.removeItem('amk');
  enterApp();
});

$('#signOut').addEventListener('click', () => {
  localStorage.removeItem('amk');
  showLogin('');
  boot();
});

// Colour reinforces the state word, it never replaces it (design plan
// principle). Tokens only define --live (listening / done) and --wait
// (leased, waiting, blocked); the brand crimson doubles as the alert
// colour for failed/rejected/canceled since the palette has no separate
// danger hue. Anything not listed stays plain ink or dim.
const STATE_COLOR = {
  done: 'text-live', completed: 'text-live', approved: 'text-live', active: 'text-live', listening: 'text-live', finding: 'text-live', ready: 'text-live',
  review: 'text-wait', blocked: 'text-wait', input_required: 'text-wait', proposed: 'text-wait', leased: 'text-wait', budget_exhausted: 'text-wait', waiting: 'text-wait',
  waiting_for_idle: 'text-wait', waiting_for_next_prompt: 'text-wait', wake_budget_exhausted: 'text-wait', sender_filtered: 'text-wait',
  failed: 'text-brand', canceled: 'text-brand', rejected: 'text-brand', error: 'text-brand',
  delivery_failed: 'text-brand', handoff_unconfirmed: 'text-brand',
  pending: 'text-dim', submitted: 'text-dim', disabled: 'text-dim', status_change: 'text-dim',
};
const stateClass = s => (Object.hasOwn(STATE_COLOR, s) ? STATE_COLOR[s] : '');
const pill = state => `<span class="pill ${stateClass(state)}">${esc(state)}</span>`;

// ---------------------------------------------------------------------
// Namespace rail (global chrome, shared across views)
// ---------------------------------------------------------------------

let ns = localStorage.getItem('ns') || '';
let namespaces = null; // cache; null forces a reload

// Paging. The board pages on the server (?limit/?offset); the overview
// table and the rail page the cached /v1/overview rows in the browser,
// since their filter and sort span every namespace. pageOffset holds the
// first row shown per pager; filters and namespace switches reset it.
const PAGE_SIZE = 50;
const RAIL_PAGE = 40;
const pageOffset = { board: 0, overview: 0 };
let railShown = RAIL_PAGE;

// pagerHTML renders "first-last of total" with prev/next buttons for
// the named pager, or nothing when everything fits on one page.
function pagerHTML(kind, offset, size, total) {
  if (total <= size && offset === 0) return '';
  const last = Math.min(offset + size, total);
  return `<div class="mt-3 flex items-center gap-2 text-xs text-dim">
    <button type="button" class="btn disabled:opacity-60" data-page="${kind}" data-dir="-1"${offset <= 0 ? ' disabled' : ''}>prev</button>
    <span class="font-mono">${total ? offset + 1 : 0}-${last} of ${total}</span>
    <button type="button" class="btn disabled:opacity-60" data-page="${kind}" data-dir="1"${last >= total ? ' disabled' : ''}>next</button>
  </div>`;
}

// pageStep moves a pager one page and clamps it to the row count.
function pageStep(kind, dir, total) {
  const max = Math.max(0, Math.floor(Math.max(0, total - 1) / PAGE_SIZE) * PAGE_SIZE);
  pageOffset[kind] = Math.min(max, Math.max(0, pageOffset[kind] + dir * PAGE_SIZE));
}

function setNS(name) {
  ns = name;
  localStorage.setItem('ns', name);
  pageOffset.board = 0;
  memWanted = MEM_PAGE;
  diagUnsupported = false; // probe the diagnostics route once for the newly selected namespace
  renderNamespaces();
  closeRail();
  refreshCurrent();
}

// The rail is fed by GET /v1/overview (activity-sorted, with liveness and
// unread counts). Servers that predate that route answer 404, so the rail
// falls back to the plain /v1/namespaces index and the Overview tab says
// the route is missing instead of rendering zeros.
let overview = null;       // last /v1/overview payload
let overviewMissing = false;
let lastRefreshOK = 0;     // ms epoch of the last successful poll, for the top-bar pulse
let lastRefreshErr = '';

const IDLE_DAYS = 7;

function nsLastActivity(n) {
  return n.last_activity || n.last_write || '';
}
function nsIsIdle(n) {
  const t = Date.parse(nsLastActivity(n));
  return !t || Date.now() - t > IDLE_DAYS * 86400000;
}

async function loadOverview() {
  if (overviewMissing) return null;
  try {
    overview = await api('/v1/overview');
    markRefresh('');
    return overview;
  } catch (e) {
    if (/: 404 /.test(e.message)) { overviewMissing = true; return null; }
    markRefresh(e.message);
    throw e;
  }
}

async function loadNamespaces() {
  let list;
  const ov = await loadOverview();
  if (ov) {
    list = ov.namespaces || [];
  } else {
    const d = await api('/v1/namespaces');
    markRefresh('');
    list = (d.namespaces || []).slice().sort((a, b) => (Date.parse(b.last_write) || 0) - (Date.parse(a.last_write) || 0));
  }
  namespaces = list;
  if (!namespaces.some(n => n.name === ns)) {
    const first = namespaces.find(n => !nsIsIdle(n)) || namespaces[0];
    ns = first ? first.name : '';
  }
  if (ns) localStorage.setItem('ns', ns);
  renderNamespaces();
}

function markRefresh(err) {
  lastRefreshErr = err || '';
  if (!err) lastRefreshOK = Date.now();
  renderPulse();
}

function renderPulse() {
  const dot = $('#pulseDot'), text = $('#pulseText');
  if (!dot || !text) return;
  if (lastRefreshErr) {
    dot.className = 'dot bg-brand';
    text.textContent = 'offline';
    $('#pulse').title = lastRefreshErr;
    return;
  }
  if (!lastRefreshOK) return;
  const secs = Math.max(0, Math.round((Date.now() - lastRefreshOK) / 1000));
  dot.className = 'dot bg-live' + (secs < 6 ? ' live-dot' : '');
  text.textContent = secs < 2 ? 'live' : 'updated ' + secs + 's ago';
  $('#pulse').title = 'last refresh ' + new Date(lastRefreshOK).toLocaleTimeString();
}
setInterval(renderPulse, 1000);

function renderNamespaces() {
  const q = $('#nsFilter').value.trim().toLowerCase();
  const hideIdle = $('#nsHideIdle').checked;
  const all = namespaces || [];
  const list = all.filter(n => (!q || n.name.toLowerCase().includes(q)) && (!hideIdle || q || !nsIsIdle(n) || n.name === ns));
  const hidden = all.length - list.length;
  // Render one page of rows (plus the selected namespace wherever it
  // sorts), so a server with hundreds of namespaces stays a short list.
  const shown = list.slice(0, railShown);
  const sel0 = list.find(n => n.name === ns);
  if (sel0 && !shown.includes(sel0)) shown.push(sel0);
  const more = list.length - shown.length;

  $('#nsList').innerHTML = (shown.length
    ? shown.map(n => {
        const live = (n.members_live || 0) > 0, recent = (n.members_recent || 0) > 0;
        const dot = live ? '<span class="dot live-dot bg-live" title="agent listening"></span>'
          : recent ? '<span class="dot bg-live" title="agent seen in the last 10 minutes"></span>'
          : '<span class="dot bg-line"></span>';
        const open = n.task_counts ? (n.tasks || 0) - (n.task_counts.done || 0) : 0;
        const meta = [
          open ? open + ' open' : '',
          n.unread ? '<span class="text-wait">' + n.unread + ' unread</span>' : '',
          esc(relAge(nsLastActivity(n)) || ''),
        ].filter(Boolean).join(' · ');
        return `<button type="button" class="flex w-full cursor-pointer items-start gap-2 rounded px-2 py-1.5 text-left text-sm hover:bg-bg${n.name === ns ? ' bg-bg text-brand' : ' text-ink'}" data-ns="${esc(n.name)}">
          <span class="mt-1.5 shrink-0">${dot}</span>
          <span class="min-w-0 flex-1"><span class="block break-all">${esc(n.name)}</span><span class="block font-mono text-xs text-dim">${meta || '&nbsp;'}</span></span>
        </button>`;
      }).join('')
    : '<div class="empty-state">no namespaces</div>') +
    (more > 0 ? `<button type="button" class="btn mx-2 my-2" data-rail-more>show ${Math.min(more, RAIL_PAGE)} more of ${more}</button>` : '') +
    (hidden && !q ? `<div class="px-2 py-2 text-xs text-dim">${hidden} idle namespace${hidden === 1 ? '' : 's'} hidden</div>` : '');

  const sel = $('#nsSelect');
  sel.innerHTML = (namespaces || []).map(n => `<option value="${esc(n.name)}"${n.name === ns ? ' selected' : ''}>${esc(n.name)}</option>`).join('')
    || '<option value="">no namespaces</option>';
  $('#nsLabelName').textContent = ns || 'none';
}

$('#nsFilter').addEventListener('input', () => { railShown = RAIL_PAGE; renderNamespaces(); });
$('#nsHideIdle').addEventListener('change', () => { railShown = RAIL_PAGE; renderNamespaces(); });
$('#nsList').addEventListener('click', e => {
  if (e.target.closest('[data-rail-more]')) { railShown += RAIL_PAGE; renderNamespaces(); return; }
  const row = e.target.closest('[data-ns]');
  if (row) setNS(row.dataset.ns);
});
$('#nsSelect').addEventListener('change', e => setNS(e.target.value));

// loadNamespacesAndRefresh always fetches a fresh namespace list (the
// rail is global chrome, shown regardless of which view is open) and,
// once it lands, refreshes whatever view is current. Used by the
// "only with tasks" toggle and by refreshAll (initial load and the
// token-change handler), so those two are never limited to reloading
// namespaces only while Board, Messages or Agents happens to be open.
async function loadNamespacesAndRefresh() {
  try {
    await loadNamespaces();
  } catch (e) {
    $('#nsList').innerHTML = '<div class="empty-state">' + esc(e.message) + '</div>';
    return;
  }
  refreshCurrent();
}

// Drawer (rail below 1024px), opened from the top bar hamburger.
function openRail() {
  $('#rail').classList.remove('-translate-x-full');
  $('#railScrim').classList.remove('hidden');
  $('#railToggle').setAttribute('aria-expanded', 'true');
}
function closeRail() {
  if (window.innerWidth >= 1024) return;
  $('#rail').classList.add('-translate-x-full');
  $('#railScrim').classList.add('hidden');
  $('#railToggle').setAttribute('aria-expanded', 'false');
}
$('#railToggle').addEventListener('click', () => {
  $('#rail').classList.contains('-translate-x-full') ? openRail() : closeRail();
});
$('#railScrim').addEventListener('click', closeRail);
document.addEventListener('keydown', e => { if (e.key === 'Escape') closeRail(); });


// ---------------------------------------------------------------------
// Overview view: server-wide coordination picture from /v1/overview.
// Global, so it renders without a selected namespace; clicking a row
// selects that namespace and opens its Board.
// ---------------------------------------------------------------------

const fmtNum = n => (n || 0).toLocaleString('en-US');

function statTile(label, value, sub, tone) {
  return `<div class="panel flex min-w-0 flex-col gap-0.5">
    <span class="text-xs text-dim">${esc(label)}</span>
    <span class="text-xl font-semibold tabular-nums ${tone || 'text-ink'}">${value}</span>
    <span class="text-xs text-dim">${sub || '&nbsp;'}</span>
  </div>`;
}

// taskBar draws one namespace's task states as a proportional strip; the
// counts are repeated in the title so colour never carries meaning alone.
function taskBar(tc, total) {
  if (!total) return '<span class="text-dim">-</span>';
  const parts = [['done', 'bg-live'], ['in_progress', 'bg-ink'], ['review', 'bg-wait'], ['blocked', 'bg-brand'], ['pending', 'bg-line']];
  const title = parts.map(([k]) => k.replace('_', ' ') + ' ' + ((tc && tc[k]) || 0)).join(', ');
  const open = total - ((tc && tc.done) || 0);
  return `<div class="flex items-center gap-2" title="${esc(title)}">
    <div class="flex h-1.5 w-24 shrink-0 overflow-hidden rounded-full bg-bg">${parts.map(([k, c]) => {
      const v = (tc && tc[k]) || 0;
      return v ? `<span class="${c}" style="width:${(v / total * 100).toFixed(1)}%"></span>` : '';
    }).join('')}</div>
    <span class="font-mono text-xs ${open ? 'text-ink' : 'text-dim'}">${open} open</span>
  </div>`;
}

function mountOverview(el) {
  el.innerHTML = `
    <div id="ovRuntime" class="mb-3 flex flex-wrap items-center gap-2 text-xs"></div>
    <div id="ovTiles" class="mb-4 grid grid-cols-2 gap-2 sm:grid-cols-3 xl:grid-cols-6"></div>
    <div class="mb-2 flex flex-wrap items-center gap-3">
      <h2 class="text-md font-semibold">Namespaces</h2>
      <input id="ovFilter" type="text" placeholder="filter" class="field w-48">
      <label class="flex items-center gap-2 text-sm text-dim">
        <input id="ovActiveOnly" type="checkbox" checked class="accent-brand">
        active in the last 7 days
      </label>
      <span id="ovCount" class="text-xs text-dim"></span>
    </div>
    <div id="ovTable"></div>`;
  $('#ovFilter', el).addEventListener('input', () => { pageOffset.overview = 0; renderOverview(); });
  $('#ovActiveOnly', el).addEventListener('change', () => { pageOffset.overview = 0; renderOverview(); });
}

async function loadOverviewView() {
  try {
    await loadNamespaces(); // also refreshes `overview`
  } catch (e) {
    $('#ovTable').innerHTML = '<div class="empty-state">' + esc(e.message) + '</div>';
    return;
  }
  renderOverview();
}

function renderOverview() {
  if (!$('#ovTable')) return;
  if (!overview) {
    $('#ovRuntime').innerHTML = '';
    $('#ovTiles').innerHTML = '';
    $('#ovTable').innerHTML = '<div class="empty-state">This server does not serve /v1/overview yet. Upgrade punk to see namespace summaries; the Board, Memory, Messages and Agents tabs still work.</div>';
    return;
  }
  const t = overview.totals || {}, rt = overview.runtime || {};
  const flag = (on, label, onWord, offWord) => `<span class="pill ${on ? 'text-live' : 'text-dim'}">${esc(label)} ${on ? onWord : offWord}</span>`;
  $('#ovRuntime').innerHTML = [
    overview.version ? `<span class="pill">punk ${esc(overview.version)}</span>` : '',
    flag(rt.messaging_enabled, 'messaging', 'on', 'off'),
    `<span class="pill ${rt.authz === 'deny' ? 'text-live' : 'text-dim'}">authz ${esc(rt.authz || 'off')}</span>`,
    flag(rt.ai_enabled, 'model', 'on', 'off'),
  ].join('');

  const nsActive = (overview.namespaces || []).filter(n => !nsIsIdle(n)).length;
  $('#ovTiles').innerHTML = [
    statTile('Agents live', fmtNum(t.members_live), fmtNum(t.members_recent) + ' recent of ' + fmtNum(t.members), t.members_live ? 'text-live' : ''),
    statTile('Open tasks', fmtNum(t.tasks_open), fmtNum(t.tasks) + ' total', t.tasks_open ? 'text-ink' : 'text-dim'),
    statTile('Unread messages', fmtNum(t.unread), fmtNum(t.messages_24h) + ' sent in 24h', t.unread ? 'text-wait' : 'text-dim'),
    statTile('Active claims', fmtNum(t.claims), 'leased work items'),
    statTile('Active namespaces', fmtNum(nsActive), 'of ' + fmtNum(t.namespaces) + ', last 7 days'),
    statTile('Facts', fmtNum(t.facts), 'live memory'),
  ].join('');

  const q = ($('#ovFilter').value || '').trim().toLowerCase();
  const activeOnly = $('#ovActiveOnly').checked;
  const rows = (overview.namespaces || []).filter(n => (!q || n.name.toLowerCase().includes(q)) && (!activeOnly || q || !nsIsIdle(n)));
  $('#ovCount').textContent = rows.length + ' of ' + (overview.namespaces || []).length;
  ovRows = rows.length;
  if (pageOffset.overview >= rows.length) pageOffset.overview = 0;
  const page = rows.slice(pageOffset.overview, pageOffset.overview + PAGE_SIZE);

  $('#ovTable').innerHTML = rows.length
    ? `<div class="table-wrap"><table class="data-table">
        <colgroup><col style="width:28%"><col style="width:14%"><col style="width:16%"><col style="width:18%"><col style="width:12%"><col style="width:12%"></colgroup>
        <thead><tr><th>Namespace</th><th>Last activity</th><th>Agents</th><th>Tasks</th><th class="num">Unread / 24h</th><th class="num">Facts</th></tr></thead>
        <tbody>${page.map(n => {
          const live = n.members_live || 0, recent = n.members_recent || 0;
          const agents = n.members
            ? `<span class="${live ? 'text-live' : recent ? 'text-ink' : 'text-dim'}">${live ? '<span class="dot live-dot mr-1.5 bg-live"></span>' + live + ' live' : recent ? recent + ' recent' : 'idle'}</span> <span class="text-dim">/ ${n.members}</span>`
            : '<span class="text-dim">none</span>';
          return `<tr class="cursor-pointer" data-open-ns="${esc(n.name)}">
            <td data-label="Namespace"><button type="button" class="break-all text-left font-semibold text-ink hover:text-brand hover:underline" data-open-ns="${esc(n.name)}">${esc(n.name)}</button></td>
            <td class="mono" data-label="Last activity">${esc(relAge(nsLastActivity(n)) || '-')}</td>
            <td data-label="Agents">${agents}</td>
            <td data-label="Tasks">${taskBar(n.task_counts, n.tasks || 0)}</td>
            <td class="num font-mono" data-label="Unread / 24h"><span class="${n.unread ? 'text-wait' : 'text-dim'}">${fmtNum(n.unread)}</span> <span class="text-dim">/ ${fmtNum(n.messages_24h)}</span></td>
            <td class="num mono" data-label="Facts">${fmtNum(n.facts)}</td>
          </tr>`;
        }).join('')}</tbody>
      </table></div>` + pagerHTML('overview', pageOffset.overview, PAGE_SIZE, rows.length)
    : '<div class="empty-state">No namespaces match. Agents create one when they connect or register.</div>';
}

let ovRows = 0;    // filtered overview rows, for the pager's clamp
let boardRows = 0; // filtered board rows (server total), likewise

$('#view').addEventListener('click', e => {
  const b = e.target.closest('[data-page]');
  if (!b || b.disabled) return;
  const kind = b.dataset.page, dir = Number(b.dataset.dir);
  if (kind === 'overview') { pageStep('overview', dir, ovRows); renderOverview(); }
  if (kind === 'board') { pageStep('board', dir, boardRows); loadBoardTable(); }
});

$('#view').addEventListener('click', e => {
  const row = e.target.closest('[data-open-ns]');
  if (!row) return;
  setNS(row.dataset.openNs);
  switchView('board');
});

// nsHeader is the one-line summary of the selected namespace shown above
// the per-namespace views, from the cached overview row when there is one.
function nsHeader() {
  const n = (namespaces || []).find(x => x.name === ns);
  if (!n) return `<span class="font-semibold text-ink">${esc(ns)}</span>`;
  const bits = [
    `<span class="font-semibold text-ink">${esc(n.name)}</span>`,
    n.members_live ? `<span class="text-live"><span class="dot live-dot mr-1.5 bg-live"></span>${n.members_live} live</span>` : '',
    n.members ? `<span>${n.members_recent || 0} recent of ${n.members} agent${n.members === 1 ? '' : 's'}</span>` : '',
    n.unread ? `<span class="text-wait">${n.unread} unread</span>` : '',
    `<span>${fmtNum(n.facts)} facts</span>`,
    nsLastActivity(n) ? `<span>active ${esc(relAge(nsLastActivity(n)))}</span>` : '',
  ];
  return `<div class="flex flex-wrap items-center gap-x-4 gap-y-1 text-sm text-dim">${bits.filter(Boolean).join('')}</div>`;
}

// ---------------------------------------------------------------------
// Memory view: what the namespace actually knows. Recent writes come from
// /recent (session capture hidden by default, it drowns everything else);
// a query switches the list to /memories/search.
// ---------------------------------------------------------------------

function mountMemory(el) {
  el.innerHTML = `
    <div id="memHeader" class="mb-3"></div>
    <form id="memSearchForm" class="mb-3 flex flex-wrap items-center gap-2">
      <input id="memQuery" type="search" placeholder="search this namespace" class="field w-full max-w-md">
      <button type="submit" class="btn">search</button>
      <label class="flex items-center gap-2 text-sm text-dim">
        <input id="memSessions" type="checkbox" class="accent-brand">
        include captured sessions
      </label>
    </form>
    <div id="memList"></div>`;
  $('#memSearchForm', el).addEventListener('submit', e => { e.preventDefault(); loadMemory(); });
  $('#memSessions', el).addEventListener('change', () => { memWanted = MEM_PAGE; loadMemory(); });
}

let memExpanded = new Set();

// The recent feed loads MEM_PAGE rows at a time; "load more" raises
// memWanted and the 5s poll re-reads the same depth, a page per request
// (the server caps a page at 200).
const MEM_PAGE = 60;
let memWanted = MEM_PAGE;
let memHasMore = false;

async function loadRecent(name, sessions) {
  const out = [];
  while (out.length < memWanted) {
    const want = Math.min(200, memWanted - out.length);
    const d = await api('/v1/namespaces/' + encodeURIComponent(name) + '/recent?limit=' + want + '&offset=' + out.length + (sessions ? '&sessions=1' : ''));
    const page = d.facts || [];
    out.push(...page);
    if (page.length < want) { memHasMore = false; return out; }
  }
  memHasMore = true;
  return out;
}

async function loadMemory() {
  if (!$('#memList')) return;
  if (!namespaces) { try { await loadNamespaces(); } catch (e) { /* header falls back to the name */ } }
  if (!ns) { $('#memHeader').innerHTML = ''; $('#memList').innerHTML = '<div class="empty-state">no namespace selected</div>'; return; }
  $('#memHeader').innerHTML = nsHeader();
  const q = $('#memQuery').value.trim();
  const sessions = $('#memSessions').checked;
  try {
    let facts, heading;
    if (q) {
      facts = await api('/v1/namespaces/' + encodeURIComponent(ns) + '/memories/search?limit=50&q=' + encodeURIComponent(q));
      if (!sessions) facts = facts.filter(f => !String(f.key).startsWith('/agent-sessions/'));
      heading = facts.length + ' match' + (facts.length === 1 ? '' : 'es') + ' for "' + esc(q) + '"';
    } else {
      facts = await loadRecent(ns, sessions);
      heading = 'Latest writes';
    }
    $('#memList').innerHTML = `<div class="mb-2 text-xs text-dim">${heading}</div>` + (facts.length
      ? `<div class="flex flex-col">${facts.map(f => {
          const id = f.id || f.key;
          const open = memExpanded.has(id);
          const body = String(f.body || '');
          const long = body.length > 220 || body.includes('\n');
          return `<div class="border-b border-line py-2.5">
            <div class="flex flex-wrap items-baseline gap-x-3 gap-y-0.5">
              <span class="min-w-0 break-all font-mono text-sm text-ink">${esc(f.key)}</span>
              <span class="font-mono text-xs text-dim">${esc(f.author || '')}</span>
              <span class="ml-auto shrink-0 font-mono text-xs text-dim">${esc(relAge(f.created_at) || '')}</span>
            </div>
            <div class="mt-1 whitespace-pre-wrap break-words text-sm ${open ? 'text-ink' : 'text-dim'}">${esc(open || !long ? body : body.slice(0, 220).split('\n')[0] + ' …')}</div>
            ${long ? `<button type="button" class="mt-1 text-xs text-dim hover:text-brand" data-mem-toggle="${esc(id)}">${open ? 'show less' : 'show more'}</button>` : ''}
          </div>`;
        }).join('')}</div>`
      : `<div class="empty-state">${q ? 'Nothing matches.' : 'No writes yet' + (sessions ? '' : ' (outside captured sessions)') + '.'}</div>`) +
      (!q && memHasMore ? '<button type="button" class="btn mt-3" data-mem-more>load more</button>' : '');
  } catch (e) {
    $('#memList').innerHTML = '<div class="empty-state">' + esc(e.message) + '</div>';
  }
}

$('#view').addEventListener('click', e => {
  if (e.target.closest('[data-mem-more]')) { memWanted += MEM_PAGE; loadMemory(); return; }
  const t = e.target.closest('[data-mem-toggle]');
  if (!t) return;
  const id = t.dataset.memToggle;
  if (memExpanded.has(id)) memExpanded.delete(id); else memExpanded.add(id);
  loadMemory();
});

// ---------------------------------------------------------------------
// Board view
// ---------------------------------------------------------------------

function mountBoard(el) {
  el.innerHTML = `
    <div class="mb-3 flex flex-wrap items-center gap-2">
      <select id="boardState" class="field font-mono text-xs">
        <option value="open">open (not done)</option>
        <option value="">all states</option>
        <option value="pending">pending</option>
        <option value="in_progress">in progress</option>
        <option value="review">review</option>
        <option value="blocked">blocked</option>
        <option value="done">done</option>
      </select>
    </div>
    <div id="boardSummary" class="mb-3"></div>
    <div id="boardTable"></div>`;
  $('#boardState', el).addEventListener('change', () => { pageOffset.board = 0; loadBoard(); });
}

async function loadBoard() {
  try {
    if (!namespaces) await loadNamespaces();
    await loadBoardTable();
  } catch (e) {
    $('#boardTable').innerHTML = '<div class="empty-state">' + esc(e.message) + '</div>';
  }
}

// boardURL is one page of the board, filtered by state ('' = all).
function boardURL(name, st, offset) {
  return '/v1/namespaces/' + encodeURIComponent(name) + '/tasks?limit=' + PAGE_SIZE + '&offset=' + offset + (st ? '&state=' + encodeURIComponent(st) : '');
}

async function loadBoardTable() {
  if (!ns) { $('#boardSummary').innerHTML = ''; $('#boardTable').innerHTML = '<div class="empty-state">no namespace selected</div>'; return; }
  try {
    const st = $('#boardState').value;
    const reqNS = ns;
    let b = await api(boardURL(reqNS, st, pageOffset.board));
    // The board shrank under the current page (tasks finished or were
    // filtered away): fall back to the last page that still has rows.
    if (b.total !== undefined && pageOffset.board > 0 && pageOffset.board >= b.total) {
      pageOffset.board = Math.max(0, Math.floor(Math.max(0, b.total - 1) / PAGE_SIZE) * PAGE_SIZE);
      b = await api(boardURL(reqNS, st, pageOffset.board));
    }
    if (reqNS !== ns) return; // a namespace switch landed mid-flight
    boardRows = b.total !== undefined ? b.total : (b.tasks || []).length;
    const c = b.counts || {};
    const names = (b.members || []).map(m => m.agent || m.name || '').filter(Boolean);
    const shown = names.slice(0, 10).map(esc).join(', ') + (names.length > 10 ? ' +' + (names.length - 10) + ' more' : '');

    $('#boardSummary').innerHTML = `
      <div class="mb-2">${nsHeader()}</div>
      <div class="flex flex-wrap items-center gap-x-4 gap-y-1 text-sm text-dim">
        <span>${c.total || 0} tasks</span>
        <span>done <b class="font-normal text-ink">${c.done || 0}</b></span>
        <span>in progress <b class="font-normal text-ink">${c.in_progress || 0}</b></span>
        <span>review <b class="font-normal text-ink">${c.review || 0}</b></span>
        <span>blocked <b class="font-normal text-ink">${c.blocked || 0}</b></span>
        <span>pending <b class="font-normal text-ink">${c.pending || 0}</b></span>
        ${b.next ? `<span>next <b class="font-normal text-ink">${esc(b.next)}</b></span>` : ''}
      </div>
      ${names.length ? `<div class="mt-1 text-xs text-dim">members: ${shown}</div>` : ''}`;

    $('#boardTable').innerHTML = (b.tasks || []).length
      ? `<div class="table-wrap"><table class="data-table">
          <colgroup><col style="width:26%"><col style="width:11%"><col style="width:11%"><col style="width:10%"><col style="width:28%"><col style="width:14%"></colgroup>
          <thead><tr><th>Task</th><th>State</th><th>Holder</th><th>Depends on</th><th>Status</th><th>Updated</th></tr></thead>
          <tbody>${b.tasks.map(t => `<tr>
            <td data-label="Task"><span class="font-semibold">${esc(t.id)}</span><div class="mt-0.5 font-mono text-xs text-dim">${esc(t.title || '')}</div></td>
            <td data-label="State">${pill(t.state)}${t.ready ? ' <span class="pill text-live">ready</span>' : ''}</td>
            <td class="mono" data-label="Holder">${esc(t.holder || '-')}</td>
            <td class="mono" data-label="Depends on">${esc((t.depends_on || []).join(', ') || '-')}</td>
            <td class="mono" data-label="Status">${esc(t.status || '')}</td>
            <td class="mono" data-label="Updated">${esc((t.updated_at || '').slice(0, 16))}<div>${esc(t.by || '')}</div></td>
          </tr>`).join('')}</tbody>
        </table></div>` + pagerHTML('board', pageOffset.board, PAGE_SIZE, boardRows)
      : '<div class="empty-state">' + (st === 'open' && c.total ? 'every task is done' : 'no tasks match') + '</div>';
  } catch (e) {
    $('#boardTable').innerHTML = '<div class="empty-state">' + esc(e.message) + '</div>';
  }
}

// ---------------------------------------------------------------------
// Approvals (proposals + parked tasks), rendered inside the Runtime view
// ---------------------------------------------------------------------

async function loadApprovals() {
  try {
    const props = await api('/v1/proposals?status=proposed');
    $('#approvalsProposals').innerHTML = props.length
      ? `<div class="table-wrap"><table class="data-table">
          <thead><tr><th>Target</th><th>Class</th><th>Task</th><th>Age</th><th></th></tr></thead>
          <tbody>${props.map(p => `<tr>
            <td class="mono" data-label="Target">${esc(p.target)}</td>
            <td data-label="Class">${pill(p.action_class)}</td>
            <td class="mono" data-label="Task">${esc(p.task_id.slice(0, 8))}</td>
            <td class="mono" data-label="Age">${esc(p.created_at.slice(0, 16))}</td>
            <td><button class="btn" data-approve="${esc(p.id)}">approve</button>
              <button class="btn" data-reject="${esc(p.id)}">reject</button></td>
          </tr>`).join('')}</tbody>
        </table></div>`
      : '<div class="empty-state">nothing awaiting approval</div>';

    const parked = await api('/v1/tasks?status=input_required');
    $('#approvalsParked').innerHTML = parked.length
      ? `<div class="table-wrap"><table class="data-table">
          <thead><tr><th>Task</th><th>Agent</th><th>Source</th><th>Updated</th><th></th></tr></thead>
          <tbody>${parked.map(t => `<tr>
            <td class="mono" data-label="Task">${esc(t.id.slice(0, 8))}</td>
            <td data-label="Agent">${esc(t.agent_name || '-')}</td>
            <td class="mono" data-label="Source">${esc(t.external_ref || t.source)}</td>
            <td class="mono" data-label="Updated">${esc(t.updated_at.slice(0, 16))}</td>
            <td><button class="btn" data-requeue="${esc(t.id)}">requeue</button>
              <button class="btn" data-cancel="${esc(t.id)}">cancel</button>
              <button class="btn" data-ledger="${esc(t.id)}">ledger</button></td>
          </tr>`).join('')}</tbody>
        </table></div>`
      : '<div class="empty-state">no parked tasks</div>';
  } catch (e) {
    $('#approvalsProposals').innerHTML = '<div class="empty-state">' + esc(e.message) + '</div>';
  }
}

async function decide(id, status) {
  await api('/v1/proposals/' + encodeURIComponent(id), { method: 'PATCH', body: JSON.stringify({ status }) });
  loadApprovals();
}
async function taskAct(id, act) {
  await api('/v1/tasks/' + encodeURIComponent(id) + '/' + encodeURIComponent(act), { method: 'POST' });
  refreshCurrent();
}

document.addEventListener('click', e => {
  const approve = e.target.closest('[data-approve]');
  const reject = e.target.closest('[data-reject]');
  const requeue = e.target.closest('[data-requeue]');
  const cancel = e.target.closest('[data-cancel]');
  const ledger = e.target.closest('[data-ledger]');
  if (approve) decide(approve.dataset.approve, 'approved');
  else if (reject) decide(reject.dataset.reject, 'rejected');
  else if (requeue) taskAct(requeue.dataset.requeue, 'requeue');
  else if (cancel) taskAct(cancel.dataset.cancel, 'cancel');
  else if (ledger) showTask(ledger.dataset.ledger);
});

// ---------------------------------------------------------------------
// Investigation tasks, rendered inside the Runtime view
// ---------------------------------------------------------------------

async function loadTasks() {
  try {
    const ts = await api('/v1/tasks');
    $('#tasksList').innerHTML = ts.length
      ? `<div class="table-wrap"><table class="data-table">
          <thead><tr><th>Task</th><th>Status</th><th>Agent</th><th>Ref</th><th>Spend</th><th>Created</th></tr></thead>
          <tbody>${ts.map(t => `<tr class="cursor-pointer" data-open-task="${esc(t.id)}">
            <td class="mono" data-label="Task"><button type="button" class="font-mono text-ink hover:text-brand hover:underline" data-open-task="${esc(t.id)}">${esc(t.id.slice(0, 8))}</button></td>
            <td data-label="Status">${pill(t.status)}</td>
            <td data-label="Agent">${esc(t.agent_name || '-')}</td>
            <td class="mono" data-label="Ref">${esc(t.external_ref || t.source)}</td>
            <td class="mono" data-label="Spend">${t.spent_tokens} tok / ${t.spent_tool_calls} calls</td>
            <td class="mono" data-label="Created">${esc(t.created_at.slice(0, 16))}</td>
          </tr>`).join('')}</tbody>
        </table></div>`
      : '<div class="empty-state">no tasks yet</div>';
  } catch (e) {
    $('#tasksList').innerHTML = '<div class="empty-state">' + esc(e.message) + '</div>';
  }
}

$('#view').addEventListener('click', e => {
  const row = e.target.closest('[data-open-task]');
  if (row) showTask(row.dataset.openTask);
});

async function showTask(id) {
  switchView('runtime');
  const d = await api('/v1/tasks/' + encodeURIComponent(id));
  const tl = $('#tasksTimeline');
  tl.classList.remove('hidden');
  tl.innerHTML = `<div class="mb-2 text-sm text-dim"><b class="font-mono font-normal text-ink">${esc(d.task.id)}</b> ${pill(d.task.status)}
      agent <b class="font-normal text-ink">${esc(d.task.agent_name || '-')}</b> ref <b class="font-mono font-normal text-ink">${esc(d.task.external_ref || '-')}</b></div>` +
    (d.events || []).map(e => `<div class="flex gap-3 border-b border-line py-1.5 text-sm last:border-b-0">
      <span class="mono min-w-[2.5rem]">${e.seq}</span>
      <span class="mono min-w-[8rem] ${stateClass(e.type)}">${esc(e.type)}</span>
      <div class="min-w-0 flex-1">
        <span class="mono">${esc(e.actor)}</span>
        <pre class="mt-0.5 whitespace-pre-wrap break-words text-xs text-dim">${esc(JSON.stringify(e.payload))}</pre>
      </div>
    </div>`).join('');
  tl.scrollIntoView({ behavior: window.matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth' });
}

// ---------------------------------------------------------------------
// Model spend, rendered inside the Runtime view
// ---------------------------------------------------------------------

async function loadCosts() {
  try {
    const c = await api('/v1/costs');
    $('#costsSummary').innerHTML = `<div class="flex flex-wrap gap-x-4 gap-y-1 text-sm text-dim">
      <span>today <b class="font-normal text-ink">${usd(c.day_total_microusd)}</b></span>
      <span>this month <b class="font-normal text-ink">${usd(c.month_total_microusd)}</b></span>
    </div>`;
    $('#costsTable').innerHTML = `<div class="table-wrap"><table class="data-table">
        <thead><tr><th>Agent</th><th class="num">Today</th><th class="num">Month</th><th class="num">Completed</th><th class="num">Avg / investigation</th></tr></thead>
        <tbody>${(c.agents || []).map(a => `<tr>
          <td data-label="Agent">${esc(a.agent)}</td>
          <td class="num mono" data-label="Today">${usd(a.day_microusd)}</td>
          <td class="num mono" data-label="Month">${usd(a.month_microusd)}</td>
          <td class="num mono" data-label="Completed">${a.completed_tasks}</td>
          <td class="num mono" data-label="Avg / investigation">${usd(a.avg_per_task_microusd)}</td>
        </tr>`).join('')}</tbody>
      </table></div>`;
  } catch (e) {
    $('#costsSummary').innerHTML = '<div class="empty-state">' + esc(e.message) + '</div>';
  }
}

// ---------------------------------------------------------------------
// Messages view: conversations grouped by unordered participant pair,
// a thread pane with per-message delivery state, and a participants
// pane with liveness from /members. Layout is one three-column panel
// (see .msg-shell in console.src.css): all three columns show at
// >=1024px; below 1024px the participants pane hides and its liveness
// folds into the thread header instead (the `lg:hidden` block below);
// below 768px only one of the list/thread panes shows at a time,
// toggled by `msgMobileView` (tapping a conversation, or the thread's
// back button).
//
// Live updates ride the same namespace SSE stream as the Agents view's
// unread counts could, but EventSource cannot carry an Authorization
// header, so the stream is read with fetch + a hand-rolled SSE parser,
// exactly like internal/api/ui/brain.js does for /v1/brain/events (that
// file is an ES module and console.js is a plain script, so the parser
// is duplicated here rather than imported). A `message` or `ack` frame
// triggers a log re-fetch; the 5s view poll (loadMessages, via the
// VIEWS.messages.refresh binding) is the fallback if the stream is
// down. The stream is stopped when the Messages view is left
// (VIEWS.messages.leave) and restarted whenever the namespace or token
// the open stream was opened for no longer matches (ensureMessageStream).
// ---------------------------------------------------------------------

const MSG_LOG_LIMIT = 500;
// pairKey encodes each address with encodeURIComponent before joining
// with a raw "|": encodeURIComponent always escapes "|", so the two
// encoded halves can never be confused with a literal "|" inside an
// address, and the key is still safe to drop straight into a data-*
// attribute.
const MSG_PAIR_SEP = '|';

let msgLog = [];             // cached /messages/log rows for the current namespace
let msgLoadedNS = null;      // namespace msgLog belongs to; a change resets selection and paging
let msgExhausted = false;    // true once "Load older" has reached the start of the log
let msgNextBefore = 0;       // `before` cursor for the next "Load older" page
let msgMembers = [];         // cached /members rows, for participant liveness
let msgDiag = Object.create(null); // address -> latest diagnostic snapshot, tagged by msgDiagNS
let msgDiagNS = '';          // namespace msgDiag was fetched for (never show one namespace's report in another)
let msgSelected = '';        // selected conversation's pair key ('' = none selected)
let msgRenderedFor = null;   // conversation key last painted into the thread pane
let msgMobileView = 'list';  // 'list' | 'thread'; only matters below 768px
let msgStreamKey = '';       // ns + token the open live stream was opened for
let msgStreamAbort = null;   // AbortController that stops the open stream's fetch loop

function pairKey(a, b) {
  const parts = [String(a || ''), String(b || '')].sort();
  return encodeURIComponent(parts[0]) + MSG_PAIR_SEP + encodeURIComponent(parts[1]);
}
function pairAddrs(key) {
  return key.split(MSG_PAIR_SEP).map(decodeURIComponent);
}

// hm formats an ISO timestamp as a 24h "18:32" clock time, the
// granularity the design plan's example state words use ("acked
// 18:32"); '' when the timestamp is missing or unparsable.
function hm(iso) {
  if (!iso) return '';
  const t = new Date(iso);
  if (Number.isNaN(t.getTime())) return '';
  return String(t.getHours()).padStart(2, '0') + ':' + String(t.getMinutes()).padStart(2, '0');
}

// excerpt collapses a message body to one line and clips it to 80
// characters, per the design plan's conversation row spec.
function excerpt(body) {
  const oneLine = String(body || '').replace(/\s+/g, ' ').trim();
  return oneLine.length > 80 ? oneLine.slice(0, 80) + '…' : oneLine;
}

// messageStateWord/messageStateClass render a message's one delivery
// state word ("acked 18:32", "leased by inbox-3a1f until 18:33",
// "unread"); colour reinforces the word, it never replaces it.
function messageStateWord(m) {
  if (m.acked_at) return 'acked ' + hm(m.acked_at);
  if (m.leased_by) return 'leased by ' + m.leased_by + (m.leased_until ? ' until ' + hm(m.leased_until) : '');
  return 'unread';
}
function messageStateClass(m) {
  return m.acked_at ? 'text-live' : 'text-wait';
}

function resetMessagesState() {
  msgLog = [];
  msgExhausted = false;
  msgNextBefore = 0;
  msgSelected = '';
  msgMobileView = 'list';
  msgDiag = Object.create(null); // diagnostics belong to the namespace they were fetched in
  msgDiagNS = '';
  applyMsgMobileView();
}

function applyMsgMobileView() {
  const shell = $('#msgShell');
  if (shell) shell.dataset.mobile = msgMobileView;
}

function mountMessages(el) {
  el.innerHTML = `
    <div id="msgShell" class="msg-shell" data-mobile="list">
      <div id="msgListPane" class="msg-list-pane">
        <div class="border-b border-line p-2">
          <input id="msgFilter" type="text" placeholder="filter by address or text" class="field w-full">
        </div>
        <div id="msgConvoList" class="flex-1 overflow-y-auto"></div>
        <div id="msgLoadOlderWrap" class="hidden border-t border-line p-2">
          <button id="msgLoadOlder" type="button" class="btn w-full">Load older</button>
        </div>
      </div>
      <div id="msgThreadPane" class="msg-thread-pane">
        <div id="msgThreadHeader" class="border-b border-line"></div>
        <div id="msgThreadBody" class="flex-1 overflow-y-auto p-3"></div>
      </div>
      <aside id="msgParticipantsPane" class="msg-participants-pane"></aside>
    </div>`;
  $('#msgFilter', el).addEventListener('input', e => { renderConversations(e.target.value); });
  $('#msgConvoList', el).addEventListener('click', e => {
    const row = e.target.closest('[data-msg-convo]');
    if (row) selectConversation(row.dataset.msgConvo);
  });
  $('#msgLoadOlder', el).addEventListener('click', loadOlderMessages);
  $('#msgThreadBody', el).addEventListener('click', e => {
    const rep = e.target.closest('[data-reply-to]');
    if (rep) { e.preventDefault(); scrollToMessage(rep.dataset.replyTo); }
  });
}

// loadMessages is VIEWS.messages.refresh: called on tab switch and on
// the 5s poll while the view is showing. It resets paging/selection
// only when the namespace actually changed, so the poll never disturbs
// the operator's current conversation or scroll position.
async function loadMessages() {
  try {
    if (!namespaces) await loadNamespaces();
  } catch (e) {
    $('#msgConvoList').innerHTML = '<div class="empty-state">' + esc(e.message) + '</div>';
    return;
  }
  if (!ns) {
    resetMessagesState();
    msgLoadedNS = null;
    stopMessageStream();
    $('#msgConvoList').innerHTML = '<div class="empty-state">no namespace selected</div>';
    renderThread();
    renderParticipants();
    return;
  }
  if (ns !== msgLoadedNS) {
    resetMessagesState();
    msgLoadedNS = ns;
  }
  if (!(await refreshMessageLog())) return;
  try {
    const md = await api('/v1/namespaces/' + encodeURIComponent(ns) + '/members');
    msgMembers = md.members || [];
  } catch (e) {
    msgMembers = []; // liveness is best-effort; the thread/list still render
  }
  const dm = await fetchDiagnostics();
  if (dm) { msgDiag = dm; msgDiagNS = ns; } // null keeps the previous map (still ns-tagged)
  renderParticipants();
  ensureMessageStream();
}

// dedupeSortDesc dedupes a row list by id and sorts it by seq desc (the
// order every reader of msgLog below assumes), keeping the first
// occurrence of a repeated id - callers put whichever copy should win
// first in the input.
function dedupeSortDesc(rows) {
  const seen = new Set();
  const merged = [];
  rows.forEach(m => {
    if (seen.has(m.id)) return;
    seen.add(m.id);
    merged.push(m);
  });
  merged.sort((a, b) => b.seq - a.seq);
  return merged;
}

// mergeNewestPage folds a freshly fetched "newest page" (poll or stream
// refresh) into the cache. The page is authoritative for every seq at
// or above its own minimum (an update such as a new ack rides along
// with a message already cached in that range), so cached rows there
// are replaced wholesale; rows below that minimum - pulled in by an
// earlier "Load older" - are kept rather than dropped.
function mergeNewestPage(page) {
  const minSeq = page.reduce((min, m) => Math.min(min, m.seq), Infinity);
  const older = Number.isFinite(minSeq) ? msgLog.filter(m => m.seq < minSeq) : msgLog;
  msgLog = dedupeSortDesc(page.concat(older));
}

// mergeOlderPage folds a "Load older" page - strictly older than
// everything currently cached - into the cache.
function mergeOlderPage(page) {
  msgLog = dedupeSortDesc(msgLog.concat(page));
}

// refreshMessageLog re-fetches the newest page, merges it into the
// cache (see mergeNewestPage; this must never drop rows an earlier
// "Load older" pulled in) and re-renders the conversation list and
// thread. Returns false (and shows the error) on a failed fetch so
// callers can bail without touching cached state. msgExhausted means
// "the cache reaches the start of the log". A newest page shorter than
// the limit proves the whole log fits in one page, so it sets the flag
// too; otherwise only a short "Load older" page can (see
// loadOlderMessages). Once true it stays true, since new messages only
// ever extend the newest end of the log.
async function refreshMessageLog() {
  try {
    const d = await api('/v1/namespaces/' + encodeURIComponent(ns) + '/messages/log?limit=' + MSG_LOG_LIMIT);
    const page = d.messages || [];
    if (page.length < MSG_LOG_LIMIT) msgExhausted = true;
    mergeNewestPage(page);
    recomputeNextBefore();
    renderConversations();
    renderThread();
    return true;
  } catch (e) {
    $('#msgConvoList').innerHTML = '<div class="empty-state">' + esc(e.message) + '</div>';
    return false;
  }
}

function recomputeNextBefore() {
  msgNextBefore = msgLog.reduce((min, m) => Math.min(min, m.seq), Infinity);
  if (!Number.isFinite(msgNextBefore)) msgNextBefore = 0;
  $('#msgLoadOlderWrap').classList.toggle('hidden', msgExhausted || !msgLog.length);
}

async function loadOlderMessages() {
  if (!ns || msgExhausted || !msgNextBefore) return;
  const btn = $('#msgLoadOlder');
  btn.disabled = true;
  try {
    const d = await api('/v1/namespaces/' + encodeURIComponent(ns) + '/messages/log?limit=' + MSG_LOG_LIMIT + '&before=' + msgNextBefore);
    const older = d.messages || [];
    if (older.length < MSG_LOG_LIMIT) msgExhausted = true;
    mergeOlderPage(older);
    recomputeNextBefore();
    renderConversations();
    renderThread();
  } catch (e) {
    // best-effort; the button stays enabled so the operator can retry
  } finally {
    btn.disabled = false;
  }
}

// renderConversations groups the cached log by unordered participant
// pair, sorts by latest message, and renders the list. `filterText`
// defaults to the live filter input's current value so the 5s poll and
// stream-driven refreshes re-apply whatever the operator last typed.
function renderConversations(filterText) {
  const input = $('#msgFilter');
  const q = (filterText !== undefined ? filterText : (input ? input.value : '')).trim().toLowerCase();

  const groups = new Map();
  msgLog.forEach(m => {
    const key = pairKey(m.sender, m.recipient);
    let g = groups.get(key);
    if (!g) { g = { key, a: m.sender, b: m.recipient, latest: m, unread: 0 }; groups.set(key, g); }
    if (Date.parse(m.created_at) > Date.parse(g.latest.created_at)) g.latest = m;
    if (!m.acked_at) g.unread++;
  });

  let rows = Array.from(groups.values());
  if (q) {
    rows = rows.filter(g => {
      if (g.a.toLowerCase().includes(q) || g.b.toLowerCase().includes(q)) return true;
      return msgLog.some(m => pairKey(m.sender, m.recipient) === g.key && (m.body || '').toLowerCase().includes(q));
    });
  }
  rows.sort((x, y) => Date.parse(y.latest.created_at) - Date.parse(x.latest.created_at));

  // Auto-select the most recent conversation when nothing is selected
  // yet (fresh namespace load), so the view never sits idle on "no
  // conversation selected" while conversations exist.
  if (!msgSelected && rows.length) msgSelected = rows[0].key;

  $('#msgConvoList').innerHTML = rows.length
    ? rows.map(g => `
      <button type="button" class="msg-convo-row w-full text-left${g.key === msgSelected ? ' on' : ''}" data-msg-convo="${esc(g.key)}">
        <div class="flex items-center justify-between gap-2">
          <span class="min-w-0 flex-1 break-all text-sm">${addressCell(g.a)} <span class="text-dim">&harr;</span> ${addressCell(g.b)}</span>
          ${g.unread ? `<span class="pill text-wait shrink-0">${g.unread}</span>` : ''}
        </div>
        <div class="mt-1 flex items-center justify-between gap-2 text-xs text-dim">
          <span class="min-w-0 flex-1 truncate">${esc(excerpt(g.latest.body))}</span>
          <span class="mono shrink-0">${esc(relAge(g.latest.created_at) || '')}</span>
        </div>
      </button>`).join('')
    : `<div class="empty-state">${msgLog.length
        ? 'No conversations match the filter.'
        : 'No messages in this namespace yet. Agents send with send_message; hooks deliver them here.'}</div>`;
}

function selectConversation(key) {
  msgSelected = key;
  msgMobileView = 'thread';
  applyMsgMobileView();
  renderConversations();
  renderThread();
  renderParticipants();
}

// msgLivenessBadge renders a short client label plus statusCell's
// liveness for one address, used in the thread header's fold-in
// (below 1024px, where the dedicated participants pane is hidden).
function msgLivenessBadge(addr) {
  const m = msgMembers.find(x => x.agent === addr);
  const i = addr.indexOf(':');
  const label = i === -1 ? addr : addr.slice(0, i);
  return '<span class="font-mono text-dim">' + esc(label) + '</span> ' + (m ? statusCell(m) : '<span class="text-dim">not a member</span>');
}

function msgRowHTML(m) {
  return `<div id="msg-${esc(m.id)}" class="msg-row">
    <div class="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-0.5">
      <span class="min-w-0 text-sm">${addressCell(m.sender)} <span class="text-dim">&rarr;</span> ${addressCell(m.recipient)}</span>
      <span class="mono shrink-0 text-xs" title="${esc(m.created_at)}">${esc(hm(m.created_at))}</span>
    </div>
    <div class="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-dim">
      ${m.task_id ? `<span>task <span class="mono">${esc(m.task_id)}</span></span>` : ''}
      ${m.reply_to ? `<a href="#msg-${esc(m.reply_to)}" class="font-mono text-brand hover:underline" data-reply-to="${esc(m.reply_to)}">re: ${esc(m.reply_to.slice(0, 8))}</a>` : ''}
      <span class="${messageStateClass(m)}">${esc(messageStateWord(m))}</span>
    </div>
    <pre class="mt-1.5 whitespace-pre-wrap break-words text-sm">${esc(m.body)}</pre>
  </div>`;
}

// renderThread paints the header and the chronological message list
// for the selected conversation. On a fresh selection (or the first
// render after a namespace switch) it always scrolls to the newest
// message at the bottom; on any other re-render (poll, stream event,
// "load older") it keeps the scroll position unless the operator was
// already pinned to the bottom, in which case it stays pinned.
function renderThread() {
  const header = $('#msgThreadHeader');
  const body = $('#msgThreadBody');
  const freshConversation = msgSelected !== msgRenderedFor;
  msgRenderedFor = msgSelected;

  if (!msgSelected) {
    header.innerHTML = '';
    body.innerHTML = '<div class="empty-state">No conversation selected. Pick one from the list to see its messages.</div>';
    return;
  }

  const [a, b] = pairAddrs(msgSelected);
  header.innerHTML = `
    <div class="flex items-center gap-2 p-2">
      <button id="msgBack" type="button" class="btn shrink-0 md:hidden">&larr; back</button>
      <div class="min-w-0 flex-1 break-all text-sm">${addressCell(a)} <span class="text-dim">&harr;</span> ${addressCell(b)}</div>
    </div>
    <div class="flex flex-wrap gap-x-4 gap-y-1 px-2 pb-2 text-xs lg:hidden">
      <span class="inline-flex items-center gap-1.5">${msgLivenessBadge(a)}</span>
      <span class="inline-flex items-center gap-1.5">${msgLivenessBadge(b)}</span>
    </div>`;
  $('#msgBack', header).addEventListener('click', () => { msgMobileView = 'list'; applyMsgMobileView(); });

  const msgs = msgLog.filter(m => pairKey(m.sender, m.recipient) === msgSelected).slice().sort((x, y) => x.seq - y.seq);

  const wasAtBottom = freshConversation || (body.scrollHeight - body.scrollTop - body.clientHeight < 24);
  const prevScrollTop = body.scrollTop;
  body.innerHTML = msgs.length ? msgs.map(msgRowHTML).join('') : '<div class="empty-state">No messages in this conversation.</div>';
  body.scrollTop = wasAtBottom ? body.scrollHeight : prevScrollTop;
}

function msgParticipantRow(addr) {
  const m = msgMembers.find(x => x.agent === addr);
  return `<div class="border-b border-line py-2 text-sm last:border-b-0">
      <div>${addressCell(addr)}</div>
      <div class="mt-1">${m ? statusCell(m) : '<span class="text-dim">not a member of this namespace</span>'}</div>
      <div class="mt-1">${diagnosticsHTML(diagFor(msgDiag, msgDiagNS, addr))}</div>
    </div>`;
}

function renderParticipants() {
  const pane = $('#msgParticipantsPane');
  if (!msgSelected) {
    pane.innerHTML = '<div class="empty-state">No conversation selected.</div>';
    return;
  }
  const [a, b] = pairAddrs(msgSelected);
  pane.innerHTML = msgParticipantRow(a) + msgParticipantRow(b);
}

// scrollToMessage jumps the thread pane to a reply_to's parent message
// and briefly outlines it, without touching location.hash (a plain
// anchor navigation would permanently repoint the page's #messages
// routing hash at a message id).
function scrollToMessage(id) {
  const el = document.getElementById('msg-' + id);
  if (!el) return;
  el.scrollIntoView({ block: 'center', behavior: window.matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth' });
  el.classList.add('msg-highlight');
  setTimeout(() => el.classList.remove('msg-highlight'), 1200);
}

// parseSSEFrames consumes every complete "\n\n"-terminated frame in a
// buffer and returns the unconsumed tail; a duplicate of brain-core.js's
// parseSSE, kept local because that file is an ES module and console.js
// is a plain script.
function parseSSEFrames(buffer) {
  const frames = [];
  let rest = buffer;
  for (;;) {
    const cut = rest.indexOf('\n\n');
    if (cut < 0) break;
    const block = rest.slice(0, cut);
    rest = rest.slice(cut + 2);
    let event = 'message';
    const data = [];
    for (const line of block.split('\n')) {
      if (line.startsWith(':')) continue;
      if (line.startsWith('event: ')) event = line.slice(7);
      else if (line.startsWith('data: ')) data.push(line.slice(6));
    }
    if (data.length) frames.push({ event, data: data.join('\n') });
  }
  return { frames, rest };
}

// ensureMessageStream (re)starts the live SSE stream when the
// namespace or token it was opened for has changed; a no-op otherwise,
// so the 5s poll never tears down a healthy connection.
function ensureMessageStream() {
  const key = ns + '\u0001' + token();
  if (key === msgStreamKey && msgStreamAbort) return;
  stopMessageStream();
  if (!ns) return;
  msgStreamKey = key;
  msgStreamAbort = new AbortController();
  streamMessages(ns, msgStreamAbort.signal);
}

function stopMessageStream() {
  if (msgStreamAbort) msgStreamAbort.abort();
  msgStreamAbort = null;
  msgStreamKey = '';
  if (msgLogRefreshTimer) { clearTimeout(msgLogRefreshTimer); msgLogRefreshTimer = null; }
  msgLogRefreshPending = false;
}

let msgLogRefreshTimer = null;   // pending trailing-debounce timer, or null
let msgLogRefreshInFlight = false; // a refreshMessageLog() fetch is in progress
let msgLogRefreshPending = false;  // a frame arrived while one was in flight; run once more after

// scheduleMessageLogRefresh coalesces bursts of stream frames (a send
// often produces a `message` frame immediately followed by an `ack`
// frame) into a single /messages/log fetch: a trailing ~300ms debounce,
// plus an in-flight flag so a refresh already underway is never started
// a second time in parallel - one more run is queued for right after it
// finishes if a frame arrived during it.
function scheduleMessageLogRefresh() {
  if (msgLogRefreshTimer) clearTimeout(msgLogRefreshTimer);
  msgLogRefreshTimer = setTimeout(runMessageLogRefresh, 300);
}
async function runMessageLogRefresh() {
  msgLogRefreshTimer = null;
  if (msgLogRefreshInFlight) { msgLogRefreshPending = true; return; }
  msgLogRefreshInFlight = true;
  try {
    await refreshMessageLog();
  } finally {
    msgLogRefreshInFlight = false;
    if (msgLogRefreshPending) {
      msgLogRefreshPending = false;
      scheduleMessageLogRefresh();
    }
  }
}

// streamMessages mirrors brain.js's streamEvents: EventSource cannot
// carry an Authorization header, so the stream is opened with fetch and
// its ReadableStream body is decoded and parsed as SSE by hand. Any
// `message` or `ack` frame schedules a debounced log re-fetch (see
// scheduleMessageLogRefresh; bodies are never sent on this stream); the
// loop reconnects with exponential backoff on error and exits for good
// once `signal` is aborted.
async function streamMessages(streamNS, signal) {
  let backoff = 1000;
  while (!signal.aborted) {
    try {
      const h = { Accept: 'text/event-stream' };
      if (token()) h['Authorization'] = 'Bearer ' + token();
      const r = await fetch('/v1/namespaces/' + encodeURIComponent(streamNS) + '/messages/stream', { headers: h, signal });
      if (!r.ok || !r.body) throw new Error('messages/stream ' + r.status);
      backoff = 1000;
      const reader = r.body.getReader();
      const dec = new TextDecoder();
      let buf = '';
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        buf += dec.decode(value, { stream: true });
        const { frames, rest } = parseSSEFrames(buf);
        buf = rest;
        for (const f of frames) {
          if ((f.event === 'message' || f.event === 'ack') && ns === streamNS && current === 'messages') scheduleMessageLogRefresh();
        }
      }
      throw new Error('stream closed');
    } catch (e) {
      if (signal.aborted) return;
      await new Promise(res => setTimeout(res, backoff));
      backoff = Math.min(15000, backoff * 2);
    }
  }
}

// ---------------------------------------------------------------------
// Agents view: namespace members (address, role, liveness, unread) plus
// a collapsed panel for the specialist agent registry. Liveness comes
// straight from GET /members (`listening`, `last_seen_at`); unread is
// derived client side by counting log rows addressed to a member with
// no `acked_at`. Filtering and the "live only" toggle re-render from
// the cached members/unread state, so typing in the filter never
// re-fetches; the 5s poll re-fetches and re-renders through the same
// path.
// ---------------------------------------------------------------------

let agentsMembers = [];   // cached GET /members rows for the current namespace
let agentsUnread = {};    // address -> count of log rows with empty acked_at
let agentsDiag = Object.create(null); // address -> latest diagnostic snapshot, tagged by agentsDiagNS
let agentsDiagNS = '';    // namespace agentsDiag was fetched for (never show one namespace's report in another)

const AGENTS_ACTIVE_WINDOW_MIN = 10; // "seen in the last 10 minutes" / "live only"

// relAge turns an ISO timestamp into "2m ago" / "3h ago" / "3d ago"; null
// when the timestamp is missing or unparsable (caller falls back to a
// "never seen" state word rather than showing a broken relative time).
function relAge(iso) {
  if (!iso) return null;
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return null;
  const secs = Math.max(0, Math.floor((Date.now() - t) / 1000));
  if (secs < 60) return 'just now';
  const mins = Math.floor(secs / 60);
  if (mins < 60) return mins + 'm ago';
  const hours = Math.floor(mins / 60);
  if (hours < 24) return hours + 'h ago';
  return Math.floor(hours / 24) + 'd ago';
}

// withinMinutes reports whether an ISO timestamp falls inside the last
// `mins` minutes; false (not "unknown") when it is missing or unparsable,
// which is the safer default for both the header count and the toggle.
function withinMinutes(iso, mins) {
  if (!iso) return false;
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return false;
  return (Date.now() - t) <= mins * 60000;
}

// statusCell renders the status column: a pulsing live dot plus the word
// "listening" for an open inbox stream, otherwise the relative age of
// last_seen_at in the dim colour. Colour reinforces the word; it never
// stands in for it alone (design plan principle).
function statusCell(m) {
  if (m.listening) {
    return '<span class="inline-flex items-center gap-1.5"><span class="dot live-dot bg-live"></span><span class="text-live">listening</span></span>';
  }
  const age = relAge(m.last_seen_at);
  return '<span class="text-dim">' + (age ? 'seen ' + age : 'never seen') + '</span>';
}

// addressCell renders an address in monospace. A `client:session` style
// address shows the part before the first colon (the client, e.g.
// "opencode") in the ink colour and the rest dim, so the prefix scans;
// a plain name with no colon is flagged "name, no inbox" since it has
// no client prefix to receive a reply on.
function addressCell(addr) {
  const i = addr.indexOf(':');
  if (i === -1) {
    return '<span class="font-mono break-all">' + esc(addr) + '</span>' +
      '<span class="ml-2 text-xs text-dim">name, no inbox</span>';
  }
  return '<span class="font-mono break-all"><span class="text-ink font-semibold">' + esc(addr.slice(0, i)) + '</span>' +
    '<span class="text-dim">' + esc(addr.slice(i)) + '</span></span>';
}

// ---------------------------------------------------------------------
// Delivery diagnostics (shared by the Agents and Messages views)
//
// GET /v1/namespaces/<ns>/messages/diagnostics returns one bounded
// latest snapshot per address, reported by the client's bridge or hook:
// capability (client + delivery_mode), the last reported state word and
// short machine reason, attempt timestamps and a pending-ACK count. It
// is the client's own observation, labeled "last reported" everywhere;
// it NEVER overrides the member liveness badge (listening/last_seen_at)
// or a message's ACK state, and a snapshot the server flags `stale`
// (older than 120 s) is shown dimmed with a "(stale)" marker rather
// than hidden. Servers older than the diagnostics route answer 404, in
// which case the console stops asking (diagUnsupported) and the views
// render exactly as before. Every remote string passes through esc().
// ---------------------------------------------------------------------

let diagUnsupported = false; // true after a 404: this server predates diagnostics

// diagWords renders a machine token ("waiting_for_idle") for operators.
function diagWords(s) {
  return String(s || '').replace(/_/g, ' ');
}

// relEta is relAge for future timestamps ("in 5m"), used for a reported
// next retry time; null when the timestamp is missing or unparsable.
function relEta(iso) {
  if (!iso) return null;
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return null;
  const secs = Math.floor((t - Date.now()) / 1000);
  if (secs <= 0) return 'now';
  if (secs < 60) return 'in ' + secs + 's';
  const mins = Math.floor(secs / 60);
  if (mins < 60) return 'in ' + mins + 'm';
  const hours = Math.floor(mins / 60);
  if (hours < 24) return 'in ' + hours + 'h';
  return 'in ' + Math.floor(hours / 24) + 'd';
}

// fetchDiagnostics returns an address -> snapshot map, null when the
// fetch failed or the namespace changed underneath it (callers keep
// their previous map, which is still namespace-tagged), and {} once the
// server is known to predate the route (404) so the poll stops asking.
// The map is prototypeless and the agent key is validated as a string,
// so a hostile address like "__proto__" or "constructor" can never
// shadow Object members or pollute a prototype.
async function fetchDiagnostics() {
  if (!ns || diagUnsupported) return {};
  const reqNS = ns; // captured: a namespace switch mid-flight discards this result
  try {
    const d = await api('/v1/namespaces/' + encodeURIComponent(reqNS) + '/messages/diagnostics');
    if (ns !== reqNS) return null; // late response for a namespace no longer shown
    const map = Object.create(null);
    (d.diagnostics || []).forEach(x => {
      if (x && typeof x.agent === 'string' && x.agent) map[x.agent] = x;
    });
    return map;
  } catch (e) {
    if (ns !== reqNS) return null; // a dead namespace's failure never marks the new one
    if (/: 404\b/.test(e.message)) { diagUnsupported = true; return {}; }
    return null;
  }
}

// diagFor reads one address from a namespace-tagged map. Diagnostics are
// only ever shown for the namespace they were fetched in: after a switch,
// a map that belongs to the old namespace (kept because its replacement
// fetch failed or is still in flight) reads as "no reports", never as
// the other namespace's stale state for the same address.
function diagFor(map, mapNS, addr) {
  if (!map || mapNS !== ns) return undefined;
  return map[addr];
}

// diagnosticsHTML projects one snapshot (or null) as a compact,
// non-authoritative observation line. Colour only reinforces the state
// word; the "last reported" label and stale marker carry the authority
// caveat in text.
function diagnosticsHTML(d) {
  if (!d) return '<span class="text-dim">no reports</span>';
  const cap = [d.client, d.delivery_mode ? diagWords(d.delivery_mode) : ''].filter(Boolean).join(' · ');
  const parts = [];
  if (cap) parts.push('<span class="text-dim">' + esc(cap) + '</span>');
  parts.push('<span class="' + (d.stale ? 'text-dim' : stateClass(d.state)) + '">' + esc(diagWords(d.state)) + '</span>');
  if (d.last_error) parts.push('<span class="' + (d.stale ? 'text-dim' : 'text-brand') + '">' + esc(diagWords(d.last_error)) + '</span>');
  const eta = relEta(d.next_attempt_at);
  if (eta) parts.push('<span class="text-dim">retry ' + esc(eta) + '</span>');
  if (d.pending_ack_count > 0) parts.push('<span class="text-dim">' + esc(d.pending_ack_count) + ' unacked</span>');
  parts.push('<span class="text-dim">last reported ' + esc(relAge(d.updated_at) || 'at an unknown time') + (d.stale ? ' (stale)' : '') + '</span>');
  return '<div class="flex flex-wrap items-baseline gap-x-2 gap-y-0.5 text-xs">' + parts.join('') + '</div>';
}

function mountAgents(el) {
  el.innerHTML = `
    <div id="agentsHeader" class="mb-3 text-sm text-dim"></div>
    <div class="mb-3 flex flex-wrap items-center gap-3">
      <input id="agentsFilter" type="text" placeholder="filter by address or role" class="field w-56">
      <label class="flex items-center gap-2 text-sm text-dim">
        <input id="agentsLiveOnly" type="checkbox" class="accent-brand">
        live only
      </label>
    </div>
    <div id="agentsTable"></div>
    <details id="agentsSpecialistPanel" class="panel mt-4 hidden">
      <summary class="cursor-pointer text-sm font-semibold text-ink">Specialist agents</summary>
      <div id="agentsSpecialistTable" class="mt-3"></div>
    </details>`;
  $('#agentsFilter', el).addEventListener('input', renderAgentsView);
  $('#agentsLiveOnly', el).addEventListener('change', renderAgentsView);
}

async function loadAgentsView() {
  try {
    // Board's refresh is what normally loads the namespace list on first
    // paint; a direct link to #agents (e.g. a bookmark, or this view's
    // own screenshot check) can land here first, so load it here too.
    if (!namespaces) await loadNamespaces();
  } catch (e) {
    $('#agentsHeader').textContent = '';
    $('#agentsTable').innerHTML = '<div class="empty-state">' + esc(e.message) + '</div>';
    $('#agentsSpecialistPanel').classList.add('hidden');
    return;
  }
  if (!ns) {
    $('#agentsHeader').textContent = '';
    $('#agentsTable').innerHTML = '<div class="empty-state">no namespace selected</div>';
    $('#agentsSpecialistPanel').classList.add('hidden');
    return;
  }
  try {
    const d = await api('/v1/namespaces/' + encodeURIComponent(ns) + '/members');
    agentsMembers = d.members || [];
  } catch (e) {
    agentsMembers = [];
    $('#agentsHeader').textContent = '';
    $('#agentsTable').innerHTML = '<div class="empty-state">' + esc(e.message) + '</div>';
    $('#agentsSpecialistPanel').classList.add('hidden');
    return;
  }
  try {
    const log = await api('/v1/namespaces/' + encodeURIComponent(ns) + '/messages/log?limit=500');
    agentsUnread = {};
    (log.messages || []).forEach(m => {
      if (!m.acked_at) agentsUnread[m.recipient] = (agentsUnread[m.recipient] || 0) + 1;
    });
  } catch (e) {
    agentsUnread = {}; // unread is best-effort; the members table still renders
  }
  const dm = await fetchDiagnostics();
  if (dm) { agentsDiag = dm; agentsDiagNS = ns; } // null keeps the previous map (still ns-tagged)
  renderAgentsView();
  loadSpecialistAgents();
}

function renderAgentsView() {
  if (!ns) return;
  const q = ($('#agentsFilter').value || '').trim().toLowerCase();
  const liveOnly = $('#agentsLiveOnly').checked;

  const listeningCount = agentsMembers.filter(m => m.listening).length;
  const seenRecentCount = agentsMembers.filter(m => withinMinutes(m.last_seen_at, AGENTS_ACTIVE_WINDOW_MIN)).length;
  $('#agentsHeader').textContent = agentsMembers.length + (agentsMembers.length === 1 ? ' member, ' : ' members, ') +
    listeningCount + ' listening, ' + seenRecentCount + ' seen in the last ' + AGENTS_ACTIVE_WINDOW_MIN + ' minutes';

  const rows = agentsMembers
    .filter(m => !liveOnly || m.listening || withinMinutes(m.last_seen_at, AGENTS_ACTIVE_WINDOW_MIN))
    .filter(m => !q || m.agent.toLowerCase().includes(q) || (m.role || '').toLowerCase().includes(q))
    .slice()
    .sort((a, b) => {
      if (a.listening !== b.listening) return a.listening ? -1 : 1;
      return (Date.parse(b.last_seen_at) || 0) - (Date.parse(a.last_seen_at) || 0);
    });

  $('#agentsTable').innerHTML = rows.length
    ? `<div class="table-wrap"><table class="data-table">
        <colgroup><col style="width:14%"><col style="width:26%"><col style="width:12%"><col style="width:8%"><col style="width:26%"><col style="width:14%"></colgroup>
        <thead><tr><th>Status</th><th>Address</th><th>Role</th><th class="num">Unread</th><th>Last reported delivery</th><th>Joined</th></tr></thead>
        <tbody>${rows.map(m => { const unread = agentsUnread[m.agent] || 0; return `<tr>
            <td data-label="Status">${statusCell(m)}</td>
            <td data-label="Address">${addressCell(m.agent)}</td>
            <td data-label="Role">${esc(m.role || '-')}</td>
            <td class="num font-mono${unread ? ' text-wait' : ' text-dim'}" data-label="Unread">${unread}</td>
            <td data-label="Last reported delivery">${diagnosticsHTML(diagFor(agentsDiag, agentsDiagNS, m.agent))}</td>
            <td class="mono" data-label="Joined">${esc((m.joined_at || '').slice(0, 16))}</td>
          </tr>`; }).join('')}</tbody>
      </table></div>`
    : '<div class="empty-state">No agents seen in this namespace yet. Members show here once they register or connect.</div>';
}

// loadSpecialistAgents fills the collapsed panel from the old specialist
// registry (GET /v1/agents). That route only exists on deployments that
// run the investigation runtime, so a 404 (registry disabled) is treated
// the same as an empty list: the panel stays hidden either way.
async function loadSpecialistAgents() {
  try {
    const d = await api('/v1/agents');
    const agents = d.agents || [];
    if (!agents.length) {
      $('#agentsSpecialistPanel').classList.add('hidden');
      return;
    }
    $('#agentsSpecialistPanel').classList.remove('hidden');
    $('#agentsSpecialistTable').innerHTML = `<div class="mb-2 text-xs text-dim">snapshot <b class="font-mono font-normal text-ink">v${esc(d.snapshot_version)}</b></div>
      <div class="table-wrap"><table class="data-table">
        <thead><tr><th>Name</th><th>Version</th><th>Autonomy</th><th>Status</th><th>Skills</th></tr></thead>
        <tbody>${agents.map(a => `<tr>
          <td data-label="Name"><span class="font-mono font-semibold">${esc(a.name)}</span><div class="mt-0.5 font-mono text-xs text-dim">${esc(a.description || '')}</div></td>
          <td class="mono" data-label="Version">${esc(a.version || '-')}</td>
          <td data-label="Autonomy">${esc(a.autonomy || '-')}</td>
          <td data-label="Status">${a.disabled ? pill('disabled') : pill('active')}</td>
          <td class="mono" data-label="Skills">${esc((a.skills || []).join(', ') || '-')}</td>
        </tr>`).join('')}</tbody>
      </table></div>`;
  } catch (e) {
    $('#agentsSpecialistPanel').classList.add('hidden');
  }
}

// ---------------------------------------------------------------------
// Runtime view: the domain-agent investigation runtime (ledger tasks,
// approval proposals, model spend). Coding-agent coordination never
// writes here, so the view explains its state instead of showing three
// empty tabs; the tab badge counts what is waiting on a human.
// ---------------------------------------------------------------------

function mountRuntime(el) {
  el.innerHTML = `
    <div id="runtimeIntro" class="panel mb-4 text-sm text-dim"></div>
    <div id="runtimeSections">
    <h2 class="mb-2 text-md font-semibold">Waiting on you</h2>
    <div id="approvalsProposals"></div>
    <div id="approvalsParked" class="mt-3"></div>
    <h2 class="mb-2 mt-6 text-md font-semibold">Investigation tasks</h2>
    <div id="tasksList"></div>
    <div id="tasksTimeline" class="panel mt-4 hidden"></div>
    <h2 class="mb-2 mt-6 text-md font-semibold">Model spend</h2>
    <div id="costsSummary" class="mb-3"></div>
    <div id="costsTable"></div>
    </div>`;
}

// runtimeEmpty: nothing has ever run here, so the three sections would
// only repeat "none" three times; the intro says it once instead.
function runtimeEmpty(rt) {
  return !!rt && !rt.ledger_tasks && !rt.pending_proposals && !rt.parked_tasks && !rt.month_cost_microusd;
}

function renderRuntimeIntro() {
  const rt = (overview && overview.runtime) || null;
  const el = $('#runtimeIntro');
  if (!el) return;
  if (!rt) {
    el.innerHTML = 'The investigation runtime runs domain agents (specs on disk) against tasks submitted through <code class="font-mono text-ink">submit_task</code>, <code class="font-mono text-ink">POST /v1/tasks</code> or webhook intake. Coding-agent tasks live on the Board.';
    return;
  }
  const idle = !rt.ai_enabled || !rt.domain_agents;
  el.innerHTML = idle
    ? `<b class="font-normal text-ink">Runtime idle${runtimeEmpty(rt) ? ', nothing has run here yet' : ''}.</b> ${rt.ai_enabled ? 'A model is configured' : 'No model is configured (<code class="font-mono text-ink">ai.enabled: false</code>)'} and ${fmtNum(rt.domain_agents)} domain agent${rt.domain_agents === 1 ? ' is' : 's are'} loaded, so nothing is investigated, proposed or billed here. Coding-agent work shows on the Board, Messages and Agents tabs.`
    : `<b class="font-normal text-ink">Runtime active.</b> ${fmtNum(rt.domain_agents)} domain agents, ${fmtNum(rt.ledger_tasks)} ledger tasks, ${usd(rt.month_cost_microusd || 0)} model spend this month.`;
}

function renderRuntimeBadge() {
  const rt = (overview && overview.runtime) || {};
  const n = (rt.pending_proposals || 0) + (rt.parked_tasks || 0);
  const b = $('#runtimeBadge');
  if (!b) return;
  b.textContent = n ? String(n) : '';
  b.classList.toggle('hidden', !n);
}

async function loadRuntime() {
  if (!overview && !overviewMissing) { try { await loadOverview(); } catch (e) { /* intro falls back to generic text */ } }
  renderRuntimeIntro();
  renderRuntimeBadge();
  const rt = overview && overview.runtime;
  const empty = runtimeEmpty(rt);
  $('#runtimeSections').classList.toggle('hidden', empty);
  if (empty) return;
  await Promise.all([loadApprovals(), loadTasks(), loadCosts()]);
}

// ---------------------------------------------------------------------
// View registry, tab strip / bottom bar, and the 5s poll
// ---------------------------------------------------------------------

const VIEWS = {
  overview: { label: 'Overview', mount: mountOverview, refresh: loadOverviewView },
  board: { label: 'Board', mount: mountBoard, refresh: loadBoard },
  memory: { label: 'Memory', mount: mountMemory, refresh: loadMemory, poll: () => { if (!$('#memQuery').value.trim()) loadMemory(); } },
  // `fill: true` gives this view's section a bounded, full height flex
  // column (see buildViewShell) instead of the default flow layout, so
  // its own panes - not the outer #view - own their scrolling (see
  // .msg-shell/#msgThreadBody in console.src.css).
  messages: { label: 'Messages', mount: mountMessages, refresh: loadMessages, leave: stopMessageStream, fill: true },
  agents: { label: 'Agents', mount: mountAgents, refresh: loadAgentsView },
  runtime: { label: 'Runtime', mount: mountRuntime, refresh: loadRuntime },
};

// Old bookmarks (#approvals, #tasks, #costs) land on the merged Runtime tab.
const VIEW_ALIASES = { approvals: 'runtime', tasks: 'runtime', costs: 'runtime' };

// The initial view can be named by URL hash (e.g. /ui#agents), so a
// screenshot or a bookmark can land directly on a tab; localStorage's
// `amk`/`ns` keys still own the token and namespace as before.
const hashView = (location.hash || '').slice(1);
let current = VIEWS[hashView] ? hashView : VIEW_ALIASES[hashView] || 'overview';

function buildViewShell() {
  const root = $('#view');
  Object.keys(VIEWS).forEach(name => {
    const section = document.createElement('section');
    section.id = 'view-' + name;
    const fillClass = VIEWS[name].fill ? ' flex min-h-0 flex-1 flex-col' : '';
    section.className = (name === current ? '' : 'hidden') + fillClass;
    root.appendChild(section);
    VIEWS[name].mount(section);
  });
}

function switchView(name) {
  name = VIEW_ALIASES[name] || name;
  if (!VIEWS[name] || name === current) {
    if (VIEWS[name]) refreshCurrent();
    return;
  }
  if (VIEWS[current].leave) VIEWS[current].leave();
  $('#view-' + current).classList.add('hidden');
  current = name;
  $('#view-' + current).classList.remove('hidden');
  $$('#tabs .tab-btn').forEach(b => b.classList.toggle('on', b.dataset.view === name));
  $$('#bottombar .bottom-btn').forEach(b => b.classList.toggle('on', b.dataset.view === name));
  if (location.hash.slice(1) !== name) history.replaceState(null, '', '#' + name);
  refreshCurrent();
}

$$('#tabs .tab-btn, #bottombar .bottom-btn').forEach(b => b.addEventListener('click', () => switchView(b.dataset.view)));

function refreshCurrent() {
  if (!authed) return;
  VIEWS[current].refresh();
}

// The 5s poll: views may define a lighter `poll` (Memory skips the poll
// while a search is open). The rail and the Runtime badge ride along on
// the cached overview so every tab stays current, not just Overview.
async function pollCurrent() {
  if (!authed || document.hidden) return;
  const v = VIEWS[current];
  (v.poll || v.refresh)();
  if (current !== 'overview' && !overviewMissing) {
    try { await loadOverview(); renderNamespaces(); renderRuntimeBadge(); } catch (e) { /* pulse shows offline */ }
  }
}
function refreshAll() {
  diagUnsupported = false; // a new token may point at a different server
  loadNamespacesAndRefresh();
}

$$('#tabs .tab-btn').forEach(b => b.classList.toggle('on', b.dataset.view === current));
$$('#bottombar .bottom-btn').forEach(b => b.classList.toggle('on', b.dataset.view === current));
buildViewShell();
boot();
setInterval(pollCurrent, 5000);

// Test seam for node --test (internal/api/ui/console.test.mjs), which
// evaluates this file in a vm sandbox with DOM stubs and a `module`
// global. Browsers never define `module`, so this block is inert when
// the console is served; it only re-exports existing bindings.
if (typeof module === 'object' && module && module.exports) {
  module.exports = {
    esc, diagWords, relEta, relAge, diagnosticsHTML, fetchDiagnostics, diagFor,
    stateClass, probeAuth, nsIsIdle, taskBar, pagerHTML, pageStep, boardURL,
    pageOffset, PAGE_SIZE,
    getDiagUnsupported: () => diagUnsupported,
    setDiagUnsupported: v => { diagUnsupported = v; },
    setNS: v => { ns = v; },
  };
}
