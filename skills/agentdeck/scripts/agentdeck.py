#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""agentdeck —— 从任意 Agent 里驱动本机 AgentDeck（cc-webui）服务。

只依赖 Python 3 标准库（urllib / json），不装任何第三方包。

设计要点：
  * base_url 与 token 自动发现：先看 AGENTDECK_URL / AGENTDECK_TOKEN 环境变量，
    再从 config.yaml（listen + gateway_token）里读，最后回落 127.0.0.1:8321。
  * 所有子命令默认输出人读得懂的表格；加 --json 则原样打印服务端 JSON。
  * run / reply 的 stdout 只输出模型最后一段回复文本，会话 id 等杂项走 stderr，
    这样 Agent 可以直接把 stdout 当返回值用。

完整用法见同目录上一层的 SKILL.md，或 `agentdeck.py --help`。
"""

from __future__ import annotations

import argparse
import base64
import json
import os
import queue
import re
import socket
import sys
import threading
import time
import unicodedata
import urllib.error
import urllib.parse
import urllib.request

PROG = "agentdeck"
DEFAULT_URL = "http://127.0.0.1:8321"

# claude 的 --effort 固定档位（见 internal/server/server.go validEffort）。
CLAUDE_EFFORTS = ["low", "medium", "high", "xhigh", "max"]

IMAGE_MIME = {
    ".png": "image/png",
    ".jpg": "image/jpeg",
    ".jpeg": "image/jpeg",
    ".gif": "image/gif",
    ".webp": "image/webp",
    ".bmp": "image/bmp",
}

DEFAULT_TURN_TIMEOUT = 1800.0  # 一轮最多等 30 分钟


# --------------------------------------------------------------------------
# 输出小工具
# --------------------------------------------------------------------------

def die(msg: str, code: int = 1) -> "None":
    print(f"{PROG}: {msg}", file=sys.stderr)
    sys.exit(code)


def log(msg: str) -> None:
    print(msg, file=sys.stderr)


def jprint(obj) -> None:
    print(json.dumps(obj, ensure_ascii=False, indent=2, sort_keys=False))


def _dw(s: str) -> int:
    """终端显示宽度：CJK 全角字符算 2 列。"""
    w = 0
    for ch in s:
        w += 2 if unicodedata.east_asian_width(ch) in ("W", "F") else 1
    return w


def table(headers, rows) -> None:
    rows = [[("" if c is None else str(c)) for c in r] for r in rows]
    cols = len(headers)
    widths = [_dw(str(headers[i])) for i in range(cols)]
    for r in rows:
        for i in range(cols):
            widths[i] = max(widths[i], _dw(r[i] if i < len(r) else ""))
    def line(cells):
        out = []
        for i, c in enumerate(cells):
            out.append(c + " " * max(0, widths[i] - _dw(c)))
        return "  ".join(out).rstrip()
    print(line([str(h) for h in headers]))
    print(line(["-" * widths[i] for i in range(cols)]))
    for r in rows:
        print(line([r[i] if i < len(r) else "" for i in range(cols)]))


def trunc(s: str, n: int) -> str:
    s = s.replace("\n", " ")
    return s if len(s) <= n else s[: n - 1] + "…"


def parse_bool(v: str):
    if v is None:
        return None
    return str(v).strip().lower() in ("1", "true", "yes", "y", "on")


# --------------------------------------------------------------------------
# 配置发现
# --------------------------------------------------------------------------

def _scalar(v: str) -> str:
    v = v.strip()
    if not v:
        return ""
    if v[0] in "\"'":
        end = v.find(v[0], 1)
        if end != -1:
            return v[1:end]
    return re.split(r"\s+#", v, maxsplit=1)[0].strip()


def read_config_file(path: str) -> dict:
    """只提取顶层标量键（listen / gateway_token / data_dir / default_cwd）。

    config.yaml 是三层嵌套结构，但这里要的几个键都在顶层；用最小解析避免
    引入 yaml 依赖。
    """
    vals: dict = {}
    try:
        with open(path, "r", encoding="utf-8", errors="replace") as f:
            for raw in f:
                if not raw or raw[0] in " \t#\n\r-":
                    continue
                m = re.match(r"^([A-Za-z_][\w]*):\s*(.*)$", raw.rstrip("\n"))
                if not m:
                    continue
                vals[m.group(1)] = _scalar(m.group(2))
    except OSError:
        return {}
    return vals


def url_from_listen(listen: str):
    listen = (listen or "").strip()
    if not listen:
        return None
    if listen.startswith("["):  # [::]:8321
        host, _, port = listen[1:].partition("]:")
        host = "127.0.0.1" if host in ("", "::", "0.0.0.0") else host
        return f"http://{host}:{port}"
    host, sep, port = listen.rpartition(":")
    if not sep or not port:
        return None
    if host in ("", "0.0.0.0", "::", "*"):
        host = "127.0.0.1"
    if ":" in host:  # 裸 IPv6
        host = f"[{host}]"
    return f"http://{host}:{port}"


def config_candidates():
    env = os.environ.get("AGENTDECK_CONFIG") or os.environ.get("AD_CONFIG")
    if env:
        yield os.path.expanduser(env)
        return
    home = os.path.expanduser("~")
    cwd = os.getcwd()
    seen = set()
    for p in (
        os.path.join(cwd, "config.yaml"),
        os.path.join(home, "Projects", "cc-webui", "config.yaml"),
        os.path.join(home, "cc-webui", "config.yaml"),
        os.path.join(home, ".config", "cc-webui", "config.yaml"),
        os.path.join(home, "agentdeck", "config.yaml"),
    ):
        if p not in seen:
            seen.add(p)
            yield p


def token_file_candidates():
    home = os.path.expanduser("~")
    env = os.environ.get("AGENTDECK_TOKEN_FILE")
    if env:
        yield os.path.expanduser(env)
    for p in (
        os.path.join(home, ".config", "cc-webui", "agentdeck.token"),
        os.path.join(home, ".config", "agentdeck", "token"),
    ):
        yield p


class Ctx:
    def __init__(self, base, token, cfg_path, data_dir, default_cwd, json_mode=False):
        self.base = base.rstrip("/")
        self.token = token
        self.cfg_path = cfg_path
        self.data_dir = data_dir
        self.default_cwd = default_cwd
        self.json = json_mode

    def url(self, path: str) -> str:
        return self.base + path


def resolve_ctx(a) -> Ctx:
    cfg_path = None
    vals: dict = {}
    for p in config_candidates():
        if os.path.isfile(p):
            cfg_path = p
            vals = read_config_file(p)
            break

    base = getattr(a, "url", None) or os.environ.get("AGENTDECK_URL") or os.environ.get("AD_URL")
    if not base:
        base = url_from_listen(vals.get("listen", "")) or DEFAULT_URL
    if not re.match(r"^https?://", base):
        base = "http://" + base

    token = getattr(a, "token", None) or os.environ.get("AGENTDECK_TOKEN") or os.environ.get("AD_TOKEN")
    if not token:
        token = vals.get("gateway_token") or ""
    if not token:
        for p in token_file_candidates():
            try:
                with open(p, "r", encoding="utf-8") as f:
                    token = f.read().strip()
            except OSError:
                continue
            if token:
                break

    return Ctx(
        base=base,
        token=token,
        cfg_path=cfg_path,
        data_dir=vals.get("data_dir") or "",
        default_cwd=vals.get("default_cwd") or "",
        json_mode=bool(getattr(a, "json", False)),
    )


# --------------------------------------------------------------------------
# HTTP
# --------------------------------------------------------------------------

def api(ctx: Ctx, method: str, path: str, payload=None, timeout: float = 600.0, raw_body=None, ctype=None):
    url = ctx.url(path)
    headers = {}
    if ctx.token:
        headers["Authorization"] = "Bearer " + ctx.token
    data = None
    if raw_body is not None:
        data = raw_body
        headers["Content-Type"] = ctype or "application/octet-stream"
    elif payload is not None:
        data = json.dumps(payload).encode("utf-8")
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=data, method=method, headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            body = resp.read()
    except urllib.error.HTTPError as e:
        body = e.read()
        msg = body.decode("utf-8", "replace")
        try:
            j = json.loads(msg)
            if isinstance(j, dict) and j.get("error"):
                msg = j["error"]
        except Exception:
            pass
        if e.code == 401:
            die(f"{method} {path} -> 401 未授权：token 不对。检查 config.yaml 的 gateway_token，"
                f"或设 AGENTDECK_TOKEN。")
        die(f"{method} {path} -> HTTP {e.code}: {msg.strip()}")
    except urllib.error.URLError as e:
        die(f"连不上 AgentDeck（{ctx.base}）：{e.reason}")
    except socket.timeout:
        die(f"{method} {path} 超时（{timeout}s）")
    if not body:
        return None
    try:
        return json.loads(body.decode("utf-8"))
    except Exception:
        return body.decode("utf-8", "replace")


def sse_lines(ctx: Ctx, path: str, deadline: float):
    """逐条 yield SSE 的 data 负载（字符串），读到 deadline 或连接结束为止。

    用后台线程读 socket，而不是在主循环里直接 readline：两套 SSE 的保活策略不同
    （会话流每 25s 一个 ping，群流根本没有 ping），而 Python 的 http.client 一旦
    遇到 socket 超时就会把连接标成「不可再读」，之后再读直接抛 OSError。线程里阻塞
    读、主循环带截止时间从队列取，超时只是结束生成器，连接由 close() 回收。
    """
    req = urllib.request.Request(
        ctx.url(path),
        method="GET",
        headers={
            "Accept": "text/event-stream",
            "Authorization": "Bearer " + ctx.token if ctx.token else "",
        },
    )
    try:
        resp = urllib.request.urlopen(req, timeout=60)
    except urllib.error.HTTPError as e:
        die(f"SSE {path} -> HTTP {e.code}: {e.read().decode('utf-8', 'replace').strip()}")
    except urllib.error.URLError as e:
        die(f"连不上 AgentDeck（{ctx.base}）：{e.reason}")
    # 连接阶段留 60s 兜底；读阶段交给 deadline，socket 本身不再超时。
    try:
        resp.fp.raw._sock.settimeout(None)
    except Exception:
        pass

    q: "queue.Queue" = queue.Queue()
    stop = threading.Event()

    def reader():
        try:
            while not stop.is_set():
                raw = resp.readline()
                if not raw:
                    break
                line = raw.decode("utf-8", "replace").rstrip("\r\n")
                if line.startswith("data:"):
                    q.put(line[5:].lstrip())
        except Exception:
            pass
        finally:
            try:
                q.put(None)
            except Exception:
                pass

    t = threading.Thread(target=reader, name="agentdeck-sse", daemon=True)
    t.start()
    try:
        while True:
            remaining = deadline - time.time()
            if remaining <= 0:
                break
            try:
                item = q.get(timeout=min(remaining, 1.0))
            except queue.Empty:
                continue
            if item is None:
                break
            yield item
    finally:
        stop.set()
        # 必须 shutdown 而不是直接 close：读线程此刻可能正阻塞在 readline 里、
        # 并且持有 BufferedReader 的锁，而 close() 要拿同一把锁——群事件流没有
        # 保活 ping，读线程可以一直卡着，close() 就永远回不来。shutdown 让阻塞的
        # read 立刻返回，锁释放后再 close。
        try:
            resp.fp.raw._sock.shutdown(socket.SHUT_RDWR)
        except Exception:
            pass
        try:
            resp.close()
        except Exception:
            pass


def assistant_text(data: dict) -> str:
    """从 canonical assistant 事件里取出所有 text 块。"""
    msg = data.get("message")
    if not isinstance(msg, dict):
        return ""
    content = msg.get("content")
    if not isinstance(content, list):
        return ""
    out = []
    for blk in content:
        if isinstance(blk, dict) and blk.get("type") == "text":
            t = blk.get("text")
            if isinstance(t, str):
                out.append(t)
    return "".join(out)


# --------------------------------------------------------------------------
# 通用取数
# --------------------------------------------------------------------------

def get_clis(ctx: Ctx):
    return api(ctx, "GET", "/api/clis") or []


def find_cli(ctx: Ctx, name: str):
    clis = get_clis(ctx)
    if not name:
        if len(clis) == 1:
            return clis[0]
        die("需要 --cli（本机有多个 cli：" + ", ".join(c["name"] for c in clis) + "）")
    for c in clis:
        if c["name"] == name:
            return c
    die(f"未知 cli: {name}（可用：" + ", ".join(c["name"] for c in clis) + "）")


def get_providers(ctx: Ctx):
    return api(ctx, "GET", "/api/providers") or []


def find_provider(ctx: Ctx, clis, name: str, kind: str = ""):
    provs = get_providers(ctx)
    if not name:
        for p in provs:
            if not kind or p.get("kind") == kind:
                return p
        die(f"没有 kind={kind or '*'} 的 provider")
    for p in provs:
        if p["name"] == name:
            return p
    die(f"未知 provider: {name}")


def engine_models(ctx: Ctx, cli: dict, prov: dict):
    """按引擎返回归一化的模型列表 [{id, name, efforts, default_effort, input}]。"""
    kind = cli.get("kind") or "claude"
    name = cli["name"]
    pname = prov["name"]
    if kind == "opencode":
        r = api(ctx, "GET", f"/api/opencode-models?cli={urllib.parse.quote(name)}&provider={urllib.parse.quote(pname)}")
        # variants 与模型清单共用服务端同一个 memo，所以这次额外调用几乎不要钱；
        # 顺手带上它，模型列表就能直接看到每个模型支持哪些 --variant。
        v = api(ctx, "GET", f"/api/opencode-model-variants?cli={urllib.parse.quote(name)}&provider={urllib.parse.quote(pname)}")
        variants = (v or {}).get("models") or {}
        out = []
        for m in (r or {}).get("models", []):
            mid = m.get("name", "")
            out.append({"id": mid, "name": mid, "input": m.get("input") or [],
                        "efforts": variants.get(mid) or [], "default_effort": ""})
        return out
    if kind == "mcode":
        r = api(ctx, "GET", f"/api/mcode-models?cli={urllib.parse.quote(name)}&provider={urllib.parse.quote(pname)}")
        return [{"id": m.get("id", ""), "name": m.get("name") or m.get("id", ""),
                 "input": m.get("input") or [], "efforts": m.get("efforts") or [],
                 "default_effort": m.get("default_effort") or ""} for m in (r or {}).get("models", [])]
    if kind == "qoder":
        r = api(ctx, "GET", f"/api/qoder-models?cli={urllib.parse.quote(name)}&provider={urllib.parse.quote(pname)}")
        return [{"id": m.get("name", ""), "name": m.get("name", ""), "input": m.get("input") or [],
                 "efforts": [], "default_effort": ""} for m in (r or {}).get("models", [])]
    if kind == "codex":
        r = api(ctx, "GET", f"/api/codex-models?cli={urllib.parse.quote(name)}&provider={urllib.parse.quote(pname)}")
        return [{"id": m.get("id", ""), "name": m.get("name") or m.get("id", ""),
                 "input": m.get("input") or [], "efforts": m.get("efforts") or [],
                 "default_effort": m.get("default_effort") or ""} for m in (r or {}).get("models", [])]
    # claude：有 models_command 的 launcher（ducc 之类）以命令输出为准
    if cli.get("models_command"):
        r = api(ctx, "GET", f"/api/cli-models?cli={urllib.parse.quote(name)}&provider={urllib.parse.quote(pname)}")
        return [{"id": m.get("name", ""), "name": m.get("name", ""), "input": m.get("input") or [],
                 "efforts": [], "default_effort": ""} for m in (r or {}).get("models", [])]
    return [{"id": m.get("name", ""), "name": m.get("name", ""), "input": m.get("input") or [],
             "efforts": [], "default_effort": ""} for m in (prov.get("models") or [])]


def engine_efforts(ctx: Ctx, cli: dict, prov: dict, model: str):
    """返回 (档位列表, 说明)。空列表 = 该模型没有可切换档位。"""
    kind = cli.get("kind") or "claude"
    if kind == "claude":
        return CLAUDE_EFFORTS, "claude --effort（留空 = 继承配置）"
    if kind == "qoder":
        r = api(ctx, "GET", f"/api/qoder-models?cli={urllib.parse.quote(cli['name'])}&provider={urllib.parse.quote(prov['name'])}")
        return (r or {}).get("efforts") or [], "qoder --reasoning-effort"
    if kind == "opencode":
        if not model:
            return [], "opencode --variant：需要 --model 才能查该模型的 variants"
        r = api(ctx, "GET", f"/api/opencode-model-variants?cli={urllib.parse.quote(cli['name'])}&provider={urllib.parse.quote(prov['name'])}")
        return ((r or {}).get("models") or {}).get(model) or [], "opencode --variant"
    models = engine_models(ctx, cli, prov)
    for m in models:
        if m["id"] == model or m["name"] == model:
            return m["efforts"], f"{kind} 推理档位"
    return [], f"{kind}：未找到模型 {model}"


# --------------------------------------------------------------------------
# 子命令
# --------------------------------------------------------------------------

def cmd_me(ctx: Ctx, a) -> int:
    api(ctx, "GET", "/api/me")
    print(f"连接正常：{ctx.base}" + (f"（配置来自 {ctx.cfg_path}）" if ctx.cfg_path else ""))
    return 0


def cmd_doctor(ctx: Ctx, a) -> int:
    print(f"base_url     : {ctx.base}")
    print(f"token        : {'已设置(' + str(len(ctx.token)) + ' 字符)' if ctx.token else '未设置'}")
    print(f"config       : {ctx.cfg_path or '未找到（用 env 或默认值）'}")
    print(f"data_dir     : {ctx.data_dir or '(未知)'}")
    print(f"default_cwd  : {ctx.default_cwd or '(未知)'}")
    try:
        api(ctx, "GET", "/api/me")
        print("连通性       : OK")
        clis = get_clis(ctx)
        print("clis         : " + ", ".join(f"{c['name']}({c['kind']})" for c in clis))
        return 0
    except SystemExit:
        print("连通性       : 失败", file=sys.stderr)
        return 1


def cmd_clis(ctx: Ctx, a) -> int:
    clis = get_clis(ctx)
    if ctx.json:
        jprint(clis)
        return 0
    table(["name", "kind", "models_command", "path"],
          [[c["name"], c.get("kind"), "yes" if c.get("models_command") else "", c.get("path", "")] for c in clis])
    return 0


def cmd_providers(ctx: Ctx, a) -> int:
    provs = get_providers(ctx)
    kind = ""
    if a.cli:
        kind = find_cli(ctx, a.cli).get("kind") or "claude"
    rows = [p for p in provs if not kind or p.get("kind") == kind]
    if ctx.json:
        jprint(rows)
        return 0
    table(["name", "kind", "models", "env_keys"],
          [[p["name"], p.get("kind"), len(p.get("models") or []), ",".join(p.get("env_keys") or [])] for p in rows])
    return 0


def cmd_profiles(ctx: Ctx, a) -> int:
    jprint(api(ctx, "GET", "/api/profiles"))
    return 0


def cmd_config(ctx: Ctx, a) -> int:
    jprint(api(ctx, "GET", "/api/config"))
    return 0


def cmd_models(ctx: Ctx, a) -> int:
    cli = find_cli(ctx, a.cli)
    prov = find_provider(ctx, None, a.provider, cli.get("kind") or "claude")
    models = engine_models(ctx, cli, prov)
    if ctx.json:
        jprint(models)
        return 0
    table(["model", "efforts", "input", "display"],
          [[m["id"], ",".join(m["efforts"]) or "-", "+".join(m["input"]) or "text", m["name"] if m["name"] != m["id"] else ""]
           for m in models])
    return 0


def cmd_efforts(ctx: Ctx, a) -> int:
    cli = find_cli(ctx, a.cli)
    prov = find_provider(ctx, None, a.provider, cli.get("kind") or "claude")
    levels, note = engine_efforts(ctx, cli, prov, a.model or "")
    if ctx.json:
        jprint({"efforts": levels, "note": note})
        return 0
    if not levels:
        print(f"(无可切换档位) {note}")
        return 0
    print(note + "：")
    for lv in levels:
        print("  " + lv)
    return 0


def _status_str(st: dict) -> str:
    if not st:
        return "idle"
    parts = []
    if st.get("running"):
        parts.append("running")
    if st.get("queued"):
        parts.append(f"queued:{st['queued']}")
    if st.get("resident"):
        parts.append("resident" + (":" + st["registered_name"] if st.get("registered_name") else ""))
    if st.get("last_error"):
        parts.append("error")
    return ",".join(parts) or "idle"


def cmd_ls(ctx: Ctx, a) -> int:
    path = "/api/sessions" + ("?archived=true" if a.archived else "")
    sessions = api(ctx, "GET", path) or []
    if ctx.json:
        jprint(sessions)
        return 0
    rows = []
    for s in sessions:
        rows.append([
            s.get("id", ""),
            trunc(s.get("name", ""), 28),
            s.get("engine", ""),
            s.get("cli", "") + "/" + s.get("provider", ""),
            trunc(s.get("model_override", "") or "-", 24),
            s.get("effort_override", "") or "-",
            _status_str(s.get("status")),
            (s.get("updated_at") or "")[:19].replace("T", " "),
        ])
    table(["id", "name", "engine", "cli/provider", "model", "effort", "status", "updated"], rows)
    return 0


def _create_payload(a) -> dict:
    body: dict = {}
    if a.name:
        body["name"] = a.name
    if a.cli:
        body["cli"] = a.cli
    if a.provider:
        body["provider"] = a.provider
    if a.model:
        body["model"] = a.model
    if a.effort is not None:
        body["effort"] = a.effort
    if a.permission_mode:
        body["permission_mode"] = a.permission_mode
    if getattr(a, "resident", False):
        body["resident"] = True
    if getattr(a, "peer_name", None):
        body["peer_name"] = a.peer_name
    return body


def _resolve_cwd(ctx: Ctx, cwd: str) -> str:
    if cwd:
        return cwd
    if ctx.default_cwd:
        return ctx.default_cwd
    return os.getcwd()


def cmd_new(ctx: Ctx, a) -> int:
    body = _create_payload(a)
    body["cwd"] = _resolve_cwd(ctx, a.cwd)
    sess = api(ctx, "POST", "/api/sessions", body)
    if ctx.json:
        jprint(sess)
        return 0
    log(f"已创建会话 {sess['id']}（{sess.get('name')}）")
    log(f"  cli/provider: {sess.get('cli')}/{sess.get('provider')}  引擎: {sess.get('engine')}")
    if sess.get("model_override") or sess.get("effort_override"):
        log(f"  model/effort: {sess.get('model_override') or '-'} / {sess.get('effort_override') or '-'}")
    log(f"  cwd: {sess.get('cwd')}")
    log("前端侧栏 5s 内会出现该会话。发送消息：agentdeck.py send " + sess["id"] + " '…'")
    print(sess["id"])
    return 0


def cmd_get(ctx: Ctx, a) -> int:
    s = api(ctx, "GET", f"/api/sessions/{urllib.parse.quote(a.id)}")
    if ctx.json:
        jprint(s)
        return 0
    for k in ("id", "name", "engine", "cli", "provider", "model_override", "effort_override",
              "permission_mode", "cwd", "claude_session_id", "resident", "peer_name",
              "archived", "unread", "todo"):
        if s.get(k) not in (None, "", False):
            print(f"{k:18}: {s[k]}")
    print(f"{'status':18}: {_status_str(s.get('status'))}")
    if s.get("resume_command"):
        print(f"{'resume_command':18}: {s['resume_command']}")
    if s.get("goal"):
        g = s["goal"]
        print(f"{'goal':18}: [{g.get('status')}] {g.get('text')} ({g.get('turns')}/{g.get('max_turns')} turns)")
    return 0


def cmd_set(ctx: Ctx, a) -> int:
    body: dict = {}
    if a.name is not None:
        body["name"] = a.name
    if a.cli is not None:
        body["cli"] = a.cli
    if a.provider is not None:
        body["provider"] = a.provider
    if a.model is not None:
        body["model"] = a.model
    if a.effort is not None:
        body["effort"] = a.effort
    if a.permission_mode is not None:
        body["permission_mode"] = a.permission_mode
    if a.cwd is not None:
        body["cwd"] = a.cwd
    for key in ("archived", "resident", "unread", "todo"):
        v = getattr(a, key, None)
        if v is not None:
            body[key] = parse_bool(v)
    if a.peer_name is not None:
        body["peer_name"] = a.peer_name
    if a.sort_order is not None:
        body["sort_order"] = a.sort_order
    if not body:
        die("没有要修改的字段")
    s = api(ctx, "PATCH", f"/api/sessions/{urllib.parse.quote(a.id)}", body)
    if ctx.json:
        jprint(s)
    else:
        log(f"已更新 {s['id']}（{s.get('name')}）")
    return 0


def cmd_rm(ctx: Ctx, a) -> int:
    api(ctx, "DELETE", f"/api/sessions/{urllib.parse.quote(a.id)}")
    log(f"已删除 {a.id}")
    return 0


def _upload(ctx: Ctx, sid: str, path: str) -> dict:
    if not os.path.isfile(path):
        die(f"文件不存在: {path}")
    with open(path, "rb") as f:
        data = f.read()
    name = os.path.basename(path)
    q = urllib.parse.urlencode({"name": name})
    r = api(ctx, "POST", f"/api/sessions/{urllib.parse.quote(sid)}/uploads?{q}", raw_body=data,
            ctype="application/octet-stream")
    return {"name": r.get("name") or name, "path": r.get("path")}


def _images(paths):
    out = []
    for p in paths or []:
        if not os.path.isfile(p):
            die(f"图片不存在: {p}")
        ext = os.path.splitext(p)[1].lower()
        mime = IMAGE_MIME.get(ext)
        if not mime:
            die(f"不支持的图片扩展名: {p}（支持 {', '.join(sorted(IMAGE_MIME))}）")
        with open(p, "rb") as f:
            out.append({"media_type": mime, "data": base64.b64encode(f.read()).decode("ascii")})
    return out


def _message_body(ctx: Ctx, sid: str, text: str, files, images) -> dict:
    body: dict = {"text": text or ""}
    if files:
        body["files"] = [_upload(ctx, sid, p) for p in files]
    if images:
        body["images"] = _images(images)
    return body


def _read_text(a) -> str:
    if a.text is not None:
        text = a.text
    else:
        text = " ".join(a.prompt or [])
    if text == "-":
        text = sys.stdin.read()
    return text


def cmd_send(ctx: Ctx, a) -> int:
    text = _read_text(a)
    body = _message_body(ctx, a.id, text, a.file, a.image)
    if not body["text"].strip() and not body.get("files") and not body.get("images"):
        die("空消息")
    api(ctx, "POST", f"/api/sessions/{urllib.parse.quote(a.id)}/messages", body)
    if ctx.json:
        jprint({"ok": True, "session_id": a.id})
    else:
        log(f"已投递到 {a.id}")
    return 0


def cmd_upload(ctx: Ctx, a) -> int:
    r = _upload(ctx, a.id, a.path)
    if ctx.json:
        jprint(r)
    else:
        print(r["path"])
    return 0


def cmd_stop(ctx: Ctx, a) -> int:
    api(ctx, "POST", f"/api/sessions/{urllib.parse.quote(a.id)}/stop")
    log(f"已请求停止 {a.id}")
    return 0


def cmd_goal(ctx: Ctx, a) -> int:
    body: dict = {"text": a.text}
    if a.max_turns is not None:
        body["max_turns"] = a.max_turns
    if a.max_hours is not None:
        body["max_hours"] = a.max_hours
    s = api(ctx, "PUT", f"/api/sessions/{urllib.parse.quote(a.id)}/goal", body)
    if ctx.json:
        jprint(s)
    else:
        log(f"已设置自动续跑目标：{a.text}")
    return 0


def cmd_ungoal(ctx: Ctx, a) -> int:
    s = api(ctx, "DELETE", f"/api/sessions/{urllib.parse.quote(a.id)}/goal")
    if ctx.json:
        jprint(s)
    else:
        log("已清除自动续跑目标")
    return 0


def _wait_idle(ctx: Ctx, sid: str, timeout: float, interval: float = 2.0):
    deadline = time.time() + timeout
    last = None
    while True:
        s = api(ctx, "GET", f"/api/sessions/{urllib.parse.quote(sid)}")
        last = s
        st = s.get("status") or {}
        if not st.get("running") and not st.get("queued"):
            return s
        if time.time() >= deadline:
            return None
        time.sleep(interval)


def cmd_wait(ctx: Ctx, a) -> int:
    s = _wait_idle(ctx, a.id, a.timeout)
    if s is None:
        die(f"等待超时（{a.timeout}s）：会话仍在运行")
    if ctx.json:
        jprint(s)
    else:
        print(_status_str(s.get("status")))
        if (s.get("status") or {}).get("last_error"):
            print((s["status"])["last_error"], file=sys.stderr)
    return 0


def _stream_turn(ctx: Ctx, sid: str, timeout: float, on_ready, verbose: bool):
    """连 SSE → 等 replay_end → 回调发消息 → 收集到本轮结束。

    返回 (text, state, error)。state 为 turn_end / interrupted / error / None(超时)。
    """
    deadline = time.time() + timeout
    texts: list = []
    state = None
    err = None
    ready = False
    delta_open = False  # 逐字增量写到一半时，下一条日志前先换行

    def vlog(msg: str):
        nonlocal delta_open
        if delta_open:
            sys.stderr.write("\n")
            delta_open = False
        log(msg)

    for payload in sse_lines(ctx, f"/api/sessions/{urllib.parse.quote(sid)}/events", deadline):
        try:
            ev = json.loads(payload)
        except Exception:
            continue
        kind = ev.get("kind")
        if not ready:
            if kind == "replay_end":
                ready = True
                on_ready()
            continue
        data = ev.get("data")
        if not isinstance(data, dict):
            data = {}
        if kind == "status":
            st = data.get("state")
            if st == "turn_start":
                state = "running"
                if verbose:
                    vlog("[status] turn_start")
            elif st in ("turn_end", "interrupted", "error"):
                state = st
                err = data.get("error")
                if verbose:
                    vlog(f"[status] {st}" + (f" {err}" if err else ""))
                break
        elif kind == "stream":
            t = data.get("type")
            if t == "assistant":
                txt = assistant_text(data)
                if txt:
                    texts.append(txt)
                    if verbose:
                        vlog("[assistant] " + trunc(txt, 200))
            elif t == "oc_text_delta":
                if verbose:
                    sys.stderr.write(data.get("delta") or "")
                    sys.stderr.flush()
                    delta_open = True
    if verbose and delta_open:
        sys.stderr.write("\n")
    return "\n".join(texts).strip(), state, err


def _emit_turn_result(ctx: Ctx, sess: dict, text: str, state, err) -> int:
    if ctx.json:
        jprint({"session_id": sess.get("id"), "name": sess.get("name"), "state": state,
                "error": err, "text": text})
        return 0
    if err:
        log("本轮出错: " + err)
    if text:
        print(text)
    if state is None:
        log("等待超时：本轮尚未结束")
        return 3
    if state == "interrupted":
        return 4
    if state == "error":
        return 5
    return 0


def cmd_run(ctx: Ctx, a) -> int:
    """新建会话 + 发一条消息 + 等本轮跑完 + 打印模型回复。"""
    body = _create_payload(a)
    body["cwd"] = _resolve_cwd(ctx, a.cwd)
    sess = api(ctx, "POST", "/api/sessions", body)
    sid = sess["id"]
    log(f"已创建会话 {sid}（{sess.get('name')}）→ {ctx.base}")

    text = _read_text(a)
    files = list(a.file or [])
    images = list(a.image or [])
    sent = {"done": False}

    def on_ready():
        b = _message_body(ctx, sid, text, files, images)
        if not b["text"].strip() and not b.get("files") and not b.get("images"):
            die("空消息")
        api(ctx, "POST", f"/api/sessions/{urllib.parse.quote(sid)}/messages", b)
        sent["done"] = True
        if a.verbose:
            log("已投递首条消息，等待模型…")

    out, state, err = _stream_turn(ctx, sid, a.timeout, on_ready, a.verbose)
    return _emit_turn_result(ctx, sess, out, state, err)


def cmd_reply(ctx: Ctx, a) -> int:
    """给已有会话发消息并等结果（要求会话当前空闲）。"""
    sid = a.id
    cur = api(ctx, "GET", f"/api/sessions/{urllib.parse.quote(sid)}")
    st = cur.get("status") or {}
    if st.get("running") or st.get("queued"):
        if not a.queue:
            log("会话正忙，先等它空闲…")
            if _wait_idle(ctx, sid, a.timeout) is None:
                die(f"等待空闲超时（{a.timeout}s）")
    text = _read_text(a)
    files = list(a.file or [])
    images = list(a.image or [])

    def on_ready():
        b = _message_body(ctx, sid, text, files, images)
        api(ctx, "POST", f"/api/sessions/{urllib.parse.quote(sid)}/messages", b)
        if a.verbose:
            log("已投递消息，等待模型…")

    out, state, err = _stream_turn(ctx, sid, a.timeout, on_ready, a.verbose)
    return _emit_turn_result(ctx, cur, out, state, err)


def cmd_tail(ctx: Ctx, a) -> int:
    """跟随 SSE，把事件打成人话；默认在「实时」阶段第一次轮次结束时退出。

    回放阶段（replay_end 之前）里的 turn_end 属于历史，不能当成结束信号——
    否则一个有历史的会话一 tail 就立刻退出。
    """
    deadline = time.time() + a.timeout
    ready = False
    state = None
    for payload in sse_lines(ctx, f"/api/sessions/{urllib.parse.quote(a.id)}/events", deadline):
        try:
            ev = json.loads(payload)
        except Exception:
            continue
        if ctx.json:
            print(json.dumps(ev, ensure_ascii=False))
            continue
        kind = ev.get("kind")
        data = ev.get("data")
        if not isinstance(data, dict):
            data = {}
        if kind == "replay_end":
            ready = True
            print("--- 历史回放结束，以下为实时 ---")
            continue
        prefix = "" if ready else "[历史] "
        if kind == "status":
            st = data.get("state") or ""
            print(f"{prefix}[status] {st}" + (f" {data.get('error')}" if data.get("error") else ""))
            if ready and st in ("turn_end", "interrupted", "error"):
                state = st
                if not a.follow:
                    break
        elif kind == "stream":
            t = data.get("type")
            if t == "assistant":
                txt = assistant_text(data)
                if txt:
                    print(prefix + "[assistant] " + txt)
                for blk in (data.get("message") or {}).get("content") or []:
                    if isinstance(blk, dict) and blk.get("type") == "tool_use":
                        print(f"{prefix}[tool] {blk.get('name')} {trunc(json.dumps(blk.get('input') or {}, ensure_ascii=False), 160)}")
            elif t == "user":
                for blk in (data.get("message") or {}).get("content") or []:
                    if isinstance(blk, dict) and blk.get("type") == "tool_result":
                        print(f"{prefix}[tool_result] " + trunc(str(blk.get("content"))[:200], 200))
            elif t == "oc_text_delta":
                sys.stdout.write(data.get("delta") or "")
                sys.stdout.flush()
            elif t == "permission_request":
                print(f"{prefix}[ask] {data.get('tool')} {trunc(json.dumps(data.get('input') or {}, ensure_ascii=False), 160)}")
    if state is None and not ctx.json:
        log("跟随结束（超时或连接关闭）")
    return 0


def cmd_fork(ctx: Ctx, a) -> int:
    body = {"name": a.name} if a.name else {}
    s = api(ctx, "POST", f"/api/sessions/{urllib.parse.quote(a.id)}/fork", body)
    if ctx.json:
        jprint(s)
    else:
        log(f"已分叉出 {s['id']}（{s.get('name')}）")
        print(s["id"])
    return 0


def cmd_rewind(ctx: Ctx, a) -> int:
    s = api(ctx, "POST", f"/api/sessions/{urllib.parse.quote(a.id)}/rewind", {"checkpoint": a.checkpoint})
    if ctx.json:
        jprint(s)
    else:
        log("已回滚")
    return 0


def cmd_pending(ctx: Ctx, a) -> int:
    r = api(ctx, "GET", f"/api/sessions/{urllib.parse.quote(a.id)}/permissions/pending")
    if ctx.json:
        jprint(r)
        return 0
    items = r if isinstance(r, list) else (r or {}).get("pending") or []
    if not items:
        print("(无待回答的提问/审批)")
        return 0
    for it in items:
        print(f"{it.get('id')}  {it.get('tool')}  {trunc(json.dumps(it.get('input') or {}, ensure_ascii=False), 200)}")
    return 0


def cmd_answer(ctx: Ctx, a) -> int:
    body: dict = {"behavior": a.behavior}
    if a.message:
        body["message"] = a.message
    if a.answers:
        body["answers"] = json.loads(a.answers)
    r = api(ctx, "POST", f"/api/sessions/{urllib.parse.quote(a.id)}/permissions/{urllib.parse.quote(a.pid)}", body)
    if ctx.json:
        jprint(r)
    else:
        log(f"已回复 {a.pid}: {a.behavior}")
    return 0


HISTORY_PATHS = {
    "claude": "/api/claude-sessions",
    "qoder": "/api/qoder-sessions",
    "opencode": "/api/opencode-sessions",
    "mcode": "/api/mcode-sessions",
    "codex": "/api/codex-sessions",
}


def cmd_history(ctx: Ctx, a) -> int:
    engine = a.engine
    path = HISTORY_PATHS.get(engine)
    if not path:
        die(f"未知引擎 {engine}（可用：{', '.join(HISTORY_PATHS)}）")
    q: dict = {}
    if a.cwd:
        q["cwd"] = a.cwd
    if a.prefix:
        q["prefix"] = a.prefix
    if engine in ("opencode", "mcode", "codex"):
        if a.cli:
            q["cli"] = a.cli
        else:
            kind_cli = [c for c in get_clis(ctx) if c.get("kind") == engine]
            if kind_cli:
                q["cli"] = kind_cli[0]["name"]
    if q:
        path += "?" + urllib.parse.urlencode(q)
    r = api(ctx, "GET", path) or []
    if ctx.json:
        jprint(r)
        return 0
    table(["session_id", "title", "cwd", "updated_at"],
          [[trunc(i.get("session_id", ""), 40), trunc(i.get("title", ""), 36), trunc(i.get("cwd", ""), 40),
            (i.get("updated_at") or "")[:19].replace("T", " ")] for i in r])
    return 0


def cmd_import(ctx: Ctx, a) -> int:
    body = {"claude_session_id": a.session_id, "cwd": a.cwd}
    if a.cli:
        body["cli"] = a.cli
    if a.provider:
        body["provider"] = a.provider
    if a.name:
        body["name"] = a.name
    s = api(ctx, "POST", "/api/sessions/import", body)
    if ctx.json:
        jprint(s)
    else:
        log(f"已导入 {s['id']}（{s.get('name')}）")
        print(s["id"])
    return 0


# ---- 群聊 ----

def cmd_groups(ctx: Ctx, a) -> int:
    gs = api(ctx, "GET", "/api/groups") or []
    if ctx.json:
        jprint(gs)
        return 0
    table(["id", "name", "cwd", "members", "running"],
          [[g.get("id", ""), g.get("name", ""), trunc(g.get("cwd", ""), 40),
            ", ".join(m.get("name", "") for m in g.get("members") or []),
            "yes" if g.get("running") else ""] for g in gs])
    return 0


def _parse_members(specs, members_json):
    members = []
    if members_json:
        members.extend(json.loads(members_json))
    for spec in specs or []:
        parts = spec.split(":")
        if not parts or not parts[0].strip():
            die(f"--member 需要 name[:cli[:provider[:model[:effort]]]]，收到 {spec!r}")
        m = {"name": parts[0].strip()}
        for key, val in zip(("cli", "provider", "model", "effort"), parts[1:]):
            if val.strip():
                m[key] = val.strip()
        members.append(m)
    return members


def cmd_group_new(ctx: Ctx, a) -> int:
    members = _parse_members(a.member, a.members_json)
    if not members:
        die("至少一个 --member")
    body = {"name": a.name, "cwd": _resolve_cwd(ctx, a.cwd), "members": members}
    g = api(ctx, "POST", "/api/groups", body)
    if ctx.json:
        jprint(g)
    else:
        log(f"已建群 {g['id']}（{g.get('name')}）：" + ", ".join(m.get("name", "") for m in g.get("members") or []))
        print(g["id"])
    return 0


def cmd_group_get(ctx: Ctx, a) -> int:
    r = api(ctx, "GET", f"/api/groups/{urllib.parse.quote(a.id)}")
    # GET /api/groups/{id} 返回 {group, transcript}，而列表接口直接给 group 数组。
    g = r.get("group") if isinstance(r, dict) else None
    if g is None:
        g = r or {}
    if ctx.json:
        jprint(r)
        return 0
    print(f"{g.get('name')}  ({g.get('id')})  cwd={g.get('cwd')}  running={'yes' if g.get('running') else 'no'}")
    table(["member_id", "name", "cli", "provider", "model", "effort"],
          [[m.get("id", ""), m.get("name", ""), m.get("cli", ""), m.get("provider", ""),
            m.get("model", "") or "-", m.get("effort", "") or "-"] for m in g.get("members") or []])
    if a.transcript and isinstance(r, dict):
        print("\n--- transcript ---")
        for e in r.get("transcript") or []:
            print(json.dumps(e, ensure_ascii=False))
    return 0


def cmd_group_set(ctx: Ctx, a) -> int:
    body: dict = {}
    for k in ("cli", "provider", "model", "effort"):
        v = getattr(a, k, None)
        if v is not None:
            body[k] = v
    if not body:
        die("没有要修改的字段")
    g = api(ctx, "PATCH", f"/api/groups/{urllib.parse.quote(a.id)}/members/{urllib.parse.quote(a.member_id)}", body)
    if ctx.json:
        jprint(g)
    else:
        log("已更新成员档案（下一轮生效）")
    return 0


def cmd_group_send(ctx: Ctx, a) -> int:
    r = api(ctx, "POST", f"/api/groups/{urllib.parse.quote(a.id)}/messages", {"text": a.text})
    if ctx.json:
        jprint(r)
    else:
        log(f"已投递（recipients: {r.get('recipients')}）")
    return 0


def cmd_group_stop(ctx: Ctx, a) -> int:
    api(ctx, "POST", f"/api/groups/{urllib.parse.quote(a.id)}/stop")
    log("已请求停止该群本轮")
    return 0


def cmd_group_rm(ctx: Ctx, a) -> int:
    api(ctx, "DELETE", f"/api/groups/{urllib.parse.quote(a.id)}")
    log(f"已删除群 {a.id}")
    return 0


def cmd_group_tail(ctx: Ctx, a) -> int:
    """跟随群的 SSE 事件流。

    群流的词汇与会话流不同：没有 status 事件，轮次边界是 kind="group" 且
    data.type=="turn_end"；成员发言是 member_begin / member_text / member_end。
    """
    deadline = time.time() + a.timeout
    ready = False
    state = None
    for payload in sse_lines(ctx, f"/api/groups/{urllib.parse.quote(a.id)}/events", deadline):
        try:
            ev = json.loads(payload)
        except Exception:
            continue
        if ctx.json:
            print(json.dumps(ev, ensure_ascii=False))
            continue
        kind = ev.get("kind")
        data = ev.get("data")
        if not isinstance(data, dict):
            data = {}
        if kind == "replay_end":
            ready = True
            print("--- 历史回放结束，以下为实时 ---")
            continue
        prefix = "" if ready else "[历史] "
        if kind == "group":
            t = data.get("type")
            who = data.get("member_name") or ""
            if t == "user":
                print(prefix + "[user] " + (data.get("text") or ""))
            elif t == "member_begin":
                print(f"{prefix}[{who}] 开始")
            elif t == "member_text":
                print(f"{prefix}[{who}] " + (data.get("text") or ""))
            elif t == "member_end":
                tag = "ok" if data.get("ok") else "失败"
                extra = (" " + data["error"]) if data.get("error") else ""
                print(f"{prefix}[{who}] 结束（{tag}）{extra}")
            elif t == "turn_end":
                print(prefix + "[turn_end]")
                state = "turn_end"
                if ready and not a.follow:
                    break
        elif kind == "permission_request":
            print(f"{prefix}[ask] {data.get('tool')} "
                  f"{trunc(json.dumps(data.get('input') or {}, ensure_ascii=False), 160)}")
    if state is None and not ctx.json:
        log("跟随结束（超时或连接关闭）")
    return 0


# ---- 杂项 ----

def cmd_notes(ctx: Ctx, a) -> int:
    r = api(ctx, "GET", "/api/notes") or []
    if ctx.json:
        jprint(r)
        return 0
    table(["done", "text", "updated_at"],
          [["x" if n.get("done") else "", trunc(n.get("text", ""), 60),
            (n.get("updated_at") or "")[:19].replace("T", " ")] for n in r])
    return 0


def cmd_notes_set(ctx: Ctx, a) -> int:
    if a.file:
        with open(a.file, "r", encoding="utf-8") as f:
            data = json.load(f)
    else:
        data = json.load(sys.stdin)
    if not isinstance(data, list):
        die("notes 必须是 JSON 数组，元素形如 {\"text\": \"...\", \"done\": false}")
    r = api(ctx, "PUT", "/api/notes", data)
    if ctx.json:
        jprint(r)
    else:
        log(f"已保存 {len(r or [])} 条备忘")
    return 0


def cmd_stats(ctx: Ctx, a) -> int:
    path = "/api/stats" + ("?fresh=1" if a.fresh else "")
    jprint(api(ctx, "GET", path, timeout=1800))
    return 0


def cmd_raw(ctx: Ctx, a) -> int:
    method = a.method.upper()
    payload = None
    if a.data_file:
        with open(a.data_file, "r", encoding="utf-8") as f:
            payload = json.load(f)
    elif a.data:
        payload = json.loads(a.data)
    r = api(ctx, method, a.path, payload)
    jprint(r)
    return 0


# --------------------------------------------------------------------------
# CLI 定义
# --------------------------------------------------------------------------

def build_parser() -> argparse.ArgumentParser:
    # 三个全局开关用 SUPPRESS 作默认值：主解析器与子解析器共用同一个 namespace，
    # 若子解析器带默认值，会把「写在子命令之前」的 --json/--url/--token 覆盖回默认。
    common = argparse.ArgumentParser(add_help=False)
    common.add_argument("--url", default=argparse.SUPPRESS,
                        help="AgentDeck base URL（默认从 config.yaml 的 listen 推导）")
    common.add_argument("--token", default=argparse.SUPPRESS,
                        help="gateway token（默认从 config.yaml 或 env 读）")
    common.add_argument("--json", action="store_true", default=argparse.SUPPRESS,
                        help="原样输出服务端 JSON")

    p = argparse.ArgumentParser(
        prog=PROG,
        description="从任意 Agent 里驱动本机 AgentDeck（cc-webui）。",
        parents=[common],
    )
    sub = p.add_subparsers(dest="cmd", metavar="<command>")

    def add(name, help_, func):
        sp = sub.add_parser(name, help=help_, parents=[common], description=help_)
        sp.set_defaults(func=func)
        return sp

    add("doctor", "打印解析到的 base_url/token/config 并做连通性检查", cmd_doctor)
    add("me", "连通性检查", cmd_me)
    add("clis", "列出 cli", cmd_clis)
    sp = add("providers", "列出 provider（可按 --cli 过滤引擎）", cmd_providers)
    sp.add_argument("--cli")
    add("profiles", "原始 cli+provider 组合视图", cmd_profiles)
    add("config", "服务端配置（default_cwd / data_dir / home）", cmd_config)

    sp = add("models", "列出某 cli+provider 的可用模型（按引擎自动分派）", cmd_models)
    sp.add_argument("--cli", required=True)
    sp.add_argument("--provider")
    sp.add_argument("--fresh", action="store_true", help="强制刷新动态目录（opencode/mcode/qoder/codex）")

    sp = add("efforts", "列出某模型可用的推理档位（effort/variant）", cmd_efforts)
    sp.add_argument("--cli", required=True)
    sp.add_argument("--provider")
    sp.add_argument("--model")

    # 会话
    sp = add("ls", "列出会话", cmd_ls)
    sp.add_argument("--archived", action="store_true")

    sp = add("new", "新建会话（默认打印 session id 到 stdout）", cmd_new)
    sp.add_argument("--name", help="会话名（省略则服务端生成 session-<id8>）")
    sp.add_argument("--cli")
    sp.add_argument("--provider")
    sp.add_argument("--model")
    sp.add_argument("--effort", help="推理档位；空串 = 继承配置")
    sp.add_argument("--permission-mode")
    sp.add_argument("--cwd", help="工作区（省略则用 config 的 default_cwd，再退回当前目录）")
    sp.add_argument("--resident", action="store_true", help="claude 专属：建会话即开常驻")
    sp.add_argument("--peer-name", help="claude 专属：常驻地址名")

    sp = add("get", "看单个会话详情", cmd_get)
    sp.add_argument("id")

    sp = add("set", "改会话（只改显式给出的字段）", cmd_set)
    sp.add_argument("id")
    sp.add_argument("--name")
    sp.add_argument("--cli")
    sp.add_argument("--provider")
    sp.add_argument("--model")
    sp.add_argument("--effort")
    sp.add_argument("--permission-mode")
    sp.add_argument("--cwd")
    sp.add_argument("--peer-name")
    sp.add_argument("--archived", help="true/false")
    sp.add_argument("--resident", help="true/false")
    sp.add_argument("--unread", help="true/false")
    sp.add_argument("--todo", help="true/false")
    sp.add_argument("--sort-order", type=int)

    sp = add("rm", "删除会话", cmd_rm)
    sp.add_argument("id")

    sp = add("send", "给会话发消息（不等待）", cmd_send)
    sp.add_argument("id")
    sp.add_argument("prompt", nargs="*", help="消息文本；用 - 从 stdin 读")
    sp.add_argument("--text", help="消息文本（与位置参数二选一）")
    sp.add_argument("--file", action="append", help="作为附件上传的文件（可重复）")
    sp.add_argument("--image", action="append", help="内嵌图片（可重复；mcode/codex 不支持）")

    sp = add("upload", "上传文件到会话，打印落盘路径", cmd_upload)
    sp.add_argument("id")
    sp.add_argument("path")

    sp = add("stop", "停止当前轮次", cmd_stop)
    sp.add_argument("id")

    sp = add("goal", "设置自动续跑目标", cmd_goal)
    sp.add_argument("id")
    sp.add_argument("text")
    sp.add_argument("--max-turns", type=int)
    sp.add_argument("--max-hours", type=float)

    sp = add("ungoal", "清除自动续跑目标", cmd_ungoal)
    sp.add_argument("id")

    sp = add("wait", "等会话空闲（本轮跑完）", cmd_wait)
    sp.add_argument("id")
    sp.add_argument("--timeout", type=float, default=DEFAULT_TURN_TIMEOUT)

    sp = add("tail", "跟随会话 SSE 事件流（默认本轮结束即退出）", cmd_tail)
    sp.add_argument("id")
    sp.add_argument("--timeout", type=float, default=DEFAULT_TURN_TIMEOUT)
    sp.add_argument("--follow", action="store_true", help="不因轮次结束而退出")

    sp = add("run", "新建会话 + 发首条消息 + 等结果（stdout 只输出模型回复）", cmd_run)
    sp.add_argument("prompt", nargs="*", help="首条消息；用 - 从 stdin 读")
    sp.add_argument("--text")
    sp.add_argument("--name")
    sp.add_argument("--cli")
    sp.add_argument("--provider")
    sp.add_argument("--model")
    sp.add_argument("--effort")
    sp.add_argument("--permission-mode")
    sp.add_argument("--cwd")
    sp.add_argument("--resident", action="store_true")
    sp.add_argument("--peer-name")
    sp.add_argument("--file", action="append")
    sp.add_argument("--image", action="append")
    sp.add_argument("--timeout", type=float, default=DEFAULT_TURN_TIMEOUT)
    sp.add_argument("--verbose", action="store_true", help="把过程打到 stderr")

    sp = add("reply", "给已有会话发消息并等结果（stdout 只输出模型回复）", cmd_reply)
    sp.add_argument("id")
    sp.add_argument("prompt", nargs="*")
    sp.add_argument("--text")
    sp.add_argument("--file", action="append")
    sp.add_argument("--image", action="append")
    sp.add_argument("--timeout", type=float, default=DEFAULT_TURN_TIMEOUT)
    sp.add_argument("--verbose", action="store_true")
    sp.add_argument("--queue", action="store_true", help="会话忙时不等待，直接排队")

    sp = add("fork", "分叉会话（claude/qoder）", cmd_fork)
    sp.add_argument("id")
    sp.add_argument("--name")

    sp = add("rewind", "回滚到某个 checkpoint（claude/opencode）", cmd_rewind)
    sp.add_argument("id")
    sp.add_argument("--checkpoint", required=True)

    sp = add("pending", "看待回答的提问/审批卡", cmd_pending)
    sp.add_argument("id")

    sp = add("answer", "回复一张提问/审批卡", cmd_answer)
    sp.add_argument("id")
    sp.add_argument("pid")
    sp.add_argument("--behavior", required=True, choices=["allow", "deny"])
    sp.add_argument("--message")
    sp.add_argument("--answers", help='JSON，例如 {"问题":"答案"}')

    sp = add("history", "列出各引擎本地历史会话（导入用）", cmd_history)
    sp.add_argument("--engine", required=True, choices=sorted(HISTORY_PATHS))
    sp.add_argument("--cli")
    sp.add_argument("--cwd")
    sp.add_argument("--prefix")

    sp = add("import", "把本地历史会话导入 AgentDeck", cmd_import)
    sp.add_argument("--session-id", required=True, help="本地 session id（history 列出的）")
    sp.add_argument("--cwd", required=True, help="工作区（mcode 必填）")
    sp.add_argument("--cli")
    sp.add_argument("--provider")
    sp.add_argument("--name")

    add("groups", "列出群聊", cmd_groups)

    sp = add("group-new", "建群", cmd_group_new)
    sp.add_argument("--name", required=True)
    sp.add_argument("--cwd")
    sp.add_argument("--member", action="append", help="name[:cli[:provider[:model[:effort]]]]（可重复，最多 8 个）")
    sp.add_argument("--members-json", help="成员数组的 JSON（与 --member 叠加）")

    sp = add("group-get", "看群详情（含成员 id）", cmd_group_get)
    sp.add_argument("id")
    sp.add_argument("--transcript", action="store_true", help="附上群的 transcript 条目")

    sp = add("group-set", "改某个成员的 cli/provider/model/effort", cmd_group_set)
    sp.add_argument("id")
    sp.add_argument("member_id")
    sp.add_argument("--cli")
    sp.add_argument("--provider")
    sp.add_argument("--model")
    sp.add_argument("--effort")

    sp = add("group-send", "给群发消息（@成员 / @all）", cmd_group_send)
    sp.add_argument("id")
    sp.add_argument("text")

    sp = add("group-stop", "停止群本轮", cmd_group_stop)
    sp.add_argument("id")

    sp = add("group-rm", "删除群", cmd_group_rm)
    sp.add_argument("id")

    sp = add("group-tail", "跟随群 SSE 事件流", cmd_group_tail)
    sp.add_argument("id")
    sp.add_argument("--timeout", type=float, default=DEFAULT_TURN_TIMEOUT)

    add("notes", "全局备忘", cmd_notes)

    sp = add("notes-set", "覆盖写入全局备忘（JSON 数组，从 --file 或 stdin）", cmd_notes_set)
    sp.add_argument("--file")

    sp = add("stats", "使用统计", cmd_stats)
    sp.add_argument("--fresh", action="store_true")

    sp = add("raw", "直连任意接口（逃生舱）", cmd_raw)
    sp.add_argument("method")
    sp.add_argument("path")
    sp.add_argument("--data", help="JSON 请求体")
    sp.add_argument("--data-file")

    return p


def main(argv=None) -> int:
    # 行缓冲：tail/run 这类命令会持续往 stdout 写，块缓冲会让输出在管道里憋住，
    # 也把「跟随结束」这类 stderr 提示顶到内容前面。
    try:
        sys.stdout.reconfigure(line_buffering=True)
    except Exception:
        pass
    p = build_parser()
    # 用 parse_known_args 而不是 parse_args：argparse 对「nargs="*" 的位置参数 +
    # 其后还有选项」有已知限制——`reply SID --timeout 300 "文本"` 里那段文本会被
    # 判为无法识别。凡是带 prompt 的命令，把多出来的参数并回 prompt 即可。
    a, extra = p.parse_known_args(argv)
    if extra:
        if hasattr(a, "prompt"):
            if any(t.startswith("-") for t in extra):
                log("警告：把疑似选项的参数当成提示文本处理：" + " ".join(extra))
            a.prompt = list(a.prompt or []) + extra
        else:
            p.error("unrecognized arguments: " + " ".join(extra))
    if not getattr(a, "cmd", None):
        p.print_help()
        return 0
    ctx = resolve_ctx(a)
    try:
        return a.func(ctx, a)
    except KeyboardInterrupt:
        return 130
    except BrokenPipeError:
        # 下游（head / 管道）提前关闭。把 stdout 指向 devnull，否则解释器退出时
        # 还会再 flush 一次缓冲区，又抛一个「Exception ignored in ... BrokenPipeError」。
        try:
            os.dup2(os.open(os.devnull, os.O_WRONLY), sys.stdout.fileno())
        except Exception:
            pass
        return 0


if __name__ == "__main__":
    sys.exit(main())
