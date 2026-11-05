package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"cc-webui/internal/a2a"
	"cc-webui/internal/config"
	"cc-webui/internal/store"
)

func bindWorkerCard(t *testing.T, s *Server, taskID, worker, session string) {
	t.Helper()
	if _, err := s.boardDB.CreateTask(map[string]any{"id": taskID, "title": "任务", "worker": worker}); err != nil {
		t.Fatal(err)
	}
	if err := s.boardDB.BindWorker(taskID, worker, session, ""); err != nil {
		t.Fatal(err)
	}
}

func waitCond(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
}

// countUserEvents counts persisted user events on a session matching pred.
func countUserEvents(st *store.Store, id string, pred func(map[string]string) bool) int {
	evs, _ := st.Events(id)
	n := 0
	for _, e := range evs {
		if e.Kind != "user" {
			continue
		}
		var d map[string]string
		if json.Unmarshal(e.Data, &d) != nil {
			continue
		}
		if pred(d) {
			n++
		}
	}
	return n
}

func incidentBody(_ *store.Session, _ map[string]any) (string, string) {
	return "worker 异常结束\n- 错误：boom", "worker 异常结束：boom"
}

func TestWorkerIncidentRoutesToMainAgent(t *testing.T) {
	s, st := boardMCPServer(t)
	bindWorkerCard(t, s, "T-1", "worker", "worker")

	s.onWorkerTurnError("worker", "cli crashed: boom")

	waitCond(t, func() bool {
		return countUserEvents(st, "main", func(d map[string]string) bool {
			return d["source"] == a2a.Source && d["from"] == incidentFrom && strings.Contains(d["body"], "worker 异常结束")
		}) == 1
	})

	task, err := s.boardDB.GetTask("T-1")
	if err != nil {
		t.Fatal(err)
	}
	timeline, _ := task["timeline"].([]map[string]any)
	found := false
	for _, ev := range timeline {
		if ev["kind"] == "worker 异常" {
			found = true
		}
	}
	if !found {
		t.Fatalf("card timeline missing the incident note: %+v", timeline)
	}
}

func TestWorkerIncidentSkipsUnboundWorker(t *testing.T) {
	s, st := boardMCPServer(t)
	// "worker" is not bound to any card → not a dispatched worker → no notify.
	s.notifyWorker("worker", "worker 异常", incidentBody)
	if n := countUserEvents(st, "main", func(map[string]string) bool { return true }); n != 0 {
		t.Fatalf("unbound worker must not notify the main agent (got %d)", n)
	}
}

func TestWorkerIncidentRateLimited(t *testing.T) {
	s, st := boardMCPServer(t)
	bindWorkerCard(t, s, "T-1", "worker", "worker")

	s.notifyWorker("worker", "worker 异常", incidentBody)
	s.notifyWorker("worker", "worker 异常", incidentBody)

	if n := countUserEvents(st, "main", func(d map[string]string) bool { return d["source"] == a2a.Source }); n != 1 {
		t.Fatalf("per-worker cooldown must collapse repeats (got %d deliveries)", n)
	}
}

func TestWorkerNotifyDisabled(t *testing.T) {
	s, st := boardMCPServer(t)
	bindWorkerCard(t, s, "T-1", "worker", "worker")
	off := false
	s.cm.Get().MainAgent = &config.MainAgentConfig{Notify: config.NotifyConfig{Enabled: &off}}
	s.notifyWorker("worker", "worker 异常", incidentBody)
	if n := countUserEvents(st, "main", func(map[string]string) bool { return true }); n != 0 {
		t.Fatalf("notify disabled must deliver nothing (got %d)", n)
	}
}
