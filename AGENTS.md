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
go test ./...                          # 仅 internal/claudehist 有测试
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

Claude 的 `~/.claude/settings.json` 的 env 段会覆盖进程继承的 env，所以只往进程注入 `ANTHROPIC_BASE_URL` 等不生效。cc-webui 的做法：把生效 env 物化成 `data/settings/<cli>__<provider>__<model>.json`（0600），以 `--settings <绝对路径>` 传给 claude（CLI 优先级最高），同时注入进程 env 供非 Claude 兼容 CLI 用。

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
- **settings 注入**：`buildEnvMap` 物化的 `--settings` 文件**无条件**写 `crossSessionInbound:"accept"`——单用户控制台，bypass-permission session 默认 `hold`，headless 下无人能答，消息会积压卡死。repo/managed settings 可收紧但本进程不接受 `hold`。
- **入站气泡**：`renderEvent` 在 tool_result 分支前判 `d.type === 'user' && d.origin?.kind === 'peer'` → `msg-peer` 气泡（紫色描边，左对齐），用 `origin.body`（已解码）走 `md()` 渲染，`origin.name` 经 `escapeHtml` 注入 role-tag。replay（`claudehist.Convert`）和实时流共用同一渲染路径。
- **前端**：header-actions 加 `📡` 按钮 + popover（toggle 开关 + 地址名输入 + 状态行 + hint）；侧栏 `.resident-tag`；toggle/name 改完走 `PATCH /api/sessions/{id}` 同步到后端，server 顺序：peer_name 先 `claudehist.RenameSession` 写 transcript，再 Upsert store，最后 `EnsureResident`/`ApplyPeerName`。删除会话先 `TeardownResident` 再 `store.Delete`。

### 7. 并发模型

- `store`：两把独立锁——`mu` 保护 sessions map + db.json；`eventsMu` 保护 events map（互不阻塞）。db.json 经 temp+rename 原子写。
- `runner`：`Manager.mu` 保护 `procs` map（按 session 懒建 `sessionRuntime`）；每个 `sessionRuntime` 自带 `mu` 保护 queue/working/cancel/subs/statusCh。worker goroutine `work()` 循环排空 queue。
- 停止：`Setpgid: true` 起进程组，`Stop` 用 `syscall.Kill(-pid, SIGKILL)` 杀整棵树。常驻进程同样走进程组；`TeardownResident` 先 stdin close 软退，5s 不退再发 SIGKILL。
- SSE 订阅：256 缓冲 channel，非阻塞 send（满了丢弃）。

### 8. 双引擎：claude / opencode

`config.CLI` 与 `config.Provider` 各带 `kind: claude|opencode`（空自动归一化为 claude）。会话绑定单一引擎（**不跨引擎切换**）：`session.engine` = 其 cli 的 kind，create/patch/import 都校验 cli 与 provider kind 一致，不一致 400 `engine mismatch`。前端每处 cli/provider 下拉都按 kind 过滤（`fillCLISelect(sel, value, kind)` 等 helpers，见 `web/app.js`）。

- **claude 引擎**（原架构）：transcript 在 `~/.claude/projects/`，`-p --output-format stream-json`，`--settings` 注入 env，checkpoint/rewind、resident/peer、fork 均可用。会话 `ClaudeSessionID` = claude session id。
- **opencode 引擎**（`internal/opencodehist`，有测试）：数据在 opencode 自己的 sqlite store——**绝不直读**，只走 CLI 官方面：`session list --format json`（列表）、`export <sid>`（单会话全量 JSON）、`run --format json -s <sid> [-m provider/model] [--auto]`（实况）。`Session.ClaudeSessionID` 字段复用来存 opencode 的 `ses_...`。
- **canonical 事件形状**：两条路径（claude `Convert` / opencode `AdaptPart`+`ToEvents`）产出同形事件——assistant text 每 part 一条 `{type:"assistant",message:{role,id,content:[{type:"text",id:partId,...}]}}`（frontend trajectory 按 message.id 合并、fingerprint 靠 text block 的 part id 区分），tool 是 `tool_use` assistant 块 + 完成/报错时的 `tool_result` user 块（`ToolTx` 按 callID 去重 running→completed 双发）。reasoning/patch/step-* 一律跳过——`run --format json` 实况不吐 reasoning，回放带 reasoning 会两态不一致。
- **opencode 实况不回显用户 prompt**：cc-webui 自己落 stored user echo；但 opencode export 的用户文本带一层**回显双引号**（`"prompt"`），replay 前必须 `stripEchoQuotes` 剥掉，否则与 stored echo 文本不一致，`mergeStoredAndClaude` 的 count 去重失效、刷新后用户气泡重复。
- **`session list` 按项目分桶**：只返回进程 cwd 所属桶（git 根 = 该仓库桶，非 git 目录 = `global` 桶）；`export`/`run -s` 则全量可按 id。API `?cwd=` 选桶（空 = 服务器自身 cwd）；`run` 永远 `cmd.Dir = sess.Cwd` 回到会话原目录，恢复不跨桶。
- **opencode 禁用的协议面**（服务端 400 + 前端隐藏控件）：resident/peer messaging、fork、rewind/checkpoint、内嵌图片 base64（服务端 400）；effort 下拉隐藏（`--auto` 由 permission_mode 非空且非 default 决定）。
- **图片/文件 = `-f` 官方附件**：前端把图片（主 composer 与 ns 视图都一样）降级为文件上传，runner 对每个 `msg.Files` 追加 `-f <path>`——视觉模型直接可见图片，文本模型可工具读取。**消息位置参数必须在 `-f` 之前**（`-f` 是数组 flag，`-f <path> <msg>` 会把消息吞进文件数组）。opencode 的 stored echo 只存 `msg.Text`（不能是 turnText 的 mention 版）：export 回显 = 原 prompt 原文（带引号），文件内容不进 echo 文本，文本一致去重才成立。
- **export 的 user 消息在带附件时会多出三个 part**：工具前导文本（`Called the Read tool with the following input: ...`）、`<path>/<type>/<content>` 文件展开文本、`type:"file"` part——`userPromptText` 过滤后只剩 prompt echo，否则回放气泡会带文件转储且 echo 失配重复。
- **run 参数**：`-m <providerID>/<modelID>`（opencode provider 名必须等于其 providerID 才能组模型串；模型名含 `/` 原样透传）；新会话无 sid 时 `--title` 传 `sess.Name`，第一行 stdout 事件就带 sessionID，捕获后回填。

## 后端包职责（`internal/`）

| 包 | 职责 | 关键点 |
|---|---|---|
| `config` | 配置加载、三层 env 合并、热加载 | `CLI.EffectiveEnv`；env 字段 `json:"-"` 不下发前端；空 token 自动生成；`Manager.Get` 读当前配置、`Watch` 轮询重载 |
| `store` | sessions 元数据 + 事件日志持久化 | `Session`/`Event`；`AppendEvent`/`UpdateEvent`/`ReplaceEvents`/`Upsert`；事件懒加载进内存 |
| `runner` | 引擎进程管理：队列、流解析、事件广播、停止、rewind、常驻进程 | `runTurn` 按 `cli.IsOpenCode()` 分派 `runClaudeTurn`/`runOpenCodeTurn`（后者的 stdout 逐行喂 `opencodehist.AdaptPart`）；`runResidentTurn` 长驻 stdin 写入；`turnText`（附件路径 mention 拼装——存储事件、stdin 载荷、`captureCheckpoint` 匹配文本三处共用，改格式必须同步）；`captureCheckpoint`（claude 专用）；`RewindFiles`；`LockForRewind`/`UnlockFromRewind`；`EnsureResident`/`TeardownResident`/`RestoreResidents`/`ApplyPeerName`；pub/sub |
| `server` | HTTP 路由（Go 1.22 method pattern）、鉴权、SSE、文件预览、附件上传 | `handleEvents` 合并双源 + 重编号（replay 侧按 engine 选 `claudehist.Convert(+Subagents)` 或 `opencodehist.FetchSession().ToEvents()`）；`handleRewind`/`handleFork`/resident/图片消息对 opencode 会话 400 guard；`handleImport` 按 cli kind 分支（opencode 分支从 export 对齐 `ModelOverride`）；`/api/opencode-sessions(+preview)`（`?cwd=` 选项目桶）；`handleReadFile`（files.go）；`handleUpload`（落盘 `data/uploads/<sid>/`，消息经 `runner.turnText` 的路径 mention 引用，只接受本会话上传目录内的路径） |
| `claudehist` | 扫描 `~/.claude/projects`、导入转换、uuid 定位/截断 | `Convert`/`ConvertSubagents`/`LastUserUUID`/`TruncateBeforeUUID`/`HasUserPrompt`；有测试 |
| `opencodehist` | opencode 引擎会话读取/转换（只走 CLI，不碰 sqlite） | `List(cli, dir)`（dir 选项目桶）/`FetchSession`/`Session.ToEvents`/`Preview`/`AdaptPart`+`ToolTx`/`stripEchoQuotes`；有测试 |

依赖方向：`store`(叶) ← `config`(yaml) ← `claudehist`(→store)、`opencodehist`(→store) ← `runner`(→config,store,claudehist,opencodehist) ← `server`(→全部)。

## 前端结构（`web/`，vanilla JS，无框架无模块系统）

- `index.html` — app shell：侧边栏 / 主聊天视图（chat+trajectory 双 tab）/ 新建会话视图 / 文件预览面板 / 各类 modal。
- `app.js`(~3200 行) — 主逻辑。全局 `state` 对象；`api()` fetch 封装（401 跳 login）；`connectEvents()` SSE 消费（`seenSeq` 去重、`replay_end` flush、`checkpoint`/`rewind` 处理）；`renderEvent()` 分发渲染（带 `parent_tool_use_id` 的子代理消息走 `renderSubagentEvent` 聚成折叠卡片，轨迹/统计跳过）；partial streaming 用 `state.live` + `requestAnimationFrame` 节流；工具视图 `buildToolView()` 按 tool 名分发（TodoWrite/Task*/Bash/Read/Write/Edit diff）；任务面板折叠自 `historyBuf`（`taskFoldEvent`/`syncTaskPanel`，dock 在 composer 卡片顶部，回放 flush/rewind 后全量重折）；slash popup；文件预览；移动端抽屉。完成通知与未读蓝点都由 `diffRunningSessions` 的轮询 diff 驱动（活跃=running∥queued，快照存 localStorage 兼作下次启动基线；未读集合 `cc-webui-unread-done`，`openSession` 清除）。
- `trajectory.js`(~665 行) — 轨迹视图：把 `state.historyBuf` 投影成按轮(turn)分组的台账，`traj` 对象管理 `byMsgId`(assistant 去重)/`byCallId`(tool 配对)/折叠/筛选/搜索/详情面板。
- **engine gating**：`openSession` 按 `s.engine` 走 `setEngineUI`（隐藏 📡 常驻按钮、推理等级、sheet 的 effort label）+ `fillCLISelect/ProviderSelect(sel, value, engine)` kind 过滤（opencode cli 带 `· opencode` 后缀标签）；侧栏会话菜单 fork 项按目标会话 engine 隐藏（`syncMenuForEngine`）；新建会话视图/provider/quick-create 弹窗切 cli 时 provider 归位同 kind 首个并隐藏 effort（`applyNsEngineUI`/`applyNewEngineUI`），opencode 下拒收内嵌图片（主 composer 自动降级为文件附件）。
- **导入弹窗**：cc 与 opencode 共用列表/预览 UI，`#import-tabs` 页签把 `#import-cli` 切到该 kind 首个 cli；`scanImport`/`selectImportPreview` 按所选 cli 的 kind 路由 `/api/claude-sessions` 或 `/api/opencode-sessions?cli=`（列表默认显示服务器 cwd 的项目桶）。
- `style.css` — 全部样式，`[data-theme]` 深浅主题。
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

- **注释语言**：Go 后端以英文 doc 注释为主；`server/files.go` 与用户可见错误/文案是中文（如 `"文件回滚失败："`、`"文件已回滚"`）；前端 JS 注释与全部 UI 字符串为中文。文档（README/rewind-rd/TODO）全中文。**改代码时匹配周围注释的语言。**
- **命名**：标准 Go——PascalCase 导出 / camelCase 包内；文件名小写单词（`store.go`、`files.go`）；包名同目录名。前端用 `$()` 作 `querySelector` 简写、`state`/`traj` 全局对象。
- **错误处理**：`fmt.Errorf("%w")` 包装；后台非致命错误 `log.Printf`；仅 `main.go` 启动失败用 `log.Fatalf`；HTTP handler 用 `writeErr(w, code, msg)` 返回 `{"error":...}`。
- **文件权限**：数据文件 0600、敏感目录 0700、events/data 根 0755。
- **测试**：只有 `claudehist` 有单测。改 rewind 相关的 `LastUserUUID`/`TruncateBeforeUUID`/`HasUserPrompt` 务必跑 `go test ./internal/claudehist/`。其它包无测试——加测试是好事但别假装覆盖了。

## 安全边界（务必遵守）

- `gateway_token` 鉴权覆盖所有 `/api/*` 与 SSE；`subtle.ConstantTimeCompare`。
- 文件预览（`/api/files`）**严格限会话 cwd 内**，`EvalSymlinks` 解析真实路径防穿越，只读 + 2 MiB 上限 + 拒二进制。**别放松这个白名单。**
- `settings/` 文件含 token，0600 且 gitignore——别打印到日志、别下发前端（env 字段 `json:"-"`）。
