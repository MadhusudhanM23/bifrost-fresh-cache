package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const sampleReceipt = `v=1; verdict=confirmed; action=served; found="59"; checked_ago_s=12; source="http://demoserver:8000/"; provider="direct_fetch (not Tavily)"; checked_at="2026-11-10T09:00:00Z"; evidence=reused; stated="59"; served_check=matches_current; fact="Acme Pro plan price"; answer=cached`

func TestParseReceiptHeader(t *testing.T) {
	r := parseReceiptHeader(sampleReceipt)
	if r == nil {
		t.Fatal("no receipt parsed")
	}
	if r.Verdict != "confirmed" || r.Action != "served" || r.Found != "59" || r.Stated != "59" || r.Source != "http://demoserver:8000/" ||
		r.Provider != "direct_fetch (not Tavily)" || r.CheckedAt != "2026-11-10T09:00:00Z" || r.Evidence != "reused" ||
		r.ServedCheck != "matches_current" || r.Fact != "Acme Pro plan price" || r.Answer != "cached" {
		t.Fatalf("receipt = %+v", *r)
	}
	if r.CheckedAgoSeconds == nil || *r.CheckedAgoSeconds != 12 {
		t.Fatalf("checked_ago_seconds = %v", r.CheckedAgoSeconds)
	}
	if want := "Checked http://demoserver:8000/ 12 s before this answer via direct fetch (notes reused): page said $59, still correct."; r.Text != want {
		t.Fatalf("text = %q, want %q", r.Text, want)
	}

	// Quoted strings may hold ';', escaped quotes and backslashes.
	r = parseReceiptHeader(`v=1; verdict=stale; action=refreshed; found="59"; source="http://x/?a=1;b=2"; stated="49"; served_check=matches_current; fact="Acme \"Pro\" \\ price"`)
	if r == nil || r.Source != "http://x/?a=1;b=2" || r.Fact != `Acme "Pro" \ price` {
		t.Fatalf("receipt = %+v", r)
	}
	if !strings.Contains(r.Text, "page said $59 but the saved answer said $49, so it was out of date; the new answer matches the page") {
		t.Fatalf("text = %q", r.Text)
	}

	// Strange input never panics and never invents a verdict.
	for _, bad := range []string{"", "   ", "nonsense", `verdict="unterminated`, "=; =;", `found="59"`} {
		if r := parseReceiptHeader(bad); r != nil {
			t.Errorf("%q parsed to %+v", bad, *r)
		}
	}
	// An unreadable tail drops only the tail.
	r = parseReceiptHeader(`v=1; verdict=unknown; action=served_with_warning; garbage`)
	if r == nil || r.Verdict != "unknown" || !strings.Contains(r.Text, "No source page was checked: it was not checked, so the saved answer was served with a warning") {
		t.Fatalf("receipt = %+v", r)
	}

	// Other verdicts read naturally too.
	cases := map[string]string{
		`verdict=contradicted; action=corrected; found="59"; stated="49"; fact="Acme Pro plan price"; source="http://p/"; answer=new`:   "page said $59 but the new answer said $49, so it was wrong; it was corrected",
		`verdict=contradicted; action=uncached; found="59"; stated="49"; fact="Acme Pro plan price"; source="http://p/"`:                "it was removed from the cache",
		`verdict=stale; action=refresh_failed; found="59"; stated="49"; fact="Support hours"; source="http://p/"`:                       "page said 59 but the saved answer said 49, so it was out of date; no new answer could be had",
		`verdict=unable_to_verify; action=refreshed; source="http://p/"; provider="tavily_extract"; checked_ago_s=90; evidence=fetched`: "Checked http://p/ 1 min before this answer via Tavily Extract (fresh look): it could not be checked, so the model was asked again",
	}
	for raw, want := range cases {
		r := parseReceiptHeader(raw)
		if r == nil || !strings.Contains(r.Text, want) {
			t.Errorf("%q\n  text %q\n  lacks %q", raw, r.Text, want)
		}
	}
}

// receiptServer answers every chat request as a confirmed cache hit carrying
// the sample receipt.
func receiptServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Bifrost-Cache-Hit", "true")
		w.Header().Set("X-Bifrost-Cache-Hit-Type", "direct")
		w.Header().Set("X-Freshness-Status", "confirmed")
		w.Header().Set("X-Freshness-Action", "served")
		w.Header().Set("X-Freshness-Receipt", sampleReceipt)
		w.Header().Set("X-Freshness-Request-Id", "req-1")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"The Acme Pro plan costs $59 per month."}}],"usage":{"total_tokens":7}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAskCapturesReceipt(t *testing.T) {
	srv := receiptServer(t)
	tally := &CacheTally{}
	r := newScenarioRunner(srv.Client(), srv.URL, srv.URL, "", tally)
	st := r.ask(context.Background(), srv.URL, "k", "Second ask", priceMessages(59), true)
	if st.Error != "" || !st.Reused || st.Status != "confirmed" {
		t.Fatalf("step = %+v", st)
	}
	if st.Receipt == nil || st.Receipt.Verdict != "confirmed" || st.Receipt.Found != "59" || !strings.HasPrefix(st.Receipt.Text, "Checked http://demoserver:8000/ 12 s before this answer") {
		t.Fatalf("step receipt = %+v", st.Receipt)
	}
	// The receipt travels into the request list too.
	snap := tally.snapshot()
	if len(snap.Recent) != 1 || snap.Recent[0].Receipt == nil || snap.Recent[0].Receipt.Text != st.Receipt.Text {
		t.Fatalf("tally event = %+v", snap.Recent)
	}
	// A reply without the header has no receipt.
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Bifrost-Cache-Hit", "false")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer plain.Close()
	if st := r.ask(context.Background(), plain.URL, "k", "Miss", priceMessages(49), false); st.Receipt != nil || st.Error != "" {
		t.Fatalf("miss step = %+v", st)
	}
}

func TestTrafficCapturesReceipt(t *testing.T) {
	srv := receiptServer(t)
	ev, _, isHit, err := sendProbe(context.Background(), srv.Client(), srv.URL, probeQuestion{"How much is the Pro plan?", "repeat"})
	if err != nil || !isHit || ev.HitType != "direct" || ev.RequestID != "req-1" {
		t.Fatalf("event = %+v, err %v", ev, err)
	}
	if ev.Receipt == nil || ev.Receipt.Verdict != "confirmed" || ev.Receipt.Evidence != "reused" || !strings.Contains(ev.Receipt.Text, "page said $59, still correct") {
		t.Fatalf("event receipt = %+v", ev.Receipt)
	}
}
