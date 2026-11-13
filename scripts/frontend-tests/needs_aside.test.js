// Personal Agent：左栏头像簇 / 入口导航 / 右栏渲染（概览、作答表单、动态、空态）。
const { makeEnv, sampleNeeds, sampleSessions, sampleBoard } = require('./pa_harness');

let failed = 0;
function check(name, cond, extra) {
  if (cond) console.log('  ok  ' + name);
  else { failed++; console.log('  FAIL ' + name + (extra ? '  →  ' + extra : '')); }
}
const J = (x) => JSON.stringify(x);

async function main() {
  const now = Date.now();
  const env = makeEnv({ api: { '/api/needs-you': () => sampleNeeds(Date.now()) } });
  const { w, doc, ev } = env;
  ev("pa.mode = 'agent'; document.body.classList.add('pa-on')");

  console.log('头像簇');
  ev('state.sessions = []; agentRenderCluster()');
  const cta = doc.querySelector('#main-agents .pa-agents-card.empty');
  check('没有主 Agent：给出新建入口而不是空白', !!cta && /新建主 Agent/.test(cta.textContent) && !doc.querySelector('#main-agents .pa-av'));
  ev(`state.sessions = ${J(sampleSessions().map((s) => (s.id === 's-pay' ? Object.assign({}, s, { status: { running: false } }) : s)))}; state.agentBoard = ${J(sampleBoard())}`);
  ev("pa.needs = Object.assign(pa.needs, { items: [], counts: { all: 0, perm: 0, ask: 0, blocked: 0, review: 0 } }); document.querySelector('#main-agents').dataset.paSig = ''; agentRenderCluster()");
  check('有主 Agent、没有 worker：只有主头像，不留占位', !!doc.querySelector('#main-agents .pa-av') && !doc.querySelector('#main-agents .pa-ws') && !doc.querySelector('#main-agents .pa-badge'));
  check('主头像取名字首字，caption 显示地址名与空闲', doc.querySelector('.pa-av').textContent === 'M' && /mainAgent/.test(doc.querySelector('.pa-ag-cap').textContent) && /空闲/.test(doc.querySelector('.pa-ag-cap').textContent));
  ev(`state.sessions = ${J(sampleSessions())}`);
  ev(`pa.needs.items = ${J(sampleNeeds(now).items)}; pa.needs.counts = ${J(sampleNeeds(now).counts)}; document.querySelector('#main-agents').dataset.paSig = ''; agentRenderCluster()`);
  let wavs = [...doc.querySelectorAll('#main-agents .pa-wav')];
  check('在跑的 worker 出现在头像簇里，新来的带弹入动效', wavs.length === 1 && wavs[0].classList.contains('new'), String(wavs.length));
  check('worker 头像上的数字 = 它在等你的条目数', wavs[0].querySelector('.wn') && wavs[0].querySelector('.wn').textContent === '1');
  check('主头像角标 = 在等你的数量，有 live 权限门时是红色脉动', doc.querySelector('.pa-badge').textContent === '5' && doc.querySelector('.pa-badge').classList.contains('hot'));
  ev("document.querySelector('#main-agents').dataset.paSig = ''; agentRenderCluster()");
  check('再渲染一次不再重放弹入动效', !doc.querySelector('#main-agents .pa-wav').classList.contains('new'));
  ev(`state.sessions = state.sessions.concat(${J(Array.from({ length: 7 }, (_, i) => ({ id: 'x' + i, name: 'w·额外' + i, parent_id: 's-main', status: { running: true }, cwd: '/x' })))}); document.querySelector('#main-agents').dataset.paSig = ''; agentRenderCluster()`);
  check('worker 太多：多出来的收成 +N', !!doc.querySelector('#main-agents .pa-more') && /^\+\d+$/.test(doc.querySelector('#main-agents .pa-more').textContent));
  check('同时可见的头像不超过上限', doc.querySelectorAll('#main-agents .pa-wav').length <= 6);

  console.log('入口导航');
  ev('pa.view = "queue"; document.querySelector("#pa-nav").dataset.sig = ""; paRenderNav()');
  const items = [...doc.querySelectorAll('#pa-nav .pa-nav-item')];
  check('三个入口：需要你 / 主 Agent 会话 / 看板任务', J(items.map((b) => b.querySelector('.pa-nav-label').textContent)) === J(['需要你', '主 Agent 会话', '看板任务']));
  check('当前视图高亮', items[0].classList.contains('on') && !items[1].classList.contains('on'));
  check('需要你：红点 + 计数；看板：待人工 + 待验收的卡数', !!items[0].querySelector('.pa-live') && items[0].querySelector('.pa-cnt').textContent === '5' && items[2].querySelector('.pa-cnt').textContent === '3');
  ev('pa.unseen = 4; document.querySelector("#pa-nav").dataset.sig = ""; paRenderNav()');
  check('主会话入口显示新折叠回报数', doc.querySelectorAll('#pa-nav .pa-nav-item')[1].querySelector('.pa-cnt').textContent === '+4');

  console.log('右栏');
  ev("document.querySelector('#agent-aside').classList.remove('hidden'); pa.asideOpen = true; pa.ui.sel = ''; pa.ui.card = ''; agentRenderAside(true)");
  check('没有用户选择：默认就是最新「待人工」卡（不是空态）', ev('pa.ui.explicit') === false && /待人工/.test(doc.querySelector('.pa-ah').textContent) && /订单库分表迁移/.test(doc.querySelector('.pa-dt').textContent));
  check('常驻留言框始终在', !!doc.querySelector('#agent-aside #pa-cnote') && doc.querySelector('#pa-cnote').placeholder.includes('mainAgent'));
  ev("agentSelectItem('perm:p1')");
  check('权限门：卡片头 + 命令预览 + worker 说明', /进行中/.test(doc.querySelector('.pa-ah').textContent) && /git push/.test(doc.querySelector('.pa-cmd').textContent) && /覆盖远端分支/.test(doc.querySelector('.pa-say').textContent));
  check('权限门：拒绝 / 本会话始终允许 / 允许一次', J([...doc.querySelectorAll('.pa-af button')].map((b) => b.textContent)) === J(['拒绝', '本会话始终允许', '允许一次']));
  check('live 条目的底栏有倒计时', !!doc.querySelector('.pa-af [data-pa-cd]'));
  ev("agentSelectItem('ask:a1')");
  check('提问：渲染问题、选项和「其他」', /退款失败时怎么处理/.test(doc.querySelector('.perm-q-text').textContent) && doc.querySelectorAll('.pa-askf .perm-opt').length === 3);
  const submit = () => doc.querySelector('.pa-af [data-pa-need]');
  check('没选之前提交按钮是禁用的', submit().disabled === true);
  doc.querySelectorAll('.pa-askf .perm-opt')[0].click();
  check('选了选项 → 可提交，回传格式 {问题原文: 选项}', submit().disabled === false && J(ev('paAskAnswers(paItemById("ask:a1")).answers')) === J({ '退款失败时怎么处理？': '原路退回' }));
  doc.querySelectorAll('.pa-askf .perm-opt')[2].click();
  check('选「其他」要填内容才算答完', submit().disabled === true && !doc.querySelector('.perm-opt-custom').classList.contains('hidden'));
  ev("agentSelectItem('blocked:shard')");
  check('阻塞：展示卡点原因和 worker 的说明，决定为空时不能提交', /全量迁移超出变更窗口/.test(doc.querySelector('.pa-ab').textContent) && /全量迁移预计 3 小时 40 分/.test(doc.querySelector('.pa-say').textContent) && submit().disabled === true);
  ev("agentSelectItem('review:login')");
  check('同一张卡上有多个待办（提问 + 待验收）时出现切换 chip', doc.querySelectorAll('.pa-fchips .pa-fc').length === 2 && doc.querySelectorAll('.pa-fchips .pa-fc.on').length === 1);
  check('待验收：打回重做 / 通过验收', J([...doc.querySelectorAll('.pa-af button')].map((b) => b.textContent)) === J(['打回重做', '通过验收']));
  check('有待办时「等你处理」排在产物前面', ev("[...document.querySelectorAll('.pa-ab .pa-sec > h3')].map(h => h.textContent.replace(/\\d+$/, '')).join('|')").indexOf('等你处理') < ev("[...document.querySelectorAll('.pa-ab .pa-sec > h3')].map(h => h.textContent.replace(/\\d+$/, '')).join('|')").indexOf('产物'));
  ev("agentSelectItem('review:nobind')");
  check('条目指向的卡不在看板快照里：明确说找不到，而不是空白', /找不到这张卡/.test(doc.querySelector('.pa-ab').textContent));

  console.log('主 Agent 显示名（内联编辑）');
  const mk = (sessionsFix, apiExtra) => {
    const e = makeEnv({ api: Object.assign({ '/api/needs-you': () => sampleNeeds(Date.now()), '/api/agent-board': () => sampleBoard() }, apiExtra || {}) });
    e.ev("pa.mode = 'agent'; document.body.classList.add('pa-on')");
    const ss = sampleSessions().map((x) => (x.main_agent ? Object.assign({}, x, sessionsFix || { name: '小七' }) : x));
    e.ev(`state.sessions = ${J(ss)}; state.agentBoard = ${J(sampleBoard())}; pa.needs.items = ${J(sampleNeeds(now).items)}; pa.needs.counts = ${J(sampleNeeds(now).counts)}; pa.needs.loaded = true; agentRenderCluster()`);
    return e;
  };
  const tick = () => new Promise((r) => setTimeout(r, 5));
  const key = (el, k) => el.dispatchEvent(new (el.ownerDocument.defaultView.KeyboardEvent)('keydown', { key: k, bubbles: true }));
  let e3 = mk();
  const nm = () => e3.doc.querySelector('.pa-ag-cap .pa-name b').textContent;
  check('caption 显示会话的显示名（不是地址名 mainAgent），头像取显示名首字', nm() === '小七' && e3.doc.querySelector('.pa-av').textContent === '小' && /小七/.test(e3.doc.querySelector('.pa-av').title));
  check('留言框提示也用显示名；寻址仍是地址名', (e3.ev('agentRenderAside(true)'), /小七/.test(e3.doc.querySelector('#pa-cnote').placeholder)) && e3.ev('paMainAddr()') === 'mainAgent');
  e3.doc.querySelector('.pa-name').click();
  let inp = e3.doc.querySelector('.pa-name-in');
  check('点名字 → 变成输入框，预填当前名字', !!inp && inp.value === '小七');
  e3.ev('agentRenderCluster()');
  check('编辑中轮询重画不会把输入框销毁', e3.doc.querySelector('.pa-name-in') === inp);
  inp.value = '小八';
  key(inp, 'Enter');
  await tick();
  const patch = e3.w.__calls.filter((c) => c.method === 'PATCH' && c.path === '/api/sessions/s-main');
  check('Enter 提交：只 PATCH name（不碰 peer_name），落库后 caption 和头像首字更新', patch.length === 1 && J(patch[0].body) === J({ name: '小八' }) && nm() === '小八' && e3.doc.querySelector('.pa-av').textContent === '小');
  e3.doc.querySelector('.pa-name').click();
  inp = e3.doc.querySelector('.pa-name-in');
  inp.value = '不会保存';
  key(inp, 'Escape');
  await tick();
  check('Esc 取消：不发请求，名字不变', e3.w.__calls.filter((c) => c.method === 'PATCH').length === 1 && nm() === '小八' && !e3.doc.querySelector('.pa-name-in'));
  e3.doc.querySelector('.pa-name').click();
  inp = e3.doc.querySelector('.pa-name-in');
  inp.value = '   ';
  inp.dispatchEvent(new e3.w.Event('blur'));
  await tick();
  check('空名字 / 没改名：失焦不发请求', e3.w.__calls.filter((c) => c.method === 'PATCH').length === 1 && nm() === '小八');
  e3.doc.querySelector('.pa-name').click();
  inp = e3.doc.querySelector('.pa-name-in');
  inp.value = '失焦也保存';
  inp.dispatchEvent(new e3.w.Event('blur'));
  await tick();
  check('改了名字再失焦 = 提交', e3.w.__calls.filter((c) => c.method === 'PATCH').length === 2 && nm() === '失焦也保存');

  e3 = mk(null, { 'PATCH /api/sessions/s-main': () => { throw new Error('502 bad gateway'); } });
  e3.doc.querySelector('.pa-name').click();
  inp = e3.doc.querySelector('.pa-name-in');
  inp.value = '会失败';
  key(inp, 'Enter');
  await tick();
  check('保存失败：原名字保留，给出错误', nm() === '小七' && /重命名失败：502 bad gateway/.test(e3.doc.querySelector('.pa-ag-err').textContent));

  e3 = mk({ name: '小七', peer_name: '' });
  let asked = 0;
  e3.w.confirm = () => { asked++; return false; };
  e3.doc.querySelector('.pa-name').click();
  inp = e3.doc.querySelector('.pa-name-in');
  inp.value = '换个名';
  key(inp, 'Enter');
  await tick();
  check('没单独设置地址名时，改名会连带改 A2A 地址：先让用户确认，取消就不发请求', asked === 1 && e3.w.__calls.filter((c) => c.method === 'PATCH').length === 0 && nm() === '小七');
  e3.w.confirm = () => true;
  e3.doc.querySelector('.pa-name').click();
  inp = e3.doc.querySelector('.pa-name-in');
  inp.value = '换个名';
  key(inp, 'Enter');
  await tick();
  check('确认后照改', e3.w.__calls.filter((c) => c.method === 'PATCH').length === 1 && nm() === '换个名');

  console.log('看板只绑主 Agent');
  const e4 = mk({ name: 'Soar.AI' });
  e4.ev("document.querySelector('#agent-aside').classList.remove('hidden'); pa.asideOpen = true; pa.view = 'queue'; pa.ui.card = 'refund'; pa.ui.sel = ''; agentRenderAside(true)");
  const asideHidden = () => e4.doc.querySelector('#agent-aside').classList.contains('hidden');
  const handleHidden = () => e4.doc.querySelector('#pa-aside-handle').classList.contains('hidden');
  check('主 Agent 这一侧：右栏显示', !asideHidden());
  e4.ev("agentOnSessionOpened(state.sessions.find(x => x.id === 's-pay'))");
  check('打开 worker 会话：右栏整个收起、不留重开把手，也不跟着这个 worker 的卡走', asideHidden() && handleHidden() && e4.ev('pa.view') === 'session' && e4.ev('pa.ui.card') === 'refund');
  e4.doc.querySelector('#agent-aside .pa-ah').__stale = true;
  e4.ev('agentRenderAside(true)');
  check('worker 会话里 agentRenderAside 空转（隐藏时不偷偷重画）', e4.doc.querySelector('#agent-aside .pa-ah').__stale === true);
  e4.ev("agentOnSessionOpened(state.sessions.find(x => x.main_agent))");
  check('回到主 Agent 会话：右栏回来（按用户之前的开关偏好）', !asideHidden() && e4.ev('pa.view') === 'chat');
  e4.ev("agentOnSessionOpened(state.sessions.find(x => x.id === 's-login'))");
  e4.ev("agentOpenCard('shard')");
  check('在 worker 会话里点别的 worker 头像 / 卡片：回到队列这一侧再开卡', e4.ev('pa.view') === 'queue' && !asideHidden() && e4.ev('pa.ui.card') === 'shard');

  console.log('动态 = 看板抽屉的任务流');
  const e5 = mk({ name: 'Soar.AI' });
  e5.ev(`state.agentBoard.tasks.find(t => t.id === 'refund').timeline = ${J([
    { at: '2026-11-13 09:00:00', kind: '建卡', text: '任务创建' },
    { at: '2026-11-13 09:10:00', kind: '评论', text: '回调幂等键已加' },
    { at: '2026-11-13 09:20:00', kind: '用户消息', text: '先别动退款' },
    { at: '2026-11-13 09:21:00', kind: '主 Agent 回复', text: '收到，退款链路先暂停', data: { turn_id: 't1' } },
    { at: '2026-11-13 09:30:00', kind: '评论', text: '单测 18/18 通过' },
  ])}`);
  e5.ev("document.querySelector('#agent-aside').classList.remove('hidden'); pa.asideOpen = true; pa.view = 'queue'; agentOpenCard('refund'); pa.ui.tab = 'feed'; agentRenderAside(true)");
  const chips = () => [...e5.doc.querySelectorAll('#agent-aside .board-drawer-filters .board-filter-chip')].map((c) => c.textContent);
  check('筛选 chip 就是抽屉那套：全部 + 按事件类型（主 Agent 回复 / 用户消息 在前，其余按出现顺序）', J(chips()) === J(['全部 5', '主 Agent 回复 1', '用户消息 1', '建卡 1', '评论 2']), J(chips()));
  const row0 = e5.doc.querySelector('#agent-aside .board-drawer-timeline .board-drawer-event');
  check('行是抽屉的 board-drawer-event：头像 / 时间 / 类型 / 正文', e5.doc.querySelectorAll('#agent-aside .board-drawer-timeline .board-drawer-event').length === 5
    && !!row0.querySelector('.board-drawer-event-avatar') && row0.querySelector('.board-drawer-event-at').textContent === '2026-11-13 09:00:00'
    && row0.querySelector('.board-drawer-event-kind').textContent === '建卡' && /任务创建/.test(row0.querySelector('.board-drawer-event-text').textContent));
  check('顺序与抽屉一致（卡片时间线原序），tab 上的数 = 事件数', e5.doc.querySelectorAll('.board-drawer-event')[0].dataset.kind === '建卡' && /5/.test(e5.doc.querySelectorAll('.pa-ctabs button')[1].textContent));
  [...e5.doc.querySelectorAll('.board-filter-chip')].find((c) => c.textContent === '评论 2').click();
  check('点 chip 筛选：只剩这一类，chip 高亮', e5.doc.querySelectorAll('#agent-aside .board-drawer-event').length === 2 && e5.doc.querySelector('.board-filter-chip.active').textContent === '评论 2' && e5.ev('pa.ui.tf') === '评论');
  e5.ev("agentOpenCard('shard'); pa.ui.tab = 'feed'; agentRenderAside(true)");
  check('换一张卡，筛选回到「全部」（和抽屉一致）', e5.ev('pa.ui.tf') === 'all' && !!e5.doc.querySelector('.board-filter-chip.active') && /^全部/.test(e5.doc.querySelector('.board-filter-chip.active').textContent));
  e5.ev("agentOpenCard('refund'); pa.ui.tab = 'ov'; agentRenderAside(true)");
  check('概览里的「最近动态」是同一种行，只取最近 3 条', e5.doc.querySelectorAll('#agent-aside .pa-recent .board-drawer-event').length === 3);


  console.log('展开态的折叠把手');
  const e6 = mk({ name: 'Soar.AI' });
  e6.ev("pa.asideOpen = true; pa.view = 'queue'; document.querySelector('#agent-aside').classList.remove('hidden'); paSyncAside()");
  const h = e6.doc.querySelector('#pa-aside-handle');
  check('展开：把手可见、切到面板左缘（collapse）、图标朝右收起', !h.classList.contains('hidden') && h.classList.contains('collapse') && h.title === '收起右栏');
  check('可访问：aria-label=收起右栏、aria-expanded=true、指向右栏', h.getAttribute('aria-label') === '收起右栏' && h.getAttribute('aria-expanded') === 'true' && h.getAttribute('aria-controls') === 'agent-aside');
  check('是原生 button（键盘可达）', h.tagName === 'BUTTON' && h.type === 'button' && h.tabIndex === 0);
  check('角标 = 待办数', !!h.querySelector('b') && h.querySelector('b').textContent === '5');
  check('右上角 × 还在，两类关闭都可用', !!e6.doc.querySelector('#agent-aside .pa-xclose'));
  h.focus();
  check('键盘焦点能落在把手上', e6.doc.activeElement === h);
  h.click();
  check('点折叠把手 → 右栏收起（和 × 同一条关闭语义）', e6.doc.querySelector('#agent-aside').classList.contains('hidden') && e6.ev('pa.asideOpen') === false && e6.ev('pa.ui.explicit') === false);
  check('收起后把手回到右缘、变成展开、角标还在', !h.classList.contains('collapse') && !h.classList.contains('hidden') && h.getAttribute('aria-label') === '展开卡片详情' && h.querySelector('b').textContent === '5');
  check('收起后队列重新给右缘让位（pa-handle-on）', e6.doc.body.classList.contains('pa-handle-on'));
  e6.ev('pa.needs.counts.all = 2; paRenderHandle()');
  check('待办数变了，把手角标跟着刷新', h.querySelector('b').textContent === '2');
  h.click();
  check('再点展开 → 右栏回来、把手回到面板左缘、aria 变回收起', !e6.doc.querySelector('#agent-aside').classList.contains('hidden') && h.classList.contains('collapse') && h.getAttribute('aria-label') === '收起右栏');
  check('展开时不给右缘让位（pa-handle-on 撤掉）', !e6.doc.body.classList.contains('pa-handle-on'));
  e6.ev("agentOnSessionOpened(state.sessions.find(x => x.id === 's-pay'))");
  check('worker 会话收起右栏时，把手也不显示', h.classList.contains('hidden'));
  e6.ev("agentOnSessionOpened(state.sessions.find(x => x.main_agent))");
  Object.defineProperty(e6.w, 'innerWidth', { value: 390, configurable: true });
  e6.ev('paRenderHandle()');
  check('手机 sheet 不显示折叠 tab（靠右上角 × / 左上角返回）', h.classList.contains('hidden'));

  console.log(failed ? `\n${failed} 项失败` : '\n全部通过');
  process.exit(failed ? 1 : 0);
}
main().catch((e) => { console.error(e); process.exit(1); });
