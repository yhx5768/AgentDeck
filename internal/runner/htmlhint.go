package runner

import (
	"strconv"
	"strings"

	"cc-webui/internal/config"
)

// 报告/可视化输出提示词——让模型知道这个界面既能渲染一整页 HTML，也有
// （AgentDeck 的）产物托管机制，从而把完整报告发成产物、把聊天留给结论。
//
// 策略：完整报告/仪表盘优先发布成产物（artifact_publish），聊天只回链接 +
// 3–5 条结论；单屏能看完的小型图表/示意图才走内联 html 围栏；没有产物机制时
// 回退内联 HTML。流程图/时序图等结构化示意图仍走 mermaid 围栏。
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
//	codex / ducx    -c instructions=<text>（见 codexInstructions）
//	opencode        没有 append 语义（v1 prompt body 的 system 字段是**覆盖**
//	                agent 自己的 system prompt，见 opencodeapi.PromptInput），
//	                改走隔离配置目录的 instructions（HTML_RENDER_HINT.md，
//	                见 ocrules.go）。
const htmlRenderHint = `【报告与可视化：产物优先，内联只做小型图】

这个对话界面能直接渲染一整页 HTML（沙箱 iframe，可跑脚本、内联 SVG 图表、
canvas、动画和交互控件），也能渲染 Mermaid。但**完整报告不应该塞进聊天**：

- **完整报告 / 仪表盘 / 多图表、多段分析 / 需要长期保存或分享的内容**：优先发布成
  会话或看板的 HTML 产物。当前环境有产物机制时就用它（AgentDeck 是 artifact_publish，
  把产物挂到对应的看板任务；其他环境有对应机制则用各自的方式）。聊天里只回复
  **产物链接 + 3–5 条关键结论**，不要把完整报告再压缩成一份内联 HTML 重复输出。
- **单屏能看完的小型图表 / 示意图 / 信息图**：才直接在聊天里输出 markdown 代码块，
  语言标记写 html，内容是一份完整的 HTML 文档（三个反引号包住 html 再跟文档）。
  不要写 .html 文件让用户自己去开——那样用户在聊天里根本看不到。
- 如果当前会话/环境**没有产物或看板机制**（例如没有可挂载的看板任务、也没有别的
  产物接口）：回退到内联 HTML（或环境里已有的可用方式）交付完整内容，不要因为
  优先产物就把报告丢掉、只给一个打不开的链接。

**不要为了展示把完整报告复制成聊天里的长 HTML**——那会把对话刷屏，也不利于保存和
分享。

内联 HTML 必须：

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

// appendSystemPrompt injects the per-session role prompt and/or the HTML render
// hint into a spawn's args when the engine has a real "append" flag and the
// user hasn't already supplied one in cli.Args.
//
// The two texts share a single --append-system-prompt argument because the CLI
// accepts exactly one: passing the flag twice would make the last occurrence
// win and silently drop the other text. Role prompt first (it defines the
// worker's identity) and the interface hint second.
//
// hasArg matters: a user who put their own --append-system-prompt in config.yaml
// must not end up with two (CLIs generally take the last one, so ours would
// silently win and theirs would be ignored). That also means a session role
// prompt is not injected when cli.Args carries its own flag — the user's
// explicit config wins wholesale.
//
// The dedup check looks at cli.Args as well as args rather than trusting the
// caller to have merged them already: all call sites do append cli.Args first,
// but mcode's ordering was once off by a few lines and the check silently
// stopped seeing the user's flag. Making the function order-independent removes
// that whole class of bug.
func (m *Manager) appendSystemPrompt(args []string, cli *config.CLI, rolePrompt string) []string {
	if hasArg(args, "--append-system-prompt") {
		return args
	}
	if cli != nil && hasArg(cli.Args, "--append-system-prompt") {
		return args
	}
	text := composeSystemPrompt(m.htmlHintEnabled(), rolePrompt)
	if text == "" {
		return args
	}
	return append(args, "--append-system-prompt", text)
}

// composeSystemPrompt joins the parts that ride the single append flag: the
// role prompt (when set) and the HTML render hint (when enabled). Empty when
// neither applies, so callers never append an empty argument.
func composeSystemPrompt(withHint bool, rolePrompt string) string {
	var parts []string
	if rp := strings.TrimSpace(rolePrompt); rp != "" {
		parts = append(parts, rp)
	}
	if withHint {
		parts = append(parts, htmlRenderHint)
	}
	return strings.Join(parts, "\n\n")
}

func (m *Manager) htmlHintEnabled() bool {
	return m.cm == nil || m.cm.Get().HTMLHintEnabled()
}

// codexInstructions renders the role prompt and/or hint as a codex
// `-c instructions=...` override. codex has no append flag, and its two
// neighbouring keys do not fit:
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
func codexInstructions(withHint bool, rolePrompt string) string {
	return "instructions=" + strconv.Quote(composeSystemPrompt(withHint, rolePrompt))
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
