package browser

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"cc-webui/internal/config"
)

var blockedMetadata = map[string]bool{
	"169.254.169.254": true, // AWS/GCP/Azure metadata
	"100.100.100.200": true, // Alibaba Cloud metadata
	"fd00:ec2::254":   true, // AWS IPv6 metadata
}

func hostAllowedByList(host string, allow []string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	for _, entry := range allow {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" {
			continue
		}
		if entry == host || strings.TrimSuffix(entry, ":") == host {
			return true
		}
		if h, _, err := net.SplitHostPort(entry); err == nil && strings.EqualFold(h, host) {
			return true
		}
	}
	return false
}

func privateOrLocal(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// ValidateURL is the PoC's URL policy. It allows only http(s) and
// about:blank; it resolves the host before navigation and refuses private,
// loopback, link-local and cloud-metadata addresses unless explicitly allowed.
// selfListen is the AgentDeck listen address; its own port is always refused.
func ValidateURL(ctx context.Context, raw string, cfg config.BrowserConfig, selfListen string) (string, error) {
	return validateWithSelf(ctx, raw, policyConfig{Browser: cfg, SelfListen: selfListen})
}

// policyConfig is the subset of config plus the manager's own listen address.
type policyConfig struct {
	Browser    config.BrowserConfig
	SelfListen string
}

func validateWithSelf(ctx context.Context, raw string, cfg policyConfig) (string, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return "", fmt.Errorf("地址为空")
	}
	if text == "about:blank" {
		return text, nil
	}
	lower := strings.ToLower(text)
	knownScheme := strings.Contains(text, "://") ||
		strings.HasPrefix(lower, "about:") || strings.HasPrefix(lower, "file:") ||
		strings.HasPrefix(lower, "javascript:") || strings.HasPrefix(lower, "chrome:") ||
		strings.HasPrefix(lower, "data:")
	if !knownScheme {
		text = "https://" + text
	}
	u, err := url.Parse(text)
	if err != nil {
		return "", fmt.Errorf("地址无效: %v", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	case "about":
		if u.String() == "about:blank" {
			return "about:blank", nil
		}
		return "", fmt.Errorf("只允许 about:blank")
	default:
		return "", fmt.Errorf("只允许 http/https 地址，拒绝 %s", u.Scheme)
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("地址没有主机名")
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	selfHost, selfPort, _ := net.SplitHostPort(cfg.SelfListen)
	if selfPort == "" {
		if p, err := strconv.Atoi(strings.TrimPrefix(cfg.SelfListen, ":")); err == nil {
			selfPort = strconv.Itoa(p)
		}
	}
	if selfPort != "" && port == selfPort {
		if host == "127.0.0.1" || host == "localhost" || host == "::1" || host == "0.0.0.0" || host == selfHost {
			return "", fmt.Errorf("拒绝访问 AgentDeck 自己的端口 %s", port)
		}
	}
	allowLocal := cfg.Browser.AllowLocal || hostAllowedByList(host, cfg.Browser.AllowHosts)
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		if !allowLocal {
			return "", fmt.Errorf("本机地址默认被拒绝；如确需访问，请开启 browser.allow_local 或加入 browser.allow_hosts")
		}
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIPAddr(lookupCtx, host)
	if err != nil {
		return "", fmt.Errorf("无法解析主机 %s: %v", host, err)
	}
	for _, entry := range ips {
		ip := entry.IP
		if blockedMetadata[ip.String()] {
			return "", fmt.Errorf("拒绝访问云 metadata 地址 %s", ip)
		}
		if privateOrLocal(ip) && !allowLocal {
			return "", fmt.Errorf("拒绝访问内网/本机地址 %s；如确需访问，请开启 browser.allow_local 或加入 browser.allow_hosts", ip)
		}
	}
	return u.String(), nil
}
