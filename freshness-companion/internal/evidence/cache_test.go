package evidence

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// countingProvider counts calls and can block or fail.
type countingProvider struct {
	calls   atomic.Int32
	content string
	err     error
	gate    chan struct{} // when non-nil, Fetch waits for it to close
	started chan struct{} // when non-nil, receives one value per call
}

func (p *countingProvider) Name() string { return "counting" }

func (p *countingProvider) Fetch(ctx context.Context, sourceURL string) (Evidence, error) {
	p.calls.Add(1)
	if p.started != nil {
		p.started <- struct{}{}
	}
	if p.gate != nil {
		<-p.gate
	}
	if p.err != nil {
		return Evidence{}, p.err
	}
	return Evidence{URL: sourceURL, Content: p.content}, nil
}

func TestInvalidateMakesTheNextLookupFetchAgain(t *testing.T) {
	p := &countingProvider{content: "Acme Pro plan — $49 per month"}
	c := NewCache(p, CacheConfig{TTL: time.Hour})
	ctx := context.Background()
	if _, err := c.Get(ctx, "https://example.com/p"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(ctx, "https://example.com/p"); err != nil || p.calls.Load() != 1 {
		t.Fatalf("calls = %d, want the second lookup served from the cache", p.calls.Load())
	}
	if !c.Invalidate("https://example.com/p") || c.Invalidate("https://example.com/p") {
		t.Fatal("Invalidate reports whether it dropped anything")
	}
	r, err := c.Get(ctx, "https://example.com/p")
	if err != nil || r.Cached || p.calls.Load() != 2 {
		t.Fatalf("after Invalidate: cached=%v calls=%d err=%v", r.Cached, p.calls.Load(), err)
	}
}

func TestInvalidateDuringAFetchDoesNotKeepItsResult(t *testing.T) {
	p := &countingProvider{content: "old", gate: make(chan struct{}), started: make(chan struct{}, 4)}
	c := NewCache(p, CacheConfig{TTL: time.Hour})
	done := make(chan Result, 1)
	go func() {
		r, _ := c.Get(context.Background(), "https://example.com/p")
		done <- r
	}()
	<-p.started
	if !c.Invalidate("https://example.com/p") {
		t.Fatal("a running fetch counts as something to drop")
	}
	close(p.gate)
	if r := <-done; r.Evidence.Content != "old" {
		t.Fatal("the waiting caller still gets its answer")
	}
	p2 := p.calls.Load()
	if _, err := c.Get(context.Background(), "https://example.com/p"); err != nil || p.calls.Load() != p2+1 {
		t.Fatalf("the result of the invalidated fetch must not have been kept (calls %d -> %d)", p2, p.calls.Load())
	}
}

func TestCacheReusesEvidenceWithinTTL(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)}
	p := &countingProvider{content: "Acme Pro plan — $49 per month"}
	c := NewCache(p, CacheConfig{TTL: 60 * time.Second, Now: clock.Now})
	ctx := context.Background()

	first, err := c.Get(ctx, "https://example.com/pricing")
	if err != nil || first.Cached {
		t.Fatalf("first Get: %+v, %v", first, err)
	}
	for i := 1; i <= 5; i++ { // 10 s ... 50 s after the fetch
		clock.Advance(10 * time.Second)
		r, err := c.Get(ctx, "https://example.com/pricing")
		if err != nil || !r.Cached {
			t.Fatalf("Get at +%ds: %+v, %v; want the cached evidence", i*10, r, err)
		}
	}
	if got := p.calls.Load(); got != 1 {
		t.Fatalf("provider calls = %d after 6 lookups within the TTL, want 1", got)
	}
	clock.Advance(10 * time.Second) // 60 s: the TTL is over
	if r, err := c.Get(ctx, "https://example.com/pricing"); err != nil || r.Cached {
		t.Fatalf("Get after the TTL: %+v, %v; want a new fetch", r, err)
	}
	if got := p.calls.Load(); got != 2 {
		t.Fatalf("provider calls = %d, want 2 (one fetch, then one after the TTL expired)", got)
	}
	if first.Evidence.Provider != "counting" || first.Evidence.FetchedAt.IsZero() {
		t.Fatalf("defaults not filled: %+v", first.Evidence)
	}
	// A different URL is a different entry.
	if _, err := c.Get(ctx, "https://example.com/other"); err != nil || p.calls.Load() != 3 {
		t.Fatalf("second URL: calls = %d, err = %v", p.calls.Load(), err)
	}
}

func TestCacheRemembersFailuresBriefly(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	p := &countingProvider{err: errors.New("tavily down")}
	c := NewCache(p, CacheConfig{TTL: time.Minute, ErrorTTL: 10 * time.Second, Now: clock.Now})
	for i := 0; i < 3; i++ {
		if _, err := c.Get(context.Background(), "https://example.com/"); err == nil {
			t.Fatal("want the provider error")
		}
	}
	if p.calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1 while the failure is remembered", p.calls.Load())
	}
	clock.Advance(11 * time.Second)
	_, _ = c.Get(context.Background(), "https://example.com/")
	if p.calls.Load() != 2 {
		t.Fatalf("calls = %d, want a retry after ErrorTTL", p.calls.Load())
	}
}

func TestCacheCoalescesConcurrentFetches(t *testing.T) {
	p := &countingProvider{content: "x", gate: make(chan struct{}), started: make(chan struct{}, 16)}
	c := NewCache(p, CacheConfig{TTL: time.Minute})
	const n = 10
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.Get(context.Background(), "https://example.com/")
			errs <- err
		}()
	}
	<-p.started
	time.Sleep(20 * time.Millisecond) // let the other callers queue on the in-flight fetch
	close(p.gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := p.calls.Load(); got != 1 {
		t.Fatalf("provider calls = %d, want 1 for %d concurrent lookups", got, n)
	}
}

func TestCacheWaiterCanGiveUp(t *testing.T) {
	p := &countingProvider{content: "x", gate: make(chan struct{})}
	c := NewCache(p, CacheConfig{TTL: time.Minute})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.Get(ctx, "https://example.com/"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want the caller's deadline", err)
	}
	close(p.gate) // the detached fetch finishes and fills the cache
	deadline := time.Now().Add(2 * time.Second)
	for {
		r, err := c.Get(context.Background(), "https://example.com/")
		if err == nil && r.Cached {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cache not filled by the detached fetch: %+v, %v", r, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if p.calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", p.calls.Load())
	}
}

type panicProvider struct{}

func (panicProvider) Name() string { return "panicky" }
func (panicProvider) Fetch(context.Context, string) (Evidence, error) {
	panic("boom")
}

func TestCacheSurvivesProviderPanic(t *testing.T) {
	c := NewCache(panicProvider{}, CacheConfig{})
	if _, err := c.Get(context.Background(), "https://example.com/"); err == nil {
		t.Fatal("a panicking provider must surface as an error")
	}
}

// fakeSchedule records every callback and hands out a TTL per URL.
type fakeSchedule struct {
	mu          sync.Mutex
	ttl         map[string]time.Duration
	fetched     []string
	fetchedURLs []string // Evidence.URL as seen by Fetched
	errs        []error
	used        []string
	invalidated []string
	onFetched   func(url string) // runs inside Fetched (tests re-enter the cache here)
}

func (s *fakeSchedule) TTL(url string) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ttl[url]
}

func (s *fakeSchedule) Fetched(url string, ev Evidence, err error) {
	s.mu.Lock()
	s.fetched = append(s.fetched, url)
	s.fetchedURLs = append(s.fetchedURLs, ev.URL)
	s.errs = append(s.errs, err)
	cb := s.onFetched
	s.mu.Unlock()
	if cb != nil {
		cb(url)
	}
}

func (s *fakeSchedule) Used(url string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.used = append(s.used, url)
}

func (s *fakeSchedule) Invalidated(url string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidated = append(s.invalidated, url)
}

// stampingProvider returns Evidence whose URL carries a _fresh stamp, as the
// Tavily provider does.
type stampingProvider struct{ countingProvider }

func (p *stampingProvider) Fetch(ctx context.Context, sourceURL string) (Evidence, error) {
	ev, err := p.countingProvider.Fetch(ctx, sourceURL)
	if err == nil {
		ev.URL = sourceURL + "?_fresh=1"
	}
	return ev, err
}

func TestScheduleTTLUsedPerSource(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	p := &countingProvider{content: "ok"}
	sched := &fakeSchedule{ttl: map[string]time.Duration{"https://a/": 10 * time.Second, "https://b/": time.Minute}}
	c := NewCache(p, CacheConfig{TTL: time.Second, Now: clock.Now, Schedule: sched})
	ctx := context.Background()
	for _, u := range []string{"https://a/", "https://b/"} {
		if _, err := c.Get(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	clock.Advance(30 * time.Second) // past a's 10 s, within b's 60 s, far past the fixed 1 s
	ra, _ := c.Get(ctx, "https://a/")
	rb, _ := c.Get(ctx, "https://b/")
	if ra.Cached || !rb.Cached {
		t.Fatalf("a cached=%v (want false), b cached=%v (want true)", ra.Cached, rb.Cached)
	}
	if got := p.calls.Load(); got != 3 {
		t.Fatalf("provider calls = %d, want 3", got)
	}
	if len(sched.used) != 4 {
		t.Fatalf("Used called %d times, want 4 (once per lookup)", len(sched.used))
	}
	c.Invalidate("https://b/")
	if len(sched.invalidated) != 1 || sched.invalidated[0] != "https://b/" {
		t.Fatalf("Invalidated = %v", sched.invalidated)
	}
}

func TestNilScheduleKeepsFixedTTL(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	p := &countingProvider{content: "ok"}
	c := NewCache(p, CacheConfig{TTL: 5 * time.Second, Now: clock.Now})
	ctx := context.Background()
	c.Get(ctx, "https://a/")
	clock.Advance(4 * time.Second)
	if r, _ := c.Get(ctx, "https://a/"); !r.Cached {
		t.Fatal("within the fixed TTL the evidence must be reused")
	}
	clock.Advance(2 * time.Second)
	if r, _ := c.Get(ctx, "https://a/"); r.Cached {
		t.Fatal("after the fixed TTL the evidence must be fetched again")
	}
	if got := p.calls.Load(); got != 2 {
		t.Fatalf("provider calls = %d, want 2", got)
	}
}

func TestFetchedCalledOutsideLockWithUnstampedKey(t *testing.T) {
	p := &stampingProvider{countingProvider{content: "ok"}}
	sched := &fakeSchedule{ttl: map[string]time.Duration{}}
	var c *Cache
	reentered := make(chan bool, 1)
	sched.onFetched = func(url string) {
		// Re-entering the cache deadlocks if Fetched runs under the lock. The
		// test's own timeout would then fail it.
		c.Invalidate("https://other/")
		reentered <- true
	}
	c = NewCache(p, CacheConfig{TTL: time.Minute, Schedule: sched})
	if _, err := c.Get(context.Background(), "https://a/"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reentered:
	case <-time.After(5 * time.Second):
		t.Fatal("Fetched never re-entered the cache: it is probably called under the lock")
	}
	if len(sched.fetched) != 1 || sched.fetched[0] != "https://a/" {
		t.Fatalf("Fetched got key %v, want the cache key https://a/", sched.fetched)
	}
	if sched.fetchedURLs[0] != "https://a/?_fresh=1" {
		t.Fatalf("Evidence.URL = %q: the stamped URL should still reach the schedule inside the Evidence", sched.fetchedURLs[0])
	}
}

func TestRefreshForcesFetchAndJoinsRunning(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	p := &countingProvider{content: "v1", started: make(chan struct{}, 8)}
	c := NewCache(p, CacheConfig{TTL: time.Hour, Now: clock.Now})
	ctx := context.Background()
	// First fetch, completed (no gate: it runs straight through).
	if _, err := c.Get(ctx, "https://a/"); err != nil {
		t.Fatal(err)
	}
	<-p.started
	if r, _ := c.Get(ctx, "https://a/"); !r.Cached {
		t.Fatal("evidence should be reused within the TTL")
	}
	// Refresh ignores the valid entry and fetches; a second Refresh and a
	// Get that arrives meanwhile join the same provider call.
	p.gate = make(chan struct{})
	p.content = "v2"
	results := make(chan Result, 3)
	go func() { r, _ := c.Refresh(ctx, "https://a/"); results <- r }()
	<-p.started // the refresh's provider call is running and blocked on the gate
	entered := make(chan struct{})
	go func() {
		close(entered)
		r, _ := c.Refresh(ctx, "https://a/")
		results <- r
	}()
	<-entered
	// The second Refresh is about to take the cache lock and join the flight.
	// Give it a moment: the gate below is what lets the shared fetch finish.
	time.Sleep(50 * time.Millisecond)
	// The entry is still valid, so a plain Get answers from it without joining.
	if r, _ := c.Get(ctx, "https://a/"); !r.Cached || r.Evidence.Content != "v1" {
		t.Fatalf("Get during a refresh = %+v, want the still-valid v1", r)
	}
	close(p.gate)
	for i := 0; i < 2; i++ {
		r := <-results
		if r.Cached || r.Evidence.Content != "v2" {
			t.Fatalf("Refresh result = %+v, want fresh v2", r)
		}
	}
	if got := p.calls.Load(); got != 2 {
		t.Fatalf("provider calls = %d, want 2 (one initial, one shared refresh)", got)
	}
	if r, _ := c.Get(ctx, "https://a/"); !r.Cached || r.Evidence.Content != "v2" {
		t.Fatalf("after the refresh Get = %+v, want cached v2", r)
	}
	if s := c.Stats(); s.Refreshes != 2 {
		t.Fatalf("Stats.Refreshes = %d, want 2", s.Refreshes)
	}
}

func TestRefreshErrorKeepsValidEvidence(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	p := &countingProvider{content: "v1"}
	c := NewCache(p, CacheConfig{TTL: time.Minute, ErrorTTL: 10 * time.Second, Now: clock.Now})
	ctx := context.Background()
	if _, err := c.Get(ctx, "https://a/"); err != nil {
		t.Fatal(err)
	}
	p.err = errors.New("source down")
	if _, err := c.Refresh(ctx, "https://a/"); err == nil {
		t.Fatal("Refresh must report the fetch error")
	}
	r, err := c.Get(ctx, "https://a/")
	if err != nil || !r.Cached || r.Evidence.Content != "v1" {
		t.Fatalf("after a failed refresh Get = %+v, %v; want the still-valid v1", r, err)
	}
	// Once the evidence has expired, a failure is remembered as before.
	clock.Advance(2 * time.Minute)
	if _, err := c.Get(ctx, "https://a/"); err == nil {
		t.Fatal("expired evidence plus a failing provider must be an error")
	}
	if _, err := c.Get(ctx, "https://a/"); err == nil {
		t.Fatal("the failure should be remembered for ErrorTTL")
	}
	if got := p.calls.Load(); got != 3 {
		t.Fatalf("provider calls = %d, want 3", got)
	}
}

func TestStatsCounts(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	p := &countingProvider{content: "v1"}
	c := NewCache(p, CacheConfig{TTL: time.Minute, ErrorTTL: time.Second, Now: clock.Now})
	ctx := context.Background()
	c.Get(ctx, "https://a/") // waited, provider call
	c.Get(ctx, "https://a/") // reused
	c.Get(ctx, "https://a/") // reused
	c.Refresh(ctx, "https://a/")
	p.err = errors.New("down")
	c.Refresh(ctx, "https://a/") // provider error, evidence kept
	want := Stats{Lookups: 3, Reused: 2, Waited: 1, ProviderCalls: 3, ProviderErrors: 1, Refreshes: 2}
	if got := c.Stats(); got != want {
		t.Fatalf("Stats = %+v, want %+v", got, want)
	}
}
