/* cc-webui 工作区面板：Codex app 风格的右侧文件区（文件树 + 文件预览 + 改动视图）。
 *
 * 参照 Codex 桌面端：左边聊天，右边「文件树 + 文件预览」两栏，面板可折叠、可拖宽。
 * Codex 桌面端本身不开源，它的文件区是建在 app-server v2 的 fs/readDirectory /
 * fs/readFile / fs/getMetadata 这套 RPC 上的；cc-webui 没有 app-server，直接
 * 对着会话工作区开一套等价接口（internal/server/workspace.go）。
 *
 * 面板锚在「当前会话的工作区」上：切会话整个重载，所有路径都是相对 cwd 的相对路径。
 */
'use strict';

const wsState = {
  open: false,
  sid: '',        // 当前面板所属会话
  root: '',       // 会话工作区绝对路径
  filter: '',     // 文件名筛选（已加载的节点）
  onlyChanged: false,
  dirs: {},       // relPath -> {entries:[], loaded:bool, expanded:bool}
  changes: {},    // relPath -> git 状态码（如 " M" / "??"）
  file: null,     // {path,name,content,binary,truncated,size,mtime,abs}
  line: 0,        // 链接指向的行号（消息里的 `path:12`），0 = 没指定
  mode: 'preview', // preview | source | diff
  diff: null,     // {unstaged, staged, untracked, repo}
  search: { q: '', entries: [], loaded: false, loading: false, error: '', truncated: false },
  searchTimer: 0,
  searchSeq: 0,
  searchAbort: null,
  width: 720,
};

const WS_OPEN_KEY = 'cc-webui:ws-panel-open';
const WS_WIDTH_KEY = 'cc-webui:ws-panel-width';
const WS_TREE_KEY = 'cc-webui:ws-panel-tree-collapsed';

// 代码/标记类扩展名：走 highlight.js 着色；其余按纯文本显示。
const WS_CODE_EXT = new Set([
  'js', 'mjs', 'cjs', 'jsx', 'ts', 'tsx', 'go', 'py', 'rb', 'rs', 'java', 'kt', 'swift',
  'c', 'h', 'cpp', 'hpp', 'cc', 'cs', 'php', 'sh', 'bash', 'zsh', 'fish', 'sql', 'lua',
  'pl', 'r', 'scala', 'dart', 'ex', 'exs', 'hs', 'clj', 'vue', 'svelte', 'astro',
  'json', 'jsonc', 'yaml', 'yml', 'toml', 'ini', 'conf', 'env', 'properties', 'xml',
  'html', 'htm', 'css', 'scss', 'less', 'dockerfile', 'makefile', 'mk', 'gradle',
  'diff', 'patch', 'proto', 'graphql', 'gql', 'tf', 'tfvars', 'zig', 'nim', 'vim',
]);

const WS_IMAGE_EXT = new Set(['png', 'jpg', 'jpeg', 'gif', 'webp', 'bmp', 'svg']);

function wsExt(name) {
  const i = name.lastIndexOf('.');
  if (i < 0) return '';
  return name.slice(i + 1).toLowerCase();
}

function wsIsImage(name) { return WS_IMAGE_EXT.has(wsExt(name)); }

function wsIsCode(name) { return WS_CODE_EXT.has(wsExt(name)); }

function wsIsMarkdown(name) {
  const e = wsExt(name);
  return e === 'md' || e === 'markdown' || e === 'mdx';
}

// ---------- 面板开关 ----------

function wsToggle(force) {
  const next = typeof force === 'boolean' ? force : !wsState.open;
  wsState.open = next;
  localStorage.setItem(WS_OPEN_KEY, next ? '1' : '0');
  $('#ws-panel').classList.toggle('hidden', !next);
  $('#btn-workspace').classList.toggle('active', next);
  if (next) {
    wsEnsureRoot();
    wsRefreshStatus();
  }
}

function wsEnsureRoot() {
  const sess = state.current;
  if (!sess) return;
  const root = sess.cwd || '';
  // 会话变了、或工作区被改过（迁移项目），都得整树重载
  if (wsState.sid === sess.id && wsState.root === root) return;
  wsState.sid = sess.id || '';
  wsState.root = root;
  wsState.dirs = {};
  wsState.changes = {};
  wsState.file = null;
  wsState.mode = 'preview';
  wsState.diff = null;
  wsState.filter = '';
  wsState.search = { q: '', entries: [], loaded: false, loading: false, error: '', truncated: false };
  wsState.searchSeq++;
  clearTimeout(wsState.searchTimer);
  wsAbortSearch();
  const filter = $('#ws-filter');
  if (filter) filter.value = '';
  if (wsState.root) {
    wsLoadDir('', true);
  } else {
    wsRenderTree();
    wsRenderViewer();
  }
}

// 切会话：面板开着就跟着换工作区。
function wsOnSessionChange() {
  if (!wsState.open) return;
  wsEnsureRoot();
  wsRefreshStatus();
}

// ---------- 文件树 ----------

async function wsLoadDir(path, force) {
  if (!wsState.sid) return;
  const node = wsState.dirs[path] || (wsState.dirs[path] = { entries: [], loaded: false, expanded: false });
  if (node.loaded && !force) return;
  try {
    const res = await api(`/api/sessions/${wsState.sid}/workspace/tree?path=${encodeURIComponent(path)}`);
    node.entries = res.entries || [];
    node.loaded = true;
    node.skipped = !!res.skipped;
    node.truncated = !!res.truncated;
  } catch (err) {
    node.entries = [];
    node.loaded = true;
    node.error = err.message || String(err);
  }
  wsRenderTree();
}

function wsToggleDir(path) {
  // 目录节点是「第一次展开时」才创建的：父目录的列表里有这一项，
  // 但 wsState.dirs 里还没有它的条目。所以这里要现建，不能只查。
  const node = wsState.dirs[path] || (wsState.dirs[path] = { entries: [], loaded: false, expanded: false });
  node.expanded = !node.expanded;
  if (node.expanded && !node.loaded) wsLoadDir(path);
  else wsRenderTree();
}

function wsStatusBadge(path) {
  const code = wsState.changes[path];
  if (!code) return '';
  const c = code.trim();
  if (c === '??') return { text: '新', cls: 'ws-git-new' };
  if (c.startsWith('A')) return { text: 'A', cls: 'ws-git-a' };
  if (c.startsWith('D')) return { text: 'D', cls: 'ws-git-d' };
  if (c.startsWith('R')) return { text: 'R', cls: 'ws-git-r' };
  if (c.startsWith('M')) return { text: 'M', cls: 'ws-git-m' };
  return { text: c || '?', cls: 'ws-git-other' };
}

function wsRenderTree() {
  const host = $('#ws-tree');
  if (!host) return;
  host.innerHTML = '';
  if (!wsState.root) {
    host.innerHTML = '<div class="ws-empty">当前会话没有工作区</div>';
    return;
  }
  const frag = document.createDocumentFragment();
  // 有筛选词时走整树搜索（服务端递归），而不是只筛已加载的节点——
  // 没展开过的深层文件、输入完整相对路径，都必须能找到。
  if (wsState.filter) wsRenderSearch(frag);
  else wsRenderDir(frag, '', 0);
  host.appendChild(frag);
  renderIcons(host);
  $('#ws-tree-root').textContent = wsState.root;
  $('#ws-tree-root').title = wsState.root;
  const changed = Object.keys(wsState.changes).length;
  $('#ws-tree-changed').textContent = changed ? `${changed} 个改动` : '';
}

// wsBuildRow 造一行树节点。showPath=true 时显示完整相对路径（搜索结果用），
// 否则只显示文件名（目录树里靠缩进表达层级）。
function wsBuildRow(entry, depth, showPath) {
  const row = document.createElement('div');
  row.className = 'ws-row' + (entry.dir ? ' ws-dir' : ' ws-file');
  row.dataset.path = entry.path;
  row.dataset.dir = entry.dir ? '1' : '';
  row.style.paddingLeft = (depth * 12 + 8) + 'px';
  row.title = entry.path;

  if (entry.dir) {
    const child = wsState.dirs[entry.path];
    const expanded = child && child.expanded;
    row.innerHTML = `<i data-lucide="${expanded ? 'chevron-down' : 'chevron-right'}" class="ws-chevron"></i>` +
      `<i data-lucide="folder" class="ws-icon"></i>`;
  } else {
    row.innerHTML = `<i data-lucide="${wsIsImage(entry.name) ? 'file-image' : wsIsCode(entry.name) ? 'file-code' : 'file-text'}" class="ws-icon"></i>`;
  }
  const name = document.createElement('span');
  name.className = 'ws-name';
  name.textContent = showPath ? entry.path : entry.name;
  row.appendChild(name);
  if (entry.skip) {
    const skip = document.createElement('span');
    skip.className = 'ws-tag';
    skip.textContent = '忽略';
    row.appendChild(skip);
  }
  if (entry.ignored) {
    // .gitignore 里的文件照样显示，只是标出来，免得用户以为它进了版本库。
    const tag = document.createElement('span');
    tag.className = 'ws-tag ws-tag-ignored';
    tag.textContent = 'git 忽略';
    row.appendChild(tag);
  }
  const badge = entry.dir ? null : wsStatusBadge(entry.path);
  if (badge) {
    const b = document.createElement('span');
    b.className = 'ws-git-badge ' + badge.cls;
    b.textContent = badge.text;
    row.appendChild(b);
  }
  return row;
}

function wsRenderDir(parent, path, depth) {
  const node = wsState.dirs[path];
  if (!node) return;
  if (!node.loaded) {
    const loading = document.createElement('div');
    loading.className = 'ws-loading';
    loading.textContent = '加载中…';
    loading.style.paddingLeft = (depth * 12 + 18) + 'px';
    parent.appendChild(loading);
    return;
  }
  if (node.skipped) {
    const skip = document.createElement('div');
    skip.className = 'ws-skip-note';
    skip.textContent = '（已忽略的大目录）';
    skip.style.paddingLeft = (depth * 12 + 18) + 'px';
    parent.appendChild(skip);
    return;
  }
  for (const entry of node.entries) {
    const changed = !!wsState.changes[entry.path];
    if (wsState.onlyChanged && !changed) continue;
    parent.appendChild(wsBuildRow(entry, depth, false));
    if (entry.dir) {
      const child = wsState.dirs[entry.path];
      if (child && child.expanded) {
        const kids = document.createElement('div');
        kids.className = 'ws-children';
        parent.appendChild(kids);
        wsRenderDir(kids, entry.path, depth + 1);
      }
    }
  }
}

// wsRenderSearch 渲染整树搜索的扁平结果（不缩进，直接显示完整相对路径）。
function wsRenderSearch(parent) {
  const res = wsState.search || {};
  if (res.error) {
    const err = document.createElement('div');
    err.className = 'ws-empty ws-error';
    err.textContent = res.error;
    parent.appendChild(err);
    return;
  }
  if (!res.loaded) {
    const loading = document.createElement('div');
    loading.className = 'ws-loading';
    loading.textContent = '搜索中…';
    parent.appendChild(loading);
    return;
  }
  const hits = res.entries.filter((e) => !wsState.onlyChanged || !!wsState.changes[e.path]);
  if (!hits.length) {
    const empty = document.createElement('div');
    empty.className = 'ws-empty';
    empty.textContent = '没有匹配的文件（已包含 git 忽略的文件）';
    parent.appendChild(empty);
    return;
  }
  for (const entry of hits) {
    const row = wsBuildRow(entry, 0, true);
    row.classList.add('ws-search-hit');
    parent.appendChild(row);
  }
  if (res.truncated) {
    const note = document.createElement('div');
    note.className = 'ws-skip-note';
    note.textContent = '结果过多，只显示前 ' + hits.length + ' 条';
    parent.appendChild(note);
  }
}

// wsAbortSearch 取消上一次还没回来的搜索：大仓库一次全树扫描要一两秒，
// 连打几个字符不取消的话会叠着跑，服务端也白扫。
function wsAbortSearch() {
  if (wsState.searchAbort) {
    try { wsState.searchAbort.abort(); } catch (_) { /* 已结束也无所谓 */ }
    wsState.searchAbort = null;
  }
}

// wsSearchNow 拉一次整树搜索。seq 防抖 + 丢弃过期响应：快速连打时
// 先发的请求可能后回来，不判序号会把旧结果盖在新筛选词上。
async function wsSearchNow(q) {
  if (!wsState.sid || !q) return;
  wsAbortSearch();
  const seq = ++wsState.searchSeq;
  const ctrl = typeof AbortController !== 'undefined' ? new AbortController() : null;
  wsState.searchAbort = ctrl;
  try {
    const res = await api(
      `/api/sessions/${wsState.sid}/workspace/search?q=${encodeURIComponent(q)}`,
      ctrl ? { signal: ctrl.signal } : {});
    if (seq !== wsState.searchSeq || wsState.filter !== q) return;
    wsState.search = {
      q,
      entries: res.entries || [],
      loaded: true,
      loading: false,
      error: '',
      truncated: !!res.truncated,
    };
  } catch (err) {
    if (seq !== wsState.searchSeq || wsState.filter !== q) return;
    wsState.search = {
      q,
      entries: [],
      loaded: true,
      loading: false,
      error: err.message || String(err),
      truncated: false,
    };
  }
  wsRenderTree();
}

// wsScheduleSearch 输入即触发，但停手 300ms 才发请求（打字时别一个字符一次）。
function wsScheduleSearch() {
  clearTimeout(wsState.searchTimer);
  const q = wsState.filter;
  if (!q) {
    wsAbortSearch();
    wsState.search = { q: '', entries: [], loaded: false, loading: false, error: '', truncated: false };
    wsState.searchSeq++;
    wsRenderTree();
    return;
  }
  // 筛选词一改，旧结果立刻作废、旧请求立刻取消。
  if (wsState.search.q !== q) {
    wsAbortSearch();
    wsState.search = { q, entries: [], loaded: false, loading: true, error: '', truncated: false };
  }
  wsRenderTree();
  wsState.searchTimer = setTimeout(() => wsSearchNow(q), 300);
}

// 清空筛选：点搜索结果里的目录时用，先回到文件树再逐级展开。
function wsClearFilter() {
  wsState.filter = '';
  wsState.search = { q: '', entries: [], loaded: false, loading: false, error: '', truncated: false };
  wsState.searchSeq++;
  clearTimeout(wsState.searchTimer);
  wsAbortSearch();
  const input = $('#ws-filter');
  if (input) input.value = '';
}

// ---------- 文件预览 ----------

async function wsOpenFile(path, line) {
  if (!wsState.sid) return;
  wsState.file = { path, name: path.split('/').pop(), loading: true };
  wsState.mode = 'preview';
  wsState.line = line || 0;
  wsState.diff = null;
  wsRenderViewer();
  try {
    const res = await api(`/api/sessions/${wsState.sid}/workspace/file?path=${encodeURIComponent(path)}`);
    wsState.file = Object.assign({ path }, res);
    wsState.file.loading = false;
  } catch (err) {
    wsState.file = { path, name: path.split('/').pop(), error: err.message || String(err) };
  }
  wsRenderViewer();
}

// wsOpenPath 按**绝对路径**打开工作区里的文件（消息里的文件链接走这条）。
// 文件不在当前会话工作区内就返回 false，由调用方决定回退到哪个面板。
function wsOpenPath(absPath, line) {
  const sess = state.current;
  if (!sess || !absPath) return false;
  const root = (sess.cwd || '').replace(/\/+$/, '');
  if (!root) return false;
  if (absPath !== root && !absPath.startsWith(root + '/')) return false;
  const rel = absPath.slice(root.length + 1);
  if (!rel) return false;
  if (!wsState.open) wsToggle(true); // 面板没收起就先摆出来
  wsEnsureRoot();
  wsOpenFile(rel, line);
  wsRevealFile(rel);
  return true;
}

// 在文件树里定位到某个文件：先把它的上级目录逐级展开，再滚到那一行。
function wsRevealFile(rel) {
  const dir = rel.split('/').slice(0, -1).join('/');
  const step = async () => {
    if (dir) await wsRevealDir(dir);
    wsRenderTree();
    const row = document.querySelector('.ws-row[data-path="' + CSS.escape(rel) + '"]');
    if (row) row.scrollIntoView({ block: 'nearest' });
  };
  step();
}

function wsRenderViewer() {
  const body = $('#ws-viewer-body');
  const crumbs = $('#ws-crumbs');
  if (!body || !crumbs) return;
  const file = wsState.file;
  crumbs.innerHTML = '';
  $('#ws-diff').classList.add('hidden');
  $('#ws-source').classList.add('hidden');
  $('#ws-open').classList.add('hidden');
  $('#ws-refresh').classList.add('hidden');
  $('#ws-diff').classList.remove('active');
  $('#ws-source').classList.remove('active');

  if (!file) {
    body.innerHTML = '<div class="ws-empty">从右侧文件树里选一个文件</div>';
    return;
  }

  // 面包屑：目录段点击 = 在文件树里展开到那一层
  const segs = file.path.split('/');
  segs.forEach((seg, i) => {
    if (i > 0) {
      const sep = document.createElement('span');
      sep.className = 'ws-crumb-sep';
      sep.textContent = '/';
      crumbs.appendChild(sep);
    }
    const b = document.createElement('button');
    b.className = 'ws-crumb' + (i === segs.length - 1 ? ' active' : '');
    b.textContent = seg;
    b.dataset.path = segs.slice(0, i + 1).join('/');
    b.dataset.dir = i === segs.length - 1 && file.dir ? '1' : '';
    crumbs.appendChild(b);
  });

  if (file.error) {
    body.innerHTML = `<div class="ws-empty ws-error">${wsEscape(file.error)}</div>`;
    return;
  }
  if (file.loading) {
    body.innerHTML = '<div class="ws-empty">加载中…</div>';
    return;
  }

  $('#ws-open').classList.remove('hidden');
  if (!file.binary) {
    $('#ws-diff').classList.remove('hidden');
    $('#ws-source').classList.remove('hidden');
  }
  $('#ws-refresh').classList.remove('hidden');
  if (wsState.mode === 'diff') $('#ws-diff').classList.add('active');
  if (wsState.mode === 'source') $('#ws-source').classList.add('active');

  if (wsState.mode === 'diff' && wsState.diff) {
    wsRenderDiff(body);
    return;
  }
  if (wsState.mode === 'source' || file.binary) {
    if (file.binary) {
      body.innerHTML = '<div class="ws-empty">二进制文件，无法预览。点右上角「打开」用系统程序查看。</div>';
      return;
    }
    body.innerHTML = '';
    const pre = document.createElement('pre');
    pre.className = 'ws-plain';
    pre.textContent = file.content || '';
    body.appendChild(pre);
    wsMarkLine(pre, wsState.line);
    wsAppendNote(body, file);
    return;
  }

  body.innerHTML = '';
  if (wsIsImage(file.name)) {
    const img = document.createElement('img');
    img.className = 'ws-image';
    img.src = '/api/image?path=' + encodeURIComponent(file.abs || '');
    img.alt = file.name;
    body.appendChild(img);
    wsAppendNote(body, file);
    return;
  }
  if (wsIsMarkdown(file.name)) {
    const div = document.createElement('div');
    div.className = 'ws-md';
    div.innerHTML = md(file.content || '', { artifacts: false });
    body.appendChild(div);
    if (wsState.line) {
      const hint = document.createElement('div');
      hint.className = 'ws-note ws-line-hint';
      hint.textContent = '链接指向第 ' + wsState.line + ' 行（Markdown 按渲染后展示，未定位）';
      body.appendChild(hint);
    }
    wsAppendNote(body, file);
    return;
  }
  const pre = document.createElement('pre');
  pre.className = 'ws-code';
  const code = document.createElement('code');
  const lang = wsExt(file.name);
  if (wsIsCode(file.name) && hljs.getLanguage(lang)) {
    try {
      code.innerHTML = hljs.highlight(file.content || '', { language: lang }).value;
      code.className = 'language-' + lang;
    } catch (_) {
      code.textContent = file.content || '';
    }
  } else {
    code.textContent = file.content || '';
  }
  pre.appendChild(code);
  body.appendChild(pre);
  wsMarkLine(pre, wsState.line);
  wsAppendNote(body, file);
}

// wsMarkLine 在代码/纯文本视图里标出链接指向的那一行。
//
// 整段高亮的 HTML 不方便切行（一个元素可能横跨多行，切开就是坏 HTML），
// 所以不切内容，改成一个绝对定位的色块盖在那一行的位置上——pre 里是等宽不换行
// 的，行高算得准，色块跟着内容一起滚。
function wsMarkLine(pre, line) {
  if (!line || line < 1 || !pre) return;
  const code = pre.querySelector('code') || pre;
  const lh = parseFloat(getComputedStyle(code).lineHeight) || 18;
  const preRect = pre.getBoundingClientRect();
  const codeRect = code.getBoundingClientRect();
  const base = (codeRect.top - preRect.top) + pre.scrollTop;
  const top = base + (line - 1) * lh;
  const mark = document.createElement('span');
  mark.className = 'ws-line-mark';
  mark.style.top = top + 'px';
  mark.style.height = lh + 'px';
  pre.appendChild(mark);
  pre.scrollTop = Math.max(0, top - pre.clientHeight / 3);
}

// 预览页脚：截断提示 + 大小 + 修改时间
function wsAppendNote(body, file) {
  const note = document.createElement('div');
  note.className = 'ws-note';
  const bits = [];
  if (file.truncated) bits.push('文件过大，已截断显示');
  if (typeof file.size === 'number') bits.push(wsHumanSize(file.size));
  if (file.mtime) bits.push(new Date(file.mtime * 1000).toLocaleString());
  note.textContent = bits.join(' · ');
  if (bits.length) body.appendChild(note);
}

function wsHumanSize(n) {
  if (n >= 1 << 30) return (n / (1 << 30)).toFixed(1) + ' GB';
  if (n >= 1 << 20) return (n / (1 << 20)).toFixed(1) + ' MB';
  if (n >= 1 << 10) return (n / (1 << 10)).toFixed(1) + ' KB';
  return n + ' B';
}

// ---------- 改动（diff） ----------

async function wsShowDiff() {
  const file = wsState.file;
  if (!file || !file.path) return;
  // 再点一次「显示更改」回到预览
  if (wsState.mode === 'diff') {
    wsState.mode = 'preview';
    wsState.diff = null;
    wsRenderViewer();
    return;
  }
  wsState.mode = 'diff';
  wsState.diff = { loading: true };
  wsRenderViewer();
  try {
    const res = await api(`/api/sessions/${wsState.sid}/workspace/diff?path=${encodeURIComponent(file.path)}`);
    wsState.diff = res;
  } catch (err) {
    wsState.diff = { error: err.message || String(err) };
  }
  wsRenderViewer();
}

// 重新拉一次当前文件（agent 改完文件后按一下就能看到新的）。
async function wsRefreshCurrentFile(silent) {
  const file = wsState.file;
  if (!file || !file.path || file.loading || file.error) return;
  const before = file.content;
  try {
    const res = await api(`/api/sessions/${wsState.sid}/workspace/file?path=${encodeURIComponent(file.path)}`);
    const changed = res.content !== before;
    wsState.file = Object.assign({ path: file.path }, res);
    if (changed || !silent) wsRenderViewer();
  } catch (_) {
    if (!silent) wsRenderViewer();
  }
}

function wsRenderDiff(body) {
  const diff = wsState.diff || {};
  body.innerHTML = '';
  if (diff.loading) {
    body.innerHTML = '<div class="ws-empty">加载中…</div>';
    return;
  }
  if (diff.error) {
    body.innerHTML = `<div class="ws-empty ws-error">${wsEscape(diff.error)}</div>`;
    return;
  }
  if (!diff.repo) {
    body.innerHTML = '<div class="ws-empty">当前工作区不是 git 仓库，没有改动可看。</div>';
    return;
  }
  const text = diff.unstaged || diff.staged || '';
  if (!text.trim()) {
    body.innerHTML = '<div class="ws-empty">这个文件没有未提交的改动。</div>';
    return;
  }
  const pre = document.createElement('pre');
  pre.className = 'ws-diff';
  for (const line of text.split('\n')) {
    const span = document.createElement('span');
    if (line.startsWith('+++') || line.startsWith('---')) span.className = 'ws-diff-meta';
    else if (line.startsWith('@@')) span.className = 'ws-diff-hunk';
    else if (line.startsWith('+')) span.className = 'ws-diff-add';
    else if (line.startsWith('-')) span.className = 'ws-diff-del';
    else if (line.startsWith('\\')) span.className = 'ws-diff-meta';
    span.textContent = line + '\n';
    pre.appendChild(span);
  }
  body.appendChild(pre);
  if (diff.untracked) {
    const note = document.createElement('div');
    note.className = 'ws-note';
    note.textContent = '这是未跟踪的新文件，下面是它的完整内容';
    body.appendChild(note);
  }
}

// ---------- git 状态 ----------

async function wsRefreshStatus() {
  if (!wsState.open || !wsState.sid || !wsState.root) return;
  try {
    const res = await api(`/api/sessions/${wsState.sid}/workspace/status`);
    wsState.changes = res.changes || {};
    wsRenderTree();
  } catch (_) {
    // 不是 git 仓库 / 出错：状态标记可有可无，不打扰用户
  }
}

// ---------- 打开（系统默认程序） ----------

async function wsOpenExternally() {
  const f = wsState.file;
  if (!f || !f.path) return;
  try {
    await api(`/api/sessions/${wsState.sid}/workspace/open`, {
      method: 'POST',
      body: JSON.stringify({ path: f.path }),
    });
    wsToast('已用系统默认程序打开');
  } catch (err) {
    wsToast('打开失败：' + (err.message || err));
  }
}

function wsToast(text) {
  const layer = $('#toast-layer');
  if (!layer) return;
  const el = document.createElement('div');
  el.className = 'toast';
  el.textContent = text;
  layer.appendChild(el);
  setTimeout(() => el.remove(), 2600);
}

function wsEscape(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}

// ---------- 事件绑定 ----------

function wsInit() {
  const btn = $('#btn-workspace');
  if (btn) btn.onclick = () => wsToggle();

  const close = $('#ws-close');
  if (close) close.onclick = () => wsToggle(false);

  const source = $('#ws-source');
  if (source) {
    source.onclick = () => {
      if (!wsState.file) return;
      wsState.mode = wsState.mode === 'source' ? 'preview' : 'source';
      wsRenderViewer();
    };
  }

  const diff = $('#ws-diff');
  if (diff) diff.onclick = () => wsShowDiff();

  const refresh = $('#ws-refresh');
  if (refresh) refresh.onclick = () => wsRefreshCurrentFile(false);

  const open = $('#ws-open');
  if (open) open.onclick = () => wsOpenExternally();

  const filter = $('#ws-filter');
  if (filter) {
    filter.oninput = () => {
      wsState.filter = filter.value.trim();
      wsScheduleSearch();
    };
  }

  const changed = $('#ws-tree-changed');
  if (changed) {
    changed.onclick = () => {
      wsState.onlyChanged = !wsState.onlyChanged;
      changed.classList.toggle('active', wsState.onlyChanged);
      wsRenderTree();
    };
  }

  const treeToggle = $('#ws-tree-toggle');
  if (treeToggle) {
    treeToggle.onclick = () => {
      const pane = $('#ws-tree-pane');
      const on = !pane.classList.contains('collapsed');
      wsTreeCollapsed(on);
      try { localStorage.setItem(WS_TREE_KEY, on ? '1' : '0'); } catch (_) { /* 存不了就算了 */ }
    };
  }

  // 文件树点击：目录展开/收起，文件打开预览
  const tree = $('#ws-tree');
  if (tree) {
    tree.onclick = (ev) => {
      const crumb = ev.target.closest('.ws-crumb');
      if (crumb) {
        const path = crumb.dataset.path || '';
        const dir = crumb.dataset.dir === '1';
        if (dir) {
          wsRevealDir(path);
        } else {
          wsOpenFile(path);
        }
        return;
      }
      const row = ev.target.closest('.ws-row');
      if (!row) return;
      const path = row.dataset.path || '';
      if (row.dataset.dir === '1') {
        // 搜索结果里的目录：先退出搜索、回到文件树，再逐级展开到它。
        if (wsState.filter) {
          wsClearFilter();
          wsRevealDir(path);
        } else {
          wsToggleDir(path);
        }
      } else {
        wsOpenFile(path);
      }
    };
  }

  // 宽度拖拽
  const splitter = $('#ws-splitter');
  if (splitter) {
    splitter.addEventListener('mousedown', (ev) => {
      ev.preventDefault();
      splitter.classList.add('dragging');
      const panel = $('#ws-panel');
      const move = (e) => {
        const w = Math.max(360, Math.min(window.innerWidth - 320, window.innerWidth - e.clientX));
        wsState.width = w;
        panel.style.width = w + 'px';
      };
      const up = () => {
        splitter.classList.remove('dragging');
        document.removeEventListener('mousemove', move);
        document.removeEventListener('mouseup', up);
        localStorage.setItem(WS_WIDTH_KEY, String(wsState.width));
      };
      document.addEventListener('mousemove', move);
      document.addEventListener('mouseup', up);
    });
  }

  // 恢复上次的宽度与展开状态
  try {
    const w = parseInt(localStorage.getItem(WS_WIDTH_KEY) || '', 10);
    if (w > 0) {
      wsState.width = w;
      $('#ws-panel').style.width = w + 'px';
    }
  } catch (_) { /* localStorage 不可用就算了 */ }

  // 恢复上次的展开状态（面板开着就先摆出来，等会话打开后自动填内容）
  try {
    if (localStorage.getItem(WS_OPEN_KEY) === '1') wsToggle(true);
  } catch (_) { /* localStorage 不可用就算了 */ }

  // 文件树折叠状态
  try {
    wsTreeCollapsed(localStorage.getItem(WS_TREE_KEY) === '1');
  } catch (_) { /* localStorage 不可用就算了 */ }

  wsInitTips();
}

// ---------- 文件树折叠 ----------

// wsTreeCollapsed 收起/展开右侧文件树：收起来后预览区吃掉整块宽度。
// 折叠态只留一条窄条 + 展开按钮，位置不变（按钮一直在树那一侧的头部）。
function wsTreeCollapsed(on) {
  const pane = $('#ws-tree-pane');
  const btn = $('#ws-tree-toggle');
  if (!pane || !btn) return;
  pane.classList.toggle('collapsed', !!on);
  btn.innerHTML = `<i data-lucide="${on ? 'panel-right-open' : 'panel-right-close'}"></i>`;
  btn.dataset.tip = on ? '展开文件树' : '收起文件树，把宽度让给预览区';
  renderIcons(btn);
}

// ---------- 悬浮说明 ----------
//
// 面板里的按钮都是纯图标，光看图标猜不出干什么，所以自己画一个 tooltip。
// 不用原生 title：它要悬停一秒才出、样式也不受控，而且触屏上根本不出现。
function wsInitTips() {
  const panel = $('#ws-panel');
  if (!panel) return;
  let tip = null;
  let timer = null;
  let current = null;

  const ensureTip = () => {
    if (!tip) {
      tip = document.createElement('div');
      tip.className = 'ws-tip';
      document.body.appendChild(tip);
    }
    return tip;
  };
  const place = (target) => {
    const el = ensureTip();
    el.textContent = target.dataset.tip || '';
    el.style.display = 'block';
    const r = target.getBoundingClientRect();
    const tw = el.offsetWidth || 160;
    const th = el.offsetHeight || 24;
    let left = r.left + r.width / 2 - tw / 2;
    left = Math.max(6, Math.min(window.innerWidth - tw - 6, left));
    let top = r.bottom + 8;
    if (top + th > window.innerHeight - 6) top = r.top - th - 8; // 下方没空间就放上面
    el.style.left = left + 'px';
    el.style.top = Math.max(6, top) + 'px';
  };
  const hide = () => {
    clearTimeout(timer);
    timer = null;
    current = null;
    if (tip) tip.style.display = 'none';
  };

  panel.addEventListener('mouseover', (e) => {
    const t = e.target.closest('[data-tip]');
    if (!t || !t.dataset.tip) { hide(); return; }
    if (t === current) return;
    current = t;
    clearTimeout(timer);
    // 一点点延迟：鼠标扫过一排图标时不会闪出一串 tooltip
    timer = setTimeout(() => place(t), 150);
  });
  panel.addEventListener('mouseout', (e) => {
    const t = e.target.closest('[data-tip]');
    if (!t) return;
    const to = e.relatedTarget;
    if (to && t.contains(to)) return; // 移到按钮自己的子元素上，别急着收
    if (current === t) hide();
  });
  panel.addEventListener('mousedown', hide);
  window.addEventListener('scroll', hide, true);
}

// 在文件树里展开到某个目录（面包屑点击用）
function wsRevealDir(path) {
  if (!path) return;
  // 逐级打开
  const parts = path.split('/');
  let cur = '';
  const step = async () => {
    for (const part of parts) {
      cur = cur ? cur + '/' + part : part;
      const node = wsState.dirs[cur] || (wsState.dirs[cur] = { entries: [], loaded: false, expanded: false });
      node.expanded = true;
      if (!node.loaded) await wsLoadDir(cur, true);
    }
    wsRenderTree();
    const row = document.querySelector(`.ws-row[data-path="${CSS.escape(path)}"]`);
    if (row) row.scrollIntoView({ block: 'nearest' });
  };
  step();
}

document.addEventListener('DOMContentLoaded', wsInit);
