// opencode 实时路径空气泡 / 工具卡丢失的回归测试。
//
// 现象（用户反馈）：opencode 会话里出现一串只有时间戳、没有正文的 CLAUDE 空气泡，
// 工具卡也不见；刷新页面后又恢复正常——说明回放路径没问题，是实时（流式）路径。
//
// 根因（用真实事件流实测定位，见下）：
//   ① opencode 每一步会在 reasoning 与工具之间吐一个纯空白的 text part（实测正文
//      就是 "\n\n"）。它的 delta 一来 renderOcTextDelta 就建了流式气泡，随后完整
//      text 块被 renderEvent 的「空正文过滤」跳过，气泡永远没人收尾 → 空气泡。
//   ② 真实事件顺序是 text delta → thinking 块 → tool_use → **text 结束**。也就是
//      工具卡/thinking 会在正文收尾之前由 wrapInMsg 挂进那个 .bubble，而正文收尾
//      是整段 innerHTML 替换 .bubble → 把刚挂进去的工具卡一起擦掉。
//   ③ 完整 thinking 块走 wrapInMsg「挂到最近一条 assistant 消息」，而正文气泡插在
//      中间，原消息里的 live thinking 被摘走后剩一个空气泡。
//
// 这里用 renderEvent 的真实代码喂入的真实顺序事件，钉住：
//   ① 纯空白 text part 不残留空气泡、不留 .streaming；
//   ② 同一步的工具卡 / thinking 都还在（没被正文收尾擦掉）；
//   ③ 有正文的 part 后面跟工具卡：正文与工具卡都在、正文不重复；
//   ④ 多步连续空白 part 也不累积空气泡，工具卡/thinking 逐条都在；
//   ⑤ 纯空白前缀（"\n\nHello"）在出现正文后照常建气泡。
//
// jsdom 不做布局：气泡高度/光标动画验不了，仍需人工过一眼。
const fs = require('fs');
const path = require('path');
const assert = require('assert');
const { JSDOM } = require('jsdom');

const WEB = path.resolve(__dirname, '../../web');
const src = fs.readFileSync(path.join(WEB, 'app.js'), 'utf8');

// 按大括号配对原样摘出一个顶层函数——执行的是真代码，不是抄的副本。
// 扫描时跳过行注释 / 块注释 / 字符串（renderEvent 的英文注释里有撇号，如
// "ducc's …"，不跳注释会把引号状态带偏）；从函数起点开始扫，所以文件里别处
// 的正则字面量不影响。renderEvent 自身不含正则字面量，故无需识别正则。
function grabFn(name) {
  let s = src.indexOf('function ' + name + '(');
  if (s < 0) throw new Error('找不到函数 ' + name);
  if (src.slice(Math.max(0, s - 6), s) === 'async ') s -= 6;
  let i = s;
  function skip() {
    const c = src[i];
    if (c === '/' && src[i + 1] === '/') {
      const nl = src.indexOf('\n', i);
      i = nl < 0 ? src.length : nl;
      return true;
    }
    if (c === '/' && src[i + 1] === '*') {
      const e = src.indexOf('*/', i + 2);
      i = e < 0 ? src.length : e + 2;
      return true;
    }
    if (c === '"' || c === "'" || c === '`') {
      const q = c; i++;
      while (i < src.length) {
        if (src[i] === '\\') { i += 2; continue; }
        if (src[i] === q) { i++; break; }
        i++;
      }
      return true;
    }
    return false;
  }
  let params = 0, paramsEnd = -1;
  i = src.indexOf('(', s);
  for (; i < src.length; i++) {
    if (skip()) continue;
    if (src[i] === '(') params++;
    else if (src[i] === ')') { params--; if (!params) { paramsEnd = i; break; } }
  }
  i = paramsEnd;
  let b = -1;
  for (; i < src.length; i++) { if (skip()) continue; if (src[i] === '{') { b = i; break; } }
  let d = 0;
  for (i = b; i < src.length; i++) {
    if (skip()) continue;
    const c = src[i];
    if (c === '{') d++;
    else if (c === '}') { d--; if (!d) return src.slice(s, i + 1); }
  }
  throw new Error(name + ' 括号不配对');
}

const setup = `
const state = {
  live: null,
  ocBubble: null, ocPartID: '', ocBuf: '',
  toolBlocks: {}, toolTimers: {}, agentToolMeta: {}, subagentCards: {},
  current: { id: 's1' },
  renderingHistory: false,
};
function $(sel){ return document.getElementById(String(sel).replace(/^#/, '')) || null; }
function md(t){ return '<md>' + String(t) + '</md>'; }
function escapeHtml(s){ return String(s); }
function formatMsgTime(ts){ return ts ? 'T' : ''; }
function scrollBottom(){}
function renderIcons(){}
function refreshPinnedPrompt(){}
function addRewindBtn(){}
function renderUserImages(){}
function renderSubagentEvent(){}
// 真实实现把结果挂进已存在的工具卡（同在一条消息里），这里保留这条关键行为，
// 否则 wrapInMsg 会以为消息断了、下一步另起气泡，掩盖真实的 DOM 结构。
function renderToolResult(block, box, raw){
  const card = state.toolBlocks[block.tool_use_id];
  const d = document.createElement('div'); d.className = 'tool-result';
  if (card) { card.querySelector('.tool-body').appendChild(d); return; }
  (raw || box).appendChild(d);
}
function renderToolUse(block, box, ts, raw){
  const details = document.createElement('details');
  details.className = 'tool-block';
  details.dataset.tool = block.name || '';
  const body = document.createElement('div'); body.className = 'tool-body';
  details.appendChild(body);
  if (raw) box.appendChild(details); else wrapInMsg(details, box, ts);
  if (block.id) state.toolBlocks[block.id] = details;
}
function updateStatusUI(){}
function applySession(){}
function goalToast(){}
function setErrorBanner(){}
function wsRefreshStatus(){}
function flushPending(){}
function refreshCurrentStatus(){}
${grabFn('addMsg')}
${grabFn('wrapInMsg')}
${grabFn('liveBlock')}
${grabFn('livePush')}
${grabFn('liveFinalizeBlock')}
${grabFn('livePaint')}
${grabFn('liveSchedulePaint')}
${grabFn('liveTake')}
${grabFn('liveFinalizeAll')}
${grabFn('liveThinkingBlock')}
${grabFn('ocTextBubble')}
${grabFn('renderOcTextDelta')}
${grabFn('renderOcReasoningDelta')}
${grabFn('renderEvent')}
window.__x = { state, renderEvent };
`;

const dom = new JSDOM('<!doctype html><html><body><div id="messages"></div></body></html>',
  { runScripts: 'outside-only', pretendToBeVisual: true });
dom.window.eval(setup);
const x = dom.window.__x;
const doc = dom.window.document;

const tests = [];
const test = (name, fn) => tests.push({ name, fn });

const ev = (data) => ({ kind: 'stream', data, ts: 1 });
const delta = (partID, d) => ev({ type: 'oc_text_delta', partID, delta: d });
const rdelta = (partID, d) => ev({ type: 'oc_reasoning_delta', partID, delta: d });
const asst = (msgID, blocks) => ev({ type: 'assistant', message: { id: msgID, role: 'assistant', content: blocks } });

function freshBox() {
  x.state.live = null;
  x.state.ocBubble = null; x.state.ocPartID = ''; x.state.ocBuf = '';
  x.state.toolBlocks = {};
  const box = doc.createElement('div');
  doc.getElementById('messages').appendChild(box);
  return box;
}
const bubbleOf = (m) => m.querySelector('.bubble');
const isEmptyBubble = (m) => {
  const b = bubbleOf(m);
  return (b.textContent || '').trim() === '' && !b.querySelector('details, .tool-result');
};
const emptyBubbles = (box) => Array.from(box.querySelectorAll('.msg-assistant')).filter(isEmptyBubble);

// 一个完整的 opencode v2 步骤，**顺序照真实事件流**（实测 2026-11-11）：
//   reasoning 增量 → text 增量 → 完整 thinking 块 → tool_use → 完整 text 块
//   → 工具结果 → usage 携带事件。
// text 缺省为空白的 "\n\n"（真实值）；有工具卡时完整 text 块排在 tool_use 之后。
function runOcStep(box, msgID, opts) {
  opts = opts || {};
  const text = opts.text !== undefined ? opts.text : '\n\n';
  const callId = 'call_' + msgID;
  x.renderEvent(rdelta(msgID + ':reasoning-0', '思考一'), box);
  x.renderEvent(rdelta(msgID + ':reasoning-0', '思考二'), box);
  x.renderEvent(delta(msgID + ':text-0', text), box);
  x.renderEvent(asst(msgID, [{ type: 'thinking', id: msgID + ':reasoning-0', thinking: '思考一思考二' }]), box);
  if (opts.tool !== false) {
    x.renderEvent(asst(msgID, [{ type: 'tool_use', id: callId, name: 'Read', input: { file_path: '/tmp/x' } }]), box);
  }
  x.renderEvent(asst(msgID, [{ type: 'text', id: msgID + ':text-0', text }]), box);
  if (opts.tool !== false) {
    x.renderEvent(ev({ type: 'user', message: { role: 'user', content: [{ type: 'tool_result', tool_use_id: callId, content: [{ type: 'text', text: 'ok' }] }] } }), box);
  }
  x.renderEvent(asst(msgID, []), box); // usage 携带事件（content 为空数组）
}

test('纯空白 text part 不留空气泡', () => {
  const box = freshBox();
  runOcStep(box, 'msg_a');
  assert.strictEqual(x.state.ocBubble, null, '收尾后不应有悬挂的 ocBubble');
  assert.strictEqual(emptyBubbles(box).length, 0, '不应残留空气泡');
  assert.strictEqual(box.querySelectorAll('.streaming').length, 0, '不应残留流式光标');
});

test('纯空白 text part 之后 thinking 与工具卡都还在（没被正文收尾擦掉）', () => {
  const box = freshBox();
  runOcStep(box, 'msg_a');
  assert.strictEqual(box.querySelectorAll('details.tool-block[data-tool="Read"]').length, 1, '工具卡不能被擦掉');
  assert.strictEqual(box.querySelectorAll('details.thinking-block').length, 1, 'thinking 块不能丢');
  const card = box.querySelector('details.tool-block[data-tool="Read"]');
  assert(card.querySelector('.tool-result'), '工具结果应挂进工具卡');
});

test('有正文的 part 后面跟工具卡：正文与工具卡都在，且不重复', () => {
  const box = freshBox();
  runOcStep(box, 'msg_real', { text: '我先看下文件。' });
  const msgs = Array.from(box.querySelectorAll('.msg-assistant'));
  assert.strictEqual(emptyBubbles(box).length, 0, '不应有空气泡');
  // 正文出现且只出现一次
  const textHits = msgs.filter((m) => (bubbleOf(m).textContent || '').includes('我先看下文件')).length;
  assert.strictEqual(textHits, 1, '正文应恰好渲染一次');
  assert.strictEqual(box.querySelectorAll('details.tool-block[data-tool="Read"]').length, 1, '工具卡要在');
  assert.strictEqual(x.state.ocBubble, null, 'ocBubble 应被完整块替换后清空');
});

test('连续多步纯空白 part 也不累积空气泡，工具卡逐条都在', () => {
  const box = freshBox();
  runOcStep(box, 'msg_a');
  runOcStep(box, 'msg_b');
  runOcStep(box, 'msg_c');
  assert.strictEqual(x.state.ocBubble, null, '多步后不应有悬挂的 ocBubble');
  assert.strictEqual(emptyBubbles(box).length, 0, '多步后仍不应有空气泡');
  const cards = box.querySelectorAll('details.tool-block[data-tool="Read"]');
  assert.strictEqual(cards.length, 3, '三个工具卡都在');
  assert.strictEqual(box.querySelectorAll('details.thinking-block').length, 3, '三个 thinking 块都在');
});

test('纯空白前缀的中途 part（\n\nHello）在出现正文后照常建气泡', () => {
  const box = freshBox();
  const msgID = 'msg_pref';
  x.renderEvent(delta(msgID + ':text-0', '\n\n'), box);
  assert.strictEqual(x.state.ocBubble, null, '纯空白前缀不建气泡');
  x.renderEvent(delta(msgID + ':text-0', 'Hello'), box);
  assert(x.state.ocBubble, '出现正文后应建气泡');
  x.renderEvent(asst(msgID, [{ type: 'text', id: msgID + ':text-0', text: '\n\nHello' }]), box);
  assert.strictEqual(x.state.ocBubble, null, '完整块收尾后清空');
  const msgs = Array.from(box.querySelectorAll('.msg-assistant'));
  assert.strictEqual(msgs.length, 1, '只应有一个气泡');
  assert((bubbleOf(msgs[0]).textContent || '').includes('Hello'));
  assert.strictEqual(emptyBubbles(box).length, 0, '不应有空气泡');
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
  console.log('oc_empty_bubble: ' + (tests.length - failed) + '/' + tests.length + ' 通过');
  if (failed) process.exit(1);
})();
