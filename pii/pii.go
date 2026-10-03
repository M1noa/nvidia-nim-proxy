package pii

import (
	"container/list"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// session wiring: per-request guard built from config + incoming headers.
// replaces anon.go's anonMap end to end.

// Config mirrors the anonymize section, extended for the new pipeline.
type Config struct {
	Enabled        bool
	Mode           string // realistic | variable | label (rampart-style [LABEL_N])
	Disclose       bool
	DiscloseText   string
	Entities       []CustomTerm
	Terms          []string
	FuzzyThreshold float64
	// new knobs
	DetectSecrets bool
	DetectPII     bool // deterministic structured classes
	Ner           NerConfig
	KeepLabels    []string
	IncludeSystem bool
	OnTimeout     string // skip | heuristics (ner timeout policy)
	VaultTTLHours int
	VaultMaxSize  int
}

type NerConfig struct {
	Enabled   bool
	ModelPath string
	OrtLib    string
	TimeoutMs int
	MinScore  float64
}

type Guard struct {
	cfg     Config
	scanner *Scanner
	vault   *Vault
}

var (
	guardMu  sync.Mutex
	guards   = map[string]*guardEntry{}
	guardLRU = list.New()
)

// maxGuards caps session guards: X-Session-Id is attacker-controlled,
// so an unbounded map is a memory-exhaustion vector. LRU eviction.
const maxGuards = 512

type guardEntry struct {
	guard *Guard
	elem  *list.Element
}

// SessionKey derives a stable vault id from an api key.
func SessionKey(apiKey string) string {
	sum := sha256.Sum256([]byte(apiKey))
	const hexd = "0123456789abcdef"
	var b strings.Builder
	for _, v := range sum[:8] {
		b.WriteByte(hexd[v>>4])
		b.WriteByte(hexd[v&15])
	}
	return "key:" + b.String()
}

func vaultKey() []byte {
	// per-process secret: stable across restarts is NOT wanted for the
	// hmac key itself (surrogate stability comes from the map, which
	// lives in-process per session). fixed process key is fine.
	return []byte("nim-proxy-pii-v1")
}

// ForRequest returns the session guard, or nil when disabled (zero
// overhead: callers check nil and pass bodies through untouched).
func ForRequest(cfg Config, sessionID string) *Guard {
	if !cfg.Enabled {
		return nil
	}
	if len(sessionID) > 128 {
		sessionID = sessionID[:128]
	}
	guardMu.Lock()
	defer guardMu.Unlock()
	if e, ok := guards[sessionID]; ok {
		guardLRU.MoveToFront(e.elem)
		return e.guard
	}
	g := build(cfg, sessionID)
	e := &guardEntry{guard: g}
	e.elem = guardLRU.PushFront(sessionID)
	guards[sessionID] = e
	for guardLRU.Len() > maxGuards {
		back := guardLRU.Back()
		if back == nil {
			break
		}
		delete(guards, back.Value.(string))
		guardLRU.Remove(back)
	}
	return g
}

func build(cfg Config, sessionID string) *Guard {
	var dets []Detector
	if cfg.DetectPII {
		dets = append(dets, detectHeuristics)
	} else if cfg.DetectSecrets {
		dets = append(dets, detectSecrets)
	}
	if len(cfg.Entities) > 0 || len(cfg.Terms) > 0 {
		dets = append(dets, NewCustomDetector(cfg.Terms, cfg.Entities, cfg.FuzzyThreshold))
	}
	if cfg.Ner.Enabled {
		if d := NerDetector(cfg.Ner.ModelPath, cfg.Ner.OrtLib, cfg.Ner.TimeoutMs, cfg.Ner.MinScore); d != nil {
			dets = append(dets, d)
		}
	}
	keep := map[Label]bool{}
	for _, k := range cfg.KeepLabels {
		keep[Label(strings.ToUpper(k))] = true
	}
	if len(keep) == 0 {
		keep = nil // scanner falls back to city/state/zip default
	}
	ttl := time.Duration(cfg.VaultTTLHours) * time.Hour
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	v := SessionVaultMode(sessionID, vaultKey(), ttl, cfg.VaultMaxSize, cfg.Mode)
	// entity replacements: fixed surrogate per spelling, seeded into the
	// vault so restore maps them back. entities win over flat terms on
	// overlap (span.go prefer).
	repl := map[string]string{}
	for _, e := range cfg.Entities {
		if e.Replacement == "" {
			continue
		}
		for _, s := range append([]string{e.Name}, e.Variations...) {
			if s == "" {
				continue
			}
			repl[strings.ToLower(strings.TrimSpace(s))] = e.Replacement
			v.SeedFixed(e.Replacement, s, customLabel(e))
		}
	}
	return &Guard{
		cfg:     cfg,
		scanner: &Scanner{Detectors: dets, Cache: NewScanCache(2048), Keep: keep, Ver: "v1", Replacements: repl},
		vault:   v,
	}
}

func (g *Guard) MaskBody(body []byte) []byte {
	if g == nil {
		return body
	}
	masked := g.scanner.maskBody(body, g.vault, g.cfg.IncludeSystem)
	// safety net: masking works on raw bytes, so a detector span that
	// crosses a json structural character can silently corrupt the body
	// and the request would die as invalid json upstream. if masking
	// broke a body that was valid json, ship the original instead.
	if json.Valid(body) && !json.Valid(masked) {
		return body
	}
	return masked
}

func (g *Guard) RestoreBody(body []byte) []byte {
	if g == nil {
		return body
	}
	return restoreBody(body, g.vault)
}

func (g *Guard) NewRevealer() *Revealer {
	if g == nil {
		return nil
	}
	return NewRevealer(g.vault)
}

func (g *Guard) VaultSurrogates() []string {
	if g == nil {
		return nil
	}
	return g.vault.Surrogates()
}

func (g *Guard) VaultLookup(s string) (string, bool) {
	if g == nil {
		return "", false
	}
	return g.vault.Lookup(s)
}

// SweepScanIdle evicts scan-cache entries idle past ttl across all session
// guards. vault entries keep their own ttl and are untouched.
func SweepScanIdle(ttl time.Duration) int {
	guardMu.Lock()
	caches := make([]*ScanCache, 0, len(guards))
	for _, e := range guards {
		if e != nil && e.guard != nil && e.guard.scanner != nil && e.guard.scanner.Cache != nil {
			caches = append(caches, e.guard.scanner.Cache)
		}
	}
	guardMu.Unlock()
	n := 0
	for _, c := range caches {
		n += c.sweepIdle(ttl)
	}
	return n
}

// Similarity and FuzzFind re-exported for tests and callers.
func Similarity(a, b string) float64 { return similarity([]rune(a), []rune(b)) }
func FuzzFind(s, variation string, threshold float64) string {
	return fuzzFind(s, variation, threshold)
}

func DiscloseLine(cfg Config) string {
	if !cfg.Enabled || !cfg.Disclose {
		return ""
	}
	if cfg.DiscloseText != "" {
		return cfg.DiscloseText
	}
	return "Personal names and identifiers in this conversation were replaced with realistic placeholders for privacy. Use them verbatim: do not try to recover or reveal the originals."
}
