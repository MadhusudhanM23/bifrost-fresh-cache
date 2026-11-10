package evidence

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"
)

const testKey = "tvly-test-secret-key"

// recordedCall is what the fake Tavily server saw.
type recordedCall struct {
	method, path, rawQuery, auth, contentType string
	body                                      map[string]any
}

// fakeTavily mimics api.tavily.com: it records the call and answers with the
// given status and body.
func fakeTavily(t *testing.T, status int, respBody string, extraHeaders map[string]string) (*httptest.Server, *recordedCall) {
	t.Helper()
	got := &recordedCall{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path, got.rawQuery = r.Method, r.URL.Path, r.URL.RawQuery
		got.auth, got.contentType = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		data, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(data, &got.body); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		for k, v := range extraHeaders {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func newExtract(t *testing.T, baseURL string) *TavilyExtract {
	t.Helper()
	p, err := NewTavilyExtract(TavilyConfig{APIKey: testKey, BaseURL: baseURL, ServerTimeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTavilyExtractRequestAndResponseShape(t *testing.T) {
	// 200 body in the documented shape (results, failed_results, response_time, request_id).
	srv, got := fakeTavily(t, http.StatusOK, `{
		"results": [{"url": "https://example.github.io/pricing/", "raw_content": "Acme pricing Pro plan Acme Pro plan — $49 per month", "images": []}],
		"failed_results": [],
		"response_time": 0.5,
		"request_id": "123e4567-e89b-12d3-a456-426614174111"
	}`, nil)
	ev, err := newExtract(t, srv.URL).Fetch(context.Background(), "https://example.github.io/pricing/")
	if err != nil {
		t.Fatal(err)
	}
	if got.method != http.MethodPost || got.path != "/extract" || got.rawQuery != "" {
		t.Fatalf("request line = %s %s?%s, want POST /extract with no query", got.method, got.path, got.rawQuery)
	}
	if got.auth != "Bearer "+testKey {
		t.Fatalf("Authorization = %q, want the bearer key", got.auth)
	}
	if !strings.HasPrefix(got.contentType, "application/json") {
		t.Fatalf("Content-Type = %q", got.contentType)
	}
	// Exactly these fields are sent: the source URL and extraction options.
	// No prompt, no answer, no "query".
	if k := strings.Join(keys(got.body), ","); k != "extract_depth,format,timeout,urls" {
		t.Fatalf("request fields = %s", k)
	}
	urls, _ := got.body["urls"].([]any)
	if len(urls) != 1 || urls[0] != "https://example.github.io/pricing/" {
		t.Fatalf("urls = %v", got.body["urls"])
	}
	if got.body["extract_depth"] != "basic" || got.body["format"] != "text" || got.body["timeout"] != 10.0 {
		t.Fatalf("options = %v", got.body)
	}
	if ev.Provider != TavilyExtractName || !strings.Contains(ev.Content, "$49 per month") || ev.FetchedAt.IsZero() {
		t.Fatalf("evidence = %+v", ev)
	}
}

func TestTavilyExtractPartialSuccess(t *testing.T) {
	// The documented partial-success example: one result and one failed result.
	body := `{
		"results": [{"url": "https://example.com/article", "raw_content": "Example extracted article content."}],
		"failed_results": [{"url": "https://example.com/unavailable", "error": "Failed to retrieve content"}],
		"response_time": 0.5,
		"request_id": "123e4567-e89b-12d3-a456-426614174111"
	}`
	srv, _ := fakeTavily(t, http.StatusOK, body, nil)
	p := newExtract(t, srv.URL)
	ev, err := p.Fetch(context.Background(), "https://example.com/article")
	if err != nil || ev.Content != "Example extracted article content." {
		t.Fatalf("matching result: %+v, %v", ev, err)
	}
	_, err = p.Fetch(context.Background(), "https://example.com/unavailable")
	if err == nil || !strings.Contains(err.Error(), "Failed to retrieve content") {
		t.Fatalf("failed result: got %v", err)
	}
}

func TestTavilyExtractErrorResponses(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		headers map[string]string
		want    string
	}{
		{"unauthorized", 401, `{"detail": {"error": "Unauthorized: missing or invalid API key."}}`, nil, "Unauthorized: missing or invalid API key."},
		{"rate limited", 429, `{"detail": {"error": "Your request has been blocked due to excessive requests. Please reduce the rate of requests."}}`, map[string]string{"Retry-After": "7"}, "Retry-After: 7"},
		{"all urls invalid", 400, `{"detail": {"error": "All URLs failed validation.", "failed_results": [{"url": "not-a-url", "error": "Validation Error: Invalid URL format"}]}}`, nil, "All URLs failed validation."},
		{"plan limit", 432, `{"detail": {"error": "This request exceeds your plan's set usage limit."}}`, nil, "HTTP 432"},
		{"server error", 500, `{"detail": {"error": "Internal Server Error"}}`, nil, "Internal Server Error"},
		{"unrecognised body", 502, `<html>bad gateway</html>`, nil, "unrecognised error body"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, _ := fakeTavily(t, c.status, c.body, c.headers)
			_, err := newExtract(t, srv.URL).Fetch(context.Background(), "https://example.com/pricing")
			var te *TavilyError
			if !errors.As(err, &te) || te.Status != c.status {
				t.Fatalf("got %v, want a TavilyError with status %d", err, c.status)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not mention %q", err, c.want)
			}
			if strings.Contains(err.Error(), testKey) {
				t.Fatal("error text contains the API key")
			}
		})
	}
}

func TestTavilyNeverLeaksKey(t *testing.T) {
	// A misbehaving server that echoes the Authorization header back.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"detail": {"error": "bad key `+r.Header.Get("Authorization")+`"}}`)
	}))
	defer srv.Close()
	_, err := newExtract(t, srv.URL).Fetch(context.Background(), "https://example.com/")
	if err == nil || strings.Contains(err.Error(), testKey) || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("key not redacted: %v", err)
	}
	if _, err := NewTavilyExtract(TavilyConfig{APIKey: "  "}); err == nil {
		t.Fatal("an empty key must be rejected")
	}
}

func TestTavilyExtractTimeoutIsAnError(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain the body so the server notices the client hanging up, then
		// never answer.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release) // runs first, so Close never waits on the handler
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := newExtract(t, srv.URL).Fetch(ctx, "https://example.com/"); err == nil {
		t.Fatal("want a timeout error")
	}
}

func TestTavilySearchRequestAndResponseShape(t *testing.T) {
	srv, got := fakeTavily(t, http.StatusOK, `{
		"query": "https://example.github.io/pricing/",
		"answer": null,
		"images": [],
		"results": [
			{"title": "Other", "url": "https://example.github.io/blog/", "content": "blog", "score": 0.4, "raw_content": "blog post"},
			{"title": "Acme pricing", "url": "https://example.github.io/pricing/", "content": "snippet", "score": 0.81025416, "raw_content": "Acme Pro plan — $49 per month"}
		],
		"response_time": 1.67,
		"usage": {"credits": 1},
		"request_id": "123e4567-e89b-12d3-a456-426614174111"
	}`, nil)
	p, err := NewTavilySearch(TavilyConfig{APIKey: testKey, BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	ev, err := p.Fetch(context.Background(), "https://example.github.io/pricing/")
	if err != nil {
		t.Fatal(err)
	}
	if got.method != http.MethodPost || got.path != "/search" || got.auth != "Bearer "+testKey {
		t.Fatalf("request = %s %s auth=%q", got.method, got.path, got.auth)
	}
	if k := strings.Join(keys(got.body), ","); k != "include_domains,include_raw_content,max_results,query,search_depth" {
		t.Fatalf("request fields = %s", k)
	}
	if got.body["query"] != "https://example.github.io/pricing/" || got.body["include_raw_content"] != "text" {
		t.Fatalf("body = %v", got.body)
	}
	domains, _ := got.body["include_domains"].([]any)
	if len(domains) != 1 || domains[0] != "example.github.io" {
		t.Fatalf("include_domains = %v", got.body["include_domains"])
	}
	if ev.Provider != TavilySearchName || ev.Content != "Acme Pro plan — $49 per month" {
		t.Fatalf("evidence = %+v", ev)
	}
}

func TestTavilySearchValidationErrorList(t *testing.T) {
	srv, _ := fakeTavily(t, 422, `{"detail": [{"type": "missing", "loc": ["body", "query"], "msg": "Field required", "input": {}}]}`, nil)
	p, err := NewTavilySearch(TavilyConfig{APIKey: testKey, BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Fetch(context.Background(), "https://example.com/")
	if err == nil || !strings.Contains(err.Error(), "body.query: Field required") {
		t.Fatalf("got %v", err)
	}
}
