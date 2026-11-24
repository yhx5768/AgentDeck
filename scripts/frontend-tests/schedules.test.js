// 定时任务页（web/schedules.js）回归：
// - 列表渲染：名称 / cron / 目标 / 上次 / 下次 / 状态 / 一次性 / 来源；
// - 行内启停、立即执行、删除（confirm 后 DELETE）；
// - 自然语言模式：描述 → 解析 → 候选（歧义时多个）→ 选中后保存；描述兼作 prompt；
// - cron 模式：示例、服务端下次执行预览、保存 POST/PUT；
// - harness 只读列表 + 导入（默认停用，不改源文件）；空态 CTA。
//
// 局限：jsdom 不做布局；时间格式随本机时区，只断言文案前缀不钉具体时刻。
const fs = require('fs');
const path = require('path');
const { JSDOM } = require('jsdom');

const WEB = path.resolve(__dirname, '../../web');
const src = fs.readFileSync(path.join(WEB, 'schedules.js'), 'utf8');

const HTML = `<!doctype html><html><body>
  <div id="empty-state" class="hidden"></div>
  <div id="chat-view" class="hidden"></div>
  <div id="needs-view" class="hidden"></div>
  <button class="chat-tab active" data-tab="chat"></button>
  <div id="schedules-view" class="hidden">
    <header id="schedules-header">
      <div class="sched-title-row">
        <button id="sched-back"></button>
        <span id="sched-count"></span>
        <button id="sched-refresh"></button>
        <button id="sched-new"></button>
      </div>
    </header>
    <div id="sched-body">
      <div id="sched-error" class="hidden"></div>
      <div id="sched-list"></div>
      <section id="sched-harness">
        <div class="sched-harness-head">
          <div class="sched-harness-title-row">
            <span class="sched-harness-title"></span>
            <span class="sched-harness-hint"></span>
          </div>
          <div class="sched-harness-tools">
            <button id="sched-harness-add"></button>
            <button id="sched-harness-restore" class="hidden"></button>
          </div>
        </div>
        <div id="sched-harness-error" class="hidden"></div>
        <div id="sched-harness-list"></div>
      </section>
    </div>
  </div>
  <div id="modal-schedule" class="modal hidden">
    <div class="modal-card">
      <h3 id="sched-modal-title"></h3>
      <div id="sched-mode">
        <button type="button" data-sched-mode="nl" class="active"></button>
        <button type="button" data-sched-mode="cron"></button>
      </div>
      <div id="sched-nl-block">
        <textarea id="sched-nl-text"></textarea>
        <div id="sched-parser">
          <button type="button" data-sched-parser="rules" class="active"></button>
          <button type="button" data-sched-parser="ai"></button>
        </div>
        <button id="sched-parse"></button>
        <span id="sched-parse-note"></span>
        <div id="sched-ai-note" class="hidden"></div>
        <div id="sched-parse-error" class="hidden"></div>
        <div id="sched-ai-meta" class="hidden"></div>
        <div id="sched-candidates" class="hidden"></div>
        <div id="sched-ai-rationale" class="hidden"></div>
        <div id="sched-ai-questions" class="hidden"></div>
      </div>
      <div id="sched-cron-block" class="hidden">
        <input id="sched-cron">
        <div id="sched-cron-examples"></div>
      </div>
      <input id="sched-name">
      <div id="sched-next-preview"></div>
      <textarea id="sched-prompt"></textarea>
      <div id="sched-prompt-hint" class="hidden"></div>
      <div class="sched-final-preview">
        <span class="sched-final-label"></span>
        <div id="sched-final-text"></div>
      </div>
      <select id="sched-target"></select>
      <input type="checkbox" id="sched-enabled" checked>
      <div id="sched-modal-error" class="hidden"></div>
      <button data-close id="sched-cancel"></button>
      <button id="sched-save"></button>
    </div>
  </div>
</body></html>`;

const dom = new JSDOM(HTML, { runScripts: 'outside-only', url: 'http://deck.local/' });
const w = dom.window;
const doc = w.document;
w.__schedSkipInit = true;

const STUBS = `
const $ = (s) => document.querySelector(s);
const state = {
  current: null,
  sessions: [
    { id: 'main', name: '主 Agent', main_agent: true, peer_name: 'mainAgent' },
    { id: 'w1', name: 'w·巡检', peer_name: 'w-xunjian' },
  ],
};
window.__tasks = [];
window.__harness = { groups: [], files: [], candidates: [], error: '', path: '' };
window.__manualPath = '';
window.__promptPath = '';
window.__apiCalls = [];
window.__confirmText = '';
window.__apiHandler = null;
function api(path, opts = {}) {
  const method = opts.method || 'GET';
  const body = opts.body ? JSON.parse(opts.body) : null;
  window.__apiCalls.push({ path, method, body });
  if (window.__apiHandler) {
    const r = window.__apiHandler(path, method, body);
    if (r !== undefined) return Promise.resolve(r);
  }
  if (path === '/api/schedules' && method === 'GET') {
    return Promise.resolve({ tasks: window.__tasks, now: Date.now() });
  }
  if (path.startsWith('/api/schedules/harness') && method === 'GET') {
    if (path.includes('?path=')) {
      const q = decodeURIComponent(path.split('?path=')[1]);
      if (q !== window.__manualPath) {
        return Promise.reject(new Error('读取不到 harness 文件：' + q));
      }
      return Promise.resolve({
        path: q, candidates: [q],
        files: [
          { path: q, workspace: '/ws/manual', container: 'workspace', empty: false,
            tasks: [{ id: 'm1', cron: '0 8 * * *', prompt: '手动路径任务', recurring: true, name: '手动路径任务', valid: true }] },
        ],
        groups: [{ workspace: '/ws/manual', container: 'workspace', task_count: 1, files: [
          { path: q, workspace: '/ws/manual', container: 'workspace', empty: false,
            tasks: [{ id: 'm1', cron: '0 8 * * *', prompt: '手动路径任务', recurring: true, name: '手动路径任务', valid: true }] },
        ] }],
      });
    }
    return Promise.resolve(window.__harness);
  }
  if (path === '/api/schedules/parse' && method === 'POST') {
    const text = String((body && body.text) || '');
    if (!text || text.includes('bad')) {
      return Promise.reject(new Error('没识别出频率。支持：每天 / 工作日 / 每周X / 每月X号 / 每小时 / 每N分钟'));
    }
    if (text.includes('18点')) {
      return Promise.resolve({ mode: 'rules', candidates: [
        { cron: '0 9 * * *', label: '每天 09:00', name: '巡检', next_run_local: '2026-11-24 09:00' },
        { cron: '0 18 * * *', label: '每天 18:00', name: '巡检', next_run_local: '2026-11-23 18:00' },
      ], timezone: 'CST' });
    }
    return Promise.resolve({ mode: 'rules', candidates: [
      { cron: '0 9 * * *', label: '每天 09:00', name: '看板巡检', next_run_local: '2026-11-24 09:00' },
    ], timezone: 'CST' });
  }
  if (path === '/api/schedules/parse-ai' && method === 'POST') {
    const text = String((body && body.text) || '');
    if (text.includes('busy')) {
      return Promise.reject(new Error('已有一个 AI 解析任务在运行，请稍候再试'));
    }
    if (text.includes('fallback')) {
      return Promise.resolve({ mode: 'rules', fallback: true, reason: 'AI 解析超时（60s）',
        candidates: [{ cron: '0 9 * * *', label: '每天 09:00', name: '看板巡检', next_run_local: '2026-11-24 09:00' }],
        elapsed_ms: 60000, timezone: 'CST' });
    }
    if (text.includes('ask')) {
      return Promise.resolve({ mode: 'ai', needs_clarification: true,
        proposal: { name: '周报巡检', questions: ['按哪一周的周报口径？'], rationale: '口径未定' },
        elapsed_ms: 1200, timezone: 'CST' });
    }
    return Promise.resolve({
      mode: 'ai',
      proposal: {
        name: '看板巡检', cron: '0 9 * * *',
        prompt: '按 work/tracking 上周周报的口径巡检看板与常驻 worker。',
        rationale: '描述是每天9点，口径来自 work/tracking。',
        questions: [], next_run_local: '2026-11-24 09:00',
      },
      context_reads: ['Read /ws/work/tracking/README.md'],
      usage: { input_tokens: 1200, output_tokens: 80 },
      elapsed_ms: 2300, timezone: 'CST',
    });
  }
  if (path === '/api/schedules/preview') {
    if (!body || String(body.cron || '').includes('bad') || String(body.cron || '').trim().split(/\\s+/).length !== 5) {
      return Promise.reject(new Error('cron 需要 5 个字段（分 时 日 月 周），当前 1 个'));
    }
    return Promise.resolve({ next_run_local: '2026-11-24 09:00', timezone: 'CST' });
  }
  if (path === '/api/schedules/import-harness' && method === 'POST') {
    for (const g of window.__harness.groups || []) {
      for (const f of g.files || []) {
        if (f.path !== body.path) continue;
        const t = (f.tasks || []).find((x) => x.id === body.id);
        if (t) t.imported_id = 'imported-' + body.id;
      }
    }
    return Promise.resolve({ ok: true });
  }
  return Promise.resolve({ ok: true });
}
function hideAllViews() {
  ['#empty-state', '#chat-view', '#schedules-view'].forEach((s) => $(s).classList.add('hidden'));
  if (typeof schedStopPoll === 'function') schedStopPoll();
}
function boardDisconnect() {}
function renderIcons() {}
function openModal(sel) { $(sel).classList.remove('hidden'); }
function switchChatTab() {}
function agentModeOn() { return false; }
function agentRestoreView() {}
function confirm(msg) { window.__confirmText = msg; return true; }
function prompt(msg) { window.__promptMsg = msg; return window.__promptPath; }
`;

w.eval(STUBS + '\n' + src + '\nschedBind();');
// jsdom 自带一个返回 null 的 window.prompt，函数声明覆盖不了它；从 Node 侧重写。
w.prompt = () => w.__promptPath;

const tick = () => new Promise((resolve) => setTimeout(resolve, 0));
const settle = async () => { await tick(); await tick(); };
let failed = 0;
function check(name, cond, extra) {
  if (cond) console.log('  ok  ' + name);
  else { failed++; console.log('  FAIL ' + name + (extra ? '  →  ' + extra : '')); }
}
const rows = () => [...doc.querySelectorAll('#sched-list .sched-row')];
const hrows = () => [...doc.querySelectorAll('#sched-harness-list .sched-harness-row')];
const rowBtn = (row, label) => [...row.querySelectorAll('.sched-btn')].find((b) => b.textContent === label);
const calls = () => w.__apiCalls;
const lastCall = () => calls()[calls().length - 1];
const modal = () => doc.querySelector('#modal-schedule');
const modeBtn = (mode) => doc.querySelector('#sched-mode [data-sched-mode="' + mode + '"]');
const candidateCards = () => [...doc.querySelectorAll('#sched-candidates .sched-candidate')];

(async () => {
  w.__tasks = [
    {
      id: 't1', name: '每日站会', cron: '0 9 * * *', prompt: '总结昨天的工作\\n第二行',
      target_session: 'main', target_name: '主 Agent', target_address: 'mainAgent',
      enabled: true, last_run_at: '2026-11-22T01:00:00Z', last_status: 'ok',
      next_run_at: '2026-11-24T01:00:00Z', run_count: 3,
    },
    {
      id: 't2', name: '巡检', cron: '*/5 * * * *', prompt: '检查构建',
      target_session: 'w1', target_name: 'w·巡检',
      enabled: false, last_status: 'error: 目标会话不存在', run_count: 0,
    },
    {
      id: 't3', name: 'M145 检查', cron: '0 10 24 11 *', prompt: '检查 M145',
      target_session: 'main', target_name: '主 Agent', enabled: false,
      source: 'harness', source_id: 'h-once', max_runs: 1, run_count: 1,
      last_status: 'ok', last_run_at: '2026-11-24T02:00:00Z',
    },
  ];
  w.__harness = {
    path: '/ws/main/.claude/scheduled_tasks.json',
    candidates: ['/ws/main/.claude/scheduled_tasks.json', '/ws/other/.claude/scheduled_tasks.json', '/ws/empty/.claude/scheduled_tasks.json', '/ws/bad/.claude/scheduled_tasks.json'],
    error: '',
    files: [],
    groups: [
      { workspace: '/ws/main', container: 'workspace', task_count: 1, files: [
        { path: '/ws/main/.claude/scheduled_tasks.json', workspace: '/ws/main', container: 'workspace', empty: false, tasks: [
          { id: 'h-once', cron: '0 10 24 11 *', prompt: 'M145 稳定性检查\\n细节', recurring: false, name: 'M145 稳定性检查', valid: true },
        ] },
      ] },
      { workspace: '/ws/other', container: 'workspace', task_count: 1, files: [
        { path: '/ws/other/.claude/scheduled_tasks.json', workspace: '/ws/other', container: 'workspace', empty: false, tasks: [
          { id: 'h-daily', cron: '7 9 * * 1-5', prompt: '每日看板巡检', recurring: true, name: '每日看板巡检', valid: true },
        ] },
      ] },
      { workspace: '/ws/empty', container: 'workspace', task_count: 0, files: [
        { path: '/ws/empty/.claude/scheduled_tasks.json', workspace: '/ws/empty', container: 'workspace', empty: true, tasks: [] },
      ] },
      { workspace: '/ws/bad', container: 'workspace', task_count: 0, files: [
        { path: '/ws/bad/.claude/scheduled_tasks.json', workspace: '/ws/bad', container: 'workspace', empty: false, error: 'JSON 解析失败：unexpected end', tasks: [] },
      ] },
    ],
  };

  // ---------- 打开页面 ----------
  w.schedulesOpen();
  await settle();
  check('打开后显示定时任务页', !doc.querySelector('#schedules-view').classList.contains('hidden'));
  check('计数 = 3 个任务 / 1 个启用', doc.querySelector('#sched-count').textContent === '3 个任务 · 1 个启用',
    doc.querySelector('#sched-count').textContent);
  check('渲染三行', rows().length === 3, String(rows().length));
  const r1 = rows()[0];
  check('第一行名称', r1.querySelector('.sched-row-name').textContent === '每日站会');
  check('第一行 cron', r1.querySelector('.sched-cron').textContent === '0 9 * * *');
  check('第一行目标', r1.querySelector('.sched-row-meta').textContent.includes('目标：主 Agent'));
  check('第一行上次成功', r1.querySelector('.sched-row-meta').textContent.includes('上次：') &&
    r1.querySelector('.sched-row-meta').textContent.includes('成功'));
  check('第一行下次有值', r1.querySelector('.sched-row-meta').textContent.includes('下次：') &&
    !r1.querySelector('.sched-row-meta').textContent.includes('下次：—'));
  const r2 = rows()[1];
  check('停用行标记暂停', r2.querySelector('.sched-state').textContent === '暂停');
  check('失败状态标红', !!r2.querySelector('.sched-status.bad'));
  check('失败原因进 tooltip', r2.querySelector('.sched-status.bad').title === '目标会话不存在');
  const r3 = rows()[2];
  check('一次性任务标记已执行', r3.textContent.includes('一次性 · 已执行'));
  check('来源标 CLI 导入', r3.textContent.includes('CLI 导入'));
  check('执行次数 1/1', r3.querySelector('.sched-row-meta').textContent.includes('已执行：1 / 1'));

  // ---------- 行内动作 ----------
  rowBtn(r2, '启用').click();
  await settle();
  const toggle = calls().find((c) => c.path === '/api/schedules/t2/enable');
  check('启用发 POST enable', !!toggle && toggle.method === 'POST' && toggle.body.enabled === true);
  check('启停后重拉列表', calls().filter((c) => c.path === '/api/schedules' && c.method === 'GET').length >= 2);

  const beforeRuns = calls().filter((c) => c.path.endsWith('/run')).length;
  rowBtn(rows()[0], '立即执行').click();
  await settle();
  check('立即执行发 POST run', calls().filter((c) => c.path.endsWith('/run')).length === beforeRuns + 1);

  rowBtn(rows()[0], '删除').click();
  await settle();
  check('删除前 confirm 带任务名', w.__confirmText.includes('每日站会'), w.__confirmText);
  check('删除发 DELETE', calls().some((c) => c.path === '/api/schedules/t1' && c.method === 'DELETE'));

  // ---------- harness：多 workspace 只读列表 + 导入 ----------
  const hgroups = () => [...doc.querySelectorAll('#sched-harness-list .sched-harness-group')];
  const hfiles = () => [...doc.querySelectorAll('#sched-harness-list .sched-harness-file')];
  check('harness 按 workspace 分 4 组', hgroups().length === 4, String(hgroups().length));
  check('第一组是 mainAgent workspace', hgroups()[0].querySelector('.sched-harness-workspace').textContent === '/ws/main',
    hgroups()[0].querySelector('.sched-harness-workspace').textContent);
  check('组头显示任务数', hgroups()[0].textContent.includes('1 个任务'));
  check('各组都显示 workspace 路径', hgroups().map((g) => g.querySelector('.sched-harness-workspace').textContent).join(',') ===
    '/ws/main,/ws/other,/ws/empty,/ws/bad');
  check('harness 任务行两行', hrows().length === 2, String(hrows().length));
  check('空文件给空态不报错', hfiles()[2].textContent.includes('这个文件里没有任务') &&
    !hfiles()[2].classList.contains('sched-harness-task-error'));
  check('坏 JSON 只标该文件错误', hfiles()[3].textContent.includes('JSON 解析失败'));
  check('harness 一次性标记', hrows()[0].textContent.includes('一次性'));
  check('harness 周期标记', hrows()[1].textContent.includes('周期'));
  check('harness 不显示删除', !rowBtn(hrows()[0], '删除'));
  hrows()[0].querySelector('.sched-btn').click();
  await settle();
  const imp = calls().find((c) => c.path === '/api/schedules/import-harness');
  check('导入发 POST + 源路径/id', !!imp && imp.body.id === 'h-once' &&
    imp.body.path === '/ws/main/.claude/scheduled_tasks.json', JSON.stringify(imp && imp.body));
  check('导入后标记已导入', hrows()[0].textContent.includes('已导入'), hrows()[0].textContent);

  // 手动移除 / 恢复（只影响显示）。
  hfiles()[0].querySelector('.sched-harness-remove').click();
  check('移除后该文件消失', !hfiles().some((f) => f.dataset.path === '/ws/main/.claude/scheduled_tasks.json'));
  check('恢复按钮出现', !doc.querySelector('#sched-harness-restore').classList.contains('hidden'));
  doc.querySelector('#sched-harness-restore').click();
  await settle();
  check('恢复后文件回来', hfiles().some((f) => f.dataset.path === '/ws/main/.claude/scheduled_tasks.json'));

  // 手动添加路径：走 ?path= 校验后并入分组。
  w.__promptPath = '/ws/manual/.claude/scheduled_tasks.json';
  w.__manualPath = w.__promptPath;
  doc.querySelector('#sched-harness-add').click();
  await settle();
  check('手动添加路径后出现新 workspace', hgroups().some((g) => g.querySelector('.sched-harness-workspace').textContent === '/ws/manual'));
  check('手动路径任务可导入', hrows().length === 3, String(hrows().length));

  // ---------- 自然语言新建 ----------
  doc.querySelector('#sched-new').click();
  check('新建弹窗打开', !modal().classList.contains('hidden'));
  check('新建默认自然语言模式', modeBtn('nl').classList.contains('active') &&
    doc.querySelector('#sched-cron-block').classList.contains('hidden'));
  const nl = doc.querySelector('#sched-nl-text');
  nl.value = '每天9点进行看板巡检，检查常驻worker和运行中卡片是否异常';
  doc.querySelector('#sched-parse').click();
  await settle();
  const parseCall = calls().find((c) => c.path === '/api/schedules/parse');
  check('解析发 POST /api/schedules/parse', !!parseCall && parseCall.body.text === nl.value);
  check('单个候选直接选中', candidateCards().length === 1 && candidateCards()[0].classList.contains('on'));
  check('候选写入 cron', doc.querySelector('#sched-cron').value === '0 9 * * *');
  check('候选名称填入名称框', doc.querySelector('#sched-name').value === '看板巡检');
  check('候选显示下次执行', candidateCards()[0].textContent.includes('2026-11-24 09:00'));
  check('解析后 prompt 自动填入描述', doc.querySelector('#sched-prompt').value === nl.value,
    doc.querySelector('#sched-prompt').value);
  check('最终 prompt 预览显示描述', doc.querySelector('#sched-final-text').textContent === nl.value,
    doc.querySelector('#sched-final-text').textContent);
  check('prompt 有内容时隐藏空提示', doc.querySelector('#sched-prompt-hint').classList.contains('hidden'));

  // 清空 prompt：保留「留空则使用描述」提示，最终预览回退到描述。
  const promptEl = doc.querySelector('#sched-prompt');
  promptEl.value = '';
  promptEl.dispatchEvent(new w.Event('input', { bubbles: true }));
  check('清空 prompt 显示回退提示', !doc.querySelector('#sched-prompt-hint').classList.contains('hidden'));
  check('清空后最终预览回退描述', doc.querySelector('#sched-final-text').textContent === nl.value,
    doc.querySelector('#sched-final-text').textContent);
  promptEl.value = '自定义最终 prompt';
  promptEl.dispatchEvent(new w.Event('input', { bubbles: true }));
  check('手写 prompt 后最终预览跟随', doc.querySelector('#sched-final-text').textContent === '自定义最终 prompt');
  promptEl.value = '';
  promptEl.dispatchEvent(new w.Event('input', { bubbles: true }));

  // 描述兼作 prompt；名称/目标默认即可保存。
  doc.querySelector('#sched-save').click();
  await settle();
  const create = calls().find((c) => c.path === '/api/schedules' && c.method === 'POST');
  check('自然语言保存发 POST', !!create);
  check('自然语言 prompt 回退到描述', create && create.body.prompt === nl.value, JSON.stringify(create && create.body));
  check('自然语言保存 cron/名称', create && create.body.cron === '0 9 * * *' && create.body.name === '看板巡检');
  check('保存成功后关闭弹窗', modal().classList.contains('hidden'));

  // ---------- 歧义：多个候选 ----------
  doc.querySelector('#sched-new').click();
  doc.querySelector('#sched-nl-text').value = '每天9点和18点巡检';
  doc.querySelector('#sched-parse').click();
  await settle();
  check('歧义时列出两个候选', candidateCards().length === 2, String(candidateCards().length));
  check('歧义时不自动选', candidateCards().every((c) => !c.classList.contains('on')));
  candidateCards()[1].querySelector('input').click();
  check('选第二个候选写入 cron', doc.querySelector('#sched-cron').value === '0 18 * * *');
  // 未解析就保存要拦下：先改描述使候选失效，再点保存。
  const nl2 = doc.querySelector('#sched-nl-text');
  nl2.value = '每天9点和18点巡检，再改一下';
  nl2.dispatchEvent(new w.Event('input', { bubbles: true }));
  check('改描述后候选清空', candidateCards().length === 0);
  const beforeBadSave = calls().filter((c) => c.path === '/api/schedules' && c.method === 'POST').length;
  doc.querySelector('#sched-save').click();
  await settle();
  check('未解析保存被拦下', calls().filter((c) => c.path === '/api/schedules' && c.method === 'POST').length === beforeBadSave);
  check('未解析错误提示', !doc.querySelector('#sched-modal-error').classList.contains('hidden'));
  doc.querySelector('#sched-cancel').click();

  // ---------- 解析失败提示 ----------
  doc.querySelector('#sched-new').click();
  doc.querySelector('#sched-nl-text').value = 'bad';
  doc.querySelector('#sched-parse').click();
  await settle();
  check('解析失败显示原因', !doc.querySelector('#sched-parse-error').classList.contains('hidden') &&
    doc.querySelector('#sched-parse-error').textContent.includes('频率'));
  doc.querySelector('#sched-cancel').click();

  // 输入描述时不自动回填 prompt；只在解析成功后填一次。
  doc.querySelector('#sched-new').click();
  const autoNl = doc.querySelector('#sched-nl-text');
  autoNl.value = '每天9点巡检';
  autoNl.dispatchEvent(new w.Event('input', { bubbles: true }));
  check('输入描述时不回填 prompt', doc.querySelector('#sched-prompt').value === '',
    doc.querySelector('#sched-prompt').value);
  check('输入时最终预览仍回退描述', doc.querySelector('#sched-final-text').textContent === '每天9点巡检');
  doc.querySelector('#sched-parse').click();
  await settle();
  check('解析后 prompt = 描述', doc.querySelector('#sched-prompt').value === '每天9点巡检');
  const autoPrompt = doc.querySelector('#sched-prompt');
  autoPrompt.value = '';
  autoPrompt.dispatchEvent(new w.Event('input', { bubbles: true }));
  check('清空 prompt 不被自动回填', autoPrompt.value === '', autoPrompt.value);
  autoNl.value = '每天9点巡检看板';
  autoNl.dispatchEvent(new w.Event('input', { bubbles: true }));
  check('改描述后 prompt 仍为空（不再自动跟随）', autoPrompt.value === '', autoPrompt.value);
  check('改描述后清掉候选', candidateCards().length === 0);
  doc.querySelector('#sched-cancel').click();

  // 用户先手写 prompt 时，解析结果不覆盖它。
  doc.querySelector('#sched-new').click();
  const preNl = doc.querySelector('#sched-nl-text');
  preNl.value = '每天9点巡检';
  preNl.dispatchEvent(new w.Event('input', { bubbles: true }));
  const prePrompt = doc.querySelector('#sched-prompt');
  prePrompt.value = '我自己的 prompt';
  prePrompt.dispatchEvent(new w.Event('input', { bubbles: true }));
  doc.querySelector('#sched-parse').click();
  await settle();
  check('手写 prompt 解析后不被覆盖', prePrompt.value === '我自己的 prompt', prePrompt.value);
  doc.querySelector('#sched-cancel').click();

  // ---------- AI 解析 ----------
  const parserBtn = (p) => doc.querySelector('#sched-parser [data-sched-parser="' + p + '"]');
  doc.querySelector('#sched-new').click();
  parserBtn('ai').click();
  check('AI 提示可见', !doc.querySelector('#sched-ai-note').classList.contains('hidden'));
  check('AI 模式按钮文案', doc.querySelector('#sched-parse').textContent === 'AI 解析',
    doc.querySelector('#sched-parse').textContent);
  const aiNl = doc.querySelector('#sched-nl-text');
  aiNl.value = '每天9点按上周周报口径做看板巡检';
  aiNl.dispatchEvent(new w.Event('input', { bubbles: true }));
  check('AI 解析前输入不回填 prompt', doc.querySelector('#sched-prompt').value === '');
  doc.querySelector('#sched-parse').click();
  await settle();
  const aiCall = calls().find((c) => c.path === '/api/schedules/parse-ai');
  check('AI 解析发 POST parse-ai', !!aiCall && aiCall.body.text.includes('周报口径'));
  check('AI 结果填名称/cron', doc.querySelector('#sched-name').value === '看板巡检' &&
    doc.querySelector('#sched-cron').value === '0 9 * * *');
  check('AI prompt 覆盖描述', doc.querySelector('#sched-prompt').value.includes('work/tracking'),
    doc.querySelector('#sched-prompt').value);
  check('AI 理由显示', !doc.querySelector('#sched-ai-rationale').classList.contains('hidden') &&
    doc.querySelector('#sched-ai-rationale').textContent.includes('work/tracking'));
  check('AI 元信息显示耗时/tokens', doc.querySelector('#sched-ai-meta').textContent.includes('AI 解析完成') &&
    doc.querySelector('#sched-ai-meta').textContent.includes('tokens'),
    doc.querySelector('#sched-ai-meta').textContent);
  check('AI 候选带 AI 徽标', candidateCards()[0].textContent.includes('AI'));
  doc.querySelector('#sched-save').click();
  await settle();
  const aiCreate = calls().filter((c) => c.path === '/api/schedules' && c.method === 'POST').pop();
  check('AI 结果需用户确认后才保存', aiCreate && aiCreate.body.prompt.includes('work/tracking') &&
    aiCreate.body.cron === '0 9 * * *', JSON.stringify(aiCreate && aiCreate.body));

  // AI 失败 → 自动回退规则，并标注原因。
  doc.querySelector('#sched-new').click();
  parserBtn('ai').click();
  const fbNl = doc.querySelector('#sched-nl-text');
  fbNl.value = '每天9点巡检 fallback';
  fbNl.dispatchEvent(new w.Event('input', { bubbles: true }));
  doc.querySelector('#sched-parse').click();
  await settle();
  check('AI 失败提示回退', doc.querySelector('#sched-ai-meta').classList.contains('warn') &&
    doc.querySelector('#sched-ai-meta').textContent.includes('已用规则结果') &&
    doc.querySelector('#sched-ai-meta').textContent.includes('超时'),
    doc.querySelector('#sched-ai-meta').textContent);
  check('回退后仍有规则候选', candidateCards().length === 1 && doc.querySelector('#sched-cron').value === '0 9 * * *');
  doc.querySelector('#sched-cancel').click();

  // AI 需要补充信息：列出问题，且不能保存。
  doc.querySelector('#sched-new').click();
  parserBtn('ai').click();
  const askNl = doc.querySelector('#sched-nl-text');
  askNl.value = '每周按上周周报口径巡检 ask';
  askNl.dispatchEvent(new w.Event('input', { bubbles: true }));
  doc.querySelector('#sched-parse').click();
  await settle();
  check('AI 问题列表显示', !doc.querySelector('#sched-ai-questions').classList.contains('hidden') &&
    doc.querySelector('#sched-ai-questions').textContent.includes('哪一周'));
  check('待确认时没有候选', candidateCards().length === 0);
  const beforeAskSave = calls().filter((c) => c.path === '/api/schedules' && c.method === 'POST').length;
  doc.querySelector('#sched-save').click();
  await settle();
  check('待确认时保存被拦下', calls().filter((c) => c.path === '/api/schedules' && c.method === 'POST').length === beforeAskSave);
  doc.querySelector('#sched-cancel').click();

  // 并发上限错误展示。
  doc.querySelector('#sched-new').click();
  parserBtn('ai').click();
  const busyNl = doc.querySelector('#sched-nl-text');
  busyNl.value = '每天9点巡检 busy';
  busyNl.dispatchEvent(new w.Event('input', { bubbles: true }));
  doc.querySelector('#sched-parse').click();
  await settle();
  check('AI 并发错误提示', !doc.querySelector('#sched-parse-error').classList.contains('hidden') &&
    doc.querySelector('#sched-parse-error').textContent.includes('已有一个 AI 解析任务'));
  doc.querySelector('#sched-cancel').click();

  // ---------- cron 模式 ----------
  doc.querySelector('#sched-new').click();
  modeBtn('cron').click();
  check('cron 模式显示 cron 输入', !doc.querySelector('#sched-cron-block').classList.contains('hidden') &&
    doc.querySelector('#sched-nl-block').classList.contains('hidden'));
  check('cron 模式显示默认 cron', doc.querySelector('#sched-cron').value === '0 9 * * *');
  await settle();
  check('cron 模式显示下次执行预览', doc.querySelector('#sched-next-preview').textContent.includes('下次执行：2026-11-24 09:00'),
    doc.querySelector('#sched-next-preview').textContent);
  const example = [...doc.querySelectorAll('.sched-example')].find((b) => b.textContent === '每 5 分钟');
  example.click();
  check('示例填入 cron', doc.querySelector('#sched-cron').value === '*/5 * * * *');
  await settle();
  const cronEl = doc.querySelector('#sched-cron');
  cronEl.value = 'bad';
  cronEl.dispatchEvent(new w.Event('input', { bubbles: true }));
  await new Promise((r) => setTimeout(r, 350));
  await settle();
  check('非法 cron 预览标错', doc.querySelector('#sched-next-preview').classList.contains('bad'));
  cronEl.value = '0 21 * * *';
  doc.querySelector('#sched-name').value = '  晚间总结  ';
  doc.querySelector('#sched-prompt').value = '汇总今天的提交';
  doc.querySelector('#sched-target').value = 'w1';
  doc.querySelector('#sched-save').click();
  await settle();
  const cronCreate = calls().filter((c) => c.path === '/api/schedules' && c.method === 'POST').pop();
  check('cron 模式保存字段 trim 且完整', cronCreate && cronCreate.body.name === '晚间总结' &&
    cronCreate.body.cron === '0 21 * * *' && cronCreate.body.prompt === '汇总今天的提交' &&
    cronCreate.body.target_session === 'w1', JSON.stringify(cronCreate && cronCreate.body));

  // ---------- 编辑 ----------
  rowBtn(rows()[0], '编辑').click();
  check('编辑默认 cron 模式', modeBtn('cron').classList.contains('active'));
  check('编辑回填名称', doc.querySelector('#sched-name').value === '每日站会');
  doc.querySelector('#sched-name').value = '每日站会（改名）';
  doc.querySelector('#sched-save').click();
  await settle();
  const put = calls().find((c) => c.path === '/api/schedules/t1' && c.method === 'PUT');
  check('编辑发 PUT', !!put && put.body.name === '每日站会（改名）', JSON.stringify(put && put.body));

  // ---------- 空态 ----------
  w.__tasks = [];
  await w.schedulesRefresh();
  check('空态出现', !!doc.querySelector('#sched-list .sched-empty'));
  check('空态文案', doc.querySelector('#sched-list .sched-empty').textContent.includes('还没有定时任务'));
  const cta = doc.querySelector('#sched-list .sched-empty .primary-btn');
  cta.click();
  check('空态 CTA 打开新建弹窗', !modal().classList.contains('hidden'));

  w.schedStopPoll();
  w.close();
  console.log(`\nschedules: ${failed ? 'FAIL' : '全部通过'} (${failed} failed)`);
  process.exit(failed ? 1 : 0);
})().catch((e) => {
  console.error(e);
  process.exit(1);
});
