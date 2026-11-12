// 看板「产物与证据」前端回归：多产物列表（标题/类型/大小/来源/时间）、
// 打开/预览/下载/复制链接、legacy report_file 兼容、HTML 预览的 sandbox 边界，
// 以及 Markdown 预览复用 ws-md 排版 + 宽表横向滚动容器。
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
function md(text) { return window.__mdHTML != null ? window.__mdHTML : text; }
${grabFn('boardMuted')}
${grabFn('boardArtifactSize')}
${grabFn('boardArtifactKindLabel')}
${grabFn('boardArtifactPreviewable')}
${grabFn('boardArtifactGroupOf')}
${grabFn('boardArtifactGroups')}
${grabFn('boardArtifactTimeRange')}
${grabFn('boardArtifactRoundCard')}
${grabFn('boardArtifactAction')}
${grabFn('boardArtifactItem')}
${grabFn('boardWrapArtifactTables')}
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

async function main() {
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

  // 2b. 按轮次分组：最新一轮置顶，同轮多文件归一起，最新标记只在第一组
  renderOverview(baseTask({
    artifacts: [
      {
        id: 'A-10', task_id: 'T-1', title: '第 10 轮报告', kind: 'html', filename: 'r10.html',
        round: 10, round_source: 'explicit', created_at: '2026-11-09 11:00:00',
        url: 'http://deck.local/api/agent-board/artifacts/A-10',
      },
      {
        id: 'A-11a', task_id: 'T-1', title: '第 11 轮报告', kind: 'html', filename: 'r11.html',
        round: 11, round_source: 'explicit', created_at: '2026-11-09 12:00:00',
        url: 'http://deck.local/api/agent-board/artifacts/A-11a',
      },
      {
        id: 'A-11b', task_id: 'T-1', title: '第 11 轮文字稿', kind: 'markdown', filename: 's11.md',
        round: 11, round_source: 'explicit', created_at: '2026-11-09 12:01:00',
        url: 'http://deck.local/api/agent-board/artifacts/A-11b',
      },
      {
        id: 'A-9', task_id: 'T-1', title: '第 9 轮压缩包', kind: 'file', filename: 'r9.zip',
        round: 9, round_source: 'explicit', created_at: '2026-11-09 10:00:00',
        url: 'http://deck.local/api/agent-board/artifacts/A-9',
      },
    ],
  }));
  const rounds = [...doc.querySelectorAll('#board-drawer-artifacts .board-artifact-round')];
  check('轮次倒序且最新置顶',
    rounds.map((g) => g.dataset.roundTitle).join(',') === '第 11 轮,第 10 轮,第 9 轮',
    rounds.map((g) => g.dataset.roundTitle).join(','));
  check('最新标记只在第一组',
    !!rounds[0].querySelector('.board-artifact-round-latest-tag')
    && !rounds[1].querySelector('.board-artifact-round-latest-tag')
    && !rounds[2].querySelector('.board-artifact-round-latest-tag'));
  const roundMeta = rounds[0].querySelector('.board-artifact-round-meta').textContent;
  check('组头显示时间与数量',
    roundMeta.includes('11-09 12:00–12:01') && roundMeta.includes('2 个产物'), roundMeta);
  check('同一轮的多个文件归入一组',
    rounds[0].querySelectorAll('.board-artifact').length === 2
    && rounds[1].querySelectorAll('.board-artifact').length === 1);
  check('组内保留服务端顺序',
    [...rounds[0].querySelectorAll('.board-artifact-title')].map((el) => el.textContent).join(',')
      === '第 11 轮报告,第 11 轮文字稿');
  check('分组后每行动作不回归',
    !!rounds[0].querySelector('a.board-artifact-action')
    && !!rounds[0].querySelector('button.board-artifact-action')
    && !!rounds[0].querySelector('.board-copy-btn'));
  check('分组容器铺平为透明层',
    doc.querySelector('#board-drawer-artifacts').classList.contains('board-artifacts-grouped'));

  // 2c. 旧产物 fallback：服务端推断的批次显示「较早产物」，绝不伪造「第 N 轮」；
  //     无法聚类的进「历史产物」，最新批次同样可以带「最新」。
  renderOverview(baseTask({
    artifacts: [
      {
        id: 'L-unknown', task_id: 'T-1', title: '无时间旧产物', kind: 'file', filename: 'x.bin',
        round: 0, round_source: '', created_at: 'not-a-time',
        url: 'http://deck.local/api/agent-board/artifacts/L-unknown',
      },
      {
        id: 'L-2', task_id: 'T-1', title: '旧批次二', kind: 'html', filename: 'old2.html',
        round: 0, round_source: 'inferred', round_batch: 2, created_at: '2026-11-08 09:00:00',
        url: 'http://deck.local/api/agent-board/artifacts/L-2',
      },
      {
        id: 'L-1', task_id: 'T-1', title: '旧批次一', kind: 'html', filename: 'old1.html',
        round: 0, round_source: 'inferred', round_batch: 1, created_at: '2026-11-09 09:00:00',
        url: 'http://deck.local/api/agent-board/artifacts/L-1',
      },
    ],
  }));
  const legacyGroups = [...doc.querySelectorAll('#board-drawer-artifacts .board-artifact-round')];
  check('旧产物按推断批次排序，未知时间进历史产物',
    legacyGroups.map((g) => g.dataset.roundTitle).join(',') === '较早产物,较早产物,历史产物',
    legacyGroups.map((g) => g.dataset.roundTitle).join(','));
  check('旧产物不伪造「第 N 轮」',
    !legacyGroups.some((g) => /第\s*\d+\s*轮/.test(g.dataset.roundTitle)));
  check('旧产物最新批次也有最新标记',
    !!legacyGroups[0].querySelector('.board-artifact-round-latest-tag')
    && !legacyGroups[1].querySelector('.board-artifact-round-latest-tag'));
  check('历史产物组保留可操作行',
    !!legacyGroups[2].querySelector('a.board-artifact-action'));

  // 2d. 无 round 字段的旧服务端数据（或纯历史数据）不会被误当成轮次
  renderOverview(baseTask({
    artifacts: [{
      id: 'A-no-round', task_id: 'T-1', title: '无轮次字段', kind: 'file', filename: 'n.bin',
      created_at: '2026-11-09 12:00:00',
      url: 'http://deck.local/api/agent-board/artifacts/A-no-round',
    }],
  }));
  check('缺 round 字段时归入历史产物',
    doc.querySelector('#board-drawer-artifacts .board-artifact-round').dataset.roundTitle === '历史产物');

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

  // 5. listen 兜底的产物标记「仅本机/局域网」
  renderOverview(baseTask({
    artifacts: [{
      id: 'A-local', task_id: 'T-1', title: '本机产物', kind: 'html', filename: 'local.html',
      url: 'http://127.0.0.1:8321/api/agent-board/artifacts/A-local',
      url_source: 'listen', url_local_only: true,
    }],
  }));
  const localNote = doc.querySelector('#board-drawer-artifacts .board-artifact-note-warn');
  check('listen 兜底提示仅本机可达', localNote && localNote.textContent.includes('仅本机/局域网可达'));

  // 6. 外链产物
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

  // 7. HTML 预览 sandbox：不能有 allow-same-origin
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

  // 7b. Markdown 预览：复用 ws-md 正文排版，宽表由外层容器横向滚动
  w.__mdHTML = '<h1>标题</h1>'
    + '<table><thead><tr><th>机器</th><th>状态</th></tr></thead>'
    + '<tbody><tr><td>node03</td><td>offline</td></tr></tbody></table>'
    + '<pre><code>very/long/path/that/needs/horizontal/scroll</code></pre>';
  w.fetch = () => Promise.resolve({ ok: true, text: () => Promise.resolve('# README') });
  openPreview({
    id: 'A-md', title: 'README', kind: 'markdown',
    url: 'http://deck.local/api/agent-board/artifacts/A-md',
  });
  await new Promise((resolve) => setTimeout(resolve, 0));
  const mdDoc = mask.querySelector('.board-artifact-markdown');
  check('Markdown 预览容器存在', !!mdDoc);
  check('Markdown 预览复用 ws-md 排版', mdDoc && mdDoc.classList.contains('ws-md'));
  const tableWrap = mdDoc && mdDoc.querySelector('.board-artifact-table-wrap');
  check('宽表套横向滚动容器', !!(tableWrap && tableWrap.querySelector('table')));
  check('表格没有被改成 display:block',
    !tableWrap || !/display\s*:\s*block/.test(tableWrap.querySelector('table').getAttribute('style') || ''));
  closePreview();
  w.__mdHTML = null;

  const css = fs.readFileSync(path.join(WEB, 'style.css'), 'utf8');
  check('CSS: 宽表滚动容器 overflow-x:auto',
    /\.board-artifact-markdown \.board-artifact-table-wrap\s*\{[^}]*overflow-x:\s*auto/s.test(css));
  check('CSS: th 不换行',
    /\.board-artifact-markdown th\s*\{[^}]*white-space:\s*nowrap/s.test(css));
  check('CSS: td word-break:normal',
    /\.board-artifact-markdown td\s*\{[^}]*word-break:\s*normal/s.test(css));
  check('CSS: 单元格行内 code 可断行',
    /\.board-artifact-markdown td code\s*\{[^}]*overflow-wrap:\s*anywhere/s.test(css));
  check('CSS: ws-md 提供标题/表格基础排版',
    /\.ws-md h1, \.ws-md h2, \.ws-md h3,/.test(css)
    && /\.ws-md table \{ border-collapse: collapse/.test(css)
    && /\.ws-md th, \.ws-md td \{/.test(css));
  check('CSS: GFM 任务列表规则',
    /\.board-artifact-markdown li:has\(> input\[type="checkbox"\]\)/.test(css));
  check('CSS: 分组容器透明化',
    /#board-drawer-artifacts\.board-artifacts-grouped\s*\{[^}]*background:\s*transparent/s.test(css));
  check('CSS: 轮次卡片有边框/圆角',
    /\.board-artifact-round\s*\{[^}]*border:\s*1px solid var\(--border\)[^}]*border-radius/s.test(css));
  check('CSS: 最新轮次高亮描边',
    /\.board-artifact-round-latest\s*\{[^}]*border-color:\s*var\(--accent\)/s.test(css));
  check('CSS: 分组不引入内层滚动',
    !/\.board-artifact-round[^{]*\{[^}]*overflow(?:-y)?:\s*(?:auto|scroll)/s.test(css));

  // 8. 关联页也列出产物
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

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
