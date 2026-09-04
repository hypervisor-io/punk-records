import * as THREE from './vendor/three.module.min.js';
import {
  MAX_REGIONS, decay, glow, sampleBrain, foldValue, regionSeeds, nearestSeed, mulberry32,
} from './brain-core.js';

const POINTS = 30000;
const reduced = window.matchMedia('(prefers-reduced-motion: reduce)').matches;

// ---- renderer, scene, camera ------------------------------------------
const canvas = document.getElementById('scene');
const renderer = new THREE.WebGLRenderer({ canvas, antialias: false, alpha: false });
renderer.setPixelRatio(Math.min(window.devicePixelRatio, 2));
renderer.setClearColor(0x05060f, 1);
const scene = new THREE.Scene();
const camera = new THREE.PerspectiveCamera(42, 1, 0.1, 50);
camera.position.set(0, 0.2, 3.6);
const rig = new THREE.Group();
scene.add(rig);

function resize() {
  const w = window.innerWidth, h = window.innerHeight;
  renderer.setSize(w, h, false);
  camera.aspect = w / h;
  camera.updateProjectionMatrix();
}
window.addEventListener('resize', resize);
resize();

// ---- point cloud --------------------------------------------------------
const positions = sampleBrain(POINTS, mulberry32(20260904));
const seeds = regionSeeds(MAX_REGIONS);
const region = new Float32Array(POINTS);
const fold = new Float32Array(POINTS);
for (let i = 0; i < POINTS; i++) {
  const x = positions[i * 3], y = positions[i * 3 + 1], z = positions[i * 3 + 2];
  region[i] = nearestSeed(x, y, z, seeds);
  fold[i] = foldValue(x, y, z);
}
const geo = new THREE.BufferGeometry();
geo.setAttribute('position', new THREE.BufferAttribute(positions, 3));
geo.setAttribute('region', new THREE.BufferAttribute(region, 1));
geo.setAttribute('fold', new THREE.BufferAttribute(fold, 1));

const activity = new Float32Array(MAX_REGIONS); // raw scores, decayed each frame
const uniforms = {
  uGlow: { value: new Float32Array(MAX_REGIONS) },
  uFocus: { value: -1 },
  uTime: { value: 0 },
  uPixelRatio: { value: renderer.getPixelRatio() },
  uPulse: { value: reduced ? 0 : 1 },
  uCold: { value: new THREE.Color(0x3fe0ff) },
  uWarm: { value: new THREE.Color(0xffb64d) },
  uHot: { value: new THREE.Color(0xff4fd8) },
};

const vertex = `
attribute float region;
attribute float fold;
uniform float uGlow[${MAX_REGIONS}];
uniform float uFocus;
uniform float uTime;
uniform float uPixelRatio;
uniform float uPulse;
varying float vGlow;
varying float vFold;
varying float vDim;
void main() {
  int r = int(region + 0.5);
  vGlow = uGlow[r];
  vFold = fold;
  vDim = (uFocus >= 0.0 && abs(uFocus - region) > 0.5) ? 0.25 : 1.0;
  vec4 mv = modelViewMatrix * vec4(position, 1.0);
  float pulse = 1.0 + uPulse * 0.18 * vGlow * sin(uTime * 6.0 + position.y * 9.0 + position.x * 5.0);
  gl_PointSize = (2.0 + 4.5 * vGlow) * pulse * uPixelRatio * (150.0 / -mv.z);
  gl_Position = projectionMatrix * mv;
}`;

const fragment = `
precision highp float;
uniform vec3 uCold;
uniform vec3 uWarm;
uniform vec3 uHot;
varying float vGlow;
varying float vFold;
varying float vDim;
void main() {
  vec2 c = gl_PointCoord - 0.5;
  float d = dot(c, c);
  if (d > 0.25) discard;
  float soft = smoothstep(0.25, 0.0, d);
  vec3 col = mix(uCold, uWarm, smoothstep(0.0, 0.6, vGlow));
  col = mix(col, uHot, smoothstep(0.6, 1.0, vGlow));
  float a = soft * (0.16 + 0.22 * vFold + 0.85 * vGlow) * vDim;
  gl_FragColor = vec4(col * (0.55 + 1.6 * vGlow), a);
}`;

const material = new THREE.ShaderMaterial({
  uniforms, vertexShader: vertex, fragmentShader: fragment,
  transparent: true, depthWrite: false, blending: THREE.AdditiveBlending,
});
const cloud = new THREE.Points(geo, material);
rig.add(cloud);

// ---- sparks: one bright expanding point per event -------------------------
const MAX_SPARKS = 96;
const SPARK_LIFE = 0.9; // seconds
const sparkPos = new Float32Array(MAX_SPARKS * 3);
const sparkAge = new Float32Array(MAX_SPARKS).fill(99);
const sparkGeo = new THREE.BufferGeometry();
sparkGeo.setAttribute('position', new THREE.BufferAttribute(sparkPos, 3));
sparkGeo.setAttribute('age', new THREE.BufferAttribute(sparkAge, 1));
const sparkMat = new THREE.ShaderMaterial({
  uniforms: { uPixelRatio: uniforms.uPixelRatio, uLife: { value: SPARK_LIFE }, uHot: uniforms.uHot, uCold: uniforms.uCold },
  vertexShader: `
attribute float age;
uniform float uPixelRatio;
uniform float uLife;
varying float vT;
void main() {
  vT = clamp(age / uLife, 0.0, 1.0);
  vec4 mv = modelViewMatrix * vec4(position, 1.0);
  float size = mix(6.0, 70.0, vT) * uPixelRatio * (150.0 / -mv.z);
  gl_PointSize = (vT >= 1.0) ? 0.0 : size;
  gl_Position = projectionMatrix * mv;
}`,
  fragmentShader: `
precision highp float;
uniform vec3 uHot;
uniform vec3 uCold;
varying float vT;
void main() {
  vec2 c = gl_PointCoord - 0.5;
  float r = length(c) * 2.0;
  float ring = smoothstep(0.55, 0.85, r) * (1.0 - smoothstep(0.85, 1.0, r));
  float core = 1.0 - smoothstep(0.0, 0.35, r);
  float a = (ring * 0.9 + core) * (1.0 - vT);
  gl_FragColor = vec4(mix(uHot, uCold, vT), a);
}`,
  transparent: true, depthWrite: false, blending: THREE.AdditiveBlending,
});
const sparks = new THREE.Points(sparkGeo, sparkMat);
rig.add(sparks);
let nextSpark = 0;

export function spark(slot) {
  const i = nextSpark++ % MAX_SPARKS;
  sparkPos[i * 3] = seeds[slot * 3];
  sparkPos[i * 3 + 1] = seeds[slot * 3 + 1];
  sparkPos[i * 3 + 2] = seeds[slot * 3 + 2];
  sparkAge[i] = 0;
  sparkGeo.attributes.position.needsUpdate = true;
  sparkGeo.attributes.age.needsUpdate = true;
}

// pulse adds weight to a region's activity and fires a spark there.
export function pulse(slot, weight) {
  if (slot < 0 || slot >= MAX_REGIONS) return;
  activity[slot] += weight;
  spark(slot);
}

export function setActivity(slot, value) { activity[slot] = value; }
export function getGlow(slot) { return uniforms.uGlow.value[slot]; }
export function setFocus(slot) { uniforms.uFocus.value = slot; }

// ---- camera interaction ---------------------------------------------------
let dragging = false, lastX = 0, lastY = 0, idleSince = performance.now();
let yaw = 0.4, pitch = 0.15, dist = 3.6;
canvas.addEventListener('pointerdown', (e) => { dragging = true; lastX = e.clientX; lastY = e.clientY; canvas.setPointerCapture(e.pointerId); });
canvas.addEventListener('pointerup', () => { dragging = false; idleSince = performance.now(); });
canvas.addEventListener('pointermove', (e) => {
  if (!dragging) return;
  yaw += (e.clientX - lastX) * 0.006;
  pitch = Math.max(-1.2, Math.min(1.2, pitch + (e.clientY - lastY) * 0.006));
  lastX = e.clientX; lastY = e.clientY; idleSince = performance.now();
});
canvas.addEventListener('wheel', (e) => {
  dist = Math.max(2.4, Math.min(6, dist + e.deltaY * 0.002));
  idleSince = performance.now();
}, { passive: true });

// ---- frame loop -------------------------------------------------------
let last = performance.now();
export const hooks = { onFrame: null }; // B7 sets onFrame(dt) for HUD updates
function frame(now) {
  const dt = Math.min(0.1, (now - last) / 1000);
  last = now;
  uniforms.uTime.value = now / 1000;
  for (let i = 0; i < MAX_REGIONS; i++) {
    activity[i] = decay(activity[i], dt);
    uniforms.uGlow.value[i] = glow(activity[i]);
  }
  for (let i = 0; i < MAX_SPARKS; i++) if (sparkAge[i] < 99) sparkAge[i] += dt;
  sparkGeo.attributes.age.needsUpdate = true;
  if (!reduced && !dragging && now - idleSince > 3000) yaw += dt * 0.15;
  rig.rotation.set(pitch, yaw, 0);
  camera.position.set(0, 0.2, dist);
  camera.lookAt(0, 0, 0);
  if (hooks.onFrame) hooks.onFrame(dt);
  renderer.render(scene, camera);
  requestAnimationFrame(frame);
}
requestAnimationFrame(frame);

// Debug: space pulses a random region until B7 wires real data.
window.addEventListener('keydown', (e) => {
  if (e.code === 'Space') pulse(Math.floor(Math.random() * 12), 1.5);
});
