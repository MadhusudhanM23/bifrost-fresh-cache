package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"freshness-companion/internal/decision"
	"freshness-companion/internal/discover"
	"freshness-companion/internal/evidence"
	"freshness-companion/internal/verified"
	"freshness-companion/internal/watch"
)

// fakeTavilyAPI serves POST /search and POST /extract like api.tavily.com,
// records every request body and counts the calls.
type fakeTavilyAPI struct {
	mu       sync.Mutex
	bodies   []string
	searches int
	extracts int
	results  string            // the "results" array of a search answer
	pages    map[string]string // URL (without _fresh) -> page text
	srv      *httptest.Server
}

const acmeSearchResults = `[
  {"title": "Acme pricing (demo)", "url": "https://acme.example/acme-pricing/", "content": "Acme pricing. Pro plan. Acme Pro plan — $49 per month", "score": 0.83},
  {"title": "Acme Pro review", "url": "https://www.reddit.com/r/acme/pro", "content": "Acme Pro is $45", "score": 0.9}
]`

func newFakeTavilyAPI(t *testing.T, price string) *fakeTavilyAPI {
	t.Helper()
	f := &fakeTavilyAPI{results: acmeSearchResults, pages: map[string]string{
		"https://acme.example/acme-pricing/": "Acme pricing\nPro plan\nAcme Pro plan — $" + price + " per month\nLast updated 2 November 2026.",
	}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.bodies = append(f.bodies, r.URL.Path+" "+string(data))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/search":
			f.searches++
			fmt.Fprintf(w, `{"query": "x", "answer": null, "results": %s, "response_time": 0.5, "request_id": "s1"}`, f.results)
		case "/extract":
			f.extracts++
			var req struct{ URLs []string }
			_ = json.Unmarshal(data, &req)
			u, _ := url.Parse(req.URLs[0])
			q := u.Query()
			q.Del("_fresh")
			u.RawQuery = q.Encode()
			page, ok := f.pages[u.String()]
			if !ok {
				fmt.Fprintf(w, `{"results": [], "failed_results": [{"url": %q, "error": "not found"}]}`, req.URLs[0])
				return
			}
			b, _ := json.Marshal(map[string]any{"results": []map[string]string{{"url": req.URLs[0], "raw_content": page}}, "failed_results": []any{}})
			_, _ = w.Write(b)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeTavilyAPI) counts() (searches, extracts int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.searches, f.extracts
}

func (f *fakeTavilyAPI) allBodies() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.bodies...)
}

func (f *fakeTavilyAPI) setPrice(price string) {
	f.mu.Lock()
	f.pages["https://acme.example/acme-pricing/"] = "Acme pricing\nAcme Pro plan — $" + price + " per month"
	f.mu.Unlock()
}

// discoverPolicyDoc has a price category with a discover block and no
// watched fact, like docker/policy.discover.json.
func discoverPolicyDoc(guard string) string {
	if guard == "" {
		guard = "{}"
	}
	return `{
	  "version": 1,
	  "guard": ` + guard + `,
	  "categories": [
	    {"name": "price", "keywords": ["price", "cost", "how much"], "max_age": "10m", "on_unverifiable": "bypass", "on_contradicted": "correct",
	     "discover": {"enabled": true, "entities": ["Acme Pro", "Acme Team"], "query": "{entities} plan price per month",
	       "include_domains": ["acme.example"], "require_domains": true, "compare": "numeric", "near_chars": 40, "min_score": 0.5}}
	  ],
	  "facts": []
	}`
}

// discoveryCompanion wires a companion whose evidence comes from the fake
// Tavily Extract and, when on, whose discovery uses the fake Tavily Search.
func discoveryCompanion(t *testing.T, fb *fakeBifrost, tav *fakeTavilyAPI, policyDoc string, on bool, tweak func(*Config)) (*companion, *discover.Service) {
	t.Helper()
	extract, err := evidence.NewTavilyExtract(evidence.TavilyConfig{APIKey: "tvly-test", BaseURL: tav.srv.URL, FreshStamp: true})
	if err != nil {
		t.Fatal(err)
	}
	var svc *discover.Service
	if on {
		searcher, err := evidence.NewTavilySearcher(evidence.TavilyConfig{APIKey: "tvly-test", BaseURL: tav.srv.URL})
		if err != nil {
			t.Fatal(err)
		}
		svc, err = discover.New(discover.Config{Searcher: searcher, Budget: watch.NewBudget(10, nil), StorePath: filepath.Join(t.TempDir(), "discovered.json")})
		if err != nil {
			t.Fatal(err)
		}
	}
	c := newCompanionWith(t, fb.srv.URL, policyDoc, time.Minute, func(cfg *Config) {
		e := &decision.Engine{Evidence: evidence.NewCache(extract, evidence.CacheConfig{TTL: time.Minute, FetchTimeout: 5 * time.Second})}
		if svc != nil {
			e.Discover = svc
		}
		cfg.Engine = e
		cfg.EvidenceProvider = extract.Name()
		if tweak != nil {
			tweak(cfg)
		}
	})
	return c, svc
}

func TestDiscoveryOffByDefaultNoSearch(t *testing.T) {
	fb := newFakeBifrost(t, "49")
	tav := newFakeTavilyAPI(t, "49")
	c, _ := discoveryCompanion(t, fb, tav, discoverPolicyDoc(""), false, nil)
	postChat(t, c.url, proPrompt, nil)
	hit, _ := postChat(t, c.url, proPrompt, nil)
	assertHeader(t, hit, HeaderCacheHit, "true")
	assertHeader(t, hit, HeaderStatus, "unknown") // served on policy only, as before discovery existed
	if hit.Header.Get(HeaderDiscovery) != "" || strings.Contains(hit.Header.Get(HeaderReceipt), "discover") {
		t.Fatalf("discovery headers with discovery off: %v", hit.Header)
	}
	if s, e := tav.counts(); s != 0 || e != 0 {
		t.Fatalf("Tavily was called with discovery off: %d searches, %d extracts", s, e)
	}
}

func TestDiscoveryNeverSendsPromptOrAnswer(t *testing.T) {
	fb := newFakeBifrost(t, "49")
	tav := newFakeTavilyAPI(t, "49")
	c, _ := discoveryCompanion(t, fb, tav, discoverPolicyDoc(""), true, nil)
	const (
		prompt   = "How much does the Acme Pro plan cost per month for Zorblax Industries, asking for Quentin Mxyzptlk?"
		system   = "SECRET-CONTEXT-7781: customer tier gold, account manager Wilhelmina Fnord."
		cacheKey = "cachekey-zz9-private"
	)
	ask := func() *http.Response {
		resp, _ := postJSON(t, c.url+"/v1/chat/completions", map[string]any{
			"model": "openai/gpt-4o-mini",
			"messages": []map[string]string{
				{"role": "system", "content": system},
				{"role": "user", "content": prompt},
			},
		}, map[string]string{"x-bf-cache-key": cacheKey, "Authorization": "Bearer caller-token-5566"})
		return resp
	}
	ask()
	hit := ask()
	assertHeader(t, hit, HeaderStatus, "confirmed")
	assertHeader(t, hit, HeaderDiscovery, "confirmed")
	answer := "The Acme Pro plan costs $49 per month."
	bodies := tav.allBodies()
	if len(bodies) != 2 {
		t.Fatalf("want one search and one extract, got %d calls: %v", len(bodies), bodies)
	}
	forbidden := []string{prompt, system, answer, cacheKey, "caller-token-5566", "Zorblax", "Quentin", "Mxyzptlk", "SECRET-CONTEXT", "Wilhelmina", "Fnord", "gold", "costs $49", "How much"}
	for _, b := range bodies {
		for _, f := range forbidden {
			if strings.Contains(strings.ToLower(b), strings.ToLower(f)) {
				t.Fatalf("a Tavily request carries %q: %s", f, b)
			}
		}
	}
	// The search body is exactly the documented shape with the derived query.
	var search map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(bodies[0], "/search ")), &search); err != nil {
		t.Fatalf("first call is not the search: %s", bodies[0])
	}
	if search["query"] != "Acme Pro plan price per month" || len(search) != 7 {
		t.Fatalf("search body = %v", search)
	}
	if !strings.HasPrefix(bodies[1], "/extract ") || !strings.Contains(bodies[1], "https://acme.example/acme-pricing/?_fresh=") {
		t.Fatalf("second call should extract the chosen URL: %s", bodies[1])
	}
}

func TestDiscoveredHeadersAndReceipt(t *testing.T) {
	fb := newFakeBifrost(t, "49")
	tav := newFakeTavilyAPI(t, "49")
	c, _ := discoveryCompanion(t, fb, tav, discoverPolicyDoc(""), true, nil)
	miss, _ := postChat(t, c.url, proPrompt, nil)
	if miss.Header.Get(HeaderDiscovery) != "" {
		t.Fatal("a miss is never checked by discovery")
	}
	if s, _ := tav.counts(); s != 0 {
		t.Fatal("a miss must not search")
	}
	hit, _ := postChat(t, c.url, proPrompt, nil)
	assertHeader(t, hit, HeaderStatus, "confirmed")
	assertHeader(t, hit, HeaderAction, ActionServed)
	assertHeader(t, hit, HeaderDiscovery, "confirmed")
	assertHeader(t, hit, HeaderDiscoveryQuery, "Acme Pro plan price per month")
	assertHeader(t, hit, HeaderDiscoveryRank, "1") // the reddit result (rank 2) is excluded
	assertHeader(t, hit, HeaderDiscoveryScore, "0.83")
	assertHeader(t, hit, HeaderFoundBy, "tavily_search")
	assertHeader(t, hit, HeaderSource, "https://acme.example/acme-pricing/")
	assertHeader(t, hit, HeaderCurrentValue, "49")
	receipt := hit.Header.Get(HeaderReceipt)
	want := `provider="tavily_extract"; discovered=true; found_by=tavily_search; query="Acme Pro plan price per month"; rank=1; score=0.83`
	if !strings.Contains(receipt, want) || !strings.HasPrefix(receipt, `v=1; verdict=confirmed; action=served; found="49"`) {
		t.Fatalf("receipt = %q", receipt)
	}
	// Ask again: the learned source and the evidence are both reused.
	again, _ := postChat(t, c.url, proPrompt, nil)
	assertHeader(t, again, HeaderStatus, "confirmed")
	assertHeader(t, again, HeaderEvidence, EvidenceReused)
	if s, e := tav.counts(); s != 1 || e != 1 {
		t.Fatalf("searches=%d extracts=%d, want 1 and 1", s, e)
	}
	if !strings.Contains(again.Header.Get(HeaderReason), "learned earlier, reused") {
		t.Fatalf("reason = %q", again.Header.Get(HeaderReason))
	}
}

func TestExplainSaysDiscovered(t *testing.T) {
	fb := newFakeBifrost(t, "49")
	tav := newFakeTavilyAPI(t, "49")
	c, _ := discoveryCompanion(t, fb, tav, discoverPolicyDoc(""), true, nil)
	postChat(t, c.url, proPrompt, nil)
	hit, _ := postChat(t, c.url, proPrompt, nil)
	e := explainOf(t, c.url, hit)
	if !e.Discovered || e.Discovery != "confirmed" || e.DiscoveryQuery != "Acme Pro plan price per month" || e.FoundBy != "tavily_search" || e.Source != "https://acme.example/acme-pricing/" {
		t.Fatalf("explanation = %+v", e)
	}
	for _, want := range []string{`No watched fact covered this; Tavily Search for "Acme Pro plan price per month" picked result #1 (score 0.83)`, "still says 49"} {
		if !strings.Contains(e.Plain, want) {
			t.Fatalf("plain %q lacks %q", e.Plain, want)
		}
	}
	if e.Receipt == nil || !e.Receipt.Discovered || e.Receipt.Query != "Acme Pro plan price per month" {
		t.Fatalf("receipt = %+v", e.Receipt)
	}
	// A request where search finds nothing usable says so, and falls back.
	tav.mu.Lock()
	tav.results = `[{"title": "Acme Pro", "url": "https://www.reddit.com/r/acme", "content": "Acme Pro is $45", "score": 0.9}]`
	tav.mu.Unlock()
	fb.mu.Lock()
	fb.answerFor = map[string]string{"How much does the Acme Team plan cost?": "The Acme Team plan costs $99 per month."}
	fb.mu.Unlock()
	postChat(t, c.url, "How much does the Acme Team plan cost?", nil)
	hit, _ = postChat(t, c.url, "How much does the Acme Team plan cost?", nil)
	assertHeader(t, hit, HeaderDiscovery, "no_source")
	e = explainOf(t, c.url, hit)
	if e.Discovered || !strings.Contains(e.Plain, `Tavily Search for "Acme Team plan price per month" found no usable page`) {
		t.Fatalf("plain = %q", e.Plain)
	}
	if !strings.Contains(hit.Header.Get(HeaderReceipt), "discovery=no_source") {
		t.Fatalf("receipt = %q", hit.Header.Get(HeaderReceipt))
	}
}

func TestDiscoveredEndpoint(t *testing.T) {
	fb := newFakeBifrost(t, "49")
	tav := newFakeTavilyAPI(t, "49")
	c, svc := discoveryCompanion(t, fb, tav, discoverPolicyDoc(""), true, nil)
	postChat(t, c.url, proPrompt, nil)
	postChat(t, c.url, proPrompt, nil)
	postChat(t, c.url, proPrompt, nil)
	mux := http.NewServeMux()
	svc.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	var out struct {
		Enabled bool
		Stats   discover.Stats
		Sources []discover.Learned
	}
	if code := getJSON(t, ts.URL+"/freshness/discovered", &out); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if !out.Enabled || len(out.Sources) != 1 {
		t.Fatalf("out = %+v", out)
	}
	src := out.Sources[0]
	if src.Category != "price" || src.Query != "Acme Pro plan price per month" || src.URL != "https://acme.example/acme-pricing/" ||
		src.FoundBy != "tavily_search" || src.Rank != 1 || src.Uses != 2 || src.LastOutcome != "confirmed" || src.LastValue != "49" {
		t.Fatalf("source = %+v", src)
	}
	if out.Stats.Searches != 1 || out.Stats.Reused != 1 || out.Stats.RejectedByReason["outside_include_domains"] != 1 {
		t.Fatalf("stats = %+v", out.Stats)
	}
}

func TestGuardedRequestNeverDiscovers(t *testing.T) {
	fb := newFakeBifrost(t, "49")
	tav := newFakeTavilyAPI(t, "49")
	c, _ := discoveryCompanion(t, fb, tav, discoverPolicyDoc(`{"sensitive": "bypass"}`), true, nil)
	prompt := proPrompt + " Send it to jane.doe@example.com"
	for i := 0; i < 2; i++ {
		resp, _ := postChat(t, c.url, prompt, nil)
		if resp.Header.Get(HeaderGuard) == "" || resp.Header.Get(HeaderDiscovery) != "" {
			t.Fatalf("guarded request: %v", resp.Header)
		}
	}
	if s, e := tav.counts(); s != 0 || e != 0 {
		t.Fatalf("a guarded request reached Tavily: %d searches, %d extracts", s, e)
	}
}

func TestVerifiedAnswerNeverDiscovers(t *testing.T) {
	fb := newFakeBifrost(t, "49")
	tav := newFakeTavilyAPI(t, "49")
	path := filepath.Join(t.TempDir(), "verified.json")
	if err := os.WriteFile(path, []byte(`[{"question": "`+proPrompt+`", "answer": "The Acme Pro plan costs $49 per month.", "label": "acme-pro"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := verified.Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := discoveryCompanion(t, fb, tav, discoverPolicyDoc(""), true, func(cfg *Config) { cfg.Verified = st })
	for i := 0; i < 2; i++ {
		resp, _ := postChat(t, c.url, proPrompt, nil)
		assertHeader(t, resp, HeaderAnswerSource, "verified")
	}
	if s, e := tav.counts(); s != 0 || e != 0 {
		t.Fatalf("a verified answer reached Tavily: %d searches, %d extracts", s, e)
	}
}

func TestDiscoveredStaleNeverCorrects(t *testing.T) {
	fb := newFakeBifrost(t, "59") // the model (and so the cached answer) says $59
	tav := newFakeTavilyAPI(t, "49")
	c, _ := discoveryCompanion(t, fb, tav, discoverPolicyDoc(""), true, nil)
	postChat(t, c.url, proPrompt, nil)
	hit, _ := postChat(t, c.url, proPrompt, nil)
	assertHeader(t, hit, HeaderStatus, "stale")
	assertHeader(t, hit, HeaderDiscovery, "stale")
	assertHeader(t, hit, HeaderCurrentValue, "49")
	// The refreshed answer still says $59. The category says on_contradicted
	// correct, but a discovered page's text never goes into a prompt: the
	// answer is uncached instead.
	assertHeader(t, hit, HeaderRefreshedCheck, decision.RefreshedDiffers)
	assertHeader(t, hit, HeaderAction, ActionUncached)
	assertHeader(t, hit, HeaderCorrection, "uncached")
	fb.mu.Lock()
	last := string(fb.lastChat)
	fb.mu.Unlock()
	if strings.Contains(last, "Verified current information") || strings.Contains(last, "acme.example") {
		t.Fatalf("page text reached the model: %s", last)
	}
	if !strings.Contains(hit.Header.Get(HeaderReceipt), "discovered=true") {
		t.Fatalf("receipt = %q", hit.Header.Get(HeaderReceipt))
	}
}

func TestDecisionLogNoPromptWithDiscovery(t *testing.T) {
	fb := newFakeBifrost(t, "49")
	tav := newFakeTavilyAPI(t, "49")
	c, _ := discoveryCompanion(t, fb, tav, discoverPolicyDoc(""), true, nil)
	postChat(t, c.url, proPrompt, nil)
	postChat(t, c.url, proPrompt, nil)
	f, err := os.Open(c.logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	lines := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		lines++
		for _, leak := range []string{proPrompt, "How much", "costs $49", "Answer in one sentence"} {
			if strings.Contains(line, leak) {
				t.Fatalf("decision log holds %q: %s", leak, line)
			}
		}
		for _, want := range []string{`"discovered":true`, `"discovery":"confirmed"`, `"discovery_query":"Acme Pro plan price per month"`, `"discovery_rank":1`, `"discovery_score":0.83`, `"found_by":"tavily_search"`} {
			if !strings.Contains(line, want) {
				t.Fatalf("decision log lacks %s: %s", want, line)
			}
		}
	}
	if lines != 1 {
		t.Fatalf("%d decision lines, want 1", lines)
	}
}
