#!/usr/bin/env python3
"""probe every proxy against zen with a short prompt and random session.

usage:
  python3 probe_zen_proxies.py [--models big-pickle,union-alpha] [--threads 20] [--limit 0] [--country US]
  python3 probe_zen_proxies.py --models big-pickle --threads 30 --out zen_probe.tsv

fetches the proxy list from the same api as zenproxy.go (minus the us-only
filter so blocked countries show up), posts a tiny prompt through each proxy
with a randomized session, and writes one tsv row per attempt plus a
per-country block summary for hardcoding exclusions.
"""
import csv
import json
import secrets
import subprocess
import sys
import time
import urllib.request
from collections import Counter, defaultdict
from concurrent.futures import ThreadPoolExecutor

ZEN = "https://opencode.ai/zen/v1"
VERSION = "1.18.31"
UA = f"opencode/{VERSION} ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14"
B62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
GATE_TOOLS = [{"type": "function", "function": {"name": n}} for n in ("bash", "read", "glob", "grep")]
LIST_URL = "https://proxies.minoa.cat/list?format=json&sort=response&limit=0"

_seq = {"ms": 0, "n": 0}


def new_id(prefix):
    ms = int(time.time() * 1000)
    if ms == _seq["ms"]:
        _seq["n"] += 1
    else:
        _seq["ms"], _seq["n"] = ms, 1
    hexpart = (((ms << 12) + _seq["n"]) & 0xFFFFFFFFFFFF).to_bytes(6, "big").hex()
    return f"{prefix}_{hexpart}{''.join(B62[b % 62] for b in secrets.token_bytes(14))}"


def endpoint(model):
    if model.startswith("muse-spark"):
        return "/responses"
    if model == "union-alpha" or model.startswith("union-alpha-"):
        return "/messages"
    return "/chat/completions"


def scheme_for(protocols):
    for p in ("socks4", "socks5", "http"):
        if p in protocols:
            return p
    return ""


def classify(status, body):
    low = body.lower()
    if status == 200:
        return "ok"
    if "not available in your country" in low:
        return "geo-blocked"
    if "[user_blocked]" in body:
        return "user-blocked"
    if "[service_overloaded]" in body:
        return "overloaded"
    if status in (429, 529):
        return "rate-limited"
    return "other"


def build_body(model):
    # mirror what the proxy actually posts: /responses takes input +
    # responses-shaped tools, chat endpoints take messages + max_tokens.
    if endpoint(model) == "/responses":
        return {
            "model": model,
            "input": [{"role": "user", "content": "reply with exactly: PONG"}],
            "stream": True,
            "max_output_tokens": 16,
            "tools": [{"type": "function", "name": n,
                       "parameters": {"type": "object", "properties": {}}}
                      for n in ("bash", "read", "glob", "grep")],
        }
    return {
        "model": model,
        "messages": [{"role": "user", "content": "reply with exactly: PONG"}],
        "max_tokens": 16,
        "stream": True,
        "tools": GATE_TOOLS,
    }


def probe_one(proxy, country, model):
    url = ZEN + endpoint(model)
    body = build_body(model)
    cmd = [
        "curl", "-sS", "--max-time", "25", "-o", "-", "-w", "\n%{http_code}",
        "-X", "POST", url, "--proxy", proxy,
        "-H", "Content-Type: application/json",
        "-H", "Accept: */*",
        "-H", "Authorization: Bearer public",
        "-H", f"User-Agent: {UA}",
        "-H", "x-opencode-client: cli",
        "-H", "x-opencode-project: global",
        "-H", f"x-opencode-session: {new_id('ses')}",
        "-H", f"x-opencode-request: {new_id('msg')}",
        "--data-binary", "@-",
    ]
    try:
        p = subprocess.run(cmd, input=json.dumps(body).encode(),
                           capture_output=True, timeout=30)
    except Exception as e:  # noqa: BLE001 - timeout, missing curl, etc
        return (proxy, country, model, 0, "net-error", str(e)[:160])
    if p.returncode != 0:
        err = p.stderr.decode("utf-8", "replace").strip().splitlines()
        return (proxy, country, model, 0, "net-error", (err[-1] if err else f"curl rc={p.returncode}")[:160])
    out = p.stdout.decode("utf-8", "replace")
    *body_lines, code_line = out.rsplit("\n", 1)
    try:
        status = int(code_line.strip())
    except ValueError:
        return (proxy, country, model, 0, "net-error", "bad curl status line")
    text = "\n".join(body_lines)
    snippet = " ".join(text.split())[:160]
    return (proxy, country, model, status, classify(status, text), snippet)


def main():
    args = sys.argv[1:]
    models = ["big-pickle"]
    threads = 20
    limit = 0
    only_country = ""
    out_path = "zen_probe.tsv"
    per_country = 0
    skip_path = ""
    retries = 0  # extra substitute probes per country when some fail
    i = 0
    while i < len(args):
        a = args[i]
        if a == "--models" and i + 1 < len(args):
            models = [m.strip() for m in args[i + 1].split(",") if m.strip()]
        elif a == "--threads" and i + 1 < len(args):
            threads = int(args[i + 1])
        elif a == "--limit" and i + 1 < len(args):
            limit = int(args[i + 1])
        elif a == "--country" and i + 1 < len(args):
            only_country = args[i + 1].upper()
        elif a == "--out" and i + 1 < len(args):
            out_path = args[i + 1]
        elif a == "--per-country" and i + 1 < len(args):
            per_country = int(args[i + 1])
        elif a == "--skip" and i + 1 < len(args):
            skip_path = args[i + 1]
        elif a == "--retries" and i + 1 < len(args):
            retries = int(args[i + 1])
        else:
            print(f"unknown arg: {a}", file=sys.stderr)
            return 1
        i += 2

    skip = set()
    if skip_path:
        try:
            with open(skip_path) as f:
                for line in f:
                    cells = line.rstrip("\n").split("\t")
                    if cells and cells[0].startswith(("http://", "socks4://", "socks5://")):
                        skip.add(cells[0].strip())
        except FileNotFoundError:
            pass

    req = urllib.request.Request(LIST_URL, headers={"User-Agent": UA})
    with urllib.request.urlopen(req, timeout=30) as r:
        entries = json.load(r)
    by_country = defaultdict(list)
    for e in entries:
        if not e.get("ip") or not e.get("port"):
            continue
        if e.get("https") is False:
            continue  # zen is https-only, no-tls proxies can never work
        scheme = scheme_for(e.get("protocols") or [])
        if not scheme:
            continue
        country = (e.get("country") or "??").upper()
        if only_country and country != only_country:
            continue
        proxy = f"{scheme}://{e['ip']}:{e['port']}"
        if proxy in skip:
            continue
        by_country[country].append((proxy, country))

    jobs = []
    if per_country:
        # n fresh proxies per country (+ spares), shuffled so dead prefixes
        # don't dominate; models cycle across the picks.
        for country in sorted(by_country):
            pool = by_country[country][:]
            secrets.SystemRandom().shuffle(pool)
            for k, (proxy, c) in enumerate(pool[:per_country + retries]):
                jobs.append((proxy, c, models[k % len(models)]))
    else:
        for country in sorted(by_country):
            for proxy, c in by_country[country]:
                for m in models:
                    jobs.append((proxy, c, m))
    if limit:
        jobs = jobs[:limit]
    print(f"{len(jobs)} probes ({len({j[0] for j in jobs})} proxies x {len(models)} models), {threads} threads")

    rows = []
    done = 0
    with ThreadPoolExecutor(max_workers=threads) as ex:
        for row in ex.map(lambda j: probe_one(*j), jobs):
            rows.append(row)
            done += 1
            if done % 50 == 0 or done == len(jobs):
                print(f"  {done}/{len(jobs)}", flush=True)

    with open(out_path, "w", newline="") as f:
        w = csv.writer(f, delimiter="\t")
        w.writerow(["proxy", "country", "model", "http_status", "result", "err"])
        w.writerows(rows)
    print(f"wrote {out_path}")

    tally = defaultdict(Counter)
    for _, country, _, _, result, _ in rows:
        tally[country][result] += 1
    print(f"\n{'country':<8} {'n':>5} {'ok':>5} {'geo':>5} {'user':>5} {'other/net':>9}")
    for c, counts in sorted(tally.items(), key=lambda kv: -kv[1]["geo-blocked"]):
        n = sum(counts.values())
        print(f"{c:<8} {n:>5} {counts['ok']:>5} {counts['geo-blocked']:>5} "
              f"{counts['user-blocked']:>5} {counts['other'] + counts['net-error'] + counts['overloaded'] + counts['rate-limited']:>9}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
