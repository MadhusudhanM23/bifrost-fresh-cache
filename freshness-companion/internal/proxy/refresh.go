package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Sender sends a buffered request to Bifrost (normally *Upstream).
type Sender interface {
	Send(ctx context.Context, req *Request) (*Response, error)
}

// RefreshStrategy obtains a fresh (not cached) answer for a request whose
// cached answer must not be served. Implementations must never return a
// response that Bifrost served from its cache.
type RefreshStrategy interface {
	Name() string
	Refresh(ctx context.Context, req *Request, staleCacheID string) (*RefreshResult, error)
}

// RefreshResult is the fresh response and how it was obtained.
type RefreshResult struct {
	Response *Response
	// SentHeader is the header set of the final re-request (after edits).
	SentHeader http.Header
	// Cleared lists the cache ids the strategy deleted.
	Cleared []string
	// ServedFromCache is true when the re-request came back from cache under
	// the id that was just cleared. Another request may have written a fresh
	// entry in between, or the clear may have failed and the stale entry is
	// still there: the caller must check the answer against the source before
	// it trusts it. ClearErrors lists any clear errors.
	ServedFromCache bool
	ClearErrors     string
}

// HeaderEdits changes headers on the re-request, e.g. to try a header-based
// bypass. Drop runs before Set.
type HeaderEdits struct {
	Set  map[string]string
	Drop []string
}

// Empty reports whether there is nothing to change.
func (e HeaderEdits) Empty() bool { return len(e.Set) == 0 && len(e.Drop) == 0 }

// Apply returns a copy of h with the edits applied.
func (e HeaderEdits) Apply(h http.Header) http.Header {
	out := h.Clone()
	if out == nil {
		out = http.Header{}
	}
	for _, name := range e.Drop {
		out.Del(name)
	}
	for name, value := range e.Set {
		out.Set(name, value)
	}
	return out
}

// CacheClearer deletes one Bifrost cache entry.
type CacheClearer interface {
	Clear(ctx context.Context, cacheID string) error
}

// KeyClearer deletes every Bifrost cache entry under one cache key.
type KeyClearer interface {
	ClearKey(ctx context.Context, cacheKey string) error
}

// ClearAndResend is the default strategy: delete the stale entry through
// Bifrost's documented DELETE /api/cache/clear/{cacheId}, then re-send the
// original request. It misses, Bifrost calls the model and writes a fresh
// entry. If the re-sent request is answered from cache again by a different
// (semantically similar) entry, that entry is cleared too, up to MaxAttempts.
// A clear error alone is not fatal: what decides is whether the re-sent
// request was a miss.
type ClearAndResend struct {
	Clearer     CacheClearer
	Sender      Sender
	MaxAttempts int
	Headers     HeaderEdits
}

// Name implements RefreshStrategy.
func (c *ClearAndResend) Name() string { return "clear_and_resend" }

// Refresh implements RefreshStrategy.
func (c *ClearAndResend) Refresh(ctx context.Context, req *Request, staleCacheID string) (*RefreshResult, error) {
	attempts := max(1, c.MaxAttempts)
	out := req.Clone()
	out.Header = c.Headers.Apply(out.Header)
	res := &RefreshResult{SentHeader: out.Header}
	id := staleCacheID
	var clearErrs []string
	for i := 0; i < attempts; i++ {
		if id != "" {
			if err := c.Clearer.Clear(ctx, id); err != nil {
				clearErrs = append(clearErrs, err.Error())
			} else {
				res.Cleared = append(res.Cleared, id)
			}
		}
		resp, err := c.Sender.Send(ctx, out)
		if err != nil {
			return nil, fmt.Errorf("re-request failed: %w", err)
		}
		if !resp.CacheHit() {
			res.Response = resp
			return res, nil
		}
		next := resp.CacheID()
		if next == "" {
			return nil, fmt.Errorf("re-request was served from cache again (entry %q)%s", id, joinErrs(clearErrs))
		}
		if next == id {
			res.Response, res.ServedFromCache, res.ClearErrors = resp, true, joinErrs(clearErrs)
			return res, nil
		}
		id = next
	}
	return nil, fmt.Errorf("re-request still served from cache after %d attempts%s", attempts, joinErrs(clearErrs))
}

func joinErrs(errs []string) string {
	if len(errs) == 0 {
		return ""
	}
	return "; clear errors: " + strings.Join(errs, "; ")
}

// ResendWithHeaders is the alternative strategy: re-send the request with
// edited headers that should force Bifrost to skip its cache (for example
// dropping x-bf-cache-key, which disables caching when no default key is
// configured). Nothing is cleared, so the stale entry stays in Bifrost and is
// checked again on its next hit.
type ResendWithHeaders struct {
	Sender  Sender
	Headers HeaderEdits
}

// Name implements RefreshStrategy.
func (r *ResendWithHeaders) Name() string { return "resend_with_headers" }

// Refresh implements RefreshStrategy.
func (r *ResendWithHeaders) Refresh(ctx context.Context, req *Request, _ string) (*RefreshResult, error) {
	if r.Headers.Empty() {
		return nil, errors.New("resend_with_headers has no header edits configured")
	}
	out := req.Clone()
	out.Header = r.Headers.Apply(out.Header)
	resp, err := r.Sender.Send(ctx, out)
	if err != nil {
		return nil, fmt.Errorf("re-request failed: %w", err)
	}
	if resp.CacheHit() {
		return nil, fmt.Errorf("re-request with bypass headers was still served from cache (entry %q)", resp.CacheID())
	}
	return &RefreshResult{Response: resp, SentHeader: out.Header}, nil
}

// BifrostClearer calls DELETE {bifrost}/api/cache/clear/{cacheId}.
type BifrostClearer struct {
	base       string
	client     *http.Client
	authHeader string
	authValue  string
}

// NewBifrostClearer returns a clearer. authValue (e.g. "Bearer <api key>" or
// "Basic <base64>") is sent in authHeader when non-empty; it is never logged.
func NewBifrostClearer(baseURL, authHeader, authValue string, timeout time.Duration) (*BifrostClearer, error) {
	base, err := parseBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	if authHeader == "" {
		authHeader = "Authorization"
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &BifrostClearer{
		base:       base,
		client:     &http.Client{Timeout: timeout, Transport: NewTransport(timeout)},
		authHeader: authHeader,
		authValue:  authValue,
	}, nil
}

// Clear implements CacheClearer.
func (b *BifrostClearer) Clear(ctx context.Context, cacheID string) error {
	if cacheID == "" {
		return errors.New("no cache id to clear")
	}
	// Bifrost cache ids are UUIDs; escaping keeps any other id inside one path segment.
	return b.delete(ctx, "/api/cache/clear/"+url.PathEscape(cacheID), cacheID)
}

// ClearKey implements KeyClearer with DELETE {bifrost}/api/cache/clear-by-key/{cacheKey}.
func (b *BifrostClearer) ClearKey(ctx context.Context, cacheKey string) error {
	if cacheKey == "" {
		return errors.New("no cache key to clear")
	}
	return b.delete(ctx, "/api/cache/clear-by-key/"+url.PathEscape(cacheKey), "cache key "+cacheKey)
}

func (b *BifrostClearer) delete(ctx context.Context, path, what string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, b.base+path, nil)
	if err != nil {
		return err
	}
	if b.authValue != "" {
		req.Header.Set(b.authHeader, b.authValue)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("clear %s: %w", what, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("clear %s: Bifrost answered HTTP %d", what, resp.StatusCode)
	}
	return nil
}
