# 「需要你」队列：注意力路由与作答路径 设计与实施

> 状态：设计已定稿（2026-11-11），**未实施**。
> 关联：看板与产物见 `AGENTS.md` §15；pending 注册表与强提示见 §11；worker 通知见 §16；
> A2A 总线见 `a2a-rd.md`；主会话派工用法见 `docs/main-agent-best-practices.md`。
> 原型：对话中交付过桌面/移动端可交互 demo（仅演示，未入库）。

## 1. 背景

### 1.1 现状

单用户本地控制台里同时跑多个 worker 会话，协调者（主 Agent）负责路由与记账：

- **主对话页** = 用户 ↔ 协调者的聊天瀑布流，worker 的 A2A 回报以气泡混在同一流里。
- **「需要人」有三种来源，路径完全不同**：
  1. CLI 自身的门：`AskUserQuestion` / 工具权限确认 —— **55s 硬超时**，由 pending 注册表管理，卡片停在**当前会话** composer 上方，SSE 只订阅当前会话。
  2. worker 报给协调者的「【需确认】」（A2A 自由文本）—— 无超时。
  3. 待验收（看板状态）。
- 强提示已有三处：顶部横幅（toast layer）+ 侧栏整行脉动（`.has-ask`）+ 工作区栏计数徽标。

### 1.2 失效场景（用户原话）

> 「并行任务太多的时候，主对话是瀑布流形式，我发的新任务和 worker 回报会掺在一起，多个 worker 回报也会掺在一起，所以有时候 worker 要求用户的回复，我经常会遗漏。」

- 用户在别处时，某 worker 的提问只停在它自己的会话里，看不到，直到超时作废、模型重问。
- 一个 worker 的「需要人」被别的 worker 的流水账刷走。

### 1.3 根因

1. **没有持久的队列**：三处提示都是瞬时唤起（横幅来了就走、脉动按会话聚合、徽标只有数字），没有「未处理」的持久态，无法排序、无法跨会话比较。
2. **没有分级**：三类门共用一个为 55s 门设计的打断通道，噪声基线被抬高。
3. **作答路径没有记账**：用户直接打开 worker 会话作答（现状可行）时，协调者对此毫无感知 —— 真正的「绕过」早已发生，缺的是账。
4. **提问超时语义错误**：超时回「请稍后重新提问」→ 模型白烧一轮重问，卡片作废，用户再次错过。

## 2. 设计决策（已定）

### 2.1 作答路径：直达 worker + 系统代记账

**用户直接答给被阻塞的 worker；作答瞬间由服务端原子完成三件事：**

1. resolve pending 注册表（现有路径）；
2. 往任务看板 timeline 追加一条「用户答了 X → Y」；
3. 给协调者发一条系统 A2A 回执（`from=runtime`，与 worker 异常通知同一条路径）。

一句话：**人答给 worker，账记给协调者**——记账不依赖 LLM 转述，而依赖系统事件。

| | 直达 worker | 经协调者中转 |
|---|---|---|
| 延迟 | 点击即 resolve，毫秒级 | 协调者是 LLM 会话，一轮 turn 几十秒起步且可能正忙（A2A 投递是排一轮），55s 内**物理上跑不完** |
| 记账准确性 | 需补系统回执（成本极低） | 天然经过协调者，但 LLM 转述可能丢失/改写决定 |
| 状态一致性 | pending 注册表单一写入者 | 两个写入者 + 与超时赛跑，转达到达时门可能已作废 |
| 算不算绕过协调者 | 不成立——协调者是人的**代理**而非审批人；且直达今天已发生 | 把人的决定降级为 LLM 的输入，多一层不可靠 |

论证要点：

- 55s 门走协调者来不及，这不是取舍是物理约束（协调者排队 + LLM 推理 >> 55s）。
- 「直达」不是新行为：用户今天就能打开 worker 会话答那张卡，协调者无感知 —— **绕过早已发生，缺的是记账**。设计任务是补记账，不是禁直达。
- 系统是唯一可靠见证者（答案本来就经过它）。业界同构：Temporal 的 signal 直达被阻塞的 workflow，event history 由系统写；LangGraph 的 resume 直达被中断的节点。没有人把 human signal 路由过另一个 agent。

### 2.2 三种门：呈现统一，传输分治

- **统一成一种「待办条目」模型**：`{来源会话/任务, 类型, 状态, 紧急档, 剩余TTL, 作答动作}`。用户在一个队列里看到它们，作答动作统一为 **resolve + 回执**。统一的是用户心智模型。
- **传输保持三条各自原子的写入路径**（pending 注册表 / A2A / board 状态迁移），不合并管道——合并没有用户价值，只会把 runner 与 board 的分层耦合。

### 2.3 信息架构：按时间尺度分三层

| 层 | 表面 | 承载 | 不承载 |
|---|---|---|---|
| 注意力层 | 「需要你」队列（桌面右栏可收起面板 / 移动端一等 tab） | 需要人拍板的**动作**，粒度=动作、按任务分组 | 流水账、历史 |
| 对话层 | 主对话页 | user ↔ 协调者正文 | worker 回报（折叠成「N 条 → 动态」分隔条） |
| 状态层 | 看板（卡片/时间线/产物）+ 任务动态（事件流） | 状态的真源、产物档案、worker 流水 | 实时打断 |

- **看板保留不动**：卡片、时间线、产物（artifacts）是「回看之前任务」的唯一入口。
- **动态是看板的投影视图**（各卡 timeline 事件 + worker 活动），不是新存储，住在看板里（桌面为看板页面板，移动为「任务」tab 顶部折叠区）。
- **侧栏**：保持会话导航（ParentID 嵌套不动）+ 顶部「需要你 N」计数入口。

**队列规则**：

- 粒度 = **动作**（一次提问 / 一次确认 / 一次验收）；按任务分组。不能是会话粒度（一个会话可挂多条待答，按会话无法排序）。
- 排序：档间固定 **权限 > 待补答 > 阻塞 > 验收**；档内：权限按剩余 TTL 升序，待补答/阻塞按已等待时长降序，验收按到达时间。
- 升级：阻塞超过阈值（默认 10min）提一档提示；**权限超时转为「回看项」**（见 2.5）。

### 2.4 通知矩阵（一条事、一个地方、一声响）

| 档 | 内容 | 通知强度 |
|---|---|---|
| 超时档（仅权限门） | 55s 硬超时，结果随时间分叉 | 顶部横幅 + 提示音 + 队列置顶 + 侧栏脉动（现有三件套**只保留给这档**） |
| 待补答（提问超时后） | worker 已收工等待，补答永久有效 | 队列 + 角标，无声音无横幅 |
| 阻塞档（看板「待人工」） | worker 停在一个决策上 | 队列 + 侧栏静态标记，无声音 |
| 验收档（看板「待验收」） | 不阻塞任何事 | 仅队列 |

依据：告警分级（航空 ECAM/EICAS 的 warning/caution/advisory，低档禁止打断）+ 报警疲劳文献（The Joint Commission 2014 NPSG.06.01.01；ECRI 历年头号技术危害）。同一事件多处报警 = 训练用户全部无视。

### 2.5 超时语义（按门分型，最终决定）

| | 窗口内（55s） | 超时瞬间 | 超时后卡片 | 迟到动作 |
|---|---|---|---|---|
| **提问** | tool_result = 答案，同轮继续 | helper 回「用户暂不在；**结束本轮等待**，勿自行探索；回答会以消息送达」→ worker 收工 | 转「待补答」，**永久有效** | 补答 = 消息送达，worker 接着做，**效果同限时内** |
| **权限** | allow/deny 立即生效 | 自动 deny（安全默认，不可悬空） | 转「回看项」 | **补发指令重新执行**（附一次性放行，否则重试时又撞权限卡） |

要点：

- 55s 对 claude/qoder 是 **Claude Code hook 的 60s 硬上限**倒逼（`internal/runner/permission.go:10-13` 注释已写明），窗口不可延长；对 opencode 是自设的 `ocCardTimeout`（`internal/runner/octurn.go:444-450`，opencode 本身无限等）。即便能延长，统一语义也更重要：**先结束等待 + 迟到补送**优于让 worker 干等。
- 提问超时**不让模型「按最佳判断继续」**：模型发问就是因为答案重要，自行探索会产生返工。让 worker 收工等待，补答到达后继续，语义与窗口内作答完全一致。
- 迟答不是那条 tool_use 的 tool_result（已随超时定案、不可改写），而是**新消息注入**（常驻 stdin / `Manager.Enqueue` / `deliverA2A`），模型在同一份 transcript 里自然对账。
- 补答消息必须带**问题原文 + 答案**（AskUserQuestion 一次可问多题，按键值对列全）。
- 模型不遵守「结束本轮」指令是软约束风险：即便它继续探索，迟答到达仍起纠正作用，自动降级为纠正模式，不会更糟。

## 3. 现状代码盘点（锚点）

| 机制 | 位置 | 现状行为（要改的点） |
|---|---|---|
| 55s 常量 | `internal/runner/permission.go:14` | `PermissionTimeout = 55s`，claude hook 60s 上限倒逼，不可延长 |
| helper 超时分支 | `internal/server/permission.go:89-105` | 超时 `PermissionTimeoutRemove` **删除条目**；文案：普通工具「用户 55s 内未审批，自动拒绝」、AskUserQuestion「用户暂未回答，请稍后重新向用户提问」 |
| 注册表 | `internal/runner/permission.go:37-51` | `pending map[id]*PermissionRequest`，无状态位；`pendingFor` 只返回在册条目 |
| 迟到 resolve | `internal/server/permission.go:141-147` | 条目已删 → 返回 `{ok:false, error:"prompt already resolved or timed out"}`，**无补答路径** |
| opencode 卡片 | `internal/runner/octurn.go:450`、`:625`；`octurn_v2.go:475` | `ocCardTimeout=55s` 自设；超时主动 reject 释放 Deferred |
| 队列数据面 | `internal/server/server.go:557`、`:580-604` | `sessionView.PendingAsks` 由 `pendingAsksOf` 从注册表派生，仅 live 条目 |
| 前端强提示 | `web/app.js` | `raiseAskToast` / `diffPendingAsks` / `focusAskCard` / `fetchPendingPermissions`；toast layer、`.session-item.has-ask`、`#ws-activity` 徽标 |
| A2A 投递 | `internal/server/a2a.go:116` | `deliverA2A(from,to,message)` 排一轮，不要求常驻 |
| 看板记账 | `internal/server/agentdeck_mcp.go:345-360` | `appendA2AComment` 按 task_id 追加 timeline 评论 |
| 看板状态 | `internal/boarddb/store.go:30` | `新建/进行中/待人工/待验收/已完成/已取消` |
| worker 通知 | `internal/server/incidents.go:156` | 经 `deliverA2A(from=runtime)` 通知所有 main_agent |
| 会话删除清理 | `internal/runner/permission.go:234-242` | `ClearPermissions` 只清 allowances，**不清 pending**（新逻辑要顺手清） |

## 4. 目标模型

### 4.1 队列条目

```jsonc
{
  "id": "pending id 或 board 派生 id",
  "kind": "perm" | "ask" | "review" | "blocked",   // blocked = 看板「待人工」
  "state": "live" | "expired-awaiting" | "expired-review",
  "session_id": "…", "session_name": "w · 支付重构",
  "task_id": "看板卡 id（有绑定才有）",
  "question": "…", "options": ["…"],
  "created_at": 0, "expires_at": 0,     // 权限/提问的 55s 截止
  "answered_at": 0
}
```

数据源（v1）：

- `pending` 注册表（perm + ask，含过期保留条目）；
- 看板：绑定到 worker 会话且状态为「待人工」/「待验收」的卡（`boarddb.TaskForSession`）。

**明确不做（v1）**：不解析 A2A 自由文本里的「【需确认】」。worker 该用看板状态「待人工」表达结构化阻塞；后续可加 MCP 工具 `ask_human` 直通队列（见 §9）。

### 4.2 状态机

```
live ──55s──> expired-awaiting（提问）──补答──> resolved(投递消息)
  │                └── 一直有效（会话删除/服务重启前）
  └──55s──> expired-review（权限）──补发指令/确认──> resolved(投递消息 + 一次性放行)
  └──用户作答──> resolved（tool_result 直达）
```

- `resolved` 后条目移出注册表；重复提交返回 `{ok:false}`（现有语义）。
- **持久化**：`expired-awaiting` 条目落盘 `data/pending-asks/<sid>.json`（很小），服务重启后恢复为队列项（补答投递在重启后照样工作；worker 进程重启后由 `--resume` 续接上下文）。live 条目无需落盘（helper 随服务重启一起死）。
- 清理：会话删除时清（`ClearPermissions` 扩展为同时清 pending 与该会话的落盘文件）；产品语义「补答一直有效」= 不设 TTL。

### 4.3 记账（receipts）

作答成功后（窗口内/迟到/权限纠正/验收，同一套）：

1. **看板**：worker 若绑定了 task，追加 timeline 评论「用户回答/审批：…（直达 worker）」；验收类直接做状态迁移（待验收→已完成 / 打回→进行中）。
2. **协调者**：`deliverA2A(from="runtime", to=main_agent, …)` 回执，让协调者的上下文里有这个决定（它下一轮汇总时自然读到）。
3. 幂等：同一 pending id 只记一次。

## 5. 实施计划

> 原则：每一步可独立发布、可回滚；先后端语义，后前端表面；`go test ./...` + `scripts/frontend-tests/run.sh` 全程绿。

### 阶段 1：提问超时语义（核心，独立可发）

改动：

1. `internal/runner/permission.go`
   - `PermissionRequest` 加 `ExpiredAt time.Time`（零值=live）。
   - `PermissionTimeoutRemove` 拆分：普通工具→删除（现状不变）；AskUserQuestion→`PermissionMarkExpired(id)` 保留条目。
   - `pendingFor` 返回过期条目（带状态），供 `PendingAsks` 下发。
   - `resolvePending` 对过期 AskUserQuestion 返回「迟到」结果，调用方走消息投递而非唤醒 channel。
2. `internal/server/permission.go`
   - 超时文案：AskUserQuestion 改为「用户暂时不在；结束本轮，不要自行决定或继续探索；回答会以消息形式送达，届时按回答继续」。
   - 超时分支：AskUserQuestion 不删条目，broadcast `permission_resolved` 带 `expired:true, awaiting_answer:true`（前端据此保留卡片转「待补答」）。
   - `handlePermissionResolve`：命中过期条目 → 构造补答消息（问题原文 + 答案键值对）经 `deliverA2A`（worker 无地址名则 `Manager.Enqueue` 按 session id）排一轮；返回 `{ok:true, late:true}`；广播 `permission_late_answer` 瞬态事件。
   - 补答事件建议新增 `source:"late-answer"`（runner `Enqueue` 的 source 字段），前端渲染为用户气泡 + 「补答」小标记，轨迹视图可区分。
3. opencode 侧：`octurn.go` / `octurn_v2.go` 超时分支对齐（卡片保留 + 补答注入）。**待实测**：`RejectQuestion` 是否支持自定义消息；不支持则 reject 仅用于释放 Deferred，语义靠补答消息兜底。
4. 持久化：`data/pending-asks/` 落盘 + 启动恢复 + 删除会话时清理。

测试：`internal/runner/permission_test.go`（过期保留/迟到结果）、`internal/server` 新增超时→补答→投递的单测（用假投递钩子）。

### 阶段 2：队列数据面

1. `pendingAskView` 扩展 `state/kind/task_id/expires_at`（`internal/server/server.go:604` 一带）。
2. 新增只读聚合接口 `GET /api/needs-you`：`{items:[…], counts:{perm,ask,blocked,review}}`。数据源 = 注册表 + 看板（待人工/待验收，绑定 worker 的卡）。
3. 排序在服务端做（规则见 2.3），前端不重排。

### 阶段 3：作答路径与记账

1. 直达作答复用现有 `POST /api/sessions/{id}/permissions/{pid}`（`handlePermissionResolve`），扩展 `action`：
   - `answer`（默认，现状）；
   - `retry`（仅 expired-review 权限项）：「补发指令重新执行」= 消息注入 + **一次性放行**（`permissionState` 加 `oneShot map[sessionID]map[tool]int`，helper 命中即消耗，不进长期 allowances）。
2. 记账：新增 `server/answer_receipt.go`（或并入 permission.go）：board timeline 追加（`appendA2AComment` 同款，或直接用 boarddb comment API）+ 协调者 A2A 回执；幂等按 pending id。
3. 验收：队列里的 review 项直接调看板状态迁移 + 回执，不经注册表。

### 阶段 4：前端队列（桌面）

1. 新增右栏「需要你」面板（可收起，状态存 localStorage），条目卡样式按 demo；侧栏顶部计数入口保留；composer 上方 dock 保留为「落点」（`focusAskCard` 跳转目标不变）。
2. `web/app.js`：`renderNeedsYou()` + 5s 轮询（或并入现有 `loadSessions` 轮询）；`diffPendingAsks` 改为 diff 队列；`raiseAskToast` **只对 perm live 触发**（ask 不再弹横幅/声音）。
3. 前端测试：`scripts/frontend-tests/needs_you_queue.test.js`（排序、角标、过期态、补答提交、review 动作）。

### 阶段 5：移动端

1. 底部 tab：对话 / 需要你（角标）/ 任务（看板 + 动态折叠区）。
2. 队列全屏、卡片全宽、作答按钮 ≥42px 落拇指区；权限门横幅在任意 tab 顶部可见。
3. 看板保持现状能力：状态分组、卡片详情（时间线 + 产物预览）。

### 阶段 6：主对话降噪（可选、单独开关）

- `renderEvent` 对来自 worker 的 A2A 消息折叠为「N 条回报 → 动态」分隔条（只影响用户视图，不影响协调者上下文）。
- 开关：config `chat.hide_worker_reports`（默认关，观察后再开）。

## 6. 移动端映射

| 桌面 | 移动 |
|---|---|
| 侧栏 | 抽屉（现有） |
| 主对话 | 默认主屏 |
| 「需要你」面板 | 底部一等 tab + 红点角标，全屏队列 |
| 看板 | 「任务」tab：状态分组卡片，已完成默认折叠；点卡进详情（时间线 + 产物预览） |
| 任务动态 | 「任务」tab 顶部折叠区 |
| 权限横幅 | 页内顶部横幅（任意 tab 可见）——手机没有侧栏脉动这条通道，横幅权重更高 |

不做：把桌面三栏等比压成三条窄栏；移动端分屏。

## 7. 业界参照

- **LangChain Agent Inbox**（LangGraph interrupts 的官方 UI）：粒度=interrupt、按 thread 分组、resume 直达被中断节点——与本设计几乎同构。
- **Temporal signals**：外部决定直达被阻塞的 workflow，event history 由系统记账——「直达 + 系统记账」的范本。
- **Linear / GitHub Notifications inbox**：triage 队列、Done 清零、按项目分组。
- **PagerDuty**：ack 与 resolve 分离 + escalation policy——对应「阻塞超时提档 / 权限超时转回看项」。
- **航空告警 ECAM/EICAS**：warning/caution/advisory 三档，低档禁止打断，同一故障不重复告警——对应通知矩阵。
- **Alarm fatigue**：The Joint Commission 2014 NPSG.06.01.01；ECRI 历年 Top 10 Health Technology Hazards——冗余报警训练用户无视，这是「三处提示同一提问」在多任务下失效的理论依据。

## 8. 备选方案与否决理由

| 方案 | 内容 | 否决理由 |
|---|---|---|
| 任务中心 | 看板升级为首页，卡内嵌迷你对话与作答，会话退为任务详情 | 重构最大；产品从「会话控制台」变成项目管理工具；55s 门落点变深（先找卡） |
| 流内分流 | 不加新表面，主对话拆两栏（左对话/右动态+待答泳道） | 待答没有一等地位；分屏挤压对话宽度（对话是主任务）；多任务时右栏照样滚屏 |
| 经协调者中转作答 | 用户答协调者，协调者转达 worker | 55s 内物理跑不完；LLM 转述不可靠；与超时赛跑必输 |
| 提问超时让模型「按最佳判断继续」 | 超时后模型自行决定，迟答起纠正作用 | 模型发问就是因为答案重要；自行探索产生返工，违背发问初衷 |

**采纳**：统一队列（本方案），吸收「回报出主流」作为对话层改造，看板维持现状做状态层。

## 9. 风险与边界

- **claude hook 60s 硬上限**：55s 窗口不可延长（`permission.go:10-13` 已写明），补答消息方案是唯一出路；opencode 窗口虽可延长，但统一语义更重要。
- **模型不遵守「结束本轮」**：软约束；迟答仍起纠正作用，降级安全。
- **补答与用户其他输入撞车**：补答按队列顺序到达，语义上是「用户的新输入」，模型自行对账；可接受。
- **服务重启**：live 条目丢失（与现状一致，helper 也死了）；`expired-awaiting` 落盘恢复，保证「补答一直有效」。
- **A2A 自由文本「需确认」不入 v1 队列**：用看板「待人工」结构化承接；后续可加 MCP `ask_human` 工具直通队列（一步到位、可记账）。
- **记账不依赖 LLM**：回执是系统事件；协调者若正忙，回执排在它的队列里，不阻塞用户作答。
- **不做**：多用户/权限体系、跨引擎统一传输管道、队列 TTL（产品语义是永久有效）。

## 10. 验收标准

1. **提问超时**：55s 后卡片仍在队列（转「待补答」）；worker 结束本轮且不继续探索；任意时刻补答 → worker 下一轮收到「问题原文 + 答案」并继续，行为与窗口内作答一致（实测 claude 一次性/常驻、qoder、opencode）。
2. **权限超时**：卡片转「回看项」；「补发指令重新执行」后 worker 重试，且一次性放行生效（不再弹卡）。
3. **记账**：每次作答后，绑定任务卡 timeline 有记录、协调者收到 A2A 回执；重复提交幂等。
4. **通知**：权限门才出横幅/声音；提问/阻塞/验收只入队；侧栏与 tab 角标与队列条数一致。
5. **重启**：重启后待补答条目仍在，补答可投递。
6. **回归**：`go test ./...`、`go vet ./...`、`./scripts/frontend-tests/run.sh` 全绿；看板/产物/移动端任务 tab 能力不回退。
