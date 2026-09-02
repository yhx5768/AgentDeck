# 跨会话消息（Peer Messaging）功能设计文档（RD）

> 状态：设计已确认，待实施。
> 调研依据：Agent SDK Python 文档、claude-agent-sdk 0.2.147 源码（含 bundled CLI 2.1.250）、本机 `~/.claude` 运行实证。

## 1. 背景与目标

Claude Code CLI（≥2.1.2xx，`peerProtocol: 1`）内置了会话间消息能力：一个会话里的 agent 通过内置 `SendMessage` 工具按**名字**向另一个**在线**会话发消息，接收方在下一个 tool round 把消息注入为一个新轮次。

cc-webui 现状是每轮 spawn 一次性 `-p` 进程（`runner.go:372` `runTurn`），轮次结束进程退出 → 会话在 peer 网络里"不在线"，收不到消息。本功能目标：

1. 会话可标记为**常驻**：cc-webui 为其维持一个长驻 streaming-input 进程，随时可被其他会话（终端 CLI、其他 cc-webui 会话、Remote Control）寻址和送达。
2. 会话有独立的**地址名**（写进 Claude transcript 的 custom-title），与 cc-webui 显示名解耦。
3. 入站 peer 消息在对话流里**可见**（今天完全不可见，见 §3）。
4. 发送侧一期不做 UI——任何会话里的 agent 自己调 `SendMessage` 即可（发送方无常驻要求）。

## 2. 官方机制调研

### 2.1 发送：SendMessage 工具

bundled CLI 2.1.250 内置工具描述（实测抽取）：

```json
{"to": "worker", "message": "check if tests pass over there"}
{"to": "worker [3fa9c1]", "message": "you, specifically"}
```

- **名字即地址**（"the name IS the address"）：裸名精确匹配唯一 live 会话即直达；一对多时才需 ` [ref]` 消歧（ref 来自 `ListAgents` 列表或报错提示，不可编造）。
- 投递语义："A listed peer is alive and will process your message; messages enqueue and drain at the receiver's next tool round"。
- 到达正文包一层 `<cross-session-message from="...">`；回复时把入站消息的 `from` 抄为 `to`。
- `notify_when_idle: true`（仅本机会话、主对话可用）：对端下次转 idle 或退出时回一条一次性通知——agent 间编排可用，cc-webui 无需感知。
- 发送方可以是同机会话、Remote Control 会话、云端会话；发送方自身无需常驻。

### 2.2 寻址：custom-title 即名字

SDK `rename_session()`（`_internal/session_mutations.py:53`）的实现 = 往 transcript 追加一行：

```json
{"type":"custom-title","customTitle":"risk-script","sessionId":"<uuid>"}
```

`list_sessions` 从文件尾部读**最后一条** custom-title，重复调用安全。无 custom-title 的会话用派生名（`目录名-NN`，`nameSource: "derived"`）。本机实证：`~/.claude/sessions/*.json` 里有 custom-title 的会话（`risk-script`）注册名为其标题，没有的为派生名。

CLI 唯一名规则：同机 live 会话不许重名，后启动/后改名的拿到 `name-word-word` 变体并被告知。以 `/` 开头的标题不可寻址（changelog 明确的 bugfix 语境）。

### 2.3 接收：注册表 + messaging socket

- 注册表：`~/.claude/sessions/<pid>.json`，字段含 `pid / sessionId / cwd / name / nameSource / status(idle|busy|waiting) / peerProtocol / kind / entrypoint`。
- socket：`$XDG_RUNTIME_DIR/cc-socks/<pid>.sock`（无 XDG_RUNTIME_DIR 时走 `CLAUDE_CODE_TMPDIR`/`/tmp`；亦可 `--messaging-socket-path` 显式指定）。发送方按注册表 pid 拼 socket 路径直连。
- 一次性 `-p` 进程运行期间也会注册并监听（本机实测：sdk-cli entrypoint 的 `-p` 进程注册名 `cc-webui-db`、socket 存在），但进程退出即地址失效；且派生名跨 respawn 不稳定（同 cwd 多会话会被加成变体）。**这就是"必须 rename + 常驻"的根因。**
- 运行中的进程会 tail-scan transcript 吸收外部追加的 custom-title（SDK docstring 确认），但**注册表 name 是进程启动时写的**——外部改名对注册名不保证即时生效，统一以"重启常驻进程后生效"为准。

### 2.4 入站消息的 transcript 形态

消息到达后落两条（本机实测 transcript）：

1. `{"type":"queue-operation","operation":"enqueue","content":"<cross-session-message ...>...</cross-session-message>"}`
2. `{"type":"user","isMeta":true,"promptSource":"system", "origin":{...}, "message":{"role":"user","content":"Another Claude session sent a message:\n<cross-session-message ...>...</cross-session-message>\n\nThis came from another Claude session — ..."}}`

`origin` 结构（SDK `types.py:1046` `MessageOrigin`，流式输出里同样带）：

```json
{"kind":"peer","from":"uds:/run/user/1000/cc-socks/<pid>.sock","name":"test1",
 "body":"<解码后的正文，渲染用它>","verifiedPeerPid":1403885,"fromSession":"<可选>"}
```

### 2.5 入站策略 crossSessionInbound

settings 键 `crossSessionInbound`：`accept`（直接投递）/ `hold`（停放待批准）/ `refuse`（拒收）。**bypass 权限的会话默认 hold**（changelog：cross-session messages sent to a session running with bypassed permissions are held for your approval）。headless 下无人能点批准 → 消息永远停住。repo/managed settings 只能收紧。

### 2.6 版本要求

`peerProtocol: 1`，实测 2.1.226 可用（注册表与 socket 均正常）。过旧 CLI 无注册表/socket 行为，需版本检测兜底。

## 3. 现状适配点

| 现状 | 差距 |
|---|---|
| 每轮一次性 `-p` 进程（`runner.go:372`） | 轮间无进程 → 不在线。需常驻 streaming-input 模式 |
| `Convert` 跳过全部 `IsMeta` 行（`claudehist.go:274`） | 入站 peer 消息回放时**完全消失** |
| 前端 stream user 分支只渲染 `tool_result` block（`app.js:1957`） | live 到达的 peer user 事件不渲染（只见 assistant 回复） |
| rename 只有 cc-webui 显示名（db.json `Session.Name`） | 无 transcript custom-title 写入能力 |
| 物化 settings 只含 `env` 段（`runner.go:331`） | 常驻会话需补 `crossSessionInbound: "accept"` |
| `Stop` = SIGKILL 进程组（`runner.go:272`） | 常驻模式下停止轮次应发 interrupt 控制帧，不杀进程 |

## 4. 总体设计决策（已与需求方确认）

1. **常驻开关在聊天头部**：按会话开关，开启即常驻、随时可收消息；关闭回退一次性进程模式。
2. **后端重启自动恢复常驻**：启动时把 `Resident=true` 的会话全部重拉起来。（机制前提：cc-webui 退出时子进程 stdin 管道 EOF，CLI 自行排空退出，不留孤儿——恢复 = 重新 spawn，不存在接管旧进程。）
3. **地址名独立于会话显示名**，默认值取会话名（清洗后）。
4. **运行中改地址名 = 自动重启常驻进程生效**（无感重建，约 1-2s）。
5. **resume 能力不改**：终端 resume 与常驻并存时，后启动方吃变体名（通常是终端），同名双活窗口内裸名发送需 `[ref]` 消歧——文档注明，一期不加 UI 检测。
6. **发送侧一期不做 UI**：agent 自行调 `SendMessage`。
7. peer 消息**不持久化进 cc-webui 事件日志**——遵循"stream 事件不落盘"原则，Claude transcript 即其持久层。

## 5. 后端设计

### 5.1 store

`Session` 加两字段（`store.go:15`）：

```go
Resident bool   `json:"resident,omitempty"`
PeerName string `json:"peer_name,omitempty"`
```

### 5.2 常驻进程协议（新文件 `internal/runner/resident.go`）

`sessionRuntime` 加 `resident *residentProc`。residentProc 持 `cmd / stdin / done chan / generation`。

**spawn**：在现有 runTurn 参数基础上改：

```
claude -p --input-format stream-json --output-format stream-json --verbose
  [--include-partial-messages] [--permission-mode X] [--model M] [--effort E]
  [--resume <claude-session-id>] --settings <file>
```

启动后写 SDK 同款 initialize 握手并等 `control_response`（`_internal/query.py:598` 线格式）：

```json
→ {"type":"control_request","request_id":"req_1","request":{"subtype":"initialize","hooks":null}}
← {"type":"control_response","request_id":"req_1","response":{...}}
```

**发一轮**：往 stdin 写一条 stream-json user 消息（复用现有多模态编码，`runner.go:459`——text-only 也统一走这条，不再用裸文本 argv 路径）。

**轮次边界**：stdout 上 `type:"result"` = turn_end（发 status `turn_end` + 跑 `captureCheckpoint`，transcript 实时落盘故 `LastUserUUID` 不变）；`origin.kind=="peer"` 的 user 事件且当前空闲 = peer 轮次的 turn_start。排队语义不变（cc-webui 自己的 queue 串行化，result 后才写下一条）。

**Stop**：常驻模式下 `Manager.Stop` 改为发控制帧 `{"subtype":"interrupt"}`（打断当前轮，进程存活）；进程级 SIGKILL 仅用于 teardown。

**teardown**：关 stdin（EOF → CLI 排空退出）→ 超时（如 5s）后 SIGKILL 进程组。触发点：关开关、改 PeerName、删除会话、fork、rewind 前置、服务 shutdown。

**崩溃自愈**：stdout EOF/退出且 `Resident=true` 且非主动 teardown → 置 dead、发 error status；下一条入队消息时按 `--resume` 重建。

### 5.3 rewind 协调

`handleRewind` 流程最前面加：若常驻活着 → teardown 并等待退出，再走现有 `LockForRewind` → `--rewind-files` → 截断流程。**绝不在常驻进程活着时截断 transcript**（其内存中有全量对话且会回写缓存 metadata）。下一条消息自然以截断后的 transcript 重建常驻。

### 5.4 启动恢复

`main.go` 在 `server.New` 后调 `mgr.RestoreResidents()`：遍历 `Resident=true` 且 `ClaudeSessionID!=""` 的会话，错峰（~300ms）spawn 常驻进程。失败记 log + status error，不 spin。`ClaudeSessionID==""` 的跳过（首条消息时再拉）。

### 5.5 地址名 API

- `claudehist` 加 `RenameSession(cwd, claudeSessionID, title)`：追加 custom-title 行（§2.2 格式），0600 权限保持。前置 `ClaudeSessionID!=""`，否则 409"先发过一轮消息"。
- `PATCH /api/sessions/{id}` 收 `resident` / `peer_name`：
  - `resident: true` → spawn（无 PeerName 则用清洗后的 Session.Name 先 `RenameSession` 兜底）；
  - `resident: false` → teardown；
  - `peer_name` 变更 → `RenameSession` + 常驻活着则自动重启（决策 4）。
- **注册表回读**：spawn 后读 `~/.claude/sessions/<pid>.json`，把 CLI 实际注册名（可能是变体）存进 runtime 并随 `GET /api/sessions/{id}` 的 status 下发（`registered_name`），前端 popover 展示真实地址。launcher 型 CLI（ducc 会 fork 子进程写注册表）pid 查不到，按 claude session id 全目录扫描兜底（`claudehist.ReadRegistryForSession`），注册晚到由 `GetStatus` 按需刷新兜住。
- 清洗规则：去控制字符、trim、限长；空则回退会话名；以 `/` 开头拒绝（不可寻址）。

### 5.6 settings 注入

`settingsFile`（`runner.go:326`）在会话 `Resident=true` 时物化 JSON 增加 `"crossSessionInbound": "accept"`。repo/managed 收紧时无解，文档注明。

### 5.7 版本检测

常驻 spawn 后 3s 内未出现注册表条目或无 socket → 视为 CLI 过旧，teardown + status error "CLI 版本过低，不支持常驻（需 ≥2.1.2xx）"。

## 6. 前端设计

### 6.1 常驻开关 popover

`index.html:98` `header-actions` 加 `btn-resident`（lucide `radio` 图标，开启时高亮）。点击弹 popover：

- 开关（当前 resident 状态）；
- 地址名 input（placeholder = 会话名清洗结果；留空保存 = 用默认）；
- 状态行：`在线 · 地址 worker-2 · pid 1776486` / `未运行` / 错误文案；
- 说明一行："其他 Claude 会话可按地址名给本会话发消息"。

保存 = PATCH；改地址名时提示"将自动重启常驻进程生效"。

### 6.2 peer 消息渲染

- **live**：`renderEvent` stream 分支（`app.js:1957`）加 `d.origin?.kind === 'peer'` 判断，渲染专用气泡：标题"⇢ 来自会话 {origin.name}"，正文 `origin.body`（无 body 时 fallback 剥 `<cross-session-message>` envelope）。不走 `msg-user` 样式，与"你"区分。
- **回放**：`claudehist.Convert` 对 `IsMeta && origin.kind=="peer"` 行放行，转成 stream 事件（`{type:"user", origin, message:{content:[{type:"text",text:body}]}}` 形状），其余 isMeta 照旧跳过。histLine 加 `Origin` 字段。注意 `eventFingerprint` 去重需覆盖（含 uuid）。
- peer 轮次的 assistant 回复照常渲染，无需特判。

### 6.3 侧边栏徽章

会话项加常驻小点（类似未读蓝点）；`diffRunningSessions` 轮询带 `resident` 状态。

## 7. 边界情况

| 场景 | 处理 |
|---|---|
| 终端 resume 与常驻并存 | 后启动方吃变体名；双活窗口裸名发送需 `[ref]`。文档注明，不检测 |
| 名字被占 | 注册名回显真实变体；改 PeerName 时预扫 `~/.claude/sessions/*.json` 提示冲突（仅提示不阻断） |
| rewind | 先 teardown 常驻再截断（§5.3） |
| fork | fork 出的新会话不带 Resident/PeerName（避免撞名） |
| 删除会话 | 先 teardown |
| cc-webui 重启 | stdin EOF 子进程自行退出；启动按标记重拉（§5.4） |
| 常驻进程崩溃 | 标记 dead，下条消息重建（§5.2） |
| bypass 权限 + hold | settings 注入 accept（§5.6）；repo 收紧无解，注明 |
| CLI 过旧 | 版本检测兜底（§5.7） |
| peer 消息不含 checkpoint | peer 行是 isMeta，`isRealUserPrompt` 已排除 → 不会误挂 rewind 按钮；peer 轮次结束也不应给"上一条真人消息"之外的条目挂按钮（captureCheckpoint 按 msg.Text 匹配真人消息，peer 轮无匹配，自然跳过） |

## 8. 测试计划

1. `claudehist` 单测：`RenameSession` 追加格式、Convert 对 peer isMeta 行的放行/转换（参照现有 `claudehist_test.go`）。
2. 常驻协议集成验证：起两个 cc-webui 会话 A（常驻、地址名 test-a）、B，在 B 里让 agent `SendMessage to: "test-a"`，断言 A 的 SSE 出现 origin.kind=peer 事件、transcript 落 isMeta 行、A 自动起一轮回复。
3. 重启恢复：开常驻 → 重启 cc-webui → 断言注册表重新出现、SendMessage 可送达。
4. 前端按 memory 中的 frontend-verify-workflow：sed 抽取 + node 驱动真实 transcript 验证渲染；harness + headless Chrome 截图验证 popover/气泡/徽章。
5. rewind × 常驻：常驻中发起 rewind，断言先 teardown 再截断、后续消息可重建。

## 9. 实施分期

- **一期（本 RD）**：常驻模式 + 地址名 + 入站渲染 + 启动恢复 + rewind 协调 + settings 注入。
- **二期（另行设计）**：composer `@会话名` 提及 / "发送给会话"选择器（headless 无交互式 mention 解析，需 prompt 注入或显式 UI，model-mediated 可靠性需评估）；同名多活检测提示；`notify_when_idle` 编排 UI。
