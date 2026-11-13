// Personal Agent：「在对话查看」真的切到主 Agent 会话；手机/浮层收起右栏，docked 保留；滚到底 + 有反馈。
const { makeEnv, sampleNeeds, sampleSessions, sampleBoard } = require('./pa_harness');

let failed = 0;
function check(name, cond, extra) {
  if (cond) console.log('  ok  ' + name);
  else { failed++; console.log('  FAIL ' + name + (extra ? '  →  ' + extra : '')); }
}
const J = (x) => JSON.stringify(x);
const tick = (ms) => new Promise((r) => setTimeout(r, ms));

function setup(width, currentMain) {
  const env = makeEnv({ api: { '/api/needs-you': () => sampleNeeds(Date.now()), '/api/agent-board': () => sampleBoard() }, width });
  const { w, doc, ev } = env;
  ev(`state.sessions = ${J(sampleSessions())}; state.agentBoard = ${J(sampleBoard())}; pa.needs.items = ${J(sampleNeeds(Date.now()).items)}; pa.needs.counts = ${J(sampleNeeds(Date.now()).counts)}; pa.needs.loaded = true`);
  ev("pa.mode = 'agent'; document.body.classList.add('pa-on'); pa.ui.status = 'pending'; pa.ui.kind = 'all'");
  ev(`pa.view = '${currentMain ? 'chat' : 'queue'}'; pa.asideOpen = true; pa.asideShown = ${width < 1280}; pa.ui.card = 'refund'; pa.ui.sel = ''`);
  ev(`state.current = state.sessions.find(s => ${currentMain ? 's.main_agent' : '!s.main_agent'})`);
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
  scrolled: ev('window.__scrolled'), opened: ev('window.__opened'),
  scrollTop: doc.querySelector('#messages').scrollTop,
  jump: doc.querySelector('#chat-view').classList.contains('pa-jump'),
  current: ev('state.current && state.current.id'),
});
async function main() {
  console.log('手机 sheet：点「在对话查看」→ 关闭右栏并切到 chat');
  let env = setup(390, false);
  let { w, doc, ev } = env;
  check('右栏底部有「在对话查看」按钮', !!link(doc));
  link(doc).click();
  await tick(80);
  let s = viewState(doc, ev);
  check('切到主 Agent 会话（当前不在主会话也能打开）', s.view === 'chat' && s.opened === 's-main' && s.current === 's-main' && !s.chatHidden, J(s));
  check('右栏 sheet 关闭、队列让开', s.asideHidden && s.asideShown === false && s.needsHidden);
  check('滚到最新消息/底部', s.scrolled >= 1 && s.scrollTop > 120, J(s));
  check('chat 视图有短暂反馈（pa-jump）', s.jump);

  console.log('浮层 1024：同样关闭右栏');
  env = setup(1024, false);
  ({ w, doc, ev } = env);
  link(doc).click();
  await tick(80);
  s = viewState(doc, ev);
  check('切到 chat、浮层关闭', s.view === 'chat' && s.asideHidden && s.asideShown === false && !s.chatHidden, J(s));

  console.log('桌面 docked：保留右栏，chat 可见并回到底部');
  env = setup(1440, false);
  ({ w, doc, ev } = env);
  link(doc).click();
  await tick(80);
  s = viewState(doc, ev);
  check('切到 chat、右栏仍展开', s.view === 'chat' && !s.asideHidden && s.asideOpen === true && !s.chatHidden, J(s));
  check('滚到底 + 有反馈', s.scrolled >= 1 && s.scrollTop > 120 && s.jump, J(s));

  console.log('已在该 chat view：点击仍有滚动/反馈');
  env = setup(1440, true);
  ({ w, doc, ev } = env);
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
