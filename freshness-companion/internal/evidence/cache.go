package evidence

import (
	"context"
	"fmt"
	"sync"
	"time"
)

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
}

// Result is evidence plus whether it came from the cache.
type Result struct {
	Evidence Evidence
	// Cached is true when the evidence was reused from an earlier fetch;
	// Evidence.FetchedAt then says how old it is.
	Cached bool
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

// Get returns evidence for sourceURL, from the cache when it is fresh enough.
// The provider call itself is detached from ctx (it is shared with other
// callers and bounded by FetchTimeout); ctx only bounds how long this caller
// waits.
func (c *Cache) Get(ctx context.Context, sourceURL string) (Result, error) {
	c.mu.Lock()
	if e, ok := c.entries[sourceURL]; ok && c.cfg.Now().Before(e.expires) {
		c.mu.Unlock()
		return Result{Evidence: e.ev, Cached: true}, e.err
	}
	f, running := c.inflight[sourceURL]
	if !running {
		f = &flight{done: make(chan struct{})}
		c.inflight[sourceURL] = f
	}
	c.mu.Unlock()
	if !running {
		go c.fetch(sourceURL, f)
	}
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
	ev, err := c.safeFetch(ctx, sourceURL)
	f.ev, f.err = ev, err
	ttl := c.cfg.TTL
	if err != nil {
		ttl = c.cfg.ErrorTTL
	}
	c.mu.Lock()
	// After Invalidate this flight is no longer the current one: its result
	// still answers its own waiters, but it must not be kept.
	if c.inflight[sourceURL] == f {
		if ttl > 0 {
			c.entries[sourceURL] = cacheEntry{ev: ev, err: err, expires: c.cfg.Now().Add(ttl)}
		} else {
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
	defer c.mu.Unlock()
	_, had := c.entries[sourceURL]
	delete(c.entries, sourceURL)
	if _, running := c.inflight[sourceURL]; running {
		delete(c.inflight, sourceURL)
		had = true
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
