package proxy

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"freshness-companion/internal/agetrack"
	"freshness-companion/internal/decision"
	"freshness-companion/internal/decisionlog"
	"freshness-companion/internal/evidence"
	"freshness-companion/internal/openai"
	"freshness-companion/internal/policy"
)

// ---------------------------------------------------------------------------
// Fake Bifrost: a direct (exact prompt) cache in front of a fake model, with
// the documented x-bifrost-cache-* headers and DELETE /api/cache/clear/{id}.
// ---------------------------------------------------------------------------

type fakeEntry struct {
	id   string
	body []byte
}

type recordedRequest struct {
	method, path, query string
	header              http.Header
}

type fakeBifrost struct {
	mu          sync.Mutex
	entries     map[string]fakeEntry // cache key + NUL + last user message
	price       string               // what the fake model currently answers
	llmCalls    int
	chatCalls   int
	clearCalls  []string
	clearAuth   []string
	clearStatus int // non-zero: the clear endpoint fails with this status and keeps the entry
	lastChat    []byte
	lastOther   recordedRequest
	srv         *httptest.Server

	answerFor map[string]string // last user message -> exact answer, ahead of the price answer
	semantic  map[string]string // prompt -> an already cached prompt it matches semantically
	judgeSays string            // reply to an audit judge call
	keys      []string          // x-bf-cache-key of every chat call, in order
	clearKeys []string
}

func newFakeBifrost(t *testing.T, price string) *fakeBifrost {
	t.Helper()
	f := &fakeBifrost{entries: map[string]fakeEntry{}, price: price}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeBifrost) setPrice(p string) {
	f.mu.Lock()
	f.price = p
	f.mu.Unlock()
}

func (f *fakeBifrost) setClearStatus(code int) {
	f.mu.Lock()
	f.clearStatus = code
	f.mu.Unlock()
}

func (f *fakeBifrost) seed(cacheKey, prompt, id, answer string) {
	f.mu.Lock()
	f.entries[cacheKey+"\x00"+prompt] = fakeEntry{id: id, body: chatCompletion("chatcmpl-seeded", answer)}
	f.mu.Unlock()
}

func (f *fakeBifrost) snapshot() (llm int, clears, auth []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.llmCalls, append([]string(nil), f.clearCalls...), append([]string(nil), f.clearAuth...)
}

func (f *fakeBifrost) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
		f.serveChat(w, r)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/cache/clear-by-key/"):
		key := strings.TrimPrefix(r.URL.Path, "/api/cache/clear-by-key/")
		f.mu.Lock()
		f.clearKeys = append(f.clearKeys, key)
		for k := range f.entries {
			if strings.HasPrefix(k, key+"\x00") {
				delete(f.entries, k)
			}
		}
		f.mu.Unlock()
		writeBody(w, http.StatusOK, []byte(`{"message":"Cache cleared successfully"}`))
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/cache/clear/"):
		f.serveClear(w, r)
	default:
		f.mu.Lock()
		f.lastOther = recordedRequest{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, header: r.Header.Clone()}
		f.mu.Unlock()
		writeBody(w, http.StatusOK, []byte(`{"object":"list","data":[{"id":"openai/gpt-4o-mini"}]}`))
	}
}

func (f *fakeBifrost) serveChat(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	chat, err := openai.ParseChatRequest(body)
	if err != nil {
		writeBody(w, http.StatusBadRequest, []byte(`{"error":{"message":"bad json"}}`))
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chatCalls++
	f.lastChat = body
	if chat.Stream { // a stream replayed from cache
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set(HeaderCacheHit, "true")
		w.Header().Set(HeaderCacheID, "stream-entry")
		_, _ = io.WriteString(w, sseBody)
		return
	}
	cacheKey := r.Header.Get("x-bf-cache-key")
	f.keys = append(f.keys, cacheKey)
	if cacheKey == "" { // no cache key: Bifrost does no cache lookup, so no cache headers
		f.llmCalls++
		writeBody(w, http.StatusOK, chatCompletion(fmt.Sprintf("chatcmpl-%d", f.llmCalls), f.answer(chat.LastUserText, body)))
		return
	}
	k := cacheKey + "\x00" + chat.LastUserText
	if e, ok := f.entries[k]; ok {
		w.Header().Set(HeaderCacheHit, "true")
		w.Header().Set(HeaderCacheID, e.id)
		w.Header().Set(HeaderCacheHitType, "direct")
		writeBody(w, http.StatusOK, e.body)
		return
	}
	if alias, ok := f.semantic[chat.LastUserText]; ok && !strings.EqualFold(r.Header.Get(HeaderCacheType), "direct") {
		if e, ok := f.entries[cacheKey+"\x00"+alias]; ok {
			w.Header().Set(HeaderCacheHit, "true")
			w.Header().Set(HeaderCacheID, e.id)
			w.Header().Set(HeaderCacheHitType, "semantic")
			w.Header().Set(HeaderCacheSimilarity, "0.84")
			w.Header().Set(HeaderCacheThreshold, "0.8")
			writeBody(w, http.StatusOK, e.body)
			return
		}
	}
	f.llmCalls++
	sum := sha1.Sum([]byte(k))
	id := fmt.Sprintf("entry-%x", sum[:4]) // deterministic per request, like Bifrost's direct ids
	b := chatCompletion(fmt.Sprintf("chatcmpl-%d", f.llmCalls), f.answer(chat.LastUserText, body))
	if !strings.EqualFold(r.Header.Get("x-bf-cache-no-store"), "true") {
		f.entries[k] = fakeEntry{id: id, body: b}
	}
	w.Header().Set(HeaderCacheHit, "false")
	w.Header().Set(HeaderCacheID, id)
	writeBody(w, http.StatusOK, b)
}

var correctedPrice = regexp.MustCompile(`\$([0-9]+) per month`)

func (f *fakeBifrost) answer(prompt string, body []byte) string {
	if strings.Contains(string(body), judgeInstruction[:30]) {
		return f.judgeSays
	}
	if a, ok := f.answerFor[prompt]; ok {
		return a
	}
	if strings.Contains(strings.ToLower(prompt), "pro plan") {
		// Given a correction, the model uses the verified price in it (the
		// correction is the first message, so its price is the first one).
		if bytes.Contains(body, []byte("Verified current information")) {
			if m := correctedPrice.FindSubmatch(body); m != nil {
				return fmt.Sprintf("The Acme Pro plan costs $%s per month.", m[1])
			}
		}
		return fmt.Sprintf("The Acme Pro plan costs $%s per month.", f.price)
	}
	return "Why did the cat sit on the computer? To keep an eye on the mouse."
}

func (f *fakeBifrost) serveClear(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/cache/clear/")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clearCalls = append(f.clearCalls, id)
	f.clearAuth = append(f.clearAuth, r.Header.Get("Authorization"))
	if f.clearStatus != 0 {
		writeBody(w, f.clearStatus, []byte(`{"error":{"message":"rejected"}}`))
		return
	}
	for k, e := range f.entries {
		if e.id == id {
			delete(f.entries, k)
		}
	}
	writeBody(w, http.StatusOK, []byte(`{"message":"Cache cleared successfully"}`))
}

const sseBody = "data: {\"choices\":[{\"delta\":{\"content\":\"The Acme Pro plan costs $49 per month.\"}}]}\n\ndata: [DONE]\n\n"

func chatCompletion(id, content string) []byte {
	b, _ := json.Marshal(map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"model":   "openai/gpt-4o-mini",
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}},
	})
	return b
}

func writeBody(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// ---------------------------------------------------------------------------
// Fake source page whose price can change between requests.
// ---------------------------------------------------------------------------

type fakePage struct {
	mu     sync.Mutex
	price  string
	status int
	body   string
	hits   int
	srv    *httptest.Server
}

func newFakePage(t *testing.T, price string) *fakePage {
	t.Helper()
	p := &fakePage{price: price}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.hits++
		if p.status != 0 {
			http.Error(w, "unavailable", p.status)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if p.body != "" {
			_, _ = io.WriteString(w, p.body)
			return
		}
		fmt.Fprintf(w, `<!doctype html><html lang="en"><head><title>Acme pricing</title></head><body><main>
<h1>Acme pricing</h1><p id="pro-plan-price">Acme Pro plan — $%s per month</p></main></body></html>`, p.price)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakePage) set(price string, status int, body string) {
	p.mu.Lock()
	p.price, p.status, p.body = price, status, body
	p.mu.Unlock()
}

func (p *fakePage) hitCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hits
}

// ---------------------------------------------------------------------------
// Companion under test: real proxy, DirectFetch evidence against the fake page.
// ---------------------------------------------------------------------------

type countingProvider struct {
	evidence.Provider
	calls atomic.Int32
}

func (c *countingProvider) Fetch(ctx context.Context, sourceURL string) (evidence.Evidence, error) {
	c.calls.Add(1)
	return c.Provider.Fetch(ctx, sourceURL)
}

type companion struct {
	url      string
	ages     *agetrack.Tracker
	log      *decisionlog.Log
	logPath  string
	provider *countingProvider
}

func testPolicyDoc(sourceURL, onUnverifiable string) string {
	return fmt.Sprintf(`{
	  "version": 1,
	  "categories": [
	    {"name": "price", "keywords": ["price", "cost", "how much"], "max_age": "10m", "on_unverifiable": %q}
	  ],
	  "facts": [{
	    "name": "Acme Pro plan price", "category": "price", "keywords": ["pro"],
	    "source_url": %q,
	    "source_pattern": "Acme Pro plan[^$0-9]{0,20}\\$\\s*([0-9][0-9,]*(?:\\.[0-9]+)?)\\s*per month",
	    "answer_pattern": "(?i)\\$\\s*([0-9][0-9,]*(?:\\.[0-9]+)?)\\s*(?:/\\s*mo(?:nth)?|per\\s+month|a\\s+month|monthly)\\b",
	    "compare": "numeric"
	  }]
	}`, onUnverifiable, sourceURL)
}

func newCompanion(t *testing.T, bifrostURL, policyDoc string) *companion {
	t.Helper()
	return newCompanionWith(t, bifrostURL, policyDoc, 0, nil)
}

// newCompanionWith builds a companion whose evidence is kept for evidenceTTL
// (0 = every check fetches) and lets the test adjust the proxy config.
func newCompanionWith(t *testing.T, bifrostURL, policyDoc string, evidenceTTL time.Duration, tweak func(*Config)) *companion {
	t.Helper()
	pol, err := policy.Parse([]byte(policyDoc))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	prov := &countingProvider{Provider: evidence.NewDirectFetch(evidence.DirectConfig{Timeout: 2 * time.Second})}
	// TTL 0: every check fetches, so a price change is seen at once. The
	// evidence cache itself is tested in the evidence package.
	cache := evidence.NewCache(prov, evidence.CacheConfig{TTL: evidenceTTL, FetchTimeout: 5 * time.Second})
	ages, err := agetrack.Open(agetrack.Options{Path: filepath.Join(dir, "ages.json")})
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "decisions.jsonl")
	dlog, err := decisionlog.Open(logPath, 100)
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
		Upstream:         up,
		Policy:           pol,
		Engine:           &decision.Engine{Evidence: cache},
		Ages:             ages,
		Decisions:        dlog,
		Refresh:          &ClearAndResend{Clearer: clearer, Sender: up, MaxAttempts: 2},
		UpstreamTimeout:  10 * time.Second,
		EvidenceProvider: prov.Name(),
		BifrostURL:       bifrostURL,
		Clearer:          clearer,
		SettleDelay:      5 * time.Millisecond,
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
	return &companion{url: ts.URL, ages: ages, log: dlog, logPath: logPath, provider: prov}
}

const proPrompt = "How much does the Acme Pro plan cost per month?"

func postChat(t *testing.T, baseURL, prompt string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"model": "openai/gpt-4o-mini",
		"messages": []map[string]string{
			{"role": "system", "content": "Answer in one sentence."},
			{"role": "user", "content": prompt},
		},
	})
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-bf-cache-key", "demo")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

func assertHeader(t *testing.T, resp *http.Response, name, want string) {
	t.Helper()
	if got := resp.Header.Get(name); got != want {
		t.Fatalf("%s = %q, want %q (reason: %s)", name, got, want, resp.Header.Get(HeaderReason))
	}
}

func freshnessHeaders(resp *http.Response) []string {
	var out []string
	for name := range resp.Header {
		// The request id is on every chat response; it says nothing about freshness.
		if http.CanonicalHeaderKey(name) == HeaderRequestID {
			continue
		}
		if strings.HasPrefix(strings.ToLower(name), "x-freshness-") {
			out = append(out, name)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// End-to-end behaviour.
// ---------------------------------------------------------------------------

func TestFreshHitIsServedFromCache(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c := newCompanion(t, fb.srv.URL, testPolicyDoc(page.srv.URL, "bypass"))

	miss, missBody := postChat(t, c.url, proPrompt, nil)
	assertHeader(t, miss, HeaderCacheHit, "false")
	if h := freshnessHeaders(miss); len(h) != 0 {
		t.Fatalf("a miss is passed through without freshness headers, got %v", h)
	}
	if _, ok := c.ages.Age(miss.Header.Get(HeaderCacheID)); !ok {
		t.Fatal("the miss should have recorded the entry's write time")
	}

	hit, hitBody := postChat(t, c.url, proPrompt, nil)
	assertHeader(t, hit, HeaderCacheHit, "true")
	assertHeader(t, hit, HeaderStatus, "confirmed")
	assertHeader(t, hit, HeaderAction, ActionServed)
	assertHeader(t, hit, HeaderCachedValue, "49")
	assertHeader(t, hit, HeaderCurrentValue, "49")
	assertHeader(t, hit, HeaderSource, page.srv.URL)
	assertHeader(t, hit, HeaderProvider, "direct_fetch (not Tavily)")
	assertHeader(t, hit, HeaderCategory, "price")
	if _, err := time.Parse(time.RFC3339, hit.Header.Get(HeaderCheckedAt)); err != nil {
		t.Fatalf("checked-at: %v", err)
	}
	if hit.Header.Get(HeaderAgeSeconds) == "" {
		t.Fatal("the tracked age should be reported")
	}
	if !bytes.Equal(hitBody, missBody) {
		t.Fatalf("the cached JSON body must be served unchanged:\n%s\n%s", hitBody, missBody)
	}
	llm, clears, _ := fb.snapshot()
	if llm != 1 || len(clears) != 0 || page.hitCount() != 1 {
		t.Fatalf("llm calls = %d, clears = %v, page hits = %d; want 1, none, 1", llm, clears, page.hitCount())
	}
	recs := c.log.Recent(5)
	if len(recs) != 1 || recs[0].Outcome != "confirmed" || recs[0].Action != ActionServed || recs[0].CacheID == "" {
		t.Fatalf("decision log = %+v", recs)
	}
}

func TestStaleHitIsClearedAndRefreshed(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c := newCompanion(t, fb.srv.URL, testPolicyDoc(page.srv.URL, "bypass"))

	miss, _ := postChat(t, c.url, proPrompt, nil) // the cache now holds "$49"
	staleID := miss.Header.Get(HeaderCacheID)

	page.set("59", 0, "") // the real price changes ...
	fb.setPrice("59")     // ... and the model (given fresh context) would now say $59

	resp, body := postChat(t, c.url, proPrompt, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	assertHeader(t, resp, HeaderStatus, "stale")
	assertHeader(t, resp, HeaderAction, ActionRefreshed)
	assertHeader(t, resp, HeaderCachedValue, "49")
	assertHeader(t, resp, HeaderCurrentValue, "59")
	assertHeader(t, resp, HeaderRefreshedValue, "59")
	assertHeader(t, resp, HeaderRefreshedCheck, decision.RefreshedMatches)
	assertHeader(t, resp, HeaderCacheHit, "false") // Bifrost's own header from the fresh response
	if !strings.Contains(string(body), "$59 per month") || strings.Contains(string(body), "$49") {
		t.Fatalf("the stale answer must not be served; body = %s", body)
	}
	llm, clears, auth := fb.snapshot()
	if len(clears) != 1 || clears[0] != staleID {
		t.Fatalf("clear calls = %v, want exactly [%s]", clears, staleID)
	}
	if auth[0] != "Bearer admin-test-token" {
		t.Fatalf("clear call auth = %q", auth[0])
	}
	if llm != 2 {
		t.Fatalf("llm calls = %d, want 2 (original miss + re-request)", llm)
	}

	// The refreshed entry is now served and confirmed.
	again, _ := postChat(t, c.url, proPrompt, nil)
	assertHeader(t, again, HeaderStatus, "confirmed")
	assertHeader(t, again, HeaderCachedValue, "59")
	recs := c.log.Recent(2)
	if len(recs) != 2 || recs[0].Outcome != "confirmed" || recs[1].Outcome != "stale" || recs[1].Action != ActionRefreshed ||
		recs[1].RefreshStrategy != "clear_and_resend" || recs[1].RefreshedCheck != decision.RefreshedMatches {
		t.Fatalf("decision log = %+v", recs)
	}
}

func TestUnverifiableFollowsPolicy(t *testing.T) {
	t.Run("serve_with_warning when the source is down", func(t *testing.T) {
		page := newFakePage(t, "49")
		fb := newFakeBifrost(t, "49")
		c := newCompanion(t, fb.srv.URL, testPolicyDoc(page.srv.URL, "serve_with_warning"))
		_, cachedBody := postChat(t, c.url, proPrompt, nil)
		page.set("49", http.StatusServiceUnavailable, "")

		resp, body := postChat(t, c.url, proPrompt, nil)
		assertHeader(t, resp, HeaderStatus, "unable_to_verify")
		assertHeader(t, resp, HeaderAction, ActionServedWithWarning)
		if resp.Header.Get(HeaderWarning) == "" || !strings.Contains(resp.Header.Get(HeaderReason), "HTTP 503") {
			t.Fatalf("warning = %q, reason = %q", resp.Header.Get(HeaderWarning), resp.Header.Get(HeaderReason))
		}
		if !bytes.Equal(body, cachedBody) {
			t.Fatal("serve_with_warning serves the cached body")
		}
		if llm, clears, _ := fb.snapshot(); llm != 1 || len(clears) != 0 {
			t.Fatalf("llm = %d, clears = %v; want no refresh", llm, clears)
		}
	})

	t.Run("bypass when the source no longer states the value", func(t *testing.T) {
		page := newFakePage(t, "49")
		fb := newFakeBifrost(t, "49")
		c := newCompanion(t, fb.srv.URL, testPolicyDoc(page.srv.URL, "bypass"))
		miss, _ := postChat(t, c.url, proPrompt, nil)
		page.set("", 0, "<html><body><p>Pricing is being updated. Contact sales.</p></body></html>")

		resp, body := postChat(t, c.url, proPrompt, nil)
		assertHeader(t, resp, HeaderStatus, "unable_to_verify")
		assertHeader(t, resp, HeaderAction, ActionRefreshed)
		if !strings.Contains(resp.Header.Get(HeaderReason), "source_pattern matched nothing") {
			t.Fatalf("reason = %q", resp.Header.Get(HeaderReason))
		}
		llm, clears, _ := fb.snapshot()
		if llm != 2 || len(clears) != 1 || clears[0] != miss.Header.Get(HeaderCacheID) {
			t.Fatalf("llm = %d, clears = %v; want a clear + re-request", llm, clears)
		}
		if resp.Header.Get(HeaderRefreshedCheck) != "" {
			t.Fatal("there is no current value to compare the refreshed answer with")
		}
		if !strings.Contains(string(body), "chatcmpl-2") {
			t.Fatalf("want the re-requested answer, got %s", body)
		}
	})
}

func TestNonTimeSensitiveHitsNeverFetchEvidence(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c := newCompanion(t, fb.srv.URL, testPolicyDoc(page.srv.URL, "bypass"))
	const joke = "Tell me a joke about cats"

	_, first := postChat(t, c.url, joke, nil)
	for i := 0; i < 3; i++ {
		resp, body := postChat(t, c.url, joke, nil)
		assertHeader(t, resp, HeaderCacheHit, "true")
		if h := freshnessHeaders(resp); len(h) != 0 {
			t.Fatalf("non-time-sensitive hit got freshness headers %v", h)
		}
		if !bytes.Equal(body, first) {
			t.Fatal("non-time-sensitive hit must pass through unchanged")
		}
	}
	if page.hitCount() != 0 || c.provider.calls.Load() != 0 {
		t.Fatalf("evidence fetched for a non-time-sensitive prompt: page hits %d, provider calls %d", page.hitCount(), c.provider.calls.Load())
	}
	if n := len(c.log.Recent(10)); n != 0 {
		t.Fatalf("decision log has %d records, want none", n)
	}
	if _, clears, _ := fb.snapshot(); len(clears) != 0 {
		t.Fatalf("clears = %v", clears)
	}
}

func TestStaleAnswerRefusedWhenRefreshImpossible(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c := newCompanion(t, fb.srv.URL, testPolicyDoc(page.srv.URL, "bypass"))
	miss, _ := postChat(t, c.url, proPrompt, nil)
	page.set("59", 0, "")
	fb.setClearStatus(http.StatusUnauthorized) // the clear is rejected, so the stale entry stays

	resp, body := postChat(t, c.url, proPrompt, nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	assertHeader(t, resp, HeaderStatus, "stale")
	assertHeader(t, resp, HeaderAction, ActionRefreshFailed)
	errText := resp.Header.Get(HeaderError)
	if !strings.Contains(errText, "served from cache again") || !strings.Contains(errText, "HTTP 401") {
		t.Fatalf("x-freshness-error = %q", errText)
	}
	if strings.Contains(string(body), "$49") || !strings.Contains(string(body), "freshness_refresh_failed") {
		t.Fatalf("the stale answer must never be served; body = %s", body)
	}
	if _, clears, _ := fb.snapshot(); len(clears) != 1 || clears[0] != miss.Header.Get(HeaderCacheID) {
		t.Fatalf("clears = %v", clears)
	}
	if recs := c.log.Recent(1); len(recs) != 1 || recs[0].Action != ActionRefreshFailed || recs[0].Error == "" {
		t.Fatalf("decision log = %+v", recs)
	}
}

func TestUnknownAgeIsRevalidated(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	// An entry written before the companion was running: its age is unknown.
	fb.seed("demo", proPrompt, "entry-preexisting", "The Acme Pro plan costs $49 per month.")
	c := newCompanion(t, fb.srv.URL, testPolicyDoc(page.srv.URL, "bypass"))

	resp, _ := postChat(t, c.url, proPrompt, nil)
	assertHeader(t, resp, HeaderCacheHit, "true")
	assertHeader(t, resp, HeaderStatus, "confirmed")
	if !strings.Contains(resp.Header.Get(HeaderReason), "revalidated") || resp.Header.Get(HeaderAgeSeconds) != "" {
		t.Fatalf("reason = %q, age = %q", resp.Header.Get(HeaderReason), resp.Header.Get(HeaderAgeSeconds))
	}
	if page.hitCount() != 1 {
		t.Fatalf("page hits = %d, want one revalidation", page.hitCount())
	}
}

func TestStreamsPassThroughUnchecked(t *testing.T) {
	page := newFakePage(t, "59")
	fb := newFakeBifrost(t, "49")
	c := newCompanion(t, fb.srv.URL, testPolicyDoc(page.srv.URL, "bypass"))
	reqBody := `{"model":"openai/gpt-4o-mini","stream":true,"messages":[{"role":"user","content":"` + proPrompt + `"}]}`
	req, _ := http.NewRequest(http.MethodPost, c.url+"/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-bf-cache-key", "demo")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != sseBody || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream changed: %q", body)
	}
	if h := freshnessHeaders(resp); len(h) != 0 || page.hitCount() != 0 {
		t.Fatalf("streams are not checked (documented limit): headers %v, page hits %d", h, page.hitCount())
	}
}

func TestPassthroughPreservesTheRequest(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c := newCompanion(t, fb.srv.URL, testPolicyDoc(page.srv.URL, "bypass"))

	req, _ := http.NewRequest(http.MethodGet, c.url+"/v1/models?limit=2&provider=openai", nil)
	req.Header.Set("Authorization", "Bearer sk-bf-virtual-key")
	req.Header.Set("X-Custom-Header", "abc")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	fb.mu.Lock()
	got := fb.lastOther
	fb.mu.Unlock()
	if got.method != http.MethodGet || got.path != "/v1/models" || got.query != "limit=2&provider=openai" {
		t.Fatalf("forwarded %s %s?%s", got.method, got.path, got.query)
	}
	if got.header.Get("Authorization") != "Bearer sk-bf-virtual-key" || got.header.Get("X-Custom-Header") != "abc" {
		t.Fatalf("headers not preserved: %v", got.header)
	}

	// The chat body reaches Bifrost byte for byte, even with unusual formatting.
	odd := "{ \"model\" : \"openai/gpt-4o-mini\",\n  \"messages\": [ {\"role\":\"user\",\"content\":\"Tell me a joke\"} ] }"
	req, _ = http.NewRequest(http.MethodPost, c.url+"/v1/chat/completions?trace=1", strings.NewReader(odd))
	req.Header.Set("x-bf-cache-key", "demo")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	fb.mu.Lock()
	sent := string(fb.lastChat)
	fb.mu.Unlock()
	if sent != odd {
		t.Fatalf("body changed:\n%q\n%q", sent, odd)
	}
}

func TestNoStoreMissIsNotTracked(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c := newCompanion(t, fb.srv.URL, testPolicyDoc(page.srv.URL, "bypass"))
	resp, _ := postChat(t, c.url, proPrompt, map[string]string{"x-bf-cache-no-store": "true"})
	assertHeader(t, resp, HeaderCacheHit, "false")
	if _, ok := c.ages.Age(resp.Header.Get(HeaderCacheID)); ok || c.ages.Len() != 0 {
		t.Fatal("Bifrost writes nothing under no-store, so the id must not get an age")
	}
}

func TestHealthAndDecisionEndpointsStoreNoPrompts(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c := newCompanion(t, fb.srv.URL, testPolicyDoc(page.srv.URL, "bypass"))
	postChat(t, c.url, proPrompt, nil)
	postChat(t, c.url, proPrompt, nil)

	resp, err := http.Get(c.url + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	var health map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&health)
	resp.Body.Close()
	if health["status"] != "ok" || health["evidence_provider"] != "direct_fetch (not Tavily)" || health["refresh_strategy"] != "clear_and_resend" {
		t.Fatalf("healthz = %v", health)
	}

	resp, err = http.Get(c.url + "/freshness/decisions?limit=1")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var out struct {
		Count     int                  `json:"count"`
		Decisions []decisionlog.Record `json:"decisions"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Count != 1 || out.Decisions[0].Outcome != "confirmed" {
		t.Fatalf("decisions = %s (%v)", raw, err)
	}
	file, err := os.ReadFile(c.logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{proPrompt, "Answer in one sentence", "The Acme Pro plan costs"} {
		if strings.Contains(string(raw), private) || strings.Contains(string(file), private) {
			t.Fatalf("decision log contains prompt or answer text %q", private)
		}
	}

	resp, err = http.Get(c.url + "/freshness/decisions?limit=abc")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad limit: status %d", resp.StatusCode)
	}
}

func TestBifrostDownIsAGatewayError(t *testing.T) {
	page := newFakePage(t, "49")
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close() // nothing listens here any more
	c := newCompanion(t, deadURL, testPolicyDoc(page.srv.URL, "bypass"))
	resp, body := postChat(t, c.url, proPrompt, nil)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "bifrost_unavailable") {
		t.Fatalf("status %d, body %s", resp.StatusCode, body)
	}
}
