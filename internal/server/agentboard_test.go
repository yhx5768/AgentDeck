package server

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"cc-webui/internal/config"
)

func TestServeStaticQueryTokenSetsCookie(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{DataDir: dir, GatewayToken: "preview-token"}
	s := &Server{
		cm: config.NewManager(filepath.Join(dir, "config.yaml"), cfg),
		static: fstest.MapFS{
			"index.html": {Data: []byte("<!doctype html><link rel=stylesheet href=/style.css>")},
			"style.css":  {Data: []byte("body{color:red}")},
		},
	}
	w := httptest.NewRecorder()
	s.serveStatic().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/?token=preview-token", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("index code = %d", w.Code)
	}
	cookies := w.Result().Cookies()
	if len(cookies) == 0 || cookies[0].Name != cookieName || cookies[0].Value != "preview-token" {
		t.Fatalf("cookies = %+v", cookies)
	}

	w = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/style.css", nil)
	req.AddCookie(cookies[0])
	s.serveStatic().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("style.css code = %d", w.Code)
	}
}

func TestBoardHubBroadcast(t *testing.T) {
	h := newBoardHub()
	a := h.subscribe()
	b := h.subscribe()
	h.broadcast(boardChange{TaskID: "T-1", Kind: "update", Status: "进行中"})
	var seqs []uint64
	for _, sub := range []*boardSub{a, b} {
		select {
		case <-sub.notify:
		case <-time.After(time.Second):
			t.Fatal("subscriber did not receive broadcast")
		}
		full, changes := sub.take()
		if full || len(changes) != 1 || changes[0].TaskID != "T-1" || changes[0].Kind != "update" {
			t.Fatalf("take() = full:%v changes:%+v", full, changes)
		}
		if changes[0].At == "" {
			t.Fatal("broadcast should stamp a server time")
		}
		if changes[0].Seq == 0 {
			t.Fatal("broadcast should stamp a sequence")
		}
		seqs = append(seqs, changes[0].Seq)
	}
	if seqs[0] != seqs[1] {
		t.Fatalf("every subscriber must see the same seq, got %v", seqs)
	}
	h.unsubscribe(a)
	h.unsubscribe(b)
}

func TestBoardHubCoalescesPerTask(t *testing.T) {
	h := newBoardHub()
	sub := h.subscribe()
	h.broadcast(boardChange{TaskID: "T-1", Kind: "event", At: "2026-11-19 10:00:00"})
	h.broadcast(boardChange{TaskID: "T-1", Kind: "reply", At: "2026-11-19 10:00:01"})
	h.broadcast(boardChange{TaskID: "T-2", Kind: "update", At: "2026-11-19 10:00:02"})
	<-sub.notify
	full, changes := sub.take()
	if full || len(changes) != 2 {
		t.Fatalf("take() = full:%v changes:%+v", full, changes)
	}
	byID := map[string]boardChange{}
	for _, ev := range changes {
		byID[ev.TaskID] = ev
	}
	if byID["T-1"].Kind != "reply" {
		t.Fatalf("latest per-task change should win, got %+v", byID["T-1"])
	}
	if byID["T-2"].Kind != "update" {
		t.Fatalf("T-2 = %+v", byID["T-2"])
	}

	// A resync marker supersedes queued deltas.
	h.broadcast(boardChange{TaskID: "T-3", Kind: "update"})
	h.broadcast(boardChange{Kind: "resync"})
	<-sub.notify
	full, changes = sub.take()
	if !full || len(changes) != 0 {
		t.Fatalf("resync should discard deltas, got full:%v changes:%+v", full, changes)
	}
}

func TestHandleAgentBoardEventsStreams(t *testing.T) {
	dir := t.TempDir()
	hub := newBoardHub()
	s := &Server{
		cm:       config.NewManager(filepath.Join(dir, "config.yaml"), &config.Config{DataDir: dir}),
		boardHub: hub,
	}
	ts := httptest.NewServer(http.HandlerFunc(s.handleAgentBoardEvents))
	defer ts.Close()

	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	reader := bufio.NewReader(resp.Body)
	readEvent := func() string {
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if strings.HasPrefix(line, "event: ") {
				return strings.TrimSpace(strings.TrimPrefix(line, "event: "))
			}
		}
	}
	if got := readEvent(); got != "ready" {
		t.Fatalf("first event = %q", got)
	}
	hub.broadcast(boardChange{TaskID: "T-1", Kind: "update", Status: "已完成"})
	if got := readEvent(); got != "board" {
		t.Fatalf("second event = %q", got)
	}
	// The data line follows the event line and carries the delta.
	var data string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read data: %v", err)
		}
		if strings.HasPrefix(line, "data: ") {
			data = strings.TrimSpace(strings.TrimPrefix(line, "data: "))
			break
		}
	}
	if !strings.Contains(data, `"task_id":"T-1"`) || !strings.Contains(data, `"status":"已完成"`) {
		t.Fatalf("delta data = %s", data)
	}

	hub.broadcast(boardChange{})
	if got := readEvent(); got != "board" {
		t.Fatalf("third event = %q", got)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read data: %v", err)
		}
		if strings.HasPrefix(line, "data: ") {
			data = strings.TrimSpace(strings.TrimPrefix(line, "data: "))
			break
		}
	}
	if !strings.Contains(data, `"full":true`) {
		t.Fatalf("resync data = %s", data)
	}
}
