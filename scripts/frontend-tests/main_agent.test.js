// 主 Agent（main_agent）UI 回归测试：
//   - 机器人面板里的「作为主 Agent」开关：payload 语义（打开写默认提示、关闭只清
//     默认值）、覆盖确认条件、地址名提醒与显式改名动作；
//   - 角色提示弹窗：字节计数/上限、保存/清空/填入默认（填入不自动写库）、错误展示；
//   - 侧栏主 Agent 头像条：无主 Agent 时隐藏、多个都显示、常驻优先、点击切换。
//
// 局限：jsdom 不做布局，头像条长什么样、横向滚动是否顺滑验不了；能钉的是 DOM
// 契约、payload 语义和「哪些动作会/不会写库」。
const fs = require('fs');
const path = require('path');
const { JSDOM } = require('jsdom');

const WEB = path.resolve(__dirname, '../../web');
const src = fs.readFileSync(path.join(WEB, 'app.js'), 'utf8');

function grabFn(name) {
  const s = src.indexOf('function ' + name + '(');
  if (s < 0) throw new Error('找不到函数 ' + name);
  // 保留 async 前缀：否则 eval 出来的函数里 await 会变成语法错误。
  const start = src.slice(Math.max(0, s - 6), s) === 'async ' ? s - 6 : s;
  const b = src.indexOf('{', s);
  let d = 0, q = null;
  for (let i = b; i < src.length; i++) {
    const c = src[i], p = src[i - 1];
    if (q) { if (c === q && p !== '\\') q = null; continue; }
    if (c === '"' || c === "'" || c === '`') { q = c; continue; }
    if (c === '{') d++;
    else if (c === '}') { d--; if (!d) return src.slice(start, i + 1); }
  }
  throw new Error(name + ' 括号不配对');
}

function grabLine(needle) {
  const i = src.indexOf(needle);
  if (i < 0) throw new Error('找不到: ' + needle);
  const end = src.indexOf('\n', i);
  return src.slice(i, end).trim();
}

const dollarLine = grabLine('const $ = (sel)');

const HTML = `<!doctype html><html><body>
  <div id="main-agents" class="main-agents hidden"></div>
  <button id="btn-role-prompt" class="round-icon-btn"></button>
  <div id="resident-pop" class="popover-panel hidden">
    <input type="checkbox" id="main-agent-toggle">
    <div id="main-agent-hint" class="rp-hint-list"></div>
    <input id="resident-name">
    <div id="resident-status"></div>
  </div>
  <div id="modal-role-prompt" class="modal hidden">
    <textarea id="role-prompt-text"></textarea>
    <span id="role-prompt-counter"></span>
    <span id="role-prompt-note"></span>
    <div id="role-prompt-error" class="hidden"></div>
    <button id="role-prompt-fill"></button>
    <button id="role-prompt-clear"></button>
    <button id="role-prompt-save"></button>
  </div>
</body></html>`;

const DEFAULT = '## AgentDeck 主 Agent 每轮规则\n\n1. 归属：先判断任务。\n2. 派工：先查 worker。';

const fn = (n) => grabFn(n);
const setup = `
${dollarLine}
const state = window.__state;
const api = window.__api;
const alert = window.__alert;
const confirm = window.__confirm;
function openModal(sel) { document.querySelector(sel).classList.remove('hidden'); }
function closeModal(el) { el.closest('.modal').classList.add('hidden'); }
function applySession(sess) { window.__applySession(sess); }
function renderSessionList() {}
function updateResidentUI() {}
function openSession(id) { window.__openSession(id); }
function ensureMainAgentBoard() {}
${fn('nameInitial')}
${fn('mainAgentSessions')}
${fn('runningMainAgentWorkers')}
${fn('mainAgentWorkerVisibleCount')}
${fn('workerHue')}
${fn('workerTipText')}
${fn('workerInitial')}
${fn('renderWorkerCluster')}
${fn('mainAgentInnerWidth')}
${fn('renderMainAgents')}
${fn('mainAgentToggleBody')}
${fn('mainAgentNeedsOverwriteConfirm')}
${fn('updateMainAgentUI')}
${fn('rolePromptMaxBytes')}
${fn('rolePromptBytes')}
${fn('ensureMainAgentPrompt')}
${fn('updateRolePromptButton')}
${fn('updateRolePromptCounter')}
${fn('rolePromptError')}
${fn('openRolePromptDialog')}
${fn('saveRolePrompt')}
${fn('fillRolePromptDefault')}
${fn('clearRolePromptDialog')}
window.__x = {
  mainAgentToggleBody, mainAgentNeedsOverwriteConfirm, updateMainAgentUI,
  renderMainAgents, mainAgentSessions, runningMainAgentWorkers,
  mainAgentWorkerVisibleCount, renderWorkerCluster, workerTipText,
  rolePromptBytes, rolePromptMaxBytes,
  ensureMainAgentPrompt, updateRolePromptCounter, updateRolePromptButton,
  openRolePromptDialog, saveRolePrompt, fillRolePromptDefault, clearRolePromptDialog,
};
`;

const dom = new JSDOM(HTML, { runScripts: 'outside-only', url: 'http://localhost/' });
const w = dom.window;

const state = {
  sessions: [], current: null, mainAgentPrompt: null, mainAgentMaxBytes: 32768,
  agentBoard: { tasks: [] }, mainAgentBoardAt: Date.now(), maWorkerIds: [],
  maWorkerMeta: {}, maRenderSig: '', previewWorkers: false,
};
const calls = { api: [], patches: [], applied: [], opened: [], alerts: [], confirms: [] };
let failNext = false;
let confirmReturn = true;
w.__state = state;
w.__calls = calls;
w.__api = async (p, opts) => {
  calls.api.push({ p, opts });
  if (p === '/api/main-agent-prompt') return { prompt: DEFAULT, max_bytes: 32768 };
  if (opts && opts.method === 'PATCH') {
    if (failNext) { failNext = false; throw new Error('boom'); }
    const body = JSON.parse(opts.body);
    calls.patches.push(body);
    return Object.assign({}, state.current, body);
  }
  return {};
};
w.__alert = (m) => calls.alerts.push(m);
w.__confirm = (m) => { calls.confirms.push(m); return confirmReturn; };
w.__applySession = (sess) => {
  calls.applied.push(sess);
  state.current = sess;
  const i = state.sessions.findIndex((x) => x.id === sess.id);
  if (i >= 0) state.sessions[i] = sess;
};
w.__openSession = (id) => calls.opened.push(id);
w.eval(setup);

const X = w.__x;
const $ = (s) => w.document.querySelector(s);

let failed = 0;
function check(name, cond, extra) {
  if (cond) console.log('  ok  ' + name);
  else { failed++; console.log('  FAIL ' + name + (extra ? '  →  ' + extra : '')); }
}
const tick = () => new Promise((r) => setTimeout(r, 0));

(async () => {
  console.log('主 Agent 开关 payload');

  // 打开：写入标记 + 默认提示（覆盖由调用方确认）
  let b = X.mainAgentToggleBody({ role_prompt: '自定义' }, true, DEFAULT);
  check('打开时写默认提示', b.main_agent === true && b.role_prompt === DEFAULT, JSON.stringify(b));
  // 关闭：仅当仍是默认提示时清空
  b = X.mainAgentToggleBody({ role_prompt: DEFAULT }, false, DEFAULT);
  check('关闭且仍为默认 → 清空', b.main_agent === false && b.role_prompt === '');
  b = X.mainAgentToggleBody({ role_prompt: '自定义' }, false, DEFAULT);
  check('关闭且自定义 → 保留', b.main_agent === false && !('role_prompt' in b), JSON.stringify(b));
  b = X.mainAgentToggleBody({ role_prompt: '' }, false, DEFAULT);
  check('关闭且为空 → 不带 role_prompt', !('role_prompt' in b), JSON.stringify(b));

  check('自定义内容需要确认覆盖',
    X.mainAgentNeedsOverwriteConfirm({ role_prompt: '自定义' }, DEFAULT) === true);
  check('默认提示不需要确认',
    X.mainAgentNeedsOverwriteConfirm({ role_prompt: DEFAULT }, DEFAULT) === false);
  check('空内容不需要确认',
    X.mainAgentNeedsOverwriteConfirm({ role_prompt: '' }, DEFAULT) === false);

  console.log('机器人面板的主 Agent 提示');

  state.current = {
    id: 's1', name: '调度', main_agent: true, peer_name: 'worker', resident: false,
    role_prompt: DEFAULT, status: {},
  };
  state.sessions = [state.current, { id: 's2', name: '另一个主 Agent', main_agent: true }];
  X.updateMainAgentUI();
  check('开关跟随 main_agent', $('#main-agent-toggle').checked === true);
  const hintText = $('#main-agent-hint').textContent;
  check('地址名不是 mainAgent 时提示', hintText.includes('地址名不是 mainAgent'), hintText);
  check('给出显式改名动作', !!$('#main-agent-hint .rp-mini-btn'));
  check('提示已有其他主 Agent', hintText.includes('另有主 Agent'), hintText);
  check('主 Agent 未开常驻时提示', hintText.includes('未开启常驻'), hintText);

  // 地址名已在 mainAgent 且没有其他主 Agent / 常驻已开 → 不显示警告
  state.current = {
    id: 's1', name: '调度', main_agent: true, peer_name: 'mainAgent', resident: true,
    role_prompt: DEFAULT, status: {},
  };
  state.sessions = [state.current];
  X.updateMainAgentUI();
  check('地址名正确时没有警告',
    !$('#main-agent-hint').textContent.includes('地址名不是 mainAgent'));

  // 显式改名的动作会发 PATCH peer_name=mainAgent
  state.current = {
    id: 's1', name: '调度', main_agent: true, peer_name: 'worker', resident: true,
    role_prompt: DEFAULT, status: {},
  };
  state.sessions = [state.current];
  X.updateMainAgentUI();
  const renameBtn = $('#main-agent-hint .rp-mini-btn');
  calls.patches.length = 0;
  renameBtn.onclick();
  await tick();
  check('点改名按钮才发 peer_name PATCH',
    calls.patches.length === 1 && calls.patches[0].peer_name === 'mainAgent',
    JSON.stringify(calls.patches));

  // 关闭标记后提示区清空、开关回 false
  state.current = { id: 's1', name: '调度', main_agent: false, role_prompt: '', status: {} };
  X.updateMainAgentUI();
  check('非主 Agent 时开关关闭', $('#main-agent-toggle').checked === false);
  check('非主 Agent 时提示区为空', $('#main-agent-hint').children.length === 0);

  console.log('侧栏主 Agent 头像条');

  state.sessions = [
    { id: 'a', name: '普通会话', main_agent: false, status: {} },
  ];
  state.current = null;
  X.renderMainAgents();
  check('无主 Agent → 整条隐藏', $('#main-agents').classList.contains('hidden'));
  check('无主 Agent → 不渲染条目', $('#main-agents').children.length === 0);

  const now = new Date().toISOString();
  state.sessions = [
    { id: 'a', name: '普通会话', main_agent: false, status: {} },
    { id: 'b', name: '中枢', main_agent: true, resident: false, unread: false,
      updated_at: now, status: {} },
    { id: 'c', name: '调度', main_agent: true, resident: true, unread: true,
      updated_at: '2026-01-01T00:00:00Z', status: { running: true } },
  ];
  state.current = { id: 'b' };
  X.renderMainAgents();
  const items = [...$('#main-agents').children];
  check('有主 Agent → 显示', !$('#main-agents').classList.contains('hidden'));
  check('多个主 Agent 都显示', items.length === 2, String(items.length));
  check('常驻主 Agent 排最前', items[0].querySelector('.ma-name').textContent === '调度');
  check('当前会话高亮', items.some((it) => it.classList.contains('active') &&
    it.querySelector('.ma-name').textContent === '中枢'));
  check('未读角标', !!items[0].querySelector('.ma-badge'));
  check('运行/常驻态 class',
    items[0].querySelector('.ma-avatar').classList.contains('running') &&
    items[0].querySelector('.ma-avatar').classList.contains('resident'));
  check('头像取名字首字', items[0].querySelector('.ma-avatar').textContent.startsWith('调'));

  calls.opened.length = 0;
  items[0].onclick();
  check('点击头像切换会话', calls.opened[0] === 'c', JSON.stringify(calls.opened));

  console.log('主 Agent 旁的 running worker');

  const strip = $('#main-agents');
  Object.defineProperty(strip, 'clientWidth', { configurable: true, value: 250 });
  state.maRenderSig = '';
  state.maWorkerIds = [];
  state.maWorkerMeta = {};
  state.current = { id: 'main' };
  const workers = Array.from({ length: 10 }, (_, i) => ({
    id: `worker-${i + 1}`, name: `worker-${i + 1}`, peer_name: `worker-${i + 1}`,
    main_agent: false, status: { running: true, queued: 0 }, updated_at: now,
  }));
  state.sessions = [
    { id: 'main', name: '主 Agent', main_agent: true, resident: true, status: {} },
    ...workers,
    { id: 'ordinary', name: '普通 running', main_agent: false, status: { running: true, queued: 0 } },
  ];
  state.agentBoard = {
    tasks: workers.map((w, i) => ({
      id: `T-${i + 1}`, title: `任务 ${i + 1}`, status: '进行中', worker: w.peer_name,
      session: w.id, updated_at: now,
    })).concat([{
      id: 'T-1-newer', title: '最新任务', status: '进行中', worker: 'worker-1',
      session: 'worker-1', updated_at: '2026-11-03T22:01:00Z',
    }]),
  };
  X.renderMainAgents();
  check('可见 worker 按 250px 计算为 8 个', strip.querySelectorAll('.ma-worker:not(.ma-worker-leaving)').length === 8,
    String(strip.querySelectorAll('.ma-worker').length));
  check('剩余显示 +2', strip.querySelector('.ma-worker-more').textContent === '+2');
  check('总数徽标显示 10', strip.querySelector('.ma-total').textContent === '10');
  check('总数徽标在左下角', !!strip.querySelector('.ma-avatar .ma-total'));
  check('tooltip 带 worker 名和任务', strip.querySelector('.ma-worker').title.includes('worker-1') &&
    strip.querySelector('.ma-worker').title.includes('最新任务'), strip.querySelector('.ma-worker').title);
  check('同 worker 多任务去重取最新', strip.querySelectorAll('.ma-worker').length === 8);
  const workerTitles = [...strip.querySelectorAll('.ma-worker')].map((el) => el.title).join('\n');
  check('普通 running 会话不计入', !workerTitles.includes('普通 running'), workerTitles);

  calls.opened.length = 0;
  strip.querySelector('.ma-worker').onclick({ stopPropagation() {} });
  check('点击 worker 打开会话', calls.opened[0] === 'worker-1', JSON.stringify(calls.opened));

  const countFn = X.mainAgentWorkerVisibleCount;
  check('动态数量 250px → 8', countFn(250, false) === 8, String(countFn(250, false)));
  check('动态数量 200px → 6', countFn(200, false) === 6, String(countFn(200, false)));
  check('动态数量过窄 → 最少 2', countFn(80, false) === 2, String(countFn(80, false)));

  // 全部结束后：总数徽标隐藏，退场鬼影 180ms 后移除
  state.sessions = [
    { id: 'main', name: '主 Agent', main_agent: true, resident: true, status: {} },
    { id: 'worker-1', name: 'worker-1', main_agent: false, status: { running: false, queued: 0 } },
  ];
  state.agentBoard = { tasks: [] };
  state.maRenderSig = '';
  X.renderMainAgents();
  check('0 running → 总数徽标隐藏', !strip.querySelector('.ma-total'));
  await new Promise((r) => setTimeout(r, 220));
  check('0 running → worker 组退场', !strip.querySelector('.ma-workers'));

  // 普通会话没有任务绑定 → 不显示任何 worker
  state.sessions = [
    { id: 'main', name: '主 Agent', main_agent: true, resident: true, status: {} },
    { id: 'ordinary', name: '普通 running', main_agent: false, status: { running: true, queued: 0 } },
  ];
  state.agentBoard = { tasks: [] };
  state.maRenderSig = '';
  state.maWorkerIds = [];
  X.renderMainAgents();
  check('无任务绑定 → 不显示 worker', !strip.querySelector('.ma-workers'));
  check('无任务绑定 → 不显示徽标', !strip.querySelector('.ma-total'));

  const css = fs.readFileSync(path.join(WEB, 'style.css'), 'utf8');
  check('CSS: worker 组负 margin', css.includes('margin-left: calc(-1 * var(--ma-gap) - var(--ma-overlap-extra))'));
  check('CSS: +N 同节奏 bob', css.includes('.ma-worker-more') && css.includes('ma-worker-more-bob'));
  check('CSS: reduced-motion 停动画', /prefers-reduced-motion[\s\S]*?\.ma-worker, \.ma-worker-more \{ animation: none !important; \}/.test(css));
  check('CSS: 移动端变量', /@media \(max-width: 760px\)[\s\S]*?--ma-overlap-extra: 14px/.test(css));
  check('CSS: worker 组底边对齐主头像', /\.ma-workers\s*\{[^}]*padding-top:\s*14px/s.test(css));

  console.log('角色提示弹窗');

  check('UTF-8 字节：ASCII', X.rolePromptBytes('abc') === 3);
  check('UTF-8 字节：中文', X.rolePromptBytes('中文') === 6);
  check('UTF-8 字节：emoji', X.rolePromptBytes('😀') === 4);
  check('UTF-8 字节：混合', X.rolePromptBytes('中文abc😀') === 13);

  state.current = { id: 's1', name: 'worker', engine: 'claude', role_prompt: '旧规则' };
  calls.api.length = 0;
  await X.openRolePromptDialog();
  check('打开时载入当前角色提示', $('#role-prompt-text').value === '旧规则');
  check('弹窗显示', !$('#modal-role-prompt').classList.contains('hidden'));
  check('说明常驻下次重启生效',
    $('#role-prompt-note').textContent.includes('下次重启'), $('#role-prompt-note').textContent);

  state.current = { id: 's1', name: 'oc', engine: 'opencode', role_prompt: '' };
  await X.openRolePromptDialog();
  check('opencode 明确提示不注入',
    $('#role-prompt-note').textContent.includes('暂不支持'), $('#role-prompt-note').textContent);

  $('#role-prompt-text').value = '中文abc';
  X.updateRolePromptCounter();
  check('字节计数', $('#role-prompt-counter').textContent.includes('9 / 32768'),
    $('#role-prompt-counter').textContent);
  check('未超限可保存', $('#role-prompt-save').disabled === false);

  $('#role-prompt-text').value = 'x'.repeat(32769);
  X.updateRolePromptCounter();
  check('超限禁用保存', $('#role-prompt-save').disabled === true);
  check('超限计数标红', $('#role-prompt-counter').classList.contains('over'));

  // 填入默认：只改 textarea，不写库
  state.current = { id: 's1', name: 'worker', engine: 'claude', role_prompt: '' };
  state.mainAgentPrompt = null;
  calls.patches.length = 0;
  await X.fillRolePromptDefault();
  check('填入主 Agent 默认', $('#role-prompt-text').value === DEFAULT);
  check('填入默认不自动保存', calls.patches.length === 0, JSON.stringify(calls.patches));

  // 保存
  $('#role-prompt-text').value = '新规则';
  X.updateRolePromptCounter();
  $('#modal-role-prompt').classList.remove('hidden');
  calls.patches.length = 0;
  await X.saveRolePrompt('新规则');
  check('保存写 role_prompt', calls.patches.length === 1 && calls.patches[0].role_prompt === '新规则',
    JSON.stringify(calls.patches));
  check('保存后关闭弹窗', $('#modal-role-prompt').classList.contains('hidden'));
  check('保存后按钮高亮', $('#btn-role-prompt').classList.contains('active'));

  // 清空：取消时不写库，确认后写空串
  $('#role-prompt-text').value = '要清掉';
  calls.patches.length = 0;
  confirmReturn = false;
  await X.clearRolePromptDialog();
  check('清空未确认不写库', calls.patches.length === 0);
  confirmReturn = true;
  calls.patches.length = 0;
  await X.clearRolePromptDialog();
  check('清空确认后写空串',
    calls.patches.length === 1 && calls.patches[0].role_prompt === '', JSON.stringify(calls.patches));

  // 保存失败：错误显示在弹窗里，不关闭
  state.current = { id: 's1', name: 'worker', engine: 'claude', role_prompt: '' };
  rolePromptTargetReset();
  $('#modal-role-prompt').classList.remove('hidden');
  failNext = true;
  calls.patches.length = 0;
  await X.saveRolePrompt('会失败');
  check('保存失败不写库', calls.patches.length === 0);
  check('保存失败显示错误',
    !$('#role-prompt-error').classList.contains('hidden') &&
    $('#role-prompt-error').textContent.includes('boom'), $('#role-prompt-error').textContent);
  check('保存失败不关闭弹窗', !$('#modal-role-prompt').classList.contains('hidden'));

  // 超限保存：本地拦截，不发请求
  calls.patches.length = 0;
  $('#role-prompt-text').value = 'y'.repeat(32769);
  X.updateRolePromptCounter();
  await X.saveRolePrompt('y'.repeat(32769));
  check('超限保存被本地拦截', calls.patches.length === 0);
  check('超限保存显示错误',
    $('#role-prompt-error').textContent.includes('超出上限'), $('#role-prompt-error').textContent);

  console.log(failed ? `\n${failed} 项失败` : '\n全部通过');
  process.exit(failed ? 1 : 0);
})();

// 测试辅助：saveRolePrompt 用 rolePromptTarget 防止切会话写错对象；多个用例复用
// s1 时需要把「打开时的目标」重置回当前会话。
function rolePromptTargetReset() {
  X.openRolePromptDialog();
}
