// Package evidence fetches the current content of a source URL named in the
// policy, so a cached answer can be compared with it.
//
// Privacy rule: a provider receives only the configured source URL (and, for
// Tavily Search, that URL's host as a domain filter). Prompts, cached answers
// and the caller's credentials never reach a provider.
package evidence

import (
	"context"
	"io"
	"net/url"
	"strings"
	"time"
)

// Evidence is the content of a source page at a point in time.
type Evidence struct {
	// URL is the source URL that was requested.
	URL string
	// Content is the page text (or markdown / raw HTML, depending on the provider).
	Content string
	// Provider names who fetched it, e.g. "tavily_extract" or "direct_fetch (not Tavily)".
	Provider string
	// FetchedAt is when the provider returned the content.
	FetchedAt time.Time
}

// Provider fetches evidence for one source URL.
type Provider interface {
	Name() string
	Fetch(ctx context.Context, sourceURL string) (Evidence, error)
}

// readLimited reads at most limit bytes; complete is false when the body is longer.
func readLimited(r io.Reader, limit int64) (data []byte, complete bool, err error) {
	data, err = io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > limit {
		return data[:limit], false, nil
	}
	return data, true, nil
}

// sameURL compares two URLs ignoring scheme and host case, a trailing slash
// and the fragment.
func sameURL(a, b string) bool {
	ua, errA := url.Parse(strings.TrimSpace(a))
	ub, errB := url.Parse(strings.TrimSpace(b))
	if errA != nil || errB != nil {
		return strings.TrimSpace(a) == strings.TrimSpace(b)
	}
	norm := func(u *url.URL) string {
		return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) +
			strings.TrimRight(u.EscapedPath(), "/") + "?" + u.RawQuery
	}
	return norm(ua) == norm(ub)
}
