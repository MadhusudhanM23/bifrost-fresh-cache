package discover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"freshness-companion/internal/evidence"
	"freshness-companion/internal/policy"
	"freshness-companion/internal/watch"
)

// How a source was found.
const (
	FoundByTavilySearch = "tavily_search"
	FoundByCandidateURL = "candidate_url"
)

// Searcher runs one search (normally *evidence.TavilySearcher).
type Searcher interface {
	Search(ctx context.Context, req evidence.SearchRequest) ([]evidence.SearchResult, error)
}

// Config configures a Service.
type Config struct {
	Searcher Searcher
	// Budget caps Search calls per rolling hour (required).
	Budget *watch.Budget
	// TTL is how long a learned source is reused (default 24h).
	TTL time.Duration
	// MissTTL is how long "search found nothing usable" is remembered, so the
	// same query is not searched again at once (default 1h).
	MissTTL time.Duration
	// MaxSources bounds the store; the oldest entries go first (default 50).
	MaxSources int
	// Timeout bounds one Search call (default 8s). On timeout the caller
	// falls back to the behaviour without discovery.
	Timeout time.Duration
	// StorePath persists learned sources as JSON ("" = memory only).
	StorePath string
	// Rules returns a category's discover block (nil = unknown category).
	// Learned sources read from the store file are checked against its URL
	// and domain rules when loaded; without Rules only the URL rules apply.
	Rules  func(category string) *policy.Discover
	Now    func() time.Time
	Logger *slog.Logger
}

// Source is one picked page.
type Source struct {
	URL   string  `json:"url"`
	Title string  `json:"title,omitempty"`
	Rank  int     `json:"rank,omitempty"`
	Score float64 `json:"score,omitempty"`
}

// Found is what Find returns: the query that was (or would have been) sent
// and the picked page(s). Sources has one entry, or two with min_sources 2.
type Found struct {
	Query    string
	Entities []string
	Sources  []Source
	FoundBy  string
	// Reused is true when the source came from the store (0 credits).
	Reused bool
}

// NoSourceError says why no page was picked and which hosts the results
// came from (no URLs, no snippets).
type NoSourceError struct {
	Detail string
	Hosts  []string
	Cached bool // remembered from an earlier search (negative cache)
}

func (e *NoSourceError) Error() string {
	s := "search found no usable page: " + e.Detail
	if len(e.Hosts) > 0 {
		s += "; result hosts: " + strings.Join(e.Hosts, ", ")
	}
	if e.Cached {
		s += " (remembered from an earlier search)"
	}
	return s
}

func (e *NoSourceError) Unwrap() error { return ErrNoSource }

// Learned is one store entry, as GET /freshness/discovered lists it.
type Learned struct {
	Category    string    `json:"category"`
	Query       string    `json:"query"`
	URL         string    `json:"url,omitempty"`
	Title       string    `json:"title,omitempty"`
	Rank        int       `json:"rank,omitempty"`
	Score       float64   `json:"score,omitempty"`
	Also        []Source  `json:"also,omitempty"` // the second page with min_sources 2
	FoundBy     string    `json:"found_by,omitempty"`
	FoundAt     time.Time `json:"found_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	Uses        int       `json:"uses"`
	LastOutcome string    `json:"last_outcome,omitempty"`
	LastValue   string    `json:"last_value,omitempty"`
	// NoSource marks a remembered miss; Reason and ResultHosts say why.
	NoSource    bool     `json:"no_source,omitempty"`
	Reason      string   `json:"reason,omitempty"`
	ResultHosts []string `json:"result_hosts,omitempty"`

	entities []string
}

// Stats are the service's counters.
type Stats struct {
	SearchesLastHour int              `json:"searches_last_hour"`
	SearchCap        int              `json:"searches_cap_per_hour"`
	Searches         int64            `json:"searches"`
	SearchErrors     int64            `json:"search_errors"`
	Reused           int64            `json:"reused"`
	NoSource         int64            `json:"no_source"`
	OverCap          int64            `json:"over_cap"`
	CandidateUsed    int64            `json:"candidate_used"`
	Shared           int64            `json:"shared_searches"`
	RejectedByReason map[string]int64 `json:"rejected_by_reason"`
}

type call struct {
	done  chan struct{}
	found Found
	err   error
}

// Service finds and remembers sources. It is safe for concurrent use.
type Service struct {
	cfg      Config
	mu       sync.Mutex
	store    map[string]*Learned
	inflight map[string]*call
	stats    Stats
	saveMu   sync.Mutex // one store write at a time
}

// New builds a service and loads the store file, when there is one.
func New(cfg Config) (*Service, error) {
	if cfg.Searcher == nil {
		return nil, errors.New("discover: Searcher is required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Budget == nil {
		cfg.Budget = watch.NewBudget(20, cfg.Now)
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 24 * time.Hour
	}
	if cfg.MissTTL <= 0 {
		cfg.MissTTL = time.Hour
	}
	if cfg.MaxSources <= 0 {
		cfg.MaxSources = 50
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 8 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := &Service{cfg: cfg, store: map[string]*Learned{}, inflight: map[string]*call{}}
	s.stats.RejectedByReason = map[string]int64{}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Find returns a source page for the question in category, by reusing a
// learned one, or by one Tavily Search. Concurrent callers with the same
// query share one search. The question is used only to derive the query.
func (s *Service) Find(ctx context.Context, category string, d *policy.Discover, question string) (Found, error) {
	if !d.Active() {
		return Found{}, ErrSkipped
	}
	q, entities, err := BuildQuery(d, question)
	if err != nil {
		s.count(Reason(err))
		return Found{Query: q, Entities: entities}, err
	}
	key := NormalizeKey(category, q)
	now := s.cfg.Now()

	s.mu.Lock()
	if e := s.store[key]; e != nil {
		// The policy (or the store file) may have changed since the page was
		// learned: a page that no longer passes the rules is dropped and
		// searched for again.
		if err := e.allowed(d); err != nil {
			s.cfg.Logger.Warn("dropping a learned source that no longer passes the URL or domain rules", "category", category, "query", q, "url", e.URL, "reason", err.Error())
			delete(s.store, key)
		}
	}
	if e := s.store[key]; e != nil && now.Before(e.ExpiresAt) {
		if e.NoSource {
			if cand := candidateURL(d); cand != "" {
				// The miss was remembered before the policy named a candidate
				// URL: use it now, without searching again.
				c := &Learned{Category: category, Query: q, URL: cand, FoundBy: FoundByCandidateURL, FoundAt: now, ExpiresAt: now.Add(s.cfg.TTL), Uses: 1}
				s.put(key, c)
				s.stats.CandidateUsed++
				s.mu.Unlock()
				s.save()
				return foundFrom(c, entities), nil
			}
			s.stats.NoSource++
			s.mu.Unlock()
			return Found{Query: q, Entities: entities}, &NoSourceError{Detail: e.Reason, Hosts: e.ResultHosts, Cached: true}
		}
		e.Uses++
		s.stats.Reused++
		f := foundFrom(e, entities)
		f.Reused = true
		s.mu.Unlock()
		s.save()
		return f, nil
	}
	if c := s.inflight[key]; c != nil {
		s.stats.Shared++
		s.mu.Unlock()
		select {
		case <-c.done:
			f := c.found
			f.Entities = entities
			return f, c.err
		case <-ctx.Done():
			return Found{Query: q, Entities: entities}, fmt.Errorf("%w: %v", ErrSearchFailed, ctx.Err())
		}
	}
	c := &call{done: make(chan struct{})}
	s.inflight[key] = c
	s.mu.Unlock()

	c.found, c.err = s.search(ctx, key, category, d, q, entities)
	s.mu.Lock()
	delete(s.inflight, key)
	s.mu.Unlock()
	close(c.done)
	return c.found, c.err
}

func foundFrom(e *Learned, entities []string) Found {
	f := Found{Query: e.Query, Entities: entities, FoundBy: e.FoundBy}
	f.Sources = append(f.Sources, Source{URL: e.URL, Title: e.Title, Rank: e.Rank, Score: e.Score})
	f.Sources = append(f.Sources, e.Also...)
	return f
}

func (s *Service) count(reason string) {
	if reason == "" {
		return
	}
	s.mu.Lock()
	s.stats.RejectedByReason[reason]++
	s.mu.Unlock()
}

// exactDomains drops wildcard entries ("amazon.*"): they are applied locally
// only, so Tavily never receives a pattern it may not accept.
func exactDomains(domains []string) []string {
	out := make([]string, 0, len(domains))
	for _, d := range domains {
		if !strings.Contains(d, "*") {
			out = append(out, d)
		}
	}
	return out
}

func (s *Service) search(ctx context.Context, key, category string, d *policy.Discover, q string, entities []string) (Found, error) {
	base := Found{Query: q, Entities: entities}
	if !s.cfg.Budget.Take() {
		s.mu.Lock()
		s.stats.OverCap++
		s.mu.Unlock()
		return base, ErrOverCap
	}
	// The search must not die with the request that happened to start it:
	// other requests may be waiting for the same result.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.Timeout)
	defer cancel()
	results, err := s.cfg.Searcher.Search(sctx, evidence.SearchRequest{
		Query: q, MaxResults: 5,
		IncludeDomains: append([]string(nil), d.IncludeDomains...),
		ExcludeDomains: exactDomains(d.Exclusions()),
	})
	s.mu.Lock()
	s.stats.Searches++
	if err != nil {
		s.stats.SearchErrors++
	}
	s.mu.Unlock()
	if err != nil {
		s.cfg.Logger.Warn("discovery search failed", "category", category, "query", q, "error", err)
		return base, fmt.Errorf("%w: %s", ErrSearchFailed, oneLine(err.Error(), 200))
	}
	accepted, rejected := Rank(results, d, entities)
	s.mu.Lock()
	for r, n := range rejected {
		s.stats.RejectedByReason[r] += int64(n)
	}
	s.mu.Unlock()

	now := s.cfg.Now()
	e := &Learned{Category: category, Query: q, FoundAt: now, ExpiresAt: now.Add(s.cfg.TTL), FoundBy: FoundByTavilySearch, Uses: 1, entities: entities}
	need := d.Sources()
	switch {
	case len(accepted) >= need:
		e.URL, e.Title, e.Rank, e.Score = accepted[0].URL, accepted[0].Title, accepted[0].Rank, accepted[0].Score
		if need == 2 {
			e.Also = []Source{{URL: accepted[1].URL, Title: accepted[1].Title, Rank: accepted[1].Rank, Score: accepted[1].Score}}
		}
	case candidateURL(d) != "":
		e.URL, e.FoundBy = candidateURL(d), FoundByCandidateURL
		s.mu.Lock()
		s.stats.CandidateUsed++
		s.mu.Unlock()
	default:
		detail := rejectionSummary(len(results), rejected)
		if len(accepted) > 0 && len(accepted) < need {
			detail = fmt.Sprintf("min_sources %d but only %d usable result", need, len(accepted))
		}
		miss := &Learned{Category: category, Query: q, FoundAt: now, ExpiresAt: now.Add(s.cfg.MissTTL), NoSource: true, Reason: detail, ResultHosts: hosts(results)}
		s.mu.Lock()
		s.stats.NoSource++
		s.put(key, miss)
		s.mu.Unlock()
		s.save()
		return base, &NoSourceError{Detail: detail, Hosts: miss.ResultHosts}
	}
	s.mu.Lock()
	s.put(key, e)
	s.mu.Unlock()
	s.save()
	return foundFrom(e, entities), nil
}

// put stores e and evicts the oldest entries over MaxSources. Caller holds mu.
func (s *Service) put(key string, e *Learned) {
	s.store[key] = e
	for len(s.store) > s.cfg.MaxSources {
		oldestKey := ""
		var oldest time.Time
		for k, v := range s.store {
			if oldestKey == "" || v.FoundAt.Before(oldest) {
				oldestKey, oldest = k, v.FoundAt
			}
		}
		delete(s.store, oldestKey)
	}
}

// Record notes how the last check against a learned source came out.
func (s *Service) Record(category, query, outcome, value string) {
	s.mu.Lock()
	e := s.store[NormalizeKey(category, query)]
	if e != nil {
		e.LastOutcome, e.LastValue = outcome, value
	}
	s.mu.Unlock()
	if e != nil {
		s.save()
	}
}

// Sources lists the store, newest first.
func (s *Service) Sources() []Learned {
	if s == nil {
		return []Learned{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Learned, 0, len(s.store))
	for _, e := range s.store {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FoundAt.After(out[j].FoundAt) })
	return out
}

// Stats returns a copy of the counters.
func (s *Service) Stats() Stats {
	if s == nil {
		return Stats{RejectedByReason: map[string]int64{}}
	}
	s.mu.Lock()
	st := s.stats
	st.RejectedByReason = make(map[string]int64, len(s.stats.RejectedByReason))
	for k, v := range s.stats.RejectedByReason {
		st.RejectedByReason[k] = v
	}
	s.mu.Unlock()
	st.SearchesLastHour = s.cfg.Budget.Used()
	st.SearchCap = s.cfg.Budget.Max()
	return st
}

type discoveredResponse struct {
	Enabled bool      `json:"enabled"`
	Note    string    `json:"note"`
	Stats   Stats     `json:"stats"`
	Sources []Learned `json:"sources"`
}

// Register mounts GET /freshness/discovered. It works on a nil *Service too
// (discovery off): the endpoint then says so.
func (s *Service) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /freshness/discovered", func(w http.ResponseWriter, _ *http.Request) {
		resp := discoveredResponse{Enabled: s != nil, Stats: s.Stats(), Sources: s.Sources()}
		if s == nil {
			resp.Note = "Discovery is off (start the companion with -discover)."
		} else {
			resp.Note = "Sources Tavily Search found for questions no watched fact covers. Only the query shown and the chosen URL were sent to Tavily."
		}
		data, err := json.Marshal(resp)
		if err != nil {
			http.Error(w, "encoding error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	})
}

type storeFile struct {
	Version int       `json:"version"`
	Sources []Learned `json:"sources"`
}

func (s *Service) load() error {
	if s.cfg.StorePath == "" {
		return nil
	}
	data, err := os.ReadFile(s.cfg.StorePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("discover: read store: %w", err)
	}
	var f storeFile
	if err := json.Unmarshal(data, &f); err != nil {
		s.cfg.Logger.Warn("discovery store is not valid JSON; starting empty", "file", s.cfg.StorePath)
		return nil
	}
	now := s.cfg.Now()
	for i := range f.Sources {
		e := f.Sources[i]
		if !now.Before(e.ExpiresAt) || e.Query == "" {
			continue
		}
		var rules *policy.Discover
		if s.cfg.Rules != nil {
			rules = s.cfg.Rules(e.Category)
		}
		if err := e.allowed(rules); err != nil {
			s.cfg.Logger.Warn("discovery store: dropping a learned source that does not pass the URL or domain rules", "file", s.cfg.StorePath, "category", e.Category, "url", e.URL, "reason", err.Error())
			continue
		}
		s.put(NormalizeKey(e.Category, e.Query), &e)
	}
	return nil
}

func (s *Service) save() {
	if s.cfg.StorePath == "" {
		return
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.mu.Lock()
	f := storeFile{Version: 1}
	for _, e := range s.store {
		f.Sources = append(f.Sources, *e)
	}
	s.mu.Unlock()
	sort.Slice(f.Sources, func(i, j int) bool { return f.Sources[i].FoundAt.Before(f.Sources[j].FoundAt) })
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.cfg.StorePath), 0o755); err != nil {
		s.cfg.Logger.Warn("could not save the discovery store", "error", err)
		return
	}
	tmp := s.cfg.StorePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		s.cfg.Logger.Warn("could not save the discovery store", "error", err)
		return
	}
	if err := os.Rename(tmp, s.cfg.StorePath); err != nil {
		s.cfg.Logger.Warn("could not save the discovery store", "error", err)
	}
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}
