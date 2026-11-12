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
  check('没选卡片：空态说明怎么选', /选一张卡片/.test(doc.querySelector('#agent-aside .pa-empty').textContent) && !doc.querySelector('#agent-aside .pa-af'));
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
  console.log(failed ? `\n${failed} 项失败` : '\n全部通过');
  process.exit(failed ? 1 : 0);
}
main().catch((e) => { console.error(e); process.exit(1); });
