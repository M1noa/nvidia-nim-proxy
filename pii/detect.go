package pii

import (
	"regexp"
	"strings"
)

// deterministic recognizers: port of rampart heuristics.ts + validators.ts,
// plus secret shapes (gitleaks-style: keyword prefilter + regex).
// RE2 has no lookaheads, so boundary rules use char classes instead.

func luhnValid(digits string) bool {
	sum, dbl := 0, false
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if dbl {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		dbl = !dbl
	}
	return sum%10 == 0
}

func validSSN(d string) bool {
	if len(d) != 9 {
		return false
	}
	area, group, serial := d[:3], d[3:5], d[5:]
	if area == "000" || area == "666" || area >= "900" {
		return false
	}
	return group != "00" && serial != "0000"
}

var digitRunRe = regexp.MustCompile(`\d(?:[ .-]?\d)*`)

type digitRule struct {
	label   Label
	lengths []int
	check   func(string) bool
}

var digitRules = []digitRule{
	{LCreditCard, []int{16, 15, 14}, luhnValid},
	{LSSN, []int{9}, validSSN},
}

func detectDigitEntities(raw string) []Span {
	type run struct {
		digits string
		idx    []int
	}
	var runs []run
	for _, loc := range digitRunRe.FindAllStringIndex(raw, -1) {
		var d strings.Builder
		var ix []int
		for i := loc[0]; i < loc[1]; i++ {
			if c := raw[i]; c >= '0' && c <= '9' {
				d.WriteByte(c)
				ix = append(ix, i)
			}
		}
		if d.Len() > 0 {
			runs = append(runs, run{d.String(), ix})
		}
	}
	_ = runs
	var out []Span
	for _, r := range runs {
		for _, rule := range digitRules {
			hit := false
			for _, l := range rule.lengths {
				if len(r.digits) == l {
					hit = true
					break
				}
			}
			if !hit || !rule.check(r.digits) {
				continue
			}
			st := r.idx[0]
			en := r.idx[len(r.idx)-1] + 1
			out = append(out, Span{st, en, rule.label, 1, "heuristic", raw[st:en]})
			break
		}
	}
	return out
}

var textRules = []struct {
	label Label
	re    *regexp.Regexp
}{
	{LEmail, regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)},
	// exclude backslash too: bodies are raw json, so a url directly before
	// an escaped quote (https://x/y\") would otherwise swallow the escape
	// and the surrogate replacement would leave the quote unescaped,
	// producing invalid json upstream.
	{LURL, regexp.MustCompile(`https?://[^\s<>"'\\\])}]+`)},
	{LURL, regexp.MustCompile(`www\.[A-Za-z0-9.\-]+\.[A-Za-z]{2,}(?:/[^\s<>"'\\\])}]*)?`)},
	{LIPAddress, regexp.MustCompile(`(?:(?:25[0-5]|2[0-4][0-9]|1?[0-9]?[0-9])\.){3}(?:25[0-5]|2[0-4][0-9]|1?[0-9]?[0-9])`)},
	{LIPAddress, regexp.MustCompile(`(?:[0-9A-Fa-f]{2}[:-]){5}[0-9A-Fa-f]{2}`)},
	{LJWT, regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_\-]+`)},
	{LPrivateKey, regexp.MustCompile(`-----BEGIN (?:RSA |EC |DSA |OPENSSH )?PRIVATE KEY-----[\s\S]*?-----END (?:RSA |EC |DSA |OPENSSH )?PRIVATE KEY-----`)},
	{LIBAN, regexp.MustCompile(`[A-Z]{2}[0-9]{2}[ ]?(?:[0-9A-Z]{4}[ ]?){2,6}[0-9A-Z]{0,4}`)},
	{LPhone, regexp.MustCompile(`(?:\+?1[-. ]?)?\(?[0-9]{3}\)?[-. ][0-9]{3}[-. ][0-9]{4}`)},
	{LPhone, regexp.MustCompile(`\+[0-9][0-9 .\-()]{7,}[0-9]`)},
}

func detectTextEntities(raw string) []Span {
	var out []Span
	for _, r := range textRules {
		for _, loc := range r.re.FindAllStringIndex(raw, -1) {
			out = append(out, Span{loc[0], loc[1], r.label, 1, "heuristic", raw[loc[0]:loc[1]]})
		}
	}
	return out
}

// secret keyword prefilter: only run the expensive generic-key regexes when
// a keyword fires nearby (gitleaks-style port rules).
var secretKeywords = []string{
	"api_key", "apikey", "api-key", "secret", "token", "password", "passwd",
	"pwd", "auth", "credential", "private_key", "privatekey", "access_key",
	"secret_key", "client_secret", "aws_", "ghp_", "gho_", "github_token",
	"bearer", "sk-", "xox", "openai", "anthropic", "nvapi-",
}

var (
	genericKeyRe = regexp.MustCompile(`(?i)(?:api[_-]?key|secret|token|password|passwd|pwd|auth[_-]?token|access[_-]?key|client[_-]?secret)\s*[:=]\s*['"]?([A-Za-z0-9_\-./+]{12,})['"]?`)
	nvapiRe      = regexp.MustCompile(`nvapi-[A-Za-z0-9_\-]{16,}`)
	ghpRe        = regexp.MustCompile(`gh[op]_[A-Za-z0-9]{20,}`)
	skRe         = regexp.MustCompile(`sk-[A-Za-z0-9]{16,}`)
)

func hasSecretKeyword(raw string) bool {
	low := strings.ToLower(raw)
	for _, k := range secretKeywords {
		if strings.Contains(low, k) {
			return true
		}
	}
	return false
}

func detectSecrets(raw string) []Span {
	var out []Span
	for _, loc := range nvapiRe.FindAllStringIndex(raw, -1) {
		out = append(out, Span{loc[0], loc[1], LAPIKey, 1, "secret", raw[loc[0]:loc[1]]})
	}
	for _, loc := range ghpRe.FindAllStringIndex(raw, -1) {
		out = append(out, Span{loc[0], loc[1], LAPIKey, 1, "secret", raw[loc[0]:loc[1]]})
	}
	for _, loc := range skRe.FindAllStringIndex(raw, -1) {
		out = append(out, Span{loc[0], loc[1], LAPIKey, 1, "secret", raw[loc[0]:loc[1]]})
	}
	if !hasSecretKeyword(raw) {
		return out
	}
	for _, m := range genericKeyRe.FindAllStringSubmatchIndex(raw, -1) {
		if len(m) < 4 || m[2] < 0 {
			continue
		}
		out = append(out, Span{m[2], m[3], LAPIKey, 0.9, "secret", raw[m[2]:m[3]]})
	}
	return out
}

func detectHeuristics(raw string) []Span {
	var out []Span
	out = append(out, detectDigitEntities(raw)...)
	out = append(out, detectTextEntities(raw)...)
	out = append(out, detectSecrets(raw)...)
	return out
}
