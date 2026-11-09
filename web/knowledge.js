// 资料空间（知识库）P0：workspace 级列表/搜索/详情/审批。
// 依赖 app.js 的全局 $ / api / md / state / openModal / closeModal /
// boardShow / boardOpenDrawer / switchChatTab。board drawer 的「相关知识」
// 通过 renderBoardKnowledge() 由 app.js 回填。
const kbState = {
  entries: [],
  total: 0,
  counts: {},
  projects: [],
  q: '',
  project: '',
  status: 'all',
  sourceTask: '',
  selectedId: null,
  loading: false,
  requestSeq: 0,
  returnView: 'empty',
  pending: false,
};

function kbEl(sel) {
  return document.querySelector(sel);
}

function kbStatusLabel(status) {
  return {
    draft: '草稿',
    approved: '已通过',
    stale: '可能过期',
    superseded: '已替代',
    rejected: '已驳回',
    quarantined: '已隔离',
  }[status] || status || '未知';
}

function kbStatusClass(status) {
  return 'kb-badge kb-badge-' + (status || 'unknown');
}

function kbSourceLabel(kind) {
  return {
    manual: '手工',
    collect: '看板采集',
    worker_report: 'worker 回报',
    task_done: '任务完成',
    decision: '用户决策',
    digest: '周报',
    import: '导入',
  }[kind] || kind || '—';
}

function kbStatusBadge(status) {
  const span = document.createElement('span');
  span.className = kbStatusClass(status);
  span.textContent = kbStatusLabel(status);
  return span;
}

function kbGeneratedBadge(entry) {
  if (!entry || entry.generated_by === 'human') return null;
  const span = document.createElement('span');
  span.className = 'kb-badge kb-badge-ai';
  span.textContent = entry.generated_by === 'system' ? '自动' : 'AI 摘要';
  if (entry.ai_fields && entry.ai_fields.length) {
    span.title = 'AI 生成字段：' + entry.ai_fields.join(', ');
  }
  return span;
}

function kbMuted(text) {
  const div = document.createElement('div');
  div.className = 'kb-muted';
  div.textContent = text;
  return div;
}

function kbAction(label, onClick, cls) {
  const button = document.createElement('button');
  button.type = 'button';
  button.className = 'kb-action' + (cls ? ' ' + cls : '');
  button.textContent = label;
  button.onclick = onClick;
  return button;
}

function kbCopyText(text) {
  if (!text) return;
  if (navigator.clipboard) {
    navigator.clipboard.writeText(String(text)).catch(() => {});
  }
}

function kbCountText() {
  const counts = kbState.counts || {};
  const parts = [];
  if (counts.approved) parts.push('已通过 ' + counts.approved);
  if (counts.draft) parts.push('草稿 ' + counts.draft);
  if (counts.stale) parts.push('可能过期 ' + counts.stale);
  if (counts.quarantined) parts.push('隔离 ' + counts.quarantined);
  return parts.join(' · ') || '暂无知识';
}

function kbUpdateFacets() {
  const counts = kbEl('#knowledge-counts');
  if (counts) counts.textContent = kbCountText();
  const sel = kbEl('#knowledge-project');
  if (sel) {
    const current = kbState.project;
    sel.innerHTML = '';
    sel.append(new Option('全部项目', ''));
    (kbState.projects || []).forEach((project) => sel.append(new Option(project, project)));
    sel.value = (kbState.projects || []).includes(current) ? current : '';
    kbState.project = sel.value;
  }
  const taskChip = kbEl('#knowledge-task-chip');
  if (taskChip) {
    if (kbState.sourceTask) {
      taskChip.classList.remove('hidden');
      taskChip.textContent = '来源任务 ' + kbState.sourceTask + ' ×';
      taskChip.title = '清除来源任务过滤';
      taskChip.onclick = () => {
        kbState.sourceTask = '';
        kbUpdateTaskChip();
        kbLoad();
      };
    } else {
      taskChip.classList.add('hidden');
      taskChip.textContent = '';
      taskChip.onclick = null;
    }
  }
}

function kbUpdateTaskChip() {
  kbUpdateFacets();
}

function kbShowEmpty(message) {
  const empty = kbEl('#knowledge-empty');
  const list = kbEl('#knowledge-list');
  if (list) list.innerHTML = '';
  if (empty) {
    empty.textContent = message;
    empty.classList.remove('hidden');
  }
}

function kbRenderList() {
  const list = kbEl('#knowledge-list');
  const empty = kbEl('#knowledge-empty');
  if (!list) return;
  list.innerHTML = '';
  if (!kbState.entries.length) {
    kbShowEmpty(kbState.q
      ? `没有匹配「${kbState.q}」的知识。试试更短的关键词，或放宽项目/状态筛选。`
      : '还没有知识。完成任务后可从看板采集草稿，也可以点「新建知识」。');
    return;
  }
  if (empty) empty.classList.add('hidden');
  const total = document.createElement('div');
  total.className = 'kb-total';
  total.textContent = `结果 ${kbState.total} 条`;
  list.append(total);
  kbState.entries.forEach((entry) => list.append(kbRenderItem(entry)));
}

function kbRenderItem(entry) {
  const item = document.createElement('article');
  item.className = 'kb-item' + (entry.id === kbState.selectedId ? ' selected' : '');
  item.dataset.entryId = entry.id;
  item.tabIndex = 0;
  item.setAttribute('role', 'button');
  item.setAttribute('aria-label', '查看知识 ' + (entry.title || entry.id));

  const head = document.createElement('div');
  head.className = 'kb-item-head';
  head.append(kbStatusBadge(entry.status));
  const generated = kbGeneratedBadge(entry);
  if (generated) head.append(generated);
  if (entry.project) {
    const project = document.createElement('span');
    project.className = 'kb-project';
    project.textContent = entry.project;
    head.append(project);
  }
  const updated = document.createElement('span');
  updated.className = 'kb-updated';
  updated.textContent = entry.updated_at || '';
  head.append(updated);

  const title = document.createElement('div');
  title.className = 'kb-item-title';
  title.textContent = entry.title || '（无标题）';
  const summary = document.createElement('div');
  summary.className = 'kb-item-summary';
  summary.textContent = entry.snippet || entry.summary || '';

  item.append(head, title, summary);
  if (Array.isArray(entry.tags) && entry.tags.length) {
    const tags = document.createElement('div');
    tags.className = 'kb-tags';
    entry.tags.forEach((tag) => {
      const chip = document.createElement('span');
      chip.className = 'kb-tag';
      chip.textContent = tag;
      tags.append(chip);
    });
    item.append(tags);
  }
  item.onclick = () => kbOpenDetail(entry.id);
  item.onkeydown = (event) => {
    if (event.key === 'Enter' || event.key === ' ') {
      event.preventDefault();
      kbOpenDetail(entry.id);
    }
  };
  return item;
}

async function kbLoad() {
  const seq = ++kbState.requestSeq;
  kbState.loading = true;
  const params = new URLSearchParams();
  if (kbState.q.trim()) params.set('q', kbState.q.trim());
  if (kbState.project) params.set('project', kbState.project);
  if (kbState.status) params.set('status', kbState.status);
  if (kbState.sourceTask) params.set('source_task_id', kbState.sourceTask);
  params.set('limit', '50');
  try {
    const data = await api('/api/knowledge?' + params.toString());
    if (seq !== kbState.requestSeq) return;
    kbState.entries = Array.isArray(data.entries) ? data.entries : [];
    kbState.total = Number(data.total) || 0;
    kbState.counts = data.counts || {};
    kbState.projects = Array.isArray(data.projects) ? data.projects : [];
    kbUpdateFacets();
    kbRenderList();
    if (kbState.selectedId) kbOpenDetail(kbState.selectedId, true);
  } catch (err) {
    if (seq !== kbState.requestSeq) return;
    kbShowEmpty('读取知识失败：' + (err && err.message ? err.message : err));
  } finally {
    if (seq === kbState.requestSeq) kbState.loading = false;
  }
}

function kbDetailEls() {
  return {
    detail: kbEl('#knowledge-detail'),
    kicker: kbEl('#knowledge-detail-kicker'),
    title: kbEl('#knowledge-detail-title'),
    meta: kbEl('#knowledge-detail-meta'),
    actions: kbEl('#knowledge-detail-actions'),
    body: kbEl('#knowledge-detail-body'),
    sources: kbEl('#knowledge-detail-sources'),
    relations: kbEl('#knowledge-detail-relations'),
    audit: kbEl('#knowledge-detail-audit'),
  };
}

function kbCloseDetail() {
  const el = kbEl('#knowledge-detail');
  if (el) el.classList.remove('open');
  kbState.selectedId = null;
  document.querySelectorAll('.kb-item.selected').forEach((item) => item.classList.remove('selected'));
}

function kbHighlightSelected() {
  document.querySelectorAll('.kb-item').forEach((item) => {
    item.classList.toggle('selected', item.dataset.entryId === kbState.selectedId);
  });
}

async function kbOpenDetail(id, keep) {
  if (!id) return;
  kbState.selectedId = id;
  kbHighlightSelected();
  const els = kbDetailEls();
  if (!els.detail) return;
  els.detail.classList.remove('hidden');
  els.detail.classList.add('open');
  if (!keep) {
    els.title.textContent = '加载中…';
    els.meta.textContent = '';
    els.actions.innerHTML = '';
    els.body.innerHTML = '';
    els.sources.innerHTML = '';
    els.relations.innerHTML = '';
    els.audit.innerHTML = '';
  }
  try {
    const data = await api('/api/knowledge/' + encodeURIComponent(id));
    if (kbState.selectedId !== id) return;
    kbRenderDetail(data.entry);
  } catch (err) {
    if (kbState.selectedId !== id) return;
    els.title.textContent = '读取失败';
    els.meta.textContent = err && err.message ? err.message : String(err);
  }
}

function kbMetaLine(label, value, mono) {
  if (value == null || value === '') return null;
  const row = document.createElement('div');
  row.className = 'kb-meta-row';
  const key = document.createElement('span');
  key.className = 'kb-meta-key';
  key.textContent = label;
  const val = document.createElement('span');
  val.className = 'kb-meta-value' + (mono ? ' mono' : '');
  val.textContent = value;
  val.title = value;
  row.append(key, val);
  return row;
}

function kbRenderDetail(entry) {
  if (!entry) return;
  const els = kbDetailEls();
  els.kicker.innerHTML = '';
  els.kicker.append(kbStatusBadge(entry.status));
  if (entry.id) {
    const id = document.createElement('span');
    id.className = 'kb-id';
    id.textContent = entry.id;
    id.title = '点击复制';
    id.onclick = () => kbCopyText(entry.id);
    els.kicker.append(id);
  }
  const generated = kbGeneratedBadge(entry);
  if (generated) els.kicker.append(generated);
  els.title.textContent = entry.title || '（无标题）';

  els.meta.innerHTML = '';
  [
    ['项目', entry.project],
    ['负责人', entry.owner],
    ['置信度', entry.confidence != null ? String(entry.confidence) : ''],
    ['来源', kbSourceLabel(entry.source_kind)],
    ['更新', entry.updated_at],
    ['最后验证', entry.last_verified_at],
    ['审核人', entry.reviewer],
    ['审核原因', entry.review_reason],
    ['版本', entry.version != null ? 'v' + entry.version : ''],
  ].forEach(([label, value]) => {
    const row = kbMetaLine(label, value);
    if (row) els.meta.append(row);
  });
  if (Array.isArray(entry.tags) && entry.tags.length) {
    const tags = document.createElement('div');
    tags.className = 'kb-tags';
    entry.tags.forEach((tag) => {
      const chip = document.createElement('span');
      chip.className = 'kb-tag';
      chip.textContent = tag;
      tags.append(chip);
    });
    els.meta.append(tags);
  }

  els.actions.innerHTML = '';
  const actions = [];
  if (entry.status === 'draft') {
    actions.push(kbAction('通过', () => kbReview(entry.id, 'approve'), 'primary'));
    actions.push(kbAction('驳回', () => kbReview(entry.id, 'reject'), 'danger'));
  } else if (entry.status === 'approved') {
    actions.push(kbAction('标为过期', () => kbReview(entry.id, 'stale')));
  } else if (entry.status === 'stale' || entry.status === 'rejected') {
    actions.push(kbAction('恢复', () => kbReview(entry.id, 'restore')));
  }
  actions.forEach((button) => els.actions.append(button));
  if (entry.id) {
    els.actions.append(kbAction('复制 ID', () => kbCopyText(entry.id)));
  }

  els.body.innerHTML = md(entry.body || entry.summary || '（无正文）', { artifacts: false });

  els.sources.innerHTML = '';
  const sourceTitle = document.createElement('div');
  sourceTitle.className = 'kb-section-title';
  sourceTitle.textContent = '来源与证据';
  els.sources.append(sourceTitle);
  if (entry.source_task) {
    const task = entry.source_task;
    const row = document.createElement('div');
    row.className = 'kb-source-row';
    const text = document.createElement('div');
    text.className = 'kb-source-text';
    text.textContent = task.missing
      ? `来源任务 ${task.id}（已不可用）`
      : `任务 ${task.id} · ${task.title || ''} · ${task.status || ''}`;
    row.append(text);
    if (!task.missing && state.current && state.current.main_agent) {
      row.append(kbAction('打开看板', () => kbOpenBoardTask(task.id)));
    }
    els.sources.append(row);
  } else if (entry.source_task_id) {
    els.sources.append(kbMuted('来源任务：' + entry.source_task_id + '（已不可用）'));
  }
  if (Array.isArray(entry.source_artifacts) && entry.source_artifacts.length) {
    entry.source_artifacts.forEach((artifact) => {
      const row = document.createElement('div');
      row.className = 'kb-source-row';
      const text = document.createElement('div');
      text.className = 'kb-source-text';
      text.textContent = artifact.missing
        ? `产物 ${artifact.id}（已不可用）`
        : `产物 ${artifact.title || artifact.filename || artifact.id} · ${artifact.kind || ''}`;
      row.append(text);
      if (!artifact.missing && artifact.url) {
        const open = document.createElement('a');
        open.className = 'kb-action';
        open.href = artifact.url;
        open.target = '_blank';
        open.rel = 'noopener';
        open.textContent = '打开';
        row.append(open);
        if (artifact.kind === 'markdown' || artifact.kind === 'html' || artifact.kind === 'file') {
          const download = document.createElement('a');
          download.className = 'kb-action';
          download.href = artifact.url + (artifact.url.includes('?') ? '&' : '?') + 'download=1';
          download.textContent = '下载';
          row.append(download);
        }
      }
      els.sources.append(row);
    });
  }
  (entry.evidence || []).forEach((evidence) => {
    const row = document.createElement('div');
    row.className = 'kb-evidence';
    const head = document.createElement('div');
    head.className = 'kb-evidence-head';
    head.textContent = `${evidence.kind}: ${evidence.ref}${evidence.locator ? ' · ' + evidence.locator : ''}`;
    row.append(head);
    if (evidence.quote) {
      const quote = document.createElement('div');
      quote.className = 'kb-evidence-quote';
      quote.textContent = evidence.quote;
      row.append(quote);
    }
    els.sources.append(row);
  });

  els.relations.innerHTML = '';
  if (Array.isArray(entry.relations) && entry.relations.length) {
    const title = document.createElement('div');
    title.className = 'kb-section-title';
    title.textContent = '关联';
    els.relations.append(title);
    entry.relations.forEach((relation) => {
      const row = document.createElement('div');
      row.className = 'kb-source-row';
      const text = document.createElement('div');
      text.className = 'kb-source-text';
      text.textContent = `${relation.kind} · ${relation.to_title || relation.to_id}`;
      row.append(text);
      if (relation.to_id) {
        row.append(kbAction('查看', () => kbOpenDetail(relation.to_id)));
      }
      els.relations.append(row);
    });
  }

  els.audit.innerHTML = '';
  if (entry.superseded_by) {
    els.audit.append(kbMuted('已被 ' + entry.superseded_by + ' 替代'));
  }
}

async function kbReview(id, action) {
  if (kbState.pending || !id) return;
  let reason = '';
  if (action === 'reject' || action === 'stale') {
    const answer = prompt(action === 'reject' ? '驳回原因（可选）' : '为什么标为过期？（可选）');
    if (answer === null) return;
    reason = answer;
  }
  kbState.pending = true;
  try {
    await api('/api/knowledge/' + encodeURIComponent(id), {
      method: 'PATCH',
      body: JSON.stringify({ action, reason }),
    });
    await kbLoad();
    if (kbState.selectedId === id) await kbOpenDetail(id, true);
  } catch (err) {
    alert('操作失败：' + (err && err.message ? err.message : err));
  } finally {
    kbState.pending = false;
  }
}

function kbNew(task) {
  const title = kbEl('#knowledge-form-title');
  const summary = kbEl('#knowledge-form-summary');
  const body = kbEl('#knowledge-form-body');
  const project = kbEl('#knowledge-form-project');
  const tags = kbEl('#knowledge-form-tags');
  const taskId = kbEl('#knowledge-form-task');
  if (title) title.value = (task && (task.full_title || task.title)) || '';
  if (summary) summary.value = '';
  if (body) body.value = '';
  if (project) project.value = (task && task.project) || '';
  if (tags) tags.value = (task && Array.isArray(task.tags)) ? task.tags.join(', ') : '';
  if (taskId) taskId.value = (task && task.id) || '';
  if (typeof openModal === 'function') openModal('#modal-knowledge');
}

async function kbSave() {
  const payload = {
    title: (kbEl('#knowledge-form-title') || {}).value || '',
    summary: (kbEl('#knowledge-form-summary') || {}).value || '',
    body: (kbEl('#knowledge-form-body') || {}).value || '',
    project: (kbEl('#knowledge-form-project') || {}).value || '',
    tags: ((kbEl('#knowledge-form-tags') || {}).value || '').split(/[,，]/).map((s) => s.trim()).filter(Boolean),
    source_task_id: (kbEl('#knowledge-form-task') || {}).value || '',
  };
  if (!payload.title.trim()) {
    alert('标题不能为空');
    return;
  }
  if (!payload.summary.trim() && !payload.body.trim()) {
    alert('摘要或正文至少填一个');
    return;
  }
  const button = kbEl('#knowledge-form-save');
  if (button) button.disabled = true;
  try {
    const data = await api('/api/knowledge', { method: 'POST', body: JSON.stringify(payload) });
    if (typeof closeModal === 'function') closeModal(kbEl('#modal-knowledge'));
    kbState.selectedId = data.id;
    await kbLoad();
    await kbOpenDetail(data.id);
  } catch (err) {
    alert('保存失败：' + (err && err.message ? err.message : err));
  } finally {
    if (button) button.disabled = false;
  }
}

async function kbCollect() {
  if (kbState.pending) return;
  if (!confirm('扫描看板中「待验收 / 已完成」的任务，生成知识草稿？已采集过的任务会跳过。')) return;
  kbState.pending = true;
  try {
    const data = await api('/api/knowledge/collect', { method: 'POST' });
    alert(`采集完成：扫描 ${data.scanned || 0} 个任务，新建 ${data.created || 0} 条草稿，跳过 ${data.skipped || 0}，隔离 ${data.quarantined || 0}。`);
    await kbLoad();
  } catch (err) {
    alert('采集失败：' + (err && err.message ? err.message : err));
  } finally {
    kbState.pending = false;
  }
}

function kbReturnView() {
  if (kbState.returnView === 'chat') {
    const chat = kbEl('#chat-view');
    if (chat) chat.classList.remove('hidden');
  } else if (kbState.returnView === 'group') {
    const group = kbEl('#group-view');
    if (group) group.classList.remove('hidden');
  } else if (kbState.returnView === 'new') {
    const view = kbEl('#new-session-view');
    if (view) view.classList.remove('hidden');
  } else {
    const empty = kbEl('#empty-state');
    if (empty) empty.classList.remove('hidden');
  }
}

function openKnowledge(opts = {}) {
  const view = kbEl('#knowledge-view');
  if (!view) return;
  if (!kbEl('#chat-view').classList.contains('hidden')) kbState.returnView = 'chat';
  else if (!kbEl('#group-view').classList.contains('hidden')) kbState.returnView = 'group';
  else if (!kbEl('#new-session-view').classList.contains('hidden')) kbState.returnView = 'new';
  else kbState.returnView = 'empty';
  ['#empty-state', '#chat-view', '#new-session-view', '#group-view'].forEach((sel) => {
    const el = kbEl(sel);
    if (el) el.classList.add('hidden');
  });
  view.classList.remove('hidden');
  if (opts.taskId) kbState.sourceTask = opts.taskId;
  else if (opts.clearTask) kbState.sourceTask = '';
  if (opts.status) {
    kbState.status = opts.status;
    const sel = kbEl('#knowledge-status');
    if (sel) sel.value = opts.status;
  }
  if (opts.project != null) {
    kbState.project = opts.project;
    const sel = kbEl('#knowledge-project');
    if (sel) sel.value = opts.project;
  }
  kbUpdateFacets();
  kbLoad();
  if (opts.id) kbOpenDetail(opts.id);
}

function closeKnowledge() {
  const view = kbEl('#knowledge-view');
  if (!view) return;
  view.classList.add('hidden');
  kbCloseDetail();
  const detail = kbEl('#knowledge-detail');
  if (detail) detail.classList.add('hidden');
  kbReturnView();
}

function openKnowledgeEntry(id) {
  openKnowledge({ id });
}

function openKnowledgeForTask(taskId) {
  openKnowledge({ taskId });
}

function openKnowledgeCompose(task) {
  kbNew(task);
}

async function kbOpenBoardTask(taskId) {
  if (!state.current || !state.current.main_agent || !taskId) return;
  closeKnowledge();
  switchChatTab('board');
  await boardShow(true);
  const data = state.agentBoard;
  const task = ((data && data.tasks) || []).find((item) => item.id === taskId);
  if (task) boardOpenDrawer(task);
  else alert('看板上没有找到任务 ' + taskId);
}

// Board drawer 「相关知识」：app.js 在概览渲染时调用。只读展示，不写库。
function renderBoardKnowledge(task) {
  const box = kbEl('#board-drawer-knowledge');
  if (!box) return;
  box.innerHTML = '';
  const list = (task && Array.isArray(task.knowledge)) ? task.knowledge : [];
  const count = Number(task && task.knowledge_count) || list.length;
  const countEl = kbEl('#board-drawer-knowledge-count');
  if (countEl) countEl.textContent = count ? '(' + count + ')' : '';
  if (!list.length) {
    box.append(kbMuted('暂无相关知识'));
  } else {
    list.forEach((item) => {
      const row = document.createElement('div');
      row.className = 'kb-related-row';
      const badge = kbStatusBadge(item.status);
      const main = document.createElement('div');
      main.className = 'kb-related-main';
      const title = document.createElement('div');
      title.className = 'kb-related-title';
      title.textContent = item.title || item.id;
      const meta = document.createElement('div');
      meta.className = 'kb-related-meta';
      meta.textContent = (item.project ? item.project + ' · ' : '') + (item.updated_at || '');
      main.append(title, meta);
      row.append(badge, main);
      row.onclick = () => openKnowledge({ id: item.id });
      box.append(row);
    });
  }
  const actions = document.createElement('div');
  actions.className = 'kb-related-actions';
  actions.append(kbAction('从本任务提炼知识', () => openKnowledgeCompose(task)));
  if (list.length) {
    actions.append(kbAction('查看全部', () => openKnowledge({ taskId: task.id })));
  }
  box.append(actions);
}

// ---------- wiring ----------
(function kbWire() {
  const openBtn = kbEl('#btn-knowledge');
  if (openBtn) openBtn.onclick = () => openKnowledge();
  const back = kbEl('#knowledge-back');
  if (back) back.onclick = () => closeKnowledge();
  const closeDetail = kbEl('#knowledge-detail-close');
  if (closeDetail) closeDetail.onclick = () => {
    kbCloseDetail();
    const detail = kbEl('#knowledge-detail');
    if (detail) detail.classList.add('hidden');
  };
  const newBtn = kbEl('#knowledge-new');
  if (newBtn) newBtn.onclick = () => kbNew(null);
  const saveBtn = kbEl('#knowledge-form-save');
  if (saveBtn) saveBtn.onclick = () => kbSave();
  const collectBtn = kbEl('#knowledge-collect');
  if (collectBtn) collectBtn.onclick = () => kbCollect();
  const q = kbEl('#knowledge-q');
  if (q) {
    let timer = null;
    q.addEventListener('input', () => {
      kbState.q = q.value;
      clearTimeout(timer);
      timer = setTimeout(() => kbLoad(), 250);
    });
    q.addEventListener('keydown', (event) => {
      if (event.key === 'Enter') {
        clearTimeout(timer);
        kbState.q = q.value;
        kbLoad();
      }
    });
  }
  const project = kbEl('#knowledge-project');
  if (project) project.onchange = () => { kbState.project = project.value; kbLoad(); };
  const status = kbEl('#knowledge-status');
  if (status) status.onchange = () => { kbState.status = status.value; kbLoad(); };
  document.addEventListener('keydown', (event) => {
    if (event.key !== 'Escape') return;
    const view = kbEl('#knowledge-view');
    if (!view || view.classList.contains('hidden')) return;
    const detail = kbEl('#knowledge-detail');
    if (detail && detail.classList.contains('open')) {
      detail.classList.remove('open');
      detail.classList.add('hidden');
      return;
    }
    closeKnowledge();
  });
})();

// Deep link: /?knowledge=1 opens the list; /?knowledge=<id> opens one entry.
// ?theme=dark|light is a test/verification convenience shared with the rest of
// the console (the normal theme toggle still persists to localStorage).
(function kbBoot() {
  const params = new URLSearchParams(location.search);
  const theme = params.get('theme');
  if ((theme === 'dark' || theme === 'light') && typeof applyTheme === 'function') {
    applyTheme(theme, true);
  }
  if (!params.has('knowledge')) return;
  const value = params.get('knowledge') || '';
  const open = () => openKnowledge({ id: value && value !== '1' ? value : '' });
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', open);
  else setTimeout(open, 250);
})();
