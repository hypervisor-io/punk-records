// Pure logic for the brain view: activity maths, event weights, SSE
// framing, log copy and coalescing, mesh decoding, surface sampling and
// signal paths. No DOM, no WebGL, so this file runs under node --test.

export const HALF_LIFE = 30;      // seconds for a region's activity to halve
export const MAX_REGIONS = 64;    // shader uniform array size

export function decay(a, dt, halfLife = HALF_LIFE) {
  if (a <= 0) return 0;
  return a * Math.pow(0.5, dt / halfLife);
}

export function glow(a) {
  const g = 1 - Math.exp(-Math.max(0, a));
  return g >= 1 ? 1 - Number.EPSILON / 2 : g; // stay strictly in [0,1)
}

const STATUS_KEY = /^\/tasks\/[^/]+\/status$/;

export function eventWeight(ev) {
  const d = ev.data || {};
  switch (ev.kind) {
    case 'memory':
      if (d.action === 'tombstone') return 0.5;
      if (STATUS_KEY.test(ev.key || '')) return 2.0;
      if (d.writer === 'agent-hook') return 0.35;
      return 1.0;
    case 'claim': return 0.6;
    case 'defense': return 0.8;
    case 'task_status': return 1.0;
    case 'cost_alert': return 1.5;
    default: return 0.5;
  }
}

export function seedActivity(writes5m) {
  return Math.min(3, 0.2 * Math.max(0, writes5m || 0));
}

// parseSSE consumes every complete "\n\n"-terminated frame in buffer and
// returns the unconsumed tail. Comment lines (": ping") are dropped.
export function parseSSE(buffer) {
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

// mulberry32 is a tiny seeded RNG so the point cloud is identical on every
// load (no flicker between reloads, reproducible tests).
export function mulberry32(seed) {
  let t = seed >>> 0;
  return function () {
    t = (t + 0x6D2B79F5) >>> 0;
    let r = Math.imul(t ^ (t >>> 15), 1 | t);
    r ^= r + Math.imul(r ^ (r >>> 7), 61 | r);
    return ((r ^ (r >>> 14)) >>> 0) / 4294967296;
  };
}

const SESSION = /^\/agent-sessions\/[^/]+\/(tool-|prompt-|start$|stop$)/;
const TASK_STATUS = /^\/tasks\/([^/]+)\/status$/;
const TASK = /^\/tasks\/([^/]+)$/;

export function category(ev) {
  const d = ev.data || {};
  const key = ev.key || '';
  switch (ev.kind) {
    case 'claim': return d.action === 'released' ? 'released' : 'claimed';
    case 'task_status': return 'task_status';
    case 'cost_alert': return 'cost_alert';
    case 'memory': {
      if (d.action === 'tombstone') return 'forgot';
      const s = SESSION.exec(key);
      if (s) return { 'tool-': 'tool', 'prompt-': 'prompt', start: 'session_start', stop: 'session_end' }[s[1]];
      if (TASK_STATUS.test(key)) return 'task_done';
      if (TASK.test(key)) return 'task_planned';
      return 'wrote';
    }
    default: return 'other';
  }
}

export function shortKey(key) {
  const parts = (key || '').split('/').filter(Boolean);
  if (parts.length <= 2) return '/' + parts.join('/');
  return '…/' + parts.slice(-2).join('/');
}

const lastSeg = (key) => (key || '').split('/').filter(Boolean).pop() || key;

export function describe2(ev, count = 1) {
  const d = ev.data || {};
  const c = category(ev);
  const who = d.writer || d.author || d.holder || (c === 'cost_alert' || c === 'task_status' ? 'server' : 'someone');
  const id = c === 'task_done' ? TASK_STATUS.exec(ev.key)[1] : lastSeg(ev.key);
  const table = {
    tool: count > 1 ? `ran ${count} tools` : 'ran a tool',
    prompt: count > 1 ? `asked ${count} prompts` : 'asked a prompt',
    session_start: 'session started',
    session_end: 'session ended',
    task_done: ({ blocked: `blocked on ${id}`, review: `sent ${id} to review`, in_progress: `started ${id}`, pending: `reset ${id}` })[d.state] || `finished ${id}`,
    task_planned: count > 1 ? `planned ${count} tasks` : `planned ${id}`,
    claimed: `claimed ${id}`,
    released: `released ${id}`,
    forgot: `forgot ${shortKey(ev.key)}`,
    wrote: count > 1 ? `wrote ${count} facts` : `wrote ${shortKey(ev.key)}`,
    task_status: `task ${ev.key} ${d.status || 'changed'}`,
    cost_alert: `cost alert level ${ev.key}`,
    other: `${ev.kind} ${shortKey(ev.key)}`,
  };
  const hot = c === 'task_done' ? !d.state || d.state === 'done' : ['task_planned', 'claimed', 'released', 'cost_alert'].includes(c);
  return { who, what: table[c], hot };
}

export function coalesce(rows, ev, nowMs, windowMs = 5000) {
  const c = category(ev);
  const first = describe2(ev);
  const ns = ev.namespace || (c === 'cost_alert' ? 'all' : '');
  const state = (ev.data && ev.data.state) || '';
  const top = rows[0];
  if (top && top.who === first.who && top.ns === ns && top.category === c && top.state === state && nowMs - top.ts <= windowMs) {
    top.count += 1;
    top.what = describe2(ev, top.count).what;
    top.ts = nowMs;
    return rows;
  }
  rows.unshift({ who: first.who, ns, category: c, state, what: first.what, count: 1, hot: first.hot, ts: nowMs, firstTs: nowMs });
  if (rows.length > 200) rows.length = 200;
  return rows;
}

export function lcg(seed) {
  let s = seed >>> 0;
  return () => { s = (s * 1664525 + 1013904223) >>> 0; return s / 4294967296; };
}

export function decodeMesh(buf) {
  const dv = new DataView(buf);
  const nv = dv.getUint32(0, true), nf = dv.getUint32(4, true);
  return { nv, nf, positions: new Float32Array(buf, 8, nv * 3), index: new Uint32Array(buf, 8 + nv * 12, nf * 3) };
}

export function triangleAreas(positions, index) {
  const nf = index.length / 3;
  const areas = new Float32Array(nf);
  const cdf = new Float32Array(nf);
  let total = 0;
  for (let i = 0; i < nf; i++) {
    const a = index[i * 3] * 3, b = index[i * 3 + 1] * 3, c = index[i * 3 + 2] * 3;
    const ux = positions[b] - positions[a], uy = positions[b + 1] - positions[a + 1], uz = positions[b + 2] - positions[a + 2];
    const vx = positions[c] - positions[a], vy = positions[c + 1] - positions[a + 1], vz = positions[c + 2] - positions[a + 2];
    const cx = uy * vz - uz * vy, cy = uz * vx - ux * vz, cz = ux * vy - uy * vx;
    areas[i] = Math.sqrt(cx * cx + cy * cy + cz * cz) / 2;
    total += areas[i];
  }
  let acc = 0;
  for (let i = 0; i < nf; i++) { acc += areas[i]; cdf[i] = acc / total; }
  return { areas, cdf };
}

export function sampleSurface(positions, index, cdf, count, rng, insets = [0.97, 0.90, 0.82]) {
  const out = new Float32Array(count * 3);
  const nf = cdf.length;
  for (let k = 0; k < count; k++) {
    const r = rng();
    let lo = 0, hi = nf - 1;
    while (lo < hi) { const mid = (lo + hi) >> 1; if (cdf[mid] < r) lo = mid + 1; else hi = mid; }
    const a = index[lo * 3] * 3, b = index[lo * 3 + 1] * 3, c = index[lo * 3 + 2] * 3;
    let u = rng(), v = rng();
    if (u + v > 1) { u = 1 - u; v = 1 - v; }
    const w = 1 - u - v, s = insets[k % insets.length];
    out[k * 3] = (positions[a] * w + positions[b] * u + positions[c] * v) * s;
    out[k * 3 + 1] = (positions[a + 1] * w + positions[b + 1] * u + positions[c + 1] * v) * s;
    out[k * 3 + 2] = (positions[a + 2] * w + positions[b + 2] * u + positions[c + 2] * v) * s;
  }
  return out;
}

export function nearestNeighbours(points, k = 2, maxDist2 = 0.02) {
  const n = points.length / 3;
  const out = [];
  for (let i = 0; i < n; i++) {
    const best = [];
    for (let j = i + 1; j < n; j++) {
      const dx = points[i * 3] - points[j * 3], dy = points[i * 3 + 1] - points[j * 3 + 1], dz = points[i * 3 + 2] - points[j * 3 + 2];
      const d = dx * dx + dy * dy + dz * dz;
      if (d < maxDist2) best.push([d, j]);
    }
    best.sort((p, q) => p[0] - q[0]);
    for (const [, j] of best.slice(0, k)) out.push(i, j);
  }
  return Uint32Array.from(out);
}

export function buildAdjacency(pairs) {
  const adj = new Map();
  for (let p = 0; p < pairs.length; p += 2) {
    const i = pairs[p], j = pairs[p + 1];
    if (!adj.has(i)) adj.set(i, []);
    if (!adj.has(j)) adj.set(j, []);
    adj.get(i).push(j);
    adj.get(j).push(i);
  }
  return adj;
}

export function topIndex(points) {
  let best = 0;
  for (let i = 1; i < points.length / 3; i++) if (points[i * 3 + 2] > points[best * 3 + 2]) best = i;
  return best;
}

export function farthestPoints(points, n, startIndex) {
  const count = points.length / 3;
  const chosen = [startIndex];
  const dist = new Float32Array(count).fill(Infinity);
  while (chosen.length < n && chosen.length < count) {
    const c = chosen[chosen.length - 1];
    let far = -1, farD = -1;
    for (let i = 0; i < count; i++) {
      const dx = points[i * 3] - points[c * 3], dy = points[i * 3 + 1] - points[c * 3 + 1], dz = points[i * 3 + 2] - points[c * 3 + 2];
      const d = dx * dx + dy * dy + dz * dz;
      if (d < dist[i]) dist[i] = d;
      if (dist[i] > farD) { farD = dist[i]; far = i; }
    }
    chosen.push(far);
  }
  return chosen;
}

export function slotAnchor(slot) { return (slot * 5) % 12; }

export function signalPath(adj, start, hops, rng) {
  const path = [start];
  let prev = -1, cur = start;
  for (let h = 0; h < hops; h++) {
    const next = (adj.get(cur) || []).filter((n) => n !== prev);
    if (!next.length) break;
    const n = next[Math.floor(rng() * next.length)];
    path.push(n);
    prev = cur; cur = n;
  }
  return path;
}
