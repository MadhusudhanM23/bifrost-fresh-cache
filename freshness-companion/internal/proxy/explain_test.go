package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func getJSON(t *testing.T, url string, v any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if v != nil {
		if err := json.Unmarshal(raw, v); err != nil {
			t.Fatalf("%s: not JSON: %s", url, raw)
		}
	}
	return resp.StatusCode
}

func explainOf(t *testing.T, base string, resp *http.Response) Explanation {
	t.Helper()
	id := resp.Header.Get(HeaderRequestID)
	if id == "" {
		t.Fatal("the response has no " + HeaderRequestID)
	}
	var e Explanation
	if code := getJSON(t, base+"/freshness/explain/"+id, &e); code != http.StatusOK {
		t.Fatalf("explain %s: HTTP %d", id, code)
	}
	if e.ID != id {
		t.Fatalf("explain id = %q, want %q", e.ID, id)
	}
	return e
}

func TestExplainMissThenDirectHit(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c := newCompanion(t, fb.srv.URL, testPolicyDoc(page.srv.URL, "bypass"))

	miss, _ := postChat(t, c.url, proPrompt, nil)
	e := explainOf(t, c.url, miss)
	if e.How != HowMiss || e.CacheHit || e.CacheID == "" || e.HTTPStatus != 200 {
		t.Fatalf("miss explanation = %+v", e)
	}

	hit, _ := postChat(t, c.url, proPrompt, nil)
	h := explainOf(t, c.url, hit)
	if h.How != HowDirectHit || !h.CacheHit || h.HitType != "direct" {
		t.Fatalf("hit explanation = %+v", h)
	}
	if h.FreshnessStatus != "confirmed" || h.FreshnessAction != "served" || h.CurrentValue != "49" || h.AgeSeconds == nil || h.CheckedAgoSeconds == nil {
		t.Fatalf("freshness fields missing: %+v", h)
	}
	for _, want := range []string{"came from the cache", "still says 49", "old"} {
		if !strings.Contains(h.Plain, want) {
			t.Errorf("plain %q lacks %q", h.Plain, want)
		}
	}
	if n := strings.Count(h.Plain, ". ") + 1; n < 2 || n > 4 {
		t.Errorf("plain should be 2-4 sentences, got %d: %q", n, h.Plain)
	}
	if hit.Header.Get(HeaderRequestID) == miss.Header.Get(HeaderRequestID) {
		t.Fatal("ids must be unique")
	}
}

func TestExplainSemanticHitShowsScores(t *testing.T) {
	fb := newFakeBifrost(t, "49")
	c := auditCompanion(t, fb, func(a *AuditConfig) { a.Rate, a.NearMargin = 0, 0 })
	fb.semantic = map[string]string{unQuestion: pwQuestion}
	fb.answerFor[unQuestion] = "Open Settings > Security."
	postChat(t, c.url, pwQuestion, nil)
	resp, _ := postChat(t, c.url, unQuestion, nil)
	e := explainOf(t, c.url, resp)
	if e.How != HowSemanticHit || e.Similarity == nil || *e.Similarity != 0.84 || e.Threshold == nil || *e.Threshold != 0.8 {
		t.Fatalf("explanation = %+v", e)
	}
	if !strings.Contains(e.Plain, "0.84 closeness") || !strings.Contains(e.Plain, "0.80") {
		t.Fatalf("plain must give the closeness and the threshold: %q", e.Plain)
	}
}

func TestExplainStaleRefreshed(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c := newCompanion(t, fb.srv.URL, testPolicyDoc(page.srv.URL, "bypass"))
	postChat(t, c.url, proPrompt, nil)
	page.set("59", 0, "")
	fb.setPrice("59")
	resp, _ := postChat(t, c.url, proPrompt, nil)
	e := explainOf(t, c.url, resp)
	if e.How != HowRefreshed || e.FreshnessStatus != "stale" || e.AnswerValue != "49" || e.CurrentValue != "59" {
		t.Fatalf("explanation = %+v", e)
	}
	if !strings.Contains(e.Plain, "said 49") || !strings.Contains(e.Plain, "says 59") || !strings.Contains(e.Plain, "out of date") {
		t.Fatalf("plain = %q", e.Plain)
	}
}

func TestExplainGuardedAndGateBlocked(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c := newCompanion(t, fb.srv.URL, guardPolicy(page.srv.URL, `{"tool_results": "bypass"}`))
	resp, _ := postTools(t, c.url, withToolResult, nil)
	e := explainOf(t, c.url, resp)
	if e.How != HowGuarded || e.GuardReason != "tool_results" || !strings.Contains(e.Plain, "kept out of the cache on purpose") {
		t.Fatalf("guarded explanation = %+v", e)
	}

	fb2 := newFakeBifrost(t, "49")
	g := gateCompanion(t, fb2, nil)
	fb2.semantic = map[string]string{unQuestion: pwQuestion}
	postChat(t, g.url, pwQuestion, nil)
	blocked, _ := postChat(t, g.url, unQuestion, nil)
	b := explainOf(t, g.url, blocked)
	if b.How != HowGateBlocked || b.Gate != GateBlocked || b.AuditID == "" || !strings.Contains(b.Plain, "double-check") {
		t.Fatalf("gate explanation = %+v", b)
	}
}

func TestExplainNeverContainsPromptsOrAnswers(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c := newCompanion(t, fb.srv.URL, testPolicyDoc(page.srv.URL, "bypass"))
	secret := "My secret project codename is BLUEHERON and the price of the Acme Pro plan?"
	postChat(t, c.url, secret, nil)
	resp, body := postChat(t, c.url, secret, nil)
	id := resp.Header.Get(HeaderRequestID)
	for _, url := range []string{c.url + "/freshness/explain/" + id, c.url + "/freshness/explain?limit=10"} {
		r, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(r.Body)
		r.Body.Close()
		for _, bad := range []string{"BLUEHERON", "codename", string(body), "Acme Pro plan costs"} {
			if strings.Contains(string(raw), bad) {
				t.Fatalf("%s leaks %q: %s", url, bad, raw)
			}
		}
	}
}

func TestExplainUnknownIDAndList(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c := newCompanion(t, fb.srv.URL, testPolicyDoc(page.srv.URL, "bypass"))
	var errBody struct {
		Error struct{ Message string }
	}
	if code := getJSON(t, c.url+"/freshness/explain/req-nope", &errBody); code != http.StatusNotFound || !strings.Contains(errBody.Error.Message, "No record of that request id") {
		t.Fatalf("code %d body %+v", code, errBody)
	}
	for i := 0; i < 3; i++ {
		postChat(t, c.url, "Tell me a joke", nil)
	}
	var list struct {
		Count    int
		Requests []explainListItem
	}
	if code := getJSON(t, c.url+"/freshness/explain?limit=2", &list); code != 200 || list.Count != 2 || len(list.Requests) != 2 {
		t.Fatalf("list = %+v (HTTP %d)", list, code)
	}
	if list.Requests[0].Summary == "" || list.Requests[0].How != HowDirectHit {
		t.Fatalf("newest first, with a summary: %+v", list.Requests)
	}
	if code := getJSON(t, c.url+"/freshness/explain?limit=zero", nil); code != http.StatusBadRequest {
		t.Fatalf("bad limit: HTTP %d", code)
	}
}

func TestExplainRingEvictsOldest(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c := newCompanionWith(t, fb.srv.URL, testPolicyDoc(page.srv.URL, "bypass"), 0, func(cfg *Config) { cfg.ExplainRing = 3 })
	var ids []string
	for i := 0; i < 5; i++ {
		resp, _ := postChat(t, c.url, "Tell me a joke", nil)
		ids = append(ids, resp.Header.Get(HeaderRequestID))
	}
	for i, id := range ids {
		code := getJSON(t, c.url+"/freshness/explain/"+id, nil)
		if want := map[bool]int{true: 404, false: 200}[i < 2]; code != want {
			t.Errorf("request %d: HTTP %d, want %d", i, code, want)
		}
	}
	var list struct {
		Count    int
		Capacity int
		Requests []explainListItem
	}
	getJSON(t, c.url+"/freshness/explain?limit=10", &list)
	if list.Count != 3 || list.Capacity != 3 || list.Requests[0].ID != ids[4] || list.Requests[2].ID != ids[2] {
		t.Fatalf("list = %+v, ids = %v", list, ids)
	}
}

func TestExplainOnStreamsAndErrorsAndOtherPaths(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c := newCompanion(t, fb.srv.URL, testPolicyDoc(page.srv.URL, "bypass"))

	resp, _ := postJSON(t, c.url+"/v1/chat/completions", map[string]any{
		"model": "m", "stream": true, "messages": []map[string]string{{"role": "user", "content": "hello"}},
	}, map[string]string{"x-bf-cache-key": "demo"})
	e := explainOf(t, c.url, resp)
	if !e.Streamed {
		t.Fatalf("a stream must be marked as streamed: %+v", e)
	}

	// A body that is not JSON is passed through; it still gets an id.
	req, _ := http.NewRequest(http.MethodPost, c.url+"/v1/chat/completions", strings.NewReader("not json"))
	bad, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	bad.Body.Close()
	if bad.Header.Get(HeaderRequestID) == "" {
		t.Fatal("a passthrough chat request must carry an id")
	}

	// Other /v1 paths are not chat answers: no id.
	models, err := http.Get(c.url + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	models.Body.Close()
	if models.Header.Get(HeaderRequestID) != "" {
		t.Fatal("only chat completions get a request id")
	}

	// Bifrost down: a 502 with an id and a plain record.
	down := newFakeBifrost(t, "49")
	dc := newCompanion(t, down.srv.URL, testPolicyDoc(page.srv.URL, "bypass"))
	down.srv.Close()
	failed, _ := postChat(t, dc.url, "hello", nil)
	if failed.StatusCode != http.StatusBadGateway {
		t.Fatalf("HTTP %d", failed.StatusCode)
	}
	f := explainOf(t, dc.url, failed)
	if f.How != HowNotCached || f.HTTPStatus != http.StatusBadGateway || !strings.Contains(f.Plain, "502") {
		t.Fatalf("error explanation = %+v", f)
	}
}
