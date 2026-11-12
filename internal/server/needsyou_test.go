package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cc-webui/internal/boarddb"
	"cc-webui/internal/config"
	"cc-webui/internal/runner"
	"cc-webui/internal/store"
)

type needsYouResp struct {
	Items      []needsYouItem `json:"items"`
	Counts     map[string]int `json:"counts"`
	Board      map[string]any `json:"board"`
	ServerTime int64          `json:"server_time"`
	WindowMs   int64          `json:"window_ms"`
}

func getNeedsYou(t *testing.T, s *Server) needsYouResp {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleNeedsYou(w, httptest.NewRequest(http.MethodGet, "/api/needs-you", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	var out needsYouResp
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	return out
}

func ids(items []needsYouItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.ID
	}
	return out
}

// 空实例：items 必须是 [] 不是 null（前端无条件索引），counts 全 0，看板可用。
func TestNeedsYouEmpty(t *testing.T) {
	s, _ := boardMCPServer(t)
	w := httptest.NewRecorder()
	s.handleNeedsYou(w, httptest.NewRequest(http.MethodGet, "/api/needs-you", nil))
	if !strings.Contains(w.Body.String(), `"items":[]`) {
		t.Fatalf("empty queue must marshal items as []: %s", w.Body.String())
	}
	out := getNeedsYou(t, s)
	if out.Counts["all"] != 0 || out.Board["ok"] != true {
		t.Fatalf("empty = %+v", out)
	}
	if out.ServerTime == 0 || out.WindowMs != runner.PermissionTimeout.Milliseconds() {
		t.Fatalf("server_time/window_ms missing: %+v", out)
	}
}

// 档间 perm > ask > blocked > review；perm/ask 档内按剩余 TTL 升序；
// 看板卡按进入当前状态的时间升序（等得最久的在前）。
func TestNeedsYouOrderingAndBinding(t *testing.T) {
	s, st := boardMCPServer(t)
	for _, sess := range []*store.Session{
		{ID: "w2", Name: "w·登录改版", PeerName: "w-login", Cwd: t.TempDir(), CLI: "claude", Provider: "gw"},
		{ID: "arch", Name: "归档会话", Cwd: t.TempDir(), CLI: "claude", Provider: "gw", Archived: true},
		{ID: "shadow", Name: "群影子", Cwd: t.TempDir(), CLI: "claude", Provider: "gw", GroupID: "g1"},
	} {
		if err := st.Upsert(sess, nil); err != nil {
			t.Fatal(err)
		}
	}
	// 注册表里按创建时间排：ask(w2) → perm(worker) → perm(w2)；创建间隔拉开，避免同毫秒。
	askInput := map[string]any{"questions": []any{map[string]any{
		"header": "口径", "question": "退款失败：原路退回还是退到余额？",
		"options": []any{map[string]any{"label": "原路退回"}, map[string]any{"label": "退到余额"}},
	}}}
	s.mgr.PermissionRequest("w2", "ask1", askToolName, askInput, "tu-ask")
	time.Sleep(8 * time.Millisecond)
	s.mgr.PermissionRequest("worker", "perm1", "Bash", map[string]any{"command": "git push --force-with-lease origin x\nsecond line"}, "tu-1")
	time.Sleep(8 * time.Millisecond)
	s.mgr.PermissionRequest("w2", "perm2", "Edit", map[string]any{"file_path": "/tmp/a.go"}, "tu-2")
	// 不该出现的：归档会话、群影子成员、未知会话的待决 prompt。
	s.mgr.PermissionRequest("arch", "permA", "Bash", map[string]any{"command": "ls"}, "")
	s.mgr.PermissionRequest("shadow", "permS", "Bash", map[string]any{"command": "ls"}, "")
	s.mgr.PermissionRequest("ghost", "permG", "Bash", map[string]any{"command": "ls"}, "")

	// 看板：worker 绑 T-blk（待人工），w2 绑 T-rev（待验收）；T-run 进行中、T-done 已完成不入队；
	// T-arch 待人工但已归档，不入队。后三张不绑会话，免得 TaskForSession 同秒并列取值不确定。
	mk := func(id, title, status, session string) {
		t.Helper()
		if _, err := s.boardDB.CreateTask(map[string]any{
			"id": id, "title": title, "status": status, "session": session, "worker": session,
		}); err != nil {
			t.Fatal(err)
		}
	}
	mk("T-blk", "订单库分表迁移", "待人工", "worker")
	mk("T-rev", "登录页改版", "待验收", "w2")
	mk("T-run", "进行中的卡", "进行中", "")
	mk("T-done", "已完成的卡", "已完成", "")
	mk("T-arch", "归档的卡", "待人工", "")
	if _, err := s.boardDB.UpdateTask("T-arch", map[string]any{"archived": true}); err != nil {
		t.Fatal(err)
	}
	// T-blk 比 T-rev 早进入各自状态：显式写「状态」事件，at 差 1 小时。
	old := time.Now().Add(-90 * time.Minute).Format("2006-01-02 15:04:05")
	recent := time.Now().Add(-10 * time.Minute).Format("2006-01-02 15:04:05")
	if _, err := s.boardDB.AppendEvent("T-blk", "状态", "进行中 → 待人工", old, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.boardDB.AppendEvent("T-rev", "状态", "进行中 → 待验收", recent, "test"); err != nil {
		t.Fatal(err)
	}

	out := getNeedsYou(t, s)
	want := []string{"perm:perm1", "perm:perm2", "ask:ask1", "blocked:T-blk", "review:T-rev"}
	if got := ids(out.Items); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", got, want)
	}
	if out.Counts["all"] != 5 || out.Counts["perm"] != 2 || out.Counts["ask"] != 1 ||
		out.Counts["blocked"] != 1 || out.Counts["review"] != 1 {
		t.Fatalf("counts = %v", out.Counts)
	}

	byID := map[string]needsYouItem{}
	for _, it := range out.Items {
		byID[it.ID] = it
	}
	p1 := byID["perm:perm1"]
	if p1.State != "live" || p1.PendingID != "perm1" || p1.SessionID != "worker" || p1.Tool != "Bash" {
		t.Fatalf("perm1 = %+v", p1)
	}
	if p1.ExpiresAt-p1.CreatedAt != runner.PermissionTimeout.Milliseconds() {
		t.Fatalf("perm1 window = %d", p1.ExpiresAt-p1.CreatedAt)
	}
	if p1.Title != "Bash：git push --force-with-lease origin x" {
		t.Fatalf("perm title = %q", p1.Title)
	}
	// 待决 prompt 借 TaskForSession 挂到绑定的卡上。
	if p1.TaskID != "T-blk" || p1.TaskStatus != "待人工" {
		t.Fatalf("perm1 should bind to T-blk: %+v", p1)
	}
	a1 := byID["ask:ask1"]
	if a1.Title != "退款失败：原路退回还是退到余额？" || a1.PeerName != "w-login" || a1.TaskID != "T-rev" {
		t.Fatalf("ask1 = %+v", a1)
	}
	if qs, ok := a1.Input["questions"].([]any); !ok || len(qs) != 1 {
		t.Fatalf("ask input must carry questions: %+v", a1.Input)
	}
	blk := byID["blocked:T-blk"]
	if blk.State != "open" || blk.SessionID != "worker" || blk.PendingID != "" || blk.ExpiresAt != 0 {
		t.Fatalf("blocked = %+v", blk)
	}
	wantAt, _ := time.ParseInLocation("2006-01-02 15:04:05", old, time.Local)
	if blk.CreatedAt != wantAt.UnixMilli() {
		t.Fatalf("blocked created_at = %d, want %d (the 状态 event time)", blk.CreatedAt, wantAt.UnixMilli())
	}
}

// 超长字符串在 polled 响应里被截断，避免一个 Write 工具把整页队列撑爆。
func TestNeedsYouClipsHugeInput(t *testing.T) {
	s, _ := boardMCPServer(t)
	huge := strings.Repeat("字", needsInputStrMax*3)
	s.mgr.PermissionRequest("worker", "pw", "Write", map[string]any{"file_path": "/tmp/x", "content": huge}, "")
	out := getNeedsYou(t, s)
	if len(out.Items) != 1 {
		t.Fatalf("items = %+v", out.Items)
	}
	content, _ := out.Items[0].Input["content"].(string)
	if n := len([]rune(content)); n > needsInputStrMax+1 || n == 0 {
		t.Fatalf("content not clipped: %d runes", n)
	}
	if out.Items[0].Input["file_path"] != "/tmp/x" {
		t.Fatalf("short fields must survive: %+v", out.Items[0].Input)
	}
}

// 看板库不可用时队列退化成只有 live prompt，并明确告诉前端看板这一路挂了。
func TestNeedsYouBoardUnavailable(t *testing.T) {
	dir := t.TempDir()
	cm := config.NewManager(filepath.Join(dir, "config.yaml"), &config.Config{DataDir: dir})
	st, err := store.Open(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{cm: cm, st: st, mgr: runner.NewManager(cm, st), boardHub: newBoardHub()}
	if err := st.Upsert(&store.Session{ID: "w", Name: "w", Cwd: dir, CLI: "claude", Provider: "gw"}, nil); err != nil {
		t.Fatal(err)
	}
	s.mgr.PermissionRequest("w", "p1", "Bash", map[string]any{"command": "ls"}, "")
	out := getNeedsYou(t, s)
	if out.Board["ok"] != false || out.Board["error"] == "" {
		t.Fatalf("board = %+v", out.Board)
	}
	if len(out.Items) != 1 || out.Items[0].Kind != "perm" || out.Items[0].TaskID != "" {
		t.Fatalf("items = %+v", out.Items)
	}
}

// AttentionTasks 只返回指定状态、未归档的卡，Since 取最近一次「→ 当前状态」事件。
func TestBoardAttentionTasks(t *testing.T) {
	dir := t.TempDir()
	b, err := boarddb.Open(filepath.Join(dir, "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	for _, in := range []map[string]any{
		{"id": "A", "title": "a", "status": "待人工"},
		{"id": "B", "title": "b", "status": "待验收", "session": "s1", "worker": "w1"},
		{"id": "C", "title": "c", "status": "进行中"},
	} {
		if _, err := b.CreateTask(in); err != nil {
			t.Fatal(err)
		}
	}
	// 同一张卡两次进入「待验收」：Since 取较新的一次。
	_, _ = b.AppendEvent("B", "状态", "进行中 → 待验收", "2026-01-01 10:00:00", "t")
	_, _ = b.AppendEvent("B", "状态", "进行中 → 待验收", "2026-01-02 10:00:00", "t")
	_, _ = b.AppendEvent("B", "状态", "待验收 → 进行中", "2026-01-03 10:00:00", "t")
	got, err := b.AttentionTasks("待人工", "待验收")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	by := map[string]boarddb.AttentionTask{}
	for _, tk := range got {
		by[tk.ID] = tk
	}
	if by["B"].Since != "2026-01-02 10:00:00" || by["B"].Session != "s1" || by["B"].Worker != "w1" {
		t.Fatalf("B = %+v", by["B"])
	}
	if by["A"].Since != "" {
		t.Fatalf("A never changed status; Since must be empty: %+v", by["A"])
	}
	if none, _ := b.AttentionTasks(); len(none) != 0 {
		t.Fatalf("no statuses → no rows, got %+v", none)
	}
}
