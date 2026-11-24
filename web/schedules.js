/* AgentDeck · 定时任务页（schedules.js）
 *
 * 管理 AgentDeck 自己的 cron 任务：<data_dir>/schedules.json，到点通过 A2A 总线
 * 把 prompt 投给目标会话（见 internal/server/schedules.go）。
 *
 * 调度器是 AgentDeck 服务进程内置的 Go 实现（15s tick），不依赖系统 crontab，
 * Windows / macOS / Linux 通用。Claude Code harness 的 CronCreate
 * （.claude/scheduled_tasks.json）是 CLI 自己的一套机制，也不是 OS cron：本页
 * 只读展示它，允许显式导入成 AgentDeck 任务，绝不改那个文件。
 *
 * 命名约定：本模块所有顶层标识以 sched 开头，避免和 app.js / agent*.js 撞名。
 */
'use strict';

const SCHED_EXAMPLES = [
  { cron: '*/5 * * * *', label: '每 5 分钟' },
  { cron: '0 * * * *', label: '每小时整点' },
  { cron: '0 9 * * *', label: '每天 09:00' },
  { cron: '30 18 * * 1-5', label: '工作日 18:30' },
  { cron: '0 9 * * 1', label: '每周一 09:00' },
];

const SCHED_HARNESS_EXTRA_KEY = 'cc-webui:sched-harness-extra-paths';
const SCHED_HARNESS_IGNORED_KEY = 'cc-webui:sched-harness-ignored-paths';

const sched = {
  tasks: [],
  loaded: false,
  busy: false,
  lastError: '',
  editing: '',      // 正在编辑的任务 id（空 = 新建）
  mode: 'nl',       // 弹窗排期方式：nl | cron
  parser: 'rules',  // 自然语言解析方式：rules | ai
  aiBusy: false,
  candidates: [],   // 自然语言解析候选
  selectedCron: '',
  parsing: false,
  autoPrompt: '',   // 描述自动填入的 prompt（描述改了跟着更新；用户手改后不再跟）
  previewSeq: 0,
  pollTimer: 0,
  previewTimer: 0,
  harness: { groups: [], files: [], candidates: [], error: '', loaded: false },
  harnessBusy: false,
  harnessExtra: [],    // 手动添加的路径（localStorage）
  harnessIgnored: [],  // 被移除显示的文件路径（localStorage）
};

function schedErrMsg(e) {
  return (e && e.message) || String(e);
}

function schedEnc(id) { return encodeURIComponent(id); }

function schedPad(n) { return String(n).padStart(2, '0'); }

// 服务端下发 RFC3339；列表里显示本机时区的 MM-DD HH:mm。
function schedFmtTime(v) {
  if (!v) return '—';
  const d = new Date(v);
  if (Number.isNaN(d.getTime())) return String(v);
  return (d.getMonth() + 1) + '-' + schedPad(d.getDate()) + ' ' + schedPad(d.getHours()) + ':' + schedPad(d.getMinutes());
}

function schedFmtFull(v) {
  if (!v) return '';
  const d = new Date(v);
  if (Number.isNaN(d.getTime())) return String(v);
  return d.getFullYear() + '-' + schedPad(d.getMonth() + 1) + '-' + schedPad(d.getDate()) + ' ' +
    schedPad(d.getHours()) + ':' + schedPad(d.getMinutes());
}

function schedFirstLine(s) {
  for (const line of String(s || '').split('\n')) {
    const t = line.trim();
    if (t) return t;
  }
  return '';
}

function schedStatus(task) {
  const raw = String(task.last_status || '');
  if (!raw) return { text: '未运行', cls: '', tip: '' };
  if (raw === 'ok') return { text: '成功', cls: 'ok', tip: task.last_run_at ? '最近一次执行成功' : '' };
  return { text: '失败', cls: 'bad', tip: raw.replace(/^error:\s*/, '') };
}

function schedShowError(msg) {
  const el = $('#sched-error');
  if (!el) return;
  sched.lastError = msg || '';
  el.textContent = msg || '';
  el.classList.toggle('hidden', !msg);
}

function schedModalError(msg) {
  const el = $('#sched-modal-error');
  if (!el) return;
  el.textContent = msg || '';
  el.classList.toggle('hidden', !msg);
}

function schedParseError(msg) {
  const el = $('#sched-parse-error');
  if (!el) return;
  el.textContent = msg || '';
  el.classList.toggle('hidden', !msg);
}

function schedLoadPaths(key) {
  try {
    const v = JSON.parse(localStorage.getItem(key) || '[]');
    return Array.isArray(v) ? v.filter((x) => typeof x === 'string' && x.trim()) : [];
  } catch (_) {
    return [];
  }
}

function schedSavePaths(key, list) {
  try { localStorage.setItem(key, JSON.stringify(list)); } catch (_) { /* 无痕模式 */ }
}

// ---------- 页面开关 ----------
function schedulesOpen() {
  const view = $('#schedules-view');
  if (!view) return;
  if (typeof hideAllViews === 'function') hideAllViews();
  if (typeof boardDisconnect === 'function') boardDisconnect();
  view.classList.remove('hidden');
  if (typeof renderIcons === 'function') renderIcons();
  sched.harnessExtra = schedLoadPaths(SCHED_HARNESS_EXTRA_KEY);
  sched.harnessIgnored = schedLoadPaths(SCHED_HARNESS_IGNORED_KEY);
  schedulesRefresh();
  schedStartPoll();
  // Personal Agent 模式下让左栏高亮跟着走（agent.js 的 paRenderNav 读 pa.view）。
  if (typeof pa === 'object' && pa) {
    pa.view = 'schedules';
    if (typeof paRenderNav === 'function') paRenderNav();
    if (typeof paSyncAside === 'function') paSyncAside();
    if (typeof agentRenderCluster === 'function') agentRenderCluster();
  }
}

function schedulesClose() {
  const view = $('#schedules-view');
  if (!view) return;
  view.classList.add('hidden');
  schedStopPoll();
  const inAgent = typeof agentModeOn === 'function' && agentModeOn();
  if (inAgent && typeof agentRestoreView === 'function') agentRestoreView();
  const nv = $('#needs-view');
  const queueShown = !!(nv && !nv.classList.contains('hidden'));
  if (!queueShown) {
    if (state.current) {
      $('#chat-view').classList.remove('hidden');
      if (typeof switchChatTab === 'function') {
        const active = document.querySelector('.chat-tab.active');
        switchChatTab(active ? active.dataset.tab : 'chat');
      }
      if (inAgent && typeof agentOnSessionOpened === 'function') agentOnSessionOpened(state.current);
    } else {
      $('#empty-state').classList.remove('hidden');
    }
  }
  if (inAgent && !queueShown && !state.current && typeof pa === 'object' && pa) {
    pa.view = 'session';
    if (typeof paRenderNav === 'function') paRenderNav();
    if (typeof agentRenderCluster === 'function') agentRenderCluster();
  }
  if (typeof renderIcons === 'function') renderIcons();
}

function schedStartPoll() {
  schedStopPoll();
  sched.pollTimer = setInterval(() => {
    const view = $('#schedules-view');
    if (view && !view.classList.contains('hidden')) schedulesRefresh();
  }, 15000);
}

function schedStopPoll() {
  clearInterval(sched.pollTimer);
  sched.pollTimer = 0;
}

// ---------- 数据 ----------
async function schedulesRefresh() {
  await Promise.all([schedRefreshTasks(), schedRefreshHarness()]);
  schedRender();
  schedRenderHarness();
}

async function schedRefreshTasks() {
  if (sched.busy) return;
  sched.busy = true;
  try {
    const d = await api('/api/schedules');
    sched.tasks = d && Array.isArray(d.tasks) ? d.tasks : [];
    sched.loaded = true;
    schedShowError('');
  } catch (e) {
    // 失败保留旧数据：一次网络抖动不该把已经在列表里的任务清空。
    sched.loaded = true;
    schedShowError('读取定时任务失败：' + schedErrMsg(e));
  } finally {
    sched.busy = false;
  }
}

function schedMergeHarnessGroup(acc, g) {
  if (!g || !g.workspace) return;
  let hit = acc.find((x) => x.workspace === g.workspace);
  if (!hit) {
    hit = { workspace: g.workspace, container: g.container || 'workspace', task_count: 0, files: [] };
    acc.push(hit);
  }
  for (const f of g.files || []) {
    if (!hit.files.some((x) => x.path === f.path)) hit.files.push(f);
  }
  hit.task_count = hit.files.reduce((n, f) => n + ((f.tasks && f.tasks.length) || 0), 0);
}

async function schedRefreshHarness() {
  if (sched.harnessBusy) return;
  sched.harnessBusy = true;
  const groups = [];
  let err = '';
  const candidates = [];
  try {
    const base = await api('/api/schedules/harness');
    for (const g of (base && base.groups) || []) schedMergeHarnessGroup(groups, g);
    for (const p of (base && base.candidates) || []) if (!candidates.includes(p)) candidates.push(p);
    // Manually added paths are fetched separately; they do not require an
    // import first, so they survive even when no session cwd points at them.
    for (const p of sched.harnessExtra) {
      try {
        const d = await api('/api/schedules/harness?path=' + encodeURIComponent(p));
        for (const g of (d && d.groups) || []) schedMergeHarnessGroup(groups, g);
        for (const q of (d && d.candidates) || []) if (!candidates.includes(q)) candidates.push(q);
      } catch (e) {
        err = '读取 ' + p + ' 失败：' + schedErrMsg(e);
      }
    }
    sched.harness = {
      groups, candidates, files: [], error: err, loaded: true,
    };
    sched.harness.files = groups.reduce((out, g) => out.concat(g.files || []), []);
  } catch (e) {
    sched.harness.error = '读取 harness 失败：' + schedErrMsg(e);
    sched.harness.loaded = true;
  } finally {
    sched.harnessBusy = false;
  }
}

function schedCountText() {
  const total = sched.tasks.length;
  const on = sched.tasks.filter((t) => t.enabled).length;
  return total ? total + ' 个任务 · ' + on + ' 个启用' : '';
}

function schedBadge(text, cls, title) {
  const b = document.createElement('span');
  b.className = 'sched-badge' + (cls ? ' ' + cls : '');
  b.textContent = text;
  if (title) b.title = title;
  return b;
}

function schedAction(label, cls, onClick) {
  const b = document.createElement('button');
  b.type = 'button';
  b.className = 'sched-btn' + (cls ? ' ' + cls : '');
  b.textContent = label;
  b.addEventListener('click', onClick);
  return b;
}

function schedRow(task) {
  const row = document.createElement('div');
  row.className = 'sched-row' + (task.enabled ? '' : ' off');
  row.dataset.id = task.id;

  const main = document.createElement('div');
  main.className = 'sched-row-main';

  const head = document.createElement('div');
  head.className = 'sched-row-head';
  const name = document.createElement('span');
  name.className = 'sched-row-name';
  name.textContent = task.name || '(未命名)';
  const cron = document.createElement('code');
  cron.className = 'sched-cron';
  cron.textContent = task.cron || '';
  const stateTag = document.createElement('span');
  stateTag.className = 'sched-state ' + (task.enabled ? 'on' : 'off');
  stateTag.textContent = task.enabled ? '启用' : '暂停';
  head.append(name, cron, stateTag);
  if (task.max_runs > 0) {
    const done = task.run_count >= task.max_runs;
    head.append(schedBadge(done ? '一次性 · 已执行' : '一次性', done ? 'once done' : 'once',
      done ? '这次性任务已经执行完，不会再次触发；点「启用」会重置后重新排期' : '执行一次后自动停用'));
  }
  if (task.source === 'harness') {
    head.append(schedBadge('CLI 导入', 'source', '来源：harness ' + (task.source_id || '')));
  }

  const prompt = document.createElement('div');
  prompt.className = 'sched-row-prompt';
  prompt.textContent = schedFirstLine(task.prompt);
  prompt.title = task.prompt || '';

  const meta = document.createElement('div');
  meta.className = 'sched-row-meta';
  const target = document.createElement('span');
  target.className = 'sched-meta-item';
  target.textContent = '目标：' + (task.target_name || task.target_session || '—');
  target.title = task.target_address ? '地址名：' + task.target_address : (task.target_session || '');
  const last = document.createElement('span');
  last.className = 'sched-meta-item';
  const st = schedStatus(task);
  last.textContent = '上次：' + (task.last_run_at ? schedFmtTime(task.last_run_at) + ' ' + st.text : '未运行');
  if (st.tip) last.title = st.tip;
  const next = document.createElement('span');
  next.className = 'sched-meta-item';
  next.textContent = '下次：' + (task.enabled ? schedFmtTime(task.next_run_at) : '—');
  if (task.next_run_at) next.title = schedFmtFull(task.next_run_at);
  const runCount = document.createElement('span');
  runCount.className = 'sched-meta-item';
  runCount.textContent = '已执行：' + (task.run_count || 0) + (task.max_runs > 0 ? ' / ' + task.max_runs : ' 次');
  meta.append(target, last, next, runCount);
  if (st.cls) {
    const badge = document.createElement('span');
    badge.className = 'sched-status ' + st.cls;
    badge.textContent = st.text;
    if (st.tip) badge.title = st.tip;
    meta.append(badge);
  }

  main.append(head, prompt, meta);

  const actions = document.createElement('div');
  actions.className = 'sched-row-actions';
  const toggle = schedAction(task.enabled ? '暂停' : '启用', 'toggle', () => schedToggle(task, toggle));
  const run = schedAction('立即执行', 'run', () => schedRunNow(task, run));
  const edit = schedAction('编辑', '', () => schedOpenModal(task));
  const del = schedAction('删除', 'danger', () => schedDelete(task));
  actions.append(toggle, run, edit, del);

  row.append(main, actions);
  return row;
}

function schedRender() {
  const list = $('#sched-list');
  const count = $('#sched-count');
  if (count) count.textContent = schedCountText();
  if (!list) return;
  list.textContent = '';
  if (!sched.tasks.length) {
    const empty = document.createElement('div');
    empty.className = 'sched-empty';
    const title = document.createElement('b');
    title.textContent = sched.loaded ? '还没有定时任务' : '正在读取…';
    const desc = document.createElement('p');
    desc.textContent = sched.loaded
      ? '用一句自然语言（如「每个工作日 9 点巡检看板」）或 cron 表达式创建。到点把 prompt 发到指定会话。'
      : '';
    empty.append(title, desc);
    if (sched.loaded) {
      const cta = document.createElement('button');
      cta.type = 'button';
      cta.className = 'primary-btn';
      cta.textContent = '新建定时任务';
      cta.addEventListener('click', () => schedOpenModal(null));
      empty.append(cta);
    }
    list.append(empty);
    return;
  }
  const frag = document.createDocumentFragment();
  for (const task of sched.tasks) frag.append(schedRow(task));
  list.append(frag);
}

// ---------- harness（CLI）多 workspace 只读列表 ----------
function schedHarnessRow(t, filePath) {
  const row = document.createElement('div');
  row.className = 'sched-harness-row';
  row.dataset.id = t.id;

  const main = document.createElement('div');
  main.className = 'sched-harness-main';
  const head = document.createElement('div');
  head.className = 'sched-row-head';
  const name = document.createElement('span');
  name.className = 'sched-row-name';
  name.textContent = t.name || t.id;
  const cron = document.createElement('code');
  cron.className = 'sched-cron';
  cron.textContent = t.cron || '';
  head.append(name, cron);
  head.append(schedBadge(t.recurring ? '周期' : '一次性', t.recurring ? 'recurring' : 'once'));
  head.append(schedBadge('harness', 'source', 'source id：' + t.id));
  if (!t.valid) head.append(schedBadge('cron 无效', 'bad', t.error || ''));
  const prompt = document.createElement('div');
  prompt.className = 'sched-row-prompt';
  prompt.textContent = schedFirstLine(t.prompt);
  prompt.title = t.prompt || '';
  main.append(head, prompt);
  if (t.error) {
    const err = document.createElement('div');
    err.className = 'sched-harness-task-error';
    err.textContent = t.error;
    main.append(err);
  }

  const actions = document.createElement('div');
  actions.className = 'sched-row-actions';
  if (t.imported_id) {
    actions.append(schedBadge('已导入', 'imported', '已导入为 AgentDeck 任务 ' + t.imported_id));
  } else {
    const btn = schedAction('导入到 AgentDeck', 'run', () => schedImportHarness(t, filePath, btn));
    btn.disabled = !t.valid;
    actions.append(btn);
  }

  row.append(main, actions);
  return row;
}

function schedHarnessFile(f) {
  const box = document.createElement('div');
  box.className = 'sched-harness-file';
  box.dataset.path = f.path;

  const head = document.createElement('div');
  head.className = 'sched-harness-file-head';
  const path = document.createElement('code');
  path.className = 'sched-harness-file-path';
  const rel = f.workspace && f.path.startsWith(f.workspace + '/') ? f.path.slice(f.workspace.length + 1) : f.path;
  path.textContent = rel;
  path.title = f.path;
  head.append(path);
  if (f.empty) head.append(schedBadge('空', 'recurring', '文件存在，但里面没有任务'));
  if (f.error) head.append(schedBadge('读取失败', 'bad', f.error));
  const remove = document.createElement('button');
  remove.type = 'button';
  remove.className = 'sched-harness-remove';
  remove.textContent = '移除';
  remove.title = '从列表移除（只影响本机显示，不改 harness 文件）';
  remove.addEventListener('click', () => schedIgnoreHarnessPath(f.path));
  head.append(remove);
  box.append(head);

  if (f.error) {
    const err = document.createElement('div');
    err.className = 'sched-harness-task-error';
    err.textContent = f.error;
    box.append(err);
    return box;
  }
  if (!f.tasks || !f.tasks.length) {
    const empty = document.createElement('div');
    empty.className = 'sched-harness-file-empty';
    empty.textContent = '这个文件里没有任务';
    box.append(empty);
    return box;
  }
  for (const t of f.tasks) box.append(schedHarnessRow(t, f.path));
  return box;
}

function schedRenderHarness() {
  const list = $('#sched-harness-list');
  if (!list) return;
  const h = sched.harness;
  const errEl = $('#sched-harness-error');
  if (errEl) {
    errEl.textContent = h.error || '';
    errEl.classList.toggle('hidden', !h.error);
  }
  const restore = $('#sched-harness-restore');
  if (restore) {
    const n = sched.harnessIgnored.length;
    restore.textContent = '恢复已移除（' + n + '）';
    restore.classList.toggle('hidden', n === 0);
  }
  const ignored = new Set(sched.harnessIgnored);
  list.textContent = '';
  const groups = (h.groups || []).map((g) => ({
    ...g,
    files: (g.files || []).filter((f) => !ignored.has(f.path)),
  })).filter((g) => g.files.length);
  if (!groups.length) {
    const empty = document.createElement('div');
    empty.className = 'sched-harness-empty';
    empty.textContent = h.loaded
      ? '没有发现 harness 文件。已检查 mainAgent / 各会话 workspace 的 .claude/scheduled_tasks.json 和 ~/.claude。可点「手动添加路径」，或设置环境变量 CC_WEBUI_HARNESS_TASKS。'
      : '正在扫描 harness 文件…';
    list.append(empty);
    return;
  }
  for (const g of groups) {
    const group = document.createElement('div');
    group.className = 'sched-harness-group';
    const gh = document.createElement('div');
    gh.className = 'sched-harness-group-head';
    const ws = document.createElement('code');
    ws.className = 'sched-harness-workspace';
    ws.textContent = g.workspace;
    gh.append(ws);
    gh.append(schedBadge(g.container === 'global' ? '全局' : 'workspace', g.container === 'global' ? 'source' : 'recurring'));
    const n = g.files.reduce((sum, f) => sum + ((f.tasks && f.tasks.length) || 0), 0);
    gh.append(schedBadge(n + ' 个任务', 'recurring'));
    group.append(gh);
    for (const f of g.files) group.append(schedHarnessFile(f));
    list.append(group);
  }
}

function schedIgnoreHarnessPath(path) {
  if (!sched.harnessIgnored.includes(path)) sched.harnessIgnored.push(path);
  schedSavePaths(SCHED_HARNESS_IGNORED_KEY, sched.harnessIgnored);
  schedRenderHarness();
}

function schedRestoreHarnessPaths() {
  sched.harnessIgnored = [];
  schedSavePaths(SCHED_HARNESS_IGNORED_KEY, []);
  schedRefreshHarness().then(schedRenderHarness);
}

async function schedAddHarnessPath() {
  if (typeof prompt !== 'function') return;
  const raw = prompt('输入 .claude/scheduled_tasks.json 的绝对路径（或 ~/…）');
  if (!raw || !raw.trim()) return;
  const path = raw.trim();
  try {
    const d = await api('/api/schedules/harness?path=' + encodeURIComponent(path));
    const found = (d && d.files && d.files[0] && d.files[0].path) || '';
    if (!found) throw new Error('该路径下没有可读取的 harness 文件');
    if (!sched.harnessExtra.includes(found)) sched.harnessExtra.push(found);
    sched.harnessIgnored = sched.harnessIgnored.filter((p) => p !== found);
    schedSavePaths(SCHED_HARNESS_EXTRA_KEY, sched.harnessExtra);
    schedSavePaths(SCHED_HARNESS_IGNORED_KEY, sched.harnessIgnored);
    await schedRefreshHarness();
    schedRenderHarness();
  } catch (e) {
    const errEl = $('#sched-harness-error');
    if (errEl) {
      errEl.textContent = '添加路径失败：' + schedErrMsg(e);
      errEl.classList.remove('hidden');
    }
  }
}

async function schedImportHarness(t, filePath, btn) {
  btn.disabled = true;
  const old = btn.textContent;
  btn.textContent = '导入中…';
  try {
    await api('/api/schedules/import-harness', {
      method: 'POST',
      body: JSON.stringify({ path: filePath, id: t.id }),
    });
    await schedulesRefresh();
  } catch (e) {
    const errEl = $('#sched-harness-error');
    if (errEl) {
      errEl.textContent = '导入失败：' + schedErrMsg(e);
      errEl.classList.remove('hidden');
    }
  } finally {
    btn.disabled = false;
    btn.textContent = old;
  }
}

// ---------- 行内动作 ----------
async function schedToggle(task, btn) {
  btn.disabled = true;
  try {
    await api('/api/schedules/' + schedEnc(task.id) + '/enable', {
      method: 'POST',
      body: JSON.stringify({ enabled: !task.enabled }),
    });
    schedShowError('');
    await schedulesRefresh();
  } catch (e) {
    schedShowError('切换启用状态失败：' + schedErrMsg(e));
  } finally {
    btn.disabled = false;
  }
}

async function schedRunNow(task, btn) {
  btn.disabled = true;
  const old = btn.textContent;
  btn.textContent = '执行中…';
  try {
    await api('/api/schedules/' + schedEnc(task.id) + '/run', { method: 'POST' });
    schedShowError('');
  } catch (e) {
    schedShowError('立即执行失败：' + schedErrMsg(e));
  } finally {
    btn.disabled = false;
    btn.textContent = old;
    await schedulesRefresh();
  }
}

async function schedDelete(task) {
  if (typeof confirm === 'function' && !confirm('删除定时任务「' + (task.name || task.id) + '」？')) return;
  try {
    await api('/api/schedules/' + schedEnc(task.id), { method: 'DELETE' });
    schedShowError('');
    await schedulesRefresh();
  } catch (e) {
    schedShowError('删除失败：' + schedErrMsg(e));
  }
}

// ---------- 新建 / 编辑 ----------
function schedRenderExamples() {
  const box = $('#sched-cron-examples');
  if (!box) return;
  box.textContent = '';
  for (const ex of SCHED_EXAMPLES) {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'sched-example';
    b.textContent = ex.label;
    b.title = ex.cron;
    b.addEventListener('click', () => {
      const input = $('#sched-cron');
      if (!input) return;
      input.value = ex.cron;
      schedPreviewNow();
      input.focus();
    });
    box.append(b);
  }
}

function schedFillTargets(selected) {
  const sel = $('#sched-target');
  if (!sel) return;
  sel.textContent = '';
  const sessions = (state.sessions || []).filter((s) => s && !s.archived);
  sessions.sort((a, b) => (b.main_agent ? 1 : 0) - (a.main_agent ? 1 : 0));
  const seen = new Set();
  for (const s of sessions) {
    const opt = document.createElement('option');
    opt.value = s.id;
    let label = s.name || s.id;
    const extras = [];
    if (s.main_agent && !label.includes('主 Agent')) extras.push('主 Agent');
    if (s.peer_name && s.peer_name !== s.name) extras.push(s.peer_name);
    if (extras.length) label += ' · ' + extras.join(' · ');
    opt.textContent = label;
    sel.append(opt);
    seen.add(s.id);
  }
  if (selected && !seen.has(selected)) {
    const opt = document.createElement('option');
    opt.value = selected;
    opt.textContent = '（找不到该会话）' + selected;
    sel.append(opt);
  }
  if (!sessions.length && !selected) {
    const opt = document.createElement('option');
    opt.value = '';
    opt.textContent = '（没有可用会话）';
    sel.append(opt);
  }
  if (selected) sel.value = selected;
  else {
    const main = sessions.find((s) => s.main_agent);
    sel.value = main ? main.id : ((sessions[0] && sessions[0].id) || '');
  }
}

function schedEffectivePrompt() {
  const prompt = ($('#sched-prompt').value || '').trim();
  if (prompt) return prompt;
  if (sched.mode === 'nl') return ($('#sched-nl-text').value || '').trim();
  return '';
}

function schedUpdatePromptUI() {
  const eff = schedEffectivePrompt();
  const finalText = $('#sched-final-text');
  if (finalText) finalText.textContent = eff || '（还没有内容；填写 prompt 或描述后显示）';
  const hint = $('#sched-prompt-hint');
  if (hint) {
    const empty = !($('#sched-prompt').value || '').trim();
    hint.classList.toggle('hidden', !(empty && sched.mode === 'nl'));
  }
}

function schedResetAIOutput() {
  const meta = $('#sched-ai-meta');
  if (meta) {
    meta.textContent = '';
    meta.title = '';
    meta.classList.add('hidden');
    meta.classList.remove('warn');
  }
  const rationale = $('#sched-ai-rationale');
  if (rationale) {
    rationale.textContent = '';
    rationale.classList.add('hidden');
  }
  const questions = $('#sched-ai-questions');
  if (questions) {
    questions.textContent = '';
    questions.classList.add('hidden');
  }
}

function schedSetParser(parser) {
  const next = parser === 'ai' ? 'ai' : 'rules';
  if (sched.parser !== next) {
    sched.candidates = [];
    sched.selectedCron = '';
    schedRenderCandidates();
    schedResetAIOutput();
  }
  sched.parser = next;
  document.querySelectorAll('#sched-parser [data-sched-parser]').forEach((b) => {
    b.classList.toggle('active', b.dataset.schedParser === next);
  });
  const btn = $('#sched-parse');
  if (btn && !sched.aiBusy) btn.textContent = next === 'ai' ? 'AI 解析' : '解析';
  const note = $('#sched-parse-note');
  if (note) {
    note.textContent = next === 'ai'
      ? '会读 mainAgent 的 memory / tracking，较慢（约 1–2 分钟）'
      : '由内置规则解析（快、离线、不耗 token）';
  }
  const aiNote = $('#sched-ai-note');
  if (aiNote) aiNote.classList.toggle('hidden', next !== 'ai');
}

function schedShowAIQuestions(qs) {
  const el = $('#sched-ai-questions');
  if (!el) return;
  el.textContent = '';
  if (!qs || !qs.length) {
    el.classList.add('hidden');
    return;
  }
  const title = document.createElement('b');
  title.textContent = 'AI 需要你确认：';
  const list = document.createElement('ul');
  for (const q of qs) {
    const li = document.createElement('li');
    li.textContent = q;
    list.append(li);
  }
  el.append(title, list);
  el.classList.remove('hidden');
}

function schedRenderAIResponse(d, text) {
  const meta = $('#sched-ai-meta');
  const note = $('#sched-parse-note');
  const reads = Array.isArray(d.context_reads) ? d.context_reads : [];
  const bits = [];
  if (typeof d.elapsed_ms === 'number') bits.push((d.elapsed_ms / 1000).toFixed(1) + 's');
  if (d.usage) bits.push('tokens ' + ((d.usage.input_tokens || 0) + (d.usage.output_tokens || 0)));
  if (reads.length) bits.push('读 ' + reads.length + ' 处上下文');
  if (meta) {
    if (d.fallback) {
      meta.textContent = 'AI 解析失败，已用规则结果：' + (d.reason || '未知原因') +
        (bits.length ? ' · ' + bits.join(' · ') : '');
      meta.classList.add('warn');
    } else {
      meta.textContent = 'AI 解析完成' + (bits.length ? ' · ' + bits.join(' · ') : '');
      meta.classList.remove('warn');
    }
    meta.title = reads.length ? '读取：\n' + reads.join('\n') : '';
    meta.classList.toggle('hidden', !meta.textContent);
  }
  const proposal = d.proposal || {};
  const rationale = $('#sched-ai-rationale');
  if (rationale) {
    const r = String(proposal.rationale || '').trim();
    const readsLine = reads.length ? '读取：' + reads.join('、') : '';
    const text = [r ? 'AI 理由：' + r : '', readsLine].filter(Boolean).join('\n');
    rationale.textContent = text;
    rationale.classList.toggle('hidden', !text);
  }
  schedShowAIQuestions(proposal.questions || []);

  if (d.fallback) {
    sched.candidates = (Array.isArray(d.candidates) ? d.candidates : []).map((c) => ({ ...c, ai: false }));
    sched.selectedCron = '';
    schedRenderCandidates();
    if (sched.candidates.length === 1) schedSelectCandidate(sched.candidates[0]);
    if (note) note.textContent = d.mode === 'none'
      ? 'AI 解析失败，规则也无法解析；请补充时间/频率'
      : 'AI 解析失败，已用规则结果；确认候选后保存';
    if (d.mode === 'none') schedParseError(d.reason || 'AI 与规则都无法解析这段描述');
    return;
  }
  if (d.needs_clarification) {
    if (note) note.textContent = 'AI 需要你补充信息后再解析';
    return;
  }
  if (!proposal.cron) {
    if (note) note.textContent = 'AI 没有给出可用的 cron';
    return;
  }
  sched.candidates = [{
    cron: proposal.cron,
    label: 'AI 建议',
    name: proposal.name,
    next_run_at: proposal.next_run_at,
    next_run_local: proposal.next_run_local,
    ai: true,
  }];
  sched.selectedCron = '';
  schedRenderCandidates();
  const name = $('#sched-name');
  if (name && proposal.name) name.value = proposal.name;
  const promptEl = $('#sched-prompt');
  if (promptEl && proposal.prompt) {
    promptEl.value = proposal.prompt;
    sched.autoPrompt = '';
  }
  schedSelectCandidate(sched.candidates[0]);
  schedUpdatePromptUI();
  if (note) note.textContent = 'AI 建议已生成，确认候选后保存';
}

async function schedParseAI(text) {
  if (sched.aiBusy) return;
  const btn = $('#sched-parse');
  const note = $('#sched-parse-note');
  sched.aiBusy = true;
  btn.disabled = true;
  btn.textContent = 'AI 解析中…';
  schedParseError('');
  schedResetAIOutput();
  schedClearCandidates();
  if (note) note.textContent = '正在 mainAgent 工作区解析，可能 1–2 分钟…';
  try {
    const d = await api('/api/schedules/parse-ai', { method: 'POST', body: JSON.stringify({ text }) });
    schedRenderAIResponse(d, text);
  } catch (e) {
    schedParseError('AI 解析失败：' + schedErrMsg(e));
    if (note) note.textContent = 'AI 解析失败；可重试或切回规则解析';
  } finally {
    sched.aiBusy = false;
    btn.disabled = false;
    btn.textContent = sched.parser === 'ai' ? 'AI 解析' : '解析';
  }
}

function schedSetMode(mode) {
  sched.mode = mode === 'cron' ? 'cron' : 'nl';
  document.querySelectorAll('#sched-mode [data-sched-mode]').forEach((b) => {
    b.classList.toggle('active', b.dataset.schedMode === sched.mode);
  });
  const nl = $('#sched-nl-block');
  const cron = $('#sched-cron-block');
  if (nl) nl.classList.toggle('hidden', sched.mode !== 'nl');
  if (cron) cron.classList.toggle('hidden', sched.mode !== 'cron');
  if (sched.mode === 'cron') schedPreviewNow();
  else if (!sched.selectedCron) {
    const el = $('#sched-next-preview');
    if (el) {
      el.textContent = '填写描述并点「解析」，再选择一个候选';
      el.classList.remove('bad');
    }
  }
  schedUpdatePromptUI();
}

function schedClearCandidates() {
  sched.candidates = [];
  sched.selectedCron = '';
  schedRenderCandidates();
}

function schedRenderCandidates() {
  const box = $('#sched-candidates');
  if (!box) return;
  box.textContent = '';
  if (!sched.candidates.length) {
    box.classList.add('hidden');
    return;
  }
  box.classList.remove('hidden');
  for (const c of sched.candidates) {
    const label = document.createElement('label');
    label.className = 'sched-candidate' + (sched.selectedCron === c.cron ? ' on' : '');
    const radio = document.createElement('input');
    radio.type = 'radio';
    radio.name = 'sched-candidate';
    radio.value = c.cron;
    radio.checked = sched.selectedCron === c.cron;
    radio.addEventListener('change', () => schedSelectCandidate(c));
    const body = document.createElement('div');
    body.className = 'sched-candidate-body';
    const top = document.createElement('div');
    top.className = 'sched-candidate-top';
    const lab = document.createElement('b');
    lab.textContent = c.label || c.cron;
    const code = document.createElement('code');
    code.textContent = c.cron;
    top.append(lab, code);
    if (c.ai) top.append(schedBadge('AI', 'source', '由 AI 解析生成，已通过 cron 校验'));
    const next = document.createElement('span');
    next.className = 'sched-candidate-next';
    next.textContent = '下次执行：' + (c.next_run_local || c.next_run_at || '—');
    body.append(top, next);
    label.append(radio, body);
    box.append(label);
  }
}

function schedSelectCandidate(c) {
  sched.selectedCron = c.cron;
  const cron = $('#sched-cron');
  if (cron) cron.value = c.cron;
  const name = $('#sched-name');
  if (name && !name.value.trim() && c.name) name.value = c.name;
  schedRenderCandidates();
  // 解析成功后描述已填进 prompt；这里只在它仍为空时兜底一次。
  const promptEl = $('#sched-prompt');
  if (promptEl && !promptEl.value.trim()) {
    const text = ($('#sched-nl-text').value || '').trim();
    if (text) {
      promptEl.value = text;
      sched.autoPrompt = text;
    }
  }
  schedUpdatePromptUI();
  // 候选自带服务端算好的 next；再走一次 preview 接口是为了复用同一条校验路径。
  const el = $('#sched-next-preview');
  if (el && (c.next_run_local || c.next_run_at)) {
    el.textContent = '下次执行：' + (c.next_run_local || c.next_run_at);
    el.classList.remove('bad');
  }
  schedPreviewNow();
}

async function schedParse() {
  if (sched.parsing || sched.aiBusy) return;
  const text = ($('#sched-nl-text').value || '').trim();
  if (!text) {
    schedParseError('描述不能为空');
    return;
  }
  if (sched.parser === 'ai') {
    await schedParseAI(text);
    return;
  }
  const btn = $('#sched-parse');
  const old = btn.textContent;
  sched.parsing = true;
  btn.disabled = true;
  btn.textContent = '解析中…';
  schedParseError('');
  try {
    const d = await api('/api/schedules/parse', { method: 'POST', body: JSON.stringify({ text }) });
    sched.candidates = Array.isArray(d.candidates) ? d.candidates : [];
    sched.selectedCron = '';
    schedRenderCandidates();
    const note = $('#sched-parse-note');
    if (note) {
      const how = d.mode === 'rules' ? '内置规则解析' : '模型解析';
      note.textContent = how + ' · ' + sched.candidates.length + ' 个候选，选一个后保存';
    }
    if (sched.candidates.length === 1) schedSelectCandidate(sched.candidates[0]);
    // Show what will actually be sent: the description becomes the initial
    // prompt (still editable; clearing it falls back to the description).
    const promptEl = $('#sched-prompt');
    if (promptEl && !promptEl.value.trim()) {
      promptEl.value = text;
      sched.autoPrompt = text;
    }
    schedUpdatePromptUI();
  } catch (e) {
    schedClearCandidates();
    schedParseError(schedErrMsg(e));
    const note = $('#sched-parse-note');
    if (note) note.textContent = '解析失败；可补充时间/频率，或切到 cron 模式手写';
  } finally {
    sched.parsing = false;
    btn.disabled = false;
    btn.textContent = old;
  }
}

function schedOpenModal(task) {
  sched.editing = task ? task.id : '';
  const title = $('#sched-modal-title');
  if (title) title.textContent = task ? '编辑定时任务' : '新建定时任务';
  $('#sched-name').value = task ? (task.name || '') : '';
  $('#sched-cron').value = task ? (task.cron || '') : '0 9 * * *';
  $('#sched-prompt').value = task ? (task.prompt || '') : '';
  $('#sched-nl-text').value = '';
  sched.autoPrompt = '';
  $('#sched-enabled').checked = task ? !!task.enabled : true;
  const parseNote = $('#sched-parse-note');
  if (parseNote) parseNote.textContent = '由内置规则解析（快、离线、不耗 token）';
  schedParseError('');
  schedResetAIOutput();
  sched.parser = 'rules';
  document.querySelectorAll('#sched-parser [data-sched-parser]').forEach((b) => {
    b.classList.toggle('active', b.dataset.schedParser === 'rules');
  });
  schedSetParser('rules');
  schedClearCandidates();
  schedFillTargets(task ? task.target_session : '');
  schedRenderExamples();
  schedModalError('');
  const preview = $('#sched-next-preview');
  if (preview) {
    preview.textContent = task ? '正在计算下次执行…' : '填写描述并点「解析」，再选择一个候选';
    preview.classList.remove('bad');
  }
  // 编辑已有任务默认看 cron；新建默认自然语言（用户反馈不要手写 cron）。
  schedSetMode(task ? 'cron' : 'nl');
  if (typeof openModal === 'function') openModal('#modal-schedule');
  else $('#modal-schedule').classList.remove('hidden');
  setTimeout(() => {
    const el = task ? $('#sched-cron') : $('#sched-nl-text');
    if (el) el.focus();
  }, 0);
}

function schedCloseModal() {
  const modal = $('#modal-schedule');
  if (modal) modal.classList.add('hidden');
  sched.editing = '';
  sched.candidates = [];
  sched.selectedCron = '';
  sched.parsing = false;
  sched.autoPrompt = '';
  clearTimeout(sched.previewTimer);
  sched.previewTimer = 0;
  schedModalError('');
  schedParseError('');
}

function schedPreviewNow() {
  clearTimeout(sched.previewTimer);
  schedPreview();
}

async function schedPreview() {
  const el = $('#sched-next-preview');
  const input = $('#sched-cron');
  if (!el || !input) return;
  const cron = input.value.trim();
  if (!cron) {
    el.textContent = '输入合法 cron 后显示下次执行时间';
    el.classList.remove('bad');
    return;
  }
  const seq = ++sched.previewSeq;
  try {
    const d = await api('/api/schedules/preview', { method: 'POST', body: JSON.stringify({ cron }) });
    if (seq !== sched.previewSeq) return;
    el.textContent = '下次执行：' + (d.next_run_local || d.next_run_at || '—') +
      (d.timezone ? '（' + d.timezone + '）' : '');
    el.classList.remove('bad');
  } catch (e) {
    if (seq !== sched.previewSeq) return;
    el.textContent = 'cron 无效：' + schedErrMsg(e);
    el.classList.add('bad');
  }
}

async function schedSubmit() {
  const nlText = ($('#sched-nl-text').value || '').trim();
  let name = ($('#sched-name').value || '').trim();
  let prompt = ($('#sched-prompt').value || '').trim();
  let cron = ($('#sched-cron').value || '').trim();
  const target = $('#sched-target').value || '';
  const enabled = !!$('#sched-enabled').checked;

  if (sched.mode === 'nl') {
    if (!sched.selectedCron) {
      schedModalError('先填写描述并点「解析」，再选择一个候选');
      return;
    }
    cron = sched.selectedCron;
    if (!prompt) prompt = nlText; // 描述本身可作为 prompt
    if (!name) {
      const picked = sched.candidates.find((c) => c.cron === sched.selectedCron);
      name = (picked && picked.name) || schedFirstLine(nlText);
    }
  }
  if (!name) name = schedFirstLine(nlText) || '定时任务';
  if (!cron) { schedModalError('cron 不能为空'); return; }
  if (!prompt) { schedModalError('prompt 不能为空（自然语言模式会使用描述）'); return; }
  if (!target) { schedModalError('没有可用的目标会话'); return; }

  const btn = $('#sched-save');
  const old = btn.textContent;
  btn.disabled = true;
  btn.textContent = '保存中…';
  schedModalError('');
  try {
    const body = JSON.stringify({ name, cron, prompt, target_session: target, enabled });
    if (sched.editing) {
      await api('/api/schedules/' + schedEnc(sched.editing), { method: 'PUT', body });
    } else {
      await api('/api/schedules', { method: 'POST', body });
    }
    schedCloseModal();
    schedShowError('');
    await schedulesRefresh();
  } catch (e) {
    schedModalError('保存失败：' + schedErrMsg(e));
  } finally {
    btn.disabled = false;
    btn.textContent = old;
  }
}

// ---------- 绑定 ----------
function schedBind() {
  const back = $('#sched-back');
  if (back) back.addEventListener('click', schedulesClose);
  const refresh = $('#sched-refresh');
  if (refresh) refresh.addEventListener('click', () => schedulesRefresh());
  const create = $('#sched-new');
  if (create) create.addEventListener('click', () => schedOpenModal(null));

  const harnessAdd = $('#sched-harness-add');
  if (harnessAdd) harnessAdd.addEventListener('click', schedAddHarnessPath);
  const harnessRestore = $('#sched-harness-restore');
  if (harnessRestore) harnessRestore.addEventListener('click', schedRestoreHarnessPaths);

  document.querySelectorAll('#sched-mode [data-sched-mode]').forEach((b) => {
    b.addEventListener('click', () => schedSetMode(b.dataset.schedMode));
  });
  document.querySelectorAll('#sched-parser [data-sched-parser]').forEach((b) => {
    b.addEventListener('click', () => schedSetParser(b.dataset.schedParser));
  });
  const parse = $('#sched-parse');
  if (parse) parse.addEventListener('click', schedParse);
  const nlText = $('#sched-nl-text');
  if (nlText) nlText.addEventListener('input', () => {
    const text = nlText.value;
    const promptEl = $('#sched-prompt');
    if (promptEl) {
      // 描述自动填的 prompt 跟着描述走；用户手改过后就不再覆盖。
      if (!promptEl.value.trim() || promptEl.value.trim() === sched.autoPrompt) {
        promptEl.value = text;
        sched.autoPrompt = text;
      } else {
        sched.autoPrompt = '';
      }
    }
    if (sched.candidates.length || sched.selectedCron) {
      schedClearCandidates();
      const note = $('#sched-parse-note');
      if (note) note.textContent = '描述已修改，请重新解析';
      const preview = $('#sched-next-preview');
      if (preview) { preview.textContent = '描述已修改，请重新解析'; preview.classList.remove('bad'); }
    }
    schedResetAIOutput();
    schedUpdatePromptUI();
  });
  const promptEl = $('#sched-prompt');
  if (promptEl) promptEl.addEventListener('input', () => {
    sched.autoPrompt = '';
    schedUpdatePromptUI();
  });

  const cron = $('#sched-cron');
  if (cron) cron.addEventListener('input', () => {
    if (sched.mode === 'cron') {
      clearTimeout(sched.previewTimer);
      sched.previewTimer = setTimeout(schedPreview, 300);
    }
  });
  const save = $('#sched-save');
  if (save) save.addEventListener('click', schedSubmit);
  const modal = $('#modal-schedule');
  if (modal) {
    modal.querySelectorAll('[data-close]').forEach((b) => b.addEventListener('click', schedCloseModal));
    // 点遮罩关闭（data-close 由 app.js 的全局点击绑定处理）。
    modal.addEventListener('mousedown', (e) => {
      if (e.target === modal) schedCloseModal();
    });
    modal.addEventListener('keydown', (e) => {
      if (e.key !== 'Enter' || e.isComposing) return;
      if (!e.target || e.target.tagName !== 'INPUT' || e.target.type === 'checkbox') return;
      e.preventDefault();
      schedSubmit();
    });
  }
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && modal && !modal.classList.contains('hidden')) {
      e.preventDefault();
      schedCloseModal();
    }
  });
}

if (window.__schedSkipInit) { /* 测试里自己调 schedBind，避免和桩的初始化顺序打架 */ }
else if (typeof document !== 'undefined') {
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', schedBind);
  else schedBind();
}
