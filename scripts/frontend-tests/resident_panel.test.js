// 机器人面板（📡）的引擎显隐回归测试：
//   - 地址名 / 主 Agent 对所有引擎开放（A2A 总线是引擎中立的），所以 📡 按钮
//     不再按引擎隐藏；
//   - 「常驻进程」开关行只在 claude 显示，其他引擎隐藏并给出 A2A 说明。
//
// 局限：jsdom 不做布局，面板长什么样仍需人工过一眼；这里钉的是 class 显隐与文案。
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

const HTML = `<!doctype html><html><body>
  <button id="btn-resident"></button>
  <div id="resident-pop" class="popover-panel">
    <label class="rp-row rp-toggle" id="resident-toggle-row">
      <input type="checkbox" id="resident-toggle">
    </label>
    <input id="resident-name">
    <div id="resident-hint"></div>
    <div id="resident-status"></div>
  </div>
</body></html>`;

const dom = new JSDOM(HTML, { runScripts: 'outside-only' });
const w = dom.window;
const doc = w.document;

w.eval(`
const $ = (sel) => document.querySelector(sel);
const state = { current: null, sessions: [] };
function updateMainAgentUI() {}
${grabFn('updateResidentUI')}
${grabFn('setEngineUI')}
window.__state = state;
window.__x = { updateResidentUI, setEngineUI };
`);

const state = w.__state;
const X = w.__x;

let failed = 0;
function check(name, cond, extra) {
  if (cond) console.log('  ok  ' + name);
  else { failed++; console.log('  FAIL ' + name + (extra ? '  →  ' + extra : '')); }
}

function main() {
  console.log('机器人面板引擎显隐');

  // claude：常驻开关行可见，状态讲常驻。
  state.current = { engine: 'claude', resident: false, name: 'x', status: {} };
  X.updateResidentUI();
  check('claude：常驻开关行可见', !doc.querySelector('#resident-toggle-row').classList.contains('hidden'));

  // opencode：常驻开关行隐藏，状态显示地址名，hint 提到 A2A。
  state.current = { engine: 'opencode', name: 'oc-1', peer_name: 'mainAgent', status: {} };
  X.updateResidentUI();
  check('opencode：常驻开关行隐藏', doc.querySelector('#resident-toggle-row').classList.contains('hidden'));
  check('opencode：状态显示地址名', doc.querySelector('#resident-status').textContent.includes('mainAgent'),
    doc.querySelector('#resident-status').textContent);
  check('opencode：hint 说明 A2A', doc.querySelector('#resident-hint').textContent.includes('A2A'));

  // 未设地址名的 opencode：状态提示无法寻址。
  state.current = { engine: 'opencode', name: '', status: {} };
  X.updateResidentUI();
  check('opencode：未设地址名时提示无法寻址',
    doc.querySelector('#resident-status').textContent.includes('未设地址名'),
    doc.querySelector('#resident-status').textContent);

  // 📡 按钮对所有引擎都显示。
  const btn = doc.querySelector('#btn-resident');
  btn.classList.add('hidden');
  X.setEngineUI('opencode');
  check('opencode：📡 按钮可见', !btn.classList.contains('hidden'));
  btn.classList.add('hidden');
  X.setEngineUI('claude');
  check('claude：📡 按钮可见', !btn.classList.contains('hidden'));

  console.log(failed ? `\n${failed} 项失败` : '\n全部通过');
  process.exit(failed ? 1 : 0);
}

main();
