package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// --- term anonymization ---
//
// entities from config.yml (anonymize.entities, plus legacy anonymize.terms)
// swap for per-request masks before a request goes upstream, and swap back
// in every response path. the model only ever sees masks; logs and dumps
// do too.
//
// two modes: realistic (default) replaces each term with a same-length,
// same-shape random string, so tokenization and casing stay close to the
// original. variable replaces with {TYPE1}-style placeholders instead.
//
// entity matching is exact and case-sensitive on the name plus every
// variation; fuzzy matching at anonymize.fuzzy_threshold (default 0.95)
// catches near-miss spellings.

type anonPair struct{ from, to, efrom, eto string }

type anonMap struct {
	fwd  []anonPair // term -> mask, longest term first
	rev  []anonPair // mask -> term, longest mask first
	max  int        // longest mask, for stream tail buffering
	fuzz []anonFuzz // fuzzy entries, longest variation first
}

// anonFuzz is one variation eligible for fuzzy matching.
type anonFuzz struct {
	variation string // original spelling
	mask      string // its realistic replacement
	threshold float64
}

// realistic first/last names for name-type entities.
var anonFirstNames = []string{
	"Julie", "Marcus", "Priya", "Tomas", "Aisha", "Henrik", "Lena", "Omar",
	"Sofia", "Dmitri", "Nadia", "Carlos", "Ingrid", "Ravi", "Elena", "Kwame",
	"Anya", "Felix", "Mara", "Jonas", "Tessa", "Viktor", "Amara", "Lucia",
	"Stefan", "Noor", "Pavel", "Diana", "Marco", "Yuki", "Sanne", "Olivia",
}
var anonLastNames = []string{
	"Andersen", "Kowalski", "Tanaka", "Novak", "Garcia", "Lindqvist", "Moreau",
	"Kaur", "Silva", "Johansen", "Petrov", "Nguyen", "Costa", "Weber", "Ali",
	"Fischer", "Bakker", "Sato", "Larsen", "Meyer", "Dubois", "Khan", "Rossi",
	"Nakamura", "Berg", "Santos", "Wolf", "Haddad", "Jensen", "Kumar",
}

// newAnonMap builds per-request masks for terms. nil when nothing to do.
func newAnonMap(terms []string) *anonMap {
	m := buildAnonMap(terms, nil, "realistic", 0.95)
	if m == nil || len(m.fwd) == 0 {
		return nil
	}
	return m
}

// buildAnonMap is the full constructor: legacy terms plus entities.
func buildAnonMap(terms []string, entities []anonymizeEntity, mode string, threshold float64) *anonMap {
	if mode == "" {
		mode = "realistic"
	}
	if threshold <= 0 {
		threshold = 0.95
	}
	m := &anonMap{}
	seen := map[string]bool{} // every known spelling, guards collisions
	used := map[string]bool{} // every mask handed out
	addPair := func(from, to string) {
		if from == "" || to == "" || from == to {
			return
		}
		// reject masks colliding with any known spelling or mask.
		bad := func(s string) bool { return seen[s] || used[s] }
		if bad(to) {
			return
		}
		used[to] = true
		m.fwd = append(m.fwd, anonPair{from, to, jsonEscape(from), jsonEscape(to)})
		m.rev = append(m.rev, anonPair{to, from, jsonEscape(to), jsonEscape(from)})
		if len(to) > m.max {
			m.max = len(to)
		}
	}
	// legacy terms: exact only, realistic masks.
	var uniq []string
	for _, t := range terms {
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		uniq = append(uniq, t)
	}
	sort.Slice(uniq, func(i, j int) bool { return len(uniq[i]) > len(uniq[j]) })
	counters := map[string]int{}
	for _, t := range uniq {
		mk := randMask(t)
		for i := 0; (mk == t || used[mk] || seen[mk]) && i < 10; i++ {
			mk = randMask(t)
		}
		if mk == t || used[mk] || seen[mk] {
			continue // unmaskable (e.g. all punctuation), skip
		}
		addPair(t, mk)
	}
	// entities: name plus variations share one replacement.
	for i, e := range entities {
		if e.Name == "" {
			continue
		}
		spellings := append([]string{e.Name}, e.Variations...)
		var clean []string
		for _, s := range spellings {
			if s == "" || seen[s] {
				continue
			}
			seen[s] = true
			clean = append(clean, s)
		}
		if len(clean) == 0 {
			continue
		}
		repl := e.Replacement
		if repl == "" {
			if mode == "variable" {
				counters[e.Type]++
				repl = fmt.Sprintf("{%s%d}", strings.ToUpper(e.Type), counters[e.Type])
				if e.Type == "" {
					repl = fmt.Sprintf("{TERM%d}", i+1)
				}
			} else {
				repl = realisticReplacement(e)
			}
		}
		var masks []string
		if mode == "variable" {
			// one placeholder for every spelling.
			for range clean {
				masks = append(masks, repl)
			}
		} else {
			// first spelling gets the base replacement; the rest get
			// per-character structure maps off the same source so
			// m1noa -> j1lie keeps positions, case, and digit slots.
			src := repl
			if src == "" {
				src = clean[0]
			}
			for _, s := range clean {
				masks = append(masks, structMap(src, s))
			}
		}
		for j, s := range clean {
			mk := masks[j]
			// variable placeholders may repeat across spellings on
			// purpose; realistic masks must stay unique.
			if mode == "variable" {
				if !used[mk] {
					used[mk] = true
					m.fwd = append(m.fwd, anonPair{s, mk, jsonEscape(s), jsonEscape(mk)})
					m.rev = append(m.rev, anonPair{mk, clean[0], jsonEscape(mk), jsonEscape(clean[0])})
					if len(mk) > m.max {
						m.max = len(mk)
					}
				} else {
					m.fwd = append(m.fwd, anonPair{s, mk, jsonEscape(s), jsonEscape(mk)})
				}
				continue
			}
			if mk == s || used[mk] || seen[mk] {
				continue
			}
			addPair(s, mk)
		}
		// fuzzy entries for name-like types and multi-char spellings.
		if threshold < 1 {
			for j, s := range clean {
				if j >= len(masks) {
					break
				}
				if entityFuzzyEligible(e.Type, s) {
					m.fuzz = append(m.fuzz, anonFuzz{s, masks[j], threshold})
				}
			}
		}
	}
	if len(m.fwd) == 0 {
		return nil
	}
	sort.Slice(m.fwd, func(i, j int) bool { return len(m.fwd[i].from) > len(m.fwd[j].from) })
	sort.Slice(m.rev, func(i, j int) bool { return len(m.rev[i].from) > len(m.rev[j].from) })
	sort.Slice(m.fuzz, func(i, j int) bool { return len(m.fuzz[i].variation) > len(m.fuzz[j].variation) })
	return m
}

// entityFuzzyEligible reports whether a spelling should get fuzzy matching:
// names, usernames, and anything else long enough to survive a 95% gate.
func entityFuzzyEligible(typ, s string) bool {
	n := utf8.RuneCountInString(s)
	switch strings.ToLower(typ) {
	case "name", "username", "user", "person", "other", "":
		return n >= 4
	}
	return n >= 6
}

// realisticReplacement picks a same-shape random value for an entity: real
// names for name types, struct-mapped randomness otherwise.
func realisticReplacement(e anonymizeEntity) string {
	switch strings.ToLower(e.Type) {
	case "name", "person", "user", "username":
		parts := strings.Fields(e.Name)
		if len(parts) >= 2 {
			return randChoice(anonFirstNames) + " " + randChoice(anonLastNames)
		}
		if utf8.RuneCountInString(e.Name) <= 12 {
			return randChoice(anonFirstNames)
		}
		return randChoice(anonFirstNames) + " " + randChoice(anonLastNames)
	case "email":
		return strings.ToLower(randChoice(anonFirstNames)) + randDigits(2) + "@example.com"
	case "phone":
		return "+1" + randDigits(10)
	}
	return ""
}

// structMap maps src onto target's shape: same rune count, per-position
// classes (upper, lower, digit, other) taken from target, random values
// drawn from src's character pool where possible. m1noa with src julie
// gives j1lie: letters remapped, digit slot kept, case kept.
func structMap(src, target string) string {
	sr := []rune(src)
	tr := []rune(target)
	if len(sr) == 0 || len(tr) == 0 {
		return randMask(target)
	}
	// pools of source letters by case, for flavor carryover.
	var lo, up, dg []rune
	for _, r := range sr {
		switch {
		case r >= 'a' && r <= 'z':
			lo = append(lo, r)
		case r >= 'A' && r <= 'Z':
			up = append(up, r)
		case r >= '0' && r <= '9':
			dg = append(dg, r)
		}
	}
	var b strings.Builder
	for i, r := range tr {
		s := sr[i%len(sr)]
		switch {
		case r >= 'a' && r <= 'z':
			if len(lo) > 0 {
				b.WriteRune(unicode.ToLower(lo[randIntn(len(lo))]))
			} else {
				b.WriteRune(unicode.ToLower(randLetter()))
			}
			_ = s
		case r >= 'A' && r <= 'Z':
			if len(up) > 0 {
				b.WriteRune(unicode.ToUpper(up[randIntn(len(up))]))
			} else if len(lo) > 0 {
				b.WriteRune(unicode.ToUpper(lo[randIntn(len(lo))]))
			} else {
				b.WriteRune(unicode.ToUpper(randLetter()))
			}
		case r >= '0' && r <= '9':
			if len(dg) > 0 {
				b.WriteRune(dg[randIntn(len(dg))])
			} else {
				b.WriteRune(randDigit())
			}
		default:
			// symbols and spaces pass through so the shape holds.
			b.WriteRune(r)
		}
	}
	return b.String()
}

// randMask returns a same-length random string: letters stay letters (case
// kept), digits stay digits, everything else passes through so shape and
// tokenization stay close to the original.
func randMask(term string) string {
	var b strings.Builder
	for _, r := range term {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteByte("abcdefghijklmnopqrstuvwxyz"[randIntn(26)])
		case r >= 'A' && r <= 'Z':
			b.WriteByte("ABCDEFGHIJKLMNOPQRSTUVWXYZ"[randIntn(26)])
		case r >= '0' && r <= '9':
			b.WriteByte("0123456789"[randIntn(10)])
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func randLetter() rune { return rune("abcdefghijklmnopqrstuvwxyz"[randIntn(26)]) }
func randDigit() rune  { return rune("0123456789"[randIntn(10)]) }

func randDigits(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteRune(randDigit())
	}
	return b.String()
}

func randChoice(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	return ss[randIntn(len(ss))]
}

func randIntn(n int) int {
	if n <= 0 {
		return 0
	}
	var one [1]byte
	if _, err := rand.Read(one[:]); err != nil {
		return 0
	}
	return int(one[0]) % n
}

// anonymize swaps terms for masks, keeping the top-level model field intact
// so routing and upstream model selection never break.
func (m *anonMap) anonymize(body []byte) []byte {
	if m == nil {
		return body
	}
	model := reqModel(body)
	replaced := false
	for _, p := range m.fwd {
		for _, q := range [][2]string{{p.from, p.to}, {p.efrom, p.eto}} {
			if q[0] == q[1] {
				continue
			}
			if bytes.Contains(body, []byte(q[0])) {
				body = bytes.ReplaceAll(body, []byte(q[0]), []byte(q[1]))
				replaced = true
			}
		}
	}
	// fuzzy pass for near-miss spellings.
	if len(m.fuzz) > 0 {
		if nb, ok := m.fuzzAnonymize(body); ok {
			body = nb
			replaced = true
		}
	}
	if replaced && model != "" && reqModel(body) != model {
		body = restoreModel(body, model)
	}
	return body
}

// fuzzAnonymize replaces near-miss spellings of known variations with the
// entity mask. returns false when nothing matched.
func (m *anonMap) fuzzAnonymize(body []byte) ([]byte, bool) {
	// only scan text-ish content: run per line to bound cost.
	lines := strings.Split(string(body), "\n")
	changed := false
	for li, line := range lines {
		for _, f := range m.fuzz {
			hit := fuzzFind(line, f.variation, f.threshold)
			if hit == "" || hit == f.mask {
				continue
			}
			// skip hits already covered by exact pairs.
			skip := false
			for _, p := range m.fwd {
				if hit == p.from || hit == p.to {
					skip = hit == p.to
					break
				}
			}
			if skip {
				continue
			}
			line = strings.ReplaceAll(line, hit, f.mask)
			changed = true
		}
		lines[li] = line
	}
	if !changed {
		return nil, false
	}
	return []byte(strings.Join(lines, "\n")), true
}

// fuzzFind returns the first substring of s similar enough to variation,
// or "". similarity is 1 - levenshtein/maxlen over rune windows.
func fuzzFind(s, variation string, threshold float64) string {
	sr := []rune(s)
	vr := []rune(variation)
	n := len(vr)
	if n == 0 || len(sr) < n {
		return ""
	}
	// windows of len-1, len, len+1 catch insertions/deletions.
	for _, w := range []int{n - 1, n, n + 1} {
		if w < 3 || w > len(sr) {
			continue
		}
		for i := 0; i+w <= len(sr); i++ {
			win := sr[i : i+w]
			if similarity(win, vr) >= threshold {
				return string(win)
			}
		}
	}
	return ""
}

// similarity is 1 - levenshtein(a,b)/max(len). case-sensitive.
func similarity(a, b []rune) float64 {
	m, n := len(a), len(b)
	if m == 0 && n == 0 {
		return 1
	}
	if m == 0 || n == 0 {
		return 0
	}
	prev := make([]int, n+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= m; i++ {
		cur := make([]int, n+1)
		cur[0] = i
		for j := 1; j <= n; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min3(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	mx := m
	if n > mx {
		mx = n
	}
	return 1 - float64(prev[n])/float64(mx)
}

func min3(a, b, c int) int {
	if a > b {
		a = b
	}
	if a > c {
		a = c
	}
	return a
}

// anonForRequest builds the per-request map from config. nil when disabled
// or termless, and every method is a nil-safe passthrough then.
func anonForRequest() *anonMap {
	c := cfg().Anonymize
	if !c.Enabled {
		return nil
	}
	if len(c.Entities) == 0 && len(c.Terms) == 0 {
		return nil
	}
	return buildAnonMap(c.Terms, c.Entities, c.Mode, c.FuzzyThreshold)
}

// discloseLine returns the anonymize notice for system prompts, or "".
func discloseLine() string {
	c := cfg().Anonymize
	if !c.Enabled || !c.Disclose {
		return ""
	}
	if c.DiscloseText != "" {
		return c.DiscloseText
	}
	return "Personal names and identifiers in this conversation were replaced with realistic placeholders for privacy. Use them verbatim: do not try to recover or reveal the originals."
}

// deanonymize swaps masks back for originals.
func (m *anonMap) deanonymize(body []byte) []byte {
	if m == nil {
		return body
	}
	for _, p := range m.rev {
		for _, q := range [][2]string{{p.from, p.to}, {p.efrom, p.eto}} {
			if q[0] == q[1] {
				continue
			}
			if bytes.Contains(body, []byte(q[0])) {
				body = bytes.ReplaceAll(body, []byte(q[0]), []byte(q[1]))
			}
		}
	}
	return body
}

// jsonEscape renders s the way encoding/json escapes a string body
// (no surrounding quotes), so terms with unicode still match.
func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	if len(b) >= 2 {
		return string(b[1 : len(b)-1])
	}
	return s
}

// restoreModel resets a top-level "model" field clobbered by masking.
func restoreModel(body []byte, model string) []byte {
	var m map[string]any
	if json.Unmarshal(body, &m) == nil {
		if _, ok := m["model"]; ok {
			m["model"] = model
			if b, err := json.Marshal(m); err == nil {
				return b
			}
		}
	}
	return body
}

// deanonWriter deanonymizes streaming response bytes, holding back a tail
// so masks split across writes still match.
type deanonWriter struct {
	w    http.ResponseWriter
	m    *anonMap
	tail []byte
}

func wrapDeanon(w http.ResponseWriter, m *anonMap) *deanonWriter {
	if m == nil {
		return nil
	}
	return &deanonWriter{w: w, m: m}
}

func (d *deanonWriter) Header() http.Header { return d.w.Header() }
func (d *deanonWriter) WriteHeader(s int)   { d.w.WriteHeader(s) }

func (d *deanonWriter) Write(b []byte) (int, error) {
	data := make([]byte, 0, len(d.tail)+len(b))
	data = append(data, d.tail...)
	data = append(data, b...)
	keep := d.m.max - 1
	if keep < 0 {
		keep = 0
	}
	if len(data) <= keep {
		d.tail = data
		return len(b), nil
	}
	cut := d.unsplit(data, len(data)-keep)
	out := d.m.deanonymize(data[:cut])
	d.tail = append([]byte(nil), data[cut:]...)
	if _, err := d.w.Write(out); err != nil {
		return 0, err
	}
	return len(b), nil
}

// unsplit pulls cut back past any partial mask at the boundary, so a mask
// split across writes is never emitted half-replaced and lost.
func (d *deanonWriter) unsplit(data []byte, cut int) int {
	for {
		moved := false
		head := data[:cut]
		for _, p := range d.m.rev {
			for _, mk := range []string{p.from, p.efrom} {
				if mk == "" {
					continue
				}
				mb := []byte(mk)
				maxL := len(mb) - 1
				if maxL > len(head) {
					maxL = len(head)
				}
				for L := maxL; L > 0; L-- {
					if bytes.HasSuffix(head, mb[:L]) {
						cut -= L
						head = data[:cut]
						moved = true
						break
					}
				}
			}
		}
		if !moved {
			return cut
		}
	}
}

// Flush forwards flushes so SSE keeps streaming.
func (d *deanonWriter) Flush() {
	if f, ok := d.w.(http.Flusher); ok {
		f.Flush()
	}
}

// finish flushes the held tail at end of response.
func (d *deanonWriter) finish() {
	if d == nil {
		return
	}
	if len(d.tail) > 0 {
		d.w.Write(d.m.deanonymize(d.tail))
		d.tail = nil
	}
}
