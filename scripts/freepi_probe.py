#!/usr/bin/env python3
# freepi model prober (stdlib only). lists the catalog, checks account,
# fetches ads, then benchmarks every model for ttft + tps via streaming.
# usage: python3 scripts/freepi_probe.py [--jwt freepi_jwt.txt] [--models id1,id2]
#        python3 scripts/freepi_probe.py --list-only
import json, sys, time, urllib.request, urllib.error, uuid

BASE = "https://api.freepi.ai"
CLI_VERSION = "0.2.19"
PROMPT = "Reply with exactly: ok"
MAX_TOKENS = 64


def req(method, path, body=None, token=None, stream=False):
    data = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(BASE + path, method=method, data=data,
        headers={"Content-Type": "application/json",
                 "x-session-id": SESSION, "x-client-version": CLI_VERSION,
                 **({"Authorization": f"Bearer {token}"} if token else {})})
    try:
        resp = urllib.request.urlopen(r, timeout=120)
        if stream:
            return resp.status, resp  # caller reads + closes
        with resp as x:
            raw = x.read()
        return resp.status, json.loads(raw) if raw else {}
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            return e.code, json.loads(raw) if raw else {}
        except Exception:
            return e.code, {"_raw": raw.decode(errors="replace")[:300]}


def bench(model, token):
    t0 = time.time()
    first = None
    chunks = 0
    text = []
    st, resp = req("POST", "/v1/chat/completions",
                   {"model": model, "messages": [{"role": "user", "content": PROMPT}],
                    "stream": True, "max_tokens": MAX_TOKENS}, token, stream=True)
    if st != 200:
        resp.close() if hasattr(resp, "close") else None
        return {"model": model, "error": f"HTTP {st}: {resp}"}
    try:
        with resp as r:
            for line in r:
                line = line.decode(errors="replace").strip()
                if not line.startswith("data:"):
                    continue
                data = line[5:].strip()
                if data == "[DONE]":
                    break
                try:
                    d = json.loads(data).get("choices", [{}])[0].get("delta", {})
                    c = d.get("content")
                except Exception:
                    continue
                if c:
                    if first is None:
                        first = time.time()
                    chunks += 1
                    text.append(c if isinstance(c, str) else "".join(
                        p.get("text", "") for p in c if isinstance(p, dict)))
    except Exception as e:
        return {"model": model, "error": f"stream fail: {e}"}
    t1 = time.time()
    out = "".join(text)
    ttft = (first - t0) if first else None
    gen = (t1 - first) if first else 0
    toks = len(out.split())  # rough word-based tps; exact counts need usage field
    return {"model": model, "ttft_s": round(ttft, 2) if ttft else None,
            "total_s": round(t1 - t0, 2), "words": len(out.split()),
            "wps": round(toks / gen, 1) if gen > 0 else 0,
            "sample": out[:120]}


SESSION = uuid.uuid4().hex[:8]


def main():
    jwt_file, only, models = "freepi_jwt.txt", False, None
    args = sys.argv[1:]
    for i, a in enumerate(args):
        if a == "--jwt":
            jwt_file = args[i + 1]
        elif a == "--list-only":
            only = True
        elif a == "--models":
            models = args[i + 1].split(",")
    try:
        token = open(jwt_file).read().strip()
    except FileNotFoundError:
        sys.exit(f"no jwt file {jwt_file}; run freepi_login.py first.")

    st, cv = req("GET", "/client-version")
    print(f"client-version [{st}]: {json.dumps(cv)}")
    catalog = models or [m["id"] for m in (cv.get("models") or [])]
    if not catalog:
        catalog = [cv.get("model", "deepseek/deepseek-v4-flash")]

    st, me = req("GET", "/me", token=token)
    print(f"/me [{st}]: {json.dumps(me)}")
    st, stats = req("GET", "/me/stats", token=token)
    print(f"/me/stats [{st}]: {json.dumps(stats)}")
    for slot in ("banner", "inline"):
        st, ad = req("GET", f"/ads/next?slot={slot}", token=token)
        print(f"/ads/next?slot={slot} [{st}]: {json.dumps(ad)[:300]}")
    if only:
        return
    print(f"\nprobing {len(catalog)} models (prompt: {PROMPT!r})...")
    results = [bench(m, token) for m in catalog]
    for r in results:
        print(json.dumps(r))
    print("\n--- summary ---")
    for r in results:
        if "error" in r:
            print(f"{r['model']}: ERROR {r['error']}")
        else:
            print(f"{r['model']}: ttft={r['ttft_s']}s total={r['total_s']}s "
                  f"words={r['words']} wps={r['wps']}")


if __name__ == "__main__":
    main()
