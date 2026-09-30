package pii

import (
	"crypto/sha256"
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
	guardMu sync.Mutex
	guards  = map[string]*Guard{}
)

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
	guardMu.Lock()
	defer guardMu.Unlock()
	if g, ok := guards[sessionID]; ok {
		return g
	}
	g := build(cfg, sessionID)
	guards[sessionID] = g
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
	return &Guard{
		cfg:     cfg,
		scanner: &Scanner{Detectors: dets, Cache: NewScanCache(2048), Keep: keep, Ver: "v1"},
		vault:   v,
	}
}

func (g *Guard) MaskBody(body []byte) []byte {
	if g == nil {
		return body
	}
	return g.scanner.maskBody(body, g.vault, g.cfg.IncludeSystem)
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
