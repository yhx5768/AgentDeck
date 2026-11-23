package server

import (
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"cc-webui/internal/runner"
	"cc-webui/internal/schedule"
)

// scheduleTick is how often the scheduler asks the store for due tasks. Cron
// resolution is one minute, so 15s keeps firing within a few seconds of :00
// while remaining cheap.
const scheduleTick = 15 * time.Second

const (
	maxScheduleNameRunes  = 80
	maxSchedulePromptByte = 32 * 1024
)

// scheduleView is the API shape: the stored task plus the resolved target's
// display name/address so the list does not need a second lookup.
type scheduleView struct {
	*schedule.Task
	TargetName    string `json:"target_name,omitempty"`
	TargetAddress string `json:"target_address,omitempty"`
}

func (s *Server) scheduleView(t *schedule.Task) scheduleView {
	v := scheduleView{Task: t}
	if sess := s.st.Get(t.TargetSession); sess != nil {
		v.TargetName = sess.Name
		v.TargetAddress = sessionAddress(sess)
	}
	return v
}

// scheduleStore lazily opens <data_dir>/schedules.json. Lazy on purpose: many
// tests build a Server literal without New, and the data dir is only known
// through the live config.
func (s *Server) scheduleStore() *schedule.Store {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	if s.schedStore == nil {
		st, err := schedule.Open(filepath.Join(s.cfg().DataDir, "schedules.json"))
		if err != nil {
			log.Printf("schedules: %v", err)
		}
		s.schedStore = st
	}
	return s.schedStore
}

// startScheduler launches the tick loop once. Startup reschedules enabled tasks
// from "now" instead of catching up missed runs: a laptop that was asleep for
// two days must not fire every missed occurrence at boot.
func (s *Server) startScheduler() {
	s.schedStart.Do(func() {
		s.schedMu.Lock()
		stop := make(chan struct{})
		s.schedStop = stop
		s.schedMu.Unlock()
		go s.schedulerLoop(stop)
	})
}

func (s *Server) stopScheduler() {
	s.schedMu.Lock()
	if s.schedStop != nil {
		close(s.schedStop)
		s.schedStop = nil
	}
	s.schedMu.Unlock()
}

func (s *Server) schedulerLoop(stop <-chan struct{}) {
	s.rescheduleAll(time.Now())
	t := time.NewTicker(scheduleTick)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-t.C:
			s.runDueSchedules(now)
		}
	}
}

// rescheduleAll recomputes NextRunAt for enabled tasks whose stored value is
// missing or already in the past. Future values are kept so a config/restart
// does not shift a schedule that is about to fire.
func (s *Server) rescheduleAll(now time.Time) {
	for _, task := range s.scheduleStore().List() {
		if !task.Enabled || (task.NextRunAt != nil && task.NextRunAt.After(now)) {
			continue
		}
		c, err := schedule.ParseCron(task.Cron)
		if err != nil {
			log.Printf("schedule %s: bad cron %q: %v", task.ID, task.Cron, err)
			continue
		}
		next, ok := c.Next(now)
		if !ok {
			continue
		}
		if _, err := s.scheduleStore().Update(task.ID, func(t *schedule.Task) {
			t.NextRunAt = &next
		}); err != nil {
			log.Printf("schedule %s: reschedule: %v", task.ID, err)
		}
	}
}

// runDueSchedules claims due tasks, delivers each one, then records the
// outcome. Claiming advances NextRunAt before delivery, so a slow delivery or
// a second tick cannot fire the same occurrence twice.
func (s *Server) runDueSchedules(now time.Time) {
	for _, task := range s.scheduleStore().ClaimDue(now) {
		status := s.fireSchedule(task)
		if _, err := s.scheduleStore().RecordRun(task.ID, status, time.Now()); err != nil {
			log.Printf("schedule %s: record run: %v", task.ID, err)
		}
	}
}

// fireSchedule delivers the prompt through the same A2A path used by
// send_message and runtime notifications, so permissions/persistence/source
// tagging all behave like any other peer message. The sender is the task name,
// which is what the recipient's peer bubble shows.
func (s *Server) fireSchedule(task *schedule.Task) string {
	from := scheduleSender(task)
	deliver := s.schedDeliver
	if deliver == nil {
		deliver = func(from, to, message string) error {
			_, _, err := s.deliverA2A(from, to, message)
			return err
		}
	}
	if err := deliver(from, task.TargetSession, task.Prompt); err != nil {
		return "error: " + err.Error()
	}
	return "ok"
}

func scheduleSender(task *schedule.Task) string {
	name := runner.CleanPeerName(task.Name)
	if name == "" {
		return "定时任务"
	}
	return runner.CleanPeerName("定时任务 · " + name)
}

type scheduleInput struct {
	Name          string `json:"name"`
	Cron          string `json:"cron"`
	Prompt        string `json:"prompt"`
	TargetSession string `json:"target_session"`
	Enabled       *bool  `json:"enabled"`
}

// validateScheduleInput trims and checks one create/update payload, resolving
// the target address (id or peer name) to a concrete session id. An empty
// target means "mainAgent", mirroring the product default.
func (s *Server) validateScheduleInput(in *scheduleInput) (targetID string, c *schedule.Cron, err error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Cron = strings.TrimSpace(in.Cron)
	in.Prompt = strings.TrimSpace(in.Prompt)
	in.TargetSession = strings.TrimSpace(in.TargetSession)
	if in.Name == "" {
		return "", nil, fmt.Errorf("名称不能为空")
	}
	if len([]rune(in.Name)) > maxScheduleNameRunes {
		return "", nil, fmt.Errorf("名称过长：最多 %d 个字符", maxScheduleNameRunes)
	}
	c, err = schedule.ParseCron(in.Cron)
	if err != nil {
		return "", nil, err
	}
	if _, ok := c.Next(time.Now()); !ok {
		return "", nil, fmt.Errorf("这段 cron 未来 5 年内不会触发")
	}
	if in.Prompt == "" {
		return "", nil, fmt.Errorf("prompt 不能为空")
	}
	if len(in.Prompt) > maxSchedulePromptByte {
		return "", nil, fmt.Errorf("prompt 过长：最多 %d 字节（当前 %d 字节）", maxSchedulePromptByte, len(in.Prompt))
	}
	target := in.TargetSession
	if target == "" {
		target = "mainAgent"
	}
	sess := s.resolveAddress(target)
	if sess == nil {
		return "", nil, fmt.Errorf("目标会话不存在：%s", target)
	}
	return sess.ID, c, nil
}

func (s *Server) handleListSchedules(w http.ResponseWriter, r *http.Request) {
	tasks := s.scheduleStore().List()
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].CreatedAt.After(tasks[j].CreatedAt) })
	out := make([]scheduleView, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, s.scheduleView(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tasks":    out,
		"now":      time.Now().UnixMilli(),
		"timezone": localZoneName(),
	})
}

func (s *Server) handleCreateSchedule(w http.ResponseWriter, r *http.Request) {
	var in scheduleInput
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	targetID, c, err := s.validateScheduleInput(&in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	task := &schedule.Task{
		Name: in.Name, Cron: c.Expr(), Prompt: in.Prompt,
		TargetSession: targetID, Enabled: enabled,
	}
	if enabled {
		if next, ok := c.Next(time.Now()); ok {
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

func (s *Server) handleUpdateSchedule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cur := s.scheduleStore().Get(id)
	if cur == nil {
		writeErr(w, http.StatusNotFound, "定时任务不存在")
		return
	}
	var in scheduleInput
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	targetID, c, err := s.validateScheduleInput(&in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	enabled := cur.Enabled
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	now := time.Now()
	cronChanged := c.Expr() != cur.Cron
	out, err := s.scheduleStore().Update(id, func(t *schedule.Task) {
		t.Name = in.Name
		t.Cron = c.Expr()
		t.Prompt = in.Prompt
		t.TargetSession = targetID
		t.Enabled = enabled
		if enabled && t.MaxRuns > 0 && t.RunCount >= t.MaxRuns {
			t.RunCount = 0 // explicit re-enable of a finished one-shot
		}
		switch {
		case !enabled:
			t.NextRunAt = nil
		case cronChanged || t.NextRunAt == nil:
			if next, ok := c.Next(now); ok {
				t.NextRunAt = &next
			} else {
				t.NextRunAt = nil
			}
		}
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.scheduleView(out))
}

func (s *Server) handleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	if err := s.scheduleStore().Delete(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleEnableSchedule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cur := s.scheduleStore().Get(id)
	if cur == nil {
		writeErr(w, http.StatusNotFound, "定时任务不存在")
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	now := time.Now()
	out, err := s.scheduleStore().Update(id, func(t *schedule.Task) {
		t.Enabled = body.Enabled
		if !body.Enabled {
			t.NextRunAt = nil
			return
		}
		// Re-enabling a finished one-shot is an explicit "run it again": reset
		// the counter so the next occurrence is allowed.
		if t.MaxRuns > 0 && t.RunCount >= t.MaxRuns {
			t.RunCount = 0
		}
		c, err := schedule.ParseCron(t.Cron)
		if err != nil {
			t.NextRunAt = nil
			return
		}
		if next, ok := c.Next(now); ok {
			t.NextRunAt = &next
		} else {
			t.NextRunAt = nil
		}
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.scheduleView(out))
}

func (s *Server) handleRunSchedule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	task := s.scheduleStore().Get(id)
	if task == nil {
		writeErr(w, http.StatusNotFound, "定时任务不存在")
		return
	}
	status := s.fireSchedule(task)
	updated, err := s.scheduleStore().RecordRun(id, status, time.Now())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if strings.HasPrefix(status, "error: ") {
		writeErr(w, http.StatusBadGateway, strings.TrimPrefix(status, "error: "))
		return
	}
	writeJSON(w, http.StatusOK, s.scheduleView(updated))
}

func (s *Server) handlePreviewSchedule(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Cron string `json:"cron"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	c, err := schedule.ParseCron(body.Cron)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	next, ok := c.Next(time.Now())
	if !ok {
		writeErr(w, http.StatusBadRequest, "这段 cron 未来 5 年内不会触发")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"cron":           c.Expr(),
		"next_run_at":    next.Format(time.RFC3339),
		"next_run_local": next.Format("2006-01-02 15:04"),
		"timezone":       localZoneName(),
	})
}

// localZoneName reports the wall-clock zone cron fields are interpreted in
// (e.g. "CST"). time.Local.String() is often just "Local", which tells the UI
// nothing, so prefer the zone abbreviation.
func localZoneName() string {
	if z, _ := time.Now().Zone(); z != "" {
		return z
	}
	return time.Local.String()
}
