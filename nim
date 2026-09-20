#!/bin/sh
# nim: run nim-proxy in foreground, as a service, or show status.
# usage: nim [run|start|stop|restart|status|logs|tail|install|uninstall]
PROXY_DIR="$(cd "$(dirname "$0")" && pwd)"
LOGFILE="$PROXY_DIR/nim-proxy.log"
BINARY="$PROXY_DIR/nim-proxy"
SVC="com.user.nvidia-nim-proxy"
PLIST="$HOME/Library/LaunchAgents/$SVC.plist"
SYSTEMD="$HOME/.config/systemd/user/nvidia-nim-proxy.service"
OS="$(uname -s 2>/dev/null)"

find_py() {
  for p in /opt/homebrew/bin/python3 /usr/local/bin/python3 python3; do
    if "$p" -c 'pass' >/dev/null 2>&1; then echo "$p"; return 0; fi
  done
  return 1
}

case "$OS" in
  Darwin) SVC_CMD="launchd";;
  Linux)  command -v systemctl >/dev/null 2>&1 && SVC_CMD="systemd" || SVC_CMD="nohup";;
  *)      SVC_CMD="nohup";;
esac

svc_running() {
  case "$SVC_CMD" in
    launchd) launchctl list "$SVC" 2>/dev/null | grep '"PID"' | grep -qv '= 0' ;;
    systemd) systemctl --user is-active -q nvidia-nim-proxy 2>/dev/null ;;
    nohup)   [ -f /tmp/nim-proxy.pid ] && kill -0 "$(cat /tmp/nim-proxy.pid)" 2>/dev/null ;;
  esac
}

svc_start() {
  case "$SVC_CMD" in
    launchd)
      sed "s|REPLACE_WITH_PATH|$PROXY_DIR|g" "$PROXY_DIR/deploy/$SVC.plist" > "$PLIST"
      launchctl bootstrap "gui/$(id -u)" "$PLIST" 2>/dev/null || launchctl load "$PLIST" 2>/dev/null ;;
    systemd)
      mkdir -p "$(dirname "$SYSTEMD")"
      sed "s|REPLACE_WITH_PATH|$PROXY_DIR|g" "$PROXY_DIR/deploy/nvidia-nim-proxy.service" > "$SYSTEMD"
      systemctl --user daemon-reload && systemctl --user enable --now nvidia-nim-proxy ;;
    nohup)
      cd "$PROXY_DIR" && nohup "$BINARY" >>"$LOGFILE" 2>&1 &
      echo $! > /tmp/nim-proxy.pid ;;
  esac
}

svc_stop() {
  case "$SVC_CMD" in
    launchd) launchctl bootout "gui/$(id -u)/$SVC" 2>/dev/null || launchctl unload "$PLIST" 2>/dev/null ;;
    systemd) systemctl --user disable --now nvidia-nim-proxy 2>/dev/null ;;
    nohup)   [ -f /tmp/nim-proxy.pid ] && kill "$(cat /tmp/nim-proxy.pid)" 2>/dev/null; rm -f /tmp/nim-proxy.pid ;;
  esac
}

pretty_status() {
  if svc_running; then echo "nim-proxy: RUNNING ($SVC_CMD)"; else echo "nim-proxy: STOPPED"; return 1; fi
  PY="$(find_py)" || PY=""
  if [ -n "$PY" ] && command -v curl >/dev/null 2>&1; then
    "$PY" << EOF 2>/dev/null
import json, urllib.request
try:
  data = json.loads(urllib.request.urlopen('http://localhost:${PORT:-5419}/status', timeout=3).read())
  print()
  print(f"  Version:    {data.get('version','?')}")
  print(f"  Uptime:     {data['uptime']}")
  if 'keys' in data:
    print(f"  Keys:       {data['available']}/{data['total']} available")
    print()
    for k in data['keys']:
      icon = '✓' if k['available'] else '✗'
      bits = []
      if k.get('cooldown'): bits.append(f"{k.get('cooldown_reason','')}:{k['cooldown']}")
      if k.get('last_used'): bits.append(f"{k['last_used']} ago")
      if k.get('fail_count'): bits.append(f"{k['fail_count']} fails")
      if k.get('consec_429'): bits.append(f"{k['consec_429']}x429")
      info = ' (' + ', '.join(bits) + ')' if bits else ''
      print(f"    {icon}  {k.get('name','?'):12s} ...{k['suffix']}{info}")
  else:
    print("  Keys:       none (keyless, opencode/* only)")
  oc = data.get('opencode') or {}
  if oc.get('enabled'):
    ocm = oc.get('models') or []
    print()
    print(f"  Opencode:   {len(ocm)} free models")
    for m in ocm: print(f"    {m}")
  print()
except Exception as e:
  print(f'  (status fetch failed: {e})')
EOF
  elif command -v curl >/dev/null; then
    echo ""; curl -s "http://localhost:${PORT:-5419}/status"; echo ""
  fi
}

case "${1:-status}" in
  run)     exec "$BINARY" ;;
  start)   svc_start; sleep 2; pretty_status ;;
  stop)    svc_stop; echo "nim-proxy stopped" ;;
  restart) svc_stop; sleep 1; svc_start; sleep 2; pretty_status ;;
  status)  pretty_status ;;
  logs)
    pretty_status
    [ -f "$LOGFILE" ] && { echo ""; echo "  Access log (last 10):"; tail -10 "$LOGFILE" | sed 's/^/    /'; } ;;
  tail)    tail -f "$LOGFILE" ;;
  install)
    svc_start; sleep 2; pretty_status
    echo "installed ($SVC_CMD). see deploy/ for system-wide setup." ;;
  uninstall) svc_stop; rm -f "$PLIST" "$SYSTEMD"; echo "nim-proxy uninstalled" ;;
  *) echo "Usage: nim [run|start|stop|restart|status|logs|tail|install|uninstall]"; exit 1 ;;
esac
