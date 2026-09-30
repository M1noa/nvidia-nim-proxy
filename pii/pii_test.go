package pii

import (
	"strings"
	"testing"
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
