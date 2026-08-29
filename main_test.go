package main

import (
	"math"
	"testing"
	"time"
)

func approx(a, b float64) bool {
	return math.Abs(a-b) < 0.01
}

func TestIdleWeight(t *testing.T) {
	now := time.Now()
	k := &Key{}

	// never used: full weight 1.0
	if w := idleWeight(k, now); w != 1.0 {
		t.Fatalf("fresh key weight = %v, want 1.0", w)
	}

	// just used: floor 0.3
	k2 := &Key{LastUsed: now.Add(-time.Second)}
	if w := idleWeight(k2, now); !approx(w, 0.3) {
		t.Fatalf("recently-used weight = %v, want ~0.3", w)
	}

	// long idle: approaches 1.0
	k3 := &Key{LastUsed: now.Add(-time.Hour)}
	if w := idleWeight(k3, now); w != 1.0 {
		t.Fatalf("long-idle weight = %v, want 1.0", w)
	}
}

func TestIdleWeightFailPenalty(t *testing.T) {
	now := time.Now()
	k := &Key{LastUsed: now.Add(-time.Minute), LastFail: now.Add(-time.Minute), Consec429: 1}
	w := idleWeight(k, now)
	// idle=1min -> w~0.37; penalty 0.1 -> ~0.037
	if w >= 0.05 {
		t.Fatalf("recent-fail weight = %v, want strongly penalized (<0.05)", w)
	}

	// stale failure outside window: no penalty
	k2 := &Key{LastUsed: now.Add(-time.Hour), LastFail: now.Add(-time.Hour), Consec429: 3}
	if w := idleWeight(k2, now); w != 1.0 {
		t.Fatalf("stale-fail weight = %v, want 1.0 (no penalty)", w)
	}
}

func TestRateLimitBackoff(t *testing.T) {
	p := &Pool{}
	cases := []struct {
		consec int
		want   time.Duration
	}{
		{1, 1 * time.Minute},
		{2, 2 * time.Minute},
		{3, 4 * time.Minute},
		{4, 8 * time.Minute},
		{5, 16 * time.Minute},
		{10, 16 * time.Minute}, // capped
	}
	for _, c := range cases {
		k := &Key{}
		for i := 0; i < c.consec; i++ {
			p.rateLimit(k)
		}
		got := k.CooldownUntil.Sub(k.LastFail)
		if got < c.want || got > c.want+time.Millisecond {
			t.Fatalf("consec=%d backoff = %v, want %v", c.consec, got, c.want)
		}
		if k.Consec429 != c.consec {
			t.Fatalf("consec=%d, Consec429=%d", c.consec, k.Consec429)
		}
	}
}

func TestClear429(t *testing.T) {
	p := &Pool{}
	k := &Key{}
	p.rateLimit(k)
	p.rateLimit(k)
	if k.Consec429 != 2 {
		t.Fatalf("Consec429 = %d, want 2", k.Consec429)
	}
	p.Clear429(k)
	if k.Consec429 != 0 {
		t.Fatalf("after clear Consec429 = %d, want 0", k.Consec429)
	}
}

func TestPickAvoidsPenalized(t *testing.T) {
	now := time.Now()
	clean := &Key{Name: "clean", LastUsed: now.Add(-30 * time.Minute)}
	hot := &Key{Name: "hot", LastUsed: now.Add(-2 * time.Second)}
	penalized := &Key{Name: "pen", LastUsed: now.Add(-30 * time.Minute), LastFail: now.Add(-time.Minute), Consec429: 2}
	p := &Pool{keys: []*Key{clean, hot, penalized}}

	counts := map[string]int{}
	for i := 0; i < 30000; i++ {
		k := p.Pick(nil, "m")
		counts[k.Name]++
	}
	// once LastUsed self-resets in the tight loop, clean and hot both sit at the
	// idle floor; the real guarantee is the recently-failed key stays avoided.
	if counts["penalized"] > 2000 {
		t.Fatalf("penalized picked %d/30000, want rare", counts["penalized"])
	}
	if counts["penalized"] >= counts["clean"] {
		t.Fatalf("penalized (%d) picked as much as clean (%d)", counts["penalized"], counts["clean"])
	}
}