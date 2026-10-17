// 图标白名单回归。
//
// renderIcons 只渲染 `wanted` 数组里的图标，白名单外的 <i data-lucide="x"> 不会被
// 替换成 svg——元素还在但里面是空的，表现为「按钮/标签只剩一圈 padding，看着像
// 没有图标」。这类 bug 肉眼极难归因（你会以为是自己没点到，或者 CSS 坏了），
// 而且白名单只在 renderIcons 一处定义，很容易漏。
//
// 实际踩过的两次：maximize-2（HTML 全屏按钮）、message-circle-question（待回答
// toast 的问号图标，从上线起就是空的）。
//
// 规则：app.js / index.html 里出现的每个 data-lucide 字面量都必须在白名单里。
// 不确定某个容器有没有调 renderIcons，就别用 <i data-lucide>，直接写原始内联
// SVG 常量（CODE_COPY_ICON / HTML_FS_ICON 那样）。
const fs = require('fs');
const path = require('path');

const WEB = path.resolve(__dirname, '../../web');
// 剥注释，免得 // ... <i data-lucide="xxx"> ... 这种说明文字被当成真用法
const stripJs = (t) => t.replace(/\/\*[\s\S]*?\*\//g, '').replace(/(^|[^:])\/\/[^\n]*/g, '$1');
const app = stripJs(fs.readFileSync(path.join(WEB, 'app.js'), 'utf8'));
const html = fs.readFileSync(path.join(WEB, 'index.html'), 'utf8').replace(/<!--[\s\S]*?-->/g, '');

const m = app.match(/const wanted = \[([\s\S]*?)\];/);
if (!m) throw new Error('抠不到 renderIcons 的 wanted 白名单');
const whitelist = new Set([...m[1].matchAll(/'([\w-]+)'/g)].map((x) => x[1]));

const used = new Map();
for (const [name, s] of [['app.js', app], ['index.html', html]]) {
  for (const re of s.matchAll(/data-lucide=(?:"([^"]+)"|'([^']+)'|`([^`]+)`)/g)) {
    const v = re[1] || re[2] || re[3];
    if (!v || v.includes('${')) continue; // 模板拼出来的名字运行时才知道，放行
    if (!used.has(v)) used.set(v, new Set());
    used.get(v).add(name);
  }
}

let bad = 0;
console.log(`白名单 ${whitelist.size} 条，用到 ${used.size} 个图标`);
for (const [icon, files] of [...used].sort()) {
  const hit = whitelist.has(icon);
  if (!hit) bad++;
  console.log(`  ${hit ? '✓' : '✗'} data-lucide="${icon}"  [${[...files].join(', ')}]`);
}

// 工具条里的图标不该依赖 lucide——那里没调过 renderIcons。
const fsIcon = app.match(/fs\.innerHTML = (.*?);/);
console.log('\n另：全屏按钮是否用了原始 SVG（不依赖 lucide）');
const usesRaw = !!fsIcon && !fsIcon[1].includes('data-lucide');
console.log(`  ${usesRaw ? '✓' : '✗'} ${fsIcon ? fsIcon[1] : '(找不到 fs.innerHTML)'}`);
if (!usesRaw) bad++;

console.log(bad ? `\nicons: ${bad} 处问题` : '\nicons: 全部通过');
process.exit(bad ? 1 : 0);
