# AGENTS.md

给在本仓库工作的工程师 / AI agent 的导览。先读这一篇，再动手。

> 用户向文档见 `README.md`；rewind 设计与实施记录见 `rewind-rd.md`；待办见 `TODO.md`。
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

## 架构核心（不读代码会踩坑的点）

### 1. 双源事件模型

事件有两个来源，**stream 事件不持久化**：

- **持久化事件**（`data/events/<sid>.jsonl`）：`user`、`status`、`rewind`。由 `store.AppendEvent` 落盘。
- **stream 事件**：**不落盘**。每次 SSE 连接时，从 Claude 自己的 `~/.claude/projects/<slug>/<sid>.jsonl` 现场解析（`claudehist.Convert`），与持久化事件合并去重（`mergeStoredAndClaude` + `eventFingerprint`）。

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

### 6. 并发模型

- `store`：两把独立锁——`mu` 保护 sessions map + db.json；`eventsMu` 保护 events map（互不阻塞）。db.json 经 temp+rename 原子写。
- `runner`：`Manager.mu` 保护 `procs` map（按 session 懒建 `sessionRuntime`）；每个 `sessionRuntime` 自带 `mu` 保护 queue/working/cancel/subs/statusCh。worker goroutine `work()` 循环排空 queue。
- 停止：`Setpgid: true` 起进程组，`Stop` 用 `syscall.Kill(-pid, SIGKILL)` 杀整棵树。
- SSE 订阅：256 缓冲 channel，非阻塞 send（满了丢弃）。

## 后端包职责（`internal/`）

| 包 | 职责 | 关键点 |
|---|---|---|
| `config` | 配置加载、三层 env 合并 | `CLI.EffectiveEnv`；env 字段 `json:"-"` 不下发前端；空 token 自动生成 |
| `store` | sessions 元数据 + 事件日志持久化 | `Session`/`Event`；`AppendEvent`/`UpdateEvent`/`ReplaceEvents`/`Upsert`；事件懒加载进内存 |
| `runner` | claude 进程管理：队列、流解析、事件广播、停止、rewind | `runTurn` spawn 进程；`captureCheckpoint`；`RewindFiles`；`LockForRewind`/`UnlockFromRewind`；pub/sub |
| `server` | HTTP 路由（Go 1.22 method pattern）、鉴权、SSE、文件预览 | `handleEvents` 合并双源 + 重编号；`handleRewind`；`handleFork`/`handleImport`；`handleReadFile`（files.go） |
| `claudehist` | 扫描 `~/.claude/projects`、导入转换、uuid 定位/截断 | `Convert`/`LastUserUUID`/`TruncateBeforeUUID`/`HasUserPrompt`；唯一有测试的包 |

依赖方向：`store`(叶) ← `config`(yaml) ← `claudehist`(→store) ← `runner`(→config,store,claudehist) ← `server`(→全部)。

## 前端结构（`web/`，vanilla JS，无框架无模块系统）

- `index.html` — app shell：侧边栏 / 主聊天视图（chat+trajectory 双 tab）/ 新建会话视图 / 文件预览面板 / 各类 modal。
- `app.js`(~2600 行) — 主逻辑。全局 `state` 对象；`api()` fetch 封装（401 跳 login）；`connectEvents()` SSE 消费（`seenSeq` 去重、`replay_end` flush、`checkpoint`/`rewind` 处理）；`renderEvent()` 分发渲染；partial streaming 用 `state.live` + `requestAnimationFrame` 节流；工具视图 `buildToolView()` 按 tool 名分发（TodoWrite/Bash/Read/Write/Edit diff）；slash popup；文件预览；移动端抽屉。
- `trajectory.js`(~665 行) — 轨迹视图：把 `state.historyBuf` 投影成按轮(turn)分组的台账，`traj` 对象管理 `byMsgId`(assistant 去重)/`byCallId`(tool 配对)/折叠/筛选/搜索/详情面板。
- `style.css` — 全部样式，`[data-theme]` 深浅主题。
- `vendor/` — marked / DOMPurify / highlight.js / lucide / 主题 css，均已 vendored。
- `go:embed all:web` → `fs.Sub(webFS, "web")` → `http.FileServerFS`。

## 运行时数据布局（`data_dir`，默认 `./data`，gitignore）

- `db.json` — 全部 sessions 元数据（按 UpdatedAt 排序，temp+rename 原子写）。
- `events/<sid>.jsonl` — 每会话一文件，每行一 JSON 事件。append 追加；`UpdateEvent`/`ReplaceEvents` 全文件重写。
- `settings/<cli>__<provider>__<model>.json` — 物化的 `--settings` 文件（0600），每轮重写。
- `backups/<sid>.<unix_ms>.jsonl` — rewind 截断前的 Claude transcript 备份。

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
