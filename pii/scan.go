package pii

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// scanner: paragraph-chunked detection with an lru span cache, scope-limited
// masking for oai + anthropic bodies, and streaming restore.

type cacheEntry struct {
	key   string
	spans []Span
	elem  *list.Element
	// lastAccess stamps the last hit/put, so the sweep can evict
	// entries idle past the scan-cache ttl (vault entries live longer).
	lastAccess time.Time
}

type ScanCache struct {
	mu    sync.Mutex
	max   int
	items map[string]*cacheEntry
	lru   *list.List
	Hits  int
	Miss  int
}

func NewScanCache(max int) *ScanCache {
	if max <= 0 {
		max = 2048
	}
	return &ScanCache{max: max, items: map[string]*cacheEntry{}, lru: list.New()}
}

func cacheKey(text, ver string) string {
	h := sha256.New()
	h.Write([]byte(ver))
	h.Write([]byte{0})
	h.Write([]byte(text))
	return hex.EncodeToString(h.Sum(nil))
}

func (c *ScanCache) get(key string) ([]Span, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		c.lru.MoveToFront(e.elem)
		e.lastAccess = time.Now()
		c.Hits++
		return e.spans, true
	}
	c.Miss++
	return nil, false
}

func (c *ScanCache) put(key string, spans []Span) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		e.spans = spans
		e.lastAccess = time.Now()
		c.lru.MoveToFront(e.elem)
		return
	}
	e := &cacheEntry{key: key, spans: spans, lastAccess: time.Now()}
	e.elem = c.lru.PushFront(key)
	c.items[key] = e
	for c.lru.Len() > c.max {
		back := c.lru.Back()
		if back == nil {
			break
		}
		delete(c.items, back.Value.(string))
		c.lru.Remove(back)
	}
}

// sweepIdle evicts entries untouched for longer than ttl. the vault keeps
// its own (longer) ttl: evicting a scan entry never evicts vault entries.
func (c *ScanCache) sweepIdle(ttl time.Duration) int {
	if ttl <= 0 {
		return 0
	}
	cutoff := time.Now().Add(-ttl)
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for k, e := range c.items {
		if e.lastAccess.Before(cutoff) {
			delete(c.items, k)
			c.lru.Remove(e.elem)
			n++
		}
	}
	return n
}

type Detector func(text string) []Span

type Scanner struct {
	Detectors []Detector
	Cache     *ScanCache
	Keep      map[Label]bool
	Ver       string
	Timeout   int // reserved for ner timeout ms
	// Replacements maps lowercased entity spellings to a fixed
	// surrogate, bypassing the vault when set.
	Replacements map[string]string
}

// surrogateFor returns a fixed replacement for entity spellings, else "".
func (s *Scanner) surrogateFor(orig string) string {
	if len(s.Replacements) == 0 {
		return ""
	}
	return s.Replacements[strings.ToLower(strings.TrimSpace(orig))]
}

func splitParagraphs(s string) []string {
	return strings.Split(s, "\n\n")
}

// detect runs detectors per paragraph with cache; offsets shifted back.
func (s *Scanner) detect(text string) []Span {
	var out []Span
	base := 0
	for _, para := range splitParagraphs(text) {
		if strings.TrimSpace(para) == "" {
			base += len(para) + 2
			continue
		}
		key := cacheKey(para, s.Ver)
		spans, ok := s.Cache.get(key)
		if !ok {
			for _, d := range s.Detectors {
				spans = append(spans, d(para)...)
			}
			s.Cache.put(key, spans)
		}
		for _, sp := range spans {
			sp.Start += base
			sp.End += base
			sp.Text = text[sp.Start:sp.End]
			out = append(out, sp)
		}
		base += len(para) + 2
	}
	return out
}

// maskText replaces redactable spans with vault surrogates, right to left.
func (s *Scanner) maskText(text string, v *Vault) string {
	spans := applyPolicy(s.detect(text), s.Keep)
	for _, sp := range spans {
		surr := s.surrogateFor(text[sp.Start:sp.End])
		if surr == "" {
			surr = v.For(sp.Label, text[sp.Start:sp.End])
		}
		text = text[:sp.Start] + surr + text[sp.End:]
	}
	return text
}

// restoreText swaps surrogates back, longest first. esc form handles json.
func restoreText(text string, v *Vault) string {
	for _, surr := range longestFirst(v.Surrogates()) {
		orig, ok := v.Lookup(surr)
		if !ok {
			continue
		}
		if strings.Contains(text, surr) {
			text = strings.ReplaceAll(text, surr, orig)
		}
		je, jo := jsonEscape(surr), jsonEscape(orig)
		if je != surr && strings.Contains(text, je) {
			text = strings.ReplaceAll(text, je, jo)
		}
	}
	return text
}

func longestFirst(ss []string) []string {
	for i := 1; i < len(ss); i++ {
		for j := i; j > 0 && len(ss[j]) > len(ss[j-1]); j-- {
			ss[j], ss[j-1] = ss[j-1], ss[j]
		}
	}
	return ss
}

func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	if len(b) >= 2 {
		return string(b[1 : len(b)-1])
	}
	return s
}

// maskBody masks pii spans directly in the raw bytes so the body stays
// byte-identical apart from the replacements (key order, spacing kept).
// structured scope: skip the top-level model id and image/base64 blobs by
// pre-marking their ranges as no-scan zones.
func (s *Scanner) maskBody(body []byte, v *Vault, includeSystem bool) []byte {
	text := string(body)
	zones := noScanZones(text, includeSystem)
	spans := s.detectZoned(text, zones)
	spans = applyPolicy(spans, s.Keep)
	model, _ := topModel(text)
	for _, sp := range spans {
		orig := text[sp.Start:sp.End]
		surr := s.surrogateFor(orig)
		if surr == "" {
			surr = v.For(sp.Label, orig)
		}
		text = text[:sp.Start] + surr + text[sp.End:]
		// shift later zones/spans: recompute by re-walking is costly;
		// spans are right-to-left so earlier offsets stay valid.
		_ = zones
	}
	if model != "" && !strings.Contains(text, `"`+model+`"`) {
		text = restoreTopModel(text, model)
	}
	return []byte(text)
}

// topModel reads the top-level model id for post-mask restore.
func topModel(text string) (string, bool) {
	var m map[string]any
	if json.Unmarshal([]byte(text), &m) != nil {
		return "", false
	}
	md, ok := m["model"].(string)
	return md, ok
}

func restoreTopModel(text, model string) string {
	var m map[string]any
	if json.Unmarshal([]byte(text), &m) != nil {
		return text
	}
	if _, ok := m["model"]; ok {
		m["model"] = model
		if b, err := json.Marshal(m); err == nil {
			// key order may shift here, but only when a mask actually
			// hit the model id; the common path returns bytes untouched.
			return string(b)
		}
	}
	return text
}

// noScanZones returns byte ranges to exclude: image/base64 blobs always,
// system content unless includeSystem. implemented as offset ranges over
// the raw text by locating string values under image keys and system roles.
func noScanZones(text string, includeSystem bool) [][2]int {
	var m map[string]any
	if json.Unmarshal([]byte(text), &m) != nil {
		return nil
	}
	var zones [][2]int
	// find base64-ish long token values and system strings by re-scanning
	// the raw text for their exact value occurrences.
	var strs []string
	collectSkips(m, includeSystem, &strs)
	for _, s := range strs {
		if len(s) < 24 {
			continue
		}
		start := 0
		for {
			i := strings.Index(text[start:], s)
			if i < 0 {
				break
			}
			st := start + i
			zones = append(zones, [2]int{st, st + len(s)})
			start = st + len(s)
		}
	}
	return zones
}

func collectSkips(x any, includeSystem bool, out *[]string) {
	switch t := x.(type) {
	case map[string]any:
		typ, _ := t["type"].(string)
		role, _ := t["role"].(string)
		if typ == "image" || typ == "image_url" || typ == "input_image" || typ == "input_file" || typ == "document" {
			if s, ok := t["url"].(string); ok {
				*out = append(*out, s)
			}
			if src, ok := t["source"].(map[string]any); ok {
				if d, ok := src["data"].(string); ok {
					*out = append(*out, d)
				}
			}
			if u, ok := t["image_url"]; ok {
				if s, ok := u.(string); ok {
					*out = append(*out, s)
				} else if mm, ok := u.(map[string]any); ok {
					if s, ok := mm["url"].(string); ok {
						*out = append(*out, s)
					}
				}
			}
			return
		}
		if (role == "system" || role == "developer") && !includeSystem {
			if c, ok := t["content"]; ok {
				flattenStrings(c, out)
			}
			if in, ok := t["input"]; ok {
				flattenStrings(in, out)
			}
			// still walk tool_calls (live args mask even in system msgs)
			if tc, ok := t["tool_calls"]; ok {
				collectSkips(tc, includeSystem, out)
			}
			return
		}
		for _, v := range t {
			collectSkips(v, includeSystem, out)
		}
	case []any:
		for _, v := range t {
			collectSkips(v, includeSystem, out)
		}
	}
}

func flattenStrings(x any, out *[]string) {
	switch t := x.(type) {
	case string:
		*out = append(*out, t)
	case []any:
		for _, v := range t {
			flattenStrings(v, out)
		}
	case map[string]any:
		for _, v := range t {
			flattenStrings(v, out)
		}
	}
}

// detectZoned runs detectors per paragraph, dropping spans inside no-scan
// zones. zones merge with paragraph offsets: paragraphs index raw text.
func (s *Scanner) detectZoned(text string, zones [][2]int) []Span {
	inside := func(st, en int) bool {
		for _, z := range zones {
			if st < z[1] && en > z[0] {
				return true
			}
		}
		return false
	}
	var out []Span
	base := 0
	for _, para := range splitParagraphs(text) {
		if strings.TrimSpace(para) == "" {
			base += len(para) + 2
			continue
		}
		// shift paragraph-local zone bounds
		var pz [][2]int
		for _, z := range zones {
			if z[1] > base && z[0] < base+len(para) {
				pz = append(pz, [2]int{z[0] - base, z[1] - base})
			}
		}
		key := cacheKey(para, s.Ver)
		spans, ok := s.Cache.get(key)
		if !ok {
			for _, d := range s.Detectors {
				spans = append(spans, d(para)...)
			}
			s.Cache.put(key, spans)
		}
		for _, sp := range spans {
			if inside(base+sp.Start, base+sp.End) {
				continue
			}
			// also drop paragraph-local zone hits (belt and suspenders
			// for spans found before zone shifting).
			drop := false
			for _, z := range pz {
				if sp.Start < z[1] && sp.End > z[0] {
					drop = true
					break
				}
			}
			if drop {
				continue
			}
			sp.Start += base
			sp.End += base
			sp.Text = text[sp.Start:sp.End]
			out = append(out, sp)
		}
		base += len(para) + 2
	}
	return out
}

// restoreBody swaps surrogates back in a complete response body.
func restoreBody(body []byte, v *Vault) []byte {
	if v == nil {
		return body
	}
	s := restoreText(string(body), v)
	return []byte(s)
}

// revealer restores placeholders in a byte stream, holding only a suffix
// that could still be a partial surrogate so ttft is unchanged.
type Revealer struct {
	v   *Vault
	buf []byte
	max int
}

func NewRevealer(v *Vault) *Revealer {
	n := 64
	if v != nil {
		if m := v.MaxSurrogateLen(); m+8 > n {
			n = m + 8
		}
	}
	return &Revealer{v: v, max: n}
}

func (r *Revealer) Push(chunk []byte) []byte {
	if r.v == nil {
		return chunk
	}
	data := append(append([]byte(nil), r.buf...), chunk...)
	if len(data) <= r.max {
		r.buf = data
		return nil
	}
	cut := len(data) - r.max
	cut = unsplit(data, cut, r.v)
	out := restoreBody(data[:cut], r.v)
	r.buf = append([]byte(nil), data[cut:]...)
	return out
}

func (r *Revealer) Flush() []byte {
	if r.v == nil || len(r.buf) == 0 {
		return nil
	}
	out := restoreBody(r.buf, r.v)
	r.buf = nil
	return out
}

func unsplit(data []byte, cut int, v *Vault) int {
	for {
		moved := false
		for _, surr := range v.Surrogates() {
			mb := []byte(surr)
			maxL := len(mb) - 1
			if maxL > cut {
				maxL = cut
			}
			for l := maxL; l > 0; l-- {
				if bytes.HasSuffix(data[:cut], mb[:l]) {
					cut -= l
					moved = true
					break
				}
			}
			if moved {
				break
			}
		}
		if !moved {
			return cut
		}
	}
}
