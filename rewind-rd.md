# Rewind 功能设计文档（RD）

> 状态：**已实施**（feat/rewind 分支，2026-08-21）
> 目标：在 cc-webui 中实现与 Claude Code TUI「双击 Esc」等价的回滚能力：回滚到任一历史用户消息处，**对话与文件同时回滚**。
>
> 实施时相对本 RD 的调整见文末「§10 实施记录」。

## 1. 背景与目标

Claude Code TUI 中双击 Esc 会列出历史用户消息，选择一条即执行回滚：**对话回到该消息之前（消息文本回填输入框），同时文件恢复到该消息发出时的状态**——对话与文件一起回滚，没有单独的选项。

cc-webui 要在 Web 端提供等价能力：每条用户消息提供 rewind 入口，确认后一次性回滚对话 + 文件。

## 2. 官方机制调研（Claude Agent SDK / CLI）

参考文档：https://code.claude.com/docs/en/agent-sdk/file-checkpointing

### 2.1 文件 checkpoint 原理

- 设置环境变量 `CLAUDE_CODE_ENABLE_SDK_FILE_CHECKPOINTING=true` 后，claude 进程在每次通过 **Write / Edit / NotebookEdit** 修改文件**之前**自动备份原内容。
- **checkpoint 标识 = 用户消息 UUID**（jsonl 历史每行的 `uuid` 字段）。rewind 到某 UUID = 恢复文件到该消息发出时的状态：之后的修改被还原、期间新建的文件被删除。
- 执行 rewind 的 CLI 等价命令：

  ```bash
  CLAUDE_CODE_ENABLE_SDK_FILE_CHECKPOINTING=true \
    claude -p --resume <claude-session-id> --rewind-files <checkpoint-uuid>
  ```

- 结果中包含恢复/删除/跳过的文件信息（`RewindFilesResult`，跳过符号链接等非常规文件计入 `skippedLinks`，需 CLI ≥ v2.1.216）。

### 2.2 限制（必须在 UI 文案中体现）

| 限制 | 说明 |
|---|---|
| 仅 Write/Edit/NotebookEdit | Bash 命令（`echo >`、`sed -i` 等）的修改**不跟踪、不可回滚** |
| subagent 修改 | 不跟踪（`context: fork` 的前台 skill 除外） |
| 仅文件内容 | 目录的创建/移动/删除不撤销 |
| 绑定 session | checkpoint 只能在创建它的 session 上 rewind |
| 需提前开启 | 未开启 checkpointing 的历史 session 无 checkpoint 数据，rewind 报 "No file checkpoint found" |

### 2.3 对话回滚

官方 SDK 的 `rewind_files()` **只回滚文件，不动对话**。对话回滚没有公开 CLI flag，需要 cc-webui 自行实现（见 §5.4）：截断 claude 的 session jsonl + 截断 cc-webui 自己的事件日志。

## 3. 现状适配点

| 现有能力 | 复用方式 |
|---|---|
| `runner.runTurn` 每个 turn spawn 一个 `claude -p --resume` 进程 | rewind 同样 spawn 一次性进程，无需常驻连接、无需引入 SDK |
| `settingsFile` 注入 env | 默认注入 `CLAUDE_CODE_ENABLE_SDK_FILE_CHECKPOINTING=true` |
| `claudehist` 解析 `~/.claude/projects/<slug>/<sid>.jsonl` | turn 结束后扫描 jsonl 捕获用户消息 UUID；对话回滚时定位截断位置 |
| store 事件日志 + `ReplaceEvents` | 持久化 checkpoint UUID；对话回滚时截断事件 |
| SSE 广播 | rewind 结果与对话截断同步到所有在线客户端 |

## 4. 总体设计决策

1. **默认开启，不加配置项**：对所有 CLI 的 turn 默认注入 `CLAUDE_CODE_ENABLE_SDK_FILE_CHECKPOINTING=true`（不支持的环境变量无害）。
2. **UUID 捕获走 jsonl 扫描，不用 `--replay-user-messages` flag**：
   - 原因：该 flag 未被第三方 claude 兼容 CLI 广泛支持，传了可能直接报错；env 变量则无此风险。
   - 方式：turn 结束后扫描该 session 的 jsonl，取新增的用户消息行 `uuid`，与本次发送文本匹配后写回事件。
3. **持久化用方案 A**：UUID 写进 `user` 事件的 data（`{"text": "...", "checkpoint": "<uuid>"}`），前端回放历史时天然携带。
4. **回滚 = 文件回滚 + 对话双截断 + 草稿回填（单一动作，对齐 TUI）**：先做文件 rewind，再截断 cc-webui 事件日志与 claude jsonl（先备份），被回滚的用户消息文本返回前端填入输入框。**前提是该消息有 checkpoint uuid**——没有 checkpoint 的消息不提供 rewind 入口（文件无法回滚，单独回滚对话会造成「对话回去了但文件没回去」的不一致状态）。

## 5. 后端设计

### 5.1 开启文件 checkpoint（runner）

`runTurn` 构建 env 时，无条件加入：

```go
envMap["CLAUDE_CODE_ENABLE_SDK_FILE_CHECKPOINTING"] = "true"
```

注意：settings file 路径（`--settings` 的 env）与进程 env 两条注入路径都要覆盖，保证无论配置是否带了额外 env 都生效。

### 5.2 捕获并持久化 checkpoint UUID（runner + store + claudehist）

流程：

1. `runTurn` 开始前记录 jsonl 当前行数（或最后一个 `uuid`）作为水位线。
2. turn 结束（进程退出、拿到 `session_id` 保证 `ClaudeSessionID` 已知）后：
   - 扫描 `claudehist.HistoryPath(sess.Cwd, sess.ClaudeSessionID)` 水位线之后的新行；
   - 找到 `type=="user"` 且非 sidechain/meta、文本与本次发送内容匹配的行，取其 `uuid`；
   - 调 store 新增能力 `UpdateEvent(sessionID, seq, mutate)` 把 uuid 写进本次 `user` 事件 data 的 `checkpoint` 字段；
   - 广播更新后的 `user` 事件（同 seq），前端已渲染的同名消息就地挂 rewind 按钮（见 §6.1）。
3. 多模态（stream-json 输入）路径同样适用：发送文本会原样落入 jsonl。
4. **导入 session 兜底**：`claudehist.Convert` 的 `histLine` 补解析 `uuid` 字段，转出的 `user` 事件直接带 `checkpoint`（终端里开启过 checkpointing 的 session 导入后即可用）。

匹配失败（jsonl 缺失、文本对不上）不报错，仅该条消息没有 rewind 按钮。

### 5.3 rewind API

```
POST /api/sessions/{id}/rewind
Body: { "seq": <user事件seq> }
200:  { "ok": true, "draft_text": "<被回滚消息原文>", "files": "<CLI输出摘要>" }
409:  { "error": "session is running" }        // 有进行中的 turn
400:  { "error": "..." }                       // seq 非法 / 该消息无 checkpoint
502:  { "error": "<claude stderr>" }           // rewind 进程失败
```

处理顺序（全程保证该 session 无 running turn，用 `sessionRuntime` 状态校验）：

1. 校验 session 存在、事件 seq 指向一条带 `checkpoint` uuid 的 `user` 事件；
2. **文件回滚**：
   - spawn：`claude -p --resume <ClaudeSessionID> --rewind-files <uuid> --output-format json`，cwd = `sess.Cwd`，env 带 `CLAUDE_CODE_ENABLE_SDK_FILE_CHECKPOINTING=true`，复用 `runTurn` 的 CLI/settings 解析逻辑；
   - 失败原样透传 stderr，**中止整个操作**（文件没回滚成功就不动对话，避免不一致）（重点场景："No file checkpoint found"、"File rewinding is not enabled"）；
3. **对话回滚**（必须在文件 rewind 成功之后执行，因为截断 jsonl 后 checkpoint 即不可寻址）：
   - **备份**：复制 claude jsonl 到 `<data_dir>/backups/<sid>.<ts>.jsonl`；
   - **截断 claude jsonl**：定位 `checkpoint` uuid 所在行，保留其之前的所有行（不含该用户消息本身——它将作为草稿重新发送）；找不到该行则返回错误、不截断（此时文件已回滚，错误信息中需说明）；
   - **截断 cc-webui 事件**：`ReplaceEvents` 保留该 `user` 事件之前的所有事件；
4. 持久化并广播 `rewind` 事件：

   ```json
   { "kind": "rewind", "data": { "seq": 12, "checkpoint": "<uuid>",
     "draft_text": "...", "ok": true, "detail": "<文件恢复摘要>" } }
   ```

   （对话截断后该事件成为事件流里的新末尾事件，作为"此处发生过回滚"的标记。）

### 5.4 数据结构变更

- `store.Event` 不变；`user` 事件 data 增加可选字段 `checkpoint: string`；
- 新事件 kind：`rewind`；
- store 新增 `UpdateEvent(sessionID, seq, mutate func(*Event))`（内存 + 文件同步重写单行；文件按 seq 定位行重写，或整文件重写——事件量小，整文件重写即可）；
- 新目录 `<data_dir>/backups/` 存放 jsonl 截断前备份。

## 6. 前端设计

### 6.1 入口

- 用户消息气泡 hover 操作区新增 ⏪ rewind 按钮；
- **可用条件**：事件 data 带 `checkpoint` 且 session 当前 idle；无 `checkpoint` 的消息不显示按钮；running 时置灰，tooltip "运行中不可回滚"；
- 收到同 seq 的更新 `user` 事件时，就地更新按钮可用态（§5.2-2）；
- trajectory 面板每个 turn 头部复用同一入口（v2，可选）。

### 6.2 确认弹窗（单一动作，对齐 TUI）

```
回滚到这条消息之前？
「<消息首行摘要…>」

· 此消息及之后的对话将被删除，消息文本回填到输入框
· 此消息之后 Claude 通过 Write/Edit 所做的文件修改将被恢复，
  期间新建的文件将被删除

说明：Bash 命令执行的文件修改无法回滚。

        [ 回滚 ]  [ 取消 ]
```

- 确认后进入执行态（按钮 loading，禁止重复点击）。

### 6.3 执行与反馈

- `POST /rewind` 成功后：
  - 移除该消息及之后的所有已渲染消息，`draft_text` 填入输入框并 focus；
  - 在对话流末尾渲染一条系统样式消息（复用 status 渲染位）：如「已回滚到此处 · 3 个文件恢复，1 个新建文件删除」；
- SSE 收到 `rewind` 事件的其他客户端执行同样的 UI 截断与提示，保证多端一致；
- 失败 toast 错误原文；「No file checkpoint found」映射为友好文案：「该会话未开启文件 checkpoint（或 CLI 版本过低），无法回滚」。

## 7. 边界情况

1. **运行中的 turn**：API 返回 409，前端按钮置灰；
2. **旧 session / 未开启 checkpointing 的 session**：消息没有 checkpoint，不显示 rewind 按钮（不提供「仅回滚对话」的降级，避免对话与文件状态不一致）；
3. **fork 的 session**：新 claude session 无历史 checkpoint，仅 fork 后新 turn 可回滚；
4. **重复 rewind**：允许；回滚后更早消息的 checkpoint 依然有效；已截断掉的消息自然消失；
5. **jsonl 备份**：截断前必须落备份到 `data/backups/`，截断不可逆；
6. **部分失败**：文件回滚成功但 jsonl 截断失败时，返回错误并明确告知「文件已回滚、对话未回滚」，用户可重试；
7. **并发端**：一端 rewind，其他端通过 SSE `rewind` 事件同步截断 UI；
8. **CLI 版本**：符号链接安全跳过需 ≥ v2.1.216，低版本风险与错误透传；
9. **Bash/subagent 修改不可回滚**：弹窗说明文案常驻。

## 8. 测试计划

1. 单测：
   - `UpdateEvent` 持久化正确性；
   - jsonl 扫描定位 uuid（含 sidechain/meta/工具结果行干扰）；
   - 截断逻辑：按 uuid 定位 / 找不到时不截断；
2. 集成（手动）：
   - 开启后新 session：发消息 → agent 改文件 → rewind：文件恢复、消息消失、草稿回填、重发后 claude 上下文中不含已回滚内容（验证 jsonl 截断生效）；
   - rewind 后继续多轮对话，再 rewind 到更早的消息，验证可重复回滚；
   - 运行中 rewind → 409；
   - 老 session（无 checkpoint）→ 无 rewind 按钮；
   - 导入终端 session → uuid 兜底生效。

## 9. 实施分期

| 阶段 | 内容 | 预估 |
|---|---|---|
| Step 1 | env 默认注入；jsonl 扫描捕获 uuid；`UpdateEvent`；写回 + 广播 | 后端 ~120 行 |
| Step 2 | `POST /rewind`：文件回滚 + jsonl 备份截断 + 事件截断 + `rewind` 事件 | 后端 ~150 行 |
| Step 3 | 前端：按钮、确认弹窗、SSE 处理、草稿回填、系统提示 | 前端 ~120 行 |
| Step 4 | 导入兜底 uuid、错误文案打磨、README/AGENTS.md 更新 | — |

## 10. 实施记录（相对原 RD 的调整）

实施中用真实 CLI（claude 2.1.226 + qianfan glm-5.2）做了端到端验证，据此做了以下调整：

1. **§4.2 uuid 捕获的真正原因**：`uuid` 在 jsonl 用户行里**始终存在**（已验证），与是否传 `--replay-user-messages` 无关——该 flag 只把 uuid 回显进*输出流*，cc-webui 直接扫 jsonl 不需要它。原 RD 给的理由（第三方 CLI 不支持）只是次要好处。
2. **§5.3 / §6 API 用 `checkpoint` 而非 `seq`**：SSE handler 每条连接把 seq 重编号（`sendSeq`），前端拿不到存储 seq，按 seq 寻址会错位。改为 `POST /rewind {checkpoint:<uuid>}`，前端按 uuid 匹配消息。`rewind` 事件 data 也以 `checkpoint` 为准。
3. **§5.2 live 通知用独立的 `checkpoint` 事件**：同 seq 重广播会被前端 `seenSeq` 去重丢弃，无法给已渲染消息补按钮。改为：checkpoint 持久化进 `user` 事件 `data.checkpoint`（回放时渲染按钮），另广播一个瞬态 `checkpoint` 事件让在线客户端给最后一条用户消息就地挂按钮。
4. **§5.3 回滚第一条消息 → 清空 ClaudeSessionID**：实测 `--resume` 对「没有用户消息的空 transcript」会报 `No conversation found`。截断到第一条消息之前会让 transcript 只剩 `queue-operation` 元数据，无法 resume。故截断后若 `HasUserPrompt==false`（即回滚了第一条消息），清空 `ClaudeSessionID`，下一轮自动开新 claude session 续接（旧 transcript 已备份）。非首条消息的回滚保留前置对话，resume 正常。
5. **§5.3 rewind 进程输出**：`--rewind-files` 的 stdout 是纯文本 `Files rewound to state at message <uuid>`（**不是** JSON，`--output-format json` 被忽略），不含文件计数。`parseRewindDetail` 防御性解析，落到通用文案「文件已回滚」。RD 设想的「3 个文件恢复」计数拿不到。
6. **§5.3 运行态校验**：同时校验 `Queued>0`（不仅 `Running`），避免排队消息与截断竞争。
7. **§5.2-4 导入兜底 uuid：不做**。导入的历史 session 多数未开 checkpointing，给它们挂按钮会变成「点了必失败」。仅 cc-webui 自身新轮（checkpointing 默认开）才捕获 uuid、显示按钮。导入 session 里新发的消息仍可回滚。
8. **§5.3 `--output-format json` 仍保留**：虽然被忽略，但无害且未来 CLI 可能支持；stdin 立即关闭（空 prompt，对齐 SDK 的 `rewind_files` 模式）。

验证过的链路：发消息→改文件→捕获 uuid 写回 user 事件+广播 checkpoint→POST /rewind→文件恢复+jsonl 备份截断+事件截断+rewind 标记事件→重发草稿开新 session（首条消息回滚）/ resume 续接（非首条）。单测覆盖 `LastUserUUID`/`TruncateBeforeUUID`/`HasUserPrompt`。
