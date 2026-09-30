package pii

import (
	"fmt"
	"strings"
)

// shape-preserving fakes selected by hmac bytes. same (label, original)
// always yields the same fake; fakes look valid (luhn cards, real domains).

var firstNames = []string{
	"Julie", "Marcus", "Priya", "Tomas", "Aisha", "Henrik", "Lena", "Omar",
	"Sofia", "Dmitri", "Nadia", "Carlos", "Ingrid", "Ravi", "Elena", "Kwame",
	"Anya", "Felix", "Mara", "Jonas", "Tessa", "Viktor", "Amara", "Lucia",
	"Stefan", "Noor", "Pavel", "Diana", "Marco", "Yuki", "Sanne", "Olivia",
	"Ruth", "Emil", "Zara", "Hugo", "Mira", "Owen", "Sana", "Peter",
}
var lastNames = []string{
	"Andersen", "Kowalski", "Tanaka", "Novak", "Garcia", "Lindqvist", "Moreau",
	"Kaur", "Silva", "Johansen", "Petrov", "Nguyen", "Costa", "Weber", "Ali",
	"Fischer", "Bakker", "Sato", "Larsen", "Meyer", "Dubois", "Khan", "Rossi",
	"Nakamura", "Berg", "Santos", "Wolf", "Haddad", "Jensen", "Kumar",
	"Lund", "Park", "Muller", "Osei", "Vidal", "Novakova", "Ito", "Bakker",
}
var fakeDomains = []string{"example.com", "example.net", "example.org", "mail.example", "post.example"}

func pick(sum []byte, off int, n int) int {
	if n <= 0 {
		return 0
	}
	return int(sum[off%len(sum)]) % n
}

// variableFor mints {TYPE_N} placeholders; the counter derives from hmac
// bytes so the same original keeps its number without shared state.
func variableFor(label Label, original string, sum []byte, v *Vault) string {
	n := pick(sum, 4, 900) + 100
	_ = original
	return "{" + string(label) + fmt.Sprint(n) + "}"
}

// labelFor mints rampart-style [LABEL_N] placeholders.
func labelFor(label Label, sum []byte, v *Vault) string {
	n := pick(sum, 4, 900) + 100
	_ = v
	return "[" + string(label) + "_" + fmt.Sprint(n) + "]"
}

func maskShape(original string, sum []byte, alpha string) string {
	var b strings.Builder
	for i, r := range original {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteByte(alpha[(int(sum[i%len(sum)])+i)%len(alpha)])
		case r >= 'A' && r <= 'Z':
			b.WriteByte(alpha[(int(sum[i%len(sum)])+i)%len(alpha)] - 32)
		case r >= '0' && r <= '9':
			b.WriteByte(byte('0' + (int(sum[i%len(sum)])+i)%10))
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func fakeFor(label Label, original string, sum []byte) string {
	low := "abcdefghijklmnopqrstuvwxyz"
	switch label {
	case LGivenName:
		return firstNames[pick(sum, 0, len(firstNames))]
	case LSurname:
		return lastNames[pick(sum, 0, len(lastNames))]
	case LCustom:
		// custom terms keep prior behavior: same-length same-shape.
		return maskShape(original, sum, low)
	case LEmail:
		local := strings.ToLower(firstNames[pick(sum, 0, len(firstNames))]) + fmt.Sprint(pick(sum, 1, 90)+10)
		return local + "@" + fakeDomains[pick(sum, 2, len(fakeDomains))]
	case LPhone:
		return fmt.Sprintf("+1%03d%03d%04d", pick(sum, 0, 800)+200, pick(sum, 1, 800)+200, pick(sum, 2, 9000)+1000)
	case LIPAddress:
		if strings.Contains(original, ":") || strings.Contains(original, "-") {
			parts := make([]string, 6)
			for i := range parts {
				parts[i] = fmt.Sprintf("%02x", sum[i%len(sum)])
			}
			sep := ":"
			if strings.Contains(original, "-") {
				sep = "-"
			}
			return strings.Join(parts, sep)
		}
		return fmt.Sprintf("%d.%d.%d.%d", pick(sum, 0, 223)+1, pick(sum, 1, 256), pick(sum, 2, 256), pick(sum, 3, 254)+1)
	case LURL:
		return "https://" + fakeDomains[pick(sum, 0, len(fakeDomains))] + "/page"
	case LSSN:
		a, g, s := pick(sum, 0, 899)+1, pick(sum, 1, 99)+1, pick(sum, 2, 9999)+1
		if a == 666 {
			a = 665
		}
		sep := "-"
		if !strings.Contains(original, "-") {
			sep = ""
		}
		if sep == "" {
			return fmt.Sprintf("%03d%02d%04d", a, g, s)
		}
		return fmt.Sprintf("%03d-%02d-%04d", a, g, s)
	case LCreditCard:
		return luhnFake(sum)
	case LJWT, LAPIKey, LPrivateKey, LIBAN, LTaxID, LBankAccount, LRoutingNumber,
		LGovernmentID, LPassport, LDriversLicense:
		return maskShape(original, sum, low)
	default:
		return maskShape(original, sum, low)
	}
}

func luhnFake(sum []byte) string {
	d := make([]int, 16)
	for i := 0; i < 15; i++ {
		d[i] = (int(sum[i%len(sum)]) + i) % 10
	}
	// luhn check digit
	s := 0
	for i := 14; i >= 0; i-- {
		v := d[i]
		if (15-i)%2 == 1 {
			v *= 2
			if v > 9 {
				v -= 9
			}
		}
		s += v
	}
	d[15] = (10 - s%10) % 10
	var b strings.Builder
	for i, v := range d {
		if i == 4 || i == 8 || i == 12 {
			b.WriteByte(' ')
		}
		b.WriteByte(byte('0' + v))
	}
	return b.String()
}
