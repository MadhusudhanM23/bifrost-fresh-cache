package main

import "fmt"

// GuardStats mirrors GET /freshness/guard: requests kept out of the cache on
// purpose because their answer depends on live tool data or may skip a real
// action. Only counts are kept, never prompts or tool names.
type GuardStats struct {
	Enabled  bool             `json:"enabled"`
	Since    string           `json:"since,omitempty"`
	Bypassed int64            `json:"bypassed"`
	ByReason map[string]int64 `json:"by_reason"`
}

// GuardView is GuardStats plus whether the companion could be asked at all
// (an older companion has no /freshness/guard).
type GuardView struct {
	Available bool `json:"available"`
	GuardStats
}

// BadStopped counts bad answers that never reached a user. Each decision
// record is counted once: a stale answer whose refresh also needed a
// correction is in StaleRefreshed only.
type BadStopped struct {
	Total          int `json:"total"`
	StaleRefreshed int `json:"stale_refreshed"`
	// WrongNewFixed: wrong new answers corrected or removed from the cache.
	WrongNewFixed int `json:"wrong_new_fixed"`
	// GateBlocked: wrong look-alike matches stopped by the check before serving.
	GateBlocked int `json:"gate_blocked"`
}

// ReachedUsers is the wrong answers audits found AFTER they were served.
type ReachedUsers struct {
	Count   int `json:"count"`
	Audited int `json:"audited"`
	// Share is Count/Audited (0 when nothing was audited).
	Share float64 `json:"share"`
	// Early is true under earlySignalBelow audits.
	Early bool `json:"early"`
}

// GateView is what the check before serving did, counted by the companion.
type GateView struct {
	Verified   int64 `json:"verified"`
	Trusted    int64 `json:"trusted"`
	Blocked    int64 `json:"blocked"`
	Unverified int64 `json:"unverified"`
	// Active is true when the gate ever acted (any count above zero).
	Active bool `json:"active"`
}

// SavingBreakdown shows every term of the net saving, in model calls.
type SavingBreakdown struct {
	// Avoided
	CacheHitsServed int `json:"cache_hits_served"`
	StampedeWaits   int `json:"stampede_waits"`
	Avoided         int `json:"avoided"`
	// Extra calls we add ourselves
	JudgeFactor    int `json:"judge_factor"` // 1 for exact compare, 2 for judge
	PostServeCalls int `json:"post_serve_calls"`
	GateCalls      int `json:"gate_calls"`
	CorrectCalls   int `json:"correct_calls"`
	Extra          int `json:"extra"`
	// Net = Avoided - Extra. It can be negative.
	Net      int  `json:"net"`
	Negative bool `json:"negative"`
	// Plain is one sentence a non-expert can read.
	Plain string `json:"plain"`
}

// Summary is the one-glance health summary served by GET /api/summary.
type Summary struct {
	BadStopped   BadStopped      `json:"bad_stopped"`
	ReachedUsers ReachedUsers    `json:"reached_users"`
	Stampedes    int             `json:"stampedes"`
	Guard        GuardView       `json:"guard"`
	Gate         GateView        `json:"gate"`
	Saving       SavingBreakdown `json:"saving"`
}

// computeSummary combines what the dashboard already has: the decision
// statistics, the audit analysis, the guard counters and its own count of
// cache hits. It is pure so it can be unit tested without a live stack.
//
// Net saving, in model calls:
//
//	avoided = cache hits served from cache + requests that shared a refresh
//	extra   = completed audits x f + gate runs x f + corrections
//	net     = avoided - extra          (f is 2 when answers are compared by a judge)
//
// A hit that was refreshed or blocked arrives at the dashboard as a fresh
// answer, not as a cache hit, so cacheHits already leaves it out. A blocked
// check's own fresh answer is the answer the user received (a cache-less
// system would have made that call too), so it is not an extra call; only
// its judge call, if any, is.
func computeSummary(d DecisionStats, a AuditAnalysis, g GuardView, cacheHits int) Summary {
	var s Summary

	gateBlocked := int(a.Stats.GateBlocked)
	s.BadStopped = BadStopped{
		StaleRefreshed: d.StaleStopped,
		WrongNewFixed:  d.WrongNewStopped,
		GateBlocked:    gateBlocked,
	}
	s.BadStopped.Total = s.BadStopped.StaleRefreshed + s.BadStopped.WrongNewFixed + s.BadStopped.GateBlocked

	s.ReachedUsers = ReachedUsers{Count: a.Different, Audited: a.Judged, Early: a.Judged < earlySignalBelow}
	if a.Judged > 0 {
		s.ReachedUsers.Share = float64(a.Different) / float64(a.Judged)
	}

	s.Stampedes = d.SharedRefreshes
	s.Guard = g
	if s.Guard.ByReason == nil {
		s.Guard.ByReason = map[string]int64{}
	}
	s.Gate = GateView{
		Verified: a.Stats.GateVerified, Trusted: a.Stats.GateTrusted,
		Blocked: a.Stats.GateBlocked, Unverified: a.Stats.GateUnverified,
	}
	s.Gate.Active = s.Gate.Verified+s.Gate.Trusted+s.Gate.Blocked+s.Gate.Unverified > 0

	v := SavingBreakdown{JudgeFactor: 1}
	if a.Compare == "judge" {
		v.JudgeFactor = 2
	}
	v.CacheHitsServed = cacheHits
	v.StampedeWaits = d.SharedRefreshes
	v.Avoided = v.CacheHitsServed + v.StampedeWaits

	v.PostServeCalls = int(a.Stats.Completed) * v.JudgeFactor
	// Gate runs that asked the model: verified, blocked, and the unverified
	// ones that got as far as a logged attempt (not those stopped by budget).
	gateRuns := int(a.Stats.GateVerified) + gateBlocked + a.GateErrors
	// A blocked run's fresh answer was the user's answer, not an extra call.
	v.GateCalls = (gateRuns-gateBlocked)*1 + gateRuns*(v.JudgeFactor-1)
	v.CorrectCalls = d.Corrected
	v.Extra = v.PostServeCalls + v.GateCalls + v.CorrectCalls

	v.Net = v.Avoided - v.Extra
	v.Negative = v.Net < 0
	switch {
	case v.Avoided == 0 && v.Extra == 0:
		v.Plain = "Nothing to count yet."
	case v.Negative:
		v.Plain = fmt.Sprintf("So far the extra checks cost %d more model calls than the cache saved.", -v.Net)
	case v.Net == 0:
		v.Plain = "So far the cache saved exactly as many calls as the extra checks used."
	default:
		v.Plain = fmt.Sprintf("The cache saved %d model calls after paying for its own extra checks.", v.Net)
	}
	s.Saving = v
	return s
}
