// Audit on-screen text: safe area, minimum size, and boxes that collide.
//
// The film draws hundreds of labels a second and jsdom cannot see any of it, so
// the only way to catch a caption running off the frame or a chip landing on top
// of a caption is to ask the real renderer. film.js records every legible string
// with its box when logging is on; this samples the timeline and reports.
//
// usage: node tools/text-check.mjs [step=0.5]
import { chromium } from 'playwright';
import { dirname, join } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');
const STEP = Number(process.argv[2] || 0.5);
const SAFE = { x0: 40, y0: 28, x1: 1880, y1: 1062 };
const MIN_SIZE = 16;

const browser = await chromium.launch({ channel: 'chrome' });
const page = await browser.newPage({ viewport: { width: 1920, height: 1080 }, deviceScaleFactor: 1 });
await page.goto(pathToFileURL(join(ROOT, 'src', 'film.html')).href);
await page.waitForFunction('window.__ready !== undefined');
await page.evaluate(() => window.__ready);

const DUR = await page.evaluate(() => window.__duration);
const SC = await page.evaluate(() => window.__sc);
const XF = await page.evaluate(() => window.__xfade);
// Two scenes are on screen at once during a cross-fade, so their text overlaps
// by construction — that is the transition working, not a layout bug.
// a scene's pre-roll starts XFADE before its entry in SC, and the outgoing scene
// stays on screen through it
const inCrossfade = (t) => Object.values(SC).some((s) => t > s - XF - 0.2 && t < s + 0.2);
const out = { offSafe: [], tiny: [], collide: [] };
for (let t = 0; t < DUR; t += STEP) {
  const boxes = await page.evaluate((tt) => {
    window.__logText(true);
    window.__seek(tt);
    const b = window.__textLog;
    window.__logText(false);
    return b;
  }, t);
  for (const b of boxes) {
    if (b.x < SAFE.x0 || b.y < SAFE.y0 || b.x + b.w > SAFE.x1 || b.y + b.h > SAFE.y1) {
      out.offSafe.push({ t: +t.toFixed(1), s: b.s, box: [Math.round(b.x), Math.round(b.y), Math.round(b.w)] });
    }
    if (b.size < MIN_SIZE) out.tiny.push({ t: +t.toFixed(1), s: b.s, size: b.size });
  }
  // collisions: ignore pairs that share a line (a rich caption's segments)
  for (let i = 0; i < boxes.length && !inCrossfade(t); i++) {
    for (let j = i + 1; j < boxes.length; j++) {
      const a = boxes[i], b = boxes[j];
      const ox = Math.min(a.x + a.w, b.x + b.w) - Math.max(a.x, b.x);
      const oy = Math.min(a.y + a.h, b.y + b.h) - Math.max(a.y, b.y);
      if (ox <= 2 || oy <= 2) continue;
      const area = ox * oy;
      if (area < 260) continue;
      out.collide.push({ t: +t.toFixed(1), a: a.s.slice(0, 26), b: b.s.slice(0, 26), area: Math.round(area) });
    }
  }
}
await browser.close();

const uniq = (rows, key) => {
  const m = new Map();
  for (const r of rows) if (!m.has(key(r))) m.set(key(r), r);
  return [...m.values()];
};
const report = (name, rows, fmt) => {
  const u = uniq(rows, (r) => (r.s || '') + (r.a || '') + (r.b || ''));
  console.log(`${name}: ${rows.length} 处${u.length !== rows.length ? `（去重后 ${u.length}）` : ''}`);
  for (const r of u.slice(0, 12)) console.log('   ', fmt(r));
};
report('超出安全区', out.offSafe, (r) => `t=${r.t}s  "${r.s}"  box=${r.box}`);
report('字号 < 16px', out.tiny, (r) => `t=${r.t}s  "${r.s}"  ${r.size}px`);
report('文字重叠', out.collide, (r) => `t=${r.t}s  "${r.a}"  ×  "${r.b}"  重叠 ${r.area}px²`);
if (!out.offSafe.length && !out.tiny.length && !out.collide.length) console.log('  全部通过');
