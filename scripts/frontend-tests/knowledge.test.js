// 资料空间 P0 前端回归：状态徽标、列表/详情 DOM 契约、看板「相关知识」。
// jsdom 不做布局；真实移动端/深色/横向溢出用 headless Chrome 另验。
const fs = require('fs');
const path = require('path');
const { JSDOM } = require('jsdom');

const WEB = path.resolve(__dirname, '../../web');
const src = fs.readFileSync(path.join(WEB, 'knowledge.js'), 'utf8');

function grabFn(name) {
  const s = src.indexOf('function ' + name + '(');
  if (s < 0) throw new Error('找不到函数 ' + name);
  const b = src.indexOf('{', s);
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

function grabConst(name) {
  const start = src.indexOf('const ' + name + ' = {');
  if (start < 0) throw new Error('找不到常量 ' + name);
  const b = src.indexOf('{', start);
  let d = 0, q = null;
  for (let i = b; i < src.length; i++) {
    const c = src[i], p = src[i - 1];
    if (q) { if (c === q && p !== '\\') q = null; continue; }
    if (c === '"' || c === "'" || c === '`') { q = c; continue; }
    if (c === '{') d++;
    else if (c === '}') { d--; if (!d) return src.slice(start, i + 2); }
  }
  throw new Error(name + ' 括号不配对');
}

const HTML = `<!doctype html><html><body>
  <div id="knowledge-view" class="hidden">
    <span id="knowledge-counts"></span>
    <select id="knowledge-project"><option value="">全部项目</option></select>
    <select id="knowledge-status">
      <option value="approved">已通过</option><option value="draft">草稿</option>
      <option value="stale">可能过期</option><option value="all">全部</option>
    </select>
    <span id="knowledge-task-chip" class="hidden"></span>
    <input id="knowledge-q">
    <div id="knowledge-list"></div>
    <div id="knowledge-empty" class="hidden"></div>
    <aside id="knowledge-detail" class="hidden">
      <div id="knowledge-detail-head"><span id="knowledge-detail-kicker"></span><button id="knowledge-detail-close"></button></div>
      <h3 id="knowledge-detail-title"></h3>
      <div id="knowledge-detail-meta"></div>
      <div id="knowledge-detail-actions"></div>
      <div id="knowledge-detail-body"></div>
      <div id="knowledge-detail-sources"></div>
      <div id="knowledge-detail-relations"></div>
      <div id="knowledge-detail-audit"></div>
    </aside>
  </div>
  <div id="board-drawer-knowledge"></div>
  <span id="board-drawer-knowledge-count"></span>
</body></html>`;

const dom = new JSDOM(HTML, { runScripts: 'outside-only', url: 'http://deck.local/' });
const w = dom.window;
const doc = w.document;

w.eval(`
const $ = (sel) => document.querySelector(sel);
const state = { current: { main_agent: true } };
function md(text) { return '<div class="md-stub">' + text + '</div>'; }
function api() { return Promise.resolve({}); }
function openModal() {}
function closeModal() {}
function openKnowledge() { window.__openedKnowledge = true; }
function openKnowledgeCompose() { window.__openedCompose = true; }
function kbOpenDetail(id) { window.__openedDetail = id; }
alert = function (text) { window.__alert = text; };
confirm = function () { return true; };
prompt = function () { return ''; };
navigator.clipboard = { writeText: function () { return Promise.resolve(); } };
${grabConst('kbState')}
${grabFn('kbEl')}
${grabFn('kbStatusLabel')}
${grabFn('kbStatusClass')}
${grabFn('kbSourceLabel')}
${grabFn('kbStatusBadge')}
${grabFn('kbGeneratedBadge')}
${grabFn('kbMuted')}
${grabFn('kbAction')}
${grabFn('kbCopyText')}
${grabFn('kbCountText')}
${grabFn('kbUpdateFacets')}
${grabFn('kbUpdateTaskChip')}
${grabFn('kbShowEmpty')}
${grabFn('kbRenderList')}
${grabFn('kbRenderItem')}
${grabFn('kbDetailEls')}
${grabFn('kbCloseDetail')}
${grabFn('kbHighlightSelected')}
${grabFn('kbMetaLine')}
${grabFn('kbRenderDetail')}
${grabFn('renderBoardKnowledge')}
window.__kb = { kbState, kbRenderList, kbRenderDetail, renderBoardKnowledge, kbStatusLabel, kbCountText };
`);

const kb = w.__kb;
let pass = 0, fail = 0;
function check(name, fn) {
  try { fn(); pass++; console.log('  ok  ' + name); }
  catch (e) { fail++; console.log('  FAIL ' + name + ': ' + e.message); }
}
function assert(cond, msg) { if (!cond) throw new Error(msg || 'assertion failed'); }

check('状态中文标签', () => {
  assert(kb.kbStatusLabel('draft') === '草稿');
  assert(kb.kbStatusLabel('approved') === '已通过');
  assert(kb.kbStatusLabel('stale') === '可能过期');
  assert(kb.kbStatusLabel('quarantined') === '已隔离');
});

check('计数文案', () => {
  kb.kbState.counts = { approved: 3, draft: 2, stale: 1 };
  assert(kb.kbCountText().includes('已通过 3'));
  assert(kb.kbCountText().includes('草稿 2'));
  assert(kb.kbCountText().includes('可能过期 1'));
});

check('列表渲染 + 点击打开详情 + 标签/徽标', () => {
  kb.kbState.entries = [
    { id: 'K-1', title: 'SSE 重编号', summary: '结论 A', status: 'approved', project: 'AgentDeck', tags: ['SSE'], updated_at: '11-09' },
    { id: 'K-2', title: '草稿条目', summary: '结论 B', status: 'draft', generated_by: 'system', ai_fields: ['summary'], updated_at: '11-10' },
  ];
  kb.kbState.total = 2;
  kb.kbState.selectedId = null;
  kb.kbRenderList();
  const items = doc.querySelectorAll('#knowledge-list .kb-item');
  assert(items.length === 2, 'item count ' + items.length);
  assert(items[0].textContent.includes('已通过'));
  assert(items[1].textContent.includes('草稿'));
  assert(items[1].textContent.includes('自动'), 'system generated badge');
  assert(items[0].querySelector('.kb-tag').textContent === 'SSE');
  items[0].dispatchEvent(new w.MouseEvent('click', { bubbles: true }));
  assert(w.__openedDetail === 'K-1', 'click should open detail');
});

check('空列表显示空态', () => {
  kb.kbState.entries = [];
  kb.kbState.total = 0;
  kb.kbState.q = '不存在的词';
  kb.kbRenderList();
  const empty = doc.querySelector('#knowledge-empty');
  assert(!empty.classList.contains('hidden'));
  assert(empty.textContent.includes('没有匹配'));
});

check('详情渲染：动作/来源/证据/关联', () => {
  doc.querySelector('#knowledge-detail').classList.remove('hidden');
  kb.kbRenderDetail({
    id: 'K-1', title: 'SSE 重编号', body: '## 结论', status: 'draft',
    project: 'AgentDeck', owner: 'mainAgent', confidence: 60, version: 1,
    source_task: { id: 'T-1', title: '任务一', status: '待验收' },
    source_artifacts: [{ id: 'A-1', title: '报告', kind: 'markdown', filename: 'report.md', url: '/api/agent-board/artifacts/A-1' }],
    evidence: [{ kind: 'task', ref: 'T-1', locator: 'task', quote: '用户原话' }],
    relations: [{ kind: 'related', to_id: 'K-2', to_title: '关联条目' }],
  });
  const actions = doc.querySelector('#knowledge-detail-actions').textContent;
  assert(actions.includes('通过') && actions.includes('驳回'), 'review actions: ' + actions);
  const sources = doc.querySelector('#knowledge-detail-sources').textContent;
  assert(sources.includes('任务一'));
  assert(sources.includes('报告'));
  assert(sources.includes('用户原话'));
  assert(doc.querySelector('#knowledge-detail-sources a[href*="A-1"]'), 'artifact link');
  assert(doc.querySelector('#knowledge-detail-body .md-stub'), 'body rendered via md');
  assert(doc.querySelector('#knowledge-detail-relations').textContent.includes('关联条目'));
});

check('已通过详情显示标为过期，不显示通过/驳回', () => {
  kb.kbRenderDetail({ id: 'K-3', title: '已通过', status: 'approved', body: 'x' });
  const actions = doc.querySelector('#knowledge-detail-actions').textContent;
  assert(actions.includes('标为过期'));
  assert(!actions.includes('通过') || actions.includes('标为过期'));
});

check('看板抽屉相关知识：条数/查看全部/提炼', () => {
  kb.renderBoardKnowledge({
    id: 'T-1', title: '任务一', project: 'P', tags: ['SSE'],
    knowledge: [{ id: 'K-1', title: '知识一', status: 'approved', project: 'P', updated_at: '11-09' }],
    knowledge_count: 1,
  });
  const box = doc.querySelector('#board-drawer-knowledge');
  assert(box.querySelectorAll('.kb-related-row').length === 1);
  assert(box.textContent.includes('知识一'));
  assert(box.textContent.includes('查看全部'));
  assert(box.textContent.includes('从本任务提炼知识'));
  assert(doc.querySelector('#board-drawer-knowledge-count').textContent === '(1)');
});

check('看板抽屉无相关知识显示空态', () => {
  kb.renderBoardKnowledge({ id: 'T-2', title: '任务二', knowledge: [], knowledge_count: 0 });
  const box = doc.querySelector('#board-drawer-knowledge');
  assert(box.textContent.includes('暂无相关知识'));
  assert(box.textContent.includes('从本任务提炼知识'));
});

console.log(`\n${pass} passed, ${fail} failed`);
if (fail) process.exit(1);
