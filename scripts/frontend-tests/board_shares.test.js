// 看板产物公开分享前端回归：行内「分享」入口、创建弹窗（1/3/7/永久）、
// 复制公开链接、分享管理列表（状态/复制/取消）、空态，以及不破坏 legacy 行。
//
// 局限：jsdom 不做布局/剪贴板，clipboard 由 copyText stub 记录调用。
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
  <div id="board-drawer-artifacts"></div>
  <button id="board-share-manage"></button>
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
  <div id="board-shares-panel" class="hidden">
    <div id="board-shares-error" class="hidden"></div>
    <div id="board-shares-list"></div>
    <button id="board-shares-close"></button>
  </div>
</body></html>`;

const dom = new JSDOM(HTML, { runScripts: 'outside-only', url: 'http://deck.local/' });
const w = dom.window;
const doc = w.document;

w.eval(`
const $ = (sel) => document.querySelector(sel);
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
${grabFn('boardShareRow')}
${grabFn('boardRenderShares')}
${grabFn('boardLoadShares')}
${grabFn('boardRevokeShare')}
${grabFn('boardOpenSharesPanel')}
${grabFn('boardCloseSharesPanel')}
window.__artifactItem = boardArtifactItem;
window.__openShare = boardOpenShareDialog;
window.__closeShare = boardCloseShareDialog;
window.__createShare = boardCreateShare;
window.__copyShare = boardCopyShareURL;
window.__renderShares = boardRenderShares;
window.__loadShares = boardLoadShares;
window.__openPanel = boardOpenSharesPanel;
window.__closePanel = boardCloseSharesPanel;
`);

const tick = () => new Promise((resolve) => setTimeout(resolve, 0));
let failed = 0;
function check(name, cond, extra) {
  if (cond) console.log('  ok  ' + name);
  else { failed++; console.log('  FAIL ' + name + (extra ? '  →  ' + extra : '')); }
}

function artifact(extra) {
  return Object.assign({
    id: 'A-1', task_id: 'T-1', title: '验收报告', kind: 'html', filename: 'report.html',
    size: 128, created_at: '2026-11-11 12:00:00',
    url: 'http://deck.local/api/agent-board/artifacts/A-1',
  }, extra || {});
}

async function main() {
  console.log('看板产物公开分享');

  // 1. 行内入口：普通产物有「分享」，legacy 没有
  const item = w.__artifactItem(null, artifact());
  const shareBtn = [...item.querySelectorAll('button.board-artifact-action')]
    .find((b) => b.textContent === '分享');
  check('普通产物行有「分享」', !!shareBtn);
  const legacy = w.__artifactItem(null, artifact({ id: 'legacy-report', legacy: true }));
  check('legacy 产物没有「分享」', ![...legacy.querySelectorAll('button')]
    .some((b) => b.textContent === '分享'));

  // 2. 打开弹窗
  shareBtn.click();
  const modal = doc.querySelector('#board-share-modal');
  check('分享弹窗打开', !modal.classList.contains('hidden'));
  check('弹窗显示产物标题/类型',
    doc.querySelector('#board-share-artifact').textContent.includes('验收报告')
    && doc.querySelector('#board-share-artifact').textContent.includes('HTML'));
  check('默认有效期 1 天', doc.querySelector('#board-share-expiry').value === '1');
  check('结果区默认隐藏', doc.querySelector('#board-share-result').classList.contains('hidden'));

  // 3. 创建分享：POST 带 expiry_days，返回 URL 可复制
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
  check('结果区可见', !doc.querySelector('#board-share-result').classList.contains('hidden'));
  check('创建按钮恢复可用',
    !doc.querySelector('#board-share-create').disabled
    && doc.querySelector('#board-share-create').textContent === '创建公开链接');

  const copyBtn = doc.querySelector('#board-share-copy');
  w.__copyShare(doc.querySelector('#board-share-url').value, copyBtn);
  await tick();
  check('复制公开链接', w.__copied[w.__copied.length - 1] === 'https://deck.example/s/PUBLICTOKEN');
  check('复制按钮反馈', copyBtn.textContent === '已复制');

  // 4. 创建失败要显示错误，而不是把弹窗关掉
  w.__apiHandler = () => { throw new Error('boom'); };
  await w.__createShare();
  await tick();
  check('创建失败显示错误', doc.querySelector('#board-share-error').textContent.includes('boom')
    && !doc.querySelector('#board-share-error').classList.contains('hidden'));
  check('失败后弹窗仍打开', !modal.classList.contains('hidden'));
  w.__closeShare();
  check('关闭弹窗', modal.classList.contains('hidden')
    && doc.querySelector('#board-share-url').value === '');

  // 5. 分享管理：有效/已过期/已取消 + 复制/取消动作
  let shares = [
    {
      id: 'S-1', artifact_id: 'A-1', artifact_title: '验收报告', task_title: '任务A',
      status: 'active', active: true, url: 'https://deck.example/s/T1',
      created_at: '2026-11-11 12:00:00', expires_at: '',
    },
    {
      id: 'S-2', artifact_id: 'A-2', artifact_title: '旧报告', status: 'expired',
      active: false, created_at: '2026-11-10 12:00:00', expires_at: '2026-11-11 12:00:00',
    },
    {
      id: 'S-3', artifact_id: 'A-3', artifact_title: '已取消报告', status: 'revoked',
      active: false, created_at: '2026-11-09 12:00:00', revoked_at: '2026-11-09 13:00:00',
    },
  ];
  const deleted = [];
  w.__apiHandler = (reqPath, opts) => {
    if (reqPath === '/api/agent-board/shares' && (!opts.method || opts.method === 'GET')) {
      return { ok: true, shares };
    }
    if (reqPath.startsWith('/api/agent-board/shares/') && opts.method === 'DELETE') {
      deleted.push(reqPath.split('/').pop());
      shares = shares.map((s) => (s.id === 'S-1'
        ? Object.assign({}, s, { status: 'revoked', active: false, url: undefined, revoked_at: 'now' })
        : s));
      return { ok: true };
    }
    throw new Error('unexpected api ' + reqPath);
  };
  w.__openPanel();
  await tick();
  await tick();
  check('管理面板打开', !doc.querySelector('#board-shares-panel').classList.contains('hidden'));
  const rows = [...doc.querySelectorAll('#board-shares-list .board-share-row')];
  check('列出全部分享', rows.length === 3, String(rows.length));
  check('状态文案',
    [...doc.querySelectorAll('.board-share-status')].map((el) => el.textContent).join(',')
      === '有效,已过期,已取消');
  check('只有有效链接可复制',
    rows[0].querySelectorAll('.board-artifact-action').length === 2
    && rows[1].querySelectorAll('.board-artifact-action').length === 1
    && rows[2].querySelectorAll('.board-artifact-action').length === 0);
  check('管理列表展示任务/创建/到期',
    rows[0].querySelector('.board-share-row-meta').textContent.includes('任务：任务A')
    && rows[0].querySelector('.board-share-row-meta').textContent.includes('创建 2026-11-11')
    && rows[0].querySelector('.board-share-row-meta').textContent.includes('永久有效'));
  check('管理列表复制走同一个链接', (() => {
    const btn = [...rows[0].querySelectorAll('button')].find((b) => b.textContent === '复制链接');
    btn.click();
    return w.__copied[w.__copied.length - 1] === 'https://deck.example/s/T1';
  })());

  // 6. 取消分享
  const revoke = rows[0].querySelector('.board-share-revoke');
  revoke.click();
  await tick();
  await tick();
  check('DELETE 对应 share id', deleted[0] === 'S-1', JSON.stringify(deleted));
  check('取消后列表刷新为已取消',
    doc.querySelector('#board-shares-list .board-share-status').textContent === '已取消');
  check('取消后该行不再显示取消按钮',
    !doc.querySelector('#board-shares-list .board-share-row[data-share-id="S-1"] .board-share-revoke'));

  // 7. 空态
  shares = [];
  await w.__loadShares();
  await tick();
  check('空态显示「暂无分享链接」',
    doc.querySelector('#board-shares-list').textContent.includes('暂无分享链接'));

  // 8. CSS 契约
  const css = fs.readFileSync(path.join(WEB, 'style.css'), 'utf8');
  check('CSS: 分享遮罩层级高于产物预览',
    /\.board-share-backdrop\s*\{[^}]*z-index:\s*280/s.test(css));
  check('CSS: 隐藏态 display:none',
    /\.board-share-backdrop\.hidden\s*\{\s*display:\s*none/s.test(css));
  check('CSS: 管理列表可滚动且不撑破弹窗',
    /\.board-shares-list\s*\{[^}]*overflow-y:\s*auto/s.test(css));

  console.log(failed ? `\n${failed} 项失败` : '\n全部通过');
  process.exit(failed ? 1 : 0);
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
