package policy

import (
	"fmt"
	"strings"
	"testing"

	"freshness-companion/internal/extract"
)

func discoverPolicy(block string) string {
	return fmt.Sprintf(`{
  "version": 1,
  "categories": [
    {"name": "price", "keywords": ["price", "cost", "how much"], "discover": %s},
    {"name": "policy", "keywords": ["refund"]}
  ],
  "facts": [{"name": "Refund window", "category": "policy", "source_url": "https://example.com/refunds",
    "source_pattern": "within ([0-9]+) days", "answer_pattern": "([0-9]+) days"}]
}`, block)
}

const goodDiscover = `{"enabled": true, "entities": ["Acme Pro", "Acme Team"],
  "query": "{entities} plan price per month", "include_domains": ["Example.github.io"],
  "require_domains": true, "compare": "numeric", "near_chars": 40, "min_score": 0.5}`

func TestDiscoverBlockParses(t *testing.T) {
	p := mustParse(t, discoverPolicy(goodDiscover))
	d := p.Categories[0].Discover
	if !d.Active() {
		t.Fatal("discover block should be active")
	}
	if d.IncludeDomains[0] != "example.github.io" {
		t.Fatalf("include_domains not normalised: %v", d.IncludeDomains)
	}
	if d.ValuePatternSource() != DefaultNumericValuePattern || d.Mode() != extract.Numeric {
		t.Fatalf("defaults: pattern %q mode %q", d.ValuePatternSource(), d.Mode())
	}
	if d.Near() != 40 || d.Score() != 0.5 || d.Sources() != 1 {
		t.Fatalf("near %d score %v sources %d", d.Near(), d.Score(), d.Sources())
	}
	if !strings.Contains(strings.Join(d.Exclusions(), ","), "reddit.com") {
		t.Fatal("default exclusions missing")
	}
	// require_domains without include_domains: discovery is off, not an error.
	p = mustParse(t, discoverPolicy(`{"enabled": true, "entities": ["Acme Pro"], "query": "{entities} price", "require_domains": true}`))
	if p.Categories[0].Discover.Active() {
		t.Fatal("require_domains with no include_domains must turn discovery off")
	}
	// enabled false: parsed, inactive.
	p = mustParse(t, discoverPolicy(`{"enabled": false, "entities": ["Acme Pro"], "query": "{entities} price"}`))
	if p.Categories[0].Discover.Active() {
		t.Fatal("enabled false must be inactive")
	}
}

func TestDiscoverTemplateValidation(t *testing.T) {
	cases := map[string]string{
		"no placeholder":    `{"enabled": true, "entities": ["Acme"], "query": "acme price"}`,
		"other placeholder": `{"enabled": true, "entities": ["Acme"], "query": "{entities} {question}"}`,
		"no entities":       `{"enabled": true, "query": "{entities} price"}`,
		"bad query_from":    `{"enabled": true, "query_from": "prompt", "query": "{entities} price"}`,
		"bad compare":       `{"enabled": true, "entities": ["Acme"], "query": "{entities}", "compare": "fuzzy"}`,
		"exact no pattern":  `{"enabled": true, "entities": ["Acme"], "query": "{entities}", "compare": "exact"}`,
		"domain with path":  `{"enabled": true, "entities": ["Acme"], "query": "{entities}", "include_domains": ["https://acme.com/x"]}`,
		"ip domain":         `{"enabled": true, "entities": ["Acme"], "query": "{entities}", "include_domains": ["10.0.0.1"]}`,
		"http candidate":    `{"enabled": true, "entities": ["Acme"], "query": "{entities}", "candidate_urls": ["http://acme.com/p"]}`,
		"near too big":      `{"enabled": true, "entities": ["Acme"], "query": "{entities}", "near_chars": 500}`,
		"score above one":   `{"enabled": true, "entities": ["Acme"], "query": "{entities}", "min_score": 2}`,
		"unknown field":     `{"enabled": true, "entities": ["Acme"], "query": "{entities}", "promt": "x"}`,
	}
	for name, block := range cases {
		if _, err := Parse([]byte(discoverPolicy(block))); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	// proper_nouns needs no entities.
	mustParse(t, discoverPolicy(`{"enabled": true, "query_from": "proper_nouns", "query": "{entities} price"}`))
}

func TestDiscoverValuePatternOneGroup(t *testing.T) {
	for _, pat := range []string{`\$[0-9]+`, `(\$)([0-9]+)`, `(`} {
		block := fmt.Sprintf(`{"enabled": true, "entities": ["Acme"], "query": "{entities}", "value_pattern": %q}`, pat)
		if _, err := Parse([]byte(discoverPolicy(block))); err == nil {
			t.Errorf("value_pattern %q should be rejected", pat)
		}
	}
	p := mustParse(t, discoverPolicy(`{"enabled": true, "entities": ["Acme"], "query": "{entities}", "compare": "exact", "value_pattern": "opens at ([0-9]+(?:am|pm))"}`))
	if p.Categories[0].Discover.ValueRegexp().NumSubexp() != 1 {
		t.Fatal("one capture group expected")
	}
}

func TestResolveCarriesDiscover(t *testing.T) {
	p := mustParse(t, discoverPolicy(goodDiscover))
	eff, ok := p.Resolve("How much does the Acme Pro plan cost?")
	if !ok || eff.Discover == nil || eff.DiscoverCategory != "price" {
		t.Fatalf("Resolve should carry the discover block: %+v", eff)
	}
	if len(eff.Facts) != 0 {
		t.Fatalf("no facts expected, got %v", eff.FactNames())
	}
	// A category without discover gives none.
	eff, _ = p.Resolve("What is the refund window?")
	if eff.Discover != nil {
		t.Fatal("policy category has no discover block")
	}
}

func TestDiscoverAbsentKeepsResolveUnchanged(t *testing.T) {
	p := mustParse(t, testPolicy)
	eff, ok := p.Resolve("How much is the Pro plan?")
	if !ok || eff.Discover != nil || eff.DiscoverCategory != "" {
		t.Fatalf("no discover block: %+v", eff)
	}
	if got := strings.Join(eff.FactNames(), ","); got != "Pro price" {
		t.Fatalf("facts = %q", got)
	}
}

func TestNewDiscoveredFactIsNeverInThePolicy(t *testing.T) {
	p := mustParse(t, discoverPolicy(goodDiscover))
	d := p.Categories[0].Discover
	f := NewDiscoveredFact("discovered: x", "price", "https://acme.example/p", d.ValueRegexp(), d.ValueRegexp(), d.Mode())
	if !f.Discovered || f.SourceRegexp() == nil || f.AnswerRegexp() == nil {
		t.Fatalf("bad fact %+v", f)
	}
	if p.Fact("discovered: x") != nil || len(p.Facts) != 1 {
		t.Fatal("a discovered fact must never be added to the policy")
	}
}
