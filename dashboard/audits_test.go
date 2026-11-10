package main

import (
	"fmt"
	"strings"
	"testing"
)

func f(v float64) *float64 { return &v }

func rec(id, verdict string, sim float64) AuditRecord {
	return AuditRecord{ID: id, Verdict: verdict, Similarity: f(sim), Threshold: f(0.80), Compare: "exact"}
}

// newestFirst builds the envelope the companion serves from records given
// oldest first.
func newestFirst(oldestFirst ...AuditRecord) auditsEnvelope {
	out := make([]AuditRecord, len(oldestFirst))
	for i, r := range oldestFirst {
		out[len(oldestFirst)-1-i] = r
	}
	return auditsEnvelope{Enabled: true, Compare: "exact", Audits: out}
}

func TestAnalyzeAuditsDisabledOrEmpty(t *testing.T) {
	a := analyzeAudits(auditsEnvelope{})
	if a.Enabled || a.Judged != 0 || a.Advice != nil || a.Drift != nil || a.Points == nil || a.Audits == nil {
		t.Fatalf("empty analysis = %+v", a)
	}
	// Errors are counted but judge nothing.
	a = analyzeAudits(newestFirst(AuditRecord{ID: "e", Verdict: "error"}))
	if a.Errors != 1 || a.Judged != 0 || a.Advice != nil {
		t.Fatalf("analysis = %+v", a)
	}
}

func TestAdviceWhenEverythingWasRight(t *testing.T) {
	a := analyzeAudits(newestFirst(rec("1", "same", 0.93), rec("2", "same", 0.84)))
	if a.Judged != 2 || a.WrongRate != 0 || a.Advice == nil || a.Advice.Kind != "ok" || !strings.Contains(a.Advice.Message, "0.84") {
		t.Fatalf("analysis = %+v / %+v", a, a.Advice)
	}
	if !a.EarlySignal {
		t.Fatal("2 audits is an early signal")
	}
	if a.Threshold == nil || *a.Threshold != 0.80 {
		t.Fatalf("threshold = %v", a.Threshold)
	}
}

func TestAdviceRaisesTheThresholdJustAboveTheWorstWrongMatch(t *testing.T) {
	a := analyzeAudits(newestFirst(
		rec("1", "same", 0.93), rec("2", "different", 0.8394), rec("3", "same", 0.8866),
		rec("4", "different", 0.8371), rec("5", "same", 0.8408), rec("6", "same", 0.82)))
	adv := a.Advice
	if adv == nil || adv.Kind != "raise" || adv.SuggestedThreshold == nil || *adv.SuggestedThreshold != 0.84 {
		t.Fatalf("advice = %+v", adv)
	}
	if adv.WrongBlocked != 2 || adv.WrongTotal != 2 || adv.RightLost != 1 || adv.RightTotal != 4 {
		t.Fatalf("counts = %+v", adv)
	}
	if adv.NextRight == nil || *adv.NextRight != 0.8408 || !strings.Contains(adv.Message, "thin margin") {
		t.Fatalf("a good hit 0.0008 above the suggestion is a thin margin: %+v", adv)
	}
	if a.Different != 2 || a.Same != 4 || a.WrongRate < 0.333 || a.WrongRate > 0.334 {
		t.Fatalf("rates = %+v", a)
	}
}

func TestAdviceIsStrictlyAboveAWrongMatchOnAnExactStep(t *testing.T) {
	a := analyzeAudits(newestFirst(rec("1", "different", 0.84), rec("2", "same", 0.9)))
	if *a.Advice.SuggestedThreshold != 0.85 {
		t.Fatalf("a wrong match at exactly 0.84 needs a cutoff above it, got %v", *a.Advice.SuggestedThreshold)
	}
	if a.Advice.RightLost != 0 {
		t.Fatalf("advice = %+v", a.Advice)
	}
}

func TestAdviceSaysWhenNoThresholdCanHelp(t *testing.T) {
	a := analyzeAudits(newestFirst(rec("1", "same", 0.93), rec("2", "different", 1.0)))
	if a.Advice.Kind != "unfixable" || a.Advice.SuggestedThreshold != nil || a.Advice.Inseparable != 1 || !strings.Contains(a.Advice.Message, "no threshold fixes") {
		t.Fatalf("advice = %+v", a.Advice)
	}
	mixed := analyzeAudits(newestFirst(rec("1", "same", 0.93), rec("2", "different", 0.84), rec("3", "different", 1.0)))
	adv := mixed.Advice
	if adv.Kind != "raise_partial" || adv.WrongBlocked != 1 || adv.WrongTotal != 2 || adv.Inseparable != 1 ||
		!strings.Contains(adv.Message, "blocked 1 of 2") || !strings.Contains(adv.Message, "no threshold fixes") {
		t.Fatalf("advice = %+v", adv)
	}
}

func TestAdviceWithoutScoresStaysHonest(t *testing.T) {
	a := analyzeAudits(newestFirst(AuditRecord{ID: "1", Verdict: "different"}, AuditRecord{ID: "2", Verdict: "same"}))
	if a.Judged != 2 || a.Advice == nil || a.Advice.SuggestedThreshold != nil {
		t.Fatalf("with no similarity there is nothing to base a cutoff on: %+v", a.Advice)
	}
}

func windowOf(wrong, total int, prefix string) []AuditRecord {
	var out []AuditRecord
	for i := 0; i < total; i++ {
		verdict := "same"
		if i < wrong {
			verdict = "different"
		}
		out = append(out, rec(fmt.Sprintf("%s%d", prefix, i), verdict, 0.9))
	}
	return out
}

func TestDriftNeedsTwoWindowsAndARealRise(t *testing.T) {
	a := analyzeAudits(newestFirst(append(windowOf(0, 10, "a"), windowOf(5, 10, "b")...)...))
	if a.Drift == nil || a.Drift.Earlier != 0 || a.Drift.Latest != 0.5 || !strings.Contains(a.Drift.Message, "rose from 0% to 50%") {
		t.Fatalf("drift = %+v", a.Drift)
	}

	// A one-answer wobble between windows is noise, not drift. It is also
	// above the 5% tolerance, so it is reported as that and not as a rise.
	a = analyzeAudits(newestFirst(append(windowOf(0, 10, "a"), windowOf(1, 10, "b")...)...))
	if a.Drift == nil || strings.Contains(a.Drift.Message, "rose") || !a.Drift.Tolerate {
		t.Fatalf("drift = %+v", a.Drift)
	}

	// Steady and clean: nothing to say.
	a = analyzeAudits(newestFirst(append(windowOf(0, 10, "a"), windowOf(0, 10, "b")...)...))
	if a.Drift != nil {
		t.Fatalf("drift = %+v", a.Drift)
	}

	// Too few audits for a window: no warning, however bad.
	if a = analyzeAudits(newestFirst(windowOf(5, 9, "c")...)); a.Drift != nil {
		t.Fatalf("9 audits is below one window, drift = %+v", a.Drift)
	}

	// One window above tolerance, no earlier window to compare with.
	a = analyzeAudits(newestFirst(windowOf(3, 10, "d")...))
	if a.Drift == nil || a.Drift.Message == "" || !strings.Contains(a.Drift.Message, "tolerance") {
		t.Fatalf("drift = %+v", a.Drift)
	}
}

func TestPointsAreOldestFirst(t *testing.T) {
	a := analyzeAudits(newestFirst(rec("1", "same", 0.9), rec("2", "different", 0.85)))
	if len(a.Points) != 2 || a.Points[0].ID != "1" || a.Points[1].ID != "2" {
		t.Fatalf("points = %+v", a.Points)
	}
}

func gateRec(id, verdict string, sim float64) AuditRecord {
	r := rec(id, verdict, sim)
	r.Gate = true
	return r
}

func TestGateChecksAreCountedApartFromServedAudits(t *testing.T) {
	a := analyzeAudits(newestFirst(
		gateRec("g1", "different", 0.84), gateRec("g2", "same", 0.85), AuditRecord{ID: "g3", Verdict: "error", Gate: true},
		rec("1", "same", 0.95), AuditRecord{ID: "2", Verdict: "error"}))
	if a.Judged != 1 || a.Same != 1 || a.Different != 0 || a.Errors != 1 || a.WrongRate != 0 {
		t.Fatalf("served side = %+v", a)
	}
	if a.GateJudged != 2 || a.GateSame != 1 || a.GateDifferent != 1 || a.GateErrors != 1 {
		t.Fatalf("gate side = %+v", a)
	}
	if len(a.Points) != 3 || !a.Points[0].Gate || a.Points[2].Gate {
		t.Fatalf("points = %+v", a.Points)
	}
}

func TestAdviceUsesBothKindsAndSaysHowManyOfEach(t *testing.T) {
	a := analyzeAudits(newestFirst(gateRec("g1", "different", 0.84), rec("1", "same", 0.93), rec("2", "same", 0.95)))
	adv := a.Advice
	if adv == nil || adv.Kind != "raise" || adv.WrongTotal != 1 || adv.RightTotal != 2 || *adv.SuggestedThreshold != 0.85 {
		t.Fatalf("advice = %+v", adv)
	}
	if adv.SamplesServed != 2 || adv.SamplesGate != 1 || !strings.Contains(adv.Message, "2 audits of answers already served and 1 checks made before serving") {
		t.Fatalf("samples = %d/%d, message %q", adv.SamplesServed, adv.SamplesGate, adv.Message)
	}
}

func TestOnlyGateChecksStillGiveAdviceButNoServedRate(t *testing.T) {
	a := analyzeAudits(newestFirst(gateRec("g1", "same", 0.85)))
	if a.Judged != 0 || a.WrongRate != 0 || a.Drift != nil || a.Advice == nil || a.Advice.SamplesGate != 1 || a.Advice.SamplesServed != 0 {
		t.Fatalf("analysis = %+v / %+v", a, a.Advice)
	}
}

func TestDriftIgnoresBlockedGateChecks(t *testing.T) {
	var rs []AuditRecord
	for i := 0; i < 10; i++ {
		rs = append(rs, gateRec(fmt.Sprint("g", i), "different", 0.84), rec(fmt.Sprint("s", i), "same", 0.95))
	}
	if a := analyzeAudits(newestFirst(rs...)); a.Drift != nil || a.WrongRate != 0 {
		t.Fatalf("blocked wrong matches never reached users, so there is no drift: %+v", a.Drift)
	}
}
