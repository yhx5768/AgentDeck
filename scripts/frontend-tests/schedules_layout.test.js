// 定时任务弹窗滚动结构回归（只钉 DOM/ CSS 契约，布局仍以浏览器截图为准）：
// - 标题/模式切换在固定 head，长内容在可滚动 body，取消/保存在固定 foot；
// - body 独立滚动（overflow-y:auto + overscroll-behavior:contain + min-height:0）；
// - 弹窗 max-height 90vh（窄屏 calc(100vh - 16px)）；
// - 候选卡片/最终 prompt 不再自己滚动，长内容交给 body。
const fs = require('fs');
const path = require('path');

const WEB = path.resolve(__dirname, '../../web');
const html = fs.readFileSync(path.join(WEB, 'index.html'), 'utf8');
const css = fs.readFileSync(path.join(WEB, 'style.css'), 'utf8');

let failed = 0;
function check(name, cond, extra) {
  if (cond) console.log('  ok  ' + name);
  else { failed++; console.log('  FAIL ' + name + (extra ? '  →  ' + extra : '')); }
}

function rule(selector) {
  const i = css.indexOf(selector + ' {');
  if (i < 0) return '';
  const b = css.indexOf('{', i);
  let d = 0;
  for (let j = b; j < css.length; j++) {
    if (css[j] === '{') d++;
    else if (css[j] === '}') { d--; if (!d) return css.slice(b + 1, j); }
  }
  return '';
}

// ---------- HTML：三段式结构 ----------
const iHead = html.indexOf('class="sched-dialog-head"');
const iBody = html.indexOf('class="sched-dialog-body"');
const iFoot = html.indexOf('class="sched-dialog-foot"');
check('弹窗有 head/body/foot 三段', iHead > 0 && iHead < iBody && iBody < iFoot);
check('card 带 sched-dialog 类', /class="modal-card sched-card sched-dialog"/.test(html));
const headHtml = html.slice(iHead, iBody);
const bodyHtml = html.slice(iBody, iFoot);
const footHtml = html.slice(iFoot);
check('标题和模式切换在固定 head', headHtml.includes('id="sched-modal-title"') && headHtml.includes('id="sched-mode"'));
check('长文本（AI 理由/问题）在可滚动 body',
  bodyHtml.includes('id="sched-ai-rationale"') && bodyHtml.includes('id="sched-ai-questions"') &&
  bodyHtml.includes('id="sched-ai-meta"'));
check('表单字段在 body', bodyHtml.includes('id="sched-prompt"') && bodyHtml.includes('id="sched-target"') &&
  bodyHtml.includes('id="sched-name"') && bodyHtml.includes('id="sched-final-text"'));
check('取消/保存在固定 foot', footHtml.includes('id="sched-save"') && footHtml.includes('data-close'));
check('错误提示在 foot（长内容滚动时也可见）', footHtml.includes('id="sched-modal-error"'));

// ---------- CSS：滚动与固定契约 ----------
const dialog = rule('#modal-schedule .sched-dialog');
check('弹窗 max-height 90vh', /max-height:\s*90vh/.test(dialog));
check('弹窗裁掉溢出，由 body 滚动', /overflow:\s*hidden/.test(dialog));
const body = rule('.sched-dialog-body');
check('body 独立纵向滚动', /overflow-y:\s*auto/.test(body));
check('body 阻止滚动链传导到背景', /overscroll-behavior:\s*contain/.test(body));
check('body 可收缩（min-height:0）', /min-height:\s*0/.test(body));
check('body 吃掉剩余高度', /flex:\s*1 1 auto/.test(body));
const head = rule('.sched-dialog-head');
check('head 不参与伸缩（fixed）', /flex:\s*none/.test(head));
const foot = rule('.sched-dialog-foot');
check('foot 不参与伸缩（fixed）', /flex:\s*none/.test(foot));
check('foot 有分隔线', /border-top:\s*1px solid/.test(foot));
const actions = rule('.sched-dialog-foot .modal-actions');
check('foot 内按钮区去掉额外 margin', /margin-top:\s*0/.test(actions));

const candidates = rule('.sched-candidates');
check('候选列表不再自己滚动（避免嵌套滚动陷阱）', !/max-height/.test(candidates) && !/overflow/.test(candidates));
const finalText = rule('.sched-final-text');
check('最终 prompt 预览不再自己滚动', !/max-height/.test(finalText) && !/overflow-y/.test(finalText));

// 窄屏
const media = css.slice(css.indexOf('@media (max-width: 640px)'));
check('窄屏 max-height 用 calc(100vh - 16px)', media.includes('calc(100vh - 16px)'));
check('窄屏 body/head/foot 有内边距覆盖', media.includes('.sched-dialog-body') && media.includes('.sched-dialog-foot'));

console.log(`\nschedules_layout: ${failed ? 'FAIL' : '全部通过'} (${failed} failed)`);
if (failed) process.exit(1);
