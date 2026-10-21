package runner

import (
	"strconv"
	"strings"

	"cc-webui/internal/config"
)

// HTML 渲染提示词——让模型知道「这个聊天界面能把一页 HTML 渲染成活的页面」。
//
// 为什么用 --append-system-prompt 注入，而不是只写在 AGENTS.md 里：
// AGENTS.md 是**仓库作用域**的。会话的工作目录不在 cc-webui 仓库里时（比如
// /tmp/scratch、某个前端项目），那份约定根本不会被加载，模型就完全不知道这个
// 界面有这个能力——实测过一次：cwd 在 /tmp 下的会话产出的页面自带一套
// `:root` 变量，跟 app 的深浅色主题不联动。注入是 per-session 的，不依赖 cwd。
//
// 覆盖范围（实测各引擎的 CLI）：
//
//	claude / qoder  --append-system-prompt <text>
//	mcode           --append-system-prompt <text>
//	codex / ducx    -c instructions=<text>（见 codexInstructionsArg）
//	opencode        没有等价 flag。v1 prompt body 里的 system 字段是 **覆盖**
//	                agent 自己的 system prompt（见 opencodeapi.PromptInput 的
//	                注释），填进去会把 opencode 的 agent 指令顶掉，比不注入更糟，
//	                所以 opencode 暂时不注入，opencode 会话只能靠 cwd 在仓库里
//	                读到 AGENTS.md 那份约定。TODO: 等 opencode 给出 append 语义
//	                的入口（或确认 system 是追加）再补上。
const htmlRenderHint = `【本界面的 HTML 渲染能力】

这个对话界面能把一整页 HTML 直接渲染成聊天里的**活的页面**（沙箱 iframe，可跑脚本、
内联 SVG 图表、canvas、动画和交互控件），不需要用户去开浏览器、不需要写文件到磁盘。

用户要图表、仪表盘、报表页、幻灯片式排版、示意图、信息图、带视觉样式的表格，或任何
"多元素的可视化"时，**直接输出一个 markdown 代码块，语言标记写 html**，内容是一份完整
的 HTML 文档（就是三个反引号包住 html 再跟文档的那种写法）。不要写成 .html 文件让
用户自己去开——那样用户在聊天里根本看不到。

这份文档必须：

- **完全自包含**：不许外部 CSS / JS / 字体 / 任何网络请求。
- 含 <!doctype html>、<html>、<head>、<body>。
- 图表用**内联 SVG** 或页面自己的 script，不要引远程图表库。
- 别在页面里做内层滚动容器（会劫持聊天的滚动）；宽表格自己套一个横向滚动盒子。
- 给图表留出空间，大约 640x320 或更宽。

配色：**优先用下面这些宿主注入的 CSS 变量，不要硬编码颜色**，这样页面会跟随 app 的
深浅色主题：
  --background（页面底） --surface（卡片/分区底） --border（描边）
  --foreground（正文）   --muted（次要文字）      --accent（强调/图表主色）
同时**一律带上兜底值**，写成 var(--background, #fff) 这样的形式——变量没注入时也
不至于裸奔。

另外，流程图 / 时序图 / 状态图 / 类图 / ER 图 / 甘特图这类结构化示意图，直接用三个
反引号包住 mermaid 写 Mermaid 源码（语言标记写 mermaid，第一行写图类型，例如
flowchart LR）。界面上会有「渲染」按钮把它画成图，跟随深浅色主题。这类图不要用
ASCII 字符画，也不要塞进 HTML 页面里。`

// appendHTMLRenderHint injects the hint into a spawn's args when the engine has a
// real "append" flag and the user hasn't already supplied one in cli.Args.
// hasArg matters: a user who put their own --append-system-prompt in config.yaml
// must not end up with two (CLIs generally take the last one, so ours would
// silently win and theirs would be ignored).
//
// The dedup check looks at cli.Args as well as args rather than trusting the
// caller to have merged them already: all three call sites do append cli.Args
// first, but mcode's ordering was once off by a few lines and the check silently
// stopped seeing the user's flag. Making the function order-independent removes
// that whole class of bug.
func (m *Manager) appendHTMLRenderHint(args []string, cli *config.CLI) []string {
	if !m.htmlHintEnabled() {
		return args
	}
	if hasArg(args, "--append-system-prompt") {
		return args
	}
	if cli != nil && hasArg(cli.Args, "--append-system-prompt") {
		return args
	}
	return append(args, "--append-system-prompt", htmlRenderHint)
}

func (m *Manager) htmlHintEnabled() bool {
	return m.cm == nil || m.cm.Get().HTMLHintEnabled()
}

// codexInstructionsArg renders the hint as a codex `-c instructions=...`
// override. codex has no append flag, and its two neighbouring keys do not fit:
//
//   - developer_instructions is stored with the thread at creation time, so
//     `exec resume` keeps the old value and silently ignores the override —
//     verified against a fake endpoint: a new session carries the marker, a
//     resume of the same thread does not.
//   - model_instructions_file replaces the model's built-in instructions.
//
// `instructions` is read from config on every run and rendered as a separate
// developer message before the built-in blocks, so resumed sessions get it too.
// The value must be TOML, not JSON; strconv.Quote's escapes are a subset TOML
// accepts for this text (quotes and newlines only — no raw control bytes).
func codexInstructionsArg() string {
	return "instructions=" + strconv.Quote(htmlRenderHint)
}

// hasConfigOverride reports whether args already carries `-c <key>=...` (or the
// --config spelling, with or without `=`), so callers do not clobber a value the
// user supplied in cli.args.
func hasConfigOverride(args []string, key string) bool {
	for i := 0; i < len(args); i++ {
		var v string
		switch a := args[i]; {
		case a == "-c" || a == "--config":
			if i+1 < len(args) {
				i++
				v = args[i]
			}
		case strings.HasPrefix(a, "-c="):
			v = a[len("-c="):]
		case strings.HasPrefix(a, "--config="):
			v = a[len("--config="):]
		}
		name := strings.TrimSpace(v)
		if j := strings.IndexAny(name, "= \t"); j >= 0 {
			name = name[:j]
		}
		if name == key {
			return true
		}
	}
	return false
}
