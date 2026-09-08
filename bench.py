#!/usr/bin/env python3
# bench.py - measure ttft + throughput for every model in model_params.jsonc via the proxy
# usage: ./bench.py [runs] [max_tokens]

import fnmatch
import json
import re
import sys
import time
import urllib.request

BASE = "http://localhost:5419"
PARAMS_FILE = "model_params.jsonc"
PROMPT = "Say hi in one word."


def load_patterns():
    src = open(PARAMS_FILE).read()
    src = re.sub(r"//[^\n]*", "", src)  # strip jsonc comments
    return [e["pattern"] for e in json.loads(src)]


def list_models():
    with urllib.request.urlopen(BASE + "/v1/models", timeout=10) as r:
        return [m["id"] for m in json.load(r)["data"]]


def bench(model, max_tokens):
    body = json.dumps({
        "model": model,
        "messages": [{"role": "user", "content": PROMPT}],
        "max_tokens": max_tokens,
        "stream": True,
        "stream_options": {"include_usage": True},
    }).encode()
    req = urllib.request.Request(
        BASE + "/v1/chat/completions",
        data=body,
        headers={"Content-Type": "application/json", "Authorization": "Bearer bench"},
    )
    t0 = time.monotonic()
    ttft = None
    chunks = 0
    usage = None
    with urllib.request.urlopen(req, timeout=300) as resp:
        for raw in resp:
            line = raw.decode("utf-8", "replace").strip()
            if not line.startswith("data:"):
                continue
            data = line[5:].strip()
            if data == "[DONE]":
                break
            try:
                evt = json.loads(data)
            except json.JSONDecodeError:
                continue
            if evt.get("error"):
                raise RuntimeError(evt["error"].get("message", "upstream error"))
            if evt.get("usage"):
                usage = evt["usage"]
            for ch in evt.get("choices") or []:
                delta = ch.get("delta") or {}
                text = (delta.get("content") or "") + (delta.get("reasoning_content") or "")
                if text:
                    if ttft is None:
                        ttft = time.monotonic() - t0
                    chunks += 1
    total = time.monotonic() - t0
    if ttft is None:
        raise RuntimeError("no tokens received")
    tokens = (usage or {}).get("completion_tokens") or chunks
    # streams through the proxy often arrive in bursts, so measure over total time
    tps = tokens / total if total > 0 else 0.0
    return ttft, total, tokens, tps


def main():
    runs = int(sys.argv[1]) if len(sys.argv) > 1 else 1
    max_tokens = int(sys.argv[2]) if len(sys.argv) > 2 else 64

    patterns = load_patterns()
    models = list_models()
    matched = []
    for pat in patterns:
        matched += [m for m in models if fnmatch.fnmatch(m, pat) and m not in matched]

    if not matched:
        sys.exit("no models matched - is the proxy running?")

    print(f"benching {len(matched)} models, {runs} run(s) each, max_tokens={max_tokens}\n")

    results = {}  # model -> list of (ttft, total, tokens, tps)
    for model in matched:
        rows = []
        for i in range(runs):
            try:
                rows.append(bench(model, max_tokens))
            except Exception as e:
                print(f"  {model}: run {i + 1} failed: {e}")
        if rows:
            results[model] = rows

    if not results:
        sys.exit("all runs failed")

    # best-of runs per model, sorted by ttft
    table = sorted(
        ((m, min(r[0] for r in rs), min(r[1] for r in rs), max(r[3] for r in rs)) for m, rs in results.items()),
        key=lambda x: x[1],
    )

    print(f"{'model':45s} {'ttft':>8s} {'total':>8s} {'tok/s':>8s}")
    print("-" * 73)
    for model, ttft, total, tps in table:
        print(f"{model:45s} {ttft * 1000:7.0f}ms {total:7.2f}s {tps:8.1f}")

    fastest = table[0]
    print(f"\nfastest ttft: {fastest[0]} ({fastest[1] * 1000:.0f}ms)")


if __name__ == "__main__":
    main()
