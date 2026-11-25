// worker 会话右侧「绑定卡片」面板的 DOM 契约回归。
//
// 面板只在 worker 会话（非 main_agent）显示：
//   ① 默认收起成窄轨（图标 + 卡片数 + 竖排标题），点开展开成竖向卡片列表；
//   ② 只显示绑定到当前会话的看板卡（task.session / task.worker 两种匹配）；
//   ③ 点任意卡片进入「完整卡片详情」页：状态/基础信息/标签/产物/任务流 +
//      回复主 Agent 输入框 + 验收通过按钮，左上角返回按钮回到三卡列表；
//   ④ 产物可打开；托管产物可分享（创建公开链接弹窗），url 产物可直接复制
//      分享链接；legacy 不可点；0 个产物显示「—」；
//   ⑤ 回复/验收都 POST 到主 Agent 会话并带 board_task_id（不直接写卡片）；
//      主 Agent 轮次结束后出现新回复时提示「已回复」并高亮；
//   ⑥ 展开时订阅 /api/agent-board/events，收起/主 Agent 会话断开；SSE 刷新
//      保持详情页与回复草稿；
//   ⑦ 零张卡显示空状态；主 Agent 会话整块隐藏。
//
// 局限：jsdom 不做布局。340px 宽度、窄屏覆盖层、遮罩、过渡手感验不了，
// 这里钉的只有 DOM 契约、匹配规则、「什么时候连 SSE」和发消息的目标会话。
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
  <div id="board-share-modal" class="hidden"></div>
  <div id="board-drawer" class="board-drawer"></div>
</body></html>`;

const dom = new JSDOM(HTML, { url: 'http://localhost/', runScripts: 'outside-only' });
const w = dom.window;
const doc = w.document;

w.eval(`
window.__esList = [];
window.__apiCalls = [];
window.__apiLast = null;
window.__nextBoard = { tasks: [] };
window.__userEvents = [];
window.__shared = [];
window.__copied = [];
window.__iconCalls = 0;
const $ = (sel) => document.querySelector(sel);
const state = {
  sessions: [{ id: 'main-1', main_agent: true, peer_name: 'mainAgent', name: '主 Agent',
    resident: true, updated_at: '2026-11-17T12:00:00Z' }],
  current: null,
  agentBoard: null, agentBoardLoading: false, mainAgentBoardAt: 0,
  workerCardsEs: null, workerCardsSid: null,
  wcDetail: { sid: '', taskId: '' },
  wcReply: { taskId: '', draft: '', notice: null, wait: null, freshKey: '' },
};
function renderIcons() { window.__iconCalls++; }
async function api(path, opts) {
  window.__apiCalls.push(path);
  window.__apiLast = { path, opts: opts || {} };
  if (opts && opts.method === 'POST' && path !== '/api/agent-board') return { ok: true };
  return window.__nextBoard;
}
async function boardAppendUserEvent(taskId, text) {
  window.__userEvents.push({ taskId, text });
}
function boardOpenShareDialog(artifact) { window.__shared.push(artifact); }
function boardCopyShareURL(url, button) {
  window.__copied.push(url);
  if (button) button.textContent = '已复制';
}
function md(text) {
  const esc = String(text == null ? '' : text)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
  return '<p>' + esc + '</p>';
}
class FakeEventSource {
  constructor(url) { this.url = url; this.listeners = {}; this.closed = false; window.__esList.push(this); }
  addEventListener(type, fn) { (this.listeners[type] = this.listeners[type] || []).push(fn); }
  close() { this.closed = true; }
}
window.EventSource = FakeEventSource;
${grabConst('WC_OPEN_PREFIX')}
${grabFn('wcStorageGet')}
${grabFn('wcStorageSet')}
${grabFn('wcIsOpen')}
${grabFn('wcSetOpen')}
${grabFn('wcBoundTasks')}
${grabFn('wcDetailTaskId')}
${grabFn('wcReplyReset')}
${grabFn('wcOpenDetail')}
${grabFn('wcCloseDetail')}
${grabFn('wcMainAgentSession')}
${grabFn('mainAgentSessions')}
${grabFn('wcStatusClass')}
${grabFn('wcCountPill')}
${grabFn('wcRail')}
${grabFn('wcCollapseButton')}
${grabFn('wcHead')}
${grabFn('wcDetailHead')}
${grabFn('wcEmpty')}
${grabFn('wcRow')}
${grabFn('boardArtifactSize')}
${grabFn('boardArtifactKindLabel')}
${grabFn('boardArtifactViewURL')}
${grabFn('boardStatusCallout')}
${grabFn('boardTaskEvents')}
${grabFn('boardReplyEvents')}
${grabFn('boardReplyKey')}
${grabFn('boardReplyKeys')}
${grabFn('boardTaskContext')}
${grabFn('wcArtifactTime')}
${grabFn('wcArtifactItem')}
${grabFn('wcDetailSection')}
${grabFn('wcArtifactsSection')}
${grabFn('wcTimelineEvent')}
${grabFn('wcTimelineSection')}
${grabFn('wcReplySync')}
${grabFn('wcDetailSetNotice')}
${grabFn('wcTaskDetail')}
${grabFn('wcSendToMain')}
${grabFn('wcReplySend')}
${grabFn('wcAcceptTask')}
${grabFn('wcDetailFooter')}
${grabFn('wcTaskItem')}
${grabFn('wcRender')}
${grabFn('wcDisconnect')}
${grabFn('wcConnect')}
${grabFn('wcRefresh')}
${grabFn('wcOnSession')}
${grabFn('wcCollapse')}
window.__state = state;
window.__wc = {
  wcIsOpen, wcSetOpen, wcBoundTasks, wcOpenDetail, wcCloseDetail, wcMainAgentSession,
  wcStatusClass, wcRender, wcDisconnect, wcConnect, wcRefresh, wcOnSession,
  wcReplySend, wcAcceptTask, wcDetailTaskId, wcCollapse,
};
`);

const state = w.__state;
const wc = w.__wc;
const panel = () => doc.querySelector('#worker-cards');
const mask = () => doc.querySelector('#worker-cards-mask');

let failed = 0;
function check(name, cond, extra) {
  if (cond) console.log('  ok  ' + name);
  else { failed++; console.log('  FAIL ' + name + (extra !== undefined ? '  →  ' + JSON.stringify(extra) : '')); }
}

const TASKS = [
  { id: 'T-1', session: 's1', worker: 'w1', title: '实现面板', status: '进行中', progress: '60%',
    last_event: '刚更新了 CSS', updated_at: '2026-11-17 10:00', tags: ['前端', '面板'],
    timeline: [{ at: '2026-11-17 10:00', kind: '最近进度', text: '刚更新了 CSS' }] },
  { id: 'T-2', session: '', worker: 'w1', title: '补测试', status: '待验收', progress: '30%',
    last_event: '等待确认', updated_at: '2026-11-17 11:00', tags: ['测试'],
    artifacts: [
      { id: 'A-1', kind: 'html', title: '排查报告', filename: 'report.html', size: 19624,
        created_at: '2026-11-17 10:30:00', url: 'http://localhost/api/agent-board/artifacts/A-1' },
      { id: 'A-2', kind: 'url', title: '分享链接', filename: 'share.url', size: 0,
        created_at: '2026-11-17 10:40:00', url: 'https://example.com/s/abc' },
      { id: 'A-3', kind: 'html', title: '跨域报告', filename: 'cross.html', size: 2048,
        created_at: '2026-11-17 10:35:00', url: 'http://127.0.0.1:8321/api/agent-board/artifacts/A-3' },
      { id: 'legacy-report', kind: 'file', title: '旧报告', filename: 'report.md', legacy: true,
        url: '', storage_path: '/tmp/report.md', created_at: '2026-11-16 09:00:00' },
    ],
    timeline: [
      { at: '2026-11-17 11:00', kind: '主 Agent 回复', text: '**第一版报告**已发布', data: { turn_id: 't-1' } },
      { at: '2026-11-17 10:50', kind: '用户消息', text: '继续' },
    ] },
  { id: 'T-3', session: 's2', worker: 'other', title: '别的任务', status: '新建', updated_at: '2026-11-17 12:00' },
  { id: 'T-4', session: '', worker: 'alice', title: '按名字绑定', status: '已完成', updated_at: '2026-11-17 09:00' },
];

function resetStorage() {
  w.localStorage.clear();
  w.__apiCalls = [];
  w.__apiLast = null;
  w.__esList = [];
  w.__userEvents = [];
  w.__shared = [];
  w.__copied = [];
  state.workerCardsEs = null;
  state.workerCardsSid = null;
  state.agentBoard = { tasks: TASKS };
  state.agentBoardLoading = false;
  state.mainAgentBoardAt = Date.now();
  state.wcDetail = { sid: '', taskId: '' };
  state.wcReply = { taskId: '', draft: '', notice: null, wait: null, freshKey: '' };
  state.sessions = [{ id: 'main-1', main_agent: true, peer_name: 'mainAgent', name: '主 Agent',
    resident: true, updated_at: '2026-11-17T12:00:00Z' }];
}

function detailCard(taskId) {
  return panel().querySelector(`.wc-card[data-task-id="${taskId}"]`);
}
function detailBody() { return panel().querySelector('.wc-detail-body'); }

async function main() {
  console.log('worker 绑定卡片面板');

  // ---- 匹配规则 ----
  resetStorage();
  const s1 = { id: 's1', peer_name: 'w1', name: 'alice' };
  let bound = wc.wcBoundTasks(s1);
  check('按 session / 地址名 / 会话名三种键匹配',
    JSON.stringify(bound.map((t) => t.id)) === JSON.stringify(['T-2', 'T-1', 'T-4']),
    JSON.stringify(bound.map((t) => t.id)));
  check('最近更新的排在前面', bound[0].id === 'T-2' && bound[1].id === 'T-1');
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

  // ---- 点窄轨展开：三卡竖向列表 ----
  panel().querySelector('.wc-rail').click();
  check('展开后不再折叠', !panel().classList.contains('wc-collapsed'));
  check('展开状态写入 localStorage', w.localStorage.getItem('cc-webui:wc-open:s1') === '1');
  check('头部显示「绑定卡片」+ 数量',
    panel().querySelector('.wc-head-title').textContent.replace(/\s+/g, '') === '绑定卡片3');
  check('展开后连上看板 SSE', !!state.workerCardsEs && state.workerCardsEs.url === '/api/agent-board/events');
  check('展开时遮罩交给 CSS（不隐藏）', !mask().classList.contains('hidden'));

  let cards = [...panel().querySelectorAll('.wc-card')];
  check('渲染出三张卡', cards.length === 3, String(cards.length));
  check('列表里没有内联详情', panel().querySelectorAll('.wc-detail-body').length === 0);
  check('折叠卡显示标题', detailCard('T-1').querySelector('.wc-card-title').textContent === '实现面板');
  check('折叠卡显示状态', detailCard('T-1').querySelector('.wc-status').textContent === '进行中');
  check('折叠卡显示最近事件', detailCard('T-1').querySelector('.wc-card-last').textContent === '刚更新了 CSS');

  // ---- 点卡片进入完整详情页 ----
  detailCard('T-2').querySelector('.wc-card-head').click();
  check('详情页替换卡片列表', panel().querySelectorAll('.wc-card').length === 0);
  check('左上角有返回按钮',
    !!panel().querySelector('.wc-detail-head .wc-back [data-lucide="arrow-left"]'));
  check('详情头显示「卡片详情」+ 状态',
    panel().querySelector('.wc-detail-head .wc-head-title').textContent.replace(/\s+/g, '') === '卡片详情待验收');
  const body = detailBody();
  check('详情显示标题', body.querySelector('.wc-detail-title').textContent === '补测试');
  check('详情显示状态说明', body.querySelector('.board-callout').textContent.includes('等待你验收'));
  check('详情显示基础信息',
    body.textContent.includes('T-2') && body.textContent.includes('30%') && body.textContent.includes('等待确认'));
  check('详情显示标签',
    [...body.querySelectorAll('.board-tag')].map((t) => t.textContent).join(',') === '测试');
  check('详情显示任务流（最新在前）',
    [...body.querySelectorAll('.wc-event .wc-event-kind')].map((t) => t.textContent).join(',') === '主 Agent 回复,用户消息');
  check('详情显示回复正文',
    body.querySelector('.wc-event-text').textContent.includes('第一版报告'));

  // ---- 产物：打开 / 分享 / 复制 / legacy / 空 ----
  const artifactItems = [...body.querySelectorAll('.wc-artifact')];
  check('列出全部产物', artifactItems.length === 4, String(artifactItems.length));
  const htmlArt = artifactItems.find((el) => el.textContent.includes('排查报告'));
  check('HTML 产物显示标题 + 类型/大小/时间',
    htmlArt && htmlArt.querySelector('.wc-artifact-title').textContent === '排查报告' &&
    htmlArt.querySelector('.wc-artifact-meta').textContent === 'HTML · 19.2 KB · 11-17 10:30',
    htmlArt && htmlArt.querySelector('.wc-artifact-meta').textContent);
  const htmlOpen = htmlArt && htmlArt.querySelector('.wc-artifact-open');
  check('HTML 产物可点开（同源视图地址）',
    htmlOpen && htmlOpen.tagName === 'A' &&
    htmlOpen.getAttribute('href') === 'http://localhost/api/agent-board/artifacts/A-1');
  check('新标签打开且断 opener', htmlOpen && htmlOpen.target === '_blank' && htmlOpen.rel === 'noopener');
  const htmlShare = htmlArt && htmlArt.querySelector('.wc-artifact-share');
  check('托管 HTML 产物有分享动作', htmlShare && htmlShare.textContent === '分享');
  htmlShare.click();
  check('分享动作打开创建公开链接弹窗', w.__shared.length === 1 && w.__shared[0].id === 'A-1');

  const crossArt = artifactItems.find((el) => el.textContent.includes('跨域报告'));
  const crossOpen = crossArt && crossArt.querySelector('.wc-artifact-open');
  check('托管产物跨 origin 时换成当前页面 origin',
    crossOpen && crossOpen.getAttribute('href') === 'http://localhost/api/agent-board/artifacts/A-3',
    crossOpen && crossOpen.getAttribute('href'));

  const urlArt = artifactItems.find((el) => el.dataset.kind === 'url');
  const urlOpen = urlArt && urlArt.querySelector('.wc-artifact-open');
  check('url 产物保持分享链接', urlOpen && urlOpen.getAttribute('href') === 'https://example.com/s/abc');
  check('url 产物类型显示为链接', urlArt && urlArt.querySelector('.wc-artifact-kind').textContent === '链接');
  const urlCopy = urlArt && urlArt.querySelector('.wc-artifact-share');
  check('url 产物有复制链接动作', urlCopy && urlCopy.textContent === '复制链接');
  urlCopy.click();
  check('复制动作复制产物分享链接',
    w.__copied.length === 1 && w.__copied[0] === 'https://example.com/s/abc');

  const legacyArt = artifactItems.find((el) => el.textContent.includes('旧报告'));
  check('legacy 产物不可点', legacyArt && !legacyArt.querySelector('a'));
  check('legacy 产物没有分享动作', legacyArt && !legacyArt.querySelector('.wc-artifact-share'));
  check('legacy 产物标注未托管路径',
    legacyArt && legacyArt.querySelector('.wc-artifact-meta').textContent.includes('未托管路径'));

  // ---- 返回按钮回到三卡列表 ----
  panel().querySelector('.wc-back').click();
  check('返回后回到卡片列表', panel().querySelectorAll('.wc-card').length === 3);
  check('返回后详情消失', panel().querySelectorAll('.wc-detail-body').length === 0);

  // 无产物的卡：详情里显示「—」
  detailCard('T-1').querySelector('.wc-card-head').click();
  const emptyArtifacts = panel().querySelector('.wc-artifacts');
  check('无产物显示「—」', !!emptyArtifacts && emptyArtifacts.textContent.includes('—'));
  check('进行中的卡没有验收按钮', !panel().querySelector('.wc-accept-btn'));
  panel().querySelector('.wc-back').click();

  // ---- 回复：发给主 Agent 会话，带 board_task_id ----
  detailCard('T-2').querySelector('.wc-card-head').click();
  const replyInput = panel().querySelector('#wc-reply-input');
  check('详情底部有回复输入框', !!replyInput && replyInput.disabled === false);
  check('回复目标提示是主 Agent',
    replyInput.placeholder.includes('mainAgent'), replyInput.placeholder);
  check('待验收卡有验收通过按钮', !!panel().querySelector('.wc-accept-btn'));

  replyInput.value = '请看下产物';
  replyInput.dispatchEvent(new w.Event('input'));
  await wc.wcReplySend(s1, TASKS[1], null);
  const replyCall = w.__apiLast;
  check('回复 POST 到主 Agent 会话（不是 worker / 不是卡片）',
    replyCall && replyCall.path === '/api/sessions/main-1/messages' &&
    replyCall.opts.method === 'POST', replyCall && replyCall.path);
  let replyBody = {};
  try { replyBody = JSON.parse(replyCall.opts.body); } catch (_) {}
  check('回复带 board_task_id 结构化绑定', replyBody.board_task_id === 'T-2', replyBody.board_task_id);
  check('回复正文附带卡片上下文与用户输入',
    String(replyBody.text || '').includes('来自 worker 卡片详情') &&
    String(replyBody.text || '').includes('请看下产物'));
  check('用户消息写进卡片任务流', w.__userEvents.length === 1 &&
    w.__userEvents[0].taskId === 'T-2' && w.__userEvents[0].text === '请看下产物');
  check('发送后提示等待主 Agent',
    panel().querySelector('#wc-detail-notice').textContent.includes('等待主 Agent 回复'));
  check('发送后清空草稿', state.wcReply.draft === '');
  check('前端没有直接 PATCH 卡片',
    !w.__apiCalls.some((p, i) => i > 0 && String(p).startsWith('/api/agent-board/tasks/')));

  // 主 Agent 自动回写：SSE 刷新出现新回复 → 已回复 + 高亮
  const withReply = TASKS.map((task) => task.id !== 'T-2' ? task : Object.assign({}, task, {
    timeline: [
      { at: '2026-11-17 11:10', kind: '主 Agent 回复', text: '已看，报告没问题', data: { turn_id: 't-2' } },
      ...task.timeline,
    ],
  }));
  w.__nextBoard = { tasks: withReply };
  await wc.wcRefresh('s1');
  check('SSE 刷新后仍停在详情页', panel().querySelectorAll('.wc-card').length === 0);
  check('新回复出现并高亮',
    !!panel().querySelector('.wc-event-fresh') &&
    panel().querySelector('.wc-event-fresh').textContent.includes('已看，报告没问题'));
  check('新回复后提示已回复',
    panel().querySelector('#wc-detail-notice').textContent.includes('已回复'));

  // ---- 验收通过：同样走主 Agent ----
  w.__apiCalls = [];
  w.__userEvents = [];
  w.__apiLast = null;
  panel().querySelector('.wc-accept-btn').click();
  await new Promise((r) => setTimeout(r, 0));
  const acceptCall = w.__apiLast;
  check('验收通过 POST 到主 Agent 会话',
    acceptCall && acceptCall.path === '/api/sessions/main-1/messages', acceptCall && acceptCall.path);
  let acceptBody = {};
  try { acceptBody = JSON.parse(acceptCall.opts.body); } catch (_) {}
  check('验收通过带 board_task_id', acceptBody.board_task_id === 'T-2');
  check('验收通过正文要求主 Agent 更新状态',
    String(acceptBody.text || '').includes('验收通过') && String(acceptBody.text || '').includes('已完成'));
  check('验收通过记入卡片任务流',
    w.__userEvents.length === 1 && w.__userEvents[0].text === '验收通过');
  check('验收后提示等待处理',
    panel().querySelector('#wc-detail-notice').textContent.includes('等待主 Agent 处理'));

  // ---- SSE 刷新保留正在输入的草稿 ----
  const input2 = panel().querySelector('#wc-reply-input');
  input2.value = '半截草稿';
  input2.dispatchEvent(new w.Event('input'));
  w.__nextBoard = { tasks: withReply };
  // wcRefresh 只接受当前会话；此时 current 还是 s1
  await wc.wcRefresh('s1');
  const input3 = panel().querySelector('#wc-reply-input');
  check('SSE 刷新后草稿还在', input3 && input3.value === '半截草稿');
  check('SSE 刷新后详情仍在', panel().querySelectorAll('.wc-detail-body').length === 1);

  // ---- 收起面板退出详情并断开 SSE ----
  const liveEs = state.workerCardsEs;
  panel().querySelector('.wc-collapse').click();
  check('收起后面板回到窄轨', panel().classList.contains('wc-collapsed'));
  check('收起后退出详情', wc.wcDetailTaskId(s1) === '');
  check('收起后断开 SSE', state.workerCardsEs === null && liveEs.closed);
  check('收起后遮罩隐藏', mask().classList.contains('hidden'));
  check('收起状态写 localStorage', w.localStorage.getItem('cc-webui:wc-open:s1') === '0');

  // ---- 没有主 Agent 会话：回复/验收禁用并提示 ----
  state.sessions = [];
  state.agentBoard = { tasks: TASKS };
  state.mainAgentBoardAt = Date.now();
  w.localStorage.setItem('cc-webui:wc-open:s1', '1');
  wc.wcCloseDetail();
  wc.wcOnSession(s1);
  detailCard('T-2').querySelector('.wc-card-head').click();
  const noMainInput = panel().querySelector('#wc-reply-input');
  check('无主 Agent 时回复框禁用', noMainInput.disabled === true);
  check('无主 Agent 时发送禁用', panel().querySelector('.wc-reply-send').disabled === true);
  check('无主 Agent 时验收禁用', panel().querySelector('.wc-accept-btn').disabled === true);
  check('无主 Agent 时说明原因',
    panel().querySelector('#wc-detail-notice').textContent.includes('未找到主 Agent 会话'));
  state.sessions = [{ id: 'main-1', main_agent: true, peer_name: 'mainAgent', name: '主 Agent',
    resident: true, updated_at: '2026-11-17T12:00:00Z' }];
  panel().querySelector('.wc-collapse').click();

  // ---- 零张卡空状态 ----
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
        artifacts: [{ id: 'A-9', kind: 'html', title: '刷新后的产物', size: 10,
          created_at: '2026-11-17 14:00:00', url: 'http://localhost/api/agent-board/artifacts/A-9' }] },
    ],
  };
  w.__apiCalls = [];
  es.listeners.board[0]();
  await new Promise((r) => setTimeout(r, 0));
  check('看板 SSE 触发重新拉取快照', w.__apiCalls.includes('/api/agent-board'));
  check('刷新后卡片列表更新',
    [...panel().querySelectorAll('.wc-card')].map((c) => c.dataset.taskId).join(',') === 'T-9,T-2');
  detailCard('T-9').querySelector('.wc-card-head').click();
  check('刷新后产物跟着更新',
    panel().querySelector('.wc-artifact-title').textContent === '刷新后的产物');
  check('已完成的卡没有验收按钮', !panel().querySelector('.wc-accept-btn'));

  // ---- 状态色映射 ----
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
