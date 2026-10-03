# FREEBUFF TODO
order matters. check off only with log evidence. main 5419 restart is last.

## phase A — verify (no code)
- [x] A1. backend live: www.codebuff.com canonical (codebuff.com 301/307s). healthz 200.
- [x] A2. scripts/freebuff_login.py works: acc1 (baccusminoa@gmail.com), full tier, 100 Freebucks/day. NOTE: acc1 BANNED 2026-09-28 from probe burst (~20 admissions/30min). treat accounts as scarce.
- [x] A3. admission works (legacy path, server UUID instanceId). chat recipe cracked (agent-run + Buffy marker + cost_mode free) but ALL models 503 at provider lane. open: retry with backoff, real-binary comparison.
- [x] A4. ads auction live (real gravity fill). impression shape confirmed (validates impUrl before auth). impression ack untested (no served turn to bill).
- [x] A5. shapes in FREEBUFF-DOCS.md §9c. ban triggers documented.
- [ ] A6. resolve chat 503: backoff retry over 24h + run real `freebuff` binary once, compare. NO further sweeps on scarce accounts.

## phase B — implement (test port only)
- [ ] B1. `freebuff.go`: config struct + account pool + per-model session map + mutexes
- [ ] B2. session manager: GET/POST/DELETE admission, compact-merge, typed-error parse, gate-code table
- [ ] B3. chat forward: openai normalize + SSE passthrough + single retry on ends-session codes
- [ ] B4. anthropic mapping: `freebuff/*` routes through same forward inside `handleAnthropic` target switch
- [ ] B5. ad manager: lazy auction, hold/TTL, single-ack impression, counters, `show_ads` gate
- [ ] B6. wiring: `/v1/models` list, `/status` block, config.yml parse + hot-reload, test-port boot
- [ ] B7. unit tests in main_test.go: gate-code classify, compact-merge, ad dedupe, model strip

## phase C — test sweep (port 5420)
- [ ] C1. `/status` + `/v1/models` shape checks (both `show_ads` values)
- [ ] C2. openai streaming turn on 3 models (1 standard, 1 premium, 1 Muse Spark)
- [ ] C3. anthropic turn via scripts/cctest.py pattern
- [ ] C4. full catalog sweep script, per-model result table
- [ ] C5. ad realism: idle 5min = 0 auctions; 3 turns = ≥1 impression; no dup impUrl ack
- [ ] C6. failure paths: bad token 401, bad model 400, `model_locked` second-model turn, gate-code retry
- [ ] C7. `go vet` + race + diff audit (only freebuff.go, config, status struct, tests)

## phase B+ — auto behaviors
- [ ] B8. model refresh loop (union of quota/offer/price keys, atomic swap, stale flag)
- [ ] B9. live usage block in /status (freebucks + per-model quotas + active turn chain)
- [ ] B10. rotation engine (session→account→egress with quarantine + logging)
- [ ] B11. egress dialer reuse from zen + per-egress country_blocked stats

## phase C+ — proxy-behavior tests
- [ ] C8. egress test: direct vs 1-2 proxies → record `country_blocked/anonymous_network` rate; decide if proxies help or hurt
- [ ] C9. rotation test: force session end (DELETE) mid-use → new instance, no dropped client stream; exhaust model quota → account switch visible in /status
- [ ] C10. refresh test: model list updates without restart; stale flag on failed refresh

## phase D — go live
- [ ] D1. user says go (explicit)
- [ ] D2. enable in main config.yml, restart 5419
- [ ] D3. repeat C1–C3 against 5419, confirm `/status` shows live freebucks + ads
- [ ] D4. cleanup: kill test port, archive sweep logs
