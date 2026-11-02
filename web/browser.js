// Browser panel and Settings toggle for the AgentDeck browser PoC.
// Frames arrive on a dedicated SSE endpoint; input goes back over POST. The
// global session event stream never carries browser frames.
(() => {
  const $ = (sel) => document.querySelector(sel);
  const panel = $('#browser-panel');
  const canvas = $('#browser-canvas');
  const ctx = canvas.getContext('2d');
  const address = $('#browser-address');
  const statusEl = $('#browser-status');
  const emptyEl = $('#browser-empty');
  let source = null;
  let currentId = '';
  let control = 'agent';
  let pendingFrame = null;
  let decoding = false;

  function activeSessionId() {
    try {
      return state && state.current && state.current.id ? state.current.id : '';
    } catch (_) {
      return '';
    }
  }

  async function browserFetch(path, opts = {}) {
    const res = await fetch(path, {
      headers: { 'Content-Type': 'application/json' },
      ...opts,
    });
    let body = {};
    try { body = await res.json(); } catch (_) {}
    return { ok: res.ok, status: res.status, body };
  }

  function setStatusText(text) {
    statusEl.textContent = text;
  }

  function setControlState(next) {
    control = next === 'human' ? 'human' : 'agent';
    const btn = $('#browser-take');
    if (!btn) return;
    btn.textContent = control === 'human' ? '交还' : '接管';
    btn.classList.toggle('active', control === 'human');
    setStatusText(control === 'human'
      ? '你正在控制浏览器；Agent 的 browser_* 工具会暂停。'
      : 'Agent 控制中；点「接管」后可以自己点击和输入。');
  }

  // ---------- live view ----------

  function drawFrame(frame) {
    if (!frame || !frame.data) return;
    if (decoding) { pendingFrame = frame; return; }
    decoding = true;
    const blob = new Blob([Uint8Array.from(atob(frame.data), (c) => c.charCodeAt(0))], { type: 'image/jpeg' });
    createImageBitmap(blob).then((bitmap) => {
      if (canvas.width !== bitmap.width || canvas.height !== bitmap.height) {
        canvas.width = bitmap.width;
        canvas.height = bitmap.height;
      }
      ctx.drawImage(bitmap, 0, 0);
      bitmap.close && bitmap.close();
      emptyEl.classList.add('hidden');
      decoding = false;
      if (pendingFrame) {
        const next = pendingFrame;
        pendingFrame = null;
        drawFrame(next);
      }
    }).catch(() => {
      decoding = false;
      if (pendingFrame) {
        const next = pendingFrame;
        pendingFrame = null;
        drawFrame(next);
      }
    });
  }

  function connectBrowser() {
    if (source) { source.close(); source = null; }
    const id = activeSessionId();
    currentId = id;
    if (!id) {
      setStatusText('先打开一个会话，再打开浏览器面板。');
      emptyEl.classList.remove('hidden');
      return;
    }
    setStatusText('正在连接浏览器…');
    emptyEl.classList.remove('hidden');
    source = new EventSource(`/api/sessions/${encodeURIComponent(id)}/browser/stream`);
    source.addEventListener('status', (ev) => {
      try {
        const st = JSON.parse(ev.data);
        setControlState(st.control);
        if (st.running) {
          const label = st.title ? `${st.title} — ${st.url}` : (st.url || '浏览器已启动');
          setStatusText(control === 'human' ? `你正在控制：${label}` : `Agent 控制中：${label}`);
          emptyEl.classList.add('hidden');
        }
      } catch (_) {}
    });
    source.addEventListener('frame', (ev) => {
      try { drawFrame(JSON.parse(ev.data)); } catch (_) {}
    });
    source.onerror = () => {
      setStatusText('浏览器连接断开，正在重连…');
    };
  }

  function openBrowserPanel() {
    panel.classList.remove('hidden');
    document.body.classList.add('browser-open');
    connectBrowser();
  }

  function closeBrowserPanel() {
    panel.classList.add('hidden');
    document.body.classList.remove('browser-open');
    if (source) { source.close(); source = null; }
    currentId = '';
  }

  // ---------- input ----------

  async function sendInput(payload) {
    const id = activeSessionId();
    if (!id) return null;
    const res = await browserFetch(`/api/sessions/${encodeURIComponent(id)}/browser/input`, {
      method: 'POST',
      body: JSON.stringify(payload),
    });
    if (!res.ok) {
      if (res.status === 409) setStatusText(res.body.error || '输入被拒绝；先点「接管」。');
      return null;
    }
    if (res.body && res.body.status) setControlState(res.body.status.control);
    return res.body;
  }

  async function takeControl() {
    if (control === 'human') return;
    const res = await sendInput({ type: 'take' });
    if (res) setControlState('human');
  }

  async function releaseControl() {
    if (control !== 'human') return;
    const res = await sendInput({ type: 'release' });
    if (res) setControlState('agent');
  }

  async function ensureHuman() {
    if (control !== 'human') await takeControl();
    return control === 'human';
  }

  function pagePoint(e) {
    const r = canvas.getBoundingClientRect();
    if (!r.width || !r.height) return null;
    return {
      x: (e.clientX - r.left) * canvas.width / r.width,
      y: (e.clientY - r.top) * canvas.height / r.height,
    };
  }

  function modifiersOf(e) {
    return (e.altKey ? 1 : 0) | (e.ctrlKey ? 2 : 0) | (e.metaKey ? 4 : 0) | (e.shiftKey ? 8 : 0);
  }

  function buttonOf(e) {
    return e.button === 2 ? 'right' : e.button === 1 ? 'middle' : 'left';
  }

  function buttonsOf(e) {
    return e.buttons || 0;
  }

  canvas.addEventListener('pointerdown', async (e) => {
    e.preventDefault();
    canvas.focus();
    if (!(await ensureHuman())) return;
    const p = pagePoint(e);
    if (!p) return;
    canvas.setPointerCapture && canvas.setPointerCapture(e.pointerId);
    sendInput({
      type: 'mouse', action: 'down', x: p.x, y: p.y,
      button: buttonOf(e), buttons: buttonsOf(e), click_count: 1, modifiers: modifiersOf(e),
    });
  });

  canvas.addEventListener('pointermove', (e) => {
    if (control !== 'human' || !e.buttons) return;
    const p = pagePoint(e);
    if (!p) return;
    sendInput({
      type: 'mouse', action: 'move', x: p.x, y: p.y,
      button: buttonOf(e), buttons: buttonsOf(e), click_count: 0, modifiers: modifiersOf(e),
    });
  });

  canvas.addEventListener('pointerup', async (e) => {
    e.preventDefault();
    const p = pagePoint(e);
    if (!p || control !== 'human') return;
    sendInput({
      type: 'mouse', action: 'up', x: p.x, y: p.y,
      button: buttonOf(e), buttons: buttonsOf(e), click_count: 1, modifiers: modifiersOf(e),
    });
  });

  canvas.addEventListener('wheel', async (e) => {
    if (control !== 'human') return;
    e.preventDefault();
    const p = pagePoint(e);
    if (!p) return;
    sendInput({
      type: 'wheel', x: p.x, y: p.y,
      delta_x: e.deltaX, delta_y: e.deltaY, modifiers: modifiersOf(e),
    });
  }, { passive: false });

  canvas.addEventListener('keydown', (e) => {
    if (control !== 'human') return;
    e.preventDefault();
    const printable = e.key && e.key.length === 1 && !e.ctrlKey && !e.metaKey && !e.altKey;
    sendInput({
      type: 'key', action: 'down', key: e.key, code: e.code,
      key_code: e.keyCode || 0, text: printable ? e.key : '', modifiers: modifiersOf(e),
    });
  });
  canvas.addEventListener('keyup', (e) => {
    if (control !== 'human') return;
    e.preventDefault();
    sendInput({
      type: 'key', action: 'up', key: e.key, code: e.code,
      key_code: e.keyCode || 0, modifiers: modifiersOf(e),
    });
  });

  address.addEventListener('keydown', async (e) => {
    if (e.key !== 'Enter') return;
    const url = address.value.trim();
    if (!url) return;
    if (!(await ensureHuman())) return;
    address.blur();
    sendInput({ type: 'navigate', url });
  });

  $('#browser-back').onclick = async () => { if (await ensureHuman()) sendInput({ type: 'history', delta: -1 }); };
  $('#browser-forward').onclick = async () => { if (await ensureHuman()) sendInput({ type: 'history', delta: 1 }); };
  $('#browser-reload').onclick = async () => { if (await ensureHuman()) sendInput({ type: 'reload' }); };
  $('#browser-take').onclick = () => { if (control === 'human') releaseControl(); else takeControl(); };
  $('#browser-close').onclick = closeBrowserPanel;

  $('#btn-browser').onclick = () => {
    if (panel.classList.contains('hidden')) openBrowserPanel(); else closeBrowserPanel();
  };

  // Keep the panel in sync with the selected AgentDeck session.
  setInterval(() => {
    if (panel.classList.contains('hidden')) return;
    const id = activeSessionId();
    if (id !== currentId) connectBrowser();
  }, 700);

  // ---------- Settings toggle ----------

  const settingStatus = $('#browser-setting-status');
  const settingNote = $('#browser-setting-note');
  const settingSwitch = $('#pref-browser');

  function renderBrowserStatus(st) {
    if (!st) {
      settingStatus.textContent = '无法读取浏览器状态。';
      return;
    }
    if (!st.available) {
      settingStatus.textContent = '未找到本地 Chrome/Chromium' + (st.reason ? `：${st.reason}` : '');
      settingNote.textContent = st.install && st.install.commands && st.install.commands.length
        ? `安装命令：${st.install.commands.join('；')}`
        : (st.install && st.install.url ? `官方下载：${st.install.url}` : '请安装 Chrome 后重新打开设置。');
      return;
    }
    settingStatus.textContent = st.enabled
      ? `已开启；Chrome：${st.chrome_path || '已找到'}；运行中 ${st.running || 0}/${st.max_browsers || 2}`
      : '已找到 Chrome，但浏览器开关未开启。';
    const profile = st.profile_mode === 'shared'
      ? `登录态：共享 profile「${st.profile_key}」，可跨会话复用。`
      : '登录态：按会话隔离；切换会话会使用新的 Chrome profile，需要重新登录。';
    settingNote.textContent = `${profile} 开关保存在服务器；开启后 Claude 会话可使用 browser_* 工具。`;
  }

  async function loadBrowserStatus() {
    const res = await browserFetch('/api/browser/status');
    if (!res.ok) {
      settingStatus.textContent = res.body.error || '无法读取浏览器状态。';
      return null;
    }
    const st = res.body.status;
    settingSwitch.checked = !!st.enabled;
    renderBrowserStatus(st);
    return st;
  }

  async function enableBrowser(install) {
    settingSwitch.disabled = true;
    const res = await browserFetch('/api/browser/enable', {
      method: 'POST',
      body: JSON.stringify({ enabled: true, install: !!install }),
    });
    settingSwitch.disabled = false;
    const st = (res.body && res.body.status) || null;
    if (res.ok) {
      settingSwitch.checked = true;
      renderBrowserStatus(st);
      return true;
    }
    renderBrowserStatus(st);
    if (st && st.install) {
      const plan = st.install;
      const hint = plan.commands && plan.commands.length
        ? `将执行：${plan.commands.join('；')}\n\n`
        : (plan.url ? `官方下载：${plan.url}\n\n` : '');
      const ok = window.confirm(`未找到 Chrome。\n\n${plan.summary || ''}\n\n${hint}是否现在安装？`);
      if (ok && plan.auto) return enableBrowser(true);
      if (ok && !plan.auto) {
        window.alert('当前平台不自动执行安装。请按设置里的官方命令安装，然后重新打开设置。');
      }
    } else {
      window.alert(res.body.error || '开启浏览器失败。');
    }
    settingSwitch.checked = false;
    return false;
  }

  settingSwitch.addEventListener('change', async (e) => {
    if (e.target.checked) {
      const ok = await enableBrowser(false);
      if (!ok) e.target.checked = false;
    } else {
      await browserFetch('/api/browser/disable', { method: 'POST', body: '{}' });
      await loadBrowserStatus();
    }
  });

  // The Settings modal is opened by app.js; refresh status each time it opens.
  const settingsButton = $('#btn-settings');
  if (settingsButton) {
    const prior = settingsButton.onclick;
    settingsButton.onclick = function (ev) {
      if (prior) prior.call(this, ev);
      loadBrowserStatus();
    };
  }

  loadBrowserStatus();
})();
