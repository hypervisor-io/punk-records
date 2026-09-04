import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  decay, glow, eventWeight, seedActivity, parseSSE, assignSlots, describe,
  insideBrain, sampleBrain, foldValue, regionSeeds, nearestSeed, slotSeed, mulberry32, MAX_REGIONS,
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

test('assignSlots keeps order and overflows into the last slot', () => {
  const names = Array.from({ length: MAX_REGIONS + 3 }, (_, i) => 'ns' + i);
  const m = assignSlots(names);
  assert.equal(m.get('ns0'), 0);
  assert.equal(m.get('ns' + (MAX_REGIONS - 1)), MAX_REGIONS - 1);
  assert.equal(m.get('ns' + (MAX_REGIONS + 2)), MAX_REGIONS - 1);
});

test('describe writes a readable log line', () => {
  assert.equal(describe({ kind: 'claim', namespace: 'agent-x', key: '/tasks/T1', data: { holder: 'w2', action: 'claimed' } }),
    'w2 claimed /tasks/T1 in agent-x');
  assert.equal(describe({ kind: 'memory', namespace: 'agent-x', key: '/tasks/T1/status', data: { writer: 'w2', action: 'add' } }),
    'w2 wrote /tasks/T1/status in agent-x');
  assert.equal(describe({ kind: 'task_status', namespace: '', key: 'task-9', data: { status: 'completed' } }),
    'task task-9 completed');
});

test('brain sampler stays inside the shape and is deterministic', () => {
  const a = sampleBrain(300, mulberry32(7));
  const b = sampleBrain(300, mulberry32(7));
  assert.deepEqual(Array.from(a), Array.from(b));
  assert.equal(a.length, 900);
  for (let i = 0; i < a.length; i += 3) {
    assert.ok(insideBrain(a[i], a[i + 1], a[i + 2]), `point ${i / 3} outside`);
    const f = foldValue(a[i], a[i + 1], a[i + 2]);
    assert.ok(f >= 0 && f <= 1);
  }
  assert.ok(!insideBrain(0, 0, 0), 'centre of a hemisphere shell is hollow');
  assert.ok(!insideBrain(3, 3, 3));
});

test('region seeds sit on the surface and nearestSeed picks the closest', () => {
  const seeds = regionSeeds(12);
  assert.equal(seeds.length, 36);
  for (let i = 0; i < 12; i++) {
    const r = Math.hypot(seeds[i * 3], seeds[i * 3 + 1], seeds[i * 3 + 2]);
    assert.ok(r > 0.5 && r < 1.8, `seed ${i} radius ${r}`);
  }
  const k = nearestSeed(seeds[9], seeds[10], seeds[11], seeds);
  assert.equal(k, 3);
});

test('slotSeed is a bijection on the slot range that separates neighbours', () => {
  const seen = new Set();
  for (let i = 0; i < MAX_REGIONS; i++) seen.add(slotSeed(i));
  assert.equal(seen.size, MAX_REGIONS);
  assert.equal(slotSeed(0), 0);
  assert.ok(Math.abs(slotSeed(1) - slotSeed(0)) > 8);
});
