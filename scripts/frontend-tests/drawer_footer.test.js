// 任务抽屉「内容滚动 + composer 固定底部」的回归测试（YHX-TODO-816 补充）。
//
// 之前的抽屉整体滚动，composer 用 position:sticky 吸附，内容不足时会停在中间。
// 现在结构必须是：抽屉 flex 列 + 独立的 .board-drawer-scroll 内容区 + 同级
// footer composer；四个 tab 的 pane 都留在滚动区内，composer 共享且不滚动。
const fs = require('fs');
const path = require('path');
const assert = require('assert');
const { JSDOM } = require('jsdom');

const WEB = path.resolve(__dirname, '../../web');
const html = fs.readFileSync(path.join(WEB, 'index.html'), 'utf8');
const css = fs.readFileSync(path.join(WEB, 'style.css'), 'utf8');
const dom = new JSDOM(html);
const doc = dom.window.document;

const tests = [];
const test = (name, fn) => tests.push({ name, fn });

test('抽屉结构：内容区独立滚动，composer 是同级底部 footer', () => {
  const drawer = doc.querySelector('#board-drawer');
  const scroll = doc.querySelector('#board-drawer-scroll');
  const composer = doc.querySelector('#board-drawer-composer');
  assert.ok(drawer && scroll && composer, '缺少 drawer/scroll/composer');
  assert.strictEqual(scroll.parentElement, drawer, 'scroll 必须是 drawer 的直接子节点');
  assert.strictEqual(composer.parentElement, drawer, 'composer 必须是 drawer 的直接子节点');
  assert.ok(!scroll.contains(composer), 'composer 不能放在滚动内容里');
});

test('四个 tab 的 pane 全部在滚动区内', () => {
  const scroll = doc.querySelector('#board-drawer-scroll');
  for (const id of ['board-pane-overview', 'board-pane-timeline', 'board-pane-links', 'board-pane-raw']) {
    const pane = doc.querySelector('#' + id);
    assert.ok(pane, '缺少 pane ' + id);
    assert.ok(scroll.contains(pane), id + ' 应在滚动区内');
  }
  assert.ok(scroll.querySelector('#board-drawer-tabs'), 'tab bar 应在滚动区内');
});

test('CSS：抽屉隐藏溢出，内容区 flex 滚动，composer 不参与滚动', () => {
  const drawer = css.match(/\.board-drawer\s*\{([\s\S]*?)\}/)[1];
  assert.ok(/overflow:\s*hidden/.test(drawer), '.board-drawer 必须 overflow:hidden');
  const scroll = css.match(/\.board-drawer-scroll\s*\{([\s\S]*?)\}/)[1];
  assert.ok(/flex:\s*1/.test(scroll), '.board-drawer-scroll 必须 flex:1');
  assert.ok(/min-height:\s*0/.test(scroll), '.board-drawer-scroll 必须 min-height:0');
  assert.ok(/overflow-y:\s*auto/.test(scroll), '.board-drawer-scroll 必须自己滚动');
  const composer = css.match(/\.board-drawer-composer\s*\{([\s\S]*?)\}/)[1];
  assert.ok(/position:\s*static/.test(composer), 'composer 不再 sticky');
  assert.ok(/flex:\s*none/.test(composer), 'composer 不参与 flex 收缩');
  assert.ok(/safe-area-inset-bottom/.test(composer), '移动端要留安全区');
});

test('移动端 CSS：抽屉不再整体加 padding，滚动区/底栏各自适配', () => {
  const media = css.match(/@media \(max-width: 700px\) \{[\s\S]*?\.board-drawer \{([\s\S]*?)\}/);
  assert.ok(media, '找不到移动端 .board-drawer 规则');
  assert.ok(/padding:\s*0/.test(media[1]), '移动端抽屉本身不应再 padding');
  assert.ok(/\.board-drawer-scroll\s*\{[\s\S]*?padding:\s*var\(--sp-4\)/.test(css),
    '移动端滚动区应有自己的 padding');
});

test('CSS：抽屉输入框聚焦只保留外层一层 ring', () => {
  const box = css.match(/\.board-composer-box:focus-within\s*\{([\s\S]*?)\}/);
  assert.ok(box, '缺少 .board-composer-box:focus-within 规则');
  assert.ok(/border-color\s*:\s*var\(--accent\)/.test(box[1]), '外层要保留 accent 边框');
  assert.ok(/box-shadow\s*:/.test(box[1]), '外层要保留 focus ring');
  // 全局 textarea:focus 会画 3px ring；输入框必须覆盖成 none，否则与外层叠成双层包边。
  const inputFocus = css.match(/#board-composer-input:focus\s*\{([\s\S]*?)\}/);
  assert.ok(inputFocus, '缺少 #board-composer-input:focus 覆盖');
  assert.ok(/box-shadow\s*:\s*none/.test(inputFocus[1]), '内层 textarea 不能再画 focus ring');
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
console.log(`\ndrawer_footer: ${tests.length - failed}/${tests.length} 通过`);
if (failed) process.exit(1);
