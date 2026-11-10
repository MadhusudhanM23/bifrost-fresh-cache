package watch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"freshness-companion/internal/decisionlog"
	"freshness-companion/internal/evidence"
	"freshness-companion/internal/extract"
)

// Errors returned by CheckNow.
var (
	ErrUnknownSource = errors.New("no watched fact reads that source")
	ErrOverBudget    = errors.New("the watcher's hourly fetch budget is used up")
	ErrWatcherOff    = errors.New("the background watcher is off (-watch=false)")
)

// Refresher is the evidence cache's Refresh.
type Refresher interface {
	Refresh(ctx context.Context, sourceURL string) (evidence.Result, error)
}

// Clearer deletes one Bifrost cache entry (proxy.BifrostClearer).
type Clearer interface {
	Clear(ctx context.Context, cacheID string) error
}

// AgeStore is the agetrack.Tracker: it says when an entry was written.
type AgeStore interface {
	Age(cacheID string) (time.Duration, bool)
	Forget(cacheID string) error
}

// Recorder is the decision log.
type Recorder interface {
	Append(decisionlog.Record) error
}

// Options configures a Watcher.
type Options struct {
	// Schedule is required. The watcher registers itself as its change handler.
	Schedule *Schedule
	// Cache is the evidence cache (required when Enabled).
	Cache Refresher
	// Index maps facts to the cache entries that state them. nil = a new one.
	Index *Index
	// Ages, Clearer and Decisions are optional: without a Clearer nothing is
	// evicted (changes are only noted).
	Ages      AgeStore
	Clearer   Clearer
	Decisions Recorder
	// Enabled is -watch. Off: Tick does nothing and CheckNow says ErrWatcherOff.
	Enabled bool
	// MaxPerHour caps watcher fetches per rolling hour (-watch-max-per-hour).
	MaxPerHour int
	// Idle: a source nobody looked up for this long is not checked
	// (-watch-idle). 0 = check every source that was ever looked up.
	Idle time.Duration
	// Evict is -watch-evict: delete entries that state an old value.
	Evict bool
	// Provider names the evidence provider in records when the evidence
	// does not say.
	Provider string
	// Tick is the loop period (default 1 s).
	Tick   time.Duration
	Now    func() time.Time
	Logger *slog.Logger
}

// Stats is the watcher part of /freshness/watch.
type Stats struct {
	Enabled           bool    `json:"enabled"`
	MaxPerHour        int     `json:"max_per_hour"`
	UsedLastHour      int     `json:"used_last_hour"`
	SkippedOverBudget int64   `json:"skipped_over_budget"`
	IdleAfterSeconds  float64 `json:"idle_after_seconds"`
	Evictions         int64   `json:"evictions"`
	ChangesDropped    int64   `json:"changes_dropped"`
	Checks            int64   `json:"checks"`
	ChangesSeen       int64   `json:"changes_seen"`
	EvictionErrors    int64   `json:"eviction_errors"`
	KnownEntries      int     `json:"known_entries"`
}

// Watcher checks active sources shortly before their evidence expires and
// removes cached answers that state the old value of a fact that changed.
type Watcher struct {
	opt     Options
	budget  *Budget
	changes chan Change
	log     *slog.Logger

	tickMu sync.Mutex // checks run one at a time

	dropped, evictions, skipped, checks, seen, evictErrs atomic.Int64
}

const changeQueue = 16

// New builds a watcher and makes it the schedule's change handler.
func New(o Options) (*Watcher, error) {
	if o.Schedule == nil {
		return nil, errors.New("watch: Schedule is required")
	}
	if o.Enabled && o.Cache == nil {
		return nil, errors.New("watch: Cache is required when the watcher is on")
	}
	if o.Index == nil {
		o.Index = NewIndex(DefaultIndexSize)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Tick <= 0 {
		o.Tick = time.Second
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	w := &Watcher{opt: o, budget: NewBudget(o.MaxPerHour, o.Now), changes: make(chan Change, changeQueue), log: o.Logger}
	o.Schedule.SetOnChange(w.OnChange)
	return w, nil
}

// Index returns the entry index (so main can subscribe it to the log).
func (w *Watcher) Index() *Index { return w.opt.Index }

// Enabled reports whether -watch is on.
func (w *Watcher) Enabled() bool { return w.opt.Enabled }

// OnChange queues a change for the eviction worker. It never blocks: when
// the queue is full the change is dropped and counted (the entries it
// would have evicted are still refreshed on their next hit).
func (w *Watcher) OnChange(c Change) {
	w.seen.Add(1)
	select {
	case w.changes <- c:
	default:
		w.dropped.Add(1)
		w.log.Warn("watcher change queue full; change dropped", "source_url", c.SourceURL)
	}
}

// Run ticks until ctx is done, with one eviction worker beside the loop.
// A panic in one iteration is logged and the loop goes on.
func (w *Watcher) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case c := <-w.changes:
				w.safely("eviction", func() { w.apply(ctx, c) })
			}
		}
	}()
	t := time.NewTicker(w.opt.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-t.C:
			w.safely("tick", func() { w.Tick(ctx) })
		}
	}
}

func (w *Watcher) safely(what string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			w.log.Error("watcher recovered from a panic", "in", what, "panic", fmt.Sprint(r))
		}
	}()
	fn()
}

// Drain applies every queued change now and returns how many it applied.
// Run's worker does the same in the background; tests and the simulator
// call Drain to stay deterministic.
func (w *Watcher) Drain(ctx context.Context) int {
	n := 0
	for {
		select {
		case c := <-w.changes:
			w.safely("eviction", func() { w.apply(ctx, c) })
			n++
		default:
			return n
		}
	}
}

// Tick checks every active source whose evidence is about to expire (lead
// = min(interval/5, 5 s) left), one at a time, within the hourly budget.
func (w *Watcher) Tick(ctx context.Context) {
	if !w.opt.Enabled {
		return
	}
	w.tickMu.Lock()
	defer w.tickMu.Unlock()
	now := w.opt.Now()
	for _, st := range w.opt.Schedule.Sources() {
		if ctx.Err() != nil {
			return
		}
		if !w.active(st, now) || !due(st, now) {
			continue
		}
		if !w.budget.Take() {
			w.skipped.Add(1)
			continue
		}
		w.checks.Add(1)
		if _, err := w.opt.Cache.Refresh(ctx, st.SourceURL); err != nil {
			w.log.Debug("watcher check failed", "source_url", st.SourceURL, "error", err)
		}
	}
}

func (w *Watcher) active(st SourceState, now time.Time) bool {
	if st.LastUsedAt == nil {
		return false
	}
	return w.opt.Idle <= 0 || now.Sub(*st.LastUsedAt) <= w.opt.Idle
}

func due(st SourceState, now time.Time) bool {
	if st.nextAt.IsZero() {
		return true // looked up but never fetched
	}
	return st.nextAt.Sub(now) <= lead(st.interval)
}

func lead(interval time.Duration) time.Duration {
	l := interval / 5
	if l > 5*time.Second {
		l = 5 * time.Second
	}
	return l
}

// Sources returns every source with Active and KnownEntries filled in.
func (w *Watcher) Sources() []SourceState {
	now := w.opt.Now()
	out := w.opt.Schedule.Sources()
	for i := range out {
		w.decorate(&out[i], now)
	}
	return out
}

func (w *Watcher) decorate(st *SourceState, now time.Time) {
	st.Active = w.active(*st, now)
	st.KnownEntries = 0
	for _, f := range st.Facts {
		st.KnownEntries += w.opt.Index.Count(f)
	}
}

// CheckNow fetches one source at once. target is a source_url or a fact
// name. It returns the source's state afterwards, with Changed set when
// this check found a different value. Errors: ErrWatcherOff, ErrUnknownSource,
// ErrOverBudget, or the fetch error (the state is still returned then).
func (w *Watcher) CheckNow(ctx context.Context, target string) (SourceState, error) {
	if !w.opt.Enabled {
		return SourceState{}, ErrWatcherOff
	}
	target = strings.TrimSpace(target)
	url := target
	before, ok := w.opt.Schedule.Source(url)
	if !ok {
		if url, ok = w.opt.Schedule.SourceForFact(target); !ok {
			return SourceState{}, ErrUnknownSource
		}
		before, _ = w.opt.Schedule.Source(url)
	}
	if !w.budget.Take() {
		return SourceState{}, ErrOverBudget
	}
	w.checks.Add(1)
	_, ferr := w.opt.Cache.Refresh(ctx, url)
	// Evict on a context the caller cannot cancel: a change seen here is used
	// up by this drain, so a disconnecting HTTP client must not cut it short.
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	w.Drain(dctx)
	after, _ := w.opt.Schedule.Source(url)
	w.decorate(&after, w.opt.Now())
	after.Changed = after.Changes > before.Changes
	if ferr != nil {
		return after, fmt.Errorf("the check ran but the fetch failed: %w", ferr)
	}
	return after, nil
}

// Stats returns the counters.
func (w *Watcher) Stats() Stats {
	return Stats{
		Enabled:           w.opt.Enabled,
		MaxPerHour:        w.budget.Max(),
		UsedLastHour:      w.budget.Used(),
		SkippedOverBudget: w.skipped.Load(),
		IdleAfterSeconds:  w.opt.Idle.Seconds(),
		Evictions:         w.evictions.Load(),
		ChangesDropped:    w.dropped.Load(),
		Checks:            w.checks.Load(),
		ChangesSeen:       w.seen.Load(),
		EvictionErrors:    w.evictErrs.Load(),
		KnownEntries:      w.opt.Index.Len(),
	}
}

// apply reacts to one change: for every fact with a readable new value,
// delete the entries the index knows state a different value, except
// entries written after the change was seen (a refresh under a
// deterministic id may have rewritten them already). One record per fact.
func (w *Watcher) apply(ctx context.Context, c Change) {
	now := w.opt.Now()
	for _, fc := range c.Facts {
		rec := decisionlog.Record{
			Time:         now.UTC(),
			Category:     fc.Category,
			Fact:         fc.Name,
			Outcome:      "changed",
			Action:       "noted",
			SourceURL:    c.SourceURL,
			Provider:     c.Provider,
			CachedValue:  fc.Old,
			CurrentValue: fc.New,
			AnswerOf:     "watcher",
		}
		if rec.Provider == "" {
			rec.Provider = w.opt.Provider
		}
		if !c.At.IsZero() {
			at := c.At.UTC()
			rec.CheckedAt = &at
		}
		switch {
		case !fc.NewReadable:
			rec.Reason = fmt.Sprintf("fact %q: the source no longer states one readable value (was %s); nothing was evicted", fc.Name, fc.Old)
		case !w.opt.Evict || w.opt.Clearer == nil:
			n := w.countOld(fc)
			rec.Reason = fmt.Sprintf("fact %q changed from %s to %s; eviction is off, %d known entries state the old value and will be refreshed on their next hit", fc.Name, fc.Old, fc.New, n)
		default:
			evicted, skipped, errs := w.evict(ctx, fc, c.At, now)
			rec.Evicted = evicted
			if len(evicted) > 0 {
				rec.Action = "evicted"
			}
			rec.Reason = fmt.Sprintf("fact %q changed from %s to %s; %d cached answers stating the old value were deleted, %d written after the change were kept", fc.Name, fc.Old, fc.New, len(evicted), skipped)
			if len(errs) > 0 {
				rec.Error = oneLine(strings.Join(errs, "; "), 300)
			}
		}
		if w.opt.Decisions != nil {
			if err := w.opt.Decisions.Append(rec); err != nil {
				w.log.Warn("could not write the watcher's decision record", "error", err)
			}
		}
		w.log.Info("watcher change", "fact", fc.Name, "source_url", c.SourceURL, "old", fc.Old, "new", fc.New, "action", rec.Action, "evicted", len(rec.Evicted), "error", rec.Error)
	}
}

func (w *Watcher) countOld(fc FactChange) int {
	n := 0
	for _, v := range w.opt.Index.Entries(fc.Name) {
		if !statesCurrent(v, fc) {
			n++
		}
	}
	return n
}

func (w *Watcher) evict(ctx context.Context, fc FactChange, changeAt, now time.Time) (evicted []string, skipped int, errs []string) {
	entries := w.opt.Index.Entries(fc.Name)
	ids := make([]string, 0, len(entries))
	for id := range entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if statesCurrent(entries[id], fc) {
			continue
		}
		if w.opt.Ages != nil && !changeAt.IsZero() {
			if age, ok := w.opt.Ages.Age(id); ok && now.Add(-age).After(changeAt) {
				skipped++ // rewritten after the change was seen: it states the new value
				continue
			}
		}
		if err := w.opt.Clearer.Clear(ctx, id); err != nil {
			w.evictErrs.Add(1)
			errs = append(errs, oneLine(err.Error(), 120))
			continue
		}
		evicted = append(evicted, id)
		w.evictions.Add(1)
		w.opt.Index.Forget(fc.Name, id)
		if w.opt.Ages != nil {
			if err := w.opt.Ages.Forget(id); err != nil {
				w.log.Warn("could not update the age store", "error", err)
			}
		}
	}
	return evicted, skipped, errs
}

// statesCurrent reports whether an indexed value equals the fact's new value.
func statesCurrent(value string, fc FactChange) bool {
	mode := fc.Mode
	if !mode.Valid() {
		mode = extract.Exact
	}
	return sameValue(value, fc.New, mode)
}
