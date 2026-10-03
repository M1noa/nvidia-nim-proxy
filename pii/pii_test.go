package pii

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func testGuard() *Guard {
	return build(Config{
		Enabled:        true,
		DetectSecrets:  true,
		DetectPII:      true,
		Entities:       []CustomTerm{{Name: "minoa", Variations: []string{"M1noa"}}},
		FuzzyThreshold: 0.95,
	}, "test")
}

func TestDetectStructured(t *testing.T) {
	spans := detectHeuristics("ssn 472-81-0094 card 4111 1111 1111 1111 mail a@b.com ip 10.0.0.1 me minoa")
	labels := map[Label]bool{}
	for _, s := range spans {
		labels[s.Label] = true
	}
	for _, want := range []Label{LSSN, LCreditCard, LEmail, LIPAddress} {
		if !labels[want] {
			t.Errorf("missing %s in %v", want, spans)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	g := testGuard()
	bodies := []string{
		`{"model":"x","messages":[{"role":"user","content":"hi minoa, ssn 472-81-0094"}]}`,
		`{"model":"x","messages":[{"role":"user","content":"café naïve \"quotes\" back\\slash minoa"}]}`,
		`{"model":"x","messages":[{"role":"user","content":"card 4111 1111 1111 1111"}],"tools":[{"type":"function","function":{"name":"pay","arguments":"{\"card\":\"4111 1111 1111 1111\"}"}}]}`,
	}
	for _, b := range bodies {
		masked := string(g.MaskBody([]byte(b)))
		if strings.Contains(masked, "minoa") || strings.Contains(masked, "472-81-0094") || strings.Contains(masked, "4111") {
			t.Errorf("leak in masked: %s", masked)
		}
		back := string(g.RestoreBody([]byte(masked)))
		if back != b {
			t.Errorf("round trip mismatch:\n got %s\nwant %s", back, b)
		}
	}
}

func TestStreamSplitEveryOffset(t *testing.T) {
	g := testGuard()
	body := `{"model":"x","messages":[{"role":"user","content":"call minoa at +1 555-123-4567"}]}`
	masked := string(g.MaskBody([]byte(body)))
	// find a surrogate in masked output
	var surr string
	for _, s := range g.vault.Surrogates() {
		if strings.Contains(masked, s) {
			surr = s
			break
		}
	}
	if surr == "" {
		t.Fatal("no surrogate produced")
	}
	want := string(g.RestoreBody([]byte(masked)))
	for off := 0; off < len(masked); off++ {
		r := NewRevealer(g.vault)
		var out strings.Builder
		for _, chunk := range [][]byte{[]byte(masked[:off]), []byte(masked[off:])} {
			out.Write(r.Push(chunk))
		}
		out.Write(r.Flush())
		if out.String() != want {
			t.Fatalf("offset %d: got %q want %q", off, out.String(), want)
		}
	}
}

func TestSurrogateStableAcrossVaults(t *testing.T) {
	a := NewVault(vaultKey(), 0, 0)
	b := NewVault(vaultKey(), 0, 0)
	if a.For(LGivenName, "Alex") != b.For(LGivenName, "Alex") {
		t.Fatal("same original must map to same surrogate across restarts")
	}
}

func TestCacheSkipsRescan(t *testing.T) {
	g := testGuard()
	text := "history line with minoa and ssn 472-81-0094\n\nsecond paragraph here"
	g.scanner.maskText(text, g.vault)
	miss0, hit0 := g.scanner.Cache.Miss, g.scanner.Cache.Hits
	g.scanner.maskText(text+"\n\nplus new tail", g.vault)
	if g.scanner.Cache.Miss != miss0+1 {
		t.Fatalf("expected 1 new scan, miss %d->%d hit %d->%d", miss0, g.scanner.Cache.Miss, hit0, g.scanner.Cache.Hits)
	}
	if g.scanner.Cache.Hits != hit0+2 {
		t.Fatalf("expected 2 cache hits for unchanged paragraphs, %+v", g.scanner.Cache)
	}
}

func TestCustomMinoaFuzzy(t *testing.T) {
	g := testGuard()
	masked := string(g.MaskBody([]byte(`{"model":"x","messages":[{"role":"user","content":"hey minoa and M1noa"}]}`)))
	if strings.Contains(masked, "minoa") || strings.Contains(masked, "M1noa") {
		t.Errorf("custom terms leaked: %s", masked)
	}
}

func BenchmarkDetect(b *testing.B) {
	text := strings.Repeat("contact alex rivera at alex@example.com ssn 472-81-0094 ip 10.0.0.1 card 4111 1111 1111 1111. ", 20)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		detectHeuristics(text)
	}
}

func TestScanSweepIdle(t *testing.T) {
	g := testGuard()
	g.scanner.maskText("history line with minoa here", g.vault)
	if len(g.scanner.Cache.items) == 0 {
		t.Fatal("want cached entries after scan")
	}
	// fresh entries survive a long ttl.
	if n := g.scanner.Cache.sweepIdle(time.Hour); n != 0 {
		t.Fatalf("fresh entries swept: %d", n)
	}
	// backdate access, then sweep evicts.
	for _, e := range g.scanner.Cache.items {
		e.lastAccess = time.Now().Add(-time.Hour)
	}
	if n := g.scanner.Cache.sweepIdle(time.Minute); n == 0 {
		t.Fatal("stale entries must be swept")
	}
	if len(g.scanner.Cache.items) != 0 {
		t.Fatalf("want empty cache, got %d", len(g.scanner.Cache.items))
	}
	// vault entries survive the scan sweep (different ttls).
	if len(g.vault.Surrogates()) == 0 {
		t.Fatal("vault must keep surrogates after scan sweep")
	}
}

func TestEntityReplacement(t *testing.T) {
	g := build(Config{
		Enabled: true, DetectPII: true,
		Entities: []CustomTerm{{Name: "minoa", Type: "name", Replacement: "REDACTED_X"}},
	}, "test-repl")
	masked := string(g.MaskBody([]byte(`{"model":"x","messages":[{"role":"user","content":"hi minoa"}]}`)))
	if strings.Contains(masked, "minoa") {
		t.Fatalf("entity leaked: %s", masked)
	}
	if !strings.Contains(masked, "REDACTED_X") {
		t.Fatalf("replacement not used: %s", masked)
	}
	back := string(g.RestoreBody([]byte(masked)))
	if !strings.Contains(back, "minoa") {
		t.Fatalf("replacement not restored: %s", back)
	}
}

func TestModeRoundTrips(t *testing.T) {
	body := `{"model":"x","messages":[{"role":"user","content":"hi minoa, ssn 472-81-0094"}]}`
	for _, mode := range []string{"realistic", "variable", "label"} {
		g := build(Config{
			Enabled: true, Mode: mode, DetectPII: true,
			Entities: []CustomTerm{{Name: "minoa"}},
		}, "test-mode-"+mode)
		masked := string(g.MaskBody([]byte(body)))
		if strings.Contains(masked, "minoa") || strings.Contains(masked, "472-81-0094") {
			t.Fatalf("mode %s leaked: %s", mode, masked)
		}
		if mode == "variable" && !strings.Contains(masked, "{") {
			t.Fatalf("mode %s: want {TYPE_N} placeholder, got %s", mode, masked)
		}
		if mode == "label" && !strings.Contains(masked, "[") {
			t.Fatalf("mode %s: want [LABEL_N] placeholder, got %s", mode, masked)
		}
		if back := string(g.RestoreBody([]byte(masked))); back != body {
			t.Fatalf("mode %s round trip mismatch:\n got %s\nwant %s", mode, back, body)
		}
	}
}

func TestEntitiesWinOverTerms(t *testing.T) {
	d := NewCustomDetector(
		[]string{"york"}, // flat term, substring of the entity name
		[]CustomTerm{{Name: "new york", Type: "city"}},
		0.95,
	)
	spans := applyPolicy(d("visit new york soon"), nil)
	if len(spans) != 1 {
		t.Fatalf("want 1 merged span, got %v", spans)
	}
	if spans[0].Source != "custom-entity" {
		t.Fatalf("entity must win overlap, got %+v", spans[0])
	}
}

func TestGuardMapBounded(t *testing.T) {
	for i := 0; i < maxGuards+50; i++ {
		ForRequest(Config{Enabled: true, DetectPII: true}, "flood-"+string(rune('a'+i%26))+itoa(i))
	}
	guardMu.Lock()
	n := len(guards)
	guardMu.Unlock()
	if n > maxGuards {
		t.Fatalf("guards map grew past cap: %d > %d", n, maxGuards)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func TestRestoreKeepsValidJSON(t *testing.T) {
	g := build(Config{Enabled: true, DetectPII: true,
		Entities: []CustomTerm{{Name: "minoa", Replacement: `a"b`}}}, "test-restore-json")
	body := `{"model":"x","messages":[{"role":"user","content":"hi minoa"}]}`
	masked := string(g.MaskBody([]byte(body)))
	back := string(g.RestoreBody([]byte(masked)))
	if back != body {
		// restore of a quote-bearing replacement must not corrupt:
		// either exact round trip or masked passthrough, never broken json.
		if !jsonValid(back) {
			t.Fatalf("restore broke json: %s", back)
		}
	}
}

func jsonValid(s string) bool { return json.Valid([]byte(s)) }
