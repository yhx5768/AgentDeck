// 运行态徽标的「已等待 mm:ss」计时回归测试。
//
// 模型在大上下文上要很久才吐第一个 token（实测某会话 prompt ≈65 万 token，
// 首 token 60–300 秒），静默期里只有一个静止的「● 运行中」，跟卡死看不出区别。
// 这里钉住：计时格式化、running 时把计时写进徽标、idle 时清掉计时器与文本。
//
// 计时器被替换成计数器，避免 jsdom 的真实 interval 把测试进程挂住。
const fs = require('fs');
const path = require('path');
const assert = require('assert');
const { JSDOM } = require('jsdom');

const WEB = path.resolve(__dirname, '../../web');
const src = fs.readFileSync(path.join(WEB, 'app.js'), 'utf8');

function grabFn(name) {
  let s = src.indexOf('function ' + name + '(');
  if (s < 0) throw new Error('找不到函数 ' + name);
  if (src.slice(Math.max(0, s - 6), s) === 'async ') s -= 6;
  let params = 0, paramsEnd = -1;
  for (let i = src.indexOf('(', s); i < src.length; i++) {
    if (src[i] === '(') params++;
    else if (src[i] === ')') { params--; if (!params) { paramsEnd = i; break; } }
  }
  const b = src.indexOf('{', paramsEnd);
  let d = 0, q = null;
  for (let i = b; i < src.length; i++) {
    const c = src[i], p = src[i - 1];
    if (q) { if (c === q && p !== '\\') q = null; continue; }
    if (c === '"' || c === "'" || c === '`') { q = c; continue; }
    if (c === '{') d++;
    else if (c === '}') { d--; if (!d) return src.slice(s, i + 1); }
  }
  throw new Error(name + ' 括号不配对');
}

const setup = `
const state = { current: { id: 's1', status: { running: false, queued: 0 } } };
const els = {};
for (const id of ['status-badge', 'btn-stop', 'queue-hint', 'composer-spinner']) {
  els[id] = document.createElement('div');
}
function $(sel) { return els[sel.replace(/^#/, '')] || null; }
let timers = 0;
let cleared = 0;
function setInterval() { timers++; return timers; }
function clearInterval() { cleared++; }
let now = 1000000;
Date.now = () => now;
let turnTick = null;
let turnStartedAt = 0;
${grabFn('fmtWait')}
${grabFn('paintTurnTick')}
${grabFn('startTurnTick')}
${grabFn('stopTurnTick')}
${grabFn('updateStatusUI')}
window.__x = {
  state, els,
  fmtWait, paintTurnTick, startTurnTick, stopTurnTick, updateStatusUI,
  counters: () => ({ timers, cleared }),
  advance(ms) { now += ms; },
  reset() { stopTurnTick(); timers = 0; cleared = 0; now = 1000000; els['status-badge'].textContent = ''; },
};
`;

const dom = new JSDOM('<!doctype html><html><body></body></html>',
  { runScripts: 'outside-only', pretendToBeVisual: true });
dom.window.eval(setup);
const x = dom.window.__x;

const tests = [];
const test = (name, fn) => tests.push({ name, fn });
const badge = () => x.els['status-badge'].textContent;
// JSDOM 的 window 里创建的对象和 Node 主 realm 的原型不同，先转普通值再比。
const plain = (v) => JSON.parse(JSON.stringify(v));

test('fmtWait 按秒/分秒格式化', () => {
  assert.strictEqual(x.fmtWait(0), '0s');
  assert.strictEqual(x.fmtWait(59000), '59s');
  assert.strictEqual(x.fmtWait(60000), '1:00');
  assert.strictEqual(x.fmtWait(125000), '2:05');
  assert.strictEqual(x.fmtWait(-5), '0s', '负数兜底为 0');
});

test('running 时徽标带「已等待」，且只起一个计时器', () => {
  x.reset();
  x.updateStatusUI({ running: true, queued: 0 });
  assert.strictEqual(badge(), '● 运行中 · 0s');
  assert.deepStrictEqual(plain(x.counters()), { timers: 1, cleared: 0 });
  // 再次进入 running（轮询/重复 status 事件）不应再起计时器
  x.updateStatusUI({ running: true, queued: 0 });
  assert.deepStrictEqual(plain(x.counters()), { timers: 1, cleared: 0 });
  // 计时由每秒的 tick 推进
  x.advance(65000);
  x.paintTurnTick();
  assert.strictEqual(badge(), '● 运行中 · 1:05');
});

test('idle 时清掉计时器与徽标文本', () => {
  x.reset();
  x.updateStatusUI({ running: true, queued: 0 });
  x.updateStatusUI({ running: false, queued: 0 });
  assert.strictEqual(badge(), '');
  assert.strictEqual(x.counters().cleared, 1);
});

test('切会话时等待计时归零（换会话不带上一个会话的时长）', () => {
  assert.ok(/stopTurnTick\(\);\s*\/\/ 换会话时等待计时归零[\s\S]{0,80}updateStatusUI\(s\.status\)/.test(src),
    'openSession 应在 updateStatusUI 前 stopTurnTick');
});

(async () => {
  let failed = 0;
  for (const t of tests) {
    try {
      await t.fn();
      console.log('  ok  ' + t.name);
    } catch (e) {
      failed++;
      console.log('  FAIL ' + t.name + '  →  ' + (e && e.message));
    }
  }
  console.log('turn_wait: ' + (tests.length - failed) + '/' + tests.length + ' 通过');
  if (failed) process.exit(1);
})();
