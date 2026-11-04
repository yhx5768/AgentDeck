package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cc-webui/internal/config"
	"cc-webui/internal/runner"
	"cc-webui/internal/store"
)

// replayTestServer wires a Server with no real CLI, plus a session holding n
// stored user events (no ClaudeSessionID, so the replay is stored-only and
// deterministic).
func replayTestServer(t *testing.T, n int) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		DataDir:   dir,
		CLIs:      []config.CLI{{Name: "claude", Kind: config.KindClaude, Path: filepath.Join(dir, "no-such-claude")}},
		Providers: []config.Provider{{Name: "gw", Kind: config.KindClaude, Models: []config.Model{{Name: "m1"}}}},
	}
	cm := config.NewManager(filepath.Join(dir, "config.yaml"), cfg)
	st, err := store.Open(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	s := &Server{cm: cm, st: st, mgr: runner.NewManager(cm, st), replayCache: map[string]*replayCacheEntry{}}
	sess := &store.Session{ID: "s1", Name: "s", CLI: "claude", Provider: "gw", Cwd: dir}
	if err := st.Upsert(sess, nil); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	for i := 0; i < n; i++ {
		if _, err := st.AppendEvent("s1", "user", map[string]string{"text": fmt.Sprintf("msg %d", i)}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	return s, "s1"
}

func TestReplayWindowStart(t *testing.T) {
	mk := func(kinds ...string) []*store.Event {
		evs := make([]*store.Event, len(kinds))
		for i, k := range kinds {
			var data string
			if k == "user" {
				data = `{"text":"hi"}`
			} else {
				data = `{"type":"assistant"}`
			}
			evs[i] = &store.Event{Kind: k, Data: json.RawMessage(data)}
		}
		return evs
	}
	cases := []struct {
		name  string
		kinds []string
		n     int
		want  int
	}{
		{"shorter than window", []string{"user", "stream"}, 50, 0},
		// len 81, start 31: last user at 70 is inside the window → keep start.
		{"last user inside window keeps start", append(repeat("stream", 70), append([]string{"user"}, repeat("stream", 10)...)...), 50, 31},
		// len 81, start 31: last user at 5 is older than the window → extend back.
		{"extends back to last user", append(repeat("stream", 5), append([]string{"user"}, repeat("stream", 75)...)...), 50, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := replayWindowStart(mk(tc.kinds...), tc.n); got != tc.want {
				t.Fatalf("replayWindowStart = %d, want %d", got, tc.want)
			}
		})
	}
}

func repeat(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}

func TestHandleSessionHistoryPages(t *testing.T) {
	s, id := replayTestServer(t, 120)

	get := func(query string) ([]*store.Event, int) {
		req := httptest.NewRequest(http.MethodGet, "/api/sessions/"+id+"/history"+query, nil)
		req.SetPathValue("id", id)
		w := httptest.NewRecorder()
		s.handleSessionHistory(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		var resp struct {
			Events []*store.Event `json:"events"`
			From   int            `json:"from"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return resp.Events, resp.From
	}

	evs, from := get("?before=100&limit=50")
	if from != 50 || len(evs) != 50 {
		t.Fatalf("before=100 limit=50: from=%d len=%d, want 50/50", from, len(evs))
	}
	var d struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(evs[0].Data, &d)
	if d.Text != "msg 50" {
		t.Fatalf("first event = %q, want msg 50", d.Text)
	}

	evs, from = get("?before=30&limit=50")
	if from != 0 || len(evs) != 30 {
		t.Fatalf("before=30: from=%d len=%d, want 0/30", from, len(evs))
	}

	evs, from = get("?before=0&limit=50")
	if from != 0 || len(evs) != 0 {
		t.Fatalf("before=0: from=%d len=%d, want 0/0", from, len(evs))
	}
}

// handleEventsStream runs the SSE handler against a pre-cancelled context so it
// replays once and returns immediately (the live loop sees Done at once).
func handleEventsStream(t *testing.T, s *Server, id string, gz bool) *httptest.ResponseRecorder {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/sessions/"+id+"/events", nil).WithContext(ctx)
	req.SetPathValue("id", id)
	if gz {
		req.Header.Set("Accept-Encoding", "gzip")
	}
	w := httptest.NewRecorder()
	s.handleEvents(w, req)
	return w
}

func TestHandleEventsSendsOnlyTailWindow(t *testing.T) {
	s, id := replayTestServer(t, 120)

	w := handleEventsStream(t, s, id, false)
	if enc := w.Header().Get("Content-Encoding"); enc != "" {
		t.Fatalf("unexpected Content-Encoding %q without Accept-Encoding", enc)
	}
	frames := sseFrames(t, w.Body.Bytes())
	// 50 window events + replay_end.
	if len(frames) != replayWindow+1 {
		t.Fatalf("frames = %d, want %d", len(frames), replayWindow+1)
	}
	end := frames[len(frames)-1]
	var meta struct {
		Kind string `json:"kind"`
		Data struct {
			Start int `json:"start"`
			Total int `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(end, &meta); err != nil {
		t.Fatalf("decode replay_end: %v", err)
	}
	if meta.Kind != "replay_end" || meta.Data.Start != 70 || meta.Data.Total != 120 {
		t.Fatalf("replay_end = %+v, want kind=replay_end start=70 total=120", meta)
	}
}

func TestHandleEventsGzipsWhenAccepted(t *testing.T) {
	s, id := replayTestServer(t, 120)

	w := handleEventsStream(t, s, id, true)
	if enc := w.Header().Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", enc)
	}
	gz, err := gzip.NewReader(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	plain, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	if n := len(sseFrames(t, plain)); n != replayWindow+1 {
		t.Fatalf("decompressed frames = %d, want %d", n, replayWindow+1)
	}
}

// sseFrames splits an SSE body into the JSON payload of each `data:` frame.
func sseFrames(t *testing.T, body []byte) [][]byte {
	t.Helper()
	var out [][]byte
	for _, chunk := range strings.Split(string(body), "\n\n") {
		chunk = strings.TrimSpace(chunk)
		if chunk == "" || strings.HasPrefix(chunk, ":") {
			continue
		}
		if !strings.HasPrefix(chunk, "data: ") {
			t.Fatalf("unexpected SSE chunk: %q", chunk)
		}
		out = append(out, []byte(strings.TrimPrefix(chunk, "data: ")))
	}
	return out
}

func TestCachedReplayFingerprintAndLRU(t *testing.T) {
	s := &Server{replayCache: map[string]*replayCacheEntry{}}
	builds := 0
	build := func() ([]*store.Event, []string) {
		builds++
		return []*store.Event{{Kind: "stream"}}, []string{"fp"}
	}

	s.cachedReplay("a", "fp1", build)
	s.cachedReplay("a", "fp1", build) // hit: no rebuild
	if builds != 1 {
		t.Fatalf("builds = %d after a hit, want 1", builds)
	}
	s.cachedReplay("a", "fp2", build) // fingerprint changed: rebuild
	if builds != 2 {
		t.Fatalf("builds = %d after fp change, want 2", builds)
	}
	for i := 0; i < replayCacheMax+2; i++ {
		s.cachedReplay(fmt.Sprintf("s%d", i), "fp", build)
	}
	s.replayMu.Lock()
	n := len(s.replayCache)
	s.replayMu.Unlock()
	if n > replayCacheMax {
		t.Fatalf("cache holds %d entries, want <= %d", n, replayCacheMax)
	}
}

func TestComputeReplayStats(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ev := func(kind string, offsetMs int, data string) *store.Event {
		return &store.Event{Kind: kind, Ts: base.Add(time.Duration(offsetMs) * time.Millisecond), Data: json.RawMessage(data)}
	}
	events := []*store.Event{
		ev("user", 0, `{"text":"hi"}`),
		ev("stream", 1000, `{"type":"assistant","message":{"content":[{"type":"text"},{"type":"tool_use"}]}}`),
		ev("stream", 3000, `{"type":"user","message":{"content":[{"type":"tool_result"}]}}`),
		ev("stream", 6000, `{"type":"assistant","message":{"content":[{"type":"tool_use"}]}}`),
		ev("stream", 9000, `{"type":"result","duration_ms":5000,"duration_api_ms":2000}`),
	}
	st := computeReplayStats(events)
	if st.Turns != 1 {
		t.Fatalf("turns = %d, want 1", st.Turns)
	}
	if st.Steps != 2 {
		t.Fatalf("steps = %d, want 2", st.Steps)
	}
	if st.FirstTs != base.UnixMilli() || st.LastTs != base.Add(9*time.Second).UnixMilli() {
		t.Fatalf("ts range = %d..%d", st.FirstTs, st.LastTs)
	}
	if st.LLMMs != 2000 || st.ToolMs != 3000 {
		t.Fatalf("llm=%d tool=%d, want 2000/3000", st.LLMMs, st.ToolMs)
	}
	// gaps: 1000 (assistant) + 2000 (user) + 3000 (assistant); the result event
	// contributes no estimate.
	if st.LLMEstMs != 4000 || st.ToolEstMs != 2000 {
		t.Fatalf("est llm=%d tool=%d, want 4000/2000", st.LLMEstMs, st.ToolEstMs)
	}
}
