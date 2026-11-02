/* AgentDeck intro film — every frame is a pure function of t.
 *
 * There is no requestAnimationFrame and no CSS transition here: render.mjs
 * calls window.__seek(t) with a fixed step and screenshots the canvas, so the
 * same code always produces the same film. Anything time-dependent must be
 * derived from t (or from the seeded PRNG below), never from wall clock.
 *
 * Layout: 1920x1080 @ 30fps. Scene list at the bottom of this file.
 */
'use strict';

const W = 1920;
const H = 1080;

const SANS = '"Noto Sans CJK SC", "Source Han Sans SC", sans-serif';
const MONO = '"Noto Sans Mono CJK SC", "DejaVu Sans Mono", monospace';

// AgentDeck's own dark theme, so the film and the product look like one thing.
const C = {
  bg0: '#080b10',
  bg1: '#0d1117',
  surface: '#161b22',
  surface2: '#1c2128',
  border: '#30363d',
  fg: '#e6edf3',
  muted: '#8b949e',
  dim: '#6e7681',
  accent: '#58a6ff',
  green: '#3fb950',
  amber: '#d29922',
  purple: '#bc8cff',
  cyan: '#39d3e0',
  red: '#f85149',
  pink: '#f778ba',
};

const ctx = (() => {
  // The backing store follows devicePixelRatio so render.mjs can supersample:
  // at DSF=2 the canvas is 3840x2160 and ffmpeg scales it down to 1080p with
  // lanczos, which is what keeps the Chinese type crisp.
  const DPR = Math.max(1, window.devicePixelRatio || 1);
  const cv = document.getElementById('stage');
  cv.width = W * DPR;
  cv.height = H * DPR;
  cv.style.width = W + 'px';
  cv.style.height = H + 'px';
  const c = cv.getContext('2d');
  c.scale(DPR, DPR);
  return c;
})();

// ---------------------------------------------------------------- primitives

const clamp01 = (x) => (x < 0 ? 0 : x > 1 ? 1 : x);
const lerpBox = (A, B, k) => ({
  x: lerp(A.x, B.x, k), y: lerp(A.y, B.y, k),
  w: lerp(A.w, B.w, k), h: lerp(A.h, B.h, k),
});
const lerp = (a, b, t) => a + (b - a) * t;
const easeOut = (t) => 1 - Math.pow(1 - clamp01(t), 3);
const easeIn = (t) => Math.pow(clamp01(t), 3);
const easeInOut = (t) => (t < 0.5 ? 4 * t * t * t : 1 - Math.pow(-2 * t + 2, 3) / 2);
const easeOutBack = (t) => 1 + 2.2 * Math.pow(t - 1, 3) + 1.2 * Math.pow(t - 1, 2);

/** deterministic PRNG so particle fields never shimmer between runs */
function mulberry(seed) {
  let a = seed >>> 0;
  return function () {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

function roundRectPath(x, y, w, h, r) {
  const rr = Math.min(r, w / 2, h / 2);
  ctx.beginPath();
  ctx.moveTo(x + rr, y);
  ctx.arcTo(x + w, y, x + w, y + h, rr);
  ctx.arcTo(x + w, y + h, x, y + h, rr);
  ctx.arcTo(x, y + h, x, y, rr);
  ctx.arcTo(x, y, x + w, y, rr);
  ctx.closePath();
}

function fillRound(x, y, w, h, r, fill) {
  roundRectPath(x, y, w, h, r);
  ctx.fillStyle = fill;
  ctx.fill();
}

function strokeRound(x, y, w, h, r, stroke, lw = 1) {
  roundRectPath(x, y, w, h, r);
  ctx.strokeStyle = stroke;
  ctx.lineWidth = lw;
  ctx.stroke();
}

/** Panel with a soft drop shadow — the visual language for every card. */
function card(x, y, w, h, r, { fill = C.surface, stroke = C.border, shadow = 34, lw = 1 } = {}) {
  ctx.save();
  ctx.shadowColor = 'rgba(0,0,0,.55)';
  ctx.shadowBlur = shadow;
  ctx.shadowOffsetY = shadow * 0.35;
  fillRound(x, y, w, h, r, fill);
  ctx.restore();
  if (stroke) strokeRound(x, y, w, h, r, stroke, lw);
}

function text(str, x, y, o = {}) {
  const {
    size = 28, weight = 500, color = C.fg, align = 'left',
    baseline = 'alphabetic', mono = false, alpha = 1, spacing = '0px', maxW,
  } = o;
  if (alpha <= 0.001) return 0;
  ctx.save();
  ctx.globalAlpha *= alpha;
  ctx.font = `${weight} ${size}px ${mono ? MONO : SANS}`;
  if ('letterSpacing' in ctx) ctx.letterSpacing = spacing;
  ctx.fillStyle = color;
  ctx.textAlign = align;
  ctx.textBaseline = baseline;
  let s = str;
  if (maxW) s = ellipsize(str, maxW);
  ctx.fillText(s, x, y);
  const w = ctx.measureText(s).width;
  ctx.restore();
  // tools/text-check.mjs turns this on to audit layout: everything that is
  // actually legible gets recorded with its box. y < 0 marks a measure-only
  // call (drawing off-screen to get a width), which is not layout.
  if (LOG_TEXT && y >= 0 && alpha > 0.5) {
    const x0 = align === 'center' ? x - w / 2 : align === 'right' ? x - w : x;
    LOG_TEXT.push({
      s, x: x0, y: y - size * 0.8, w, h: size * 1.05, size, weight,
    });
  }
  return w;
}

function ellipsize(str, maxW) {
  if (ctx.measureText(str).width <= maxW) return str;
  let s = str;
  while (s.length > 1 && ctx.measureText(s + '…').width > maxW) s = s.slice(0, -1);
  return s + '…';
}

/** CJK-aware wrap: break between any two chars, prefer not to split latin runs. */
function wrap(str, maxW, o = {}) {
  ctx.save();
  ctx.font = `${o.weight || 500} ${o.size || 28}px ${o.mono ? MONO : SANS}`;
  const lines = [];
  let line = '';
  for (const ch of str) {
    if (ch === '\n') { lines.push(line); line = ''; continue; }
    const test = line + ch;
    if (ctx.measureText(test).width > maxW && line) {
      lines.push(line);
      line = ch;
    } else {
      line = test;
    }
  }
  if (line) lines.push(line);
  ctx.restore();
  return lines;
}

function para(str, x, y, maxW, o = {}) {
  const { size = 26, lh = 1.6, ...rest } = o;
  const lines = wrap(str, maxW, { size, ...o });
  lines.forEach((l, i) => text(l, x, y + i * size * lh, { size, ...rest }));
  return lines.length * size * lh;
}

// ------------------------------------------------------------------- images

const ASSETS = {
  chatTools: 'chat-tools.png',
  chatTail: 'chat-tail.png',
  chatToolsLight: 'chat-tools-light.png',
  group: 'group.png',
  ask: 'ask.png',
  askCross: 'ask-cross.png',
  peerSend: 'peer-send.png',
  peerInbound: 'peer-inbound.png',
  peerBubble: 'peer-bubble.png',
  peerPopover: 'peer-popover.png',
  askToast: 'ask-toast.png',
  workspaceCode: 'workspace-code.png',
  workspaceSearch: 'workspace-search.png',
  workspaceDiff: 'workspace-diff.png',
  stats: 'stats.png',
  trajectory: 'trajectory.png',
  mobile: 'mobile.png',
  sidebar: 'sidebar.png',
  newSession: 'new-session.png',
};

const IMG = {};

/** filled by tools/text-check.mjs; null (and free) in normal renders */
let LOG_TEXT = null;

// Padded clip rects measured by tools/capture.mjs into assets/rects.json.
// These are the pieces the film animates on their own (a message arriving, a
// notification dropping in), so their placement has to be exact.
const RECTS_PEER_BUBBLE = { x: 968, y: 542, w: 1816, h: 223 };
const RECTS_ASK_TOAST = { x: 1112, y: 0, w: 976, h: 246 };

function loadImages() {
  const jobs = Object.entries(ASSETS).map(
    ([k, f]) =>
      new Promise((res, rej) => {
        const im = new Image();
        im.onload = () => { IMG[k] = im; res(); };
        im.onerror = () => rej(new Error('asset failed: ' + f));
        im.src = '../assets/' + f;
      }),
  );
  return Promise.all(jobs);
}

/**
 * Draw a screenshot inside `box`, clipped and rounded.
 * `cam` = {zoom, cx, cy} where zoom is screen px per source px and (cx,cy) is
 * the source point pinned to the box centre. Defaults to "fit whole image".
 * Returns a mapping function so callouts can be anchored in source coords.
 */
function shot(img, box, cam, r = 14, { alpha = 1, stroke = C.border } = {}) {
  const fit = Math.min(box.w / img.width, box.h / img.height);
  const z = cam && cam.zoom != null ? cam.zoom : fit;
  const cx = cam && cam.cx != null ? cam.cx : img.width / 2;
  const cy = cam && cam.cy != null ? cam.cy : img.height / 2;
  ctx.save();
  ctx.globalAlpha *= alpha;
  roundRectPath(box.x, box.y, box.w, box.h, r);
  ctx.clip();
  ctx.fillStyle = '#0b0f14';
  ctx.fillRect(box.x, box.y, box.w, box.h);
  ctx.imageSmoothingQuality = 'high';
  ctx.drawImage(
    img,
    box.x + box.w / 2 - cx * z,
    box.y + box.h / 2 - cy * z,
    img.width * z,
    img.height * z,
  );
  ctx.restore();
  if (stroke) strokeRound(box.x, box.y, box.w, box.h, r, stroke, 1.5);
  return (sx, sy) => ({ x: box.x + box.w / 2 + (sx - cx) * z, y: box.y + box.h / 2 + (sy - cy) * z });
}

/**
 * Spotlight a region of the screenshot: `rect` is in source pixels, `map` comes
 * from shot(). Draws a rounded outline plus a soft ring, which reads far better
 * at video speed than a leader line into a moving camera.
 */
function spotlight(map, rect, { alpha = 1, color = C.accent, pad = 10, pulse = 0 } = {}) {
  if (alpha <= 0.01) return;
  const a = map(rect.x, rect.y);
  const b = map(rect.x + rect.w, rect.y + rect.h);
  const x = Math.min(a.x, b.x) - pad;
  const y = Math.min(a.y, b.y) - pad;
  const w = Math.abs(b.x - a.x) + pad * 2;
  const h = Math.abs(b.y - a.y) + pad * 2;
  ctx.save();
  ctx.globalAlpha *= alpha;
  ctx.shadowColor = color;
  ctx.shadowBlur = 18 + pulse * 22;
  ctx.strokeStyle = color;
  ctx.lineWidth = 2.5;
  roundRectPath(x, y, w, h, 12);
  ctx.stroke();
  ctx.restore();
}

/** A small label chip next to a screen-space anchor (no leader line). */
function tag(anchor, label, { alpha = 1, color = C.accent } = {}) {
  if (alpha <= 0.01) return;
  ctx.save();
  ctx.globalAlpha *= alpha;
  ctx.font = `600 21px ${SANS}`;
  const w = ctx.measureText(label).width + 32;
  const h = 42;
  const x = Math.max(16, Math.min(W - w - 16, anchor.x + 26));
  const y = Math.max(16, Math.min(H - h - 16, anchor.y - h / 2));
  card(x, y, w, h, 9, { fill: 'rgba(13,17,23,.95)', stroke: color, shadow: 16 });
  text(label, x + 16, y + h / 2 + 8, { size: 21, weight: 600, color: C.fg });
  ctx.restore();
}

/** One caption at a time, crossfaded. items = [{on, off, text}] */
function captionSwitcher(t, items, x, y, o = {}) {
  for (const it of items) {
    const a = clamp01((t - it.on) / 0.4) * clamp01((it.off - t) / 0.35);
    if (a <= 0.01) continue;
    ctx.save();
    ctx.globalAlpha *= a;
    const off = (1 - easeOut(clamp01((t - it.on) / 0.45))) * 12;
    text(it.text, x, y + off, { size: 30, weight: 700, color: it.color || o.color || C.fg });
    ctx.restore();
  }
}

// --------------------------------------------------------------- background

const rand = mulberry(20261030);
const DUST = Array.from({ length: 90 }, () => ({
  x: rand() * W,
  y: rand() * H,
  r: 0.6 + rand() * 1.8,
  s: 4 + rand() * 16,
  a: 0.06 + rand() * 0.22,
}));

function backdrop(t) {
  const g = ctx.createLinearGradient(0, 0, W * 0.35, H);
  g.addColorStop(0, C.bg0);
  g.addColorStop(0.5, C.bg1);
  g.addColorStop(1, C.bg0);
  ctx.fillStyle = g;
  ctx.fillRect(0, 0, W, H);

  // two slow-drifting glows keep the frame from reading as flat black
  const glow = (x, y, r, col, a) => {
    const rg = ctx.createRadialGradient(x, y, 0, x, y, r);
    rg.addColorStop(0, col);
    rg.addColorStop(1, 'rgba(0,0,0,0)');
    ctx.save();
    ctx.globalAlpha = a;
    ctx.fillStyle = rg;
    ctx.fillRect(0, 0, W, H);
    ctx.restore();
  };
  glow(W * 0.18 + Math.sin(t * 0.07) * 60, H * 0.16 + Math.cos(t * 0.05) * 40, 760, 'rgba(88,166,255,.34)', 0.55);
  glow(W * 0.88 + Math.cos(t * 0.06) * 70, H * 0.86 + Math.sin(t * 0.045) * 50, 820, 'rgba(188,140,255,.24)', 0.5);

  // drifting grid
  ctx.save();
  ctx.strokeStyle = 'rgba(139,148,158,.075)';
  ctx.lineWidth = 1;
  const step = 72;
  const ox = (t * 7) % step;
  const oy = (t * 4) % step;
  for (let x = -step + ox; x < W + step; x += step) {
    ctx.beginPath(); ctx.moveTo(x, 0); ctx.lineTo(x, H); ctx.stroke();
  }
  for (let y = -step + oy; y < H + step; y += step) {
    ctx.beginPath(); ctx.moveTo(0, y); ctx.lineTo(W, y); ctx.stroke();
  }
  ctx.restore();

  // dust
  ctx.save();
  for (const d of DUST) {
    const y = (d.y - t * d.s) % (H + 40);
    ctx.globalAlpha = d.a;
    ctx.fillStyle = '#9fb6d1';
    ctx.beginPath();
    ctx.arc(d.x, y < -20 ? y + H + 40 : y, d.r, 0, Math.PI * 2);
    ctx.fill();
  }
  ctx.restore();

  // vignette
  const v = ctx.createRadialGradient(W / 2, H / 2, H * 0.34, W / 2, H / 2, H * 0.98);
  v.addColorStop(0, 'rgba(0,0,0,0)');
  v.addColorStop(1, 'rgba(0,0,0,.62)');
  ctx.fillStyle = v;
  ctx.fillRect(0, 0, W, H);
}

/** Accent per scene — the progress head and the chapter chip take this colour,
 *  so the bar reads as "where we are in the story", not just "how far along". */
const SCENE_ACCENT = {
  title: C.accent, problem: C.red, engines: C.purple, chat: C.green, peers: C.purple,
  group: C.red, panels: C.accent, mobile: C.amber, outro: C.green,
};

/**
 * Persistent chrome. This is the film's one continuous element: a head that
 * travels the bottom bar for the whole 83 seconds, ticks for each scene, and a
 * ripple on every cut. Scenes come and go; this does not — that continuity is
 * what keeps the whole thing feeling like one piece rather than a slide show.
 * (The reference film does the same thing with a ring that is on screen from
 * scene 1 to the end.)
 */
function chrome(sceneIdx, sceneLabel, t) {
  const accent = SCENE_ACCENT[SCENES[sceneIdx].id] || C.accent;
  ctx.save();
  ctx.globalAlpha = 0.9;
  fillRound(56, H - 66, 12, 12, 3, accent);
  text('AgentDeck', 78, H - 54, { size: 21, weight: 700, color: C.fg, spacing: '.4px' });
  if (sceneLabel) {
    const w = text(sceneLabel, 0, -999, { size: 19, color: C.muted });
    text(sceneLabel, W - 56 - w, H - 54, { size: 19, weight: 500, color: C.muted });
  }
  ctx.restore();

  const pw = W - 112;
  const barY = H - 26;
  const xAt = (tt) => 56 + pw * clamp01(tt / TOTAL);
  const head = xAt(t);

  fillRound(56, barY, pw, 3, 1.5, 'rgba(139,148,158,.20)');
  fillRound(56, barY, head - 56, 3, 1.5, accent);

  // a tick per scene, lighting up as its scene arrives
  for (let i = 1; i < SCENES.length; i++) {
    const x = xAt(STARTS[i]);
    const lit = clamp01((t - STARTS[i]) / 0.45);
    fillRound(x - 1, barY - 3, 2, 9, 1,
      lit > 0.5 ? SCENE_ACCENT[SCENES[i].id] || C.accent : 'rgba(139,148,158,.45)');
  }

  // the head: a soft glow, a ripple on every cut, and a slow breathe
  const ripple = clamp01((t - STARTS[sceneIdx]) / 0.9);
  ctx.save();
  ctx.shadowColor = accent;
  ctx.shadowBlur = 12 + 4 * Math.sin(t * 2.4);
  fillRound(head - 4, barY - 3.5, 8, 10, 4, accent);
  ctx.restore();
  if (ripple < 1) {
    ctx.save();
    ctx.globalAlpha = (1 - ripple) * 0.7;
    ctx.strokeStyle = accent;
    ctx.lineWidth = 2;
    ctx.beginPath();
    ctx.arc(head, barY + 1.5, 6 + ripple * 22, 0, Math.PI * 2);
    ctx.stroke();
    ctx.restore();
  }
}

/** Bilingual subtitle: Chinese on top, English dimmer underneath. */
function subtitle(cn, en, alpha) {
  if (alpha <= 0.01) return;
  const a = clamp01(alpha);
  ctx.save();
  ctx.globalAlpha = a;
  ctx.font = `600 34px ${SANS}`;
  const w1 = ctx.measureText(cn).width;
  ctx.font = `400 22px ${SANS}`;
  const w2 = en ? ctx.measureText(en).width : 0;
  const bw = Math.max(w1, w2) + 64;
  const bh = en ? 104 : 66;
  const bx = (W - bw) / 2;
  const by = H - 128 - bh + 26;
  const rise = (1 - easeOut(a)) * 14;
  ctx.translate(0, rise);
  fillRound(bx, by, bw, bh, 14, 'rgba(8,11,16,.72)');
  ctx.strokeStyle = 'rgba(48,54,61,.9)';
  ctx.lineWidth = 1;
  roundRectPath(bx, by, bw, bh, 14);
  ctx.stroke();
  text(cn, W / 2, by + 46, { size: 34, weight: 600, align: 'center', color: C.fg });
  if (en) text(en, W / 2, by + 82, { size: 22, weight: 400, align: 'center', color: C.muted, spacing: '.2px' });
  ctx.restore();
}

/** fade in over `fi` seconds, out over `fo` seconds, both at the scene's edges */
function sceneAlpha(p, dur, fi = 0.35, fo = 0.34) {
  const tt = p * dur;
  return Math.min(clamp01(tt / fi), clamp01((dur - tt) / fo));
}

// -------------------------------------------------------------------- scenes

/** Scene 1 — title */
function sceneTitle(p, t) {
  const a = sceneAlpha(p, 6, 0.54, 0.54);
  ctx.save();
  ctx.globalAlpha = a;

  // deck mark: three stacked bars sliding in
  const mx = W / 2;
  const my = 322;
  const bars = [
    { c: C.accent, w: 168, dy: 0 },
    { c: C.purple, w: 128, dy: 0 },
    { c: C.green, w: 88, dy: 0 },
  ];
  bars.forEach((b, i) => {
    const lp = clamp01((t - B.title.bars[i]) / 0.6);
    const e = easeOutBack(lp);
    const y = my - 44 + i * 30;
    ctx.save();
    ctx.globalAlpha = a * lp;
    ctx.shadowColor = b.c;
    ctx.shadowBlur = 26;
    fillRound(mx - b.w / 2, y + (1 - e) * 26, b.w, 12, 6, b.c);
    ctx.restore();
  });

  // wordmark, per character
  const word = 'AgentDeck';
  ctx.font = `700 138px ${SANS}`;
  const widths = [...word].map((c) => ctx.measureText(c).width + 2);
  const total = widths.reduce((s, x) => s + x, 0);
  let cx = mx - total / 2;
  [...word].forEach((c, i) => {
    const lp = clamp01((t - B.title.word - i * 0.055) / 0.55);
    const e = easeOut(lp);
    ctx.save();
    ctx.globalAlpha = a * lp;
    text(c, cx, 566 + (1 - e) * 34, { size: 138, weight: 700, color: C.fg, spacing: '-3px' });
    ctx.restore();
    cx += widths[i];
  });

  // accent rule
  const rp = clamp01((t - B.title.rule) / 0.7);
  const rw = 260 * easeInOut(rp);
  ctx.globalAlpha = a;
  fillRound(mx - rw / 2, 624, rw, 3, 2, C.accent);

  text('五个编码 CLI，一个 Web 控制台', mx, 676, {
    size: 42, weight: 600, align: 'center', color: C.fg, alpha: clamp01((t - B.title.cn) / 0.6),
  });
  text('FIVE CODING CLIS · ONE WEB CONSOLE', mx, 718, {
    size: 20, weight: 500, align: 'center', color: C.muted, spacing: '4px',
    alpha: clamp01((t - B.title.en) / 0.6),
  });

  const chips = ['单文件 Go 二进制', '前端内嵌 · 无构建', '手机 / 电脑开箱即用'];
  ctx.font = `500 23px ${SANS}`;
  const cw = chips.map((c) => ctx.measureText(c).width + 46);
  const gap = 20;
  const tot = cw.reduce((s, x) => s + x, 0) + gap * (chips.length - 1);
  let x = mx - tot / 2;
  chips.forEach((c, i) => {
    const lp = clamp01((t - B.title.chips[i]) / 0.55);
    ctx.save();
    ctx.globalAlpha = a * lp;
    card(x, 786 + (1 - easeOut(lp)) * 14, cw[i], 52, 26, {
      fill: 'rgba(22,27,34,.9)', stroke: C.border, shadow: 16,
    });
    text(c, x + cw[i] / 2, 786 + 34 + (1 - easeOut(lp)) * 14, {
      size: 23, align: 'center', color: C.muted,
    });
    ctx.restore();
    x += cw[i] + gap;
  });
  ctx.restore();
}

/** Scene 2 — the problem */
function sceneProblem(p, t) {
  const a = sceneAlpha(p, 8.5, 0.45, 0.45);
  ctx.save();
  ctx.globalAlpha = a;

  text('为什么需要 AgentDeck', 140, 148, { size: 46, weight: 700 });
  text('WHY AGENTDECK', 140, 186, { size: 19, weight: 500, color: C.dim, spacing: '4px' });

  // terminal card
  const tx = 140, ty = 258, tw = 812, th = 404;
  card(tx, ty, tw, th, 14, { fill: '#0c1017' });
  fillRound(tx, ty, tw, 40, 14, '#161b22');
  ctx.fillStyle = '#161b22';
  ctx.fillRect(tx, ty + 26, tw, 14);
  ['#f85149', '#d29922', '#3fb950'].forEach((c, i) => {
    ctx.beginPath();
    ctx.arc(tx + 24 + i * 20, ty + 20, 6, 0, Math.PI * 2);
    ctx.fillStyle = c;
    ctx.fill();
  });
  text('dev-box-03 — ssh', tx + 104, ty + 27, { size: 18, color: C.dim, mono: true });

  const lines = [
    { s: '$ ', c: C.green, t: 'ssh dev-box-03' },
    { s: '$ ', c: C.green, t: 'tmux ls' },
    { s: '', c: C.muted, t: '0: claude-api    1: opencode-ui   2: mcode-fix' },
    { s: '', c: C.muted, t: '3: qoder-migrate 4: codex-parse   5: docs-site' },
    { s: '', c: C.muted, t: '6: stats-refactor  7: ws-panel  …' },
    { s: '', c: C.amber, t: '# 谁在跑？跑到哪了？改过哪些文件？' },
  ];
  let ly = ty + 92;
  lines.forEach((l, i) => {
    const lp = clamp01((t - B.problem.lines[i]) / 0.36);
    if (lp <= 0) return;
    const chars = Math.floor(l.t.length * easeOut(lp));
    if (l.s) text(l.s, tx + 26, ly, { size: 22, mono: true, color: C.green });
    text(l.t.slice(0, chars), tx + 26 + (l.s ? 26 : 0), ly, { size: 22, mono: true, color: l.c });
    ly += 46;
  });
  const caretOn = Math.floor(t * 2) % 2 === 0 && t < 6.4;
  if (caretOn) fillRound(tx + 26, ly - 20, 13, 24, 2, C.fg);

  // pain rows
  const rows = [
    { cn: '远程开发，等于反复 SSH 回终端', en: 'Remote work means going back to a terminal' },
    { cn: '多项目并行，会话散在十几个 tmux 窗口', en: 'Parallel projects scattered across tmux windows' },
    { cn: '工具调用看不见，只能翻滚动日志', en: 'Tool calls buried in scrolling logs' },
  ];
  rows.forEach((r, i) => {
    const lp = clamp01((t - B.problem.rows[i]) / 0.7);
    if (lp <= 0) return;
    const y = 320 + i * 158;
    ctx.save();
    ctx.globalAlpha = a * lp;
    const off = (1 - easeOut(lp)) * 30;
    card(1046, y + off, 734, 118, 14, { fill: 'rgba(22,27,34,.86)' });
    fillRound(1074, y + 34 + off, 50, 50, 12, 'rgba(248,81,73,.14)');
    ctx.strokeStyle = C.red;
    ctx.lineWidth = 3;
    ctx.beginPath();
    ctx.moveTo(1089, y + 49 + off); ctx.lineTo(1109, y + 69 + off);
    ctx.moveTo(1109, y + 49 + off); ctx.lineTo(1089, y + 69 + off);
    ctx.stroke();
    text(r.cn, 1146, y + 56 + off, { size: 27, weight: 600 });
    text(r.en, 1146, y + 88 + off, { size: 18, color: C.dim, spacing: '.2px' });
    ctx.restore();
  });

  ctx.restore();
  subtitle('会话散在十几个终端窗口里', 'Sessions scattered across a dozen terminal windows', sceneAlpha(p, 8.5, 0.8, 0.9) * (t > 1.2 ? 1 : 0));
}

/** Scene 3 — five engines */
function sceneEngines(p, t) {
  const a = sceneAlpha(p, 9.0, 0.45, 0.5);
  ctx.save();
  ctx.globalAlpha = a;

  const hubX = 1418, hubY = 486;
  // Codex sits directly under Claude Code: it is the second engine the
  // console supports end to end.
  const engines = [
    { name: 'Claude Code', hint: 'stream-json · 常驻 · 跨会话消息', c: C.accent },
    { name: 'Codex', hint: 'exec --json · rollout', c: C.cyan },
    { name: 'opencode', hint: 'serve + SSE · 权限卡', c: C.green },
    { name: 'MiniMax Code', hint: 'exec stream-json', c: C.amber },
    { name: 'Qoder', hint: 'Claude Code fork', c: C.purple },
  ];
  const bx = 150, bw = 560, bh = 104;
  const ys = engines.map((_, i) => 138 + i * 146);

  engines.forEach((e, i) => {
    const lp = clamp01((t - B.engines.badges[i]) / 0.55);
    if (lp <= 0) return;
    const off = (1 - easeOut(lp)) * -34;
    ctx.save();
    ctx.globalAlpha = a * lp;
    card(bx + off, ys[i], bw, bh, 14, { fill: 'rgba(22,27,34,.92)' });
    ctx.save();
    ctx.shadowColor = e.c;
    ctx.shadowBlur = 22;
    fillRound(bx + off + 24, ys[i] + 34, 36, 36, 11, e.c);
    ctx.restore();
    text(e.name, bx + off + 78, ys[i] + 46, { size: 29, weight: 600 });
    text(e.hint, bx + off + 78, ys[i] + 78, { size: 18, color: C.dim, mono: true });
    ctx.restore();

    // connector: badge right edge → hub left edge, drawn progressively
    const cp = clamp01((t - B.engines.conn[i]) / 0.8);
    if (cp > 0) {
      const x0 = bx + bw, y0 = ys[i] + bh / 2;
      const x1 = hubX - 148, y1 = hubY;
      const mx = lerp(x0, x1, 0.55);
      ctx.save();
      ctx.globalAlpha = a * 0.85;
      ctx.strokeStyle = e.c;
      ctx.lineWidth = 2.2;
      ctx.setLineDash([9, 7]);
      ctx.lineDashOffset = -t * 34;
      const seg = easeInOut(cp);
      ctx.beginPath();
      ctx.moveTo(x0, y0);
      ctx.bezierCurveTo(lerp(x0, mx, seg), y0, lerp(mx, x1, seg), y1, lerp(x0, x1, seg), lerp(y0, y1, seg));
      ctx.stroke();
      ctx.restore();
      // travelling dot once connected
      if (cp >= 1) {
        const dp = (t * 0.55 + i * 0.17) % 1;
        const dx = lerp(x0, x1, easeInOut(dp));
        const dy = lerp(y0, y1, easeInOut(dp));
        ctx.save();
        ctx.shadowColor = e.c;
        ctx.shadowBlur = 18;
        ctx.beginPath();
        ctx.arc(dx, dy, 5, 0, Math.PI * 2);
        ctx.fillStyle = e.c;
        ctx.fill();
        ctx.restore();
      }
    }
  });

  // hub
  const hp = clamp01((t - B.engines.hub) / 0.7);
  ctx.save();
  ctx.globalAlpha = a * hp;
  const glowPulse = 0.5 + 0.5 * Math.sin(t * 2.4);
  ctx.shadowColor = C.accent;
  ctx.shadowBlur = 40 + glowPulse * 26;
  card(hubX - 148, hubY - 96, 300, 192, 20, { fill: '#111823', stroke: C.accent, shadow: 46, lw: 1.5 });
  ctx.restore();
  text('AgentDeck', hubX + 2, hubY - 12, { size: 44, weight: 700, align: 'center', color: C.fg, spacing: '-1px' });
  text('统一控制台', hubX + 2, hubY + 30, { size: 22, align: 'center', color: C.muted });
  const done = clamp01((t - B.engines.done) / 0.9);
  text('1 个二进制 · 5 个引擎', hubX + 2, hubY + 74, {
    size: 20, weight: 600, align: 'center', color: C.green, alpha: done,
  });

  ctx.restore();
  subtitle('同一套界面里管理五个引擎的会话', 'Five engines, one interface', sceneAlpha(p, 9.0, 0.9, 1.05) * (t > 3.4 ? 1 : 0));
}

/** Scene 4 — the live chat, with a moving camera over the real screenshot */
function sceneChat(p, t) {
  const a = sceneAlpha(p, 14, 0.5, 0.6);
  const img = IMG.chatTools;
  const fit = Math.min(1500 / img.width, 937 / img.height);
  const box = { x: 210, y: 96, w: 1500, h: 937 };

  // Camera keyframes: (time, zoom, centre) in source pixels. `zoom` is screen px
  // per source px, so fit ≈ 0.469 and 0.72 is a ~1.5x magnification of the UI.
  const cam = (tt) => {
    const keys = [
      [0.0, fit, 1600, 1000],
      [2.4, fit, 1600, 1000],
      [B.chat.push, 0.72, 1900, 1010],
      [6.9, 0.72, 1900, 1010],
      [B.chat.pan, 0.72, 1900, 1349],
      [10.8, 0.72, 1900, 1349],
      [B.chat.back, fit, 1600, 1000],
      [14.0, fit, 1600, 1000],
    ];
    let z = fit, cx = 1600, cy = 1000;
    for (let i = 0; i < keys.length - 1; i++) {
      const [t0, z0, x0, y0] = keys[i];
      const [t1, z1, x1, y1] = keys[i + 1];
      if (tt >= t0 && tt <= t1) {
        const k = easeInOut((tt - t0) / (t1 - t0));
        z = lerp(z0, z1, k); cx = lerp(x0, x1, k); cy = lerp(y0, y1, k);
        break;
      }
      const last = keys[keys.length - 1];
      z = last[1]; cx = last[2]; cy = last[3];
    }
    // keep the visible window inside the image, otherwise the frame shows the
    // letterbox fill below the screenshot's bottom edge
    const vw = box.w / z;
    const vh = box.h / z;
    return {
      zoom: z,
      cx: Math.max(vw / 2, Math.min(img.width - vw / 2, cx)),
      cy: Math.max(vh / 2, Math.min(img.height - vh / 2, cy)),
    };
  };

  const c = cam(t);
  const enter = easeOut(clamp01(t / 0.8));
  const b2 = {
    x: box.x + (1 - enter) * 40,
    y: box.y + (1 - enter) * 54,
    w: box.w * lerp(1.05, 1, enter),
    h: box.h * lerp(1.05, 1, enter),
  };
  ctx.save();
  ctx.globalAlpha = a * clamp01(t / 0.55);
  ctx.save();
  ctx.shadowColor = 'rgba(0,0,0,.7)';
  ctx.shadowBlur = 60;
  ctx.shadowOffsetY = 24;
  fillRound(b2.x, b2.y, b2.w, b2.h, 16, '#0b0f14');
  ctx.restore();
  const map = shot(img, b2, c, 16, { stroke: 'rgba(88,166,255,.35)' });
  ctx.restore();

  // spotlight the feature the caption is talking about (source-pixel rects)
  const rects = {
    thinking: { x: 992, y: 472, w: 1760, h: 176 },
    bash: { x: 992, y: 840, w: 1760, h: 468 },
    code: { x: 992, y: 1306, w: 1760, h: 190 },
    task: { x: 992, y: 1390, w: 1760, h: 600 },
  };
  const spot = (key, on, off, color) => {
    const al = a * clamp01((t - on) / 0.4) * clamp01((off - t) / 0.35);
    if (al > 0.01) spotlight(map, rects[key], { alpha: al, color, pulse: 0.5 + 0.5 * Math.sin(t * 3) });
  };
  spot('thinking', B.chat.think, B.chat.push, C.purple);
  spot('bash', B.chat.bash, B.chat.pan, C.green);
  spot('task', B.chat.task, B.chat.back, C.amber);

  const captions = [
    { on: 0.6, off: 4.2, text: '流式 Markdown · 代码高亮 · thinking 折叠' },
    { on: B.chat.caps[0], off: B.chat.pan, text: '工具调用卡片：Bash / Edit diff / TodoWrite' },
    { on: B.chat.caps[1], off: 12.3, text: '任务面板：TodoWrite 实时折叠，不占对话流' },
    { on: B.chat.caps[2], off: 14.0, text: '会话信息条：轮数 · 步数 · 耗时 · tok/s' },
  ];
  ctx.save();
  ctx.globalAlpha = a;
  captionSwitcher(t, captions, 210, 66);
  ctx.restore();

  subtitle('流式对话、工具卡片、thinking 折叠', 'Streaming chat, tool cards, collapsible thinking', sceneAlpha(p, 14, 1.4, 1.4) * clamp01((t - 0.9) / 0.6));
}

/** Scene 5 — agent2agent: two sessions talking to each other by address name */
function scenePeers(p, t) {
  const a = sceneAlpha(p, 12, 0.6, 0.7);
  const full = { x: 210, y: 128, w: 1500, h: 937 };
  const shrunk = { x: 856, y: 286, w: 1000, h: 625 };
  const base = 1500 / 1760; // the message column exactly fills the frame

  // Timeline. The camera only moves when it has a reason to; the motion in this
  // scene is the exchange itself, not a drift.
  const TL = B.peers; // every beat lives in src/timeline.js, where the score reads it too
  const span = ([t0, t1]) => clamp01((t - t0) / (t1 - t0));
  const push = easeInOut(span(TL.switchView));
  const shrink = easeInOut(span(TL.toMechanism));
  const enter = easeOut(clamp01(t / 0.8));

  const boxAt = (b, dx = 0, dy = 0, k = 1) => ({
    x: b.x + (1 - enter) * 40 + dx,
    y: b.y + (1 - enter) * 54 + dy,
    w: b.w * k,
    h: b.h * k,
  });
  const mapOf = (b, cam) => (sx, sy) => ({
    x: b.x + b.w / 2 + (sx - cam.cx) * cam.zoom,
    y: b.y + b.h / 2 + (sy - cam.cy) * cam.zoom,
  });

  const camA = { zoom: base, cx: 1876, cy: 1250 };
  const camB = {
    zoom: lerp(base, 1000 / 3200, shrink),
    cx: lerp(1876, 1600, shrink),
    cy: lerp(867, 1000, shrink),
  };

  const bA = boxAt(full, -push * 90, 0, 1);
  const bB = boxAt(lerpBox(full, shrunk, shrink), (1 - push) * 150, 0, 1);
  const mapA = mapOf(bA, camA);
  const mapB = mapOf(bB, camB);

  const paint = (img, b, cam, alpha) => {
    if (alpha <= 0.01) return;
    ctx.save();
    ctx.globalAlpha = a * alpha * clamp01(t / 0.5);
    ctx.save();
    ctx.shadowColor = 'rgba(0,0,0,.7)';
    ctx.shadowBlur = 60;
    ctx.shadowOffsetY = 24;
    fillRound(b.x, b.y, b.w, b.h, 16, '#0b0f14');
    ctx.restore();
    shot(img, b, cam, 16, { stroke: 'rgba(188,140,255,.45)' });
    ctx.restore();
  };
  paint(IMG.peerSend, bA, camA, 1 - push);
  paint(IMG.peerInbound, bB, camB, push);

  // ---- the link between the two sessions (a HUD, not part of the UI) ----
  const linkAlpha = a * clamp01(t / 0.5) * (1 - shrink);
  const LK = {
    lineY: 62,
    lineX0: 1050,
    lineX1: 1430,
    left: { x: 840, y: 40, w: 200, h: 44 },   // arch-review
    right: { x: 1440, y: 40, w: 190, h: 44 }, // docs-site
  };
  // `active` is 1 for the session we are looking at, 0 for the other, and
  // briefly 1 on the other side when a message lands there.
  const chip = (r, name, color, active) => {
    const on = active > 0.5;
    ctx.save();
    ctx.globalAlpha = linkAlpha * (0.42 + 0.58 * active);
    if (on) {
      ctx.save();
      ctx.shadowColor = color;
      ctx.shadowBlur = 16 + (active - 0.5) * 34;
      fillRound(r.x, r.y, r.w, r.h, 22, 'rgba(13,17,23,.96)');
      ctx.restore();
    }
    card(r.x, r.y, r.w, r.h, 22, {
      fill: 'rgba(13,17,23,.94)',
      stroke: on ? color : C.border,
      shadow: 16,
    });
    ctx.beginPath();
    ctx.arc(r.x + 24, r.y + r.h / 2, 7, 0, Math.PI * 2);
    ctx.fillStyle = color;
    ctx.fill();
    text(name, r.x + 42, r.y + r.h / 2 + 8, {
      size: 21, weight: 600, mono: true, color: on ? C.fg : C.muted,
    });
    ctx.restore();
  };

  if (linkAlpha > 0.02) {
    // the arch-review side is "us" until the view switches
    const hlA = 1 - push;
    const hlB = push;
    // arrival flashes
    const upLand = span([TL.packetUp[1] - 0.12, TL.packetUp[1] + 0.5]);
    const backLand = span([TL.packetBack[1] - 0.12, TL.packetBack[1] + 0.5]);
    const flash = (u) => (u > 0 && u < 1 ? Math.sin(Math.PI * u) : 0);

    // the dashed link
    ctx.save();
    ctx.globalAlpha = linkAlpha * 0.55;
    ctx.strokeStyle = C.purple;
    ctx.lineWidth = 2;
    ctx.setLineDash([8, 7]);
    ctx.lineDashOffset = -t * 26;
    ctx.beginPath();
    ctx.moveTo(LK.lineX0, LK.lineY);
    ctx.lineTo(LK.lineX1, LK.lineY);
    ctx.stroke();
    ctx.restore();

    chip(LK.left, 'arch-review', C.accent, Math.max(hlA, flash(backLand)));
    chip(LK.right, 'docs-site', C.green, Math.max(hlB, flash(upLand)));

    // expanding ring on arrival
    const ring = (r, u, color) => {
      if (u <= 0 || u >= 1) return;
      ctx.save();
      ctx.globalAlpha = linkAlpha * (1 - u) * 0.9;
      ctx.strokeStyle = color;
      ctx.lineWidth = 2.5;
      roundRectPath(r.x - u * 26, r.y - u * 18, r.w + u * 52, r.h + u * 36, 22 + u * 10);
      ctx.stroke();
      ctx.restore();
    };
    ring(LK.right, upLand, C.green);
    ring(LK.left, backLand, C.accent);
  }

  // a message travelling between the two sessions
  const packet = (from, to, u, color, ctrl) => {
    if (u <= 0 || u >= 1) return;
    const e = easeInOut(u);
    const q = (p0, p1, p2, k) => (1 - k) * (1 - k) * p0 + 2 * (1 - k) * k * p1 + k * k * p2;
    const x = q(from.x, ctrl.x, to.x, e);
    const y = q(from.y, ctrl.y, to.y, e);
    ctx.save();
    ctx.globalAlpha = a * Math.sin(Math.PI * Math.min(1, u * 1.15)) * 0.95;
    ctx.strokeStyle = color;
    ctx.lineWidth = 2.5;
    ctx.beginPath();
    ctx.moveTo(q(from.x, ctrl.x, to.x, Math.max(0, e - 0.12)), q(from.y, ctrl.y, to.y, Math.max(0, e - 0.12)));
    ctx.lineTo(x, y);
    ctx.stroke();
    ctx.shadowColor = color;
    ctx.shadowBlur = 20;
    ctx.beginPath();
    ctx.arc(x, y, 6.5, 0, Math.PI * 2);
    ctx.fillStyle = color;
    ctx.fill();
    ctx.restore();
  };

  // ---- the SendMessage card lights up, then the packet leaves it ----
  const sweep = (map, rect, u, alpha) => {
    if (u <= 0 || u >= 1 || alpha <= 0.01) return;
    const p0 = map(rect.x, rect.y);
    const p1 = map(rect.x + rect.w, rect.y + rect.h);
    ctx.save();
    ctx.globalAlpha = a * alpha;
    roundRectPath(p0.x, p0.y, p1.x - p0.x, p1.y - p0.y, 12);
    ctx.clip();
    const cx = p0.x - (p1.x - p0.x) * 0.35 + (p1.x - p0.x) * 1.7 * u;
    const g = ctx.createLinearGradient(cx - 130, 0, cx + 130, 0);
    g.addColorStop(0, 'rgba(188,140,255,0)');
    g.addColorStop(0.5, 'rgba(188,140,255,.5)');
    g.addColorStop(1, 'rgba(188,140,255,0)');
    ctx.fillStyle = g;
    ctx.fillRect(p0.x, p0.y, p1.x - p0.x, p1.y - p0.y);
    ctx.restore();
  };

  const SEND_CARD = { x: 996, y: 1121, w: 1760, h: 325 };
  const REPLY_CARD = { x: 996, y: 884, w: 1760, h: 280 };
  const BUBBLE = { x: 996, y: 570, w: 1760, h: 167 };

  if (push < 0.5) {
    const u = span(TL.sendSweep);
    sweep(mapA, SEND_CARD, u, 1 - push * 2);
    // packet: out of the card, up to the docs-site chip
    packet(
      { x: 1600, y: mapA(SEND_CARD.x, SEND_CARD.y).y },
      { x: LK.right.x + LK.right.w / 2, y: LK.right.y + LK.right.h / 2 },
      span(TL.packetUp),
      C.purple,
      { x: 1700, y: 220 },
    );
  }

  // ---- the message arrives in the other conversation ----
  if (push >= 1) {
    packet(
      { x: LK.right.x + LK.right.w / 2, y: LK.right.y + LK.right.h / 2 },
      { x: 1600, y: mapB(BUBBLE.x, BUBBLE.y).y },
      span(TL.packetDown),
      C.green,
      { x: 1560, y: 200 },
    );

    const cl = RECTS_PEER_BUBBLE;
    const p0 = mapB(cl.x, cl.y);
    const p1 = mapB(cl.x + cl.w, cl.y + cl.h);
    const arrive = clamp01((t - TL.bubbleIn) / 0.75);
    const sp = easeOutBack(arrive);
    const dx = (1 - sp) * 120;
    const dy = (1 - sp) * -14;
    ctx.save();
    ctx.globalAlpha = a * clamp01((t - TL.bubbleIn + 0.05) / 0.35);
    ctx.drawImage(IMG.peerBubble, p0.x + dx, p0.y + dy, p1.x - p0.x, p1.y - p0.y);
    ctx.restore();
    const fl = clamp01((t - TL.bubbleIn + 0.1) / 0.6);
    if (fl > 0 && fl < 1) {
      ctx.save();
      ctx.globalAlpha = a * (1 - fl) * 0.75;
      ctx.shadowColor = C.purple;
      ctx.shadowBlur = 26;
      ctx.strokeStyle = C.purple;
      ctx.lineWidth = 3;
      roundRectPath(p0.x + dx - 4, p0.y + dy - 4, p1.x - p0.x + 8, p1.y - p0.y + 8, 12);
      ctx.stroke();
      ctx.restore();
    }

    // the reply: the card lights up and a packet heads back
    sweep(mapB, REPLY_CARD, span(TL.replySweep), 1 - shrink * 1.4);
    packet(
      { x: 1600, y: mapB(REPLY_CARD.x, REPLY_CARD.y).y },
      { x: LK.left.x + LK.left.w / 2, y: LK.left.y + LK.left.h / 2 },
      span(TL.packetBack),
      C.accent,
      { x: 1620, y: 180 },
    );

    if (shrink < 0.5) {
      const sp = (rect, on, off, color) =>
        spotlight(mapB, rect, {
          alpha: a * clamp01((t - on) / 0.4) * clamp01((off - t) / 0.4),
          color,
          pulse: 0.5 + 0.5 * Math.sin(t * 3),
        });
      sp(BUBBLE, 6.0, 8.4, C.purple);
      sp(REPLY_CARD, 6.7, 8.4, C.green);
    }
  }

  // ---- beat C: how it is addressable at all ----
  const pc = clamp01((t - B.peers.popover) / 0.75);
  if (pc > 0.01) {
    const sp = easeOutBack(pc);
    const pw = 620;
    const ph = pw * (800 / 1360);
    const px = 140 - (1 - sp) * 260;
    const py = 366;
    ctx.save();
    ctx.globalAlpha = a * clamp01(pc * 2);
    ctx.save();
    ctx.shadowColor = 'rgba(0,0,0,.75)';
    ctx.shadowBlur = 46;
    ctx.shadowOffsetY = 18;
    fillRound(px, py, pw, ph, 12, '#0d1117');
    ctx.restore();
    ctx.save();
    roundRectPath(px, py, pw, ph, 12);
    ctx.clip();
    ctx.drawImage(IMG.peerPopover, px, py, pw, ph);
    ctx.restore();
    strokeRound(px, py, pw, ph, 12, 'rgba(63,185,80,.6)', 1.5);
    ctx.restore();

    const chips = ['常驻进程', '地址名 = 可寻址', '双向可见'];
    ctx.font = `600 24px ${SANS}`;
    let cy = py + ph + 34;
    chips.forEach((label, i) => {
      const lp = clamp01((t - B.peers.chips[i]) / 0.5);
      if (lp <= 0) return;
      const off = (1 - easeOutBack(lp)) * 18;
      const w = ctx.measureText(label).width + 56;
      ctx.save();
      ctx.globalAlpha = a * lp;
      card(px, cy + off, w, 56, 28, { fill: 'rgba(22,27,34,.92)', stroke: C.border, shadow: 14 });
      fillRound(px + 22, cy + 24 + off, 9, 9, 5, C.green);
      text(label, px + 44, cy + 37 + off, { size: 24, weight: 600, color: C.fg });
      ctx.restore();
      cy += 72;
    });
  }

  captionSwitcher(
    t,
    [
      { on: 0.6, off: 4.4, text: '会话 A：按地址名把结论投递过去' },
      { on: B.peers.caps[0], off: 8.6, text: '会话 B：消息作为一条新输入进对话流' },
      { on: B.peers.caps[1], off: 12.0, text: '常驻 + 地址名，其他会话就能寻址它' },
    ],
    210,
    66,
  );

  subtitle(
    t < 8.4 ? '两个会话直接对话，不用共享剪贴板' : '常驻进程 + 地址名 = 可被其他会话寻址',
    t < 8.4 ? 'Two sessions talking, no clipboard in between' : 'Resident process + address name',
    sceneAlpha(p, 12, 1.2, 1.3) * clamp01((t - 0.8) / 0.6),
  );
}


/** Scene 6 — group chat, then the pending ask seen from ANOTHER session */
function sceneGroup(p, t) {
  const a = sceneAlpha(p, 11, 0.6, 0.7);
  const box = { x: 200, y: 132, w: 1520, h: 950 };
  const fit = Math.min(box.w / 3200, box.h / 2000);
  const base = 0.72; // pushed in on the approval card

  // beats: group (0–4.2) → the ask (5.2–7.0) → the ask from elsewhere (7.9–11)
  const cross1 = easeInOut(clamp01((t - B.group.toAsk) / 1.0));
  const cross2 = easeInOut(clamp01((t - B.group.toCross) / 0.9));
  const enter = easeOut(clamp01(t / 0.8));

  const boxAt = (dx = 0, dy = 0) => ({
    x: box.x + (1 - enter) * 40 + dx,
    y: box.y + (1 - enter) * 54 + dy,
    w: box.w,
    h: box.h,
  });
  const mapOf = (b, cam) => (sx, sy) => ({
    x: b.x + b.w / 2 + (sx - (cam ? cam.cx : 1600)) * (cam ? cam.zoom : fit),
    y: b.y + b.h / 2 + (sy - (cam ? cam.cy : 1000)) * (cam ? cam.zoom : fit),
  });

  // the approval card docks above the composer, so it rises into place.
  // No drift: the camera holds a framing unless it has a reason to move.
  const camAsk = { zoom: base, cx: 1884, cy: 1115 };

  const paint = (img, b, cam, alpha) => {
    if (alpha <= 0.01) return;
    ctx.save();
    ctx.globalAlpha = a * alpha * clamp01(t / 0.5);
    ctx.save();
    ctx.shadowColor = 'rgba(0,0,0,.72)';
    ctx.shadowBlur = 60;
    ctx.shadowOffsetY = 24;
    fillRound(b.x, b.y, b.w, b.h, 16, '#0b0f14');
    ctx.restore();
    shot(img, b, cam, 16, { stroke: 'rgba(248,81,73,.5)' });
    ctx.restore();
  };

  paint(IMG.group, boxAt(0, 0), { zoom: fit, cx: 1600, cy: 1000 }, 1 - cross1);
  paint(IMG.ask, boxAt(0, (1 - cross1) * 90), camAsk, cross1 * (1 - cross2));
  // switching conversation: the other session's view slides in
  paint(IMG.askCross, boxAt((1 - cross2) * 210, 0), { zoom: fit, cx: 1600, cy: 1000 }, cross2);

  const mapAsk = mapOf(boxAt(0, (1 - cross1) * 90), camAsk);
  const mapCross = mapOf(boxAt((1 - cross2) * 210, 0), null);

  // member chips over the group timeline
  const members = [
    { n: '架构', c: C.accent },
    { n: '前端', c: C.amber },
    { n: '评审', c: C.green },
  ];
  members.forEach((m, i) => {
    const lp = clamp01((t - B.group.members[i]) / 0.5);
    if (lp <= 0) return;
    ctx.save();
    ctx.globalAlpha = a * lp * (1 - cross1);
    const y = box.y + 26 + (1 - easeOutBack(lp)) * -22;
    const x = box.x + 40 + i * 168;
    card(x, y, 148, 52, 26, { fill: 'rgba(13,17,23,.96)', stroke: m.c, shadow: 16 });
    ctx.beginPath();
    ctx.arc(x + 30, y + 26, 8, 0, Math.PI * 2);
    ctx.fillStyle = m.c;
    ctx.fill();
    text(m.n, x + 48, y + 34, { size: 22, weight: 600 });
    ctx.restore();
  });

  // the ask itself: the card pulses, the countdown is the point
  spotlight(mapAsk, { x: 1030, y: 821, w: 1708, h: 588 }, {
    alpha: a * clamp01((t - B.group.askLit) / 0.4) * clamp01((7.6 - t) / 0.4),
    color: C.red,
    pulse: 0.5 + 0.5 * Math.sin(t * 3.2),
  });

  // the cross-session shot: the ask belongs to another conversation
  if (cross2 > 0.02) {
    const al = (on, off) => a * clamp01((t - on) / 0.4) * clamp01((off - t) / 0.4);
    // ask-cross.png: #toast-layer .toast x 1140..2060 y 28..218
    //               .session-item.has-ask x 16..542 y 665..790
    //               #ws-activity x 122..323 y 239..281
    // the banner drops in from above, the way a notification actually arrives
    const drop = clamp01((t - B.group.toastDrop) / 0.65);
    if (drop > 0) {
      const cl = RECTS_ASK_TOAST;
      const p0 = mapCross(cl.x, cl.y);
      const p1 = mapCross(cl.x + cl.w, cl.y + cl.h);
      const sp = easeOutBack(drop);
      const dy = (1 - sp) * -190;
      ctx.save();
      ctx.globalAlpha = a * clamp01(drop * 1.6);
      ctx.save();
      ctx.shadowColor = 'rgba(0,0,0,.8)';
      ctx.shadowBlur = 40;
      ctx.shadowOffsetY = 16;
      ctx.drawImage(IMG.askToast, p0.x, p0.y + dy, p1.x - p0.x, p1.y - p0.y);
      ctx.restore();
      ctx.restore();
    }
    spotlight(mapCross, { x: 1140, y: 28, w: 920, h: 190 }, { alpha: al(8.5, 11.0), color: C.red, pulse: 0.5 + 0.5 * Math.sin(t * 4) });
    // the sidebar row pulses in the real app; the screenshot can't show that,
    // so the pulse is drawn on top
    const rowA = mapCross(16, 665);
    const rowB = mapCross(542, 790);
    ctx.save();
    ctx.globalAlpha = a * clamp01((t - B.group.rowPulse) / 0.4) * (0.10 + 0.13 * (0.5 + 0.5 * Math.sin(t * 3.4)));
    fillRound(rowA.x, rowA.y, rowB.x - rowA.x, rowB.y - rowA.y, 8, C.amber);
    ctx.restore();
    spotlight(mapCross, { x: 16, y: 665, w: 526, h: 125 }, { alpha: al(B.group.rowPulse, 11.0), color: C.amber });
    spotlight(mapCross, { x: 122, y: 239, w: 201, h: 42 }, { alpha: al(B.group.badge, 11.0), color: C.accent });
    tag(mapCross(2070, 240), '横幅：别的会话在提问', { alpha: al(8.6, 11.0), color: C.red });
    tag(mapCross(556, 727), '侧栏整行脉动 + 待回答', { alpha: al(8.8, 11.0), color: C.amber });
  }

  captionSwitcher(
    t,
    [
      { on: 0.6, off: 4.3, text: '群聊 · 多 agent 并行' },
      { on: B.group.caps[0], off: 7.8, text: '模型提问，卡片只停在当前会话' },
      { on: B.group.caps[1], off: 11.0, text: '你在别的会话？横幅照样弹出来', color: C.red },
    ],
    200,
    88,
  );

  subtitle(
    t < 4.2
      ? '一个群拉多个成员，@成员定向、@all 并行'
      : t < 8.2
        ? '提问卡只出现在你打开的那个会话里'
        : '点「去回答」直达那张卡，侧栏整行脉动',
    t < 4.2
      ? 'Parallel agents in one group'
      : t < 8.2
        ? 'The card only lives in the session you have open'
        : 'Jump straight to the card from anywhere',
    sceneAlpha(p, 11, 1.1, 1.2) * clamp01((t - 0.8) / 0.6),
  );
}


/** Scene 7 — workspace / stats / trajectory, closing on a real diff */
function scenePanels(p, t) {
  const a = sceneAlpha(p, 9.5, 0.5, 0.6);
  const items = [
    {
      img: IMG.workspaceCode, label: '工作区面板', en: 'Workspace',
      focus: { x: 1620, y: 60, w: 1580, h: 1880 },
      note: '文件树 + 预览 + git 改动',
    },
    {
      img: IMG.stats, label: '使用统计', en: 'Stats',
      focus: { x: 1010, y: 500, w: 1800, h: 1140 },
      note: '五引擎合并 · 热力图',
    },
    {
      img: IMG.trajectory, label: '轨迹视图', en: 'Trajectory',
      focus: { x: 1010, y: 250, w: 1800, h: 1130 },
      note: '按轮分组的台账',
    },
  ];
  const bw = 566, bh = 354;
  const xs = [92, 677, 1262];
  const y = 300;

  // phase B: the row slides up and a full-frame diff takes over
  const zoom = clamp01((t - B.panels.zoom) / 1.0);
  const rowAlpha = a * (1 - zoom);
  if (rowAlpha > 0.01) {
    ctx.save();
    ctx.globalAlpha = rowAlpha;
    ctx.translate(0, -zoom * 90);
    items.forEach((it, i) => {
      const lp = clamp01((t - B.panels.cards[i]) / 0.6);
      const hl = clamp01((t - B.panels.hilite[i]) / 0.4) * (1 - clamp01((t - B.panels.hilite[i] - 1.2) / 0.4));
      const s = 1 + hl * 0.055;
      const w = bw * s, h = bh * s;
      const x = xs[i] + (bw - w) / 2;
      const yy = y + (bh - h) / 2 + (1 - easeOut(lp)) * 34;
      ctx.save();
      ctx.globalAlpha = rowAlpha * lp;
      ctx.save();
      ctx.shadowColor = hl > 0.1 ? 'rgba(88,166,255,.55)' : 'rgba(0,0,0,.6)';
      ctx.shadowBlur = 34 + hl * 26;
      ctx.shadowOffsetY = 14;
      fillRound(x, yy, w, h, 14, '#0b0f14');
      ctx.restore();
      const f = it.focus;
      const z = Math.max(w / f.w, h / f.h);
      shot(it.img, { x, y: yy, w, h }, { zoom: z, cx: f.x + f.w / 2, cy: f.y + f.h / 2 }, 14, {
        stroke: hl > 0.4 ? C.accent : C.border,
      });
      card(x, yy + h + 22, w, 92, 12, { fill: 'rgba(22,27,34,.94)', shadow: 16 });
      text(it.label, x + 24, yy + h + 60, { size: 28, weight: 700 });
      text(it.en.toUpperCase(), x + w - 24, yy + h + 58, {
        size: 17, color: C.dim, align: 'right', spacing: '3px',
      });
      text(it.note, x + 24, yy + h + 92, { size: 19, color: C.muted });
      ctx.restore();
    });
    ctx.restore();
  }

  // phase B: real git diff, full frame, camera pushed into the preview pane
  if (zoom > 0.01) {
    const box = { x: 210, y: 128, w: 1500, h: 937 };
    const e = easeOut(zoom);
    const b = {
      x: box.x, y: box.y + (1 - e) * 60,
      w: box.w, h: box.h,
    };
    // push in on the diff pane so the hunk is readable at video size
    const cam = { zoom: lerp(0.4685, 0.72, e), cx: lerp(1600, 2140, e), cy: lerp(1000, 980, e) };
    ctx.save();
    ctx.globalAlpha = a * e;
    ctx.save();
    ctx.shadowColor = 'rgba(0,0,0,.7)';
    ctx.shadowBlur = 60;
    fillRound(b.x, b.y, b.w, b.h, 16, '#0b0f14');
    ctx.restore();
    const map = shot(IMG.workspaceDiff, b, cam, 16, { stroke: 'rgba(88,166,255,.35)' });
    ctx.restore();
    const sp = (rect, on, off, color) =>
      spotlight(map, rect, {
        alpha: a * e * clamp01((t - on) / 0.4) * clamp01((off - t) / 0.4),
        color, pulse: 0.5 + 0.5 * Math.sin(t * 3),
      });
    sp({ x: 1790, y: 330, w: 880, h: 640 }, B.panels.diff[0], 9.5, C.green);
    sp({ x: 2688, y: 300, w: 500, h: 330 }, B.panels.diff[1], 9.5, C.amber);
    text('git 改动直接看 diff', 210, 96, { size: 30, weight: 700, alpha: a * e });
  } else {
    text('文件、统计、轨迹，都在同一页', 92, 158, { size: 46, weight: 700, alpha: a });
    text('FILES · STATS · TRAJECTORY', 92, 196, {
      size: 19, weight: 500, color: C.dim, spacing: '4px', alpha: a,
    });
  }

  subtitle(
    zoom > 0.5 ? '未暂存、已暂存、未跟踪文件都能看' : '工作区面板、使用统计、轨迹视图',
    zoom > 0.5 ? 'Staged, unstaged and untracked — all of it' : 'Workspace panel, usage stats, trajectory view',
    sceneAlpha(p, 9.5, 0.95, 1.1) * clamp01((t - 0.7) / 0.6),
  );
}

/** Scene 8 — mobile */
function sceneMobile(p, t) {
  const a = sceneAlpha(p, 5.5, 0.4, 0.45);
  const img = IMG.mobile;
  const pw = 372, ph = pw / (img.width / img.height);
  const float = Math.sin(t * 1.1) * 9;
  const px = 1246, py = (H - ph) / 2 + float + 6;
  const e1 = easeOut(clamp01(t / 0.8));

  ctx.save();
  ctx.globalAlpha = a;
  // phone body
  ctx.save();
  ctx.shadowColor = 'rgba(0,0,0,.75)';
  ctx.shadowBlur = 54;
  ctx.shadowOffsetY = 20;
  fillRound(px - 14, py - 14, pw + 28, ph + 28, 44, '#1b2028');
  ctx.restore();
  strokeRound(px - 14, py - 14, pw + 28, ph + 28, 44, '#3d444d', 2);
  shot(img, { x: px, y: py + (1 - e1) * 30, w: pw, h: ph }, null, 34, { stroke: null });
  // notch
  fillRound(px + pw / 2 - 52, py + 8, 104, 22, 11, '#0b0f14');
  ctx.restore();

  const lx = 150;
  text('移动优先的 PWA', lx, 420, { size: 52, weight: 700, alpha: a * clamp01((t - 0.5) / 0.6) });
  text('MOBILE-FIRST PWA', lx, 462, {
    size: 19, weight: 500, color: C.dim, spacing: '4px', alpha: a * clamp01((t - 0.62) / 0.6),
  });
  const bullets = ['抽屉导航，触屏优化', '底部操作面板', '深浅主题，避开状态栏安全区'];
  bullets.forEach((b, i) => {
    const lp = clamp01((t - B.mobile.bullets[i]) / 0.55);
    if (lp <= 0) return;
    ctx.save();
    ctx.globalAlpha = a * lp;
    fillRound(lx, 524 + i * 74 + (1 - easeOut(lp)) * 14, 10, 10, 5, C.accent);
    text(b, lx + 30, 534 + i * 74 + (1 - easeOut(lp)) * 14, { size: 27, weight: 500, color: C.fg });
    ctx.restore();
  });

  subtitle('外面也能跟进会话', 'Follow your sessions from anywhere', sceneAlpha(p, 5.5, 0.7, 0.8) * clamp01((t - 1.2) / 0.6));
}

/** Scene 9 — outro */
function sceneOutro(p, t) {
  const a = sceneAlpha(p, 8, 0.4, 0.9);
  const cmd = 'go build -o agentdeck .';
  const cw = 880, cx = (W - cw) / 2, cy = 208;
  const e1 = easeOut(clamp01(t / 0.7));

  ctx.save();
  ctx.globalAlpha = a;
  card(cx, cy + (1 - e1) * 30, cw, 216, 16, { fill: '#0c1017', stroke: C.border, shadow: 40 });
  fillRound(cx, cy + (1 - e1) * 30, cw, 40, 16, '#161b22');
  ctx.fillStyle = '#161b22';
  ctx.fillRect(cx, cy + 26 + (1 - e1) * 30, cw, 14);
  ['#f85149', '#d29922', '#3fb950'].forEach((c, i) => {
    ctx.beginPath();
    ctx.arc(cx + 24 + i * 20, cy + 20 + (1 - e1) * 30, 6, 0, Math.PI * 2);
    ctx.fillStyle = c;
    ctx.fill();
  });
  const ly = cy + 104 + (1 - e1) * 30;
  text('$', cx + 30, ly, { size: 26, mono: true, color: C.green });
  const chars = Math.floor(cmd.length * easeOut(clamp01((t - B.outro.typed) / 1.5)));
  text(cmd.slice(0, chars), cx + 60, ly, { size: 26, mono: true, color: C.fg });
  if (chars >= cmd.length && Math.floor(t * 2) % 2 === 0) {
    fillRound(cx + 60 + ctx.measureText(cmd).width + 4, ly - 22, 14, 26, 2, C.fg);
  }
  const okP = clamp01((t - B.outro.ok) / 0.6);
  ctx.save();
  ctx.globalAlpha = a * okP;
  fillRound(cx + 30, ly + 44, 26, 26, 13, 'rgba(63,185,80,.18)');
  text('✓', cx + 43, ly + 64, { size: 18, weight: 700, color: C.green, align: 'center' });
  text('单文件二进制 · 前端已内嵌 · 无需 Node 构建', cx + 70, ly + 64, { size: 24, color: C.fg });
  ctx.restore();

  const bp = clamp01((t - B.outro.big) / 0.8);
  ctx.save();
  ctx.globalAlpha = a * bp;
  const off = (1 - easeOut(bp)) * 26;
  text('一个二进制，五个引擎。', W / 2, 590 + off, {
    size: 72, weight: 700, align: 'center', color: C.fg, spacing: '-1px',
  });
  text('ONE BINARY · FIVE ENGINES', W / 2, 638 + off, {
    size: 21, weight: 500, align: 'center', color: C.dim, spacing: '5px',
  });
  ctx.restore();

  const wp = clamp01((t - B.outro.mark) / 0.9);
  ctx.save();
  ctx.globalAlpha = a * wp;
  const off2 = (1 - easeOut(wp)) * 18;
  text('AgentDeck', W / 2, 748 + off2, { size: 46, weight: 700, align: 'center', color: C.fg, spacing: '-1px' });
  text('在浏览器里远程驾驭 Claude Code / Codex / opencode / MiniMax Code / Qoder', W / 2, 794 + off2, {
    size: 22, align: 'center', color: C.muted,
  });
  ctx.restore();

  subtitle('跑起来，打开浏览器就能用', 'Run it, open a browser, get to work', sceneAlpha(p, 8, 1.12, 1.6) * clamp01((t - B.outro.sub) / 0.6));

  // final fade to black
  const fade = clamp01((t - B.outro.fade) / 0.9);
  if (fade > 0) {
    ctx.globalAlpha = 1;
    ctx.fillStyle = `rgba(3,5,8,${fade})`;
    ctx.fillRect(0, 0, W, H);
  }
  ctx.restore();
}

// -------------------------------------------------------------------- driver

// Scene list. Durations are derived from the timeline so retiming means editing
// src/timeline.js only.
const SCENES = [
  { id: 'title', label: '', draw: sceneTitle },
  { id: 'problem', label: '问题', draw: sceneProblem },
  { id: 'engines', label: '五引擎', draw: sceneEngines },
  { id: 'chat', label: '实况对话', draw: sceneChat },
  { id: 'peers', label: '跨会话消息', draw: scenePeers },
  { id: 'group', label: '群聊 · 提问', draw: sceneGroup },
  { id: 'panels', label: '三个视图', draw: scenePanels },
  { id: 'mobile', label: '移动端', draw: sceneMobile },
  { id: 'outro', label: '收尾', draw: sceneOutro },
].map((s) => ({ ...s, dur: span(s.id)[1] - SC[s.id] }));

const STARTS = SCENES.map((s) => SC[s.id]);
const TOTAL = DUR;

// Scene boundaries crossfade instead of dipping to black. No scene paints an
// opaque full-frame background (only cards, phone bodies and screenshots), so
// painting the outgoing scene first and the incoming one on top is a real
// crossfade with no offscreen compositing. XFADE lives in src/timeline.js.

function render(t) {
  t = Math.max(0, Math.min(TOTAL, t));
  let idx = 0;
  for (let i = 0; i < SCENES.length; i++) {
    if (t - STARTS[i] + XFADE >= 0) idx = i;
  }
  const main = SCENES[idx];
  const mlocal = t - STARTS[idx] + PRE(main.id);

  backdrop(t);
  // outgoing scene: still on screen for XFADE after the incoming one starts
  if (idx > 0 && mlocal < XFADE) {
    const prev = SCENES[idx - 1];
    const plocal = t - STARTS[idx - 1];
    if (plocal >= 0) prev.draw(plocal / prev.dur, plocal);
  }
  // scenes arrive rather than just fade: a short scale-and-rise settle
  const se = easeOut(clamp01(mlocal / 0.75));
  ctx.save();
  ctx.translate(W / 2, H / 2 + (1 - se) * 24);
  const sc = lerp(1.022, 1, se);
  ctx.scale(sc, sc);
  ctx.translate(-W / 2, -H / 2);
  main.draw(mlocal / main.dur, mlocal);
  ctx.restore();
  chrome(idx, main.label, t);
}

// text-check hooks
window.__textLog = null;
window.__logText = (on) => { LOG_TEXT = on ? [] : null; window.__textLog = LOG_TEXT; return LOG_TEXT; };
window.__duration = TOTAL;
window.__sc = SC;
window.__xfade = XFADE;
window.__fps = FPS;
window.__size = { w: W, h: H };
window.__seek = render;
window.__ready = loadImages().then(() => {
  render(0);
  return true;
});
