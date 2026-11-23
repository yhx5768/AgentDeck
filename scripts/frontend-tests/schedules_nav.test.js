// Personal Agent 左栏「定时任务」入口：紧跟在「看板任务」后面，点击进入
// schedules 视图（由 web/schedules.js 提供 schedulesOpen）并高亮。
const { makeEnv } = require('./pa_harness');

let failed = 0;
function check(name, cond, extra) {
  if (cond) console.log('  ok  ' + name);
  else { failed++; console.log('  FAIL ' + name + (extra ? '  →  ' + extra : '')); }
}

const env = makeEnv();
const { w, doc, ev } = env;
ev("pa.mode = 'agent'; document.body.classList.add('pa-on'); pa.view = 'chat'; paRenderNav();");

const navItems = () => [...doc.querySelectorAll('#pa-nav .pa-nav-item')];
const labelOf = (b) => (b.querySelector('.pa-nav-label') || {}).textContent;
const labels = navItems().map(labelOf);
check('左栏有定时任务入口', labels.includes('定时任务'), JSON.stringify(labels));
check('定时任务紧跟看板任务', labels.indexOf('定时任务') === labels.indexOf('看板任务') + 1, JSON.stringify(labels));

ev('window.__schedOpen = 0; function schedulesOpen() { window.__schedOpen++; }');
navItems()[labels.indexOf('定时任务')].click();
check('点击调用 schedulesOpen', w.__schedOpen === 1, String(w.__schedOpen));
check('pa.view 切到 schedules', ev('pa.view') === 'schedules', ev('pa.view'));
const after = navItems();
const on = after.find((b) => labelOf(b) === '定时任务');
check('入口高亮', !!on && on.classList.contains('on'));
check('看板入口不再高亮', !(after.find((b) => labelOf(b) === '看板任务') || {}).classList.contains ||
  !after.find((b) => labelOf(b) === '看板任务').classList.contains('on'));

console.log(`\nschedules_nav: ${failed ? 'FAIL' : '全部通过'} (${failed} failed)`);
if (failed) process.exit(1);
