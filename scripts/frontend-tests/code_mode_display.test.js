// opencode Code Mode（execute）的展示契约。
//
// 用户反馈：主 Agent 派工时，对话流里直接展开整段 `const taskCwd...` 的 JS，
// 又长又难读。这里钉住三件事：
//   1) codeModeCalls 能从 tools.agentdeck.<tool>({...}) 里抽出每一步；
//   2) a2aSendInfo 能认 const 变量 / shorthand（to: workerPeer, message），
//      不再把整段代码当消息正文；
//   3) 多步 execute 渲染成结构化步骤卡，原始 JS 只放在默认收起的 .cm-raw 里；
//      单条 board_comment 之类默认折叠，单条 send_message 仍走紫色发件卡。
//
// 局限：jsdom 不做布局，卡片实际观感仍需人工过一眼；这里钉 DOM 契约。
const fs = require('fs');
const path = require('path');
const { JSDOM } = require('jsdom');
const { grabFn } = require('./pa_harness');

const WEB = path.resolve(__dirname, '../../web');
const trajSrc = fs.readFileSync(path.join(WEB, 'trajectory.js'), 'utf8');

const HTML = `<!doctype html><html><body>
  <div id="messages"></div>
  <div id="traj-view">
    <button id="traj-toggle-turns"></button>
    <div class="traj-filter"></div>
    <input id="traj-search-input">
    <span id="traj-stats"></span>
    <div id="traj-main">
      <div id="traj-list-wrap"><div id="traj-list"></div></div>
      <aside id="traj-detail">
        <div id="traj-detail-header"></div>
        <div id="traj-detail-tabs"></div>
        <div id="traj-detail-body"></div>
      </aside>
    </div>
  </div>
</body></html>`;

const dom = new JSDOM(HTML, { runScripts: 'outside-only', pretendToBeVisual: true });
const w = dom.window;
const doc = w.document;

const setup = `
const state = { sessions: [], toolBlocks: {}, toolTimers: {} };
function md(text) {
  const d = document.createElement('div');
  d.textContent = String(text == null ? '' : text);
  return d.innerHTML;
}
function openSession(id) { window.__opened = id; }
function buildHtmlArtifactNode() { return document.createElement('div'); }
function mountHtmlFrame() {}
function buildTodoView() { return document.createElement('div'); }
function buildTaskToolView() { return document.createElement('div'); }
function buildBashView() { return document.createElement('div'); }
function buildReadView() { return document.createElement('div'); }
function buildWriteView() { return document.createElement('div'); }
function buildEditDiff() { return null; }
${trajSrc}
${['toolSummary', 'buildToolView', 'buildCodeModeView', 'buildCodeModeMessage', 'buildPeerSendView',
  'buildPeerChip', 'findPeerSession', 'buildRawCodeDetails', 'cmApplyResult'].map(grabFn).join('\n')}
window.__x = {
  toolSummary, buildToolView, buildCodeModeView, buildPeerSendView, cmApplyResult,
  codeModeCalls, codeModeSummary, codeModeStepInfo, a2aSendInfo, cmArgText,
};
`;
w.eval(setup);
const X = w.__x;

let failed = 0;
function check(name, cond, extra) {
  if (cond) console.log('  ok  ' + name);
  else { failed++; console.log('  FAIL ' + name + (extra ? '  →  ' + extra : '')); }
}

// 一段贴近主 Agent 实际派工的 Code Mode 代码：先查重，再建卡 / 建 worker /
// 绑定 / 评论 / 改状态，最后发派工单。变量 / shorthand 的写法也照实保留。
const DISPATCH = `const taskCwd = "/Users/yhx5768/Projects/cc-webui";
const workerName = "cc-webui-派工展示优化";
const title = "AgentDeck：主 Agent 派工/工具调用展示优化";

const [board, sessions] = await Promise.all([
  tools.agentdeck.board_list({}),
  tools.agentdeck.list_sessions(),
]);

const existingCard = (board.tasks || board.board || []).find(
  (t) => t.title === title && !["已完成", "已取消"].includes(t.status)
);
const existingWorker = (sessions.sessions || []).find((s) => s.name === workerName);

let card = existingCard;
if (!card) {
  const created = await tools.agentdeck.board_create({
    title,
    description: "用户反馈：opencode 发任务感觉有点丑。",
    cwd: taskCwd,
    project: "cc-webui",
    status: "新建",
    tags: ["cc-webui", "AgentDeck"],
  });
  card = created.task || created;
}

let worker = existingWorker;
if (!worker) {
  worker = await tools.agentdeck.new_session({
    name: workerName,
    peer_name: workerName,
    cli: "opencode",
    provider: "opencode-go",
    model: "deepseek-v4.1-flash",
    effort: "max",
    cwd: taskCwd,
  });
}

const taskId = card.id;
const workerSession = worker.id || worker.session_id;
const workerPeer = worker.address || worker.peer_name || workerName;

await tools.agentdeck.board_bind_worker({
  id: taskId, worker: workerPeer, session: workerSession, cwd: taskCwd,
});

await tools.agentdeck.board_comment({
  id: taskId, kind: "派工", text: "worker=" + workerPeer,
});

await tools.agentdeck.board_update({ id: taskId, status: "进行中" });

const message = \`【派工】\${taskId}

用户原话：
opencode 发任务感觉有点丑

路由信息：
- 工作目录：\${taskCwd}\`;

await tools.agentdeck.send_message({
  from: "mainAgent",
  to: workerPeer,
  task_id: taskId,
  send_id: \`dispatch-\${taskId}-1\`,
  message,
});

return { card_id: taskId, worker: workerPeer };`;

function render(code) {
  const details = doc.createElement('details');
  const body = doc.createElement('div');
  const handled = X.buildToolView({ name: 'execute', input: { code } }, details, body);
  // 真实渲染在 buildToolView 之后才 append（renderToolUse 里 details.append(summary, body)），
  // 这里照做，保证 cmApplyResult 能在卡内找到 .peer-send-msg 和折叠行摘要。
  if (handled) {
    const summary = doc.createElement('summary');
    summary.innerHTML = '<span class="tool-summary-text"></span>';
    details.append(summary, body);
  }
  return { details, body, handled };
}

console.log('Code Mode 展示');
const calls = X.codeModeCalls(DISPATCH);
check('抽出 8 个 agentdeck 调用，顺序正确', calls.map((c) => c.tool).join(',') ===
  'board_list,list_sessions,board_create,new_session,board_bind_worker,board_comment,board_update,send_message',
  calls.map((c) => c.tool).join(','));
check('多步摘要可读（不是整段代码）', /^8 步 · 看板列表 → 列会话 → 建卡/.test(X.codeModeSummary(calls)) &&
  !/const taskCwd|board_create\(\{/.test(X.codeModeSummary(calls)), X.codeModeSummary(calls));
check('toolSummary 对 execute 代码给出人话摘要', (() => {
  const s = X.toolSummary({ code: DISPATCH });
  return s.startsWith('8 步') && !s.includes('const taskCwd') && !s.includes('${taskId}');
})(), X.toolSummary({ code: DISPATCH }));

const info = X.a2aSendInfo('execute', { code: DISPATCH });
check('a2aSendInfo 认出 const 变量 / shorthand 的收件人', info && info.to === 'cc-webui-派工展示优化', info && info.to);
check('a2aSendInfo 抽出派工正文，而不是整段 JS', info && /^【派工】/.test(info.message) &&
  info.message.includes('用户原话') && info.message.includes('opencode 发任务感觉有点丑') &&
  !/const taskCwd|tools.agentdeck/.test(info.message), info && info.message.slice(0, 80));
check('a2aSendInfo 不是 native（Code Mode 包装）', info && info.native === false);

const multi = render(DISPATCH);
check('多步 execute 被识别为 Code Mode 卡', multi.handled === true &&
  multi.details.classList.contains('tool-code-mode'));
check('含 send_message 时保留发件卡语义 / 收件人', multi.details.classList.contains('tool-peer-send') &&
  multi.details.dataset.peerSendTo === 'cc-webui-派工展示优化');
check('派工卡默认展开，正文是派工单而不是 JS', multi.details.open === true &&
  !!multi.body.querySelector('.peer-send-msg') &&
  multi.body.querySelector('.peer-send-msg').textContent.includes('用户原话'));
check('回执到达前正文不显示字面量 ${taskId}', (() => {
  const t = multi.body.querySelector('.peer-send-msg').textContent;
  return !t.includes('${taskId}') && /^【派工】…/.test(t);
})(), multi.body.querySelector('.peer-send-msg').textContent.slice(0, 40));
check('长派工单正文默认折起，给「展开派工单」按钮', (() => {
  const msg = multi.body.querySelector('.peer-send-msg');
  const btn = multi.body.querySelector('.cm-msg-toggle');
  if (!msg || !btn || !msg.classList.contains('clip')) return false;
  btn.click();
  const opened = !msg.classList.contains('clip') && btn.textContent === '收起派工单';
  btn.click();
  return opened && msg.classList.contains('clip') && /^展开派工单/.test(btn.textContent);
})());
check('步骤列表逐条渲染（8 步）', multi.body.querySelectorAll('.cm-step').length === 8,
  String(multi.body.querySelectorAll('.cm-step').length));
const stepText = multi.body.querySelector('.cm-steps').textContent;
check('步骤标签是人话：看板 / 会话 / 发送', /看板 · 建卡/.test(stepText) &&
  /新建会话/.test(stepText) && /看板 · 绑定 worker/.test(stepText) &&
  /看板 · 评论/.test(stepText) && /发送消息/.test(stepText), stepText);
check('原始 JS 只在默认收起的 .cm-raw 里', (() => {
  const raw = multi.body.querySelector('.cm-raw');
  const pres = multi.body.querySelectorAll('pre');
  return !!raw && raw.open === false && pres.length === 1 && pres[0].closest('.cm-raw') === raw &&
    pres[0].textContent.includes('tools.agentdeck.board_list');
})());

X.cmApplyResult(multi.details, JSON.stringify({ card_id: 'T-20261117-230933.335532', worker: 'cc-webui-派工展示优化', session: 's-1' }));
const applied = multi.body.querySelector('.peer-send-msg').textContent;
check('工具回执把 ${taskId} 补成真实卡号', applied.includes('【派工】T-20261117-230933.335532') &&
  !applied.includes('${taskId}'), applied.slice(0, 40));
check('工具回执把 send_id 里的占位符也补上',
  multi.details.dataset.peerSendId === 'dispatch-T-20261117-230933.335532-1',
  multi.details.dataset.peerSendId);
check('工具回执把折叠行摘要里的卡号也补上', (() => {
  const s = multi.details.querySelector(':scope > summary .tool-summary-text').textContent;
  return s.includes('T-20261117-230933.335532') && !s.includes('${taskId}');
})(), multi.details.querySelector(':scope > summary .tool-summary-text').textContent);

const single = render('const taskId = "T-1";\nawait tools.agentdeck.board_comment({ id: taskId, kind: "回报", text: "worker 回报：已完成" });');
check('单条 board_comment 渲染成折叠的步骤卡，不摊代码', single.handled === true &&
  single.details.open === false && !single.details.classList.contains('tool-peer-send') &&
  single.body.querySelectorAll('.cm-step').length === 1 &&
  /看板 · 评论/.test(single.body.textContent));
check('单条 board_comment 摘要带 kind / 正文', (() => {
  const s = X.toolSummary({ code: 'await tools.agentdeck.board_comment({ id: "T-1", kind: "回报", text: "worker 回报：已完成" });' });
  return s === '看板 · 评论 · 「回报」 · worker 回报：已完成';
})(), X.toolSummary({ code: 'await tools.agentdeck.board_comment({ id: "T-1", kind: "回报", text: "worker 回报：已完成" });' }));

const simple = render('const r = await tools.agentdeck.send_message({ from: "oc", to: "mainAgent", message: "ack oc" });\nreturn r;');
check('单条 send_message 仍走原来的紫色发件卡', simple.handled === true &&
  simple.details.classList.contains('tool-peer-send') && !simple.details.classList.contains('tool-code-mode') &&
  !simple.body.querySelector('.cm-steps') &&
  simple.body.querySelector('.peer-send-msg').textContent === 'ack oc');

const dynamic = render('await tools.agentdeck.send_message({ to: dynTo, message: dynMsg });');
check('正文是运行时变量时不把代码当 message', dynamic.handled === true &&
  dynamic.details.classList.contains('tool-peer-send') &&
  /运行时变量/.test(dynamic.body.querySelector('.peer-send-msg').textContent) &&
  !/const |tools\.agentdeck/.test(dynamic.body.querySelector('.peer-send-msg').textContent));
check('抽不出正文时原始代码仍可查，但默认收起', (() => {
  const raw = dynamic.body.querySelector('.cm-raw');
  return !!raw && raw.open === false && raw.textContent.includes('tools.agentdeck.send_message');
})());

console.log(failed ? `\n${failed} 项失败` : '\n全部通过');
process.exit(failed ? 1 : 0);
