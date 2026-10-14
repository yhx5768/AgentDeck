#!/usr/bin/env bash
# sync-opencode-models.sh — 把 config.yaml 里 kind: opencode 的 provider 的
# models[] 与 `opencode models <providerID> --verbose` 的上游清单对齐。
#
# 只做行级外科替换：重写目标 provider 的 models: 列表 + 刷新头注释里的日期，
# 其余字节（注释/token/其它 provider）原样保留。文件无变化时不落盘（mtime
# 不变，后端 config 热加载不会误触发）。
#
# 每小时由 cron 跑一次（见下方安装示例），后端 3s 轮询 config mtime 自动热加载，
# 无需重启服务：
#   11 * * * * /home/ubuntu/Tools/cc-webview/scripts/sync-opencode-models.sh \
#     >> /home/ubuntu/Tools/cc-webview/data/sync-models.log 2>&1
#
# 用法：
#   sync-opencode-models.sh [--config PATH] [--check]
#     --config PATH  指定 config.yaml（默认：脚本所在仓库根目录的 config.yaml）
#     --check        dry-run：只报告是否有差异，有差异 exit 2，无差异 exit 0
set -u
CONFIG=""
CHECK=0
while [ $# -gt 0 ]; do
  case "$1" in
    --config) CONFIG="$2"; shift 2 ;;
    --check) CHECK=1; shift ;;
    *) echo "unknown arg: $1" >&2; exit 1 ;;
  esac
done

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
if [ -z "$CONFIG" ]; then
  CONFIG="$REPO_DIR/config.yaml"
fi

exec python3 - "$CONFIG" "$CHECK" <<'PYEOF'
import json
import os
import re
import subprocess
import sys
from datetime import date

config_path, check = sys.argv[1], sys.argv[2] == "1"

# --- 1. 从 config.yaml 里找出 opencode 二进制 + 待同步的 provider 名 ---
# 只做行扫描（不做 YAML 解析），保注释。
with open(config_path) as f:
    lines = f.read().splitlines(keepends=True)

opencode_bins, opencode_providers = [], []
section, cur_name, cur_kind, cur_path = None, None, None, None

def flush_block():
    global cur_name, cur_kind, cur_path
    if section == "clis" and cur_kind == "opencode" and cur_path:
        opencode_bins.append(cur_path)
    if section == "providers" and cur_kind == "opencode" and cur_name:
        opencode_providers.append(cur_name)
    cur_name, cur_kind, cur_path = None, None, None

for line in lines:
    if re.match(r"^clis:\s*$", line):
        flush_block(); section = "clis"; continue
    if re.match(r"^providers:\s*$", line):
        flush_block(); section = "providers"; continue
    if re.match(r"^\S", line):
        flush_block(); section = None; continue
    if section not in ("clis", "providers"):
        continue
    m = re.match(r"^  - name:\s*\"([^\"]+)\"", line)
    if m:
        flush_block(); cur_name = m.group(1); continue
    m = re.match(r"^    kind:\s*\"([^\"]+)\"", line)
    if m:
        cur_kind = m.group(1); continue
    m = re.match(r"^    path:\s*\"([^\"]+)\"", line)
    if m:
        cur_path = m.group(1); continue
flush_block()

def resolve_cli():
    for p in opencode_bins:
        if os.path.isfile(p) and os.access(p, os.X_OK):
            return p
    cand = os.path.expanduser("~/.opencode/bin/opencode")
    if os.path.isfile(cand) and os.access(cand, os.X_OK):
        return cand
    for d in os.environ.get("PATH", "").split(os.pathsep):
        cand = os.path.join(d, "opencode")
        if os.path.isfile(cand) and os.access(cand, os.X_OK):
            return cand
    return None

cli = resolve_cli()
if not cli:
    print("sync-opencode-models: no opencode binary found", file=sys.stderr)
    sys.exit(1)
if not opencode_providers:
    print("sync-opencode-models: no kind:opencode providers in config, nothing to do")
    sys.exit(0)

# --- 2. 拉上游清单（顺序即 CLI 原序，image 取 capabilities.input.image） ---
def fetch(provider):
    raw = subprocess.run([cli, "models", provider, "--verbose"],
                         capture_output=True, text=True, timeout=120)
    if raw.returncode != 0:
        raise RuntimeError(f"`opencode models {provider} --verbose` exit "
                           f"{raw.returncode}: {raw.stderr.strip()[:300]}")
    models = []  # [(model_id, image_bool)]
    cur, buf = None, []
    def flush():
        if not cur or not buf:
            return
        try:
            meta = json.loads("\n".join(buf))
            img = bool(meta.get("capabilities", {}).get("input", {}).get("image", False))
        except json.JSONDecodeError:
            return  # 版式漂移：跳过该块，保其余
        models.append((cur.split("/", 1)[1], img))
    for rl in raw.stdout.splitlines():
        if rl and not rl.startswith((" ", "{", "}")) and "/" in rl:
            flush(); cur, buf = rl.strip(), []
        else:
            buf.append(rl)
    flush()
    if not models:
        raise RuntimeError(f"`opencode models {provider} --verbose` parsed 0 models, refuse to wipe list")
    return models

# --- 3. 外科替换 models: 列表 ---
def build_block(models):
    out = ["    models:\n"]
    for mid, img in models:
        out.append(f"      - name: \"{mid}\"\n")
        if img:
            out.append("        input: [text, image]\n")
        # 纯文本模型不写 input 行（与现有风格一致：无 input 即默认纯文本）
    return out

pat_item = re.compile(r"^  - name:\s*\"([^\"]+)\"")

def replace_models(src, provider, models):
    """返回 (new_lines, old_ids, changed)。找不到 provider/models 段则抛错。"""
    out, i, n = [], 0, len(src)
    # provider 头只在 providers: 段内找（cli 里可能有重名，如 "opencode"）
    prov_sec = next(idx for idx, line in enumerate(src)
                    if re.match(r"^providers:\s*$", line))
    head = None
    for idx in range(prov_sec + 1, n):
        m = pat_item.match(src[idx])
        if m and m.group(1) == provider:
            head = idx
            break
    if head is None:
        raise RuntimeError(f"provider {provider!r} not found in {config_path}")
    # provider 块止于下一个同级 "- name:" 或顶层 key
    end = n
    for idx in range(head + 1, n):
        if re.match(r"^  - name:", src[idx]) or re.match(r"^\S", src[idx]):
            end = idx
            break
    # 块内找 models:
    mline = None
    for idx in range(head, end):
        if re.match(r"^    models:", src[idx]):
            mline = idx
            break
    if mline is None:
        raise RuntimeError(f"models: not found under provider {provider!r}")
    old_ids = []
    if re.match(r"^    models:\s*\[\]\s*$", src[mline]):
        mend = mline + 1
    else:
        old_ids = re.findall(r"^      - name:\s*\"([^\"]+)\"", "".join(src[mline:end]), re.M)
        mend = end
        for idx in range(mline + 1, end):
            if re.match(r"^      - name:", src[idx]) or re.match(r"^        \w", src[idx]):
                continue
            mend = idx
            break
    new = src[:mline] + build_block(models) + src[mend:]
    return new, old_ids, [m for m, _ in models] != old_ids

overall_changed = False
new_lines = list(lines)
for prov in opencode_providers:
    try:
        upstream = fetch(prov)
    except Exception as e:
        print(f"sync-opencode-models: [{prov}] SKIP ({e})", file=sys.stderr)
        continue
    try:
        new_lines, old_ids, changed = replace_models(new_lines, prov, upstream)
    except Exception as e:
        print(f"sync-opencode-models: [{prov}] SKIP ({e})", file=sys.stderr)
        continue
    old, new = set(old_ids), set(m for m, _ in upstream)
    if changed:
        overall_changed = True
        print(f"sync-opencode-models: [{prov}] {len(old_ids)} -> {len(upstream)} "
              f"models (+{sorted(new - old) or []} -{sorted(old - new) or []})")
    else:
        print(f"sync-opencode-models: [{prov}] up to date ({len(upstream)} models)")

if not overall_changed:
    sys.exit(0)

# 刷新头注释里的日期（只动这一行其余不动）
today = date.today().isoformat()
dated = False
for idx, line in enumerate(new_lines):
    if "全量模型用 `opencode models` 查（" in line:
        new_lines[idx] = re.sub(r"查（\d{4}-\d{2}-\d{2}", f"查（{today}", line)
        dated = True
        break

if check:
    print(f"sync-opencode-models: --check: changes pending (dated line: {dated})")
    sys.exit(2)

# 落盘前自检：别把文件写坏
text = "".join(new_lines)
assert "\nproviders:\n" in text and "\nclis:\n" in text, "sanity: top-level sections missing"
for prov in opencode_providers:
    assert f'- name: "{prov}"' in text, f"sanity: provider {prov} missing"
tmp = config_path + ".tmp-sync-models"
with open(tmp, "w") as f:
    f.write(text)
os.chmod(tmp, 0o600)
os.replace(tmp, config_path)
print(f"sync-opencode-models: wrote {config_path}")
PYEOF
