// Personal Agent：主 Agent 会话里 worker 回报 / 静默步骤的折叠——分组边界、去重、计数、幂等、合并、还原。
const { makeEnv, sampleNeeds, sampleSessions, sampleBoard } = require('./pa_harness');

let failed = 0;
function check(name, cond, extra) {
  if (cond) console.log('  ok  ' + name);
  else { failed++; console.log('  FAIL ' + name + (extra ? '  →  ' + extra : '')); }
}
const J = (x) => JSON.stringify(x);
const T0 = Date.parse('2026-11-13T10:00:00');
const iso = (ms) => new Date(ms).toISOString();

function build(doc) {
  const box = doc.querySelector('#messages');
  const msg = (cls, html, attrs) => {
    const d = doc.createElement('div');
    d.className = 'msg ' + cls;
    d.innerHTML = html;
    Object.entries(attrs || {}).forEach(([k, v]) => { d.dataset[k] = v; });
    box.appendChild(d);
    return d;
  };
  const user = (t) => msg('msg-user', '<div class="role-tag">你</div><div class="bubble"></div>').querySelector('.bubble').append(t);
  const text = (t) => msg('msg-assistant', '<div class="bubble"><p></p></div>').querySelector('p').append(t);
  const quiet = () => msg('msg-assistant', '<div class="bubble"><div class="tool-block">Bash</div></div>');
  const peer = (from, t, ms) => {
    const m = msg('msg-peer', '<div class="role-tag">⇢ 来自 ' + from + '</div><div class="bubble"></div>', { peerFrom: from, peerTs: iso(ms), peerVia: 'a2a' });
    m.querySelector('.bubble').append(t);
    return m;
  };
  return { box, user, text, quiet, peer };
}

// 工具卡 / thinking 卡 / 助手步骤（带可读的 .msg-time，和真实渲染一致）。
const esc = (t) => String(t).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/"/g, '&quot;');
const toolHtml = (name, sum, body) => '<details class="tool-block"><summary><span class="tool-name">' + esc(name)
  + '</span><span class="tool-summary-text">' + esc(sum) + '</span></summary><div class="tool-body">' + esc(body) + '</div></details>';
const thinkHtml = (t) => '<details class="tool-block thinking-block"><summary>thinking…</summary><div class="tool-body">' + esc(t) + '</div></details>';
function asstMaker(doc, box) {
  return (inner, clock) => {
    const m = doc.createElement('div');
    m.className = 'msg msg-assistant';
    m.innerHTML = '<div class="role-tag">agent<span class="msg-time">2026-11-13 ' + clock + '</span></div><div class="bubble">' + inner + '</div>';
    box.appendChild(m);
    return m;
  };
}

async function main() {
  console.log('分组边界 / worker 回报');
  const env = makeEnv({ api: { '/api/needs-you': () => sampleNeeds(Date.now()), '/api/agent-board': () => sampleBoard() } });
  const { w, doc, ev } = env;
  ev("pa.mode = 'agent'; document.body.classList.add('pa-on'); pa.view = 'chat'");
  ev(`state.sessions = ${J(sampleSessions())}; state.agentBoard = ${J(sampleBoard())}`);
  ev(`pa.needs.items = ${J(sampleNeeds(Date.now()).items)}; pa.needs.loaded = true`);
  ev("state.current = state.sessions.find(s => s.main_agent)");
  const b = build(doc);
  b.user('开始吧');
  b.peer('w·支付重构', '【进度】回调幂等键已加', T0 + 1000);
  b.quiet();
  b.peer('w·登录改版', '【完成】v2.html 已上传', T0 + 2000);
  b.peer('runtime', '【卡住】w·数据迁移 超过 10 分钟没有输出', T0 + 3000);
  b.peer('w·支付重构', '【进度】回调幂等键已加', T0 + 4000);
  b.text('收到，我来汇总。');
  b.peer('w·支付重构', '【进度】第二轮', T0 + 60 * 60000);
  ev('agentFoldNow()');
  const repFolds = () => [...doc.querySelectorAll('#messages > .pa-fold:not(.silent)')];
  const silentFolds = () => [...doc.querySelectorAll('#messages > .pa-fold.silent')];
  check('用户输入、主 Agent 正文不被折进去；静默工具步骤不切开 worker 回报组', repFolds().length === 2
    && repFolds()[0].querySelectorAll('.pa-rep').length === 4 && repFolds()[1].querySelectorAll('.pa-rep').length === 1, String(repFolds().length));
  check('静默工具步骤自己折成一组（不再平铺）', silentFolds().length === 1 && silentFolds()[0].querySelectorAll('.pa-rep.step').length === 1);
  check('折叠组占原来的位置：回报组 / 静默组 / 正文回复顺序不变', J([...b.box.children].map((n) => (n.classList.contains('pa-fold') ? (n.classList.contains('silent') ? 'silent' : 'fold') : n.classList.contains('msg-user') ? 'user' : n.querySelector('.tool-block') ? 'quiet' : 'text'))) === J(['user', 'fold', 'silent', 'text', 'fold']), J([...b.box.children].map((n) => n.className)));
  check('两类组都默认折叠', !repFolds()[0].classList.contains('open') && !silentFolds()[0].classList.contains('open'));
  const head = repFolds()[0].querySelector('.pa-fold-h').textContent;
  check('回报组头部仍按 worker / 系统通知计数', /2 条 worker 回报 · 1 条系统通知/.test(head), head);
  check('重复回报只算一条并标注去重', /去重 1/.test(head) && repFolds()[0].querySelectorAll('.pa-rep.dup').length === 1);
  check('已入「需要你」计数仍挂在回报组上', /3 项已入「需要你」/.test(head), head);
  repFolds()[0].querySelector('.pa-fold-h').click();
  check('回报组点头部展开并记住', repFolds()[0].classList.contains('open') && Object.values(ev('pa.foldOpen')).includes(true));
  const repSnap = b.box.innerHTML;
  ev('agentFoldNow()');
  check('重复扫描不改动结构（幂等）', b.box.innerHTML === repSnap);
  b.peer('w·数据迁移', '【受阻】全量迁移超窗口', T0 - 5000);
  b.box.insertBefore(b.box.lastElementChild, b.box.querySelector('.msg-user').nextSibling);
  ev('agentFoldNow()');
  check('前插更早的回报并进已有组，且保持展开', repFolds().length === 2 && repFolds()[0].querySelectorAll('.pa-rep').length === 5 && repFolds()[0].classList.contains('open'));
  const assistBefore = doc.querySelectorAll('#messages > .msg-assistant').length;
  ev('agentUnfoldAll()');
  check('还原：折叠组全拆掉，peers 和静默步骤都回到对话流', doc.querySelectorAll('.pa-fold').length === 0 && doc.querySelectorAll('#messages > .msg-assistant').length === assistBefore + 1);

  console.log('静默步骤统一折叠：混合工具 / thinking / 纯看板 / 带正文');
  const env2 = makeEnv({ api: { '/api/needs-you': () => sampleNeeds(Date.now()), '/api/agent-board': () => sampleBoard() } });
  const d2 = env2.doc, ev2 = env2.ev;
  ev2("pa.mode = 'agent'; document.body.classList.add('pa-on'); pa.view = 'chat'");
  ev2(`state.sessions = ${J(sampleSessions())}; state.agentBoard = ${J(sampleBoard())}`);
  ev2("state.current = state.sessions.find(s => s.main_agent)");
  const b2 = build(d2);
  const asst = asstMaker(d2, b2.box);
  b2.user('看一眼支付重构在干什么，顺手记到退款卡上');
  b2.peer('w·支付重构', '【进度】回调幂等键已加', T0 + 1000);
  asst(toolHtml('mcp__agentdeck__board_comment', '{"id":"refund"}', '{"id":"refund","text":"回调幂等键已加"}')
    + toolHtml('mcp__agentdeck__board_update', '{"id":"refund"}', '{"id":"refund","status":"进行中"}'), '10:00:01');
  asst(toolHtml('mcp__agentdeck__get_session', 'session=支付重构', '{"session":"s-pay"}')
    + toolHtml('mcp__agentdeck__read_session_messages', 'before=…', '{"session":"s-pay"}')
    + toolHtml('Bash', 'git status --short', 'git status')
    + toolHtml('Read', '/tmp/refund.md', '{}'), '10:00:02');
  asst(thinkHtml('先看看 worker 现在跑到哪一步了，再决定要不要打扰它。'), '10:00:03');
  const withText = asst(thinkHtml('顺手确认一下没有未提交的改动。')
    + toolHtml('Bash', 'git log -1 --oneline', 'git log') + '<p>查完了，正在汇总。</p>', '10:00:04');
  b2.text('查完了，正在汇总。');
  asst(toolHtml('Bash', 'agentdeck board comment --task "$AGENTDECK_TASK_ID" --text "已核对退款进度"', 'agentdeck board comment …'), '10:05:00');
  asst(toolHtml('mcp__agentdeck__board_comment', '{"id":"refund"}', '{"id":"refund","text":"已核对退款进度"}'), '10:05:01');
  b2.peer('w·登录改版', '【完成】v2.html 已上传', T0 + 10 * 60000);
  ev2('agentFoldNow()');
  const sf = () => [...d2.querySelectorAll('#messages > .pa-fold.silent')];
  const rf = () => [...d2.querySelectorAll('#messages > .pa-fold:not(.silent)')];
  check('混合工具 + thinking 的连续步骤成一整组；带正文的回复另起一组；纯看板一组；回报组各归各', sf().length === 3 && rf().length === 2, J([...b2.box.children].map((n) => n.className)));
  const a = sf()[0];
  const headA = a.querySelector('.pa-fold-h').textContent.replace(/\s+/g, ' ');
  check('头部：N 个工具调用 · M 步操作，默认折叠', /^6 个工具调用 · 3 步操作已折叠/.test(headA) && !a.classList.contains('open'), headA);
  check('类型小计：看板 / 读取会话 / Bash / 其他', J([...a.querySelectorAll('.ws .pa-pill')].map((p) => p.textContent)) === J(['看板 ×2', '读取会话 ×2', 'Bash', '其他']), J([...a.querySelectorAll('.ws .pa-pill')].map((p) => p.textContent)));
  check('每步一行：看板步给动作和卡点，混合步给工具名与摘要，thinking 步标「思考」', a.querySelectorAll('.pa-rep.step').length === 3
    && /支付网关：退款链路迁移/.test(a.querySelectorAll('.pa-rep')[0].textContent) && /回调幂等键已加/.test(a.querySelectorAll('.pa-rep')[0].textContent)
    && /get_session/.test(a.querySelectorAll('.pa-rep')[1].textContent) && /git status/.test(a.querySelectorAll('.pa-rep')[1].textContent)
    && /思考/.test(a.querySelectorAll('.pa-rep')[2].textContent) && /先看看 worker/.test(a.querySelectorAll('.pa-rep')[2].textContent));
  check('thinking 收在组里，不留在对话流', a.querySelectorAll('.thinking-block').length === 1 && !b2.box.querySelector(':scope > .msg-assistant > .bubble > .thinking-block'));
  check('带正文的回复：正文留在对话流，工具卡从消息里搬走', withText.parentElement === b2.box
    && /查完了，正在汇总/.test(withText.textContent) && !withText.querySelector(':scope > .bubble > .tool-block'));
  const partFold = sf()[1];
  check('工具部分单独一组，就挂在这条回复下面', withText.nextElementSibling === partFold && partFold.classList.contains('silent') && partFold.querySelectorAll('.pa-rep.step').length === 1);
  const pHead = partFold.querySelector('.pa-fold-h').textContent.replace(/\s+/g, ' ');
  check('这组头部只数它的工具调用（thinking 不算调用）', /^1 个工具调用 · 1 步操作/.test(pHead) && J([...partFold.querySelectorAll('.ws .pa-pill')].map((p) => p.textContent)) === J(['Bash']), pHead);
  check('组里保留 thinking + 工具卡，正文不在组里', partFold.querySelectorAll('.pa-rep-b .tool-block').length === 2 && partFold.querySelectorAll('.thinking-block').length === 1 && !/查完了，正在汇总/.test(partFold.textContent));
  const bHead = sf()[2].querySelector('.pa-fold-h').textContent.replace(/\s+/g, ' ');
  check('纯看板组：动作小计（评论 ×2），不给工具类型 chip', /^2 个工具调用 · 2 步操作/.test(bHead)
    && J([...sf()[2].querySelectorAll('.ws .pa-pill')].map((p) => p.textContent)) === J(['评论 ×2']), bHead + ' | ' + J([...sf()[2].querySelectorAll('.ws .pa-pill')].map((p) => p.textContent)));
  console.log('静默组：展开 / 幂等 / load more');
  a.querySelector('.pa-fold-h').click();
  check('点头部展开静默组', a.classList.contains('open') && a.querySelector('.pa-fold-h').getAttribute('aria-expanded') === 'true');
  a.querySelector('.pa-rep-h').click();
  check('点一行才展开原始步骤（工具卡都在）', a.querySelector('.pa-rep').classList.contains('open') && a.querySelectorAll('.pa-rep-b .tool-block').length === 7);
  const snapA = b2.box.innerHTML;
  ev2('agentFoldNow()');
  check('重复扫描不改结构（幂等）', b2.box.innerHTML === snapA && a.classList.contains('open'));
  const early = asst(toolHtml('mcp__agentdeck__get_session', 'session=支付重构', '{"session":"s-pay"}'), '09:59:00');
  a.before(early); // 「加载更多」：更早的步骤前插到组旁边
  ev2('agentFoldNow()');
  const aAfter = sf()[0];
  check('前插的更早步骤并进同一组，计数增加，展开状态不重置', sf().length === 3 && aAfter.querySelectorAll('.pa-rep.step').length === 4
    && /^7 个工具调用 · 4 步操作/.test(aAfter.querySelector('.pa-fold-h').textContent.replace(/\s+/g, ' ')) && aAfter.classList.contains('open'));
  ev2('agentUnfoldAll()');
  check('还原：带正文回复的工具卡回到那条回复下面，正文不动', d2.querySelectorAll('.pa-fold').length === 0
    && !!withText.querySelector(':scope > .bubble > .tool-block') && /查完了，正在汇总/.test(withText.textContent));

  console.log('thinking-only');
  const env3 = makeEnv({ api: { '/api/needs-you': () => sampleNeeds(Date.now()), '/api/agent-board': () => sampleBoard() } });
  const d3 = env3.doc, ev3 = env3.ev;
  ev3("pa.mode = 'agent'; document.body.classList.add('pa-on'); pa.view = 'chat'");
  ev3(`state.sessions = ${J(sampleSessions())}; state.agentBoard = ${J(sampleBoard())}`);
  ev3("state.current = state.sessions.find(s => s.main_agent)");
  const b3 = build(d3);
  const asst3 = asstMaker(d3, b3.box);
  b3.user('想一下');
  asst3(thinkHtml('先确认要不要动线上，再决定下一步。'), '10:10:00');
  b3.text('想好了。');
  ev3('agentFoldNow()');
  const only = [...d3.querySelectorAll('#messages > .pa-fold.silent')];
  check('只有 thinking 的步骤也折，标题写「N 步思考」', only.length === 1 && /^1 步思考已折叠/.test(only[0].querySelector('.pa-fold-h').textContent.replace(/\s+/g, ' ')));
  check('thinking 组给「思考」小计，thinking 本体藏在组里', J([...only[0].querySelectorAll('.ws .pa-pill')].map((p) => p.textContent)) === J(['思考 ×1'])
    && only[0].querySelectorAll('.thinking-block').length === 1 && !b3.box.querySelector(':scope > .msg-assistant > .bubble > .thinking-block'));
  check('正文回复仍然在对话流里，不被吞', d3.querySelectorAll('#messages > .msg-assistant').length === 1 && b3.box.querySelector(':scope > .msg-assistant').textContent.includes('想好了'));

  console.log(failed ? `\n${failed} 项失败` : '\n全部通过');
  process.exit(failed ? 1 : 0);
}
main().catch((e) => { console.error(e); process.exit(1); });
