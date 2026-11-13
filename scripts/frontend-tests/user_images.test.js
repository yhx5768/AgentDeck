// 用户消息图片渲染的回归测试。
//
// 图片消息的事件里只存文件名（见 runner.Message.ImageNames），图片本体存在会话
// 上传目录里，前端经 /api/sessions/{id}/uploads/{name} 取回。此前事件只存文本、
// 图片在服务端回推权威 user 事件后就被顶掉了（乐观气泡闪一下），刷新/切会话更是
// 彻底消失。这里钉住渲染契约：文件名 → 带状态与灯箱的图片按钮、路径按会话 id、
// 名字要 URL 编码、图片在上正文在下、没有图片/没有当前会话时不画。
const fs = require('fs');
const path = require('path');
const assert = require('assert');
const { JSDOM } = require('jsdom');

const WEB = path.resolve(__dirname, '../../web');
const src = fs.readFileSync(path.join(WEB, 'app.js'), 'utf8');
const css = fs.readFileSync(path.join(WEB, 'style.css'), 'utf8');

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
let userImgLightbox = null;
let userImgLightboxReturnFocus = null;
const USER_IMG_MAX_W = 360;
const USER_IMG_MAX_H = 340;
${grabFn('userImageSrc')}
${grabFn('sizeSingleUserImage')}
${grabFn('buildUserImage')}
${grabFn('appendUserImages')}
${grabFn('renderUserImages')}
${grabFn('appendUserContent')}
${grabFn('ensureUserImageLightbox')}
${grabFn('openUserImageLightbox')}
${grabFn('closeUserImageLightbox')}
window.__x = {
  state, renderUserImages, appendUserContent, openUserImageLightbox, closeUserImageLightbox,
  mk() { return document.createElement('div'); },
};
`;

const dom = new JSDOM('<!doctype html><html><body></body></html>',
  { runScripts: 'outside-only', pretendToBeVisual: true });
dom.window.eval(setup);
const x = dom.window.__x;
const doc = dom.window.document;

const tests = [];
const test = (name, fn) => tests.push({ name, fn });
const gallery = (bubble) => bubble.querySelector('.user-imgs');
const cells = (bubble) => [...bubble.querySelectorAll('.user-img')];
const imgs = (bubble) => [...bubble.querySelectorAll('.user-img img')];
const fire = (el, type) => el.dispatchEvent(new dom.window.Event(type));
const lightbox = () => doc.querySelector('.user-img-lightbox');

test('单图：一个图片按钮，指向会话上传接口，初始带 loading 骨架', () => {
  x.state.current = { id: 's1' };
  const b = x.mk();
  x.appendUserContent(b, '', ['image.png']);
  const box = gallery(b);
  assert.ok(box, '应有 .user-imgs 容器');
  assert.ok(box.classList.contains('n1'), '单图应标记 .n1');
  const cell = cells(b);
  assert.strictEqual(cell.length, 1);
  assert.strictEqual(cell[0].tagName, 'BUTTON', '图片应裹在 button 里（键盘可达）');
  assert.strictEqual(cell[0].getAttribute('type'), 'button');
  assert.ok(cell[0].classList.contains('loading'));
  assert.strictEqual(imgs(b)[0].getAttribute('src'), '/api/sessions/s1/uploads/image.png');
  assert.strictEqual(imgs(b)[0].className, 'thumb');
});

test('多图：等宽网格标记按张数（n3），顺序不变，文件名 URL 编码', () => {
  const b = x.mk();
  x.appendUserContent(b, '', ['a.png', 'b c.png', 'd/e.png']);
  const box = gallery(b);
  assert.ok(box.classList.contains('n3'));
  assert.ok(!box.classList.contains('n1'));
  const el = imgs(b);
  assert.strictEqual(el.length, 3);
  assert.strictEqual(el[1].getAttribute('src'), '/api/sessions/s1/uploads/b%20c.png');
  assert.strictEqual(el[2].getAttribute('src'), '/api/sessions/s1/uploads/d%2Fe.png');
  assert.deepStrictEqual(cells(b).map((c) => c.getAttribute('aria-label')),
    ['查看图片 1/3', '查看图片 2/3', '查看图片 3/3']);
});

test('图片在上、正文在下', () => {
  const b = x.mk();
  x.appendUserContent(b, '看这张截图', ['image.png']);
  assert.strictEqual(b.firstElementChild, gallery(b), '图片容器应在文本之前');
  assert.strictEqual(b.lastChild.nodeType, 3, '最后一个子节点应是文本');
  assert.strictEqual(b.lastChild.nodeValue, '看这张截图');
});

test('加载成功去掉骨架；失败进入 error 占位态', () => {
  const b = x.mk();
  x.appendUserContent(b, '', ['ok.png', 'bad.png']);
  const [ok, bad] = cells(b);
  fire(ok.querySelector('img'), 'load');
  assert.ok(!ok.classList.contains('loading'));
  assert.ok(!ok.classList.contains('error'));

  fire(bad.querySelector('img'), 'error');
  assert.ok(!bad.classList.contains('loading'));
  assert.ok(bad.classList.contains('error'));
  assert.strictEqual(bad.getAttribute('aria-label'), '图片加载失败');
});

test('单图加载后按上限落定行内宽度（避免内联块按原图撑开留白）', () => {
  const wide = x.mk();
  x.appendUserContent(wide, '', ['wide.png']);
  const wi = wide.querySelector('img');
  Object.defineProperty(wi, 'naturalWidth', { value: 880 });
  Object.defineProperty(wi, 'naturalHeight', { value: 560 });
  fire(wi, 'load');
  assert.strictEqual(wi.style.width, '360px');

  const tallB = x.mk();
  x.appendUserContent(tallB, '', ['tall.png']);
  const ti = tallB.querySelector('img');
  Object.defineProperty(ti, 'naturalWidth', { value: 480 });
  Object.defineProperty(ti, 'naturalHeight', { value: 1500 });
  fire(ti, 'load');
  assert.strictEqual(ti.style.width, '109px', '竖长图按高度上限反推宽度');

  // 多图网格不需要行内尺寸（CSS 用方块 + cover 裁切）
  const multi = x.mk();
  x.appendUserContent(multi, '', ['a.png', 'b.png']);
  const mi = multi.querySelector('img');
  Object.defineProperty(mi, 'naturalWidth', { value: 880 });
  Object.defineProperty(mi, 'naturalHeight', { value: 560 });
  fire(mi, 'load');
  assert.strictEqual(mi.style.width, '');
});

test('点击缩略图打开灯箱；灯箱图片用同一 src，焦点进关闭按钮', () => {
  const b = x.mk();
  x.appendUserContent(b, '', ['image.png']);
  const cell = cells(b)[0];
  cell.click();
  const mask = lightbox();
  assert.ok(mask, '应创建灯箱');
  assert.ok(!mask.classList.contains('hidden'));
  assert.strictEqual(mask.getAttribute('role'), 'dialog');
  assert.strictEqual(mask.querySelector('img').getAttribute('src'), '/api/sessions/s1/uploads/image.png');
  assert.strictEqual(doc.activeElement, mask.querySelector('.uib-close'));
});

test('Esc 关闭灯箱，焦点还给刚才那张缩略图', () => {
  const b = x.mk();
  doc.body.appendChild(b); // 焦点还原只对仍在文档里的节点生效（真实场景如此）
  x.appendUserContent(b, '', ['image.png']);
  const cell = cells(b)[0];
  cell.focus();
  cell.click();
  const mask = lightbox();
  const close = mask.querySelector('.uib-close');
  close.dispatchEvent(new dom.window.KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
  assert.ok(mask.classList.contains('hidden'));
  assert.strictEqual(doc.activeElement, cell, '关闭后焦点应回到缩略图');
  b.remove();
});

test('点遮罩关闭灯箱（点图片本身不关）', () => {
  const b = x.mk();
  x.appendUserContent(b, '', ['image.png']);
  cells(b)[0].click();
  const mask = lightbox();
  mask.querySelector('img').click();
  assert.ok(!mask.classList.contains('hidden'), '点图片不应关闭');
  mask.click();
  assert.ok(mask.classList.contains('hidden'));
});

test('加载失败的图点击不打开灯箱', () => {
  const b = x.mk();
  x.appendUserContent(b, '', ['bad.png']);
  const cell = cells(b)[0];
  fire(cell.querySelector('img'), 'error');
  cell.click();
  const mask = lightbox();
  assert.ok(!mask || mask.classList.contains('hidden'), '失败图不应打开灯箱');
});

test('没有图片 / 空数组不画', () => {
  const b = x.mk();
  x.appendUserContent(b, '', undefined);
  x.appendUserContent(b, '', []);
  assert.strictEqual(gallery(b), null);
});

test('没有当前会话时不画（避免拼出坏 URL）', () => {
  x.state.current = null;
  const b = x.mk();
  x.appendUserContent(b, '', ['image.png']);
  assert.strictEqual(gallery(b), null);
  x.state.current = { id: 's1' };
});

// jsdom 不做布局，下面只钉样式契约：网格、单图上限、状态样式、灯箱层级，
// 以及不碰 composer 附件 / 模型回复图片 / 新会话附件那三处既有样式。
test('样式契约：两列网格、单图限高、骨架/失败态、灯箱层级', () => {
  const grid = css.match(/\.msg-user \.bubble \.user-imgs\s*\{([\s\S]*?)\}/);
  assert.ok(grid, 'style.css 应有 .user-imgs 规则');
  assert.match(grid[1], /grid-template-columns:\s*repeat\(2,/, '多图应两列等宽');
  assert.match(grid[1], /gap:/, '图片之间应有统一间距');

  const single = css.match(/\.msg-user \.bubble \.user-imgs\.n1 \.user-img \.thumb\s*\{([\s\S]*?)\}/);
  assert.ok(single, '单图应有独立展示规则');
  assert.match(single[1], /max-height:\s*340px/, '单图应限高，竖长图不撑爆气泡');

  assert.match(css, /\.user-img\.loading::after\s*\{/, '应有加载骨架');
  assert.match(css, /\.user-img\.error\s*\{/, '应有失败占位态');

  const lb = css.match(/\.user-img-lightbox\s*\{([\s\S]*?)\}/);
  assert.ok(lb, '应有灯箱样式');
  assert.match(lb[1], /position:\s*fixed/);
  assert.match(lb[1], /z-index:\s*240/, '灯箱要盖在弹窗之上');

  // 既有三处图片样式保持原样（本次改动不得波及）
  assert.match(css, /\.attached-thumb\s*\{/);
  assert.match(css, /\.local-img\s*\{/);
  assert.match(css, /\.ns-attached \.thumb\s*\{/);
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