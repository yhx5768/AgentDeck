#!/usr/bin/env bash
# 造一张待回答的模型提问卡，用来肉眼验收「待回答」强提示（浮层/侧栏脉动/跳转）。
#
# 走的是 CLI hook 完全相同的那个端点：cc-webui -perm-helper 就是 POST 到
# /api/sessions/{id}/permissions/request 然后阻塞等决议。所以卡片、55s 倒计时、
# SSE 广播、决议回传、跨会话提示全都是真的，只是不跑真实模型、不花 token。
# 55s 没人答就自动拒绝（AskUserQuestion 那档回传「请稍后重新提问」，模型会重问）。
#
# 用法：
#   scripts/ask-demo.sh                 # 造一个新会话并发一张 AskUserQuestion
#   scripts/ask-demo.sh <session-id>    # 指定已有会话发（不新建）
#   scripts/ask-demo.sh <sid> Bash '{"command":"ls"}'   # 换成工具审批卡
#
# 环境：CC_WEBUI_BASE（默认 http://127.0.0.1:8321）
set -euo pipefail
cd "$(dirname "$0")/.."

BASE=${CC_WEBUI_BASE:-http://127.0.0.1:8321}
TOKEN=$(grep -m1 '^gateway_token:' config.yaml | sed 's/^gateway_token: *//' | tr -d "\"'")
[ -n "$TOKEN" ] || { echo "读不到 gateway_token（config.yaml）" >&2; exit 1; }

SID=${1:-}
TOOL=${2:-AskUserQuestion}
if [ -n "$SID" ]; then
  SID=$(curl -fsS "$BASE/api/sessions/$SID?token=$TOKEN" | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])')
else
  # 新建一个一次性会话：cli/provider 取配置里第一个 claude 对（跟手建会话同一条路）
  SID=$(python3 - "$BASE" "$TOKEN" <<'PY'
import json, sys, urllib.request, urllib.parse
base, token = sys.argv[1], sys.argv[2]
def get(path):
    with urllib.request.urlopen(f"{base}{path}{'&' if '?' in path else '?'}token={urllib.parse.quote(token)}") as r:
        return json.load(r)
clis = get("/api/clis")
provs = get("/api/providers")
cli = next((c["name"] for c in clis if (c.get("kind") or "claude") == "claude"), clis[0]["name"])
prov = next((p["name"] for p in provs if (p.get("kind") or "claude") == "claude"), provs[0]["name"])
body = json.dumps({"name": "ask-demo（看完可删）", "cli": cli, "provider": prov,
                   "cwd": "."}).encode()
req = urllib.request.Request(f"{base}/api/sessions?token={urllib.parse.quote(token)}", data=body,
                             headers={"Content-Type": "application/json"}, method="POST")
with urllib.request.urlopen(req) as r:
    print(json.load(r)["id"])
PY
  )
  echo "新建会话 $SID"
fi

if [ "$TOOL" = "AskUserQuestion" ]; then
  INPUT='{"questions":[{"header":"实现方式","question":"AskUserQuestion 这个卡片，你希望用哪种实现方式？","multiSelect":false,"options":[{"label":"方案 A：单文件","description":"改动最小，但后续维护会变难"},{"label":"方案 B：拆模块","description":"结构干净，要动几个文件"}]},{"header":"范围","question":"这次改动要连带覆盖哪些？","multiSelect":true,"options":[{"label":"后端"},{"label":"前端"},{"label":"测试"}]}]}'
else
  INPUT=${3:-'{"command":"ls -1","description":"AskUserQuestion 之外的工具审批卡，长这样"'}
fi

# 端点会阻塞到有人决议或 55s 超时，所以放后台，别把脚本挂死在这儿
curl -fsS -X POST "$BASE/api/sessions/$SID/permissions/request?token=$TOKEN" \
  -H 'Content-Type: application/json' \
  -d "$(python3 -c 'import json,sys;print(json.dumps({"tool":sys.argv[1],"input":json.loads(sys.argv[2])}))' "$TOOL" "$INPUT")" \
  >/dev/null 2>&1 &

# 等一拍让 pending 进注册表、SSE 广播出去，再报状态
sleep 1
curl -fsS "$BASE/api/sessions/$SID/permissions/pending?token=$TOKEN" |
  python3 -c 'import json,sys;p=json.load(sys.stdin)["pending"];print("已挂上待回答卡：", [(x["tool"], x["input"].get("questions",[{}])[0].get("question","")[:24]) for x in p] or "（没有，检查会话是否在跑）")'
echo "去 $BASE 看：顶部居中横幅 + 侧栏整行脉动。55s 后自动拒绝，再跑一次即可重放。"