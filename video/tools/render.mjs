// Frame stepper: seek the film to t, screenshot the canvas, pipe PNGs straight
// into ffmpeg. No frame ever touches the disk, so a 4K supersampled render
// costs no gigabytes.
//
// Usage:
//   node tools/render.mjs                      # whole film → out/silent.mp4
//   node tools/render.mjs --dsf 2 --crf 16
//   node tools/render.mjs --still 12.5 -o out/frame-12.5.png
//   node tools/render.mjs --range 27:40        # a slice, for iterating
import { chromium } from 'playwright';
import { spawn } from 'node:child_process';
import { mkdirSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const ROOT = resolve(HERE, '..');

// Accept both `-o x` and `--o x` (and `--range` / `-range`): the flags grew
// organically and a single-dash/double-dash mismatch silently falls back to the
// default, which once made three parallel slices clobber one file.
function arg(name, def) {
  for (const form of ['--' + name, '-' + name]) {
    const i = process.argv.indexOf(form);
    if (i !== -1) return process.argv[i + 1];
  }
  return def;
}
const has = (name) => process.argv.includes('--' + name) || process.argv.includes('-' + name);

const DSF = Number(arg('dsf', 1.5));
const CRF = arg('crf', '16');
const PRESET = arg('preset', 'slow');
const QUALITY = Number(arg('quality', 97));
const STILL = arg('still', null);
const STILLS = arg('stills', null);
const OUT = arg('o', join(ROOT, 'out', 'silent.mp4'));
const RANGE = arg('range', null);
// "i/N": render only the i-th of N equal slices, with an IDR forced on its first
// frame so the slices can be concatenated with -c copy (see render-parallel.sh).
const CHUNK = arg('chunk', null);
mkdirSync(join(ROOT, 'out'), { recursive: true });

const browser = await chromium.launch({ channel: 'chrome' });
const page = await browser.newPage({
  viewport: { width: 1920, height: 1080 },
  deviceScaleFactor: DSF,
});
page.on('pageerror', (e) => console.error('[pageerror]', e.message));
await page.goto(pathToFileURL(join(ROOT, 'src', 'film.html')).href);
await page.waitForFunction('window.__ready !== undefined', null, { timeout: 30000 });
await page.evaluate(() => window.__ready);

const FPS = await page.evaluate(() => window.__fps);
const DUR = await page.evaluate(() => window.__duration);
console.log(`film: ${DUR}s @ ${FPS}fps, dsf=${DSF}`);

if (STILL !== null || STILLS) {
  const times = STILLS ? STILLS.split(',').map(Number) : [Number(STILL)];
  for (const t of times) {
    await page.evaluate((tt) => window.__seek(tt), t);
    await page.waitForTimeout(90);
    const name = `still-${String(t).replace('.', '_')}.png`;
    await page.screenshot({ path: join(ROOT, 'out', name) });
    console.log('  ✓', name);
  }
  await browser.close();
  process.exit(0);
}

let first = 0;
let last = Math.round(DUR * FPS);
if (RANGE) {
  const [a, b] = RANGE.split(':').map(Number);
  first = Math.round(a * FPS);
  last = Math.round(b * FPS);
}
if (CHUNK) {
  const [i, n] = CHUNK.split('/').map(Number);
  const total = Math.round(DUR * FPS);
  first = Math.round((total * i) / n);
  last = Math.round((total * (i + 1)) / n);
}
const count = last - first;
console.log(`  frames ${first}..${last} (${count})`);

const ff = spawn(
  'ffmpeg',
  [
    '-y', '-hide_banner', '-loglevel', 'error',
    // JPEG rather than PNG: the browser's PNG encoder costs ~1.5s per 4K frame,
    // which dominates the render. q97 mjpeg is visually transparent here and
    // x264 re-encodes it anyway.
    '-f', 'image2pipe', '-vcodec', 'mjpeg', '-framerate', String(FPS), '-i', '-',
    '-an',
    '-c:v', 'libx264', '-preset', PRESET, '-crf', CRF,
    // An IDR on the slice's first frame: without it a -c copy concat would
    // start each slice mid-GOP and corrupt everything up to the next keyframe.
    '-force_key_frames', 'expr:eq(n,0)',
    '-pix_fmt', 'yuv420p', '-movflags', '+faststart',
    '-vf', 'scale=1920:1080:flags=lanczos',
    OUT,
  ],
  { stdio: ['pipe', 'inherit', 'inherit'] },
);
ff.on('exit', (code) => { if (code) console.error('ffmpeg exited', code); });

const started = Date.now();
for (let i = first; i < last; i++) {
  const t = i / FPS;
  await page.evaluate((tt) => window.__seek(tt), t);
  const buf = await page.screenshot({ type: 'jpeg', quality: QUALITY });
  if (!ff.stdin.write(buf)) {
    await new Promise((r) => ff.stdin.once('drain', r));
  }
  const done = i - first + 1;
  if (done % 30 === 0 || done === count) {
    const el = (Date.now() - started) / 1000;
    const eta = (el / done) * (count - done);
    process.stdout.write(
      `\r  ${done}/${count} frames  ${(el / done * 1000).toFixed(0)}ms/f  eta ${(eta / 60).toFixed(1)}min   `,
    );
  }
}
ff.stdin.end();
await new Promise((r) => ff.on('close', r));
await browser.close();
console.log(`\n→ ${OUT}  (${((Date.now() - started) / 1000 / 60).toFixed(1)} min)`);
