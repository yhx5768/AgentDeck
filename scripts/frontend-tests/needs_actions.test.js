// Personal Agent：作答动作（权限 / 提问 / 拍板 / 验收）发哪些请求、回执怎么记、失败怎么处理，
// 以及右栏常驻留言框。api 全是桩，断言的是「请求的形状与顺序」。
const { makeEnv, sampleNeeds, sampleSessions, sampleBoard } = require('./pa_harness');

let failed = 0;
function check(name, cond, extra) {
  if (cond) console.log('  ok  ' + name);
  else { failed++; console.log('  FAIL ' + name + (extra ? '  →  ' + extra : '')); }
}
const J = (x) => JSON.stringify(x);

// 一个够用的假服务端：决议 prompt / 改看板状态后，下一次 GET /api/needs-you 里对应条目就没了
// （和真服务端一致），否则乐观摘除后的对账刷新会把条目又拉回来。
function fresh(api) {
  const now = Date.now();
  const srv = { items: sampleNeeds(now).items };
  const needs = () => {
    const n = sampleNeeds(Date.now());
    n.items = srv.items;
    n.counts = { all: srv.items.length, perm: 0, ask: 0, blocked: 0, review: 0 };
    srv.items.forEach((i) => { n.counts[i.kind]++; });
    return n;
  };
  const env = makeEnv({ api: Object.assign({
    '/api/needs-you': needs,
    '/api/agent-board': () => sampleBoard(),
    'POST /api/sessions/*': (b, p) => {
      const m = /permissions\/(\w+)$/.exec(p);
      if (m) srv.items = srv.items.filter((i) => i.pending_id !== m[1]);
      return { ok: true };
    },
    'PATCH /api/agent-board/tasks/*': (b, p) => {
      const id = p.split('/').pop();
      srv.items = srv.items.filter((i) => !(i.task_id === id && (i.kind === 'blocked' || i.kind === 'review')));
      return { ok: true };
    },
  }, api || {}) });
  const { ev } = env;
  ev("pa.mode = 'agent'; document.body.classList.add('pa-on')");
  ev(`state.sessions = ${J(sampleSessions())}; state.agentBoard = ${J(sampleBoard())}`);
  ev(`pa.needs.items = ${J(sampleNeeds(now).items)}; pa.needs.counts = ${J(sampleNeeds(now).counts)}; pa.needs.loaded = true`);
  ev("document.querySelector('#agent-aside').classList.remove('hidden')");
  return env;
}
const callsTo = (w, re) => w.__calls.filter((c) => re.test(c.method + ' ' + c.path));

async function main() {
  console.log('权限审批');
  let env = fresh();
  let { w, doc, ev } = env;
  ev("agentSelectItem('perm:p1')");
  let ok = await ev("paAnswerPerm(paItemById('perm:p1'), 'allow')");
  const post = callsTo(w, /POST \/api\/sessions\/s-pay\/permissions\/p1/)[0];
  check('允许一次 → POST 决议到 worker 会话', ok === true && post && post.body.behavior === 'allow');
  check('看板记账：往卡片时间线追加「用户批准」', callsTo(w, /POST \/api\/agent-board\/tasks\/refund\/events/)[0].body.kind === '用户批准');
  check('权限审批不唤醒主 Agent（不发 A2A 回执）', callsTo(w, /a2a\/send/).length === 0);
  check('条目本地先摘掉，计数同步', ev("paItemById('perm:p1')") === null && ev('pa.needs.counts.perm') === 0 && ev('pa.needs.counts.all') === 4);
  check('进「已处理」记录并选中回执', ev('pa.done.length') === 1 && ev('pa.ui.sel').startsWith('done:') && ev('pa.ui.pin') === ev('pa.ui.sel'));
  check('回执面板列出实际做过的步骤', doc.querySelectorAll('.pa-rcp li').length === 2 && !doc.querySelector('.pa-rcp li.bad'));
  check('已处理记录持久化到 localStorage', J(JSON.parse(w.localStorage.getItem('cc-webui:pa-done')).map((d) => d.kind)) === J(['perm']));

  env = fresh();
  ({ w, doc, ev } = env);
  ev("agentSelectItem('perm:p1')");
  await ev("paAnswerPerm(paItemById('perm:p1'), 'deny', '不要覆盖远端')");
  const deny = callsTo(w, /permissions\/p1/)[0].body;
  check('拒绝带理由回传模型', deny.behavior === 'deny' && deny.message === '不要覆盖远端');

  env = fresh({ 'POST /api/sessions/s-pay/permissions/p1': { ok: false, error: 'prompt already resolved or timed out' } });
  ({ w, doc, ev } = env);
  ev("agentSelectItem('perm:p1')");
  ok = await ev("paAnswerPerm(paItemById('perm:p1'), 'allow')");
  check('已被处理 / 已超时：主动作失败 → 条目保留、给出错误、不记已处理', ok === false && /这条请求已被处理或已超时/.test(ev("pa.err['perm:p1']")) && ev('pa.done.length') === 0 && !!ev("paItemById('perm:p1')"));
  check('主动作失败就不写看板、不发回执', callsTo(w, /agent-board|a2a/).length === 0);

  console.log('提问');
  env = fresh();
  ({ w, doc, ev } = env);
  ev("agentSelectItem('ask:a1')");
  doc.querySelectorAll('.pa-askf .perm-opt')[1].click();
  await ev("paAnswerAsk(paItemById('ask:a1'), false)");
  const ans = callsTo(w, /permissions\/a1/)[0].body;
  check('回传 {问题原文: 选项 label}，behavior=allow', ans.behavior === 'allow' && J(ans.answers) === J({ '退款失败时怎么处理？': '退到余额' }));
  const rc = callsTo(w, /a2a\/send/)[0].body;
  check('提问作答后给主 Agent 发 runtime 回执（带幂等键）', rc.from === 'runtime' && rc.to === 'mainAgent' && /退到余额/.test(rc.message) && /^needs-you:ask:a1:/.test(rc.send_id));
  check('回执顺序：先决议、再看板、最后回执', J(w.__calls.filter((c) => c.method === 'POST' && !/needs-you/.test(c.path)).map((c) => c.path.replace(/^\/api\//, '').split('/')[0] + (c.path.includes('a2a') ? '' : ''))) === J(['sessions', 'agent-board', 'a2a']), J(w.__calls.map((c) => c.method + ' ' + c.path)));

  env = fresh();
  ({ w, ev } = env);
  await ev("paAnswerAsk(paItemById('ask:a1'), true)");
  check('跳过 → deny + 说明', callsTo(w, /permissions\/a1/)[0].body.behavior === 'deny' && /跳过/.test(callsTo(w, /permissions\/a1/)[0].body.message));

  console.log('拍板');
  env = fresh();
  ({ w, doc, ev } = env);
  ev("agentSelectItem('blocked:shard')");
  check('没写决定不让提交', (await ev("paDecideBlocked(paItemById('blocked:shard'))")) === false && w.__calls.filter((c) => c.method !== 'GET').length === 0);
  ev("paDraft('blocked:shard').note = '先迁热表'");
  await ev("paDecideBlocked(paItemById('blocked:shard'))");
  const seq = w.__calls.filter((c) => c.method !== 'GET').map((c) => c.method + ' ' + c.path.replace(/^\/api\//, ''));
  check('拍板：直达 worker 发消息 → 看板状态回进行中 → 记账 → 回执', J(seq) === J(['POST sessions/s-mig/messages', 'PATCH agent-board/tasks/shard', 'POST agent-board/tasks/shard/events', 'POST a2a/send']), J(seq));
  check('给 worker 的消息带原话', /【拍板】/.test(callsTo(w, /s-mig\/messages/)[0].body.text) && /先迁热表/.test(callsTo(w, /s-mig\/messages/)[0].body.text));
  check('看板状态置「进行中」', callsTo(w, /PATCH/)[0].body.status === '进行中');

  console.log('验收');
  env = fresh();
  ({ w, doc, ev } = env);
  ev("agentSelectItem('review:login')");
  check('打回不写原因 → 不发任何请求，提示必填', (await ev("paDecideReview(paItemById('review:login'), false)")) === false && w.__calls.filter((c) => c.method !== 'GET').length === 0 && /打回需要写明原因/.test(ev("pa.err['review:login']")));
  ev("paDraft('review:login').note = '深色对比度不够'");
  await ev("paDecideReview(paItemById('review:login'), false)");
  check('打回：把意见发给 worker + 状态回进行中', /【打回】/.test(callsTo(w, /s-login\/messages/)[0].body.text) && callsTo(w, /PATCH/)[0].body.status === '进行中');
  env = fresh();
  ({ w, ev } = env);
  await ev("paDecideReview(paItemById('review:login'), true)");
  check('通过：看板置已完成，不打扰 worker', callsTo(w, /PATCH/)[0].body.status === '已完成' && callsTo(w, /\/messages/).length === 0);

  console.log('次要步骤失败');
  env = fresh({ 'POST /api/agent-board/tasks/login/events': () => { throw new Error('board down'); } });
  ({ w, doc, ev } = env);
  ev("agentSelectItem('ask:a1')");
  doc.querySelectorAll('.pa-askf .perm-opt')[0].click();
  ok = await ev("paAnswerAsk(paItemById('ask:a1'), false)");
  check('看板记账失败不撤销已送达的决定：仍算已处理，回执里这一步标红并写原因', ok === true && ev('pa.done.length') === 1 && !!doc.querySelector('.pa-rcp li.bad') && /board down/.test(doc.querySelector('.pa-rcp li.bad').textContent));
  check('后续的回执步骤照常发', callsTo(w, /a2a\/send/).length === 1);

  console.log('右栏常驻留言框');
  env = fresh();
  ({ w, doc, ev } = env);
  ev("agentSelectItem('perm:p1')");
  const ta = () => doc.querySelector('#pa-cnote');
  check('选了卡片：提示会带上这张卡的上下文', /mainAgent/.test(ta().placeholder) && /这张卡/.test(ta().placeholder));
  check('空内容不发请求', (await ev('paSendNote()')) === undefined && w.__calls.filter((c) => c.method !== 'GET').length === 0);
  ta().value = '先别动退款，等我确认口径';
  ta().dispatchEvent(new w.Event('input'));
  await ev('paSendNote()');
  const msg = callsTo(w, /POST \/api\/sessions\/s-main\/messages/)[0];
  check('留言发给主 Agent，带结构化 board_task_id', !!msg && msg.body.board_task_id === 'refund' && /先别动退款/.test(msg.body.text));
  check('消息正文带卡片上下文（task_id / 标题）', /task_id: refund/.test(msg.body.text) && /支付网关/.test(msg.body.text));
  check('同时往卡片时间线记一条用户消息', callsTo(w, /POST \/api\/agent-board\/tasks\/refund\/events/)[0].body.kind === '用户消息');
  check('发完清掉草稿，状态进入「回复中」并开始等回复', ta().value === '' && /回复中/.test(doc.querySelector('.pa-cstatus').textContent) && ev('pa.cmp.wait.taskId') === 'refund');
  check('回写之前不算已回复', (ev('paCheckReply()'), ev('!!pa.cmp.wait')));
  ev(`state.agentBoard.tasks.find(t => t.id === 'refund').timeline.push({ at: '2026-11-13 10:05:00', kind: '主 Agent 回复', text: '收到，退款链路先暂停', data: { turn_id: 't9' } })`);
  ev('paCheckReply()');
  check('看板里出现新的主 Agent 回复 → 标已回复、切到「动态」并把这条回复闪一下（和抽屉一致）', !ev('pa.cmp.wait') && /已回复/.test(doc.querySelector('.pa-cstatus').textContent) && ev('pa.ui.tab') === 'feed'
    && !!doc.querySelector('#agent-aside .board-drawer-event.board-event-fresh[data-kind="主 Agent 回复"]'));

  env = fresh();
  ({ w, doc, ev } = env);
  ev("pa.ui.sel = ''; pa.ui.card = ''; agentRenderAside(true)");
  ta().value = '汇总一下今天的进展';
  ta().dispatchEvent(new w.Event('input'));
  await ev('paSendNote()');
  const plain = callsTo(w, /POST \/api\/sessions\/s-main\/messages/)[0];
  check('没选卡片：普通消息，不带任务上下文也不记看板', plain.body.text === '汇总一下今天的进展' && plain.body.board_task_id === undefined && callsTo(w, /agent-board/).length === 0);
  check('状态写明发给了谁', /已发送给 mainAgent/.test(doc.querySelector('.pa-cstatus').textContent));

  env = fresh({ 'POST /api/sessions/s-main/messages': () => { throw new Error('502 bad gateway'); } });
  ({ w, doc, ev } = env);
  ev("pa.ui.sel = ''; pa.ui.card = ''; agentRenderAside(true)");
  ta().value = '这条会发失败';
  ta().dispatchEvent(new w.Event('input'));
  await ev('paSendNote()');
  check('发送失败：给出原因、保留草稿、发送键恢复可用', /发送失败：502 bad gateway/.test(doc.querySelector('.pa-cstatus.error').textContent) && ta().value === '这条会发失败' && !doc.querySelector('#pa-csend').disabled);

  env = fresh();
  ({ w, doc, ev } = env);
  ev("state.sessions = []; pa.ui.sel = ''; pa.ui.card = ''; agentRenderAside(true)");
  check('没有主 Agent：留言框禁用并说明原因', ta().disabled === true && /还没有主 Agent/.test(ta().placeholder) && doc.querySelector('#pa-csend').disabled === true);

  console.log(failed ? `\n${failed} 项失败` : '\n全部通过');
  process.exit(failed ? 1 : 0);
}
main().catch((e) => { console.error(e); process.exit(1); });
