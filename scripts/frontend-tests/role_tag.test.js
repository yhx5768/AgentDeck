// 主对话 assistant 气泡角色标签的回归测试。
//
// 用户要求把气泡左侧那枚大写标签从「CLAUDE」改成「AGENT」。它是 addMsg() 里
// 写死的 roleTag 字符串，源里写小写、靠 .role-tag 的 text-transform: uppercase
// 显示成大写。这里钉住三件事：
//   ① 所有 assistant 气泡的 roleTag 都是 'agent'（别再有 'claude' 残留）；
//   ② '.role-tag' 规则里仍有 text-transform: uppercase（否则页面显示成小写）；
//   ③ addMsg 本身的行为没变（roleTag 原样进 DOM），且用户「你」与 peer 标签
//      照旧——这次只动 assistant 的展示标签，不许波及它们。
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
function formatMsgTime() { return ''; }
${grabFn('addMsg')}
window.__x = { addMsg, box: document.createElement('div') };
`;

const dom = new JSDOM('<!doctype html><html><body></body></html>',
  { runScripts: 'outside-only', pretendToBeVisual: true });
dom.window.eval(setup);
const x = dom.window.__x;

const tests = [];
const test = (name, fn) => tests.push({ name, fn });
const roleTag = (el) => el.querySelector('.role-tag').textContent;

test('assistant 气泡的角色标签是 agent', () => {
  const b = x.addMsg('msg-assistant', 'agent', x.box, null);
  assert.strictEqual(roleTag(b.parentElement), 'agent');
});

test('源里不再有 addMsg 的 claude 角色标签', () => {
  assert.strictEqual(/addMsg\('msg-assistant',\s*'claude'/.test(src), false,
    '仍有一处 assistant 气泡写死 claude');
  const n = (src.match(/addMsg\('msg-assistant',\s*'agent'/g) || []).length;
  assert.ok(n >= 4, '应为全部 4 处 assistant 气泡都写 agent，实得 ' + n);
});

test('role-tag 仍靠 CSS 转大写（源里小写即可显示 AGENT）', () => {
  const m = css.match(/^\.role-tag\s*\{[^}]*\}/m);
  assert.ok(m, '找不到 .role-tag 规则');
  assert.ok(/text-transform:\s*uppercase/.test(m[0]), '.role-tag 需保留 uppercase');
});

test('peer 气泡的标签不受影响', () => {
  const b = x.addMsg('msg-peer', '⇢ 来自 另一个会话', x.box, null);
  assert.strictEqual(roleTag(b.parentElement), '⇢ 来自 另一个会话');
});

test('用户气泡的标签仍是「你」', () => {
  const b = x.addMsg('msg-user', '你', x.box, null);
  assert.strictEqual(roleTag(b.parentElement), '你');
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
  console.log('role_tag: ' + (tests.length - failed) + '/' + tests.length + ' 通过');
  if (failed) process.exit(1);
})();
