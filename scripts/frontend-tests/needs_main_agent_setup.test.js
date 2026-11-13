// Personal Agent：没有 mainAgent 时的创建引导（CLI/provider/model 联动 + 提交 payload）。
const { makeEnv, sampleSessions, sampleBoard } = require('./pa_harness');
const fs = require('fs');
const path = require('path');
const WEB = path.resolve(__dirname, '../../web');

let failed = 0;
function check(name, cond, extra) {
  if (cond) console.log('  ok  ' + name);
  else { failed++; console.log('  FAIL ' + name + (extra ? '  →  ' + extra : '')); }
}
const J = (x) => JSON.stringify(x);
const tick = (ms) => new Promise((r) => setTimeout(r, ms));

function setupEnv(extra) {
  const env = makeEnv({ api: { '/api/agent-board': () => sampleBoard() } });
  const { w, doc, ev } = env;
  ev("pa.mode = 'agent'; document.body.classList.add('pa-on'); pa.view = 'queue'; pa.asideOpen = true");
  ev(`state.sessions = ${J(sampleSessions().filter((s) => !s.main_agent))}`);
  ev(`state.clis = ${J(extra.clis)}; state.providers = ${J(extra.providers)}; state.defaultCwd = '/workspace'; state.home = '/home/u'`);
  ev("document.querySelector('#needs-view').classList.remove('hidden'); document.querySelector('#chat-view').classList.add('hidden')");
  ev('agentRenderQueue(true)');
  return env;
}
const opts = (sel) => [...sel.options].map((o) => ({ value: o.value, text: o.textContent }));

async function main() {
  const clis = [
    { name: 'ducc', kind: 'claude' },
    { name: 'ducx', kind: 'codex' },
    { name: 'oc', kind: 'opencode' },
  ];
  const providers = [
    { name: 'ducc-models', kind: 'claude', models: [{ name: 'm1' }, { name: 'm2' }] },
    { name: 'codex-ducx', kind: 'codex', models: [{ name: 'cx1' }] },
    { name: 'oc-p', kind: 'opencode', models: [{ name: 'kimi' }] },
  ];
  const env = setupEnv({ clis, providers });
  const { w, doc, ev } = env;
  const q = (s) => doc.querySelector(s);
  check('无主 Agent 时中栏显示创建引导（不是空队列）', !!q('#needs-view .pa-setup')
    && /创建主 Agent/.test(q('#needs-view .pa-setup').textContent)
    && /还没有主 Agent/.test(q('#needs-view .pa-setup').textContent));
  check('CLI 下拉列出所有 CLI，并显示 name + engine kind', J(opts(q('#pa-setup-cli'))) === J([
    { value: 'ducc', text: 'ducc · claude' },
    { value: 'ducx', text: 'ducx · codex' },
    { value: 'oc', text: 'oc · opencode' },
  ]), J(opts(q('#pa-setup-cli'))));
  check('默认选中第一个 CLI/provider，cwd 用 default_cwd，显示名默认主 Agent',
    q('#pa-setup-cli').value === 'ducc' && q('#pa-setup-provider').value === 'ducc-models'
    && q('#pa-setup-cwd').value === '/workspace' && q('#pa-setup-name').value === '主 Agent');
  check('模型下拉复用现有 picker：保留空值 = CLI 默认，并列出 provider 模型',
    q('#pa-setup-model').value === ''
    && J(opts(q('#pa-setup-model')).map((o) => o.value)) === J(['', 'm1', 'm2'])
    && /默认 \(CLI 默认\)/.test(q('#pa-setup-model').options[0].textContent), q('#pa-setup-model').innerHTML);

  // 切 CLI：provider 只留同 kind，model 重新加载。
  q('#pa-setup-cli').value = 'ducx';
  q('#pa-setup-cli').dispatchEvent(new w.Event('change', { bubbles: true }));
  await tick(20);
  check('切到 codex CLI：provider 只剩 codex provider', J(opts(q('#pa-setup-provider')).map((o) => o.value)) === J(['codex-ducx'])
    && q('#pa-setup-provider').value === 'codex-ducx');
  check('切 CLI 后模型下拉跟着换', J(opts(q('#pa-setup-model')).map((o) => o.value)) === J(['', 'cx1']), q('#pa-setup-model').innerHTML);

  q('#pa-setup-cli').value = 'oc';
  q('#pa-setup-cli').dispatchEvent(new w.Event('change', { bubbles: true }));
  await tick(20);
  check('切到 opencode CLI：provider/model 继续联动', q('#pa-setup-provider').value === 'oc-p'
    && J(opts(q('#pa-setup-model')).map((o) => o.value)) === J(['', 'kimi']), q('#pa-setup-model').innerHTML);

  // 回到 claude，填完整 payload 并提交。
  q('#pa-setup-cli').value = 'ducc';
  q('#pa-setup-cli').dispatchEvent(new w.Event('change', { bubbles: true }));
  await tick(20);
  q('#pa-setup-model').value = 'm2';
  q('#pa-setup-model').dispatchEvent(new w.Event('change', { bubbles: true }));
  q('#pa-setup-cwd').value = '/work/agent';
  q('#pa-setup-cwd').dispatchEvent(new w.Event('input', { bubbles: true }));
  q('#pa-setup-name').value = '我的主 Agent';
  q('#pa-setup-name').dispatchEvent(new w.Event('input', { bubbles: true }));
  w.__api['POST /api/sessions'] = (body) => ({ id: 'new-main', name: body.name, main_agent: true });
  ev("loadSessions = async () => { state.sessions.push({ id: 'new-main', name: '我的主 Agent', main_agent: true, cli: 'ducc', provider: 'ducc-models', cwd: '/work/agent', status: { running: false, queued: 0 } }); }");
  q('#needs-view .pa-setup-foot .primary-btn').click();
  await tick(30);
  const post = w.__calls.filter((c) => c.method === 'POST' && c.path === '/api/sessions').pop();
  check('提交 payload：cli/provider/model/cwd/name/main_agent/peer_name/role_prompt/resident 正确',
    !!post && post.body.cli === 'ducc' && post.body.provider === 'ducc-models'
    && post.body.model === 'm2' && post.body.cwd === '/work/agent'
    && post.body.name === '我的主 Agent' && post.body.main_agent === true
    && post.body.peer_name === 'mainAgent' && post.body.role_prompt === 'test main prompt'
    && post.body.resident === true, post && J(post.body));
  check('创建成功走 set current main + 进 chat 的路径', ev('window.__opened') === 'new-main');

  // 没有 CLI/provider 时给出配置提示，而不是只报创建失败。
  const env2 = setupEnv({ clis: [], providers: [] });
  const d2 = env2.doc;
  check('没有可用 CLI 时明确提示去 config.yaml 配置', /config\.yaml 里没有可用的 CLI/.test(d2.querySelector('#needs-view .pa-setup').textContent)
    && d2.querySelector('#needs-view .pa-setup-foot .primary-btn').disabled);
  const env3 = setupEnv({ clis: [{ name: 'oc', kind: 'opencode' }], providers: [] });
  check('没有同 kind provider 时明确提示配置 providers', /没有可用的 provider/.test(env3.doc.querySelector('#needs-view .pa-setup').textContent)
    && env3.doc.querySelector('#needs-view .pa-setup-foot .primary-btn').disabled);

  const html = fs.readFileSync(path.join(WEB, 'index.html'), 'utf8');
  const appSrc = fs.readFileSync(path.join(WEB, 'app.js'), 'utf8');
  check('设置页保留自动创建开关（服务端持久化）', html.includes('id="pref-ma-autocreate"')
    && html.includes('首次启动自动创建主 Agent')
    && appSrc.includes("'/api/main-agent/autocreate'"));

  console.log(failed ? `\n${failed} 项失败` : '\n全部通过');
  process.exit(failed ? 1 : 0);
}
main().catch((e) => { console.error(e); process.exit(1); });
