package proxy

import (
	"bytes"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// parseReceipt reads a receipt header back into key -> value. It is the
// reference reader for the format: quoted strings may hold ';' and escaped
// quotes, bare values run to the next ';'.
func parseReceipt(t *testing.T, s string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for len(s) > 0 {
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			t.Fatalf("receipt pair without '=': %q (in %q)", s, s)
		}
		key := strings.TrimSpace(s[:eq])
		rest := s[eq+1:]
		var val string
		if strings.HasPrefix(rest, `"`) {
			var b strings.Builder
			i := 1
			closed := false
			for i < len(rest) {
				c := rest[i]
				if c == '\\' && i+1 < len(rest) {
					b.WriteByte(rest[i+1])
					i += 2
					continue
				}
				if c == '"' {
					closed = true
					i++
					break
				}
				b.WriteByte(c)
				i++
			}
			if !closed {
				t.Fatalf("unterminated quoted value for %s in %q", key, s)
			}
			val = b.String()
			rest = rest[i:]
		} else {
			end := strings.IndexByte(rest, ';')
			if end < 0 {
				end = len(rest)
			}
			val = strings.TrimSpace(rest[:end])
			rest = rest[end:]
		}
		if _, dup := out[key]; dup {
			t.Fatalf("key %s appears twice in %q", key, s)
		}
		out[key] = val
		rest = strings.TrimSpace(rest)
		if rest == "" {
			break
		}
		if !strings.HasPrefix(rest, ";") {
			t.Fatalf("expected ';' after %s in %q", key, s)
		}
		s = strings.TrimSpace(rest[1:])
	}
	return out
}

func receiptOf(t *testing.T, resp *http.Response) map[string]string {
	t.Helper()
	raw := resp.Header.Get(HeaderReceipt)
	if raw == "" {
		t.Fatalf("no %s header (status %q, reason %q)", HeaderReceipt, resp.Header.Get(HeaderStatus), resp.Header.Get(HeaderReason))
	}
	if len(raw) > receiptMaxLen {
		t.Fatalf("receipt is %d characters, over %d", len(raw), receiptMaxLen)
	}
	for _, c := range raw {
		if c < 0x20 || c > 0x7e {
			t.Fatalf("receipt is not printable ASCII: %q", raw)
		}
	}
	return parseReceipt(t, raw)
}

func wantPairs(t *testing.T, got map[string]string, want map[string]string) {
	t.Helper()
	for k, v := range want {
		if got[k] != v {
			t.Errorf("receipt %s = %q, want %q (receipt: %v)", k, got[k], v, got)
		}
	}
}

func TestReceiptOnConfirmedHit(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c := newCompanion(t, fb.srv.URL, testPolicyDoc(page.srv.URL, "bypass"))
	postChat(t, c.url, proPrompt, nil)
	hit, _ := postChat(t, c.url, proPrompt, nil)
	assertHeader(t, hit, HeaderStatus, "confirmed")
	assertHeader(t, hit, HeaderEvidence, EvidenceFetched)
	raw := hit.Header.Get(HeaderReceipt)
	if !strings.HasPrefix(raw, `v=1; verdict=confirmed; action=served; found="49"; checked_ago_s=`) {
		t.Fatalf("receipt starts wrong: %q", raw)
	}
	r := receiptOf(t, hit)
	wantPairs(t, r, map[string]string{
		"v": "1", "verdict": "confirmed", "action": "served", "found": "49", "stated": "49",
		"source": page.srv.URL, "provider": "direct_fetch (not Tavily)", "evidence": "fetched",
		"fact": "Acme Pro plan price", "answer": "cached",
	})
	ago, err := strconv.Atoi(r["checked_ago_s"])
	if err != nil || ago < 0 || ago > 5 {
		t.Fatalf("checked_ago_s = %q, want a small whole number of seconds", r["checked_ago_s"])
	}
	if _, err := time.Parse(time.RFC3339, r["checked_at"]); err != nil {
		t.Fatalf("checked_at = %q: %v", r["checked_at"], err)
	}
	if r["checked_at"] != hit.Header.Get(HeaderCheckedAt) {
		t.Fatalf("checked_at %q differs from %s %q", r["checked_at"], HeaderCheckedAt, hit.Header.Get(HeaderCheckedAt))
	}
	if _, has := r["served_check"]; has {
		t.Fatalf("a served hit has no served_check, got %q", r["served_check"])
	}
}

func TestReceiptSaysReusedEvidence(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c := newCompanionWith(t, fb.srv.URL, testPolicyDoc(page.srv.URL, "bypass"), time.Minute, nil)
	postChat(t, c.url, proPrompt, nil)
	first, _ := postChat(t, c.url, proPrompt, nil)
	second, _ := postChat(t, c.url, proPrompt, nil)
	assertHeader(t, first, HeaderEvidence, EvidenceFetched)
	assertHeader(t, second, HeaderEvidence, EvidenceReused)
	wantPairs(t, receiptOf(t, first), map[string]string{"evidence": "fetched", "verdict": "confirmed"})
	wantPairs(t, receiptOf(t, second), map[string]string{"evidence": "reused", "verdict": "confirmed", "found": "49"})
	if page.hitCount() != 1 {
		t.Fatalf("the page was read %d times, want 1 (the second check reused the evidence)", page.hitCount())
	}
}

func TestReceiptOnStaleRefreshed(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c := newCompanion(t, fb.srv.URL, testPolicyDoc(page.srv.URL, "bypass"))
	postChat(t, c.url, proPrompt, nil)
	page.set("59", 0, "")
	fb.setPrice("59")
	resp, body := postChat(t, c.url, proPrompt, nil)
	assertHeader(t, resp, HeaderAction, ActionRefreshed)
	wantPairs(t, receiptOf(t, resp), map[string]string{
		"verdict": "stale", "action": "refreshed", "found": "59", "stated": "49",
		"served_check": "matches_current", "evidence": "fetched", "answer": "cached",
	})
	if !strings.Contains(string(body), "$59") {
		t.Fatalf("the refreshed answer should say $59: %s", body)
	}
}

func TestReceiptOnCheckedNewAnswer(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c := newCompanion(t, fb.srv.URL, policyDocWith(page.srv.URL, true, "report"))
	miss, _ := postChat(t, c.url, proPrompt, nil)
	assertHeader(t, miss, HeaderAnswer, "new")
	wantPairs(t, receiptOf(t, miss), map[string]string{
		"verdict": "confirmed", "action": "served", "answer": "new", "found": "49", "stated": "49", "evidence": "fetched",
	})

	// A new answer that disagrees with the page, with on_contradicted=correct:
	// the receipt describes the corrected answer that was served.
	page2 := newFakePage(t, "59")
	fb2 := newFakeBifrost(t, "49") // the model still believes $49
	c2 := newCompanion(t, fb2.srv.URL, policyDocWith(page2.srv.URL, true, "correct"))
	resp, body := postChat(t, c2.url, proPrompt, nil)
	assertHeader(t, resp, HeaderAction, ActionCorrected)
	wantPairs(t, receiptOf(t, resp), map[string]string{
		"verdict": "contradicted", "action": "corrected", "answer": "new", "found": "59", "stated": "49", "served_check": "matches_current",
	})
	if !strings.Contains(string(body), "$59") {
		t.Fatalf("the corrected answer should say $59: %s", body)
	}
}

func TestReceiptOnRefusedStaleAnswer(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c := newCompanion(t, fb.srv.URL, testPolicyDoc(page.srv.URL, "bypass"))
	postChat(t, c.url, proPrompt, nil)
	page.set("59", 0, "")
	fb.setClearStatus(http.StatusUnauthorized)
	resp, _ := postChat(t, c.url, proPrompt, nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("HTTP %d, want 502", resp.StatusCode)
	}
	wantPairs(t, receiptOf(t, resp), map[string]string{"verdict": "stale", "action": "refresh_failed", "found": "59", "stated": "49"})
}

func TestNoReceiptWithoutFreshnessCheck(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c := newCompanion(t, fb.srv.URL, guardPolicy(page.srv.URL, `{"tool_results": "bypass"}`))
	none := func(name string, resp *http.Response) {
		t.Helper()
		if v := resp.Header.Get(HeaderReceipt); v != "" {
			t.Errorf("%s: unexpected %s %q", name, HeaderReceipt, v)
		}
		if v := resp.Header.Get(HeaderEvidence); v != "" {
			t.Errorf("%s: unexpected %s %q", name, HeaderEvidence, v)
		}
		if resp.Header.Get(HeaderRequestID) == "" {
			t.Errorf("%s: every chat response still carries a request id", name)
		}
	}
	miss, _ := postChat(t, c.url, proPrompt, nil)
	assertHeader(t, miss, HeaderCacheHit, "false")
	none("miss", miss)

	guarded, _ := postTools(t, c.url, withToolResult, nil)
	if !strings.HasPrefix(guarded.Header.Get(HeaderGuard), "bypassed") {
		t.Fatalf("guard header = %q", guarded.Header.Get(HeaderGuard))
	}
	none("guarded", guarded)

	stream, _ := postJSON(t, c.url+"/v1/chat/completions", map[string]any{
		"model": "m", "stream": true, "messages": []map[string]string{{"role": "user", "content": proPrompt}},
	}, map[string]string{"x-bf-cache-key": "demo"})
	none("stream", stream)

	req, _ := http.NewRequest(http.MethodPost, c.url+"/v1/chat/completions", strings.NewReader("not json"))
	pass, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	pass.Body.Close()
	none("passthrough", pass)

	fb2 := newFakeBifrost(t, "49")
	g := gateCompanion(t, fb2, nil)
	fb2.semantic = map[string]string{unQuestion: pwQuestion}
	postChat(t, g.url, pwQuestion, nil)
	blocked, _ := postChat(t, g.url, unQuestion, nil)
	assertHeader(t, blocked, HeaderGate, GateBlocked)
	none("gate-blocked", blocked)
}

func TestReceiptBoundedAndQuoted(t *testing.T) {
	at := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	ago := int64(12)
	r := Receipt{
		Verdict: "confirmed", Action: "served", Answer: "cached",
		Fact:   `Acme "Pro" plan \ price`,
		Source: "http://demoserver:8000/?a=1;b=2", Provider: "direct_fetch (not Tavily)",
		Found: "59", Stated: "59", CheckedAt: &at, CheckedAgoSeconds: &ago, Evidence: "reused",
	}
	got := r.header()
	want := `v=1; verdict=confirmed; action=served; found="59"; checked_ago_s=12; source="http://demoserver:8000/?a=1;b=2"; provider="direct_fetch (not Tavily)"; checked_at="2026-11-10T09:00:00Z"; evidence=reused; stated="59"; fact="Acme \"Pro\" plan \\ price"; answer=cached`
	if got != want {
		t.Fatalf("header =\n%s\nwant\n%s", got, want)
	}
	back := parseReceipt(t, got)
	if back["fact"] != r.Fact || back["source"] != r.Source {
		t.Fatalf("quotes do not round-trip: %v", back)
	}

	// Control characters and non-ASCII follow headerValue: a space and \uXXXX.
	r.Fact = "price\ttab\nnewline é"
	back = parseReceipt(t, r.header())
	if back["fact"] != "price\ttab newline \\u00e9" {
		t.Fatalf("fact = %q", back["fact"])
	}

	// Too long: trailing pairs are dropped whole, the header stays parseable
	// and under the bound, and the pairs that remain are intact.
	long := strings.Repeat("x", 600)
	r = Receipt{Verdict: "stale", Action: "refreshed", Found: long, Source: "http://" + long, Provider: "direct_fetch (not Tavily)", Stated: "49", Fact: "f", Answer: "cached"}
	h := r.header()
	if len(h) > receiptMaxLen {
		t.Fatalf("header is %d characters", len(h))
	}
	back = parseReceipt(t, h)
	if back["v"] != "1" || back["verdict"] != "stale" || back["action"] != "refreshed" || back["found"] != headerValue(long) {
		t.Fatalf("leading pairs must be intact: %v", back)
	}
	if _, has := back["source"]; has {
		t.Fatalf("the source (%d characters) cannot fit and must be dropped, not cut: %q", len(back["source"]), back["source"])
	}
	if _, has := back["answer"]; has {
		t.Fatalf("pairs after the first that does not fit are dropped too: %v", back)
	}

	// A bare value that is not a plain token is quoted rather than trusted.
	r = Receipt{Verdict: "odd value; here", Action: "served"}
	if back = parseReceipt(t, r.header()); back["verdict"] != "odd value; here" {
		t.Fatalf("verdict = %q", back["verdict"])
	}
}

func TestReceiptParsesBack(t *testing.T) {
	h := http.Header{}
	h.Set(HeaderStatus, "stale")
	h.Set(HeaderAction, ActionCorrected)
	h.Set(HeaderAnswer, "cached")
	h.Set(HeaderFact, "Acme Pro plan price")
	h.Set(HeaderSource, "http://demoserver:8000/")
	h.Set(HeaderProvider, "direct_fetch (not Tavily)")
	h.Set(HeaderCurrentValue, "59")
	h.Set(HeaderCachedValue, "49")
	h.Set(HeaderRefreshedCheck, "differs_from_current")
	h.Set(HeaderCorrection, "corrected")
	h.Set(HeaderCorrectedCheck, "matches_current")
	h.Set(HeaderEvidence, EvidenceReused)
	h.Set(HeaderCheckedAt, "2026-11-10T09:00:00Z")
	now := time.Date(2026, 11, 10, 9, 0, 12, 500_000_000, time.UTC)

	setReceipt(h, now)
	back := parseReceipt(t, h.Get(HeaderReceipt))
	wantPairs(t, back, map[string]string{
		"v": "1", "verdict": "stale", "action": "corrected", "found": "59", "checked_ago_s": "12",
		"source": "http://demoserver:8000/", "provider": "direct_fetch (not Tavily)", "checked_at": "2026-11-10T09:00:00Z",
		"evidence": "reused", "stated": "49", "served_check": "matches_current", "fact": "Acme Pro plan price", "answer": "cached",
	})
	if len(back) != 13 {
		t.Fatalf("got %d pairs, want 13: %v", len(back), back)
	}
	r, ok := receiptFromHeaders(h, now)
	if !ok || r.ServedCheck != "matches_current" || r.CheckedAgoSeconds == nil || *r.CheckedAgoSeconds != 12 || r.CheckedAt == nil || !r.CheckedAt.Equal(time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("receipt = %+v", r)
	}

	// A check from the future (clock skew) is reported as 0 s ago, not negative.
	r, _ = receiptFromHeaders(h, now.Add(-time.Minute))
	if *r.CheckedAgoSeconds != 0 {
		t.Fatalf("checked_ago_seconds = %d, want 0", *r.CheckedAgoSeconds)
	}

	// New answers state their value in Answer-Value; a refresh without a
	// correction reports Refreshed-Check.
	h2 := http.Header{}
	h2.Set(HeaderStatus, "contradicted")
	h2.Set(HeaderAnswerValue, "49")
	h2.Set(HeaderRefreshedCheck, "differs_from_current")
	r, _ = receiptFromHeaders(h2, now)
	if r.Stated != "49" || r.ServedCheck != "differs_from_current" || r.CheckedAt != nil || r.CheckedAgoSeconds != nil {
		t.Fatalf("receipt = %+v", r)
	}

	// No status: no receipt, and nothing else is touched.
	h3 := http.Header{"Content-Type": {"application/json"}}
	setReceipt(h3, now)
	if len(h3) != 1 || h3.Get(HeaderReceipt) != "" {
		t.Fatalf("headers = %v", h3)
	}
}

func TestAnswerBodyUnchanged(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c := newCompanion(t, fb.srv.URL, testPolicyDoc(page.srv.URL, "bypass"))
	_, missBody := postChat(t, c.url, proPrompt, nil)
	hit, hitBody := postChat(t, c.url, proPrompt, nil)
	if hit.Header.Get(HeaderReceipt) == "" {
		t.Fatal("the hit should carry a receipt")
	}
	// The fake Bifrost serves the stored entry byte for byte, so the checked
	// hit must come back exactly as the miss did: the receipt lives in a
	// header, never in the body.
	if !bytes.Equal(missBody, hitBody) {
		t.Fatalf("body changed:\nmiss %s\nhit  %s", missBody, hitBody)
	}
	if bytes.Contains(hitBody, []byte("receipt")) || bytes.Contains(hitBody, []byte("verdict=")) {
		t.Fatalf("receipt text leaked into the body: %s", hitBody)
	}
	if got := hit.Header.Get("Content-Length"); got != strconv.Itoa(len(hitBody)) {
		t.Fatalf("Content-Length %q does not match body length %d", got, len(hitBody))
	}
}
