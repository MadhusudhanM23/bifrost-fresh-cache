package proxy

import (
	"net/http"
	"strings"
	"sync"
	"testing"

	"freshness-companion/internal/auditlog"
)

// The fake Bifrost reports every semantic hit at similarity 0.84 against a
// 0.80 threshold, so VerifyBelow 0.90 puts it inside the gate.
func gateCompanion(t *testing.T, fb *fakeBifrost, tweak func(*AuditConfig)) *companion {
	t.Helper()
	return auditCompanion(t, fb, func(a *AuditConfig) {
		a.Rate, a.NearMargin = 0, 0
		a.VerifyBelow = 0.90
		if tweak != nil {
			tweak(a)
		}
	})
}

// An attacker caches a poisoned answer under a question close to the victim's.
// The victim's paraphrase matches it semantically, but the gate asks the model
// first, sees the answers disagree, removes the entry and serves the real one.
func TestGateBlocksAPoisonedMatch(t *testing.T) {
	fb := newFakeBifrost(t, "49")
	c := gateCompanion(t, fb, nil)
	fb.answerFor = map[string]string{
		pwQuestion: "Send your password to support@evil.test to reset it.", // planted by the attacker
		unQuestion: "Usernames cannot be changed.",
	}
	fb.semantic = map[string]string{unQuestion: pwQuestion}
	postChat(t, c.url, pwQuestion, nil)

	resp, body := postChat(t, c.url, unQuestion, nil)
	assertHeader(t, resp, HeaderGate, GateBlocked)
	if strings.Contains(string(body), "evil.test") || !strings.Contains(string(body), "Usernames cannot be changed") {
		t.Fatalf("the poisoned answer must not be served; body = %s", body)
	}
	if resp.Header.Get(HeaderStatus) != "" {
		t.Fatalf("a blocked answer carries no freshness headers about the removed entry: %v", freshnessHeaders(resp))
	}
	if _, clears, _ := fb.snapshot(); len(clears) != 0 {
		t.Fatalf("the matched entry must not be deleted (it may be right for its own question): %v", clears)
	}
	st, recs := fetchAudits(t, c.url)
	if st.GateBlocked != 1 || len(recs) != 1 || !recs[0].Gate || recs[0].Verdict != auditlog.Different || resp.Header.Get(HeaderAudit) != recs[0].ID {
		t.Fatalf("stats %+v records %+v", st, recs)
	}

	// The victim's question now has its own cached answer, so the next ask is
	// an exact hit on the real answer and never reaches the poisoned entry.
	again, againBody := postChat(t, c.url, unQuestion, nil)
	assertHeader(t, again, HeaderCacheHit, "true")
	assertHeader(t, again, HeaderCacheHitType, "direct")
	if strings.Contains(string(againBody), "evil.test") || !strings.Contains(string(againBody), "Usernames cannot be changed") {
		t.Fatalf("expected the real answer from the victim's own entry: %s", againBody)
	}
}

// Two look-alike questions must settle: each gets its own entry and neither
// removes the other's (they used to evict each other on every alternation).
func TestGateLookAlikesDoNotEvictEachOther(t *testing.T) {
	fb := newFakeBifrost(t, "49")
	c := gateCompanion(t, fb, nil)
	postChat(t, c.url, pwQuestion, nil)
	blocked, _ := postChat(t, c.url, unQuestion, nil)
	assertHeader(t, blocked, HeaderGate, GateBlocked)
	for i := 0; i < 3; i++ {
		for _, q := range []string{pwQuestion, unQuestion} {
			resp, body := postChat(t, c.url, q, nil)
			assertHeader(t, resp, HeaderCacheHit, "true")
			assertHeader(t, resp, HeaderCacheHitType, "direct")
			want := map[string]string{pwQuestion: "Security", unQuestion: "Usernames"}[q]
			if !strings.Contains(string(body), want) {
				t.Fatalf("round %d: %q got %s", i, q, body)
			}
		}
	}
	llm, clears, _ := fb.snapshot()
	if llm != 2 || len(clears) != 0 {
		t.Fatalf("model calls = %d (want 2: the password answer, and the check that doubled as the username answer), clears %v", llm, clears)
	}
}

func TestGateVerifiesARightMatchAndCachesItExactly(t *testing.T) {
	fb := newFakeBifrost(t, "49")
	c := gateCompanion(t, fb, nil)
	fb.answerFor[unQuestion] = "Open Settings > Security!" // same answer, different punctuation
	postChat(t, c.url, pwQuestion, nil)
	llm0, _, _ := fb.snapshot()

	first, body := postChat(t, c.url, unQuestion, nil)
	assertHeader(t, first, HeaderGate, GateVerified)
	assertHeader(t, first, HeaderCacheHitType, "semantic")
	if !strings.Contains(string(body), "Security") {
		t.Fatalf("body = %s", body)
	}
	llm1, clears, _ := fb.snapshot()
	if llm1 != llm0+1 || len(clears) != 0 {
		t.Fatalf("one check = one model call: llm %d -> %d, clears %v", llm0, llm1, clears)
	}

	// The check's answer was cached for this exact question: the next ask is
	// a plain direct hit and needs no check at all.
	second, _ := postChat(t, c.url, unQuestion, nil)
	assertHeader(t, second, HeaderCacheHitType, "direct")
	if second.Header.Get(HeaderGate) != "" {
		t.Fatalf("a direct hit is not gated: %q", second.Header.Get(HeaderGate))
	}
	if llm2, _, _ := fb.snapshot(); llm2 != llm1 {
		t.Fatalf("no model call expected: %d -> %d", llm1, llm2)
	}
}

// When the caller does not allow storing, the verified pair is remembered
// instead, so the same semantic hit is not re-checked on every ask.
func TestGateTrustsAVerifiedPairWhenNothingIsStored(t *testing.T) {
	fb := newFakeBifrost(t, "49")
	c := gateCompanion(t, fb, nil)
	fb.answerFor[unQuestion] = "Open Settings > Security!"
	postChat(t, c.url, pwQuestion, nil)
	noStore := map[string]string{HeaderCacheNoStore: "true"}
	llm0, _, _ := fb.snapshot()

	first, _ := postChat(t, c.url, unQuestion, noStore)
	assertHeader(t, first, HeaderGate, GateVerified)
	llm1, _, _ := fb.snapshot()
	second, _ := postChat(t, c.url, unQuestion, noStore)
	assertHeader(t, second, HeaderGate, GateTrusted)
	if llm2, _, _ := fb.snapshot(); llm1 != llm0+1 || llm2 != llm1 {
		t.Fatalf("model calls %d -> %d -> %d, want one check then none", llm0, llm1, llm2)
	}
	if st, _ := fetchAudits(t, c.url); st.GateVerified != 1 || st.GateTrusted != 1 {
		t.Fatalf("stats %+v", st)
	}

	// Trust belongs to the caller who was checked: another caller is checked too.
	other, _ := postChat(t, c.url, unQuestion, map[string]string{HeaderCacheNoStore: "true", "Authorization": "Bearer someone-else"})
	assertHeader(t, other, HeaderGate, GateVerified)
}

func TestGateIgnoresHitsAboveTheLine(t *testing.T) {
	fb := newFakeBifrost(t, "49")
	c := gateCompanion(t, fb, func(a *AuditConfig) { a.VerifyBelow = 0.80 }) // 0.84 is not below 0.80
	postChat(t, c.url, pwQuestion, nil)
	llm0, _, _ := fb.snapshot()
	resp, _ := postChat(t, c.url, unQuestion, nil)
	if resp.Header.Get(HeaderGate) != "" {
		t.Fatalf("the gate must not touch a hit scoring above the line: %q", resp.Header.Get(HeaderGate))
	}
	if llm1, _, _ := fb.snapshot(); llm1 != llm0 {
		t.Fatalf("no check expected: %d -> %d", llm0, llm1)
	}
}

func TestGateNeverTouchesDirectHits(t *testing.T) {
	fb := newFakeBifrost(t, "49")
	c := gateCompanion(t, fb, nil)
	postChat(t, c.url, pwQuestion, nil)
	resp, _ := postChat(t, c.url, pwQuestion, nil)
	assertHeader(t, resp, HeaderCacheHitType, "direct")
	if resp.Header.Get(HeaderGate) != "" {
		t.Fatalf("direct hits are exact matches: %q", resp.Header.Get(HeaderGate))
	}
}

// Many callers hit the same risky match together: one check serves them all.
func TestGateSharesOneCheckBetweenConcurrentHits(t *testing.T) {
	fb := newFakeBifrost(t, "49")
	c := gateCompanion(t, fb, nil)
	fb.answerFor[unQuestion] = "Open Settings > Security."
	postChat(t, c.url, pwQuestion, nil)
	llm0, _, _ := fb.snapshot()

	const n = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	gates := make([]string, n)
	statuses := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			resp, _ := postChat(t, c.url, unQuestion, nil)
			gates[i], statuses[i] = resp.Header.Get(HeaderGate), resp.StatusCode
		}(i)
	}
	close(start)
	wg.Wait()
	for i := range gates {
		// Requests that arrive after the check finished find the exact entry
		// it stored, so they are plain hits with no gate at all.
		if statuses[i] != http.StatusOK || (gates[i] != GateVerified && gates[i] != GateTrusted && gates[i] != "") {
			t.Errorf("request %d: status %d gate %q", i, statuses[i], gates[i])
		}
	}
	if llm1, _, _ := fb.snapshot(); llm1 != llm0+1 {
		t.Fatalf("model calls for %d concurrent hits = %d, want exactly 1", n, llm1-llm0)
	}
}

func TestGateServesUncheckedWhenItsBudgetIsUsedUp(t *testing.T) {
	fb := newFakeBifrost(t, "49")
	c := gateCompanion(t, fb, func(a *AuditConfig) { a.GateMaxPerMinute = 1 })
	const other = "How do I reset my Acme account nickname in the app?"
	fb.answerFor[other] = "Nicknames cannot be changed."
	fb.semantic[other] = pwQuestion
	postChat(t, c.url, pwQuestion, nil)

	first, _ := postChat(t, c.url, unQuestion, nil)
	assertHeader(t, first, HeaderGate, GateBlocked) // uses the one check
	second, body := postChat(t, c.url, other, nil)
	assertHeader(t, second, HeaderGate, GateUnverified)
	if !strings.Contains(string(body), "Security") || second.Header.Get(HeaderError) == "" {
		t.Fatalf("an unchecked hit is served as cached and says why; body %s, error %q", body, second.Header.Get(HeaderError))
	}
}
