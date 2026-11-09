// 看板「产物与证据」前端回归：多产物列表（标题/类型/大小/来源/时间）、
// 打开/预览/下载/复制链接、legacy report_file 兼容、HTML 预览的 sandbox 边界。
//
// 局限：jsdom 不做布局，只能钉 DOM 契约；真实 iframe 加载与移动端排版用
// headless Chrome 另验。
const fs = require('fs');
const path = require('path');
const { JSDOM } = require('jsdom');

const WEB = path.resolve(__dirname, '../../web');
const src = fs.readFileSync(path.join(WEB, 'app.js'), 'utf8');

function grabFn(name) {
  const s = src.indexOf('function ' + name + '(');
  if (s < 0) throw new Error('找不到函数 ' + name);
  const b = src.indexOf('{', s);
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
  <div id="board-drawer-callout"></div>
  <div id="board-drawer-agent"></div>
  <div id="board-drawer-artifacts"></div>
  <div id="board-drawer-meta"></div>
  <div id="board-drawer-stats"></div>
  <div id="board-drawer-links"></div>
  <div id="board-artifact-preview" class="hidden">
    <span id="board-artifact-preview-title"></span>
    <a id="board-artifact-preview-open" href="#"></a>
    <button id="board-artifact-preview-close"></button>
    <div id="board-artifact-preview-body"></div>
  </div>
</body></html>`;

const dom = new JSDOM(HTML, { runScripts: 'outside-only', url: 'http://deck.local/' });
const w = dom.window;
const doc = w.document;

w.eval(`
const $ = (sel) => document.querySelector(sel);
function boardStatusCallout() { return { cls: '', title: '状态', text: '' }; }
function boardPaneRow() { const d = document.createElement('div'); return d; }
function boardDrawerLine() {}
function boardTaskEvents() { return []; }
function boardTaskAge() { return '—'; }
function boardCopyButton(text, label = '复制') {
  const b = document.createElement('button');
  b.type = 'button';
  b.className = 'board-copy-btn';
  b.textContent = label;
  b.dataset.value = text;
  return b;
}
function md(text) { return text; }
${grabFn('boardMuted')}
${grabFn('boardArtifactSize')}
${grabFn('boardArtifactKindLabel')}
${grabFn('boardArtifactPreviewable')}
${grabFn('boardArtifactAction')}
${grabFn('boardArtifactItem')}
${grabFn('boardOpenArtifactPreview')}
${grabFn('boardCloseArtifactPreview')}
${grabFn('boardRenderOverview')}
${grabFn('boardRenderLinks')}
window.__renderOverview = boardRenderOverview;
window.__renderLinks = boardRenderLinks;
window.__openPreview = boardOpenArtifactPreview;
window.__closePreview = boardCloseArtifactPreview;
`);

const renderOverview = w.__renderOverview;
const renderLinks = w.__renderLinks;
const openPreview = w.__openPreview;
const closePreview = w.__closePreview;

let failed = 0;
function check(name, cond, extra) {
  if (cond) console.log('  ok  ' + name);
  else { failed++; console.log('  FAIL ' + name + (extra ? '  →  ' + extra : '')); }
}

function baseTask(extra) {
  return Object.assign({
    id: 'T-1', title: '任务A', status: '待验收', project: 'cc-webui',
    worker: 'w1', session: 's1', cwd: '/tmp', updated_at: '2026-11-09 12:00:00',
    last_event: '产物', report_file: '', artifacts: [],
  }, extra || {});
}

function main() {
  console.log('看板产物列表');

  // 1. 空状态
  renderOverview(baseTask());
  check('空状态显示「暂无产物」',
    doc.querySelector('#board-drawer-artifacts').textContent.includes('暂无产物'));

  // 2. 多产物
  renderOverview(baseTask({
    artifacts: [
      {
        id: 'A-1', task_id: 'T-1', title: '验收报告', kind: 'html', filename: 'report.html',
        size: 2048, source_session: 'worker-1', created_at: '2026-11-09 12:01:00',
        url: 'http://deck.local/api/agent-board/artifacts/A-1',
      },
      {
        id: 'A-2', task_id: 'T-1', title: '数据文件', kind: 'file', filename: 'data.csv',
        size: 1048576, source_session: 'worker-1', created_at: '2026-11-09 12:02:00',
        url: 'http://deck.local/api/agent-board/artifacts/A-2',
      },
    ],
  }));
  const items = [...doc.querySelectorAll('#board-drawer-artifacts .board-artifact')];
  check('渲染多个产物', items.length === 2, String(items.length));
  const first = items[0];
  check('标题', first.querySelector('.board-artifact-title').textContent === '验收报告');
  check('类型徽标', first.querySelector('.board-artifact-badge').textContent === 'HTML');
  check('大小/来源/时间', first.querySelector('.board-artifact-meta').textContent.includes('2 KB')
    && first.querySelector('.board-artifact-meta').textContent.includes('来源 worker-1')
    && first.querySelector('.board-artifact-meta').textContent.includes('2026-11-09 12:01:00'));
  const open = first.querySelector('a.board-artifact-action');
  check('打开链接', open && open.textContent === '打开' && open.href === 'http://deck.local/api/agent-board/artifacts/A-1'
    && open.target === '_blank' && open.rel === 'noopener');
  const download = [...first.querySelectorAll('a.board-artifact-action')].find((a) => a.textContent === '下载');
  check('下载链接带 download=1', download && download.href.endsWith('?download=1') && download.hasAttribute('download'));
  const preview = [...first.querySelectorAll('button.board-artifact-action')].find((b) => b.textContent === '预览');
  check('HTML 有预览按钮', !!preview);
  const copy = first.querySelector('.board-copy-btn');
  check('复制链接按钮', copy && copy.textContent === '复制链接' && copy.dataset.value.endsWith('/A-1'));
  check('文件类型没有预览按钮',
    ![...items[1].querySelectorAll('button.board-artifact-action')].some((b) => b.textContent === '预览'));

  // 3. 服务端下发的 legacy report_file 产物
  renderOverview(baseTask({
    artifacts: [{
      id: 'legacy-report', task_id: 'T-1', title: '验证/进展报告（旧）', kind: 'html',
      filename: 'old.html', storage_path: '/worker/local/old.html', legacy: true, url: '',
    }],
  }));
  const legacy = doc.querySelector('#board-drawer-artifacts .board-artifact-legacy');
  check('legacy 产物有说明', legacy && legacy.querySelector('.board-artifact-note').textContent.includes('未托管'));
  check('legacy 只有复制路径', legacy && legacy.querySelector('.board-copy-btn').textContent === '复制路径'
    && !legacy.querySelector('a.board-artifact-action'));

  // 4. 旧服务端只有 report_file 时的兜底
  renderOverview(baseTask({ report_file: 'reports/old.md' }));
  const fallback = doc.querySelector('#board-drawer-artifacts .board-artifact');
  check('旧 report_file 兜底展示', fallback && fallback.textContent.includes('reports/old.md')
    && fallback.querySelector('.board-copy-btn').textContent === '复制路径');

  // 5. 外链产物
  renderOverview(baseTask({
    artifacts: [{
      id: 'A-3', task_id: 'T-1', title: '设计稿', kind: 'url', filename: 'link',
      url: 'https://example.com/design', created_at: '2026-11-09 12:03:00',
    }],
  }));
  const urlItem = doc.querySelector('#board-drawer-artifacts .board-artifact');
  const urlOpen = urlItem.querySelector('a.board-artifact-action');
  check('外链直接打开', urlOpen && urlOpen.href === 'https://example.com/design');
  check('外链不提供下载/预览',
    ![...urlItem.querySelectorAll('.board-artifact-action')].some((el) => el.textContent === '下载' || el.textContent === '预览'));

  // 6. HTML 预览 sandbox：不能有 allow-same-origin
  openPreview({
    id: 'A-1', title: '验收报告', kind: 'html',
    url: 'http://deck.local/api/agent-board/artifacts/A-1',
  });
  const mask = doc.querySelector('#board-artifact-preview');
  const frame = mask.querySelector('iframe.board-artifact-frame');
  check('预览遮罩打开', !mask.classList.contains('hidden'));
  check('iframe 存在且 src 正确', frame && frame.src === 'http://deck.local/api/agent-board/artifacts/A-1');
  const sandbox = frame.getAttribute('sandbox') || '';
  check('sandbox 不允许 same-origin', sandbox.includes('allow-scripts') && !sandbox.includes('allow-same-origin'));
  check('iframe referrer 策略', frame.getAttribute('referrerpolicy') === 'no-referrer');
  check('预览标题/新窗口链接',
    mask.querySelector('#board-artifact-preview-title').textContent === '验收报告'
    && mask.querySelector('#board-artifact-preview-open').href === 'http://deck.local/api/agent-board/artifacts/A-1');
  closePreview();
  check('关闭后卸载 iframe 并隐藏',
    mask.classList.contains('hidden') && !mask.querySelector('iframe'));

  // 7. 关联页也列出产物
  renderLinks(baseTask({
    artifacts: [{
      id: 'A-1', task_id: 'T-1', title: '验收报告', kind: 'html', filename: 'report.html',
      url: 'http://deck.local/api/agent-board/artifacts/A-1',
    }],
  }));
  const linkItems = [...doc.querySelectorAll('#board-drawer-links .board-link-item')];
  const artifactLink = linkItems.find((el) => el.querySelector('.board-link-title').textContent === '验收报告');
  check('关联页列出产物', !!artifactLink
    && artifactLink.querySelector('a.board-link-open').href === 'http://deck.local/api/agent-board/artifacts/A-1');

  console.log(failed ? `\n${failed} 项失败` : '\n全部通过');
  process.exit(failed ? 1 : 0);
}

main();
