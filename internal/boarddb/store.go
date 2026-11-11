// Package boarddb is the SQLite task store used by the AgentDeck board.
//
// tasks is the current projection; task_events is append-only history;
// task_links holds report/session links; workers records agent/session
// bindings. The board is DB-native: there is no file import.
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
	"strconv"
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
INSERT INTO schema_meta(key, value) VALUES ('version', '4')
  ON CONFLICT(key) DO UPDATE SET value = excluded.value;

CREATE TABLE IF NOT EXISTS tasks (
  id TEXT PRIMARY KEY,
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

-- task_artifacts is the multi-artifact model behind the drawer's "产物与证据"
-- panel. A task may publish many artifacts (HTML pages, markdown, files, or
-- external URLs). The file bytes themselves live under <data_dir>/artifacts/;
-- storage_path is relative to that root and is only resolved by the server.
-- report_file (tasks) stays for compatibility and is exposed as a synthetic
-- legacy artifact when non-empty.
CREATE TABLE IF NOT EXISTS task_artifacts (
  id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  title TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL DEFAULT 'file',
  mime TEXT NOT NULL DEFAULT '',
  filename TEXT NOT NULL DEFAULT '',
  size INTEGER NOT NULL DEFAULT 0,
  storage_path TEXT NOT NULL DEFAULT '',
  url TEXT NOT NULL DEFAULT '',
  source_session TEXT NOT NULL DEFAULT '',
  round INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_artifacts_task ON task_artifacts(task_id, created_at);
CREATE INDEX IF NOT EXISTS idx_artifacts_created ON task_artifacts(created_at DESC);

-- artifact_shares turns one artifact into a public, unauthenticated link.
-- Only the SHA-256 hash of the URL token is stored; the token itself is
-- derived from the share id plus a server-side secret (see the server's
-- artifact-share.key), so management can rebuild the URL after a restart
-- without ever persisting the capability token. Empty expires_at = permanent;
-- non-empty revoked_at = cancelled immediately.
CREATE TABLE IF NOT EXISTS artifact_shares (
  id TEXT PRIMARY KEY,
  artifact_id TEXT NOT NULL REFERENCES task_artifacts(id) ON DELETE CASCADE,
  token_hash TEXT NOT NULL,
  expires_at TEXT NOT NULL DEFAULT '',
  revoked_at TEXT NOT NULL DEFAULT '',
  created_by TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_shares_token ON artifact_shares(token_hash);
CREATE INDEX IF NOT EXISTS idx_shares_artifact ON artifact_shares(artifact_id, created_at DESC);

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
	// CREATE TABLE IF NOT EXISTS does not add columns to an existing database,
	// so bring older boards up to the current artifact shape here.
	if err := ensureArtifactRoundColumn(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, path: path}, nil
}

// ensureArtifactRoundColumn adds task_artifacts.round to boards created before
// per-round grouping existed. The default 0 means "no explicit round"; such
// artifacts are clustered by timestamp when read (see annotateArtifactRounds).
func ensureArtifactRoundColumn(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(task_artifacts)`)
	if err != nil {
		return err
	}
	hasRound := false
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notNull int
			dflt    any
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == "round" {
			hasRound = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if hasRound {
		return nil
	}
	_, err = db.Exec(`ALTER TABLE task_artifacts ADD COLUMN round INTEGER NOT NULL DEFAULT 0`)
	return err
}

func (s *Store) Close() error { return s.db.Close() }
func (s *Store) Path() string { return s.path }

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
		// Runtime-generated worker notifications (see internal/server/incidents.go).
		"worker 异常": "system", "worker 卡住": "system", "worker 提问": "system",
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

func int64FromAny(value any, fallback int64) int64 {
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int64:
		return typed
	case float64:
		return int64(typed)
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
	for _, link := range links {
		_, _ = s.db.Exec("INSERT OR IGNORE INTO task_links(task_id,kind,target,label) VALUES(?,?,?,?)",
			id, link[0], link[1], link[2])
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
	id, title, description, status, statusReason, project, labels, owner, assignee,
	session, cwd, source, progress, lastEvent, reportFile, metadata, createdAt, updatedAt string
	priority, progressLocked, archived int
}

func scanTask(scanner interface{ Scan(...any) error }) (*taskRow, error) {
	row := &taskRow{}
	err := scanner.Scan(&row.id, &row.title, &row.description, &row.status, &row.statusReason,
		&row.priority, &row.project, &row.labels, &row.owner, &row.assignee, &row.session, &row.cwd,
		&row.source, &row.progress, &row.progressLocked, &row.lastEvent, &row.reportFile,
		&row.archived, &row.metadata, &row.createdAt, &row.updatedAt)
	return row, err
}

const taskSelect = `SELECT id,title,description,status,status_reason,priority,project,labels,
	owner,assignee,session_id,cwd,source,progress,progress_locked,last_event,report_file,
	archived,metadata,created_at,updated_at FROM tasks`

func (s *Store) getTaskLocked(id string) (*taskRow, error) {
	return scanTask(s.db.QueryRow(taskSelect+" WHERE id=?", id))
}

func (r *taskRow) toMap(events []map[string]any, artifacts []map[string]any) map[string]any {
	var tags []string
	_ = json.Unmarshal([]byte(r.labels), &tags)
	if tags == nil {
		tags = []string{}
	}
	if legacy := legacyReportArtifact(r); legacy != nil {
		artifacts = append(artifacts, legacy)
	}
	annotateArtifactRounds(artifacts)
	if artifacts == nil {
		artifacts = []map[string]any{}
	}
	task := map[string]any{
		"id": r.id, "source": r.source, "title": r.title,
		"full_title": r.title, "status": r.status, "project": r.project, "owner": r.owner,
		"worker": r.assignee, "session": r.session, "cwd": r.cwd, "updated_at": r.updatedAt,
		"progress": r.progress, "last_event": r.lastEvent, "report_file": r.reportFile,
		"tags": tags, "timeline": events, "artifacts": artifacts,
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

// TaskArtifact is one row of task_artifacts.
type TaskArtifact struct {
	ID, TaskID, Title, Kind, MIME, Filename, StoragePath, URL, SourceSession, CreatedAt string
	Size                                                                                int64
	Round                                                                               int
}

func (a *TaskArtifact) toMap() map[string]any {
	return map[string]any{
		"id": a.ID, "task_id": a.TaskID, "title": a.Title, "kind": a.Kind,
		"mime": a.MIME, "filename": a.Filename, "size": a.Size,
		"storage_path": a.StoragePath, "url": a.URL,
		"source_session": a.SourceSession, "round": a.Round, "created_at": a.CreatedAt,
	}
}

const artifactSelect = `SELECT id,task_id,title,kind,mime,filename,size,storage_path,url,
	source_session,round,created_at FROM task_artifacts`

func scanArtifact(scanner interface{ Scan(...any) error }) (*TaskArtifact, error) {
	artifact := &TaskArtifact{}
	err := scanner.Scan(&artifact.ID, &artifact.TaskID, &artifact.Title, &artifact.Kind,
		&artifact.MIME, &artifact.Filename, &artifact.Size, &artifact.StoragePath,
		&artifact.URL, &artifact.SourceSession, &artifact.Round, &artifact.CreatedAt)
	return artifact, err
}

// GetArtifact returns one artifact record (no task join, no URL decoration —
// the server owns public URL minting).
func (s *Store) GetArtifact(id string) (*TaskArtifact, error) {
	return scanArtifact(s.db.QueryRow(artifactSelect+" WHERE id=?", id))
}

func (s *Store) taskArtifacts(taskID string) ([]map[string]any, error) {
	rows, err := s.db.Query(artifactSelect+" WHERE task_id=? ORDER BY rowid ASC", taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		artifact, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, artifact.toMap())
	}
	return out, rows.Err()
}

// allArtifacts groups every artifact by task id (one query for Snapshot).
func (s *Store) allArtifacts() (map[string][]map[string]any, error) {
	rows, err := s.db.Query(artifactSelect + " ORDER BY rowid ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]map[string]any{}
	for rows.Next() {
		artifact, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		out[artifact.TaskID] = append(out[artifact.TaskID], artifact.toMap())
	}
	return out, rows.Err()
}

// legacyReportArtifact synthesizes an artifact for a task's legacy report_file
// path so old cards still light up the artifacts panel. It is display-only:
// nothing is copied, no URL is minted, and the path is not validated (it may
// live on a worker's machine).
func legacyReportArtifact(r *taskRow) map[string]any {
	path := strings.TrimSpace(r.reportFile)
	if path == "" {
		return nil
	}
	name := strings.ReplaceAll(path, "\\", "/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	if name == "" {
		name = path
	}
	kind := "file"
	switch strings.ToLower(filepath.Ext(name)) {
	case ".html", ".htm":
		kind = "html"
	case ".md", ".markdown":
		kind = "markdown"
	}
	return map[string]any{
		"id": "legacy-report", "task_id": r.id, "title": "验证/进展报告（旧）",
		"kind": kind, "mime": "", "filename": name, "size": int64(0),
		"storage_path": path, "url": "", "source_session": "",
		"round": 0, "created_at": r.updatedAt, "legacy": true,
	}
}

// artifactClusterGap is the maximum gap between adjacent publish timestamps
// that still counts as one batch when inferring groups for artifacts that were
// published before explicit rounds existed. It is deliberately generous: a
// round's files (HTML report, transcript, ZIP, screenshots) are normally
// published within seconds or minutes of each other, while separate rounds are
// usually separated by more than half an hour.
const artifactClusterGap = 30 * time.Minute

// annotateArtifactRounds decorates the artifacts of one task with grouping
// hints for the drawer:
//
//   - round > 0: published with an explicit round. round_source = "explicit".
//   - round == 0: infer a batch by clustering adjacent created_at timestamps.
//     round_source = "inferred" and round_batch counts clusters from the
//     newest (1 = most recent legacy batch).
//   - unknown/unparsable timestamps (and the synthetic legacy report_file
//     artifact) stay ungrouped: round_source = "" and round_batch = 0, rendered
//     as the final "历史产物" group.
//
// Inferred batches are intentionally NOT labelled "第 N 轮": the old data has
// no round information, so the UI only says "较早产物" plus a time range and
// never fabricates a precise round number.
func annotateArtifactRounds(artifacts []map[string]any) {
	type entry struct {
		artifact map[string]any
		at       time.Time
	}
	var timed []entry
	for _, artifact := range artifacts {
		if legacy, _ := artifact["legacy"].(bool); legacy {
			artifact["round_source"] = ""
			artifact["round_batch"] = 0
			continue
		}
		if intFromAny(artifact["round"], 0) > 0 {
			artifact["round_source"] = "explicit"
			artifact["round_batch"] = 0
			continue
		}
		artifact["round_source"] = ""
		artifact["round_batch"] = 0
		raw, _ := artifact["created_at"].(string)
		at, err := time.ParseInLocation("2006-01-02 15:04:05", strings.TrimSpace(raw), time.Local)
		if err == nil {
			timed = append(timed, entry{artifact: artifact, at: at})
		}
	}
	if len(timed) == 0 {
		return
	}
	sort.SliceStable(timed, func(i, j int) bool { return timed[i].at.Before(timed[j].at) })
	var batches [][]entry
	for _, item := range timed {
		n := len(batches)
		if n > 0 {
			last := batches[n-1][len(batches[n-1])-1]
			if item.at.Sub(last.at) <= artifactClusterGap {
				batches[n-1] = append(batches[n-1], item)
				continue
			}
		}
		batches = append(batches, []entry{item})
	}
	// Newest batch is 1, so the frontend can sort ascending after explicit
	// rounds without re-parsing timestamps.
	for i := len(batches) - 1; i >= 0; i-- {
		batch := len(batches) - i
		for _, item := range batches[i] {
			item.artifact["round_source"] = "inferred"
			item.artifact["round_batch"] = batch
		}
	}
}

// artifactRoundInput normalizes the optional round field accepted by
// CreateArtifact. Models and HTTP clients sometimes send numbers as strings, so
// both shapes are accepted; anything else (or a negative value) is rejected.
func artifactRoundInput(value any) (int, error) {
	if value == nil {
		return 0, nil
	}
	var round int
	switch typed := value.(type) {
	case int:
		round = typed
	case int64:
		round = int(typed)
	case float64:
		if typed != float64(int(typed)) {
			return 0, fmt.Errorf("round must be an integer")
		}
		round = int(typed)
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return 0, fmt.Errorf("round must be an integer")
		}
		round = int(parsed)
	case string:
		text := strings.TrimSpace(typed)
		if text == "" {
			return 0, nil
		}
		parsed, err := strconv.Atoi(text)
		if err != nil {
			return 0, fmt.Errorf("round must be an integer")
		}
		round = parsed
	default:
		return 0, fmt.Errorf("round must be an integer")
	}
	if round < 0 {
		return 0, fmt.Errorf("round must be non-negative")
	}
	return round, nil
}

var artifactKinds = map[string]bool{"html": true, "markdown": true, "file": true, "url": true}

func validArtifactID(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

// CreateArtifact records one artifact for a task. The caller (server) has
// already written the bytes to stable storage when storage_path is set; for
// kind=url the external URL is stored instead. Duplicate publishes are
// append-only: every publish gets a fresh id so a re-upload can never clobber
// an earlier artifact.
func (s *Store) CreateArtifact(input map[string]any) (map[string]any, error) {
	taskID := inputString(input, "task_id")
	if taskID == "" {
		return nil, fmt.Errorf("task_id required")
	}
	id := inputString(input, "id")
	if id == "" {
		id = "A-" + time.Now().Format("20060102-150405.000000")
	}
	if !validArtifactID(id) {
		return nil, fmt.Errorf("invalid artifact id")
	}
	kind := strings.ToLower(defaultString(inputString(input, "kind"), "file"))
	if !artifactKinds[kind] {
		return nil, fmt.Errorf("unsupported artifact kind: %s", kind)
	}
	title := inputString(input, "title")
	filename := inputString(input, "filename")
	if title == "" {
		title = filename
	}
	storagePath := inputString(input, "storage_path")
	externalURL := inputString(input, "url")
	if kind == "url" && externalURL == "" {
		return nil, fmt.Errorf("url required for kind=url")
	}
	if kind != "url" && storagePath == "" {
		return nil, fmt.Errorf("storage_path required for hosted artifacts")
	}
	at := now()
	round, err := artifactRoundInput(input["round"])
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var exists int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM tasks WHERE id=?", taskID).Scan(&exists); err != nil {
		return nil, err
	}
	if exists == 0 {
		return nil, fmt.Errorf("task not found")
	}
	_, err = s.db.Exec(`INSERT INTO task_artifacts(id,task_id,title,kind,mime,filename,size,
		storage_path,url,source_session,round,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, taskID, title, kind, inputString(input, "mime"), filename,
		int64FromAny(input["size"], 0), storagePath, externalURL,
		inputString(input, "source_session"), round, at)
	if err != nil {
		return nil, err
	}
	_, _ = s.insertEventTx(taskID, at, "产物", "system",
		fmt.Sprintf("新增产物：%s（%s）", title, filename), "", "local", 0)
	_, _ = s.db.Exec(`UPDATE tasks SET last_event=?, updated_at=CASE WHEN ? > updated_at THEN ? ELSE updated_at END WHERE id=?`,
		"产物", at, at, taskID)
	artifact, err := s.GetArtifact(id)
	if err != nil {
		return nil, err
	}
	return artifact.toMap(), nil
}

// ArtifactShare is one row of artifact_shares. TokenHash is the SHA-256 of the
// public URL token; the plaintext capability token is never persisted. The
// Artifact*/Task* fields are filled by ListShares for the management view.
type ArtifactShare struct {
	ID, ArtifactID, TokenHash, ExpiresAt, RevokedAt, CreatedBy, CreatedAt string
	ArtifactTitle, ArtifactKind, ArtifactFilename, TaskID, TaskTitle      string
	TaskProject                                                           string
}

func (s *ArtifactShare) toMap() map[string]any {
	out := map[string]any{
		"id": s.ID, "artifact_id": s.ArtifactID,
		"expires_at": s.ExpiresAt, "revoked_at": s.RevokedAt,
		"created_by": s.CreatedBy, "created_at": s.CreatedAt,
	}
	if s.ArtifactTitle != "" || s.ArtifactKind != "" || s.ArtifactFilename != "" {
		out["artifact_title"] = s.ArtifactTitle
		out["artifact_kind"] = s.ArtifactKind
		out["artifact_filename"] = s.ArtifactFilename
	}
	if s.TaskID != "" || s.TaskTitle != "" {
		out["task_id"] = s.TaskID
		out["task_title"] = s.TaskTitle
	}
	if s.TaskProject != "" {
		out["project"] = s.TaskProject
	}
	return out
}

const shareSelect = `SELECT id,artifact_id,token_hash,expires_at,revoked_at,created_by,created_at
	FROM artifact_shares`

func scanShare(scanner interface{ Scan(...any) error }) (*ArtifactShare, error) {
	share := &ArtifactShare{}
	err := scanner.Scan(&share.ID, &share.ArtifactID, &share.TokenHash, &share.ExpiresAt,
		&share.RevokedAt, &share.CreatedBy, &share.CreatedAt)
	return share, err
}

// CreateShare records one public link for an artifact. The caller (server)
// supplies the share id and the hash of the derived token; plaintext tokens
// never reach the database. The id must be supplied because the public token is
// derived from it (see the server's artifactShareToken).
func (s *Store) CreateShare(input map[string]any) (map[string]any, error) {
	artifactID := inputString(input, "artifact_id")
	if artifactID == "" {
		return nil, fmt.Errorf("artifact_id required")
	}
	tokenHash := inputString(input, "token_hash")
	if tokenHash == "" {
		return nil, fmt.Errorf("token_hash required")
	}
	id := inputString(input, "id")
	if id == "" {
		return nil, fmt.Errorf("share id required")
	}
	if !validArtifactID(id) {
		return nil, fmt.Errorf("invalid share id")
	}
	at := now()
	s.mu.Lock()
	defer s.mu.Unlock()
	var exists int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM task_artifacts WHERE id=?", artifactID).Scan(&exists); err != nil {
		return nil, err
	}
	if exists == 0 {
		return nil, fmt.Errorf("artifact not found")
	}
	_, err := s.db.Exec(`INSERT INTO artifact_shares(id,artifact_id,token_hash,expires_at,
		revoked_at,created_by,created_at) VALUES(?,?,?,?,'',?,?)`,
		id, artifactID, tokenHash, inputString(input, "expires_at"),
		inputString(input, "created_by"), at)
	if err != nil {
		return nil, err
	}
	share, err := scanShare(s.db.QueryRow(shareSelect+" WHERE id=?", id))
	if err != nil {
		return nil, err
	}
	return share.toMap(), nil
}

func (s *Store) GetShare(id string) (*ArtifactShare, error) {
	return scanShare(s.db.QueryRow(shareSelect+" WHERE id=?", id))
}

// GetShareByTokenHash resolves a public URL token. Revocation/expiry are
// checked by the server so it can distinguish 404 (unknown) from 410 (known
// but no longer valid).
func (s *Store) GetShareByTokenHash(tokenHash string) (*ArtifactShare, error) {
	return scanShare(s.db.QueryRow(shareSelect+" WHERE token_hash=?", tokenHash))
}

// ListShares returns every share with the artifact/task labels the management
// panel needs, newest first. Revoked/expired rows stay visible so the user can
// see what happened to a link.
func (s *Store) ListShares() ([]map[string]any, error) {
	rows, err := s.db.Query(`SELECT s.id,s.artifact_id,s.token_hash,s.expires_at,s.revoked_at,
		s.created_by,s.created_at,a.title,a.kind,a.filename,a.task_id,
		COALESCE(t.title,''),COALESCE(t.project,'')
		FROM artifact_shares s
		JOIN task_artifacts a ON a.id=s.artifact_id
		LEFT JOIN tasks t ON t.id=a.task_id
		ORDER BY s.created_at DESC, s.rowid DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		share := &ArtifactShare{}
		if err := rows.Scan(&share.ID, &share.ArtifactID, &share.TokenHash, &share.ExpiresAt,
			&share.RevokedAt, &share.CreatedBy, &share.CreatedAt, &share.ArtifactTitle,
			&share.ArtifactKind, &share.ArtifactFilename, &share.TaskID, &share.TaskTitle,
			&share.TaskProject); err != nil {
			return nil, err
		}
		out = append(out, share.toMap())
	}
	return out, rows.Err()
}

// RevokeShare cancels a link immediately. Revoking an already-revoked share is
// a no-op that still returns the record.
func (s *Store) RevokeShare(id string) (map[string]any, error) {
	at := now()
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`UPDATE artifact_shares SET revoked_at=? WHERE id=? AND revoked_at=''`, at, id)
	if err != nil {
		return nil, err
	}
	affected, _ := res.RowsAffected()
	share, err := scanShare(s.db.QueryRow(shareSelect+" WHERE id=?", id))
	if err != nil {
		return nil, err
	}
	if affected == 0 && share.RevokedAt == "" {
		return nil, fmt.Errorf("share not found")
	}
	return share.toMap(), nil
}

func (s *Store) Snapshot() (map[string]any, error) {
	// Archived tasks are hidden from the board (and its counts) — the read-only
	// UI has no "show archived" toggle, so surfacing them only inflates the
	// columns and status counts.
	rows, err := s.db.Query(taskSelect + " WHERE archived=0 ORDER BY updated_at DESC")
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
	artifactsByTask, err := s.allArtifacts()
	if err != nil {
		return nil, err
	}
	for _, row := range taskRows {
		events, err := s.taskEvents(row.id)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, row.toMap(events, artifactsByTask[row.id]))
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
	artifacts, err := s.taskArtifacts(id)
	if err != nil {
		return nil, err
	}
	return row.toMap(events, artifacts), nil
}

// TaskForSession returns the board task whose bound worker session is
// sessionID, or nil when none is bound. It backs the runtime's worker
// notifications: only a session the main agent dispatched (i.e. bound to a
// card) reports back to the main agent. Lock-free like GetTask/Snapshot (the
// pool is capped at one connection).
func (s *Store) TaskForSession(sessionID string) (map[string]any, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, nil
	}
	row, err := scanTask(s.db.QueryRow(taskSelect+" WHERE session_id=? ORDER BY updated_at DESC LIMIT 1", sessionID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	events, err := s.taskEvents(row.id)
	if err != nil {
		return nil, err
	}
	artifacts, err := s.taskArtifacts(row.id)
	if err != nil {
		return nil, err
	}
	return row.toMap(events, artifacts), nil
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
	at := now()
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO tasks(id,title,description,status,project,labels,
		owner,assignee,session_id,cwd,source,progress,last_event,report_file,
		priority,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, title, description, status, project, tags, owner, assignee, session, cwd,
		source, progress, lastEvent, reportFile, priority, at, at)
	if err != nil {
		return nil, err
	}
	_, _ = s.insertEventTx(id, at, "建卡", "system", "任务创建", "", "local", 0)
	_ = s.upsertLinks(map[string]any{
		"id":      id,
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
