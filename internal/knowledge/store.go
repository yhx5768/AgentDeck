package knowledge

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Store is the SQLite knowledge store. The pool is capped at one connection
// (the same pattern as boarddb); knowledge traffic is low-QPS and this keeps
// FTS writes and reads from fighting over SQLITE_BUSY.
type Store struct {
	db   *sql.DB
	path string
	mu   sync.Mutex
}

// Entry is one knowledge card. GeneratedBy is "human", "ai", "hybrid", or
// "system" for deterministic collection.
type Entry struct {
	ID                string     `json:"id"`
	Title             string     `json:"title"`
	Summary           string     `json:"summary"`
	Body              string     `json:"body,omitempty"`
	Project           string     `json:"project"`
	Owner             string     `json:"owner"`
	Status            string     `json:"status"`
	Confidence        int        `json:"confidence"`
	Sensitivity       string     `json:"sensitivity"`
	SourceKind        string     `json:"source_kind"`
	SourceTaskID      string     `json:"source_task_id,omitempty"`
	SourceSessionID   string     `json:"source_session_id,omitempty"`
	SourceArtifactIDs []string   `json:"source_artifact_ids"`
	SourcePaths       []string   `json:"source_paths"`
	GeneratedBy       string     `json:"generated_by"`
	AIFields          []string   `json:"ai_fields"`
	Version           int        `json:"version"`
	SupersedesID      string     `json:"supersedes_id,omitempty"`
	SupersededBy      string     `json:"superseded_by,omitempty"`
	DedupeKey         string     `json:"-"`
	ContentHash       string     `json:"-"`
	LastVerifiedAt    string     `json:"last_verified_at,omitempty"`
	ValidUntil        string     `json:"valid_until,omitempty"`
	Reviewer          string     `json:"reviewer,omitempty"`
	ReviewedAt        string     `json:"reviewed_at,omitempty"`
	ReviewReason      string     `json:"review_reason,omitempty"`
	DeletedAt         string     `json:"deleted_at,omitempty"`
	CreatedAt         string     `json:"created_at"`
	UpdatedAt         string     `json:"updated_at"`
	Tags              []string   `json:"tags"`
	Evidence          []Evidence `json:"evidence,omitempty"`
	Relations         []Relation `json:"relations,omitempty"`
	Snippet           string     `json:"snippet,omitempty"`
}

// Evidence is one provenance pointer. Quote is already redacted and capped.
type Evidence struct {
	Kind       string `json:"kind"`
	Ref        string `json:"ref"`
	Locator    string `json:"locator,omitempty"`
	Quote      string `json:"quote,omitempty"`
	CapturedAt string `json:"captured_at,omitempty"`
}

// Relation links two entries. The relation is stored on the from side.
type Relation struct {
	Kind    string `json:"kind"`
	FromID  string `json:"from_id"`
	ToID    string `json:"to_id"`
	ToTitle string `json:"to_title,omitempty"`
}

// Summary is the compact shape the board drawer embeds.
type Summary struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	Project   string `json:"project"`
	UpdatedAt string `json:"updated_at"`
}

// CreateInput is one knowledge_add / collect / UI save payload.
type CreateInput struct {
	Title             string
	Summary           string
	Body              string
	Project           string
	Owner             string
	Status            string
	Confidence        int
	Sensitivity       string
	SourceKind        string
	SourceTaskID      string
	SourceSessionID   string
	SourceArtifactIDs []string
	SourcePaths       []string
	GeneratedBy       string
	AIFields          []string
	Tags              []string
	Evidence          []Evidence
	Actor             string
}

// UpdateInput is a partial update. Nil pointers mean "leave unchanged".
type UpdateInput struct {
	Title        *string
	Summary      *string
	Body         *string
	Project      *string
	Tags         *[]string
	Status       *string
	Confidence   *int
	Sensitivity  *string
	ReviewReason *string
	Reviewer     *string
	Actor        string
	Action       string
}

// SearchOptions is the shared filter set for the UI and MCP search.
type SearchOptions struct {
	Query            string
	Project          string
	Status           string // exact status, or "all"
	Tag              string
	SourceTaskID     string
	Limit            int
	Offset           int
	IncludeDrafts    bool
	IncludeStale     bool
	IncludeSensitive bool
}

// SearchResult is one page plus the total matching count.
type SearchResult struct {
	Entries []*Entry
	Total   int
}

const schema = `
CREATE TABLE IF NOT EXISTS schema_meta (key TEXT PRIMARY KEY, value TEXT);
INSERT INTO schema_meta(key,value) VALUES ('version','1')
  ON CONFLICT(key) DO UPDATE SET value=excluded.value;

CREATE TABLE IF NOT EXISTS knowledge_entries (
  id TEXT PRIMARY KEY,
  title TEXT NOT NULL DEFAULT '',
  summary TEXT NOT NULL DEFAULT '',
  body TEXT NOT NULL DEFAULT '',
  project TEXT NOT NULL DEFAULT '',
  owner TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'draft',
  confidence INTEGER NOT NULL DEFAULT 50,
  sensitivity TEXT NOT NULL DEFAULT 'internal',
  source_kind TEXT NOT NULL DEFAULT 'manual',
  source_task_id TEXT NOT NULL DEFAULT '',
  source_session_id TEXT NOT NULL DEFAULT '',
  source_artifact_ids TEXT NOT NULL DEFAULT '[]',
  source_paths TEXT NOT NULL DEFAULT '[]',
  dedupe_key TEXT NOT NULL DEFAULT '',
  content_hash TEXT NOT NULL DEFAULT '',
  generated_by TEXT NOT NULL DEFAULT 'human',
  ai_fields TEXT NOT NULL DEFAULT '[]',
  version INTEGER NOT NULL DEFAULT 1,
  supersedes_id TEXT NOT NULL DEFAULT '',
  last_verified_at TEXT NOT NULL DEFAULT '',
  valid_until TEXT NOT NULL DEFAULT '',
  reviewer TEXT NOT NULL DEFAULT '',
  reviewed_at TEXT NOT NULL DEFAULT '',
  review_reason TEXT NOT NULL DEFAULT '',
  deleted_at TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_knowledge_dedupe_active
  ON knowledge_entries(dedupe_key)
  WHERE deleted_at='' AND status IN ('draft','approved','stale');
CREATE INDEX IF NOT EXISTS idx_knowledge_status
  ON knowledge_entries(status, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_knowledge_project
  ON knowledge_entries(project, status, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_knowledge_task
  ON knowledge_entries(source_task_id, status);
CREATE INDEX IF NOT EXISTS idx_knowledge_hash
  ON knowledge_entries(content_hash);

CREATE TABLE IF NOT EXISTS knowledge_tags (
  entry_id TEXT NOT NULL REFERENCES knowledge_entries(id) ON DELETE CASCADE,
  tag TEXT NOT NULL,
  PRIMARY KEY(entry_id, tag)
);
CREATE INDEX IF NOT EXISTS idx_knowledge_tags_tag ON knowledge_tags(tag, entry_id);

CREATE TABLE IF NOT EXISTS knowledge_evidence (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  entry_id TEXT NOT NULL REFERENCES knowledge_entries(id) ON DELETE CASCADE,
  kind TEXT NOT NULL,
  ref TEXT NOT NULL,
  locator TEXT NOT NULL DEFAULT '',
  quote TEXT NOT NULL DEFAULT '',
  captured_at TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_evidence_unique
  ON knowledge_evidence(entry_id, kind, ref, locator);
CREATE INDEX IF NOT EXISTS idx_evidence_entry ON knowledge_evidence(entry_id);

CREATE TABLE IF NOT EXISTS knowledge_relations (
  from_id TEXT NOT NULL,
  to_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  created_by TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(from_id, to_id, kind)
);
CREATE INDEX IF NOT EXISTS idx_relations_to ON knowledge_relations(to_id, kind);

CREATE TABLE IF NOT EXISTS knowledge_events (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  entry_id TEXT NOT NULL DEFAULT '',
  at TEXT NOT NULL DEFAULT '',
  actor TEXT NOT NULL DEFAULT '',
  action TEXT NOT NULL,
  detail TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS idx_kevents_entry ON knowledge_events(entry_id, at DESC);

CREATE VIRTUAL TABLE IF NOT EXISTS knowledge_fts USING fts5(
  entry_id UNINDEXED,
  title_norm,
  summary_norm,
  body_norm,
  tags_norm,
  tokenize='unicode61'
);
`

// Open creates/opens <path> and applies the schema. The database and its
// directory are private (0600/0700): knowledge bodies can contain internal
// project detail even after redaction.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
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
	_ = os.Chmod(path, 0o600)
	return &Store{db: db, path: path}, nil
}

func (s *Store) Close() error { return s.db.Close() }
func (s *Store) Path() string { return s.path }

func now() string { return time.Now().Format("2006-01-02 15:04:05") }

func newID() string {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("K-%d", time.Now().UnixNano())
	}
	return "K-" + time.Now().Format("20060102-150405") + "-" + hex.EncodeToString(buf)
}

func normalizeText(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(s)), " "))
}

func dedupeKey(project, title, body string) string {
	sum := sha256.Sum256([]byte(normalizeText(project) + "|" + normalizeText(title) + "|" + normalizeText(body)))
	return hex.EncodeToString(sum[:])
}

func contentHash(body string) string {
	sum := sha256.Sum256([]byte(normalizeText(body)))
	return hex.EncodeToString(sum[:])
}

func jsonStrings(values []string) string {
	if values == nil {
		values = []string{}
	}
	raw, _ := json.Marshal(values)
	return string(raw)
}

func decodeStrings(raw string) []string {
	var out []string
	if strings.TrimSpace(raw) == "" || json.Unmarshal([]byte(raw), &out) != nil || out == nil {
		return []string{}
	}
	return out
}

func clipRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "…"
}

func sanitizeTags(tags []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" || seen[tag] {
			continue
		}
		seen[tag] = true
		out = append(out, tag)
		if len(out) >= 20 {
			break
		}
	}
	return out
}

var entrySelect = `SELECT id,title,summary,body,project,owner,status,confidence,sensitivity,
source_kind,source_task_id,source_session_id,source_artifact_ids,source_paths,
dedupe_key,content_hash,generated_by,ai_fields,version,supersedes_id,
last_verified_at,valid_until,reviewer,reviewed_at,review_reason,deleted_at,
created_at,updated_at FROM knowledge_entries`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanEntry(scanner rowScanner) (*Entry, error) {
	e := &Entry{}
	var artifactIDs, sourcePaths, aiFields string
	err := scanner.Scan(&e.ID, &e.Title, &e.Summary, &e.Body, &e.Project, &e.Owner,
		&e.Status, &e.Confidence, &e.Sensitivity, &e.SourceKind, &e.SourceTaskID,
		&e.SourceSessionID, &artifactIDs, &sourcePaths, &e.DedupeKey, &e.ContentHash,
		&e.GeneratedBy, &aiFields, &e.Version, &e.SupersedesID,
		&e.LastVerifiedAt, &e.ValidUntil, &e.Reviewer, &e.ReviewedAt, &e.ReviewReason,
		&e.DeletedAt, &e.CreatedAt, &e.UpdatedAt)
	if err != nil {
		return nil, err
	}
	e.SourceArtifactIDs = decodeStrings(artifactIDs)
	e.SourcePaths = decodeStrings(sourcePaths)
	e.AIFields = decodeStrings(aiFields)
	e.Tags = []string{}
	e.Evidence = []Evidence{}
	e.Relations = []Relation{}
	return e, nil
}

// getEntry loads one entry. full=false skips evidence/relations (list view).
func (s *Store) getEntry(id string, full bool) (*Entry, error) {
	e, err := scanEntry(s.db.QueryRow(entrySelect+" WHERE id=?", id))
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query("SELECT tag FROM knowledge_tags WHERE entry_id=? ORDER BY tag", id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var tag string
		if err := rows.Scan(&tag); err != nil {
			rows.Close()
			return nil, err
		}
		e.Tags = append(e.Tags, tag)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if !full {
		return e, nil
	}
	evRows, err := s.db.Query(`SELECT kind,ref,locator,quote,captured_at
		FROM knowledge_evidence WHERE entry_id=? ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	for evRows.Next() {
		var ev Evidence
		if err := evRows.Scan(&ev.Kind, &ev.Ref, &ev.Locator, &ev.Quote, &ev.CapturedAt); err != nil {
			evRows.Close()
			return nil, err
		}
		e.Evidence = append(e.Evidence, ev)
	}
	if err := evRows.Err(); err != nil {
		evRows.Close()
		return nil, err
	}
	evRows.Close()

	relRows, err := s.db.Query(`SELECT r.kind,r.from_id,r.to_id,COALESCE(e.title,'')
		FROM knowledge_relations r
		LEFT JOIN knowledge_entries e ON e.id = CASE WHEN r.from_id=? THEN r.to_id ELSE r.from_id END
		WHERE r.from_id=? OR r.to_id=?
		ORDER BY r.kind, r.from_id, r.to_id`, id, id, id)
	if err != nil {
		return nil, err
	}
	for relRows.Next() {
		var rel Relation
		if err := relRows.Scan(&rel.Kind, &rel.FromID, &rel.ToID, &rel.ToTitle); err != nil {
			relRows.Close()
			return nil, err
		}
		incoming := rel.ToID == id
		if incoming {
			rel.FromID, rel.ToID = id, rel.FromID
		}
		if rel.Kind == "supersedes" && incoming {
			e.SupersededBy = rel.ToID
		}
		e.Relations = append(e.Relations, rel)
	}
	if err := relRows.Err(); err != nil {
		relRows.Close()
		return nil, err
	}
	relRows.Close()
	return e, nil
}

// Get returns one entry with evidence and relations.
func (s *Store) Get(id string) (*Entry, error) {
	return s.getEntry(strings.TrimSpace(id), true)
}

func (s *Store) indexEntry(e *Entry) error {
	var rowid int64
	if err := s.db.QueryRow("SELECT rowid FROM knowledge_entries WHERE id=?", e.ID).Scan(&rowid); err != nil {
		return err
	}
	if _, err := s.db.Exec("DELETE FROM knowledge_fts WHERE rowid=?", rowid); err != nil {
		return err
	}
	if e.Status == "quarantined" {
		return nil
	}
	_, err := s.db.Exec(`INSERT INTO knowledge_fts(rowid,entry_id,title_norm,summary_norm,body_norm,tags_norm)
		VALUES(?,?,?,?,?,?)`, rowid, e.ID, NormalizeFTS(e.Title), NormalizeFTS(e.Summary),
		NormalizeFTS(e.Body), NormalizeFTS(strings.Join(e.Tags, " ")))
	return err
}

func (s *Store) insertEvidenceTx(tx *sql.Tx, entryID string, evidence []Evidence) error {
	for _, ev := range evidence {
		ev.Kind = strings.TrimSpace(ev.Kind)
		ev.Ref = strings.TrimSpace(ev.Ref)
		if ev.Kind == "" || ev.Ref == "" {
			continue
		}
		ev.Quote = clipRunes(ev.Quote, 512)
		ev.Quote, _ = RedactSensitive(ev.Quote)
		if _, err := tx.Exec(`INSERT OR IGNORE INTO knowledge_evidence(entry_id,kind,ref,locator,quote,captured_at)
			VALUES(?,?,?,?,?,?)`, entryID, ev.Kind, ev.Ref, ev.Locator, ev.Quote,
			defaultString(ev.CapturedAt, now())); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) auditTx(tx *sql.Tx, entryID, actor, action, detail string) error {
	if detail == "" {
		detail = "{}"
	}
	_, err := tx.Exec(`INSERT INTO knowledge_events(entry_id,at,actor,action,detail)
		VALUES(?,?,?,?,?)`, entryID, now(), actor, action, detail)
	return err
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// Create inserts a draft (or approved, for an admin). It is idempotent on the
// dedupe key: a hit appends evidence to the existing entry and returns
// duplicate=true instead of creating a second card. Sensitive hits are
// redacted and stored as quarantined, never indexed.
func (s *Store) Create(in CreateInput) (*Entry, bool, error) {
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return nil, false, fmt.Errorf("title required")
	}
	title = clipRunes(title, 200)
	summary := clipRunes(strings.TrimSpace(in.Summary), 500)
	body := clipRunes(in.Body, 256*1024)
	if summary == "" && body == "" {
		return nil, false, fmt.Errorf("summary or body required")
	}
	project := strings.TrimSpace(in.Project)
	status := strings.ToLower(strings.TrimSpace(in.Status))
	if status == "" {
		status = "draft"
	}
	if status != "draft" && status != "approved" {
		return nil, false, fmt.Errorf("status must be draft or approved")
	}
	sensitivity := strings.ToLower(strings.TrimSpace(in.Sensitivity))
	if sensitivity == "" {
		sensitivity = "internal"
	}
	if !validSensitivity(sensitivity) {
		return nil, false, fmt.Errorf("invalid sensitivity: %s", sensitivity)
	}
	generatedBy := strings.TrimSpace(in.GeneratedBy)
	if generatedBy == "" {
		generatedBy = "human"
	}
	confidence := in.Confidence
	if confidence <= 0 {
		confidence = 50
	}
	if confidence > 100 {
		confidence = 100
	}
	sourceKind := strings.TrimSpace(in.SourceKind)
	if sourceKind == "" {
		sourceKind = "manual"
	}
	evidence := append([]Evidence(nil), in.Evidence...)

	s.mu.Lock()
	defer s.mu.Unlock()

	redacted, findings := RedactSensitive(title + "\n" + summary + "\n" + body)
	if len(findings) > 0 {
		status = "quarantined"
		sensitivity = "sensitive"
		_ = redacted // redaction is applied field-by-field below
	}
	rt, rf := RedactSensitive(title)
	rs, _ := RedactSensitive(summary)
	rb, _ := RedactSensitive(body)
	title, summary, body = rt, rs, rb
	_ = rf

	key := dedupeKey(project, title, body)
	var existing string
	err := s.db.QueryRow(`SELECT id FROM knowledge_entries
		WHERE dedupe_key=? AND deleted_at='' AND status IN ('draft','approved','stale') LIMIT 1`, key).Scan(&existing)
	if err == nil && existing != "" {
		if len(evidence) > 0 {
			tx, txErr := s.db.Begin()
			if txErr == nil {
				_ = s.insertEvidenceTx(tx, existing, evidence)
				_ = tx.Commit()
			}
		}
		_, _ = s.db.Exec("UPDATE knowledge_entries SET updated_at=? WHERE id=?", now(), existing)
		entry, getErr := s.getEntry(existing, true)
		if getErr != nil {
			return nil, false, getErr
		}
		return entry, true, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}

	id := newID()
	at := now()
	lastVerified := ""
	reviewer := ""
	reviewedAt := ""
	if status == "approved" {
		lastVerified = at
		reviewer = in.Actor
		reviewedAt = at
	}
	rewardReason := ""
	if len(findings) > 0 {
		rewardReason = "sensitive: " + findingTypes(findings)
	}
	tags := sanitizeTags(in.Tags)
	sourceArtifacts := sanitizeStrings(in.SourceArtifactIDs)
	sourcePaths := sanitizeStrings(in.SourcePaths)
	aiFields := sanitizeStrings(in.AIFields)

	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, err
	}
	_, err = tx.Exec(`INSERT INTO knowledge_entries(id,title,summary,body,project,owner,status,
		confidence,sensitivity,source_kind,source_task_id,source_session_id,source_artifact_ids,
		source_paths,dedupe_key,content_hash,generated_by,ai_fields,version,supersedes_id,
		last_verified_at,valid_until,reviewer,reviewed_at,review_reason,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, title, summary, body, project, strings.TrimSpace(in.Owner), status, confidence,
		sensitivity, sourceKind, strings.TrimSpace(in.SourceTaskID), strings.TrimSpace(in.SourceSessionID),
		jsonStrings(sourceArtifacts), jsonStrings(sourcePaths), key, contentHash(body), generatedBy,
		jsonStrings(aiFields), 1, "", lastVerified, "", reviewer, reviewedAt, rewardReason, at, at)
	if err != nil {
		tx.Rollback()
		return nil, false, err
	}
	for _, tag := range tags {
		if _, err := tx.Exec("INSERT OR IGNORE INTO knowledge_tags(entry_id,tag) VALUES(?,?)", id, tag); err != nil {
			tx.Rollback()
			return nil, false, err
		}
	}
	if err := s.insertEvidenceTx(tx, id, evidence); err != nil {
		tx.Rollback()
		return nil, false, err
	}
	_ = s.auditTx(tx, id, in.Actor, "create", `{"source_kind":`+jsonQuote(sourceKind)+`}`)
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	entry, err := s.getEntry(id, true)
	if err != nil {
		return nil, false, err
	}
	if err := s.indexEntry(entry); err != nil {
		return nil, false, err
	}
	return entry, false, nil
}

func jsonQuote(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

func sanitizeStrings(values []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func validSensitivity(value string) bool {
	switch value {
	case "public", "internal", "sensitive", "secret":
		return true
	}
	return false
}

func validStatus(value string) bool {
	switch value {
	case "draft", "approved", "stale", "superseded", "rejected", "quarantined":
		return true
	}
	return false
}

// Update applies a partial edit. Status changes are authorized by the server
// (admin only); the store only validates the value.
func (s *Store) Update(id string, in UpdateInput) (*Entry, error) {
	id = strings.TrimSpace(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, err := s.getEntry(id, true)
	if err != nil {
		return nil, err
	}
	contentChanged := false
	sensitiveHit := false
	if in.Title != nil {
		title := clipRunes(strings.TrimSpace(*in.Title), 200)
		if title == "" {
			return nil, fmt.Errorf("title required")
		}
		if title != entry.Title {
			entry.Title = title
			contentChanged = true
		}
	}
	if in.Summary != nil {
		summary := clipRunes(strings.TrimSpace(*in.Summary), 500)
		if summary != entry.Summary {
			entry.Summary = summary
			contentChanged = true
		}
	}
	if in.Body != nil {
		body := clipRunes(*in.Body, 256*1024)
		if body != entry.Body {
			entry.Body = body
			contentChanged = true
		}
	}
	if in.Project != nil {
		project := strings.TrimSpace(*in.Project)
		if project != entry.Project {
			entry.Project = project
			contentChanged = true
		}
	}
	if in.Tags != nil {
		entry.Tags = sanitizeTags(*in.Tags)
		contentChanged = true
	}
	if in.Confidence != nil {
		confidence := *in.Confidence
		if confidence < 0 {
			confidence = 0
		}
		if confidence > 100 {
			confidence = 100
		}
		entry.Confidence = confidence
	}
	if in.Sensitivity != nil && validSensitivity(strings.ToLower(strings.TrimSpace(*in.Sensitivity))) {
		entry.Sensitivity = strings.ToLower(strings.TrimSpace(*in.Sensitivity))
	}
	if contentChanged {
		entry.Title, _ = RedactSensitive(entry.Title)
		entry.Summary, _ = RedactSensitive(entry.Summary)
		entry.Body, _ = RedactSensitive(entry.Body)
		entry.DedupeKey = dedupeKey(entry.Project, entry.Title, entry.Body)
		entry.ContentHash = contentHash(entry.Body)
		entry.Version++
		var findings []Finding
		_, findings = RedactSensitive(entry.Title + "\n" + entry.Summary + "\n" + entry.Body)
		if len(findings) > 0 {
			sensitiveHit = true
			entry.Status = "quarantined"
			entry.Sensitivity = "sensitive"
			entry.ReviewReason = "sensitive: " + findingTypes(findings)
		}
	}
	if in.Status != nil {
		status := strings.ToLower(strings.TrimSpace(*in.Status))
		if !validStatus(status) {
			return nil, fmt.Errorf("invalid status: %s", status)
		}
		entry.Status = status
		at := now()
		if in.Reviewer != nil {
			entry.Reviewer = strings.TrimSpace(*in.Reviewer)
		}
		if in.ReviewReason != nil {
			entry.ReviewReason = strings.TrimSpace(*in.ReviewReason)
		}
		switch status {
		case "approved":
			entry.ReviewedAt = at
			entry.LastVerifiedAt = at
		case "rejected", "stale":
			entry.ReviewedAt = at
		case "draft":
			// keep review metadata for audit
		}
	}
	// A content edit that hits the sensitive scanner always wins over a
	// concurrent approve/reject request.
	if sensitiveHit {
		entry.Status = "quarantined"
		entry.Sensitivity = "sensitive"
	}
	entry.UpdatedAt = now()

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	_, err = tx.Exec(`UPDATE knowledge_entries SET title=?,summary=?,body=?,project=?,status=?,
		confidence=?,sensitivity=?,dedupe_key=?,content_hash=?,version=?,last_verified_at=?,
		reviewer=?,reviewed_at=?,review_reason=?,updated_at=? WHERE id=?`,
		entry.Title, entry.Summary, entry.Body, entry.Project, entry.Status, entry.Confidence,
		entry.Sensitivity, entry.DedupeKey, entry.ContentHash, entry.Version, entry.LastVerifiedAt,
		entry.Reviewer, entry.ReviewedAt, entry.ReviewReason, entry.UpdatedAt, id)
	if err != nil {
		tx.Rollback()
		return nil, err
	}
	_, _ = tx.Exec("DELETE FROM knowledge_tags WHERE entry_id=?", id)
	for _, tag := range entry.Tags {
		_, _ = tx.Exec("INSERT OR IGNORE INTO knowledge_tags(entry_id,tag) VALUES(?,?)", id, tag)
	}
	action := strings.TrimSpace(in.Action)
	if action == "" {
		action = "update"
	}
	_ = s.auditTx(tx, id, in.Actor, action, `{"status":`+jsonQuote(entry.Status)+`}`)
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	updated, err := s.getEntry(id, true)
	if err != nil {
		return nil, err
	}
	if err := s.indexEntry(updated); err != nil {
		return nil, err
	}
	return updated, nil
}

// Search returns one page plus the total. Workers pass IncludeDrafts=false and
// IncludeSensitive=false; the server enforces that for non-admin callers.
func (s *Store) Search(opts SearchOptions) (*SearchResult, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 50 {
		limit = 50
	}
	offset := opts.Offset
	if offset < 0 {
		offset = 0
	}
	q := strings.TrimSpace(opts.Query)
	match := MatchQuery(q)

	joins := ""
	if match != "" {
		joins = " JOIN knowledge_fts ON knowledge_fts.rowid = e.rowid"
	}
	where := []string{"e.deleted_at=''"}
	args := []any{}
	if match != "" {
		where = append(where, "knowledge_fts MATCH ?")
		args = append(args, match)
	}
	status := strings.ToLower(strings.TrimSpace(opts.Status))
	if status != "" && status != "all" {
		if !validStatus(status) {
			return nil, fmt.Errorf("invalid status: %s", status)
		}
		where = append(where, "e.status = ?")
		args = append(args, status)
	} else if status == "all" {
		where = append(where, "e.status IN ('draft','approved','stale','rejected','superseded')")
	} else {
		allowed := []string{"'approved'"}
		if opts.IncludeDrafts {
			allowed = append(allowed, "'draft'")
		}
		if opts.IncludeStale {
			allowed = append(allowed, "'stale'")
		}
		where = append(where, "e.status IN ("+strings.Join(allowed, ",")+")")
	}
	if !opts.IncludeSensitive {
		where = append(where, "e.status <> 'quarantined'")
		where = append(where, "e.sensitivity NOT IN ('sensitive','secret')")
	}
	if project := strings.TrimSpace(opts.Project); project != "" {
		where = append(where, "e.project = ?")
		args = append(args, project)
	}
	if tag := strings.TrimSpace(opts.Tag); tag != "" {
		where = append(where, "EXISTS (SELECT 1 FROM knowledge_tags t WHERE t.entry_id=e.id AND t.tag=?)")
		args = append(args, tag)
	}
	if taskID := strings.TrimSpace(opts.SourceTaskID); taskID != "" {
		where = append(where, "e.source_task_id = ?")
		args = append(args, taskID)
	}
	whereSQL := " WHERE " + strings.Join(where, " AND ")

	var total int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM knowledge_entries e"+joins+whereSQL, args...).Scan(&total); err != nil {
		return nil, err
	}
	order := " ORDER BY CASE e.status WHEN 'approved' THEN 0 WHEN 'draft' THEN 1 ELSE 2 END, e.updated_at DESC"
	if match != "" {
		order = " ORDER BY bm25(knowledge_fts, 0.0, 5.0, 2.0, 1.0, 3.0) ASC, e.updated_at DESC"
	}
	queryArgs := append(append([]any{}, args...), limit, offset)
	rows, err := s.db.Query("SELECT e.id FROM knowledge_entries e"+joins+whereSQL+order+" LIMIT ? OFFSET ?", queryArgs...)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	out := make([]*Entry, 0, len(ids))
	for _, id := range ids {
		entry, err := s.getEntry(id, false)
		if err != nil {
			return nil, err
		}
		entry.Snippet = makeSnippet(q, entry.Title, entry.Summary, entry.Body, 180)
		out = append(out, entry)
	}
	return &SearchResult{Entries: out, Total: total}, nil
}

func makeSnippet(query, title, summary, body string, max int) string {
	text := strings.TrimSpace(summary)
	if text == "" {
		text = strings.TrimSpace(body)
	}
	if text == "" {
		return clipRunes(strings.TrimSpace(title), max)
	}
	lower := strings.ToLower(text)
	for _, term := range QueryTerms(query) {
		if term == "" {
			continue
		}
		if idx := strings.Index(lower, strings.ToLower(term)); idx >= 0 {
			runeIdx := len([]rune(lower[:idx]))
			start := runeIdx - max/3
			if start < 0 {
				start = 0
			}
			runes := []rune(text)
			end := start + max
			if start > len(runes) {
				start = len(runes)
			}
			if end > len(runes) {
				end = len(runes)
			}
			prefix := ""
			if start > 0 {
				prefix = "…"
			}
			suffix := ""
			if end < len(runes) {
				suffix = "…"
			}
			return prefix + string(runes[start:end]) + suffix
		}
	}
	return clipRunes(text, max)
}

// Facets returns status counts and the distinct project list for the UI.
func (s *Store) Facets() (map[string]int, []string, error) {
	counts := map[string]int{}
	rows, err := s.db.Query("SELECT status, COUNT(*) FROM knowledge_entries WHERE deleted_at='' GROUP BY status")
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			rows.Close()
			return nil, nil, err
		}
		counts[status] = n
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, err
	}
	rows.Close()
	projectRows, err := s.db.Query(`SELECT DISTINCT project FROM knowledge_entries
		WHERE deleted_at='' AND project<>'' ORDER BY project`)
	if err != nil {
		return nil, nil, err
	}
	projects := []string{}
	for projectRows.Next() {
		var project string
		if err := projectRows.Scan(&project); err != nil {
			projectRows.Close()
			return nil, nil, err
		}
		projects = append(projects, project)
	}
	if err := projectRows.Err(); err != nil {
		projectRows.Close()
		return nil, nil, err
	}
	projectRows.Close()
	return counts, projects, nil
}

// SummariesForTasks is the board-drawer enrichment: up to limit entries per
// task, approved first, no quarantined rows.
func (s *Store) SummariesForTasks(taskIDs []string, limit int) (map[string][]Summary, map[string]int, error) {
	out := map[string][]Summary{}
	counts := map[string]int{}
	if len(taskIDs) == 0 {
		return out, counts, nil
	}
	if limit <= 0 {
		limit = 3
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(taskIDs)), ",")
	args := make([]any, 0, len(taskIDs))
	for _, id := range taskIDs {
		args = append(args, id)
	}
	rows, err := s.db.Query(`SELECT id,title,status,project,updated_at,source_task_id
		FROM knowledge_entries
		WHERE source_task_id IN (`+placeholders+`) AND deleted_at=''
		  AND status IN ('draft','approved','stale')
		ORDER BY CASE status WHEN 'approved' THEN 0 WHEN 'draft' THEN 1 ELSE 2 END, updated_at DESC`, args...)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var item Summary
		var taskID string
		if err := rows.Scan(&item.ID, &item.Title, &item.Status, &item.Project, &item.UpdatedAt, &taskID); err != nil {
			rows.Close()
			return nil, nil, err
		}
		counts[taskID]++
		if len(out[taskID]) < limit {
			out[taskID] = append(out[taskID], item)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, err
	}
	rows.Close()
	return out, counts, nil
}

// HasTaskEntry reports whether a non-rejected entry already exists for a task.
// It is the collect idempotency guard: repeated manual collects do not create
// another card.
func (s *Store) HasTaskEntry(taskID string) (bool, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return false, nil
	}
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM knowledge_entries
		WHERE source_task_id=? AND deleted_at='' AND status NOT IN ('rejected')`, taskID).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// Reindex rebuilds the FTS table from entries. Safe to run at startup or via
// an admin endpoint.
func (s *Store) Reindex() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec("DELETE FROM knowledge_fts"); err != nil {
		return 0, err
	}
	rows, err := s.db.Query("SELECT id FROM knowledge_entries WHERE deleted_at=''")
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	for _, id := range ids {
		entry, err := s.getEntry(id, true)
		if err != nil {
			return 0, err
		}
		if err := s.indexEntry(entry); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// SortSummaries is a small helper for deterministic tests.
func SortSummaries(list []Summary) {
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
}
