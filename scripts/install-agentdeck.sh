#!/usr/bin/env bash
# AgentDeck 一键安装 / 升级（Linux / macOS；默认取最新 release）
# Release: https://github.com/yhx5768/AgentDeck/releases/latest
# 二进制使用新名 agentdeck；旧的 ~/AgentDeck/cc-webui 会迁移并保留备份。
# 无法检查最新版本时回退 v0.6.0。
#
# 本文件的权威副本就在仓库里（scripts/install-agentdeck.sh），对外分发用 raw 链接：
#   curl -fsSL https://raw.githubusercontent.com/yhx5768/AgentDeck/master/scripts/install-agentdeck.sh | bash
# 改脚本请直接改这里并推 master，别再往别的镜像上传副本。
set -euo pipefail

INSTALL_DIR="$HOME/AgentDeck"
BIN="$INSTALL_DIR/agentdeck"
LEGACY_BIN="$INSTALL_DIR/cc-webui"
CONFIG="$INSTALL_DIR/config.yaml"
LOG="$INSTALL_DIR/agentdeck.log"
PID_FILE="$INSTALL_DIR/agentdeck.pid"

VERSION="v0.6.0"
RELEASE_BASE="https://github.com/yhx5768/AgentDeck/releases/download"
LATEST_URL="https://github.com/yhx5768/AgentDeck/releases/latest"
# 仓库 raw 前缀：配置模板与本脚本都从这里取，随时与 master 同步。
RAW_BASE="https://raw.githubusercontent.com/yhx5768/AgentDeck/master"
CONFIG_URL="$RAW_BASE/config.example.yaml"

echo "默认安装目录：~/AgentDeck"
echo "脚本会自动探测 claude / codex / opencode 并写进配置；只装了一个也可以，找不到的可以直接回车跳过。"
echo "全新安装默认取 GitHub 最新 release；重复执行同一命令即可升级。"

command -v curl >/dev/null 2>&1 || { echo "需要 curl，请先安装"; exit 1; }

# curl | bash 时 stdin 是脚本管道，交互输入必须从 /dev/tty 读。
if [ -t 0 ]; then
  TTY_IN=""
elif { : < /dev/tty; } 2>/dev/null; then
  TTY_IN="/dev/tty"
else
  echo "错误：没有可交互的终端（stdin 不是 TTY，且 /dev/tty 不可用）。" >&2
  echo "请先下载脚本再执行：curl -fsSL ${RAW_BASE}/scripts/install-agentdeck.sh -o install-agentdeck.sh && bash install-agentdeck.sh" >&2
  exit 1
fi

# read_line "<提示>" <变量名> [silent]
read_line() {
  local prompt="$1" var="$2" silent="${3:-}" line=""
  printf '%s' "$prompt" >&2
  if [ "$silent" = "silent" ]; then
    if [ -n "$TTY_IN" ]; then
      IFS= read -r -s line < "$TTY_IN" || { printf '\n' >&2; echo "读取输入失败（EOF）" >&2; exit 1; }
    else
      IFS= read -r -s line || { printf '\n' >&2; echo "读取输入失败（EOF）" >&2; exit 1; }
    fi
  else
    if [ -n "$TTY_IN" ]; then
      IFS= read -r line < "$TTY_IN" || { echo; echo "读取输入失败（EOF）" >&2; exit 1; }
    else
      IFS= read -r line || { echo; echo "读取输入失败（EOF）" >&2; exit 1; }
    fi
  fi
  printf -v "$var" '%s' "$line"
}

# Like read_line, but EOF means "skip" rather than aborting the whole install.
read_line_soft() {
  local prompt="$1" var="$2" line=""
  printf '%s' "$prompt" >&2
  if [ -n "$TTY_IN" ]; then
    IFS= read -r line < "$TTY_IN" || { echo; return 1; }
  else
    IFS= read -r line || { echo; return 1; }
  fi
  printf -v "$var" '%s' "$line"
  return 0
}

# 1. 识别系统和架构
case "$(uname -s)" in
  Linux)  os="linux" ;;
  Darwin) os="darwin" ;;
  *) echo "不支持的系统：$(uname -s)"; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch="amd64" ;;
  arm64|aarch64) arch="arm64" ;;
  *) echo "不支持的架构：$(uname -m)"; exit 1 ;;
esac
asset="agentdeck-${os}-${arch}"
legacy_asset="cc-webui-${os}-${arch}"

FIND_PATH=""

# 端口探测：优先 ss（Linux）/ lsof（macOS），不可用时用 bash /dev/tcp 兜底。
port_is_busy() {
  local p="$1"
  if [ "$os" = "linux" ] && command -v ss >/dev/null 2>&1; then
    local ssout
    if ssout="$(ss -ltn 2>/dev/null)"; then
      if printf '%s\n' "$ssout" | awk 'NR > 1 {print $4}' | grep -qE "[:.]${p}$"; then
        return 0
      fi
      return 1
    fi
  fi
  if command -v lsof >/dev/null 2>&1; then
    if lsof -nP -iTCP:"$p" -sTCP:LISTEN >/dev/null 2>&1; then
      return 0
    fi
  fi
  if (exec 3<>"/dev/tcp/127.0.0.1/$p") 2>/dev/null; then
    return 0
  fi
  return 1
}

find_free_port() {
  local p="$1" end="$2"
  while [ "$p" -le "$end" ]; do
    if ! port_is_busy "$p"; then
      printf '%s\n' "$p"
      return 0
    fi
    p=$((p + 1))
  done
  return 1
}

# 当前实际安装的二进制：新名优先，兼容旧名 cc-webui。
installed_bin() {
  if [ -x "$BIN" ]; then printf '%s\n' "$BIN"; return 0; fi
  if [ -x "$LEGACY_BIN" ]; then printf '%s\n' "$LEGACY_BIN"; return 0; fi
  return 1
}

# 版本：current_version / latest_version / version_gt
current_version() {
  local bin out
  bin="$(installed_bin)" || return 0
  out="$("$bin" -version 2>/dev/null || true)"
  printf '%s\n' "$out" | grep -oE 'v[0-9]+(\.[0-9]+)+' | head -n1 || true
}

latest_version() {
  local url tag
  url="$(curl -fsSL -o /dev/null -w '%{url_effective}' "$LATEST_URL" 2>/dev/null || true)"
  tag="${url##*/}"
  case "$tag" in
    v[0-9]*) printf '%s\n' "$tag" ;;
    *) return 1 ;;
  esac
}

version_gt() {
  local a="${1#v}" b="${2#v}" i ai bi
  local IFS=.
  local -a av bv
  # shellcheck disable=SC2206
  av=($a)
  # shellcheck disable=SC2206
  bv=($b)
  for i in 0 1 2; do
    ai="${av[$i]:-0}"
    bi="${bv[$i]:-0}"
    case "$ai" in ''|*[!0-9]*) return 1 ;; esac
    case "$bi" in ''|*[!0-9]*) return 1 ;; esac
    if [ "$ai" -gt "$bi" ]; then return 0; fi
    if [ "$ai" -lt "$bi" ]; then return 1; fi
  done
  return 1
}

# 配置 / 二进制 / 进程
download_config() {
  echo "下载配置模板 ..."
  if curl -fL --retry 3 --connect-timeout 15 -o config.yaml.tmp "$CONFIG_URL"; then
    mv -f config.yaml.tmp config.yaml
    cp config.yaml config.yaml.bak
    echo "配置模板来源: $CONFIG_URL"
    return 0
  fi
  rm -f config.yaml.tmp
  echo "错误：配置模板下载失败（$CONFIG_URL 不可用）。" >&2
  echo "请手动下载 config.example.yaml 为 config.yaml 后重试。" >&2
  return 1
}

download_binary() {
  local dest="$1" ver="${2:-$VERSION}" name
  for name in "$asset" "$legacy_asset"; do
    echo "下载 ${name} (${ver}) ..."
    if curl -fL --retry 3 --connect-timeout 15 -o "$dest" "${RELEASE_BASE}/${ver}/${name}"; then
      chmod +x "$dest"
      return 0
    fi
    rm -f "$dest"
    if [ "$name" = "$asset" ]; then
      echo "未找到新命名 asset，回退旧名 ${legacy_asset} ..." >&2
    fi
  done
  echo "下载失败：${ver} 同时缺少 ${asset} 和 ${legacy_asset}" >&2
  return 1
}

backup_config() {
  local ts bak
  ts="$(date +%Y%m%d-%H%M%S)"
  bak="config.yaml.bak-$ts"
  if [ -f config.yaml ]; then
    cp -p config.yaml "$bak"
    echo "已备份配置: $bak"
  fi
}

config_listen() {
  sed -nE 's/^listen:[[:space:]]*"?([^"#]*)"?.*/\1/p' config.yaml 2>/dev/null | head -n1 | tr -d '[:space:]'
}

config_port() {
  local listen
  listen="$(config_listen)"
  [ -n "$listen" ] || listen="0.0.0.0:8321"
  printf '%s\n' "${listen##*:}"
}

access_url_from_config() {
  local listen host port
  listen="$(config_listen)"
  [ -n "$listen" ] || listen="0.0.0.0:8321"
  host="${listen%:*}"
  port="${listen##*:}"
  case "$host" in
    127.0.0.1|localhost) printf 'http://%s:%s\n' "$host" "$port" ;;
    0.0.0.0|"") printf 'http://<本机内网IP>:%s（本机可用 http://127.0.0.1:%s）\n' "$port" "$port" ;;
    *) printf 'http://%s:%s\n' "$host" "$port" ;;
  esac
}

pid_cwd() {
  local pid="$1"
  if [ -d "/proc/$pid" ]; then
    readlink "/proc/$pid/cwd" 2>/dev/null || true
  elif command -v lsof >/dev/null 2>&1; then
    lsof -a -p "$pid" -d cwd -Fn 2>/dev/null | sed -n 's/^n//p' | head -n1
  fi
}

stop_agentdeck() {
  local pids="" p cwd i alive
  for p in $(pgrep -f "agentdeck -config" 2>/dev/null || true) $(pgrep -f "cc-webui -config" 2>/dev/null || true); do
    cwd="$(pid_cwd "$p")"
    if [ "$cwd" = "$INSTALL_DIR" ]; then
      pids="$pids $p"
    fi
  done
  for p in $(pgrep -f "$BIN" 2>/dev/null || true) $(pgrep -f "$LEGACY_BIN" 2>/dev/null || true); do
    case " $pids " in
      *" $p "*) ;;
      *) pids="$pids $p" ;;
    esac
  done
  if [ -z "$pids" ]; then
    echo "未发现正在运行的旧进程"
    rm -f "$PID_FILE"
    return 0
  fi
  echo "停止旧进程:$pids"
  kill $pids 2>/dev/null || true
  i=0
  while [ "$i" -lt 20 ]; do
    alive=0
    for p in $pids; do
      if kill -0 "$p" 2>/dev/null; then alive=1; fi
    done
    [ "$alive" -eq 0 ] && break
    sleep 0.5
    i=$((i + 1))
  done
  for p in $pids; do
    if kill -0 "$p" 2>/dev/null; then
      echo "进程 $p 未退出，强制停止" >&2
      kill -9 "$p" 2>/dev/null || true
    fi
  done
  rm -f "$PID_FILE"
}

start_agentdeck() {
  local url="${1:-}" verify_port="${2:-}" pid bin
  bin="$(installed_bin || true)"
  [ -n "$bin" ] || bin="$BIN"
  [ -n "$url" ] || url="$(access_url_from_config)"
  [ -n "$verify_port" ] || verify_port="$(config_port)"
  echo "启动 AgentDeck ..."
  (
    cd "$INSTALL_DIR"
    nohup "$bin" -config config.yaml > "$LOG" 2>&1 &
    echo $! > "$PID_FILE"
  )
  pid="$(cat "$PID_FILE" 2>/dev/null || true)"
  sleep 1
  if [ -z "$pid" ] || ! kill -0 "$pid" 2>/dev/null; then
    echo "启动失败，最近日志：" >&2
    tail -n 20 "$LOG" >&2 || true
    rm -f "$PID_FILE"
    return 1
  fi
  echo "AgentDeck 已启动"
  echo "  PID : $pid"
  echo "  访问: $url"
  echo "  日志: $LOG"
  echo "  停止: kill $pid"
  echo "  验证（登录页返回 200 即 HTTP 服务正常）："
  curl -fsS -o /dev/null -w '  HTTP %{http_code}\n' "http://127.0.0.1:$verify_port/login.html" || true
  return 0
}

# sed 辅助（兼容 GNU sed 和 macOS BSD sed）
sed_escape() {
  printf '%s' "$1" | sed -e 's/[\\&|]/\\&/g'
}
if [ "$os" = "darwin" ]; then
  sed_inplace() { sed -i '' "$@"; }
else
  sed_inplace() { sed -i "$@"; }
fi

# 探测原生 CLI：claude / codex / opencode。
# 这份脚本面向自己装 CLI 的用户，只探测官方命令；二次封装（wrapper）请自己写进 config.yaml。
CLI_NAMES="claude codex opencode"

# config.example.yaml 里 clis[] 的条目名，只有 claude 的条目叫 official-claude。
cli_config_name() {
  case "$1" in
    claude) printf '%s\n' "official-claude" ;;
    *) printf '%s\n' "$1" ;;
  esac
}

# CLI 常见安装位置：当前 shell 的 PATH 里没有时（非交互式 shell、刚装完还没重开终端）兜底。
cli_candidates() {
  case "$1" in
    claude)
      printf '%s\n' \
        "$HOME/.claude/local/claude" \
        "$HOME/.local/bin/claude" \
        "/opt/homebrew/bin/claude" \
        "/usr/local/bin/claude"
      ;;
    codex)
      printf '%s\n' \
        "$HOME/.local/bin/codex" \
        "$HOME/.npm-global/bin/codex" \
        "/opt/homebrew/bin/codex" \
        "/usr/local/bin/codex"
      ;;
    opencode)
      printf '%s\n' \
        "$HOME/.opencode/bin/opencode" \
        "$HOME/.local/bin/opencode" \
        "/opt/homebrew/bin/opencode" \
        "/usr/local/bin/opencode"
      ;;
  esac
}

# 找到可执行文件则写进 FIND_PATH：先查 PATH，再查常见路径，最后交互式问一个绝对路径。
# 故意不解析符号链接：Homebrew 的 Cellar 路径会随升级变化，保留 /opt/homebrew/bin/xxx
# 这类稳定入口才能在升级后继续可用。
find_cli() {
  local name="$1" p
  FIND_PATH=""
  p="$(command -v "$name" 2>/dev/null || true)"
  case "$p" in
    /*) if [ -x "$p" ]; then FIND_PATH="$p"; return 0; fi ;;
  esac
  while IFS= read -r p; do
    [ -n "$p" ] || continue
    if [ -x "$p" ]; then
      FIND_PATH="$p"
      return 0
    fi
  done <<EOF
$(cli_candidates "$name")
EOF
  while true; do
    if ! read_line_soft "未找到 ${name}，可回车跳过（之后在 config.yaml 的 clis 里补），或直接输入绝对路径: " p; then
      echo "未找到 ${name}，已跳过（无交互输入）；之后可在 config.yaml 的 clis 里补" >&2
      return 1
    fi
    if [ -z "$p" ]; then
      return 1
    fi
    case "$p" in
      "~") p="$HOME" ;;
      "~/"*) p="$HOME/${p#~/}" ;;
    esac
    if [ -x "$p" ]; then
      FIND_PATH="$p"
      return 0
    fi
    echo "$name 路径不存在或不可执行：${p}，请重试（直接回车跳过）" >&2
  done
}

# config.yaml 的 clis[] 里是否有某个条目（按 name 匹配）。
config_has_cli() {
  awk -v name="$1" '
    /^clis:[[:space:]]*$/ { in_clis = 1; next }
    in_clis && /^[^[:space:]#]/ { exit }
    in_clis && /^  - name:[[:space:]]*"/ && $0 ~ ("^  - name:[[:space:]]*\"" name "\"") { found = 1; exit }
    END { exit(found ? 0 : 1) }
  ' config.yaml 2>/dev/null
}

# 读某个 cli 条目的 path（去掉引号与行尾注释）。
config_cli_path() {
  awk -v name="$1" '
    /^clis:[[:space:]]*$/ { in_clis = 1; next }
    in_clis && /^[^[:space:]#]/ { exit }
    in_clis && /^  - name:[[:space:]]*"/ { hit = ($0 ~ ("^  - name:[[:space:]]*\"" name "\"")) }
    in_clis && hit && /^    path:/ {
      line = $0
      sub(/^    path:[[:space:]]*/, "", line)
      sub(/[[:space:]]*#.*$/, "", line)
      gsub(/"/, "", line)
      print line
      exit
    }
  ' config.yaml 2>/dev/null
}

# 在 config.yaml 的 clis[] 里按条目名改 path。
# 只在 clis: 段落内生效，遇到行首非空格的顶层键就收工，不会误伤 providers。
#   set_cli_path <条目名> <path>
set_cli_path() {
  local name="$1" path="$2" tmp="config.yaml.tmp"
  awk -v name="$name" -v path="$path" '
    /^clis:[[:space:]]*$/ { in_clis = 1; print; next }
    in_clis && /^[^[:space:]#]/ { in_clis = 0; hit = 0 }
    in_clis && /^  - name:[[:space:]]*"/ {
      hit = ($0 ~ ("^  - name:[[:space:]]*\"" name "\""))
      print
      next
    }
    in_clis && hit && /^    path:/ {
      printf "    path: \"%s\"\n", path
      next
    }
    { print }
  ' config.yaml > "$tmp" && mv -f "$tmp" config.yaml
}

# 把探测结果写进 config.yaml：找到 → 绝对路径；没找到 → 裸命令名，
# 之后用 PATH 装上就能跑，不用再改配置。
apply_cli_path() {
  local name="$1" path="$2" entry
  entry="$(cli_config_name "$name")"
  [ -n "$path" ] || path="$name"
  if config_has_cli "$entry"; then
    set_cli_path "$entry" "$path"
  else
    echo "提示：config 模板里没有 ${entry} 条目，跳过（可自己往 clis[] 里加一条）" >&2
  fi
}

# 冒烟检查：版本号能打印出来，说明二进制至少能跑通。
# 这里必须有硬超时：有些 launcher 型 CLI（内部 wrapper、还没登录的二进制）会一直等输入，
# 没有超时就会把安装卡死在那儿。macOS 默认没有 timeout/gtimeout，所以兜底自己实现，
# 而且**杀进程组**——wrapper 派生的子进程只杀父进程是清不掉的。
run_with_timeout() {
  local secs="$1"; shift
  if command -v timeout >/dev/null 2>&1; then
    timeout "$secs" "$@"
    return $?
  fi
  if command -v gtimeout >/dev/null 2>&1; then
    gtimeout "$secs" "$@"
    return $?
  fi
  local pid rc i deadline
  set -m 2>/dev/null || true
  "$@" &
  pid=$!
  set +m 2>/dev/null || true
  i=0
  deadline=$((secs * 2))
  while [ "$i" -lt "$deadline" ] && kill -0 "$pid" 2>/dev/null; do
    sleep 0.5
    i=$((i + 1))
  done
  if kill -0 "$pid" 2>/dev/null; then
    kill -TERM "-$pid" 2>/dev/null || kill -TERM "$pid" 2>/dev/null || true
    sleep 0.5
    kill -KILL "-$pid" 2>/dev/null || kill -KILL "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null
    return 124
  fi
  wait "$pid" 2>/dev/null
  rc=$?
  return "$rc"
}

smoke_check_cli() {
  local name="$1" path="$2"
  [ -n "$path" ] || return 0
  case "$path" in
    /*) ;;
    *) path="$(command -v "$path" 2>/dev/null || true)" ;;
  esac
  [ -n "$path" ] || return 0
  if [ ! -x "$path" ]; then
    echo "警告：${name} 路径不存在或不可执行：$path" >&2
    return 0
  fi
  echo "冒烟检查 ${name} --version: $path"
  if run_with_timeout 10 "$path" --version >/dev/null 2>&1; then
    echo "  ${name} 冒烟检查通过"
  else
    echo "警告：${name} 冒烟检查没跑通（超时 / 二进制或运行时有问题 / 未登录）。" >&2
    echo "      确认能手动跑通 ${name} --version 后再重启 AgentDeck。" >&2
  fi
  return 0
}

smoke_check_clis_from_config() {
  local name path
  for name in $CLI_NAMES; do
    path="$(config_cli_path "$(cli_config_name "$name")")"
    [ -n "$path" ] || continue
    smoke_check_cli "$name" "$path"
  done
}

# 配置向导：端口探测 + 监听地址 + token + 原生 CLI 探测 + sed 修改 config.yaml
run_config_wizard() {
  port=8321
  if port_is_busy "$port"; then
    newport="$(find_free_port 8322 8399 || true)"
    if [ -z "$newport" ]; then
      echo "8321 已被占用，且 8322-8399 没有空闲端口。" >&2
      exit 1
    fi
    echo "8321 已被占用，改用 $newport"
    port="$newport"
  fi

  echo
  echo "选择监听地址（端口 ${port}）："
  echo "  1) 127.0.0.1:$port  仅本机访问（推荐）"
  echo "  2) 0.0.0.0:$port    内网其他机器可访问"
  echo "  3) 自定义端口（默认 ${port}）"
  read_line "输入 1、2 或 3 [1]: " listen_choice
  listen_choice="${listen_choice:-1}"
  case "$listen_choice" in
    1)
      listen_addr="127.0.0.1:$port"
      access_url="http://127.0.0.1:$port"
      ;;
    2)
      listen_addr="0.0.0.0:$port"
      access_url="http://<本机内网IP>:${port}（本机可用 http://127.0.0.1:${port}）"
      ;;
    3)
      read_line "输入端口（默认 ${port}）: " custom_port
      custom_port="${custom_port:-$port}"
      case "$custom_port" in
        ''|*[!0-9]*)
          echo "无效端口：$custom_port"
          exit 1
          ;;
      esac
      if [ "$custom_port" -lt 1 ] || [ "$custom_port" -gt 65535 ]; then
        echo "端口超出范围（1-65535）：$custom_port"
        exit 1
      fi
      port="$custom_port"
      if port_is_busy "$port"; then
        newport="$(find_free_port $((port + 1)) 65535 || true)"
        if [ -z "$newport" ]; then
          echo "端口 $port 已被占用，且后面没有空闲端口。" >&2
          exit 1
        fi
        echo "端口 $port 已被占用，改用 $newport"
        port="$newport"
      fi
      read_line "监听 127.0.0.1 还是 0.0.0.0？输入 1 或 2 [1]: " bind_choice
      bind_choice="${bind_choice:-1}"
      case "$bind_choice" in
        1) listen_addr="127.0.0.1:$port"; access_url="http://127.0.0.1:$port" ;;
        2) listen_addr="0.0.0.0:${port}"; access_url="http://<本机内网IP>:${port}（本机可用 http://127.0.0.1:${port}）" ;;
        *) echo "无效选择：$bind_choice"; exit 1 ;;
      esac
      ;;
    *)
      echo "无效选择：$listen_choice"
      exit 1
      ;;
  esac

  # token（不回显）
  while true; do
    read_line "设置 gateway_token（输入不回显）: " token silent
    printf '\n' >&2
    if [ -n "$token" ]; then break; fi
    echo "token 不能为空，请重新输入。"
  done

  # 原生 CLI 探测：claude / codex / opencode。
  # 只写这三个的 path；二次封装（wrapper）不在这份脚本的范围内。
  echo
  echo "探测本机 CLI（claude / codex / opencode）："
  claude_path=""
  codex_path=""
  opencode_path=""
  found_any=0
  for name in $CLI_NAMES; do
    if find_cli "$name"; then
      case "$name" in
        claude) claude_path="$FIND_PATH" ;;
        codex) codex_path="$FIND_PATH" ;;
        opencode) opencode_path="$FIND_PATH" ;;
      esac
      found_any=1
      echo "  已找到 $name: $FIND_PATH"
    else
      echo "  未找到 $name（config 里留占位，之后装上就能用）" >&2
    fi
  done

  if [ "$found_any" -eq 0 ]; then
    echo "错误：claude / codex / opencode 都没找到，无法继续。" >&2
    echo "AgentDeck 是这些 CLI 的控制层，请先装其中一个；或在 config.yaml 的 clis 里手动填 path 后自行启动。" >&2
    exit 1
  fi

  listen_esc="$(sed_escape "$listen_addr")"
  token_esc="$(sed_escape "$token")"
  sed_inplace -e "s|^listen:.*|listen: \"${listen_esc}\"|" config.yaml
  sed_inplace -e "s|^gateway_token:.*|gateway_token: \"${token_esc}\"|" config.yaml

  apply_cli_path claude "$claude_path"
  apply_cli_path codex "$codex_path"
  apply_cli_path opencode "$opencode_path"

  echo
  echo "已更新配置（token 不打印）："
  grep -nE '^listen:' config.yaml
  awk '
    /^clis:[[:space:]]*$/ { in_clis = 1; next }
    in_clis && /^[^[:space:]#]/ { exit }
    in_clis && /^  - name:/ { line = $0; sub(/^  - name:[[:space:]]*/, "", line); gsub(/"/, "", line); name = line }
    in_clis && /^    path:/ { p = $0; sub(/^    path:[[:space:]]*/, "", p); printf "  cli %-16s %s\n", name, p }
  ' config.yaml
}

reconfigure() {
  if [ ! -f config.yaml ]; then
    echo "未找到 config.yaml，重新下载模板 ..."
    download_config
  fi
  backup_config
  stop_agentdeck
  if [ ! -x "$BIN" ] && [ -x "$LEGACY_BIN" ]; then
    cur="$(current_version)"
    [ -n "$cur" ] || cur="unknown"
    cp -p "$LEGACY_BIN" "$INSTALL_DIR/cc-webui.bak-${cur}"
    mv "$LEGACY_BIN" "$BIN"
    chmod +x "$BIN"
    ln -sfn "agentdeck" "$LEGACY_BIN"
    echo "已将旧二进制迁移为 agentdeck（备份 cc-webui.bak-${cur}）"
  fi
  run_config_wizard
  if ! start_agentdeck "$access_url" "$port"; then
    echo "配置后启动失败，请检查 config.yaml 或日志。" >&2
    exit 1
  fi
  smoke_check_clis_from_config
  exit 0
}

# 主流程
mkdir -p "$INSTALL_DIR"
cd "$INSTALL_DIR"

if [ -x "$BIN" ] || [ -x "$LEGACY_BIN" ]; then
  current_bin="$(installed_bin || true)"
  current="$(current_version)"
  [ -n "$current" ] || current="unknown"
  echo "当前版本: $current"

  if ! latest="$(latest_version)"; then
    latest=""
    echo "无法检查最新版本"
    read_line "是否继续重新走一遍配置？[y/N]: " reconf
    case "$reconf" in
      y|Y) reconfigure ;;
      *) echo "保持现有配置，退出"; exit 0 ;;
    esac
  fi

  echo "最新版本: $latest"

  if version_gt "$latest" "$current"; then
    echo "发现新版本: $current -> $latest"
    download_binary "$INSTALL_DIR/agentdeck.new" "$latest"
    stop_agentdeck
    bak="$INSTALL_DIR/cc-webui.bak-${current}"
    cp -p "$current_bin" "$bak"
    for f in "$INSTALL_DIR"/cc-webui.bak-*; do
      [ "$f" = "$bak" ] || rm -f "$f"
    done
    mv -f "$INSTALL_DIR/agentdeck.new" "$BIN"
    chmod +x "$BIN"
    # 保留旧命令名：指向新二进制，旧脚本/习惯命令继续可用。
    ln -sfn "agentdeck" "$LEGACY_BIN" 2>/dev/null || true
    echo "已升级 $current -> $latest"
    echo "新版本: $latest"
    if ! start_agentdeck "" ""; then
      echo "新版本启动失败，回滚到 $current ..." >&2
      cp -p "$bak" "$BIN"
      start_agentdeck "" "" || true
      exit 1
    fi
    smoke_check_clis_from_config
    exit 0
  fi

  if [ "$current" = "$latest" ]; then
    echo "已是最新版本 $current"
    read_line "已是最新版本 ${current}，是否重新走一遍配置？[y/N]: " reconf
  else
    echo "当前版本不低于最新版本（或无法解析当前版本）"
    read_line "是否重新走一遍配置？[y/N]: " reconf
  fi
  case "$reconf" in
    y|Y) reconfigure ;;
    *) echo "保持现有配置，退出"; exit 0 ;;
  esac
else
  echo "未检测到已安装的 AgentDeck，开始全新安装。"
  install_ver="$(latest_version || true)"
  if [ -z "$install_ver" ]; then
    install_ver="$VERSION"
    echo "无法检查最新版本，使用 $VERSION"
  fi
  echo "安装版本: $install_ver"
  download_binary "$INSTALL_DIR/agentdeck.new" "$install_ver"
  mv -f "$INSTALL_DIR/agentdeck.new" "$BIN"
  chmod +x "$BIN"
  ln -sfn "agentdeck" "$LEGACY_BIN" 2>/dev/null || true
  download_config
  run_config_wizard
  if ! start_agentdeck "$access_url" "$port"; then
    exit 1
  fi
  smoke_check_clis_from_config
  echo "已安装版本: $install_ver"
  exit 0
fi
