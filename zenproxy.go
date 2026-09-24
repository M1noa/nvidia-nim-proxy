package main

import (
	"context"
	"crypto/tls"
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
var zenBlockedCountries = map[string]bool{
	"PK": true, "RU": true, "VE": true, "HK": true,
	"BY": true, "TJ": true, "IQ": true, "MM": true,
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
	// geo/user blocks per country, for hardcoding bad regions.
	zenCountryBlockMu sync.Mutex
	zenCountryBlocks  = map[string]int{}
)

// zenVersion is the opencode release tag reported in the User-Agent. Defaults
// to 1.18.31 and is refreshed from GitHub on startup and every 6h after.
var zenVersion = "1.18.31"

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
	if v != "" && v != zenVersion {
		zenVersion = v
		log.Printf("  opencode version: %s", v)
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
	var fast []proxyEntry
	for _, e := range list {
		if e.Port == 0 || e.IP == "" {
			continue
		}
		if schemeFor(e.Protocols) == "" {
			continue
		}
		if e.ResponseTimeMs > 400 {
			continue
		}
		if zenBlockedCountries[strings.ToUpper(strings.TrimSpace(e.Country))] {
			continue
		}
		if !e.HTTPS && schemeFor(e.Protocols) == "http" {
			continue // https-only zen, no-tls http proxies can't connect
		}
		fast = append(fast, e)
	}
	sort.Slice(fast, func(i, j int) bool { return fast[i].ResponseTimeMs < fast[j].ResponseTimeMs })
	if len(fast) > 400 {
		fast = fast[:400]
	}
	cands := make([]string, 0, len(fast))
	countries := make(map[string]string, len(fast))
	for _, e := range fast {
		u := schemeFor(e.Protocols) + "://" + e.IP + ":" + itoa(e.Port)
		cands = append(cands, u)
		countries[u] = strings.ToUpper(strings.TrimSpace(e.Country))
	}
	setProxyCountries(countries)

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

// testProxy quickly verifies a proxy is reachable via TCP dial.
func testProxy(proxyURL string) bool {
	if proxyURL == "" {
		return true
	}
	pu, err := url.Parse(proxyURL)
	if err != nil {
		return false
	}
	conn, err := net.DialTimeout("tcp", pu.Host, 3*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// pickFastProxy returns a random TCP-reachable proxy, or "" if none.
func pickFastProxy() string {
	zenProxiesMu.RLock()
	defer zenProxiesMu.RUnlock()
	if len(zenProxies) == 0 {
		return ""
	}
	for i := 0; i < 8 && i < len(zenProxies); i++ {
		idx := rand.Intn(len(zenProxies))
		if testProxy(zenProxies[idx]) {
			return zenProxies[idx]
		}
	}
	return zenProxies[rand.Intn(len(zenProxies))]
}

// dropProxy removes a dead proxy from the pool so it is not picked again.
func dropProxy(proxyURL string) {
	if proxyURL == "" {
		return
	}
	zenProxiesMu.Lock()
	defer zenProxiesMu.Unlock()
	for i, p := range zenProxies {
		if p == proxyURL {
			zenProxies = append(zenProxies[:i], zenProxies[i+1:]...)
			log.Printf("  zen proxies: dropped %s (left %d)", proxyURL, len(zenProxies))
			return
		}
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
	refreshZenProxies()
	for {
		time.Sleep(time.Hour)
		refreshZenProxies()
	}
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
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	switch pu.Scheme {
	case "socks4":
		tr.Proxy = nil
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return socks4Dial(ctx, network, addr, pu.Host)
		}
	case "socks5":
		tr.Proxy = nil
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return socks5Dial(ctx, network, addr, pu.Host)
		}
	default:
		tr.Proxy = http.ProxyURL(pu)
	}
	return &http.Client{Timeout: 300 * time.Second, Transport: tr}
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

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
