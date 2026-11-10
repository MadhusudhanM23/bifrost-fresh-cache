package evidence

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Request and response shapes follow Tavily's API reference:
//   - Extract: https://docs.tavily.com/documentation/api-reference/endpoint/extract
//   - Search:  https://docs.tavily.com/documentation/api-reference/endpoint/search
//
// Both are POST requests with a JSON body and "Authorization: Bearer <key>".
// Error bodies are {"detail": {"error": "..."}}; Search's 422 uses a list,
// {"detail": [{"loc": [...], "msg": "..."}]}.

// DefaultTavilyBaseURL is Tavily's API root.
const DefaultTavilyBaseURL = "https://api.tavily.com"

// Provider names as they appear in headers and logs.
const (
	TavilyExtractName = "tavily_extract"
	TavilySearchName  = "tavily_search"
)

const defaultTavilyMaxBody = 4 << 20

// TavilyConfig configures the Tavily providers.
type TavilyConfig struct {
	// APIKey is sent only as "Authorization: Bearer <key>". It is never put
	// in a URL, and it is redacted from any error text.
	APIKey string
	// BaseURL defaults to https://api.tavily.com (tests use an httptest server).
	BaseURL string
	// ExtractDepth is "basic" (default) or "advanced" (Extract only).
	ExtractDepth string
	// Format is "text" (default) or "markdown". Plain text keeps patterns
	// simple; Tavily notes that "text" may add latency.
	Format string
	// ServerTimeout is sent as Extract's "timeout" parameter, clamped to the
	// documented 1-60 s. Zero leaves Tavily's default (10 s basic, 30 s advanced).
	ServerTimeout time.Duration
	// MaxBodyBytes bounds the response size (default 4 MiB).
	MaxBodyBytes int64
	// HTTPClient is optional. The caller's context bounds every call.
	HTTPClient *http.Client
}

type tavilyClient struct {
	cfg    TavilyConfig
	base   string
	client *http.Client
}

func newTavilyClient(cfg TavilyConfig) (*tavilyClient, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("tavily: API key is empty (set TAVILY_API_KEY)")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultTavilyBaseURL
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("tavily: invalid base URL %q", cfg.BaseURL)
	}
	if cfg.ExtractDepth == "" {
		cfg.ExtractDepth = "basic"
	}
	if cfg.ExtractDepth != "basic" && cfg.ExtractDepth != "advanced" {
		return nil, fmt.Errorf("tavily: extract depth %q: want basic or advanced", cfg.ExtractDepth)
	}
	if cfg.Format == "" {
		cfg.Format = "text"
	}
	if cfg.Format != "text" && cfg.Format != "markdown" {
		return nil, fmt.Errorf("tavily: format %q: want text or markdown", cfg.Format)
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = defaultTavilyMaxBody
	}
	client := cfg.HTTPClient
	if client == nil {
		// Backstop only; the evidence cache gives every call a shorter deadline.
		client = &http.Client{Timeout: 70 * time.Second}
	}
	return &tavilyClient{cfg: cfg, base: strings.TrimRight(u.String(), "/"), client: client}, nil
}

func (c *tavilyClient) serverTimeoutSeconds() float64 {
	if c.cfg.ServerTimeout <= 0 {
		return 0
	}
	return math.Min(60, math.Max(1, c.cfg.ServerTimeout.Seconds()))
}

// TavilyError is a non-200 answer from Tavily.
type TavilyError struct {
	Endpoint   string
	Status     int
	Message    string
	RetryAfter string
}

func (e *TavilyError) Error() string {
	s := fmt.Sprintf("tavily %s: HTTP %d: %s", e.Endpoint, e.Status, e.Message)
	if e.RetryAfter != "" {
		s += " (Retry-After: " + e.RetryAfter + ")"
	}
	return s
}

func (c *tavilyClient) post(ctx context.Context, endpoint string, payload, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("tavily %s: encoding request: %w", endpoint, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/"+endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("tavily %s: %w", endpoint, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return c.scrub(fmt.Errorf("tavily %s: %w", endpoint, err))
	}
	defer resp.Body.Close()
	data, complete, err := readLimited(resp.Body, c.cfg.MaxBodyBytes)
	if err != nil {
		return c.scrub(fmt.Errorf("tavily %s: reading response: %w", endpoint, err))
	}
	if resp.StatusCode != http.StatusOK {
		return &TavilyError{
			Endpoint:   endpoint,
			Status:     resp.StatusCode,
			Message:    c.redact(tavilyErrorMessage(data)),
			RetryAfter: c.redact(resp.Header.Get("Retry-After")),
		}
	}
	if !complete {
		return fmt.Errorf("tavily %s: response larger than %d bytes", endpoint, c.cfg.MaxBodyBytes)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("tavily %s: response is not the documented JSON shape", endpoint)
	}
	return nil
}

func (c *tavilyClient) redact(s string) string {
	return strings.ReplaceAll(s, c.cfg.APIKey, "[redacted]")
}

func (c *tavilyClient) scrub(err error) error {
	msg := err.Error()
	if red := c.redact(msg); red != msg {
		return errors.New(red)
	}
	return err
}

// tavilyErrorMessage decodes {"detail": {"error": "..."}}, the 422 list
// form {"detail": [{"msg": "..."}]}, or a plain {"detail": "..."}. It never
// echoes an unrecognised body.
func tavilyErrorMessage(data []byte) string {
	var envelope struct {
		Detail json.RawMessage `json:"detail"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil || len(envelope.Detail) == 0 {
		return "unrecognised error body"
	}
	var obj struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(envelope.Detail, &obj); err == nil && obj.Error != "" {
		return truncateText(obj.Error, 300)
	}
	var list []struct {
		Loc []any  `json:"loc"`
		Msg string `json:"msg"`
	}
	if err := json.Unmarshal(envelope.Detail, &list); err == nil && len(list) > 0 {
		msgs := make([]string, 0, len(list))
		for _, item := range list {
			loc := make([]string, 0, len(item.Loc))
			for _, l := range item.Loc {
				loc = append(loc, fmt.Sprint(l))
			}
			msgs = append(msgs, strings.Join(loc, ".")+": "+item.Msg)
		}
		return truncateText(strings.Join(msgs, "; "), 300)
	}
	var s string
	if err := json.Unmarshal(envelope.Detail, &s); err == nil && s != "" {
		return truncateText(s, 300)
	}
	return "unrecognised error body"
}

func truncateText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) > n {
		r = r[:n]
	}
	return string(r) + "..."
}

// tavilyExtractRequest is the POST /extract body the companion sends. Only
// these documented fields are used. The optional "query" (chunk reranking)
// is deliberately never sent: it would carry user intent to Tavily.
type tavilyExtractRequest struct {
	URLs         []string `json:"urls"`
	ExtractDepth string   `json:"extract_depth,omitempty"`
	Format       string   `json:"format,omitempty"`
	Timeout      float64  `json:"timeout,omitempty"`
}

// tavilyExtractResponse is the documented 200 body. Tavily says to check
// both results and failed_results even on HTTP 200.
type tavilyExtractResponse struct {
	Results []struct {
		URL        string `json:"url"`
		RawContent string `json:"raw_content"`
	} `json:"results"`
	FailedResults []struct {
		URL   string `json:"url"`
		Error string `json:"error"`
	} `json:"failed_results"`
	ResponseTime json.Number `json:"response_time"`
	RequestID    string      `json:"request_id"`
}

// TavilyExtract fetches a source page through Tavily's Extract API.
type TavilyExtract struct {
	c *tavilyClient
}

// NewTavilyExtract returns an Extract provider. It fails when the key is empty.
func NewTavilyExtract(cfg TavilyConfig) (*TavilyExtract, error) {
	c, err := newTavilyClient(cfg)
	if err != nil {
		return nil, err
	}
	return &TavilyExtract{c: c}, nil
}

// Name implements Provider.
func (t *TavilyExtract) Name() string { return TavilyExtractName }

// Fetch implements Provider: POST /extract with {"urls": [sourceURL], ...}.
func (t *TavilyExtract) Fetch(ctx context.Context, sourceURL string) (Evidence, error) {
	payload := tavilyExtractRequest{
		URLs:         []string{sourceURL},
		ExtractDepth: t.c.cfg.ExtractDepth,
		Format:       t.c.cfg.Format,
		Timeout:      t.c.serverTimeoutSeconds(),
	}
	var resp tavilyExtractResponse
	if err := t.c.post(ctx, "extract", payload, &resp); err != nil {
		return Evidence{}, err
	}
	content, found := "", false
	for _, r := range resp.Results {
		if sameURL(r.URL, sourceURL) {
			content, found = r.RawContent, true
			break
		}
	}
	if !found && len(resp.Results) == 1 && len(resp.FailedResults) == 0 {
		// One URL was requested; Tavily may report it normalised or redirected.
		content, found = resp.Results[0].RawContent, true
	}
	if !found {
		for _, f := range resp.FailedResults {
			if sameURL(f.URL, sourceURL) || len(resp.FailedResults) == 1 {
				return Evidence{}, fmt.Errorf("tavily extract: source URL not extracted: %s", t.c.redact(truncateText(f.Error, 300)))
			}
		}
		return Evidence{}, errors.New("tavily extract: no result for the source URL")
	}
	if strings.TrimSpace(content) == "" {
		return Evidence{}, errors.New("tavily extract: empty raw_content for the source URL")
	}
	return Evidence{URL: sourceURL, Content: content, Provider: TavilyExtractName, FetchedAt: time.Now()}, nil
}

// tavilySearchRequest is the POST /search body the companion sends. The
// query is the source URL itself and results are restricted to its host, so
// nothing but the configured URL leaves the companion.
type tavilySearchRequest struct {
	Query             string   `json:"query"`
	SearchDepth       string   `json:"search_depth,omitempty"`
	MaxResults        int      `json:"max_results,omitempty"`
	IncludeDomains    []string `json:"include_domains,omitempty"`
	IncludeRawContent string   `json:"include_raw_content,omitempty"`
}

// tavilySearchResponse is the part of the documented 200 body the companion reads.
type tavilySearchResponse struct {
	Query   string `json:"query"`
	Results []struct {
		Title      string      `json:"title"`
		URL        string      `json:"url"`
		Content    string      `json:"content"`
		Score      json.Number `json:"score"`
		RawContent *string     `json:"raw_content"`
	} `json:"results"`
	ResponseTime json.Number `json:"response_time"`
	RequestID    string      `json:"request_id"`
}

// TavilySearch is an experimental alternative to Extract: it searches for
// the source URL within the source's domain and uses the matching result's
// raw_content. Extract is the better fit for a known URL.
type TavilySearch struct {
	c *tavilyClient
}

// NewTavilySearch returns a Search provider. It fails when the key is empty.
func NewTavilySearch(cfg TavilyConfig) (*TavilySearch, error) {
	c, err := newTavilyClient(cfg)
	if err != nil {
		return nil, err
	}
	return &TavilySearch{c: c}, nil
}

// Name implements Provider.
func (t *TavilySearch) Name() string { return TavilySearchName }

// Fetch implements Provider.
func (t *TavilySearch) Fetch(ctx context.Context, sourceURL string) (Evidence, error) {
	u, err := url.Parse(sourceURL)
	if err != nil || u.Hostname() == "" {
		return Evidence{}, errors.New("tavily search: invalid source URL")
	}
	payload := tavilySearchRequest{
		Query:             sourceURL,
		SearchDepth:       "basic",
		MaxResults:        5,
		IncludeDomains:    []string{u.Hostname()},
		IncludeRawContent: t.c.cfg.Format,
	}
	var resp tavilySearchResponse
	if err := t.c.post(ctx, "search", payload, &resp); err != nil {
		return Evidence{}, err
	}
	for _, r := range resp.Results {
		if !sameURL(r.URL, sourceURL) {
			continue
		}
		content := ""
		if r.RawContent != nil {
			content = *r.RawContent
		}
		if strings.TrimSpace(content) == "" {
			content = r.Content
		}
		if strings.TrimSpace(content) == "" {
			return Evidence{}, errors.New("tavily search: matching result has no content")
		}
		return Evidence{URL: sourceURL, Content: content, Provider: TavilySearchName, FetchedAt: time.Now()}, nil
	}
	return Evidence{}, fmt.Errorf("tavily search: source URL not among %d results", len(resp.Results))
}
