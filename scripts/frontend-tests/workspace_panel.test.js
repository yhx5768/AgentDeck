// web/workspace.js（工作区面板）的 DOM 契约回归测试。
//
// 这套面板是照着 Codex 桌面端的「右侧文件区」做的：左边文件预览、右边文件树。
// 它的坏处很隐蔽——文件树是懒加载的（点开目录才请求下一层），
// 所以「目录节点在第一次展开时才创建」这件事一旦写错，表现就是
// **点目录没反应**（不报错、没红字，树上就是不展开）。
//
// 能钉住的是 DOM 契约：
//   ① 树行带 data-path，目录排在文件前面；
//   ② 点目录 → 建节点 → 发请求 → 展开出子行（回归点）；
//   ③ 筛选只留命中的节点；
//   ④ 只看改动时只留有 git 标记的文件；
//   ⑤ 打开文件 → 面包屑 + 正文（markdown 走 md()）；
//   ⑥ diff 行按 +/-/@@ 落到不同的 class 上。
//
// jsdom 不做布局：面板宽度、滚动、遮盖主对话这些验不了，仍需人工过一眼。
const fs = require('fs');
const path = require('path');
const assert = require('assert');
const { JSDOM } = require('jsdom');

const WEB = path.resolve(__dirname, '../../web');
const src = fs.readFileSync(path.join(WEB, 'workspace.js'), 'utf8');

// 按大括号配对原样摘出一个顶层函数——执行的是真代码，不是抄的副本。
// 扫描器要处理转义（源码里有 '\\\\' 这种字面量）、行注释、块注释和模板字符串，
// 否则配对会跑偏（早先的版本就栽在 '\\' 上）。
function grabFn(name) {
  const s = src.indexOf('function ' + name + '(');
  if (s < 0) throw new Error('找不到函数 ' + name);
  // async function xxx：切片起点在 "function" 上，async 得带上，否则 await 语法错
  const prefix = src.slice(Math.max(0, s - 6), s) === 'async ' ? 'async ' : '';
  const b = src.indexOf('{', s);
  let d = 0, q = null;
  for (let i = b; i < src.length; i++) {
    const c = src[i];
    if (q) {
      if (c === '\\') { i++; continue; } // 转义：下一个字符不是结束符
      if (c === q) q = null;
      continue;
    }
    if (c === '"' || c === "'" || c === '`') { q = c; continue; }
    if (c === '/' && src[i + 1] === '/') {
      while (i < src.length && src[i] !== '\n') i++;
      continue;
    }
    if (c === '/' && src[i + 1] === '*') {
      const end = src.indexOf('*/', i + 2);
      i = end < 0 ? src.length : end + 1;
      continue;
    }
    // 正则字面量：源码里有 /"/g 这种带引号的，扫成字符串就配错了。
    // 用「前一个有效字符」粗判是除号还是正则开头。
    if (c === '/') {
      const prev = src.slice(b, i).trimEnd().slice(-1);
      if (prev === '' || '(,=:[!&|?{};+-*%^~<>'.includes(prev)) {
        let j = i + 1, inClass = false;
        while (j < src.length) {
          if (src[j] === '\\') { j += 2; continue; }
          if (src[j] === '[') inClass = true;
          else if (src[j] === ']') inClass = false;
          else if (src[j] === '/' && !inClass) break;
          else if (src[j] === '\n') break;
          j++;
        }
        i = j;
        continue;
      }
    }
    if (c === '{') d++;
    else if (c === '}') { d--; if (!d) return prefix + src.slice(s, i + 1); }
  }
  throw new Error(name + ' 括号不配对');
}

function grabConst(name) {
  const m = src.match(new RegExp('const ' + name + '\\s*=\\s*[^;]+;'));
  if (!m) throw new Error('找不到常量 ' + name);
  return m[0];
}

// 树/预览的假数据：根目录两个子目录 + 两个文件。
const TREE_ROOT = [
  { name: 'docs', path: 'docs', dir: true, size: 96, mtime: 1 },
  { name: 'src', path: 'src', dir: true, size: 96, mtime: 1 },
  { name: 'NEW.md', path: 'NEW.md', dir: false, size: 16, mtime: 1 },
  { name: 'README.md', path: 'README.md', dir: false, size: 176, mtime: 1 },
];
const TREE_DOCS = [
  { name: 'notes.md', path: 'docs/notes.md', dir: false, size: 202, mtime: 1 },
];
// 整树搜索的假结果：一个从未展开过的深层文件（回归：懒加载树搜不到）。
const SEARCH_HITS = [
  { name: 'riskminer-tool-20261026.zip', path: 'work/projects/risk-identification/deliverables/riskminer-skills/riskminer-tool-20261026.zip', dir: false, size: 1, mtime: 1, ignored: false },
  { name: 'secret.env', path: 'secret.env', dir: false, size: 1, mtime: 1, ignored: true },
];

const calls = [];

const setup = `
const $ = (sel) => document.querySelector(sel);
// jsdom 不做布局：scrollIntoView 没实现，定位相关的调用直接空转
if (!Element.prototype.scrollIntoView) Element.prototype.scrollIntoView = function () {};
// jsdom 没有 CSS.escape（浏览器里是有的），补个够用的转义
const CSS = { escape: (s) => String(s).split('').map((c) => (/[a-zA-Z0-9_-]/.test(c) ? c : String.fromCharCode(92) + c)).join('') };
const state = { current: { id: 's1', cwd: '/tmp/wsdemo' } };
const md = (t) => '<div class="md">' + String(t).replace(/</g, '&lt;') + '</div>';
const renderIcons = () => {};
async function api(url) {
  window.__calls.push(url);
  if (url.includes('/workspace/tree')) {
    const p = decodeURIComponent((url.split('path=')[1] || ''));
    return { entries: p === 'docs' ? window.__treeDocs : window.__treeRoot, skipped: false };
  }
  if (url.includes('/workspace/file')) {
    return { path: 'docs/notes.md', name: 'notes.md', abs: '/tmp/wsdemo/docs/notes.md',
             content: '# 会议纪要\\n\\n正文', binary: false, truncated: false, size: 20, mtime: 1 };
  }
  if (url.includes('/workspace/search')) {
    const q = decodeURIComponent((url.split('q=')[1] || '')).toLowerCase();
    const entries = window.__search.filter((e) => e.path.toLowerCase().includes(q) || e.name.toLowerCase().includes(q));
    return { q, entries, truncated: false };
  }
  if (url.includes('/workspace/diff')) {
    return { repo: true, path: 'docs/notes.md', unstaged: '@@ -1 +1 @@\\n-old\\n+new', staged: '', diff: '@@ -1 +1 @@\\n-old\\n+new', untracked: false };
  }
  if (url.includes('/workspace/status')) {
    return { repo: true, changes: { 'NEW.md': '??', 'src/main.go': ' M' } };
  }
  return {};
}
${grabConst('WS_OPEN_KEY')}
${grabConst('WS_WIDTH_KEY')}
${grabConst('wsState')}
${grabConst('WS_CODE_EXT')}
${grabConst('WS_IMAGE_EXT')}
${grabFn('wsExt')}
${grabFn('wsIsImage')}
${grabFn('wsIsCode')}
${grabFn('wsIsMarkdown')}
${grabFn('wsStatusBadge')}
${grabFn('wsRenderTree')}
${grabFn('wsBuildRow')}
${grabFn('wsRenderDir')}
${grabFn('wsRenderSearch')}
${grabFn('wsAbortSearch')}
${grabFn('wsSearchNow')}
${grabFn('wsScheduleSearch')}
${grabFn('wsClearFilter')}
${grabFn('wsLoadDir')}
${grabFn('wsToggleDir')}
${grabFn('wsOpenFile')}
${grabFn('wsRenderViewer')}
${grabFn('wsAppendNote')}
${grabFn('wsHumanSize')}
${grabFn('wsShowDiff')}
${grabFn('wsRenderDiff')}
${grabFn('wsEscape')}
${grabFn('wsToggle')}
${grabFn('wsEnsureRoot')}
${grabFn('wsOpenPath')}
${grabFn('wsRevealFile')}
${grabFn('wsRefreshStatus')}
${grabFn('wsRevealDir')}
${grabFn('wsTreeCollapsed')}
${grabFn('wsInitTips')}
window.__ws = { wsState, wsRenderTree, wsLoadDir, wsToggleDir, wsOpenFile, wsRenderViewer, wsShowDiff, wsOpenPath, wsToggle, wsTreeCollapsed, wsInitTips, wsSearchNow };
`;

const dom = new JSDOM(
  `<!doctype html><html><body>
     <div id="ws-panel">
       <button id="btn-workspace"></button>
       <div id="ws-crumbs"></div>
       <div id="ws-viewer-body"></div>
       <span id="ws-tree-root"></span>
       <button id="ws-tree-changed"></button>
       <div id="ws-tree"></div>
       <div id="ws-tree-pane">
         <div class="ws-tree-head">
           <button id="ws-tree-toggle" class="icon-btn" data-tip="收起文件树"></button>
           <span id="ws-tree-root"></span>
           <button id="ws-tree-changed" class="ws-changed" data-tip="只看有改动的文件"></button>
         </div>
         <div class="ws-tree-search"><input id="ws-filter"></div>
       </div>
       <button id="ws-diff" class="hidden"></button>
       <button id="ws-source" class="hidden"></button>
       <button id="ws-refresh" class="hidden"></button>
       <button id="ws-open" class="hidden"></button>
     </div>
   </body></html>`,
  { runScripts: 'outside-only', pretendToBeVisual: true, url: 'http://localhost/' });
const win = dom.window;
win.__calls = calls;
win.__treeRoot = TREE_ROOT;
win.__treeDocs = TREE_DOCS;
win.__search = SEARCH_HITS;
win.eval(setup);
const { wsState, wsRenderTree, wsLoadDir, wsToggleDir, wsOpenFile, wsRenderViewer, wsShowDiff, wsOpenPath, wsTreeCollapsed, wsSearchNow } = win.__ws;
const $ = (sel) => win.document.querySelector(sel);
const rows = () => [...win.document.querySelectorAll('#ws-tree .ws-row')].map((r) => r.dataset.path);

const tests = [];
const test = (name, fn) => tests.push({ name, fn });

test('根目录渲染出带 data-path 的树行，目录排在文件前面', () => {
  wsState.sid = 's1';
  wsState.root = '/tmp/wsdemo';
  wsState.dirs = { '': { entries: TREE_ROOT, loaded: true, expanded: true } };
  wsState.changes = {};
  wsState.filter = '';
  wsState.onlyChanged = false;
  wsRenderTree();
  assert.deepStrictEqual(rows(), ['docs', 'src', 'NEW.md', 'README.md']);
});

test('点目录会建节点、发请求并展开出子行（回归：点目录没反应）', async () => {
  wsState.dirs = { '': { entries: TREE_ROOT, loaded: true, expanded: true } };
  calls.length = 0;
  wsToggleDir('docs');
  assert.strictEqual(wsState.dirs['docs'].expanded, true, '目录节点应被建出来并标记为展开');
  await new Promise((r) => setTimeout(r, 0)); // 等 wsLoadDir 的 promise 落地
  assert.ok(calls.some((u) => u.includes('workspace/tree') && u.includes('path=docs')),
    '应请求 docs 子目录，实际请求：' + JSON.stringify(calls));
  assert.ok(rows().includes('docs/notes.md'), '展开后应出现 docs/notes.md 行，实际：' + JSON.stringify(rows()));
});

test('筛选走整树搜索：没展开过的深层文件按文件名也能找到', async () => {
  wsState.dirs = { '': { entries: TREE_ROOT, loaded: true, expanded: true } };
  wsState.filter = 'riskminer-tool';
  await wsSearchNow('riskminer-tool');
  assert.deepStrictEqual(rows(),
    ['work/projects/risk-identification/deliverables/riskminer-skills/riskminer-tool-20261026.zip']);
  wsState.filter = '';
  wsRenderTree();
});

test('筛选支持完整相对路径，并显示 git 忽略标记', async () => {
  wsState.filter = 'work/projects/risk-identification';
  await wsSearchNow(wsState.filter);
  assert.ok(rows().includes('work/projects/risk-identification/deliverables/riskminer-skills/riskminer-tool-20261026.zip'),
    '输入完整相对路径也要能命中：' + JSON.stringify(rows()));
  wsState.filter = '';
  wsRenderTree();
});

test('git 忽略的文件仍然渲染，并带「git 忽略」标记', () => {
  wsState.dirs = { '': { entries: SEARCH_HITS, loaded: true, expanded: true } };
  wsState.filter = '';
  wsRenderTree();
  const row = win.document.querySelector('.ws-row[data-path="secret.env"]');
  assert.ok(row, '被忽略的文件不能被隐藏');
  const tag = row.querySelector('.ws-tag-ignored');
  assert.ok(tag, '应带 git 忽略标记');
  assert.strictEqual(tag.textContent, 'git 忽略');
});

test('只看改动时只留有 git 标记的文件', () => {
  wsState.dirs = { '': { entries: TREE_ROOT, loaded: true, expanded: true } };
  wsState.changes = { 'NEW.md': '??', 'src/main.go': ' M' };
  wsState.onlyChanged = true;
  wsRenderTree();
  assert.deepStrictEqual(rows(), ['NEW.md'], '只有 NEW.md 在这个目录里被标记过');
  wsState.onlyChanged = false;
  wsState.changes = {};
});

test('打开文件后面包屑和正文都出来，markdown 走 md()', async () => {
  await wsOpenFile('docs/notes.md');
  assert.strictEqual($('#ws-crumbs').textContent, 'docs/notes.md');
  const body = $('#ws-viewer-body').innerHTML;
  assert.ok(body.includes('会议纪要'), '正文应渲染出来：' + body);
  assert.ok(body.includes('class="md"'), 'markdown 应走 md()：' + body);
  assert.ok(!$('#ws-source').classList.contains('hidden'), '源码按钮应可见');
  assert.ok($('#ws-diff').classList.contains('hidden') === false, '改动按钮应可见');
});

// 全局 reset 是 `* { margin: 0; padding: 0; }`，所以 Markdown 正文的段间距一旦
// 不在 .ws-md 里补，整篇就会黏成一片（用户反馈过「行间距太小、太挤」）。
// jsdom 不做布局，这里只钉「规则写没写、写在哪个选择器上」。
test('markdown 正文有行距和块间距（reset 之后要自己补）', () => {
  const css = fs.readFileSync(path.join(WEB, 'style.css'), 'utf8');
  const rule = (sel) => {
    const m = css.match(new RegExp(sel + '[^{]*\\{([^}]*)\\}'));
    assert.ok(m, 'style.css 里应有 ' + sel + ' 规则');
    return m[1];
  };
  const md = rule('\\.ws-md');
  assert.ok(/line-height:\s*var\(--lh-relaxed\)/.test(md), '.ws-md 行距应放宽到 --lh-relaxed');
  assert.ok(/font-size:\s*var\(--fs-base\)/.test(md), '.ws-md 字号不应比聊天区小一档');
  assert.ok(/margin:/.test(rule('\\.ws-md p')), '段落之间要有间距');
  assert.ok(/margin:/.test(rule('\\.ws-md h1')), '标题要有间距');
  assert.ok(/margin:/.test(rule('\\.ws-md ul')), '列表要有间距');
  assert.ok(/padding|background/.test(rule('\\.ws-md pre')), '代码块要有壳，不能是裸文本');
});

test('改动视图按 +/-/@@ 给不同的 class', async () => {
  await wsShowDiff();
  const diff = $('#ws-viewer-body').innerHTML;
  assert.ok(diff.includes('ws-diff-add'), '新增行应有 ws-diff-add');
  assert.ok(diff.includes('ws-diff-del'), '删除行应有 ws-diff-del');
  assert.ok(diff.includes('ws-diff-hunk'), '@@ 行应有 ws-diff-hunk');
  assert.ok($('#ws-diff').classList.contains('active'), '改动按钮应处于选中态');
});

test('点消息里的文件链接 → 工作区面板（工作区内的文件）', async () => {
  wsState.sid = 's1';
  wsState.root = '/tmp/wsdemo';
  wsState.dirs = { '': { entries: TREE_ROOT, loaded: true, expanded: true } };
  const handled = wsOpenPath('/tmp/wsdemo/docs/notes.md', 3);
  assert.strictEqual(handled, true, '工作区内的文件应交给工作区面板处理');
  await new Promise((r) => setTimeout(r, 0));
  assert.strictEqual(wsState.file && wsState.file.path, 'docs/notes.md', '应换成相对路径打开');
  assert.strictEqual(wsState.line, 3, '链接里的行号要带过来');
});

test('工作区外的文件不交给工作区面板（退回旧预览浮层）', () => {
  wsState.sid = 's1';
  wsState.root = '/tmp/wsdemo';
  assert.strictEqual(wsOpenPath('/etc/hosts', 0), false, '工作区外的文件必须返回 false');
  assert.strictEqual(wsOpenPath('/tmp/别的项目/x.md', 0), false, '别的项目的文件必须返回 false');
});

test('文件树可折叠：折叠态只留窄条，按钮提示跟着换', () => {
  const pane = win.document.querySelector('#ws-tree-pane');
  const btn = win.document.querySelector('#ws-tree-toggle');
  wsTreeCollapsed(true);
  assert.ok(pane.classList.contains('collapsed'), '折叠后应带 collapsed 类');
  assert.strictEqual(btn.dataset.tip, '展开文件树', '按钮提示要改成「展开文件树」');
  wsTreeCollapsed(false);
  assert.ok(!pane.classList.contains('collapsed'), '再点一次应展开');
  assert.strictEqual(btn.dataset.tip, '收起文件树，把宽度让给预览区');
});

test('悬浮图标按钮会弹出说明（真的跑一次 mouseover）', async () => {
  const { wsInitTips } = win.__ws;
  wsInitTips();
  const btn = win.document.querySelector('#ws-tree-toggle');
  assert.ok(btn.dataset.tip, '按钮应带 data-tip');
  btn.dispatchEvent(new win.MouseEvent('mouseover', { bubbles: true }));
  await new Promise((r) => setTimeout(r, 250)); // 等 150ms 的显示延迟
  const tip = win.document.querySelector('.ws-tip');
  assert.ok(tip, '应生成 tooltip 元素');
  assert.strictEqual(tip.textContent, btn.dataset.tip, 'tooltip 文案应等于 data-tip');
  assert.notStrictEqual(tip.style.display, 'none', 'tooltip 应可见');
  // 移开后收掉
  btn.dispatchEvent(new win.MouseEvent('mouseout', { bubbles: true, relatedTarget: win.document.body }));
  await new Promise((r) => setTimeout(r, 20));
  assert.strictEqual(tip.style.display, 'none', '移开后 tooltip 应隐藏');
});

// CommonJS 文件里不能混顶层 await：整个跑一遍包进 async main。
(async () => {
  let failed = 0;
  for (const t of tests) {
    try {
      await t.fn();
      console.log('  ✓', t.name);
    } catch (err) {
      failed++;
      console.log('  ✗', t.name, '\n     ', err.message);
    }
  }
  console.log(failed ? `\nworkspace panel: ${failed} 处失败` : '\nworkspace panel: 全部通过');
  process.exit(failed ? 1 : 0);
})();
