// 升级说明弹窗（#modal-whats-new）的触发契约。
//
// 老版本升级上来时侧栏的主 Agent 行没了，第一次打开容易以为会话丢了，所以升级后
// 首次启动弹一次说明；全新安装不该被弹（那是用户第一次见这个产品）。判定完全在
// 前端 localStorage：
//   - 没有 cc-webui:whats-new 且存在任意其它 cc-webui* 键 → 老用户，弹一次；
//   - 什么都没有 → 新装，只写标记；
//   - 标记已是当前 changelog id → 不再弹；
//   - 标记是旧 id（下一次更新）→ 再弹一次。
//
// 局限：jsdom 不做布局，弹窗长什么样验不了；这里钉「谁该看到」和 index.html/CSS
// 里的关键文案与关闭入口。
const fs = require('fs');
const path = require('path');
const { JSDOM } = require('jsdom');

const WEB = path.resolve(__dirname, '../../web');
const src = fs.readFileSync(path.join(WEB, 'app.js'), 'utf8');
const htmlSrc = fs.readFileSync(path.join(WEB, 'index.html'), 'utf8');
const cssSrc = fs.readFileSync(path.join(WEB, 'style.css'), 'utf8');

function grabFn(name) {
  const s = src.indexOf('function ' + name + '(');
  if (s < 0) throw new Error('找不到函数 ' + name);
  const start = src.slice(Math.max(0, s - 6), s) === 'async ' ? s - 6 : s;
  let i = src.indexOf('(', s), pd = 0;
  for (; i < src.length; i++) {
    const c = src[i];
    if (c === '(') pd++;
    else if (c === ')') { pd--; if (!pd) { i++; break; } }
  }
  const b = src.indexOf('{', i);
  let d = 0, q = null;
  for (let i = b; i < src.length; i++) {
    const c = src[i], p = src[i - 1];
    if (q) { if (c === q && p !== '\\') q = null; continue; }
    if (c === '"' || c === "'" || c === '`') { q = c; continue; }
    if (c === '{') d++;
    else if (c === '}') { d--; if (!d) return src.slice(start, i + 1); }
  }
  throw new Error(name + ' 括号不配对');
}

function grabLine(needle) {
  const i = src.indexOf(needle);
  if (i < 0) throw new Error('找不到: ' + needle);
  const end = src.indexOf('\n', i);
  return src.slice(i, end).trim();
}

const setup = `
const $ = (sel) => document.querySelector(sel);
${grabLine('const WHATS_NEW_ID')}
${grabFn('maybeShowWhatsNew')}
${grabFn('closeWhatsNew')}
window.__x = { maybeShowWhatsNew, closeWhatsNew, WHATS_NEW_ID };
`;

const dom = new JSDOM(`<!doctype html><html><body>
  <div id="modal-whats-new" class="modal hidden">
    <button id="whats-new-close">知道了</button>
  </div>
</body></html>`, { runScripts: 'outside-only', url: 'http://localhost/' });
const w = dom.window;
w.eval(setup);
const X = w.__x;
const modal = () => w.document.querySelector('#modal-whats-new');

let failed = 0;
function check(name, cond, extra) {
  if (cond) console.log('  ok  ' + name);
  else { failed++; console.log('  FAIL ' + name + (extra ? '  →  ' + extra : '')); }
}
const reset = () => { w.localStorage.clear(); modal().classList.add('hidden'); };

console.log('全新安装不弹');

reset();
X.maybeShowWhatsNew();
check('没有历史键 → 不弹', modal().classList.contains('hidden'));
check('仍写入已看标记', w.localStorage.getItem('cc-webui:whats-new') === X.WHATS_NEW_ID,
  w.localStorage.getItem('cc-webui:whats-new'));

console.log('老用户升级弹一次');

reset();
w.localStorage.setItem('cc-webui-running-snapshot', '{"s1":true}');
X.maybeShowWhatsNew();
check('有历史 cc-webui 键 → 弹出', !modal().classList.contains('hidden'));
check('弹过后写当前 id', w.localStorage.getItem('cc-webui:whats-new') === X.WHATS_NEW_ID);
modal().classList.add('hidden');

console.log('已看过 / 更新 id');

reset();
w.localStorage.setItem('cc-webui:whats-new', X.WHATS_NEW_ID);
w.localStorage.setItem('cc-webui-view', '{}');
X.maybeShowWhatsNew();
check('同一 changelog id → 不再弹', modal().classList.contains('hidden'));

reset();
w.localStorage.setItem('cc-webui:whats-new', '2026-01-old-update');
w.localStorage.setItem('cc-webui-view', '{}');
X.maybeShowWhatsNew();
check('旧 changelog id → 新一次更新再弹', !modal().classList.contains('hidden'));
check('并更新为当前 id', w.localStorage.getItem('cc-webui:whats-new') === X.WHATS_NEW_ID);

console.log('关闭与 boot 顺序');

X.closeWhatsNew();
check('closeWhatsNew 隐藏弹窗', modal().classList.contains('hidden'));

const bootIdx = src.indexOf('(async function boot()');
const callIdx = src.indexOf('maybeShowWhatsNew();', bootIdx);
const authIdx = src.indexOf("await api('/api/me')", bootIdx);
check('boot 最先执行升级判定（早于 running-snapshot / auth）',
  bootIdx >= 0 && callIdx > bootIdx && authIdx > callIdx);

console.log('index.html / CSS 契约');

check('index.html 有升级弹窗', /id="modal-whats-new"/.test(htmlSrc));
check('弹窗有 role="dialog"', /id="modal-whats-new"[^>]*role="dialog"/.test(htmlSrc));
check('文案说明主 Agent 移到顶部', htmlSrc.includes('主 Agent 移到顶部') && htmlSrc.includes('从会话列表换到了顶部头像'));
check('文案提到 Worker 按目录归组', htmlSrc.includes('Worker 按各自的工作目录归组'));
check('文案提到机器人图标', htmlSrc.includes('Worker 名字前会有一个机器人图标'));
check('有关闭按钮', /id="whats-new-close"/.test(htmlSrc));
check('CSS 有弹窗卡片与列表样式',
  /\.whats-new-card\s*\{[^}]*width:\s*580px/.test(cssSrc) && /\.wn-list\s*\{/.test(cssSrc));

if (failed) { console.log(`\n${failed} 项失败`); process.exit(1); }
console.log('\n全部通过');
