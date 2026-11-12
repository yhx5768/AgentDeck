package server

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"cc-webui/internal/boarddb"
	"cc-webui/internal/runner"
	"cc-webui/internal/store"
)

// 「需要你」队列的只读聚合（docs/needs-you-rd.md §4.1 / 阶段 2）。
//
// 一条队列条目 = 一个需要人动手的动作，来源有两路：
//   - 全部会话的待决 prompt（runner 的 pending 注册表）：工具权限 → perm，
//     AskUserQuestion → ask；都是 live 条目，55s 内不处理 helper 就按超时处理。
//   - 看板里状态为「待人工」/「待验收」的卡 → blocked / review。
//
// 排序在服务端做，前端不重排：档间固定 perm > ask > blocked > review；档内
// perm/ask 按剩余 TTL 升序（最急的在前），blocked/review 按等待时间降序（等得最久的在前）。

const (
	needsKindPerm    = "perm"
	needsKindAsk     = "ask"
	needsKindBlocked = "blocked"
	needsKindReview  = "review"

	boardStatusBlocked = "待人工"
	boardStatusReview  = "待验收"

	// needsInputStrMax caps any single string inside a prompt's input before it
	// rides this (polled) response; the full payload stays on the per-session
	// permissions/pending endpoint.
	needsInputStrMax = 8000
)

var needsKindRank = map[string]int{
	needsKindPerm: 0, needsKindAsk: 1, needsKindBlocked: 2, needsKindReview: 3,
}

type needsYouItem struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`  // perm | ask | blocked | review
	State string `json:"state"` // live（55s 窗口内）| open（看板卡，无倒计时）

	// perm / ask：对应 POST /api/sessions/{session_id}/permissions/{pending_id}
	PendingID string         `json:"pending_id,omitempty"`
	Tool      string         `json:"tool,omitempty"`
	Input     map[string]any `json:"input,omitempty"`
	ToolUseID string         `json:"tool_use_id,omitempty"`

	SessionID   string `json:"session_id,omitempty"`
	SessionName string `json:"session_name,omitempty"`
	PeerName    string `json:"peer_name,omitempty"`

	TaskID     string `json:"task_id,omitempty"`
	TaskTitle  string `json:"task_title,omitempty"`
	TaskStatus string `json:"task_status,omitempty"`

	Title   string `json:"title"`
	Summary string `json:"summary,omitempty"`

	CreatedAt int64 `json:"created_at"`           // 到达时间（unix ms）
	ExpiresAt int64 `json:"expires_at,omitempty"` // 仅 live：窗口截止（unix ms）
}

func (s *Server) handleNeedsYou(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	items := []needsYouItem{}
	boardInfo := map[string]any{"ok": true}

	// 先收会话索引：名字/地址名，以及哪些会话不该进队列（群影子成员、已归档）。
	sessions := map[string]*store.Session{}
	for _, sess := range s.st.List() {
		sessions[sess.ID] = sess
	}

	// 看板卡的绑定关系：pending 条目借它挂到任务上（TaskForSession 会连带取
	// timeline/产物，所以每个会话只查一次）。
	taskOf := map[string]map[string]any{}
	taskFor := func(sessionID string) map[string]any {
		if s.boardDB == nil {
			return nil
		}
		if t, ok := taskOf[sessionID]; ok {
			return t
		}
		t, _ := s.boardDB.TaskForSession(sessionID)
		taskOf[sessionID] = t
		return t
	}

	for _, pr := range s.mgr.PermissionPendingAll() {
		sess := sessions[pr.SessionID]
		if sess == nil || sess.GroupID != "" || sess.Archived {
			continue
		}
		it := needsYouItem{
			ID:        pr.Tool + ":" + pr.ID,
			State:     "live",
			PendingID: pr.ID,
			Tool:      pr.Tool,
			Input:     clipNeedsInput(pr.Input),
			ToolUseID: pr.ToolUseID,
			CreatedAt: pr.CreatedAt.UnixMilli(),
			ExpiresAt: pr.CreatedAt.Add(runner.PermissionTimeout).UnixMilli(),
		}
		if pr.Tool == askToolName {
			it.Kind = needsKindAsk
			it.ID = needsKindAsk + ":" + pr.ID
			it.Title = askTitle(pr.Input)
		} else {
			it.Kind = needsKindPerm
			it.ID = needsKindPerm + ":" + pr.ID
			it.Title = permTitle(pr.Tool, pr.Input)
		}
		fillNeedsSession(&it, sess)
		if t := taskFor(sess.ID); t != nil {
			it.TaskID, _ = t["id"].(string)
			it.TaskTitle, _ = t["title"].(string)
			it.TaskStatus, _ = t["status"].(string)
		}
		items = append(items, it)
	}

	if s.boardDB == nil {
		boardInfo["ok"] = false
		boardInfo["error"] = "看板数据库不可用"
		if s.boardDBErr != nil {
			boardInfo["error"] = "看板数据库不可用: " + s.boardDBErr.Error()
		}
	} else if tasks, err := s.boardDB.AttentionTasks(boardStatusBlocked, boardStatusReview); err != nil {
		boardInfo["ok"] = false
		boardInfo["error"] = "读取看板失败: " + err.Error()
	} else {
		for _, t := range tasks {
			items = append(items, boardNeedsItem(t, sessions))
		}
	}

	sortNeedsItems(items)
	counts := map[string]int{"all": len(items), needsKindPerm: 0, needsKindAsk: 0, needsKindBlocked: 0, needsKindReview: 0}
	for _, it := range items {
		counts[it.Kind]++
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":       items,
		"counts":      counts,
		"board":       boardInfo,
		"server_time": now.UnixMilli(),
		"window_ms":   runner.PermissionTimeout.Milliseconds(),
	})
}

func fillNeedsSession(it *needsYouItem, sess *store.Session) {
	if sess == nil {
		return
	}
	it.SessionID = sess.ID
	it.SessionName = sess.Name
	it.PeerName = sessionAddress(sess)
}

func boardNeedsItem(t boarddb.AttentionTask, sessions map[string]*store.Session) needsYouItem {
	it := needsYouItem{
		State:      "open",
		TaskID:     t.ID,
		TaskTitle:  t.Title,
		TaskStatus: t.Status,
		Title:      t.Title,
		Summary:    strings.TrimSpace(firstNonEmpty(t.StatusReason, t.Progress)),
	}
	if t.Status == boardStatusReview {
		it.Kind = needsKindReview
	} else {
		it.Kind = needsKindBlocked
	}
	it.ID = it.Kind + ":" + t.ID
	if sess := sessions[t.Session]; sess != nil && sess.GroupID == "" && !sess.Archived {
		fillNeedsSession(&it, sess)
	} else if t.Worker != "" {
		it.PeerName = t.Worker
	}
	if ts, ok := parseBoardTime(firstNonEmpty(t.Since, t.UpdatedAt)); ok {
		it.CreatedAt = ts.UnixMilli()
	}
	return it
}

// sortNeedsItems orders the queue (see the header comment): tier first, then
// urgency inside the tier. The id tie-break keeps the order stable across polls
// so rows do not shuffle while a person is reaching for one.
func sortNeedsItems(items []needsYouItem) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if ra, rb := needsKindRank[a.Kind], needsKindRank[b.Kind]; ra != rb {
			return ra < rb
		}
		if a.State == "live" && b.State == "live" {
			if a.ExpiresAt != b.ExpiresAt {
				return a.ExpiresAt < b.ExpiresAt
			}
		} else if a.CreatedAt != b.CreatedAt {
			return a.CreatedAt < b.CreatedAt
		}
		return a.ID < b.ID
	})
}

// parseBoardTime reads the board's local "2006-01-02 15:04:05" stamps.
func parseBoardTime(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05", v, time.Local)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// askTitle is the first question's text (what a person is being asked).
func askTitle(input map[string]any) string {
	if q := questionSummary(input); q != "" {
		if first, _, ok := strings.Cut(q, " / "); ok {
			return first
		}
		return q
	}
	return "模型提问"
}

// permTitle names a tool permission request: "<Tool>：<what it touches>".
func permTitle(tool string, input map[string]any) string {
	detail := ""
	for _, key := range []string{"command", "file_path", "path", "url", "pattern", "description", "prompt"} {
		if v, ok := input[key].(string); ok && strings.TrimSpace(v) != "" {
			detail = v
			break
		}
	}
	detail = truncateRunes(a2aFirstLine(detail), 120)
	if detail == "" {
		return tool
	}
	return tool + "：" + detail
}

// clipNeedsInput copies a prompt input, capping every string so a pasted file
// body in a Write request cannot make the polled queue response huge.
func clipNeedsInput(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = clipNeedsValue(v)
	}
	return out
}

func clipNeedsValue(v any) any {
	switch x := v.(type) {
	case string:
		return truncateRunes(x, needsInputStrMax)
	case map[string]any:
		return clipNeedsInput(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = clipNeedsValue(e)
		}
		return out
	case []map[string]any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = clipNeedsInput(e)
		}
		return out
	default:
		return v
	}
}
