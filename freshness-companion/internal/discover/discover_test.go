package discover

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"freshness-companion/internal/evidence"
	"freshness-companion/internal/extract"
	"freshness-companion/internal/policy"
	"freshness-companion/internal/watch"
)

func discoverBlock(t *testing.T, block string) *policy.Discover {
	t.Helper()
	p, err := policy.Parse([]byte(fmt.Sprintf(`{"version": 1, "categories": [
	  {"name": "price", "keywords": ["price", "cost", "how much"], "discover": %s}]}`, block)))
	if err != nil {
		t.Fatal(err)
	}
	return p.Categories[0].Discover
}

const acmeBlock = `{"enabled": true, "entities": ["Acme Pro", "Acme Team"], "query": "{entities} plan price per month"}`

// ---- query ----

func TestQueryAllowlistOnly(t *testing.T) {
	d := discoverBlock(t, acmeBlock)
	q, ents, err := BuildQuery(d, "how much does the ACME pro plan cost for my company Initech?")
	if err != nil {
		t.Fatal(err)
	}
	if q != "Acme Pro plan price per month" || len(ents) != 1 || ents[0] != "Acme Pro" {
		t.Fatalf("q=%q ents=%v", q, ents)
	}
	// Whole words only: "Acme Professional" is not "Acme Pro".
	if _, _, err := BuildQuery(d, "What does Acme Professional cost?"); !errors.Is(err, ErrNoEntities) {
		t.Fatalf("partial word must not match: %v", err)
	}
	q, _, _ = BuildQuery(d, "Acme Team vs Acme Pro: price?")
	if q != "Acme Pro Acme Team plan price per month" {
		t.Fatalf("q=%q (policy order expected)", q)
	}
}

func TestQueryNeverContainsPromptWords(t *testing.T) {
	d := discoverBlock(t, acmeBlock)
	question := "My boss Jennifer Zyxwv at Globex secretly wants the Acme Pro price, how much is it?"
	q, _, err := BuildQuery(d, question)
	if err != nil {
		t.Fatal(err)
	}
	template := map[string]bool{"acme": true, "pro": true, "plan": true, "price": true, "per": true, "month": true}
	for _, w := range strings.Fields(strings.ToLower(q)) {
		if !template[w] {
			t.Fatalf("query %q holds %q, which is neither the template nor an allowlisted name", q, w)
		}
	}
	for _, w := range []string{"jennifer", "zyxwv", "globex", "boss", "secretly"} {
		if strings.Contains(strings.ToLower(q), w) {
			t.Fatalf("query %q leaks %q", q, w)
		}
	}
}

func TestQueryProperNounsDropsDigits(t *testing.T) {
	d := discoverBlock(t, `{"enabled": true, "query_from": "proper_nouns", "query": "{entities} price"}`)
	q, ents, err := BuildQuery(d, "How much does the Raspberry Pi 5 8GB cost in the United Kingdom Store Today, Tell Me?")
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 2 || ents[0] != "Raspberry Pi" || ents[1] != "United Kingdom Store" {
		t.Fatalf("ents=%v", ents)
	}
	if strings.ContainsAny(q, "0123456789") {
		t.Fatalf("query %q contains digits", q)
	}
	if _, _, err := BuildQuery(d, "how much is it?"); !errors.Is(err, ErrNoEntities) {
		t.Fatalf("no capitalised names: %v", err)
	}
	if _, _, err := BuildQuery(d, "What Is The Price?"); !errors.Is(err, ErrNoEntities) {
		t.Fatalf("stopwords only: %v", err)
	}
}

func TestQuerySensitiveRefused(t *testing.T) {
	d := discoverBlock(t, acmeBlock)
	for _, question := range []string{
		"How much is Acme Pro? mail me at jane.doe@example.com",
		"Acme Pro price, call +44 20 7946 0958",
		"Acme Pro price for card 4111 1111 1111 1111",
	} {
		if _, _, err := BuildQuery(d, question); !errors.Is(err, ErrSensitive) {
			t.Errorf("%q: got %v, want ErrSensitive", question, err)
		}
	}
	// A template that makes the query too long is refused too.
	long := discoverBlock(t, `{"enabled": true, "entities": ["Acme Pro"], "query": "{entities} `+strings.Repeat("price ", 14)+`"}`)
	if _, _, err := BuildQuery(long, "Acme Pro price?"); !errors.Is(err, ErrQueryTooLong) {
		t.Fatalf("got %v, want ErrQueryTooLong", err)
	}
}

func TestQueryNoEntitiesNoSearch(t *testing.T) {
	d := discoverBlock(t, acmeBlock)
	fs := &fakeSearcher{}
	s := newService(t, fs, Config{})
	_, err := s.Find(context.Background(), "price", d, "How much does the Globex plan cost?")
	if !errors.Is(err, ErrNoEntities) || fs.calls.Load() != 0 {
		t.Fatalf("err=%v calls=%d", err, fs.calls.Load())
	}
	if s.Stats().RejectedByReason["no_entities"] != 1 {
		t.Fatalf("stats = %+v", s.Stats())
	}
}

// ---- pick ----

func res(url, title, content string, score float64) evidence.SearchResult {
	return evidence.SearchResult{URL: url, Title: title, Content: content, Score: score}
}

func TestPickScoreThreshold(t *testing.T) {
	d := discoverBlock(t, acmeBlock)
	_, why, ok := Pick([]evidence.SearchResult{res("https://acme.example/p", "Acme Pro", "Acme Pro $49", 0.49)}, d, []string{"Acme Pro"})
	if ok || !strings.Contains(why, "low_score") {
		t.Fatalf("ok=%v why=%q", ok, why)
	}
	r, _, ok := Pick([]evidence.SearchResult{res("https://acme.example/p", "Acme Pro", "Acme Pro $49", 0.5)}, d, []string{"Acme Pro"})
	if !ok || r.URL != "https://acme.example/p" {
		t.Fatal("score 0.5 must pass the default threshold")
	}
}

func TestPickIncludeDomains(t *testing.T) {
	d := discoverBlock(t, `{"enabled": true, "entities": ["Acme Pro"], "query": "{entities} price", "include_domains": ["acme.example"]}`)
	results := []evidence.SearchResult{
		res("https://evil.example/acme-pro", "Acme Pro", "Acme Pro $9", 0.99),
		res("https://acme.example.evil.example/p", "Acme Pro", "Acme Pro $9", 0.98),
		res("https://shop.acme.example/pricing", "Acme Pro", "Acme Pro $49", 0.6),
	}
	r, _, ok := Pick(results, d, []string{"Acme Pro"})
	if !ok || r.URL != "https://shop.acme.example/pricing" || r.Rank != 3 {
		t.Fatalf("picked %+v", r)
	}
}

func TestPickExcludesAggregators(t *testing.T) {
	d := discoverBlock(t, `{"enabled": true, "entities": ["Acme Pro"], "query": "{entities} price", "exclude_domains": ["pricespy.example"]}`)
	results := []evidence.SearchResult{
		res("https://www.reddit.com/r/acme", "Acme Pro", "Acme Pro $10", 0.9),
		res("https://en.wikipedia.org/wiki/Acme", "Acme Pro", "Acme Pro $10", 0.9),
		res("https://www.amazon.co.uk/acme", "Acme Pro", "Acme Pro $10", 0.9),
		res("https://pricespy.example/acme", "Acme Pro", "Acme Pro $10", 0.9),
	}
	_, why, ok := Pick(results, d, []string{"Acme Pro"})
	if ok || !strings.Contains(why, "4 excluded_domain") {
		t.Fatalf("ok=%v why=%q", ok, why)
	}
}

func TestPickNeedsEntityInSnippet(t *testing.T) {
	d := discoverBlock(t, acmeBlock)
	results := []evidence.SearchResult{
		res("https://acme.example/blog", "Acme news", "Our team grew", 0.9),
		res("https://acme.example/pricing", "Pricing", "The Acme Pro plan is $49", 0.7),
	}
	r, _, ok := Pick(results, d, []string{"Acme Pro"})
	if !ok || r.URL != "https://acme.example/pricing" {
		t.Fatalf("picked %+v", r)
	}
	_, why, ok := Pick(results[:1], d, []string{"Acme Pro"})
	if ok || !strings.Contains(why, "entity_not_in_snippet") {
		t.Fatalf("why=%q", why)
	}
}

func TestPickPrefersSameSite(t *testing.T) {
	d := discoverBlock(t, acmeBlock)
	results := []evidence.SearchResult{
		res("https://reviews.example/best-plans", "Acme Pro review", "Acme Pro costs $45", 0.95),
		res("https://acme.example/pricing", "Acme pricing", "Acme Pro plan $49", 0.7),
	}
	r, _, ok := Pick(results, d, []string{"Acme Pro"})
	if !ok || r.URL != "https://acme.example/pricing" || r.Rank != 2 {
		t.Fatalf("picked %+v, want the same-site result", r)
	}
}

func TestPickRejectsIPAndHTTP(t *testing.T) {
	d := discoverBlock(t, acmeBlock)
	for _, u := range []string{"http://acme.example/p", "https://127.0.0.1/p", "https://[::1]/p", "https://localhost/p", "https://api.localhost/p", "https://user:pw@acme.example/p", "ftp://acme.example/p"} {
		if _, _, ok := Pick([]evidence.SearchResult{res(u, "Acme Pro", "Acme Pro $49", 0.9)}, d, []string{"Acme Pro"}); ok {
			t.Errorf("%s must be rejected", u)
		}
	}
}

// ---- pattern ----

func TestEntityPatternSingleValue(t *testing.T) {
	re, err := EntityPattern([]string{"Acme Pro"}, policy.DefaultNumericValuePattern, 40)
	if err != nil {
		t.Fatal(err)
	}
	page := "Acme pricing\nPro plan\nAcme Pro plan — $49 per month\nLast updated 2 November 2026."
	v, err := extract.Extract(re, page, extract.Numeric)
	if err != nil || v.Raw != "49" {
		t.Fatalf("v=%+v err=%v", v, err)
	}
	// A digit between the name and the value breaks the link.
	if _, err := extract.Extract(re, "Acme Pro 2 seats from $49", extract.Numeric); !errors.Is(err, extract.ErrNoMatch) {
		t.Fatalf("digit in between: %v", err)
	}
	// So does a line break.
	if _, err := extract.Extract(re, "Acme Pro\n$49", extract.Numeric); !errors.Is(err, extract.ErrNoMatch) {
		t.Fatalf("line break in between: %v", err)
	}
}

func TestEntityPatternConflictIsUnverifiable(t *testing.T) {
	re, err := EntityPattern([]string{"Acme Pro"}, policy.DefaultNumericValuePattern, 40)
	if err != nil {
		t.Fatal(err)
	}
	table := "Acme Pro monthly: $19\nAcme Pro yearly: $49\nAcme Pro enterprise: $99"
	_, err = extract.Extract(re, table, extract.Numeric)
	var conflict *extract.ConflictError
	if !errors.As(err, &conflict) || len(conflict.Values) != 3 {
		t.Fatalf("a pricing table must be a conflict, got %v", err)
	}
}

// ---- service ----

type fakeSearcher struct {
	mu      sync.Mutex
	calls   atomic.Int32
	results []evidence.SearchResult
	err     error
	delay   time.Duration
	queries []string
}

func (f *fakeSearcher) Search(ctx context.Context, req evidence.SearchRequest) ([]evidence.SearchResult, error) {
	f.calls.Add(1)
	f.mu.Lock()
	f.queries = append(f.queries, req.Query)
	f.mu.Unlock()
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	out := make([]evidence.SearchResult, len(f.results))
	copy(out, f.results)
	for i := range out {
		out[i].Rank = i + 1
	}
	return out, nil
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newService(t *testing.T, fs *fakeSearcher, cfg Config) *Service {
	t.Helper()
	cfg.Searcher = fs
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

var acmeResults = []evidence.SearchResult{res("https://acme.example/pricing", "Acme pricing", "Acme Pro plan — $49 per month", 0.83)}

func TestServiceReusesWithinTTL(t *testing.T) {
	d := discoverBlock(t, acmeBlock)
	c := &clock{t: time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)}
	fs := &fakeSearcher{results: acmeResults}
	s := newService(t, fs, Config{TTL: time.Hour, Now: c.Now})
	f, err := s.Find(context.Background(), "price", d, "How much is Acme Pro?")
	if err != nil || f.Reused || f.FoundBy != FoundByTavilySearch || f.Sources[0].URL != "https://acme.example/pricing" || f.Sources[0].Rank != 1 {
		t.Fatalf("first find: %+v %v", f, err)
	}
	c.add(30 * time.Minute)
	f, err = s.Find(context.Background(), "price", d, "acme pro price per month?")
	if err != nil || !f.Reused || fs.calls.Load() != 1 {
		t.Fatalf("second find should reuse: %+v %v calls=%d", f, err, fs.calls.Load())
	}
	c.add(31 * time.Minute)
	if _, err := s.Find(context.Background(), "price", d, "How much is Acme Pro?"); err != nil || fs.calls.Load() != 2 {
		t.Fatalf("after the TTL a new search is due: calls=%d err=%v", fs.calls.Load(), err)
	}
	// The same query in another category is another key.
	if _, err := s.Find(context.Background(), "other", d, "How much is Acme Pro?"); err != nil || fs.calls.Load() != 3 {
		t.Fatalf("calls=%d", fs.calls.Load())
	}
	st := s.Stats()
	if st.Searches != 3 || st.Reused != 1 || st.SearchesLastHour != 2 { // the first search is over an hour old
		t.Fatalf("stats = %+v", st)
	}
}

func TestServiceNegativeCache(t *testing.T) {
	d := discoverBlock(t, acmeBlock)
	c := &clock{t: time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)}
	fs := &fakeSearcher{results: []evidence.SearchResult{res("https://www.reddit.com/r/acme", "Acme Pro", "Acme Pro $10", 0.9)}}
	s := newService(t, fs, Config{MissTTL: 10 * time.Minute, Now: c.Now})
	_, err := s.Find(context.Background(), "price", d, "Acme Pro price?")
	var ns *NoSourceError
	if !errors.As(err, &ns) || !errors.Is(err, ErrNoSource) || ns.Hosts[0] != "www.reddit.com" || !strings.Contains(ns.Detail, "excluded_domain") {
		t.Fatalf("err = %v", err)
	}
	_, err = s.Find(context.Background(), "price", d, "Acme Pro price?")
	if !errors.As(err, &ns) || !ns.Cached || fs.calls.Load() != 1 {
		t.Fatalf("a remembered miss must not search again: %v calls=%d", err, fs.calls.Load())
	}
	c.add(11 * time.Minute)
	_, _ = s.Find(context.Background(), "price", d, "Acme Pro price?")
	if fs.calls.Load() != 2 {
		t.Fatalf("after miss_ttl a new search is due: calls=%d", fs.calls.Load())
	}
	// With a candidate URL, the miss becomes a candidate_url source.
	dc := discoverBlock(t, `{"enabled": true, "entities": ["Acme Pro"], "query": "{entities} cost", "candidate_urls": ["https://acme.example/pricing"]}`)
	f, err := s.Find(context.Background(), "price", dc, "Acme Pro price?")
	if err != nil || f.FoundBy != FoundByCandidateURL || f.Sources[0].URL != "https://acme.example/pricing" {
		t.Fatalf("candidate: %+v %v", f, err)
	}
	// A miss remembered before the policy named a candidate URL: the
	// candidate is used without a new search.
	calls := fs.calls.Load()
	dc2 := discoverBlock(t, `{"enabled": true, "entities": ["Acme Pro"], "query": "{entities} plan price per month", "candidate_urls": ["https://acme.example/pricing"]}`)
	f, err = s.Find(context.Background(), "price", dc2, "Acme Pro price?")
	if err != nil || f.FoundBy != FoundByCandidateURL || fs.calls.Load() != calls {
		t.Fatalf("remembered miss + candidate: %+v %v calls %d -> %d", f, err, calls, fs.calls.Load())
	}
}

func TestServiceHourlyCap(t *testing.T) {
	d := discoverBlock(t, acmeBlock)
	c := &clock{t: time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)}
	fs := &fakeSearcher{results: []evidence.SearchResult{res("https://acme.example/pricing", "Acme pricing", "Acme Pro plan $49, Acme Team plan $99", 0.83)}}
	s := newService(t, fs, Config{Budget: watch.NewBudget(1, c.Now), Now: c.Now})
	if _, err := s.Find(context.Background(), "price", d, "Acme Pro price?"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Find(context.Background(), "price", d, "Acme Team price?"); !errors.Is(err, ErrOverCap) || Reason(err) != "over_cap" {
		t.Fatalf("err = %v", err)
	}
	if fs.calls.Load() != 1 {
		t.Fatalf("calls = %d", fs.calls.Load())
	}
	// Reuse costs nothing, even over the cap.
	if f, err := s.Find(context.Background(), "price", d, "Acme Pro price?"); err != nil || !f.Reused {
		t.Fatalf("reuse over the cap: %+v %v", f, err)
	}
	c.add(61 * time.Minute)
	if _, err := s.Find(context.Background(), "price", d, "Acme Team price?"); err != nil {
		t.Fatalf("a new hour: %v", err)
	}
	if st := s.Stats(); st.OverCap != 1 || st.SearchCap != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestServiceSharesConcurrentSearch(t *testing.T) {
	d := discoverBlock(t, acmeBlock)
	fs := &fakeSearcher{results: acmeResults, delay: 100 * time.Millisecond}
	s := newService(t, fs, Config{})
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f, err := s.Find(context.Background(), "price", d, "How much is Acme Pro?")
			if err == nil && f.Sources[0].URL != "https://acme.example/pricing" {
				err = fmt.Errorf("wrong source %+v", f)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if fs.calls.Load() != 1 {
		t.Fatalf("8 concurrent finds made %d searches, want 1", fs.calls.Load())
	}
}

func TestServicePersists(t *testing.T) {
	d := discoverBlock(t, acmeBlock)
	path := filepath.Join(t.TempDir(), "sub", "discovered.json")
	c := &clock{t: time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)}
	fs := &fakeSearcher{results: acmeResults}
	s := newService(t, fs, Config{StorePath: path, Now: c.Now})
	if _, err := s.Find(context.Background(), "price", d, "Acme Pro price?"); err != nil {
		t.Fatal(err)
	}
	s.Record("price", "Acme Pro plan price per month", "confirmed", "49")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"Acme Pro price?", "How much"} {
		if strings.Contains(string(data), leak) {
			t.Fatalf("store holds question text %q", leak)
		}
	}
	// A new service reads the file and reuses the source without searching.
	fs2 := &fakeSearcher{results: acmeResults}
	s2 := newService(t, fs2, Config{StorePath: path, Now: c.Now})
	f, err := s2.Find(context.Background(), "price", d, "Acme Pro price?")
	if err != nil || !f.Reused || fs2.calls.Load() != 0 {
		t.Fatalf("restart: %+v %v calls=%d", f, err, fs2.calls.Load())
	}
	src := s2.Sources()
	if len(src) != 1 || src[0].LastOutcome != "confirmed" || src[0].LastValue != "49" || src[0].Uses != 2 {
		t.Fatalf("sources = %+v", src)
	}
	// Expired entries are not loaded.
	c.add(25 * time.Hour)
	s3 := newService(t, &fakeSearcher{}, Config{StorePath: path, Now: c.Now})
	if len(s3.Sources()) != 0 {
		t.Fatalf("expired entries loaded: %+v", s3.Sources())
	}
}

func TestServiceMaxSourcesAndSearchFailure(t *testing.T) {
	d := discoverBlock(t, `{"enabled": true, "entities": ["Acme Pro", "Acme Team", "Acme Max"], "query": "{entities} price"}`)
	c := &clock{t: time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)}
	fs := &fakeSearcher{results: []evidence.SearchResult{res("https://acme.example/p", "Acme", "Acme Pro Acme Team Acme Max $1", 0.9)}}
	s := newService(t, fs, Config{MaxSources: 2, Now: c.Now})
	for _, q := range []string{"Acme Pro?", "Acme Team?", "Acme Max?"} {
		if _, err := s.Find(context.Background(), "price", d, q); err != nil {
			t.Fatal(err)
		}
		c.add(time.Second)
	}
	if n := len(s.Sources()); n != 2 {
		t.Fatalf("kept %d sources, want 2", n)
	}
	fs.err = errors.New("boom")
	if _, err := s.Find(context.Background(), "price", d, "Acme Pro and Acme Max?"); Reason(err) != "search_failed" {
		t.Fatalf("err = %v", err)
	}
}

func TestDiscoveredEndpointShape(t *testing.T) {
	d := discoverBlock(t, acmeBlock)
	s := newService(t, &fakeSearcher{results: acmeResults}, Config{})
	_, _ = s.Find(context.Background(), "price", d, "Acme Pro price?")
	mux := http.NewServeMux()
	s.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/freshness/discovered", nil))
	body := rec.Body.String()
	for _, want := range []string{`"enabled":true`, `"query":"Acme Pro plan price per month"`, `"url":"https://acme.example/pricing"`, `"found_by":"tavily_search"`, `"searches_last_hour":1`, `"searches_cap_per_hour":20`} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s in %s", want, body)
		}
	}
	// Off: a nil service still answers.
	var off *Service
	mux = http.NewServeMux()
	off.Register(mux)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/freshness/discovered", nil))
	if !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Fatalf("off: %s", rec.Body.String())
	}
}

// Learned sources read from the store file pass the same URL and domain
// rules as search results, on load and again on reuse; others are dropped.
func TestServiceRechecksStoredSources(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)}
	exp := c.t.Add(time.Hour).Format(time.RFC3339)
	path := filepath.Join(t.TempDir(), "discovered.json")
	store := fmt.Sprintf(`{"version": 1, "sources": [
	  {"category": "price", "query": "Acme Pro plan price per month", "url": "https://acme.example/pricing", "expires_at": %[1]q},
	  {"category": "price", "query": "Acme Team plan price per month", "url": "http://127.0.0.1/admin", "expires_at": %[1]q},
	  {"category": "price", "query": "Acme Max plan price per month", "url": "https://acme.example/max", "also": [{"url": "https://localhost/x"}], "expires_at": %[1]q},
	  {"category": "price", "query": "Acme Min plan price per month", "url": "https://www.reddit.com/r/acme", "expires_at": %[1]q},
	  {"category": "trusted", "query": "Acme Pro plan price per month", "url": "https://acme.example/pricing", "expires_at": %[1]q},
	  {"category": "price", "query": "Acme Gone plan price per month", "no_source": true, "reason": "none usable", "expires_at": %[1]q}]}`, exp)
	if err := os.WriteFile(path, []byte(store), 0o600); err != nil {
		t.Fatal(err)
	}
	open := discoverBlock(t, acmeBlock)
	trusted := discoverBlock(t, `{"enabled": true, "entities": ["Acme Pro"], "query": "{entities} plan price per month", "include_domains": ["other.example"]}`)
	rules := func(cat string) *policy.Discover {
		switch cat {
		case "price":
			return open
		case "trusted":
			return trusted
		}
		return nil
	}
	s := newService(t, &fakeSearcher{}, Config{StorePath: path, Now: c.Now, Rules: rules})
	var kept []string
	for _, l := range s.Sources() {
		kept = append(kept, l.Category+"|"+l.Query)
	}
	got := strings.Join(kept, ",")
	for _, want := range []string{"price|Acme Pro plan price per month", "price|Acme Gone plan price per month"} {
		if !strings.Contains(got, want) {
			t.Fatalf("kept %v, want %q", kept, want)
		}
	}
	if len(kept) != 2 {
		t.Fatalf("unsafe, excluded or out-of-domain sources were loaded: %v", kept)
	}

	// On reuse: loaded without Rules (only the URL rules), the page then
	// fails the category's include_domains, so it is dropped and searched
	// for again instead of being reused.
	fs := &fakeSearcher{results: []evidence.SearchResult{res("https://other.example/pricing", "Acme pricing", "Acme Pro plan — $49 per month", 0.9)}}
	s2 := newService(t, fs, Config{StorePath: path, Now: c.Now})
	f, err := s2.Find(context.Background(), "trusted", trusted, "How much is Acme Pro?")
	if err != nil || f.Reused || fs.calls.Load() != 1 || f.Sources[0].URL != "https://other.example/pricing" {
		t.Fatalf("reuse of an out-of-domain source: %+v %v calls=%d", f, err, fs.calls.Load())
	}
}
