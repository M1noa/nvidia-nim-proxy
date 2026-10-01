package pii

import "sort"

// label set: rampart taxonomy + extras for keys/secrets.
type Label string

const (
	LSSN              Label = "SSN"
	LCreditCard       Label = "CREDIT_CARD"
	LIPAddress        Label = "IP_ADDRESS"
	LGivenName        Label = "GIVEN_NAME"
	LSurname          Label = "SURNAME"
	LEmail            Label = "EMAIL"
	LPhone            Label = "PHONE"
	LURL              Label = "URL"
	LTaxID            Label = "TAX_ID"
	LBankAccount      Label = "BANK_ACCOUNT"
	LRoutingNumber    Label = "ROUTING_NUMBER"
	LGovernmentID     Label = "GOVERNMENT_ID"
	LPassport         Label = "PASSPORT"
	LDriversLicense   Label = "DRIVERS_LICENSE"
	LBuildingNumber   Label = "BUILDING_NUMBER"
	LStreetName       Label = "STREET_NAME"
	LSecondaryAddress Label = "SECONDARY_ADDRESS"
	LCity             Label = "CITY"
	LState            Label = "STATE"
	LZipCode          Label = "ZIP_CODE"
	LAPIKey           Label = "API_KEY"
	LPrivateKey       Label = "PRIVATE_KEY"
	LJWT              Label = "JWT"
	LIBAN             Label = "IBAN"
	LCustom           Label = "CUSTOM"
)

type Span struct {
	Start  int
	End    int
	Label  Label
	Score  float64
	Source string // heuristic | ner | custom | secret
	Text   string
}

var keepDefault = map[Label]bool{
	LCity: true, LState: true, LZipCode: true,
}

func mergeSpans(spans []Span) []Span {
	if len(spans) == 0 {
		return nil
	}
	sorted := append([]Span(nil), spans...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Start != sorted[j].Start {
			return sorted[i].Start < sorted[j].Start
		}
		return sorted[i].End > sorted[j].End
	})
	var out []Span
	for _, s := range sorted {
		if len(out) == 0 {
			out = append(out, s)
			continue
		}
		p := &out[len(out)-1]
		if s.Start >= p.End {
			out = append(out, s)
			continue
		}
		w := p
		if prefer(s, *p) {
			*w = s
			w = p
		}
		prevContains := p.Start <= s.Start && p.End >= s.End
		spanContains := s.Start <= p.Start && s.End >= p.End
		if prevContains || spanContains {
			// winner already set
		} else {
			st := p.Start
			if s.Start < st {
				st = s.Start
			}
			en := p.End
			if s.End > en {
				en = s.End
			}
			w.Start, w.End = st, en
		}
	}
	return out
}

func prefer(a, b Span) bool {
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	// entities win over flat terms on overlap (documented precedence).
	ae, be := a.Source == "custom-entity", b.Source == "custom-entity"
	if ae != be {
		return ae
	}
	la, lb := a.End-a.Start, b.End-b.Start
	if la != lb {
		return la > lb
	}
	return a.Source == "heuristic"
}

func applyPolicy(spans []Span, keep map[Label]bool) []Span {
	if keep == nil {
		keep = keepDefault
	}
	m := mergeSpans(spans)
	var out []Span
	for _, s := range m {
		if !keep[s.Label] {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start > out[j].Start })
	return out
}
