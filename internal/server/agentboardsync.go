package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// The board was originally synced by an external Python watcher. This file
// moves that loop into cc-webui: it watches routing-table.md and rewrites the
// configured JSON stores in place. The JSON format is unchanged, so the
// existing file watcher + SSE keep working.

var boardStatusOrder = []string{"新建", "进行中", "待人工", "待验收", "已完成", "已取消"}

var boardRoutingColumns = []string{
	"task_id", "card", "status", "worker_peer", "session_id", "cwd",
	"last_event", "last_report", "report_file", "card_modified",
	"comment_id", "updated",
}

var boardProjectAliases = map[string]string{
	"鸿蒙cuid":   "鸿蒙CUID",
	"鸿蒙cuid指标": "鸿蒙CUID",
	"鸿蒙编译机":    "CI",
	"风险检查":     "风险识别",
	"主agent":   "AgentDeck",
	"主 agent":  "AgentDeck",
	"q4规划":     "风险识别",
}

// watchAgentBoardSync runs the routing-table watcher when configured. It reads
// the live config every tick so a hot reload can enable/disable or repoint it.
func (s *Server) watchAgentBoardSync() {
	var lastPath string
	var lastFingerprint string
	for {
		cfg := s.cfg()
		routing := strings.TrimSpace(cfg.AgentBoardRoutingTable)
		if routing == "" {
			lastPath = ""
			lastFingerprint = ""
			time.Sleep(time.Second)
			continue
		}
		if routing != lastPath {
			lastPath = routing
			lastFingerprint = ""
		}
		fingerprint := boardFileFingerprint(routing)
		if fingerprint != "" && fingerprint != lastFingerprint {
			time.Sleep(600 * time.Millisecond) // debounce a burst of writes
			fingerprint = boardFileFingerprint(routing)
			stores := boardSyncStores(cfg.AgentBoardSyncStores, s.agentBoardPath())
			for _, store := range stores {
				if err := syncBoardStore(store, routing); err != nil {
					fmt.Fprintf(os.Stderr, "agent-board sync %s: %v\n", store, err)
				}
			}
			lastFingerprint = fingerprint
		}
		time.Sleep(time.Second)
	}
}

func boardSyncStores(configured []string, primary string) []string {
	if len(configured) == 0 {
		return []string{primary}
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(configured))
	for _, path := range append([]string{primary}, configured...) {
		path = strings.TrimSpace(path)
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, path)
	}
	return out
}

func boardFileFingerprint(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d:%d", info.ModTime().UnixNano(), info.Size())
}

type boardRoutingRow map[string]string

func parseBoardRoutingTable(path string) ([]boardRoutingRow, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rows []boardRoutingRow
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		line = strings.Trim(line, "|")
		parts := strings.Split(line, "|")
		cells := make([]string, len(parts))
		for i, part := range parts {
			cells[i] = strings.TrimSpace(part)
		}
		if len(cells) < len(boardRoutingColumns) {
			continue
		}
		if cells[0] == "task_id" || cells[0] == "---" || onlySeparator(cells[0]) {
			continue
		}
		row := boardRoutingRow{}
		for i, name := range boardRoutingColumns {
			row[name] = cells[i]
		}
		if strings.HasPrefix(row["card"], "YHX-TODO-") {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

func onlySeparator(value string) bool {
	if value == "" {
		return false
	}
	for _, ch := range value {
		if ch != '-' && ch != ':' {
			return false
		}
	}
	return true
}

func boardValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "—" || value == "-" {
		return ""
	}
	return value
}

func normalizeBoardProject(value string) string {
	value = strings.TrimSpace(value)
	if canonical, ok := boardProjectAliases[strings.ToLower(value)]; ok {
		return canonical
	}
	return value
}

func normalizeBoardTags(value any) []string {
	var items []string
	switch typed := value.(type) {
	case string:
		for _, part := range strings.FieldsFunc(typed, func(r rune) bool { return r == ',' || r == '，' || r == '、' }) {
			items = append(items, part)
		}
	case []string:
		items = append(items, typed...)
	case []any:
		for _, item := range typed {
			items = append(items, fmt.Sprint(item))
		}
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item != "" && !seen[item] {
			seen[item] = true
			out = append(out, item)
		}
	}
	return out
}

func isBoardTimestamp(value string) bool {
	if len(value) < 10 {
		return false
	}
	for i := 0; i < 10; i++ {
		switch i {
		case 4, 7:
			if value[i] != '-' {
				return false
			}
		default:
			if value[i] < '0' || value[i] > '9' {
				return false
			}
		}
	}
	return true
}

func boardNow() string { return time.Now().Format("2006-01-02 15:04:05") }

func boardTaskFromRow(row boardRoutingRow) map[string]any {
	card := row["card"]
	worker := strings.TrimSpace(row["worker_peer"])
	project := "未分类"
	if worker != "" {
		parts := strings.FieldsFunc(worker, func(r rune) bool { return r == '-' || r == '—' })
		if len(parts) > 0 {
			project = strings.TrimSpace(parts[0])
		}
	}
	project = normalizeBoardProject(project)
	if project == "" {
		project = "未分类"
	}
	report := boardValue(row["last_report"])
	updated := strings.TrimSpace(row["updated"])
	if updated == "" {
		updated = boardNow()
	}
	timeline := []any{}
	if report != "" {
		timeline = append(timeline, map[string]any{"at": updated, "kind": "最近回报", "text": report})
	}
	task := map[string]any{
		"id":          card,
		"icafe_id":    card,
		"source":      "local",
		"title":       row["task_id"],
		"full_title":  row["task_id"],
		"status":      row["status"],
		"project":     project,
		"owner":       "",
		"worker":      boardValue(worker),
		"session":     boardValue(row["session_id"]),
		"cwd":         boardValue(row["cwd"]),
		"updated_at":  updated,
		"progress":    report,
		"last_event":  boardValue(row["last_event"]),
		"report_file": boardValue(row["report_file"]),
		"tags":        []string{},
		"timeline":    timeline,
	}
	if task["status"] == "" {
		task["status"] = "进行中"
	}
	if task["title"] == "" {
		task["title"] = card
		task["full_title"] = card
	}
	task["url"] = "https://console.cloud.baidu-int.com/devops/icafe/issue/" + card + "/show"
	return task
}

func boardString(task map[string]any, key string) string {
	if value, ok := task[key].(string); ok {
		return value
	}
	return ""
}

func boardTimeline(task map[string]any) []any {
	if value, ok := task["timeline"].([]any); ok {
		return value
	}
	if value, ok := task["timeline"].([]map[string]any); ok {
		out := make([]any, len(value))
		for i := range value {
			out[i] = value[i]
		}
		return out
	}
	return nil
}

func boardEventAt(event any) string {
	if item, ok := event.(map[string]any); ok {
		if at, ok := item["at"].(string); ok {
			return at
		}
	}
	return ""
}

func mergeBoardRoutingRow(task map[string]any, row boardRoutingRow) map[string]any {
	if status := boardValue(row["status"]); status != "" {
		task["status"] = status
	}
	for source, target := range map[string]string{"worker_peer": "worker", "session_id": "session", "cwd": "cwd"} {
		if value := boardValue(row[source]); value != "" {
			task[target] = value
		}
	}
	if value := boardValue(row["last_event"]); value != "" {
		task["last_event"] = value
	}
	report := boardValue(row["last_report"])
	if report != "" {
		task["progress"] = report
		timeline := boardTimeline(task)
		found := false
		for _, event := range timeline {
			if item, ok := event.(map[string]any); ok {
				if item["kind"] == "最近回报" && item["text"] == report {
					found = true
					break
				}
			}
		}
		if !found {
			at := strings.TrimSpace(row["updated"])
			if at == "" {
				at = boardString(task, "updated_at")
			}
			if at == "" {
				at = boardNow()
			}
			timeline = append(timeline, map[string]any{"at": at, "kind": "最近回报", "text": report})
			sort.SliceStable(timeline, func(i, j int) bool {
				return boardEventAt(timeline[i]) > boardEventAt(timeline[j])
			})
			if len(timeline) > 20 {
				timeline = timeline[:20]
			}
		}
		task["timeline"] = timeline
	}
	current := boardString(task, "updated_at")
	incoming := strings.TrimSpace(row["updated"])
	if isBoardTimestamp(incoming) && (!isBoardTimestamp(current) || incoming > current) {
		task["updated_at"] = incoming
	}
	task["project"] = normalizeBoardProject(boardString(task, "project"))
	if task["project"] == "" {
		task["project"] = "未分类"
	}
	task["tags"] = normalizeBoardTags(task["tags"])
	task["url"] = "https://console.cloud.baidu-int.com/devops/icafe/issue/" + boardString(task, "icafe_id") + "/show"
	return task
}

func loadBoardStore(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]any{"tasks": []any{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var store map[string]any
	if err := json.Unmarshal(raw, &store); err != nil {
		return nil, err
	}
	return store, nil
}

func syncBoardStore(storePath, routingPath string) error {
	rows, err := parseBoardRoutingTable(routingPath)
	if err != nil {
		return err
	}
	unlock, err := lockBoardStore(storePath, 5*time.Second)
	if err != nil {
		return err
	}
	defer unlock()

	store, err := loadBoardStore(storePath)
	if err != nil {
		return err
	}
	existing := map[string]map[string]any{}
	if tasks, ok := store["tasks"].([]any); ok {
		for _, raw := range tasks {
			if task, ok := raw.(map[string]any); ok {
				existing[fmt.Sprint(task["id"])] = task
			}
		}
	}
	tasks := make([]any, 0, len(rows))
	for _, row := range rows {
		card := row["card"]
		task := existing[card]
		if task == nil {
			task = boardTaskFromRow(row)
		} else {
			task = mergeBoardRoutingRow(task, row)
		}
		task["project"] = normalizeBoardProject(boardString(task, "project"))
		if task["project"] == "" {
			task["project"] = "未分类"
		}
		task["tags"] = normalizeBoardTags(task["tags"])
		tasks = append(tasks, task)
	}
	store["tasks"] = tasks
	store["generated_at"] = boardNow()
	store["source"] = "local"
	store["status_order"] = boardStatusOrder
	store["ledger_source"] = routingPath
	counts := map[string]int{}
	for _, status := range boardStatusOrder {
		counts[status] = 0
	}
	projects := map[string]bool{}
	for _, raw := range tasks {
		task := raw.(map[string]any)
		status := fmt.Sprint(task["status"])
		counts[status]++
		if project := strings.TrimSpace(fmt.Sprint(task["project"])); project != "" {
			projects[project] = true
		}
	}
	store["counts"] = counts
	projectList := make([]string, 0, len(projects))
	for project := range projects {
		projectList = append(projectList, project)
	}
	sort.Strings(projectList)
	store["projects"] = projectList

	encoded, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(storePath), 0o755); err != nil {
		return err
	}
	tmp := storePath + ".tmp"
	if err := os.WriteFile(tmp, encoded, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, storePath)
}

func lockBoardStore(path string, timeout time.Duration) (func(), error) {
	lockPath := path + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			file.Close()
			return nil, fmt.Errorf("等待 store 锁超时: %s", lockPath)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}
