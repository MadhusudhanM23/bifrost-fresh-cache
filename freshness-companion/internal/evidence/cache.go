package evidence

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Scheduler decides how long evidence for each source URL stays fresh, and
// is told about every fetch. The watch package implements it; nil keeps the
// fixed TTL. The URL passed in is always the cache key (the policy's
// source_url), never a provider-stamped URL.
type Scheduler interface {
	// TTL returns how long a successful fetch of sourceURL stays fresh.
	TTL(sourceURL string) time.Duration
	// Fetched is called after every provider call (success or error), with
	// no cache lock held.
	Fetched(sourceURL string, ev Evidence, err error)
	// Used is called on every lookup of sourceURL.
	Used(sourceURL string)
	// Invalidated is called when a change notice drops the evidence.
	Invalidated(sourceURL string)
}

// CacheConfig configures the per-URL evidence cache.
type CacheConfig struct {
	// TTL keeps successful evidence this long (e.g. 60 s) so the provider is
	// not called on every cache hit. Zero disables reuse.
	TTL time.Duration
	// ErrorTTL keeps a failure this long, so a provider outage is not
	// hammered. Zero disables it.
	ErrorTTL time.Duration
	// FetchTimeout bounds each provider call (default 15 s).
	FetchTimeout time.Duration
	// Now is the clock (tests inject one).
	Now func() time.Time
	// Schedule, when set, replaces TTL for successful fetches with a
	// per-source interval and is told about every fetch (smart checking).
	// nil keeps the fixed TTL: exactly the behaviour without it.
	Schedule Scheduler
}

// Result is evidence plus whether it came from the cache.
type Result struct {
	Evidence Evidence
	// Cached is true when the evidence was reused from an earlier fetch;
	// Evidence.FetchedAt then says how old it is.
	Cached bool
}

// Stats counts what the cache did since it was created.
type Stats struct {
	// Lookups is the number of Get calls.
	Lookups int64 `json:"lookups"`
	// Reused is how many lookups were answered from kept evidence.
	Reused int64 `json:"reused"`
	// Waited is how many lookups had to wait for a provider call.
	Waited int64 `json:"waited"`
	// ProviderCalls is the number of provider fetches (any caller).
	ProviderCalls int64 `json:"provider_calls"`
	// ProviderErrors is how many of those failed.
	ProviderErrors int64 `json:"provider_errors"`
	// Refreshes is the number of Refresh calls (the background watcher).
	Refreshes int64 `json:"refreshes"`
}

type cacheEntry struct {
	ev      Evidence
	err     error
	expires time.Time
}

type flight struct {
	done chan struct{}
	ev   Evidence
	err  error
}

// Cache wraps a Provider with a per-URL TTL cache and request coalescing:
// concurrent lookups of one URL share a single provider call.
type Cache struct {
	provider Provider
	cfg      CacheConfig

	mu       sync.Mutex
	entries  map[string]cacheEntry
	inflight map[string]*flight

	lookups, reused, waited, calls, callErrs, refreshes atomic.Int64
}

// NewCache returns a cache in front of p.
func NewCache(p Provider, cfg CacheConfig) *Cache {
	if cfg.FetchTimeout <= 0 {
		cfg.FetchTimeout = 15 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Cache{
		provider: p,
		cfg:      cfg,
		entries:  make(map[string]cacheEntry),
		inflight: make(map[string]*flight),
	}
}

// ProviderName returns the wrapped provider's name.
func (c *Cache) ProviderName() string { return c.provider.Name() }

// Stats returns the counters.
func (c *Cache) Stats() Stats {
	return Stats{
		Lookups:        c.lookups.Load(),
		Reused:         c.reused.Load(),
		Waited:         c.waited.Load(),
		ProviderCalls:  c.calls.Load(),
		ProviderErrors: c.callErrs.Load(),
		Refreshes:      c.refreshes.Load(),
	}
}

// Get returns evidence for sourceURL, from the cache when it is fresh enough.
// The provider call itself is detached from ctx (it is shared with other
// callers and bounded by FetchTimeout); ctx only bounds how long this caller
// waits.
func (c *Cache) Get(ctx context.Context, sourceURL string) (Result, error) {
	c.lookups.Add(1)
	if s := c.cfg.Schedule; s != nil {
		safeCall(func() { s.Used(sourceURL) })
	}
	c.mu.Lock()
	if e, ok := c.entries[sourceURL]; ok && c.cfg.Now().Before(e.expires) {
		c.mu.Unlock()
		c.reused.Add(1)
		return Result{Evidence: e.ev, Cached: true}, e.err
	}
	f := c.startLocked(sourceURL)
	c.mu.Unlock()
	c.waited.Add(1)
	return c.wait(ctx, f)
}

// Refresh fetches sourceURL now, whatever the cache holds, and keeps the
// result under the schedule's interval (or TTL). A fetch already running is
// joined instead of starting a second one. On a fetch error, evidence that is
// still valid stays in place: the error is returned but not stored over it.
func (c *Cache) Refresh(ctx context.Context, sourceURL string) (Result, error) {
	c.refreshes.Add(1)
	c.mu.Lock()
	f := c.startLocked(sourceURL)
	c.mu.Unlock()
	return c.wait(ctx, f)
}

// startLocked returns the running flight for sourceURL, or starts one.
func (c *Cache) startLocked(sourceURL string) *flight {
	f, running := c.inflight[sourceURL]
	if !running {
		f = &flight{done: make(chan struct{})}
		c.inflight[sourceURL] = f
		go c.fetch(sourceURL, f)
	}
	return f
}

func (c *Cache) wait(ctx context.Context, f *flight) (Result, error) {
	select {
	case <-f.done:
		return Result{Evidence: f.ev}, f.err
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}

func (c *Cache) fetch(sourceURL string, f *flight) {
	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.FetchTimeout)
	defer cancel()
	c.calls.Add(1)
	ev, err := c.safeFetch(ctx, sourceURL)
	if err != nil {
		c.callErrs.Add(1)
	}
	f.ev, f.err = ev, err
	ttl := c.cfg.TTL
	// The schedule is told with no lock held (it may call back into the
	// cache), and with the cache key, not the provider's stamped URL.
	if s := c.cfg.Schedule; s != nil {
		safeCall(func() { s.Fetched(sourceURL, ev, err) })
		if err == nil {
			safeCall(func() { ttl = s.TTL(sourceURL) })
		}
	}
	if err != nil {
		ttl = c.cfg.ErrorTTL
	}
	c.mu.Lock()
	// After Invalidate this flight is no longer the current one: its result
	// still answers its own waiters, but it must not be kept.
	if c.inflight[sourceURL] == f {
		now := c.cfg.Now()
		kept, have := c.entries[sourceURL]
		switch {
		case err != nil && have && kept.err == nil && now.Before(kept.expires):
			// A failed re-fetch must not replace evidence that is still valid.
		case ttl > 0:
			c.entries[sourceURL] = cacheEntry{ev: ev, err: err, expires: now.Add(ttl)}
		default:
			delete(c.entries, sourceURL)
		}
		delete(c.inflight, sourceURL)
	}
	c.mu.Unlock()
	close(f.done)
}

// Invalidate drops the evidence kept for sourceURL, so the next lookup asks
// the provider again (a change notice uses it). A fetch already running
// still answers the callers waiting on it, but later lookups do not join it
// and its result is not kept. It reports whether anything was dropped.
func (c *Cache) Invalidate(sourceURL string) bool {
	c.mu.Lock()
	_, had := c.entries[sourceURL]
	delete(c.entries, sourceURL)
	if _, running := c.inflight[sourceURL]; running {
		delete(c.inflight, sourceURL)
		had = true
	}
	c.mu.Unlock()
	if s := c.cfg.Schedule; s != nil {
		safeCall(func() { s.Invalidated(sourceURL) })
	}
	return had
}

// safeFetch calls the provider and turns a panic into an error, so a broken
// provider can never take the proxy down.
func (c *Cache) safeFetch(ctx context.Context, sourceURL string) (ev Evidence, err error) {
	defer func() {
		if r := recover(); r != nil {
			ev, err = Evidence{}, fmt.Errorf("evidence provider %s panicked", c.provider.Name())
		}
	}()
	ev, err = c.provider.Fetch(ctx, sourceURL)
	if err != nil {
		return Evidence{}, err
	}
	if ev.Provider == "" {
		ev.Provider = c.provider.Name()
	}
	if ev.FetchedAt.IsZero() {
		ev.FetchedAt = c.cfg.Now()
	}
	if ev.URL == "" {
		ev.URL = sourceURL
	}
	return ev, nil
}

// safeCall runs a schedule callback and swallows a panic: the schedule is an
// optimisation and must never break an evidence lookup.
func safeCall(fn func()) {
	defer func() { _ = recover() }()
	fn()
}
