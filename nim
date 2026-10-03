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
    systemd) systemctl --user is-active -q nvidia-nim-proxy 2>/dev/null ;;
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
  if svc_running || port_alive; then echo "nim-proxy: RUNNING ($SVC_CMD)"; else echo "nim-proxy: STOPPED"; return 1; fi
  PY="$(find_py)" || PY=""
  if [ -n "$PY" ] && command -v curl >/dev/null 2>&1; then
    "$PY" << EOF 2>/dev/null
import json, os, urllib.request
try:
  # NIM_AUTH forwards your token so authed proxies show full status.
  req = urllib.request.Request('http://localhost:${PORT:-5419}/status')
  if os.environ.get('NIM_AUTH'):
    req.add_header('Authorization', 'Bearer ' + os.environ['NIM_AUTH'])
  data = json.loads(urllib.request.urlopen(req, timeout=3).read())
  print()
  print(f"  Version:    {data.get('version','?')}")
  print(f"  Uptime:     {data['uptime']}")
  print(f"  Load:       {data.get('concurrent',0)}/{data.get('sem_limit',0)} concurrent")
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
  if data.get('model_locks'):
    print()
    print(f"  Locks:      {data['model_locks']}")
  if data.get('zen_session') or data.get('zen_proxy'):
    print()
    print(f"  Zen:        {data.get('zen_session','?')} via {data.get('zen_proxy') or 'direct'} ({data.get('zen_ago','?')} ago)")
  zl = data.get('zen_lanes')
  if zl is not None:
    cap = data.get('lane_cap', len(zl))
    print()
    print(f"  Lanes:      {len(zl)}/{cap} live")
    for l in zl:
      icon = '❄' if l.get('cooldown_remaining') else '●'
      bits = []
      if l.get('proxy'): bits.append(f"{l['proxy']} [{l.get('country','?')}]")
      else: bits.append('no exit')
      if l.get('cooldown_remaining'): bits.append(f"cool {l['cooldown_remaining']}")
      if l.get('last_used_ago'): bits.append(f"used {l['last_used_ago']} ago")
      if l.get('requests'): bits.append(f"{l['requests']} req")
      if l.get('rate_limited'): bits.append(f"{l['rate_limited']}x429")
      print(f"    {icon}  {l.get('id','?'):14s} {' '.join(bits)}")
  pl = data.get('pool')
  if pl:
    print()
    print(f"  Pool:       {pl.get('verified',0)} verified of {pl.get('candidates',0)} cands, {pl.get('dropped_total',0)} dropped, refresh {pl.get('last_refresh_ago','?')} ago")
    for e in (pl.get('exits') or [])[:10]:
      pin = '📌' if e.get('pinned') else '  '
      print(f"    {pin} {e.get('proxy','?'):32s} [{e.get('country','?')}] {e.get('latency_ms','?')}ms")
    if len(pl.get('exits') or []) > 10:
      print(f"    ... +{len(pl['exits'])-10} more")
  print()
except Exception as e:
  print(f'  (status fetch failed: {e})')
EOF
  elif command -v curl >/dev/null; then
    echo ""; curl -s "http://localhost:${PORT:-5419}/status"; echo ""
  fi
}

# status_tail live-loops pretty_status every 2s. distinct from log tail.
status_tail() {
  trap 'exit 0' INT TERM
  while true; do
    clear 2>/dev/null || printf '\033c'
    pretty_status
    sleep 2
  done
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
  logs)    pretty_status; [ -f "$LOGFILE" ] && { echo ""; echo "  Access log (last 10):"; tail -10 "$LOGFILE" | sed 's/^/    /'; } ;;
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
