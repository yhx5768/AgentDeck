// opencode 推理增量（oc_reasoning_delta）流式渲染的回归测试。
//
// v2 把 reasoning 和正文拆成两个事件命名空间（session.reasoning.* /
// session.text.*），所以推理增量可以安全地透出——此前完全丢弃，代价是模型思考的
// 那几分钟里前端一直空转（TUI 却能看到思考在跑）。它复用 claude thinking_delta
// 的同一套 live 块，完整 thinking 块到达时由 liveTake('thinking') 取出替换。
//
// 能钉住的是 DOM 契约：
//   ① 第一个 delta 建出展开的 thinking 块（details.thinking-block + streaming 光标）；
//   ② 同一 part 的后续 delta 追加进同一个块，不新起气泡；
//   ③ 空 delta 不建块；
//   ④ 完整 thinking 块到达时 liveTake('thinking') 取走该块，且只取一次；
//   ⑤ 上一个块被取走后，新 part 起新块；
//   ⑥ claude 的 content_block_start(thinking) 复用同一个 liveThinkingBlock，
//      保证两条引擎只有一套 thinking 收尾逻辑。
//
// jsdom 不做布局：块的高度、折叠手感验不了，仍需人工过一眼。
const fs = require('fs');
const path = require('path');
const assert = require('assert');
const { JSDOM } = require('jsdom');

const WEB = path.resolve(__dirname, '../../web');
const src = fs.readFileSync(path.join(WEB, 'app.js'), 'utf8');

// 按大括号配对原样摘出一个顶层函数——执行的是真代码，不是抄的副本。
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
const state = { live: null };
function scrollBottom() {}
function wrapInMsg(node, box, ts) {
  const div = document.createElement('div');
  div.className = 'msg-assistant';
  div.appendChild(node);
  (box || document.body).appendChild(div);
  return div;
}
${grabFn('liveBlock')}
${grabFn('livePush')}
${grabFn('liveFinalizeBlock')}
${grabFn('livePaint')}
${grabFn('liveSchedulePaint')}
${grabFn('liveTake')}
${grabFn('liveThinkingBlock')}
${grabFn('renderOcReasoningDelta')}
window.__x = {
  state, renderOcReasoningDelta, liveThinkingBlock, liveTake, liveFinalizeBlock,
  mk() { return document.createElement('div'); },
};
`;

const dom = new JSDOM('<!doctype html><html><body></body></html>',
  { runScripts: 'outside-only', pretendToBeVisual: true });
dom.window.eval(setup);
const x = dom.window.__x;

const tests = [];
const test = (name, fn) => tests.push({ name, fn });
const delta = (partID, d) => ({ type: 'oc_reasoning_delta', partID, delta: d });
const blocks = (box) => box.querySelectorAll('details.thinking-block');

test('第一个 delta 建出展开的 thinking 块', () => {
  x.state.live = null;
  const box = x.mk();
  x.renderOcReasoningDelta(delta('msg_a:reasoning-0', '17*23'), box, 't0');
  const cur = x.state.live && x.state.live.cur;
  assert(cur && cur.kind === 'thinking', 'live.cur 应是 thinking 块');
  const details = blocks(box);
  assert.strictEqual(details.length, 1);
  assert.strictEqual(details[0].open, true, '流式期间应展开');
  assert.strictEqual(details[0].querySelector('summary').textContent, 'thinking…');
  assert(details[0].querySelector('.tool-body').classList.contains('streaming'), '流式光标');
  assert.strictEqual(cur.buf, '17*23');
});

test('同一 part 的后续 delta 追加进同一个块', () => {
  x.state.live = null;
  const box = x.mk();
  x.renderOcReasoningDelta(delta('msg_a:reasoning-0', 'a'), box, 't0');
  x.renderOcReasoningDelta(delta('msg_a:reasoning-0', 'b'), box, 't0');
  assert.strictEqual(x.state.live.blocks.length, 1, '不应新起块');
  assert.strictEqual(x.state.live.cur.buf, 'ab');
  assert.strictEqual(blocks(box).length, 1);
});

test('空 delta 不建块', () => {
  x.state.live = null;
  const box = x.mk();
  x.renderOcReasoningDelta(delta('p', ''), box, 't0');
  x.renderOcReasoningDelta({ type: 'oc_reasoning_delta', partID: 'p' }, box, 't0');
  assert.strictEqual(x.state.live, null);
  assert.strictEqual(blocks(box).length, 0);
});

test('完整 thinking 块到达时 liveTake 取走该块，且只取一次', () => {
  x.state.live = null;
  const box = x.mk();
  x.renderOcReasoningDelta(delta('msg_a:reasoning-0', '思考中'), box, 't0');
  const live = x.state.live.cur;
  const taken = x.liveTake('thinking');
  assert.strictEqual(taken, live, 'liveTake 应取到刚建出的块');
  assert.strictEqual(live.taken, true);
  assert.strictEqual(x.liveTake('thinking'), null, '同一个块不能被取两次');
  // 真实路径里 renderEvent 会 remove 掉它并用完整 thinking 块重建；这里固化后
  // 内容应与累积的增量一致。
  x.liveFinalizeBlock(live);
  assert.strictEqual(live.bodyEl.textContent, '思考中');
});

test('上一个块被取走后，新 part 起新块', () => {
  x.state.live = null;
  const box = x.mk();
  x.renderOcReasoningDelta(delta('msg_a:reasoning-0', '一'), box, 't0');
  x.liveTake('thinking');
  x.renderOcReasoningDelta(delta('msg_a:reasoning-1', '二'), box, 't0');
  assert.strictEqual(x.state.live.blocks.length, 2);
  assert.strictEqual(x.state.live.cur.buf, '二');
  assert.strictEqual(x.state.live.cur.taken, false);
  assert.strictEqual(blocks(box).length, 2);
});

test('claude 的 content_block_start(thinking) 复用同一个 liveThinkingBlock', () => {
  assert(/cb\.type === 'thinking'\)\s*\{\s*liveThinkingBlock\(/.test(src),
    'renderStreamEvent 应复用 liveThinkingBlock，避免两套 thinking 收尾逻辑');
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
  console.log('oc_reasoning: ' + (tests.length - failed) + '/' + tests.length + ' 通过');
  if (failed) process.exit(1);
})();
