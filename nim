#!/bin/sh
PROXY_DIR="$(cd "$(dirname "$0")" && pwd)"
PIDFILE="/tmp/nim-proxy.pid"
LOGFILE="$PROXY_DIR/nim-proxy.log"
BINARY="$PROXY_DIR/nim-proxy"

status_text() {
  if [ ! -f "$PIDFILE" ] || ! kill -0 "$(cat "$PIDFILE")" 2>/dev/null; then
    echo "nim-proxy: STOPPED"
    [ -f "$PIDFILE" ] && rm -f "$PIDFILE"
    return 1
  fi
  PID=$(cat "$PIDFILE")
  UPTIME=$(ps -o etime= -p "$PID" 2>/dev/null | tr -d ' ')
  echo "nim-proxy: RUNNING  (PID $PID, up ${UPTIME:-?})"
  return 0
}

pretty_status() {
  status_text || return
  if command -v curl >/dev/null 2>&1 && command -v python3 >/dev/null 2>&1; then
    python3 << EOF 2>/dev/null
import json, urllib.request
try:
  data = json.loads(urllib.request.urlopen('http://localhost:${PORT:-5419}/status', timeout=3).read())
  cc = data.get('concurrent', 0)
  sl = data.get('sem_limit', 0)
  print()
  print(f"  Version:   {data.get('version','?')}")
  print(f"  Uptime:    {data['uptime']}")
  print(f"  Keys:      {data['available']}/{data['total']} available")
  print(f"  Concurrent: {cc}/{sl}")
  print()
  for k in data.get('keys', []):
    n = k.get('name', '?')
    s = k['suffix']
    a = k['available']
    cd = k.get('cooldown', '')
    cr = k.get('cooldown_reason', '')
    lu = k.get('last_used', '')
    fc = k['fail_count']
    icon = '\033[32m\u2713\033[0m' if a else '\033[31m\u2717\033[0m'
    bits = []
    if cd: bits.append(f'{cr}:{cd}')
    if lu: bits.append(f'{lu} ago')
    if fc: bits.append(f'{fc} fails')
    info = ' (' + ', '.join(bits) + ')' if bits else ''
    print(f'    {icon}  {n:12s} ...{s}{info}')
  print()
except Exception as e:
  print(f'  (status fetch failed: {e})')
EOF
  elif command -v curl >/dev/null; then
    echo ""
    curl -s "http://localhost:${PORT:-5419}/status" | python3 -m json.tool 2>/dev/null || curl -s "http://localhost:${PORT:-5419}/status"
  fi
}

case "${1:-help}" in
  start)
    if [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; then
      echo "nim-proxy already running (PID $(cat "$PIDFILE"))"
      exit 0
    fi
    rm -f "$PIDFILE"
    cd "$PROXY_DIR" || exit 1
    nohup "$BINARY" >> "$LOGFILE" 2>&1 &
    PID=$!
    echo "$PID" > "$PIDFILE"
    sleep 1
    if kill -0 "$PID" 2>/dev/null; then
      echo "nim-proxy started (PID $PID, log: $LOGFILE)"
    else
      echo "nim-proxy failed to start"
      tail -5 "$LOGFILE" 2>/dev/null
      rm -f "$PIDFILE"
      exit 1
    fi
    ;;
  stop)
    if [ ! -f "$PIDFILE" ]; then
      echo "nim-proxy not running"
      exit 0
    fi
    PID=$(cat "$PIDFILE")
    if ! kill -0 "$PID" 2>/dev/null; then
      echo "nim-proxy not running (stale pidfile)"
      rm -f "$PIDFILE"
      exit 0
    fi
    echo "stopping nim-proxy (PID $PID)..."
    kill "$PID" 2>/dev/null
    i=0
    while [ $i -lt 5 ]; do
      if ! kill -0 "$PID" 2>/dev/null; then
        echo "nim-proxy stopped"
        rm -f "$PIDFILE"
        exit 0
      fi
      sleep 1
      i=$((i + 1))
    done
    echo "force killing..."
    kill -9 "$PID" 2>/dev/null
    sleep 1
    if ! kill -0 "$PID" 2>/dev/null; then
      echo "nim-proxy force killed"
      rm -f "$PIDFILE"
      exit 0
    fi
    echo "failed to stop nim-proxy"
    exit 1
    ;;
  status)
    pretty_status
    ;;
  logs)
    pretty_status
    if [ -f "$LOGFILE" ]; then
      echo ""
      echo "  Access log (last 10):"
      tail -10 "$LOGFILE" | sed 's/^/    /'
    fi
    ;;
  status-tail|live)
    while clear && pretty_status; do sleep 1; done
    ;;
  tail)
    echo "Usage: nim <command>"
    echo ""
    echo "  start          Start the proxy (if not running)"
    echo "  stop           Stop the proxy (force kill after 5s)"
  echo "  status         Show pretty status with key pool info"
  echo "  status-tail    Live status (updates every 1s)"
  echo "  logs           Show status + last log lines"
    echo "  tail           Tail the access log (live)"
    exit 0
    ;;
esac
