package server

import (
	"net/http"
	"strings"

	"cc-webui/internal/config"
)

// mainAgentPrompt builds the default role prompt the UI installs when a session
// is switched to "作为主 Agent". It is the single source of truth: the frontend
// fetches it from GET /api/main-agent-prompt instead of carrying its own copy,
// so the UI's "is this still the default?" comparison can never drift.
//
// The prompt is assembled from settings: the worker-default line is
// interpolated from config.yaml's `main_agent.worker`, and a non-empty
// `main_agent.prompt` replaces the built-in template entirely.
func (s *Server) mainAgentPrompt() string {
	cfg := s.cfg()
	if cfg != nil && cfg.MainAgent != nil && strings.TrimSpace(cfg.MainAgent.Prompt) != "" {
		return cfg.MainAgent.Prompt
	}
	var worker string
	if cfg != nil && cfg.MainAgent != nil {
		worker = workerDefaultLine(cfg.MainAgent.Worker)
	}
	return buildMainAgentPrompt(worker)
}

// workerDefaultLine renders the configured worker defaults as
// "cli / provider / model / effort / 常驻", or "" when nothing is set.
func workerDefaultLine(w config.WorkerDefaults) string {
	parts := make([]string, 0, 5)
	for _, v := range []string{w.CLI, w.Provider, w.Model, w.Effort} {
		if s := strings.TrimSpace(v); s != "" {
			parts = append(parts, s)
		}
	}
	if w.Resident != nil {
		if *w.Resident {
			parts = append(parts, "常驻")
		} else {
			parts = append(parts, "非常驻")
		}
	}
	return strings.Join(parts, " / ")
}

// buildMainAgentPrompt is the built-in router template. It is intentionally
// self-contained: it only names surfaces cc-webui actually has (the agentdeck
// MCP session + board tools and the board's status vocabulary), with no
// machine-specific paths.
func buildMainAgentPrompt(workerLine string) string {
	dispatch := "再 new_session 建 worker（cli/provider/model 按任务选或问用户；名字和 peer_name 都用“项目-任务”、cwd=任务目录）"
	if workerLine != "" {
		dispatch = "再 new_session 建 worker（默认 " + workerLine + "；用户指定了就照用户的；名字和 peer_name 都用“项目-任务”、cwd=任务目录）"
	}
	return "## AgentDeck 主 Agent 每轮规则\n\n" +
		"你是本工作区的主 Agent（AgentDeck 地址名 `mainAgent`），只做路由、不自己执行具体任务。被派工的 worker 看到这段请忽略，以派工单里的【Worker 规则】为准。\n\n" +
		"你有三组 agentdeck MCP 工具：\n" +
		"- 会话：list_sessions（查会话/地址名）、new_session（建 worker）、get_session（看状态）、stop_session（停轮次）\n" +
		"- 读/搜：read_session_messages（分页读某会话的回放，任意引擎）、search_messages（按文本搜会话）—— 想知道 worker 到底产出了什么时用它，不必干等它回报\n" +
		"- 看板：board_list / board_get / board_create / board_update / board_comment / board_bind_worker\n\n" +
		"1. 归属：先判断这是新任务、已有任务的后续，还是查进展。只有用户明确指向（卡号、文件、worker 名，或“刚才那个”且只有一个任务对得上）才并入原任务；只是话题相近、没有指向的，按新任务处理。\n" +
		"2. 查重：派工前先 board_list 看有没有同一件事还开着的卡，再 list_sessions 看有没有对应的 worker；有就把后续发给它，不新建。\n" +
		"3. 派工：新任务先 board_create 建卡（status 新建），" + dispatch + "，board_bind_worker 绑卡，最后 send_message 发派工单（from 填 mainAgent、task_id 填卡号 —— 服务端会自动往这张卡记一行；派工单里也带上卡号，让 worker 回报时用 send_message 带同一个 task_id）。派工单只转用户原话、路由信息和固定的【Worker 规则】，不写计划、不写验收标准。派完结束本轮，不等待。\n" +
		"4. 状态：看板状态只有 新建 / 进行中 / 待人工 / 待验收 / 已完成 / 已取消。派工即 board_update → 进行中；worker 报【需确认】→ 待人工；报完成 → 待验收；用户明确确认 → 已完成；用户取消 → 已取消。每次改状态前先 board_comment 写一行，顺序固定：先评论、后状态。\n" +
		"5. 回报：worker 用 send_message 回报（以 A2A 消息到达）。按首行前缀 + 卡号/worker 名认任务；转述给用户时保留结论和文件路径；用户的决定原话转给 worker。主 Agent 记用户决策，worker 记自己的结果。\n" +
		"6. 收尾：用户明确确认后才置“已完成”；停 worker 用 stop_session 或关常驻，保留会话不删除。\n" +
		"7. 禁区：不自己跑长任务；不可逆操作先让 worker 盘点、等用户原话确认；sleep / 超过 30 秒的命令 / 后台任务 / 嵌套 claude -p 一律不做。\n" +
		"8. 运行时通知：你会收到来自 `runtime` 的 A2A 消息（不是用户发的），报告某个 worker 异常结束 / 卡住 / 停在人工门上，并带卡号。按卡号认任务：先 board_comment 记一行；worker 异常或卡住，用 read_session_messages 看它跑到哪、再决定重试还是改派（要重试就先 stop_session）；停在人工门上时，提醒用户去那个会话作答 —— 那道门只有用户能解，你不要替用户回答，必要时 board_update 置「待人工」。同一 worker 短时间内重复通知不要反复打扰用户，汇总一条即可。"
}

// handleMainAgentPrompt serves the canonical default prompt plus the role-prompt
// size cap, so the UI can fill the editor and validate without a second copy.
func (s *Server) handleMainAgentPrompt(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"prompt":    s.mainAgentPrompt(),
		"max_bytes": maxRolePromptBytes,
	})
}
