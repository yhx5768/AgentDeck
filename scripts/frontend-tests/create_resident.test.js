// web/app.js 里「新建对话」常驻开关的引擎可见面回归测试。
//
// 常驻/peer messaging 是 claude 独有的协议能力（后端对其它 kind 的
// resident/peer_name 直接 400），所以两个新建入口（主 composer 视图
// #new-session-view 和 quick-create 弹窗 #modal-new）都只在 claude 引擎下
// 显示这个开关，切到别的引擎要把它藏起来、并把已填的勾选/地址名清掉，
// 否则用户勾了常驻切到 opencode，提交时后端 400 就白填了。
//
// 局限：jsdom 不做布局，开关长什么样验不了；能钉的是「哪个引擎显示/隐藏」
// 和「切换引擎时状态被清掉」这两层。
const fs = require('fs');
const path = require('path');
const { JSDOM } = require('jsdom');

const WEB = path.resolve(__dirname, '../../web');
const src = fs.readFileSync(path.join(WEB, 'app.js'), 'utf8');

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

function grabLine(needle) {
  const i = src.indexOf(needle);
  if (i < 0) throw new Error('找不到: ' + needle);
  const end = src.indexOf('\n', i);
  return src.slice(i, end).trim();
}

const dollarLine = grabLine('const $ = (sel)');

const HTML = `<!doctype html><html><body>
  <input id="ns-name" class="ns-name-input">
  <label id="ns-resident-wrap" class="ns-resident-toggle hidden">
    <input type="checkbox" id="ns-resident-check"><span>常驻</span>
  </label>
  <input id="ns-resident-name" class="ns-name-input hidden">
  <label id="new-resident-wrap" class="modal-check hidden">
    <span>常驻进程</span><input type="checkbox" id="new-resident">
  </label>
  <label id="new-peer-wrap" class="hidden">地址名 <input id="new-peer-name"></label>
</body></html>`;

const setup = `
${dollarLine}
function refreshNsEffort() {}
function refreshNewEffort() {}
${grabFn('applyNsEngineUI')}
${grabFn('applyNewEngineUI')}
window.__x = { applyNsEngineUI, applyNewEngineUI };
`;

const dom = new JSDOM(HTML, { runScripts: 'outside-only' });
const w = dom.window;
w.eval(setup);

const $ = (s) => w.document.querySelector(s);
const hidden = (sel) => $(sel).classList.contains('hidden');

let failed = 0;
function check(name, cond, extra) {
  if (cond) console.log('  ok  ' + name);
  else { failed++; console.log('  FAIL ' + name + (extra ? '  →  ' + extra : '')); }
}

console.log('新建对话常驻开关（claude-only）');

// ① composer 视图：claude 显示，其它引擎隐藏
w.__x.applyNsEngineUI('claude');
check('composer: claude 显示常驻开关', !hidden('#ns-resident-wrap'));
w.__x.applyNsEngineUI('opencode');
check('composer: opencode 隐藏常驻开关', hidden('#ns-resident-wrap'));
w.__x.applyNsEngineUI('qoder');
check('composer: qoder 隐藏常驻开关', hidden('#ns-resident-wrap'));

// ② 切走时把已填状态清掉（否则提交时后端 400 白填）
w.__x.applyNsEngineUI('claude');
$('#ns-resident-check').checked = true;
$('#ns-resident-name').value = '小王';
$('#ns-resident-name').classList.remove('hidden');
w.__x.applyNsEngineUI('opencode');
check('composer: 切走取消勾选', !$('#ns-resident-check').checked);
check('composer: 切走清空地址名', $('#ns-resident-name').value === '');
check('composer: 切走藏地址名输入', hidden('#ns-resident-name'));

// ③ quick-create 弹窗同上
w.__x.applyNewEngineUI('claude');
check('弹窗: claude 显示常驻开关', !hidden('#new-resident-wrap'));
w.__x.applyNewEngineUI('mcode');
check('弹窗: mcode 隐藏常驻开关', hidden('#new-resident-wrap'));
w.__x.applyNewEngineUI('claude');
$('#new-resident').checked = true;
$('#new-peer-name').value = 'alice';
$('#new-peer-wrap').classList.remove('hidden');
w.__x.applyNewEngineUI('codex');
check('弹窗: 切走取消勾选', !$('#new-resident').checked);
check('弹窗: 切走清空地址名', $('#new-peer-name').value === '');
check('弹窗: 切走藏地址名输入', hidden('#new-peer-wrap'));

console.log(failed ? `\n${failed} 项失败` : '\n全部通过');
process.exit(failed ? 1 : 0);
