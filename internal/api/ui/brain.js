import * as THREE from './vendor/three.module.min.js';
import {
  MAX_REGIONS, decay, glow, lcg, decodeMesh, triangleAreas, sampleSurface, nearestNeighbours,
  buildAdjacency, topIndex, farthestPoints, slotAnchor, signalPath,
} from './brain-core.js';

const reduced = window.matchMedia('(prefers-reduced-motion: reduce)').matches;
const canvas = document.getElementById('scene');
const renderer = new THREE.WebGLRenderer({ canvas, antialias: true, alpha: false });
renderer.setPixelRatio(Math.min(window.devicePixelRatio, 2));
renderer.setClearColor(0x050507, 1);
renderer.toneMapping = THREE.NoToneMapping;
const scene = new THREE.Scene();
const camera = new THREE.PerspectiveCamera(38, 1, 0.1, 50);
const rig = new THREE.Group();
rig.rotation.x = -Math.PI / 2; // mesh is z-up
scene.add(rig);
function resize() { renderer.setSize(window.innerWidth, window.innerHeight, false); camera.aspect = window.innerWidth / window.innerHeight; camera.updateProjectionMatrix(); }
window.addEventListener('resize', resize); resize();

export const activity = new Float32Array(MAX_REGIONS);
export const hooks = { onFrame: null };
let focus = -1;
export function setFocus(slot) { focus = slot; }

const fresnel = (side, rim) => new THREE.ShaderMaterial({
  uniforms: { uColor: { value: new THREE.Color(0x4fd8ff) }, uRim: { value: new THREE.Color(rim) } },
  vertexShader: `varying vec3 vN; varying vec3 vV; void main(){ vec4 mv = modelViewMatrix*vec4(position,1.0); vN = normalize(normalMatrix*normal); vV = normalize(-mv.xyz); gl_Position = projectionMatrix*mv; }`,
  fragmentShader: `uniform vec3 uColor; uniform vec3 uRim; varying vec3 vN; varying vec3 vV;
    void main(){ float f = pow(1.0 - max(dot(normalize(vN), normalize(vV)), 0.0), 2.6); vec3 c = uColor*0.05 + uRim*f*1.1; gl_FragColor = vec4(c, 0.06 + f*0.9); }`,
  transparent: true, depthWrite: false, blending: THREE.AdditiveBlending, side,
});

let anchors = [];            // neuron indices (12)
let anchorPos = [];          // THREE.Vector3 in rig space
let neuronPos, adjacency, hazeSprites = [], fireLayers = [], nodeEls;
const signals = [];          // { path: number[], t: 0..1, opacity }
let signalPoints;

export const ready = (async () => {
  const buf = await (await fetch('/brain/mesh/brain.bin')).arrayBuffer();
  const mesh = decodeMesh(buf);
  const geo = new THREE.BufferGeometry();
  geo.setAttribute('position', new THREE.BufferAttribute(mesh.positions, 3));
  geo.setIndex(new THREE.BufferAttribute(mesh.index, 1));
  geo.computeVertexNormals();

  const body = new THREE.Mesh(geo, new THREE.MeshBasicMaterial({ color: 0x07101c, transparent: true, opacity: 0.45, depthWrite: true }));
  const back = new THREE.Mesh(geo, fresnel(THREE.BackSide, 0x3a8fb0));
  const front = new THREE.Mesh(geo, fresnel(THREE.FrontSide, 0xbfefff));
  body.renderOrder = 0; back.renderOrder = 1; front.renderOrder = 2;
  const edges = new THREE.LineSegments(new THREE.EdgesGeometry(geo, 38), new THREE.LineBasicMaterial({ color: 0x4fd8ff, transparent: true, opacity: 0.28, blending: THREE.AdditiveBlending, depthWrite: false }));
  edges.renderOrder = 3;
  rig.add(body, back, front, edges);

  const { cdf } = triangleAreas(mesh.positions, mesh.index);
  neuronPos = sampleSurface(mesh.positions, mesh.index, cdf, 1800, lcg(7));
  const pairs = nearestNeighbours(neuronPos, 2, 0.02);
  adjacency = buildAdjacency(pairs);
  const axonPos = new Float32Array(pairs.length * 3);
  for (let p = 0; p < pairs.length; p++) axonPos.set(neuronPos.subarray(pairs[p] * 3, pairs[p] * 3 + 3), p * 3);
  const axons = new THREE.LineSegments(new THREE.BufferGeometry().setAttribute('position', new THREE.BufferAttribute(axonPos, 3)),
    new THREE.LineBasicMaterial({ color: 0x4fd8ff, transparent: true, opacity: 0.10, blending: THREE.AdditiveBlending, depthWrite: false, depthTest: false }));
  axons.renderOrder = 3;
  const neurons = new THREE.Points(new THREE.BufferGeometry().setAttribute('position', new THREE.BufferAttribute(neuronPos, 3)),
    new THREE.PointsMaterial({ color: 0xbfefff, size: 0.02, transparent: true, opacity: 0.55, blending: THREE.AdditiveBlending, depthWrite: false, depthTest: false }));
  neurons.renderOrder = 4;
  rig.add(axons, neurons);

  anchors = farthestPoints(neuronPos, 12, topIndex(neuronPos));
  anchorPos = anchors.map((i) => new THREE.Vector3(neuronPos[i * 3], neuronPos[i * 3 + 1], neuronPos[i * 3 + 2]));
  const tex = hazeTexture();
  anchors.forEach((ai, a) => {
    const near = [];
    for (let k = 0; k < 1800; k++) {
      const dx = neuronPos[k * 3] - anchorPos[a].x, dy = neuronPos[k * 3 + 1] - anchorPos[a].y, dz = neuronPos[k * 3 + 2] - anchorPos[a].z;
      if (dx * dx + dy * dy + dz * dz < 0.09) near.push(neuronPos[k * 3], neuronPos[k * 3 + 1], neuronPos[k * 3 + 2]);
    }
    const fire = new THREE.Points(new THREE.BufferGeometry().setAttribute('position', new THREE.BufferAttribute(new Float32Array(near), 3)),
      new THREE.PointsMaterial({ color: 0xff4fd8, size: 0.03, transparent: true, opacity: 0, blending: THREE.AdditiveBlending, depthWrite: false, depthTest: false }));
    fire.renderOrder = 5;
    const haze = new THREE.Sprite(new THREE.SpriteMaterial({ map: tex, transparent: true, opacity: 0, blending: THREE.AdditiveBlending, depthWrite: false, depthTest: false }));
    haze.position.copy(anchorPos[a]); haze.scale.set(0.9, 0.9, 1); haze.renderOrder = 6;
    fire.userData.indices = near.length / 3;
    fireLayers.push(fire); hazeSprites.push(haze);
    rig.add(fire, haze);
  });

  const sp = new Float32Array(96 * 3);
  signalPoints = new THREE.Points(new THREE.BufferGeometry().setAttribute('position', new THREE.BufferAttribute(sp, 3)),
    new THREE.PointsMaterial({ color: 0xffffff, size: 0.035, transparent: true, opacity: 0.9, blending: THREE.AdditiveBlending, depthWrite: false, depthTest: false }));
  signalPoints.renderOrder = 7;
  rig.add(signalPoints);
})();

function hazeTexture() {
  const c = document.createElement('canvas'); c.width = c.height = 128;
  const g = c.getContext('2d'); const grad = g.createRadialGradient(64, 64, 0, 64, 64, 64);
  grad.addColorStop(0, 'rgba(255,79,216,0.9)'); grad.addColorStop(0.4, 'rgba(255,79,216,0.25)'); grad.addColorStop(1, 'rgba(255,79,216,0)');
  g.fillStyle = grad; g.fillRect(0, 0, 128, 128);
  return new THREE.CanvasTexture(c);
}

const signalRng = lcg(99);
function spawnSignal(anchorIndex, opacity) {
  if (!adjacency) return;
  const start = anchors[anchorIndex];
  const path = signalPath(adjacency, start, 3, signalRng);
  if (path.length < 2) return;
  signals.push({ path, t: 0, opacity });
  if (signals.length > 96) signals.shift();
}

export function pulse(slot, weight) {
  if (slot < 0 || slot >= MAX_REGIONS) return;
  activity[slot] += weight;
  const a = slotAnchor(slot);
  for (let i = 0; i < 6; i++) spawnSignal(a, 0.9);
}

export function anchorScreen(a) {
  if (!anchorPos[a]) return { x: 0, y: 0, visible: false };
  const v = anchorPos[a].clone().applyMatrix4(rig.matrixWorld).project(camera);
  return { x: (v.x + 1) / 2 * window.innerWidth, y: (1 - v.y) / 2 * window.innerHeight, visible: v.z < 1 };
}

// camera and input
let yaw = Math.PI / 2, pitch = 0.17, dist = 3.6, dragging = false, lastX = 0, lastY = 0, idleSince = performance.now();
canvas.addEventListener('pointerdown', (e) => { dragging = true; lastX = e.clientX; lastY = e.clientY; canvas.setPointerCapture(e.pointerId); });
canvas.addEventListener('pointerup', () => { dragging = false; idleSince = performance.now(); });
canvas.addEventListener('pointermove', (e) => { if (!dragging) return; yaw += (e.clientX - lastX) * 0.006; pitch = Math.max(-0.9, Math.min(0.9, pitch + (e.clientY - lastY) * 0.006)); lastX = e.clientX; lastY = e.clientY; idleSince = performance.now(); });
canvas.addEventListener('wheel', (e) => { dist = Math.max(2.6, Math.min(5, dist + e.deltaY * 0.002)); idleSince = performance.now(); }, { passive: true });

let last = performance.now(), ambientAcc = 0;
function frame(now) {
  const dt = Math.min(0.1, (now - last) / 1000); last = now;
  const anchorGlow = new Float32Array(12);
  for (let s = 0; s < MAX_REGIONS; s++) { activity[s] = decay(activity[s], dt); anchorGlow[slotAnchor(s)] = Math.max(anchorGlow[slotAnchor(s)], glow(activity[s])); }
  const focusAnchor = focus >= 0 ? slotAnchor(focus) : -1;
  for (let a = 0; a < hazeSprites.length; a++) {
    const dim = focusAnchor >= 0 && focusAnchor !== a ? 0.35 : 1;
    hazeSprites[a].material.opacity = anchorGlow[a] * dim;
    fireLayers[a].material.opacity = anchorGlow[a] * dim;
  }
  if (!reduced && adjacency) { ambientAcc += dt; if (ambientAcc > 1) { ambientAcc = 0; spawnSignal(Math.floor(signalRng() * 12), 0.35); } }
  if (signalPoints) {
    const pos = signalPoints.geometry.attributes.position.array;
    pos.fill(0);
    for (let i = signals.length - 1; i >= 0; i--) {
      const s = signals[i]; s.t += dt / 0.9;
      if (s.t >= 1) { signals.splice(i, 1); continue; }
      const seg = Math.min(s.path.length - 2, Math.floor(s.t * (s.path.length - 1)));
      const f = s.t * (s.path.length - 1) - seg;
      const a = s.path[seg] * 3, b = s.path[seg + 1] * 3;
      pos[i * 3] = neuronPos[a] + (neuronPos[b] - neuronPos[a]) * f;
      pos[i * 3 + 1] = neuronPos[a + 1] + (neuronPos[b + 1] - neuronPos[a + 1]) * f;
      pos[i * 3 + 2] = neuronPos[a + 2] + (neuronPos[b + 2] - neuronPos[a + 2]) * f;
    }
    signalPoints.geometry.setDrawRange(0, signals.length);
    signalPoints.geometry.attributes.position.needsUpdate = true;
  }
  if (!reduced && !dragging && now - idleSince > 4000) yaw += dt * 0.08;
  camera.position.set(Math.sin(yaw) * Math.cos(pitch) * dist, Math.sin(pitch) * dist + 0.2, Math.cos(yaw) * Math.cos(pitch) * dist);
  camera.lookAt(0, 0, 0);
  rig.updateMatrixWorld();
  if (hooks.onFrame) hooks.onFrame(dt);
  renderer.render(scene, camera);
  requestAnimationFrame(frame);
}
requestAnimationFrame(frame);
