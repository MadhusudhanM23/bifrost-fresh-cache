package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type scriptedSender struct {
	responses []*Response
	sent      []*Request
}

func (s *scriptedSender) Send(_ context.Context, req *Request) (*Response, error) {
	s.sent = append(s.sent, req)
	if len(s.responses) == 0 {
		return nil, errors.New("no scripted response left")
	}
	r := s.responses[0]
	s.responses = s.responses[1:]
	return r, nil
}

type recordingClearer struct {
	ids []string
	err error
}

func (c *recordingClearer) Clear(_ context.Context, id string) error {
	c.ids = append(c.ids, id)
	return c.err
}

func cacheResp(hit bool, id string) *Response {
	h := http.Header{}
	if hit {
		h.Set(HeaderCacheHit, "true")
	} else {
		h.Set(HeaderCacheHit, "false")
	}
	h.Set(HeaderCacheID, id)
	return &Response{StatusCode: http.StatusOK, Header: h, Body: []byte(`{}`)}
}

func testRequest() *Request {
	h := http.Header{}
	h.Set("X-Bf-Cache-Key", "demo")
	h.Set("Authorization", "Bearer sk-bf-virtual-key")
	return &Request{Method: http.MethodPost, Path: "/v1/chat/completions", Header: h, Body: []byte(`{}`)}
}

func TestClearAndResendClearsASemanticNeighbour(t *testing.T) {
	sender := &scriptedSender{responses: []*Response{cacheResp(true, "neighbour"), cacheResp(false, "fresh")}}
	clearer := &recordingClearer{}
	s := &ClearAndResend{Clearer: clearer, Sender: sender, MaxAttempts: 3}
	res, err := s.Refresh(context.Background(), testRequest(), "stale")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(clearer.ids, ",") != "stale,neighbour" || res.Response.CacheID() != "fresh" || len(res.Cleared) != 2 {
		t.Fatalf("cleared %v, result %+v", clearer.ids, res)
	}
}

func TestClearAndResendReportsWhenTheSameEntryComesBack(t *testing.T) {
	sender := &scriptedSender{responses: []*Response{cacheResp(true, "stale")}}
	s := &ClearAndResend{Clearer: &recordingClearer{err: errors.New("clear stale: Bifrost answered HTTP 401")}, Sender: sender, MaxAttempts: 3}
	res, err := s.Refresh(context.Background(), testRequest(), "stale")
	// Not an error here: the caller decides, by the source, whether that
	// cached answer is fresh (a concurrent request may have written it).
	if err != nil || !res.ServedFromCache || res.Response.CacheID() != "stale" || !strings.Contains(res.ClearErrors, "HTTP 401") {
		t.Fatalf("got %+v, %v", res, err)
	}
}

func TestClearAndResendGivesUp(t *testing.T) {
	sender := &scriptedSender{responses: []*Response{cacheResp(true, "a"), cacheResp(true, "b")}}
	s := &ClearAndResend{Clearer: &recordingClearer{}, Sender: sender, MaxAttempts: 2}
	if _, err := s.Refresh(context.Background(), testRequest(), "stale"); err == nil || !strings.Contains(err.Error(), "after 2 attempts") {
		t.Fatalf("got %v", err)
	}
}

func TestResendWithHeadersEditsOnlyTheCopy(t *testing.T) {
	sender := &scriptedSender{responses: []*Response{cacheResp(false, "fresh")}}
	s := &ResendWithHeaders{Sender: sender, Headers: HeaderEdits{
		Drop: []string{"x-bf-cache-key"},
		Set:  map[string]string{"X-Bf-Cache-Threshold": "1"},
	}}
	orig := testRequest()
	if _, err := s.Refresh(context.Background(), orig, "stale"); err != nil {
		t.Fatal(err)
	}
	sent := sender.sent[0].Header
	if sent.Get("X-Bf-Cache-Key") != "" || sent.Get("X-Bf-Cache-Threshold") != "1" || sent.Get("Authorization") == "" {
		t.Fatalf("sent headers = %v", sent)
	}
	if orig.Header.Get("X-Bf-Cache-Key") != "demo" {
		t.Fatal("the original request must not be modified")
	}

	// Still a hit after the bypass headers: refuse rather than serve it.
	sender = &scriptedSender{responses: []*Response{cacheResp(true, "stale")}}
	s.Sender = sender
	if _, err := s.Refresh(context.Background(), testRequest(), "stale"); err == nil {
		t.Fatal("a cached response must never count as refreshed")
	}
	if _, err := (&ResendWithHeaders{Sender: sender}).Refresh(context.Background(), testRequest(), "x"); err == nil {
		t.Fatal("no header edits configured: want an error")
	}
}

func TestBifrostClearerRequest(t *testing.T) {
	var (
		mu                         sync.Mutex
		gotMethod, gotURI, gotAuth string
		status                     = http.StatusOK
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gotMethod, gotURI, gotAuth = r.Method, r.RequestURI, r.Header.Get("X-Admin-Auth")
		w.WriteHeader(status)
	}))
	defer srv.Close()
	seen := func() (string, string, string) {
		mu.Lock()
		defer mu.Unlock()
		return gotMethod, gotURI, gotAuth
	}
	c, err := NewBifrostClearer(srv.URL, "X-Admin-Auth", "Basic c2VjcmV0", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Clear(context.Background(), "550e8500-e29b-41d4-a725-446655440001"); err != nil {
		t.Fatal(err)
	}
	if m, uri, auth := seen(); m != http.MethodDelete || uri != "/api/cache/clear/550e8500-e29b-41d4-a725-446655440001" || auth != "Basic c2VjcmV0" {
		t.Fatalf("request = %s %s auth=%q", m, uri, auth)
	}
	if err := c.Clear(context.Background(), "../a/b c"); err != nil {
		t.Fatal(err)
	}
	if _, uri, _ := seen(); uri != "/api/cache/clear/..%2Fa%2Fb%20c" {
		t.Fatalf("id not kept inside one path segment: %s", uri)
	}
	mu.Lock()
	status = http.StatusUnauthorized
	mu.Unlock()
	err = c.Clear(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") || strings.Contains(err.Error(), "c2VjcmV0") {
		t.Fatalf("got %v", err)
	}
}
