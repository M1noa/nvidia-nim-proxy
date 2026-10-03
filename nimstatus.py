#!/usr/bin/env python3
"""nimstatus: rich renderer for ./nim status/log commands. stdlib fallback."""
import json
import os
import sys
import urllib.request

PORT = os.environ.get("PORT", "5419")
BASE = "http://localhost:" + PORT


def fetch(path):
    req = urllib.request.Request(BASE + path)
    if os.environ.get("NIM_AUTH"):
        req.add_header("Authorization", "Bearer " + os.environ["NIM_AUTH"])
    return json.loads(urllib.request.urlopen(req, timeout=3).read())


try:
    from rich.console import Console
    from rich.table import Table
    from rich.text import Text

    RICH = True
except ImportError:
    RICH = False


def plain_status(d):
    c = []
    c.append("NimRoute %s | up %s | load %d/%d" % (
        d.get("version", "?"), d.get("uptime", "?"),
        d.get("concurrent", 0), d.get("sem_limit", 0)))
    if "keys" in d:
        c.append("keys %d/%d avail" % (d.get("available", 0), d.get("total", 0)))
    lanes = d.get("zen_lanes") or []
    live = sum(1 for l in lanes if not l.get("cooldown_remaining"))
    c.append("lanes %d/%d live" % (live, len(lanes)))
    p = d.get("pool") or {}
    c.append("pool %d verified, %d dropped" % (
        p.get("verified", 0), p.get("dropped_total", 0)))
    u = d.get("usage") or {}
    c.append("usage %d req, %d tok" % (
        u.get("requests", 0), u.get("prompt_tokens", 0) + u.get("completion_tokens", 0)))
    return "\n".join(c)


def rich_status(d):
    con = Console()
    hdr = Text("NimRoute %s" % d.get("version", "?"), style="bold cyan")
    hdr.append("  up %s" % d.get("uptime", "?"), style="dim")
    hdr.append("  load %d/%d" % (d.get("concurrent", 0), d.get("sem_limit", 0)))
    con.print(hdr)
    if "keys" in d:
        con.print("keys  %d/%d available" % (d.get("available", 0), d.get("total", 0)),
                  style="green" if d.get("available") else "red")
    else:
        con.print("keys  keyless (opencode/* only)", style="dim")
    lanes = d.get("zen_lanes") or []
    if lanes:
        t = Table(title="lanes %d" % len(lanes), show_header=True,
                  header_style="bold", box=None, pad_edge=False)
        t.add_column("", width=2)
        t.add_column("lane", style="cyan")
        t.add_column("exit")
        t.add_column("req", justify="right")
        t.add_column("state")
        for l in lanes:
            cool = l.get("cooldown_remaining")
            mark = Text("○", style="red") if cool else Text("●", style="green")
            state = Text("cool " + cool, style="red") if cool else Text(
                "live", style="dim")
            proxy = l.get("proxy") or Text("no exit", style="dim")
            t.add_row(mark, (l.get("id") or "?")[:14], proxy,
                      str(l.get("requests", 0)), state)
        con.print(t)
    p = d.get("pool") or {}
    if p:
        con.print("pool  %d verified · %d dropped · refresh %s ago" % (
            p.get("verified", 0), p.get("dropped_total", 0),
            p.get("last_refresh_ago", "?")))
        exits = p.get("exits") or []
        t = Table(show_header=True, header_style="bold", box=None,
                  pad_edge=False)
        t.add_column("", width=2)
        t.add_column("exit")
        t.add_column("cc")
        t.add_column("ms", justify="right")
        for e in exits[:8]:
            pin = Text("◆", style="yellow") if e.get("pinned") else Text("·", style="dim")
            t.add_row(pin, e.get("proxy", "?"), e.get("country", "?"),
                      str(e.get("latency_ms", "?")))
        con.print(t)
        if len(exits) > 8:
            con.print("      +%d more" % (len(exits) - 8), style="dim")
    u = d.get("usage") or {}
    if u:
        con.print("usage %d req · %d in · %d out" % (
            u.get("requests", 0), u.get("prompt_tokens", 0),
            u.get("completion_tokens", 0)), style="dim")


def rich_status_renderable(d, con):
    """Render status into capturable output for Live refresh."""
    with con.capture() as cap:
        rich_status(d)
    return Text(cap.get())


def main():
    cmd = sys.argv[1] if len(sys.argv) > 1 else "status"
    if cmd == "status":
        try:
            d = fetch("/status")
        except Exception as e:
            print("(status fetch failed: %s)" % e)
            return 1
        if RICH:
            rich_status(d)
        else:
            print(plain_status(d))
    elif cmd == "tail-status":
        if not RICH:
            print("rich not available, install with: pip install rich")
            return 1
        from rich.live import Live
        from rich.console import Console
        con = Console()

        def render():
            try:
                d = fetch("/status")
            except Exception as e:
                return Text("(status fetch failed: %s)" % e, style="red")
            return rich_status_renderable(d, con)
        with Live(render(), refresh_per_second=0.5, console=con,
                  screen=False) as live:
            import time
            try:
                while True:
                    time.sleep(2)
                    live.update(render())
            except KeyboardInterrupt:
                pass
    elif cmd == "logs":
        n = int(sys.argv[2]) if len(sys.argv) > 2 else 10
        log = os.path.join(os.path.dirname(os.path.abspath(__file__)),
                           "nim-proxy.log")
        try:
            with open(log) as f:
                lines = f.readlines()[-n:]
        except OSError as e:
            print("(log read failed: %s)" % e)
            return 1
        if RICH:
            con = Console()
            for line in lines:
                line = line.rstrip("\n")
                if "!!" in line:
                    con.print(line, style="red")
                elif "retry" in line or "429" in line or "403" in line:
                    con.print(line, style="yellow")
                else:
                    con.print(line, style="dim")
        else:
            print("".join(lines), end="")
    else:
        print("usage: nimstatus.py [status|tail-status|logs [N]]")
        return 1


if __name__ == "__main__":
    sys.exit(main())
