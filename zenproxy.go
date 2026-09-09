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

const proxyListURL = "https://proxies.minoa.cat/list?format=json&sort=response&limit=0&country=us&response_max=400"

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
		fast = append(fast, e)
	}
	sort.Slice(fast, func(i, j int) bool { return fast[i].ResponseTimeMs < fast[j].ResponseTimeMs })
	if len(fast) > 200 {
		fast = fast[:200]
	}
	cands := make([]string, 0, len(fast))
	for _, e := range fast {
		cands = append(cands, schemeFor(e.Protocols)+"://"+e.IP+":"+itoa(e.Port))
	}

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
	cl.Timeout = 8 * time.Second
	resp, err := cl.Get("https://opencode.ai/zen/v1/models")
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