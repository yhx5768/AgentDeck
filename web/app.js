/* cc-webui frontend */
'use strict';

marked.setOptions({
  breaks: true,
  highlight(code, lang) {
    if (lang && hljs.getLanguage(lang)) {
      try { return hljs.highlight(code, { language: lang }).value; } catch (_) {}
    }
    return code;
  },
});
// 识别指向本地文件的链接（claude 常在回复里写绝对/相对路径，:N 为行号）。
// 返回 {path, line}；非本地路径返回 null。
function parseFileLink(href) {
  if (!href || href.startsWith('#') || href.startsWith('//')) return null;
  if (/^[a-z][a-z0-9+.-]*:/i.test(href)) return null; // http: mailto: 等外部协议
  let p, line = 0;
  try { p = decodeURIComponent(href); } catch (_) { p = href; }
  const m = p.match(/:(\d+)$/); // 尾部 :行号
  if (m) { line = parseInt(m[1], 10); p = p.slice(0, m.index); }
  if (p.startsWith('/')) return { path: p, line }; // 绝对路径
  // 相对路径：基于当前会话 cwd 解析
  const cwd = state.current && state.current.cwd;
  if (cwd && !p.startsWith('..')) return { path: cwd.replace(/\/$/, '') + '/' + p, line };
  return null;
}

// 代码块复制按钮的图标：直接内联 lucide copy 的 SVG，不走 <i data-lucide>。
// 流式期间 md() 每帧重绘，renderIcons 每帧扫描会刷日志且没必要。
// 工具条里的图标一律用原始 SVG，不走 lucide：renderIcons 只认 wanted 白名单，
// 白名单外的 <i data-lucide> 不会被替换成 svg，按钮就只剩一圈 padding（看着像没有）。
const HTML_FS_ICON = '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round"><polyline points="15 3 21 3 21 9"/><polyline points="9 21 3 21 3 15"/><line x1="21" x2="14" y1="3" y2="10"/><line x1="3" x2="10" y1="21" y2="14"/></svg>';
const CODE_COPY_ICON = '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round"><rect width="14" height="14" x="8" y="8" rx="2" ry="2"/><path d="M4 16c-1.1 0-2-.9-2-2V4c0-1.1.9-2 2-2h10c1.1 0 2 .9 2 2"/></svg>';

// Render markdown to sanitized HTML; force all links to open in a new tab
// so clicking a link never navigates away from the chat.
const CJK_TRAIL_RE = /[。、，；：？！「」『』（）【】《》〈〉“”‘’—…·）】〉》\].]+$/;
// 中英混排 autolink 修正：marked 会把紧贴 URL 的中文标点吞进 href（如
// "看https://x。")，这里把尾部 CJK 标点从 href 里剥出来，还给正文。
function fixCjkAutolink(tpl) {
  tpl.content.querySelectorAll('a[href]').forEach((a) => {
    const href = a.getAttribute('href');
    if (!href || !/^https?:\/\//i.test(href)) return;
    const m = href.match(CJK_TRAIL_RE);
    if (!m) return;
    const trail = m[0];
    a.setAttribute('href', href.slice(0, href.length - trail.length));
    // 链接文本尾部同样被吞时一起剥离，保持显示与跳转一致
    if (a.textContent.endsWith(trail)) {
      a.textContent = a.textContent.slice(0, a.textContent.length - trail.length);
    }
    a.after(document.createTextNode(trail));
  });
}
// 单 ~ 保护：GFM 删除线只要 ~~，孤立的 ~（如 a~b、~/.config）不应触发 del。
// 只处理非代码段，代码块/行内代码原样保留。
function guardSingleTilde(text) {
  const parts = String(text || '').split(/(```[\s\S]*?```|`[^`\n]*`)/g);
  for (let i = 0; i < parts.length; i += 2) {
    parts[i] = parts[i].replace(/(?<!~)~(?!~)/g, '&#126;');
  }
  return parts.join('');
}
// 本地图片：模型在回复里写 ![](/abs/path/x.png) 或 ![](docs/x.png) 时，浏览器会
// 把它当站点相对 URL 去请求 http://host/abs/path/x.png —— cc-webui 的静态服务只
// 提供内嵌的 web/，一律 404，图就裂在消息里（用户报「模型发的截图前端看不到」）。
// 改成走 /api/image 拉本地文件：后端限会话工作区内 + 图片类型白名单。
// 路径语义复用 parseFileLink（绝对路径直用、相对路径按会话 cwd 解析、.. 拒绝）。
function fixLocalImages(tpl) {
  tpl.content.querySelectorAll('img[src]').forEach((img) => {
    let src = img.getAttribute('src') || '';
    if (!src) return;
    // 已经是可直接取到的 URL：data:/http(s):/blob:/应用内路径，都不动。
    if (/^(data:|https?:|blob:|\/api\/)/i.test(src)) return;
    if (src.startsWith('file://')) src = src.slice('file://'.length);
    const file = parseFileLink(src);
    if (!file) return;
    img.setAttribute('src', '/api/image?path=' + encodeURIComponent(file.path));
    img.setAttribute('loading', 'lazy');
    img.classList.add('local-img');
    // 加载失败时（路径不在工作区内、文件被删了）浏览器显示 alt 文字，
    // 至少能看出是哪个路径出的问题，而不是一片碎图。
    if (!img.getAttribute('alt')) img.setAttribute('alt', '图片：' + file.path);
  });
}
const md = (text, opts) => {
  const tpl = document.createElement('template');
  tpl.innerHTML = DOMPurify.sanitize(marked.parse(guardSingleTilde(text || '')));
  fixCjkAutolink(tpl);
  fixLocalImages(tpl);
  tpl.content.querySelectorAll('a[href]').forEach((a) => {
    const file = parseFileLink(a.getAttribute('href'));
    if (file) {
      // 本地文件链接：交由文件查看器在侧栏打开，而不是跳转到 404
      a.dataset.filePath = file.path;
      if (file.line) a.dataset.fileLine = String(file.line);
      a.classList.add('file-link');
      a.href = '#'; // 占位，点击由 #messages 委托拦截，不会跳转
      return;
    }
    a.target = '_blank';
    a.rel = 'noopener noreferrer';
  });
  // 代码块：包一层 .code-block 容器，右上角悬浮复制按钮（点击走全局委托）。
  // 按钮放在 pre 外面而不是里面：pre 横向滚动时按钮不跟着内容跑。
  // ```html 围栏额外升级成沙箱渲染容器（默认仍显示代码，点「渲染」才挂 iframe）。
  tpl.content.querySelectorAll('pre').forEach((pre) => {
    const parent = pre.parentNode;
    if (!parent) return; // 已被上一轮摘走：没有原位可放回去，直接跳过
    const anchor = pre.nextSibling;
    let detached = false;
    try {
      const code = pre.querySelector('code');
      const lang = code ? (code.className.match(/language-([\w+-]+)/) || [])[1] || '' : '';
      const allowArtifact = !opts || opts.artifacts !== false;
      const langLc = lang.toLowerCase();
      const wantArtifact = allowArtifact && langLc === 'html';
      const wantMermaid = allowArtifact && langLc === 'mermaid';
      // 先把 pre 从树上摘下来再交给构造器：构造器会把 pre 收进新节点里，
      // 若此时 pre 还挂在原位，replaceWith 会因为它已无父节点而抛异常。
      parent.removeChild(pre);
      detached = true;
      const node = wantArtifact ? buildHtmlArtifactNode(pre)
        : wantMermaid ? buildMermaidArtifactNode(pre)
        : buildCodeBlockNode(pre);
      parent.insertBefore(node, anchor);
    } catch (err) {
      // 这一段是纯 DOM 手术，不该让整条消息渲染失败——抛出去会中断 flushHistory
      // 的回放循环，用户只看到"对话到一半没了"，还查不出是哪条消息。
      // 降级：把 pre 按原位放回去（没有复制按钮的裸代码块），其余照常。
      if (detached) { try { parent.insertBefore(pre, anchor); } catch (_) {} }
      console.warn('md: 代码块壳构造失败，已降级为裸代码块', err);
    }
  });
  return tpl.innerHTML;
};

// 普通代码块壳：.code-block 定位复制按钮，按钮放 pre 外面所以横向滚动时不跟内容跑。
function buildCodeBlockNode(pre) {
  const wrap = document.createElement('div');
  wrap.className = 'code-block';
  const btn = document.createElement('button');
  btn.className = 'code-copy-btn';
  btn.type = 'button';
  btn.title = '复制代码';
  btn.innerHTML = CODE_COPY_ICON;
  wrap.append(btn, pre);
  return wrap;
}

// 沙箱渲染壳：源码留在 pre 里，点「渲染」才挂 iframe。
// md() 的 ```html 围栏和 MCP 的 render_html 工具共用这一份 DOM，安全边界
// （sandbox 不给 allow-same-origin）完全一致。
function buildHtmlArtifactNode(pre) {
  const btn = document.createElement('button');
  btn.className = 'code-copy-btn';
  btn.type = 'button';
  btn.title = '复制代码';
  btn.innerHTML = CODE_COPY_ICON;
  // 全屏：框内封顶后长页面要滚，收起聊天其余部分看整页
  const fs = document.createElement('button');
  fs.className = 'html-artifact-fs';
  fs.type = 'button';
  fs.title = '全屏查看';
  fs.setAttribute('aria-label', '全屏查看');
  fs.innerHTML = HTML_FS_ICON;
  const toggle = document.createElement('button');
  toggle.className = 'html-artifact-toggle';
  toggle.type = 'button';
  toggle.textContent = '渲染';
  toggle.title = '在沙箱 iframe 里渲染这段 HTML';
  const tag = document.createElement('span');
  tag.className = 'html-artifact-tag';
  tag.textContent = 'HTML';
  const bar = document.createElement('div');
  bar.className = 'html-artifact-bar';
  bar.append(fs, tag, toggle); // toggle 靠 margin-left:auto 顶到右边
  const box = document.createElement('div');
  box.className = 'html-frame-box';
  const art = document.createElement('div');
  art.className = 'html-artifact';
  art.append(bar, box, pre);
  const wrap = document.createElement('div');
  wrap.className = 'code-block code-block-artifact';
  wrap.append(btn, art);
  return wrap;
}

// Mermaid 渲染壳：与 HTML 一样，源码默认可见，点「渲染」才换成图。
// 不开 iframe：mermaid 直接产出 SVG，宿主 DOM 里挂上即可；安全边界靠
// mermaid 的 securityLevel:'strict'（它内部会 DOMPurify 标签）+ 我们再对
// 产出的 SVG 过一遍 DOMPurify。
function buildMermaidArtifactNode(pre) {
  const btn = document.createElement('button');
  btn.className = 'code-copy-btn';
  btn.type = 'button';
  btn.title = '复制代码';
  btn.innerHTML = CODE_COPY_ICON;
  const toggle = document.createElement('button');
  toggle.className = 'mermaid-artifact-toggle';
  toggle.type = 'button';
  toggle.textContent = '渲染';
  toggle.title = '把这段 Mermaid 源码画成图';
  const tag = document.createElement('span');
  tag.className = 'html-artifact-tag';
  tag.textContent = 'MERMAID';
  const bar = document.createElement('div');
  bar.className = 'html-artifact-bar';
  bar.append(tag, toggle);
  const box = document.createElement('div');
  box.className = 'mermaid-frame-box';
  const art = document.createElement('div');
  art.className = 'html-artifact';
  art.append(bar, box, pre);
  const wrap = document.createElement('div');
  wrap.className = 'code-block code-block-artifact code-block-mermaid';
  wrap.append(btn, art);
  return wrap;
}

// ---------- Mermaid 渲染 ----------
// 库有 5 MB，不能塞进 index.html 让每页首屏都解析一遍——第一次点「渲染」时才加载。
let mermaidLoadPromise = null;
let mermaidSeq = 0;
const mermaidMounted = new Set(); // 已渲染的 wrap，切主题时重画

function mermaidConfig() {
  const cs = getComputedStyle(document.documentElement);
  const v = (n, d) => cs.getPropertyValue(n).trim() || d;
  const dark = document.documentElement.dataset.theme === 'dark';
  return {
    startOnLoad: false,
    securityLevel: 'strict',
    // 不让标签走 foreignObject：DOMPurify 会把它整段剥掉（标签就没了）。
    htmlLabels: false,
    theme: dark ? 'dark' : 'default',
    fontFamily: v('--font-sans', 'sans-serif'),
    themeVariables: {
      background: 'transparent',
      primaryColor: v('--bg2', dark ? '#161b22' : '#f6f8fa'),
      primaryTextColor: v('--fg', dark ? '#e6edf3' : '#1f2328'),
      primaryBorderColor: v('--border', dark ? '#30363d' : '#d0d7de'),
      lineColor: v('--dim', dark ? '#8b949e' : '#656d76'),
      secondaryColor: v('--bg3', dark ? '#21262d' : '#eaeef2'),
      tertiaryColor: v('--bg', dark ? '#0d1117' : '#fff'),
    },
  };
}

function ensureMermaid() {
  if (window.mermaid) return Promise.resolve(window.mermaid);
  if (mermaidLoadPromise) return mermaidLoadPromise;
  mermaidLoadPromise = new Promise((resolve, reject) => {
    const s = document.createElement('script');
    s.src = '/vendor/mermaid.min.js';
    s.async = true;
    s.onload = () => {
      const m = window.mermaid;
      if (m && typeof m.render === 'function') resolve(m);
      else { mermaidLoadPromise = null; reject(new Error('mermaid 未挂到全局')); }
    };
    s.onerror = () => {
      mermaidLoadPromise = null;
      reject(new Error('mermaid 库加载失败'));
    };
    document.head.appendChild(s);
  });
  return mermaidLoadPromise;
}

// 把源码画进 box。返回是否成功；失败时 box 里留一行错误说明，源码保持可见。
async function paintMermaid(wrap) {
  const pre = wrap.querySelector('pre');
  const box = wrap.querySelector('.mermaid-frame-box');
  if (!pre || !box) return false;
  const src = (pre.textContent || '').trim();
  if (!src) return false;
  const gen = String(++mermaidSeq);
  wrap.dataset.mermaidGen = gen;
  try {
    const m = await ensureMermaid();
    if (wrap.dataset.mermaidGen !== gen) return false;
    m.initialize(mermaidConfig());
    const { svg } = await m.render('ccgw-mermaid-' + gen, src);
    if (wrap.dataset.mermaidGen !== gen) return false;
    // 再洗一遍：mermaid 的 strict 模式已经处理过标签，这里是纵深防御。
    const holder = document.createElement('div');
    holder.innerHTML = DOMPurify.sanitize(svg, { USE_PROFILES: { svg: true, svgFilters: true } });
    const el = holder.querySelector('svg');
    if (el) {
      // 按 viewBox 钉成自然尺寸再做框内滚动：mermaid 默认 width="100%" 会把一张
      // 1900px 宽的水平流程图压进 500px 的聊天宽度里，字小到看不清。
      const vb = (el.getAttribute('viewBox') || '').trim().split(/[\s,]+/).map(Number);
      if (vb.length === 4 && vb[2] > 0 && vb[3] > 0) {
        el.setAttribute('width', String(Math.ceil(vb[2])));
        el.setAttribute('height', String(Math.ceil(vb[3])));
      }
      el.style.maxWidth = 'none';
    }
    box.textContent = '';
    while (holder.firstChild) box.appendChild(holder.firstChild);
    return true;
  } catch (err) {
    if (wrap.dataset.mermaidGen !== gen) return false;
    box.textContent = '';
    const msg = document.createElement('div');
    msg.className = 'mermaid-error';
    msg.textContent = 'Mermaid 渲染失败：' + ((err && err.message) || String(err));
    box.appendChild(msg);
    return false;
  }
}

async function mountMermaid(wrap) {
  const toggle = wrap.querySelector('.mermaid-artifact-toggle');
  if (toggle) { toggle.disabled = true; toggle.textContent = '渲染中…'; }
  const ok = await paintMermaid(wrap);
  if (toggle) toggle.disabled = false;
  if (ok) {
    wrap.classList.add('rendered');
    mermaidMounted.add(wrap);
    if (toggle) toggle.textContent = '源码';
  } else if (!wrap.classList.contains('rendered')) {
    if (toggle) toggle.textContent = '渲染';
  }
}

function unmountMermaid(wrap) {
  // 让还在飞的 paintMermaid 作废，别等它回来把图又塞回去。
  wrap.dataset.mermaidGen = String(++mermaidSeq);
  const box = wrap.querySelector('.mermaid-frame-box');
  if (box) {
    box.textContent = '';
  }
  wrap.classList.remove('rendered');
  mermaidMounted.delete(wrap);
  const toggle = wrap.querySelector('.mermaid-artifact-toggle');
  if (toggle) { toggle.disabled = false; toggle.textContent = '渲染'; }
}

function syncMermaidThemes() {
  if (!window.mermaid) return;
  for (const wrap of [...mermaidMounted]) {
    if (!wrap.isConnected) { mermaidMounted.delete(wrap); continue; }
    paintMermaid(wrap);
  }
}

// ---------- HTML 沙箱渲染 ----------
// 模型用 ```html 围栏交一整页文档/图表/可视化。默认仍按代码块显示，
// 用户点「渲染」才挂一个 iframe —— 这么设计有两个原因：
//   1. 模型只是想举一段 HTML 例子时不该被自动展开；
//   2. md() 在流式时每帧重跑（livePaint），自动建 iframe 会每帧重建、
//      脚本状态全丢还会闪屏。点开才建则天然躲开。
// 安全边界：sandbox 只给 allow-scripts，不给 allow-same-origin —— 页面拿到
// opaque origin，碰不到我们的 localStorage / DOM / 带 cookie 的同源接口。
// 反过来绝不能为了方便补上 allow-same-origin：MDN 明确警告同源文档同时拿到
// allow-scripts + allow-same-origin 时能自己摘掉 sandbox 属性，等于没有沙箱。
// 由于 opaque origin 的 origin 是 "null"，postMessage 只能发 '*'，所以宿主侧
// 用「消息来源是否等于自己挂载的 frame 的 contentWindow」来认领发送方。
const HTML_FRAME_MIN_H = 48;
const HTML_FRAME_MAX_H = 20000;
const htmlFrames = new Set();

// 页面内 bootstrap：插在 <head> 最前，页面自己的样式随后才加载（可覆盖主题）。
// 负责三件事：上报真实高度、把 a[href] 的 http(s) 点击转交宿主打开、接收主题切换。
const HTML_FRAME_BOOTSTRAP =
  '(function(){' +
  'var last=-1;' +
  'function post(m){try{parent.postMessage(m,"*");}catch(e){}}' +
  'function report(){' +
  'var d=document,h=Math.max(d.body?d.body.scrollHeight:0,d.documentElement.scrollHeight);' +
  'h=Math.max(' + HTML_FRAME_MIN_H + ',Math.min(' + HTML_FRAME_MAX_H + ',Math.ceil(h)));' +
  'if(h!==last){last=h;post({type:"ccgw:htmlframe:size",height:h});}' +
  '}' +
  // sandbox 不给 allow-popups，target=_blank 会静默失败，所以链接一律转交宿主
  'document.addEventListener("click",function(e){' +
  'var a=e.target&&e.target.closest?e.target.closest("a[href]"):null;if(!a)return;' +
  'var u;try{u=new URL(a.getAttribute("href"),document.baseURI);}catch(x){return;}' +
  'if(u.protocol!=="http:"&&u.protocol!=="https:")return;' +
  'e.preventDefault();post({type:"ccgw:htmlframe:link",url:u.href});},true);' +
  'if(window.ResizeObserver){try{new ResizeObserver(report).observe(document.documentElement);}catch(x){}}' +
  'window.addEventListener("resize",report);' +
  'window.addEventListener("load",report);' +
  'report();' +
  'window.addEventListener("message",function(e){' +
  'if(e.source!==parent)return;var d=e.data;' +
  'if(!d||d.type!=="ccgw:htmlframe:theme"||typeof d.css!=="string")return;' +
  'var el=document.getElementById("__ccgw_theme");if(!el){el=document.createElement("style");' +
  'el.id="__ccgw_theme";document.head.appendChild(el);}' +
  'el.textContent=d.css;last=-1;report();});' +
  '})()';

// 把 app 当前主题的取值导出成页面能用的 CSS 变量，页面靠这些跟明暗主题对齐。
function htmlFrameThemeCss() {
  const cs = getComputedStyle(document.documentElement);
  const v = (n) => cs.getPropertyValue(n).trim();
  return ':root{color-scheme:' +
    (document.documentElement.dataset.theme === 'dark' ? 'dark' : 'light') +
    ';--background:' + v('--bg') + ';--surface:' + v('--bg2') +
    ';--border:' + v('--border') + ';--foreground:' + v('--fg') +
    ';--muted:' + v('--dim') + ';--accent:' + v('--accent') +
    ';--code-bg:' + v('--code-bg') + '}';
}

function buildHtmlFrameDoc(src) {
  // </script> 写成拼接形式：本文件是独立 <script src> 加载的，但 srcdoc 里
  // 这段 bootstrap 会被当作真实 <script> 解析，闭合标签不能提前出现。
  const closeScript = '<' + '/script>';
  return '<!doctype html><html><head><meta charset="utf-8">' +
    '<style id="__ccgw_theme">' + htmlFrameThemeCss() + '</style>' +
    '<script>' + HTML_FRAME_BOOTSTRAP + closeScript +
    '</head><body>' + src + '</body></html>';
}

function mountHtmlFrame(wrap) {
  const pre = wrap.querySelector('pre');
  const box = wrap.querySelector('.html-frame-box');
  if (!pre || !box) return;
  const src = pre.textContent || '';
  if (!src.trim()) return;
  box.textContent = '';
  const frame = document.createElement('iframe');
  frame.className = 'html-frame';
  // 只给 allow-scripts。给 allow-same-origin 等于把我们的 origin 交出去（见上）。
  frame.setAttribute('sandbox', 'allow-scripts');
  frame.setAttribute('referrerpolicy', 'no-referrer');
  frame.setAttribute('title', 'HTML 沙箱渲染');
  frame.style.height = HTML_FRAME_MIN_H + 'px';
  frame.srcdoc = buildHtmlFrameDoc(src);
  box.appendChild(frame);
  htmlFrames.add(frame);
  wrap.classList.add('rendered');
  const toggle = wrap.querySelector('.html-artifact-toggle');
  if (toggle) toggle.textContent = '源码';
}

function unmountHtmlFrame(wrap) {
  // 必须先收全屏：enterHtmlFullscreen 把 .html-frame-box 搬进了遮罩，此时它在
  // wrap 之外，下面那句 querySelector 会找不到而整个函数提前 return——表现为
  // 「全屏中点源码没反应，iframe 留在遮罩里」。
  exitHtmlFullscreen();
  const box = wrap.querySelector('.html-frame-box');
  if (!box) return;
  // 拆掉而不是隐藏：iframe 常驻会一直占着内存和 rAF，页面里的定时器也不停。
  box.querySelectorAll('iframe').forEach((f) => htmlFrames.delete(f));
  box.textContent = '';
  wrap.classList.remove('rendered');
  const toggle = wrap.querySelector('.html-artifact-toggle');
  if (toggle) toggle.textContent = '渲染';
}

// 全屏：把承载 iframe 的 .html-frame-box 整体搬进 fixed 遮罩。
// 搬节点而不是重建 iframe —— 重建会把页面里的脚本状态、定时器、滚动位置全清掉。
// 退出时原路搬回同一个父节点。
let htmlFsReturn = null;

function enterHtmlFullscreen(wrap) {
  const box = wrap && wrap.querySelector('.html-frame-box');
  const frame = box && box.querySelector('iframe.html-frame');
  if (!box || !frame) return;
  // 已经在全屏里、又点了另一个块的按钮：先收掉再进新的，否则这一次点击是死的，
  // 用户只会以为按钮坏了。
  if (htmlFsReturn) {
    if (htmlFsReturn.box === box) return; // 已经在全屏的就是它，别把自己搬自己
    exitHtmlFullscreen();
  }
  const layer = htmlFsLayer();
  const body = layer.querySelector('.html-fs-body');
  htmlFsReturn = { box, parent: box.parentNode, next: box.nextSibling };
  body.appendChild(box);
  layer.classList.remove('hidden');
  document.body.classList.add('html-fs-open');
  const close = layer.querySelector('.html-fs-close');
  if (close) close.focus();
}

function exitHtmlFullscreen() {
  if (!htmlFsReturn) return;
  const layer = htmlFsLayer();
  const { box, parent, next } = htmlFsReturn;
  htmlFsReturn = null;
  // parent 可能已经被替换掉了：流式重绘会把整段 innerHTML 换掉，原 box 连同
  // 旧父节点一起脱离文档。这时别往一个游离节点里插，overlay 照常收起就行。
  if (parent && parent.isConnected) {
    parent.insertBefore(box, next && next.parentNode === parent ? next : null);
  }
  layer.classList.add('hidden');
  layer.querySelector('.html-fs-body').textContent = '';
  document.body.classList.remove('html-fs-open');
}

function isHtmlFullscreen() { return !!htmlFsReturn; }

function htmlFsLayer() {
  let layer = document.getElementById('html-fs-layer');
  if (!layer) {
    layer = document.createElement('div');
    layer.id = 'html-fs-layer';
    layer.className = 'html-fs hidden';
    layer.innerHTML =
      '<div class="html-fs-bar">' +
      '<span class="html-fs-title">HTML 预览</span>' +
      '<button class="html-fs-close" type="button">✕ 退出全屏（Esc）</button>' +
      '</div><div class="html-fs-body"></div>';
    document.body.appendChild(layer);
  }
  return layer;
}

function syncHtmlFrameThemes() {
  const css = htmlFrameThemeCss();
  for (const frame of [...htmlFrames]) {
    if (!frame.isConnected) { htmlFrames.delete(frame); continue; }
    try { frame.contentWindow.postMessage({ type: 'ccgw:htmlframe:theme', css }, '*'); } catch (_) {}
  }
}


const $ = (sel) => document.querySelector(sel);
const state = {
  clis: [],
  providers: [],
  profilesSig: null, // 最近一次拉取 clis/providers 的签名；config 热加载时据此判断是否真有变动
  sessions: [],
  archived: [],
  current: null,      // current session object
  es: null,
  pendingBySid: {},   // sessionId -> 待发送消息 {text, images}；本轮结束后自动提交，切会话不丢
  seenSeq: new Set(), // seqs already rendered on this connection
  toolBlocks: {},     // tool_use_id -> details element
  toolTimers: {},     // tool_use_id -> {timer, start} live elapsed ticker
  permCards: {},      // permission id -> {wrap, card, locked} outstanding approval cards
  toasts: {},         // toast id -> {el, timers[]} 页内浮层（待回答提问），见「强提示」段
  groups: [],         // group list
  currentGroup: null, // open group object {id, name, cwd, members}
  groupEs: null,
  groupSeen: null,    // per-connection seq set for group SSE
  groupLive: null,    // turnID -> {memberID -> {bubble, buf}} streaming member bubbles
  subagentCards: {},  // parent_tool_use_id -> 子代理卡片 {summary, body, count, label}
  agentToolMeta: {},  // Agent tool_use_id -> {title, type}（实时侧卡片标题来源）
  // opencode token 级流式：当前累积中的 part 气泡（按 partID 归属）
  ocPartID: '', ocBuf: '', ocBubble: null,
  ocVariants: {},     // "cli/provider" -> {ts, data:{model:[--variant 键]}|null}（带 TTL，见 ocVariantsFor）
  ocModels: {},       // "cli/provider" -> {ts, data:{models:[{name,input}]}|null}（带 TTL，见 ocModelsFor）
  mcodeCatalog: {},   // "cli/provider" -> {ts, data:{provider_id,models:[…],default_model}|null}（带 TTL，见 mcodeCatalogFor）
  qoderCatalog: {},   // "cli/provider" -> {ts, data:{models:[…],efforts:[…]}|null}（带 TTL，见 qoderCatalogFor）
  codexCatalog: {},   // "cli/provider" -> {ts, data:{models:[…]}|null}（带 TTL，见 codexCatalogFor）
  nearBottom: true,
  home: '',           // server home dir, for ~ path shortening
  drafts: {},         // sessionId -> {text, attachments}
  attached: [],       // 待发送附件：[{kind:'image',media_type,data} | {kind:'file',name,size,path}]
  nsAttached: [],     // images staged in the new-session composer
  // workspace sidebar
  showSearch: false,
  searchQuery: '',
  viewGroup: 'workspace', // 'workspace' | 'none'
  viewSort: 'updated',     // 'manual' | 'updated'
  collapsedGroups: new Set(),
  // history pagination: replayed events are buffered, only the latest
  // HISTORY_PAGE events are rendered; older ones load on demand.
  historyBuf: [],      // all replayed events (oldest first)
  histRenderedFrom: 0, // index in historyBuf of the oldest rendered event
  replaying: false,    // true until the server sends the replay_end marker
  renderingHistory: false, // 正在渲染回放/翻页历史（旧 status 事件不触发 flushPending 等副作用）
  stats: null,         // {turns, steps, firstTs, lastTs} for the current session
  live: null,          // live partial-stream state: {msgId, blocks:[{kind,el,bodyEl,buf,done,taken,raf}]}
  // 任务面板折叠状态，见「任务列表面板」段；数据源是 historyBuf
  taskFold: { tasks: {}, order: [], pendingCreates: {}, pendingLists: {}, todos: null },
  taskPanelOpen: true,   // 面板展开；未手动操作过：全完成自动收起、来新任务自动展开
  taskPanelTouched: false, // 本会话手动开合过 → 停止自动开合
  slashCommandsCache: (() => { try { return JSON.parse(localStorage.getItem('cc-webui:slash-cache') || '{}'); } catch { return {}; } })(),
};

// 切换会话时首屏只渲染最新 N 条事件，向上滚动点「加载更多」再翻页
const HISTORY_PAGE = 50;

// 收集当前 session 的待发送内容
function draft() {
  if (!state.current) return { text: '', attachments: [] };
  const id = state.current.id;
  if (!state.drafts[id]) state.drafts[id] = { text: '', attachments: [] };
  return state.drafts[id];
}

// ---------- theme ----------
function applyTheme(t, persist) {
  document.documentElement.dataset.theme = t;
  $('#hljs-light').disabled = t === 'dark';
  $('#hljs-dark').disabled = t !== 'dark';
  const btn = $('#btn-theme');
  if (btn) btn.innerHTML = `<i data-lucide="${t === 'dark' ? 'sun' : 'moon'}"></i>`;
  renderIcons(btn);
  // 已挂载的沙箱 iframe 是 opaque origin，变量得由宿主推过去（重建 iframe 会
  // 丢掉页面内状态，所以不拆了重挂）
  syncHtmlFrameThemes();
  syncMermaidThemes();
  if (persist) localStorage.setItem('cc-webui-theme', t);
}
$('#btn-theme').onclick = () => {
  const next = document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark';
  applyTheme(next, true);
};

// lucide icons: replace <i data-lucide="xxx"> with <svg>....
// Falls back to inner text if lucide isn't loaded.
function renderIcons(root) {
  const scope = root || document;
  if (!window.lucide || !window.lucide.createIcons) {
    console.warn('[lucide] not loaded yet');
    return;
  }
  // lucide 内部把 "chevron-down" 转 "ChevronDown" 再去查 icons map（属性值是 kebab-case）
  const all = window.lucide.icons || {};
  const wanted = [
    'moon', 'sun', 'plus',
    'chevron-down', 'paperclip', 'square', 'terminal', 'trash-2',
    'copy', 'check', 'x', 'send',
    'alert-triangle', 'file', 'file-text', 'file-plus', 'terminal-square',
    'rotate-ccw', 'pencil', 'search',
    'more-horizontal', 'archive', 'archive-restore', 'git-fork', 'git-branch',
    'flag', 'flag-off',
    'settings-2', 'arrow-up', 'settings', 'bar-chart-3',
    'menu', 'more-vertical', 'send-horizontal', 'folder',
    'panel-left-close', 'panel-left-open', 'list-collapse',
    'bot', 'users',
    'list-todo', 'circle', 'circle-check',
    'target', // Session Goals 的目标条靶心（§12）
    'message-circle-question', // 待回答 toast 的问号图标（§11，一直漏了，白名单外的 data-lucide 会渲染成空壳）
    'sliders-horizontal',
  ];
  // lucide 自动 kebab→Pascal，所以 icons map 的 key 也用 PascalCase
  const toPascal = (s) =>
    s.replace(/-([a-z0-9])/g, (_, c) => c.toUpperCase())
     .replace(/^[a-z]/, (c) => c.toUpperCase());
  const icons = {};
  const missing = [];
  for (const k of wanted) {
    // lucide 把 "chevron-down" 转 "ChevronDown" 后查 icons map，map 必须用 PascalCase key
    const pascal = toPascal(k);
    if (all[pascal]) icons[pascal] = all[pascal];
    else missing.push(k);
  }
  const matches = scope.querySelectorAll('[data-lucide]').length;
  console.log('[lucide] found', matches, 'data-lucide elements; icons:', Object.keys(icons).length, 'missing:', missing);
  window.lucide.createIcons({
    icons,
    attrs: { 'stroke-width': 1.75 },
    scope,
  });
}

// ---------- API ----------
async function api(path, opts = {}) {
  const res = await fetch(path, {
    headers: { 'Content-Type': 'application/json' },
    ...opts,
  });
  if (res.status === 401) {
    location.href = '/login.html';
    throw new Error('unauthorized');
  }
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new Error(body.error || `HTTP ${res.status}`);
  }
  return res.json();
}

// ---------- 任务完成通知（系统横幅 + 提示音） ----------
// 开关存 localStorage，跟着浏览器走：多个终端同时连着时各自决定是否提醒。
// 触发源是 loadSessions() 轮询后的 running 状态 diff（SSE 只订阅当前打开的
// 会话，轮询才能覆盖其他会话）。两轮之间的调度间隙 running=false 但
// queued>0，要求队列排空才算整轮跑完；interrupted（主动停止）不提醒。
// last_error 每轮结束时被后端覆盖（成功清空），所以 diff 时读到的就是本轮结果。
// 同一个 diff 也负责打「未读完成」标记（见下），通知可关，未读标记始终打。
const NOTIFY_KEY = 'cc-webui-notify';
function notifyPrefs() {
  try {
    const p = JSON.parse(localStorage.getItem(NOTIFY_KEY) || '{}');
    return { banner: p.banner !== false, sound: p.sound !== false }; // 默认全开
  } catch (_) {
    return { banner: true, sound: true };
  }
}
function saveNotifyPrefs(p) {
  localStorage.setItem(NOTIFY_KEY, JSON.stringify(p));
}

// ---------- 未读 / TODO 徽标（会话状态点 + 工作区栏计数） ----------
// 未读（蓝点）：会话本轮跑完但还没点开过；服务端在 turn_end / 出错时置位，
// 客户端点开即清除。TODO（橙黄点）：用户从三点菜单人工标注的「过会儿回来问
// 跑完了么」，发送下一条消息时自动清除。两者都存服务端（db.json），多端一致
// ——旧的 localStorage 蓝点只标在看到任务跑完的那一端，别的端读过了仍蓝点。
function patchSessionFlag(id, flags) {
  // 乐观更新本地缓存并立刻重渲染，再异步落库；失败则重拉整表兜底。这样点开
  // 会话 / 标待办时蓝橙点即时消失，不必等 PATCH 回包。
  const apply = (list) => {
    if (!list) return;
    for (const s of list) {
      if (s.id !== id) continue;
      if ('unread' in flags) s.unread = flags.unread;
      if ('todo' in flags) s.todo = flags.todo;
    }
  };
  apply(state.sessions);
  apply(state.archived);
  if (state.current && state.current.id === id) {
    if ('unread' in flags) state.current.unread = flags.unread;
    if ('todo' in flags) state.current.todo = flags.todo;
  }
  renderSessionList();
  renderWsActivity();
  api(`/api/sessions/${id}`, { method: 'PATCH', body: JSON.stringify(flags) })
    .catch(() => loadSessions());
}

// sessionId -> 活跃（running 或 queued>0）；每轮轮询把快照存 localStorage，下次打开
// 页面时拿它当基线——关页期间跑完的任务也能检出并标未读（但属旧闻，不弹横幅/出声，
// 通知只给在线转变）。没有快照时（首次使用）纯建基线，不把存量活跃当转变。
// 注意快照要把 queued 也算活跃：两轮之间的调度间隙 running=false 但 queued>0，
// 若只记 running，间隙那轮会把「曾在跑」的记忆冲掉，队列排空时就检不出完成了。
const RUN_SNAPSHOT_KEY = 'cc-webui-running-snapshot';
let runPrev = null;
function diffRunningSessions(list) {
  const boot = runPrev === null;
  let prev = runPrev;
  if (!prev) {
    try { prev = JSON.parse(localStorage.getItem(RUN_SNAPSHOT_KEY) || 'null'); } catch { prev = null; }
  }
  const now = {};
  for (const s of list) now[s.id] = !!(s.status && (s.status.running || s.status.queued > 0));
  runPrev = now;
  try { localStorage.setItem(RUN_SNAPSHOT_KEY, JSON.stringify(now)); } catch {}
  if (!prev) return;
  for (const s of list) {
    if (!prev[s.id]) continue; // 只有 活跃 → 停止 的转变才算跑完
    const st = s.status || {};
    if (st.running || st.queued > 0) continue;
    const err = st.last_error || '';
    if (err === 'interrupted') continue;
    const watching = state.current && state.current.id === s.id && document.visibilityState === 'visible';
    if (watching) {
      // 服务端在 turn_end 已置 unread；此刻用户正盯着该会话，结果就在眼前，
      // 清掉它——既避免眼前闪一下蓝点，也让其他端看到「已读」。非观看端不动：
      // 服务端已置位，下方渲染自然是蓝点。
      if (s.unread) patchSessionFlag(s.id, { unread: false });
    }
    if (!boot) notifyTurnDone(s, err);
  }
}

function notifyTurnDone(s, err) {
  const prefs = notifyPrefs();
  const title = (err ? '任务出错：' : '任务完成：') + (s.name || '会话');
  const body = err ? String(err).slice(0, 120) : (s.cwd || '');
  if (prefs.sound) playNotifySound(!!err);
  if (prefs.banner) showBannerNotify(title, body, s.id);
}

// 系统横幅通知。点击聚焦窗口并打开对应会话；tag 防止同一会话的通知堆叠。
function showBannerNotify(title, body, sessionId) {
  if (!('Notification' in window)) return;
  if (Notification.permission !== 'granted') return; // 授权在设置页/首次交互时请求
  try {
    const n = new Notification(title, {
      body,
      icon: '/icons/icon-192.png',
      tag: 'cc-webui-done-' + sessionId,
    });
    n.onclick = () => {
      window.focus();
      if (sessionId) openSession(sessionId).catch(() => {});
      n.close();
    };
  } catch (_) {}
}

// 提示音用 Web Audio 合成，无需音频文件。完成 = 上行双音；出错 = 下行双音；
// 待回答提问 = 三声「叮咚」强调（低频双音在忙碌的会话里太轻）。
let audioCtx = null;
function getAudioCtx() {
  const AC = window.AudioContext || window.webkitAudioContext;
  if (!AC) return null;
  if (!audioCtx) audioCtx = new AC();
  if (audioCtx.state === 'suspended') audioCtx.resume().catch(() => {});
  return audioCtx;
}
function playTones(notes, gap = 0.18) {
  const ctx = getAudioCtx();
  if (!ctx) return;
  notes.forEach((freq, i) => {
    const t0 = ctx.currentTime + i * gap;
    const osc = ctx.createOscillator();
    const gain = ctx.createGain();
    osc.type = 'sine';
    osc.frequency.value = freq;
    gain.gain.setValueAtTime(0, t0);
    gain.gain.linearRampToValueAtTime(0.18, t0 + 0.02);
    gain.gain.exponentialRampToValueAtTime(0.001, t0 + gap - 0.02);
    osc.connect(gain).connect(ctx.destination);
    osc.start(t0);
    osc.stop(t0 + gap);
  });
}
function playNotifySound(isError) {
  playTones(isError ? [440, 330] : [659.25, 880]);
}
function playAskSound() {
  playTones([880, 1174.66, 880], 0.14);
}

// autoplay 策略：AudioContext 与通知权限请求都要求用户手势上下文。
// 在首次交互时一次性预热/请求，之后轮询回调里才能真正出声、弹横幅。
let notifyUnlocked = false;
document.addEventListener('pointerdown', () => {
  if (notifyUnlocked) return;
  notifyUnlocked = true;
  const prefs = notifyPrefs();
  if (prefs.sound) getAudioCtx();
  if (prefs.banner && 'Notification' in window && Notification.permission === 'default') {
    Notification.requestPermission().catch(() => {});
  }
}, { capture: true });

// ---------- 待回答提问的强提示（页内浮层 + 侧栏徽标 + 一键跳转） ----------
// 为什么不用系统横幅：横幅要先授权、点了还得把窗口拉到前台，而提问有 55s
// 硬超时（helper 到点自动拒绝；AskUserQuestion 那档让模型稍后重问）。所以必须
// 有一个「没授权也在、不聚焦窗口也在、点一下直接落到卡片上」的页内浮层。
//
// 触发源有三条，按提问 id 去重，谁先到谁弹：
//   1. SSE permission_request —— 当前打开的会话；群视图里是成员转发来的卡
//   2. loadSessions() 轮询 diff —— 覆盖所有会话 + 群。SSE 只订阅当前会话，
//      「别的会话在提问」只有轮询看得见（5s 一跳，55s 窗口内绰绰有余）
//   3. fetchPendingPermissions —— 刷新/重连后补回的未决卡
const ASK_TOAST_KEY = (id) => 'ask:' + id;
const ASK_TOAST_MAX = 3;
const ASK_TOAST_GONE = 4200; // 超时后浮层再停留多久才自己退场

function toastLayer() {
  let el = $('#toast-layer');
  if (!el) {
    el = document.createElement('div');
    el.id = 'toast-layer';
    document.body.appendChild(el);
  }
  return el;
}

// 幂等：同一个 id 再触发只刷新内容，不叠加第二条。定时器挂在 rec 上，
// dropToast 一并清掉——浮层被挤掉、撤下或超时时都不留后台 tick。
// 退场走 .leaving 动画再摘节点，所以「还在淡出」的条目不计入占位（见 pushToast）。
function dropToast(id) {
  const rec = state.toasts[id];
  if (!rec) return;
  delete state.toasts[id];
  (rec.timers || []).forEach((t) => { clearTimeout(t); clearInterval(t); });
  rec.el.classList.add('leaving');
  setTimeout(() => rec.el.remove(), 180);
}

function fillToast(rec, t) {
  const el = rec.el;
  el.querySelector('.toast-title').textContent = t.title || '';
  el.querySelector('.toast-body').textContent = t.body || '';
  const acts = el.querySelector('.toast-actions');
  acts.innerHTML = '';
  for (const a of t.actions || []) {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'toast-btn' + (a.primary ? ' primary' : '');
    b.textContent = a.label;
    b.onclick = (e) => { e.stopPropagation(); a.onClick(); };
    acts.appendChild(b);
  }
  el.classList.toggle('clickable', !!t.onClick);
  el.onclick = t.onClick || null;
}

function pushToast(t) {
  const layer = toastLayer();
  const exist = state.toasts[t.id];
  if (exist) { fillToast(exist, t); return exist; }
  // 超量时挤掉最早的一条（连它的定时器一起）。正在淡出的条目已从 state.toasts
  // 摘掉、节点 180ms 后才消失，所以只数「还在场上」的，否则会误挤。
  const alive = () => [...layer.children].filter((c) => !c.classList.contains('leaving'));
  while (alive().length >= ASK_TOAST_MAX) {
    const old = alive()[0];
    let dropped = false;
    for (const id of Object.keys(state.toasts)) {
      if (state.toasts[id].el === old) { dropToast(id); dropped = true; break; }
    }
    if (!dropped) old.remove();
  }
  const el = document.createElement('div');
  el.className = 'toast' + (t.kind ? ' toast-' + t.kind : '');
  el.innerHTML =
    '<div class="toast-icon"><i data-lucide="message-circle-question"></i></div>' +
    '<div class="toast-main"><div class="toast-title"></div><div class="toast-body"></div></div>' +
    '<div class="toast-side"><span class="toast-timer"></span>' +
    '<button class="toast-x" type="button" title="关闭"><i data-lucide="x"></i></button></div>' +
    '<div class="toast-actions"></div>';
  el.querySelector('.toast-x').onclick = (e) => { e.stopPropagation(); dropToast(t.id); };
  layer.appendChild(el);
  const rec = { el, timers: [] };
  state.toasts[t.id] = rec;
  fillToast(rec, t);
  renderIcons(el);
  return rec;
}

// raiseAskToast：一条未决提问 → 常驻浮层（倒计时 + 「去回答」）。同一 id 只弹一次。
function raiseAskToast(ask, ctx) {
  if (!ask || !ask.id) return null;
  const key = ASK_TOAST_KEY(ask.id);
  if (state.toasts[key]) return state.toasts[key];
  const rec = pushToast({
    id: key,
    kind: 'ask',
    title: '模型在提问 · ' + (ctx.where || '会话'),
    body: ask.question || ask.header || '（模型在等你回答）',
    onClick: () => gotoAsk(ask, ctx),
    actions: [{ label: '去回答', primary: true, onClick: () => gotoAsk(ask, ctx) }],
  });
  // 倒计时按服务端给的创建时间算，刷新页面后也和 55s 硬超时对齐
  const deadline = (ask.ts || Date.now()) + PERM_TIMEOUT_S * 1000;
  const timerEl = rec.el.querySelector('.toast-timer');
  const paint = () => {
    const left = Math.max(0, Math.ceil((deadline - Date.now()) / 1000));
    timerEl.textContent = left > 0 ? left + 's' : '已超时';
    rec.el.classList.toggle('urgent', left > 0 && left <= 15);
    return left;
  };
  const timer = setInterval(() => {
    if (paint() > 0) return;
    clearInterval(timer);
    // 超时后卡片已作废（模型会重问），留几秒说明白再退场，别留一条点不动的僵尸提示
    rec.el.classList.add('expired');
    rec.timers.push(setTimeout(() => dropToast(key), ASK_TOAST_GONE));
  }, 500);
  rec.timers.push(timer);
  paint();
  if (notifyPrefs().sound) playAskSound();
  return rec;
}

// gotoAsk：从浮层跳到那张卡。跨会话要开一次会话、群成员卡要开一次群视图；
// 两处的「补未决卡」都是异步 fire-and-forget，所以定位靠轮询等它出现。
async function gotoAsk(ask, ctx) {
  dropToast(ASK_TOAST_KEY(ask.id));
  try {
    if (ctx.gid && !(state.currentGroup && state.currentGroup.id === ctx.gid)) {
      await openGroup(ctx.gid);
    } else if (ctx.sid && !(state.current && state.current.id === ctx.sid)) {
      await openSession(ctx.sid);
    }
  } catch (_) { /* 跳不过去也要把提示撤了，别留着误导 */ }
  await focusAskCard(ask.id);
}

async function focusAskCard(askId) {
  for (let i = 0; i < 25; i++) {
    const e = state.permCards[askId];
    if (e && e.wrap && !e.locked) {
      e.card.classList.remove('ask-flash');
      void e.card.offsetWidth; // 强制重排，让闪烁动画能重播
      e.card.classList.add('ask-flash');
      const first = e.card.querySelector('.perm-opt') || e.card.querySelector('.perm-btn');
      if (first) first.focus({ preventScroll: true });
      return true;
    }
    await new Promise((r) => setTimeout(r, 100));
  }
  return false;
}

// 轮询 diff：把「谁在提问」的全集和上一拍比一次。SSE 只覆盖当前打开的会话，
// 别的会话只有这条 5s 轮询能看见——也顺带兜住刷新后仍在等的那张卡。
function diffPendingAsks() {
  const live = new Set();
  const fresh = [];
  const collect = (asks, ctx) => {
    for (const a of asks || []) {
      live.add(a.id);
      fresh.push({ ask: a, ctx });
    }
  };
  for (const s of state.sessions) {
    collect(s.pending_asks, { sid: s.id, where: s.name || '未命名会话' });
  }
  for (const g of state.groups || []) {
    for (const a of g.pending_asks || []) {
      collect([a], {
        gid: g.id,
        where: (a.member_name ? a.member_name + '（' : '') + (g.name || '群聊') + (a.member_name ? '）' : ''),
      });
    }
  }
  for (const { ask, ctx } of fresh) raiseAskToast(ask, ctx);
  // 已回答/已超时的提问下一拍就从列表里消失：提示随之撤掉（不必等 resolve 事件，
  // 那一路只覆盖当前会话，跨会话时只有这里能撤）
  for (const id of Object.keys(state.toasts)) {
    if (id.startsWith('ask:') && !live.has(id.slice(4))) dropToast(id);
  }
}

// ---------- sessions list ----------
async function loadSessions() {
  const [active2, archived] = await Promise.all([
    api('/api/sessions'),
    api('/api/sessions?archived=true').catch(() => []),
  ]);
  state.sessions = active2;
  state.archived = archived;
  diffRunningSessions(active2);
  diffPendingAsks();
  renderSessionList();
  renderArchiveList();
  renderWsActivity();
  loadGroups().catch(() => {}); // 群列表随会话轮询一起刷新（量小）
}

function statusDot(s) {
  // 待回答的提问压过一切运行态：它有 55s 硬超时，红点脉冲是唯一「现在就得动」
  if ((s.pending_asks || []).length) return 'dot ask';
  if (s.status.running) return 'dot running';
  if (s.status.queued > 0) return 'dot queued';
  if (s.status.last_error && s.status.last_error !== 'interrupted') return 'dot error';
  if (s.todo) return 'dot todo';      // 人工标注：过会儿回来问跑完了么 → 橙黄
  if (s.unread) return 'dot unread';  // 跑完未查看 → 蓝，点开恢复灰
  return 'dot';
}

function timeAgo(ts) {
  const d = new Date(ts);
  const diff = (Date.now() - d.getTime()) / 1000;
  if (diff < 60) return '刚刚';
  if (diff < 3600) return `${Math.floor(diff / 60)} 分钟前`;
  if (diff < 86400) return `${Math.floor(diff / 3600)} 小时前`;
  return d.toLocaleDateString();
}

function formatMsgTime(ts) {
  const d = ts ? new Date(ts) : new Date();
  if (isNaN(d.getTime())) return '';
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

function shortenPath(p) {
  if (state.home && (p === state.home || p.startsWith(state.home + '/'))) {
    return '~' + p.slice(state.home.length);
  }
  return p;
}

// 折叠的目录组（按 cwd 记，localStorage 持久化）
const collapsedGroups = new Set(
  JSON.parse(localStorage.getItem('cc-webui-collapsed-groups') || '[]')
);
function saveCollapsedGroups() {
  localStorage.setItem('cc-webui-collapsed-groups', JSON.stringify([...collapsedGroups]));
}

// ---------- session-item rendering ----------
function sessionItemHTML(s, opts = {}) {
  const active = state.current && state.current.id === s.id ? ' active' : '';
  // 常驻会话只留紫点一个（运行态合并成紫点脉冲）；非常驻走原状态点，
  // 避免两个点并排的视觉噪音。
  const mark = s.resident
    ? `<span class="resident-mark on${s.status.running ? ' running' : ''}"></span>`
    : `<span class="${statusDot(s)}"></span>`;
  return `
    <button class="icon-btn menu-btn" title="更多操作" data-menu-for="${s.id}"><i data-lucide="more-horizontal"></i></button>
    <div class="name">${mark}<span class="name-text"></span>${askFlagHTML(s.pending_asks)}</div>
    <div class="meta">
      <span class="profile-tag"></span>
      <span class="time">${timeAgo(s.updated_at)}</span>
    </div>`;
}
// 「待回答」行内标签：提问不进消息流、只停在输入框上方的 dock，人不在这个会话
// 时侧栏这行是唯一的线索，所以给它一个和状态点同级的常驻标签。
function askFlagHTML(asks) {
  const n = (asks || []).length;
  if (!n) return '';
  return `<span class="ask-flag">待回答${n > 1 ? ' ' + n : ''}</span>`;
}
function askTip(asks) {
  const list = asks || [];
  if (!list.length) return '';
  return '⚠ 待回答：\n' + list.map((a) => (a.header ? a.header + '：' : '') + (a.question || '')).join('\n');
}

function bindSessionItem(item, s, isArchived = false) {
  // 待回答提问：整行底色脉动（光靠状态点太弱，扫侧栏时几乎看不见）
  item.classList.toggle('has-ask', (s.pending_asks || []).length > 0);
  item.querySelector('.name-text').textContent = s.name;
  item.querySelector('.profile-tag').textContent = s.provider || s.cli;
  const tip = `${s.cli} / ${s.provider} · ${s.cwd}`;
  // 引擎侧 session id（claude transcript / 文件锁 / opencode ses_…）挂在 tooltip 上，
  // 便于从文件名里的 sid 反查对应会话时核对。
  const ask = askTip(s.pending_asks);
  item.title = [ask, tip, s.claude_session_id].filter(Boolean).join('\n');
  item.onclick = (e) => {
    if (e.target.closest('.menu-btn')) return; // let menu-btn handle its own
    openSession(s.id);
  };
  item.querySelector('.menu-btn').onclick = (e) => {
    e.stopPropagation();
    openSessionMenu(s, item, isArchived);
  };
}

// ---------- sessions list rendering (filtered + grouped) ----------
// 工作区栏活动指示：运行中（含排队）计数 + 跑完未查看计数；都没有时隐藏。
// 徽章复用 .dot 配色，与会话列表里的状态点一致。
function renderWsActivity() {
  const el = $('#ws-activity');
  if (!el) return;
  let running = 0, unread = 0, todo = 0, asking = 0;
  for (const s of state.sessions) {
    const st = s.status || {};
    if (st.running || st.queued > 0) running++;
    if (s.unread) unread++;
    if (s.todo) todo++;
    if ((s.pending_asks || []).length) asking++;
  }
  // 群聊同样算：成员的提问卡只在群视图里，不进单聊列表
  for (const g of state.groups || []) {
    if ((g.pending_asks || []).length) asking++;
  }
  // 已删除的会话不再出现在 state.sessions，徽标随之自然消失，无需本地清理。
  el.innerHTML = '';
  if (!running && !unread && !todo && !asking) { el.classList.add('hidden'); return; }
  el.classList.remove('hidden');
  if (asking) {
    const b = document.createElement('span');
    b.className = 'ws-badge';
    b.dataset.kind = 'pending';
    b.title = `${asking} 个会话/群在等你回答，点击查看`;
    b.innerHTML = `<span class="dot ask"></span>待回答 ${asking}`;
    bindWsBadge(b, 'pending');
    el.appendChild(b);
  }
  if (running) {
    const b = document.createElement('span');
    b.className = 'ws-badge';
    b.dataset.kind = 'running';
    b.title = `${running} 个任务运行中，悬停或点击查看`;
    b.innerHTML = `<span class="dot running"></span>${running}`;
    bindWsBadge(b, 'running');
    el.appendChild(b);
  }
  if (unread) {
    const b = document.createElement('span');
    b.className = 'ws-badge';
    b.dataset.kind = 'unread';
    b.title = `${unread} 个任务已完成，点击查看`;
    b.innerHTML = `<span class="dot unread"></span>${unread}`;
    bindWsBadge(b, 'unread');
    el.appendChild(b);
  }
  if (todo) {
    const b = document.createElement('span');
    b.className = 'ws-badge';
    b.dataset.kind = 'todo';
    b.title = `${todo} 个会话待回看，点击查看`;
    b.innerHTML = `<span class="dot todo"></span>${todo}`;
    bindWsBadge(b, 'todo');
    el.appendChild(b);
  }
  // 轮询重建徽章时 popover 若还开着，续刷内容——旧徽章被替换后鼠标
  // mouseleave 不会触发，不能让它停在过期数据上
  if (wsPopKind && !$('#ws-pop').classList.contains('hidden')) {
    renderWsPop();
    if (wsPopAnchor) positionWsPop(wsPopAnchor);
  }
}

// ---------- 工作区任务 popover ----------
// 徽章悬停/点击展开对应状态的任务列表，点击条目跳转会话。
// wsPopKind 记录当前打开的类型，轮询刷新 renderWsActivity 时据此续刷。
let wsPopKind = null;
let wsPopAnchor = null;
let wsPopHideTimer = null;

function bindWsBadge(badge, kind) {
  badge.addEventListener('mouseenter', () => openWsPop(kind, badge));
  badge.addEventListener('mouseleave', () => scheduleWsPopHide(250));
  badge.addEventListener('click', (e) => {
    e.stopPropagation(); // 走下方 pointerdown 关闭之前不给 document 级监听机会
    openWsPop(kind, badge);
  });
}

function openWsPop(kind, anchor) {
  cancelWsPopHide();
  wsPopKind = kind;
  wsPopAnchor = anchor;
  renderWsPop();
  // 先解除 hidden 再定位：offsetHeight 在 display:none 时为 0，位置钳制会失真
  $('#ws-pop').classList.remove('hidden');
  positionWsPop(anchor);
}

function closeWsPop() {
  wsPopKind = null;
  wsPopAnchor = null;
  cancelWsPopHide();
  $('#ws-pop').classList.add('hidden');
}

function cancelWsPopHide() {
  if (wsPopHideTimer) { clearTimeout(wsPopHideTimer); wsPopHideTimer = null; }
}

function scheduleWsPopHide(delay) {
  cancelWsPopHide();
  wsPopHideTimer = setTimeout(closeWsPop, delay);
}

function positionWsPop(anchor) {
  const pop = $('#ws-pop');
  const rect = anchor.getBoundingClientRect();
  const pw = 280, ph = pop.offsetHeight || 240;
  let x = rect.left;
  let y = rect.bottom + 6;
  x = Math.max(8, Math.min(x, window.innerWidth - pw - 8));
  if (y + ph > window.innerHeight - 8) y = Math.max(8, rect.top - ph - 6);
  pop.style.left = x + 'px';
  pop.style.top = y + 'px';
}

function renderWsPop() {
  const pop = $('#ws-pop');
  pop.innerHTML = '';
  if (!wsPopKind) return;
  const byNewest = (a, b) => new Date(b.updated_at) - new Date(a.updated_at);
  // 「待回答」不从 state.sessions 里筛：单聊一条、群一条，条目自带跳转目标
  if (wsPopKind === 'pending') return renderWsPopPending();
  let list;
  if (wsPopKind === 'running') {
    list = state.sessions.filter((s) => s.status && (s.status.running || s.status.queued > 0));
  } else if (wsPopKind === 'todo') {
    list = state.sessions.filter((s) => s.todo);
  } else {
    list = state.sessions.filter((s) => s.unread);
  }
  list.sort(byNewest);
  const titleText = wsPopKind === 'running' ? '运行中'
    : wsPopKind === 'todo' ? '待回看' : '已完成';
  const title = document.createElement('div');
  title.className = 'ws-pop-title';
  title.textContent = titleText + `（${list.length}）`;
  pop.appendChild(title);
  if (!list.length) {
    const empty = document.createElement('div');
    empty.className = 'ws-pop-empty';
    empty.textContent = '暂无任务';
    pop.appendChild(empty);
    return;
  }
  for (const s of list) {
    const item = document.createElement('button');
    item.className = 'ws-pop-item';
    const dotKind = wsPopKind === 'running'
      ? (s.status.running ? 'running' : 'queued')
      : wsPopKind === 'todo' ? 'todo' : 'unread';
    const dot = document.createElement('span');
    dot.className = 'dot ' + dotKind;
    const name = document.createElement('span');
    name.className = 'name-text';
    name.textContent = s.name || '未命名会话';
    const meta = document.createElement('span');
    meta.className = 'meta';
    if (wsPopKind === 'running') {
      meta.textContent = s.status.running ? shortenPath(s.cwd || '') : '排队中';
    } else if (wsPopKind === 'todo') {
      meta.textContent = `标注于 ${timeAgo(s.updated_at)}`;
    } else {
      meta.textContent = `完成于 ${timeAgo(s.updated_at)}`;
    }
    item.append(dot, name, meta);
    item.onclick = () => {
      closeWsPop();
      openSession(s.id);
    };
    pop.appendChild(item);
  }
}

// 「待回答」popover：一个待回答的会话/群一条（不是一张卡一条），条目直接带上
// 最老那张卡的跳转目标——最早提问的离 55s 超时最近，该先答它。
function renderWsPopPending() {
  const pop = $('#ws-pop');
  const rows = [];
  for (const s of state.sessions) {
    if ((s.pending_asks || []).length) {
      rows.push({ name: s.name || '未命名会话', asks: s.pending_asks, ctx: { sid: s.id } });
    }
  }
  for (const g of state.groups || []) {
    if ((g.pending_asks || []).length) rows.push({ name: g.name || '群聊', asks: g.pending_asks, ctx: { gid: g.id } });
  }
  rows.sort((a, b) => (a.asks[0].ts || 0) - (b.asks[0].ts || 0));
  const title = document.createElement('div');
  title.className = 'ws-pop-title';
  title.textContent = `待回答（${rows.length}）`;
  pop.appendChild(title);
  for (const r of rows) {
    const ask = r.asks[0];
    const item = document.createElement('button');
    item.className = 'ws-pop-item';
    const dot = document.createElement('span');
    dot.className = 'dot ask';
    const name = document.createElement('span');
    name.className = 'name-text';
    name.textContent = r.name;
    const meta = document.createElement('span');
    meta.className = 'meta';
    const left = Math.max(0, Math.ceil(((ask.ts || Date.now()) + PERM_TIMEOUT_S * 1000 - Date.now()) / 1000));
    meta.textContent = left > 0 ? `${left}s 后超时${r.asks.length > 1 ? ` · ${r.asks.length} 张` : ''}` : '已超时';
    item.append(dot, name, meta);
    item.title = ask.question || '';
    item.onclick = () => {
      closeWsPop();
      gotoAsk(ask, r.ctx);
    };
    pop.appendChild(item);
  }
}

// 鼠标从徽章滑入 popover 时不关；滑出再延迟关
$('#ws-pop').addEventListener('mouseenter', cancelWsPopHide);
$('#ws-pop').addEventListener('mouseleave', () => scheduleWsPopHide(300));

// pointerdown 关闭（同 session-menu 的理由）；落在徽章/popover 上的跳过
document.addEventListener('pointerdown', (e) => {
  if (!wsPopKind) return;
  if (e.target.closest('#ws-pop') || e.target.closest('.ws-badge')) return;
  closeWsPop();
});

function renderSessionList() {
  const el = $('#session-list');
  el.innerHTML = '';

  // search filter：匹配会话名 + 两类 session id。
  //   s.id              —— cc-webui 自身 sid（data/events/<sid>.jsonl、data/uploads/<sid>/ 等）
  //   s.claude_session_id —— 当前引擎 sid（~/.claude/projects/<slug>/<sid>.jsonl、registry、文件锁等）
  //   s.previous_claude_session_ids —— 历史引擎 sid；rewind 到首条 / 进程重启换过 sid 后，
  //     旧 sid 仍在磁盘文件名里（transcript / backup / registry），靠它反查到当前会话。
  const q = state.searchQuery.trim().toLowerCase();
  let list = state.sessions;
  if (q) list = list.filter((s) => {
    const name = (s.name || '').toLowerCase();
    const id = (s.id || '').toLowerCase();
    const csid = (s.claude_session_id || '').toLowerCase();
    if (name.includes(q) || id.includes(q) || csid.includes(q)) return true;
    return (s.previous_claude_session_ids || []).some((x) => x.toLowerCase().includes(q));
  });

  // sort
  const residentFirst = (a, b) => (b.resident ? 1 : 0) - (a.resident ? 1 : 0);
  const byUpdatedDesc = (a, b) => new Date(b.updated_at) - new Date(a.updated_at);
  if (state.viewSort === 'manual') {
    // manual: by sort_order desc, fallback to resident first, then updated_at
    list = [...list].sort((a, b) => {
      const ao = a.sort_order || 0, bo = b.sort_order || 0;
      if (ao !== bo) return bo - ao;
      const r = residentFirst(a, b);
      if (r) return r;
      return byUpdatedDesc(a, b);
    });
  } else {
    // default: resident pinned to top within each project directory, then
    // newest first
    list = [...list].sort((a, b) => {
      const r = residentFirst(a, b);
      return r || byUpdatedDesc(a, b);
    });
  }

  if (!list.length && !(state.groups || []).length) {
    const empty = document.createElement('div');
    empty.style.cssText = 'padding:24px 12px;color:var(--dim);font-size:12px;text-align:center';
    empty.textContent = q ? '没有匹配的会话' : '暂无会话';
    el.appendChild(empty);
    renderIcons(el);
    return;
  }

  if (state.viewGroup === 'none') {
    const flat = document.createElement('div');
    flat.className = 'session-list-flat';
    const mixed = [
      ...list.map((s) => ({ ts: new Date(s.updated_at).getTime() || 0, session: s })),
      ...(state.groups || [])
        .filter((g) => !q || (g.name || '').toLowerCase().includes(q))
        .map((g) => ({ ts: new Date(g.updated_at || 0).getTime() || 0, group: g })),
    ].sort((a, b) => b.ts - a.ts);
    for (const entry of mixed) {
      if (entry.group) {
        appendGroupItem(flat, entry.group);
        continue;
      }
      const s = entry.session;
      const item = document.createElement('div');
      item.className = 'session-item' + (state.current && state.current.id === s.id ? ' active' : '');
      item.dataset.sessionId = s.id;
      item.innerHTML = sessionItemHTML(s);
      bindSessionItem(item, s);
      flat.appendChild(item);
    }
    el.appendChild(flat);
    renderIcons(el);
    return;
  }

  // group by cwd（单聊＋群聊按各自 cwd 落进同一工作区）
  const groups = new Map();
  for (const s of list) {
    const key = s.cwd || '(无目录)';
    if (!groups.has(key)) groups.set(key, []);
    groups.get(key).push(s);
  }
  for (const g of (state.groups || [])) {
    if (q && !(g.name || '').toLowerCase().includes(q)) continue;
    const key = g.cwd || '(无目录)';
    if (!groups.has(key)) groups.set(key, []);
    groups.get(key).push({ __group: true, ...g });
  }
  const ordered = [...groups.entries()].sort((a, b) => {
    const ta = Math.max(...a[1].map((s) => new Date(s.updated_at || 0).getTime() || 0));
    const tb = Math.max(...b[1].map((s) => new Date(s.updated_at || 0).getTime() || 0));
    return tb - ta;
  });

  for (const [cwd, sessions] of ordered) {
    const group = document.createElement('div');
    group.className = 'session-group' + (collapsedGroups.has(cwd) ? ' collapsed' : '');

    const header = document.createElement('div');
    header.className = 'group-header';
    header.innerHTML = `<span class="chev"><i data-lucide="chevron-down"></i></span><i data-lucide="folder" class="dir-icon"></i><span class="dir"></span><span class="count">${sessions.length}</span><button class="group-migrate-btn icon-btn" title="迁移/修改工作区路径"><i data-lucide="settings"></i></button><button class="group-add-btn icon-btn" title="新建单聊 / 群聊"><i data-lucide="plus"></i></button>`;
    header.querySelector('.dir').textContent = shortenPath(cwd);
    header.title = cwd;
    renderIcons(header);
    header.onclick = (e) => {
      if (e.target.closest('.group-add-btn') || e.target.closest('.group-migrate-btn')) return;
      if (collapsedGroups.has(cwd)) {
        collapsedGroups.delete(cwd);
        group.classList.remove('collapsed');
      } else {
        collapsedGroups.add(cwd);
        group.classList.add('collapsed');
      }
      saveCollapsedGroups();
    };
    // "+" button: choose single chat or group chat in this workspace
    header.querySelector('.group-add-btn').onclick = (e) => {
      e.stopPropagation();
      openWsAddMenu(cwd, e.currentTarget);
    };
    // "settings" button: migrate every session of this workspace to a new path
    header.querySelector('.group-migrate-btn').onclick = (e) => {
      e.stopPropagation();
      openWsMigratePop(sessions, e.currentTarget);
    };
    group.appendChild(header);

    for (const s of sessions) {
      if (s.__group) {
        appendGroupItem(group, s);
        continue;
      }
      const item = document.createElement('div');
      item.className = 'session-item' + (state.current && state.current.id === s.id ? ' active' : '');
      item.dataset.sessionId = s.id;
      item.innerHTML = sessionItemHTML(s);
      bindSessionItem(item, s);
      group.appendChild(item);
    }
    el.appendChild(group);
  }
  renderIcons(el);
}

// ---------- archive list ----------
function renderArchiveList() {
  const el = $('#archive-list');
  el.innerHTML = '';
  document.querySelector('.archive-count').textContent = state.archived.length;
  if (!state.archived.length) {
    const empty = document.createElement('div');
    empty.style.cssText = 'padding:12px;color:var(--dim);font-size:11.5px;text-align:center';
    empty.textContent = '暂无归档';
    el.appendChild(empty);
    renderIcons(document.getElementById('archive-section'));
    return;
  }
  for (const s of state.archived) {
    const item = document.createElement('div');
    item.className = 'session-item';
    item.dataset.sessionId = s.id;
    item.innerHTML = sessionItemHTML(s);
    bindSessionItem(item, s, true);
    el.appendChild(item);
  }
  renderIcons(el);
}

// ---------- session actions menu ----------
let menuTargetSession = null;

function openSessionMenu(s, anchor, isArchived) {
  menuTargetSession = s;
  syncMenuForEngine(s);
  const menu = $('#session-menu');
  // toggle action labels based on whether the session is archived
  const archiveBtn = menu.querySelector('[data-action="archive"]');
  archiveBtn.innerHTML = isArchived
    ? '<i data-lucide="archive-restore"></i>取消归档'
    : '<i data-lucide="archive"></i>归档';
  const todoBtn = menu.querySelector('[data-action="todo"]');
  todoBtn.innerHTML = s.todo
    ? '<i data-lucide="flag-off"></i>取消待办'
    : '<i data-lucide="flag"></i>标记待办';
  const goalBtn = menu.querySelector('[data-action="goal"]');
  goalBtn.innerHTML = s.goal
    ? '<i data-lucide="pencil"></i>编辑续跑目标…'
    : '<i data-lucide="target"></i>自动续跑目标…';
  menu.classList.remove('hidden');
  renderIcons(menu);
  const rect = anchor.getBoundingClientRect();
  // 实测宽高而非写死：菜单行数随功能增长会变，写死的估值会过期（曾按 5 行估 200，
  // 实际 5 按钮 + 1 分隔线只有 164）。renderIcons 先跑，因为待办按钮每次开菜单都会被
  // 换成新的 <i data-lucide>，实测要在图标替换成 svg 之后才准。
  const mw = menu.offsetWidth, mh = menu.offsetHeight;
  let x = rect.right + 4;
  let y = rect.top - 4;
  // 右侧放不下则右对齐锚点右缘，再钳回视口内（手机抽屉场景）
  if (x + mw > window.innerWidth - 8) x = rect.right - mw;
  x = Math.max(8, Math.min(x, window.innerWidth - mw - 8));
  if (y + mh > window.innerHeight - 8) y = window.innerHeight - mh - 8;
  if (y < 8) y = 8;
  menu.style.left = x + 'px';
  menu.style.top = y + 'px';
}

function closeSessionMenu() {
  $('#session-menu').classList.add('hidden');
  menuTargetSession = null;
}

// pointerdown 关闭（click 在触屏重渲染场景可能丢失）；
// 落在 ⋯ 按钮上的 tap 由按钮自己的 pointerdown 处理，跳过
document.addEventListener('pointerdown', (e) => {
  if (e.target.closest('#session-menu') || e.target.closest('.menu-btn')) return;
  closeSessionMenu();
});

$('#session-menu').addEventListener('click', async (e) => {
  const btn = e.target.closest('button[data-action]');
  if (!btn) return;
  const action = btn.dataset.action;
  const s = menuTargetSession;
  if (!s) return;
  closeSessionMenu();
  await handleSessionAction(action, s);
});

async function handleSessionAction(action, s) {
  if (action === 'rename') return openRenameDialog(s);
  if (action === 'fork')   return forkSession(s);
  if (action === 'archive') return toggleArchive(s);
  if (action === 'todo')   return patchSessionFlag(s.id, { todo: !s.todo });
  if (action === 'goal')   return openGoalDialog(s);
  if (action === 'delete') return deleteSession(s);
}

// ---------- 工作区整组迁移 ----------
// 侧栏分组头部的 ⚙ 按钮：把该工作区（cwd）下全部会话一次性迁移到新路径。
// 逐会话 PATCH，服务端负责各自的 claude transcript 迁移 / 常驻重启；运行中的
// 会话会被 409 拒绝并汇总在状态行里，其余照常完成。
let wsMigrateSessions = [];

function openWsMigratePop(sessions, anchor) {
  wsMigrateSessions = sessions;
  const from = (sessions[0] && sessions[0].cwd) || '';
  const input = $('#ws-migrate-input');
  input.value = from;
  $('#ws-migrate-count').textContent = `（${sessions.length} 个会话）`;
  const status = $('#ws-migrate-status');
  status.textContent = '';
  status.classList.remove('err');
  $('#ws-migrate-submit').disabled = false;
  const pop = $('#ws-migrate-pop');
  pop.classList.remove('hidden');
  const rect = anchor.getBoundingClientRect();
  const pw = 320, ph = 210;
  let x = rect.right + 4;
  let y = rect.top - 4;
  if (x + pw > window.innerWidth - 8) x = rect.right - pw;
  x = Math.max(8, Math.min(x, window.innerWidth - pw - 8));
  if (y + ph > window.innerHeight - 8) y = Math.max(8, rect.top - ph - 6);
  pop.style.left = x + 'px';
  pop.style.top = y + 'px';
  input.focus();
  input.select();
}

function closeWsMigratePop() {
  $('#ws-migrate-pop').classList.add('hidden');
  wsMigrateSessions = [];
}

// pointerdown 关闭（同 resident-pop）；落在面板/按钮上的跳过
document.addEventListener('pointerdown', (e) => {
  if (e.target.closest('#ws-migrate-pop') || e.target.closest('.group-migrate-btn')) return;
  closeWsMigratePop();
});

$('#ws-migrate-input').onkeydown = (e) => {
  if (e.key === 'Enter' && !e.isComposing) { e.preventDefault(); submitWsMigrate(); }
  else if (e.key === 'Escape') { e.preventDefault(); closeWsMigratePop(); }
};

$('#ws-migrate-submit').onclick = submitWsMigrate;

async function submitWsMigrate() {
  const to = $('#ws-migrate-input').value.trim();
  const sessions = wsMigrateSessions;
  if (!to || !sessions.length) return;
  const status = $('#ws-migrate-status');
  const btn = $('#ws-migrate-submit');
  status.classList.remove('err');
  status.textContent = `正在迁移 ${sessions.length} 个会话…`;
  btn.disabled = true;
  const ok = []; // {id,name}
  const fail = [];
  for (const s of sessions) {
    try {
      await api(`/api/sessions/${s.id}`, { method: 'PATCH', body: JSON.stringify({ cwd: to }) });
      ok.push({ id: s.id, name: s.name || s.id.slice(0, 8) });
    } catch (err) {
      fail.push(`${s.name || s.id.slice(0, 8)}：${err.message}`);
    }
  }
  btn.disabled = false;
  if (fail.length) {
    status.textContent = `成功 ${ok.length}，失败 ${fail.length}：` + fail.join('；');
    status.classList.add('err');
  } else {
    status.textContent = `已迁移 ${ok.length} 个会话`;
  }
  loadSessions();
  // 当前打开的会话若被迁移，重连刷新（服务端从新 cwd 回放）
  if (state.current && ok.some((x) => x.id === state.current.id)) {
    openSession(state.current.id);
  }
}

// toggle archived flag based on current state (looked up fresh)
async function toggleArchive(s) {
  const fresh = await api(`/api/sessions/${s.id}`);
  if (fresh.archived) return unarchiveSession(s);
  return archiveSession(s);
}

// 重命名目标独立保存：菜单点击会先 closeSessionMenu() 把
// menuTargetSession 置 null，保存时不能再依赖它
let renameTarget = null;

function openRenameDialog(s) {
  renameTarget = s;
  const input = $('#rename-input');
  input.value = s.name;
  $('#modal-rename').classList.remove('hidden');
  setTimeout(() => input.focus(), 0);
}

function closeRenameDialog() {
  $('#modal-rename').classList.add('hidden');
  renameTarget = null;
}

async function submitRename() {
  const name = $('#rename-input').value.trim();
  if (!name) return;
  const s = renameTarget;
  closeRenameDialog();
  if (s) {
    await api(`/api/sessions/${s.id}`, {
      method: 'PATCH', body: JSON.stringify({ name }),
    });
    s.name = name;
    loadSessions();
  }
}

$('#rename-submit').onclick = submitRename;

// 回车保存、Esc 取消；isComposing 防止中文输入法选词的回车误提交
$('#rename-input').onkeydown = (e) => {
  if (e.key === 'Enter' && !e.isComposing) { e.preventDefault(); submitRename(); }
  else if (e.key === 'Escape') { e.preventDefault(); closeRenameDialog(); }
};

async function forkSession(s) {
  try {
    const newS = await api(`/api/sessions/${s.id}/fork`, { method: 'POST' });
    await loadSessions();
    openSession(newS.id);
  } catch (e) { alert('分叉失败：' + e.message); }
}

async function archiveSession(s) {
  try {
    await api(`/api/sessions/${s.id}`, {
      method: 'PATCH', body: JSON.stringify({ archived: true }),
    });
    if (state.current && state.current.id === s.id) {
      state.current = null;
      $('#chat-view').classList.add('hidden');
      $('#empty-state').classList.remove('hidden');
    }
    await loadSessions();
  } catch (e) { alert('归档失败：' + e.message); }
}

async function unarchiveSession(s) {
  try {
    await api(`/api/sessions/${s.id}`, {
      method: 'PATCH', body: JSON.stringify({ archived: false }),
    });
    await loadSessions();
  } catch (e) { alert('取消归档失败：' + e.message); }
}

async function deleteSession(s) {
  if (!confirm(`彻底删除 "${s.name}"？前端历史会清掉，磁盘上的 claude jsonl 不会被删除（仍可用 --resume 恢复）。`)) return;
  await api(`/api/sessions/${s.id}`, { method: 'DELETE' });
  if (state.current && state.current.id === s.id) {
    state.current = null;
    $('#chat-view').classList.add('hidden');
    $('#empty-state').classList.remove('hidden');
  }
  await loadSessions();
}

// archive section toggle
$('#archive-header').onclick = () => {
  $('#archive-section').classList.toggle('collapsed');
};

// ---------- search bar ----------
$('#btn-search-toggle').onclick = () => {
  state.showSearch = !state.showSearch;
  $('#search-bar').classList.toggle('hidden', !state.showSearch);
  if (state.showSearch) {
    setTimeout(() => $('#search-input').focus(), 0);
  } else {
    $('#search-input').value = '';
    state.searchQuery = '';
    renderSessionList();
  }
};
$('#btn-search-close').onclick = () => $('#btn-search-toggle').click();
// Esc 关闭搜索栏的分支挂在文末全局 keydown 的层级链上（最上层，见文件底部）
$('#search-input').addEventListener('input', (e) => {
  state.searchQuery = e.target.value;
  renderSessionList();
});

// ---------- view menu ----------
const collapsedDirs = new Set(JSON.parse(localStorage.getItem('cc-webui-collapsed-groups') || '[]'));

// load view prefs from localStorage
try {
  const v = JSON.parse(localStorage.getItem('cc-webui-view') || '{}');
  if (v.group) state.viewGroup = v.group;
  if (v.sort) state.viewSort = v.sort;
} catch {}
function saveView() {
  localStorage.setItem('cc-webui-view', JSON.stringify({ group: state.viewGroup, sort: state.viewSort }));
}

function refreshViewMenuChecks() {
  const menu = $('#view-menu');
  menu.querySelectorAll('[data-view-group]').forEach((b) => {
    b.classList.toggle('check', b.dataset.viewGroup === state.viewGroup);
  });
  menu.querySelectorAll('[data-view-sort]').forEach((b) => {
    b.classList.toggle('check', b.dataset.viewSort === state.viewSort);
  });
}

function openViewMenu(anchor) {
  const menu = $('#view-menu');
  refreshViewMenuChecks();
  menu.classList.remove('hidden');
  const rect = anchor.getBoundingClientRect();
  const mw = 200, mh = 200;
  let x = rect.right + 4;
  let y = rect.bottom + 4;
  if (x + mw > window.innerWidth - 8) x = rect.left - mw - 4;
  if (y + mh > window.innerHeight - 8) y = rect.top - mh - 8;
  if (y < 8) y = 8;
  menu.style.left = x + 'px';
  menu.style.top = y + 'px';
  renderIcons(menu);
}

$('#btn-view-toggle').onclick = (e) => {
  e.stopPropagation();
  const menu = $('#view-menu');
  if (menu.classList.contains('hidden')) openViewMenu(e.currentTarget);
  else closeViewMenu();
};

function closeViewMenu() { $('#view-menu').classList.add('hidden'); }

document.addEventListener('click', (e) => {
  if (!e.target.closest('#view-menu') && !e.target.closest('#btn-view-toggle')) closeViewMenu();
});

$('#view-menu').addEventListener('click', (e) => {
  const g = e.target.closest('[data-view-group]');
  const s = e.target.closest('[data-view-sort]');
  if (g) { state.viewGroup = g.dataset.viewGroup; saveView(); refreshViewMenuChecks(); renderSessionList(); }
  if (s) { state.viewSort = s.dataset.viewSort; saveView(); refreshViewMenuChecks(); renderSessionList(); }
});

// ---------- 常驻（peer messaging）面板 ----------
// 右上角 radio 按钮弹出的面板：常驻开关 + 地址名。常驻 = 长驻 streaming-input
// 进程，注册地址名后可被其他 Claude 会话 SendMessage。status.resident /
// status.registered_name 反映进程真实在线情况，打开面板时拉最新值。
function updateResidentUI() {
  const s = state.current;
  const on = !!(s && s.resident);
  $('#btn-resident').classList.toggle('active', on);
  const pop = $('#resident-pop');
  if (pop.classList.contains('hidden')) return;
  $('#resident-toggle').checked = on;
  const st = (s && s.status) || {};
  const el = $('#resident-status');
  if (!on) {
    el.textContent = '未开启——会话是一次性进程，收不到其他会话的消息';
  } else if (!st.resident) {
    el.textContent = '进程启动中…';
  } else {
    // effective = PeerName || CleanPeerName(Name)，跟后端 effectivePeerName 同口径。
    const eff = (s.peer_name || s.name || '').trim();
    const reg = (st.registered_name || '').trim();
    if (reg && eff && reg === eff) {
      el.textContent = `在线 · 注册名 ${reg}`;
    } else if (reg && eff && reg !== eff) {
      el.textContent = `在线 · 待重命名为 ${eff}（当前 ${reg}）`;
    } else if (eff) {
      el.textContent = `在线 · 注册中（将注册为 ${eff}）`;
    } else {
      el.textContent = '在线 · 等待首轮消息后注册地址';
    }
  }
}

function closeResidentPop() {
  $('#resident-pop').classList.add('hidden');
  if (residentPopPoll) { clearInterval(residentPopPoll); residentPopPoll = null; }
}

// 面板打开期间每 1s 拉一次状态：spawn 后 procBootstrap 写好注册名时，
// "将注册为 X" 就能立刻变成 "注册名 X"。
let residentPopPoll = null;

$('#btn-resident').onclick = async (e) => {
  e.stopPropagation();
  const pop = $('#resident-pop');
  if (!pop.classList.contains('hidden')) return closeResidentPop();
  if (!state.current) return;
  // 打开前拉最新状态：注册名/在线情况是进程侧状态，可能刚变
  try { state.current = await api(`/api/sessions/${state.current.id}`); } catch (_) {}
  const s = state.current;
  $('#resident-name').value = s.peer_name || '';
  $('#resident-name').placeholder = s.name || '默认取会话名';
  updateResidentUI();
  pop.classList.remove('hidden');
  if (residentPopPoll) clearInterval(residentPopPoll);
  residentPopPoll = setInterval(async () => {
    if (!state.current || pop.classList.contains('hidden')) return;
    try {
      state.current = await api(`/api/sessions/${state.current.id}`);
      updateResidentUI();
    } catch (_) {}
  }, 1000);
  const rect = $('#btn-resident').getBoundingClientRect();
  const pw = 320, ph = 200;
  let x = rect.right - pw;
  let y = rect.bottom + 6;
  x = Math.max(8, Math.min(x, window.innerWidth - pw - 8));
  if (y + ph > window.innerHeight - 8) y = Math.max(8, rect.top - ph - 6);
  pop.style.left = x + 'px';
  pop.style.top = y + 'px';
};

// pointerdown 关闭（同 session-menu 的理由）；落在按钮/面板上的跳过
document.addEventListener('pointerdown', (e) => {
  if (e.target.closest('#resident-pop') || e.target.closest('#btn-resident')) return;
  closeResidentPop();
});

// 开关常驻：后端 EnsureResident 要等初始化+注册完成，可能花几秒，期间禁用开关
$('#resident-toggle').onchange = async (e) => {
  if (!state.current) return;
  const on = e.target.checked;
  e.target.disabled = true;
  try {
    state.current = await api(`/api/sessions/${state.current.id}`, {
      method: 'PATCH', body: JSON.stringify({ resident: on }),
    });
  } catch (err) {
    alert((on ? '开启常驻失败：' : '关闭常驻失败：') + err.message);
  }
  e.target.disabled = false;
  updateResidentUI();
  renderSessionList(); // 侧栏"常驻"徽章
};

// 地址名：Enter 提交、Esc 还原；失焦（change）也算提交
$('#resident-name').onkeydown = (e) => {
  if (e.key === 'Enter' && !e.isComposing) { e.preventDefault(); e.target.blur(); }
  else if (e.key === 'Escape') {
    e.preventDefault();
    e.target.value = (state.current && state.current.peer_name) || '';
    e.target.blur();
  }
};
$('#resident-name').onchange = async (e) => {
  if (!state.current) return;
  const v = e.target.value.trim();
  if (v === (state.current.peer_name || '')) return;
  e.target.disabled = true;
  try {
    state.current = await api(`/api/sessions/${state.current.id}`, {
      method: 'PATCH', body: JSON.stringify({ peer_name: v }),
    });
    e.target.value = state.current.peer_name || '';
  } catch (err) {
    e.target.value = (state.current && state.current.peer_name) || '';
    alert('修改地址名失败：' + err.message);
  }
  e.target.disabled = false;
  updateResidentUI();
};

// ---------- current session ----------
async function openSession(id) {
  const s = await api(`/api/sessions/${id}`);
  state.current = s;
  // 待发送消息按会话留在 pendingBySid 里，切换不清空；下面随视图重建恢复显示。
  state.seenSeq = new Set();
  state.toolBlocks = {};
  state.toolTimers = {};
  state.permCards = {};
  if (typeof permDock === 'function') {
    const dock0 = permDock();
    if (dock0) { dock0.innerHTML = ''; syncPermDock(); }
  }
  state.subagentCards = {};
  state.agentToolMeta = {};
  state.historyBuf = [];
  state.histRenderedFrom = 0;
  state.replaying = true;
  state.live = null;
  // opencode 流式气泡不属于 state.live，切会话/重连时单独收尾。
  if (state.ocBubble) { liveFinalizeBlock(state.ocBubble); state.ocBubble = null; }
  state.ocPartID = ''; state.ocBuf = '';
  taskFoldReset();
  state.stats = freshStats();
  updateStatsUI();
  trajReset();
  switchChatTab('chat');
  $('#empty-state').classList.add('hidden');
  $('#new-session-view').classList.add('hidden');
  $('#group-view').classList.add('hidden');
  relocatePermCards();
  $('#chat-view').classList.remove('hidden');
  renderGoalBar();
  // 清 DOM 前先摘 observer，防悬空引用 + 旧会话文本闪现
  hidePinnedPrompt();
  $('#messages').innerHTML = '';
  // 恢复本会话的待发送 chip（若有）——chip 挂在输入框卡片上，不随消息流重建。
  renderPendingBubble();
  syncTaskPanel(); // 清空上一会话的任务面板（回放 flush 后用新会话的事件重建）
  $('#error-banner').classList.add('hidden');
  $('#session-name').value = s.name;
  $('#session-cwd').value = s.cwd || '';
  $('#session-cwd').title = s.cwd || '';
  $('#session-perm').value = s.permission_mode || '';
  const engine = s.engine || 'claude';
  fillCLISelect($('#session-cli'), s.cli, engine);
  fillProviderSelect($('#session-provider'), s.provider, engine);
  fillModelSelect($('#session-model'), s.provider, s.model_override, s.cli);
  $('#session-effort').value = s.effort_override || '';
  setEngineUI(engine);
  // Restore draft (text + attachments) for this session.
  state.attached = (state.drafts[id] && state.drafts[id].attachments) || [];
  $('#input').value = (state.drafts[id] && state.drafts[id].text) || '';
  input.style.height = 'auto';
  input.style.height = Math.min(input.scrollHeight, 180) + 'px';
  renderInputMirror(); // 高度定案后再镜像，镜像高度与 scrollTop 同步才有意义
  updateStatusUI(s.status);
  updateResidentUI();
  applyWorkspaceGate(s.cwd);
  applyAttachGate();
  renderAttachedThumbs();
  if (s.unread) patchSessionFlag(s.id, { unread: false }); // 点开即视为已查看：蓝点恢复灰
  renderSessionList();
  renderWsActivity();
  connectEvents(id);
  fetchPendingPermissions(id); // 重连/刷新后补回未决审批卡（transient 事件不进回放）
  refreshSessionEffort(); // opencode 会话按模型 variants 重建推理等级下拉（异步）
  // 切会话后光标直接进输入框，省一次点击；无 cwd 时输入框被禁用则不 focus，
  // 移动端也不 focus——自动弹软键盘会挡住刚加载的历史。
  if (!isMobile() && !input.disabled) {
    input.focus();
    input.setSelectionRange(input.value.length, input.value.length);
  }
}

// ---------- 对话 / 轨迹 tab ----------
// 轨迹视图（trajectory.js）复用同一份事件流投影成台账；composer 仅在对话页显示，
// 切到轨迹页时隐藏。
function switchChatTab(tab) {
  document.querySelectorAll('.chat-tab').forEach((b) => {
    b.classList.toggle('active', b.dataset.tab === tab);
  });
  const isTraj = tab === 'traj';
  $('#msg-wrap').classList.toggle('hidden', isTraj);
  $('#traj-view').classList.toggle('hidden', !isTraj);
  $('#composer').classList.toggle('hidden', isTraj);
  if (isTraj) trajShow();
}
document.querySelectorAll('.chat-tab').forEach((b) => {
  b.onclick = () => switchChatTab(b.dataset.tab);
});

// 在没有 cwd 时禁用 composer
function applyWorkspaceGate(cwd) {
  const gate = $('#workspace-gate');
  const disabled = !cwd;
  gate.classList.toggle('hidden', !disabled);
  $('#input').disabled = disabled;
  $('#btn-send').disabled = disabled;
  $('#btn-attach').disabled = disabled;
  if (disabled) {
    $('#input').placeholder = '请先在新建会话时填写工作目录';
  } else {
    updateMobileInputHint();
  }
}

// 常驻/peer 消息是 Claude Code 独占的协议面（要 stream-json 输入 + 进程注册表），
// 其他引擎不提供这个入口（服务端同样 400 拒绝），把控件藏起来。推理等级不再随
// 引擎一刀切隐藏：opencode 走 --variant、mcode/qoder 走各自的 effort 集合，都由
// refreshSessionEffort 按模型元数据填充/隐藏（openSession 尾部触发）。
function setEngineUI(engine) {
  $('#btn-resident').classList.toggle('hidden', (engine || 'claude') !== 'claude');
}

// 会话菜单随目标会话（而非当前打开的会话）引擎过滤；菜单每次打开前重设。
// fork 需要的是"可复制的 transcript 文件"，claude 与 qoder 都有；opencode /
// mcode 的会话存在各自 CLI 的库里，复制不出来。
function syncMenuForEngine(s) {
  const forkItem = document.querySelector('#session-menu [data-action="fork"]');
  if (forkItem) {
    const kind = s.engine || 'claude';
    forkItem.classList.toggle('hidden', kind !== 'claude' && kind !== 'qoder');
  }
}

// 附件按钮：任意模型都能拿文件让 agent 用工具处理（图片仍要求 input 含 image），
// 只在无 cwd 时禁用。
function applyAttachGate() {
  if (!state.current) return;
  const prov = state.providers.find((p) => p.name === state.current.provider);
  // opencode 的模型列表在 CLI 目录里，config 是空的，所以判定也得走目录。
  // 先用已缓存的（模型下拉刚拉过，命中率高）同步判一次；没命中再异步拉，拉到
  // 后回调里重判——否则首屏在目录回来之前按钮一直是禁用的。
  const cached = ocModelsCached(state.current.cli, state.current.provider);
  const gate = (models) => {
    const m = models.find((x) => x.name === state.current.model_override);
    const allow = !!(prov && m);
    const btn = $('#btn-attach');
    btn.disabled = !allow || $('#input').disabled;
    btn.title = allow
      ? '添加图片或文件（可粘贴/拖拽）'
      : '当前模型未声明能力；如需附件请检查模型配置';
  };
  gate(cached.length ? cached : providerModels(state.current.provider));
  if (!cached.length && state.current.cli) {
    const cli = state.current.cli;
    const provider = state.current.provider;
    const kind = provKind(provider);
    // opencode / mcode / qoder / codex 的模型清单是 CLI 侧的，config 里通常
    // 为空；这里异步拉一次再判，避免首屏附件按钮一直是禁用的。
    if (kind === 'opencode' || kind === 'mcode' || kind === 'qoder' || kind === 'codex') {
      modelsForProvider(provider, cli).then((models) => {
        // 期间可能已经切了会话/provider，判定必须落在当前这一组上。
        if (!state.current || state.current.cli !== cli || state.current.provider !== provider) return;
        gate(models.length ? models : providerModels(provider));
      });
    }
  }
}

function fillSelect(sel, items, value, formatter) {
  sel.innerHTML = '';
  for (const item of items) {
    const opt = document.createElement('option');
    opt.value = item.name;
    opt.textContent = formatter ? formatter(item) : item.name;
    sel.appendChild(opt);
  }
  if (value) sel.value = value;
}

// clis/providers 各带 kind（"claude"|"opencode"|"mcode"|"qoder"，config 归一化后必有值）。
// 会话绑定单一 engine（不做跨引擎切换），所以按 kind 过滤下拉选项。
const kindOf = (item) => (item && item.kind) || 'claude';
const cliKind = (name) => kindOf(state.clis.find((c) => c.name === name));
const provKind = (name) => kindOf(state.providers.find((p) => p.name === name));

// 引擎是否接受内嵌 base64 图片。
// claude 与 qoder 都吃：qoderclicn 1.1.65 实测能正确读出 base64 PNG 的内容，
// 都走 --input-format stream-json 的多模态载荷。
// opencode 走 `opencode serve` 的 prompt file part，同样原样内嵌。
// mcode 与 codex 没有内嵌通道，图片降级成文件附件（见 attachFiles /
// nsAttachFiles）；codex 另外会把图片文件的路径以 -i 传给 CLI。
const engineTakesImages = (kind) => kind === 'claude' || kind === 'qoder' || kind === 'opencode';

function fillCLISelect(sel, value, kind) {
  fillSelect(sel, kind ? state.clis.filter((c) => kindOf(c) === kind) : state.clis, value,
    (c) => (kindOf(c) === 'claude' ? c.name : `${c.name} · ${c.kind}`));
}

// provider 的 models 一律经此取，永远返回数组。服务端已保证空列表序列化成
// [] 而非 null，但前端不能依赖这一点：opencode / mcode / qoder 的 provider
// 正常就是 models 为空（模型由 CLI 运行时解析、经 /api/opencode-models 等取），
// 一旦某条路径拿到 null，`prov.models[0]` 会抛 TypeError 并中断 openSession
// 的后续渲染——表现为整个对话区空白，连刚发的 user prompt 都不显示。
function providerModels(providerName) {
  const p = state.providers.find((x) => x.name === providerName);
  return (p && Array.isArray(p.models)) ? p.models : [];
}

function fillProviderSelect(sel, value, kind) {
  fillSelect(sel, kind ? state.providers.filter((p) => kindOf(p) === kind) : state.providers, value);
}

// ---- 推理深度下拉（engine-aware）----
// claude 引擎用固定 --effort 集合（HTML 静态选项）；opencode 引擎走
// --variant，可选项 = 该模型 metadata 的 variants 键，模型间差异很大
// （gpt-5.6-luna 有六个、kimi-k3 只有 max、minimax-m3 是 none/thinking，
// 还有模型完全没有）。opencode 会话/新建视图按所选模型重建下拉，
// claude 会话恢复静态集合；无 variants 的模型隐藏控件。
const EFFORT_STATIC = {}; // select id -> 静态 HTML（claude 全集）

// 页面加载即快照（script 在 body 尾部，元素已就绪）：任何重建都发生在用户
// 操作之后，这里保证 claude 全集永远是初始 HTML。effortStaticHTML 的懒
// 缓存只作兜底——首个打开的会话若是 opencode 它也不会被污染（opencode
// 分支不读静态 HTML）。
for (const eid of ['session-effort', 'sheet-effort', 'ns-effort', 'new-effort']) {
  // 注意 '#'：$ 就是 querySelector，裸 id 会被当成标签名，永远匹配不到，
  // 快照会整张表都落空（只剩下面的懒缓存兜底）。
  const el = $('#' + eid);
  if (el) EFFORT_STATIC[eid] = el.innerHTML;
}

function effortStaticHTML(sel) {
  if (!EFFORT_STATIC[sel.id]) EFFORT_STATIC[sel.id] = sel.innerHTML;
  return EFFORT_STATIC[sel.id];
}

// 运行时动态建的 effort 下拉（群成员行、成员档案面板）没有 id，进不了上面的
// 按 id 快照，而懒缓存会把「第一次重建前的 innerHTML」当成 claude 全集——先开过
// opencode 的成员再切回 claude 就会拿到一份 opencode 的键。这里留一份 claude
// 模板，动态下拉由调用方显式传进 applyEffortOptions。
const EFFORT_CLAUDE_HTML = EFFORT_STATIC['ns-effort'] || EFFORT_STATIC['sheet-effort'] || '';

// 按 engine + 模型 variants 键重建/恢复一个 effort 下拉并决定显隐。
// wrap 可选：随下拉一起显隐的外层 label（sheet/new 面板里的 select 裸着
// 不好看）。staticHTML 可选：动态下拉的 claude 模板，见 EFFORT_CLAUDE_HTML。
// 重建时保留当前选中值（能对上才恢复）。
function applyEffortOptions(sel, kind, keys, wrap, staticHTML) {
  const show = (v) => {
    sel.classList.toggle('hidden', !v);
    if (wrap) wrap.classList.toggle('hidden', !v);
  };
  if (kind === 'claude') {
    const cur = sel.value;
    sel.innerHTML = staticHTML || effortStaticHTML(sel);
    if (cur) sel.value = cur;
    show(true);
    return;
  }
  if (!keys || !keys.length) {
    show(false);
    return;
  }
  const cur = sel.value;
  const firstText = sel.options[0] ? sel.options[0].textContent : '默认';
  sel.innerHTML = '';
  const def = document.createElement('option');
  def.value = '';
  def.textContent = firstText;
  sel.appendChild(def);
  for (const k of keys) {
    const opt = document.createElement('option');
    opt.value = k;
    opt.textContent = k;
    sel.appendChild(opt);
  }
  if (keys.includes(cur)) sel.value = cur;
  show(true);
}

// 取模型 variants（服务端进程内缓存，前端缓存带 TTL）。缓存不能永久：
// opencode 侧新模型上线后旧映射里没有对应条目，effort 下拉会凭空消失
// （选别的模型又正常），而用户不该为此硬刷新。客户端每次真拉都带 fresh=1
// 绕开服务端同样 10 分钟的缓存（页面加载正好落在服务端旧缓存窗口内时，
// 不带 fresh 会继续拿到旧映射）；config 热加载改了模型列表则立即作废
// （见 refreshProfiles）。
const OC_VARIANTS_TTL = 10 * 60 * 1000;
async function ocVariantsFor(cliName, providerName) {
  const key = `${cliName}/${providerName}`;
  const ent = state.ocVariants[key];
  if (ent && Date.now() - ent.ts < OC_VARIANTS_TTL) return ent.data;
  try {
    const data = await api(`/api/opencode-model-variants?cli=${encodeURIComponent(cliName)}&provider=${encodeURIComponent(providerName)}&fresh=1`);
    const models = (data && data.models) || null;
    state.ocVariants[key] = { ts: Date.now(), data: models };
    return models;
  } catch (_) {
    // 拉取失败（典型：cc-webui 重启期间连接被拒）——只返回 null，不写缓存。
    // 若把失败也存进去，一次启动窗口内的失败会把 effort 下拉整整藏 10 分钟，
    // 服务早就起来了也不恢复；不缓存则下次 openSession / 换模型即重试。
    return null;
  }
}

// 用 config 的 model 名从 variants 映射里取键。映射的键是 opencode CLI 打印的
// <providerID>/<modelID> 全路径，但 config 的 model 名两种形态都有：多数 provider
// 是裸 modelID（openCodeModelID 运行时补前缀），openrouter 那类带斜杠的模型名是
// 全路径原样配置（走 openCodeModelID 的原样透传）。两种都能对上，不能只查一种——
// 只按裸名查会让 opencode-go 整条 provider 的下拉全空。
function variantKeysFor(v, providerName, modelName) {
  if (!v || !modelName) return [];
  return v[modelName] || v[`${providerName}/${modelName}`] || [];
}

// ---- mcode 模型目录 ----
// mcode 的可用模型是动态的：`mcode provider list --json` 对每个 provider 都返回
// `models: []`（真实集合由凭据源在运行时解析），ACP 的 session/new 又要先完成
// 整轮 JSON-RPC 握手才肯应答。真正的清单在它自己的运行时配置里，服务端读出来
// 经 /api/mcode-models 提供（见 mcodehist.LoadCatalog）。
//
// 因此 mcode provider 的模型下拉不取 config 里的静态 models，而是拉这份目录；
// 缓存策略与 opencode variants 一致（10 分钟 TTL + 每次真拉带 fresh=1），这样
// 新装的模型不必硬刷新页面就出现。
const MCODE_CATALOG_TTL = 10 * 60 * 1000;
async function mcodeCatalogFor(cliName, providerName) {
  const key = `${cliName}/${providerName}`;
  const ent = state.mcodeCatalog[key];
  if (ent && Date.now() - ent.ts < MCODE_CATALOG_TTL) return ent.data;
  let data = null;
  try {
    data = await api(`/api/mcode-models?cli=${encodeURIComponent(cliName)}&provider=${encodeURIComponent(providerName)}&fresh=1`);
  } catch (_) {
    // 拉取失败 = 读不到 mcode 自己的配置，保持空目录，不打扰对话
  }
  state.mcodeCatalog[key] = { ts: Date.now(), data: data || null };
  return data;
}

// mcodeEffortsFor 取某模型的推理档位。目录的键是裸 modelId，而 config 里的
// model 名可能已带 provider 前缀，两种都试。
function mcodeEffortsFor(catalog, providerName, modelName) {
  if (!catalog || !modelName || !Array.isArray(catalog.models)) return [];
  const bare = modelName.includes('/') ? modelName.slice(modelName.indexOf('/') + 1) : modelName;
  const m = catalog.models.find((x) => x.id === modelName || x.id === bare);
  return (m && m.efforts) || [];
}

// ---- opencode 模型目录 ----
// opencode 的可用模型不在 config 里静态声明，而是 CLI 自己的注册表
// （`opencode models <provider> --verbose`）：模型上下线、套餐变化都由上游决定，
// 写死必然过期。下拉改取这份目录（见 ocModelsFor），effort 下拉取同一份目录里的
// variants（ocVariantsFor）——服务端两个端点共用一个 memo，所以一次拉取两样都有。
// 缓存策略与 mcode / qoder 目录一致（10 分钟 TTL）。
const OC_MODELS_TTL = 10 * 60 * 1000;
async function ocModelsFor(cliName, providerName) {
  const key = `${cliName}/${providerName}`;
  const ent = state.ocModels[key];
  if (ent && Date.now() - ent.ts < OC_MODELS_TTL) return ent.data;
  let data = null;
  try {
    data = await api(`/api/opencode-models?cli=${encodeURIComponent(cliName)}&provider=${encodeURIComponent(providerName)}&fresh=1`);
  } catch (_) {
    // 目录读不到 = CLI 没装/没登录或 providerID 不对。保持 null，让调用方退回
    // config 的静态列表（旧配置还没清空 models 时仍然可用），而不是显示空下拉。
  }
  state.ocModels[key] = { ts: Date.now(), data: data || null };
  return data;
}

// 同步取已缓存的 opencode 模型列表（不触发请求）：applyAttachGate 这类需要在
// 当前调用栈里立刻判定的路径用。缓存未命中返回 []，调用方据此退回 config。
function ocModelsCached(cliName, providerName) {
  const ent = state.ocModels[`${cliName}/${providerName}`];
  return (ent && ent.data && Array.isArray(ent.data.models)) ? ent.data.models : [];
}

// ---- qoder 模型目录 ----
// qoder（Qoder CLI CN）的可用模型由账号决定，CLI 自己知道：`qoderclicn
// --list-models`。写死进 config.yaml 必然过期（换套餐/CLI 升级就变），所以模型
// 下拉与推理档位都拉这份目录——档位同样是 CLI 侧的固定集合（--reasoning-effort
// 认 auto/none/low/medium/high/xhigh/max/ultracode），由服务端一并下发，前端
// 不再自带一份副本。缓存策略与 opencode variants / mcode 目录一致。
const QODER_CATALOG_TTL = 10 * 60 * 1000;
async function qoderCatalogFor(cliName, providerName) {
  const key = `${cliName}/${providerName}`;
  const ent = state.qoderCatalog[key];
  if (ent && Date.now() - ent.ts < QODER_CATALOG_TTL) return ent.data;
  let data = null;
  try {
    data = await api(`/api/qoder-models?cli=${encodeURIComponent(cliName)}&provider=${encodeURIComponent(providerName)}&fresh=1`);
  } catch (_) {
    // 拉取失败 = CLI 没装或没登录，保持空目录，不打扰对话
  }
  state.qoderCatalog[key] = { ts: Date.now(), data: data || null };
  return data;
}

// qoder 的档位与模型无关（CLI 侧固定集合），所以整个 provider 共用一份。
function qoderEfforts(catalog) {
  return (catalog && Array.isArray(catalog.efforts)) ? catalog.efforts : [];
}

// ---- codex 模型目录 ----
// codex 的模型清单编译在 CLI 里（`codex debug models --bundled`），随 CLI
// 升级和账号档位变化，写进 config 必然过期。服务端解析后经 /api/codex-models
// 下发；ducx 是同一协议，只是 home 和内置模型不同。缓存策略与其它目录一致。
const CODEX_CATALOG_TTL = 10 * 60 * 1000;
async function codexCatalogFor(cliName, providerName) {
  const key = `${cliName}/${providerName}`;
  const ent = state.codexCatalog[key];
  if (ent && Date.now() - ent.ts < CODEX_CATALOG_TTL) return ent.data;
  let data = null;
  try {
    data = await api(`/api/codex-models?cli=${encodeURIComponent(cliName)}&provider=${encodeURIComponent(providerName)}&fresh=1`);
  } catch (_) {
    // 拉取失败 = CLI 太老不支持 debug models；退回 config 的静态列表
  }
  state.codexCatalog[key] = { ts: Date.now(), data: data || null };
  return data;
}

function codexEffortsFor(catalog, modelName) {
  if (!catalog || !modelName || !Array.isArray(catalog.models)) return [];
  const m = catalog.models.find((x) => x.id === modelName);
  return (m && m.efforts) || [];
}

// 某 provider 的模型列表：opencode / mcode / qoder 走 CLI 侧目录，其余走 config
// 静态列表。
//
// mcode 的模型名带 provider 前缀（"<providerId>/<modelId>"）而不是裸 id：
// runner 的 mcodeModelID 见到斜杠就原样透传，spawn 的 --model 才是 mcode
// 真正认的形式。前缀取自目录返回的 provider_id，不能用 cc-webui 侧的
// provider 名——两者不必同名（config 里的 provider 名只需在本项目内唯一）。
async function modelsForProvider(providerName, cliName) {
  const kind = provKind(providerName);
  if (kind === 'opencode' && cliName) {
    const cat = await ocModelsFor(cliName, providerName);
    if (cat && Array.isArray(cat.models) && cat.models.length) {
      return cat.models;
    }
  }
  if (kind === 'mcode' && cliName) {
    const cat = await mcodeCatalogFor(cliName, providerName);
    if (cat && Array.isArray(cat.models) && cat.models.length) {
      const pid = cat.provider_id || '';
      return cat.models.map((m) => ({
        name: pid ? `${pid}/${m.id}` : m.id,
        input: m.input || [],
      }));
    }
  }
  if (kind === 'qoder' && cliName) {
    const cat = await qoderCatalogFor(cliName, providerName);
    if (cat && Array.isArray(cat.models) && cat.models.length) {
      return cat.models.map((m) => ({ name: m.name, input: m.input || [] }));
    }
  }
  if (kind === 'codex' && cliName) {
    const cat = await codexCatalogFor(cliName, providerName);
    if (cat && Array.isArray(cat.models) && cat.models.length) {
      return cat.models.map((m) => ({ name: m.id, input: m.input || [] }));
    }
  }
  return providerModels(providerName);
}

// 会话视图：按当前会话的 cli/provider/model 刷新 session 与 sheet 两处
// effort 下拉。挂在 rerenderProfileSelects / 各 PATCH 成功 / openSession 尾部。
async function refreshSessionEffort() {
  const s = state.current;
  if (!s) return;
  const engine = s.engine || 'claude';
  const main = $('#session-effort');
  const sheet = $('#sheet-effort');
  const sheetWrap = sheet.closest('label');
  if (engine === 'opencode') {
    // 模型列表来自 CLI 目录而非 config（见 ocModelsFor），所以「默认 = 首个模型」
    // 也得从目录取，否则 model_override 为空时拿不到任何模型名，effort 下拉会
    // 因为无从判断变体而整个隐藏。
    const ms = await modelsForProvider(s.provider, s.cli);
    const model = s.model_override || (ms[0] && ms[0].name) || '';
    const keys = model
      ? variantKeysFor(await ocVariantsFor(s.cli, s.provider), s.provider, model)
      : [];
    applyEffortOptions(main, 'opencode', keys);
    applyEffortOptions(sheet, 'opencode', keys, sheetWrap);
    return;
  }
  if (engine === 'mcode') {
    // mcode 的档位随模型而异（default/low/…/max、none-thinking/thinking，
    // 或模型压根不支持），取自它自己的运行时配置。
    const ms = providerModels(s.provider);
    const model = s.model_override || (ms[0] && ms[0].name) || '';
    const cat = model ? await mcodeCatalogFor(s.cli, s.provider) : null;
    const keys = mcodeEffortsFor(cat, s.provider, model);
    applyEffortOptions(main, 'mcode', keys);
    applyEffortOptions(sheet, 'mcode', keys, sheetWrap);
    return;
  }
  if (engine === 'qoder') {
    const keys = qoderEfforts(await qoderCatalogFor(s.cli, s.provider));
    applyEffortOptions(main, 'qoder', keys);
    applyEffortOptions(sheet, 'qoder', keys, sheetWrap);
    return;
  }
  if (engine === 'codex') {
    const ms = await modelsForProvider(s.provider, s.cli);
    const model = s.model_override || (ms[0] && ms[0].name) || '';
    const keys = codexEffortsFor(await codexCatalogFor(s.cli, s.provider), model);
    applyEffortOptions(main, 'codex', keys);
    applyEffortOptions(sheet, 'codex', keys, sheetWrap);
    return;
  }
  applyEffortOptions(main, 'claude');
  applyEffortOptions(sheet, 'claude', null, sheetWrap);
}

// ns 视图与 quick-create 弹窗：effort 跟随所选 cli/provider/model（读下拉值）
async function refreshNsEffort() {
  const cliName = $('#ns-cli').value;
  const sel = $('#ns-effort');
  const kind = cliKind(cliName);
  if (kind === 'opencode') {
    const v = await ocVariantsFor(cliName, $('#ns-provider').value);
    applyEffortOptions(sel, 'opencode', variantKeysFor(v, $('#ns-provider').value, $('#ns-model').value));
    return;
  }
  if (kind === 'mcode') {
    const cat = await mcodeCatalogFor(cliName, $('#ns-provider').value);
    applyEffortOptions(sel, 'mcode', mcodeEffortsFor(cat, $('#ns-provider').value, $('#ns-model').value));
    return;
  }
  if (kind === 'qoder') {
    applyEffortOptions(sel, 'qoder', qoderEfforts(await qoderCatalogFor(cliName, $('#ns-provider').value)));
    return;
  }
  if (kind === 'codex') {
    const cat = await codexCatalogFor(cliName, $('#ns-provider').value);
    applyEffortOptions(sel, 'codex', codexEffortsFor(cat, $('#ns-model').value));
    return;
  }
  applyEffortOptions(sel, 'claude');
}

async function refreshNewEffort() {
  const cliName = $('#new-cli').value;
  const sel = $('#new-effort');
  const wrap = sel.closest('label');
  const kind = cliKind(cliName);
  if (kind === 'opencode') {
    const v = await ocVariantsFor(cliName, $('#new-provider').value);
    applyEffortOptions(sel, 'opencode', variantKeysFor(v, $('#new-provider').value, $('#new-model').value), wrap);
    return;
  }
  if (kind === 'mcode') {
    const cat = await mcodeCatalogFor(cliName, $('#new-provider').value);
    applyEffortOptions(sel, 'mcode', mcodeEffortsFor(cat, $('#new-provider').value, $('#new-model').value), wrap);
    return;
  }
  if (kind === 'qoder') {
    applyEffortOptions(sel, 'qoder', qoderEfforts(await qoderCatalogFor(cliName, $('#new-provider').value)), wrap);
    return;
  }
  if (kind === 'codex') {
    const cat = await codexCatalogFor(cliName, $('#new-provider').value);
    applyEffortOptions(sel, 'codex', codexEffortsFor(cat, $('#new-model').value), wrap);
    return;
  }
  applyEffortOptions(sel, 'claude', null, wrap);
}

// Model 下拉随 provider 联动；空值 = provider 默认模型（models 第一个）
function renderModelList(sel, models, value, defaultLabel) {
  sel.innerHTML = '';
  const def = document.createElement('option');
  def.value = '';
  def.textContent = models.length ? `默认 (${defaultLabel || models[0].name})` : '(无)';
  sel.appendChild(def);
  for (const m of models) {
    const opt = document.createElement('option');
    opt.value = m.name;
    opt.textContent = m.name;
    sel.appendChild(opt);
  }
  sel.value = value || '';
}

// mcode 的 provider 不在 config 里静态列模型——可用集合由 mcode 自己的运行时
// 配置决定，config 写死会立刻过期。所以这类 provider 走一次异步目录拉取
// （modelsForProvider），并用 dataset.provider 做竞态防护：用户连点切换
// provider 时，先发的请求后到也不能把下拉覆盖成上一个 provider 的模型。
function fillModelSelect(sel, providerName, value, cliName, defaultLabel) {
  const kind = provKind(providerName);
  if ((kind === 'opencode' || kind === 'mcode' || kind === 'qoder' || kind === 'codex') && cliName) {
    sel.dataset.provider = providerName;
    modelsForProvider(providerName, cliName).then((models) => {
      if (sel.dataset.provider !== providerName) return;
      renderModelList(sel, models, value, defaultLabel);
    });
    return;
  }
  const p = state.providers.find((x) => x.name === providerName);
  renderModelList(sel, providerModels(providerName), value);
}

// config 热加载：后台 watch 到 config.yaml 变动后广播 config_changed（或前端
// 轮询兜底），这里重拉 clis/providers，有变动才重建当前会话的下拉，保留已选值。
async function refreshProfiles() {
  try {
    const [clis, providers] = await Promise.all([api('/api/clis'), api('/api/providers')]);
    const sig = JSON.stringify([clis, providers]);
    if (sig === state.profilesSig) return;
    state.profilesSig = sig;
    state.clis = clis;
    state.providers = providers;
    state.ocVariants = {}; // 模型列表变了 → variants 映射可能过期，重建下拉时重拉
    state.ocModels = {};   // 同理：opencode 目录按 cli/provider 缓存，改名/新增后失效
    state.mcodeCatalog = {}; // 同理：mcode provider 改名/新增后旧目录对不上
    state.qoderCatalog = {}; // 同理：qoder 目录按 cli/provider 缓存，改名后失效
    state.codexCatalog = {}; // 同理：codex 目录按 cli/provider 缓存，改名后失效
    rerenderProfileSelects();
  } catch (_) {
    // 网络抖动忽略：下一轮轮询/广播再试
  }
}

// 用最新 state.providers/state.clis 重建「当前会话」可见的 cli/provider/model
// 下拉（刷新选项不触发 change 事件，已选值原样保留；模型被删则退回默认项）。
function rerenderProfileSelects() {
  if (!state.current) return;
  const engine = state.current.engine || 'claude';
  fillCLISelect($('#session-cli'), state.current.cli, engine);
  fillProviderSelect($('#session-provider'), state.current.provider, engine);
  fillModelSelect($('#session-model'), state.current.provider, state.current.model_override, state.current.cli);
  applyAttachGate();
  patchSheetIfOpen(state.current);
  refreshSessionEffort(); // fire-and-forget：opencode/mcode 需拉模型元数据重建 effort
}

function connectEvents(id) {
  if (state.es) state.es.close();
  const es = new EventSource(`/api/sessions/${id}/events`);
  state.es = es;
  // 服务端每条连接的 seq 都从 1 重新编号，而 seenSeq 跨重连保留：EventSource
  // 自动重连后（网络抖动/服务重启/切后台），新 live 事件的 seq 与旧的撞车会被
  // 当重复事件静默丢弃，流式就此"死掉"。重连时重置视图状态，让新的全量回放重建。
  let opened = false;
  es.onopen = () => {
    if (opened) resetChatForReplay();
    opened = true;
  };
  es.onmessage = (e) => {
    const ev = JSON.parse(e.data);
    if (state.seenSeq.has(ev.seq)) return;
    state.seenSeq.add(ev.seq);
    if (ev.kind === 'replay_end') {
      flushHistory();
      trajOnEvent(); // 回放补齐轨迹投影（若轨迹页开着）
      return;
    }
    // Live checkpoint notification: attach a rewind button to the last user
    // message (the turn just ended). Transient — not in replay history; the
    // button is persisted in the user event's data.checkpoint for reconnects.
    if (ev.kind === 'checkpoint') {
      attachCheckpointToLastUser(ev.data && ev.data.checkpoint);
      return;
    }
    // Live rewind broadcast: truncate the UI + show a marker. Replay-time
    // rewind events (already truncated server-side) fall through to render as
    // a history marker via renderEvent.
    if (ev.kind === 'rewind' && !state.replaying) {
      applyRewindInUI(ev, false);
      return;
    }
    // config 热加载广播：刷新 clis/providers 与模型下拉，不渲染为消息
    if (ev.kind === 'config_changed') {
      refreshProfiles();
      return;
    }
    // 权限审批：transient 事件，不进回放缓冲，直接渲染/锁卡
    if (ev.kind === 'permission_request') {
      renderPermissionCard(ev.data);
      return;
    }
    if (ev.kind === 'permission_resolved') {
      resolvePermissionCard(ev.data);
      return;
    }
    // 子代理（Agent 工具）消息：实时流由父进程 stdout 以 parent_tool_use_id
    // 透传；回放侧由服务端 ConvertSubagents 从 subagents/*.jsonl 产出同形
    // 事件。两侧都经 renderEvent 渲染成折叠卡片，但不计入统计/轨迹。
    const isSubagent = ev.kind === 'stream' && ev.data && ev.data.parent_tool_use_id;
    if (!isSubagent) trackStats(ev);
    // Buffer replayed history; render only the latest page when the
    // replay_end marker arrives. Live events render immediately.
    if (state.replaying) {
      state.historyBuf.push(ev);
      return;
    }
    safeRenderEvent(ev);
    // live 事件同样进入 historyBuf，作为轨迹视图的数据源；
    // stream_event 增量太多且轨迹用不到（完整 assistant 事件兜底），不入。
    if (ev.kind !== 'stream' || !ev.data || ev.data.type !== 'stream_event') {
      state.historyBuf.push(ev);
      trajOnEvent();
      if (taskFoldEvent(ev)) syncTaskPanel(); // Task*/TodoWrite 变化 → 更新任务面板
    }
    scrollBottom();
    updateStatsUI();
  };
}

// EventSource 重连后重置会话视图：清空已渲染消息与 seq 去重集合，让服务端
// 新一轮全量回放重建视图（回放覆盖所有已持久化事件，不会丢内容；进行中的
// 流式消息由后续完整 assistant 事件兜底）。乐观气泡保留在末尾。
function resetChatForReplay() {
  // 清 DOM 前先摘悬浮条，防旧会话文本闪现（flushHistory 会重挂）
  if (typeof hidePinnedPrompt === 'function') hidePinnedPrompt();
  const box = $('#messages');
  const optimistic = [...box.querySelectorAll('.msg-optimistic')];
  box.innerHTML = '';
  for (const n of optimistic) box.appendChild(n);
  state.seenSeq = new Set();
  state.toolBlocks = {};
  for (const id of Object.keys(state.toolTimers || {})) {
    try { clearInterval(state.toolTimers[id].timer); } catch (_) {}
  }
  state.toolTimers = {};
  for (const id of Object.keys(state.permCards || {})) {
    const e = state.permCards[id];
    if (e && e.timer) { try { clearInterval(e.timer); } catch (_) {} }
  }
  state.permCards = {};
  const dock = permDock();
  if (dock) { dock.innerHTML = ''; syncPermDock(); }
  state.subagentCards = {};
  state.agentToolMeta = {};
  state.historyBuf = [];
  state.histRenderedFrom = 0;
  state.replaying = true;
  state.live = null;
  // opencode 流式气泡不属于 state.live，切会话/重连时单独收尾。
  if (state.ocBubble) { liveFinalizeBlock(state.ocBubble); state.ocBubble = null; }
  state.ocPartID = ''; state.ocBuf = '';
  taskFoldReset();
  state.stats = freshStats();
  trajReset();
  syncTaskPanel();
  updateLoadMoreBtn();
}

// ---------- session stats (轮数 / 步数 / 累计耗时 / 最近一轮 tok/s) ----------
// 轮数 = user 事件数；步数 = assistant 消息里的 tool_use 块数；
// 耗时 = 首尾事件时间差（随实时事件增长）。
// `last*` 只统计最后一条 user 之后的事件，用来算最近一轮的输出速度。
function freshStats() {
  return {
    turns: 0, steps: 0, firstTs: 0, lastTs: 0, llmMs: 0, toolMs: 0, llmEstMs: 0, toolEstMs: 0, prevTs: 0,
    lastOut: 0, lastLlmMs: 0, lastLlmEstMs: 0, lastSeen: {},
  };
}

function trackStats(ev) {
  const st = state.stats;
  if (!st) return;
  const ts = Date.parse(ev.ts) || 0;
  if (ts) {
    if (!st.firstTs || ts < st.firstTs) st.firstTs = ts;
    if (ts > st.lastTs) st.lastTs = ts;
  }
  if (ev.kind === 'user') {
    st.turns++;
    st.lastOut = 0;
    st.lastLlmMs = 0;
    st.lastLlmEstMs = 0;
    st.lastSeen = {};
  } else if (ev.kind === 'stream' && ev.data && ev.data.type === 'assistant') {
    const msg = ev.data.message || {};
    const content = msg.content;
    if (Array.isArray(content)) {
      for (const b of content) if (b && b.type === 'tool_use') st.steps++;
    }
    // 输出 token 按 message.id 去重：claude 的 transcript 会把同一次 API
    // 调用的 thinking/tool_use 拆成多行，每行重复携带同一份 usage。
    // 没有 id 的 usage（mcode/codex 的整轮 usage 事件）每轮只出现一次。
    if (msg.usage) {
      const mid = msg.id || '';
      if (!mid || !st.lastSeen[mid]) {
        if (mid) st.lastSeen[mid] = true;
        st.lastOut += Number(msg.usage.output_tokens) || 0;
      }
    }
  } else if (ev.kind === 'stream' && ev.data && ev.data.type === 'result') {
    // LLM 耗时 = result.duration_api_ms（API 时间）；
    // 工具耗时 ≈ duration_ms - duration_api_ms（含少量进程开销的近似值）
    const total = ev.data.duration_ms || 0;
    const api = ev.data.duration_api_ms || 0;
    st.llmMs += api;
    st.toolMs += Math.max(0, total - api);
    st.lastLlmMs += api;
  }
  // 无 result 事件的会话（如 claude jsonl 重放的历史）退化为时间差估算：
  // assistant 前的间隙 ≈ LLM 耗时，tool_result 前的间隙 ≈ 工具耗时。
  // 单段封顶 2 分钟，避免权限确认等待等长停顿把估算打爆。
  if (ev.kind === 'stream' && ev.data && ts && st.prevTs) {
    const gap = Math.min(Math.max(0, ts - st.prevTs), 120000);
    if (ev.data.type === 'assistant') {
      st.llmEstMs += gap;
      st.lastLlmEstMs += gap;
    } else if (ev.data.type === 'user') {
      st.toolEstMs += gap;
    }
  }
  if (ts) st.prevTs = ts;
}

function fmtDuration(ms) {
  if (ms < 1000) return '<1 秒';
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s} 秒`;
  const m = Math.floor(s / 60);
  if (m < 60) return s % 60 ? `${m} 分 ${s % 60} 秒` : `${m} 分钟`;
  const h = Math.floor(m / 60);
  if (h < 24) return m % 60 ? `${h} 小时 ${m % 60} 分` : `${h} 小时`;
  const d = Math.floor(h / 24);
  return h % 24 ? `${d} 天 ${h % 24} 小时` : `${d} 天`;
}

function updateStatsUI() {
  const el = $('#session-stats');
  if (!el) return;
  const st = state.stats;
  if (!st || !st.turns) {
    el.textContent = '';
    return;
  }
  let out = `${st.turns} 轮对话 · ${st.steps} 步执行`;
  // 有 result 事件用精确值，否则用相邻事件时间差的估算值
  const exact = st.llmMs || st.toolMs;
  const llm = exact ? st.llmMs : st.llmEstMs;
  const tool = exact ? st.toolMs : st.toolEstMs;
  if (llm || tool) {
    out += `（LLM ${fmtDuration(llm)} · 工具 ${fmtDuration(tool)}${exact ? '' : '，估算'}）`;
  }
  const dur = st.lastTs > st.firstTs ? fmtDuration(st.lastTs - st.firstTs) : '—';
  el.textContent = `${out} · 累计 ${dur}`;
  // 最近一轮的输出速度：分母优先用 result 的精确 API 时间，回放里没有
  // result 就退回相邻事件间隙的估算（前缀 ~ 标明）。没有 usage 的引擎
  // （opencode 的流事件不带 usage）自然不显示。
  const lastMs = st.lastLlmMs || st.lastLlmEstMs || 0;
  if (st.lastOut > 0 && lastMs > 0) {
    const tps = st.lastOut / (lastMs / 1000);
    el.textContent += ` · 最近一轮 ${st.lastLlmMs ? '' : '~'}${tps.toFixed(1)} tok/s`;
  }
}

// 会话里最后一条用户输入在回放缓冲中的下标（没有则 -1）。三个引擎的回放都把真实
// 输入转成 kind:"user" + data.text（claudehist / opencodehist / mcodehist 同口径），
// tool_result 与 peer 消息走 kind:"stream" 不算——所以它就是「DOM 里最后一条
// .msg-user」在缓冲里的对应物。空文本的 user 事件渲染出来是空气泡，一并跳过。
function lastUserEventIndex(buf) {
  for (let i = buf.length - 1; i >= 0; i--) {
    const ev = buf[i];
    if (ev && ev.kind === 'user' && ev.data && typeof ev.data.text === 'string' && ev.data.text.trim()) return i;
  }
  return -1;
}

// Render the latest page of buffered history (called on replay_end).
function flushHistory() {
  // EventSource auto-reconnect replays history again with fresh seqs; the
  // replayed events are dropped by seenSeq dedup, and the duplicate marker
  // must not re-render the buffer.
  if (!state.replaying) return;
  state.replaying = false;
  const box = $('#messages');
  // Optimistic bubbles sent while history was loading belong at the end.
  const optimistic = [...box.querySelectorAll('.msg-optimistic')];
  const buf = state.historyBuf;
  // 首屏窗口必须含最后一条 user 事件：顶部悬浮条钉的就是它（内容取 DOM 里最后一条
  // .msg-user），而一轮几十步工具调用就能在它后面堆出上百条 assistant/tool 事件
  // ——固定 50 条的窗口会把最后一条输入整条截在外面，切会话回来悬浮条凭空消失，
  // 得点几次「加载更多」才回来。窗口只向前扩到那条输入为止，多渲染的正是用户
  // 要看的那一轮，不算浪费；「加载更多」的剩余条数仍是 histRenderedFrom，口径不变。
  const lastUser = lastUserEventIndex(buf);
  state.histRenderedFrom = (lastUser >= 0 && lastUser < buf.length - HISTORY_PAGE)
    ? lastUser
    : Math.max(0, buf.length - HISTORY_PAGE);
  // 历史渲染期间旧 status 事件不得触发副作用（旧 turn_end 会把待发送消息
  // 在轮次仍在跑时误提交；见 renderEvent 的 status 分支）。
  state.renderingHistory = true;
  try {
    for (let i = state.histRenderedFrom; i < buf.length; i++) {
      safeRenderEvent(buf[i], box);
    }
  } finally {
    state.renderingHistory = false;
  }
  // 只回填仍在文档里的乐观气泡：新任务流程（nsSend）里回放已含本条 user
  // 事件，renderEvent 会摘掉已对账的乐观气泡，这里不能把摘掉的又接回来
  //（否则 prompt 显示两次，切走再切回才恢复一条）。
  for (const node of optimistic) if (node.isConnected) box.appendChild(node);
  // 回放事件只渲染了最新一页，但任务面板要覆盖全部历史——从缓冲区全量重折。
  taskFoldAll();
  syncTaskPanel();
  // 切走期间本轮已结束的会话：回放里的旧 turn_end 被上面挡住，在这里按
  // 最新状态补交该会话的待发送消息。
  if (state.current && state.pendingBySid[state.current.id] && !isRunning()) flushPending();
  state.nearBottom = true;
  scrollBottom();
  updateLoadMoreBtn();
  updateStatsUI();
  refreshPinnedPrompt();
}

// Prepend the previous page of buffered history, keeping the viewport
// anchored on the message the user was looking at.
function loadMoreHistory() {
  if (state.histRenderedFrom <= 0) return;
  const box = $('#messages');
  const from = Math.max(0, state.histRenderedFrom - HISTORY_PAGE);
  const frag = document.createDocumentFragment();
  state.renderingHistory = true;
  try {
    for (let i = from; i < state.histRenderedFrom; i++) {
      safeRenderEvent(state.historyBuf[i], frag);
    }
  } finally {
    state.renderingHistory = false;
  }
  state.histRenderedFrom = from;
  const prevHeight = box.scrollHeight;
  const wrap = $('#load-more-wrap');
  box.insertBefore(frag, wrap ? wrap.nextSibling : box.firstChild);
  box.scrollTop += box.scrollHeight - prevHeight;
  updateLoadMoreBtn();
}

// The pill button pinned at the top of the message list while there are
// older buffered events not rendered yet.
function updateLoadMoreBtn() {
  let wrap = $('#load-more-wrap');
  if (state.histRenderedFrom <= 0) {
    if (wrap) wrap.classList.add('hidden');
    return;
  }
  if (!wrap) {
    wrap = document.createElement('div');
    wrap.id = 'load-more-wrap';
    const btn = document.createElement('button');
    btn.className = 'load-more-btn';
    btn.onclick = loadMoreHistory;
    wrap.appendChild(btn);
    const box = $('#messages');
    box.insertBefore(wrap, box.firstChild);
  }
  wrap.firstChild.textContent = `加载更多（还有 ${state.histRenderedFrom} 条）`;
  wrap.classList.remove('hidden');
}

function updateStatusUI(st) {
  const badge = $('#status-badge');
  const stopBtn = $('#btn-stop');
  const hint = $('#queue-hint');
  const spinner = $('#composer-spinner');
  // Keep state.current.status in sync so isRunning() reflects the latest SSE
  // status event immediately (refreshCurrentStatus overwrites it async later).
  if (state.current) {
    state.current.status = { running: !!(st && st.running), queued: (st && st.queued) || 0 };
  }
  // Grey out rewind buttons while a turn is in flight (the API also 409s).
  document.body.classList.toggle('session-running', !!(st && st.running));
  if (st.running) {
    badge.textContent = '● 运行中';
    badge.className = 'status-badge running';
    stopBtn.classList.remove('hidden');
    if (spinner) spinner.classList.remove('hidden');
  } else {
    badge.textContent = '';
    badge.className = 'status-badge';
    stopBtn.classList.add('hidden');
    if (spinner) spinner.classList.add('hidden');
  }
  hint.textContent = st.queued > 0 ? `队列中还有 ${st.queued} 条消息` : '';
}

// ---------- pinned prompt（钉住最后一条输入） ----------
// 悬浮条状态全派生、零持久化：内容 = 最后一条 .msg-user 的 textContent；显隐
// = 本端 IntersectionObserver；展开/折叠 = 本端内存瞬态（切会话/新消息即重置）。
// 单一幂等入口 refreshPinnedPrompt() 处理任何时序（回放/重连/多端乱序）。
const pinnedPrompt = {
  observer: null,
  target: null,
  expanded: false,
  fadeTimer: null,
};

// 幂等重算：摘旧 observer → 取最后一条 .msg-user → 无则隐藏，有则更新文本并
// 复位折叠/滚动/渐隐后重挂。任何调用点重复调用都安全。
function refreshPinnedPrompt() {
  if (pinnedPrompt.observer) {
    pinnedPrompt.observer.disconnect();
    pinnedPrompt.observer = null;
  }
  const pp = $('#pinned-prompt');
  const msgs = $('#messages');
  const text = $('#pp-text');
  if (!pp || !text) return;
  const users = msgs.querySelectorAll('.msg-user');
  if (!users.length) { hidePinnedPrompt(); return; }
  const last = users[users.length - 1];
  // 只取 .bubble 文本：rewind 按钮（「回滚」）与 role-tag（「你」+ 时间）挂在
  // .msg-user 直接子级，用整条 textContent 会把 UI 文字带进悬浮条；附件 chip
  // 文件名在 bubble 内，仍包含。兜底（无 .bubble 的结构）克隆后剔除 UI 节点。
  let src = last.querySelector('.bubble');
  if (!src) {
    const clone = last.cloneNode(true);
    clone.querySelectorAll('.role-tag, .msg-time, .rewind-btn').forEach((n) => n.remove());
    src = clone;
  }
  text.textContent = src.textContent || '';
  // 换钉一律复位折叠态 + 正文滚动 + 渐隐
  pinnedPrompt.expanded = false;
  pp.classList.remove('expanded');
  text.scrollTop = 0;
  const fade = pp.querySelector('.pp-fade');
  if (fade) fade.classList.add('hidden');
  renderIcons(pp);
  updatePpExpandBtn();
  const root = msgs;
  const io = new IntersectionObserver((entries) => {
    for (const ent of entries) {
      if (ent.isIntersecting) { hidePinnedPrompt(false); return; }
      // gate：切轨迹 tab 或 #chat-view 被隐藏时一律不显示
      const mw = $('#msg-wrap');
      const cv = $('#chat-view');
      if (mw && mw.offsetParent === null) { hidePinnedPrompt(false); return; }
      if (cv && cv.offsetParent === null) { hidePinnedPrompt(false); return; }
      if (ent.boundingClientRect.top < root.getBoundingClientRect().top) {
        showPinnedPrompt();
      } else {
        hidePinnedPrompt(false);
      }
    }
  }, { root, threshold: 0 });
  io.observe(last);
  pinnedPrompt.observer = io;
  pinnedPrompt.target = last;
}

function hidePinnedPrompt(disconnect) {
  const pp = $('#pinned-prompt');
  if (!pp) return;
  if (disconnect !== false && pinnedPrompt.observer) {
    pinnedPrompt.observer.disconnect();
    pinnedPrompt.observer = null;
  }
  pinnedPrompt.target = null;
  if (pinnedPrompt.expanded) {
    pinnedPrompt.expanded = false;
    pp.classList.remove('expanded');
    const text = $('#pp-text');
    if (text) text.scrollTop = 0;
    const fade = pp.querySelector('.pp-fade');
    if (fade) fade.classList.add('hidden');
    renderIcons(pp);
    updatePpExpandBtn();
  }
  pp.classList.remove('shown');
  clearTimeout(pinnedPrompt.fadeTimer);
  pinnedPrompt.fadeTimer = setTimeout(() => pp.classList.add('hidden'), 180);
}

function showPinnedPrompt() {
  const pp = $('#pinned-prompt');
  if (!pp) return;
  // 取消 hide 的延时 hidden：滚回（hide 180ms 后加 hidden）又立即滚出（show）
  // 时，若不清定时器，hidden 会在 show 之后补上，悬浮条再也看不见。
  clearTimeout(pinnedPrompt.fadeTimer);
  pp.classList.remove('hidden');
  void pp.offsetHeight;
  pp.classList.add('shown');
  // 刷新时若悬浮条处于 display:none 无法测量溢出，展开按钮判定会漏；现身时校正。
  updatePpExpandBtn();
}

// 折叠态下正文是否超过 2 行截断（决定展开按钮显隐）：
// line-clamp 只裁剪显示，scrollHeight 仍是全文高 → scrollHeight > clientHeight 即溢出。
// 只在可布局时有效（.hidden 的 display:none 下两者都算 0），showPinnedPrompt 会再校正。
function updatePpExpandBtn() {
  const btn = $('#pp-expand');
  const text = $('#pp-text');
  if (!btn || !text) return;
  const overflow = text.scrollHeight - text.clientHeight > 1;
  btn.classList.toggle('hidden', !overflow);
}

$('#pp-expand').addEventListener('click', (e) => {
  e.stopPropagation();
  const pp = $('#pinned-prompt');
  const text = $('#pp-text');
  const fade = pp.querySelector('.pp-fade');
  const expand = pp.querySelector('.pp-expand i');
  pinnedPrompt.expanded = !pinnedPrompt.expanded;
  pp.classList.toggle('expanded', pinnedPrompt.expanded);
  if (expand) {
    expand.dataset.lucide = pinnedPrompt.expanded ? 'chevron-up' : 'chevron-down';
    renderIcons(pp);
  }
  if (pinnedPrompt.expanded) {
    // 展开后立即按当前滚动位置校正遮罩（短文本不可滚时遮罩应隐藏，等 scroll 事件会漏）
    updatePpFade();
  } else {
    if (fade) fade.classList.add('hidden');
    if (text) text.scrollTop = 0;
    // 折叠后重新按溢出状态校正按钮显隐（收起只可能在内容溢出时发生，一般无变化）
    updatePpExpandBtn();
  }
});

// 渐隐遮罩显隐的唯一判定：正文可滚且未滚到底时显示。
// 展开、滚动、换钉都会经此校正，避免"内容不可滚但遮罩残留"。
function updatePpFade() {
  const text = $('#pp-text');
  const pp = $('#pinned-prompt');
  if (!text || !pp) return;
  const fade = pp.querySelector('.pp-fade');
  if (!fade) return;
  const canScroll = text.scrollHeight > text.clientHeight;
  const atBottom = text.scrollTop + text.clientHeight >= text.scrollHeight - 4;
  fade.classList.toggle('hidden', !canScroll || atBottom);
}

$('#pp-text').addEventListener('scroll', () => {
  updatePpFade();
});

$('#pp-text').addEventListener('click', (e) => {
  e.stopPropagation();
  if (pinnedPrompt.expanded) return;
  jumpToPinned();
});

function jumpToPinned() {
  if (pinnedPrompt.target && pinnedPrompt.target.isConnected) {
    pinnedPrompt.target.scrollIntoView({ behavior: 'smooth', block: 'start' });
  }
}

// ---------- event rendering ----------
function scrollBottom() {
  const box = $('#messages');
  if (state.nearBottom) box.scrollTop = box.scrollHeight;
}
$('#messages').addEventListener('scroll', () => {
  const box = $('#messages');
  state.nearBottom = box.scrollHeight - box.scrollTop - box.clientHeight < 120;
});

function addMsg(cls, roleTag, box, ts) {
  box = box || $('#messages');
  const div = document.createElement('div');
  div.className = `msg ${cls}`;
  const timeStr = ts ? formatMsgTime(ts) : '';
  const timeHTML = timeStr ? `<span class="msg-time">${timeStr}</span>` : '';
  div.innerHTML = `<div class="role-tag">${roleTag}${timeHTML}</div><div class="bubble"></div>`;
  box.appendChild(div);
  return div.querySelector('.bubble');
}

// ---------- rewind (file checkpoint + conversation truncation) ----------
// Each user message whose event carries data.checkpoint gets a ⏪ button.
// Clicking confirms, then POST /rewind: the CLI restores files, the server
// truncates both claude's jsonl and our event log, and broadcasts a `rewind`
// event. Live clients truncate their UI; the initiator also refills the
// composer with the rolled-back message text.

function addRewindBtn(msgDiv) {
  if (msgDiv.querySelector('.rewind-btn')) return;
  const btn = document.createElement('button');
  btn.className = 'rewind-btn';
  btn.type = 'button';
  btn.title = '回滚到这条消息之前';
  btn.innerHTML = '<i data-lucide="rotate-ccw"></i><span>回滚</span>';
  btn.onclick = (e) => {
    e.stopPropagation();
    if (document.body.classList.contains('session-running')) return;
    openRewindModal(msgDiv);
  };
  msgDiv.appendChild(btn);
  renderIcons(btn);
}

// Live checkpoint event → attach a button to the most recently rendered user
// message (the turn that just ended). Safe because the next user message can't
// be enqueued until this turn's cleanup (incl. this broadcast) finishes.
function attachCheckpointToLastUser(checkpoint) {
  if (!checkpoint) return;
  const msgs = $('#messages').querySelectorAll('.msg-user');
  if (!msgs.length) return;
  const last = msgs[msgs.length - 1];
  if (last.dataset.checkpoint) return; // already rewound-able
  last.dataset.checkpoint = checkpoint;
  addRewindBtn(last);
}

function appendRewindMarker(box, checkpoint, detail) {
  const marker = document.createElement('div');
  marker.className = 'rewind-marker';
  if (checkpoint) marker.dataset.checkpoint = checkpoint;
  marker.innerHTML = '<span class="rm-icon">⏪</span><span class="rm-text"></span>';
  marker.querySelector('.rm-text').textContent = '已回滚到此处' + (detail ? ' · ' + detail : '');
  box.appendChild(marker);
}

// Apply a rewind to the live UI: drop the target user message and everything
// after it (DOM + historyBuf), re-project the trajectory, render a marker, and
// optionally refill the composer (only the initiating client passes refill).
// Idempotent on the checkpoint uuid.
function applyRewindInUI(ev, refill) {
  const d = (ev && ev.data) || {};
  const checkpoint = d.checkpoint;
  if (!checkpoint) return;
  // 回滚会作废 Session Goal（服务端 handleRewind 里 InvalidateGoal）：被回滚掉的
  // 正是目标在追的那部分工作。这里同步清掉本地那份，否则目标条会继续显示一条
  // 服务端已经删掉的目标。放在这里而不是提交按钮里，是因为 SSE 转发给别的客户端
  // 也走这个函数——那一端同样需要清。回读服务端而不是就地改，尽量别让两端分叉。
  if (state.current) {
    api(`/api/sessions/${state.current.id}`)
      .then(applySession)
      .catch(() => {});
  }
  const box = $('#messages');
  if (box.querySelector(`.rewind-marker[data-checkpoint="${checkpoint}"]`)) {
    if (refill && d.draft_text) setDraftText(d.draft_text);
    return;
  }
  // DOM: remove the target user message and all following siblings.
  const target = box.querySelector(`.msg-user[data-checkpoint="${checkpoint}"]`);
  if (target) {
    let node = target;
    while (node) {
      const next = node.nextElementSibling;
      node.remove();
      node = next;
    }
  }
  // Model: truncate historyBuf at the target user event, then make the rewind
  // event the new tail so reconnect replay would match.
  const buf = state.historyBuf;
  let cutAt = buf.length;
  for (let i = 0; i < buf.length; i++) {
    const e = buf[i];
    if (e.kind === 'user' && e.data && e.data.checkpoint === checkpoint) { cutAt = i; break; }
  }
  buf.splice(cutAt);
  buf.push(ev);
  trajReset();
  trajSync();
  if (trajActive()) trajRender();
  // 任务面板状态折叠自 historyBuf，截断后全量重折（被回滚的 Task* 调用随之消失）
  taskFoldAll();
  syncTaskPanel();
  appendRewindMarker(box, checkpoint, d.detail);
  box.querySelectorAll('.msg-optimistic').forEach((n) => n.remove());
  state.nearBottom = true;
  scrollBottom();
  updateLoadMoreBtn();
  // Recompute stats from the truncated buffer.
  state.stats = freshStats();
  for (const e of buf) trackStats(e);
  updateStatsUI();
  if (refill && d.draft_text) setDraftText(d.draft_text);
  refreshPinnedPrompt();
}

function setDraftText(text) {
  if (!state.current) return;
  input.value = text;
  draft().text = text;
  input.style.height = 'auto';
  input.style.height = Math.min(input.scrollHeight, 180) + 'px';
  renderInputMirror(); // 高度定案后再镜像（见 renderInputMirror）
  input.focus();
}

let rewindTarget = null; // {checkpoint, text} for the in-flight confirm modal
function openRewindModal(msgDiv) {
  const bubble = msgDiv.querySelector('.bubble');
  rewindTarget = {
    checkpoint: msgDiv.dataset.checkpoint,
    text: (bubble && bubble.textContent) || '',
  };
  const firstLine = (rewindTarget.text.split('\n').map((s) => s.trim()).find(Boolean) || '').slice(0, 120);
  $('#rewind-preview').textContent = firstLine || '(空)';
  openModal('#modal-rewind');
}

$('#rewind-submit').onclick = async () => {
  if (!rewindTarget || !state.current) return;
  const btn = $('#rewind-submit');
  const orig = btn.textContent;
  btn.disabled = true;
  btn.textContent = '回滚中…';
  try {
    const res = await api(`/api/sessions/${state.current.id}/rewind`, {
      method: 'POST',
      body: JSON.stringify({ checkpoint: rewindTarget.checkpoint }),
    });
    $('#modal-rewind').classList.add('hidden');
    // Apply locally immediately (the SSE rewind broadcast keeps other clients
    // in sync and is a no-op here thanks to idempotency).
    applyRewindInUI({ kind: 'rewind', data: {
      checkpoint: rewindTarget.checkpoint,
      detail: res.files,
      draft_text: res.draft_text,
    } }, true);
  } catch (e) {
    let msg = e.message || '回滚失败';
    if (/No file checkpoint found|not enabled|File rewinding/i.test(msg)) {
      msg = '该会话未开启文件 checkpoint（或 CLI 版本过低），无法回滚';
    }
    alert(msg);
  } finally {
    btn.disabled = false;
    btn.textContent = orig;
    rewindTarget = null;
  }
};

function toolSummary(input) {
  if (!input || typeof input !== 'object') return '';
  // MCP render_html：摘要行别把整页 HTML 摊开成一坨 JSON
  if (typeof input.html === 'string') return `一页 HTML 文档 · ${input.html.length} 字符`;
  if (Array.isArray(input.todos)) {
    const done = input.todos.filter((t) => t && t.status === 'completed').length;
    return `${done}/${input.todos.length} 完成`;
  }
  if (input.command) return String(input.command).slice(0, 120);
  if (input.file_path) return input.file_path;
  if (input.pattern) return input.pattern;
  if (input.url) return input.url;
  if (input.prompt) return String(input.prompt).slice(0, 120);
  // SendMessage（常驻会话间通信）：收件人 + 正文预览。summary 是模型给的
  // 5-10 词预览，优先于自己截断正文；轨迹视图也复用这条摘要。
  if (typeof input.to === 'string' && typeof input.message === 'string') {
    const preview = (typeof input.summary === 'string' && input.summary.trim())
      || input.message.trim().replace(/\s+/g, ' ').slice(0, 80);
    return `⇢ ${input.to} · ${preview}`;
  }
  // TaskCreate/TaskUpdate：任务详情看输入框上方的任务面板，摘要一行带过
  if (input.taskId != null && input.taskId !== '') return `#${input.taskId}` + (input.status ? ` → ${input.status}` : '');
  if (typeof input.subject === 'string' && input.subject) return input.subject;
  const keys = Object.keys(input);
  return keys.length ? JSON.stringify(input).slice(0, 120) : '';
}

function renderToolUse(block, box, ts, raw) {
  // 记录 Agent 工具调用的元数据：实时子代理事件只带 parent_tool_use_id，
  // 卡片标题从这里回查（回放侧由服务端在事件里直接给 d.subagent）。
  const bn = (block.name || '').toLowerCase();
  if ((bn === 'agent' || bn === 'task') && block.id && block.input) {
    state.agentToolMeta[block.id] = { title: block.input.description || '', type: block.input.subagent_type || '' };
  }
  const details = document.createElement('details');
  details.className = 'tool-block tool-running';
  const summary = document.createElement('summary');
  summary.innerHTML = `<span class="tool-status" title="执行中"></span><span class="tool-name"></span><span class="tool-summary-text"></span><span class="tool-elapsed"></span>`;
  summary.querySelector('.tool-status').textContent = '●';
  summary.querySelector('.tool-name').textContent = block.name || 'tool';
  summary.querySelector('.tool-summary-text').textContent = toolSummary(block.input);
  const body = document.createElement('div');
  body.className = 'tool-body';
  if (!buildToolView(block, details, body)) {
    body.textContent = JSON.stringify(block.input, null, 2);
  }
  details.append(summary, body);
  // raw：直接挂进调用方给的容器（子代理卡片体），不包消息气泡壳
  if (raw) box.appendChild(details);
  else wrapInMsg(details, box, ts);
  if (block.id) {
    state.toolBlocks[block.id] = details;
    // 工具计时：tool_use 落盘即起表，每秒刷一次已用时长；tool_result 回来定格。
    const start = Date.now();
    const elapsedEl = summary.querySelector('.tool-elapsed');
    const tick = () => {
      const s = Math.max(0, Math.floor((Date.now() - start) / 1000));
      elapsedEl.textContent = s >= 60 ? `${Math.floor(s / 60)}m${s % 60}s` : `${s}s`;
    };
    tick();
    const timer = setInterval(tick, 1000);
    state.toolTimers[block.id] = { timer, start };
  }
}

// 工具 body 定制视图：bash 终端风 / read 文件头 / write 全绿 diff / edit 红绿 diff。
// bash、read 的关键信息已在 summary 行，保持折叠；write、edit 默认展开。
// 返回 true 表示 body 已被定制填充。
function buildToolView(block, details, body) {
  const input = block.input;
  const name = (block.name || '').toLowerCase();
  // MCP 的 render_html：工具名是 mcp__<server>__<tool>（stream-json 里就这个形状）。
  // 工具真被调用时 html 已经完整落定，不像正文会流式重绘，所以直接展开。
  if (/^mcp__[\w.-]+__render_html$/.test(name) && input && typeof input.html === 'string') {
    details.classList.add('tool-htmlframe');
    const pre = document.createElement('pre');
    const code = document.createElement('code');
    code.className = 'language-html';
    code.textContent = input.html; // textContent：绝不用 innerHTML，模型产物不进我们的 DOM
    pre.appendChild(code);
    const node = buildHtmlArtifactNode(pre);
    body.appendChild(node);
    if (input.html.trim()) {
      mountHtmlFrame(node);
      details.open = true;
    }
    return true;
  }
  if (name === 'todowrite' && input && Array.isArray(input.todos)) {
    details.classList.add('tool-todo');
    body.classList.add('todo-body');
    body.appendChild(buildTodoView(input.todos));
    details.open = true;
    return true;
  }
  if (/^task(create|update|list|get)$/.test(name)) {
    details.classList.add('tool-task');
    body.classList.add('task-body');
    body.appendChild(buildTaskToolView(input || {}));
    return true;
  }
  if (name === 'sendmessage' && input && typeof input.message === 'string') {
    return buildPeerSendView(input, details, body);
  }
  if (name === 'bash' && input && typeof input.command === 'string') {
    details.classList.add('tool-bash');
    body.appendChild(buildBashView(input));
    return true;
  }
  if (name === 'read' && input && typeof input.file_path === 'string') {
    body.appendChild(buildReadView(input));
    return true;
  }
  if (name === 'write' && input && typeof input.content === 'string') {
    body.classList.add('edit-diff');
    body.appendChild(buildWriteView(input));
    details.open = true;
    return true;
  }
  const diff = buildEditDiff(input);
  if (diff) {
    // edit 类工具：old_string/new_string 渲染成红绿对比 diff，默认展开
    body.classList.add('edit-diff');
    body.appendChild(diff);
    details.open = true;
    return true;
  }
  return false;
}

// SendMessage（常驻会话间通信）：把一次调用渲染成"发件"卡片，与入站 msg-peer
// 气泡同一紫色系，进出两向在对话流里成对可读。折叠行只留收件人 + 预览
// （toolSummary 那条），展开是收件人 + 正文 markdown；投递结果由
// renderToolResult 追加在正文下方，失败时在折叠行补红字标记。
function buildPeerSendView(input, details, body) {
  details.classList.add('tool-peer-send');
  body.classList.add('peer-send-body');
  const to = String(input.to || '');
  if (to) {
    const head = document.createElement('div');
    head.className = 'peer-send-head';
    const arrow = document.createElement('span');
    arrow.className = 'peer-send-arrow';
    arrow.textContent = '⇢';
    const chip = document.createElement('span');
    chip.className = 'peer-send-to';
    chip.textContent = to;
    // 收件人是本控制台托管的会话时，点地址名直接跳过去看对方怎么处理
    const peer = findPeerSession(to);
    if (peer) {
      chip.classList.add('peer-send-to-link');
      chip.title = `打开会话「${peer.name}」`;
      chip.onclick = () => openSession(peer.id);
    } else {
      chip.title = `地址名 ${to}`;
    }
    head.append(arrow, chip);
    body.appendChild(head);
  }
  const msg = document.createElement('div');
  msg.className = 'peer-send-msg';
  msg.innerHTML = md(input.message || '');
  body.appendChild(msg);
  details.open = true;
  return true;
}

// 按地址名找本控制台托管的会话：`name [ref]` 的 ref 只在同名多活时消歧，
// 匹配前剥掉；peer_name 未显式设置时地址名回落为会话名（server 侧
// CleanPeerName(sess.Name) 兜底）。大小写不敏感——这里只做导航，不参与寻址。
function findPeerSession(to) {
  const key = String(to || '').split(/\s*\[/)[0].trim().toLowerCase();
  if (!key) return null;
  for (const s of state.sessions || []) {
    const name = (s.peer_name || s.name || '').trim().toLowerCase();
    if (name && name === key) return s;
  }
  return null;
}

// TodoWrite：checklist 视图 —— 状态着色（completed 绿勾 / in_progress 蓝点 / pending 空心）
function buildTodoView(todos) {
  const list = document.createElement('ul');
  list.className = 'todo-list';
  for (const t of todos) {
    if (!t || typeof t.content !== 'string') continue;
    const item = document.createElement('li');
    item.className = 'todo-item todo-' + (t.status || 'pending');
    const check = document.createElement('span');
    check.className = 'todo-check';
    const text = document.createElement('span');
    text.className = 'todo-text';
    text.textContent = t.content;
    item.append(check, text);
    list.appendChild(item);
  }
  return list;
}

// TaskCreate/TaskUpdate/TaskGet：字段键值对视图。任务的整体状态看输入框上方
// 的任务面板，这里只展示本次调用带了哪些参数。
function buildTaskToolView(input) {
  const kv = document.createElement('div');
  kv.className = 'task-kv';
  const rows = [
    ['任务', input.taskId != null && input.taskId !== '' ? `#${input.taskId}` : ''],
    ['主题', input.subject],
    ['状态', input.status],
    ['进行中显示', input.activeForm],
    ['描述', input.description],
  ];
  for (const [k, v] of rows) {
    if (!v) continue;
    const row = document.createElement('div');
    row.className = 'task-kv-row';
    const key = document.createElement('span');
    key.className = 'task-kv-key';
    key.textContent = k;
    const val = document.createElement('span');
    val.className = 'task-kv-val';
    val.textContent = String(v);
    row.append(key, val);
    kv.appendChild(row);
  }
  if (!kv.children.length) kv.textContent = '(无参数)';
  return kv;
}

// Bash：终端样式 —— 描述注释 + `$ 命令`，输出由 tool_result 追加（同色终端底）
function buildBashView(input) {
  const term = document.createElement('div');
  term.className = 'term-view';
  if (input.description) {
    const desc = document.createElement('div');
    desc.className = 'term-desc';
    desc.textContent = '# ' + input.description;
    term.appendChild(desc);
  }
  const cmd = document.createElement('div');
  cmd.className = 'term-cmd';
  const prompt = document.createElement('span');
  prompt.className = 'term-prompt';
  prompt.textContent = '$ ';
  cmd.appendChild(prompt);
  cmd.appendChild(document.createTextNode(input.command));
  term.appendChild(cmd);
  return term;
}

// 文件路径头部：路径文本 + 右侧复制按钮
function filePathHead(cls, text, path) {
  const head = document.createElement('div');
  head.className = cls;
  const span = document.createElement('span');
  span.className = 'path-text';
  span.textContent = text;
  span.title = path;
  head.appendChild(span);
  const btn = document.createElement('button');
  btn.className = 'copy-path-btn';
  btn.title = '复制文件路径';
  btn.innerHTML = '<i data-lucide="copy"></i>';
  btn.onclick = async (e) => {
    e.stopPropagation();
    try {
      await copyText(path);
      flashCheck(btn, btn.innerHTML);
    } catch (_) {
      prompt('复制失败，请手动复制：', path);
    }
  };
  head.appendChild(btn);
  renderIcons(head);
  return head;
}

// Read：文件路径 + 行范围；文件内容在 tool_result 里（等宽展示即可）
function buildReadView(input) {
  const view = document.createElement('div');
  view.className = 'read-view';
  view.appendChild(filePathHead('read-path', input.file_path, input.file_path));
  const meta = [];
  if (typeof input.offset === 'number') meta.push(`从第 ${input.offset} 行起`);
  if (typeof input.limit === 'number') meta.push(`共 ${input.limit} 行`);
  if (meta.length) {
    const m = document.createElement('div');
    m.className = 'read-meta';
    m.textContent = meta.join(' · ');
    view.appendChild(m);
  }
  return view;
}

// Write：新文件 = 全部新增，复用 diff 行的全绿样式；超长内容截断，避免大文件
// 把 DOM 撑爆（结果里另有完整回执）。
const WRITE_MAX_LINES = 200;
function buildWriteView(input) {
  const view = document.createElement('div');
  view.className = 'diff-view';
  const lines = input.content.split('\n');
  // 末尾换行导致的空行不算一行内容
  const count = lines.length && lines[lines.length - 1] === '' ? lines.length - 1 : lines.length;
  view.appendChild(filePathHead('diff-file', `${input.file_path || '(新文件)'}（+${count} 行）`, input.file_path || ''));
  const shown = lines.slice(0, WRITE_MAX_LINES);
  for (const l of shown) view.appendChild(diffLineEl('add', '+', l));
  if (lines.length > WRITE_MAX_LINES) {
    view.appendChild(diffLineEl('fold', '', `⋮ 还有 ${lines.length - WRITE_MAX_LINES} 行未展示`));
  }
  return view;
}

// ---------- edit diff view ----------
// 提取 edit 入参里的 (old_string, new_string) 对；兼容单 edit 与 edits 数组。
function editPairs(input) {
  if (!input || typeof input !== 'object') return [];
  if (typeof input.old_string === 'string' && typeof input.new_string === 'string') {
    return [[input.old_string, input.new_string]];
  }
  if (Array.isArray(input.edits)) {
    return input.edits
      .filter((e) => e && typeof e.old_string === 'string' && typeof e.new_string === 'string')
      .map((e) => [e.old_string, e.new_string]);
  }
  return [];
}

function buildEditDiff(input) {
  const pairs = editPairs(input);
  if (!pairs.length) return null;
  const view = document.createElement('div');
  view.className = 'diff-view';
  let totalDel = 0, totalAdd = 0;
  for (const [oldStr, newStr] of pairs) {
    totalDel += oldStr.split('\n').length;
    totalAdd += newStr.split('\n').length;
  }
  const stat = `(−${totalDel} +${totalAdd}${pairs.length > 1 ? ` · ${pairs.length} 处` : ''})`;
  if (input.file_path) {
    view.appendChild(filePathHead('diff-file', `${input.file_path} ${stat}`, input.file_path));
  } else {
    const s = document.createElement('div');
    s.className = 'diff-file diff-stat';
    s.textContent = stat;
    view.appendChild(s);
  }
  for (const [oldStr, newStr] of pairs) {
    appendDiffHunk(view, oldStr, newStr);
  }
  return view;
}

// 简单行级对比：剥掉公共前后缀作为上下文，中间差异段红/绿整段展示。
function diffLineEl(cls, sign, text) {
  const line = document.createElement('div');
  line.className = 'diff-line ' + cls;
  const g = document.createElement('span');
  g.className = 'diff-sign';
  g.textContent = sign;
  line.appendChild(g);
  line.appendChild(document.createTextNode(text));
  return line;
}

function appendDiffHunk(view, oldStr, newStr) {
  const a = oldStr.split('\n');
  const b = newStr.split('\n');
  let pre = 0;
  while (pre < a.length && pre < b.length && a[pre] === b[pre]) pre++;
  let suf = 0;
  while (suf < a.length - pre && suf < b.length - pre && a[a.length - 1 - suf] === b[b.length - 1 - suf]) suf++;
  const ctxPre = a.slice(0, pre);
  const del = a.slice(pre, a.length - suf);
  const add = b.slice(pre, b.length - suf);
  const ctxSuf = a.slice(a.length - suf);

  const addLine = (cls, sign, text) => view.appendChild(diffLineEl(cls, sign, text));
  const fold = (n) => addLine('fold', '', `⋮ ${n} 行未变化`);

  // 上下文只保留贴着差异段的 CTX 行，远端折叠（之前版本索引越界会吞行）。
  const CTX = 3;
  if (ctxPre.length > CTX) {
    fold(ctxPre.length - CTX);
    for (const l of ctxPre.slice(ctxPre.length - CTX)) addLine('ctx', ' ', l);
  } else {
    for (const l of ctxPre) addLine('ctx', ' ', l);
  }
  for (const l of del) addLine('del', '-', l);
  for (const l of add) addLine('add', '+', l);
  if (ctxSuf.length > CTX) {
    for (const l of ctxSuf.slice(0, CTX)) addLine('ctx', ' ', l);
    fold(ctxSuf.length - CTX);
  } else {
    for (const l of ctxSuf) addLine('ctx', ' ', l);
  }
}

function toolResultText(content) {
  if (typeof content === 'string') return content;
  if (Array.isArray(content)) {
    return content.map((c) => (c && c.text) || '').filter(Boolean).join('\n');
  }
  return content ? JSON.stringify(content) : '';
}

function renderToolResult(block, box, raw) {
  const text = toolResultText(block.content);
  // 工具计时定格：无论结果是否展示（todo/task 成功结果会提前 return），状态都
  // 要从"执行中"翻成完成/失败，否则转圈停不下来。
  const finalizeTool = (target, isError) => {
    const t = block.tool_use_id && state.toolTimers[block.tool_use_id];
    if (t) {
      clearInterval(t.timer);
      delete state.toolTimers[block.tool_use_id];
    }
    if (!target) return;
    target.classList.remove('tool-running');
    target.classList.add(isError ? 'tool-error' : 'tool-done');
    const st = target.querySelector('.tool-status');
    if (st) {
      st.textContent = isError ? '✗' : '✓';
      st.title = isError ? '失败' : '完成';
    }
    const el = target.querySelector('.tool-elapsed');
    if (el && t) {
      const s = Math.max(0, Math.floor((Date.now() - t.start) / 1000));
      el.textContent = s >= 60 ? `${Math.floor(s / 60)}m${s % 60}s` : `${s}s`;
      el.title = '工具耗时';
    }
  };
  const target = block.tool_use_id && state.toolBlocks[block.tool_use_id];
  if (target) {
    // TodoWrite 的结果固定是 "Todos have been modified…"，无信息量，不展示
    if (target.classList.contains('tool-todo')) { finalizeTool(target, !!block.is_error); return; }
    // Task* 的成功结果同理（"Task #N created successfully"），面板已反映状态；
    // 但错误结果（如输入校验失败）必须展示
    if (target.classList.contains('tool-task') && !block.is_error) { finalizeTool(target, false); return; }
    const r = document.createElement('div');
    r.className = 'tool-result';
    const body = document.createElement('div');
    body.className = 'tool-body';
    // bash 的输出沿用终端配色，和命令行连成一整个终端窗口
    if (target.classList.contains('tool-bash')) body.classList.add('term-out');
    // SendMessage 的返回是投递回执（一行状态），不用等宽代码块样式
    const isPeerSend = target.classList.contains('tool-peer-send');
    if (isPeerSend) body.classList.add('peer-send-status');
    body.textContent = text.length > 4000 ? text.slice(0, 4000) + '\n… (截断)' : text;
    if (block.is_error) body.style.color = 'var(--red)';
    r.appendChild(body);
    target.appendChild(r);
    finalizeTool(target, !!block.is_error);
    if (isPeerSend && block.is_error) markPeerSendFailed(target);
    return;
  }
  // Orphan result (e.g. from imported history without the tool_use block).
  const details = document.createElement('details');
  details.className = 'tool-block tool-result';
  const summary = document.createElement('summary');
  summary.innerHTML = '<span class="tool-name">tool_result</span>';
  const body = document.createElement('div');
  body.className = 'tool-body';
  body.textContent = text.length > 4000 ? text.slice(0, 4000) + '\n… (截断)' : text;
  details.append(summary, body);
  if (raw) box.appendChild(details); // 子代理卡片体：不包消息气泡壳
  else wrapInMsg(details, box);
}

// 投递失败（对端离线 / 名字不匹配 / 被拒收）：折叠行也要能看出，否则一串紫色
// 卡片看起来都像已送达。正文里的回执仍照常展示完整错误文案。
function markPeerSendFailed(details) {
  details.classList.add('peer-send-failed');
  const st = details.querySelector('.tool-summary-text');
  if (!st || st.querySelector('.peer-send-fail')) return;
  const m = document.createElement('span');
  m.className = 'peer-send-fail';
  m.textContent = ' · 投递失败';
  st.appendChild(m);
}

// ---------- 权限审批卡（claude/qoder hook；opencode serve；mcode 无） ----------
// 三条引擎路径，同一套卡片：
//   claude/qoder — 后端注入 PermissionRequest hook，工具调用前 CLI 起 helper
//                  问服务端；opencode — 引擎走 `opencode serve`，把 SSE 的
//                  permission.asked / question.asked 直接映射成本事件；
//   mcode — 无 headless 审批面，不走这里。
// 三档：允许本次 / 本次会话都允许（同名工具后续免问）/ 拒绝（可附理由回传模型）。
// 55s 无人审批自动拒绝。
// AskUserQuestion 不是"放不放行"，是模型在问你问题：答案要以问题原文为 key、
// 选项 label 为 value 经 updatedInput.answers 回传，否则 CLI 报
// "The user did not answer the questions"。畸形 input 才退回通用卡。
function parsePermQuestions(input) {
  if (!input || !Array.isArray(input.questions)) return [];
  return input.questions.filter((q) => q && typeof q.question === 'string');
}
const PERM_TIMEOUT_S = 55;
const PERM_OTHER = '__other__';
// 待审批卡停靠在输入框上方（composer 卡片顶部），位置固定不随消息流跑；
// 审批完（锁卡）才落进消息流随历史走。dock 在 composer 里，显隐撑高输入框，
// 高度变化由已有的 ResizeObserver 自动换算成消息区 padding。
// 群视图有自己的 composer+dock：永远用当前可见的那个。
function permDock() {
  const gv = $('#group-view');
  if (gv && !gv.classList.contains('hidden')) return $('#perm-dock-group');
  return $('#perm-dock');
}
function activeMsgBox() {
  const gv = $('#group-view');
  if (gv && !gv.classList.contains('hidden')) return $('#group-messages');
  return $('#messages');
}
function syncPermDock() {
  const dock = permDock();
  if (!dock) return;
  dock.classList.toggle('hidden', dock.children.length === 0);
}
// 卡片落进消息流：已锁定的卡从 dock 搬到消息区末尾（appendChild 即搬迁）。
// 成员审批结果归群时间线（toGroup），单聊卡走当前视图。
function landPermCard(entry, toGroup) {
  const box = toGroup ? $('#group-messages') : activeMsgBox();
  if (entry && entry.wrap && entry.wrap.parentElement !== box) box.appendChild(entry.wrap);
  document.querySelectorAll('#perm-dock, #perm-dock-group').forEach((dock) => {
    if (dock) dock.classList.toggle('hidden', dock.children.length === 0);
  });
}
// 切视图时把未锁卡搬到当前 dock，避免悬在隐藏的 composer 里
function relocatePermCards() {
  const dock = permDock();
  if (!dock) return;
  for (const id of Object.keys(state.permCards || {})) {
    const e = state.permCards[id];
    if (e && !e.locked && e.wrap && e.wrap.parentElement !== dock) dock.appendChild(e.wrap);
  }
  document.querySelectorAll('#perm-dock, #perm-dock-group').forEach((dd) => {
    if (dd) dd.classList.toggle('hidden', dd.children.length === 0);
  });
}
// 群转发的审批卡自带 member_id（决议 POST 到成员影子会话）；单聊卡走当前会话
function permResolveSid(d) {
  if (d && d.member_id) return d.member_id;
  return (state.current && state.current.id) || '';
}
function permInputSummary(tool, input) {
  if (tool === 'AskUserQuestion' && input && Array.isArray(input.questions)) {
    return `提问 ${input.questions.length} 个`;
  }
  return toolSummary(input);
}
// AskUserQuestion 选题卡：每题单选/多选 + "其他"自定义输入，提交把
// {问题原文: 选项label} 回传（多选逗号连接），跳过走 deny。
function buildAskUserCard(card, d, questions, box, wrap) {
  const picked = {}; // qi -> [label]
  const custom = {}; // qi -> text
  let html =
    '<div class="perm-head"><span class="perm-title">模型提问</span>' +
    '<span class="perm-countdown"></span></div>';
  questions.forEach((q, qi) => {
    const opts = Array.isArray(q.options) ? q.options : [];
    html += '<div class="perm-q" data-qi="' + qi + '">';
    if (q.header) html += '<div class="perm-q-header"></div>';
    html += '<div class="perm-q-text"></div><div class="perm-opts">';
    opts.forEach((o) => {
      const label = (o && o.label) || '';
      html += '<button class="perm-opt" type="button" data-label="' + escapeHtml(label).replace(/"/g, '&quot;') + '">' +
        '<span class="perm-opt-label"></span>' +
        (o && o.description ? '<span class="perm-opt-desc"></span>' : '') + '</button>';
    });
    html += '<button class="perm-opt perm-opt-other" type="button" data-label="' + PERM_OTHER + '">' +
      '<span class="perm-opt-label">其他（自定义）</span></button>' +
      '<input class="perm-opt-custom hidden" placeholder="自定义回答" maxlength="500">' +
      '</div></div>';
  });
  html += '<div class="perm-btns">' +
    '<button class="perm-btn perm-submit" type="button" disabled>提交回答</button>' +
    '<button class="perm-btn perm-deny" type="button">跳过</button></div>' +
    '<div class="perm-hint">超时未作答会自动拒绝，模型会重新提问，不会丢问题</div>' +
    '<div class="perm-state hidden"></div>';
  card.innerHTML = html;
  card.querySelectorAll('.perm-q').forEach((qEl) => {
    const qi = Number(qEl.dataset.qi);
    const q = questions[qi];
    if (q.header) qEl.querySelector('.perm-q-header').textContent = q.header;
    qEl.querySelector('.perm-q-text').textContent = q.question;
    const labels = qEl.querySelectorAll('.perm-opt[data-label]');
    labels.forEach((btn, bi) => {
      const opt = (q.options || [])[bi];
      if (opt && opt.label) {
        btn.querySelector('.perm-opt-label').textContent = opt.label;
        const desc = btn.querySelector('.perm-opt-desc');
        if (desc && opt.description) desc.textContent = opt.description;
      }
    });
  });
  const submitBtn = card.querySelector('.perm-submit');
  const customInputs = card.querySelectorAll('.perm-opt-custom');
  const answerFor = (qi) => {
    const cur = picked[qi] || [];
    return cur.map((l) => (l === PERM_OTHER ? (custom[qi] || '').trim() : l)).filter(Boolean).join('，');
  };
  const refresh = () => {
    submitBtn.disabled = !questions.every((_, qi) => answerFor(qi).length > 0);
  };
  card.querySelectorAll('.perm-opt').forEach((btn) => {
    btn.onclick = () => {
      const qEl = btn.closest('.perm-q');
      const qi = Number(qEl.dataset.qi);
      const label = btn.dataset.label;
      const multi = !!questions[qi].multiSelect;
      let cur = picked[qi] || [];
      if (!multi) cur = (cur[0] === label) ? [] : [label];
      else cur = cur.includes(label) ? cur.filter((x) => x !== label) : [...cur, label];
      picked[qi] = cur;
      qEl.querySelectorAll('.perm-opt').forEach((b) => {
        b.classList.toggle('picked', (picked[qi] || []).includes(b.dataset.label));
      });
      const customInput = qEl.querySelector('.perm-opt-custom');
      const showCustom = (picked[qi] || []).includes(PERM_OTHER);
      customInput.classList.toggle('hidden', !showCustom);
      if (showCustom) customInput.focus();
      refresh();
    };
  });
  customInputs.forEach((inp) => {
    inp.oninput = () => {
      const qi = Number(inp.closest('.perm-q').dataset.qi);
      custom[qi] = inp.value;
      refresh();
    };
  });
  const setBusy = (busy) => card.querySelectorAll('.perm-btns button').forEach((b) => { b.disabled = busy; });
  submitBtn.onclick = async () => {
    const answers = {};
    questions.forEach((q, qi) => {
      const a = answerFor(qi);
      if (a) answers[q.question] = a;
    });
    setBusy(true);
    try {
      await api(`/api/sessions/${permResolveSid(d)}/permissions/${d.id}`, {
        method: 'POST', body: JSON.stringify({ behavior: 'allow', answers }),
      });
      lockPermCard(card, 'allow', '已回答');
      state.permCards[d.id].locked = true;
      landPermCard(state.permCards[d.id], !!(d && d.member_id));
    } catch (e) {
      setBusy(false);
      refresh();
      const st = card.querySelector('.perm-state');
      st.classList.remove('hidden');
      st.textContent = '提交失败：' + e.message + '，可重试';
    }
  };
  card.querySelector('.perm-deny').onclick = async () => {
    setBusy(true);
    try {
      await api(`/api/sessions/${permResolveSid(d)}/permissions/${d.id}`, {
        method: 'POST', body: JSON.stringify({ behavior: 'deny', message: '用户跳过了这个提问' }),
      });
      lockPermCard(card, 'deny', '已跳过');
      state.permCards[d.id].locked = true;
      landPermCard(state.permCards[d.id], !!(d && d.member_id));
    } catch (e) {
      setBusy(false);
      const st = card.querySelector('.perm-state');
      st.classList.remove('hidden');
      st.textContent = '提交失败：' + e.message + '，可重试';
    }
  };
  const cd = card.querySelector('.perm-countdown');
  const deadline = Date.now() + PERM_TIMEOUT_S * 1000;
  const timer = setInterval(() => {
    const left = Math.max(0, Math.ceil((deadline - Date.now()) / 1000));
    cd.textContent = left > 0 ? `${left}s` : '已超时';
    if (left <= 0) clearInterval(timer);
  }, 1000);
  cd.textContent = `${PERM_TIMEOUT_S}s`;
  wrap.appendChild(card);
  // 待审批停靠输入框上方（dock），不进消息流；锁卡后才落进历史
  permDock().appendChild(wrap);
  syncPermDock();
  state.permCards[d.id] = { wrap, card, locked: false, timer };
  // 提问卡和工具审批卡的紧迫度不一样：55s 到点作废、模型还得重问一遍。弹一条
  // 常驻浮层 + 侧栏徽标，保证人不在这个会话时也能看见，并能直接跳过来作答。
  raiseAskToast(askOf(d), permToastCtx(d));
}
// 提问摘要：从权限事件本身现取，给 SSE 这条路径当场起浮层用；形状与后端
// sessionView.pending_asks 一致（轮询那条路径直接用后端给的）。
function askOf(d) {
  const qs = parsePermQuestions(d && d.input);
  return {
    id: d && d.id,
    header: qs[0] ? qs[0].header : '',
    question: qs[0] ? qs[0].question : '',
    ts: (d && d.ts) || Date.now(),
  };
}
function permToastCtx(d) {
  if (d && d.member_id) return { gid: d.group_id, sid: d.member_id, where: (d.member_name || '群成员') + '（群聊）' };
  return { sid: (state.current && state.current.id) || '', where: (state.current && state.current.name) || '会话' };
}
function renderPermissionCard(d, box) {
  box = box || $('#messages');
  if (!d || !d.id || state.permCards[d.id]) return;
  const wrap = document.createElement('div');
  wrap.className = 'msg msg-perm';
  wrap.dataset.permId = d.id;
  const card = document.createElement('div');
  card.className = 'perm-card';
  const tool = d.tool || 'tool';
  card.dataset.tool = tool;
  const questions = tool === 'AskUserQuestion' ? parsePermQuestions(d.input) : [];
  if (questions.length > 0) {
    buildAskUserCard(card, d, questions, box, wrap);
    return;
  }
  const summary = permInputSummary(tool, d.input);
  card.innerHTML =
    '<div class="perm-head"><span class="perm-title">权限请求</span>' +
    '<span class="perm-dot">·</span><span class="perm-tool"></span>' +
    '<span class="perm-countdown"></span></div>' +
    (summary ? '<div class="perm-summary"></div>' : '') +
    '<details class="perm-detail"><summary>查看完整参数</summary><pre class="perm-json"></pre></details>' +
    '<div class="perm-reason-row hidden"><input class="perm-reason" placeholder="拒绝理由（会回传给模型，可空）" maxlength="500"></div>' +
    '<div class="perm-btns">' +
    '<button class="perm-btn perm-allow" type="button">允许本次</button>' +
    '<button class="perm-btn perm-allow-session" type="button">本次会话都允许</button>' +
    '<button class="perm-btn perm-deny" type="button">拒绝</button>' +
    '</div>' +
    '<div class="perm-state hidden"></div>';
  card.querySelector('.perm-tool').textContent = tool;
  if (summary) card.querySelector('.perm-summary').textContent = summary;
  card.querySelector('.perm-json').textContent = JSON.stringify(d.input || {}, null, 2);
  if (d.auto_allowed) {
    lockPermCard(card, 'allow', '已按规则允许（同名工具）');
    wrap.appendChild(card);
    box.appendChild(wrap);
    state.permCards[d.id] = { wrap, card, locked: true };
    return;
  }
  const btns = card.querySelector('.perm-btns');
  const reasonRow = card.querySelector('.perm-reason-row');
  const reasonInput = card.querySelector('.perm-reason');
  const setBusy = (busy) => btns.querySelectorAll('button').forEach((b) => { b.disabled = busy; });
  const answer = async (behavior, message) => {
    setBusy(true);
    try {
      await api(`/api/sessions/${permResolveSid(d)}/permissions/${d.id}`, {
        method: 'POST', body: JSON.stringify({ behavior, message }),
      });
      // 结果经 SSE permission_resolved 回来统一锁卡；这里先乐观锁，丢包也不悬空
      lockPermCard(card, behavior === 'deny' ? 'deny' : 'allow',
        behavior === 'deny' ? '已拒绝' : behavior === 'allow_tool_session' ? '已允许同名工具' : '已允许本次');
      state.permCards[d.id].locked = true;
      landPermCard(state.permCards[d.id], !!(d && d.member_id));
    } catch (e) {
      setBusy(false);
      const st = card.querySelector('.perm-state');
      st.classList.remove('hidden');
      st.textContent = '提交失败：' + e.message + '，可重试';
    }
  };
  card.querySelector('.perm-allow').onclick = () => answer('allow');
  card.querySelector('.perm-allow-session').onclick = () => answer('allow_tool_session');
  const denyBtn = card.querySelector('.perm-deny');
  denyBtn.onclick = () => {
    // 第一次点展开理由框，第二次点真正拒绝（理由可选填）
    if (reasonRow.classList.contains('hidden')) {
      reasonRow.classList.remove('hidden');
      reasonInput.focus();
      denyBtn.textContent = '确认拒绝';
      return;
    }
    answer('deny', reasonInput.value.trim());
  };
  // 倒计时：55s 到 helper 自动拒绝，卡片置灰提示
  const cd = card.querySelector('.perm-countdown');
  const deadline = Date.now() + PERM_TIMEOUT_S * 1000;
  const timer = setInterval(() => {
    const left = Math.max(0, Math.ceil((deadline - Date.now()) / 1000));
    cd.textContent = left > 0 ? `${left}s` : '已超时';
    if (left <= 0) clearInterval(timer);
  }, 1000);
  cd.textContent = `${PERM_TIMEOUT_S}s`;
  wrap.appendChild(card);
  // 待审批停靠输入框上方（dock），不进消息流；锁卡后才落进历史
  permDock().appendChild(wrap);
  syncPermDock();
  state.permCards[d.id] = { wrap, card, locked: false, timer };
}
function lockPermCard(card, behavior, label) {
  card.classList.add('perm-locked');
  const btns = card.querySelector('.perm-btns');
  if (btns) btns.remove();
  const reasonRow = card.querySelector('.perm-reason-row');
  if (reasonRow) reasonRow.remove();
  const cd = card.querySelector('.perm-countdown');
  if (cd) cd.remove();
  const st = card.querySelector('.perm-state');
  st.classList.remove('hidden');
  st.textContent = label || (behavior === 'deny' ? '已拒绝' : '已允许');
  st.classList.toggle('perm-denied', behavior === 'deny');
}
function resolvePermissionCard(d) {
  if (!d || !d.id) return;
  // 卡锁了就作废，对应浮层立刻撤（跨会话时 resolve 事件收不到，由轮询 diff 兜）
  dropToast(ASK_TOAST_KEY(d.id));
  const permLabel = (card, behavior, timeout, auto) => {
    const isAsk = card && card.dataset && card.dataset.tool === 'AskUserQuestion';
    if (timeout) return isAsk ? '已超时，模型会重问' : '已超时自动拒绝';
    if (behavior === 'deny') return isAsk ? '已跳过' : '已拒绝';
    if (auto) return '已按规则允许（同名工具）';
    return isAsk ? '已回答' : '已允许';
  };
  const entry = state.permCards[d.id];
  if (entry) {
    if (entry.timer) clearInterval(entry.timer);
    if (!entry.locked) {
      lockPermCard(entry.card, d.behavior, permLabel(entry.card, d.behavior, d.timeout, d.auto_allowed));
      entry.locked = true;
      landPermCard(entry, !!(d && d.member_id));
    }
    return;
  }
  // 重连/刷新时错过 request：补一张已锁定的卡，避免"工具没动静"的 mystery gap
  if (d.behavior && state.current) {
    renderPermissionCard({ id: d.id, tool: 'tool', input: {}, auto_allowed: false }, $('#messages'));
    const e2 = state.permCards[d.id];
    if (e2) {
      if (e2.timer) clearInterval(e2.timer);
      lockPermCard(e2.card, d.behavior, d.timeout ? '已超时自动拒绝' : d.behavior === 'deny' ? '已拒绝' : '已允许');
      e2.locked = true;
      landPermCard(e2, !!(d && d.member_id));
    }
  }
}
async function fetchPendingPermissions(id) {
  try {
    const r = await api(`/api/sessions/${id}/permissions/pending`);
    for (const p of (r && r.pending) || []) renderPermissionCard(p);
  } catch (_) { /* 无待审批时静默 */ }
}

// ---------- file viewer（右侧栏预览本地文件） ----------
function escapeHtml(s) {
  return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

async function openFileViewer(path, line) {
  const panel = $('#file-viewer');
  panel.classList.remove('hidden');
  $('#fv-path').textContent = path;
  $('#fv-path').title = path;
  const body = $('#fv-body');
  body.innerHTML = '<div class="fv-status">加载中…</div>';
  renderIcons(panel);
  try {
    const data = await api(`/api/files?path=${encodeURIComponent(path)}`);
    renderFileContent(body, data, line);
  } catch (e) {
    body.innerHTML = '';
    const err = document.createElement('div');
    err.className = 'fv-status fv-error';
    err.textContent = '无法读取：' + e.message;
    body.appendChild(err);
  }
}

function closeFileViewer() {
  $('#file-viewer').classList.add('hidden');
}

const FV_MAX_LINES = 5000; // 大文件只渲染前 5000 行，避免 DOM 爆炸
function renderFileContent(body, data, line) {
  body.innerHTML = '';
  const lines = data.content.split('\n');
  const shown = lines.slice(0, FV_MAX_LINES);
  const list = document.createElement('div');
  list.className = 'fv-code';
  let target = null;
  shown.forEach((text, i) => {
    const row = document.createElement('div');
    row.className = 'fv-line';
    const no = document.createElement('span');
    no.className = 'fv-ln';
    no.textContent = i + 1;
    const lc = document.createElement('span');
    lc.className = 'fv-lc';
    lc.innerHTML = escapeHtml(text) || ' ';
    row.append(no, lc);
    if (line === i + 1) { row.classList.add('target'); target = row; }
    list.appendChild(row);
  });
  body.appendChild(list);
  if (lines.length > FV_MAX_LINES || data.truncated) {
    const more = document.createElement('div');
    more.className = 'fv-status';
    more.textContent = data.truncated ? '（文件过大，已截断）' : `（仅显示前 ${FV_MAX_LINES} 行）`;
    body.appendChild(more);
  }
  if (target) target.scrollIntoView({ block: 'center' });
}

// 点击消息里的本地文件链接 → 侧栏预览
$('#messages').addEventListener('click', (e) => {
  const a = e.target.closest('a[data-file-path]');
  if (!a) return;
  e.preventDefault();
  openFileViewer(a.dataset.filePath, parseInt(a.dataset.fileLine || '0', 10));
});

// 代码块复制按钮（md() 注入；主对话气泡和轨迹详情里的代码块都走这里）
document.addEventListener('click', async (e) => {
  const btn = e.target.closest('.code-copy-btn');
  if (!btn) return;
  const wrap = btn.closest('.code-block');
  const code = wrap && (wrap.querySelector('pre code') || wrap.querySelector('pre'));
  if (!code) return;
  try {
    await copyText(code.textContent);
    flashCheck(btn, btn.innerHTML);
  } catch (_) {}
});

// HTML 全屏开关（md 围栏和 render_html 工具块共用同一份工具条）
document.addEventListener('click', (e) => {
  if (e.target.closest('.html-fs-close')) { exitHtmlFullscreen(); return; }
  const fs = e.target.closest('.html-artifact-fs');
  if (!fs) return;
  const wrap = fs.closest('.code-block-artifact');
  if (!wrap) return;
  // 只有「在看的正是这个块」才收遮罩；在看别的块时要把全屏切过去，
  // 早先写成 if (isHtmlFullscreen()) exit... else enter...，结果全屏中点第二个块的
  // 按钮只会把遮罩关掉，永远切不过去。
  const box = wrap.querySelector('.html-frame-box');
  if (isHtmlFullscreen() && htmlFsReturn && htmlFsReturn.box === box) exitHtmlFullscreen();
  else enterHtmlFullscreen(wrap);
});
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape' && isHtmlFullscreen()) exitHtmlFullscreen();
});

// 本地图片加载失败（路径不在会话工作区内、文件被删、不是图片类型）时给个说明。
// <img> 的 error 事件不冒泡，只能用捕获阶段在 document 上兜。
document.addEventListener('error', (e) => {
  const img = e.target;
  if (!img || img.tagName !== 'IMG' || !img.classList.contains('local-img')) return;
  if (img.dataset.failed) return;
  img.dataset.failed = '1';
  const box = document.createElement('div');
  box.className = 'img-missing';
  box.textContent = '图片无法显示（不在会话工作区内、已被删除，或不是受支持的图片类型）';
  if (img.parentNode) img.parentNode.replaceChild(box, img);
}, true);

// HTML 沙箱渲染的开关按钮（md() 注入）。已渲染则拆掉 iframe 回到源码，
// 未渲染则挂 iframe —— 切换即销毁/重建，不留上一页面的脚本和定时器。
document.addEventListener('click', (e) => {
  const btn = e.target.closest('.html-artifact-toggle');
  if (!btn) return;
  const wrap = btn.closest('.code-block-artifact');
  if (!wrap) return;
  if (wrap.classList.contains('rendered')) unmountHtmlFrame(wrap);
  else mountHtmlFrame(wrap);
});

// Mermaid 渲染开关（与上面的 HTML 同一套交互：渲染 / 源码切换）。
document.addEventListener('click', (e) => {
  const btn = e.target.closest('.mermaid-artifact-toggle');
  if (!btn) return;
  const wrap = btn.closest('.code-block-mermaid');
  if (!wrap) return;
  if (wrap.classList.contains('rendered')) unmountMermaid(wrap);
  else mountMermaid(wrap);
});

// 沙箱页面的 postMessage 入口。opaque origin 只能发 '*'，所以唯一的信任依据是
// 「发送方是不是某个我们自己挂载的 frame」——别的窗口冒名发什么都不认。
window.addEventListener('message', (e) => {
  const d = e.data;
  if (!d || typeof d !== 'object') return;
  let frame = null;
  for (const f of htmlFrames) {
    if (e.source === f.contentWindow) { frame = f; break; }
  }
  if (!frame) return;
  if (d.type === 'ccgw:htmlframe:size') {
    const h = Number(d.height);
    if (!Number.isFinite(h)) return;
    frame.style.height = Math.max(HTML_FRAME_MIN_H, Math.min(HTML_FRAME_MAX_H, Math.ceil(h))) + 'px';
  } else if (d.type === 'ccgw:htmlframe:link') {
    // frame 侧已经按协议筛过，这里再挡一道，只放 http(s)。
    if (/^https?:\/\//i.test(d.url || '')) window.open(d.url, '_blank', 'noopener,noreferrer');
  }
});

$('#fv-close').onclick = closeFileViewer;

$('#fv-copy').onclick = async (e) => {
  const btn = e.currentTarget;
  try {
    await copyText($('#fv-path').textContent);
    flashCheck(btn, btn.innerHTML);
  } catch (_) {}
};

function wrapInMsg(node, box, ts) {
  // Attach to the last assistant message if present, else create one.
  const msgs = box || $('#messages');
  const last = msgs.lastElementChild;
  if (last && last.classList.contains('msg-assistant')) {
    last.querySelector('.bubble').appendChild(node);
  } else {
    addMsg('msg-assistant', 'claude', msgs, ts).appendChild(node);
  }
}

// ---------- partial streaming (stream_event) ----------
// CLI 开了 --include-partial-messages 后，除完整 assistant 事件外还会推原
// 始 API 流事件（type=stream_event）。text/thinking 的 delta 增量渲染进一
// 个带光标的 live 元素；block 完成时 CLI 会发含完整内容的 assistant 事件，
// renderEvent 用完整版替换对应 live 元素（去重）。若后端把 thinking strip
// 成空（完整版里没有内容），则固化保留 live 版。
// live 事件不落盘，历史回放里没有 stream_event，刷新后由完整事件兜底。

function liveBlock(kind) {
  // 当前活跃的 live block；类型切换时固化上一个（同时只有一个在流）
  const l = state.live;
  if (l && l.cur && l.cur.kind !== kind) liveFinalizeBlock(l.cur);
  return l && l.cur && l.cur.kind === kind ? l.cur : null;
}

function livePush(b) {
  if (!state.live) state.live = { msgId: '', blocks: [], cur: null };
  state.live.blocks.push(b);
  state.live.cur = b;
}

function liveFinalizeBlock(b) {
  b.done = true;
  if (b.raf) { cancelAnimationFrame(b.raf); b.raf = null; }
  livePaint(b);
  b.bodyEl.classList.remove('streaming');
  if (state.live && state.live.cur === b) state.live.cur = null;
}

function livePaint(b) {
  if (b.kind === 'text') b.bodyEl.innerHTML = md(b.buf);
  else b.bodyEl.textContent = b.buf;
}

function liveSchedulePaint(b) {
  if (b.raf) return; // 每帧最多重绘一次
  b.raf = requestAnimationFrame(() => {
    b.raf = null;
    livePaint(b);
    scrollBottom();
  });
}

// 取出最早创建的同类 live block（用于 assistant 完整事件到达时匹配替换）
function liveTake(kind) {
  const l = state.live;
  if (!l) return null;
  for (const b of l.blocks) {
    if (b.kind === kind && !b.taken) {
      b.taken = true;
      if (b.raf) { cancelAnimationFrame(b.raf); b.raf = null; } // 元素将被移除或固化，取消挂起的重绘
      return b;
    }
  }
  return null;
}

// turn 结束/中断时兜底：固化所有未被完整事件替换的 live block
function liveFinalizeAll() {
  const l = state.live;
  if (!l) return;
  for (const b of l.blocks) if (!b.taken && !b.done) liveFinalizeBlock(b);
  state.live = null;
}

function renderStreamEvent(d, box, ts) {
  const ev = d.event;
  if (!ev || typeof ev !== 'object') return;
  switch (ev.type) {
    case 'message_start':
      liveFinalizeAll();
      state.live = { msgId: (ev.message && ev.message.id) || '', blocks: [], cur: null };
      break;
    case 'content_block_start': {
      const cb = ev.content_block || {};
      const l = state.live;
      if (l && l.cur) liveFinalizeBlock(l.cur); // 新 block 开始，固化上一个（覆盖 text→text 等连续场景）
      if (cb.type === 'text') {
        const bubble = addMsg('msg-assistant', 'claude', box, ts);
        bubble.classList.add('streaming');
        const b = { kind: 'text', el: bubble.parentElement, bodyEl: bubble, buf: cb.text || '', done: false, taken: false, raf: null };
        livePush(b);
        liveSchedulePaint(b);
      } else if (cb.type === 'thinking') {
        const details = document.createElement('details');
        details.className = 'tool-block thinking-block';
        details.open = true; // 流式期间展开，完成后用户可手动折叠
        const summary = document.createElement('summary');
        summary.textContent = 'thinking…';
        const body = document.createElement('div');
        body.className = 'tool-body streaming';
        details.append(summary, body);
        wrapInMsg(details, box, ts);
        const b = { kind: 'thinking', el: details, bodyEl: body, buf: cb.thinking || '', done: false, taken: false, raf: null };
        livePush(b);
        liveSchedulePaint(b);
      } else {
        // tool_use 等：无增量渲染，等完整 assistant 事件（cur 已在上面固化）
      }
      break;
    }
    case 'content_block_delta': {
      const delta = ev.delta || {};
      const kind = delta.type === 'thinking_delta' ? 'thinking' : delta.type === 'text_delta' ? 'text' : null;
      if (!kind) break; // input_json_delta 等忽略
      const b = liveBlock(kind);
      if (!b) break;
      b.buf += kind === 'text' ? (delta.text || '') : (delta.thinking || '');
      liveSchedulePaint(b);
      break;
    }
    case 'content_block_stop': {
      const l = state.live;
      if (l && l.cur) liveFinalizeBlock(l.cur);
      break;
    }
    // message_delta/message_stop 无需处理：收尾由完整 assistant/result 事件承担
  }
}

// box defaults to the live message list; history pages render into a
// DocumentFragment which is then inserted at the top.
// opencode 的 token 级增量：后端把 message.part.delta 原样透出（只有
// field+delta，没有完整 part）。按 partID 聚进同一个气泡——同一 part 的
// 多次 delta 追加到同一处，换 part 则新起气泡。随后到达的完整
// message.part.updated（canonical assistant text block）会 liveTake 掉这个
// 气泡并用全文替换，因此流式与最终内容不会重复。
function renderOcTextDelta(d, box, ts) {
  const partId = d.partID || d.messageID;
  const text = d.delta || '';
  if (!partId || !text) return;
  if (state.ocPartID === partId && state.ocBubble) {
    state.ocBuf += text;
    const b = state.ocBubble;
    b.buf = state.ocBuf;
    liveSchedulePaint(b);
    return;
  }
  // 新 part：固化上一个气泡
  if (state.ocBubble) {
    liveFinalizeBlock(state.ocBubble);
    state.ocBubble = null;
  }
  const bubble = addMsg('msg-assistant', 'claude', box, ts);
  bubble.classList.add('streaming');
  const b = {
    kind: 'text', el: bubble.parentElement, bodyEl: bubble,
    buf: text, done: false, taken: false, raf: null,
  };
  state.ocPartID = partId;
  state.ocBuf = text;
  state.ocBubble = b;
  liveSchedulePaint(b);
}

// 单条事件渲染失败不能拖垮整段回放。renderEvent 里全是 DOM 手术（md 的代码块壳、
// 工具卡片配对、流式气泡替换），历史内容又是模型自由输出的文本——一旦某条抛错，
// flushHistory 的循环会当场中断，用户看到的是"对话到一半就没了"，而且完全不知道
// 是哪条消息出的问题。这里兜住：占位一行说明跳过了哪条，其余事件继续渲染。
function safeRenderEvent(ev, box) {
  try {
    renderEvent(ev, box);
  } catch (err) {
    const what = (err && err.name) || 'Error';
    console.warn('renderEvent 失败，已跳过该事件', ev && ev.kind, err);
    try {
      const host = box || $('#messages');
      const line = document.createElement('div');
      line.className = 'meta-line';
      line.textContent = `⚠️ 一条消息渲染失败（${what}），已跳过，其余内容不受影响`;
      host.appendChild(line);
    } catch (_) { /* 连占位都放不下就算了，不能再抛 */ }
  }
}

// 错误横幅：长文本必须折叠，否则能把主对话整个盖掉。
//
// #error-banner 在 <footer id="composer"> 里，而 composer 是 position:absolute 的浮层
// （浮在 #messages 之上，z-index:10）。banner 原本 white-space:pre-wrap 且不限高，
// 一条 CLI 崩溃堆栈（node OOM 有 40+ 帧、几千行）灌进去，banner 就长到几千像素：
//   ① 从底部向上溢出，把主对话盖得看不见；
//   ② syncComposerSpace() 按 composer 的 offsetHeight 给 #messages 设 padding-bottom，
//      同样被撑成几千 px，消息被顶到视口外。
// 所以：短的照旧整段显示；长的默认折叠成「摘要 + 展开全文」，展开后也只占有限的
// 视口高度并在内部滚动——限高是硬约束，任何情况下都不许盖住主对话。
const ERROR_INLINE_MAX_CHARS = 400;
const ERROR_INLINE_MAX_LINES = 6;

// 摘要取第一行"像错误"的行。node 的 OOM 首行是 "<--- Last few GCs --->" 这类噪声，
// 真正有用的是后面那行 "FATAL ERROR: ... JavaScript heap out of memory"。
function errorSummary(text) {
  const noise = /^\s*(<[-=]{2,}|={2,})/;
  const lines = String(text).split('\n');
  let pick = lines.find((l) => /error/i.test(l) && !noise.test(l));
  if (!pick) pick = lines.find((l) => l.trim() && !noise.test(l));
  return (pick || '出错了').trim();
}

// 所有写 #error-banner 的地方都走这里，长短按同一套规则渲染。
function setErrorBanner(text, opts) {
  const banner = $('#error-banner');
  if (!banner) return;
  const o = opts || {};
  banner.textContent = '';
  banner.classList.toggle('warning', !!o.warning);
  const raw = text == null ? '' : String(text);
  const lines = raw.split('\n');
  if (raw.length <= ERROR_INLINE_MAX_CHARS && lines.length <= ERROR_INLINE_MAX_LINES) {
    const pre = document.createElement('div');
    pre.className = 'err-text';
    pre.textContent = raw || o.fallback || 'unknown error';
    banner.appendChild(pre);
  } else {
    const head = document.createElement('div');
    head.className = 'err-head';
    const sum = document.createElement('span');
    sum.className = 'err-summary';
    sum.textContent = errorSummary(raw);
    sum.title = errorSummary(raw);
    const btn = document.createElement('button');
    btn.className = 'err-toggle';
    btn.type = 'button';
    btn.textContent = '展开全文';
    head.appendChild(sum);
    head.appendChild(btn);
    banner.appendChild(head);

    const full = document.createElement('div');
    full.className = 'err-full';
    full.textContent = raw;
    banner.appendChild(full);

    btn.addEventListener('click', () => {
      const open = full.classList.toggle('open');
      btn.textContent = open ? '收起' : '展开全文';
    });
  }
  banner.classList.remove('hidden');
}

function renderEvent(ev, box) {
  box = box || $('#messages');
  const d = ev.data;
  if (ev.kind === 'rewind') {
    // Replay-time marker: the server already truncated history, so just render.
    appendRewindMarker(box, d && d.checkpoint, d && d.detail);
    return;
  }
  if (ev.kind === 'user') {
    // Replace any pending optimistic bubble with the authoritative one
    // (which contains only text — images are not stored in events).
    const last = box.lastElementChild;
    if (last && last.classList.contains('msg-optimistic')) last.remove();
    const bubble = addMsg('msg-user', '你', box, ev.ts);
    bubble.textContent = d.text;
    // A checkpoint uuid means this message can be rewound (files + dialogue).
    if (d && d.checkpoint) {
      const msgDiv = bubble.parentElement;
      msgDiv.dataset.checkpoint = d.checkpoint;
      addRewindBtn(msgDiv);
    }
    // User event authoritative bubble rendered → refresh pinned target
    if (box === $('#messages')) refreshPinnedPrompt();
  } else if (ev.kind === 'stream') {
    if (d.type === 'system') {
      if (d.subtype === 'init') {
        if (Array.isArray(d.slash_commands) && state.current) {
          state.current.slash_commands = d.slash_commands;
          const key = (state.current.cli || '') + '\x00' + (state.current.cwd || '');
          state.slashCommandsCache[key] = d.slash_commands;
          try { localStorage.setItem('cc-webui:slash-cache', JSON.stringify(state.slashCommandsCache)); } catch (_) {}
        }
        const line = document.createElement('div');
        line.className = 'meta-line';
        line.textContent = `session ${String(d.session_id || '').slice(0, 8)} · model ${d.model || '?'} · ${d.cwd || ''}`;
        box.appendChild(line);
      }
      return; // ignore thinking_tokens and other system noise
    }
    if (d.type === 'assistant' && d.parent_tool_use_id) {
      // 子代理消息（Agent 工具派生）：聚合进按 parent_tool_use_id 分组的
      // 折叠卡片，不作为主对话气泡渲染。
      renderSubagentEvent(d, box, ev.ts);
    } else if (d.type === 'assistant' && d.message && Array.isArray(d.message.content)) {
      for (const block of d.message.content) {
        // 空消息过滤：无正文的 text/thinking 不占位（Claude 5 会发无文本 thinking，
        // 中断/空 delta 也会留下空壳），否则时间线被空白块切断。
        if (block.type === 'text') {
          if (!block.text || !block.text.trim()) continue;
          const lb = liveTake('text');
          if (lb) lb.el.remove(); // 完整版替换 live 气泡，避免重复
          // opencode 流式：完整 part 的 id 与正在累积的 partID 相同时，复用那个
          // 气泡（原文替换流式缓冲），否则会在正文上方留一个重复气泡。
          if (state.ocBubble && block.id && state.ocPartID === block.id) {
            const b = state.ocBubble;
            b.buf = block.text;
            liveFinalizeBlock(b);
            b.bodyEl.innerHTML = md(block.text);
            state.ocBubble = null;
            state.ocPartID = '';
            state.ocBuf = '';
            break;
          }
          const bubble = addMsg('msg-assistant', 'claude', box, ev.ts);
          bubble.innerHTML = md(block.text);
        } else if (block.type === 'tool_use') {
          if (!block.name) continue;
          renderToolUse(block, box, ev.ts);
        } else if (block.type === 'thinking') {
          const lb = liveTake('thinking');
          if (block.thinking && block.thinking.trim()) {
            if (lb) lb.el.remove(); // 完整版替换 live thinking
            const details = document.createElement('details');
            details.className = 'tool-block thinking-block';
            const summary = document.createElement('summary');
            summary.textContent = 'thinking…';
            const body = document.createElement('div');
            body.className = 'tool-body';
            body.textContent = block.thinking;
            details.append(summary, body);
            wrapInMsg(details, box, ev.ts);
          } else if (lb) {
            liveFinalizeBlock(lb); // 完整版被后端 strip 成空，保留 live 版
          }
        }
      }
    } else if (d.type === 'oc_text_delta') {
      renderOcTextDelta(d, box, ev.ts);
    } else if (d.type === 'stream_event') {
      renderStreamEvent(d, box, ev.ts);
    } else if (d.type === 'user' && d.origin && d.origin.kind === 'peer') {
      // 入站 peer 消息（其他会话 SendMessage 来的）：专用气泡，左边对齐、
      // 紫色描边以区别于自己的消息。body 是解码后的原文，按 md 渲染。
      const name = escapeHtml(d.origin.name || '另一个会话');
      const bubble = addMsg('msg-peer', `⇢ 来自 ${name}`, box, ev.ts);
      bubble.innerHTML = md(d.origin.body || '');
    } else if (d.type === 'user' && d.parent_tool_use_id && d.message && Array.isArray(d.message.content)) {
      // 子代理的 tool_result：配对进对应卡片的工具视图（result 到得早于
      // 卡片创建时也会先把卡片壳建出来）
      const card = ensureSubagentCard(d.parent_tool_use_id, d, box, ev.ts);
      for (const b of d.message.content) {
        if (b && b.type === 'tool_result') renderToolResult(b, card.body, true);
      }
    } else if (d.type === 'user' && !d.parent_tool_use_id && d.message && Array.isArray(d.message.content)) {
      for (const block of d.message.content) {
        if (block && block.type === 'tool_result') renderToolResult(block, box);
      }
    } else if (d.type === 'result') {
      liveFinalizeAll(); // 兜底：未被完整 assistant 事件替换的 live block 固化保留
      const meta = document.createElement('div');
      meta.className = 'result-meta';
      const parts = [];
      if (d.subtype) parts.push(d.subtype);
      if (typeof d.duration_ms === 'number') parts.push(`${(d.duration_ms / 1000).toFixed(1)}s`);
      if (typeof d.total_cost_usd === 'number') parts.push(`$${d.total_cost_usd.toFixed(4)}`);
      if (d.usage) {
        const u = d.usage;
        parts.push(`tokens in:${u.input_tokens || 0} out:${u.output_tokens || 0}`);
      }
      meta.textContent = parts.join(' · ');
      box.appendChild(meta);
    }
  } else if (ev.kind === 'status') {
    if (d.state === 'turn_start') {
      updateStatusUI({ running: true, queued: 0 });
    } else if (d.state === 'goal_round' || d.state === 'goal_end') {
      // 目标状态由服务端判定：这里只回读一次会话把轮次/终态拉回来，不自己算。
      if (state.current && !state.renderingHistory) {
        api(`/api/sessions/${state.current.id}`).then((sess) => {
          applySession(sess);
          if (d.state === 'goal_end') {
            const msg = d.status === 'met' ? '自动续跑结束：目标已达标'
              : d.status === 'exhausted' ? '自动续跑结束：已达上限'
              : '自动续跑已停止';
            goalToast(msg, d.reason || '');
          }
        }).catch(() => {});
      }
    } else if (d.state === 'turn_end' || d.state === 'interrupted') {
      liveFinalizeAll(); // 中断时部分 block 永远等不到完整事件，固化已生成的内容
      updateStatusUI({ running: false, queued: 0 });
      // Submit any message held while this turn was running. 历史回放里的旧
      // turn_end 不算数——由 flushHistory 在回放结束后按最新状态统一补交。
      if (!state.renderingHistory) {
        flushPending();
        refreshCurrentStatus();
      }
    } else if (d.state === 'error') {
      liveFinalizeAll();
      updateStatusUI({ running: false, queued: 0 });
      // For recoverable CLI errors (e.g. ducc's unrecognized_model warning
      // after usable output has already streamed), show a warning banner and
      // allow the pending message to flush so the user can keep chatting.
      if (d.recoverable) {
        setErrorBanner(d.error || 'CLI 报告非致命错误', { warning: true });
        if (!state.renderingHistory) {
          flushPending();
          refreshCurrentStatus();
        }
      } else {
        // Don't auto-flush on hard error — leave the pending bubble visible so the
        // user can cancel or wait and resend after the failure is resolved.
        setErrorBanner(d.error || 'unknown error');
        refreshCurrentStatus();
      }
    }
  }
}

// 子代理消息卡片：Agent 工具派生的 subagent 在聊天流里按 parent_tool_use_id
// 聚合成一个可折叠卡片。标题优先用服务端回放给的 d.subagent（来自
// meta.json），实时流没有它时回查 agentToolMeta（Agent tool_use 的 input）。
// 渲染 text 块与 tool_use 块（与回放侧 ConvertSubagents 的产出一致，
// thinking 两侧都剔——CLI 实时透传会把完整事件里的 thinking strip 掉）；
// 子代理的 tool_result 经 renderEvent 的 user 分支配对进卡片。默认折叠。
function ensureSubagentCard(key, d, box, ts) {
  let card = state.subagentCards[key];
  if (card) return card;
  const sa = (d && d.subagent) || {};
  const meta = state.agentToolMeta[key] || {};
  const title = sa.title || meta.title || '子代理';
  const type = sa.type || meta.type || '';
  const details = document.createElement('details');
  details.className = 'tool-block subagent-block';
  const summary = document.createElement('summary');
  const body = document.createElement('div');
  body.className = 'tool-body subagent-body';
  details.append(summary, body);
  wrapInMsg(details, box, ts);
  const label = `🤖 子代理${type ? ' · ' + escapeHtml(type) : ''} · ${escapeHtml(title)}`;
  card = state.subagentCards[key] = { summary, body, count: 0, tools: 0, label };
  return card;
}

function subagentRefreshSummary(card) {
  const parts = [];
  if (card.count) parts.push(card.count + ' 条');
  if (card.tools) parts.push(card.tools + ' 次调用');
  card.summary.innerHTML = card.label + (parts.length ? `<span class="subagent-count">${parts.join(' · ')}</span>` : '');
}

function renderSubagentEvent(d, box, ts) {
  const blocks = (d.message && Array.isArray(d.message.content)) ? d.message.content : [];
  // 空消息过滤：整条 subagent 事件无有效内容时连卡片壳都不建，避免空白卡片。
  const hasContent = blocks.some((b) => b && ((b.type === 'text' && b.text && b.text.trim()) || (b.type === 'tool_use' && b.name)));
  if (!hasContent) return;
  const card = ensureSubagentCard(d.parent_tool_use_id, d, box, ts);
  for (const b of blocks) {
    if (!b) continue;
    if (b.type === 'text') {
      if (!b.text || !b.text.trim()) continue;
      const div = document.createElement('div');
      div.className = 'subagent-msg';
      div.innerHTML = md(b.text);
      card.body.appendChild(div);
      card.count++;
    } else if (b.type === 'tool_use' && b.name) {
      // raw：工具视图直接挂进卡片体，不包消息气泡壳；结果由 tool_result
      // 事件经 state.toolBlocks 配对回同一个 details 里
      renderToolUse(b, card.body, ts, true);
      card.tools++;
    }
  }
  subagentRefreshSummary(card);
}

async function refreshCurrentStatus() {
  if (!state.current) return;
  try {
    const s = await api(`/api/sessions/${state.current.id}`);
    state.current = s;
    updateStatusUI(s.status);
    updateResidentUI();
    loadSessions();
  } catch (_) {}
}

// ---------- composer ----------
const input = $('#input');

// The composer floats over #messages (absolute on desktop, fixed on mobile),
// which reserves room for it via padding-bottom. That padding must track the
// composer's real height — the textarea auto-grows, and attachments / error
// banner / workspace gate make it taller — otherwise an expanded composer
// covers the bottom of the last messages.
const composerEl = $('#composer');
const messagesEl = $('#messages');
const trajViewEl = $('#traj-view');
function syncComposerSpace() {
  const gap = parseFloat(getComputedStyle(composerEl).bottom) || 0;
  const pad = `${composerEl.offsetHeight + gap + 8}px`;
  messagesEl.style.paddingBottom = pad;
  if (trajViewEl) trajViewEl.style.paddingBottom = pad;
}
new ResizeObserver(() => {
  syncComposerSpace();
  // Padding grows downward; keep the view pinned if the user was at the bottom.
  if (state.nearBottom) messagesEl.scrollTop = messagesEl.scrollHeight;
}).observe(composerEl);
syncComposerSpace();

// ---------- slash command autocomplete ----------
// Slash commands come from the system/init event (persisted server-side in
// session.SlashCommands, also captured live in renderEvent). We detect `/` at
// the start of a word, show a filtered popup, and let the user navigate with
// arrow keys / Tab. Selected commands get a blue highlight in the input via
// a mirror div behind the transparent-text textarea.

const slashPopupEl = $('#slash-popup');
const inputMirror = $('#input-mirror');
const nsSlashPopupEl = $('#ns-slash-popup');
const nsInputEl = $('#ns-input');
let slashState = { open: false, items: [], active: 0 };
// 活跃的 slash 上下文：哪个 textarea + 哪个 popup 在用（focus 切换）
let slashPopup = slashPopupEl;
let slashTextarea = input;

function getSlashCommands() {
  if (state.current) {
    const key = (state.current.cli || '') + '\x00' + (state.current.cwd || '');
    return state.current.slash_commands || state.slashCommandsCache[key] || [];
  }
  // ns-view 上下文：用 nsPendingCwd + #ns-cli 查缓存
  const cli = (nsInputEl && $('#ns-cli')?.value) || '';
  const key = cli + '\x00' + (nsPendingCwd || '');
  return state.slashCommandsCache[key] || [];
}

// Find the slash token being typed: `/word` at start of input or after whitespace.
function currentSlashToken(text, pos) {
  const before = text.slice(0, pos);
  const m = before.match(/(?:^|\s)(\/[\w-]*)$/);
  return m ? { prefix: m[1], start: pos - m[1].length } : null;
}

function openSlashPopup(filter) {
  const commands = getSlashCommands();
  const q = filter.toLowerCase();
  const items = q ? commands.filter((c) => c.toLowerCase().includes(q)) : commands.slice();
  if (items.length === 0) { closeSlashPopup(); return; }
  slashState.open = true;
  slashState.items = items;
  slashState.active = 0;
  slashPopup.innerHTML = '';
  for (let i = 0; i < items.length; i++) {
    const el = document.createElement('div');
    el.className = 'slash-popup-item' + (i === 0 ? ' active' : '');
    el.innerHTML = `<span class="slash-name">/${escapeHtml(items[i])}</span>`;
    el.onclick = () => selectSlashItem(i);
    slashPopup.appendChild(el);
  }
  slashPopup.classList.remove('hidden');
}

function closeSlashPopup() {
  slashState.open = false;
  slashState.items = [];
  slashPopup.classList.add('hidden');
}

function moveSlashActive(delta) {
  if (!slashState.open) return;
  const n = slashState.items.length;
  slashState.active = (slashState.active + delta + n) % n;
  for (const el of slashPopup.children) el.classList.remove('active');
  slashPopup.children[slashState.active].classList.add('active');
  slashPopup.children[slashState.active].scrollIntoView({ block: 'nearest' });
}

function selectSlashItem(idx) {
  if (!slashState.open) return;
  const name = slashState.items[idx ?? slashState.active];
  if (!name) return;
  const ta = slashTextarea;
  const text = ta.value;
  const pos = ta.selectionStart;
  const before = text.slice(0, pos);
  const m = before.match(/(?:^|\s)(\/[\w-]*)$/);
  if (!m) { closeSlashPopup(); return; }
  const start = pos - m[1].length;
  const replacement = (m[0].startsWith(' ') ? ' ' : '') + '/' + name + ' ';
  const newText = text.slice(0, start) + replacement + text.slice(pos);
  ta.value = newText;
  const caret = start + replacement.length;
  ta.setSelectionRange(caret, caret);
  ta.focus();
  closeSlashPopup();
  onInputChange();
}

// Render the mirror: slash command tokens that match known commands get blue.
// The mirror ALWAYS renders the text so the transparent textarea stays readable.
function renderInputMirror() {
  const text = input.value;
  if (!text) { inputMirror.innerHTML = ''; return; }
  const commands = getSlashCommands();
  const cmdSet = commands.length ? new Set(commands) : null;
  const parts = text.split(/(\s+)/);
  let html = '';
  for (const part of parts) {
    if (cmdSet && part.startsWith('/') && cmdSet.has(part.slice(1))) {
      html += `<span class="slash-cmd">${escapeHtml(part)}</span>`;
    } else {
      html += escapeHtml(part);
    }
  }
  inputMirror.innerHTML = html;
  // 镜像高度钉到 textarea 实际高度（180 封顶），它才成为真正的滚动盒，
  // scrollTop 同步才会生效（内容超高后 textarea 内部滚动，镜像不滚则光标错位）。
  inputMirror.style.height = input.clientHeight + 'px';
  inputMirror.scrollTop = input.scrollTop;
}

function onInputChange() {
  const ta = slashTextarea;
  ta.style.height = 'auto';
  ta.style.height = Math.min(ta.scrollHeight, ta === input ? 180 : 320) + 'px';
  if (state.current && ta === input) draft().text = ta.value;
  // Slash popup detection
  const tok = currentSlashToken(ta.value, ta.selectionStart);
  if (tok) {
    const filter = tok.prefix.slice(1); // without the leading /
    openSlashPopup(filter);
  } else {
    closeSlashPopup();
  }
  if (ta === input) renderInputMirror();
}

input.addEventListener('focus', () => { slashTextarea = input; slashPopup = slashPopupEl; });
input.addEventListener('input', onInputChange);
input.addEventListener('scroll', renderInputMirror);
input.addEventListener('keydown', (e) => {
  if (slashState.open) {
    if (e.key === 'ArrowDown') { e.preventDefault(); moveSlashActive(1); return; }
    if (e.key === 'ArrowUp') { e.preventDefault(); moveSlashActive(-1); return; }
    if (e.key === 'Tab' || (e.key === 'Enter' && !e.shiftKey)) {
      e.preventDefault(); selectSlashItem(); return;
    }
    if (e.key === 'Escape') { e.preventDefault(); closeSlashPopup(); return; }
  }
  if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
    e.preventDefault();
    sendMessage();
  }
});
input.addEventListener('blur', () => { setTimeout(closeSlashPopup, 200); });
input.addEventListener('paste', async (e) => {
  const items = e.clipboardData && e.clipboardData.items;
  if (!items) return;
  const files = [];
  for (const item of items) {
    if (item.kind === 'file') {
      const f = item.getAsFile();
      if (f) files.push(f);
    }
  }
  if (files.length) {
    e.preventDefault();
    await attachFiles(files);
  }
});

$('#btn-send').onclick = sendMessage;

// ---------- attachments ----------
// 两类附件：图片走流式多模态（base64 内联），其他文件上传到服务器落盘
// （data/uploads/<sid>/），agent 通过消息文本里的路径用 Read/Bash 处理。
const ALLOWED_MIME = ['image/png', 'image/jpeg', 'image/gif', 'image/webp'];
const MAX_IMAGES = 4;
const MAX_FILES = 8;
const MAX_IMAGE_BYTES = 10 * 1024 * 1024; // 10MB per image
const MAX_FILE_BYTES = 40 * 1024 * 1024;  // 40MB per file

$('#btn-attach').onclick = () => $('#file-input').click();
$('#file-input').addEventListener('change', async (e) => {
  await attachFiles([...e.target.files]);
  e.target.value = '';
});

input.addEventListener('dragover', (e) => {
  if (!e.dataTransfer || !e.dataTransfer.types.includes('Files')) return;
  e.preventDefault();
  e.dataTransfer.dropEffect = 'copy';
});
input.addEventListener('drop', async (e) => {
  if (!e.dataTransfer || !e.dataTransfer.files.length) return;
  e.preventDefault();
  await attachFiles([...e.dataTransfer.files]);
});

async function attachFiles(files) {
  // mcode 引擎没有内嵌图片（base64）通道（服务端 400）；图片降级为文件附件
  // 上传，runner 以引擎自己的官方附件参数（mcode 的 --file）传给模型——视觉
  // 模型直接可见，文本模型也能用工具读取。
  // claude 与 qoder 走 --input-format stream-json 的多模态载荷，图片原样内嵌；
  // opencode 走 `opencode serve` 的 prompt file part，图片同样原样内嵌。
  const noInlineImages = state.current && !engineTakesImages(state.current.engine || 'claude');
  for (const f of files) {
    if (ALLOWED_MIME.includes(f.type) && !noInlineImages) {
      const imgs = state.attached.filter((a) => a.kind === 'image').length;
      if (imgs >= MAX_IMAGES) {
        alert(`最多 ${MAX_IMAGES} 张图片`);
        continue;
      }
      if (f.size > MAX_IMAGE_BYTES) {
        alert(`图片过大：${f.name}（${fmtMiB(f.size)}MB > ${MAX_IMAGE_BYTES >> 20}MB）`);
        continue;
      }
      const data = await readAsDataURL(f).then(stripDataURLPrefix);
      state.attached.push({ kind: 'image', media_type: f.type, data });
      continue;
    }
    // 非图片文件：先上传到服务器落盘，后面发送的消息只带路径元数据，
    // 待发送/草稿/切会话都不会带着大 base64 在内存里打转。
    const nFiles = state.attached.filter((a) => a.kind === 'file').length;
    if (nFiles >= MAX_FILES) {
      alert(`最多 ${MAX_FILES} 个文件附件`);
      continue;
    }
    if (f.size > MAX_FILE_BYTES) {
      alert(`文件过大：${f.name}（${fmtMiB(f.size)}MB > ${MAX_FILE_BYTES >> 20}MB）`);
      continue;
    }
    try {
      const up = await uploadAttachment(f, state.current.id);
      state.attached.push({ kind: 'file', name: up.name, size: up.size, path: up.path });
    } catch (e) {
      alert(`上传失败：${f.name}（${e.message || 'unknown error'}）`);
      continue;
    }
  }
  if (state.current) draft().attachments = state.attached;
  renderAttachedThumbs();
}

// POST the raw file bytes to the server; it persists them and returns the
// on-disk path that the agent will be told about. sid 显式传入——主 composer
// 传当前会话；新建会话流程在会话建成后才上传，也走这里。
async function uploadAttachment(f, sid) {
  if (!sid) throw new Error('请先进入一个会话再添加附件');
  const name = attachmentName(f);
  return api(`/api/sessions/${sid}/uploads?name=${encodeURIComponent(name)}`, {
    method: 'POST',
    body: f,
    headers: { 'Content-Type': 'application/octet-stream' },
  });
}

// 上传用的文件名。剪贴板里来的文件可能压根没有名字（Safari），服务端
// sanitizeUploadName 会直接 400，按 MIME 补一个即可。同一批剪贴板图片在
// 各浏览器里都叫 image.png——落盘重名由服务端去重（writeUploadExclusive
// 追加 -2/-3 后缀），这里不掺和，免得显示名和真实文件名对不上。
function attachmentName(f) {
  if (f.name && f.name.trim()) return f.name.trim();
  const ext = String(f.type || '').split('/')[1] || 'bin';
  return `attachment.${ext.replace(/[^a-z0-9]/gi, '')}`;
}

function fmtMiB(n) { return (n / 1024 / 1024).toFixed(1); }
function fmtBytes(n) {
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(0) + ' KB';
  return fmtMiB(n) + ' MB';
}

function readAsDataURL(file) {
  return new Promise((resolve, reject) => {
    const r = new FileReader();
    r.onload = () => resolve(r.result);
    r.onerror = () => reject(r.error);
    r.readAsDataURL(file);
  });
}
function stripDataURLPrefix(s) {
  const i = s.indexOf(',');
  return i >= 0 ? s.slice(i + 1) : s;
}

function renderAttachedThumbs() {
  const box = $('#attached-images');
  box.innerHTML = '';
  state.attached.forEach((att, i) => {
    const t = document.createElement('div');
    if (att.kind === 'image') {
      t.className = 'attached-thumb';
      t.innerHTML = `<img alt=""><button class="x" title="移除"><i data-lucide="x"></i></button>`;
      t.querySelector('img').src = `data:${att.media_type};base64,${att.data}`;
    } else {
      t.className = 'attached-thumb attached-file';
      t.innerHTML = `<i data-lucide="file" class="af-icon"></i><span class="af-name"></span><span class="af-size"></span><button class="x" title="移除"><i data-lucide="x"></i></button>`;
      t.querySelector('.af-name').textContent = att.name;
      t.querySelector('.af-name').title = att.name;
      t.querySelector('.af-size').textContent = fmtBytes(att.size);
    }
    t.querySelector('.x').onclick = () => {
      state.attached.splice(i, 1);
      if (state.current) draft().attachments = state.attached;
      renderAttachedThumbs();
    };
    box.appendChild(t);
  });
  renderIcons(box);
}

// Is a turn currently running for the open session? Derived from the session
// status (set live by status SSE events and refreshCurrentStatus).
function isRunning() {
  return !!(state.current && state.current.status && state.current.status.running);
}

async function sendMessage() {
  const text = input.value.trim();
  if ((!text && state.attached.length === 0) || !state.current) return;
  const images = state.attached.filter((a) => a.kind === 'image');
  // 文件附件只带服务端已落盘的路径元数据，不带上大文件字节。
  const files = state.attached.filter((a) => a.kind === 'file').map((a) => ({ name: a.name, path: a.path }));
  // Clear the composer regardless of path: the text now lives either in the
  // POST (sent now) or in state.pendingBySid (held for the running turn to end).
  input.value = '';
  input.style.height = 'auto';
  renderInputMirror();
  state.attached = [];
  if (state.current) { draft().text = ''; draft().attachments = []; }
  renderAttachedThumbs();
  $('#error-banner').classList.add('hidden');

  // 用户人工追问 = 这次回来过了：清掉 TODO 橙黄点（若标过）。落库在服务端，
  // 多端同步；乐观先清，不必等 PATCH 回包才消点。
  if (state.current.todo) patchSessionFlag(state.current.id, { todo: false });

  // While a turn is streaming, don't POST immediately — the backend would
  // queue it, but the optimistic bubble lands mid-stream and the on-screen
  // order diverges from the actual (post-turn) order. Hold the message and
  // auto-submit when the running turn ends.
  if (isRunning()) {
    holdPending(text, images, files);
    return;
  }
  await postMessage({ text, images, files });
}

// POST a message and optimistically render its user bubble.
async function postMessage(payload) {
  appendOptimisticUser(payload.text, payload.images, payload.files);
  try {
    await api(`/api/sessions/${state.current.id}/messages`, {
      method: 'POST',
      body: JSON.stringify(payload),
    });
    updateStatusUI({ running: true, queued: 0 });
  } catch (e) {
    // Send failed: drop the optimistic bubble and surface the error so the
    // user can retry, instead of leaving a ghost message.
    $('#messages').querySelectorAll('.msg-optimistic').forEach((n) => n.remove());
    throw e;
  }
}

// Hold a message until the running turn ends, then flushPending submits it.
// 按会话保存：切走再切回 chip 仍在，轮次在切走期间结束的由 flushHistory 补交。
// Renders a dashed chip docked to the composer so the user sees what's queued.
function holdPending(text, images, files) {
  if (!state.current) return;
  // A second send while one is already held replaces it (latest intent wins).
  state.pendingBySid[state.current.id] = { text, images, files };
  renderPendingBubble();
}

// Submit the held message (if any) now that the session is idle. Called from
// the status event handler on live turn_end / interrupted, and from
// flushHistory when the turn ended while the session was in the background.
async function flushPending() {
  if (!state.current) return;
  const sid = state.current.id;
  const payload = state.pendingBySid[sid];
  if (!payload) return;
  delete state.pendingBySid[sid];
  clearPendingBubble();
  try {
    await postMessage(payload);
  } catch (e) {
    // Re-hold on transient failure so it isn't lost; surface the error.
    // 若 await 期间用户已切走，payload 仍挂回原会话，chip 由 renderPendingBubble
    // 按当前会话重绘（当前会话无 pending 时等于清除）。
    state.pendingBySid[sid] = payload;
    renderPendingBubble();
    setErrorBanner('待发送消息提交失败：' + (e.message || 'unknown error'));
  }
}

function cancelPending() {
  if (state.current) delete state.pendingBySid[state.current.id];
  clearPendingBubble();
}

// ---------- pending message chip (pinned to the composer) ----------
// A held message shows as a dashed chip docked at the top of the composer card
// (above the attachments row) with a 待发送 label and a cancel button. It's a
// composer affordance, not part of the message stream — living inside the
// floating composer it never scrolls away while the turn streams, and the
// composer's ResizeObserver grows the #messages padding to make room for it.
function pendingEl() { return $('#pending-bubble'); }

function renderPendingBubble() {
  const p = state.current && state.pendingBySid[state.current.id];
  if (!p) { clearPendingBubble(); return; }
  let el = pendingEl();
  if (!el) {
    el = document.createElement('div');
    el.id = 'pending-bubble';
    el.className = 'pending-bubble';
    el.innerHTML = '<span class="pb-tag"></span><span class="pb-text"></span><button class="pb-cancel" type="button" title="取消待发送"></button>';
    el.querySelector('.pb-cancel').onclick = cancelPending;
    const cancelIcon = document.createElement('i');
    cancelIcon.dataset.lucide = 'x';
    el.querySelector('.pb-cancel').appendChild(cancelIcon);
    renderIcons(el);
  }
  // 固定在输入框卡片顶部（insertBefore 对已有节点是移动），与消息流无关，
  // 流式事件再多也不会把它顶走。
  const card = $('.composer-card');
  card.insertBefore(el, card.firstChild);
  el.querySelector('.pb-tag').textContent = '待发送 · 本轮结束后自动提交';
  const txt = el.querySelector('.pb-text');
  const label = p.text || ((p.images && p.images.length) || (p.files && p.files.length) ? '(仅附件)' : '');
  txt.textContent = label;
  el.classList.toggle('has-text', !!label);
  el.classList.toggle('has-images', !!(p.images && p.images.length));
}

function clearPendingBubble() {
  const el = pendingEl();
  if (el) el.remove();
}

// ---------- 任务列表面板（dock 在输入框卡片顶部） ----------
// 数据源是 historyBuf：回放、实时事件、rewind 截断都汇聚到它，面板状态从它
// 折叠（fold）出来，单一事实来源。重置点（切会话/重连/rewind）全量重折，
// 实时事件增量折叠。追踪 TaskCreate/TaskUpdate（新任务系统），旧会话的
// TodoWrite 作为回退（其入参每次都是全量快照，最后一次调用生效）。
// 任务 id 由 CLI 分配，TaskCreate 入参里没有——先以 callId 占位，等
// tool_result（"Task #N created successfully"）到达后换成真实 id。

function taskFoldReset() {
  state.taskFold = { tasks: {}, order: [], pendingCreates: {}, pendingLists: {}, todos: null };
  state.taskPanelTouched = false;
}

// 全量重折：回放 flush / rewind 截断后调用（缓冲区的增删使增量状态失效）
function taskFoldAll() {
  taskFoldReset();
  for (const ev of state.historyBuf) taskFoldEvent(ev);
}

// 增量折叠单个事件；返回任务状态是否有变化
function taskFoldEvent(ev) {
  const f = state.taskFold;
  if (!f || ev.kind !== 'stream' || !ev.data) return false;
  const d = ev.data;
  const content = d.message && Array.isArray(d.message.content) ? d.message.content : null;
  if (!content) return false;
  let changed = false;
  if (d.type === 'assistant') {
    for (const b of content) {
      if (!b || b.type !== 'tool_use') continue;
      const input = b.input || {};
      switch ((b.name || '').toLowerCase()) {
        case 'todowrite':
          if (Array.isArray(input.todos)) { f.todos = input.todos; changed = true; }
          break;
        case 'taskcreate': {
          const tmp = 'tmp:' + (b.id || f.order.length);
          f.tasks[tmp] = { id: tmp, subject: input.subject || '', description: input.description || '', activeForm: input.activeForm || '', status: 'pending' };
          f.order.push(tmp);
          if (b.id) f.pendingCreates[b.id] = tmp;
          changed = true;
          break;
        }
        case 'taskupdate': {
          const id = String(input.taskId || '');
          if (!id) break;
          if (input.status === 'deleted') { // deleted 任务直接移除，与 CLI 行为一致
            if (f.tasks[id]) { delete f.tasks[id]; f.order = f.order.filter((x) => x !== id); changed = true; }
            break;
          }
          // 引用了未知 id（任务建在本会话之外）→ 先立占位，字段后续补齐
          const t = f.tasks[id] || (f.tasks[id] = { id, subject: '', description: '', activeForm: '', status: 'pending' }, f.order.push(id), f.tasks[id]);
          if (input.subject) t.subject = input.subject;
          if (input.description) t.description = input.description;
          if (input.activeForm) t.activeForm = input.activeForm;
          if (input.status) t.status = input.status;
          changed = true;
          break;
        }
        case 'tasklist': // 状态在结果里，记下 callId 等 tool_result
          if (b.id) f.pendingLists[b.id] = true;
          break;
      }
    }
  } else if (d.type === 'user') {
    for (const b of content) {
      if (!b || b.type !== 'tool_result') continue;
      const tmp = f.pendingCreates[b.tool_use_id];
      if (tmp) {
        delete f.pendingCreates[b.tool_use_id];
        const m = /Task #(\d+) created/i.exec(toolResultText(b.content));
        const t = f.tasks[tmp];
        if (m && t && !f.tasks[m[1]]) {
          // 换真实 id，保持列表位置不变
          delete f.tasks[tmp];
          t.id = m[1];
          f.tasks[t.id] = t;
          f.order = f.order.map((x) => (x === tmp ? t.id : x));
        } else {
          // 创建失败（错误结果）或同 id 已存在 → 丢弃占位项
          delete f.tasks[tmp];
          f.order = f.order.filter((x) => x !== tmp);
        }
        changed = true;
      } else if (f.pendingLists[b.tool_use_id]) {
        delete f.pendingLists[b.tool_use_id];
        // TaskList 结果为 JSON 数组时并入（任务建在本会话之外的兜底）；纯文本
        // 格式不解析——常规流程已被 create/update 折叠覆盖
        let arr = null;
        try { arr = JSON.parse(toolResultText(b.content)); } catch (_) {}
        if (Array.isArray(arr)) {
          for (const it of arr) {
            if (!it || it.id == null) continue;
            const id = String(it.id);
            if (it.status === 'deleted') {
              if (f.tasks[id]) { delete f.tasks[id]; f.order = f.order.filter((x) => x !== id); }
              continue;
            }
            const t = f.tasks[id] || (f.tasks[id] = { id, subject: '', description: '', activeForm: '', status: 'pending' }, f.order.push(id), f.tasks[id]);
            if (it.subject) t.subject = it.subject;
            if (it.description) t.description = it.description;
            if (it.activeForm) t.activeForm = it.activeForm;
            if (it.status) t.status = it.status;
          }
          changed = true;
        }
      }
    }
  }
  return changed;
}

// 面板当前应展示的条目：Task* 模型优先，为空则回退 TodoWrite 快照
function taskPanelItems() {
  const f = state.taskFold;
  if (!f) return [];
  if (f.order.length) return f.order.map((id) => f.tasks[id]).filter(Boolean);
  return (f.todos || []).filter((t) => t && typeof t.content === 'string').map((t) => ({
    id: '', subject: t.content, activeForm: t.activeForm || '', status: t.status || 'pending',
  }));
}

function taskPanelEl() {
  let el = $('#task-panel');
  if (!el) {
    el = document.createElement('div');
    el.id = 'task-panel';
    el.className = 'task-panel hidden';
    el.innerHTML = '<button type="button" class="tp-head">' +
      '<span class="tp-chevron"><i data-lucide="chevron-down"></i></span>' +
      '<span class="tp-title">任务</span><span class="tp-progress"></span>' +
      '<span class="tp-spin ct-spinner hidden"></span>' +
      '<span class="tp-current"></span>' +
      '</button><div class="tp-list"></div>';
    el.querySelector('.tp-head').onclick = () => {
      state.taskPanelOpen = !state.taskPanelOpen;
      state.taskPanelTouched = true;
      syncTaskPanel();
    };
    renderIcons(el);
  }
  return el;
}

function syncTaskPanel() {
  const el = taskPanelEl();
  const items = state.current ? taskPanelItems() : [];
  if (!items.length) { el.classList.add('hidden'); return; }
  // 固定在输入框卡片顶部、附件行之上（insertBefore 对已有节点是移动）；
  // 待发送 chip 在更上面。与消息流无关，流式事件不会把它顶走。
  const card = $('.composer-card');
  if (card && el.parentElement !== card) card.insertBefore(el, $('#attached-images'));
  el.classList.remove('hidden');
  const done = items.filter((t) => t.status === 'completed').length;
  const allDone = done === items.length;
  el.querySelector('.tp-progress').textContent = `${done}/${items.length}`;
  const cur = items.find((t) => t.status === 'in_progress');
  el.querySelector('.tp-spin').classList.toggle('hidden', !cur);
  const curEl = el.querySelector('.tp-current');
  curEl.textContent = cur ? (cur.activeForm || cur.subject) : (allDone ? '全部完成' : '');
  curEl.title = curEl.textContent;
  // 未手动开合过时自动收展：全部完成收起成一行，来了新任务重新展开
  if (!state.taskPanelTouched) state.taskPanelOpen = !allDone;
  el.classList.toggle('open', state.taskPanelOpen);
  const list = el.querySelector('.tp-list');
  list.innerHTML = '';
  if (!state.taskPanelOpen) return;
  for (const t of items) {
    const row = document.createElement('div');
    // 复用 TodoWrite 的状态着色（todo-check 绿勾/蓝点/空心）
    row.className = 'tp-item todo-item todo-' + (t.status || 'pending');
    const check = document.createElement('span');
    check.className = 'todo-check';
    row.appendChild(check);
    if (t.id) {
      const idEl = document.createElement('span');
      idEl.className = 'tp-id';
      idEl.textContent = '#' + t.id;
      row.appendChild(idEl);
    }
    const text = document.createElement('span');
    text.className = 'todo-text';
    // 进行中的行优先显示 activeForm（"正在做某事"），与 CLI 一致
    text.textContent = (t.status === 'in_progress' && t.activeForm) ? t.activeForm : (t.subject || '(未命名)');
    text.title = text.textContent;
    row.appendChild(text);
    list.appendChild(row);
  }
}

function appendOptimisticUser(text, images, files) {
  const bubble = addMsg('msg-user msg-optimistic', '你', null, new Date());
  if (images && images.length) {
    for (const img of images) {
      const im = document.createElement('img');
      im.className = 'thumb';
      im.src = `data:${img.media_type};base64,${img.data}`;
      im.alt = '';
      bubble.appendChild(im);
    }
  }
  if (files && files.length) {
    for (const f of files) {
      const chip = document.createElement('span');
      chip.className = 'file-chip';
      const ic = document.createElement('i');
      ic.dataset.lucide = 'file';
      chip.appendChild(ic);
      chip.appendChild(document.createTextNode(f.name));
      bubble.appendChild(chip);
    }
  }
  if (text) bubble.appendChild(document.createTextNode((text ? ' ' : '') + text));
  if (bubble.querySelector('i')) renderIcons(bubble);
  scrollBottom();
  refreshPinnedPrompt();
}

$('#btn-stop').onclick = async () => {
  if (state.current) await api(`/api/sessions/${state.current.id}/stop`, { method: 'POST' });
};

async function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) {
    return navigator.clipboard.writeText(text);
  }
  return new Promise((resolve, reject) => {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.style.cssText = 'position:fixed;opacity:0';
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand('copy') ? resolve() : reject(new Error('copy failed')); }
    catch (e) { reject(e); }
    finally { ta.remove(); }
  });
}

async function flashCheck(btn, originalHTML) {
  btn.innerHTML = '<i data-lucide="check"></i>';
  renderIcons(btn);
  setTimeout(() => { btn.innerHTML = originalHTML; renderIcons(btn); }, 1200);
}

$('#btn-resume').onclick = async () => {
  if (!state.current) return;
  const btn = $('#btn-resume');
  const cmd = state.current.resume_command;
  if (!cmd) {
    alert((state.current.engine || 'claude') !== 'claude'
      ? `该会话还没有产生 ${state.current.engine} session（发过首轮消息后可用）`
      : '该会话还没有产生 claude session（发过首轮消息后可用）');
    return;
  }
  try {
    await copyText(cmd);
    flashCheck(btn, originalHTML);
  } catch (_) {
    prompt('复制失败，请手动复制：', cmd);
  }
};

// ---------- header controls ----------
let nameTimer = null;
$('#session-name').addEventListener('input', (e) => {
  clearTimeout(nameTimer);
  const name = e.target.value;
  nameTimer = setTimeout(async () => {
    if (!state.current) return;
    await api(`/api/sessions/${state.current.id}`, {
      method: 'PATCH', body: JSON.stringify({ name }),
    });
    loadSessions();
  }, 600);
});

// 工作区（cwd）编辑：项目目录迁移/改名后把会话指到新路径。服务端会顺带迁移
// claude transcript（--resume / 回放跟随新 slug），失败时回滚输入框显示。
let cwdTimer = null;
$('#session-cwd').addEventListener('input', (e) => {
  clearTimeout(cwdTimer);
  const cwd = e.target.value.trim();
  cwdTimer = setTimeout(async () => {
    if (!state.current || cwd === state.current.cwd) return;
    try {
      const updated = await api(`/api/sessions/${state.current.id}`, {
        method: 'PATCH', body: JSON.stringify({ cwd }),
      });
      state.current = updated;
      $('#session-cwd').value = updated.cwd || '';
      $('#session-cwd').title = updated.cwd || '';
      patchSheetIfOpen(updated);
      applyWorkspaceGate(updated.cwd);
      loadSessions();
      // 迁移后重连：服务端从新 cwd 的 transcript 回放，补出此前因旧路径失效
      // 而缺失的历史消息。
      openSession(state.current.id);
    } catch (err) {
      alert('修改工作区失败：' + err.message);
      $('#session-cwd').value = (state.current && state.current.cwd) || '';
    }
  }, 600);
});

$('#session-cli').addEventListener('change', async (e) => {
  if (!state.current) return;
  const updated = await api(`/api/sessions/${state.current.id}`, {
    method: 'PATCH', body: JSON.stringify({ cli: e.target.value }),
  });
  state.current = updated;
  patchSheetIfOpen(updated);
  loadSessions();
  refreshSessionEffort();
});

$('#session-provider').addEventListener('change', async (e) => {
  if (!state.current) return;
  const updated = await api(`/api/sessions/${state.current.id}`, {
    method: 'PATCH', body: JSON.stringify({ provider: e.target.value }),
  });
  state.current = updated;
  // 服务端会清掉新 provider 里不存在的 model override，以返回值为准重建下拉
  fillModelSelect($('#session-model'), updated.provider, updated.model_override, updated.cli);
  patchSheetIfOpen(updated);
  loadSessions();
  refreshSessionEffort();
});

$('#session-model').addEventListener('change', async (e) => {
  if (!state.current) return;
  const updated = await api(`/api/sessions/${state.current.id}`, {
    method: 'PATCH', body: JSON.stringify({ model: e.target.value }),
  });
  state.current = updated;
  patchSheetIfOpen(updated);
  refreshSessionEffort(); // 模型变了 → opencode 的 variants 键集合可能变
});

$('#session-effort').addEventListener('change', async (e) => {
  if (!state.current) return;
  const updated = await api(`/api/sessions/${state.current.id}`, {
    method: 'PATCH', body: JSON.stringify({ effort: e.target.value }),
  });
  state.current = updated;
  patchSheetIfOpen(updated);
});

$('#session-perm').addEventListener('change', async (e) => {
  if (!state.current) return;
  const updated = await api(`/api/sessions/${state.current.id}`, {
    method: 'PATCH', body: JSON.stringify({ permission_mode: e.target.value }),
  });
  state.current = updated;
  patchSheetIfOpen(updated);
});

$('#btn-delete').onclick = async () => {
  if (!state.current) return;
  if (!confirm(`删除会话「${state.current.name}」？历史事件将被清除。`)) return;
  await api(`/api/sessions/${state.current.id}`, { method: 'DELETE' });
  if (state.es) state.es.close();
  state.current = null;
  $('#chat-view').classList.add('hidden');
  $('#empty-state').classList.remove('hidden');
  loadSessions();
};

function setSidebarCollapsed(collapsed) {
  document.body.classList.toggle('sidebar-collapsed', collapsed);
}
$('#btn-collapse').onclick = () => setSidebarCollapsed(true);
$('#btn-expand').onclick = () => setSidebarCollapsed(false);

// ---------- sidebar drag-to-resize ----------
(function initSidebarResize() {
  const splitter = $('#sidebar-splitter');
  const sidebar = $('#sidebar');
  if (!splitter || !sidebar) return;
  const KEY = 'cc-webui-sidebar-width';
  const MIN = 220, MAX = 480;
  const saved = parseInt(localStorage.getItem(KEY), 10);
  if (saved >= MIN && saved <= MAX) {
    sidebar.style.width = saved + 'px';
    sidebar.style.minWidth = saved + 'px';
  }
  let dragging = false, startX = 0, startW = 0;
  splitter.addEventListener('pointerdown', (e) => {
    if (document.body.classList.contains('sidebar-collapsed')) return;
    dragging = true;
    startX = e.clientX;
    startW = sidebar.getBoundingClientRect().width;
    splitter.setPointerCapture(e.pointerId);
    splitter.classList.add('dragging');
    sidebar.style.transition = 'none';
    document.body.style.cursor = 'col-resize';
    document.body.style.userSelect = 'none';
  });
  splitter.addEventListener('pointermove', (e) => {
    if (!dragging) return;
    const w = Math.min(MAX, Math.max(MIN, startW + e.clientX - startX));
    sidebar.style.width = w + 'px';
    sidebar.style.minWidth = w + 'px';
  });
  const end = () => {
    if (!dragging) return;
    dragging = false;
    splitter.classList.remove('dragging');
    sidebar.style.transition = '';
    document.body.style.cursor = '';
    document.body.style.userSelect = '';
    const w = Math.round(parseFloat(sidebar.style.width));
    if (w >= MIN && w <= MAX) localStorage.setItem(KEY, w);
  };
  splitter.addEventListener('pointerup', end);
  splitter.addEventListener('pointercancel', end);
})();

// ---------- modals ----------
function openModal(sel) { $(sel).classList.remove('hidden'); }
function closeModal(el) { el.closest('.modal').classList.add('hidden'); }
document.querySelectorAll('[data-close]').forEach((b) => {
  b.onclick = () => closeModal(b);
});
document.querySelectorAll('.modal').forEach((m) => {
  m.addEventListener('click', (e) => { if (e.target === m) m.classList.add('hidden'); });
});

// ---------- 设置 modal（通知开关，存 localStorage，见上方通知模块） ----------
function notifyPermText() {
  if (!('Notification' in window)) return '当前浏览器不支持系统通知';
  if (Notification.permission === 'granted') return '浏览器已授权';
  if (Notification.permission === 'denied') return '已被浏览器拒绝，请在地址栏的站点设置中允许通知';
  return '未授权，打开开关时将请求浏览器权限';
}

$('#btn-settings').onclick = () => {
  const prefs = notifyPrefs();
  $('#pref-banner').checked = prefs.banner;
  $('#pref-sound').checked = prefs.sound;
  $('#notify-perm-hint').textContent = notifyPermText();
  openModal('#modal-settings');
};

$('#pref-banner').addEventListener('change', async (e) => {
  const prefs = notifyPrefs();
  prefs.banner = e.target.checked;
  saveNotifyPrefs(prefs);
  // 打开开关顺手请求权限（change 由点击触发，满足手势要求）
  if (prefs.banner && 'Notification' in window && Notification.permission === 'default') {
    try { await Notification.requestPermission(); } catch (_) {}
  }
  $('#notify-perm-hint').textContent = notifyPermText();
});

$('#pref-sound').addEventListener('change', (e) => {
  const prefs = notifyPrefs();
  prefs.sound = e.target.checked;
  saveNotifyPrefs(prefs);
  if (prefs.sound) playNotifySound(false); // 打开时试听一声，顺带解锁 AudioContext
});

$('#btn-test-sound').onclick = () => playNotifySound(false);

// ---------- 待办备忘（服务端持久化，重启不丢） ----------
// 全局便签：整列表 PUT /api/notes 落盘 data/notes.json。前端为权威源，
// 每次增删改后整表提交（ack 即可，不回填响应）；打开弹窗时 GET 一次回显。
// ID 由前端生成；保存串行化——在途请求期间的新改动合并为一次后续提交，
// 避免并发 PUT 对 notes.json 的 lost-update。
let notes = [];
let notesSaving = false;
let notesDirty = false;
let notesFlashTimer = null;

function newNoteId() {
  if (window.crypto && crypto.randomUUID) return crypto.randomUUID();
  return Date.now().toString(36) + Math.random().toString(36).slice(2);
}

$('#btn-notes').onclick = () => { openModal('#modal-notes'); loadNotes(); };

async function loadNotes() {
  try {
    notes = await api('/api/notes');
  } catch (e) {
    notes = [];
    notesFlash('加载失败：' + e.message);
  }
  renderNotes();
  $('#notes-new').focus();
}

function renderNotes() {
  const list = $('#notes-list');
  list.innerHTML = '';
  $('#notes-empty').classList.toggle('hidden', notes.length > 0);
  for (const n of notes) {
    const row = document.createElement('div');
    row.className = 'note-row' + (n.done ? ' done' : '');

    const check = document.createElement('button');
    check.className = 'note-check';
    check.title = n.done ? '标记为未完成' : '标记为已完成';
    check.innerHTML = n.done
      ? '<i data-lucide="circle-check"></i>'
      : '<i data-lucide="circle"></i>';
    check.onclick = () => { n.done = !n.done; saveNotes(); };

    const text = document.createElement('span');
    text.className = 'note-text';
    text.textContent = n.text;

    const del = document.createElement('button');
    del.className = 'note-del';
    del.title = '删除';
    del.innerHTML = '<i data-lucide="x"></i>';
    del.onclick = () => {
      notes = notes.filter(x => x.id !== n.id);
      saveNotes();
    };

    row.append(check, text, del);
    list.append(row);
  }
  renderIcons(list);
}

// saveNotes 先即时重渲染反映本地改动，再串行化提交：在途时仅置 dirty，
// 其 finally 触发一次携带最新状态的后续提交。
async function saveNotes() {
  renderNotes();
  if (notesSaving) { notesDirty = true; return; }
  notesSaving = true;
  try {
    await api('/api/notes', { method: 'PUT', body: JSON.stringify(notes) });
    notesFlash('已保存', true);
  } catch (e) {
    notesFlash('保存失败：' + e.message);
  } finally {
    notesSaving = false;
    if (notesDirty) { notesDirty = false; saveNotes(); }
  }
}

function notesFlash(msg, ok) {
  const el = $('#notes-status');
  el.textContent = msg;
  el.classList.toggle('ok', !!ok);
  el.classList.remove('hidden');
  clearTimeout(notesFlashTimer);
  notesFlashTimer = setTimeout(() => el.classList.add('hidden'), 1800);
}

function addNote() {
  const input = $('#notes-new');
  const text = input.value.trim();
  if (!text) return;
  notes.unshift({ id: newNoteId(), text, done: false });
  input.value = '';
  saveNotes();
  input.focus();
}

$('#notes-add').onclick = addNote;
$('#notes-new').addEventListener('keydown', (e) => {
  if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); addNote(); }
});
$('#notes-clear').onclick = () => {
  if (!notes.some(n => n.done)) return;
  notes = notes.filter(n => !n.done);
  saveNotes();
};

// ---------- 使用统计弹窗 ----------
// 数据来自 GET /api/stats。服务端增量聚合：仅重扫 mtime/size 变了的会话文件，
// 其余复用逐文件缓存再合并，打开弹窗通常只重扫当前活跃会话一个文件。
// 前端再叠加 stale-while-revalidate：有旧数据先秒渲染、后台静默刷新。
// 热力图口径移植自 cc-haha ActivitySettings：评分 score = 当日会话数*3 +
// ceil(tokens/50000)，按全期 max 分 5 档。
const HEAT_COLS = 52; // 周列数（GitHub 贡献图口径：一年）
const HEAT_CELL = 10; // 格子边长 px
const HEAT_GAP = 3;

// 每天的聚合数据（hover/click tooltip 用），renderHeatmap 时重建
let heatByDate = new Map();
let heatPinned = null; // 点击固定的日期
let lastStats = null; // 上次拉取的统计，打开弹窗时即时渲染（stale-while-revalidate）

function fmtTokens(n) {
  if (n >= 1e9) return (n / 1e9).toFixed(n >= 1e10 ? 0 : 1) + 'B';
  if (n >= 1e6) return (n / 1e6).toFixed(n >= 1e7 ? 0 : 1) + 'M';
  if (n >= 1e3) return Math.round(n / 1e3) + 'K';
  return String(n);
}

function fmtInt(n) { return Number(n).toLocaleString('zh-CN'); }

function dayKey(d) {
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`;
}

$('#btn-stats').onclick = () => { openModal('#modal-stats'); loadStats(false); };
$('#stats-refresh').onclick = () => loadStats(true);

async function loadStats(fresh) {
  const loading = $('#stats-loading');
  const body = $('#stats-body');
  const btn = $('#stats-refresh');
  const hasOld = !!lastStats;
  if (hasOld) {
    // 已有数据：先即时渲染旧数据，再后台静默刷新（fresh 整页重扫可能稍慢）
    loading.classList.add('hidden');
    body.classList.remove('hidden');
    renderStats(lastStats);
  } else {
    loading.textContent = '统计中…（首次扫描较慢）';
    loading.classList.remove('hidden');
    body.classList.add('hidden');
  }
  if (fresh) { btn.disabled = true; btn.textContent = '刷新中…'; }
  let stats;
  try {
    stats = await api('/api/stats' + (fresh ? '?fresh=1' : ''));
  } catch (e) {
    if (!hasOld) loading.textContent = '统计失败：' + e.message;
    if (fresh) { btn.disabled = false; btn.textContent = '刷新'; }
    return; // 有旧数据则保持显示，不弹错误
  }
  lastStats = stats;
  if (fresh) { btn.disabled = false; btn.textContent = '刷新'; }
  // 弹窗已关闭则不重渲染（避免无谓 DOM 操作）
  if (!$('#modal-stats').classList.contains('hidden')) {
    loading.classList.add('hidden');
    body.classList.remove('hidden');
    renderStats(stats);
  }
}

function renderStats(stats) {
  // 统计口径说明：按引擎拆出各自贡献的会话数（四引擎合并统计）
  const engines = stats.engines || {};
  const scopeParts = [];
  if (engines.claude) scopeParts.push(`Claude Code ${fmtInt(engines.claude)} 个（~/.claude/projects）`);
  if (engines.opencode) scopeParts.push(`opencode ${fmtInt(engines.opencode)} 个`);
  if (engines.mcode) scopeParts.push(`mcode ${fmtInt(engines.mcode)} 个`);
  if (engines.qoder) scopeParts.push(`qoder ${fmtInt(engines.qoder)} 个（~/.qoder-cn/projects）`);
  if (engines.codex) scopeParts.push(`codex ${fmtInt(engines.codex)} 个`);
  $('#stats-scope').textContent = scopeParts.join(' + ') || (stats.total_sessions ? '本机全部会话' : '无');
  const daily = stats.daily_activity || [];
  const totalTokens = (stats.daily_model_tokens || []).reduce(
    (s, d) => s + Object.values(d.tokens_by_model).reduce((a, b) => a + b, 0), 0);
  const peakHour = stats.peak_activity_hour == null ? '—' : `${stats.peak_activity_hour}:00`;
  const tile = (v, l) => `<div class="stat-tile"><div class="v">${v}</div><div class="l">${l}</div></div>`;
  $('#stats-tiles').innerHTML = [
    tile(fmtInt(stats.total_sessions), '会话'),
    tile(fmtInt(stats.total_messages), '消息'),
    tile(fmtTokens(totalTokens), '总 tokens'),
    tile(`${fmtInt(stats.active_days)} / ${fmtInt(stats.total_days)}`, '活跃天数'),
    tile(`${fmtInt(stats.streaks.current)} 天`, '当前连续'),
    tile(`${fmtInt(stats.streaks.longest)} 天`, '最长连续'),
    tile(peakHour, '峰值时段'),
    tile(stats.favorite_model || '—', '常用模型'),
  ].join('');
  // renderHeatmap 返回"近 30 天"那段尾部；这里一次性拼好热力图脚注
  // （旧写法先由 renderHeatmap 写入、再被这里整体覆盖，近 30 天信息永远丢失）
  const toolCalls = fmtInt(daily.reduce((s, d) => s + d.tool_call_count, 0));
  const heatTail = renderHeatmap(stats);
  $('#heat-detail').textContent = `工具调用共 ${toolCalls} 次 · ${heatTail}`;
}

function renderHeatmap(stats) {
  const act = new Map((stats.daily_activity || []).map((d) => [d.date, d]));
  const tok = new Map((stats.daily_model_tokens || []).map((d) => [
    d.date, Object.values(d.tokens_by_model).reduce((a, b) => a + b, 0)]));
  const today = new Date(); today.setHours(0, 0, 0, 0);
  const weekStart = new Date(today); weekStart.setDate(weekStart.getDate() - weekStart.getDay()); // 本周日
  const start = new Date(weekStart); start.setDate(start.getDate() - (HEAT_COLS - 1) * 7);
  const dates = [];
  for (let d = new Date(start); d <= today; d = new Date(d.getFullYear(), d.getMonth(), d.getDate() + 1)) {
    dates.push(new Date(d));
  }
  const scores = dates.map((d) => {
    const a = act.get(dayKey(d));
    if (!a && !tok.has(dayKey(d))) return 0;
    return ((a ? a.session_count : 0) * 3) + Math.ceil((tok.get(dayKey(d)) || 0) / 50000);
  });
  const maxScore = Math.max(0, ...scores);
  const level = (s) => s <= 0 ? 0 : maxScore <= 0 ? 1
    : s / maxScore >= 0.78 ? 4 : s / maxScore >= 0.5 ? 3 : s / maxScore >= 0.24 ? 2 : 1;
  // tooltip 数据：每天会话/消息/工具/tokens/模型构成
  const tokByModel = new Map((stats.daily_model_tokens || []).map((d) => [d.date, d.tokens_by_model]));
  heatByDate = new Map(dates.map((d) => {
    const key = dayKey(d);
    const a = act.get(key) || null;
    const tm = tokByModel.get(key);
    return [key, {
      a,
      tokens: tok.get(key) || 0,
      models: tm ? Object.entries(tm).sort((x, y) => y[1] - x[1]).slice(0, 3) : [],
    }];
  }));
  const cells = dates.map((d, i) =>
    `<div class="heat-cell lv${level(scores[i])}" data-date="${dayKey(d)}"></div>`).join('');
  $('#heatmap').innerHTML = `
    <div class="heat-days">
      <span class="heat-day-label" style="grid-row:2">周一</span>
      <span class="heat-day-label" style="grid-row:4">周三</span>
      <span class="heat-day-label" style="grid-row:6">周五</span>
    </div>
    <div class="heat-grid">${cells}</div>`;
  // 月份标签：每列（周）首日所在月份变化时标一个
  const labels = [];
  let prevMonth = -1;
  for (let col = 0; col < HEAT_COLS; col++) {
    const d = new Date(start.getFullYear(), start.getMonth(), start.getDate() + col * 7);
    if (d > today) break;
    if (d.getMonth() !== prevMonth) {
      labels.push(`<span class="heat-month-label" style="left:${col * (HEAT_CELL + HEAT_GAP)}px">${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}</span>`);
      prevMonth = d.getMonth();
    }
  }
  $('#heat-months').innerHTML = labels.join('');
  const l30 = dates.slice(-30).filter((d) => act.has(dayKey(d)) || tok.has(dayKey(d)));
  $('#heat-legend').innerHTML = `少 ${[0, 1, 2, 3, 4].map((i) => `<span class="heat-cell lv${i}"></span>`).join('')} 多`;
  // 返回"近 30 天"尾部，由 renderStats 一次性写入 #heat-detail（避免读写顺序丢信息）
  return `近 30 天：${fmtInt(l30.length)} 天活跃，${fmtTokens(l30.reduce((s, d) => s + (tok.get(dayKey(d)) || 0), 0))} tokens`;
}

// ---------- 热力图 tooltip：hover 跟随，点击固定（再点同一天取消） ----------
function heatTipHtml(date) {
  const d = heatByDate.get(date);
  const head = `<div class="ht-date">${date}</div>`;
  if (!d || (!d.a && d.tokens === 0)) return head + '<div class="ht-empty">这一天没有活动</div>';
  const rows = [];
  if (d.a) {
    rows.push(`<div class="ht-row"><span>会话</span><span class="v">${d.a.session_count}</span></div>`);
    rows.push(`<div class="ht-row"><span>消息</span><span class="v">${fmtInt(d.a.message_count)}</span></div>`);
    rows.push(`<div class="ht-row"><span>工具调用</span><span class="v">${fmtInt(d.a.tool_call_count)}</span></div>`);
  }
  rows.push(`<div class="ht-row"><span>tokens</span><span class="v">${fmtTokens(d.tokens)}</span></div>`);
  for (const [model, n] of d.models) {
    rows.push(`<div class="ht-row"><span class="ht-model" title="${escHtml(model)}">${escHtml(model)}</span><span class="v">${fmtTokens(n)}</span></div>`);
  }
  return head + rows.join('');
}

function showHeatTip(cell) {
  const tip = $('#heat-tooltip');
  const wrap = document.querySelector('.stats-heat');
  if (!tip || !wrap) return;
  tip.innerHTML = heatTipHtml(cell.dataset.date);
  tip.classList.remove('hidden');
  const w = wrap.getBoundingClientRect();
  const r = cell.getBoundingClientRect();
  const tw = tip.offsetWidth || 170;
  const th = tip.offsetHeight || 90;
  let left = r.left - w.left + 14;                        // 默认贴右侧
  if (left + tw > w.width - 4) left = r.left - w.left - tw - 6; // 越界翻到左侧
  let top = r.top - w.top - th - 6;                       // 默认放上方
  if (top < 2) top = r.top - w.top + 16;                  // 上方没空间放下方
  tip.style.left = Math.max(2, left) + 'px';
  tip.style.top = Math.max(2, top) + 'px';
}

function hideHeatTip() {
  heatPinned = null;
  $('#heat-tooltip').classList.add('hidden');
}

$('#heatmap').addEventListener('mouseover', (e) => {
  const cell = e.target.closest ? e.target.closest('.heat-cell') : null;
  if (!cell || heatPinned) return; // 点击固定后 hover 不打扰
  showHeatTip(cell);
});
$('#heatmap').addEventListener('mouseleave', () => {
  if (!heatPinned) $('#heat-tooltip').classList.add('hidden');
});
$('#heatmap').addEventListener('click', (e) => {
  const cell = e.target.closest ? e.target.closest('.heat-cell') : null;
  if (!cell) return;
  if (heatPinned === cell.dataset.date) { hideHeatTip(); return; }
  heatPinned = cell.dataset.date;
  showHeatTip(cell);
});

$('#btn-new').onclick = async () => {
  const firstCli = state.clis[0] && state.clis[0].name;
  fillCLISelect($('#new-cli'), firstCli);
  const k0 = cliKind(firstCli);
  fillProviderSelect($('#new-provider'), '', k0);
  fillModelSelect($('#new-model'), $('#new-provider').value, '', $('#new-cli').value);
  applyNewEngineUI(k0);
  const cfg = await api('/api/config');
  $('#new-cwd').placeholder = cfg.default_cwd || '/path/to/project';
  openModal('#modal-new');
};

// quick-create 弹窗：clis 列表本就带 kind 后缀（fillCLISelect 标签），切 cli
// 时 provider 归位同 kind 首个；effort 下拉由 refreshNewEffort 按模型 variants
// 动态填充（原「opencode 无此概念」的固定隐藏不再适用）
function applyNewEngineUI(kind) {
  refreshNewEffort();
}

$('#new-cli').addEventListener('change', () => {
  const kind = cliKind($('#new-cli').value);
  fillProviderSelect($('#new-provider'), '', kind);
  fillModelSelect($('#new-model'), $('#new-provider').value, '', $('#new-cli').value);
  applyNewEngineUI(kind);
});

$('#new-provider').addEventListener('change', (e) => {
  // cli 名必须传下去：mcode / qoder 的模型列表来自 CLI 侧目录（按 cli+provider
  // 拉取），漏了它就会退回 config 的静态 models——对这两个引擎恒为空。
  fillModelSelect($('#new-model'), e.target.value, '', $('#new-cli').value);
  refreshNewEffort();
});

$('#new-model').addEventListener('change', refreshNewEffort);

$('#new-submit').onclick = async () => {
  const body = {
    name: $('#new-name').value.trim(),
    cli: $('#new-cli').value,
    provider: $('#new-provider').value,
    cwd: $('#new-cwd').value.trim(),
    model: $('#new-model').value,
    effort: $('#new-effort').value,
    permission_mode: $('#new-perm').value,
  };
  try {
    const s = await api('/api/sessions', { method: 'POST', body: JSON.stringify(body) });
    $('#modal-new').classList.add('hidden');
    $('#new-name').value = ''; $('#new-cwd').value = '';
    await loadSessions();
    openSession(s.id);
  } catch (e) {
    alert('新建会话失败：' + e.message);
  }
};

// 各引擎共用一套列表/预览 UI，源由 #import-cli 的 kind 决定；顶部页签是
// 快捷切换（把 import-cli 指到该 kind 的第一个 cli 并重扫）。
const IMPORT_HINTS = {
  claude: 'SSH 里找 session id：<code>ls -t ~/.claude/projects/*/*.jsonl | head</code>（文件名 <code>~/.claude/projects/&lt;cwd-slug&gt;/&lt;session-id&gt;.jsonl</code>）',
  opencode: 'opencode 会话存于 opencode 自己的存储（<code>opencode session list</code>）。导入时会话按导出里的模型对齐到所选 provider。',
  mcode: 'mcode 会话存于它自己的运行时数据目录（<code>~/.minimax/v2/sessions/</code>，每个会话一个日期目录，session id 是 <code>mvs_</code> 开头的串）。导入时按 transcript 里的模型对齐到所选 provider；工作区需在下一步自行填写。',
  qoder: 'qoder 会话存于 <code>~/.qoder-cn/projects/&lt;cwd-slug&gt;/&lt;session-id&gt;.jsonl</code>（与 Claude Code 同构，只是 home 不同）。也可以 <code>qoderclicn --list-sessions</code> 看当前项目的会话。',
  codex: 'codex 会话存于 <code>$CODEX_HOME/sessions/YYYY/MM/DD/rollout-*-&lt;session-id&gt;.jsonl</code>（ducx 的 home 是 <code>~/.baidu-cx</code>）。导入时按 transcript 里的模型对齐到所选 provider。',
};

function syncImportTabs(kind) {
  $('#import-hint').innerHTML = IMPORT_HINTS[kind] || '';
  document.querySelectorAll('#import-tabs .import-tab').forEach((t) => {
    t.classList.toggle('active', t.dataset.kind === kind);
  });
}

$('#import-tabs').addEventListener('click', (e) => {
  const tab = e.target.closest('.import-tab');
  if (!tab || tab.classList.contains('hidden') || tab.classList.contains('active')) return;
  const cli = state.clis.find((c) => kindOf(c) === tab.dataset.kind);
  if (!cli) return;
  $('#import-cli').value = cli.name;
  $('#import-cli').dispatchEvent(new Event('change'));
});

$('#btn-import').onclick = () => {
  // 页签只显示有对应 kind cli 的引擎
  document.querySelectorAll('#import-tabs .import-tab').forEach((t) => {
    const has = state.clis.some((c) => kindOf(c) === t.dataset.kind);
    t.classList.toggle('hidden', !has);
  });
  const cli = state.clis[0];
  fillCLISelect($('#import-cli'), cli && cli.name);
  fillProviderSelect($('#import-provider'), '', cli && kindOf(cli));
  state.importList = [];
  $('#import-cwd').value = '';
  $('#import-prefix').value = '';
  syncImportTabs(cli && kindOf(cli));
  openModal('#modal-import');
  scanImport();
};

$('#import-scan').onclick = scanImport;
$('#import-prefix').addEventListener('input', renderImportList);
$('#import-cwd').addEventListener('input', renderImportList);
// 切 cli（含页签驱动）→ provider 归位到同 kind 首个 + 重扫
$('#import-cli').addEventListener('change', () => {
  const kind = cliKind($('#import-cli').value);
  syncImportTabs(kind);
  fillProviderSelect($('#import-provider'), '', kind);
  scanImport();
});

async function scanImport() {
  const el = $('#import-list');
  el.innerHTML = '<div style="color:var(--dim);padding:20px;text-align:center">扫描中...</div>';
  const cliSel = $('#import-cli');
  const kind = cliKind(cliSel.value);
  state.importKind = kind;
  const cwd = $('#import-cwd').value.trim();
  const prefix = $('#import-prefix').value.trim();
  // mcode 的会话目录不记录工作目录（那份信息只在它的运行时 sqlite 里，而本项目
  // 刻意不链接 sqlite 驱动），所以这个输入框对 mcode 从"过滤"变成"导入时使用"，
  // 必须让用户看见并填。
  $('#import-cwd').placeholder = kind === 'mcode'
    ? '工作目录（mcode 不记录会话目录，导入时按此设置）'
    : (kind === 'opencode' ? '工作目录过滤（留空 = 全部项目）' : '工作目录过滤');
  const params = new URLSearchParams();
  if (kind === 'opencode' || kind === 'mcode' || kind === 'codex') {
    // opencode / mcode / codex 的会话源在各自 CLI 自己的存储里，按 cli 查询
    params.set('cli', cliSel.value);
  }
  // claude / qoder / opencode / codex 都记录会话目录，cwd 是过滤条件；
  // 留空时服务端列全部项目。
  if (cwd && (kind === 'claude' || kind === 'qoder' || kind === 'opencode' || kind === 'codex')) params.set('cwd', cwd);
  if (prefix) params.set('prefix', prefix);
  const url = (kind === 'claude'
      ? '/api/claude-sessions'
      : (kind === 'mcode' ? '/api/mcode-sessions'
        : (kind === 'qoder' ? '/api/qoder-sessions'
          : (kind === 'codex' ? '/api/codex-sessions' : '/api/opencode-sessions'))))
    + '?' + params.toString();
  try {
    state.importList = await api(url);
  } catch (e) {
    el.innerHTML = '<div style="color:var(--red);padding:20px">' + e.message + '</div>';
    return;
  }
  renderImportList();
}

function renderImportList() {
  const cwd = $('#import-cwd').value.trim();
  const prefix = $('#import-prefix').value.trim().toLowerCase();
  // 客户端二次筛选（扫描时 cwd 已传给服务端，但 prefix 实时生效更顺手）
  let list = state.importList || [];
  if (cwd && list.length && !list[0].cwd) { /* 服务端 cwd 已过滤；这里不重复 */ }
  if (prefix) {
    list = list.filter((it) => it.session_id.toLowerCase().startsWith(prefix));
  }
  const el = $('#import-list');
  el.innerHTML = '';
  if (!list.length) {
    el.innerHTML = '<div style="color:var(--dim);padding:20px;text-align:center">没有匹配的会话</div>';
    return;
  }
  for (const item of list) {
    const row = document.createElement('div');
    row.className = 'import-item';
    row.innerHTML = `
      <div class="info">
        <div class="t"></div>
        <div class="m"></div>
      </div>
      <button class="ghost-btn copy-id" title="复制完整 session id"><i data-lucide="copy"></i></button>
      <button class="primary-btn import-btn">导入</button>`;
    row.querySelector('.t').textContent = item.title || '(untitled)';
    row.querySelector('.m').textContent = `${item.session_id.slice(0, 8)} · ${item.cwd || '?'} · ${timeAgo(item.updated_at)}`;
    row.onclick = (e) => {
      if (e.target.closest('button')) return;
      selectImportPreview(item, row);
    };
    row.querySelector('.copy-id').onclick = async (e) => {
      const btn = e.target.closest('button') || e.target;
      try {
        await copyText(item.session_id);
        flashCheck(btn, btn.innerHTML);
      } catch (_) {
        prompt('复制失败，请手动复制：', item.session_id);
      }
    };
    row.querySelector('.import-btn').onclick = async () => {
      if (state.importKind === 'mcode' && !cwd) {
        alert('mcode 会话不记录创建时的目录，请先在上方填写该会话的工作目录再导入。');
        $('#import-cwd').focus();
        return;
      }
      try {
        const s = await api('/api/sessions/import', {
          method: 'POST',
          body: JSON.stringify({
            claude_session_id: item.session_id,
            cwd: item.cwd || cwd,
            cli: $('#import-cli').value,
            provider: $('#import-provider').value,
            name: item.title.slice(0, 60),
          }),
        });
        $('#modal-import').classList.add('hidden');
        await loadSessions();
        openSession(s.id);
      } catch (e) {
        alert('导入失败：' + e.message);
      }
    };
    el.appendChild(row);
  }
  renderIcons(el);
}

// ---------- 导入预览（右侧面板）：选中列表行后拉取该会话最近消息 ----------
const importPreviewCache = new Map(); // key: session_id|updated_at（文件 mtime 变化即过期）

async function selectImportPreview(item, row) {
  document.querySelectorAll('.import-item.sel').forEach((el) => el.classList.remove('sel'));
  row.classList.add('sel');
  const pane = $('#import-preview');
  const cacheKey = item.session_id + '|' + item.updated_at;
  let p = importPreviewCache.get(cacheKey);
  if (!p) {
    pane.innerHTML = '<div class="ip-empty">加载中…</div>';
    const params = new URLSearchParams({ id: item.session_id });
    const kind = state.importKind;
    if (kind === 'opencode' || kind === 'mcode' || kind === 'codex') {
      params.set('cli', $('#import-cli').value);
    } else if (item.cwd) {
      params.set('cwd', item.cwd);
    }
    try {
      const base = kind === 'claude' ? 'claude-sessions'
        : (kind === 'mcode' ? 'mcode-sessions'
          : (kind === 'qoder' ? 'qoder-sessions'
            : (kind === 'codex' ? 'codex-sessions' : 'opencode-sessions')));
      p = await api('/api/' + base + '/preview?' + params.toString());
    } catch (e) {
      pane.innerHTML = '<div class="ip-empty">加载失败：' + e.message + '</div>';
      return;
    }
    importPreviewCache.set(cacheKey, p);
    if (importPreviewCache.size > 50) importPreviewCache.delete(importPreviewCache.keys().next().value);
  }
  pane.innerHTML = renderImportPreview(p, item.title);
  renderIcons(pane);
}

function escHtml(s) {
  return String(s).replace(/[&<>"']/g, (c) => (
    { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]
  ));
}

function renderImportPreview(p, title) {
  const msgs = (p.messages || []).map((m) => `
    <div class="ip-msg">
      <div class="ip-msg-head">
        <span class="role-badge ${m.role === 'user' ? 'user' : 'asst'}">${m.role === 'user' ? '用户' : '助手'}</span>
        <span class="ip-msg-ts">${m.ts ? formatMsgTime(m.ts).slice(0, 16) : ''}</span>
      </div>
      <div class="ip-msg-text">${escHtml(m.text)}</div>
    </div>`).join('');
  return `
    <div class="ip-title">${escHtml(title || '(empty)')}</div>
    <div class="ip-meta">
      <span class="ip-cwd" title="${escHtml(p.cwd || '')}">${escHtml(p.cwd || '?')}</span>
      ${p.branch ? `<span class="ip-branch"><i data-lucide="git-branch"></i>${escHtml(p.branch)}</span>` : ''}
      <span class="ip-active">最后活跃 ${timeAgo(p.updated_at)}</span>
    </div>
    <div class="ip-where">离开时</div>
    ${msgs || '<div class="ip-empty">（无消息）</div>'}`;
}

// ---------- new-session composer view ----------
let nsPendingCwd = '';

async function openNewSessionView(cwd) {
  nsPendingCwd = cwd || '';
  $('#chat-view').classList.add('hidden');
  $('#group-view').classList.add('hidden');
  relocatePermCards();
  $('#empty-state').classList.add('hidden');
  $('#new-session-view').classList.remove('hidden');
  $('#ns-cwd-display').textContent = cwd ? `工作区: ${cwd}` : '';
  const inp = $('#ns-input');
  inp.value = '';
  inp.style.height = 'auto';
  state.nsAttached = [];
  renderNsAttached();
  inp.focus();

  const cliSel = $('#ns-cli');
  const providerSel = $('#ns-provider');
  const modelSel = $('#ns-model');
  cliSel.innerHTML = state.clis.map((c) => `<option value="${c.name}">${c.name}${kindOf(c) === 'claude' ? '' : ' · ' + c.kind}</option>`).join('');
  if (state.clis.length) cliSel.value = state.clis[0].name;
  const k0 = cliKind(cliSel.value);
  renderProviderOptions(providerSel, k0);
  renderModelOptions(modelSel, providerSel.value);
  applyNsEngineUI(k0);
  // cli 决定引擎 → provider 列表、推理等级控件、已附图片随之联动
  cliSel.onchange = () => {
    const kind = cliKind(cliSel.value);
    renderProviderOptions(providerSel, kind);
    renderModelOptions(modelSel, providerSel.value);
    applyNsEngineUI(kind);
    if (!engineTakesImages(kind)) dropNsImages(`已切换到 ${kind} 引擎，暂不支持图片消息，已移除图片附件`);
  };
  providerSel.onchange = () => {
    renderModelOptions(modelSel, providerSel.value);
    refreshNsEffort(); // provider/模型变了 → variants 键集合可能变
  };
  modelSel.onchange = () => {
    refreshNsEffort(); // 模型变了 → variants 键集合可能变
  };
}

function renderProviderOptions(sel, kind) {
  const items = kind ? state.providers.filter((p) => kindOf(p) === kind) : state.providers;
  sel.innerHTML = items.map((p) => `<option value="${p.name}">${p.name}</option>`).join('');
  if (items.length) sel.value = items[0].name;
}

// 新建会话视图的引擎可见面：effort 下拉由 refreshNsEffort 按所选模型
// variants 动态填充（opencode 无 variants 的模型自然隐藏）
function applyNsEngineUI(kind) {
  refreshNsEffort();
}

function dropNsImages(msg) {
  if (!(state.nsAttached || []).some((a) => a.kind === 'image')) return;
  state.nsAttached = state.nsAttached.filter((a) => a.kind !== 'image');
  renderNsAttached();
  alert(msg);
}
function renderModelOptions(sel, providerName) {
  // opencode / mcode / qoder 的模型不在 config 里静态声明（可用集合由 CLI 自己
  // 决定），异步拉目录后重建；dataset.provider 防止连点切换时旧请求覆盖新结果。
  const kind = provKind(providerName);
  if (kind === 'opencode' || kind === 'mcode' || kind === 'qoder' || kind === 'codex') {
    const cliName = $('#ns-cli').value;
    sel.dataset.provider = providerName;
    modelsForProvider(providerName, cliName).then((models) => {
      if (sel.dataset.provider !== providerName) return;
      sel.innerHTML = models.map((m) => `<option value="${m.name}">${m.name}</option>`).join('');
      if (models.length) sel.value = models[0].name;
      // 目录是异步到的：模型真正落定后重建一次档位下拉，否则首屏会
      // 因为 model.value 还是空而把 reasoning 控件整个隐藏。
      refreshNsEffort();
    });
    return;
  }
  const models = providerModels(providerName);
  sel.innerHTML = models.map((m) => `<option value="${m.name}">${m.name}</option>`).join('');
  if (models.length) sel.value = models[0].name;
}

function renderNsAttached() {
  const box = $('#ns-attached');
  box.innerHTML = '';
  box.classList.toggle('hidden', !state.nsAttached || state.nsAttached.length === 0);
  (state.nsAttached || []).forEach((att, idx) => {
    const t = document.createElement('div');
    if (att.kind === 'file') {
      t.className = 'attached-thumb attached-file';
      t.innerHTML = `<i data-lucide="file" class="af-icon"></i><span class="af-name"></span><span class="af-size"></span><button class="remove" title="移除">×</button>`;
      t.querySelector('.af-name').textContent = att.name;
      t.querySelector('.af-name').title = att.name;
      t.querySelector('.af-size').textContent = fmtBytes(att.size);
    } else {
      t.className = 'thumb';
      t.innerHTML = `<img src="${att.data}"><button class="remove">×</button>`;
    }
    t.querySelector('.remove').onclick = () => {
      state.nsAttached.splice(idx, 1);
      renderNsAttached();
    };
    box.appendChild(t);
  });
  renderIcons(box);
}

async function nsSend() {
  const content = $('#ns-input').value.trim();
  if (!content && !(state.nsAttached && state.nsAttached.length)) return;
  const cli = $('#ns-cli').value;
  const provider = $('#ns-provider').value;
  const model = $('#ns-model').value;
  const effort = $('#ns-effort').value;
  const perm = $('#ns-perm').value;
  if (!cli || !provider || !model) { alert('请先选择配置'); return; }

  // Backend runner.Image expects {media_type, data} with data being raw
  // base64 (no "data:...;base64," prefix). nsAttached keeps the full dataURL
  // for thumbnails, so normalize here.
  const images = (state.nsAttached || []).filter((a) => a.kind === 'image').map((img) => ({
    media_type: img.mime,
    data: stripDataURLPrefix(img.data),
  }));
  if (images.length && !engineTakesImages(cliKind(cli))) {
    alert(`${cliKind(cli)} 引擎暂不支持图片消息（文件附件可用）`);
    return;
  }
  const nAttach = state.nsAttached ? state.nsAttached.length : 0;
  const name = content
    ? (content.length > 30 ? content.slice(0, 30) + '…' : content)
    : (nAttach ? (images.length ? '[图片]' : '[附件]') : '新会话');
  const sess = await api('/api/sessions', {
    method: 'POST',
    body: JSON.stringify({
      name, cli, provider, model, cwd: nsPendingCwd,
      effort: effort || undefined,
      permission_mode: perm || undefined,
    }),
  });
  $('#new-session-view').classList.add('hidden');
  const nsFiles = [...(state.nsAttached || [])];
  state.nsAttached = [];
  // 文件附件：会话建成后才拿到 sid，此时才上传到 data/uploads/<sid>/。
  // 失败则停在当前会话，用户可在聊天里补传。
  const files = [];
  for (const a of nsFiles) {
    if (a.kind !== 'file') continue;
    try {
      const up = await uploadAttachment(a.file, sess.id);
      files.push({ name: up.name, path: up.path });
    } catch (e) {
      setErrorBanner(`上传失败：${a.name}（${e.message || 'unknown error'}）`);
      files.length = 0; // 半截文件不发给模型，整条附件作废
      break;
    }
  }
  await loadSessions();
  await openSession(sess.id);
  // Optimistic bubble so the user sees their message immediately even if SSE
  // hasn't connected yet (race between EventSource open + first event push).
  appendOptimisticUser(content, images, files);
  scrollBottom();
  await api(`/api/sessions/${sess.id}/messages`, {
    method: 'POST',
    body: JSON.stringify({ text: content, images, files }),
  });
  updateStatusUI({ running: true, queued: 0 });
}

$('#ns-send').onclick = nsSend;
$('#ns-input').onkeydown = (e) => {
  if (e.key === 'Enter' && !e.shiftKey) {
    e.preventDefault();
    nsSend();
  }
};
// textarea auto-grow
$('#ns-input').addEventListener('focus', () => { slashTextarea = nsInputEl; slashPopup = nsSlashPopupEl; });
$('#ns-input').addEventListener('input', onInputChange);
$('#ns-input').addEventListener('blur', () => { setTimeout(closeSlashPopup, 200); });
$('#ns-input').onkeydown = (e) => {
  if (slashState.open) {
    if (e.key === 'ArrowDown') { e.preventDefault(); moveSlashActive(1); return; }
    if (e.key === 'ArrowUp') { e.preventDefault(); moveSlashActive(-1); return; }
    if (e.key === 'Tab' || (e.key === 'Enter' && !e.shiftKey)) {
      e.preventDefault(); selectSlashItem(); return;
    }
    if (e.key === 'Escape') { e.preventDefault(); closeSlashPopup(); return; }
  }
  if (e.key === 'Enter' && !e.shiftKey) {
    e.preventDefault();
    nsSend();
  }
};
$('#ns-attach').onclick = () => $('#ns-attach-file').click();
$('#ns-attach-file').onchange = async (e) => {
  await nsAttachFiles([...e.target.files]);
  e.target.value = '';
};

// 与主 composer 不同的地方：文件在会话建成那一刻才上传（先要拿到 sid），
// 这里先把 File 对象留在内存里，nsSend 里统一上传。
async function nsAttachFiles(files) {
  for (const f of files) {
    if (!nsAttachOk(f)) continue;
    state.nsAttached = state.nsAttached || [];
    const noInlineImages = !engineTakesImages(cliKind($('#ns-cli').value));
    if (ALLOWED_MIME.includes(f.type) && noInlineImages) {
      // mcode 引擎没有内嵌图片通道：图片按文件附件走（`mcode exec --file`），
      // 视觉模型可直接看到。会话建成后 nsSend 统一上传为 path。
      state.nsAttached.push({ kind: 'file', name: f.name, size: f.size, file: f });
      continue;
    }
    if (ALLOWED_MIME.includes(f.type)) {
      const data = await new Promise((res) => {
        const r = new FileReader();
        r.onload = () => res(r.result);
        r.readAsDataURL(f);
      });
      state.nsAttached.push({ kind: 'image', data, mime: f.type });
    } else {
      state.nsAttached.push({ kind: 'file', name: f.name, size: f.size, file: f });
    }
  }
  renderNsAttached();
}

function nsAttachOk(f) {
  const atts = state.nsAttached || [];
  // 无内嵌图片通道的引擎，图片按文件附件计（见 nsAttachFiles）
  const asFile = ALLOWED_MIME.includes(f.type) && !engineTakesImages(cliKind($('#ns-cli').value));
  if (!ALLOWED_MIME.includes(f.type) || asFile) {
    if (atts.filter((a) => a.kind === 'file').length >= MAX_FILES) {
      alert(`最多 ${MAX_FILES} 个文件附件`);
      return false;
    }
    if (f.size > MAX_FILE_BYTES) {
      alert(`文件过大：${f.name}（${fmtMiB(f.size)}MB > ${MAX_FILE_BYTES >> 20}MB）`);
      return false;
    }
  } else {
    if (atts.filter((a) => a.kind === 'image').length >= MAX_IMAGES) {
      alert(`最多 ${MAX_IMAGES} 张图片`);
      return false;
    }
    if (f.size > MAX_IMAGE_BYTES) {
      alert(`图片过大：${f.name}（${(f.size / 1024 / 1024).toFixed(1)}MB > ${MAX_IMAGE_BYTES / 1024 / 1024}MB）`);
      return false;
    }
  }
  return true;
}
// paste 支持图片与文件
$('#ns-input').addEventListener('paste', async (e) => {
  const items = e.clipboardData && e.clipboardData.items;
  if (!items) return;
  const files = [];
  for (const it of items) {
    if (it.kind === 'file') {
      const f = it.getAsFile();
      if (f) files.push(f);
    }
  }
  if (files.length) {
    e.preventDefault();
    await nsAttachFiles(files);
  }
});

// ---------- mobile: drawer nav + bottom sheet ----------
const isMobile = () => window.matchMedia('(max-width: 640px)').matches;

function openDrawer() {
  document.body.classList.add('drawer-open');
  $('#drawer-overlay').classList.remove('hidden');
  renderIcons();
}
function closeDrawer() {
  document.body.classList.remove('drawer-open');
  $('#drawer-overlay').classList.add('hidden');
}

function syncSheetFromCurrent() {
  if (!state.current) return;
  const engine = state.current.engine || 'claude';
  fillCLISelect($('#sheet-cli'), state.current.cli, engine);
  fillProviderSelect($('#sheet-provider'), state.current.provider, engine);
  fillModelSelect($('#sheet-model'), state.current.provider, state.current.model_override, state.current.cli);
  $('#sheet-effort').value = state.current.effort_override || '';
  $('#sheet-perm').value = state.current.permission_mode || '';
  const cwd = state.current.cwd || '';
  $('#sheet-cwd').value = cwd;
  $('#sheet-cwd').title = cwd;
  renderIcons();
}

function openSheet() {
  if (!state.current) return;
  syncSheetFromCurrent();
  document.body.classList.add('sheet-open');
  $('#chat-sheet').classList.remove('hidden');
  renderIcons();
}
function closeSheet() {
  document.body.classList.remove('sheet-open');
  $('#chat-sheet').classList.add('hidden');
}

$('#btn-drawer').onclick = openDrawer;
$('#btn-drawer-standalone').onclick = openDrawer;
$('#btn-chat-menu').onclick = () => {
  if (document.body.classList.contains('sheet-open')) closeSheet();
  else openSheet();
};
$('#drawer-overlay').onclick = closeDrawer;

// click outside the bottom sheet closes it
document.addEventListener('pointerdown', (e) => {
  if (!document.body.classList.contains('sheet-open')) return;
  if (!e.target.closest('#chat-sheet')) closeSheet();
});

document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape') {
    // 层级链：最上层先关，一次 Esc 只退一层
    if (gmOpen) closeGroupMemberPanel();
    else if (state.showSearch) $('#btn-search-toggle').click();
    else if (!$('#file-viewer').classList.contains('hidden')) closeFileViewer();
    else if (document.body.classList.contains('sheet-open')) closeSheet();
    else if (document.body.classList.contains('drawer-open')) closeDrawer();
  }
});

// sheet 里的选择要先同步到主下拉再派发 change（主处理器读的是 e.target.value）
const syncSheetSelect = (sheetSel, mainSel) => {
  sheetSel.onchange = (e) => {
    mainSel.value = e.target.value;
    mainSel.dispatchEvent(new Event('change'));
  };
};
syncSheetSelect($('#sheet-cli'), $('#session-cli'));
syncSheetSelect($('#sheet-provider'), $('#session-provider'));
syncSheetSelect($('#sheet-model'), $('#session-model'));
syncSheetSelect($('#sheet-effort'), $('#session-effort'));
syncSheetSelect($('#sheet-perm'), $('#session-perm'));
// 工作区输入框同样从 sheet 同步到主输入框再派发（主处理器在 input 事件上）
$('#sheet-cwd').oninput = () => {
  $('#session-cwd').value = $('#sheet-cwd').value;
  $('#session-cwd').dispatchEvent(new Event('input'));
};

$('#sheet-resume').onclick = () => { closeSheet(); $('#btn-resume').click(); };
$('#sheet-delete').onclick = () => { closeSheet(); $('#btn-delete').click(); };
$('#sheet-archive').onclick = async () => {
  if (!state.current) return;
  const id = state.current.id;
  const archived = !state.current.archived;
  await api(`/api/sessions/${id}`, { method: 'PATCH', body: JSON.stringify({ archived }) });
  closeSheet();
  loadSessions();
};

// 主下拉 change 处理器在 PATCH 成功后调用，把最新会话状态回填到 sheet
// （重建选项列表，保证 provider 切换后 model 列表联动刷新）
function patchSheetIfOpen(s) {
  if (!document.body.classList.contains('sheet-open')) return;
  const engine = s.engine || 'claude';
  fillCLISelect($('#sheet-cli'), s.cli, engine);
  fillProviderSelect($('#sheet-provider'), s.provider, engine);
  fillModelSelect($('#sheet-model'), s.provider, s.model_override, s.cli);
  $('#sheet-effort').value = s.effort_override || '';
  $('#sheet-perm').value = s.permission_mode || '';
  $('#sheet-cwd').value = s.cwd || '';
  $('#sheet-cwd').title = s.cwd || '';
}

// When the user picks a session in the list while the drawer is open,
// close the drawer so they see the chat.
const _origOpenSession = openSession;
openSession = async function(id) {
  await _origOpenSession(id);
  if (isMobile()) closeDrawer();
};

// Mobile Enter behavior: on a touch device, soft keyboards usually lack
// Shift+Enter. Switch the placeholder/hint so the user knows Enter sends.
function updateMobileInputHint() {
  const el = $('#input');
  if (!el) return;
  el.placeholder = isMobile() ? '发消息…' : '输入消息，Enter 发送，Shift+Enter 换行';
}
window.addEventListener('resize', updateMobileInputHint);
document.addEventListener('visibilitychange', () => {
  if (document.visibilityState === 'visible') {
    loadSessions().catch(() => {});
    refreshProfiles();
    if (state.current) refreshCurrentStatus();
  }
});


// ---------- PWA ----------
if ('serviceWorker' in navigator) {
  window.addEventListener('load', () => {
    navigator.serviceWorker.register('/sw.js').catch(() => {});
  });
}

// ---------- 群聊（影子成员会话＋并行编排） ----------
// 群时间线只看正文（user/成员回复）；工具调用、审批之外的细节去成员会话看
// （成员是真实会话，点名字直接打开，回放/工具卡全量）。审批卡经群 SSE 转发，
// 挂同一个 dock，决议 POST 到成员影子会话。
async function loadGroups() {
  try {
    state.groups = await api('/api/groups') || [];
  } catch (_) {
    state.groups = [];
  }
  renderSessionList(); // 群与单聊合并渲染（按工作区分组，群挂群聊小标签）
  renderWsActivity(); // 群也可能有待回答提问（成员卡只在群视图里），徽标得跟着刷
}
// 群条目：与会话同行，挂「群聊」小标签，点开进群视图
function appendGroupItem(box, g) {
  const el = document.createElement('div');
  const asking = (g.pending_asks || []).length > 0;
  el.className = 'session-item group-item' + (state.currentGroup && state.currentGroup.id === g.id ? ' active' : '') +
    (asking ? ' has-ask' : '');
  el.innerHTML = '<span class="dot"></span><span class="sess-name"></span><span class="group-tag">群聊</span>' +
    askFlagHTML(g.pending_asks) + '<span class="sess-meta"></span>';
  el.querySelector('.sess-name').textContent = g.name;
  el.querySelector('.sess-meta').textContent = `${(g.members || []).length} 成员${g.running ? ' · 跑着' : ''}`;
  // 成员提问只在群视图里有卡，所以群条目也得自己标出来（成员影子会话不进侧栏）
  el.querySelector('.dot').className = asking ? 'dot ask' : 'dot' + (g.running ? ' running' : '');
  el.title = [askTip(g.pending_asks), g.cwd].filter(Boolean).join('\n');
  el.onclick = () => openGroup(g.id);
  box.appendChild(el);
}
function hideAllViews() {
  $('#empty-state').classList.add('hidden');
  $('#chat-view').classList.add('hidden');
  $('#new-session-view').classList.add('hidden');
  $('#group-view').classList.add('hidden');
  closeGroupMemberPanel();
  closeGroupMentionPopup();
}
async function openGroup(gid) {
  let data;
  try {
    data = await api(`/api/groups/${gid}`);
  } catch (e) {
    alert('打开群聊失败：' + e.message);
    return;
  }
  if (state.es) { state.es.close(); state.es = null; }
  state.currentGroup = data.group;
  state.groupSeen = new Set();
  state.groupLive = {};
  hideAllViews();
  $('#group-view').classList.remove('hidden');
  $('#group-name').textContent = data.group.name;
  renderGroupMembers(data.group);
  // 时间线只由 SSE 回放画（与单聊一致）：这里若先把 data.transcript 画一遍，
  // 紧接着 connectGroupEvents 的回放会把同一批条目再发一次，而 groupSeen 刚
  // 被清空、没有任何东西能拦它——每条消息都会出现两遍。断开重连时 onopen 又
  // 走一遍 openGroup，清空 + 新连接回放，同样只画一次。
  $('#group-messages').innerHTML = '';
  updateGroupStatus(data.group.running);
  relocatePermCards();
  connectGroupEvents(gid);
  // 各成员的未决审批补卡（transient，不进回放）
  for (const mb of (data.group.members || [])) {
    try {
      const r = await api(`/api/sessions/${mb.id}/permissions/pending`);
      for (const p of (r && r.pending) || []) {
        p.member_id = mb.id;
        p.member_name = mb.name;
        p.group_id = gid; // 提示的「去回答」据此留在群视图里，而不是跳去成员影子会话
        renderPermissionCard(p);
      }
    } catch (_) {}
  }
  renderSessionList(); // 刷新合并列表里的 active 高亮
  if (!isMobile()) $('#group-input').focus();
}
$('#ng-add-member').onclick = () => {
  const { row } = ngMemberRow();
  $('#ng-members').appendChild(row);
};
$('#ng-submit').onclick = async () => {
  const name = $('#ng-name').value.trim();
  const cwd = $('#ng-cwd').value.trim();
  const members = [...$('#ng-members').querySelectorAll('.ng-member')].map((row) => ({
    name: row.querySelector('.ng-name').value.trim(),
    cli: row.querySelector('.ng-cli').value,
    provider: row.querySelector('.ng-prov').value,
    // 空 = 继承：provider 默认模型 / 不指定档位
    model: row.querySelector('.ng-model').value,
    effort: row.querySelector('.ng-effort').value,
  })).filter((m) => m.name && m.cli && m.provider);
  if (!name || !cwd || members.length === 0) {
    alert('群名、工作目录、至少一个完整成员必填');
    return;
  }
  try {
    const g = await api('/api/groups', { method: 'POST', body: JSON.stringify({ name, cwd, members }) });
    $('#modal-new-group').classList.add('hidden');
    await loadGroups();
    openGroup(g.id);
  } catch (e) {
    alert('建群失败：' + e.message);
  }
};
$('#btn-group-send').onclick = () => sendGroupMessage();
// ---------- 群聊 @成员 自动补全 ----------
// 打 @ 列出本群成员（外加 @all 群发），↑↓/Tab/Enter 选中，鼠标点选也行；选中后
// 把光标前的 @词 替换成 "@名 "。只认词首的 @——服务端 parseGroupRecipients 按空白
// 切 token 识别 @"名字"（成员名本身不含空格），非词首的 @ 不弹正好与它一致。
const groupInputEl = $('#group-input');
const groupMentionPopupEl = $('#group-mention-popup');
const groupMention = { open: false, items: [], active: 0 };

// 光标前正在输入的 @词。成员名按建群校验不含空格/@，但可以是中文，所以 token
// 用「非空白非 @」而不是 \w。只认词首：开头、空白之后，或紧跟中文/全角标点
// （中文里「看下@小王」这种不带空格的写法很常见）；邮箱/URL 里的 @ 不弹。
// 这条判定与服务端 parseGroupRecipients 对齐——那边按空白切 token、要求 token
// 以 @ 开头，所以选中时若前一个字不是空白会补一个空格，否则这条 @ 不会被路由。
const CJK_OR_SPACE = /[\s぀-鿿　-〿＀-￯]/;

function currentMentionToken(text, pos) {
  const before = text.slice(0, pos);
  const m = before.match(/@[^\s@]*$/);
  if (!m) return null;
  const start = pos - m[0].length;
  if (start > 0 && !CJK_OR_SPACE.test(before[start - 1])) return null;
  return { prefix: m[0], start };
}
// 候选项：@all 固定排第一，后面是本群成员（右侧提示模型/后端，分辨同名近似的人）
function groupMentionCandidates() {
  const ms = (state.currentGroup && state.currentGroup.members) || [];
  return [{ name: 'all', hint: '群发全体' }]
    .concat(ms.map((m) => ({ name: m.name, hint: m.model || m.provider || '' })));
}
function openGroupMentionPopup(filter) {
  const q = filter.toLowerCase();
  const items = groupMentionCandidates().filter((x) => !q || x.name.toLowerCase().includes(q));
  if (!items.length) { closeGroupMentionPopup(); return; }
  groupMention.open = true;
  groupMention.items = items;
  groupMention.active = 0;
  groupMentionPopupEl.innerHTML = '';
  items.forEach((it, i) => {
    const el = document.createElement('div');
    el.className = 'slash-popup-item' + (i === 0 ? ' active' : '');
    el.innerHTML = `<span class="slash-name">@${escapeHtml(it.name)}</span>` +
      (it.hint ? `<span class="slash-desc">${escapeHtml(it.hint)}</span>` : '');
    // 用 mousedown 而不是 click：先 preventDefault 掉失焦，弹窗不会被 blur 的
    // 延时关闭抢在 click 之前收掉，textarea 也保持焦点。
    el.onmousedown = (e) => { e.preventDefault(); selectGroupMention(i); };
    el.onmouseenter = () => setGroupMentionActive(i);
    groupMentionPopupEl.appendChild(el);
  });
  groupMentionPopupEl.classList.remove('hidden');
}
function setGroupMentionActive(i) {
  groupMention.active = i;
  for (const el of groupMentionPopupEl.children) el.classList.remove('active');
  const el = groupMentionPopupEl.children[i];
  if (el) {
    el.classList.add('active');
    el.scrollIntoView({ block: 'nearest' });
  }
}
function moveGroupMentionActive(delta) {
  if (!groupMention.open) return;
  const n = groupMention.items.length;
  setGroupMentionActive((groupMention.active + delta + n) % n);
}
function closeGroupMentionPopup() {
  groupMention.open = false;
  groupMention.items = [];
  groupMentionPopupEl.classList.add('hidden');
}
function selectGroupMention(idx) {
  if (!groupMention.open) return;
  const it = groupMention.items[idx ?? groupMention.active];
  if (!it) return;
  const ta = groupInputEl;
  const tok = currentMentionToken(ta.value, ta.selectionStart);
  closeGroupMentionPopup();
  if (!tok) return;
  // 前一个字不是空白就补一个空格：中文常写成「看下@小王」，不补服务端切不出
  // 这个 token，这条 @ 就白发了。
  const lead = tok.start > 0 && !/\s/.test(ta.value[tok.start - 1]) ? ' ' : '';
  const rep = lead + '@' + it.name + ' ';
  const caret = tok.start + rep.length;
  ta.value = ta.value.slice(0, tok.start) + rep + ta.value.slice(ta.selectionStart);
  ta.setSelectionRange(caret, caret);
  ta.focus();
  syncGroupInputHeight();
}
function syncGroupInputHeight() {
  const ta = groupInputEl;
  ta.style.height = 'auto';
  ta.style.height = Math.min(ta.scrollHeight, 180) + 'px';
}
groupInputEl.addEventListener('input', () => {
  syncGroupInputHeight();
  const tok = currentMentionToken(groupInputEl.value, groupInputEl.selectionStart);
  if (tok) openGroupMentionPopup(tok.prefix.slice(1));
  else closeGroupMentionPopup();
});
groupInputEl.addEventListener('keydown', (e) => {
  if (groupMention.open) {
    if (e.key === 'ArrowDown') { e.preventDefault(); moveGroupMentionActive(1); return; }
    if (e.key === 'ArrowUp') { e.preventDefault(); moveGroupMentionActive(-1); return; }
    if (e.key === 'Tab' || (e.key === 'Enter' && !e.shiftKey && !e.isComposing)) {
      e.preventDefault(); selectGroupMention(); return;
    }
    if (e.key === 'Escape') { e.preventDefault(); closeGroupMentionPopup(); return; }
  }
  // isComposing：中文输入法选词时按 Enter 只是确认候选词，不能拿去发消息
  if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
    e.preventDefault();
    sendGroupMessage();
  }
});
groupInputEl.addEventListener('blur', () => setTimeout(closeGroupMentionPopup, 150));
$('#btn-group-stop').onclick = async () => {
  if (!state.currentGroup) return;
  try {
    await api(`/api/groups/${state.currentGroup.id}/stop`, { method: 'POST' });
  } catch (e) {
    alert('停止失败：' + e.message);
  }
};
$('#btn-group-delete').onclick = async () => {
  const g = state.currentGroup;
  if (!g) return;
  if (!confirm(`解散群聊「${g.name}」？成员影子会话一并删除，群记录清除。`)) return;
  try {
    await api(`/api/groups/${g.id}`, { method: 'DELETE' });
    if (state.groupEs) state.groupEs.close();
    state.groupEs = null;
    state.currentGroup = null;
    $('#group-view').classList.add('hidden');
    $('#empty-state').classList.remove('hidden');
    await loadGroups();
  } catch (e) {
    alert('解散失败：' + e.message);
  }
};
$('#btn-group-drawer').onclick = openDrawer;
function renderGroupMembers(g) {
  const bar = $('#group-members');
  bar.innerHTML = '';
  for (const mb of (g.members || [])) {
    const item = document.createElement('span');
    item.className = 'group-member-item';
    const b = document.createElement('button');
    b.className = 'group-member-chip';
    b.title = `打开成员会话 ${mb.name}（看工具调用与完整回放）`;
    b.textContent = `👤 ${mb.name}`;
    b.onclick = () => openSession(mb.id);
    const tune = document.createElement('button');
    tune.className = 'icon-btn group-member-tune';
    tune.title = '调整该成员的 CLI / Provider / 模型 / 推理档位';
    tune.innerHTML = '<i data-lucide="sliders-horizontal"></i>';
    tune.onclick = () => openGroupMemberPanel(g, mb, tune);
    item.appendChild(b);
    item.appendChild(tune);
    bar.appendChild(item);
  }
  const hint = document.createElement('span');
  hint.className = 'group-members-hint';
  hint.textContent = '点成员看它的单独回放（含工具调用）';
  bar.appendChild(hint);
  renderIcons(bar);
}
// 成员档案面板：运行时改单个成员的 CLI/Provider/模型/档位，落
// PATCH /api/groups/{id}/members/{memberId}。服务端同时改影子会话（真正跑的那
// 份）与群 meta（这里显示的那份），改动从下一轮派发生效。
let gmOpen = null; // { gid, memberId }
function closeGroupMemberPanel() {
  $('#group-member-panel').classList.add('hidden');
  gmOpen = null;
}
// 把一条成员记录灌进面板：初值与「改失败后回滚」共用一份。
function gmFillPanel(row, mb) {
  const p = ngProfileParts(row);
  const engine = cliKind(mb.cli);
  // 会话绑定单一引擎，所以 cli 下拉只列同 kind 的（与单聊视图一致）。
  fillCLISelect(p.cli, mb.cli, engine);
  fillProviderSelect(p.prov, mb.provider, engine);
  fillModelSelect(p.model, mb.provider, mb.model, mb.cli);
  p.effort.value = mb.effort || '';
  // 档位按 engine+模型重建（动态下拉：模板显式传，不走按 id 的懒缓存），
  // 重建完再落回成员自己的值。
  return refreshNgEffort(row, p.effort.closest('label')).then(() => { p.effort.value = mb.effort || ''; });
}
async function openGroupMemberPanel(g, mb, anchor) {
  const panel = $('#group-member-panel');
  if (gmOpen && gmOpen.memberId === mb.id) { closeGroupMemberPanel(); return; }
  closeGroupMemberPanel();
  gmOpen = { gid: g.id, memberId: mb.id };
  const row = panel.querySelector('.gm-profile');
  const p = ngProfileParts(row);
  panel.querySelector('.gm-title').textContent = `成员设置 · ${mb.name}`;
  panel.classList.remove('hidden');
  const rect = anchor.getBoundingClientRect();
  const box = panel.getBoundingClientRect();
  panel.style.left = Math.max(8, Math.min(rect.right + 4, window.innerWidth - box.width - 8)) + 'px';
  panel.style.top = Math.max(8, Math.min(rect.top - 4, window.innerHeight - box.height - 8)) + 'px';
  gmFillPanel(row, mb);
  // 落库 + 按服务端返回的成员重建联动下拉。cascade 在「改了会连带别的下拉」的
  // 情况下重拉它们（cli→provider→模型→档位），否则服务端已清掉的 model/effort
  // 会留在下拉上。
  const patch = async (body, cascade) => {
    try {
      const updated = await api(`/api/groups/${g.id}/members/${mb.id}`, { method: 'PATCH', body: JSON.stringify(body) });
      const me = (updated.members || []).find((x) => x.id === mb.id);
      if (!me) return;
      mb = me; // 成功后才推进「当前值」，失败时 catch 里回滚到的仍是它
      gmOpen = { gid: g.id, memberId: me.id };
      state.currentGroup = updated;
      renderGroupMembers(updated);
      if (cascade) cascade(me);
    } catch (e) {
      alert('改成员配置失败：' + e.message);
      gmFillPanel(row, mb); // 拨回服务端那份，别停在一个没生效的值上
    }
  };
  p.cli.onchange = () => patch({ cli: p.cli.value }, (me) => {
    fillProviderSelect(p.prov, me.provider, cliKind(me.cli));
    fillModelSelect(p.model, me.provider, me.model, me.cli);
    refreshNgEffort(row, p.effort.closest('label')).then(() => { p.effort.value = me.effort || ''; });
  });
  p.prov.onchange = () => patch({ provider: p.prov.value }, (me) => {
    fillModelSelect(p.model, me.provider, me.model, me.cli);
    refreshNgEffort(row, p.effort.closest('label')).then(() => { p.effort.value = me.effort || ''; });
  });
  p.model.onchange = () => patch({ model: p.model.value }, (me) => {
    refreshNgEffort(row, p.effort.closest('label')).then(() => { p.effort.value = me.effort || ''; });
  });
  p.effort.onchange = () => patch({ effort: p.effort.value });
}
function updateGroupStatus(running) {
  $('#group-status').textContent = running ? '跑着…' : '';
  $('#group-status').classList.toggle('running', !!running);
  $('#btn-group-stop').classList.toggle('hidden', !running);
}
// 群成员竖线颜色：按成员在 meta.members 里的序号取色板（相邻序号冷暖交替，
// 并排发言的两个成员一定不同色；名字本来就在 role-tag 里，颜色只是辅助）。
// 写的是 var(--agent-N) 而不是具体色值——切深浅主题时 CSS 变量重新解析，
// 不用重渲染 DOM。取不到该成员（如成员刚被删）就退回名字的哈希，稳定不随
// 成员增删漂移。
function applyMemberColor(bubble, memberID) {
  const ms = (state.currentGroup && state.currentGroup.members) || [];
  let idx = ms.findIndex((m) => m.id === memberID);
  if (idx < 0) {
    let h = 0;
    const key = String(memberID || '');
    for (let i = 0; i < key.length; i++) h = (h * 31 + key.charCodeAt(i)) >>> 0;
    idx = h % 8;
  }
  const row = bubble.closest('.msg') || bubble.parentElement;
  if (row) row.style.setProperty('--agent-color', `var(--agent-${(idx % 8) + 1})`);
}
function gStatusLine(key, text) {
  const box = $('#group-messages');
  let el = box.querySelector(`[data-gstatus="${key}"]`);
  if (!el) {
    el = document.createElement('div');
    el.className = 'meta-line';
    el.dataset.gstatus = key;
    box.appendChild(el);
  }
  el.textContent = text;
  return el;
}
function gClearStatus(key) {
  const el = $('#group-messages').querySelector(`[data-gstatus="${key}"]`);
  if (el) el.remove();
}
function connectGroupEvents(gid) {
  if (state.groupEs) state.groupEs.close();
  const es = new EventSource(`/api/groups/${gid}/events`);
  state.groupEs = es;
  let opened = false;
  es.onopen = () => {
    if (opened && state.currentGroup && state.currentGroup.id === gid) openGroup(gid);
    opened = true;
  };
  es.onmessage = (e) => {
    const ev = JSON.parse(e.data);
    if (!state.groupSeen) state.groupSeen = new Set();
    if (state.groupSeen.has(ev.seq)) return;
    state.groupSeen.add(ev.seq);
    if (ev.kind === 'replay_end') return;
    // 成员审批卡：与单聊同一渲染器，决议挂成员会话
    if (ev.kind === 'permission_request') {
      const d = ev.data || {};
      renderPermissionCard(d);
      return;
    }
    if (ev.kind === 'permission_resolved') {
      resolvePermissionCard(ev.data);
      return;
    }
    if (ev.kind !== 'group') return;
    const d = ev.data || {};
    const box = $('#group-messages');
    if (d.type === 'user') {
      const bubble = addMsg('msg-user', '你', box);
      bubble.textContent = d.text || '';
      updateGroupStatus(true);
    } else if (d.type === 'member_begin') {
      gStatusLine(`begin-${d.turn_id}-${d.member_id}`, `👤 ${d.member_name} 正在输入…`);
      updateGroupStatus(true);
    } else if (d.type === 'member_text') {
      state.groupLive = state.groupLive || {};
      const turn = (state.groupLive[d.turn_id] = state.groupLive[d.turn_id] || {});
      let live = turn[d.member_id];
      if (!live) {
        const bubble = addMsg('msg-assistant group-member-msg', escapeHtml(d.member_name || '成员'), box);
        applyMemberColor(bubble, d.member_id);
        live = turn[d.member_id] = { bubble, buf: '' };
      }
      live.buf += (live.buf ? '\n\n' : '') + (d.text || '');
      live.bubble.textContent = live.buf; // 流式先纯文本，结束再 md
      gClearStatus(`begin-${d.turn_id}-${d.member_id}`);
    } else if (d.type === 'member_end') {
      const turn = state.groupLive && state.groupLive[d.turn_id];
      const live = turn && turn[d.member_id];
      if (live) {
        if (d.ok && d.text) live.bubble.innerHTML = md(d.text);
        else if (!d.ok) live.bubble.innerHTML = `<span class="group-error">（${escapeHtml(d.error || '无回复')}）</span>`;
        delete turn[d.member_id];
      } else if (d.ok && d.text) {
        // 重连/刷新错过直播：直接落正文（回放里也有，不会丢）
        const bubble = addMsg('msg-assistant group-member-msg', escapeHtml(d.member_name || '成员'), box);
        applyMemberColor(bubble, d.member_id);
        bubble.innerHTML = md(d.text);
      }
      gClearStatus(`begin-${d.turn_id}-${d.member_id}`);
    } else if (d.type === 'turn_end') {
      updateGroupStatus(false);
      loadGroups().catch(() => {});
    }
    box.scrollTop = box.scrollHeight;
  };
}
async function sendGroupMessage() {
  const g = state.currentGroup;
  if (!g) return;
  const ta = $('#group-input');
  const text = ta.value.trim();
  if (!text) return;
  $('#group-hint').textContent = '';
  try {
    await api(`/api/groups/${g.id}/messages`, { method: 'POST', body: JSON.stringify({ text }) });
    ta.value = '';
    ta.style.height = 'auto';
  } catch (e) {
    // 409 本轮未结束：留着草稿提示，不吞消息
    $('#group-hint').textContent = e.message.includes('in flight') ? '本轮还没跑完，稍等再发（草稿已保留）' : ('发送失败：' + e.message);
  }
}
// ---------- 新建群聊弹窗 ----------
// 成员行 = 成员名 + CLI + Provider + 模型 + 推理档位（最后两列与单聊视图同一套
// 下拉：模型随 provider 联动、档位随 engine+模型重建）。群视图的成员档案面板
// 复用同一批 ng-* 类名，所以联动只有这一份。
function ngProfileParts(row) {
  return {
    cli: row.querySelector('.ng-cli'),
    prov: row.querySelector('.ng-prov'),
    model: row.querySelector('.ng-model'),
    effort: row.querySelector('.ng-effort'),
  };
}

// cli 变了：provider 归位同 kind 首个，模型/档位跟着重建
function ngResetProvider(row) {
  const { cli, prov, model } = ngProfileParts(row);
  fillProviderSelect(prov, '', cliKind(cli.value));
  fillModelSelect(model, prov.value, '', cli.value);
  return refreshNgEffort(row);
}
// provider 变了：模型列表重建，档位跟着重建
function ngResetModel(row) {
  const { prov, model, cli } = ngProfileParts(row);
  fillModelSelect(model, prov.value, '', cli.value);
  return refreshNgEffort(row);
}

// 成员行的推理档位：与 ns/新建/会话视图同一套 engine 分流（见 refreshNsEffort）。
// wrap 是随下拉显隐的 label——建群弹窗里下拉是裸的，面板里包在 label 里。
async function refreshNgEffort(row, wrap) {
  const { cli, prov, model, effort } = ngProfileParts(row);
  const cliName = cli.value;
  const provName = prov.value;
  const kind = cliKind(cliName);
  const apply = (k, keys) => {
    applyEffortOptions(effort, k, keys, wrap, EFFORT_CLAUDE_HTML);
    // 该模型没有档位时下拉是隐藏的，留着的选中值会被当档位提交——
    // 置空，让它回到继承。
    if (effort.classList.contains('hidden')) effort.value = '';
  };
  if (kind === 'opencode') {
    apply('opencode', variantKeysFor(await ocVariantsFor(cliName, provName), provName, model.value));
    return;
  }
  if (kind === 'mcode') {
    apply('mcode', mcodeEffortsFor(await mcodeCatalogFor(cliName, provName), provName, model.value));
    return;
  }
  if (kind === 'qoder') {
    apply('qoder', qoderEfforts(await qoderCatalogFor(cliName, provName)));
    return;
  }
  if (kind === 'codex') {
    apply('codex', codexEffortsFor(await codexCatalogFor(cliName, provName), model.value));
    return;
  }
  apply('claude');
}
function ngMemberRow() {
  const row = document.createElement('div');
  row.className = 'ng-member';
  row.innerHTML =
    '<input class="ng-name" placeholder="成员名（@用，无空格）">' +
    '<select class="ng-cli"></select>' +
    '<select class="ng-prov"></select>' +
    '<select class="ng-model"></select>' +
    '<select class="ng-effort"></select>' +
    '<button class="icon-btn ng-del" title="删成员">✕</button>';
  const p = ngProfileParts(row);
  p.effort.innerHTML = EFFORT_CLAUDE_HTML;
  fillCLISelect(p.cli, '', null);
  ngResetProvider(row);
  p.cli.onchange = () => ngResetProvider(row);
  p.prov.onchange = () => ngResetModel(row);
  p.model.onchange = () => refreshNgEffort(row);
  row.querySelector('.ng-del').onclick = () => row.remove();
  return { row, cliSel: p.cli };
}
// 工作区加号：单聊 / 群聊二选一（群聊预填该工作区）
let wsAddCwd = '';
function openWsAddMenu(cwd, anchor) {
  wsAddCwd = cwd;
  const pop = $('#ws-add-menu');
  pop.classList.remove('hidden');
  const rect = anchor.getBoundingClientRect();
  const pw = 180, ph = 90;
  let x = rect.right + 4;
  let y = rect.top - 4;
  if (x + pw > window.innerWidth - 8) x = rect.right - pw;
  x = Math.max(8, Math.min(x, window.innerWidth - pw - 8));
  if (y + ph > window.innerHeight - 8) y = Math.max(8, rect.top - ph - 6);
  pop.style.left = x + 'px';
  pop.style.top = y + 'px';
}
function closeWsAddMenu() { $('#ws-add-menu').classList.add('hidden'); }
document.addEventListener('pointerdown', (e) => {
  // 点在菜单/加号之外才关。反过来写成「点在菜单里就关」，菜单会在 pointerdown
  // 就 display:none，mouseup 打不中按钮，click 被改派到 body——菜单项点了没反应。
  if (!e.target.closest('#ws-add-menu') && !e.target.closest('.group-add-btn')) closeWsAddMenu();
  // 成员档案面板同 ws 菜单：点外面关
  if (gmOpen && !e.target.closest('#group-member-panel') && !e.target.closest('.group-member-tune')) closeGroupMemberPanel();
});
$('#ws-add-menu').addEventListener('click', (e) => {
  const btn = e.target.closest('[data-add-kind]');
  if (!btn) return;
  closeWsAddMenu();
  if (btn.dataset.addKind === 'group') {
    $('#ng-cwd').value = wsAddCwd;
    openNewGroupDialog(wsAddCwd);
  } else {
    openNewSessionView(wsAddCwd);
  }
});
function openNewGroupDialog(cwd) {
  $('#ng-name').value = '';
  $('#ng-cwd').value = cwd || (state.current && state.current.cwd) || state.home || '';
  const box = $('#ng-members');
  box.innerHTML = '';
  // 默认两个成员位：名取前两个 cli，省手工
  for (let i = 0; i < 2; i++) {
    const { row, cliSel } = ngMemberRow();
    if (state.clis[i]) cliSel.value = state.clis[i].name;
    row.querySelector('.ng-name').value = state.clis[i] ? state.clis[i].name : '';
    ngResetProvider(row);
    box.appendChild(row);
  }
  openModal('#modal-new-group');
  renderIcons($('#modal-new-group'));
}

(async function boot() {
  await api('/api/me'); // redirects to login when unauthorized
  applyTheme(document.documentElement.dataset.theme || 'light', false);
  state.clis = await api('/api/clis');
  state.providers = await api('/api/providers');
  state.profilesSig = JSON.stringify([state.clis, state.providers]);
  const cfg = await api('/api/config');
  state.home = cfg.home || '';
  await loadSessions();
  renderIcons();
  updateMobileInputHint();
  setInterval(() => {
    loadSessions().catch(() => {});
    refreshProfiles(); // config 热加载的轮询兜底（无 SSE 连接或广播丢失时）
  }, 5000);
})();

// ---------- Session Goals（自动续跑） ----------
// 服务端在每轮干净结束后判定：达标就停，没达标就自动再排一轮。UI 只做三件事——
// 设目标、显示当前轮次/状态、手动停。不在客户端做任何判定逻辑，否则和服务端的
// 熔断计数会各说各话。
let goalTarget = null;

// pushToast 接的是对象（id/title/body/actions），不是 (文案, 类型)——goal 这几处
// 统一走它，避免同一条提示反复弹。
function goalToast(title, body) {
  pushToast({ id: 'goal-toast-' + (state.current ? state.current.id : 'x'), title, body: body || '' });
}

const GOAL_STATUS_TEXT = {
  active: '续跑中',
  met: '已达标',
  stopped: '已停止',
  exhausted: '已达上限',
};

async function setSessionGoal(id, payload) {
  const res = await fetch(`/api/sessions/${id}/goal`, {
    method: payload == null ? 'DELETE' : 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: payload == null ? undefined : JSON.stringify(payload),
  });
  if (!res.ok) {
    const b = await res.json().catch(() => ({}));
    throw new Error(b.error || `HTTP ${res.status}`);
  }
  return res.json();
}

function openGoalDialog(s) {
  goalTarget = s;
  const g = s.goal;
  $('#goal-text').value = g ? g.text : '';
  $('#goal-turns').value = (g && g.max_turns) || 20;
  $('#goal-hours').value = (g && g.max_hours) || 4;
  $('#modal-goal').classList.remove('hidden');
  setTimeout(() => $('#goal-text').focus(), 0);
}

function closeGoalDialog() {
  $('#modal-goal').classList.add('hidden');
  goalTarget = null;
}

async function submitGoal() {
  const s = goalTarget;
  if (!s) return;
  const text = $('#goal-text').value.trim();
  if (!text) return;
  closeGoalDialog();
  try {
    applySession(await setSessionGoal(s.id, {
      text,
      max_turns: parseInt($('#goal-turns').value, 10) || 20,
      max_hours: parseFloat($('#goal-hours').value) || 4,
    }));
    renderGoalBar();
  } catch (e) {
    goalToast('设置目标失败', e.message);
  }
}

// 目标条只挂在当前会话的 composer 上：目标是 per-session 的，侧栏列表里再塞一条
// 会让窄屏完全没法用。
function renderGoalBar() {
  const bar = $('#goal-bar');
  const s = state.current;
  if (!s || !s.goal) {
    bar.classList.add('hidden');
    bar.textContent = '';
    return;
  }
  const g = s.goal;
  bar.className = 'goal-bar status-' + (g.status || 'active');
  bar.textContent = '';

  const icon = document.createElement('i');
  icon.dataset.lucide = 'target';
  bar.appendChild(icon);

  const body = document.createElement('div');
  body.className = 'goal-bar-body';
  const t = document.createElement('div');
  t.className = 'goal-bar-text';
  t.textContent = g.text;
  const meta = document.createElement('div');
  meta.className = 'goal-bar-meta';
  const status = GOAL_STATUS_TEXT[g.status] || g.status;
  meta.textContent = `${status} · 第 ${g.rounds}/${g.max_turns} 轮` +
    (g.last_reason ? ` · ${g.last_reason}` : '');
  body.append(t, meta);
  bar.appendChild(body);

  const stop = document.createElement('button');
  stop.className = 'goal-bar-stop';
  stop.type = 'button';
  stop.textContent = '停止';
  stop.title = '停止自动续跑（不影响当前这一轮）';
  stop.onclick = async () => {
    try {
      applySession(await setSessionGoal(s.id, null));
      renderGoalBar();
    } catch (e) {
      goalToast('停止失败', e.message);
    }
  };
  bar.appendChild(stop);

  renderIcons(bar);
}

// 把 PATCH/goal 返回的会话视图并回本地状态。goal 是 server-owned 的，别让前端
// 各处自己改，否则侧栏点、目标条、弹窗三份状态会打架。
function applySession(sess) {
  if (!sess) return;
  const i = state.sessions.findIndex((x) => x.id === sess.id);
  if (i >= 0) state.sessions[i] = sess;
  if (state.current && state.current.id === sess.id) state.current = sess;
  renderGoalBar();
}

// 弹窗接线（goal 代码块在文件末尾，这里补事件绑定）
document.addEventListener('click', (e) => {
  if (e.target.closest('#modal-goal [data-close]')) closeGoalDialog();
});
$('#goal-submit').onclick = submitGoal;
$('#goal-clear').onclick = async () => {
  const s = goalTarget;
  if (!s) return;
  closeGoalDialog();
  try {
    applySession(await setSessionGoal(s.id, null));
  } catch (err) {
    goalToast('清除目标失败', err.message);
  }
};
$('#modal-goal').addEventListener('keydown', (e) => {
  if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) submitGoal();
});
