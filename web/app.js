/* cc-webui frontend */
'use strict';

marked.setOptions({
  breaks: true,
  highlight(code, lang) {
    if (lang && hljs.getLanguage(lang)) {
      try { return hljs.highlight(code, { language: lang }).value; } catch (_) {}
    }
    return code;
  },
});
// 识别指向本地文件的链接（claude 常在回复里写绝对/相对路径，:N 为行号）。
// 返回 {path, line}；非本地路径返回 null。
function parseFileLink(href) {
  if (!href || href.startsWith('#') || href.startsWith('//')) return null;
  if (/^[a-z][a-z0-9+.-]*:/i.test(href)) return null; // http: mailto: 等外部协议
  let p, line = 0;
  try { p = decodeURIComponent(href); } catch (_) { p = href; }
  const m = p.match(/:(\d+)$/); // 尾部 :行号
  if (m) { line = parseInt(m[1], 10); p = p.slice(0, m.index); }
  if (p.startsWith('/')) return { path: p, line }; // 绝对路径
  // 相对路径：基于当前会话 cwd 解析
  const cwd = state.current && state.current.cwd;
  if (cwd && !p.startsWith('..')) return { path: cwd.replace(/\/$/, '') + '/' + p, line };
  return null;
}

// 代码块复制按钮的图标：直接内联 lucide copy 的 SVG，不走 <i data-lucide>。
// 流式期间 md() 每帧重绘，renderIcons 每帧扫描会刷日志且没必要。
const CODE_COPY_ICON = '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round"><rect width="14" height="14" x="8" y="8" rx="2" ry="2"/><path d="M4 16c-1.1 0-2-.9-2-2V4c0-1.1.9-2 2-2h10c1.1 0 2 .9 2 2"/></svg>';

// Render markdown to sanitized HTML; force all links to open in a new tab
// so clicking a link never navigates away from the chat.
const md = (text) => {
  const tpl = document.createElement('template');
  tpl.innerHTML = DOMPurify.sanitize(marked.parse(text || ''));
  tpl.content.querySelectorAll('a[href]').forEach((a) => {
    const file = parseFileLink(a.getAttribute('href'));
    if (file) {
      // 本地文件链接：交由文件查看器在侧栏打开，而不是跳转到 404
      a.dataset.filePath = file.path;
      if (file.line) a.dataset.fileLine = String(file.line);
      a.classList.add('file-link');
      a.href = '#'; // 占位，点击由 #messages 委托拦截，不会跳转
      return;
    }
    a.target = '_blank';
    a.rel = 'noopener noreferrer';
  });
  // 代码块：包一层 .code-block 容器，右上角悬浮复制按钮（点击走全局委托）。
  // 按钮放在 pre 外面而不是里面：pre 横向滚动时按钮不跟着内容跑。
  tpl.content.querySelectorAll('pre').forEach((pre) => {
    const wrap = document.createElement('div');
    wrap.className = 'code-block';
    const btn = document.createElement('button');
    btn.className = 'code-copy-btn';
    btn.type = 'button';
    btn.title = '复制代码';
    btn.innerHTML = CODE_COPY_ICON;
    pre.replaceWith(wrap);
    wrap.append(btn, pre);
  });
  return tpl.innerHTML;
};

const $ = (sel) => document.querySelector(sel);
const state = {
  clis: [],
  providers: [],
  profilesSig: null, // 最近一次拉取 clis/providers 的签名；config 热加载时据此判断是否真有变动
  sessions: [],
  archived: [],
  current: null,      // current session object
  es: null,
  pendingBySid: {},   // sessionId -> 待发送消息 {text, images}；本轮结束后自动提交，切会话不丢
  seenSeq: new Set(), // seqs already rendered on this connection
  toolBlocks: {},     // tool_use_id -> details element
  subagentCards: {},  // parent_tool_use_id -> 子代理卡片 {summary, body, count, label}
  agentToolMeta: {},  // Agent tool_use_id -> {title, type}（实时侧卡片标题来源）
  nearBottom: true,
  home: '',           // server home dir, for ~ path shortening
  drafts: {},         // sessionId -> {text, attachments}
  attached: [],       // 待发送附件：[{kind:'image',media_type,data} | {kind:'file',name,size,path}]
  nsAttached: [],     // images staged in the new-session composer
  // workspace sidebar
  showSearch: false,
  searchQuery: '',
  viewGroup: 'workspace', // 'workspace' | 'none'
  viewSort: 'updated',     // 'manual' | 'updated'
  collapsedGroups: new Set(),
  // history pagination: replayed events are buffered, only the latest
  // HISTORY_PAGE events are rendered; older ones load on demand.
  historyBuf: [],      // all replayed events (oldest first)
  histRenderedFrom: 0, // index in historyBuf of the oldest rendered event
  replaying: false,    // true until the server sends the replay_end marker
  renderingHistory: false, // 正在渲染回放/翻页历史（旧 status 事件不触发 flushPending 等副作用）
  stats: null,         // {turns, steps, firstTs, lastTs} for the current session
  live: null,          // live partial-stream state: {msgId, blocks:[{kind,el,bodyEl,buf,done,taken,raf}]}
  // 任务面板折叠状态，见「任务列表面板」段；数据源是 historyBuf
  taskFold: { tasks: {}, order: [], pendingCreates: {}, pendingLists: {}, todos: null },
  taskPanelOpen: true,   // 面板展开；未手动操作过：全完成自动收起、来新任务自动展开
  taskPanelTouched: false, // 本会话手动开合过 → 停止自动开合
  slashCommandsCache: (() => { try { return JSON.parse(localStorage.getItem('cc-webui:slash-cache') || '{}'); } catch { return {}; } })(),
};

// 切换会话时首屏只渲染最新 N 条事件，向上滚动点「加载更多」再翻页
const HISTORY_PAGE = 50;

// 收集当前 session 的待发送内容
function draft() {
  if (!state.current) return { text: '', attachments: [] };
  const id = state.current.id;
  if (!state.drafts[id]) state.drafts[id] = { text: '', attachments: [] };
  return state.drafts[id];
}

// ---------- theme ----------
function applyTheme(t, persist) {
  document.documentElement.dataset.theme = t;
  $('#hljs-light').disabled = t === 'dark';
  $('#hljs-dark').disabled = t !== 'dark';
  const btn = $('#btn-theme');
  if (btn) btn.innerHTML = `<i data-lucide="${t === 'dark' ? 'sun' : 'moon'}"></i>`;
  renderIcons(btn);
  if (persist) localStorage.setItem('cc-webui-theme', t);
}
$('#btn-theme').onclick = () => {
  const next = document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark';
  applyTheme(next, true);
};

// lucide icons: replace <i data-lucide="xxx"> with <svg>....
// Falls back to inner text if lucide isn't loaded.
function renderIcons(root) {
  const scope = root || document;
  if (!window.lucide || !window.lucide.createIcons) {
    console.warn('[lucide] not loaded yet');
    return;
  }
  // lucide 内部把 "chevron-down" 转 "ChevronDown" 再去查 icons map（属性值是 kebab-case）
  const all = window.lucide.icons || {};
  const wanted = [
    'moon', 'sun', 'plus',
    'chevron-down', 'paperclip', 'square', 'terminal', 'trash-2',
    'copy', 'check', 'x', 'send',
    'alert-triangle', 'file', 'file-text', 'file-plus', 'terminal-square',
    'rotate-ccw', 'pencil', 'search',
    'more-horizontal', 'archive', 'archive-restore', 'git-fork', 'git-branch',
    'settings-2', 'arrow-up', 'settings', 'bar-chart-3',
    'menu', 'more-vertical', 'send-horizontal', 'folder',
    'panel-left-close', 'panel-left-open', 'list-collapse',
    'bot',
    'list-todo', 'circle', 'circle-check',
  ];
  // lucide 自动 kebab→Pascal，所以 icons map 的 key 也用 PascalCase
  const toPascal = (s) =>
    s.replace(/-([a-z0-9])/g, (_, c) => c.toUpperCase())
     .replace(/^[a-z]/, (c) => c.toUpperCase());
  const icons = {};
  const missing = [];
  for (const k of wanted) {
    // lucide 把 "chevron-down" 转 "ChevronDown" 后查 icons map，map 必须用 PascalCase key
    const pascal = toPascal(k);
    if (all[pascal]) icons[pascal] = all[pascal];
    else missing.push(k);
  }
  const matches = scope.querySelectorAll('[data-lucide]').length;
  console.log('[lucide] found', matches, 'data-lucide elements; icons:', Object.keys(icons).length, 'missing:', missing);
  window.lucide.createIcons({
    icons,
    attrs: { 'stroke-width': 1.75 },
    scope,
  });
}

// ---------- API ----------
async function api(path, opts = {}) {
  const res = await fetch(path, {
    headers: { 'Content-Type': 'application/json' },
    ...opts,
  });
  if (res.status === 401) {
    location.href = '/login.html';
    throw new Error('unauthorized');
  }
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new Error(body.error || `HTTP ${res.status}`);
  }
  return res.json();
}

// ---------- 任务完成通知（系统横幅 + 提示音） ----------
// 开关存 localStorage，跟着浏览器走：多个终端同时连着时各自决定是否提醒。
// 触发源是 loadSessions() 轮询后的 running 状态 diff（SSE 只订阅当前打开的
// 会话，轮询才能覆盖其他会话）。两轮之间的调度间隙 running=false 但
// queued>0，要求队列排空才算整轮跑完；interrupted（主动停止）不提醒。
// last_error 每轮结束时被后端覆盖（成功清空），所以 diff 时读到的就是本轮结果。
// 同一个 diff 也负责打「未读完成」标记（见下），通知可关，未读标记始终打。
const NOTIFY_KEY = 'cc-webui-notify';
function notifyPrefs() {
  try {
    const p = JSON.parse(localStorage.getItem(NOTIFY_KEY) || '{}');
    return { banner: p.banner !== false, sound: p.sound !== false }; // 默认全开
  } catch (_) {
    return { banner: true, sound: true };
  }
}
function saveNotifyPrefs(p) {
  localStorage.setItem(NOTIFY_KEY, JSON.stringify(p));
}

// ---------- 未读完成标记（会话蓝点 + 工作区栏计数） ----------
// 跑完但还没点开过的会话集合，localStorage 持久化（和通知开关一样按浏览器走）。
// 正开着该会话且页面可见时不标（结果就在眼前）；点开会话即清除，蓝点恢复灰色。
const UNREAD_KEY = 'cc-webui-unread-done';
const unreadDone = new Set(JSON.parse(localStorage.getItem(UNREAD_KEY) || '[]'));
function saveUnreadDone() {
  try { localStorage.setItem(UNREAD_KEY, JSON.stringify([...unreadDone])); } catch {}
}
function markUnreadDone(id) {
  if (unreadDone.has(id)) return;
  unreadDone.add(id);
  saveUnreadDone();
}
function clearUnreadDone(id) {
  if (!unreadDone.delete(id)) return;
  saveUnreadDone();
}

// sessionId -> 活跃（running 或 queued>0）；每轮轮询把快照存 localStorage，下次打开
// 页面时拿它当基线——关页期间跑完的任务也能检出并标未读（但属旧闻，不弹横幅/出声，
// 通知只给在线转变）。没有快照时（首次使用）纯建基线，不把存量活跃当转变。
// 注意快照要把 queued 也算活跃：两轮之间的调度间隙 running=false 但 queued>0，
// 若只记 running，间隙那轮会把「曾在跑」的记忆冲掉，队列排空时就检不出完成了。
const RUN_SNAPSHOT_KEY = 'cc-webui-running-snapshot';
let runPrev = null;
function diffRunningSessions(list) {
  const boot = runPrev === null;
  let prev = runPrev;
  if (!prev) {
    try { prev = JSON.parse(localStorage.getItem(RUN_SNAPSHOT_KEY) || 'null'); } catch { prev = null; }
  }
  const now = {};
  for (const s of list) now[s.id] = !!(s.status && (s.status.running || s.status.queued > 0));
  runPrev = now;
  try { localStorage.setItem(RUN_SNAPSHOT_KEY, JSON.stringify(now)); } catch {}
  if (!prev) return;
  for (const s of list) {
    if (!prev[s.id]) continue; // 只有 活跃 → 停止 的转变才算跑完
    const st = s.status || {};
    if (st.running || st.queued > 0) continue;
    const err = st.last_error || '';
    if (err === 'interrupted') continue;
    const watching = state.current && state.current.id === s.id && document.visibilityState === 'visible';
    if (!watching) markUnreadDone(s.id);
    if (!boot) notifyTurnDone(s, err);
  }
}

function notifyTurnDone(s, err) {
  const prefs = notifyPrefs();
  const title = (err ? '任务出错：' : '任务完成：') + (s.name || '会话');
  const body = err ? String(err).slice(0, 120) : (s.cwd || '');
  if (prefs.sound) playNotifySound(!!err);
  if (prefs.banner) showBannerNotify(title, body, s.id);
}

// 系统横幅通知。点击聚焦窗口并打开对应会话；tag 防止同一会话的通知堆叠。
function showBannerNotify(title, body, sessionId) {
  if (!('Notification' in window)) return;
  if (Notification.permission !== 'granted') return; // 授权在设置页/首次交互时请求
  try {
    const n = new Notification(title, {
      body,
      icon: '/icons/icon-192.png',
      tag: 'cc-webui-done-' + sessionId,
    });
    n.onclick = () => {
      window.focus();
      if (sessionId) openSession(sessionId).catch(() => {});
      n.close();
    };
  } catch (_) {}
}

// 提示音用 Web Audio 合成，无需音频文件。完成 = 上行双音；出错 = 下行双音。
let audioCtx = null;
function getAudioCtx() {
  const AC = window.AudioContext || window.webkitAudioContext;
  if (!AC) return null;
  if (!audioCtx) audioCtx = new AC();
  if (audioCtx.state === 'suspended') audioCtx.resume().catch(() => {});
  return audioCtx;
}
function playNotifySound(isError) {
  const ctx = getAudioCtx();
  if (!ctx) return;
  const notes = isError ? [440, 330] : [659.25, 880];
  notes.forEach((freq, i) => {
    const t0 = ctx.currentTime + i * 0.18;
    const osc = ctx.createOscillator();
    const gain = ctx.createGain();
    osc.type = 'sine';
    osc.frequency.value = freq;
    gain.gain.setValueAtTime(0, t0);
    gain.gain.linearRampToValueAtTime(0.18, t0 + 0.02);
    gain.gain.exponentialRampToValueAtTime(0.001, t0 + 0.16);
    osc.connect(gain).connect(ctx.destination);
    osc.start(t0);
    osc.stop(t0 + 0.18);
  });
}

// autoplay 策略：AudioContext 与通知权限请求都要求用户手势上下文。
// 在首次交互时一次性预热/请求，之后轮询回调里才能真正出声、弹横幅。
let notifyUnlocked = false;
document.addEventListener('pointerdown', () => {
  if (notifyUnlocked) return;
  notifyUnlocked = true;
  const prefs = notifyPrefs();
  if (prefs.sound) getAudioCtx();
  if (prefs.banner && 'Notification' in window && Notification.permission === 'default') {
    Notification.requestPermission().catch(() => {});
  }
}, { capture: true });

// ---------- sessions list ----------
async function loadSessions() {
  const [active2, archived] = await Promise.all([
    api('/api/sessions'),
    api('/api/sessions?archived=true').catch(() => []),
  ]);
  state.sessions = active2;
  state.archived = archived;
  diffRunningSessions(active2);
  renderSessionList();
  renderArchiveList();
  renderWsActivity();
}

function statusDot(s) {
  if (s.status.running) return 'dot running';
  if (s.status.queued > 0) return 'dot queued';
  if (s.status.last_error && s.status.last_error !== 'interrupted') return 'dot error';
  if (unreadDone.has(s.id)) return 'dot unread'; // 跑完未查看 → 蓝色，点开恢复灰
  return 'dot';
}

function timeAgo(ts) {
  const d = new Date(ts);
  const diff = (Date.now() - d.getTime()) / 1000;
  if (diff < 60) return '刚刚';
  if (diff < 3600) return `${Math.floor(diff / 60)} 分钟前`;
  if (diff < 86400) return `${Math.floor(diff / 3600)} 小时前`;
  return d.toLocaleDateString();
}

function formatMsgTime(ts) {
  const d = ts ? new Date(ts) : new Date();
  if (isNaN(d.getTime())) return '';
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

function shortenPath(p) {
  if (state.home && (p === state.home || p.startsWith(state.home + '/'))) {
    return '~' + p.slice(state.home.length);
  }
  return p;
}

// 折叠的目录组（按 cwd 记，localStorage 持久化）
const collapsedGroups = new Set(
  JSON.parse(localStorage.getItem('cc-webui-collapsed-groups') || '[]')
);
function saveCollapsedGroups() {
  localStorage.setItem('cc-webui-collapsed-groups', JSON.stringify([...collapsedGroups]));
}

// ---------- session-item rendering ----------
function sessionItemHTML(s, opts = {}) {
  const active = state.current && state.current.id === s.id ? ' active' : '';
  // 常驻会话只留紫点一个（运行态合并成紫点脉冲）；非常驻走原状态点，
  // 避免两个点并排的视觉噪音。
  const mark = s.resident
    ? `<span class="resident-mark on${s.status.running ? ' running' : ''}"></span>`
    : `<span class="${statusDot(s)}"></span>`;
  return `
    <button class="icon-btn menu-btn" title="更多操作" data-menu-for="${s.id}"><i data-lucide="more-horizontal"></i></button>
    <div class="name">${mark}<span class="name-text"></span></div>
    <div class="meta">
      <span class="profile-tag"></span>
      <span class="time">${timeAgo(s.updated_at)}</span>
    </div>`;
}

function bindSessionItem(item, s, isArchived = false) {
  item.querySelector('.name-text').textContent = s.name;
  item.querySelector('.profile-tag').textContent = s.provider || s.cli;
  item.title = `${s.cli} / ${s.provider} · ${s.cwd}`;
  item.onclick = (e) => {
    if (e.target.closest('.menu-btn')) return; // let menu-btn handle its own
    openSession(s.id);
  };
  item.querySelector('.menu-btn').onclick = (e) => {
    e.stopPropagation();
    openSessionMenu(s, item, isArchived);
  };
}

// ---------- sessions list rendering (filtered + grouped) ----------
// 工作区栏活动指示：运行中（含排队）计数 + 跑完未查看计数；都没有时隐藏。
// 徽章复用 .dot 配色，与会话列表里的状态点一致。
function renderWsActivity() {
  const el = $('#ws-activity');
  if (!el) return;
  let running = 0, unread = 0;
  const known = new Set(state.archived.map((s) => s.id));
  for (const s of state.sessions) {
    known.add(s.id);
    const st = s.status || {};
    if (st.running || st.queued > 0) running++;
    if (unreadDone.has(s.id)) unread++;
  }
  // 清理已彻底删除的会话残留的未读标记
  let pruned = false;
  for (const id of unreadDone) if (!known.has(id)) { unreadDone.delete(id); pruned = true; }
  if (pruned) saveUnreadDone();
  el.innerHTML = '';
  if (!running && !unread) { el.classList.add('hidden'); return; }
  el.classList.remove('hidden');
  if (running) {
    const b = document.createElement('span');
    b.className = 'ws-badge';
    b.dataset.kind = 'running';
    b.title = `${running} 个任务运行中，悬停或点击查看`;
    b.innerHTML = `<span class="dot running"></span>${running}`;
    bindWsBadge(b, 'running');
    el.appendChild(b);
  }
  if (unread) {
    const b = document.createElement('span');
    b.className = 'ws-badge';
    b.dataset.kind = 'unread';
    b.title = `${unread} 个任务已完成，点击查看`;
    b.innerHTML = `<span class="dot unread"></span>${unread}`;
    bindWsBadge(b, 'unread');
    el.appendChild(b);
  }
  // 轮询重建徽章时 popover 若还开着，续刷内容——旧徽章被替换后鼠标
  // mouseleave 不会触发，不能让它停在过期数据上
  if (wsPopKind && !$('#ws-pop').classList.contains('hidden')) {
    renderWsPop();
    if (wsPopAnchor) positionWsPop(wsPopAnchor);
  }
}

// ---------- 工作区任务 popover ----------
// 徽章悬停/点击展开对应状态的任务列表，点击条目跳转会话。
// wsPopKind 记录当前打开的类型，轮询刷新 renderWsActivity 时据此续刷。
let wsPopKind = null;
let wsPopAnchor = null;
let wsPopHideTimer = null;

function bindWsBadge(badge, kind) {
  badge.addEventListener('mouseenter', () => openWsPop(kind, badge));
  badge.addEventListener('mouseleave', () => scheduleWsPopHide(250));
  badge.addEventListener('click', (e) => {
    e.stopPropagation(); // 走下方 pointerdown 关闭之前不给 document 级监听机会
    openWsPop(kind, badge);
  });
}

function openWsPop(kind, anchor) {
  cancelWsPopHide();
  wsPopKind = kind;
  wsPopAnchor = anchor;
  renderWsPop();
  // 先解除 hidden 再定位：offsetHeight 在 display:none 时为 0，位置钳制会失真
  $('#ws-pop').classList.remove('hidden');
  positionWsPop(anchor);
}

function closeWsPop() {
  wsPopKind = null;
  wsPopAnchor = null;
  cancelWsPopHide();
  $('#ws-pop').classList.add('hidden');
}

function cancelWsPopHide() {
  if (wsPopHideTimer) { clearTimeout(wsPopHideTimer); wsPopHideTimer = null; }
}

function scheduleWsPopHide(delay) {
  cancelWsPopHide();
  wsPopHideTimer = setTimeout(closeWsPop, delay);
}

function positionWsPop(anchor) {
  const pop = $('#ws-pop');
  const rect = anchor.getBoundingClientRect();
  const pw = 280, ph = pop.offsetHeight || 240;
  let x = rect.left;
  let y = rect.bottom + 6;
  x = Math.max(8, Math.min(x, window.innerWidth - pw - 8));
  if (y + ph > window.innerHeight - 8) y = Math.max(8, rect.top - ph - 6);
  pop.style.left = x + 'px';
  pop.style.top = y + 'px';
}

function renderWsPop() {
  const pop = $('#ws-pop');
  pop.innerHTML = '';
  if (!wsPopKind) return;
  const byId = new Map(state.sessions.map((s) => [s.id, s]));
  for (const s of state.archived) byId.set(s.id, s);
  const byNewest = (a, b) => new Date(b.updated_at) - new Date(a.updated_at);
  let list;
  if (wsPopKind === 'running') {
    list = state.sessions.filter((s) => s.status && (s.status.running || s.status.queued > 0));
  } else {
    list = [...unreadDone].map((id) => byId.get(id)).filter(Boolean);
  }
  list.sort(byNewest);
  const title = document.createElement('div');
  title.className = 'ws-pop-title';
  title.textContent = (wsPopKind === 'running' ? '运行中' : '已完成') + `（${list.length}）`;
  pop.appendChild(title);
  if (!list.length) {
    const empty = document.createElement('div');
    empty.className = 'ws-pop-empty';
    empty.textContent = '暂无任务';
    pop.appendChild(empty);
    return;
  }
  for (const s of list) {
    const item = document.createElement('button');
    item.className = 'ws-pop-item';
    const dot = document.createElement('span');
    dot.className = 'dot ' + (wsPopKind === 'running'
      ? (s.status.running ? 'running' : 'queued')
      : 'unread');
    const name = document.createElement('span');
    name.className = 'name-text';
    name.textContent = s.name || '未命名会话';
    const meta = document.createElement('span');
    meta.className = 'meta';
    if (wsPopKind === 'running') {
      meta.textContent = s.status.running ? shortenPath(s.cwd || '') : '排队中';
    } else {
      meta.textContent = `完成于 ${timeAgo(s.updated_at)}`;
    }
    item.append(dot, name, meta);
    item.onclick = () => {
      closeWsPop();
      openSession(s.id);
    };
    pop.appendChild(item);
  }
}

// 鼠标从徽章滑入 popover 时不关；滑出再延迟关
$('#ws-pop').addEventListener('mouseenter', cancelWsPopHide);
$('#ws-pop').addEventListener('mouseleave', () => scheduleWsPopHide(300));

// pointerdown 关闭（同 session-menu 的理由）；落在徽章/popover 上的跳过
document.addEventListener('pointerdown', (e) => {
  if (!wsPopKind) return;
  if (e.target.closest('#ws-pop') || e.target.closest('.ws-badge')) return;
  closeWsPop();
});

function renderSessionList() {
  const el = $('#session-list');
  el.innerHTML = '';

  // search filter
  const q = state.searchQuery.trim().toLowerCase();
  let list = state.sessions;
  if (q) list = list.filter((s) => (s.name || '').toLowerCase().includes(q));

  // sort
  const residentFirst = (a, b) => (b.resident ? 1 : 0) - (a.resident ? 1 : 0);
  const byUpdatedDesc = (a, b) => new Date(b.updated_at) - new Date(a.updated_at);
  if (state.viewSort === 'manual') {
    // manual: by sort_order desc, fallback to resident first, then updated_at
    list = [...list].sort((a, b) => {
      const ao = a.sort_order || 0, bo = b.sort_order || 0;
      if (ao !== bo) return bo - ao;
      const r = residentFirst(a, b);
      if (r) return r;
      return byUpdatedDesc(a, b);
    });
  } else {
    // default: resident pinned to top within each project directory, then
    // newest first
    list = [...list].sort((a, b) => {
      const r = residentFirst(a, b);
      return r || byUpdatedDesc(a, b);
    });
  }

  if (!list.length) {
    const empty = document.createElement('div');
    empty.style.cssText = 'padding:24px 12px;color:var(--dim);font-size:12px;text-align:center';
    empty.textContent = q ? '没有匹配的会话' : '暂无会话';
    el.appendChild(empty);
    renderIcons(el);
    return;
  }

  if (state.viewGroup === 'none') {
    const flat = document.createElement('div');
    flat.className = 'session-list-flat';
    for (const s of list) {
      const item = document.createElement('div');
      item.className = 'session-item' + (state.current && state.current.id === s.id ? ' active' : '');
      item.dataset.sessionId = s.id;
      item.innerHTML = sessionItemHTML(s);
      bindSessionItem(item, s);
      flat.appendChild(item);
    }
    el.appendChild(flat);
    renderIcons(el);
    return;
  }

  // group by cwd
  const groups = new Map();
  for (const s of list) {
    const key = s.cwd || '(无目录)';
    if (!groups.has(key)) groups.set(key, []);
    groups.get(key).push(s);
  }
  const ordered = [...groups.entries()].sort((a, b) => {
    const ta = Math.max(...a[1].map((s) => new Date(s.updated_at).getTime()));
    const tb = Math.max(...b[1].map((s) => new Date(s.updated_at).getTime()));
    return tb - ta;
  });

  for (const [cwd, sessions] of ordered) {
    const group = document.createElement('div');
    group.className = 'session-group' + (collapsedGroups.has(cwd) ? ' collapsed' : '');

    const header = document.createElement('div');
    header.className = 'group-header';
    header.innerHTML = `<span class="chev"><i data-lucide="chevron-down"></i></span><i data-lucide="folder" class="dir-icon"></i><span class="dir"></span><span class="count">${sessions.length}</span><button class="group-add-btn icon-btn" title="在此工作区新建会话"><i data-lucide="plus"></i></button>`;
    header.querySelector('.dir').textContent = shortenPath(cwd);
    header.title = cwd;
    renderIcons(header);
    header.onclick = (e) => {
      if (e.target.closest('.group-add-btn')) return;
      if (collapsedGroups.has(cwd)) {
        collapsedGroups.delete(cwd);
        group.classList.remove('collapsed');
      } else {
        collapsedGroups.add(cwd);
        group.classList.add('collapsed');
      }
      saveCollapsedGroups();
    };
    // "+" button: jump to new-session composer with this cwd pre-filled
    header.querySelector('.group-add-btn').onclick = (e) => {
      e.stopPropagation();
      openNewSessionView(cwd);
    };
    group.appendChild(header);

    for (const s of sessions) {
      const item = document.createElement('div');
      item.className = 'session-item' + (state.current && state.current.id === s.id ? ' active' : '');
      item.dataset.sessionId = s.id;
      item.innerHTML = sessionItemHTML(s);
      bindSessionItem(item, s);
      group.appendChild(item);
    }
    el.appendChild(group);
  }
  renderIcons(el);
}

// ---------- archive list ----------
function renderArchiveList() {
  const el = $('#archive-list');
  el.innerHTML = '';
  document.querySelector('.archive-count').textContent = state.archived.length;
  if (!state.archived.length) {
    const empty = document.createElement('div');
    empty.style.cssText = 'padding:12px;color:var(--dim);font-size:11.5px;text-align:center';
    empty.textContent = '暂无归档';
    el.appendChild(empty);
    renderIcons(document.getElementById('archive-section'));
    return;
  }
  for (const s of state.archived) {
    const item = document.createElement('div');
    item.className = 'session-item';
    item.dataset.sessionId = s.id;
    item.innerHTML = sessionItemHTML(s);
    bindSessionItem(item, s, true);
    el.appendChild(item);
  }
  renderIcons(el);
}

// ---------- session actions menu ----------
let menuTargetSession = null;

function openSessionMenu(s, anchor, isArchived) {
  menuTargetSession = s;
  syncMenuForEngine(s);
  const menu = $('#session-menu');
  // toggle action labels based on whether the session is archived
  const archiveBtn = menu.querySelector('[data-action="archive"]');
  archiveBtn.innerHTML = isArchived
    ? '<i data-lucide="archive-restore"></i>取消归档'
    : '<i data-lucide="archive"></i>归档';
  menu.classList.remove('hidden');
  const rect = anchor.getBoundingClientRect();
  const mw = 180, mh = 160;
  let x = rect.right + 4;
  let y = rect.top - 4;
  // 右侧放不下则右对齐锚点右缘，再钳回视口内（手机抽屉场景）
  if (x + mw > window.innerWidth - 8) x = rect.right - mw;
  x = Math.max(8, Math.min(x, window.innerWidth - mw - 8));
  if (y + mh > window.innerHeight - 8) y = window.innerHeight - mh - 8;
  if (y < 8) y = 8;
  menu.style.left = x + 'px';
  menu.style.top = y + 'px';
  renderIcons(menu);
}

function closeSessionMenu() {
  $('#session-menu').classList.add('hidden');
  menuTargetSession = null;
}

// pointerdown 关闭（click 在触屏重渲染场景可能丢失）；
// 落在 ⋯ 按钮上的 tap 由按钮自己的 pointerdown 处理，跳过
document.addEventListener('pointerdown', (e) => {
  if (e.target.closest('#session-menu') || e.target.closest('.menu-btn')) return;
  closeSessionMenu();
});

$('#session-menu').addEventListener('click', async (e) => {
  const btn = e.target.closest('button[data-action]');
  if (!btn) return;
  const action = btn.dataset.action;
  const s = menuTargetSession;
  if (!s) return;
  closeSessionMenu();
  await handleSessionAction(action, s);
});

async function handleSessionAction(action, s) {
  if (action === 'rename') return openRenameDialog(s);
  if (action === 'fork')   return forkSession(s);
  if (action === 'archive') return toggleArchive(s);
  if (action === 'delete') return deleteSession(s);
}

// toggle archived flag based on current state (looked up fresh)
async function toggleArchive(s) {
  const fresh = await api(`/api/sessions/${s.id}`);
  if (fresh.archived) return unarchiveSession(s);
  return archiveSession(s);
}

// 重命名目标独立保存：菜单点击会先 closeSessionMenu() 把
// menuTargetSession 置 null，保存时不能再依赖它
let renameTarget = null;

function openRenameDialog(s) {
  renameTarget = s;
  const input = $('#rename-input');
  input.value = s.name;
  $('#modal-rename').classList.remove('hidden');
  setTimeout(() => input.focus(), 0);
}

function closeRenameDialog() {
  $('#modal-rename').classList.add('hidden');
  renameTarget = null;
}

async function submitRename() {
  const name = $('#rename-input').value.trim();
  if (!name) return;
  const s = renameTarget;
  closeRenameDialog();
  if (s) {
    await api(`/api/sessions/${s.id}`, {
      method: 'PATCH', body: JSON.stringify({ name }),
    });
    s.name = name;
    loadSessions();
  }
}

$('#rename-submit').onclick = submitRename;

// 回车保存、Esc 取消；isComposing 防止中文输入法选词的回车误提交
$('#rename-input').onkeydown = (e) => {
  if (e.key === 'Enter' && !e.isComposing) { e.preventDefault(); submitRename(); }
  else if (e.key === 'Escape') { e.preventDefault(); closeRenameDialog(); }
};

async function forkSession(s) {
  try {
    const newS = await api(`/api/sessions/${s.id}/fork`, { method: 'POST' });
    await loadSessions();
    openSession(newS.id);
  } catch (e) { alert('分叉失败：' + e.message); }
}

async function archiveSession(s) {
  try {
    await api(`/api/sessions/${s.id}`, {
      method: 'PATCH', body: JSON.stringify({ archived: true }),
    });
    if (state.current && state.current.id === s.id) {
      state.current = null;
      $('#chat-view').classList.add('hidden');
      $('#empty-state').classList.remove('hidden');
    }
    await loadSessions();
  } catch (e) { alert('归档失败：' + e.message); }
}

async function unarchiveSession(s) {
  try {
    await api(`/api/sessions/${s.id}`, {
      method: 'PATCH', body: JSON.stringify({ archived: false }),
    });
    await loadSessions();
  } catch (e) { alert('取消归档失败：' + e.message); }
}

async function deleteSession(s) {
  if (!confirm(`彻底删除 "${s.name}"？前端历史会清掉，磁盘上的 claude jsonl 不会被删除（仍可用 --resume 恢复）。`)) return;
  await api(`/api/sessions/${s.id}`, { method: 'DELETE' });
  if (state.current && state.current.id === s.id) {
    state.current = null;
    $('#chat-view').classList.add('hidden');
    $('#empty-state').classList.remove('hidden');
  }
  await loadSessions();
}

// archive section toggle
$('#archive-header').onclick = () => {
  $('#archive-section').classList.toggle('collapsed');
};

// ---------- search bar ----------
$('#btn-search-toggle').onclick = () => {
  state.showSearch = !state.showSearch;
  $('#search-bar').classList.toggle('hidden', !state.showSearch);
  if (state.showSearch) {
    setTimeout(() => $('#search-input').focus(), 0);
  } else {
    $('#search-input').value = '';
    state.searchQuery = '';
    renderSessionList();
  }
};
$('#btn-search-close').onclick = () => $('#btn-search-toggle').click();
$('#search-input').addEventListener('input', (e) => {
  state.searchQuery = e.target.value;
  renderSessionList();
});

// ---------- view menu ----------
const collapsedDirs = new Set(JSON.parse(localStorage.getItem('cc-webui-collapsed-groups') || '[]'));

// load view prefs from localStorage
try {
  const v = JSON.parse(localStorage.getItem('cc-webui-view') || '{}');
  if (v.group) state.viewGroup = v.group;
  if (v.sort) state.viewSort = v.sort;
} catch {}
function saveView() {
  localStorage.setItem('cc-webui-view', JSON.stringify({ group: state.viewGroup, sort: state.viewSort }));
}

function refreshViewMenuChecks() {
  const menu = $('#view-menu');
  menu.querySelectorAll('[data-view-group]').forEach((b) => {
    b.classList.toggle('check', b.dataset.viewGroup === state.viewGroup);
  });
  menu.querySelectorAll('[data-view-sort]').forEach((b) => {
    b.classList.toggle('check', b.dataset.viewSort === state.viewSort);
  });
}

function openViewMenu(anchor) {
  const menu = $('#view-menu');
  refreshViewMenuChecks();
  menu.classList.remove('hidden');
  const rect = anchor.getBoundingClientRect();
  const mw = 200, mh = 200;
  let x = rect.right + 4;
  let y = rect.bottom + 4;
  if (x + mw > window.innerWidth - 8) x = rect.left - mw - 4;
  if (y + mh > window.innerHeight - 8) y = rect.top - mh - 8;
  if (y < 8) y = 8;
  menu.style.left = x + 'px';
  menu.style.top = y + 'px';
  renderIcons(menu);
}

$('#btn-view-toggle').onclick = (e) => {
  e.stopPropagation();
  const menu = $('#view-menu');
  if (menu.classList.contains('hidden')) openViewMenu(e.currentTarget);
  else closeViewMenu();
};

function closeViewMenu() { $('#view-menu').classList.add('hidden'); }

document.addEventListener('click', (e) => {
  if (!e.target.closest('#view-menu') && !e.target.closest('#btn-view-toggle')) closeViewMenu();
});

$('#view-menu').addEventListener('click', (e) => {
  const g = e.target.closest('[data-view-group]');
  const s = e.target.closest('[data-view-sort]');
  if (g) { state.viewGroup = g.dataset.viewGroup; saveView(); refreshViewMenuChecks(); renderSessionList(); }
  if (s) { state.viewSort = s.dataset.viewSort; saveView(); refreshViewMenuChecks(); renderSessionList(); }
});

// ---------- 常驻（peer messaging）面板 ----------
// 右上角 radio 按钮弹出的面板：常驻开关 + 地址名。常驻 = 长驻 streaming-input
// 进程，注册地址名后可被其他 Claude 会话 SendMessage。status.resident /
// status.registered_name 反映进程真实在线情况，打开面板时拉最新值。
function updateResidentUI() {
  const s = state.current;
  const on = !!(s && s.resident);
  $('#btn-resident').classList.toggle('active', on);
  const pop = $('#resident-pop');
  if (pop.classList.contains('hidden')) return;
  $('#resident-toggle').checked = on;
  const st = (s && s.status) || {};
  const el = $('#resident-status');
  if (!on) {
    el.textContent = '未开启——会话是一次性进程，收不到其他会话的消息';
  } else if (!st.resident) {
    el.textContent = '进程启动中…';
  } else {
    // effective = PeerName || CleanPeerName(Name)，跟后端 effectivePeerName 同口径。
    const eff = (s.peer_name || s.name || '').trim();
    const reg = (st.registered_name || '').trim();
    if (reg && eff && reg === eff) {
      el.textContent = `在线 · 注册名 ${reg}`;
    } else if (reg && eff && reg !== eff) {
      el.textContent = `在线 · 待重命名为 ${eff}（当前 ${reg}）`;
    } else if (eff) {
      el.textContent = `在线 · 注册中（将注册为 ${eff}）`;
    } else {
      el.textContent = '在线 · 等待首轮消息后注册地址';
    }
  }
}

function closeResidentPop() {
  $('#resident-pop').classList.add('hidden');
  if (residentPopPoll) { clearInterval(residentPopPoll); residentPopPoll = null; }
}

// 面板打开期间每 1s 拉一次状态：spawn 后 procBootstrap 写好注册名时，
// "将注册为 X" 就能立刻变成 "注册名 X"。
let residentPopPoll = null;

$('#btn-resident').onclick = async (e) => {
  e.stopPropagation();
  const pop = $('#resident-pop');
  if (!pop.classList.contains('hidden')) return closeResidentPop();
  if (!state.current) return;
  // 打开前拉最新状态：注册名/在线情况是进程侧状态，可能刚变
  try { state.current = await api(`/api/sessions/${state.current.id}`); } catch (_) {}
  const s = state.current;
  $('#resident-name').value = s.peer_name || '';
  $('#resident-name').placeholder = s.name || '默认取会话名';
  updateResidentUI();
  pop.classList.remove('hidden');
  if (residentPopPoll) clearInterval(residentPopPoll);
  residentPopPoll = setInterval(async () => {
    if (!state.current || pop.classList.contains('hidden')) return;
    try {
      state.current = await api(`/api/sessions/${state.current.id}`);
      updateResidentUI();
    } catch (_) {}
  }, 1000);
  const rect = $('#btn-resident').getBoundingClientRect();
  const pw = 320, ph = 200;
  let x = rect.right - pw;
  let y = rect.bottom + 6;
  x = Math.max(8, Math.min(x, window.innerWidth - pw - 8));
  if (y + ph > window.innerHeight - 8) y = Math.max(8, rect.top - ph - 6);
  pop.style.left = x + 'px';
  pop.style.top = y + 'px';
};

// pointerdown 关闭（同 session-menu 的理由）；落在按钮/面板上的跳过
document.addEventListener('pointerdown', (e) => {
  if (e.target.closest('#resident-pop') || e.target.closest('#btn-resident')) return;
  closeResidentPop();
});

// 开关常驻：后端 EnsureResident 要等初始化+注册完成，可能花几秒，期间禁用开关
$('#resident-toggle').onchange = async (e) => {
  if (!state.current) return;
  const on = e.target.checked;
  e.target.disabled = true;
  try {
    state.current = await api(`/api/sessions/${state.current.id}`, {
      method: 'PATCH', body: JSON.stringify({ resident: on }),
    });
  } catch (err) {
    alert((on ? '开启常驻失败：' : '关闭常驻失败：') + err.message);
  }
  e.target.disabled = false;
  updateResidentUI();
  renderSessionList(); // 侧栏"常驻"徽章
};

// 地址名：Enter 提交、Esc 还原；失焦（change）也算提交
$('#resident-name').onkeydown = (e) => {
  if (e.key === 'Enter' && !e.isComposing) { e.preventDefault(); e.target.blur(); }
  else if (e.key === 'Escape') {
    e.preventDefault();
    e.target.value = (state.current && state.current.peer_name) || '';
    e.target.blur();
  }
};
$('#resident-name').onchange = async (e) => {
  if (!state.current) return;
  const v = e.target.value.trim();
  if (v === (state.current.peer_name || '')) return;
  e.target.disabled = true;
  try {
    state.current = await api(`/api/sessions/${state.current.id}`, {
      method: 'PATCH', body: JSON.stringify({ peer_name: v }),
    });
    e.target.value = state.current.peer_name || '';
  } catch (err) {
    e.target.value = (state.current && state.current.peer_name) || '';
    alert('修改地址名失败：' + err.message);
  }
  e.target.disabled = false;
  updateResidentUI();
};

// ---------- current session ----------
async function openSession(id) {
  const s = await api(`/api/sessions/${id}`);
  state.current = s;
  // 待发送消息按会话留在 pendingBySid 里，切换不清空；下面随视图重建恢复显示。
  state.seenSeq = new Set();
  state.toolBlocks = {};
  state.subagentCards = {};
  state.agentToolMeta = {};
  state.historyBuf = [];
  state.histRenderedFrom = 0;
  state.replaying = true;
  state.live = null;
  taskFoldReset();
  state.stats = { turns: 0, steps: 0, firstTs: 0, lastTs: 0, llmMs: 0, toolMs: 0, llmEstMs: 0, toolEstMs: 0, prevTs: 0 };
  updateStatsUI();
  trajReset();
  switchChatTab('chat');
  $('#empty-state').classList.add('hidden');
  $('#new-session-view').classList.add('hidden');
  $('#chat-view').classList.remove('hidden');
  // 清 DOM 前先摘 observer，防悬空引用 + 旧会话文本闪现
  hidePinnedPrompt();
  $('#messages').innerHTML = '';
  // 恢复本会话的待发送 chip（若有）——chip 挂在输入框卡片上，不随消息流重建。
  renderPendingBubble();
  syncTaskPanel(); // 清空上一会话的任务面板（回放 flush 后用新会话的事件重建）
  $('#error-banner').classList.add('hidden');
  $('#session-name').value = s.name;
  $('#session-cwd').textContent = s.cwd;
  $('#session-cwd').title = s.cwd;
  $('#session-perm').value = s.permission_mode || '';
  const engine = s.engine || 'claude';
  fillCLISelect($('#session-cli'), s.cli, engine);
  fillProviderSelect($('#session-provider'), s.provider, engine);
  fillModelSelect($('#session-model'), s.provider, s.model_override);
  $('#session-effort').value = s.effort_override || '';
  setEngineUI(engine);
  // Restore draft (text + attachments) for this session.
  state.attached = (state.drafts[id] && state.drafts[id].attachments) || [];
  $('#input').value = (state.drafts[id] && state.drafts[id].text) || '';
  input.style.height = 'auto';
  input.style.height = Math.min(input.scrollHeight, 180) + 'px';
  renderInputMirror(); // 高度定案后再镜像，镜像高度与 scrollTop 同步才有意义
  updateStatusUI(s.status);
  updateResidentUI();
  applyWorkspaceGate(s.cwd);
  applyAttachGate();
  renderAttachedThumbs();
  clearUnreadDone(s.id); // 点开即视为已查看：蓝点恢复灰色
  renderSessionList();
  renderWsActivity();
  connectEvents(id);
  // 切会话后光标直接进输入框，省一次点击；无 cwd 时输入框被禁用则不 focus，
  // 移动端也不 focus——自动弹软键盘会挡住刚加载的历史。
  if (!isMobile() && !input.disabled) {
    input.focus();
    input.setSelectionRange(input.value.length, input.value.length);
  }
}

// ---------- 对话 / 轨迹 tab ----------
// 轨迹视图（trajectory.js）复用同一份事件流投影成台账；composer 仅在对话页显示，
// 切到轨迹页时隐藏。
function switchChatTab(tab) {
  document.querySelectorAll('.chat-tab').forEach((b) => {
    b.classList.toggle('active', b.dataset.tab === tab);
  });
  const isTraj = tab === 'traj';
  $('#msg-wrap').classList.toggle('hidden', isTraj);
  $('#traj-view').classList.toggle('hidden', !isTraj);
  $('#composer').classList.toggle('hidden', isTraj);
  if (isTraj) trajShow();
}
document.querySelectorAll('.chat-tab').forEach((b) => {
  b.onclick = () => switchChatTab(b.dataset.tab);
});

// 在没有 cwd 时禁用 composer
function applyWorkspaceGate(cwd) {
  const gate = $('#workspace-gate');
  const disabled = !cwd;
  gate.classList.toggle('hidden', !disabled);
  $('#input').disabled = disabled;
  $('#btn-send').disabled = disabled;
  $('#btn-attach').disabled = disabled;
  if (disabled) {
    $('#input').placeholder = '请先在新建会话时填写工作目录';
  } else {
    updateMobileInputHint();
  }
}

// 常驻/peer 消息、推理等级、fork 都是 Claude Code 协议面；opencode 会话不提供
// 这些入口（服务端同样 400 拒绝），把对应控件藏起来。
function setEngineUI(engine) {
  const oc = engine === 'opencode';
  $('#btn-resident').classList.toggle('hidden', oc);
  $('#session-effort').classList.toggle('hidden', oc);
  $('#sheet-effort').closest('label').classList.toggle('hidden', oc);
}

// 会话菜单随目标会话（而非当前打开的会话）引擎过滤；菜单每次打开前重设。
function syncMenuForEngine(s) {
  const forkItem = document.querySelector('#session-menu [data-action="fork"]');
  if (forkItem) forkItem.classList.toggle('hidden', (s.engine || 'claude') === 'opencode');
}

// 附件按钮：任意模型都能拿文件让 agent 用工具处理（图片仍要求 input 含 image），
// 只在无 cwd 时禁用。
function applyAttachGate() {
  if (!state.current) return;
  const prov = state.providers.find((p) => p.name === state.current.provider);
  const m = prov && prov.models.find((x) => x.name === state.current.model_override);
  const allow = !!(prov && m);
  const btn = $('#btn-attach');
  btn.disabled = !allow || $('#input').disabled;
  btn.title = allow
    ? '添加图片或文件（可粘贴/拖拽）'
    : '当前模型未声明能力；如需附件请检查模型配置';
}

function fillSelect(sel, items, value, formatter) {
  sel.innerHTML = '';
  for (const item of items) {
    const opt = document.createElement('option');
    opt.value = item.name;
    opt.textContent = formatter ? formatter(item) : item.name;
    sel.appendChild(opt);
  }
  if (value) sel.value = value;
}

// clis/providers 各带 kind（"claude"|"opencode"，config 归一化后必有值）。
// 会话绑定单一 engine（不做跨引擎切换），所以按 kind 过滤下拉选项。
const kindOf = (item) => (item && item.kind) || 'claude';
const cliKind = (name) => kindOf(state.clis.find((c) => c.name === name));
const provKind = (name) => kindOf(state.providers.find((p) => p.name === name));

function fillCLISelect(sel, value, kind) {
  fillSelect(sel, kind ? state.clis.filter((c) => kindOf(c) === kind) : state.clis, value,
    (c) => (kindOf(c) === 'claude' ? c.name : `${c.name} · ${c.kind}`));
}

function fillProviderSelect(sel, value, kind) {
  fillSelect(sel, kind ? state.providers.filter((p) => kindOf(p) === kind) : state.providers, value);
}

// Model 下拉随 provider 联动；空值 = provider 默认模型（models 第一个）
function fillModelSelect(sel, providerName, value) {
  const p = state.providers.find((x) => x.name === providerName);
  const models = (p && p.models) || [];
  sel.innerHTML = '';
  const def = document.createElement('option');
  def.value = '';
  def.textContent = models.length ? `默认 (${models[0].name})` : '(无)';
  sel.appendChild(def);
  for (const m of models) {
    const opt = document.createElement('option');
    opt.value = m.name;
    opt.textContent = m.name;
    sel.appendChild(opt);
  }
  sel.value = value || '';
}

// config 热加载：后台 watch 到 config.yaml 变动后广播 config_changed（或前端
// 轮询兜底），这里重拉 clis/providers，有变动才重建当前会话的下拉，保留已选值。
async function refreshProfiles() {
  try {
    const [clis, providers] = await Promise.all([api('/api/clis'), api('/api/providers')]);
    const sig = JSON.stringify([clis, providers]);
    if (sig === state.profilesSig) return;
    state.profilesSig = sig;
    state.clis = clis;
    state.providers = providers;
    rerenderProfileSelects();
  } catch (_) {
    // 网络抖动忽略：下一轮轮询/广播再试
  }
}

// 用最新 state.providers/state.clis 重建「当前会话」可见的 cli/provider/model
// 下拉（刷新选项不触发 change 事件，已选值原样保留；模型被删则退回默认项）。
function rerenderProfileSelects() {
  if (!state.current) return;
  const engine = state.current.engine || 'claude';
  fillCLISelect($('#session-cli'), state.current.cli, engine);
  fillProviderSelect($('#session-provider'), state.current.provider, engine);
  fillModelSelect($('#session-model'), state.current.provider, state.current.model_override);
  applyAttachGate();
  patchSheetIfOpen(state.current);
}

function connectEvents(id) {
  if (state.es) state.es.close();
  const es = new EventSource(`/api/sessions/${id}/events`);
  state.es = es;
  // 服务端每条连接的 seq 都从 1 重新编号，而 seenSeq 跨重连保留：EventSource
  // 自动重连后（网络抖动/服务重启/切后台），新 live 事件的 seq 与旧的撞车会被
  // 当重复事件静默丢弃，流式就此"死掉"。重连时重置视图状态，让新的全量回放重建。
  let opened = false;
  es.onopen = () => {
    if (opened) resetChatForReplay();
    opened = true;
  };
  es.onmessage = (e) => {
    const ev = JSON.parse(e.data);
    if (state.seenSeq.has(ev.seq)) return;
    state.seenSeq.add(ev.seq);
    if (ev.kind === 'replay_end') {
      flushHistory();
      trajOnEvent(); // 回放补齐轨迹投影（若轨迹页开着）
      return;
    }
    // Live checkpoint notification: attach a rewind button to the last user
    // message (the turn just ended). Transient — not in replay history; the
    // button is persisted in the user event's data.checkpoint for reconnects.
    if (ev.kind === 'checkpoint') {
      attachCheckpointToLastUser(ev.data && ev.data.checkpoint);
      return;
    }
    // Live rewind broadcast: truncate the UI + show a marker. Replay-time
    // rewind events (already truncated server-side) fall through to render as
    // a history marker via renderEvent.
    if (ev.kind === 'rewind' && !state.replaying) {
      applyRewindInUI(ev, false);
      return;
    }
    // config 热加载广播：刷新 clis/providers 与模型下拉，不渲染为消息
    if (ev.kind === 'config_changed') {
      refreshProfiles();
      return;
    }
    // 子代理（Agent 工具）消息：实时流由父进程 stdout 以 parent_tool_use_id
    // 透传；回放侧由服务端 ConvertSubagents 从 subagents/*.jsonl 产出同形
    // 事件。两侧都经 renderEvent 渲染成折叠卡片，但不计入统计/轨迹。
    const isSubagent = ev.kind === 'stream' && ev.data && ev.data.parent_tool_use_id;
    if (!isSubagent) trackStats(ev);
    // Buffer replayed history; render only the latest page when the
    // replay_end marker arrives. Live events render immediately.
    if (state.replaying) {
      state.historyBuf.push(ev);
      return;
    }
    renderEvent(ev);
    // live 事件同样进入 historyBuf，作为轨迹视图的数据源；
    // stream_event 增量太多且轨迹用不到（完整 assistant 事件兜底），不入。
    if (ev.kind !== 'stream' || !ev.data || ev.data.type !== 'stream_event') {
      state.historyBuf.push(ev);
      trajOnEvent();
      if (taskFoldEvent(ev)) syncTaskPanel(); // Task*/TodoWrite 变化 → 更新任务面板
    }
    scrollBottom();
    updateStatsUI();
  };
}

// EventSource 重连后重置会话视图：清空已渲染消息与 seq 去重集合，让服务端
// 新一轮全量回放重建视图（回放覆盖所有已持久化事件，不会丢内容；进行中的
// 流式消息由后续完整 assistant 事件兜底）。乐观气泡保留在末尾。
function resetChatForReplay() {
  // 清 DOM 前先摘悬浮条，防旧会话文本闪现（flushHistory 会重挂）
  if (typeof hidePinnedPrompt === 'function') hidePinnedPrompt();
  const box = $('#messages');
  const optimistic = [...box.querySelectorAll('.msg-optimistic')];
  box.innerHTML = '';
  for (const n of optimistic) box.appendChild(n);
  state.seenSeq = new Set();
  state.toolBlocks = {};
  state.subagentCards = {};
  state.agentToolMeta = {};
  state.historyBuf = [];
  state.histRenderedFrom = 0;
  state.replaying = true;
  state.live = null;
  taskFoldReset();
  state.stats = { turns: 0, steps: 0, firstTs: 0, lastTs: 0, llmMs: 0, toolMs: 0, llmEstMs: 0, toolEstMs: 0, prevTs: 0 };
  trajReset();
  syncTaskPanel();
  updateLoadMoreBtn();
}

// ---------- session stats (轮数 / 步数 / 累计耗时) ----------
// 轮数 = user 事件数；步数 = assistant 消息里的 tool_use 块数；
// 耗时 = 首尾事件时间差（随实时事件增长）。
function trackStats(ev) {
  const st = state.stats;
  if (!st) return;
  const ts = Date.parse(ev.ts) || 0;
  if (ts) {
    if (!st.firstTs || ts < st.firstTs) st.firstTs = ts;
    if (ts > st.lastTs) st.lastTs = ts;
  }
  if (ev.kind === 'user') {
    st.turns++;
  } else if (ev.kind === 'stream' && ev.data && ev.data.type === 'assistant') {
    const content = ev.data.message && ev.data.message.content;
    if (Array.isArray(content)) {
      for (const b of content) if (b && b.type === 'tool_use') st.steps++;
    }
  } else if (ev.kind === 'stream' && ev.data && ev.data.type === 'result') {
    // LLM 耗时 = result.duration_api_ms（API 时间）；
    // 工具耗时 ≈ duration_ms - duration_api_ms（含少量进程开销的近似值）
    const total = ev.data.duration_ms || 0;
    const api = ev.data.duration_api_ms || 0;
    st.llmMs += api;
    st.toolMs += Math.max(0, total - api);
  }
  // 无 result 事件的会话（如 claude jsonl 重放的历史）退化为时间差估算：
  // assistant 前的间隙 ≈ LLM 耗时，tool_result 前的间隙 ≈ 工具耗时。
  // 单段封顶 2 分钟，避免权限确认等待等长停顿把估算打爆。
  if (ev.kind === 'stream' && ev.data && ts && st.prevTs) {
    const gap = Math.min(Math.max(0, ts - st.prevTs), 120000);
    if (ev.data.type === 'assistant') st.llmEstMs += gap;
    else if (ev.data.type === 'user') st.toolEstMs += gap;
  }
  if (ts) st.prevTs = ts;
}

function fmtDuration(ms) {
  if (ms < 1000) return '<1 秒';
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s} 秒`;
  const m = Math.floor(s / 60);
  if (m < 60) return s % 60 ? `${m} 分 ${s % 60} 秒` : `${m} 分钟`;
  const h = Math.floor(m / 60);
  if (h < 24) return m % 60 ? `${h} 小时 ${m % 60} 分` : `${h} 小时`;
  const d = Math.floor(h / 24);
  return h % 24 ? `${d} 天 ${h % 24} 小时` : `${d} 天`;
}

function updateStatsUI() {
  const el = $('#session-stats');
  if (!el) return;
  const st = state.stats;
  if (!st || !st.turns) {
    el.textContent = '';
    return;
  }
  let out = `${st.turns} 轮对话 · ${st.steps} 步执行`;
  // 有 result 事件用精确值，否则用相邻事件时间差的估算值
  const exact = st.llmMs || st.toolMs;
  const llm = exact ? st.llmMs : st.llmEstMs;
  const tool = exact ? st.toolMs : st.toolEstMs;
  if (llm || tool) {
    out += `（LLM ${fmtDuration(llm)} · 工具 ${fmtDuration(tool)}${exact ? '' : '，估算'}）`;
  }
  const dur = st.lastTs > st.firstTs ? fmtDuration(st.lastTs - st.firstTs) : '—';
  el.textContent = `${out} · 累计 ${dur}`;
}

// Render the latest page of buffered history (called on replay_end).
function flushHistory() {
  // EventSource auto-reconnect replays history again with fresh seqs; the
  // replayed events are dropped by seenSeq dedup, and the duplicate marker
  // must not re-render the buffer.
  if (!state.replaying) return;
  state.replaying = false;
  const box = $('#messages');
  // Optimistic bubbles sent while history was loading belong at the end.
  const optimistic = [...box.querySelectorAll('.msg-optimistic')];
  const buf = state.historyBuf;
  state.histRenderedFrom = Math.max(0, buf.length - HISTORY_PAGE);
  // 历史渲染期间旧 status 事件不得触发副作用（旧 turn_end 会把待发送消息
  // 在轮次仍在跑时误提交；见 renderEvent 的 status 分支）。
  state.renderingHistory = true;
  try {
    for (let i = state.histRenderedFrom; i < buf.length; i++) {
      renderEvent(buf[i], box);
    }
  } finally {
    state.renderingHistory = false;
  }
  // 只回填仍在文档里的乐观气泡：新任务流程（nsSend）里回放已含本条 user
  // 事件，renderEvent 会摘掉已对账的乐观气泡，这里不能把摘掉的又接回来
  //（否则 prompt 显示两次，切走再切回才恢复一条）。
  for (const node of optimistic) if (node.isConnected) box.appendChild(node);
  // 回放事件只渲染了最新一页，但任务面板要覆盖全部历史——从缓冲区全量重折。
  taskFoldAll();
  syncTaskPanel();
  // 切走期间本轮已结束的会话：回放里的旧 turn_end 被上面挡住，在这里按
  // 最新状态补交该会话的待发送消息。
  if (state.current && state.pendingBySid[state.current.id] && !isRunning()) flushPending();
  state.nearBottom = true;
  scrollBottom();
  updateLoadMoreBtn();
  updateStatsUI();
  refreshPinnedPrompt();
}

// Prepend the previous page of buffered history, keeping the viewport
// anchored on the message the user was looking at.
function loadMoreHistory() {
  if (state.histRenderedFrom <= 0) return;
  const box = $('#messages');
  const from = Math.max(0, state.histRenderedFrom - HISTORY_PAGE);
  const frag = document.createDocumentFragment();
  state.renderingHistory = true;
  try {
    for (let i = from; i < state.histRenderedFrom; i++) {
      renderEvent(state.historyBuf[i], frag);
    }
  } finally {
    state.renderingHistory = false;
  }
  state.histRenderedFrom = from;
  const prevHeight = box.scrollHeight;
  const wrap = $('#load-more-wrap');
  box.insertBefore(frag, wrap ? wrap.nextSibling : box.firstChild);
  box.scrollTop += box.scrollHeight - prevHeight;
  updateLoadMoreBtn();
}

// The pill button pinned at the top of the message list while there are
// older buffered events not rendered yet.
function updateLoadMoreBtn() {
  let wrap = $('#load-more-wrap');
  if (state.histRenderedFrom <= 0) {
    if (wrap) wrap.classList.add('hidden');
    return;
  }
  if (!wrap) {
    wrap = document.createElement('div');
    wrap.id = 'load-more-wrap';
    const btn = document.createElement('button');
    btn.className = 'load-more-btn';
    btn.onclick = loadMoreHistory;
    wrap.appendChild(btn);
    const box = $('#messages');
    box.insertBefore(wrap, box.firstChild);
  }
  wrap.firstChild.textContent = `加载更多（还有 ${state.histRenderedFrom} 条）`;
  wrap.classList.remove('hidden');
}

function updateStatusUI(st) {
  const badge = $('#status-badge');
  const stopBtn = $('#btn-stop');
  const hint = $('#queue-hint');
  const spinner = $('#composer-spinner');
  // Keep state.current.status in sync so isRunning() reflects the latest SSE
  // status event immediately (refreshCurrentStatus overwrites it async later).
  if (state.current) {
    state.current.status = { running: !!(st && st.running), queued: (st && st.queued) || 0 };
  }
  // Grey out rewind buttons while a turn is in flight (the API also 409s).
  document.body.classList.toggle('session-running', !!(st && st.running));
  if (st.running) {
    badge.textContent = '● 运行中';
    badge.className = 'status-badge running';
    stopBtn.classList.remove('hidden');
    if (spinner) spinner.classList.remove('hidden');
  } else {
    badge.textContent = '';
    badge.className = 'status-badge';
    stopBtn.classList.add('hidden');
    if (spinner) spinner.classList.add('hidden');
  }
  hint.textContent = st.queued > 0 ? `队列中还有 ${st.queued} 条消息` : '';
}

// ---------- pinned prompt（钉住最后一条输入） ----------
// 悬浮条状态全派生、零持久化：内容 = 最后一条 .msg-user 的 textContent；显隐
// = 本端 IntersectionObserver；展开/折叠 = 本端内存瞬态（切会话/新消息即重置）。
// 单一幂等入口 refreshPinnedPrompt() 处理任何时序（回放/重连/多端乱序）。
const pinnedPrompt = {
  observer: null,
  target: null,
  expanded: false,
  fadeTimer: null,
};

// 幂等重算：摘旧 observer → 取最后一条 .msg-user → 无则隐藏，有则更新文本并
// 复位折叠/滚动/渐隐后重挂。任何调用点重复调用都安全。
function refreshPinnedPrompt() {
  if (pinnedPrompt.observer) {
    pinnedPrompt.observer.disconnect();
    pinnedPrompt.observer = null;
  }
  const pp = $('#pinned-prompt');
  const msgs = $('#messages');
  const text = $('#pp-text');
  if (!pp || !text) return;
  const users = msgs.querySelectorAll('.msg-user');
  if (!users.length) { hidePinnedPrompt(); return; }
  const last = users[users.length - 1];
  // 只取 .bubble 文本：rewind 按钮（「回滚」）与 role-tag（「你」+ 时间）挂在
  // .msg-user 直接子级，用整条 textContent 会把 UI 文字带进悬浮条；附件 chip
  // 文件名在 bubble 内，仍包含。兜底（无 .bubble 的结构）克隆后剔除 UI 节点。
  let src = last.querySelector('.bubble');
  if (!src) {
    const clone = last.cloneNode(true);
    clone.querySelectorAll('.role-tag, .msg-time, .rewind-btn').forEach((n) => n.remove());
    src = clone;
  }
  text.textContent = src.textContent || '';
  // 换钉一律复位折叠态 + 正文滚动 + 渐隐
  pinnedPrompt.expanded = false;
  pp.classList.remove('expanded');
  text.scrollTop = 0;
  const fade = pp.querySelector('.pp-fade');
  if (fade) fade.classList.add('hidden');
  renderIcons(pp);
  updatePpExpandBtn();
  const root = msgs;
  const io = new IntersectionObserver((entries) => {
    for (const ent of entries) {
      if (ent.isIntersecting) { hidePinnedPrompt(false); return; }
      // gate：切轨迹 tab 或 #chat-view 被隐藏时一律不显示
      const mw = $('#msg-wrap');
      const cv = $('#chat-view');
      if (mw && mw.offsetParent === null) { hidePinnedPrompt(false); return; }
      if (cv && cv.offsetParent === null) { hidePinnedPrompt(false); return; }
      if (ent.boundingClientRect.top < root.getBoundingClientRect().top) {
        showPinnedPrompt();
      } else {
        hidePinnedPrompt(false);
      }
    }
  }, { root, threshold: 0 });
  io.observe(last);
  pinnedPrompt.observer = io;
  pinnedPrompt.target = last;
}

function hidePinnedPrompt(disconnect) {
  const pp = $('#pinned-prompt');
  if (!pp) return;
  if (disconnect !== false && pinnedPrompt.observer) {
    pinnedPrompt.observer.disconnect();
    pinnedPrompt.observer = null;
  }
  pinnedPrompt.target = null;
  if (pinnedPrompt.expanded) {
    pinnedPrompt.expanded = false;
    pp.classList.remove('expanded');
    const text = $('#pp-text');
    if (text) text.scrollTop = 0;
    const fade = pp.querySelector('.pp-fade');
    if (fade) fade.classList.add('hidden');
    renderIcons(pp);
    updatePpExpandBtn();
  }
  pp.classList.remove('shown');
  clearTimeout(pinnedPrompt.fadeTimer);
  pinnedPrompt.fadeTimer = setTimeout(() => pp.classList.add('hidden'), 180);
}

function showPinnedPrompt() {
  const pp = $('#pinned-prompt');
  if (!pp) return;
  // 取消 hide 的延时 hidden：滚回（hide 180ms 后加 hidden）又立即滚出（show）
  // 时，若不清定时器，hidden 会在 show 之后补上，悬浮条再也看不见。
  clearTimeout(pinnedPrompt.fadeTimer);
  pp.classList.remove('hidden');
  void pp.offsetHeight;
  pp.classList.add('shown');
  // 刷新时若悬浮条处于 display:none 无法测量溢出，展开按钮判定会漏；现身时校正。
  updatePpExpandBtn();
}

// 折叠态下正文是否超过 2 行截断（决定展开按钮显隐）：
// line-clamp 只裁剪显示，scrollHeight 仍是全文高 → scrollHeight > clientHeight 即溢出。
// 只在可布局时有效（.hidden 的 display:none 下两者都算 0），showPinnedPrompt 会再校正。
function updatePpExpandBtn() {
  const btn = $('#pp-expand');
  const text = $('#pp-text');
  if (!btn || !text) return;
  const overflow = text.scrollHeight - text.clientHeight > 1;
  btn.classList.toggle('hidden', !overflow);
}

$('#pp-expand').addEventListener('click', (e) => {
  e.stopPropagation();
  const pp = $('#pinned-prompt');
  const text = $('#pp-text');
  const fade = pp.querySelector('.pp-fade');
  const expand = pp.querySelector('.pp-expand i');
  pinnedPrompt.expanded = !pinnedPrompt.expanded;
  pp.classList.toggle('expanded', pinnedPrompt.expanded);
  if (expand) {
    expand.dataset.lucide = pinnedPrompt.expanded ? 'chevron-up' : 'chevron-down';
    renderIcons(pp);
  }
  if (pinnedPrompt.expanded) {
    // 展开后立即按当前滚动位置校正遮罩（短文本不可滚时遮罩应隐藏，等 scroll 事件会漏）
    updatePpFade();
  } else {
    if (fade) fade.classList.add('hidden');
    if (text) text.scrollTop = 0;
    // 折叠后重新按溢出状态校正按钮显隐（收起只可能在内容溢出时发生，一般无变化）
    updatePpExpandBtn();
  }
});

// 渐隐遮罩显隐的唯一判定：正文可滚且未滚到底时显示。
// 展开、滚动、换钉都会经此校正，避免"内容不可滚但遮罩残留"。
function updatePpFade() {
  const text = $('#pp-text');
  const pp = $('#pinned-prompt');
  if (!text || !pp) return;
  const fade = pp.querySelector('.pp-fade');
  if (!fade) return;
  const canScroll = text.scrollHeight > text.clientHeight;
  const atBottom = text.scrollTop + text.clientHeight >= text.scrollHeight - 4;
  fade.classList.toggle('hidden', !canScroll || atBottom);
}

$('#pp-text').addEventListener('scroll', () => {
  updatePpFade();
});

$('#pp-text').addEventListener('click', (e) => {
  e.stopPropagation();
  if (pinnedPrompt.expanded) return;
  jumpToPinned();
});

function jumpToPinned() {
  if (pinnedPrompt.target && pinnedPrompt.target.isConnected) {
    pinnedPrompt.target.scrollIntoView({ behavior: 'smooth', block: 'start' });
  }
}

// ---------- event rendering ----------
function scrollBottom() {
  const box = $('#messages');
  if (state.nearBottom) box.scrollTop = box.scrollHeight;
}
$('#messages').addEventListener('scroll', () => {
  const box = $('#messages');
  state.nearBottom = box.scrollHeight - box.scrollTop - box.clientHeight < 120;
});

function addMsg(cls, roleTag, box, ts) {
  box = box || $('#messages');
  const div = document.createElement('div');
  div.className = `msg ${cls}`;
  const timeStr = ts ? formatMsgTime(ts) : '';
  const timeHTML = timeStr ? `<span class="msg-time">${timeStr}</span>` : '';
  div.innerHTML = `<div class="role-tag">${roleTag}${timeHTML}</div><div class="bubble"></div>`;
  box.appendChild(div);
  return div.querySelector('.bubble');
}

// ---------- rewind (file checkpoint + conversation truncation) ----------
// Each user message whose event carries data.checkpoint gets a ⏪ button.
// Clicking confirms, then POST /rewind: the CLI restores files, the server
// truncates both claude's jsonl and our event log, and broadcasts a `rewind`
// event. Live clients truncate their UI; the initiator also refills the
// composer with the rolled-back message text.

function addRewindBtn(msgDiv) {
  if (msgDiv.querySelector('.rewind-btn')) return;
  const btn = document.createElement('button');
  btn.className = 'rewind-btn';
  btn.type = 'button';
  btn.title = '回滚到这条消息之前';
  btn.innerHTML = '<i data-lucide="rotate-ccw"></i><span>回滚</span>';
  btn.onclick = (e) => {
    e.stopPropagation();
    if (document.body.classList.contains('session-running')) return;
    openRewindModal(msgDiv);
  };
  msgDiv.appendChild(btn);
  renderIcons(btn);
}

// Live checkpoint event → attach a button to the most recently rendered user
// message (the turn that just ended). Safe because the next user message can't
// be enqueued until this turn's cleanup (incl. this broadcast) finishes.
function attachCheckpointToLastUser(checkpoint) {
  if (!checkpoint) return;
  const msgs = $('#messages').querySelectorAll('.msg-user');
  if (!msgs.length) return;
  const last = msgs[msgs.length - 1];
  if (last.dataset.checkpoint) return; // already rewound-able
  last.dataset.checkpoint = checkpoint;
  addRewindBtn(last);
}

function appendRewindMarker(box, checkpoint, detail) {
  const marker = document.createElement('div');
  marker.className = 'rewind-marker';
  if (checkpoint) marker.dataset.checkpoint = checkpoint;
  marker.innerHTML = '<span class="rm-icon">⏪</span><span class="rm-text"></span>';
  marker.querySelector('.rm-text').textContent = '已回滚到此处' + (detail ? ' · ' + detail : '');
  box.appendChild(marker);
}

// Apply a rewind to the live UI: drop the target user message and everything
// after it (DOM + historyBuf), re-project the trajectory, render a marker, and
// optionally refill the composer (only the initiating client passes refill).
// Idempotent on the checkpoint uuid.
function applyRewindInUI(ev, refill) {
  const d = (ev && ev.data) || {};
  const checkpoint = d.checkpoint;
  if (!checkpoint) return;
  const box = $('#messages');
  if (box.querySelector(`.rewind-marker[data-checkpoint="${checkpoint}"]`)) {
    if (refill && d.draft_text) setDraftText(d.draft_text);
    return;
  }
  // DOM: remove the target user message and all following siblings.
  const target = box.querySelector(`.msg-user[data-checkpoint="${checkpoint}"]`);
  if (target) {
    let node = target;
    while (node) {
      const next = node.nextElementSibling;
      node.remove();
      node = next;
    }
  }
  // Model: truncate historyBuf at the target user event, then make the rewind
  // event the new tail so reconnect replay would match.
  const buf = state.historyBuf;
  let cutAt = buf.length;
  for (let i = 0; i < buf.length; i++) {
    const e = buf[i];
    if (e.kind === 'user' && e.data && e.data.checkpoint === checkpoint) { cutAt = i; break; }
  }
  buf.splice(cutAt);
  buf.push(ev);
  trajReset();
  trajSync();
  if (trajActive()) trajRender();
  // 任务面板状态折叠自 historyBuf，截断后全量重折（被回滚的 Task* 调用随之消失）
  taskFoldAll();
  syncTaskPanel();
  appendRewindMarker(box, checkpoint, d.detail);
  box.querySelectorAll('.msg-optimistic').forEach((n) => n.remove());
  state.nearBottom = true;
  scrollBottom();
  updateLoadMoreBtn();
  // Recompute stats from the truncated buffer.
  state.stats = { turns: 0, steps: 0, firstTs: 0, lastTs: 0, llmMs: 0, toolMs: 0, llmEstMs: 0, toolEstMs: 0, prevTs: 0 };
  for (const e of buf) trackStats(e);
  updateStatsUI();
  if (refill && d.draft_text) setDraftText(d.draft_text);
  refreshPinnedPrompt();
}

function setDraftText(text) {
  if (!state.current) return;
  input.value = text;
  draft().text = text;
  input.style.height = 'auto';
  input.style.height = Math.min(input.scrollHeight, 180) + 'px';
  renderInputMirror(); // 高度定案后再镜像（见 renderInputMirror）
  input.focus();
}

let rewindTarget = null; // {checkpoint, text} for the in-flight confirm modal
function openRewindModal(msgDiv) {
  const bubble = msgDiv.querySelector('.bubble');
  rewindTarget = {
    checkpoint: msgDiv.dataset.checkpoint,
    text: (bubble && bubble.textContent) || '',
  };
  const firstLine = (rewindTarget.text.split('\n').map((s) => s.trim()).find(Boolean) || '').slice(0, 120);
  $('#rewind-preview').textContent = firstLine || '(空)';
  openModal('#modal-rewind');
}

$('#rewind-submit').onclick = async () => {
  if (!rewindTarget || !state.current) return;
  const btn = $('#rewind-submit');
  const orig = btn.textContent;
  btn.disabled = true;
  btn.textContent = '回滚中…';
  try {
    const res = await api(`/api/sessions/${state.current.id}/rewind`, {
      method: 'POST',
      body: JSON.stringify({ checkpoint: rewindTarget.checkpoint }),
    });
    $('#modal-rewind').classList.add('hidden');
    // Apply locally immediately (the SSE rewind broadcast keeps other clients
    // in sync and is a no-op here thanks to idempotency).
    applyRewindInUI({ kind: 'rewind', data: {
      checkpoint: rewindTarget.checkpoint,
      detail: res.files,
      draft_text: res.draft_text,
    } }, true);
  } catch (e) {
    let msg = e.message || '回滚失败';
    if (/No file checkpoint found|not enabled|File rewinding/i.test(msg)) {
      msg = '该会话未开启文件 checkpoint（或 CLI 版本过低），无法回滚';
    }
    alert(msg);
  } finally {
    btn.disabled = false;
    btn.textContent = orig;
    rewindTarget = null;
  }
};

function toolSummary(input) {
  if (!input || typeof input !== 'object') return '';
  if (Array.isArray(input.todos)) {
    const done = input.todos.filter((t) => t && t.status === 'completed').length;
    return `${done}/${input.todos.length} 完成`;
  }
  if (input.command) return String(input.command).slice(0, 120);
  if (input.file_path) return input.file_path;
  if (input.pattern) return input.pattern;
  if (input.url) return input.url;
  if (input.prompt) return String(input.prompt).slice(0, 120);
  // TaskCreate/TaskUpdate：任务详情看输入框上方的任务面板，摘要一行带过
  if (input.taskId != null && input.taskId !== '') return `#${input.taskId}` + (input.status ? ` → ${input.status}` : '');
  if (typeof input.subject === 'string' && input.subject) return input.subject;
  const keys = Object.keys(input);
  return keys.length ? JSON.stringify(input).slice(0, 120) : '';
}

function renderToolUse(block, box, ts, raw) {
  // 记录 Agent 工具调用的元数据：实时子代理事件只带 parent_tool_use_id，
  // 卡片标题从这里回查（回放侧由服务端在事件里直接给 d.subagent）。
  const bn = (block.name || '').toLowerCase();
  if ((bn === 'agent' || bn === 'task') && block.id && block.input) {
    state.agentToolMeta[block.id] = { title: block.input.description || '', type: block.input.subagent_type || '' };
  }
  const details = document.createElement('details');
  details.className = 'tool-block';
  const summary = document.createElement('summary');
  summary.innerHTML = `<span class="tool-name"></span><span class="tool-summary-text"></span>`;
  summary.querySelector('.tool-name').textContent = block.name || 'tool';
  summary.querySelector('.tool-summary-text').textContent = toolSummary(block.input);
  const body = document.createElement('div');
  body.className = 'tool-body';
  if (!buildToolView(block, details, body)) {
    body.textContent = JSON.stringify(block.input, null, 2);
  }
  details.append(summary, body);
  // raw：直接挂进调用方给的容器（子代理卡片体），不包消息气泡壳
  if (raw) box.appendChild(details);
  else wrapInMsg(details, box, ts);
  state.toolBlocks[block.id] = details;
}

// 工具 body 定制视图：bash 终端风 / read 文件头 / write 全绿 diff / edit 红绿 diff。
// bash、read 的关键信息已在 summary 行，保持折叠；write、edit 默认展开。
// 返回 true 表示 body 已被定制填充。
function buildToolView(block, details, body) {
  const input = block.input;
  const name = (block.name || '').toLowerCase();
  if (name === 'todowrite' && input && Array.isArray(input.todos)) {
    details.classList.add('tool-todo');
    body.classList.add('todo-body');
    body.appendChild(buildTodoView(input.todos));
    details.open = true;
    return true;
  }
  if (/^task(create|update|list|get)$/.test(name)) {
    details.classList.add('tool-task');
    body.classList.add('task-body');
    body.appendChild(buildTaskToolView(input || {}));
    return true;
  }
  if (name === 'bash' && input && typeof input.command === 'string') {
    details.classList.add('tool-bash');
    body.appendChild(buildBashView(input));
    return true;
  }
  if (name === 'read' && input && typeof input.file_path === 'string') {
    body.appendChild(buildReadView(input));
    return true;
  }
  if (name === 'write' && input && typeof input.content === 'string') {
    body.classList.add('edit-diff');
    body.appendChild(buildWriteView(input));
    details.open = true;
    return true;
  }
  const diff = buildEditDiff(input);
  if (diff) {
    // edit 类工具：old_string/new_string 渲染成红绿对比 diff，默认展开
    body.classList.add('edit-diff');
    body.appendChild(diff);
    details.open = true;
    return true;
  }
  return false;
}

// TodoWrite：checklist 视图 —— 状态着色（completed 绿勾 / in_progress 蓝点 / pending 空心）
function buildTodoView(todos) {
  const list = document.createElement('ul');
  list.className = 'todo-list';
  for (const t of todos) {
    if (!t || typeof t.content !== 'string') continue;
    const item = document.createElement('li');
    item.className = 'todo-item todo-' + (t.status || 'pending');
    const check = document.createElement('span');
    check.className = 'todo-check';
    const text = document.createElement('span');
    text.className = 'todo-text';
    text.textContent = t.content;
    item.append(check, text);
    list.appendChild(item);
  }
  return list;
}

// TaskCreate/TaskUpdate/TaskGet：字段键值对视图。任务的整体状态看输入框上方
// 的任务面板，这里只展示本次调用带了哪些参数。
function buildTaskToolView(input) {
  const kv = document.createElement('div');
  kv.className = 'task-kv';
  const rows = [
    ['任务', input.taskId != null && input.taskId !== '' ? `#${input.taskId}` : ''],
    ['主题', input.subject],
    ['状态', input.status],
    ['进行中显示', input.activeForm],
    ['描述', input.description],
  ];
  for (const [k, v] of rows) {
    if (!v) continue;
    const row = document.createElement('div');
    row.className = 'task-kv-row';
    const key = document.createElement('span');
    key.className = 'task-kv-key';
    key.textContent = k;
    const val = document.createElement('span');
    val.className = 'task-kv-val';
    val.textContent = String(v);
    row.append(key, val);
    kv.appendChild(row);
  }
  if (!kv.children.length) kv.textContent = '(无参数)';
  return kv;
}

// Bash：终端样式 —— 描述注释 + `$ 命令`，输出由 tool_result 追加（同色终端底）
function buildBashView(input) {
  const term = document.createElement('div');
  term.className = 'term-view';
  if (input.description) {
    const desc = document.createElement('div');
    desc.className = 'term-desc';
    desc.textContent = '# ' + input.description;
    term.appendChild(desc);
  }
  const cmd = document.createElement('div');
  cmd.className = 'term-cmd';
  const prompt = document.createElement('span');
  prompt.className = 'term-prompt';
  prompt.textContent = '$ ';
  cmd.appendChild(prompt);
  cmd.appendChild(document.createTextNode(input.command));
  term.appendChild(cmd);
  return term;
}

// 文件路径头部：路径文本 + 右侧复制按钮
function filePathHead(cls, text, path) {
  const head = document.createElement('div');
  head.className = cls;
  const span = document.createElement('span');
  span.className = 'path-text';
  span.textContent = text;
  span.title = path;
  head.appendChild(span);
  const btn = document.createElement('button');
  btn.className = 'copy-path-btn';
  btn.title = '复制文件路径';
  btn.innerHTML = '<i data-lucide="copy"></i>';
  btn.onclick = async (e) => {
    e.stopPropagation();
    try {
      await copyText(path);
      flashCheck(btn, btn.innerHTML);
    } catch (_) {
      prompt('复制失败，请手动复制：', path);
    }
  };
  head.appendChild(btn);
  renderIcons(head);
  return head;
}

// Read：文件路径 + 行范围；文件内容在 tool_result 里（等宽展示即可）
function buildReadView(input) {
  const view = document.createElement('div');
  view.className = 'read-view';
  view.appendChild(filePathHead('read-path', input.file_path, input.file_path));
  const meta = [];
  if (typeof input.offset === 'number') meta.push(`从第 ${input.offset} 行起`);
  if (typeof input.limit === 'number') meta.push(`共 ${input.limit} 行`);
  if (meta.length) {
    const m = document.createElement('div');
    m.className = 'read-meta';
    m.textContent = meta.join(' · ');
    view.appendChild(m);
  }
  return view;
}

// Write：新文件 = 全部新增，复用 diff 行的全绿样式
function buildWriteView(input) {
  const view = document.createElement('div');
  view.className = 'diff-view';
  const lines = input.content.split('\n');
  // 末尾换行导致的空行不算一行内容
  const count = lines.length && lines[lines.length - 1] === '' ? lines.length - 1 : lines.length;
  view.appendChild(filePathHead('diff-file', `${input.file_path || '(新文件)'}（+${count} 行）`, input.file_path || ''));
  for (const l of lines) view.appendChild(diffLineEl('add', '+', l));
  return view;
}

// ---------- edit diff view ----------
// 提取 edit 入参里的 (old_string, new_string) 对；兼容单 edit 与 edits 数组。
function editPairs(input) {
  if (!input || typeof input !== 'object') return [];
  if (typeof input.old_string === 'string' && typeof input.new_string === 'string') {
    return [[input.old_string, input.new_string]];
  }
  if (Array.isArray(input.edits)) {
    return input.edits
      .filter((e) => e && typeof e.old_string === 'string' && typeof e.new_string === 'string')
      .map((e) => [e.old_string, e.new_string]);
  }
  return [];
}

function buildEditDiff(input) {
  const pairs = editPairs(input);
  if (!pairs.length) return null;
  const view = document.createElement('div');
  view.className = 'diff-view';
  if (input.file_path) {
    view.appendChild(filePathHead('diff-file', input.file_path, input.file_path));
  }
  for (const [oldStr, newStr] of pairs) {
    appendDiffHunk(view, oldStr, newStr);
  }
  return view;
}

// 简单行级对比：剥掉公共前后缀作为上下文，中间差异段红/绿整段展示。
function diffLineEl(cls, sign, text) {
  const line = document.createElement('div');
  line.className = 'diff-line ' + cls;
  const g = document.createElement('span');
  g.className = 'diff-sign';
  g.textContent = sign;
  line.appendChild(g);
  line.appendChild(document.createTextNode(text));
  return line;
}

function appendDiffHunk(view, oldStr, newStr) {
  const a = oldStr.split('\n');
  const b = newStr.split('\n');
  let pre = 0;
  while (pre < a.length && pre < b.length && a[pre] === b[pre]) pre++;
  let suf = 0;
  while (suf < a.length - pre && suf < b.length - pre && a[a.length - 1 - suf] === b[b.length - 1 - suf]) suf++;
  const ctxPre = a.slice(0, pre);
  const del = a.slice(pre, a.length - suf);
  const add = b.slice(pre, b.length - suf);
  const ctxSuf = a.slice(a.length - suf);

  const addLine = (cls, sign, text) => view.appendChild(diffLineEl(cls, sign, text));
  const addCtx = (lines, from, to) => {
    for (let i = from; i < to; i++) addLine('ctx', ' ', lines[i]);
  };
  const fold = (n) => addLine('fold', '', `⋮ ${n} 行未变化`);

  const CTX = 3; // 差异段两端保留的上下文行数
  if (ctxPre.length > CTX) {
    addCtx(ctxPre, 0, 1);
    fold(ctxPre.length - CTX);
    addCtx(ctxPre, ctxPre.length - CTX + 1, ctxPre.length);
  } else {
    addCtx(ctxPre, 0, ctxPre.length);
  }
  for (const l of del) addLine('del', '-', l);
  for (const l of add) addLine('add', '+', l);
  if (ctxSuf.length > CTX) {
    addCtx(ctxSuf, 0, CTX - 1);
    fold(ctxSuf.length - CTX);
    addCtx(ctxSuf, ctxSuf.length - 1, ctxSuf.length);
  } else {
    addCtx(ctxSuf, 0, ctxSuf.length);
  }
}

function toolResultText(content) {
  if (typeof content === 'string') return content;
  if (Array.isArray(content)) {
    return content.map((c) => (c && c.text) || '').filter(Boolean).join('\n');
  }
  return content ? JSON.stringify(content) : '';
}

function renderToolResult(block, box, raw) {
  const text = toolResultText(block.content);
  const target = block.tool_use_id && state.toolBlocks[block.tool_use_id];
  if (target) {
    // TodoWrite 的结果固定是 "Todos have been modified…"，无信息量，不展示
    if (target.classList.contains('tool-todo')) return;
    // Task* 的成功结果同理（"Task #N created successfully"），面板已反映状态；
    // 但错误结果（如输入校验失败）必须展示
    if (target.classList.contains('tool-task') && !block.is_error) return;
    const r = document.createElement('div');
    r.className = 'tool-result';
    const body = document.createElement('div');
    body.className = 'tool-body';
    // bash 的输出沿用终端配色，和命令行连成一整个终端窗口
    if (target.classList.contains('tool-bash')) body.classList.add('term-out');
    body.textContent = text.length > 4000 ? text.slice(0, 4000) + '\n… (截断)' : text;
    if (block.is_error) body.style.color = 'var(--red)';
    r.appendChild(body);
    target.appendChild(r);
    return;
  }
  // Orphan result (e.g. from imported history without the tool_use block).
  const details = document.createElement('details');
  details.className = 'tool-block tool-result';
  const summary = document.createElement('summary');
  summary.innerHTML = '<span class="tool-name">tool_result</span>';
  const body = document.createElement('div');
  body.className = 'tool-body';
  body.textContent = text.length > 4000 ? text.slice(0, 4000) + '\n… (截断)' : text;
  details.append(summary, body);
  if (raw) box.appendChild(details); // 子代理卡片体：不包消息气泡壳
  else wrapInMsg(details, box);
}

// ---------- file viewer（右侧栏预览本地文件） ----------
function escapeHtml(s) {
  return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

async function openFileViewer(path, line) {
  const panel = $('#file-viewer');
  panel.classList.remove('hidden');
  $('#fv-path').textContent = path;
  $('#fv-path').title = path;
  const body = $('#fv-body');
  body.innerHTML = '<div class="fv-status">加载中…</div>';
  renderIcons(panel);
  try {
    const data = await api(`/api/files?path=${encodeURIComponent(path)}`);
    renderFileContent(body, data, line);
  } catch (e) {
    body.innerHTML = '';
    const err = document.createElement('div');
    err.className = 'fv-status fv-error';
    err.textContent = '无法读取：' + e.message;
    body.appendChild(err);
  }
}

function closeFileViewer() {
  $('#file-viewer').classList.add('hidden');
}

const FV_MAX_LINES = 5000; // 大文件只渲染前 5000 行，避免 DOM 爆炸
function renderFileContent(body, data, line) {
  body.innerHTML = '';
  const lines = data.content.split('\n');
  const shown = lines.slice(0, FV_MAX_LINES);
  const list = document.createElement('div');
  list.className = 'fv-code';
  let target = null;
  shown.forEach((text, i) => {
    const row = document.createElement('div');
    row.className = 'fv-line';
    const no = document.createElement('span');
    no.className = 'fv-ln';
    no.textContent = i + 1;
    const lc = document.createElement('span');
    lc.className = 'fv-lc';
    lc.innerHTML = escapeHtml(text) || ' ';
    row.append(no, lc);
    if (line === i + 1) { row.classList.add('target'); target = row; }
    list.appendChild(row);
  });
  body.appendChild(list);
  if (lines.length > FV_MAX_LINES || data.truncated) {
    const more = document.createElement('div');
    more.className = 'fv-status';
    more.textContent = data.truncated ? '（文件过大，已截断）' : `（仅显示前 ${FV_MAX_LINES} 行）`;
    body.appendChild(more);
  }
  if (target) target.scrollIntoView({ block: 'center' });
}

// 点击消息里的本地文件链接 → 侧栏预览
$('#messages').addEventListener('click', (e) => {
  const a = e.target.closest('a[data-file-path]');
  if (!a) return;
  e.preventDefault();
  openFileViewer(a.dataset.filePath, parseInt(a.dataset.fileLine || '0', 10));
});

// 代码块复制按钮（md() 注入；主对话气泡和轨迹详情里的代码块都走这里）
document.addEventListener('click', async (e) => {
  const btn = e.target.closest('.code-copy-btn');
  if (!btn) return;
  const wrap = btn.closest('.code-block');
  const code = wrap && (wrap.querySelector('pre code') || wrap.querySelector('pre'));
  if (!code) return;
  try {
    await copyText(code.textContent);
    flashCheck(btn, btn.innerHTML);
  } catch (_) {}
});
$('#fv-close').onclick = closeFileViewer;
$('#fv-copy').onclick = async (e) => {
  const btn = e.currentTarget;
  try {
    await copyText($('#fv-path').textContent);
    flashCheck(btn, btn.innerHTML);
  } catch (_) {}
};

function wrapInMsg(node, box, ts) {
  // Attach to the last assistant message if present, else create one.
  const msgs = box || $('#messages');
  const last = msgs.lastElementChild;
  if (last && last.classList.contains('msg-assistant')) {
    last.querySelector('.bubble').appendChild(node);
  } else {
    addMsg('msg-assistant', 'claude', msgs, ts).appendChild(node);
  }
}

// ---------- partial streaming (stream_event) ----------
// CLI 开了 --include-partial-messages 后，除完整 assistant 事件外还会推原
// 始 API 流事件（type=stream_event）。text/thinking 的 delta 增量渲染进一
// 个带光标的 live 元素；block 完成时 CLI 会发含完整内容的 assistant 事件，
// renderEvent 用完整版替换对应 live 元素（去重）。若后端把 thinking strip
// 成空（完整版里没有内容），则固化保留 live 版。
// live 事件不落盘，历史回放里没有 stream_event，刷新后由完整事件兜底。

function liveBlock(kind) {
  // 当前活跃的 live block；类型切换时固化上一个（同时只有一个在流）
  const l = state.live;
  if (l && l.cur && l.cur.kind !== kind) liveFinalizeBlock(l.cur);
  return l && l.cur && l.cur.kind === kind ? l.cur : null;
}

function livePush(b) {
  if (!state.live) state.live = { msgId: '', blocks: [], cur: null };
  state.live.blocks.push(b);
  state.live.cur = b;
}

function liveFinalizeBlock(b) {
  b.done = true;
  if (b.raf) { cancelAnimationFrame(b.raf); b.raf = null; }
  livePaint(b);
  b.bodyEl.classList.remove('streaming');
  if (state.live && state.live.cur === b) state.live.cur = null;
}

function livePaint(b) {
  if (b.kind === 'text') b.bodyEl.innerHTML = md(b.buf);
  else b.bodyEl.textContent = b.buf;
}

function liveSchedulePaint(b) {
  if (b.raf) return; // 每帧最多重绘一次
  b.raf = requestAnimationFrame(() => {
    b.raf = null;
    livePaint(b);
    scrollBottom();
  });
}

// 取出最早创建的同类 live block（用于 assistant 完整事件到达时匹配替换）
function liveTake(kind) {
  const l = state.live;
  if (!l) return null;
  for (const b of l.blocks) {
    if (b.kind === kind && !b.taken) {
      b.taken = true;
      if (b.raf) { cancelAnimationFrame(b.raf); b.raf = null; } // 元素将被移除或固化，取消挂起的重绘
      return b;
    }
  }
  return null;
}

// turn 结束/中断时兜底：固化所有未被完整事件替换的 live block
function liveFinalizeAll() {
  const l = state.live;
  if (!l) return;
  for (const b of l.blocks) if (!b.taken && !b.done) liveFinalizeBlock(b);
  state.live = null;
}

function renderStreamEvent(d, box, ts) {
  const ev = d.event;
  if (!ev || typeof ev !== 'object') return;
  switch (ev.type) {
    case 'message_start':
      liveFinalizeAll();
      state.live = { msgId: (ev.message && ev.message.id) || '', blocks: [], cur: null };
      break;
    case 'content_block_start': {
      const cb = ev.content_block || {};
      const l = state.live;
      if (l && l.cur) liveFinalizeBlock(l.cur); // 新 block 开始，固化上一个（覆盖 text→text 等连续场景）
      if (cb.type === 'text') {
        const bubble = addMsg('msg-assistant', 'claude', box, ts);
        bubble.classList.add('streaming');
        const b = { kind: 'text', el: bubble.parentElement, bodyEl: bubble, buf: cb.text || '', done: false, taken: false, raf: null };
        livePush(b);
        liveSchedulePaint(b);
      } else if (cb.type === 'thinking') {
        const details = document.createElement('details');
        details.className = 'tool-block thinking-block';
        details.open = true; // 流式期间展开，完成后用户可手动折叠
        const summary = document.createElement('summary');
        summary.textContent = 'thinking…';
        const body = document.createElement('div');
        body.className = 'tool-body streaming';
        details.append(summary, body);
        wrapInMsg(details, box, ts);
        const b = { kind: 'thinking', el: details, bodyEl: body, buf: cb.thinking || '', done: false, taken: false, raf: null };
        livePush(b);
        liveSchedulePaint(b);
      } else {
        // tool_use 等：无增量渲染，等完整 assistant 事件（cur 已在上面固化）
      }
      break;
    }
    case 'content_block_delta': {
      const delta = ev.delta || {};
      const kind = delta.type === 'thinking_delta' ? 'thinking' : delta.type === 'text_delta' ? 'text' : null;
      if (!kind) break; // input_json_delta 等忽略
      const b = liveBlock(kind);
      if (!b) break;
      b.buf += kind === 'text' ? (delta.text || '') : (delta.thinking || '');
      liveSchedulePaint(b);
      break;
    }
    case 'content_block_stop': {
      const l = state.live;
      if (l && l.cur) liveFinalizeBlock(l.cur);
      break;
    }
    // message_delta/message_stop 无需处理：收尾由完整 assistant/result 事件承担
  }
}

// box defaults to the live message list; history pages render into a
// DocumentFragment which is then inserted at the top.
function renderEvent(ev, box) {
  box = box || $('#messages');
  const d = ev.data;
  if (ev.kind === 'rewind') {
    // Replay-time marker: the server already truncated history, so just render.
    appendRewindMarker(box, d && d.checkpoint, d && d.detail);
    return;
  }
  if (ev.kind === 'user') {
    // Replace any pending optimistic bubble with the authoritative one
    // (which contains only text — images are not stored in events).
    const last = box.lastElementChild;
    if (last && last.classList.contains('msg-optimistic')) last.remove();
    const bubble = addMsg('msg-user', '你', box, ev.ts);
    bubble.textContent = d.text;
    // A checkpoint uuid means this message can be rewound (files + dialogue).
    if (d && d.checkpoint) {
      const msgDiv = bubble.parentElement;
      msgDiv.dataset.checkpoint = d.checkpoint;
      addRewindBtn(msgDiv);
    }
    // User event authoritative bubble rendered → refresh pinned target
    if (box === $('#messages')) refreshPinnedPrompt();
  } else if (ev.kind === 'stream') {
    if (d.type === 'system') {
      if (d.subtype === 'init') {
        if (Array.isArray(d.slash_commands) && state.current) {
          state.current.slash_commands = d.slash_commands;
          const key = (state.current.cli || '') + '\x00' + (state.current.cwd || '');
          state.slashCommandsCache[key] = d.slash_commands;
          try { localStorage.setItem('cc-webui:slash-cache', JSON.stringify(state.slashCommandsCache)); } catch (_) {}
        }
        const line = document.createElement('div');
        line.className = 'meta-line';
        line.textContent = `session ${String(d.session_id || '').slice(0, 8)} · model ${d.model || '?'} · ${d.cwd || ''}`;
        box.appendChild(line);
      }
      return; // ignore thinking_tokens and other system noise
    }
    if (d.type === 'assistant' && d.parent_tool_use_id) {
      // 子代理消息（Agent 工具派生）：聚合进按 parent_tool_use_id 分组的
      // 折叠卡片，不作为主对话气泡渲染。
      renderSubagentEvent(d, box, ev.ts);
    } else if (d.type === 'assistant' && d.message && Array.isArray(d.message.content)) {
      for (const block of d.message.content) {
        if (block.type === 'text' && block.text) {
          const lb = liveTake('text');
          if (lb) lb.el.remove(); // 完整版替换 live 气泡，避免重复
          const bubble = addMsg('msg-assistant', 'claude', box, ev.ts);
          bubble.innerHTML = md(block.text);
        } else if (block.type === 'tool_use') {
          renderToolUse(block, box, ev.ts);
        } else if (block.type === 'thinking') {
          const lb = liveTake('thinking');
          if (block.thinking) {
            if (lb) lb.el.remove(); // 完整版替换 live thinking
            const details = document.createElement('details');
            details.className = 'tool-block thinking-block';
            const summary = document.createElement('summary');
            summary.textContent = 'thinking…';
            const body = document.createElement('div');
            body.className = 'tool-body';
            body.textContent = block.thinking;
            details.append(summary, body);
            wrapInMsg(details, box, ev.ts);
          } else if (lb) {
            liveFinalizeBlock(lb); // 完整版被后端 strip 成空，保留 live 版
          }
        }
      }
    } else if (d.type === 'stream_event') {
      renderStreamEvent(d, box, ev.ts);
    } else if (d.type === 'user' && d.origin && d.origin.kind === 'peer') {
      // 入站 peer 消息（其他会话 SendMessage 来的）：专用气泡，左边对齐、
      // 紫色描边以区别于自己的消息。body 是解码后的原文，按 md 渲染。
      const name = escapeHtml(d.origin.name || '另一个会话');
      const bubble = addMsg('msg-peer', `⇢ 来自 ${name}`, box, ev.ts);
      bubble.innerHTML = md(d.origin.body || '');
    } else if (d.type === 'user' && d.parent_tool_use_id && d.message && Array.isArray(d.message.content)) {
      // 子代理的 tool_result：配对进对应卡片的工具视图（result 到得早于
      // 卡片创建时也会先把卡片壳建出来）
      const card = ensureSubagentCard(d.parent_tool_use_id, d, box, ev.ts);
      for (const b of d.message.content) {
        if (b && b.type === 'tool_result') renderToolResult(b, card.body, true);
      }
    } else if (d.type === 'user' && !d.parent_tool_use_id && d.message && Array.isArray(d.message.content)) {
      for (const block of d.message.content) {
        if (block && block.type === 'tool_result') renderToolResult(block, box);
      }
    } else if (d.type === 'result') {
      liveFinalizeAll(); // 兜底：未被完整 assistant 事件替换的 live block 固化保留
      const meta = document.createElement('div');
      meta.className = 'result-meta';
      const parts = [];
      if (d.subtype) parts.push(d.subtype);
      if (typeof d.duration_ms === 'number') parts.push(`${(d.duration_ms / 1000).toFixed(1)}s`);
      if (typeof d.total_cost_usd === 'number') parts.push(`$${d.total_cost_usd.toFixed(4)}`);
      if (d.usage) {
        const u = d.usage;
        parts.push(`tokens in:${u.input_tokens || 0} out:${u.output_tokens || 0}`);
      }
      meta.textContent = parts.join(' · ');
      box.appendChild(meta);
    }
  } else if (ev.kind === 'status') {
    if (d.state === 'turn_start') {
      updateStatusUI({ running: true, queued: 0 });
    } else if (d.state === 'turn_end' || d.state === 'interrupted') {
      liveFinalizeAll(); // 中断时部分 block 永远等不到完整事件，固化已生成的内容
      updateStatusUI({ running: false, queued: 0 });
      // Submit any message held while this turn was running. 历史回放里的旧
      // turn_end 不算数——由 flushHistory 在回放结束后按最新状态统一补交。
      if (!state.renderingHistory) {
        flushPending();
        refreshCurrentStatus();
      }
    } else if (d.state === 'error') {
      liveFinalizeAll();
      updateStatusUI({ running: false, queued: 0 });
      // For recoverable CLI errors (e.g. ducc's unrecognized_model warning
      // after usable output has already streamed), show a warning banner and
      // allow the pending message to flush so the user can keep chatting.
      const banner = $('#error-banner');
      banner.classList.remove('hidden');
      if (d.recoverable) {
        banner.classList.add('warning');
        banner.textContent = d.error || 'CLI 报告非致命错误';
        if (!state.renderingHistory) {
          flushPending();
          refreshCurrentStatus();
        }
      } else {
        banner.classList.remove('warning');
        banner.textContent = d.error || 'unknown error';
        // Don't auto-flush on hard error — leave the pending bubble visible so the
        // user can cancel or wait and resend after the failure is resolved.
        refreshCurrentStatus();
      }
    }
  }
}

// 子代理消息卡片：Agent 工具派生的 subagent 在聊天流里按 parent_tool_use_id
// 聚合成一个可折叠卡片。标题优先用服务端回放给的 d.subagent（来自
// meta.json），实时流没有它时回查 agentToolMeta（Agent tool_use 的 input）。
// 渲染 text 块与 tool_use 块（与回放侧 ConvertSubagents 的产出一致，
// thinking 两侧都剔——CLI 实时透传会把完整事件里的 thinking strip 掉）；
// 子代理的 tool_result 经 renderEvent 的 user 分支配对进卡片。默认折叠。
function ensureSubagentCard(key, d, box, ts) {
  let card = state.subagentCards[key];
  if (card) return card;
  const sa = (d && d.subagent) || {};
  const meta = state.agentToolMeta[key] || {};
  const title = sa.title || meta.title || '子代理';
  const type = sa.type || meta.type || '';
  const details = document.createElement('details');
  details.className = 'tool-block subagent-block';
  const summary = document.createElement('summary');
  const body = document.createElement('div');
  body.className = 'tool-body subagent-body';
  details.append(summary, body);
  wrapInMsg(details, box, ts);
  const label = `🤖 子代理${type ? ' · ' + escapeHtml(type) : ''} · ${escapeHtml(title)}`;
  card = state.subagentCards[key] = { summary, body, count: 0, tools: 0, label };
  return card;
}

function subagentRefreshSummary(card) {
  const parts = [];
  if (card.count) parts.push(card.count + ' 条');
  if (card.tools) parts.push(card.tools + ' 次调用');
  card.summary.innerHTML = card.label + (parts.length ? `<span class="subagent-count">${parts.join(' · ')}</span>` : '');
}

function renderSubagentEvent(d, box, ts) {
  const card = ensureSubagentCard(d.parent_tool_use_id, d, box, ts);
  const blocks = (d.message && Array.isArray(d.message.content)) ? d.message.content : [];
  for (const b of blocks) {
    if (!b) continue;
    if (b.type === 'text' && b.text) {
      const div = document.createElement('div');
      div.className = 'subagent-msg';
      div.innerHTML = md(b.text);
      card.body.appendChild(div);
      card.count++;
    } else if (b.type === 'tool_use' && b.name) {
      // raw：工具视图直接挂进卡片体，不包消息气泡壳；结果由 tool_result
      // 事件经 state.toolBlocks 配对回同一个 details 里
      renderToolUse(b, card.body, ts, true);
      card.tools++;
    }
  }
  subagentRefreshSummary(card);
}

async function refreshCurrentStatus() {
  if (!state.current) return;
  try {
    const s = await api(`/api/sessions/${state.current.id}`);
    state.current = s;
    updateStatusUI(s.status);
    updateResidentUI();
    loadSessions();
  } catch (_) {}
}

// ---------- composer ----------
const input = $('#input');

// The composer floats over #messages (absolute on desktop, fixed on mobile),
// which reserves room for it via padding-bottom. That padding must track the
// composer's real height — the textarea auto-grows, and attachments / error
// banner / workspace gate make it taller — otherwise an expanded composer
// covers the bottom of the last messages.
const composerEl = $('#composer');
const messagesEl = $('#messages');
const trajViewEl = $('#traj-view');
function syncComposerSpace() {
  const gap = parseFloat(getComputedStyle(composerEl).bottom) || 0;
  const pad = `${composerEl.offsetHeight + gap + 8}px`;
  messagesEl.style.paddingBottom = pad;
  if (trajViewEl) trajViewEl.style.paddingBottom = pad;
}
new ResizeObserver(() => {
  syncComposerSpace();
  // Padding grows downward; keep the view pinned if the user was at the bottom.
  if (state.nearBottom) messagesEl.scrollTop = messagesEl.scrollHeight;
}).observe(composerEl);
syncComposerSpace();

// ---------- slash command autocomplete ----------
// Slash commands come from the system/init event (persisted server-side in
// session.SlashCommands, also captured live in renderEvent). We detect `/` at
// the start of a word, show a filtered popup, and let the user navigate with
// arrow keys / Tab. Selected commands get a blue highlight in the input via
// a mirror div behind the transparent-text textarea.

const slashPopupEl = $('#slash-popup');
const inputMirror = $('#input-mirror');
const nsSlashPopupEl = $('#ns-slash-popup');
const nsInputEl = $('#ns-input');
let slashState = { open: false, items: [], active: 0 };
// 活跃的 slash 上下文：哪个 textarea + 哪个 popup 在用（focus 切换）
let slashPopup = slashPopupEl;
let slashTextarea = input;

function getSlashCommands() {
  if (state.current) {
    const key = (state.current.cli || '') + '\x00' + (state.current.cwd || '');
    return state.current.slash_commands || state.slashCommandsCache[key] || [];
  }
  // ns-view 上下文：用 nsPendingCwd + #ns-cli 查缓存
  const cli = (nsInputEl && $('#ns-cli')?.value) || '';
  const key = cli + '\x00' + (nsPendingCwd || '');
  return state.slashCommandsCache[key] || [];
}

// Find the slash token being typed: `/word` at start of input or after whitespace.
function currentSlashToken(text, pos) {
  const before = text.slice(0, pos);
  const m = before.match(/(?:^|\s)(\/[\w-]*)$/);
  return m ? { prefix: m[1], start: pos - m[1].length } : null;
}

function openSlashPopup(filter) {
  const commands = getSlashCommands();
  const q = filter.toLowerCase();
  const items = q ? commands.filter((c) => c.toLowerCase().includes(q)) : commands.slice();
  if (items.length === 0) { closeSlashPopup(); return; }
  slashState.open = true;
  slashState.items = items;
  slashState.active = 0;
  slashPopup.innerHTML = '';
  for (let i = 0; i < items.length; i++) {
    const el = document.createElement('div');
    el.className = 'slash-popup-item' + (i === 0 ? ' active' : '');
    el.innerHTML = `<span class="slash-name">/${escapeHtml(items[i])}</span>`;
    el.onclick = () => selectSlashItem(i);
    slashPopup.appendChild(el);
  }
  slashPopup.classList.remove('hidden');
}

function closeSlashPopup() {
  slashState.open = false;
  slashState.items = [];
  slashPopup.classList.add('hidden');
}

function moveSlashActive(delta) {
  if (!slashState.open) return;
  const n = slashState.items.length;
  slashState.active = (slashState.active + delta + n) % n;
  for (const el of slashPopup.children) el.classList.remove('active');
  slashPopup.children[slashState.active].classList.add('active');
  slashPopup.children[slashState.active].scrollIntoView({ block: 'nearest' });
}

function selectSlashItem(idx) {
  if (!slashState.open) return;
  const name = slashState.items[idx ?? slashState.active];
  if (!name) return;
  const ta = slashTextarea;
  const text = ta.value;
  const pos = ta.selectionStart;
  const before = text.slice(0, pos);
  const m = before.match(/(?:^|\s)(\/[\w-]*)$/);
  if (!m) { closeSlashPopup(); return; }
  const start = pos - m[1].length;
  const replacement = (m[0].startsWith(' ') ? ' ' : '') + '/' + name + ' ';
  const newText = text.slice(0, start) + replacement + text.slice(pos);
  ta.value = newText;
  const caret = start + replacement.length;
  ta.setSelectionRange(caret, caret);
  ta.focus();
  closeSlashPopup();
  onInputChange();
}

// Render the mirror: slash command tokens that match known commands get blue.
// The mirror ALWAYS renders the text so the transparent textarea stays readable.
function renderInputMirror() {
  const text = input.value;
  if (!text) { inputMirror.innerHTML = ''; return; }
  const commands = getSlashCommands();
  const cmdSet = commands.length ? new Set(commands) : null;
  const parts = text.split(/(\s+)/);
  let html = '';
  for (const part of parts) {
    if (cmdSet && part.startsWith('/') && cmdSet.has(part.slice(1))) {
      html += `<span class="slash-cmd">${escapeHtml(part)}</span>`;
    } else {
      html += escapeHtml(part);
    }
  }
  inputMirror.innerHTML = html;
  // 镜像高度钉到 textarea 实际高度（180 封顶），它才成为真正的滚动盒，
  // scrollTop 同步才会生效（内容超高后 textarea 内部滚动，镜像不滚则光标错位）。
  inputMirror.style.height = input.clientHeight + 'px';
  inputMirror.scrollTop = input.scrollTop;
}

function onInputChange() {
  const ta = slashTextarea;
  ta.style.height = 'auto';
  ta.style.height = Math.min(ta.scrollHeight, ta === input ? 180 : 320) + 'px';
  if (state.current && ta === input) draft().text = ta.value;
  // Slash popup detection
  const tok = currentSlashToken(ta.value, ta.selectionStart);
  if (tok) {
    const filter = tok.prefix.slice(1); // without the leading /
    openSlashPopup(filter);
  } else {
    closeSlashPopup();
  }
  if (ta === input) renderInputMirror();
}

input.addEventListener('focus', () => { slashTextarea = input; slashPopup = slashPopupEl; });
input.addEventListener('input', onInputChange);
input.addEventListener('scroll', renderInputMirror);
input.addEventListener('keydown', (e) => {
  if (slashState.open) {
    if (e.key === 'ArrowDown') { e.preventDefault(); moveSlashActive(1); return; }
    if (e.key === 'ArrowUp') { e.preventDefault(); moveSlashActive(-1); return; }
    if (e.key === 'Tab' || (e.key === 'Enter' && !e.shiftKey)) {
      e.preventDefault(); selectSlashItem(); return;
    }
    if (e.key === 'Escape') { e.preventDefault(); closeSlashPopup(); return; }
  }
  if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
    e.preventDefault();
    sendMessage();
  }
});
input.addEventListener('blur', () => { setTimeout(closeSlashPopup, 200); });
input.addEventListener('paste', async (e) => {
  const items = e.clipboardData && e.clipboardData.items;
  if (!items) return;
  const files = [];
  for (const item of items) {
    if (item.kind === 'file') {
      const f = item.getAsFile();
      if (f) files.push(f);
    }
  }
  if (files.length) {
    e.preventDefault();
    await attachFiles(files);
  }
});

$('#btn-send').onclick = sendMessage;

// ---------- attachments ----------
// 两类附件：图片走流式多模态（base64 内联），其他文件上传到服务器落盘
// （data/uploads/<sid>/），agent 通过消息文本里的路径用 Read/Bash 处理。
const ALLOWED_MIME = ['image/png', 'image/jpeg', 'image/gif', 'image/webp'];
const MAX_IMAGES = 4;
const MAX_FILES = 8;
const MAX_IMAGE_BYTES = 10 * 1024 * 1024; // 10MB per image
const MAX_FILE_BYTES = 40 * 1024 * 1024;  // 40MB per file

$('#btn-attach').onclick = () => $('#file-input').click();
$('#file-input').addEventListener('change', async (e) => {
  await attachFiles([...e.target.files]);
  e.target.value = '';
});

input.addEventListener('dragover', (e) => {
  if (!e.dataTransfer || !e.dataTransfer.types.includes('Files')) return;
  e.preventDefault();
  e.dataTransfer.dropEffect = 'copy';
});
input.addEventListener('drop', async (e) => {
  if (!e.dataTransfer || !e.dataTransfer.files.length) return;
  e.preventDefault();
  await attachFiles([...e.dataTransfer.files]);
});

async function attachFiles(files) {
  // opencode 引擎没有内嵌图片（base64）通道（服务端 400）；图片降级为文件
  // 附件上传，runner 以 `opencode run -f` 官方附件传给模型——视觉模型直接
  // 可见，文本模型也能用工具读取。
  const ocSession = state.current && state.current.engine === 'opencode';
  for (const f of files) {
    if (ALLOWED_MIME.includes(f.type) && !ocSession) {
      const imgs = state.attached.filter((a) => a.kind === 'image').length;
      if (imgs >= MAX_IMAGES) {
        alert(`最多 ${MAX_IMAGES} 张图片`);
        continue;
      }
      if (f.size > MAX_IMAGE_BYTES) {
        alert(`图片过大：${f.name}（${fmtMiB(f.size)}MB > ${MAX_IMAGE_BYTES >> 20}MB）`);
        continue;
      }
      const data = await readAsDataURL(f).then(stripDataURLPrefix);
      state.attached.push({ kind: 'image', media_type: f.type, data });
      continue;
    }
    // 非图片文件：先上传到服务器落盘，后面发送的消息只带路径元数据，
    // 待发送/草稿/切会话都不会带着大 base64 在内存里打转。
    const nFiles = state.attached.filter((a) => a.kind === 'file').length;
    if (nFiles >= MAX_FILES) {
      alert(`最多 ${MAX_FILES} 个文件附件`);
      continue;
    }
    if (f.size > MAX_FILE_BYTES) {
      alert(`文件过大：${f.name}（${fmtMiB(f.size)}MB > ${MAX_FILE_BYTES >> 20}MB）`);
      continue;
    }
    try {
      const up = await uploadAttachment(f, state.current.id);
      state.attached.push({ kind: 'file', name: up.name, size: up.size, path: up.path });
    } catch (e) {
      alert(`上传失败：${f.name}（${e.message || 'unknown error'}）`);
      continue;
    }
  }
  if (state.current) draft().attachments = state.attached;
  renderAttachedThumbs();
}

// POST the raw file bytes to the server; it persists them and returns the
// on-disk path that the agent will be told about. sid 显式传入——主 composer
// 传当前会话；新建会话流程在会话建成后才上传，也走这里。
async function uploadAttachment(f, sid) {
  if (!sid) throw new Error('请先进入一个会话再添加附件');
  return api(`/api/sessions/${sid}/uploads?name=${encodeURIComponent(f.name)}`, {
    method: 'POST',
    body: f,
    headers: { 'Content-Type': 'application/octet-stream' },
  });
}

function fmtMiB(n) { return (n / 1024 / 1024).toFixed(1); }
function fmtBytes(n) {
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(0) + ' KB';
  return fmtMiB(n) + ' MB';
}

function readAsDataURL(file) {
  return new Promise((resolve, reject) => {
    const r = new FileReader();
    r.onload = () => resolve(r.result);
    r.onerror = () => reject(r.error);
    r.readAsDataURL(file);
  });
}
function stripDataURLPrefix(s) {
  const i = s.indexOf(',');
  return i >= 0 ? s.slice(i + 1) : s;
}

function renderAttachedThumbs() {
  const box = $('#attached-images');
  box.innerHTML = '';
  state.attached.forEach((att, i) => {
    const t = document.createElement('div');
    if (att.kind === 'image') {
      t.className = 'attached-thumb';
      t.innerHTML = `<img alt=""><button class="x" title="移除"><i data-lucide="x"></i></button>`;
      t.querySelector('img').src = `data:${att.media_type};base64,${att.data}`;
    } else {
      t.className = 'attached-thumb attached-file';
      t.innerHTML = `<i data-lucide="file" class="af-icon"></i><span class="af-name"></span><span class="af-size"></span><button class="x" title="移除"><i data-lucide="x"></i></button>`;
      t.querySelector('.af-name').textContent = att.name;
      t.querySelector('.af-name').title = att.name;
      t.querySelector('.af-size').textContent = fmtBytes(att.size);
    }
    t.querySelector('.x').onclick = () => {
      state.attached.splice(i, 1);
      if (state.current) draft().attachments = state.attached;
      renderAttachedThumbs();
    };
    box.appendChild(t);
  });
  renderIcons(box);
}

// Is a turn currently running for the open session? Derived from the session
// status (set live by status SSE events and refreshCurrentStatus).
function isRunning() {
  return !!(state.current && state.current.status && state.current.status.running);
}

async function sendMessage() {
  const text = input.value.trim();
  if ((!text && state.attached.length === 0) || !state.current) return;
  const images = state.attached.filter((a) => a.kind === 'image');
  // 文件附件只带服务端已落盘的路径元数据，不带上大文件字节。
  const files = state.attached.filter((a) => a.kind === 'file').map((a) => ({ name: a.name, path: a.path }));
  // Clear the composer regardless of path: the text now lives either in the
  // POST (sent now) or in state.pendingBySid (held for the running turn to end).
  input.value = '';
  input.style.height = 'auto';
  renderInputMirror();
  state.attached = [];
  if (state.current) { draft().text = ''; draft().attachments = []; }
  renderAttachedThumbs();
  $('#error-banner').classList.add('hidden');

  // While a turn is streaming, don't POST immediately — the backend would
  // queue it, but the optimistic bubble lands mid-stream and the on-screen
  // order diverges from the actual (post-turn) order. Hold the message and
  // auto-submit when the running turn ends.
  if (isRunning()) {
    holdPending(text, images, files);
    return;
  }
  await postMessage({ text, images, files });
}

// POST a message and optimistically render its user bubble.
async function postMessage(payload) {
  appendOptimisticUser(payload.text, payload.images, payload.files);
  try {
    await api(`/api/sessions/${state.current.id}/messages`, {
      method: 'POST',
      body: JSON.stringify(payload),
    });
    updateStatusUI({ running: true, queued: 0 });
  } catch (e) {
    // Send failed: drop the optimistic bubble and surface the error so the
    // user can retry, instead of leaving a ghost message.
    $('#messages').querySelectorAll('.msg-optimistic').forEach((n) => n.remove());
    throw e;
  }
}

// Hold a message until the running turn ends, then flushPending submits it.
// 按会话保存：切走再切回 chip 仍在，轮次在切走期间结束的由 flushHistory 补交。
// Renders a dashed chip docked to the composer so the user sees what's queued.
function holdPending(text, images, files) {
  if (!state.current) return;
  // A second send while one is already held replaces it (latest intent wins).
  state.pendingBySid[state.current.id] = { text, images, files };
  renderPendingBubble();
}

// Submit the held message (if any) now that the session is idle. Called from
// the status event handler on live turn_end / interrupted, and from
// flushHistory when the turn ended while the session was in the background.
async function flushPending() {
  if (!state.current) return;
  const sid = state.current.id;
  const payload = state.pendingBySid[sid];
  if (!payload) return;
  delete state.pendingBySid[sid];
  clearPendingBubble();
  try {
    await postMessage(payload);
  } catch (e) {
    // Re-hold on transient failure so it isn't lost; surface the error.
    // 若 await 期间用户已切走，payload 仍挂回原会话，chip 由 renderPendingBubble
    // 按当前会话重绘（当前会话无 pending 时等于清除）。
    state.pendingBySid[sid] = payload;
    renderPendingBubble();
    const banner = $('#error-banner');
    banner.textContent = '待发送消息提交失败：' + (e.message || 'unknown error');
    banner.classList.remove('hidden');
  }
}

function cancelPending() {
  if (state.current) delete state.pendingBySid[state.current.id];
  clearPendingBubble();
}

// ---------- pending message chip (pinned to the composer) ----------
// A held message shows as a dashed chip docked at the top of the composer card
// (above the attachments row) with a 待发送 label and a cancel button. It's a
// composer affordance, not part of the message stream — living inside the
// floating composer it never scrolls away while the turn streams, and the
// composer's ResizeObserver grows the #messages padding to make room for it.
function pendingEl() { return $('#pending-bubble'); }

function renderPendingBubble() {
  const p = state.current && state.pendingBySid[state.current.id];
  if (!p) { clearPendingBubble(); return; }
  let el = pendingEl();
  if (!el) {
    el = document.createElement('div');
    el.id = 'pending-bubble';
    el.className = 'pending-bubble';
    el.innerHTML = '<span class="pb-tag"></span><span class="pb-text"></span><button class="pb-cancel" type="button" title="取消待发送"></button>';
    el.querySelector('.pb-cancel').onclick = cancelPending;
    const cancelIcon = document.createElement('i');
    cancelIcon.dataset.lucide = 'x';
    el.querySelector('.pb-cancel').appendChild(cancelIcon);
    renderIcons(el);
  }
  // 固定在输入框卡片顶部（insertBefore 对已有节点是移动），与消息流无关，
  // 流式事件再多也不会把它顶走。
  const card = $('.composer-card');
  card.insertBefore(el, card.firstChild);
  el.querySelector('.pb-tag').textContent = '待发送 · 本轮结束后自动提交';
  const txt = el.querySelector('.pb-text');
  const label = p.text || ((p.images && p.images.length) || (p.files && p.files.length) ? '(仅附件)' : '');
  txt.textContent = label;
  el.classList.toggle('has-text', !!label);
  el.classList.toggle('has-images', !!(p.images && p.images.length));
}

function clearPendingBubble() {
  const el = pendingEl();
  if (el) el.remove();
}

// ---------- 任务列表面板（dock 在输入框卡片顶部） ----------
// 数据源是 historyBuf：回放、实时事件、rewind 截断都汇聚到它，面板状态从它
// 折叠（fold）出来，单一事实来源。重置点（切会话/重连/rewind）全量重折，
// 实时事件增量折叠。追踪 TaskCreate/TaskUpdate（新任务系统），旧会话的
// TodoWrite 作为回退（其入参每次都是全量快照，最后一次调用生效）。
// 任务 id 由 CLI 分配，TaskCreate 入参里没有——先以 callId 占位，等
// tool_result（"Task #N created successfully"）到达后换成真实 id。

function taskFoldReset() {
  state.taskFold = { tasks: {}, order: [], pendingCreates: {}, pendingLists: {}, todos: null };
  state.taskPanelTouched = false;
}

// 全量重折：回放 flush / rewind 截断后调用（缓冲区的增删使增量状态失效）
function taskFoldAll() {
  taskFoldReset();
  for (const ev of state.historyBuf) taskFoldEvent(ev);
}

// 增量折叠单个事件；返回任务状态是否有变化
function taskFoldEvent(ev) {
  const f = state.taskFold;
  if (!f || ev.kind !== 'stream' || !ev.data) return false;
  const d = ev.data;
  const content = d.message && Array.isArray(d.message.content) ? d.message.content : null;
  if (!content) return false;
  let changed = false;
  if (d.type === 'assistant') {
    for (const b of content) {
      if (!b || b.type !== 'tool_use') continue;
      const input = b.input || {};
      switch ((b.name || '').toLowerCase()) {
        case 'todowrite':
          if (Array.isArray(input.todos)) { f.todos = input.todos; changed = true; }
          break;
        case 'taskcreate': {
          const tmp = 'tmp:' + (b.id || f.order.length);
          f.tasks[tmp] = { id: tmp, subject: input.subject || '', description: input.description || '', activeForm: input.activeForm || '', status: 'pending' };
          f.order.push(tmp);
          if (b.id) f.pendingCreates[b.id] = tmp;
          changed = true;
          break;
        }
        case 'taskupdate': {
          const id = String(input.taskId || '');
          if (!id) break;
          if (input.status === 'deleted') { // deleted 任务直接移除，与 CLI 行为一致
            if (f.tasks[id]) { delete f.tasks[id]; f.order = f.order.filter((x) => x !== id); changed = true; }
            break;
          }
          // 引用了未知 id（任务建在本会话之外）→ 先立占位，字段后续补齐
          const t = f.tasks[id] || (f.tasks[id] = { id, subject: '', description: '', activeForm: '', status: 'pending' }, f.order.push(id), f.tasks[id]);
          if (input.subject) t.subject = input.subject;
          if (input.description) t.description = input.description;
          if (input.activeForm) t.activeForm = input.activeForm;
          if (input.status) t.status = input.status;
          changed = true;
          break;
        }
        case 'tasklist': // 状态在结果里，记下 callId 等 tool_result
          if (b.id) f.pendingLists[b.id] = true;
          break;
      }
    }
  } else if (d.type === 'user') {
    for (const b of content) {
      if (!b || b.type !== 'tool_result') continue;
      const tmp = f.pendingCreates[b.tool_use_id];
      if (tmp) {
        delete f.pendingCreates[b.tool_use_id];
        const m = /Task #(\d+) created/i.exec(toolResultText(b.content));
        const t = f.tasks[tmp];
        if (m && t && !f.tasks[m[1]]) {
          // 换真实 id，保持列表位置不变
          delete f.tasks[tmp];
          t.id = m[1];
          f.tasks[t.id] = t;
          f.order = f.order.map((x) => (x === tmp ? t.id : x));
        } else {
          // 创建失败（错误结果）或同 id 已存在 → 丢弃占位项
          delete f.tasks[tmp];
          f.order = f.order.filter((x) => x !== tmp);
        }
        changed = true;
      } else if (f.pendingLists[b.tool_use_id]) {
        delete f.pendingLists[b.tool_use_id];
        // TaskList 结果为 JSON 数组时并入（任务建在本会话之外的兜底）；纯文本
        // 格式不解析——常规流程已被 create/update 折叠覆盖
        let arr = null;
        try { arr = JSON.parse(toolResultText(b.content)); } catch (_) {}
        if (Array.isArray(arr)) {
          for (const it of arr) {
            if (!it || it.id == null) continue;
            const id = String(it.id);
            if (it.status === 'deleted') {
              if (f.tasks[id]) { delete f.tasks[id]; f.order = f.order.filter((x) => x !== id); }
              continue;
            }
            const t = f.tasks[id] || (f.tasks[id] = { id, subject: '', description: '', activeForm: '', status: 'pending' }, f.order.push(id), f.tasks[id]);
            if (it.subject) t.subject = it.subject;
            if (it.description) t.description = it.description;
            if (it.activeForm) t.activeForm = it.activeForm;
            if (it.status) t.status = it.status;
          }
          changed = true;
        }
      }
    }
  }
  return changed;
}

// 面板当前应展示的条目：Task* 模型优先，为空则回退 TodoWrite 快照
function taskPanelItems() {
  const f = state.taskFold;
  if (!f) return [];
  if (f.order.length) return f.order.map((id) => f.tasks[id]).filter(Boolean);
  return (f.todos || []).filter((t) => t && typeof t.content === 'string').map((t) => ({
    id: '', subject: t.content, activeForm: t.activeForm || '', status: t.status || 'pending',
  }));
}

function taskPanelEl() {
  let el = $('#task-panel');
  if (!el) {
    el = document.createElement('div');
    el.id = 'task-panel';
    el.className = 'task-panel hidden';
    el.innerHTML = '<button type="button" class="tp-head">' +
      '<span class="tp-chevron"><i data-lucide="chevron-down"></i></span>' +
      '<span class="tp-title">任务</span><span class="tp-progress"></span>' +
      '<span class="tp-spin ct-spinner hidden"></span>' +
      '<span class="tp-current"></span>' +
      '</button><div class="tp-list"></div>';
    el.querySelector('.tp-head').onclick = () => {
      state.taskPanelOpen = !state.taskPanelOpen;
      state.taskPanelTouched = true;
      syncTaskPanel();
    };
    renderIcons(el);
  }
  return el;
}

function syncTaskPanel() {
  const el = taskPanelEl();
  const items = state.current ? taskPanelItems() : [];
  if (!items.length) { el.classList.add('hidden'); return; }
  // 固定在输入框卡片顶部、附件行之上（insertBefore 对已有节点是移动）；
  // 待发送 chip 在更上面。与消息流无关，流式事件不会把它顶走。
  const card = $('.composer-card');
  if (card && el.parentElement !== card) card.insertBefore(el, $('#attached-images'));
  el.classList.remove('hidden');
  const done = items.filter((t) => t.status === 'completed').length;
  const allDone = done === items.length;
  el.querySelector('.tp-progress').textContent = `${done}/${items.length}`;
  const cur = items.find((t) => t.status === 'in_progress');
  el.querySelector('.tp-spin').classList.toggle('hidden', !cur);
  const curEl = el.querySelector('.tp-current');
  curEl.textContent = cur ? (cur.activeForm || cur.subject) : (allDone ? '全部完成' : '');
  curEl.title = curEl.textContent;
  // 未手动开合过时自动收展：全部完成收起成一行，来了新任务重新展开
  if (!state.taskPanelTouched) state.taskPanelOpen = !allDone;
  el.classList.toggle('open', state.taskPanelOpen);
  const list = el.querySelector('.tp-list');
  list.innerHTML = '';
  if (!state.taskPanelOpen) return;
  for (const t of items) {
    const row = document.createElement('div');
    // 复用 TodoWrite 的状态着色（todo-check 绿勾/蓝点/空心）
    row.className = 'tp-item todo-item todo-' + (t.status || 'pending');
    const check = document.createElement('span');
    check.className = 'todo-check';
    row.appendChild(check);
    if (t.id) {
      const idEl = document.createElement('span');
      idEl.className = 'tp-id';
      idEl.textContent = '#' + t.id;
      row.appendChild(idEl);
    }
    const text = document.createElement('span');
    text.className = 'todo-text';
    // 进行中的行优先显示 activeForm（"正在做某事"），与 CLI 一致
    text.textContent = (t.status === 'in_progress' && t.activeForm) ? t.activeForm : (t.subject || '(未命名)');
    text.title = text.textContent;
    row.appendChild(text);
    list.appendChild(row);
  }
}

function appendOptimisticUser(text, images, files) {
  const bubble = addMsg('msg-user msg-optimistic', '你', null, new Date());
  if (images && images.length) {
    for (const img of images) {
      const im = document.createElement('img');
      im.className = 'thumb';
      im.src = `data:${img.media_type};base64,${img.data}`;
      im.alt = '';
      bubble.appendChild(im);
    }
  }
  if (files && files.length) {
    for (const f of files) {
      const chip = document.createElement('span');
      chip.className = 'file-chip';
      const ic = document.createElement('i');
      ic.dataset.lucide = 'file';
      chip.appendChild(ic);
      chip.appendChild(document.createTextNode(f.name));
      bubble.appendChild(chip);
    }
  }
  if (text) bubble.appendChild(document.createTextNode((text ? ' ' : '') + text));
  if (bubble.querySelector('i')) renderIcons(bubble);
  scrollBottom();
  refreshPinnedPrompt();
}

$('#btn-stop').onclick = async () => {
  if (state.current) await api(`/api/sessions/${state.current.id}/stop`, { method: 'POST' });
};

async function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) {
    return navigator.clipboard.writeText(text);
  }
  return new Promise((resolve, reject) => {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.style.cssText = 'position:fixed;opacity:0';
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand('copy') ? resolve() : reject(new Error('copy failed')); }
    catch (e) { reject(e); }
    finally { ta.remove(); }
  });
}

async function flashCheck(btn, originalHTML) {
  btn.innerHTML = '<i data-lucide="check"></i>';
  renderIcons(btn);
  setTimeout(() => { btn.innerHTML = originalHTML; renderIcons(btn); }, 1200);
}

$('#btn-resume').onclick = async () => {
  if (!state.current) return;
  const btn = $('#btn-resume');
  const cmd = state.current.resume_command;
  if (!cmd) {
    alert(state.current.engine === 'opencode'
      ? '该会话还没有产生 opencode session（发过首轮消息后可用）'
      : '该会话还没有产生 claude session（发过首轮消息后可用）');
    return;
  }
  try {
    await copyText(cmd);
    flashCheck(btn, originalHTML);
  } catch (_) {
    prompt('复制失败，请手动复制：', cmd);
  }
};

// ---------- header controls ----------
let nameTimer = null;
$('#session-name').addEventListener('input', (e) => {
  clearTimeout(nameTimer);
  const name = e.target.value;
  nameTimer = setTimeout(async () => {
    if (!state.current) return;
    await api(`/api/sessions/${state.current.id}`, {
      method: 'PATCH', body: JSON.stringify({ name }),
    });
    loadSessions();
  }, 600);
});

$('#session-cli').addEventListener('change', async (e) => {
  if (!state.current) return;
  const updated = await api(`/api/sessions/${state.current.id}`, {
    method: 'PATCH', body: JSON.stringify({ cli: e.target.value }),
  });
  state.current = updated;
  patchSheetIfOpen(updated);
  loadSessions();
});

$('#session-provider').addEventListener('change', async (e) => {
  if (!state.current) return;
  const updated = await api(`/api/sessions/${state.current.id}`, {
    method: 'PATCH', body: JSON.stringify({ provider: e.target.value }),
  });
  state.current = updated;
  // 服务端会清掉新 provider 里不存在的 model override，以返回值为准重建下拉
  fillModelSelect($('#session-model'), updated.provider, updated.model_override);
  patchSheetIfOpen(updated);
  loadSessions();
});

$('#session-model').addEventListener('change', async (e) => {
  if (!state.current) return;
  const updated = await api(`/api/sessions/${state.current.id}`, {
    method: 'PATCH', body: JSON.stringify({ model: e.target.value }),
  });
  state.current = updated;
  patchSheetIfOpen(updated);
});

$('#session-effort').addEventListener('change', async (e) => {
  if (!state.current) return;
  const updated = await api(`/api/sessions/${state.current.id}`, {
    method: 'PATCH', body: JSON.stringify({ effort: e.target.value }),
  });
  state.current = updated;
  patchSheetIfOpen(updated);
});

$('#session-perm').addEventListener('change', async (e) => {
  if (!state.current) return;
  const updated = await api(`/api/sessions/${state.current.id}`, {
    method: 'PATCH', body: JSON.stringify({ permission_mode: e.target.value }),
  });
  state.current = updated;
  patchSheetIfOpen(updated);
});

$('#btn-delete').onclick = async () => {
  if (!state.current) return;
  if (!confirm(`删除会话「${state.current.name}」？历史事件将被清除。`)) return;
  await api(`/api/sessions/${state.current.id}`, { method: 'DELETE' });
  if (state.es) state.es.close();
  state.current = null;
  $('#chat-view').classList.add('hidden');
  $('#empty-state').classList.remove('hidden');
  loadSessions();
};

function setSidebarCollapsed(collapsed) {
  document.body.classList.toggle('sidebar-collapsed', collapsed);
}
$('#btn-collapse').onclick = () => setSidebarCollapsed(true);
$('#btn-expand').onclick = () => setSidebarCollapsed(false);

// ---------- sidebar drag-to-resize ----------
(function initSidebarResize() {
  const splitter = $('#sidebar-splitter');
  const sidebar = $('#sidebar');
  if (!splitter || !sidebar) return;
  const KEY = 'cc-webui-sidebar-width';
  const MIN = 220, MAX = 480;
  const saved = parseInt(localStorage.getItem(KEY), 10);
  if (saved >= MIN && saved <= MAX) {
    sidebar.style.width = saved + 'px';
    sidebar.style.minWidth = saved + 'px';
  }
  let dragging = false, startX = 0, startW = 0;
  splitter.addEventListener('pointerdown', (e) => {
    if (document.body.classList.contains('sidebar-collapsed')) return;
    dragging = true;
    startX = e.clientX;
    startW = sidebar.getBoundingClientRect().width;
    splitter.setPointerCapture(e.pointerId);
    splitter.classList.add('dragging');
    sidebar.style.transition = 'none';
    document.body.style.cursor = 'col-resize';
    document.body.style.userSelect = 'none';
  });
  splitter.addEventListener('pointermove', (e) => {
    if (!dragging) return;
    const w = Math.min(MAX, Math.max(MIN, startW + e.clientX - startX));
    sidebar.style.width = w + 'px';
    sidebar.style.minWidth = w + 'px';
  });
  const end = () => {
    if (!dragging) return;
    dragging = false;
    splitter.classList.remove('dragging');
    sidebar.style.transition = '';
    document.body.style.cursor = '';
    document.body.style.userSelect = '';
    const w = Math.round(parseFloat(sidebar.style.width));
    if (w >= MIN && w <= MAX) localStorage.setItem(KEY, w);
  };
  splitter.addEventListener('pointerup', end);
  splitter.addEventListener('pointercancel', end);
})();

// ---------- modals ----------
function openModal(sel) { $(sel).classList.remove('hidden'); }
function closeModal(el) { el.closest('.modal').classList.add('hidden'); }
document.querySelectorAll('[data-close]').forEach((b) => {
  b.onclick = () => closeModal(b);
});
document.querySelectorAll('.modal').forEach((m) => {
  m.addEventListener('click', (e) => { if (e.target === m) m.classList.add('hidden'); });
});

// ---------- 设置 modal（通知开关，存 localStorage，见上方通知模块） ----------
function notifyPermText() {
  if (!('Notification' in window)) return '当前浏览器不支持系统通知';
  if (Notification.permission === 'granted') return '浏览器已授权';
  if (Notification.permission === 'denied') return '已被浏览器拒绝，请在地址栏的站点设置中允许通知';
  return '未授权，打开开关时将请求浏览器权限';
}

$('#btn-settings').onclick = () => {
  const prefs = notifyPrefs();
  $('#pref-banner').checked = prefs.banner;
  $('#pref-sound').checked = prefs.sound;
  $('#notify-perm-hint').textContent = notifyPermText();
  openModal('#modal-settings');
};

$('#pref-banner').addEventListener('change', async (e) => {
  const prefs = notifyPrefs();
  prefs.banner = e.target.checked;
  saveNotifyPrefs(prefs);
  // 打开开关顺手请求权限（change 由点击触发，满足手势要求）
  if (prefs.banner && 'Notification' in window && Notification.permission === 'default') {
    try { await Notification.requestPermission(); } catch (_) {}
  }
  $('#notify-perm-hint').textContent = notifyPermText();
});

$('#pref-sound').addEventListener('change', (e) => {
  const prefs = notifyPrefs();
  prefs.sound = e.target.checked;
  saveNotifyPrefs(prefs);
  if (prefs.sound) playNotifySound(false); // 打开时试听一声，顺带解锁 AudioContext
});

$('#btn-test-sound').onclick = () => playNotifySound(false);

// ---------- 待办备忘（服务端持久化，重启不丢） ----------
// 全局便签：整列表 PUT /api/notes 落盘 data/notes.json。前端为权威源，
// 每次增删改后整表提交（ack 即可，不回填响应）；打开弹窗时 GET 一次回显。
// ID 由前端生成；保存串行化——在途请求期间的新改动合并为一次后续提交，
// 避免并发 PUT 对 notes.json 的 lost-update。
let notes = [];
let notesSaving = false;
let notesDirty = false;
let notesFlashTimer = null;

function newNoteId() {
  if (window.crypto && crypto.randomUUID) return crypto.randomUUID();
  return Date.now().toString(36) + Math.random().toString(36).slice(2);
}

$('#btn-notes').onclick = () => { openModal('#modal-notes'); loadNotes(); };

async function loadNotes() {
  try {
    notes = await api('/api/notes');
  } catch (e) {
    notes = [];
    notesFlash('加载失败：' + e.message);
  }
  renderNotes();
  $('#notes-new').focus();
}

function renderNotes() {
  const list = $('#notes-list');
  list.innerHTML = '';
  $('#notes-empty').classList.toggle('hidden', notes.length > 0);
  for (const n of notes) {
    const row = document.createElement('div');
    row.className = 'note-row' + (n.done ? ' done' : '');

    const check = document.createElement('button');
    check.className = 'note-check';
    check.title = n.done ? '标记为未完成' : '标记为已完成';
    check.innerHTML = n.done
      ? '<i data-lucide="circle-check"></i>'
      : '<i data-lucide="circle"></i>';
    check.onclick = () => { n.done = !n.done; saveNotes(); };

    const text = document.createElement('span');
    text.className = 'note-text';
    text.textContent = n.text;

    const del = document.createElement('button');
    del.className = 'note-del';
    del.title = '删除';
    del.innerHTML = '<i data-lucide="x"></i>';
    del.onclick = () => {
      notes = notes.filter(x => x.id !== n.id);
      saveNotes();
    };

    row.append(check, text, del);
    list.append(row);
  }
  renderIcons(list);
}

// saveNotes 先即时重渲染反映本地改动，再串行化提交：在途时仅置 dirty，
// 其 finally 触发一次携带最新状态的后续提交。
async function saveNotes() {
  renderNotes();
  if (notesSaving) { notesDirty = true; return; }
  notesSaving = true;
  try {
    await api('/api/notes', { method: 'PUT', body: JSON.stringify(notes) });
    notesFlash('已保存', true);
  } catch (e) {
    notesFlash('保存失败：' + e.message);
  } finally {
    notesSaving = false;
    if (notesDirty) { notesDirty = false; saveNotes(); }
  }
}

function notesFlash(msg, ok) {
  const el = $('#notes-status');
  el.textContent = msg;
  el.classList.toggle('ok', !!ok);
  el.classList.remove('hidden');
  clearTimeout(notesFlashTimer);
  notesFlashTimer = setTimeout(() => el.classList.add('hidden'), 1800);
}

function addNote() {
  const input = $('#notes-new');
  const text = input.value.trim();
  if (!text) return;
  notes.unshift({ id: newNoteId(), text, done: false });
  input.value = '';
  saveNotes();
  input.focus();
}

$('#notes-add').onclick = addNote;
$('#notes-new').addEventListener('keydown', (e) => {
  if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); addNote(); }
});
$('#notes-clear').onclick = () => {
  if (!notes.some(n => n.done)) return;
  notes = notes.filter(n => !n.done);
  saveNotes();
};

// ---------- 使用统计弹窗 ----------
// 数据来自 GET /api/stats。服务端增量聚合：仅重扫 mtime/size 变了的会话文件，
// 其余复用逐文件缓存再合并，打开弹窗通常只重扫当前活跃会话一个文件。
// 前端再叠加 stale-while-revalidate：有旧数据先秒渲染、后台静默刷新。
// 热力图口径移植自 cc-haha ActivitySettings：评分 score = 当日会话数*3 +
// ceil(tokens/50000)，按全期 max 分 5 档。
const HEAT_COLS = 52; // 周列数（GitHub 贡献图口径：一年）
const HEAT_CELL = 10; // 格子边长 px
const HEAT_GAP = 3;

// 每天的聚合数据（hover/click tooltip 用），renderHeatmap 时重建
let heatByDate = new Map();
let heatPinned = null; // 点击固定的日期
let lastStats = null; // 上次拉取的统计，打开弹窗时即时渲染（stale-while-revalidate）

function fmtTokens(n) {
  if (n >= 1e9) return (n / 1e9).toFixed(n >= 1e10 ? 0 : 1) + 'B';
  if (n >= 1e6) return (n / 1e6).toFixed(n >= 1e7 ? 0 : 1) + 'M';
  if (n >= 1e3) return Math.round(n / 1e3) + 'K';
  return String(n);
}

function fmtInt(n) { return Number(n).toLocaleString('zh-CN'); }

function dayKey(d) {
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`;
}

$('#btn-stats').onclick = () => { openModal('#modal-stats'); loadStats(false); };
$('#stats-refresh').onclick = () => loadStats(true);

async function loadStats(fresh) {
  const loading = $('#stats-loading');
  const body = $('#stats-body');
  const btn = $('#stats-refresh');
  const hasOld = !!lastStats;
  if (hasOld) {
    // 已有数据：先即时渲染旧数据，再后台静默刷新（fresh 整页重扫可能稍慢）
    loading.classList.add('hidden');
    body.classList.remove('hidden');
    renderStats(lastStats);
  } else {
    loading.textContent = '统计中…';
    loading.classList.remove('hidden');
    body.classList.add('hidden');
  }
  if (fresh) { btn.disabled = true; btn.textContent = '刷新中…'; }
  let stats;
  try {
    stats = await api('/api/stats' + (fresh ? '?fresh=1' : ''));
  } catch (e) {
    if (!hasOld) loading.textContent = '统计失败：' + e.message;
    if (fresh) { btn.disabled = false; btn.textContent = '刷新'; }
    return; // 有旧数据则保持显示，不弹错误
  }
  lastStats = stats;
  if (fresh) { btn.disabled = false; btn.textContent = '刷新'; }
  // 弹窗已关闭则不重渲染（避免无谓 DOM 操作）
  if (!$('#modal-stats').classList.contains('hidden')) {
    loading.classList.add('hidden');
    body.classList.remove('hidden');
    renderStats(stats);
  }
}

function renderStats(stats) {
  const daily = stats.daily_activity || [];
  const totalTokens = (stats.daily_model_tokens || []).reduce(
    (s, d) => s + Object.values(d.tokens_by_model).reduce((a, b) => a + b, 0), 0);
  const peakHour = stats.peak_activity_hour == null ? '—' : `${stats.peak_activity_hour}:00`;
  const tile = (v, l) => `<div class="stat-tile"><div class="v">${v}</div><div class="l">${l}</div></div>`;
  $('#stats-tiles').innerHTML = [
    tile(fmtInt(stats.total_sessions), '会话'),
    tile(fmtInt(stats.total_messages), '消息'),
    tile(fmtTokens(totalTokens), '总 tokens'),
    tile(`${fmtInt(stats.active_days)} / ${fmtInt(stats.total_days)}`, '活跃天数'),
    tile(`${fmtInt(stats.streaks.current)} 天`, '当前连续'),
    tile(`${fmtInt(stats.streaks.longest)} 天`, '最长连续'),
    tile(peakHour, '峰值时段'),
    tile(stats.favorite_model || '—', '常用模型'),
  ].join('');
  renderHeatmap(stats);
  $('#heat-detail').textContent = `工具调用共 ${fmtInt((daily).reduce((s, d) => s + d.tool_call_count, 0))} 次`;
}

function renderHeatmap(stats) {
  const act = new Map((stats.daily_activity || []).map((d) => [d.date, d]));
  const tok = new Map((stats.daily_model_tokens || []).map((d) => [
    d.date, Object.values(d.tokens_by_model).reduce((a, b) => a + b, 0)]));
  const today = new Date(); today.setHours(0, 0, 0, 0);
  const weekStart = new Date(today); weekStart.setDate(weekStart.getDate() - weekStart.getDay()); // 本周日
  const start = new Date(weekStart); start.setDate(start.getDate() - (HEAT_COLS - 1) * 7);
  const dates = [];
  for (let d = new Date(start); d <= today; d = new Date(d.getFullYear(), d.getMonth(), d.getDate() + 1)) {
    dates.push(new Date(d));
  }
  const scores = dates.map((d) => {
    const a = act.get(dayKey(d));
    if (!a && !tok.has(dayKey(d))) return 0;
    return ((a ? a.session_count : 0) * 3) + Math.ceil((tok.get(dayKey(d)) || 0) / 50000);
  });
  const maxScore = Math.max(0, ...scores);
  const level = (s) => s <= 0 ? 0 : maxScore <= 0 ? 1
    : s / maxScore >= 0.78 ? 4 : s / maxScore >= 0.5 ? 3 : s / maxScore >= 0.24 ? 2 : 1;
  // tooltip 数据：每天会话/消息/工具/tokens/模型构成
  const tokByModel = new Map((stats.daily_model_tokens || []).map((d) => [d.date, d.tokens_by_model]));
  heatByDate = new Map(dates.map((d) => {
    const key = dayKey(d);
    const a = act.get(key) || null;
    const tm = tokByModel.get(key);
    return [key, {
      a,
      tokens: tok.get(key) || 0,
      models: tm ? Object.entries(tm).sort((x, y) => y[1] - x[1]).slice(0, 3) : [],
    }];
  }));
  const cells = dates.map((d, i) =>
    `<div class="heat-cell lv${level(scores[i])}" data-date="${dayKey(d)}"></div>`).join('');
  $('#heatmap').innerHTML = `
    <div class="heat-days">
      <span class="heat-day-label" style="grid-row:2">周一</span>
      <span class="heat-day-label" style="grid-row:4">周三</span>
      <span class="heat-day-label" style="grid-row:6">周五</span>
    </div>
    <div class="heat-grid">${cells}</div>`;
  // 月份标签：每列（周）首日所在月份变化时标一个
  const labels = [];
  let prevMonth = -1;
  for (let col = 0; col < HEAT_COLS; col++) {
    const d = new Date(start.getFullYear(), start.getMonth(), start.getDate() + col * 7);
    if (d > today) break;
    if (d.getMonth() !== prevMonth) {
      labels.push(`<span class="heat-month-label" style="left:${col * (HEAT_CELL + HEAT_GAP)}px">${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}</span>`);
      prevMonth = d.getMonth();
    }
  }
  $('#heat-months').innerHTML = labels.join('');
  const l30 = dates.slice(-30).filter((d) => act.has(dayKey(d)) || tok.has(dayKey(d)));
  const heatDetail = $('#heat-detail');
  const base = heatDetail ? heatDetail.textContent : '';
  $('#heat-legend').innerHTML = `少 ${[0, 1, 2, 3, 4].map((i) => `<span class="heat-cell lv${i}"></span>`).join('')} 多`;
  if (heatDetail) {
    heatDetail.textContent = base ? base + ` · 近 30 天：${fmtInt(l30.length)} 天活跃，${fmtTokens(l30.reduce((s, d) => s + (tok.get(dayKey(d)) || 0), 0))} tokens` : '';
  }
}

// ---------- 热力图 tooltip：hover 跟随，点击固定（再点同一天取消） ----------
function heatTipHtml(date) {
  const d = heatByDate.get(date);
  const head = `<div class="ht-date">${date}</div>`;
  if (!d || (!d.a && d.tokens === 0)) return head + '<div class="ht-empty">这一天没有活动</div>';
  const rows = [];
  if (d.a) {
    rows.push(`<div class="ht-row"><span>会话</span><span class="v">${d.a.session_count}</span></div>`);
    rows.push(`<div class="ht-row"><span>消息</span><span class="v">${fmtInt(d.a.message_count)}</span></div>`);
    rows.push(`<div class="ht-row"><span>工具调用</span><span class="v">${fmtInt(d.a.tool_call_count)}</span></div>`);
  }
  rows.push(`<div class="ht-row"><span>tokens</span><span class="v">${fmtTokens(d.tokens)}</span></div>`);
  for (const [model, n] of d.models) {
    rows.push(`<div class="ht-row"><span class="ht-model" title="${escHtml(model)}">${escHtml(model)}</span><span class="v">${fmtTokens(n)}</span></div>`);
  }
  return head + rows.join('');
}

function showHeatTip(cell) {
  const tip = $('#heat-tooltip');
  const wrap = document.querySelector('.stats-heat');
  if (!tip || !wrap) return;
  tip.innerHTML = heatTipHtml(cell.dataset.date);
  tip.classList.remove('hidden');
  const w = wrap.getBoundingClientRect();
  const r = cell.getBoundingClientRect();
  const tw = tip.offsetWidth || 170;
  const th = tip.offsetHeight || 90;
  let left = r.left - w.left + 14;                        // 默认贴右侧
  if (left + tw > w.width - 4) left = r.left - w.left - tw - 6; // 越界翻到左侧
  let top = r.top - w.top - th - 6;                       // 默认放上方
  if (top < 2) top = r.top - w.top + 16;                  // 上方没空间放下方
  tip.style.left = Math.max(2, left) + 'px';
  tip.style.top = Math.max(2, top) + 'px';
}

function hideHeatTip() {
  heatPinned = null;
  $('#heat-tooltip').classList.add('hidden');
}

$('#heatmap').addEventListener('mouseover', (e) => {
  const cell = e.target.closest ? e.target.closest('.heat-cell') : null;
  if (!cell || heatPinned) return; // 点击固定后 hover 不打扰
  showHeatTip(cell);
});
$('#heatmap').addEventListener('mouseleave', () => {
  if (!heatPinned) $('#heat-tooltip').classList.add('hidden');
});
$('#heatmap').addEventListener('click', (e) => {
  const cell = e.target.closest ? e.target.closest('.heat-cell') : null;
  if (!cell) return;
  if (heatPinned === cell.dataset.date) { hideHeatTip(); return; }
  heatPinned = cell.dataset.date;
  showHeatTip(cell);
});

$('#btn-new').onclick = async () => {
  const firstCli = state.clis[0] && state.clis[0].name;
  fillCLISelect($('#new-cli'), firstCli);
  const k0 = cliKind(firstCli);
  fillProviderSelect($('#new-provider'), '', k0);
  fillModelSelect($('#new-model'), $('#new-provider').value, '');
  applyNewEngineUI(k0);
  const cfg = await api('/api/config');
  $('#new-cwd').placeholder = cfg.default_cwd || '/path/to/project';
  openModal('#modal-new');
};

// quick-create 弹窗：clis 列表本就带 kind 后缀（fillCLISelect 标签），切 cli
// 时 provider 归位同 kind 首个、隐藏推理等级（opencode 无此概念）
function applyNewEngineUI(kind) {
  $('#new-effort').closest('label').classList.toggle('hidden', kind === 'opencode');
}

$('#new-cli').addEventListener('change', () => {
  const kind = cliKind($('#new-cli').value);
  fillProviderSelect($('#new-provider'), '', kind);
  fillModelSelect($('#new-model'), $('#new-provider').value, '');
  applyNewEngineUI(kind);
});

$('#new-provider').addEventListener('change', (e) => {
  fillModelSelect($('#new-model'), e.target.value, '');
});

$('#new-submit').onclick = async () => {
  const body = {
    name: $('#new-name').value.trim(),
    cli: $('#new-cli').value,
    provider: $('#new-provider').value,
    cwd: $('#new-cwd').value.trim(),
    model: $('#new-model').value,
    effort: $('#new-effort').value,
    permission_mode: $('#new-perm').value,
  };
  try {
    const s = await api('/api/sessions', { method: 'POST', body: JSON.stringify(body) });
    $('#modal-new').classList.add('hidden');
    $('#new-name').value = ''; $('#new-cwd').value = '';
    await loadSessions();
    openSession(s.id);
  } catch (e) {
    alert('新建会话失败：' + e.message);
  }
};

// 两引擎共用一套列表/预览 UI，源由 #import-cli 的 kind 决定；顶部页签是
// 快捷切换（把 import-cli 指到该 kind 的第一个 cli 并重扫）。
const IMPORT_HINTS = {
  claude: 'SSH 里找 session id：<code>ls -t ~/.claude/projects/*/*.jsonl | head</code>（文件名 <code>~/.claude/projects/&lt;cwd-slug&gt;/&lt;session-id&gt;.jsonl</code>）',
  opencode: 'opencode 会话存于 opencode 自己的存储（<code>opencode session list</code>）。导入时会话按导出里的模型对齐到所选 provider。',
};

function syncImportTabs(kind) {
  $('#import-hint').innerHTML = IMPORT_HINTS[kind] || '';
  document.querySelectorAll('#import-tabs .import-tab').forEach((t) => {
    t.classList.toggle('active', t.dataset.kind === kind);
  });
}

$('#import-tabs').addEventListener('click', (e) => {
  const tab = e.target.closest('.import-tab');
  if (!tab || tab.classList.contains('hidden') || tab.classList.contains('active')) return;
  const cli = state.clis.find((c) => kindOf(c) === tab.dataset.kind);
  if (!cli) return;
  $('#import-cli').value = cli.name;
  $('#import-cli').dispatchEvent(new Event('change'));
});

$('#btn-import').onclick = () => {
  // 页签只显示有对应 kind cli 的引擎
  document.querySelectorAll('#import-tabs .import-tab').forEach((t) => {
    const has = state.clis.some((c) => kindOf(c) === t.dataset.kind);
    t.classList.toggle('hidden', !has);
  });
  const cli = state.clis[0];
  fillCLISelect($('#import-cli'), cli && cli.name);
  fillProviderSelect($('#import-provider'), '', cli && kindOf(cli));
  state.importList = [];
  $('#import-cwd').value = '';
  $('#import-prefix').value = '';
  syncImportTabs(cli && kindOf(cli));
  openModal('#modal-import');
  scanImport();
};

$('#import-scan').onclick = scanImport;
$('#import-prefix').addEventListener('input', renderImportList);
$('#import-cwd').addEventListener('input', renderImportList);
// 切 cli（含页签驱动）→ provider 归位到同 kind 首个 + 重扫
$('#import-cli').addEventListener('change', () => {
  const kind = cliKind($('#import-cli').value);
  syncImportTabs(kind);
  fillProviderSelect($('#import-provider'), '', kind);
  scanImport();
});

async function scanImport() {
  const el = $('#import-list');
  el.innerHTML = '<div style="color:var(--dim);padding:20px;text-align:center">扫描中...</div>';
  const cliSel = $('#import-cli');
  const kind = cliKind(cliSel.value);
  state.importKind = kind;
  const cwd = $('#import-cwd').value.trim();
  const prefix = $('#import-prefix').value.trim();
  const params = new URLSearchParams();
  if (kind === 'opencode') {
    // opencode 会话源在 opencode 自己的存储，按 cli 查询
    params.set('cli', cliSel.value);
  }
  if (cwd) params.set('cwd', cwd);
  if (prefix) params.set('prefix', prefix);
  const url = (kind === 'opencode' ? '/api/opencode-sessions' : '/api/claude-sessions')
    + '?' + params.toString();
  try {
    state.importList = await api(url);
  } catch (e) {
    el.innerHTML = '<div style="color:var(--red);padding:20px">' + e.message + '</div>';
    return;
  }
  renderImportList();
}

function renderImportList() {
  const cwd = $('#import-cwd').value.trim();
  const prefix = $('#import-prefix').value.trim().toLowerCase();
  // 客户端二次筛选（扫描时 cwd 已传给服务端，但 prefix 实时生效更顺手）
  let list = state.importList || [];
  if (cwd && list.length && !list[0].cwd) { /* 服务端 cwd 已过滤；这里不重复 */ }
  if (prefix) {
    list = list.filter((it) => it.session_id.toLowerCase().startsWith(prefix));
  }
  const el = $('#import-list');
  el.innerHTML = '';
  if (!list.length) {
    el.innerHTML = '<div style="color:var(--dim);padding:20px;text-align:center">没有匹配的会话</div>';
    return;
  }
  for (const item of list) {
    const row = document.createElement('div');
    row.className = 'import-item';
    row.innerHTML = `
      <div class="info">
        <div class="t"></div>
        <div class="m"></div>
      </div>
      <button class="ghost-btn copy-id" title="复制完整 session id"><i data-lucide="copy"></i></button>
      <button class="primary-btn import-btn">导入</button>`;
    row.querySelector('.t').textContent = item.title || '(untitled)';
    row.querySelector('.m').textContent = `${item.session_id.slice(0, 8)} · ${item.cwd || '?'} · ${timeAgo(item.updated_at)}`;
    row.onclick = (e) => {
      if (e.target.closest('button')) return;
      selectImportPreview(item, row);
    };
    row.querySelector('.copy-id').onclick = async (e) => {
      const btn = e.target.closest('button') || e.target;
      try {
        await copyText(item.session_id);
        flashCheck(btn, btn.innerHTML);
      } catch (_) {
        prompt('复制失败，请手动复制：', item.session_id);
      }
    };
    row.querySelector('.import-btn').onclick = async () => {
      try {
        const s = await api('/api/sessions/import', {
          method: 'POST',
          body: JSON.stringify({
            claude_session_id: item.session_id,
            cwd: item.cwd || cwd,
            cli: $('#import-cli').value,
            provider: $('#import-provider').value,
            name: item.title.slice(0, 60),
          }),
        });
        $('#modal-import').classList.add('hidden');
        await loadSessions();
        openSession(s.id);
      } catch (e) {
        alert('导入失败：' + e.message);
      }
    };
    el.appendChild(row);
  }
  renderIcons(el);
}

// ---------- 导入预览（右侧面板）：选中列表行后拉取该会话最近消息 ----------
const importPreviewCache = new Map(); // key: session_id|updated_at（文件 mtime 变化即过期）

async function selectImportPreview(item, row) {
  document.querySelectorAll('.import-item.sel').forEach((el) => el.classList.remove('sel'));
  row.classList.add('sel');
  const pane = $('#import-preview');
  const cacheKey = item.session_id + '|' + item.updated_at;
  let p = importPreviewCache.get(cacheKey);
  if (!p) {
    pane.innerHTML = '<div class="ip-empty">加载中…</div>';
    const params = new URLSearchParams({ id: item.session_id });
    if (state.importKind === 'opencode') {
      params.set('cli', $('#import-cli').value);
    } else if (item.cwd) {
      params.set('cwd', item.cwd);
    }
    try {
      p = await api('/api/' + (state.importKind === 'opencode' ? 'opencode-sessions' : 'claude-sessions') + '/preview?' + params.toString());
    } catch (e) {
      pane.innerHTML = '<div class="ip-empty">加载失败：' + e.message + '</div>';
      return;
    }
    importPreviewCache.set(cacheKey, p);
    if (importPreviewCache.size > 50) importPreviewCache.delete(importPreviewCache.keys().next().value);
  }
  pane.innerHTML = renderImportPreview(p, item.title);
  renderIcons(pane);
}

function escHtml(s) {
  return String(s).replace(/[&<>"']/g, (c) => (
    { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]
  ));
}

function renderImportPreview(p, title) {
  const msgs = (p.messages || []).map((m) => `
    <div class="ip-msg">
      <div class="ip-msg-head">
        <span class="role-badge ${m.role === 'user' ? 'user' : 'asst'}">${m.role === 'user' ? '用户' : '助手'}</span>
        <span class="ip-msg-ts">${m.ts ? formatMsgTime(m.ts).slice(0, 16) : ''}</span>
      </div>
      <div class="ip-msg-text">${escHtml(m.text)}</div>
    </div>`).join('');
  return `
    <div class="ip-title">${escHtml(title || '(empty)')}</div>
    <div class="ip-meta">
      <span class="ip-cwd" title="${escHtml(p.cwd || '')}">${escHtml(p.cwd || '?')}</span>
      ${p.branch ? `<span class="ip-branch"><i data-lucide="git-branch"></i>${escHtml(p.branch)}</span>` : ''}
      <span class="ip-active">最后活跃 ${timeAgo(p.updated_at)}</span>
    </div>
    <div class="ip-where">离开时</div>
    ${msgs || '<div class="ip-empty">（无消息）</div>'}`;
}

// ---------- new-session composer view ----------
let nsPendingCwd = '';

async function openNewSessionView(cwd) {
  nsPendingCwd = cwd || '';
  $('#chat-view').classList.add('hidden');
  $('#empty-state').classList.add('hidden');
  $('#new-session-view').classList.remove('hidden');
  $('#ns-cwd-display').textContent = cwd ? `工作区: ${cwd}` : '';
  const inp = $('#ns-input');
  inp.value = '';
  inp.style.height = 'auto';
  state.nsAttached = [];
  renderNsAttached();
  inp.focus();

  const cliSel = $('#ns-cli');
  const providerSel = $('#ns-provider');
  const modelSel = $('#ns-model');
  cliSel.innerHTML = state.clis.map((c) => `<option value="${c.name}">${c.name}${kindOf(c) === 'claude' ? '' : ' · ' + c.kind}</option>`).join('');
  if (state.clis.length) cliSel.value = state.clis[0].name;
  const k0 = cliKind(cliSel.value);
  renderProviderOptions(providerSel, k0);
  renderModelOptions(modelSel, providerSel.value);
  applyNsEngineUI(k0);
  // cli 决定引擎 → provider 列表、推理等级控件、已附图片随之联动
  cliSel.onchange = () => {
    const kind = cliKind(cliSel.value);
    renderProviderOptions(providerSel, kind);
    renderModelOptions(modelSel, providerSel.value);
    applyNsEngineUI(kind);
    if (kind === 'opencode') dropNsImages('已切换到 opencode 引擎，暂不支持图片消息，已移除图片附件');
  };
  providerSel.onchange = () => renderModelOptions(modelSel, providerSel.value);
}

function renderProviderOptions(sel, kind) {
  const items = kind ? state.providers.filter((p) => kindOf(p) === kind) : state.providers;
  sel.innerHTML = items.map((p) => `<option value="${p.name}">${p.name}</option>`).join('');
  if (items.length) sel.value = items[0].name;
}

// 新建会话视图的引擎可见面：opencode 无推理等级概念，隐藏控件
function applyNsEngineUI(kind) {
  $('#ns-effort').classList.toggle('hidden', kind === 'opencode');
}

function dropNsImages(msg) {
  if (!(state.nsAttached || []).some((a) => a.kind === 'image')) return;
  state.nsAttached = state.nsAttached.filter((a) => a.kind !== 'image');
  renderNsAttached();
  alert(msg);
}
function renderModelOptions(sel, providerName) {
  const p = state.providers.find((x) => x.name === providerName);
  sel.innerHTML = (p ? p.models : []).map((m) => `<option value="${m.name}">${m.name}</option>`).join('');
  if (p && p.models.length) sel.value = p.models[0].name;
}

function renderNsAttached() {
  const box = $('#ns-attached');
  box.innerHTML = '';
  box.classList.toggle('hidden', !state.nsAttached || state.nsAttached.length === 0);
  (state.nsAttached || []).forEach((att, idx) => {
    const t = document.createElement('div');
    if (att.kind === 'file') {
      t.className = 'attached-thumb attached-file';
      t.innerHTML = `<i data-lucide="file" class="af-icon"></i><span class="af-name"></span><span class="af-size"></span><button class="remove" title="移除">×</button>`;
      t.querySelector('.af-name').textContent = att.name;
      t.querySelector('.af-name').title = att.name;
      t.querySelector('.af-size').textContent = fmtBytes(att.size);
    } else {
      t.className = 'thumb';
      t.innerHTML = `<img src="${att.data}"><button class="remove">×</button>`;
    }
    t.querySelector('.remove').onclick = () => {
      state.nsAttached.splice(idx, 1);
      renderNsAttached();
    };
    box.appendChild(t);
  });
  renderIcons(box);
}

async function nsSend() {
  const content = $('#ns-input').value.trim();
  if (!content && !(state.nsAttached && state.nsAttached.length)) return;
  const cli = $('#ns-cli').value;
  const provider = $('#ns-provider').value;
  const model = $('#ns-model').value;
  const effort = $('#ns-effort').value;
  const perm = $('#ns-perm').value;
  if (!cli || !provider || !model) { alert('请先选择配置'); return; }

  // Backend runner.Image expects {media_type, data} with data being raw
  // base64 (no "data:...;base64," prefix). nsAttached keeps the full dataURL
  // for thumbnails, so normalize here.
  const images = (state.nsAttached || []).filter((a) => a.kind === 'image').map((img) => ({
    media_type: img.mime,
    data: stripDataURLPrefix(img.data),
  }));
  if (images.length && cliKind(cli) === 'opencode') {
    alert('opencode 引擎暂不支持图片消息（文件附件可用）');
    return;
  }
  const nAttach = state.nsAttached ? state.nsAttached.length : 0;
  const name = content
    ? (content.length > 30 ? content.slice(0, 30) + '…' : content)
    : (nAttach ? (images.length ? '[图片]' : '[附件]') : '新会话');
  const sess = await api('/api/sessions', {
    method: 'POST',
    body: JSON.stringify({
      name, cli, provider, model, cwd: nsPendingCwd,
      effort: effort || undefined,
      permission_mode: perm || undefined,
    }),
  });
  $('#new-session-view').classList.add('hidden');
  const nsFiles = [...(state.nsAttached || [])];
  state.nsAttached = [];
  // 文件附件：会话建成后才拿到 sid，此时才上传到 data/uploads/<sid>/。
  // 失败则停在当前会话，用户可在聊天里补传。
  const files = [];
  for (const a of nsFiles) {
    if (a.kind !== 'file') continue;
    try {
      const up = await uploadAttachment(a.file, sess.id);
      files.push({ name: up.name, path: up.path });
    } catch (e) {
      const banner = $('#error-banner');
      banner.textContent = `上传失败：${a.name}（${e.message || 'unknown error'}）`;
      banner.classList.remove('hidden');
      files.length = 0; // 半截文件不发给模型，整条附件作废
      break;
    }
  }
  await loadSessions();
  await openSession(sess.id);
  // Optimistic bubble so the user sees their message immediately even if SSE
  // hasn't connected yet (race between EventSource open + first event push).
  appendOptimisticUser(content, images, files);
  scrollBottom();
  await api(`/api/sessions/${sess.id}/messages`, {
    method: 'POST',
    body: JSON.stringify({ text: content, images, files }),
  });
  updateStatusUI({ running: true, queued: 0 });
}

$('#ns-send').onclick = nsSend;
$('#ns-input').onkeydown = (e) => {
  if (e.key === 'Enter' && !e.shiftKey) {
    e.preventDefault();
    nsSend();
  }
};
// textarea auto-grow
$('#ns-input').addEventListener('focus', () => { slashTextarea = nsInputEl; slashPopup = nsSlashPopupEl; });
$('#ns-input').addEventListener('input', onInputChange);
$('#ns-input').addEventListener('blur', () => { setTimeout(closeSlashPopup, 200); });
$('#ns-input').onkeydown = (e) => {
  if (slashState.open) {
    if (e.key === 'ArrowDown') { e.preventDefault(); moveSlashActive(1); return; }
    if (e.key === 'ArrowUp') { e.preventDefault(); moveSlashActive(-1); return; }
    if (e.key === 'Tab' || (e.key === 'Enter' && !e.shiftKey)) {
      e.preventDefault(); selectSlashItem(); return;
    }
    if (e.key === 'Escape') { e.preventDefault(); closeSlashPopup(); return; }
  }
  if (e.key === 'Enter' && !e.shiftKey) {
    e.preventDefault();
    nsSend();
  }
};
$('#ns-attach').onclick = () => $('#ns-attach-file').click();
$('#ns-attach-file').onchange = async (e) => {
  await nsAttachFiles([...e.target.files]);
  e.target.value = '';
};

// 与主 composer 不同的地方：文件在会话建成那一刻才上传（先要拿到 sid），
// 这里先把 File 对象留在内存里，nsSend 里统一上传。
async function nsAttachFiles(files) {
  for (const f of files) {
    if (!nsAttachOk(f)) continue;
    state.nsAttached = state.nsAttached || [];
    const ocFile = cliKind($('#ns-cli').value) === 'opencode';
    if (ALLOWED_MIME.includes(f.type) && ocFile) {
      // opencode 引擎没有内嵌图片通道：图片按文件附件走（`opencode run -f`，
      // 视觉模型可直接看到）。会话建成后 nsSend 统一上传为 path。
      state.nsAttached.push({ kind: 'file', name: f.name, size: f.size, file: f });
      continue;
    }
    if (ALLOWED_MIME.includes(f.type)) {
      const data = await new Promise((res) => {
        const r = new FileReader();
        r.onload = () => res(r.result);
        r.readAsDataURL(f);
      });
      state.nsAttached.push({ kind: 'image', data, mime: f.type });
    } else {
      state.nsAttached.push({ kind: 'file', name: f.name, size: f.size, file: f });
    }
  }
  renderNsAttached();
}

function nsAttachOk(f) {
  const atts = state.nsAttached || [];
  // opencode 引擎的图片按文件附件计（见 nsAttachFiles）
  const asFile = ALLOWED_MIME.includes(f.type) && cliKind($('#ns-cli').value) === 'opencode';
  if (!ALLOWED_MIME.includes(f.type) || asFile) {
    if (atts.filter((a) => a.kind === 'file').length >= MAX_FILES) {
      alert(`最多 ${MAX_FILES} 个文件附件`);
      return false;
    }
    if (f.size > MAX_FILE_BYTES) {
      alert(`文件过大：${f.name}（${fmtMiB(f.size)}MB > ${MAX_FILE_BYTES >> 20}MB）`);
      return false;
    }
  } else {
    if (atts.filter((a) => a.kind === 'image').length >= MAX_IMAGES) {
      alert(`最多 ${MAX_IMAGES} 张图片`);
      return false;
    }
    if (f.size > MAX_IMAGE_BYTES) {
      alert(`图片过大：${f.name}（${(f.size / 1024 / 1024).toFixed(1)}MB > ${MAX_IMAGE_BYTES / 1024 / 1024}MB）`);
      return false;
    }
  }
  return true;
}
// paste 支持图片与文件
$('#ns-input').addEventListener('paste', async (e) => {
  const items = e.clipboardData && e.clipboardData.items;
  if (!items) return;
  const files = [];
  for (const it of items) {
    if (it.kind === 'file') {
      const f = it.getAsFile();
      if (f) files.push(f);
    }
  }
  if (files.length) {
    e.preventDefault();
    await nsAttachFiles(files);
  }
});

// ---------- mobile: drawer nav + bottom sheet ----------
const isMobile = () => window.matchMedia('(max-width: 640px)').matches;

function openDrawer() {
  document.body.classList.add('drawer-open');
  $('#drawer-overlay').classList.remove('hidden');
  renderIcons();
}
function closeDrawer() {
  document.body.classList.remove('drawer-open');
  $('#drawer-overlay').classList.add('hidden');
}

function syncSheetFromCurrent() {
  if (!state.current) return;
  const engine = state.current.engine || 'claude';
  fillCLISelect($('#sheet-cli'), state.current.cli, engine);
  fillProviderSelect($('#sheet-provider'), state.current.provider, engine);
  fillModelSelect($('#sheet-model'), state.current.provider, state.current.model_override);
  $('#sheet-effort').value = state.current.effort_override || '';
  $('#sheet-perm').value = state.current.permission_mode || '';
  const cwd = state.current.cwd || '';
  $('#sheet-cwd').textContent = cwd;
  $('#sheet-cwd').title = cwd;
  renderIcons();
}

function openSheet() {
  if (!state.current) return;
  syncSheetFromCurrent();
  document.body.classList.add('sheet-open');
  $('#chat-sheet').classList.remove('hidden');
  renderIcons();
}
function closeSheet() {
  document.body.classList.remove('sheet-open');
  $('#chat-sheet').classList.add('hidden');
}

$('#btn-drawer').onclick = openDrawer;
$('#btn-drawer-standalone').onclick = openDrawer;
$('#btn-chat-menu').onclick = () => {
  if (document.body.classList.contains('sheet-open')) closeSheet();
  else openSheet();
};
$('#drawer-overlay').onclick = closeDrawer;

// click outside the bottom sheet closes it
document.addEventListener('pointerdown', (e) => {
  if (!document.body.classList.contains('sheet-open')) return;
  if (!e.target.closest('#chat-sheet')) closeSheet();
});

document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape') {
    if (!$('#file-viewer').classList.contains('hidden')) closeFileViewer();
    else if (document.body.classList.contains('sheet-open')) closeSheet();
    else if (document.body.classList.contains('drawer-open')) closeDrawer();
  }
});

// sheet 里的选择要先同步到主下拉再派发 change（主处理器读的是 e.target.value）
const syncSheetSelect = (sheetSel, mainSel) => {
  sheetSel.onchange = (e) => {
    mainSel.value = e.target.value;
    mainSel.dispatchEvent(new Event('change'));
  };
};
syncSheetSelect($('#sheet-cli'), $('#session-cli'));
syncSheetSelect($('#sheet-provider'), $('#session-provider'));
syncSheetSelect($('#sheet-model'), $('#session-model'));
syncSheetSelect($('#sheet-effort'), $('#session-effort'));
syncSheetSelect($('#sheet-perm'), $('#session-perm'));

$('#sheet-resume').onclick = () => { closeSheet(); $('#btn-resume').click(); };
$('#sheet-delete').onclick = () => { closeSheet(); $('#btn-delete').click(); };
$('#sheet-archive').onclick = async () => {
  if (!state.current) return;
  const id = state.current.id;
  const archived = !state.current.archived;
  await api(`/api/sessions/${id}`, { method: 'PATCH', body: JSON.stringify({ archived }) });
  closeSheet();
  loadSessions();
};

// 主下拉 change 处理器在 PATCH 成功后调用，把最新会话状态回填到 sheet
// （重建选项列表，保证 provider 切换后 model 列表联动刷新）
function patchSheetIfOpen(s) {
  if (!document.body.classList.contains('sheet-open')) return;
  const engine = s.engine || 'claude';
  fillCLISelect($('#sheet-cli'), s.cli, engine);
  fillProviderSelect($('#sheet-provider'), s.provider, engine);
  fillModelSelect($('#sheet-model'), s.provider, s.model_override);
  $('#sheet-effort').value = s.effort_override || '';
  $('#sheet-perm').value = s.permission_mode || '';
}

// When the user picks a session in the list while the drawer is open,
// close the drawer so they see the chat.
const _origOpenSession = openSession;
openSession = async function(id) {
  await _origOpenSession(id);
  if (isMobile()) closeDrawer();
};

// Mobile Enter behavior: on a touch device, soft keyboards usually lack
// Shift+Enter. Switch the placeholder/hint so the user knows Enter sends.
function updateMobileInputHint() {
  const el = $('#input');
  if (!el) return;
  el.placeholder = isMobile() ? '发消息…' : '输入消息，Enter 发送，Shift+Enter 换行';
}
window.addEventListener('resize', updateMobileInputHint);
document.addEventListener('visibilitychange', () => {
  if (document.visibilityState === 'visible') {
    loadSessions().catch(() => {});
    refreshProfiles();
    if (state.current) refreshCurrentStatus();
  }
});


// ---------- PWA ----------
if ('serviceWorker' in navigator) {
  window.addEventListener('load', () => {
    navigator.serviceWorker.register('/sw.js').catch(() => {});
  });
}

(async function boot() {
  await api('/api/me'); // redirects to login when unauthorized
  applyTheme(document.documentElement.dataset.theme || 'light', false);
  state.clis = await api('/api/clis');
  state.providers = await api('/api/providers');
  state.profilesSig = JSON.stringify([state.clis, state.providers]);
  const cfg = await api('/api/config');
  state.home = cfg.home || '';
  await loadSessions();
  renderIcons();
  updateMobileInputHint();
  setInterval(() => {
    loadSessions().catch(() => {});
    refreshProfiles(); // config 热加载的轮询兜底（无 SSE 连接或广播丢失时）
  }, 5000);
})();
