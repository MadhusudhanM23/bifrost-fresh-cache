package main

import "testing"

// TestComputeDecisionStats_RepeatStaleWarning is the load-bearing test: it
// proves the warning logic actually computes from decision data instead of
// being a UI mockup. A fact caught stale twice must warn; a fact caught
// stale only once must not.
func TestComputeDecisionStats_RepeatStaleWarning(t *testing.T) {
	records := []DecisionRecord{
		{Fact: "Acme Pro plan price", Outcome: "stale", Category: "price"},
		{Fact: "Acme Pro plan price", Outcome: "confirmed", Category: "price"},
		{Fact: "Acme Pro plan price", Outcome: "stale", Category: "price"},
		{Fact: "Support hours", Outcome: "stale", Category: "schedule"},
		{Fact: "Support hours", Outcome: "confirmed", Category: "schedule"},
		{Outcome: "unknown", Category: ""}, // no watched fact: must not appear per-fact
	}

	stats := computeDecisionStats(records, 2)

	if stats.Total != 6 {
		t.Fatalf("Total = %d, want 6", stats.Total)
	}
	if stats.Outcomes["stale"] != 3 {
		t.Fatalf("Outcomes[stale] = %d, want 3", stats.Outcomes["stale"])
	}
	if stats.Outcomes["confirmed"] != 2 {
		t.Fatalf("Outcomes[confirmed] = %d, want 2", stats.Outcomes["confirmed"])
	}
	if stats.Outcomes["unknown"] != 1 {
		t.Fatalf("Outcomes[unknown] = %d, want 1", stats.Outcomes["unknown"])
	}

	if len(stats.Warnings) != 1 {
		t.Fatalf("len(Warnings) = %d, want 1 (only the fact with >=2 stale hits should warn): %+v", len(stats.Warnings), stats.Warnings)
	}
	w := stats.Warnings[0]
	if w.Fact != "Acme Pro plan price" {
		t.Errorf("warning fact = %q, want %q", w.Fact, "Acme Pro plan price")
	}
	if w.StaleCount != 2 {
		t.Errorf("warning stale count = %d, want 2", w.StaleCount)
	}
	if w.Message == "" {
		t.Errorf("warning message is empty")
	}

	// "Support hours" went stale only once: it must not generate a warning.
	for _, w := range stats.Warnings {
		if w.Fact == "Support hours" {
			t.Errorf("Support hours went stale only once but still warned: %+v", w)
		}
	}

	// Per-fact breakdown sanity.
	var acme, hours *FactStat
	for i := range stats.Facts {
		switch stats.Facts[i].Fact {
		case "Acme Pro plan price":
			acme = &stats.Facts[i]
		case "Support hours":
			hours = &stats.Facts[i]
		}
	}
	if acme == nil || acme.Stale != 2 || acme.Confirmed != 1 || acme.Total != 3 {
		t.Errorf("acme fact stat = %+v, want Stale=2 Confirmed=1 Total=3", acme)
	}
	if hours == nil || hours.Stale != 1 || hours.Confirmed != 1 || hours.Total != 2 {
		t.Errorf("hours fact stat = %+v, want Stale=1 Confirmed=1 Total=2", hours)
	}
}

// TestComputeDecisionStats_ThresholdBoundary checks the threshold is
// inclusive (">=") and configurable, not hardcoded to 2.
func TestComputeDecisionStats_ThresholdBoundary(t *testing.T) {
	records := []DecisionRecord{
		{Fact: "X", Outcome: "stale"},
		{Fact: "X", Outcome: "stale"},
		{Fact: "X", Outcome: "stale"},
	}

	if stats := computeDecisionStats(records, 3); len(stats.Warnings) != 1 {
		t.Fatalf("threshold=3 with exactly 3 stale hits: got %d warnings, want 1", len(stats.Warnings))
	}
	if stats := computeDecisionStats(records, 4); len(stats.Warnings) != 0 {
		t.Fatalf("threshold=4 with only 3 stale hits: got %d warnings, want 0", len(stats.Warnings))
	}
	if stats := computeDecisionStats(records, 0); stats.StaleThreshold != defaultStaleThreshold {
		t.Fatalf("threshold=0 should fall back to defaultStaleThreshold, got %d", stats.StaleThreshold)
	}
}

// TestComputeDecisionStats_CorrectionsAndNotices checks the counters for the
// wrong-from-the-start answers and the "this changed" notices.
func TestComputeDecisionStats_CorrectionsAndNotices(t *testing.T) {
	records := []DecisionRecord{
		{Fact: "Pro", Outcome: "contradicted", AnswerOf: "new", Correction: "corrected"},
		{Fact: "Pro", Outcome: "contradicted", AnswerOf: "new", Correction: "uncached"},
		{Fact: "Pro", Outcome: "confirmed", AnswerOf: "new"},
		{Fact: "Pro", Outcome: "stale", AnswerOf: "cached", Correction: "corrected", ChangeNotice: "chg-1"},
		{Fact: "Pro", Outcome: "confirmed", AnswerOf: "cached", ChangeNotice: "chg-1"},
		{Fact: "Pro", Outcome: "confirmed", AnswerOf: "cached"},
	}
	s := computeDecisionStats(records, 2)
	if s.NewChecked != 3 || s.NewContradicted != 2 || s.Corrected != 2 || s.Uncached != 1 || s.NoticeDriven != 2 {
		t.Fatalf("counters = checked %d contradicted %d corrected %d uncached %d notices %d",
			s.NewChecked, s.NewContradicted, s.Corrected, s.Uncached, s.NoticeDriven)
	}
	if s.Outcomes["contradicted"] != 2 || len(s.Facts) != 1 || s.Facts[0].Contradicted != 2 {
		t.Fatalf("outcomes %v facts %+v", s.Outcomes, s.Facts)
	}
}

// TestComputeDecisionStats_Empty guards against a nil-map / index panic when
// the live companion has logged nothing yet.
func TestComputeDecisionStats_Empty(t *testing.T) {
	stats := computeDecisionStats(nil, 2)
	if stats.Total != 0 || len(stats.Warnings) != 0 || len(stats.Facts) != 0 {
		t.Fatalf("empty input produced non-empty stats: %+v", stats)
	}
}

func TestComputeDecisionStats_SharedRefreshAndStopped(t *testing.T) {
	st := computeDecisionStats([]DecisionRecord{
		{Outcome: "stale", SharedRefresh: true},
		{Outcome: "stale", Correction: "corrected"},
		{Outcome: "contradicted", Correction: "uncached"},
		{Outcome: "contradicted", Correction: "reported"},
		{Outcome: "confirmed", Gate: "verified"},
	}, 2)
	if st.SharedRefreshes != 1 || st.StaleStopped != 2 || st.WrongNewStopped != 1 {
		t.Fatalf("stats = %+v", st)
	}
	// The existing counters stay as they were.
	if st.Corrected != 1 || st.Uncached != 1 {
		t.Fatalf("corrected/uncached = %d/%d", st.Corrected, st.Uncached)
	}
}
