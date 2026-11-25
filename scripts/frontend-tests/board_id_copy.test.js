// 任务抽屉头部「卡号 + 复制按钮」回归。
//
// 需求：卡号后面加复制按钮，点击复制**完整卡号**（展示可能被截断），
// 复制成功/失败都要有就地反馈；不改右侧外部链接与关闭按钮；窄屏不被挤坏。
//
// jsdom 不做布局：宽度、省略号的实际裁剪、窄抽屉换行效果验不了；
// 这里钉的是 DOM 契约、复制参数（完整 id 而不是截断文本）、反馈状态与 CSS 契约。
const fs = require('fs');
const path = require('path');
const { JSDOM } = require('jsdom');

const WEB = path.resolve(__dirname, '../../web');
const appSrc = fs.readFileSync(path.join(WEB, 'app.js'), 'utf8');
const htmlSrc = fs.readFileSync(path.join(WEB, 'index.html'), 'utf8');
const cssSrc = fs.readFileSync(path.join(WEB, 'style.css'), 'utf8');

// 按大括号配对原样摘出顶层函数（处理转义、字符串、模板串、注释、正则）。
function grabFn(name) {
  const s = appSrc.indexOf('function ' + name + '(');
  if (s < 0) throw new Error('找不到函数 ' + name);
  const start = appSrc.slice(Math.max(0, s - 6), s) === 'async ' ? s - 6 : s;
  const b = appSrc.indexOf('{', s);
  let d = 0, q = null;
  for (let i = b; i < appSrc.length; i++) {
    const c = appSrc[i];
    if (q) {
      if (c === '\\') { i++; continue; }
      if (c === q) q = null;
      continue;
    }
    if (c === '"' || c === "'" || c === '`') { q = c; continue; }
    if (c === '/' && appSrc[i + 1] === '/') { while (i < appSrc.length && appSrc[i] !== '\n') i++; continue; }
    if (c === '/' && appSrc[i + 1] === '*') { const e = appSrc.indexOf('*/', i + 2); i = e < 0 ? appSrc.length : e + 1; continue; }
    if (c === '/') {
      const prev = appSrc.slice(b, i).trimEnd().slice(-1);
      if (prev === '' || '(,=:[!&|?{};+-*%^~<>'.includes(prev)) {
        let j = i + 1, cls = false;
        while (j < appSrc.length) {
          if (appSrc[j] === '\\') { j += 2; continue; }
          if (appSrc[j] === '[') cls = true; else if (appSrc[j] === ']') cls = false;
          else if (appSrc[j] === '/' && !cls) break; else if (appSrc[j] === '\n') break;
          j++;
        }
        i = j; continue;
      }
    }
    if (c === '{') d++;
    else if (c === '}') { d--; if (!d) return appSrc.slice(start, i + 1); }
  }
  throw new Error(name + ' 括号不配对');
}

const HTML = `<!doctype html><html><body>
  <div id="board-drawer">
    <div class="board-drawer-head">
      <div class="board-drawer-kicker">
        <span class="board-drawer-id-wrap">
          <span id="board-drawer-id"></span>
          <button id="board-drawer-copy-id" class="board-copy-btn board-drawer-id-copy hidden" type="button"
                  title="复制完整卡号" aria-label="复制完整卡号">复制</button>
        </span>
        <span id="board-drawer-project" class="board-project"></span>
      </div>
      <button id="board-drawer-close" class="icon-btn" title="关闭" aria-label="关闭">x</button>
    </div>
  </div>
</body></html>`;

const dom = new JSDOM(HTML, { url: 'http://localhost/', runScripts: 'outside-only' });
const w = dom.window;
const doc = w.document;

w.eval(`
window.__copied = [];
window.__copyMode = 'ok';
window.__timers = [];
window.__bubbled = 0;
window.setTimeout = (fn) => { window.__timers.push(fn); return window.__timers.length; };
window.clearTimeout = () => {};
const $ = (sel) => document.querySelector(sel);
function copyText(text) {
  window.__copied.push(String(text));
  return window.__copyMode === 'ok'
    ? Promise.resolve()
    : Promise.reject(new Error('copy failed'));
}
${grabFn('boardDrawerHeader')}
${grabFn('boardCopyTaskId')}
${grabFn('bindBoardDrawerCopy')}
window.__boardDrawerHeader = boardDrawerHeader;
window.__boardCopyTaskId = boardCopyTaskId;
window.__bindBoardDrawerCopy = bindBoardDrawerCopy;
`);

const copied = w.__copied;
const boardDrawerHeader = w.__boardDrawerHeader;
const boardCopyTaskId = w.__boardCopyTaskId;
const bindBoardDrawerCopy = w.__bindBoardDrawerCopy;
const idEl = doc.querySelector('#board-drawer-id');
const copyBtn = doc.querySelector('#board-drawer-copy-id');
const projectEl = doc.querySelector('#board-drawer-project');
const closeBtn = doc.querySelector('#board-drawer-close');

let failed = 0;
function check(name, cond, extra) {
  if (cond) console.log('  ok  ' + name);
  else { failed++; console.log('  FAIL ' + name + (extra ? '  →  ' + extra : '')); }
}

async function flush() { await Promise.resolve(); await Promise.resolve(); }

async function main() {
  console.log('任务抽屉卡号复制');

  const LONG_ID = '20261123-m145-experiment-group-metrics-dashboard-very-long-card-id';

  // ---- DOM 契约：按钮紧跟卡号，关闭按钮仍在右侧且没被改 ----
  check('复制按钮在 .board-drawer-id-wrap 内、紧跟卡号',
    copyBtn.parentElement === doc.querySelector('.board-drawer-id-wrap') &&
    copyBtn.previousElementSibling === idEl);
  check('卡号 + 复制按钮位于 kicker，项目 chip 仍是下一个兄弟',
    copyBtn.parentElement.parentElement === doc.querySelector('.board-drawer-kicker') &&
    projectEl.previousElementSibling === copyBtn.parentElement);
  check('关闭按钮仍在 head 里且未加复制按钮',
    closeBtn.parentElement === doc.querySelector('.board-drawer-head') &&
    !closeBtn.querySelector('#board-drawer-copy-id'));

  // ---- 长卡号：展示完整、复制值也是完整 id ----
  boardDrawerHeader({ id: LONG_ID, project: 'cc-webui' });
  check('卡号展示完整 id', idEl.textContent === LONG_ID, idEl.textContent);
  check('卡号 title 带完整 id', idEl.title === LONG_ID);
  check('复制按钮显示', !copyBtn.classList.contains('hidden'));
  check('复制按钮挂着完整 id', copyBtn.dataset.taskId === LONG_ID, copyBtn.dataset.taskId);
  check('按钮 tooltip/aria 带完整卡号',
    copyBtn.title.includes(LONG_ID) && copyBtn.getAttribute('aria-label').includes(LONG_ID));
  check('项目 chip 不受影响', projectEl.textContent === ''); // boardOpenDrawer 负责设置 project
  projectEl.textContent = 'cc-webui';

  // ---- 空卡号（本地任务）：按钮隐藏，显示兜底文案 ----
  boardDrawerHeader({ id: '', project: 'local' });
  check('无卡号时展示「本地任务」', idEl.textContent === '本地任务');
  check('无卡号时复制按钮隐藏', copyBtn.classList.contains('hidden'));
  check('无卡号时 dataset 为空', copyBtn.dataset.taskId === '');
  boardDrawerHeader({ id: LONG_ID, project: 'cc-webui' });

  // ---- 点击复制：复制完整 id，成功反馈后复位 ----
  bindBoardDrawerCopy();
  doc.querySelector('#board-drawer').addEventListener('click', () => { w.__bubbled++; });
  copyBtn.click();
  await flush();
  check('点击只触发一次复制', copied.length === 1, JSON.stringify(copied));
  check('复制的是完整卡号而不是展示文本', copied[0] === LONG_ID, copied[0]);
  check('点击不冒泡到抽屉（不误触其它动作）', w.__bubbled === 0);
  check('成功后就地显示「已复制」', copyBtn.textContent === '已复制', copyBtn.textContent);
  check('成功反馈带 copied 配色 class', copyBtn.classList.contains('copied'));
  check('反馈有复位定时器', w.__timers.length >= 1);
  w.__timers.shift()();
  check('定时器复位为「复制」', copyBtn.textContent === '复制' && !copyBtn.classList.contains('copied'));

  // ---- 失败反馈 ----
  w.__copyMode = 'fail';
  w.__timers = [];
  copied.length = 0;
  boardCopyTaskId(copyBtn);
  await flush();
  check('失败仍尝试复制完整卡号', copied[0] === LONG_ID);
  check('失败显示「复制失败」', copyBtn.textContent === '复制失败', copyBtn.textContent);
  check('失败反馈带 copy-failed 配色 class', copyBtn.classList.contains('copy-failed'));
  w.__timers.shift()();
  check('失败反馈也会复位', copyBtn.textContent === '复制' && !copyBtn.classList.contains('copy-failed'));
  w.__copyMode = 'ok';

  // ---- 空 id 直接点：不发复制调用 ----
  copied.length = 0;
  boardDrawerHeader({ id: '' });
  boardCopyTaskId(copyBtn);
  await flush();
  check('空卡号不发起复制', copied.length === 0);

  // ---- 重新打开另一张卡：复位反馈文案与 class ----
  boardDrawerHeader({ id: LONG_ID });
  copyBtn.textContent = '已复制';
  copyBtn.classList.add('copied');
  boardDrawerHeader({ id: 'T-2' });
  check('重开后按钮文案复位', copyBtn.textContent === '复制');
  check('重开后反馈 class 清掉', !copyBtn.classList.contains('copied') && !copyBtn.classList.contains('copy-failed'));
  check('重开后 dataset 换成新卡号', copyBtn.dataset.taskId === 'T-2');

  // ---- boardOpenDrawer 确实走统一头部逻辑（源码契约） ----
  const openMarker = appSrc.indexOf('function boardOpenDrawer(');
  const openBodyStart = appSrc.indexOf('{', openMarker);
  let d = 0, openBody = '';
  for (let i = openBodyStart; i < appSrc.length; i++) {
    if (appSrc[i] === '{') d++;
    else if (appSrc[i] === '}') { d--; if (!d) { openBody = appSrc.slice(openBodyStart, i + 1); break; } }
  }
  check('boardOpenDrawer 调用 boardDrawerHeader(task)', openBody.includes('boardDrawerHeader(task)'));
  check('初始化时绑定复制按钮', /bindBoardDrawerCopy\(\);\s*$/m.test(appSrc.trimEnd()) ||
    appSrc.includes('\nbindBoardDrawerCopy();'));
  check('index.html 的头部结构含复制按钮', /<span id="board-drawer-id"><\/span>\s*<button id="board-drawer-copy-id"/.test(htmlSrc));

  // ---- CSS 契约：长卡号截断但按钮不缩，窄屏可换行 ----
  check('kicker 允许换行且可收缩',
    /\.board-drawer-kicker\s*\{[^}]*flex-wrap:\s*wrap[^}]*min-width:\s*0[^}]*\}/.test(cssSrc));
  check('卡号容器可收缩、按钮不缩',
    /\.board-drawer-id-wrap\s*\{[^}]*min-width:\s*0[^}]*max-width:\s*100%[^}]*\}/.test(cssSrc) &&
    /\.board-drawer-id-copy\s*\{[^}]*flex:\s*none[^}]*\}/.test(cssSrc));
  check('长卡号单行省略（不撑破抽屉）',
    /#board-drawer-id\s*\{[^}]*min-width:\s*0[^}]*overflow:\s*hidden[^}]*text-overflow:\s*ellipsis[^}]*white-space:\s*nowrap[^}]*\}/.test(cssSrc));
  check('成功/失败反馈配色', /\.board-copy-btn\.copied\s*\{[^}]*var\(--green\)/.test(cssSrc) &&
    /\.board-copy-btn\.copy-failed\s*\{[^}]*var\(--red\)/.test(cssSrc));

  console.log(failed ? `\n${failed} 项失败` : '\n全部通过');
  process.exit(failed ? 1 : 0);
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
