package proxy

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"freshness-companion/internal/auditlog"
	"freshness-companion/internal/openai"
)

// Gate verdicts, reported in X-Freshness-Gate.
const (
	// GateVerified: the model was asked again and agreed, so the cached answer
	// was served and the pair is trusted from now on.
	GateVerified = "verified"
	// GateTrusted: this question and entry were verified before.
	GateTrusted = "trusted"
	// GateBlocked: the model disagreed with the cached answer. The fresh
	// answer was served instead, and cached for this exact question only, so
	// the look-alike entry cannot be served to it again.
	GateBlocked = "blocked"
	// GateUnverified: the check could not be run (error or budget), so the
	// cached answer was served unchecked.
	GateUnverified = "unverified"
)

// HeaderGate reports what the pre-serve check did for a semantic hit.
const HeaderGate = "X-Freshness-Gate"

// gateVerdict is the outcome of checking one risky semantic hit.
type gateVerdict struct {
	kind    string    // one of the Gate* constants
	fresh   *Response // the model's own answer (cached for the exact question), when kind is blocked
	auditID string
	err     string
}

// gateApplies reports whether a hit must be verified before it is served:
// only semantic hits can be wrong matches, and only those scoring below
// VerifyBelow (a near-miss is where look-alike questions and planted
// near-duplicates match).
func (a *auditor) gateApplies(cached *Response) (sim float64, ok bool) {
	if a == nil || a.cfg.VerifyBelow <= 0 || !cached.IsSemanticHit() {
		return 0, false
	}
	s := parseScore(cached.Header, HeaderCacheSimilarity)
	if s == nil || *s >= a.cfg.VerifyBelow {
		return 0, false
	}
	return *s, true
}

// gate verifies a risky semantic hit before it is served. The caller serves
// cached for every verdict except GateBlocked, where it serves v.fresh.
// An empty kind means the gate does not apply to this hit.
func (a *auditor) gate(ctx context.Context, req *Request, question string, cached *Response) gateVerdict {
	sim, ok := a.gateApplies(cached)
	if !ok {
		return gateVerdict{}
	}
	key := askKey(req, cached.CacheID())
	if a.trust.has(key, a.srv.cfg.Now()) {
		a.gateTrusted.Add(1)
		return gateVerdict{kind: GateTrusted}
	}
	v, _, err := a.gateFlights.do(ctx, key, func() gateVerdict {
		return a.verify(ctx, req, question, cached, sim, key)
	})
	if err != nil {
		return gateVerdict{kind: GateUnverified, err: oneLine(err.Error(), 200)}
	}
	return v
}

// verify asks the model the question again, bypassing the cache, and compares
// the two answers. One run is shared by requests that arrive together.
func (a *auditor) verify(parent context.Context, req *Request, question string, cached *Response, sim float64, key string) gateVerdict {
	if !a.gateBudget.allow(a.srv.cfg.Now()) {
		a.gateUnverified.Add(1)
		return gateVerdict{kind: GateUnverified, err: "the gate's per-minute budget is used up"}
	}
	id := fmt.Sprintf("gate-%s-%d", a.pref, a.seq.Add(1))
	rec := auditlog.Record{ID: id, Time: a.srv.cfg.Now().UTC(), CacheID: cached.CacheID(), Gate: true, Compare: a.cfg.Compare,
		Similarity: &sim, Threshold: parseScore(cached.Header, HeaderCacheThreshold)}
	start := time.Now()
	finish := func(v gateVerdict) gateVerdict {
		rec.LatencyMS = float64(time.Since(start).Microseconds()) / 1000
		if rec.Verdict == "" {
			rec.Verdict = auditlog.Error
		}
		if err := a.srv.cfg.Audit.Log.Append(rec); err != nil {
			a.srv.log.Warn("could not write the audit log", "error", err)
		}
		v.auditID = id
		return v
	}
	fail := func(format string, args ...any) gateVerdict {
		rec.Error = fmt.Sprintf(format, args...)
		a.gateUnverified.Add(1)
		return finish(gateVerdict{kind: GateUnverified, err: oneLine(rec.Error, 200)})
	}

	// The check must outlive the request that started it: others share it.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), a.cfg.Timeout)
	defer cancel()
	// Ask with the caller's own key but only the exact (direct) path: the
	// look-alike entry is not looked up, and a fresh answer is stored for this
	// exact question, so the next ask is a direct hit and look-alike questions
	// never push each other out of the cache.
	ask := req.Clone()
	ask.Header.Set(HeaderCacheType, "direct")
	fresh, err := a.srv.cfg.Upstream.Send(ctx, ask)
	if err == nil {
		a.srv.observe(ask.Header, fresh.Header)
	}
	switch {
	case err != nil:
		return fail("fresh answer failed: %s", oneLine(err.Error(), 200))
	case fresh.StatusCode != http.StatusOK:
		return fail("fresh answer returned HTTP %d", fresh.StatusCode)
	case fresh.CacheHit():
		return fail("the fresh request was served from cache, so it proves nothing")
	}
	cachedText, err1 := openai.AnswerText(inspectable(cached, a.srv.cfg.MaxResponseBytes))
	freshText, err2 := openai.AnswerText(inspectable(fresh, a.srv.cfg.MaxResponseBytes))
	if err1 != nil || err2 != nil {
		return fail("an answer has no text to compare")
	}
	same, reason, err := a.compare(ctx, req, question, cachedText, freshText)
	if err != nil {
		return fail("%s", oneLine(err.Error(), 200))
	}
	rec.Reason = reason
	if same {
		rec.Verdict = auditlog.Same
		a.trust.add(key, a.srv.cfg.Now())
		a.gateVerified.Add(1)
		return finish(gateVerdict{kind: GateVerified})
	}
	rec.Verdict = auditlog.Different
	a.gateBlocked.Add(1)
	// The matched entry is left alone. It may be perfectly right for its own
	// question (a look-alike was only matched to it), and deleting it would
	// make look-alike questions evict each other for good. Nothing trusts it
	// for this question, so it is never served to it.
	return finish(gateVerdict{kind: GateBlocked, fresh: fresh})
}

// trustSet remembers (entry, caller, question) pairs the gate has verified.
type trustSet struct {
	ttl time.Duration
	max int

	mu      sync.Mutex
	expires map[string]time.Time
}

func newTrustSet(ttl time.Duration, max int) *trustSet {
	return &trustSet{ttl: ttl, max: max, expires: map[string]time.Time{}}
}

func (t *trustSet) has(key string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	exp, ok := t.expires[key]
	if ok && now.After(exp) {
		delete(t.expires, key)
		return false
	}
	return ok
}

func (t *trustSet) add(key string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.expires) >= t.max {
		for k, exp := range t.expires {
			if now.After(exp) {
				delete(t.expires, k)
			}
		}
		for k := range t.expires { // still full: make room
			if len(t.expires) < t.max {
				break
			}
			delete(t.expires, k)
		}
	}
	t.expires[key] = now.Add(t.ttl)
}

// perMinute is a fixed one-minute window counter.
type perMinute struct {
	limit int

	mu    sync.Mutex
	start time.Time
	n     int
}

func (p *perMinute) allow(now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if now.Sub(p.start) >= time.Minute {
		p.start, p.n = now, 0
	}
	if p.n >= p.limit {
		return false
	}
	p.n++
	return true
}
