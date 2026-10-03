package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"nimroute/pii"
)

// zen lanes: pinned proxy+session pairs so concurrent conversations spread
// across exit ips instead of hammering one into 429s. lane identity falls
// back down a chain: x-session-id header -> conversation hash (first user
// + last assistant block) -> shared lru pool.

type zenLane struct {
	id        string // lane key (header value, convo hash, or shared-n)
	session   string // stable ses_* for this lane, minted once
	proxy     string // pinned exit
	country   string
	lastUsed  time.Time
	cooldown  time.Time // 429 backoff until
	fails     int       // consecutive 429s, drives backoff
	requests  int
	rateLimit int
	// lastSend is the pacing clock: a lane waits lane_min_gap_ms between
	// sends so one exit never sees a burst from a single conversation.
	lastSend time.Time
	// escalated latches once a direct attempt proved unusable, so the
	// lane keeps using a proxy even when always_proxy is off.
	escalated bool
}

type laneStatus struct {
	ID        string `json:"id"`
	Proxy     string `json:"proxy"`
	Country   string `json:"country"`
	LastUsed  string `json:"last_used_ago"`
	Cooldown  string `json:"cooldown_remaining,omitempty"`
	Requests  int    `json:"requests"`
	RateLimit int    `json:"rate_limited"`
}

var (
	lanesMu sync.Mutex
	lanes   = map[string]*zenLane{}
	// convoSeen counts convo-hash sightings so lanes pin only from the
	// second request. tiny: hashes evicted on the sweep with the lanes.
	convoSeenMu sync.Mutex
	convoSeen   = map[string]int{}
)

// seenConvoLocked reports whether a convo hash was seen before.
// caller holds lanesMu (same critical section as lane assignment).
func seenConvoLocked(h string) bool {
	convoSeenMu.Lock()
	defer convoSeenMu.Unlock()
	return convoSeen[h] > 0
}

// noteConvoLocked records a first sighting. caller holds lanesMu.
func noteConvoLocked(h string) {
	convoSeenMu.Lock()
	defer convoSeenMu.Unlock()
	convoSeen[h]++
	if len(convoSeen) > 4*laneCap()+64 {
		for k := range convoSeen {
			delete(convoSeen, k)
			break
		}
	}
}

// dropConvoSeen forgets a sighting when its lane is swept. caller holds
// lanesMu; takes convoSeenMu (leaf lock, no inversion: convoSeenMu never
// acquires lanesMu).
func dropConvoSeen(k string) {
	convoSeenMu.Lock()
	defer convoSeenMu.Unlock()
	delete(convoSeen, k)
}

// exit burns: an exit that 403s FreeTierError for a model is skipped for
// that model until the burn lapses. per (exit, model), not per exit: a
// burned-for-pickle exit can still serve space-bunny. without this every
// lane retries the same fast (and burned) exits and a servable model 403s.
var (
	burnMu sync.Mutex
	burned = map[string]time.Time{}
)

func burnTTL() time.Duration {
	if m := cfg().Zen.ExitBurnMinutes; m > 0 {
		return time.Duration(m) * time.Minute
	}
	return 30 * time.Minute
}

func burnKey(proxy, model string) string { return proxy + "\x00" + model }

// burnExit marks proxy as refusing model until the burn lapses. "" proxy
// (direct) is not burnable: there is only one direct exit and laneBlock
// already handles it.
func burnExit(proxy, model string) {
	if proxy == "" || model == "" {
		return
	}
	burnMu.Lock()
	burned[burnKey(proxy, model)] = time.Now().Add(burnTTL())
	burnMu.Unlock()
}

func exitBurned(proxy, model string) bool {
	if proxy == "" || model == "" {
		return false
	}
	burnMu.Lock()
	defer burnMu.Unlock()
	until, ok := burned[burnKey(proxy, model)]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(burned, burnKey(proxy, model))
		return false
	}
	return true
}

// sweepBurns drops lapsed burns. exitBurned deletes on read, but entries
// for dropped exits or renamed models are never read again and would
// linger: the pool churns hourly, so this runs on the same sweep.
func sweepBurns() {
	burnMu.Lock()
	defer burnMu.Unlock()
	now := time.Now()
	for k, until := range burned {
		if now.After(until) {
			delete(burned, k)
		}
	}
}

func laneCap() int {
	if n := cfg().Zen.Lanes; n > 0 {
		return n
	}
	return 25
}

func laneTTL() time.Duration {
	if m := cfg().Zen.LaneTTLMinutes; m > 0 {
		return time.Duration(m) * time.Minute
	}
	return 30 * time.Minute
}

func laneCooldownBase() time.Duration {
	if s := cfg().Zen.LaneCooldownSecs; s > 0 {
		return time.Duration(s) * time.Second
	}
	return time.Minute
}

// sessionIdleTTL is the idle time after which a lane's session hash/id is
// evicted on the sweep. lane_ttl_minutes stays the hard cap.
func sessionIdleTTL() time.Duration {
	if m := cfg().Zen.SessionIdleMinutes; m > 0 {
		return time.Duration(m) * time.Minute
	}
	return 0
}

// convoIdleTTL is the shorter idle TTL for convo-hash lanes only: one-shot
// requests must not pin a lane for minutes. hdr: and shared lanes keep
// the longer TTLs above.
func convoIdleTTL() time.Duration {
	if m := cfg().Zen.ConvoIdleMinutes; m > 0 {
		return time.Duration(m) * time.Minute
	}
	return 0
}

func laneMinGap() time.Duration {
	return time.Duration(cfg().Zen.LaneMinGapMs) * time.Millisecond
}

func laneRelease() time.Duration {
	return time.Duration(cfg().Zen.LaneReleaseSecs) * time.Second
}

func probeTimeout() time.Duration {
	if ms := cfg().Zen.ProbeTimeoutMs; ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	return time.Second
}

func raceTimeout() time.Duration {
	if ms := cfg().Zen.RaceTimeoutMs; ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	return 3 * time.Second
}

// zenMaxRetries caps zen upstream attempts per request (loop bound is
// attempts, log labels stay /N on retries).
func zenMaxRetries() int {
	if n := cfg().Zen.MaxRetries; n > 0 {
		return n + 1
	}
	if cfg().Zen.MaxRetries < 0 {
		return 1
	}
	return 5 // default: first try + 4 retries
}

func zenRetryLabel() int { return zenMaxRetries() - 1 }

// zenLastRetry reports whether retry index is the final attempt.
func zenLastRetry(retry int) bool { return retry+1 >= zenMaxRetries() }

// laneGate blocks until the lane may send: pacing gap since its previous
// send, plus any residual cooldown. the slot is claimed under the lane lock
// before sleeping, so concurrent requests are spaced one gap apart instead
// of all waking together and bursting the same exit. wait is capped so a
// lane never stalls a request indefinitely.
func laneGate(l *zenLane) time.Duration {
	now := time.Now()
	maxWait := 10 * time.Second
	if s := cfg().Zen.LaneGateMaxSecs; s > 0 {
		maxWait = time.Duration(s) * time.Second
	}
	lanesMu.Lock()
	var wait time.Duration
	if gap := laneMinGap(); gap > 0 && !l.lastSend.IsZero() {
		if w := gap - now.Sub(l.lastSend); w > 0 {
			wait = w
		}
	}
	if cd := l.cooldown.Sub(now); cd > wait {
		wait = cd
	}
	if wait > maxWait {
		wait = maxWait
	}
	// claim: the next caller's gap is measured from this send's schedule,
	// so two concurrent callers serialize one gap apart.
	l.lastSend = now.Add(wait)
	lanesMu.Unlock()
	if wait > 0 {
		time.Sleep(wait)
	}
	return wait
}

// convoHash fingerprints a conversation from the first user text block and
// the last assistant text block: cheap (two blocks, not full history) and
// stable across turns as history grows.
func convoHash(body []byte) string {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	var firstUser, lastAsst string
	feed := func(role, text string) {
		if text == "" {
			return
		}
		if role == "user" && firstUser == "" {
			firstUser = text
		}
		if role == "assistant" {
			lastAsst = text
		}
	}
	if msgs, ok := m["messages"].([]any); ok {
		for _, raw := range msgs {
			mm, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			role, _ := mm["role"].(string)
			feed(role, contentText(mm["content"]))
		}
	}
	if in, ok := m["input"].([]any); ok {
		for _, raw := range in {
			it, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := it["type"].(string)
			if typ == "function_call" || typ == "function_call_output" {
				continue
			}
			role, _ := it["role"].(string)
			feed(role, contentText(it["content"]))
		}
	}
	if firstUser == "" && lastAsst == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(firstUser + "\x00" + lastAsst))
	return "convo:" + hex.EncodeToString(sum[:8])
}

func contentText(c any) string {
	switch t := c.(type) {
	case string:
		return t
	case []any:
		var b strings.Builder
		for _, p := range t {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			pt, _ := pm["type"].(string)
			if pt == "text" || pt == "input_text" || pt == "output_text" {
				if s, ok := pm["text"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	}
	return ""
}

// laneFor assigns a lane: explicit header wins, then conversation hash,
// then the least-recently-used healthy lane (shared pool fallback).
// convo-hash lanes pin only from the second sighting: single-turn
// one-shots (title-gen) fall to the shared pool and never pin a lane.
func laneFor(headerSession string, body []byte) *zenLane {
	lanesMu.Lock()
	defer lanesMu.Unlock()
	key := ""
	if headerSession != "" {
		key = "hdr:" + headerSession
	} else if h := convoHash(body); h != "" {
		if seenConvoLocked(h) {
			key = h
		} else {
			noteConvoLocked(h)
		}
	}
	if key != "" {
		if l, ok := lanes[key]; ok {
			return l
		}
		if len(lanes) < laneCap() {
			l := &zenLane{id: shortLaneID(key), session: zenSession(), lastUsed: time.Now()}
			lanes[key] = l
			return l
		}
		// table full: fall through to shared pool rather than evict a
		// pinned conversation mid-flight.
	}
	return sharedLaneLocked()
}

func shortLaneID(key string) string {
	if len(key) <= 12 {
		return key
	}
	return key[:8] + "..." + key[len(key)-4:]
}

// sharedLaneLocked picks the lru non-cooling lane, creating shared-n slots
// up to cap. caller holds lanesMu.
func sharedLaneLocked() *zenLane {
	var cands []*zenLane
	for k, l := range lanes {
		if strings.HasPrefix(k, "shared:") {
			cands = append(cands, l)
		}
	}
	now := time.Now()
	sort.Slice(cands, func(i, j int) bool { return cands[i].lastUsed.Before(cands[j].lastUsed) })
	for _, l := range cands {
		if now.After(l.cooldown) {
			return l
		}
	}
	if len(lanes) < laneCap() {
		l := &zenLane{id: "shared", session: zenSession(), lastUsed: now}
		k := "shared:" + l.session[len(l.session)-6:]
		l.id = shortLaneID(k)
		lanes[k] = l
		return l
	}
	// all lanes cooling: return the one whose cooldown ends first so the
	// caller waits on the shortest backoff rather than failing outright.
	var best *zenLane
	for _, l := range lanes {
		if best == nil || l.cooldown.Before(best.cooldown) {
			best = l
		}
	}
	if best == nil {
		l := &zenLane{id: "shared", session: zenSession(), lastUsed: now}
		lanes["shared:fallback"] = l
		return l
	}
	return best
}

// laneCool marks a 429 on the lane: exponential backoff, capped at 10m.
// The pinned exit is forgotten so the lane picks a fresh one when its
// cooldown lapses, instead of walking straight back into the same limit.
func laneCool(l *zenLane) {
	lanesMu.Lock()
	l.fails++
	l.rateLimit++
	// cap before shifting: an unbounded fails count overflows the duration
	// after ~31 doublings, producing a negative (past) cooldown and no
	// backoff at all — the exact opposite of what 429 handling needs.
	if l.fails > 8 {
		l.fails = 8
	}
	d := laneCooldownBase() << (l.fails - 1)
	if d > 10*time.Minute {
		d = 10 * time.Minute
	}
	l.cooldown = time.Now().Add(d)
	l.proxy = ""
	lanesMu.Unlock()
	noteRateLimit()
}

// laneCoolSoft records an exit-specific failure (geo-block) without the
// full 429 backoff: the lane fails over once, but stays usable.
func laneCoolSoft(l *zenLane) {
	lanesMu.Lock()
	defer lanesMu.Unlock()
	l.rateLimit++
}

// laneBlock marks an identity-level block (free-tier / user-blocked) on
// the lane. those are per-exit, not a request-rate signal, so the lane only
// forgets the exit and fails over: no long cooldown, since the next exit
// usually works.
func laneBlock(l *zenLane) {
	lanesMu.Lock()
	defer lanesMu.Unlock()
	l.proxy = ""
	l.cooldown = time.Now().Add(3 * time.Second)
}

// noteRateLimit counts lanes rate-limited since the last pool refresh and
// fires one once the threshold is crossed: when several distinct exits are
// limited at once, the whole pool is stale, not one proxy.
func noteRateLimit() {
	n := cfg().Zen.PoolRefresh429s
	if n <= 0 {
		return
	}
	poolRefresh429Mu.Lock()
	poolRefresh429s++
	hit := poolRefresh429s >= n
	if hit {
		poolRefresh429s = 0
	}
	poolRefresh429Mu.Unlock()
	if hit {
		acclog.Printf("  zen: %d rate-limited lanes, refreshing proxy pool", n)
		refreshZenProxiesAsync()
	}
}

func resetPoolRefresh429s() {
	poolRefresh429Mu.Lock()
	poolRefresh429s = 0
	poolRefresh429Mu.Unlock()
}

var (
	poolRefresh429Mu sync.Mutex
	poolRefresh429s  int
)

// laneHealthy reports whether the lane may take a request right now.
func laneHealthy(l *zenLane) bool {
	lanesMu.Lock()
	defer lanesMu.Unlock()
	return time.Now().After(l.cooldown)
}

// laneTouch records a successful use: clears backoff, stamps last-used,
// pins the working proxy.
func laneTouch(l *zenLane, proxy string) {
	lanesMu.Lock()
	defer lanesMu.Unlock()
	l.lastUsed = time.Now()
	l.fails = 0
	l.cooldown = time.Time{}
	l.requests++
	if proxy != "" {
		l.proxy = proxy
		l.country = proxyCountry(proxy)
	}
}

// pickLaneProxy returns a verified proxy not already pinned by another
// lane and not burned for model, so lanes actually spread across distinct
// exits. custom proxies win; otherwise samples the latency-weighted pool
// and excludes taken exits. "" only when every pool exit is taken or the
// pool is empty.
func pickLaneProxy() string { return pickLaneProxyFor("") }

// pickLaneProxyFor is pickLaneProxy skipping exits burned for model.
func pickLaneProxyFor(model string) string {
	taken := map[string]bool{}
	lanesMu.Lock()
	for _, l := range lanes {
		if l.proxy != "" {
			taken[l.proxy] = true
		}
	}
	lanesMu.Unlock()

	if cps := getCustomProxies(); len(cps) > 0 {
		var free []string
		for _, c := range cps {
			if !taken[c] && !exitBurned(c, model) {
				free = append(free, c)
			}
		}
		if len(free) == 0 {
			return ""
		}
		if w := raceProbe(free, raceTimeout()); w != "" {
			return w
		}
		return free[rand.Intn(len(free))]
	}

	zenProxiesMu.RLock()
	pool := append([]string(nil), zenProxies...)
	zenProxiesMu.RUnlock()
	if len(pool) == 0 {
		return ""
	}
	// untaken, unburned exits, fastest+most-reliable first. weighting
	// (not strict quartiles) keeps the whole pool in play while fast
	// exits win most draws.
	var cands []string
	for _, p := range pool {
		if taken[p] || exitBurned(p, model) {
			continue
		}
		cands = append(cands, p)
	}
	if len(cands) == 0 {
		return ""
	}
	sort.Slice(cands, func(i, j int) bool { return proxyWeight(cands[i]) > proxyWeight(cands[j]) })
	if w := raceProbe(sampleWeighted(cands, 3), raceTimeout()); w != "" {
		return w
	}
	return sampleWeighted(cands, 1)[0]
}

// laneReproxy swaps a dead exit for a fresh verified one that no other lane
// holds and that is not burned for model, keeping the lane session pinned
// (affinity survives proxy churn).
func laneReproxy(l *zenLane, model string) string {
	p := pickLaneProxyFor(model)
	lanesMu.Lock()
	defer lanesMu.Unlock()
	l.proxy = p
	l.country = proxyCountry(p)
	return p
}

// laneDropProxy tells lanes holding a burned proxy to forget it; they pick
// a fresh exit on next use instead of reusing a dead one.
func laneDropProxy(proxyURL string) {
	if proxyURL == "" {
		return
	}
	lanesMu.Lock()
	defer lanesMu.Unlock()
	for _, l := range lanes {
		if l.proxy == proxyURL {
			l.proxy = ""
		}
	}
}

// laneEscalate pins a fresh proxy to a lane that just got rate-limited or
// free-tier-blocked going direct. without it every lane shares one burned
// home ip and failover is a no-op: the retry loop re-sends direct and gets
// the same 403. after escalation laneProxyFor returns this exit.
func laneEscalate(l *zenLane, model string) string {
	p := pickLaneProxyFor(model)
	if p == "" {
		return ""
	}
	lanesMu.Lock()
	l.proxy = p
	l.country = proxyCountry(p)
	l.escalated = true
	lanesMu.Unlock()
	acclog.Printf("  opencode lane %s escalating direct -> %s (%s)", l.id, p, proxyCountry(p))
	return p
}

// laneWait returns how long until the soonest lane leaves cooldown,
// clamped so one stuck lane can't stall a request.
func laneWait() time.Duration {
	lanesMu.Lock()
	defer lanesMu.Unlock()
	now := time.Now()
	var soonest time.Time
	for _, l := range lanes {
		if l.cooldown.After(now) && (soonest.IsZero() || l.cooldown.Before(soonest)) {
			soonest = l.cooldown
		}
	}
	if soonest.IsZero() {
		return 0
	}
	d := soonest.Sub(now)
	maxWait := 3 * time.Second
	if s := cfg().Zen.LaneWaitMaxSecs; s > 0 {
		maxWait = time.Duration(s) * time.Second
	}
	if d > maxWait {
		return maxWait
	}
	return d
}

// lastZenSuccess tracks the last working zen response, so the retry loops
// can tell "all exits limited for a while" (stale pool/sessions) from a
// momentary burst.
var (
	zenSuccessMu sync.Mutex
	zenSuccessAt time.Time
)

func noteZenSuccessAt() {
	zenSuccessMu.Lock()
	zenSuccessAt = time.Now()
	zenSuccessMu.Unlock()
}

func zenStaleSince() time.Duration {
	zenSuccessMu.Lock()
	defer zenSuccessMu.Unlock()
	if zenSuccessAt.IsZero() {
		return 0
	}
	return time.Since(zenSuccessAt)
}

// maybeRefreshStalePool fires a pool refresh + session rotation when every
// lane has been cooling with no success for 15s: the pool/sessions are
// stale, not just busy. returns true when it fired.
func maybeRefreshStalePool() bool {
	stale := 15 * time.Second
	if s := cfg().Zen.StalePoolSecs; s > 0 {
		stale = time.Duration(s) * time.Second
	}
	if zenStaleSince() < stale {
		return false
	}
	lanesMu.Lock()
	now := time.Now()
	allCooling := len(lanes) > 0
	for _, l := range lanes {
		if now.After(l.cooldown) {
			allCooling = false
			break
		}
	}
	lanesMu.Unlock()
	if !allCooling {
		return false
	}
	acclog.Printf("  zen: all lanes limited 15s+ with no success, refreshing pool + sessions")
	refreshZenProxiesAsync()
	resetStaleLanes()
	return true
}

// resetStaleLanes clears cooldowns and mints fresh sessions after a stale
// refresh, so retry loops re-enter on new sessions instead of waiting out
// dead backoffs.
func resetStaleLanes() {
	lanesMu.Lock()
	defer lanesMu.Unlock()
	for _, l := range lanes {
		l.cooldown = time.Time{}
		l.fails = 0
		l.proxy = ""
		l.session = zenSession()
	}
	zenSuccessMu.Lock()
	zenSuccessAt = time.Now()
	zenSuccessMu.Unlock()
}

// sweepLanes runs on the pool refresh tick: drops lanes idle past ttl and
// releases exits idle past lane_release_secs, so a lane that has been cold
// resumes on a fresh proxy instead of a stale one. lanes idle past
// session_idle_minutes are evicted too, clearing the session hash/id from
// cache while lane_ttl_minutes stays the hard cap.
func sweepLanes() {
	cutoff := time.Now().Add(-laneTTL())
	rel := laneRelease()
	idle := sessionIdleTTL()
	var idleCutoff time.Time
	if idle > 0 {
		idleCutoff = time.Now().Add(-idle)
	}
	convo := convoIdleTTL()
	var convoCutoff time.Time
	if convo > 0 {
		convoCutoff = time.Now().Add(-convo)
	}
	lanesMu.Lock()
	for k, l := range lanes {
		if l.lastUsed.Before(cutoff) {
			delete(lanes, k)
			dropConvoSeen(k)
			continue
		}
		if !convoCutoff.IsZero() && strings.HasPrefix(k, "convo:") && l.lastUsed.Before(convoCutoff) {
			delete(lanes, k)
			dropConvoSeen(k)
			continue
		}
		if !idleCutoff.IsZero() && l.lastUsed.Before(idleCutoff) {
			delete(lanes, k)
			dropConvoSeen(k)
			continue
		}
		if rel > 0 && l.proxy != "" && time.Since(l.lastUsed) > rel {
			l.proxy = ""
		}
	}
	lanesMu.Unlock()
	resetPoolRefresh429s()
	sweepBurns()
	// scan-cache entries idle past the same ttl go too; vault entries
	// keep their own (longer) ttl and are untouched.
	if idle > 0 {
		if n := pii.SweepScanIdle(idle); n > 0 {
			acclog.Printf("  pii scan cache: %d idle entr(ies) evicted", n)
		}
	}
}

// lanesRevalidate clears lane exits that are no longer in the freshly
// verified pool, so a refresh actually re-points the lanes instead of
// leaving them pinned to proxies that just died.
func lanesRevalidate(verified []string) {
	if len(verified) == 0 {
		return
	}
	inPool := make(map[string]bool, len(verified))
	for _, v := range verified {
		inPool[v] = true
	}
	lanesMu.Lock()
	stale := 0
	for _, l := range lanes {
		if l.proxy != "" && !inPool[l.proxy] {
			l.proxy = ""
			stale++
		}
	}
	lanesMu.Unlock()
	if stale > 0 {
		acclog.Printf("  zen lanes: %d stale exit(s) cleared by pool refresh", stale)
	}
}

// redactProxyUserinfo strips user:pass from a proxy url so a credentialed
// proxy never reaches /status, which is open even when auth.tokens is set.
// built by hand: url.String() would percent-escape the mask.
func redactProxyUserinfo(p string) string {
	if p == "" {
		return ""
	}
	i := strings.Index(p, "://")
	if i < 0 {
		return p
	}
	rest := p[i+3:]
	slash := strings.Index(rest, "/")
	at := strings.Index(rest, "@")
	// an @ before the first slash means the authority carries credentials
	// (user or user:pass) — redact the whole userinfo segment.
	if at < 0 || (slash >= 0 && slash < at) {
		return p
	}
	return p[:i+3] + "***@" + rest[at+1:]
}

func laneSnapshot() []laneStatus {
	lanesMu.Lock()
	defer lanesMu.Unlock()
	now := time.Now()
	out := make([]laneStatus, 0, len(lanes))
	for _, l := range lanes {
		st := laneStatus{
			ID: l.id, Proxy: redactProxyUserinfo(l.proxy), Country: l.country,
			LastUsed: now.Sub(l.lastUsed).Round(time.Second).String(),
			Requests: l.requests, RateLimit: l.rateLimit,
		}
		if now.Before(l.cooldown) {
			st.Cooldown = l.cooldown.Sub(now).Round(time.Second).String()
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// laneSession returns the lane's stable session id under lock.
func laneSession(l *zenLane) string {
	lanesMu.Lock()
	defer lanesMu.Unlock()
	return l.session
}

// laneFailover returns the healthiest lane other than l, or nil when every
// lane (including l) is cooling — the caller then waits out the backoff.
func laneFailover(l *zenLane) *zenLane {
	lanesMu.Lock()
	defer lanesMu.Unlock()
	now := time.Now()
	var best *zenLane
	for _, c := range lanes {
		if c == l || now.Before(c.cooldown) {
			continue
		}
		if best == nil || c.lastUsed.Before(best.lastUsed) {
			best = c
		}
	}
	if best != nil {
		return best
	}
	if len(lanes) < laneCap() {
		nl := &zenLane{id: "shared", session: zenSession(), lastUsed: now}
		k := "shared:" + nl.session[len(nl.session)-6:]
		nl.id = shortLaneID(k)
		lanes[k] = nl
		return nl
	}
	return nil
}

// laneProxyFor returns the lane's pinned proxy, verifying and (re)picking
// when empty or dead. "" means direct, which is correct on a cold lane when
// always_proxy is off; once a lane has been escalated (direct got limited)
// the pinned exit is returned regardless of that setting. repicks skip
// exits burned for model.
func laneProxyFor(l *zenLane, model string) string {
	lanesMu.Lock()
	p := l.proxy
	escalated := l.escalated
	lanesMu.Unlock()
	if p != "" {
		if !exitBurned(p, model) && probeProxyFull(p, probeTimeout()) {
			return p
		}
		if exitBurned(p, model) {
			lanesMu.Lock()
			if l.proxy == p {
				l.proxy = ""
			}
			lanesMu.Unlock()
		} else {
			dropProxy(p)
			laneDropProxy(p)
		}
	}
	if !cfg().Zen.AlwaysProxy && !escalated {
		return ""
	}
	return laneReproxy(l, model)
}
