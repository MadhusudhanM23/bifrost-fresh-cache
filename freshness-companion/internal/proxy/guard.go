package proxy

import (
	"bytes"
	"net/http"
	"sort"
	"sync"
	"time"
)

// HeaderGuard marks a request the cache guard kept out of the cache; the
// value names the reason.
const HeaderGuard = "X-Freshness-Guard"

// guardBypassKey is a cache key nothing is ever stored under: requests sent
// with it (and no-store) can neither read nor write an entry, whatever
// default_cache_key Bifrost has.
const guardBypassKey = "freshness-guard-bypass"

// GuardStats is what GET /freshness/guard reports. No prompt or tool name is
// kept: only counts per reason.
type GuardStats struct {
	Enabled  bool             `json:"enabled"`
	Since    time.Time        `json:"since"`
	Bypassed int64            `json:"bypassed"`
	ByReason map[string]int64 `json:"by_reason"`
}

type guardCounter struct {
	since time.Time

	mu       sync.Mutex
	bypassed int64
	byReason map[string]int64
}

func (g *guardCounter) add(reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.bypassed++
	g.byReason[reason]++
}

func (g *guardCounter) snapshot(enabled bool) GuardStats {
	g.mu.Lock()
	defer g.mu.Unlock()
	by := make(map[string]int64, len(g.byReason))
	keys := make([]string, 0, len(g.byReason))
	for k := range g.byReason {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		by[k] = g.byReason[k]
	}
	return GuardStats{Enabled: enabled, Since: g.since, Bypassed: g.bypassed, ByReason: by}
}

// guardEnabled reports whether the policy has any guard rule on.
func (s *Server) guardEnabled() bool {
	g := s.cfg.Policy.Guard
	return g.ToolResults == "bypass" || len(g.SideEffectTools) > 0 || g.SensitiveOn()
}

// guarded sends a request the guard keeps out of the cache. It goes to Bifrost
// with a key nothing is stored under and no-store, so it can neither be
// answered from the cache nor leave an answer in it. Streams work too, since
// only headers change.
func (s *Server) guarded(w http.ResponseWriter, r *http.Request, body []byte, reason string) {
	s.guard.add(reason)
	r.Header.Set("X-Bf-Cache-Key", guardBypassKey)
	r.Header.Set(HeaderCacheNoStore, "true")
	r.Header.Del(HeaderCacheType)
	resp, err := s.cfg.Upstream.Open(r.Context(), r.Method, r.URL.EscapedPath(), r.URL.RawQuery, r.Header, bytes.NewReader(body), int64(len(body)))
	if err != nil {
		s.upstreamFailed(w, err)
		return
	}
	defer resp.Body.Close()
	w.Header().Set(HeaderGuard, "bypassed: "+reason)
	s.stream(w, resp, nil)
}

func (s *Server) handleGuard(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.guard.snapshot(s.guardEnabled()))
}
