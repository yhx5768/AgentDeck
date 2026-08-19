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
  return tpl.innerHTML;
};

const $ = (sel) => document.querySelector(sel);
const state = {
  clis: [],
  providers: [],
  sessions: [],
  archived: [],
  current: null,      // current session object
  es: null,
  seenSeq: new Set(), // seqs already rendered on this connection
  toolBlocks: {},     // tool_use_id -> details element
  nearBottom: true,
  home: '',           // server home dir, for ~ path shortening
  drafts: {},         // sessionId -> {text, images}
  attached: [],       // images staged for the next send
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
  stats: null,         // {turns, steps, firstTs, lastTs} for the current session
  live: null,          // live partial-stream state: {msgId, blocks:[{kind,el,bodyEl,buf,done,taken,raf}]}
};

// 切换会话时首屏只渲染最新 N 条事件，向上滚动点「加载更多」再翻页
const HISTORY_PAGE = 50;

// 收集当前 session 的待发送内容
function draft() {
  if (!state.current) return { text: '', images: [] };
  const id = state.current.id;
  if (!state.drafts[id]) state.drafts[id] = { text: '', images: [] };
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
    'alert-triangle', 'file-text', 'file-plus', 'terminal-square',
    'rotate-ccw', 'pencil', 'search',
    'more-horizontal', 'archive', 'archive-restore', 'git-fork',
    'settings-2', 'arrow-up',
    'menu', 'more-vertical', 'send-horizontal', 'folder',
    'panel-left-close', 'panel-left-open',
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

// ---------- sessions list ----------
async function loadSessions() {
  const [active2, archived] = await Promise.all([
    api('/api/sessions'),
    api('/api/sessions?archived=true').catch(() => []),
  ]);
  state.sessions = active2;
  state.archived = archived;
  renderSessionList();
  renderArchiveList();
}

function statusDot(s) {
  if (s.status.running) return 'dot running';
  if (s.status.queued > 0) return 'dot queued';
  if (s.status.last_error && s.status.last_error !== 'interrupted') return 'dot error';
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
  return `
    <button class="icon-btn menu-btn" title="更多操作" data-menu-for="${s.id}"><i data-lucide="more-horizontal"></i></button>
    <div class="name"><span class="${statusDot(s)}"></span><span class="name-text"></span></div>
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
function renderSessionList() {
  const el = $('#session-list');
  el.innerHTML = '';

  // search filter
  const q = state.searchQuery.trim().toLowerCase();
  let list = state.sessions;
  if (q) list = list.filter((s) => (s.name || '').toLowerCase().includes(q));

  // sort
  if (state.viewSort === 'manual') {
    // manual: by sort_order desc, fallback to updated_at
    list = [...list].sort((a, b) => {
      const ao = a.sort_order || 0, bo = b.sort_order || 0;
      if (ao !== bo) return bo - ao;
      return new Date(b.updated_at) - new Date(a.updated_at);
    });
  } // else: default order from server (updated_at desc)

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

$('#rename-submit').onclick = async () => {
  const name = $('#rename-input').value.trim();
  if (!name) return;
  $('#modal-rename').classList.add('hidden');
  if (renameTarget) {
    await api(`/api/sessions/${renameTarget.id}`, {
      method: 'PATCH', body: JSON.stringify({ name }),
    });
    renameTarget.name = name;
    renameTarget = null;
    loadSessions();
  }
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

// ---------- current session ----------
async function openSession(id) {
  const s = await api(`/api/sessions/${id}`);
  state.current = s;
  state.seenSeq = new Set();
  state.toolBlocks = {};
  state.historyBuf = [];
  state.histRenderedFrom = 0;
  state.replaying = true;
  state.live = null;
  state.stats = { turns: 0, steps: 0, firstTs: 0, lastTs: 0, llmMs: 0, toolMs: 0, llmEstMs: 0, toolEstMs: 0, prevTs: 0 };
  updateStatsUI();
  $('#empty-state').classList.add('hidden');
  $('#new-session-view').classList.add('hidden');
  $('#chat-view').classList.remove('hidden');
  $('#messages').innerHTML = '';
  $('#error-banner').classList.add('hidden');
  $('#session-name').value = s.name;
  $('#session-cwd').textContent = s.cwd;
  $('#session-cwd').title = s.cwd;
  $('#session-perm').value = s.permission_mode || '';
  fillCLISelect($('#session-cli'), s.cli);
  fillProviderSelect($('#session-provider'), s.provider);
  fillModelSelect($('#session-model'), s.provider, s.model_override);
  $('#session-effort').value = s.effort_override || '';
  // Restore draft (text + images) for this session.
  state.attached = (state.drafts[id] && state.drafts[id].images) || [];
  $('#input').value = (state.drafts[id] && state.drafts[id].text) || '';
  input.style.height = 'auto';
  input.style.height = Math.min(input.scrollHeight, 180) + 'px';
  updateStatusUI(s.status);
  applyWorkspaceGate(s.cwd);
  applyAttachGate();
  renderAttachedThumbs();
  renderSessionList();
  connectEvents(id);
}

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

// 根据当前 model 的 input 能力控制附件按钮可用性
function applyAttachGate() {
  if (!state.current) return;
  const prov = state.providers.find((p) => p.name === state.current.provider);
  const m = prov && prov.models.find((x) => x.name === state.current.model_override);
  const allow = !!(prov && m && (m.input || ['text']).includes('image'));
  const btn = $('#btn-attach');
  btn.disabled = !allow || $('#input').disabled;
  btn.title = allow
    ? '添加图片（可粘贴/拖拽）'
    : '当前模型未声明 image 能力；如需附件请在模型上加 input: [text, image]';
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

function fillCLISelect(sel, value) {
  fillSelect(sel, state.clis, value);
}

function fillProviderSelect(sel, value) {
  fillSelect(sel, state.providers, value);
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

function connectEvents(id) {
  if (state.es) state.es.close();
  const es = new EventSource(`/api/sessions/${id}/events`);
  state.es = es;
  es.onmessage = (e) => {
    const ev = JSON.parse(e.data);
    if (state.seenSeq.has(ev.seq)) return;
    state.seenSeq.add(ev.seq);
    if (ev.kind === 'replay_end') {
      flushHistory();
      return;
    }
    trackStats(ev);
    // Buffer replayed history; render only the latest page when the
    // replay_end marker arrives. Live events render immediately.
    if (state.replaying) {
      state.historyBuf.push(ev);
      return;
    }
    renderEvent(ev);
    scrollBottom();
    updateStatsUI();
  };
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
  for (let i = state.histRenderedFrom; i < buf.length; i++) {
    renderEvent(buf[i], box);
  }
  for (const node of optimistic) box.appendChild(node);
  state.nearBottom = true;
  scrollBottom();
  updateLoadMoreBtn();
  updateStatsUI();
}

// Prepend the previous page of buffered history, keeping the viewport
// anchored on the message the user was looking at.
function loadMoreHistory() {
  if (state.histRenderedFrom <= 0) return;
  const box = $('#messages');
  const from = Math.max(0, state.histRenderedFrom - HISTORY_PAGE);
  const frag = document.createDocumentFragment();
  for (let i = from; i < state.histRenderedFrom; i++) {
    renderEvent(state.historyBuf[i], frag);
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
  if (st.running) {
    badge.textContent = '● 运行中';
    badge.className = 'status-badge running';
    stopBtn.classList.remove('hidden');
  } else {
    badge.textContent = '';
    badge.className = 'status-badge';
    stopBtn.classList.add('hidden');
  }
  hint.textContent = st.queued > 0 ? `队列中还有 ${st.queued} 条消息` : '';
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

function addMsg(cls, roleTag, box) {
  box = box || $('#messages');
  const div = document.createElement('div');
  div.className = `msg ${cls}`;
  div.innerHTML = `<div class="role-tag">${roleTag}</div><div class="bubble"></div>`;
  box.appendChild(div);
  return div.querySelector('.bubble');
}

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
  const keys = Object.keys(input);
  return keys.length ? JSON.stringify(input).slice(0, 120) : '';
}

function renderToolUse(block, box) {
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
  wrapInMsg(details, box);
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

function renderToolResult(block, box) {
  const text = toolResultText(block.content);
  const target = block.tool_use_id && state.toolBlocks[block.tool_use_id];
  if (target) {
    // TodoWrite 的结果固定是 "Todos have been modified…"，无信息量，不展示
    if (target.classList.contains('tool-todo')) return;
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
  wrapInMsg(details, box);
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
$('#fv-close').onclick = closeFileViewer;
$('#fv-copy').onclick = async (e) => {
  const btn = e.currentTarget;
  try {
    await copyText($('#fv-path').textContent);
    flashCheck(btn, btn.innerHTML);
  } catch (_) {}
};

function wrapInMsg(node, box) {
  // Attach to the last assistant message if present, else create one.
  const msgs = box || $('#messages');
  const last = msgs.lastElementChild;
  if (last && last.classList.contains('msg-assistant')) {
    last.querySelector('.bubble').appendChild(node);
  } else {
    addMsg('msg-assistant', 'claude', msgs).appendChild(node);
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

function renderStreamEvent(d, box) {
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
        const bubble = addMsg('msg-assistant', 'claude', box);
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
        wrapInMsg(details, box);
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
  if (ev.kind === 'user') {
    // Replace any pending optimistic bubble with the authoritative one
    // (which contains only text — images are not stored in events).
    const last = box.lastElementChild;
    if (last && last.classList.contains('msg-optimistic')) last.remove();
    const bubble = addMsg('msg-user', '你', box);
    bubble.textContent = d.text;
  } else if (ev.kind === 'stream') {
    if (d.type === 'system') {
      if (d.subtype === 'init') {
        const line = document.createElement('div');
        line.className = 'meta-line';
        line.textContent = `session ${String(d.session_id || '').slice(0, 8)} · model ${d.model || '?'} · ${d.cwd || ''}`;
        box.appendChild(line);
      }
      return; // ignore thinking_tokens and other system noise
    }
    if (d.type === 'assistant' && d.message && Array.isArray(d.message.content)) {
      for (const block of d.message.content) {
        if (block.type === 'text' && block.text) {
          const lb = liveTake('text');
          if (lb) lb.el.remove(); // 完整版替换 live 气泡，避免重复
          const bubble = addMsg('msg-assistant', 'claude', box);
          bubble.innerHTML = md(block.text);
        } else if (block.type === 'tool_use') {
          renderToolUse(block, box);
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
            wrapInMsg(details, box);
          } else if (lb) {
            liveFinalizeBlock(lb); // 完整版被后端 strip 成空，保留 live 版
          }
        }
      }
    } else if (d.type === 'stream_event') {
      renderStreamEvent(d, box);
    } else if (d.type === 'user' && d.message && Array.isArray(d.message.content)) {
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
      refreshCurrentStatus();
    } else if (d.state === 'error') {
      liveFinalizeAll();
      updateStatusUI({ running: false, queued: 0 });
      const banner = $('#error-banner');
      banner.textContent = d.error || 'unknown error';
      banner.classList.remove('hidden');
      refreshCurrentStatus();
    }
  }
}

async function refreshCurrentStatus() {
  if (!state.current) return;
  try {
    const s = await api(`/api/sessions/${state.current.id}`);
    state.current = s;
    updateStatusUI(s.status);
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
function syncComposerSpace() {
  const gap = parseFloat(getComputedStyle(composerEl).bottom) || 0;
  messagesEl.style.paddingBottom = `${composerEl.offsetHeight + gap + 8}px`;
}
new ResizeObserver(() => {
  syncComposerSpace();
  // Padding grows downward; keep the view pinned if the user was at the bottom.
  if (state.nearBottom) messagesEl.scrollTop = messagesEl.scrollHeight;
}).observe(composerEl);
syncComposerSpace();

input.addEventListener('input', () => {
  input.style.height = 'auto';
  input.style.height = Math.min(input.scrollHeight, 180) + 'px';
  if (state.current) draft().text = input.value;
});
input.addEventListener('keydown', (e) => {
  if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
    e.preventDefault();
    sendMessage();
  }
});
$('#btn-send').onclick = sendMessage;

// ---------- attachments ----------
const ALLOWED_MIME = ['image/png', 'image/jpeg', 'image/gif', 'image/webp'];
const MAX_IMAGES = 4;
const MAX_BYTES = 10 * 1024 * 1024; // 10MB per image

$('#btn-attach').onclick = () => $('#file-input').click();
$('#file-input').addEventListener('change', async (e) => {
  await attachFiles([...e.target.files]);
  e.target.value = '';
});

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
  for (const f of files) {
    if (state.attached.length >= MAX_IMAGES) {
      alert(`最多 ${MAX_IMAGES} 张图片`);
      break;
    }
    const mime = f.type;
    if (!ALLOWED_MIME.includes(mime)) {
      alert(`不支持的图片类型：${mime || f.name}（仅 png/jpeg/gif/webp）`);
      continue;
    }
    if (f.size > MAX_BYTES) {
      alert(`图片过大：${f.name}（${(f.size / 1024 / 1024).toFixed(1)}MB > ${MAX_BYTES / 1024 / 1024}MB）`);
      continue;
    }
    const data = await readAsDataURL(f).then(stripDataURLPrefix);
    state.attached.push({ media_type: mime, data });
  }
  if (state.current) draft().images = state.attached;
  renderAttachedThumbs();
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
  state.attached.forEach((img, i) => {
    const t = document.createElement('div');
    t.className = 'attached-thumb';
    t.innerHTML = `<img alt=""><button class="x" title="移除"><i data-lucide="x"></i></button>`;
    t.querySelector('img').src = `data:${img.media_type};base64,${img.data}`;
    t.querySelector('.x').onclick = () => {
      state.attached.splice(i, 1);
      if (state.current) draft().images = state.attached;
      renderAttachedThumbs();
    };
    box.appendChild(t);
  });
  renderIcons(box);
}

async function sendMessage() {
  const text = input.value.trim();
  if ((!text && state.attached.length === 0) || !state.current) return;
  const payload = { text, images: state.attached };
  // Optimistic user bubble showing images immediately (will be replaced
  // by the server's user event which contains only text).
  appendOptimisticUser(text, state.attached);
  input.value = '';
  input.style.height = 'auto';
  state.attached = [];
  if (state.current) { draft().text = ''; draft().images = []; }
  renderAttachedThumbs();
  $('#error-banner').classList.add('hidden');
  await api(`/api/sessions/${state.current.id}/messages`, {
    method: 'POST',
    body: JSON.stringify(payload),
  });
  updateStatusUI({ running: true, queued: 0 });
}

function appendOptimisticUser(text, images) {
  const bubble = addMsg('msg-user msg-optimistic', '你');
  if (images && images.length) {
    for (const img of images) {
      const im = document.createElement('img');
      im.className = 'thumb';
      im.src = `data:${img.media_type};base64,${img.data}`;
      im.alt = '';
      bubble.appendChild(im);
    }
    if (text) bubble.appendChild(document.createTextNode(' '));
  }
  if (text) bubble.appendChild(document.createTextNode(text));
  scrollBottom();
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
    alert('该会话还没有产生 claude session（发过首轮消息后可用）');
    return;
  }
  const originalHTML = btn.innerHTML;
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

$('#btn-collapse').onclick = () => setSidebarCollapsed(true);
$('#btn-expand').onclick = () => setSidebarCollapsed(false);

// ---------- modals ----------
function openModal(sel) { $(sel).classList.remove('hidden'); }
function closeModal(el) { el.closest('.modal').classList.add('hidden'); }
document.querySelectorAll('[data-close]').forEach((b) => {
  b.onclick = () => closeModal(b);
});
document.querySelectorAll('.modal').forEach((m) => {
  m.addEventListener('click', (e) => { if (e.target === m) m.classList.add('hidden'); });
});

$('#btn-new').onclick = async () => {
  const firstCli = state.clis[0] && state.clis[0].name;
  const firstProv = state.providers[0] && state.providers[0].name;
  fillCLISelect($('#new-cli'), firstCli);
  fillProviderSelect($('#new-provider'), firstProv);
  fillModelSelect($('#new-model'), firstProv, '');
  const cfg = await api('/api/config');
  $('#new-cwd').placeholder = cfg.default_cwd || '/path/to/project';
  openModal('#modal-new');
};

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
  const s = await api('/api/sessions', { method: 'POST', body: JSON.stringify(body) });
  $('#modal-new').classList.add('hidden');
  $('#new-name').value = ''; $('#new-cwd').value = '';
  await loadSessions();
  openSession(s.id);
};

$('#btn-import').onclick = () => {
  fillCLISelect($('#import-cli'), state.clis[0] && state.clis[0].name);
  fillProviderSelect($('#import-provider'), state.providers[0] && state.providers[0].name);
  state.importList = [];
  $('#import-cwd').value = '';
  $('#import-prefix').value = '';
  openModal('#modal-import');
  scanImport();
};

$('#import-scan').onclick = scanImport;
$('#import-prefix').addEventListener('input', renderImportList);
$('#import-cwd').addEventListener('input', renderImportList);

async function scanImport() {
  const el = $('#import-list');
  el.innerHTML = '<div style="color:var(--dim);padding:20px;text-align:center">扫描中...</div>';
  const cwd = $('#import-cwd').value.trim();
  const prefix = $('#import-prefix').value.trim();
  const params = new URLSearchParams();
  if (cwd) params.set('cwd', cwd);
  if (prefix) params.set('prefix', prefix);
  const url = '/api/claude-sessions' + (params.toString() ? '?' + params : '');
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
    row.querySelector('.t').textContent = item.title;
    row.querySelector('.m').textContent = `${item.session_id.slice(0, 8)} · ${item.cwd || '?'} · ${timeAgo(item.updated_at)}`;
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
    };
    el.appendChild(row);
  }
  renderIcons(el);
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
  cliSel.innerHTML = state.clis.map((c) => `<option value="${c.name}">${c.name}</option>`).join('');
  if (state.clis.length) cliSel.value = state.clis[0].name;
  renderProviderOptions(providerSel);
  renderModelOptions(modelSel, providerSel.value);
  providerSel.onchange = () => renderModelOptions(modelSel, providerSel.value);
}

function renderProviderOptions(sel) {
  sel.innerHTML = state.providers.map((p) => `<option value="${p.name}">${p.name}</option>`).join('');
  if (state.providers.length) sel.value = state.providers[0].name;
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
  (state.nsAttached || []).forEach((img, idx) => {
    const t = document.createElement('div');
    t.className = 'thumb';
    t.innerHTML = `<img src="${img.data}"><button class="remove">×</button>`;
    t.querySelector('.remove').onclick = () => {
      state.nsAttached.splice(idx, 1);
      renderNsAttached();
    };
    box.appendChild(t);
  });
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
  const images = (state.nsAttached || []).map((img) => ({
    media_type: img.mime,
    data: stripDataURLPrefix(img.data),
  }));
  const name = content
    ? (content.length > 30 ? content.slice(0, 30) + '…' : content)
    : (images.length ? '[图片]' : '新会话');
  const sess = await api('/api/sessions', {
    method: 'POST',
    body: JSON.stringify({
      name, cli, provider, model, cwd: nsPendingCwd,
      effort: effort || undefined,
      permission_mode: perm || undefined,
    }),
  });
  $('#new-session-view').classList.add('hidden');
  state.nsAttached = [];
  await loadSessions();
  await openSession(sess.id);
  // Optimistic bubble so the user sees their message immediately even if SSE
  // hasn't connected yet (race between EventSource open + first event push).
  appendOptimisticUser(content, images);
  scrollBottom();
  await api(`/api/sessions/${sess.id}/messages`, {
    method: 'POST',
    body: JSON.stringify({ text: content, images }),
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
$('#ns-input').addEventListener('input', (e) => {
  const el = e.target;
  el.style.height = 'auto';
  el.style.height = Math.min(el.scrollHeight, 320) + 'px';
});
// attach button → file picker
$('#ns-attach').onclick = () => $('#ns-attach-file').click();
$('#ns-attach-file').onchange = async (e) => {
  const files = Array.from(e.target.files || []);
  for (const f of files) {
    if (!nsAttachOk(f)) continue;
    const data = await new Promise((res) => {
      const r = new FileReader();
      r.onload = () => res(r.result);
      r.readAsDataURL(f);
    });
    state.nsAttached = state.nsAttached || [];
    state.nsAttached.push({ data, mime: f.type });
  }
  e.target.value = '';
  renderNsAttached();
};

function nsAttachOk(f) {
  state.nsAttached = state.nsAttached || [];
  if (state.nsAttached.length >= MAX_IMAGES) {
    alert(`最多 ${MAX_IMAGES} 张图片`);
    return false;
  }
  if (!ALLOWED_MIME.includes(f.type)) {
    alert(`不支持的图片类型：${f.type || f.name}（仅 png/jpeg/gif/webp）`);
    return false;
  }
  if (f.size > MAX_BYTES) {
    alert(`图片过大：${f.name}（${(f.size / 1024 / 1024).toFixed(1)}MB > ${MAX_BYTES / 1024 / 1024}MB）`);
    return false;
  }
  return true;
}
// paste image support
$('#ns-input').addEventListener('paste', (e) => {
  const items = e.clipboardData && e.clipboardData.items;
  if (!items) return;
  for (const it of items) {
    if (it.kind === 'file' && it.type.startsWith('image/')) {
      e.preventDefault();
      const f = it.getAsFile();
      if (!f || !nsAttachOk(f)) continue;
      const r = new FileReader();
      r.onload = () => {
        state.nsAttached = state.nsAttached || [];
        state.nsAttached.push({ data: r.result, mime: f.type });
        renderNsAttached();
      };
      r.readAsDataURL(f);
    }
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
  fillCLISelect($('#sheet-cli'), state.current.cli);
  fillProviderSelect($('#sheet-provider'), state.current.provider);
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
  fillCLISelect($('#sheet-cli'), s.cli);
  fillProviderSelect($('#sheet-provider'), s.provider);
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
  const cfg = await api('/api/config');
  state.home = cfg.home || '';
  await loadSessions();
  renderIcons();
  updateMobileInputHint();
  setInterval(() => { loadSessions().catch(() => {}); }, 5000);
})();
