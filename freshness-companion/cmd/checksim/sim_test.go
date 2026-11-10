package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Short traces keep the tests fast; the shapes match the default ones.
func shortQuiet() Trace {
	return Trace{Name: "quiet", Duration: 2 * time.Hour, RequestsPerMinute: 3, Changes: []time.Duration{time.Hour}, Questions: 3}
}

func shortSale() Trace {
	return Trace{Name: "sale", Duration: time.Hour, RequestsPerMinute: 6, Changes: []time.Duration{10 * time.Minute, 20 * time.Minute, 30 * time.Minute, 40 * time.Minute, 50 * time.Minute}, Questions: 3}
}

func armsByName(t *testing.T, traces []Trace) map[string]Metrics {
	t.Helper()
	r, err := Report(traces, DefaultArms(), 7, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]Metrics{}
	for _, m := range r.Results {
		out[m.Trace+"/"+m.Arm] = m
	}
	return out
}

func TestSimIsDeterministic(t *testing.T) {
	a, err := Report([]Trace{shortQuiet()}, DefaultArms(), 42, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Report([]Trace{shortQuiet()}, DefaultArms(), 42, time.Unix(0, 0))
	ja, _ := json.Marshal(a.Results)
	jb, _ := json.Marshal(b.Results)
	if string(ja) != string(jb) {
		t.Fatal("the same seed must give the same results")
	}
	c, _ := Report([]Trace{shortQuiet()}, DefaultArms(), 43, time.Unix(0, 0))
	jc, _ := json.Marshal(c.Results)
	if string(ja) == string(jc) {
		t.Fatal("a different seed should change the arrivals")
	}
}

func TestFixedArmsBehaveAsExpected(t *testing.T) {
	m := armsByName(t, []Trace{shortQuiet()})
	f5, f60 := m["quiet/fixed-5s"], m["quiet/fixed-60s"]
	if f5.Requests != f60.Requests || f5.Requests < 200 {
		t.Fatalf("requests differ or too few: %d vs %d", f5.Requests, f60.Requests)
	}
	if f5.EvidenceFetches <= f60.EvidenceFetches {
		t.Fatalf("a 5 s TTL must fetch more than a 60 s TTL: %d vs %d", f5.EvidenceFetches, f60.EvidenceFetches)
	}
	if f5.WorstLagSecs > 60 || f60.WorstLagSecs > 120 {
		t.Fatalf("detection lags: 5s=%v 60s=%v", f5.WorstLagSecs, f60.WorstLagSecs)
	}
	if f5.Detected != 1 || f60.Detected != 1 {
		t.Fatalf("the change must be detected: %+v %+v", f5, f60)
	}
	if f5.WatcherFetches != 0 || f5.Evictions != 0 {
		t.Fatalf("fixed arms have no watcher: %+v", f5)
	}
	if f5.StaleServed > f60.StaleServed {
		t.Fatalf("a shorter TTL must not serve more stale answers: %d vs %d", f5.StaleServed, f60.StaleServed)
	}
}

func TestAdaptiveReducesFetchesOnQuietTrace(t *testing.T) {
	m := armsByName(t, []Trace{shortQuiet()})
	f5, ad := m["quiet/fixed-5s"], m["quiet/adaptive"]
	if ad.EvidenceFetches*2 > f5.EvidenceFetches {
		t.Fatalf("adaptive should at least halve the fetches on a quiet page: %d vs %d", ad.EvidenceFetches, f5.EvidenceFetches)
	}
	if ad.FinalIntervalS != 300 {
		t.Fatalf("after an hour of no changes the interval should sit at the 5 m cap, got %v s", ad.FinalIntervalS)
	}
	if ad.WorstLagSecs > 300 {
		t.Fatalf("the detection lag is bounded by the cap: %v", ad.WorstLagSecs)
	}
	if ad.Detected != 1 {
		t.Fatalf("the change must still be detected: %+v", ad)
	}
}

func TestWatcherEvictsAndRequestsStopWaiting(t *testing.T) {
	m := armsByName(t, []Trace{shortSale()})
	ad, w := m["sale/adaptive"], m["sale/adaptive+watch"]
	if w.Evictions == 0 {
		t.Fatalf("the watcher should delete old answers on a changing page: %+v", w)
	}
	if w.WatcherFetches == 0 || w.RequestsWaited >= ad.RequestsWaited {
		t.Fatalf("with the watcher fewer requests should wait: watch=%+v adaptive=%+v", w, ad)
	}
	if w.Detected != 5 || ad.Detected != 5 {
		t.Fatalf("every change must be detected: %d %d", w.Detected, ad.Detected)
	}
}

func TestNoticesRemoveStaleAnswers(t *testing.T) {
	m := armsByName(t, []Trace{shortQuiet(), shortSale()})
	for _, tr := range []string{"quiet", "sale"} {
		n := m[tr+"/adaptive+watch+notices"]
		if n.StaleServed != 0 {
			t.Fatalf("%s: with a notice at every change no stale answer should be served, got %d", tr, n.StaleServed)
		}
		if n.WorstLagSecs > 5 {
			t.Fatalf("%s: a notice makes the next lookup fetch; lag %v", tr, n.WorstLagSecs)
		}
	}
}

func TestMarkdownNamesEveryArmAndTrace(t *testing.T) {
	r, err := Report([]Trace{shortQuiet()}, DefaultArms(), 1, time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"fixed-5s", "fixed-60s", "adaptive-cap60s", "adaptive+watch+notices", "Trace `quiet`", "Reading the numbers honestly", "1 credit per 5 URLs"} {
		if !strings.Contains(r.Markdown, want) {
			t.Fatalf("markdown lacks %q", want)
		}
	}
	if len(r.Results) != 6 {
		t.Fatalf("results = %d, want 6 arms", len(r.Results))
	}
	// The 60 s cap bounds the lag at about a minute plus the wait for a request.
	m := armsByName(t, []Trace{shortQuiet()})
	if c := m["quiet/adaptive-cap60s"]; c.WorstLagSecs > 120 || c.FinalIntervalS != 60 {
		t.Fatalf("adaptive-cap60s = %+v", c)
	}
}
