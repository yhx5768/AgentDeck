# opencode 引擎接入设计

> 状态：调研完成，待需求方确认范围后实施。实测基于本地 `/home/soar/.opencode/bin/opencode` 1.18.25（2026-09-06 探针，会话已清理）。
> 用户原话：「你看下我本地有 opencode 二进制，他也有 -p -c 等功能，你看看怎么把他也集成进来，跟 cc 一样。不涉及 opencode 和 cc 相互切换。」——即：opencode 成为**第二引擎**，与 cc（Claude Code 及其兼容 CLI）平级；会话建在哪个引擎上就用哪个引擎，不做引擎互转/切换。

## 背景：cc 集成面 vs engine-agnostic 面

cc-webui 现有代码里，**与 Claude Code 协议强耦合**的部分（其余都是通用聊天基础设施）：

| 耦合点 | cc 的做法 | opencode 对应物 |
|---|---|---|
| 转录回放源 | 现场读 `~/.claude/projects/<slug>/<sid>.jsonl`（`claudehist.Convert`） | `opencode export <sid>` 输出整会话 JSON（或 `run --format json` 现场逐事件） |
| 会话捕获 | stream 里 `system/init` 事件的 `session_id` | **任意事件行都带 `sessionID`**（实测），首行即取 |
| spawn 参数 | `-p --output-format stream-json --resume <sid> --settings <file> --model …` | `run --format json -s <sid> -m <provider>/<model> --auto …`（无 --settings 概念） |
| 流式增量 | `--include-partial-messages` → `stream_event` | 无 token 级 partial；按 part 完成出事件 |
| checkpoint/rewind | `--rewind-files` + 文件截断 + `TruncateBeforeUUID` | CLI 无对应命令（内部有 step 级 snapshot 与 db revert 字段，无 CLI 面）→ **v1 不支持** |
| 常驻/peer | stream-json 双向 + custom-title + `~/.claude/sessions/<pid>.json` 注册表 | 无此协议（`serve` 是另一套 HTTP server 模型）→ **v1 不支持** |
| fork | 复制 jsonl 换 id | opencode 无 CLI 等价 → **v1 禁用**（v2 可 export→改 id→`opencode import`） |
| slash commands | `system/init` 事件下发 | 无 → 空（前端空列表已能隐藏 popup） |
| 错误面 | stderr `[claude-code:kind]` 结构化解析 | stderr 原样上抛（通用路径已有） |

**engine-agnostic、不用动**：store 会话元数据/事件日志、队列 worker、SSE 双源合并/seq 重编、事件 schema（`user`/`stream`/`status`/`rewind`）、前端按块渲染（每个 content block 独立气泡/工具卡）、轨迹 byMsgId 分组、停止（杀进程组）、上传/附件、notes、鉴权、热加载。

## opencode 侧调研结论（实测证据）

### CLI 面（1.18.25）

- `opencode run [message..] --format json`：headless 单轮，**stdout 逐行 JSON 事件**；`-s <sid>`/`-c` 续会话、`-m provider/model`、`--auto`（headless 无交互审批，受限工具非 --auto 时可能挂起）、`--title`、`--agent`、`--variant`、`--thinking`（未验证是否让 reasoning 事件出现在 live）。
- `opencode session list --format json`：`[{id,title,directory,created,updated,projectId,…}]`。
- `opencode export <sid>`：整会话 JSON（stderr 打一行提示，stdout 纯 JSON）→ 回放源。
- 会话数据落在 `~/.local/share/opencode/opencode.db`（sqlite，内部 schema 带版本迁移）+ `storage/`。**不直读 DB**——schema 随版本演进（本次 export 样本是 1.18.19 写的、1.18.25 读），一律走 CLI 官方命令，延续「CLI 是真相源」哲学。

### live 事件词汇（实测两轮：纯文本 + bash 工具）

每行 `{type(snake_case), timestamp(ms), sessionID, part{…}}`，事件类型实测见：

```
step_start  part.type="step-start"   (assistant 步骤开始)
text        part.type="text"         text=整段文本（一步一段，无 token 增量）
tool_use    part.type="tool"         tool=bash, callID=tool_xxx, state={status:"completed", input:{…}, output:"hi\n"}
step_finish part.type="step-finish"  reason=stop|tool-calls, tokens{…}, cost
```

要点：
- **用户 prompt 在 live 不回显**——cc-webui 自己持久化的 user 事件就是唯一来源（同 cc 现状）。
- 同一 assistant 步骤的多个 part（step-start→text→step-finish）**共享一个 messageID**（`msg_…`），跨步骤递增。
- text 事件在 part 完成时整段到达；未见工具「先 running 后 completed」的双事件，但**工具耗时长的场景未实测**——adapter 需按 callID 记账以防万一（见转换规则）。
- 无 reasoning 事件出 live（尽管 export 里有 reasoning part）——疑似与 cc 一致：live 剥离思考，除非 `--thinking`（待实测）。

### export 结构（回放源）

`{info:{id,title,directory,model:{id,providerID},agent,version,tokens…}, messages:[{info:{role,id,finish,modelID,providerID,tokens,time}, parts:[…]}]}`。

part 类型字典（实测字段）：

| type | 字段 | 备注 |
|---|---|---|
| text | text | 用户消息/assistant 文本都在 |
| step-start | snapshot | 步骤前仓库快照 hash（opencode 内部 checkpoint，无 CLI 回滚面） |
| reasoning | text, metadata.anthropic.signature, time | 形如 cc thinking 块；**live 不出现** |
| tool | tool, callID, state{status,input,output} | 一次调用一个 part，call/result 合体 |
| patch | hash, files[] | 文件改动摘要；**内容不随 export**（在 tool-output 存储侧），v1 不渲染 |
| step-finish | reason, tokens{total,input,output,reasoning,cache}, cost | 该步骤用量 |

无 sidechain/subagent 概念；消息线性排列。

## 设计

### 1. config：`clis[]` 加 `kind`，会话 engine 由 cli 解析

```yaml
clis:
  - name: "opencode"            # kind 缺省 "claude"，老配置零改动
    kind: "opencode"
    path: "/home/soar/.opencode/bin/opencode"
    permission_mode: "bypassPermissions"   # opencode 分支把它映射为 --auto
    args: []
    env: {}                      # 仅进子进程 env；不生成 --settings 文件
```

- `store.Session` **不加字段**：`ClaudeSessionID` 语义扩展为「外部引擎会话 ID」（cc uuid 或 opencode `ses_…`；ducc 已在此字段存 cc uuid，先例在此）。session 的 engine = `cfg.CLIByName(sess.CLI).Kind`，由 `/api/clis` 与 session view 下发（`sessionView` 加 `engine`），前端据此 gate cc-only 控件。
- `providers/models` 镜像 opencode 的 providerID/modelID（名字对齐，runner 用 `-m <provider>/<model>` 拼装；env 段对 opencode 分支不生效也无害——它读自己的 auth.json/凭据，见风险 7）。config.example.yaml 附样例段。
- 热加载：engine 随 cli 走；会话中途把 cli 从 claude 改到 opencode = 用户在配置上自己作出来的语义（会话持有对方引擎的 session id，续跑会失败并给出错信息），不额外防护，文档注明。

### 2. runner 分支（`runTurn` 按 `cli.Kind` 拆两路）

cc 路径原样不动。opencode 路径：

- args：`run --format json` + `--auto`（session/`cli.PermissionMode` 为 `bypassPermissions` 时；否则不传并在错误面提示）+ `-m <provider>/<model>`（provider 为会话 provider，model 取 `EffectiveModel`）+ 已有 `ClaudeSessionID` 时 `-s <sid>`；新会话时可选 `--title <sess.Name>`（opencode 侧标题友好）+ `cli.Args`。不拼 `--settings`/`--effort`/`--permission-mode`。
- 消息注入：**待实测**「无 positionals 时 stdin 是否被读作消息」——是则走 stdin（与 cc 同构、无 argv 长度顾虑）；否则走位置参数（文本通常 <128KB，可接受；超长 fallback stdin 再测）。
- stdout 逐行 JSON → `adapter.Live(line)` → `emit(sessionID,"stream", 事件对象)`。**任意行带 sessionID**：首行即 `Upsert ClaudeSessionID`（若与存量不同）。
- 错误/中断/exit：与 cc 共用（`interrupted`/`error` + stderr 原样）；`parseClaudeCodeError` 仅 cc 路径调用。
- `captureCheckpoint`：`kind==opencode` 直接 return（无 jsonl 可扫，也省一次无效 IO）。

### 3. 新包 `internal/opencodehist`（镜像 claudehist 的职责）

CLI 操作全部 `exec` 配置里的 cli path（子进程 30s 超时；list 无超时压力）：

- `List(cliPath, cwd)` → `opencode session list --format json`；cwd 非空时过滤 `directory==cwd`（cc 的 List(cwd) 是目录过滤语义）。
- `Convert(cliPath, cwd, sessionID)`：`opencode export <sid>` → 按转换规则 → `[]*store.Event`。SSE 每连接现场调（同 cc：不复制、不落盘）。
- 转换规则（live 与 replay **共用同一映射**，产出的 stream 事件对象同构、确定性）：

  | 输入 | 输出事件 |
  |---|---|
  | user 消息 text part | `kind=user, data={text}`（`mergeStoredAndClaude` 按文本与 store 去重，与 cc 回显抑制同机制） |
  | assistant text part | stream `assistant`，content=`[{type:text,text,id:<partID>}]`——part id 作块 id 使同 messageID 多文本块指纹不撞 |
  | assistant tool part（首次见 callID） | stream `assistant`，content=`[{type:tool_use,id:callID,name:tool,input:state.input}]` |
  | tool part 状态 completed/error | stream `user`，content=`[{type:tool_result,tool_use_id:callID,content:[{type:text,text:state.output}]}]`；error 时 `is_error:true` |
  | tool 先 running 后 completed（若存在） | 按 callID 记账：running 只发 tool_use，completed 才发 result，step 结束清账（防双事件重复） |
  | reasoning part | v1 **跳过**（live 不出现；回放单独出现会让「刷新后多了思考」与实时不一致——同 `ConvertSubagents` 剔 thinking 的先例）。留开关：将来 `--thinking` 实测可用后，live 出 reasoning、回放映射为 thinking 块一并启用 |
  | step-start/step-finish/patch | 跳过（step-finish 的 tokens 可作后续 usage 附加到该 msg 的 assistant 事件，v1 不做） |
  | 时间戳 | part/message `time.created`(ms) → `Event.Ts` |
  | 块排序 | 按 part 在消息内的顺序展开（回放逐 part 展开成独立事件，与 live 逐行同构——前端本来就是一个块一个气泡/卡片地渲染，见 app.js `renderEvent`） |
- 转换是纯函数、可单测（构造 export JSON fixture → 断言事件序列；镜像 claudehist 的测试风格）。

### 4. server

- `handleEvents`：回放源按 engine 分发——cc 走现有 `claudehist.Convert(+ConvertSubagents)`；opencode 走 `opencodehist.Convert(cliPath, …)`。合并/去重逻辑零改动（fingerprint 规则已兼容：assistant 块带 id 签名、tool_result 按 tool_use_id、user 文本按 count 抑制）。
- 导入：`/api/opencode-sessions`（list，按 cwd/prefix 过滤同 claude-sessions）+ `/api/opencode-sessions/preview`（export 转前 N 事件）+ `handleImport` 增加 `cli`/`kind` 判别：opencode 会话校验 `export` 可读；`Cwd` 用其 `directory`（经 `ResolveCwd` 校验存在）；provider/model 尝试按 export `model.providerID/modelID` 对齐配置，对不上则要求前端选一个存在的 provider（模型可后改）。会话 model 存 `ModelOverride` 供续聊 `-m`。
- cc-only 操作按 engine 拒绝：`handleRewind`/`handleFork` 对 opencode 会话 400；`PATCH resident/peer_name` 400（+ `EnsureResident`/`RestoreResidents` 入口再挡一道，防配置/请求竞态）。
- `resumeCommand`（终端恢复命令）：opencode 会话拼 `cd <cwd> && opencode -s <sid>`。

### 5. 前端（vanilla JS，无框架）

- sessionView 带 `engine`；新会话弹窗/配置 sheet 的 cli 下拉本来就会列出 opencode 条目（选中即建 opencode 会话，无需改）。选中 opencode cli 时 provider/model 下拉照常（选项来自 config）。
- cc-only 控件按 `engine==='opencode'` 隐藏：📡 常驻 popover、rewind 按钮（无 checkpoint 数据时本就不出现，双重保险）、fork、effort 下拉、图片附件（composer 的图片按钮；**文件上传保留**——文本 mention 路径、agent 用工具读）。
- 导入面板加来源页签（cc 历史 / opencode 会话），列表与预览走新端点；导入后会话继续聊天走 `-s`。
- 工具卡：opencode 工具名与 cc 大小写不同（bash/read/write/edit/webfetch vs Bash/Read/Write/Edit/WebFetch），`buildToolView` 按名分发会漏 → 回落通用卡（名+输入 JSON+输出，可折叠）。v1 **不做**名称映射表/任务面板适配（opencode todo 工具 input 结构与 TodoWrite 不同，硬映射会污染任务面板）；v2 按实测工具清单再定。

### 6. 安全边界

- 子进程 = 配置内 path/args（同 cc）；不直读写 opencode.db；export/session list 输出只进本进程内存与事件流（`env` 字段仍 `json:"-"` 不下发）。
- opencode 凭据在用户自己的 auth.json，cc-webui 不接触。
- 唯一新网络面：无。`--auto` 语义 = 用户配置的 `permission_mode: bypassPermissions` 等价物，写入 config.example 注释说明。

## 风险与待实测（实施第一件事）

1. stdin 无 message 位置参数时是否被读作消息（决定消息注入通道）。
2. 长回复/多段文本是否拆多个 text part、同 messageID 多 text part 的回放指纹（已用 part id 兜底）。
3. 工具长耗时是否出现「running → completed」双事件（adapter 已按 callID 记账兜底，需实测确认清理时机）。
4. `--thinking` 是否让 reasoning 事件进 live（决定 reasoning/thinking 块策略）。
5. `--title` 行为、`-s` 续会话是否回放历史（有 `--no-replay` 选项说明 TUI 会回放；run -s 大概率不回放，需确认首行事件不会是历史消息）。
6. 非 `--auto` 的挂起面：调研中一次 3 分钟零输出（0 行 stdout、空 stderr、exit 143 超时杀）待复现定性（权限挂起 or 网关抖动）。cc-webui 侧有 Stop 兜底 + stderr 原样上抛，可接受。
7. 进程 env 会带着 shell 的 `ANTHROPIC_BASE_URL=https://opencode.ai/zen/go` 与 config 的 cli/provider env 进 opencode 子进程——opencode 以自身 auth.json 为准，大概率无感；若实测受 ANTHROPIC_* 干扰，opencode 分支 env 按白名单过滤。
8. 同一 opencode 会话被 cc-webui 与 opencode TUI 同时续跑（与 cc 的 jsonl 共享同类风险，无新增防护，文档注明）。
9. import 会话的 `directory` 可能不在 cc-webui 机器（远程场景）——ResolveCwd 校验兜住，报「工作目录不存在」。

## 实测补遗（E2E 落地时发现，已定案）

- **`session list` 按项目分桶**：同一 DB（~/.local/share/opencode/opencode.db，单文件、70MB 级），但 `session list` 只返回进程 cwd 所属桶的会话——git 根 = 该仓库桶（projectID 是路径的哈希，见 export `info.projectID`），非 git 目录 = `global` 桶。`export <id>` 与 `run -s <id>` 全量可按 id（跨桶 export 实测成功），只有 list 分桶。→ `opencodehist.List(cli, dir)` 带 cwd 选桶；API `?cwd=` 空则服务器自身 cwd；UI hint 注明。opencode `run` 侧永远 `cmd.Dir = sess.Cwd`，恢复不跨桶。
- **用户 prompt 带回显双引号**：opencode 的 run echo 与 export 里用户文本都是 `"prompt"` 形态（引号是 opencode 回显样式，非用户输入）。cc-webui 自己落 stored echo（无引号），若回放原样带引号，`mergeStoredAndClaude` 按文本 count 去重会失配 → 刷新后用户气泡重复。→ `stripEchoQuotes`（剥一层，只剥首尾各一个引号）用在 export 转换与导入预览的 user 文本上。
- **工具卡双事件**（风险 #3 实测确认）：`run --format json` 对工具先发 `status:"running"` 再发 `status:"completed"` 的同 callID 两个 part——`ToolTx` 按 callID 记 assistant/result 已发，实测单卡单结果。
- **`-f` 附件 = 多模态通道**：`opencode run` 不接受内嵌 base64 图片（API 层拒绝），官方附件通道是 `-f <path>`（可重复）——图片/文件都走它，`run` 会把文件内容展开注入模型上下文（视觉模型直接见图）。**解析陷阱**：`-f` 是数组 flag，消息位置参数必须放前面（`-f a.png "msg"` 会把 `"msg"` 吞进文件数组报 `File not found`）；实测 `message-first` 或 `--file=` 两种排布都通。**echo 规则**：stored user echo 只存 `msg.Text`（不用 turnText 的路径 mention 拼装版）——export 回显 = 原 prompt 原文，`-f` 内容只进部分展开不进 echo 文本，去重才成立。
- **带附件 export 的 user 消息多三个 part**（工具前导文本 `Called the Read tool with the following input: …`、`<path>/<type>/<content>` 文件展开、`type:"file"`）——`userPromptText` 只认其余 text（即 prompt echo），回放/预览共用该过滤，否则用户气泡会带文件转储、echo 失配重复。
- **推理深度 = `--variant`，不是 effort**：`run --variant <key>` 的键集合按模型走（`opencode models <provider> --verbose` 的 `variants` 字段，`opencodehist.ModelVariants`/`parseModelsOutput` 解析，有单测）。实测集合差异巨大：gpt-5.6-luna `[none low medium high xhigh max]`、deepseek-v4-flash `[low high max]`、kimi-k3 只有 `[max]`、minimax-m3 `[none thinking]`、半数模型无 variants。→ server `/api/opencode-model-variants`（进程内缓存 10 分钟）+ 前端 effort 下拉按模型键动态重建（`EFFORT_STATIC` 页面加载即快照 claude 全集，首个会话是 opencode 也不污染），effort 服务端校验按 engine 分流（claude 固定集合 / opencode 形状校验）。
- **风险 #6/#7 定案**：慢响应是网关抖动（复现一轮 3.5 分钟才回话，无零输出）；env 的 `ANTHROPIC_BASE_URL` 等对 opencode 子进程无感（自带 auth.json）。

## 实施顺序（每步可独立合入）

- **P0 对话 MVP**：config `kind` + runner opencode 分支 + adapter（live）+ handleEvents 回放源分发 + 前端 cc-only 控件 gate。产出：opencode 会话可建、可聊、工具卡可见、刷新可回放、可停止。
- **P1 导入**：`/api/opencode-sessions(+preview)` + handleImport 分支 + 导入 UI 页签。产出：既有 opencode 会话（含 TUI 建的）进控制台续聊。
- **P2 打磨**：resumeCommand、--title、错误面文案、tool 名映射表与任务面板适配（实测工具清单后）、usage 徽章、thinking 开关、opencode `session delete` 联动。

## 文件级改动清单

| 文件 | 改动 |
|---|---|
| `internal/config/config.go` | CLI 加 `Kind string`（yaml `kind`，缺省补 "claude"） |
| `config.example.yaml` | opencode clis 样例段 + 注释 |
| `internal/runner/runner.go` | `runTurn` 按 kind 分支（opencode args/env/解析/session 捕获）；`captureCheckpoint`/`RewindFiles` 出口拦 opencode |
| `internal/runner/resident.go` | `EnsureResident` 拦 opencode kind（返回错误） |
| `internal/opencodehist/`（新） | `opencodehist.go`（List/Convert/转换规则）+ `opencodehist_test.go`（export fixture 单测） |
| `internal/server/server.go` | `sessionView.Engine`；`handleEvents` 回放源分发；import 分支 + opencode list/preview 端点；rewind/fork/resident PATCH 按 engine 400；`resumeCommand` 分支 |
| `internal/server/stats.go` | 不动（数据源天然只有 cc jsonl；页面文案注明覆盖 cc） |
| `web/app.js` | sessionView.engine 读取 + `isOC(sess)` gate（resident popover/fork/effort/图片附件）；导入面板页签 |
| `web/index.html` | 导入面板来源切换（若现为单列） |
| `web/style.css` | 页签/徽章样式（若有） |
