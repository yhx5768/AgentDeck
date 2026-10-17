// web/app.js 里 HTML 沙箱渲染 + 全屏的回归测试。
//
// 为什么要住进仓库：这块出过三个 bug（图标空壳、全屏中卸载失效、滚动容器写错
// CSS 层级），全部是「单测全绿也照样漏」的类型，靠人工在浏览器点出来的。留着的
// 价值是把踩过的坑钉成断言，下次改这块不用重新踩。
//
// 局限要说清楚：jsdom 不做布局。max-height 到底裁不裁、overflow 在 flex 链里
// 生不生效、iframe 里的滚动条长什么样，这里一概验不了。能验的是 DOM 契约、
// 事件委托、节点搬运，以及「CSS 规则是否写在了正确的元素上」——后者正是滚动
// 那个 bug 的本质，所以仍然值得钉。
const fs = require('fs');
const path = require('path');
const { JSDOM } = require('jsdom');

const WEB = path.resolve(__dirname, '../../web');
const src = fs.readFileSync(path.join(WEB, 'app.js'), 'utf8');
const css = fs.readFileSync(path.join(WEB, 'style.css'), 'utf8');

// 剥掉注释，免得说明文字里的代码片段被当成真用法。
const stripJs = (t) => t.replace(/\/\*[\s\S]*?\*\//g, '').replace(/(^|[^:])\/\/[^\n]*/g, '$1');

// 按大括号配对原样摘出一个顶层函数——执行的是真代码，不是抄的副本。
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

// 摘出注册在模块顶层的委托（不是函数，切不出函数体）。做法是先用一条不含换行
// 的特征串定位到函数体内部，再回退到最近的 document.addEventListener，向前配对到
// 第一个 }); 为止——靠带 \`\n\` 的 marker 定位很容易在模板字符串里被转义坑到。
function grabListener(needle) {
  const hit = src.indexOf(needle);
  if (hit < 0) throw new Error('找不到监听器: ' + needle);
  const start = src.lastIndexOf('document.addEventListener', hit);
  if (start < 0) throw new Error('找不到 addEventListener 起点: ' + needle);
  const end = src.indexOf('});', hit);
  if (end < 0) throw new Error('找不到监听器结尾: ' + needle);
  return src.slice(start, end + 3);
}

const mdCut = src.indexOf('const $ = (sel) => document.querySelector(sel);');
const setup = `
document.documentElement.dataset.theme = 'light';
const st = document.createElement('style');
st.textContent = ':root{--bg:#fff;--bg2:#f6f8fa;--border:#d0d7de;--fg:#1f2328;--dim:#656d76;--accent:#0969da;--code-bg:#f6f8fa}';
document.head.appendChild(st);
` + src.slice(0, mdCut) + `
${grabListener("if (e.target.closest('.html-fs-close'))")}
${grabListener("if (e.key === 'Escape' && isHtmlFullscreen())")}
${grabListener("const btn = e.target.closest('.html-artifact-toggle');")}
${grabFn('buildHtmlArtifactNode')}
${grabFn('mountHtmlFrame')}
${grabFn('unmountHtmlFrame')}
${grabFn('enterHtmlFullscreen')}
${grabFn('exitHtmlFullscreen')}
${grabFn('isHtmlFullscreen')}
${grabFn('htmlFsLayer')}
${grabFn('buildHtmlFrameDoc')}
${grabFn('htmlFrameThemeCss')}
window.__x = { buildHtmlArtifactNode, mountHtmlFrame, unmountHtmlFrame, enterHtmlFullscreen,
               exitHtmlFullscreen, isHtmlFullscreen, htmlFsLayer };
`;

const dom = new JSDOM('<!doctype html><html><body></body></html>',
  { runScripts: 'outside-only', pretendToBeVisual: true });
const win = dom.window;
for (const f of ['marked.min.js', 'highlight.min.js', 'purify.min.js']) {
  win.eval(fs.readFileSync(path.join(WEB, 'vendor', f), 'utf8'));
}
win.eval(setup);
const X = win.__x;

let pass = 0, fail = 0;
const ok = (n, c, x) => {
  if (c) { pass++; console.log('  ✓ ' + n); }
  else { fail++; console.log('  ✗ ' + n + (x !== undefined ? '  → ' + x : '')); }
};
const section = (t) => console.log('\n' + t);

const HTML = '<!doctype html><html><body><h1>hi</h1><script>document.title="ran"</script></body></html>';
function makeArtifact() {
  const pre = win.document.createElement('pre');
  pre.textContent = HTML;
  const node = X.buildHtmlArtifactNode(pre);
  win.document.body.appendChild(node);
  return node;
}
const clickEl = (el) => el.dispatchEvent(new win.MouseEvent('click', { bubbles: true }));

section('[1] 工具条结构');
{
  const bar = makeArtifact().querySelector('.html-artifact-bar');
  const kids = [...bar.children].map((e) => e.className);
  ok('全屏在左、标签居中、源码/渲染在右',
     kids[0] === 'html-artifact-fs' && kids[1] === 'html-artifact-tag' && kids[2] === 'html-artifact-toggle',
     kids.join(' | '));
  ok('开关靠 margin-left:auto 顶右', /\.html-artifact-toggle\s*\{[^}]*margin-left:\s*auto/.test(css));
  ok('未渲染时全屏按钮隐藏，靠 .rendered 控制',
     /\.code-block-artifact\.rendered \.html-artifact-fs\s*\{[^}]*display:\s*inline-flex/.test(css));
}

section('[2] 沙箱安全边界');
{
  const a = makeArtifact();
  X.mountHtmlFrame(a);
  const f = a.querySelector('iframe.html-frame');
  const sb = f.getAttribute('sandbox');
  ok('sandbox 只给 allow-scripts', sb === 'allow-scripts', sb);
  ok('绝不给 allow-same-origin', !sb.includes('allow-same-origin'));
  ok('referrerpolicy=no-referrer', f.getAttribute('referrerpolicy') === 'no-referrer');
  ok('模型产物不进我们的 DOM（只在 srcdoc 里）',
     !a.querySelector('h1') && !a.querySelector('script') && f.srcdoc.includes('<h1>hi</h1>'));
}

section('[3] 全屏搬运节点而非重建');
{
  const a = makeArtifact();
  X.mountHtmlFrame(a);
  const frameRef = a.querySelector('iframe.html-frame');
  const box = a.querySelector('.html-frame-box');
  X.enterHtmlFullscreen(a);
  const layer = X.htmlFsLayer();
  ok('遮层显示', !layer.classList.contains('hidden'));
  ok('锁住背景滚动', win.document.body.classList.contains('html-fs-open'));
  ok('box 已搬进遮层', layer.querySelector('.html-fs-body').contains(box));
  ok('box 已离开 wrap', !a.contains(box));
  ok('iframe 是同一个节点（脚本状态/滚动位置不丢）',
     layer.querySelector('iframe.html-frame') === frameRef);
  X.exitHtmlFullscreen();
  ok('退出后 box 原路搬回', a.contains(box) && a.querySelector('iframe.html-frame') === frameRef);
  ok('退出后解锁背景', !win.document.body.classList.contains('html-fs-open'));
}

section('[4] 重复进入 / 切到另一个块');
{
  const a = makeArtifact(), b = makeArtifact();
  X.mountHtmlFrame(a); X.mountHtmlFrame(b);
  const boxA = a.querySelector('.html-frame-box');
  const boxB = b.querySelector('.html-frame-box');
  X.enterHtmlFullscreen(a);
  X.enterHtmlFullscreen(a);
  ok('已在全屏再点自己不叠加', X.htmlFsLayer().querySelectorAll('.html-fs-body > *').length === 1);
  clickEl(b.querySelector('.html-artifact-fs'));
  ok('点另一个块的按钮能切过去（不是死按钮）', X.isHtmlFullscreen());
  ok('A 的 box 已回到自己身上', a.contains(boxA));
  ok('B 的 box 在全屏里', X.htmlFsLayer().querySelector('.html-fs-body').contains(boxB));
  ok('切过去后遮罩里只有一个', X.htmlFsLayer().querySelectorAll('.html-fs-body > *').length === 1);
  X.exitHtmlFullscreen();
}

section('[5] Esc 与关闭按钮（走真实委托）');
{
  const a = makeArtifact();
  X.mountHtmlFrame(a);
  clickEl(a.querySelector('.html-artifact-fs'));
  ok('点按钮能进全屏', X.isHtmlFullscreen());
  win.document.dispatchEvent(new win.KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
  ok('Esc 退出', !X.isHtmlFullscreen());
  clickEl(a.querySelector('.html-artifact-fs'));
  clickEl(win.document.querySelector('.html-fs-close'));
  ok('关闭按钮退出', !X.isHtmlFullscreen());
}

section('[6] 全屏中卸载必须先收遮罩');
{
  const a = makeArtifact();
  X.mountHtmlFrame(a);
  clickEl(a.querySelector('.html-artifact-fs'));
  ok('先进全屏', X.isHtmlFullscreen());
  clickEl(a.querySelector('.html-artifact-toggle')); // 点「源码」
  ok('遮层已收', !X.isHtmlFullscreen());
  ok('遮罩里无残留 iframe', !win.document.querySelector('.html-fs-body iframe'));
  ok('背景已解锁', !win.document.body.classList.contains('html-fs-open'));
  ok('artifact 退回未渲染', !a.classList.contains('rendered'));
  ok('源码回来了', !!a.querySelector('pre'));
}

section('[7] 父节点已被替换时退出不炸');
{
  const a = makeArtifact();
  X.mountHtmlFrame(a);
  clickEl(a.querySelector('.html-artifact-fs'));
  ok('在全屏', X.isHtmlFullscreen());
  // 模拟流式重绘把整段 innerHTML 换掉：旧 box 连同父节点一起脱离文档
  a.parentNode.removeChild(a);
  let threw = false;
  try { clickEl(win.document.querySelector('.html-fs-close')); } catch (e) { threw = true; }
  ok('退出不抛异常', !threw);
  ok('遮层确实收掉了', !X.isHtmlFullscreen() && X.htmlFsLayer().classList.contains('hidden'));
}

section('[8] 全屏滚动容器的 CSS 契约');
{
  const bodyRule = css.match(/\.html-fs-body\s*\{([^}]*)\}/);
  ok('有 .html-fs-body 规则', !!bodyRule);
  ok('滚动发生在 .html-fs-body（overflow:auto）', bodyRule && /overflow:\s*auto/.test(bodyRule[1]));
  ok('.html-fs-body 分到高度（flex:1）', bodyRule && /flex:\s*1/.test(bodyRule[1]));
  ok('.html-fs-body 可收缩（min-height:0，缺了 flex:1 形同虚设）',
     bodyRule && /min-height:\s*0/.test(bodyRule[1]));
  const boxRule = css.match(/\.html-fs \.html-frame-box\s*\{([^}]*)\}/);
  ok('全屏下内层 box 解除封顶', boxRule && /max-height:\s*none/.test(boxRule[1]));
  ok('全屏下内层 box 不自己滚（交给父级）', boxRule && /overflow:\s*visible/.test(boxRule[1]));
  // DOM 嵌套必须和 CSS 选择器对得上，否则样式落不到点上
  const art = makeArtifact();
  X.mountHtmlFrame(art);
  X.enterHtmlFullscreen(art);
  const layer = X.htmlFsLayer();
  const bodyEl = layer.querySelector('.html-fs-body');
  ok('.html-fs-body 是 .html-fs 的直接子元素', bodyEl.parentNode === layer);
  ok('.html-frame-box 是 .html-fs-body 的直接子元素',
     !!bodyEl.firstElementChild && bodyEl.firstElementChild.classList.contains('html-frame-box'));
  X.exitHtmlFullscreen();
}

section('[9] 封顶与不误伤常见页面');
{
  ok('聊天内封顶有上限值', /\.html-artifact \.html-frame-box\s*\{[^}]*max-height:\s*var\(--html-frame-max/.test(css));
  ok('框内滚动用 auto 而非 contain（contain 会让滚到底也带不动聊天）',
     !/\.html-artifact \.html-frame-box\s*\{[^}]*overscroll-behavior:\s*contain/.test(css));
  ok('iframe 上限仍然是硬兜底', src.includes('HTML_FRAME_MAX_H = 20000'));
}

section('[10] rewind 作废目标（前端同步）');
{
  // 服务端 handleRewind 会 InvalidateGoal，前端必须在 applyRewindInUI 里跟着清，
  // 否则目标条会一直显示一条服务端已经删掉的目标。这条走 SSE 也一样会触发。
  const fn = src.indexOf('function applyRewindInUI');
  ok('找到 applyRewindInUI', fn >= 0);
  const body = src.slice(fn, src.indexOf('\nfunction ', fn + 10) > 0 ? src.indexOf('\nfunction ', fn + 10) : fn + 2500);
  ok('rewind 时回读会话并 applySession（清掉本地那份 goal）',
     /api\(`\/api\/sessions\/\$\{state\.current\.id\}`\)/.test(body) && /\.then\(applySession\)/.test(body));
}

section('[11] fork 不继承目标');
{
  const f = src.indexOf('func (s *Server) handleFork');
  const g = fs.readFileSync(path.resolve(__dirname, '../../internal/server/server.go'), 'utf8');
  const gi = g.indexOf('func (s *Server) handleFork');
  const seg = g.slice(gi, gi + 2200);
  const lit = seg.indexOf('&store.Session{');
  ok('fork 里构造了新 session', lit >= 0);
  ok('构造段里没有 Goal 字段（fork 是新分支，不该继承自动续跑契约）',
     !/Goal\s*:/.test(seg.slice(lit, seg.indexOf('\n\t}', lit))));
}

console.log(`\nhtml_frame: ${pass} 通过 / ${fail} 失败`);
console.log('注意：真实裁剪效果与移动端滚动手感 jsdom 验不了，仍需浏览器过一眼。');
process.exit(fail ? 1 : 0);
