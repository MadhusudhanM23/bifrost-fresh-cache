package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"freshness-companion/internal/changes"
	"freshness-companion/internal/evidence"
	"freshness-companion/internal/policy"
)

// ---------------------------------------------------------------------------
// Fixtures: a fake source page, a policy that watches it, a fake notice store
// and a recording Report function.
// ---------------------------------------------------------------------------

type fakePage struct {
	mu     sync.Mutex
	price  string
	status int
	srv    *httptest.Server
}

func newFakePage(t *testing.T, price string) *fakePage {
	t.Helper()
	p := &fakePage{price: price}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.status != 0 {
			http.Error(w, "unavailable", p.status)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<html><body><p>Acme Pro plan — $%s per month</p></body></html>`, p.price)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakePage) set(price string, status int) {
	p.mu.Lock()
	p.price, p.status = price, status
	p.mu.Unlock()
}

func testPolicy(t *testing.T, sourceURL string) *policy.Policy {
	t.Helper()
	doc := fmt.Sprintf(`{
	  "version": 1,
	  "categories": [
	    {"name": "price", "keywords": ["price", "cost", "how much"], "max_age": "10m", "on_unverifiable": "bypass"},
	    {"name": "policy", "keywords": ["refund*", "policy"], "max_age": "24h", "on_unverifiable": "serve_with_warning"}
	  ],
	  "facts": [{
	    "name": "Acme Pro plan price", "category": "price", "keywords": ["pro"],
	    "source_url": %q,
	    "source_pattern": "Acme Pro plan[^$0-9]{0,20}\\$\\s*([0-9][0-9,]*(?:\\.[0-9]+)?)\\s*per month",
	    "answer_pattern": "(?i)\\$\\s*([0-9][0-9,]*(?:\\.[0-9]+)?)\\s*(?:/\\s*mo(?:nth)?|per\\s+month|a\\s+month|monthly)\\b",
	    "compare": "numeric"
	  }]
	}`, sourceURL)
	pol, err := policy.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return pol
}

type fakeChanges struct{ notices []changes.Notice }

func (f *fakeChanges) Recent(limit int) []changes.Notice {
	if limit > len(f.notices) {
		limit = len(f.notices)
	}
	return f.notices[:limit]
}

type recorder struct {
	mu    sync.Mutex
	calls []ChangeRequest
	err   error
}

func (r *recorder) report(_ context.Context, req ChangeRequest) (changes.Notice, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, req)
	if r.err != nil {
		return changes.Notice{}, r.err
	}
	n := changes.Notice{ID: "chg-1", Time: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC), Note: req.Note}
	switch {
	case req.Fact != "":
		n.Kind, n.Target, n.Facts = "fact", req.Fact, []string{req.Fact}
	case req.Category != "":
		n.Kind, n.Target, n.Categories = "category", req.Category, []string{req.Category}
	default:
		n.Kind, n.Target = "source_url", req.SourceURL
	}
	n.EvidenceDropped = []string{"http://example.test/"}
	return n, nil
}

type fixture struct {
	page    *fakePage
	rec     *recorder
	store   *fakeChanges
	srv     *Server
	ts      *httptest.Server
	explain map[string]string
}

func newFixture(t *testing.T, tweak func(*Deps, *Options)) *fixture {
	t.Helper()
	page := newFakePage(t, "49")
	pol := testPolicy(t, page.srv.URL)
	cache := evidence.NewCache(evidence.NewDirectFetch(evidence.DirectConfig{Timeout: 2 * time.Second}), evidence.CacheConfig{FetchTimeout: 2 * time.Second})
	f := &fixture{page: page, rec: &recorder{}, store: &fakeChanges{}, explain: map[string]string{"req-abc-1": "This answer came from the cache."}}
	deps := Deps{
		Policy:   pol,
		Evidence: cache,
		Explain: func(id string) (string, any, bool) {
			plain, ok := f.explain[id]
			if !ok {
				return "", nil, false
			}
			return plain, map[string]any{"id": id, "how": "direct_hit", "plain": plain}, true
		},
		ExplainKept: 2000,
		Changes:     f.store,
		Report:      f.rec.report,
	}
	opts := Options{}
	if tweak != nil {
		tweak(&deps, &opts)
	}
	srv, err := New(deps, opts)
	if err != nil {
		t.Fatal(err)
	}
	f.srv = srv
	mux := http.NewServeMux()
	mux.Handle("POST /mcp", srv)
	f.ts = httptest.NewServer(mux)
	t.Cleanup(f.ts.Close)
	return f
}

// rpc posts one JSON-RPC request and returns the HTTP response and body.
func (f *fixture) rpc(t *testing.T, body string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, f.ts.URL+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
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

type rpcReply struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

func (f *fixture) call(t *testing.T, id int, method string, params any) rpcReply {
	t.Helper()
	return f.callWith(t, id, method, params, nil)
}

func (f *fixture) callWith(t *testing.T, id int, method string, params any, headers map[string]string) rpcReply {
	t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		body["params"] = params
	}
	raw, _ := json.Marshal(body)
	resp, out := f.rpc(t, string(raw), headers)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("%s: HTTP %d %s: %s", method, resp.StatusCode, resp.Header.Get("Content-Type"), out)
	}
	var reply rpcReply
	if err := json.Unmarshal(out, &reply); err != nil {
		t.Fatalf("%s: not JSON-RPC: %s", method, out)
	}
	if reply.JSONRPC != "2.0" || string(reply.ID) != fmt.Sprint(id) {
		t.Fatalf("%s: wrong envelope: %s", method, out)
	}
	return reply
}

// tool calls a tool and decodes its result.
func (f *fixture) tool(t *testing.T, name string, args map[string]any, headers map[string]string) (toolResult, json.RawMessage) {
	t.Helper()
	reply := f.callWith(t, 7, "tools/call", map[string]any{"name": name, "arguments": args}, headers)
	if reply.Error != nil {
		t.Fatalf("%s: JSON-RPC error %d: %s", name, reply.Error.Code, reply.Error.Message)
	}
	var res struct {
		Content           []content       `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
	}
	if err := json.Unmarshal(reply.Result, &res); err != nil {
		t.Fatalf("%s: bad result %s", name, reply.Result)
	}
	if len(res.Content) != 1 || res.Content[0].Type != "text" || res.Content[0].Text == "" {
		t.Fatalf("%s: want one text content, got %s", name, reply.Result)
	}
	return toolResult{Content: res.Content, IsError: res.IsError}, res.StructuredContent
}

// ---------------------------------------------------------------------------
// Transport and handshake
// ---------------------------------------------------------------------------

func TestInitializeNegotiatesTheProtocolVersion(t *testing.T) {
	cases := []struct {
		cap, requested, want string
	}{
		{"", "2025-06-18", "2025-06-18"},
		{"", "2025-11-25", "2025-06-18"}, // above the default cap: offer the cap
		{"", "2026-07-28", "2025-06-18"},
		{"", "1999-01-01", "2025-06-18"}, // unknown: offer the newest we support
		{"", "", "2025-06-18"},
		{"2025-11-25", "2025-11-25", "2025-11-25"},
		{"2025-11-25", "2025-06-18", "2025-06-18"}, // older than the cap and known: echoed
		{"2025-11-25", "2026-07-28", "2025-11-25"},
	}
	for _, tc := range cases {
		f := newFixture(t, func(_ *Deps, o *Options) { o.MaxProtocolVersion = tc.cap })
		reply := f.call(t, 1, "initialize", map[string]any{
			"protocolVersion": tc.requested,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "bifrost", "version": "test"},
		})
		if reply.Error != nil {
			t.Fatalf("initialize error: %+v", reply.Error)
		}
		var res struct {
			ProtocolVersion string `json:"protocolVersion"`
			Capabilities    struct {
				Tools *struct {
					ListChanged bool `json:"listChanged"`
				} `json:"tools"`
			} `json:"capabilities"`
			ServerInfo struct {
				Name, Version string
			} `json:"serverInfo"`
		}
		_ = json.Unmarshal(reply.Result, &res)
		if res.ProtocolVersion != tc.want {
			t.Errorf("cap %q requested %q: got %q want %q", tc.cap, tc.requested, res.ProtocolVersion, tc.want)
		}
		if res.Capabilities.Tools == nil || res.Capabilities.Tools.ListChanged || res.ServerInfo.Name != "freshness-companion" || res.ServerInfo.Version == "" {
			t.Errorf("initialize result = %s", reply.Result)
		}
	}
	if _, err := New(Deps{Policy: policy.Default()}, Options{MaxProtocolVersion: "2030-01-01"}); err == nil {
		t.Fatal("an unknown cap must be refused")
	}
}

func TestNotificationIsAcceptedWithNoBody(t *testing.T) {
	f := newFixture(t, nil)
	resp, out := f.rpc(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil)
	if resp.StatusCode != http.StatusAccepted || len(out) != 0 {
		t.Fatalf("HTTP %d body %q, want 202 and empty", resp.StatusCode, out)
	}
	resp, out = f.rpc(t, `{"jsonrpc":"2.0","id":null,"method":"notifications/cancelled"}`, nil)
	if resp.StatusCode != http.StatusAccepted || len(out) != 0 {
		t.Fatalf("null id: HTTP %d body %q", resp.StatusCode, out)
	}
}

func TestUnknownMethodsAnswerMethodNotFound(t *testing.T) {
	f := newFixture(t, nil)
	for _, m := range []string{"server/discover", "resources/list", "prompts/list", "nonsense"} {
		reply := f.call(t, 3, m, nil)
		if reply.Error == nil || reply.Error.Code != codeMethodNotFound || !strings.Contains(reply.Error.Message, m) {
			t.Fatalf("%s: want -32601, got %+v", m, reply.Error)
		}
	}
	if reply := f.call(t, 4, "ping", nil); reply.Error != nil || string(reply.Result) != "{}" {
		t.Fatalf("ping = %s / %+v", reply.Result, reply.Error)
	}
}

func TestMalformedBodies(t *testing.T) {
	f := newFixture(t, nil)
	for name, body := range map[string]string{
		"not json": `{"jsonrpc":`,
		"batch":    `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`,
	} {
		resp, out := f.rpc(t, body, nil)
		var reply rpcReply
		_ = json.Unmarshal(out, &reply)
		if resp.StatusCode != http.StatusOK || reply.Error == nil || reply.Error.Code != codeParse || string(reply.ID) != "null" {
			t.Fatalf("%s: HTTP %d %s", name, resp.StatusCode, out)
		}
	}
	resp, out := f.rpc(t, `{"jsonrpc":"2.0","id":9}`, nil)
	var reply rpcReply
	_ = json.Unmarshal(out, &reply)
	if resp.StatusCode != http.StatusOK || reply.Error == nil || reply.Error.Code != codeInvalidRequest || string(reply.ID) != "9" {
		t.Fatalf("missing method: HTTP %d %s", resp.StatusCode, out)
	}

	// GET is refused by the handler itself (and by a "POST /mcp" mux pattern).
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mcp", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d", rec.Code)
	}
	get, err := http.Get(f.ts.URL + "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	get.Body.Close()
	if get.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET through the mux = %d", get.StatusCode)
	}

	big := bytes.Repeat([]byte("x"), maxBody+10)
	resp, out = f.rpc(t, string(big), nil)
	_ = json.Unmarshal(out, &reply)
	if resp.StatusCode != http.StatusOK || reply.Error == nil || reply.Error.Code != codeInvalidRequest {
		t.Fatalf("oversized: HTTP %d %s", resp.StatusCode, out)
	}
}

// ---------------------------------------------------------------------------
// tools/list
// ---------------------------------------------------------------------------

func TestToolsListShape(t *testing.T) {
	f := newFixture(t, nil)
	reply := f.call(t, 2, "tools/list", nil)
	if reply.Error != nil {
		t.Fatal(reply.Error)
	}
	var res struct {
		Tools []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			InputSchema struct {
				Type       string         `json:"type"`
				Properties map[string]any `json:"properties"`
				Required   []string       `json:"required"`
			} `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(reply.Result, &res); err != nil {
		t.Fatal(err)
	}
	want := []string{ToolCheckFreshness, ToolExplainRequest, ToolListWatchedSources, ToolReportChange}
	if len(res.Tools) != len(want) {
		t.Fatalf("got %d tools", len(res.Tools))
	}
	for i, tl := range res.Tools {
		if tl.Name != want[i] || tl.Description == "" || tl.InputSchema.Type != "object" || tl.InputSchema.Properties == nil || tl.InputSchema.Required == nil {
			t.Fatalf("tool %d = %+v", i, tl)
		}
	}
	if r := res.Tools[0].InputSchema.Required; len(r) != 1 || r[0] != "question" {
		t.Fatalf("check_freshness required = %v", r)
	}
	if r := res.Tools[1].InputSchema.Required; len(r) != 1 || r[0] != "request_id" {
		t.Fatalf("explain_request required = %v", r)
	}
	if len(res.Tools[2].InputSchema.Required) != 0 || len(res.Tools[3].InputSchema.Required) != 0 {
		t.Fatal("list_watched_sources and report_change must require nothing")
	}
	// Unknown tool and bad params are protocol errors, not tool errors.
	if reply := f.call(t, 5, "tools/call", map[string]any{"name": "nope", "arguments": map[string]any{}}); reply.Error == nil || reply.Error.Code != codeInvalidParams {
		t.Fatalf("unknown tool = %+v", reply.Error)
	}
	if reply := f.call(t, 6, "tools/call", map[string]any{"name": ToolCheckFreshness, "arguments": "text"}); reply.Error == nil || reply.Error.Code != codeInvalidParams {
		t.Fatalf("bad arguments = %+v", reply.Error)
	}
}

// ---------------------------------------------------------------------------
// check_freshness
// ---------------------------------------------------------------------------

func decodeCheck(t *testing.T, raw json.RawMessage) checkResult {
	t.Helper()
	var out checkResult
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("structuredContent %s: %v", raw, err)
	}
	return out
}

func TestCheckFreshnessConfirmedStaleAndNoAnswer(t *testing.T) {
	f := newFixture(t, nil)
	const q = "How much does the Acme Pro plan cost per month?"

	res, raw := f.tool(t, ToolCheckFreshness, map[string]any{"question": q, "answer": "The Acme Pro plan is $49 per month."}, nil)
	out := decodeCheck(t, raw)
	if res.IsError || out.Outcome != "confirmed" || out.Action != "serve" || !out.TimeSensitive || out.Category != "price" || out.MaxAge != "10m0s" {
		t.Fatalf("confirmed: %+v (%s)", out, res.Content[0].Text)
	}
	if len(out.Facts) != 1 || out.Facts[0].Verdict != "confirmed" || out.Facts[0].CurrentValue != "49" || out.Facts[0].ValueInAnswer != "49" || out.Facts[0].SourceURL != f.page.srv.URL {
		t.Fatalf("facts = %+v", out.Facts)
	}
	if !strings.Contains(res.Content[0].Text, "current") {
		t.Fatalf("text = %q", res.Content[0].Text)
	}

	f.page.set("59", 0)
	res, raw = f.tool(t, ToolCheckFreshness, map[string]any{"question": q, "answer": "It costs $49/month."}, nil)
	out = decodeCheck(t, raw)
	if res.IsError || out.Outcome != "stale" || out.Action != "refresh" || out.Facts[0].Verdict != "stale" || out.Facts[0].CurrentValue != "59" {
		t.Fatalf("stale: %+v", out)
	}
	if !strings.Contains(res.Content[0].Text, "out of date") || !strings.Contains(res.Content[0].Text, "59") {
		t.Fatalf("text = %q", res.Content[0].Text)
	}

	res, raw = f.tool(t, ToolCheckFreshness, map[string]any{"question": q}, nil)
	out = decodeCheck(t, raw)
	if res.IsError || out.Outcome != OutcomeNotCmp || out.Action != ActionNone || out.Facts[0].Verdict != VerdictNoAnswer || out.Facts[0].CurrentValue != "59" {
		t.Fatalf("no answer: %+v", out)
	}
	if !strings.Contains(res.Content[0].Text, "currently reads 59") {
		t.Fatalf("text = %q", res.Content[0].Text)
	}
}

func TestCheckFreshnessPolicyEdges(t *testing.T) {
	f := newFixture(t, nil)

	// Not time-sensitive: nothing to check.
	res, raw := f.tool(t, ToolCheckFreshness, map[string]any{"question": "Tell me a fun fact about owls."}, nil)
	if out := decodeCheck(t, raw); res.IsError || out.TimeSensitive || out.Outcome != OutcomeNotTime || out.Action != "serve" || len(out.Facts) != 0 {
		t.Fatalf("not time-sensitive: %+v", out)
	}

	// Time-sensitive, no watched fact: the policy's max_age is the answer.
	res, raw = f.tool(t, ToolCheckFreshness, map[string]any{"question": "What is your refund policy?", "answer": "30 days."}, nil)
	if out := decodeCheck(t, raw); res.IsError || out.Outcome != "unknown" || out.Action != ActionNone || out.Category != "policy" || out.MaxAge != "24h0m0s" || len(out.Facts) != 0 {
		t.Fatalf("no fact: %+v", out)
	}

	// The category override applies a category the keywords would not pick.
	res, raw = f.tool(t, ToolCheckFreshness, map[string]any{"question": "Tell me about the Pro tier", "category": "price", "answer": "$49 per month"}, nil)
	if out := decodeCheck(t, raw); res.IsError || out.Outcome != "confirmed" || out.Category != "price" {
		t.Fatalf("override: %+v", out)
	}

	// Answer with no readable value: unable_to_verify, and price says bypass -> refresh.
	res, raw = f.tool(t, ToolCheckFreshness, map[string]any{"question": "How much is the Pro plan?", "answer": "It depends."}, nil)
	if out := decodeCheck(t, raw); res.IsError || out.Outcome != "unable_to_verify" || out.Action != "refresh" || out.Facts[0].AnswerError == "" || out.Facts[0].CurrentValue != "49" {
		t.Fatalf("no value in answer: %+v", out)
	}

	// Source down: unable_to_verify with the error named.
	f.page.set("49", http.StatusServiceUnavailable)
	res, raw = f.tool(t, ToolCheckFreshness, map[string]any{"question": "How much is the Pro plan?", "answer": "$49 per month"}, nil)
	if out := decodeCheck(t, raw); res.IsError || out.Outcome != "unable_to_verify" || out.Action != "refresh" || !strings.Contains(out.Facts[0].CurrentError, "503") {
		t.Fatalf("source down: %+v", out)
	}

	// Errors: missing question, unknown category, wrong type.
	for name, args := range map[string]map[string]any{
		"missing question": {"answer": "x"},
		"unknown category": {"question": "How much?", "category": "weather"},
		"wrong type":       {"question": 12},
	} {
		res, _ := f.tool(t, ToolCheckFreshness, args, nil)
		if !res.IsError {
			t.Fatalf("%s: want isError, got %q", name, res.Content[0].Text)
		}
	}
}

// ---------------------------------------------------------------------------
// explain_request
// ---------------------------------------------------------------------------

func TestExplainRequest(t *testing.T) {
	f := newFixture(t, nil)
	res, raw := f.tool(t, ToolExplainRequest, map[string]any{"request_id": "req-abc-1"}, nil)
	if res.IsError || res.Content[0].Text != "This answer came from the cache." || !strings.Contains(string(raw), `"how":"direct_hit"`) {
		t.Fatalf("explain = %+v %s", res, raw)
	}
	res, _ = f.tool(t, ToolExplainRequest, map[string]any{"request_id": "req-nope-9"}, nil)
	if !res.IsError || !strings.Contains(res.Content[0].Text, "not found") || !strings.Contains(res.Content[0].Text, "2000") {
		t.Fatalf("missing id = %+v", res)
	}
	res, _ = f.tool(t, ToolExplainRequest, map[string]any{}, nil)
	if !res.IsError || !strings.Contains(res.Content[0].Text, "request_id is required") {
		t.Fatalf("no id = %+v", res)
	}
	off := newFixture(t, func(d *Deps, _ *Options) { d.Explain = nil })
	res, _ = off.tool(t, ToolExplainRequest, map[string]any{"request_id": "req-abc-1"}, nil)
	if !res.IsError || !strings.Contains(res.Content[0].Text, "switched off") {
		t.Fatalf("explain off = %+v", res)
	}
}

// ---------------------------------------------------------------------------
// list_watched_sources
// ---------------------------------------------------------------------------

func TestListWatchedSources(t *testing.T) {
	f := newFixture(t, nil)
	f.store.notices = []changes.Notice{{ID: "chg-2", Kind: "category", Target: "policy", Time: time.Now().UTC(), Categories: []string{"policy"}}}
	res, raw := f.tool(t, ToolListWatchedSources, nil, nil)
	var out sourcesResult
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if res.IsError || len(out.Facts) != 1 || out.Facts[0].Name != "Acme Pro plan price" || out.Facts[0].SourceURL != f.page.srv.URL || out.Facts[0].MaxAge != "10m0s" || out.Facts[0].Category != "price" {
		t.Fatalf("facts = %+v", out.Facts)
	}
	if len(out.Categories) != 2 || out.Categories[1].Name != "policy" || out.Categories[1].MaxAge != "24h0m0s" {
		t.Fatalf("categories = %+v", out.Categories)
	}
	if len(out.ChangeNotices) != 1 || out.ChangeNotices[0].ID != "chg-2" || out.EvidenceProvider != evidence.DirectFetchName {
		t.Fatalf("notices = %+v provider %q", out.ChangeNotices, out.EvidenceProvider)
	}
	text := res.Content[0].Text
	if !strings.Contains(text, "1 watched fact") || !strings.Contains(text, f.page.srv.URL) || !strings.Contains(text, "chg-2") {
		t.Fatalf("text = %q", text)
	}
	// Without a notice store the list is empty, not null.
	bare := newFixture(t, func(d *Deps, _ *Options) { d.Changes = nil })
	_, raw = bare.tool(t, ToolListWatchedSources, nil, nil)
	if !strings.Contains(string(raw), `"change_notices":[]`) {
		t.Fatalf("raw = %s", raw)
	}
}

// ---------------------------------------------------------------------------
// report_change
// ---------------------------------------------------------------------------

func TestReportChange(t *testing.T) {
	f := newFixture(t, nil)
	res, raw := f.tool(t, ToolReportChange, map[string]any{"fact": "Acme Pro plan price", "note": "price raised", "clear_cache_keys": []string{"demo"}}, nil)
	if res.IsError {
		t.Fatalf("report = %q", res.Content[0].Text)
	}
	var n changes.Notice
	_ = json.Unmarshal(raw, &n)
	if n.ID != "chg-1" || n.Kind != "fact" || n.Target != "Acme Pro plan price" || n.Note != "price raised" {
		t.Fatalf("notice = %+v", n)
	}
	if len(f.rec.calls) != 1 || f.rec.calls[0].Fact != "Acme Pro plan price" || f.rec.calls[0].Note != "price raised" || len(f.rec.calls[0].ClearCacheKeys) != 1 || f.rec.calls[0].ClearCacheKeys[0] != "demo" {
		t.Fatalf("report received %+v", f.rec.calls)
	}
	if !strings.Contains(res.Content[0].Text, "chg-1") || !strings.Contains(res.Content[0].Text, "dropped") {
		t.Fatalf("text = %q", res.Content[0].Text)
	}

	// The error the companion gives is passed through as a tool error.
	f.rec.err = errors.New("no watched fact with that name")
	res, _ = f.tool(t, ToolReportChange, map[string]any{"fact": "nope"}, nil)
	if !res.IsError || res.Content[0].Text != "no watched fact with that name" {
		t.Fatalf("refused = %+v", res)
	}
	f.rec.err = nil

	res, _ = f.tool(t, ToolReportChange, map[string]any{"clear_cache_keys": "demo"}, nil)
	if !res.IsError || !strings.Contains(res.Content[0].Text, "array of strings") {
		t.Fatalf("bad type = %+v", res)
	}

	none := newFixture(t, func(d *Deps, _ *Options) { d.Report = nil })
	res, _ = none.tool(t, ToolReportChange, map[string]any{"fact": "x"}, nil)
	if !res.IsError || !strings.Contains(res.Content[0].Text, "not available") {
		t.Fatalf("no reporter = %+v", res)
	}
}

func TestReportChangeNeedsTheNoticeToken(t *testing.T) {
	f := newFixture(t, func(d *Deps, _ *Options) { d.NoticeToken = "s3cret" })
	args := map[string]any{"category": "policy"}

	res, _ := f.tool(t, ToolReportChange, args, nil)
	if !res.IsError || !strings.Contains(res.Content[0].Text, "unauthorized") || len(f.rec.calls) != 0 {
		t.Fatalf("no token = %+v calls %d", res, len(f.rec.calls))
	}
	res, _ = f.tool(t, ToolReportChange, args, map[string]string{"Authorization": "Bearer wrong"})
	if !res.IsError || len(f.rec.calls) != 0 {
		t.Fatalf("wrong token = %+v", res)
	}
	res, _ = f.tool(t, ToolReportChange, args, map[string]string{"Authorization": "Bearer s3cret"})
	if res.IsError || len(f.rec.calls) != 1 {
		t.Fatalf("bearer = %+v", res)
	}
	res, _ = f.tool(t, ToolReportChange, args, map[string]string{NoticeTokenHeader: "s3cret"})
	if res.IsError || len(f.rec.calls) != 2 {
		t.Fatalf("header = %+v", res)
	}
	// The other tools stay open: the token guards writes only.
	if res, _ := f.tool(t, ToolListWatchedSources, nil, nil); res.IsError {
		t.Fatal("list must not need the token")
	}
}
