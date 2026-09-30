package runner

// qoder 引擎与 claude 引擎共用一条 spawn 路径（runStreamJSONTurn），差别只在
// argv 拼法。这些差异是实测 qoderclicn 1.1.65 得到的：--verbose 与 --effort
// 都被它拒收（exit 1，"error: unknown option"），档位 flag 叫
// --reasoning-effort，--permission-mode 收下划线拼写。拼错一个 flag 就是整轮
//  spawn 失败，所以在这里钉住。

import (
	"testing"

	"cc-webui/internal/config"
)

func qoderCLI() *config.CLI { return &config.CLI{Name: "qoder", Kind: config.KindQoder} }

func claudeCLI() *config.CLI { return &config.CLI{Name: "claude", Kind: config.KindClaude} }

func TestDialectForSelectsQoder(t *testing.T) {
	if got := dialectFor(qoderCLI()); got.effortFlag != "--reasoning-effort" {
		t.Errorf("qoder effortFlag = %q", got.effortFlag)
	}
	if got := dialectFor(claudeCLI()); got.effortFlag != "--effort" || !got.verbose {
		t.Errorf("claude dialect = %+v, want --effort with verbose", got)
	}
	// 未识别的 kind 走 claude 拼法（缺省 kind 就是 claude）。
	if got := dialectFor(&config.CLI{Name: "x"}); got.effortFlag != "--effort" {
		t.Errorf("default dialect = %+v, want claude", got)
	}
}

// qoder 没有 --verbose：多传一个就整轮失败。
func TestQoderDialectOmitsVerbose(t *testing.T) {
	if qoderDialect.verbose {
		t.Error("qoderclicn has no --verbose flag")
	}
	if !claudeDialect.verbose {
		t.Error("claude requires --verbose alongside stream-json")
	}
}

// 档位 flag 名字不同；qoder 也不吃 CLAUDE_CODE_EFFORT_LEVEL 这条 env 通道
// （只有 --reasoning-effort 是已验证的口子），所以 effortEnv 关掉。
func TestQoderDialectEffortChannel(t *testing.T) {
	if qoderDialect.effortEnv {
		t.Error("qoder must not mirror effort into the process env")
	}
	if !claudeDialect.effortEnv {
		t.Error("claude's CLAUDE_CODE_EFFORT_LEVEL mirror is load-bearing")
	}
	// qoder 没有 --rewind-files，captureCheckpoint 必须关掉：否则会给消息挂上
	// 一个 rewind 按钮，点了却回滚不了文件。
	if qoderDialect.checkpoint {
		t.Error("qoder sessions must not advertise rewindable checkpoints")
	}
	if !claudeDialect.checkpoint {
		t.Error("claude must keep capturing checkpoints")
	}
}

func TestSnakeCasePermissionMode(t *testing.T) {
	cases := map[string]string{
		"bypassPermissions":  "bypass_permissions",
		"acceptEdits":        "accept_edits",
		"dontAsk":            "dont_ask",
		"default":            "default",            // already lower case
		"auto":               "auto",               // qoder-only spelling, passes through
		"bypass_permissions": "bypass_permissions", // already snake
		"":                   "",
	}
	for in, want := range cases {
		if got := snakeCasePermissionMode(in); got != want {
			t.Errorf("snakeCasePermissionMode(%q) = %q, want %q", in, got, want)
		}
	}
	// claude 侧的 dialect 原样透传，不做改写。
	if got := claudeDialect.permissionMode("bypassPermissions"); got != "bypassPermissions" {
		t.Errorf("claude permissionMode = %q, want unchanged", got)
	}
}
