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
)

func laneCap() int {
	if n := cfg().Zen.Lanes; n > 0 {
		return n
	}
	return 5
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

func laneMinGap() time.Duration {
	return time.Duration(cfg().Zen.LaneMinGapMs) * time.Millisecond
}

func laneRelease() time.Duration {
	return time.Duration(cfg().Zen.LaneReleaseSecs) * time.Second
}

// laneGate blocks until the lane may send: pacing gap since its previous
// send, plus any residual cooldown. without pacing a single conversation
// can fire several requests at one exit inside a second and trip the
// short-window rate limit. wait is capped so a lane never stalls a
// request indefinitely.
func laneGate(l *zenLane) time.Duration {
	wait := time.Duration(0)
	if gap := laneMinGap(); gap > 0 {
		lanesMu.Lock()
		var since time.Duration
		if !l.lastSend.IsZero() {
			since = time.Since(l.lastSend)
		}
		lanesMu.Unlock()
		if w := gap - since; w > 0 {
			wait = w
		}
	}
	if w := laneCooldownLeft(l); w > wait {
		wait = w
	}
	if wait > 0 {
		if wait > 10*time.Second {
			wait = 10 * time.Second
		}
		time.Sleep(wait)
	}
	lanesMu.Lock()
	l.lastSend = time.Now()
	lanesMu.Unlock()
	return wait
}

// laneCooldownLeft is this lane's own remaining cooldown.
func laneCooldownLeft(l *zenLane) time.Duration {
	lanesMu.Lock()
	defer lanesMu.Unlock()
	if l.cooldown.IsZero() || time.Now().After(l.cooldown) {
		return 0
	}
	return l.cooldown.Sub(time.Now())
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
func laneFor(headerSession string, body []byte) *zenLane {
	lanesMu.Lock()
	defer lanesMu.Unlock()
	key := ""
	if headerSession != "" {
		key = "hdr:" + headerSession
	} else if h := convoHash(body); h != "" {
		key = h
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
	l.cooldown = time.Now().Add(5 * time.Second)
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
// lane, so lanes actually spread across distinct exits. custom proxies win;
// otherwise samples the latency-weighted pool and excludes taken exits.
// "" only when every pool exit is taken or the pool is empty.
func pickLaneProxy() string {
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
			if !taken[c] {
				free = append(free, c)
			}
		}
		if len(free) == 0 {
			return ""
		}
		if w := raceProbe(free, 3*time.Second); w != "" {
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
	sort.Slice(pool, func(i, j int) bool { return proxyLatency(pool[i]) < proxyLatency(pool[j]) })
	q := len(pool) / 4
	if q < 1 {
		q = 1
	}
	// prefer untaken exits from the fastest quartile, then the rest.
	var fast, slow []string
	for _, p := range pool {
		if taken[p] {
			continue
		}
		if proxyLatency(p) <= proxyLatency(pool[q-1]) {
			fast = append(fast, p)
		} else {
			slow = append(slow, p)
		}
	}
	cands := append(fast, slow...)
	if len(cands) == 0 {
		return ""
	}
	if w := raceProbe(sampleFrom(cands, 3), 3*time.Second); w != "" {
		return w
	}
	return cands[rand.Intn(len(cands))]
}

func sampleFrom(ss []string, n int) []string {
	if len(ss) <= n {
		return ss
	}
	out := make([]string, 0, n)
	seen := map[string]bool{}
	for len(out) < n {
		p := ss[rand.Intn(len(ss))]
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// laneReproxy swaps a dead exit for a fresh verified one that no other lane
// holds, keeping the lane session pinned (affinity survives proxy churn).
func laneReproxy(l *zenLane) string {
	p := pickLaneProxy()
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
func laneEscalate(l *zenLane) string {
	p := pickLaneProxy()
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
	if d > 3*time.Second {
		return 3 * time.Second
	}
	return d
}

// sweepLanes runs on the pool refresh tick: drops lanes idle past ttl and
// releases exits idle past lane_release_secs, so a lane that has been cold
// resumes on a fresh proxy instead of a stale one.
func sweepLanes() {
	cutoff := time.Now().Add(-laneTTL())
	rel := laneRelease()
	lanesMu.Lock()
	for k, l := range lanes {
		if l.lastUsed.Before(cutoff) {
			delete(lanes, k)
			continue
		}
		if rel > 0 && l.proxy != "" && time.Since(l.lastUsed) > rel {
			l.proxy = ""
		}
	}
	lanesMu.Unlock()
	resetPoolRefresh429s()
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
	at := strings.Index(rest, "@")
	if at < 0 {
		return p
	}
	// only strip when the part before @ is userinfo, not a host with a port
	head := rest[:at]
	if !strings.Contains(head, ":") && !strings.Contains(head, "/") {
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
// the pinned exit is returned regardless of that setting.
func laneProxyFor(l *zenLane) string {
	lanesMu.Lock()
	p := l.proxy
	escalated := l.escalated
	lanesMu.Unlock()
	if p != "" {
		if probeProxyFull(p, time.Second) {
			return p
		}
		dropProxy(p)
		laneDropProxy(p)
	}
	if !cfg().Zen.AlwaysProxy && !escalated {
		return ""
	}
	return laneReproxy(l)
}
