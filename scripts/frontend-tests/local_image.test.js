// web/app.js 里「模型回复里的本地图片」的路径重写行为的回归测试。
//
// 背景：模型在回复里写 ![](/abs/path/x.png) 时，浏览器会把它当站点相对 URL 去
// 请求 http://host/abs/path/x.png —— cc-webui 的静态服务只提供内嵌的 web/，
// 一律 404，图就裂在消息里（用户报「模型发的截图前端看不到」）。md() 里的
// fixLocalImages 把本地路径改写成 /api/image?path=…，后端限会话工作区内。
//
// 这个测试钉的是「哪些 src 该改写、哪些该原样保留」——改错了有两种后果：
// 把外链改掉（远程图挂掉），或者把不该暴露的路径指到本地文件接口上。
const fs = require('fs');
const path = require('path');
const assert = require('assert');
const { JSDOM } = require('jsdom');

const WEB = path.resolve(__dirname, '../../web');
const src = fs.readFileSync(path.join(WEB, 'app.js'), 'utf8');

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

const setup = `
const $ = (sel) => document.querySelector(sel);
const state = { current: { cwd: '/ws/project' } };
${grabFn('parseFileLink')}
${grabFn('fixLocalImages')}
window.__x = { parseFileLink, fixLocalImages, state };
`;

const dom = new JSDOM('<!doctype html><html><body></body></html>',
  { runScripts: 'outside-only', pretendToBeVisual: true });
const win = dom.window;
win.eval(fs.readFileSync(path.join(WEB, 'vendor', 'marked.min.js'), 'utf8'));
win.eval(setup);
const { fixLocalImages } = win.__x;

// 走真实的 markdown 解析：模型写的是 markdown，不是一个手搓的 <img>。
function render(md) {
  const tpl = win.document.createElement('template');
  tpl.innerHTML = win.marked.parse(md);
  fixLocalImages(tpl);
  return tpl.content.querySelector('img');
}

const tests = [];
const test = (name, fn) => tests.push({ name, fn });

test('绝对路径的图片被改写成 /api/image', () => {
  const img = render('![截图](/ws/project/docs/shot.png)');
  assert.ok(img, '应有 img');
  assert.strictEqual(img.getAttribute('src'),
    '/api/image?path=' + encodeURIComponent('/ws/project/docs/shot.png'));
  assert.ok(img.classList.contains('local-img'), '应打上 local-img 标记');
});

test('相对路径按当前会话 cwd 解析', () => {
  const img = render('![截图](docs/shot.png)');
  assert.strictEqual(img.getAttribute('src'),
    '/api/image?path=' + encodeURIComponent('/ws/project/docs/shot.png'));
});

test('中文与空格的路径被正确 URL 编码', () => {
  // markdown 链接目标里不能有裸空格（会被截断），所以带空格的路径有两种合法写法：
  // 尖括号包裹，或 %20 预编码。两种都要能还原成真实路径。
  for (const written of [
    '![旧版](</ws/project/docs/旧版-盖住 对话.png>)',
    '![旧版](/ws/project/docs/%E6%97%A7%E7%89%88-%E7%9B%96%E4%BD%8F%20%E5%AF%B9%E8%AF%9D.png)',
  ]) {
    const img = render(written);
    assert.ok(img, '应有 img: ' + written);
    const src = img.getAttribute('src');
    assert.ok(src.startsWith('/api/image?path='), src);
    // 编码后不能残留裸的中文/空格，否则 query 会被截断
    assert.ok(!/[\s一-龥]/.test(src.slice('/api/image?path='.length)),
      'query 部分不该有未编码字符: ' + src);
    // 反解回来必须还是原路径
    const back = decodeURIComponent(src.slice('/api/image?path='.length));
    assert.strictEqual(back, '/ws/project/docs/旧版-盖住 对话.png');
  }
});

test('data: / http(s): / blob: 的图片原样保留', () => {
  for (const u of ['data:image/png;base64,AAAA', 'https://x.com/a.png', 'http://x.com/a.png', 'blob:abc']) {
    const img = render('![i](' + u + ')');
    assert.strictEqual(img.getAttribute('src'), u, u + ' 不该被改写');
    assert.ok(!img.classList.contains('local-img'), u + ' 不该打 local-img');
  }
});

test('已经是应用内路径的图片不动（别把 /api/… 再包一层）', () => {
  const img = render('![i](/api/image?path=%2Fws%2Fproject%2Fa.png)');
  assert.strictEqual(img.getAttribute('src'), '/api/image?path=%2Fws%2Fproject%2Fa.png');
});

test('file:// 前缀被剥掉后按绝对路径处理', () => {
  const img = render('![i](file:///ws/project/a.png)');
  assert.strictEqual(img.getAttribute('src'),
    '/api/image?path=' + encodeURIComponent('/ws/project/a.png'));
});

test('.. 开头的相对路径不改写（不能指到工作区外）', () => {
  const img = render('![i](../secret.png)');
  assert.strictEqual(img.getAttribute('src'), '../secret.png');
  assert.ok(!img.classList.contains('local-img'));
});

test('alt 缺省时补上路径，图片加载失败能看出是哪个文件', () => {
  const img = render('![](/ws/project/a.png)');
  assert.ok(/a\.png/.test(img.getAttribute('alt') || ''), 'alt 应包含路径');
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
console.log(`\nlocal_image: ${tests.length - failed}/${tests.length} 通过`);
if (failed) process.exit(1);
