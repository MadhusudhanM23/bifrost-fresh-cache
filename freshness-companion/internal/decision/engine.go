// Package decision turns a time-sensitive cache hit into exactly one of four
// outcomes, and the action the companion takes:
//
//	confirmed        evidence shows the cached value is current      -> serve
//	stale            evidence shows a different current value         -> refresh
//	unable_to_verify the check could not be done or was inconclusive  -> on_unverifiable
//	unknown          no reliable metadata, or no check was performed  -> on_unknown / policy
//
// A fresh answer (a cache miss) can be checked too, before it is reused
// (CheckNewAnswer). Its outcome is confirmed, unable_to_verify, or
// contradicted: the model's new answer states a different value than the
// evidence, so it was wrong from the start rather than gone stale.
//
// "confirmed" is only ever produced by an evidence comparison. Serving on
// policy alone (age within max_age, no watched fact) is labelled unknown.
package decision

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"freshness-companion/internal/evidence"
	"freshness-companion/internal/extract"
	"freshness-companion/internal/openai"
	"freshness-companion/internal/policy"
)

// Outcome is the freshness verdict on a cached answer.
type Outcome string

// The four outcomes of a cache hit, plus contradicted for a new answer.
const (
	Confirmed      Outcome = "confirmed"
	Stale          Outcome = "stale"
	UnableToVerify Outcome = "unable_to_verify"
	Unknown        Outcome = "unknown"
	Contradicted   Outcome = "contradicted"
)

// Action is what the companion should do with the cached answer.
type Action string

// Actions.
const (
	// Serve returns the cached answer.
	Serve Action = "serve"
	// ServeWithWarning returns the cached answer with a warning header.
	ServeWithWarning Action = "serve_with_warning"
	// Refresh does not serve the cached answer: the entry is refreshed
	// (by the configured strategy) and the fresh answer is returned.
	Refresh Action = "refresh"
	// Uncache returns a fresh but contradicted answer, labelled, and deletes
	// its cache entry so it is never reused.
	Uncache Action = "uncache"
	// Correct deletes a contradicted answer's entry and asks the model again
	// with the verified current value (see CorrectionPrompt).
	Correct Action = "correct"
)

// ContradictedAction maps on_contradicted to the action for an answer the
// evidence contradicts. report keeps today's behaviour: serve it, labelled.
func ContradictedAction(o policy.OnContradicted) Action {
	switch o {
	case policy.ContradictedUncache:
		return Uncache
	case policy.ContradictedCorrect:
		return Correct
	}
	return Serve
}

// ChangeNotice is a "this changed" notice that covers a request (its
// category or one of its facts). The proxy passes one only when it arrived
// after the entry was written, or when the entry's age is unknown.
type ChangeNotice struct {
	// Subject names what changed, e.g. `category "policy"`.
	Subject string
	At      time.Time
}

// EvidenceSource returns evidence for a source URL (normally *evidence.Cache).
type EvidenceSource interface {
	Get(ctx context.Context, sourceURL string) (evidence.Result, error)
	ProviderName() string
}

// Engine makes decisions. It holds no per-request state.
type Engine struct {
	Evidence EvidenceSource
}

// Input describes one time-sensitive cache hit.
type Input struct {
	Policy   policy.Effective
	CacheID  string
	Age      time.Duration
	AgeKnown bool
	// CachedBody is the (decoded) OpenAI JSON body of the cached response.
	// Only the values captured by the policy patterns leave the engine.
	CachedBody []byte
	// Change, when set, is a change notice newer than the entry. With a
	// watched fact the evidence decides as usual (the notice already dropped
	// the cached evidence); without one the entry is treated as stale.
	Change *ChangeNotice
}

// Decision is the verdict plus everything the headers and the log show.
type Decision struct {
	Outcome        Outcome
	Action         Action
	Reason         string
	Categories     []string
	Fact           string
	SourceURL      string
	Provider       string
	CachedValue    string
	CurrentValue   string
	CheckedAt      time.Time // when the evidence was fetched (may be up to the evidence TTL old)
	EvidenceCached bool
	AgeKnown       bool
	Age            time.Duration

	fact        *policy.Fact
	current     extract.Value
	haveCurrent bool
}

// Decide evaluates one cache hit. It never panics on bad input and never
// returns Confirmed without a successful evidence comparison.
func (e *Engine) Decide(ctx context.Context, in Input) Decision {
	p := in.Policy
	d := Decision{Categories: p.Categories, AgeKnown: in.AgeKnown, Age: in.Age}
	age := "unknown"
	if in.AgeKnown {
		age = in.Age.Round(time.Second).String()
	}
	onUnknown := p.OnUnknown
	if onUnknown == policy.UnknownAuto || onUnknown == "" {
		if len(p.Facts) > 0 {
			onUnknown = policy.UnknownRevalidate
		} else {
			onUnknown = policy.UnknownServeWithWarning
		}
	}

	if len(p.Facts) == 0 {
		switch {
		case in.Change != nil:
			d.Outcome, d.Action = Stale, Refresh
			when := "at an unknown time"
			if in.AgeKnown {
				when = age + " ago"
			}
			d.Reason = fmt.Sprintf("a change notice for %s arrived at %s, after this entry was written (%s); no watched fact applies, so the entry is treated as stale",
				in.Change.Subject, in.Change.At.UTC().Format(time.RFC3339), when)
		case in.AgeKnown && in.Age <= p.MaxAge:
			d.Outcome, d.Action = Unknown, Serve
			d.Reason = fmt.Sprintf("served on policy only: entry age %s is within max_age %s; no watched fact applies, so nothing was checked against evidence", age, p.MaxAge)
		case in.AgeKnown:
			d.Outcome, d.Action = UnableToVerify, unverifiableAction(p.OnUnverifiable)
			d.Reason = fmt.Sprintf("entry age %s exceeds max_age %s and no watched fact applies to verify it (on_unverifiable=%s)", age, p.MaxAge, p.OnUnverifiable)
		case onUnknown == policy.UnknownBypass:
			d.Outcome, d.Action = Unknown, Refresh
			d.Reason = "entry age unknown (cache id never seen on a miss) and no watched fact applies (on_unknown=bypass)"
		case onUnknown == policy.UnknownRevalidate:
			d.Outcome, d.Action = UnableToVerify, unverifiableAction(p.OnUnverifiable)
			d.Reason = fmt.Sprintf("entry age unknown and on_unknown=revalidate, but no watched fact applies, so there is nothing to check (on_unverifiable=%s)", p.OnUnverifiable)
		default:
			d.Outcome, d.Action = Unknown, ServeWithWarning
			d.Reason = "entry age unknown (cache id never seen on a miss) and no watched fact applies; served with a warning"
		}
		return d
	}

	if !in.AgeKnown {
		switch onUnknown {
		case policy.UnknownServeWithWarning:
			d.Outcome, d.Action = Unknown, ServeWithWarning
			d.Reason = "entry age unknown and on_unknown=serve_with_warning, so the watched fact was not checked"
			return d
		case policy.UnknownBypass:
			d.Outcome, d.Action = Unknown, Refresh
			d.Reason = "entry age unknown and on_unknown=bypass, so the watched fact was not checked"
			return d
		}
	}

	answer, answerErr := openai.AnswerText(in.CachedBody)
	var chosen factResult
	have := false
	for _, f := range p.Facts {
		r := e.checkFact(ctx, f, answer, answerErr)
		if !have || severity(r.outcome) > severity(chosen.outcome) {
			chosen, have = r, true
		}
		if chosen.outcome == Stale {
			break // the answer will be refreshed anyway; skip the remaining facts
		}
	}

	d.Outcome = chosen.outcome
	d.fact = chosen.fact
	d.Fact = chosen.fact.Name
	d.SourceURL = chosen.fact.SourceURL
	if chosen.fetched {
		d.Provider = chosen.provider
		d.CheckedAt = chosen.checkedAt
		d.EvidenceCached = chosen.evidenceCached
	}
	if chosen.haveCached {
		d.CachedValue = chosen.cached.Raw
	}
	if chosen.haveCurrent {
		d.CurrentValue = chosen.current.Raw
		d.current, d.haveCurrent = chosen.current, true
	}
	reason := chosen.reason
	switch d.Outcome {
	case Confirmed:
		d.Action = Serve
		if len(p.Facts) > 1 {
			reason += fmt.Sprintf("; all %d watched facts match", len(p.Facts))
		}
	case Stale:
		d.Action = Refresh
	default:
		d.Action = unverifiableAction(p.OnUnverifiable)
		reason += fmt.Sprintf(" (on_unverifiable=%s)", p.OnUnverifiable)
	}
	if in.AgeKnown {
		reason += "; entry age " + age
	} else {
		reason += "; entry age unknown, so it was revalidated against evidence"
	}
	if in.Change != nil {
		reason += fmt.Sprintf("; a change notice for %s arrived at %s, after this entry was written, so the evidence was re-checked", in.Change.Subject, in.Change.At.UTC().Format(time.RFC3339))
	}
	d.Reason = reason
	return d
}

// CheckNewAnswer checks a fresh answer (a cache miss that Bifrost has just
// stored) against the watched facts in p, before any later request reuses
// it. Every fact is checked, whatever its age, and the outcome is
// confirmed, unable_to_verify or contradicted. A contradicted answer gets
// the action of p.OnContradicted; anything else is served, labelled.
func (e *Engine) CheckNewAnswer(ctx context.Context, p policy.Effective, body []byte) Decision {
	d := Decision{Categories: p.Categories, AgeKnown: true}
	if len(p.Facts) == 0 {
		d.Outcome, d.Action = Unknown, Serve
		d.Reason = "new answer: no watched fact applies, so nothing was checked"
		return d
	}
	answer, answerErr := openai.AnswerText(body)
	var chosen factResult
	have := false
	for _, f := range p.Facts {
		r := e.checkFact(ctx, f, answer, answerErr)
		if !have || severity(r.outcome) > severity(chosen.outcome) {
			chosen, have = r, true
		}
		if chosen.outcome == Stale {
			break
		}
	}
	d.Outcome = chosen.outcome
	d.fact = chosen.fact
	d.Fact = chosen.fact.Name
	d.SourceURL = chosen.fact.SourceURL
	if chosen.fetched {
		d.Provider = chosen.provider
		d.CheckedAt = chosen.checkedAt
		d.EvidenceCached = chosen.evidenceCached
	}
	if chosen.haveCached {
		d.CachedValue = chosen.cached.Raw
	}
	if chosen.haveCurrent {
		d.CurrentValue = chosen.current.Raw
		d.current, d.haveCurrent = chosen.current, true
	}
	f := chosen.fact
	var reason string
	switch d.Outcome {
	case Stale:
		d.Outcome = Contradicted
		d.Action = ContradictedAction(p.OnContradicted)
		reason = fmt.Sprintf("new answer: fact %q: the answer states %s but the source states %s (%s compare; on_contradicted=%s)",
			f.Name, chosen.cached.Raw, chosen.current.Raw, f.Compare, p.OnContradicted)
	case Confirmed:
		d.Action = Serve
		reason = fmt.Sprintf("new answer: fact %q: value %s matches current value %s (%s compare)", f.Name, chosen.cached.Raw, chosen.current.Raw, f.Compare)
		if len(p.Facts) > 1 {
			reason += fmt.Sprintf("; all %d watched facts match", len(p.Facts))
		}
	default:
		// checkFact speaks of "the cached answer"; here it is the new one.
		d.Action = Serve
		reason = "new answer: " + strings.ReplaceAll(chosen.reason, "the cached answer", "the new answer")
	}
	d.Reason = reason
	return d
}

// severity orders fact outcomes: one stale fact makes the answer stale, and
// one unverifiable fact prevents confirmation.
func severity(o Outcome) int {
	switch o {
	case Stale:
		return 2
	case UnableToVerify:
		return 1
	}
	return 0
}

func unverifiableAction(o policy.OnUnverifiable) Action {
	if o == policy.UnverifiableServeWithWarning {
		return ServeWithWarning
	}
	return Refresh // bypass, and the fail-safe default
}

type factResult struct {
	fact           *policy.Fact
	outcome        Outcome
	reason         string
	fetched        bool
	provider       string
	checkedAt      time.Time
	evidenceCached bool
	cached         extract.Value
	haveCached     bool
	current        extract.Value
	haveCurrent    bool
}

// checkFact compares one watched fact. The cached answer is read first: if
// it states no value, the evidence provider is not called at all.
func (e *Engine) checkFact(ctx context.Context, f *policy.Fact, answer string, answerErr error) factResult {
	r := factResult{fact: f, outcome: UnableToVerify}
	if answerErr != nil {
		r.reason = fmt.Sprintf("fact %q: cannot read the cached answer: %s", f.Name, answerErr)
		return r
	}
	cached, err := extract.Extract(f.AnswerRegexp(), answer, f.Compare)
	if err != nil {
		r.reason = fmt.Sprintf("fact %q: answer_pattern %s in the cached answer", f.Name, describe(err))
		return r
	}
	r.cached, r.haveCached = cached, true

	if e.Evidence == nil {
		r.reason = fmt.Sprintf("fact %q: no evidence provider configured", f.Name)
		return r
	}
	r.fetched, r.provider = true, e.Evidence.ProviderName()
	res, err := e.Evidence.Get(ctx, f.SourceURL)
	if err != nil {
		r.reason = fmt.Sprintf("fact %q: evidence fetch failed: %s", f.Name, oneLine(err.Error(), 200))
		return r
	}
	if res.Evidence.Provider != "" {
		r.provider = res.Evidence.Provider
	}
	r.checkedAt, r.evidenceCached = res.Evidence.FetchedAt, res.Cached
	current, err := extract.Extract(f.SourceRegexp(), res.Evidence.Content, f.Compare)
	if err != nil {
		r.reason = fmt.Sprintf("fact %q: source_pattern %s in the evidence", f.Name, describe(err))
		return r
	}
	r.current, r.haveCurrent = current, true
	if extract.Equal(cached, current, f.Compare) {
		r.outcome = Confirmed
		r.reason = fmt.Sprintf("fact %q: cached value %s matches current value %s (%s compare)", f.Name, cached.Raw, current.Raw, f.Compare)
	} else {
		r.outcome = Stale
		r.reason = fmt.Sprintf("fact %q: cached value %s differs from current value %s (%s compare)", f.Name, cached.Raw, current.Raw, f.Compare)
	}
	return r
}

func describe(err error) string {
	var conflict *extract.ConflictError
	switch {
	case errors.Is(err, extract.ErrNoMatch):
		return "matched nothing"
	case errors.As(err, &conflict):
		return "found conflicting values (" + strings.Join(conflict.Values, ", ") + ")"
	default:
		return "failed: " + oneLine(err.Error(), 200)
	}
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}

// Results of comparing a refreshed answer with the evidence behind a decision.
const (
	RefreshedMatches = "matches_current"
	RefreshedDiffers = "differs_from_current"
	RefreshedNoValue = "no_value"
)

// CheckRefreshed compares the value stated in a refreshed answer with the
// current evidence value used for d (no new evidence fetch). ok is false
// when d had no current value, so there is nothing to compare. A refreshed
// answer that still differs is reported, not hidden: refreshing the cache
// does not make the model itself know the current value.
func CheckRefreshed(d Decision, body []byte) (value, result string, ok bool) {
	if d.fact == nil || !d.haveCurrent {
		return "", "", false
	}
	text, err := openai.AnswerText(body)
	if err != nil {
		return "", RefreshedNoValue, true
	}
	v, err := extract.Extract(d.fact.AnswerRegexp(), text, d.fact.Compare)
	if err != nil {
		return "", RefreshedNoValue, true
	}
	if extract.Equal(v, d.current, d.fact.Compare) {
		return v.Raw, RefreshedMatches, true
	}
	return v.Raw, RefreshedDiffers, true
}

// CorrectionPrompt returns the system message a correction puts in front of
// the conversation when it asks the model again: the current value in the
// source's own words (the whole source_pattern match, so the pattern bounds
// what is quoted; one line, at most 200 characters), where it came from and
// when it was checked. Nothing else from the evidence reaches the model.
// ok is false when d has no current value to correct with.
func CorrectionPrompt(d Decision) (string, bool) {
	if d.fact == nil || !d.haveCurrent {
		return "", false
	}
	stated := d.current.Match
	if stated == "" {
		stated = d.fact.Name + ": " + d.current.Raw
	}
	checked := "just now"
	if !d.CheckedAt.IsZero() {
		checked = "at " + d.CheckedAt.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("Verified current information, checked against the live source %s. "+
		"Use it in your answer; it replaces anything else in this conversation that disagrees:\n- %s (source: %s)",
		checked, oneLine(stated, 200), d.SourceURL), true
}
