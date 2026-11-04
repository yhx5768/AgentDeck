/* cc-webui 轨迹视图（移植自 dsh 的 trajectory 插件设计）：
 * 把会话事件流投影成按轮(turn)分组的事件台账。
 *
 * 数据源：state.historyBuf —— 回放事件 + live 事件（stream_event 除外，
 * 见 app.js connectEvents）。投影是增量的：traj.built 记录已处理前缀长度。
 *
 * 记录类型：system / user / assistant / tool / result / status / peer。
 * 角色筛选在 user / assistant / tool 之外单列 a2a：入站 peer 消息与出站
 * SendMessage 工具调用都归入 A2A 角色（筛选按钮位于助手和工具之间），行内
 * 用紫色系高亮，方便在主对话之外单独看会话间通信。
 * turn 边界由 kind=user 事件划定（入站 peer 消息也算新一轮）；首个 user
 * 之前的事件归入「系统初始化」。
 * assistant 记录按 message.id 去重更新（partial_messages 下同一消息会推
 * 多个快照事件）；tool 记录按 tool_use id 与 tool_result 配对。
 */
'use strict';

const traj = {
  built: 0,            // 已投影到 historyBuf 的前缀长度
  turns: [],           // [{n, userText, records, startTs, el, bodyEl, metaEl, dirty}]
  turnCount: 0,        // 用户轮次计数（turn 0 固定为系统初始化段）
  byMsgId: {},         // assistant message.id -> record
  byCallId: {},        // tool_use id -> tool record
  rc: 0,               // record id 计数器
  query: '',
  collapsed: new Set(),   // 手动折叠的 turn.n
  expandAll: true,        // 全局折叠开关（false = 全部折叠）
  kindFilter: new Set(['user', 'assistant', 'a2a', 'tool']),  // 启用显示的角色
  selectedId: null,       // 当前在右侧详情面板选中的 record id
  detailTab: 'summary',   // 当前详情面板的 tab
};

// ---------- 投影 ----------

function trajNormUsage(u) {
  if (!u || typeof u !== 'object') return null;
  return {
    input: u.input_tokens || 0,
    output: u.output_tokens || 0,
    cacheRead: u.cache_read_input_tokens || 0,
    cacheWrite: u.cache_creation_input_tokens || 0,
  };
}

function trajFirstLine(text, max = 100) {
  const line = String(text || '').split('\n').map((s) => s.trim()).find(Boolean) || '';
  return line.length > max ? line.slice(0, max) + '…' : line;
}

// 出站 A2A 的工具名判定。Claude 原生走 SendMessage；其余引擎走 agentdeck MCP
// 的 send_message，而各引擎对 MCP 工具的拼法不同（实测）：
//   claude / mcode: mcp__agentdeck__send_message
//   opencode:       agentdeck.send_message
//   codex:          agentdeck.send_message / agentdeck__send_message
// 统一判据 = 名字里同时含 agentdeck 与 send_message（避免误伤无关工具）。
// 判定放在这里，app.js 的对话流渲染也复用同一个函数。
function isA2ASendTool(name) {
  const n = String(name || '').toLowerCase();
  if (!n) return false;
  if (n === 'sendmessage') return true; // claude 原生
  if (n === 'send_message') return true; // 裸名（codex 的 mcp_tool_call 可能只给 tool）
  return n.includes('agentdeck') && n.includes('send_message');
}

// 出站 A2A 的调用信息（{to, message, native}），认不出返回 null。两种来源：
//   1) 原生 MCP 工具 —— claude/mcode 记成 mcp__agentdeck__send_message，input
//      里直接是 to/message；
//   2) opencode 走 Code Mode —— 所有工具都记成 execute/bash，agentdeck 的调用
//      埋在 input.code 里（tools.agentdeck.send_message({to,message})）。这里
//      从代码里认出它，尽量抽出 to/message；抽不到就退回展示整段代码。
// 轨迹视图与对话流渲染共用这一个判定，两处对「是不是 A2A 发」永远一致。
function a2aSendInfo(toolName, input) {
  if (isA2ASendTool(toolName)) {
    return { to: String((input && input.to) || ''), message: String((input && input.message) || ''), native: true };
  }
  const code = input && (input.code || input.command);
  if (typeof code !== 'string' || !/agentdeck/i.test(code) || !/send_message/.test(code)) return null;
  const to = (code.match(/to\s*:\s*["'`]([^"'`]+)["'`]/) || [])[1] || '';
  const msg = (code.match(/message\s*:\s*["'`]([\s\S]*?)["'`]\s*[,}\n)]/) || [])[1] || '';
  return { to, message: msg || code, native: false, code };
}

// A2A（会话间消息）方向：in = 收到其他会话的 peer 消息，out = 调用发送工具
// 发给其他会话。两种记录在台账里归到同一个 A2A 角色，但高亮样式区分方向。
function trajA2ADir(r) {
  if (r.kind === 'peer') return 'in';
  if (r.kind === 'tool' && a2aSendInfo(r.toolName, r.toolInput)) return 'out';
  return '';
}

function trajRecordRole(r) {
  return trajA2ADir(r) ? 'a2a' : r.kind;
}

function trajCurrentTurn() {
  if (!traj.turns.length) trajNewTurn(null, 0);
  return traj.turns[traj.turns.length - 1];
}

// userText 为 null 时是系统初始化段（n=0），否则开启新一轮（n 从 1 递增）。
function trajNewTurn(userText, ts, opts) {
  const n = userText == null ? 0 : ++traj.turnCount;
  const turn = { n, userText: userText || '', records: [], startTs: ts || 0, el: null, bodyEl: null, metaEl: null, dirty: true };
  if (opts && opts.peer) turn.peer = opts.peer; // { name, body } when this turn was triggered by an inbound peer message
  traj.turns.push(turn);
  return turn;
}

function trajAddRecord(turn, rec) {
  rec.id = 'r' + ++traj.rc;
  turn.records.push(rec);
  turn.dirty = true;
  return rec;
}

function trajTouchRecord(rec) {
  const turn = traj.turns.find((t) => t.records.includes(rec));
  if (turn) turn.dirty = true;
}

function trajProject(ev) {
  const ts = Date.parse(ev.ts) || 0;
  if (ev.kind === 'user') {
    const d = ev.data || {};
    // A2A 入站（总线投递的跨会话消息）：作为 peer 记录归入 A2A 角色，与 claude
    // 原生 peer 消息（走 stream 分支）同一套呈现。source 是服务端落库时打的。
    if (d.source === 'a2a') {
      const name = d.from || 'peer';
      const body = d.body || d.text || '';
      const turn = trajNewTurn(`⇢ 来自 ${name}：${body.split('\n')[0].slice(0, 60)}`, ts, { peer: { name, body } });
      trajAddRecord(turn, { kind: 'peer', ts, summary: `来自 ${name}`, body, name });
      return;
    }
    const text = d.text || '';
    const turn = trajNewTurn(text, ts);
    trajAddRecord(turn, { kind: 'user', ts, summary: trajFirstLine(text), text });
    return;
  }
  if (ev.kind === 'status') {
    const d = ev.data || {};
    if (d.state === 'interrupted') {
      trajAddRecord(trajCurrentTurn(), { kind: 'status', ts, summary: '已中断', isError: true });
    } else if (d.state === 'error') {
      trajAddRecord(trajCurrentTurn(), { kind: 'status', ts, summary: d.error || '错误', text: d.error || '', isError: true });
    }
    return; // turn_start/turn_end 不产生记录
  }
  if (ev.kind !== 'stream') return;
  const d = ev.data || {};
  if (d.parent_tool_use_id) return; // 子代理消息：聊天侧卡片渲染，轨迹不重复记
  switch (d.type) {
    case 'system': {
      if (d.subtype !== 'init') return;
      const tools = Array.isArray(d.tools) ? d.tools.join(', ') : '';
      trajAddRecord(trajCurrentTurn(), {
        kind: 'system', ts,
        summary: `model ${d.model || '?'} · ${d.cwd || ''}`,
        detail: { session_id: d.session_id, model: d.model, cwd: d.cwd, tools, slash_commands: (d.slash_commands || []).join(', ') },
      });
      return;
    }
    case 'user': {
      // 入站 peer 消息：起一个新轮次，title 显示来源 name，正文按 md 渲染。
      // 主对话的入站气泡是 msg-peer，这里在轨迹里走同样的语义但用台账视图。
      const origin = d.origin || {};
      if (origin.kind === 'peer') {
        const name = origin.name || 'peer';
        const body = origin.body || '';
        const turn = trajNewTurn(`⇢ 来自 ${name}：${body.split('\n')[0].slice(0, 60)}`, ts, { peer: { name, body } });
        trajAddRecord(turn, { kind: 'peer', ts, summary: `来自 ${name}`, body, name });
        return;
      }
      // 普通 user stream（tool_result 块）：配对到既有 tool 记录。
      const content = (d.message && d.message.content) || [];
      for (const b of content) {
        if (!b || b.type !== 'tool_result') continue;
        const t = traj.byCallId[b.tool_use_id];
        const txt = toolResultText(b.content);
        if (t) {
          t.toolResult = txt;
          t.isError = !!b.is_error;
          t.durMs = ts && t.ts ? Math.max(0, ts - t.ts) : null;
          trajTouchRecord(t);
        } else {
          trajAddRecord(trajCurrentTurn(), {
            kind: 'tool', ts, toolName: '(未知工具)', toolResult: txt,
            isError: !!b.is_error, summary: '(工具结果)',
          });
        }
      }
      return;
    }
    case 'assistant': {
      const m = d.message || {};
      const blocks = Array.isArray(m.content) ? m.content : [];
      // 纯 usage 携带事件（mcode/codex/opencodev2 每步一条 content 为空的
      // assistant 事件）不是消息，别在台账里留一条 "(空)"。
      if (blocks.length === 0) return;
      const mid = m.id || 'seq' + ev.seq;
      const text = blocks.filter((b) => b.type === 'text').map((b) => b.text || '').join('\n');
      const thinking = blocks.filter((b) => b.type === 'thinking').map((b) => b.thinking || '').join('\n');
      let rec = traj.byMsgId[mid];
      if (!rec) {
        rec = trajAddRecord(trajCurrentTurn(), { kind: 'assistant', ts, msgId: mid, toolIds: new Set() });
        traj.byMsgId[mid] = rec;
      }
      rec.text = text;
      rec.thinking = thinking;
      if (m.usage) rec.usage = trajNormUsage(m.usage);
      if (m.model) rec.model = m.model;
      // 先累计 tool_use，再算摘要（拆分行共享 message.id，块分多行到达）
      for (const b of blocks) {
        if (b.type !== 'tool_use') continue;
        rec.toolIds.add(b.id);
        let t = traj.byCallId[b.id];
        if (!t) {
          const toolName = b.name || 'tool';
          const a2a = a2aSendInfo(toolName, b.input);
          const tsum = toolSummary(b.input);
          // A2A 发送的摘要本身就是「⇢ 收件人 · 预览」，不再重复工具名。opencode
          // 的 Code Mode 里 toolName 是 execute，只有抽到收件人时才用 A2A 摘要，
          // 否则退回原来的「execute …」。
          let summary;
          if (a2a && a2a.to) summary = `⇢ ${a2a.to}${tsum ? '  ' + tsum : ''}`;
          else if (a2a) summary = tsum || toolName;
          else summary = toolName + (tsum ? '  ' + tsum : '');
          t = trajAddRecord(trajCurrentTurn(), {
            kind: 'tool', ts, callId: b.id, toolName, toolInput: b.input,
            summary,
          });
          traj.byCallId[b.id] = t;
        } else {
          t.toolInput = b.input;
          trajTouchRecord(t);
        }
      }
      const toolCount = rec.toolIds.size;
      rec.summary = trajFirstLine(text.replace(/[#*`>\[\]]/g, ''))
        || (thinking ? '(thinking)' : toolCount ? `(调用 ${toolCount} 个工具)` : '(空)');
      trajTouchRecord(rec);
      return;
    }
    case 'result': {
      trajAddRecord(trajCurrentTurn(), {
        kind: 'result', ts,
        summary: d.subtype === 'success' ? '完成' : (d.subtype || 'result'),
        durMs: d.duration_ms || 0, apiMs: d.duration_api_ms || 0,
        cost: d.total_cost_usd || 0, usage: trajNormUsage(d.usage),
        numTurns: d.num_turns || 0, isError: d.is_error === true,
      });
      return;
    }
    // stream_event 等：跳过（完整 assistant 事件兜底）
  }
}

// 把 historyBuf 中未投影的事件处理掉；返回是否有新增。
function trajSync() {
  const buf = state.historyBuf;
  if (traj.built >= buf.length) return false;
  for (let i = traj.built; i < buf.length; i++) trajProject(buf[i]);
  traj.built = buf.length;
  // 估算 assistant 记录耗时 = 到下一条记录的时间差（封顶 10 分钟）
  for (const turn of traj.turns) {
    const rs = turn.records;
    for (let i = 0; i < rs.length; i++) {
      const r = rs[i];
      if (r.kind === 'assistant' && r.durMs == null && i + 1 < rs.length && rs[i + 1].ts && r.ts) {
        r.durMs = Math.min(Math.max(0, rs[i + 1].ts - r.ts), 600000);
      }
    }
  }
  return true;
}

function trajReset() {
  traj.built = 0;
  traj.turns = [];
  traj.turnCount = 0;
  traj.byMsgId = {};
  traj.byCallId = {};
  traj.rc = 0;
  traj.query = '';
  traj.collapsed = new Set();
  traj.expandAll = true;
  traj.kindFilter = new Set(['user', 'assistant', 'a2a', 'tool']);
  traj.selectedId = null;
  traj.detailTab = 'summary';
  const list = $('#traj-list');
  if (list) list.innerHTML = '';
  const inp = $('#traj-search-input');
  if (inp) inp.value = '';
  // 筛选按钮的 active 态跟随重置后的 kindFilter（切会话时不会留下上个会话的筛选）
  document.querySelectorAll('.traj-filter-btn').forEach((btn) => {
    btn.classList.toggle('active', traj.kindFilter.has(btn.dataset.kind));
  });
  trajCloseDetail();
  trajUpdateStats();
}

// ---------- 渲染 ----------

const TRAJ_KIND_LABEL = { system: '系统', user: '用户', assistant: '助手', tool: '工具', result: '结果', status: '状态', peer: 'A2A' };

// A2A 记录按方向显示：收到其他会话消息 / 发出 SendMessage。其余走 kind 标签。
function trajKindLabel(r) {
  const dir = trajA2ADir(r);
  if (dir === 'in') return 'A2A 收';
  if (dir === 'out') return 'A2A 发';
  return TRAJ_KIND_LABEL[r.kind] || r.kind;
}

function trajFmtTok(n) {
  if (!n) return '0';
  if (n >= 1000) return (n / 1000).toFixed(1).replace(/\.0$/, '') + 'k';
  return String(n);
}

function trajUsageText(u) {
  if (!u) return '';
  const parts = [`in ${trajFmtTok(u.input + u.cacheRead + u.cacheWrite)}`, `out ${trajFmtTok(u.output)}`];
  if (u.cacheRead) parts.push(`cached ${trajFmtTok(u.cacheRead)}`);
  return parts.join(' · ');
}

// 轮次头部汇总：条数 / token / 花费 / 耗时（优先取 result 事件的精确值）
function trajTurnMeta(turn) {
  let recs = turn.records.length;
  let cost = 0, durMs = 0;
  const acc = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 };
  let hasUsage = false;
  for (const r of turn.records) {
    if (r.kind === 'result') {
      cost += r.cost || 0;
      durMs += r.durMs || 0;
      if (r.usage) {
        hasUsage = true;
        acc.input += r.usage.input; acc.output += r.usage.output;
        acc.cacheRead += r.usage.cacheRead; acc.cacheWrite += r.usage.cacheWrite;
      }
    }
  }
  if (!hasUsage) {
    for (const r of turn.records) {
      if (r.usage) {
        hasUsage = true;
        acc.input += r.usage.input; acc.output += r.usage.output;
        acc.cacheRead += r.usage.cacheRead; acc.cacheWrite += r.usage.cacheWrite;
      }
    }
  }
  const parts = [`${recs} 条`];
  if (hasUsage) parts.push(`in ${trajFmtTok(acc.input + acc.cacheRead + acc.cacheWrite)} · out ${trajFmtTok(acc.output)}`);
  if (cost) parts.push(`$${cost.toFixed(4)}`);
  if (durMs) parts.push(fmtDuration(durMs));
  return parts.join(' · ');
}

function trajTurnTitle(turn) {
  if (turn.n === 0) return '系统初始化';
  if (turn.peer) return `轮次 ${turn.n} · ⇢ ${turn.peer.name || 'peer'}`;
  return `轮次 ${turn.n}`;
}

function trajIsCollapsed(turn) {
  return traj.expandAll ? traj.collapsed.has(turn.n) : !traj.collapsed.has(turn.n);
}

function trajRecordSearchText(r) {
  if (r._search == null) {
    r._search = [
      r.summary, r.text, r.thinking, r.toolName, r.body,
      r.toolInput ? JSON.stringify(r.toolInput) : '', r.toolResult,
    ].filter(Boolean).join('\n').toLowerCase();
  }
  return r._search;
}

function trajBuildRecordEl(r) {
  const el = document.createElement('div');
  const a2aDir = trajA2ADir(r);
  el.className = `traj-rec traj-kind-${r.kind}${a2aDir ? ` traj-a2a traj-a2a-${a2aDir}` : ''}${r.isError ? ' traj-error' : ''}`;
  el.dataset.rid = r.id;
  el.dataset.kind = r.kind;

  const head = document.createElement('div');
  head.className = 'traj-rec-head';
  const meta = [];
  if (r.durMs != null && r.durMs > 0) meta.push(fmtDuration(r.durMs));
  if (r.usage) meta.push(trajUsageText(r.usage));
  if (r.cost) meta.push(`$${r.cost.toFixed(4)}`);
  if (r.ts) meta.push(formatMsgTime(new Date(r.ts).toISOString()));
  head.innerHTML =
    `<span class="traj-dot"></span>` +
    `<span class="traj-kind"></span>` +
    `<span class="traj-summary"></span>` +
    `<span class="traj-rec-meta">${meta.join(' · ')}</span>`;
  head.querySelector('.traj-kind').textContent = trajKindLabel(r);
  head.querySelector('.traj-summary').textContent = r.summary || '';
  head.title = r.summary || '';
  head.onclick = () => trajSelectRecord(r);
  el.appendChild(head);

  if (traj.selectedId === r.id) el.classList.add('selected');
  return el;
}

function trajBuildDetail(r) {
  // 已弃用：详情改在右侧面板渲染。保留以防外部调用。
  return document.createElement('div');
}

function trajToggleDetail() { /* no-op：已改用右侧面板 */ }

// 查找 record 所在 turn 与其在 turn 内的步骤序号（1-based）
function trajLocateRecord(r) {
  for (const turn of traj.turns) {
    const idx = turn.records.indexOf(r);
    if (idx >= 0) return { turn, step: idx + 1 };
  }
  return { turn: null, step: 0 };
}

function trajSelectRecord(r) {
  traj.selectedId = r.id;
  // 刷新选中态
  const list = $('#traj-list');
  if (list) {
    list.querySelectorAll('.traj-rec.selected').forEach((el) => el.classList.remove('selected'));
    const el = list.querySelector(`[data-rid="${r.id}"]`);
    if (el) el.classList.add('selected');
  }
  trajOpenDetail(r);
}

function trajOpenDetail(r) {
  const panel = $('#traj-detail');
  if (!panel) return;
  panel.classList.remove('hidden');
  $('#traj-main').classList.add('has-detail');
  trajRenderDetailHeader(r);
  trajRenderDetailBody(r);
}

function trajCloseDetail() {
  const panel = $('#traj-detail');
  if (!panel) return;
  panel.classList.add('hidden');
  $('#traj-main').classList.remove('has-detail');
  traj.selectedId = null;
  const list = $('#traj-list');
  if (list) list.querySelectorAll('.traj-rec.selected').forEach((el) => el.classList.remove('selected'));
}

function trajRenderDetailHeader(r) {
  const hdr = $('#traj-detail-header');
  if (!hdr) return;
  const { turn, step } = trajLocateRecord(r);
  const turnLabel = turn ? (turn.n === 0 ? '系统初始化' : `第 ${turn.n} 轮`) : '';
  const stepLabel = step ? `第 ${step} 步` : '';
  const role = trajRecordRole(r);
  const roleLabel = trajKindLabel(r);
  const metaLines = [];
  if (r.durMs != null && r.durMs > 0) metaLines.push(`耗时 ${fmtDuration(r.durMs)}`);
  if (r.usage) metaLines.push(trajUsageText(r.usage));
  if (r.cost) metaLines.push(`$${r.cost.toFixed(4)}`);
  if (r.model) metaLines.push(`model ${r.model}`);
  if (r.ts) metaLines.push(formatMsgTime(new Date(r.ts).toISOString()));
  if (r.toolName) metaLines.push(`工具 ${r.toolName}`);
  hdr.innerHTML =
    `<div class="traj-dh-title">${turnLabel} · ${stepLabel}</div>` +
    `<div class="traj-dh-role r-${role}"><span class="traj-dot"></span>${roleLabel}</div>` +
    (metaLines.length ? `<div class="traj-dh-meta">${metaLines.map((t) => `<div>${t}</div>`).join('')}</div>` : '');
}

function trajRenderDetailBody(r) {
  const body = $('#traj-detail-body');
  if (!body) return;
  body.innerHTML = '';
  // 高亮 tab
  $('#traj-detail-tabs').querySelectorAll('.traj-tab').forEach((b) => {
    b.classList.toggle('active', b.dataset.tab === traj.detailTab);
  });
  if (traj.detailTab === 'summary') body.appendChild(trajDetailSummary(r));
  else if (traj.detailTab === 'preview') body.appendChild(trajDetailPreview(r));
  else body.appendChild(trajDetailRaw(r));
}

function trajAddSec(det, label, text, cls) {
  if (!text) return;
  const sec = document.createElement('div');
  sec.className = 'traj-sec' + (cls ? ' ' + cls : '');
  sec.innerHTML = `<div class="traj-sec-label">${label}</div>`;
  const pre = document.createElement('pre');
  pre.textContent = text;
  sec.appendChild(pre);
  det.appendChild(sec);
}

function trajDetailSummary(r) {
  const det = document.createElement('div');
  trajAddSec(det, '摘要', r.summary);
  if (r.kind === 'assistant') {
    if (r.thinking) trajAddSec(det, 'thinking', r.thinking, 'traj-thinking');
    if (r.text) {
      const sec = document.createElement('div');
      sec.className = 'traj-sec';
      sec.innerHTML = '<div class="traj-sec-label">回复</div>';
      const mdEl = document.createElement('div');
      mdEl.className = 'traj-md';
      mdEl.innerHTML = md(r.text, { artifacts: false });
      sec.appendChild(mdEl);
      det.appendChild(sec);
    }
  } else if (r.kind === 'tool') {
    // 出站 A2A：正文按 md 展示（与对话流的发送卡片同一语义），投递回执在下面
    if (trajA2ADir(r) === 'out' && r.toolInput && typeof r.toolInput.message === 'string') {
      const to = String(r.toolInput.to || '');
      const sec = document.createElement('div');
      sec.className = 'traj-sec traj-peer';
      const label = document.createElement('div');
      label.className = 'traj-sec-label';
      label.textContent = to ? `发送给 ${to}` : '发送';
      sec.appendChild(label);
      const mdEl = document.createElement('div');
      mdEl.className = 'traj-md';
      mdEl.innerHTML = md(r.toolInput.message, { artifacts: false });
      sec.appendChild(mdEl);
      det.appendChild(sec);
    }
    if (r.toolResult) trajAddSec(det, r.isError ? '结果（错误）' : '结果', r.toolResult, r.isError ? 'traj-err' : '');
  } else if (r.kind === 'user') {
    trajAddSec(det, '内容', r.text);
  } else if (r.kind === 'peer') {
    const sec = document.createElement('div');
    sec.className = 'traj-sec traj-peer';
    const label = document.createElement('div');
    label.className = 'traj-sec-label';
    label.textContent = '来自 ' + (r.name || 'peer'); // textContent 自动转义
    sec.appendChild(label);
    const mdEl = document.createElement('div');
    mdEl.className = 'traj-md';
    mdEl.innerHTML = md(r.body || '', { artifacts: false }); // body 走 marked + DOMPurify 已在 md() 里处理
    sec.appendChild(mdEl);
    det.appendChild(sec);
  } else if (r.kind === 'system' && r.detail) {
    for (const [k, v] of Object.entries(r.detail)) if (v) trajAddSec(det, k, String(v));
  } else if (r.kind === 'result') {
    const facts = [];
    if (r.durMs) facts.push(`总耗时 ${fmtDuration(r.durMs)}`);
    if (r.apiMs) facts.push(`API ${fmtDuration(r.apiMs)}`);
    if (r.cost) facts.push(`$${r.cost.toFixed(4)}`);
    if (r.usage) facts.push(trajUsageText(r.usage));
    if (r.numTurns) facts.push(`${r.numTurns} 个子步`);
    trajAddSec(det, '详情', facts.join('\n'));
  } else if (r.text) {
    trajAddSec(det, '详情', r.text, r.isError ? 'traj-err' : '');
  }
  if (!det.childElementCount) det.innerHTML = '<div class="traj-sec"><pre>(无更多内容)</pre></div>';
  return det;
}

function trajDetailPreview(r) {
  const det = document.createElement('div');
  if (r.kind === 'assistant') {
    if (r.text) {
      const sec = document.createElement('div');
      sec.className = 'traj-sec';
      sec.innerHTML = '<div class="traj-sec-label">回复</div>';
      const mdEl = document.createElement('div');
      mdEl.className = 'traj-md';
      mdEl.innerHTML = md(r.text, { artifacts: false });
      sec.appendChild(mdEl);
      det.appendChild(sec);
    } else {
      trajAddSec(det, '回复', '(空)');
    }
  } else if (r.kind === 'tool') {
    if (r.toolInput != null) trajAddSec(det, '输入', JSON.stringify(r.toolInput, null, 2));
    if (r.toolResult) trajAddSec(det, r.isError ? '结果（错误）' : '结果', r.toolResult, r.isError ? 'traj-err' : '');
  } else if (r.kind === 'user') {
    const sec = document.createElement('div');
    sec.className = 'traj-sec';
    sec.innerHTML = '<div class="traj-sec-label">内容</div>';
    const mdEl = document.createElement('div');
    mdEl.className = 'traj-md';
    mdEl.innerHTML = md(r.text || '', { artifacts: false });
    sec.appendChild(mdEl);
    det.appendChild(sec);
  } else {
    trajAddSec(det, '内容', r.text || r.summary || '(无)');
  }
  if (!det.childElementCount) det.innerHTML = '<div class="traj-sec"><pre>(无更多内容)</pre></div>';
  return det;
}

function trajDetailRaw(r) {
  const det = document.createElement('div');
  // 导出可读的 raw 视图：剥离内部字段
  const raw = {};
  for (const k of Object.keys(r)) {
    if (k.startsWith('_') || k === 'el') continue;
    const v = r[k];
    raw[k] = v instanceof Set ? Array.from(v) : v;
  }
  trajAddSec(det, 'raw', JSON.stringify(raw, null, 2));
  return det;
}

function trajBuildTurnEl(turn) {
  const root = document.createElement('div');
  root.className = 'traj-turn';
  root.dataset.turn = String(turn.n);

  const header = document.createElement('div');
  header.className = 'traj-turn-header' + (turn.peer ? ' peer' : '');
  const chev = trajIsCollapsed(turn) ? '▸' : '▾';
  header.innerHTML =
    `<span class="traj-chev">${chev}</span>` +
    `<span class="traj-turn-title">${trajTurnTitle(turn)}</span>` +
    `<span class="traj-turn-summary"></span>` +
    `<span class="traj-turn-meta"></span>`;
  header.querySelector('.traj-turn-summary').textContent = turn.userText ? trajFirstLine(turn.userText, 60) : '';
  turn.metaEl = header.querySelector('.traj-turn-meta');
  turn.metaEl.textContent = trajTurnMeta(turn);
  header.onclick = () => trajToggleTurn(turn);

  const body = document.createElement('div');
  body.className = 'traj-turn-body';
  if (trajIsCollapsed(turn)) body.classList.add('hidden');

  root.append(header, body);
  turn.el = root;
  turn.bodyEl = body;
  turn.dirty = true;
  return root;
}

function trajRenderTurnBody(turn) {
  const body = turn.bodyEl;
  body.innerHTML = '';
  const frag = document.createDocumentFragment();
  for (const r of turn.records) {
    const el = trajBuildRecordEl(r);
    frag.appendChild(el);
  }
  body.appendChild(frag);
  turn.metaEl.textContent = trajTurnMeta(turn);
  turn.dirty = false;
  trajApplyFilter(turn);
}

// 全量/增量渲染：先统一编号（跨 turn 连续，对齐 dsh 的 #N），
// 然后新 turn 建 DOM，脏 turn 重建 body。
function trajRender() {
  const list = $('#traj-list');
  if (!list) return;
  let gidx = 0;
  for (const t of traj.turns) for (const r of t.records) r.gidx = ++gidx;
  let dirtySelected = false;
  for (const turn of traj.turns) {
    if (!turn.el) list.appendChild(trajBuildTurnEl(turn));
    if (turn.dirty) {
      if (traj.selectedId && turn.records.some((r) => r.id === traj.selectedId)) dirtySelected = true;
      trajRenderTurnBody(turn);
    }
  }
  trajUpdateStats();
  if (dirtySelected) {
    const r = trajFindRecordById(traj.selectedId);
    if (r) { trajRenderDetailHeader(r); trajRenderDetailBody(r); }
  }
}

function trajToggleTurn(turn) {
  if (traj.collapsed.has(turn.n)) traj.collapsed.delete(turn.n);
  else traj.collapsed.add(turn.n);
  const collapsed = trajIsCollapsed(turn);
  turn.bodyEl.classList.toggle('hidden', collapsed);
  turn.el.querySelector('.traj-chev').textContent = collapsed ? '▸' : '▾';
}

function trajUpdateStats() {
  const el = $('#traj-stats');
  if (!el) return;
  let recs = 0;
  for (const t of traj.turns) recs += t.records.length;
  let text = `${recs} 条记录 · ${traj.turnCount} 轮`;
  if (traj.query) {
    let hit = 0;
    for (const t of traj.turns) for (const r of t.records) {
      if (trajRecordSearchText(r).includes(traj.query)) hit++;
    }
    text += ` · 命中 ${hit}`;
  }
  el.textContent = text;
}

// 搜索：未命中行调暗（对齐 dsh 的 dim 而非隐藏）；角色筛选则隐藏整行
function trajApplyFilter(scope) {
  const q = traj.query;
  const turns = scope ? [scope] : traj.turns;
  for (const turn of turns) {
    if (!turn.el) continue;
    let anyHit = false;
    let anyVisible = false;
    for (const r of turn.records) {
      const kindHidden = !traj.kindFilter.has(trajRecordRole(r));
      const el = turn.bodyEl.querySelector(`[data-rid="${r.id}"]`);
      if (!el) continue;
      el.classList.toggle('kind-hidden', kindHidden);
      const hit = !q || trajRecordSearchText(r).includes(q);
      if (hit && !kindHidden) anyHit = true;
      if (!kindHidden) anyVisible = true;
      el.classList.toggle('traj-dim', !hit);
    }
    turn.el.classList.toggle('traj-dim', !anyHit);
    turn.el.classList.toggle('kind-hidden', !anyVisible);
  }
}

function trajSetQuery(q) {
  traj.query = q.trim().toLowerCase();
  trajApplyFilter();
  trajUpdateStats();
}

// ---------- 入口 ----------

function trajActive() {
  const v = $('#traj-view');
  return v && !v.classList.contains('hidden');
}

// 新事件到达（live）：投影；若轨迹页可见则渲染。
function trajOnEvent() {
  if (!trajSync()) return;
  if (trajActive()) trajRender();
}

// 切到轨迹 tab 时调用：补齐投影并全量渲染。
function trajShow() {
  trajSync();
  trajRender();
}

// 工具栏事件（加载时执行：不能用 app.js 的 $，它随 app.js 后加载才定义）
document.querySelector('#traj-search-input').addEventListener('input', (e) => trajSetQuery(e.target.value));
document.querySelector('#traj-toggle-turns').onclick = () => {
  traj.expandAll = !traj.expandAll;
  traj.collapsed.clear();
  for (const turn of traj.turns) {
    if (!turn.el) continue;
    const collapsed = trajIsCollapsed(turn);
    turn.bodyEl.classList.toggle('hidden', collapsed);
    turn.el.querySelector('.traj-chev').textContent = collapsed ? '▸' : '▾';
  }
  document.querySelector('#traj-toggle-turns').classList.toggle('active', !traj.expandAll);
};

// 角色筛选按钮
document.querySelectorAll('.traj-filter-btn').forEach((btn) => {
  btn.addEventListener('click', () => {
    const kind = btn.dataset.kind;
    if (traj.kindFilter.has(kind)) traj.kindFilter.delete(kind);
    else traj.kindFilter.add(kind);
    btn.classList.toggle('active', traj.kindFilter.has(kind));
    trajApplyFilter();
    trajUpdateStats();
  });
});

// 详情面板 tab 切换
document.querySelectorAll('#traj-detail-tabs .traj-tab').forEach((btn) => {
  btn.addEventListener('click', () => {
    traj.detailTab = btn.dataset.tab;
    if (traj.selectedId) {
      const r = trajFindRecordById(traj.selectedId);
      if (r) trajRenderDetailBody(r);
    }
  });
});

function trajFindRecordById(rid) {
  for (const turn of traj.turns) {
    for (const r of turn.records) {
      if (r.id === rid) return r;
    }
  }
  return null;
}
