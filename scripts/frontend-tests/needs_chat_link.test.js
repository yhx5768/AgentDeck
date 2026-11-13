// Personal Agent：「在对话查看」优先跳当前卡片绑定的 worker 会话；没有绑定 / 会话已不存在时
// 回退主 Agent 会话。手机 / 浮层收起右栏，docked 保留开关偏好；滚到底 + 有反馈。
const { makeEnv, sampleNeeds, sampleSessions, sampleBoard } = require('./pa_harness');

let failed = 0;
function check(name, cond, extra) {
  if (cond) console.log('  ok  ' + name);
  else { failed++; console.log('  FAIL ' + name + (extra ? '  →  ' + extra : '')); }
}
const J = (x) => JSON.stringify(x);
const tick = (ms) => new Promise((r) => setTimeout(r, ms));

// 在看板快照里、但没有绑定 worker 会话的卡
const DOCS = { id: 'docs', title: '文档同步', status: '待验收', session: '', worker: '', updated_at: '2026-11-13 10:00:00', project: 'P', timeline: [], artifacts: [] };
// 绑定的 worker 会话已经打不开（404 / 已删除）的卡
const GONE = { id: 'gone', title: '已删会话的卡', status: '进行中', session: 's-gone', worker: 'w·已删除', updated_at: '2026-11-13 10:00:00', project: 'P', timeline: [], artifacts: [] };

// opts.card = 右栏当前卡；opts.task = 额外塞进看板快照的卡；opts.brokenWorker = 打开这个会话时抛错（模拟已不存在）。
function setup(width, opts = {}) {
  const { currentMain = false, card = 'refund', task = null, brokenWorker = '' } = opts;
  const env = makeEnv({ api: { '/api/needs-you': () => sampleNeeds(Date.now()), '/api/agent-board': () => sampleBoard() }, width });
  const { w, doc, ev } = env;
  ev(`state.sessions = ${J(sampleSessions())}; state.agentBoard = ${J(sampleBoard())}; pa.needs.items = ${J(sampleNeeds(Date.now()).items)}; pa.needs.counts = ${J(sampleNeeds(Date.now()).counts)}; pa.needs.loaded = true`);
  ev("pa.mode = 'agent'; document.body.classList.add('pa-on'); pa.ui.status = 'pending'; pa.ui.kind = 'all'");
  if (task) ev(`state.agentBoard.tasks.push(${J(task)})`);
  if (brokenWorker) ev(`window.__tried = []; window.__origOpenSession = openSession; openSession = async (id) => { window.__tried.push(id); if (id === ${J(brokenWorker)}) throw new Error('session not found'); return window.__origOpenSession(id); };`);
  ev(`pa.view = '${currentMain ? 'chat' : 'queue'}'; pa.asideOpen = true; pa.asideShown = ${width < 1280}; pa.ui.card = ${J(card)}; pa.ui.sel = ''; pa.ui.explicit = true`);
  ev(`state.current = state.sessions.find(s => s.id === ${J(currentMain ? 's-main' : 's-login')})`);
  ev("document.querySelector('#agent-aside').classList.remove('hidden'); document.querySelector('#needs-view').classList.remove('hidden'); document.querySelector('#chat-view').classList.add('hidden')");
  if (currentMain) { ev("document.querySelector('#chat-view').classList.remove('hidden'); document.querySelector('#needs-view').classList.add('hidden')"); }
  ev('agentRenderAside(true)');
  const box = doc.querySelector('#messages');
  Object.defineProperty(box, 'scrollHeight', { value: 5000, configurable: true });
  box.scrollTop = 120;
  return env;
}
function link(doc) { return [...doc.querySelectorAll('#agent-aside .pa-link')].find((b) => /在对话查看/.test(b.textContent)); }
const viewState = (doc, ev) => ({
  view: ev('pa.view'), asideHidden: doc.querySelector('#agent-aside').classList.contains('hidden'),
  asideShown: ev('pa.asideShown'), asideOpen: ev('pa.asideOpen'),
  chatHidden: doc.querySelector('#chat-view').classList.contains('hidden'),
  needsHidden: doc.querySelector('#needs-view').classList.contains('hidden'),
  scrolled: ev('window.__scrolled'), opened: ev('window.__opened'), tried: ev('window.__tried || []'),
  scrollTop: doc.querySelector('#messages').scrollTop,
  jump: doc.querySelector('#chat-view').classList.contains('pa-jump'),
  current: ev('state.current && state.current.id'),
  toasts: ev('window.__toasts'),
});
async function main() {
  console.log('有 worker：手机 sheet 点「在对话查看」→ 打开绑定的 worker 会话，收起右栏');
  let env = setup(390);
  let { doc, ev } = env;
  check('右栏底部有「在对话查看」按钮', !!link(doc));
  link(doc).click();
  await tick(80);
  let s = viewState(doc, ev);
  check('打开卡片绑定的 worker 会话（不是主 Agent）', s.opened === 's-pay' && s.current === 's-pay' && !s.chatHidden, J(s));
  check('worker 会话视图生效（右栏按 §17 整体收起）', s.view === 'session' && s.asideHidden && s.asideShown === false && s.needsHidden);
  check('滚到最新消息/底部', s.scrolled >= 1 && s.scrollTop > 120, J(s));
  check('chat 视图有短暂反馈（pa-jump）', s.jump);

  console.log('有 worker：浮层 1024 → 打开 worker，浮层关闭');
  env = setup(1024);
  ({ doc, ev } = env);
  link(doc).click();
  await tick(80);
  s = viewState(doc, ev);
  check('打开 worker、浮层关闭', s.view === 'session' && s.opened === 's-pay' && s.asideHidden && s.asideShown === false && !s.chatHidden, J(s));

  console.log('有 worker：桌面 docked → 打开 worker，右栏收起但不改「开」的偏好');
  env = setup(1440);
  ({ doc, ev } = env);
  link(doc).click();
  await tick(80);
  s = viewState(doc, ev);
  check('打开 worker、右栏整体收起（view=session）', s.view === 'session' && s.opened === 's-pay' && s.asideHidden && !s.chatHidden, J(s));
  check('docked 保留 asideOpen 偏好（回主会话仍按原开关显示）', s.asideOpen === true && s.asideShown === false);
  check('滚到底 + 有反馈', s.scrolled >= 1 && s.scrollTop > 120 && s.jump, J(s));

  console.log('无 worker：手机回退主 Agent（原有行为）');
  env = setup(390, { card: 'docs', task: DOCS });
  ({ doc, ev } = env);
  link(doc).click();
  await tick(80);
  s = viewState(doc, ev);
  check('回退打开主 Agent 会话', s.view === 'chat' && s.opened === 's-main' && s.current === 's-main' && !s.chatHidden, J(s));
  check('右栏 sheet 关闭、队列让开', s.asideHidden && s.asideShown === false && s.needsHidden);
  check('滚到底 + 有反馈', s.scrolled >= 1 && s.scrollTop > 120 && s.jump, J(s));

  console.log('无 worker：桌面 docked 回退主 Agent，右栏保留');
  env = setup(1440, { card: 'docs', task: DOCS });
  ({ doc, ev } = env);
  link(doc).click();
  await tick(80);
  s = viewState(doc, ev);
  check('回退主 Agent、右栏仍展开', s.view === 'chat' && s.opened === 's-main' && !s.asideHidden && s.asideOpen === true && !s.chatHidden, J(s));
  check('滚到底 + 有反馈', s.scrolled >= 1 && s.scrollTop > 120 && s.jump, J(s));

  console.log('worker 会话不存在：提示失败并回退主 Agent');
  env = setup(1440, { card: 'gone', task: GONE, brokenWorker: 's-gone' });
  ({ doc, ev } = env);
  link(doc).click();
  await tick(80);
  s = viewState(doc, ev);
  check('先尝试 worker，再回退主 Agent', J(s.tried) === J(['s-gone', 's-main']) && s.opened === 's-main' && s.current === 's-main' && s.view === 'chat', J(s));
  check('失败 toast 说明已回退', s.toasts.some((t) => t.id === 'pa-open-worker' && /打开 worker 会话失败/.test(t.title) && /回退/.test(t.body)), J(s.toasts));
  check('仍滚到底 + 有反馈', s.scrolled >= 1 && s.scrollTop > 120 && s.jump, J(s));

  console.log('已在该 chat view：无 worker 卡点击仍有滚动/反馈');
  env = setup(1440, { currentMain: true, card: 'docs', task: DOCS });
  ({ doc, ev } = env);
  doc.querySelector('#messages').scrollTop = 120;
  link(doc).click();
  await tick(80);
  s = viewState(doc, ev);
  check('不关闭 docked 右栏，view 仍是 chat', s.view === 'chat' && !s.asideHidden && !s.chatHidden, J(s));
  check('重新滚到底并闪一下（不是 no-op）', s.scrolled >= 1 && s.scrollTop === 5000 && s.jump, J(s));
  check('留言框还在（不受影响）', !!doc.querySelector('#agent-aside #pa-cnote'));

  console.log(failed ? `\n${failed} 项失败` : '\n全部通过');
  process.exit(failed ? 1 : 0);
}
main().catch((e) => { console.error(e); process.exit(1); });