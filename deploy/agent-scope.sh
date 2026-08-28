#!/usr/bin/env bash
# agent-scope 项目内启停脚本(便于移植: 拷贝整个项目目录即可在任何同构服务器管理)
# 用法: ./deploy/agent-scope.sh {start|stop|restart|status}
#
# 设计:
# - 二进制默认 backend/agent-scope(缺失则提示先构建)
# - 配置自举: backend/agent-scope.yaml 不存在时复制 example
# - pid/log/db 放在项目根 run/ 目录(随目录走, 不污染系统)
# - 以当前用户运行; eBPF 需要 root 或 CAP_BPF, 非 root 会降级为 pty 读取(脚本会提示)
set -e

ScriptDir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ProjectDir="$(cd "$ScriptDir/.." && pwd)"
BackendDir="$ProjectDir/backend"
RunDir="$ProjectDir/run"
Bin="$BackendDir/agent-scope"
Config="$BackendDir/agent-scope.yaml"
ConfigExample="$BackendDir/agent-scope.yaml.example"
PidFile="$RunDir/agent-scope.pid"
OutLog="$RunDir/agent-scope.out.log"
ErrLog="$RunDir/agent-scope.err.log"
DbPath="$RunDir/agent-scope.db"

mkdir -p "$RunDir"

# 从配置提取监听端口(默认 8090); 先剥离行内注释再提首个数字端口
get_port() {
  local addr=""
  if [[ -f "$Config" ]]; then
    addr="$(grep -E '^\s*addr:' "$Config" | head -1 | sed -E 's/#.*$//' | sed -E 's/.*addr:[[:space:]]*//' | tr -d '"'"'"'')"
  fi
  if [[ -z "$addr" ]]; then addr=":8090"; fi
  # 提取首个数字序列作为端口(避免行尾空格导致 $ 锚定失败)
  echo "$addr" | grep -oE '[0-9]+' | head -1
}

is_running() {
  [[ -f "$PidFile" ]] || return 1
  local pid
  pid="$(cat "$PidFile" 2>/dev/null)"
  [[ -n "$pid" ]] || return 1
  kill -0 "$pid" 2>/dev/null
}

start() {
  if is_running; then
    echo "agent-scope 已在运行 (pid $(cat "$PidFile"))"
    return 0
  fi

  # 二进制自举
  if [[ ! -x "$Bin" ]]; then
    echo "未找到二进制: $Bin"
    echo "请先构建: cd $BackendDir && go build -o agent-scope ."
    exit 1
  fi

  # 配置自举
  if [[ ! -f "$Config" ]]; then
    if [[ -f "$ConfigExample" ]]; then
      cp "$ConfigExample" "$Config"
      echo "已复制配置样例: $Config (按需修改 addr / collect.match 等)"
    else
      echo "未找到配置: $Config 或 $ConfigExample"
      exit 1
    fi
  fi

  local port
  port="$(get_port)"
  echo "启动 agent-scope (端口 :$port, 用户 $(whoami)) ..."

  # 清空旧日志, 避免误读历史
  : > "$OutLog"
  : > "$ErrLog"

  # 以当前用户后台运行(不 sudo); eBPF 需 root/CAP_BPF, 非 root 时后端会降级 pty 读取
  nohup "$Bin" -config "$Config" -db "$DbPath" > "$OutLog" 2> "$ErrLog" &
  local startpid=$!
  echo "$startpid" > "$PidFile"
  sleep 1

  # 健康检查: 轮询 healthz 直到就绪或超时 10s
  local ok=0
  for _ in $(seq 1 20); do
    if kill -0 "$startpid" 2>/dev/null && curl -s -o /dev/null "http://127.0.0.1:${port}/healthz"; then
      ok=1
      break
    fi
    sleep 0.5
  done

  if [[ $ok -eq 0 ]]; then
    echo "启动失败或服务未在 :$port 就绪, 请查看日志: $OutLog / $ErrLog"
    kill "$startpid" 2>/dev/null
    for _ in $(seq 1 10); do kill -0 "$startpid" 2>/dev/null || break; sleep 0.5; done
    rm -f "$PidFile"
    exit 1
  fi

  echo "已启动 (pid $startpid), 监听 :$port, 日志: $OutLog"

  # eBPF 状态提示(加载失败会降级, 不影响运行但影响精度)
  if grep -qi "eBPF 不可用" "$ErrLog" 2>/dev/null; then
    echo "⚠️  eBPF 不可用, 已降级为 pty 读取(精度下降)。若需 eBPF, 请以 root 运行或赋予 CAP_BPF/CAP_SYS_ADMIN。"
  else
    echo "✅ eBPF 已加载(running 主信号生效)"
  fi
}

stop() {
  # 清理所有 agent-scope 实例(包括手动启动的旧实例), 确保端口释放新代码生效。
  # 匹配: 可执行文件 basename 为 agent-scope 且路径在项目目录内(避免误杀同名其他程序)。
  # 仅杀真正的 agent-scope 二进制进程(排除脚本自身和 bash/shell 进程)
  local pids
  pids="$(pgrep "^agent-scope$" 2>/dev/null || true)"
  if [[ -n "$pids" ]]; then
    echo "停止 agent-scope 实例 ..."
    for pid in $pids; do
      # 仅杀项目目录内的 agent-scope 进程
      local exe_pid_dir
      exe_pid_dir="$(readlink -f /proc/"$pid"/cwd 2>/dev/null || echo "")"
      if [[ "$exe_pid_dir" == "$ProjectDir"* ]] || [[ -n "$(cat /proc/"$pid"/cmdline 2>/dev/null | tr '\0' ' ' | grep -F "$ProjectDir")" ]]; then
        kill "$pid" 2>/dev/null || true
      fi
    done
    # 等待所有进程退出
    for _ in $(seq 1 10); do
      local remaining=""
      for pid in $pids; do
        kill -0 "$pid" 2>/dev/null && remaining="$remaining $pid" || true
      done
      pids="${remaining# }"
      [[ -z "$pids" ]] && break
      sleep 0.5
    done
    # 强杀残留
    for pid in $pids; do
      kill -9 "$pid" 2>/dev/null || true
    done
    sleep 1
  fi
  rm -f "$PidFile"
  # 验证端口已释放
  local port
  port="$(get_port)"
  if ss -tlnp 2>/dev/null | grep -qE "[: ]${port} "; then
    echo "⚠️  端口 :${port} 仍有进程占用, 尝试强杀..."
    local holder
    holder="$(ss -tlnp 2>/dev/null | grep -E "[: ]${port} " | grep -oP 'pid=\K[0-9]+' || true)"
    if [[ -n "$holder" ]]; then
      kill -9 "$holder" 2>/dev/null || true
      sleep 1
    fi
  fi
  echo "已停止"
}

status() {
  if is_running; then
    local pid port
    pid="$(cat "$PidFile")"
    port="$(get_port)"
    echo "agent-scope 运行中 (pid $pid, 端口 :$port)"
    echo "--- 最近日志 ---"
    tail -n 5 "$OutLog" 2>/dev/null
  else
    echo "agent-scope 未运行"
  fi
}

case "${1:-}" in
  start)   start ;;
  stop)    stop ;;
  restart) stop; start ;;
  status)  status ;;
  *)
    echo "用法: $0 {start|stop|restart|status}"
    exit 1
    ;;
esac
