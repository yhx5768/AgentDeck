# 知识沉淀 P0 实现说明

> 状态：P0 已实现并起隔离测试实例；P1/P2 未做。
> 设计依据：`docs/knowledge-design.md`（`docs/knowledge-design.html` 是设计预览）。
> 范围：`feature/agentdeck-knowledge` 分支，P0 vertical slice；未 push/CR/部署。

## 1. 已实现

| 层 | 内容 |
|---|---|
| 数据 | 独立 `<data_dir>/knowledge.db`（SQLite + FTS5）；entries / tags / evidence / relations / events / FTS 表；draft/approved/stale/rejected/quarantined/superseded 状态；去重键；证据；审计事件 |
| 检索 | 应用侧 CJK unigram + bigram 分词（`看板`、`重编号` 可命中）；BM25 排序；项目/标签/状态/来源任务过滤；分页 |
| API | `GET /api/knowledge`、`GET /api/knowledge/{id}`、`POST /api/knowledge`、`PATCH /api/knowledge/{id}`、`POST /api/knowledge/collect` |
| MCP | `knowledge_search` / `knowledge_get` / `knowledge_add`（所有会话）；`knowledge_review` / `knowledge_collect`（仅主 Agent，沿用 env + `requireMainAgent` 两层闸门） |
| 采集 | `POST /api/knowledge/collect` 扫描看板 `待验收/已完成` 任务，生成每任务 1 条 draft；按 task 幂等；只读看板字段 + 已托管 artifact 元数据；敏感命中 quarantined；不读工作区/transcript/legacy report_file 路径 |
| UI | mainAgent 会话的「资料空间」tab（与「看板」同级，2026-11-10 按用户反馈从独立页/侧栏入口改为 tab 绑定）；桌面列表+详情两栏，移动端详情全屏 sheet；搜索、项目/状态筛选、审批、从看板采集、新建；看板任务抽屉「相关知识」（只读，最多 3 条 + 查看全部切 tab + 从本任务提炼）；深浅色跟随现有主题；`knowledge.js` |
| 测试 | Go 单测（tokenizer/FTS/去重/敏感/collect/权限/HTTP）；前端 jsdom 8 项；CDP 真浏览器验证 1440/1024/768/390 × light/dark，0 console error、无横向溢出 |

### 1.1 UI 结构（2026-11-10 重设计）

- 入口：`#chat-tabs` 内的「资料空间」tab，位于「看板」右侧；只有 `main_agent` 会话显示（`syncMainAgentTabs` 同时控制 board/knowledge，非 mainAgent 隐藏两个 tab，若停在其中则回「对话」）。侧栏右上角入口已删除。
- `switchChatTab('knowledge')`：显示 `#knowledge-view`，隐藏消息区/轨迹/看板/composer；切走时关闭详情并清掉选中态。知识库不再是覆盖层/独立 workspace 页。
- 顶部 header：标题 + `mainAgent · 会话名` + 统计 chips（已通过/草稿/可能过期，可点击筛选）+ 搜索 + 项目/状态筛选 + 「从看板采集」「新建知识」；无独立返回按钮（tab 切换即可）。
- 桌面：左侧卡片列表（状态/项目/来源任务/更新时间/标题/摘要/标签），右侧详情面板（正文 Markdown、来源任务与 artifact、证据、审批/过期动作）；选中卡片有左侧 accent 边和描边。
- 移动端（≤900px）：列表全宽；详情为全屏 sheet（右上角关闭），避免页面级横向溢出和嵌套滚动。
- 加载/空/错误三态：列表加载中、无结果空态、请求失败带「重试」。
- 看板抽屉「查看全部」→ 切到资料空间 tab 并带 `source_task_id` 过滤（header 显示可清除的任务 chip）；「从本任务提炼知识」→ 打开新建知识 modal。
- 深链 `?knowledge=1` / `?knowledge=<id>`：先定位 mainAgent 会话，再切到资料空间 tab；`<id>` 直接打开详情。`?theme=dark|light` 仍用于预览验证。

## 2. 未实现 / TODO

- **自动触发**：任务状态变化/artifact 发布后自动入队尚未接；P0 用手动「从看板采集」和 `knowledge_collect` 演示。设计里的 hook 位置已留（server handler 成功后调用 collector）。
- **artifact 文本抽取**：collect 只写 artifact 标题/类型/文件名的元数据和证据，不解析 HTML/Markdown/CSV/JSON 正文；P1 再做。
- **relations API**：表和详情已支持 relations，但 `knowledge_link` / supersedes 写接口未暴露（P0 任务未要求）。
- **审计 UI**：`knowledge_events` 已写，但详情页暂未展示；P1 加。
- **stale 自动流转**：`stale` 状态可由审批动作设置，但没有定时把超期 approved 转 stale 的后台任务；P1 加。
- **自动 approved**：P0 全部 draft；主 Agent/UI 可手动通过。用户逐字决策自动 approved 的策略未接。
- **知识文件上传**：千问式用户直接上传文件（`data/knowledge/files/`）未做；P0 只引用任务 artifact。
- **语义检索/周报/分享**：P2。
- **多用户 ACL**：当前单用户控制台；HTTP UI 视为 admin，MCP 按 `main_agent` 闸门。不是权限系统。
- **opencode/mcode 共享 MCP 身份**：没有 per-session `CC_WEBUI_A2A_SESSION` 时，worker 仍可 `knowledge_search`（approved）和 `knowledge_add`（必须带 `source_task_id`），但无法按 session 认领自己的 draft；主 Agent 审批。
- **UI 的 `PATCH` 语义**：用 `PATCH /api/knowledge/{id}` + `{action: approve|reject|stale|restore}` 完成审批；没有单独的 `/review` 路由。
- **分页 UI**：列表首屏最多 50 条；API 支持 `offset`，但页面暂无「加载更多」。

## 3. 代码位置

**新增**
- `internal/knowledge/tokenize.go` — CJK unigram/bigram 归一化与 MATCH 查询构造
- `internal/knowledge/redact.go` — 敏感扫描/脱敏（凭据、私钥、cookie、高熵串）
- `internal/knowledge/store.go` — SQLite schema、CRUD、状态、去重、证据、FTS、审计
- `internal/knowledge/store_test.go` — tokenizer/FTS/去重/敏感/collect 前置
- `internal/server/knowledge.go` — HTTP handler、MCP 数据函数、collect、board 装饰
- `internal/server/knowledge_test.go` — collect 幂等/权限/HTTP 可见性
- `web/knowledge.js` — 资料空间页 + 看板「相关知识」渲染
- `scripts/frontend-tests/knowledge.test.js` — 8 项 DOM 契约

**修改**
- `internal/a2a/mcp.go` — `KnowledgeTools`（search/get/add）与 `KnowledgeAdminTools`（review/collect）；stdio 代理按 `CC_WEBUI_A2A_KNOWLEDGE_ADMIN` 追加管理工具
- `internal/runner/browser_mcp.go` — 主 Agent 的 MCP env 加 `CC_WEBUI_A2A_KNOWLEDGE_ADMIN=1`
- `internal/server/agentdeck_mcp.go` — knowledge 工具分派 + `requireMainAgent`
- `internal/server/server.go` — knowledge store 初始化/关闭、路由
- `internal/server/agentboarddb.go` — 任务响应注入 `knowledge` / `knowledge_count`；snapshot 批量装饰
- `internal/boarddb/store.go` — `toMap` 补 `description`（collect 需要；此前 snapshot 不暴露描述）
- `web/index.html` — 侧栏 `#btn-knowledge`、`#knowledge-view`、新建知识弹窗、看板抽屉「相关知识」容器、`knowledge.js` 脚本
- `web/app.js` — 视图切换隐藏 knowledge、图标白名单、看板概览调用 `renderBoardKnowledge`
- `web/style.css` — `#knowledge-*` / `.kb-*` 样式与移动端全屏详情
- `scripts/frontend-tests/package.json` — 注册新测试

## 4. API / MCP 速查

### HTTP

```bash
# 列表/搜索（登录 cookie 或 ?token=）
GET /api/knowledge?q=看板&project=AgentDeck&status=all&limit=20&offset=0
GET /api/knowledge/K-...
POST /api/knowledge           # {title, summary, body, project, tags[], source_task_id?, source_artifact_ids[]?}
PATCH /api/knowledge/K-...    # {action:"approve"|"reject"|"stale"|"restore", reason?} 或内容字段
POST /api/knowledge/collect   # 扫描看板生成 draft（admin）
```

- `GET /api/knowledge` 返回 `{entries,total,counts,projects}`；列表不返回 body。
- `GET /api/knowledge/{id}` 返回完整 entry + `source_task` + `source_artifacts`（artifact URL 已按 `public_base_url`/trusted_origins/request/listen 生成）+ evidence + relations。
- UI（无 `X-CC-Webui-Session` 头）= console user/admin；MCP 用 `X-CC-Webui-Session` 识别，未知/空 session 视为 worker（非 admin）。
- 敏感/隔离条目只对 admin 可见；worker 只看到 approved、非 sensitive 的条目。

### MCP

| 工具 | worker | 主 Agent | 说明 |
|---|---|---|---|
| `knowledge_search` | ✅ | ✅ | worker 只看 approved；支持 q/project/tag/status/source_task_id/limit/offset |
| `knowledge_get` | ✅ | ✅ | worker 只能读 approved + 自己的 draft（共享 MCP 无 session 身份时读不到自己的 draft） |
| `knowledge_add` | ✅ | ✅ | worker 必须带 `source_task_id`；强制 draft；artifact 必须属于该 task |
| `knowledge_review` | ❌ | ✅ | approve/reject/stale/restore；工具表 + 服务端两层闸门 |
| `knowledge_collect` | ❌ | ✅ | 手动采集 |

worker 的 `knowledge_add` 提示：`title` 必填，`summary`/`body` 至少一个；`source_artifact_ids` 只能引用 `source_task_id` 下的 artifact；文本命中敏感规则会被脱敏并标 `quarantined`。

## 5. 采集边界（硬规则）

- 只读：`boarddb` 的 tasks/task_events/task_artifacts 元数据；不读 artifact 字节（P0 不抽取正文）。
- 不读：任务 cwd 下任意文件、`data/events`、CLI transcript、`legacy report_file` 路径、外链内容。
- `source_paths` 只是路径标注；服务端不会按它读文件。
- 敏感扫描：私钥、Bearer、`token=/password=/secret=` 赋值、cookie、常见 token 前缀、混合大小写+数字的高熵串；命中后字段脱敏、状态 `quarantined`、不写 FTS。
- 演示数据扫描：83 个任务 → 68 条 draft、3 条 quarantine（其余跳过：进行中/无内容）。3 条隔离分别为凭据/高熵命中。

## 6. 测试实例

- 数据目录：`./data-demo`（从生产 `data/agent-board.db` 用 `sqlite3 .backup` 做的一致性快照；`data/artifacts` 复制；`db.json` 手工 seed 了两个会话：`main_agent` 的「资料空间演示」和非 main 的「Worker 演示」，用于验证 tab 显隐）。
- 监听：`0.0.0.0:8327`。
- URL：`http://xpsl.soarx.cc:8327/`（未登录 302 到 `/login.html`；用当前 `config.yaml` 的 `gateway_token` 登录；token 不进仓库/不写进本文）。
- 服务单元：`cc-webui-knowledge-demo.service`（`systemd-run --user`，transient）。

**重启**

```bash
go build -o cc-webui .
systemctl --user stop cc-webui-knowledge-demo.service
systemd-run --user --unit=cc-webui-knowledge-demo --collect \
  --working-directory="$PWD" \
  --setenv=CC_WEBUI_DATA_DIR="$PWD/data-demo" \
  --setenv=CC_WEBUI_LISTEN="0.0.0.0:8327" \
  "$PWD/cc-webui"
```

**查看日志**

```bash
journalctl --user -u cc-webui-knowledge-demo -n 100 --no-pager
```

**清理**

```bash
systemctl --user stop cc-webui-knowledge-demo.service
rm -rf data-demo
```

不要动 8321 上的生产实例。

## 7. 验证结果

- `go test ./internal/knowledge/ ./internal/server/`：通过。
- `go test ./...`：`internal/knowledge`、`server`、`a2a` 等通过；`internal/runner` 的 `TestServeKeyIsCompact` 在当前 shell 的 ambient 凭据 env（`ANTHROPIC_API_KEY`/`ANTHROPIC_AUTH_TOKEN`/`ANTHROPIC_BASE_URL`/`DUCC_AUTH_TOKEN`/`SN_API_KEY`/`SN_BASE_URL`）下会因 key 段数 >8 失败；用 `env -i HOME=$HOME PATH=$PATH ... go test ./internal/runner/ -run TestServeKeyIsCompact` 通过，属既有环境敏感测试，与本次改动无关。
- `./scripts/frontend-tests/run.sh`：全部通过（含新增 `knowledge.test.js` 8 项）。
- CDP 真浏览器（Playwright headless_shell，2026-11-10 UI 重设计后）：
  - mainAgent 会话显示「看板 | 资料空间」两个 tab，默认 active 是「对话」；非 main 会话两个 tab 都隐藏，且从资料空间切回「对话」。
  - 资料空间 tab：active 正确，列表 50 条/页；搜索「浏览器」→ 8 条命中；详情标题/来源/证据/artifact 链接正常。
  - 看板任务 `YHX-TODO-780` 抽屉「相关知识 (1)」显示 approved 条目；点「查看全部」切到资料空间 tab，任务 chip 为「来源任务 YHX-TODO-780 ×」，结果 1 条。
  - 深链 `?knowledge=<id>&theme=dark` 自动打开 mainAgent 资料空间 tab 和详情。
  - 1440/1024/768/390 × light/dark：`scrollWidth == innerWidth`（无横向溢出）；0 console error / runtime exception。
  - 新增/更新的 jsdom 共 14 项通过（tab 显隐、active、看板→知识切换、加载/空/错误态、来源任务 chip 等）。
- 演示数据：68 条 draft、3 条 quarantined；为演示手动 approve 了 6 条（通过 API `action=approve, reason=demo seed review`），不改变“自动只出 draft”的策略。

## 8. 已知取舍

- `knowledge.db` 与 `agent-board.db` 独立；详情页的 `source_task`/`source_artifacts` 由 server 在读取时补齐，没有跨库事务。
- 任务 `description` 之前不在 board snapshot 里，本次在 `boarddb.toMap` 补上；这是 P0 collect 需要的字段。
- 知识列表默认 UI 状态是「全部」（admin 视角）；MCP/worker 默认 approved。
- 敏感扫描的“高熵”规则已收紧为「32–128 字符、同时含大小写+数字、熵≥4.3」，避免把 commit SHA/任务 id 误判；仍有少量误报，P1 再做可配置/可放行。
- 当前 UI 审批动作直接走 `PATCH`，没有二次确认（驳回/过期会 prompt 原因）；批量审批、审计时间线 P1。
