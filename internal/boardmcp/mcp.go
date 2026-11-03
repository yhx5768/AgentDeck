// Package boardmcp exposes the AgentDeck task board as MCP stdio tools.
//
// The process is spawned by an agent CLI (claude/qoder/opencode/codex/ducx)
// and talks to the running cc-webui HTTP API. It reads the gateway URL and
// token from config.yaml itself; neither value is placed in tool schemas,
// prompts, or model-visible output.
package boardmcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cc-webui/internal/config"
)

// Server is one board API client.
type Server struct {
	BaseURL string
	Token   string
	Client  *http.Client
}

// NewFromConfig loads the same config the cc-webui server uses.
func NewFromConfig(path string) (*Server, error) {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	if strings.TrimSpace(cfg.GatewayToken) == "" || cfg.TokenGenerated {
		return nil, fmt.Errorf("config %s has no explicit gateway_token; board MCP needs one", path)
	}
	return New(loopbackURL(cfg.Listen), cfg.GatewayToken), nil
}

// New builds a client from an explicit URL and token (tests and embedding).
func New(baseURL, token string) *Server {
	return &Server{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		Client:  &http.Client{Timeout: 20 * time.Second},
	}
}

func loopbackURL(listen string) string {
	listen = strings.TrimSpace(listen)
	if strings.HasPrefix(listen, "http://") || strings.HasPrefix(listen, "https://") {
		return strings.TrimRight(listen, "/")
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://" + listen
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// Run is the `cc-webui -board-mcp` stdio server.
func Run(configPath string) {
	srv, err := NewFromConfig(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "board-mcp:", err)
		os.Exit(1)
	}
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
			continue
		}
		switch req.Method {
		case "initialize":
			writeRPC(out, req.ID, map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "cc-webui-board", "version": "0.1.0"},
			}, nil)
		case "ping":
			writeRPC(out, req.ID, map[string]any{}, nil)
		case "tools/list":
			writeRPC(out, req.ID, map[string]any{"tools": Tools}, nil)
		case "tools/call":
			var call struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &call); err != nil || call.Name == "" {
				writeRPC(out, req.ID, toolError("invalid tools/call params"), nil)
				continue
			}
			result, err := srv.Call(call.Name, call.Arguments)
			if err != nil {
				writeRPC(out, req.ID, toolError(err.Error()), nil)
				continue
			}
			writeRPC(out, req.ID, toolResult(result), nil)
		default:
			writeRPC(out, req.ID, nil, fmt.Errorf("method not found: %s", req.Method))
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "board-mcp:", err)
	}
}

func toolResult(value any) map[string]any {
	text, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		text = []byte(fmt.Sprint(value))
	}
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(text)}},
		"isError": false,
	}
}

func toolError(message string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": message}},
		"isError": true,
	}
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

// Call dispatches one tool. It is exported so tests and other embeddings can
// use the same handlers without a stdio loop.
func (s *Server) Call(name string, args map[string]any) (any, error) {
	switch name {
	case "board_task_create":
		return s.request(http.MethodPost, "/api/agent-board/tasks", args)
	case "board_task_update":
		taskID, err := taskID(args)
		if err != nil {
			return nil, err
		}
		patch := copyWithout(args, "task_id")
		if len(patch) == 0 {
			return nil, fmt.Errorf("board_task_update 至少需要一个更新字段")
		}
		return s.request(http.MethodPatch, "/api/agent-board/tasks/"+url.PathEscape(taskID), patch)
	case "board_task_event":
		taskID, err := taskID(args)
		if err != nil {
			return nil, err
		}
		body := copyWithout(args, "task_id")
		return s.request(http.MethodPost,
			"/api/agent-board/tasks/"+url.PathEscape(taskID)+"/events", body)
	case "board_task_bind":
		taskID, err := taskID(args)
		if err != nil {
			return nil, err
		}
		body := copyWithout(args, "task_id")
		if argString(body, "worker") == "" {
			return nil, fmt.Errorf("board_task_bind 需要 worker")
		}
		return s.request(http.MethodPost,
			"/api/agent-board/tasks/"+url.PathEscape(taskID)+"/worker", body)
	case "board_task_get":
		taskID, err := taskID(args)
		if err != nil {
			return nil, err
		}
		return s.request(http.MethodGet, "/api/agent-board/tasks/"+url.PathEscape(taskID), nil)
	case "board_task_list":
		return s.listTasks(args)
	case "board_worker_list":
		return s.request(http.MethodGet, "/api/agent-board/workers", nil)
	case "board_snapshot":
		return s.request(http.MethodGet, "/api/agent-board", nil)
	case "board_backup":
		return s.request(http.MethodPost, "/api/agent-board/backup", nil)
	default:
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
}

func (s *Server) listTasks(args map[string]any) (any, error) {
	snapshot, err := s.request(http.MethodGet, "/api/agent-board", nil)
	if err != nil {
		return nil, err
	}
	payload, _ := snapshot.(map[string]any)
	tasks, _ := payload["tasks"].([]any)
	filtered := make([]any, 0, len(tasks))
	for _, raw := range tasks {
		task, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if !matches(task, "status", argString(args, "status")) ||
			!matches(task, "project", argString(args, "project")) ||
			!matches(task, "worker", argString(args, "worker")) {
			continue
		}
		filtered = append(filtered, task)
	}
	if limit, ok := numberArg(args, "limit"); ok && limit > 0 && len(filtered) > limit {
		filtered = filtered[:limit]
	}
	if truthy(args["summary"]) {
		return map[string]any{
			"tasks": len(filtered), "counts": payload["counts"],
			"projects": payload["projects"], "source": payload["source"],
		}, nil
	}
	return map[string]any{
		"tasks": filtered, "counts": payload["counts"], "source": payload["source"],
	}, nil
}

func matches(task map[string]any, key, want string) bool {
	return want == "" || fmt.Sprint(task[key]) == want
}

func taskID(args map[string]any) (string, error) {
	id := argString(args, "task_id")
	if id == "" {
		return "", fmt.Errorf("需要 task_id")
	}
	return id, nil
}

func argString(args map[string]any, key string) string {
	if value, ok := args[key]; ok && value != nil {
		return strings.TrimSpace(fmt.Sprint(value))
	}
	return ""
}

func numberArg(args map[string]any, key string) (int, bool) {
	value, ok := args[key]
	if !ok {
		return 0, false
	}
	switch typed := value.(type) {
	case float64:
		return int(typed), true
	case int:
		return typed, true
	case json.Number:
		n, err := typed.Int64()
		return int(n), err == nil
	}
	return 0, false
}

func truthy(value any) bool {
	typed, ok := value.(bool)
	return ok && typed
}

func copyWithout(args map[string]any, key string) map[string]any {
	out := make(map[string]any, len(args))
	for name, value := range args {
		if name != key {
			out[name] = value
		}
	}
	return out
}

func (s *Server) request(method, path string, body any) (any, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, s.BaseURL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连接 AgentDeck 失败：%v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32*1024*1024))
	var payload any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &payload)
	}
	if resp.StatusCode >= 400 {
		message := strings.TrimSpace(string(raw))
		if item, ok := payload.(map[string]any); ok {
			if text, ok := item["error"].(string); ok {
				message = text
			}
		}
		return nil, fmt.Errorf("看板 API HTTP %d: %s", resp.StatusCode, message)
	}
	return payload, nil
}
