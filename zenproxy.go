package main

import (
	"encoding/json"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"time"
)

const proxyListURL = "https://proxies.minoa.cat/list?format=json&sort=response&limit=0&country=us&response_max=120&ip_type=isp&ip_type=education_research&ip_type=government_admin"

var (
	zenProxiesMu sync.RWMutex
	zenProxies   []string
)

type proxyEntry struct {
	IP             string   `json:"ip"`
	Port           int      `json:"port"`
	Protocols      []string `json:"protocols"`
	Anonymity      string   `json:"anonymity"`
	Reliability    float64  `json:"reliability"`
	Quality        float64  `json:"quality"`
	ResponseTimeMs int      `json:"response_time_ms"`
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
		hasHTTP := false
		for _, p := range e.Protocols {
			if p == "http" {
				hasHTTP = true
				break
			}
		}
		if !hasHTTP {
			continue
		}
		if e.Anonymity != "elite" && e.Anonymity != "anonymous" {
			continue
		}
		if e.Reliability < 1.0 || e.Quality < 1.0 {
			continue
		}
		if e.ResponseTimeMs > 120 {
			continue
		}
		fast = append(fast, e)
	}
	sort.Slice(fast, func(i, j int) bool { return fast[i].ResponseTimeMs < fast[j].ResponseTimeMs })
	if len(fast) > 40 {
		fast = fast[:40]
	}
	out := make([]string, 0, len(fast))
	for _, e := range fast {
		out = append(out, "http://"+e.IP+":"+itoa(e.Port))
	}
	zenProxiesMu.Lock()
	zenProxies = out
	zenProxiesMu.Unlock()
	log.Printf("  zen proxies: %d fast proxies loaded", len(out))
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

// pickFastProxy returns the first proxy that passes testProxy, or "" if none.
func pickFastProxy() string {
	zenProxiesMu.RLock()
	defer zenProxiesMu.RUnlock()
	for _, p := range zenProxies {
		if testProxy(p) {
			return p
		}
	}
	return ""
}

// pickFastProxyWeighted returns a random proxy that passes testProxy.
func pickFastProxyWeighted() string {
	zenProxiesMu.RLock()
	defer zenProxiesMu.RUnlock()
	var candidates []string
	for _, p := range zenProxies {
		if testProxy(p) {
			candidates = append(candidates, p)
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	return candidates[rand.Intn(len(candidates))]
}

func watchZenProxies() {
	refreshZenProxies()
	for {
		time.Sleep(5 * time.Minute)
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
	tr.Proxy = http.ProxyURL(pu)
	return &http.Client{Timeout: 300 * time.Second, Transport: tr}
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
