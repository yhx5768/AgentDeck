// Capture the AgentDeck UI screenshots that the film composites.
//
// Everything here drives the *isolated demo instance* (video/demo-config.yaml,
// port 8323) whose content was seeded by tools/seed-demo.py. The real
// dogfooding instance on :8321 is never touched, and /api/stats is intercepted
// with a synthetic payload so the stats shot is both instant and free of the
// real machine's numbers.
//
// Run: node tools/capture.mjs        (server must already be up)
import { chromium } from 'playwright';
import { mkdirSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const OUT = join(HERE, '..', 'assets');
mkdirSync(OUT, { recursive: true });

const BASE = process.env.DEMO_BASE || 'http://127.0.0.1:8323';
const TOKEN = process.env.DEMO_TOKEN || 'agentdeck-demo';
const DSF = Number(process.env.DSF || 2);

// ---------------------------------------------------------------- fake stats
// 120 days of plausible activity; shape copied from server.handleStats.
function fakeStats() {
  const days = [];
  const models = ['deepseek-v4.1-flash', 'glm-5.3', 'MiniMax-M3.1-Flash-Preview', 'deepseek-v4-pro'];
  const today = new Date('2026-10-30T00:00:00Z');
  for (let i = 119; i >= 0; i--) {
    const d = new Date(today.getTime() - i * 86400000);
    const key = d.toISOString().slice(0, 10);
    const dow = d.getUTCDay();
    const weekend = dow === 0 || dow === 6;
    // ramp up over the window, quiet weekends, a few gaps
    const base = 1 + Math.round((119 - i) / 22);
    const n = weekend ? Math.max(0, base - 4) : base;
    const sessions = i === 40 || i === 41 ? 0 : n;
    if (sessions === 0) continue;
    const tools = sessions * (18 + ((i * 7) % 40));
    days.push({ date: key, session_count: sessions, tool_call_count: tools });
  }
  const tokens = days.map((d) => {
    const byModel = {};
    const total = d.session_count * (240000 + ((d.session_count * 31337) % 900000));
    models.forEach((m, i) => {
      const share = [0.46, 0.27, 0.17, 0.1][i];
      const v = Math.round(total * share);
      if (v > 0) byModel[m] = v;
    });
    return { date: d.date, tokens_by_model: byModel };
  });
  return {
    engines: { claude: 512, opencode: 148, mcode: 63, qoder: 41, codex: 88 },
    total_sessions: 852,
    total_messages: 61_482,
    active_days: 97,
    total_days: 120,
    streaks: { current: 23, longest: 47 },
    peak_activity_hour: 22,
    favorite_model: 'deepseek-v4.1-flash',
    daily_activity: days,
    daily_model_tokens: tokens,
  };
}

// ------------------------------------------------------------------- driving
async function settle(page, ms = 1800) {
  await page.waitForTimeout(ms);
}

async function shot(page, name, clip) {
  const path = join(OUT, name + '.png');
  await page.screenshot({ path, clip });
  console.log('  ✓', name + '.png');
}

/**
 * Screenshot one element, clipped with padding. Used for the pieces the film
 * animates on their own (the arriving peer bubble, the dropping toast): those
 * need a frame *without* them plus a cut-out of them.
 * Records the padded rect in asset px so film.js knows where to draw it.
 */
async function shotEl(page, name, sel, pad = 14) {
  const r = await page.evaluate(
    ([s, p]) => {
      const el = document.querySelector(s);
      if (!el) return null;
      const b = el.getBoundingClientRect();
      return {
        x: Math.max(0, b.x - p), y: Math.max(0, b.y - p),
        width: b.width + p * 2, height: b.height + p * 2,
      };
    },
    [sel, pad],
  );
  if (!r) { console.log('  ! shotEl missing', sel); return; }
  await page.screenshot({ path: join(OUT, name + '.png'), clip: r });
  RECTS[name + '.clip'] = {
    x: Math.round(r.x * DSF), y: Math.round(r.y * DSF),
    w: Math.round(r.width * DSF), h: Math.round(r.height * DSF),
  };
  console.log('  ✓', name + '.png (clipped)', JSON.stringify(RECTS[name + '.clip']));
}

// Rects of interesting elements, in *asset* pixels (viewport px × deviceScaleFactor).
// film.js hardcodes cameras and spotlights from these; measuring beats eyeballing a
// downscaled preview, which cost a couple of wasted 11-minute renders.
const RECTS = {};
async function measure(page, name, sels, dsf = DSF) {
  RECTS[name] = await page.evaluate(
    ([list, scale]) => {
      const out = {};
      for (const sel of list) {
        const el = document.querySelector(sel);
        if (!el) { out[sel] = null; continue; }
        const r = el.getBoundingClientRect();
        out[sel] = {
          x: Math.round(r.x * scale), y: Math.round(r.y * scale),
          w: Math.round(r.width * scale), h: Math.round(r.height * scale),
        };
      }
      return out;
    },
    [sels, dsf],
  );
  console.log('  · rects', name, JSON.stringify(RECTS[name]));
}

/** open a session and wait until its replay has actually painted */
async function openSession(page, id) {
  await page.evaluate((sid) => openSession(sid), id);
  await page.waitForFunction(() => document.querySelectorAll('#messages > *').length > 0, null, {
    timeout: 20000,
  });
  await settle(page, 1500);
}

async function openGroup(page, gid) {
  await page.evaluate((g) => openGroup(g), gid);
  await page.waitForFunction(
    () => !document.querySelector('#group-view')?.classList.contains('hidden'),
    null,
    { timeout: 20000 },
  );
  await settle(page, 1800);
}

/** scroll the message list so the element matching `sel` sits at `frac` of the viewport */
async function scrollTo(page, sel, frac = 0.35) {
  await page.evaluate(
    ([s, f]) => {
      const box = document.querySelector('#messages');
      const el = document.querySelector(s);
      if (!box || !el) return;
      const delta = el.getBoundingClientRect().top - box.getBoundingClientRect().top;
      box.scrollTop += delta - box.clientHeight * f;
    },
    [sel, frac],
  );
  await page.waitForTimeout(400);
}

/** expand every tool card so the whole turn is legible in the still */
async function expandTools(page) {
  await page.evaluate(() => {
    document.querySelectorAll('#messages details.tool-block').forEach((d) => (d.open = true));
  });
  await page.waitForTimeout(500);
}

const browser = await chromium.launch({ channel: 'chrome' });

async function newPage({ width = 1600, height = 1000, mobile = false, dsf = DSF } = {}) {
  const ctx = await browser.newContext({
    viewport: { width, height },
    deviceScaleFactor: dsf,
    isMobile: mobile,
    hasTouch: mobile,
    userAgent: mobile
      ? 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1'
      : undefined,
  });
  await ctx.addCookies([{ name: 'ccgw_token', value: TOKEN, url: BASE }]);
  const page = await ctx.newPage();
  await page.route('**/api/stats*', (route) =>
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(fakeStats()) }),
  );
  await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
  await page.evaluate(() => {
    localStorage.setItem('cc-webui-theme', 'dark');
    localStorage.setItem('cc-webui:ws-panel-tree-collapsed', '0');
  });
  await page.reload({ waitUntil: 'networkidle' });
  await settle(page, 600);
  return { ctx, page };
}

console.log('capturing →', OUT);

// ------------------------------------------------- 1. hero chat, tool cards
{
  const { ctx, page } = await newPage();
  await openSession(page, 'demo-hero-0001');
  await expandTools(page);
  await scrollTo(page, '#messages .tool-block', 0.16);
  await shot(page, 'chat-tools');
  await ctx.close();
}

// ------------------------------------------------- 2. hero chat, task panel
{
  const { ctx, page } = await newPage();
  await openSession(page, 'demo-hero-0001');
  await page.evaluate(() => {
    const box = document.querySelector('#messages');
    box.scrollTop = box.scrollHeight;
  });
  await settle(page, 500);
  await shot(page, 'chat-tail');
  await ctx.close();
}

// ----------------------------------------------------------- 4. group view
{
  const { ctx, page } = await newPage();
  await openGroup(page, 'demo-group-0001');
  await shot(page, 'group');
  await ctx.close();
}

// ------------------------------------------------------- 5. workspace panel
{
  const { ctx, page } = await newPage();
  await openSession(page, 'demo-hero-0001');
  await page.evaluate(() => wsToggle(true));
  await settle(page, 800);
  // open a real file from the worktree so the tree/highlight is authentic
  await page.evaluate(() => {
    const root = (state.current.cwd || '').replace(/\/+$/, '');
    wsOpenPath(root + '/internal/server/workspace.go');
  });
  await page.waitForFunction(() => wsState.file && !wsState.file.loading, null, { timeout: 15000 });
  await settle(page, 700);
  await shot(page, 'workspace-code');
  // server-side whole-tree search
  await page.evaluate(() => {
    const f = document.querySelector('#ws-filter');
    f.value = 'workspace.go';
    f.dispatchEvent(new Event('input', { bubbles: true }));
  });
  await settle(page, 1500);
  await shot(page, 'workspace-search');
  await ctx.close();
}

// ------------------------------- 5b. workspace panel over the demo project
{
  const { ctx, page } = await newPage();
  await openSession(page, 'demo-ws-0009');
  await page.evaluate(() => wsToggle(true));
  await settle(page, 800);
  await page.evaluate(() => {
    const root = (state.current.cwd || '').replace(/\/+$/, '');
    wsOpenPath(root + '/internal/router/router.go');
  });
  await page.waitForFunction(() => wsState.file && !wsState.file.loading, null, { timeout: 15000 });
  await settle(page, 600);
  await page.evaluate(() => wsShowDiff());
  await page.waitForFunction(
    () => wsState.mode === 'diff' && wsState.diff && !wsState.diff.loading,
    null,
    { timeout: 15000 },
  );
  await settle(page, 700);
  await shot(page, 'workspace-diff');
  await ctx.close();
}

// ------------------------------------ 5c. agent2agent: the sending side
{
  const { ctx, page } = await newPage();
  await openSession(page, 'demo-a2a-0010');
  await expandTools(page);
  await scrollTo(page, '#messages .tool-peer-send', 0.52);
  await measure(page, 'peer-send', ['.tool-peer-send', '.msg-peer'], DSF);
  await shot(page, 'peer-send');
  await ctx.close();
}

// ------------------------------------ 5d. agent2agent: the receiving side
// Two assets from one setup: the frame with the inbound bubble hidden (the
// film animates it arriving) and a cut-out of the bubble itself.
{
  const { ctx, page } = await newPage();
  await openSession(page, 'demo-docs-0006');
  await expandTools(page);
  await scrollTo(page, '#messages .msg-peer', 0.34);
  await measure(page, 'peer-inbound', ['.msg-peer', '.tool-peer-send'], DSF);
  await page.evaluate(() => { document.querySelector('.msg-peer').style.visibility = 'hidden'; });
  await settle(page, 300);
  await shot(page, 'peer-inbound');
  await page.evaluate(() => { document.querySelector('.msg-peer').style.visibility = ''; });
  await settle(page, 300);
  await shotEl(page, 'peer-bubble', '.msg-peer');
  await ctx.close();
}

// --------------------------- 5e. the 📡 popover, clipped at 4x for a crisp card
{
  const { ctx, page } = await newPage({ dsf: 4 });
  await openSession(page, 'demo-a2a-0010');
  await page.click('#btn-resident');
  await settle(page, 700);
  const popBox = await page.evaluate(() => {
    // The popover's first status refresh happens on the 1s poll, and the click
    // handler's own updateResidentUI() call is a no-op (the panel is still
    // hidden at that point). So stop the poll and run one refresh by hand —
    // otherwise the switch still renders as unchecked.
    if (typeof residentPopPoll !== 'undefined' && residentPopPoll) {
      clearInterval(residentPopPoll);
      residentPopPoll = null;
    }
    updateResidentUI();
    // The demo instance deliberately holds no real resident process — that
    // would spawn the user's own `claude` with their real credentials. So the
    // status line reads "进程启动中…". Replace it with the wording
    // updateResidentUI() produces for a live process; it is a video prop,
    // copied verbatim.
    const el = document.querySelector('#resident-status');
    if (el) el.textContent = '在线 · 注册名 arch-review';
    const r = document.querySelector('#resident-pop').getBoundingClientRect();
    return { x: r.x, y: r.y, w: r.width, h: r.height };
  });
  await shot(page, 'peer-popover', {
    x: Math.round(popBox.x - 10),
    y: Math.round(popBox.y - 10),
    width: Math.round(popBox.w + 20),
    height: Math.round(popBox.h + 20),
  });
  await ctx.close();
}


// --------------------------------------------------------- 6. stats modal
{
  const { ctx, page } = await newPage();
  await openSession(page, 'demo-hero-0001');
  await page.click('#btn-stats');
  await settle(page, 1600);
  await shot(page, 'stats');
  await ctx.close();
}

// ------------------------------------------------------- 7. trajectory tab
{
  const { ctx, page } = await newPage();
  await openSession(page, 'demo-hero-0001');
  await page.evaluate(() => {
    document.querySelectorAll('.chat-tab').forEach((b) => {
      if (b.dataset.tab === 'traj') b.click();
    });
  });
  await settle(page, 1200);
  await shot(page, 'trajectory');
  await ctx.close();
}

// ------------------------------------------------------------- 9. mobile
{
  const { ctx, page } = await newPage({ width: 414, height: 896, mobile: true });
  await openSession(page, 'demo-hero-0001');
  await page.evaluate(() => {
    const box = document.querySelector('#messages');
    box.scrollTop = box.scrollHeight;
  });
  await settle(page, 400);
  await shot(page, 'mobile');
  await ctx.close();
}

// -------------------------------------------------- 9b. new-session view
{
  const { ctx, page } = await newPage();
  await page.evaluate(() => openNewSessionView());
  await settle(page, 1200);
  await shot(page, 'new-session');
  await ctx.close();
}

// -------------------------------------------------- 9c. sidebar close-up
{
  const { ctx, page } = await newPage({ width: 900, height: 1000 });
  await settle(page, 900);
  await shot(page, 'sidebar', { x: 0, y: 0, width: 280, height: 1000 });
  await ctx.close();
}

// ------------------------------------------- 10. light-theme hero (for cuts)
{
  const { ctx, page } = await newPage();
  await page.evaluate(() => localStorage.setItem('cc-webui-theme', 'light'));
  await page.reload({ waitUntil: 'networkidle' });
  await settle(page, 700);
  await openSession(page, 'demo-hero-0001');
  await expandTools(page);
  await scrollTo(page, '#messages .tool-block', 0.16);
  await shot(page, 'chat-tools-light');
  await ctx.close();
}

// ------------------------------------- 11. pending ask: card + toast banner
// MUST run last: the pending ask is server-side state with a 55s timeout, so
// every page captured afterwards would carry the banner.
{
  const { ctx, page } = await newPage();
  await openSession(page, 'demo-hero-0001');
  // fire-and-forget: the endpoint blocks until answered or the 55s timeout
  await page.evaluate(() => {
    fetch('/api/sessions/demo-hero-0001/permissions/request', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        tool: 'AskUserQuestion',
        input: {
          questions: [
            {
              header: '实现方式',
              question: '「待回答」这个提示，你希望用哪种实现方式？',
              multiSelect: false,
              options: [
                { label: '方案 A：顶部横幅', description: '视线落点就在那儿，不用找' },
                { label: '方案 B：右下角浮层', description: '不挡内容，但容易被忽略' },
              ],
            },
          ],
        },
      }),
    }).catch(() => {});
  });
  await settle(page, 2600);
  await measure(page, 'ask', ['.perm-card', '#toast-layer .toast', '#messages .msg-user'], DSF);
  await shot(page, 'ask');
  await ctx.close();
}

// --------------- 11b. the same pending ask, seen from ANOTHER conversation
// This is the shot that carries the point: the ask lives on demo-hero-0001, but
// demo-oc-0002 is the conversation on screen. The banner arrives through the
// 5s list-polling diff (the only channel that sees another session's asks).
{
  const { ctx, page } = await newPage();
  await openSession(page, 'demo-oc-0002');
  await page.evaluate(() => {
    fetch('/api/sessions/demo-hero-0001/permissions/request', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        tool: 'AskUserQuestion',
        input: {
          questions: [
            {
              header: '实现方式',
              question: '「待回答」这个提示，你希望用哪种实现方式？',
              multiSelect: false,
              options: [
                { label: '方案 A：顶部横幅', description: '视线落点就在那儿，不用找' },
                { label: '方案 B：右下角浮层', description: '不挡内容，但容易被忽略' },
              ],
            },
          ],
        },
      }),
    }).catch(() => {});
  });
  // wait for the polling diff to raise the banner, then let it settle
  await page.waitForSelector('#toast-layer .toast', { timeout: 20000 });
  await page.waitForSelector('.session-item.has-ask', { timeout: 20000 });
  await settle(page, 1200);
  await measure(page, 'ask-cross', [
    '#toast-layer .toast', '.session-item.has-ask', '#ws-activity', '#ws-pop-pending',
  ], DSF);
  // cut the banner out, then take the frame without it — the film drops the
  // banner in from above, the way a notification actually arrives
  await shotEl(page, 'ask-toast', '#toast-layer .toast');
  await page.evaluate(() => { document.querySelector('#toast-layer').innerHTML = ''; });
  await settle(page, 400);
  await shot(page, 'ask-cross');
  await ctx.close();
}

await browser.close();
writeFileSync(join(OUT, 'rects.json'), JSON.stringify(RECTS, null, 2));
console.log('rects →', join(OUT, 'rects.json'));
writeFileSync(join(OUT, '.captured'), new Date().toISOString());
console.log('done');
