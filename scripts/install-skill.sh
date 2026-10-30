#!/usr/bin/env bash
# 把仓库里的 AgentDeck skill 安装到各 Agent 的 skill 目录。
#
#   ./scripts/install-skill.sh                 # 软链到 ~/.claude/skills/agentdeck
#   ./scripts/install-skill.sh --opencode      # 同时软链到 ~/.config/opencode/skills/agentdeck
#   ./scripts/install-skill.sh --copy          # 复制而不是软链（跨机器/只读仓库时用）
#   ./scripts/install-skill.sh --target DIR    # 指定安装目录（DIR/agentdeck）
#   ./scripts/install-skill.sh --uninstall     # 卸载
#   ./scripts/install-skill.sh --force         # 覆盖已存在的真实目录
#
# Claude Code 读 ~/.claude/skills；opencode 读 ~/.config/opencode/skills，并且兼容
# ~/.claude/skills（所以只装 ~/.claude/skills 通常就够 claude + opencode 两边用）。
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SRC="$REPO_ROOT/skills/agentdeck"
NAME="agentdeck"

mode="link"
targets=()
explicit_target=0
uninstall=0
force=0

while [ $# -gt 0 ]; do
  case "$1" in
    --copy) mode="copy" ;;
    --link) mode="link" ;;
    --opencode) targets+=("$HOME/.config/opencode/skills") ;;
    --target) shift; targets+=("$1"); explicit_target=1 ;;
    --uninstall) uninstall=1 ;;
    --force) force=1 ;;
    -h|--help) sed -n '2,15p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "未知参数：$1（-h 看用法）" >&2; exit 2 ;;
  esac
  shift
done

# --opencode / --target 都是「追加」；没显式指定过目录时才带上默认的 ~/.claude/skills。
if [ "$explicit_target" = "0" ]; then
  targets=("$HOME/.claude/skills" "${targets[@]+"${targets[@]}"}")
fi

if [ ! -f "$SRC/SKILL.md" ]; then
  echo "找不到 skill 源：$SRC/SKILL.md" >&2
  exit 1
fi

for dir in "${targets[@]}"; do
  dest="$dir/$NAME"
  if [ "$uninstall" = "1" ]; then
    if [ -e "$dest" ] || [ -L "$dest" ]; then
      rm -rf "$dest"
      echo "已卸载 $dest"
    else
      echo "未安装：$dest"
    fi
    continue
  fi

  mkdir -p "$dir"

  # 已经是正确的软链：幂等退出。
  if [ -L "$dest" ] && [ "$(readlink "$dest")" = "$SRC" ]; then
    echo "已是最新（软链）：$dest"
    continue
  fi

  if [ -e "$dest" ] || [ -L "$dest" ]; then
    if [ "$mode" = "link" ] && [ -L "$dest" ]; then
      rm -f "$dest"                     # 指向别处的旧软链，直接换掉
    elif [ "$force" = "1" ]; then
      rm -rf "$dest"
    else
      echo "已存在且不是本仓库的软链：$dest" >&2
      echo "  用 --force 覆盖，或先手动删掉。" >&2
      exit 1
    fi
  fi

  if [ "$mode" = "link" ]; then
    ln -s "$SRC" "$dest"
    echo "已软链：$dest -> $SRC"
  else
    cp -R "$SRC" "$dest"
    echo "已复制：$dest"
  fi
done

echo
echo "完成。skill 会被自动发现（opencode 改完配置可 /reload；Claude Code 新开会话即生效）。"
echo "自检：python3 \"${targets[0]}/$NAME/scripts/agentdeck.py\" doctor"
