package main

import (
	"net/http"
	"sync"
	"testing"
	"time"
)

// laneGate must serialize concurrent senders one min-gap apart: the second
// caller's slot is claimed before the first finishes sleeping, so two
// goroutines never burst the same exit together.
func TestLaneGateSerializesConcurrentSenders(t *testing.T) {
	c := testConfig()
	c.Zen.LaneMinGapMs = 300
	applyConfig(c)
	defer applyConfig(testConfig())

	resetLanes()
	defer resetLanes()
	l := &zenLane{id: "test", session: zenSession()}

	var wg sync.WaitGroup
	sends := make([]time.Time, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			laneGate(l)
			sends[i] = time.Now()
		}(i)
	}
	wg.Wait()

	gap := sends[1].Sub(sends[0])
	if gap < 0 {
		gap = -gap
	}
	// the second send must be scheduled at least one full gap after the
	// first (small tolerance for sleep wakeup jitter).
	if gap < 250*time.Millisecond {
		t.Fatalf("concurrent laneGate calls fired %v apart; want ~>= %v (pacing TOCTOU)", gap, 300*time.Millisecond)
	}
}

// laneCool must stay bounded: many consecutive 429s may not overflow the
// duration into the negative (which would clear the cooldown entirely).
func TestLaneCoolBackoffNeverNegative(t *testing.T) {
	applyConfig(testConfig())
	defer applyConfig(testConfig())

	resetLanes()
	defer resetLanes()
	l := &zenLane{id: "test", session: zenSession()}
	for i := 0; i < 100; i++ {
		laneCool(l)
		if l.cooldown.Before(time.Now()) {
			t.Fatalf("laneCool iteration %d produced a past cooldown (%v): backoff overflow", i, l.cooldown)
		}
	}
}

// same contract for the nvidia key backoff.
func TestKeyRateLimitBackoffNeverNegative(t *testing.T) {
	p := newPool(map[string]string{"a": "nvapi-x"})
	k := p.keys[0]
	for i := 0; i < 100; i++ {
		p.rateLimit(k)
		if k.CooldownUntil.Before(time.Now()) {
			t.Fatalf("rateLimit iteration %d produced a past cooldown: backoff overflow", i)
		}
	}
	if k.CooldownUntil.Sub(time.Now()) > maxBackoff+time.Minute {
		t.Fatalf("backoff exceeds maxBackoff: %v", k.CooldownUntil.Sub(time.Now()))
	}
}

// zenClient must reuse one transport per proxy instead of leaking a fresh
// transport (and its idle connections) on every call.
func TestZenClientTransportsCachedPerProxy(t *testing.T) {
	a1 := zenClient("http://1.2.3.4:8080")
	a2 := zenClient("http://1.2.3.4:8080")
	if a1.Transport != a2.Transport {
		t.Fatal("zenClient built two transports for the same proxy (fd leak)")
	}
	b := zenClient("http://5.6.7.8:8080")
	if a1.Transport == b.Transport {
		t.Fatal("different proxies must not share a transport")
	}
	direct := zenClient("")
	if direct.Transport != nil {
		t.Fatal("direct client should use the default transport")
	}
	closeZenTransport("http://1.2.3.4:8080")
	a3 := zenClient("http://1.2.3.4:8080")
	if a3.Transport == a1.Transport {
		t.Fatal("closeZenTransport did not drop the cached transport")
	}
}

// forwarded zen requests must not carry client auth headers.
func TestZenHeaderForwardingDropsCredentials(t *testing.T) {
	out := http.Header{}
	hdrs := http.Header{}
	hdrs.Set("X-Api-Key", "sk-secret")
	hdrs.Set("Authorization", "Bearer sk-secret")
	hdrs.Set("Cookie", "session=xyz")
	hdrs.Set("X-Custom", "keepme")
	for k, v := range hdrs {
		if zenHeaderForwarded(k) {
			out[k] = v
		}
	}
	if out.Get("X-Api-Key") != "" || out.Get("Authorization") != "" || out.Get("Cookie") != "" {
		t.Fatalf("credentials forwarded upstream: %v", out)
	}
	if out.Get("X-Custom") != "keepme" {
		t.Fatalf("custom headers must survive forwarding, got %v", out)
	}
}
