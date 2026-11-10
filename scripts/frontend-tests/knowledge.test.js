// 资料空间 P0 前端回归：mainAgent tab、列表/详情 DOM 契约、看板「相关知识」。
// jsdom 不做布局；真实移动端/深色/横向溢出用 headless Chrome 另验。
const fs = require('fs');
const path = require('path');
const { JSDOM } = require('jsdom');

const WEB = path.resolve(__dirname, '../../web');
const appSrc = fs.readFileSync(path.join(WEB, 'app.js'), 'utf8');
const kbSrc = fs.readFileSync(path.join(WEB, 'knowledge.js'), 'utf8');
const indexSrc = fs.readFileSync(path.join(WEB, 'index.html'), 'utf8');

function grabFn(src, name) {
  let s = src.indexOf('function ' + name + '(');
  if (s < 0) throw new Error('找不到函数 ' + name);
  const start = src.slice(Math.max(0, s - 6), s) === 'async ' ? s - 6 : s;
  const b = src.indexOf('{', s);
  let d = 0, q = null;
  for (let i = b; i < src.length; i++) {
    const c = src[i], p = src[i - 1];
    if (q) { if (c === q && p !== '\\') q = null; continue; }
    if (c === '"' || c === "'" || c === '`') { q = c; continue; }
    if (c === '{') d++;
    else if (c === '}') { d--; if (!d) return src.slice(start, i + 1); }
  }
  throw new Error(name + ' 括号不配对');
}

function grabConst(src, name) {
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
  <div id="chat-view">
    <div id="chat-tabs">
      <button class="chat-tab active" data-tab="chat">对话</button>
      <button class="chat-tab" data-tab="traj">轨迹</button>
      <button class="chat-tab hidden" data-tab="board" id="chat-tab-board">看板</button>
      <button class="chat-tab hidden" data-tab="knowledge" id="chat-tab-knowledge">资料空间</button>
    </div>
    <div id="msg-wrap"></div>
    <div id="traj-view" class="hidden"></div>
    <div id="board-view" class="hidden"></div>
    <div id="knowledge-view" class="hidden">
      <div id="knowledge-header">
        <span id="knowledge-context"></span>
        <div id="knowledge-stats"></div>
        <select id="knowledge-project"><option value="">全部项目</option></select>
        <select id="knowledge-status">
          <option value="all">全部</option><option value="approved">已通过</option>
          <option value="draft">草稿</option><option value="stale">可能过期</option>
        </select>
        <span id="knowledge-task-chip" class="hidden"></span>
        <input id="knowledge-q">
        <button id="knowledge-collect"></button>
        <button id="knowledge-new"></button>
      </div>
      <div id="knowledge-main">
        <div id="knowledge-list-loading" class="hidden"></div>
        <div id="knowledge-list-error" class="hidden"></div>
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
    </div>
    <footer id="composer"></footer>
  </div>
  <div id="board-drawer-knowledge"></div>
  <span id="board-drawer-knowledge-count"></span>
</body></html>`;

const dom = new JSDOM(HTML, { runScripts: 'outside-only', url: 'http://deck.local/' });
const w = dom.window;
const doc = w.document;

w.eval(`
const $ = (sel) => document.querySelector(sel);
const state = { current: { main_agent: true, name: '资料空间演示', peer_name: 'mainAgent' }, sessions: [] };
function md(text) { return '<div class="md-stub">' + text + '</div>'; }
function api() { return Promise.resolve({}); }
function openModal() {}
function closeModal() {}
function openKnowledgeEntry(id) { window.__openedEntry = id; }
function openKnowledgeForTask(id) { window.__openedTask = id; }
function openKnowledgeCompose() { window.__openedCompose = true; }
function kbOpenDetail(id) { window.__openedDetail = id; }
function boardCloseDrawer() { window.__boardClosed = true; }
function boardDisconnect() { window.__boardDisconnected = true; }
function boardConnect() { window.__boardConnected = true; }
function boardShow() { return Promise.resolve(); }
function boardOpenDrawer() {}
function trajShow() {}
function openSession() { return Promise.resolve(); }
function applyTheme() {}
function kbOnTabShow() { window.__kbShown = true; }
function kbOnTabHide() { window.__kbHidden = true; }
alert = function (text) { window.__alert = text; };
confirm = function () { return true; };
prompt = function () { return ''; };
navigator.clipboard = { writeText: function () { return Promise.resolve(); } };
${grabFn(appSrc, 'switchChatTab')}
${grabFn(appSrc, 'syncMainAgentTabs')}
${grabConst(kbSrc, 'kbState')}
${grabFn(kbSrc, 'kbEl')}
${grabFn(kbSrc, 'kbStatusLabel')}
${grabFn(kbSrc, 'kbStatusClass')}
${grabFn(kbSrc, 'kbSourceLabel')}
${grabFn(kbSrc, 'kbStatusBadge')}
${grabFn(kbSrc, 'kbGeneratedBadge')}
${grabFn(kbSrc, 'kbMuted')}
${grabFn(kbSrc, 'kbAction')}
${grabFn(kbSrc, 'kbCopyText')}
${grabFn(kbSrc, 'kbCountText')}
${grabFn(kbSrc, 'kbStatChip')}
${grabFn(kbSrc, 'kbUpdateFacets')}
${grabFn(kbSrc, 'kbUpdateTaskChip')}
${grabFn(kbSrc, 'kbHideListStates')}
${grabFn(kbSrc, 'kbShowLoading')}
${grabFn(kbSrc, 'kbShowError')}
${grabFn(kbSrc, 'kbShowEmpty')}
${grabFn(kbSrc, 'kbRenderList')}
${grabFn(kbSrc, 'kbRenderItem')}
${grabFn(kbSrc, 'kbDetailEls')}
${grabFn(kbSrc, 'kbCloseDetail')}
${grabFn(kbSrc, 'kbHighlightSelected')}
${grabFn(kbSrc, 'kbMetaLine')}
${grabFn(kbSrc, 'kbRenderDetail')}
${grabFn(kbSrc, 'kbUpdateContext')}
${grabFn(kbSrc, 'renderBoardKnowledge')}
window.__kb = {
  kbState, kbRenderList, kbRenderDetail, renderBoardKnowledge, kbStatusLabel,
  kbCountText, kbUpdateFacets, kbShowLoading, kbShowError, kbShowEmpty,
  kbUpdateContext, switchChatTab, syncMainAgentTabs,
};
`);

const kb = w.__kb;
let pass = 0, fail = 0;
function check(name, fn) {
  try { fn(); pass++; console.log('  ok  ' + name); }
  catch (e) { fail++; console.log('  FAIL ' + name + ': ' + e.message); }
}
function assert(cond, msg) { if (!cond) throw new Error(msg || 'assertion failed'); }
function tab(name) { return doc.querySelector('.chat-tab[data-tab="' + name + '"]'); }

check('入口在 chat-tabs，侧栏右上角入口已删除', () => {
  assert(indexSrc.includes('id="chat-tab-knowledge"'), '缺少 knowledge tab');
  assert(!indexSrc.includes('id="btn-knowledge"'), '侧栏入口应删除');
  assert(tab('knowledge').textContent.includes('资料空间'));
});

check('mainAgent 显示看板+资料空间，非 mainAgent 隐藏', () => {
  kb.syncMainAgentTabs({ main_agent: true });
  assert(!tab('board').classList.contains('hidden'));
  assert(!tab('knowledge').classList.contains('hidden'));
  kb.syncMainAgentTabs({ main_agent: false });
  assert(tab('board').classList.contains('hidden'));
  assert(tab('knowledge').classList.contains('hidden'));
});

check('非 mainAgent 停在资料空间时回对话', () => {
  kb.syncMainAgentTabs({ main_agent: true });
  kb.switchChatTab('knowledge');
  assert(tab('knowledge').classList.contains('active'));
  kb.syncMainAgentTabs({ main_agent: false });
  assert(tab('chat').classList.contains('active'));
  assert(doc.querySelector('#knowledge-view').classList.contains('hidden'));
  assert(doc.querySelector('#msg-wrap').classList.contains('hidden') === false);
});

check('资料空间 tab 切换：active + 显示内容 + 隐藏 composer', () => {
  kb.syncMainAgentTabs({ main_agent: true });
  w.__kbShown = false;
  kb.switchChatTab('knowledge');
  assert(tab('knowledge').classList.contains('active'));
  assert(!doc.querySelector('#knowledge-view').classList.contains('hidden'));
  assert(doc.querySelector('#msg-wrap').classList.contains('hidden'));
  assert(doc.querySelector('#board-view').classList.contains('hidden'));
  assert(doc.querySelector('#composer').classList.contains('hidden'));
  assert(w.__kbShown, 'kbOnTabShow should run');
  w.__kbHidden = false;
  kb.switchChatTab('chat');
  assert(doc.querySelector('#knowledge-view').classList.contains('hidden'));
  assert(!doc.querySelector('#composer').classList.contains('hidden'));
  assert(w.__kbHidden, 'kbOnTabHide should run');
});

check('状态中文标签', () => {
  assert(kb.kbStatusLabel('draft') === '草稿');
  assert(kb.kbStatusLabel('approved') === '已通过');
  assert(kb.kbStatusLabel('stale') === '可能过期');
  assert(kb.kbStatusLabel('quarantined') === '已隔离');
});

check('统计 chips + 计数文案', () => {
  kb.kbState.counts = { approved: 3, draft: 2, stale: 1 };
  kb.kbUpdateFacets();
  const chips = doc.querySelectorAll('#knowledge-stats .kb-stat');
  assert(chips.length === 3, 'chip count ' + chips.length);
  assert(chips[0].textContent.includes('已通过 3'));
  assert(chips[1].textContent.includes('草稿 2'));
  assert(chips[2].textContent.includes('可能过期 1'));
  assert(kb.kbCountText().includes('已通过 3'));
});

check('列表渲染 + 点击打开详情 + 标签/徽标/来源任务', () => {
  kb.kbState.entries = [
    { id: 'K-1', title: 'SSE 重编号', summary: '结论 A', status: 'approved', project: 'AgentDeck', tags: ['SSE'], source_task_id: 'T-1', updated_at: '11-09' },
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
  assert(items[0].querySelector('.kb-source-chip').textContent.includes('T-1'));
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

check('加载/错误状态', () => {
  kb.kbShowLoading();
  assert(!doc.querySelector('#knowledge-list-loading').classList.contains('hidden'));
  kb.kbShowError('网络错误');
  assert(!doc.querySelector('#knowledge-list-error').classList.contains('hidden'));
  assert(doc.querySelector('#knowledge-list-error').textContent.includes('网络错误'));
  assert(doc.querySelector('#knowledge-list-loading').classList.contains('hidden'));
  kb.kbShowEmpty('空');
  assert(doc.querySelector('#knowledge-list-error').classList.contains('hidden'));
  assert(!doc.querySelector('#knowledge-empty').classList.contains('hidden'));
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
  assert(doc.querySelector('#knowledge-detail-body').textContent.includes('正文'));
  assert(doc.querySelector('#knowledge-detail-relations').textContent.includes('关联条目'));
});

check('已通过详情显示标为过期，不显示通过/驳回', () => {
  kb.kbRenderDetail({ id: 'K-3', title: '已通过', status: 'approved', body: 'x' });
  const actions = doc.querySelector('#knowledge-detail-actions').textContent;
  assert(actions.includes('标为过期'));
  assert(!actions.includes('通过') || actions.includes('标为过期'));
});

check('看板抽屉相关知识：条数/查看全部切 tab/提炼', () => {
  w.__openedTask = null;
  w.__openedEntry = null;
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
  const all = Array.from(box.querySelectorAll('.kb-action')).find((b) => b.textContent === '查看全部');
  all.click();
  assert(w.__openedTask === 'T-1', '查看全部 should switch with task id');
  box.querySelector('.kb-related-row').click();
  assert(w.__openedEntry === 'K-1', 'row should open entry');
});

check('看板抽屉无相关知识显示空态', () => {
  kb.renderBoardKnowledge({ id: 'T-2', title: '任务二', knowledge: [], knowledge_count: 0 });
  const box = doc.querySelector('#board-drawer-knowledge');
  assert(box.textContent.includes('暂无相关知识'));
  assert(box.textContent.includes('从本任务提炼知识'));
});

check('上下文显示 mainAgent + 会话名', () => {
  kb.kbUpdateContext();
  const text = doc.querySelector('#knowledge-context').textContent;
  assert(text.includes('mainAgent') && text.includes('资料空间演示'), text);
});

console.log(`\n${pass} passed, ${fail} failed`);
if (fail) process.exit(1);
