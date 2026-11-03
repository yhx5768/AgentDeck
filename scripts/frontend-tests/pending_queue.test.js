// 待发送消息队列的回归测试（YHX-TODO-816）。
//
// 旧实现每个会话只有一个槽位，第二次发送会覆盖第一次；从多个任务抽屉快速
// 连发时，除最后一条外都会静默丢失。这里直接摘出 app.js 里的队列函数跑，
// 钉住：追加不覆盖、逐条 drain、失败保留队首、取消/重试、会话隔离、
// 每条消息自带的任务上下文不被后来的卡片覆盖。
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
  // 参数表里可能有默认对象（meta = {}），不能拿第一个 { 当函数体。
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
const state = { current: { id: 's1' }, pendingBySid: {}, boardMockBusy: false };
let pendingSeq = 0;
let postMessageImpl = async () => {};
const sent = [];
const sentEvents = [];
const errors = [];
async function postMessage(payload) { return postMessageImpl(payload); }
async function boardAppendUserEvent(taskId, text) { sentEvents.push({ taskId, text }); }
function renderPendingBubble() {}
function clearPendingBubble() {}
function setErrorBanner(message) { errors.push(message); }
function isRunning() { return false; }
${grabFn('pendingItems')}
${grabFn('pendingQueue')}
${grabFn('enqueuePending')}
${grabFn('removePendingItem')}
${grabFn('holdPending')}
${grabFn('flushPending')}
${grabFn('retryPending')}
${grabFn('cancelPending')}
window.__x = {
  state, sent, sentEvents, errors,
  pendingItems, enqueuePending, holdPending, flushPending, retryPending, cancelPending,
  setPost(fn) { postMessageImpl = fn; },
  reset() { state.pendingBySid = {}; state.current = { id: 's1' }; state.boardMockBusy = false; sent.length = 0; sentEvents.length = 0; errors.length = 0; postMessageImpl = async () => {}; },
};
`;

const dom = new JSDOM('<!doctype html><html><body></body></html>',
  { runScripts: 'outside-only', pretendToBeVisual: true });
dom.window.eval(setup);
const x = dom.window.__x;

const tests = [];
const test = (name, fn) => tests.push({ name, fn });
const tick = () => new Promise((resolve) => setTimeout(resolve, 0));
// JSDOM 的 window 里创建的对象/数组和 Node 主 realm 的原型不同，先转成普通值再比。
const plain = (value) => JSON.parse(JSON.stringify(value));

test('12 条快速发送全部保留且保持顺序（不再覆盖）', () => {
  x.reset();
  const texts = Array.from({ length: 12 }, (_, i) => `msg-${i + 1}`);
  texts.forEach((text) => x.holdPending(text, [], []));
  assert.deepStrictEqual(plain(x.pendingItems('s1').map((item) => item.text)), texts);
  assert.strictEqual(x.pendingItems('s1').length, 12);
});

test('每次 idle 只提交队首，成功后队列前移', async () => {
  x.reset();
  x.setPost(async (payload) => { x.sent.push(payload.text); });
  ['a', 'b', 'c'].forEach((text) => x.holdPending(text, [], []));
  await x.flushPending();
  assert.deepStrictEqual(plain(x.sent), ['a']);
  assert.deepStrictEqual(plain(x.pendingItems('s1').map((item) => item.text)), ['b', 'c']);
  await x.flushPending();
  assert.deepStrictEqual(plain(x.sent), ['a', 'b']);
  assert.deepStrictEqual(plain(x.pendingItems('s1').map((item) => item.text)), ['c']);
});

test('提交失败保留队首并标记 error，可重试', async () => {
  x.reset();
  let fail = true;
  x.setPost(async (payload) => {
    if (fail) throw new Error('boom');
    x.sent.push(payload.text);
  });
  ['a', 'b'].forEach((text) => x.holdPending(text, [], []));
  await x.flushPending();
  const queue = x.pendingItems('s1');
  assert.strictEqual(queue.length, 2);
  assert.strictEqual(queue[0].status, 'error');
  assert.ok(x.errors.some((message) => message.includes('boom')));
  fail = false;
  x.retryPending(queue[0].id);
  await tick();
  assert.deepStrictEqual(plain(x.sent), ['a']);
  assert.deepStrictEqual(plain(x.pendingItems('s1').map((item) => item.text)), ['b']);
});

test('取消队首后下一条继续提交', async () => {
  x.reset();
  x.setPost(async (payload) => { x.sent.push(payload.text); });
  ['a', 'b'].forEach((text) => x.holdPending(text, [], []));
  x.cancelPending(x.pendingItems('s1')[0].id);
  await tick();
  assert.deepStrictEqual(plain(x.sent), ['b']);
  assert.strictEqual(x.pendingItems('s1').length, 0);
});

test('不同会话队列互相隔离', () => {
  x.reset();
  x.holdPending('s1-a', [], []);
  x.holdPending('s1-b', [], []);
  x.state.current = { id: 's2' };
  x.holdPending('s2-a', [], []);
  x.state.current = { id: 's1' };
  assert.deepStrictEqual(plain(x.pendingItems('s1').map((item) => item.text)), ['s1-a', 's1-b']);
  assert.deepStrictEqual(plain(x.pendingItems('s2').map((item) => item.text)), ['s2-a']);
});

test('每条消息自带的任务上下文不被后来的卡片覆盖，成功后才记任务流', async () => {
  x.reset();
  x.setPost(async (payload) => { x.sent.push(payload.text); });
  const ctxA = '【来自看板任务抽屉】\ntask_id: A\n---\n请处理 A';
  const ctxB = '【来自看板任务抽屉】\ntask_id: B\n---\n请处理 B';
  x.holdPending(ctxA, [], [], { taskId: 'A', bodyText: '请处理 A' });
  x.holdPending(ctxB, [], [], { taskId: 'B', bodyText: '请处理 B' });
  await x.flushPending();
  assert.deepStrictEqual(plain(x.sent), [ctxA]);
  assert.deepStrictEqual(plain(x.sentEvents), [{ taskId: 'A', text: '请处理 A' }]);
  assert.deepStrictEqual(plain(x.pendingItems('s1').map((item) => item.text)), [ctxB]);
  await x.flushPending();
  assert.deepStrictEqual(plain(x.sent), [ctxA, ctxB]);
  assert.deepStrictEqual(plain(x.sentEvents), [
    { taskId: 'A', text: '请处理 A' },
    { taskId: 'B', text: '请处理 B' },
  ]);
});

test('模拟忙碌开关开启时只保留队列，不 POST；关闭后继续发送', async () => {
  x.reset();
  x.setPost(async (payload) => { x.sent.push(payload.text); });
  x.state.boardMockBusy = true;
  x.holdPending('a', [], []);
  await x.flushPending();
  assert.strictEqual(x.sent.length, 0);
  assert.strictEqual(x.pendingItems('s1').length, 1);
  x.state.boardMockBusy = false;
  await x.flushPending();
  assert.deepStrictEqual(plain(x.sent), ['a']);
  assert.strictEqual(x.pendingItems('s1').length, 0);
});

test('CSS 契约：队列列表可滚动、有逐条状态和失败重试', () => {
  const css = fs.readFileSync(path.join(WEB, 'style.css'), 'utf8');
  assert.ok(/\.pending-bubble \.pb-list\s*\{[\s\S]*?overflow-y\s*:\s*auto/.test(css),
    '.pb-list 必须内部滚动，避免 10+ 条把 composer 撑爆');
  assert.ok(/\.pending-bubble \.pb-state\.error\s*\{/.test(css), '失败状态要有独立样式');
  assert.ok(/\.pending-bubble \.pb-retry\s*\{/.test(css), '失败条目要有重试按钮');
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
  console.log(`\npending_queue: ${tests.length - failed}/${tests.length} 通过`);
  if (failed) process.exit(1);
})();
