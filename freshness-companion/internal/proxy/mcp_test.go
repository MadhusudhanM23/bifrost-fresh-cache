package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"freshness-companion/internal/agetrack"
	"freshness-companion/internal/changes"
	"freshness-companion/internal/decision"
	"freshness-companion/internal/decisionlog"
	"freshness-companion/internal/evidence"
	"freshness-companion/internal/policy"
)

// newMCPCompanion is newCompanionWith, but it also returns the *Server so a
// test can call EnableMCP on it.
func newMCPCompanion(t *testing.T, bifrostURL, policyDoc string, tweak func(*Config)) (*Server, string) {
	t.Helper()
	pol, err := policy.Parse([]byte(policyDoc))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cache := evidence.NewCache(evidence.NewDirectFetch(evidence.DirectConfig{Timeout: 2 * time.Second}), evidence.CacheConfig{TTL: time.Hour, FetchTimeout: 5 * time.Second})
	ages, err := agetrack.Open(agetrack.Options{Path: filepath.Join(dir, "ages.json")})
	if err != nil {
		t.Fatal(err)
	}
	dlog, err := decisionlog.Open(filepath.Join(dir, "decisions.jsonl"), 100)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dlog.Close() })
	up, err := NewUpstream(bifrostURL, nil, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	clearer, err := NewBifrostClearer(bifrostURL, "Authorization", "Bearer admin-test-token", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Upstream: up, Policy: pol, Engine: &decision.Engine{Evidence: cache}, Ages: ages, Decisions: dlog,
		Refresh: &ClearAndResend{Clearer: clearer, Sender: up, MaxAttempts: 2}, Clearer: clearer,
		UpstreamTimeout: 10 * time.Second, SettleDelay: 5 * time.Millisecond,
	}
	if tweak != nil {
		tweak(&cfg)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts.URL
}

type mcpToolReply struct {
	Result struct {
		Content []struct {
			Type, Text string
		} `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
	} `json:"result"`
	Error *struct {
		Code    int
		Message string
	} `json:"error"`
}

func mcpCall(t *testing.T, base, tool string, args map[string]any, headers map[string]string) mcpToolReply {
	t.Helper()
	resp, out := postJSON(t, base+"/mcp", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	}, headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /mcp = %d: %s", resp.StatusCode, out)
	}
	var reply mcpToolReply
	if err := json.Unmarshal(out, &reply); err != nil {
		t.Fatalf("not JSON-RPC: %s", out)
	}
	if reply.Error != nil {
		t.Fatalf("JSON-RPC error %d: %s", reply.Error.Code, reply.Error.Message)
	}
	return reply
}

func TestMCPRouteIsOffByDefault(t *testing.T) {
	fb := newFakeBifrost(t, "49")
	_, base := newMCPCompanion(t, fb.srv.URL, policyDocWith("http://127.0.0.1:9/", false, "report"), nil)
	resp, out := postJSON(t, base+"/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"}, nil)
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(out), "-mcp") {
		t.Fatalf("off: %d %s", resp.StatusCode, out)
	}
}

// TestMCPReportChangeEqualsTheHTTPEndpoint sends the same notice through
// POST /freshness/changes and through the MCP tool, and checks that both
// produce the same kind of record, both drop the cached evidence, both are
// listed, and both refuse the same bad inputs with the same messages.
func TestMCPReportChangeEqualsTheHTTPEndpoint(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	srv, base := newMCPCompanion(t, fb.srv.URL, policyDocWith(page.srv.URL, false, "report"), nil)
	tools, err := srv.EnableMCP(MCPOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(tools, ",") != "check_freshness,explain_request,list_watched_sources,report_change" {
		t.Fatalf("tools = %v", tools)
	}

	// Warm the hour-long evidence cache so a notice has evidence to drop.
	postChat(t, base, proPrompt, nil)
	hit, _ := postChat(t, base, proPrompt, nil)
	assertHeader(t, hit, HeaderStatus, "confirmed")

	// 1. HTTP endpoint.
	resp, out := postJSON(t, base+"/freshness/changes", map[string]any{"fact": "Acme Pro plan price", "note": "via http", "clear_cache_keys": []string{"k1"}}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("http notice = %d %s", resp.StatusCode, out)
	}
	var viaHTTP changes.Notice
	_ = json.Unmarshal(out, &viaHTTP)

	// Re-warm the evidence so the MCP notice also has something to drop.
	postChat(t, base, proPrompt, nil)

	// 2. MCP tool, same arguments.
	reply := mcpCall(t, base, "report_change", map[string]any{"fact": "Acme Pro plan price", "note": "via mcp", "clear_cache_keys": []string{"k2"}}, nil)
	if reply.Result.IsError {
		t.Fatalf("mcp notice refused: %s", reply.Result.Content[0].Text)
	}
	var viaMCP changes.Notice
	if err := json.Unmarshal(reply.Result.StructuredContent, &viaMCP); err != nil {
		t.Fatal(err)
	}

	if viaHTTP.Kind != viaMCP.Kind || viaHTTP.Target != viaMCP.Target || len(viaHTTP.Facts) != 1 || len(viaMCP.Facts) != 1 || viaHTTP.Facts[0] != viaMCP.Facts[0] {
		t.Fatalf("records differ:\n http %+v\n mcp  %+v", viaHTTP, viaMCP)
	}
	if len(viaHTTP.EvidenceDropped) != 1 || len(viaMCP.EvidenceDropped) != 1 || viaHTTP.EvidenceDropped[0] != viaMCP.EvidenceDropped[0] {
		t.Fatalf("evidence dropped differs: http %v mcp %v", viaHTTP.EvidenceDropped, viaMCP.EvidenceDropped)
	}
	if len(viaHTTP.ClearedCacheKeys) != 1 || viaHTTP.ClearedCacheKeys[0] != "k1" || len(viaMCP.ClearedCacheKeys) != 1 || viaMCP.ClearedCacheKeys[0] != "k2" {
		t.Fatalf("cleared keys: http %v mcp %v", viaHTTP.ClearedCacheKeys, viaMCP.ClearedCacheKeys)
	}
	if viaHTTP.Note != "via http" || viaMCP.Note != "via mcp" || viaHTTP.ID == viaMCP.ID || viaHTTP.ID == "" || viaMCP.ID == "" {
		t.Fatalf("ids/notes: http %+v mcp %+v", viaHTTP, viaMCP)
	}
	fb.mu.Lock()
	clearedKeys := append([]string(nil), fb.clearKeys...)
	fb.mu.Unlock()
	if strings.Join(clearedKeys, ",") != "k1,k2" {
		t.Fatalf("Bifrost saw clear-by-key %v, want k1 and k2", clearedKeys)
	}

	// Both are in the same list, newest first.
	listed, err := http.Get(base + "/freshness/changes")
	if err != nil {
		t.Fatal(err)
	}
	defer listed.Body.Close()
	var list struct {
		Count   int              `json:"count"`
		Notices []changes.Notice `json:"notices"`
	}
	_ = json.NewDecoder(listed.Body).Decode(&list)
	if list.Count != 2 || list.Notices[0].ID != viaMCP.ID || list.Notices[1].ID != viaHTTP.ID {
		t.Fatalf("list = %+v", list)
	}

	// The MCP list tool shows the same notices.
	reply = mcpCall(t, base, "list_watched_sources", nil, nil)
	if !strings.Contains(string(reply.Result.StructuredContent), viaHTTP.ID) || !strings.Contains(string(reply.Result.StructuredContent), viaMCP.ID) {
		t.Fatalf("list_watched_sources = %s", reply.Result.StructuredContent)
	}

	// Same refusals, same words.
	bad := []struct {
		name string
		args map[string]any
	}{
		{"unknown fact", map[string]any{"fact": "nope"}},
		{"unknown category", map[string]any{"category": "weather"}},
		{"unknown source", map[string]any{"source_url": "http://elsewhere.test/"}},
		{"two selectors", map[string]any{"fact": "Acme Pro plan price", "category": "price"}},
		{"nothing", map[string]any{"note": "x"}},
	}
	for _, tc := range bad {
		resp, out := postJSON(t, base+"/freshness/changes", tc.args, nil)
		var httpErr struct {
			Error struct{ Message string } `json:"error"`
		}
		_ = json.Unmarshal(out, &httpErr)
		if resp.StatusCode == http.StatusOK || httpErr.Error.Message == "" {
			t.Fatalf("%s: http accepted it: %d %s", tc.name, resp.StatusCode, out)
		}
		reply := mcpCall(t, base, "report_change", tc.args, nil)
		if !reply.Result.IsError || reply.Result.Content[0].Text != httpErr.Error.Message {
			t.Fatalf("%s: mcp said %+v, http said %q", tc.name, reply.Result, httpErr.Error.Message)
		}
	}
	if list, _ := http.Get(base + "/freshness/changes"); list != nil {
		var l struct {
			Count int `json:"count"`
		}
		_ = json.NewDecoder(list.Body).Decode(&l)
		list.Body.Close()
		if l.Count != 2 {
			t.Fatalf("refused notices were recorded: count %d", l.Count)
		}
	}
}

func TestMCPReportChangeUsesTheSameToken(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	srv, base := newMCPCompanion(t, fb.srv.URL, policyDocWith(page.srv.URL, false, "report"), func(cfg *Config) { cfg.ChangeToken = "s3cret" })
	if _, err := srv.EnableMCP(MCPOptions{}); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"category": "policy"}
	if resp, _ := postJSON(t, base+"/freshness/changes", args, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("http without token = %d", resp.StatusCode)
	}
	if reply := mcpCall(t, base, "report_change", args, nil); !reply.Result.IsError || !strings.Contains(reply.Result.Content[0].Text, "unauthorized") {
		t.Fatalf("mcp without token = %+v", reply.Result)
	}
	if reply := mcpCall(t, base, "report_change", args, map[string]string{"Authorization": "Bearer s3cret"}); reply.Result.IsError {
		t.Fatalf("mcp with bearer = %+v", reply.Result)
	}
	if reply := mcpCall(t, base, "report_change", args, map[string]string{"X-Freshness-Notice-Token": "s3cret"}); reply.Result.IsError {
		t.Fatalf("mcp with header = %+v", reply.Result)
	}
	// Reads need no token.
	if reply := mcpCall(t, base, "list_watched_sources", nil, nil); reply.Result.IsError {
		t.Fatal("list refused")
	}
}

// TestMCPExplainAndCheckUseTheProxyState checks the two read tools against
// what the proxy itself did.
func TestMCPExplainAndCheckUseTheProxyState(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	srv, base := newMCPCompanion(t, fb.srv.URL, policyDocWith(page.srv.URL, false, "report"), nil)
	if _, err := srv.EnableMCP(MCPOptions{}); err != nil {
		t.Fatal(err)
	}
	postChat(t, base, proPrompt, nil)
	hit, _ := postChat(t, base, proPrompt, nil)
	id := hit.Header.Get(HeaderRequestID)
	if id == "" {
		t.Fatal("no request id")
	}
	reply := mcpCall(t, base, "explain_request", map[string]any{"request_id": id}, nil)
	var e Explanation
	if err := json.Unmarshal(reply.Result.StructuredContent, &e); err != nil {
		t.Fatal(err)
	}
	if reply.Result.IsError || e.ID != id || e.How != HowDirectHit || e.FreshnessStatus != "confirmed" || reply.Result.Content[0].Text != e.Plain || e.Plain == "" {
		t.Fatalf("explain = %+v text %q", e, reply.Result.Content[0].Text)
	}
	if reply := mcpCall(t, base, "explain_request", map[string]any{"request_id": "req-00000000-999"}, nil); !reply.Result.IsError || !strings.Contains(reply.Result.Content[0].Text, "not found") {
		t.Fatalf("missing = %+v", reply.Result)
	}

	// check_freshness reads through the same evidence cache the proxy uses.
	page.set("59", 0, "")
	reply = mcpCall(t, base, "check_freshness", map[string]any{"question": proPrompt, "answer": "$49 per month"}, nil)
	var out struct {
		Outcome string `json:"outcome"`
		Facts   []struct {
			CurrentValue   string `json:"current_value"`
			EvidenceCached bool   `json:"evidence_cached"`
		} `json:"facts"`
	}
	_ = json.Unmarshal(reply.Result.StructuredContent, &out)
	// The hour-long evidence cache still holds 49, exactly as the proxy would see it.
	if out.Outcome != "confirmed" || len(out.Facts) != 1 || out.Facts[0].CurrentValue != "49" || !out.Facts[0].EvidenceCached {
		t.Fatalf("cached evidence: %s", reply.Result.StructuredContent)
	}
	// A notice drops it; the next check reads the page again and sees 59.
	mcpCall(t, base, "report_change", map[string]any{"fact": "Acme Pro plan price"}, nil)
	reply = mcpCall(t, base, "check_freshness", map[string]any{"question": proPrompt, "answer": "$49 per month"}, nil)
	_ = json.Unmarshal(reply.Result.StructuredContent, &out)
	if out.Outcome != "stale" || out.Facts[0].CurrentValue != "59" {
		t.Fatalf("after notice: %s", reply.Result.StructuredContent)
	}
}
