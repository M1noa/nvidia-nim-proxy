# PLAN.md — lanes, status, freepi, service fix

source: user requests 2026-10-03. order: service fix first (blocks
restarts), then lanes/pool, status UI, freepi last (biggest).

## 0. service restart/stop broken [bug, first]

symptom: `./nim restart|stop` leaves the proxy running.
cause: launchd still runs old label `com.user.nvidia-nim-proxy`
(`launchctl list` shows it); `nim` bootouts `com.user.nimroute`
after the rename, which matches nothing.
fix: `svc_stop` bootouts BOTH labels (old + new). one-line change
in `nim`, plus `deploy/` note. verify: restart kills old PID, new
PID serves, no orphans (`pgrep -af nim-proxy` shows one).
touches: `nim`.

## 1. lanes + pool perf [go]

1.1 `max_response_ms` 400→250: `config.go` default, live
`config.yml`, example. pool latencies run 5–240ms so this trims
the tail only.
1.2 convo-lane 1m idle expiry: new `zen.convo_idle_minutes`
(default 1). `sweepLanes` drops `convo:*` lanes idle past it.
`hdr:` and active shared lanes keep existing TTLs. agentic convos
stay alive: every request refreshes `lastUsed`.
1.3 two-request convo rule: `laneFor` skips convo-hash lanes until
the hash is seen twice (small seen-map). single-turn title-gen
falls to the shared pool, never pins a lane. explicit, cheap.
1.4 skip `/status` access logs: `ServeHTTP` doesn't `acclog` the
`-->` line for `/status`+`/health`. kills the 2/sec spam from
`status tail`.
1.5 clear log on start: `os.Truncate(nim-proxy.log)` in
`serverMain` (go-side, survives manual runs too).
1.6 memory audit (read-only first): `lanes` (cap+TTL), `burned`
map (deletes on read only — add sweep for never-read entries),
`totals` (per-model, bounded by model count), `guardrailPrefixes`
/`modelParams` (rebuilt on reload — verify no append leak), pii
vault/scan TTL+caps. fix what the audit finds.
tests: convo expiry, two-request rule, status silence, burn sweep.
verify: build, vet, full suite, scratch-port (:5420) live test.

## 2. status menu [nimstatus.py]

2.1 live refresh 2s→1s.
2.2 `fmt_tok` commas + B units (`1,234`, `5.6B`), both paths.
2.3 all-time start date: track `first_ts` in jsonl scan, show
`since Oct 1`.
2.4 models list: render `opencode.models` (already in /status)
as a compact wrapped line.
2.5 lane legend: one line explaining `convo` (sticky
conversation), `shared` (overflow pool), `hdr` (explicit pin).
2.6 tokenizer note: no local tokenizer — usage is upstream
`usage` verbatim. document in AGENTS.md so nobody "fixes" it.
verify: `./nim status`, tail-status, stdlib fallback.

## 3. freepi backend [go, big]

docs: FREEPI-DOCS.md (complete: auth, endpoints, ads, models,
errors, fingerprint). no /tmp clone present; docs suffice, clone
only if a behavior is ambiguous.

3.1 config: `freepi:` section — `enabled`, `accounts[]`
(`name`, `jwt`), `client_version` (default 0.2.19),
`ad_inline_every` (default 5). example docs.
3.2 auth: `./nim freepi auth` — github device flow
(`scripts/freepi_login.py` exists): prints url+code, polls,
writes jwt into `config.yml` accounts (append or named update).
multi-account: repeat, each gets a lane set.
3.3 backend: `freePiBase = https://api.freepi.ai`. per-request
headers exactly: `authorization: Bearer <jwt>`,
`x-session-id` (ONE stable id per account, minted once —
second stream 409s), `x-client-version`. model id verbatim.
`freeepi/<model>` prefix routes there; list via
`GET /client-version` models[] at startup (fallback builtin).
3.4 lanes: separate lane table keyed `freepi:<account>:...`,
same mechanics (cooldown, burns per model, failover) but never
shared with zen lanes. one account = one concurrent stream
(server 409s the second).
3.5 ads (silent): per account — `GET /ads/next?slot=banner`
at lane creation + 10min, `slot=inline` every ~5th turn,
`POST /ads/impression` once per fresh click_token,
`GET /me` per turn (meter). never reuse click_token, never
impress without a preceding next. failures swallowed. nothing
displayed (no status rows for ads).
3.6 errors: map freepi envelope per docs §6 — 429 daily/
rolling/concurrent/hourly (backoff + account failover),
409 concurrent_session (do NOT retry, rotate account),
403 ads_required (force impression cycle, retry once),
426 (bump client_version), 502/503 (failover).
3.7 status: separate `freepi:` section per account —
models, usage (req/tok via `GET /me/stats` + local counters),
proxy host/cc/ms, lane state. same redaction rules as zen.
3.8 verify: unit (auth parse, ad cadence, error map) + live
probe via scripts/freepi_probe.py with a real jwt (user
supplies via `./nim freepi auth`), scratch port only.

## 4. land

build, vet, full suite, scratch-port live test (chat +
anthropic + freepi with jwt), restart primary, verify single
PID + serving, commit, push github + gitlab, verify both SHAs.
delete /tmp clone if one was made.
