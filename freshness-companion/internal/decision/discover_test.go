package decision

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"freshness-companion/internal/discover"
	"freshness-companion/internal/evidence"
	"freshness-companion/internal/policy"
)

// fakeDiscoverer returns a fixed page (or error) and records the calls.
type fakeDiscoverer struct {
	found     discover.Found
	err       error
	calls     int
	questions []string
	recorded  []string
}

func (f *fakeDiscoverer) Find(_ context.Context, category string, d *policy.Discover, question string) (discover.Found, error) {
	f.calls++
	f.questions = append(f.questions, question)
	q, ents, err := discover.BuildQuery(d, question)
	if err != nil {
		return discover.Found{}, err
	}
	if f.err != nil {
		return discover.Found{Query: q, Entities: ents}, f.err
	}
	out := f.found
	out.Query, out.Entities = q, ents
	return out, nil
}

func (f *fakeDiscoverer) Record(category, query, outcome, value string) {
	f.recorded = append(f.recorded, category+"|"+query+"|"+outcome+"|"+value)
}

func discoverPolicy(t *testing.T, facts string) *policy.Policy {
	t.Helper()
	return discoverPolicyWith(t, facts, "")
}

// trustedPolicy is discoverPolicy with search limited to include_domains.
func trustedPolicy(t *testing.T) *policy.Policy {
	t.Helper()
	return discoverPolicyWith(t, "[]", `, "include_domains": ["acme.example"]`)
}

func discoverPolicyWith(t *testing.T, facts, extra string) *policy.Policy {
	t.Helper()
	p, err := policy.Parse([]byte(fmt.Sprintf(`{
	  "version": 1,
	  "defaults": {"max_age": "10m", "on_unverifiable": "bypass", "on_unknown": "auto", "on_contradicted": "correct"},
	  "categories": [{"name": "price", "keywords": ["price", "cost", "how much"],
	    "discover": {"enabled": true, "entities": ["Acme Pro", "Acme Team"], "query": "{entities} plan price per month"%s}}],
	  "facts": %s
	}`, extra, facts)))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func acmeFound() discover.Found {
	return discover.Found{FoundBy: discover.FoundByTavilySearch, Sources: []discover.Source{{URL: "https://acme.example/pricing", Title: "Acme pricing", Rank: 1, Score: 0.83}}}
}

func TestDecideDiscoveredConfirmed(t *testing.T) {
	ev := &fakeEvidence{content: page49}
	disc := &fakeDiscoverer{found: acmeFound()}
	e := &Engine{Evidence: ev, Discover: disc}
	// include_domains is set, so a confirmed page serves the answer, even
	// past max_age (20m > 10m): without discovery it would be refreshed.
	d := e.Decide(context.Background(), Input{
		Policy: resolve(t, trustedPolicy(t), proQuestion), CacheID: "id-1",
		Age: 20 * time.Minute, AgeKnown: true, CachedBody: answerBody(answer49), Question: proQuestion,
	})
	if d.Outcome != Confirmed || d.Action != Serve || !d.Discovered || d.DiscoveryResult != "confirmed" {
		t.Fatalf("got %s/%s discovered=%v (%s)", d.Outcome, d.Action, d.Discovered, d.Reason)
	}
	if d.SourceURL != "https://acme.example/pricing" || d.CurrentValue != "49" || d.CachedValue != "49" || d.FoundBy != "tavily_search" ||
		d.DiscoveryQuery != "Acme Pro plan price per month" || d.DiscoveryRank != 1 || d.DiscoveryScore != 0.83 {
		t.Fatalf("details = %+v", d)
	}
	if !strings.Contains(d.Reason, `Tavily Search for "Acme Pro plan price per month" picked result #1 (score 0.83)`) || !strings.HasPrefix(d.Fact, DiscoveredFactPrefix) {
		t.Fatalf("reason/fact = %q / %q", d.Reason, d.Fact)
	}
	if len(disc.recorded) != 1 || disc.recorded[0] != "price|Acme Pro plan price per month|confirmed|49" {
		t.Fatalf("recorded = %v", disc.recorded)
	}
}

func TestDecideDiscoveredStale(t *testing.T) {
	e := &Engine{Evidence: &fakeEvidence{content: page49}, Discover: &fakeDiscoverer{found: acmeFound()}}
	d := e.Decide(context.Background(), Input{
		Policy: resolve(t, discoverPolicy(t, "[]"), proQuestion), Age: time.Minute, AgeKnown: true,
		CachedBody: answerBody("The Acme Pro plan costs $59 per month."), Question: proQuestion,
	})
	if d.Outcome != Stale || d.Action != Refresh || !d.Discovered {
		t.Fatalf("got %s/%s (%s)", d.Outcome, d.Action, d.Reason)
	}
	// The refreshed answer is compared with the discovered page's value.
	if v, check, ok := CheckRefreshed(d, answerBody(answer49)); !ok || check != RefreshedMatches || v != "49" {
		t.Fatalf("CheckRefreshed = %q %q %v", v, check, ok)
	}
}

// An open-web page (no include_domains) that agrees is never a step up past
// the normal rules: the action is the one without discovery, plus a note.
func TestDecideOpenWebConfirmedKeepsNormalAction(t *testing.T) {
	pol := discoverPolicy(t, "[]")
	for _, age := range []time.Duration{time.Minute, 20 * time.Minute} {
		disc := &fakeDiscoverer{found: acmeFound()}
		in := Input{Policy: resolve(t, pol, proQuestion), Age: age, AgeKnown: true, CachedBody: answerBody(answer49), Question: proQuestion}
		d := (&Engine{Evidence: &fakeEvidence{content: page49}, Discover: disc}).Decide(context.Background(), in)
		plain := (&Engine{Evidence: &fakeEvidence{content: page49}}).Decide(context.Background(), in)
		if disc.calls != 1 || d.Discovered || d.DiscoveryResult != "confirmed" || d.Outcome != plain.Outcome || d.Action != plain.Action {
			t.Fatalf("age %s: got %s/%s discovered=%v result=%s, without discovery %s/%s (%s)",
				age, d.Outcome, d.Action, d.Discovered, d.DiscoveryResult, plain.Outcome, plain.Action, d.Reason)
		}
		if !strings.Contains(d.Reason, "open web") || !strings.HasPrefix(d.Reason, plain.Reason) {
			t.Fatalf("age %s: reason %q", age, d.Reason)
		}
		if age > 10*time.Minute && d.Action == Serve {
			t.Fatalf("past max_age an open-web confirmed must not serve: %s", d.Reason)
		}
	}
}

func TestDecideDiscoveryInconclusiveFallsBack(t *testing.T) {
	pol := discoverPolicy(t, "[]")
	cases := []struct {
		name, page, answer string
		disc               *fakeDiscoverer
		age                time.Duration
		want               Outcome
		wantAction         Action
		wantResult, reason string
		searched           bool
	}{
		{"pricing table conflict", "Acme Pro monthly $19\nAcme Pro yearly $49\nAcme Pro max $99", answer49, &fakeDiscoverer{found: acmeFound()}, time.Minute, Unknown, Serve, "unable_to_verify", "conflicting values", true},
		{"page names no value", "Contact sales for Acme Pro pricing.", answer49, &fakeDiscoverer{found: acmeFound()}, 20 * time.Minute, UnableToVerify, Refresh, "unable_to_verify", "matched nothing", true},
		{"no source", page49, answer49, &fakeDiscoverer{err: &discover.NoSourceError{Detail: "none of 2 results was usable", Hosts: []string{"reddit.com"}}}, time.Minute, Unknown, Serve, "no_source", "result hosts: reddit.com", true},
		{"over cap", page49, answer49, &fakeDiscoverer{err: discover.ErrOverCap}, time.Minute, Unknown, Serve, "over_cap", "budget", true},
		{"answer states two prices: no search", page49, "It was $39 per month, now $49 per month.", &fakeDiscoverer{found: acmeFound()}, time.Minute, Unknown, Serve, "unable_to_verify", "nothing was searched", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := &Engine{Evidence: &fakeEvidence{content: c.page}, Discover: c.disc}
			d := e.Decide(context.Background(), Input{
				Policy: resolve(t, pol, proQuestion), Age: c.age, AgeKnown: true, CachedBody: answerBody(c.answer), Question: proQuestion,
			})
			if d.Outcome != c.want || d.Action != c.wantAction || d.Discovered || d.DiscoveryResult != c.wantResult {
				t.Fatalf("got %s/%s discovered=%v result=%s (%s)", d.Outcome, d.Action, d.Discovered, d.DiscoveryResult, d.Reason)
			}
			if !strings.Contains(d.Reason, c.reason) || d.SourceURL != "" || d.Fact != "" {
				t.Fatalf("reason %q should contain %q; source %q fact %q", d.Reason, c.reason, d.SourceURL, d.Fact)
			}
			if (c.disc.calls > 0) != c.searched {
				t.Fatalf("searched = %v, want %v", c.disc.calls > 0, c.searched)
			}
			// A fallback is the same decision as without discovery.
			plain := (&Engine{Evidence: &fakeEvidence{content: c.page}}).Decide(context.Background(), Input{
				Policy: resolve(t, pol, proQuestion), Age: c.age, AgeKnown: true, CachedBody: answerBody(c.answer),
			})
			if plain.Outcome != d.Outcome || plain.Action != d.Action {
				t.Fatalf("fallback %s/%s differs from no discovery %s/%s", d.Outcome, d.Action, plain.Outcome, plain.Action)
			}
		})
	}
	// Age unknown with an explicit on_unknown: skipped, no search.
	disc := &fakeDiscoverer{found: acmeFound()}
	eff := resolve(t, pol, proQuestion)
	eff.OnUnknown = policy.UnknownServeWithWarning
	d := (&Engine{Evidence: &fakeEvidence{content: page49}, Discover: disc}).Decide(context.Background(), Input{Policy: eff, CachedBody: answerBody(answer49), Question: proQuestion})
	if disc.calls != 0 || d.DiscoveryResult != "skipped" || d.Action != ServeWithWarning {
		t.Fatalf("on_unknown=serve_with_warning: calls=%d result=%s action=%s", disc.calls, d.DiscoveryResult, d.Action)
	}
	// Age unknown with auto: discovery runs (auto means revalidate when it
	// can); a page from include_domains confirms.
	disc = &fakeDiscoverer{found: acmeFound()}
	d = (&Engine{Evidence: &fakeEvidence{content: page49}, Discover: disc}).Decide(context.Background(), Input{Policy: resolve(t, trustedPolicy(t), proQuestion), CachedBody: answerBody(answer49), Question: proQuestion})
	if disc.calls != 1 || d.Outcome != Confirmed || !strings.Contains(d.Reason, "entry age unknown") {
		t.Fatalf("on_unknown=auto: calls=%d %s (%s)", disc.calls, d.Outcome, d.Reason)
	}
}

func TestDecideWatchedFactWinsNoSearch(t *testing.T) {
	facts := `[{"name": "Acme Pro plan price", "category": "price", "keywords": ["pro"],
	  "source_url": "https://example.github.io/pricing/",
	  "source_pattern": "Acme Pro plan[^$0-9]{0,20}\\$\\s*([0-9]+)", "answer_pattern": "\\$\\s*([0-9]+)", "compare": "numeric"}]`
	disc := &fakeDiscoverer{found: acmeFound()}
	d := (&Engine{Evidence: &fakeEvidence{content: page49}, Discover: disc}).Decide(context.Background(), Input{
		Policy: resolve(t, discoverPolicy(t, facts), proQuestion), Age: time.Minute, AgeKnown: true, CachedBody: answerBody(answer49), Question: proQuestion,
	})
	if disc.calls != 0 || d.Fact != "Acme Pro plan price" || d.Discovered || d.DiscoveryResult != "" {
		t.Fatalf("a watched fact must decide without a search: calls=%d fact=%q", disc.calls, d.Fact)
	}
}

func TestDecideChangeNoticeSkipsDiscovery(t *testing.T) {
	disc := &fakeDiscoverer{found: acmeFound()}
	d := (&Engine{Evidence: &fakeEvidence{content: page49}, Discover: disc}).Decide(context.Background(), Input{
		Policy: resolve(t, discoverPolicy(t, "[]"), proQuestion), Age: time.Minute, AgeKnown: true, CachedBody: answerBody(answer49), Question: proQuestion,
		Change: &ChangeNotice{Subject: `category "price"`, At: fetchedAt},
	})
	if disc.calls != 0 || d.Outcome != Stale || d.Action != Refresh || d.DiscoveryResult != "" {
		t.Fatalf("change notice: calls=%d %s/%s", disc.calls, d.Outcome, d.Action)
	}
}

func TestDecideNilDiscovererUnchanged(t *testing.T) {
	pol := discoverPolicy(t, "[]")
	for _, age := range []time.Duration{time.Minute, time.Hour} {
		in := Input{Policy: resolve(t, pol, proQuestion), Age: age, AgeKnown: true, CachedBody: answerBody(answer49), Question: proQuestion}
		with := (&Engine{Evidence: &fakeEvidence{content: page49}}).Decide(context.Background(), in)
		in.Question = ""
		without := (&Engine{Evidence: &fakeEvidence{content: page49}}).Decide(context.Background(), in)
		if with.Outcome != without.Outcome || with.Action != without.Action || with.Reason != without.Reason || with.DiscoveryResult != "" {
			t.Fatalf("nil discoverer changed the decision: %+v vs %+v", with, without)
		}
	}
}

func TestDecideMinSourcesDisagree(t *testing.T) {
	ev := &perURLEvidence{pages: map[string]string{"https://a.example/p": page49, "https://b.example/p": page59}}
	found := discover.Found{FoundBy: discover.FoundByTavilySearch, Sources: []discover.Source{{URL: "https://a.example/p", Rank: 1, Score: 0.9}, {URL: "https://b.example/p", Rank: 2, Score: 0.8}}}
	d := (&Engine{Evidence: ev, Discover: &fakeDiscoverer{found: found}}).Decide(context.Background(), Input{
		Policy: resolve(t, discoverPolicy(t, "[]"), proQuestion), Age: time.Minute, AgeKnown: true, CachedBody: answerBody(answer49), Question: proQuestion,
	})
	if d.Discovered || d.DiscoveryResult != "unable_to_verify" || !strings.Contains(d.Reason, "disagree") {
		t.Fatalf("two disagreeing pages: %s %s (%s)", d.Outcome, d.DiscoveryResult, d.Reason)
	}
}

type perURLEvidence struct{ pages map[string]string }

func (p *perURLEvidence) ProviderName() string { return "fake_provider" }
func (p *perURLEvidence) Get(_ context.Context, u string) (evidence.Result, error) {
	return evidence.Result{Evidence: evidence.Evidence{URL: u, Content: p.pages[u], Provider: "fake_provider", FetchedAt: fetchedAt}}, nil
}
