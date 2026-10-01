# TODO

sorted simplest first. each item keeps current defaults unchanged.

## 1. config: merge new keys on update, never wipe user config [small]

`loadOrCreateConfig` only seeds `config.yml` when the file is missing. when
`config.yml.example` gains keys (new feature), existing configs never get
them and reload keeps running on stale defaults.

- on startup, compare example keys against the user file; append missing
  sections/keys as commented defaults, preserving everything the user set.
- malformed user yaml must still keep the old running config (already does
  in `watchConfig`; keep that).
- test: seed an old config, start, assert new keys appear commented and
  user values untouched.

## 2. status: proxy-pool + lane section, with config flags [small]

`/status` shows lanes (`zen_lanes`) but not the underlying pool: verified
proxy count, per-exit latency/country, pool refresh age, lane cap/usage.

- add `pool: {verified, candidates, last_refresh_ago}` + per-exit rows
  (proxy, country, latency_ms, pinned_lane?) to `StatusResponse`.
- `status:` flags: `show_pool` (default true). respect the existing
  unauthenticated redaction in `StatusFor` (hide when tokens set + no token).
- dead exits already drop via `dropProxy`; surface `dropped_total` counter.

## 3. guardrail tuning knobs in config [small]

threshold `0.6` (`guardrailMatchThreshold`), window/density weights
(`0.7 + 0.3*density` in `guardrailScore`), and the enable switch exist but
only `enabled/file/extra` are configurable.

- add `guardrails.threshold`, `guardrails.window_weight`,
  `guardrails.density_weight`, keep defaults `0.6` / `0.7` / `0.3`.
- `enabled: false` already short-circuits in `stripSomeGuardrails`; keep.
- test: same input strips at default, passes at `threshold: 1.0`.

## 4. required zen params on every request shape [medium]

title-gen/summary calls 403 (`FreeTierError`) because they skip what
agentic calls carry: the four gate tools (`ensureZenTools` only fills
`tools` when the key exists and is non-empty) plus `stream: true` +
`stream_options.include_usage`.

- always run `ensureZenTools` shape fixup: when `tools` is missing/empty,
  still inject the four gate stubs (they are never invoked; zen only
  checks presence). verify against the current 403: title-gen body vs
  agentic body.
- audit `handleOpenCode` + `handleOpenCodeAnthropic` + jev path: every
  outbound body must carry model, stream, stream_options, tools, and the
  `x-opencode-*` header set. add a `assertZenBody` test helper.
- `nudge_no_tools`/cache keys must keep being stripped (`stripCacheFields`).

## 5. zen lanes: 6m session-idle eviction + scan-cache TTL [medium]

lanes expire by `lane_ttl_minutes` (default 30m) swept on the pool tick;
the ask is 6m of no activity clears the session hash/id from cache.

- add `zen.session_idle_minutes` (default 6): `sweepLanes` drops lanes
  idle past it (keep `lane_ttl_minutes` as the hard cap, default 30).
- scan-cache entries keyed by paragraph hash already exist in `pii/scan.go`;
  give them a last-access timestamp and evict after 6m idle on the same
  sweep. `vault_ttl_hours` (24h) stays for the vault itself.
- per-session surrogate maps already live in the vault; evicting the lane
  must not evict vault entries (different TTLs).

## 6. anonymize config wiring pass [medium]

most knobs exist (`mode`, `fuzzy_threshold`, `detect_secrets`,
`detect_pii`, `keep_labels`, `include_system`, `on_timeout`,
`vault_ttl_hours`, `vault_max_size`, entities/terms, ner block). gaps:

- `replacement` on entities is accepted but undocumented in the example;
  document it. `terms` flat list duplicates entity names; keep both,
  document precedence (entities win).
- `label` mode ([LABEL_N] rampart-style) is in the mode comment but
  verify the vault actually emits it; add a round-trip test per mode.
- `disclose_text: ""` falls back to built-in; document that empty means
  default, not silent (silent = `disclose: false`).
- confirm `include_system: true` also covers the injected helpful line,
  not just client system prompts.

## 7. rampart NER: wire the existing build, verify cache [large]

status: rampart IS implemented (`pii/` package: `detect.go` ports the
heuristics, `wordpiece.go` is the tokenizer, `ner_ort.go` loads the model
behind `-tags ner`, `vault.go`/`scan.go` are vault + scan cache, NOTICE
credits rampart CC BY 4.0). what remains is wiring + verification:

- confirm `-tags ner` builds and the model lazy-loads once (no per-request
  load); NER timeout (`timeout_ms`, default 500) follows `on_timeout`.
- verify the scan cache keys on paragraph hash + config version and skips
  rescan of unchanged history; add a cache-hit counter to `/status`
  (fits item 2 work).
- benchmark: regex us/KB + NER ms/request, log at startup when ner enabled.
- `fetch_model.sh` downloads weights; never commit them.

## 8. proxy pool hardening per the rough outline [large]

lanes exist (`zenlanes.go`) but several outline behaviors are missing:

- per-(proxy,session) lock held during the call + 2s after (like nvidia
  key postpone), so concurrent same-lane requests serialize instead of
  bursting one exit.
- two-tier refresh: full refresh every 30m (`pool_refresh_...`, new
  `pool_refresh_minutes` default 30) PLUS early refresh when all exits
  429 for >15s with no success.
- exit health check: on start and before pinning, probe must complete
  within `max_response_ms + 150ms` AND a zen probe call must not be
  rate-limited before the exit is pinned.
- lane cap default is 5; outline asks 6. change default `lanes: 5` -> `6`
  in example + `laneCap` fallback, keep user values.
- overflow past cap: assign to least-recently-used lane (already does via
  shared pool fallback; verify under a 8-concurrent test).

## 9. full model params audit [large]

`models.params` covers the main models but a new/renamed model silently
gets no params. zen 400/403s when required fields are missing.

- on startup, log models in `/v1/models` output with no matching params
  entry (warn, don't fail).
- add a test that every `opencode/*` id returned by `fetchZenModelIDs`
  matches at least one params glob.
- keep `inject.params: false` as the escape hatch; document `_nim: true`
  per-request opt-out (already in example).
