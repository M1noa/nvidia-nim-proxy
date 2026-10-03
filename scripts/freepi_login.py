#!/usr/bin/env python3
# freepi login helper (stdlib only). runs the github device flow and saves
# the jwt to a local file for freepi_probe.py.
# usage: python3 scripts/freepi_login.py [--out freepi_jwt.txt]
import json, sys, time, urllib.request, urllib.error

BASE = "https://api.freepi.ai"
CONSENT_VERSION = "2"


def api(method, path, body=None, token=None):
    req = urllib.request.Request(BASE + path, method=method,
        data=json.dumps(body).encode() if body is not None else None,
        headers={"Content-Type": "application/json",
                 **({"Authorization": f"Bearer {token}"} if token else {})})
    try:
        with urllib.request.urlopen(req, timeout=20) as r:
            raw = r.read()
            return r.status, json.loads(raw) if raw else {}
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            return e.code, json.loads(raw) if raw else {}
        except Exception:
            return e.code, {"_raw": raw.decode(errors="replace")}


def main():
    out = "freepi_jwt.txt"
    for i, a in enumerate(sys.argv[1:]):
        if a == "--out":
            out = sys.argv[i + 2]

    st, dev = api("POST", "/auth/github/device", {})
    assert st == 200 and "session_id" in dev, f"device start failed: {st} {dev}"
    sid, interval = dev["session_id"], dev.get("interval", 5)
    print(f"open {dev['verification_uri']} and enter code: {dev['user_code']}")
    input("sign in with github in the browser, then press enter here... ")

    st, res = api("POST", "/auth/consent",
                  {"session_id": sid, "consent_version": CONSENT_VERSION})
    assert st == 200, f"consent failed: {st} {res}"

    fails = 0
    while True:
        try:
            st, res = api("POST", "/auth/token", {"session_id": sid})
            fails = 0
        except Exception as e:
            fails += 1
            if fails >= 6:
                sys.exit(f"network unreachable: {e}")
            time.sleep(max(interval, 5))
            continue
        if st == 403 and res.get("code") == "consent_required":
            sys.exit("server did not record consent, restart login.")
        if st != 200:
            sys.exit(f"token poll failed: {st} {res}")
        if "token" in res:
            break
        time.sleep(max(interval, res.get("retry_after", 0)))

    open(out, "w").write(res["token"])
    print(f"saved jwt to {out}")

    st, me = api("GET", "/me", token=res["token"])
    print(f"token check: HTTP {st} {me if st != 200 else me.get('handle')}")


if __name__ == "__main__":
    main()
