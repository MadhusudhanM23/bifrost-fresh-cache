package decision

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"freshness-companion/internal/evidence"
	"freshness-companion/internal/extract"
	"freshness-companion/internal/policy"
)

var fetchedAt = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

// fakeEvidence returns fixed content (or an error) and counts calls.
type fakeEvidence struct {
	content string
	err     error
	calls   int
}

func (f *fakeEvidence) ProviderName() string { return "fake_provider" }

func (f *fakeEvidence) Get(_ context.Context, sourceURL string) (evidence.Result, error) {
	f.calls++
	if f.err != nil {
		return evidence.Result{}, f.err
	}
	return evidence.Result{Evidence: evidence.Evidence{URL: sourceURL, Content: f.content, Provider: "fake_provider", FetchedAt: fetchedAt}}, nil
}

func testPolicy(t *testing.T, onUnverifiable, onUnknown string) *policy.Policy {
	t.Helper()
	p, err := policy.Parse([]byte(fmt.Sprintf(`{
	  "version": 1,
	  "defaults": {"max_age": "10m", "on_unverifiable": %q, "on_unknown": %q},
	  "categories": [{"name": "price", "keywords": ["price", "cost", "how much"]}],
	  "facts": [{
	    "name": "Acme Pro plan price", "category": "price", "keywords": ["pro"],
	    "source_url": "https://example.github.io/pricing/",
	    "source_pattern": "Acme Pro plan[^$0-9]{0,20}\\$\\s*([0-9][0-9,]*(?:\\.[0-9]+)?)\\s*per month",
	    "answer_pattern": "(?i)\\$\\s*([0-9][0-9,]*(?:\\.[0-9]+)?)\\s*(?:/\\s*mo(?:nth)?|per\\s+month|a\\s+month|monthly)\\b",
	    "compare": "numeric"
	  }]
	}`, onUnverifiable, onUnknown)))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func resolve(t *testing.T, p *policy.Policy, prompt string) policy.Effective {
	t.Helper()
	eff, ok := p.Resolve(prompt)
	if !ok {
		t.Fatalf("%q should be time-sensitive", prompt)
	}
	return eff
}

func answerBody(text string) []byte {
	return []byte(fmt.Sprintf(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}]}`, text))
}

const (
	proQuestion = "How much does the Acme Pro plan cost?"
	page49      = "Acme pricing Pro plan Acme Pro plan — $49 per month Last updated"
	page59      = "Acme pricing Pro plan Acme Pro plan — $59 per month Last updated"
	answer49    = "The Acme Pro plan costs $49 per month."
)

func TestOutcomeConfirmed(t *testing.T) {
	ev := &fakeEvidence{content: page49}
	e := &Engine{Evidence: ev}
	d := e.Decide(context.Background(), Input{
		Policy: resolve(t, testPolicy(t, "bypass", "auto"), proQuestion), CacheID: "id-1",
		Age: 2 * time.Minute, AgeKnown: true, CachedBody: answerBody("The Acme Pro plan costs $49.00 per month."),
	})
	if d.Outcome != Confirmed || d.Action != Serve {
		t.Fatalf("got %s/%s (%s), want confirmed/serve", d.Outcome, d.Action, d.Reason)
	}
	if d.CachedValue != "49.00" || d.CurrentValue != "49" || d.Provider != "fake_provider" || !d.CheckedAt.Equal(fetchedAt) {
		t.Fatalf("details = %+v", d)
	}
	if d.Fact != "Acme Pro plan price" || d.SourceURL != "https://example.github.io/pricing/" || ev.calls != 1 {
		t.Fatalf("fact/source/calls = %q %q %d", d.Fact, d.SourceURL, ev.calls)
	}
}

func TestOutcomeStale(t *testing.T) {
	e := &Engine{Evidence: &fakeEvidence{content: page59}}
	d := e.Decide(context.Background(), Input{
		Policy: resolve(t, testPolicy(t, "serve_with_warning", "auto"), proQuestion),
		Age:    time.Minute, AgeKnown: true, CachedBody: answerBody(answer49),
	})
	if d.Outcome != Stale || d.Action != Refresh {
		t.Fatalf("got %s/%s (%s), want stale/refresh", d.Outcome, d.Action, d.Reason)
	}
	if d.CachedValue != "49" || d.CurrentValue != "59" || !strings.Contains(d.Reason, "differs") {
		t.Fatalf("details = %+v", d)
	}
}

func TestOutcomeUnableToVerify(t *testing.T) {
	cases := []struct {
		name           string
		evidence       *fakeEvidence
		answer         []byte
		wantFetch      bool
		wantReasonPart string
	}{
		{"fetch error", &fakeEvidence{err: errors.New("tavily extract: HTTP 500: Internal Server Error")}, answerBody(answer49), true, "evidence fetch failed"},
		{"timeout", &fakeEvidence{err: context.DeadlineExceeded}, answerBody(answer49), true, "deadline exceeded"},
		{"no match in source", &fakeEvidence{content: "Pricing is changing, contact sales."}, answerBody(answer49), true, "source_pattern matched nothing"},
		{"conflicting source", &fakeEvidence{content: page49 + " " + page59}, answerBody(answer49), true, "conflicting values (49, 59)"},
		{"no match in answer", &fakeEvidence{content: page49}, answerBody("Please check the pricing page."), false, "answer_pattern matched nothing"},
		{"conflicting answer", &fakeEvidence{content: page49}, answerBody("It was $39 per month, now $49 per month."), false, "conflicting values"},
		{"unreadable answer", &fakeEvidence{content: page49}, []byte("not json"), false, "cannot read the cached answer"},
	}
	for _, c := range cases {
		for _, onUnverifiable := range []string{"bypass", "serve_with_warning"} {
			t.Run(c.name+"/"+onUnverifiable, func(t *testing.T) {
				ev := &fakeEvidence{content: c.evidence.content, err: c.evidence.err}
				d := (&Engine{Evidence: ev}).Decide(context.Background(), Input{
					Policy: resolve(t, testPolicy(t, onUnverifiable, "auto"), proQuestion),
					Age:    time.Minute, AgeKnown: true, CachedBody: c.answer,
				})
				want := Refresh
				if onUnverifiable == "serve_with_warning" {
					want = ServeWithWarning
				}
				if d.Outcome != UnableToVerify || d.Action != want {
					t.Fatalf("got %s/%s (%s), want unable_to_verify/%s", d.Outcome, d.Action, d.Reason, want)
				}
				if !strings.Contains(d.Reason, c.wantReasonPart) {
					t.Fatalf("reason %q does not mention %q", d.Reason, c.wantReasonPart)
				}
				if (ev.calls > 0) != c.wantFetch {
					t.Fatalf("evidence calls = %d, want fetch=%v (the answer is read first)", ev.calls, c.wantFetch)
				}
			})
		}
	}
}

func TestNoWatchedFactUsesAgeAndPolicy(t *testing.T) {
	p := testPolicy(t, "bypass", "auto")
	eff := resolve(t, p, "What is the price of shipping?") // price category, but the "pro" fact does not apply
	if len(eff.Facts) != 0 {
		t.Fatalf("no fact should apply: %+v", eff.Facts)
	}
	ev := &fakeEvidence{content: page49}
	e := &Engine{Evidence: ev}

	within := e.Decide(context.Background(), Input{Policy: eff, Age: 5 * time.Minute, AgeKnown: true, CachedBody: answerBody("$5")})
	if within.Outcome != Unknown || within.Action != Serve || !strings.Contains(within.Reason, "policy only") {
		t.Fatalf("within max_age: got %s/%s (%s); want unknown/serve, labelled as policy-only", within.Outcome, within.Action, within.Reason)
	}

	tooOld := e.Decide(context.Background(), Input{Policy: eff, Age: 11 * time.Minute, AgeKnown: true})
	if tooOld.Outcome != UnableToVerify || tooOld.Action != Refresh {
		t.Fatalf("older than max_age: got %s/%s (%s); want unable_to_verify/refresh (bypass)", tooOld.Outcome, tooOld.Action, tooOld.Reason)
	}

	unknownAge := e.Decide(context.Background(), Input{Policy: eff})
	if unknownAge.Outcome != Unknown || unknownAge.Action != ServeWithWarning {
		t.Fatalf("unknown age, no fact: got %s/%s; want unknown/serve_with_warning", unknownAge.Outcome, unknownAge.Action)
	}
	if ev.calls != 0 {
		t.Fatalf("no watched fact applies, so evidence must not be fetched (calls = %d)", ev.calls)
	}
}

func TestOutcomeUnknownWithWatchedFact(t *testing.T) {
	// Default (auto): unknown age + watched fact => revalidate against evidence.
	ev := &fakeEvidence{content: page49}
	d := (&Engine{Evidence: ev}).Decide(context.Background(), Input{
		Policy: resolve(t, testPolicy(t, "bypass", "auto"), proQuestion), CachedBody: answerBody(answer49),
	})
	if d.Outcome != Confirmed || ev.calls != 1 || !strings.Contains(d.Reason, "revalidated") {
		t.Fatalf("auto: got %s (%s), calls %d; want a revalidation", d.Outcome, d.Reason, ev.calls)
	}

	// on_unknown=serve_with_warning skips the check and says so.
	ev = &fakeEvidence{content: page49}
	d = (&Engine{Evidence: ev}).Decide(context.Background(), Input{
		Policy: resolve(t, testPolicy(t, "bypass", "serve_with_warning"), proQuestion), CachedBody: answerBody(answer49),
	})
	if d.Outcome != Unknown || d.Action != ServeWithWarning || ev.calls != 0 {
		t.Fatalf("serve_with_warning: got %s/%s, calls %d", d.Outcome, d.Action, ev.calls)
	}

	// on_unknown=bypass refreshes without checking.
	ev = &fakeEvidence{content: page49}
	d = (&Engine{Evidence: ev}).Decide(context.Background(), Input{
		Policy: resolve(t, testPolicy(t, "serve_with_warning", "bypass"), proQuestion), CachedBody: answerBody(answer49),
	})
	if d.Outcome != Unknown || d.Action != Refresh || ev.calls != 0 {
		t.Fatalf("bypass: got %s/%s, calls %d", d.Outcome, d.Action, ev.calls)
	}
}

func TestNeverConfirmedWithoutEvidence(t *testing.T) {
	contents := []*fakeEvidence{{content: page49}, {content: page59}, {err: errors.New("down")}, {content: ""}}
	answers := [][]byte{answerBody(answer49), answerBody("no price"), nil}
	prompts := []string{proQuestion, "What is the price of shipping?"}
	for _, onUnknown := range []string{"auto", "revalidate", "serve_with_warning", "bypass"} {
		p := testPolicy(t, "bypass", onUnknown)
		for _, prompt := range prompts {
			for _, c := range contents {
				for _, a := range answers {
					for _, known := range []bool{true, false} {
						ev := &fakeEvidence{content: c.content, err: c.err}
						d := (&Engine{Evidence: ev}).Decide(context.Background(), Input{
							Policy: resolve(t, p, prompt), Age: time.Minute, AgeKnown: known, CachedBody: a,
						})
						if d.Outcome == Confirmed && (ev.calls == 0 || d.CurrentValue == "" || d.CheckedAt.IsZero()) {
							t.Fatalf("confirmed without evidence: %+v", d)
						}
						switch d.Outcome {
						case Confirmed, Stale, UnableToVerify, Unknown:
						default:
							t.Fatalf("unexpected outcome %q", d.Outcome)
						}
					}
				}
			}
		}
	}
}

// booleanPolicy builds a policy with one boolean-compare watched fact,
// mirroring examples/policy.boolean-demo.json, to prove the decision engine
// handles a non-numeric fact end to end (confirmed/stale), not just prices.
func booleanPolicy(t *testing.T) *policy.Policy {
	t.Helper()
	p, err := policy.Parse([]byte(`{
	  "version": 1,
	  "defaults": {"max_age": "10m", "on_unverifiable": "bypass", "on_unknown": "auto"},
	  "categories": [{"name": "availability", "keywords": ["live chat", "chat support"]}],
	  "facts": [{
	    "name": "Live chat availability", "category": "availability",
	    "source_url": "https://example.com/support",
	    "source_pattern": "(?i)(live chat[^.!?]*)",
	    "answer_pattern": "(?i)(live chat[^.!?]*)",
	    "compare": "boolean"
	  }]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

const chatQuestion = "Is live chat support currently available?"

func TestOutcomeStaleBooleanCachedTrueCurrentFalse(t *testing.T) {
	// Cached answer claims chat is available; the live page now says it is not.
	e := &Engine{Evidence: &fakeEvidence{content: "Live chat support is currently unavailable."}}
	d := e.Decide(context.Background(), Input{
		Policy: resolve(t, booleanPolicy(t), chatQuestion),
		Age:    time.Minute, AgeKnown: true,
		CachedBody: answerBody("Yes, live chat support is currently available."),
	})
	if d.Outcome != Stale || d.Action != Refresh {
		t.Fatalf("got %s/%s (%s), want stale/refresh", d.Outcome, d.Action, d.Reason)
	}
	if d.CachedValue == "" || d.CurrentValue == "" {
		t.Fatalf("expected both raw values to be reported, got %+v", d)
	}
	cached, err := extract.Extract(regexp.MustCompile(`(?i)(live chat[^.!?]*)`), d.CachedValue, extract.Boolean)
	if err != nil || cached.Normalized != "true" {
		t.Fatalf("cached value %q should normalize to true: %v", d.CachedValue, err)
	}
	current, err := extract.Extract(regexp.MustCompile(`(?i)(live chat[^.!?]*)`), d.CurrentValue, extract.Boolean)
	if err != nil || current.Normalized != "false" {
		t.Fatalf("current value %q should normalize to false: %v", d.CurrentValue, err)
	}
	if !strings.Contains(d.Reason, "differs") {
		t.Fatalf("reason should mention the mismatch: %q", d.Reason)
	}
}

func TestOutcomeStaleBooleanCachedFalseCurrentTrue(t *testing.T) {
	// Cached answer claims chat is unavailable; the live page now says it is.
	e := &Engine{Evidence: &fakeEvidence{content: "Live chat support is currently available."}}
	d := e.Decide(context.Background(), Input{
		Policy: resolve(t, booleanPolicy(t), chatQuestion),
		Age:    time.Minute, AgeKnown: true,
		CachedBody: answerBody("No, live chat is currently unavailable."),
	})
	if d.Outcome != Stale || d.Action != Refresh {
		t.Fatalf("got %s/%s (%s), want stale/refresh", d.Outcome, d.Action, d.Reason)
	}
	if v, _, err := extract.ParseBoolean(d.CachedValue); err != nil || v != "false" {
		t.Fatalf("cached value %q should normalize to false: %v", d.CachedValue, err)
	}
	if v, _, err := extract.ParseBoolean(d.CurrentValue); err != nil || v != "true" {
		t.Fatalf("current value %q should normalize to true: %v", d.CurrentValue, err)
	}
}

func TestOutcomeConfirmedBooleanBothTrue(t *testing.T) {
	// Cached and live both say chat is available, worded differently: confirmed.
	ev := &fakeEvidence{content: "Live chat support is currently available."}
	e := &Engine{Evidence: ev}
	d := e.Decide(context.Background(), Input{
		Policy: resolve(t, booleanPolicy(t), chatQuestion),
		Age:    time.Minute, AgeKnown: true,
		CachedBody: answerBody("Yes, live chat is online and ready to help."),
	})
	if d.Outcome != Confirmed || d.Action != Serve {
		t.Fatalf("got %s/%s (%s), want confirmed/serve", d.Outcome, d.Action, d.Reason)
	}
	if ev.calls != 1 || !strings.Contains(d.Reason, "matches") {
		t.Fatalf("details = %+v", d)
	}
}

func TestCheckRefreshed(t *testing.T) {
	e := &Engine{Evidence: &fakeEvidence{content: page59}}
	d := e.Decide(context.Background(), Input{
		Policy: resolve(t, testPolicy(t, "bypass", "auto"), proQuestion), Age: time.Minute, AgeKnown: true, CachedBody: answerBody(answer49),
	})
	if v, r, ok := CheckRefreshed(d, answerBody("The Acme Pro plan costs $59 per month.")); !ok || r != RefreshedMatches || v != "59" {
		t.Fatalf("fresh answer: %q %q %v", v, r, ok)
	}
	if v, r, ok := CheckRefreshed(d, answerBody(answer49)); !ok || r != RefreshedDiffers || v != "49" {
		t.Fatalf("still-old answer: %q %q %v", v, r, ok)
	}
	if _, r, ok := CheckRefreshed(d, answerBody("I am not sure.")); !ok || r != RefreshedNoValue {
		t.Fatalf("no value: %q %v", r, ok)
	}
	if _, _, ok := CheckRefreshed(Decision{}, nil); ok {
		t.Fatal("nothing to compare without a current value")
	}
}
