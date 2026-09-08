package runner

import (
	"strings"
	"testing"
)

// enrichOpenCodeError 只在“opencode 通用 server error + 会话目录确认缺失”
// 时追加中文诊断，其余输入原样透出（happy path 与其它失败原因不受影响）。
func TestEnrichOpenCodeError(t *testing.T) {
	generic := "Unexpected server error. Check server logs for details.（ref err_82dd1912）"
	cases := []struct {
		name       string
		errMsg     string
		dir        string
		missing    bool
		wantChange bool
	}{
		{name: "generic+missing", errMsg: generic, dir: "/home/ubuntu/Tools/cc-webui", missing: true, wantChange: true},
		{name: "generic+exists", errMsg: generic, dir: "/tmp", missing: false, wantChange: false},
		{name: "generic+emptydir", errMsg: generic, dir: "", missing: true, wantChange: false},
		{name: "other-error+missing", errMsg: "exit status 1", dir: "/no/such/dir", missing: true, wantChange: false},
		{name: "empty", errMsg: "", dir: "/no/such/dir", missing: true, wantChange: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := enrichOpenCodeError(c.errMsg, c.dir, c.missing)
			if !c.wantChange {
				if got != c.errMsg {
					t.Fatalf("期望原样透出，实际 %q", got)
				}
				return
			}
			if !strings.HasPrefix(got, c.errMsg) {
				t.Fatalf("期望保留原文前缀，实际 %q", got)
			}
			if !strings.Contains(got, c.dir) || !strings.Contains(got, "新建会话") {
				t.Fatalf("诊断信息缺目录/处置建议：%q", got)
			}
		})
	}
}
