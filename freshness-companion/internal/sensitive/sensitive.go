// Package sensitive finds text that must never be written to a shared cache:
// email addresses, payment card numbers, API keys, social security numbers,
// IBANs and phone numbers. It returns only the NAME of the kind it found,
// never the matched text, so nothing built on it can leak the text.
//
// Everything is standard library. The detectors favour few false alarms over
// catching every format: a real number written in an odd way can be missed.
package sensitive

import (
	"regexp"
	"strings"
)

// Kinds. These names are the only thing that ever leaves this package.
const (
	Email       = "email"
	Card        = "card"
	APIKey      = "api_key"
	JWT         = "jwt"
	BearerToken = "bearer_token"
	SSN         = "ssn"
	IBAN        = "iban"
	Phone       = "phone"
)

// AllKinds lists every kind, in check order.
var AllKinds = []string{Email, Card, APIKey, JWT, BearerToken, SSN, IBAN, Phone}

// Valid reports whether name is a known kind.
func Valid(name string) bool {
	for _, k := range AllKinds {
		if k == name {
			return true
		}
	}
	return false
}

// Set is a chosen group of kinds. A nil or empty Set means all kinds.
type Set map[string]bool

// NewSet returns a Set of kinds (all kinds when none are given).
func NewSet(kinds []string) Set {
	if len(kinds) == 0 {
		return nil
	}
	s := Set{}
	for _, k := range kinds {
		s[k] = true
	}
	return s
}

func (s Set) has(kind string) bool { return len(s) == 0 || s[kind] }

var (
	apiKeyRE = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{20,}|AKIA[0-9A-Z]{16}|gh[pousr]_[A-Za-z0-9]{30,}|xox[baprs]-[A-Za-z0-9-]{10,})`)
	jwtRE    = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}`)
	bearerRE = regexp.MustCompile(`(?i)\bbearer\s+([A-Za-z0-9._~+/-]{20,})=*`)
	ssnRE    = regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)
	ibanRE   = regexp.MustCompile(`\b[A-Z]{2}\d{2}(?: ?[A-Z0-9]{4}){2,7}(?: ?[A-Z0-9]{1,3})?\b`)
	// Phones: an international number must start with "+"; the US shapes
	// (555) 123-4567 and 555-123-4567 are also caught. Bare digit strings are
	// not, because they look like order numbers, prices and dates.
	phoneIntlRE = regexp.MustCompile(`\+[1-9](?:[ .-]?\(?\d{1,4}\)?){2,6}`)
	phoneUSRE   = regexp.MustCompile(`(?:\(\d{3}\)\s?|\b\d{3}-)\d{3}-\d{4}\b`)
	digitRunRE  = regexp.MustCompile(`\d+(?:[ -]\d+)*`)
)

// Find returns the first kind found in text ("" = none). Only kinds in set
// are looked for.
func Find(text string, set Set) string {
	if text == "" {
		return ""
	}
	type detector struct {
		kind string
		fn   func(string) bool
	}
	for _, d := range []detector{
		{Email, hasEmail}, {Card, hasCard}, {APIKey, hasAPIKey}, {JWT, hasJWT},
		{BearerToken, hasBearer}, {SSN, hasSSN}, {IBAN, hasIBAN}, {Phone, hasPhone},
	} {
		if set.has(d.kind) && d.fn(text) {
			return d.kind
		}
	}
	return ""
}

// FindAny returns the first kind found in any of the texts.
func FindAny(texts []string, set Set) string {
	for _, t := range texts {
		if k := Find(t, set); k != "" {
			return k
		}
	}
	return ""
}

// ---- email ----

func isLocal(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("._%+-", c) >= 0
}

func isDomain(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.'
}

// hasEmail looks around each "@": a local part before it and a domain with a
// dot and a letters-only ending after it. "@mention" and "meet @ 5pm" do not
// count.
func hasEmail(s string) bool {
	for from := 0; ; {
		i := strings.IndexByte(s[from:], '@')
		if i < 0 {
			return false
		}
		i += from
		from = i + 1
		if i == 0 || !isLocal(s[i-1]) {
			continue
		}
		j := i + 1
		for j < len(s) && isDomain(s[j]) {
			j++
		}
		domain := strings.TrimRight(s[i+1:j], ".-")
		dot := strings.LastIndexByte(domain, '.')
		if dot <= 0 || strings.Contains(domain, "..") || domain[0] == '-' {
			continue
		}
		tld := domain[dot+1:]
		if len(tld) >= 2 && isAlpha(tld) {
			return true
		}
	}
}

func isAlpha(s string) bool {
	for i := 0; i < len(s); i++ {
		if !(s[i] >= 'a' && s[i] <= 'z' || s[i] >= 'A' && s[i] <= 'Z') {
			return false
		}
	}
	return true
}

// ---- payment cards ----

func hasDigit(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			return true
		}
	}
	return false
}

// hasCard finds 13-19 digits, written together or in groups split by single
// spaces or dashes, that pass the Luhn check. Groups are tried in every
// contiguous combination, so "order 2024-01-15 4111 1111 1111 1111" is still
// found. Repeated digits (0000...) are ignored.
func hasCard(s string) bool {
	if !hasDigit(s) {
		return false
	}
	for _, run := range digitRunRE.FindAllString(s, -1) {
		if len(run) < 13 {
			continue
		}
		groups := strings.FieldsFunc(run, func(r rune) bool { return r == ' ' || r == '-' })
		for i := range groups {
			digits := ""
			for j := i; j < len(groups); j++ {
				digits += groups[j]
				if len(digits) > 19 {
					break
				}
				if len(digits) >= 13 && luhn(digits) && !allSame(digits) {
					return true
				}
			}
		}
	}
	return false
}

func allSame(d string) bool {
	for i := 1; i < len(d); i++ {
		if d[i] != d[0] {
			return false
		}
	}
	return true
}

func luhn(d string) bool {
	sum, double := 0, false
	for i := len(d) - 1; i >= 0; i-- {
		n := int(d[i] - '0')
		if double {
			if n *= 2; n > 9 {
				n -= 9
			}
		}
		sum += n
		double = !double
	}
	return sum%10 == 0
}

// ---- keys and tokens ----

func hasAPIKey(s string) bool {
	if !strings.ContainsAny(s, "-_") && !strings.Contains(s, "AKIA") {
		return false
	}
	return apiKeyRE.MatchString(s)
}

func hasJWT(s string) bool {
	return strings.Contains(s, "eyJ") && jwtRE.MatchString(s)
}

// hasBearer needs "Bearer " followed by 20+ token characters that include a
// digit, so the sentence "bearer of responsibilities" is not flagged.
func hasBearer(s string) bool {
	if len(s) < 27 || !strings.Contains(strings.ToLower(s), "bearer") {
		return false
	}
	for _, m := range bearerRE.FindAllStringSubmatch(s, -1) {
		if hasDigit(m[1]) {
			return true
		}
	}
	return false
}

// ---- social security numbers ----

func hasSSN(s string) bool {
	if !strings.Contains(s, "-") {
		return false
	}
	for _, loc := range ssnRE.FindAllStringIndex(s, -1) {
		a, b := loc[0], loc[1]
		if a > 0 && s[a-1] == '-' || b < len(s) && s[b] == '-' {
			continue // part of a longer dashed number
		}
		m := s[a:b]
		area := m[:3]
		if area == "000" || area == "666" || area[0] == '9' || m[4:6] == "00" || m[7:] == "0000" {
			continue // numbers the SSA never issues
		}
		return true
	}
	return false
}

// ---- IBAN ----

func hasIBAN(s string) bool {
	for _, m := range ibanRE.FindAllString(s, -1) {
		iban := strings.ReplaceAll(m, " ", "")
		if len(iban) >= 15 && len(iban) <= 34 && ibanMod97(iban) {
			return true
		}
	}
	return false
}

func ibanMod97(iban string) bool {
	rearranged := iban[4:] + iban[:4]
	rem := 0
	for i := 0; i < len(rearranged); i++ {
		c := rearranged[i]
		switch {
		case c >= '0' && c <= '9':
			rem = (rem*10 + int(c-'0')) % 97
		case c >= 'A' && c <= 'Z':
			rem = (rem*100 + int(c-'A') + 10) % 97
		default:
			return false
		}
	}
	return rem == 1
}

// ---- phone numbers ----

func hasPhone(s string) bool {
	if !strings.ContainsAny(s, "+-(") {
		return false
	}
	for _, loc := range phoneIntlRE.FindAllStringIndex(s, -1) {
		if loc[0] > 0 && isWord(s[loc[0]-1]) {
			continue // "a+1 2 3": not a number written with a plus
		}
		n := 0
		for i := loc[0]; i < loc[1]; i++ {
			if s[i] >= '0' && s[i] <= '9' {
				n++
			}
		}
		if n >= 8 && n <= 15 {
			return true
		}
	}
	for _, loc := range phoneUSRE.FindAllStringIndex(s, -1) {
		a, b := loc[0], loc[1]
		if a > 0 && (isWord(s[a-1]) || s[a-1] == '-') || b < len(s) && s[b] == '-' {
			continue
		}
		d := strings.Trim(s[a:b], "( ")
		if d[0] >= '2' && d[0] <= '9' { // US area codes never start with 0 or 1
			return true
		}
	}
	return false
}

func isWord(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}
