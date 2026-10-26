// web/app.js 里「新建会话」弹窗的 effort（variants）下拉显隐回归测试。
//
// 为什么要住进仓库：2026-10-26 opencode 从 v1 迁到 v2 时，这块同时坏了两头——
// v1 的 `opencode models <provider> --verbose` 在 v2 上直接打帮助页，服务端拿不到
// variants，弹窗就把下拉藏了（旧二进制：新建对话没有 variants 选项框）；迁移到 v2
// 之后映射键换成了目录里的 `id` 字段，而 config 里的模型名有裸名和带 provider 前缀
// 两种形态，只查一种就会让整条 provider 的下拉空掉。
//
// 局限要说清楚：jsdom 不做布局，弹窗里下拉实际长什么样这里验不了，能验的是
// 「有没有 variants 就显不显」「键能不能两种形态都命中」「拉取失败会不会把下拉
// 藏死十分钟」——正是这次踩坑的那几层。
const fs = require('fs');
const path = require('path');
const { JSDOM } = require('jsdom');

const WEB = path.resolve(__dirname, '../../web');
const src = fs.readFileSync(path.join(WEB, 'app.js'), 'utf8');

function grabFn(name) {
  // async function 也认：漏掉 async 前缀会让函数体里的 await 直接语法错。
  const s = src.indexOf('function ' + name + '(');
  if (s < 0) throw new Error('找不到函数 ' + name);
  const isAsync = src.slice(Math.max(0, s - 6), s) === 'async ';
  const b = src.indexOf('{', s);
  let d = 0, q = null;
  for (let i = b; i < src.length; i++) {
    const c = src[i], p = src[i - 1];
    if (q) { if (c === q && p !== '\\') q = null; continue; }
    if (c === '"' || c === "'" || c === '`') { q = c; continue; }
    if (c === '{') d++;
    else if (c === '}') { d--; if (!d) return (isAsync ? 'async ' : '') + src.slice(s, i + 1); }
  }
  throw new Error(name + ' 括号不配对');
}

// 这几个是顶层 const 箭头函数，切不出「函数体」，按行原文摘出来用。
function grabLine(needle) {
  const i = src.indexOf(needle);
  if (i < 0) throw new Error('找不到: ' + needle);
  const end = src.indexOf('\n', i);
  return src.slice(i, end).trim();
}

const kindBlock = src.slice(src.indexOf('const kindOf'), src.indexOf('const provKind'));
const ttlLine = grabLine('const OC_VARIANTS_TTL');
const dollarLine = grabLine('const $ = (sel)');

// 新建会话弹窗里跟 effort 有关的最小 DOM：四个下拉 + 承载 effort 的 label。
const HTML = `<!doctype html><html><body>
  <label>CLI <select id="new-cli"></select></label>
  <label>Provider <select id="new-provider"></select></label>
  <label>Model <select id="new-model"></select></label>
  <label>推理
    <select id="new-effort">
      <option value="">继承</option>
      <option value="low">low</option>
      <option value="medium">medium</option>
      <option value="high">high</option>
      <option value="xhigh">xhigh</option>
      <option value="max">max</option>
    </select>
  </label>
</body></html>`;

const setup = `
${dollarLine}
const state = {
  ocVariants: {},
  clis: [{ name: 'opencode', kind: 'opencode' }, { name: 'claude', kind: 'claude' }],
};
${kindBlock}
${ttlLine}
const EFFORT_STATIC = {};
const EFFORT_CLAUDE_HTML = '';
const __apiCalls = [];
const api = async (p) => { __apiCalls.push(p); return window.__apiImpl(p); };
${grabFn('effortStaticHTML')}
${grabFn('applyEffortOptions')}
${grabFn('variantKeysFor')}
${grabFn('ocVariantsFor')}
${grabFn('refreshNewEffort')}
window.__x = {
  refreshNewEffort, variantKeysFor, calls: () => __apiCalls,
  clearCache: () => { state.ocVariants = {}; },
};
`;

const dom = new JSDOM(HTML, { runScripts: 'outside-only' });
const w = dom.window;
w.__apiImpl = async () => ({ models: {} });
w.eval(setup);

const $ = (s) => w.document.querySelector(s);
const sel = () => $('#new-effort');
const wrap = () => sel().closest('label');
const visible = () => !sel().classList.contains('hidden') && !wrap().classList.contains('hidden');
const optionValues = () => Array.from(sel().options).map((o) => o.value);

function setForm({ cli, provider, model }) {
  setSelectValue($('#new-cli'), cli);
  setSelectValue($('#new-provider'), provider);
  setSelectValue($('#new-model'), model);
}

// variants 映射有 10 分钟 TTL，用例之间不隔离就会互相串（上一个用例缓存的
// 映射被下一个用例当成命中），所以每个用例自己清一次。
const clearOcCache = () => w.__x.clearCache();

// 真页面里这些 select 由 renderXxxList 填满选项；这里按需补一个 option，
// 否则 select 存不住值（value 会被浏览器回写成 ''，下拉就串到 claude 分支去）。
function setSelectValue(sel, v) {
  if (!Array.from(sel.options).some((o) => o.value === v)) {
    const o = w.document.createElement('option');
    o.value = v;
    o.textContent = v;
    sel.appendChild(o);
  }
  sel.value = v;
}

let failed = 0;
function check(name, cond, extra) {
  if (cond) {
    console.log('  ok  ' + name);
  } else {
    failed++;
    console.log('  FAIL ' + name + (extra ? '  →  ' + extra : ''));
  }
}

(async () => {
  console.log('新建会话弹窗的 effort(variants) 下拉');

  // ① 有 variants → 下拉显示，选项 = 继承 + 变体键
  clearOcCache();
  w.__apiImpl = async () => ({
    models: { 'cline-pass/deepseek-v4.1-flash': ['none', 'low', 'high', 'max'] },
  });
  setForm({ cli: 'opencode', provider: 'cline-pass', model: 'cline-pass/deepseek-v4.1-flash' });
  await w.__x.refreshNewEffort();
  check('有 variants 时下拉显示', visible());
  check('选项是 继承 + 变体键', JSON.stringify(optionValues()) === JSON.stringify(['', 'none', 'low', 'high', 'max']),
    JSON.stringify(optionValues()));
  check('走的是 variants 接口且带 fresh=1',
    w.__x.calls().some((p) => p.includes('/api/opencode-model-variants') && p.includes('cli=opencode') && p.includes('provider=cline-pass') && p.includes('fresh=1')),
    JSON.stringify(w.__x.calls()));

  // ② config 里的模型名是裸 modelID，映射键是 provider/model 全路径 → 也要命中
  clearOcCache();
  w.__apiImpl = async () => ({
    models: { 'cline-pass/deepseek-v4.1-flash': ['none', 'low', 'high', 'max'] },
  });
  setForm({ cli: 'opencode', provider: 'cline-pass', model: 'deepseek-v4.1-flash' });
  await w.__x.refreshNewEffort();
  check('裸模型名也能命中全路径键', visible(), JSON.stringify(optionValues()));

  // ③ 模型没有 variants → 下拉隐藏
  clearOcCache();
  w.__apiImpl = async () => ({ models: { 'cline-pass/qwen3.7-plus': [] } });
  setForm({ cli: 'opencode', provider: 'cline-pass', model: 'cline-pass/qwen3.7-plus' });
  await w.__x.refreshNewEffort();
  check('没有 variants 时下拉隐藏', !visible());

  // ④ 拉取失败 → 下拉隐藏，且不写缓存（不能把一次失败藏住下拉十分钟）
  clearOcCache();
  w.__apiImpl = async () => { throw new Error('boom'); };
  const before = w.__x.calls().length;
  setForm({ cli: 'opencode', provider: 'cline-pass', model: 'cline-pass/deepseek-v4.1-flash' });
  await w.__x.refreshNewEffort();
  check('拉取失败时下拉隐藏', !visible());
  check('拉取失败不抛异常', true);
  check('拉取失败会重试（不写死缓存）', w.__x.calls().length > before);

  // ⑤ 缓存命中：10 分钟内再取同一个 cli/provider 不重复打接口
  clearOcCache();
  w.__apiImpl = async () => ({
    models: { 'cline-pass/deepseek-v4.1-flash': ['none', 'low', 'high', 'max'] },
  });
  setForm({ cli: 'opencode', provider: 'cline-pass', model: 'cline-pass/deepseek-v4.1-flash' });
  await w.__x.refreshNewEffort();
  const n1 = w.__x.calls().length;
  await w.__x.refreshNewEffort();
  check('TTL 内不重复拉取', w.__x.calls().length === n1, `${n1} → ${w.__x.calls().length}`);

  // ⑥ claude 分支：静态全集，始终显示（别被 opencode 的空映射带跑）
  setForm({ cli: 'claude', provider: 'claude', model: 'claude-opus-4' });
  await w.__x.refreshNewEffort();
  check('claude 分支下拉仍然显示', visible());
  check('claude 分支保留静态选项', optionValues().includes('max'), JSON.stringify(optionValues()));

  console.log(failed ? `\n${failed} 项失败` : '\n全部通过');
  process.exit(failed ? 1 : 0);
})();
