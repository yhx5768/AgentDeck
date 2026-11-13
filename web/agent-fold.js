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
 * 主 Agent 自己执行时的「静默步骤」（没有正文、只有工具调用 / thinking）同样会刷屏：连续的一段折成
 * 一组「N 个工具调用 · M 步操作」，默认折叠、点开看每一步（每步一行摘要，点开才是原始工具卡）。
 * 头部按工具类型小计（看板 / 读取会话 / Bash / 其他）；整段都是看板操作时改给动作小计（评论 ×N、
 * 更新 ×N）。thinking 一起收进组里，不单独刷屏；带正文的回复、用户输入、worker 回报组不折。
 * 出站 A2A 发件卡（含单条）单独折成「N 条 A2A 发送」，默认展开一层（组头 + 每条摘要行），
 * 组头保留收件人 / 任务 id / 预览 / 失败标记，用户可手动折叠；只有 send-only 步骤成组，
 * 带正文的混合步骤仍把正文和发件卡留在原处。
 * 只在 DOM 层做，不改任何事件、不影响主 Agent 的上下文；关掉 Personal Agent 模式会原样还原。
 * 对 replay / 实时 / 「加载更多」都幂等：扫整个 #messages，已折叠的组会被识别并合并。
 */
'use strict';

const paSeenPeers = new WeakSet();
let paFoldRaf = 0;

function paIsPeer(n) { return n.nodeType === 1 && n.classList.contains('msg-peer') && !n.closest('.pa-fold'); }
function paIsFold(n) { return n.nodeType === 1 && n.classList.contains('pa-fold'); }
function paIsSilentFold(n) { return paIsFold(n) && n.classList.contains('silent'); }
function paIsA2aFold(n) { return paIsFold(n) && n.classList.contains('a2a'); }

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

// ---------- 静默步骤 / 工具分类 ----------
const PA_BOARD_TOOL = /(?:^|__|\.)board_(list|get|create|update|comment|bind_worker)$/i; // agentdeck MCP：mcp__agentdeck__board_comment
const PA_BOARD_CMD = /\bagentdeck(?:\.py)?\s+board\s+([\w-]+)/i; // Bash：agentdeck board comment --task …
const PA_READ_TOOL = /(?:^|__|\.)(get_session|read_session_messages|search_messages|list_sessions)$/i;
const PA_VERB = { comment: '评论', update: '更新', create: '建卡', get: '读取', list: '列表', bind_worker: '派工', status: '状态', assign: '派工', artifact: '产物', archive: '归档' };
const PA_TYPES = [['board', '看板'], ['read', '读取会话'], ['bash', 'Bash'], ['other', '其他']];

function paToolMeta(tb) {
  if (tb.classList.contains('thinking-block')) return { type: 'thinking', name: '思考', sum: '' };
  const txt = (sel) => ((tb.querySelector(sel) || {}).textContent || '').trim();
  const name = txt(':scope > summary .tool-name');
  const sum = txt(':scope > summary .tool-summary-text');
  const m = PA_BOARD_TOOL.exec(name);
  if (m) {
    let input = null;
    try { input = JSON.parse(txt(':scope > .tool-body')); } catch (_) { /* 摘要里有就够用 */ }
    return { type: 'board', verb: m[1].toLowerCase(), name: 'board_' + m[1].toLowerCase(), sum, input: input && typeof input === 'object' ? input : null };
  }
  if (/^bash$/i.test(name)) {
    const c = PA_BOARD_CMD.exec(sum);
    if (c) return { type: 'board', verb: c[1].toLowerCase(), name: 'Bash', sum, input: null };
    return { type: 'bash', name: 'Bash', sum };
  }
  if (PA_READ_TOOL.test(name)) return { type: 'read', name: name.split('__').pop(), sum };
  return { type: 'other', name: name.split('__').pop() || name || '工具', sum };
}
// 一个助手步骤：至少有一张工具卡（thinking 也算）。出站 A2A 发件卡（.tool-peer-send）单独分出来：
// 它们是给人看的「发送」记录，不能跟普通工具一起埋进静默折叠。
//   - sendOnly：整条消息只有发件卡（无正文、无其它工具）——可以整条进 A2A 发送组；
//   - partial：消息里还有正文或发件卡要留在原处，只把 others（其它工具 / thinking）折起来；
//   - 没有工具卡返回 null。
function paStepOf(n) {
  if (n.nodeType !== 1 || !n.classList.contains('msg-assistant')) return null;
  const blocks = [...n.querySelectorAll(':scope > .bubble > .tool-block')];
  if (!blocks.length) return null;
  const sends = blocks.filter((b) => b.classList.contains('tool-peer-send'));
  const others = blocks.filter((b) => !b.classList.contains('tool-peer-send'));
  const text = paHasVisibleText(n);
  return {
    msg: n, blocks, sends, others, metas: others.map(paToolMeta),
    partial: text || sends.length > 0,
    sendOnly: sends.length > 0 && others.length === 0 && !text,
  };
}
function paCountBy(list) {
  const m = new Map();
  list.forEach((k) => m.set(k, (m.get(k) || 0) + 1));
  return [...m.entries()].map(([k, v]) => k + ':' + v).join(',');
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

// 一次看板操作在折叠组里的一行摘要：卡片标题 + 评论 / 状态等要点（MCP 的入参从工具卡 body 的 JSON 里取）。
function paOpText(c) {
  const i = c.input;
  const task = i && i.id ? paTaskById(i.id) : null;
  const card = task ? (task.title || task.id) : (i && i.id) || '';
  let t = '';
  if (i) {
    if (c.verb === 'comment') t = i.text || '';
    else if (c.verb === 'update') t = [i.status ? '状态 → ' + i.status : '', i.progress || i.status_reason || i.title || ''].filter(Boolean).join('；');
    else if (c.verb === 'create') t = i.title || '';
    else if (c.verb === 'bind_worker') t = i.worker || '';
  } else {
    const m = /--text\s+(?:"([^"]*)"|'([^']*)')/.exec(c.sum);
    t = m ? (m[1] || m[2] || '') : c.sum.replace(PA_BOARD_CMD, '').trim();
  }
  t = String(t).replace(/\s+/g, ' ').trim();
  return (card ? paClip(card, 16) + ' · ' : '') + paClip(t, 100);
}

// 一个静默步骤在折叠组里的一行：时间 + 这步在干什么（看板操作给动作，混合工具给工具名 / 摘要，
// 只有 thinking 就给「思考」和第一行）。带正文的消息只把工具卡搬进行里，正文留在原处。
function paMakeStepRow(step) {
  const node = step.msg;
  const metas = step.metas;
  const ts = paParseTime(((node.querySelector('.msg-time') || {}).textContent || '').trim()) || Date.now();
  const calls = metas.filter((m) => m.type !== 'thinking');
  const think = metas.length - calls.length;
  const pure = calls.length > 0 && calls.every((c) => c.type === 'board');
  let w, pill, tx;
  if (!calls.length) {
    w = '思考';
    pill = '思考';
    tx = paFirstLine(((node.querySelector('.thinking-block .tool-body') || {}).textContent || '')) || '这一步只有思考';
  } else if (pure) {
    w = '看板';
    pill = [...new Set(calls.map((c) => PA_VERB[c.verb] || c.verb))].join(' / ');
    tx = calls.map(paOpText).join('　');
  } else {
    w = '工具';
    pill = calls.length + ' 个工具';
    tx = calls.map((c) => c.name + (c.sum ? ' · ' + c.sum : '')).join('　');
  }
  tx = paClip(String(tx).replace(/\s+/g, ' ').trim(), 180);
  const types = {};
  calls.forEach((c) => { types[c.type] = (types[c.type] || 0) + 1; });
  const typesStr = Object.entries(types).map(([k, v]) => k + ':' + v).join(',');
  const verbsStr = calls.filter((c) => c.type === 'board').length
    ? paCountBy(calls.filter((c) => c.type === 'board').map((c) => c.verb)) : '';
  const row = paEl('div', { class: 'pa-rep step' + (step.partial ? ' part' : ''), dataset: {
    ts: String(ts), n: String(calls.length), think: String(think), pure: pure ? '1' : '0', types: typesStr, verbs: verbsStr,
    partial: step.partial ? '1' : '0',
  } });
  const head = paEl('button', { type: 'button', class: 'pa-rep-h', 'aria-expanded': 'false' },
    paEl('time', { text: paHM(ts) }), paEl('span', { class: 'w', text: w }),
    paEl('span', { class: 'pa-pill', text: pill }),
    paEl('span', { class: 'tx', text: tx }));
  head.addEventListener('click', () => {
    const open = row.classList.toggle('open');
    head.setAttribute('aria-expanded', open ? 'true' : 'false');
  });
  if (step.partial) {
    // 只把「其它工具」搬到组里；正文和发件卡留在消息壳里；还原时按这个引用放回去
    const part = paEl('div', { class: 'pa-tool-part' });
    part.append(...step.others);
    row.__paSrc = node;
    row.__paBlocks = step.others;
    row.append(head, paEl('div', { class: 'pa-rep-b' }, part));
  } else {
    row.append(head, paEl('div', { class: 'pa-rep-b' }, node));
  }
  return row;
}

// ---------- 出站 A2A 发送组 ----------
// 发件卡（含单条）都折进「N 条 A2A 发送」组，组头保留收件人 / 任务 id / 预览 / 失败标记，
// 点开看每条原始发件卡（收件人跳转、markdown、投递结果都在卡里）。
function paSendCards(msg) {
  return [...msg.querySelectorAll(':scope > .bubble > .tool-peer-send')];
}
function paSendMeta(card) {
  const to = String(card.dataset.peerSendTo || ((card.querySelector('.peer-send-head .peer-send-to') || {}).textContent || '')).trim();
  const id = String(card.dataset.peerSendId || '').trim();
  const text = String(((card.querySelector('.peer-send-msg') || {}).textContent) || '').trim();
  const summary = String(((card.querySelector(':scope > summary .tool-summary-text') || {}).textContent) || '').trim();
  const tag = paTagOf(text);
  return {
    to, id, tag, failed: card.classList.contains('peer-send-failed'),
    preview: paClip(paFirstLine(paUntag(text)) || paFirstLine(text) || summary, 100),
  };
}
function paMakeA2aRow(node) {
  const metas = paSendCards(node).map(paSendMeta);
  const ts = paParseTime(((node.querySelector('.msg-time') || {}).textContent || '').trim()) || Date.now();
  const tos = [...new Set(metas.map((m) => m.to).filter(Boolean))];
  const ids = [...new Set(metas.map((m) => m.id).filter(Boolean))];
  const pv = paClip(metas.map((m) => m.preview).filter(Boolean).join('　'), 160);
  const fail = metas.filter((m) => m.failed).length;
  const row = paEl('div', { class: 'pa-rep a2a-send', dataset: {
    ts: String(ts), n: String(metas.length), tos: JSON.stringify(tos), ids: JSON.stringify(ids), pv, fail: String(fail),
  } });
  const pills = [paEl('span', { class: 'w', text: '⇢' })];
  metas[0] && metas[0].tag ? pills.push(paEl('span', { class: 'pa-pill v', text: metas[0].tag })) : null;
  (tos.length ? tos.slice(0, 3) : ['A2A']).forEach((to) => pills.push(paEl('span', { class: 'pa-pill v', title: to, text: paClip(to, 14) })));
  if (ids.length) pills.push(paEl('span', { class: 'pa-pill mono', title: ids.join(' · '), text: '#' + paClip(ids.join(' · #'), 26) }));
  if (fail) pills.push(paEl('span', { class: 'pa-pill bad', text: '失败' }));
  const head = paEl('button', { type: 'button', class: 'pa-rep-h', 'aria-expanded': 'false' },
    paEl('time', { text: paHM(ts) }), ...pills, paEl('span', { class: 'tx', text: pv }));
  head.addEventListener('click', () => {
    const open = row.classList.toggle('open');
    head.setAttribute('aria-expanded', open ? 'true' : 'false');
  });
  row.append(head, paEl('div', { class: 'pa-rep-b' }, node));
  return row;
}
function paUpdateA2a(fold) {
  const rows = [...fold.querySelectorAll(':scope > .pa-fold-b > .pa-rep')];
  const tos = new Set();
  const ids = new Set();
  let n = 0, fail = 0, pv = '';
  rows.forEach((r) => {
    n += +r.dataset.n || 0;
    fail += +r.dataset.fail || 0;
    try { JSON.parse(r.dataset.tos || '[]').forEach((x) => tos.add(x)); } catch (_) { /* 忽略坏数据 */ }
    try { JSON.parse(r.dataset.ids || '[]').forEach((x) => ids.add(x)); } catch (_) { /* 忽略坏数据 */ }
    if (!pv && r.dataset.pv) pv = r.dataset.pv;
  });
  const open = fold.classList.contains('open');
  const head = fold.querySelector(':scope > .pa-fold-h');
  head.textContent = '';
  const chips = paEl('span', { class: 'ws' });
  [...tos].slice(0, 5).forEach((to) => chips.append(paEl('span', { class: 'pa-pill v', title: to, text: paClip(to, 12) })));
  [...ids].slice(0, 3).forEach((id) => chips.append(paEl('span', { class: 'pa-pill mono', title: id, text: '#' + paClip(id, 16) })));
  if (fail) chips.append(paEl('span', { class: 'pa-pill bad', text: '失败 ×' + fail }));
  head.append(paEl('span', { class: 'chev', html: paSvg('chevR', 's') }),
    paEl('span', { class: 'pa-fold-t' },
      paEl('b', { text: n + ' 条 A2A 发送' }),
      pv ? paEl('span', { class: 'pv', text: pv }) : null,
      paEl('span', { class: 'st', text: open ? '已展开' : '已折叠' })), chips);
}

function paMakeFold(kind) {
  const fold = paEl('div', { class: 'pa-fold' + (kind === 'silent' ? ' silent' : kind === 'a2a' ? ' a2a' : '') });
  const head = paEl('button', { type: 'button', class: 'pa-fold-h', 'aria-expanded': 'false' });
  head.addEventListener('click', (e) => {
    if (e.target.closest('.lk')) return;
    const open = fold.classList.toggle('open');
    head.setAttribute('aria-expanded', open ? 'true' : 'false');
    if (fold.dataset.key) pa.foldOpen[fold.dataset.key] = open;
    (paIsSilentFold(fold) ? paUpdateSilent : paIsA2aFold(fold) ? paUpdateA2a : paUpdateFold)(fold);
  });
  fold.append(head, paEl('div', { class: 'pa-fold-b' }));
  return fold;
}
// 重算一组的头部：去重、计数、worker chip、已入「需要你」。
function paUpdateFold(fold) {
  const rows = [...fold.querySelectorAll(':scope > .pa-fold-b > .pa-rep')].filter((r) => r.dataset.dk);
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

// 静默步骤组的头部：总工具调用 / 步数 + 类型小计；整组都是看板操作时改给动作小计。
function paUpdateSilent(fold) {
  const rows = [...fold.querySelectorAll(':scope > .pa-fold-b > .pa-rep')];
  let calls = 0, think = 0, any = false, pure = true;
  const types = new Map();
  const verbs = new Map();
  rows.forEach((r) => {
    const n = +r.dataset.n || 0;
    calls += n;
    think += +r.dataset.think || 0;
    if (n) any = true;
    if (r.dataset.pure !== '1') pure = false;
    (r.dataset.types || '').split(',').filter(Boolean).forEach((p) => {
      const [k, v] = p.split(':');
      types.set(k, (types.get(k) || 0) + (+v || 1));
    });
    (r.dataset.verbs || '').split(',').filter(Boolean).forEach((p) => {
      const [k, v] = p.split(':');
      verbs.set(k, (verbs.get(k) || 0) + (+v || 1));
    });
  });
  pure = pure && any; // 至少有一个调用，且全部是看板操作
  const open = fold.classList.contains('open');
  const head = fold.querySelector(':scope > .pa-fold-h');
  head.textContent = '';
  const chips = paEl('span', { class: 'ws' });
  if (pure) {
    [...verbs.entries()].slice(0, 6).forEach(([v, n]) => chips.append(paEl('span', { class: 'pa-pill', text: (PA_VERB[v] || v) + (n > 1 ? ' ×' + n : '') })));
  } else {
    PA_TYPES.forEach(([k, label]) => {
      const n = types.get(k) || 0;
      if (n) chips.append(paEl('span', { class: 'pa-pill', text: label + (n > 1 ? ' ×' + n : '') }));
    });
    if (!calls && think) chips.append(paEl('span', { class: 'pa-pill', text: '思考 ×' + think }));
  }
  head.append(paEl('span', { class: 'chev', html: paSvg('chevR', 's') }),
    paEl('span', { class: 'pa-fold-t' },
      paEl('b', { text: calls ? calls + ' 个工具调用 · ' + rows.length + ' 步操作' : rows.length + ' 步思考' }),
      paEl('span', { class: 'st', text: open ? '已展开' : '已折叠' })), chips);
}

function paFoldRun(members, kind) {
  const silent = kind === 'silent';
  const a2a = kind === 'a2a';
  const existing = members.find(paIsFold);
  const target = existing || paMakeFold(kind);
  const wasOpen = members.some((m) => paIsFold(m) && m.classList.contains('open'));
  const lead = silent && !paIsFold(members[0]) ? paStepOf(members[0]) : null;
  if (lead && lead.partial) {
    // 带正文的回复：正文留在原处，组挂在这条回复下面
    members[0].after(target);
  } else if (target !== members[0]) {
    // 纯工具 / thinking 的步骤 / 发件消息：组占这一段第一个位置，消息随后被搬进组里
    members[0].before(target);
  }
  const rows = [];
  members.forEach((m) => {
    if (paIsFold(m)) rows.push(...m.querySelectorAll(':scope > .pa-fold-b > .pa-rep'));
    else if (silent) {
      const step = paStepOf(m);
      if (step) rows.push(paMakeStepRow(step));
    } else if (a2a) rows.push(paMakeA2aRow(m));
    else rows.push(paMakeRow(m));
  });
  const body = target.querySelector(':scope > .pa-fold-b');
  body.textContent = '';
  body.append(...rows);
  members.forEach((m) => { if (paIsFold(m) && m !== target) m.remove(); });
  const first = rows[0];
  target.dataset.key = first ? (silent ? 'silent' : a2a ? 'a2a' : first.dataset.from) + '@' + first.dataset.ts : '';
  const key = target.dataset.key;
  // worker 回报组和 A2A 发送组默认展开；静默步骤组仍默认折叠。
  // 用户手动折叠/展开过就按 pa.foldOpen 的显式状态来；「加载更多」前插更早的内容会让组 key 变，
  // existing 的 DOM 展开状态优先，所以不会把用户折叠的组又撑开。
  const openByDefault = !silent;
  const want = existing
    ? (wasOpen || !!(key && pa.foldOpen[key]))
    : (key && (key in pa.foldOpen) ? !!pa.foldOpen[key] : openByDefault);
  if (key) pa.foldOpen[key] = want;
  if (want !== target.classList.contains('open')) {
    target.classList.toggle('open', want);
    const h = target.querySelector(':scope > .pa-fold-h');
    if (h) h.setAttribute('aria-expanded', want ? 'true' : 'false');
  }
  (silent ? paUpdateSilent : a2a ? paUpdateA2a : paUpdateFold)(target);
}

// 一次扫描处理两种组：worker 回报组（入站 peer 气泡）和静默步骤组（没有正文、只有工具 / thinking 的
// 连续助手步骤）。回报组对静默步骤透明（回报可以跨过静默步骤连成一轮）；静默组以回报组 / 用户输入 /
// 正文回复为边界，不跨过去吞内容。
function paFoldScan(box) {
  let repRuns = 0;
  const process = (kind, isMember, isBarrier) => {
    const found = [];
    let run = null;
    for (const n of [...box.children]) {
      if (isMember(n)) {
        if (!run) { run = []; found.push(run); }
        run.push(n);
      } else if (isBarrier(n)) {
        run = null;
      }
    }
    // 只有一个已折叠的组、没有新来的：不用重建，只刷新头部（worker 状态可能变了）
    found.forEach((r) => {
      if (r.length === 1 && paIsFold(r[0])) (kind === 'silent' ? paUpdateSilent : paUpdateFold)(r[0]);
      else paFoldRun(r, kind);
    });
    return found.length;
  };
  // 0) 出站 A2A 发送组：每一段连续 send-only 步骤都折（含单条），默认展开一层、显示每条摘要行。
  const ignorable = (n) => n.nodeType === 1 && (n.classList.contains('meta-line') || n.classList.contains('result-meta') || n.classList.contains('hidden'));
  const sendFound = [];
  let sendRun = null;
  for (const n of [...box.children]) {
    const step = !paIsFold(n) && n.nodeType === 1 && n.classList.contains('msg-assistant') ? paStepOf(n) : null;
    if (paIsA2aFold(n) || (step && step.sendOnly)) {
      if (!sendRun) { sendRun = []; sendFound.push(sendRun); }
      sendRun.push(n);
      continue;
    }
    if (!ignorable(n)) sendRun = null;
  }
  sendFound.forEach((r) => {
    if (r.length === 1 && paIsA2aFold(r[0])) paUpdateA2a(r[0]);
    else paFoldRun(r, 'a2a');
  });
  repRuns = process('rep',
    (n) => paIsPeer(n) || (paIsFold(n) && !paIsSilentFold(n) && !paIsA2aFold(n)),
    (n) => !(paIsSilent(n) || paIsSilentFold(n)));
  // 静默步骤：纯工具 / thinking 的连续步骤成组；带正文的步骤起新的一组（工具折到那条回复下面，
  // 正文不能跟着进组）。回报组 / 用户输入 / 正文回复是边界。
  const found = [];
  let run = null;
  for (const n of [...box.children]) {
    if (paIsSilentFold(n)) {
      if (!run) { run = []; found.push(run); }
      run.push(n);
      continue;
    }
    const step = paStepOf(n);
    if (step) {
      // 发件卡 / 带正文的回复要留在对话流里，不能整条进静默组
      if (step.sendOnly || !step.others.length) { run = null; continue; }
      if (step.partial) { run = [n]; found.push(run); }
      else { if (!run) { run = []; found.push(run); } run.push(n); }
      continue;
    }
    if (!paIsSilent(n)) run = null;
  }
  found.forEach((r) => {
    if (r.length === 1 && paIsFold(r[0])) paUpdateSilent(r[0]);
    else paFoldRun(r, 'silent');
  });
  if (repRuns && pa.unseen) paRenderNav();
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
    const out = [];
    fold.querySelectorAll(':scope > .pa-fold-b > .pa-rep').forEach((row) => {
      if (row.dataset.partial === '1') {
        // 带正文的消息：把工具卡放回原消息（消息还在就放回它下面）
        const part = row.querySelector(':scope > .pa-rep-b > .pa-tool-part');
        const blocks = part ? [...part.children] : [];
        const src = row.__paSrc;
        const bubble = src && src.isConnected ? src.querySelector(':scope > .bubble') : null;
        if (bubble) { bubble.append(...blocks); return; }
        if (blocks.length) {
          const msg = document.createElement('div');
          msg.className = 'msg msg-assistant';
          const b = document.createElement('div');
          b.className = 'bubble';
          b.append(...blocks);
          msg.append(b);
          out.push(msg);
        }
      } else {
        const msg = row.querySelector(':scope > .pa-rep-b > .msg');
        if (msg) out.push(msg);
      }
    });
    fold.replaceWith(...out);
  });
}
// 队列 / worker 状态变了，已折叠的组头部要跟着刷新（已入「需要你」的计数）
function agentFoldRefresh() {
  const box = $('#messages');
  if (box && agentModeOn()) box.querySelectorAll('.pa-fold:not(.silent):not(.a2a)').forEach(paUpdateFold);
}

