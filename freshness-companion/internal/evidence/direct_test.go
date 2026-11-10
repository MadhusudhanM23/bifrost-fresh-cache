package evidence

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTMLToText(t *testing.T) {
	page := `<!doctype html><html><head><title>Acme</title><style>p{color:red}</style>
<script>var price = "$99 per month";</script></head>
<body><!-- old: Acme Pro plan — $39 per month -->
<p id="pro-plan-price">Acme Pro plan &mdash; <strong>$49</strong> per month</p></body></html>`
	got := HTMLToText(page)
	if !strings.Contains(got, "Acme Pro plan — $49 per month") {
		t.Fatalf("visible text missing: %q", got)
	}
	for _, hidden := range []string{"$99", "$39", "color:red"} {
		if strings.Contains(got, hidden) {
			t.Errorf("text should not contain %q (script, comment or style): %q", hidden, got)
		}
	}
}

func TestDirectFetchIsLabelledNotTavily(t *testing.T) {
	var gotUA, gotAuth, gotCookie string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA, gotAuth, gotCookie = r.UserAgent(), r.Header.Get("Authorization"), r.Header.Get("Cookie")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<p>Acme Pro plan — $49 per month</p>`)
	}))
	defer srv.Close()
	d := NewDirectFetch(DirectConfig{})
	ev, err := d.Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if d.Name() != DirectFetchName || ev.Provider != DirectFetchName || !strings.Contains(ev.Provider, "not Tavily") {
		t.Fatalf("provider label = %q", ev.Provider)
	}
	if ev.Content != "Acme Pro plan — $49 per month" {
		t.Fatalf("content = %q", ev.Content)
	}
	if !strings.Contains(gotUA, "not Tavily") || gotAuth != "" || gotCookie != "" {
		t.Fatalf("request headers: UA=%q Authorization=%q Cookie=%q", gotUA, gotAuth, gotCookie)
	}
}

func TestDirectFetchErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/big" {
			_, _ = io.WriteString(w, strings.Repeat("x", 2048))
			return
		}
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	d := NewDirectFetch(DirectConfig{MaxBodyBytes: 1024})
	if _, err := d.Fetch(context.Background(), srv.URL+"/down"); err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("HTTP error: got %v", err)
	}
	if _, err := d.Fetch(context.Background(), srv.URL+"/big"); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("oversized page: got %v", err)
	}
}
