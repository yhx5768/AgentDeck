package server

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"cc-webui/internal/runner"
	"cc-webui/internal/store"
)

// Worker notifications (incidents + escalations).
//
// A worker cannot report its own failure: when its turn crashes, its process
// hangs, or it parks on a gate only a person can lift, the worker is the thing
// that is stuck. The runtime is the supervisor that stays alive, so it detects
// all three (see runner/notify.go) and hands them here. This file routes them
// to the workspace main agent(s) as A2A messages, so the coordinator learns
// about a broken or blocked worker instead of finding out hours later.
//
// Routing is keyed on the board: only a session bound to a card (i.e. one the
// main agent dispatched) reports back. The message carries the card id, and a
// one-line note lands on the card's timeline. Notifications are rate-limited so
// a crash loop cannot become a notification storm.

// incidentFrom is the address the runtime notifications are sent from. It shows
// up in the peer bubble's role tag on the main agent's side.
const incidentFrom = "runtime"

// notifyLimiter bounds the notification rate: a per-worker cooldown plus a
// global hourly cap.
type notifyLimiter struct {
	mu        sync.Mutex
	perWorker map[string]time.Time
	recent    []time.Time
}

func (l *notifyLimiter) allow(sessionID string, now time.Time, cooldown time.Duration, hourlyCap int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.perWorker == nil {
		l.perWorker = map[string]time.Time{}
	}
	cut := now.Add(-time.Hour)
	kept := l.recent[:0]
	for _, t := range l.recent {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	l.recent = kept
	if hourlyCap > 0 && len(l.recent) >= hourlyCap {
		return false
	}
	if last, ok := l.perWorker[sessionID]; ok && now.Sub(last) < cooldown {
		return false
	}
	l.perWorker[sessionID] = now
	l.recent = append(l.recent, now)
	// Keep the per-worker map from growing without bound on a long-lived server.
	if len(l.perWorker) > 1024 {
		for k, t := range l.perWorker {
			if now.Sub(t) > 24*time.Hour {
				delete(l.perWorker, k)
			}
		}
	}
	return true
}

// workerNotifier is the runner callback set wired to this server. The callbacks
// are invoked from the runner's goroutines, so each spawns its own goroutine
// and never blocks the turn it describes.
func (s *Server) workerNotifier() runner.Notifier {
	return runner.Notifier{
		TurnError: s.onWorkerTurnError,
		Stalled:   s.onWorkerStalled,
		Ask:       s.onWorkerAsk,
	}
}

func (s *Server) onWorkerTurnError(sessionID, lastErr string) {
	go s.notifyWorker(sessionID, "worker 异常", func(sess *store.Session, task map[string]any) (string, string) {
		return fmt.Sprintf("worker 异常结束\n- 错误：%s", truncateRunes(strings.TrimSpace(lastErr), 600)),
			"worker 异常结束：" + truncateRunes(a2aFirstLine(lastErr), 200)
	})
}

func (s *Server) onWorkerStalled(sessionID string, silentFor time.Duration) {
	go s.notifyWorker(sessionID, "worker 卡住", func(sess *store.Session, task map[string]any) (string, string) {
		mins := int(silentFor.Round(time.Minute).Minutes())
		if mins < 1 {
			mins = 1
		}
		return fmt.Sprintf("worker 卡住：已静默 %d 分钟没有产出（进程可能挂死或模型无响应）", mins),
			fmt.Sprintf("worker 卡住：静默 %d 分钟", mins)
	})
}

func (s *Server) onWorkerAsk(sessionID string, pr *runner.PermissionRequest) {
	if pr == nil {
		return
	}
	tool := pr.Tool
	detail := ""
	if tool == "AskUserQuestion" {
		detail = questionSummary(pr.Input)
	}
	go s.notifyWorker(sessionID, "worker 提问", func(sess *store.Session, task map[string]any) (string, string) {
		var b strings.Builder
		b.WriteString("worker 停在一个人才能解的门上\n")
		if tool == "AskUserQuestion" {
			b.WriteString("- 提问：")
			if detail == "" {
				b.WriteString("（问题内容见会话）")
			} else {
				b.WriteString(detail)
			}
		} else {
			fmt.Fprintf(&b, "- 权限审批：%s", tool)
		}
		b.WriteString("\n- 说明：这道门只有用户能解，界面顶部横幅已经提示用户；你不要替用户回答。")
		b.WriteString("\n- 建议：board_comment 记录这次卡点；如需人工介入，board_update 把状态置「待人工」。")
		line := "worker 等待人工：" + tool
		if detail != "" {
			line += " · " + truncateRunes(a2aFirstLine(detail), 120)
		}
		return b.String(), line
	})
}

// notifyWorker is the shared path: resolve the worker session, its bound card
// and the main-agent targets; rate-limit; then deliver one A2A message per
// target and note it on the card. build returns (message body, one-line note).
func (s *Server) notifyWorker(sessionID, kind string, build func(*store.Session, map[string]any) (string, string)) {
	settings := s.cfg().Notify()
	if !settings.Enabled {
		return
	}
	sess, task, targets, ok := s.workerNotifyContext(sessionID)
	if !ok {
		return
	}
	if !s.notifyRate.allow(sessionID, time.Now(), settings.WorkerCooldown, settings.HourlyCap) {
		return
	}
	body, note := build(sess, task)
	if strings.TrimSpace(body) == "" {
		return
	}
	taskID, _ := task["id"].(string)
	for _, target := range targets {
		addr := sessionAddress(target)
		if _, _, err := s.deliverA2A(incidentFrom, addr, body); err != nil {
			log.Printf("worker notify %s -> %s: %v", sessionID, addr, err)
		}
	}
	if taskID != "" {
		s.appendBoardNote(taskID, kind, note)
	}
}

// workerNotifyContext resolves the routing for a worker notification: the
// worker session, the card it is bound to, and the main-agent targets. ok is
// false when there is nothing to notify (unknown/archived/group session, no
// bound card, or no main agent).
func (s *Server) workerNotifyContext(sessionID string) (*store.Session, map[string]any, []*store.Session, bool) {
	sess := s.st.Get(sessionID)
	if sess == nil || sess.GroupID != "" || sess.Archived {
		return nil, nil, nil, false
	}
	if s.boardDB == nil {
		return nil, nil, nil, false
	}
	task, err := s.boardDB.TaskForSession(sessionID)
	if err != nil {
		log.Printf("worker notify: board lookup %s: %v", sessionID, err)
		return nil, nil, nil, false
	}
	if task == nil {
		return nil, nil, nil, false // not a dispatched worker
	}
	targets := s.mainAgentTargets(sessionID)
	if len(targets) == 0 {
		return nil, nil, nil, false
	}
	return sess, task, targets, true
}

// mainAgentTargets lists the sessions a worker notification should reach: every
// main agent except the worker itself. A group shadow session is never a target.
func (s *Server) mainAgentTargets(exclude string) []*store.Session {
	var out []*store.Session
	for _, sess := range s.st.List() {
		if sess.MainAgent && sess.ID != exclude && sess.GroupID == "" && !sess.Archived {
			out = append(out, sess)
		}
	}
	return out
}

// appendBoardNote records a runtime notification on a card's timeline. Failures
// are swallowed: the A2A delivery already happened and a missing card must not
// turn into a retry storm.
func (s *Server) appendBoardNote(taskID, kind, text string) {
	if s.boardDB == nil || strings.TrimSpace(taskID) == "" || strings.TrimSpace(text) == "" {
		return
	}
	if _, err := s.boardDB.AppendEvent(taskID, kind, text, "", "incident"); err != nil {
		log.Printf("worker notify: board note %s: %v", taskID, err)
	}
	s.broadcastAgentBoard()
}

// questionSummary renders an AskUserQuestion input's questions as one line.
func questionSummary(input map[string]any) string {
	if input == nil {
		return ""
	}
	raw, ok := input["questions"]
	if !ok {
		return ""
	}
	var questions []any
	switch qs := raw.(type) {
	case []any:
		questions = qs
	case []map[string]any:
		for _, q := range qs {
			questions = append(questions, q)
		}
	default:
		return ""
	}
	var parts []string
	for _, q := range questions {
		m, ok := q.(map[string]any)
		if !ok {
			continue
		}
		if text, _ := m["question"].(string); strings.TrimSpace(text) != "" {
			parts = append(parts, strings.TrimSpace(text))
		}
	}
	return truncateRunes(strings.Join(parts, " / "), 300)
}

// truncateRunes caps s at n runes, appending an ellipsis when it cut anything.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
