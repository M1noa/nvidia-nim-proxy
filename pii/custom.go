package pii

import (
	"sort"
	"strings"
	"unicode/utf8"
)

// custom terms: config entities + legacy terms as a span detector with
// exact + fuzzy (levenshtein) matching. keeps `minoa`-style personal
// entries working that no model would catch.

type CustomTerm struct {
	Name        string
	Type        string
	Variations  []string
	Replacement string
}

func customLabel(t CustomTerm) Label {
	switch strings.ToLower(t.Type) {
	case "name", "person", "user", "username":
		return LCustom
	case "email":
		return LEmail
	case "phone":
		return LPhone
	default:
		return LCustom
	}
}

func fuzzyEligible(typ, s string) bool {
	n := utf8.RuneCountInString(s)
	switch strings.ToLower(typ) {
	case "name", "username", "user", "person", "other", "":
		return n >= 4
	}
	return n >= 6
}

type customDetector struct {
	spellings []string
	threshold float64
}

func NewCustomDetector(terms []string, entities []CustomTerm, threshold float64) Detector {
	if threshold <= 0 {
		threshold = 0.95
	}
	var spells []string
	seen := map[string]bool{}
	for _, t := range terms {
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		spells = append(spells, t)
	}
	for _, e := range entities {
		for _, s := range append([]string{e.Name}, e.Variations...) {
			if s == "" || seen[s] {
				continue
			}
			seen[s] = true
			spells = append(spells, s)
		}
	}
	sort.Slice(spells, func(i, j int) bool { return len(spells[i]) > len(spells[j]) })
	d := &customDetector{spells, threshold}
	return d.detect
}

func (d *customDetector) detect(text string) []Span {
	var out []Span
	for _, sp := range d.spellings {
		start := 0
		for {
			i := strings.Index(text[start:], sp)
			if i < 0 {
				break
			}
			st := start + i
			out = append(out, Span{st, st + len(sp), LCustom, 1, "custom", sp})
			start = st + len(sp)
		}
	}
	// fuzzy pass per line for near-miss spellings.
	for _, line := range strings.Split(text, "\n") {
		for _, sp := range d.spellings {
			if !fuzzyEligible("", sp) {
				continue
			}
			hit := fuzzFind(line, sp, d.threshold)
			if hit == "" {
				continue
			}
			i := strings.Index(line, hit)
			if i < 0 {
				continue
			}
			// anchor to full-text offset of this line occurrence.
			base := strings.Index(text, line)
			if base < 0 {
				continue
			}
			out = append(out, Span{base + i, base + i + len(hit), LCustom, 0.9, "custom", hit})
		}
	}
	return out
}

func fuzzFind(s, variation string, threshold float64) string {
	sr := []rune(s)
	vr := []rune(variation)
	n := len(vr)
	if n == 0 || len(sr) < n {
		return ""
	}
	for _, w := range []int{n - 1, n, n + 1} {
		if w < 3 || w > len(sr) {
			continue
		}
		for i := 0; i+w <= len(sr); i++ {
			if similarity(sr[i:i+w], vr) >= threshold {
				return string(sr[i : i+w])
			}
		}
	}
	return ""
}

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
			c0 := prev[j] + 1
			c1 := cur[j-1] + 1
			c2 := prev[j-1] + cost
			if c1 < c0 {
				c0 = c1
			}
			if c2 < c0 {
				c0 = c2
			}
			cur[j] = c0
		}
		prev = cur
	}
	mx := m
	if n > mx {
		mx = n
	}
	return 1 - float64(prev[n])/float64(mx)
}
