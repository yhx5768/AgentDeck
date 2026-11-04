// 轨迹视图 A2A 角色筛选回归测试：
//   - 筛选按钮顺序：用户 / 助手 / A2A / 工具；
//   - 入站 peer 消息与出站 SendMessage 工具调用都归入 A2A 角色；
//   - 两种记录默认可见，切换 A2A 筛选只影响它们，不影响普通工具；
//   - 行内高亮方向（A2A 收 / A2A 发）、搜索覆盖 peer 正文、重置同步按钮状态。
//
// 局限：jsdom 不做布局，紫色高亮的实际观感仍需人工过一眼；这里钉的是
// 投影分类、筛选可见性和 DOM 契约。
const fs = require('fs');
const path = require('path');
const { JSDOM } = require('jsdom');

const WEB = path.resolve(__dirname, '../../web');
const trajSrc = fs.readFileSync(path.join(WEB, 'trajectory.js'), 'utf8');
const appSrc = fs.readFileSync(path.join(WEB, 'app.js'), 'utf8');
const indexHtml = fs.readFileSync(path.join(WEB, 'index.html'), 'utf8');

function grabFn(name) {
  const s = appSrc.indexOf('function ' + name + '(');
  if (s < 0) throw new Error('找不到函数 ' + name);
  const b = appSrc.indexOf('{', s);
  let d = 0, q = null;
  for (let i = b; i < appSrc.length; i++) {
    const c = appSrc[i], p = appSrc[i - 1];
    if (q) { if (c === q && p !== '\\') q = null; continue; }
    if (c === '"' || c === "'" || c === '`') { q = c; continue; }
    if (c === '{') d++;
    else if (c === '}') { d--; if (!d) return appSrc.slice(s, i + 1); }
  }
  throw new Error(name + ' 括号不配对');
}

const HTML = `<!doctype html><html><body>
  <div id="traj-view">
    <button id="traj-toggle-turns"></button>
    <div class="traj-filter">
      <button class="traj-filter-btn active" data-kind="user">用户</button>
      <button class="traj-filter-btn active" data-kind="assistant">助手</button>
      <button class="traj-filter-btn active" data-kind="a2a">A2A</button>
      <button class="traj-filter-btn active" data-kind="tool">工具</button>
    </div>
    <input id="traj-search-input">
    <span id="traj-stats"></span>
    <div id="traj-main">
      <div id="traj-list-wrap"><div id="traj-list"></div></div>
      <aside id="traj-detail" class="hidden">
        <div id="traj-detail-header"></div>
        <div id="traj-detail-tabs">
          <button class="traj-tab active" data-tab="summary">summary</button>
          <button class="traj-tab" data-tab="preview">preview</button>
          <button class="traj-tab" data-tab="raw">raw</button>
        </div>
        <div id="traj-detail-body"></div>
      </aside>
    </div>
  </div>
</body></html>`;

const dom = new JSDOM(HTML, { runScripts: 'outside-only', url: 'http://localhost/' });
const w = dom.window;
const doc = w.document;

const setup = `
const $ = (sel) => document.querySelector(sel);
const state = { historyBuf: [] };
function fmtDuration(ms) { return ms + 'ms'; }
function formatMsgTime(iso) { return iso; }
function md(text) { return String(text || ''); }
function toolResultText(content) {
  if (typeof content === 'string') return content;
  if (Array.isArray(content)) return content.map((c) => (c && c.text) || '').filter(Boolean).join('\\n');
  return content ? JSON.stringify(content) : '';
}
${grabFn('toolSummary')}
${trajSrc}
window.__state = state;
window.__traj = traj;
window.__x = {
  trajA2ADir, trajRecordRole, trajKindLabel, trajRecordSearchText,
  trajSync, trajRender, trajReset, a2aSendInfo, isA2ASendTool,
};`;

w.eval(setup);

const X = w.__x;
const state = w.__state;

let failed = 0;
function check(name, cond, extra) {
  if (cond) console.log('  ok  ' + name);
  else { failed++; console.log('  FAIL ' + name + (extra ? '  →  ' + extra : '')); }
}

const EVENTS = [
  { kind: 'user', ts: '2026-11-03T10:00:00.000Z', data: { text: '整理一下' } },
  {
    kind: 'stream', ts: '2026-11-03T10:00:01.000Z',
    data: {
      type: 'assistant',
      message: {
        id: 'm1',
        content: [
          { type: 'text', text: '我来处理' },
          { type: 'tool_use', id: 't-send', name: 'SendMessage', input: { to: 'mainAgent', message: '结果已整理' } },
          { type: 'tool_use', id: 't-read', name: 'Read', input: { file_path: '/tmp/a.txt' } },
        ],
      },
    },
  },
  {
    kind: 'stream', ts: '2026-11-03T10:00:02.000Z',
    data: { type: 'user', message: { content: [{ type: 'tool_result', tool_use_id: 't-send', content: '已送达' }] } },
  },
  {
    kind: 'stream', ts: '2026-11-03T10:00:03.000Z',
    data: { type: 'user', origin: { kind: 'peer', name: 'mainAgent', body: '请处理这个任务' } },
  },
  // 引擎中立的 A2A 总线：消息落成一条 stored user 事件，带 source/from/body。
  {
    kind: 'user', ts: '2026-11-03T10:00:04.000Z',
    data: {
      text: 'Another agent session sent you a message.\n\n<cross-session-message from="worker-2">\n总线回报\n</cross-session-message>\n',
      source: 'a2a', from: 'worker-2', body: '总线回报',
    },
  },
  {
    kind: 'stream', ts: '2026-11-03T10:00:05.000Z',
    data: {
      type: 'assistant',
      message: {
        id: 'm2',
        content: [
          { type: 'tool_use', id: 't-mcp', name: 'mcp__agentdeck__send_message', input: { to: 'worker-2', message: '收到' } },
          // opencode 对 MCP 工具的拼法是点号（实测），也必须归 A2A 发。
          { type: 'tool_use', id: 't-oc', name: 'agentdeck.send_message', input: { to: 'worker-3', message: '收到' } },
        ],
      },
    },
  },
  // opencode 走 Code Mode：所有工具都记成 execute，agentdeck 调用埋在代码里。
  {
    kind: 'stream', ts: '2026-11-03T10:00:06.000Z',
    data: {
      type: 'assistant',
      message: {
        id: 'm3',
        content: [
          {
            type: 'tool_use', id: 't-exec-send', name: 'execute',
            input: { code: 'const r = await tools.agentdeck.send_message({ from: "oc", to: "mainAgent", message: "ack oc" });\nreturn JSON.stringify(r);' },
          },
          {
            // 非发送的 execute（list_sessions）不能被误判成 A2A 发。
            type: 'tool_use', id: 't-exec-list', name: 'execute',
            input: { code: 'const r = await tools.agentdeck.list_sessions();\nreturn JSON.stringify(r);' },
          },
        ],
      },
    },
  },
];

/** 从 index.html 里取轨迹筛选按钮的 data-kind 顺序。 */
function filterOrder() {
  const block = indexHtml.slice(indexHtml.indexOf('class="traj-filter"'));
  const end = block.indexOf('</div>');
  return [...block.slice(0, end).matchAll(/data-kind="([^"]+)"/g)].map((m) => m[1]);
}

function main() {
  console.log('轨迹 A2A 筛选');

  check('筛选按钮顺序为 用户/助手/A2A/工具',
    JSON.stringify(filterOrder()) === JSON.stringify(['user', 'assistant', 'a2a', 'tool']),
    JSON.stringify(filterOrder()));

  state.historyBuf = EVENTS;
  X.trajSync();
  X.trajRender();

  const records = w.__traj.turns.flatMap((t) => t.records);
  const send = records.find((r) => r.toolName === 'SendMessage');
  const read = records.find((r) => r.toolName === 'Read');
  const peer = records.find((r) => r.kind === 'peer');

  check('出站 SendMessage 归入 A2A/out',
    !!send && X.trajA2ADir(send) === 'out' && X.trajRecordRole(send) === 'a2a');
  check('入站 peer 消息归入 A2A/in',
    !!peer && X.trajA2ADir(peer) === 'in' && X.trajRecordRole(peer) === 'a2a');
  check('普通工具仍是工具角色',
    !!read && X.trajA2ADir(read) === '' && X.trajRecordRole(read) === 'tool');
  check('A2A 发送摘要不重复工具名',
    !!send && send.summary.startsWith('⇢ mainAgent') && !send.summary.startsWith('SendMessage'),
    send && send.summary);
  check('SendMessage 结果仍按 tool 记录配对', !!send && send.toolResult === '已送达');
  check('搜索覆盖入站 peer 正文',
    !!peer && X.trajRecordSearchText(peer).includes('请处理这个任务'));

  // 总线投递的 A2A 入站（stored user 事件 + source=a2a）与 claude 原生 peer 同归 A2A。
  const bus = records.find((r) => r.kind === 'peer' && r.name === 'worker-2');
  check('总线 A2A 入站归入 A2A/in',
    !!bus && X.trajA2ADir(bus) === 'in' && X.trajRecordRole(bus) === 'a2a');
  check('总线 A2A 入站不残留 user 记录',
    !records.some((r) => r.kind === 'user' && r.text === '总线回报'));
  check('总线 A2A 入站搜索覆盖正文',
    !!bus && X.trajRecordSearchText(bus).includes('总线回报'));

  // agentdeck MCP 的 send_message（mcp__agentdeck__send_message）也是出站 A2A。
  const mcpSend = records.find((r) => r.toolName === 'mcp__agentdeck__send_message');
  check('MCP send_message（claude/mcode 双下划线）归入 A2A/out',
    !!mcpSend && X.trajA2ADir(mcpSend) === 'out' && X.trajRecordRole(mcpSend) === 'a2a');
  const ocSend = records.find((r) => r.toolName === 'agentdeck.send_message');
  check('MCP send_message（opencode 点号）归入 A2A/out',
    !!ocSend && X.trajA2ADir(ocSend) === 'out' && X.trajRecordRole(ocSend) === 'a2a');

  // opencode Code Mode：execute 里包着的 send_message 也要归 A2A 发，且摘要带收件人。
  const execSend = records.find((r) => r.callId === 't-exec-send');
  check('execute 里包着的 send_message 归入 A2A/out',
    !!execSend && X.trajA2ADir(execSend) === 'out' && X.trajRecordRole(execSend) === 'a2a',
    execSend && execSend.summary);
  check('execute A2A 摘要带收件人',
    !!execSend && execSend.summary.startsWith('⇢ mainAgent'), execSend && execSend.summary);
  const execList = records.find((r) => r.callId === 't-exec-list');
  check('execute 里的 list_sessions 不算 A2A',
    !!execList && X.trajA2ADir(execList) === '' && X.trajRecordRole(execList) === 'tool');
  check('a2aSendInfo 从代码里抽出 to/message',
    (() => { const i = X.a2aSendInfo('execute', execSend.toolInput); return i && i.to === 'mainAgent' && i.message === 'ack oc'; })());

  const el = (r) => doc.querySelector(`[data-rid="${r.id}"]`);
  const sendEl = el(send), readEl = el(read), peerEl = el(peer);
  check('A2A 发送行带高亮方向 class',
    !!sendEl && sendEl.classList.contains('traj-a2a') && sendEl.classList.contains('traj-a2a-out'));
  check('A2A 收到行带高亮方向 class',
    !!peerEl && peerEl.classList.contains('traj-a2a') && peerEl.classList.contains('traj-a2a-in'));
  check('A2A 行首标签区分收发',
    sendEl.querySelector('.traj-kind').textContent === 'A2A 发' &&
    peerEl.querySelector('.traj-kind').textContent === 'A2A 收',
    sendEl.querySelector('.traj-kind').textContent + ' / ' + peerEl.querySelector('.traj-kind').textContent);
  check('普通工具行不带 A2A 高亮', !readEl.classList.contains('traj-a2a'));

  check('默认筛选下 A2A 收发可见',
    !sendEl.classList.contains('kind-hidden') && !peerEl.classList.contains('kind-hidden'));
  check('默认筛选下工具可见', !readEl.classList.contains('kind-hidden'));

  const btn = (kind) => doc.querySelector(`.traj-filter-btn[data-kind="${kind}"]`);
  btn('a2a').click();
  check('关掉 A2A：发送行隐藏', sendEl.classList.contains('kind-hidden'));
  check('关掉 A2A：收到行隐藏', peerEl.classList.contains('kind-hidden'));
  check('关掉 A2A：普通工具不受影响', !readEl.classList.contains('kind-hidden'));

  btn('a2a').click();
  btn('tool').click();
  check('关掉工具：A2A 发送仍可见', !sendEl.classList.contains('kind-hidden'));
  check('关掉工具：A2A 收到仍可见', !peerEl.classList.contains('kind-hidden'));
  check('关掉工具：普通工具隐藏', readEl.classList.contains('kind-hidden'));

  X.trajReset();
  check('重置后 A2A 筛选恢复启用', w.__traj.kindFilter.has('a2a'));
  check('重置后按钮 active 与筛选集合一致',
    [...doc.querySelectorAll('.traj-filter-btn')].every((b) => b.classList.contains('active') === w.__traj.kindFilter.has(b.dataset.kind)));

  console.log(failed ? `\n${failed} 项失败` : '\n全部通过');
  process.exit(failed ? 1 : 0);
}

main();
