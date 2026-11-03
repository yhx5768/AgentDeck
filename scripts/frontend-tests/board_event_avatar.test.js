// 任务流事件头像的回归测试（YHX-TODO-820）。
const fs = require('fs');
const path = require('path');
const assert = require('assert');
const { JSDOM } = require('jsdom');

const src = fs.readFileSync(path.resolve(__dirname, '../../web/app.js'), 'utf8');
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
const dom = new JSDOM('<!doctype html><html><body></body></html>');
const win = dom.window;
const avatar = new Function('document', grabFn('boardEventAvatar') + '; return boardEventAvatar;')(win.document);

const tests = [];
const test = (name, fn) => tests.push({ name, fn });
test('原始任务使用「原」头像', () => {
  const el = avatar('原始任务');
  assert.strictEqual(el.textContent, '原');
  assert.ok(el.className.includes('ev-create'));
});
test('A2A 发/收使用不同头像和颜色', () => {
  const send = avatar('A2A 发');
  const recv = avatar('A2A 收');
  assert.strictEqual(send.textContent, '发');
  assert.ok(send.className.includes('ev-agent'));
  assert.strictEqual(recv.textContent, '收');
  assert.ok(recv.className.includes('ev-user'));
});
test('未知类型仍有兜底头像', () => {
  const el = avatar('其他');
  assert.strictEqual(el.textContent, '事');
  assert.ok(el.className.includes('ev-other'));
});

let failed = 0;
for (const t of tests) {
  try { t.fn(); console.log('  ok  ' + t.name); }
  catch (e) { failed++; console.error('  FAIL ' + t.name + '：' + (e.message || e)); }
}
console.log(`\nboard_event_avatar: ${tests.length - failed}/${tests.length} 通过`);
if (failed) process.exit(1);
