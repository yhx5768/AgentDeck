package browser

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// InstallPlan is what the Settings dialog shows when Chrome is missing. Auto
// is true only for a command this process is willing to run after the user
// confirms; everything else is a copyable official command.
type InstallPlan struct {
	Platform string   `json:"platform"`
	Summary  string   `json:"summary"`
	Commands []string `json:"commands,omitempty"`
	URL      string   `json:"url,omitempty"`
	Auto     bool     `json:"auto"`
	Reason   string   `json:"reason,omitempty"`
}

// ChromeInfo is the result of looking for a local Chromium-family browser.
type ChromeInfo struct {
	Path   string `json:"path,omitempty"`
	Source string `json:"source,omitempty"`
}

var chromeCandidates = map[string][]string{
	"darwin": {
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
		"~/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"~/Applications/Chromium.app/Contents/MacOS/Chromium",
	},
	"linux": {
		"/usr/bin/google-chrome",
		"/usr/bin/google-chrome-stable",
		"/usr/bin/chromium",
		"/usr/bin/chromium-browser",
		"/snap/bin/chromium",
		"google-chrome",
		"google-chrome-stable",
		"chromium",
		"chromium-browser",
	},
	"windows": {
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files\Chromium\Application\chrome.exe`,
	},
}

func executableFile(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && (runtime.GOOS == "windows" || info.Mode()&0111 != 0)
}

// DetectChrome looks only at local paths. The PoC never attaches to a remote
// CDP endpoint; the browser is always a local Chrome process.
func DetectChrome(override string) ChromeInfo {
	if strings.TrimSpace(override) != "" {
		if executableFile(override) {
			return ChromeInfo{Path: override, Source: "config"}
		}
		if p, err := exec.LookPath(override); err == nil && executableFile(p) {
			return ChromeInfo{Path: p, Source: "config"}
		}
	}
	for _, candidate := range chromeCandidates[runtime.GOOS] {
		path := candidate
		if strings.HasPrefix(path, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				path = filepath.Join(home, path[2:])
			}
		}
		if executableFile(path) {
			return ChromeInfo{Path: path, Source: "default path"}
		}
		if !strings.Contains(path, string(os.PathSeparator)) {
			if p, err := exec.LookPath(path); err == nil && executableFile(p) {
				return ChromeInfo{Path: p, Source: "PATH"}
			}
		}
	}
	return ChromeInfo{}
}

// InstallPlanFor returns a platform-specific, explicit install path. Auto is
// deliberately conservative: only Homebrew on macOS is executed after the
// user confirms. Linux and Windows get official commands/URLs instead of a
// silent package-manager invocation.
func InstallPlanFor() InstallPlan {
	switch runtime.GOOS {
	case "darwin":
		if brew, err := exec.LookPath("brew"); err == nil {
			return InstallPlan{
				Platform: "darwin",
				Summary:  "未找到 Chrome。可用 Homebrew 安装 Google Chrome（用户确认后执行，可用 brew uninstall --cask google-chrome 回退）。",
				Commands: []string{brew + " install --cask google-chrome"},
				URL:      "https://www.google.com/chrome/",
				Auto:     true,
			}
		}
		return InstallPlan{
			Platform: "darwin",
			Summary:  "未找到 Chrome，也未找到 Homebrew。请安装官方 Chrome 后回到设置重新检测。",
			URL:      "https://www.google.com/chrome/",
			Reason:   "没有可安全自动执行的安装器",
		}
	case "linux":
		return InstallPlan{
			Platform: "linux",
			Summary:  "未找到 Chrome/Chromium。请用官方包或发行版包安装，然后回到设置重新检测。",
			Commands: []string{
				"wget -O /tmp/google-chrome-stable_current_amd64.deb https://dl.google.com/linux/direct/google-chrome-stable_current_amd64.deb",
				"sudo apt-get install -y /tmp/google-chrome-stable_current_amd64.deb",
			},
			URL:    "https://www.google.com/chrome/",
			Reason: "需要用户自己的包管理器和权限，服务不代跑 sudo",
		}
	case "windows":
		return InstallPlan{
			Platform: "windows",
			Summary:  "未找到 Chrome。可用官方安装包或 winget 安装，然后在设置里重新检测。",
			Commands: []string{"winget install --id Google.Chrome -e"},
			URL:      "https://www.google.com/chrome/",
			Reason:   "服务不代跑交互式安装器",
		}
	default:
		return InstallPlan{
			Platform: runtime.GOOS,
			Summary:  "当前平台没有受支持的 Chrome 自动安装方式。",
			URL:      "https://www.google.com/chrome/",
			Reason:   "unsupported platform",
		}
	}
}

// Install runs only the one whitelisted, reversible command. It never shells
// out through sh -c and never downloads or executes a remote script.
func Install(ctx context.Context, plan InstallPlan) (string, error) {
	if !plan.Auto || len(plan.Commands) != 1 {
		return "", fmt.Errorf("此平台不自动安装，请按提示执行官方命令")
	}
	if runtime.GOOS != "darwin" {
		return "", fmt.Errorf("当前平台不自动安装")
	}
	brew, err := exec.LookPath("brew")
	if err != nil {
		return "", fmt.Errorf("找不到 Homebrew")
	}
	cmd := exec.CommandContext(ctx, brew, "install", "--cask", "google-chrome")
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if len(text) > 4000 {
		text = text[len(text)-4000:]
	}
	if err != nil {
		if text != "" {
			return text, fmt.Errorf("Homebrew 安装失败: %w", err)
		}
		return "", fmt.Errorf("Homebrew 安装失败: %w", err)
	}
	return text, nil
}

// probeChrome starts a throwaway Chrome and loads about:blank. It proves the
// binary can actually start under this user before the toggle reports ready.
func probeChrome(ctx context.Context, chromePath string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return probeChromeContext(ctx, chromePath)
}
