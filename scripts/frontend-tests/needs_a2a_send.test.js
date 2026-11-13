// Personal Agent：出站 A2A 发件卡一律折进发送组（含单条）/ 组头信息 / 手动开合持久 / 混合步骤拆分 / 不被回报组吞掉。
const { makeEnv, sampleNeeds, sampleSessions, sampleBoard } = require('./pa_harness');

let failed = 0;
function check(name, cond, extra) {
  if (cond) console.log('  ok  ' + name);
  else { failed++; console.log('  FAIL ' + name + (extra ? '  →  ' + extra : '')); }
}
const J = (x) => JSON.stringify(x);
const T0 = Date.parse('2026-11-13T10:00:00');
const iso = (ms) => new Date(ms).toISOString();
const esc = (t) => String(t).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/"/g, '&quot;');

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
  const text = (t, clock) => {
    const m = msg('msg-assistant', '<div class="role-tag">agent<span class="msg-time">2026-11-13 ' + clock + '</span></div><div class="bubble"><p></p></div>');
    m.querySelector('p').append(t);
    return m;
  };
  const peer = (from, t, ms) => {
    const m = msg('msg-peer', '<div class="role-tag">⇢ 来自 ' + from + '</div><div class="bubble"></div>', { peerFrom: from, peerTs: iso(ms), peerVia: 'a2a' });
    m.querySelector('.bubble').append(t);
    return m;
  };
  const sendHtml = (to, body, id, bad) => '<details class="tool-block tool-peer-send' + (bad ? ' peer-send-failed' : '') + '" open data-peer-send-to="' + esc(to) + '"' + (id ? ' data-peer-send-id="' + esc(id) + '"' : '') + '>'
    + '<summary><span class="tool-name">mcp__agentdeck__send_message</span><span class="tool-summary-text">⇢ ' + esc(to) + ' · ' + esc(body.slice(0, 30)) + '</span></summary>'
    + '<div class="tool-body peer-send-body"><div class="peer-send-head"><span class="peer-send-arrow">⇢</span><span class="peer-send-to">' + esc(to) + '</span></div>'
    + '<div class="peer-send-msg">' + esc(body) + '</div></div></details>';
  const asst = (inner, clock) => msg('msg-assistant', '<div class="role-tag">agent<span class="msg-time">2026-11-13 ' + clock + '</span></div><div class="bubble">' + inner + '</div>');
  const bash = (clock) => '<details class="tool-block"><summary><span class="tool-name">Bash</span><span class="tool-summary-text">git status</span></summary><div class="tool-body">git status</div></details>';
  return { box, user, text, peer, sendHtml, asst, bash };
}

async function main() {
  const env = makeEnv({ api: { '/api/needs-you': () => sampleNeeds(Date.now()), '/api/agent-board': () => sampleBoard() } });
  const { w, doc, ev } = env;
  ev("pa.mode = 'agent'; document.body.classList.add('pa-on'); pa.view = 'chat'");
  ev(`state.sessions = ${J(sampleSessions())}; state.agentBoard = ${J(sampleBoard())}`);
  ev(`pa.needs.items = ${J(sampleNeeds(Date.now()).items)}; pa.needs.loaded = true`);
  ev("state.current = state.sessions.find(s => s.main_agent)");
  const b = build(doc);
  b.user('开始');
  b.peer('w·支付重构', '【进度】回调幂等键已加', T0 + 1000);
  // 单条发送：也折进独立的 A2A 发送组，默认展开一层
  const solo = b.asst(b.sendHtml('w·支付重构', '【派工】跟进退款卡剩余事项', 'solo-refund'), '10:00:01');
  b.text('已安排支付重构跟进。', '10:00:02');
  // 两条连续 send-only：并进同一个 A2A 发送组（第二条投递失败）
  const s1 = b.asst(b.sendHtml('w·支付重构', '【派工】退款失败口径确认', 'dispatch-refund'), '10:05:01');
  const s2 = b.asst(b.sendHtml('w·登录改版', '【派工】风控文案继续推进', 'dispatch-login', true), '10:05:02');
  b.text('已安排 worker。', '10:05:03');
  // 一条消息里两张发件卡：仍然算 2 条发送，一个组、一行
  const two = b.asst(b.sendHtml('w·数据迁移', '【催办】演练方案进度', 'nudge-mig') + b.sendHtml('w·监控告警', '【催办】分级清单进度', 'nudge-alert'), '10:08:00');
  b.text('已经催过了。', '10:08:01');
  // 混合步骤：正文 + 发件卡 + 其它工具（正文和发件卡留在原处，只折其它工具）
  const mixed = b.asst('<p>顺手记一下。</p>' + b.sendHtml('w·登录改版', '【同步】灰度先按现在的来', 'sync-login') + b.bash(), '10:10:00');
  b.peer('w·登录改版', '【完成】已收到同步', T0 + 20 * 60000);
  ev('agentFoldNow()');

  const a2a = () => [...doc.querySelectorAll('#messages > .pa-fold.a2a')];
  const rep = () => [...doc.querySelectorAll('#messages > .pa-fold:not(.silent):not(.a2a)')];
  const silent = () => [...doc.querySelectorAll('#messages > .pa-fold.silent')];
  check('单条 A2A 发送也折进独立组，默认展开，组头 + 一行摘要可见', a2a().length === 3
    && solo.closest('.pa-fold') === a2a()[0] && solo.parentElement !== b.box
    && a2a()[0].classList.contains('open')
    && a2a()[0].querySelector('.pa-fold-h').getAttribute('aria-expanded') === 'true');
  const soloHead = a2a()[0].querySelector('.pa-fold-h').textContent.replace(/\s+/g, ' ');
  check('单条组头部：1 条 A2A 发送 + 收件人 chip + 任务 id + 预览', /^1 条 A2A 发送/.test(soloHead)
    && /支付重构/.test(soloHead) && /solo-refund/.test(soloHead) && /跟进退款卡剩余事项/.test(soloHead)
    && /已展开/.test(soloHead), soloHead);
  check('单条组默认展开时原始发件卡在组内，但摘要行仍收起', a2a()[0].querySelectorAll('.pa-rep.a2a-send').length === 1
    && /solo-refund/.test(a2a()[0].querySelector('.pa-rep.a2a-send').textContent)
    && !a2a()[0].querySelector('.pa-rep.a2a-send').classList.contains('open')
    && !!a2a()[0].querySelector('.pa-rep-b > .msg-assistant .tool-peer-send'));
  a2a()[0].querySelector('.pa-fold-h').click();
  const soloKey = a2a()[0].dataset.key;
  check('手动折叠单条组：状态记进 pa.foldOpen', !a2a()[0].classList.contains('open')
    && ev(`pa.foldOpen[${J(soloKey)}]`) === false
    && /已折叠/.test(a2a()[0].querySelector('.pa-fold-h').textContent));
  ev('agentFoldNow()');
  check('重复扫描保持手动折叠', !a2a()[0].classList.contains('open') && ev(`pa.foldOpen[${J(soloKey)}]`) === false);
  check('两条连续发送折成独立的「2 条 A2A 发送」组，默认展开', a2a().length === 3 && a2a()[1].classList.contains('open'));
  const h = a2a()[1].querySelector('.pa-fold-h').textContent.replace(/\s+/g, ' ');
  check('组头部：N 条 A2A 发送 + 收件人 chip + 任务 id + 预览 + 已展开', /^2 条 A2A 发送/.test(h)
    && /支付重构/.test(h) && /登录改版/.test(h) && /dispatch-refund/.test(h) && /退款失败口径确认/.test(h)
    && /已展开/.test(h), h);
  check('组头标出投递失败', /失败 ×1/.test(h), h);
  check('组里每条一行：收件人 / id / 预览，点开才看原始发件卡', a2a()[1].querySelectorAll('.pa-rep.a2a-send').length === 2
    && /dispatch-login/.test(a2a()[1].querySelectorAll('.pa-rep.a2a-send')[1].textContent)
    && /风控文案继续推进/.test(a2a()[1].querySelectorAll('.pa-rep.a2a-send')[1].textContent)
    && [...a2a()[1].querySelectorAll('.pa-rep.a2a-send')].every((r) => !r.classList.contains('open'))
    && a2a()[1].querySelectorAll('.pa-rep-b > .msg-assistant .tool-peer-send').length === 2);
  a2a()[1].querySelectorAll('.pa-rep-h')[1].click();
  check('点一行展开发件卡', a2a()[1].querySelectorAll('.pa-rep')[1].classList.contains('open') && !!a2a()[1].querySelectorAll('.pa-rep')[1].querySelector('.peer-send-to'));
  const h2 = a2a()[2].querySelector('.pa-fold-h').textContent.replace(/\s+/g, ' ');
  check('一条消息里两张发件卡也算 2 条，成一个默认展开的组、一行', a2a()[2].classList.contains('open')
    && a2a()[2].querySelectorAll('.pa-rep.a2a-send').length === 1 && /^2 条 A2A 发送/.test(h2) && two.closest('.pa-fold') === a2a()[2], h2);
  check('混合步骤：正文和发件卡留在消息里，其它工具折到下面', mixed.parentElement === b.box && !!mixed.querySelector('.tool-peer-send')
    && !mixed.querySelector('.tool-block:not(.tool-peer-send)') && mixed.nextElementSibling === silent()[0]
    && silent()[0].querySelectorAll('.pa-rep.step').length === 1 && !silent()[0].querySelector('.tool-peer-send'));
  check('回报组默认展开、静默组默认折叠，与 A2A 发送组互不混合', rep().length === 2 && rep().every((f) => f.classList.contains('open') && f.querySelectorAll('.pa-rep').length === 1 && !f.querySelector('.tool-peer-send'))
    && silent().length === 1 && !silent()[0].classList.contains('open'));
  const snapshot = b.box.innerHTML;
  ev('agentFoldNow()');
  check('重复扫描幂等', b.box.innerHTML === snapshot);
  // load more：在已手动折叠的单条组前插一条更早的发送，合并后必须保持折叠
  const early = b.asst(b.sendHtml('w·对账脚本', '【派工】对账脚本继续重跑', 'early-recon'), '09:59:00');
  const target = a2a()[0];
  target.before(early);
  ev('agentFoldNow()');
  const after = a2a()[0];
  check('前插更早的发送并进手动折叠的组，计数增加但保持折叠', a2a().length === 3
    && after.querySelectorAll('.pa-rep.a2a-send').length === 2
    && /^2 条 A2A 发送/.test(after.querySelector('.pa-fold-h').textContent.replace(/\s+/g, ' '))
    && !after.classList.contains('open') && ev(`pa.foldOpen[${J(after.dataset.key)}]`) === false);
  // 手动展开后，实时新来的发送并进同一组也不能覆盖展开状态
  after.querySelector('.pa-fold-h').click();
  const expandedKey = after.dataset.key;
  check('手动展开合并后的组，状态记进 pa.foldOpen', after.classList.contains('open')
    && /已展开/.test(after.querySelector('.pa-fold-h').textContent)
    && ev(`pa.foldOpen[${J(expandedKey)}]`) === true);
  const live = b.asst(b.sendHtml('w·对账脚本', '【催办】对账脚本最新进度', 'live-recon'), '10:25:00');
  after.after(live);
  ev('agentFoldNow()');
  const afterLive = a2a()[0];
  check('实时新发送并进手动展开的组，计数增加且保持展开', a2a().length === 3
    && afterLive.querySelectorAll('.pa-rep.a2a-send').length === 3
    && /^3 条 A2A 发送/.test(afterLive.querySelector('.pa-fold-h').textContent.replace(/\s+/g, ' '))
    && afterLive.classList.contains('open') && ev(`pa.foldOpen[${J(afterLive.dataset.key)}]`) === true);
  const openSnap = b.box.innerHTML;
  ev('agentFoldNow()');
  check('重复扫描保持手动展开', b.box.innerHTML === openSnap && a2a()[0].classList.contains('open'));
  ev('agentUnfoldAll()');
  check('还原：折叠组全拆掉，发件卡回到原消息里', doc.querySelectorAll('.pa-fold').length === 0
    && solo.parentElement === b.box && early.parentElement === b.box && live.parentElement === b.box
    && mixed.parentElement === b.box && !!solo.querySelector('.tool-peer-send')
    && !!early.querySelector('.tool-peer-send') && !!live.querySelector('.tool-peer-send')
    && !!mixed.querySelector('.tool-peer-send'));
  console.log(failed ? `\n${failed} 项失败` : '\n全部通过');
  process.exit(failed ? 1 : 0);
}
main().catch((e) => { console.error(e); process.exit(1); });
