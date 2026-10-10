package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// RunPermHelper implements `cc-webui -perm-helper`: the process the CLI spawns
// for every PermissionRequest hook invocation. Stdin carries the hook event
// JSON; stdout must carry the decision JSON. Env carries the session/address/
// token (CC_WEBUI_PERM_*), set at spawn time — the shared --settings file
// holds only the static command line.
//
// Fail-closed: any error (missing env, unreachable server, timeout) yields a
// deny decision with exit code 0, so a broken approval path blocks the tool
// instead of silently allowing it.
func RunPermHelper() {
	if err := runPermHelper(); err != nil {
		// Last resort: still deny cleanly on stdout; diagnostics go to stderr.
		fmt.Fprintf(os.Stderr, "perm-helper: %v\n", err)
		deny("approval helper failed: " + err.Error())
	}
}

func runPermHelper() error {
	sessionID := os.Getenv("CC_WEBUI_PERM_SESSION")
	addr := os.Getenv("CC_WEBUI_PERM_ADDR")
	token := os.Getenv("CC_WEBUI_PERM_TOKEN")
	if sessionID == "" || addr == "" || token == "" {
		deny("approval helper not configured")
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 4<<20))
	if err != nil {
		deny("cannot read hook input")
		return nil
	}
	tool, input, toolUseID := parseHookTool(raw)
	body, _ := json.Marshal(map[string]any{
		"tool": tool, "input": input, "tool_use_id": toolUseID,
	})
	url := "http://" + addr + "/api/sessions/" + sessionID + "/permissions/request"
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		deny("cannot build approval request")
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: PermissionTimeout + 10*time.Second}
	resp, err := client.Do(req)
	if err != nil {
		deny("approval server unreachable")
		return nil
	}
	defer resp.Body.Close()
	respRaw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		deny("cannot read approval response")
		return nil
	}
	var decision struct {
		Behavior string            `json:"behavior"`
		Message  string            `json:"message"`
		Answers  map[string]string `json:"answers"`
	}
	if err := json.Unmarshal(respRaw, &decision); err != nil {
		deny("bad approval response")
		return nil
	}
	if decision.Behavior == "allow" {
		allow(decision.Answers)
		return nil
	}
	msg := decision.Message
	if msg == "" {
		msg = "user denied"
	}
	deny(msg)
	return nil
}

// parseHookTool extracts the tool call from a hook event payload, tolerating
// the key spellings across hook types and CLI versions (PermissionRequest vs
// PreToolUse shapes). Unknown shapes degrade to a generic "tool" label with
// the raw payload as input context — the card still renders and deny still
// holds.
func parseHookTool(raw []byte) (string, map[string]any, string) {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "tool", nil, ""
	}
	str := func(keys ...string) string {
		for _, k := range keys {
			if v, _ := obj[k].(string); v != "" {
				return v
			}
		}
		return ""
	}
	tool := str("tool_name", "tool", "toolName")
	if tool == "" {
		tool = "tool"
	}
	var input map[string]any
	for _, k := range []string{"tool_input", "input", "toolInput"} {
		if m, ok := obj[k].(map[string]any); ok {
			input = m
			break
		}
	}
	toolUseID := str("tool_use_id", "toolUseId", "tool_useId")
	return tool, input, toolUseID
}

func allow(answers map[string]string) {
	decision := map[string]any{"behavior": "allow"}
	// AskUserQuestion answers ride updatedInput: key = question text, value =
	// option label. Without them the CLI reports "the user did not answer".
	if len(answers) > 0 {
		decision["updatedInput"] = map[string]any{"answers": answers}
	}
	out, _ := json.Marshal(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName": "PermissionRequest",
			"decision":      decision,
		},
	})
	fmt.Println(string(out))
}

func deny(msg string) {
	if msg == "" {
		msg = "user denied"
	}
	out, _ := json.Marshal(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName": "PermissionRequest",
			"decision":      map[string]any{"behavior": "deny", "message": msg},
		},
	})
	fmt.Println(string(out))
}
