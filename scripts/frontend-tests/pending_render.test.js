// 待发送队列的展示契约（20261110-pending-queue-ui）。
//
// 队列逻辑在 pending_queue.test.js 里钉住；这里只钉渲染：看板任务上下文
// 与正文分层、长任务 ID 中间省略、排队/发送中/失败状态、重试/取消入口，
// 以及窄屏不溢出的 CSS 契约（jsdom 不做布局，只能钉规则本身）。
const fs = require('fs');
const path = require('path');
const assert = require('assert');
const { JSDOM } = require('jsdom');

const WEB = path.resolve(__dirname, '../../web');
const src = fs.readFileSync(path.join(WEB, 'app.js'), 'utf8');
const css = fs.readFileSync(path.join(WEB, 'style.css'), 'utf8');

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
const state = { current: { id: 's1' }, pendingBySid: {} };
const $ = (sel) => document.querySelector(sel);
const calls = { retry: [], cancel: [], cancelAll: 0, queueStatus: 0 };
function pendingItems(sid) { return (sid && state.pendingBySid[sid]) || []; }
function boardQueueStatus() { calls.queueStatus++; }
function retryPending(id) { calls.retry.push(id); }
function cancelPending(id) { calls.cancel.push(id); if (!id) calls.cancelAll++; }
function clearPendingBubble() {
  const el = document.getElementById('pending-bubble');
  if (el) el.remove();
}
${grabFn('middleEllipsis')}
${grabFn('pendingPreviewText')}
${grabFn('pendingEl')}
${grabFn('renderPendingBubble')}
window.__x = {
  state, calls, middleEllipsis, pendingPreviewText, renderPendingBubble,
  reset(items) {
    const card = document.querySelector('.composer-card');
    card.innerHTML = '';
    state.current = { id: 's1' };
    state.pendingBySid = items && items.length ? { s1: items } : {};
    calls.retry = []; calls.cancel = []; calls.cancelAll = 0; calls.queueStatus = 0;
    renderPendingBubble();
  },
};
`;

const dom = new JSDOM('<!doctype html><html><body><div class="composer-card"></div></body></html>',
  { runScripts: 'outside-only', pretendToBeVisual: true });
dom.window.eval(setup);
const x = dom.window.__x;
const doc = dom.window.document;

let seq = 0;
const item = (over = {}) => ({
  id: 'p' + (++seq), text: '', images: [], files: [], status: 'queued', error: '',
  ...over,
});
// JSDOM 的 window 里创建的对象/数组和 Node 主 realm 的原型不同，先转成普通值再比。
const plain = (value) => JSON.parse(JSON.stringify(value));

const tests = [];
const test = (name, fn) => tests.push({ name, fn });

test('头部层级：待发送 N 条为主，提交时机为辅助说明，全部取消在右侧', () => {
  x.reset([item({ text: 'a' }), item({ text: 'b' })]);
  assert.strictEqual(doc.querySelector('.pb-title').textContent, '待发送 2 条');
  assert.strictEqual(doc.querySelector('.pb-sub').textContent, '本轮结束后逐条提交');
  const head = doc.querySelector('.pb-head');
  assert.strictEqual(head.lastElementChild.className, 'pb-cancel-all');
  assert.strictEqual(head.firstElementChild.className, 'pb-tag');
});

test('taskId 条目正文优先用 bodyText，不再重复看板上下文前缀', () => {
  const taskId = '20261104-verify-aiqa-annotation-page';
  const body = '请检查标注页在 390 宽下的空状态、长文本和键盘顶起行为，确认没有横向溢出。';
  x.reset([item({
    text: `【来自看板任务抽屉】\ntask_id: ${taskId}\n标题: 标注页验收\n状态: 进行中\n---\n${body}`,
    bodyText: body,
    taskId,
  })]);
  const text = doc.querySelector('.pb-text');
  assert.strictEqual(text.textContent, body);
  assert.ok(!text.textContent.includes('【来自看板任务抽屉】'), '正文不应重复来源前缀');
  assert.ok(!text.textContent.includes('task_id:'), '正文不应重复 task_id');
  assert.strictEqual(text.title, body, '悬停 title 要看得到全文');
  assert.strictEqual(doc.querySelector('.pb-task').title, taskId, 'chip 的 title 是完整任务 ID');
});

test('旧队列项没有 bodyText 时，正文剥掉注入的看板上下文', () => {
  x.reset([item({
    text: '【来自看板任务抽屉】\ntask_id: T-1\n标题: 老条目\n---\n正文只有这句',
    taskId: 'T-1',
  })]);
  assert.strictEqual(doc.querySelector('.pb-text').textContent, '正文只有这句');
});

test('长任务 ID 中间省略，保留头尾，chip 不撑宽', () => {
  const taskId = '20261104-verify-aiqa-annotation-page-0123456789-abcdef';
  x.reset([item({ text: 'x', taskId })]);
  const idEl = doc.querySelector('.pb-task-id');
  assert.ok(idEl.textContent.length <= 34, `显示长度应受控：${idEl.textContent.length}`);
  assert.ok(idEl.textContent.includes('…'), '长 ID 应中间省略');
  assert.ok(idEl.textContent.startsWith('2026'), '保留头部便于辨认日期');
  assert.ok(idEl.textContent.endsWith('abcdef'), '保留尾部便于辨认唯一性');
  assert.strictEqual(doc.querySelector('.pb-task').title, taskId);
  assert.ok(idEl.textContent.length < taskId.length);
});

test('排队/发送中/失败三种状态，失败时重试与取消可见', () => {
  // 排队
  x.reset([item({ text: 'a' })]);
  assert.strictEqual(doc.querySelector('.pb-state.queued').textContent, '排队');
  assert.strictEqual(doc.querySelector('.pb-retry'), null);
  assert.ok(doc.querySelector('.pb-cancel'));

  // 发送中：不给取消入口（与旧行为一致）
  x.reset([item({ text: 'b', status: 'sending' })]);
  assert.strictEqual(doc.querySelector('.pb-state.sending').textContent, '发送中');
  assert.strictEqual(doc.querySelector('.pb-cancel'), null);

  // 失败：状态 chip + 重试 + 取消，error 详情进 title
  x.reset([item({ text: 'c', status: 'error', error: 'network down' })]);
  const badge = doc.querySelector('.pb-state.error');
  assert.strictEqual(badge.textContent, '失败');
  assert.strictEqual(badge.title, 'network down');
  assert.ok(doc.querySelector('.pb-item').classList.contains('has-error'));
  assert.ok(doc.querySelector('.pb-retry'));
  assert.ok(doc.querySelector('.pb-cancel'));
});

test('重试与取消按钮接到对应条目', () => {
  x.reset([item({ id: 'pend-x', text: 'c', status: 'error', error: 'boom' })]);
  doc.querySelector('.pb-retry').click();
  doc.querySelector('.pb-cancel').click();
  assert.deepStrictEqual(plain(x.calls.retry), ['pend-x']);
  assert.deepStrictEqual(plain(x.calls.cancel), ['pend-x']);
});

test('多条目：序号递增，正文在内容列、操作在独立侧列（DOM 上不交叠）', () => {
  x.reset([item({ text: 'one' }), item({ text: 'two' }), item({ text: 'three' })]);
  const rows = [...doc.querySelectorAll('.pb-item')];
  assert.strictEqual(rows.length, 3);
  assert.deepStrictEqual(rows.map((row) => row.querySelector('.pb-index').textContent), ['1', '2', '3']);
  for (const row of rows) {
    assert.strictEqual(row.children.length, 3, '序号 / 内容 / 状态操作为三个独立列');
    const [idx, main, side] = row.children;
    assert.strictEqual(idx.className, 'pb-index');
    assert.strictEqual(main.className, 'pb-main');
    assert.strictEqual(side.className, 'pb-side');
    assert.ok(main.querySelector('.pb-text'), '正文在内容列');
    assert.ok(side.querySelector('.pb-state'), '状态在侧列');
    assert.strictEqual(main.querySelector('.pb-side'), null, '正文与状态不共享容器');
  }
});

test('仅附件条目显示附件数量', () => {
  x.reset([item({ images: ['a.png'], files: ['b.txt'] })]);
  assert.strictEqual(doc.querySelector('.pb-text').textContent, '(仅附件 · 2 个)');
});

test('CSS 契约：三列网格 + 长文本/长 ID 不溢出 + 列表滚动 + 窄屏降级', () => {
  const rule = (selector) => {
    const re = new RegExp(selector.replace(/[.*+?^${}()|[\]\\]/g, '\\$&') + '\\s*\\{([^}]*)\\}');
    const m = css.match(re);
    assert.ok(m, '缺少样式规则 ' + selector);
    return m[1];
  };
  const itemCss = rule('.pending-bubble .pb-item');
  assert.ok(/grid-template-columns\s*:\s*16px minmax\(0,\s*1fr\) auto/.test(itemCss),
    '每行必须是 序号/内容/状态操作 三列，内容列 minmax(0,1fr) 防撑宽');
  assert.ok(/min-width\s*:\s*0/.test(rule('.pending-bubble .pb-main')), '内容列要能收缩');
  const textCss = rule('.pending-bubble .pb-text');
  assert.ok(/-webkit-line-clamp\s*:\s*2/.test(textCss), '正文最多两行');
  assert.ok(/overflow-wrap\s*:\s*anywhere/.test(textCss), '超长无空格串必须在内部断行');
  const idCss = rule('.pending-bubble .pb-task-id');
  assert.ok(/text-overflow\s*:\s*ellipsis/.test(idCss), 'ID 外层还要有省略兜底');
  assert.ok(/min-width\s*:\s*0/.test(idCss));
  const listCss = rule('.pending-bubble .pb-list');
  assert.ok(/overflow-y\s*:\s*auto/.test(listCss), '多条队列要内部滚动');
  assert.ok(/max-height\s*:\s*\d+px/.test(listCss));

  const mediaStart = css.indexOf('@media (max-width: 480px)');
  assert.ok(mediaStart >= 0, '缺少窄屏媒体查询');
  const media = css.slice(mediaStart, mediaStart + 1600);
  assert.ok(/\.pb-sub[\s\S]*?display\s*:\s*none/.test(media), '窄屏隐藏辅助说明');
  assert.ok(/\.pb-task-label[\s\S]*?display\s*:\s*none/.test(media), '窄屏隐藏 chip 的「看板任务」前缀');
  assert.ok(/grid-template-columns\s*:\s*14px minmax\(0,\s*1fr\) auto/.test(media),
    '窄屏仍保持内容列可收缩');
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
  console.log(`\npending_render: ${tests.length - failed}/${tests.length} 通过`);
  if (failed) process.exit(1);
})();
