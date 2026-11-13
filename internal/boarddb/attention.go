package boarddb

import "strings"

// AttentionTask is the light row the "需要你" queue needs for a card that is
// waiting on a person (待人工 / 待验收). It deliberately skips timelines and
// artifacts: the queue is polled every few seconds, so it must not pay for the
// full Snapshot.
type AttentionTask struct {
	ID           string
	Title        string
	Status       string
	StatusReason string
	Progress     string
	Project      string
	Worker       string
	Session      string
	Cwd          string
	UpdatedAt    string
	// Since is when the card entered its current status: the newest "状态"
	// event of the form "<old> → <status>". Empty when the card never got one
	// (e.g. created directly in that status) — callers fall back to UpdatedAt.
	Since string
}

// AttentionTasks lists non-archived cards in any of the given statuses, newest
// update first. It is lock-free like Snapshot/GetTask (the pool is capped at
// one connection, so the rows are drained before returning).
func (s *Store) AttentionTasks(statuses ...string) ([]AttentionTask, error) {
	if len(statuses) == 0 {
		return []AttentionTask{}, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(statuses)), ",")
	args := make([]any, len(statuses))
	for i, st := range statuses {
		args[i] = st
	}
	rows, err := s.db.Query(`SELECT t.id,t.title,t.status,t.status_reason,t.progress,t.project,
		t.assignee,t.session_id,t.cwd,t.updated_at,
		COALESCE((SELECT MAX(e.at) FROM task_events e
			WHERE e.task_id=t.id AND e.kind='状态' AND e.text LIKE '% → ' || t.status),'') AS since
		FROM tasks t WHERE t.archived=0 AND t.status IN (`+marks+`)
		ORDER BY t.updated_at DESC, t.id ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AttentionTask{}
	for rows.Next() {
		var t AttentionTask
		if err := rows.Scan(&t.ID, &t.Title, &t.Status, &t.StatusReason, &t.Progress, &t.Project,
			&t.Worker, &t.Session, &t.Cwd, &t.UpdatedAt, &t.Since); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
