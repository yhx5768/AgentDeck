/* AgentDeck · 主 Agent 会话里的 worker 回报折叠
 *
 * 主 Agent 的对话流里，worker 经 A2A 的回报和 runtime 的系统通知（worker 异常 / 卡住 / 提问）
 * 会和用户自己的对话混在一起。Personal Agent 模式下，把「连续到达的入站 peer 气泡」按轮次折成一组：
 *   - 一轮 = 一串入站回报，中间只隔着主 Agent 的静默步骤（只有工具调用 / thinking、没有正文）；
 *     用户输入、主 Agent 的正文回复、权限卡等可见内容会把轮次切开；
 *   - 默认折叠，头部给「N 条 worker 回报 · M 条系统通知」、各 worker 的 chip、已入「需要你」的计数；
 *   - 同一 worker、同一正文的重复入站（A2A 总线与 Claude 原生 peer 两条路径各来一次、
 *     重放与实时各来一次）只算一条，多出来的藏起来并在头部标注「去重 N」；
 *   - 展开后每条回报一行摘要，点开才是原始气泡（保留 md 渲染）。
 * 只在 DOM 层做，不改任何事件、不影响主 Agent 的上下文；关掉 Personal Agent 模式会原样还原。
 * 对 replay / 实时 / 「加载更多」都幂等：扫整个 #messages，已折叠的组会被识别并合并。
 */
'use strict';

const paSeenPeers = new WeakSet();
let paFoldRaf = 0;

function paIsPeer(n) { return n.nodeType === 1 && n.classList.contains('msg-peer') && !n.closest('.pa-fold'); }
function paIsFold(n) { return n.nodeType === 1 && n.classList.contains('pa-fold'); }

// 主 Agent 的一步里有没有给人看的正文。只有工具卡 / thinking 的步骤是「静默」的。
function paHasVisibleText(msg) {
  const b = msg.querySelector(':scope > .bubble');
  if (!b) return false;
  for (const ch of b.childNodes) {
    if (ch.nodeType === 3) { if (ch.textContent.trim()) return true; continue; }
    if (ch.nodeType !== 1) continue;
    if (ch.classList.contains('tool-block') || ch.classList.contains('thinking-block')) continue;
    if (ch.textContent.trim()) return true;
  }
  return false;
}
function paIsSilent(n) {
  if (n.nodeType !== 1) return true;
  const c = n.classList;
  if (c.contains('meta-line') || c.contains('result-meta') || c.contains('hidden')) return true;
  if (c.contains('msg-assistant')) return !paHasVisibleText(n);
  return false;
}

function paPeerFrom(node) {
  if (node.dataset.peerFrom) return node.dataset.peerFrom;
  const tag = node.querySelector('.role-tag');
  const txt = tag ? tag.firstChild && tag.firstChild.textContent : '';
  return String(txt || '').replace(/^⇢\s*来自\s*/, '').trim() || '另一个会话';
}
function paNorm(s) { return String(s || '').replace(/\s+/g, ' ').trim(); }

function paMakeRow(node) {
  const from = paPeerFrom(node);
  const ts = paParseTime(node.dataset.peerTs) || Date.now();
  const bubble = node.querySelector('.bubble');
  const text = bubble ? bubble.textContent : '';
  const sys = from.toLowerCase() === 'runtime';
  const tag = paTagOf(text);
  const fresh = !paSeenPeers.has(node);
  paSeenPeers.add(node);
  if (fresh && !state.renderingHistory && !state.replaying && pa.view !== 'chat') pa.unseen++;
  const row = paEl('div', { class: 'pa-rep' + (sys ? ' sys' : ''), dataset: { from, ts: String(ts), dk: from + '\x00' + paNorm(paUntag(text)) } });
  const head = paEl('button', { type: 'button', class: 'pa-rep-h', 'aria-expanded': 'false' },
    paEl('time', { text: paHM(ts) }), paEl('span', { class: 'w', text: sys ? '系统' : from }),
    tag ? paEl('span', { class: 'pa-pill ' + (tag === '受阻' ? 'a' : tag === '需确认' || tag === '需人工' ? 'b' : ''), text: tag }) : null,
    paEl('span', { class: 'tx', text: paClip(paFirstLine(paUntag(text)) || paFirstLine(text), 160) }),
    paEl('span', { class: 'lk-slot' }));
  head.addEventListener('click', (e) => {
    if (e.target.closest('.lk')) return;
    const open = row.classList.toggle('open');
    head.setAttribute('aria-expanded', open ? 'true' : 'false');
  });
  row.append(head, paEl('div', { class: 'pa-rep-b' }, node));
  return row;
}

function paMakeFold() {
  const fold = paEl('div', { class: 'pa-fold' });
  const head = paEl('button', { type: 'button', class: 'pa-fold-h', 'aria-expanded': 'false' });
  head.addEventListener('click', (e) => {
    if (e.target.closest('.lk')) return;
    const open = fold.classList.toggle('open');
    head.setAttribute('aria-expanded', open ? 'true' : 'false');
    if (fold.dataset.key) pa.foldOpen[fold.dataset.key] = open;
    paUpdateFold(fold);
  });
  fold.append(head, paEl('div', { class: 'pa-fold-b' }));
  return fold;
}
// 重算一组的头部：去重、计数、worker chip、已入「需要你」。
function paUpdateFold(fold) {
  const rows = [...fold.querySelectorAll(':scope > .pa-fold-b > .pa-rep')];
  const seen = new Map();
  let dups = 0;
  rows.forEach((r) => {
    const ts = +r.dataset.ts;
    const prev = seen.get(r.dataset.dk);
    const dup = prev !== undefined && Math.abs(ts - prev) < 5 * 60000;
    r.classList.toggle('dup', dup);
    if (dup) dups++;
    else seen.set(r.dataset.dk, ts);
  });
  const live = rows.filter((r) => !r.classList.contains('dup'));
  const reports = live.filter((r) => !r.classList.contains('sys'));
  const sysN = live.length - reports.length;
  const by = new Map();
  reports.forEach((r) => by.set(r.dataset.from, (by.get(r.dataset.from) || 0) + 1));
  // 「已入需要你」：这组里出过回报的 worker，现在还挂在队列里的待办。只在该 worker 在本组的
  // 最后一条回报上放链接——更早的回报未必是这些待办的起因。
  let queued = 0;
  const lastOf = new Map();
  reports.forEach((r) => lastOf.set(r.dataset.from, r));
  rows.forEach((r) => { const slot = r.querySelector('.lk-slot'); if (slot) slot.textContent = ''; });
  by.forEach((_, from) => {
    const w = paSessionByAddr(from);
    const items = w ? paItemsOfSession(w.id) : [];
    if (!items.length) return;
    queued += items.length;
    const slot = lastOf.get(from).querySelector('.lk-slot');
    slot.append(paEl('span', { class: 'lk', role: 'link', tabindex: '0', text: '已入「需要你」→', onclick: (e) => { e.stopPropagation(); agentFocusItem(items[0].id); } }));
  });
  const open = fold.classList.contains('open');
  const head = fold.querySelector(':scope > .pa-fold-h');
  head.textContent = '';
  const parts = [];
  if (reports.length) parts.push(reports.length + ' 条 worker 回报');
  if (sysN) parts.push(sysN + ' 条系统通知');
  const titleEl = paEl('span', { class: 'pa-fold-t' }, paEl('b', { text: parts.join(' · ') || '0 条' }),
    dups ? paEl('span', { class: 'dim', title: '同一 worker 的同一条回报经不同通道到达了多次，只算一条', text: '（去重 ' + dups + '）' }) : null,
    paEl('span', { class: 'st', text: open ? '已展开' : '已折叠' }));
  const chips = paEl('span', { class: 'ws' });
  [...by.entries()].slice(0, 6).forEach(([from, n]) => chips.append(paEl('span', { class: 'pa-pill', title: from, text: paClip(from.replace(/^w[·.\-_ ]/i, ''), 12) + (n > 1 ? ' ×' + n : '') })));
  if (by.size > 6) chips.append(paEl('span', { class: 'pa-pill', text: '+' + (by.size - 6) }));
  if (queued) chips.append(paEl('span', { class: 'pa-pill a lk', role: 'link', text: queued + ' 项已入「需要你」', onclick: (e) => { e.stopPropagation(); agentShowView('queue'); } }));
  head.append(paEl('span', { class: 'chev', html: paSvg('chevR', 's') }), titleEl, chips);
}

function paFoldRun(members) {
  const existing = members.find(paIsFold);
  const target = existing || paMakeFold();
  const wasOpen = members.some((m) => paIsFold(m) && m.classList.contains('open'));
  // 先把组摆到这一串的第一个位置——后面 paMakeRow 会把气泡搬进行里，members[0] 就不在文档流里了
  if (target !== members[0]) members[0].before(target);
  const rows = [];
  members.forEach((m) => {
    if (paIsFold(m)) rows.push(...m.querySelectorAll(':scope > .pa-fold-b > .pa-rep'));
    else rows.push(paMakeRow(m));
  });
  const body = target.querySelector(':scope > .pa-fold-b');
  body.textContent = '';
  body.append(...rows);
  members.forEach((m) => { if (paIsFold(m) && m !== target) m.remove(); });
  const first = rows[0];
  target.dataset.key = first ? first.dataset.from + '@' + first.dataset.ts : '';
  // 「加载更多」往前插了更早的回报，组的 key（第一条回报）会变；已展开的组不能因此合上
  const want = wasOpen || !!pa.foldOpen[target.dataset.key];
  if (want) pa.foldOpen[target.dataset.key] = true;
  if (want !== target.classList.contains('open')) {
    target.classList.toggle('open', want);
    const h = target.querySelector(':scope > .pa-fold-h');
    if (h) h.setAttribute('aria-expanded', want ? 'true' : 'false');
  }
  paUpdateFold(target);
}

function paFoldScan(box) {
  const runs = [];
  let run = null;
  for (const n of [...box.children]) {
    if (paIsPeer(n) || paIsFold(n)) {
      if (!run) { run = []; runs.push(run); }
      run.push(n);
    } else if (!paIsSilent(n)) {
      run = null;
    }
  }
  // 只有一个已折叠的组、没有新来的：不用重建，只刷新头部（worker 状态可能变了）
  runs.forEach((r) => {
    if (r.length === 1 && paIsFold(r[0])) paUpdateFold(r[0]);
    else paFoldRun(r);
  });
  if (runs.length && pa.unseen) paRenderNav();
}

// ---------- 对外挂点（app.js 里 typeof 守卫调用） ----------
function agentFoldNow() {
  if (!agentModeOn() || !state.current || !state.current.main_agent) return;
  const box = $('#messages');
  if (!box) return;
  try {
    paFoldScan(box);
  } catch (e) {
    // 折叠只是展示层的整理，出岔子就退回平铺，绝不能拖垮 flushHistory / 实时渲染
    console.warn('[agent-fold] 折叠失败，退回平铺：', e);
    try { agentUnfoldAll(); } catch (_) { /* 已尽力 */ }
  }
}
function agentFoldSoon() {
  if (paFoldRaf || !agentModeOn()) return;
  paFoldRaf = requestAnimationFrame(() => {
    paFoldRaf = 0;
    agentFoldNow();
    if (state.nearBottom && typeof scrollBottom === 'function') scrollBottom();
  });
}
// 切回普通对话：把折叠的气泡原样放回对话流
function agentUnfoldAll() {
  const box = $('#messages');
  if (!box) return;
  box.querySelectorAll('.pa-fold').forEach((fold) => {
    const nodes = [...fold.querySelectorAll(':scope > .pa-fold-b > .pa-rep > .pa-rep-b > .msg')];
    fold.replaceWith(...nodes);
  });
}
// 队列 / worker 状态变了，已折叠的组头部要跟着刷新（已入「需要你」的计数）
function agentFoldRefresh() {
  const box = $('#messages');
  if (box && agentModeOn()) box.querySelectorAll('.pa-fold').forEach(paUpdateFold);
}

