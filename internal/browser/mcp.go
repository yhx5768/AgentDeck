package browser

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// RunMCPProxy is the stdio MCP server a Claude/Qoder turn spawns as
// `cc-webui -browser-mcp`. It forwards tools/list and tools/call to the
// loopback-only HTTP endpoint with a bearer token; the browser itself never
// sees the agent process.
func RunMCPProxy() {
	session := os.Getenv("CC_WEBUI_BROWSER_SESSION")
	addr := os.Getenv("CC_WEBUI_BROWSER_ADDR")
	token := os.Getenv("CC_WEBUI_BROWSER_TOKEN")
	if session == "" || addr == "" || token == "" {
		fmt.Fprintln(os.Stderr, "browser-mcp: missing CC_WEBUI_BROWSER_SESSION/ADDR/TOKEN")
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
			// notifications/initialized and other notifications have no reply
			continue
		}
		switch req.Method {
		case "initialize":
			writeRPC(out, req.ID, map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "cc-webui-browser", "version": "0.1.0"},
			}, nil)
		case "ping":
			writeRPC(out, req.ID, map[string]any{}, nil)
		case "tools/list":
			writeRPC(out, req.ID, map[string]any{"tools": Tools}, nil)
		case "tools/call":
			result, err := postMCP(client, addr, token, session, req.Params)
			if err != nil {
				writeRPC(out, req.ID, map[string]any{
					"content": []Content{{Type: "text", Text: err.Error()}},
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
		fmt.Fprintln(os.Stderr, "browser-mcp:", err)
	}
}

func postMCP(client *http.Client, addr, token, session string, params json.RawMessage) (any, error) {
	body, _ := json.Marshal(map[string]any{"method": "tools/call", "params": params})
	url := "http://" + addr + "/api/internal/browser/mcp"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-CC-Webui-Session", session)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("浏览器服务不可达：%v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024*1024))
	var payload struct {
		Result any    `json:"result"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("浏览器服务返回了无效响应")
	}
	if resp.StatusCode >= 400 {
		if payload.Error != "" {
			return nil, fmt.Errorf("%s", payload.Error)
		}
		return nil, fmt.Errorf("浏览器服务 HTTP %d", resp.StatusCode)
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

// RPC serves one internal MCP request. The caller has already authenticated
// the session and confirmed the request came from loopback.
func (m *Manager) RPC(ctx context.Context, sessionID, method string, params json.RawMessage) (any, error) {
	switch method {
	case "tools/list":
		return map[string]any{"tools": Tools}, nil
	case "tools/call":
		var call struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(params, &call); err != nil {
			return nil, fmt.Errorf("invalid tools/call params")
		}
		if call.Name == "" {
			return nil, fmt.Errorf("missing tool name")
		}
		return m.Execute(ctx, sessionID, call.Name, call.Arguments), nil
	default:
		return nil, fmt.Errorf("method not found: %s", method)
	}
}
