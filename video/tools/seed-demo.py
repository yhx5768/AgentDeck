#!/usr/bin/env python3
"""Seed a self-contained AgentDeck demo dataset for the intro video.

Writes data-video/{db.json,events/*.jsonl,groups/*} directly, then the demo
server (video/demo-config.yaml) replays it. Two reasons for seeding instead of
driving real models:

  1. privacy  - the screenshots must not leak the real conversations sitting in
                the dogfooding instance on :8321;
  2. determinism - the film has to render the same frames every time, so the
                chat content, tool cards and diffs are pinned here.

Events are written in the canonical shapes the frontend already renders
(see internal/claudehist.ConvertIn for the producer side). Every demo session
has an EMPTY claude_session_id on purpose: handleEvents only replays stored
events verbatim when there is no external transcript to merge, which is exactly
what we want for a fixture.
"""

import json
import pathlib
import shutil

ROOT = pathlib.Path(__file__).resolve().parents[2]
DATA = ROOT / "data-video"
EVENTS = DATA / "events"
GROUPS = DATA / "groups"
CWD = str(ROOT)

# Base timestamp: "just now" at seed time, so the sidebar reads 刚刚/N 分钟前
# no matter when the film is rendered. Ascending by 20s per event so the replay
# ordering is stable.
BASE_MS = int(__import__("time").time() * 1000) - 25 * 60 * 1000
T = [BASE_MS]


def ts():
    T[0] += 20_000
    return T[0]


def iso(ms):
    import datetime

    return datetime.datetime.fromtimestamp(
        ms / 1000, datetime.timezone.utc
    ).isoformat().replace("+00:00", "Z")


# ---------------------------------------------------------------- event shapes


def user(text, checkpoint=None):
    d = {"text": text}
    if checkpoint:
        d["checkpoint"] = checkpoint
    return {"kind": "user", "data": d}


def status(state, **kw):
    return {"kind": "status", "data": {"state": state, **kw}}


def asst(*blocks, msg_id=None, model=None, usage=None):
    msg = {"role": "assistant", "content": list(blocks)}
    if msg_id:
        msg["id"] = msg_id
    if model:
        msg["model"] = model
    if usage:
        msg["usage"] = usage
    return {"kind": "stream", "data": {"type": "assistant", "message": msg}}


def text(s):
    return {"type": "text", "text": s}


def thinking(s):
    return {"type": "thinking", "thinking": s}


def tool(name, tid, **input):
    return {"type": "tool_use", "id": tid, "name": name, "input": input}


def tool_result(tid, body, is_error=False):
    block = {"type": "tool_result", "tool_use_id": tid, "content": [{"type": "text", "text": body}]}
    if is_error:
        block["is_error"] = True
    return {
        "kind": "stream",
        "data": {"type": "user", "message": {"role": "user", "content": [block]}},
    }


def result(subtype="success", duration_ms=41_800, cost=0.0831, usage=None):
    return {
        "kind": "stream",
        "data": {
            "type": "result",
            "subtype": subtype,
            "duration_ms": duration_ms,
            "total_cost_usd": cost,
            "usage": usage or {"input_tokens": 12_884, "output_tokens": 2_106},
        },
    }


def rewind(detail):
    return {"kind": "rewind", "data": {"detail": detail}}


# ---------------------------------------------------------------- hero session

HERO_CLI, HERO_PROV = "claude", "ducc-models"
HERO_MODEL = "deepseek-v4.1-flash"

HERO = [
    user(
        "工作区面板的文件树是懒加载的，搜索却只筛已经拉下来的节点——深层文件一条都搜不到。"
        "帮我改成服务端整树搜索，顺带把 .gitignore 忽略的文件标出来。",
        checkpoint="ck-a41f7d20-9c3e-4b81-8a5e-6d2f0b7c1e33",
    ),
    status("turn_start"),
    asst(
        thinking(
            "用户说的是 web/workspace.js 里的筛选。文件树一层一层懒加载，"
            "没展开过的目录根本不在 wsState.dirs 里，所以前端筛 entry.name 必然漏。"
            "正确做法是把查询词交给服务端，递归扫当前会话工作区，"
            "名字和完整相对路径都参与匹配；查询里带 / 时先定位存在的目录前缀、"
            "只扫那棵子树，否则全树扫。忽略名单只影响展示，gitignore 的文件照常列出、"
            "打一个标记就行——面板不是 git，藏起来反而让人以为文件没了。"
        ),
        text(
            "先定位现有实现。问题基本可以确定：筛选跑在**已加载**的树节点上，"
            "而文件树是懒加载的，没展开过的深层文件根本不在内存里。"
        ),
        msg_id="msg_demo_01",
        model=HERO_MODEL,
    ),
    asst(
        tool("Bash", "toolu_demo_bash_1", command="grep -n 'wsFilter\\|wsRenderTree\\|wsSearchNow' web/workspace.js", description="定位筛选实现"),
        msg_id="msg_demo_02",
        model=HERO_MODEL,
    ),
    tool_result(
        "toolu_demo_bash_1",
        "812:function wsRenderTree() {\n"
        "836:function wsFilterTree(q) {\n"
        "841:  const hit = (e) => e.name.toLowerCase().includes(q);\n"
        "862:function wsSearchNow() {\n"
        "866:  wsFilterTree(q);\n"
        "871:  // 只筛已加载节点：未展开的目录里的文件匹配不到\n",
    ),
    asst(
        text(
            "确认了。`wsFilterTree` 只遍历 `wsState.dirs` 里已经拉下来的节点，"
            "`/workspace/search` 这个接口当时根本没建。改法分两步："
            "\n\n1. 服务端加一条整树递归搜索（名字 + 完整相对路径都参与匹配，"
            "查询带 `/` 时先解析目录前缀、只扫那棵子树）；"
            "\n2. 前端换成 300ms 防抖 + `AbortController`，换词即取消上一次请求。"
        ),
        msg_id="msg_demo_03",
        model=HERO_MODEL,
    ),
    asst(
        tool(
            "TodoWrite",
            "toolu_demo_todo_1",
            todos=[
                {"content": "服务端加 /workspace/search 整树递归搜索", "status": "completed", "activeForm": "实现服务端整树搜索"},
                {"content": "查询带 / 时先定位目录前缀，只扫子树", "status": "completed", "activeForm": "优化子树定位"},
                {"content": "前端换成防抖 + AbortController", "status": "in_progress", "activeForm": "改造前端筛选"},
                {"content": "gitignore 文件照常列出，只打标记", "status": "pending", "activeForm": "标记被忽略的文件"},
                {"content": "补 jsdom 契约测试并跑通", "status": "pending", "activeForm": "补前端测试"},
            ],
        ),
        msg_id="msg_demo_04",
        model=HERO_MODEL,
    ),
    asst(
        tool(
            "Edit",
            "toolu_demo_edit_1",
            file_path="web/workspace.js",
            old_string=(
                "function wsSearchNow() {\n"
                "  const q = $('#ws-filter').value.trim().toLowerCase();\n"
                "  if (!q) { wsRenderTree(); return; }\n"
                "  wsFilterTree(q);\n"
                "}"
            ),
            new_string=(
                "function wsSearchNow() {\n"
                "  const q = $('#ws-filter').value.trim();\n"
                "  if (!q) { wsState.searchHits = null; wsRenderTree(); return; }\n"
                "  clearTimeout(wsState.searchTimer);\n"
                "  wsState.searchTimer = setTimeout(async () => {\n"
                "    wsState.searchCtl?.abort();\n"
                "    wsState.searchCtl = new AbortController();\n"
                "    const r = await fetch(`/api/sessions/${state.current.id}/workspace/search`\n"
                "      + `?q=${encodeURIComponent(q)}`, { signal: wsState.searchCtl.signal });\n"
                "    wsState.searchHits = (await r.json()).entries || [];\n"
                "    wsRenderSearch();\n"
                "  }, 300);\n"
                "}"
            ),
        ),
        msg_id="msg_demo_05",
        model=HERO_MODEL,
    ),
    tool_result("toolu_demo_edit_1", "Edited web/workspace.js (+13 -4)"),
    asst(
        tool("Bash", "toolu_demo_bash_2", command="./scripts/frontend-tests/run.sh", description="跑前端契约测试"),
        msg_id="msg_demo_06",
        model=HERO_MODEL,
    ),
    tool_result(
        "toolu_demo_bash_2",
        "▶ workspace_panel.test.js\n"
        "  ✓ 树行带 data-path\n"
        "  ✓ 点目录会展开（目录节点在第一次展开时才创建）\n"
        "  ✓ 筛选走服务端整树搜索，不是筛已加载节点\n"
        "  ✓ gitignore 的文件照常列出并标记\n"
        "  ✓ 只看改动\n"
        "  ✓ diff 行分类\n"
        "\n"
        "6 passing (1.4s)",
    ),
    asst(
        text(
            "改完了，测试全绿。\n\n"
            "关键差异在这里——旧实现筛的是 `entry.name`，新实现由服务端整树递归：\n\n"
            "```js\n"
            "// 查询带 / 时先解析目录前缀，只扫那棵子树（66 万项的全树扫要 1.7s，\n"
            "// 定位前缀只要 10ms），裸文件名才全树扫，上限 100 万项 / 6s。\n"
            "const prefix = q.includes('/') ? wsResolve(q) : null;\n"
            "const hits = prefix ? walk(prefix, q) : walk(sess.Cwd, q);\n"
            "```\n\n"
            "被 `.gitignore` 忽略的文件**不再隐藏**了——照常列出、照常搜到，"
            "只在行尾打一个「git 忽略」标记，判断走一条 `git check-ignore --stdin -z` 批量查询。"
        ),
        msg_id="msg_demo_07",
        model=HERO_MODEL,
    ),
    result(),
    status("turn_end"),
    user("再帮我确认一下路径边界：`..` 段和符号链接逃逸都挡住了吗？"),
    status("turn_start"),
    asst(
        text(
            "三层都挡住了，而且这套口径比 `/api/files` 更严：\n\n"
            "- `..` 段直接 403（`filepath.Clean(\"/../x\")` 会把 `..` 吃掉，"
            "那只是 Clean 的副作用、不是授权语义）；\n"
            "- 绝对路径 403；\n"
            "- 解析符号链接**之后**再比对 cwd 前缀，防 symlink 逃逸。\n\n"
            "面板锚在**当前会话**的 cwd 上，所以一个会话看不到别的项目的目录树。"
        ),
        msg_id="msg_demo_08",
        model=HERO_MODEL,
    ),
    result(subtype="success", duration_ms=9_200, cost=0.0142, usage={"input_tokens": 18_402, "output_tokens": 618}),
    status("turn_end"),
]

# ------------------------------------------------------------ other sessions

SESSIONS = [
    {
        "id": "demo-hero-0001",
        "name": "工作区面板：服务端整树搜索",
        "cli": HERO_CLI,
        "provider": HERO_PROV,
        "model_override": HERO_MODEL,
        "effort_override": "high",
        "permission_mode": "default",
        "events": HERO,
        "unread": True,
    },
    {
        "id": "demo-oc-0002",
        "name": "重构导出层：一次拿全项目",
        "cli": "opencode",
        "provider": "opencode-go",
        "model_override": "glm-5.3",
        "events": [
            user("session list 只回当前目录所属的桶，统计要跨项目枚举，怎么办？"),
            status("turn_start"),
            asst(
                thinking("opencode 没有列全部桶的 CLI 面。但有一条只读 db 查询可以一次拿全……"),
                text(
                    "opencode 没有「列出全部桶」的 CLI 面，`session list` 只回进程 cwd 所属的桶。"
                    "所以枚举走一条**只读** `opencode db` SELECT，`WHERE parent_id IS NULL` 对齐"
                    "`session list` 的语义（它隐藏 subagent 子会话）。实测查询前后 db / -wal / -shm"
                    "三个文件的 md5、inode、size、mtime_ns 完全一致——只读不写。"
                ),
                msg_id="msg_oc_01",
                model="glm-5.3",
            ),
            asst(
                tool("Bash", "toolu_oc_bash_1", command="opencode db \"SELECT id,title,directory FROM session WHERE parent_id IS NULL\" --format json", description="枚举全部项目"),
                msg_id="msg_oc_02",
                model="glm-5.3",
            ),
            tool_result(
                "toolu_oc_bash_1",
                '[\n  {"id":"ses_0f31","title":"重构导出层","directory":"/home/soar/Projects/agentdeck"},\n'
                '  {"id":"ses_7a02","title":"openapi 对齐","directory":"/home/soar/Projects/gateway"},\n'
                '  {"id":"ses_bb19","title":"前端渲染契约","directory":"/home/soar/Projects/agentdeck"}\n]\n\n30 rows · 0.94s',
            ),
            asst(
                text("拿到了，30 个会话一次全回来，1 秒。db 查询失败时退回按桶 `session list` 扫，两条路按 session id 去重。"),
                msg_id="msg_oc_03",
                model="glm-5.3",
            ),
            result(cost=0.031, usage={"input_tokens": 9_120, "output_tokens": 884}),
            status("turn_end"),
        ],
    },
    {
        "id": "demo-mcode-0003",
        "name": "长任务：compaction 快照排查",
        "cli": "mcode",
        "provider": "mcode-minimax",
        "model_override": "MiniMax-M3.1-Flash-Preview",
        "events": [
            user("单个会话快照 17.8MB，跑着跑着就 OOM 了，看看堆上限。"),
            status("turn_start"),
            asst(
                thinking("node 默认旧堆上限约 2GB，compaction 快照要把整份历史解析进内存……"),
                text(
                    "这是 node 的堆上限撞墙了：compaction 快照会把整份历史解析进内存，"
                    "顶到默认旧堆（~2GB）就 `FATAL ERROR: JavaScript heap out of memory`。\n\n"
                    "缓解办法是在 cli 的 env 里注入 `NODE_OPTIONS=--max-old-space-size=4096`"
                    "——本机 8GB 物理内存，**别设 8GB**，那会让 node 跟系统抢内存、OOM 之前先整机卡死。"
                ),
                msg_id="msg_mc_01",
                model="MiniMax-M3.1-Flash-Preview",
            ),
            asst(
                tool("Bash", "toolu_mc_bash_1", command="node -e \"console.log(require('v8').getHeapStatistics().heap_size_limit/1048576|0,'MB')\"", description="查看堆上限"),
                msg_id="msg_mc_02",
                model="MiniMax-M3.1-Flash-Preview",
            ),
            tool_result("toolu_mc_bash_1", "2048 MB"),
            asst(text("确认是 2048 MB。注入之后是 4096 MB，而且 env 在 spawn 时固定——改配置对正在跑的那一轮无效，下一轮起生效。"),
                msg_id="msg_mc_03", model="MiniMax-M3.1-Flash-Preview"),
            result(cost=0.021, usage={"input_tokens": 7_640, "output_tokens": 512}),
            status("turn_end"),
        ],
    },
    {
        "id": "demo-qoder-0004",
        "name": "Qoder：档位参数对齐",
        "cli": "qoder",
        "provider": "qoder-cn",
        "model_override": "Qwen3.8-Flash",
        "events": [
            user("档位 flag 是不是不叫 --effort？"),
            status("turn_start"),
            asst(
                text(
                    "对，Qoder 是 Claude Code 的 fork，但 argv 有差异——多一个 flag 就整轮 exit 1：\n\n"
                    "- **没有 `--verbose`**（报 `unknown option '--verbose'`）；\n"
                    "- 档位 flag 是 `--reasoning-effort`，取值 `auto/none/low/medium/high/xhigh/max/ultracode`；\n"
                    "- `--permission-mode` 收 **snake_case**（`bypassPermissions` → `bypass_permissions`）。\n\n"
                    "另外它的 `usage` 全是 0，计费走 `credits`/`billable`——所以 token 类统计对它无意义。"
                ),
                msg_id="msg_qd_01",
                model="Qwen3.8-Flash",
            ),
            result(cost=0.0, usage={"input_tokens": 0, "output_tokens": 0}),
            status("turn_end"),
        ],
    },
    {
        "id": "demo-codex-0005",
        "name": "Codex rollout：模型目录",
        "cli": "ducx",
        "provider": "codex-ducx",
        "model_override": "GLM-5.3",
        "events": [
            user("ducx 的 debug models --bundled 里怎么没有网关模型？"),
            status("turn_start"),
            asst(
                text(
                    "因为它是 launcher wrapper。`ducx debug models --bundled` 只有上游 gpt 模型，"
                    "网关实际可用的 GLM-5.3 / DeepSeek-V4 全都不在里面。\n\n"
                    "wrapper 把真实清单落在 `<home>/model-catalogs/<model_provider>-models.json`"
                    "（`ducx models` 打印的就是它），并与 bundled 同形——slug / display_name /"
                    "supported_reasoning_levels / input_modalities / context_window 都在。"
                ),
                msg_id="msg_cx_01",
                model="GLM-5.3",
            ),
            result(cost=0.004, usage={"input_tokens": 4_210, "output_tokens": 366}),
            status("turn_end"),
        ],
    },
    {
        # One end of the agent2agent exchange: the receiving session. Its address
        # name is docs-site; arch-review sends to it and it replies back.
        "id": "demo-docs-0006",
        "name": "文档站改版",
        "cli": HERO_CLI,
        "provider": HERO_PROV,
        "model_override": "glm-5.3",
        "resident": True,
        "peer_name": "docs-site",
        "events": [
            user("README 的表格太长了，拆成两节吧。"),
            status("turn_start"),
            asst(text("好，我把「核心特性」拆成「引擎与模型」「会话能力」两节，并把特性表压到 8 行以内。"),
                msg_id="msg_doc_01", model="glm-5.3"),
            {
                "kind": "stream",
                "data": {
                    "type": "user",
                    "uuid": "peer-0001",
                    "origin": {
                        "kind": "peer",
                        "name": "arch-review",
                        "body": "架构评审的结论：工作区面板锚在**当前会话**的 cwd 上，不要放宽成「任一会话的 cwd」；"
                                "`..` 段直接 403，别依赖 filepath.Clean 把 .. 吃掉那个副作用。README 里那条权限说明顺手改一下。",
                    },
                    "message": {"role": "user", "content": [{"type": "text", "text": "…"}]},
                },
            },
            asst(
                text("收到 arch-review 的结论，我按这个口径把 README 的权限说明改掉，并在表格上方补一句「面板只认当前会话工作区」。"),
                msg_id="msg_doc_02",
                model="glm-5.3",
            ),
            asst(
                tool(
                    "SendMessage",
                    "toolu_peer_send_2",
                    to="arch-review",
                    message="改好了。README 权限说明已按「锚在会话 cwd、.. 段直接 403」的口径更新，表格也收窄了。",
                ),
                msg_id="msg_doc_03",
                model="glm-5.3",
            ),
            tool_result("toolu_peer_send_2", "已投递：arch-review 将在其下一个工具轮次处理"),
            result(cost=0.006, usage={"input_tokens": 5_120, "output_tokens": 402}),
            status("turn_end"),
        ],
    },
    {
        # The other end: the sending session. Lists live peers, then hands the
        # conclusion to docs-site by name.
        "id": "demo-a2a-0010",
        "name": "跨会话消息：把结论投递给文档站",
        "cli": HERO_CLI,
        "provider": HERO_PROV,
        "model_override": "deepseek-v4.1-flash",
        "resident": True,
        "peer_name": "arch-review",
        "events": [
            user("看看现在有哪些会话在线，把刚才的结论发给文档站那个会话。"),
            status("turn_start"),
            asst(
                thinking("要先知道谁在线。ListAgents 列出来的才是可寻址的 live 会话，名字就是地址。"),
                text("先列一下在线的会话，确认文档站那边开着。"),
                msg_id="msg_a2a_01",
                model="deepseek-v4.1-flash",
            ),
            asst(tool("ListAgents", "toolu_a2a_list", **{}), msg_id="msg_a2a_02", model="deepseek-v4.1-flash"),
            tool_result(
                "toolu_a2a_list",
                "\n".join([
                    "在线会话（name 即地址）：",
                    "  arch-review    [3fa9c1]  idle   本会话",
                    "  docs-site      [7b2e40]  idle   ~/Projects/cc-webui",
                    "  frontend-pair  [c18a52]  busy   ~/Projects/agentdeck-demo",
                    "  stats-refactor [9d0f13]  idle   ~/Projects/cc-webui",
                ]),
            ),
            asst(
                text("文档站在线，地址名 `docs-site`。把结论发过去——投递是异步的，对方在下一个工具轮次处理。"),
                msg_id="msg_a2a_03",
                model="deepseek-v4.1-flash",
            ),
            asst(
                tool(
                    "SendMessage",
                    "toolu_peer_send_1",
                    to="docs-site",
                    message="架构评审的结论：工作区面板锚在**当前会话**的 cwd 上，不要放宽成「任一会话的 cwd」；"
                            "`..` 段直接 403，别依赖 filepath.Clean 把 .. 吃掉那个副作用。README 里那条权限说明顺手改一下。",
                ),
                msg_id="msg_a2a_04",
                model="deepseek-v4.1-flash",
            ),
            tool_result("toolu_peer_send_1", "已投递：docs-site 将在其下一个工具轮次处理"),
            asst(
                text("发出去了。对方回消息走的是同一条通道，会作为一条新输入进我的对话流——不用轮询，也不用共享剪贴板。"),
                msg_id="msg_a2a_05",
                model="deepseek-v4.1-flash",
            ),
            {
                "kind": "stream",
                "data": {
                    "type": "user",
                    "uuid": "peer-0002",
                    "origin": {
                        "kind": "peer",
                        "name": "docs-site",
                        "body": "改好了。README 权限说明已按「锚在会话 cwd、.. 段直接 403」的口径更新，表格也收窄了。",
                    },
                    "message": {"role": "user", "content": [{"type": "text", "text": "…"}]},
                },
            },
            asst(text("docs-site 回复了，改动已落地。这条来回就是两个会话之间的完整一次对话。"),
                msg_id="msg_a2a_06", model="deepseek-v4.1-flash"),
            result(cost=0.012, usage={"input_tokens": 6_240, "output_tokens": 618}),
            status("turn_end"),
        ],
    },
    {
        "id": "demo-stats-0007",
        "name": "统计口径：两侧计数对齐",
        "cli": HERO_CLI,
        "provider": HERO_PROV,
        "model_override": "deepseek-v4.1-flash",
        "todo": True,
        "events": [
            user("claude 和 opencode 的计数口径再对一遍。"),
            status("turn_start"),
            asst(
                text(
                    "口径是：user prompt 与 assistant 步各计一条消息，工具调用按 callID 去重计调用，"
                    "usage 每步计一次。改口径要两侧同步，所以跑 `go test ./internal/claudehist/ ./internal/opencodehist/`。\n\n"
                    "实测 27 个真实 opencode 会话的分步累加与它自己的会话级 `info.tokens` 完全相等。"
                ),
                msg_id="msg_st_01",
                model="deepseek-v4.1-flash",
            ),
            result(cost=0.011, usage={"input_tokens": 6_880, "output_tokens": 470}),
            status("turn_end"),
        ],
    },
    {
        # This one lives in the separate fixture project (video/tools/mk-demo-project.sh)
        # so the workspace panel has real uncommitted changes to render.
        "id": "demo-ws-0009",
        "name": "网关：路由改成最长前缀匹配",
        "cli": HERO_CLI,
        "provider": HERO_PROV,
        "model_override": "deepseek-v4.1-flash",
        "cwd": "/home/soar/Projects/agentdeck-demo",
        "events": [
            user("路由现在按注册顺序首个匹配，/v1/chat/stream 会被 /v1/chat 抢走。改成最长前缀匹配。"),
            status("turn_start"),
            asst(
                thinking("first-match 让行为依赖 Add() 的调用顺序，这是隐患。改成取最长前缀……"),
                text("问题在 `ServeHTTP` 的 first-match 循环：行为取决于 `Add()` 的调用顺序。改成取最长匹配前缀，顺带补一条测试。"),
                msg_id="msg_ws_01",
                model="deepseek-v4.1-flash",
            ),
            asst(
                tool(
                    "Edit",
                    "toolu_ws_edit_1",
                    file_path="internal/router/router.go",
                    old_string=(
                        "func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {\n"
                        "\tfor _, rt := range r.routes {\n"
                        "\t\tif strings.HasPrefix(req.URL.Path, rt.prefix) {"
                    ),
                    new_string=(
                        "// match picks the longest matching prefix, so /v1/chat/stream wins over\n"
                        "// /v1/chat regardless of registration order.\n"
                        "func (r *Router) match(path string) (route, bool) {\n"
                        "\tvar best route\n"
                        "\tfound := false\n"
                        "\tfor _, rt := range r.routes {\n"
                        "\t\tif !strings.HasPrefix(path, rt.prefix) {\n"
                        "\t\t\tcontinue\n"
                        "\t\t}\n"
                        "\t\tif !found || len(rt.prefix) > len(best.prefix) {\n"
                        "\t\t\tbest, found = rt, true\n"
                        "\t\t}\n"
                        "\t}\n"
                        "\treturn best, found\n"
                        "}"
                    ),
                ),
                msg_id="msg_ws_02",
                model="deepseek-v4.1-flash",
            ),
            tool_result("toolu_ws_edit_1", "Edited internal/router/router.go (+19 -6)"),
            asst(
                tool("Write", "toolu_ws_write_1", file_path="internal/router/router_test.go",
                     content="package router\n\nfunc TestMatchPrefersLongestPrefix(t *testing.T) { ... }"),
                msg_id="msg_ws_03",
                model="deepseek-v4.1-flash",
            ),
            tool_result("toolu_ws_write_1", "Created internal/router/router_test.go (+16)"),
            asst(text("改完了：`match()` 取最长前缀，新增 `router_test.go` 覆盖两种情形。面板右侧可以看到 `router.go` 的改动和这个新文件。"),
                msg_id="msg_ws_04", model="deepseek-v4.1-flash"),
            result(cost=0.019, usage={"input_tokens": 8_240, "output_tokens": 720}),
            status("turn_end"),
        ],
    },
    {
        "id": "demo-rewind-0008",
        "name": "侧栏排序：按最近更新",
        "cli": HERO_CLI,
        "provider": HERO_PROV,
        "model_override": "deepseek-v4.1-flash",
        "events": [
            user("把 session 列表改成按名字排序。", checkpoint="ck-7b21c9aa-4d0e-4f2a-9c11-2a8b6e4d0f77"),
            status("turn_start"),
            asst(text("改成按 Name 字典序了，`sort.Slice` 放在 `List()` 里。"), msg_id="msg_rw_01", model="deepseek-v4.1-flash"),
            result(cost=0.005, usage={"input_tokens": 3_100, "output_tokens": 180}),
            status("turn_end"),
            rewind("已回滚到「把 session 列表改成按名字排序。」之前"),
            user("不对，我要的是按最近更新排序——别动 List()。", checkpoint="ck-9d40e1bb-6a77-4c35-8f02-71c5b93e2d18"),
            status("turn_start"),
            asst(text("明白，`List()` 保持按 UpdatedAt 排序不动，我只调前端侧栏的渲染顺序。"),
                msg_id="msg_rw_02", model="deepseek-v4.1-flash"),
            result(cost=0.004, usage={"input_tokens": 3_640, "output_tokens": 210}),
            status("turn_end"),
        ],
    },
]

GROUPS_SEED = [
    {
        "id": "demo-group-0001",
        "name": "架构评审小组",
        "members": [
            {"name": "架构", "cli": HERO_CLI, "provider": HERO_PROV, "model": "deepseek-v4-pro"},
            {"name": "前端", "cli": "opencode", "provider": "opencode-go", "model": "glm-5.3"},
            {"name": "评审", "cli": "mcode", "provider": "mcode-minimax", "model": "MiniMax-M3.1-Flash-Preview"},
        ],
        "log": [
            ("user", None, None, "@all 工作区面板要开放「任一会话的 cwd」给 /api/files 吗？"),
            ("member", "架构", "m1", "不行。那等于一个会话能看别的项目的目录树，token 泄露就直接变成整机沦陷。工作区面板锚在**当前会话**的 cwd 上，这条不能放宽。"),
            ("member", "前端", "m2", "前端这边补一条：消息里的文件链接先问 `wsOpenPath`，落在工作区外就退回旧浮层——两条口径分开，不要混。"),
            ("member", "评审", "m3", "同意。另外 `..` 段直接 403，不要依赖 `filepath.Clean` 把 `..` 吃掉那个副作用，那只是 Clean 的行为、不是授权语义。"),
            ("user", None, None, "@前端 那链接里的 :行号 呢？"),
            ("member", "前端", "m2", "带过去。代码/纯文本视图用一条绝对定位的色带标出那一行——整段高亮的 HTML 切不开行，一个元素可能横跨多行，切开就是坏 HTML。Markdown 只提示行号不定位。"),
        ],
    }
]


def write_events(sid, events):
    path = EVENTS / f"{sid}.jsonl"
    with path.open("w", encoding="utf-8") as fh:
        for i, ev in enumerate(events):
            rec = {
                "seq": i + 1,
                "ts": iso(ts()),
                "kind": ev["kind"],
                "data": ev["data"],
            }
            fh.write(json.dumps(rec, ensure_ascii=False) + "\n")


def main():
    if DATA.exists():
        shutil.rmtree(DATA)
    EVENTS.mkdir(parents=True)
    GROUPS.mkdir(parents=True)

    sessions = []
    for s in SESSIONS:
        write_events(s["id"], s["events"])
        sessions.append(
            {
                "id": s["id"],
                "name": s["name"],
                "cli": s["cli"],
                "provider": s["provider"],
                "model_override": s.get("model_override", ""),
                "effort_override": s.get("effort_override", ""),
                "permission_mode": s.get("permission_mode", ""),
                "cwd": s.get("cwd", CWD),
                "claude_session_id": "",
                "resident": s.get("resident", False),
                "peer_name": s.get("peer_name", ""),
                "archived": False,
                "unread": s.get("unread", False),
                "todo": s.get("todo", False),
                "sort_order": 0,
                "created_at": iso(BASE_MS - 3_600_000),
                "updated_at": iso(BASE_MS + 1_000_000 - len(sessions) * 60_000),
            }
        )

    for g in GROUPS_SEED:
        gid = g["id"]
        gdir = GROUPS / gid
        gdir.mkdir(parents=True)
        members = []
        for i, m in enumerate(g["members"]):
            mid = f"{gid}-m{i + 1}"
            members.append(
                {
                    "id": mid,
                    "name": m["name"],
                    "cli": m["cli"],
                    "provider": m["provider"],
                    "model": m["model"],
                    "cursor": 0,
                }
            )
            sessions.append(
                {
                    "id": mid,
                    "name": f"{g['name']} · {m['name']}",
                    "cli": m["cli"],
                    "provider": m["provider"],
                    "model_override": m["model"],
                    "cwd": CWD,
                    "claude_session_id": "",
                    "group_id": gid,
                    "archived": False,
                    "unread": False,
                    "todo": False,
                    "sort_order": 0,
                    "created_at": iso(BASE_MS - 3_600_000),
                    "updated_at": iso(BASE_MS + 900_000),
                }
            )
        meta = {
            "id": gid,
            "name": g["name"],
            "cwd": CWD,
            "created_at": iso(BASE_MS - 3_600_000),
            "updated_at": iso(BASE_MS + 900_000),
            "members": members,
        }
        (gdir / "meta.json").write_text(json.dumps(meta, ensure_ascii=False, indent=2), encoding="utf-8")
        byname = {m["name"]: m["id"] for m in members}
        with (gdir / "transcript.jsonl").open("w", encoding="utf-8") as fh:
            for idx, (kind, mname, _mid, body) in enumerate(g["log"]):
                entry = {
                    "id": f"ge-{idx}",
                    "ts": BASE_MS + idx * 45_000,
                    "turn_id": f"turn-{idx // 4}",
                    "kind": kind,
                    "text": body,
                }
                if kind == "member":
                    entry["member_id"] = byname[mname]
                    entry["member_name"] = mname
                else:
                    entry["recipients"] = [m["id"] for m in members]
                fh.write(json.dumps(entry, ensure_ascii=False) + "\n")

    (DATA / "db.json").write_text(
        json.dumps({"sessions": sessions}, ensure_ascii=False, indent=2), encoding="utf-8"
    )
    print(f"seeded {len(sessions)} sessions, {len(GROUPS_SEED)} group(s) -> {DATA}")


if __name__ == "__main__":
    main()
