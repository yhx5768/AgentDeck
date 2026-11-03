package server

import (
	"net/http/httptest"
	"testing"
)

func TestSameOrigin(t *testing.T) {
	tests := []struct {
		name           string
		remote         string
		host           string
		headers        map[string]string
		trusted        []string
		trustForwarded bool
		want           bool
	}{
		{
			name:   "no origin is allowed for non-browser clients",
			remote: "203.0.113.9:4444",
			host:   "ccview.xpsl.soarx.cc",
			want:   true,
		},
		{
			name:    "direct http same host",
			remote:  "203.0.113.9:4444",
			host:    "xpsl.soarx.cc:8321",
			headers: map[string]string{"Origin": "http://xpsl.soarx.cc:8321"},
			want:    true,
		},
		{
			name:    "direct https origin behind tls termination",
			remote:  "203.0.113.9:4444",
			host:    "xpsl.soarx.cc:8321",
			headers: map[string]string{"Origin": "https://xpsl.soarx.cc:8321"},
			want:    true,
		},
		{
			name:    "direct different host is refused",
			remote:  "203.0.113.9:4444",
			host:    "xpsl.soarx.cc:8321",
			headers: map[string]string{"Origin": "https://evil.example"},
			want:    false,
		},
		{
			name:   "proxy host rewrite without forwarded headers is refused",
			remote: "127.0.0.1:5000",
			host:   "127.0.0.1:8321",
			headers: map[string]string{
				"Origin": "https://ccview.xpsl.soarx.cc",
			},
			want: false,
		},
		{
			name:   "forwarded headers are ignored unless explicitly trusted",
			remote: "127.0.0.1:5000",
			host:   "127.0.0.1:8321",
			headers: map[string]string{
				"Origin":            "https://ccview.xpsl.soarx.cc",
				"X-Forwarded-Host":  "ccview.xpsl.soarx.cc",
				"X-Forwarded-Proto": "https",
			},
			trustForwarded: false,
			want:           false,
		},
		{
			name:   "proxy forwarded host and proto are accepted",
			remote: "127.0.0.1:5000",
			host:   "127.0.0.1:8321",
			headers: map[string]string{
				"Origin":            "https://ccview.xpsl.soarx.cc",
				"X-Forwarded-Host":  "ccview.xpsl.soarx.cc",
				"X-Forwarded-Proto": "https",
			},
			trustForwarded: true,
			want:           true,
		},
		{
			name:   "proxy forwarded values use the first list entry",
			remote: "127.0.0.1:5000",
			host:   "127.0.0.1:8321",
			headers: map[string]string{
				"Origin":            "https://ccview.xpsl.soarx.cc",
				"X-Forwarded-Host":  "ccview.xpsl.soarx.cc, inner.proxy:8080",
				"X-Forwarded-Proto": "https, http",
			},
			trustForwarded: true,
			want:           true,
		},
		{
			name:   "proxy scheme mismatch is refused",
			remote: "127.0.0.1:5000",
			host:   "127.0.0.1:8321",
			headers: map[string]string{
				"Origin":            "https://ccview.xpsl.soarx.cc",
				"X-Forwarded-Host":  "ccview.xpsl.soarx.cc",
				"X-Forwarded-Proto": "http",
			},
			trustForwarded: true,
			want:           false,
		},
		{
			name:   "forwarded headers from a remote peer are not trusted",
			remote: "203.0.113.9:4444",
			host:   "127.0.0.1:8321",
			headers: map[string]string{
				"Origin":            "https://ccview.xpsl.soarx.cc",
				"X-Forwarded-Host":  "ccview.xpsl.soarx.cc",
				"X-Forwarded-Proto": "https",
			},
			trustForwarded: true,
			want:           false,
		},
		{
			name:   "configured trusted origin works without forwarded headers",
			remote: "203.0.113.9:4444",
			host:   "127.0.0.1:8321",
			headers: map[string]string{
				"Origin": "https://ccview.xpsl.soarx.cc",
			},
			trusted: []string{"https://ccview.xpsl.soarx.cc"},
			want:    true,
		},
		{
			name:   "trusted origin accepts a trailing slash in config",
			remote: "203.0.113.9:4444",
			host:   "127.0.0.1:8321",
			headers: map[string]string{
				"Origin": "https://ccview.xpsl.soarx.cc",
			},
			trusted: []string{"https://ccview.xpsl.soarx.cc/"},
			want:    true,
		},
		{
			name:   "trusted origin scheme must match",
			remote: "203.0.113.9:4444",
			host:   "127.0.0.1:8321",
			headers: map[string]string{
				"Origin": "https://ccview.xpsl.soarx.cc",
			},
			trusted: []string{"http://ccview.xpsl.soarx.cc"},
			want:    false,
		},
		{
			name:   "trusted origin with a path is ignored",
			remote: "203.0.113.9:4444",
			host:   "127.0.0.1:8321",
			headers: map[string]string{
				"Origin": "https://ccview.xpsl.soarx.cc",
			},
			trusted: []string{"https://ccview.xpsl.soarx.cc/api"},
			want:    false,
		},
		{
			name:   "trusted origin does not cover another host",
			remote: "203.0.113.9:4444",
			host:   "127.0.0.1:8321",
			headers: map[string]string{
				"Origin": "https://evil.example",
			},
			trusted: []string{"https://ccview.xpsl.soarx.cc"},
			want:    false,
		},
		{
			name:   "default https port matches an explicit port",
			remote: "127.0.0.1:5000",
			host:   "127.0.0.1:8321",
			headers: map[string]string{
				"Origin":            "https://ccview.xpsl.soarx.cc",
				"X-Forwarded-Host":  "ccview.xpsl.soarx.cc:443",
				"X-Forwarded-Proto": "https",
			},
			trustForwarded: true,
			want:           true,
		},
		{
			name:   "wrong forwarded port is refused",
			remote: "127.0.0.1:5000",
			host:   "127.0.0.1:8321",
			headers: map[string]string{
				"Origin":            "https://ccview.xpsl.soarx.cc:8443",
				"X-Forwarded-Host":  "ccview.xpsl.soarx.cc:9443",
				"X-Forwarded-Proto": "https",
			},
			trustForwarded: true,
			want:           false,
		},
		{
			name:    "origin with a path is refused",
			remote:  "127.0.0.1:5000",
			host:    "127.0.0.1:8321",
			headers: map[string]string{"Origin": "https://ccview.xpsl.soarx.cc/api"},
			want:    false,
		},
		{
			name:    "null origin is refused",
			remote:  "127.0.0.1:5000",
			host:    "127.0.0.1:8321",
			headers: map[string]string{"Origin": "null"},
			want:    false,
		},
		{
			name:    "malformed origin is refused",
			remote:  "127.0.0.1:5000",
			host:    "127.0.0.1:8321",
			headers: map[string]string{"Origin": "https://"},
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "http://"+tt.host+"/api/sessions/x/browser/input", nil)
			req.Host = tt.host
			req.RemoteAddr = tt.remote
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			if got := sameOrigin(req, tt.trusted, tt.trustForwarded); got != tt.want {
				t.Fatalf("sameOrigin() = %v, want %v", got, tt.want)
			}
		})
	}
}
