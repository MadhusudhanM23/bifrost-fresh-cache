package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"freshness-companion/internal/evidence"
	"freshness-companion/internal/extract"
)

const testPolicy = `{
  "version": 1,
  "defaults": {"max_age": "15m", "on_unverifiable": "bypass"},
  "categories": [
    {"name": "price", "keywords": ["price", "cost", "how much", "plan"], "max_age": "10m"},
    {"name": "policy", "keywords": ["refund*", "return policy"], "max_age": "24h", "on_unverifiable": "serve_with_warning"},
    {"name": "availability", "keywords": ["in stock", "available"], "max_age": "5m", "on_unknown": "bypass"}
  ],
  "facts": [
    {
      "name": "Pro price", "category": "price", "keywords": ["pro"],
      "source_url": "https://example.com/pricing",
      "source_pattern": "Pro plan[^$]{0,20}\\$([0-9.]+)",
      "answer_pattern": "\\$([0-9.]+)",
      "compare": "numeric"
    },
    {
      "name": "Refund window", "category": "policy",
      "source_url": "https://example.com/refunds",
      "source_pattern": "within ([0-9]+) days",
      "answer_pattern": "([0-9]+) days"
    }
  ]
}`

func mustParse(t *testing.T, s string) *Policy {
	t.Helper()
	p, err := Parse([]byte(s))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return p
}

func TestClassifierKeywords(t *testing.T) {
	p := mustParse(t, testPolicy)
	cases := []struct {
		text string
		want string // comma-joined category names, "" = not time-sensitive
	}{
		{"How much does the Pro plan cost?", "price"},
		{"WHAT IS THE PRICE", "price"},
		{"how   much\tis it", "price"}, // whitespace inside a phrase
		{"Is it in stock and how much?", "price,availability"},
		{"Can I get refunds?", "policy"}, // trailing * wildcard
		{"Where is the return policy?", "policy"},
		{"Tell me a joke about cats", ""},
		{"priceless advice please", ""}, // word boundary: price != priceless
		{"I love my planet", ""},        // plan != planet
		{"", ""},
	}
	for _, c := range cases {
		var names []string
		for _, cat := range p.Classify(c.text) {
			names = append(names, cat.Name)
		}
		if got := strings.Join(names, ","); got != c.want {
			t.Errorf("Classify(%q) = %q, want %q", c.text, got, c.want)
		}
	}
}

func TestResolveMergesCategoriesAndFacts(t *testing.T) {
	p := mustParse(t, testPolicy)

	eff, ok := p.Resolve("How much is the Pro plan?")
	if !ok || len(eff.Facts) != 1 || eff.Facts[0].Name != "Pro price" {
		t.Fatalf("Resolve(pro price) = %+v, %v", eff, ok)
	}
	if eff.MaxAge != 10*time.Minute || eff.OnUnverifiable != UnverifiableBypass || eff.OnUnknown != UnknownAuto {
		t.Fatalf("unexpected effective settings %+v", eff)
	}

	// The fact keyword "pro" must not match "product".
	eff, ok = p.Resolve("How much is the product?")
	if !ok || len(eff.Facts) != 0 {
		t.Fatalf("Resolve(product) should be time-sensitive without facts, got %+v, %v", eff, ok)
	}

	// Several categories: smallest max_age, strictest on_unverifiable and on_unknown.
	eff, ok = p.Resolve("Is the refund available?")
	if !ok || strings.Join(eff.Categories, ",") != "policy,availability" {
		t.Fatalf("categories = %v", eff.Categories)
	}
	if eff.MaxAge != 5*time.Minute || eff.OnUnverifiable != UnverifiableBypass || eff.OnUnknown != UnknownBypass {
		t.Fatalf("merged settings = %+v", eff)
	}
	if len(eff.Facts) != 1 || eff.Facts[0].Name != "Refund window" {
		t.Fatalf("facts = %+v", eff.Facts)
	}
	if eff.Facts[0].Compare != "exact" {
		t.Fatalf("default compare should be exact, got %q", eff.Facts[0].Compare)
	}

	if _, ok := p.Resolve("Tell me a joke"); ok {
		t.Fatal("a joke is not time-sensitive")
	}
}

func TestDefaultCategories(t *testing.T) {
	p := Default()
	var names []string
	for _, c := range p.Categories {
		names = append(names, c.Name)
	}
	if got := strings.Join(names, ","); got != "price,policy,schedule,availability" {
		t.Fatalf("default categories = %s", got)
	}
	if len(p.Facts) != 0 {
		t.Fatal("the built-in policy has no watched facts")
	}
	// A policy file without "categories" gets the defaults too.
	q := mustParse(t, `{"version": 1}`)
	if len(q.Categories) != 4 {
		t.Fatalf("got %d categories, want the 4 defaults", len(q.Categories))
	}
	eff, ok := q.Resolve("Is it in stock?")
	if !ok || eff.Categories[0] != "availability" || eff.MaxAge != DefaultMaxAge || eff.OnUnverifiable != UnverifiableBypass {
		t.Fatalf("defaults not applied: %+v", eff)
	}
}

func TestExamplePolicyFileLoads(t *testing.T) {
	p, err := Load(filepath.Join("..", "..", "examples", "policy.example.json"))
	if err != nil {
		t.Fatalf("examples/policy.example.json: %v", err)
	}
	eff, ok := p.Resolve("How much does the Acme Pro plan cost per month?")
	if !ok || len(eff.Facts) != 1 {
		t.Fatalf("the demo question should hit the Pro plan fact: %+v, %v", eff, ok)
	}
	f := eff.Facts[0]
	if !f.AnswerRegexp().MatchString("The Acme Pro plan costs $49 per month.") {
		t.Fatal("answer_pattern does not match a typical answer")
	}

	// The published demo page must state exactly one value for the pattern,
	// both as visible text (DirectFetch default, close to Tavily's text
	// format) and as raw HTML (-direct-raw).
	page, err := os.ReadFile(filepath.Join("..", "..", "demo-page", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	for mode, content := range map[string]string{"text": evidence.HTMLToText(string(page)), "raw": string(page)} {
		v, err := extract.Extract(f.SourceRegexp(), content, f.Compare)
		if err != nil || v.Number != 49 || v.Matches != 1 {
			t.Fatalf("demo page (%s): got %+v, %v; want exactly one value, 49", mode, v, err)
		}
	}
}

func TestBooleanDemoPolicyFileLoads(t *testing.T) {
	p, err := Load(filepath.Join("..", "..", "examples", "policy.boolean-demo.json"))
	if err != nil {
		t.Fatalf("examples/policy.boolean-demo.json: %v", err)
	}
	eff, ok := p.Resolve("Is live chat support currently available?")
	if !ok || len(eff.Facts) != 1 || eff.Facts[0].Name != "Live chat availability" {
		t.Fatalf("the demo question should hit the live chat fact: %+v, %v", eff, ok)
	}
	f := eff.Facts[0]
	if f.Compare != extract.Boolean {
		t.Fatalf("compare = %q, want boolean", f.Compare)
	}

	// Two differently-worded cached answers must extract to opposite
	// booleans against the answer_pattern.
	trueVal, err := extract.Extract(f.AnswerRegexp(), "Yes, live chat support is currently available.", f.Compare)
	if err != nil || trueVal.Normalized != "true" {
		t.Fatalf("available answer: got %+v, %v; want true", trueVal, err)
	}
	falseVal, err := extract.Extract(f.AnswerRegexp(), "No, live chat support is currently unavailable.", f.Compare)
	if err != nil || falseVal.Normalized != "false" {
		t.Fatalf("unavailable answer: got %+v, %v; want false", falseVal, err)
	}

	// The published demo page must state exactly one value for the pattern,
	// both as visible text (DirectFetch default) and as raw HTML
	// (-direct-raw), and it currently says "available".
	page, err := os.ReadFile(filepath.Join("..", "..", "demo-page", "availability.html"))
	if err != nil {
		t.Fatal(err)
	}
	for mode, content := range map[string]string{"text": evidence.HTMLToText(string(page)), "raw": string(page)} {
		v, err := extract.Extract(f.SourceRegexp(), content, f.Compare)
		if err != nil || v.Normalized != "true" || v.Matches != 1 {
			t.Fatalf("demo page (%s): got %+v, %v; want exactly one value, true", mode, v, err)
		}
	}
}

func TestValidationErrors(t *testing.T) {
	cases := map[string]string{
		"unknown field":         `{"version":1,"categories":[{"name":"price","keywords":["price"],"max_agee":"1m"}]}`,
		"no keywords":           `{"version":1,"categories":[{"name":"price","keywords":[]}]}`,
		"duplicate category":    `{"version":1,"categories":[{"name":"a","keywords":["x"]},{"name":"a","keywords":["y"]}]}`,
		"bad on_unverifiable":   `{"version":1,"categories":[{"name":"a","keywords":["x"],"on_unverifiable":"ignore"}]}`,
		"bad duration":          `{"version":1,"defaults":{"max_age":"ten minutes"}}`,
		"negative max_age":      `{"version":1,"defaults":{"max_age":"-1m"}}`,
		"fact unknown category": `{"version":1,"facts":[{"name":"f","category":"nope","source_url":"https://e.com","source_pattern":"(x)","answer_pattern":"(x)"}]}`,
		"fact two groups":       `{"version":1,"facts":[{"name":"f","category":"price","source_url":"https://e.com","source_pattern":"(a)(b)","answer_pattern":"(x)"}]}`,
		"fact no group":         `{"version":1,"facts":[{"name":"f","category":"price","source_url":"https://e.com","source_pattern":"(x)","answer_pattern":"x"}]}`,
		"fact relative url":     `{"version":1,"facts":[{"name":"f","category":"price","source_url":"/pricing","source_pattern":"(x)","answer_pattern":"(x)"}]}`,
		"fact url credentials":  `{"version":1,"facts":[{"name":"f","category":"price","source_url":"https://u:p@e.com","source_pattern":"(x)","answer_pattern":"(x)"}]}`,
		"fact bad compare":      `{"version":1,"facts":[{"name":"f","category":"price","source_url":"https://e.com","source_pattern":"(x)","answer_pattern":"(x)","compare":"fuzzy"}]}`,
		"trailing data":         `{"version":1} {"version":1}`,
		"version":               `{"version":2}`,
	}
	for name, doc := range cases {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: want a validation error", name)
		}
	}
}
