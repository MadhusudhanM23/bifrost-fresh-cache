package evidence

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// DirectFetchName labels DirectFetch everywhere (headers, logs, /healthz),
// so nobody mistakes a plain GET for Tavily.
const DirectFetchName = "direct_fetch (not Tavily)"

const (
	defaultDirectTimeout = 5 * time.Second
	defaultDirectMaxBody = 2 << 20
)

// DirectConfig configures DirectFetch.
type DirectConfig struct {
	// Timeout bounds the whole GET (default 5 s).
	Timeout time.Duration
	// MaxBodyBytes bounds the page size (default 2 MiB); bigger pages fail.
	MaxBodyBytes int64
	// Raw matches patterns against the raw HTML. By default HTML is reduced
	// to visible text first, which is closer to what Tavily returns.
	Raw bool
	// HTTPClient is optional.
	HTTPClient *http.Client
}

// DirectFetch is a plain HTTP GET of the source URL, for local or offline
// demos. It is not Tavily and is labelled as such.
type DirectFetch struct {
	client  *http.Client
	maxBody int64
	raw     bool
}

// NewDirectFetch returns a DirectFetch provider.
func NewDirectFetch(cfg DirectConfig) *DirectFetch {
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultDirectTimeout
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = defaultDirectMaxBody
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: cfg.Timeout}
	}
	return &DirectFetch{client: client, maxBody: cfg.MaxBodyBytes, raw: cfg.Raw}
}

// Name implements Provider.
func (d *DirectFetch) Name() string { return DirectFetchName }

// Fetch implements Provider. The request carries no cookies or credentials.
func (d *DirectFetch) Fetch(ctx context.Context, sourceURL string) (Evidence, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return Evidence{}, fmt.Errorf("direct fetch: %w", err)
	}
	req.Header.Set("User-Agent", "freshness-companion/0.1 (direct fetch, not Tavily)")
	req.Header.Set("Accept", "text/html, text/plain;q=0.9, */*;q=0.5")
	resp, err := d.client.Do(req)
	if err != nil {
		return Evidence{}, fmt.Errorf("direct fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Evidence{}, fmt.Errorf("direct fetch: source returned HTTP %d", resp.StatusCode)
	}
	data, complete, err := readLimited(resp.Body, d.maxBody)
	if err != nil {
		return Evidence{}, fmt.Errorf("direct fetch: reading page: %w", err)
	}
	if !complete {
		return Evidence{}, fmt.Errorf("direct fetch: page larger than %d bytes", d.maxBody)
	}
	content := string(data)
	if !d.raw && looksLikeHTML(resp.Header.Get("Content-Type"), content) {
		content = HTMLToText(content)
	}
	if strings.TrimSpace(content) == "" {
		return Evidence{}, errors.New("direct fetch: page is empty")
	}
	return Evidence{URL: sourceURL, Content: content, Provider: DirectFetchName, FetchedAt: time.Now()}, nil
}

func looksLikeHTML(contentType, body string) bool {
	if strings.Contains(strings.ToLower(contentType), "html") {
		return true
	}
	return contentType == "" && strings.HasPrefix(strings.TrimSpace(body), "<")
}

var (
	reHTMLComment = regexp.MustCompile(`(?s)<!--.*?-->`)
	reHTMLScript  = regexp.MustCompile(`(?is)<script\b.*?</script\s*>`)
	reHTMLStyle   = regexp.MustCompile(`(?is)<style\b.*?</style\s*>`)
	reHTMLTag     = regexp.MustCompile(`(?s)<[^>]*>`)
)

// HTMLToText reduces an HTML page to whitespace-collapsed visible text. It
// is deliberately simple (standard library only): comments, scripts and
// styles are dropped, tags become spaces and entities are decoded.
func HTMLToText(s string) string {
	s = reHTMLComment.ReplaceAllString(s, " ")
	s = reHTMLScript.ReplaceAllString(s, " ")
	s = reHTMLStyle.ReplaceAllString(s, " ")
	s = reHTMLTag.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	return strings.Join(strings.Fields(s), " ")
}
