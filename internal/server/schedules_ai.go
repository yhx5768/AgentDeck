package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"cc-webui/internal/claudehist"
	"cc-webui/internal/config"
	"cc-webui/internal/runner"
	"cc-webui/internal/schedule"
	"cc-webui/internal/store"
)

// scheduleAIParseTimeout holds one one-shot parse turn. A cold CLI plus a few
// tracking reads can take a minute on a large workspace; the UI warns that AI
// parsing is slower than rules and offers a rules fallback.
const scheduleAIParseTimeout = 120 * time.Second

// scheduleAITempGroup marks the temp parse session as a shadow session so it
// never appears in the sidebar, needs-you queue or statistics.
const scheduleAITempGroup = "__schedule_parse__"

// scheduleAIParseRolePrompt is the temp session's system prompt. The model may
// read memory/tracking, but must not write or execute state-changing commands,
// and must answer with one JSON object.
const scheduleAIParseRolePrompt = `你是 AgentDeck 的定时任务解析器（一次性只读任务）。
规则：
1. 只读。不要写文件、不要修改仓库、不要执行会改变状态的命令、不要调用外部 IM/网络服务。
2. 可以读取工作区内与描述相关的记忆/台账（例如 work/tracking/、.claude/、项目文档），把用户提到的口径落到最终 prompt 里。
3. 最终只输出一个 JSON 对象，且从第一个字符开始就是左花括号；不要 markdown 代码块、不要前言、不要分析过程、不要输出多个对象。
JSON schema:
{"name":"任务名","cron":"分 时 日 月 周","prompt":"到点后真正发送的完整消息","questions":["需要用户确认的问题"],"rationale":"为什么这样排期/选用这个口径"}
约束：
- cron 必须是 5 段，按当前本机时区解释。
- 频率或时间不明确、或口径需要用户确认时，cron 留空字符串，并在 questions 里列出最多 3 个具体问题；不要猜。
- prompt 必须自包含，读到的台账/记忆要点要写进去。
- 上下文预算：用户消息已附带服务端预读的台账摘要。默认不要调用任何工具；只有摘要明显缺少关键口径时，最多调用 1 次 Read/Grep。不要用 Bash、不要遍历目录、不要全仓搜索。
- 找不到口径就按描述做合理默认，并在 rationale 里说明。
- name 不超过 40 个字符。`

type scheduleAIProposal struct {
	Name      string   `json:"name"`
	Cron      string   `json:"cron"`
	Prompt    string   `json:"prompt"`
	Questions []string `json:"questions"`
	Rationale string   `json:"rationale"`
}

type scheduleAIUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// scheduleAIResult is what the one-shot runner returns to the handler. The
// context/token counters are best-effort metadata for the UI.
type scheduleAIResult struct {
	Output       string
	InputTokens  int
	OutputTokens int
	ContextReads []string
}

type scheduleAIProposalView struct {
	Name         string   `json:"name"`
	Cron         string   `json:"cron,omitempty"`
	Prompt       string   `json:"prompt,omitempty"`
	Questions    []string `json:"questions,omitempty"`
	Rationale    string   `json:"rationale,omitempty"`
	NextRunAt    string   `json:"next_run_at,omitempty"`
	NextRunLocal string   `json:"next_run_local,omitempty"`
}

type scheduleAIResponse struct {
	Mode               string                  `json:"mode"` // ai | rules | none
	Fallback           bool                    `json:"fallback"`
	Reason             string                  `json:"reason,omitempty"`
	Candidates         []scheduleCandidateView `json:"candidates,omitempty"`
	Proposal           *scheduleAIProposalView `json:"proposal,omitempty"`
	NeedsClarification bool                    `json:"needs_clarification,omitempty"`
	ContextReads       []string                `json:"context_reads,omitempty"`
	ElapsedMS          int64                   `json:"elapsed_ms"`
	Usage              *scheduleAIUsage        `json:"usage,omitempty"`
	Timezone           string                  `json:"timezone"`
}

func (s *Server) beginScheduleAI() bool {
	s.schedAI.mu.Lock()
	defer s.schedAI.mu.Unlock()
	if s.schedAI.busy {
		return false
	}
	s.schedAI.busy = true
	return true
}

func (s *Server) endScheduleAI() {
	s.schedAI.mu.Lock()
	s.schedAI.busy = false
	s.schedAI.mu.Unlock()
}

// handleParseScheduleAI runs one one-shot agent turn in the mainAgent workspace
// to interpret a description that may reference memory/tracking. It never
// creates a schedule: the response is a proposal the user must confirm in the
// dialog. Any failure falls back to the deterministic rule parser.
func (s *Server) handleParseScheduleAI(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeErr(w, http.StatusBadRequest, "描述不能为空")
		return
	}
	if !s.beginScheduleAI() {
		writeErr(w, http.StatusConflict, "已有一个 AI 解析任务在运行，请稍候再试")
		return
	}
	defer s.endScheduleAI()

	timeout := s.schedAITimeout
	if timeout <= 0 {
		timeout = scheduleAIParseTimeout
		// Escape hatch for slow launcher CLIs / large workspaces.
		if v := strings.TrimSpace(os.Getenv("CC_WEBUI_SCHEDULE_AI_TIMEOUT")); v != "" {
			if d, err := time.ParseDuration(v); err == nil && d > 0 {
				timeout = d
			}
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	start := time.Now()
	result, err := s.runScheduleAIParse(ctx, text)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		reason := err.Error()
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			reason = fmt.Sprintf("AI 解析超时（%s）", timeout)
		}
		s.writeScheduleAIFallback(w, text, "AI 解析失败："+reason, elapsed)
		return
	}
	proposal, err := parseAIProposal(result.Output)
	if err != nil {
		s.writeScheduleAIFallback(w, text, "AI 输出无法使用："+err.Error(), elapsed)
		return
	}
	resp := scheduleAIResponse{
		Mode:         "ai",
		ContextReads: result.ContextReads,
		ElapsedMS:    elapsed,
		Timezone:     localZoneName(),
	}
	if result.InputTokens+result.OutputTokens > 0 {
		resp.Usage = &scheduleAIUsage{InputTokens: result.InputTokens, OutputTokens: result.OutputTokens}
	}
	if strings.TrimSpace(proposal.Cron) == "" {
		resp.NeedsClarification = true
		resp.Proposal = &scheduleAIProposalView{
			Name: proposal.Name, Prompt: proposal.Prompt, Rationale: proposal.Rationale,
			Questions: proposal.Questions,
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	cron, err := schedule.ParseCron(proposal.Cron)
	if err != nil {
		s.writeScheduleAIFallback(w, text, "AI 给出的 cron 无效："+err.Error(), elapsed)
		return
	}
	next, ok := cron.Next(time.Now())
	if !ok {
		s.writeScheduleAIFallback(w, text, "AI 给出的 cron 永不触发", elapsed)
		return
	}
	name := strings.TrimSpace(proposal.Name)
	if name == "" {
		name = firstLineText(proposal.Prompt)
	}
	if name == "" {
		name = "定时任务"
	}
	if len([]rune(name)) > maxScheduleNameRunes {
		name = string([]rune(name)[:maxScheduleNameRunes])
	}
	prompt := strings.TrimSpace(proposal.Prompt)
	if prompt == "" {
		prompt = text
	}
	if len(prompt) > maxSchedulePromptByte {
		s.writeScheduleAIFallback(w, text, "AI 给出的 prompt 过长", elapsed)
		return
	}
	resp.Proposal = &scheduleAIProposalView{
		Name: name, Cron: cron.Expr(), Prompt: prompt,
		Questions: proposal.Questions, Rationale: strings.TrimSpace(proposal.Rationale),
		NextRunAt: next.Format(time.RFC3339), NextRunLocal: next.Format("2006-01-02 15:04"),
	}
	writeJSON(w, http.StatusOK, resp)
}

func firstLineText(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			r := []rune(line)
			if len(r) > 40 {
				r = r[:40]
			}
			return string(r)
		}
	}
	return ""
}

func (s *Server) writeScheduleAIFallback(w http.ResponseWriter, text, reason string, elapsed int64) {
	cands, err := scheduleRuleCandidates(text)
	resp := scheduleAIResponse{
		Mode: "rules", Fallback: true, Reason: reason,
		ElapsedMS: elapsed, Timezone: localZoneName(),
	}
	if err != nil {
		resp.Mode = "none"
		resp.Reason = reason + "；规则解析也失败：" + err.Error()
		resp.Candidates = []scheduleCandidateView{}
	} else {
		resp.Candidates = cands
	}
	writeJSON(w, http.StatusOK, resp)
}

// scheduleAIContextDigest pre-reads a small, plausible slice of the workspace's
// memory/tracking so the one-shot agent does not have to explore a large repo
// (that exploration is what made the first real run take minutes). It reads
// work/tracking/README.md, the 3 newest tracking files and the 2 newest
// weekly-report files, capped at 3 KB each. It is read-only and best-effort.
func scheduleAIContextDigest(workspace string) (string, []string) {
	type entry struct {
		path string
		mod  time.Time
	}
	seen := map[string]bool{}
	var entries []entry
	add := func(p string) {
		p = filepath.Clean(p)
		if seen[p] {
			return
		}
		info, err := os.Stat(p)
		if err != nil || info.IsDir() || info.Size() > 2<<20 {
			return
		}
		seen[p] = true
		entries = append(entries, entry{p, info.ModTime()})
	}
	tracking := filepath.Join(workspace, "work", "tracking")
	add(filepath.Join(tracking, "README.md"))
	var trackingFiles []entry
	_ = filepath.WalkDir(tracking, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d == nil {
			return nil
		}
		if d.IsDir() {
			rel, _ := filepath.Rel(tracking, p)
			if rel != "." && strings.Count(rel, string(filepath.Separator)) >= 2 {
				return fs.SkipDir
			}
			return nil
		}
		if len(trackingFiles) >= 200 {
			return fs.SkipAll
		}
		if info, err := d.Info(); err == nil && info.Size() <= 2<<20 {
			trackingFiles = append(trackingFiles, entry{p, info.ModTime()})
		}
		return nil
	})
	sort.Slice(trackingFiles, func(i, j int) bool { return trackingFiles[i].mod.After(trackingFiles[j].mod) })
	for i := 0; i < len(trackingFiles) && i < 3; i++ {
		add(trackingFiles[i].path)
	}
	weekly, _ := filepath.Glob(filepath.Join(workspace, "work", "projects", "*", "weekly-report-*.md"))
	sort.Slice(weekly, func(i, j int) bool {
		ai, _ := os.Stat(weekly[i])
		aj, _ := os.Stat(weekly[j])
		if ai == nil || aj == nil {
			return weekly[i] > weekly[j]
		}
		return ai.ModTime().After(aj.ModTime())
	})
	for i := 0; i < len(weekly) && i < 2; i++ {
		add(weekly[i])
	}
	if len(entries) == 0 {
		return "", nil
	}
	// README first, then newest-first.
	sort.SliceStable(entries, func(i, j int) bool {
		if filepath.Base(entries[i].path) == "README.md" {
			return true
		}
		if filepath.Base(entries[j].path) == "README.md" {
			return false
		}
		return entries[i].mod.After(entries[j].mod)
	})
	var b strings.Builder
	var reads []string
	for i, e := range entries {
		if i >= 5 {
			break
		}
		raw, err := os.ReadFile(e.path)
		if err != nil {
			continue
		}
		rel, _ := filepath.Rel(workspace, e.path)
		text := truncateUTF8(string(raw), 3000)
		fmt.Fprintf(&b, "\n### %s（修改于 %s）\n%s\n", rel, e.mod.Format("2006-01-02 15:04"), strings.TrimSpace(text))
		reads = append(reads, "预读 "+rel)
	}
	return b.String(), reads
}

func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

func scheduleAIPrompt(workspace, text string, now time.Time, digest string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "当前时间：%s（%s）\n工作区：%s\n", now.Format("2006-01-02 15:04:05"), localZoneName(), workspace)
	if digest != "" {
		b.WriteString("\n服务端预读的台账摘要（可能不完整，优先使用；需要时再补读最多 3 个文件）：\n")
		b.WriteString(digest)
	}
	fmt.Fprintf(&b, "\n用户描述：\n%s\n\n按系统提示只输出 JSON。", text)
	return b.String()
}

func uniqStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	if len(out) > 12 {
		out = out[:12]
	}
	return out
}

// runScheduleAIParse is the seam tests replace; production runs the real
// one-shot agent turn.
func (s *Server) runScheduleAIParse(ctx context.Context, text string) (scheduleAIResult, error) {
	if s.schedAgentRun != nil {
		return s.schedAgentRun(ctx, text)
	}
	return s.runScheduleAIAgent(ctx, text)
}

// scheduleAIWorkspace picks the mainAgent (or a sensible fallback) and its cwd.
func (s *Server) scheduleAIWorkspace() (*store.Session, string) {
	for _, sess := range s.st.List() {
		if sess.MainAgent && !sess.Archived && sess.GroupID == "" {
			return sess, sess.Cwd
		}
	}
	workspace := s.cfg().DefaultCwd
	if workspace == "" {
		workspace, _ = os.UserHomeDir()
	}
	return nil, workspace
}

func scheduleAIPermissionMode(kind string) string {
	switch kind {
	case config.KindClaude, config.KindQoder:
		// No PermissionRequest hook, no interactive approval: unapproved tools
		// (writes/Bash) are denied, reads still work.
		return "dontAsk"
	case config.KindCodex:
		// codex maps plan -> --sandbox read-only.
		return "plan"
	default:
		// opencode/mcode have no reliable read-only mode in this path; the
		// strict role prompt carries the read-only requirement.
		return "bypassPermissions"
	}
}

// pickScheduleAIClip returns a CLI/provider pair for the temp session, copying
// the main agent when possible (same environment) and otherwise preferring a
// claude-kind cli.
func (s *Server) pickScheduleAIClip(main *store.Session) (*config.CLI, *config.Provider, error) {
	cfg := s.cfg()
	pick := func(cliName, provName string) (*config.CLI, *config.Provider, bool) {
		cli := cfg.CLIByName(cliName)
		prov := cfg.ProviderByName(provName)
		if cli == nil || prov == nil || cli.Kind != prov.Kind {
			return nil, nil, false
		}
		return cli, prov, true
	}
	if main != nil {
		if cli, prov, ok := pick(main.CLI, main.Provider); ok {
			return cli, prov, nil
		}
	}
	for _, cli := range cfg.CLIs {
		for _, prov := range cfg.Providers {
			if cli.Kind == prov.Kind {
				return &cli, &prov, nil
			}
		}
	}
	return nil, nil, fmt.Errorf("没有可用于 AI 解析的 CLI/provider")
}

func (s *Server) waitScheduleAITurn(ctx context.Context, id string) error {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		if !s.mgr.Busy(id) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// runScheduleAIAgent creates a hidden temp session, runs one turn, reads the
// assistant text back from the engine transcript, then removes the session and
// its transcript. It never touches the mainAgent conversation.
func (s *Server) runScheduleAIAgent(ctx context.Context, text string) (scheduleAIResult, error) {
	main, workspace := s.scheduleAIWorkspace()
	cli, prov, err := s.pickScheduleAIClip(main)
	if err != nil {
		return scheduleAIResult{}, err
	}
	workspace = strings.TrimSpace(workspace)
	if info, err := os.Stat(workspace); err != nil || !info.IsDir() {
		workspace = s.cfg().DefaultCwd
		if info, err := os.Stat(workspace); err != nil || !info.IsDir() {
			workspace, _ = os.UserHomeDir()
		}
	}
	kind := cli.Kind
	id := "schedparse-" + newID()
	now := time.Now()
	sess := &store.Session{
		ID: id, Name: "定时任务解析（临时）",
		CLI: cli.Name, Provider: prov.Name, Cwd: workspace,
		PermissionMode: scheduleAIPermissionMode(kind),
		EffortOverride: "low", // parsing needs speed, not deep reasoning
		RolePrompt:     scheduleAIParseRolePrompt,
		GroupID:        scheduleAITempGroup,
		CreatedAt:      now, UpdatedAt: now,
	}
	if main != nil {
		sess.ModelOverride = main.ModelOverride
	}
	if err := s.st.Upsert(sess, nil); err != nil {
		return scheduleAIResult{}, fmt.Errorf("创建临时解析会话失败：%w", err)
	}
	log.Printf("schedule ai: temp session %s cli=%s provider=%s workspace=%s", id, cli.Name, prov.Name, workspace)
	defer func() {
		if s.mgr.Busy(id) {
			s.mgr.Stop(id)
			deadline := time.Now().Add(800 * time.Millisecond)
			for s.mgr.Busy(id) && time.Now().Before(deadline) {
				time.Sleep(100 * time.Millisecond)
			}
		}
		final := s.st.Get(id)
		_ = s.st.Delete(id)
		if final != nil {
			s.removeScheduleAITranscript(final, cli)
		}
	}()

	digest, digestReads := scheduleAIContextDigest(workspace)
	prompt := scheduleAIPrompt(workspace, text, now, digest)
	if err := s.mgr.Enqueue(id, runner.Message{Text: prompt}); err != nil {
		return scheduleAIResult{}, fmt.Errorf("启动临时解析会话失败：%w", err)
	}
	if err := s.waitScheduleAITurn(ctx, id); err != nil {
		log.Printf("schedule ai: temp session %s wait: %v", id, err)
		s.mgr.Stop(id)
		return scheduleAIResult{}, err
	}
	// Let the CLI finish flushing its transcript before parsing it.
	time.Sleep(200 * time.Millisecond)
	final := s.st.Get(id)
	if final == nil {
		return scheduleAIResult{}, fmt.Errorf("临时解析会话已被清理")
	}
	events, err := s.replayEvents(final)
	if err != nil {
		return scheduleAIResult{}, fmt.Errorf("读取解析结果失败：%w", err)
	}
	out, usage, reads := assistantOutput(events)
	log.Printf("schedule ai: temp session %s output=%d bytes events=%d tokens=%d/%d reads=%d",
		id, len(out), len(events), usage.InputTokens, usage.OutputTokens, len(reads))
	if strings.TrimSpace(out) == "" {
		if st := s.mgr.GetStatus(id); st.LastError != "" {
			return scheduleAIResult{}, fmt.Errorf("解析任务失败：%s", st.LastError)
		}
		return scheduleAIResult{}, fmt.Errorf("解析任务没有输出")
	}
	return scheduleAIResult{
		Output: out, InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens,
		ContextReads: uniqStrings(append(digestReads, reads...)),
	}, nil
}

// removeScheduleAITranscript deletes the on-disk transcript created by the
// temp turn (claude/qoder), so it never appears in import lists or stats.
func (s *Server) removeScheduleAITranscript(sess *store.Session, cli *config.CLI) {
	if sess == nil || sess.ClaudeSessionID == "" {
		return
	}
	kind := ""
	if cli != nil {
		kind = cli.Kind
	}
	var prof claudehist.Profile
	switch kind {
	case config.KindClaude:
		prof = claudehist.ClaudeProfile
	case config.KindQoder:
		prof = claudehist.QoderProfile
	default:
		return
	}
	path := claudehist.HistoryPathIn(prof, sess.Cwd, sess.ClaudeSessionID)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.Printf("schedule ai: remove transcript %s: %v", path, err)
	}
	// Subagent transcripts live in <sid>/ next to the main jsonl.
	_ = os.RemoveAll(filepath.Join(filepath.Dir(path), sess.ClaudeSessionID))
}

// assistantOutput extracts text, usage and read-tool targets from canonical
// replay events. Text blocks are concatenated in order; the JSON extractor
// later picks the last object.
func assistantOutput(events []*store.Event) (string, scheduleAIUsage, []string) {
	var b strings.Builder
	var usage scheduleAIUsage
	var reads []string
	seen := map[string]bool{}
	for _, ev := range events {
		if ev == nil || ev.Kind != "stream" {
			continue
		}
		var m struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct {
					Type  string         `json:"type"`
					Text  string         `json:"text"`
					Name  string         `json:"name"`
					Input map[string]any `json:"input"`
				} `json:"content"`
				Usage struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(ev.Data, &m) != nil || m.Type != "assistant" {
			continue
		}
		usage.InputTokens += m.Message.Usage.InputTokens
		usage.OutputTokens += m.Message.Usage.OutputTokens
		for _, c := range m.Message.Content {
			switch c.Type {
			case "text":
				b.WriteString(c.Text)
				b.WriteString("\n")
			case "tool_use":
				if r := scheduleAIContextRead(c.Name, c.Input); r != "" && !seen[r] {
					seen[r] = true
					reads = append(reads, r)
				}
			}
		}
	}
	if len(reads) > 12 {
		reads = reads[:12]
	}
	return b.String(), usage, reads
}

func scheduleAIContextRead(tool string, input map[string]any) string {
	str := func(k string) string {
		if v, ok := input[k].(string); ok {
			return strings.TrimSpace(v)
		}
		return ""
	}
	switch tool {
	case "Read":
		if p := str("file_path"); p != "" {
			return "Read " + p
		}
	case "Grep":
		if p := str("pattern"); p != "" {
			return "Grep " + p
		}
	case "Glob":
		if p := str("pattern"); p != "" {
			return "Glob " + p
		}
	}
	return ""
}

// parseAIProposal accepts a bare JSON object or one wrapped in a markdown code
// fence and validates the field shapes. Models sometimes emit several objects
// despite the instruction; we scan for balanced top-level objects and use the
// last valid one instead of failing on the first/last brace.
func parseAIProposal(raw string) (scheduleAIProposal, error) {
	text := strings.TrimSpace(raw)
	if i := strings.Index(text, "```"); i >= 0 {
		rest := text[i+3:]
		rest = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest), "json"))
		if j := strings.Index(rest, "```"); j >= 0 {
			text = rest[:j]
		} else {
			text = rest
		}
	}
	objects := extractJSONObjects(text)
	if len(objects) == 0 {
		return scheduleAIProposal{}, fmt.Errorf("没有找到 JSON 对象")
	}
	var lastErr error
	for i := len(objects) - 1; i >= 0; i-- {
		var p scheduleAIProposal
		if err := json.Unmarshal([]byte(objects[i]), &p); err != nil {
			lastErr = err
			continue
		}
		p.Name = strings.TrimSpace(p.Name)
		p.Cron = strings.TrimSpace(p.Cron)
		p.Prompt = strings.TrimSpace(p.Prompt)
		p.Rationale = strings.TrimSpace(p.Rationale)
		var qs []string
		for _, q := range p.Questions {
			if q = strings.TrimSpace(q); q != "" {
				qs = append(qs, q)
			}
			if len(qs) >= 5 {
				break
			}
		}
		p.Questions = qs
		if p.Cron == "" && len(p.Questions) == 0 {
			lastErr = fmt.Errorf("既没有 cron 也没有需要确认的问题")
			continue
		}
		return p, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("JSON 解析失败")
	}
	return scheduleAIProposal{}, lastErr
}

// extractJSONObjects returns balanced top-level {...} substrings in order.
// Braces inside JSON strings are ignored.
func extractJSONObjects(text string) []string {
	var out []string
	depth := 0
	start := -1
	inStr := false
	escaped := false
	for i := 0; i < len(text); i++ {
		c := text[i]
		if inStr {
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					out = append(out, text[start:i+1])
					start = -1
				}
			}
		}
	}
	return out
}
