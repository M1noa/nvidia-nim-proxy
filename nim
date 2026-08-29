#!/bin/sh
PROXY_DIR="$(cd "$(dirname "$0")" && pwd)"
PIDFILE="/tmp/nim-proxy.pid"
LOGFILE="$PROXY_DIR/nim-proxy.log"
BINARY="$PROXY_DIR/nim-proxy"
PLIST="$HOME/Library/LaunchAgents/com.user.nvidia-nim-proxy.plist"
SVC="com.user.nvidia-nim-proxy"

launchd_pid() {
  launchctl list "$SVC" 2>/dev/null | grep '"PID"' | sed 's/.*= \([0-9]*\).*/\1/'
}

launchd_status_text() {
  PID=$(launchd_pid)
  if [ -z "$PID" ] || [ "$PID" = "0" ]; then
    return 1
  fi
  UPTIME=$(ps -o etime= -p "$PID" 2>/dev/null | tr -d ' ')
  echo "nim-proxy: RUNNING  (PID $PID, up ${UPTIME:-?})"
  rm -f "$PIDFILE"
  return 0
}

status_text() {
  launchd_status_text && return 0
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
    fc = k.get('fail_count', 0)
    c429 = k.get('consec_429', 0)
    icon = '\033[32m\u2713\033[0m' if a else '\033[31m\u2717\033[0m'
    bits = []
    if cd: bits.append(f'{cr}:{cd}')
    if lu: bits.append(f'{lu} ago')
    if fc: bits.append(f'{fc} fails')
    if c429: bits.append(f'{c429}x429')
    info = ' (' + ', '.join(bits) + ')' if bits else ''
    print(f'    {icon}  {n:12s} ...{s}{info}')
  oc = data.get('opencode') or {}
  if oc.get('enabled'):
    ocm = oc.get('models') or []
    print()
    print(f"  Opencode:  {len(ocm)} free models \033[2m{oc.get('base','')}\033[0m")
    if ocm:
      half = (len(ocm) + 1) // 2
      for i in range(half):
        left = ocm[i]
        right = ocm[i+half] if i+half < len(ocm) else ''
        print(f"    {left:38s} {right}")
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
    launchctl bootstrap gui/$(id -u) "$PLIST" 2>/dev/null || launchctl load "$PLIST" 2>/dev/null
    sleep 2
    PID=$(launchd_pid)
    if [ -n "$PID" ] && [ "$PID" != "0" ]; then
      echo "nim-proxy started (launchd, PID $PID, log: $LOGFILE)"
      rm -f "$PIDFILE"
    else
      echo "nim-proxy failed to start via launchd"
      launchctl list "$SVC" 2>&1
      exit 1
    fi
    ;;
  stop)
    launchctl unload "$PLIST" 2>/dev/null
    rm -f "$PIDFILE"
    echo "nim-proxy stopped"
    ;;
  restart)
    launchctl unload "$PLIST" 2>/dev/null
    rm -f "$PIDFILE"
    sleep 1
    launchctl bootstrap gui/$(id -u) "$PLIST" 2>/dev/null || launchctl load "$PLIST" 2>/dev/null
    sleep 2
    PID=$(launchd_pid)
    if [ -n "$PID" ] && [ "$PID" != "0" ]; then
      echo "nim-proxy restarted (launchd, PID $PID)"
    else
      echo "nim-proxy failed to restart via launchd"
      launchctl list "$SVC" 2>&1
      exit 1
    fi
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
    tail -f "$LOGFILE"
    ;;
  help|*)
    echo "Usage: nim <command>"
    echo ""
    echo "  start          Start the proxy via launchd"
    echo "  stop           Stop the proxy via launchd"
    echo "  restart        Restart the proxy via launchd"
    echo "  status         Show pretty status with key pool info"
    echo "  status-tail    Live status (updates every 1s)"
    echo "  logs           Show status + last log lines"
    echo "  tail           Tail the access log (live)"
    exit 0
    ;;
esac
