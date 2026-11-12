// 「钉住最后一条输入」悬浮条显隐的回归测试。
//
// 用户反馈：主对话顶部的悬浮条盖住了「加载更多（还有 N 条）」胶囊，按钮点不到；
// 预期是滚回去看见那条用户输入之后悬浮条就该收起。
//
// 根因（headless Chrome 实测，见测试末尾的几何模拟）：
//   IntersectionObserver 只在「相交比例跨过阈值」时回调。从长回复底部一下把
//   滚动条拖到顶部（去点「加载更多」时最常见的动作），目标用户消息会从视口
//   上方直接跳到视口下方——两次都不相交、比例恒为 0，observer 一次都不会回调。
//   旧代码把 observer 回调当成唯一判定源，于是悬浮条停在 show 状态，正好浮在
//   #messages 顶部盖住「加载更多」。
//
// 修法：显隐改由 syncPinnedVisibility() 按实时 getBoundingClientRect 判定
// （完全滚出顶部才显示），observer 只当触发源，滚动监听按 rAF 节流兜底。
//
// jsdom 不做布局，几何值全部由测试桩给出；真实滚动/按钮点击仍需人工过一眼。

const fs = require('fs');
const path = require('path');
const assert = require('assert');
const { JSDOM } = require('jsdom');

const WEB = path.resolve(__dirname, '../../web');
const src = fs.readFileSync(path.join(WEB, 'app.js'), 'utf8');

// 按大括号配对原样摘出函数 / 顶层块——执行的是真代码，不是抄的副本。
// 扫描时跳过行注释 / 块注释 / 字符串（app.js 注释里有中文标点与撇号）。
function scanner(startAt) {
  let i = startAt;
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
  return {
    get i() { return i; },
    set i(v) { i = v; },
    skip,
    // 从当前位置（一个 '{' 之前的标记处）扫到配对的 '}'，返回 [start, end] 下标。
    blockEnd(from, openBraceAt) {
      let d = 0;
      i = openBraceAt;
      for (; i < src.length; i++) {
        if (skip()) continue;
        const c = src[i];
        if (c === '{') d++;
        else if (c === '}') { d--; if (!d) return i; }
      }
      throw new Error('括号不配对（从 ' + from + ' 开始）');
    },
  };
}

function grabFn(name) {
  let s = src.indexOf('function ' + name + '(');
  if (s < 0) throw new Error('找不到函数 ' + name);
  if (src.slice(Math.max(0, s - 6), s) === 'async ') s -= 6;
  const sc = scanner(s);
  sc.i = src.indexOf('{', src.indexOf('(', s));
  const end = sc.blockEnd(name, sc.i);
  return src.slice(s, end + 1);
}

// 摘出「滚动监听」这整段顶层代码（含 let pinScrollRaf 声明），钉住接线本身。
function grabScrollWiring() {
  const marker = 'let pinScrollRaf = 0;';
  const s = src.indexOf(marker);
  if (s < 0) throw new Error('找不到滚动监听接线');
  const listenAt = src.indexOf("$('#messages').addEventListener('scroll'", s);
  if (listenAt < 0) throw new Error('找不到滚动监听注册');
  const sc = scanner(listenAt);
  const open = src.indexOf('{', listenAt);
  const end = sc.blockEnd('scroll listener', open);
  // 闭合 '}' 之后跟 ');'
  const tail = src.indexOf(');', end);
  return src.slice(s, tail + 2);
}

const setup = `
const state = { nearBottom: true };
const pinnedPrompt = { observer: null, target: null, expanded: false, fadeTimer: null };
function $(sel){ return document.querySelector(sel); }
function renderIcons(){}
function updatePpFade(){}
class FakeIO {
  constructor(cb, opts){ this.cb = cb; this.opts = opts; this.targets = []; FakeIO.instances.push(this); }
  observe(t){ this.targets.push(t); }
  disconnect(){ this.targets = []; }
  fire(entries){ this.cb(entries || []); }
}
FakeIO.instances = [];
const IntersectionObserver = FakeIO;
${grabFn('refreshPinnedPrompt')}
${grabFn('syncPinnedVisibility')}
${grabFn('hidePinnedPrompt')}
${grabFn('showPinnedPrompt')}
${grabFn('updatePpExpandBtn')}
${grabScrollWiring()}
window.__x = {
  state, pinnedPrompt, FakeIO,
  refreshPinnedPrompt, syncPinnedVisibility, hidePinnedPrompt, showPinnedPrompt,
};
`;

const dom = new JSDOM(`<!doctype html><html><body>
  <div id="chat-view">
    <div id="msg-wrap">
      <div id="pinned-prompt" class="hidden">
        <div class="pp-head"><span class="pp-tag">你</span>
          <button id="pp-expand" class="pp-expand icon-btn hidden"></button></div>
        <div class="pp-text" id="pp-text"></div>
        <div class="pp-fade hidden"></div>
      </div>
      <div id="messages"></div>
    </div>
  </div>
</body></html>`, { runScripts: 'outside-only', pretendToBeVisual: true });

dom.window.eval(setup);
const x = dom.window.__x;
const doc = dom.window.document;
const win = dom.window;
const pp = doc.getElementById('pinned-prompt');
const box = doc.getElementById('messages');
const text = doc.getElementById('pp-text');

// 几何桩：target 的上下边、滚动区顶边、gate 用的 offsetParent 全部可调。
const geo = {
  boxTop: 100,
  targetTop: -500,
  targetBottom: -400,
  msgWrapHidden: false,
  chatHidden: false,
};
const mw = doc.getElementById('msg-wrap');
const cv = doc.getElementById('chat-view');
Object.defineProperty(mw, 'offsetParent', { get: () => (geo.msgWrapHidden ? null : doc.body), configurable: true });
Object.defineProperty(cv, 'offsetParent', { get: () => (geo.chatHidden ? null : doc.body), configurable: true });
win.Element.prototype.getBoundingClientRect = function () {
  const r = (top, bottom) => ({ top, bottom, left: 0, right: 600, width: 600, height: bottom - top });
  if (this.id === 'messages') return r(geo.boxTop, geo.boxTop + 600);
  if (this.classList && this.classList.contains('msg-user')) return r(geo.targetTop, geo.targetBottom);
  return r(0, 0);
};

const visible = () => pp.classList.contains('shown') && !pp.classList.contains('hidden');

function mkUser(txt, withRewind) {
  const d = doc.createElement('div');
  d.className = 'msg msg-user';
  const tag = doc.createElement('div');
  tag.className = 'role-tag';
  tag.textContent = '你 12:00';
  const b = doc.createElement('div');
  b.className = 'bubble';
  b.textContent = txt;
  d.appendChild(tag);
  d.appendChild(b);
  if (withRewind) {
    const r = doc.createElement('button');
    r.className = 'rewind-btn';
    r.textContent = '回滚';
    d.appendChild(r);
  }
  return d;
}

function reset(opts) {
  opts = opts || {};
  clearTimeout(x.pinnedPrompt.fadeTimer);
  x.pinnedPrompt.observer = null;
  x.pinnedPrompt.target = null;
  x.pinnedPrompt.expanded = false;
  box.innerHTML = '';
  pp.className = 'hidden';
  text.textContent = '';
  geo.boxTop = 100;
  geo.targetTop = -500;
  geo.targetBottom = -400;
  geo.msgWrapHidden = false;
  geo.chatHidden = false;
  if (opts.withLoadMore) {
    const w = doc.createElement('div');
    w.id = 'load-more-wrap';
    const b = doc.createElement('button');
    b.className = 'load-more-btn';
    b.textContent = '加载更多（还有 120 条）';
    w.appendChild(b);
    box.appendChild(w);
  }
  const users = opts.users == null ? ['hello'] : opts.users;
  for (const u of users) box.appendChild(mkUser(u, opts.rewind));
}

const tick = () => new Promise((r) => setTimeout(r, 30));
const tests = [];
const test = (name, fn) => tests.push({ name, fn });

test('目标滚出顶部：悬浮条显示，文本只取 .bubble（不含「你/回滚」）', () => {
  reset({ rewind: true });
  x.refreshPinnedPrompt();
  assert.strictEqual(visible(), true, '目标在视口上方时应显示');
  assert.strictEqual(text.textContent, 'hello', '文本不应混入 role-tag / rewind 按钮');
});

test('目标仍可见（部分可见）时不显示', () => {
  reset();
  geo.targetTop = -50; // 顶部被裁掉一部分，但底边还在视口里
  geo.targetBottom = 150;
  x.refreshPinnedPrompt();
  assert.strictEqual(visible(), false);
});

test('目标在视口下方时不显示（防止刚打开/未滚到底误显示）', () => {
  reset();
  geo.targetTop = 400;
  geo.targetBottom = 500;
  x.refreshPinnedPrompt();
  assert.strictEqual(visible(), false);
});

// 回归主场景：用户从长回复底部一把拖到顶部去点「加载更多」。目标从视口上方
// 直接跳到下方，observer 不会回调（相交比例恒为 0），旧代码悬浮条不消失。
test('从底部直接跳到顶部（observer 不回调）时悬浮条收起，不挡「加载更多」', async () => {
  reset({ withLoadMore: true });
  geo.targetTop = -800;
  geo.targetBottom = -700;
  x.refreshPinnedPrompt();
  assert.strictEqual(visible(), true, '底部时应钉住');

  // 跳跃滚动后的几何位置：目标跑到视口下方（没有经过相交状态）
  geo.targetTop = 2000;
  geo.targetBottom = 2100;
  box.dispatchEvent(new win.Event('scroll'));
  await tick(); // rAF 兜底重算

  assert.strictEqual(visible(), false, '滚到顶部后悬浮条必须收起，否则会盖住「加载更多」');
  await new Promise((r) => setTimeout(r, 220));
  assert.strictEqual(pp.classList.contains('hidden'), true, '淡出结束后应回到 hidden');
  assert.ok(doc.querySelector('.load-more-btn'), '「加载更多」按钮仍在');
});

test('observer 回调也不能信 entry 快照：一律按实时几何值判定', () => {
  reset();
  x.refreshPinnedPrompt();
  assert.strictEqual(visible(), true);

  // 模拟一条过期的 entry：它说目标在顶部（旧代码会据此继续 show），
  // 但真实几何已经跑到视口下方。
  geo.targetTop = 2000;
  geo.targetBottom = 2100;
  const io = x.FakeIO.instances[x.FakeIO.instances.length - 1];
  io.fire([{ isIntersecting: false, boundingClientRect: { top: -999 } }]);
  assert.strictEqual(visible(), false);
});

test('切轨迹 tab / chat-view 隐藏时一律不显示（gate）', () => {
  reset();
  x.refreshPinnedPrompt();
  assert.strictEqual(visible(), true);
  geo.msgWrapHidden = true;
  x.syncPinnedVisibility();
  assert.strictEqual(visible(), false);
  geo.msgWrapHidden = false;
  x.syncPinnedVisibility();
  assert.strictEqual(visible(), true, 'gate 解除后按几何位置恢复');
  geo.chatHidden = true;
  x.syncPinnedVisibility();
  assert.strictEqual(visible(), false);
});

test('没有用户消息：隐藏并摘掉 observer', () => {
  reset({ users: [] });
  x.refreshPinnedPrompt();
  assert.strictEqual(visible(), false);
});

test('换钉到最后一条（多端新消息）并按新目标重算显隐', () => {
  reset({ users: ['first'] });
  x.refreshPinnedPrompt();
  assert.strictEqual(text.textContent, 'first');
  const second = mkUser('second');
  box.appendChild(second);
  geo.targetTop = 400;
  geo.targetBottom = 500;
  x.refreshPinnedPrompt();
  assert.strictEqual(text.textContent, 'second');
  assert.strictEqual(visible(), false, '新目标在视口下方时不应显示');
});

(async () => {
  let failed = 0;
  for (const t of tests) {
    try {
      await t.fn();
      console.log('ok   ' + t.name);
    } catch (e) {
      failed++;
      console.error('FAIL ' + t.name + '\n     ' + (e && e.stack ? e.stack.split('\n')[0] : e));
    }
  }
  if (failed) {
    console.error('\n' + failed + ' 个测试失败');
    process.exit(1);
  }
  console.log('\n全部通过（' + tests.length + ' 个）');
})();
