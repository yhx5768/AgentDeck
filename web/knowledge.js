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
  type: '',
  verification: '',
  sourceTask: '',
  selectedId: null,
  loading: false,
  requestSeq: 0,
  pending: false,
  preserveTask: false,
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

function kbTypeLabel(kind) {
  return {
    env_map: '环境地图',
    playbook: '操作配方',
    strategy: '策略计划',
    tested_rule: '已验证规则',
    pitfall: '坑与反例',
    index: '索引入口',
  }[kind] || kind || '未分类';
}

function kbVerificationLabel(value) {
  return {
    tested: '已验证',
    observed: '已观察',
    unverified: '未验证',
  }[value] || value || '未验证';
}

function kbTypeBadge(kind) {
  if (!kind) return null;
  const span = document.createElement('span');
  span.className = 'kb-type kb-type-' + kind;
  span.textContent = kbTypeLabel(kind);
  return span;
}

function kbVerificationBadge(value) {
  const span = document.createElement('span');
  span.className = 'kb-verification kb-verification-' + (value || 'unverified');
  span.textContent = kbVerificationLabel(value);
  span.title = value === 'tested'
    ? '有证据且验证通过'
    : value === 'observed'
      ? '观察到但未做完整验证'
      : '未验证，采信前请复核';
  return span;
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

function kbStatChip(label, count, status) {
  const button = document.createElement('button');
  button.type = 'button';
  button.className = 'kb-stat' + (kbState.status === status ? ' active' : '');
  button.textContent = label + ' ' + count;
  button.title = '筛选' + label;
  button.onclick = () => {
    kbState.status = status;
    const sel = kbEl('#knowledge-status');
    if (sel) sel.value = status;
    kbLoad();
  };
  return button;
}

function kbUpdateFacets() {
  const stats = kbEl('#knowledge-stats');
  if (stats) {
    stats.innerHTML = '';
    const counts = kbState.counts || {};
    const chips = [
      ['已通过', counts.approved || 0, 'approved'],
      ['草稿', counts.draft || 0, 'draft'],
      ['可能过期', counts.stale || 0, 'stale'],
    ];
    if (!counts.approved && !counts.draft && !counts.stale) {
      const none = document.createElement('span');
      none.className = 'kb-stat muted';
      none.textContent = '暂无知识';
      stats.append(none);
    } else {
      chips.forEach(([label, count, status]) => {
        if (count > 0) stats.append(kbStatChip(label, count, status));
      });
    }
  }
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

function kbHideListStates() {
  ['#knowledge-list-loading', '#knowledge-list-error', '#knowledge-empty'].forEach((sel) => {
    const el = kbEl(sel);
    if (el) el.classList.add('hidden');
  });
}

function kbShowLoading() {
  kbHideListStates();
  const list = kbEl('#knowledge-list');
  if (list) list.innerHTML = '';
  const loading = kbEl('#knowledge-list-loading');
  if (loading) loading.classList.remove('hidden');
}

function kbShowError(message) {
  kbHideListStates();
  const list = kbEl('#knowledge-list');
  if (list) list.innerHTML = '';
  const error = kbEl('#knowledge-list-error');
  if (!error) return;
  error.innerHTML = '';
  const text = document.createElement('div');
  text.className = 'kb-error-text';
  text.textContent = message;
  error.append(text, kbAction('重试', () => kbLoad(), 'primary'));
  error.classList.remove('hidden');
}

function kbShowEmpty(message) {
  kbHideListStates();
  const list = kbEl('#knowledge-list');
  if (list) list.innerHTML = '';
  const empty = kbEl('#knowledge-empty');
  if (empty) {
    empty.textContent = message;
    empty.classList.remove('hidden');
  }
}

function kbRenderList() {
  const list = kbEl('#knowledge-list');
  if (!list) return;
  kbHideListStates();
  list.innerHTML = '';
  if (!kbState.entries.length) {
    kbShowEmpty(kbState.q
      ? `没有匹配「${kbState.q}」的知识。试试更短的关键词，或放宽项目/状态筛选。`
      : '还没有知识。可以沉淀：环境地图 / 操作配方 / 策略计划 / 已验证规则 / 坑与反例 / 索引；不要沉淀密钥、一次性日志、实时状态。可从看板采集，或点「新建知识」。');
    return;
  }
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
  head.append(kbVerificationBadge(entry.verification));
  const typeBadge = kbTypeBadge(entry.type);
  if (typeBadge) head.append(typeBadge);
  const generated = kbGeneratedBadge(entry);
  if (generated) head.append(generated);
  if (entry.project) {
    const project = document.createElement('span');
    project.className = 'kb-project';
    project.textContent = entry.project;
    head.append(project);
  }
  if (entry.source_task_id) {
    const source = document.createElement('span');
    source.className = 'kb-source-chip';
    source.textContent = '任务 ' + entry.source_task_id;
    source.title = '来源任务 ' + entry.source_task_id;
    head.append(source);
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
  kbShowLoading();
  const params = new URLSearchParams();
  if (kbState.q.trim()) params.set('q', kbState.q.trim());
  if (kbState.project) params.set('project', kbState.project);
  if (kbState.status) params.set('status', kbState.status);
  if (kbState.type) params.set('type', kbState.type);
  if (kbState.verification) params.set('verification', kbState.verification);
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
    kbShowError('读取知识失败：' + (err && err.message ? err.message : err));
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
  const main = kbEl('#knowledge-main');
  if (main) main.classList.remove('kb-split');
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
  const main = kbEl('#knowledge-main');
  if (main) main.classList.add('kb-split');
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
  els.kicker.append(kbVerificationBadge(entry.verification));
  const detailType = kbTypeBadge(entry.type);
  if (detailType) els.kicker.append(detailType);
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
    ['类型', kbTypeLabel(entry.type)],
    ['验证', kbVerificationLabel(entry.verification)],
    ['适用范围', entry.scope],
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

  els.body.innerHTML = '<div class="kb-section-title">正文</div>'
    + md(entry.body || entry.summary || '（无正文）', { artifacts: false });

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
    if (/^https?:\/\//i.test(evidence.ref || '')) {
      const link = document.createElement('a');
      link.href = evidence.ref;
      link.target = '_blank';
      link.rel = 'noopener';
      link.textContent = evidence.ref;
      head.append(document.createTextNode(evidence.kind + ': '), link);
    } else {
      head.textContent = `${evidence.kind}: ${evidence.ref}${evidence.locator ? ' · ' + evidence.locator : ''}`;
      head.title = '点击复制来源';
      head.style.cursor = 'pointer';
      head.onclick = () => kbCopyText(evidence.ref);
    }
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
  const formType = kbEl('#knowledge-form-type');
  const formVerification = kbEl('#knowledge-form-verification');
  const scope = kbEl('#knowledge-form-scope');
  const verified = kbEl('#knowledge-form-verified');
  if (title) title.value = (task && (task.full_title || task.title)) || '';
  if (summary) summary.value = '';
  if (body) body.value = '';
  if (project) project.value = (task && task.project) || '';
  if (tags) tags.value = (task && Array.isArray(task.tags)) ? task.tags.join(', ') : '';
  if (taskId) taskId.value = (task && task.id) || '';
  if (formType) formType.value = 'playbook';
  if (formVerification) formVerification.value = 'unverified';
  if (scope) scope.value = (task && task.project) || '';
  if (verified) verified.value = '';
  if (typeof openModal === 'function') openModal('#modal-knowledge');
}

async function kbSave() {
  const payload = {
    title: (kbEl('#knowledge-form-title') || {}).value || '',
    summary: (kbEl('#knowledge-form-summary') || {}).value || '',
    body: (kbEl('#knowledge-form-body') || {}).value || '',
    project: (kbEl('#knowledge-form-project') || {}).value || '',
    tags: ((kbEl('#knowledge-form-tags') || {}).value || '').split(/[,，]/).map((s) => s.trim()).filter(Boolean),
    type: (kbEl('#knowledge-form-type') || {}).value || 'playbook',
    verification: (kbEl('#knowledge-form-verification') || {}).value || 'unverified',
    scope: (kbEl('#knowledge-form-scope') || {}).value || '',
    last_verified_at: (kbEl('#knowledge-form-verified') || {}).value || '',
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

function kbUpdateContext() {
  const context = kbEl('#knowledge-context');
  if (!context) return;
  const sess = state.current;
  if (!sess) {
    context.textContent = 'mainAgent';
    context.title = '';
    return;
  }
  const name = sess.name || '未命名会话';
  context.textContent = 'mainAgent · ' + name;
  context.title = '当前主 Agent 会话：' + name
    + (sess.peer_name && sess.peer_name !== 'mainAgent' ? '（地址 ' + sess.peer_name + '）' : '');
}

// Called by switchChatTab('knowledge') in app.js. Clearing sourceTask here is
// what makes a direct tab click show the whole library; board drawer
// "查看全部" sets preserveTask before switching.
function kbOnTabShow() {
  if (!kbState.preserveTask) kbState.sourceTask = '';
  kbState.preserveTask = false;
  kbUpdateContext();
  kbLoad();
}

function kbOnTabHide() {
  const detail = kbEl('#knowledge-detail');
  if (detail) {
    detail.classList.remove('open');
    detail.classList.add('hidden');
  }
  const main = kbEl('#knowledge-main');
  if (main) main.classList.remove('kb-split');
  kbState.selectedId = null;
  document.querySelectorAll('.kb-item.selected').forEach((item) => item.classList.remove('selected'));
}

function openKnowledgeTab(opts = {}) {
  if (!state.current || !state.current.main_agent) return false;
  if (opts.taskId) {
    kbState.sourceTask = opts.taskId;
    kbState.preserveTask = true;
  } else if (opts.clearTask !== false) {
    kbState.sourceTask = '';
  }
  if (opts.id) kbState.selectedId = opts.id;
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
  switchChatTab('knowledge');
  return true;
}

function openKnowledgeEntry(id) {
  return openKnowledgeTab({ id });
}

function openKnowledgeForTask(taskId) {
  return openKnowledgeTab({ taskId });
}

function openKnowledgeCompose(task) {
  kbNew(task);
}

async function kbOpenBoardTask(taskId) {
  if (!state.current || !state.current.main_agent || !taskId) return;
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
      const verify = kbVerificationBadge(item.verification);
      const main = document.createElement('div');
      main.className = 'kb-related-main';
      const title = document.createElement('div');
      title.className = 'kb-related-title';
      title.textContent = item.title || item.id;
      const meta = document.createElement('div');
      meta.className = 'kb-related-meta';
      meta.textContent = (item.project ? item.project + ' · ' : '') + (item.updated_at || '');
      main.append(title, meta);
      row.append(badge, verify, main);
      row.onclick = () => openKnowledgeEntry(item.id);
      box.append(row);
    });
  }
  const actions = document.createElement('div');
  actions.className = 'kb-related-actions';
  actions.append(kbAction('从本任务提炼知识', () => openKnowledgeCompose(task)));
  if (list.length) {
    actions.append(kbAction('查看全部', () => openKnowledgeForTask(task.id)));
  }
  box.append(actions);
}

// ---------- wiring ----------
(function kbWire() {
  const kbTab = kbEl('#chat-tab-knowledge');
  if (kbTab) {
    // Replace app.js's generic tab handler so a direct tab click clears the
    // task filter; board drawer "查看全部" calls openKnowledgeForTask instead.
    kbTab.onclick = () => {
      kbState.preserveTask = false;
      kbState.sourceTask = '';
      switchChatTab('knowledge');
    };
  }
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
  const type = kbEl('#knowledge-type');
  if (type) type.onchange = () => { kbState.type = type.value; kbLoad(); };
  const verification = kbEl('#knowledge-verification');
  if (verification) verification.onchange = () => { kbState.verification = verification.value; kbLoad(); };
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
    switchChatTab('chat');
  });
})();

// Deep link: /?knowledge=1 opens the tab; /?knowledge=<id> opens one entry.
// The target must be a main-agent session because the knowledge tab is only
// rendered for main agents.
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
  const wantId = value && value !== '1' ? value : '';
  const open = async () => {
    const deadline = Date.now() + 8000;
    while (Date.now() < deadline) {
      if (state.current && state.current.main_agent) break;
      const main = (state.sessions || []).find((s) => s.main_agent);
      if (main) {
        try { await openSession(main.id); } catch (err) {}
        break;
      }
      await new Promise((resolve) => setTimeout(resolve, 200));
    }
    if (!state.current || !state.current.main_agent) {
      alert('资料空间需要主 Agent 会话；当前没有可用的 mainAgent。');
      return;
    }
    switchChatTab('knowledge');
    if (wantId) {
      kbState.selectedId = wantId;
      kbOpenDetail(wantId);
    }
  };
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', open);
  else setTimeout(open, 250);
})();
