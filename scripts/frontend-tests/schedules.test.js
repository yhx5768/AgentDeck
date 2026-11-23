// 定时任务页（web/schedules.js）回归：
// - 列表渲染：名称 / cron / 目标 / 上次 / 下次 / 状态 / 启停标记；
// - 行内启停、立即执行、删除（confirm 后 DELETE）；
// - 新建 / 编辑弹窗：目标默认主 Agent、cron 示例、下次执行预览（服务端预览）、
//   保存走 POST/PUT；空态 CTA。
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
    </div>
  </div>
  <div id="modal-schedule" class="modal hidden">
    <div class="modal-card">
      <h3 id="sched-modal-title"></h3>
      <input id="sched-name">
      <input id="sched-cron">
      <div id="sched-cron-examples"></div>
      <div id="sched-next-preview"></div>
      <textarea id="sched-prompt"></textarea>
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
  if (path === '/api/schedules/preview') {
    if (!body || String(body.cron || '').includes('bad') || String(body.cron || '').trim().split(/\\s+/).length !== 5) {
      return Promise.reject(new Error('cron 需要 5 个字段（分 时 日 月 周），当前 1 个'));
    }
    return Promise.resolve({ next_run_local: '2026-11-24 09:00', timezone: 'CST' });
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
`;

w.eval(STUBS + '\n' + src + '\nschedBind();');

const tick = () => new Promise((resolve) => setTimeout(resolve, 0));
const settle = async () => { await tick(); await tick(); };
let failed = 0;
function check(name, cond, extra) {
  if (cond) console.log('  ok  ' + name);
  else { failed++; console.log('  FAIL ' + name + (extra ? '  →  ' + extra : '')); }
}
const rows = () => [...doc.querySelectorAll('#sched-list .sched-row')];
const rowBtn = (row, label) => [...row.querySelectorAll('.sched-btn')].find((b) => b.textContent === label);
const calls = () => w.__apiCalls;
const lastCall = () => calls()[calls().length - 1];

(async () => {
  w.__tasks = [
    {
      id: 't1', name: '每日站会', cron: '0 9 * * *', prompt: '总结昨天的工作\\n第二行',
      target_session: 'main', target_name: '主 Agent', target_address: 'mainAgent',
      enabled: true, last_run_at: '2026-11-22T01:00:00Z', last_status: 'ok',
      next_run_at: '2026-11-24T01:00:00Z',
    },
    {
      id: 't2', name: '巡检', cron: '*/5 * * * *', prompt: '检查构建',
      target_session: 'w1', target_name: 'w·巡检',
      enabled: false, last_status: 'error: 目标会话不存在',
    },
  ];

  // 打开页面：隐藏其它视图、拉列表、渲染两行。
  w.schedulesOpen();
  await settle();
  check('打开后显示定时任务页', !doc.querySelector('#schedules-view').classList.contains('hidden'));
  check('计数 = 2 个任务 / 1 个启用', doc.querySelector('#sched-count').textContent === '2 个任务 · 1 个启用',
    doc.querySelector('#sched-count').textContent);
  check('渲染两行', rows().length === 2, String(rows().length));
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

  // 行内启停。
  rowBtn(r2, '启用').click();
  await settle();
  const toggle = calls().find((c) => c.path === '/api/schedules/t2/enable');
  check('启用发 POST enable', !!toggle && toggle.method === 'POST' && toggle.body.enabled === true);
  check('启停后重拉列表', calls().filter((c) => c.path === '/api/schedules' && c.method === 'GET').length >= 2);

  // 立即执行。
  const beforeRuns = calls().filter((c) => c.path.endsWith('/run')).length;
  rowBtn(rows()[0], '立即执行').click();
  await settle();
  check('立即执行发 POST run', calls().filter((c) => c.path.endsWith('/run')).length === beforeRuns + 1 &&
    lastCall().path === '/api/schedules' /* 执行后刷新是最后一个请求 */);

  // 删除：confirm 文本带任务名，确认后 DELETE。
  rowBtn(rows()[0], '删除').click();
  await settle();
  check('删除前 confirm 带任务名', w.__confirmText.includes('每日站会'), w.__confirmText);
  const del = calls().find((c) => c.path === '/api/schedules/t1' && c.method === 'DELETE');
  check('删除发 DELETE', !!del);

  // 新建弹窗：默认目标主 Agent、默认 cron、预览走服务端。
  doc.querySelector('#sched-new').click();
  check('新建弹窗打开', !doc.querySelector('#modal-schedule').classList.contains('hidden'));
  check('新建标题', doc.querySelector('#sched-modal-title').textContent === '新建定时任务');
  check('默认 cron 0 9 * * *', doc.querySelector('#sched-cron').value === '0 9 * * *');
  check('默认目标主 Agent', doc.querySelector('#sched-target').value === 'main');
  await settle();
  check('显示下次执行预览', doc.querySelector('#sched-next-preview').textContent.includes('下次执行：2026-11-24 09:00'),
    doc.querySelector('#sched-next-preview').textContent);
  check('预览请求发到 /api/schedules/preview', calls().some((c) => c.path === '/api/schedules/preview'));

  // cron 示例按钮填入并刷新预览。
  const example = [...doc.querySelectorAll('.sched-example')].find((b) => b.textContent === '每 5 分钟');
  example.click();
  check('示例填入 cron', doc.querySelector('#sched-cron').value === '*/5 * * * *');
  await settle();

  // 非法 cron 的预览错误提示（不阻断输入）。
  const cronEl = doc.querySelector('#sched-cron');
  cronEl.value = 'bad';
  cronEl.dispatchEvent(new w.Event('input', { bubbles: true }));
  await new Promise((r) => setTimeout(r, 350));
  await settle();
  check('非法 cron 预览标错', doc.querySelector('#sched-next-preview').classList.contains('bad') &&
    doc.querySelector('#sched-next-preview').textContent.includes('cron 无效'));

  // 保存：POST /api/schedules，字段正确并关闭弹窗。
  cronEl.value = '0 21 * * *';
  doc.querySelector('#sched-name').value = '  晚间总结  ';
  doc.querySelector('#sched-prompt').value = '汇总今天的提交';
  doc.querySelector('#sched-target').value = 'w1';
  doc.querySelector('#sched-enabled').checked = true;
  doc.querySelector('#sched-save').click();
  await settle();
  const create = calls().find((c) => c.path === '/api/schedules' && c.method === 'POST');
  check('保存发 POST /api/schedules', !!create);
  check('保存字段 trim 且完整', create && create.body.name === '晚间总结' && create.body.cron === '0 21 * * *' &&
    create.body.prompt === '汇总今天的提交' && create.body.target_session === 'w1' && create.body.enabled === true,
    JSON.stringify(create && create.body));
  check('保存成功后关闭弹窗', doc.querySelector('#modal-schedule').classList.contains('hidden'));

  // 编辑：打开第一行，值回填，保存走 PUT。
  rowBtn(rows()[0], '编辑').click();
  check('编辑标题', doc.querySelector('#sched-modal-title').textContent === '编辑定时任务');
  check('编辑回填名称', doc.querySelector('#sched-name').value === '每日站会');
  doc.querySelector('#sched-name').value = '每日站会（改名）';
  doc.querySelector('#sched-save').click();
  await settle();
  const put = calls().find((c) => c.path === '/api/schedules/t1' && c.method === 'PUT');
  check('编辑发 PUT', !!put && put.body.name === '每日站会（改名）', JSON.stringify(put && put.body));

  // 空态 + CTA。
  w.__tasks = [];
  await w.schedulesRefresh();
  check('空态出现', !!doc.querySelector('#sched-list .sched-empty'));
  check('空态文案', doc.querySelector('#sched-list .sched-empty').textContent.includes('还没有定时任务'));
  const cta = doc.querySelector('#sched-list .sched-empty .primary-btn');
  cta.click();
  check('空态 CTA 打开新建弹窗', !doc.querySelector('#modal-schedule').classList.contains('hidden'));

  w.schedStopPoll();
  w.close();
  console.log(`\nschedules: ${failed ? 'FAIL' : '全部通过'} (${failed} failed)`);
  process.exit(failed ? 1 : 0);
})().catch((e) => {
  console.error(e);
  process.exit(1);
});
