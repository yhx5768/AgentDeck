// Personal Agent：右栏的默认焦点 = 最新「待人工」卡；显式跟随、离队回退、无待人工、关闭重开、worker 会话收起。
const { makeEnv, sampleNeeds, sampleSessions } = require('./pa_harness');

let failed = 0;
function check(name, cond, extra) {
  if (cond) console.log('  ok  ' + name);
  else { failed++; console.log('  FAIL ' + name + (extra ? '  →  ' + extra : '')); }
}
const J = (x) => JSON.stringify(x);

function board() {
  const task = (id, status, updated, sid) => ({
    id, title: '任务 ' + id, full_title: '任务 ' + id, status, session: sid, worker: sid,
    updated_at: updated, timeline: [], artifacts: [], project: 'P',
  });
  return { tasks: [
    task('b-old', '待人工', '2026-11-13 09:00:00', 's-mig'),
    task('b-new', '待人工', '2026-11-13 10:10:00', 's-pay'),
    task('b-newer', '待人工', '2026-11-13 10:20:00', 's-login'),
    task('rev', '待验收', '2026-11-13 10:30:00', 's-login'),
    task('busy', '进行中', '2026-11-13 10:40:00', 's-pay'),
  ] };
}
function items(now) {
  const base = sampleNeeds(now);
  return { items: [
    { id: 'blocked:b-old', kind: 'blocked', state: 'open', session_id: 's-mig', session_name: 'w·数据迁移', task_id: 'b-old', task_title: '任务 b-old', task_status: '待人工', title: '任务 b-old', summary: '全量迁移超窗口', created_at: now - 3600000 },
    { id: 'review:rev', kind: 'review', state: 'open', session_id: 's-login', session_name: 'w·登录改版', task_id: 'rev', task_title: '任务 rev', task_status: '待验收', title: '任务 rev', created_at: now - 600000 },
  ], counts: { all: 2, perm: 0, ask: 0, blocked: 1, review: 1 }, server_time: now, window_ms: 55000, board: { ok: true } };
}

function main() {
  const now = Date.now();
  const env = makeEnv({ api: { '/api/needs-you': () => items(Date.now()), '/api/agent-board': () => board() } });
  const { w, doc, ev } = env;
  ev(`state.sessions = ${J(sampleSessions())}; state.agentBoard = ${J(board())}; state.mainAgentBoardAt = ${Date.now() + 1e9}`);
  ev(`pa.needs.items = ${J(items(now).items)}; pa.needs.counts = ${J(items(now).counts)}; pa.needs.loaded = true`);
  ev("pa.mode = 'agent'; document.body.classList.add('pa-on'); pa.view = 'queue'; pa.asideOpen = true; pa.ui.status = 'pending'; pa.ui.kind = 'all'; pa.ui.group = 'tier'");
  ev("document.querySelector('#needs-view').classList.remove('hidden'); document.querySelector('#agent-aside').classList.remove('hidden')");
  const show = () => { ev('agentRenderAside(true)'); return ev("JSON.stringify({ card: paAsideModel().taskId, explicit: pa.ui.explicit, blocked: pa.ui.explicitBlocked, sel: pa.ui.sel, ui: pa.ui.card })"); };
  const setTask = (id, patch) => ev(`Object.assign(paBoardTasks().find(t => t.id === '${id}'), ${J(patch)}); state.mainAgentBoardAt = ${Date.now() + 1e9}`);
  const setItems = (list) => ev(`pa.needs.items = ${J(list)}; pa.needs.counts = { all: ${list.length}, perm: 0, ask: 0, blocked: ${list.filter((x) => x.kind === 'blocked').length}, review: ${list.filter((x) => x.kind === 'review').length} }; agentRenderAside(true)`);
  const head = () => (doc.querySelector('#agent-aside .pa-ah') || {}).textContent || '';
  const st = () => (doc.querySelector('#agent-aside .pa-dt') || {}).textContent || '';

  console.log('默认焦点');
  ev('agentRenderQueue(true)');
  let s = JSON.parse(show());
  check('没有任何选择：默认最新「待人工」卡', s.card === 'b-old' && s.explicit === false, JSON.stringify(s));
  check('头部是这张卡的状态和 id，正文是它的标题', /待人工/.test(head()) && /b-old/.test(head()) && /任务 b-old/.test(st()), head() + ' | ' + st());
  check('有 needs-you 待办的卡优先（b-old 更旧但有待办）', s.card === 'b-old');
  check('队列的自动选中也跟着它，两边不打架', s.sel === 'blocked:b-old' && s.ui === 'b-old', JSON.stringify(s));

  console.log('同优先级按 updated_at 倒序');
  setItems(items(now).items.filter((i) => i.id !== 'blocked:b-old'));
  s = JSON.parse(show());
  check('去掉待办后：最新的 b-newer 胜出（不是按 id / 建卡顺序）', s.card === 'b-newer' && /任务 b-newer/.test(st()), JSON.stringify(s));

  console.log('显式选择');
  ev("agentSelectItem('review:rev')");
  s = JSON.parse(show());
  check('点队列里的待验收条目：右栏跟随这张卡，并记为显式选择', s.card === 'rev' && s.explicit === true && s.blocked === false, JSON.stringify(s));
  setTask('b-new', { updated_at: '2026-11-13 10:59:00' });
  s = JSON.parse(show());
  check('别的待人工卡更新了也不抢走焦点（用户选的就跟着用户）', s.card === 'rev' && s.explicit === true);

  console.log('当前卡离开待人工');
  // 给下一张待人工卡挂一条待办，验证回退时队列选中也跟着走
  ev(`pa.needs.items.push(${J({ id: 'blocked:b-new', kind: 'blocked', state: 'open', session_id: 's-pay', session_name: 'w·支付重构', task_id: 'b-new', task_title: '任务 b-new', task_status: '待人工', title: '任务 b-new', summary: '等你拍板', created_at: Date.now() - 300000 })}); pa.needs.counts.all++; pa.needs.counts.blocked++`);
  ev("agentOpenCard('b-newer')");
  s = JSON.parse(show());
  check('点看板卡 / 头像：跟随，并记住它是待人工卡', s.card === 'b-newer' && s.explicit === true && s.blocked === true, JSON.stringify(s));
  setTask('b-newer', { status: '进行中' });
  s = JSON.parse(show());
  check('这张卡被处理（离开待人工）→ 自动交还默认，切到下一张最新的待人工', s.card === 'b-new' && s.explicit === false, JSON.stringify(s));
  check('队列选中跟着默认焦点走（b-new 的那条待办）', s.sel === 'blocked:b-new' && s.ui === 'b-new', JSON.stringify(s));
  check('被切走的卡不再出现在右栏', /任务 b-new/.test(st()) && !/任务 b-newer/.test(st()));

  console.log('没有待人工');
  setTask('b-new', { status: '已完成' });
  setTask('b-old', { status: '进行中' });
  ev('agentRenderAside(true)');
  check('一张待人工都没有：显示现有空态，不报错', !!doc.querySelector('#agent-aside .pa-empty') && /选一张卡片/.test(doc.querySelector('#agent-aside .pa-empty').textContent) && !doc.querySelector('#agent-aside .pa-dt'));
  check('默认焦点为空，也没有把显式选择留在体内', ev('paAsideModel().taskId') === '' && ev('pa.ui.explicit') === false);

  console.log('关闭 / 重开');
  setTask('b-old', { status: '待人工' });
  ev("agentSelectItem('review:rev')");
  check('先显式选到别的卡', JSON.parse(show()).card === 'rev');
  ev('paCloseAside()');
  check('关闭：右栏收起、显式焦点作废', ev('pa.asideOpen') === false && ev('pa.ui.explicit') === false && doc.querySelector('#agent-aside').classList.contains('hidden'));
  ev('paSyncAside(true)');
  s = JSON.parse(show());
  check('重新打开：回到最新待人工（不是关之前那张）', ev('pa.asideOpen') === true && !doc.querySelector('#agent-aside').classList.contains('hidden') && s.card === 'b-old' && s.explicit === false, JSON.stringify(s));

  console.log('worker 会话');
  ev("agentOnSessionOpened(state.sessions.find(x => x.id === 's-pay'))");
  check('打开 worker 会话：右栏按现有规则收起', ev('pa.view') === 'session' && doc.querySelector('#agent-aside').classList.contains('hidden') && doc.querySelector('#pa-aside-handle').classList.contains('hidden'));
  const before = doc.querySelector('#agent-aside').innerHTML;
  ev('agentRenderAside(true)');
  check('收起时不会被异步刷新偷偷画出来', doc.querySelector('#agent-aside').innerHTML === before && doc.querySelector('#agent-aside').classList.contains('hidden'));
  ev("agentOnSessionOpened(state.sessions.find(x => x.main_agent))");
  s = JSON.parse(show());
  check('回到主 Agent 会话：右栏回来，焦点还是最新待人工', !doc.querySelector('#agent-aside').classList.contains('hidden') && s.card === 'b-old' && s.explicit === false, JSON.stringify(s));
  console.log(failed ? `\n${failed} 项失败` : '\n全部通过');
  process.exit(failed ? 1 : 0);
}
try { main(); } catch (e) { console.error(e); process.exit(1); }
