import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  decay, glow, eventWeight, seedActivity, parseSSE,
  category, describe2, coalesce, shortKey, lcg, decodeMesh, triangleAreas, sampleSurface,
  nearestNeighbours, farthestPoints, topIndex, slotAnchor, signalPath, buildAdjacency,
} from './brain-core.js';

test('decay halves per half-life and never goes negative', () => {
  assert.ok(Math.abs(decay(2, 30, 30) - 1) < 1e-9);
  assert.ok(Math.abs(decay(2, 60, 30) - 0.5) < 1e-9);
  assert.equal(decay(0, 5, 30), 0);
});

test('glow maps activity into [0,1)', () => {
  assert.equal(glow(0), 0);
  assert.ok(glow(1) > 0.6 && glow(1) < 0.64);
  assert.ok(glow(50) < 1);
});

test('event weights follow the spec table', () => {
  const ev = (kind, key = '/x', data = {}) => ({ kind, namespace: 'ns', key, data });
  assert.equal(eventWeight(ev('memory', '/notes', { action: 'add', writer: 'me' })), 1.0);
  assert.equal(eventWeight(ev('memory', '/agent-sessions/s/tool-1', { action: 'add', writer: 'agent-hook' })), 0.35);
  assert.equal(eventWeight(ev('memory', '/notes', { action: 'tombstone' })), 0.5);
  assert.equal(eventWeight(ev('memory', '/tasks/T1/status', { action: 'add', writer: 'w' })), 2.0);
  assert.equal(eventWeight(ev('claim', '/tasks/T1', { action: 'claimed' })), 0.6);
  assert.equal(eventWeight(ev('defense')), 0.8);
  assert.equal(eventWeight(ev('task_status')), 1.0);
  assert.equal(eventWeight(ev('cost_alert')), 1.5);
  assert.equal(eventWeight(ev('something_new')), 0.5);
});

test('seedActivity caps at 3', () => {
  assert.equal(seedActivity(0), 0);
  assert.equal(seedActivity(5), 1);
  assert.equal(seedActivity(500), 3);
});

test('parseSSE splits complete frames and keeps the remainder', () => {
  const buf = 'event: hello\ndata: {"a":1}\n\n: ping\n\nevent: memory\ndata: {"b":2}\n\nevent: cl';
  const { frames, rest } = parseSSE(buf);
  assert.deepEqual(frames, [{ event: 'hello', data: '{"a":1}' }, { event: 'memory', data: '{"b":2}' }]);
  assert.equal(rest, 'event: cl');
});

const mem = (key, data = {}) => ({ kind: 'memory', namespace: 'ns', key, data: { action: 'add', writer: 'w1', ...data } });

test('category reads the key and kind', () => {
  assert.equal(category(mem('/agent-sessions/s1/tool-abc')), 'tool');
  assert.equal(category(mem('/agent-sessions/s1/prompt-1')), 'prompt');
  assert.equal(category(mem('/agent-sessions/s1/start')), 'session_start');
  assert.equal(category(mem('/agent-sessions/s1/stop')), 'session_end');
  assert.equal(category(mem('/tasks/T1/status')), 'task_done');
  assert.equal(category(mem('/tasks/T1')), 'task_planned');
  assert.equal(category({ kind: 'claim', namespace: 'ns', key: '/tasks/T1', data: { action: 'claimed', holder: 'w2' } }), 'claimed');
  assert.equal(category({ kind: 'claim', namespace: 'ns', key: '/tasks/T1', data: { action: 'released', holder: 'w2' } }), 'released');
  assert.equal(category(mem('/notes/a', { action: 'tombstone' })), 'forgot');
  assert.equal(category(mem('/decisions/schema')), 'wrote');
  assert.equal(category({ kind: 'task_status', namespace: '', key: 'task-9', data: { status: 'completed' } }), 'task_status');
  assert.equal(category({ kind: 'cost_alert', namespace: '', key: '2', data: {} }), 'cost_alert');
});

test('describe2 writes plain copy and marks task and claim rows hot', () => {
  assert.deepEqual(describe2(mem('/agent-sessions/s1/tool-abc')), { who: 'w1', what: 'ran a tool', hot: false });
  assert.deepEqual(describe2(mem('/agent-sessions/s1/tool-abc'), 14), { who: 'w1', what: 'ran 14 tools', hot: false });
  assert.deepEqual(describe2(mem('/tasks/S1A-7/status')), { who: 'w1', what: 'finished S1A-7', hot: true });
  assert.deepEqual(describe2({ kind: 'claim', namespace: 'ns', key: '/tasks/S1A-9', data: { action: 'claimed', holder: 'w2' } }), { who: 'w2', what: 'claimed S1A-9', hot: true });
  assert.deepEqual(describe2(mem('/a/b/c/d')), { who: 'w1', what: 'wrote …/c/d', hot: false });
  assert.deepEqual(describe2(mem('/x', { writer: '', author: '' })), { who: 'someone', what: 'wrote /x', hot: false });
  assert.deepEqual(describe2({ kind: 'cost_alert', namespace: '', key: '2', data: {} }), { who: 'server', what: 'cost alert level 2', hot: true });
});

test('shortKey keeps the last two segments', () => {
  assert.equal(shortKey('/a'), '/a');
  assert.equal(shortKey('/a/b'), '/a/b');
  assert.equal(shortKey('/a/b/c'), '…/b/c');
});

test('coalesce merges same who+ns+category within the window and caps at 200', () => {
  let rows = [];
  rows = coalesce(rows, mem('/agent-sessions/s1/tool-1'), 1000);
  rows = coalesce(rows, mem('/agent-sessions/s1/tool-2'), 2000);
  rows = coalesce(rows, mem('/agent-sessions/s1/tool-3'), 3000);
  assert.equal(rows.length, 1);
  assert.equal(rows[0].count, 3);
  assert.equal(rows[0].what, 'ran 3 tools');
  rows = coalesce(rows, mem('/agent-sessions/s1/prompt-1'), 3500);
  assert.equal(rows.length, 2);
  rows = coalesce(rows, mem('/agent-sessions/s1/tool-4'), 20000);
  assert.equal(rows.length, 3);
  for (let i = 0; i < 300; i++) rows = coalesce(rows, mem('/k' + i), 30000 + i * 10000);
  assert.equal(rows.length, 200);
});

function cubeMesh() {
  // two triangles forming a unit square in z=1 plus a tiny triangle far away, as a binary buffer
  const verts = [0, 0, 1, 1, 0, 1, 1, 1, 1, 0, 1, 1, 0, 0, -1, 0.01, 0, -1, 0, 0.01, -1];
  const faces = [0, 1, 2, 0, 2, 3, 4, 5, 6];
  const buf = new ArrayBuffer(8 + verts.length * 4 + faces.length * 4);
  const dv = new DataView(buf);
  dv.setUint32(0, verts.length / 3, true); dv.setUint32(4, faces.length / 3, true);
  new Float32Array(buf, 8, verts.length).set(verts);
  new Uint32Array(buf, 8 + verts.length * 4, faces.length).set(faces);
  return buf;
}

test('decodeMesh and triangleAreas read the binary layout', () => {
  const m = decodeMesh(cubeMesh());
  assert.equal(m.nv, 7); assert.equal(m.nf, 3);
  assert.equal(m.positions[3], 1);
  const { areas, cdf } = triangleAreas(m.positions, m.index);
  assert.ok(Math.abs(areas[0] - 0.5) < 1e-6 && Math.abs(areas[1] - 0.5) < 1e-6);
  assert.ok(areas[2] < 1e-3);
  assert.ok(Math.abs(cdf[2] - 1) < 1e-6);
});

test('sampleSurface is area weighted, inset and deterministic', () => {
  const m = decodeMesh(cubeMesh());
  const { cdf } = triangleAreas(m.positions, m.index);
  const a = sampleSurface(m.positions, m.index, cdf, 300, lcg(3));
  const b = sampleSurface(m.positions, m.index, cdf, 300, lcg(3));
  assert.deepEqual(Array.from(a), Array.from(b));
  assert.equal(a.length, 900);
  let onSquare = 0;
  for (let k = 0; k < 300; k++) { if (a[k * 3 + 2] > 0.5) onSquare++; }
  assert.ok(onSquare > 290, `only ${onSquare} of 300 samples landed on the big triangles`);
  assert.ok(Math.abs(a[2] - 0.97) < 1e-6 || Math.abs(a[2] - 0.90) < 1e-6 || Math.abs(a[2] - 0.82) < 1e-6);
});

test('nearestNeighbours, adjacency, farthestPoints, topIndex, slotAnchor, signalPath', () => {
  const pts = new Float32Array([0, 0, 0, 0.05, 0, 0, 0.1, 0, 0, 5, 5, 0.5, 0, 0, 1]);
  const pairs = nearestNeighbours(pts, 2, 0.02);
  const list = Array.from(pairs);
  assert.ok(list.length % 2 === 0);
  for (let i = 0; i < list.length; i += 2) assert.ok(list[i] < list[i + 1]);
  assert.ok(list.includes(3) === false, 'the far point has no neighbours');
  const adj = buildAdjacency(pairs);
  assert.ok(adj.get(0).includes(1));
  assert.equal(topIndex(pts), 4);
  const far = farthestPoints(pts, 3, 4);
  assert.equal(far[0], 4);
  assert.equal(far[1], 3);
  assert.equal(slotAnchor(0), 0); assert.equal(slotAnchor(1), 5); assert.equal(slotAnchor(3), 3); assert.equal(slotAnchor(12), 0);
  const path = signalPath(adj, 0, 3, lcg(1));
  assert.equal(path[0], 0);
  assert.ok(path.length >= 2 && path.length <= 4);
  for (let i = 1; i < path.length; i++) assert.ok(adj.get(path[i - 1]).includes(path[i]));
});

test('describe2 words a status write by its state', () => {
  const ev = (state) => ({ kind: 'memory', key: '/tasks/T1/status', data: { writer: 'w1', action: 'add', state } });
  assert.equal(describe2(ev('done')).what, 'finished T1');
  assert.equal(describe2(ev('blocked')).what, 'blocked on T1');
  assert.equal(describe2(ev('review')).what, 'sent T1 to review');
  assert.equal(describe2(ev('in_progress')).what, 'started T1');
  assert.equal(describe2(ev(undefined)).what, 'finished T1');
  assert.equal(describe2(ev('in_progress')).hot, false);
  assert.equal(describe2(ev('done')).hot, true);
});

test('coalesce folds status writes only when the state matches', () => {
  const ev = (state) => ({ kind: 'memory', namespace: 'ns', key: '/tasks/T1/status', data: { writer: 'w1', action: 'add', state } });
  let rows = [];
  rows = coalesce(rows, ev('in_progress'), 1000);
  rows = coalesce(rows, ev('done'), 2000);
  assert.equal(rows.length, 2);
  assert.equal(rows[0].what, 'finished T1');
  assert.equal(rows[1].what, 'started T1');
  rows = [];
  rows = coalesce(rows, ev('in_progress'), 1000);
  rows = coalesce(rows, ev('in_progress'), 2000);
  assert.equal(rows.length, 1);
  assert.equal(rows[0].count, 2);
});
