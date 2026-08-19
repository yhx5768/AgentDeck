# cc-webui

**在浏览器里远程驾驭 Claude Code。单文件 Go 二进制，前端内嵌，开箱即用。**

cc-webui 是 Claude Code（以及任何兼容 CLI）的 Web 控制台。它把会话管理、流式对话、工具调用可视化和移动化体验整合到一个轻量级服务里——你只需把它跑在远程机器上，手机或电脑打开浏览器即可开始工作。

---

## 为什么需要 cc-webui

- **远程开发**：Claude Code 运行在服务器 / 跳板机 / 容器里，本地不想反复 SSH。
- **多会话并行**：同时为多个项目维护独立会话，随时切换、归档、恢复。
- **模型与后端自由切换**：同一会话可在不同 API 后端或模型之间热切换，不丢失上下文。
- **工具调用可视化**：命令、文件读写、编辑 diff、todo 列表、本地文件链接都以卡片形式呈现，不用在终端里翻找。
- **移动端可用**：PWA 式体验，抽屉导航、底部操作面板、触屏优化，外出时也能跟进会话。

---

## 核心特性

| 特性 | 说明 |
|---|---|
| 单二进制部署 | `go build` 生成一个可执行文件，前端资源已用 `go:embed` 打包，无需 Node 构建。 |
| 多 CLI / Provider / Model | 配置文件定义多个 Claude 兼容 CLI 与 API 后端；会话内一键热切换 CLI、Provider、Model、权限模式。 |
| 导入已有会话 | 扫描 `~/.claude/projects`，把本地历史会话一键接进 Web UI 继续聊。 |
| 流式实时对话 | SSE 推送，Markdown 渲染、代码高亮、工具调用折叠块、token 与花费统计、排队与停止。CLI 开启 `partial_messages` 后支持打字机式逐 token 增量渲染（`--include-partial-messages`）。 |
| 工具调用可视化 | Bash 终端视图、Read/Write/Edit diff、TodoWrite 检查清单、thinking 折叠块。 |
| 本地文件预览 | 点击消息中的本地文件链接（如 `/path/to/file.md:12`），右侧面板只读打开并自动跳转到对应行号。 |
| TUI 与 Web 自由切换 | 每个会话都有 session id，点「resume」复制命令即可回到 TUI 继续跟进，Web 里做的一切状态都在。 |
| 移动优先 PWA | 抽屉导航、底部操作面板、响应式布局、深浅主题切换。 |
| 安全网关 | 单一 `gateway_token` 鉴权；文件预览严格限制在会话工作区内。 |

---

## 快速开始

```bash
# 构建：产出单个二进制，前端已内嵌
go build -o cc-webui .

# 配置
cp config.example.yaml config.yaml
# 编辑 config.yaml：设置 gateway_token、你的 clis 与 providers

# 运行
./cc-webui
# 浏览器打开 http://<host>:8321，输入 gateway_token 登录
```

也支持环境变量覆盖：

```bash
GATEWAY_TOKEN=xxx CC_WEBUI_LISTEN=0.0.0.0:8321 ./cc-webui
```

命令行参数：`-config <path>`、`-listen <addr>`。

---

## 配置示例

```yaml
listen: "0.0.0.0:8321"
gateway_token: "change-me-to-a-long-random-string"
data_dir: "./data"      # 会话元数据 + 事件流
default_cwd: "~"        # 新建会话默认工作目录

# 三层正交结构：
#   clis[]      - 一个 Claude 兼容二进制安装
#   providers[] - 一个 API 后端（共享 env，通常是 BASE_URL / TOKEN）
#   providers[].models[] - 该后端可选的模型（可带自己的 env）
# 生效 env：cli.env → provider.env → model.env（后者覆盖前者）
clis:
  - name: "official-claude"
    path: "claude"
    permission_mode: "bypassPermissions"
    partial_messages: true   # 打字机式逐 token 流式渲染（需新版 Claude Code）
    args: []
    env: {}

  - name: "ducc"
    path: "/home/you/.baidu-cc/baidu-cc/bin/ducc"
    permission_mode: "bypassPermissions"
    args: []
    env: {}

providers:
  - name: "anthropic"
    env: {}
    models:
      - name: "claude-sonnet-4-5"
        input: [text, image]

  - name: "kimi"
    env:
      ANTHROPIC_BASE_URL: "http://127.0.0.1:3456"
      ANTHROPIC_AUTH_TOKEN: "..."
    models:
      - name: "kimi-k3"
        input: [text, image]
      - name: "kimi-k2.7"
        env:
          CLAUDE_CODE_AUTO_COMPACT_WINDOW: "1000000"
```

完整字段请参考 `config.example.yaml`。

---

## 工作原理（简要）

- 每条用户消息会启动一个 `claude -p --output-format stream-json --verbose [--resume <sid>] [--model <m>]` 进程，prompt 走 stdin 注入。
- 首轮从 `system/init` 事件捕获 `session_id`，后续轮次用 `--resume` 续接，因此**切换 CLI / Provider / Model / 权限模式不会丢失上下文**。
- 事件流持久化在 `data_dir`，SSE 先回放历史再实时推送；服务重启不影响会话数据。
- 停止 = 杀掉当前 claude 进程组。

### 关于 env 注入

Claude Code 的 `~/.claude/settings.json` 里的 `env` 段会覆盖进程继承的环境变量，因此只往进程注入 `ANTHROPIC_BASE_URL` 等并不生效。cc-webui 的处理方式：

1. 合并当前会话的生效 env（cli → provider → model，后者覆盖前者）。
2. 将合并结果物化为 `data/settings/<cli>__<provider>__<model>.json`（权限 0600），以 `--settings <绝对路径>` 传给 claude——命令行 settings 优先级最高，可以压过 `settings.json`。
3. 同时把生效 env 注入进程环境变量，供其他 CLI 兼容层使用。

---

## 安全说明

- **鉴权**：所有 API 与 SSE 接口都需要 `gateway_token`。支持 Cookie `ccgw_token`、`Authorization: Bearer <token>` 或 `?token=` 查询参数。
- **文件预览白名单**：`/api/files` 仅允许读取**位于任一会话工作区（cwd）内**的文件，并会 `EvalSymlinks` 解析真实路径，防止通过软链接或 `..` 穿越到 `/etc/passwd`、`~/.ssh` 等敏感目录。
- **只读 + 截断**：文件预览为只读，上限 2 MiB；检测到二进制内容（NUL 字节）会拒绝返回。
- **提醒**：请把 `gateway_token` 设置为强随机字符串，并避免在公网直接暴露端口。建议通过反向代理或 VPN 使用。

---

## 部署到其他机器

```bash
# 交叉编译（例如在 macOS 上编译给 Linux）
GOOS=linux GOARCH=amd64 go build -o cc-webui .

# 拷贝二进制 + 一份 config.yaml 到目标机器即可，无运行时依赖
scp cc-webui config.yaml user@remote:~/
ssh user@remote 'GATEWAY_TOKEN=xxx ./cc-webui'
```

目标机器需要至少一个可用的 Claude 兼容 CLI，并在 `config.yaml` 中正确配置。

---

## API 概览

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/login` | `{token}` 登录并写入 Cookie |
| GET | `/api/me` | 当前登录信息 |
| GET | `/api/profiles` | profile 列表（env 值不下发） |
| GET / POST | `/api/sessions` | 会话列表 / 创建 |
| GET / PATCH / DELETE | `/api/sessions/{id}` | 详情 / 改名·换 profile·model·权限 / 删除 |
| POST | `/api/sessions/{id}/messages` | 发送消息 `{text, images}` |
| POST | `/api/sessions/{id}/stop` | 中断当前轮 |
| GET | `/api/sessions/{id}/events` | SSE：历史回放 + 实时事件 |
| GET | `/api/claude-sessions?cwd=` | 扫描本机 `~/.claude/projects` |
| POST | `/api/sessions/import` | 导入已有 claude session |
| GET | `/api/files?path=` | 读取本地文件（限会话工作区内） |

---

## 项目结构

```
main.go                  # 入口 + go:embed
internal/config/         # 配置加载
internal/store/          # 会话元数据 + 事件日志持久化
internal/runner/         # claude 进程管理：队列、流解析、事件广播、停止
internal/server/         # HTTP 路由、鉴权、SSE
internal/claudehist/     # ~/.claude/projects 扫描与导入转换
web/                     # 免构建前端（vanilla JS + vendor 的 marked/DOMPurify/highlight.js）
```

---

## 反馈与贡献

如果你在使用过程中遇到问题，或者有新功能建议，欢迎提交 Issue 或 PR。

---

*cc-webui 是一个社区驱动的 Claude Code Web 控制层，不是 Anthropic 官方产品。*
