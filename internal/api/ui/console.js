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
  if (!r.ok) throw new Error(path + ': ' + r.status + ' ' + await r.text());
  return r.json();
}

// Colour reinforces the state word, it never replaces it (design plan
// principle). Tokens only define --live (listening / done) and --wait
// (leased, waiting, blocked); the brand crimson doubles as the alert
// colour for failed/rejected/canceled since the palette has no separate
// danger hue. Anything not listed stays plain ink or dim.
const STATE_COLOR = {
  done: 'text-live', completed: 'text-live', approved: 'text-live', active: 'text-live', listening: 'text-live', finding: 'text-live',
  review: 'text-wait', blocked: 'text-wait', input_required: 'text-wait', proposed: 'text-wait', leased: 'text-wait', budget_exhausted: 'text-wait', waiting: 'text-wait',
  failed: 'text-brand', canceled: 'text-brand', rejected: 'text-brand', error: 'text-brand',
  pending: 'text-dim', submitted: 'text-dim', disabled: 'text-dim', status_change: 'text-dim',
};
const stateClass = s => (Object.hasOwn(STATE_COLOR, s) ? STATE_COLOR[s] : '');
const pill = state => `<span class="pill ${stateClass(state)}">${esc(state)}</span>`;

// ---------------------------------------------------------------------
// Namespace rail (global chrome, shared across views)
// ---------------------------------------------------------------------

let ns = localStorage.getItem('ns') || '';
let namespaces = null; // cache; null forces a reload

function setNS(name) {
  ns = name;
  localStorage.setItem('ns', name);
  renderNamespaces();
  closeRail();
  refreshCurrent();
}

async function loadNamespaces() {
  const onlyTasks = $('#nsTasksOnly').checked;
  const d = await api('/v1/namespaces' + (onlyTasks ? '?tasks=1' : ''));
  namespaces = d.namespaces || [];
  if (!namespaces.some(n => n.name === ns)) ns = namespaces.length ? namespaces[0].name : '';
  if (ns) localStorage.setItem('ns', ns);
  renderNamespaces();
}

function renderNamespaces() {
  const q = $('#nsFilter').value.trim().toLowerCase();
  const list = (namespaces || []).filter(n => !q || n.name.toLowerCase().includes(q));

  $('#nsList').innerHTML = list.length
    ? list.map(n => `<button type="button" class="flex w-full cursor-pointer items-baseline gap-2 rounded px-2 py-1.5 text-left text-sm hover:bg-bg${n.name === ns ? ' bg-bg text-brand' : ' text-ink'}" data-ns="${esc(n.name)}">
        <span class="min-w-0 flex-1 break-all">${esc(n.name)}</span>
        <span class="shrink-0 font-mono text-xs text-dim">${n.tasks ? n.tasks + 't ' : ''}${n.facts}f</span>
      </button>`).join('')
    : '<div class="empty-state">no namespaces</div>';

  const sel = $('#nsSelect');
  sel.innerHTML = (namespaces || []).map(n => `<option value="${esc(n.name)}"${n.name === ns ? ' selected' : ''}>${esc(n.name)}</option>`).join('')
    || '<option value="">no namespaces</option>';
  $('#nsLabelName').textContent = ns || 'none';
}

$('#nsFilter').addEventListener('input', renderNamespaces);
$('#nsTasksOnly').addEventListener('change', () => { namespaces = null; loadNamespacesAndRefresh(); });
$('#nsList').addEventListener('click', e => {
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
// Token field
// ---------------------------------------------------------------------

$('#token').value = token();
$('#token').addEventListener('change', e => {
  localStorage.setItem('amk', e.target.value);
  namespaces = null;
  refreshAll();
});

// ---------------------------------------------------------------------
// Board view
// ---------------------------------------------------------------------

function mountBoard(el) {
  el.innerHTML = `
    <div class="mb-3 flex flex-wrap items-center gap-2">
      <select id="boardState" class="field font-mono text-xs">
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
  $('#boardState', el).addEventListener('change', loadBoard);
}

async function loadBoard() {
  try {
    if (!namespaces) await loadNamespaces();
    await loadBoardTable();
  } catch (e) {
    $('#boardTable').innerHTML = '<div class="empty-state">' + esc(e.message) + '</div>';
  }
}

async function loadBoardTable() {
  if (!ns) { $('#boardSummary').innerHTML = ''; $('#boardTable').innerHTML = '<div class="empty-state">no namespace selected</div>'; return; }
  try {
    const st = $('#boardState').value;
    const b = await api('/v1/namespaces/' + encodeURIComponent(ns) + '/tasks' + (st ? '?state=' + st : ''));
    const c = b.counts || {};
    const names = (b.members || []).map(m => m.agent || m.name || '').filter(Boolean);
    const shown = names.slice(0, 10).map(esc).join(', ') + (names.length > 10 ? ' +' + (names.length - 10) + ' more' : '');

    $('#boardSummary').innerHTML = `
      <div class="flex flex-wrap items-center gap-x-4 gap-y-1 text-sm text-dim">
        <span class="font-semibold text-ink">${esc(b.namespace)}</span>
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
        </table></div>`
      : '<div class="empty-state">no tasks match</div>';
  } catch (e) {
    $('#boardTable').innerHTML = '<div class="empty-state">' + esc(e.message) + '</div>';
  }
}

// ---------------------------------------------------------------------
// Approvals view (old Inbox: proposals + parked tasks)
// ---------------------------------------------------------------------

function mountApprovals(el) {
  el.innerHTML = `
    <h2 class="mb-2 text-md font-semibold">Proposals awaiting approval</h2>
    <div id="approvalsProposals"></div>
    <h2 class="mb-2 mt-6 text-md font-semibold">Parked tasks (input required)</h2>
    <div id="approvalsParked"></div>`;
}

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
// Tasks view
// ---------------------------------------------------------------------

function mountTasks(el) {
  el.innerHTML = `
    <div id="tasksList"></div>
    <div id="tasksTimeline" class="panel mt-4 hidden"></div>`;
}

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
  switchView('tasks');
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
// Costs view
// ---------------------------------------------------------------------

function mountCosts(el) {
  el.innerHTML = `
    <div id="costsSummary" class="mb-3"></div>
    <div id="costsTable"></div>`;
}

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
        <colgroup><col style="width:16%"><col style="width:32%"><col style="width:14%"><col style="width:12%"><col style="width:26%"></colgroup>
        <thead><tr><th>Status</th><th>Address</th><th>Role</th><th class="num">Unread</th><th>Joined</th></tr></thead>
        <tbody>${rows.map(m => { const unread = agentsUnread[m.agent] || 0; return `<tr>
            <td data-label="Status">${statusCell(m)}</td>
            <td data-label="Address">${addressCell(m.agent)}</td>
            <td data-label="Role">${esc(m.role || '-')}</td>
            <td class="num font-mono${unread ? ' text-wait' : ' text-dim'}" data-label="Unread">${unread}</td>
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
// View registry, tab strip / bottom bar, and the 5s poll
// ---------------------------------------------------------------------

const VIEWS = {
  board: { label: 'Board', mount: mountBoard, refresh: loadBoard },
  // `fill: true` gives this view's section a bounded, full height flex
  // column (see buildViewShell) instead of the default flow layout, so
  // its own panes - not the outer #view - own their scrolling (see
  // .msg-shell/#msgThreadBody in console.src.css).
  messages: { label: 'Messages', mount: mountMessages, refresh: loadMessages, leave: stopMessageStream, fill: true },
  agents: { label: 'Agents', mount: mountAgents, refresh: loadAgentsView },
  approvals: { label: 'Approvals', mount: mountApprovals, refresh: loadApprovals },
  tasks: { label: 'Tasks', mount: mountTasks, refresh: loadTasks },
  costs: { label: 'Costs', mount: mountCosts, refresh: loadCosts },
};

// The initial view can be named by URL hash (e.g. /ui#agents), so a
// screenshot or a bookmark can land directly on a tab; localStorage's
// `amk`/`ns` keys still own the token and namespace as before.
let current = VIEWS[(location.hash || '').slice(1)] ? location.hash.slice(1) : 'board';

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
  VIEWS[current].refresh();
}
function refreshAll() {
  loadNamespacesAndRefresh();
}

$$('#tabs .tab-btn').forEach(b => b.classList.toggle('on', b.dataset.view === current));
$$('#bottombar .bottom-btn').forEach(b => b.classList.toggle('on', b.dataset.view === current));
buildViewShell();
refreshAll();
setInterval(refreshCurrent, 5000);
