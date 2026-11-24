package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"cc-webui/internal/schedule"
)

type aiParseResp struct {
	Mode               string                  `json:"mode"`
	Fallback           bool                    `json:"fallback"`
	Reason             string                  `json:"reason"`
	Candidates         []scheduleCandidateView `json:"candidates"`
	Proposal           *scheduleAIProposalView `json:"proposal"`
	NeedsClarification bool                    `json:"needs_clarification"`
	ContextReads       []string                `json:"context_reads"`
	ElapsedMS          int64                   `json:"elapsed_ms"`
	Usage              *scheduleAIUsage        `json:"usage"`
}

func callAIParse(t *testing.T, s *Server, text string) (*aiParseResp, int) {
	t.Helper()
	w := scheduleCall(t, s, http.MethodPost, "/api/schedules/parse-ai", "", map[string]any{"text": text}, s.handleParseScheduleAI)
	var out aiParseResp
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v (%s)", err, w.Body.String())
		}
	}
	return &out, w.Code
}

func TestParseScheduleAIProposal(t *testing.T) {
	s, _ := scheduleServer(t)
	before := len(s.scheduleStore().List())
	s.schedAgentRun = func(ctx context.Context, text string) (scheduleAIResult, error) {
		return scheduleAIResult{
			Output:      `{"name":"每日看板巡检","cron":"0 9 * * *","prompt":"按 work/tracking 的周报口径做巡检","questions":[],"rationale":"描述写的是每天9点"}`,
			InputTokens: 1234, OutputTokens: 88,
			ContextReads: []string{"Read /ws/work/tracking/README.md"},
		}, nil
	}
	out, code := callAIParse(t, s, "每天9点按上周周报口径做看板巡检")
	if code != http.StatusOK {
		t.Fatalf("code = %d", code)
	}
	if out.Mode != "ai" || out.Fallback || out.Proposal == nil {
		t.Fatalf("response = %+v", out)
	}
	p := out.Proposal
	if p.Name != "每日看板巡检" || p.Cron != "0 9 * * *" || p.Prompt == "" ||
		p.Rationale == "" || p.NextRunLocal == "" {
		t.Fatalf("proposal = %+v", p)
	}
	if len(out.ContextReads) != 1 || out.ContextReads[0] == "" {
		t.Fatalf("context reads = %+v", out.ContextReads)
	}
	if out.Usage == nil || out.Usage.InputTokens != 1234 || out.Usage.OutputTokens != 88 {
		t.Fatalf("usage = %+v", out.Usage)
	}
	if out.ElapsedMS < 0 {
		t.Fatalf("elapsed = %d", out.ElapsedMS)
	}
	// AI parsing only proposes; the user must confirm before a task is created.
	if got := len(s.scheduleStore().List()); got != before {
		t.Fatalf("AI parse created a schedule: %d -> %d", before, got)
	}
}

func TestParseScheduleAIFallbacks(t *testing.T) {
	cases := []struct {
		name   string
		text   string
		result scheduleAIResult
		err    error
		want   string
	}{
		{"non-json", "每天9点巡检", scheduleAIResult{Output: "抱歉，我无法解析"}, nil, "JSON"},
		{"invalid-cron", "每天9点巡检", scheduleAIResult{Output: `{"name":"x","cron":"61 * * * *","prompt":"p"}`}, nil, "cron"},
		{"agent-error", "每天9点巡检", scheduleAIResult{}, context.Canceled, "AI 解析失败"},
		{"unparseable-rules", "帮我弄个定时任务", scheduleAIResult{}, context.Canceled, "规则解析也失败"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := scheduleServer(t)
			s.schedAgentRun = func(ctx context.Context, text string) (scheduleAIResult, error) {
				return tc.result, tc.err
			}
			out, code := callAIParse(t, s, tc.text)
			if code != http.StatusOK {
				t.Fatalf("code = %d", code)
			}
			if !out.Fallback || !strings.Contains(out.Reason, tc.want) {
				t.Fatalf("fallback = %+v, want reason containing %q", out, tc.want)
			}
			if tc.name == "unparseable-rules" {
				if out.Mode != "none" || len(out.Candidates) != 0 {
					t.Fatalf("unparseable fallback = %+v", out)
				}
			} else if out.Mode != "rules" || len(out.Candidates) == 0 || out.Candidates[0].Cron == "" {
				t.Fatalf("rule fallback = %+v", out)
			}
		})
	}
}

func TestParseScheduleAITimeout(t *testing.T) {
	s, _ := scheduleServer(t)
	s.schedAITimeout = 60 * time.Millisecond
	s.schedAgentRun = func(ctx context.Context, text string) (scheduleAIResult, error) {
		<-ctx.Done()
		return scheduleAIResult{}, ctx.Err()
	}
	out, code := callAIParse(t, s, "每天9点巡检")
	if code != http.StatusOK || !out.Fallback || !strings.Contains(out.Reason, "超时") {
		t.Fatalf("timeout response = code %d, %+v", code, out)
	}
	if len(out.Candidates) == 0 {
		t.Fatalf("timeout should fall back to rules: %+v", out)
	}
}

func TestParseScheduleAIConcurrencyLimit(t *testing.T) {
	s, _ := scheduleServer(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	s.schedAgentRun = func(ctx context.Context, text string) (scheduleAIResult, error) {
		once.Do(func() { close(entered) })
		<-release
		return scheduleAIResult{Output: `{"name":"x","cron":"0 9 * * *","prompt":"p"}`}, nil
	}
	first := make(chan *aiParseResp, 1)
	go func() {
		out, _ := callAIParse(t, s, "每天9点巡检")
		first <- out
	}()
	<-entered
	out, code := callAIParse(t, s, "每天10点巡检")
	if code != http.StatusConflict {
		t.Fatalf("second concurrent call code = %d, want 409 (%+v)", code, out)
	}
	close(release)
	if got := <-first; got.Mode != "ai" {
		t.Fatalf("first call = %+v", got)
	}
	// The slot is released after the first call finishes.
	s.schedAgentRun = func(ctx context.Context, text string) (scheduleAIResult, error) {
		return scheduleAIResult{Output: `{"name":"y","cron":"0 10 * * *","prompt":"p"}`}, nil
	}
	if out, code := callAIParse(t, s, "每天10点巡检"); code != http.StatusOK || out.Mode != "ai" {
		t.Fatalf("after release: code=%d %+v", code, out)
	}
}

func TestParseScheduleAIClarification(t *testing.T) {
	s, _ := scheduleServer(t)
	s.schedAgentRun = func(ctx context.Context, text string) (scheduleAIResult, error) {
		return scheduleAIResult{Output: `{"name":"周报巡检","cron":"","prompt":"","questions":["按哪一周的周报口径？","需要检查哪些看板项目？"],"rationale":"口径未定"}`}, nil
	}
	out, code := callAIParse(t, s, "每周按上周周报口径巡检")
	if code != http.StatusOK || !out.NeedsClarification || out.Proposal == nil {
		t.Fatalf("clarification = code %d, %+v", code, out)
	}
	if len(out.Proposal.Questions) != 2 || out.Proposal.Cron != "" {
		t.Fatalf("proposal = %+v", out.Proposal)
	}
	if len(s.scheduleStore().List()) != 0 {
		t.Fatal("clarification must not create a schedule")
	}
}

// The real temp-session path cleans up the shadow session even when the CLI
// cannot start. The nonexistent claude path in createResidentServer makes the
// turn fail fast; the short context bounds the wait.
func TestRunScheduleAIAgentCleansTempSession(t *testing.T) {
	s, st := scheduleServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := s.runScheduleAIAgent(ctx, "每天9点巡检")
	if err == nil {
		t.Fatal("expected failure with a nonexistent CLI")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("cleanup took too long: %v", time.Since(start))
	}
	for _, sess := range st.List() {
		if sess.GroupID == scheduleAITempGroup {
			t.Fatalf("temp parse session not cleaned up: %+v", sess)
		}
	}
}

func TestParseAIProposalShapes(t *testing.T) {
	p, err := parseAIProposal("```json\n{\"name\":\"x\",\"cron\":\"0 9 * * *\",\"prompt\":\"p\",\"questions\":[],\"rationale\":\"r\"}\n```")
	if err != nil || p.Cron != "0 9 * * *" {
		t.Fatalf("fenced JSON = %+v, %v", p, err)
	}
	// Several objects: use the last valid one instead of failing on braces.
	multi := "说明 {\"name\":\"bad\",\"cron\":\"nope\",\"prompt\":\"p\"}\n最终：\n" +
		`{"name":"good","cron":"30 8 * * *","prompt":"p2","questions":[],"rationale":"r2"}`
	p, err = parseAIProposal(multi)
	if err != nil || p.Cron != "30 8 * * *" || p.Name != "good" {
		t.Fatalf("multi-object = %+v, %v", p, err)
	}
	if _, err := parseAIProposal("no json here"); err == nil {
		t.Fatal("missing JSON should error")
	}
	if _, err := parseAIProposal(`{"name":"x","cron":"","prompt":"p"}`); err == nil {
		t.Fatal("empty cron without questions should error")
	}
	if _, err := schedule.ParseCron("0 9 * * *"); err != nil {
		t.Fatal(err)
	}
}
