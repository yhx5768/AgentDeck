---
name: agentdeck
description: 在本机的 AgentDeck（cc-webui）Web 控制台上管理会话——新建/命名会话、选 cli/provider/model/effort/是否常驻、发消息并读回模型回复、改档、停止、归档、删除、分叉、回滚、群聊编排。当用户说「在 AgentDeck 里建个会话」「把这个任务丢到前端去跑」「给某个会话发消息/看它跑到哪了」「AgentDeck 上有哪些会话/模型」，或需要把工作交给另一个可见的会话时使用。
---

# AgentDeck Skill

AgentDeck（内部标识 `cc-webui`）是本机的一个 Web 控制台，把 claude / opencode / mcode / qoder /
codex 五个编码 CLI 统一成浏览器里的会话列表。本 skill 让**任意 Agent** 通过它的 HTTP API
直接建会话、发消息、读结果——建出来的会话会出现在前端侧栏里，用户能实时看到。

配套脚本：`scripts/agentdeck.py`（只用 Python 3 标准库，无需安装依赖）。
下文命令里的路径都相对于本 skill 所在目录。

```bash
AD="python3 <skill-dir>/scripts/agentdeck.py"
$AD doctor        # 先确认能连上、token 对不对
```

## 30 秒上手

```bash
# 1) 看看有什么引擎/后端/模型可用
$AD clis
$AD providers --cli opencode
$AD models --cli opencode --provider cline-pass      # 动态目录，别写死模型名

# 2) 建一个会话（会立刻出现在前端侧栏，5s 内）
$AD new --name "修 login 的 500" --cli opencode --provider cline-pass \
        --model cline-pass/deepseek-v4.1-flash --effort low \
        --cwd ~/Projects/my-app
# stdout 是 session id，可直接存下来

# 3) 发消息并等它跑完，stdout 只输出模型回复
$AD reply <session-id> "看一下 src/auth.ts 里为什么会 500"

# 或者一步到位：建会话 + 发首条消息 + 等结果
$AD run --name "查 500" --cli claude --cwd ~/Projects/my-app \
        "排查 src/auth.ts 的 500，给结论不要改代码"
```

`run` / `reply` 的 **stdout 只有模型最后一段回复文本**（会话 id、进度都走 stderr），
可以直接当返回值用；加 `--json` 则输出 `{session_id, state, error, text}`。

## 连接信息从哪来

优先级：`--url`/`--token` 参数 → `AGENTDECK_URL`/`AGENTDECK_TOKEN` 环境变量 →
`config.yaml` 的 `listen` + `gateway_token` → 兜底 `http://127.0.0.1:8321`。

`config.yaml` 的查找顺序：`$AGENTDECK_CONFIG` → 当前目录 → `~/Projects/cc-webui/` →
`~/cc-webui/` → `~/.config/cc-webui/`。token 也可以放在
`~/.config/cc-webui/agentdeck.token`（config 里没写 token 时用）。

连不上或 401 时先跑 `$AD doctor`，它会打印实际解析到的 base_url / token / config 路径。

## 命令全集

所有命令都接受 `--json`（原样打印服务端 JSON）。`$AD <command> --help` 看细节。

### 探查

| 命令 | 说明 |
|---|---|
| `doctor` | 打印解析到的连接信息 + 连通性检查 |
| `clis` | 列出 cli（name / kind / path / 是否配了 models_command） |
| `providers [--cli C]` | 列出 provider；给了 `--cli` 就只列同引擎的 |
| `models --cli C [--provider P] [--fresh]` | 该后端可用的模型（**按引擎自动分派**，见下） |
| `efforts --cli C [--provider P] [--model M]` | 该模型的推理档位 |
| `profiles` / `config` | 原始配置视图 |

**模型清单是动态的，不要凭记忆写模型名。** claude 从 `config.yaml` 读；
opencode / mcode / qoder / codex 由各自 CLI 在运行时给出，`--fresh` 才强制刷新。
`models` 的输出里 `efforts` 列就是该模型支持的档位。

### 会话

| 命令 | 说明 |
|---|---|
| `ls [--archived]` | 列出会话（含状态：running / queued / resident / error） |
| `new --cwd P [--name N] [--cli C] [--provider P] [--model M] [--effort E] [--permission-mode PM] [--resident] [--peer-name NAME]` | 建会话，stdout 打印 session id |
| `get ID` | 会话详情（含 `resume_command`、goal、resident 状态） |
| `set ID [--name/--cli/--provider/--model/--effort/--permission-mode/--cwd/--peer-name/--archived/--resident/--unread/--todo/--sort-order]` | 只改显式给出的字段 |
| `send ID TEXT` | 发消息（不等结果） |
| `reply ID TEXT [--timeout S]` | 发消息并等本轮结束，stdout 输出回复 |
| `run ... TEXT` | 建会话 + 发首条消息 + 等结果 |
| `wait ID [--timeout S]` | 等会话空闲 |
| `tail ID [--timeout S] [--follow]` | 跟随 SSE 事件流（默认本轮结束即退出） |
| `stop ID` | 停止当前轮次 |
| `goal ID TEXT [--max-turns N] [--max-hours H]` / `ungoal ID` | 自动续跑目标 |
| `fork ID [--name N]` | 分叉（claude / qoder） |
| `rewind ID --checkpoint UUID` | 回滚（claude / opencode） |
| `pending ID` / `answer ID PID --behavior allow\|deny [--message M]` | 看待回答的提问/审批卡并回复 |
| `rm ID` | 删除 |
| `upload ID PATH` | 上传附件，打印落盘路径 |

`send` / `reply` / `run` 还可以带 `--file PATH`（作为附件上传）和 `--image PATH`
（内嵌图片，**mcode / codex 不支持**，会被服务端 400）。文本可以用位置参数、
`--text`，或 `-` 从 stdin 读。

### 导入历史会话

```bash
$AD history --engine claude --prefix 5c1b          # 列出本机历史会话
$AD import --session-id <id> --cwd ~/Projects/app  # 导入
```
`--engine` 取 `claude|opencode|mcode|qoder|codex`；opencode/mcode/codex 需要 `--cli`
（不给就自动挑该 kind 的第一个 cli）。**mcode 的 `--cwd` 是必填**（它不记录工作区）。

### 群聊（多 agent 并行编排）

```bash
$AD group-new --name "评审团" --cwd ~/Projects/app \
     --member "小王:opencode:cline-pass:cline-pass/glm-5.3:high" \
     --member "小李:claude:sensenova:deepseek-v4-flash:medium"
$AD group-send <gid> "@小王 看下 diff"     # @成员 定向；@all 或空群发则全员并行
$AD group-tail <gid> --timeout 300
$AD group-get <gid>                       # 成员 id 在这里；改档用 group-set <gid> <member_id>
```
成员格式 `name[:cli[:provider[:model[:effort]]]]`，1–8 个。`group-set` 的改动**下一轮**生效。

### 其它

`groups` / `group-get` / `group-set` / `group-send` / `group-stop` / `group-rm` / `group-tail`、
`notes` / `notes-set`、`stats [--fresh]`，以及逃生舱
`raw METHOD PATH [--data JSON]`（直连任意接口，比如 `raw GET /api/sessions`）。

## 引擎差异（会踩的坑）

- **会话绑定单一引擎**：cli 与 provider 的 `kind` 必须一致（`claude`/`opencode`/`mcode`/`qoder`/`codex`），
  不一致服务端直接 400 `engine mismatch`。`providers --cli X` 已经按引擎过滤过。
- **档位语义随引擎变**：claude 是 `--effort`（low/medium/high/xhigh/max）；qoder 是
  `--reasoning-effort`（auto/none/low/…/ultracode）；opencode 是 `--variant`，**每个模型的键都不同**
  （有的只有 `max`，有的 `none/thinking`，有的干脆没有）；mcode/codex 的档位也逐模型不同。
  先 `$AD efforts --cli C --provider P --model M` 问，别猜。留空 = 继承配置。
- **常驻 / peer 消息（`--resident` / `--peer-name`）只有 claude 支持**，别的引擎 400。
- **内嵌图片**只有 claude / qoder / opencode 支持；mcode / codex 只能走 `--file` 附件。
- **fork** 只有 claude / qoder；**rewind** 只有 claude / opencode。
- 改 `--cli`/`--provider` 而没给新 `--model` 时，旧 model 会退回新 provider 的默认模型。

## 典型工作流

**把一件事交给一个可见的新会话**（用户能在前端看到进度）：

```bash
$AD run --name "重跑前端测试" --cwd ~/Projects/cc-webui --cli claude \
        "跑 ./scripts/frontend-tests/run.sh，失败就把失败用例和原因列出来"
```

**给已有会话追加任务并读回结果**：先 `ls` 找到 id，再 `$AD reply <id> "..."`。
若会话正忙，`reply` 会先等它空闲（可用 `--queue` 直接排队）。

**长时间任务**：`send` 投递后用 `wait`（只看状态）或 `tail`（看事件流）；
要「干到达成条件为止」就配 `goal`。

**模型提问/审批卡**：模型提问有 55s 硬超时，`pending ID` 能看到待回答的卡，
用 `answer ID <pid> --behavior allow`（或 `deny --message 理由`）回复。

## 注意

- 建会话是**真实副作用**：会出现在用户的前端侧栏里、会真的起进程跑模型。别建垃圾会话，
  用完就 `rm`。名字写清楚用途，用户一眼能认出是谁建的。
- 删除会话会连带删掉它的 cc-webui 事件日志（引擎自己的 transcript 另存，不一定一起删）。
- 所有接口都要 token（`Authorization: Bearer`）。脚本自己处理，不要把它打印到日志里。
