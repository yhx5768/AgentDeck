package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"cc-webui/internal/knowledge"
)

// Knowledge P0: a workspace-level curated store on top of the board/artifacts.
// The server owns all reads of board and artifact metadata; the knowledge
// package never scans a workspace or a transcript.

type knowledgeActor struct {
	SessionID string
	Address   string
	Admin     bool
}

func (s *Server) initKnowledgeDB() {
	path := filepath.Join(s.cfg().DataDir, "knowledge.db")
	st, err := knowledge.Open(path)
	if err != nil {
		s.knowledgeDBErr = fmt.Errorf("open knowledge db: %w", err)
		log.Printf("knowledge: %v; knowledge API disabled", s.knowledgeDBErr)
		return
	}
	s.knowledgeDB = st
	log.Printf("knowledge: sqlite %s ready", path)
}

func (s *Server) closeKnowledgeDB() {
	s.knowledgeDBOnce.Do(func() {
		if s.knowledgeDB != nil {
			_ = s.knowledgeDB.Close()
		}
	})
}

func (s *Server) knowledgeDBStore(w http.ResponseWriter) (*knowledge.Store, bool) {
	if s.knowledgeDB != nil {
		return s.knowledgeDB, true
	}
	message := "知识库未初始化"
	if s.knowledgeDBErr != nil {
		message = "知识库不可用: " + s.knowledgeDBErr.Error()
	}
	writeErr(w, http.StatusInternalServerError, message)
	return nil, false
}

// knowledgeActorFromRequest identifies MCP callers by the session header. The
// browser UI has no session header, so it is the console user (admin). This is
// the same single-user model as the rest of AgentDeck: main_agent is a business
// gate, not a multi-user ACL.
func (s *Server) knowledgeActorFromRequest(r *http.Request) knowledgeActor {
	return s.knowledgeActorForSession(strings.TrimSpace(r.Header.Get("X-CC-Webui-Session")))
}

func (s *Server) knowledgeActorForSession(sessionID string) knowledgeActor {
	if strings.TrimSpace(sessionID) == "" {
		return knowledgeActor{Address: "user", Admin: true}
	}
	if s.st == nil {
		return knowledgeActor{SessionID: sessionID, Address: "worker", Admin: false}
	}
	if sess := s.st.Get(sessionID); sess != nil {
		return knowledgeActor{SessionID: sessionID, Address: sessionAddress(sess), Admin: sess.MainAgent}
	}
	return knowledgeActor{SessionID: sessionID, Address: "worker", Admin: false}
}

// knowledgeActorForMCP never treats an unknown caller as the UI admin: shared
// MCP configs (opencode/mcode) may have no per-session identity, and a missing
// session header must mean "worker", not "console owner".
func (s *Server) knowledgeActorForMCP(sessionID string) knowledgeActor {
	if strings.TrimSpace(sessionID) == "" {
		return knowledgeActor{Address: "worker", Admin: false}
	}
	return s.knowledgeActorForSession(sessionID)
}

func knowledgeSearchParamsFromArgs(args map[string]any) knowledgeSearchParams {
	return knowledgeSearchParams{
		Query:            stringArg(args, "query"),
		Project:          stringArg(args, "project"),
		Status:           stringArg(args, "status"),
		Tag:              stringArg(args, "tag"),
		SourceTaskID:     stringArg(args, "source_task_id"),
		Limit:            intArg(args, "limit"),
		Offset:           intArg(args, "offset"),
		IncludeDrafts:    boolArg(args, "include_drafts"),
		IncludeStale:     boolArg(args, "include_stale"),
		IncludeSensitive: boolArg(args, "include_sensitive"),
	}
}

func boolArg(args map[string]any, key string) bool {
	value, _ := args[key].(bool)
	return value
}

func jsonResult(value any, err error) (string, error) {
	if err != nil {
		return "", err
	}
	raw, marshalErr := json.MarshalIndent(value, "", "  ")
	if marshalErr != nil {
		return "", marshalErr
	}
	return string(raw), nil
}

type knowledgeSearchParams struct {
	Query            string
	Project          string
	Status           string
	Tag              string
	SourceTaskID     string
	Limit            int
	Offset           int
	IncludeDrafts    bool
	IncludeStale     bool
	IncludeSensitive bool
}

func (p *knowledgeSearchParams) normalize(actor knowledgeActor) {
	p.Query = strings.TrimSpace(p.Query)
	p.Project = strings.TrimSpace(p.Project)
	p.Status = strings.ToLower(strings.TrimSpace(p.Status))
	p.Tag = strings.TrimSpace(p.Tag)
	p.SourceTaskID = strings.TrimSpace(p.SourceTaskID)
	if p.Limit <= 0 {
		p.Limit = 20
	}
	if p.Limit > 50 {
		p.Limit = 50
	}
	if p.Offset < 0 {
		p.Offset = 0
	}
	if !actor.Admin {
		p.IncludeDrafts = false
		p.IncludeStale = false
		p.IncludeSensitive = false
		switch p.Status {
		case "draft", "stale", "quarantined", "rejected", "superseded":
			p.Status = "approved"
		case "":
			p.Status = "approved"
		case "all":
			p.Status = "approved"
		}
		return
	}
	if p.Status == "all" {
		p.IncludeDrafts = true
		p.IncludeStale = true
	}
	if p.Status == "draft" {
		p.IncludeDrafts = true
	}
	if p.Status == "stale" {
		p.IncludeStale = true
	}
	if p.Status == "quarantined" {
		p.IncludeSensitive = true
	}
}

func (p knowledgeSearchParams) options() knowledge.SearchOptions {
	return knowledge.SearchOptions{
		Query: p.Query, Project: p.Project, Status: p.Status, Tag: p.Tag,
		SourceTaskID: p.SourceTaskID, Limit: p.Limit, Offset: p.Offset,
		IncludeDrafts: p.IncludeDrafts, IncludeStale: p.IncludeStale,
		IncludeSensitive: p.IncludeSensitive,
	}
}

func (s *Server) handleKnowledgeList(w http.ResponseWriter, r *http.Request) {
	st, ok := s.knowledgeDBStore(w)
	if !ok {
		return
	}
	actor := s.knowledgeActorFromRequest(r)
	values := r.URL.Query()
	params := knowledgeSearchParams{
		Query: values.Get("q"), Project: values.Get("project"), Status: values.Get("status"),
		Tag: values.Get("tag"), SourceTaskID: values.Get("source_task_id"),
		Limit: intQuery(values.Get("limit")), Offset: intQuery(values.Get("offset")),
		IncludeDrafts:    truthyQuery(values.Get("include_drafts")),
		IncludeStale:     truthyQuery(values.Get("include_stale")),
		IncludeSensitive: truthyQuery(values.Get("include_sensitive")),
	}
	params.normalize(actor)
	res, err := st.Search(params.options())
	if err != nil {
		writeErr(w, http.StatusBadRequest, "搜索知识失败: "+err.Error())
		return
	}
	counts, projects, _ := st.Facets()
	entries := make([]any, 0, len(res.Entries))
	for _, entry := range res.Entries {
		entries = append(entries, s.knowledgeEntryView(r, entry, false))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "entries": entries, "total": res.Total,
		"counts": counts, "projects": projects,
	})
}

func (s *Server) handleKnowledgeGet(w http.ResponseWriter, r *http.Request) {
	st, ok := s.knowledgeDBStore(w)
	if !ok {
		return
	}
	actor := s.knowledgeActorFromRequest(r)
	entry, err := st.Get(r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "知识不存在: "+r.PathValue("id"))
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取知识失败: "+err.Error())
		return
	}
	if !knowledgeCanRead(actor, entry) {
		writeErr(w, http.StatusNotFound, "知识不存在: "+r.PathValue("id"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "entry": s.knowledgeEntryView(r, entry, true),
	})
}

func knowledgeCanRead(actor knowledgeActor, entry *knowledge.Entry) bool {
	if entry == nil {
		return false
	}
	if actor.Admin {
		return true
	}
	if entry.Status == "quarantined" || entry.Sensitivity == "sensitive" || entry.Sensitivity == "secret" {
		return false
	}
	if entry.Status == "rejected" || entry.Status == "superseded" {
		return false
	}
	if entry.Status == "draft" && entry.SourceSessionID != actor.SessionID {
		return false
	}
	return true
}

func (s *Server) handleKnowledgeCreate(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.knowledgeDBStore(w); !ok {
		return
	}
	var body map[string]any
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	actor := s.knowledgeActorFromRequest(r)
	result, err := s.knowledgeAddData(actor, body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "创建知识失败: "+err.Error())
		return
	}
	status := http.StatusCreated
	if duplicate, _ := result["duplicate"].(bool); duplicate {
		status = http.StatusOK
	}
	s.broadcastAgentBoard()
	writeJSON(w, status, result)
}

func (s *Server) handleKnowledgeUpdate(w http.ResponseWriter, r *http.Request) {
	st, ok := s.knowledgeDBStore(w)
	if !ok {
		return
	}
	var body map[string]any
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	actor := s.knowledgeActorFromRequest(r)
	id := r.PathValue("id")
	current, err := st.Get(id)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "知识不存在: "+id)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取知识失败: "+err.Error())
		return
	}
	if !actor.Admin {
		if current.SourceSessionID != actor.SessionID || current.Status != "draft" {
			writeErr(w, http.StatusForbidden, "只能编辑自己创建的草稿")
			return
		}
		for _, key := range []string{"status", "review_reason", "reviewer"} {
			if _, exists := body[key]; exists {
				writeErr(w, http.StatusForbidden, "审批操作仅主 Agent 可用")
				return
			}
		}
	}
	if _, err := s.knowledgeUpdateData(actor, id, body); err != nil {
		writeErr(w, http.StatusBadRequest, "更新知识失败: "+err.Error())
		return
	}
	entry, err := st.Get(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取知识失败: "+err.Error())
		return
	}
	s.broadcastAgentBoard()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "entry": s.knowledgeEntryView(r, entry, true),
	})
}

func (s *Server) handleKnowledgeCollect(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.knowledgeDBStore(w); !ok {
		return
	}
	actor := s.knowledgeActorFromRequest(r)
	if !actor.Admin {
		writeErr(w, http.StatusForbidden, "知识采集仅主 Agent 可用")
		return
	}
	result, err := s.knowledgeCollectData(actor)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "知识采集失败: "+err.Error())
		return
	}
	s.broadcastAgentBoard()
	writeJSON(w, http.StatusOK, result)
}

// knowledgeSearchData is shared by the HTTP handler and the agentdeck MCP
// knowledge_search tool. The caller has already been authorized.
func (s *Server) knowledgeSearchData(actor knowledgeActor, params knowledgeSearchParams) (map[string]any, error) {
	st, ok := s.knowledgeStoreReady()
	if !ok {
		return nil, fmt.Errorf("知识库未初始化")
	}
	params.normalize(actor)
	res, err := st.Search(params.options())
	if err != nil {
		return nil, err
	}
	counts, projects, _ := st.Facets()
	entries := make([]any, 0, len(res.Entries))
	for _, entry := range res.Entries {
		entries = append(entries, s.knowledgeEntryView(nil, entry, false))
	}
	return map[string]any{
		"ok": true, "entries": entries, "total": res.Total,
		"counts": counts, "projects": projects,
	}, nil
}

func (s *Server) knowledgeStoreReady() (*knowledge.Store, bool) {
	if s.knowledgeDB == nil {
		return nil, false
	}
	return s.knowledgeDB, true
}

func (s *Server) knowledgeGetData(actor knowledgeActor, id string) (map[string]any, error) {
	st, ok := s.knowledgeStoreReady()
	if !ok {
		return nil, fmt.Errorf("知识库未初始化")
	}
	entry, err := st.Get(strings.TrimSpace(id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("知识不存在: %s", id)
		}
		return nil, err
	}
	if !knowledgeCanRead(actor, entry) {
		return nil, fmt.Errorf("知识不存在: %s", id)
	}
	return map[string]any{"ok": true, "entry": s.knowledgeEntryView(nil, entry, true)}, nil
}

func (s *Server) knowledgeAddData(actor knowledgeActor, input map[string]any) (map[string]any, error) {
	st, ok := s.knowledgeStoreReady()
	if !ok {
		return nil, fmt.Errorf("知识库未初始化")
	}
	sourceTaskID := stringArg(input, "source_task_id")
	sourceSessionID := stringArg(input, "source_session_id")
	if sourceSessionID == "" {
		sourceSessionID = actor.SessionID
	}
	if !actor.Admin && sourceTaskID == "" {
		return nil, fmt.Errorf("worker knowledge_add requires source_task_id")
	}
	status := strings.ToLower(strings.TrimSpace(stringArg(input, "status")))
	if status == "" {
		status = "draft"
	}
	if !actor.Admin {
		status = "draft"
	}
	if status != "draft" && status != "approved" {
		return nil, fmt.Errorf("status must be draft or approved")
	}
	sourceKind := strings.TrimSpace(stringArg(input, "source_kind"))
	if sourceKind == "" {
		if actor.Admin {
			sourceKind = "manual"
		} else {
			sourceKind = "worker_report"
		}
	}
	project := strings.TrimSpace(stringArg(input, "project"))
	sourceArtifacts := stringSliceArg(input, "source_artifact_ids")
	sourcePaths := stringSliceArg(input, "source_paths")
	tags := stringSliceArg(input, "tags")
	evidence := []knowledge.Evidence{}
	if sourceTaskID != "" {
		if s.boardDB == nil {
			return nil, fmt.Errorf("看板数据库未初始化")
		}
		task, err := s.boardDB.GetTask(sourceTaskID)
		if err != nil {
			return nil, fmt.Errorf("来源任务不存在: %s", sourceTaskID)
		}
		if project == "" {
			project, _ = task["project"].(string)
		}
		if len(tags) == 0 {
			tags = anyStrings(task["tags"])
		}
		quote, _ := task["description"].(string)
		if strings.TrimSpace(quote) == "" {
			quote, _ = task["last_event"].(string)
		}
		if strings.TrimSpace(quote) == "" {
			quote, _ = task["title"].(string)
		}
		evidence = append(evidence, knowledge.Evidence{
			Kind: "task", Ref: sourceTaskID, Locator: "task", Quote: quote,
		})
		for _, artifactID := range sourceArtifacts {
			artifact, err := s.boardDB.GetArtifact(artifactID)
			if err != nil || artifact.TaskID != sourceTaskID {
				return nil, fmt.Errorf("产物不属于来源任务: %s", artifactID)
			}
			evidence = append(evidence, knowledge.Evidence{
				Kind: "artifact", Ref: artifact.ID, Locator: artifact.Filename, Quote: artifact.Title,
			})
		}
	}
	for _, path := range sourcePaths {
		evidence = append(evidence, knowledge.Evidence{Kind: "path", Ref: path, Locator: "path"})
	}

	confidence := intArg(input, "confidence")
	generatedBy := strings.TrimSpace(stringArg(input, "generated_by"))
	aiFields := stringSliceArg(input, "ai_fields")
	if generatedBy == "" {
		if len(aiFields) > 0 {
			generatedBy = "hybrid"
		} else {
			generatedBy = "human"
		}
	}
	owner := strings.TrimSpace(stringArg(input, "owner"))
	if owner == "" {
		owner = actor.Address
	}
	entry, duplicate, err := st.Create(knowledge.CreateInput{
		Title:             stringArg(input, "title"),
		Summary:           stringArg(input, "summary"),
		Body:              stringArg(input, "body"),
		Project:           project,
		Owner:             owner,
		Status:            status,
		Confidence:        confidence,
		Sensitivity:       stringArg(input, "sensitivity"),
		SourceKind:        sourceKind,
		SourceTaskID:      sourceTaskID,
		SourceSessionID:   sourceSessionID,
		SourceArtifactIDs: sourceArtifacts,
		SourcePaths:       sourcePaths,
		GeneratedBy:       generatedBy,
		AIFields:          aiFields,
		Tags:              tags,
		Evidence:          evidence,
		Actor:             actor.Address,
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"ok": true, "id": entry.ID, "duplicate": duplicate, "status": entry.Status,
		"entry": s.knowledgeEntryView(nil, entry, true),
	}, nil
}

func (s *Server) knowledgeUpdateData(actor knowledgeActor, id string, input map[string]any) (*knowledge.Entry, error) {
	st, ok := s.knowledgeStoreReady()
	if !ok {
		return nil, fmt.Errorf("知识库未初始化")
	}
	var in knowledge.UpdateInput
	in.Actor = actor.Address
	if value, ok := input["title"].(string); ok {
		in.Title = &value
	}
	if value, ok := input["summary"].(string); ok {
		in.Summary = &value
	}
	if value, ok := input["body"].(string); ok {
		in.Body = &value
	}
	if value, ok := input["project"].(string); ok {
		in.Project = &value
	}
	if _, ok := input["tags"]; ok {
		tags := stringSliceArg(input, "tags")
		in.Tags = &tags
	}
	if value, ok := input["confidence"]; ok {
		confidence := intArg(map[string]any{"confidence": value}, "confidence")
		in.Confidence = &confidence
	}
	if value, ok := input["sensitivity"].(string); ok {
		in.Sensitivity = &value
	}
	action := strings.ToLower(strings.TrimSpace(stringArg(input, "action")))
	statusValue := strings.ToLower(strings.TrimSpace(stringArg(input, "status")))
	switch action {
	case "approve":
		statusValue = "approved"
	case "reject":
		statusValue = "rejected"
	case "stale", "expire":
		statusValue = "stale"
	case "restore":
		statusValue = "approved"
	case "draft":
		statusValue = "draft"
	}
	if statusValue != "" {
		if !actor.Admin {
			return nil, fmt.Errorf("审批操作仅主 Agent 可用")
		}
		in.Status = &statusValue
		reviewer := actor.Address
		in.Reviewer = &reviewer
		reason := stringArg(input, "reason")
		if reason == "" {
			reason = stringArg(input, "review_reason")
		}
		if reason != "" {
			in.ReviewReason = &reason
		}
		if action == "" {
			action = statusValue
		}
		in.Action = action
	} else if in.Action == "" {
		in.Action = "update"
	}
	return st.Update(id, in)
}

func (s *Server) knowledgeReviewData(actor knowledgeActor, id string, input map[string]any) (map[string]any, error) {
	if !actor.Admin {
		return nil, fmt.Errorf("审批操作仅主 Agent 可用")
	}
	entry, err := s.knowledgeUpdateData(actor, id, input)
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "entry": s.knowledgeEntryView(nil, entry, true)}, nil
}

func (s *Server) knowledgeCollectData(actor knowledgeActor) (map[string]any, error) {
	st, ok := s.knowledgeStoreReady()
	if !ok {
		return nil, fmt.Errorf("知识库未初始化")
	}
	if s.boardDB == nil {
		return nil, fmt.Errorf("看板数据库未初始化")
	}
	snapshot, err := s.boardDB.Snapshot()
	if err != nil {
		return nil, err
	}
	tasksAny, _ := snapshot["tasks"].([]any)
	created := 0
	skipped := 0
	duplicates := 0
	quarantined := 0
	for _, item := range tasksAny {
		task, ok := item.(map[string]any)
		if !ok {
			continue
		}
		status, _ := task["status"].(string)
		if status != "待验收" && status != "已完成" {
			skipped++
			continue
		}
		taskID, _ := task["id"].(string)
		if taskID == "" {
			skipped++
			continue
		}
		has, err := st.HasTaskEntry(taskID)
		if err != nil {
			return nil, err
		}
		if has {
			skipped++
			continue
		}
		in := collectTaskInput(task)
		if strings.TrimSpace(in.Title) == "" {
			skipped++
			continue
		}
		in.Actor = actor.Address
		entry, duplicate, err := st.Create(in)
		if err != nil {
			log.Printf("knowledge collect %s: %v", taskID, err)
			skipped++
			continue
		}
		if duplicate {
			duplicates++
			continue
		}
		if entry.Status == "quarantined" {
			quarantined++
		}
		created++
	}
	return map[string]any{
		"ok": true, "scanned": len(tasksAny), "created": created,
		"skipped": skipped, "duplicates": duplicates, "quarantined": quarantined,
	}, nil
}

// collectTaskInput builds a deterministic draft from board metadata only. It
// never reads a workspace file, a transcript, or a legacy report_file path.
func collectTaskInput(task map[string]any) knowledge.CreateInput {
	taskID, _ := task["id"].(string)
	title, _ := task["title"].(string)
	project, _ := task["project"].(string)
	status, _ := task["status"].(string)
	owner, _ := task["owner"].(string)
	worker, _ := task["worker"].(string)
	session, _ := task["session"].(string)
	description, _ := task["description"].(string)
	progress, _ := task["progress"].(string)
	lastEvent, _ := task["last_event"].(string)
	timeline := mapSlice(task["timeline"])
	artifacts := mapSlice(task["artifacts"])

	tags := anyStrings(task["tags"])
	tags = append(tags, project)
	artifactIDs := []string{}
	evidence := []knowledge.Evidence{}
	var artifactLines []string
	for _, artifact := range artifacts {
		if legacy, _ := artifact["legacy"].(bool); legacy {
			continue
		}
		id, _ := artifact["id"].(string)
		kind, _ := artifact["kind"].(string)
		artifactTitle, _ := artifact["title"].(string)
		filename, _ := artifact["filename"].(string)
		if id == "" {
			continue
		}
		artifactIDs = append(artifactIDs, id)
		tags = append(tags, kind)
		artifactLines = append(artifactLines, fmt.Sprintf("- [%s] %s（%s） · id=%s", kind, artifactTitle, filename, id))
		evidence = append(evidence, knowledge.Evidence{
			Kind: "artifact", Ref: id, Locator: filename, Quote: artifactTitle,
		})
	}

	summary := strings.TrimSpace(description)
	if summary == "" && len(timeline) > 0 {
		summary, _ = timeline[0]["text"].(string)
	}
	if summary == "" {
		summary = strings.TrimSpace(lastEvent)
	}
	if summary == "" {
		summary = fmt.Sprintf("任务「%s」%s", title, status)
	}
	if runes := []rune(summary); len(runes) > 300 {
		summary = string(runes[:300]) + "…"
	}

	var body strings.Builder
	body.WriteString("## 任务\n")
	body.WriteString("- 状态：" + status + "\n")
	if project != "" {
		body.WriteString("- 项目：" + project + "\n")
	}
	if worker != "" {
		body.WriteString("- worker：" + worker + "\n")
	}
	if session != "" {
		body.WriteString("- 会话：" + session + "\n")
	}
	if lastEvent != "" {
		body.WriteString("- 最近事件：" + lastEvent + "\n")
	}
	if progress != "" {
		body.WriteString("- 进度：" + progress + "\n")
	}
	if description != "" {
		body.WriteString("\n## 描述\n" + description + "\n")
	}
	if len(timeline) > 0 {
		body.WriteString("\n## 最近时间线\n")
		limit := len(timeline)
		if limit > 12 {
			limit = 12
		}
		for i := 0; i < limit; i++ {
			at, _ := timeline[i]["at"].(string)
			kind, _ := timeline[i]["kind"].(string)
			text, _ := timeline[i]["text"].(string)
			if strings.TrimSpace(text) == "" {
				continue
			}
			body.WriteString(fmt.Sprintf("- [%s] %s：%s\n", at, kind, clipText(text, 300)))
		}
	}
	if len(artifactLines) > 0 {
		body.WriteString("\n## 产物\n" + strings.Join(artifactLines, "\n") + "\n")
	}
	quote := description
	if strings.TrimSpace(quote) == "" {
		quote = lastEvent
	}
	if strings.TrimSpace(quote) == "" {
		quote = title
	}
	evidence = append([]knowledge.Evidence{{
		Kind: "task", Ref: taskID, Locator: "task", Quote: clipText(quote, 300),
	}}, evidence...)

	return knowledge.CreateInput{
		Title:             clipText(title, 200),
		Summary:           summary,
		Body:              body.String(),
		Project:           project,
		Owner:             owner,
		Status:            "draft",
		Confidence:        55,
		Sensitivity:       "internal",
		SourceKind:        "collect",
		SourceTaskID:      taskID,
		SourceSessionID:   session,
		SourceArtifactIDs: artifactIDs,
		GeneratedBy:       "system",
		Tags:              sanitizeKnowledgeTags(tags),
		Evidence:          evidence,
	}
}

func sanitizeKnowledgeTags(tags []string) []string {
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

func clipText(value string, limit int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= limit {
		return string(runes)
	}
	return string(runes[:limit]) + "…"
}

func mapSlice(value any) []map[string]any {
	switch typed := value.(type) {
	case []map[string]any:
		return typed
	case []any:
		out := make([]map[string]any, 0, len(typed))
		for _, item := range typed {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

func anyStrings(value any) []string {
	switch typed := value.(type) {
	case []string:
		return typed
	case []any:
		out := []string{}
		for _, item := range typed {
			if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
				out = append(out, text)
			}
		}
		return out
	}
	return nil
}

func stringSliceArg(input map[string]any, key string) []string {
	value, ok := input[key]
	if !ok {
		return nil
	}
	switch typed := value.(type) {
	case []string:
		return typed
	case []any:
		out := []string{}
		for _, item := range typed {
			if text, ok := item.(string); ok {
				out = append(out, text)
			}
		}
		return out
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil
		}
		parts := strings.FieldsFunc(typed, func(r rune) bool { return r == ',' || r == '，' || r == '\n' })
		out := []string{}
		for _, part := range parts {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				out = append(out, trimmed)
			}
		}
		return out
	}
	return nil
}

func intQuery(value string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(value))
	return n
}

func truthyQuery(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// knowledgeEntryView converts an entry to the API shape and decorates full
// views with source task/artifact metadata (best effort; missing sources are
// marked, not fatal).
func (s *Server) knowledgeEntryView(r *http.Request, entry *knowledge.Entry, full bool) map[string]any {
	if entry == nil {
		return nil
	}
	raw, _ := json.Marshal(entry)
	var view map[string]any
	_ = json.Unmarshal(raw, &view)
	if !full {
		delete(view, "body")
		delete(view, "evidence")
		delete(view, "relations")
		return view
	}
	if taskID, _ := view["source_task_id"].(string); taskID != "" && s.boardDB != nil {
		task, err := s.boardDB.GetTask(taskID)
		if err == nil {
			view["source_task"] = map[string]any{
				"id": taskID, "title": task["title"], "status": task["status"],
				"project": task["project"], "worker": task["worker"], "missing": false,
			}
		} else {
			view["source_task"] = map[string]any{"id": taskID, "missing": true}
		}
	}
	artifacts := make([]any, 0, len(entry.SourceArtifactIDs))
	if s.boardDB != nil {
		for _, artifactID := range entry.SourceArtifactIDs {
			artifact, err := s.boardDB.GetArtifact(artifactID)
			if err != nil {
				artifacts = append(artifacts, map[string]any{"id": artifactID, "missing": true})
				continue
			}
			item := map[string]any{
				"id": artifact.ID, "task_id": artifact.TaskID, "title": artifact.Title,
				"kind": artifact.Kind, "mime": artifact.MIME, "filename": artifact.Filename,
				"size": artifact.Size, "storage_path": artifact.StoragePath, "url": artifact.URL,
				"source_session": artifact.SourceSession, "created_at": artifact.CreatedAt,
			}
			s.decorateArtifact(r, item)
			artifacts = append(artifacts, item)
		}
	}
	view["source_artifacts"] = artifacts
	return view
}

// decorateTaskKnowledge embeds up to three related knowledge summaries in one
// board task (drawer). A missing knowledge store is non-fatal.
func (s *Server) decorateTaskKnowledge(r *http.Request, task map[string]any) {
	if task == nil {
		return
	}
	if s.knowledgeDB == nil {
		task["knowledge"] = []knowledge.Summary{}
		task["knowledge_count"] = 0
		return
	}
	id, _ := task["id"].(string)
	if id == "" {
		return
	}
	summaries, counts, err := s.knowledgeDB.SummariesForTasks([]string{id}, 3)
	if err != nil {
		return
	}
	list := summaries[id]
	if list == nil {
		list = []knowledge.Summary{}
	}
	task["knowledge"] = list
	task["knowledge_count"] = counts[id]
}

// decorateBoardKnowledge does the same for a whole snapshot in one query.
func (s *Server) decorateBoardKnowledge(r *http.Request, snapshot map[string]any) {
	if snapshot == nil {
		return
	}
	tasks, _ := snapshot["tasks"].([]any)
	if s.knowledgeDB == nil {
		for _, item := range tasks {
			if task, ok := item.(map[string]any); ok {
				task["knowledge"] = []knowledge.Summary{}
				task["knowledge_count"] = 0
			}
		}
		return
	}
	ids := make([]string, 0, len(tasks))
	for _, item := range tasks {
		if task, ok := item.(map[string]any); ok {
			if id, _ := task["id"].(string); id != "" {
				ids = append(ids, id)
			}
		}
	}
	summaries, counts, err := s.knowledgeDB.SummariesForTasks(ids, 3)
	if err != nil {
		return
	}
	for _, item := range tasks {
		task, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id, _ := task["id"].(string)
		list := summaries[id]
		if list == nil {
			list = []knowledge.Summary{}
		}
		task["knowledge"] = list
		task["knowledge_count"] = counts[id]
	}
}

// sortKnowledgeSummaries is used by tests and keeps list order deterministic.
func sortKnowledgeSummaries(list []knowledge.Summary) {
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
}
