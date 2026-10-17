# AGENTS.md

给在本仓库工作的工程师 / AI agent 的导览。先读这一篇，再动手。

> 用户向文档见 `README.md`；rewind 设计与实施记录见 `rewind-rd.md`；跨会话消息（常驻/地址名）设计见 `peer-rd.md`；待办见 `TODO.md`。
> 本篇只讲「怎么在这里干活」。

## 项目是什么

cc-webui：单文件 Go 二进制 + `go:embed` 内嵌 vanilla JS 前端，把 Claude Code（及任何兼容 CLI）变成 Web 控制台。会话管理、SSE 流式对话、工具调用可视化、rewind 回滚、轨迹视图。**无前端构建步骤**——改了 `web/` 直接 `go build`。

- 模块名 `cc-webui`，Go 1.22.2，唯一外部依赖 `gopkg.in/yaml.v3`。
- 入口 `main.go`：`config.Load` → `store.Open` → `runner.NewManager` → `fs.Sub(embed, "web")` → `server.New` → `srv.Run`。

## 构建 / 运行 / 测试

```bash
go build -o cc-webui .                 # 构建（前端已内嵌）
GOOS=linux GOARCH=amd64 go build -o cc-webui .   # 交叉编译
go test ./...                          # 单测：claudehist/opencodehist/server/runner/config
go vet ./...                           # 干净

./cc-webui                             # 用 ./config.yaml
./cc-webui -config <path> -listen 0.0.0.0:8321
GATEWAY_TOKEN=xxx CC_WEBUI_LISTEN=... CC_WEBUI_DATA_DIR=... ./cc-webui   # env 覆盖
```

配置：`cp config.example.yaml config.yaml`。三层正交结构 `clis[] × providers[] × models[]`，生效 env 合并序 `cli.env → provider.env → model.env`（后者覆盖前者）。`config.yaml` 与 `data/` 均被 gitignore。

配置**热加载**：`config.Manager`（`internal/config/hot.go`）以 `atomic.Pointer[Config]` 持有当前配置，每 3s 轮询文件 mtime/size，变了就整体换指针——读配置永远经 `Server.cfg()` / `Manager.cfg()` getter，**严禁缓存 `*config.Config`**（否则热加载不生效）。`listen`/`data_dir` 保留启动值不换（服务/存储已绑定）；文件未显式给 token 时保留启动的自动生成 token（否则每次改动都登出）。解析失败保持旧配置并在下一 tick 重试。热加载成功后 `main.go` 里 `cm.Watch(3s, mgr.BroadcastConfigChanged)` 向所有 SSE 连接广播 transient `config_changed` 事件；前端 `refreshProfiles()` 重拉 `/api/clis`、`/api/providers` 并按签名去重，只重建当前会话的 cli/provider/model 下拉（已选值保留）。`/api/clis`、`/api/providers` 的 `env_keys` 由服务端 `sortedKeys` 排序——前端靠 JSON 字符串化签名判断变动，无序的 map 迭代会让签名每次都不一样。

## 架构核心（不读代码会踩坑的点）

### 1. 双源事件模型

事件有两个来源，**stream 事件不持久化**：

- **持久化事件**（`data/events/<sid>.jsonl`）：`user`、`status`、`rewind`。由 `store.AppendEvent` 落盘。
- **stream 事件**：**不落盘**。每次 SSE 连接时，从 Claude 自己的 `~/.claude/projects/<slug>/<sid>.jsonl` 现场解析（`claudehist.Convert`），与持久化事件合并去重（`mergeStoredAndClaude` + `eventFingerprint`）。子代理（Agent 工具）消息主 transcript 里没有（在 `<sid>/subagents/agent-*.jsonl`，meta.json 提供 `toolUseId`/标题/类型），由 `claudehist.ConvertSubagents` 产出与实时透传同形的事件（`parent_tool_use_id` + `subagent{title,type}`，assistant 行透传 text+tool_use、user 行透传 tool_result，thinking 剔除——CLI 实时透传会 strip 它，保留会让回放比实时丰富），前端按 key 聚成折叠卡片（工具视图直接挂卡片体、结果经 `state.toolBlocks` 配对）。

后果：stream 事件靠 Claude 磁盘存活、服务重启不丢；但 cc-webui 存储里永远没有它的副本。`runner.emit` 对 `kind=="stream"` 只广播不落盘——别给 stream 事件加持久化逻辑。

### 2. SSE 每连接重编号 seq

`handleEvents` 给每条出站事件分配自己的单调 `sendSeq`，**与存储 seq 无关**。客户端拿不到存储 seq，按 seq 寻址会错位。

后果：rewind API 用 `checkpoint` uuid（不是 seq）定位目标用户消息；同 seq 重广播会被前端 `seenSeq` 去重丢弃——所以要给已渲染消息补 rewind 按钮，用独立的瞬态 `checkpoint` 事件，别重广播 user 事件。

### 3. env 经 `--settings` 注入

Claude 的 `~/.claude/settings.json` 的 env 段会覆盖进程继承的 env，所以只往进程注入 `ANTHROPIC_BASE_URL` 等不生效。cc-webui 的做法：把生效 env 物化成 `data/settings/<cli>__<provider>__<model>.json`（0600），以 `--settings <绝对路径>` 传给 claude（CLI 优先级最高），同时注入进程 env 供非 Claude 兼容 CLI 用。**只在生效 env 非空时才物化/传这个文件**：env 为空的 provider（ducc-models）端点由 CLI 自己给，文件里就只剩一个 effort 变量，却让 spawn 依赖 data 目录的绝对路径，所以不写不传——`runClaudeTurn`/`ensureResident` 都判 `needsSettings := len(env) > 0`，且必须在 `applyEffortEnv` **之前**取值（后者会往同一个 map 塞 effort，之后判据就恒为真）。

**`--effort` 输给 env**：`CLAUDE_CODE_EFFORT_LEVEL`（典型来源是用户全局 `~/.claude/settings.json` 的 env 段）压过会话传的 `--effort` flag——拦包实测 2.1.226：`--effort high` 遇上全局 `max`，请求体里仍是 `output_config.effort=max`。所以 `Session.EffortOverride` 不能只拼 flag，还要经 `applyEffortEnv` 写进物化 settings 的 env（唯一比全局 settings 更高的一层）。带 effort 时文件名是 `...__effort-<level>.json`：常驻进程整个生命周期持有该文件，同模型不同 effort 的会话不能共用一个（会互相覆盖）。网关的合法档位各不相同（sensenova 只认 low/medium/high/xhigh/none，发 max 直接 400 `field ReasoningEffort invalid`，而 CLI 自己是认 max 的），所以「继承」档会在 provider/model env 里显式钉一个该网关接受的级别。**env 为空的 provider 没有这个文件兜底**：档位只剩 `--effort` 与 spawn 的进程 env（`applyEffortEnv` 两处都会写），进程 env 压得过 flag 但压不过全局 settings——全局那条一旦设上，ducc 这类会话的档位就跟着全局走。

**复制的 resume 命令**：`resumeCommand`（server）拼 `--resume <sid> --model <m> [--effort <level>] [--settings <file>]`，模型/档位缺一终端里就会跑成另一个。三条规则：① model **只能走 `--model`**——`ducc-models` 这类 provider `env` 为空，且 ducc launcher 会把该 flag 改写成它自己内部的 `.settings.session.<pid>.json`（子进程 claude.exe 的命令行里看不到 `--model`），别指望往 settings env 里塞 `ANTHROPIC_MODEL` 来选 ducc 模型。实测（2.1.233.3）：ducc 收到 `--model 'Claude Sonnet 5'` 后自己写成 `Claude Sonnet 5[1m]` 并配上该模型的 `CLAUDE_CODE_AUTO_COMPACT_WINDOW`（1M）；而 CLI 对多个 `--settings` 是**逐 key 合并、后者胜**（假端点实测），我们的文件排在 ducc 的后面，env 里钉的原始名字会盖掉 ducc 规范化后的名字——不带 `--model` 只靠 env 钉模型会卡住不出结果（对照：带 `--model` 同模型正常返回）。所以别在 `EffectiveEnv` 里替 provider「补」模型 env。② `--settings` **只在 config env 非空时给**（endpoint/token 不随命令走就会连错端点，`EnsureSettingsFile` 顺手物化，避免 "Settings file not found"）；env 为空的 provider 不给——那种文件里只剩一个 `CLAUDE_CODE_EFFORT_LEVEL`，却让命令钉死在本仓库 data 目录的绝对路径上，换机器/挪目录即失效，不值。③ 因此 env 为空时档位只剩 `--effort`，而它**会输给全局 env / `~/.claude/settings.json` 的 `CLAUDE_CODE_EFFORT_LEVEL`**（见上一节），TUI 里可能不是会话里那个档位——这是为命令可搬运付的代价，想钉死就自己带 `--settings` 指到物化文件。同一条规则现在对后端 spawn 也成立（env 为空的 provider 同样不再物化文件），所以 TUI 与 Web 里的档位由同一组规则决定，不会两态。

### 4. 文件 checkpoint 常开

`buildEnvMap` 无条件注入 `CLAUDE_CODE_ENABLE_SDK_FILE_CHECKPOINTING=true`。每轮结束 `captureCheckpoint` 扫描 Claude jsonl，把该用户消息的 `uuid` 写进 user 事件的 `data.checkpoint`，并广播瞬态 `checkpoint` 事件让在线客户端给最后一条用户消息挂 rewind 按钮。

### 5. rewind 是多步协调操作

`POST /api/sessions/{id}/rewind {checkpoint}` 的顺序（`handleRewind`）：

1. `LockForRewind`（原子占用 session，running/queued 时 409）——持锁期间发的消息会排队但不启动 worker，避免与截断竞争。
2. 文件回滚：`claude --rewind-files <uuid>`。**必须先成功**，否则不动对话（避免文件回去了对话没回去的不一致）。
3. 备份 Claude jsonl 到 `data/backups/`，再 `TruncateBeforeUUID` 截断。
4. 若截断后 `HasUserPrompt==false`（即回滚了第一条消息，transcript 变空无法 `--resume`），清空 `ClaudeSessionID`，下一轮开新 session 续接。
5. `ReplaceEvents` 截断 cc-webui 事件日志，`AppendEvent` 追加 `rewind` 标记，`BroadcastEvent` 通知所有端。

### 6. 常驻（resident）与 peer messaging

`Session.Resident` + `Session.PeerName` 控制一个**长驻 streaming-input 进程**：常驻 = 保持进程在线（用 `--input-format stream-json` 双向通信，而非一次性 `-p`）；地址名 = 写到 Claude transcript 末尾的 `custom-title`，CLI 启动时据此在 `~/.claude/sessions/<pid>.json` 注册自己的 peer 名 + `$XDG_RUNTIME_DIR/cc-socks/<pid>.sock` 的 messaging socket。其他 Claude 会话通过内置的 `SendMessage({"to":"<name>","message":...})` 工具发来消息；消息入站后 CLI 在自己的 transcript 写一条 `type:"user",isMeta:true,origin:{kind:"peer",...}`，并由 cc-webui 通过既有 stream 事件通路透传到前端。

实操要点（详见 `peer-rd.md`）：

- **进程模型**：常驻进程不在 `Manager.procs` 走队列式 `work()` 循环，由独立的 `residentProc`（`internal/runner/resident.go`）管 stdin/stdout/状态机。`Status` 加 `Resident bool` + `RegisteredName string` 字段，`GetStatus` 在 `r.resident` 存活时填。同一 `sessionRuntime` 同时持有一次性进程和常驻进程（互斥）——常驻开启期间，新 user 消息走 `runResidentTurn`（写 stdin、收 result）；常驻关闭走原 `runTurn`。
- **注册表查找**：registry 文件按 `<pid>.json` 命名，但 launcher 型 CLI（如 ducc，会 fork 出真正的 claude.exe 子进程）把文件写到**子进程**的 pid 下，cc-webui 手里的 spawn pid 永远查不到。`lookupRegistry`（procBootstrap 轮询 + GetStatus 按需刷新共用）先按 pid 查，查不到再按 claude session id 全目录扫描（`claudehist.ReadRegistryForSession`，registry 的 `sessionId` 字段与 `--resume`/init 事件的 session id 一致）。注册晚于 spawn（慢启动）也会被 GetStatus 的 2s 节流刷新兜住，不会卡"注册中"。
- **初始化握手**：spawn 后写 `{"type":"control_request","request":{"subtype":"initialize","hooks":null}}` 等 CLI 回 `control_response`，**request_id 在响应对象的 `response.request_id` 里**，不是顶层。两 value receive（`resp, ok := <-ch`）防 closed-channel 假成功。15s 超时后即便没收到也放过——老 CLI 可能不实现控制协议，但 stream-json 仍能工作。
- **地址名变更**：写到 transcript 后必须**重启常驻进程**——注册表名是进程启动时固定一次写入的，文件改不刷新。`ApplyPeerName` 在常驻存活且旧名≠新名时 restartResident；`EnsureResident` 的 spawn 路径在新名时也重启一次避免竞态。
- **入站 peer 消息**：CLI 把它当普通 user line 写进 transcript（同时 broadcast 成 stream 事件），recipient 进程不需要特殊处理——它就是把这条消息当成下一轮 user 提示。cc-webui 的 runner 在 `procEvent` 里识别 `d.origin.kind=="peer"` 且进程当前 idle，转 busy="peer" 并 emit `turn_start`；result 回来后 emit `turn_end`/`interrupted` + setIdle。如果是**我们的轮次进行中**注入，peer 消息只渲染、不转状态机（result 仍属于我们的轮次）。
- **rewind 与常驻**：先 `TeardownResident` 再截断 transcript。绝对不能一边留着活进程一边 `TruncateBeforeUUID`——进程内存里的消息历史会跟磁盘分叉。
- **重启恢复**：`main.go` 启动后 `go mgr.RestoreResidents()`（300ms 错峰）；每个 resident session 用 `--resume <ClaudeSessionID>` + 已写入的 custom-title 重新 spawn。进程死的 session 由下一次 `Enqueue` 触发 `EnsureResident` 懒启动。
- **settings 注入**：物化的 `--settings` 文件**无条件**写 `crossSessionInbound:"accept"`——单用户控制台，bypass-permission session 默认 `hold`，headless 下无人能答，消息会积压卡死。repo/managed settings 可收紧但本进程不接受 `hold`。**env 为空的 provider 不物化文件**（见 §3），那些会话没有这个钉死项，入站 peer 消息退回 CLI 默认策略。
- **入站气泡**：`renderEvent` 在 tool_result 分支前判 `d.type === 'user' && d.origin?.kind === 'peer'` → `msg-peer` 气泡（紫色描边，左对齐），用 `origin.body`（已解码）走 `md()` 渲染，`origin.name` 经 `escapeHtml` 注入 role-tag。replay（`claudehist.Convert`）和实时流共用同一渲染路径。
- **前端**：header-actions 加 `📡` 按钮 + popover（toggle 开关 + 地址名输入 + 状态行 + hint）；侧栏 `.resident-tag`；toggle/name 改完走 `PATCH /api/sessions/{id}` 同步到后端，server 顺序：peer_name 先 `claudehist.RenameSession` 写 transcript，再 Upsert store，最后 `EnsureResident`/`ApplyPeerName`。删除会话先 `TeardownResident` 再 `store.Delete`。

### 7. 并发模型

- `store`：两把独立锁——`mu` 保护 sessions map + db.json；`eventsMu` 保护 events map（互不阻塞）。db.json 经 temp+rename 原子写。
- `runner`：`Manager.mu` 保护 `procs` map（按 session 懒建 `sessionRuntime`）；每个 `sessionRuntime` 自带 `mu` 保护 queue/working/cancel/subs/statusCh。worker goroutine `work()` 循环排空 queue。
- 停止：`Setpgid: true` 起进程组，`Stop` 用 `syscall.Kill(-pid, SIGKILL)` 杀整棵树。常驻进程同样走进程组；`TeardownResident` 先 stdin close 软退，5s 不退再发 SIGKILL。
- SSE 订阅：256 缓冲 channel，非阻塞 send（满了丢弃）。

### 8. 四引擎：claude / opencode / mcode / qoder

`config.CLI` 与 `config.Provider` 各带 `kind: claude|opencode|mcode|qoder`（空自动归一化为 claude）。会话绑定单一引擎（**不跨引擎切换**）：`session.engine` = 其 cli 的 kind，create/patch/import 都校验 cli 与 provider kind 一致，不一致 400 `engine mismatch`。前端每处 cli/provider 下拉都按 kind 过滤（`fillCLISelect(sel, value, kind)` 等 helpers，见 `web/app.js`）。

- **claude 引擎**（原架构）：transcript 在 `~/.claude/projects/`，`-p --output-format stream-json`，`--settings` 注入 env，checkpoint/rewind、resident/peer、fork 均可用。会话 `ClaudeSessionID` = claude session id。
- **opencode 引擎**（`internal/opencodehist` 读会话 + `internal/opencodeapi` 跑实况，都有测试）：数据在 opencode 自己的 sqlite store——**主面绝不直读**，只走 CLI 官方面：`session list --format json`（列表）、`export <sid>`（单会话全量 JSON）；**实况走 `opencode serve` HTTP 面而不是 `run` 子进程**（`opencodeapi` 包，见下条）。唯一例外是**只读枚举**：使用统计，以及导入列表在没给 cwd 时的跨项目枚举，走一条固定 `SELECT` 经 `opencode db`（只读，见 section 9），失败都有回落；回放/实况/导入执行这些面仍不得碰 db。`Session.ClaudeSessionID` 字段复用来存 opencode 的 `ses_...`。
  - **模型列表是动态的**（与 mcode/qoder 同策略）：`opencode models <provider> --verbose`，`opencodehist.ProviderCatalog` 一次解析同时产出模型清单（id + `capabilities.input.image`）与每个模型的 variants 键。**所以 config 里 opencode provider 的 `models: []` 留空是对的**——模型上下线、套餐变化都由上游决定，写死必然过期；`modelKnown` 对 opencode 改查目录而非 config 列表，目录读不到时跳过校验（宁可让首轮报 CLI 自己的错，也不该因此建不了会话）。config 里**已声明**的列表仍然优先，所以老配置不清空也能照常工作。
  - **模型名取 JSON 的 `id`，不是标题行**：输出是 `providerID/modelID` 一行 + 该模型的 JSON 块，而两者**在 model id 自带 provider 前缀时不相等**——cline-pass 每个模型打印 `cline-pass/cline-pass/deepseek-v4-pro`，其 `id` 是 `cline-pass/deepseek-v4-pro`（opencode-go 则相反，`id` 是裸 id）。runner 的 `openCodeModelRef` 把 config 的模型名原样当 `ModelRef.ID` 传出去（且 `ProviderID` 取 cc-webui 的 provider 名，**所以 provider 名必须等于 opencode 的 providerID**），照抄标题行会给每个 cline-pass 模型寻址错。`Catalog.Find` 也只认 `id`。
  - **`--refresh` 是唯一的刷新手段，且很贵**：不带它的 `opencode models` 读 `~/.cache/opencode/models.json`（实测 2.6s，**不联网**，文件 mtime 不变）；只有 `--refresh` 才回 models.dev。服务端 `opencodeCatalog` 因此是 stale-while-revalidate：冷启同步拉一次（走磁盘缓存那次），未过期直接返回，**过期先立刻返回旧值、后台 goroutine 跑 `--refresh`**，结果留给下一个读者；`ocRefreshing` 防惊群（实测 6 次 `fresh=1` 只触发 1 次刷新）；刷新失败只记日志、保留旧目录，绝不把能用的下拉清空。`/api/opencode-models` 与 `/api/opencode-model-variants` **共用同一个 memo**，所以开一个会话不会为了档位再多跑一次 CLI。
- **mcode 引擎**（`internal/mcodehist`，有测试）：MiniMax Code（`mcode` 0.5.9），**协议与前两者都不同**，是独立的第三套 shape，不能套 claude 或 opencode。实况 `mcode exec --output-format stream-json [--session <mvs_…>] [--model <providerId>/<modelId>] [--effort <level>] [--permission full|smart] [--file <path>…] <prompt>`（位置参数必须排在 `--file` 之前，它是数组 flag，会吞掉尾随的位置参数）。`Session.ClaudeSessionID` 复用存 `mvs_…`。
  - **事件适配**：live 与 replay 共用 `mcodehist` 的同一套映射，产出与 claude 同形的 canonical 事件。`item.completed`(agent_message) → assistant text；`item.completed`(reasoning) → assistant **thinking**（mcode 实况确实吐 reasoning，所以透传以保证刷新后与实时一致——这与 claude 的 strip、opencode 的不吐都不同）；`item.started`(tool_call, status 1) → tool_use；`item.completed`(tool_call, status 2/3) → tool_result（3 = 失败，带 `is_error`）。**只适配 `item.completed` 的全量内容，不碰 `item.started`/`item.updated` 的 `contentDelta` 增量**——`item.completed` 带全文，两条路径因此产出逐字节一致的块。tool_call status 码实测：4/5=pending（无 input，不渲染）、1=input 就绪、2=成功、3=失败。`turn.completed` 的 usage 不属于任何 assistant 消息，单独发一条 **content 为空数组**的 assistant 事件（前端不渲染，统计照常读 `message.usage`）。
  - **usage 归一化**：mcode 自己的键是 `{input,output,cacheRead,cacheWrite,totalTokens,cost}`，live（stream 事件）与 replay（transcript）**都**经 `normalizeUsage`/`normalizeTranscriptUsage` 映射成 claude 那套 snake_case，两条路径形状一致，跨引擎也一致。
  - **transcript 存储**：`~/.minimax/v2/sessions/YYYY/MM/DD/HH-MM-SS-mmm-session_<base64(sid)>/`，含 `manifest.json`（**session id 的唯一权威来源**）+ `messages.jsonl`（`{message_id, turn_id, message:{role,content:[…]}}`，content 块类型 `text`/`thinking`/`toolCall`，工具结果在独立的 `role:"toolResult"` 消息里）。mcode 没有任何 `session list`/`export` CLI 面，所以和 claude 一样直读文件；目录按 `manifest.sessionId` 匹配（目录名里的 base64 只是快速筛选，不是判据）。
  - **user 消息带注入前缀**：mcode 给每条存下来的 user 消息前面塞 `<system-reminder>…</system-reminder>`（运行时上下文）。回放与统计都必须先 `systemReminderRe` 剥掉，否则与 cc-webui 自己存的 echo 文本对不上，`mergeStoredAndClaude` 的 count 去重失效、刷新后用户气泡重复。
  - **模型列表是动态的**：`mcode provider list --json` 对每个 provider 都返回 `models: []`（真实集合由凭据源运行时解析），ACP 的 `session/new` 要先完成整轮 JSON-RPC 握手才应答——都不可用。真正的清单在 mcode 自己的运行时配置 `~/.minimax/config.yaml`（`provider.<pid>.models` / `model_order` / `thinking.effortOptions` / `variants` / `modalities.input`），`mcodehist.LoadCatalog` 读它，服务端经 `/api/mcode-models` 提供（10 分钟 TTL，`?fresh=1` 绕过），前端模型下拉与 effort 下拉都走它。**所以 config 里 mcode provider 的 `models: []` 留空是对的**——写死必然过期；`models` 校验（`Server.modelKnown`）对 mcode 改查目录而非 config 列表，目录读不到时跳过校验（宁可让首轮报 CLI 自己的错，也不该因此建不了会话）。
  - **effort 档位随模型而异**：`thinking.effortOptions`（M3.1：default/low/medium/high/xhigh/max，CLI 自己会报这个列表）优先，没有则退回 `variants` 的键（M3：none-thinking/thinking），两者都没有（如 forced_on 的 M2.7）就没有档位、下拉隐藏。服务端 PATCH 时按目录精确校验（400 并列出可选值），不是只查形状——非法值否则要等 spawn 后才被 CLI 拒绝。
  - **mcode 不读它的 sqlite**：会话 cwd/title 其实在 `runtime-state.sqlite` 的 `local_runtime_sessions` 里（实测查询前后该库及 -wal/-shm 三个文件 md5 完全一致，只读），但那需要链接 sqlite 驱动，与"单文件二进制 + 唯一外部依赖 yaml"的约束冲突，所以**不读**。代价：导入会话时 `cwd` 未知，导入弹窗对 mcode 把该输入框从"过滤"变成"导入时必须填写的工作区"（服务端 400 拦住空值）。
  - **mcode 禁用的协议面**（服务端 400 + 前端隐藏）：resident/peer messaging、fork、rewind/checkpoint、内嵌图片 base64（图片降级为 `--file` 附件，与 opencode 同样处理）；工作区变更**不迁移** transcript（它按 session id 存自己的库，不在工作区里）。
  - **resume 命令**：`mcode --session <mvs_…>`（TUI 形式，模型/档位由 session 自身带，不追加 `--model`/`--effort`）。
- **qoder 引擎**（Qoder CLI CN，`qoderclicn` 1.1.65）：**协议与 claude 同构**——它是 Claude Code 的 fork，`-p --output-format stream-json` 的 stdout 事件形状、`system/init` + `assistant.message.content[]` + `result` 全部一致，transcript 也同构（`~/.qoder-cn/projects/<slug>/<sid>.jsonl` + `<sid>/subagents/agent-<type>-<hash>.jsonl` 与同名 `.meta.json`，键名与 claude 相同），所以**没有新 hist 包**：`claudehist.Profile{Home,Engine}` 参数化 home 目录与统计用的 engine kind（`ClaudeProfile` / `QoderProfile`），`Convert/ConvertSubagents/List/Preview/Move/Rename/Truncate/HasUserPrompt` 都有 `*In` 变体，原函数是 claude 拼法。runner 侧共用一条 spawn 路径 `runStreamJSONTurn`，只有 argv 拼法不同，由 `streamJSONDialect` 表达。`Session.ClaudeSessionID` 复用存 qoder 的 session id。
  - **argv 差异（实测，多一个 flag 就整轮 exit 1）**：**没有 `--verbose`**（claude 必需，qoder 报 `unknown option '--verbose'`）；档位 flag 是 **`--reasoning-effort`**（`--effort` 被拒），取值 `auto/none/low/medium/high/xhigh/max/ultracode`（超集于 claude 的 `low..max`，非法值 CLI 会自己列出这八项——服务端 `qoderEfforts` 就是照它抄的，`/api/qoder-models` 一起下发，前端不再自带副本）；`--permission-mode` 收 **snake_case**（`snakeCasePermissionMode` 把 config 的 `bypassPermissions` 转成 `bypass_permissions`）；`--settings` 仍支持（env 非空才物化，规则与 claude 一样）。
  - **模型列表是动态的**：`qoderclicn --list-models` 是唯一来源（`~/.qoder-cn/.models/<uid>/catalog-v6` 是加密的，读不了）。所以 config 里 qoder provider 的 `models: []` 留空是对的；`parseQoderModels` 解析那张单列表（取每行第一段、跳过 `MODEL` 表头、丢掉不像模型名的行——未登录时 CLI 也在 stdout 报错，否则 `error:` 会被当成模型缓存 10 分钟），`/api/qoder-models` 10 分钟 TTL 提供给前端模型下拉，`modelKnown` 对 qoder 改查该清单而不是 config。输入模态一律声明 `text+image`（Flash 实测能读 base64 图，CLI 不给模态元数据）。
  - **transcript 里的模型名是内部 key**：每条 assistant 行与 `runtime-config` 记的都是 `qfmodel`，磁盘上**从头到尾没有显示名**，所以统计按模型分桶会全部塌进一个无意义桶。`handleStats` 用 `qoderModelHints()`（session id → 该会话 spawn 时用的 `--model`，即 `ModelOverride` 回退 provider 默认）经 `StatsCache.RefreshWithModels` 盖掉；未被 cc-webui 托管的会话无从得知，退回 `qfmodel`。
  - **token 数恒为 0**：`usage` 各字段全是 0，计费走 `credits`/`billable` 字段（`billable:false` = 免费模型）。所以 qoder 会话的消息数/工具数/活跃天数/连续天数都准，**token 类 tile 与热力图强度对它无意义**——`statsagg` 没有 credits 维度，要按额度统计得另开一个 feature，别为它改 statsagg。
  - **qoder 禁用的协议面**（服务端 400 + 前端隐藏）：resident/peer messaging（peer 靠 claude 的 `SendMessage` + `~/.claude/sessions/<pid>.json` 注册表，qoder 的工具表里没有 SendMessage）、**rewind/checkpoint**（没有 `--rewind-files`，文件回滚这一半做不了，所以 dialect 的 `checkpoint` 关掉、不产生 checkpoint uuid）。**fork 可用**（transcript 就是个可复制的文件，`forkHistoryIn` 参数化后 qoder 走同一条路）；**内嵌图片 base64 可用**（与 claude 同一个多模态 stdin 载荷，实测经 cc-webui 也能正确读出左右半图颜色）。前端「图片是否内嵌」不再用 `kind !== 'claude'` 判定，统一走 `engineTakesImages(kind)`。工作区变更**会**迁移 transcript（`MoveSessionIn`，同 claude）。
  - **计费分档**：`--list-models` 不标 free/paid，但响应里 `usage.billable` 会说明——实测 Qwen3.8-Flash `billable:false`（免费），Qwen3.8-Max 走积分且额度用尽会直接失败（`Your personal Credits have been exhausted`）。

- **canonical 事件形状**：两条路径（claude `Convert` / opencode `AdaptPart`+`ToEvents`）产出同形事件——assistant text 每 part 一条 `{type:"assistant",message:{role,id,content:[{type:"text",id:partId,...}]}}`（frontend trajectory 按 message.id 合并、fingerprint 靠 text block 的 part id 区分），tool 是 `tool_use` assistant 块 + 完成/报错时的 `tool_result` user 块（`ToolTx` 按 callID 去重 running→completed 双发）。**reasoning → assistant thinking block**（`{type:"thinking",id:partId,thinking:…}`，与 claude 引擎、mcode 同形，前端折叠成 `thinking…` 卡片）——**两条路径都映射**，不一致才是问题。patch/step-* 才是一律跳过。
- **opencode 实况不回显用户 prompt**：cc-webui 自己落 stored user echo；但 opencode export 的用户文本带一层**回显双引号**（`"prompt"`），replay 前必须 `stripEchoQuotes` 剥掉，否则与 stored echo 文本不一致，`mergeStoredAndClaude` 的 count 去重失效、刷新后用户气泡重复。
- **`session list` 按项目分桶**：只返回进程 cwd 所属桶（git 根 = 该仓库桶，非 git 目录 = `global` 桶）；`export`/`run -s` 则全量可按 id。API `?cwd=` 选桶（空 = 全部项目，经 `opencodehist.ListAll` 的 db 只读枚举，失败回落服务器自身 cwd 的桶）；`run` 永远 `cmd.Dir = sess.Cwd` 回到会话原目录，恢复不跨桶。绕过分桶的**只有只读枚举**（`opencodehist.ListAll`，见 section 9：统计，以及导入列表的空 cwd 路径）；给了 cwd 的导入列表/会话列表仍按桶，回放/实况/导入执行这些面一步都不越界。
- **opencode 实况 = `opencode serve`（不是 `run`）**：`run` 在 1.18.35 里硬编码一组 deny 规则（question/plan_enter/plan_exit），并在事件循环里把 `permission.asked` 直接回成 `once`/`reject`，`--format json` 的 emitter 根本不序列化 permission/question 事件——**结构上就带不了审批卡**。serve 面把这些全开：`POST /session`（permission ruleset 落在这里）、`POST /api/session/{id}/prompt`、`GET /event`（SSE）、`POST /api/session/{id}/permission/{rid}/reply`（`once|always|reject`）、`POST /api/session/{id}/question/{rid}/reply`（`{answers:[["标签"],…]}`）与 `/reject`、`POST /api/session/{id}/interrupt`（停止）、`POST /api/session/{id}/revert/stage` + `/revert/commit`（rewind）。**只剩 resident/peer messaging 与 fork 仍禁用**（cc-webui 服务端 400 / 前端隐藏）。
- **推理深度 = `--variant`（不是 effort）**：opencode 侧每个模型的 variants 键集合由模型 metadata 决定且差异巨大（`opencode models <provider> --verbose`：gpt-5.6-luna 有 none/low/medium/high/xhigh/max，kimi-k3 只有 max，minimax-m3 是 none/thinking，还有模型完全不带）。`opencodehist.ModelVariants` 解析该输出（`parseModelsOutput` 纯函数有单测），server `/api/opencode-model-variants?cli=&provider=` 按 cli+provider 缓存 10 分钟（`?fresh=1` 绕过）；前端 `ocVariantsFor` 缓存也带 10 分钟 TTL 且每次真拉都带 `fresh=1`——否则新模型上线后旧映射缺条目会让它的 effort 下拉凭空消失，而重开页才恢复；config 热加载改动了模型列表时 `refreshProfiles` 直接清空前端缓存；effort 校验按 engine 分流（`effortOKFor`：claude 固定 low..max 集合，opencode 只限形状）。前端四处 effort 下拉（session/sheet/ns/new）由 `refreshSessionEffort`/`refreshNsEffort`/`refreshNewEffort` 按当前模型 variants 键动态重建——`EFFORT_STATIC` 页面加载即快照 claude 静态全集（首个会话是 opencode 也不会污染），无 variants 的模型隐藏下拉。`Session.EffortOverride` 对 opencode 会话经 runner 追加 `--variant`。
- **图片/文件 = prompt 的 `files[]`**：serve 面 `POST /api/session/{id}/prompt` 的 body 是 `{prompt:{text,files[]}}`（注意不是 `parts[]`——`parts` 是 v1 `prompt_async` 的形状）。附件带 `uri`：上传图片用 `data:<mime>;base64,<data>` **原样内嵌**（`run -f` 时代没有这条通道，图片只能降级成路径 mention），磁盘文件用 `file://<path>`。opencode 的 stored echo 只存 `msg.Text`。
- **export 的 user 消息在带附件时会多出三个 part**：工具前导文本（`Called the Read tool with the following input: ...`）、`<path>/<type>/<content>` 文件展开文本、`type:"file"` part——`userPromptText` 过滤后只剩 prompt echo，否则回放气泡会带文件转储且 echo 失配重复。
- **实况 prompt 必须走 v1（`POST /session/{id}/prompt_async`），不能用 v2 的 `/api/session/{id}/prompt`**——这是 1.18.35 的真坑：v2 那个端点只**入队不执行**，10ms 就返回 `{admittedSeq}`，turn 永远不启动，**没有任何报错**（表现就是「发消息没反应」，库里 user 消息在、assistant 一条没有）。实测对照：v1 prompt_async + v1 body → `session.status busy` → assistant 消息 → `message.part.delta` 流式 → idle，模型真实回答；v2 prompt → tokens 恒为 0/0。`opencode run` 自己用的也是 v1（run.ts 调 v1 `session.prompt`）。v1 body 是 `{model:{providerID,modelID}, parts:[{type:"text"|"file",…}]}`，**不是** v2 的 `{prompt:{text,files}}`。别"顺手现代化"到 v2。
- **`/event` 的 sessionID 在 properties 顶层**：每条事件都是 `{"sessionID":"ses_x", …}`，**不是** v1 那种再嵌一层 `properties`。`Event.view()` 先读顶层、缺失才回落嵌套 v1 形状。踩过的坑：只按嵌套解析时 `SessionIDOf` 对 `session.status` 返回空 → Hub 把 idle 事件**丢弃** → `waitTurn` 永久阻塞 → 模型明明答了，前端一直转圈。两个解码入口（`SessionIDOf` / `Next` / `Part` / `PermissionAsked` / `QuestionAsked` / `SessionError`）都走同一个 `view()`，别只改一个。
- **serveKey 必须是「凭据白名单」，绝不能用黑名单**：输入 `buildEnvMap` 起手就是整个 `os.Environ()`，早期版本只黑名单掉 9 个变量（PATH/HOME/TERM…），结果 55 个变量里有 48 个进了 key，包含 SHLVL / `_` / OLDPWD / STY / SECURITYSESSIONID / ITERM_SESSION_ID —— **每个 shell 启动都不同**，于是每次重启 cc-webui（或换个终端）都算出新 key、新起一个 `opencode serve`，旧的被 launchd 收养变孤儿，手里还攥着 provider 凭据。现在 `credentialEnv(k)` 白名单：`*_API_KEY` / `*_AUTH_TOKEN` / `*_ACCESS_TOKEN` / `*_BASE_URL` / `*_API_HOST` / `*_SECRET*` / `*_API_BASE` / PROXY 族 / 显式模型变量。实测 key 从 ~2500 字节降到 119 字节，两个环境迥异的独立进程算出**逐字节相同**。
- **退出必须回收 serve 子进程，且信号处理要注册在 `srv.Run()` 之前**（`Run()` 阻塞到进程结束，注册在它之后的 handler 是死代码，永远不执行）。链路：`signal.Notify(SIGINT/SIGTERM)` → `srv.Shutdown(ctx)` → `defer mgr.Close()` kill 进程组。screen/tmux 里 Ctrl-C 发的是 SIGINT，没 handler 就是默认杀死。
- **SIGKILL 兜底 = pid 文件**：每起一个 serve 把 pid 追加到 `<data_dir>/opencode-serve.pids`；**下次启动先 `reapOrphanServes()`**，把文件里的 pid 逐个 `isOpenCodeServe()` 校验后 SIGKILL 再清空文件。校验必须做——pid 会复用，不能盲杀；且只匹配 `opencode serve` argv，**用户自己的 `opencode -s`（TUI）和 `opencode run` 必须放过**。同一 data_dir 同一时刻只有一个 cc-webui，所以文件里的 pid 都属于已死实例。
- **轮次边界 = `session.status`**：v1 prompt_async 立即返回 204，唯一的完成信号是 `session.status` 的 idle（`{"status":{"type":"busy"}}` → `{"type":"idle"}}`）。`turn.waitTurn` 等它，否则轮次永不关闭。同一条流上还会有 `message.updated` / `message.part.updated` / `message.part.delta`（真正的内容增量）。
- **模型热切换**：opencode 在**建 session 时**绑定 model+variance，而 claude/qoder/mcode 每轮重读 `sess.ModelOverride`——所以 opencode 对**已存在**的 session 每轮先 `ensureModel`：`SessionModel` 读当前值，不同就 `POST /api/session/{id}/model {model:{id,providerID,variant}}`（实测 204，且 `GET /session/{id}` 立即反映）。缺这一步的话前端换了模型/推理等级、UI 显示新档位，opencode 仍用建会话时的旧模型——**静默说谎**。实测该写入落在共享存储，**换 serve 进程（切 provider）后依然生效**，不必重建会话。新建 session 走 create 自带 model，不必再切。
- **session 创建**：`POST /session?directory=<sess.Cwd>`（directory 走 query 不走 body），body `{title, model:{id,providerID,variant}, permission:[{permission,pattern,action}]}`。模型/reasoning 深度（= variant）在**建 session 时**定，不是每次 prompt 传。`permission` ruleset 是**扁平数组**（非对象），action 只收 `allow|ask|deny`——`always` 只存在于 reply 端点。
- **token 级流式 = `message.part.delta`，它没有 `part` 字段**：`{partID, field, delta}`，完整 part 要等后面的 `message.part.updated` 才到。只认 `Part()`（要求 `pe.Part != nil`）会把增量全丢掉，表现为**整段回答最后一次性蹦出来**。runner 用 `Event.PartDelta()` 取出，透成**独立事件类型 `oc_text_delta`**（不是 canonical assistant block——前端每来一个 canonical text block 就 `addMsg` 建一个独立气泡，发增量会碎成一堆气泡）。前端 `renderOcTextDelta` 按 **partID** 聚进同一气泡，随后的完整 text block（带同 id）在 `renderEvent` 里 `liveFinalizeBlock` + 原文替换，不重复。切会话/重连要清 `state.ocBubble/ocPartID/ocBuf`（它不属于 `state.live`）。
- **reasoning 的增量与正文长得一模一样，必须靠 part 公告区分**：`message.part.delta` 只有 `{partID, field, delta}`，而 **reasoning part 同样用 `field:"text"` 推流**——`field` 区分不了。实测事件顺序是 `message.part.updated`（带 `part.type`，此时 text 还是空）**先到**，该 part 的 delta 才跟在其后（3 个 step、5 段全部如此），所以 `turn.partTypes` 记 partID→type，`turn.streamable` 只放行 `type=="text"`。漏了这一步的表现就是**模型内心独白被当成正常回答显示**（实测泄漏原文：`The user is asking me to run \`ls -1 …\` using the bash tool…`）。**没被公告过的 partID 一律不流式**：丢公告最多少一次逐字渲染（完整 part 照样到），漏 reasoning 则收不回来。**reasoning 的 delta 仍然丢弃、不做 thinking 流式**——thinking 块在 reasoning part 跑完时由 `AdaptPart` 一次性产出（前端零改动：opencode 侧 `state.live` 恒为 null，`liveTake('thinking')` 直接返回 null，走的是既有 `block.type==='thinking'` 分支）。代价是思考要等整段跑完才出现，换来不写任何未经浏览器实测的前端代码。回归测试 `TestOpenCodeTurnSkipsReasoningDeltas`。顺带实测：**tool part 根本不发 delta**（只有 pending→running→completed 的 `message.part.updated`），所以这道闸不影响工具卡。
- **tool_use 必须等 `state.input` 非空再发**：opencode 的 tool part 走 pending → running → completed，**第一个 pending 的 `state.input` 是 `{}`**，参数下一拍才填。此刻发块就得到「有工具名没参数」的卡，而 `ToolTx.assistantSent[callID]` 已标记，之后 running/completed 都不会补发。`adaptTool` 改为 input 为空时 `return nil` 等下一拍。回放不受影响（历史 part 的 input 都有值）。
- **用户 prompt 会作为 text part 回到 part 总线上，而 part 本身不带 role**：`message.part.updated` 先来一条 `{type:"text",text:"<用户原话>"}`（`messageID` 属于 role=user），随后才是 assistant 的 step/reasoning/text。渲染器若无差别适配，**用户那句话会被当成模型回复显示出来**（实测：问「介绍下当前的项目吧」，界面把这句原样标成 CLAUDE 回复）。`turn.msgRoles` 用 `message.updated`（带 `info.role`）建立 id→role 映射，`message.updated` 严格先于该消息的 part 到达，所以查表可靠；查不到就**放行**，宁可多渲染也不能吞掉模型输出。
- **事件流有两套方言都要认**：`/event` 同时可能推 v1 的 `message.part.updated`（整 part，走 `opencodehist.AdaptPart`）与当前的 `session.next.*` 生命周期（`prompted`/`step.started`/`text.*`/`step.failed`/`session.idle`，字段全在 properties 顶层）。`session.idle` 是**轮次边界**——serve 是共享长驻进程，没有子进程可退出，轮次必须由事件流关闭。

### 9. 使用统计（四引擎合并）

`GET /api/stats[?fresh=1]` 出 8 个 tile + GitHub 式热力图，**claude、opencode、mcode、qoder 四引擎会话合并成一份统计**，按 `engines` map 拆出各自贡献的会话数（前端 `#stats-scope` 显示 `Claude Code N 个 + opencode M 个 + mcode K 个 + qoder J 个`）。

- **管线**：四个引擎各自把会话聚成 `statsagg.FileAgg`（自包含的每会话聚合，字段导出）→ `statsagg.Recompute` 纯算术合并。**计数规则由每个引擎各自实现并保持一致**（user prompt 与 assistant 步各计一条消息，工具调用按 callID 去重计调用，usage 每步计一次），口径变了要各侧同步。
- **`statsagg` 是叶子包**（只依赖 `config`），被 `claudehist`、`opencodehist` 与 `mcodehist` 同时依赖——**别**为了共享代码让 `opencodehist`/`mcodehist` 反向依赖 `claudehist`。`FavoriteModel`/最长会话并列时按字典序/更小的 fileID 决定，`Recompute` 遍历 `Sorted(fas)`，保证结果确定。
- **claude 侧**：mtime+size 指纹缓存（`claudehist.StatsCache`），只有变动的 transcript 重扫。
- **qoder 侧**：与 claude 共用同一个 reader 和同一个指纹缓存机制，只是 root 不同、engine kind 不同（各自一个 `StatsCache`），外加一层模型名修正 `RefreshWithModels`（见 section 8 的「transcript 里的模型名是内部 key」）。
- **mcode 侧**（`mcodehist.StatsCache`）：比 opencode 便宜得多——transcript 是本地文件而非 CLI export，全量重扫是毫秒级而不是每会话 ~1s，因此不需要 `opencode db` 那样的枚举面、不需要并发 worker、不需要桶目录兜底：`List()` 走日期目录树，`FetchSession` 直读 `messages.jsonl`，用 transcript 的 size+mtime 指纹做增量（与 claude 侧同款）。usage 键是 mcode 自有格式，在 `ScanSession` 里就地归一化，**不要**为它改 statsagg（那会破坏 statsagg 只依赖 config 的叶子约束）。一次枚举都没成功时同样整体保留上次缓存。
- **opencode 侧**（`opencodehist.StatsCache`）：只能经 CLI 取数——先枚举全部会话（见下条），再只对 `updated` 前进过的会话 `export`（~1s/会话，长会话几 MB），**8 路并发**；`fresh=1` 或配置的 opencode cli 集变化（`sig`）时整缓存作废重导。导出失败的会话**保留旧聚合**（不当作删除，下次刷新重试）；**一次枚举都没成功**（db 查询与全部桶列表都失败，CLI 挂了）则整体保留上次缓存，避免一次环境问题把统计清零——注意这与「db 查询成功但返回空」不同，后者按真实结果清缓存。
- **枚举 = 一条只读 `opencode db` SELECT（唯一触碰 sqlite 的地方）**：`session list` 只回当前 cwd 所属桶、opencode 又没有列全部桶的 CLI 面，所以枚举走 `opencodehist.ListAll`：`opencode db "SELECT id, title, directory, time_created, time_updated FROM session WHERE parent_id IS NULL ORDER BY time_updated DESC" --format json`（~1s，一次拿全所有项目，本机 30 行）。SQL 是包内常量、无用户输入参与拼接；实测 `opencode.db` / `-wal` / `-shm` 三个文件在查询前后 md5、inode、size、mtime_ns 完全一致（库是 WAL 模式，查询期间还有活的 `opencode run`），**只读不写**——这是把 section 8「绝不直读」收窄为「只允许只读枚举」的依据（两个消费者：统计，以及导入列表的空 cwd 路径），但 `opencode db` 本身接受任意 SQL、没有 `--read-only` 开关，只读是纪律而非强制。`WHERE parent_id IS NULL` 对齐 `session list`（它隐藏 subagent 子会话；子会话的消息不在父会话的 export 里，列出来会把子代理任务当成顶层会话）。这条查询成功后**不再碰桶枚举**，所以 server 派生的桶目录候选只是兜底路径（导入列表的兜底更简单：直接回落到服务进程所在桶的 `session list`）。
- **兜底 = 按桶枚举**：表名/列名是 opencode 内部实现，可能变。db 查询失败时退回 `session list` 按桶扫——桶集合从 cc-webui 已知的 cwd 推导（服务器 cwd + 所有托管会话的 cwd，归一到 git 根，顺带保留 cwd 本身），跨桶按 session id 去重（id 全局唯一）。这条路覆盖不全：**只在 opencode TUI 里、cc-webui 从未用到的仓库建的会话不进统计**（导入列表仍能列到它们：空 cwd 走的就是 db 枚举，只有 db 挂了才退化成按桶扫）。
- **模型归属**：opencode 的 `export` 里 **assistant 步不带 model 字段**（user 步用 `model.modelID`，会话级 `info.model` 用 `id`——同一份文档两种键混用）。统计按步优先用步上模型、为空回退会话级模型，key 形如 `providerID/modelID`（与 `-m` 模型串同形），缺半边退回另一半边而非 `unknown`。因此 opencode 的 per-model 口径**是会话级模型的近似**，步内换模型分不开。
- **tokens**：`ModelUsage` 有独立的 `reasoning_tokens`（opencode 单独上报，claude stream-json usage 无此字段），`TotalTokens()` 含它，所以按日与按模型两条汇总口径一致。实测 27 个真实 opencode 会话的分步累加与 opencode 自己的会话级 `info.tokens` 完全相等。
- **性能**：首次冷扫（本机 571 claude + 30 opencode 会话）约 25s（瓶颈是 8 路并发的 `export`，不是枚举——一条 db SELECT 只要 ~1s），随后命中缓存约 1.1s。`handleStats` 全程持 `server.statsMu`（串起各引擎缓存，不并发）；HTTP 无读写超时，长冷扫不会被掐断。前端 stale-while-revalidate（`lastStats`），首次提示「统计中…（首次扫描较慢）」。

### 10. 群聊（多 agent 并行编排）

`server/groups.go` 是 HTTP 与校验层，`runner/group.go` 是编排层。一个群拥有每个成员一份**影子会话**（`store.Session.GroupID`，侧栏隐藏、统计排除、可按 id 直接打开看单独回放），群本身另有独立 transcript（`data/groups/<gid>/transcript.jsonl`，只有干净的 user/member 正文）。一轮里所有目标成员**并发**跑同一份历史快照（`composeGroupPrompt`，成员 resume 上下文已有的不再重发，只注 peers 自其 `Cursor` 以来的回复）；轮与轮之间串行（忙时 409 `turn_busy`）。

- **成员档案存两份**：影子会话 `store.Session`（真正 spawn 用的那份）与 `runner.GroupMember`（群视图渲染用的那份）。建群与 `PATCH /api/groups/{id}/members/{memberId}` 都必须同改两处，否则界面显示的和实际跑的不是一回事。改档从**下一轮**生效，不打断在跑的成员。
- **换 provider 不带新模型**时旧 model override 退回该 provider 默认（与 `handlePatchSession` 同规则），而不是 400。
- 档案校验走 `server.resolveProfile`（cli/provider 存在且 engine kind 一致）与 `server.validateEffort`（按 engine 分流：claude 固定集 / opencode 任意键 / qoder 固定集 / mcode 查它自己的模型目录）——**建群、改成员、改单聊共用这两个函数**，别再各写一份。
- 前端成员档案下拉复用单聊那套（`fillCLISelect`/`fillModelSelect`/`applyEffortOptions`），建群弹窗的成员行与群视图的成员面板共用 `ng-*` 类名，联动只有 `refreshNgEffort` 一份。动态建的 effort 下拉没有 id，进不了 `EFFORT_STATIC` 的按 id 快照，claude 模板由 `EFFORT_CLAUDE_HTML` 显式传入。
- **群时间线只由 SSE 回放画**（与单聊一致）：`openGroup` 拉到的 `data.transcript` **不要**自己渲染，紧接着 `connectGroupEvents` 的回放会把同一批条目再发一遍，而 `state.groupSeen` 刚被清空、拦不住——两条路径都画就是每条消息两遍。断线重连走 `es.onopen → openGroup`（清空 + 新连接回放），因此同样只有一份。**别在群视图加第二处渲染入口。**
- **成员竖线配色**：`--agent-1..8` 在 `:root` 与 `[data-theme="dark"]` 各定义一份（沿用 GitHub 那套色，深色版同色相提亮）。**顺序刻意冷暖交替**（蓝→琥珀→绿→品红→青→红→紫→金棕），并排发言的两个成员（成员 1 和 2）一定不同色；`applyMemberColor` 按成员在 `meta.members` 里的**序号**取色，写的是 `var(--agent-N)` 字符串而非色值，所以切主题靠 CSS 变量重解析、不用重渲染。成员增删会让后面成员换色，认人仍以 role-tag 的名字为准，颜色只是辅助。
- **@成员补全**：群 composer 打 `@` 弹 `#group-mention-popup`（`@all` 固定第一项 + 本群成员，右侧带模型/后端提示），↑↓/Tab/Enter 选中、鼠标点选用 `mousedown`+`preventDefault`（先于 blur 的延时关闭，否则 click 会被抢掉）。触发条件要和服务端对齐：`parseGroupRecipients` 按空白切 token 且要求 token 以 `@` 开头，所以 `currentMentionToken` 只认词首的 `@`（开头/空白/中文后），选中时若前一个字不是空白**补一个空格**——中文里「看下@小王」这种写法不补就切不出 token，这条 @ 白发。邮箱/URL 里的 `@` 不弹。

### 11. 待回答提问的强提示（AskUserQuestion）

模型的提问（AskUserQuestion / opencode 的 `question.asked`）有 **55s 硬超时**——helper 到点自动拒绝，AskUserQuestion 那档回传「请稍后重新提问」让模型重问。原来的呈现只有一处：卡片停在**当前打开会话**的 composer 上方 dock 里。后果是致命的两个盲区：

- **SSE 只订阅当前会话**（§3 每连接一套事件），换个会话就收不到那条 `permission_request`；
- 卡片不进消息流，不刷新就永远看不见。

于是「模型在提问」变成了纯静默事件，用户往往等到超时后模型重问才发现。三层提示都建立在同一个事实上——服务端内存里的 pending 注册表（`runner.permissionState.pending`）本来就知道「谁在提问」，只是没人把它铺到界面上：

1. **数据面**：`sessionView.PendingAsks` / `groupView.PendingAsks` 随列表下发（见「前端结构」段的条目）。走会话/群列表而不是新开接口，是因为 5s 轮询本来就在拉这两个列表，零额外往返。
2. **触发面**：三条源（当前会话 SSE、刷新补卡、轮询 diff）按提问 id 去重，`raiseAskToast` 幂等。**轮询那条是唯一跨会话的通道**，别删。
3. **呈现面**：**顶部居中的横幅通知**（iPad 来通知那种形态，整条可点 = `gotoAsk` 跳到那张卡；跨会话开会话、群卡开群视图，定位靠 `focusAskCard` 轮询等异步补卡出现）+ 侧栏**整行底色脉动**（`.session-item.has-ask`，红点只作补充）+ 工作区栏 `#ws-activity` 的「待回答 N」徽标 + `renderWsPopPending` 列表。**别只做浮层**——人往往在别的标签页/会话，侧栏那一条才是回来第一眼看到的。

踩过的坑：

- **浮层放底部等于没做，藏角落也等于没做**（两种都实测过）：composer 是 `position: absolute` + `max-width: 880px` 居中浮在底部，待回答的卡片就在它正上方的 dock 里——浮层压那儿等于挡住要作答的地方；而右上角一个 360px 的小卡片在真实使用中根本不会被注意到。最后定**顶部居中横幅**：要立刻做决定的一次性事件，视线落点就在那儿，不用找。
- **侧栏只改状态点太弱**：一屏几十行会话，8px 红点在扫视里等于不存在，必须让**整行底色**脉动才跳得出来（`bindSessionItem`/`appendGroupItem` 两处都要加 class）。
- **退场动画会和「挤掉旧提示」打架**：`dropToast` 改成先加 `.leaving`、延迟摘节点之后，`layer.children.length` 就不再等于在场条数，`pushToast` 的挤占循环必须改成只数非 leaving 的，否则死循环。
- **不用系统横幅**：要授权、要窗口聚焦，而这是 55s 的一次性窗口。页内浮层不依赖任何授权；提示音仍走既有的 `notifyPrefs()` 开关（`playAskSound` 是从 `playNotifySound` 拆出来的 `playTones`）。
- **跳转要能等**：`openSession`/`openGroup` 里的「补未决卡」是 fire-and-forget，所以 `focusAskCard` 轮询等卡片出现再闪烁+聚焦，不能同步取。
- 群成员的卡只在群视图里，转发事件带 `group_id`（`forwardGroupPermission`）才能「去回答」时回到群视图而不是跳去成员影子会话。

### 12. Session Goals（自动续跑到达成条件）

会话上可挂一条「一直干到这条条件成立为止」的契约。每轮**干净**结束后，runner 向模型注入一轮自评；模型判定未达成就自动再排一轮，直到达标或触发熔断。实现见 `internal/runner/goal.go`，存储挂在 `store.Session.Goal`。

- **判定者是模型自己**，没有独立裁判——这是刻意的取舍。代价是模型可能把半成品说成完成，所以条件写得越可验证越稳（`go test ./... 全绿 且 ./build 退出码为 0` 这种，远好过「功能完成」）。熔断的作用就是让一次误判最多浪费一轮。
- **熔断**：`max_turns`（默认 20，上限 200）+ `max_hours`（默认 4h，上限 48h），两者都必须有值——一个永远达不成的目标若无上限会无限自转。触发后落 `exhausted` 并记 `last_reason`，UI 直接显示。
- **判定信号**是模型正文最后一行里的 `<!--goal:met-->` / `<!--goal:unmet ... -->`。注释形式是刻意的：DOMPurify 会把它整条剥掉（已实测），用户看不见，而服务端按纯文本就能解析。
- **marker 从广播流读，不从事件日志读**：stream 事件不落盘（见 `emit`），所以 `emit` 里顺手调 `noteGoalOutput`，在 runtime 上留一条 256 字节的滚动尾巴——**marker 会被流式 delta 切开**，只扫最后一块会漏。
- **`setIdle` 里起 `go m.advanceGoal(...)`，且只在 `lastErr == ""` 时起**：出错或被打断的那轮没有可判定的产物，自动续跑只会掩盖问题、或者跟用户自己的 stop 抢。
- **坑（踩过）**：`advanceGoal` 判断「用户是否排了消息」必须看 `len(r.queue)`，**不能用 `Busy()`**。`setIdle` 是 `work()` 在 worker 内部调的，此刻 `r.working` 仍为 true，`Busy()` 恒为真，会让整个功能静默失效——单测全绿也照样漏，只有真跑一轮才暴露。
- **终止语义**：用户按 stop（`StopGoal`）→ 落 `stopped`，因为被打断的那轮结束瞬间会再触发一次 `advanceGoal`，不主动停就会自己复活；rewind（`InvalidateGoal`）→ 直接清空，因为回滚掉的正是目标在追的那部分工作，旧目标对着回滚后的 transcript 没有意义。
- **不做的**：不在客户端做任何判定逻辑，目标状态一律回读服务端（`applySession` + `renderGoalBar`）。前端和服务端各算一套轮数必然对不上。
- UI：会话 ⋯ 菜单「自动续跑目标…」设目标，当前会话 composer 顶部一条 `.goal-bar` 显示条件/轮次/终态并提供停止。

## 后端包职责（`internal/`）

| 包 | 职责 | 关键点 |
|---|---|---|
| `config` | 配置加载、三层 env 合并、热加载 | `CLI.EffectiveEnv`；env 字段 `json:"-"` 不下发前端；空 token 自动生成；`Manager.Get` 读当前配置、`Watch` 轮询重载 |
| `store` | sessions 元数据 + 事件日志持久化 | `Session`/`Event`；`AppendEvent`/`UpdateEvent`/`ReplaceEvents`/`Upsert`；事件懒加载进内存 |
| `runner` | 引擎进程管理：队列、流解析、事件广播、停止、rewind、常驻进程 | `runTurn` 按 cli kind 分派 `runStreamJSONTurn`（claude + qoder 共用，argv 拼法由 `streamJSONDialect` 表达）/`runOpenCodeTurn`（**serve 面**：共享 `opencode serve` 进程 + SSE 事件流驱动，权限卡/提问卡在 `octurn.go` 里把 opencode 的 permission/question 事件映射到既有的卡面与 pending 注册表）/`runMcodeTurn`；`runResidentTurn` 长驻 stdin 写入；`turnText`（附件路径 mention 拼装——存储事件、stdin 载荷、`captureCheckpoint` 匹配文本三处共用，改格式必须同步）；`captureCheckpoint`（claude 专用）；`RewindFiles`；`LockForRewind`/`UnlockFromRewind`；`EnsureResident`/`TeardownResident`/`RestoreResidents`/`ApplyPeerName`；pub/sub |
| `server` | HTTP 路由（Go 1.22 method pattern）、鉴权、SSE、文件预览、附件上传 | `handleEvents` 合并双源 + 重编号（replay 侧按 engine 选 `claudehist.ConvertIn(+SubagentsIn)`（claude/qoder 传不同 Profile）、`opencodehist.FetchSession().ToEvents()` 或 `mcodehist.FetchSession().ToEvents()`）；`handleRewind`/`handleFork`/resident/图片消息按引擎 400 guard（rewind/resident 只放行 claude，fork 放行 claude+qoder，图片只拒 opencode+mcode）；`handleImport` 按 cli kind 分支（opencode 分支从 export 对齐 `ModelOverride`；mcode 分支从 transcript 对齐，且要求请求带 cwd；claude/qoder 共用 transcript 存在性校验）；`/api/opencode-sessions(+preview)`（`?cwd=` 选项目桶，空 = 跨项目枚举）、`/api/opencode-models` + `/api/opencode-model-variants`（**共用一个 `ocCatalog` memo**，stale-while-revalidate，见 section 8）、`/api/mcode-sessions(+preview)`、`/api/mcode-models`、`/api/qoder-sessions(+preview)`、`/api/qoder-models`（qoder.go）；`handleStats`（`statsMu` 串起四引擎缓存，`?fresh=1` 强制全量重扫；`opencodeStatsTargets` 派生 opencode cli 路径 + 桶候选目录，桶目录仅在 db 枚举失败时兜底，见 section 9）；`handleReadFile`（files.go）；`handleUpload`（落盘 `data/uploads/<sid>/`，消息经 `runner.turnText` 的路径 mention 引用，只接受本会话上传目录内的路径）；**落盘名由 `writeUploadExclusive` 去重，绝不覆盖同名文件**——同批剪贴板图片在浏览器里全叫 `image.png`，而 mcode 这类没有内嵌图片通道的引擎把每张图都降级成文件上传，直接 `WriteFile` 覆盖会让一轮里的 N 张图变成「最后一张 × N」；`groups.go`（`/api/groups` 全套，含 `PATCH /api/groups/{id}/members/{memberId}` 改单个成员档案，见 section 10） |
| `claudehist` | 扫描 `~/.claude/projects`（`Profile` 参数化，qoder 走同一套读 `~/.qoder-cn/projects`）、导入转换、uuid 定位/截断、claude/qoder 侧统计聚合 | `Profile{Home,Engine}` + `ClaudeProfile`/`QoderProfile`；`Convert(Subagents)/List/Preview/Move/Rename/LastUserUUID/Truncate/HasUserPrompt` 都有 `*In` 变体；`StatsCache.Refresh`（mtime+size 指纹增量）/`RefreshWithModels`（模型名修正）/`AggregateStats`；有测试 |
| `opencodeapi` | opencode 引擎实况客户端（`opencode serve` 子进程 + HTTP/SSE，不依赖 store/config） | `Start`（拉起 serve、等 `/global/health`、进程组清理）/`Close`；`CreateSession`/`Prompt`/`Messages`/`Interrupt`；`ListPermissionRequests`/`ReplyPermission`、`ListQuestionRequests`/`ReplyQuestion`/`RejectQuestion`；`StageRevert`/`CommitRevert`/`ClearRevert`；`Stream`+`Hub`（SSE 订阅与按 session 分发）；`Event.Part()`/`Next()`/`PermissionAsked()`/`QuestionAsked()`/`SessionError()`（两套事件方言的解码，有测试）。**wire 形状一律以运行中 server 的 `GET /doc` OpenAPI 为准**，不凭记忆——形状错了 POST 仍回 200 但静默不跑 |
| `opencodehist` | opencode 引擎会话读取/转换（只走 CLI 官方面，唯一例外是只读枚举的 `opencode db` SELECT）+ opencode 侧统计聚合 | `List(cli, dir)`（dir 选项目桶）/`ListAll`（只读 db 枚举全部项目，跨项目消费者：统计 + 导入列表的空 cwd 路径，失败回落 `List`）/`FetchSession`/`Session.ToEvents`/`Preview`/`AdaptPart`+`ToolTx`/`stripEchoQuotes`；`StatsCache.Refresh`（db 枚举优先 + 桶兜底、`updated` 指纹增量 + 8 路并发 export）/`ScanSession`；有测试 |
| `mcodehist` | MiniMax Code 引擎适配：stream-json 事件 → canonical 事件、`messages.jsonl` 回放、mcode 会话枚举、运行时配置里的模型/档位目录、统计聚合 | `ParseLive`/`LiveEvent.Adapt`/`ToolTx`（与 opencode 同构，running→completed 双发去重）；`Session.ToEvents`/`Preview`；`List`/`FetchSession`/`FindSessionDir`；`LoadCatalog`（读 `~/.minimax/config.yaml`）；`ScanSession`/`StatsCache`；`BareModelID`/`normalizeUsage` |
| `statsagg` | 引擎无关的统计聚合核心 | `FileAgg`（每会话聚合，字段导出）/`Recompute`（纯算术合并，确定序）/`DailyActivity`/`DailyModelTokens`/`ModelUsage.TotalTokens`/`CalculateStreaks`/`ActiveSessionGapMs`；四引擎共用，别在此加引擎分支 |

依赖方向：`store`(叶) ← `config`(yaml) ← `statsagg`(叶) ← `claudehist`(→store,statsagg)、`opencodehist`(→store,statsagg)、`opencodeapi`(→叶，只用标准库) ← `runner`(→config,store,claudehist,opencodehist,opencodeapi) ← `server`(→全部)。

## 前端结构（`web/`，vanilla JS，无框架无模块系统）

- `index.html` — app shell：侧边栏 / 主聊天视图（chat+trajectory 双 tab）/ 新建会话视图 / 文件预览面板 / 各类 modal。
- `app.js`(~3200 行) — 主逻辑。全局 `state` 对象；`api()` fetch 封装（401 跳 login）；`connectEvents()` SSE 消费（`seenSeq` 去重、`replay_end` flush、`checkpoint`/`rewind` 处理）；`renderEvent()` 分发渲染（带 `parent_tool_use_id` 的子代理消息走 `renderSubagentEvent` 聚成折叠卡片，轨迹/统计跳过）；partial streaming 用 `state.live` + `requestAnimationFrame` 节流；工具视图 `buildToolView()` 按 tool 名分发（TodoWrite/Task*/Bash/Read/Write/Edit diff）；任务面板折叠自 `historyBuf`（`taskFoldEvent`/`syncTaskPanel`，dock 在 composer 卡片顶部，回放 flush/rewind 后全量重折）；slash popup；文件预览；移动端抽屉。完成通知与未读蓝点都由 `diffRunningSessions` 的轮询 diff 驱动（活跃=running∥queued，快照存 localStorage 兼作下次启动基线；未读集合 `cc-webui-unread-done`，`openSession` 清除）。
- `trajectory.js`(~665 行) — 轨迹视图：把 `state.historyBuf` 投影成按轮(turn)分组的台账，`traj` 对象管理 `byMsgId`(assistant 去重)/`byCallId`(tool 配对)/折叠/筛选/搜索/详情面板。
- **engine gating**：`openSession` 按 `s.engine` 走 `setEngineUI`（隐藏 📡 常驻按钮）+ `fillCLISelect/ProviderSelect(sel, value, engine)` kind 过滤（opencode cli 带 `· opencode` 后缀标签）；侧栏会话菜单 fork 项按目标会话 engine 隐藏（`syncMenuForEngine`）；新建会话视图/provider/quick-create 弹窗切 cli 时 provider 归位同 kind 首个（`applyNsEngineUI`/`applyNewEngineUI`），opencode 下拒收内嵌图片（主 composer 自动降级为文件附件）。effort/推理深度下拉不按引擎一刀切——`refreshSessionEffort` 等按模型 variants 动态填充（见 section 8）。
- **导入弹窗**：cc 与 opencode 共用列表/预览 UI，`#import-tabs` 页签把 `#import-cli` 切到该 kind 首个 cli；`scanImport`/`selectImportPreview` 按所选 cli 的 kind 路由 `/api/claude-sessions` 或 `/api/opencode-sessions?cli=`（cwd 框对 claude/qoder/opencode 都是选桶条件，留空则列全部项目——opencode 的跨项目枚举见 §9）。
- **待回答提问的强提示**（AskUserQuestion 专用，见 §11）：提问卡只停在**当前会话** composer 上方的 dock 里，而 SSE 只订阅当前会话（§3），所以换个会话这提问就彻底静默、只剩 55s 超时。三层提示因此都由「谁在提问」这个全局事实驱动：
  - **数据面**：`server.sessionView.PendingAsks`（`pendingAsksOf`）与 `server.groupView.PendingAsks`（`groupPendingAsksOf`，额外带 `member_id`/`member_name`——成员影子会话不在侧栏里，跳转得靠群 id）随会话/群列表一起下发，最老的一条在前（最接近超时）。`input["questions"]` 两种形状都要认：claude/qoder 是 JSON 解出来的 `[]any`，opencode 是进程内造的 `[]map[string]any`（`ask_test.go` 守住这条）。**列表空时也返回 `[]` 不是 null**，前端无条件索引。
  - **触发面**：三条源按提问 id 去重（`raiseAskToast` 幂等）——SSE `permission_request`（当前会话/群转发）、`fetchPendingPermissions`（刷新后补卡）、`diffPendingAsks()`（`loadSessions` 5s 轮询 diff，**唯一能看见别的会话在提问的通道**）。`resolvePermissionCard` 立即撤对应浮层，跨会话时由下一轮轮询兜。
  - **呈现面**：`#toast-layer` 顶部居中横幅（iPad 来通知形态，**不是右下角小卡片**——见 §11 踩过的坑），整条可点 = `gotoAsk` 跳到那张卡（跨会话开会话、群卡开群视图，定位靠 `focusAskCard` 轮询等异步补卡出现）。侧栏另有：整行底色脉动 `.session-item.has-ask` + `.dot.ask`（压过 running）+ 行内 `待回答` 标签、工作区栏 `#ws-activity` 的「待回答 N」徽标 + `renderWsPopPending` 列表。**别只做浮层**——人往往在别的标签页/会话，侧栏那一条才是回来第一眼看到的。
- **lucide 图标是白名单制**：`renderIcons` 只渲染 `wanted` 数组里的图标，白名单外的 `<i data-lucide="x">` **不会被替换成 svg**——元素还在但里面是空的，表现为「按钮/标签只剩一圈 padding」。加图标必须同步加白名单；拿不准就直接用原始内联 SVG 常量（`CODE_COPY_ICON` / `HTML_FS_ICON` 那样），不依赖 `renderIcons` 被调用。`scripts` 里没有这个检查，测试得自己钉。
- `style.css` — 全部样式，`[data-theme]` 深浅主题；`.goal-bar` 是 Session Goals 的 composer 顶部条（§12）。
- **HTML 沙箱渲染的封顶与全屏**（` ```html ` 围栏和 `mcp__*__render_html` 工具块共用）：
  - iframe 高度 = 页面自报的真实高度，但外层 `.html-frame-box` 封顶 `var(--html-frame-max, 640px)`。不封顶的话一个 3000px 的页面会把整个聊天撑爆，滚几千像素才能越过这个块。640px 是按常见页面实际高度定的——仪表盘、换算器那类 300–600px 的页面永远碰不到上限，所以对日常场景是零变化。
  - 框内滚动用 `overflow-y: auto` **而不是 `contain`**：`contain` 会让手指在框里滚到底之后也带不动聊天，那种"卡住"比内滚本身更糟。代价是手机触摸滚动会被 iframe 捕获，全屏按钮就是解药。
  - 全屏是**搬节点**不是重建 iframe：重建会把页面里的脚本状态、定时器、滚动位置全清掉。`enterHtmlFullscreen` 把 `.html-frame-box` 整体搬进 fixed 遮罩，退出原路搬回。
  - **滚动容器是 `.html-fs-body`，不是里面的 `.html-frame-box`**。后者是孙节点，拿不到高度；滚动写在内层会撑破父容器、外层又没 overflow，整块就彻底滚不动。而且 flex 子项必须配 `min-height: 0`，少了它 `flex: 1` 形同虚设。
- `vendor/` — marked / DOMPurify / highlight.js / lucide / 主题 css，均已 vendored。
- `go:embed all:web` → `fs.Sub(webFS, "web")` → `http.FileServerFS`。

## 运行时数据布局（`data_dir`，默认 `./data`，gitignore）

- `db.json` — 全部 sessions 元数据（按 UpdatedAt 排序，temp+rename 原子写）。
- `events/<sid>.jsonl` — 每会话一文件，每行一 JSON 事件。append 追加；`UpdateEvent`/`ReplaceEvents` 全文件重写。
- `settings/<cli>__<provider>__<model>.json` — 物化的 `--settings` 文件（0600），每轮重写。
- `backups/<sid>.<unix_ms>.jsonl` — rewind 截断前的 Claude transcript 备份。
- `notes.json` — 全局待办备忘（与 session 无关，跨重启持久化；前端权威，全量 PUT，0600 temp+rename 原子写，`notesMu` 单独加锁）。

Claude 自己的 transcript 仍在 `~/.claude/projects/<slug>/<sid>.jsonl`，cc-webui 现场读、不复制。

## 约定

- **在本项目的会话里交付可视化页面**：当用户要图表、仪表盘、交互页、幻灯片式排版这类「一页 HTML」的东西时，**直接输出一整段 ` ```html ` 围栏代码块**（完整 `<!doctype html>` 文档），不要写 `.html` 文件到磁盘让用户自己去浏览器开。`md()` 会把 ` ```html ` 围栏升级成沙箱渲染容器：默认显示代码，点「渲染」才挂一个 `sandbox="allow-scripts"` 的 srcdoc iframe（不给 `allow-same-origin`，所以页面碰不到 app 的 localStorage / DOM / 带 cookie 的同源接口）。
  - 要求：**完全自包含**——不许外部 CSS/JS/字体/网络请求。图表用内联 SVG 或页面自己的 script。
  - iframe 里注入了宿主主题的 CSS 变量，**优先用它们而不是硬编码颜色**，页面就能跟随 app 的深浅色：`--background` `--surface` `--border` `--foreground` `--muted` `--accent` `--code-bg`。同时写上兜底值 `var(--background, #fff)`，没注入时也不至于裸奔。
  - 高度自动跟随内容，别在页面里做内层滚动（聊天滚动会被劫持）；宽表格自己套一个横向滚动容器。
  - 轨迹视图是台账，不渲染 frame（`md(x, {artifacts:false})`），只看代码——这是有意的，不是 bug。
  - 已知现状：这条约定目前只有部分模型遵守（实测 `opencode-go/deepseek-v4.1-flash` 会照做；`mcode`、`claude 引擎上的 minimax` 会改成写文件）。另外前端也支持一个 `mcp__*__render_html` 工具形态（`buildToolView` 分支），但**至今没有任何模型真的调用过它**，别默认它可用。
- **并行开发用 worktree**：多需求/bug 并行时每条开一个 worktree，别在 master 上堆未提交改动互相打架。`git worktree add ../cc-webui-<topic> -b <topic-branch>`；每个 worktree 的 `data/`、构建产物 `cc-webui` 都 gitignore、各自隔离、互不污染。`config.yaml` 也 gitignore——新 worktree 要么 `cp config.example.yaml config.yaml` 重新填 token，要么软链主 worktree 的 config——**只为共享 token + clis/providers/models 静态配置**（这些本来就想各 worktree 一致）；软链后 `listen` 也随之共享成 8321，**端口别改 config（会动 master），靠启动时 `CC_WEBUI_LISTEN` env 覆盖**（见下条）。`data_dir: "./data"` 经 `filepath.Abs` 按**进程 CWD**解析（非 config 文件位置），各 worktree 自带独立 `<worktree>/data`，不会写穿 master。完事回主 worktree 合 master，再 `git worktree remove ../cc-webui-<topic>`。

- **实测别杀活进程**：跑 harness / 手测时**不要 kill 正在运行的 cc-webui**（用户在 screen 里 dogfood 的活实例，默认 `0.0.0.0:8321`）。改完 `go build` 出新二进制，用**别的端口 + 独立 data_dir** 起测试进程：`CC_WEBUI_DATA_DIR=./data-test CC_WEBUI_LISTEN=127.0.0.1:8322 ./cc-webui`——8321 留给活实例，127.0.0.1 不外暴测试实例。`go test ./...` 只跑单测、不起服务、不冲突。
- **注释语言**：Go 后端以英文 doc 注释为主；`server/files.go` 与用户可见错误/文案是中文（如 `"文件回滚失败："`、`"文件已回滚"`）；前端 JS 注释与全部 UI 字符串为中文。文档（README/rewind-rd/TODO）全中文。**改代码时匹配周围注释的语言。**
- **命名**：标准 Go——PascalCase 导出 / camelCase 包内；文件名小写单词（`store.go`、`files.go`）；包名同目录名。前端用 `$()` 作 `querySelector` 简写、`state`/`traj` 全局对象。
- **错误处理**：`fmt.Errorf("%w")` 包装；后台非致命错误 `log.Printf`；仅 `main.go` 启动失败用 `log.Fatalf`；HTTP handler 用 `writeErr(w, code, msg)` 返回 `{"error":...}`。
- **文件权限**：数据文件 0600、敏感目录 0700、events/data 根 0755。
- **测试**：有单测的包 `claudehist`（含 stats）、`opencodehist`（含 stats）、`server`、`runner`、`config`；`statsagg`、`store` 无测试。改 rewind 相关的 `LastUserUUID`/`TruncateBeforeUUID`/`HasUserPrompt` 务必跑 `go test ./internal/claudehist/`；改统计口径（section 9）务必跑 `go test ./internal/claudehist/ ./internal/opencodehist/`——两侧计数规则要一致。pre-commit 守卫（`scripts/hooks/pre-commit`，一次性激活 `git config core.hooksPath "$(pwd)/scripts/hooks"`）：staged 含 `.go` 自动跑 `go vet ./...`，含 `internal/claudehist/` 时加跑其测试——commit 被挡多半是这俩没过。

## 安全边界（务必遵守）

- `gateway_token` 鉴权覆盖所有 `/api/*` 与 SSE；`subtle.ConstantTimeCompare`。
- 文件预览（`/api/files`）**严格限会话 cwd 内**，`EvalSymlinks` 解析真实路径防穿越，只读 + 2 MiB 上限 + 拒二进制。**别放松这个白名单。**
- `settings/` 文件含 token，0600 且 gitignore——别打印到日志、别下发前端（env 字段 `json:"-"`）。
