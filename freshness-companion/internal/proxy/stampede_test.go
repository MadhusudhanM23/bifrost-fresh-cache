package proxy

import (
	"net/http"
	"strings"
	"sync"
	"testing"
)

// When a popular entry goes stale, every request that arrives together must
// share ONE refresh: one clear, one model call, and the same fresh answer for
// all of them.
func TestStaleEntryIsRefreshedOnceForConcurrentRequests(t *testing.T) {
	page := newFakePage(t, "49")
	fb := newFakeBifrost(t, "49")
	c := newCompanion(t, fb.srv.URL, testPolicyDoc(page.srv.URL, "bypass"))

	postChat(t, c.url, proPrompt, nil) // the cache now holds "$49"
	page.set("59", 0, "")
	fb.setPrice("59")

	const n = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	statuses := make([]int, n)
	bodies := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			resp, body := postChat(t, c.url, proPrompt, nil)
			statuses[i], bodies[i] = resp.StatusCode, string(body)
		}(i)
	}
	close(start)
	wg.Wait()

	for i := 0; i < n; i++ {
		if statuses[i] != http.StatusOK {
			t.Errorf("request %d: status %d, body %s", i, statuses[i], bodies[i])
			continue
		}
		if !strings.Contains(bodies[i], "$59 per month") || strings.Contains(bodies[i], "$49") {
			t.Errorf("request %d did not get the fresh answer: %s", i, bodies[i])
		}
	}
	llm, clears, _ := fb.snapshot()
	if llm != 2 || len(clears) != 1 {
		t.Fatalf("llm calls = %d, clears = %d; want 2 (the first miss + ONE shared refresh) and 1", llm, len(clears))
	}
}
