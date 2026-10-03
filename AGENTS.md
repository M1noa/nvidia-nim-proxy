# AGENTS.md — contributor guidance for LLM coding agents

Module `nimroute`. NimRoute is an OpenAI/Anthropic-compatible LLM
proxy/router in Go. Primary purpose: pool free-model access with
key/proxy/lane rotation to survive per-exit rate limits.

## 1. backends and routing

- **opencode zen**: free `opencode/<model>` ids, keyless. upstream
  `https://opencode.ai/zen/v1`. every request mimics the real opencode
  client (`setZenHeaders` in anthropic.go): `Authorization: Bearer public`,
  `x-opencode-client: cli`, `x-opencode-session: ses_*`,
  `x-opencode-request: msg_*`, `x-opencode-project: global`,
  `User-Agent: opencode/<version>` (live tag from sst/opencode releases,
  refreshed every 6h, default in zenproxy.go), plus
  `anthropic-version: 2023-06-01`. muse-spark models use `/responses`,
  everything else `/chat/completions`; shape follows the model, converted
  both ways (`convertToResponses` / `chatFromResponses`).
- **NVIDIA NIM**: upstream `integrate.api.nvidia.com`, needs
  `nvidia_keys` in config.yml. `nvidia/` prefix is listing-only, stripped
  before forwarding. no keys (or `nvidia.enabled: false`) = NIM 503s,
  `opencode/*` keeps working.
- `ServeHTTP` (main.go): `/status` + `/health` -> `StatusFor`;
  `/v1/models`; `/v1/messages`, `/v1/messages/count_tokens`,
  `/v1/messages/classifier`; `opencode/` prefix -> zen; else NIM.
  inbound bodies capped at 10MB (`bodyLimit`). service via
  `./nim run|start|stop|restart|status|logs|tail|install|uninstall`.

## 2. code quality rules

- smallest working diff. surgical edits: every changed line traces to the
  request. never "improve" adjacent code, comments, or formatting.
- no speculative scaffolding: no features beyond the ask, no single-use
  abstractions, no configurability nobody requested, no handling for
  impossible states.
- clean up only your own mess: drop imports/vars/functions your change
  orphaned. mention pre-existing dead code, don't delete it.
- comments: short, all lowercase. match neighboring style.
- behavior changes require tests: table-driven, owner boundary named
  (who owns the invariant). reproduce-first for bug fixes.
- green before commit: `gofmt -l .`, `go vet ./...`, `go test ./...`.
  ner build is separate: `go build -tags ner`.
- never interrupt the primary instance (:5419 live). test on a separate
  port with a scratch config until the change is verified end to end:
  copy `config.yml` to `config.test.yml`, set `server.port` to an unused
  port (e.g. 5420) and `usage.path` to a scratch file, then
  `CONFIG_FILE=config.test.yml ./nim-proxy`. only restart the primary
  (`./nim restart`) when done and confident. delete scratch configs,
  binaries, and usage files after.

## 3. config rules

- `config.yml.example` is the schema source of truth. `defaultConfig()`
  (config.go) holds runtime defaults; the two must agree.
- true-by-default bools need raw-doc presence checks in `loadConfigFile`:
  yaml can't tell absent from false, so check the raw map for the key
  before honoring a false (see `nvidia.enabled`, `zen.enabled`,
  `usage.enabled`, `status.show_usage`/`show_pool`, guardrail
  threshold/weights). any new default-true bool needs the same treatment.
- new keys: document in the example with a comment, give a default in
  `defaultConfig()`, wire the presence check if default-true.
  `mergeMissingKeys` is append-only commented (`# added:` header,
  idempotent); never wipe user values, malformed user yaml keeps the old
  running config.
- new `/status` fields need a `status.show_*` flag plus redaction in
  `StatusFor` (section 4).
- `models.params` globs are first-match-wins, applied by `injectParams`
  unless `inject.params: false` or the body carries `"_nim": true`.
  `context_length`/`description`/`nudge_no_tools` are proxy-local, never
  sent upstream. new models need a params entry or they silently get no
  defaults (TODO item 9: startup warn for unmatched `/v1/models` ids).
- usage tracking (`usage.go`): gated on `usage.enabled`, one json line
  per request to `nim-usage.jsonl` plus in-memory totals for `/status`.
  counters only move through `logUsage`; zero overhead when disabled.
  no local tokenizer exists anywhere: token counts are upstream `usage`
  verbatim (prompt/completion/total). never add one.
  `nimstatus.py` reads the jsonl for today/all-time display.

## 4. /status rules

- every new counter/section goes behind a `status.show_*` flag (default
  preserves current output). follow `poolSnapshot` + `show_pool` as the
  pattern: `Status()` builds, `StatusFor()` gates.
- auth redaction lives in `StatusFor`: when `auth.tokens` is set and the
  request is unauthenticated, hide keys, locks, zen session/proxy,
  lanes, pool, and usage — no matter what the flags say. model ids stay
  visible (always safe).
- never widen unauthenticated output without a flag defaulting off.

## 5. upstream error guidance

| signal | meaning | action |
|---|---|---|
| 429 | per-exit rate limit. lane cooldown (exp backoff, 10m cap) + failover. `pool_refresh_429s` lanes limited = stale pool, refresh fires | normal, no fix |
| 403 FreeTierError | per-exit-per-model burn (`burnExit`, `exit_burn_minutes` default 30m). lane forgets exit, rotates away | normal, no fix. do NOT inject gate stubs into tool-less bodies — the summarizer emits calls zen rejects ("tool call not allowed while generating summary"); `ensureZenTools` skips empty `tools` deliberately |
| 403 geo (`not available in your country`) / `[user_blocked]` | exit banned or region-blocked. `dropProxy` + failover; threshold (`error_threshold`, default 3) refreshes pool | normal, no fix |
| 502 / transport error | dead exit, dropped from pool (`dropProxy`, transport closed) | normal noise, no fix |
| 529 | treated as 429 in both NIM and zen paths | normal, no fix |
| persistent all-exit failure | no success for `stale_pool_secs` (default 15s) with all lanes cooling = stale pool/sessions: refresh + `resetStaleLanes` | fix the refresh/rotation path, not the retry loop |

Investigate only when rotation stops working: same burned exit reused,
cooldowns never clearing, refresh storms (see TODO item 10: raise
`pool_refresh_429s`, cooldown between refreshes before touching retry
logic).

## 6. skills

- writing docs, readmes, or reports: invoke the `anti-slop` skill first.
- auditing for vulns/secrets: call the `security-audit` skill, if
  installed; otherwise say so and do a manual pass.

## 7. landmines

- lanes key on `x-session-id` verbatim (`"hdr:" + headerSession` in
  `laneFor`). never normalize, hash, or trim it lane-side; pii session
  derivation (`piiSessionID`) is separate.
- proxy urls may embed `user:pass`. run `redactProxyUserinfo` before any
  proxy value reaches `/status` AND logs.
- never forward client credentials upstream: NIM path has a header
  deny-list (`authorization`, `x-api-key`, `x-session-id`, `cookie`,
  `host`, `origin`); zen has its own blocklist in `zenHeaderForwarded`
  (also drops `content-type`, `user-agent`, all `x-opencode-*`, which the
  proxy sets itself). new forwarded headers must pass both lists.
- several upstream reads are uncapped (`io.ReadAll` on response bodies
  in NIM/zen paths). cap any new read with `io.LimitReader`.
- `math/rand` is for sampling only (weighted picks, probes). secrets and
  session/message ids use `crypto/rand` (`zenID`); keep it that way.
- `mergeMissingKeys` is append-only comments. never reorder, rewrite, or
  reformat the user file.
- pii: `piiwiring.go` + `pii/` (`detect.go` heuristics, `vault.go` +
  `scan.go` vault/scan cache, `wordpiece.go` tokenizer, `ner_ort.go`
  behind `-tags ner`). lane eviction must not evict vault entries
  (different TTLs); scan-cache idle sweep via `pii.SweepScanIdle`.
