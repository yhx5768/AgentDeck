// Personal Agent：出站 A2A 发件卡在主对话里的可见性 / 多条发送组 / 混合步骤拆分 / 不被回报组吞掉。
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
  // 单条发送：保持紫色卡片可见
  const solo = b.asst(b.sendHtml('w·支付重构', '【派工】跟进退款卡剩余事项', 'solo-refund'), '10:00:01');
  b.text('已安排支付重构跟进。', '10:00:02');
  // 两条连续 send-only：折成 A2A 发送组（第二条投递失败）
  const s1 = b.asst(b.sendHtml('w·支付重构', '【派工】退款失败口径确认', 'dispatch-refund'), '10:05:01');
  const s2 = b.asst(b.sendHtml('w·登录改版', '【派工】风控文案继续推进', 'dispatch-login', true), '10:05:02');
  b.text('已安排 worker。', '10:05:03');
  // 一条消息里两张发件卡：仍然算 2 条发送，一个组、一行
  const two = b.asst(b.sendHtml('w·数据迁移', '【催办】演练方案进度', 'nudge-mig') + b.sendHtml('w·监控告警', '【催办】分级清单进度', 'nudge-alert'), '10:08:00');
  b.text('已经催过了。', '10:08:01');
  // 混合步骤：正文 + 发件卡 + 其它工具
  const mixed = b.asst('<p>顺手记一下。</p>' + b.sendHtml('w·登录改版', '【同步】灰度先按现在的来', 'sync-login') + b.bash(), '10:10:00');
  b.peer('w·登录改版', '【完成】已收到同步', T0 + 20 * 60000);
  ev('agentFoldNow()');

  const a2a = () => [...doc.querySelectorAll('#messages > .pa-fold.a2a')];
  const rep = () => [...doc.querySelectorAll('#messages > .pa-fold:not(.silent):not(.a2a)')];
  const silent = () => [...doc.querySelectorAll('#messages > .pa-fold.silent')];
  check('单条 A2A 发送保持可见（没有进任何折叠组）', solo.parentElement === b.box && !!solo.querySelector(':scope > .bubble > .tool-peer-send')
    && !solo.closest('.pa-fold'));
  check('两条连续发送折成独立的「2 条 A2A 发送」组，默认折叠', a2a().length === 2 && !a2a()[0].classList.contains('open'));
  const h = a2a()[0].querySelector('.pa-fold-h').textContent.replace(/\s+/g, ' ');
  check('组头部：N 条 A2A 发送 + 收件人 chip + 任务 id + 预览', /^2 条 A2A 发送/.test(h)
    && /支付重构/.test(h) && /登录改版/.test(h) && /dispatch-refund/.test(h) && /退款失败口径确认/.test(h), h);
  check('组头标出投递失败', /失败 ×1/.test(h), h);
  check('组里每条一行：收件人 / id / 预览，点开才看原始发件卡', a2a()[0].querySelectorAll('.pa-rep.a2a-send').length === 2
    && /dispatch-login/.test(a2a()[0].querySelectorAll('.pa-rep.a2a-send')[1].textContent)
    && /风控文案继续推进/.test(a2a()[0].querySelectorAll('.pa-rep.a2a-send')[1].textContent)
    && !a2a()[0].querySelector('.pa-rep-b .tool-peer-send').closest('.open'));
  a2a()[0].querySelector('.pa-fold-h').click();
  check('点头部展开，组内恢复原始发件卡（markdown/收件人/结果都在卡里）', a2a()[0].classList.contains('open')
    && a2a()[0].querySelectorAll('.pa-rep-b > .msg-assistant .tool-peer-send').length === 2
    && /dispatch-login/.test(a2a()[0].textContent));
  a2a()[0].querySelectorAll('.pa-rep-h')[1].click();
  check('点一行展开发件卡', a2a()[0].querySelectorAll('.pa-rep')[1].classList.contains('open') && !!a2a()[0].querySelectorAll('.pa-rep')[1].querySelector('.peer-send-to'));
  const h2 = a2a()[1].querySelector('.pa-fold-h').textContent.replace(/\s+/g, ' ');
  check('一条消息里两张发件卡也算 2 条，成一个组、一行', a2a()[1].querySelectorAll('.pa-rep.a2a-send').length === 1 && /^2 条 A2A 发送/.test(h2) && two.closest('.pa-fold') === a2a()[1], h2);
  check('混合步骤：正文和发件卡留在消息里，其它工具折到下面', mixed.parentElement === b.box && !!mixed.querySelector('.tool-peer-send')
    && !mixed.querySelector('.tool-block:not(.tool-peer-send)') && mixed.nextElementSibling === silent()[0]
    && silent()[0].querySelectorAll('.pa-rep.step').length === 1 && !silent()[0].querySelector('.tool-peer-send'));
  check('回报组与 A2A 发送组互不混合、各数各的', rep().length === 2 && rep().every((f) => f.querySelectorAll('.pa-rep').length === 1 && !f.querySelector('.tool-peer-send')));
  const snapshot = b.box.innerHTML;
  ev('agentFoldNow()');
  check('重复扫描幂等', b.box.innerHTML === snapshot);
  // load more：在第一个 A2A 组前插一条更早的发送，并进同一组且保持展开
  const early = b.asst(b.sendHtml('w·对账脚本', '【派工】对账脚本继续重跑', 'early-recon'), '09:59:00');
  ev('agentFoldNow()');
  const target = a2a()[0];
  target.before(early);
  ev('agentFoldNow()');
  const after = a2a()[0];
  check('前插更早的发送并进同一组，计数增加，展开状态不重置', a2a().length === 2
    && after.querySelectorAll('.pa-rep.a2a-send').length === 3
    && /^3 条 A2A 发送/.test(after.querySelector('.pa-fold-h').textContent.replace(/\s+/g, ' '))
    && after.classList.contains('open'));
  ev('agentUnfoldAll()');
  check('还原：折叠组全拆掉，发件卡回到原消息里', doc.querySelectorAll('.pa-fold').length === 0
    && !!solo.querySelector('.tool-peer-send') && !!mixed.querySelector('.tool-peer-send')
    && !!early.querySelector('.tool-peer-send'));
  console.log(failed ? `\n${failed} 项失败` : '\n全部通过');
  process.exit(failed ? 1 : 0);
}
main().catch((e) => { console.error(e); process.exit(1); });
