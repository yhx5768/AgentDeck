# 引擎中立的跨会话消息（A2A）设计与实施

> 状态：已实施（`internal/a2a` + `POST /api/a2a/send` + agentdeck MCP）。
> 关联：常驻进程与地址名的机制见 `peer-rd.md`；主会话派工用法见
> `docs/main-agent-best-practices.md`。

## 1. 目标

让**任何引擎**的会话都能当主 Agent、都能收发跨会话消息（A2A），而不只是 claude：

- 地址名（`peer_name`）引擎无关；
- 投递走一条引擎中立的 HTTP 总线，而不是 claude 的 `SendMessage`；
- 入站消息在对话流与轨迹视图里**归属 A2A**；
- agentdeck 从「只读文档的 skill」升级为**所有引擎都能用的 MCP 工具面**。

## 2. 为什么不是 claude 的 peer 协议

claude 的 `SendMessage` 依赖：常驻 streaming-input 进程 + `~/.claude/sessions/<pid>.json`
注册表 + messaging socket。qoder/mcode/codex/opencode 都没有 `SendMessage` 工具，
也没有注册表，所以那条路对它们不可用。A2A 总线因此绕开 claude 的私有面。

**关键简化：接收方不需要常驻。** 总线投递 = 往目标会话**排一轮**（与用户发消息同一条
`Manager.Enqueue` 路径）。opencode 的 `serve` 常驻、claude 一次性进程、mcode/codex 的
一次性进程，都能被排一轮。常驻只在 claude **原生** `SendMessage` 那条路上还有意义。

## 3. 消息模型

### 3.1 信封（envelope）

总线把消息包成 claude 已经在用的形状（`internal/a2a.Envelope`）：

```
Another agent session sent you a message. It is not from the user; ...

<cross-session-message from="cc-webui-修SSE断线">
<body>
</cross-session-message>
```

`from` 是发送方地址名。接收方模型据此知道这是别的 agent 发来的、不是用户指令。

### 3.2 入站 = 一条带 `source:"a2a"` 的 user 事件

投递时 `Enqueue` 落一条持久化 `user` 事件，`text` 是信封（引擎看到的就是它），
额外带 `source:"a2a"` / `from` / `body`：

- **实况**：`Enqueue` 广播这条事件 → 前端立即渲染成 A2A 气泡；
- **回放**：`mergeStoredAndClaude` 用「trimmed text 计数」抑制引擎 transcript 里
  同一段信封的回显，保留我们这条带 `source` 的事件 → 回放与实况一致。

**因此不需要改任何引擎的 reader**——这是本方案相对「逐引擎识别信封」的最大简化。

### 3.3 前端归属 A2A（点 1）

- `web/app.js` `renderEvent`：`ev.kind === 'user' && d.source === 'a2a'` → `msg-peer`
  气泡（与 claude 原生 peer 同形）；
- `web/trajectory.js` `trajProject`：`source === 'a2a'` → `kind:'peer'` 记录 →
  `trajA2ADir` 判为 `in` → A2A 角色（`A2A 收`）；
- `lastUserEventIndex` 排除 `source === 'a2a'`（A2A 不是真人输入，不能当悬浮条目标）；
- 出站：`isA2ASendTool(name)` 同时认 `SendMessage` 与 MCP 的
  `mcp__agentdeck__send_message`，两者都归 A2A 角色（`A2A 发`）并渲染同一张发件卡。

## 4. 投递 API

`POST /api/a2a/send {from, to, message}`：

- `resolveAddress(to)` 依次尝试：精确 session id → `peer_name` / 清洗后的会话名
  （大小写不敏感）→ 剥掉 `name [ref]` 消歧后缀；
- 命中后 `Enqueue(target, {Text: envelope, Source:"a2a", From, Body})`；
- 返回 `{ok, session_id, to, from}`。

`agentSendBlocked` 现在只拦 **claude 主 Agent** 的 `agent-send`（那里与原生
`SendMessage` 重复）；非 claude 主 Agent 的 `agent-send` 放行（那是唯一入站通道）。

## 5. agentdeck：skill → MCP（点 2）

### 5.1 形态

`internal/a2a/mcp.go` 定义工具面（单一来源），`cc-webui -agentdeck-mcp` 是 stdio
代理，转发到回环 `POST /api/internal/agentdeck/mcp`（Bearer = gateway token），
与 browser MCP 同一套管线。工具：`list_sessions` / `send_message` / `new_session` /
`get_session` / `stop_session`。实现是**在进程内调用既有 HTTP handler**
（`callHandler`），校验/落库单一来源。

### 5.2 注入方式（五引擎）

| 引擎 | 注入 |
|---|---|
| claude / qoder | 每轮写 `--mcp-config`（含 browser + agentdeck 两个 server） |
| codex / ducx | `-c mcp_servers.<name>.*`，经 0700 wrapper 读 0600 env（token 不进 argv） |
| opencode | **仅** per-session 配置的 `mcp.servers`（见下） |
| mcode | 合并进 `~/.minimax/mcp.json` 的 `mcpServers.agentdeck`，指向 0700 wrapper |

开关：`agentdeck_mcp`（`*bool`，**nil = 开**）。关掉时 mcode 的条目会被清理。

### 5.3 身份（`from`）

`send_message` 的 `from` 可显式传；claude/qoder/codex 的 per-session MCP 配置带
`CC_WEBUI_A2A_SESSION`，代理据此把 `from` 默认成该会话地址。opencode/mcode 是共享
配置、没有 per-session env，所以由调用方显式传 `from`（角色提示里写明自己的地址）。

### 5.4 看板工具（仅主 Agent）

主 Agent 需要维护「看板」卡片，所以同一台 agentdeck MCP 服务器还带一组看板工具：
`board_list` / `board_get` / `board_create` / `board_update` / `board_comment` /
`board_bind_worker`（映射到既有的 `/api/agent-board/*` handler）。

**只在主 Agent 会话里暴露**，两层保证：

- **注入面**：`agentdeckMCPInfo(sessionID)` 当该会话 `main_agent` 为真时，往 MCP 环境里
  加 `CC_WEBUI_A2A_BOARD=1`；stdio 代理读这个 env，只有置位时 `tools/list` 才把
  `a2a.BoardTools` 追加进工具表。worker 的工具表因此完全看不到看板工具。
- **调用面**：`/api/internal/agentdeck/mcp` 收到看板工具调用时，用 `requireMainAgent`
  **再查一次**调用会话的 `main_agent` 标记（`sessionID` 为空或非主 Agent 一律拒绝），
  所以即使有客户端硬猜工具名也写不进看板。

看板数据是 `<data_dir>/agent-board.db`（SQLite）。**注意**：本机没有迁移来源
（`agent-board.local.json` / reading 工作区的 routing-table），所以看板初始为空——主
Agent 用上面的工具建卡即可。归档的任务（`board_update {archived:true}`）不出现在
`Snapshot()` 与计数里。

**`send_message` 的可选 `task_id`**：投递成功后，服务端顺手往那张卡的 timeline 追加一条
`路由事件` 评论（`⇢ <from> → <to>：<正文首行>`，source `a2a`）。这样派工/回报不必再单独
调 `board_comment` —— 派工单带上 `task_id=<卡号>`，worker 回报时也带同一个 `task_id`，
一来一回都自动记在卡上。卡不存在只静默跳过（消息已经投出去了，不能因此失败）。**它不是
board 工具**：`send_message` 对所有会话可用，所以 worker 也能「回报式」写卡；但建卡/改
状态/绑 worker 这些仍然只有主 Agent 能做。

## 6. opencode 作为主 Agent

### 6.1 地址名

`peer_name` 对所有引擎开放（create/patch 不再 400）；写 claude transcript
`custom-title` 只对 claude/qoder 做（其他引擎只存 store，避免往 `~/.claude/...`
写一个假 jsonl）。

### 6.2 角色提示

opencode 没有 append 语义，也不该用 prompt 的 `system`（覆盖 agent 自身 prompt）。
改走**隔离配置的 `instructions` 数组**：per-session 目录里写
`HTML_RENDER_HINT.md` + `ROLE_PROMPT.md`，`opencode.json` 的 `instructions` 指向两者。
角色提示变更需要该 serve 重启后生效（与 claude 常驻同款取舍）。

### 6.3 per-session 配置与 serve

opencode 的共享 base 配置**只放 instructions**——声明 `mcp` 会盖掉用户自己的 mcp
段（配置同名键后者覆盖）。所以：

- 需要 per-session 配置的会话（有 browser、或 `MainAgent` / `peer_name` / `Resident` /
  非空 `role_prompt`）→ `ensureOpencodeRuleFor(sessionID)`；
- serve key 带 `scope=<sessionID>`，这些会话各起一个 serve（主 Agent 数量少，可接受）。

## 7. 服务端/前端改动清单

- `internal/a2a/`：`a2a.go`（信封）、`mcp.go`（工具面 + stdio 代理）。
- `internal/runner`：`Message.From/Body` + `Enqueue` 落库；`browser_mcp.go` 泛化成多
  server 注入（`appendMCP`/`appendMCPCodec`/`writeMCPWrapper`）；`ocrules.go` 加
  agentdeck + role prompt + `opencodeConfigScope`；`mcode_mcp.go`；`runMcodeTurn` 刷新
  mcode 的 mcp.json。
- `internal/server`：`a2a.go`（地址解析 + `/api/a2a/send` + `agentSendBlocked`）、
  `agentdeck_mcp.go`（回环 MCP 端点）；create/patch 的 resident 仍 claude-only、
  peer_name 放开；peer rename 只写 claude/qoder。
- `internal/config`：`agentdeck_mcp`。
- `web/`：`renderEvent` A2A 分支、`lastUserEventIndex`、`trajA2ADir`/`isA2ASendTool`、
  📡 对所有引擎显示（常驻开关行仅 claude）。
- `skills/agentdeck`：`main_agent_send_blocked` 与服务端同口径。

## 8. 已知取舍

- 非 claude 主 Agent 的 `from` 需要模型显式传（opencode/mcode 无 per-session env）。
- opencode 的 `OPENCODE_CONFIG_DIR` 会替换用户配置目录（既有行为，browser MCP 已如此）。
- mcode 的 MCP 配置是用户级 `~/.minimax/mcp.json`（mcode 没有 per-session 机制），
  条目带命名空间，关掉时会清理。
- claude 的 peer 轮次仍走原生 `SendMessage`；A2A 总线主要服务非 claude 引擎，两者在
  同一 A2A 角色下统一呈现。
