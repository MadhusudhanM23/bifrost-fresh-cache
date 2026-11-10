package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// flights lets requests that need the same work share one run of it. When a popular
// answer goes stale, every request that arrives together would otherwise
// delete the entry and call the model again, and they trample each other
// (a second request deletes the fresh entry the first one just wrote).
//
// A call stays shared for `window` after it finishes, so a request that read
// the stale entry just before the refresh finished is still served the fresh
// answer instead of starting another refresh. Failed calls are not kept.
type flights[T any] struct {
	window time.Duration
	now    func() time.Time
	// keep reports whether a finished result is worth sharing with later
	// callers inside the window (failed results are not).
	keep func(T) bool

	mu    sync.Mutex
	calls map[string]*flight[T]
}

type flight[T any] struct {
	done     chan struct{}
	val      T
	finished time.Time
}

func newFlights[T any](window time.Duration, now func() time.Time, keep func(T) bool) *flights[T] {
	return &flights[T]{window: window, now: now, keep: keep, calls: map[string]*flight[T]{}}
}

// do runs fn once per key. The caller that runs it gets shared=false; callers
// that wait for it, or reuse its recent result, get shared=true. A waiting
// caller gives up when ctx ends.
func (f *flights[T]) do(ctx context.Context, key string, fn func() T) (out T, shared bool, err error) {
	f.mu.Lock()
	f.sweepLocked()
	if c, ok := f.calls[key]; ok {
		f.mu.Unlock()
		select {
		case <-c.done:
			return c.val, true, nil
		case <-ctx.Done():
			var zero T
			return zero, true, ctx.Err()
		}
	}
	c := &flight[T]{done: make(chan struct{})}
	f.calls[key] = c
	f.mu.Unlock()

	c.val = fn()
	f.mu.Lock()
	c.finished = f.now()
	if f.window <= 0 || (f.keep != nil && !f.keep(c.val)) {
		delete(f.calls, key) // nothing worth sharing afterwards
	}
	f.mu.Unlock()
	close(c.done)
	return c.val, false, nil
}

// sweepLocked drops finished calls older than the window.
func (f *flights[T]) sweepLocked() {
	cutoff := f.now().Add(-f.window)
	for k, c := range f.calls {
		select {
		case <-c.done:
			if c.finished.Before(cutoff) {
				delete(f.calls, k)
			}
		default:
		}
	}
}

// askKey identifies "the same ask": the same cache entry, asked the same way
// by the same caller. The headers are part of it because they carry who is
// asking (virtual key, user, cache key): one caller's result is never handed
// to another.
func askKey(req *Request, cacheID string) string {
	h := sha256.New()
	h.Write([]byte(cacheID))
	h.Write([]byte{0})
	h.Write(req.Body)
	h.Write([]byte{0})
	names := make([]string, 0, len(req.Header))
	for name := range req.Header {
		if !volatileHeader(name) {
			names = append(names, http.CanonicalHeaderKey(name))
		}
	}
	sort.Strings(names)
	for _, name := range names {
		h.Write([]byte(name))
		h.Write([]byte{0})
		h.Write([]byte(strings.Join(req.Header.Values(name), "\x01")))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// volatileHeader reports headers that differ between otherwise identical
// requests and say nothing about who is asking.
func volatileHeader(name string) bool {
	switch strings.ToLower(name) {
	case "content-length", "accept-encoding", "connection", "user-agent",
		"x-request-id", "traceparent", "tracestate", "x-forwarded-for", "x-real-ip":
		return true
	}
	return false
}
