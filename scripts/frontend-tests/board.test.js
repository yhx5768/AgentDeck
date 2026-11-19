// 主 Agent 看板（#board-view）前端渲染回归：只读看板的列分组、卡片字段、
// 项目筛选与空态。数据来自 GET /api/agent-board 快照（服务端已过滤归档任务）。
//
// 局限：jsdom 不做布局，列的横向排布/折叠手感验不了；这里钉的是 DOM 契约
//（分列、卡片字段、筛选、空态文案）。
const fs = require('fs');
const path = require('path');
const { JSDOM } = require('jsdom');

const WEB = path.resolve(__dirname, '../../web');
const src = fs.readFileSync(path.join(WEB, 'app.js'), 'utf8');

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

const HTML = `<!doctype html><html><body>
  <div id="board-view">
    <span id="board-updated"></span>
    <select id="board-project-filter"></select>
    <div id="board-columns"></div>
    <div id="board-empty" class="hidden"></div>
  </div>
</body></html>`;

const dom = new JSDOM(HTML, { runScripts: 'outside-only' });
const w = dom.window;
const doc = w.document;

w.eval(`
const $ = (sel) => document.querySelector(sel);
const state = { agentBoard: null, boardSelectedId: null, boardCollapsed: new Set() };
function boardCloseDrawer() { state.boardSelectedId = null; }
function boardOpenDrawer() {}
${grabFn('boardTaskTags')}
${grabFn('boardTaskCard')}
${grabFn('boardStatusOrder')}
${grabFn('boardTaskVisible')}
${grabFn('boardSyncProjects')}
${grabFn('boardColumnNode')}
${grabFn('boardCounts')}
${grabFn('boardSortTasks')}
${grabFn('boardUpdateEmptyState')}
${grabFn('boardRebuildStatusColumn')}
${grabFn('boardApplyTaskUpdates')}
${grabFn('renderBoard')}
window.__state = state;
window.__renderBoard = renderBoard;
window.__apply = boardApplyTaskUpdates;
`);

const state = w.__state;
const renderBoard = w.__renderBoard;
const applyUpdates = w.__apply;

let failed = 0;
function check(name, cond, extra) {
  if (cond) console.log('  ok  ' + name);
  else { failed++; console.log('  FAIL ' + name + (extra ? '  →  ' + extra : '')); }
}

function main() {
  console.log('主 Agent 看板渲染');

  state.agentBoard = {
    source: 'sqlite', generated_at: '2026-11-04 15:40',
    projects: ['cc-webui', 'other'],
    status_order: ['新建', '进行中', '待人工', '待验收', '已完成', '已取消'],
    counts: {},
    tasks: [
      { id: 'T-1', title: '任务A', status: '进行中', project: 'cc-webui', worker: 'w1', tags: ['测试', '看板'], progress: '80%', updated_at: '2026-11-04 15:40' },
      { id: 'T-2', title: '任务B', status: '新建', project: 'other', updated_at: '2026-11-04 15:40' },
    ],
  };
  renderBoard();

  const columns = [...doc.querySelectorAll('.board-column')];
  const colByStatus = (s) => columns.find((c) => c.querySelector('.board-column-title').textContent === s);
  check('按 status_order 出列（新建 / 进行中）',
    !!colByStatus('新建') && !!colByStatus('进行中'), columns.map((c) => c.querySelector('.board-column-title').textContent).join(','));
  check('列头显示计数', colByStatus('进行中').querySelector('.board-column-count').textContent === '1');

  const card = doc.querySelector('.board-card[data-task-id="T-1"]');
  check('卡片渲染', !!card);
  check('卡片显示任务 id', card.querySelector('.board-card-id').textContent === 'T-1');
  check('卡片显示项目', card.querySelector('.board-project').textContent === 'cc-webui');
  check('卡片显示标题', card.querySelector('.board-card-title').textContent === '任务A');
  check('卡片显示 worker', card.querySelector('.board-card-meta').textContent.includes('w1'));
  check('卡片显示标签', [...card.querySelectorAll('.board-tag')].map((t) => t.textContent).join(',') === '测试,看板');
  check('卡片显示进度', card.querySelector('.board-card-progress').textContent === '80%');

  const opts = [...doc.querySelectorAll('#board-project-filter option')].map((o) => o.value);
  check('项目下拉含全部项目 + 两个项目', JSON.stringify(opts) === JSON.stringify(['', 'cc-webui', 'other']), JSON.stringify(opts));

  // 切到 other 项目：只剩 T-2。
  doc.querySelector('#board-project-filter').value = 'other';
  renderBoard();
  const ids = [...doc.querySelectorAll('.board-card')].map((c) => c.dataset.taskId);
  check('项目筛选只留该项目任务', JSON.stringify(ids) === JSON.stringify(['T-2']), JSON.stringify(ids));

  // 空态。
  doc.querySelector('#board-project-filter').value = '';
  state.agentBoard.tasks = [];
  renderBoard();
  check('无任务时显示空态', !doc.querySelector('#board-empty').classList.contains('hidden') &&
    doc.querySelector('#board-empty').textContent.includes('暂无任务'));

  // ---------- 增量 patch（SSE delta） ----------
  console.log('看板增量 patch');
  const baseTasks = () => ([
    { id: 'T-1', title: '任务A', status: '进行中', project: 'cc-webui', worker: 'w1', tags: ['测试'], progress: '80%', updated_at: '2026-11-04 15:40' },
    { id: 'T-2', title: '任务B', status: '进行中', project: 'cc-webui', updated_at: '2026-11-04 15:39' },
    { id: 'T-3', title: '任务C', status: '新建', project: 'other', updated_at: '2026-11-04 15:38' },
  ]);
  const freshBoard = () => ({
    source: 'sqlite', generated_at: '2026-11-04 15:40',
    projects: ['cc-webui', 'other'],
    status_order: ['新建', '进行中', '待人工', '待验收', '已完成', '已取消'],
    counts: {}, tasks: baseTasks(),
  });
  state.agentBoard = freshBoard();
  doc.querySelector('#board-project-filter').value = '';
  renderBoard();

  const untouched = doc.querySelector('.board-card[data-task-id="T-3"]');
  const updatedT1 = Object.assign({}, baseTasks()[0], { status: '已完成', progress: '100%' });
  applyUpdates([{ id: 'T-1', task: updatedT1 }]);

  const newCol = [...doc.querySelectorAll('.board-column')].find((c) => c.querySelector('.board-column-title').textContent === '进行中');
  const doneCol = [...doc.querySelectorAll('.board-column')].find((c) => c.querySelector('.board-column-title').textContent === '已完成');
  check('delta：卡片移动到新状态列', !!doneCol.querySelector('.board-card[data-task-id="T-1"]') &&
    !newCol.querySelector('.board-card[data-task-id="T-1"]'));
  check('delta：旧状态列计数下降', newCol.querySelector('.board-column-count').textContent === '1');
  check('delta：新状态列计数上升', doneCol.querySelector('.board-column-count').textContent === '1');
  check('delta：其他列卡片节点原地保留（未整板重绘）',
    doc.contains(untouched) && untouched === doc.querySelector('.board-card[data-task-id="T-3"]'));
  check('delta：项目筛选项不变', JSON.stringify([...doc.querySelectorAll('#board-project-filter option')].map((o) => o.value)) ===
    JSON.stringify(['', 'cc-webui', 'other']));
  check('delta：卡片内容已更新', doneCol.querySelector('.board-card[data-task-id="T-1"] .board-card-progress').textContent === '100%');

  // 新项目出现：下拉补项，当前筛选保持。
  doc.querySelector('#board-project-filter').value = 'other';
  renderBoard();
  applyUpdates([{ id: 'T-4', task: { id: 'T-4', title: '任务D', status: '新建', project: 'new-proj', updated_at: '2026-11-04 15:41' } }]);
  check('delta：新项目出现在下拉里', [...doc.querySelectorAll('#board-project-filter option')].some((o) => o.value === 'new-proj'));
  check('delta：当前项目筛选保持', doc.querySelector('#board-project-filter').value === 'other');
  check('delta：被筛掉的卡不渲染', !doc.querySelector('.board-card[data-task-id="T-4"]'));

  // 归档/删除：卡片移除并关闭抽屉。
  doc.querySelector('#board-project-filter').value = '';
  renderBoard();
  state.boardSelectedId = 'T-1';
  applyUpdates([{ id: 'T-1', remove: true }]);
  check('delta：归档任务从列里移除', !doc.querySelector('.board-card[data-task-id="T-1"]'));
  check('delta：移除选中任务后关闭抽屉', state.boardSelectedId === null);

  // 全量重排时保留滚动位置。
  const view = doc.querySelector('#board-view');
  view.scrollTop = 120;
  renderBoard({ preserveScroll: true });
  check('全量重排保留滚动位置', view.scrollTop === 120);

  console.log(failed ? `\n${failed} 项失败` : '\n全部通过');
  process.exit(failed ? 1 : 0);
}

main();
