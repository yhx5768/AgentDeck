// web/app.js 里错误横幅（#error-banner）的长文本折叠行为的回归测试。
//
// 为什么要住进仓库：这个 bug 的表现是「报错太长会挡住主对话，看不到消息了」。
// 机制是 #error-banner 在 <footer id="composer"> 里，而 composer 是 position:absolute
// 的浮层（浮在 #messages 之上，z-index:10）。banner 原本 white-space:pre-wrap 且不限高，
// 一条 CLI 崩溃堆栈（node OOM，40+ 帧几千行）灌进去，banner 就长到几千像素：
//   ① 从底部向上溢出盖住主对话；
//   ② syncComposerSpace() 按 composer 的 offsetHeight 给 #messages 设 padding-bottom，
//      同样被撑成几千 px，消息被顶出视口。
//
// jsdom 不做布局，验不了「到底盖没盖住」。能钉的是三样：
//   ① DOM 契约：长错误默认折叠、摘要取到关键行、点击能展开/收起；
//   ② CSS 契约：展开区有 max-height 且 overflow-y:auto —— 这条正是本 bug 的本质，
//      哪天有人把限高删了就该红；
//   ③ 短错误不被折叠（免得把「一句话报错」也塞进「展开全文」里）。
const fs = require('fs');
const path = require('path');
const assert = require('assert');
const { JSDOM } = require('jsdom');

const WEB = path.resolve(__dirname, '../../web');
const src = fs.readFileSync(path.join(WEB, 'app.js'), 'utf8');
const css = fs.readFileSync(path.join(WEB, 'style.css'), 'utf8');

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

// 常量也从源码里取（不抄副本）：阈值改了测试自动跟随，但仍能钉住"存在且被用上"。
function grabConst(name) {
  const m = src.match(new RegExp('const ' + name + '\\s*=\\s*[^;]+;'));
  if (!m) throw new Error('找不到常量 ' + name);
  return m[0];
}

const setup = `
const $ = (sel) => document.querySelector(sel);
${grabConst('ERROR_INLINE_MAX_CHARS')}
${grabConst('ERROR_INLINE_MAX_LINES')}
${grabFn('errorSummary')}
${grabFn('setErrorBanner')}
window.__x = { errorSummary, setErrorBanner };
`;

const dom = new JSDOM(
  '<!doctype html><html><body><div id="error-banner" class="hidden"></div></body></html>',
  { runScripts: 'outside-only', pretendToBeVisual: true });
const win = dom.window;
win.eval(setup);
const { errorSummary, setErrorBanner } = win.__x;
const banner = win.document.querySelector('#error-banner');

// 一条真实的 node OOM 崩溃输出（截取自 cc-webui 会话里 mcode CLI 的 stderr）。
const OOM = [
  '<--- Last few GCs --->',
  '',
  '[32801:0x128008000]  3231328 ms: Mark-Compact 2029.2 (2088.5) -> 2011.5 (2086.7) MB, pooled: 0 MB, 69.29 / 0.04 ms',
  '',
  '<--- JS stacktrace --->',
  '',
  'FATAL ERROR: Ineffective mark-compacts near heap limit Allocation failed - JavaScript heap out of memory',
  '----- Native stack trace -----',
  ' 1: 0x100d1b680 node::OOMErrorHandler(char const*, v8::OOMDetails const*)',
  ' 2: 0x100ef6a94 v8::internal::V8::FatalProcessOutOfMemory(v8::OOMDetails const*)',
].join('\n');

const tests = [];
const test = (name, fn) => tests.push({ name, fn });

test('长错误默认折叠：只露摘要 + 展开按钮，全文收在 .err-full 里', () => {
  setErrorBanner(OOM);
  const head = banner.querySelector('.err-head');
  assert.ok(head, '长错误应渲染出 .err-head');
  const full = banner.querySelector('.err-full');
  assert.ok(full, '长错误应渲染出 .err-full');
  assert.ok(!full.classList.contains('open'), '默认必须是收起的');
  assert.strictEqual(full.textContent, OOM, '全文要一字不少地留在 .err-full 里');
  assert.ok(!banner.querySelector('.err-text'), '长错误不该走 .err-text 直出路径');
});

test('摘要跳过 "<--- Last few GCs --->" 这类噪声行，取到 FATAL ERROR 那行', () => {
  const sum = banner.querySelector('.err-summary');
  assert.ok(sum, '应有 .err-summary');
  assert.ok(/heap out of memory/i.test(sum.textContent),
    '摘要应包含关键错误行，实际是：' + sum.textContent);
  assert.ok(!/Last few GCs/.test(sum.textContent), '摘要不该是噪声行');
});

test('点「展开全文」能展开，再点能收起', () => {
  const btn = banner.querySelector('.err-toggle');
  const full = banner.querySelector('.err-full');
  assert.ok(btn, '应有展开按钮');
  assert.strictEqual(btn.textContent, '展开全文');
  btn.click();
  assert.ok(full.classList.contains('open'), '点击后应展开');
  assert.strictEqual(btn.textContent, '收起');
  btn.click();
  assert.ok(!full.classList.contains('open'), '再点应收起');
  assert.strictEqual(btn.textContent, '展开全文');
});

test('短错误不折叠，整段直出（免得一句话报错也塞进「展开全文」）', () => {
  setErrorBanner('mcode exec failed: The run failed: terminated.');
  const pre = banner.querySelector('.err-text');
  assert.ok(pre, '短错误应走 .err-text');
  assert.strictEqual(pre.textContent, 'mcode exec failed: The run failed: terminated.');
  assert.ok(!banner.querySelector('.err-toggle'), '短错误不该有展开按钮');
});

test('banner 会被显示出来（去掉 hidden）', () => {
  setErrorBanner(OOM);
  assert.ok(!banner.classList.contains('hidden'), 'setErrorBanner 应让 banner 可见');
});

test('warning 变体可切换', () => {
  setErrorBanner('CLI 报告非致命错误', { warning: true });
  assert.ok(banner.classList.contains('warning'));
  setErrorBanner('普通错误');
  assert.ok(!banner.classList.contains('warning'), '普通错误应清掉 warning 态');
});

test('CSS 契约：展开区必须限高 + 内部滚动（这就是盖住主对话的根因）', () => {
  const m = css.match(/\.err-full\s*\{([\s\S]*?)\}/);
  assert.ok(m, 'style.css 里应有 .err-full 规则');
  const body = m[1];
  assert.ok(/max-height\s*:/.test(body), '.err-full 必须有 max-height，否则会把 composer 撑爆');
  assert.ok(/overflow-y\s*:\s*auto/.test(body), '.err-full 必须内部滚动');
  assert.ok(/overscroll-behavior\s*:\s*contain/.test(body),
    '.err-full 要 contain，否则滚到底会连带把聊天区一起滚');
  // banner 本身不能是「不限高的 pre-wrap」——那正是原始 bug 的形状。
  const b = css.match(/#error-banner\s*\{([\s\S]*?)\}/);
  assert.ok(b, 'style.css 里应有 #error-banner 规则');
  assert.ok(!/white-space\s*:\s*pre-wrap/.test(b[1]),
    '#error-banner 不该再是 pre-wrap（长文本的 pre-wrap 交给 .err-text/.err-full）');
});

let failed = 0;
for (const t of tests) {
  try {
    t.fn();
    console.log('  ok  ' + t.name);
  } catch (e) {
    failed++;
    console.error('  FAIL ' + t.name);
    console.error('       ' + (e && e.message ? e.message : e));
  }
}
console.log(`\nerror_banner: ${tests.length - failed}/${tests.length} 通过`);
if (failed) process.exit(1);
