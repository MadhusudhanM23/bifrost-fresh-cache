package main

import (
	"strings"
	"testing"
)

func TestSummaryBadStoppedDoesNotDoubleCount(t *testing.T) {
	d := computeDecisionStats([]DecisionRecord{
		{Outcome: "stale", Action: "refreshed"},
		{Outcome: "stale", Action: "refreshed", Correction: "corrected"}, // one record: counted once
		{Outcome: "contradicted", AnswerOf: "new", Correction: "corrected"},
		{Outcome: "contradicted", AnswerOf: "new", Correction: "uncached"},
		{Outcome: "contradicted", AnswerOf: "new", Correction: "reported"}, // reported only: not stopped
		{Outcome: "confirmed"},
	}, 2)
	a := AuditAnalysis{Stats: AuditStats{GateBlocked: 3}}
	s := computeSummary(d, a, GuardView{}, 0)
	b := s.BadStopped
	if b.StaleRefreshed != 2 || b.WrongNewFixed != 2 || b.GateBlocked != 3 || b.Total != 7 {
		t.Fatalf("bad stopped = %+v", b)
	}
}

func TestSummaryReachedUsersUsesOnlyPostServeAudits(t *testing.T) {
	// Two blocked look-alikes must not count as wrong answers that reached users.
	a := analyzeAudits(newestFirst(
		AuditRecord{ID: "g1", Verdict: "different", Gate: true, Similarity: f(0.84)},
		AuditRecord{ID: "g2", Verdict: "different", Gate: true, Similarity: f(0.85)},
		rec("1", "same", 0.95), rec("2", "different", 0.97), rec("3", "same", 0.96), rec("4", "same", 0.99)))
	s := computeSummary(DecisionStats{}, a, GuardView{}, 0)
	r := s.ReachedUsers
	if r.Count != 1 || r.Audited != 4 || r.Share != 0.25 || !r.Early {
		t.Fatalf("reached users = %+v", r)
	}
	if a.GateDifferent != 2 || a.GateJudged != 2 || a.Different != 1 || a.Same != 3 || a.WrongRate != 0.25 {
		t.Fatalf("analysis = %+v", a)
	}
}

func TestSummaryReachedUsersWithNothingAudited(t *testing.T) {
	s := computeSummary(DecisionStats{}, AuditAnalysis{}, GuardView{}, 0)
	if s.ReachedUsers.Share != 0 || s.ReachedUsers.Count != 0 || s.ReachedUsers.Audited != 0 {
		t.Fatalf("reached users = %+v", s.ReachedUsers)
	}
	if s.Saving.Plain != "Nothing to count yet." || s.Saving.Net != 0 || s.Saving.Negative {
		t.Fatalf("saving = %+v", s.Saving)
	}
}

func TestSummaryStampedesAndGuard(t *testing.T) {
	d := computeDecisionStats([]DecisionRecord{{SharedRefresh: true}, {SharedRefresh: true}, {}}, 2)
	g := GuardView{Available: true, GuardStats: GuardStats{Enabled: true, Bypassed: 3, ByReason: map[string]int64{"tool_results": 2, "side_effect_tool": 1}}}
	s := computeSummary(d, AuditAnalysis{}, g, 0)
	if s.Stampedes != 2 || s.Guard.Bypassed != 3 || s.Guard.ByReason["tool_results"] != 2 || !s.Guard.Available {
		t.Fatalf("summary = %+v", s)
	}
	// A missing guard must still give a usable (empty) map.
	if got := computeSummary(d, AuditAnalysis{}, GuardView{}, 0).Guard.ByReason; got == nil {
		t.Fatal("ByReason must never be nil")
	}
}

func TestSummaryNetSavingFormula(t *testing.T) {
	d := DecisionStats{SharedRefreshes: 2, Corrected: 1}
	a := AuditAnalysis{Compare: "exact", GateErrors: 1, Stats: AuditStats{Completed: 4, GateVerified: 3, GateBlocked: 2}}
	s := computeSummary(d, a, GuardView{}, 10)
	v := s.Saving
	// avoided = 10 hits + 2 stampede waits; extra = 4 audits + (3 verified + 1 failed run; blocked are free) + 1 correction
	if v.Avoided != 12 || v.PostServeCalls != 4 || v.GateCalls != 4 || v.CorrectCalls != 1 || v.Extra != 9 || v.Net != 3 || v.Negative {
		t.Fatalf("saving = %+v", v)
	}
	if !strings.Contains(v.Plain, "saved 3") {
		t.Fatalf("plain = %q", v.Plain)
	}
}

func TestSummaryJudgeModeDoublesChecks(t *testing.T) {
	a := AuditAnalysis{Compare: "judge", Stats: AuditStats{Completed: 4, GateVerified: 1, GateBlocked: 1}}
	v := computeSummary(DecisionStats{}, a, GuardView{}, 20).Saving
	// post-serve 4x2 = 8; gate: 2 runs, a blocked run's answer is free but its judge call is not: (2-1)*1 + 2*1 = 3
	if v.JudgeFactor != 2 || v.PostServeCalls != 8 || v.GateCalls != 3 || v.Extra != 11 || v.Net != 9 {
		t.Fatalf("saving = %+v", v)
	}
}

func TestSummaryNegativeSavingIsSaidPlainly(t *testing.T) {
	a := AuditAnalysis{Compare: "exact", Stats: AuditStats{Completed: 5, GateVerified: 3}}
	v := computeSummary(DecisionStats{}, a, GuardView{}, 2).Saving
	if v.Net != -6 || !v.Negative {
		t.Fatalf("saving = %+v", v)
	}
	if !strings.Contains(v.Plain, "6 more model calls than the cache saved") {
		t.Fatalf("plain = %q", v.Plain)
	}
}

func TestSummaryBreakEven(t *testing.T) {
	a := AuditAnalysis{Stats: AuditStats{Completed: 2}}
	v := computeSummary(DecisionStats{}, a, GuardView{}, 2).Saving
	if v.Net != 0 || v.Negative || !strings.Contains(v.Plain, "exactly as many") {
		t.Fatalf("saving = %+v", v)
	}
}

func TestSummaryGateView(t *testing.T) {
	a := AuditAnalysis{Stats: AuditStats{GateVerified: 1, GateTrusted: 2, GateBlocked: 3, GateUnverified: 4}}
	g := computeSummary(DecisionStats{}, a, GuardView{}, 0).Gate
	if g.Verified != 1 || g.Trusted != 2 || g.Blocked != 3 || g.Unverified != 4 || !g.Active {
		t.Fatalf("gate = %+v", g)
	}
	if computeSummary(DecisionStats{}, AuditAnalysis{}, GuardView{}, 0).Gate.Active {
		t.Fatal("no gate activity must not be active")
	}
}
