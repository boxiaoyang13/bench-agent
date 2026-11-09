#!/usr/bin/env bash
# ============================================================================
# THVV Server 本机联调启动器（对应 config/thvv.dev.yaml，真实 subprocess 引擎）
#   ./dev-up.sh            前台启动（Ctrl-C 退出）
#   ./dev-up.sh -d         后台启动，日志写 data/thvv-server.log
#   ./dev-up.sh check      只做配置校验
#   ./dev-up.sh stop       停止后台实例
#   ./dev-up.sh status     查看状态
# 密钥首次运行自动生成并持久化到 data/.secret_key（data/ 已被 gitignore）
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")"

CONFIG=${CONFIG:-./config/thvv.dev.yaml}
DATA_DIR=./data
SECRET_FILE=$DATA_DIR/.secret_key
LOG=$DATA_DIR/thvv-server.log
PIDFILE=$DATA_DIR/thvv-server.pid
BIN=./bin/thvv-server
export PATH=/usr/local/go/bin:$PATH

mkdir -p "$DATA_DIR"
if [[ ! -s "$SECRET_FILE" ]]; then
  openssl rand -base64 32 > "$SECRET_FILE"
  chmod 600 "$SECRET_FILE"
fi
export THVV_SECRET_KEY=${THVV_SECRET_KEY:-$(cat "$SECRET_FILE")}

[[ -x $BIN ]] || make build

cmd=${1:-up}

case "$cmd" in
  check)
    exec $BIN -config "$CONFIG" -check
    ;;
  roots)
    exec $BIN -config "$CONFIG" -print-roots
    ;;
  stop)
    [[ -f $PIDFILE ]] && kill "$(cat $PIDFILE)" 2>/dev/null && echo "stopped $(cat $PIDFILE)" || echo "no pidfile"
    rm -f "$PIDFILE"
    ;;
  status)
    if [[ -f $PIDFILE ]] && kill -0 "$(cat $PIDFILE)" 2>/dev/null; then
      echo "running pid=$(cat $PIDFILE)  log=$LOG"
      curl -sS -o /dev/null -w "health: %{http_code}\n" http://127.0.0.1:18080/v1/health || true
    else
      echo "not running"
      exit 1
    fi
    ;;
  -d|--daemon|daemon)
    [[ -f $PIDFILE ]] && kill -0 "$(cat $PIDFILE)" 2>/dev/null && { echo "already running pid=$(cat $PIDFILE)"; exit 0; }
    nohup $BIN -config "$CONFIG" >"$LOG" 2>&1 &
    echo $! > "$PIDFILE"
    for _ in $(seq 1 50); do
      sleep 0.2
      if curl -sS -o /dev/null http://127.0.0.1:18080/v1/health 2>/dev/null; then
        echo "started pid=$(cat $PIDFILE)  http://127.0.0.1:18080/ui/  log=$LOG"
        exit 0
      fi
    done
    echo "启动超时，查看 $LOG"; tail -20 "$LOG"; exit 1
    ;;
  up)
    exec $BIN -config "$CONFIG"
    ;;
  *)
    echo "用法: $0 [-d|stop|status|check|roots]" >&2
    exit 2
    ;;
esac
