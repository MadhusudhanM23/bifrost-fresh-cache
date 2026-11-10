package evidence

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestTavilySearcherRequestShape(t *testing.T) {
	srv, got := fakeTavily(t, http.StatusOK, `{
		"query": "Acme Pro plan price per month",
		"answer": null,
		"results": [
			{"title": "Acme pricing", "url": "https://acme.example/pricing", "content": "Acme Pro plan — $49 per month", "score": 0.83},
			{"title": "Other", "url": "https://other.example/", "content": "other", "score": 0.2}
		],
		"response_time": 0.9, "request_id": "r1"
	}`, nil)
	s, err := NewTavilySearcher(TavilyConfig{APIKey: testKey, BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Search(context.Background(), SearchRequest{
		Query: "Acme Pro plan price per month", IncludeDomains: []string{"acme.example"}, ExcludeDomains: []string{"reddit.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.method != http.MethodPost || got.path != "/search" || got.auth != "Bearer "+testKey || got.rawQuery != "" {
		t.Fatalf("request = %s %s?%s auth=%q", got.method, got.path, got.rawQuery, got.auth)
	}
	// Exactly these fields, and nothing else, leave the companion.
	if k := strings.Join(keys(got.body), ","); k != "exclude_domains,include_answer,include_domains,include_raw_content,max_results,query,search_depth" {
		t.Fatalf("request fields = %s", k)
	}
	if got.body["query"] != "Acme Pro plan price per month" || got.body["search_depth"] != "basic" ||
		got.body["max_results"] != float64(5) || got.body["include_answer"] != false || got.body["include_raw_content"] != false {
		t.Fatalf("body = %v", got.body)
	}
	if inc, _ := got.body["include_domains"].([]any); len(inc) != 1 || inc[0] != "acme.example" {
		t.Fatalf("include_domains = %v", got.body["include_domains"])
	}
	if exc, _ := got.body["exclude_domains"].([]any); len(exc) != 1 || exc[0] != "reddit.com" {
		t.Fatalf("exclude_domains = %v", got.body["exclude_domains"])
	}
	if len(res) != 2 || res[0].Rank != 1 || res[1].Rank != 2 || res[0].Score != 0.83 || res[0].URL != "https://acme.example/pricing" || res[0].Title != "Acme pricing" {
		t.Fatalf("results = %+v", res)
	}
	// Empty domain lists are sent as [] (never null), and an empty query is refused locally.
	if _, err := s.Search(context.Background(), SearchRequest{Query: "x"}); err != nil {
		t.Fatal(err)
	}
	if inc, ok := got.body["include_domains"].([]any); !ok || len(inc) != 0 {
		t.Fatalf("include_domains = %#v, want []", got.body["include_domains"])
	}
	if _, err := s.Search(context.Background(), SearchRequest{Query: "  "}); err == nil {
		t.Fatal("empty query must be refused")
	}
}

func TestTavilySearcherErrorsRedactKey(t *testing.T) {
	srv, _ := fakeTavily(t, http.StatusUnauthorized, `{"detail": {"error": "Unauthorized: missing or invalid API key `+testKey+`"}}`, map[string]string{"Retry-After": testKey})
	s, err := NewTavilySearcher(TavilyConfig{APIKey: testKey, BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Search(context.Background(), SearchRequest{Query: "Acme Pro price"})
	if err == nil || strings.Contains(err.Error(), testKey) || !strings.Contains(err.Error(), "[redacted]") || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("error must name the status and never show the key: %v", err)
	}
	if _, err := NewTavilySearcher(TavilyConfig{}); err == nil {
		t.Fatal("an empty key must be refused")
	}
}
