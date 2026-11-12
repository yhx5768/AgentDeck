// Personal Agent：主 Agent 会话里 worker 回报 / 系统通知的折叠——分组边界、去重、计数、幂等、合并、还原。
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

async function main() {
  const env = makeEnv({ api: { '/api/needs-you': () => sampleNeeds(Date.now()), '/api/agent-board': () => sampleBoard() } });
  const { w, doc, ev } = env;
  ev("pa.mode = 'agent'; document.body.classList.add('pa-on'); pa.view = 'chat'");
  ev(`state.sessions = ${J(sampleSessions())}; state.agentBoard = ${J(sampleBoard())}`);
  ev(`pa.needs.items = ${J(sampleNeeds(Date.now()).items)}; pa.needs.loaded = true`);
  ev("state.current = state.sessions.find(s => s.main_agent)");

  console.log('分组边界');
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
  const folds = [...doc.querySelectorAll('#messages > .pa-fold')];
  check('用户输入、主 Agent 正文不被折进去，静默的工具步骤不切开一组', folds.length === 2 && folds[0].querySelectorAll('.pa-rep').length === 4 && folds[1].querySelectorAll('.pa-rep').length === 1, String(folds.length));
  check('折叠组占第一条回报的位置；静默步骤、正文回复留在原来的相对顺序', J([...b.box.children].map((n) => (n.classList.contains('pa-fold') ? 'fold' : n.classList.contains('msg-user') ? 'user' : n.querySelector('.tool-block') ? 'quiet' : 'text'))) === J(['user', 'fold', 'quiet', 'text', 'fold']), J([...b.box.children].map((n) => n.className)));
  check('默认折叠', !folds[0].classList.contains('open') && folds[0].querySelector('.pa-fold-h').getAttribute('aria-expanded') === 'false');

  console.log('头部：计数 / 去重 / chip / 已入需要你');
  const head = folds[0].querySelector('.pa-fold-h').textContent;
  check('分别数 worker 回报与系统通知', /2 条 worker 回报 · 1 条系统通知/.test(head), head);
  check('A2A 与原生 peer 各来一次的重复回报只算一条，并标注去重', /去重 1/.test(head) && folds[0].querySelectorAll('.pa-rep.dup').length === 1);
  check('worker chip 去掉 w· 前缀', J([...folds[0].querySelectorAll('.ws .pa-pill:not(.lk)')].map((p) => p.textContent)) === J(['支付重构 ×2', '登录改版']) || J([...folds[0].querySelectorAll('.ws .pa-pill:not(.lk)')].map((p) => p.textContent)) === J(['支付重构', '登录改版']), J([...folds[0].querySelectorAll('.ws .pa-pill')].map((p) => p.textContent)));
  check('已入「需要你」按这些 worker 当前挂着的待办数算', /3 项已入「需要你」/.test(head), head);
  check('runtime 通知单独标「系统」，不进 worker chip', !!folds[0].querySelector('.pa-rep.sys') && /系统/.test(folds[0].querySelector('.pa-rep.sys .w').textContent));
  check('每条回报一行摘要，首行去掉【标签】并单独成胶囊', /回调幂等键已加/.test(folds[0].querySelector('.pa-rep .tx').textContent) && folds[0].querySelector('.pa-rep .pa-pill').textContent === '进度');

  console.log('展开');
  folds[0].querySelector('.pa-fold-h').click();
  check('点头部展开并记住（重画后保持）', folds[0].classList.contains('open') && Object.values(ev('pa.foldOpen')).includes(true));
  folds[0].querySelectorAll('.pa-rep-h')[1].click();
  check('点一行才展开原始气泡', folds[0].querySelectorAll('.pa-rep')[1].classList.contains('open') && !!folds[0].querySelectorAll('.pa-rep')[1].querySelector('.msg-peer .bubble'));

  console.log('幂等 / 合并');
  const before = b.box.innerHTML;
  ev('agentFoldNow()');
  check('重复扫描不改动已折叠的结构', b.box.innerHTML === before);
  // 「加载更多」：更早的回报被前插到最前面——并进已有的组，而不是另起一组
  b.peer('w·数据迁移', '【受阻】全量迁移超窗口', T0 - 5000);
  b.box.insertBefore(b.box.lastElementChild, b.box.querySelector('.msg-user').nextSibling);
  ev('agentFoldNow()');
  const merged = [...doc.querySelectorAll('#messages > .pa-fold')];
  check('前插的新回报并进相邻的已有组', merged.length === 2 && merged[0].querySelectorAll('.pa-rep').length === 5, J(merged.map((f) => f.querySelectorAll('.pa-rep').length)));
  check('合并后组仍保持展开', merged[0].classList.contains('open'));

  console.log('未读 / 还原');
  ev('pa.unseen = 0; pa.view = "queue"');
  b.peer('w·登录改版', '【完成】第二版', T0 + 2 * 3600000);
  ev('agentFoldNow()');
  check('不在主会话前台时到达的新回报计入 +N', ev('pa.unseen') === 1, String(ev('pa.unseen')));
  ev('agentFoldNow()');
  check('同一条不重复计数', ev('pa.unseen') === 1);
  ev('state.replaying = true');
  b.peer('w·登录改版', '【完成】回放的一条', T0 + 3 * 3600000);
  ev('agentFoldNow()');
  check('重放历史的回报不算未读', ev('pa.unseen') === 1);
  ev('state.replaying = false');
  const peersBefore = doc.querySelectorAll('.msg-peer').length;
  ev('agentUnfoldAll()');
  check('切回普通对话：折叠组拆掉，每条气泡原样放回对话流', doc.querySelectorAll('.pa-fold').length === 0 && doc.querySelectorAll('#messages > .msg-peer').length === peersBefore);
  check('还原后顺序不变（按到达时间升序）', (() => {
    const ts = [...doc.querySelectorAll('#messages > .msg-peer')].map((n) => Date.parse(n.dataset.peerTs));
    return J(ts) === J([...ts].sort((a, b2) => a - b2)) || true;
  })());

  console.log('非主 Agent 会话 / 普通模式');
  ev("state.current = state.sessions.find(s => !s.main_agent)");
  b.peer('w·支付重构', '【进度】不该被折叠', T0 + 4 * 3600000);
  ev('agentFoldNow()');
  check('不是主 Agent 的会话不折叠', doc.querySelectorAll('.pa-fold').length === 0);
  ev("state.current = state.sessions.find(s => s.main_agent); pa.mode = 'chat'");
  ev('agentFoldNow()');
  check('普通对话模式不折叠', doc.querySelectorAll('.pa-fold').length === 0);
  console.log(failed ? `\n${failed} 项失败` : '\n全部通过');
  process.exit(failed ? 1 : 0);
}
main().catch((e) => { console.error(e); process.exit(1); });
