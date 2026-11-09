package a2a

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// Tool is one MCP tool exposed by the agentdeck server. The same list is used
// by the stdio proxy (tools/list) and the loopback HTTP endpoint, so the model
// and the dispatcher can never disagree about the surface.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func obj(props map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": props, "required": required}
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

// Tools is the agentdeck MCP tool surface: the machine-facing replacement for
// the agentdeck skill. Every engine that speaks MCP gets these injected, so an
// agent on any engine can drive AgentDeck and talk to peer sessions.
var Tools = []Tool{
	{
		Name:        "list_sessions",
		Description: "List AgentDeck sessions with their id, name, address (peer name), engine, resident/online and running state. Use this to find workers before dispatching or to check on a task.",
		InputSchema: obj(map[string]any{}),
	},
	{
		Name:        "send_message",
		Description: "Send an agent-to-agent (A2A) message to another session by its address. The message is delivered as a peer turn on the target session (any engine) and rendered as an A2A bubble. Use this to dispatch work to a worker or report a result back to the main agent. `from` defaults to your own address when the runtime can resolve it; pass it explicitly otherwise.",
		InputSchema: obj(map[string]any{
			"to":      str("Target session address (peer name) or session id."),
			"message": str("Message body. Plain text; the receiving agent sees it as a peer message, not a user instruction."),
			"from":    str("Your own address (peer name). Optional when your runtime injected your session identity."),
			"task_id": str("Optional board task id. When set, a successful delivery also appends a one-line comment to that board task's timeline, so dispatch/report bookkeeping lands on the card without a separate board_comment call."),
			"send_id": str("Optional idempotency key. Re-sending the same send_id (from the same sender) returns the first delivery's receipt instead of delivering the message twice."),
		}, "to", "message"),
	},
	{
		Name:        "read_session_messages",
		Description: "Read a bounded page of another session's transcript, projected to text (user/assistant/peer text and tool names; thinking and tool output are omitted). Use this to see what a worker actually produced instead of waiting for it to report. Works on any engine.",
		InputSchema: obj(map[string]any{
			"session": str("Session address (peer name) or id."),
			"before":  map[string]any{"type": "integer", "description": "Absolute index to page before (exclusive); defaults to the end."},
			"limit":   map[string]any{"type": "integer", "description": "Max messages to return (default 50, max 200)."},
		}, "session"),
	},
	{
		Name:        "search_messages",
		Description: "Search session transcripts for a text query (case-insensitive). Give a session to search just it, or omit it to scan recent sessions (bounded by a hit cap and a time budget). Returns matching messages with a snippet.",
		InputSchema: obj(map[string]any{
			"query":   str("Text to search for."),
			"session": str("Optional session address or id to limit the search."),
			"limit":   map[string]any{"type": "integer", "description": "Max hits (default 50, max 100)."},
		}, "query"),
	},
	{
		Name:        "new_session",
		Description: "Create a new AgentDeck session (typically a worker). Returns the created session. The session appears in the sidebar immediately.",
		InputSchema: obj(map[string]any{
			"name":        str("Display name, e.g. \"项目-任务\"."),
			"cli":         str("CLI name (see /api/clis). Determines the engine."),
			"provider":    str("Provider name for the cli's engine."),
			"model":       str("Model id for the provider. Omit to use the provider default."),
			"effort":      str("Reasoning effort / variant. Engine-specific."),
			"cwd":         str("Workspace directory (required)."),
			"role_prompt": str("Extra system-prompt text defining the worker's identity and rules."),
			"peer_name":   str("Address name other sessions use to reach it. Defaults to the cleaned display name."),
			"resident":    map[string]any{"type": "boolean", "description": "Keep a resident process online (claude only)."},
			"main_agent":  map[string]any{"type": "boolean", "description": "Mark as the workspace main agent."},
		}, "name", "cli", "cwd"),
	},
	{
		Name:        "get_session",
		Description: "Fetch one session's current view (name, address, engine, resident, running, last error).",
		InputSchema: obj(map[string]any{
			"id": str("Session id."),
		}, "id"),
	},
	{
		Name: "artifact_publish",
		Description: "Publish an artifact (HTML page, markdown, file, or external URL) on a board task. " +
			"HTML is hosted by AgentDeck as a shareable page (sandboxed, own URL); the task's 产物与证据 panel updates immediately. " +
			"Content is uploaded inline — this works from a worker on another machine. Returns the artifact metadata and URL. " +
			"For binary content pass encoding=base64; for kind=url pass url instead of content.",
		InputSchema: obj(map[string]any{
			"task_id":        str("Board task id to attach the artifact to (required)."),
			"title":          str("Human-readable title; defaults to the filename."),
			"kind":           str("html | markdown | file | url. Defaults to inferred from filename/mime."),
			"filename":       str("Safe filename; defaults to index.html / artifact.md / artifact.bin / link."),
			"content":        str("Artifact content: text for html/markdown, or base64 when encoding=base64. Omit for kind=url."),
			"encoding":       str("utf8 (default) or base64."),
			"mime":           str("Optional MIME type override."),
			"source_session": str("Session id/address that produced the artifact; defaults to the caller."),
			"url":            str("External http(s) URL when kind=url."),
		}, "task_id"),
	},
	{
		Name: "knowledge_search",
		Description: "Search the AgentDeck knowledge base. Workers see approved entries only " +
			"(sensitive/quarantined rows are hidden); the main agent can include drafts/stale/sensitive. " +
			"Chinese two-character queries work (unigram+bigram FTS5 tokenizer).",
		InputSchema: obj(map[string]any{
			"query":             str("Free-text query (title/summary/body/tags)."),
			"project":           str("Filter by project."),
			"tag":               str("Filter by one tag."),
			"status":            str("approved (default) | draft | stale | all. Draft/stale/all require the main agent."),
			"source_task_id":    str("Only entries collected from / attached to this board task."),
			"limit":             map[string]any{"type": "integer", "description": "Max results (default 20, max 50)."},
			"offset":            map[string]any{"type": "integer", "description": "Pagination offset."},
			"include_drafts":    map[string]any{"type": "boolean", "description": "Main agent only: include drafts."},
			"include_stale":     map[string]any{"type": "boolean", "description": "Main agent only: include stale entries."},
			"include_sensitive": map[string]any{"type": "boolean", "description": "Main agent only: include sensitive rows."},
		}),
	},
	{
		Name:        "knowledge_get",
		Description: "Fetch one knowledge entry with sources/evidence. Workers may read approved entries and their own drafts; sensitive/quarantined entries are main-agent only.",
		InputSchema: obj(map[string]any{"id": str("Knowledge entry id (K-...).")}, "id"),
	},
	{
		Name: "knowledge_add",
		Description: "Add a knowledge draft. Workers MUST pass source_task_id (and should pass source_artifact_ids); " +
			"drafts are reviewed by the main agent before workers can search them. The store dedupes on project+title+body " +
			"and returns duplicate=true with the existing id. Sensitive text is redacted and quarantined.",
		InputSchema: obj(map[string]any{
			"title":               str("Title (required)."),
			"body":                str("Markdown body."),
			"summary":             str("One-paragraph summary."),
			"project":             str("Project; defaults to the source task's project."),
			"tags":                map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Tags (max 20)."},
			"source_task_id":      str("Source board task id. Required for workers."),
			"source_artifact_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Artifacts that back this entry; must belong to source_task_id."},
			"source_paths":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Path labels only; the server never reads them."},
			"confidence":          map[string]any{"type": "integer", "description": "0-100."},
			"sensitivity":         str("public | internal | sensitive | secret."),
			"status":              str("draft (default) or approved (main agent only)."),
		}, "title"),
	},
	{
		Name:        "stop_session",
		Description: "Interrupt the running turn on a session (does not delete it).",
		InputSchema: obj(map[string]any{
			"id": str("Session id."),
		}, "id"),
	},
}

// BoardTools are the main-agent board tools. They are appended to Tools only
// when the spawning session is the workspace main agent (env
// CC_WEBUI_A2A_BOARD=1), so a worker's tool list stays lean and a worker can
// never write the board even if it guesses a tool name — the server re-checks
// the caller's main_agent flag on every call.
var BoardTools = []Tool{
	{
		Name:        "board_list",
		Description: "List the main-agent board tasks (cards): id, title, status, project, worker, tags, progress and last event. Optionally filter by project or status.",
		InputSchema: obj(map[string]any{
			"project": str("Only tasks in this project."),
			"status":  str("Only tasks with this status."),
		}),
	},
	{
		Name:        "board_get",
		Description: "Fetch one board task with its full event timeline.",
		InputSchema: obj(map[string]any{"id": str("Task id.")}, "id"),
	},
	{
		Name:        "board_create",
		Description: "Create a board task (a card). Title is required; id defaults to a timestamp id. Returns the created task.",
		InputSchema: obj(map[string]any{
			"title":       str("Task title (required)."),
			"id":          str("Explicit task id (optional; default T-<timestamp>)."),
			"project":     str("Project / workspace name."),
			"status":      str("Initial status; defaults to 新建."),
			"worker":      str("Bound worker address name."),
			"description": str("Longer description."),
			"owner":       str("Owner."),
			"progress":    str("Progress text."),
			"cwd":         str("Workspace directory."),
			"report_file": str("Legacy single report path (prefer artifact_publish for new artifacts)."),
			"tags":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Tags."},
			"priority":    map[string]any{"type": "integer", "description": "Priority (1 high … 3 low; default 2)."},
		}, "title"),
	},
	{
		Name:        "board_update",
		Description: "Update a board task's fields. Use this to move a card's status, set progress, or archive it.",
		InputSchema: obj(map[string]any{
			"id":            str("Task id (required)."),
			"status":        str("New status (新建 / 进行中 / 待人工 / 待验收 / 已完成 / 已取消)."),
			"progress":      str("Progress text."),
			"last_event":    str("One-line summary of the latest event."),
			"status_reason": str("Reason for the current status."),
			"worker":        str("Bound worker address name."),
			"report_file":   str("Legacy single report path (prefer artifact_publish for new artifacts)."),
			"archived":      map[string]any{"type": "boolean", "description": "Archive (hide from the board)."},
		}, "id"),
	},
	{
		Name:        "board_comment",
		Description: "Append one event/comment line to a task's timeline (dispatch, report, decision, done, cancel, reassign). Idempotent on (kind, text, at).",
		InputSchema: obj(map[string]any{
			"id":     str("Task id (required)."),
			"text":   str("Comment text (required)."),
			"kind":   str("Event kind; defaults to 用户消息."),
			"at":     str("Timestamp 'YYYY-MM-DD HH:MM:SS'; defaults to now."),
			"source": str("Source label; defaults to drawer."),
		}, "id", "text"),
	},
	{
		Name:        "board_bind_worker",
		Description: "Bind a worker (session / address name) to a board task.",
		InputSchema: obj(map[string]any{
			"id":      str("Task id (required)."),
			"worker":  str("Worker address name (required)."),
			"session": str("Worker session id."),
			"cwd":     str("Worker workspace directory."),
		}, "id", "worker"),
	},
}

// KnowledgeAdminTools are main-agent-only knowledge management tools. They are
// advertised only when CC_WEBUI_A2A_KNOWLEDGE_ADMIN=1 and are re-checked by the
// server, the same two-layer gate as the board tools.
var KnowledgeAdminTools = []Tool{
	{
		Name: "knowledge_review",
		Description: "Review a knowledge entry: approve, reject, mark stale, or restore. " +
			"Only the main agent can review; AI-generated drafts are never auto-approved.",
		InputSchema: obj(map[string]any{
			"id":      str("Knowledge entry id (required)."),
			"action":  str("approve | reject | stale | restore | draft."),
			"reason":  str("Review reason / user confirmation quote."),
			"title":   str("Optional edited title."),
			"summary": str("Optional edited summary."),
			"body":    str("Optional edited Markdown body."),
			"tags":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		}, "id", "action"),
	},
	{
		Name: "knowledge_collect",
		Description: "Scan board tasks in 待验收 / 已完成 and generate one draft per task from task metadata and hosted artifact metadata. " +
			"Idempotent per task; never reads a workspace file, transcript, or legacy report_file path.",
		InputSchema: obj(map[string]any{}),
	},
}

// IsKnowledgeAdminTool reports whether name is a main-agent-only knowledge tool.
func IsKnowledgeAdminTool(name string) bool {
	for _, t := range KnowledgeAdminTools {
		if t.Name == name {
			return true
		}
	}
	return false
}

// IsBoardTool reports whether name is one of the main-agent-only board tools.
func IsBoardTool(name string) bool {
	for _, t := range BoardTools {
		if t.Name == name {
			return true
		}
	}
	return false
}

// Content is one MCP content block.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// TextContent wraps s as a single text content block.
func TextContent(s string) []Content { return []Content{{Type: "text", Text: s}} }

// RunMCPProxy is the stdio MCP server an engine spawns as
// `cc-webui -agentdeck-mcp`. It forwards tools/list and tools/call to the
// loopback-only HTTP endpoint with a bearer token, exactly like the browser
// MCP proxy — the agent process never talks to the store directly.
func RunMCPProxy() {
	addr := os.Getenv("CC_WEBUI_A2A_ADDR")
	token := os.Getenv("CC_WEBUI_A2A_TOKEN")
	session := os.Getenv("CC_WEBUI_A2A_SESSION")
	if addr == "" || token == "" {
		fmt.Fprintln(os.Stderr, "agentdeck-mcp: missing CC_WEBUI_A2A_ADDR/TOKEN")
		os.Exit(1)
	}
	client := &http.Client{Timeout: 65 * time.Second}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(line, &req); err != nil {
			writeRPC(out, nil, nil, fmt.Errorf("parse error"))
			continue
		}
		if len(req.ID) == 0 {
			continue // notifications have no reply
		}
		switch req.Method {
		case "initialize":
			writeRPC(out, req.ID, map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "cc-webui-agentdeck", "version": "0.1.0"},
			}, nil)
		case "ping":
			writeRPC(out, req.ID, map[string]any{}, nil)
		case "tools/list":
			tools := append([]Tool(nil), Tools...)
			if os.Getenv("CC_WEBUI_A2A_BOARD") != "" {
				// Main-agent-only board tools ride the same server; the env is set
				// only when the spawning session is the workspace main agent.
				tools = append(tools, BoardTools...)
			}
			if os.Getenv("CC_WEBUI_A2A_KNOWLEDGE_ADMIN") != "" {
				// Knowledge review/collect is main-agent only, same pattern.
				tools = append(tools, KnowledgeAdminTools...)
			}
			writeRPC(out, req.ID, map[string]any{"tools": tools}, nil)
		case "tools/call":
			result, err := postMCP(client, addr, token, session, req.Params)
			if err != nil {
				writeRPC(out, req.ID, map[string]any{
					"content": TextContent(err.Error()),
					"isError": true,
				}, nil)
				continue
			}
			writeRPC(out, req.ID, result, nil)
		default:
			writeRPC(out, req.ID, nil, fmt.Errorf("method not found: %s", req.Method))
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "agentdeck-mcp:", err)
	}
}

func postMCP(client *http.Client, addr, token, session string, params json.RawMessage) (any, error) {
	body, _ := json.Marshal(map[string]any{"method": "tools/call", "params": params})
	url := "http://" + addr + "/api/internal/agentdeck/mcp"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	if session != "" {
		req.Header.Set("X-CC-Webui-Session", session)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("AgentDeck 服务不可达：%v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024*1024))
	var payload struct {
		Result any    `json:"result"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("AgentDeck 服务返回了无效响应")
	}
	if resp.StatusCode >= 400 {
		if payload.Error != "" {
			return nil, fmt.Errorf("%s", payload.Error)
		}
		return nil, fmt.Errorf("AgentDeck 服务 HTTP %d", resp.StatusCode)
	}
	return payload.Result, nil
}

func writeRPC(w *bufio.Writer, id json.RawMessage, result any, rpcErr error) {
	resp := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcErr != nil {
		resp["error"] = map[string]any{"code": -32603, "message": rpcErr.Error()}
	} else {
		resp["result"] = result
	}
	raw, _ := json.Marshal(resp)
	fmt.Fprintf(w, "%s\n", raw)
	_ = w.Flush()
}
