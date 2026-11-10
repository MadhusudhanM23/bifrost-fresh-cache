package decision

import (
	"context"
	"fmt"
	"strings"

	"freshness-companion/internal/discover"
	"freshness-companion/internal/extract"
	"freshness-companion/internal/openai"
	"freshness-companion/internal/policy"
)

// Discoverer finds a source page for a time-sensitive hit that no watched
// fact covers (normally *discover.Service). Find derives a short query from
// the category's template and the question; only that query is sent out.
type Discoverer interface {
	Find(ctx context.Context, category string, d *policy.Discover, question string) (discover.Found, error)
	Record(category, query, outcome, value string)
}

// DiscoveredFactPrefix starts the name of a fact built for a discovered page.
const DiscoveredFactPrefix = "discovered: "

// tryDiscovery checks a cache hit that no watched fact covers against a page
// discovery finds. It returns true when that page decided the outcome
// (confirmed or stale). Otherwise d.discoveryNote says why, and the caller
// falls back to the behaviour without discovery. A discovered "confirmed"
// serves the answer only when the category limits search to include_domains;
// from an open-web page it keeps the action the normal rules give. Low
// confidence never yields confirmed: confirmed needs the answer and the page
// to state exactly one value each, and the same one.
func (e *Engine) tryDiscovery(ctx context.Context, in Input, p policy.Effective, d *Decision, age string) bool {
	disc := p.Discover
	fail := func(result, note string) bool {
		d.DiscoveryResult, d.discoveryNote = result, note
		return false
	}
	if !in.AgeKnown && (p.OnUnknown == policy.UnknownBypass || p.OnUnknown == policy.UnknownServeWithWarning) {
		return fail("skipped", fmt.Sprintf("discovery skipped: entry age unknown and on_unknown=%s", p.OnUnknown))
	}
	answer, err := openai.AnswerText(in.CachedBody)
	if err != nil {
		return fail(string(UnableToVerify), "discovery not tried: cannot read the cached answer: "+oneLine(err.Error(), 200))
	}
	// The answer is read first: if it states no single value there is
	// nothing to check, and no search credit is spent.
	if _, err := extract.Extract(disc.ValueRegexp(), answer, disc.Mode()); err != nil {
		return fail(string(UnableToVerify), "discovery not tried: the value pattern "+describe(err)+" in the cached answer, so nothing was searched")
	}
	found, err := e.Discover.Find(ctx, p.DiscoverCategory, disc, in.Question)
	d.DiscoveryQuery = found.Query
	if err != nil {
		return fail(discover.Reason(err), "discovery: "+discover.Reason(err)+": "+oneLine(err.Error(), 300))
	}
	if len(found.Sources) == 0 {
		return fail("no_source", "discovery: no page was returned")
	}
	d.FoundBy = found.FoundBy
	d.DiscoveryRank, d.DiscoveryScore = found.Sources[0].Rank, found.Sources[0].Score
	srcRE, err := discover.EntityPattern(found.Entities, disc.ValuePatternSource(), disc.Near())
	if err != nil {
		return fail(string(UnableToVerify), "discovery: could not build the page pattern: "+oneLine(err.Error(), 200))
	}
	var results []factResult
	for _, src := range found.Sources {
		f := policy.NewDiscoveredFact(DiscoveredFactPrefix+found.Query, p.DiscoverCategory, src.URL, srcRE, disc.ValueRegexp(), disc.Mode())
		results = append(results, e.checkFact(ctx, f, answer, nil))
	}
	r := results[0]
	pages := make([]PageCheck, len(results))
	for i, x := range results {
		pages[i] = PageCheck{Outcome: x.outcome, Current: x.current, HaveCurrent: x.haveCurrent, Reason: x.reason}
	}
	if why, ok := PagesAgree(pages, disc.Mode()); !ok {
		r.outcome, r.reason = UnableToVerify, why
	}
	value := ""
	if r.haveCurrent {
		value = r.current.Raw
	}
	e.Discover.Record(p.DiscoverCategory, found.Query, string(r.outcome), value)
	if r.outcome != Confirmed && r.outcome != Stale {
		return fail(string(UnableToVerify), "discovery: "+foundWords(found)+", but it could not settle the answer: "+r.reason)
	}
	if r.outcome == Confirmed && !disc.Trusted() {
		// A page from the open web (no include_domains) may say "stale" and
		// so force a refresh, but its "confirmed" is never a step up past
		// the normal rules: the action stays what it would be without
		// discovery, with this note added to the reason.
		return fail(string(Confirmed), "discovery: "+foundWords(found)+" and it agrees ("+r.reason+
			"), but the page came from the open web (the category has no include_domains), so the normal rules decide")
	}

	d.Outcome = r.outcome
	d.Discovered, d.DiscoveryResult = true, string(r.outcome)
	d.fact = r.fact
	d.Fact = r.fact.Name
	d.SourceURL = r.fact.SourceURL
	if r.fetched {
		d.Provider = r.provider
		d.CheckedAt = r.checkedAt
		d.EvidenceCached = r.evidenceCached
	}
	if r.haveCached {
		d.CachedValue = r.cached.Raw
	}
	if r.haveCurrent {
		d.CurrentValue = r.current.Raw
		d.current, d.haveCurrent = r.current, true
	}
	if d.Outcome == Confirmed {
		d.Action = Serve
	} else {
		d.Action = Refresh
	}
	reason := "no watched fact applies; " + foundWords(found) + "; " + r.reason
	if in.AgeKnown {
		reason += "; entry age " + age
	} else {
		reason += "; entry age unknown, so it was revalidated against the discovered page"
	}
	d.Reason = reason
	return true
}

// PageCheck is the result of checking an answer against one discovered page.
type PageCheck struct {
	Outcome     Outcome
	Current     extract.Value
	HaveCurrent bool
	Reason      string
}

// PagesAgree reports whether all discovered pages (min_sources) reached the
// same outcome and, where they read a value, the same value. When they do
// not, it returns why; the caller then treats the answer as unable_to_verify.
func PagesAgree(pages []PageCheck, mode extract.Mode) (string, bool) {
	if len(pages) < 2 {
		return "", true
	}
	first := pages[0]
	var ref *PageCheck
	agree := true
	for i := range pages {
		p := &pages[i]
		if p.Outcome != first.Outcome {
			agree = false
		}
		if p.HaveCurrent {
			if ref == nil {
				ref = p
			} else if !extract.Equal(ref.Current, p.Current, mode) {
				agree = false
			}
		}
	}
	if agree {
		return "", true
	}
	reasons := make([]string, len(pages))
	for i, p := range pages {
		reasons[i] = p.Reason
	}
	return fmt.Sprintf("the %d discovered pages disagree: %s", len(pages), strings.Join(reasons, "; ")), false
}

// foundWords says how the page was found, in plain words.
func foundWords(f discover.Found) string {
	if len(f.Sources) == 0 {
		return fmt.Sprintf("Tavily Search for %q found no page", f.Query)
	}
	src := f.Sources[0]
	if f.FoundBy == discover.FoundByCandidateURL {
		return fmt.Sprintf("Tavily Search for %q found no usable page, so the policy's candidate URL %s was read", f.Query, src.URL)
	}
	s := fmt.Sprintf("Tavily Search for %q picked result #%d (score %.2f) %s", f.Query, src.Rank, src.Score, src.URL)
	if len(f.Sources) > 1 {
		s += fmt.Sprintf(" and result #%d %s", f.Sources[1].Rank, f.Sources[1].URL)
	}
	if f.Reused {
		s += " (learned earlier, reused without a new search)"
	}
	return s
}
