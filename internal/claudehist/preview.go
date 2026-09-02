package claudehist

import (
	"strings"
	"time"
)

// PreviewMessage 是导入预览里的一条消息。
type PreviewMessage struct {
	Role string    `json:"role"` // user | assistant
	Text string    `json:"text"`
	TS   time.Time `json:"ts"`
}

// previewTextMax 单条消息预览文本上限，防止超大工具输出把弹窗撑爆。
const previewTextMax = 600

func truncatePreview(s string) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= previewTextMax {
		return string(r)
	}
	return string(r[:previewTextMax]) + "…"
}

// Preview 返回会话 transcript 最近的消息（最多 maxMessages 条，按时间正序）。
// 口径与 Convert 一致：跳过 sidechain/meta、tool_result 回显（无文本的 user
// 行）和系统注入的 xml/中断文本；assistant 的消息 id 相同的内容块行折叠为一条。
func Preview(cwd, sessionID string, maxMessages int) []PreviewMessage {
	return previewFile(HistoryPath(cwd, sessionID), maxMessages)
}

// previewFile 是 Preview 的路径直通形式，便于注入测试 fixture。
func previewFile(path string, maxMessages int) []PreviewMessage {
	lines := scanFile(path)
	seenMsg := map[string]bool{}
	var out []PreviewMessage
	for i := len(lines) - 1; i >= 0 && len(out) < maxMessages; i-- {
		l := lines[i]
		if l.IsSidechain || l.IsMeta {
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, l.Timestamp)
		if err != nil {
			ts = time.Time{}
		}
		switch l.Type {
		case "user":
			t := strings.TrimSpace(textOf(l.Message.Content))
			if t == "" || strings.HasPrefix(t, "<") || strings.HasPrefix(t, "[Request interrupted") {
				continue
			}
			out = append(out, PreviewMessage{Role: "user", Text: truncatePreview(t), TS: ts})
		case "assistant":
			key := l.Message.ID
			if key == "" {
				key = "line:" + l.UUID
			}
			if seenMsg[key] {
				continue
			}
			seenMsg[key] = true
			text := truncatePreview(textOf(l.Message.Content))
			if text == "" {
				// 先扫到的是同 id 的 thinking/tool_use 行：从相邻行里找带文本的一条
				for j := i - 1; j >= 0; j-- {
					prev := lines[j]
					if prev.Type != "assistant" || prev.Message.ID != l.Message.ID {
						break
					}
					if t := truncatePreview(textOf(prev.Message.Content)); t != "" {
						text = t
						break
					}
				}
			}
			if text == "" {
				continue
			}
			out = append(out, PreviewMessage{Role: "assistant", Text: text, TS: ts})
		}
	}
	// 反向收集后调回时间正序
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}
