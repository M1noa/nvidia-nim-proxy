package pii

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// LURL's fake pool is only 5 strings (5 example domains). Before the salt
// loop was bounded, the 6th distinct url in one vault spun forever inside
// Vault.For, hanging the request before it was even logged.
func TestForBoundedWhenPoolExhausted(t *testing.T) {
	v := NewVault(vaultKey(), time.Hour, 0)
	done := make(chan string, 1)
	go func() {
		var out string
		for i := 1; i <= 8; i++ {
			out = v.For(LURL, "https://example.com/"+strings.Repeat("p", i))
		}
		done <- out
	}()
	select {
	case s := <-done:
		if s == "" {
			t.Fatal("empty surrogate")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Vault.For spun past pool exhaustion (infinite salt loop)")
	}
}

func TestSurrogatesUniqueAndStableAfterExhaustion(t *testing.T) {
	v := NewVault(vaultKey(), time.Hour, 0)
	seen := map[string]bool{}
	first := map[string]string{}
	for round := 0; round < 2; round++ {
		for i := 1; i <= 8; i++ {
			orig := "https://example.com/" + strings.Repeat("p", i)
			s := v.For(LURL, orig)
			if prev, ok := first[orig]; ok {
				if prev != s {
					t.Fatalf("unstable surrogate for %q: %q then %q", orig, prev, s)
				}
				continue
			}
			first[orig] = s
			if seen[s] {
				t.Fatalf("duplicate surrogate %q for distinct originals", s)
			}
			seen[s] = true
		}
	}
}

// a url sitting right before an escaped quote (docs\") used to swallow
// the backslash into the url span; replacing it then left the quote
// unescaped and the body died upstream as invalid json.
func TestMaskBodyKeepsEscapedQuotesValid(t *testing.T) {
	g := build(Config{Enabled: true, DetectPII: true}, "test-esc")
	body := `{"model":"x","messages":[{"role":"user","content":"see https://example.com/docs\" ok"}]}`
	if !json.Valid([]byte(body)) {
		t.Fatal("test body itself must be valid json")
	}
	masked := g.MaskBody([]byte(body))
	if !json.Valid(masked) {
		t.Fatalf("masking broke json:\n%s", masked)
	}
	if strings.Contains(string(masked), "https://example.com/docs") {
		t.Fatalf("url was not masked:\n%s", masked)
	}
	back := string(g.RestoreBody(masked))
	if back != body {
		t.Fatalf("round trip mismatch:\n got %s\nwant %s", back, body)
	}
}

// exhausted-pool surrogates must still restore back to the original.
func TestExhaustedPoolSurrogateRestores(t *testing.T) {
	v := NewVault(vaultKey(), time.Hour, 0)
	orig := "https://example.com/docs"
	for i := 1; i <= 6; i++ {
		v.For(LURL, "https://example.com/"+strings.Repeat("p", i))
	}
	surr := v.For(LURL, orig)
	if back := restoreText(surr, v); back != orig {
		t.Fatalf("restore mismatch: %q -> %q", surr, back)
	}
}
