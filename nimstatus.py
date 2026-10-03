#!/usr/bin/env python3
"""nimstatus: rich renderer for ./nim status/log commands. stdlib fallback."""
import datetime
import json
import os
import sys
import urllib.request

PORT = os.environ.get("PORT", "5419")
BASE = "http://localhost:" + PORT
ROOT = os.path.dirname(os.path.abspath(__file__))


def fetch(path):
    req = urllib.request.Request(BASE + path)
    if os.environ.get("NIM_AUTH"):
        req.add_header("Authorization", "Bearer " + os.environ["NIM_AUTH"])
    return json.loads(urllib.request.urlopen(req, timeout=3).read())


try:
    from rich.console import Console, Group
    from rich.live import Live
    from rich.table import Table
    from rich.text import Text

    RICH = True
except ImportError:
    RICH = False


def today_str():
    return datetime.date.today().isoformat()


def jsonl_stats():
    """All-time + today totals from nim-usage.jsonl. Returns dict."""
    out = {"all": {"req": 0, "tok": 0}, "today": {"req": 0, "tok": 0}}
    path = os.path.join(ROOT, "nim-usage.jsonl")
    today = today_str()
    try:
        with open(path) as f:
            for line in f:
                try:
                    r = json.loads(line)
                except ValueError:
                    continue
                tok = r.get("total_tokens") or (
                    r.get("prompt_tokens", 0) + r.get("completion_tokens", 0))
                out["all"]["req"] += 1
                out["all"]["tok"] += tok
                if str(r.get("ts", ""))[:10] == today:
                    out["today"]["req"] += 1
                    out["today"]["tok"] += tok
    except OSError:
        pass
    return out


def fmt_tok(n):
    if n >= 1_000_000:
        return "%.1fM" % (n / 1_000_000)
    if n >= 1_000:
        return "%.1fk" % (n / 1_000)
    return str(n)


def plain_status(d):
    c = []
    c.append("NimRoute %s | up %s | load %d/%d" % (
        d.get("version", "?"), d.get("uptime", "?"),
        d.get("concurrent", 0), d.get("sem_limit", 0)))
    lanes = d.get("zen_lanes") or []
    live = sum(1 for l in lanes if not l.get("cooldown_remaining"))
    c.append("lanes %d/%d live" % (live, len(lanes)))
    p = d.get("pool") or {}
    c.append("pool %d verified, %d dropped" % (
        p.get("verified", 0), p.get("dropped_total", 0)))
    st = jsonl_stats()
    c.append("today %d req, %s tok · all-time %d req, %s tok" % (
        st["today"]["req"], fmt_tok(st["today"]["tok"]),
        st["all"]["req"], fmt_tok(st["all"]["tok"])))
    return "\n".join(c)


def merged_table(d):
    """One table: lane + its exit (host, cc, ms) + req + state."""
    lanes = d.get("zen_lanes") or []
    pool = d.get("pool") or {}
    exits = {e.get("proxy"): e for e in (pool.get("exits") or [])}
    t = Table(show_header=True, header_style="bold", box=None,
              pad_edge=False)
    t.add_column("", width=2)
    t.add_column("lane", style="cyan")
    t.add_column("exit")
    t.add_column("cc")
    t.add_column("ms", justify="right")
    t.add_column("req", justify="right")
    t.add_column("state")
    for l in lanes:
        cool = l.get("cooldown_remaining")
        mark = Text("○", style="red") if cool else Text("●", style="green")
        state = Text("cool " + cool, style="red") if cool else Text(
            "live", style="dim")
        proxy = l.get("proxy") or ""
        e = exits.get(proxy, {})
        if proxy:
            host = proxy.split("://", 1)[-1]
            exit_t = Text(host)
        else:
            exit_t = Text("—", style="dim")
        t.add_row(mark, (l.get("id") or "?")[:14], exit_t,
                  str(e.get("country", "·")), str(e.get("latency_ms", "·")),
                  str(l.get("requests", 0)), state)
    return t


def build_renderable(d, stats):
    parts = []
    hdr = Text("NimRoute %s" % d.get("version", "?"), style="bold cyan")
    hdr.append("  up %s" % d.get("uptime", "?"), style="dim")
    hdr.append("  load %d/%d" % (d.get("concurrent", 0),
                                 d.get("sem_limit", 0)))
    if "keys" in d:
        ok = bool(d.get("available"))
        hdr.append("  keys %d/%d" % (d.get("available", 0),
                                     d.get("total", 0)),
                   style="green" if ok else "red")
    parts.append(hdr)
    parts.append(merged_table(d))
    p = d.get("pool") or {}
    parts.append(Text("pool  %d verified · %d dropped · refresh %s ago" % (
        p.get("verified", 0), p.get("dropped_total", 0),
        p.get("last_refresh_ago", "?")), style="dim"))
    parts.append(Text("today %d req · %s tok   │   all-time %d req · %s tok" % (
        stats["today"]["req"], fmt_tok(stats["today"]["tok"]),
        stats["all"]["req"], fmt_tok(stats["all"]["tok"])), style="dim"))
    return Group(*parts)


def rich_status(d):
    con = Console()
    con.print(build_renderable(d, jsonl_stats()))


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
        con = Console()

        def render():
            try:
                d = fetch("/status")
            except Exception as e:
                return Text("(status fetch failed: %s)" % e, style="red")
            return build_renderable(d, jsonl_stats())
        with Live(render(), refresh_per_second=0.5, console=con) as live:
            import time
            try:
                while True:
                    time.sleep(2)
                    live.update(render())
            except KeyboardInterrupt:
                pass
    elif cmd == "logs":
        n = int(sys.argv[2]) if len(sys.argv) > 2 else 10
        log = os.path.join(ROOT, "nim-proxy.log")
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
