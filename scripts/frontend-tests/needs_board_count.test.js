// Personal Agent 左栏「看板任务」角标：显示全部任务数（含新建/已完成），
// 不是「待人工 + 待验收」；看板加载前/后、任务变化、0 隐藏都要实时重绘。
const { makeEnv, sampleNeeds, sampleSessions, sampleBoard } = require('./pa_harness');

let failed = 0;
function check(name, cond, extra) {
  if (cond) console.log('  ok  ' + name);
  else { failed++; console.log('  FAIL ' + name + (extra ? '  →  ' + extra : '')); }
}

const env = makeEnv();
const { w, doc, ev } = env;
const nav = () => doc.querySelector('#pa-nav');
const item = (label) => [...doc.querySelectorAll('#pa-nav .pa-nav-item')]
  .find((b) => (b.querySelector('.pa-nav-label') || {}).textContent === label) || null;
const badge = (label) => {
  const el = item(label) && item(label).querySelector('.pa-cnt');
  return el ? el.textContent : null;
};

ev("pa.mode = 'agent'; document.body.classList.add('pa-on')");
ev(`state.sessions = ${JSON.stringify(sampleSessions())}`);
ev(`pa.needs.counts = ${JSON.stringify(sampleNeeds(Date.now()).counts)}; pa.needs.loaded = true`);
ev('pa.view = "queue"');

// 看板还没加载：看板角标隐藏，需要你角标仍是队列待办数。
ev('state.agentBoard = null; paRenderNav();');
check('看板加载前隐藏看板角标', badge('看板任务') === null, String(badge('看板任务')));
check('看板加载前需要你仍显示待办数', badge('需要你') === '5', String(badge('需要你')));

// 看板加载后：全部状态都计入，archived 排除。
const tasks = [
  { id: 'a', status: '新建' },
  { id: 'b', status: '进行中' },
  { id: 'c', status: '待人工' },
  { id: 'd', status: '待验收' },
  { id: 'e', status: '已完成' },
  { id: 'f', status: '已取消' },
  { id: 'g', status: '已完成', archived: true },
];
ev(`state.agentBoard = ${JSON.stringify({ tasks })}; paRenderNav();`);
check('看板角标显示全部非 archived 任务数', badge('看板任务') === '6', String(badge('看板任务')));
check('看板角标不是待人工+待验收', badge('看板任务') !== '2', String(badge('看板任务')));
check('需要你角标不随看板任务数变化', badge('需要你') === '5', String(badge('需要你')));
check('看板角标 tooltip 说明总数', item('看板任务').querySelector('.pa-cnt').title === '看板任务总数（含新建/已完成）',
  item('看板任务').querySelector('.pa-cnt').title);

// 任务变化后必须重绘（sig 含 boardN）；同一 count 但内容变化不要求重绘。
const beforeSig = nav().dataset.sig;
ev(`state.agentBoard = ${JSON.stringify({ tasks: tasks.concat([{ id: 'h', status: '新建' }]) })}; paRenderNav();`);
check('看板任务数变化后角标 +1', badge('看板任务') === '7', String(badge('看板任务')));
check('看板任务数变化触发 nav sig 更新', nav().dataset.sig !== beforeSig, nav().dataset.sig);

// 归零隐藏角标；需要你不受影响。
ev('state.agentBoard = { tasks: [] }; paRenderNav();');
check('看板 0 任务时隐藏角标', badge('看板任务') === null, String(badge('看板任务')));
check('需要你仍显示待办数', badge('需要你') === '5', String(badge('需要你')));
check('任务数变化后看板卡事件仍可渲染', (() => {
  ev(`state.agentBoard = ${JSON.stringify(sampleBoard())}; paRenderNav();`);
  return badge('看板任务') === String(sampleBoard().tasks.length);
})(), String(badge('看板任务')));

console.log(`\nneeds_board_count: ${failed ? 'FAIL' : '全部通过'} (${failed} failed)`);
if (failed) process.exit(1);
