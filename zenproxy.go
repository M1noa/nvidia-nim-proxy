package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const proxyListURL = "https://proxies.minoa.cat/list?format=json&sort=response&limit=0&response_max=400"

// zenBlockedCountries are api-reported countries whose exits zen geo-blocks
// for muse-spark (RegionError "not available in your country"). from the
// probe_zen_proxies.py sweep: PK 4/4 blocked, RU/VE/HK blocked with 0 ok,
// BY/TJ/IQ/MM blocked with 0 ok.
var (
	zenBlockedMu        sync.RWMutex
	zenBlockedCountries = map[string]bool{
		"PK": true, "RU": true, "VE": true, "HK": true,
		"BY": true, "TJ": true, "IQ": true, "MM": true,
	}
)

func zenBlocked(cc string) bool {
	zenBlockedMu.RLock()
	defer zenBlockedMu.RUnlock()
	return zenBlockedCountries[cc]
}

var (
	zenProxiesMu sync.RWMutex
	zenProxies   []string
	zenNetErrMu  sync.Mutex
	zenNetErrs   int
	zenGeoMu     sync.Mutex
	zenGeoErrs   int
	zenBlockMu   sync.Mutex
	zenBlockErrs int
	// api-reported proxy -> country, rebuilt on each refresh.
	zenProxyCountryMu sync.RWMutex
	zenProxyCountry   = map[string]string{}
	// api-reported proxy -> response ms, rebuilt on each refresh.
	zenLatencyMu sync.RWMutex
	zenLatency   = map[string]int{}
	// geo/user blocks per country, for hardcoding bad regions.
	zenCountryBlockMu sync.Mutex
	zenCountryBlocks  = map[string]int{}
)

// zenVersion is the opencode release tag reported in the User-Agent. Defaults
// to 1.18.31 and is refreshed from GitHub on startup and every 6h after.
// guarded by a mutex: the refresher goroutine writes it while request
// goroutines read it for every zen request.
var (
	zenVersionMu sync.RWMutex
	zenVersion   = "1.18.31"
)

func zenVersionStr() string {
	zenVersionMu.RLock()
	defer zenVersionMu.RUnlock()
	return zenVersion
}

// fetchOpenCodeVersion pulls the latest opencode release tag from the GitHub
// API, falling back to the default on any error.
func fetchOpenCodeVersion() {
	cl := &http.Client{Timeout: 5 * time.Second}
	resp, err := cl.Get("https://api.github.com/repos/sst/opencode/releases/latest")
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var r struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return
	}
	v := strings.TrimPrefix(strings.TrimSpace(r.TagName), "v")
	if v != "" {
		zenVersionMu.Lock()
		changed := v != zenVersion
		zenVersion = v
		zenVersionMu.Unlock()
		if changed {
			log.Printf("  opencode version: %s", v)
		}
	}
}

// watchOpenCodeVersion refreshes the User-Agent version every 6h.
func watchOpenCodeVersion() {
	t := time.NewTicker(6 * time.Hour)
	defer t.Stop()
	for range t.C {
		fetchOpenCodeVersion()
	}
}

type proxyEntry struct {
	IP             string   `json:"ip"`
	Port           int      `json:"port"`
	Country        string   `json:"country"`
	HTTPS          bool     `json:"https"`
	Protocols      []string `json:"protocols"`
	Anonymity      string   `json:"anonymity"`
	Reliability    float64  `json:"reliability"`
	Quality        float64  `json:"quality"`
	ResponseTimeMs int      `json:"response_time_ms"`
}

func schemeFor(protocols []string) string {
	has := map[string]bool{}
	for _, p := range protocols {
		has[p] = true
	}
	if has["socks4"] {
		return "socks4"
	}
	if has["socks5"] {
		return "socks5"
	}
	if has["http"] {
		return "http"
	}
	return ""
}

func refreshZenProxies() {
	cl := &http.Client{Timeout: 15 * time.Second}
	resp, err := cl.Get(proxyListURL)
	if err != nil {
		log.Printf("  zen proxies: fetch failed: %v", err)
		return
	}
	defer resp.Body.Close()
	var list []proxyEntry
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		log.Printf("  zen proxies: decode failed: %v", err)
		return
	}
	c := cfg() // caps live in config.yml zen section
	var fast []proxyEntry
	for _, e := range list {
		if e.Port == 0 || e.IP == "" {
			continue
		}
		if schemeFor(e.Protocols) == "" {
			continue
		}
		if c.Zen.MaxResponseMs > 0 && e.ResponseTimeMs > c.Zen.MaxResponseMs {
			continue
		}
		if zenBlocked(strings.ToUpper(strings.TrimSpace(e.Country))) {
			continue
		}
		if !e.HTTPS && schemeFor(e.Protocols) == "http" {
			continue // https-only zen, no-tls http proxies can't connect
		}
		fast = append(fast, e)
	}
	sort.Slice(fast, func(i, j int) bool { return fast[i].ResponseTimeMs < fast[j].ResponseTimeMs })
	if n := c.Zen.PoolSize; n > 0 && len(fast) > n {
		fast = fast[:n]
	}
	cands := make([]string, 0, len(fast))
	countries := make(map[string]string, len(fast))
	lats := make(map[string]int, len(fast))
	for _, e := range fast {
		u := schemeFor(e.Protocols) + "://" + e.IP + ":" + strconv.Itoa(e.Port)
		cands = append(cands, u)
		countries[u] = strings.ToUpper(strings.TrimSpace(e.Country))
		lats[u] = e.ResponseTimeMs
	}
	setProxyCountries(countries)
	setProxyLatency(lats)

	var wg sync.WaitGroup
	var mu sync.Mutex
	verified := make([]string, 0, len(cands))
	sem := make(chan struct{}, 40)
	for _, u := range cands {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if verifyProxy(u) {
				mu.Lock()
				verified = append(verified, u)
				mu.Unlock()
			}
		}(u)
	}
	wg.Wait()

	zenProxiesMu.Lock()
	zenProxies = verified
	zenProxiesMu.Unlock()
	// a fresh pool means every lane's pinned exit is suspect: revalidate
	// against the new verified set so lanes stop burning stale exits.
	lanesRevalidate(verified)
	log.Printf("  zen proxies: %d verified of %d candidates", len(verified), len(cands))
}

// verifyProxy confirms a proxy can reach the zen API via a lightweight GET.
func verifyProxy(proxyURL string) bool {
	cl := zenClient(proxyURL)
	cl.Timeout = 15 * time.Second
	req, err := http.NewRequest("GET", "https://opencode.ai/zen/v1/models", nil)
	if err != nil {
		return false
	}
	setZenHeaders(req, zenSession())
	resp, err := cl.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if err != nil {
		return false
	}
	return strings.Contains(string(body), `"object":"list"`)
}

// setProxyLatency replaces the proxy->response-ms map after a refresh.
func setProxyLatency(m map[string]int) {
	zenLatencyMu.Lock()
	defer zenLatencyMu.Unlock()
	zenLatency = m
}

// proxyLatency returns api-reported ms, big default when unknown.
func proxyLatency(proxyURL string) int {
	zenLatencyMu.RLock()
	defer zenLatencyMu.RUnlock()
	if ms := zenLatency[proxyURL]; ms > 0 {
		return ms
	}
	return 10000
}

// probeProxyFull verifies the full path to zen's front door through the
// proxy with a short timeout: socks dials opencode.ai:443 via the proxy,
// http issues a CONNECT for it. beats a tcp ping: dead egress fails here,
// not 30s into the real request.
func probeProxyFull(proxyURL string, timeout time.Duration) bool {
	if proxyURL == "" {
		return true
	}
	pu, err := url.Parse(proxyURL)
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	switch pu.Scheme {
	case "socks4":
		c, err := socks4Dial(ctx, "tcp", "opencode.ai:443", pu.Host)
		if err != nil {
			return false
		}
		c.Close()
		return true
	case "socks5":
		c, err := socks5Dial(ctx, "tcp", "opencode.ai:443", pu.Host)
		if err != nil {
			return false
		}
		c.Close()
		return true
	default:
		return probeHTTPConnect(ctx, pu, "opencode.ai:443")
	}
}

// probeHTTPConnect dials the proxy and issues CONNECT target, true on 200.
func probeHTTPConnect(ctx context.Context, pu *url.URL, target string) bool {
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", pu.Host)
	if err != nil {
		return false
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
		defer c.SetDeadline(time.Time{})
	}
	var sb strings.Builder
	sb.WriteString("CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n")
	if pu.User != nil {
		pw, _ := pu.User.Password()
		cred := base64.StdEncoding.EncodeToString([]byte(pu.User.Username() + ":" + pw))
		sb.WriteString("Proxy-Authorization: Basic " + cred + "\r\n")
	}
	sb.WriteString("\r\n")
	if _, err := io.WriteString(c, sb.String()); err != nil {
		return false
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return false
	}
	return strings.Contains(line, " 200")
}

// raceProbe probes proxies in parallel, first full-path winner wins.
// "" when none passes within timeout.
func raceProbe(proxies []string, timeout time.Duration) string {
	win := make(chan string, 1)
	var wg sync.WaitGroup
	for _, u := range proxies {
		if u == "" {
			continue
		}
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			if probeProxyFull(u, timeout) {
				select {
				case win <- u:
				default:
				}
			}
		}(u)
	}
	go func() {
		wg.Wait()
		close(win)
	}()
	t := time.NewTimer(timeout + 500*time.Millisecond)
	defer t.Stop()
	select {
	case w := <-win:
		return w
	case <-t.C:
		return ""
	}
}

// sampleProxies returns up to n distinct pool proxies, weighted toward low
// api-reported latency: the fastest quartile wins ~3/4 of draws.
func sampleProxies(n int) []string {
	zenProxiesMu.RLock()
	pool := append([]string(nil), zenProxies...)
	zenProxiesMu.RUnlock()
	if len(pool) == 0 {
		return nil
	}
	sort.Slice(pool, func(i, j int) bool { return proxyLatency(pool[i]) < proxyLatency(pool[j]) })
	q := len(pool) / 4
	if q < 1 {
		q = 1
	}
	var out []string
	seen := map[string]bool{}
	for len(out) < n && len(seen) < len(pool) {
		u := pool[rand.Intn(len(pool))]
		if rand.Intn(4) < 3 {
			u = pool[rand.Intn(q)]
		}
		if !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	return out
}

// dropProxy removes a dead proxy from the pool so it is not picked again.
func dropProxy(proxyURL string) {
	if proxyURL == "" {
		return
	}
	zenProxiesMu.Lock()
	found := false
	for i, p := range zenProxies {
		if p == proxyURL {
			zenProxies = append(zenProxies[:i], zenProxies[i+1:]...)
			found = true
			log.Printf("  zen proxies: dropped %s (left %d)", proxyURL, len(zenProxies))
			break
		}
	}
	zenProxiesMu.Unlock()
	if found {
		laneDropProxy(proxyURL)
		closeZenTransport(proxyURL)
	}
}

// noteZenNetworkError counts consecutive network failures and returns
// true once the threshold is crossed, signaling a stale/dead pool.
func noteZenNetworkError() bool {
	zenNetErrMu.Lock()
	defer zenNetErrMu.Unlock()
	zenNetErrs++
	return zenNetErrs >= 3
}

func resetZenNetworkErrors() {
	zenNetErrMu.Lock()
	defer zenNetErrMu.Unlock()
	zenNetErrs = 0
}

// zenBackoff caps retry sleeps at 300ms: 0, 150ms, 300ms, 300ms.
// old linear 1-4s sleeps added up to 10s per request on a dead pool.
func zenBackoff(retry int) {
	d := time.Duration(retry) * 150 * time.Millisecond
	if d > 300*time.Millisecond {
		d = 300 * time.Millisecond
	}
	if d > 0 {
		time.Sleep(d)
	}
}

// zenGeoBlocked reports whether an upstream error body means the model is
// geo-restricted for the exit IP. Switching proxies fixes the request.
func zenGeoBlocked(body []byte) bool {
	return strings.Contains(strings.ToLower(string(body)), "not available in your country")
}

// zenServiceOverloaded reports whether zen returned a transient overload
// error. Those are retried on the same proxy and session, not rotated.
func zenServiceOverloaded(body []byte) bool {
	return strings.Contains(string(body), "[service_overloaded]")
}

// noteZenGeoErr counts consecutive geo-blocks and returns true once the
// threshold is crossed, signaling most of the pool sits in a blocked region.
func noteZenGeoErr() bool {
	zenGeoMu.Lock()
	defer zenGeoMu.Unlock()
	zenGeoErrs++
	return zenGeoErrs >= 3
}

func resetZenGeoErrs() {
	zenGeoMu.Lock()
	defer zenGeoMu.Unlock()
	zenGeoErrs = 0
}

// zenUserBlocked reports whether an upstream error body means the exit IP is
// banned from zen (403 [user_blocked]). Switching to another proxy fixes it.
func zenUserBlocked(body []byte) bool {
	return strings.Contains(string(body), "[user_blocked]")
}

// noteZenBlockErr counts consecutive user-blocks and returns true once the
// threshold is crossed, signaling most of the pool is banned.
func noteZenBlockErr() bool {
	zenBlockMu.Lock()
	defer zenBlockMu.Unlock()
	zenBlockErrs++
	return zenBlockErrs >= 3
}

func resetZenBlockErrs() {
	zenBlockMu.Lock()
	defer zenBlockMu.Unlock()
	zenBlockErrs = 0
}

var (
	zenFreeTierMu   sync.Mutex
	zenFreeTierErrs int
	zenRefreshMu    sync.Mutex
)

// zenFreeTierBlocked reports whether an upstream error body refuses the exit
// IP the anonymous free tier (403 FreeTierError). the flag is per-IP: the
// same request succeeds from another exit, so rotate proxies instead of
// failing the client request.
func zenFreeTierBlocked(body []byte) bool {
	s := string(body)
	return strings.Contains(s, "FreeTierError") ||
		strings.Contains(strings.ToLower(s), "free tier can only be used from within")
}

// noteZenFreeTierErr counts consecutive free-tier blocks and returns true
// once the threshold is crossed, signaling most of the pool is flagged.
func noteZenFreeTierErr() bool {
	zenFreeTierMu.Lock()
	defer zenFreeTierMu.Unlock()
	zenFreeTierErrs++
	return zenFreeTierErrs >= 3
}

func resetZenFreeTierErrs() {
	zenFreeTierMu.Lock()
	defer zenFreeTierMu.Unlock()
	zenFreeTierErrs = 0
}

// refreshZenProxiesAsync triggers a pool refresh without blocking the request
// path. concurrent triggers collapse into one in-flight refresh.
func refreshZenProxiesAsync() {
	go func() {
		if !zenRefreshMu.TryLock() {
			return
		}
		defer zenRefreshMu.Unlock()
		refreshZenProxies()
	}()
}

// setProxyCountries replaces the proxy->country map after a refresh.
func setProxyCountries(m map[string]string) {
	zenProxyCountryMu.Lock()
	defer zenProxyCountryMu.Unlock()
	zenProxyCountry = m
}

// proxyCountry returns the api-reported country for a proxy.
func proxyCountry(proxyURL string) string {
	if proxyURL == "" {
		return "direct"
	}
	zenProxyCountryMu.RLock()
	defer zenProxyCountryMu.RUnlock()
	if c := zenProxyCountry[proxyURL]; c != "" {
		return c
	}
	return "??"
}

// noteZenCountryBlock counts a geo/user block for a country, returns total.
func noteZenCountryBlock(country string) int {
	zenCountryBlockMu.Lock()
	defer zenCountryBlockMu.Unlock()
	zenCountryBlocks[country]++
	return zenCountryBlocks[country]
}

// logZenNetErr logs a transport-level zen failure with model and country.
func logZenNetErr(retry int, model, proxy, tag string, err error) {
	acclog.Printf("!! opencode zen error (retry %d/4) model=%s country=%s proxy=%s %s: %v",
		retry, model, proxyCountry(proxy), proxy, tag, err)
}

// logZenBlocked logs a geo/user block with status, snippet, and the running
// per-country block count. returns the count.
func logZenBlocked(kind string, retry, status int, model, proxy, tag string, body []byte) int {
	n := noteZenCountryBlock(proxyCountry(proxy))
	acclog.Printf("  opencode %s-blocked %d (retry %d/4) model=%s country=%s blocks=%d via proxy=%s %s err=%q",
		kind, status, retry, model, proxyCountry(proxy), n, proxy, tag, errSnippet(body, 160))
	return n
}

// errSnippet squashes a body to one line, capped at n chars.
func errSnippet(b []byte, n int) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func watchZenProxies() {
	if zenEnabled() {
		refreshZenProxies()
	}
	for {
		time.Sleep(time.Hour)
		if zenEnabled() {
			refreshZenProxies()
		}
		sweepLanes()
	}
}

// zenTransports caches one transport per proxy. building a fresh transport
// per client leaked its idle connections (and fds) until gc on every request;
// transports are safe for concurrent use, so sharing per-proxy is correct.
var (
	zenTransportMu sync.Mutex
	zenTransports  = map[string]*http.Transport{}
)

// zenTransport returns the cached transport for proxyURL, building it on
// first use. pu must be a valid parse of proxyURL.
func zenTransport(proxyURL string, pu *url.URL) *http.Transport {
	zenTransportMu.Lock()
	defer zenTransportMu.Unlock()
	if tr, ok := zenTransports[proxyURL]; ok {
		return tr
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSHandshakeTimeout = 10 * time.Second
	tr.ResponseHeaderTimeout = 30 * time.Second
	tr.ExpectContinueTimeout = 1 * time.Second
	tr.MaxIdleConnsPerHost = 4
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	switch pu.Scheme {
	case "socks4":
		tr.Proxy = nil
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, cancel := context.WithTimeout(ctx, 12*time.Second)
			defer cancel()
			return socks4Dial(c, network, addr, pu.Host)
		}
	case "socks5":
		tr.Proxy = nil
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, cancel := context.WithTimeout(ctx, 12*time.Second)
			defer cancel()
			return socks5Dial(c, network, addr, pu.Host)
		}
	default:
		tr.Proxy = http.ProxyURL(pu)
		tr.DialContext = dialer.DialContext
	}
	zenTransports[proxyURL] = tr
	return tr
}

// closeZenTransport drops a proxy's cached transport and its idle conns.
func closeZenTransport(proxyURL string) {
	zenTransportMu.Lock()
	if tr, ok := zenTransports[proxyURL]; ok {
		delete(zenTransports, proxyURL)
		tr.CloseIdleConnections()
	}
	zenTransportMu.Unlock()
}

// zenClient builds a client routing via proxyURL, or direct if ""/bad.
func zenClient(proxyURL string) *http.Client {
	if proxyURL == "" {
		return &http.Client{Timeout: 300 * time.Second}
	}
	pu, err := url.Parse(proxyURL)
	if err != nil {
		return &http.Client{Timeout: 300 * time.Second}
	}
	return &http.Client{Timeout: 120 * time.Second, Transport: zenTransport(proxyURL, pu)}
}

// socks4Dial connects to proxyAddr and requests a SOCKS4/SOCKS4a CONNECT to addr.
func socks4Dial(ctx context.Context, network, addr string, proxyAddr string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		return nil, fmt.Errorf("socks4: bad port %q", portStr)
	}

	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
		defer conn.SetDeadline(time.Time{})
	}

	req := []byte{0x04, 0x01, byte(port >> 8), byte(port & 0xff)}
	if ip4 := net.ParseIP(host).To4(); ip4 != nil {
		req = append(req, ip4...)
		req = append(req, 0x00)
	} else {
		req = append(req, 0x00, 0x00, 0x00, 0x01)
		req = append(req, 0x00)
		req = append(req, host...)
		req = append(req, 0x00)
	}
	if _, err := conn.Write(req); err != nil {
		conn.Close()
		return nil, err
	}
	resp := make([]byte, 8)
	if _, err := io.ReadFull(conn, resp); err != nil {
		conn.Close()
		return nil, err
	}
	if resp[0] != 0x00 || resp[1] != 0x5a {
		conn.Close()
		return nil, fmt.Errorf("socks4: connect failed, status=%d", resp[1])
	}
	return conn, nil
}

// socks5Dial connects to proxyAddr and requests a SOCKS5 (no-auth) CONNECT to addr.
func socks5Dial(ctx context.Context, network, addr string, proxyAddr string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		return nil, fmt.Errorf("socks5: bad port %q", portStr)
	}

	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
		defer conn.SetDeadline(time.Time{})
	}

	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		conn.Close()
		return nil, err
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		conn.Close()
		return nil, err
	}
	if resp[0] != 0x05 || resp[1] == 0xff {
		conn.Close()
		return nil, fmt.Errorf("socks5: no acceptable auth method")
	}

	req := []byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}
	req = append(req, host...)
	req = append(req, byte(port>>8), byte(port&0xff))
	if _, err := conn.Write(req); err != nil {
		conn.Close()
		return nil, err
	}
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		conn.Close()
		return nil, err
	}
	if hdr[1] != 0x00 {
		conn.Close()
		return nil, fmt.Errorf("socks5: connect failed, rep=%d", hdr[1])
	}
	var skip int
	switch hdr[3] {
	case 0x01:
		skip = 4 + 2
	case 0x03:
		l := []byte{0}
		if _, err := io.ReadFull(conn, l); err != nil {
			conn.Close()
			return nil, err
		}
		skip = int(l[0]) + 2
	case 0x04:
		skip = 16 + 2
	default:
		conn.Close()
		return nil, fmt.Errorf("socks5: bad atyp %d", hdr[3])
	}
	if _, err := io.ReadFull(conn, make([]byte, skip)); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}
