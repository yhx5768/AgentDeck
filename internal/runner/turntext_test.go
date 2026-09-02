package runner

import (
	"strings"
	"testing"
)

// turnText 拼出真正发往 CLI 的文本；存储的用户事件、stdin 载荷和
// captureCheckpoint 的匹配文本都靠它保持一致——改格式前先看这三处。
func TestTurnText(t *testing.T) {
	files := []UploadedFile{
		{Name: "a.pdf", Path: "/data/uploads/s1/a.pdf"},
		{Name: "b.docx", Path: "/data/uploads/s1/b.docx"},
	}
	cases := []struct {
		name  string
		msg   Message
		check func(t *testing.T, out string)
	}{
		{
			name: "text only",
			msg:  Message{Text: "hello"},
			check: func(t *testing.T, out string) {
				if out != "hello" {
					t.Fatalf("got %q", out)
				}
			},
		},
		{
			name: "text plus files",
			msg:  Message{Text: "看下这个", Files: files},
			check: func(t *testing.T, out string) {
				if !strings.HasPrefix(out, "看下这个\n\n（用户上传了 2 个文件") {
					t.Fatalf("bad prefix: %q", out)
				}
				if !strings.Contains(out, "- /data/uploads/s1/a.pdf") ||
					!strings.Contains(out, "- /data/uploads/s1/b.docx") {
					t.Fatalf("paths missing: %q", out)
				}
			},
		},
		{
			name: "files only",
			msg:  Message{Files: files},
			check: func(t *testing.T, out string) {
				if strings.TrimSpace(out) == "" || strings.Contains(out, "（用户上传了\n") {
					t.Fatalf("unexpected: %q", out)
				}
				if !strings.HasPrefix(strings.TrimSpace(out), "（用户上传了 2 个文件") {
					t.Fatalf("bad start: %q", out)
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.check(t, turnText(c.msg)) })
	}
}
