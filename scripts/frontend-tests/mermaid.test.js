// web/app.js 里 Mermaid 渲染的回归测试。
//
// 和 html_frame.test.js 同一个套路：真代码 + 真委托，只把 mermaid 库换成 stub。
// jsdom 做不了 SVG 布局，所以这里验的是——```mermaid 围栏有没有升级成带按钮的壳、
// 渲染/源码切换、失败时源码仍然可见、切主题会重画、全屏搬运与遮罩生命周期、
// 封顶 CSS 写在了正确的元素上。
const fs = require('fs');
const path = require('path');
const { JSDOM } = require('jsdom');

const WEB = path.resolve(__dirname, '../../web');
const src = fs.readFileSync(path.join(WEB, 'app.js'), 'utf8');
const css = fs.readFileSync(path.join(WEB, 'style.css'), 'utf8');

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
st.textContent = ':root{--bg:#fff;--bg2:#f6f8fa;--bg3:#eaeef2;--border:#d0d7de;--fg:#1f2328;--dim:#656d76;--accent:#0969da;--red:#cf222e;--font-sans:sans-serif}';
document.head.appendChild(st);
` + src.slice(0, mdCut) + `
${grabListener("if (e.target.closest('.html-fs-close'))")}
${grabListener("if (e.key === 'Escape' && isHtmlFullscreen())")}
${grabListener("const btn = e.target.closest('.mermaid-artifact-toggle');")}
window.__x = { md, buildMermaidArtifactNode, mountMermaid, unmountMermaid, paintMermaid,
               ensureMermaid, mermaidConfig, syncMermaidThemes,
               enterHtmlFullscreen, exitHtmlFullscreen, isHtmlFullscreen, htmlFsLayer };
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
const tick = () => new Promise((r) => setTimeout(r, 0));
const clickEl = (el) => el.dispatchEvent(new win.MouseEvent('click', { bubbles: true }));

let renderCalls = 0;
let renderImpl = async () => ({ svg: '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1900 100"><g><text>diagram</text></g></svg>' });
win.mermaid = {
  initialize() {},
  render() { renderCalls++; return renderImpl(); },
};

const SRC = 'flowchart LR\n  A[需求] --> B[功能点]\n  B --> C[验证目标]';

function makeMermaid() {
  const pre = win.document.createElement('pre');
  const code = win.document.createElement('code');
  code.className = 'language-mermaid';
  code.textContent = SRC;
  pre.appendChild(code);
  const node = X.buildMermaidArtifactNode(pre);
  win.document.body.appendChild(node);
  return node;
}

(async () => {
  section('[1] ```mermaid 围栏升级成带「渲染」按钮的壳');
  {
    const html = X.md('```mermaid\n' + SRC + '\n```');
    const host = win.document.createElement('div');
    host.innerHTML = html;
    win.document.body.appendChild(host);
    const wrap = host.querySelector('.code-block-mermaid');
    ok('mermaid 围栏有专用壳', !!wrap);
    ok('仍然是 .code-block-artifact（复用源码隐藏/工具条样式）',
       wrap && wrap.classList.contains('code-block-artifact'));
    ok('有 MERMAID 标签', !!wrap && wrap.querySelector('.html-artifact-tag').textContent === 'MERMAID');
    ok('有渲染按钮且默认文字是「渲染」',
       !!wrap && wrap.querySelector('.mermaid-artifact-toggle').textContent === '渲染');
    ok('源码还在 pre 里（复制按钮可复用）', !!wrap && wrap.querySelector('pre').textContent.includes('flowchart LR'));
    const plain = win.document.createElement('div');
    plain.innerHTML = X.md('```js\nconst x = 1;\n```');
    ok('普通代码块不受影响', !!plain.querySelector('.code-block:not(.code-block-artifact)'));
    ok('轨迹视图（artifacts:false）不升级',
       !X.md('```mermaid\n' + SRC + '\n```', { artifacts: false }).includes('mermaid-artifact-toggle'));
  }

  section('[2] 渲染 / 源码切换');
  {
    const a = makeMermaid();
    clickEl(a.querySelector('.mermaid-artifact-toggle'));
    await tick(); await tick();
    const svg = a.querySelector('.mermaid-frame-box svg');
    ok('渲染后挂上了 svg', !!svg);
    ok('按 viewBox 钉成自然尺寸（宽图不被压扁）',
       !!svg && svg.getAttribute('width') === '1900' && svg.getAttribute('height') === '100');
    ok('加上 .rendered（源码由 CSS 收起）', a.classList.contains('rendered'));
    ok('按钮变成「源码」', a.querySelector('.mermaid-artifact-toggle').textContent === '源码');
    clickEl(a.querySelector('.mermaid-artifact-toggle'));
    ok('切回源码后 svg 被拆掉', !a.querySelector('.mermaid-frame-box svg'));
    ok('去掉 .rendered', !a.classList.contains('rendered'));
    ok('按钮变回「渲染」', a.querySelector('.mermaid-artifact-toggle').textContent === '渲染');
  }

  section('[3] 渲染失败时源码保持可见');
  {
    const a = makeMermaid();
    const old = renderImpl;
    renderImpl = async () => { throw new Error('bad diagram'); };
    clickEl(a.querySelector('.mermaid-artifact-toggle'));
    await tick(); await tick();
    ok('错误写进框里', !!a.querySelector('.mermaid-error'));
    ok('错误里带原因', /bad diagram/.test(a.querySelector('.mermaid-error').textContent));
    ok('没有进 rendered（源码仍然看得见）', !a.classList.contains('rendered'));
    ok('按钮回到「渲染」，可以重试', a.querySelector('.mermaid-artifact-toggle').textContent === '渲染');
    renderImpl = old;
  }

  section('[4] 切主题重画已渲染的图');
  {
    const a = makeMermaid();
    clickEl(a.querySelector('.mermaid-artifact-toggle'));
    await tick(); await tick();
    const before = renderCalls;
    X.syncMermaidThemes();
    await tick(); await tick();
    ok('重画了一次', renderCalls === before + 1, renderCalls + ' vs ' + before);
    ok('图还在', !!a.querySelector('.mermaid-frame-box svg'));
    X.unmountMermaid(a);
  }

  section('[5] 首屏不加载库，第一次渲染才拉');
  {
    const saved = win.mermaid;
    delete win.mermaid;
    X.ensureMermaid();
    const s = [...win.document.querySelectorAll('script')]
      .find((el) => el.getAttribute('src') === '/vendor/mermaid.min.js');
    ok('动态插入 /vendor/mermaid.min.js', !!s);
    win.mermaid = saved;
  }

  section('[6] CSS 契约');
  {
    const boxRule = css.match(/\.code-block-mermaid \.mermaid-frame-box\s*\{([^}]*)\}/);
    ok('渲染框有封顶', !!boxRule && /max-height:\s*var\(--mermaid-frame-max/.test(boxRule[1]));
    ok('框内滚动（overflow:auto）', !!boxRule && /overflow:\s*auto/.test(boxRule[1]));
    ok('空框隐藏，避免未渲染时留白',
       /\.code-block-mermaid \.mermaid-frame-box:empty\s*\{[^}]*display:\s*none/.test(css));
    ok('svg 按自然尺寸渲染、宽图靠框内横向滚动',
       /\.mermaid-frame-box svg\s*\{[^}]*max-width:\s*none/.test(css));
    ok('svg 跟随主题变量而不是硬编码色', !/#(?:fff|000)\b/i.test((css.match(/\.mermaid-error\s*\{[^}]*\}/) || [''])[0]));
  }

  section('[7] 全屏查看（与 HTML 共用遮罩，搬 box 而非重建）');
  {
    const a = makeMermaid();
    ok('未渲染时全屏按钮被 CSS 藏起（.rendered 才显示）',
       /\.code-block-artifact\.rendered \.html-artifact-fs\s*\{[^}]*display:\s*inline-flex/.test(css));
    ok('工具条里有全屏按钮（与 HTML 同一套 class/图标）',
       !!a.querySelector('.html-artifact-fs svg'));
    clickEl(a.querySelector('.mermaid-artifact-toggle'));
    await tick(); await tick();
    ok('全屏按钮在 MERMAID 标签左边',
       [...a.querySelector('.html-artifact-bar').children].map((x) => x.className).join('|') ===
       'html-artifact-fs|html-artifact-tag|mermaid-artifact-toggle');
    const box = a.querySelector('.mermaid-frame-box');
    const svg = box.querySelector('svg');
    clickEl(a.querySelector('.html-artifact-fs'));
    ok('点按钮进全屏', X.isHtmlFullscreen());
    const layer = X.htmlFsLayer();
    ok('box 搬进遮罩', layer.querySelector('.html-fs-body').contains(box));
    ok('box 已离开 wrap', !a.contains(box));
    ok('svg 还是同一个节点（不重建、不重画）',
       layer.querySelector('.mermaid-frame-box svg') === svg);
    ok('标题是 Mermaid 预览', layer.querySelector('.html-fs-title').textContent === 'Mermaid 预览');
    ok('锁住背景滚动', win.document.body.classList.contains('html-fs-open'));

    // 全屏中切主题：box 不在 wrap 里，paintMermaid 必须从遮罩里找回它
    const before = renderCalls;
    X.syncMermaidThemes();
    await tick(); await tick();
    ok('全屏中换主题仍能重画', renderCalls === before + 1, renderCalls + ' vs ' + before);
    ok('重画后图还在遮罩里', !!layer.querySelector('.mermaid-frame-box svg'));

    win.document.dispatchEvent(new win.KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    ok('Esc 退出全屏', !X.isHtmlFullscreen());
    ok('box 原路搬回 wrap', a.contains(box) && !!a.querySelector('.mermaid-frame-box svg'));
    X.unmountMermaid(a);
  }

  section('[8] 全屏中点「源码」先收遮罩');
  {
    const a = makeMermaid();
    clickEl(a.querySelector('.mermaid-artifact-toggle'));
    await tick(); await tick();
    clickEl(a.querySelector('.html-artifact-fs'));
    ok('先进全屏', X.isHtmlFullscreen());
    clickEl(a.querySelector('.mermaid-artifact-toggle')); // 点「源码」
    ok('遮罩已收', !X.isHtmlFullscreen());
    ok('遮罩里无残留', !X.htmlFsLayer().querySelector('.mermaid-frame-box'));
    ok('背景已解锁', !win.document.body.classList.contains('html-fs-open'));
    ok('artifact 退回未渲染', !a.classList.contains('rendered'));
    ok('源码回来了、图已拆掉', !!a.querySelector('pre') && !a.querySelector('.mermaid-frame-box svg'));
  }

  section('[9] 全屏 CSS 契约');
  {
    const rule = css.match(/\.html-fs \.mermaid-frame-box\s*\{([^}]*)\}/);
    ok('全屏下解除封顶', !!rule && /max-height:\s*none/.test(rule[1]));
    ok('全屏下框不自己滚（滚动交给 .html-fs-body）',
       !!rule && /overflow:\s*visible/.test(rule[1]));
    ok('搬离 .code-block-mermaid 后仍有底色/内边距',
       !!rule && /background:\s*var\(--bg\)/.test(rule[1]) && /padding:\s*12px/.test(rule[1]));
  }

  console.log(`\nmermaid: ${pass} 通过 / ${fail} 失败`);
  process.exit(fail ? 1 : 0);
})();
