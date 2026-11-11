// 看板产物公开分享前端回归：
// - 产物行「分享」入口 + 创建弹窗（1/3/7/永久）/复制，不破坏 legacy 行；
// - 设置页「分享管理」入口 → 独立页面（不是弹窗/抽屉），跨任务列表；
// - 搜索/状态/任务项目/类型筛选、排序、计数、复制、撤销、空态、hash 路由。
//
// 局限：jsdom 不做布局/剪贴板；copyText 用 stub 记录调用。
const fs = require('fs');
const path = require('path');
const { JSDOM } = require('jsdom');

const WEB = path.resolve(__dirname, '../../web');
const src = fs.readFileSync(path.join(WEB, 'app.js'), 'utf8');

function grabFn(name) {
  const found = src.indexOf('function ' + name + '(');
  if (found < 0) throw new Error('找不到函数 ' + name);
  // Include the `async ` keyword when the function is async.
  const s = src.slice(Math.max(0, found - 6), found) === 'async ' ? found - 6 : found;
  const b = src.indexOf('{', found);
  let d = 0, q = null;
  for (let i = b; i < src.length; i++) {
    const c = src[i], p = src[i - 1];
    if (q) { if (c === q && p !== '\\') q = null; continue; }
    if (c === '"' || c === "'" || c === '`') { q = c; continue; }
    if (c === '{') d++;
    else if (c === '}') { d--; if (!d) return src.slice(s, i + 1); }
  }
  throw new Error(name + ' 括号不配对');
}

const HTML = `<!doctype html><html><body>
  <div id="empty-state"></div>
  <div id="chat-view" class="hidden"></div>
  <div id="new-session-view" class="hidden"></div>
  <div id="group-view" class="hidden"></div>
  <div id="board-drawer-artifacts"></div>
  <div id="modal-settings" class="hidden">
    <button id="btn-settings-shares"></button>
  </div>
  <div id="shares-view" class="hidden">
    <header id="shares-header">
      <div class="shares-title-row">
        <button id="shares-back"></button>
        <span id="shares-count"></span>
        <button id="shares-refresh"></button>
      </div>
      <div class="shares-toolbar">
        <input id="shares-search">
        <select id="shares-status-filter">
          <option value="">全部状态</option>
          <option value="active">有效</option>
          <option value="expired">已过期</option>
          <option value="revoked">已取消</option>
        </select>
        <select id="shares-scope-filter"><option value="">全部任务/项目</option></select>
        <select id="shares-kind-filter">
          <option value="">全部类型</option>
          <option value="html">HTML</option>
          <option value="markdown">Markdown</option>
          <option value="file">文件</option>
          <option value="url">链接</option>
        </select>
        <select id="shares-sort">
          <option value="created_desc">创建时间（新→旧）</option>
          <option value="created_asc">创建时间（旧→新）</option>
          <option value="expires_asc">到期时间（近→远，永久最后）</option>
          <option value="expires_desc">到期时间（远→近，永久最前）</option>
        </select>
      </div>
    </header>
    <div id="shares-body">
      <div id="shares-summary"></div>
      <div id="shares-error" class="hidden"></div>
      <div id="shares-list"></div>
    </div>
  </div>
  <div id="board-share-modal" class="hidden">
    <div id="board-share-artifact"></div>
    <select id="board-share-expiry">
      <option value="1">1 天</option><option value="3">3 天</option>
      <option value="7">7 天</option><option value="0">永久</option>
    </select>
    <button id="board-share-create"></button>
    <div id="board-share-result" class="hidden">
      <input id="board-share-url" readonly>
      <button id="board-share-copy"></button>
      <div id="board-share-expiry-note"></div>
    </div>
    <div id="board-share-error" class="hidden"></div>
    <button id="board-share-close"></button>
  </div>
</body></html>`;

const dom = new JSDOM(HTML, { runScripts: 'outside-only', url: 'http://deck.local/' });
const w = dom.window;
const doc = w.document;

w.eval(`
const $ = (sel) => document.querySelector(sel);
const state = { current: null, currentGroup: null };
function renderIcons() {}
function boardDisconnect() {}
function hideAllViews() {
  ['#empty-state', '#chat-view', '#new-session-view', '#group-view', '#shares-view']
    .forEach((sel) => { const el = $(sel); if (el) el.classList.add('hidden'); });
}
function boardMuted(text) {
  const el = document.createElement('div');
  el.className = 'board-pane-muted';
  el.textContent = text;
  return el;
}
function boardCopyButton(text, label = '复制') {
  const b = document.createElement('button');
  b.type = 'button';
  b.className = 'board-copy-btn';
  b.textContent = label;
  b.dataset.value = text;
  return b;
}
window.__apiCalls = [];
window.__copied = [];
window.__apiHandler = () => ({});
async function api(path, opts = {}) {
  window.__apiCalls.push({ path, opts });
  return window.__apiHandler(path, opts);
}
function copyText(text) {
  window.__copied.push(text);
  return Promise.resolve();
}
const SHARES_HASH = '#/shares';
let sharesAdminItems = [];
let sharesAdminReturn = 'empty';
const sharesAdminFilters = { q: '', status: '', scope: '', kind: '', sort: 'created_desc' };
${grabFn('boardArtifactSize')}
${grabFn('boardArtifactKindLabel')}
${grabFn('boardArtifactPreviewable')}
${grabFn('boardArtifactAction')}
${grabFn('boardArtifactItem')}
${grabFn('boardShareStatusLabel')}
${grabFn('boardShareShowError')}
${grabFn('boardShareBusy')}
${grabFn('boardOpenShareDialog')}
${grabFn('boardCloseShareDialog')}
${grabFn('boardCreateShare')}
${grabFn('boardCopyShareURL')}
${grabFn('sharesAdminVisible')}
${grabFn('clearSharesHash')}
${grabFn('sharesAdminEmpty')}
${grabFn('sharesAdminCurrentView')}
${grabFn('openSharesAdmin')}
${grabFn('closeSharesAdmin')}
${grabFn('sharesAdminOpenFromSettings')}
${grabFn('sharesAdminBindControls')}
${grabFn('sharesAdminStatusCounts')}
${grabFn('sharesAdminPopulateScope')}
${grabFn('sharesAdminFiltered')}
${grabFn('sharesAdminRenderSummary')}
${grabFn('sharesAdminRow')}
${grabFn('sharesAdminRender')}
${grabFn('sharesAdminRefresh')}
${grabFn('boardRevokeShare')}
sharesAdminBindControls();
window.__artifactItem = boardArtifactItem;
window.__openShare = boardOpenShareDialog;
window.__closeShare = boardCloseShareDialog;
window.__createShare = boardCreateShare;
window.__copyShare = boardCopyShareURL;
window.__setShares = (items) => { sharesAdminItems = items; sharesAdminRender(); };
window.__filters = sharesAdminFilters;
window.__openShares = openSharesAdmin;
window.__closeShares = closeSharesAdmin;
window.__settingsEntry = sharesAdminOpenFromSettings;
window.__refreshShares = sharesAdminRefresh;
window.__renderShares = sharesAdminRender;
`);

const tick = () => new Promise((resolve) => setTimeout(resolve, 0));
const settle = async () => { await tick(); await tick(); };
let failed = 0;
function check(name, cond, extra) {
  if (cond) console.log('  ok  ' + name);
  else { failed++; console.log('  FAIL ' + name + (extra ? '  →  ' + extra : '')); }
}

function makeShare(extra) {
  return Object.assign({
    id: 'S-1', artifact_id: 'A-1', artifact_title: '验收报告', artifact_kind: 'html',
    artifact_filename: 'report.html', task_id: 'T-1', task_title: '任务A', project: '项目A',
    status: 'active', active: true, url: 'https://deck.example/s/T1',
    created_at: '2026-11-11 12:00:00', expires_at: '', created_by: 'local',
  }, extra || {});
}

function sharesFixture() {
  return [
    makeShare(),
    makeShare({
      id: 'S-2', artifact_id: 'A-2', artifact_title: '旧报告', artifact_kind: 'markdown',
      artifact_filename: 'old.md', task_id: 'T-2', task_title: '任务B', project: '项目B',
      status: 'expired', active: false, url: undefined,
      created_at: '2026-11-10 12:00:00', expires_at: '2026-11-11 12:00:00',
    }),
    makeShare({
      id: 'S-3', artifact_id: 'A-3', artifact_title: '已取消报告', artifact_kind: 'html',
      artifact_filename: 'revoked.html', task_id: 'T-3', task_title: '任务C', project: '项目A',
      status: 'revoked', active: false, url: undefined, revoked_at: '2026-11-09 13:00:00',
      created_at: '2026-11-09 12:00:00', expires_at: '2026-11-12 12:00:00',
    }),
    makeShare({
      id: 'S-4', artifact_id: 'A-4', artifact_title: '数据包', artifact_kind: 'file',
      artifact_filename: 'bundle.zip', task_id: 'T-1', task_title: '任务A', project: '项目A',
      url: 'https://deck.example/s/T4',
      created_at: '2026-11-11 13:00:00', expires_at: '2026-11-20 12:00:00',
    }),
  ];
}

function resetFilters() {
  Object.assign(w.__filters, { q: '', status: '', scope: '', kind: '', sort: 'created_desc' });
  doc.querySelector('#shares-search').value = '';
  doc.querySelector('#shares-status-filter').value = '';
  doc.querySelector('#shares-scope-filter').value = '';
  doc.querySelector('#shares-kind-filter').value = '';
  doc.querySelector('#shares-sort').value = 'created_desc';
}

function rows() {
  return [...doc.querySelectorAll('#shares-list .shares-row:not(.shares-row-head)')];
}

async function main() {
  console.log('看板产物公开分享');

  // 1. 产物行入口：普通产物有「分享」，legacy 没有
  const item = w.__artifactItem(null, {
    id: 'A-1', task_id: 'T-1', title: '验收报告', kind: 'html', filename: 'report.html',
    size: 128, url: 'http://deck.local/api/agent-board/artifacts/A-1',
  });
  const shareBtn = [...item.querySelectorAll('button.board-artifact-action')]
    .find((b) => b.textContent === '分享');
  check('普通产物行有「分享」', !!shareBtn);
  const legacy = w.__artifactItem(null, {
    id: 'legacy-report', title: '旧报告', kind: 'html', filename: 'old.html', legacy: true,
  });
  check('legacy 产物没有「分享」', ![...legacy.querySelectorAll('button')]
    .some((b) => b.textContent === '分享'));

  // 2. 创建弹窗（保持原有行为）
  shareBtn.click();
  const modal = doc.querySelector('#board-share-modal');
  check('分享弹窗打开', !modal.classList.contains('hidden'));
  check('弹窗显示产物标题/类型',
    doc.querySelector('#board-share-artifact').textContent.includes('验收报告')
    && doc.querySelector('#board-share-artifact').textContent.includes('HTML'));

  let createBody = null;
  w.__apiHandler = (reqPath, opts) => {
    if (reqPath === '/api/agent-board/artifacts/A-1/shares' && opts.method === 'POST') {
      createBody = JSON.parse(opts.body);
      return {
        ok: true,
        share: {
          id: 'S-1', artifact_id: 'A-1', status: 'active', active: true,
          url: 'https://deck.example/s/PUBLICTOKEN', expires_at: '2026-11-14 12:00:00',
        },
      };
    }
    throw new Error('unexpected api ' + reqPath);
  };
  doc.querySelector('#board-share-expiry').value = '3';
  await w.__createShare();
  await tick();
  check('创建请求 expiry_days=3', createBody && createBody.expiry_days === 3,
    JSON.stringify(createBody));
  check('结果区显示公开 URL',
    doc.querySelector('#board-share-url').value === 'https://deck.example/s/PUBLICTOKEN');
  check('显示到期时间',
    doc.querySelector('#board-share-expiry-note').textContent.includes('2026-11-14 12:00:00'));
  const copyBtn = doc.querySelector('#board-share-copy');
  w.__copyShare(doc.querySelector('#board-share-url').value, copyBtn);
  await tick();
  check('复制公开链接',
    w.__copied[w.__copied.length - 1] === 'https://deck.example/s/PUBLICTOKEN');
  w.__closeShare();
  check('关闭创建弹窗并清空 URL', modal.classList.contains('hidden')
    && doc.querySelector('#board-share-url').value === '');

  // 3. 设置页入口 → 独立页面（不是弹窗），并加载跨任务列表
  let sharesData = sharesFixture();
  const deleted = [];
  w.__apiHandler = (reqPath, opts) => {
    if (reqPath === '/api/agent-board/shares' && (!opts.method || opts.method === 'GET')) {
      return { ok: true, shares: sharesData };
    }
    if (reqPath.startsWith('/api/agent-board/shares/') && opts.method === 'DELETE') {
      deleted.push(reqPath.split('/').pop());
      sharesData = sharesData.map((s) => (s.id === 'S-1'
        ? Object.assign({}, s, { status: 'revoked', active: false, url: undefined, revoked_at: 'now' })
        : s));
      return { ok: true };
    }
    throw new Error('unexpected api ' + reqPath);
  };
  doc.querySelector('#modal-settings').classList.remove('hidden');
  doc.querySelector('#btn-settings-shares').click();
  await settle();
  check('设置弹窗被关闭', doc.querySelector('#modal-settings').classList.contains('hidden'));
  check('进入分享管理独立页', !doc.querySelector('#shares-view').classList.contains('hidden'));
  check('独立页不是弹窗/抽屉（无 .modal 包裹）',
    !doc.querySelector('#shares-view').classList.contains('modal')
    && !doc.querySelector('#shares-view').closest('.modal'));
  check('hash 路由为 #/shares', w.location.hash === '#/shares');
  check('跨任务列表渲染 4 条', rows().length === 4, String(rows().length));
  check('列表包含多个任务/项目',
    new Set(rows().map((r) => r.querySelector('.shares-task .shares-row-title').textContent)).size === 3);
  check('状态计数',
    [...doc.querySelectorAll('#shares-summary .shares-count-chip')]
      .map((el) => el.textContent).join(',') === '全部 4,有效 2,已过期 1,已取消 1');
  check('可见条数', doc.querySelector('#shares-count').textContent === '共 4 条');

  // 4. 行字段：产物/任务/链接/时间/状态
  const row1 = doc.querySelector('#shares-list .shares-row[data-share-id="S-1"]');
  check('行有产物标题/类型/文件名',
    row1.querySelector('.shares-artifact .shares-row-title').textContent === '验收报告'
    && row1.querySelector('.shares-artifact .shares-row-meta').textContent.includes('HTML')
    && row1.querySelector('.shares-artifact .shares-row-meta').textContent.includes('report.html'));
  check('行有任务标题/卡号/项目',
    row1.querySelector('.shares-task .shares-row-title').textContent === '任务A'
    && row1.querySelector('.shares-task .shares-row-meta').textContent.includes('T-1')
    && row1.querySelector('.shares-task .shares-row-meta').textContent.includes('项目A'));
  check('行有分享链接/创建/到期',
    row1.querySelector('.shares-link .shares-url').href === 'https://deck.example/s/T1'
    && row1.querySelector('.shares-link .shares-row-meta').textContent.includes('创建 2026-11-11 12:00:00')
    && row1.querySelector('.shares-link .shares-row-meta').textContent.includes('永久有效'));
  check('行有状态徽章', row1.querySelector('.board-share-status').textContent === '有效');
  check('失效行不显示可点击链接',
    doc.querySelector('#shares-list .shares-row[data-share-id="S-2"] .shares-url').tagName !== 'A');

  // 5. 复制
  const rowCopy = [...row1.querySelectorAll('button')].find((b) => b.textContent === '复制链接');
  rowCopy.click();
  check('列表复制链接', w.__copied[w.__copied.length - 1] === 'https://deck.example/s/T1');

  // 6. 搜索
  const search = doc.querySelector('#shares-search');
  search.value = '旧报告';
  search.dispatchEvent(new w.Event('input'));
  check('搜索标题', rows().length === 1 && rows()[0].dataset.shareId === 'S-2');
  check('搜索后可见条数', doc.querySelector('#shares-count').textContent === '显示 1 / 4 条');
  search.value = 'T-3';
  search.dispatchEvent(new w.Event('input'));
  check('搜索卡号', rows().length === 1 && rows()[0].dataset.shareId === 'S-3');
  search.value = '';
  search.dispatchEvent(new w.Event('input'));

  // 7. 状态筛选（计数仍显示全量）
  const statusFilter = doc.querySelector('#shares-status-filter');
  statusFilter.value = 'expired';
  statusFilter.dispatchEvent(new w.Event('change'));
  check('状态筛选 expired', rows().length === 1 && rows()[0].dataset.shareId === 'S-2');
  check('筛选不改变全局计数',
    doc.querySelector('#shares-summary .shares-count-chip').textContent === '全部 4');
  statusFilter.value = '';
  statusFilter.dispatchEvent(new w.Event('change'));

  // 8. 任务/项目筛选
  const scopeFilter = doc.querySelector('#shares-scope-filter');
  check('任务/项目筛选选项含项目与任务',
    [...scopeFilter.options].some((o) => o.value === 'project:项目A')
    && [...scopeFilter.options].some((o) => o.value === 'task:T-2'));
  scopeFilter.value = 'project:项目A';
  scopeFilter.dispatchEvent(new w.Event('change'));
  check('按项目筛选', rows().length === 3, String(rows().length));
  scopeFilter.value = 'task:T-2';
  scopeFilter.dispatchEvent(new w.Event('change'));
  check('按任务筛选', rows().length === 1 && rows()[0].dataset.shareId === 'S-2');
  scopeFilter.value = '';
  scopeFilter.dispatchEvent(new w.Event('change'));

  // 9. 类型筛选
  const kindFilter = doc.querySelector('#shares-kind-filter');
  kindFilter.value = 'markdown';
  kindFilter.dispatchEvent(new w.Event('change'));
  check('按产物类型筛选', rows().length === 1 && rows()[0].dataset.shareId === 'S-2');
  kindFilter.value = '';
  kindFilter.dispatchEvent(new w.Event('change'));

  // 10. 排序
  const sort = doc.querySelector('#shares-sort');
  sort.value = 'created_desc';
  sort.dispatchEvent(new w.Event('change'));
  check('创建时间倒序', rows()[0].dataset.shareId === 'S-4');
  sort.value = 'expires_asc';
  sort.dispatchEvent(new w.Event('change'));
  check('到期时间近→远（永久最后）',
    rows().map((r) => r.dataset.shareId).join(',') === 'S-2,S-3,S-4,S-1',
    rows().map((r) => r.dataset.shareId).join(','));
  sort.value = 'expires_desc';
  sort.dispatchEvent(new w.Event('change'));
  check('到期时间远→近（永久最前）', rows()[0].dataset.shareId === 'S-1');
  sort.value = 'created_desc';
  sort.dispatchEvent(new w.Event('change'));

  // 11. 撤销：DELETE 后立即重新拉取并更新状态/计数
  doc.querySelector('#shares-list .shares-row[data-share-id="S-1"] .board-share-revoke').click();
  await settle();
  check('DELETE 对应 share id', deleted[0] === 'S-1', JSON.stringify(deleted));
  const revokedRow = doc.querySelector('#shares-list .shares-row[data-share-id="S-1"]');
  check('撤销后状态更新', revokedRow.querySelector('.board-share-status').textContent === '已取消');
  check('撤销后不再显示取消按钮', !revokedRow.querySelector('.board-share-revoke'));
  check('撤销后计数更新',
    [...doc.querySelectorAll('#shares-summary .shares-count-chip')]
      .map((el) => el.textContent).join(',') === '全部 4,有效 1,已过期 1,已取消 2');

  // 12. 空态
  w.__setShares([]);
  check('无分享时显示引导空态',
    doc.querySelector('#shares-list').textContent.includes('还没有分享链接'));
  w.__setShares(sharesFixture());
  const search2 = doc.querySelector('#shares-search');
  search2.value = '不存在的产物';
  search2.dispatchEvent(new w.Event('input'));
  check('筛选无结果显示匹配空态',
    doc.querySelector('#shares-list').textContent.includes('没有匹配的分享链接'));
  search2.value = '';
  search2.dispatchEvent(new w.Event('input'));

  // 13. 返回：独立页关闭、hash 清理、回到空态（无当前会话）
  doc.querySelector('#shares-back').click();
  await tick();
  check('返回后关闭独立页', doc.querySelector('#shares-view').classList.contains('hidden'));
  check('返回后清理 hash', w.location.hash === '');
  check('返回后回到空态', !doc.querySelector('#empty-state').classList.contains('hidden'));

  // 14. hash 直接访问路径
  w.__openShares();
  await settle();
  check('openSharesAdmin 设置 #/shares', w.location.hash === '#/shares');
  w.__closeShares();
  check('closeSharesAdmin 清理 hash', w.location.hash === '');
  check('打开时隐藏其它主视图', doc.querySelector('#chat-view').classList.contains('hidden'));

  // 15. CSS 契约
  const css = fs.readFileSync(path.join(WEB, 'style.css'), 'utf8');
  check('CSS: 独立页是纵向 flex 视图',
    /#shares-view\s*\{[^}]*display:\s*flex[^}]*flex-direction:\s*column/s.test(css));
  check('CSS: 列表行是 grid（桌面五列）',
    /\.shares-row\s*\{[^}]*display:\s*grid[^}]*grid-template-columns/s.test(css));
  check('CSS: 窄屏列表切换为堆叠',
    /@media \(max-width: 900px\)[\s\S]*?\.shares-row\s*\{[^}]*grid-template-columns/s.test(css));
  check('CSS: 状态计数有 active/expired/revoked 配色',
    /\.shares-count-chip\.active/.test(css)
    && /\.shares-count-chip\.expired/.test(css)
    && /\.shares-count-chip\.revoked/.test(css));
  check('CSS: 旧的抽屉内分享列表样式已移除',
    !/\.board-shares-list\s*\{/.test(css));

  console.log(failed ? `\n${failed} 项失败` : '\n全部通过');
  process.exit(failed ? 1 : 0);
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
