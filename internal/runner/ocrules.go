package runner

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// opencode 的全局规则文件。opencode 的 Rules 机制读它，并把它并进每一轮的
// LLM context（https://opencode.ai/docs/rules/，原文："This gets applied
// across all opencode sessions"）。
//
// 为什么 opencode 要走文件而不是 --append-system-prompt：
//   - `opencode run` 没有 append 语义的 flag；v1 prompt body 的 system 字段
//     是**覆盖** agent 自身 system prompt 的（见 opencodeapi.PromptInput），
//     填进去会把 opencode 的 agent 指令顶掉，比不注入更糟。
//   - 文件路径是 per-user 而不是 per-repo，绕开了 AGENTS.md 的仓库作用域问题：
//     会话 cwd 不在 cc-webui 仓库里时仓库那份根本不加载。
//   - 实测过：文件放这里，用户自己在终端跑的 opencode 同样生效——这一点
//     --append-system-prompt 做不到，它只覆盖 cc-webui 起的进程。
//
// 已知代价：全局规则是「第一个匹配的胜出」，同一时刻整个用户只有一份全局
// 规则。下面的迁移逻辑负责不吞掉用户已有的 ~/.claude/CLAUDE.md。
const (
	ocRuleBegin = "<!-- cc-webui:html-render-hint:begin -->"
	ocRuleEnd   = "<!-- cc-webui:html-render-hint:end -->"
	// ocClaudeNote 解释为什么 CLAUDE.md 的内容会被搬进来。
	ocClaudeNote = "以下内容原先在 ~/.claude/CLAUDE.md，由 cc-webui 原样迁入此处。\n" +
		"原因：opencode 的全局规则是「第一个匹配的胜出」，两个文件都在时它只读\n" +
		"~/.config/opencode/AGENTS.md，CLAUDE.md 会被静默忽略。想改这些规则，直接改本文件。\n"
)

// opencodeGlobalRulePath returns ~/.config/opencode/AGENTS.md.
// XDG 不用：opencode 文档给的就是这个字面路径，自己推断 ~/.xdg 反而可能
// 写到一个它不读的地方——写错文件比不写更糟（看起来成功了，其实没生效）。
func opencodeGlobalRulePath() (string, error) {
	home := os.Getenv("HOME")
	if home == "" {
		return "", errors.New("HOME is not set")
	}
	return filepath.Join(home, ".config", "opencode", "AGENTS.md"), nil
}

func (m *Manager) ensureOpencodeRule() {
	if m.cm != nil && !m.cm.Get().HTMLHintEnabled() {
		return
	}
	changed, err := ensureOpencodeGlobalRule(htmlRenderHint)
	switch {
	case err != nil:
		log.Printf("opencode 全局规则写入失败（opencode 会话将不注入 HTML 渲染约定）: %v", err)
	case changed:
		if p, perr := opencodeGlobalRulePath(); perr == nil {
			log.Printf("已把 HTML 渲染约定写入 opencode 全局规则 %s", p)
		}
	}
}

// ensureOpencodeGlobalRule makes htmlRenderHint present in opencode's global
// rules file. Idempotent in both directions: the block between the markers is
// owned by cc-webui and is rewritten to match; everything outside is never
// touched, so the user's own rules in that file survive.
//
// Returns (changed, err). changed=false means the file already said exactly
// this and was left untouched — callers must not write on that path, or every
// turn would churn the user's mtime.
func ensureOpencodeGlobalRule(hint string) (bool, error) {
	path, err := opencodeGlobalRulePath()
	if err != nil {
		return false, err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, fmt.Errorf("create %s: %w", dir, err)
	}

	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	existing := string(raw)

	var next string
	switch {
	case strings.Contains(existing, ocRuleBegin) && strings.Contains(existing, ocRuleEnd):
		next = replaceRuleBlock(existing, hint)
	case strings.TrimSpace(existing) == "":
		// Creating the file. This is the one moment that would make opencode
		// stop reading ~/.claude/CLAUDE.md, so bring that content along.
		next = adoptClaudeRules() + "\n" + ruleBlockInner(hint) + "\n"
	default:
		// No block yet, and the file already has the user's own rules. Append
		// ours after them; nothing above moves.
		next = existing + "\n" + ruleBlockInner(hint) + "\n"
	}
	if next == existing {
		return false, nil
	}
	if err := os.WriteFile(path, []byte(next), 0o644); err != nil {
		return false, fmt.Errorf("write %s: %w", path, err)
	}
	return true, nil
}

// ruleBlockInner renders just the markers and the hint, with no leading or
// trailing newline. Callers own the surrounding whitespace, which is what
// makes replaceRuleBlock byte-stable: head and tail are slices of the original
// file, so re-running with unchanged text reproduces the file exactly and the
// idempotency check can say "nothing to do".
func ruleBlockInner(hint string) string {
	return ocRuleBegin + "\n" + strings.TrimSpace(hint) + "\n" + ocRuleEnd
}

// replaceRuleBlock swaps only what is between the markers. Everything before
// the begin marker and after the end marker survives byte-for-byte.
func replaceRuleBlock(existing, hint string) string {
	i := strings.Index(existing, ocRuleBegin)
	j := strings.Index(existing, ocRuleEnd)
	if i < 0 || j < 0 || j < i {
		return existing
	}
	head := existing[:i]
	tail := existing[j+len(ocRuleEnd):]
	return head + ruleBlockInner(hint) + tail
}

// adoptClaudeRules returns the contents of ~/.claude/CLAUDE.md when it exists,
// to be preserved into a global file we are about to create. Returns "" when
// there is nothing to preserve.
//
// This matters because opencode reads the global category first-match-wins:
// the moment ~/.config/opencode/AGENTS.md exists, ~/.claude/CLAUDE.md is
// silently ignored. Creating the file without carrying the old content over
// would quietly delete the user's global opencode rules. Only called when the
// file did not exist before — if it already existed, opencode was already
// preferring it over CLAUDE.md and none of that is ours to change.
func adoptClaudeRules() string {
	home := os.Getenv("HOME")
	if home == "" {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(home, ".claude", "CLAUDE.md"))
	if err != nil || strings.TrimSpace(string(raw)) == "" {
		return ""
	}
	return "\n" + ocClaudeNote + "\n" + string(raw) + "\n"
}
