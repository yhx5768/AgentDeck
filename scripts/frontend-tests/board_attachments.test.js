// 看板任务抽屉 composer 的附件回归：粘贴图片、选择文件、拖拽、移除、附件-only
// 发送、忙碌排队保留附件、切卡清空、非主 Agent 禁用。
//
// 数据面契约：图片仍以 base64 进 postMessage；任务时间线只写上主 Agent 会话的
// 上传 URL 链接（/api/sessions/<sid>/uploads/<name>），绝不把 base64 写进看板。
const fs = require('fs');
const path = require('path');
const assert = require('assert');
const { JSDOM } = require('jsdom');

const WEB = path.resolve(__dirname, '../../web');
const src = fs.readFileSync(path.join(WEB, 'app.js'), 'utf8');

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

const HTML = `<!doctype html><html><body>
  <div id="board-drawer-composer">
    <div id="board-composer-status" class="hidden"></div>
    <div id="board-composer-attached" class="attached-images"></div>
    <div class="board-composer-box">
      <button id="board-composer-attach" type="button"></button>
      <textarea id="board-composer-input"></textarea>
      <button id="board-composer-send" type="button">发送</button>
    </div>
    <input id="board-file-input" type="file" multiple hidden>
  </div>
</body></html>`;

const dom = new JSDOM(HTML, { runScripts: 'outside-only', pretendToBeVisual: true });
const w = dom.window;

w.eval(`
const $ = (sel) => document.querySelector(sel);
const state = {
  current: { id: 'main-1', main_agent: true, engine: 'claude', peer_name: 'mainAgent', status: { running: false } },
  boardMockBusy: false,
  pendingBySid: {},
};
let boardComposerTaskId = null;
let boardDrawerTask = null;
let boardAttached = [];
const ALLOWED_MIME = ['image/png', 'image/jpeg', 'image/gif', 'image/webp'];
const MAX_IMAGES = 4;
const MAX_FILES = 8;
const MAX_IMAGE_BYTES = 10 * 1024 * 1024;
const MAX_FILE_BYTES = 40 * 1024 * 1024;
const engineTakesImages = (kind) => kind === 'claude' || kind === 'qoder' || kind === 'opencode';
const statuses = [];
const uploads = [];
const sent = [];
const boardEvents = [];
const held = [];
let uploadImpl = async (f, sid) => {
  uploads.push({ name: f.name, sid });
  return { name: f.name, size: f.size, path: '/data/uploads/' + sid + '/' + f.name };
};
let postMessageImpl = async (payload) => { sent.push(payload); };
function boardComposerStatus(text, kind = '') { statuses.push({ text, kind }); }
function boardQueueStatus() {}
function renderIcons() {}
function fmtBytes(n) { return String(n); }
function fmtMiB(n) { return (n / 1024 / 1024).toFixed(1); }
function readAsDataURL(f) { return Promise.resolve('data:' + f.type + ';base64,QUJD'); }
function stripDataURLPrefix(s) { return s.slice(s.indexOf(',') + 1); }
async function uploadAttachment(f, sid) { return uploadImpl(f, sid); }
async function postMessage(payload) { return postMessageImpl(payload); }
async function boardAppendUserEvent(taskId, text) { boardEvents.push({ taskId, text }); }
function isRunning() { return !!state.busy; }
function pendingItems() { return []; }
function holdPending(text, images, files, meta) {
  held.push({ text, images, files, meta });
  return { text, images, files, meta };
}
${grabFn('boardTaskContext')}
${grabFn('boardAttachmentLinks')}
${grabFn('boardEventText')}
${grabFn('boardCurrentAttachments')}
${grabFn('boardClearComposer')}
${grabFn('renderAttachmentThumbs')}
${grabFn('boardRenderAttachedThumbs')}
${grabFn('boardAddFiles')}
${grabFn('boardOnPaste')}
${grabFn('boardOnDrop')}
${grabFn('boardOnFilePicked')}
${grabFn('boardComposerSync')}
${grabFn('boardSendToMain')}
window.__x = {
  state, statuses, uploads, sent, boardEvents, held,
  get attached() { return boardAttached; },
  set attached(value) { boardAttached = value; },
  get task() { return boardDrawerTask; },
  set task(value) { boardDrawerTask = value; },
  get composerTask() { return boardComposerTaskId; },
  get composerBox() { return $('#board-composer-attached'); },
  get input() { return $('#board-composer-input'); },
  get sendBtn() { return $('#board-composer-send'); },
  get attachBtn() { return $('#board-composer-attach'); },
  boardAddFiles, boardOnPaste, boardOnDrop, boardOnFilePicked,
  boardComposerSync, boardSendToMain,
  boardRenderAttachedThumbs,
  setUpload(fn) { uploadImpl = fn; },
  setPost(fn) { postMessageImpl = fn; },
  reset() {
    boardAttached = [];
    boardComposerTaskId = null;
    boardDrawerTask = null;
    state.current = { id: 'main-1', main_agent: true, engine: 'claude', peer_name: 'mainAgent', status: { running: false } };
    state.busy = false;
    state.boardMockBusy = false;
    statuses.length = 0;
    uploads.length = 0;
    sent.length = 0;
    boardEvents.length = 0;
    held.length = 0;
    document.querySelector('#board-composer-input').value = '';
    boardRenderAttachedThumbs();
    uploadImpl = async (f, sid) => {
      uploads.push({ name: f.name, sid });
      return { name: f.name, size: f.size, path: '/data/uploads/' + sid + '/' + f.name };
    };
    postMessageImpl = async (payload) => { sent.push(payload); };
  },
};
`);

const x = w.__x;
const plain = (value) => JSON.parse(JSON.stringify(value));
const makeFile = (name, type, body = 'abc') => new w.File([body], name, { type });

const tests = [];
const test = (name, fn) => tests.push({ name, fn });

test('粘贴图片：读成 base64 内嵌，同时上传一份供时间线链接', async () => {
  x.reset();
  const file = makeFile('paste.png', 'image/png');
  let prevented = false;
  await x.boardOnPaste({
    clipboardData: { items: [{ kind: 'file', getAsFile: () => file }] },
    preventDefault() { prevented = true; },
  });
  assert.strictEqual(prevented, true, '有附件时要 preventDefault');
  const att = plain(x.attached);
  assert.strictEqual(att.length, 1);
  assert.strictEqual(att[0].kind, 'image');
  assert.strictEqual(att[0].media_type, 'image/png');
  assert.strictEqual(att[0].data, 'QUJD', '图片本体仍走 base64');
  assert.strictEqual(att[0].name, 'paste.png', '上传返回的落盘名要留着生成 URL');
  assert.deepStrictEqual(plain(x.uploads), [{ name: 'paste.png', sid: 'main-1' }]);
  assert.strictEqual(w.document.querySelectorAll('#board-composer-attached .attached-thumb').length, 1);
});

test('拖拽文件：非图片走上传，附件行显示文件 chip', async () => {
  x.reset();
  const file = makeFile('notes.txt', 'text/plain');
  let prevented = false;
  await x.boardOnDrop({
    dataTransfer: { files: [file] },
    preventDefault() { prevented = true; },
  });
  assert.strictEqual(prevented, true);
  const att = plain(x.attached);
  assert.strictEqual(att.length, 1);
  assert.strictEqual(att[0].kind, 'file');
  assert.strictEqual(att[0].name, 'notes.txt');
  assert.strictEqual(att[0].path, '/data/uploads/main-1/notes.txt');
  assert.strictEqual(w.document.querySelectorAll('#board-composer-attached .attached-file').length, 1);
});

test('选择文件：走 change 事件，并清空 input.value', async () => {
  x.reset();
  const file = makeFile('report.pdf', 'application/pdf');
  const target = { files: [file], value: 'C:\\fakepath\\report.pdf' };
  await x.boardOnFilePicked({ target });
  assert.strictEqual(target.value, '', '同名文件再次选择要能触发 change');
  assert.strictEqual(plain(x.attached).length, 1);
  assert.strictEqual(plain(x.attached)[0].name, 'report.pdf');
});

test('移除附件：缩略图上的 x 会从待发列表和 DOM 里删掉', async () => {
  x.reset();
  const a = makeFile('a.png', 'image/png');
  const b = makeFile('b.txt', 'text/plain');
  await x.boardAddFiles([a, b]);
  assert.strictEqual(plain(x.attached).length, 2);
  const firstX = w.document.querySelector('#board-composer-attached .attached-thumb .x');
  firstX.click();
  assert.strictEqual(plain(x.attached).length, 1);
  assert.strictEqual(plain(x.attached)[0].kind, 'file');
  assert.strictEqual(w.document.querySelectorAll('#board-composer-attached .attached-thumb').length, 1);
});

test('附件-only 发送：允许空文字，images/files 传给 postMessage，事件只写链接', async () => {
  x.reset();
  x.task = { id: 'T-1', title: '任务一', status: '进行中', project: 'cc-webui' };
  x.boardComposerSync(x.task);
  await x.boardAddFiles([makeFile('only.png', 'image/png')]);
  x.input.value = '';
  await x.boardSendToMain();
  assert.strictEqual(x.sent.length, 1);
  assert.ok(x.sent[0].text.includes('task_id: T-1'), '正文仍要带任务上下文');
  assert.deepStrictEqual(plain(x.sent[0].images), [{ media_type: 'image/png', data: 'QUJD' }]);
  assert.deepStrictEqual(plain(x.sent[0].files), []);
  assert.strictEqual(x.boardEvents.length, 1);
  assert.strictEqual(x.boardEvents[0].taskId, 'T-1');
  assert.ok(x.boardEvents[0].text.includes(
    '[图片 only.png](/api/sessions/main-1/uploads/only.png)'), x.boardEvents[0].text);
  assert.ok(!x.boardEvents[0].text.includes('base64'));
  assert.strictEqual(plain(x.attached).length, 0, '发送后清空待发附件');
});

test('主 Agent 忙碌：走 holdPending 且排队项保留附件与看板链接', async () => {
  x.reset();
  x.state.busy = true;
  x.task = { id: 'T-2', title: '任务二', status: '进行中', project: 'cc-webui' };
  x.boardComposerSync(x.task);
  await x.boardAddFiles([makeFile('busy.png', 'image/png'), makeFile('busy.txt', 'text/plain')]);
  x.input.value = '排队内容';
  await x.boardSendToMain();
  assert.strictEqual(x.sent.length, 0, '忙碌时不能直接 POST');
  assert.strictEqual(x.held.length, 1);
  assert.strictEqual(x.held[0].images.length, 1);
  assert.strictEqual(x.held[0].files.length, 1);
  assert.strictEqual(x.held[0].meta.taskId, 'T-2');
  assert.strictEqual(x.held[0].meta.bodyText, '排队内容');
  assert.strictEqual(x.held[0].meta.attachmentLinks.length, 2);
  assert.strictEqual(x.boardEvents.length, 0, '排队项还没发送，不能先记任务流');
  assert.strictEqual(plain(x.attached).length, 0);
  assert.strictEqual(x.input.value, '');
});

test('切卡清空待发附件；同一张卡重开不清空', async () => {
  x.reset();
  x.task = { id: 'T-A' };
  x.boardComposerSync({ id: 'T-A' });
  await x.boardAddFiles([makeFile('a.txt', 'text/plain')]);
  assert.strictEqual(plain(x.attached).length, 1);
  x.boardComposerSync({ id: 'T-A' });
  assert.strictEqual(plain(x.attached).length, 1, '同一张卡刷新不清空');
  x.boardComposerSync({ id: 'T-B' });
  assert.strictEqual(plain(x.attached).length, 0, '切到另一张卡必须清空');
  assert.strictEqual(w.document.querySelectorAll('#board-composer-attached .attached-thumb').length, 0);
});

test('非主 Agent 会话：输入框/发送/附件按钮禁用，粘贴也不收', async () => {
  x.reset();
  x.state.current = { id: 'worker-1', main_agent: false, engine: 'claude', status: { running: false } };
  x.boardComposerSync({ id: 'T-9' });
  assert.strictEqual(x.input.disabled, true);
  assert.strictEqual(x.sendBtn.disabled, true);
  assert.strictEqual(x.attachBtn.disabled, true);
  let prevented = false;
  await x.boardOnPaste({
    clipboardData: { items: [{ kind: 'file', getAsFile: () => makeFile('x.png', 'image/png') }] },
    preventDefault() { prevented = true; },
  });
  assert.strictEqual(prevented, true, '仍然拦下浏览器默认的图片粘贴');
  assert.strictEqual(plain(x.attached).length, 0);
  assert.ok(x.statuses.some((entry) => entry.kind === 'error' && entry.text.includes('不是主 Agent')));
});

test('上传过程中切卡：返回的附件丢弃，不挂到新卡', async () => {
  x.reset();
  x.task = { id: 'T-A' };
  x.boardComposerSync({ id: 'T-A' });
  let resolveUpload;
  x.setUpload((f, sid) => new Promise((resolve) => {
    resolveUpload = () => resolve({ name: f.name, size: f.size, path: '/data/uploads/' + sid + '/' + f.name });
  }));
  const pending = x.boardAddFiles([makeFile('slow.txt', 'text/plain')]);
  await new Promise((resolve) => setTimeout(resolve, 0));
  x.boardComposerSync({ id: 'T-B' });
  resolveUpload();
  await pending;
  assert.strictEqual(plain(x.attached).length, 0, '旧卡的上传结果不能落到新卡');
});

test('会话切换后发送：旧会话上传的附件被清空，不把路径带错会话', async () => {
  x.reset();
  x.task = { id: 'T-S' };
  x.boardComposerSync(x.task);
  await x.boardAddFiles([makeFile('old.png', 'image/png'), makeFile('old.txt', 'text/plain')]);
  assert.strictEqual(plain(x.attached).length, 2);
  x.state.current = { id: 'main-2', main_agent: true, engine: 'claude', status: { running: false } };
  x.input.value = '换会话后发送';
  await x.boardSendToMain();
  assert.strictEqual(x.sent.length, 0, '不能把旧会话的附件发给新会话');
  assert.strictEqual(plain(x.attached).length, 0);
  assert.ok(x.statuses.some((entry) => entry.text.includes('会话已切换')));
});

(async () => {
  let failed = 0;
  for (const t of tests) {
    try {
      await t.fn();
      console.log('  ok  ' + t.name);
    } catch (e) {
      failed++;
      console.error('  FAIL ' + t.name);
      console.error('       ' + (e && e.message ? e.message : e));
    }
  }
  console.log(`\nboard_attachments: ${tests.length - failed}/${tests.length} 通过`);
  if (failed) process.exit(1);
})();
