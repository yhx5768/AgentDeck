// 侧栏「worker 挂在主 Agent 下」的渲染回归。
//
// 契约：parent_id 指向的会话把子会话嵌进自己的行下面（缩进 + session-worker），
// **与子会话自己的 cwd 无关** —— 归属是「谁派的活」。子会话在别的目录时，行尾
// 加一个 .worker-cwd 目录小标，并且不跟着父会话的工作区一起「迁移」。
//
// 局限：jsdom 不做布局，缩进/连接线好不好看验不了；这里钉的是 DOM 契约
//（嵌套顺序、缩进类、目录小标、迁移集合、环路不卡死、父被过滤掉的降级）。
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
function statusDot() { return 'dot'; }
function askFlagHTML() { return ''; }
function askTip() { return ''; }
function timeAgo() { return 'now'; }
${fn('shortenPath')}
${fn('isSessionAncestor')}
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

console.log('worker 挂在主 Agent 下（含异目录）');

state.current = null;
state.sessions = [
  s('main', { name: '主Agent', updated_at: '2026-01-02T00:00:00Z' }),
  s('w1', { parent_id: 'main' }),
  s('w2', { cwd: '/other/b', parent_id: 'main' }),
  s('free'),
];
X.renderSessionList();

let r = rows();
check('顺序：主 Agent → 两个 worker → 独立会话',
  r.map((el) => el.dataset.sessionId).join(',') === 'main,w1,w2,free',
  r.map((el) => el.dataset.sessionId).join(','));
check('worker 带缩进类', r[1].classList.contains('session-worker') && r[2].classList.contains('session-worker'));
check('主 Agent / 独立会话不缩进', !r[0].classList.contains('session-worker') && !r[3].classList.contains('session-worker'));
check('同目录 worker 没有目录小标', !r[1].querySelector('.worker-cwd'));
const badge = r[2].querySelector('.worker-cwd');
check('异目录 worker 有目录小标', !!badge && badge.textContent === '/other/b', badge && badge.textContent);
check('异目录 worker 仍挂在主 Agent 的工作区分组里（只一个 cwd 组）',
  $('#session-list').querySelectorAll('.session-group').length === 1,
  String($('#session-list').querySelectorAll('.session-group').length));

// 「迁移」只搬真正在这个目录里的会话：异目录的 worker 不能跟着搬走。
$('#session-list .group-migrate-btn').onclick({ stopPropagation() {}, currentTarget: $('.group-migrate-btn') });
check('迁移集合 = 本目录会话（排除异目录 worker）',
  (w.__migrate || []).join(',') === 'main,w1,free', (w.__migrate || []).join(','));

console.log('降级与健壮性');

state.sessions = [s('w1', { parent_id: 'gone' })];
X.renderSessionList();
r = rows();
check('父被过滤掉 → 子降级为顶层', r.length === 1 && !r[0].classList.contains('session-worker'));

state.sessions = [
  s('a', { parent_id: 'b', updated_at: '2026-01-02T00:00:00Z' }),
  s('b', { parent_id: 'a' }),
];
X.renderSessionList();
r = rows();
check('parent_id 成环不会卡死，两行都按顶层渲染',
  r.length === 2 && r.every((el) => !el.classList.contains('session-worker')), String(r.length));

console.log('平铺模式也嵌套');
state.viewGroup = 'none';
state.sessions = [
  s('main', { name: '主Agent', updated_at: '2026-01-02T00:00:00Z' }),
  s('w1', { parent_id: 'main' }),
];
state.viewGroup = 'none';
X.renderSessionList();
r = [...$('#session-list').querySelectorAll('.session-item:not(.group-item)')];
check('平铺模式：worker 紧跟主 Agent 且带缩进',
  r.map((el) => el.dataset.sessionId).join(',') === 'main,w1' && r[1].classList.contains('session-worker'),
  r.map((el) => el.dataset.sessionId).join(','));

if (failed) { console.log(`\n${failed} 项失败`); process.exit(1); }
console.log('\n全部通过');
