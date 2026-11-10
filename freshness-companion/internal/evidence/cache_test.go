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
