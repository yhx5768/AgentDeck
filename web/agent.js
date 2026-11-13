/* AgentDeck · Personal Agent 模式（核心）
 *
 * 设置页「界面模式」选 Personal Agent 后，界面变成以主 Agent 为中心的三栏工作台：
 *   左  真实侧栏 + 主 Agent 头像簇（待办角标 / 运行中 worker）+ 三个入口
 *   中  需要你队列（agent-queue.js）/ 主 Agent 会话（真实 SSE 的 #chat-view）/ 看板（真实 #board-view）
 *   右  选中卡片的概览 / 动态 / 产物 / 等你处理 + 常驻留言框（agent-aside.js）
 * 主 Agent 会话里 worker 回报与系统通知按轮次折叠（agent-fold.js）。
 *
 * 数据全部来自真实接口：GET /api/needs-you（待决 prompt + 待人工/待验收的卡）、
 * /api/sessions、/api/agent-board(+events)、/api/a2a/send、权限决议接口。
 * 默认「普通对话」，什么都不改；模式只存本机 localStorage。
 *
 * 命名约定：本模块所有顶层标识以 pa / agent / PA_ 开头，避免和 app.js 的全局撞名。
 */
'use strict';

const PA_MODE_KEY = 'cc-webui:mode';       // 'chat'（默认）| 'agent'
const PA_ASIDE_KEY = 'cc-webui:pa-aside';  // 'open'（默认）| 'closed'：右栏是否被用户关掉
const PA_MAIN_KEY = 'cc-webui:pa-main';    // 多个主 Agent 时记住选的那个
const PA_DONE_KEY = 'cc-webui:pa-done';    // 本机最近处理（含回执）
const PA_UI_KEY = 'cc-webui:pa-ui';        // 队列视图偏好
const PA_DOCK_MIN = 1280;                  // 视口 ≥ 此宽度，右栏与主区并排；否则浮层
const PA_DONE_MAX = 60;

const pa = {
  mode: 'chat',
  view: 'queue',        // queue | chat | board | session（打开的是别的会话）
  applied: false,
  needs: {
    items: [], counts: { all: 0, perm: 0, ask: 0, blocked: 0, review: 0 },
    board: { ok: true, error: '' }, skew: 0, at: 0, err: '', loaded: false, windowMs: 55000,
  },
  needsBusy: false, needsAgain: false,
  done: [],
  ui: { status: 'pending', kind: 'all', group: 'tier', sel: '', pin: '', card: '', tab: 'ov', tf: 'all', tfId: '' },
  drafts: {},           // 条目 id -> 作答表单草稿（选项 / 文本 / 拒绝理由），轮询重绘不丢
  busy: {},             // 条目 id -> 正在提交
  err: {},              // 条目 id -> 上次提交的错误
  cmp: { att: [], busy: false, status: '', kind: '', drafts: {}, wait: null },
  mainErr: '',
  boardErr: '',
  asideOpen: true,
  asideShown: false,    // 窄屏浮层 / 手机整屏：当前是否被打开
  unseen: 0,            // 主 Agent 会话不在前台时到达的折叠回报数
  workerIds: [],
  freshAt: {},          // 条目 id -> 首次出现的时间（只记首屏之后新来的，队列里据此闪一下）
  foldOpen: {},         // 折叠组 key -> 是否展开
  boardEs: null,
  tickTimer: 0,
};

function paGet(key, fallback) {
  try { const v = localStorage.getItem(key); return v == null ? fallback : v; } catch (_) { return fallback; }
}
function paSet(key, val) {
  try { localStorage.setItem(key, val); } catch (_) { /* 无痕模式等：丢掉偏好，不影响功能 */ }
}
function paJSON(key, fallback) {
  try { const v = JSON.parse(paGet(key, '') || 'null'); return v == null ? fallback : v; } catch (_) { return fallback; }
}

// ---------- 小工具 ----------
// 建节点：属性里 text 走 textContent（不会被当 HTML），html 只给内联 svg 常量用。
function paEl(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k === 'text') el.textContent = v;
    else if (k === 'html') el.innerHTML = v;
    else if (k === 'dataset') Object.assign(el.dataset, v);
    else if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2), v);
    else el.setAttribute(k, v === true ? '' : String(v));
  }
  for (const kid of kids.flat(2)) {
    if (kid == null || kid === false) continue;
    el.append(typeof kid === 'object' && kid.nodeType ? kid : document.createTextNode(String(kid)));
  }
  return el;
}

// Element.append 会把数组 / null 当文本塞进去（"[object HTMLDivElement]"、"null"）；先压平、丢空值再 append。
function paAppend(parent, ...kids) {
  for (const kid of kids.flat(3)) {
    if (kid == null || kid === false) continue;
    parent.append(typeof kid === 'object' && kid.nodeType ? kid : document.createTextNode(String(kid)));
  }
  return parent;
}

const PA_ICONS = {
  inbox: '<path d="M22 12h-6l-2 3h-4l-2-3H2"/><path d="M5.45 5.11 2 12v6a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-6l-3.45-6.89A2 2 0 0 0 16.76 4H7.24a2 2 0 0 0-1.79 1.11z"/>',
  bot: '<rect x="3" y="11" width="18" height="10" rx="2"/><circle cx="12" cy="5" r="2"/><path d="M12 7v4"/><path d="M8 16h.01M16 16h.01"/>',
  kanban: '<path d="M6 5v11"/><path d="M12 5v6"/><path d="M18 5v14"/>',
  shield: '<path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z"/><path d="M12 8v4"/><path d="M12 16h.01"/>',
  help: '<circle cx="12" cy="12" r="10"/><path d="M9.09 9a3 3 0 0 1 5.83 1c0 2-3 3-3 3"/><path d="M12 17h.01"/>',
  pause: '<rect x="6" y="5" width="4" height="14" rx="1"/><rect x="14" y="5" width="4" height="14" rx="1"/>',
  check: '<path d="M20 6 9 17l-5-5"/>',
  checkc: '<circle cx="12" cy="12" r="10"/><path d="m9 12 2 2 4-4"/>',
  msg: '<path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/>',
  send: '<path d="m22 2-7 20-4-9-9-4z"/><path d="M22 2 11 13"/>',
  x: '<path d="M18 6 6 18"/><path d="m6 6 12 12"/>',
  chevR: '<path d="m9 18 6-6-6-6"/>',
  arrowUR: '<path d="M7 17 17 7"/><path d="M7 7h10v10"/>',
  arrowR: '<path d="M5 12h14"/><path d="m12 5 7 7-7 7"/>',
  back: '<path d="m15 18-6-6 6-6"/>',
  term: '<path d="m4 17 6-6-6-6"/><path d="M12 19h8"/>',
  file: '<path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><path d="M14 2v6h6"/><path d="M16 13H8"/><path d="M16 17H8"/>',
  alert: '<path d="m21.73 18-8-14a2 2 0 0 0-3.48 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.73-3"/><path d="M12 9v4"/><path d="M12 17h.01"/>',
  info: '<circle cx="12" cy="12" r="10"/><path d="M12 16v-4"/><path d="M12 8h.01"/>',
  menu: '<path d="M4 6h16M4 12h16M4 18h16"/>',
  zap: '<path d="M13 2 3 14h9l-1 8 10-12h-9z"/>',
  layers: '<path d="m12 2 10 5-10 5L2 7z"/><path d="m2 17 10 5 10-5"/><path d="m2 12 10 5 10-5"/>',
  plus: '<path d="M12 5v14M5 12h14"/>',
  panelR: '<rect x="3" y="3" width="18" height="18" rx="2"/><path d="M15 3v18"/>',
  eye: '<path d="M2 12s3-7 10-7 10 7 10 7-3 7-10 7-10-7-10-7z"/><circle cx="12" cy="12" r="3"/>',
  pencil: '<path d="M17 3a2.85 2.83 0 1 1 4 4L7.5 20.5 2 22l1.5-5.5Z"/>',
};
function paSvg(name, cls) {
  return '<svg class="pa-ic' + (cls ? ' ' + cls : '') + '" viewBox="0 0 24 24" aria-hidden="true">' + (PA_ICONS[name] || '') + '</svg>';
}
function paIcon(name, cls) {
  const t = document.createElement('template');
  t.innerHTML = paSvg(name, cls);
  return t.content.firstElementChild;
}

// ---------- 时间 ----------
// 倒计时按服务端时钟走：needs-you 响应带 server_time，skew = 服务端 - 本机，避免两台机器
// 时钟差几秒就把 55s 窗口读歪。
function paNow() { return Date.now() + (pa.needs.skew || 0); }
function paPad(n) { return String(n).padStart(2, '0'); }
function paHM(ts) { const d = new Date(ts); return paPad(d.getHours()) + ':' + paPad(d.getMinutes()); }
function paMMSS(ms) { const s = Math.max(0, Math.ceil(ms / 1000)); return Math.floor(s / 60) + ':' + paPad(s % 60); }
function paDur(ms) {
  const s = Math.max(1, Math.round(ms / 1000));
  if (s < 60) return s + ' 秒';
  const m = Math.floor(s / 60);
  if (m < 60) return m + ' 分钟';
  const hr = Math.floor(m / 60);
  return hr + ' 小时' + (m % 60 ? ' ' + (m % 60) + ' 分' : '');
}
function paRel(ts) {
  const s = Math.max(0, Math.round((paNow() - ts) / 1000));
  if (s < 10) return '刚刚';
  if (s < 60) return s + ' 秒前';
  const m = Math.floor(s / 60);
  if (m < 60) return m + ' 分钟前';
  const hr = Math.floor(m / 60);
  if (hr < 48) return hr + ' 小时' + (m % 60 ? ' ' + (m % 60) + ' 分' : '') + '前';
  return Math.floor(hr / 24) + ' 天前';
}
// 看板时间戳是服务器本地时间 "YYYY-MM-DD HH:MM:SS"；会话事件是 ISO。统一成毫秒。
function paParseTime(v) {
  if (typeof v === 'number') return v;
  const s = String(v || '').trim();
  if (!s) return 0;
  const m = /^(\d{4})-(\d{2})-(\d{2})[ T](\d{2}):(\d{2}):(\d{2})$/.exec(s);
  if (m) return new Date(+m[1], +m[2] - 1, +m[3], +m[4], +m[5], +m[6]).getTime();
  const t = Date.parse(s);
  return Number.isFinite(t) ? t : 0;
}
function paClip(s, n) {
  s = String(s == null ? '' : s);
  return s.length > n ? s.slice(0, n - 1) + '…' : s;
}
function paFirstLine(s) {
  for (const ln of String(s || '').split('\n')) { const t = ln.trim(); if (t) return t; }
  return '';
}
// ---------- 偏好 / 模式 ----------
function agentModeOn() { return pa.mode === 'agent'; }

function paLoadPrefs() {
  pa.mode = paGet(PA_MODE_KEY, 'chat') === 'agent' ? 'agent' : 'chat';
  pa.asideOpen = paGet(PA_ASIDE_KEY, 'open') !== 'closed';
  const ui = paJSON(PA_UI_KEY, {});
  if (ui.group === 'task' || ui.group === 'tier') pa.ui.group = ui.group;
  if (ui.status === 'pending' || ui.status === 'done' || ui.status === 'all') pa.ui.status = ui.status;
  const done = paJSON(PA_DONE_KEY, []);
  pa.done = Array.isArray(done) ? done.filter((d) => d && d.id).slice(0, PA_DONE_MAX) : [];
}
function paSaveUI() { paSet(PA_UI_KEY, JSON.stringify({ group: pa.ui.group, status: pa.ui.status })); }
function paSaveDone() { paSet(PA_DONE_KEY, JSON.stringify(pa.done.slice(0, PA_DONE_MAX))); }

function agentSetMode(mode) {
  const next = mode === 'agent' ? 'agent' : 'chat';
  const changed = pa.mode !== next;
  pa.mode = next;
  paSet(PA_MODE_KEY, next);
  paApplyMode(changed);
}

function paSyncModeSeg() {
  document.querySelectorAll('#pa-mode-seg [data-pa-mode]').forEach((b) => {
    const on = b.dataset.paMode === pa.mode;
    b.classList.toggle('on', on);
    b.setAttribute('aria-checked', on ? 'true' : 'false');
  });
}

// 把模式落到界面上。fromUser：用户刚切过，需要把中栏带到该去的地方；
// 页面刚加载时（false）只套样式，中栏由 paBootView 决定。
function paApplyMode(fromUser) {
  const on = agentModeOn();
  document.body.classList.toggle('pa-on', on);
  paSyncModeSeg();
  const nav = $('#pa-nav');
  if (nav) nav.classList.toggle('hidden', !on);
  if (on) {
    paConnectBoard();
    paStartTick();
    agentRefreshNeeds();
    paEnsureBoard();
    paRenderAll();
    if (fromUser || !pa.applied) paBootView();
  } else {
    paDisconnectBoard();
    paStopTick();
    document.title = 'AgentDeck';
    const nv = $('#needs-view');
    if (nv) nv.classList.add('hidden');
    paHideAside();
    if (typeof agentUnfoldAll === 'function') agentUnfoldAll();
    // 还原原版头像条：清掉签名让 renderMainAgents 重画一遍
    const ma = $('#main-agents');
    if (ma) { ma.classList.remove('pa-agents'); ma.innerHTML = ''; }
    state.maRenderSig = '';
    if (typeof renderMainAgents === 'function') renderMainAgents();
    // 中栏回到普通对话该有的样子
    if (fromUser) {
      if (state.current) openSession(state.current.id);
      else { const es = $('#empty-state'); if (es) es.classList.remove('hidden'); }
    }
  }
  pa.applied = true;
}

// 进入模式时中栏显示什么：当前正开着主 Agent 会话就留在会话；否则回到队列首页。
function paBootView() {
  const main = paPrimaryMain();
  if (state.current && main && state.current.id === main.id) {
    pa.view = paActiveChatTab() === 'board' ? 'board' : 'chat';
    paRenderNav();
    paSyncAside();
    return;
  }
  agentShowView('queue');
}
function paActiveChatTab() {
  const t = document.querySelector('.chat-tab.active');
  return t ? t.dataset.tab : 'chat';
}

// ---------- 主 Agent / worker 解析 ----------
function paPrimaryMain() {
  const list = typeof mainAgentSessions === 'function' ? mainAgentSessions() : [];
  if (!list.length) return null;
  const want = paGet(PA_MAIN_KEY, '');
  return list.find((s) => s.id === want) || list[0];
}
// A2A 寻址用的地址名（peer_name，没设就取显示名清洗后的值）。只在发消息 / 对账时用，别拿它当展示。
function paMainAddr(main) {
  main = main || paPrimaryMain();
  return main ? (main.peer_name || main.name || '') : '';
}
// 展示用的名字 = 会话的显示名（name，和侧栏、重命名菜单是同一个字段），可以随便改；地址名不跟着动。
function paMainName(main) {
  main = main || paPrimaryMain();
  return main ? (main.name || main.peer_name || '主 Agent') : '';
}
function paSessionById(id) { return (state.sessions || []).find((s) => s.id === id) || null; }
function paSessionByAddr(addr) {
  const a = String(addr || '').trim().toLowerCase();
  if (!a) return null;
  return (state.sessions || []).find((s) => String(s.peer_name || s.name || '').trim().toLowerCase() === a) || null;
}
function paBoardTasks() { return (state.agentBoard && Array.isArray(state.agentBoard.tasks)) ? state.agentBoard.tasks : []; }
function paTaskById(id) { return paBoardTasks().find((t) => t.id === id) || null; }
function paTaskOfSession(sid) {
  const ts = paBoardTasks().filter((t) => t.session === sid);
  if (!ts.length) return null;
  // 同一会话绑过多张卡时取没收尾的、最近更新的那张
  const open = ts.filter((t) => t.status !== '已完成' && t.status !== '已取消');
  return (open.length ? open : ts).sort((a, b) => String(b.updated_at).localeCompare(String(a.updated_at)))[0];
}

// 头像簇里的 worker：绑了卡且在跑的（沿用侧栏旧逻辑）∪ 主 Agent 派生的、正在跑的子会话。
function paRunningWorkers() {
  const main = paPrimaryMain();
  const out = new Map();
  const base = typeof runningMainAgentWorkers === 'function' ? runningMainAgentWorkers() : [];
  for (const w of base) {
    if (main && w.sessionId === main.id) continue;
    out.set(w.sessionId, { sessionId: w.sessionId, name: w.name, taskId: w.task && w.task.id });
  }
  for (const s of state.sessions || []) {
    if (!main || s.parent_id !== main.id || s.main_agent) continue;
    if (!(s.status && s.status.running) || out.has(s.id)) continue;
    const t = paTaskOfSession(s.id);
    out.set(s.id, { sessionId: s.id, name: s.name || s.peer_name || 'worker', taskId: t && t.id });
  }
  const list = [...out.values()];
  const urgent = (w) => paItemsOfSession(w.sessionId).some((i) => i.state === 'live') ? 1 : 0;
  return list.sort((a, b) => (urgent(b) - urgent(a)) || a.name.localeCompare(b.name, 'zh'));
}

// ---------- 队列数据的取用 ----------
function paItems() { return pa.needs.items || []; }
function paItemsOfSession(sid) { return paItems().filter((i) => i.session_id === sid); }
function paItemsOfTask(tid) { return paItems().filter((i) => i.task_id === tid); }
function paItemById(id) { return paItems().find((i) => i.id === id) || null; }
function paHasLivePerm() { return paItems().some((i) => i.kind === 'perm' && i.state === 'live'); }
function paLeftMs(item) { return item.expires_at ? item.expires_at - paNow() : 0; }
// ---------- 数据：GET /api/needs-you ----------
// 单飞：请求在途时再来的刷新只记一个「回头再拉一次」，不叠请求也不丢更新。
async function agentRefreshNeeds() {
  if (!agentModeOn()) return;
  if (pa.needsBusy) { pa.needsAgain = true; return; }
  pa.needsBusy = true;
  try {
    const d = await api('/api/needs-you');
    const had = new Set(pa.needs.items.map((i) => i.id));
    const wasLoaded = pa.needs.loaded;
    pa.needs = {
      items: Array.isArray(d.items) ? d.items : [],
      counts: Object.assign({ all: 0, perm: 0, ask: 0, blocked: 0, review: 0 }, d.counts || {}),
      board: d.board || { ok: true },
      skew: (Number(d.server_time) || Date.now()) - Date.now(),
      at: Date.now(), err: '', loaded: true,
      windowMs: Number(d.window_ms) || 55000,
    };
    const nowMs = Date.now();
    if (wasLoaded) pa.needs.items.forEach((i) => { if (!had.has(i.id)) pa.freshAt[i.id] = nowMs; });
    Object.keys(pa.freshAt).forEach((id) => { if (nowMs - pa.freshAt[id] > 10000) delete pa.freshAt[id]; });
  } catch (e) {
    // 取失败不清空旧数据：队列里还在倒计时的卡不该因为一次网络抖动凭空消失
    pa.needs.err = (e && e.message) || String(e);
    pa.needs.loaded = true;
  } finally {
    pa.needsBusy = false;
  }
  paAfterNeeds();
  if (pa.needsAgain) { pa.needsAgain = false; agentRefreshNeeds(); }
}

function paAfterNeeds() {
  if (!agentModeOn()) return;
  paRenderNav();
  agentRenderCluster();
  paRaisePermBanners();
  paSyncTitle();
  if (typeof agentRenderQueue === 'function') agentRenderQueue();
  if (typeof agentAsideNeedsChanged === 'function') agentAsideNeedsChanged();
  if (typeof agentRenderTabbar === 'function') agentRenderTabbar();
  if (typeof agentFoldRefresh === 'function') agentFoldRefresh(); // 折叠组头部的「已入需要你」计数跟着队列走
}

function paSyncTitle() {
  const n = pa.needs.counts.all || 0;
  document.title = (n ? '(' + n + ') ' : '') + 'AgentDeck';
}

// ---------- 数据：看板快照 + SSE ----------
let paBoardBusy = false;
async function paEnsureBoard(force) {
  if (!agentModeOn() || paBoardBusy) return;
  if (!force && state.agentBoard && Date.now() - (state.mainAgentBoardAt || 0) < 8000) return;
  paBoardBusy = true;
  try {
    state.agentBoard = await api('/api/agent-board');
    state.mainAgentBoardAt = Date.now();
    pa.boardErr = '';
  } catch (e) {
    pa.boardErr = (e && e.message) || String(e);
  } finally {
    paBoardBusy = false;
  }
  paAfterBoard();
}
function paAfterBoard() {
  if (!agentModeOn()) return;
  agentRenderCluster();
  paRenderNav();
  if (typeof agentRenderQueue === 'function') agentRenderQueue();
  if (typeof agentAsideBoardChanged === 'function') agentAsideBoardChanged();
}

let paBoardTimer = 0;
function paConnectBoard() {
  if (pa.boardEs || typeof EventSource === 'undefined') return;
  const es = new EventSource('/api/agent-board/events');
  const refresh = () => {
    clearTimeout(paBoardTimer);
    paBoardTimer = setTimeout(() => { paEnsureBoard(true); agentRefreshNeeds(); }, 250);
  };
  es.addEventListener('ready', refresh);
  es.addEventListener('board', refresh);
  pa.boardEs = es;
}
function paDisconnectBoard() {
  clearTimeout(paBoardTimer);
  if (pa.boardEs) { pa.boardEs.close(); pa.boardEs = null; }
}

// ---------- 5s 轮询挂点（loadSessions 末尾调） ----------
function agentOnPoll() {
  if (!agentModeOn()) return;
  agentRefreshNeeds();
  paEnsureBoard(false);
  agentRenderCluster();
}

// ---------- 倒计时 / 相对时间 ----------
function paStartTick() { if (!pa.tickTimer) pa.tickTimer = setInterval(paTick, 500); }
function paStopTick() { clearInterval(pa.tickTimer); pa.tickTimer = 0; }
let paRelAt = 0;
function paTick() {
  if (!agentModeOn()) return;
  const now = paNow();
  document.querySelectorAll('[data-pa-cd]').forEach((el) => {
    const it = paItemById(el.dataset.paCd);
    if (!it || !it.expires_at) return;
    const left = it.expires_at - now;
    el.textContent = left > 0 ? paMMSS(left) : '已超时';
    el.classList.toggle('low', left > 0 && left < 15000);
    el.classList.toggle('over', left <= 0);
  });
  document.querySelectorAll('[data-pa-bar]').forEach((el) => {
    const it = paItemById(el.dataset.paBar);
    if (!it || !it.expires_at) return;
    const win = pa.needs.windowMs || 55000;
    el.style.width = Math.max(0, Math.min(100, ((it.expires_at - now) / win) * 100)) + '%';
  });
  // 到点的 live 条目：服务端会在超时那一刻把它摘掉，这里补拉一次对账（每条只触发一次）
  const stale = paItems().filter((i) => i.state === 'live' && i.expires_at && i.expires_at < now - 1200 && !i._synced);
  if (stale.length) { stale.forEach((i) => { i._synced = true; }); agentRefreshNeeds(); }
  if (Date.now() - paRelAt > 15000) {
    paRelAt = Date.now();
    document.querySelectorAll('[data-pa-rel]').forEach((el) => { el.textContent = paRel(+el.dataset.paRel); });
    document.querySelectorAll('[data-pa-wait]').forEach((el) => { el.textContent = '已等 ' + paDur(now - +el.dataset.paWait); });
  }
}

// ---------- 权限门横幅（通知矩阵里唯一会打断人的一档） ----------
// 提问/阻塞/验收只入队列；权限 55s 不处理就自动拒绝，所以只有它顶部弹横幅 + 提示音。
// 复用侧栏同一套 toast（#toast-layer），按 pending id 幂等。
const paBannered = new Set();
// 队列视图正摆在用户面前且停在顶部时，新来的权限门 / 提问就出现在队列顶上——不再压一条横幅盖住队列头，
// 只响一声（key 记下来，轮询里重复调用不重复响）。用户在别的视图 / 页签不可见 / 队列已滚走时照常弹横幅。
const paAbsorbed = new Map(); // key -> 首次吸收的时间
function agentAbsorbBanner(key) {
  if (!agentModeOn()) return false;
  if (paAbsorbed.has(key)) return true;
  if (pa.view !== 'queue' || pa.ui.status === 'done' || document.visibilityState === 'hidden') return false;
  const nv = $('#needs-view');
  const qb = nv && !nv.classList.contains('hidden') ? nv.querySelector('.pa-qb') : null;
  if (!qb || qb.scrollTop > 80) return false;
  paAbsorbed.set(key, Date.now());
  if (typeof notifyPrefs === 'function' && notifyPrefs().sound && typeof playAskSound === 'function') playAskSound();
  return true;
}
function paRaisePermBanners() {
  if (typeof pushToast !== 'function') return;
  const live = new Set();
  for (const it of paItems()) {
    if (it.kind !== 'perm' || it.state !== 'live') continue;
    const key = 'pa-perm:' + it.pending_id;
    live.add(key);
    if (paBannered.has(key)) continue;
    paBannered.add(key);
    if (agentAbsorbBanner(key)) continue;
    const go = () => { dropToast(key); agentFocusItem(it.id); };
    const rec = pushToast({
      id: key, kind: 'perm',
      title: '权限门 · ' + (it.session_name || '会话') + ' 在等你',
      body: it.title, onClick: go,
      actions: [{ label: '去处理', primary: true, onClick: go }],
    });
    const icon = rec.el.querySelector('.toast-icon');
    if (icon) icon.innerHTML = paSvg('shield');
    const timer = rec.el.querySelector('.toast-timer');
    const paint = () => {
      const left = it.expires_at - paNow();
      if (timer) timer.textContent = left > 0 ? Math.ceil(left / 1000) + 's' : '已超时';
      rec.el.classList.toggle('urgent', left > 0 && left <= 15000);
      if (left <= 0) { clearInterval(iv); rec.el.classList.add('expired'); }
    };
    const iv = setInterval(paint, 500);
    rec.timers.push(iv);
    paint();
    if (typeof notifyPrefs === 'function' && notifyPrefs().sound && typeof playAskSound === 'function') playAskSound();
  }
  // 吸收记录按时间清（窗口只有 55s）：按「还在不在队列里」清的话，轮询刚好撞上队列刷新前的旧数据就会重复响
  const cut = Date.now() - 180000;
  for (const [key, at] of [...paAbsorbed]) if (at < cut) paAbsorbed.delete(key);
  // 已处理/已超时的权限门：横幅随之撤掉
  for (const key of [...paBannered]) {
    if (live.has(key)) continue;
    paBannered.delete(key);
    if (typeof dropToast === 'function') dropToast(key);
  }
}
// ---------- 左栏：三个入口 ----------
function paRenderNav() {
  const nav = $('#pa-nav');
  if (!nav || !agentModeOn()) return;
  const c = pa.needs.counts;
  const hot = paHasLivePerm();
  const waiting = (c.blocked || 0) + (c.review || 0);
  const sig = [pa.view, c.all, hot, waiting, pa.unseen].join('|');
  if (nav.dataset.sig === sig && nav.childElementCount) return;
  nav.dataset.sig = sig;
  nav.textContent = '';
  const item = (view, icon, label, ...extra) => paEl('button', {
    type: 'button', class: 'pa-nav-item' + (pa.view === view ? ' on' : ''),
    'aria-current': pa.view === view ? 'page' : null,
    onclick: () => agentShowView(view),
  }, paIcon(icon), paEl('span', { class: 'pa-nav-label', text: label }), extra);
  const cnt = (n, cls, tip) => (n > 0 ? paEl('em', { class: 'pa-cnt ' + cls, text: String(n), title: tip || null }) : null);
  nav.append(
    item('queue', 'inbox', '需要你', hot ? paEl('i', { class: 'pa-live' }) : null, cnt(c.all, hot ? 'hot' : 'warm', '在等你处理的事')),
    item('chat', 'bot', '主 Agent 会话', pa.unseen ? paEl('em', { class: 'pa-cnt', text: '+' + pa.unseen, title: '新回报已折叠，不打扰' }) : null),
    item('board', 'kanban', '看板任务', cnt(waiting, 'warm', '待人工 / 待验收的卡片')),
  );
}

// ---------- 左栏：主 Agent 头像簇 ----------
// 主头像居中；有 worker 在跑时它们轻微叠在主头像右侧并上下浮动，新来的弹入；
// 没有 worker 就只有主头像，不留占位。左上角角标 = 在等你的事，右下角绿点 = 在线 / 处理中。
function paInitial(name) {
  const n = String(name || '').replace(/^w[·.\-_ ]/i, '').trim();
  return n ? [...n][0].toUpperCase() : 'A';
}
function paClusterMax(el) {
  // 尺寸 / 叠放和侧栏原版头像条是同一套（主头像 40、worker 26、叠 6），可见数直接用它的公式
  //（含给「+N」留的位置）；卡片的边框 + 内边距占 26px。
  const inner = typeof mainAgentInnerWidth === 'function' ? mainAgentInnerWidth(el) : 240;
  return typeof mainAgentWorkerVisibleCount === 'function' ? mainAgentWorkerVisibleCount(Math.max(0, inner - 26)) : 6;
}
function paWorkerAvatar(w, fresh) {
  const n = paItemsOfSession(w.sessionId).length;
  const task = w.taskId ? paTaskById(w.taskId) : null;
  const tip = [w.name + ' · 运行中', task ? task.title : '', n ? '在等你 ' + n + ' 项' : '', w.taskId ? '点开它的卡片' : '点开它的会话'].filter(Boolean).join('\n');
  const on = !!(w.taskId && pa.ui.card === w.taskId && paAsideVisible());
  const btn = paEl('button', {
    type: 'button', class: 'pa-wav' + (fresh ? ' new' : '') + (on ? ' on' : ''),
    title: tip, 'aria-label': w.name + '，运行中',
    onclick: (e) => { e.stopPropagation(); if (w.taskId) agentOpenCard(w.taskId); else openSession(w.sessionId); },
  }, paEl('span', { text: paInitial(w.name) }), n ? paEl('b', { class: 'wn', text: String(n) }) : null);
  btn.style.setProperty('--worker-hue', typeof workerHue === 'function' ? workerHue(w.name) : 210);
  return btn;
}
function agentRenderCluster() {
  const el = $('#main-agents');
  if (!el || !agentModeOn()) return;
  const main = paPrimaryMain();
  el.classList.add('pa-agents');
  el.classList.remove('hidden');
  if (!main) {
    const sig = 'none:' + (pa.mainErr || '');
    if (el.dataset.paSig === sig) return;
    el.dataset.paSig = sig;
    el.textContent = '';
    const btn = paEl('button', { type: 'button', class: 'primary-btn', text: '新建主 Agent' });
    btn.onclick = () => paCreateMainAgent(btn);
    el.append(paEl('div', { class: 'pa-agents-card empty' },
      paEl('b', { text: '还没有主 Agent' }),
      paEl('p', { text: 'Personal Agent 需要一个主 Agent 会话来派工和汇总。可以一键新建，也可以在任意会话的 📡 面板里打开「作为主 Agent」。' }),
      btn, pa.mainErr ? paEl('p', { class: 'pa-err', text: pa.mainErr }) : null));
    return;
  }
  // 正在改名时别重画：输入框会被销毁（轮询每隔几秒就会触发一次）
  if (pa.renaming && el.querySelector('.pa-name-in')) return;
  const workers = paRunningWorkers();
  const c = pa.needs.counts.all || 0;
  const hot = paHasLivePerm();
  const run = !!(main.status && main.status.running);
  const max = paClusterMax(el);
  const name = paMainName(main);
  const sig = [main.id, main.name, main.peer_name, run, c, hot, pa.view, max, pa.ui.card, pa.asideShown, pa.renameErr,
    workers.map((w) => w.sessionId + ':' + paItemsOfSession(w.sessionId).length + ':' + (w.taskId || '')).join(',')].join(';');
  if (el.dataset.paSig === sig && el.childElementCount) return;
  el.dataset.paSig = sig;
  const prev = new Set(pa.workerIds);
  pa.workerIds = workers.map((w) => w.sessionId);
  const shown = workers.slice(0, max);
  const more = workers.length - shown.length;
  const onMain = pa.view === 'chat' || pa.view === 'board';
  const av = paEl('button', {
    type: 'button', class: 'pa-av' + (onMain ? ' on' : ''),
    title: name + '\n主 Agent · 点开会话', 'aria-label': '打开主 Agent 会话',
    onclick: () => agentShowView('chat'),
  }, paEl('span', { text: paInitial(name) }));
  const badge = c ? paEl('button', {
    type: 'button', class: 'pa-badge' + (hot ? ' hot' : ''), text: c > 9 ? '9+' : String(c),
    title: c + ' 项在等你，点开「需要你」', 'aria-label': c + ' 项在等你',
    onclick: (e) => { e.stopPropagation(); agentShowView('queue'); },
  }) : null;
  const dot = paEl('i', { class: 'pa-dot' + (run ? ' run' : ''), title: '主 Agent 在线 · ' + (run ? '处理中' : '空闲') });
  const ws = shown.length ? paEl('div', { class: 'pa-ws' },
    shown.map((w) => paWorkerAvatar(w, !prev.has(w.sessionId))),
    more ? paEl('span', { class: 'pa-more', text: '+' + more, title: workers.slice(shown.length).map((w) => w.name).join('\n') }) : null) : null;
  const nameBtn = paEl('button', { type: 'button', class: 'pa-name', title: '点击重命名（只改显示名，A2A 地址名不变）', 'aria-label': '重命名主 Agent，当前叫 ' + name },
    paEl('b', { text: name }), paEl('span', { class: 'pe', html: paSvg('pencil', 's') }));
  nameBtn.onclick = () => paStartRename(main, nameBtn);
  el.textContent = '';
  el.append(paEl('div', { class: 'pa-agents-card' },
    paEl('div', { class: 'pa-ag-row' + (ws ? ' has-ws' : '') }, paEl('div', { class: 'pa-av-main' }, av, badge, dot), ws),
    paEl('div', { class: 'pa-ag-cap' }, nameBtn,
      paEl('span', { class: 'rn' + (workers.length ? ' on' : ''), text: workers.length ? '运行中 ' + workers.length : (run ? '处理中' : '空闲') })),
    pa.renameErr ? paEl('div', { class: 'pa-ag-err', role: 'alert', text: pa.renameErr }) : null));
}

// 主 Agent 显示名：点名字就地改，PATCH 的是和侧栏「重命名」同一个 name 字段，刷新后还在。
// 只动 name——peer_name（A2A 地址名）不碰，所以 worker 往 mainAgent 报告的链路不受影响。
function paRerenderCluster() {
  const el = $('#main-agents');
  if (el) { el.dataset.paSig = ''; agentRenderCluster(); }
}
function paStartRename(main, nameBtn) {
  if (pa.renaming) return;
  pa.renaming = true;
  pa.renameErr = '';
  const input = paEl('input', { class: 'pa-name-in', type: 'text', value: main.name || '', maxlength: '60', spellcheck: 'false', 'aria-label': '主 Agent 显示名' });
  let settled = false;
  const finish = async (commit) => {
    if (settled) return;
    settled = true;
    const name = input.value.trim();
    if (!commit || !name || name === main.name) { pa.renaming = false; paRerenderCluster(); return; }
    // 没单独设置地址名时，A2A 地址是跟着显示名走的：改名会让 worker 的回报找不到它，得让用户知情
    if (!main.peer_name && typeof confirm === 'function' && !confirm('这个主 Agent 没有单独设置地址名，A2A 地址跟着显示名走。\n改成「' + name + '」后，worker 往「' + main.name + '」发的回报会找不到它（可以先在 📡 面板里设置地址名）。\n仍要改吗？')) {
      pa.renaming = false; paRerenderCluster(); return;
    }
    input.disabled = true;
    try {
      await api('/api/sessions/' + paEnc(main.id), { method: 'PATCH', body: JSON.stringify({ name }) });
      main.name = name;
      if (typeof loadSessions === 'function') loadSessions();
    } catch (e) {
      pa.renameErr = '重命名失败：' + ((e && e.message) || e);
      setTimeout(() => { if (pa.renameErr) { pa.renameErr = ''; paRerenderCluster(); } }, 8000);
    }
    pa.renaming = false;
    paRerenderCluster();
  };
  input.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.isComposing) { e.preventDefault(); finish(true); }
    else if (e.key === 'Escape') { e.preventDefault(); finish(false); }
  });
  input.addEventListener('blur', () => finish(true));
  nameBtn.replaceWith(input);
  input.focus();
  input.select();
}

async function paCreateMainAgent(btn) {
  btn.disabled = true;
  btn.textContent = '创建中…';
  pa.mainErr = '';
  try {
    const cli = (state.clis || []).find((c) => (c.kind || 'claude') === 'claude') || (state.clis || [])[0];
    if (!cli) throw new Error('config.yaml 里没有可用的 CLI');
    const kind = cli.kind || 'claude';
    const prov = (state.providers || []).find((p) => (p.kind || 'claude') === kind);
    if (!prov) throw new Error('没有和 ' + cli.name + ' 同引擎的 provider');
    let prompt = '';
    try { prompt = await ensureMainAgentPrompt(); } catch (_) { /* 提示词取不到也能建，之后可在 📡 面板补 */ }
    const body = { name: '主 Agent', cli: cli.name, provider: prov.name, cwd: state.home || '~', peer_name: 'mainAgent', main_agent: true, resident: kind === 'claude' };
    if (prompt) body.role_prompt = prompt;
    const s = await api('/api/sessions', { method: 'POST', body: JSON.stringify(body) });
    paSet(PA_MAIN_KEY, s.id);
    await loadSessions();
    agentShowView('chat');
  } catch (e) {
    pa.mainErr = '创建失败：' + ((e && e.message) || e);
    const el = $('#main-agents');
    if (el) el.dataset.paSig = '';
    agentRenderCluster();
  }
}
// ---------- 中栏切换 ----------
// queue 是 #needs-view；chat / board 是主 Agent 会话 #chat-view 的两个 tab（真实 SSE）。
function agentShowView(view) {
  if (!agentModeOn()) return;
  if (view === 'queue') paShowQueue();
  else paShowMain(view === 'board' ? 'board' : 'chat');
}
function paShowQueue() {
  if (typeof hideAllViews === 'function') hideAllViews();
  pa.view = 'queue';
  pa.resumeQueue = false;
  const nv = $('#needs-view');
  if (nv) nv.classList.remove('hidden');
  if (typeof isMobile === 'function' && isMobile() && typeof closeDrawer === 'function') closeDrawer();
  if (typeof agentRenderQueue === 'function') agentRenderQueue(true);
  paRenderNav();
  agentRenderCluster();
  paSyncAside();
}
async function paShowMain(view) {
  const main = paPrimaryMain();
  if (!main) { paShowQueue(); return; }
  try {
    if (!state.current || state.current.id !== main.id) {
      await openSession(main.id);
    } else {
      hideAllViews();
      const nv = $('#needs-view');
      if (nv) nv.classList.add('hidden');
      $('#chat-view').classList.remove('hidden');
    }
    switchChatTab(view);
  } catch (e) {
    if (typeof pushToast === 'function') pushToast({ id: 'pa-open-main', title: '打开主 Agent 会话失败', body: (e && e.message) || String(e) });
    return;
  }
  pa.view = view;
  pa.resumeQueue = false;
  if (view === 'chat') pa.unseen = 0;
  if (typeof isMobile === 'function' && isMobile() && typeof closeDrawer === 'function') closeDrawer();
  paRenderNav();
  agentRenderCluster();
  paSyncAside();
}

// 以下三个由 app.js 在对应时机调（typeof 守卫，普通对话模式下全部空转）。
function agentOnSessionOpened(s) {
  if (!agentModeOn()) return;
  const nv = $('#needs-view');
  if (nv) nv.classList.add('hidden');
  pa.resumeQueue = false;
  if (s && s.main_agent) paSet(PA_MAIN_KEY, s.id); // 点开哪个主 Agent，头像簇就跟着它
  pa.view = s && s.main_agent ? (paActiveChatTab() === 'board' ? 'board' : 'chat') : 'session';
  if (pa.view === 'chat') pa.unseen = 0;
  paRenderNav();
  agentRenderCluster();
  paSyncAside();
}
function agentOnTabSwitch(tab) {
  if (!agentModeOn() || !state.current || !state.current.main_agent) return;
  if (tab !== 'chat' && tab !== 'board') return;
  pa.view = tab;
  if (tab === 'chat') pa.unseen = 0;
  paRenderNav();
  agentRenderCluster();
}
function agentHideNeedsView() {
  const nv = $('#needs-view');
  const wasQueue = !!nv && !nv.classList.contains('hidden');
  if (nv) nv.classList.add('hidden');
  if (!agentModeOn()) return;
  // 队列 / 主会话被别的视图（群聊 / 新建 / 分享管理）顶掉：中栏已经不是它了，导航高亮要跟着；
  // 队列被顶掉时记一笔，分享管理关闭时能回来
  if (wasQueue && pa.view === 'queue') pa.resumeQueue = true;
  if (pa.view === 'queue' || pa.view === 'chat' || pa.view === 'board') {
    pa.view = 'session';
    paRenderNav();
    agentRenderCluster();
    paSyncAside();
  }
}
function agentRestoreView() {
  if (agentModeOn() && pa.resumeQueue) paShowQueue();
}

// 点横幅 / 头像角标 / 行：定位到队列里那一项（顺带在右栏展开它的卡）
function agentFocusItem(id) {
  if (!agentModeOn()) return false;
  const it = paItemById(id);
  if (!it) return false;
  pa.ui.status = 'pending';
  pa.ui.kind = 'all';
  agentShowView('queue');
  if (typeof agentSelectItem === 'function') agentSelectItem(id, { reveal: true });
  return true;
}

function paRenderAll() {
  paRenderNav();
  agentRenderCluster();
  if (typeof agentRenderQueue === 'function') agentRenderQueue();
  paSyncAside();
  if (typeof agentRenderTabbar === 'function') agentRenderTabbar();
}

// ---------- 启动 ----------
let paResizeT = 0;
function paBindSettings() {
  document.querySelectorAll('#pa-mode-seg [data-pa-mode]').forEach((b) => {
    b.addEventListener('click', () => agentSetMode(b.dataset.paMode));
  });
  const open = $('#btn-settings');
  if (open) open.addEventListener('click', paSyncModeSeg);
}
function paInit() {
  paLoadPrefs();
  paBindSettings();
  window.addEventListener('resize', () => {
    clearTimeout(paResizeT);
    paResizeT = setTimeout(() => {
      if (!agentModeOn()) return;
      const el = $('#main-agents');
      if (el) el.dataset.paSig = '';
      agentRenderCluster();
      paSyncAside();
    }, 150);
  });
  document.addEventListener('visibilitychange', () => {
    if (document.hidden || !agentModeOn()) return;
    agentRefreshNeeds();
    paEnsureBoard(true);
  });
  paApplyMode(false);
}
// 其余 agent-*.js 在本文件之后加载，paInit 要等它们都就位（DOMContentLoaded 在所有
// 同步脚本执行完之后触发）；测试里 readyState 已是 complete，直接跑。
if (window.__paSkipInit) { /* 测试里自己调 paInit，免得和桩的就位顺序打架 */ }
else if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', paInit);
else paInit();
