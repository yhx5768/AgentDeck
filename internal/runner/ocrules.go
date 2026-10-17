package runner

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// opencode 的 HTML 渲染约定走它自己的 Rules / instructions 机制，把一段文字
// 并进每一轮的 LLM context。
//
// 为什么不走 --append-system-prompt：`opencode run` 没有 append 语义的 flag，
// v1 prompt body 的 system 字段是**覆盖** agent 自身 system prompt 的（见
// opencodeapi.PromptInput），填进去会把 opencode 的 agent 指令顶掉，比不注入更糟。
//
// 为什么不用 ~/.config/opencode/AGENTS.md（opencode 的全局规则文件）：
// 那是 per-user 的，写进去意味着**用户在终端里自己跑的 opencode 和 TUI 也会吃到**
// ——劫持别人的工具，不是 cc-webui 的能力。
//
// 走 instructions 的关键在于 opencode 会读 OPENCODE_CONFIG_DIR 指向目录下的
// opencode.json，且配置是**合并不是替换**（实测：把 CONFIG_DIR 指到一个只有
// 指令、没有配置文件的目录，用户的 provider / mcp 配置照常生效）。所以只给
// cc-webui 起的 serve 进程设这个 env，约定就只对 cc-webui 里的 opencode 会话
// 生效，终端和 TUI 一个字节都不受影响。
//
// 实测三连（同一个 HOME 连续跑，问一个只存在于指令文件里的哨兵串）：
// 无 CONFIG_DIR → 无；带 CONFIG_DIR → 有；再回到无 CONFIG_DIR → 无。
// 第三步排除了「缓存把指令泄漏给 TUI」这个最该担心的问题。
//
// 已知取舍：instructions 是配置里的一个键，而同名键是**后者覆盖**。如果用户
// 自己在 ~/.config/opencode/*.json(c) 里也配了 instructions，cc-webui 里的
// 会话会用 ours 替换掉那份，而不是追加。合并它需要解析用户的 JSONC（含注释），
// 代价远大于收益；当前用户配置里没有这个键。

// opencodeConfigDir returns the directory cc-webui points OPENCODE_CONFIG_DIR
// at. XDG 不用：opencode 文档给的是 ~/.config 这个字面路径，自己推断 ~/.xdg
// 反而可能指到一个它不读的地方——静默不生效比报错更难查。
func opencodeConfigDir() (string, error) {
	home := os.Getenv("HOME")
	if home == "" {
		return "", fmt.Errorf("HOME is not set")
	}
	return filepath.Join(home, ".config", "cc-webui", "opencode"), nil
}

// ensureOpencodeRule materialises cc-webui's opencode config dir — one config
// file whose `instructions` points at the hint — and returns the dir to hand the
// serve child as OPENCODE_CONFIG_DIR.
//
// Returns "" when the hint is switched off or the files could not be written;
// the caller must then leave the env alone and run opencode with the user's
// normal configuration. Degrading output quality (the model writes an .html file
// the user opens by hand) is better than failing the turn over $HOME.
//
// No marker block is needed here: this directory belongs to cc-webui, nothing
// else writes to it, and both files are rewritten whole. Read-back-and-compare
// keeps an identical run from churning mtimes on a directory the user may well
// be looking at while debugging.
func (m *Manager) ensureOpencodeRule() string {
	if m.cm != nil && !m.cm.Get().HTMLHintEnabled() {
		return ""
	}
	dir, err := opencodeConfigDir()
	if err != nil {
		log.Printf("opencode 隔离配置目录不可用（opencode 会话将不注入 HTML 渲染约定）: %v", err)
		return ""
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("opencode 隔离配置目录创建失败 %s: %v", dir, err)
		return ""
	}

	hintPath := filepath.Join(dir, "HTML_RENDER_HINT.md")
	hint := strings.TrimSpace(htmlRenderHint) + "\n"
	if err := writeIfChanged(hintPath, []byte(hint)); err != nil {
		log.Printf("写 %s 失败: %v", hintPath, err)
		return ""
	}

	// Only one key, and it points at a path inside our own directory, so the
	// user's provider / mcp / theme settings keep merging in normally.
	cfg := fmt.Sprintf(
		"{\n  \"$schema\": \"https://opencode.ai/config.json\",\n  \"instructions\": [%q]\n}\n",
		hintPath)
	cfgPath := filepath.Join(dir, "opencode.json")
	if err := writeIfChanged(cfgPath, []byte(cfg)); err != nil {
		log.Printf("写 %s 失败: %v", cfgPath, err)
		return ""
	}
	return dir
}

// writeIfChanged writes data only when it differs from what is already there.
func writeIfChanged(path string, data []byte) error {
	if old, err := os.ReadFile(path); err == nil && string(old) == string(data) {
		return nil
	}
	return os.WriteFile(path, data, 0o644)
}
