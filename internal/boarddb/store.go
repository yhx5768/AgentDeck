// Package boarddb is the SQLite task store used by the AgentDeck board.
//
// tasks is the current projection; task_events is append-only history;
// task_links holds reports/sessions/cards; workers records agent/session
// bindings. The JSON store is only an import/export format now.
package boarddb

import (
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db   *sql.DB
	path string
	mu   sync.Mutex
}

var statusOrder = []string{"新建", "进行中", "待人工", "待验收", "已完成", "已取消"}

const schema = `
CREATE TABLE IF NOT EXISTS schema_meta (key TEXT PRIMARY KEY, value TEXT);
INSERT INTO schema_meta(key, value) VALUES ('version', '1')
  ON CONFLICT(key) DO UPDATE SET value = excluded.value;

CREATE TABLE IF NOT EXISTS tasks (
  id TEXT PRIMARY KEY,
  display_id TEXT NOT NULL DEFAULT '',
  title TEXT NOT NULL DEFAULT '',
  description TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT '新建',
  status_reason TEXT NOT NULL DEFAULT '',
  priority INTEGER NOT NULL DEFAULT 2,
  project TEXT NOT NULL DEFAULT '',
  labels TEXT NOT NULL DEFAULT '[]',
  owner TEXT NOT NULL DEFAULT '',
  assignee TEXT NOT NULL DEFAULT '',
  session_id TEXT NOT NULL DEFAULT '',
  cwd TEXT NOT NULL DEFAULT '',
  source TEXT NOT NULL DEFAULT 'local',
  external_ref TEXT NOT NULL DEFAULT '{}',
  progress TEXT NOT NULL DEFAULT '',
  progress_locked INTEGER NOT NULL DEFAULT 0,
  last_event TEXT NOT NULL DEFAULT '',
  report_file TEXT NOT NULL DEFAULT '',
  archived INTEGER NOT NULL DEFAULT 0,
  metadata TEXT NOT NULL DEFAULT '{}',
  created_at TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status, archived);
CREATE INDEX IF NOT EXISTS idx_tasks_project ON tasks(project, archived);
CREATE INDEX IF NOT EXISTS idx_tasks_assignee ON tasks(assignee, archived);
CREATE INDEX IF NOT EXISTS idx_tasks_updated ON tasks(updated_at DESC);

CREATE TABLE IF NOT EXISTS task_events (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  at TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL DEFAULT '',
  actor TEXT NOT NULL DEFAULT '',
  text TEXT NOT NULL DEFAULT '',
  data TEXT NOT NULL DEFAULT '{}',
  source TEXT NOT NULL DEFAULT 'local',
  idem_key TEXT,
  position INTEGER NOT NULL DEFAULT 0,
  UNIQUE(task_id, idem_key)
);
CREATE INDEX IF NOT EXISTS idx_events_task ON task_events(task_id, at DESC, position ASC, seq ASC);
CREATE INDEX IF NOT EXISTS idx_events_at ON task_events(at DESC);
CREATE INDEX IF NOT EXISTS idx_events_kind ON task_events(kind);

CREATE TABLE IF NOT EXISTS task_links (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  kind TEXT NOT NULL DEFAULT '',
  target TEXT NOT NULL DEFAULT '',
  label TEXT NOT NULL DEFAULT '',
  UNIQUE(task_id, kind, target)
);
CREATE INDEX IF NOT EXISTS idx_links_task ON task_links(task_id, kind);

CREATE TABLE IF NOT EXISTS workers (
  peer_name TEXT PRIMARY KEY,
  session_id TEXT NOT NULL DEFAULT '',
  cwd TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL DEFAULT '',
  metadata TEXT NOT NULL DEFAULT '{}'
);
`

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, path: path}, nil
}

func (s *Store) Close() error { return s.db.Close() }
func (s *Store) Path() string { return s.path }

func (s *Store) HasTasks() (bool, error) {
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM tasks").Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *Store) CountTasks() (int, error) {
	var n int
	err := s.db.QueryRow("SELECT COUNT(*) FROM tasks").Scan(&n)
	return n, err
}

func (s *Store) metaLocked(key string) string {
	var value string
	if err := s.db.QueryRow("SELECT value FROM schema_meta WHERE key=?", key).Scan(&value); err != nil {
		return ""
	}
	return value
}

func (s *Store) setMetaLocked(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO schema_meta(key,value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// MigrationResult reports what a one-time legacy import did.
type MigrationResult struct {
	Migrated bool   `json:"migrated"`
	Reason   string `json:"reason,omitempty"`
	Tasks    int    `json:"tasks"`
	Events   int    `json:"events"`
	Source   string `json:"source,omitempty"`
}

// MigrateOnce imports the legacy JSON store and routing table only when the
// database is still empty. A completed marker lives in schema_meta, so a
// restart never re-imports an old JSON snapshot over newer DB rows. If the DB
// already has tasks but no marker (for example one created by the earlier
// Python prototype), it is adopted as-is and marked complete.
func (s *Store) MigrateOnce(jsonPath, routingPath string) (MigrationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.metaLocked("migration_complete") == "1" {
		return MigrationResult{Reason: "already-migrated"}, nil
	}
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM tasks").Scan(&n); err != nil {
		return MigrationResult{}, err
	}
	if n > 0 {
		if err := s.setMetaLocked("migration_complete", "1"); err != nil {
			return MigrationResult{}, err
		}
		return MigrationResult{Reason: "database-not-empty", Tasks: n}, nil
	}
	tasks, events, err := s.migrateLocked(jsonPath, routingPath)
	if err != nil {
		return MigrationResult{}, err
	}
	if err := s.db.QueryRow("SELECT COUNT(*) FROM tasks").Scan(&tasks); err != nil {
		return MigrationResult{}, err
	}
	if err := s.setMetaLocked("migration_complete", "1"); err != nil {
		return MigrationResult{}, err
	}
	_ = s.setMetaLocked("migration_source", jsonPath)
	_ = s.setMetaLocked("migration_routing_table", routingPath)
	_ = s.setMetaLocked("migration_at", now())
	return MigrationResult{Migrated: true, Tasks: tasks, Events: events, Source: jsonPath}, nil
}

// Backup writes a consistent point-in-time copy using VACUUM INTO. The target
// must not exist; callers choose a timestamped filename.
func (s *Store) Backup(dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("VACUUM INTO ?", dest)
	return err
}

// Workers lists the peer/session bindings collected from routing updates and
// explicit bind calls.
func (s *Store) Workers() ([]map[string]any, error) {
	rows, err := s.db.Query(`SELECT peer_name,session_id,cwd,updated_at FROM workers ORDER BY updated_at DESC, peer_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var peer, session, cwd, updated string
		if err := rows.Scan(&peer, &session, &cwd, &updated); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"peer_name": peer, "session": session, "cwd": cwd, "updated_at": updated,
		})
	}
	return out, rows.Err()
}

func actorFor(kind string) string {
	return map[string]string{
		"用户消息": "user", "原始任务": "user", "主 Agent 回复": "mainAgent",
		"A2A 发": "mainAgent", "A2A 收": "worker", "最近回报": "worker", "建卡": "system",
	}[kind]
}

func eventKey(kind, at, text string) string {
	sum := sha1.Sum([]byte(kind + "|" + at + "|" + text))
	return hex.EncodeToString(sum[:])
}

func now() string { return time.Now().Format("2006-01-02 15:04:05") }

func isTimestamp(value string) bool {
	if len(value) < 10 {
		return false
	}
	for i := 0; i < 10; i++ {
		if i == 4 || i == 7 {
			if value[i] != '-' {
				return false
			}
		} else if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

type routingRow map[string]string

var routingColumns = []string{"task_id", "card", "status", "worker_peer", "session_id", "cwd",
	"last_event", "last_report", "report_file", "card_modified", "comment_id", "updated"}

var projectAliases = map[string]string{
	"鸿蒙cuid": "鸿蒙CUID", "鸿蒙cuid指标": "鸿蒙CUID", "鸿蒙编译机": "CI",
	"风险检查": "风险识别", "主agent": "AgentDeck", "主 agent": "AgentDeck", "q4规划": "风险识别",
}

func normalizeProject(value string) string {
	value = strings.TrimSpace(value)
	if canonical, ok := projectAliases[strings.ToLower(value)]; ok {
		return canonical
	}
	return value
}

func routingValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "—" || value == "-" {
		return ""
	}
	return value
}

func routingTaskID(row routingRow) string {
	if card := routingValue(row["card"]); card != "" {
		return card
	}
	return routingValue(row["task_id"])
}

func parseRoutingTable(path string) ([]routingRow, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rows []routingRow
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		parts := strings.Split(strings.Trim(line, "|"), "|")
		cells := make([]string, len(parts))
		for i, part := range parts {
			cells[i] = strings.TrimSpace(part)
		}
		if len(cells) < len(routingColumns) || cells[0] == "task_id" || cells[0] == "---" {
			continue
		}
		row := routingRow{}
		for i, column := range routingColumns {
			row[column] = cells[i]
		}
		// Ledger-first: a row may have no iCafe card; task_id is then the
		// stable identity. This is the gap 822 called out for the old syncers.
		if routingTaskID(row) != "" {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

func projectFromWorker(worker string) string {
	if worker == "" {
		return "未分类"
	}
	parts := strings.FieldsFunc(worker, func(r rune) bool { return r == '-' || r == '—' })
	if len(parts) == 0 {
		return "未分类"
	}
	project := normalizeProject(parts[0])
	if project == "" {
		return "未分类"
	}
	return project
}

// Migrate imports the legacy JSON store and the routing table. It is
// idempotent: tasks upsert, events dedupe by kind|at|text. Prefer MigrateOnce
// for server startup; this method stays available for explicit re-imports.
func (s *Store) Migrate(jsonPath, routingPath string) (int, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.migrateLocked(jsonPath, routingPath)
}

func (s *Store) migrateLocked(jsonPath, routingPath string) (int, int, error) {
	tasks, events := 0, 0
	raw, readErr := os.ReadFile(jsonPath)
	if readErr != nil && !os.IsNotExist(readErr) {
		return 0, 0, readErr
	}
	if readErr == nil {
		var store map[string]any
		if err := json.Unmarshal(raw, &store); err != nil {
			return 0, 0, err
		}
		list, _ := store["tasks"].([]any)
		for _, rawTask := range list {
			task, ok := rawTask.(map[string]any)
			if !ok {
				continue
			}
			if err := s.upsertTaskJSON(task); err != nil {
				return tasks, events, err
			}
			tasks++
			timeline, _ := task["timeline"].([]any)
			for position, rawEvent := range timeline {
				event, ok := rawEvent.(map[string]any)
				if !ok {
					continue
				}
				kind := jsonString(event, "kind")
				at := jsonString(event, "at")
				text := jsonString(event, "text")
				created, err := s.insertEventTx(jsonString(task, "id"), at, kind, actorFor(kind), text, "", "local", position)
				if err != nil {
					return tasks, events, err
				}
				if created {
					events++
				}
			}
			_ = s.upsertLinks(task)
		}
	}
	if routingPath != "" {
		rows, err := parseRoutingTable(routingPath)
		if err != nil {
			if !os.IsNotExist(err) {
				return tasks, events, err
			}
		} else {
			for _, row := range rows {
				if err := s.mergeRoutingRow(row); err != nil {
					return tasks, events, err
				}
				tasks++
			}
		}
	}
	return tasks, events, nil
}

func jsonString(task map[string]any, key string) string {
	value, ok := task[key]
	if !ok || value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func inputString(input map[string]any, key string) string { return jsonString(input, key) }

func inputTags(input map[string]any) string {
	raw, ok := input["tags"]
	if !ok {
		raw, ok = input["labels"]
	}
	if !ok {
		return "[]"
	}
	return jsonTags(map[string]any{"tags": raw})
}

func jsonBool(task map[string]any, key string) bool {
	value, _ := task[key].(bool)
	return value
}

func jsonTags(task map[string]any) string {
	raw := task["tags"]
	items, _ := raw.([]any)
	if items == nil {
		if list, ok := raw.([]string); ok {
			for _, item := range list {
				items = append(items, item)
			}
		}
	}
	out := []string{}
	for _, item := range items {
		text := strings.TrimSpace(fmt.Sprint(item))
		if text != "" {
			out = append(out, text)
		}
	}
	encoded, _ := json.Marshal(out)
	return string(encoded)
}

func (s *Store) upsertTaskJSON(task map[string]any) error {
	id := jsonString(task, "id")
	if id == "" {
		return nil
	}
	external := map[string]string{}
	if jsonString(task, "icafe_id") != "" {
		external = map[string]string{"system": "icafe", "id": jsonString(task, "icafe_id"),
			"url": jsonString(task, "url")}
	}
	externalJSON, _ := json.Marshal(external)
	created := jsonString(task, "updated_at")
	if created == "" {
		created = now()
	}
	_, err := s.db.Exec(`INSERT INTO tasks(id, display_id, title, description, status, status_reason,
		priority, project, labels, owner, assignee, session_id, cwd, source, external_ref,
		progress, progress_locked, last_event, report_file, archived, metadata, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
		display_id=excluded.display_id, title=excluded.title, description=excluded.description,
		status=excluded.status, status_reason=excluded.status_reason, priority=excluded.priority,
		project=excluded.project, labels=excluded.labels, owner=excluded.owner,
		assignee=excluded.assignee, session_id=excluded.session_id, cwd=excluded.cwd,
		source=excluded.source, external_ref=excluded.external_ref, progress=excluded.progress,
		progress_locked=excluded.progress_locked, last_event=excluded.last_event,
		report_file=excluded.report_file, archived=excluded.archived, metadata=excluded.metadata,
		updated_at=excluded.updated_at`,
		id, jsonString(task, "icafe_id"), jsonString(task, "title"), jsonString(task, "description"),
		defaultString(jsonString(task, "status"), "新建"), jsonString(task, "status_reason"),
		intFromAny(task["priority"], 2), normalizeProject(jsonString(task, "project")),
		jsonTags(task), jsonString(task, "owner"), jsonString(task, "worker"),
		jsonString(task, "session"), jsonString(task, "cwd"), defaultString(jsonString(task, "source"), "local"),
		string(externalJSON), jsonString(task, "progress"), boolInt(jsonBool(task, "progress_locked")),
		jsonString(task, "last_event"), jsonString(task, "report_file"), 0, "{}", created,
		defaultString(jsonString(task, "updated_at"), created))
	if err != nil {
		return err
	}
	return s.upsertWorkerLocked(jsonString(task, "worker"), jsonString(task, "session"),
		jsonString(task, "cwd"), defaultString(jsonString(task, "updated_at"), created))
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func intFromAny(value any, fallback int) int {
	switch typed := value.(type) {
	case int:
		return typed
	case float64:
		return int(typed)
	}
	return fallback
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (s *Store) upsertWorkerLocked(worker, session, cwd, at string) error {
	worker = strings.TrimSpace(worker)
	if worker == "" {
		return nil
	}
	_, err := s.db.Exec(`INSERT INTO workers(peer_name,session_id,cwd,updated_at,metadata)
		VALUES(?,?,?,?, '{}') ON CONFLICT(peer_name) DO UPDATE SET
		session_id=CASE WHEN excluded.session_id<>'' THEN excluded.session_id ELSE workers.session_id END,
		cwd=CASE WHEN excluded.cwd<>'' THEN excluded.cwd ELSE workers.cwd END,
		updated_at=CASE WHEN excluded.updated_at<>'' THEN excluded.updated_at ELSE workers.updated_at END`,
		worker, session, cwd, at)
	return err
}

func (s *Store) upsertLinks(task map[string]any) error {
	id := jsonString(task, "id")
	links := [][3]string{}
	if report := jsonString(task, "report_file"); report != "" {
		links = append(links, [3]string{"report", report, "报告"})
	}
	if session := jsonString(task, "session"); session != "" {
		links = append(links, [3]string{"session", session, "会话"})
	}
	if jsonString(task, "icafe_id") != "" && jsonString(task, "url") != "" {
		links = append(links, [3]string{"card", jsonString(task, "url"), jsonString(task, "icafe_id")})
	}
	for _, link := range links {
		_, _ = s.db.Exec("INSERT OR IGNORE INTO task_links(task_id,kind,target,label) VALUES(?,?,?,?)",
			id, link[0], link[1], link[2])
	}
	return nil
}

func (s *Store) mergeRoutingRow(row routingRow) error {
	id := routingTaskID(row)
	card := routingValue(row["card"])
	worker := routingValue(row["worker_peer"])
	status := routingValue(row["status"])
	project := projectFromWorker(worker)
	updated := strings.TrimSpace(row["updated"])
	current := ""
	_ = s.db.QueryRow("SELECT updated_at FROM tasks WHERE id=?", id).Scan(&current)
	var exists int
	_ = s.db.QueryRow("SELECT COUNT(*) FROM tasks WHERE id=?", id).Scan(&exists)
	url := ""
	externalJSON := "{}"
	if card != "" {
		url = "https://console.cloud.baidu-int.com/devops/icafe/issue/" + card + "/show"
		if encoded, err := json.Marshal(map[string]string{"system": "icafe", "id": card, "url": url}); err == nil {
			externalJSON = string(encoded)
		}
	}
	if exists == 0 {
		_, err := s.db.Exec(`INSERT INTO tasks(id,display_id,title,status,project,assignee,session_id,cwd,
			progress,last_event,report_file,external_ref,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, card, defaultString(routingValue(row["task_id"]), id), defaultString(status, "进行中"), project, worker,
			routingValue(row["session_id"]), routingValue(row["cwd"]), routingValue(row["last_report"]),
			routingValue(row["last_event"]), routingValue(row["report_file"]), externalJSON,
			defaultString(updated, now()), defaultString(updated, now()))
		if err != nil {
			return err
		}
	} else {
		_, err := s.db.Exec(`UPDATE tasks SET display_id=COALESCE(NULLIF(?,''),display_id),
			status=COALESCE(NULLIF(?,''),status),
			project=COALESCE(NULLIF(?,''),project), assignee=COALESCE(NULLIF(?,''),assignee),
			session_id=COALESCE(NULLIF(?,''),session_id), cwd=COALESCE(NULLIF(?,''),cwd),
			last_event=COALESCE(NULLIF(?,''),last_event),
			report_file=COALESCE(NULLIF(?,''),report_file),
			external_ref=CASE WHEN ?<>'{}' THEN ? ELSE external_ref END,
			progress=CASE WHEN progress_locked=1 THEN progress ELSE COALESCE(NULLIF(?,''),progress) END,
			updated_at=CASE WHEN ?<>'' AND (?='' OR ? > updated_at) THEN ? ELSE updated_at END
			WHERE id=?`, card, status, project, worker, routingValue(row["session_id"]), routingValue(row["cwd"]),
			routingValue(row["last_event"]), routingValue(row["report_file"]),
			externalJSON, externalJSON, routingValue(row["last_report"]),
			updated, current, updated, updated, id)
		if err != nil {
			return err
		}
	}
	_ = s.upsertLinks(map[string]any{
		"id": id, "icafe_id": card, "url": url,
		"session": routingValue(row["session_id"]), "report_file": routingValue(row["report_file"]),
	})
	if err := s.upsertWorkerLocked(worker, routingValue(row["session_id"]),
		routingValue(row["cwd"]), updated); err != nil {
		return err
	}
	report := routingValue(row["last_report"])
	if report != "" {
		_, _ = s.insertEventTx(id, updated, "最近回报", "worker", report, "", "local", 0)
	}
	return nil
}

func (s *Store) insertEventTx(taskID, at, kind, actor, text, data, source string, position int) (bool, error) {
	result, err := s.db.Exec(`INSERT OR IGNORE INTO task_events(task_id,at,kind,actor,text,data,source,idem_key,position)
		VALUES(?,?,?,?,?,?,?,?,?)`, taskID, at, kind, actor, text, data, source,
		eventKey(kind, at, text), position)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}

func (s *Store) AppendEvent(taskID, kind, text, at, source string) (bool, error) {
	kind = strings.TrimSpace(kind)
	text = strings.TrimSpace(text)
	if kind == "" || text == "" {
		return false, fmt.Errorf("kind/text required")
	}
	if at == "" {
		at = now()
	}
	if source == "" {
		source = "local"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var exists int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM tasks WHERE id=?", taskID).Scan(&exists); err != nil {
		return false, err
	}
	if exists == 0 {
		return false, fmt.Errorf("task not found")
	}
	created, err := s.insertEventTx(taskID, at, kind, actorFor(kind), text, "", source, 0)
	if err != nil {
		return false, err
	}
	if created {
		_, _ = s.db.Exec("UPDATE tasks SET last_event=?, updated_at=CASE WHEN ? > updated_at THEN ? ELSE updated_at END WHERE id=?",
			kind, at, at, taskID)
	}
	return created, nil
}

func (s *Store) BindWorker(taskID, worker, session, cwd string) error {
	worker = strings.TrimSpace(worker)
	if worker == "" {
		return fmt.Errorf("worker required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var exists int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM tasks WHERE id=?", taskID).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return fmt.Errorf("task not found")
	}
	at := now()
	if _, err := s.db.Exec(`UPDATE tasks SET assignee=?, session_id=COALESCE(NULLIF(?,''),session_id),
		cwd=COALESCE(NULLIF(?,''),cwd), updated_at=? WHERE id=?`,
		worker, session, cwd, at, taskID); err != nil {
		return err
	}
	if err := s.upsertWorkerLocked(worker, session, cwd, at); err != nil {
		return err
	}
	_, _ = s.insertEventTx(taskID, at, "派工", "mainAgent",
		fmt.Sprintf("worker=%s session=%s cwd=%s", worker, session, cwd), "", "local", 0)
	return nil
}

type taskRow struct {
	id, displayID, title, description, status, statusReason, project, labels, owner, assignee,
	session, cwd, source, externalRef, progress, lastEvent, reportFile, metadata, createdAt, updatedAt string
	priority, progressLocked, archived int
}

func scanTask(scanner interface{ Scan(...any) error }) (*taskRow, error) {
	row := &taskRow{}
	err := scanner.Scan(&row.id, &row.displayID, &row.title, &row.description, &row.status, &row.statusReason,
		&row.priority, &row.project, &row.labels, &row.owner, &row.assignee, &row.session, &row.cwd,
		&row.source, &row.externalRef, &row.progress, &row.progressLocked, &row.lastEvent, &row.reportFile,
		&row.archived, &row.metadata, &row.createdAt, &row.updatedAt)
	return row, err
}

const taskSelect = `SELECT id,display_id,title,description,status,status_reason,priority,project,labels,
	owner,assignee,session_id,cwd,source,external_ref,progress,progress_locked,last_event,report_file,
	archived,metadata,created_at,updated_at FROM tasks`

func (s *Store) getTaskLocked(id string) (*taskRow, error) {
	return scanTask(s.db.QueryRow(taskSelect+" WHERE id=?", id))
}

func (r *taskRow) toMap(events []map[string]any) map[string]any {
	var tags []string
	_ = json.Unmarshal([]byte(r.labels), &tags)
	if tags == nil {
		tags = []string{}
	}
	var external map[string]string
	_ = json.Unmarshal([]byte(r.externalRef), &external)
	task := map[string]any{
		"id": r.id, "icafe_id": r.displayID, "source": r.source, "title": r.title,
		"full_title": r.title, "status": r.status, "project": r.project, "owner": r.owner,
		"worker": r.assignee, "session": r.session, "cwd": r.cwd, "updated_at": r.updatedAt,
		"progress": r.progress, "last_event": r.lastEvent, "report_file": r.reportFile,
		"tags": tags, "timeline": events, "url": external["url"],
	}
	if r.statusReason != "" {
		task["status_reason"] = r.statusReason
	}
	if r.progressLocked != 0 {
		task["progress_locked"] = true
	}
	return task
}

func (s *Store) taskEvents(taskID string) ([]map[string]any, error) {
	rows, err := s.db.Query(`SELECT at,kind,text,data FROM task_events WHERE task_id=?
		ORDER BY at DESC, position ASC, seq ASC`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []map[string]any{}
	for rows.Next() {
		var at, kind, text, data string
		if err := rows.Scan(&at, &kind, &text, &data); err != nil {
			return nil, err
		}
		event := map[string]any{"at": at, "kind": kind, "text": text}
		if data != "" && data != "{}" {
			var payload any
			if json.Unmarshal([]byte(data), &payload) == nil {
				event["data"] = payload
			}
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *Store) Snapshot() (map[string]any, error) {
	rows, err := s.db.Query(taskSelect + " ORDER BY updated_at DESC")
	if err != nil {
		return nil, err
	}
	// Drain and close before querying events: the pool is capped at one
	// connection, so nested queries while rows is open would block forever.
	var taskRows []*taskRow
	for rows.Next() {
		row, err := scanTask(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		taskRows = append(taskRows, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	tasks := make([]any, 0, len(taskRows))
	counts := map[string]int{}
	projectSet := map[string]bool{}
	for _, row := range taskRows {
		events, err := s.taskEvents(row.id)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, row.toMap(events))
		counts[row.status]++
		if row.project != "" {
			projectSet[row.project] = true
		}
	}
	for _, status := range statusOrder {
		if _, ok := counts[status]; !ok {
			counts[status] = 0
		}
	}
	projects := make([]string, 0, len(projectSet))
	for project := range projectSet {
		projects = append(projects, project)
	}
	sort.Strings(projects)
	return map[string]any{
		"generated_at": now(), "source": "sqlite", "status_order": statusOrder,
		"counts": counts, "projects": projects, "tasks": tasks,
	}, nil
}

func (s *Store) GetTask(id string) (map[string]any, error) {
	row, err := s.getTaskLocked(id)
	if err != nil {
		return nil, err
	}
	events, err := s.taskEvents(id)
	if err != nil {
		return nil, err
	}
	return row.toMap(events), nil
}

func (s *Store) CreateTask(input map[string]any) (map[string]any, error) {
	title := inputString(input, "title")
	if title == "" {
		return nil, fmt.Errorf("title required")
	}
	id := inputString(input, "id")
	if id == "" {
		id = "T-" + time.Now().Format("20060102-150405.000000")
	}
	displayID := inputString(input, "display_id")
	if displayID == "" {
		displayID = inputString(input, "icafe_id")
	}
	status := defaultString(inputString(input, "status"), "新建")
	project := normalizeProject(inputString(input, "project"))
	assignee := inputString(input, "worker")
	session := inputString(input, "session")
	cwd := inputString(input, "cwd")
	owner := inputString(input, "owner")
	description := inputString(input, "description")
	source := defaultString(inputString(input, "source"), "local")
	progress := inputString(input, "progress")
	lastEvent := inputString(input, "last_event")
	reportFile := inputString(input, "report_file")
	tags := inputTags(input)
	priority := intFromAny(input["priority"], 2)
	external := map[string]string{}
	if displayID != "" {
		url := inputString(input, "url")
		if url == "" {
			url = "https://console.cloud.baidu-int.com/devops/icafe/issue/" + displayID + "/show"
		}
		external = map[string]string{"system": "icafe", "id": displayID, "url": url}
	}
	externalJSON, _ := json.Marshal(external)
	at := now()
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO tasks(id,display_id,title,description,status,project,labels,
		owner,assignee,session_id,cwd,source,external_ref,progress,last_event,report_file,
		priority,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, displayID, title, description, status, project, tags, owner, assignee, session, cwd,
		source, string(externalJSON), progress, lastEvent, reportFile, priority, at, at)
	if err != nil {
		return nil, err
	}
	_, _ = s.insertEventTx(id, at, "建卡", "system", "任务创建", "", "local", 0)
	_ = s.upsertLinks(map[string]any{
		"id": id, "icafe_id": displayID, "url": external["url"],
		"session": session, "report_file": reportFile,
	})
	if err := s.upsertWorkerLocked(assignee, session, cwd, at); err != nil {
		return nil, err
	}
	return s.GetTask(id)
}

func patchString(patch map[string]any, key, current string) string {
	if value, ok := patch[key]; ok {
		if value == nil {
			return ""
		}
		return strings.TrimSpace(fmt.Sprint(value))
	}
	return current
}

func (s *Store) UpdateTask(id string, patch map[string]any) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, err := s.getTaskLocked(id)
	if err != nil {
		return nil, err
	}
	at := now()
	status := patchString(patch, "status", row.status)
	if status == "" {
		status = row.status
	}
	title := patchString(patch, "title", row.title)
	if title == "" {
		title = row.title
	}
	description := patchString(patch, "description", row.description)
	progress := patchString(patch, "progress", row.progress)
	statusReason := patchString(patch, "status_reason", row.statusReason)
	project := normalizeProject(patchString(patch, "project", row.project))
	assignee := patchString(patch, "worker", row.assignee)
	sessionID := patchString(patch, "session", row.session)
	cwd := patchString(patch, "cwd", row.cwd)
	owner := patchString(patch, "owner", row.owner)
	lastEvent := patchString(patch, "last_event", row.lastEvent)
	reportFile := patchString(patch, "report_file", row.reportFile)
	labels := row.labels
	if _, ok := patch["tags"]; ok {
		labels = inputTags(patch)
	} else if _, ok := patch["labels"]; ok {
		labels = inputTags(patch)
	}
	priority := row.priority
	if value, ok := patch["priority"]; ok {
		priority = intFromAny(value, row.priority)
	}
	progressLocked := row.progressLocked
	if value, ok := patch["progress_locked"]; ok {
		if typed, ok := value.(bool); ok {
			progressLocked = boolInt(typed)
		}
	}
	archived := row.archived
	if value, ok := patch["archived"]; ok {
		if typed, ok := value.(bool); ok {
			archived = boolInt(typed)
		}
	}
	_, err = s.db.Exec(`UPDATE tasks SET title=?,description=?,status=?,status_reason=?,project=?,
		labels=?,owner=?,assignee=?,session_id=?,cwd=?,progress=?,progress_locked=?,last_event=?,
		report_file=?,priority=?,archived=?,updated_at=? WHERE id=?`,
		title, description, status, statusReason, project, labels, owner, assignee, sessionID, cwd,
		progress, progressLocked, lastEvent, reportFile, priority, archived, at, id)
	if err != nil {
		return nil, err
	}
	if status != row.status {
		_, _ = s.insertEventTx(id, at, "状态", "mainAgent", row.status+" → "+status, "", "local", 0)
	}
	if assignee != "" && (assignee != row.assignee || sessionID != row.session || cwd != row.cwd) {
		if err := s.upsertWorkerLocked(assignee, sessionID, cwd, at); err != nil {
			return nil, err
		}
	}
	return s.GetTask(id)
}

func (s *Store) ExportJSON(path string) error {
	snapshot, err := s.Snapshot()
	if err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, encoded, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
