# 浏览器 PoC（browser-poc 分支）

> 状态：PoC，未合入 master。目标是把 OpenMausBot 的“Agent 用浏览器 + 人看实时画面/接管”这套能力，按 AgentDeck 的 Go 单二进制 + MCP 架构做一个最小验证。

## 已实现

- 设置页新增「浏览器」开关，服务端持久化到 `<data_dir>/browser/settings.json`。
- 开启时检测本机 Chrome/Chromium：
  - macOS：`/Applications/Google Chrome.app/...`、`~/Applications/...`、Chromium。
  - Linux：`google-chrome`、`google-chrome-stable`、`chromium`、`chromium-browser`、`/snap/bin/chromium`。
  - Windows：`Program Files` / `Program Files (x86)` 下的 Chrome/Chromium。
  - 也支持 `browser.chrome_path` 显式指定。
- 找不到 Chrome 时弹窗询问是否安装：
  - macOS 且找到 Homebrew：用户确认后执行 `brew install --cask google-chrome`，可用 `brew uninstall --cask google-chrome` 回退。
  - Linux/Windows 或没有 Homebrew：只展示官方下载地址/安装命令，不代跑 `sudo` 或交互式安装器。
  - 用户拒绝则保持关闭并显示原因。
- 单会话/单 Chrome/单标签页的运行时：
  - 每个 AgentDeck session 一个 `user-data-dir`，登录态跨轮次保留。
  - 最大并发 `browser.max_browsers`（默认 2），空闲 `browser.idle_minutes`（默认 10）关闭。
  - 浏览器帧走独立 `GET /api/sessions/{id}/browser/stream` SSE，不进入会话事件流。
  - 人工接管/交还走 `POST /api/sessions/{id}/browser/input`；接管期间 agent 工具被拒绝。
- Claude 通过 MCP 使用：
  - runner 在开启浏览器时生成 `data/browser/mcp/<session>.json`（0600），以 `--mcp-config` 注入。
  - `cc-webui -browser-mcp` 是 stdio MCP 代理，转发到 loopback-only 的 `/api/internal/browser/mcp`。
  - 工具：`browser_status/open/navigate/snapshot/read/screenshot/click/fill/type/press/wait/close/restart`。
- 安全：
  - 只允许 `http`/`https`/`about:blank`，拒绝 `file:`、`javascript:`、`chrome:`、`data:`。
  - 默认拒绝 localhost/内网/link-local/云 metadata；`browser.allow_local` 或 `browser.allow_hosts` 才能放行。
  - 始终拒绝 AgentDeck 自己的 listen 端口。
  - snapshot 不返回 password 字段的值（显示 `[value masked]`）。
  - MCP 配置和 settings 文件 0600；日志不打印 token。
  - 输入接口要求同源；内部 MCP 接口只接受 loopback + Bearer token。

## 截图

设置开关（Chrome 已检测，开关打开）：

![浏览器设置开关](screenshots/browser-poc-settings.png)

浏览器面板（左侧 AgentDeck，右侧实时画面 + 地址栏 + 接管）：

![浏览器面板](screenshots/browser-poc-panel.png)

## 配置

`config.example.yaml` 增加了注释块：

```yaml
browser:
  enabled: false
  chrome_path: ""
  max_browsers: 2
  idle_minutes: 10
  fps: 8
  quality: 60
  allow_local: false
  allow_hosts: []
```

运行时开关由设置页写 `<data_dir>/browser/settings.json`，优先于 `browser.enabled`。

## 使用

1. 构建：`go build -o cc-webui .`
2. 启动：`./cc-webui -config <config.yaml>`
3. 设置 → 浏览器 → 打开开关；Chrome 已安装时会做一次 30 秒内的启动探针。
4. 打开一个 Claude 会话，直接说“用 browser 工具打开 https://example.com 并告诉我标题”。
5. 右上角地球图标打开浏览器面板；点「接管」后可以自己点击/输入，点「交还」恢复 agent 控制。

MCP 配置只注入 Claude/qoder 这一类支持 `--mcp-config` 的引擎；codex/opencode/mcode 尚未接入，UI 不区分引擎，后续按引擎能力显示。

## 测试记录（2026-11-02，本机）

- `go build -o cc-webui .`：通过。
- `go vet ./...`：通过。
- `go test ./... -count=1`：`internal/browser`、`server`、`store`、`config` 等通过；`internal/runner` 的 `TestServeKeyIsCompact` 失败（master 基线同样失败，和本改动无关）。
- 隔离实例（127.0.0.1:18821，临时 data_dir）实测：
  - 设置开关开/关、Chrome 检测、状态回读通过。
  - `tools/list` 返回 13 个工具；`browser_open https://example.com` 返回标题和快照。
  - 本地测试页：`browser_fill #name` → `browser_click #go` → `browser_read #out` 得到 `Hello, Ada`。
  - `browser_screenshot` 返回 PNG（约 100 KB base64）。
  - SSE 连续收到 JPEG 帧；浏览器面板截图见上。
  - 人工接管时 `browser_snapshot` 返回“已被人工接管”，交还后恢复。
  - 并发第 3 个会话得到“浏览器并发已达上限”。
  - `file:///etc/passwd`、`169.254.169.254`、`100.100.100.200`、AgentDeck 自己的端口均被拒绝。
  - `browser_restart` 可用；密码字段 snapshot 显示 `[value masked]`，不泄露值。
- Claude Code 2.1.226 真实会话实测：`browser_status` MCP 工具被调用并返回 `running=true` 等原文。

## 未完成 / 已知限制

- 单标签页；没有多 tab、frames/shadow DOM 完整支持、dialog、下载、上传、文件选择器。
- 没有真正的出口代理（SOCKS5/HTTP CONNECT）；目前是导航前 URL 解析 + 私网/metadata 拦截，挡不住已打开页面里的子资源请求直连内网。产品化前应补浏览器级 egress gate（参考 OpenMausBot `server/browser/egress/`）。
- MCP 配置里目前放的是 gateway token（0600 文件，仅 loopback 使用）；产品化建议改成 per-session capability。
- 浏览器开关对已存在的常驻进程不即时生效；需要重启常驻或下一轮 spawn。
- macOS 自动安装只在有 Homebrew 时执行；Linux/Windows 只给命令/URL。
- 只有 Claude/qoder 路径注入 `--mcp-config`；codex/opencode/mcode 未接。
- 没有浏览器证据（步骤截图/页面健康）、没有登录库/1Password、没有 My Chrome 附着。
- 没有移动端适配；面板是桌面优先。
- 没有清理 profile 目录的 UI；删除 session 会关闭浏览器并删 profile 目录。
