package server

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cc-webui/internal/schedule"
)

// handleParseSchedule turns a natural-language description into concrete cron
// candidates. It is rule-based on purpose: cc-webui's server has no model
// client (it spawns CLIs), and the exact cron string must be deterministic.
// Ambiguous input returns every concrete candidate instead of guessing.
func (s *Server) handleParseSchedule(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	cands, err := schedule.ParseNatural(body.Text)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	now := time.Now()
	type candidateView struct {
		Cron         string `json:"cron"`
		Label        string `json:"label"`
		Name         string `json:"name,omitempty"`
		NextRunAt    string `json:"next_run_at"`
		NextRunLocal string `json:"next_run_local"`
	}
	out := make([]candidateView, 0, len(cands))
	for _, c := range cands {
		cron, err := schedule.ParseCron(c.Cron)
		if err != nil {
			continue
		}
		next, ok := cron.Next(now)
		if !ok {
			continue
		}
		out = append(out, candidateView{
			Cron: c.Cron, Label: c.Label, Name: c.Name,
			NextRunAt: next.Format(time.RFC3339), NextRunLocal: next.Format("2006-01-02 15:04"),
		})
	}
	if len(out) == 0 {
		writeErr(w, http.StatusBadRequest, "解析结果没有可执行时间")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"mode":       "rules",
		"candidates": out,
		"timezone":   localZoneName(),
	})
}

type harnessTaskView struct {
	ID         string `json:"id"`
	Cron       string `json:"cron"`
	Prompt     string `json:"prompt"`
	Recurring  bool   `json:"recurring"`
	CreatedAt  int64  `json:"created_at,omitempty"`
	Name       string `json:"name"`
	Valid      bool   `json:"valid"`
	Error      string `json:"error,omitempty"`
	ImportedID string `json:"imported_id,omitempty"`
}

// harnessCwds collects session workspaces so discovery can find the
// project-local .claude/scheduled_tasks.json.
func (s *Server) harnessCwds() []string {
	var out []string
	for _, sess := range s.st.List() {
		if sess.GroupID != "" || strings.TrimSpace(sess.Cwd) == "" {
			continue
		}
		out = append(out, sess.Cwd)
	}
	return out
}

// resolveHarnessPath accepts an explicit path (shown in the UI) or discovers
// one from env / cwd / session workspaces / ~/.claude. Explicit paths must be
// named scheduled_tasks.json so this endpoint cannot read arbitrary files.
func (s *Server) resolveHarnessPath(explicit string) (string, []string, error) {
	candidates := schedule.DiscoverHarnessPaths(s.harnessCwds())
	explicit = strings.TrimSpace(explicit)
	if explicit == "" {
		if len(candidates) == 0 {
			return "", candidates, nil
		}
		return candidates[0], candidates, nil
	}
	p := explicit
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", candidates, fmt.Errorf("展开家目录失败：%v", err)
		}
		p = filepath.Join(home, strings.TrimPrefix(p, "~/"))
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", candidates, fmt.Errorf("路径无效：%v", err)
	}
	if filepath.Base(abs) != schedule.HarnessFileName {
		return "", candidates, fmt.Errorf("只支持读取名为 %s 的文件", schedule.HarnessFileName)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", candidates, fmt.Errorf("读取不到 harness 文件：%v", err)
	}
	if info.IsDir() {
		return "", candidates, fmt.Errorf("路径是目录，不是 %s", schedule.HarnessFileName)
	}
	return abs, candidates, nil
}

func (s *Server) importedHarnessIDs() map[string]string {
	out := map[string]string{}
	for _, t := range s.scheduleStore().List() {
		if t.Source == "harness" && t.SourceID != "" {
			out[t.SourceID] = t.ID
		}
	}
	return out
}

// handleHarnessTasks lists Claude Code harness tasks (source=harness) without
// touching the file. Read/discovery problems are reported in the body (200) so
// the UI can show the path input and a hint instead of a hard error page.
func (s *Server) handleHarnessTasks(w http.ResponseWriter, r *http.Request) {
	path, candidates, err := s.resolveHarnessPath(r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	resp := map[string]any{
		"path":       path,
		"candidates": candidates,
		"tasks":      []harnessTaskView{},
		"error":      "",
	}
	if path == "" {
		resp["error"] = "未找到 .claude/scheduled_tasks.json；可在页面上填写路径，或设置 CC_WEBUI_HARNESS_TASKS"
		writeJSON(w, http.StatusOK, resp)
		return
	}
	tasks, err := schedule.ReadHarnessFile(path)
	if err != nil {
		resp["error"] = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	imported := s.importedHarnessIDs()
	views := make([]harnessTaskView, 0, len(tasks))
	for _, t := range tasks {
		v := harnessTaskView{
			ID: t.ID, Cron: t.Cron, Prompt: t.Prompt, Recurring: t.Recurring,
			Name: schedule.HarnessName(t), ImportedID: imported[t.ID],
		}
		if !t.CreatedAt.IsZero() {
			v.CreatedAt = t.CreatedAt.UnixMilli()
		}
		if _, err := schedule.ParseCron(t.Cron); err != nil {
			v.Error = err.Error()
		} else {
			v.Valid = true
		}
		views = append(views, v)
	}
	resp["tasks"] = views
	writeJSON(w, http.StatusOK, resp)
}

// handleImportHarnessTask copies one harness task into AgentDeck. The copy is
// disabled by default (both would otherwise fire), records source/source_id,
// and gets MaxRuns=1 when the harness task is one-shot. The source file is
// never modified.
func (s *Server) handleImportHarnessTask(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path          string `json:"path"`
		ID            string `json:"id"`
		TargetSession string `json:"target_session"`
		Enabled       *bool  `json:"enabled"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	body.ID = strings.TrimSpace(body.ID)
	if body.ID == "" {
		writeErr(w, http.StatusBadRequest, "缺少 harness 任务 id")
		return
	}
	path, _, err := s.resolveHarnessPath(body.Path)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if path == "" {
		writeErr(w, http.StatusBadRequest, "未找到 .claude/scheduled_tasks.json")
		return
	}
	tasks, err := schedule.ReadHarnessFile(path)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var src *schedule.HarnessTask
	for i := range tasks {
		if tasks[i].ID == body.ID {
			src = &tasks[i]
			break
		}
	}
	if src == nil {
		writeErr(w, http.StatusNotFound, "harness 任务不存在："+body.ID)
		return
	}
	cron, err := schedule.ParseCron(src.Cron)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "harness 任务的 cron 无效："+err.Error())
		return
	}
	if _, ok := cron.Next(time.Now()); !ok {
		writeErr(w, http.StatusBadRequest, "harness 任务的 cron 永不触发")
		return
	}
	if src.Prompt == "" {
		writeErr(w, http.StatusBadRequest, "harness 任务没有 prompt")
		return
	}
	for _, existing := range s.scheduleStore().List() {
		if existing.Source == "harness" && existing.SourceID == src.ID {
			writeErr(w, http.StatusConflict, "该 harness 任务已导入（"+existing.ID+"）")
			return
		}
	}
	target := strings.TrimSpace(body.TargetSession)
	if target == "" {
		target = "mainAgent"
	}
	sess := s.resolveAddress(target)
	if sess == nil {
		writeErr(w, http.StatusBadRequest, "目标会话不存在："+target)
		return
	}
	enabled := false
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	task := &schedule.Task{
		Name:          schedule.HarnessName(*src),
		Cron:          cron.Expr(),
		Prompt:        src.Prompt,
		TargetSession: sess.ID,
		Enabled:       enabled,
		Source:        "harness",
		SourceID:      src.ID,
	}
	if !src.Recurring {
		task.MaxRuns = 1
	}
	if enabled {
		if next, ok := cron.Next(time.Now()); ok {
			task.NextRunAt = &next
		}
	}
	out, err := s.scheduleStore().Create(task)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.scheduleView(out))
}
