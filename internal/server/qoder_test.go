package server

// Qoder CLI CN 的模型清单由 CLI 自己给出（`qoderclicn --list-models`），服务端
// 解析后连同推理档位一起下发，前端两个下拉都从这份响应重建。这里钉住解析规则。

import (
	"reflect"
	"testing"
)

// realQoderListModels 是 `qoderclicn --list-models` 的真实输出（1.1.65，登录后）：
// 一个 MODEL 表头，每行一个模型名。
const realQoderListModels = "MODEL\nQwen3.8-Max\nQwen3.8-Flash\n"

func TestParseQoderModelsRealOutput(t *testing.T) {
	got := parseQoderModels(realQoderListModels)
	if len(got) != 2 {
		t.Fatalf("parseQoderModels returned %d models, want 2: %+v", len(got), got)
	}
	if got[0].Name != "Qwen3.8-Max" || got[1].Name != "Qwen3.8-Flash" {
		t.Fatalf("names = %q, %q", got[0].Name, got[1].Name)
	}
	// 顺序保留：CLI 按它自己的偏好排序，前端「默认 (首个)」就靠它。
	// 输入模态声明 text+image（Flash 实测能读 base64 图），CLI 不提供该元数据。
	for _, m := range got {
		if !reflect.DeepEqual(m.Input, []string{"text", "image"}) {
			t.Errorf("%s input = %v, want [text image]", m.Name, m.Input)
		}
	}
}

func TestParseQoderModelsSkipsHeaderAndBlanks(t *testing.T) {
	got := parseQoderModels("MODEL\n\n  Qwen3.8-Flash  \n\n")
	if len(got) != 1 || got[0].Name != "Qwen3.8-Flash" {
		t.Fatalf("got %+v, want one trimmed model", got)
	}
}

// 一旦 CLI 将来给这张表加列（如上下文窗口/额度），不能把整行当模型名。
func TestParseQoderModelsTakesFirstColumnOnly(t *testing.T) {
	got := parseQoderModels("MODEL  CONTEXT\nQwen3.8-Flash  1000000\n")
	if len(got) != 1 || got[0].Name != "Qwen3.8-Flash" {
		t.Fatalf("got %+v, want only the first column", got)
	}
}

// 去重：同一行重复不该让下拉出现两项同名模型（选中值按名字匹配，重复项会
// 让 fillModelSelect 的回填落到最后一项）。
func TestParseQoderModelsDedupes(t *testing.T) {
	got := parseQoderModels("MODEL\nQwen3.8-Flash\nQwen3.8-Flash\n")
	if len(got) != 1 {
		t.Fatalf("got %d models, want 1: %+v", len(got), got)
	}
}

// 未登录/异常输出（空、只有表头、纯 usage 文本）必须解析成空切片，调用方
// 据此报错，而不是把一张空清单当正常结果缓存 10 分钟。
func TestParseQoderModelsEmptyForNonTableOutput(t *testing.T) {
	for _, out := range []string{"", "\n\n", "MODEL\n", "error: not logged in\n"} {
		if got := parseQoderModels(out); len(got) != 0 {
			t.Errorf("parseQoderModels(%q) = %+v, want empty", out, got)
		}
	}
}

// 档位集合是 CLI 侧的固定值（--reasoning-effort 认这些），由 /api/qoder-models
// 连同模型一起下发，前端据此重建下拉（不再自带副本）。改动它等于改 CLI 契约，
// 这里钉住当前集合。
func TestQoderEffortsServedToFrontend(t *testing.T) {
	want := []string{"auto", "none", "low", "medium", "high", "xhigh", "max", "ultracode"}
	if !reflect.DeepEqual(qoderEfforts, want) {
		t.Fatalf("qoderEfforts = %v, want %v", qoderEfforts, want)
	}
	// 与服务端校验用的是同一份列表（下发什么就只允许什么）。
	for _, lv := range qoderEfforts {
		if !validQoderEffort(lv) {
			t.Errorf("validQoderEffort(%q) = false, want true", lv)
		}
	}
}
