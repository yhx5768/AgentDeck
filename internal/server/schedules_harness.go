package server

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
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

type harnessFileView struct {
	Path      string            `json:"path"`
	Workspace string            `json:"workspace"`
	Container string            `json:"container"` // workspace | global
	Error     string            `json:"error,omitempty"`
	Empty     bool              `json:"empty"`
	Tasks     []harnessTaskView `json:"tasks"`
}

type harnessGroupView struct {
	Workspace string            `json:"workspace"`
	Container string            `json:"container"`
	TaskCount int               `json:"task_count"`
	Files     []harnessFileView `json:"files"`
}

type harnessListResp struct {
	Groups     []harnessGroupView `json:"groups"`
	Files      []harnessFileView  `json:"files"`
	Candidates []string           `json:"candidates"`
	Path       string             `json:"path"`
	Error      string             `json:"error"`
	Tasks      []harnessTaskView  `json:"tasks"`
}

// harnessCwds lists workspaces to scan: the main agent's cwd first, then every
// non-shadow session cwd, deduplicated. Each yields <cwd>/.claude/scheduled_tasks.json.
func (s *Server) harnessCwds() []string {
	var mainCwd string
	var out []string
	seen := map[string]bool{}
	add := func(cwd string) {
		cwd = strings.TrimSpace(cwd)
		if cwd == "" || seen[cwd] {
			return
		}
		seen[cwd] = true
		out = append(out, cwd)
	}
	for _, sess := range s.st.List() {
		if sess.GroupID != "" || strings.TrimSpace(sess.Cwd) == "" {
			continue
		}
		if sess.MainAgent && !sess.Archived && mainCwd == "" {
			mainCwd = sess.Cwd
		}
		add(sess.Cwd)
	}
	if mainCwd != "" {
		// mainAgent first: its workspace is the one most users care about.
		for i, c := range out {
			if c == mainCwd {
				copy(out[1:i+1], out[0:i])
				out[0] = mainCwd
				break
			}
		}
	}
	return out
}

func (s *Server) harnessStoredPaths() []string {
	var out []string
	for _, t := range s.scheduleStore().List() {
		if t.Source == "harness" && strings.TrimSpace(t.SourcePath) != "" {
			out = append(out, t.SourcePath)
		}
	}
	return out
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~/"))
		}
	}
	return p
}

// validateHarnessPath turns a user-supplied path into an absolute existing file.
// Only scheduled_tasks.json is accepted so this endpoint cannot read arbitrary
// files.
func validateHarnessPath(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("路径为空")
	}
	abs, err := filepath.Abs(expandHome(raw))
	if err != nil {
		return "", fmt.Errorf("路径无效：%v", err)
	}
	if filepath.Base(abs) != schedule.HarnessFileName {
		return "", fmt.Errorf("只支持读取名为 %s 的文件", schedule.HarnessFileName)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("读取不到 harness 文件：%v", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("路径是目录，不是 %s", schedule.HarnessFileName)
	}
	return abs, nil
}

func harnessContainer(workspace string) string {
	if home, err := os.UserHomeDir(); err == nil && filepath.Clean(workspace) == filepath.Clean(home) {
		return "global"
	}
	return "workspace"
}

type importedHarness struct {
	byPath map[string]string // path \x00 id -> schedule id
	legacy map[string]string // id -> schedule id (v2 imports without source_path)
}

func (s *Server) importedHarness() importedHarness {
	out := importedHarness{byPath: map[string]string{}, legacy: map[string]string{}}
	for _, t := range s.scheduleStore().List() {
		if t.Source != "harness" || t.SourceID == "" {
			continue
		}
		if t.SourcePath == "" {
			out.legacy[t.SourceID] = t.ID
			continue
		}
		out.byPath[t.SourcePath+"\x00"+t.SourceID] = t.ID
	}
	return out
}

func (s *Server) readHarnessFileView(path string, imported importedHarness) harnessFileView {
	workspace := schedule.WorkspaceRoot(path)
	f := harnessFileView{
		Path: path, Workspace: workspace, Container: harnessContainer(workspace),
		Tasks: []harnessTaskView{},
	}
	tasks, err := schedule.ReadHarnessFile(path)
	if err != nil {
		switch {
		case errors.Is(err, schedule.ErrHarnessNotExist):
			f.Error = "文件不存在"
		case errors.Is(err, schedule.ErrHarnessInvalidJSON):
			f.Error = "JSON 解析失败：" + err.Error()
		default:
			f.Error = err.Error()
		}
		return f
	}
	if len(tasks) == 0 {
		f.Empty = true
		return f
	}
	for _, t := range tasks {
		v := harnessTaskView{
			ID: t.ID, Cron: t.Cron, Prompt: t.Prompt, Recurring: t.Recurring,
			Name: schedule.HarnessName(t),
			// Legacy (v2) imports have no source_path; matching by id is the
			// best we can do and ids are random 8-char strings.
			ImportedID: imported.byPath[path+"\x00"+t.ID],
		}
		if v.ImportedID == "" {
			v.ImportedID = imported.legacy[t.ID]
		}
		if !t.CreatedAt.IsZero() {
			v.CreatedAt = t.CreatedAt.UnixMilli()
		}
		if _, err := schedule.ParseCron(t.Cron); err != nil {
			v.Error = err.Error()
		} else {
			v.Valid = true
		}
		f.Tasks = append(f.Tasks, v)
	}
	return f
}

// handleHarnessTasks returns every discoverable harness file grouped by
// workspace. An explicit ?path= restricts the response to that one file.
// Missing/malformed files are reported per file (200) so one bad project does
// not hide the rest.
func (s *Server) handleHarnessTasks(w http.ResponseWriter, r *http.Request) {
	explicit := strings.TrimSpace(r.URL.Query().Get("path"))
	var groups []schedule.WorkspacePaths
	var candidates []string
	cwds := s.harnessCwds()

	if explicit != "" {
		abs, err := validateHarnessPath(explicit)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		groups = []schedule.WorkspacePaths{{Workspace: schedule.WorkspaceRoot(abs), Paths: []string{abs}}}
		candidates = []string{abs}
	} else {
		groups = schedule.DiscoverHarnessGroups(cwds, s.harnessStoredPaths())
		// Stored import paths that no longer exist should still appear (with a
		// "文件不存在" note) instead of silently vanishing.
		discovered := map[string]bool{}
		for _, g := range groups {
			for _, p := range g.Paths {
				discovered[p] = true
			}
		}
		for _, p := range s.harnessStoredPaths() {
			if discovered[p] {
				continue
			}
			ws := schedule.WorkspaceRoot(p)
			merged := false
			for i := range groups {
				if groups[i].Workspace == ws {
					groups[i].Paths = append(groups[i].Paths, p)
					merged = true
					break
				}
			}
			if !merged {
				groups = append(groups, schedule.WorkspacePaths{Workspace: ws, Paths: []string{p}})
			}
			discovered[p] = true
		}
		for _, g := range groups {
			candidates = append(candidates, g.Paths...)
		}
	}

	imported := s.importedHarness()
	resp := harnessListResp{
		Groups: []harnessGroupView{}, Files: []harnessFileView{}, Candidates: candidates,
		Tasks: []harnessTaskView{},
	}
	sort.Slice(groups, func(i, j int) bool {
		a, b := groups[i].Workspace, groups[j].Workspace
		mainCwd := ""
		if len(cwds) > 0 {
			mainCwd = cwds[0]
		}
		if mainCwd != "" && (a == mainCwd) != (b == mainCwd) {
			return a == mainCwd
		}
		ag, bg := harnessContainer(a) == "global", harnessContainer(b) == "global"
		if ag != bg {
			return !ag
		}
		return a < b
	})
	for _, g := range groups {
		sort.Strings(g.Paths)
		gv := harnessGroupView{
			Workspace: g.Workspace, Container: harnessContainer(g.Workspace),
			Files: []harnessFileView{},
		}
		for _, p := range g.Paths {
			fv := s.readHarnessFileView(p, imported)
			gv.Files = append(gv.Files, fv)
			gv.TaskCount += len(fv.Tasks)
			resp.Files = append(resp.Files, fv)
			resp.Tasks = append(resp.Tasks, fv.Tasks...)
			if resp.Path == "" {
				resp.Path = p
			}
		}
		resp.Groups = append(resp.Groups, gv)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleImportHarnessTask copies one harness task into AgentDeck. The copy is
// disabled by default (both would otherwise fire), records source/path/id, and
// gets MaxRuns=1 when the harness task is one-shot. The source file is never
// modified. Duplicate detection is per (path, id).
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
	var path string
	if strings.TrimSpace(body.Path) != "" {
		abs, err := validateHarnessPath(body.Path)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		path = abs
	} else {
		paths := schedule.DiscoverHarnessPaths(s.harnessCwds())
		if len(paths) == 0 {
			writeErr(w, http.StatusBadRequest, "未发现可导入的 harness 文件，请显式提供 path")
			return
		}
		path = paths[0]
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
		if existing.Source != "harness" || existing.SourceID != src.ID {
			continue
		}
		if existing.SourcePath == path {
			writeErr(w, http.StatusConflict, "该 harness 任务已导入（"+existing.ID+"）")
			return
		}
		if existing.SourcePath == "" {
			// Migrate a v2 import that predates source_path so future
			// duplicate checks are exact.
			_, _ = s.scheduleStore().Update(existing.ID, func(t *schedule.Task) {
				t.SourcePath = path
			})
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
		SourcePath:    path,
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
