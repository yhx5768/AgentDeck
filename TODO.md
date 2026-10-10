# cc-webui 后续功能 TODO

> 盘点基于：Claude Agent SDK 0.2.140（Python 源码） + Claude Code CLI 2.1.226。
> 标 [V] 的表示本机 CLI 已确认接受（实测 `claude -p <flag>` 不报 `unknown option`），无论是否出现在 `--help` 里。**注意**：`--help` 会**漏掉**几个真实支持的 flag（实测验证：`--max-turns` `--permission-prompt-tool` `--max-thinking-tokens` `--resume-session-at` `--task-budget` `--thinking-display` `--session-id`），原因是这些是 SDK/print 模式专属。所以"需要验证"清单会随实测收缩。

---

## ✅ 已完成

- [x] **流式增量渲染**（`--include-partial-messages`）—— 打字机式逐 token 输出，含 thinking/text delta 增量渲染、与完整 assistant 事件去重、中断兜底固化。
- [x] **思考努力度**（`--effort`）—— profile 行 model 右侧推理等级下拉（继承/low/medium/high/xhigh/max），sheet/新建弹窗/ns 内联同步，后端校验+传递 flag。
- [x] **Slash command 自动补全**（`init.slash_commands`）—— 输入 `/` 触发 popup，按文本过滤，↑↓+Tab/Enter 选中，选中命令蓝色高亮（mirror overlay 技法），slash_commands 从 init 事件捕获并持久化到 session。

---

## T1 — 高价值低成本（单 flag 或 UI 小改）

### 1.1 单 flag 加（仅后端 + config + 文档，无前端改动）

| 项 | CLI flag | SDK 选项 | 价值 | 备注 |
|---|---|---|---|---|
| 防跑飞：会话级预算 | `--max-budget-usd <n>` [V] | `max_budget_usd` | 远程挂着的会话超支保护 | session 表加一列，UI 显示预算/已用 |
| 思考努力度 | `--effort <low\|medium\|high\|xhigh\|max>` [V] | `effort` | 用户按需控制 | 加进 session 配置 |
| 自动 fallback 模型 | `--fallback-model <m,...>` [V] | `fallback_model` | 主模型过载自动切换 | 同上 |
| per-session 系统提示 | `--append-system-prompt <text>` [V] | `system_prompt` (append) | 「全程中文」「先给 plan 再写」 | composer 上方加个小输入框 |
| 多工作目录 | `--add-dir <dirs...>` [V] | `add_dirs` | 跨目录会话免开多个 | session 配置里改 cwd 的同时加附加目录 |
| 自动压缩窗口 | `--autocompact <auto\|N>` [V] | — | 长会话更稳 | 简单加到模型配置 |
| 限制 turn 数 | `--max-turns <n>` [V] | `max_turns` | 防止失控 | session 表加一列 |
| 任务级 token 预算 | `--task-budget <tokens>` [V] | `task_budget` | 细分预算 | 单 turn token 上限 |
| Thinking token 上限 | `--max-thinking-tokens <n>` [V] | `max_thinking_tokens` | 控制 thinking 预算 |  |
| thinking 显示控制 | `--thinking-display <auto\|always\|never>` [V] | `thinking` | 前端可禁用 thinking 折叠块 |  |
| 自定义 session ID | `--session-id <uuid>` [V] | `session_id` | 与导入会话 ID 对齐 | 后端默认随机，可用此固定 |

### 1.2 UI 小改（前后端联动）

- [ ] **会话预算/已用实时展示**：`--max-budget-usd` 配合 `result.total_cost_usd` 累加，session 头部显示「已用 $0.42 / $2.00」。
- [x] **effort 选择器**：session 配置文件加 effort 下拉。
- [ ] **fallback 模型配置**：model 选择器旁加 fallback 输入框。
- [ ] **per-session 工具策略**：`--tools ""` 禁用所有 / `--tools "Read,Grep,Glob"` 白名单 / `--disallowed-tools "Bash(rm *)"` 黑名单。一键造「只读审查」「禁 rm」会话（[permissions 文档语义](https://code.claude.com/docs/en/agent-sdk/permissions)）。

---

## T2 — 中等价值中等成本（需前后端配合设计）

### 2.1 File checkpointing + 回滚 UI

> 用户原话：「checkpointing 需要结合前端再设计下」。最优先的 T2。

- 后端：`runner.go` 注入 `CLAUDE_CODE_ENABLE_SDK_FILE_CHECKPOINTING=true`，加 `--replay-user-messages`（需要 `--input-format stream-json`，统一输入路径），流里捕获 `type=user && isReplay` 的 `uuid`。
- 存储：每个 user turn 的 checkpoint UUID 入库（轻量：按 turn seq 索引）。
- 新增 API：`POST /api/sessions/{id}/rewind` body=`{checkpoint_uuid}`，spawn `claude -p --resume <sid> --rewind-files <uuid>`（空 stdin）执行回滚。
- 前端设计点：
  - 每个 user 消息气泡旁加「↶ 回滚到此」按钮（hover 时显示）。
  - 回滚成功后刷新消息列表（jsonl 不变，回滚只动文件）。
  - UI 上明确告知「回滚只回退文件改动，不影响后续对话上下文」。
- 风险/局限（已在 SDK 文档里写明）：只跟踪 Write/Edit/NotebookEdit；Bash 改的文件不跟踪；subagent 改动不跟踪；不回滚目录创建/删除。
- 预计工作量：后端 1/2 天，前端 1/2 天。

### 2.2 Subagent 流式渲染（`--forward-subagent-text`）

- 加 `--forward-subagent-text` [V] flag，让 Task 工具调用生成的 subagent 文本/思考也作为 `assistant/user` 事件（带 `parent_tool_use_id`）流出，而不是藏在 Task 工具卡片里。
- 前端：识别 `parent_tool_use_id`，把对应事件**嵌套**显示在 Task 卡片内（缩进的子对话视图）。
- 价值：subagent（Explore / Explore / general-purpose）是 claude 重要的复杂任务手段，目前 cc-webui 看不到它们的过程。
- 预计工作量：后端 1h（加 flag），前端 1 天（嵌套 UI）。

### 2.3 下一条 prompt 建议（`--prompt-suggestions`）

- 加 `--prompt-suggestions true` [V]，turn 结束后 CLI 推一条 `prompt_suggestion` 类型事件。
- 前端：当前 turn 结束后在用户输入框上方显示「💡 建议：…」（点击填入输入框）。
- 预计工作量：后端 1h，前端 几小时。

### 2.4 移动审批（Permission prompts）✅ 已完成（2026-10-09）

> 远程 Web 场景的杀手特性。已落地：非 bypass 的 claude/qoder 会话经 `--settings`
> 的 `PermissionRequest` hook（`cc-webui -perm-helper`）走 Web 审批，不再全放行。

- 落地为**方案 A 变体**：`--settings` 里 `hooks.PermissionRequest` 指向本二进制
  的 `-perm-helper`（env 传 session/addr/token），helper POST
  `/api/sessions/{id}/permissions/request` 并阻塞等决策 → SSE `permission_request`
  通知前端 → 审批卡 → `POST .../permissions/{pid}` → helper 输出 hook JSON 退出。
  注：`--permission-prompt-tool` 经核实是 MCP tool 名（需跑 MCP server + 30s 启动
  等待），不是 shell 命令，遂未采用；TODO 原方案 B 描述有误。
- 三档：允许本次 / 本次会话都允许（同名工具 allowance，后续免问，前端显示
  "已按规则允许"）/ 拒绝（可附理由回传模型）。hook 上限 60s，helper 等 55s，
  超时自动拒绝（fail-closed，故障也拒绝不放行）。
- 多 CLI 适配：仅 claude/qoder 注入 hook（`permissionHookWanted`）；opencode
 （`--auto` 自动放行，无 headless 审批面）、mcode（smart/full，ask 需 TUI）不注
  入，前端权限下拉 title 已注明。`auto`/`dontAsk` 选项已加入三个权限下拉。
- AskUserQuestion 答案回填 ✅（2026-10-09）：题目按单选/多选 + 其他自定义渲染，
  答案以问题原文为 key 经 `updatedInput.answers` 回传（多选逗号连接），跳过走
  deny；后端拒绝给 AskUserQuestion 建 session allowance（ crafted 调用也拿不到
  免问）。缺省行为：不填答案提交按钮保持禁用，超时/跳过模型收到未回答。
- 待补（MVP 明确未做）：`updatedPermissions` 的 setMode 规则下发；allowance 跨重
  启持久化（现仅内存，删会话即清）。

### 2.5 Hook 事件可视化（`--include-hook-events`）

- 加 `--include-hook-events` [V]，CLI 在 stream 里发 hook lifecycle 事件。
- 价值：用户能看到「PreToolUse hook 拦截了 X」「SessionStart hook 加载了 .claude/settings.json」等，对调试自定义 hook 配置有用。
- 长尾功能，可与 2.2 一起做。

### 2.6 Slash command 按钮（来自 `init.slash_commands`）

- `system/init` 事件带 `slash_commands` 列表（用户在 .claude/skills/、bundled skills、built-in commands 等暴露出来的命令），本机实测：`baidu-search`、`icode`、`-calendar`、`-im`、`ku-doc-manage`、`deep-research`、`code-review`、`/init`、`/compact`、`/clear` 等几十个。
- 前端：composer 顶部展示一张快捷按钮条（按使用频次排序或分组），点击填入 `/<name> [args]` 发送。
- 价值：把 claude 已经支持的技能/命令暴露成 web 按钮，减少记忆负担。
- 预计工作量：前端半天。`init.slash_commands` 已经在流里了，只需要解析 + 渲染。

### 2.7 `/compact` 按钮 — 手动压缩上下文

- 用户点击 → cc-webui spawn `claude -p --resume <sid> --output-format stream-json "/compact"` 单独跑一次。
- CLI 会发 `system/subtype=compact_boundary` 事件，含 `compact_metadata.pre_tokens` / `trigger`。
- 前端：在 result-meta 行旁加「📦 压缩前 X tokens → Y」徽章。
- 注意：cc-webui 单 turn 模型下，发 `/compact` 后需要再发一次「继续」才有意义 — 提示用户也行。
- 预计工作量：后端 1h，前端 2h。

### 2.8 Resume from message（`--resume-session-at <uuid>`）

> **新发现**（[V 已验证]）：CLI 提供 `--resume-session-at <message-id>`，与 `--resume` 配合可从历史某个点重新开始，比 checkpoint 更强。

- 与 checkpointing 的区别：
  - **checkpoint** 回滚**文件系统改动**，对话上下文不变；
  - **`--resume-session-at`** 回滚**对话上下文**到某个历史点，文件不动。
- 用法：`claude -p --resume <sid> --resume-session-at <msg-uuid>` 下个 turn 就从那点开始（历史视作截止到那个点）。
- 配合 `--resume-drops-turn` [V] 可在到达点之后"丢一条 turn"。
- 前端设计点：
  - 每条 user 消息旁加「↶ 回到此处（清上下文）」
  - 与 checkpointing 按钮**并存**，职责清晰分工。
- 预计工作量：与 checkpointing 类似，1 天。

### 2.9 `auto` 权限模式（v2.1.218+，模型分类器审批）

- [V] CLI 已支持（`--permission-mode auto` 选项里有）。
- 子代理继承父模式且 `bypassPermissions`/`acceptEdits`/`auto` 不可被 subagent 覆盖。
- 价值：在 `auto` 模式下，绝大多数 prompt 不打扰用户，只有真正危险的操作（critical path rm 等）才弹审批 — 比 `bypassPermissions` 安全、比 `default` 安静。
- cc-webui config 的 permission_mode enum 加上 `auto`；前端下拉加上。
- 预计工作量：后端 1h（enum 加值），前端 1h。

---

## T3 — 大价值高成本（架构级）

### 3.1 per-session MCP 配置（`--mcp-config`）

- [V] flag 已确认。session 创建/编辑时让用户上传 MCP 配置 JSON 或选本地文件路径，写入 `--mcp-config`。
- 价值：每个会话可用不同工具集（浏览器自动化、数据库、内部 API...）。
- 预计工作量：1 天（UI + 后端 + settings.json 物化）。

### 3.2 per-session 自定义 Agent（`--agents`）

- `--agents <json>` [V]，在 session 内注册程序化 subagent（如 `{"reviewer":{"description":"...","prompt":"..."}}`）。
- 价值：把项目级的「code reviewer」「security auditor」做成会话级。
- 预计工作量：1-2 天（需要设计 UI 来编辑 agent 定义 + 选哪些 agent 注入该会话）。

### 3.3 Plugin per-session（`--plugin-dir` / `--plugin-url`）

- [V] flag 已确认。当前 plugin 是 user/project 级别的，session 级支持更好隔离。
- 工作量与 3.2 类似，可打包做。

### 3.4 Structured output（`--json-schema`）

- `--json-schema <schema>` [V]：强制 assistant 输出符合 schema 的 JSON。
- 价值：把 claude 当成自动化任务 runner（提取结构化数据、生成配置文件...）。
- 前端：提供 schema 编辑器（JSON Schema 简化版），结果以 JSON 高亮 + 校验展示。
- 工作量：1-2 天。

### 3.5 SessionStore 可插拔

- SDK 0.2.x 新增：把 session transcript 存到自己 DB 而非 `~/.claude/projects/`。
- cc-webui 设计本来就依赖 `~/.claude/projects`（为了和 TUI 互操作 + import 功能），**这个不做**。
- 但「等价的 Go 实现」已经在 `internal/claudehist` 里有了（`list_sessions`、`get_session_info`、`get_session_messages` 等的 Go 版），已是 done。

### 3.6 CLAUDE.md viewer / editor（per-session 项目记忆）

- `system/init` 事件含会话加载的所有 CLAUDE.md / rules 路径。SDK 通过 `setting_sources` 控制（user / project / local），cc-webui 可以让用户**选择**本次会话加载哪些源（默认全开）。
- 前端：
  - session 配置面板显示「加载的 CLAUDE.md」列表（点开看内容，编辑保存 → 自动触发新会话）。
  - 注意 CLAUDE.md 是项目文件，写回盘上 — 用现有 `/api/files` 权限模型即可。
- 价值：用户远程给项目加「记住这条规矩」很方便。
- 工作量：2 天（前端编辑 + 后端解析 init.memory_paths 路径）。

### 3.7 Filesystem hooks UI（编辑 `.claude/settings.json` 的 hooks 段）

- Hooks 类型丰富（`command` shell 脚本、`http` POST 到端点、`mcp_tool`、prompt、agent），cc-webui 可以提供 UI 让用户在项目 `.claude/settings.json` 里可视化编辑 hooks。
- 杀手场景：用户加一个 `Notification` hook，类型 `http`，POST 到 cc-webui 自己的 `/api/hooks/notify` 端点 → cc-webui 给前端 SSE 推送 → 前端弹桌面通知 / Web Push。
- 工作量：2-3 天（UI + 后端 hook 端点 + 通知推送）。

### 3.8 `setting_sources` 控制面板

- `--setting-sources "user,project"` [V] 让用户明确选择本次会话加载哪些 settings 源（user = ~/.claude/，project = <cwd>/.claude/，local = 本地覆盖）。
- 用法：用户想做一个「纯净环境」会话（不加载任何 CLAUDE.md/skills/hooks），传 `--setting-sources ""`。
- cc-webui 价值：多租户隔离 / 可复现会话 / 调试「去掉 .claude/ 配置后行为变了」。
- 工作量：1 天。

---

## T4 — 体验润色（不影响核心功能）

- [x] **渲染三件套**（2026-10-09）：CJK autolink（尾部中文标点从 href 剥离）、单 `~`
  保护（`~~` 删除线不受影响）、空消息过滤（空 text/thinking 不占位，时间线不断
  裂）、工具三态 + 耗时（执行中蓝点转圈 → ✓/✗，summary 行实时计时 + 定格）、
  edit/write diff（`(−N +M)` 统计、Write 200 行截断、上下文折叠越界修复）。
- [ ] **Subagent 消息浏览面板**：UI 上点 subagent 卡片 → 打开侧栏显示完整 subagent transcript（前端 + `claudehist` 子 agent 解析已支持）。
- [ ] **rate limit 状态栏**：流里 `rate_limit_event`（SDK 0.2.x 类型）出现时显示在状态栏。
- [ ] **Web Push 通知**：turn_end 时浏览器通知；PWA 友好（service worker 已就绪）。
- [ ] **token / 成本曲线**：把 `result.usage` 累积成图表（Chart.js），看每轮趋势。
- [ ] **thinking 块默认折叠状态**：现在 live 时是展开，完成后是折叠；统一或加偏好。
- [ ] **批量操作**：多选会话 → 归档/删除/导出。
- [ ] **prompt 模板片段**：composer 顶部加几个快捷按钮（review / explain / refactor / test）。
- [ ] **快捷键**：⌘K 全局搜索会话；⌘/ 切换 cwd 面板。

---

## 实测 / 优先级待定

| 项 | 状态 | 说明 |
|---|---|---|
| `--permission-prompt-tool` | ⚠️ 纠正 | 曾标 [V] 为 shell 命令；2026-10-09 核实官方文档：它是 **MCP tool 名**（需跑 MCP server + 30s 启动等待），遂未采用，改走 `PermissionRequest` hook |
| `--max-turns` | ✅ [V] | 同上 |
| `--max-thinking-tokens` | ✅ [V] | 同上 |
| `--thinking-display` | ✅ [V] | 同上 |
| `--resume-session-at` | ✅ [V] | **新发现** — 与 `--resume` 配合使用 |
| `--resume-drops-turn` | ✅ [V] | （SDK 文档）；可能需配合 `--resume-session-at` |
| `--task-budget` | ✅ [V] | 单 turn token 预算 |
| `--session-id` | ✅ [V] | 自定义 session UUID |
| `--include-partial-messages` 在 `--input-format text` 路径 | ✅ | 已实测兼容（`--replay-user-messages` 才要求 stream-json） |
| `--bare` 模式 | ⏭ | 价值低（troubleshooting），跳过 |
| `--safe-mode` | ⏭ | 同上 |
| `--sandbox` | ❌ | CLI 不支持；SDK 独有（`SandboxSettings` 类型） |
| Agent teams（多 Claude 实例协同） | ⏭ | 纯 CLI 特性，cc-webui 用不上 |
| Web/cloud session（`--cloud` / `--from-pr`） | ⏭ | 与本仓库自托管定位不符，跳过 |

---

## 实施顺序建议

按「用户体验杠杆 / 实现成本」综合排：

1. T2.1 checkpointing + 回滚 UI（你已点名要做的）
2. T2.6 slash command 按钮（半天活，最大甜点）
3. T1.1 + T1.2 一组（防跑飞 + 细粒度控制，单 flag 多）：max-budget、max-turns、effort、fallback、append-system-prompt、add-dir、autocompact、max-thinking、session-id、tools 策略
4. T2.2 subagent 流式渲染（让 subagent 工作可见，重要 UX 提升）
5. T2.3 + 2.7 prompt 建议 + /compact 按钮（小成本，组合做）
6. T2.8 resume-from-message（与 2.1 互补，工期相近，一起做）
7. T2.4 移动审批（最大架构动作，远程场景差异化卖点）
8. T3.x per-session MCP / Agents / Plugins / Structured output / CLAUDE.md / hooks UI / setting_sources（按需求做）

---

## T5 — 待设计（用户已点名，排期在权限卡+渲染验收之后）

### 5.1 多 agent 群聊 orchestrator

- 等权限卡 + 渲染两批验收通过后再做。对标 KyrieXu11/cc-webui 的 `server/groups/`：
  canonical transcript + `@claude/@codex/@all` 路由 + pipeline 顺序执行（后者
  在 prompt 里以 `[来自 X 的回复]` 看到前者输出）+ 每 agent 独立 model/mode/
  effort/system prompt + `GroupChatView`。
- 本仓库差异点：四引擎（对方只有 claude/codex），常驻 peer 消息已有一套点对点
  通道，群聊是" orchestrator 组装 prompt" 的另一条路，两者关系需先定（复用
  transcript 还是另起 group transcript）。

### 5.2 文件面板 + 终端面板

- 用户想做，但涉及安全，先设计再动手，不直接开工。
- 安全面清单（设计时必须回答）：
  - 文件面板现在 `/api/files` 是**只读 + 限会话 cwd + EvalSymlinks + 2MiB +
    拒二进制**；可编辑意味着写回盘上 → 需要写白名单（cwd 内 + 拒 dotfile/
    拒 symlink 逃逸）、并发写冲突（CLI 正好也在改同一文件）、大文件截断策略。
  - 终端面板意味着远端 shell → 需要执行白名单/黑名单（`rm -rf /` 熔断）、进程组
    隔离 + 超时 + 输出窗口上限（参考对方 256KB rolling）、会话级隔离、重启后孤
    儿进程清理。默认必须关闭，显式开启才给。
  - 鉴权面：`gateway_token` 是唯一 gate，面板一旦可写/可执行，token 泄露 = 整机
    沦陷，需先评估 token 分级或只读/读写模式开关。
9. T4 体验润色长尾