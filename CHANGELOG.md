# Changelog

AgentDeck 的版本变更。发布说明以 annotated tag 的说明为准；完整 diff 链接附在各版本 Release 末尾。

## v0.5.1 — 2026-11-13

### 变更

- 主 Agent 出站 A2A 发送（含单条）默认折叠为「N 条 A2A 发送」，组头保留收件人、`send_id`、预览与失败标记。
- codex/ducx 的 agentdeck MCP 注入不再依赖 browser 开关；默认配置下会话工具与 A2A 工具可用。

### 修复与测试

- 补充 A2A 发送折叠的前端 jsdom 回归，以及 codex/ducx MCP 注入组合的 runner 测试。
- 前端全量测试与 `go vet ./...` 通过；`go test ./...` 仅保留既有 `TestServeKeyIsCompact` 基线失败。

## v0.5.0 — 2026-11-13

### 变更

- Personal Agent 正式发布：以主 Agent 为中心的工作台，包含「需要你」队列、主 Agent 会话、看板和右侧卡片详情。
- 多 CLI 协作与引擎中立 A2A：agentdeck MCP 注入 claude / qoder / mcode / codex / opencode 等引擎，会话间可跨 CLI 通信。
- README 重写：围绕 Personal Agent 与多 CLI A2A 重新组织安装、配置和使用说明。

## 发布流程

1. 打 annotated tag，并在 tag 说明里写版本要点：
   `git tag -a vX.Y.Z -m "vX.Y.Z" -m "- 变更一\n- 变更二"`
2. 推送 tag：`git push origin vX.Y.Z`
3. GitHub Actions 的 `release` workflow 优先把 tag 说明作为 Release body；tag 没有说明时回退到 `--generate-notes`。
4. 需要完整 diff 时，在 tag 说明末尾附 compare 链接：
   `**完整 diff**：https://github.com/yhx5768/AgentDeck/compare/vPREV...vX.Y.Z`
