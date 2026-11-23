/* AgentDeck · 定时任务页（schedules.js）
 *
 * 管理 AgentDeck 自己的 cron 任务：<data_dir>/schedules.json，到点通过 A2A 总线
 * 把 prompt 投给目标会话（见 internal/server/schedules.go）。
 *
 * 注意：Claude Code harness 的 CronCreate（.claude/scheduled_tasks.json）是另一套
 * 机制，这里只显示一条只读说明，不导入、不混写、也不会触发它们。
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

const sched = {
  tasks: [],
  loaded: false,
  busy: false,
  lastError: '',
  editing: '',      // 正在编辑的任务 id（空 = 新建）
  previewSeq: 0,
  pollTimer: 0,
  previewTimer: 0,
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

// ---------- 页面开关 ----------
function schedulesOpen() {
  const view = $('#schedules-view');
  if (!view) return;
  if (typeof hideAllViews === 'function') hideAllViews();
  if (typeof boardDisconnect === 'function') boardDisconnect();
  view.classList.remove('hidden');
  if (typeof renderIcons === 'function') renderIcons();
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

// ---------- 列表 ----------
async function schedulesRefresh() {
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
  schedRender();
}

function schedCountText() {
  const total = sched.tasks.length;
  const on = sched.tasks.filter((t) => t.enabled).length;
  return total ? total + ' 个任务 · ' + on + ' 个启用' : '';
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
  meta.append(target, last, next);
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
      ? '到点把一段 prompt 发到指定会话。适合每日巡检、定时提醒、周期性跑测试。'
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

function schedOpenModal(task) {
  sched.editing = task ? task.id : '';
  const title = $('#sched-modal-title');
  if (title) title.textContent = task ? '编辑定时任务' : '新建定时任务';
  $('#sched-name').value = task ? (task.name || '') : '';
  $('#sched-cron').value = task ? (task.cron || '') : '0 9 * * *';
  $('#sched-prompt').value = task ? (task.prompt || '') : '';
  $('#sched-enabled').checked = task ? !!task.enabled : true;
  schedFillTargets(task ? task.target_session : '');
  schedRenderExamples();
  schedModalError('');
  const preview = $('#sched-next-preview');
  if (preview) {
    preview.textContent = '输入合法 cron 后显示下次执行时间';
    preview.classList.remove('bad');
  }
  if (typeof openModal === 'function') openModal('#modal-schedule');
  else $('#modal-schedule').classList.remove('hidden');
  schedPreviewNow();
  setTimeout(() => { const el = $('#sched-name'); if (el) el.focus(); }, 0);
}

function schedCloseModal() {
  const modal = $('#modal-schedule');
  if (modal) modal.classList.add('hidden');
  sched.editing = '';
  clearTimeout(sched.previewTimer);
  sched.previewTimer = 0;
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
  const name = ($('#sched-name').value || '').trim();
  const cron = ($('#sched-cron').value || '').trim();
  const prompt = ($('#sched-prompt').value || '').trim();
  const target = $('#sched-target').value || '';
  const enabled = !!$('#sched-enabled').checked;
  if (!name) { schedModalError('名称不能为空'); return; }
  if (!cron) { schedModalError('cron 不能为空'); return; }
  if (!prompt) { schedModalError('prompt 不能为空'); return; }
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
  const cron = $('#sched-cron');
  if (cron) cron.addEventListener('input', () => {
    clearTimeout(sched.previewTimer);
    sched.previewTimer = setTimeout(schedPreview, 300);
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
