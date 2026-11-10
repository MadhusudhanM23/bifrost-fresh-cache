package policy

import (
	"strings"
	"testing"
)

const contradictionPolicy = `{
  "version": 1,
  "defaults": {"verify_new_answers": false, "on_contradicted": "report"},
  "categories": [
    {"name": "price", "keywords": ["price", "cost"], "verify_new_answers": true, "on_contradicted": "correct"},
    {"name": "policy", "keywords": ["policy"], "on_contradicted": "uncache"},
    {"name": "schedule", "keywords": ["schedule"]}
  ],
  "facts": [
    {"name": "Pro price", "category": "price", "source_url": "https://Example.com/pricing/#top",
     "source_pattern": "([0-9]+)", "answer_pattern": "([0-9]+)", "compare": "numeric"},
    {"name": "Team price", "category": "price", "source_url": "https://example.com/pricing",
     "source_pattern": "([0-9]+)", "answer_pattern": "([0-9]+)", "compare": "numeric"},
    {"name": "Refunds", "category": "policy", "source_url": "https://example.com/refunds",
     "source_pattern": "(\\w+)", "answer_pattern": "(\\w+)"}
  ]
}`

func TestContradictionSettingsResolveAndMerge(t *testing.T) {
	p, err := Parse([]byte(contradictionPolicy))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		text   string
		verify bool
		mode   OnContradicted
	}{
		{"What is the price?", true, ContradictedCorrect},
		{"What is the refund policy?", false, ContradictedUncache},
		{"What is the schedule?", false, ContradictedReport}, // the defaults apply
		{"price schedule", true, ContradictedCorrect},        // verify if any category asks
		{"policy schedule", false, ContradictedUncache},      // the more active mode wins
		{"price policy", true, ContradictedCorrect},          // correct is the most active
	}
	for _, c := range cases {
		eff, ok := p.Resolve(c.text)
		if !ok || eff.VerifyNewAnswers != c.verify || eff.OnContradicted != c.mode {
			t.Errorf("%q -> verify=%v mode=%q ok=%v, want %v %q", c.text, eff.VerifyNewAnswers, eff.OnContradicted, ok, c.verify, c.mode)
		}
	}
}

func TestContradictionSettingsAreValidated(t *testing.T) {
	for name, doc := range map[string]string{
		"bad default":  `{"defaults": {"on_contradicted": "delete"}, "categories": [{"name": "a", "keywords": ["a"]}]}`,
		"bad category": `{"categories": [{"name": "a", "keywords": ["a"], "on_contradicted": "fix"}]}`,
	} {
		if _, err := Parse([]byte(doc)); err == nil || !strings.Contains(err.Error(), "on_contradicted") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// Off and report unless the policy says otherwise.
	p, err := Parse([]byte(`{"categories": [{"name": "a", "keywords": ["a"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if eff, _ := p.Resolve("a"); eff.VerifyNewAnswers || eff.OnContradicted != ContradictedReport {
		t.Fatalf("defaults = %+v", eff)
	}
}

func TestPolicyLookups(t *testing.T) {
	p, err := Parse([]byte(contradictionPolicy))
	if err != nil {
		t.Fatal(err)
	}
	if p.Fact(" Pro price ") == nil || p.Fact("nope") != nil {
		t.Fatal("Fact lookup")
	}
	if !p.HasCategory("price") || p.HasCategory("weather") {
		t.Fatal("HasCategory")
	}
	if got := p.FactsInCategory("price"); len(got) != 2 {
		t.Fatalf("FactsInCategory = %d", len(got))
	}
	// Scheme and host case, a trailing slash and the fragment are ignored.
	if got := p.FactsWithSource("HTTPS://example.COM/pricing/"); len(got) != 2 {
		t.Fatalf("FactsWithSource = %d, want both price facts", len(got))
	}
	if len(p.FactsWithSource("https://example.com/pricing?x=1")) != 0 || len(p.FactsWithSource("not a url")) != 0 {
		t.Fatal("a different query or a non-URL matches nothing")
	}
	eff, _ := p.Resolve("price")
	if names := eff.FactNames(); len(names) != 2 || names[0] != "Pro price" {
		t.Fatalf("FactNames = %v", names)
	}
}

func TestGuardCheck(t *testing.T) {
	g := Guard{ToolResults: "bypass", SideEffectTools: []string{"create_*", "Send_Email"}}
	cases := []struct {
		results bool
		tools   []string
		want    string
	}{
		{true, nil, GuardToolResults},
		{false, []string{"get_weather"}, ""},
		{false, []string{"get_weather", "CREATE_TICKET"}, GuardSideEffect},
		{false, []string{"send_email"}, GuardSideEffect},
		{false, []string{"resend_email"}, ""}, // patterns match the whole name
		{false, nil, ""},
	}
	for _, c := range cases {
		if got := g.Check(c.results, c.tools); got != c.want {
			t.Errorf("Check(%v, %v) = %q, want %q", c.results, c.tools, got, c.want)
		}
	}
	if got := (Guard{}).Check(true, []string{"create_x"}); got != "" {
		t.Errorf("an empty guard does nothing, got %q", got)
	}
	if got := (Guard{ToolResults: "allow"}).Check(true, nil); got != "" {
		t.Errorf("allow does nothing, got %q", got)
	}
}
