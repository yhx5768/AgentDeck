/* AgentDeck · Personal Agent 右栏（#agent-aside）
 *
 * 选中一张卡片（或一条队列条目）后显示：概览（产物 / 等你处理 / 最近动态 / 档案）、
 * 动态（卡片时间线，复用看板抽屉的渲染）、作答表单与回执，底部常驻一个给主 Agent 的留言框。
 * 卡片数据 = /api/agent-board 快照里的真实任务；产物渲染直接复用看板抽屉的
 * boardArtifactGroups / boardArtifactRoundCard（预览、下载、分享都是同一套）。
 * 宽屏（≥1280）与主区并排；更窄时是右侧浮层；手机是整屏 sheet。可完全关闭，
 * 关闭后右缘留一个重开入口。
 */
'use strict';

// ---------- 可见性 ----------
function paDocked() { return window.innerWidth >= PA_DOCK_MIN; }
function paIsMobile() { return window.innerWidth <= 640; }
// 右栏是看板卡的详情，只跟主 Agent（需要你队列 / 主会话 / 看板）：打开 worker 会话等「别的视图」时整个收起，
// 不跟着 worker 会话走；回到主 Agent 会话再按用户之前开 / 关的偏好显示。
function paAsideAllowed() { return agentModeOn() && pa.view !== 'session'; }
function paAsideVisible() {
  if (!paAsideAllowed()) return false;
  return paDocked() ? pa.asideOpen : pa.asideShown;
}

// reveal：用户明确点了条目 / 卡片 / 头像——被关掉的右栏要重新打开（宽屏同时记住「开」）。
function paSyncAside(reveal) {
  const el = $('#agent-aside');
  if (!el) return;
  if (!agentModeOn()) { paHideAside(); return; }
  if (reveal) {
    if (paDocked()) {
      if (!pa.asideOpen) { pa.asideOpen = true; paSet(PA_ASIDE_KEY, 'open'); }
    } else {
      pa.asideShown = true;
    }
  }
  if (paDocked()) pa.asideShown = false;
  const vis = paAsideVisible();
  el.classList.toggle('hidden', !vis);
  el.classList.toggle('pa-docked', paDocked());
  document.body.classList.toggle('pa-aside-on', vis);
  document.body.classList.toggle('pa-aside-sheet', vis && !paDocked());
  paRenderHandle();
  paSyncMask(vis);
  agentRenderTabbar();
  if (vis) agentRenderAside();
}
// 浮层模式（641–1279px）下右栏盖在主区上：和看板抽屉一样给一层遮罩，点遮罩 / 按 Esc 关闭。
function paSyncMask(vis) {
  const want = vis && !paDocked() && !paIsMobile();
  let m = $('#pa-aside-mask');
  if (!m && !want) return;
  if (!m) {
    m = paEl('div', { id: 'pa-aside-mask', class: 'pa-aside-mask' });
    m.addEventListener('click', paCloseAside);
    document.body.append(m);
  }
  m.classList.toggle('hidden', !want);
}
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape' && agentModeOn() && pa.asideShown && !paDocked() && !document.querySelector('.modal:not(.hidden)')) paCloseAside();
});
function paHideAside() {
  const el = $('#agent-aside');
  if (el) el.classList.add('hidden');
  const h = $('#pa-aside-handle');
  if (h) h.classList.add('hidden');
  const tb = $('#pa-tabbar');
  if (tb) tb.classList.add('hidden');
  document.body.classList.remove('pa-aside-on', 'pa-aside-sheet', 'pa-handle-on');
  pa.asideShown = false;
  paSyncMask(false);
  paClearExplicit();
}
function paCloseAside() {
  if (paDocked()) { pa.asideOpen = false; paSet(PA_ASIDE_KEY, 'closed'); }
  else pa.asideShown = false;
  // 关闭 = 放弃这次临时跟随；重新打开回到「最新待人工」
  paResetFocusToDefault();
  if (typeof agentRenderQueue === 'function') agentRenderQueue(true);
  paSyncAside();
  agentRenderCluster();
}

// 右缘的重开入口：右栏被关掉后留着，角标 = 在等你的事。手机上没有（点行 / 点卡即开）。
function paRenderHandle() {
  const h = $('#pa-aside-handle');
  if (!h) return;
  const open = paAsideVisible();
  // 右栏收起时把手在右缘（展开）；桌面并排展开时挪到面板左缘（收起）。浮层展开时它
  // 会压在遮罩下的内容上，就不显示（用右上角 × / 点遮罩关）；手机整屏 sheet 也不用。
  const show = paAsideAllowed() && !paIsMobile() && (!open || paDocked());
  h.classList.toggle('hidden', !show);
  h.classList.toggle('collapse', show && open);
  document.body.classList.toggle('pa-handle-on', show && !open); // 收起时队列给右缘把手让位
  if (!show) return;
  const n = pa.needs.counts.all || 0;
  h.innerHTML = paSvg(open ? 'chevR' : 'panelR', open ? 'l' : '') + (n ? '<b>' + (n > 9 ? '9+' : n) + '</b>' : '');
  h.title = open ? '收起右栏' : '展开卡片详情';
  h.setAttribute('aria-label', h.title);
  h.setAttribute('aria-expanded', open ? 'true' : 'false');
  h.setAttribute('aria-controls', 'agent-aside');
  h.onclick = () => (open ? paCloseAside() : paSyncAside(true));
}

// 手机底部三标签（对话 / 需要你 / 看板），桌面不显示。
function agentRenderTabbar() {
  const bar = $('#pa-tabbar');
  if (!bar) return;
  const show = agentModeOn() && paIsMobile();
  bar.classList.toggle('hidden', !show);
  document.body.classList.toggle('pa-tabbar-on', show);
  if (!show) return;
  const c = pa.needs.counts;
  const waiting = (c.blocked || 0) + (c.review || 0);
  const sig = [pa.view, c.all, paHasLivePerm(), waiting, pa.unseen].join('|');
  if (bar.dataset.sig === sig && bar.childElementCount) return;
  bar.dataset.sig = sig;
  bar.textContent = '';
  const tab = (view, icon, label, n, hot) => paEl('button', {
    type: 'button', class: 'pa-tab-i' + (pa.view === view ? ' on' : ''), 'aria-current': pa.view === view ? 'page' : null,
    onclick: () => agentShowView(view),
  }, paEl('span', { class: 'ic', html: paSvg(icon, 'l') + (n ? '<b class="' + (hot ? 'hot' : '') + '">' + (n > 9 ? '9+' : n) + '</b>' : '') }), paEl('span', { text: label }));
  bar.append(tab('queue', 'inbox', '需要你', c.all, paHasLivePerm()), tab('chat', 'bot', '对话', pa.unseen, false), tab('board', 'kanban', '看板', waiting, false));
}

// ---------- 对外挂点 ----------
// 头像 / 看板卡 / 对话里的链接打开一张卡：焦点条目取这张卡上最急的待办。
function agentOpenCard(taskId, itemId) {
  if (!agentModeOn() || !taskId) return;
  if (pa.view === 'session') paShowQueue(); // 从 worker 会话里点别的 worker 头像：回到看板这一侧再开卡
  const open = paItemsOfTask(taskId);
  const pick = itemId ? paItemById(itemId) : open[0];
  pa.ui.card = taskId;
  pa.ui.sel = pick ? pick.id : '';
  pa.ui.pin = '';
  pa.ui.tab = 'ov';
  paMarkExplicit({ task_id: taskId });
  state.boardSelectedId = taskId;
  document.querySelectorAll('.board-card').forEach((el) => {
    el.classList.toggle('board-card-selected', el.dataset.taskId === taskId);
  });
  paSyncAside(true);
  agentRenderQueue(true);
  agentRenderCluster();
  paEnsureBoard(false);
}
function agentAsideNeedsChanged() { if (paAsideVisible()) agentRenderAside(); }
function agentAsideBoardChanged() {
  if (!paAsideVisible()) return;
  agentRenderAside();
  paCheckReply();
}
// ---------- 模型：当前该显示什么 ----------
function paWorkerOf(task) {
  if (!task) return null;
  return (task.session && paSessionById(task.session)) || (task.worker && paSessionByAddr(task.worker)) || null;
}
// ---------- 焦点：默认「最新待人工」 ----------
// 没有用户显式选中时，右栏常驻显示最新一张「待人工」卡：有 needs-you 待办的卡优先（右栏在那里
// 能直接作答），其余按 updated_at 倒序；同刻按 id 稳定排序。
function paNewestBlockedTask() {
  const list = paBoardTasks().filter((t) => t && t.status === '待人工');
  if (!list.length) return null;
  const rank = (t) => [paItemsOfTask(t.id).length ? 1 : 0, paParseTime(t.updated_at) || 0];
  list.sort((a, b) => {
    const ra = rank(a), rb = rank(b);
    if (ra[0] !== rb[0]) return rb[0] - ra[0];
    if (ra[1] !== rb[1]) return rb[1] - ra[1];
    return String(a.id).localeCompare(String(b.id));
  });
  return list[0];
}
function paFocusCardId() { return paAsideFocus().taskId || ''; }
function paClearExplicit() { pa.ui.explicit = false; pa.ui.explicitBlocked = false; }
// 用户点了队列行 / 看板卡 / 头像：右栏临时跟随。它是不是「待人工」卡要记下来——之后这张卡被
// 处理掉（离开待人工）焦点就交还默认规则；选的是别的状态的卡（比如待验收）则一直跟到关闭 / 换选。
function paMarkExplicit(it) {
  pa.ui.explicit = true;
  const t = it && (it.task_id ? paTaskById(it.task_id) : (it.id ? paTaskById(it.id) : null));
  pa.ui.explicitBlocked = !!(t && t.status === '待人工');
}
// 回到默认焦点：选中项跟着指到那张卡上的第一条待办，队列高亮和右栏保持一致。
function paResetFocusToDefault() {
  paClearExplicit();
  const t = paNewestBlockedTask();
  pa.ui.pin = '';
  pa.ui.tab = 'ov';
  pa.ui.defaultCard = t ? t.id : '';
  if (t) {
    const it = paItemsOfTask(t.id)[0];
    pa.ui.card = t.id;
    pa.ui.sel = it ? it.id : '';
  } else {
    pa.ui.card = '';
    pa.ui.sel = '';
  }
}
// 默认焦点换了一张卡（不是用户点的）：队列高亮跟过去，两边不打架。只在默认卡变化时同步，
// 不覆盖用户用 j/k 在队列里移动出来的选中。
function paSyncDefaultSelection(t) {
  const id = t ? t.id : '';
  if (pa.ui.defaultCard === id) return;
  pa.ui.defaultCard = id;
  if (!t) return;
  const it = paItemsOfTask(t.id)[0];
  pa.ui.card = t.id;
  pa.ui.sel = it ? it.id : '';
  pa.ui.pin = '';
  if (typeof agentRenderQueue === 'function') agentRenderQueue();
}
// 当前该显示什么：显式焦点（失效就作废、交还默认）→ 最新待人工 → 空。
function paAsideFocus() {
  if (pa.ui.explicit) {
    const sel = pa.ui.sel || '';
    const done = sel.startsWith('done:') ? (pa.done.find((d) => 'done:' + d.id === sel) || null) : null;
    const item = done ? null : paItemById(sel);
    const cardId = pa.ui.card || (item && item.task_id) || (done && done.taskId) || '';
    const card = cardId ? paTaskById(cardId) : null;
    // 卡不在快照里（已归档 / 快照没加载）不算失效：照旧显示「找不到这张卡」，等用户关掉或换选
    const stale = done ? false
      : (!cardId && !item) || (pa.ui.explicitBlocked && card && card.status !== '待人工');
    if (!stale) {
      let focus = item && (!cardId || item.task_id === cardId) ? item : null;
      if (!focus && !done && cardId) focus = paItemsOfTask(cardId)[0] || null;
      return { taskId: cardId, task: card, item: focus, done, explicit: true };
    }
    // 卡被处理掉 / 不在快照里了：不留在一条已经没有动作的卡上
    paResetFocusToDefault();
    if (typeof agentRenderQueue === 'function') agentRenderQueue();
  }
  const t = paNewestBlockedTask();
  paSyncDefaultSelection(t);
  return { taskId: t ? t.id : '', task: t, item: null, done: null, explicit: false };
}

function paAsideModel() {
  const f = paAsideFocus();
  const taskId = f.taskId;
  const task = f.task;
  let focus = f.item;
  if (!focus && !f.done && taskId) focus = paItemsOfTask(taskId)[0] || null;
  return { taskId, task, item: focus, done: f.done, openOnCard: taskId ? paItemsOfTask(taskId) : (focus ? [focus] : []) };
}
function paAsideSig(m) {
  const t = m.task;
  const w = paWorkerOf(t);
  return [
    pa.ui.sel, pa.ui.card, pa.ui.explicit, pa.ui.tab, pa.ui.tf,
    m.item ? m.item.id + '|' + m.item.state + '|' + m.item.title : '',
    m.done ? m.done.id + '|' + m.done.steps.map((s) => s.ok).join('') : '',
    t ? [t.id, t.status, t.updated_at, (t.timeline || []).length, (t.artifacts || []).length, t.progress].join(',') : (m.taskId ? 'missing:' + !!state.agentBoard : ''),
    m.openOnCard.map((i) => i.id).join(','), w ? w.id + (w.status && w.status.running ? 'R' : 'I') + w.name : '',
    Object.keys(pa.busy).join(','), JSON.stringify(pa.err), pa.ui.pin,
  ].join('#');
}

// ---------- 渲染入口 ----------
function agentRenderAside(force) {
  const root = $('#agent-aside');
  if (!root || !paAsideVisible()) return;
  let main = root.querySelector('.pa-aside-main');
  if (!main) {
    main = paEl('div', { class: 'pa-aside-main' });
    root.append(main, paEl('div', { class: 'pa-acmp', id: 'pa-acmp' }));
  }
  const m = paAsideModel();
  const sig = paAsideSig(m);
  if (force || main.dataset.sig !== sig) {
    const keyOf = m.taskId + '|' + pa.ui.tab + '|' + pa.ui.sel;
    const old = main.querySelector('.pa-ab');
    const top = old && main.dataset.key === keyOf ? old.scrollTop : 0;
    const ae = document.activeElement;
    const fid = ae && main.contains(ae) && ae.id ? ae.id : '';
    const caret = fid && typeof ae.selectionStart === 'number' ? [ae.selectionStart, ae.selectionEnd] : null;
    main.dataset.sig = sig;
    main.dataset.key = keyOf;
    main.textContent = '';
    main.append(...paAsideParts(m));
    const body = main.querySelector('.pa-ab');
    if (body) body.scrollTop = top;
    if (fid) {
      const n = document.getElementById(fid);
      if (n) { n.focus({ preventScroll: true }); if (caret && n.setSelectionRange) n.setSelectionRange(caret[0], caret[1]); }
    }
  }
  paRenderComposer();
}

function paAsideParts(m) {
  if (!m.task && !m.item && !m.done) return [paAsideHeader(m), paAsideEmpty(m)];
  const parts = [paAsideHeader(m)];
  if (m.task) parts.push(paCardTabs(m));
  let body;
  if (!m.task && m.taskId) body = paEl('div', { class: 'pa-ab' }, paEl('div', { class: 'pa-empty' }, paEl('h3', { text: '找不到这张卡' }),
    paEl('p', { text: '卡片 ' + m.taskId + (state.agentBoard ? ' 不在当前看板快照里（可能已归档）。' : '：看板快照还没加载出来。') })));
  else if (m.task) body = paEl('div', { class: 'pa-ab' }, pa.ui.tab === 'feed' ? paFeedView(m.task) : paOverview(m));
  else body = paEl('div', { class: 'pa-ab' }, paItemOnly(m));
  parts.push(body);
  if (m.item) parts.push(paFoot(m.item));
  return parts;
}

function paAsideHeader(m) {
  const t = m.task;
  const it = m.item || m.done;
  const sid = t ? (paWorkerOf(t) || {}).id : (it && (it.session_id || it.sessionId));
  const w = sid ? paSessionById(sid) : null;
  const left = [];
  if (t) {
    left.push(paEl('span', { class: 'pa-pill ' + (PA_TASK_PILL[t.status] || ''), text: t.status }), paEl('span', { class: 'mono pa-dim pa-ell', text: t.id }));
  } else if (it) {
    left.push(paEl('span', { class: 'pa-kchip-s k-' + it.kind, html: paSvg(paKindOf(it).icon, 's') + paKindOf(it).label }));
  } else {
    left.push(paEl('h2', { class: 'pa-h2', text: '卡片详情' }));
  }
  return paEl('header', { class: 'pa-ah' },
    paEl('button', { type: 'button', class: 'icon-btn pa-back', title: '收起右栏', 'aria-label': '收起右栏', html: paSvg('back'), onclick: paCloseAside }),
    left, paEl('span', { class: 'pa-sp' }),
    w ? paEl('button', { type: 'button', class: 'icon-btn', title: '打开 ' + w.name + ' 的会话\n次要入口：作答和跟进都能在这张卡里完成', 'aria-label': '打开 worker 会话', html: paSvg('arrowUR'), onclick: () => openSession(w.id) }) : null,
    paEl('button', { type: 'button', class: 'icon-btn pa-xclose', title: '关闭右栏', 'aria-label': '关闭右栏', html: paSvg('x'), onclick: paCloseAside }));
}

function paAsideEmpty() {
  const c = pa.needs.counts.all || 0;
  const tasks = paBoardTasks().length;
  return paEl('div', { class: 'pa-ab' },
    paEl('div', { class: 'pa-empty' },
      paEl('div', { class: 'big dim', html: paSvg('kanban', 'l') }),
      paEl('h3', { text: '选一张卡片' }),
      paEl('p', { text: '在队列、看板或左栏头像里点一张卡片，这里显示它的概览、产物和动态，并可直接作答。' })),
    paEl('p', { class: 'pa-hint center', text: tasks + ' 张卡 · ' + paRunningWorkers().length + ' 个 worker 运行中 · ' + c + ' 项在等你' }));
}

function paCardTabs(m) {
  const n = boardTaskEvents(m.task).length;
  const tab = (v, label, count) => paEl('button', {
    type: 'button', class: pa.ui.tab === v ? 'on' : '', onclick: () => { pa.ui.tab = v; agentRenderAside(true); },
  }, label, count != null ? paEl('em', { text: String(count) }) : null);
  return paEl('div', { class: 'pa-ctabs' }, tab('ov', '概览'), tab('feed', '动态', n));
}
// ---------- 概览 ----------
function paSec(title, count, extra) {
  const h = paEl('h3', {}, title, count != null ? paEl('em', { text: String(count) }) : null, extra || null);
  return paEl('section', { class: 'pa-sec' }, h);
}
function paWorkerStatusText(w) {
  const st = w.status || {};
  if (st.running) return '运行中';
  if (st.queued > 0) return '排队中';
  if (st.last_error && st.last_error !== 'interrupted') return '出错';
  return '空闲';
}
function paOverview(m) {
  const t = m.task;
  const w = paWorkerOf(t);
  const ts = paParseTime(t.updated_at);
  const prov = paEl('div', { class: 'pa-prov' });
  if (w) prov.append(paEl('span', { class: typeof statusDot === 'function' ? statusDot(w) : 'dot' }), paEl('b', { text: w.name }), paEl('span', { text: paWorkerStatusText(w) }));
  else prov.append(paEl('span', { text: t.worker ? t.worker + '（会话未找到）' : '未派工' }));
  if (ts) prov.append(paEl('i', { text: '·' }), paEl('span', {}, '更新于 ', paEl('span', { 'data-pa-rel': ts, text: paRel(ts) })));
  return [
    paEl('h2', { class: 'pa-dt', text: t.full_title || t.title || '（无标题）' }), prov,
    // 有要处理的事 / 刚处理完的回执时放在产物前面：产物卡片很高，排在前面会把作答表单顶出视口
    m.item || m.done || m.openOnCard.length ? paActionArea(m) : null,
    paArtifactsSection(t),
    paRecentSection(t), paProfile(t, w),
  ];
}

function paArtifactsSection(t) {
  const list = Array.isArray(t.artifacts) ? t.artifacts : [];
  const sec = paSec('产物', list.length || null);
  if (!list.length) {
    sec.append(paEl('p', { class: 'pa-none', text: '暂无产物。worker 发布到这张卡的文件会出现在这里。' }));
    return sec;
  }
  // 复用看板抽屉的产物渲染：按轮次分组、预览 / 下载 / 复制 / 分享都是同一套。
  const box = paEl('div', { class: 'pa-arts' });
  boardArtifactGroups(list).forEach((g, i) => box.append(boardArtifactRoundCard(g, i === 0)));
  sec.append(box);
  return sec;
}

function paRecentSection(t) {
  const es = boardTaskEvents(t).slice(-3);
  if (!es.length) return null;
  const more = paEl('button', { type: 'button', class: 'more', text: '查看全部 →', onclick: () => { pa.ui.tab = 'feed'; agentRenderAside(true); } });
  const sec = paSec('最近动态', null, more);
  // 和「动态」同一种行，只是正文截成一行摘要
  const list = paEl('div', { class: 'board-drawer-timeline pa-recent' });
  es.forEach((e) => list.append(boardTimelineRow(Object.assign({}, e, { text: paClip(String(e.text || '').replace(/\s+/g, ' ').trim(), 120) }))));
  sec.append(list);
  return sec;
}

function paProfile(t, w) {
  const kv = paEl('dl', { class: 'pa-kv' });
  const row = (k, v, mono) => { if (v) kv.append(paEl('dt', { text: k }), paEl('dd', { class: mono ? 'mono' : '', text: v })); };
  row('worker', w ? w.name : t.worker);
  if (w) row('引擎', [w.engine || w.cli, w.provider, w.model_override, w.effort_override].filter(Boolean).join(' · '));
  row('会话', t.session, true);
  row('工作区', t.cwd || (w && w.cwd), true);
  row('项目', t.project);
  row('说明', t.status_reason || t.progress);
  return paEl('details', { class: 'pa-src' }, paEl('summary', {}, paEl('b', { text: '卡片档案' }), paEl('span', { class: 'pa-ell', text: paClip(t.progress || t.status_reason || t.id, 28) })), kv);
}

// 条目没绑看板卡（比如一个独立会话在要权限）：右栏只显示这条请求本身。
function paItemOnly(m) {
  const it = m.item || m.done;
  const sid = it.session_id || it.sessionId;
  const w = sid ? paSessionById(sid) : null;
  const title = it.title;
  return [
    paEl('h2', { class: 'pa-dt', text: title }),
    paEl('div', { class: 'pa-prov' }, paEl('b', { text: it.session_name || it.sessionName || (w && w.name) || '会话' }),
      paEl('span', { text: '没有绑定看板卡' }), w && w.cwd ? [paEl('i', { text: '·' }), paEl('span', { class: 'mono', text: shortenPath(w.cwd) })] : null),
    paActionArea(m),
  ];
}
// ---------- 等你处理 ----------
function paDraft(id) {
  if (!pa.drafts) pa.drafts = {};
  return pa.drafts[id] || (pa.drafts[id] = { picked: {}, custom: {}, note: '', reasonOpen: false, reason: '' });
}

function paActionArea(m) {
  const open = m.openOnCard;
  const fi = m.item;
  const done = m.done;
  const sec = paSec(open.length ? '等你处理' : '处理结果', open.length > 1 ? open.length : null);
  if (open.length > 1 || (done && open.length)) {
    const chips = paEl('div', { class: 'pa-fchips' });
    open.forEach((it) => {
      const live = it.state === 'live';
      chips.append(paEl('button', {
        type: 'button', class: 'pa-fc k-' + it.kind + (fi && fi.id === it.id ? ' on' : ''),
        onclick: () => agentSelectItem(it.id, { reveal: false }),
      }, paEl('span', { html: paSvg(paKindOf(it).icon, 's') }), paKindOf(it).label,
      live ? paEl('span', { class: 'pa-cd', 'data-pa-cd': it.id, text: paMMSS(paLeftMs(it)) }) : null));
    });
    if (done) chips.append(paEl('button', { type: 'button', class: 'pa-fc done on', text: '已处理' }));
    sec.append(chips);
  }
  if (done && !fi) sec.append(paReceipt(done));
  else if (fi) paAppend(sec, paItemHead(fi, open.length < 2), paPayload(fi), paForm(fi));
  return sec;
}

function paItemHead(it, solo) {
  const k = paKindOf(it);
  return [
    solo ? paEl('div', { class: 'pa-ih k-' + it.kind },
      paEl('span', { class: 'pa-kchip-s k-' + it.kind, html: paSvg(k.icon, 's') + k.label }),
      paEl('span', { class: 'ihs' + (it.state === 'live' ? ' live' : ''), text: it.state === 'live' ? '限时' : (it.kind === 'blocked' ? '待人工' : '等你决定') })) : null,
    paEl('h3', { class: 'pa-it', text: it.title }),
    paEl('div', { class: 'pa-prov' }, paEl('b', { text: it.session_name || it.peer_name || '会话' }), paEl('span', { text: k.verb }),
      paEl('i', { text: '·' }), paEl('span', { 'data-pa-rel': it.created_at, text: paRel(it.created_at) })),
  ];
}

// 权限请求的主体预览：Bash 显示命令，Edit / Write 显示文件与改动，其余退回关键字段或 JSON。
function paPermPreview(tool, inp) {
  inp = inp || {};
  if (typeof inp.command === 'string') return inp.command;
  const lines = [];
  const file = inp.file_path || inp.path || inp.notebook_path || '';
  if (file) lines.push(file);
  if (typeof inp.old_string === 'string' || typeof inp.new_string === 'string') {
    String(inp.old_string || '').split('\n').slice(0, 10).forEach((l) => lines.push('- ' + l));
    String(inp.new_string || '').split('\n').slice(0, 10).forEach((l) => lines.push('+ ' + l));
  } else if (typeof inp.content === 'string') {
    lines.push('（写入 ' + inp.content.length + ' 个字符）');
  }
  if (lines.length) return lines.join('\n');
  const s = typeof toolSummary === 'function' ? toolSummary(inp) : '';
  return s || JSON.stringify(inp, null, 2);
}

// worker 最近一条「说得上话」的记录（看板时间线是新→旧，取第一条匹配的）。
function paLatestWorkerSay(task) {
  const kinds = ['受阻', '评论', '回报', '最近回报', '需确认'];
  const ev = ((task && task.timeline) || []).find((e) => kinds.includes(e.kind) && String(e.text || '').trim());
  return ev ? String(ev.text).trim() : '';
}
// worker 的说明 / 回报：走主对话同一套 md()（marked + DOMPurify，breaks 保留换行），保留引用条和
// 标签；长文本默认折起，点「展开全文」看完整内容，短文本直出。
const PA_SAY_CHARS = 400;
const PA_SAY_LINES = 7;
function paSay(text, label, opts) {
  const o = opts || {};
  const raw = String(text == null ? '' : text).trim();
  const block = paEl('blockquote', { class: 'pa-say md' });
  block.innerHTML = md(raw, { artifacts: false }); // md 内部已 DOMPurify
  const wrap = paEl('div', { class: 'pa-say-w' }, paEl('h4', { html: paSvg('msg', 's') + label }), block);
  const long = o.collapsible && (raw.length > PA_SAY_CHARS || raw.split('\n').length > PA_SAY_LINES);
  if (long) {
    wrap.classList.add('long');
    block.classList.add('clip');
    const more = paEl('button', { type: 'button', class: 'pa-say-more', 'aria-expanded': 'false', text: '展开全文' });
    more.addEventListener('click', () => {
      const open = block.classList.toggle('open');
      block.classList.toggle('clip', !open);
      more.setAttribute('aria-expanded', open ? 'true' : 'false');
      more.textContent = open ? '收起' : '展开全文';
    });
    wrap.append(more);
  }
  return wrap;
}

function paPayload(it) {
  const task = it.task_id ? paTaskById(it.task_id) : null;
  if (it.kind === 'perm') {
    const inp = it.input || {};
    const s = paSessionById(it.session_id);
    return [
      paEl('div', { class: 'pa-cmdbox' },
        paEl('div', { class: 'pa-cmdh' }, paEl('span', { class: 'tname', text: it.tool }), paEl('span', { class: 'pa-sp' }),
          s && s.cwd ? paEl('span', { class: 'mono pa-ell', title: s.cwd, text: shortenPath(s.cwd) }) : null),
        paEl('pre', { class: 'pa-cmd', text: paClip(paPermPreview(it.tool, inp), 4000) })),
      inp.description ? paSay(String(inp.description), 'worker 的说明') : null,
      paEl('div', { class: 'pa-callout info' }, paEl('span', { html: paSvg('info') }),
        paEl('div', {}, paEl('b', { text: '55 秒窗口　' }), '不处理会自动拒绝，worker 收到的是「拒绝」。「本会话始终允许」只对同名工具生效。')),
      paEl('details', { class: 'pa-src' }, paEl('summary', {}, paEl('b', { text: '查看完整参数' })), paEl('pre', { class: 'pa-json', text: JSON.stringify(inp, null, 2) })),
    ];
  }
  if (it.kind === 'ask') {
    return [paEl('div', { class: 'pa-callout info' }, paEl('span', { html: paSvg('info') }),
      paEl('div', {}, paEl('b', { text: '55 秒窗口　' }), '超时后这次提问作废，模型会重新问你一遍。'))];
  }
  const say = paLatestWorkerSay(task);
  const out = [];
  if (it.summary) out.push(paEl('p', { class: 'pa-qtext', text: it.summary }));
  if (say && say !== it.summary) out.push(paSay(say, it.kind === 'blocked' ? 'worker 的说明' : 'worker 的回报', { collapsible: true }));
  if (it.kind === 'review') out.push(paEl('p', { class: 'pa-hint', text: '交付物见下方「产物」；通过后卡片移到「已完成」，打回会让 worker 按你的意见返工。' }));
  if (!out.length) out.push(paEl('p', { class: 'pa-none', text: '这张卡没有写明原因。可以在「动态」里看 worker 最近说了什么。' }));
  return out;
}
// ---------- 作答表单 ----------
function paForm(it) {
  const d = paDraft(it.id);
  if (it.kind === 'perm') {
    if (!d.reasonOpen) return null;
    const inp = paEl('input', { id: 'pa-reason-' + it.id, class: 'pa-input', maxlength: '500', placeholder: '可空' });
    inp.value = d.reason;
    inp.oninput = () => { d.reason = inp.value; };
    return paEl('div', { class: 'pa-noteform' }, paEl('label', { class: 'pa-lbl', for: 'pa-reason-' + it.id, text: '拒绝理由（会回传给模型）' }), inp);
  }
  if (it.kind === 'ask') return paAskForm(it, d);
  return paNoteForm(it, d);
}

function paAskForm(it, d) {
  const qs = parsePermQuestions(it.input);
  if (!qs.length) return paEl('p', { class: 'pa-none', text: '这条提问的内容读不出来（input 里没有 questions）。可以跳过，或到那个会话里看。' });
  const wrap = paEl('div', { class: 'pa-askf' });
  qs.forEach((q, qi) => {
    const box = paEl('div', { class: 'perm-q' });
    if (q.header) box.append(paEl('div', { class: 'perm-q-header', text: q.header }));
    box.append(paEl('div', { class: 'perm-q-text', text: q.question }));
    const opts = paEl('div', { class: 'perm-opts' });
    const mk = (label, desc, other) => {
      const b = paEl('button', {
        type: 'button', class: 'perm-opt' + (other ? ' perm-opt-other' : '') + ((d.picked[qi] || []).includes(label) ? ' picked' : ''), 'data-label': label,
      }, paEl('span', { class: 'perm-opt-label', text: other ? '其他（自定义）' : label }), desc ? paEl('span', { class: 'perm-opt-desc', text: desc }) : null);
      b.onclick = () => paAskPick(it, qs, qi, label, opts);
      return b;
    };
    (q.options || []).forEach((o) => { if (o && o.label) opts.append(mk(o.label, o.description, false)); });
    opts.append(mk(PERM_OTHER, '', true));
    const custom = paEl('input', { id: 'pa-custom-' + it.id + '-' + qi, class: 'perm-opt-custom' + ((d.picked[qi] || []).includes(PERM_OTHER) ? '' : ' hidden'), placeholder: '自定义回答', maxlength: '500' });
    custom.value = d.custom[qi] || '';
    custom.oninput = () => { d.custom[qi] = custom.value; paSyncSubmit(it); };
    opts.append(custom);
    box.append(opts);
    wrap.append(box);
  });
  return wrap;
}
function paAskPick(it, qs, qi, label, optsEl) {
  const d = paDraft(it.id);
  let cur = d.picked[qi] || [];
  if (!qs[qi].multiSelect) cur = cur[0] === label ? [] : [label];
  else cur = cur.includes(label) ? cur.filter((x) => x !== label) : [...cur, label];
  d.picked[qi] = cur;
  optsEl.querySelectorAll('.perm-opt').forEach((b) => b.classList.toggle('picked', cur.includes(b.dataset.label)));
  const custom = optsEl.querySelector('.perm-opt-custom');
  const show = cur.includes(PERM_OTHER);
  custom.classList.toggle('hidden', !show);
  if (show) custom.focus();
  paSyncSubmit(it);
}
// 回传格式与主对话的提问卡一致：{问题原文: 选项 label}，多选用「，」连接。
function paAskAnswers(it) {
  const qs = parsePermQuestions(it.input);
  const d = paDraft(it.id);
  const answers = {};
  let ok = qs.length > 0;
  qs.forEach((q, qi) => {
    const a = (d.picked[qi] || []).map((l) => (l === PERM_OTHER ? (d.custom[qi] || '').trim() : l)).filter(Boolean).join('，');
    if (a) answers[q.question] = a;
    else ok = false;
  });
  return { answers, ok };
}

function paNoteForm(it, d) {
  const review = it.kind === 'review';
  const id = 'pa-note-' + it.id;
  const ta = paEl('textarea', { id, class: 'pa-ta', rows: '3', placeholder: review ? '验收意见（打回时必填：哪里不满足、希望怎么改）' : '写下你的决定、约束或下一步……' });
  ta.value = d.note;
  ta.oninput = () => { d.note = ta.value; ta.classList.remove('err'); paSetErr(it.id, ''); paSyncSubmit(it); };
  const quick = review ? null : paEl('div', { class: 'pa-quick' }, ['同意，继续', '暂缓，等我通知', '换个方案：'].map((t) => paEl('button', {
    type: 'button', class: 'pa-qk', text: t,
    onclick: () => { ta.value = (ta.value ? ta.value + '\n' : '') + t; ta.dispatchEvent(new Event('input')); ta.focus(); },
  })));
  return paEl('div', { class: 'pa-noteform' }, paEl('label', { class: 'pa-lbl', for: id, text: review ? '验收意见' : '你的决定' }), quick, ta);
}

function paSubmitReady(it) {
  if (it.kind === 'ask') return paAskAnswers(it).ok;
  if (it.kind === 'blocked') return !!paDraft(it.id).note.trim();
  return true;
}
function paSyncSubmit(it) {
  document.querySelectorAll('[data-pa-need]').forEach((b) => {
    if (b.dataset.paNeed === it.id) b.disabled = !paSubmitReady(it) || !!(pa.busy && pa.busy[it.id]);
  });
}

// ---------- 底部操作条 ----------
function paFoot(it) {
  const d = paDraft(it.id);
  const busy = !!(pa.busy && pa.busy[it.id]);
  const err = pa.err && pa.err[it.id];
  const kids = [];
  if (it.state === 'live') {
    const win = pa.needs.windowMs || 55000;
    kids.push(paEl('div', { class: 'pa-cdw' }, paEl('span', { text: it.kind === 'perm' ? '超时自动拒绝' : '超时后模型会重新提问' }),
      paEl('div', { class: 'trk' }, paEl('i', { 'data-pa-bar': it.id, style: 'width:' + Math.max(0, Math.min(100, (paLeftMs(it) / win) * 100)) + '%' })),
      paEl('b', { class: 'pa-cd', 'data-pa-cd': it.id, text: paMMSS(paLeftMs(it)) })));
  }
  if (busy) kids.push(paEl('div', { class: 'pa-fbusy', text: '处理中…' }));
  if (err) kids.push(paEl('div', { class: 'pa-ferr', role: 'alert', text: err }));
  const btn = (cls, text, fn, extra) => paEl('button', Object.assign({ type: 'button', class: cls, disabled: busy ? true : null, onclick: fn }, extra || {}), text);
  const sp = paEl('span', { class: 'pa-sp' });
  let row;
  if (it.kind === 'perm') {
    row = [btn('perm-btn perm-deny', d.reasonOpen ? '确认拒绝' : '拒绝', () => {
      if (d.reasonOpen) { paAnswerPerm(it, 'deny', d.reason); return; }
      d.reasonOpen = true;
      agentRenderAside(true);
      const r = document.getElementById('pa-reason-' + it.id);
      if (r) r.focus();
    }), sp, btn('perm-btn', '本会话始终允许', () => paAnswerPerm(it, 'allow_tool_session')), btn('perm-btn perm-allow', '允许一次', () => paAnswerPerm(it, 'allow'))];
  } else if (it.kind === 'ask') {
    row = [btn('perm-btn perm-deny', '跳过', () => paAnswerAsk(it, true)), sp,
      btn('perm-btn perm-allow', '提交回答', () => paAnswerAsk(it, false), { 'data-pa-need': it.id, disabled: busy || !paSubmitReady(it) ? true : null })];
  } else if (it.kind === 'blocked') {
    row = [sp, btn('perm-btn perm-allow', '拍板并回复 worker', () => paDecideBlocked(it), { 'data-pa-need': it.id, disabled: busy || !paSubmitReady(it) ? true : null })];
  } else {
    row = [btn('perm-btn perm-deny', '打回重做', () => paDecideReview(it, false)), sp, btn('perm-btn perm-allow', '通过验收', () => paDecideReview(it, true))];
  }
  kids.push(paEl('div', { class: 'pa-btns' }, row));
  return paEl('footer', { class: 'pa-af k-' + it.kind }, kids);
}
// ---------- 作答动作 ----------
// 人答给 worker，账记给协调者。每一步都是真实接口调用，结果原样进回执：
//   ① 主动作（权限 / 提问决议、给 worker 发消息、改看板状态）——失败就整体失败，条目保留；
//   ② 看板记账（往卡片时间线追加一条用户事件）；
//   ③ 主 Agent 回执（POST /api/a2a/send, from=runtime）——权限审批不发（不值得为每次点「允许」唤醒一轮 LLM）。
// ②③ 失败只记在回执里，不撤销已送达 worker 的决定。
const paEnc = encodeURIComponent;
function paPost(path, body, method) { return api(path, { method: method || 'POST', body: JSON.stringify(body) }); }
function paSetBusy(id, on) { pa.busy = pa.busy || {}; if (on) pa.busy[id] = true; else delete pa.busy[id]; }
function paSetErr(id, msg) { pa.err = pa.err || {}; if (msg) pa.err[id] = msg; else delete pa.err[id]; }

async function paDo(it, spec) {
  if (pa.busy && pa.busy[it.id]) return false;
  paSetBusy(it.id, true);
  paSetErr(it.id, '');
  agentRenderAside(true);
  const steps = [];
  try {
    for (let i = 0; i < spec.steps.length; i++) {
      const s = spec.steps[i];
      try {
        const detail = await s.run();
        steps.push({ t: s.text, ok: true, at: Date.now(), d: detail || '' });
      } catch (e) {
        steps.push({ t: s.text, ok: false, at: Date.now(), d: (e && e.message) || String(e) });
        if (i === 0) throw e;
      }
    }
  } catch (e) {
    paSetBusy(it.id, false);
    paSetErr(it.id, '操作没有成功：' + ((e && e.message) || e));
    agentRenderAside(true);
    agentRefreshNeeds();
    return false;
  }
  const d = {
    id: 'd' + Date.now().toString(36) + Math.random().toString(36).slice(2, 6), itemId: it.id, kind: it.kind, title: it.title,
    sessionId: it.session_id || '', sessionName: it.session_name || '', taskId: it.task_id || '', taskTitle: it.task_title || '',
    label: spec.label, via: spec.via, at: Date.now(), steps,
  };
  pa.done.unshift(d);
  pa.done = pa.done.slice(0, PA_DONE_MAX);
  paSaveDone();
  delete pa.drafts[it.id];
  paSetBusy(it.id, false);
  // 本地先把条目摘掉，不等下一拍轮询
  pa.needs.items = pa.needs.items.filter((x) => x.id !== it.id);
  pa.needs.counts[it.kind] = Math.max(0, (pa.needs.counts[it.kind] || 0) - 1);
  pa.needs.counts.all = Math.max(0, (pa.needs.counts.all || 0) - 1);
  pa.ui.sel = 'done:' + d.id;
  pa.ui.pin = pa.ui.sel;
  pa.ui.card = d.taskId || pa.ui.card;
  // 刚处理完的这条停在自己的回执上（不立刻跳走）；关闭或换选后回到默认规则。
  // 后台被别人处理掉的卡（不是在这里操作的）会照规则自动切到下一张待人工。
  pa.ui.explicit = true;
  pa.ui.explicitBlocked = false;
  paAfterNeeds();
  agentRefreshNeeds();
  paEnsureBoard(true);
  return true;
}

async function paResolve(it, body) {
  const r = await paPost('/api/sessions/' + paEnc(it.session_id) + '/permissions/' + paEnc(it.pending_id), body);
  if (r && r.ok === false) throw new Error('这条请求已被处理或已超时');
  return '';
}
async function paBoardEvent(it, kind, text) {
  if (!it.task_id) return '未绑定看板卡，已跳过';
  await paPost('/api/agent-board/tasks/' + paEnc(it.task_id) + '/events', { kind, text, source: 'needs-you' });
  return '';
}
async function paBoardStatus(it, status, reason) {
  if (!it.task_id) return '未绑定看板卡，已跳过';
  await paPost('/api/agent-board/tasks/' + paEnc(it.task_id), { status, status_reason: reason || '' }, 'PATCH');
  return '';
}
async function paSendWorker(it, text) {
  if (!it.session_id) throw new Error('这张卡没有绑定 worker 会话');
  await paPost('/api/sessions/' + paEnc(it.session_id) + '/messages', { text });
  return '';
}
async function paReceipt2Main(it, kindWord, result) {
  const main = paPrimaryMain();
  if (!main) return '没有主 Agent，已跳过';
  if (it.session_id === main.id) return '来源就是主 Agent，已跳过';
  const to = paMainAddr(main);
  const lines = ['用户处理了一项「需要你」条目（决定已直达 worker，本消息只是系统回执，无需回复）',
    '- 来源：' + (it.session_name || '—'), '- 类型：' + kindWord,
    '- 任务：' + (it.task_id ? (it.task_title || '') + '（' + it.task_id + '）' : '未绑定看板卡'), '- 结果：' + result];
  await paPost('/api/a2a/send', { from: 'runtime', to, message: lines.join('\n'), send_id: 'needs-you:' + it.id + ':' + it.created_at });
  return '已投递给 ' + to;
}

function paAnswerPerm(it, behavior, reason) {
  const deny = behavior === 'deny';
  const label = deny ? '已拒绝' : behavior === 'allow_tool_session' ? '本会话始终允许 ' + it.tool : '允许一次';
  const body = { behavior };
  if (deny) body.message = String(reason || '').trim() || 'user denied';
  return paDo(it, { label, via: '直达 worker', steps: [
    { text: '已回传 ' + (it.session_name || 'worker') + '：' + label, run: () => paResolve(it, body) },
    { text: '看板「' + (it.task_title || '—') + '」已追加记录', run: () => paBoardEvent(it, deny ? '用户拒绝' : '用户批准', label + ' ' + it.tool + '：' + paClip(paFirstLine(paPermPreview(it.tool, it.input)), 80) + '（直达 worker）') },
  ] });
}
function paAnswerAsk(it, skip) {
  let body, label, brief;
  if (skip) { body = { behavior: 'deny', message: '用户跳过了这个提问' }; label = '已跳过'; brief = '跳过'; }
  else {
    const a = paAskAnswers(it);
    if (!a.ok) return Promise.resolve(false);
    body = { behavior: 'allow', answers: a.answers };
    brief = Object.values(a.answers).join(' / ');
    label = '已回答：' + paClip(brief, 24);
  }
  return paDo(it, { label, via: '直达 · 同轮继续', steps: [
    { text: '已作为工具结果回传 ' + (it.session_name || 'worker') + (skip ? '（跳过）' : '，同轮继续'), run: () => paResolve(it, body) },
    { text: '看板「' + (it.task_title || '—') + '」已追加记录', run: () => paBoardEvent(it, '用户作答', '「' + paClip(it.title, 60) + '」→ ' + brief + '（直达 worker）') },
    { text: '主 Agent 已收到回执（A2A · runtime）', run: () => paReceipt2Main(it, '提问', brief) },
  ] });
}
function paDecideBlocked(it) {
  const note = paDraft(it.id).note.trim();
  if (!note) return Promise.resolve(false);
  const text = '【拍板】' + it.title + '\n' + note + '\n（用户经 AgentDeck「需要你」直接答复；你停在「待人工」，收到后请继续。）';
  return paDo(it, { label: '已拍板：' + paClip(note, 20), via: '直达 worker', steps: [
    { text: '已把拍板意见送达 ' + (it.session_name || 'worker'), run: () => paSendWorker(it, text) },
    { text: '看板状态：待人工 → 进行中', run: () => paBoardStatus(it, '进行中', '用户已拍板：' + paClip(note, 60)) },
    { text: '看板「' + (it.task_title || '—') + '」已追加记录', run: () => paBoardEvent(it, '用户拍板', paClip(note, 200) + '（直达 worker）') },
    { text: '主 Agent 已收到回执（A2A · runtime）', run: () => paReceipt2Main(it, '拍板', note) },
  ] });
}
function paDecideReview(it, pass) {
  const note = paDraft(it.id).note.trim();
  if (!pass && !note) {
    const ta = document.getElementById('pa-note-' + it.id);
    if (ta) { ta.classList.remove('err'); void ta.offsetWidth; ta.classList.add('err'); ta.focus(); }
    paSetErr(it.id, '打回需要写明原因，worker 会按它返工。');
    agentRenderAside(true);
    const t2 = document.getElementById('pa-note-' + it.id);
    if (t2) { t2.classList.add('err'); t2.focus(); }
    return Promise.resolve(false);
  }
  if (pass) {
    return paDo(it, { label: '验收通过', via: '直达看板', steps: [
      { text: '看板状态：待验收 → 已完成', run: () => paBoardStatus(it, '已完成', '用户验收通过' + (note ? '：' + paClip(note, 60) : '')) },
      { text: '看板「' + (it.task_title || '—') + '」已追加记录', run: () => paBoardEvent(it, '验收通过', '待验收 → 已完成' + (note ? '：' + paClip(note, 200) : '')) },
      { text: '主 Agent 已收到回执（A2A · runtime）', run: () => paReceipt2Main(it, '验收', '通过') },
    ] });
  }
  const text = '【打回】' + it.title + '\n' + note + '\n（验收未通过，请按以上意见返工；完成后再提请验收。）';
  return paDo(it, { label: '已打回重做', via: '直达 worker', steps: [
    { text: '已把打回意见送达 ' + (it.session_name || 'worker'), run: () => paSendWorker(it, text) },
    { text: '看板状态：待验收 → 进行中', run: () => paBoardStatus(it, '进行中', '用户打回：' + paClip(note, 60)) },
    { text: '看板「' + (it.task_title || '—') + '」已追加记录', run: () => paBoardEvent(it, '验收打回', '待验收 → 进行中：' + paClip(note, 200)) },
    { text: '主 Agent 已收到回执（A2A · runtime）', run: () => paReceipt2Main(it, '验收', '打回：' + paClip(note, 80)) },
  ] });
}

// ---------- 回执 ----------
function paReceipt(d) {
  const nxt = paQueueFlat().find((x) => x.state && x.id !== d.itemId);
  const lis = d.steps.map((s) => paEl('li', { class: s.ok ? '' : 'bad' },
    paEl('span', { class: 'mk', html: paSvg(s.ok ? 'check' : 'x') }),
    paEl('div', { class: 'rp' }, s.t, s.d ? paEl('small', { text: s.d }) : null),
    paEl('time', { text: paHM(s.at) })));
  const sess = d.sessionId ? paSessionById(d.sessionId) : null;
  return paEl('div', { class: 'pa-rcp-w' },
    paEl('div', { class: 'pa-done-h' }, paEl('span', { class: 'okc', html: paSvg('check', 'l') }), paEl('div', {}, paEl('b', { text: d.label }), paEl('small', { text: paHM(d.at) + ' · ' + d.via }))),
    paEl('ul', { class: 'pa-rcp' }, lis),
    paEl('p', { class: 'pa-hint', text: '人答给 worker，账记给协调者：上面每一步都是作答后实际发出的调用，失败的步骤会标红并写明原因；它不影响已送达 worker 的决定。' }),
    paEl('div', { class: 'pa-btns row' },
      nxt ? paEl('button', { type: 'button', class: 'primary-btn', onclick: () => agentSelectItem(nxt.id, { reveal: false, scroll: true }) }, '下一项 ', paEl('span', { html: paSvg('arrowR', 's') })) : null,
      sess ? paEl('button', { type: 'button', class: 'ghost-btn', text: '打开 worker 会话', onclick: () => openSession(sess.id) }) : null,
      d.taskId ? paEl('button', { type: 'button', class: 'ghost-btn', text: '在看板查看', onclick: () => { agentShowView('board'); setTimeout(() => agentOpenCard(d.taskId), 0); } }) : null));
}
// ---------- 动态 ----------
// 卡片时间线（看板快照里的 task.timeline）。渲染和筛选直接复用看板抽屉「任务流」那一份
// （boardRenderTimeline / boardTimelineRow：按事件类型分的筛选 chip、头像 / 时间 / 类型 / markdown
// 正文，样式用 board-drawer-* 同一组类），不另造一套。
function paTagOf(s) { const m = /^【([^】]+)】/.exec(String(s || '').trim()); return m ? m[1] : ''; }
function paUntag(s) { return String(s || '').trim().replace(/^【[^】]+】\s*/, ''); }

function paFeedView(task) {
  // 换了一张卡，筛选回到「全部」（和抽屉一致）；筛选的类型这张卡上已经没有时也回到「全部」
  if (pa.ui.tfId !== task.id || (pa.ui.tf !== 'all' && !boardTaskEvents(task).some((e) => e.kind === pa.ui.tf))) {
    pa.ui.tfId = task.id;
    pa.ui.tf = 'all';
  }
  const filters = paEl('div', { class: 'board-drawer-filters' });
  const timeline = paEl('div', { class: 'board-drawer-timeline' });
  boardRenderTimeline(task, { filters, timeline, filter: pa.ui.tf, onFilter: (k) => { pa.ui.tf = k; agentRenderAside(true); } });
  return [filters, timeline];
}
// 右栏底部的「在对话查看」：优先打开当前卡片绑定的 worker 会话；没有绑定 / 会话已不存在 /
// 打开失败时回退到主 Agent 会话。手机 / 浮层顺手收起右栏，docked 只同步可见性（保留开关
// 偏好；worker 会话下右栏本来就整体收起）。两条路径都强制滚到最新消息并给 chat 视图一个
// 短暂的边框反馈（已经在该视图时也能看出点到了）。
async function paGoChat() {
  const task = pa.ui.card ? paTaskById(pa.ui.card) : null;
  const wid = paCardWorkerId(task);
  if (wid) {
    try {
      await openSession(wid); // 会话不存在 / 打不开会 reject → 回退主 Agent
      paFinishGoChat();
      return;
    } catch (e) {
      if (typeof pushToast === 'function') {
        pushToast({ id: 'pa-open-worker', title: '打开 worker 会话失败', body: '已回退到主 Agent 会话：' + ((e && e.message) || e) });
      }
    }
  }
  const ok = await agentShowView('chat');
  if (!ok) return;
  paFinishGoChat();
}
// 卡片绑定的 worker 会话 id：看板的 session 就是绑定的会话（归档后也仍可按 id 打开）；
// 只有没绑 session 时才按 worker 地址名找活跃会话。
function paCardWorkerId(task) {
  if (!task) return '';
  const sid = task.session ? String(task.session).trim() : '';
  if (sid) return sid;
  const w = task.worker ? paSessionByAddr(task.worker) : null;
  return w ? w.id : '';
}
function paFinishGoChat() {
  if (!paDocked()) paCloseAside();
  else paSyncAside();
  paJumpChat();
}
function paJumpChat() {
  const view = $('#chat-view');
  if (!view) return;
  view.classList.remove('pa-jump');
  void view.offsetWidth; // 重启动画
  view.classList.add('pa-jump');
  setTimeout(() => view.classList.remove('pa-jump'), 750);
  const scroll = () => {
    const box = $('#messages');
    if (!box) return;
    state.nearBottom = true; // scrollBottom() 平时只在贴底时跟随；这里是用户显式要求回到底部
    box.scrollTop = box.scrollHeight;
    if (typeof scrollBottom === 'function') scrollBottom();
  };
  scroll();
  requestAnimationFrame(scroll);
  setTimeout(scroll, 120);
  setTimeout(scroll, 400);
  setTimeout(scroll, 900);
}

// ---------- 常驻留言框 ----------
// 给主 Agent 留言：带上选中卡片的上下文和结构化 board_task_id，服务端据此把主 Agent
// 这一轮的最终回复自动写回卡片时间线（与看板抽屉里的 composer 同一条链路）。
// 不选卡片时就是一条普通消息。附件走会话上传目录，规则与抽屉一致。
function paCmpKey() { return pa.ui.card || '_'; }
function paCmpStatus(text, kind) {
  pa.cmp.status = text || '';
  pa.cmp.kind = kind || '';
  paRenderComposer();
}
function paRenderComposer() {
  const box = $('#pa-acmp');
  if (!box) return;
  const main = paPrimaryMain();
  const task = pa.ui.card ? paTaskById(pa.ui.card) : null;
  const c = pa.cmp;
  const sig = [main && main.id, task && task.id, c.att.map((a) => a.name).join(','), c.busy, c.status, c.kind].join('|');
  if (box.dataset.sig === sig && box.childElementCount) return;
  const old = box.querySelector('textarea');
  const foc = !!old && document.activeElement === old;
  if (old) c.drafts[box.dataset.card || '_'] = old.value;
  box.dataset.sig = sig;
  box.dataset.card = paCmpKey();
  box.textContent = '';
  const off = !main;
  const ta = paEl('textarea', {
    id: 'pa-cnote', rows: '1', 'aria-label': '给主 Agent 留言', disabled: off ? true : null,
    placeholder: off ? '还没有主 Agent，先在中间选择 CLI 和模型创建' : (task ? '给 ' + paMainName(main) + ' 留言（带上这张卡的上下文）' : '给 ' + paMainName(main) + ' 留言'),
  });
  ta.value = c.drafts[paCmpKey()] || '';
  const grow = () => { ta.style.height = 'auto'; ta.style.height = Math.min(120, ta.scrollHeight) + 'px'; };
  ta.addEventListener('input', () => { c.drafts[paCmpKey()] = ta.value; grow(); });
  ta.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) { e.preventDefault(); paSendNote(); }
  });
  ta.addEventListener('paste', (e) => {
    const fs = [...((e.clipboardData && e.clipboardData.files) || [])];
    if (fs.length) { e.preventDefault(); paAddFiles(fs); }
  });
  const picker = paEl('input', { type: 'file', multiple: '', hidden: '' });
  picker.addEventListener('change', () => { const fs = [...picker.files]; picker.value = ''; paAddFiles(fs); });
  const attBox = paEl('div', { class: 'attached-images pa-catt' });
  renderAttachmentThumbs(attBox, c.att, () => { c.status = ''; paRenderComposer(); });
  box.ondragover = (e) => { if (e.dataTransfer && e.dataTransfer.types.includes('Files')) e.preventDefault(); };
  box.ondrop = (e) => { if (e.dataTransfer && e.dataTransfer.files.length) { e.preventDefault(); paAddFiles([...e.dataTransfer.files]); } };
  paAppend(box,
    c.status ? paEl('div', { class: 'pa-cstatus ' + c.kind, role: 'status', text: c.status }) : null,
    attBox,
    paEl('div', { class: 'pa-cbox' },
      paEl('button', { type: 'button', class: 'pa-cattach', title: '添加图片或文件', 'aria-label': '添加图片或文件', disabled: off ? true : null, html: paSvg('plus'), onclick: () => picker.click() }),
      ta, picker,
      paEl('button', { type: 'button', class: 'pa-csend', id: 'pa-csend', text: '发送', disabled: off || c.busy ? true : null, onclick: paSendNote })),
    paEl('div', { class: 'pa-cmeta' }, paEl('span', { text: 'Enter 发送 · Shift+Enter 换行' }), paEl('span', { class: 'pa-sp' }),
      paEl('button', { type: 'button', class: 'pa-link', text: '在对话查看 ↗', onclick: () => paGoChat() })));
  grow();
  if (foc) { ta.focus(); ta.setSelectionRange(ta.value.length, ta.value.length); }
}

async function paAddFiles(files) {
  const main = paPrimaryMain();
  const c = pa.cmp;
  if (!main || !files.length) return;
  const noInline = !engineTakesImages(main.engine || 'claude');
  for (const f of files) {
    try {
      if (ALLOWED_MIME.includes(f.type) && !noInline) {
        if (c.att.filter((a) => a.kind === 'image').length >= MAX_IMAGES) { paCmpStatus('最多 ' + MAX_IMAGES + ' 张图片', 'error'); continue; }
        if (f.size > MAX_IMAGE_BYTES) { paCmpStatus('图片过大：' + f.name + '（' + fmtMiB(f.size) + 'MB）', 'error'); continue; }
        const [data, up] = await Promise.all([readAsDataURL(f).then(stripDataURLPrefix), uploadAttachment(f, main.id)]);
        c.att.push({ kind: 'image', media_type: f.type, data, name: up.name, size: up.size, sid: main.id });
        continue;
      }
      if (c.att.filter((a) => a.kind === 'file').length >= MAX_FILES) { paCmpStatus('最多 ' + MAX_FILES + ' 个文件附件', 'error'); continue; }
      if (f.size > MAX_FILE_BYTES) { paCmpStatus('文件过大：' + f.name + '（' + fmtMiB(f.size) + 'MB）', 'error'); continue; }
      const up = await uploadAttachment(f, main.id);
      c.att.push({ kind: 'file', name: up.name, size: up.size, path: up.path, sid: main.id });
    } catch (e) {
      paCmpStatus('添加附件失败：' + (f.name || '') + '（' + ((e && e.message) || e) + '）', 'error');
    }
  }
  c.status = '';
  paRenderComposer();
}

async function paSendNote() {
  const c = pa.cmp;
  const main = paPrimaryMain();
  if (c.busy || !main) return;
  const box = $('#pa-acmp');
  const ta = $('#pa-cnote');
  const text = ta ? ta.value.trim() : '';
  if (!text && !c.att.length) return;
  const task = pa.ui.card ? paTaskById(pa.ui.card) : null;
  const images = c.att.filter((a) => a.kind === 'image').map((a) => ({ media_type: a.media_type, data: a.data }));
  const files = c.att.filter((a) => a.kind === 'file').map((a) => ({ name: a.name, path: a.path }));
  const links = boardAttachmentLinks(c.att, main.id);
  const baseline = task ? boardReplyKeys(task) : [];
  const body = text || '（见附件）';
  const key = paCmpKey();
  c.busy = true;
  paCmpStatus('发送中…', 'queued');
  try {
    await paPost('/api/sessions/' + paEnc(main.id) + '/messages', {
      text: task ? boardTaskContext(task) + '\n' + body : body, images, files, board_task_id: task ? task.id : undefined,
    });
    if (task) await boardAppendUserEvent(task.id, boardEventText(text, links));
    c.att = [];
    delete c.drafts[key];
    // 重画时会把旧输入框的内容存回草稿，得先把框里的字清掉，否则发出去的话又回来了
    const cur = $('#pa-cnote');
    if (cur && box.dataset.card === key) cur.value = '';
    if (typeof bumpSessionActivity === 'function') bumpSessionActivity(main.id);
    if (task) { c.wait = { taskId: task.id, baseline }; c.busy = false; paCmpStatus('已发送 · 回复中…', 'queued'); }
    else { c.busy = false; paCmpStatus('已发送给 ' + paMainName(main), 'ok'); }
  } catch (e) {
    c.wait = null;
    c.busy = false;
    paCmpStatus('发送失败：' + ((e && e.message) || e), 'error');
  }
}

// 看板快照刷新后看有没有出现新的「主 Agent 回复」：服务端在 board 来源的 turn 结束后自动回写。
function paCheckReply() {
  const w = pa.cmp.wait;
  if (!w) return;
  const task = paTaskById(w.taskId);
  if (!task) return;
  const fresh = boardReplyKeys(task).find((k) => !w.baseline.includes(k));
  if (!fresh) return;
  pa.cmp.wait = null;
  pa.cmp.status = '已回复 · 最新回复见「动态」';
  pa.cmp.kind = 'ok';
  if (pa.ui.card === task.id) { pa.ui.tab = 'feed'; pa.ui.tf = 'all'; pa.ui.tfId = task.id; agentRenderAside(true); }
  paRenderComposer();
  // 和抽屉的 boardReplyReveal 一样：定位到这条新回复并闪一下
  const rows = [...document.querySelectorAll('#agent-aside .board-drawer-event[data-kind="主 Agent 回复"]')];
  const row = rows.find((r) => r.dataset.replyKey === fresh) || rows[rows.length - 1];
  if (row) { row.classList.add('board-event-fresh'); if (row.scrollIntoView) row.scrollIntoView({ block: 'nearest' }); }
}

