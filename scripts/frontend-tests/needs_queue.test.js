// Personal Agent：模式开关 / 队列数据与分组 / 队列视图 / 头像簇 / 入口导航。
// 局限：jsdom 不做布局——头像叠放的像素、动效手感验不了；这里钉 DOM 契约和规则。
const { makeEnv, sampleNeeds, sampleSessions, sampleBoard } = require('./pa_harness');
const fs = require('fs');
const path = require('path');
const WEB = path.resolve(__dirname, '../../web');
const webRead = (f) => fs.readFileSync(path.join(WEB, f), 'utf8');

let failed = 0;
function check(name, cond, extra) {
  if (cond) console.log('  ok  ' + name);
  else { failed++; console.log('  FAIL ' + name + (extra ? '  →  ' + extra : '')); }
}
const J = (x) => JSON.stringify(x);

async function main() {
  const now = Date.now();
  const env = makeEnv({ api: { '/api/needs-you': () => sampleNeeds(Date.now() + 2000) } });
  const { w, doc, ev } = env;
  ev(`state.sessions = ${J(sampleSessions())}; state.agentBoard = ${J(sampleBoard())};`);

  console.log('模式开关');
  w.__ccDefaultUiMode = 'agent';
  ev('paLoadPrefs()');
  check('localStorage 为空时使用服务端默认 Personal Agent', ev('agentModeOn()') === true
    && ev('pa.mode') === 'agent' && w.localStorage.getItem('cc-webui:mode') == null);
  w.__ccDefaultUiMode = 'chat';
  ev('paLoadPrefs()');
  check('服务端默认 chat 时回到普通对话', ev('agentModeOn()') === false && ev('pa.mode') === 'chat');
  w.localStorage.setItem('cc-webui:mode', 'agent');
  w.__ccDefaultUiMode = 'chat';
  ev('paLoadPrefs()');
  check('localStorage 已有 agent 时优先于服务端默认', ev('agentModeOn()') === true);
  w.localStorage.setItem('cc-webui:mode', 'chat');
  w.__ccDefaultUiMode = 'agent';
  ev('paLoadPrefs()');
  check('localStorage 已有 chat 时优先于服务端默认', ev('agentModeOn()') === false);
  w.localStorage.removeItem('cc-webui:mode');
  w.__ccDefaultUiMode = 'agent';
  ev('paLoadPrefs()');
  ev("agentSetMode('agent')");
  check('切到 Personal Agent：持久化 + body 样式 + 入口显示',
    w.localStorage.getItem('cc-webui:mode') === 'agent' && doc.body.classList.contains('pa-on') && !doc.querySelector('#pa-nav').classList.contains('hidden'));
  check('设置页分段按钮同步', doc.querySelector('[data-pa-mode="agent"]').classList.contains('on') && !doc.querySelector('[data-pa-mode="chat"]').classList.contains('on'));
  ev("agentSetMode('chat')");
  check('切回普通对话：样式撤掉、入口隐藏、右栏隐藏、队列视图隐藏',
    !doc.body.classList.contains('pa-on') && doc.querySelector('#pa-nav').classList.contains('hidden')
    && doc.querySelector('#agent-aside').classList.contains('hidden') && doc.querySelector('#needs-view').classList.contains('hidden'));
  check('切回时让原版头像条重画', w.__origMA >= 1);
  ev("agentSetMode('nonsense')");
  check('非法值回落普通对话', ev('pa.mode') === 'chat' && w.localStorage.getItem('cc-webui:mode') === 'chat');

  console.log('队列数据');
  ev("pa.mode = 'agent'");
  await ev('agentRefreshNeeds()');
  check('items / counts 落进状态', ev('pa.needs.items.length') === 5 && ev('pa.needs.counts.all') === 5 && ev('pa.needs.loaded') === true);
  check('server_time 对出时钟差（本机比服务端慢约 2s）', Math.abs(ev('pa.needs.skew') - 2000) < 800, String(ev('pa.needs.skew')));
  check('window_ms 取服务端给的', ev('pa.needs.windowMs') === 55000);
  w.__api['/api/needs-you'] = () => { throw new Error('网络抖动'); };
  await ev('agentRefreshNeeds()');
  check('取失败保留旧数据并记错误（倒计时中的卡不凭空消失）', ev('pa.needs.items.length') === 5 && ev('pa.needs.err') === '网络抖动');
  w.__api['/api/needs-you'] = () => sampleNeeds(Date.now());
  await ev('agentRefreshNeeds()');
  check('恢复后清掉错误', ev('pa.needs.err') === '');
  // 单飞：在途时再来的刷新合并成「回头再拉一次」
  const before = w.__calls.filter((c) => c.path === '/api/needs-you').length;
  await Promise.all([ev('agentRefreshNeeds()'), ev('agentRefreshNeeds()'), ev('agentRefreshNeeds()')]);
  await new Promise((r) => setTimeout(r, 30));
  const issued = w.__calls.filter((c) => c.path === '/api/needs-you').length - before;
  check('并发刷新不叠请求（3 次调用 ≤ 2 个请求）', issued >= 1 && issued <= 2, String(issued));

  console.log('分组 / 筛选');
  const keys = (gs) => gs.map((g) => g.key + ':' + g.items.map((i) => i.id).join('+'));
  ev("pa.ui.status = 'pending'; pa.ui.kind = 'all'; pa.ui.group = 'tier'");
  let gs = ev('paQueueGroups()');
  check('按紧急度：perm > ask > blocked > review', J(gs.map((g) => g.key)) === J(['perm', 'ask', 'blocked', 'review']), J(keys(gs)));
  check('档内保持服务端顺序', J(gs[3].items.map((i) => i.id)) === J(['review:login', 'review:nobind']));
  ev("pa.ui.kind = 'review'");
  check('按类型筛选只留该类', J(ev('paQueueGroups()').map((g) => g.key)) === J(['review']));
  ev("pa.ui.kind = 'all'; pa.ui.group = 'task'");
  gs = ev('paQueueGroups()');
  check('按任务：任务按最急条目先后出现，没绑卡的归 _none', gs.length >= 4 && gs[0].key === 'task:refund', J(keys(gs)));
  check('按任务：一张卡上的多个条目聚在一起', gs.find((g) => g.key === 'task:login').items.length === 2);
  ev("pa.done = [{ id: 'd1', kind: 'ask', title: '旧提问', label: '已回答', at: Date.now(), steps: [] }]");
  ev("pa.ui.group = 'tier'; pa.ui.status = 'done'");
  check('已处理 tab 只有本机记录', J(ev('paQueueGroups()').map((g) => g.key)) === J(['done']));
  ev("pa.ui.status = 'all'");
  check('全部 tab = 待处理 + 已处理', J(ev('paQueueGroups()').map((g) => g.key)) === J(['perm', 'ask', 'blocked', 'review', 'done']));
  ev("pa.ui.status = 'pending'; pa.ui.pin = 'done:d1'");
  check('刚处理的条目（pin）在待处理视图顶上留一行', ev('paQueueGroups()')[0].key === 'pinned');
  ev("pa.ui.pin = ''");

  console.log('队列视图渲染');
  ev("pa.view = 'queue'; pa.ui.sel = ''; pa.ui.card = ''; document.querySelector('#needs-view').classList.remove('hidden')");
  ev('agentRenderQueue(true)');
  const rows = [...doc.querySelectorAll('#needs-view .pa-row')];
  check('每个待办一行，id 在 data-pa-id 上', rows.length === 5 && rows[0].dataset.paId === 'perm:p1');
  check('没有选中时自动选第一项并带出它的卡', ev('pa.ui.sel') === 'perm:p1' && ev('pa.ui.card') === 'refund');
  check('选中行带 sel 样式', rows[0].classList.contains('sel'));
  check('live 条目有倒计时和进度条', !!rows[0].querySelector('[data-pa-cd]') && !!rows[0].querySelector('[data-pa-bar]'));
  check('看板条目没有倒计时，blocked 显示已等多久', !rows[2].querySelector('[data-pa-cd]') && /已等/.test(rows[2].querySelector('[data-pa-wait]').textContent));
  check('1 小时前进入待人工 → 带「已提档」', /已提档/.test(rows[2].textContent));
  check('头部给出待处理数和最久等待', /5 项待处理/.test(doc.querySelector('.pa-qh .sub').textContent), doc.querySelector('.pa-qh .sub').textContent);
  check('三个 tab 的计数', J([...doc.querySelectorAll('.pa-tabs .pa-tab em')].map((e) => e.textContent)) === J(['5', '1', '6']));
  rows[3].click();
  check('点行 → 选中并联动右栏的卡', ev('pa.ui.sel') === 'review:login' && ev('pa.ui.card') === 'login');
  w.document.dispatchEvent(new w.KeyboardEvent('keydown', { key: 'j', bubbles: true }));
  check('j 往下移一项', ev('pa.ui.sel') === 'review:nobind', ev('pa.ui.sel'));
  // 选中项被别处处理掉了：挪到当前第一项
  ev("pa.needs.items = pa.needs.items.filter(i => i.id !== 'review:nobind'); agentRenderQueue(true)");
  check('选中项消失 → 回到第一项', ev('pa.ui.sel') === 'perm:p1');

  console.log('空态 / 错误态');
  ev("pa.needs.items = []; pa.needs.counts = {all:0,perm:0,ask:0,blocked:0,review:0}; pa.ui.sel = ''; pa.ui.card = ''; agentRenderQueue(true)");
  check('队列空：明确的空态文案', /没有待处理的事/.test(doc.querySelector('#needs-view .pa-empty').textContent));
  ev("pa.ui.status = 'done'; pa.done = []; agentRenderQueue(true)");
  check('已处理为空：说明只存本机', /只存本机/.test(doc.querySelector('#needs-view .pa-empty').textContent));
  ev("pa.ui.status = 'pending'; pa.needs.err = '502 bad gateway'; agentRenderQueue(true)");
  check('读取失败：错误条 + 重试', /队列读取失败：502 bad gateway/.test(doc.querySelector('#needs-view .pa-note.err').textContent) && !!doc.querySelector('#needs-view .pa-note.err button'));
  ev("pa.needs.err = ''; pa.needs.board = { ok: false, error: '看板数据库不可用' }; agentRenderQueue(true)");
  check('看板不可用：明确提示只剩权限门和提问', /看板不可用：看板数据库不可用/.test(doc.querySelector('#needs-view .pa-note.warn').textContent));
  ev("pa.needs.loaded = false; agentRenderQueue(true)");
  check('首次加载中：不是空态', /正在读取/.test(doc.querySelector('#needs-view .pa-empty').textContent));

  console.log('横幅 / 新来的高亮');
  // 干净的一局：队列视图开着、停在顶部；服务端先给 1 个提问，再多出一个权限门
  const e2 = makeEnv({ api: { '/api/needs-you': () => ({ items: [], counts: { all: 0, perm: 0, ask: 0, blocked: 0, review: 0 }, server_time: Date.now(), window_ms: 55000, board: { ok: true } }) } });
  const ev2 = e2.ev, d2 = e2.doc, w2 = e2.w;
  ev2("pa.mode = 'agent'; document.body.classList.add('pa-on'); pa.view = 'queue'");
  ev2(`state.sessions = ${J(sampleSessions())}; state.agentBoard = ${J(sampleBoard())}`);
  ev2("document.querySelector('#needs-view').classList.remove('hidden')");
  const all = sampleNeeds(Date.now());
  const ask = all.items.find((i) => i.kind === 'ask');
  const perm = all.items.find((i) => i.kind === 'perm');
  const serve = (items) => { w2.__api['/api/needs-you'] = () => Object.assign({}, all, { server_time: Date.now(), items, counts: { all: items.length, perm: items.filter((i) => i.kind === 'perm').length, ask: items.filter((i) => i.kind === 'ask').length, blocked: 0, review: 0 } }); };
  serve([ask]);
  await ev2('agentRefreshNeeds()');
  check('首次加载：已有的条目不闪', !ev2('Object.keys(pa.freshAt).length') && !d2.querySelector('.pa-row.fresh'));
  const live = (it) => Object.assign({}, it, { created_at: Date.now(), expires_at: Date.now() + 55000 });
  serve([live(perm), live(ask)]);
  await ev2('agentRefreshNeeds()');
  check('队列开着且在顶部：新来的权限门不再弹横幅盖住队列头', w2.__toasts.length === 0, J(w2.__toasts.map((t) => t.id)));
  check('新来的那一行闪一下，原有的不闪', d2.querySelectorAll('.pa-row.fresh').length === 1 && d2.querySelector('.pa-row.fresh').dataset.paId === 'perm:p1');
  check('提问同样被吸收，轮询里重复调用也是同一个结论', ev2("agentAbsorbBanner('ask:a1')") === true && ev2("agentAbsorbBanner('ask:a1')") === true);
  ev2("pa.view = 'chat'");
  check('换到别的视图：新的 key 照常弹横幅', ev2("agentAbsorbBanner('ask:zz')") === false);
  ev2("pa.view = 'queue'; pa.ui.status = 'done'");
  check('看的是「已处理」页：不算队列在眼前', ev2("agentAbsorbBanner('ask:zz')") === false);
  ev2("pa.ui.status = 'pending'");
  ev2("Object.defineProperty(document.querySelector('.pa-qb'), 'scrollTop', { value: 400, configurable: true })");
  check('队列已经滚离顶部：新的 key 照常弹横幅', ev2("agentAbsorbBanner('ask:zz')") === false);
  ev2("pa.view = 'board'; pa.ui.sel = ''");
  const p2 = Object.assign({}, live(perm), { id: 'perm:p2', pending_id: 'p2', title: 'Bash：rm -rf build' });
  serve([live(perm), p2]);
  await ev2('agentRefreshNeeds()');
  check('不在队列视图：权限门照常弹横幅，且只弹一次', w2.__toasts.filter((t) => t.id === 'pa-perm:p2').length === 1 && (ev2('paRaisePermBanners()'), w2.__toasts.filter((t) => t.id === 'pa-perm:p2').length === 1));

  console.log('首屏默认模式契约');
  const indexHtml = webRead('index.html');
  const styleCss = webRead('style.css');
  const appSrc = webRead('app.js');
  const agentSrc = webRead('agent.js');
  check('无本机偏好时首屏等 /api/config，并先隐藏外壳避免闪普通页',
    indexHtml.includes('pa-mode-pending') && styleCss.includes('html.pa-mode-pending body')
    && appSrc.includes('__ccConfigReady') && appSrc.includes('default_ui_mode')
    && agentSrc.includes('__ccConfigReady') && agentSrc.includes('paStoredMode'));

  console.log(failed ? `\n${failed} 项失败` : '\n全部通过');
  process.exit(failed ? 1 : 0);
}
main().catch((e) => { console.error(e); process.exit(1); });
