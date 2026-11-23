// 侧栏「主 Agent / 子会话」分组渲染回归。
//
// 契约：
//  - 主 Agent 只出现在顶部 #main-agents 头像条，不再占 #session-list 的行；
//  - 子会话（parent_id 非空）按**自己的 cwd** 落进项目分组，与普通会话同级，
//    没有缩进/连接线/目录小标；
//  - worker 行带 Worker 机器人图标（.sess-worker-mark），普通会话没有；tooltip 写明由哪个会话创建；
//  - 分组计数与「迁移」都按会话自己的 cwd 走；
//  - 平铺视图同样平铺；只剩主 Agent 时给引导空态。
//
// 局限：jsdom 不做布局，图标大小/间距是否舒服验不了；这里钉 DOM 契约。
const fs = require('fs');
const path = require('path');
const { JSDOM } = require('jsdom');

const WEB = path.resolve(__dirname, '../../web');
const src = fs.readFileSync(path.join(WEB, 'app.js'), 'utf8');

function grabFn(name) {
  const s = src.indexOf('function ' + name + '(');
  if (s < 0) throw new Error('找不到函数 ' + name);
  const start = src.slice(Math.max(0, s - 6), s) === 'async ' ? s - 6 : s;
  // Skip the whole parameter list first: a default like `opts = {}` would
  // otherwise be mistaken for the body brace.
  let i = src.indexOf('(', s), pd = 0;
  for (; i < src.length; i++) {
    const c = src[i];
    if (c === '(') pd++;
    else if (c === ')') { pd--; if (!pd) { i++; break; } }
  }
  const b = src.indexOf('{', i);
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

function grabLine(needle) {
  const i = src.indexOf(needle);
  if (i < 0) throw new Error('找不到: ' + needle);
  const end = src.indexOf('\n', i);
  return src.slice(i, end).trim();
}

const fn = (n) => grabFn(n);
const dollarLine = grabLine('const $ = (sel)');

const setup = `
${dollarLine}
const state = window.__state;
const collapsedGroups = new Set();
function renderMainAgents() {}
function renderIcons() {}
function appendGroupItem(box, g) {
  const d = document.createElement('div');
  d.className = 'session-item group-item';
  d.dataset.sessionId = 'group:' + g.id;
  d.textContent = g.name;
  box.appendChild(d);
}
function openWsAddMenu() {}
function openWsMigratePop(list) { window.__migrate = list.map((s) => s.id); }
function saveCollapsedGroups() {}
function mainAgentSessions() { return (state.sessions || []).filter((s) => s.main_agent); }
function statusDot() { return 'dot'; }
function askFlagHTML() { return ''; }
function askTip() { return ''; }
function timeAgo() { return 'now'; }
${fn('shortenPath')}
${fn('sessionItemHTML')}
${fn('buildSessionRow')}
${fn('bindSessionItem')}
${fn('renderSessionList')}
window.__x = { renderSessionList };
`;

const dom = new JSDOM('<!doctype html><html><body><div id="session-list"></div></body></html>',
  { runScripts: 'outside-only', url: 'http://localhost/' });
const w = dom.window;
const $ = (s) => w.document.querySelector(s);

const state = { sessions: [], groups: [], current: null, searchQuery: '', viewGroup: 'cwd', viewSort: 'default', home: '' };
w.__state = state;
w.eval(setup);
const X = w.__x;

let failed = 0;
function check(name, cond, extra) {
  if (cond) console.log('  ok  ' + name);
  else { failed++; console.log('  FAIL ' + name + (extra ? '  →  ' + extra : '')); }
}

const s = (id, extra) => Object.assign({
  id, name: id, cwd: '/repo/a', updated_at: '2026-01-01T00:00:00Z', status: {},
}, extra);
const rows = () => [...$('#session-list').querySelectorAll('.session-item:not(.group-item)')];
const groups = () => [...$('#session-list').querySelectorAll('.session-group')];
const groupOf = (dir) => groups().find((g) => g.querySelector('.dir').textContent === dir);
const rowIds = () => rows().map((el) => el.dataset.sessionId).join(',');

console.log('主 Agent 不占列表行，子会话按自己的 cwd 归组');

state.current = null;
state.groups = [];
state.sessions = [
  s('main', { name: '主Agent', main_agent: true, updated_at: '2026-01-05T00:00:00Z' }),
  s('w1', { name: '同目录子会话', parent_id: 'main', updated_at: '2026-01-04T00:00:00Z' }),
  s('w2', { name: '异目录子会话', cwd: '/other/b', parent_id: 'main', updated_at: '2026-01-02T00:00:00Z' }),
  s('free', { name: '普通会话', updated_at: '2026-01-03T00:00:00Z' }),
  s('main2', { name: '另一个主Agent', main_agent: true, updated_at: '2026-01-01T00:00:00Z' }),
];
X.renderSessionList();

check('主 Agent 不出现在列表', !rows().some((el) => el.dataset.sessionId === 'main' || el.dataset.sessionId === 'main2'), rowIds());
check('按自己的 cwd 落进两个项目组', groups().length === 2, String(groups().length));
check('/repo/a = 同目录子会话 + 普通会话',
  groupOf('/repo/a') && groupOf('/repo/a').querySelectorAll('.session-item:not(.group-item)').length === 2);
check('/other/b = 异目录子会话',
  groupOf('/other/b') && groupOf('/other/b').querySelectorAll('.session-item').length === 1);
check('组内顺序按 updated_at 倒序',
  [...groupOf('/repo/a').querySelectorAll('.session-item:not(.group-item)')].map((el) => el.dataset.sessionId).join(',') === 'w1,free');
check('不再有嵌套缩进/连接线',
  !$('#session-list').querySelector('.session-worker') && rows().every((el) => !el.style.getPropertyValue('--depth')));
check('不再有目录小标', !$('#session-list').querySelector('.worker-cwd'));

const w1row = rows().find((el) => el.dataset.sessionId === 'w1');
const freeRow = rows().find((el) => el.dataset.sessionId === 'free');
check('Worker 带机器人图标', !!w1row.querySelector('.sess-worker-mark i[data-lucide="bot"]') && w1row.querySelector('.sess-worker-mark').getAttribute('aria-label') === 'Worker');
check('普通会话没有 Worker 图标', !freeRow.querySelector('.sess-worker-mark'));
check('Worker tooltip 写明由谁创建', w1row.title.includes('Worker · 由 主Agent 创建'), w1row.title);
check('普通会话 tooltip 不带 Worker 说明', !freeRow.title.includes('Worker'), freeRow.title);

check('分组计数 = 本组会话数',
  groupOf('/repo/a').querySelector('.count').textContent === '2' &&
  groupOf('/other/b').querySelector('.count').textContent === '1',
  groupOf('/repo/a').querySelector('.count').textContent + '/' + groupOf('/other/b').querySelector('.count').textContent);

w.__migrate = null;
groupOf('/repo/a').querySelector('.group-migrate-btn').onclick({ stopPropagation() {}, currentTarget: groupOf('/repo/a').querySelector('.group-migrate-btn') });
check('迁移按自己的 cwd 收集（含子会话，不含异目录的）',
  (w.__migrate || []).join(',') === 'w1,free', (w.__migrate || []).join(','));

console.log('群聊仍在同一工作区分组，但不进迁移集合');
state.groups = [{ id: 'g1', name: '讨论组', cwd: '/repo/a', updated_at: '2026-01-06T00:00:00Z' }];
X.renderSessionList();
check('群聊与会话同组',
  !!groupOf('/repo/a').querySelector('.session-item.group-item') &&
  groupOf('/repo/a').querySelector('.count').textContent === '3');
w.__migrate = null;
groupOf('/repo/a').querySelector('.group-migrate-btn').onclick({ stopPropagation() {}, currentTarget: groupOf('/repo/a').querySelector('.group-migrate-btn') });
check('群聊不参与工作区迁移', (w.__migrate || []).join(',') === 'w1,free', (w.__migrate || []).join(','));

console.log('父不存在 / parent_id 成环');
state.groups = [];
state.sessions = [
  s('orphan', { parent_id: 'gone' }),
  s('a', { parent_id: 'b' }),
  s('b', { parent_id: 'a' }),
];
X.renderSessionList();
check('不卡死、全部平铺', rowIds() === 'orphan,a,b', rowIds());
check('找不到父会话也带 Worker 图标', rows().every((el) => !!el.querySelector('.sess-worker-mark')));

console.log('平铺视图');
state.viewGroup = 'none';
state.searchQuery = '';
state.sessions = [
  s('main', { name: '主Agent', main_agent: true, updated_at: '2026-01-05T00:00:00Z' }),
  s('w1', { name: '子会话A', parent_id: 'main', updated_at: '2026-01-04T00:00:00Z' }),
  s('free', { name: '普通会话', updated_at: '2026-01-03T00:00:00Z' }),
];
X.renderSessionList();
check('平铺视图不渲染主 Agent', rowIds() === 'w1,free', rowIds());
check('平铺视图子会话不缩进', !$('#session-list').querySelector('.session-worker'));
check('平铺视图仍带 Worker 图标', !!rows()[0].querySelector('.sess-worker-mark i[data-lucide="bot"]'));
state.viewGroup = 'cwd';

console.log('只剩主 Agent 时的空态');
state.sessions = [s('main', { name: '主Agent', main_agent: true })];
X.renderSessionList();
check('引导去顶部点头像，而不是「暂无会话」',
  $('#session-list').textContent.includes('主 Agent 已在顶部') && $('#session-list').textContent.includes('Worker'));

console.log('搜索');
state.searchQuery = '主Agent';
state.sessions = [
  s('main', { name: '主Agent', main_agent: true }),
  s('w1', { name: '子会话A', parent_id: 'main' }),
  s('free', { name: '普通会话' }),
];
X.renderSessionList();
check('搜索命中主 Agent 名称也不出现在列表', rows().length === 0, rowIds());
check('给出无匹配提示', $('#session-list').textContent.includes('没有匹配的会话'));
state.searchQuery = '子会话A';
X.renderSessionList();
check('搜索仍能命中 Worker 且带图标', rowIds() === 'w1' && !!rows()[0].querySelector('.sess-worker-mark'), rowIds());
state.searchQuery = '';

if (failed) { console.log(`\n${failed} 项失败`); process.exit(1); }
console.log('\n全部通过');
