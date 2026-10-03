#!/bin/sh
# nim: run nim-proxy in foreground, as a service, or show status.
# usage: nim [run|start|stop|restart|status [tail]|status-tail|logs|log [N|tail]|tail|install|uninstall]
PROXY_DIR="$(cd "$(dirname "$0")" && pwd)"
LOGFILE="$PROXY_DIR/nim-proxy.log"
BINARY="$PROXY_DIR/nim-proxy"
SVC="com.user.nimroute"
PLIST="$HOME/Library/LaunchAgents/$SVC.plist"
SYSTEMD="$HOME/.config/systemd/user/nimroute.service"
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
    systemd) systemctl --user is-active -q nimroute 2>/dev/null ;;
    nohup)   [ -f /tmp/nim-proxy.pid ] && kill -0 "$(cat /tmp/nim-proxy.pid)" 2>/dev/null ;;
  esac
}

# port_alive reports whether something is serving on the proxy port. catches
# instances started manually (outside the service manager) so status reads
# correctly and start does not double-bind.
port_alive() {
  command -v curl >/dev/null 2>&1 && curl -sf -m 2 "http://localhost:${PORT:-5419}/status" >/dev/null 2>&1
}

svc_start() {
  case "$SVC_CMD" in
    launchd)
      sed "s|REPLACE_WITH_PATH|$PROXY_DIR|g" "$PROXY_DIR/deploy/$SVC.plist" > "$PLIST"
      launchctl bootout "gui/$(id -u)/$SVC" 2>/dev/null
      launchctl bootstrap "gui/$(id -u)" "$PLIST" || launchctl load "$PLIST" ;;
    systemd)
      mkdir -p "$(dirname "$SYSTEMD")"
      sed "s|REPLACE_WITH_PATH|$PROXY_DIR|g" "$PROXY_DIR/deploy/nimroute.service" > "$SYSTEMD"
      systemctl --user daemon-reload && systemctl --user enable --now nimroute ;;
    nohup)
      cd "$PROXY_DIR" && nohup "$BINARY" >>"$LOGFILE" 2>&1 &
      echo $! > /tmp/nim-proxy.pid ;;
  esac
}

svc_stop() {
  case "$SVC_CMD" in
    launchd)
      # bootout both labels: pre-rename instances still run as
      # com.user.nvidia-nim-proxy, current as com.user.nimroute.
      launchctl bootout "gui/$(id -u)/$SVC" 2>/dev/null || launchctl unload "$PLIST" 2>/dev/null
      launchctl bootout "gui/$(id -u)/com.user.nvidia-nim-proxy" 2>/dev/null
      pkill -f "nim-proxy$" 2>/dev/null
      ;;
    systemd) systemctl --user disable --now nimroute 2>/dev/null ;;
    nohup)   [ -f /tmp/nim-proxy.pid ] && kill "$(cat /tmp/nim-proxy.pid)" 2>/dev/null; rm -f /tmp/nim-proxy.pid ;;
  esac
}

pretty_status() {
  if svc_running || port_alive; then echo "nimroute: RUNNING ($SVC_CMD)"; else echo "nimroute: STOPPED"; return 1; fi
  PY="$(find_py)" || PY=""
  if [ -n "$PY" ] && command -v curl >/dev/null 2>&1; then
    "$PY" "$PROXY_DIR/nimstatus.py" status 2>/dev/null || { echo ""; curl -s "http://localhost:${PORT:-5419}/status"; echo ""; }
  elif command -v curl >/dev/null; then
    echo ""; curl -s "http://localhost:${PORT:-5419}/status"; echo ""
  fi
}

# status_tail refreshes in place (rich Live) instead of clearing.
status_tail() {
  PY="$(find_py)" || PY=""
  if [ -n "$PY" ]; then
    "$PY" "$PROXY_DIR/nimstatus.py" tail-status
  else
    trap 'exit 0' INT TERM
    while true; do
      clear 2>/dev/null || printf '\033c'
      pretty_status
      sleep 2
    done
  fi
}

case "${1:-status}" in
  run)     exec "$BINARY" ;;
  start)
    if port_alive; then pretty_status; exit 0; fi
    svc_start; sleep 2; pretty_status ;;
  stop)    svc_stop; echo "nim-proxy stopped" ;;
  restart) svc_stop; sleep 1; svc_start; sleep 2; pretty_status ;;
  status)  pretty_status; [ "$2" = "tail" ] && status_tail ;;
  status-tail) status_tail ;;
  logs)    pretty_status; PY="$(find_py)" || PY=""; [ -n "$PY" ] && "$PY" "$PROXY_DIR/nimstatus.py" logs 10 || { [ -f "$LOGFILE" ] && { echo ""; echo "  Access log (last 10):"; tail -10 "$LOGFILE" | sed 's/^/    /'; }; } ;;
  log)
    case "$2" in
      ""|tail|f) tail -f "$LOGFILE" ;;
      [0-9]*) [ -f "$LOGFILE" ] && { echo ""; echo "  Access log (last $2):"; tail -n "$2" "$LOGFILE" | sed 's/^/    /'; } ;;
      *) echo "Usage: nim log [N|tail]"; exit 1 ;;
    esac ;;
  tail)    tail -f "$LOGFILE" ;;
  install)
    port_alive && { pretty_status; exit 0; }
    svc_start; sleep 2; pretty_status
    echo "installed ($SVC_CMD). see deploy/ for system-wide setup." ;;
  uninstall) svc_stop; rm -f "$PLIST" "$SYSTEMD"; echo "nim-proxy uninstalled" ;;
  *) echo "Usage: nim [run|start|stop|restart|status [tail]|status-tail|logs|log [N|tail]|tail|install|uninstall]"; exit 1 ;;
esac
