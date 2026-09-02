package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ResolveCwd 展开 ~ 并校验目录存在；缺失的 cmd.Dir 会让 cmd.Start 报出
// 误导性的 "fork/exec <binary>: no such file or directory"（怪罪二进制而非
// 目录），所以这里必须把真正的错误（目录不存在/非目录）报清楚。
func TestResolveCwd(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("os.UserHomeDir: %v", err)
	}
	fileOK := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(fileOK, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		dir     string
		wantErr string // 期望错误信息包含的子串；空串表示期望成功
		wantOK  string // 成功时期望返回值（为空则只校验非空/无错）
	}{
		{name: "empty", dir: "", wantErr: "不能为空"},
		{name: "nonexistent", dir: "/no/such/path/xyz", wantErr: "不存在"},
		{name: "tilde-only", dir: "~", wantOK: home},
		{name: "tilde-slash-nonexistent", dir: "~/no/such/here/zzz", wantErr: "不存在"},
		{name: "file-not-dir", dir: fileOK, wantErr: "不是目录"},
		{name: "real-dir", dir: t.TempDir()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ResolveCwd(c.dir)
			switch {
			case c.wantErr != "":
				if err == nil {
					t.Fatalf("ResolveCwd(%q) 期望出错（含 %q），实际成功返回 %q", c.dir, c.wantErr, got)
				}
				if !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("ResolveCwd(%q) 错误 %q 不含 %q", c.dir, err.Error(), c.wantErr)
				}
			default:
				if err != nil {
					t.Fatalf("ResolveCwd(%q) 意外出错: %v", c.dir, err)
				}
				if c.wantOK != "" && got != c.wantOK {
					t.Fatalf("ResolveCwd(%q) = %q, 期望 %q", c.dir, got, c.wantOK)
				}
			}
		})
	}
}
