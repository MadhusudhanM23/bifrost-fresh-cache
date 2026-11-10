// Package watch decides how often each source page is checked (smart
// checking) and, with the background watcher on, checks pages before their
// evidence expires and removes cached answers that state an old value.
//
// Smart checking in one sentence: a page that keeps saying the same thing
// is checked less and less often (up to a cap), and the moment it changes
// the checks go back to the fastest rate.
package watch

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"freshness-companion/internal/evidence"
	"freshness-companion/internal/extract"
	"freshness-companion/internal/policy"
)

// Config sets the re-check interval rules.
type Config struct {
	// Floor is the shortest interval: the -evidence-ttl. Every source starts
	// here and returns here after a change.
	Floor time.Duration
	// Max caps the interval (-check-max). Must be >= Floor.
	Max time.Duration
	// Growth multiplies the interval after an unchanged check (-check-growth).
	Growth float64
	// Adaptive false keeps every interval at Floor: today's fixed TTL.
	Adaptive bool
	// Now is the clock (tests inject one).
	Now func() time.Time
}

// FactChange is one fact whose value differs from the last check.
type FactChange struct {
	Name     string `json:"name"`
	Category string `json:"category,omitempty"`
	// Old and New are the raw values; an empty New with NewReadable false
	// means the pattern no longer finds one value on the page.
	Old         string `json:"old"`
	New         string `json:"new"`
	NewReadable bool   `json:"new_readable"`
	Mode        extract.Mode
}

// Change says a source page states something different from last time.
type Change struct {
	SourceURL string
	Provider  string
	At        time.Time
	Facts     []FactChange
}

// SourceState is a snapshot of one source for /freshness/watch.
type SourceState struct {
	SourceURL       string            `json:"source_url"`
	Facts           []string          `json:"facts"`
	Values          map[string]string `json:"values"`
	IntervalSeconds float64           `json:"interval_seconds"`
	LastCheckedAt   *time.Time        `json:"last_checked_at,omitempty"`
	NextCheckAt     *time.Time        `json:"next_check_at,omitempty"`
	UnchangedStreak int               `json:"unchanged_streak"`
	Checks          int               `json:"checks"`
	Changes         int               `json:"changes"`
	LastChangeAt    *time.Time        `json:"last_change_at,omitempty"`
	Errors          int               `json:"errors"`
	LastError       string            `json:"last_error"`
	LastUsedAt      *time.Time        `json:"last_used_at,omitempty"`
	Active          bool              `json:"active"`
	KnownEntries    int               `json:"known_entries"`
	// Changed is set only in a CheckNow result: this check found a change.
	Changed bool `json:"changed,omitempty"`

	interval time.Duration
	nextAt   time.Time
}

type source struct {
	url         string
	facts       []*policy.Fact
	interval    time.Duration
	lastCheck   time.Time
	retryAt     time.Time // set after a fetch error: the next check is due then
	fingerprint string    // normalized value of every fact, "" before the first good check
	values      map[string]string
	streak      int
	checks      int
	changes     int
	errors      int
	lastChange  time.Time
	lastError   string
	lastUsed    time.Time
	hasValues   bool
}

// Schedule keeps one interval per source URL. It implements evidence.Scheduler.
type Schedule struct {
	mu       sync.Mutex
	cfg      Config
	sources  map[string]*source
	onChange func(Change)
}

// NewSchedule builds a schedule for every source URL the facts read. The
// key is the fact's exact source_url string, which is also the evidence
// cache key.
func NewSchedule(cfg Config, facts []*policy.Fact) (*Schedule, error) {
	if cfg.Floor <= 0 {
		return nil, errors.New("the check floor (-evidence-ttl) must be positive")
	}
	if cfg.Max <= 0 {
		cfg.Max = cfg.Floor
	}
	if cfg.Max < cfg.Floor {
		return nil, fmt.Errorf("-check-max %s is shorter than the floor %s", cfg.Max, cfg.Floor)
	}
	if cfg.Adaptive && cfg.Growth <= 1 {
		return nil, fmt.Errorf("-check-growth must be greater than 1, got %g", cfg.Growth)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	s := &Schedule{cfg: cfg, sources: map[string]*source{}}
	for _, f := range facts {
		if f == nil || f.SourceURL == "" {
			continue
		}
		src := s.sources[f.SourceURL]
		if src == nil {
			src = &source{url: f.SourceURL, interval: cfg.Floor, values: map[string]string{}}
			s.sources[f.SourceURL] = src
		}
		src.facts = append(src.facts, f)
	}
	return s, nil
}

// SetOnChange sets the function called (with no lock held) whenever a
// fetch shows a source states something different from the last check.
func (s *Schedule) SetOnChange(fn func(Change)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onChange = fn
}

// Config returns the rules in use.
func (s *Schedule) Config() Config { return s.cfg }

// TTL implements evidence.Scheduler: the current interval of sourceURL, or
// the floor for a URL no fact reads.
func (s *Schedule) TTL(sourceURL string) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if src := s.sources[sourceURL]; src != nil {
		return src.interval
	}
	return s.cfg.Floor
}

// Used implements evidence.Scheduler.
func (s *Schedule) Used(sourceURL string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if src := s.sources[sourceURL]; src != nil {
		src.lastUsed = s.cfg.Now()
	}
}

// Invalidated implements evidence.Scheduler: a change notice resets the
// interval to the floor. The remembered values stay, so the next fetch can
// still report what changed.
func (s *Schedule) Invalidated(sourceURL string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if src := s.sources[sourceURL]; src != nil {
		src.interval = s.cfg.Floor
		src.streak = 0
		src.retryAt = time.Time{}
	}
}

// Fetched implements evidence.Scheduler. It reads every fact of the source
// from the evidence, compares the result with the last check and sets the
// next interval:
//   - every fact readable and all unchanged: interval x Growth, capped (adaptive only);
//   - a readable fact differs: interval back to the floor, and a Change is emitted;
//   - a fact unreadable: interval back to the floor, no growth;
//   - fetch error: interval unchanged, next check after the floor.
func (s *Schedule) Fetched(sourceURL string, ev evidence.Evidence, err error) {
	s.mu.Lock()
	src := s.sources[sourceURL]
	if src == nil {
		s.mu.Unlock()
		return
	}
	now := s.cfg.Now()
	src.checks++
	if err != nil {
		src.errors++
		src.lastError = oneLine(err.Error(), 200)
		src.retryAt = now.Add(s.cfg.Floor)
		s.mu.Unlock()
		return
	}
	src.lastCheck = now
	src.retryAt = time.Time{}
	src.lastError = ""

	values := map[string]string{}
	norms := make([]string, 0, len(src.facts))
	allReadable := true
	for _, f := range src.facts {
		v, xerr := extract.Extract(f.SourceRegexp(), ev.Content, f.Compare)
		if xerr != nil {
			allReadable = false
			norms = append(norms, f.Name+"=!unreadable")
			continue
		}
		values[f.Name] = v.Raw
		norms = append(norms, f.Name+"="+v.Normalized)
	}
	fingerprint := strings.Join(norms, "\x00")

	var change *Change
	switch {
	case !src.hasValues:
		// First good check: nothing to compare with.
		src.interval = s.cfg.Floor
		src.streak = 0
	case fingerprint == src.fingerprint:
		src.streak++
		if s.cfg.Adaptive && allReadable {
			src.interval = grow(src.interval, s.cfg.Growth, s.cfg.Max)
		} else {
			src.interval = s.cfg.Floor
		}
	default:
		src.interval = s.cfg.Floor
		src.streak = 0
		src.changes++
		src.lastChange = now
		change = &Change{SourceURL: sourceURL, Provider: ev.Provider, At: now}
		for _, f := range src.facts {
			oldV, hadOld := src.values[f.Name]
			newV, hasNew := values[f.Name]
			if hadOld == hasNew && (!hasNew || sameValue(oldV, newV, f.Compare)) {
				continue
			}
			change.Facts = append(change.Facts, FactChange{Name: f.Name, Category: f.Category, Old: oldV, New: newV, NewReadable: hasNew, Mode: f.Compare})
		}
	}
	src.fingerprint = fingerprint
	src.values = values
	src.hasValues = true
	onChange := s.onChange
	s.mu.Unlock()

	if change != nil && onChange != nil && len(change.Facts) > 0 {
		onChange(*change)
	}
}

func grow(interval time.Duration, growth float64, max time.Duration) time.Duration {
	next := time.Duration(float64(interval) * growth)
	if next <= interval { // guard against rounding
		next = interval + 1
	}
	if next > max {
		next = max
	}
	return next
}

func sameValue(a, b string, mode extract.Mode) bool {
	va, errA := extract.Normalize(a, mode)
	vb, errB := extract.Normalize(b, mode)
	if errA != nil || errB != nil {
		return a == b
	}
	return extract.Equal(va, vb, mode)
}

// Sources returns a snapshot of every source, sorted by URL. Active and
// KnownEntries are filled in by the watcher.
func (s *Schedule) Sources() []SourceState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SourceState, 0, len(s.sources))
	for _, src := range s.sources {
		out = append(out, s.stateLocked(src))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SourceURL < out[j].SourceURL })
	return out
}

// Source returns the state of one source URL.
func (s *Schedule) Source(sourceURL string) (SourceState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.sources[sourceURL]
	if src == nil {
		return SourceState{}, false
	}
	return s.stateLocked(src), true
}

// SourceForFact returns the source URL a fact reads.
func (s *Schedule) SourceForFact(name string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for url, src := range s.sources {
		for _, f := range src.facts {
			if f.Name == name {
				return url, true
			}
		}
	}
	return "", false
}

// Facts returns the facts that read sourceURL.
func (s *Schedule) Facts(sourceURL string) []*policy.Fact {
	s.mu.Lock()
	defer s.mu.Unlock()
	if src := s.sources[sourceURL]; src != nil {
		return append([]*policy.Fact(nil), src.facts...)
	}
	return nil
}

func (s *Schedule) stateLocked(src *source) SourceState {
	st := SourceState{
		SourceURL:       src.url,
		Facts:           make([]string, 0, len(src.facts)),
		Values:          make(map[string]string, len(src.values)),
		IntervalSeconds: src.interval.Seconds(),
		UnchangedStreak: src.streak,
		Checks:          src.checks,
		Changes:         src.changes,
		Errors:          src.errors,
		LastError:       src.lastError,
		interval:        src.interval,
	}
	for _, f := range src.facts {
		st.Facts = append(st.Facts, f.Name)
	}
	for k, v := range src.values {
		st.Values[k] = v
	}
	st.LastCheckedAt = optTime(src.lastCheck)
	st.LastChangeAt = optTime(src.lastChange)
	st.LastUsedAt = optTime(src.lastUsed)
	switch {
	case !src.retryAt.IsZero():
		st.nextAt = src.retryAt
	case !src.lastCheck.IsZero():
		st.nextAt = src.lastCheck.Add(src.interval)
	}
	st.NextCheckAt = optTime(st.nextAt)
	return st
}

func optTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}
