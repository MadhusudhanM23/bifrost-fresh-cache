package sensitive

import (
	"strings"
	"testing"
	"time"
)

func TestDetectors(t *testing.T) {
	cases := []struct {
		name, text, want string
	}{
		// true positives
		{"email plain", "write to jane.doe+news@example.co.uk please", Email},
		{"email end of sentence", "my mail is bob@corp.io.", Email},
		{"card spaced", "pay with 4111 1111 1111 1111 today", Card},
		{"card dashed", "card: 5500-0000-0000-0004", Card},
		{"card plain 16", "4111111111111111", Card},
		{"card 15 digit amex", "378282246310005", Card},
		{"card after a date", "on 2024-01-15 4111 1111 1111 1111", Card},
		{"openai style key", "key sk-proj-AbCdEfGhIjKlMnOpQrStUv123456", APIKey},
		{"aws key", "AKIAIOSFODNN7EXAMPLE is mine", APIKey},
		{"github token", "ghp_" + strings.Repeat("a1B2", 9), APIKey},
		{"slack token", "xoxb-1234567890-abcdefghij", APIKey},
		{"jwt", "token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r", JWT},
		{"bearer", "Authorization: Bearer abcdEF1234567890abcdEF1234567890", BearerToken},
		{"ssn", "my ssn is 123-45-6789", SSN},
		{"iban de", "send to DE89 3704 0044 0532 0130 00 now", IBAN},
		{"iban gb compact", "GB82WEST12345698765432", IBAN},
		{"phone e164", "call +14155552671", Phone},
		{"phone spaced", "call +44 20 7946 0958 tomorrow", Phone},
		{"phone us parens", "call (415) 555-2671", Phone},
		{"phone us dashes", "call 415-555-2671", Phone},
	}
	for _, c := range cases {
		if got := Find(c.text, nil); got != c.want {
			t.Errorf("%s: Find = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestDetectorsIgnoreLookAlikes(t *testing.T) {
	negatives := []string{
		"How much does the Acme Pro plan cost per month?",
		"The Pro plan costs $49 per month, or $490 per year.",
		"Follow @acme on social media",                  // a mention, not an email
		"meet me @ 5pm at the cafe",                     // lone @
		"user@localhost is not an address",              // no dot in the domain
		"a@b.c",                                         // one-letter ending
		"order number 1234567890123456",                 // 16 digits, fails Luhn
		"tracking 4111 1111 1111 1112",                  // Luhn off by one
		"id 0000000000000000",                           // repeated digits pass Luhn but are not a card
		"timestamp 1700000000000 ms",                    // 13 digits, fails Luhn
		"released 2024-01-15 and 2025-12-31",            // dates
		"phone-like 555-12-345",                         // too short
		"ssn-looking 000-12-3456",                       // never issued
		"ssn-looking 666-12-3456",                       // never issued
		"ssn-looking 900-12-3456",                       // never issued
		"part 1-123-45-6789-0",                          // inside a longer dashed number
		"sk-learn is a python library",                  // too short to be a key
		"ask-me-anything-about-our-super-long-faq-page", // "sk-" only inside a word
		"the bearer of responsibilities arrives",        // no digit, not a token
		"Bearer",                                        // nothing after it
		"eyJ is how base64 JSON begins",                 // no dots
		"AB12 CDEF GHIJ KLMN OPQR",                      // IBAN-shaped, fails mod 97
		"DE89 3704 0044 0532 0130 02",                   // one digit changed
		"call me on 0412345678",                         // bare digits are not flagged
		"+$49 extra per seat",                           // plus before a price
		"temperature rose +5 degrees",                   // one digit after +
		"version 2.0.1-123-4567 build",                  // dashed digits inside a version
		"call 115-555-2671",                             // area code starting with 1
		"between 10-20 and 30-40",                       // ranges
		"",
	}
	for _, text := range negatives {
		if got := Find(text, nil); got != "" {
			t.Errorf("%q flagged as %q", text, got)
		}
	}
}

func TestKindSetNarrowsTheSearch(t *testing.T) {
	text := "write to bob@corp.io or call +14155552671"
	if got := Find(text, NewSet([]string{Phone})); got != Phone {
		t.Fatalf("got %q, want phone only", got)
	}
	if got := Find(text, NewSet([]string{Card})); got != "" {
		t.Fatalf("got %q, want nothing", got)
	}
	if got := Find(text, nil); got != Email {
		t.Fatalf("all kinds: got %q, want email first", got)
	}
}

func TestFindAnyAndValid(t *testing.T) {
	if got := FindAny([]string{"hello", "my ssn 123-45-6789"}, nil); got != SSN {
		t.Fatalf("got %q", got)
	}
	for _, k := range AllKinds {
		if !Valid(k) {
			t.Fatalf("%q must be valid", k)
		}
	}
	if Valid("passport") {
		t.Fatal("unknown kind accepted")
	}
}

func TestLuhnAndMod97(t *testing.T) {
	if !luhn("4111111111111111") || luhn("4111111111111112") {
		t.Fatal("luhn wrong")
	}
	if !ibanMod97("GB82WEST12345698765432") || ibanMod97("GB83WEST12345698765432") {
		t.Fatal("mod 97 wrong")
	}
}

// bigBody builds roughly n bytes of ordinary text with numbers, dashes and
// at-signs sprinkled in, none of which is sensitive.
func bigBody(n int) string {
	unit := "The Acme Pro plan costs $49 per month (order 2024-01-15, ref 12-34, @team, 3 of 10). " +
		"Our sk-learn notes: version 1.2.3-45-6789, call-me-maybe, Bearer of news, 1234567 units. "
	return strings.Repeat(unit, n/len(unit)+1)[:n]
}

func TestHundredKilobyteBodyIsFast(t *testing.T) {
	body := bigBody(100 << 10)
	start := time.Now()
	if got := Find(body, nil); got != "" {
		t.Fatalf("clean body flagged as %q", got)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("scanning 100 KB took %v", d)
	}
}

func BenchmarkFind100KBClean(b *testing.B) {
	body := bigBody(100 << 10)
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if Find(body, nil) != "" {
			b.Fatal("flagged")
		}
	}
}

// Worst case for the digit scanner: long runs of digit groups.
func BenchmarkFind100KBDigitHeavy(b *testing.B) {
	body := strings.Repeat("1234 5678 9012 3456 7 ", (100<<10)/22)
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Find(body, nil)
	}
}
