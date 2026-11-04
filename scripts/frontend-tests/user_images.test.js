// 用户消息图片渲染的回归测试。
//
// 图片消息的事件里只存文件名（见 runner.Message.ImageNames），图片本体存在会话
// 上传目录里，前端经 /api/sessions/{id}/uploads/{name} 取回。此前事件只存文本、
// 图片在服务端回推权威 user 事件后就被顶掉了（乐观气泡闪一下），刷新/切会话更是
// 彻底消失。这里钉住渲染契约：文件名 → <img>、路径按会话 id、名字要 URL 编码、
// 没有图片/没有当前会话时不画。
const fs = require('fs');
const path = require('path');
const assert = require('assert');
const { JSDOM } = require('jsdom');

const WEB = path.resolve(__dirname, '../../web');
const src = fs.readFileSync(path.join(WEB, 'app.js'), 'utf8');

function grabFn(name) {
  let s = src.indexOf('function ' + name + '(');
  if (s < 0) throw new Error('找不到函数 ' + name);
  if (src.slice(Math.max(0, s - 6), s) === 'async ') s -= 6;
  let params = 0, paramsEnd = -1;
  for (let i = src.indexOf('(', s); i < src.length; i++) {
    if (src[i] === '(') params++;
    else if (src[i] === ')') { params--; if (!params) { paramsEnd = i; break; } }
  }
  const b = src.indexOf('{', paramsEnd);
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

const setup = `
const state = { current: { id: 's1' } };
${grabFn('renderUserImages')}
window.__x = { state, renderUserImages, mk() { return document.createElement('div'); } };
`;

const dom = new JSDOM('<!doctype html><html><body></body></html>',
  { runScripts: 'outside-only', pretendToBeVisual: true });
dom.window.eval(setup);
const x = dom.window.__x;

const tests = [];
const test = (name, fn) => tests.push({ name, fn });
const imgs = (bubble) => [...bubble.querySelectorAll('img')];

test('文件名渲染成指向会话上传接口的 <img>', () => {
  x.state.current = { id: 's1' };
  const b = x.mk();
  x.renderUserImages(b, ['image.png']);
  const el = imgs(b);
  assert.strictEqual(el.length, 1);
  assert.strictEqual(el[0].getAttribute('src'), '/api/sessions/s1/uploads/image.png');
  assert.strictEqual(el[0].className, 'thumb');
});

test('多张图按顺序渲染，文件名 URL 编码', () => {
  const b = x.mk();
  x.renderUserImages(b, ['a.png', 'b c.png', 'd/e.png']);
  const el = imgs(b);
  assert.strictEqual(el.length, 3);
  assert.strictEqual(el[1].getAttribute('src'), '/api/sessions/s1/uploads/b%20c.png');
  assert.strictEqual(el[2].getAttribute('src'), '/api/sessions/s1/uploads/d%2Fe.png');
});

test('没有图片 / 空数组不画', () => {
  const b = x.mk();
  x.renderUserImages(b, undefined);
  x.renderUserImages(b, []);
  assert.strictEqual(imgs(b).length, 0);
});

test('没有当前会话时不画（避免拼出坏 URL）', () => {
  x.state.current = null;
  const b = x.mk();
  x.renderUserImages(b, ['image.png']);
  assert.strictEqual(imgs(b).length, 0);
  x.state.current = { id: 's1' };
});

(async () => {
  let failed = 0;
  for (const t of tests) {
    try {
      await t.fn();
      console.log('  ok  ' + t.name);
    } catch (e) {
      failed++;
      console.log('  FAIL ' + t.name + '  →  ' + (e && e.message));
    }
  }
  console.log('user_images: ' + (tests.length - failed) + '/' + tests.length + ' 通过');
  if (failed) process.exit(1);
})();
