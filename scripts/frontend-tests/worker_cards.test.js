// worker 会话右侧「绑定卡片」面板的 DOM 契约回归。
//
// 面板只在 worker 会话（非 main_agent）显示：
//   ① 默认收起成窄轨（图标 + 卡片数 + 竖排标题），点开展开为面板；
//   ② 只显示绑定到当前会话的看板卡（task.session / task.worker 两种匹配）；
//   ③ 多张卡走手风琴：默认只展开最近更新的一张，同一时间最多一张；
//   ④ 展开态内联显示任务 ID / 状态 / progress / last_event / 标签 / 产物；
//   ⑤ 产物显示标题 + 类型/大小/时间，有 URL 的可点开（html 换同源视图地址，
//      url 保持分享链接），legacy report_file 不可点，0 个产物显示「—」；
//   ⑥ 展开状态与选中卡片按会话存 localStorage，切换会话互不影响；
//   ⑦ 展开时订阅 /api/agent-board/events，收起/主 Agent 会话断开；
//   ⑧ 零张卡显示空状态；主 Agent 会话整块隐藏。
//
// 局限：jsdom 不做布局。340px 宽度、窄屏覆盖层、遮罩、过渡手感验不了，
// 这里钉的只有 DOM 契约、匹配规则和「什么时候连 SSE / 什么时候不连」。
const fs = require('fs');
const path = require('path');
const { JSDOM } = require('jsdom');

const WEB = path.resolve(__dirname, '../../web');
const src = fs.readFileSync(path.join(WEB, 'app.js'), 'utf8');

// 按大括号配对原样摘出顶层函数（处理转义、字符串、模板串、注释、正则），
// 执行的是真代码而不是抄的副本。
function grabFn(name) {
  const s = src.indexOf('function ' + name + '(');
  if (s < 0) throw new Error('找不到函数 ' + name);
  const start = src.slice(Math.max(0, s - 6), s) === 'async ' ? s - 6 : s;
  const b = src.indexOf('{', s);
  let d = 0, q = null;
  for (let i = b; i < src.length; i++) {
    const c = src[i];
    if (q) {
      if (c === '\\') { i++; continue; }
      if (c === q) q = null;
      continue;
    }
    if (c === '"' || c === "'" || c === '`') { q = c; continue; }
    if (c === '/' && src[i + 1] === '/') { while (i < src.length && src[i] !== '\n') i++; continue; }
    if (c === '/' && src[i + 1] === '*') { const e = src.indexOf('*/', i + 2); i = e < 0 ? src.length : e + 1; continue; }
    if (c === '/') {
      const prev = src.slice(b, i).trimEnd().slice(-1);
      if (prev === '' || '(,=:[!&|?{};+-*%^~<>'.includes(prev)) {
        let j = i + 1, cls = false;
        while (j < src.length) {
          if (src[j] === '\\') { j += 2; continue; }
          if (src[j] === '[') cls = true; else if (src[j] === ']') cls = false;
          else if (src[j] === '/' && !cls) break; else if (src[j] === '\n') break;
          j++;
        }
        i = j; continue;
      }
    }
    if (c === '{') d++;
    else if (c === '}') { d--; if (!d) return src.slice(start, i + 1); }
  }
  throw new Error(name + ' 括号不配对');
}

function grabConst(name) {
  const m = src.match(new RegExp('const ' + name + '\\s*=\\s*[^;\\n]+;'));
  if (!m) throw new Error('找不到常量 ' + name);
  return m[0];
}

const HTML = `<!doctype html><html><body>
  <div id="chat-shell">
    <div id="chat-view"></div>
    <aside id="worker-cards" class="hidden"></aside>
  </div>
  <div id="worker-cards-mask" class="wc-mask hidden"></div>
</body></html>`;

const dom = new JSDOM(HTML, { url: 'http://localhost/', runScripts: 'outside-only' });
const w = dom.window;
const doc = w.document;

w.eval(`
window.__esList = [];
window.__apiCalls = [];
window.__nextBoard = { tasks: [] };
window.__iconCalls = 0;
const $ = (sel) => document.querySelector(sel);
const state = {
  agentBoard: null, agentBoardLoading: false, mainAgentBoardAt: 0,
  current: null, workerCardsEs: null, workerCardsSid: null,
};
function renderIcons() { window.__iconCalls++; }
async function api(path) {
  window.__apiCalls.push(path);
  return window.__nextBoard;
}
class FakeEventSource {
  constructor(url) { this.url = url; this.listeners = {}; this.closed = false; window.__esList.push(this); }
  addEventListener(type, fn) { (this.listeners[type] = this.listeners[type] || []).push(fn); }
  close() { this.closed = true; }
}
window.EventSource = FakeEventSource;
${grabConst('WC_OPEN_PREFIX')}
${grabConst('WC_CARD_PREFIX')}
${grabConst('WC_CARD_NONE')}
${grabFn('wcStorageGet')}
${grabFn('wcStorageSet')}
${grabFn('wcIsOpen')}
${grabFn('wcSetOpen')}
${grabFn('wcBoundTasks')}
${grabFn('wcSelectedTaskId')}
${grabFn('wcSetSelectedTaskId')}
${grabFn('wcStatusClass')}
${grabFn('wcCountPill')}
${grabFn('wcRail')}
${grabFn('wcHead')}
${grabFn('wcEmpty')}
${grabFn('wcRow')}
${grabFn('boardArtifactSize')}
${grabFn('boardArtifactKindLabel')}
${grabFn('boardArtifactViewURL')}
${grabFn('wcArtifactTime')}
${grabFn('wcArtifactItem')}
${grabFn('wcArtifactsRow')}
${grabFn('wcTaskDetail')}
${grabFn('wcTaskItem')}
${grabFn('wcRender')}
${grabFn('wcDisconnect')}
${grabFn('wcConnect')}
${grabFn('wcRefresh')}
${grabFn('wcOnSession')}
window.__state = state;
window.__wc = {
  wcIsOpen, wcSetOpen, wcBoundTasks, wcSelectedTaskId, wcSetSelectedTaskId,
  wcStatusClass, wcRender, wcDisconnect, wcConnect, wcRefresh, wcOnSession,
  WC_CARD_NONE,
};
`);

const state = w.__state;
const wc = w.__wc;
const panel = () => doc.querySelector('#worker-cards');
const mask = () => doc.querySelector('#worker-cards-mask');

let failed = 0;
function check(name, cond, extra) {
  if (cond) console.log('  ok  ' + name);
  else { failed++; console.log('  FAIL ' + name + (extra ? '  →  ' + extra : '')); }
}

const TASKS = [
  { id: 'T-1', session: 's1', worker: 'w1', title: '实现面板', status: '进行中', progress: '60%', last_event: '刚更新了 CSS', updated_at: '2026-11-17 10:00', tags: ['前端', '面板'] },
  { id: 'T-2', session: '', worker: 'w1', title: '补测试', status: '待人工', progress: '30%', last_event: '等待确认', updated_at: '2026-11-17 11:00', tags: ['测试'],
    artifacts: [
      { id: 'A-1', kind: 'html', title: '排查报告', filename: 'report.html', size: 19624, created_at: '2026-11-17 10:30:00', url: 'http://localhost/api/agent-board/artifacts/A-1' },
      { id: 'A-2', kind: 'url', title: '分享链接', filename: 'share.url', size: 0, created_at: '2026-11-17 10:40:00', url: 'https://example.com/s/abc' },
      { id: 'A-3', kind: 'html', title: '跨域报告', filename: 'cross.html', size: 2048, created_at: '2026-11-17 10:35:00', url: 'http://127.0.0.1:8321/api/agent-board/artifacts/A-3' },
      { id: 'legacy-report', kind: 'file', title: '旧报告', filename: 'report.md', legacy: true, url: '', storage_path: '/tmp/report.md', created_at: '2026-11-16 09:00:00' },
    ] },
  { id: 'T-3', session: 's2', worker: 'other', title: '别的任务', status: '新建', updated_at: '2026-11-17 12:00' },
  { id: 'T-4', session: '', worker: 'alice', title: '按名字绑定', status: '已完成', updated_at: '2026-11-17 09:00' },
];

function resetStorage() {
  w.localStorage.clear();
  w.__apiCalls = [];
  w.__esList = [];
  state.workerCardsEs = null;
  state.workerCardsSid = null;
  state.agentBoard = { tasks: TASKS };
  state.agentBoardLoading = false;
  state.mainAgentBoardAt = Date.now();
}

async function main() {
  console.log('worker 绑定卡片面板');

  // ---- 匹配规则 ----
  resetStorage();
  const s1 = { id: 's1', peer_name: 'w1', name: 'alice' };
  let bound = wc.wcBoundTasks(s1);
  check('按 session / 地址名 / 会话名三种键匹配',
    JSON.stringify(bound.map((t) => t.id)) === JSON.stringify(['T-2', 'T-1', 'T-4']),
    JSON.stringify(bound.map((t) => t.id)));
  check('最近更新的排在前面',
    bound[0].id === 'T-2' && bound[1].id === 'T-1');
  check('别的会话的卡不进来', !bound.some((t) => t.id === 'T-3'));
  check('main_agent 不匹配任何卡', wc.wcBoundTasks({ id: 's1', main_agent: true, peer_name: 'w1' }).length === 0);

  // ---- 默认收起成窄轨 ----
  resetStorage();
  state.current = s1;
  wc.wcOnSession(s1);
  check('worker 会话显示面板', !panel().classList.contains('hidden'));
  check('默认是收起态', panel().classList.contains('wc-collapsed'));
  check('窄轨显示绑定数量', panel().querySelector('.wc-rail-count').textContent === '3');
  check('窄轨带展开图标', !!panel().querySelector('[data-lucide="panel-right-open"]'));
  check('窄轨有竖排标题', panel().querySelector('.wc-rail-label').textContent === '绑定卡片');
  check('收起时不连 SSE', state.workerCardsEs === null);
  check('收起时遮罩隐藏', mask().classList.contains('hidden'));

  // ---- 点窄轨展开 ----
  panel().querySelector('.wc-rail').click();
  check('展开后不再折叠', !panel().classList.contains('wc-collapsed'));
  check('展开状态写入 localStorage', w.localStorage.getItem('cc-webui:wc-open:s1') === '1');
  check('头部显示「绑定卡片」+ 数量',
    panel().querySelector('.wc-head-title').textContent.replace(/\s+/g, '') === '绑定卡片3');
  check('展开后连上看板 SSE', !!state.workerCardsEs && state.workerCardsEs.url === '/api/agent-board/events');
  check('展开时遮罩交给 CSS（不隐藏）', !mask().classList.contains('hidden'));

  let cards = [...panel().querySelectorAll('.wc-card')];
  check('渲染出三张卡', cards.length === 3, String(cards.length));
  const openCards = [...panel().querySelectorAll('.wc-card.wc-open')];
  check('默认只展开最近更新的一张', openCards.length === 1 && openCards[0].dataset.taskId === 'T-2');

  const collapsedCard = cards.find((c) => c.dataset.taskId === 'T-1');
  check('折叠卡显示标题', collapsedCard.querySelector('.wc-card-title').textContent === '实现面板');
  check('折叠卡显示状态', collapsedCard.querySelector('.wc-status').textContent === '进行中');
  check('折叠卡显示最近事件', collapsedCard.querySelector('.wc-card-last').textContent === '刚更新了 CSS');
  check('折叠卡不渲染详情', !collapsedCard.querySelector('.wc-card-body'));

  const openCard = openCards[0];
  const detailText = openCard.querySelector('.wc-card-body').textContent;
  check('展开卡显示任务 ID', detailText.includes('T-2'));
  check('展开卡显示状态', detailText.includes('待人工'));
  check('展开卡显示 progress', detailText.includes('30%'));
  check('展开卡显示最近事件', detailText.includes('等待确认'));
  check('展开卡显示标签', [...openCard.querySelectorAll('.wc-card-body .board-tag')].map((t) => t.textContent).join(',') === '测试');

  // ---- 产物：标题 / 类型 / 大小 / 时间，有 URL 的可点开 ----
  check('详情里有产物行', openCard.querySelectorAll('.wc-card-body .wc-artifacts').length === 1);
  const artifactItems = [...openCard.querySelectorAll('.wc-artifact')];
  check('列出全部产物', artifactItems.length === 4, String(artifactItems.length));
  const htmlArt = artifactItems.find((el) => el.textContent.includes('排查报告'));
  check('HTML 产物显示标题 + 类型/大小/时间',
    htmlArt && htmlArt.querySelector('.wc-artifact-title').textContent === '排查报告' &&
    htmlArt.querySelector('.wc-artifact-meta').textContent === 'HTML · 19.2 KB · 11-17 10:30',
    htmlArt && htmlArt.querySelector('.wc-artifact-meta').textContent);
  check('HTML 产物可点开（同源视图地址）',
    htmlArt && htmlArt.tagName === 'A' &&
    htmlArt.getAttribute('href') === 'http://localhost/api/agent-board/artifacts/A-1');
  check('新标签打开且断 opener',
    htmlArt && htmlArt.target === '_blank' && htmlArt.rel === 'noopener');
  const crossArt = artifactItems.find((el) => el.textContent.includes('跨域报告'));
  check('托管产物跨 origin 时换成当前页面 origin',
    crossArt && crossArt.getAttribute('href') === 'http://localhost/api/agent-board/artifacts/A-3',
    crossArt && crossArt.getAttribute('href'));
  const urlArt = artifactItems.find((el) => el.dataset.kind === 'url');
  check('url 产物保持分享链接', urlArt && urlArt.tagName === 'A' &&
    urlArt.getAttribute('href') === 'https://example.com/s/abc');
  check('url 产物类型显示为链接',
    urlArt && urlArt.querySelector('.wc-artifact-kind').textContent === '链接');
  const legacyArt = artifactItems.find((el) => el.textContent.includes('旧报告'));
  check('legacy 产物不可点', legacyArt && legacyArt.tagName === 'SPAN' && !legacyArt.hasAttribute('href'));
  check('legacy 产物标注未托管路径',
    legacyArt && legacyArt.querySelector('.wc-artifact-meta').textContent.includes('未托管路径'));

  // ---- 手风琴：点另一张切换，同一时间只有一张 ----
  cards.find((c) => c.dataset.taskId === 'T-1').querySelector('.wc-card-head').click();
  cards = [...panel().querySelectorAll('.wc-card')];
  let opens = [...panel().querySelectorAll('.wc-card.wc-open')];
  check('点另一张后只展开它', opens.length === 1 && opens[0].dataset.taskId === 'T-1');
  check('选中卡片按会话存 localStorage', w.localStorage.getItem('cc-webui:wc-card:s1') === 'T-1');
  check('切卡后详情跟着换', opens[0].querySelector('.wc-card-body').textContent.includes('实现面板') === false &&
    opens[0].querySelector('.wc-card-body').textContent.includes('60%'));
  const emptyArtifactRow = opens[0].querySelector('.wc-card-body .wc-artifacts');
  check('无产物显示「—」', !!emptyArtifactRow &&
    emptyArtifactRow.querySelector('.wc-row-val').textContent === '—');

  // 点当前展开的卡 → 全部收起（存哨兵值，刷新后不会又自动展开第一张）
  opens[0].querySelector('.wc-card-head').click();
  check('点开着的卡可以全部收起', panel().querySelectorAll('.wc-card.wc-open').length === 0);
  check('显式收起存哨兵值', w.localStorage.getItem('cc-webui:wc-card:s1') === wc.WC_CARD_NONE);
  wc.wcRender(s1);
  check('重新渲染后保持全部收起', panel().querySelectorAll('.wc-card.wc-open').length === 0);

  // ---- 按会话隔离持久化 ----
  const s2 = { id: 's2', peer_name: 'other', name: 'other' };
  state.current = s2;
  wc.wcOnSession(s2);
  w.localStorage.setItem('cc-webui:wc-open:s2', '1');
  wc.wcRender(s2);
  const s2Open = [...panel().querySelectorAll('.wc-card.wc-open')];
  check('另一个会话默认展开它自己的最近一张',
    s2Open.length === 1 && s2Open[0].dataset.taskId === 'T-3');
  check('s1 的手风琴选择不影响 s2',
    w.localStorage.getItem('cc-webui:wc-card:s2') === null);

  // ---- 空状态 ----
  const s3 = { id: 's3', peer_name: 'nobody', name: 'nobody' };
  state.current = s3;
  w.localStorage.setItem('cc-webui:wc-open:s3', '1');
  wc.wcOnSession(s3);
  check('零张卡显示空状态', panel().querySelector('.wc-empty-title').textContent === '暂无绑定卡片');
  check('空状态提示绑定对象', panel().querySelector('.wc-empty-hint').textContent.includes('nobody'));
  check('空状态数量为 0', panel().querySelector('.wc-count').textContent === '0');

  // ---- 主 Agent 会话：整块隐藏并断开 SSE ----
  const prevEs = state.workerCardsEs;
  state.current = { id: 'main', main_agent: true, peer_name: 'mainAgent', name: 'mainAgent' };
  wc.wcOnSession(state.current);
  check('主 Agent 会话隐藏面板', panel().classList.contains('hidden'));
  check('主 Agent 会话清空内容', panel().innerHTML === '');
  check('主 Agent 会话断开 SSE', state.workerCardsEs === null && (!prevEs || prevEs.closed));
  check('主 Agent 会话隐藏遮罩', mask().classList.contains('hidden'));

  // ---- 展开时 SSE 推送会重拉快照并重绘 ----
  resetStorage();
  state.current = s1;
  w.localStorage.setItem('cc-webui:wc-open:s1', '1');
  wc.wcOnSession(s1);
  const es = state.workerCardsEs;
  check('展开的 worker 会话有 SSE', !!es);
  w.__nextBoard = {
    tasks: [
      { id: 'T-2', session: 's1', worker: 'w1', title: '补测试（新）', status: '已完成', updated_at: '2026-11-17 13:00' },
      { id: 'T-9', session: 's1', worker: 'w1', title: '新绑定的卡', status: '新建', updated_at: '2026-11-17 14:00',
        artifacts: [{ id: 'A-9', kind: 'html', title: '刷新后的产物', size: 10, created_at: '2026-11-17 14:00:00', url: 'http://localhost/api/agent-board/artifacts/A-9' }] },
    ],
  };
  w.__apiCalls = [];
  es.listeners.board[0]();
  await new Promise((r) => setTimeout(r, 0));
  check('看板 SSE 触发重新拉取快照', w.__apiCalls.includes('/api/agent-board'));
  check('刷新后卡片列表更新', [...panel().querySelectorAll('.wc-card')].map((c) => c.dataset.taskId).join(',') === 'T-9,T-2');
  check('刷新后默认展开最新那张', panel().querySelector('.wc-card.wc-open').dataset.taskId === 'T-9');
  check('刷新后产物跟着更新',
    panel().querySelector('.wc-card.wc-open .wc-artifact-title').textContent === '刷新后的产物');

  // ---- 收起面板断开 SSE ----
  const liveEs = state.workerCardsEs;
  panel().querySelector('.wc-collapse').click();
  check('收起后面板回到窄轨', panel().classList.contains('wc-collapsed'));
  check('收起后断开 SSE', state.workerCardsEs === null && liveEs.closed);
  check('收起后遮罩隐藏', mask().classList.contains('hidden'));
  check('收起状态写 localStorage', w.localStorage.getItem('cc-webui:wc-open:s1') === '0');

  // ---- 状态映射 ----
  check('状态色映射', wc.wcStatusClass('进行中') === 'active' &&
    wc.wcStatusClass('待人工') === 'warn' && wc.wcStatusClass('待验收') === 'warn' &&
    wc.wcStatusClass('已完成') === 'done' && wc.wcStatusClass('已取消') === 'muted');

  console.log(failed ? `\n${failed} 项失败` : '\n全部通过');
  process.exit(failed ? 1 : 0);
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
