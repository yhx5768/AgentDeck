// jsdom 用的 md 子集：粗体 / 行内 code / 链接 / 列表 / 换行；真实实现是 app.js 的 marked + DOMPurify。
// 单独放在普通 JS 文件里（不塞进 pa_harness 的模板字符串），反引号和正则转义才不会被吃掉。
function md(s, opts) {
  window.__mdCalls.push({ text: String(s || ''), opts: opts || null });
  let h = escapeHtml(String(s || ''));
  h = h.replace(/`([^`]+)`/g, '<code>$1</code>')
       .replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>')
       .replace(/\[([^\]]+)\]\(([^)]+)\)/g, '<a href="$2">$1</a>');
  h = h.replace(/^([-*]) (.*)$/gm, '<li>$2</li>');
  if (/<li>/.test(h)) h = '<ul>' + h + '</ul>';
  h = h.replace(/\n/g, '<br>');
  return h;
}
