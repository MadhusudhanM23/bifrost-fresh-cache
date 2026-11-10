package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleExplainRefusesOddIDsAndPassesThrough(t *testing.T) {
	companion := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/freshness/explain/req-0a0b0c0d-1" {
			w.Write([]byte(`{"how":"direct_hit","plain":"This answer came from the cache."}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer companion.Close()
	s := &server{companionBase: companion.URL, client: companion.Client()}
	for id, want := range map[string]int{"req-0a0b0c0d-1": 200, "req-0a0b0c0d-2": 404, "../healthz": 400, "req-ZZ-1": 400} {
		req := httptest.NewRequest("GET", "/api/explain/x", nil)
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		s.handleExplain(rec, req)
		if rec.Code != want {
			t.Errorf("id %q: status %d, want %d (%s)", id, rec.Code, want, rec.Body.String())
		}
	}
}
