/* AgentDeck · Personal Agent「需要你」队列视图（#needs-view）
 *
 * 一条队列条目 = 一个需要人动手的动作：权限门 / 提问（55s 窗口，live）与看板卡的
 * 待人工 / 待验收。数据来自 GET /api/needs-you，排序由服务端做，前端不重排；
 * 这里只负责分组（按紧急度 / 按任务）、筛选、倒计时和选中。
 * 「已处理」是本机记录（localStorage），服务端没有这份历史。
 */
'use strict';

const PA_KIND = {
  perm: { label: '权限门', icon: 'shield', hint: '55 秒窗口，超时自动拒绝', verb: '请求你批准' },
  ask: { label: '提问', icon: 'help', hint: '55 秒窗口，超时模型会重新提问', verb: '向你提问' },
  blocked: { label: '阻塞', icon: 'pause', hint: '看板「待人工」· worker 停在一个决策上', verb: '待你拍板' },
  review: { label: '待验收', icon: 'check', hint: '看板「待验收」· 不阻塞其他任务', verb: '提请你验收' },
};
const PA_ORDER = ['perm', 'ask', 'blocked', 'review'];
const PA_TASK_PILL = { '新建': '', '进行中': 'b', '待人工': 'a', '待验收': 'v', '已完成': 'g', '已取消': '' };

function paKindOf(item) { return PA_KIND[item.kind] || PA_KIND.perm; }

// 过滤后的分组。tier：按 perm > ask > blocked > review；task：按任务聚合（保持服务端的紧急顺序）。
function paQueueGroups() {
  const u = pa.ui;
  const kindOK = (i) => u.kind === 'all' || i.kind === u.kind;
  const pend = u.status === 'done' ? [] : paItems().filter(kindOK);
  const done = u.status === 'pending' ? [] : pa.done.filter(kindOK);
  const groups = [];
  if (u.group === 'tier') {
    PA_ORDER.forEach((k) => {
      const items = pend.filter((i) => i.kind === k);
      if (items.length) groups.push({ key: k, label: PA_KIND[k].label, hint: PA_KIND[k].hint, items });
    });
  } else {
    const keys = [];
    pend.forEach((i) => { const k = i.task_id || '_none'; if (!keys.includes(k)) keys.push(k); });
    keys.forEach((k) => {
      const items = pend.filter((i) => (i.task_id || '_none') === k);
      groups.push({ key: 'task:' + k, task: k === '_none' ? null : { id: k, title: items[0].task_title || k, status: items[0].task_status }, items });
    });
  }
  // 刚作答的条目已经离开服务端队列，但它的回执还在右栏里：在待处理视图顶上留一行，
  // 选别的行时才撤（pin 被清掉）。
  const pinned = u.status !== 'done' && u.pin ? pa.done.find((d) => 'done:' + d.id === u.pin) : null;
  if (pinned && !done.includes(pinned)) groups.unshift({ key: 'pinned', label: '刚处理', hint: '回执见右栏', items: [pinned], done: true });
  if (done.length) groups.push({ key: 'done', label: '已处理', hint: '仅本机记录', items: done, done: true });
  return groups;
}
function paQueueFlat() { return paQueueGroups().flatMap((g) => g.items); }

function paQueueSub() {
  const n = pa.needs.counts.all || 0;
  if (!n) return '没有待处理的事';
  const oldest = paItems().reduce((m, i) => Math.min(m, i.created_at || Infinity), Infinity);
  return n + ' 项待处理' + (Number.isFinite(oldest) ? ' · 最久已等 ' + paDur(paNow() - oldest) : '');
}

function paQueueHead() {
  const seg = (v, label) => paEl('button', {
    type: 'button', class: pa.ui.group === v ? 'on' : '', text: label,
    onclick: () => { pa.ui.group = v; paSaveUI(); agentRenderQueue(true); },
  });
  return paEl('header', { class: 'pa-qh' },
    paEl('button', { type: 'button', class: 'icon-btn drawer-toggle', title: '打开会话列表', 'aria-label': '打开会话列表', html: paSvg('menu'), onclick: () => openDrawer() }),
    paEl('div', { class: 'pa-qh-t' }, paEl('h1', { text: '需要你' }), paEl('p', { class: 'sub', text: paQueueSub() })),
    paEl('span', { class: 'pa-sp' }),
    paEl('div', { class: 'pa-seg', role: 'group', 'aria-label': '分组方式' }, seg('tier', '按紧急度'), seg('task', '按任务')));
}

function paQueueTabs() {
  const u = pa.ui;
  const tab = (v, label, n) => paEl('button', {
    type: 'button', class: 'pa-tab' + (u.status === v ? ' on' : ''),
    onclick: () => { u.status = v; paSaveUI(); agentRenderQueue(true); },
  }, label, paEl('em', { text: String(n) }));
  const kinds = ['all', ...PA_ORDER].map((k) => {
    const n = k === 'all' ? pa.needs.counts.all : (pa.needs.counts[k] || 0);
    return paEl('button', {
      type: 'button', class: 'pa-kchip' + (u.kind === k ? ' on' : ''),
      onclick: () => { u.kind = k; agentRenderQueue(true); },
    }, k === 'all' ? null : paEl('i', { class: 'pa-kd k-' + k }), k === 'all' ? '全部' : PA_KIND[k].label, ' ', String(n));
  });
  return [
    paEl('div', { class: 'pa-tabs' }, tab('pending', '待处理', pa.needs.counts.all || 0), tab('done', '已处理', pa.done.length),
      tab('all', '全部', (pa.needs.counts.all || 0) + pa.done.length)),
    paEl('div', { class: 'pa-kindbar' }, kinds),
  ];
}
// ---------- 行 ----------
function paItemPill(it) {
  if (it.kind === 'perm') return paEl('span', { class: 'pa-pill r', text: '限时' });
  if (it.kind === 'ask') return paEl('span', { class: 'pa-pill b', text: '限时' });
  if (it.kind === 'blocked') {
    const late = paNow() - it.created_at > 10 * 60000;
    return [paEl('span', { class: 'pa-pill a', text: '待人工' }), late ? paEl('span', { class: 'pa-pill a', html: paSvg('zap', 's') + '已提档' }) : null];
  }
  return paEl('span', { class: 'pa-pill v', text: '待验收' });
}

function paQueueRow(it) {
  const k = paKindOf(it);
  const live = it.state === 'live';
  const left = paLeftMs(it);
  let right;
  if (live) right = paEl('span', { class: 'pa-cd' + (left < 15000 ? ' low' : ''), 'data-pa-cd': it.id, text: left > 0 ? paMMSS(left) : '已超时' });
  else if (it.kind === 'blocked') right = paEl('span', { 'data-pa-wait': it.created_at, text: '已等 ' + paDur(paNow() - it.created_at) });
  else right = paEl('span', { 'data-pa-rel': it.created_at, text: paRel(it.created_at) });
  const taskLine = it.task_title && it.task_title !== it.title ? it.task_title : '';
  const row = paEl('div', {
    class: 'pa-row k-' + it.kind + (pa.ui.sel === it.id ? ' sel' : '') + (live ? ' hasbar' : '') + (pa.freshAt[it.id] && paNow() - pa.freshAt[it.id] < 2400 ? ' fresh' : ''),
    role: 'button', tabindex: '0', 'data-pa-id': it.id,
  },
  paEl('span', { class: 'pa-tile', html: paSvg(k.icon, 'l') }),
  paEl('div', { class: 'pa-rm' },
    paEl('div', { class: 'pa-rt pa-ell', title: it.title, text: it.title }),
    paEl('div', { class: 'pa-rs' }, paItemPill(it), paEl('span', { class: 'who', text: it.session_name || it.peer_name || '未绑定会话' }),
      taskLine ? [paEl('span', { class: 'sep', text: '·' }), paEl('span', { class: 'pa-ell', text: taskLine })] : null)),
  paEl('div', { class: 'pa-rr' }, right));
  if (live) row.append(paEl('div', { class: 'pa-bar' }, paEl('i', { 'data-pa-bar': it.id, style: 'width:' + Math.max(0, Math.min(100, (left / (pa.needs.windowMs || 55000)) * 100)) + '%' })));
  return row;
}

function paDoneRow(d) {
  const id = 'done:' + d.id;
  const k = PA_KIND[d.kind] || PA_KIND.perm;
  return paEl('div', { class: 'pa-row done k-' + d.kind + (pa.ui.sel === id ? ' sel' : ''), role: 'button', tabindex: '0', 'data-pa-id': id },
    paEl('span', { class: 'pa-tile', html: paSvg('check', 'l') }),
    paEl('div', { class: 'pa-rm' },
      paEl('div', { class: 'pa-rt pa-ell', title: d.title, text: d.title }),
      paEl('div', { class: 'pa-rs' }, paEl('span', { class: 'pa-pill g', text: d.label || '已处理' }),
        paEl('span', { class: 'who', text: d.sessionName || '' }),
        d.taskTitle ? [paEl('span', { class: 'sep', text: '·' }), paEl('span', { class: 'pa-ell', text: d.taskTitle })] : null,
        paEl('span', { class: 'sep', text: '·' }), paEl('span', { text: k.label }))),
    paEl('div', { class: 'pa-rr', text: paHM(d.at) }));
}

function paQueueEmpty() {
  const p = pa.ui.status === 'pending';
  const wrap = paEl('div', { class: 'pa-empty' },
    paEl('div', { class: 'big', html: paSvg('check', 'l') }),
    paEl('h3', { text: p ? '没有待处理的事' : '这里还没有记录' }),
    paEl('p', { text: p ? 'worker 的回报留在「主 Agent 会话」里（已折叠）；真正需要你拍板的事会第一时间出现在这里。' : '你在这里处理过的条目会连同回执留在这里（只存本机）。' }));
  if (p && pa.done.length) wrap.append(paEl('button', { type: 'button', class: 'ghost-btn', text: '看已处理的', onclick: () => { pa.ui.status = 'done'; paSaveUI(); agentRenderQueue(true); } }));
  return wrap;
}

function paQueueNotice() {
  const out = [];
  if (pa.needs.err) {
    out.push(paEl('div', { class: 'pa-note err' }, paEl('span', { class: 'pa-note-t', text: '队列读取失败：' + pa.needs.err + (pa.needs.items.length ? '（下面是上一次的数据）' : '') }),
      paEl('button', { type: 'button', class: 'ghost-btn', text: '重试', onclick: () => agentRefreshNeeds() })));
  }
  if (pa.needs.board && pa.needs.board.ok === false) {
    out.push(paEl('div', { class: 'pa-note warn', text: '看板不可用：' + (pa.needs.board.error || '未知错误') + '。队列里暂时只有权限门和提问，待人工 / 待验收的卡看不到。' }));
  }
  return out;
}

function paQueueGroupHead(g) {
  const n = g.items.length;
  if (g.task || (g.key && g.key.startsWith('task:'))) {
    const t = g.task;
    return paEl('div', { class: 'pa-grp' }, paEl('b', { class: 'pa-ell', text: t ? t.title : '未绑定看板卡' }), paEl('em', { text: String(n) }),
      t && t.status ? paEl('span', { class: 'gh' }, paEl('span', { class: 'pa-pill ' + (PA_TASK_PILL[t.status] || ''), text: t.status })) : null);
  }
  return paEl('div', { class: 'pa-grp' }, paEl('b', { text: g.label }), paEl('em', { text: String(n) }), g.hint ? paEl('span', { class: 'gh', text: g.hint }) : null);
}
// ---------- 选中 ----------
// 选中项消失（别处作答 / 超时）就挪到当前第一项；刚作答的条目靠 pin 留住，回执还要给人看。
function paEnsureSel() {
  if (!agentModeOn() || pa.view !== 'queue') return;
  const flat = paQueueFlat();
  const ok = (id) => id && flat.some((x) => (x.kind && x.state ? x.id : 'done:' + x.id) === id);
  if (ok(pa.ui.sel) || (pa.ui.pin && pa.ui.sel === pa.ui.pin)) return;
  const first = flat.find((x) => x.state) || null;
  if (first) { pa.ui.sel = first.id; pa.ui.card = first.task_id || ''; pa.ui.tab = 'ov'; }
  else if (pa.ui.sel) { pa.ui.sel = ''; }
}

function agentSelectItem(id, opts) {
  opts = opts || {};
  const isDone = String(id).startsWith('done:');
  const it = isDone ? pa.done.find((d) => 'done:' + d.id === id) : paItemById(id);
  if (!it) return;
  if (id !== pa.ui.sel) pa.ui.pin = isDone ? id : '';
  pa.ui.sel = id;
  pa.ui.card = (isDone ? it.taskId : it.task_id) || '';
  pa.ui.tab = 'ov';
  agentRenderQueue(true);
  agentRenderCluster();
  paSyncAside(opts.reveal !== false);
  if (opts.scroll) {
    const row = document.querySelector('#needs-view .pa-row.sel');
    if (row && row.scrollIntoView) row.scrollIntoView({ block: 'nearest' });
  }
}
function paMoveSel(d) {
  const flat = paQueueFlat().map((x) => (x.state ? x.id : 'done:' + x.id));
  if (!flat.length) return;
  const n = flat.indexOf(pa.ui.sel);
  agentSelectItem(flat[Math.max(0, Math.min(flat.length - 1, n + d))], { reveal: false, scroll: true });
}

// ---------- 渲染 ----------
function agentRenderQueue(force) {
  const root = $('#needs-view');
  if (!root || !agentModeOn()) return;
  paEnsureSel();
  if (root.classList.contains('hidden')) return;
  const groups = paQueueGroups();
  const sig = JSON.stringify([pa.ui.status, pa.ui.kind, pa.ui.group, pa.ui.sel, pa.ui.pin, pa.needs.err, pa.needs.loaded,
    pa.needs.board && pa.needs.board.ok, pa.needs.counts, pa.done.length,
    groups.map((g) => g.key + ':' + g.items.map((i) => i.id + '|' + (i.title || '') + '|' + (i.state || '')).join(','))]);
  if (!force && root.dataset.sig === sig) return;
  root.dataset.sig = sig;
  const old = root.querySelector('.pa-qb');
  const top = old ? old.scrollTop : 0;
  const body = paEl('div', { class: 'pa-qb' });
  body.append(...paQueueNotice());
  if (!pa.needs.loaded) {
    body.append(paEl('div', { class: 'pa-empty' }, paEl('p', { text: '正在读取队列…' })));
  } else if (!groups.length) {
    body.append(paQueueEmpty());
  } else {
    const list = paEl('div', { class: 'pa-list' });
    groups.forEach((g) => {
      list.append(paQueueGroupHead(g));
      g.items.forEach((i) => list.append(g.done ? paDoneRow(i) : paQueueRow(i)));
    });
    body.append(list);
  }
  const note = paEl('button', { type: 'button', class: 'pa-note link', onclick: () => agentShowView('chat') },
    paEl('span', { html: paSvg('layers') }),
    paEl('span', { class: 'pa-note-t', text: '不需要你动手的 worker 回报已折叠，留在「主 Agent 会话」里，不会混进这个队列。' }),
    paEl('span', { class: 'go', text: '去看看 →' }));
  body.append(note);
  root.textContent = '';
  root.append(paQueueHead(), ...paQueueTabs(), body);
  body.scrollTop = top;
}

function paQueueBind() {
  const root = $('#needs-view');
  if (!root || root.dataset.paBound) return;
  root.dataset.paBound = '1';
  root.addEventListener('click', (e) => {
    const row = e.target.closest('.pa-row');
    if (row && row.dataset.paId) agentSelectItem(row.dataset.paId);
  });
  root.addEventListener('keydown', (e) => {
    if ((e.key === 'Enter' || e.key === ' ') && e.target.matches && e.target.matches('.pa-row')) {
      e.preventDefault();
      e.target.click();
    }
  });
  document.addEventListener('keydown', (e) => {
    if (!agentModeOn() || pa.view !== 'queue' || e.metaKey || e.ctrlKey || e.altKey) return;
    const t = e.target;
    if (t && (/^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName) || t.isContentEditable)) return;
    if (e.key === 'j') paMoveSel(1);
    else if (e.key === 'k') paMoveSel(-1);
  });
}
paQueueBind();

