package browser

import (
	"context"
	"strings"
	"testing"

	"cc-webui/internal/config"
)

func TestValidateURLPolicy(t *testing.T) {
	cfg := config.BrowserConfig{AllowLocal: false}
	cases := []struct {
		name    string
		url     string
		wantErr string
	}{
		{"blank", "about:blank", ""},
		{"file", "file:///etc/passwd", "http/https"},
		{"javascript", "javascript:alert(1)", "http/https"},
		{"metadata v4", "http://169.254.169.254/latest/meta-data/", "metadata"},
		{"metadata aliyun", "http://100.100.100.200/latest/meta-data/", "metadata"},
		{"loopback blocked", "http://127.0.0.1:18822/", "内网/本机"},
		{"localhost blocked", "http://localhost:18822/", "本机地址"},
	}
	for _, tc := range cases {
		_, err := ValidateURL(context.Background(), tc.url, cfg, "")
		if tc.wantErr == "" {
			if err != nil {
				t.Fatalf("%s: unexpected error: %v", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Fatalf("%s: error = %v, want substring %q", tc.name, err, tc.wantErr)
		}
	}
}

func TestValidateURLAllowsLocalWhenConfigured(t *testing.T) {
	cfg := config.BrowserConfig{AllowLocal: true}
	if _, err := ValidateURL(context.Background(), "http://127.0.0.1:18822/", cfg, "127.0.0.1:18821"); err != nil {
		t.Fatalf("local page should be allowed: %v", err)
	}
	if _, err := ValidateURL(context.Background(), "http://127.0.0.1:18821/api/config", cfg, "127.0.0.1:18821"); err == nil || !strings.Contains(err.Error(), "自己的端口") {
		t.Fatalf("own port should be refused, got %v", err)
	}
}

func TestValidateURLAllowHosts(t *testing.T) {
	cfg := config.BrowserConfig{AllowHosts: []string{"127.0.0.1:18822"}}
	if _, err := ValidateURL(context.Background(), "http://127.0.0.1:18822/", cfg, "127.0.0.1:18821"); err != nil {
		t.Fatalf("allow_hosts should permit 18822: %v", err)
	}
}

func TestProfileKey(t *testing.T) {
	if got := profileKey("../a b"); got != ".._a_b" {
		t.Fatalf("profileKey = %q", got)
	}
	if len(profileKey(strings.Repeat("x", 200))) > 80 {
		t.Fatal("profileKey not capped")
	}
}
