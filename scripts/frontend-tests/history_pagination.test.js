// 历史分页的回归测试。
//
// 服务端只通过 SSE 回放「最新一页」窗口（replay_end 带 {start,total}），更早的
// 事件由前端点「加载更多」经 GET /history?before=&limit= 按页拉取。这里把
// app.js 里分页相关函数摘出来跑，钉住：窗口起点进 histBase、剩余条数 = histBase
// + histRenderedFrom、缓冲还有未渲染事件时先渲染不请求、缓冲耗尽才发请求、
// 请求回的页 unshift 进缓冲并前插 DOM、拉空后按钮隐藏。
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
const HISTORY_PAGE = 50;
const state = {
  current: { id: 's1' },
  historyBuf: [], histBase: 0, histRenderedFrom: 0, loadingMore: false,
  replaying: true, renderingHistory: false, nearBottom: true,
  stats: null,
};
const rendered = [];
const errors = [];
const requests = [];
function safeRenderEvent(ev, box) { rendered.push(ev.id); if (box && box.appendChild) box.appendChild(document.createTextNode(String(ev.id))); }
function taskFoldAll() {}
function syncTaskPanel() {}
function trajReset() {}
function trajSync() {}
function pendingItems() { return []; }
function isRunning() { return false; }
function flushPending() {}
function scrollBottom() {}
function updateStatsUI() {}
function refreshPinnedPrompt() {}
function setErrorBanner(m) { errors.push(m); }
let pages = {};
let failNext = false;
async function api(url) {
  requests.push(url);
  if (failNext) { failNext = false; throw new Error('boom'); }
  const m = url.match(/before=(\\d+)/);
  const before = m ? Number(m[1]) : 0;
  const page = pages[before];
  if (!page) return { events: [], from: 0 };
  return page;
}
const $ = (sel) => document.querySelector(sel);
function mkEvents(from, n) { const a = []; for (let i = 0; i < n; i++) a.push({ id: from + i, kind: 'stream' }); return a; }
${grabFn('flushHistory')}
${grabFn('renderPrevHistoryPage')}
${grabFn('loadMoreHistory')}
${grabFn('updateLoadMoreBtn')}
${grabFn('freshStats')}
${grabFn('applyReplayStats')}
window.__x = {
  state, rendered, errors, requests,
  flushHistory, renderPrevHistoryPage, loadMoreHistory, updateLoadMoreBtn,
  freshStats, applyReplayStats,
  setPage(before, events, from) { pages[before] = { events, from }; },
  setFail() { failNext = true; },
  mkEvents,
  reset() {
    state.current = { id: 's1' };
    state.historyBuf = []; state.histBase = 0; state.histRenderedFrom = 0;
    state.loadingMore = false; state.replaying = true; state.renderingHistory = false;
    state.stats = null;
    rendered.length = 0; errors.length = 0; requests.length = 0; pages = {};
    document.getElementById('messages').innerHTML = '';
  },
};
`;

const dom = new JSDOM('<!doctype html><html><body><div id="messages"></div></body></html>',
  { runScripts: 'outside-only', pretendToBeVisual: true });
dom.window.eval(setup);
const x = dom.window.__x;

const tests = [];
const test = (name, fn) => tests.push({ name, fn });
const tick = () => new Promise((r) => setTimeout(r, 0));
const btnText = () => {
  const w = dom.window.document.getElementById('load-more-wrap');
  return w && !w.classList.contains('hidden') ? w.firstChild.textContent : null;
};

test('flushHistory 记录窗口起点并把整窗渲染出来', () => {
  x.reset();
  x.state.historyBuf = x.mkEvents(0, 30);
  x.flushHistory({ start: 100, total: 130 });
  assert.strictEqual(x.state.histBase, 100);
  assert.strictEqual(x.state.histRenderedFrom, 0);
  assert.strictEqual(x.rendered.length, 30);
  assert.strictEqual(btnText(), '加载更多（还有 100 条）');
});

test('缓冲内还有未渲染事件时，先渲染不发请求', async () => {
  x.reset();
  x.state.historyBuf = x.mkEvents(0, 30);
  x.state.histBase = 5;
  x.state.histRenderedFrom = 20; // 0..19 未渲染
  await x.loadMoreHistory();
  assert.strictEqual(x.requests.length, 0, '不该请求服务端');
  assert.strictEqual(x.state.histRenderedFrom, 0);
  assert.strictEqual(x.rendered.length, 20);
  assert.strictEqual(btnText(), '加载更多（还有 5 条）');
});

test('缓冲耗尽时按 before=histBase 拉上一页并前插', async () => {
  x.reset();
  x.state.historyBuf = x.mkEvents(0, 30);
  x.state.histBase = 100;
  x.setPage(100, x.mkEvents(-50, 50), 50);
  await x.loadMoreHistory();
  assert.strictEqual(x.requests.length, 1);
  assert.ok(x.requests[0].includes('before=100'), x.requests[0]);
  assert.strictEqual(x.state.histBase, 50);
  assert.strictEqual(x.state.historyBuf.length, 80);
  assert.strictEqual(x.state.historyBuf[0].id, -50);
  assert.strictEqual(x.rendered.length, 50);
  assert.strictEqual(btnText(), '加载更多（还有 50 条）');
});

test('拉到最早一页后按钮隐藏', async () => {
  x.reset();
  x.state.historyBuf = x.mkEvents(0, 30);
  x.state.histBase = 50;
  x.setPage(50, x.mkEvents(-50, 50), 0);
  await x.loadMoreHistory();
  assert.strictEqual(x.state.histBase, 0);
  assert.strictEqual(btnText(), null);
});

test('服务端返回空页时收敛为没有更早历史', async () => {
  x.reset();
  x.state.historyBuf = x.mkEvents(0, 30);
  x.state.histBase = 10; // 服务端却说没有更早的了
  await x.loadMoreHistory();
  assert.strictEqual(x.state.histBase, 0);
  assert.strictEqual(btnText(), null);
});

test('请求失败提示错误且不清空缓冲', async () => {
  x.reset();
  x.state.historyBuf = x.mkEvents(0, 30);
  x.state.histBase = 100;
  x.setFail();
  await x.loadMoreHistory();
  assert.strictEqual(x.state.histBase, 100, '失败后起点不变');
  assert.strictEqual(x.state.historyBuf.length, 30);
  assert.strictEqual(x.errors.length, 1);
  assert.ok(/加载更早的历史失败/.test(x.errors[0]));
});

test('applyReplayStats 把窗口前的统计并进 state.stats', () => {
  x.reset();
  x.state.stats = x.freshStats();
  // 窗口内已 track 的 1 轮
  x.state.stats.turns = 1;
  x.state.stats.steps = 2;
  x.state.stats.firstTs = 5000;
  x.state.stats.lastTs = 9000;
  x.applyReplayStats({ turns: 10, steps: 20, first_ts: 1000, last_ts: 8000, llm_ms: 100, tool_ms: 50, llm_est_ms: 7, tool_est_ms: 3 });
  assert.strictEqual(x.state.stats.turns, 11);
  assert.strictEqual(x.state.stats.steps, 22);
  assert.strictEqual(x.state.stats.firstTs, 1000, '取更早的 first_ts');
  assert.strictEqual(x.state.stats.lastTs, 9000, '保留更晚的 last_ts');
  assert.strictEqual(x.state.stats.llmMs, 100);
  assert.strictEqual(x.state.stats.toolMs, 50);
  assert.strictEqual(x.state.stats.llmEstMs, 7);
  assert.strictEqual(x.state.stats.toolEstMs, 3);
  // 没有 stats（老服务端 / 空会话）时安全 no-op
  x.applyReplayStats(undefined);
  assert.strictEqual(x.state.stats.turns, 11);
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
  console.log('history_pagination: ' + (tests.length - failed) + '/' + tests.length + ' 通过');
  if (failed) process.exit(1);
})();
