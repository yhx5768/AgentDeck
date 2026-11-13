// 看板抽屉「回复中 / 已回复」状态回归。
//
// 服务端会自动把看板来源 turn 的最终回复写回卡片时间线；前端不比对时钟，
// 而是发送前记下最新「主 Agent 回复」的指纹，看板刷新后指纹变化即判定已回复，
// 并直接把抽屉切到时间线、高亮最新回复。
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

const HTML = `<!doctype html><html><body>
  <div id="board-drawer-timeline"></div>
  <div id="board-drawer-filters"></div>
  <span id="board-drawer-timeline-count"></span>
</body></html>`;

const dom = new JSDOM(HTML, { runScripts: 'outside-only', pretendToBeVisual: true });
const w = dom.window;

w.eval(`
const $ = (sel) => document.querySelector(sel);
const state = { current: { id: 'main-1' }, boardSelectedId: 'T-1' };
let boardReplyWaits = {};
let boardTimelineFilter = 'all';
const statuses = [];
const panes = [];
function boardComposerStatus(text, kind = '') { statuses.push({ text, kind }); }
function boardShowPane(name) { panes.push(name); }
function pendingItems() { return []; }
function boardMuted(text) { const el = document.createElement('div'); el.textContent = text; return el; }
function md(text) { return text; }
${grabFn('boardTaskEvents')}
${grabFn('boardEventAvatar')}
${grabFn('boardTimelineRow')}
${grabFn('boardRenderTimeline')}
${grabFn('boardReplyEvents')}
${grabFn('boardReplyKey')}
${grabFn('boardReplyKeys')}
${grabFn('boardReplyMarkWait')}
${grabFn('boardReplyClearWait')}
${grabFn('boardReplySync')}
${grabFn('boardReplyReveal')}
window.__x = {
  state, statuses, panes,
  get waits() { return boardReplyWaits; },
  get filter() { return boardTimelineFilter; },
  boardReplyKeys, boardReplyMarkWait, boardReplyClearWait,
  boardReplySync, boardReplyReveal, boardRenderTimeline,
  timeline() { return document.querySelector('#board-drawer-timeline'); },
  reset() {
    boardReplyWaits = {};
    boardTimelineFilter = 'all';
    statuses.length = 0;
    panes.length = 0;
    state.boardSelectedId = 'T-1';
    document.querySelector('#board-drawer-timeline').innerHTML = '';
  },
};
`);

const x = w.__x;
const tests = [];
const test = (name, fn) => tests.push({ name, fn });

test('发送前已有旧回复：刷新到同一份内容仍是「回复中」；新回复到达切「已回复」并展开时间线', () => {
  x.reset();
  const task = {
    id: 'T-1',
    timeline: [
      { at: '2026-11-12 10:00:00', kind: '主 Agent 回复', text: '旧回复' },
      { at: '2026-11-12 09:59:00', kind: '用户消息', text: '上一个问题' },
    ],
  };
  x.boardReplyMarkWait('T-1', x.boardReplyKeys(task));
  assert.strictEqual(x.boardReplySync(task), false, '旧回复不能被当成本轮回复');
  assert.ok(x.statuses.some((s) => s.text.includes('回复中')), '等待时显示回复中');
  assert.strictEqual(x.panes.length, 0);

  task.timeline.unshift({
    at: '2026-11-12 10:05:00', kind: '主 Agent 回复',
    text: '## 新回复\n\n- 已完成', data: { auto: true },
  });
  assert.strictEqual(x.boardReplySync(task), true);
  assert.ok(x.statuses.some((s) => s.text.includes('已回复') && s.kind === 'ok'));
  assert.strictEqual(x.panes[x.panes.length - 1], 'timeline', '检测到回复后直接展开时间线');
  assert.strictEqual(x.filter, 'all', '筛选不能把新回复藏掉');
  const fresh = x.timeline().querySelector('.board-event-fresh');
  assert.ok(fresh, '最新回复要高亮');
  assert.ok(fresh.textContent.includes('新回复'), fresh.textContent);
  assert.strictEqual(x.waits['T-1'], undefined, '已回复后清掉等待状态');
});

test('自动回写按 turn_id 区分：同秒同文案的新回复也能识别，旧回复不会误报', () => {
  x.reset();
  const task = {
    id: 'T-1',
    timeline: [
      { at: '2026-11-12 10:00:00', kind: '主 Agent 回复', text: '完成', data: { turn_id: 's#1' } },
      { at: '2026-11-12 10:00:00', kind: '主 Agent 回复', text: '上一条', data: { turn_id: 's#0' } },
    ],
  };
  x.boardReplyMarkWait('T-1', x.boardReplyKeys(task));
  assert.strictEqual(x.boardReplySync(task), false, '已有的两条回复都不能当成本轮回复');
  task.timeline.unshift({
    at: '2026-11-12 10:00:00', kind: '主 Agent 回复', text: '完成', data: { turn_id: 's#2' },
  });
  assert.strictEqual(x.boardReplySync(task), true, 'turn_id 不同的回复即使 at/正文相同也算新回复');
});

test('没有旧回复时，空指纹也能判定第一条自动回复', () => {
  x.reset();
  const task = { id: 'T-1', timeline: [{ kind: '用户消息', at: '2026-11-12 10:00:00', text: '问' }] };
  x.boardReplyMarkWait('T-1', x.boardReplyKeys(task));
  assert.strictEqual(x.boardReplySync(task), false);
  task.timeline.unshift({ at: '2026-11-12 10:01:00', kind: '主 Agent 回复', text: '答' });
  assert.strictEqual(x.boardReplySync(task), true);
});

test('多张卡的等待状态互不干扰', () => {
  x.reset();
  const a = { id: 'T-A', timeline: [] };
  const b = { id: 'T-B', timeline: [] };
  x.boardReplyMarkWait('T-A', x.boardReplyKeys(a));
  x.boardReplyMarkWait('T-B', x.boardReplyKeys(b));
  b.timeline.unshift({ at: '2026-11-12 10:02:00', kind: '主 Agent 回复', text: 'B 的回复' });
  assert.strictEqual(x.boardReplySync(b), true);
  assert.strictEqual(x.boardReplySync(a), false, 'A 仍在等待');
  assert.ok(x.waits['T-A'], 'A 的等待状态保留');
});

test('没有等待状态时不改 composer 状态；取消等待后也不再提示', () => {
  x.reset();
  const task = { id: 'T-1', timeline: [{ at: '2026-11-12 10:00:00', kind: '主 Agent 回复', text: 'x' }] };
  assert.strictEqual(x.boardReplySync(task), false);
  assert.strictEqual(x.statuses.length, 0);
  x.boardReplyMarkWait('T-1', '');
  x.boardReplyClearWait('T-1');
  x.boardReplySync(task);
  assert.strictEqual(x.statuses.length, 0, '取消后不再显示回复中');
});

test('时间线行带 data-kind，供展开时定位主 Agent 回复', () => {
  x.reset();
  x.boardRenderTimeline({ id: 'T-1', timeline: [
    { at: '2026-11-12 10:00:00', kind: '主 Agent 回复', text: '答' },
    { at: '2026-11-12 09:59:00', kind: '用户消息', text: '问' },
  ] });
  const rows = [...x.timeline().querySelectorAll('.board-drawer-event')];
  assert.deepStrictEqual(rows.map((row) => row.dataset.kind), ['主 Agent 回复', '用户消息']);
});

(async () => {
  let failed = 0;
  for (const t of tests) {
    try {
      await t.fn();
      console.log('  ok  ' + t.name);
    } catch (e) {
      failed++;
      console.error('  FAIL ' + t.name);
      console.error('       ' + (e && e.message ? e.message : e));
    }
  }
  console.log(`\nboard_reply_sync: ${tests.length - failed}/${tests.length} 通过`);
  if (failed) process.exit(1);
})();
