// Pure logic for the brain view: activity maths, event weights, SSE
// framing, and the procedural brain sampler. No DOM, no WebGL, so this
// file runs under node --test.

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

export function assignSlots(names) {
  const m = new Map();
  names.forEach((n, i) => m.set(n, Math.min(i, MAX_REGIONS - 1)));
  return m;
}

export function describe(ev) {
  const d = ev.data || {};
  const where = ev.namespace ? ` in ${ev.namespace}` : '';
  switch (ev.kind) {
    case 'claim': return `${d.holder || 'someone'} ${d.action || 'claimed'} ${ev.key}${where}`;
    case 'memory': {
      const who = d.writer || d.author || 'someone';
      const verb = d.action === 'tombstone' ? 'forgot' : 'wrote';
      return `${who} ${verb} ${ev.key}${where}`;
    }
    case 'task_status': return `task ${ev.key} ${d.status || 'changed'}`;
    case 'defense': return `defense ${d.action || 'event'} on ${ev.key}${where}`;
    case 'cost_alert': return `cost alert level ${ev.key}`;
    default: return `${ev.kind} ${ev.key}${where}`;
  }
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

// Brain shape in a 2.4 x 2 x 2.8 box: two hemisphere shells, a fissure
// down the middle, a cerebellum shell at the back-bottom, a brainstem.
// Shells (not solids) so the lit points read as a cortex surface.
export function insideBrain(x, y, z) {
  const hx = Math.abs(x) - 0.42;
  const cer = (hx * hx) / (0.72 * 0.72) + (y * y) / (0.78 * 0.78) + (z * z) / (1.2 * 1.2);
  if (cer < 1 && cer > 0.86 && !(Math.abs(x) < 0.08 && y > -0.25)) return true;
  const cy = y + 0.55, cz = z + 0.75;
  const cb = (x * x) / (0.55 * 0.55) + (cy * cy) / (0.32 * 0.32) + (cz * cz) / (0.42 * 0.42);
  if (cb < 1 && cb > 0.72) return true;
  const sz = z + 0.35;
  if (y < -0.45 && y > -1.1 && x * x + sz * sz < 0.18 * 0.18) return true;
  return false;
}

export function sampleBrain(count, rng) {
  const out = new Float32Array(count * 3);
  let i = 0;
  while (i < count) {
    const x = (rng() * 2 - 1) * 1.2, y = (rng() * 2 - 1) * 1.1, z = (rng() * 2 - 1) * 1.4;
    if (!insideBrain(x, y, z)) continue;
    // Carve sulci: on the hemispheres, drop points where the fold value is
    // low so the gyri read as ridges instead of a uniform haze.
    if (y > -0.45 && foldValue(x, y, z) < 0.3 && rng() < 0.85) continue;
    out[i * 3] = x; out[i * 3 + 1] = y; out[i * 3 + 2] = z;
    i++;
  }
  return out;
}

// foldValue is a cheap gyri pattern: layered sines, remapped to [0,1].
export function foldValue(x, y, z) {
  const v = Math.sin(9 * x + 4 * Math.sin(3 * z) + 2 * y) * Math.sin(8 * y + 3 * Math.cos(2 * x + 5 * z));
  return 0.5 + 0.5 * v;
}

// regionSeeds spreads n points over the brain's surface with a golden
// angle spiral, scaled to the hemisphere ellipsoid so every region gets a
// contiguous patch of cortex.
export function regionSeeds(n) {
  const out = new Float32Array(n * 3);
  const golden = Math.PI * (3 - Math.sqrt(5));
  for (let i = 0; i < n; i++) {
    const y = 1 - (2 * (i + 0.5)) / n;
    const r = Math.sqrt(1 - y * y);
    const a = golden * i;
    out[i * 3] = Math.cos(a) * r * 1.14;
    out[i * 3 + 1] = y * 0.78;
    out[i * 3 + 2] = Math.sin(a) * r * 1.2;
  }
  return out;
}

// slotSeed spreads consecutive namespace slots across the surface: the
// golden spiral puts seeds 0..n next to each other at the top, so slot k
// takes seed (k * 37) mod 64 (37 is coprime with 64, so it is a bijection).
export function slotSeed(slot) {
  return (slot * 37) % MAX_REGIONS;
}

export function nearestSeed(x, y, z, seeds) {
  let best = 0, bd = Infinity;
  for (let i = 0; i < seeds.length / 3; i++) {
    const dx = x - seeds[i * 3], dy = y - seeds[i * 3 + 1], dz = z - seeds[i * 3 + 2];
    const d = dx * dx + dy * dy + dz * dz;
    if (d < bd) { bd = d; best = i; }
  }
  return best;
}
