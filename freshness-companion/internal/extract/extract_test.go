package extract

import (
	"errors"
	"regexp"
	"testing"
)

// The patterns used by the demo policy (examples/policy.example.json).
var (
	demoSource = regexp.MustCompile(`Acme Pro plan[^$0-9]{0,20}\$\s*([0-9][0-9,]*(?:\.[0-9]+)?)\s*per month`)
	demoAnswer = regexp.MustCompile(`(?i)\$\s*([0-9][0-9,]*(?:\.[0-9]+)?)\s*(?:/\s*mo(?:nth)?|per\s+month|a\s+month|monthly)\b`)
)

func mustExtract(t *testing.T, re *regexp.Regexp, text string, mode Mode) Value {
	t.Helper()
	v, err := Extract(re, text, mode)
	if err != nil {
		t.Fatalf("Extract(%q): %v", text, err)
	}
	return v
}

func TestNumericCompareDollarPrices(t *testing.T) {
	cached := mustExtract(t, demoAnswer, "The Acme Pro plan costs $49 per month.", Numeric)
	current := mustExtract(t, demoSource, "Acme pricing Pro plan Acme Pro plan — $59 per month Last updated", Numeric)
	if cached.Raw != "49" || current.Raw != "59" {
		t.Fatalf("raw values = %q, %q; want 49, 59", cached.Raw, current.Raw)
	}
	if Equal(cached, current, Numeric) {
		t.Fatal(`"$49" and "$59" compared equal`)
	}
	same := mustExtract(t, demoSource, "Acme Pro plan — $49 per month", Numeric)
	if !Equal(cached, same, Numeric) {
		t.Fatal(`"$49" and "$49" compared different`)
	}
}

func TestMatchIsTheWholeSourceSentence(t *testing.T) {
	v := mustExtract(t, demoSource, "Pricing\nAcme Pro plan — $49 per month\nLast updated Monday", Numeric)
	if v.Raw != "49" || v.Match != "Acme Pro plan — $49 per month" {
		t.Fatalf("Raw = %q, Match = %q", v.Raw, v.Match)
	}
	long := mustExtract(t, regexp.MustCompile(`(x)\s*[a-z ]+`), "x "+string(make([]byte, 0))+"abcdefghij abcdefghij abcdefghij abcdefghij abcdefghij abcdefghij abcdefghij abcdefghij abcdefghij abcdefghij abcdefghij abcdefghij abcdefghij abcdefghij abcdefghij abcdefghij abcdefghij abcdefghij abcdefghij abcdefghij abcdefghij abcdefghij", Exact)
	if len(long.Match) > 210 {
		t.Fatalf("Match must be bounded, got %d bytes", len(long.Match))
	}
}

func TestNumericEquivalentForms(t *testing.T) {
	cases := []struct{ a, b string }{
		{"49.00", "49"},
		{"$49", "49"},
		{"1,299", "1299"},
		{"$1,299.00", "1299"},
		{"1.299,50", "1299.5"},
		{"49,00", "49"},
		{"1.234.567", "1234567"},
		{"USD 49", "49.0"},
		{"-5", "-5.00"},
	}
	for _, c := range cases {
		a, errA := ParseNumber(c.a)
		b, errB := ParseNumber(c.b)
		if errA != nil || errB != nil {
			t.Fatalf("ParseNumber(%q, %q): %v, %v", c.a, c.b, errA, errB)
		}
		if !Equal(Value{Number: a}, Value{Number: b}, Numeric) {
			t.Errorf("%q (%v) and %q (%v) should be equal", c.a, a, c.b, b)
		}
	}
	if _, err := ParseNumber("no digits here"); err == nil {
		t.Error("ParseNumber without digits: want an error")
	}
}

func TestDemoAnswerPatternForms(t *testing.T) {
	for _, text := range []string{
		"The Acme Pro plan costs $49 per month.",
		"It is $49/month.",
		"Pro is $49/mo, billed monthly.",
		"You pay $49 a month.",
		"$49.00 monthly",
	} {
		v, err := Extract(demoAnswer, text, Numeric)
		if err != nil || v.Number != 49 {
			t.Errorf("%q: got %+v, %v; want 49", text, v, err)
		}
	}
	if _, err := Extract(demoAnswer, "It costs $490 per year.", Numeric); !errors.Is(err, ErrNoMatch) {
		t.Errorf("yearly price: got %v, want ErrNoMatch", err)
	}
}

func TestExactCompare(t *testing.T) {
	re := regexp.MustCompile(`(?i)open\s+on\s+(\w+)`)
	a := mustExtract(t, re, "We are open on Monday.", Exact)
	b := mustExtract(t, re, "WE ARE OPEN ON monday", Exact)
	c := mustExtract(t, re, "open on Tuesday", Exact)
	if !Equal(a, b, Exact) {
		t.Error("exact compare should ignore case")
	}
	if Equal(a, c, Exact) {
		t.Error("Monday and Tuesday compared equal")
	}
	ws := regexp.MustCompile(`hours:\s*([^.]+)\.`)
	x := mustExtract(t, ws, "hours: 9am  -   5pm.", Exact)
	y := mustExtract(t, ws, "hours: 9AM - 5PM.", Exact)
	if !Equal(x, y, Exact) {
		t.Errorf("exact compare should collapse whitespace: %q vs %q", x.Normalized, y.Normalized)
	}
}

func TestNoMatchAndConflict(t *testing.T) {
	if _, err := Extract(demoSource, "this page has no price", Numeric); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("got %v, want ErrNoMatch", err)
	}
	_, err := Extract(demoSource, "Acme Pro plan — $49 per month ... Acme Pro plan — $59 per month", Numeric)
	var conflict *ConflictError
	if !errors.As(err, &conflict) || len(conflict.Values) != 2 {
		t.Fatalf("got %v, want a conflict between 49 and 59", err)
	}
	// Repeating the same value (in any notation) is not a conflict.
	v, err := Extract(demoSource, "Acme Pro plan — $49 per month ... Acme Pro plan — $49.00 per month", Numeric)
	if err != nil || v.Matches != 2 {
		t.Fatalf("repeated value: got %+v, %v", v, err)
	}
}

func TestCapturedValueMustParse(t *testing.T) {
	re := regexp.MustCompile(`price: (\S+)`)
	if _, err := Extract(re, "price: free", Numeric); err == nil {
		t.Fatal("non-numeric capture in numeric mode: want an error")
	}
}

// The patterns used by the boolean demo policy (examples/policy.boolean-demo.json).
var chatAvailability = regexp.MustCompile(`(?i)(live chat[^.!?]*)`)

func TestBooleanCompareAvailability(t *testing.T) {
	cached := mustExtract(t, chatAvailability, "Yes, live chat support is currently available.", Boolean)
	current := mustExtract(t, chatAvailability, "Live chat support is currently unavailable.", Boolean)
	if cached.Normalized != "true" || current.Normalized != "false" {
		t.Fatalf("normalized = %q, %q; want true, false", cached.Normalized, current.Normalized)
	}
	if Equal(cached, current, Boolean) {
		t.Fatal("available and unavailable compared equal")
	}
	same := mustExtract(t, chatAvailability, "Live chat is available right now.", Boolean)
	if !Equal(cached, same, Boolean) {
		t.Fatal("two differently-worded true answers compared different")
	}
}

func TestBooleanSignalWords(t *testing.T) {
	trueCases := []string{
		"Yes, live chat is available.",
		"live chat support is online",
		"Live chat is enabled for Pro customers.",
		"True, live chat support is active right now.",
		"live chat is in stock", // nonsensical but exercises the "in stock" signal
	}
	for _, text := range trueCases {
		v, err := Extract(chatAvailability, text, Boolean)
		if err != nil || v.Normalized != "true" {
			t.Errorf("%q: got %+v, %v; want true", text, v, err)
		}
	}
	falseCases := []string{
		"No, live chat is not available right now.",
		"live chat support is currently unavailable",
		"Live chat is no longer available.",
		"live chat support is disabled for this plan",
		"live chat is offline at the moment",
		"live chat support is closed for the night",
	}
	for _, text := range falseCases {
		v, err := Extract(chatAvailability, text, Boolean)
		if err != nil || v.Normalized != "false" {
			t.Errorf("%q: got %+v, %v; want false", text, v, err)
		}
	}
}

func TestBooleanNegationBeatsThePositiveWordItContains(t *testing.T) {
	// "unavailable" contains "available"; "not available" and "no longer
	// supported" contain "available"/"supported". False must win every time.
	for _, raw := range []string{
		"unavailable", "not available", "no longer available",
		"unsupported", "not supported", "no longer supported",
	} {
		norm, num, err := ParseBoolean(raw)
		if err != nil || norm != "false" || num != 0 {
			t.Errorf("ParseBoolean(%q) = %q, %v, %v; want false, 0, nil", raw, norm, num, err)
		}
	}
}

func TestBooleanNoSignal(t *testing.T) {
	re := regexp.MustCompile(`status: (\w+)`)
	if _, err := Extract(re, "status: pending", Boolean); !errors.Is(err, ErrNoSignal) {
		t.Fatalf("got %v, want ErrNoSignal", err)
	}
}

func TestCompilePatternRequiresOneGroup(t *testing.T) {
	if _, err := CompilePattern(`\$([0-9]+)`); err != nil {
		t.Fatalf("one group: %v", err)
	}
	for _, bad := range []string{``, `\$[0-9]+`, `(\$)([0-9]+)`, `([`} {
		if _, err := CompilePattern(bad); err == nil {
			t.Errorf("CompilePattern(%q): want an error", bad)
		}
	}
	if _, err := CompilePattern(`(?:USD|\$)\s*([0-9]+)`); err != nil {
		t.Errorf("non-capturing groups should be allowed: %v", err)
	}
}

func TestNormalizeMatchesExtract(t *testing.T) {
	fromText := mustExtract(t, demoSource, "Acme Pro plan — $1,299.00 per month", Numeric)
	fromValue, err := Normalize(" 1299 ", Numeric)
	if err != nil {
		t.Fatal(err)
	}
	if !Equal(fromText, fromValue, Numeric) {
		t.Fatalf("Normalize(%q) = %+v, does not equal the extracted %+v", "1299", fromValue, fromText)
	}
	if v, err := Normalize("Monday  9AM", Exact); err != nil || v.Normalized != "monday 9am" {
		t.Fatalf("exact: got %+v, %v", v, err)
	}
	if v, err := Normalize("currently unavailable", Boolean); err != nil || v.Normalized != "false" {
		t.Fatalf("boolean: got %+v, %v", v, err)
	}
	if _, err := Normalize("", Numeric); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("empty: got %v, want ErrNoMatch", err)
	}
	if _, err := Normalize("free", Numeric); err == nil {
		t.Fatal("a non-number must not normalize as numeric")
	}
	if _, err := Normalize("49", Mode("fuzzy")); err == nil {
		t.Fatal("an unknown mode must be rejected")
	}
}
