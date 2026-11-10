package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"freshness-companion/internal/discover"
	"freshness-companion/internal/evidence"
	"freshness-companion/internal/policy"
)

type staticEvidence struct {
	pages map[string]string
	calls int
}

func (s *staticEvidence) ProviderName() string { return "fake_provider" }
func (s *staticEvidence) Get(_ context.Context, u string) (evidence.Result, error) {
	s.calls++
	return evidence.Result{Evidence: evidence.Evidence{URL: u, Content: s.pages[u], Provider: "fake_provider", FetchedAt: time.Now()}}, nil
}

type fakeDiscoverer struct {
	sources   []discover.Source // nil: the one Acme pricing page
	calls     int
	questions []string
	recorded  []string
}

func (f *fakeDiscoverer) Find(_ context.Context, category string, d *policy.Discover, question string) (discover.Found, error) {
	f.calls++
	q, ents, err := discover.BuildQuery(d, question)
	if err != nil {
		return discover.Found{}, err
	}
	sources := f.sources
	if sources == nil {
		sources = []discover.Source{{URL: "https://acme.example/pricing", Title: "Acme pricing", Rank: 1, Score: 0.83}}
	}
	return discover.Found{Query: q, Entities: ents, FoundBy: discover.FoundByTavilySearch, Sources: sources}, nil
}

func (f *fakeDiscoverer) Record(category, query, outcome, value string) {
	f.recorded = append(f.recorded, category+"|"+query+"|"+outcome+"|"+value)
}

func (f *fakeDiscoverer) Sources() []discover.Learned {
	return []discover.Learned{
		{Category: "price", Query: "Acme Pro plan price per month", URL: "https://acme.example/pricing", FoundBy: "tavily_search", Uses: 3},
		{Category: "price", Query: "Acme Team plan price per month", NoSource: true, Reason: "none of 2 results was usable"},
	}
}

func discoverFixture(t *testing.T) (*fixture, *fakeDiscoverer, *staticEvidence) {
	t.Helper()
	pol, err := policy.Parse([]byte(`{"version": 1, "categories": [
	  {"name": "price", "keywords": ["price", "cost", "how much"], "max_age": "10m",
	   "discover": {"enabled": true, "entities": ["Acme Pro", "Acme Team"], "query": "{entities} plan price per month"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	disc := &fakeDiscoverer{}
	ev := &staticEvidence{pages: map[string]string{"https://acme.example/pricing": "Acme pricing\nAcme Pro plan — $49 per month"}}
	f := newFixture(t, func(d *Deps, _ *Options) {
		d.Policy, d.Evidence, d.Discover = pol, ev, disc
	})
	return f, disc, ev
}

func TestCheckFreshnessUsesDiscovery(t *testing.T) {
	f, disc, _ := discoverFixture(t)
	res, raw := f.tool(t, ToolCheckFreshness, map[string]any{"question": "How much does the Acme Pro plan cost?", "answer": "It is $59 per month."}, nil)
	out := decodeCheck(t, raw)
	if res.IsError || out.Outcome != "stale" || out.Discovery != "found" || len(out.Facts) != 1 {
		t.Fatalf("out = %+v (%s)", out, res.Content[0].Text)
	}
	fc := out.Facts[0]
	if !fc.Discovered || fc.Query != "Acme Pro plan price per month" || fc.FoundBy != "tavily_search" || fc.Rank != 1 || fc.Score != 0.83 ||
		fc.SourceURL != "https://acme.example/pricing" || fc.CurrentValue != "49" || fc.ValueInAnswer != "59" {
		t.Fatalf("fact = %+v", fc)
	}
	if len(disc.recorded) != 1 || disc.recorded[0] != "price|Acme Pro plan price per month|stale|49" {
		t.Fatalf("recorded = %v", disc.recorded)
	}
	// No allowlisted name: no page, the old "no watched fact" answer plus why.
	res, raw = f.tool(t, ToolCheckFreshness, map[string]any{"question": "How much does Globex cost?", "answer": "$5 per month"}, nil)
	out = decodeCheck(t, raw)
	if out.Outcome != "unknown" || out.Discovery != "no_entities" || !strings.Contains(res.Content[0].Text, "Discovery did not find a page") {
		t.Fatalf("no entities: %+v (%s)", out, res.Content[0].Text)
	}
}

func TestListWatchedSourcesShowsLearned(t *testing.T) {
	f, _, _ := discoverFixture(t)
	res, raw := f.tool(t, ToolListWatchedSources, map[string]any{}, nil)
	var out sourcesResult
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.LearnedSources) != 2 || out.LearnedSources[0].URL != "https://acme.example/pricing" {
		t.Fatalf("learned = %+v", out.LearnedSources)
	}
	text := res.Content[0].Text
	if !strings.Contains(text, "2 learned source(s) from discovery") || !strings.Contains(text, `"Acme Pro plan price per month" (category price): https://acme.example/pricing`) ||
		!strings.Contains(text, "no usable page") {
		t.Fatalf("text = %q", text)
	}
	// Without discovery the list is empty (never null) and the text unchanged.
	plain := newFixture(t, nil)
	_, raw = plain.tool(t, ToolListWatchedSources, map[string]any{}, nil)
	if !strings.Contains(string(raw), `"learned_sources":[]`) {
		t.Fatalf("raw = %s", raw)
	}
}

// With min_sources 2 every discovered page is checked, not just the first:
// pages that disagree make the answer unable_to_verify.
func TestCheckFreshnessDiscoveryChecksAllSources(t *testing.T) {
	f, disc, ev := discoverFixture(t)
	disc.sources = []discover.Source{{URL: "https://acme.example/pricing", Rank: 1, Score: 0.9}, {URL: "https://b.example/pricing", Rank: 2, Score: 0.7}}
	ev.pages["https://b.example/pricing"] = "Acme Pro plan — $59 per month"
	q := map[string]any{"question": "How much does the Acme Pro plan cost?", "answer": "It is $49 per month."}
	res, raw := f.tool(t, ToolCheckFreshness, q, nil)
	out := decodeCheck(t, raw)
	if out.Outcome != "unable_to_verify" || len(out.Facts) != 2 || !strings.Contains(res.Content[0].Text, "disagree") {
		t.Fatalf("disagreeing pages: %+v (%s)", out, res.Content[0].Text)
	}
	for _, fc := range out.Facts {
		if fc.Verdict != "unable_to_verify" || !fc.Discovered {
			t.Fatalf("fact = %+v", fc)
		}
	}
	if out.Facts[1].Rank != 2 || out.Facts[1].SourceURL != "https://b.example/pricing" {
		t.Fatalf("second page = %+v", out.Facts[1])
	}
	if len(disc.recorded) != 1 || disc.recorded[0] != "price|Acme Pro plan price per month|unable_to_verify|49" {
		t.Fatalf("recorded = %v", disc.recorded)
	}
	// Both pages agree: each page confirms (open web, so the overall
	// verdict stays the one without discovery).
	ev.pages["https://b.example/pricing"] = "Acme Pro plan — $49 per month"
	_, raw = f.tool(t, ToolCheckFreshness, q, nil)
	if out = decodeCheck(t, raw); out.Outcome != "unknown" || len(out.Facts) != 2 || out.Facts[0].Verdict != "confirmed" || out.Facts[1].Verdict != "confirmed" {
		t.Fatalf("agreeing pages: %+v", out)
	}
}

// No answer, or an answer that states no value: nothing to check, so no
// search and no credit spent.
func TestCheckFreshnessDiscoverySkippedWithoutValue(t *testing.T) {
	f, disc, ev := discoverFixture(t)
	for _, c := range []struct{ answer, discovery, note string }{
		{"", "skipped", "no answer was given"},
		{"Please contact sales for a quote.", "unable_to_verify", "nothing was searched"},
		{"It was $39, now $49 per month.", "unable_to_verify", "nothing was searched"},
	} {
		args := map[string]any{"question": "How much does the Acme Pro plan cost?"}
		if c.answer != "" {
			args["answer"] = c.answer
		}
		res, raw := f.tool(t, ToolCheckFreshness, args, nil)
		out := decodeCheck(t, raw)
		if disc.calls != 0 || ev.calls != 0 || out.Discovery != c.discovery || len(out.Facts) != 0 || !strings.Contains(res.Content[0].Text, c.note) {
			t.Fatalf("answer %q: calls=%d reads=%d %+v (%s)", c.answer, disc.calls, ev.calls, out, res.Content[0].Text)
		}
	}
}

// An open-web page that agrees does not confirm (same verdict as without
// discovery, plus a note); with include_domains it does; stale stays stale.
func TestCheckFreshnessOpenWebConfirmedNotTrusted(t *testing.T) {
	q := map[string]any{"question": "How much does the Acme Pro plan cost?", "answer": "It is $49 per month."}
	f, _, _ := discoverFixture(t)
	res, raw := f.tool(t, ToolCheckFreshness, q, nil)
	out := decodeCheck(t, raw)
	if out.Outcome != "unknown" || out.Action != ActionNone || out.Discovery != "found" || len(out.Facts) != 1 ||
		out.Facts[0].Verdict != "confirmed" || !strings.Contains(res.Content[0].Text, "unverified source") {
		t.Fatalf("open web: %+v (%s)", out, res.Content[0].Text)
	}
	pol, err := policy.Parse([]byte(`{"version": 1, "categories": [
	  {"name": "price", "keywords": ["price", "cost", "how much"], "max_age": "10m",
	   "discover": {"enabled": true, "entities": ["Acme Pro"], "query": "{entities} plan price per month", "include_domains": ["acme.example"]}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	ev := &staticEvidence{pages: map[string]string{"https://acme.example/pricing": "Acme Pro plan — $49 per month"}}
	trusted := newFixture(t, func(d *Deps, _ *Options) { d.Policy, d.Evidence, d.Discover = pol, ev, &fakeDiscoverer{} })
	_, raw = trusted.tool(t, ToolCheckFreshness, q, nil)
	if out = decodeCheck(t, raw); out.Outcome != "confirmed" || out.Action != "serve" {
		t.Fatalf("include_domains: %+v", out)
	}
	_, raw = f.tool(t, ToolCheckFreshness, map[string]any{"question": "How much does the Acme Pro plan cost?", "answer": "It is $59 per month."}, nil)
	if out = decodeCheck(t, raw); out.Outcome != "stale" || out.Action != "refresh" {
		t.Fatalf("open web stale: %+v", out)
	}
}
