// Personal Agent 前端测试的公共 harness：把真实的 agent*.js 当经典脚本塞进 jsdom（顶层 const / let
// 跨脚本共享，和浏览器一致），app.js 里被它们依赖的纯逻辑函数从源码里原样摘出来，其余全局用桩。
// 局限：jsdom 不做布局，头像叠放 / 右栏停靠 / 动效手感验不了；能钉的是 DOM 契约、分组 / 排序 / 去重
// 规则和「哪些动作会发哪些请求」。
const fs = require('fs');
const path = require('path');
const { JSDOM } = require('jsdom');

const WEB = path.resolve(__dirname, '../../web');
const read = (f) => fs.readFileSync(path.join(WEB, f), 'utf8');
const appSrc = read('app.js');

// 按大括号配对原样摘出 app.js 里的一个顶层函数——执行的是真代码，不是抄的副本。
function grabFn(name) {
  const s = appSrc.indexOf('function ' + name + '(');
  if (s < 0) throw new Error('找不到函数 ' + name);
  const start = appSrc.slice(Math.max(0, s - 6), s) === 'async ' ? s - 6 : s;
  const b = appSrc.indexOf('{', s);
  let d = 0, q = null;
  for (let i = b; i < appSrc.length; i++) {
    const c = appSrc[i];
    if (q) { if (c === '\\') { i++; continue; } if (c === q) q = null; continue; }
    if (c === '"' || c === "'" || c === '`') { q = c; continue; }
    if (c === '/' && appSrc[i + 1] === '/') { while (i < appSrc.length && appSrc[i] !== '\n') i++; continue; }
    if (c === '/' && appSrc[i + 1] === '*') { const e = appSrc.indexOf('*/', i + 2); i = e < 0 ? appSrc.length : e + 1; continue; }
    if (c === '/') {
      const prev = appSrc.slice(b, i).trimEnd().slice(-1);
      if (prev === '' || '(,=:[!&|?{};+-*%^~<>'.includes(prev)) {
        let j = i + 1, cls = false;
        while (j < appSrc.length) {
          if (appSrc[j] === '\\') { j += 2; continue; }
          if (appSrc[j] === '[') cls = true; else if (appSrc[j] === ']') cls = false;
          else if (appSrc[j] === '/' && !cls) break; else if (appSrc[j] === '\n') break;
          j++;
        }
        i = j; continue;
      }
    }
    if (c === '{') d++;
    else if (c === '}') { d--; if (!d) return appSrc.slice(start, i + 1); }
  }
  throw new Error(name + ' 括号不配对');
}
function grabLine(needle) {
  const i = appSrc.indexOf(needle);
  if (i < 0) throw new Error('找不到: ' + needle);
  return appSrc.slice(i, appSrc.indexOf('\n', i)).trim();
}

const GRABBED = ['mainAgentSessions', 'runningMainAgentWorkers', 'workerHue', 'nameInitial', 'mainAgentInnerWidth', 'shortenPath',
  'statusDot', 'parsePermQuestions', 'toolSummary', 'boardTaskEvents', 'boardTaskContext', 'boardReplyEvents', 'boardReplyKey',
  'boardReplyKeys', 'boardEventText', 'boardAttachmentLinks', 'boardAppendUserEvent', 'escapeHtml',
  'boardEventAvatar', 'boardMuted', 'boardTimelineRow', 'boardRenderTimeline'];

const HTML = `<!doctype html><html><body>
<div id="app">
  <aside id="sidebar"><div id="main-agents" class="main-agents hidden"></div><nav id="pa-nav" class="pa-nav hidden"></nav></aside>
  <main id="main">
    <div id="empty-state"></div>
    <div id="needs-view" class="hidden"></div>
    <div id="chat-view" class="hidden">
      <div id="chat-tabs"><button class="chat-tab active" data-tab="chat"></button><button class="chat-tab" data-tab="board"></button></div>
      <div id="msg-wrap"><div id="messages"></div></div>
    </div>
  </main>
  <aside id="agent-aside" class="pa-aside hidden"></aside>
  <button id="pa-aside-handle" class="pa-aside-handle hidden" type="button" title="展开卡片详情" aria-label="展开卡片详情"></button>
  <nav id="pa-tabbar" class="pa-tabbar hidden"></nav>
</div>
<div id="modal-settings"><button id="btn-settings"></button>
  <div id="pa-mode-seg"><button data-pa-mode="chat" class="on"></button><button data-pa-mode="agent"></button></div></div>
</body></html>`;

const STUBS = `
const $ = (s) => document.querySelector(s);
const state = { sessions: [], current: null, agentBoard: null, clis: [], providers: [], home: '/home/u', mainAgentBoardAt: 0,
  maRenderSig: '', maWorkerIds: [], toasts: {}, nearBottom: true, replaying: false, renderingHistory: false };
const PERM_OTHER = '__other__';
async function api(path, o) {
  o = o || {};
  const method = o.method || 'GET';
  const body = o.body ? JSON.parse(o.body) : undefined;
  window.__calls.push({ method, path, body });
  const h = window.__api[method + ' ' + path] || window.__api[path] || Object.entries(window.__api).find(([k]) => k.endsWith('*') && (method + ' ' + path).startsWith(k.slice(0, -1)))?.[1];
  if (typeof h === 'function') return h(body, path);
  if (h !== undefined) return h;
  return {};
}
function isMobile() { return window.__mobile === true; }
function openDrawer() { window.__drawer = true; }
function closeDrawer() { window.__drawer = false; }
function hideAllViews() { ['#empty-state', '#chat-view'].forEach((s) => $(s).classList.add('hidden')); agentHideNeedsView(); }
async function openSession(id) {
  state.current = state.sessions.find((s) => s.id === id);
  $('#chat-view').classList.remove('hidden');
  window.__opened = id;
  agentOnSessionOpened(state.current);
}
function switchChatTab(tab) {
  document.querySelectorAll('.chat-tab').forEach((b) => b.classList.toggle('active', b.dataset.tab === tab));
  agentOnTabSwitch(tab);
}
function scrollBottom() {}
function pushToast(t) { window.__toasts.push(t); const el = document.createElement('div'); el.innerHTML = '<div class="toast-icon"></div><span class="toast-timer"></span>'; return { el, timers: [] }; }
function dropToast(id) { window.__dropped.push(id); }
function notifyPrefs() { return { sound: false }; }
function playAskSound() {}
function renderMainAgents() { window.__origMA = (window.__origMA || 0) + 1; }
function loadSessions() { return Promise.resolve(); }
function bumpSessionActivity() {}
function engineTakesImages() { return true; }
const ALLOWED_MIME = ['image/png', 'image/jpeg'];
const MAX_IMAGES = 4, MAX_FILES = 8, MAX_IMAGE_BYTES = 10 << 20, MAX_FILE_BYTES = 40 << 20;
function fmtMiB(n) { return (n / 1048576).toFixed(1); }
function readAsDataURL() { return Promise.resolve('data:image/png;base64,AAAA'); }
function stripDataURLPrefix(s) { return s.slice(s.indexOf(',') + 1); }
async function uploadAttachment(f) { return { name: f.name, size: f.size, path: '/up/' + f.name }; }
function renderAttachmentThumbs(box, list) { box.innerHTML = ''; list.forEach((a) => { const d = document.createElement('div'); d.className = 'attached-thumb'; d.textContent = a.name; box.appendChild(d); }); }
function md(s) { return escapeHtml(String(s)); }
function boardArtifactGroups(list) { return [{ title: '第 1 轮', items: list.map((a, i) => ({ artifact: a, index: i })) }]; }
function boardArtifactRoundCard(g) { const d = document.createElement('section'); d.className = 'board-artifact-round'; d.textContent = g.items.map((i) => i.artifact.title).join(','); return d; }
`;

function run(w, code, name) {
  const s = w.document.createElement('script');
  s.textContent = code + '\n//# sourceURL=' + (name || 'inline.js');
  w.document.body.appendChild(s);
}

// 建一个装好全部 agent*.js 的页面。opts.api：'METHOD path' → 返回值 / 函数；opts.mode：预置 localStorage 里的模式。
function makeEnv(opts = {}) {
  const dom = new JSDOM(HTML, { runScripts: 'dangerously', url: 'http://localhost/', pretendToBeVisual: true });
  const w = dom.window;
  w.__paSkipInit = true;
  w.__calls = [];
  w.__toasts = [];
  w.__dropped = [];
  w.__api = opts.api || {};
  Object.defineProperty(w, 'innerWidth', { value: opts.width || 1440, configurable: true, writable: true });
  if (opts.mode) w.localStorage.setItem('cc-webui:mode', opts.mode);
  run(w, STUBS + GRABBED.map(grabFn).join('\n'), 'stubs.js');
  for (const f of ['agent.js', 'agent-queue.js', 'agent-aside.js', 'agent-fold.js']) run(w, read(f), f);
  return { dom, w, doc: w.document, ev: (code) => w.eval(code) };
}

// 一份常用的假数据：不同 kind 的队列条目 + 对应看板卡。
function sampleNeeds(now) {
  const base = { server_time: now, window_ms: 55000, board: { ok: true } };
  const items = [
    { id: 'perm:p1', kind: 'perm', state: 'live', pending_id: 'p1', tool: 'Bash', input: { command: 'git push --force-with-lease origin x', description: '覆盖远端分支' }, session_id: 's-pay', session_name: 'w·支付重构', task_id: 'refund', task_title: '支付网关：退款链路迁移', task_status: '进行中', title: 'Bash：git push --force-with-lease origin x', created_at: now - 20000, expires_at: now + 35000 },
    { id: 'ask:a1', kind: 'ask', state: 'live', pending_id: 'a1', tool: 'AskUserQuestion', input: { questions: [{ header: '口径', question: '退款失败时怎么处理？', multiSelect: false, options: [{ label: '原路退回', description: '回到原渠道' }, { label: '退到余额' }] }] }, session_id: 's-login', session_name: 'w·登录改版', task_id: 'login', task_title: '登录页改版', task_status: '待验收', title: '退款失败时怎么处理？', created_at: now - 10000, expires_at: now + 45000 },
    { id: 'blocked:shard', kind: 'blocked', state: 'open', session_id: 's-mig', session_name: 'w·数据迁移', task_id: 'shard', task_title: '订单库分表迁移', task_status: '待人工', title: '订单库分表迁移', summary: '全量迁移超出变更窗口', created_at: now - 3600000 },
    { id: 'review:login', kind: 'review', state: 'open', session_id: 's-login', session_name: 'w·登录改版', task_id: 'login', task_title: '登录页改版', task_status: '待验收', title: '登录页改版', summary: '交付物已上传', created_at: now - 1500000 },
    { id: 'review:nobind', kind: 'review', state: 'open', task_id: 'docs', task_title: '文档同步', task_status: '待验收', title: '文档同步', created_at: now - 600000 },
  ];
  return Object.assign({ items, counts: { all: 5, perm: 1, ask: 1, blocked: 1, review: 2 } }, base);
}
function sampleSessions() {
  const mk = (id, name, extra) => Object.assign({ id, name, peer_name: name, status: { running: false, queued: 0 }, cwd: '/w/' + id, engine: 'claude' }, extra);
  return [
    mk('s-main', 'mainAgent', { main_agent: true, resident: true }),
    mk('s-pay', 'w·支付重构', { parent_id: 's-main', status: { running: true, queued: 0 } }),
    mk('s-login', 'w·登录改版', { parent_id: 's-main' }),
    mk('s-mig', 'w·数据迁移', { parent_id: 's-main' }),
  ];
}
function sampleBoard() {
  const task = (id, title, status, sid, extra) => Object.assign({ id, title, full_title: title, status, session: sid, worker: sid, updated_at: '2026-11-13 10:00:00', project: 'P', timeline: [], artifacts: [] }, extra);
  return { tasks: [
    task('refund', '支付网关：退款链路迁移', '进行中', 's-pay', { timeline: [{ at: '2026-11-13 09:00:00', kind: '评论', text: '回调幂等键已加' }] }),
    task('login', '登录页改版', '待验收', 's-login', { artifacts: [{ id: 'a1', title: 'v2.html', kind: 'html', round: 1 }] }),
    task('shard', '订单库分表迁移', '待人工', 's-mig', { timeline: [{ at: '2026-11-13 09:30:00', kind: '受阻', text: '全量迁移预计 3 小时 40 分' }] }),
  ] };
}

module.exports = { makeEnv, sampleNeeds, sampleSessions, sampleBoard, grabFn };
