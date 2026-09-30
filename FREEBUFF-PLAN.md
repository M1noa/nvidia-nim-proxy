# FREEBUFF proxy integration plan
status: planning only. no proxy code touched. main instance (5419) untouched until 100% verified on test port.

## 0. goal
- `freebuff/<wire-id>` models on both `/v1/chat/completions` (+`/v1/responses`) and `/v1/messages` (claude code), same auth, same streaming SSE shapes as existing providers.
- `/v1/models` lists `freebuff/*`; `/status` shows freebuff usage + ads.
- ad flow runs exactly like the real `Freebuff-CLI`: auction + impression, browser-like UA, real conversation slice, idle gating, impression only after a real served turn. `show_ads:false` hides text in `/status` but still POSTs impressions (legit billing, infinite usage appearance).
- ads fetched only when useful: session start, 60s rotation while in use, or when freebucks/quota low — never on idle poll.

## 1. new files (2 max)
- `freebuff.go` — everything: config struct, account pool, session manager, ad manager, chat forward, anthropic+openai translate, status snapshot. no changes to anthropic.go/zenproxy.go logic.
- `main_test.go` additions only (new test funcs, no refactor).

## 2. config.yml additions
```yaml
freebuff:
  enabled: false
  test_port: 5420
  accounts:
    - name: "acc1"
      token: "<authToken from ~/.config/manicode/credentials.json>"
  show_ads: true        # false = still POST impressions, hide text in /status
  ad_rotation_sec: 60
  session_poll_sec: 30
  timeout_ms: 20000
```
- token source documented: login via real `freebuff` binary once, copy `authToken`. proxy never stores email, only bearer.
- multi-account = same rotation pattern as `Pool.keys` (pick least-recently-failed, per-model lockout like `ModelLock`).

## 3. request lifecycle (per chat turn)
1. strip `freebuff/` prefix → wire id (e.g. `freebuff/deepseek-v4-flash` → `deepseek/deepseek-v4-flash`). reject unknown ids with 400 + valid list.
2. pick account (sticky per api-key caller like `PickSticky`, skip 429/backoff).
3. ensure session: `GET /api/v1/freebuff/session` (heartbeat headers if instance held) → if `none/ended/gone` → `POST /api/v1/freebuff/session/admission` with `x-freebuff-model`, `x-freebuff-wallet-spend-limit: 0`. handle `model_locked` (DELETE other-model instance? no — keep per-model instance map, one slot per model per account), `rate_limited/spend_limited/ip_capped/country_blocked/banned` → surface as 429/403 with upstream message, mark account cooldown.
4. forward chat: normalize inbound (anthropic messages OR openai messages) → openai `POST https://codebuff.com/api/v1/chat/completions` with `model=<wire-id>`, `codebuff_metadata={freebuff_instance_id, surface:'cli'}`, headers `Authorization: Bearer <token>`, `user-agent: Freebuff-CLI/<ver>`. stream SSE bytes through untouched (same pattern as `callUpstream`).
5. gate-code check on error body: match `error`+`status` per `FREEBUFF_GATE_CODES`; `endsTheSession` → drop instance, re-admit once, retry once; `keep` → return typed 429/428 to client so claude code retries.
6. after first response bytes flow → fire ad impression for the currently held ad (ties billable event to real usage), update freebucks snapshot from next session GET.

## 4. ad manager (realistic, lazy)
- state per account+model: `heldAd {adText,title,cta,url,impUrl,receivedAtMs}`, `expiresAt`, `ackedImpressions set`, `lastActivity`.
- auction triggers: session admission (1x), then only if `now-lastServe > ad_rotation_sec` AND requests served since last auction > 0 (mirrors 60s dock + 30s activity gate). extra trigger: freebucks remaining < 20% or `rate_limited` seen → auction immediately (credits-low path).
- auction request = byte-shape of `buildAdAuctionRequest`: `POST https://codebuff.com/api/v1/ads`, UA `Freebuff-CLI/0.0.x`, body `{messages: last ≤6 user/assistant text turns of THIS proxy conversation, sessionId: <uuid per proxy session>, device: {os,timezone,locale of host}, userAgent: <chrome-151 per-os>, surface:'cli_chat'}`. no traceContext/sponsoredCapability (omit = server fallback, per source).
- impression: `POST .../api/v1/ads/impression {impUrl, mode:'FREE', userAgent, os, renderDelayMs}` + `x-freebuff-event-id: uuid`, once per held fill, only after a chat turn actually streamed. zeroclick ids if present.
- `/status` shows `adText/title/cta/url` when `show_ads:true`, else `"ads":"hidden (processing)"` + counters (`auctions, impressions, lastAdAt`). counters always increment.

## 5. status + models wiring
- `StatusResponse` += `Freebuff *FreebuffInfo {accounts: [{name, suffix, sessionModel, sessionExpiresIn, freebucksDaily, freebucksWallet, rateLimited}], ads: {auctions, impressions, lastAdAt, currentAd?}}`. reuse existing `Status()` lock pattern.
- `handleModels`: append `freebuff/<wire-id>` for full catalog when enabled (limited-tier ids only if account tier == limited, detected from session GET `accessTier`).
- anthropic path: map `freebuff/*` model → same forward; `handleAnthropic` already converts tools, keep that, just swap upstream target + headers (like zen `always_proxy` branch pattern).

## 6. realism checklist (must all hold in test)
- UA pair matches (request UA product, body UA browser-like chrome-151).
- os/timezone/locale = host values, consistent across auction+impression.
- messages slice = real proxy turns, text-only, latest user wrapped `<user_message>`.
- 1 impression per fill max; no impression without a served turn; no auction while idle.
- heartbeat GET keeps slot; DELETE on clean shutdown (test binary only).
- concurrent callers share one session per model (mutex), no admission stampede.

## 7. test protocol (separate port, main untouched)
1. build `nim-proxy-test` from clean tree + new file, run with `config-test.yml` (port 5420, `enabled:true`, 1 token).
2. `GET /status` → freebuff block present, no ads text leak when `show_ads:false`.
3. `GET /v1/models` → `freebuff/*` listed.
4. openai: `POST :5420/v1/chat/completions {model: freebuff/z-ai/glm-5.3-flash, stream:true}` → 200 SSE, text flows.
5. anthropic: `POST :5420/v1/messages {model: freebuff/deepseek/deepseek-v4-flash}` via cctest.py pattern → valid `message` SSE.
6. all-models sweep: loop catalog (script), record per-model ok/rate_limited/gated.
7. idle check: no ad auction for 5 idle min (log grep); usage check: 3 turns → ≥1 impression logged.
8. audit: `go vet`, race run, diff review (new file + config + status struct only).
9. only then: copy to main config with `enabled:true`, restart main 5419, re-run steps 3-5 against 5419.

## 8b. auto behaviors (new asks)
- model refresh: background goroutine per `model_refresh_sec` (300s): session GET per account → union(`rateLimitsByModel`, `limitedModelOffers`, `freebucks.prices`) → rebuild `freebuff/*` list + per-model premium/limited flags → atomic swap into `handleModels` + claude_map fallback targets. stale flag if refresh fails.
- live usage in /status: per account `{daily remaining/limit + resetIn, wallet balance, monthly $ left}` from latest freebucks block; per model `used/limit` from `rateLimitsByModel`; per active turn `{model, account, sessionExpiresIn, egress}`. poll `session_poll_sec` (30s) + refresh after every served turn (piggyback, no extra timer).
- realistic ads: auction only on (admission | rotation expiry + served-since | freebucks<20% | rate_limited seen). body uses last ≤6 REAL proxy turns. impression only after streamed turn, once per fill. counters + last ad timestamps always live.
- rotation on limit: `ended/expired/superseded` → rotate instance, re-admit once. `rate_limited/spend_limited` → next account same model. `model_locked` → use that model's own slot. `ip_capped` or `anonymous_network country_blocked` → quarantine egress, try next egress, else surface 429. all transitions logged + visible in /status chain.
- egress/proxy: reuse zen `proxies/proxy_file` dialer. default direct (proxies likely trigger anonymous_network blocks — verify in testing phase C8 before relying on them).

## 8. explicit non-goals
- no full agent tool loop (single-turn passthrough; claude code drives tools client-side).
- no sponsored/agentic-offer route (no workspace capability to offer).
- no login flow in proxy (token provisioned out-of-band).
- no partner placements (no TUI slots to fill).
- no credits-grant logic (free mode grants nothing; we just report server balances).
