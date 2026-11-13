// Personal Agent 右栏：worker 的回报用 md() 渲染、长文默认折叠、短文直出、权限说明不折。
const { makeEnv, sampleNeeds, sampleSessions, sampleBoard } = require('./pa_harness');

let failed = 0;
function check(name, cond, extra) {
  if (cond) console.log('  ok  ' + name);
  else { failed++; console.log('  FAIL ' + name + (extra ? '  →  ' + extra : '')); }
}
const J = (x) => JSON.stringify(x);

const LONG = [
  '**登录页改版 · 验收前回报**',
  '',
  '- [x] 短信验证码主链路：`send-code` / `verify-code` 已联调',
  '- [x] v2 原型与验收清单已上传',
  '- [ ] 深色模式下风控提示条对比度 4.1:1（目标 4.5:1），已定位到 `--risk-fg`',
  '- [ ] 风控文案等你拍板（见「需要你」）',
  '',
  '参考：[验收清单](https://example.com/login-checklist)',
  '',
  '另外补一条：验证码短信模板 B 的文案已按风控要求改成中性描述，灰度 10% 观察；如果今天的回执里没有异常，我会把灰度提到 50% 再回报。',
  '验收前还需要你点一遍「通过验收」或「打回重做」。',
  '<img src=x onerror="alert(1)">',
].join('\n');
const SHORT = '短回报：规则去重完成，312 → 241，分级清单已上传。';

function main() {
  const env = makeEnv({ api: { '/api/needs-you': () => sampleNeeds(Date.now()), '/api/agent-board': () => sampleBoard() } });
  const { w, doc, ev } = env;
  const bd = sampleBoard();
  bd.tasks.find((t) => t.id === 'login').timeline = [
    { at: '2026-11-13 10:00:00', kind: '评论', text: LONG },
    { at: '2026-11-13 09:00:00', kind: '评论', text: '旧的短回报（不应再被选中）' },
  ];
  bd.tasks.push({
    id: 'docs', title: '文档同步', full_title: '文档同步', status: '待验收', session: 's-login', worker: 's-login',
    updated_at: '2026-11-13 10:05:00', project: 'P', artifacts: [],
    timeline: [{ at: '2026-11-13 10:05:00', kind: '评论', text: SHORT }],
  });
  ev(`state.sessions = ${J(sampleSessions())}; state.agentBoard = ${J(bd)}; pa.needs.items = ${J(sampleNeeds(Date.now()).items)}; pa.needs.counts = ${J(sampleNeeds(Date.now()).counts)}; pa.needs.loaded = true`);
  ev("pa.mode = 'agent'; document.body.classList.add('pa-on'); pa.view = 'queue'; pa.asideOpen = true; pa.ui.status = 'pending'; pa.ui.kind = 'all'");
  ev("document.querySelector('#agent-aside').classList.remove('hidden'); document.querySelector('#needs-view').classList.remove('hidden')");

  console.log('长回报：md 渲染 + 默认折叠');
  w.__mdCalls.length = 0;
  ev("agentSelectItem('review:login')");
  const sayW = doc.querySelector('#agent-aside .pa-say-w');
  const block = sayW && sayW.querySelector('.pa-say');
  check('标签仍是「worker 的回报」，引用条还在', !!sayW && /worker 的回报/.test(sayW.querySelector('h4').textContent) && !!block && block.classList.contains('pa-say') && block.classList.contains('md'));
  check('用最新一条回报（不是更早的那条）', /登录页改版 · 验收前回报/.test(block.textContent) && /灰度提到 50%/.test(block.textContent) && !/旧的短回报/.test(block.textContent));
  check('walk md()：粗体 / 行内 code / 链接 / 列表 / 换行都渲染出来', !!block.querySelector('strong') && !!block.querySelector('code') && !!block.querySelector('a') && block.querySelectorAll('li').length >= 3 && !!block.querySelector('br'));
  check('md 调用带 {artifacts:false}（不是自己拼 HTML）', w.__mdCalls.some((c) => /登录页改版 · 验收前回报/.test(c.text) && c.opts && c.opts.artifacts === false));
  check('原始 HTML 没有直接进 DOM（测试桩按 md 管线转义）', !block.querySelector('img') && !block.querySelector('script'));
  check('长回报默认折起：有 clip、没有 open', block.classList.contains('clip') && !block.classList.contains('open'));
  const more = sayW.querySelector('.pa-say-more');
  check('给出「展开全文」按钮', !!more && more.textContent === '展开全文' && more.getAttribute('aria-expanded') === 'false');
  more.click();
  check('展开后去掉 clip、按钮变「收起」', block.classList.contains('open') && !block.classList.contains('clip') && more.textContent === '收起' && more.getAttribute('aria-expanded') === 'true');
  more.click();
  check('再点收回', block.classList.contains('clip') && !block.classList.contains('open') && more.textContent === '展开全文');
  check('操作按钮仍在右栏底栏（不被正文挤走）', !!doc.querySelector('#agent-aside .pa-af button'));

  console.log('短回报：直出');
  ev("agentSelectItem('review:nobind')");
  const shortW = doc.querySelector('#agent-aside .pa-say-w');
  const shortBlock = shortW && shortW.querySelector('.pa-say');
  check('短报告也走 md，但不折叠、没有展开按钮', !!shortBlock && shortBlock.classList.contains('md') && !shortBlock.classList.contains('clip') && !shortW.querySelector('.pa-say-more') && /312 → 241/.test(shortBlock.textContent));

  console.log('权限说明不折（保留完整上下文）');
  ev(`pa.needs.items.find(i => i.id === 'perm:p1').input.description = ${J(LONG)}`);
  ev("agentSelectItem('perm:p1')");
  const permSay = [...doc.querySelectorAll('#agent-aside .pa-say-w')].find((x) => /worker 的说明/.test(x.textContent));
  check('长说明照常 md 渲染，但不默认折叠', !!permSay && !!permSay.querySelector('.pa-say.md') && !permSay.querySelector('.pa-say.clip') && !permSay.querySelector('.pa-say-more'));
  console.log(failed ? `\n${failed} 项失败` : '\n全部通过');
  process.exit(failed ? 1 : 0);
}
try { main(); } catch (e) { console.error(e); process.exit(1); }
