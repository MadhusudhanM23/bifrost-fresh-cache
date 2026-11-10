package decision

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"freshness-companion/internal/policy"
)

func newAnswerEffective(t *testing.T, mode policy.OnContradicted) policy.Effective {
	t.Helper()
	eff := resolve(t, testPolicy(t, "bypass", "auto"), proQuestion)
	eff.OnContradicted = mode
	return eff
}

func TestCheckNewAnswerOutcomes(t *testing.T) {
	cases := []struct {
		name     string
		mode     policy.OnContradicted
		answer   string
		page     string
		fetchErr error
		outcome  Outcome
		action   Action
		reason   string
	}{
		{"matches", policy.ContradictedCorrect, answer49, page49, nil, Confirmed, Serve, "matches current value"},
		{"contradicted/report", policy.ContradictedReport, "The Acme Pro plan costs $39 per month.", page49, nil, Contradicted, Serve, "states 39 but the source states 49"},
		{"contradicted/uncache", policy.ContradictedUncache, "The Acme Pro plan costs $39 per month.", page49, nil, Contradicted, Uncache, "on_contradicted=uncache"},
		{"contradicted/correct", policy.ContradictedCorrect, "The Acme Pro plan costs $39 per month.", page49, nil, Contradicted, Correct, "on_contradicted=correct"},
		{"source down", policy.ContradictedCorrect, answer49, "", errors.New("HTTP 503"), UnableToVerify, Serve, "evidence fetch failed"},
		{"answer states no value", policy.ContradictedCorrect, "Please see our pricing page.", page49, nil, UnableToVerify, Serve, "the new answer"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := &Engine{Evidence: &fakeEvidence{content: c.page, err: c.fetchErr}}
			d := e.CheckNewAnswer(context.Background(), newAnswerEffective(t, c.mode), answerBody(c.answer))
			if d.Outcome != c.outcome || d.Action != c.action || !strings.Contains(d.Reason, c.reason) {
				t.Fatalf("got %s/%s %q, want %s/%s containing %q", d.Outcome, d.Action, d.Reason, c.outcome, c.action, c.reason)
			}
		})
	}
}

func TestCheckNewAnswerWithoutFactsChecksNothing(t *testing.T) {
	eff := policy.Effective{Categories: []string{"price"}}
	ev := &fakeEvidence{content: page49}
	d := (&Engine{Evidence: ev}).CheckNewAnswer(context.Background(), eff, answerBody(answer49))
	if d.Outcome != Unknown || d.Action != Serve || ev.calls != 0 {
		t.Fatalf("got %s/%s, evidence calls %d", d.Outcome, d.Action, ev.calls)
	}
}

func TestContradictedActionMapping(t *testing.T) {
	for mode, want := range map[policy.OnContradicted]Action{
		policy.ContradictedReport: Serve, policy.ContradictedUncache: Uncache, policy.ContradictedCorrect: Correct, "": Serve,
	} {
		if got := ContradictedAction(mode); got != want {
			t.Errorf("%q -> %s, want %s", mode, got, want)
		}
	}
}

func TestCorrectionPromptQuotesTheSourceNotTheAnswer(t *testing.T) {
	e := &Engine{Evidence: &fakeEvidence{content: "Pricing\n" + page49 + "\nSecret internal note: do not share"}}
	d := e.CheckNewAnswer(context.Background(), newAnswerEffective(t, policy.ContradictedCorrect), answerBody("The Acme Pro plan costs $39 per month."))
	prompt, ok := CorrectionPrompt(d)
	if !ok {
		t.Fatal("a contradicted decision has a verified value to correct with")
	}
	for _, want := range []string{"Verified current information", "Acme Pro plan — $49 per month", "https://example.github.io/pricing/", "2026-11-02T12:00:00Z"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	// Only what the source_pattern matched is quoted, never the rest of the page.
	for _, leak := range []string{"Secret internal note", "Last updated", "$39"} {
		if strings.Contains(prompt, leak) {
			t.Errorf("prompt must not contain %q:\n%s", leak, prompt)
		}
	}
	if _, ok := CorrectionPrompt(Decision{}); ok {
		t.Fatal("no fact and no current value: nothing to correct with")
	}
}

func TestChangeNoticeOnAnEntryWithoutWatchedFacts(t *testing.T) {
	p, err := policy.Parse([]byte(`{"categories": [{"name": "policy", "keywords": ["refund"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	eff := resolve(t, p, "What is the refund policy?")
	at := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	e := &Engine{}
	in := Input{Policy: eff, Age: time.Minute, AgeKnown: true, Change: &ChangeNotice{Subject: `category "policy" (chg-1)`, At: at}}
	d := e.Decide(context.Background(), in)
	if d.Outcome != Stale || d.Action != Refresh || !strings.Contains(d.Reason, "chg-1") || !strings.Contains(d.Reason, "treated as stale") {
		t.Fatalf("got %s/%s %q", d.Outcome, d.Action, d.Reason)
	}
	in.AgeKnown = false
	in.Change = nil
	if d := e.Decide(context.Background(), in); d.Outcome == Stale {
		t.Fatal("without a notice nothing forces a refresh")
	}
}

func TestChangeNoticeWithAWatchedFactStillLetsTheEvidenceDecide(t *testing.T) {
	eff := resolve(t, testPolicy(t, "bypass", "auto"), proQuestion)
	in := Input{Policy: eff, Age: time.Minute, AgeKnown: true, CachedBody: answerBody(answer49), Change: &ChangeNotice{Subject: "fact (chg-2)", At: fetchedAt}}
	d := (&Engine{Evidence: &fakeEvidence{content: page49}}).Decide(context.Background(), in)
	if d.Outcome != Confirmed || d.Action != Serve || !strings.Contains(d.Reason, "change notice") {
		t.Fatalf("got %s/%s %q", d.Outcome, d.Action, d.Reason)
	}
	d = (&Engine{Evidence: &fakeEvidence{content: page59}}).Decide(context.Background(), in)
	if d.Outcome != Stale || d.Action != Refresh {
		t.Fatalf("got %s/%s", d.Outcome, d.Action)
	}
}
