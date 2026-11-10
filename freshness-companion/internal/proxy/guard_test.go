package proxy

import (
	"encoding/json"
	"freshness-companion/internal/policy"
	"net/http"
	"strings"
	"testing"
)

func guardPolicy(sourceURL, guard string) string {
	doc := testPolicyDoc(sourceURL, "bypass")
	return strings.Replace(doc, `"version": 1,`, `"version": 1, "guard": `+guard+`,`, 1)
}

func postTools(t *testing.T, base string, messages []map[string]any, tools []string) (*http.Response, []byte) {
	t.Helper()
	body := map[string]any{"model": "openai/gpt-4o-mini", "messages": messages}
	if len(tools) > 0 {
		var ts []map[string]any
		for _, n := range tools {
			ts = append(ts, map[string]any{"type": "function", "function": map[string]any{"name": n}})
		}
		body["tools"] = ts
	}
	return postJSON(t, base+"/v1/chat/completions", body, map[string]string{"x-bf-cache-key": "demo"})
}

var withToolResult = []map[string]any{
	{"role": "user", "content": "What is the status of my order?"},
	{"role": "tool", "content": `{"status":"shipped"}`, "tool_call_id": "c1"},
}

func TestGuardKeepsToolResultRequestsOutOfTheCache(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c := newCompanion(t, fb.srv.URL, guardPolicy(page.srv.URL, `{"tool_results": "bypass"}`))

	first, _ := postTools(t, c.url, withToolResult, nil)
	second, _ := postTools(t, c.url, withToolResult, nil)
	for i, r := range []*http.Response{first, second} {
		if r.Header.Get(HeaderGuard) != "bypassed: tool_results" || r.Header.Get(HeaderCacheHit) == "true" {
			t.Fatalf("request %d: guard %q, cache hit %q", i, r.Header.Get(HeaderGuard), r.Header.Get(HeaderCacheHit))
		}
	}
	fb.mu.Lock()
	llm, entries, keys := fb.llmCalls, len(fb.entries), append([]string(nil), fb.keys...)
	fb.mu.Unlock()
	if llm != 2 || entries != 0 {
		t.Fatalf("both asks must reach the model and leave nothing cached: llm %d, cached entries %d", llm, entries)
	}
	for _, k := range keys {
		if k != guardBypassKey {
			t.Fatalf("a guarded request must use the bypass key, not %q", k)
		}
	}
	// An ordinary request with the same client key still caches.
	postChat(t, c.url, "Tell me a joke", nil)
	hit, _ := postChat(t, c.url, "Tell me a joke", nil)
	assertHeader(t, hit, HeaderCacheHit, "true")

	resp, err := http.Get(c.url + "/freshness/guard")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st GuardStats
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if !st.Enabled || st.Bypassed != 2 || st.ByReason[policyGuardToolResults] != 2 {
		t.Fatalf("stats = %+v", st)
	}
}

const policyGuardToolResults = "tool_results"

func TestGuardIsOffByDefault(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c := newCompanion(t, fb.srv.URL, testPolicyDoc(page.srv.URL, "bypass"))
	postTools(t, c.url, withToolResult, nil)
	second, _ := postTools(t, c.url, withToolResult, nil)
	assertHeader(t, second, HeaderCacheHit, "true")
	if second.Header.Get(HeaderGuard) != "" {
		t.Fatal("the guard must do nothing unless the policy turns it on")
	}
}

func TestGuardSideEffectTools(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c := newCompanion(t, fb.srv.URL, guardPolicy(page.srv.URL, `{"side_effect_tools": ["create_*", "Send_Email"]}`))
	ask := []map[string]any{{"role": "user", "content": "Open a ticket for my broken screen"}}

	for _, tools := range [][]string{{"lookup_order", "Create_Ticket"}, {"send_email"}} {
		postTools(t, c.url, ask, tools)
		again, _ := postTools(t, c.url, ask, tools)
		if again.Header.Get(HeaderGuard) != "bypassed: side_effect_tool" || again.Header.Get(HeaderCacheHit) == "true" {
			t.Fatalf("tools %v: guard %q, hit %q", tools, again.Header.Get(HeaderGuard), again.Header.Get(HeaderCacheHit))
		}
	}
	// Read-only tools are cached as usual.
	postTools(t, c.url, ask, []string{"get_weather"})
	read, _ := postTools(t, c.url, ask, []string{"get_weather"})
	assertHeader(t, read, HeaderCacheHit, "true")
	if read.Header.Get(HeaderGuard) != "" {
		t.Fatal("a read-only tool must not be guarded")
	}
}

func TestGuardRejectsBadPolicy(t *testing.T) {
	page := newFakePage(t, "49")
	for _, g := range []string{`{"tool_results": "maybe"}`, `{"side_effect_tools": ["[x"]}`} {
		if _, err := policy.Parse([]byte(guardPolicy(page.srv.URL, g))); err == nil {
			t.Errorf("guard %s should be rejected", g)
		}
	}
}
