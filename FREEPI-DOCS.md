# FREEPI reverse engineering notes
source: `/tmp/free-pi-cli` (`dennisonbertram/free-pi-cli`, cloned 2026-10-03, depth 1)
proxy repo: `nimroute` repo (go, port 5419)

> THIS PROJECT WAS MADE PARTIALLY USING AGENTIC AI CODING TOOLS

## 1. what free-pi is
- free ad-supported distro of the pi coding agent (`@earendil-works/pi-coding-agent` 0.84.4). inference paid by ads.
- base url: `https://api.freepi.ai` (`FREEPI_BASE_URL` override). config + jwt under `~/.free-pi/agent`, creds file `credentials.json` mode 0600 shape `{token: <jwt>}`.
- completions = openai-compatible `POST {base}/v1/chat/completions`. pi's openai-completions api appends `/chat/completions` to baseURL, so provider baseUrl must end in `/v1` (`completionsBaseUrl` in provider.ts). default model `deepseek/deepseek-v4-flash`.
- every completion carries `Authorization: Bearer <jwt>` + static headers `x-session-id: <pi logical session id>` and `x-client-version: <cli semver>`. server enforces one live session per jwt (409) and min cli version (426).
- model list/timing below is catalog-derived, not live-tested (sandbox dns blocked api.free-pi.dev during research; scripts now fixed to api.freepi.ai). run scripts/freepi_probe.py with a jwt to fill in live data.

## 2. auth (github device flow, no client secret)
- `POST /auth/github/device {}` -> `{session_id, user_code, verification_uri, interval}`. retry 4x on network fail only (1.5s * attempt), http errors returned as-is.
- user opens verification_uri + enters user_code. cli immediately `POST /auth/consent {session_id, consent_version}` (current version `"2"` in consent.ts) so token poll is not 403-gated.
- `POST /auth/token {session_id}` polled every max(interval, retry_after)s -> `{status:"pending", retry_after?}` | `{token: <jwt>}` | `{code:"consent_required", message}`. 403 + consent_required body = consent missed. network fails tolerated 6x in a row (floor 5s), counter resets on any http response.
- jwt is 90-day credential shared across processes (provider.ts comment). `GET /me` with `Authorization: Bearer <jwt>` validates: 401 = logged out (delete creds, re-login), anything else = keep.
- login script: `python3 scripts/freepi_login.py [--out freepi_jwt.txt]` prints the github url + code, waits for enter, submits consent, polls, saves jwt, verifies via /me. paste the jwt back here for probing.

## 3. endpoints (all json; auth = `Authorization: Bearer <jwt>` unless noted)

| method + path | auth | req body / query | resp | client use |
|---|---|---|---|---|
| `POST /auth/github/device` | none | `{}` | `{session_id, user_code, verification_uri, interval}` | login start |
| `POST /auth/consent` | none | `{session_id, consent_version}` | 200 `{ok:true}` | login, right after device code shown |
| `POST /auth/token` | none | `{session_id}` | pending / `{token}` / consent_required | login poll |
| `GET /client-version` | none | - | `{min, latest, model?, notice?, models?[{id, name}]}` | startup gate; 3s timeout, any failure = proceed |
| `GET /me` | jwt | - | `MeResponse` (tier, caps, credit_usd, notice?, buy_url?) | token check, buy page, meter |
| `GET /me/stats` | jwt | - | `MeStatsResponse` (spend/tokens/lifetime/credit_credits) | /usage, usage tool |
| `POST /v1/chat/completions` | jwt + x-session-id + x-client-version | openai chat (model, messages, stream, max_tokens, temperature, tools, passthrough) | openai chat completion (stream sse or json) | inference |
| `GET /ads/next?slot=banner\|inline` | jwt | - | 200 `{ad_id, click_token, creative, click_url}` / 204 no ad | banner + inline widgets |
| `POST /ads/impression` | jwt + x-session-id | `{ad_id, click_token}` | 200 (ignored) | fired once per shown ad, best-effort |
| `POST /session/reset` | jwt | - (empty post) | `{released: bool}` | /close-other-session |
| `POST /telemetry/tool-guard` | jwt | `{tool_name}` | 200 (ignored) | rogue-tool report, best-effort 3s |

curl examples (replace $JWT):
```
curl -s https://api.freepi.ai/client-version
curl -s -X POST https://api.freepi.ai/auth/github/device -H 'content-type: application/json' -d '{}'
curl -s -H "authorization: Bearer $JWT" https://api.freepi.ai/me
curl -s -H "authorization: Bearer $JWT" https://api.freepi.ai/me/stats
curl -s -N -H "authorization: Bearer $JWT" -H 'content-type: application/json' \
  -H 'x-session-id: abc123' -H 'x-client-version: 0.2.19' \
  -d '{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":true}' \
  https://api.freepi.ai/v1/chat/completions
curl -s -H "authorization: Bearer $JWT" "https://api.freepi.ai/ads/next?slot=banner"
curl -s -X POST -H "authorization: Bearer $JWT" -H 'content-type: application/json' \
  -d '{"ad_id":"...","click_token":"..."}' https://api.freepi.ai/ads/impression
curl -s -X POST -H "authorization: Bearer $JWT" https://api.freepi.ai/session/reset
```

## 4. ads api (detail)
- fetch: `GET /ads/next?slot=<banner|inline>`, bearer jwt. 204 or any failure = no ad, widget cleared, no retry. 3s abort timeout.
- shape 200: `{ad_id, click_token, creative: {headline, body, cta, accent}, click_url}`. click_url is a `/c/<token>` tracking redirect, shown to user as OSC8 hyperlink text = human cta, never the raw url.
- impression: `POST /ads/impression {ad_id, click_token}` with bearer + `x-session-id`. dedup per renderer instance: same click_token never double-posts (each /ads/next mints a fresh token server-side, so dedup only suppresses repaints). failures swallowed.
- cadence: banner renders on session_start + every 10min interval (unref'd, torn down on session_shutdown); inline renders after every Nth assistant turn (default 5, env `AD_INLINE_TURN_FREQUENCY`), visible exactly one turn then cleared. under 60 cols (`AD_MIN_COLUMNS`) both degrade to one plain line.
- meter: `GET /me` polled on session_start + every turn_end. shows nothing normally; shows server `notice` (sanitized plain text) if set, `daily_cap` line when `remaining_usd_today <= 0`, or a `Payment received - N credits added` line for 3 turns after credit_usd rises.
- creative is untrusted input: client strips ANSI/OSC/C0/C1 + collapses whitespace before render. card rows all share one sha256(click_url)[:12] OSC8 id so the whole card is one clickable link.
- gate tie-in: `ads_required` 403 (code in error envelope) fires when a `restricted` account has no confirmed impression in the trailing window — bypassing the ads client pauses free completions. /support opens the current banner click_url in a browser (no server call, no reward).

## 5. models
- ids come from `GET /client-version` `models[]`; `id` is the exact `model` string sent in completions (server resolves against its model_catalog), `name` is the picker label. no `models` field = single model in `model` field, fallback builtin `deepseek/deepseek-v4-flash`.
- live catalog 2026-10-03 (`GET /client-version`: min 0.2.13, latest 0.2.19): 7 models, all verified working via openai-compatible `POST /v1/chat/completions` (stream + non-stream).
- provider config per model: `reasoning: false, input: [text], cost 0, contextWindow 1048576, maxTokens 8192`. server forces the real upstream per request, so these are display/routing values.
- probe: `python3 scripts/freepi_probe.py [--jwt freepi_jwt.txt] [--models id1,id2] [--list-only]`.
- live bench 2026-10-03 (long-form story prompt, max_tokens 400, streamed; ttft = first visible content token, tps = server-reported completion_tokens / generation window):

| model id | picker label | usage mult | upstream (provider field) | ttft | tps | notes |
|---|---|---|---|---|---|---|
| `deepseek-v4-flash` | DeepSeek V4 Flash | 1x | - | 1.1s | ~199 | fastest ttft+tps combo |
| `deepseek-v4.1-flash` | DeepSeek V4.1 Flash | ~0.1x | Venice (`deepseek/deepseek-v4.1-flash`) | 1.7s to first byte, all reasoning | ~46 (400 tok / 8.7s wall) | streams `reasoning`+`reasoning_details` deltas, content empty until reason done; cheapest |
| `glm-5.3-flash` | GLM 5.3 Flash | ~0.7x | Z.AI (`z-ai/glm-5.3-flash`) | 3.2s to first byte, all reasoning | ~36 (400 / 11.1s) | same reasoning-stream shape as 4.1 |
| `gpt-oss-120b-speed` | GPT-OSS 120B | ~1.6x | AkashML (`openai/gpt-oss-120b`) | 0.6s to first byte, all reasoning | ~42 (400 / 9.5s) | fastest first byte, reasoning-heavy |
| `mimo-v2.6-flash` | MiMo V2.6 Flash (Xiaomi) | ~0.4x | - | 2.7s | ~58 | clean content stream, finish stop |
| `muse-spark-1.3-contributor` | Muse Spark 1.3 Contributor, trains on your data (free-pi + Meta) | ~0.5x | Meta (`meta/muse-spark-1.3-contributor`) | 4.6s then 1 reasoning summary chunk | n/a (0 content in 400 budget — all reasoning) | reasoning-only: 1 summary chunk then `finish_reason: length` with `native_finish_reason: max_output_tokens`; usage shows completion_tokens=400 all reasoning; needs much larger max_tokens for content |
| `solar-pro-4` | Solar Pro 4 | ~1.6x | - | 0.8s | ~75 | clean content stream, finish stop |

- response envelope extras (not plain openai): each chunk carries `provider`, `model` (real upstream id), sometimes `service_tier`; final chunk carries `usage {prompt_tokens, completion_tokens, cost, cost_details.upstream_inference_*}` and `native_finish_reason`. reasoning models add `delta.reasoning` + `delta.reasoning_details[]`.
- whole session cost: 20 reqs, 932 prompt + 4711 completion tokens, $0.0029 of $2.50 daily cap (tier `established`).

## 6. error contract (server envelope `{code, message}`, status-mapped)
- 429 + Retry-After: `daily_cap` (free allowance spent), `rolling_cap` (5h spend cap), `concurrent` (one request at a time), `hourly_ceiling` (per-hour completion starts).
- 409: `concurrent_session` (second session on same jwt while lease fresh — do NOT auto-retry; pi retried 3x and printed it 5x, 2026-08-18).
- 403: `consent_required`, `account_review` (suspended), `ads_required` (no confirmed impression).
- 413 `context_ceiling`, 502 `upstream_error`, 503 `global_cap`, 426 `upgrade_required` (cli below min).
- client surfaces one server-owned line via turn_end error envelope (`"<status>: {json}"`), deduped 15s per code. 429s retried by pi sdk 3x internally.

## 7. mimicking the client (go proxy notes)
- fingerprint: bearer jwt only credential; no api key, no device id, no fingerprint headers. per-request headers exactly
: `authorization: Bearer <jwt>`, `content-type: application/json` (posts), `x-session-id: <stable id per logical session
>`, `x-client-version: <semver e.g. 0.2.19>`. no User-Agent set by client (fetch default). keep ONE x-session-id per acc
ount — second concurrent stream 409s.
- chat body: openai schema, `model` = catalog id verbatim, messages passthrough (tools/tool_choice survive). server reso
lves model itself; wrong id presumably errors, probe to confirm.
- ads to stay undetectable: poll `GET /ads/next?slot=banner` at session start + 10min, `slot=inline` every ~5th turn; `P
OST /ads/impression {ad_id, click_token}` once per fresh click_token with same x-session-id; `GET /me` every turn (meter
). never reuse a click_token, never post an impression without a preceding /ads/next for that token. `ads_required` 403
is the detector for skipping this.
- version gate: `GET /client-version` (no auth) at startup; below `min` = 426 on completions, so send a current x-client
-version.
- request pacing: tool-guard posts to `/telemetry/tool-guard` only on anomaly — a proxy never needs it. usage reads (`/m
e`, `/me/stats`) are GETs; `/session/reset` is the only other POST and only on user request.
- auth: bearer jwt only. per-request headers the official client sends: `authorization: Bearer <jwt>`, `content-type: application/json` (posts), `x-session-id`, `x-client-version`.
- chat body: openai schema, `model` = catalog id verbatim. server resolves model itself.
- ads fund the inference. the official client fetches ads and posts impressions as documented in §4. faking impressions or stripping ads to get completions without the ad support is not documented here — use `npx free-pi-cli` or get permission from the operator before building any third-party integration.
- version gate: `GET /client-version` (no auth) at startup; below `min` = 426 on completions.
