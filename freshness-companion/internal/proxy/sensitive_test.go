package proxy

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"freshness-companion/internal/policy"
)

const sensitiveGuard = `{"sensitive": "bypass"}`

func ask(t *testing.T, base string, messages []map[string]any) *http.Response {
	t.Helper()
	resp, _ := postJSON(t, base+"/v1/chat/completions", map[string]any{"model": "openai/gpt-4o-mini", "messages": messages}, map[string]string{"x-bf-cache-key": "demo"})
	return resp
}

func TestSensitiveTextIsKeptOutOfTheCache(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c := newCompanion(t, fb.srv.URL, guardPolicy(page.srv.URL, sensitiveGuard))

	cases := map[string]struct {
		messages []map[string]any
		kind     string
	}{
		"user email":    {[]map[string]any{{"role": "user", "content": "Send the invoice to jane.doe@example.com"}}, "email"},
		"system card":   {[]map[string]any{{"role": "system", "content": "Customer card 4111 1111 1111 1111"}, {"role": "user", "content": "hi"}}, "card"},
		"parts array":   {[]map[string]any{{"role": "user", "content": []map[string]any{{"type": "text", "text": "my ssn is 123-45-6789"}}}}, "ssn"},
		"assistant key": {[]map[string]any{{"role": "assistant", "content": "use sk-proj-AbCdEfGhIjKlMnOpQrStUv123456"}, {"role": "user", "content": "ok"}}, "api_key"},
	}
	for name, c1 := range cases {
		first := ask(t, c.url, c1.messages)
		second := ask(t, c.url, c1.messages)
		for i, r := range []*http.Response{first, second} {
			if r.Header.Get(HeaderGuard) != "bypassed: sensitive:"+c1.kind || r.Header.Get(HeaderCacheHit) == "true" {
				t.Fatalf("%s, request %d: guard %q, hit %q", name, i, r.Header.Get(HeaderGuard), r.Header.Get(HeaderCacheHit))
			}
		}
	}
	fb.mu.Lock()
	entries, keys := len(fb.entries), append([]string(nil), fb.keys...)
	fb.mu.Unlock()
	if entries != 0 {
		t.Fatalf("nothing sensitive may be cached, found %d entries", entries)
	}
	for _, k := range keys {
		if k != guardBypassKey {
			t.Fatalf("a sensitive request must use the bypass key, not %q", k)
		}
	}
	// Ordinary requests still cache.
	ask(t, c.url, []map[string]any{{"role": "user", "content": "Tell me a joke"}})
	hit := ask(t, c.url, []map[string]any{{"role": "user", "content": "Tell me a joke"}})
	assertHeader(t, hit, HeaderCacheHit, "true")
}

func TestSensitiveStatsExplainAndLogsNeverHoldTheText(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c := newCompanion(t, fb.srv.URL, guardPolicy(page.srv.URL, sensitiveGuard))
	secretEmail := "zebra.secret@hidden-domain.example"
	secretCard := "4111111111111111"
	r1 := ask(t, c.url, []map[string]any{{"role": "user", "content": "mail " + secretEmail}})
	ask(t, c.url, []map[string]any{{"role": "user", "content": "card " + secretCard}})
	ask(t, c.url, []map[string]any{{"role": "user", "content": "mail " + secretEmail}})

	var st GuardStats
	getJSON(t, c.url+"/freshness/guard", &st)
	if !st.Enabled || st.Bypassed != 3 || st.ByReason["sensitive:email"] != 2 || st.ByReason["sensitive:card"] != 1 || len(st.ByReason) != 2 {
		t.Fatalf("stats = %+v", st)
	}
	e := explainOf(t, c.url, r1)
	if e.How != HowGuarded || e.GuardReason != "sensitive:email" || !strings.Contains(e.Plain, "an email address") {
		t.Fatalf("explanation = %+v", e)
	}
	var all []string
	for _, url := range []string{"/freshness/guard", "/freshness/explain?limit=20", "/freshness/explain/" + r1.Header.Get(HeaderRequestID), "/freshness/decisions", "/healthz"} {
		resp, err := http.Get(c.url + url)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		all = append(all, string(raw), headersText(resp.Header))
	}
	all = append(all, headersText(r1.Header))
	for _, text := range all {
		if strings.Contains(text, "zebra") || strings.Contains(text, "hidden-domain") || strings.Contains(text, secretCard) {
			t.Fatalf("matched text leaked: %s", text)
		}
	}
}

func headersText(h http.Header) string {
	var b strings.Builder
	for k, v := range h {
		b.WriteString(k + ": " + strings.Join(v, ",") + "\n")
	}
	return b.String()
}

func TestSensitiveKindsNarrowTheRule(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c := newCompanion(t, fb.srv.URL, guardPolicy(page.srv.URL, `{"sensitive": "bypass", "sensitive_kinds": ["card"]}`))
	email := []map[string]any{{"role": "user", "content": "contact bob@corp.io"}}
	ask(t, c.url, email)
	again := ask(t, c.url, email)
	assertHeader(t, again, HeaderCacheHit, "true") // email is not in the chosen kinds
	card := []map[string]any{{"role": "user", "content": "card 4111 1111 1111 1111"}}
	assertHeader(t, ask(t, c.url, card), HeaderGuard, "bypassed: sensitive:card")
}

func TestSensitiveIsOffByDefault(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c := newCompanion(t, fb.srv.URL, testPolicyDoc(page.srv.URL, "bypass"))
	m := []map[string]any{{"role": "user", "content": "contact bob@corp.io"}}
	ask(t, c.url, m)
	second := ask(t, c.url, m)
	assertHeader(t, second, HeaderCacheHit, "true")
	if second.Header.Get(HeaderGuard) != "" {
		t.Fatal("the rule must do nothing unless the policy turns it on")
	}
	// "allow" is the same as absent.
	c2 := newCompanion(t, fb.srv.URL, guardPolicy(page.srv.URL, `{"sensitive": "allow"}`))
	ask(t, c2.url, m)
	assertHeader(t, ask(t, c2.url, m), HeaderCacheHit, "true")
}

func TestSensitiveStreamsAreGuardedToo(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c := newCompanion(t, fb.srv.URL, guardPolicy(page.srv.URL, sensitiveGuard))
	resp, _ := postJSON(t, c.url+"/v1/chat/completions", map[string]any{
		"model": "m", "stream": true, "messages": []map[string]string{{"role": "user", "content": "mail me at a.b@c.org"}},
	}, map[string]string{"x-bf-cache-key": "demo"})
	assertHeader(t, resp, HeaderGuard, "bypassed: sensitive:email")
}

func TestSensitivePolicyValidation(t *testing.T) {
	page := newFakePage(t, "49")
	for _, g := range []string{`{"sensitive": "maybe"}`, `{"sensitive": "bypass", "sensitive_kinds": ["passport"]}`} {
		if _, err := policy.Parse([]byte(guardPolicy(page.srv.URL, g))); err == nil {
			t.Errorf("guard %s should be rejected", g)
		}
	}
	if _, err := policy.Parse([]byte(guardPolicy(page.srv.URL, `{"sensitive": "bypass", "sensitive_kinds": ["email", "phone"]}`))); err != nil {
		t.Error(err)
	}
}
