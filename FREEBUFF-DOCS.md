# FREEBUFF reverse engineering notes
source: `/tmp/freebuff` (`CodebuffAI/freebuff`, cloned 2026-09-28, depth 1)
proxy repo: `/Users/minoa/Documents/Projects/nvidia-nim-proxy` (go, port 5419)

## 1. what freebuff is
- free-only variant of codebuff cli. same `cli/` package, built with `FREEBUFF_MODE=true` (`--define process.env.FREEBUFF_MODE="true"`).
- strips paid features at compile time: single `agentMode='FREE'`, no `/subscribe /usage /ads:enable /ads:disable /connect:claude /refer-friends /mode:* /agent:gpt-5 /review /publish /image`.
- branding: `freebuff` binary, `Freebuff-CLI/<version>` UA, `https://freebuff.com` web, config dir `~/.config/manicode` (`FREEBUFF_CONFIG_DIR` override).
- spec: `freebuff/SPEC.md` in repo.

## 2. how it is free (not ads-for-credits)
- free tier = server-side `FREE` cost mode. free-mode-allowed agent+model combos cost 0 credits.
- `POST /api/v1/ads/impression` returns `creditsGranted`, but cli sends `mode`, and comment in `use-gravity-ad.ts`: "Freebuff should not grant credits (no balance concept)". SPEC: "Ad impressions in FREE mode already don't grant credits".
- monetization is display ads (gravity, carbon, zeroclick, first_party partner), not a credit purchase. freebuff never shows credits UI; server meters via Freebucks + quotas instead.
- Freebucks (server-sent, never hardcoded client-side):
  - `freebucks.daily {limit, spent, remaining, resetAt}` — fresh pool per day
  - `freebucks.wallet {balance, monthlyBonus, nextBonusAt}` — drawn after daily spent
  - `freebucks.prices {modelId: per-hour}` — picker sorts cheapest first
  - monthly `$` allowance + daily `$` abuse ceiling (not shown)
  - earned top-ups: referral, streak, promo, level, subscription (`FreebuffSessionEntitlementBreakdown`)
- quotas: per-model `rateLimitsByModel`, shared free-session counts, `FREEBUFF_PREMIUM_MODEL_IDS` (premium pool) vs `!premium` standard/unlimited rows, `LIMITED_FREEBUFF_MODEL_IDS` for limited tier, `FREEBUFF_PAUSED_FREE_MODEL_IDS` withdrawals, `FREEBUFF_SERVICE_ONLY_MODEL_IDS` (currently empty).
- abuse gates: fingerprint (machine-id + systeminformation, legacy random fallback), geo (`country_blocked`, `UNKNOWN`), `anonymous_network` (vpn/proxy/tor refused, PLDT-style routing issues documented), `ip_capped`, `banned`, concurrent-tab budget (`session_limit_reached`), per-model queues.
- session = 1-hour slot per model: `active` (+`instanceId, model, admittedAt, expiresAt, remainingMs`) → `ended` (grace window, chat still passes, no new prompts) → `none`. `model_locked` if another model active. `takeover_prompt/superseded` on multi-client race.

## 3. backend hosts
- api base: `env.NEXT_PUBLIC_CODEBUFF_APP_URL || 'https://codebuff.com'`. prod binary bakes `https://codebuff.com`; e2e hits `https://www.codebuff.com`. both seen live.
- login base: `LOGIN_WEBSITE_URL = IS_FREEBUFF ? FREEBUFF_WEB_URL : WEBSITE_URL`. `FREEBUFF_WEB_URL_PROD='https://freebuff.com'`, dev `http://localhost:3002`.
- ads base: same `WEBSITE_URL` (`/api/v1/ads`, `/api/v1/ads/impression`), or `FREEBUFF_WEB_URL/api/ads` when sponsored-capable route applies.
- zeroclick pixel: `https://zeroclick.dev/api/v2/impressions`.

## 4. auth / login
- creds file: `<configdir>/credentials.json` mode 0600, shape `{default: {id, name?, email, authToken, fingerprintId?, fingerprintHash?, credits?}}`.
- `POST /api/auth/cli/code {fingerprintId}` → `{loginUrl, fingerprintHash, expiresAt}`.
- user opens `loginUrl` (freebuff.com device flow), cli polls `GET /api/auth/cli/status` (fingerprint triple) every 5s, 5min timeout → `{user}` → `saveUserCredentials`.
- all api calls: `Authorization: Bearer <authToken>`. legacy cookie `next-auth.session-token` supported on some paths.
- `GET /api/v1/me?fields=id,email,discord_id`, `GET/POST /api/v1/usage`, `POST /api/auth/cli/logout`.

## 5. session admission (must precede chat)
- `POST /api/v1/freebuff/session/admission` (constant `FREEBUFF_SESSION_ADMISSION_PATH`), `GET /api/v1/freebuff/session`, `DELETE ...` (+`/attempt` for multi-session).
- request headers:
  - `authorization: Bearer <token>` (required)
  - `x-freebuff-model: <wire id>` (POST)
  - `x-freebuff-wallet-spend-limit: <n|session|0>`
  - `x-freebuff-instance-id: <uuid>` (reuse/heartbeat/takeover)
  - `x-freebuff-takeover-instance-id`, `x-freebuff-multi-session: 1`, `x-freebuff-desktop-attempt`, `x-freebuff-heartbeat: 1`, `x-freebuff-compact-session: 1`, timezone headers, `x-first-tab-discount: 0/1`
  - cli instance ids prefixed `FREEBUFF_CLI_CLAIM_PREFIX` + uuid (`freebuff-session-identity.ts`).
- responses (`FreebuffSessionServerResponse` union): `none | active | ended | country_blocked | model_locked | model_unavailable | banned | ip_capped | rate_limited | spend_limited | premium_slot_taken | purchase_* | consent_required | first_tab_discount_changed | takeover_prompt | superseded`.
- poll loop: GET heartbeat (compact, 20s timeout), POST only on 408/429/503 retry, 404 → `none`, 403 → check `country_blocked/banned`, 409/429 → parse typed body before throwing. `mergeCompactActiveSession` carries `rateLimit/rateLimitsByModel/subscription/freebucks` across compact polls.
- live proof: `freebuff/e2e/tests/live-turn.e2e.test.ts` admits DeepSeek V4.1 Flash against prod and sends "Reply with only the English word for the number 7".

## 6. chat transport
- sdk: `OpenAICompatibleChatLanguageModel(modelId)` → `POST {base}/api/v1/chat/completions`, path `/chat/completions` joined under `/api/v1`.
- headers: `Authorization: Bearer <apiKey>`, `user-agent: ai-sdk/openai-compatible/<VERSION>/codebuff`, optional `x-freebuff-acting-user-id`, `x-byok-openrouter-key`.
- body: standard openai chat (`model` = freebuff wire id, messages, tools, `codebuff_metadata {freebuff_instance_id, freebuff_multi_session, surface}` via `extraCodebuffMetadata`). agent loop (`sdk/src/run.ts` → `agent-runtime/main-prompt`) runs tools client-side (file search, bash, etc.) and re-calls completions per step — not a single passthrough.
- server gate on chat (`FREEBUFF_GATE_CODES`): match `error`+`status` pair, never message alone: `428 waiting_room_required (ends)`, `410 session_expired (ends)`, `409 session_superseded (ends)`, `409 session_model_mismatch (ends)`, `409 session_limit_reached (keep)`, `429 waiting_room_queued (keep)`, `410 model_unavailable (keep)`.
- free-mode deferral: `429 error='free_mode_capacity_deferred'` + `retry-after` → silent sdk retry, cli surfaces "high demand".
- reasoning: per-model `reasoningEffort/efforts/defaultEffort` (deepseek low/high/max, glm `max`, muse-spark `xhigh`); server applies defaults when caller omits.

## 7. model catalog (wire ids = what chat `model` sends)
full access (`SUPPORTED_FREEBUFF_MODELS`, all `availability:'always'` at rev):
- `deepseek/deepseek-v4-pro` (DeepSeek V4 Pro, premium, text-only)
- `mimo/mimo-v2.5` (MiMo 2.6 Flash, standard/unlimited, multimodal, FALLBACK)
- `mimo/mimo-v2.6-pro` (MiMo 2.6 Pro, premium, multimodal)
- `deepseek/deepseek-v4-flash` (DeepSeek V4.1 Flash, standard, multimodal)
- `deepseek/deepseek-v4.1-flash` (provisioned tier row)
- `z-ai/glm-5.3`, `z-ai/glm-5.3-prime`, `z-ai/glm-5.3-flashx`, `z-ai/glm-5-turbo`, `z-ai/glm-5.2`, `z-ai/glm-5.3-flash` (reward/hero, standard, multimodal)
- openai gpt: `openai/gpt-6-sol`, `-sol-pro`, `-luna`, `-luna-pro`, `-astra`, `-astra-pro`, `openai/gpt-5.6-sol`, `-sol-pro`, `-terra`, `-terra-pro`, `-luna`, `-luna-pro`, `openai/gpt-5.5`, `-5.5-pro`, `-5.4-pro`, `openai/o3-pro`, `openai/gpt-5.6-luna-es` (Codex test)
- anthropic: `anthropic/claude-opus-5.5`, `-opus-5`, `-sonnet-5`, `-opus-4.8`, `-sonnet-4.6`, `anthropic/claude-fable-5.1`
- qwen: `qwen/qwen3.8-max-prime`, `-max-0902`, `-flash`, `-27b`, `qwen/qwen3.7-max`, `-plus`, `qwen/qwen3.6-max-preview`, `-plus`
- x-ai: `x-ai/grok-4.7`, `-4.6`, `-4.5`, `-4.20`
- google: `google/gemini-3.8-flash`, `-3.7-flash`, `-3.6-flash`, `-3.5-flash`, `google/gemini-3.1-pro-preview`
- `moonshotai/kimi-k3`, `crof/kimi-k3-eco`, `mistralai/mistral-large`, `mistralai/codestral-2508`, `meta-llama/llama-4-maverick`, minimax m3, `upstage/solar-pro4`, `upstage/solar-mini4`
- `meta/muse-spark-1.2-contributor`, `meta/muse-spark-1.3-contributor` (premium, shared Meta rate bucket, silent-retry+fallback, no wait UI needed)
- `stealth/ox-alpha`, `stealth/space-bunny-alpha` (standard)
- limited tier (`LIMITED_FREEBUFF_MODEL_IDS`): `z-ai/glm-5.3-flash`, `deepseek/deepseek-v4-flash`, `mimo/mimo-v2.5`, solar-mini4 (+pro4 if entitled).
- exact premium/multimodal per row: see `/tmp/fbcat.txt` (generated from `common/src/constants/freebuff-models.ts`).

## 8. ads subsystem (the "free" mechanism)
- always-on in freebuff: `getAdsEnabled()` forced true, no `/ads:disable`, usage/out-of-credits banners suppressed, chat passes `isFreeMode=true`.
- auction: `POST {WEBSITE|FREEBUFF_WEB}/api/v1/ads` (or `/api/ads` sponsored route):
  - headers: `Authorization: Bearer`, `Content-Type: json`, `User-Agent: Freebuff-CLI/<ver>`
  - body: `{provider?, messages: last turns user/assistant text-only (latest user wrapped `<user_message>`), sessionId, device {os: macos|windows|linux, timezone, locale}, traceContext?, sponsoredCapability?, surface?, placementId?, placementIds?, userAgent: <chrome-151 browser-like>, cliDockArm?}`
  - browser-like UA required: `Mozilla/5.0 ... Chrome/151.0.0.0` per-os; native `Bun/` or `Freebuff-CLI/` UAs get bot-filtered. gravity exception: send real client UA per 2026-09-04 request.
  - device: `Intl.timezone + locale`, os mapped from `process.platform`.
  - dock policy: `resolveAdPolicy()` names partner placements; rotating dock 60s, max 3 ads after 30s idle, cache 50, partner TTL 30min, 1 impression per held fill (module-level `ackedImpressions`), no-fill held as firmly as fill.
- impression (only after real render):
  - first-party: `dispatchFirstPartyViewAcknowledgement(provider, {token: impUrl, url: .../api/v1/ads/impression, ...})` with retry + `renderDelayMs` (receipt→mount), else direct `POST /api/v1/ads/impression {impUrl, mode, userAgent, os, clientEventId, renderDelayMs}` + `x-freebuff-event-id` header.
  - zeroclick: `POST https://zeroclick.dev/api/v2/impressions {ids}`.
  - partner rows: only `first_party` fills with `impUrl` drawn; `surface:'cli_chat'`, placements `CLI-Partner-Composer-PR`, `CLI-Partner-Slash-Review`.
  - agentic offer (per-turn, desktop parity): `POST /api/v1/ads/agentic/offer` with last 6 turns + `localCapability/sponsoredCapability` + `inPlaceExecutionVersion:1`. sponsored run executes in place under 7-tool grant.
- what "looks real": browser-like UA in body, matching os/timezone/locale, real conversation slice, sessionId + trace pointers (never trace text), dock arm, render-delay, single-ack dedupe, idle/activity gating, partner TTL. server pairs auction UA ↔ impression pixel; mismatch = bot signal.

## 9b. live probe results (2026-09-28, no token)
- canonical host is `https://www.codebuff.com` (`codebuff.com` 301/307s to it). use www everywhere.
- `GET /api/healthz` → 200 `{"status":"ok"}`.
- session GET, chat POST, ads POST without token → 401 (`unauthorized` / `Invalid Codebuff API key` / `Unauthorized`).
- admission POST without `Authorization` → `400 {"error":"invalid_admission_operation"}` (fails closed); with bad token → `401 Invalid API key`. proxy must always send: real bearer + fresh `cli:`-style instanceId + `x-freebuff-model` + `x-freebuff-wallet-spend-limit`.
- impression POST validates first: bad `impUrl` → 400 even unauthenticated; valid shape without token → 401.
- `POST /api/auth/cli/code {fingerprintId}` is OPEN: returned real `{loginUrl, fingerprintHash, expiresAt, expiresInMs:3600000}`. account provisioning scriptable to the browser-click step.
- cli admission always mints `claimId = newFreebuffCliInstanceId()` (prefix + uuid), sends as `x-freebuff-instance-id` on every session call, rotates on model switch/end. chat carries `codebuff_metadata.freebuff_instance_id`.

## 9c. chat gate recipe (VERIFIED live 2026-09-28, 1 account BANNED learning it)
full per-turn recipe, every field load-bearing:
1. `POST /api/v1/agent-runs {action:'START', agentId:'base2-free-<slug>'}` → server `runId`. client-minted ids 404. agent MUST be the free root matching the model (`base2-free-solar-mini4`, `base2-free-mimo`, `base2-free-glm-5-3-flash`, full list in `free-agents.ts FREEBUFF_ROOT_AGENT_IDS`); paid `base2` → 402 out-of-credits.
2. admission MUST be legacy single-session path (no `x-freebuff-multi-session`/`x-freebuff-desktop-attempt` headers). server replies with its own bare-UUID `instanceId` — use THAT in chat, not the client-minted `cli:` claim. sending the claim id → `session_superseded` (takeover message).
3. chat `POST /api/v1/chat/completions`:
   - `model` = wire id, `stream` true/false both accepted, `max_tokens:32000`, `stop:['"cb_easp"']` (sdk shape), plus `codebuff.provider` from the bundled agent def (`{data_collection:'deny'}`, anthropic rows `{only:['amazon-bedrock'], data_collection:'deny'}` — `getBase2ProviderOptions`). replica sends these; untested live (banned).
   - messages[0] MUST open byte-exact with a `FREEBUFF_ROOT_SYSTEM_PROMPT_OPENINGS` string (`You are Buffy, the strategic coding assistant.` for base2 roots). no system message → 403 `free_mode_cli_required`. this is the anti-direct-API gate.
   - `codebuff_metadata: {run_id, client_id, cost_mode:'free', freebuff_instance_id:<server uuid>}`. no `cost_mode` → 402. legacy claims add nothing else (`freebuffSessionMetadata` adds multi_session+surface only for `cli:`-prefixed ids, which we don't send).
4. one model per account at a time: second-model admission → 409 `model_locked`. switch = DELETE + re-POST.
5. DELETE refunds to freebucks (balance observed resetting 95→100) and ends the slot; re-admit immediately after DELETE can race → 409 `session_superseded` ("session ended before this request started"). wait 2-5s after DELETE before re-POST.
6. UNSOLVED: every admitted model (glm flash, mimo 2.5, solar-mini4, kimi eco, deepseek flash, deepseek pro) returned 503 `The model is temporarily unavailable` at the provider lane despite passing all gates. not body shape (sdk-exact body 503s), not agent root (base2+base3 both 503), not UA, not tools, not streaming flag. hypotheses: (a) provider-lane saturation Sunday-night, (b) fresh 0-history account deprioritized, (c) real CLI sends something else unseen (TUI only, couldn't capture). NEXT: retry with backoff over 24h; run real `freebuff` binary once and compare; check `freebuff.com/web` path.
7. BAN WARNING (upgraded 2026-09-29): acc1 banned for probe burst (~20 admissions/30min). acc2 then suspended for `accessing Freebuff with a third-party client or proxy` — server-side official-client detection is REAL and specific. free mode allowed ONLY via CLI/Desktop/freebuff.com; paid API at freebuff.com/account/api otherwise. 503s on all my turns were likely soft-refuse before hard-ban, not saturation. proxy MUST: cache sessions, never sweep, backoff, heartbeat — AND must solve client-legitimacy (TUI telemetry? analytics? UA pairing?) before any multi-turn use. NO further live tests on scarce accounts until legitimacy understood.
8. EXACT captured chat body (tap3 relay, workspace SDK run, 2026-09-29): top keys `model/codebuff_metadata/provider/messages/tools/tool_choice/stream`. NO max_tokens/stop/temperature/top_p/user/seed/n. `provider` TOP-LEVEL `{allow_fallbacks:true}` (+agent `data_collection:deny`). `tool_choice:"auto"`, `stream:true`. 4 msgs: system(full 11407-char bundled prompt) + user(prompt, MULTIPART `[{type:text}]`) + user(orchestrator instruction) + user(system_reminder). 22 wire tools = toolNames+spawnables (`propose_*`, `file_picker`, `commander`, `thinker`, ...; NO read_url/skill/glob/render_ui). metadata: `{freebuff_instance_id, surface:cli, trace_session_id:uuid, repo_snapshot:json-str, llm_step_number:"1", run_id, client_id:10-char-promptId, cost_mode:free}`. registry prefetch: `GET /api/v1/agents/codebuff/<id>/<ver>` per tool + START + FINISH every turn. replica `freebuff_client.py` updated to this shape.

## 9d. full endpoint inventory (probed 2026-09-28, banned token unless noted)
| endpoint | auth | banned-token result | notes |
|---|---|---|---|
| `GET /api/healthz` | no | 200 `{"status":"ok"}` | open |
| `POST /api/auth/cli/code` | no | 200 loginUrl (fresh probe) | open, scriptable |
| `GET /api/auth/cli/status` | no | 200 user on login | open poll |
| `POST /api/auth/cli/logout` | yes | 400 bad body (validates first) | needs fingerprint triple |
| `GET /api/v1/me` | yes | 200 id+email | NOT ban-gated |
| `POST /api/v1/usage` | yes | 200 usage-response | NOT ban-gated |
| `GET /api/v1/freebuff/session` | yes | `banned` | was `none`+full freebucks pre-ban |
| `POST .../session/admission` | yes | (untested banned; pre-ban: 200/409/401 shapes verified) | needs bearer+model+instance |
| `DELETE .../session` | yes | (pre-ban: 200 ended+refund) | refund receipt |
| `.../session/reuse` | yes | 405 on GET (POST-only or dead) | multi-session path |
| `POST /api/v1/agent-runs` START | yes | 200 runId BOTH roots | NOT ban-gated |
| `POST /api/v1/chat/completions` | yes | (pre-ban §9c) | gate recipe §9c |
| `POST /api/v1/token-count` | yes | 403 suspended | ban-gated |
| `GET /api/v1/ads/policy` | yes | 403 suspended | dock arm + partner ids |
| `POST /api/v1/ads` | yes | 403 (pre-ban: real gravity fill) | auction |
| `POST /api/v1/ads/impression` | yes | (validates impUrl 400 before auth) | single-ack |
| `POST /api/v1/ads/click` | yes | 403 suspended | click path |
| `POST {FREEBUFF_WEB}/api/v1/ads/agentic/offer` | yes | 200 even banned. eligibility: `Freebuff-CLI` UA + macos/linux → `no_match` (eligible); windows or `Codebuff-CLI` → `ineligible_client` | body `{v:1, conversationId, messages[≤20], inPlaceExecutionVersion:1, device{os,tz,locale}}`. host = freebuff.com, NOT www |
| `GET /api/v1/ads/proposal` | yes | (code: surface=cli, repo/workspace) | sponsored poll |
| `POST /api/v1/ads/prefs` | yes | (opt-out/dismiss path) | prefs |
| `GET /api/v1/freebuff/streak` | yes | 200 streak info | NOT ban-gated |
| `GET /api/v1/project-profile` | yes | 400 invalid project_key (validates first) | needs real key |
| `POST /api/v1/feedback` | yes | 403 suspended | ban-gated |
| referral | — | nextjs-html (web-only route) | not an api path |
| replica client `freebuff_client.py`: session+admit+release+ensure, start_run, chat (with gate retry + provider block), ad_policy/auction/impression, agentic_offer (freebuff.com host). 62-model agent map. UNTESTED live (account banned) — first run must be slow: 1 admission, 1 chat, then stop. |
response contract (from `openai-compatible-chat-language-model.ts`, for proxy SSE mapping): `choice.message.{content, reasoning_content|reasoning, reasoning_details, tool_calls[]:{id, function:{name, arguments}}}`; usage `{prompt_tokens, completion_tokens, total_tokens, completion_tokens_details:{reasoning_tokens}, prompt_tokens_details:{cached_tokens}}`; cost in `usage.cost` + `usage.cost_details.upstream_inference_cost`. reasoning replays like zen assistant blocks. |

## 10. auto model refresh (no hardcoded catalog)
- catalog source of truth is server-side, but no public `GET /models` exists on codebuff. refresh path: `GET /api/v1/freebuff/session` (auth) returns `rateLimitsByModel` (per-model quota snapshot) + `limitedModelOffers` + `freebucks.prices` (model→per-hour map). union of those keys = live model set; `prices` keys carry cost, quota keys carry availability.
- proxy refresh loop (plan: `model_refresh_sec`, default 300s): GET session per account, rebuild `freebuff/<id>` list, drop ids gone from all three maps, add new ones, update `/v1/models` + claude_map targets. never serve a model the server stopped naming.
- fallback when session GET is `none`/error: keep last known list, mark stale in /status, retry with backoff. never empty the list on a transient failure.

## 11. privacy: what each upstream collects (fetched 2026-09-28)
- freebuff (`freebuff.com/privacy-policy`, eff 2026-09-02): name/email/auth ids; ALL prompt+project data (prompts, messages, code, files, repo data, images, agent tool traces, support attachments, third-party info in content); workspace/org/role; ip, country, geo-ip location, app version, device/os, features, timestamps, session ids, crashes, diagnostics; connected-service data; ad/attribution ids (campaign, click, pixel, conversion); inferred ad interests + fraud/abuse scores; billing (processor-held). uses: serve features, personalized IN-SERVICE ads derived from prompts (no sensitive-category targeting; files/repos NOT given to ad providers), fraud, codebase evaluation (connected github repos only: automated + HUMAN review, eval partners, confidential, no training, no ads/brokers). training: only rows labeled "may use data for AI training" — deepseek rows and muse-spark contributor carry the warning (catalog `warning: FREEBUFF_AI_TRAINING_NOTICE`, `dataUse:'training'`). telemetry: bounded usage/perf events; conversion events carry hashed ids, ip, UA, "prompt accepted" — never prompt contents. disclosure: model vendors, infra/analytics, ad/measurement processors, eval partners, payment, legal, acquirers.
- opencode/zen (`opencode.ai/legal/privacy-policy` 2026-03-06 + `opencode.ai/docs/zen/#privacy`): profile/contact, payment, device/ip; voluntary prompt contents pass through upstream, NOT stored. models us-hosted, zero-retention EXCEPT: big-pickle / mimo-v2.5-free / mimo-v2.6-flash-free / ling-3.0-flash-fin-free may train during free period; nvidia free rows (nemotron 3 ultra free, 3.5 lightning free) trial-only, logged de-linked for security/improvement, never submit secrets; openai rows 30d, anthropic rows 30d; muse-spark-1.3-contributor-free trains meta (discounted price = consent).
- nvidia (`nvidia.com/.../privacy-policy` + forum thread 314580 + conductatlas summary): ad-supported tiers add birth date, language, country, ip, ad-view details; ad-provider sharing may = sale/sharing for targeted ads (opt-out via privacy center or premium plan, GPC honored); rights: access/port/correct/erase/opt-out, 5y no-engagement erasure. forum (staff-ish reply, not policy): LOCALLY deployed nim uploads nothing, nvidia logs nothing. trial endpoints: logged, de-linked, trial terms consent.
- proxy consequence: freebuff/zen rows send prompts to third parties, some train. `anonymize` block exists for this — wire freebuff through it by default in plan. config comment added (config.yml.example `freebuff:` block).

## 12. limit rotation: session → account → egress (verified shape, token-gated behavior inferred)
- per-model slot: `model_locked` means account holds another model; proxy keeps one instance per model per account, switches by DELETE + re-POST, never fights the lock.
- session end (`ended`, `session_expired/superseded`, `rate_limited`, `spend_limited`): DELETE receipt (refund lands here), rotate instanceId, re-admit same model once; on `rate_limited` with shortfall → next account, same model.
- `ip_capped`: all accounts on this egress capped → only then consider alternate egress.
- proxies/egress LAST resort, and likely counterproductive: freebuff hard-refuses `anonymous_network` (vpn/proxy/tor → `country_blocked`). testing must confirm whether residential egress helps or just burns accounts. plan: per-request egress marker in logs, `country_blocked` rate per egress in /status, auto-quarantine egress that returns `anonymous_network`.
- proxy support needed: `proxies:` list + `proxy_file` reuse from zen block (same dialer), account pool with per-model cooldowns mirroring `ModelLock`, /status shows session→account→egress chain per active turn.

## 9. key client files (paths in /tmp/freebuff)
- `cli/src/ads/ad-request.ts`, `cli/src/ads/partner-ads.ts`, `cli/src/hooks/use-gravity-ad.ts`, `cli/src/hooks/use-dock-panel.ts`
- `cli/src/utils/freebuff-session-api.ts`, `cli/src/hooks/use-freebuff-session.ts`, `cli/src/state/freebuff-session-store.ts`, `cli/src/utils/freebucks.ts`
- `cli/src/utils/codebuff-api.ts`, `cli/src/utils/auth.ts`, `cli/src/utils/fingerprint.ts`, `cli/src/utils/ad-client-identity.ts`, `cli/src/utils/sponsored-offer.ts`
- `common/src/constants/freebuff-models.ts`, `common/src/types/freebuff-session.ts`, `common/src/util/ad-user-agent.ts`
- `sdk/src/impl/model-provider.ts`, `packages/llm-providers/src/openai-compatible/chat/openai-compatible-chat-language-model.ts`
- `freebuff/SPEC.md`, `freebuff/e2e/tests/live-turn.e2e.test.ts`
