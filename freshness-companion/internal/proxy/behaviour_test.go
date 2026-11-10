package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"freshness-companion/internal/auditlog"
	"freshness-companion/internal/decision"
)

// policyDocWith is testPolicyDoc plus a "policy" category with no watched
// fact, and the new-answer settings on the price category.
func policyDocWith(sourceURL string, verify bool, mode string) string {
	return fmt.Sprintf(`{
	  "version": 1,
	  "categories": [
	    {"name": "price", "keywords": ["price", "cost", "how much"], "max_age": "10m", "on_unverifiable": "bypass",
	     "verify_new_answers": %t, "on_contradicted": %q},
	    {"name": "policy", "keywords": ["refund*", "policy"], "max_age": "24h", "on_unverifiable": "serve_with_warning"}
	  ],
	  "facts": [{
	    "name": "Acme Pro plan price", "category": "price", "keywords": ["pro"],
	    "source_url": %q,
	    "source_pattern": "Acme Pro plan[^$0-9]{0,20}\\$\\s*([0-9][0-9,]*(?:\\.[0-9]+)?)\\s*per month",
	    "answer_pattern": "(?i)\\$\\s*([0-9][0-9,]*(?:\\.[0-9]+)?)\\s*(?:/\\s*mo(?:nth)?|per\\s+month|a\\s+month|monthly)\\b",
	    "compare": "numeric"
	  }]
	}`, verify, mode, sourceURL)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func postJSON(t *testing.T, url string, payload any, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	b, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, out
}

// ---------------------------------------------------------------------------
// A new answer that is wrong from the start.
// ---------------------------------------------------------------------------

func TestNewAnswerContradictedIsReported(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "39") // the model's context is outdated: it says $39
	c := newCompanion(t, fb.srv.URL, policyDocWith(page.srv.URL, true, "report"))

	resp, body := postChat(t, c.url, proPrompt, nil)
	assertHeader(t, resp, HeaderStatus, "contradicted")
	assertHeader(t, resp, HeaderAction, ActionServed)
	assertHeader(t, resp, HeaderAnswer, "new")
	assertHeader(t, resp, HeaderAnswerValue, "39")
	assertHeader(t, resp, HeaderCurrentValue, "49")
	assertHeader(t, resp, HeaderCorrection, "reported")
	if resp.Header.Get(HeaderCachedValue) != "" || resp.Header.Get(HeaderAgeSeconds) != "" {
		t.Fatal("a new answer has no cached value or age")
	}
	if !strings.Contains(string(body), "$39") {
		t.Fatalf("report mode serves the answer as the model gave it: %s", body)
	}
	if _, clears, _ := fb.snapshot(); len(clears) != 0 {
		t.Fatalf("report mode must not delete anything, got %v", clears)
	}
	recs := c.log.Recent(1)
	if len(recs) != 1 || recs[0].AnswerOf != "new" || recs[0].Outcome != "contradicted" || recs[0].Correction != "reported" {
		t.Fatalf("decision log = %+v", recs)
	}
}

func TestNewAnswerThatMatchesIsConfirmedAndOffByDefault(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c := newCompanion(t, fb.srv.URL, policyDocWith(page.srv.URL, true, "correct"))
	resp, _ := postChat(t, c.url, proPrompt, nil)
	assertHeader(t, resp, HeaderStatus, "confirmed")
	assertHeader(t, resp, HeaderAnswer, "new")
	assertHeader(t, resp, HeaderAction, ActionServed)

	// verify_new_answers is off unless the policy asks: a miss is untouched.
	fb2 := newFakeBifrost(t, "39")
	c2 := newCompanion(t, fb2.srv.URL, policyDocWith(page.srv.URL, false, "correct"))
	resp, _ = postChat(t, c2.url, proPrompt, nil)
	if h := freshnessHeaders(resp); len(h) != 0 {
		t.Fatalf("verify_new_answers off, but the miss got %v", h)
	}
}

func TestNewAnswerUncache(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "39")
	c := newCompanion(t, fb.srv.URL, policyDocWith(page.srv.URL, true, "uncache"))

	resp, body := postChat(t, c.url, proPrompt, nil)
	assertHeader(t, resp, HeaderStatus, "contradicted")
	assertHeader(t, resp, HeaderAction, ActionUncached)
	assertHeader(t, resp, HeaderCorrection, "uncached")
	if !strings.Contains(string(body), "$39") {
		t.Fatalf("the contradicted answer is returned, labelled: %s", body)
	}
	id := resp.Header.Get(HeaderCacheID)
	_, clears, _ := fb.snapshot()
	if len(clears) < 1 || clears[0] != id {
		t.Fatalf("clears = %v, want the new entry %s", clears, id)
	}
	// It is not reused: the next identical request is another model call.
	postChat(t, c.url, proPrompt, nil)
	if llm, _, _ := fb.snapshot(); llm != 2 {
		t.Fatalf("llm calls = %d, want 2 (nothing was cached)", llm)
	}
	// A second delete follows, in case the first beat Bifrost's background write.
	waitFor(t, "the repeat delete", func() bool { _, cl, _ := fb.snapshot(); return len(cl) >= 2 })
}

func TestNewAnswerCorrect(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "39")
	c := newCompanion(t, fb.srv.URL, policyDocWith(page.srv.URL, true, "correct"))

	resp, body := postChat(t, c.url, proPrompt, nil)
	assertHeader(t, resp, HeaderStatus, "contradicted")
	assertHeader(t, resp, HeaderAction, ActionCorrected)
	assertHeader(t, resp, HeaderCorrection, "corrected")
	assertHeader(t, resp, HeaderAnswerValue, "39")
	assertHeader(t, resp, HeaderCorrectedValue, "49")
	assertHeader(t, resp, HeaderCorrectedCheck, decision.RefreshedMatches)
	if !strings.Contains(string(body), "$49 per month") || strings.Contains(string(body), "$39") {
		t.Fatalf("the corrected answer is served: %s", body)
	}
	// The correction was a system message in front of the conversation, and
	// the original request is otherwise unchanged.
	fb.mu.Lock()
	sent := string(fb.lastChat)
	fb.mu.Unlock()
	var corrected struct {
		Messages []struct{ Role, Content string } `json:"messages"`
	}
	if err := json.Unmarshal([]byte(sent), &corrected); err != nil || len(corrected.Messages) != 3 ||
		corrected.Messages[0].Role != "system" || !strings.HasPrefix(corrected.Messages[0].Content, "Verified current information") ||
		corrected.Messages[1].Content != "Answer in one sentence." || corrected.Messages[2].Content != proPrompt {
		t.Fatalf("the corrected request = %s (%v)", sent, err)
	}
	if llm, clears, _ := fb.snapshot(); llm != 2 || len(clears) != 1 {
		t.Fatalf("llm = %d, clears = %v; want the wrong entry cleared and one more model call", llm, clears)
	}
	// The corrected answer is what is cached now.
	again, _ := postChat(t, c.url, proPrompt, nil)
	assertHeader(t, again, HeaderStatus, "confirmed")
	assertHeader(t, again, HeaderCachedValue, "49")
	recs := c.log.Recent(2)
	if recs[1].Correction != "corrected" || recs[1].CorrectedValue != "49" || recs[1].CachedValue != "39" {
		t.Fatalf("decision log = %+v", recs[1])
	}
}

func TestNewAnswerCorrectionThatStillDisagreesIsDropped(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "39")
	fb.answerFor = map[string]string{proPrompt: "The Acme Pro plan costs $39 per month."} // ignores any correction
	c := newCompanion(t, fb.srv.URL, policyDocWith(page.srv.URL, true, "correct"))

	resp, body := postChat(t, c.url, proPrompt, nil)
	assertHeader(t, resp, HeaderAction, ActionUncached)
	assertHeader(t, resp, HeaderCorrection, "uncached")
	assertHeader(t, resp, HeaderCorrectedValue, "39")
	assertHeader(t, resp, HeaderCorrectedCheck, decision.RefreshedDiffers)
	if !strings.Contains(string(body), "$39") {
		t.Fatalf("body = %s", body)
	}
	if _, clears, _ := fb.snapshot(); len(clears) < 2 {
		t.Fatalf("both the wrong entry and the corrected-but-wrong one are deleted, clears = %v", clears)
	}
}

func TestStaleRefreshThatStillDisagreesIsCorrected(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c := newCompanion(t, fb.srv.URL, policyDocWith(page.srv.URL, false, "correct"))
	postChat(t, c.url, proPrompt, nil) // cached: $49

	page.set("59", 0, "") // the price changes, but the model's context still says $49
	resp, body := postChat(t, c.url, proPrompt, nil)
	assertHeader(t, resp, HeaderStatus, "stale")
	assertHeader(t, resp, HeaderAction, ActionCorrected)
	assertHeader(t, resp, HeaderRefreshedCheck, decision.RefreshedDiffers) // the plain refresh was still wrong
	assertHeader(t, resp, HeaderCorrection, "corrected")
	assertHeader(t, resp, HeaderCorrectedValue, "59")
	if !strings.Contains(string(body), "$59 per month") {
		t.Fatalf("body = %s", body)
	}
	if llm, _, _ := fb.snapshot(); llm != 3 {
		t.Fatalf("llm calls = %d, want 3 (first answer, plain refresh, corrected)", llm)
	}
}

func TestStaleRefreshStaysAsBeforeWhenReporting(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c := newCompanion(t, fb.srv.URL, policyDocWith(page.srv.URL, false, "report"))
	postChat(t, c.url, proPrompt, nil)
	page.set("59", 0, "")
	resp, body := postChat(t, c.url, proPrompt, nil)
	assertHeader(t, resp, HeaderAction, ActionRefreshed)
	assertHeader(t, resp, HeaderRefreshedCheck, decision.RefreshedDiffers)
	if resp.Header.Get(HeaderCorrection) != "" || !strings.Contains(string(body), "$49") {
		t.Fatalf("the default is unchanged: %v %s", resp.Header, body)
	}
}

// ---------------------------------------------------------------------------
// Change notices.
// ---------------------------------------------------------------------------

func TestChangeNoticeDropsCachedEvidence(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c := newCompanionWith(t, fb.srv.URL, policyDocWith(page.srv.URL, false, "report"), time.Hour, nil)
	postChat(t, c.url, proPrompt, nil)
	hit, _ := postChat(t, c.url, proPrompt, nil)
	assertHeader(t, hit, HeaderStatus, "confirmed")

	page.set("59", 0, "")
	fb.setPrice("59")
	// Without a notice the hour-long evidence cache still says $49.
	lag, _ := postChat(t, c.url, proPrompt, nil)
	assertHeader(t, lag, HeaderStatus, "confirmed")
	assertHeader(t, lag, HeaderCurrentValue, "49")

	tick()
	resp, out := postJSON(t, c.url+"/freshness/changes", map[string]any{"fact": "Acme Pro plan price", "note": "price raised"}, nil)
	tick()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, out)
	}
	var notice struct {
		ID              string   `json:"id"`
		Kind            string   `json:"kind"`
		EvidenceDropped []string `json:"evidence_dropped"`
	}
	_ = json.Unmarshal(out, &notice)
	if notice.ID == "" || notice.Kind != "fact" || len(notice.EvidenceDropped) != 1 {
		t.Fatalf("notice = %s", out)
	}

	after, body := postChat(t, c.url, proPrompt, nil)
	assertHeader(t, after, HeaderStatus, "stale")
	assertHeader(t, after, HeaderAction, ActionRefreshed)
	assertHeader(t, after, HeaderChangeNotice, notice.ID)
	assertHeader(t, after, HeaderCurrentValue, "59")
	if !strings.Contains(string(body), "$59 per month") || !strings.Contains(after.Header.Get(HeaderReason), "change notice") {
		t.Fatalf("reason %q body %s", after.Header.Get(HeaderReason), body)
	}
	// The refreshed entry is newer than the notice, so it is not forced again.
	next, _ := postChat(t, c.url, proPrompt, nil)
	assertHeader(t, next, HeaderStatus, "confirmed")
	if next.Header.Get(HeaderChangeNotice) != "" {
		t.Fatal("an entry written after the notice must not be forced again")
	}
}

func TestChangeNoticeRefreshesAnEntryWithNoWatchedFact(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c := newCompanion(t, fb.srv.URL, policyDocWith(page.srv.URL, false, "report"))
	const q = "What is your refund policy?"
	postChat(t, c.url, q, nil)
	before, _ := postChat(t, c.url, q, nil)
	assertHeader(t, before, HeaderStatus, "unknown") // served on policy only
	assertHeader(t, before, HeaderAction, ActionServed)

	tick()
	resp, out := postJSON(t, c.url+"/freshness/changes", map[string]any{"category": "policy"}, nil)
	tick()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, out)
	}
	after, _ := postChat(t, c.url, q, nil)
	assertHeader(t, after, HeaderStatus, "stale")
	assertHeader(t, after, HeaderAction, ActionRefreshed)
	if after.Header.Get(HeaderChangeNotice) == "" || !strings.Contains(after.Header.Get(HeaderReason), "treated as stale") {
		t.Fatalf("headers = %v", after.Header)
	}
	if llm, clears, _ := fb.snapshot(); llm != 2 || len(clears) != 1 {
		t.Fatalf("llm = %d clears = %v", llm, clears)
	}
	// The category price is not covered by a policy notice.
	postChat(t, c.url, proPrompt, nil)
	other, _ := postChat(t, c.url, proPrompt, nil)
	if other.Header.Get(HeaderChangeNotice) != "" {
		t.Fatal("a policy notice must not touch price answers")
	}
}

func TestChangeNoticeValidationAndAuth(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c := newCompanionWith(t, fb.srv.URL, policyDocWith(page.srv.URL, false, "report"), 0, func(cfg *Config) { cfg.ChangeToken = "s3cret" })
	url := c.url + "/freshness/changes"
	auth := map[string]string{"Authorization": "Bearer s3cret"}

	if resp, _ := postJSON(t, url, map[string]any{"fact": "Acme Pro plan price"}, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
	if resp, _ := postJSON(t, url, map[string]any{"fact": "Acme Pro plan price"}, map[string]string{"Authorization": "Bearer nope"}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", resp.StatusCode)
	}
	for name, payload := range map[string]map[string]any{
		"no selector":         {},
		"two selectors":       {"fact": "Acme Pro plan price", "category": "price"},
		"unknown field":       {"fact": "Acme Pro plan price", "colour": "red"},
		"too many cache keys": {"category": "price", "clear_cache_keys": []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k"}},
	} {
		if resp, _ := postJSON(t, url, payload, auth); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: status %d, want 400", name, resp.StatusCode)
		}
	}
	for name, payload := range map[string]map[string]any{
		"unknown fact":     {"fact": "No such fact"},
		"unknown category": {"category": "weather"},
		"unknown source":   {"source_url": "https://example.com/nothing"},
	} {
		if resp, _ := postJSON(t, url, payload, auth); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: status %d, want 404", name, resp.StatusCode)
		}
	}
	resp, out := postJSON(t, url, map[string]any{"source_url": strings.ToUpper(page.srv.URL[:4]) + page.srv.URL[4:] + "/"}, auth)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(out), "Acme Pro plan price") {
		t.Fatalf("source_url notice (URL compared loosely): %d %s", resp.StatusCode, out)
	}
	list, err := http.Get(url + "?limit=5")
	if err != nil {
		t.Fatal(err)
	}
	defer list.Body.Close()
	raw, _ := io.ReadAll(list.Body)
	if !strings.Contains(string(raw), `"count":1`) {
		t.Fatalf("GET /freshness/changes = %s", raw)
	}
}

func TestChangeNoticeCanClearCacheKeys(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c := newCompanion(t, fb.srv.URL, policyDocWith(page.srv.URL, false, "report"))
	postChat(t, c.url, proPrompt, nil)
	tick()
	resp, out := postJSON(t, c.url+"/freshness/changes", map[string]any{"category": "price", "clear_cache_keys": []string{"demo"}}, nil)
	tick()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(out), `"cleared_cache_keys":["demo"]`) {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	fb.mu.Lock()
	cleared := append([]string(nil), fb.clearKeys...)
	fb.mu.Unlock()
	if len(cleared) != 1 || cleared[0] != "demo" {
		t.Fatalf("clear-by-key calls = %v", cleared)
	}
	miss, _ := postChat(t, c.url, proPrompt, nil)
	assertHeader(t, miss, HeaderCacheHit, "false")
}

// ---------------------------------------------------------------------------
// Wrong-match audits.
// ---------------------------------------------------------------------------

const (
	pwQuestion = "How do I reset my Acme account password in the app?"
	unQuestion = "How do I reset my Acme account username in the app?"
)

func auditCompanion(t *testing.T, fb *fakeBifrost, tweak func(*AuditConfig)) *companion {
	t.Helper()
	page := newFakePage(t, "49")
	fb.answerFor = map[string]string{pwQuestion: "Open Settings > Security.", unQuestion: "Usernames cannot be changed."}
	fb.semantic = map[string]string{unQuestion: pwQuestion}
	return newCompanionWith(t, fb.srv.URL, testPolicyDoc(page.srv.URL, "bypass"), 0, func(cfg *Config) {
		log, err := auditlog.Open("", 50)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Audit = AuditConfig{Log: log, Rate: 1, Compare: CompareExact, Timeout: 5 * time.Second}
		if tweak != nil {
			tweak(&cfg.Audit)
		}
	})
}

func fetchAudits(t *testing.T, base string) (stats AuditStats, recs []auditlog.Record) {
	t.Helper()
	resp, err := http.Get(base + "/freshness/audits")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Enabled bool              `json:"enabled"`
		Stats   AuditStats        `json:"stats"`
		Audits  []auditlog.Record `json:"audits"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Stats, out.Audits
}

func TestAuditFindsAWrongMatch(t *testing.T) {
	fb := newFakeBifrost(t, "49")
	c := auditCompanion(t, fb, nil)
	postChat(t, c.url, pwQuestion, nil) // miss, cached

	resp, body := postChat(t, c.url, unQuestion, nil)
	assertHeader(t, resp, HeaderCacheHitType, "semantic")
	id := resp.Header.Get(HeaderAudit)
	if id == "" || !strings.Contains(string(body), "Security") {
		t.Fatalf("the semantic hit is served as cached and marked for audit; id %q body %s", id, body)
	}
	waitFor(t, "the audit", func() bool { st, _ := fetchAudits(t, c.url); return st.Completed == 1 })
	st, recs := fetchAudits(t, c.url)
	if st.Different != 1 || st.Same != 0 || len(recs) != 1 {
		t.Fatalf("stats %+v records %+v", st, recs)
	}
	r := recs[0]
	if r.ID != id || r.Verdict != auditlog.Different || r.Compare != CompareExact || r.Similarity == nil || *r.Similarity != 0.84 || *r.Threshold != 0.8 {
		t.Fatalf("record = %+v", r)
	}
	// The audit asked the model with a throwaway key and stored nothing.
	fb.mu.Lock()
	last := fb.keys[len(fb.keys)-1]
	entries := len(fb.entries)
	fb.mu.Unlock()
	if !strings.HasPrefix(last, "audit-aud-") || entries != 1 {
		t.Fatalf("audit used key %q and the cache holds %d entries", last, entries)
	}
	// No prompt or answer text is kept.
	raw, _ := json.Marshal(recs)
	for _, private := range []string{pwQuestion, unQuestion, "Security", "Usernames"} {
		if strings.Contains(string(raw), private) {
			t.Fatalf("audit record contains %q", private)
		}
	}
}

func TestAuditAcceptsARightMatch(t *testing.T) {
	fb := newFakeBifrost(t, "49")
	c := auditCompanion(t, fb, nil)
	fb.answerFor[unQuestion] = "Open Settings > Security!" // same answer, different punctuation
	postChat(t, c.url, pwQuestion, nil)
	postChat(t, c.url, unQuestion, nil)
	waitFor(t, "the audit", func() bool { st, _ := fetchAudits(t, c.url); return st.Completed == 1 })
	if st, _ := fetchAudits(t, c.url); st.Same != 1 || st.Different != 0 {
		t.Fatalf("stats %+v", st)
	}
}

func TestAuditOnlyCoversSemanticHitsAndHonoursTheCaps(t *testing.T) {
	fb := newFakeBifrost(t, "49")
	c := auditCompanion(t, fb, nil)
	postChat(t, c.url, pwQuestion, nil)
	direct, _ := postChat(t, c.url, pwQuestion, nil) // an exact hit
	if direct.Header.Get(HeaderAudit) != "" {
		t.Fatal("exact hits are not audited")
	}

	fb2 := newFakeBifrost(t, "49")
	c2 := auditCompanion(t, fb2, func(a *AuditConfig) { a.Rate, a.NearMargin = 0, 0.01 })
	postChat(t, c2.url, pwQuestion, nil)
	far, _ := postChat(t, c2.url, unQuestion, nil) // 0.84 is 0.04 above 0.8: outside a 0.01 margin
	if far.Header.Get(HeaderAudit) != "" {
		t.Fatal("outside the margin and at rate 0, nothing is audited")
	}
	if c2.auditorOff() {
		t.Fatal("near-margin alone should enable auditing")
	}

	fb3 := newFakeBifrost(t, "49")
	c3 := auditCompanion(t, fb3, func(a *AuditConfig) { a.Rate, a.NearMargin = 0, 0.05 })
	postChat(t, c3.url, pwQuestion, nil)
	near, _ := postChat(t, c3.url, unQuestion, nil)
	if near.Header.Get(HeaderAudit) == "" {
		t.Fatal("a hit 0.04 above the threshold is within the 0.05 margin")
	}
	waitFor(t, "the audit", func() bool { st, _ := fetchAudits(t, c3.url); return st.Completed == 1 })
	if _, recs := fetchAudits(t, c3.url); !recs[0].Near {
		t.Fatalf("record = %+v", recs[0])
	}

	fb4 := newFakeBifrost(t, "49")
	c4 := auditCompanion(t, fb4, func(a *AuditConfig) { a.MaxPerMinute = 1 })
	postChat(t, c4.url, pwQuestion, nil)
	postChat(t, c4.url, unQuestion, nil)
	second, _ := postChat(t, c4.url, unQuestion, nil)
	if second.Header.Get(HeaderAudit) != "" {
		t.Fatal("the per-minute cap was reached")
	}
	if st, _ := fetchAudits(t, c4.url); st.Skipped != 1 {
		t.Fatalf("stats %+v", st)
	}
}

func TestAuditIsOffByDefault(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	fb.semantic = map[string]string{unQuestion: pwQuestion}
	c := newCompanion(t, fb.srv.URL, testPolicyDoc(page.srv.URL, "bypass"))
	postChat(t, c.url, pwQuestion, nil)
	resp, _ := postChat(t, c.url, unQuestion, nil)
	if resp.Header.Get(HeaderAudit) != "" {
		t.Fatal("no audit unless it is switched on")
	}
	if st, recs := fetchAudits(t, c.url); st.Scheduled != 0 || len(recs) != 0 {
		t.Fatalf("stats %+v", st)
	}
}

func TestAuditJudgeMode(t *testing.T) {
	for _, tc := range []struct {
		says, verdict string
	}{
		{"SAME", auditlog.Same},
		{"different.", auditlog.Different},
		{"It depends", auditlog.Error},
	} {
		t.Run(tc.says, func(t *testing.T) {
			fb := newFakeBifrost(t, "49")
			fb.judgeSays = tc.says
			c := auditCompanion(t, fb, func(a *AuditConfig) { a.Compare = CompareJudge })
			postChat(t, c.url, pwQuestion, nil)
			postChat(t, c.url, unQuestion, nil)
			waitFor(t, "the audit", func() bool { st, _ := fetchAudits(t, c.url); return st.Completed == 1 })
			_, recs := fetchAudits(t, c.url)
			if recs[0].Verdict != tc.verdict || recs[0].Compare != CompareJudge {
				t.Fatalf("record = %+v", recs[0])
			}
			fb.mu.Lock()
			judgeKey := fb.keys[len(fb.keys)-1]
			fb.mu.Unlock()
			if !strings.HasPrefix(judgeKey, "audit-judge-") {
				t.Fatalf("the judge call used cache key %q", judgeKey)
			}
		})
	}
}

func (c *companion) auditorOff() bool {
	resp, err := http.Get(c.url + "/healthz")
	if err != nil {
		return true
	}
	defer resp.Body.Close()
	var h struct {
		AuditEnabled bool `json:"audit_enabled"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&h)
	return !h.AuditEnabled
}

// tick waits longer than one clock tick (Windows clocks can be ~15ms coarse),
// so "written before the notice" and "written after it" are never a tie.
func tick() { time.Sleep(25 * time.Millisecond) }
