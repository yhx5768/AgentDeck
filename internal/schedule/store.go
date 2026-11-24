package schedule

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Task is one scheduled prompt: cron when to fire, prompt what to send, and
// target the session address/id to deliver it to.
//
// NextRunAt is persisted so a restart shows the same "next run" in the UI and
// the scheduler does not need a side computation per read. It is only set for
// enabled tasks; disabled tasks show "—".
type Task struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Cron          string     `json:"cron"`
	Prompt        string     `json:"prompt"`
	TargetSession string     `json:"target_session"`
	Enabled       bool       `json:"enabled"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	LastRunAt     *time.Time `json:"last_run_at,omitempty"`
	// LastStatus is "ok" or an error string prefixed with "error: ".
	LastStatus string     `json:"last_status,omitempty"`
	NextRunAt  *time.Time `json:"next_run_at,omitempty"`
	// Source/SourceID/SourcePath record that this task was imported from another
	// system. Empty source = created in AgentDeck. "harness" = imported from
	// Claude Code's .claude/scheduled_tasks.json; the file is never modified, so
	// the import is a copy the user can pause/delete independently. Duplicate
	// detection is per (path, id) because different projects may reuse short ids.
	Source     string `json:"source,omitempty"`
	SourceID   string `json:"source_id,omitempty"`
	SourcePath string `json:"source_path,omitempty"`
	// MaxRuns > 0 marks a one-shot task: after RunCount reaches it the task
	// disables itself and NextRunAt is cleared. 0 means unlimited.
	MaxRuns  int `json:"max_runs,omitempty"`
	RunCount int `json:"run_count,omitempty"`
}

// Exhausted reports whether a one-shot task has used up its runs.
func (t *Task) Exhausted() bool {
	return t.MaxRuns > 0 && t.RunCount >= t.MaxRuns
}

// Store persists tasks in one JSON file (atomic temp+rename). It is safe for
// concurrent use: the scheduler and HTTP handlers share it.
type Store struct {
	mu    sync.Mutex
	path  string
	tasks []*Task
}

type schedulesFile struct {
	Tasks []*Task `json:"tasks"`
}

// Open loads <path>, creating an empty store when the file does not exist.
// A malformed file is moved aside (<path>.corrupt-<ts>) and an empty store is
// returned together with the parse error; callers should log it and continue —
// one bad file must not keep the whole console from booting.
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return s, err
	}
	var f schedulesFile
	if err := json.Unmarshal(raw, &f); err != nil {
		aside := path + ".corrupt-" + time.Now().Format("20060102-150405")
		if mvErr := os.Rename(path, aside); mvErr != nil {
			return s, fmt.Errorf("parse schedules.json: %w（备份失败：%v）", err, mvErr)
		}
		return s, fmt.Errorf("parse schedules.json: %w（已备份到 %s）", err, aside)
	}
	for _, t := range f.Tasks {
		if t == nil || strings.TrimSpace(t.ID) == "" {
			continue
		}
		s.tasks = append(s.tasks, t)
	}
	return s, nil
}

func (s *Store) saveLocked() error {
	f := schedulesFile{Tasks: s.tasks}
	if f.Tasks == nil {
		f.Tasks = []*Task{}
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	cp := *t
	return &cp
}

func cloneTask(t *Task) *Task {
	if t == nil {
		return nil
	}
	cp := *t
	cp.LastRunAt = cloneTime(t.LastRunAt)
	cp.NextRunAt = cloneTime(t.NextRunAt)
	return &cp
}

// List returns deep copies in stored order (oldest created first).
func (s *Store) List() []*Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Task, 0, len(s.tasks))
	for _, t := range s.tasks {
		out = append(out, cloneTask(t))
	}
	return out
}

// Get returns a deep copy, or nil when the id is unknown.
func (s *Store) Get(id string) *Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tasks {
		if t.ID == id {
			return cloneTask(t)
		}
	}
	return nil
}

func newTaskID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("sch-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// Create inserts a task, assigning an id and timestamps when missing.
func (s *Store) Create(t *Task) (*Task, error) {
	now := time.Now()
	cp := cloneTask(t)
	if strings.TrimSpace(cp.ID) == "" {
		cp.ID = newTaskID()
	}
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = now
	}
	cp.UpdatedAt = now
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, cur := range s.tasks {
		if cur.ID == cp.ID {
			return nil, fmt.Errorf("定时任务 id 重复：%s", cp.ID)
		}
	}
	s.tasks = append(s.tasks, cp)
	if err := s.saveLocked(); err != nil {
		s.tasks = s.tasks[:len(s.tasks)-1]
		return nil, err
	}
	return cloneTask(cp), nil
}

// Update applies mutate to the task under lock and rewrites the file. It
// returns a deep copy of the updated task.
func (s *Store) Update(id string, mutate func(*Task)) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tasks {
		if t.ID != id {
			continue
		}
		prev := cloneTask(t)
		if mutate != nil {
			mutate(t)
		}
		t.UpdatedAt = time.Now()
		if err := s.saveLocked(); err != nil {
			*t = *prev // roll back so memory matches the disk after a failed write
			return nil, err
		}
		return cloneTask(t), nil
	}
	return nil, fmt.Errorf("定时任务不存在")
}

// Delete removes a task and rewrites the file.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, t := range s.tasks {
		if t.ID != id {
			continue
		}
		removed := t
		s.tasks = append(s.tasks[:i], s.tasks[i+1:]...)
		if err := s.saveLocked(); err != nil {
			// Put it back (same position is not important for correctness).
			s.tasks = append(s.tasks, removed)
			return err
		}
		return nil
	}
	return fmt.Errorf("定时任务不存在")
}

// ClaimDue advances and returns enabled tasks whose NextRunAt is at or before
// now. Claiming moves NextRunAt forward (strictly after now) and persists that
// change before returning, so overlapping ticks or a second process cannot
// deliver the same occurrence twice. A missed occurrence while the process was
// down is skipped, never caught up in a burst.
func (s *Store) ClaimDue(now time.Time) []*Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	var due []*Task
	changed := false
	for _, t := range s.tasks {
		if !t.Enabled || t.Exhausted() || t.NextRunAt == nil || t.NextRunAt.After(now) {
			continue
		}
		c, err := ParseCron(t.Cron)
		if err != nil {
			// Do not deliver: a task whose cron no longer parses has no valid
			// occurrence. Record why and stop scheduling it until it is edited.
			log.Printf("schedule %s: bad cron %q: %v", t.ID, t.Cron, err)
			t.LastStatus = "error: " + err.Error()
			t.NextRunAt = nil
			changed = true
			continue
		}
		if next, ok := c.Next(now); ok {
			t.NextRunAt = &next
		} else {
			t.NextRunAt = nil
		}
		due = append(due, cloneTask(t))
		changed = true
	}
	if changed {
		if err := s.saveLocked(); err != nil {
			log.Printf("schedule: save after claim: %v", err)
		}
	}
	return due
}

// RecordRun records the outcome of one delivery and advances the run counter.
// A one-shot task (MaxRuns > 0) disables itself once the limit is reached, so a
// stale NextRunAt can never fire it again.
func (s *Store) RecordRun(id, status string, at time.Time) (*Task, error) {
	return s.Update(id, func(t *Task) {
		t.LastRunAt = &at
		t.LastStatus = status
		t.RunCount++
		if t.Exhausted() {
			t.Enabled = false
			t.NextRunAt = nil
		}
	})
}
