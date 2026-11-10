package proxy

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"freshness-companion/internal/verified"
)

func verifiedCompanion(t *testing.T, fb *fakeBifrost, page *fakePage, doc string) (*companion, *verified.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "verified.json")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := verified.Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := newCompanionWith(t, fb.srv.URL, testPolicyDoc(page.srv.URL, "bypass"), 0, func(cfg *Config) { cfg.Verified = st })
	return c, st, path
}

const refundDoc = `[
  {"question": "What is your refund policy?", "answer": "Refunds are given within 30 days.", "label": "refunds-2026", "source": "legal team"},
  {"question": "Where is the office?", "answer": "12 Example Street.", "scope": "acme", "label": "acme-office"}
]`

func TestVerifiedAnswerIsServedWithoutTheModelOrCache(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c, _, _ := verifiedCompanion(t, fb, page, refundDoc)

	for _, q := range []string{"What is your refund policy?", "  what is YOUR refund policy!! ", "WHAT IS YOUR REFUND POLICY"} {
		resp, body := postChat(t, c.url, q, nil)
		assertHeader(t, resp, HeaderVerified, "human")
		assertHeader(t, resp, HeaderVerifiedLabel, "refunds-2026")
		assertHeader(t, resp, HeaderAnswerSource, "verified")
		if resp.Header.Get(HeaderCacheHit) != "" || resp.Header.Get(HeaderRequestID) == "" {
			t.Fatalf("no cache headers expected, request id expected: %v", resp.Header)
		}
		var out struct {
			ID, Object, Model string
			Choices           []struct {
				Message      struct{ Role, Content string }
				FinishReason string `json:"finish_reason"`
			}
			Usage map[string]int
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("%s", body)
		}
		if out.Object != "chat.completion" || !strings.HasPrefix(out.ID, "chatcmpl-verified-") || out.Model != "openai/gpt-4o-mini" ||
			len(out.Choices) != 1 || out.Choices[0].Message.Content != "Refunds are given within 30 days." ||
			out.Choices[0].Message.Role != "assistant" || out.Choices[0].FinishReason != "stop" ||
			out.Usage["total_tokens"] != 0 || len(out.Usage) != 3 {
			t.Fatalf("response = %s", body)
		}
	}
	fb.mu.Lock()
	calls, entries := fb.chatCalls, len(fb.entries)
	fb.mu.Unlock()
	if calls != 0 || entries != 0 {
		t.Fatalf("Bifrost must not be called: %d chat calls, %d cached entries", calls, entries)
	}
	// Not an exact match: it goes to the model as usual.
	resp, _ := postChat(t, c.url, "What is your refund policy for gift cards?", nil)
	if resp.Header.Get(HeaderVerified) != "" {
		t.Fatal("matching is exact; a longer question must not match")
	}
	fb.mu.Lock()
	calls = fb.chatCalls
	fb.mu.Unlock()
	if calls != 1 {
		t.Fatalf("the unmatched question must reach Bifrost once, got %d", calls)
	}
}

func TestVerifiedExplainsItself(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c, _, _ := verifiedCompanion(t, fb, page, refundDoc)
	resp, _ := postChat(t, c.url, "What is your refund policy?", nil)
	e := explainOf(t, c.url, resp)
	if e.How != HowVerified || e.VerifiedLabel != "refunds-2026" || e.VerifiedStatus != "human" || !strings.Contains(e.Plain, "verified answers") {
		t.Fatalf("explanation = %+v", e)
	}
}

func TestVerifiedStreamIsAProperSSE(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c, _, _ := verifiedCompanion(t, fb, page, refundDoc)
	resp, body := postJSON(t, c.url+"/v1/chat/completions", map[string]any{
		"model": "m-1", "stream": true, "messages": []map[string]string{{"role": "user", "content": "What is your refund policy?"}},
	}, nil)
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	var text strings.Builder
	var sawStop, sawDone bool
	sc := bufio.NewScanner(strings.NewReader(string(body)))
	for sc.Scan() {
		line := sc.Text()
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		if data == "[DONE]" {
			sawDone = true
			continue
		}
		var ch struct {
			Object, Model string
			Choices       []struct {
				Delta        struct{ Content string }
				FinishReason *string `json:"finish_reason"`
			}
		}
		if err := json.Unmarshal([]byte(data), &ch); err != nil || ch.Object != "chat.completion.chunk" || ch.Model != "m-1" {
			t.Fatalf("bad chunk %q", data)
		}
		text.WriteString(ch.Choices[0].Delta.Content)
		if ch.Choices[0].FinishReason != nil && *ch.Choices[0].FinishReason == "stop" {
			sawStop = true
		}
	}
	if text.String() != "Refunds are given within 30 days." || !sawStop || !sawDone {
		t.Fatalf("stream text %q stop %v done %v", text.String(), sawStop, sawDone)
	}
	assertHeader(t, resp, HeaderAnswerSource, "verified")
}

func TestVerifiedRespectsScope(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c, _, _ := verifiedCompanion(t, fb, page, refundDoc)

	mine, body := postChat(t, c.url, "Where is the office?", map[string]string{"x-bf-vk": "acme"})
	assertHeader(t, mine, HeaderVerifiedLabel, "acme-office")
	if !strings.Contains(string(body), "12 Example Street") {
		t.Fatalf("body = %s", body)
	}
	for _, h := range []map[string]string{{"x-bf-vk": "globex"}, nil} {
		other, body := postChat(t, c.url, "Where is the office?", h)
		if other.Header.Get(HeaderVerified) != "" || strings.Contains(string(body), "12 Example Street") {
			t.Fatalf("a customer's curated answer leaked (headers %v): %s", h, body)
		}
	}
}

func TestVerifiedScopeHeaderIsConfigurable(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	path := filepath.Join(t.TempDir(), "v.json")
	_ = os.WriteFile(path, []byte(refundDoc), 0o600)
	st, err := verified.Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := newCompanionWith(t, fb.srv.URL, testPolicyDoc(page.srv.URL, "bypass"), 0, func(cfg *Config) {
		cfg.Verified, cfg.VerifiedScopeHeader = st, "X-Tenant"
	})
	ok, _ := postChat(t, c.url, "Where is the office?", map[string]string{"X-Tenant": "acme"})
	assertHeader(t, ok, HeaderVerified, "human")
	no, _ := postChat(t, c.url, "Where is the office?", map[string]string{"x-bf-vk": "acme"})
	if no.Header.Get(HeaderVerified) != "" {
		t.Fatal("the default header must not count once another is configured")
	}
}

const priceDoc = `[{"question": "How much does the Acme Pro plan cost per month?", "answer": "The Acme Pro plan costs $39 per month.", "label": "pro-price"}]`

func TestVerifiedAnswerContradictedBySourceIsNotServed(t *testing.T) {
	page := newFakePage(t, "49") // the page says $49, the curated answer says $39
	fb := newFakeBifrost(t, "49")
	c, _, _ := verifiedCompanion(t, fb, page, priceDoc)

	resp, body := postChat(t, c.url, proPrompt, nil)
	assertHeader(t, resp, HeaderVerified, "stale_verified")
	assertHeader(t, resp, HeaderVerifiedLabel, "pro-price")
	if resp.Header.Get(HeaderAnswerSource) != "" || strings.Contains(string(body), "$39") || !strings.Contains(string(body), "$49") {
		t.Fatalf("the out-of-date curated answer must not be served; body = %s", body)
	}
	e := explainOf(t, c.url, resp)
	if e.VerifiedStatus != "stale_verified" || e.How == HowVerified || !strings.Contains(e.Plain, "source page disagrees") {
		t.Fatalf("explanation = %+v", e)
	}
	var list struct {
		Entries      []verifiedListEntry
		StaleSkipped int64 `json:"stale_skipped"`
	}
	getJSON(t, c.url+"/freshness/verified", &list)
	if len(list.Entries) != 1 || !list.Entries[0].Stale || list.StaleSkipped != 1 {
		t.Fatalf("list = %+v", list)
	}

	// The page changes to match the curated answer: it is served again.
	page.set("39", 0, "")
	again, body := postChat(t, c.url, proPrompt, nil)
	assertHeader(t, again, HeaderVerified, "human")
	assertHeader(t, again, HeaderVerifiedCheck, "confirmed")
	if !strings.Contains(string(body), "$39") {
		t.Fatalf("body = %s", body)
	}
	getJSON(t, c.url+"/freshness/verified", &list)
	if list.Entries[0].Stale {
		t.Fatal("the stale mark must clear once the source agrees")
	}
}

func TestVerifiedAnswerServedWhenSourceCannotBeChecked(t *testing.T) {
	page := newFakePage(t, "49")
	page.set("49", http.StatusInternalServerError, "")
	fb := newFakeBifrost(t, "49")
	c, _, _ := verifiedCompanion(t, fb, page, priceDoc)
	resp, _ := postChat(t, c.url, proPrompt, nil)
	assertHeader(t, resp, HeaderVerified, "human")
	assertHeader(t, resp, HeaderVerifiedCheck, "unable_to_verify")
}

func TestGuardBeatsVerifiedAnswers(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	path := filepath.Join(t.TempDir(), "v.json")
	_ = os.WriteFile(path, []byte(`[{"question":"What is the status of my order?","answer":"Always shipped."}]`), 0o600)
	st, _ := verified.Open(path, nil)
	c := newCompanionWith(t, fb.srv.URL, guardPolicy(page.srv.URL, `{"tool_results": "bypass"}`), 0, func(cfg *Config) { cfg.Verified = st })
	resp, body := postTools(t, c.url, withToolResult, nil)
	if resp.Header.Get(HeaderVerified) != "" || strings.Contains(string(body), "Always shipped") {
		t.Fatalf("a request carrying tool data must not get a curated answer: %s", body)
	}
	assertHeader(t, resp, HeaderGuard, "bypassed: tool_results")
}

func TestVerifiedHotReload(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c, st, path := verifiedCompanion(t, fb, page, refundDoc)
	before, _ := postChat(t, c.url, "Do you ship abroad?", nil)
	if before.Header.Get(HeaderVerified) != "" {
		t.Fatal("not listed yet")
	}
	doc := `[{"question": "Do you ship abroad?", "answer": "Yes, to 40 countries.", "label": "shipping"}]`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Minute)
	_ = os.Chtimes(path, future, future)
	if !st.Reload() {
		t.Fatal("the changed file was not reloaded")
	}
	after, body := postChat(t, c.url, "Do you ship abroad?", nil)
	assertHeader(t, after, HeaderVerifiedLabel, "shipping")
	if !strings.Contains(string(body), "40 countries") {
		t.Fatalf("body = %s", body)
	}
	gone, _ := postChat(t, c.url, "What is your refund policy?", nil)
	if gone.Header.Get(HeaderVerified) != "" {
		t.Fatal("a removed entry must stop being served after the reload")
	}
}

func TestVerifiedListShowsHashesAndLabelsOnly(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c, _, _ := verifiedCompanion(t, fb, page, refundDoc)
	postChat(t, c.url, "What is your refund policy?", nil)
	resp, err := http.Get(c.url + "/freshness/verified")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	for _, leak := range []string{"refund policy", "Refunds are given", "12 Example", "legal team", "Where is", `"acme"`} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("the listing leaks %q: %s", leak, raw)
		}
	}
	var out struct {
		Enabled bool
		Count   int
		Served  int64
		Entries []verifiedListEntry
	}
	_ = json.Unmarshal(raw, &out)
	if !out.Enabled || out.Count != 2 || out.Served != 1 || out.Entries[0].Label != "refunds-2026" || len(out.Entries[0].ID) != 12 || !out.Entries[1].Scoped {
		t.Fatalf("listing = %s", raw)
	}
}

func TestVerifiedOffByDefault(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c := newCompanion(t, fb.srv.URL, testPolicyDoc(page.srv.URL, "bypass"))
	var out struct {
		Enabled bool
		Count   int
	}
	if code := getJSON(t, c.url+"/freshness/verified", &out); code != 200 || out.Enabled || out.Count != 0 {
		t.Fatalf("HTTP %d %+v", code, out)
	}
}
