package watch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"freshness-companion/internal/agetrack"
	"freshness-companion/internal/decisionlog"
	"freshness-companion/internal/evidence"
)

// pageProvider serves a settable page per URL and counts fetches.
type pageProvider struct {
	mu    sync.Mutex
	pages map[string]string
	err   error
	calls atomic.Int32
	clock *fakeClock
}

func (p *pageProvider) Name() string { return "test_provider" }

func (p *pageProvider) Fetch(_ context.Context, url string) (evidence.Evidence, error) {
	p.calls.Add(1)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return evidence.Evidence{}, p.err
	}
	return evidence.Evidence{URL: url, Content: p.pages[url], FetchedAt: p.clock.Now()}, nil
}

func (p *pageProvider) set(url, content string) {
	p.mu.Lock()
	p.pages[url] = content
	p.mu.Unlock()
}

// fakeClearer records deletes and can fail for chosen ids.
type fakeClearer struct {
	mu      sync.Mutex
	cleared []string
	fail    map[string]bool
}

func (c *fakeClearer) Clear(_ context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail[id] {
		return errors.New("Bifrost answered HTTP 500")
	}
	c.cleared = append(c.cleared, id)
	return nil
}

func (c *fakeClearer) ids() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.cleared...)
}

// rig wires a schedule, cache, index, ages, clearer, log and watcher on one clock.
type rig struct {
	clock    *fakeClock
	provider *pageProvider
	cache    *evidence.Cache
	sched    *Schedule
	index    *Index
	ages     *agetrack.Tracker
	clearer  *fakeClearer
	dlog     *decisionlog.Log
	w        *Watcher
}

func newRig(t *testing.T, enabled bool, tweak func(*Options)) *rig {
	t.Helper()
	clock := newClock()
	provider := &pageProvider{pages: map[string]string{srcURL: page("49", 5), hoursURL: "Support: 9-5."}, clock: clock}
	sched, err := NewSchedule(Config{Floor: 5 * time.Second, Max: 60 * time.Second, Growth: 2, Adaptive: true, Now: clock.Now}, testPol(t).Facts)
	if err != nil {
		t.Fatal(err)
	}
	cache := evidence.NewCache(provider, evidence.CacheConfig{TTL: 5 * time.Second, ErrorTTL: time.Second, Now: clock.Now, Schedule: sched})
	ages, _ := agetrack.Open(agetrack.Options{Now: clock.Now})
	dlog, _ := decisionlog.Open("", 100)
	clearer := &fakeClearer{fail: map[string]bool{}}
	index := NewIndex(0)
	dlog.Subscribe(index.Learn)
	o := Options{Schedule: sched, Cache: cache, Index: index, Ages: ages, Clearer: clearer, Decisions: dlog,
		Enabled: enabled, MaxPerHour: 100, Idle: 30 * time.Minute, Evict: true, Provider: "test_provider", Now: clock.Now, Tick: time.Millisecond}
	if tweak != nil {
		tweak(&o)
	}
	w, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return &rig{clock: clock, provider: provider, cache: cache, sched: sched, index: index, ages: ages, clearer: clearer, dlog: dlog, w: w}
}

// ask simulates a request path lookup of the price source.
func (r *rig) ask(t *testing.T) evidence.Result {
	t.Helper()
	res, err := r.cache.Get(context.Background(), srcURL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return res
}

// served records that a cached answer stating value was served from id.
func (r *rig) served(id, value string) {
	_ = r.ages.RecordMiss(id)
	_ = r.dlog.Append(decisionlog.Record{Time: r.clock.Now(), AnswerOf: "cached", Action: "served", Fact: "Acme Pro plan price", Category: "price", CacheID: id, CachedValue: value})
}

func (r *rig) watcherRecords() []decisionlog.Record {
	var out []decisionlog.Record
	for _, rec := range r.dlog.Recent(0) {
		if rec.AnswerOf == "watcher" {
			out = append(out, rec)
		}
	}
	return out
}

func TestTickChecksOnlyDueActiveSources(t *testing.T) {
	r := newRig(t, true, nil)
	ctx := context.Background()
	// Nothing has been asked: the watcher never fetches on its own.
	r.w.Tick(ctx)
	if r.provider.calls.Load() != 0 {
		t.Fatalf("a source nobody asked about was fetched (%d calls)", r.provider.calls.Load())
	}
	r.ask(t) // 1 fetch; the hours page is still never used
	r.w.Tick(ctx)
	if r.provider.calls.Load() != 1 {
		t.Fatalf("just-fetched evidence must not be refreshed: %d calls", r.provider.calls.Load())
	}
	r.clock.Advance(5 * time.Second) // interval 5 s, lead 1 s: due
	r.w.Tick(ctx)
	if r.provider.calls.Load() != 2 {
		t.Fatalf("a due active source must be refreshed: %d calls", r.provider.calls.Load())
	}
	for _, st := range r.w.Sources() {
		if st.SourceURL == hoursURL && (st.Active || st.Checks != 0) {
			t.Fatalf("the never-used hours source must stay inactive and unchecked: %+v", st)
		}
	}
	// Once nobody has asked for -watch-idle, the source goes idle.
	r.clock.Advance(31 * time.Minute)
	before := r.provider.calls.Load()
	r.w.Tick(ctx)
	if r.provider.calls.Load() != before {
		t.Fatal("an idle source must not be checked")
	}
	if st, _ := r.sched.Source(srcURL); r.w.Sources()[0].Active || st.Checks != 2 {
		t.Fatalf("source should be inactive: %+v", r.w.Sources()[0])
	}
}

func TestTickChecksBeforeExpiry(t *testing.T) {
	r := newRig(t, true, nil)
	ctx := context.Background()
	r.ask(t)
	// Grow the interval to 20 s (lead = 4 s) with unchanged checks.
	for i := 0; i < 2; i++ {
		r.clock.Advance(r.sched.TTL(srcURL))
		r.ask(t)
	}
	if r.sched.TTL(srcURL) != 20*time.Second {
		t.Fatalf("TTL = %s, want 20 s", r.sched.TTL(srcURL))
	}
	calls := r.provider.calls.Load()
	r.clock.Advance(15 * time.Second) // 5 s left > lead 4 s
	r.w.Tick(ctx)
	if r.provider.calls.Load() != calls {
		t.Fatal("not yet within the lead: no fetch expected")
	}
	r.clock.Advance(time.Second) // 4 s left = lead
	r.w.Tick(ctx)
	if r.provider.calls.Load() != calls+1 {
		t.Fatal("within the lead the watcher must refresh")
	}
	// The request that follows finds fresh evidence and does not wait.
	r.clock.Advance(3 * time.Second)
	if res := r.ask(t); !res.Cached {
		t.Fatal("the request should reuse the evidence the watcher fetched")
	}
	s := r.cache.Stats()
	if s.Refreshes != 1 || s.Waited != 3 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestTickRespectsHourlyCap(t *testing.T) {
	r := newRig(t, true, func(o *Options) { o.MaxPerHour = 2 })
	ctx := context.Background()
	r.ask(t)
	// Ticks every 5 s for 100 s. The interval grows 5 -> 10 -> 20 s, so the
	// watcher checks at +5 s and +15 s; from +35 s the source is due again
	// on every tick, and every one of those is over the cap.
	for i := 0; i < 20; i++ {
		r.clock.Advance(5 * time.Second)
		r.w.Tick(ctx)
	}
	if got := r.provider.calls.Load(); got != 3 { // 1 request + 2 watcher
		t.Fatalf("provider calls = %d, want 3", got)
	}
	st := r.w.Stats()
	if st.SkippedOverBudget != 14 || st.UsedLastHour != 2 || st.MaxPerHour != 2 || st.Checks != 2 {
		t.Fatalf("stats = %+v", st)
	}
	// The request path still fetches when the evidence has expired.
	r.clock.Advance(5 * time.Second)
	if res := r.ask(t); res.Cached {
		t.Fatal("expired evidence must be fetched by the request, cap or no cap")
	}
}

func TestChangeEvictsOnlyOldValueEntries(t *testing.T) {
	r := newRig(t, true, nil)
	ctx := context.Background()
	r.ask(t)
	r.served("old-1", "49")
	r.served("old-2", "$49.00")
	r.served("already-new", "59")
	r.clock.Advance(5 * time.Second)
	r.provider.set(srcURL, page("59", 5))
	r.w.Tick(ctx)
	if n := r.w.Drain(ctx); n != 1 {
		t.Fatalf("drained %d changes, want 1", n)
	}
	got := r.clearer.ids()
	if len(got) != 2 || got[0] != "old-1" || got[1] != "old-2" {
		t.Fatalf("cleared %v, want old-1 and old-2 only", got)
	}
	if r.index.Count("Acme Pro plan price") != 1 {
		t.Fatalf("index should keep only already-new: %v", r.index.Entries("Acme Pro plan price"))
	}
	if _, ok := r.ages.Age("old-1"); ok {
		t.Fatal("evicted ids must be forgotten in the age store")
	}
	recs := r.watcherRecords()
	if len(recs) != 1 {
		t.Fatalf("watcher records = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec.Outcome != "changed" || rec.Action != "evicted" || rec.CachedValue != "49" || rec.CurrentValue != "59" || rec.Fact != "Acme Pro plan price" || rec.Category != "price" || rec.SourceURL != srcURL || rec.Provider != "test_provider" || rec.CheckedAt == nil || len(rec.Evicted) != 2 {
		t.Fatalf("record = %+v", rec)
	}
	if r.w.Stats().Evictions != 2 {
		t.Fatalf("evictions = %d", r.w.Stats().Evictions)
	}
	// Idempotent: the same page again is not a change.
	r.clock.Advance(5 * time.Second)
	r.w.Tick(ctx)
	if r.w.Drain(ctx) != 0 || len(r.watcherRecords()) != 1 {
		t.Fatal("an unchanged check must not produce another record")
	}
}

func TestChangeSkipsEntriesRewrittenAfterChange(t *testing.T) {
	r := newRig(t, true, nil)
	ctx := context.Background()
	r.ask(t)
	r.served("same-id", "49")
	r.clock.Advance(5 * time.Second)
	r.provider.set(srcURL, page("59", 5))
	r.w.Tick(ctx) // the change is seen now and queued
	// Before the worker runs, a request refreshes the entry under the same
	// deterministic id (Bifrost rewrote it with the new answer).
	r.clock.Advance(time.Second)
	_ = r.ages.RecordMiss("same-id")
	r.w.Drain(ctx)
	if len(r.clearer.ids()) != 0 {
		t.Fatalf("an entry rewritten after the change must not be deleted: %v", r.clearer.ids())
	}
	recs := r.watcherRecords()
	if len(recs) != 1 || recs[0].Action != "noted" || !strings.Contains(recs[0].Reason, "1 written after the change were kept") {
		t.Fatalf("record = %+v", recs)
	}
}

func TestRequestPathChangeAlsoEvicts(t *testing.T) {
	r := newRig(t, true, nil)
	ctx := context.Background()
	r.ask(t)
	r.served("old-1", "49")
	r.clock.Advance(6 * time.Second)
	r.provider.set(srcURL, page("59", 5))
	if res := r.ask(t); res.Cached { // the request itself fetches and finds the change
		t.Fatal("expected a fresh fetch")
	}
	r.w.Drain(ctx)
	if ids := r.clearer.ids(); len(ids) != 1 || ids[0] != "old-1" {
		t.Fatalf("cleared %v, want old-1", ids)
	}
	if r.cache.Stats().Refreshes != 0 {
		t.Fatal("the watcher did not fetch anything here")
	}
}

func TestNoEvictionWithoutReadableNewValue(t *testing.T) {
	r := newRig(t, true, nil)
	ctx := context.Background()
	r.ask(t)
	r.served("old-1", "49")
	r.clock.Advance(5 * time.Second)
	r.provider.set(srcURL, "The page was redesigned and says nothing about prices.")
	r.w.Tick(ctx)
	r.w.Drain(ctx)
	if len(r.clearer.ids()) != 0 {
		t.Fatalf("nothing may be evicted when the new value is unreadable: %v", r.clearer.ids())
	}
	recs := r.watcherRecords()
	// Two facts read this page, both became unreadable: one noted record each.
	if len(recs) != 2 {
		t.Fatalf("records = %d, want 2", len(recs))
	}
	for _, rec := range recs {
		if rec.Action != "noted" || rec.CurrentValue != "" || len(rec.Evicted) != 0 {
			t.Fatalf("record = %+v", rec)
		}
	}
	// Eviction off: the change is noted, entries stay.
	r2 := newRig(t, true, func(o *Options) { o.Evict = false })
	r2.ask(t)
	r2.served("old-1", "49")
	r2.clock.Advance(5 * time.Second)
	r2.provider.set(srcURL, page("59", 5))
	r2.w.Tick(ctx)
	r2.w.Drain(ctx)
	if len(r2.clearer.ids()) != 0 || len(r2.watcherRecords()) != 1 || r2.watcherRecords()[0].Action != "noted" {
		t.Fatalf("with -watch-evict=false: cleared=%v records=%+v", r2.clearer.ids(), r2.watcherRecords())
	}
}

func TestEvictionErrorRecordedNotFatal(t *testing.T) {
	r := newRig(t, true, nil)
	ctx := context.Background()
	r.ask(t)
	r.served("bad", "49")
	r.served("good", "49")
	r.clearer.fail["bad"] = true
	r.clock.Advance(5 * time.Second)
	r.provider.set(srcURL, page("59", 5))
	r.w.Tick(ctx)
	r.w.Drain(ctx)
	if ids := r.clearer.ids(); len(ids) != 1 || ids[0] != "good" {
		t.Fatalf("cleared %v, want good only", ids)
	}
	rec := r.watcherRecords()[0]
	if rec.Action != "evicted" || len(rec.Evicted) != 1 || !strings.Contains(rec.Error, "HTTP 500") {
		t.Fatalf("record = %+v", rec)
	}
	if r.index.Count("Acme Pro plan price") != 1 {
		t.Fatal("the entry that could not be cleared stays indexed for the next change")
	}
	if r.w.Stats().EvictionErrors != 1 {
		t.Fatalf("stats = %+v", r.w.Stats())
	}
	// The watcher keeps working afterwards.
	r.clock.Advance(5 * time.Second)
	r.w.Tick(ctx)
	if r.w.Stats().Checks != 2 {
		t.Fatalf("checks = %d", r.w.Stats().Checks)
	}
}

func TestCheckNowErrors(t *testing.T) {
	off := newRig(t, false, nil)
	ctx := context.Background()
	if _, err := off.w.CheckNow(ctx, srcURL); !errors.Is(err, ErrWatcherOff) {
		t.Fatalf("watcher off: %v", err)
	}
	off.w.Tick(ctx) // no-op
	if off.provider.calls.Load() != 0 {
		t.Fatal("a disabled watcher must not fetch")
	}

	r := newRig(t, true, func(o *Options) { o.MaxPerHour = 1 })
	if _, err := r.w.CheckNow(ctx, "http://unknown/"); !errors.Is(err, ErrUnknownSource) {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := r.w.CheckNow(ctx, "no such fact"); !errors.Is(err, ErrUnknownSource) {
		t.Fatalf("unknown fact: %v", err)
	}
	// By fact name; nobody asked before, so this is the first check (no change).
	st, err := r.w.CheckNow(ctx, "Acme Pro plan price")
	if err != nil || st.Changed || st.Checks != 1 || st.Values["Acme Pro plan price"] != "49" {
		t.Fatalf("CheckNow = %+v, %v", st, err)
	}
	if _, err := r.w.CheckNow(ctx, srcURL); !errors.Is(err, ErrOverBudget) {
		t.Fatalf("over budget: %v", err)
	}
	// With budget, a change is reported and applied synchronously.
	r2 := newRig(t, true, nil)
	r2.ask(t)
	r2.served("old-1", "49")
	r2.provider.set(srcURL, page("59", 5))
	st, err = r2.w.CheckNow(ctx, srcURL)
	if err != nil || !st.Changed || st.Values["Acme Pro plan price"] != "59" || st.KnownEntries != 0 || !st.Active {
		t.Fatalf("CheckNow after a change = %+v, %v", st, err)
	}
	if ids := r2.clearer.ids(); len(ids) != 1 {
		t.Fatalf("CheckNow must apply the eviction before returning: %v", ids)
	}
	// A fetch error returns the state and the error.
	r2.provider.err = errors.New("connection refused")
	st, err = r2.w.CheckNow(ctx, srcURL)
	if err == nil || st.Errors != 1 || !strings.Contains(st.LastError, "connection refused") {
		t.Fatalf("fetch error: %+v, %v", st, err)
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	r := newRig(t, true, nil)
	r.ask(t)
	r.served("old-1", "49")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.w.Run(ctx); close(done) }()
	// Real time drives the ticker; the fake clock makes the source due.
	r.clock.Advance(5 * time.Second)
	r.provider.set(srcURL, page("59", 5))
	deadline := time.After(5 * time.Second)
	for len(r.clearer.ids()) == 0 {
		select {
		case <-deadline:
			t.Fatal("Run never checked the due source and evicted the old answer")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestChangeQueueDropsWhenFull(t *testing.T) {
	r := newRig(t, true, nil)
	for i := 0; i < changeQueue+3; i++ {
		r.w.OnChange(Change{SourceURL: srcURL})
	}
	if r.w.Stats().ChangesDropped != 3 || r.w.Stats().ChangesSeen != int64(changeQueue+3) {
		t.Fatalf("stats = %+v", r.w.Stats())
	}
}

func newServer(r *rig, token string) *httptest.Server {
	mux := http.NewServeMux()
	(&Server{Schedule: r.sched, Watcher: r.w, Stats: r.cache.Stats, Token: token, Now: r.clock.Now}).Register(mux)
	return httptest.NewServer(mux)
}

func TestWatchEndpointShape(t *testing.T) {
	r := newRig(t, true, nil)
	r.ask(t)
	r.served("old-1", "49")
	ts := newServer(r, "")
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/freshness/watch")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"adaptive", "floor_seconds", "max_seconds", "growth", "watcher", "evidence", "sources"} {
		if _, ok := body[k]; !ok {
			t.Fatalf("status lacks %q: %v", k, body)
		}
	}
	if body["adaptive"] != true || body["floor_seconds"] != 5.0 || body["max_seconds"] != 60.0 || body["growth"] != 2.0 {
		t.Fatalf("top level = %v", body)
	}
	wt := body["watcher"].(map[string]any)
	for _, k := range []string{"enabled", "max_per_hour", "used_last_hour", "skipped_over_budget", "idle_after_seconds", "evictions", "changes_dropped"} {
		if _, ok := wt[k]; !ok {
			t.Fatalf("watcher lacks %q: %v", k, wt)
		}
	}
	if wt["enabled"] != true || wt["max_per_hour"] != 100.0 || wt["idle_after_seconds"] != 1800.0 {
		t.Fatalf("watcher = %v", wt)
	}
	ev := body["evidence"].(map[string]any)
	for _, k := range []string{"lookups", "reused", "waited", "provider_calls", "provider_errors", "refreshes"} {
		if _, ok := ev[k]; !ok {
			t.Fatalf("evidence lacks %q: %v", k, ev)
		}
	}
	if ev["lookups"] != 1.0 || ev["provider_calls"] != 1.0 {
		t.Fatalf("evidence = %v", ev)
	}
	sources := body["sources"].([]any)
	if len(sources) != 2 {
		t.Fatalf("sources = %v", sources)
	}
	src := sources[0].(map[string]any)
	for _, k := range []string{"source_url", "facts", "values", "interval_seconds", "last_checked_at", "next_check_at", "unchanged_streak", "checks", "changes", "errors", "last_error", "last_used_at", "active", "known_entries"} {
		if _, ok := src[k]; !ok {
			t.Fatalf("source lacks %q: %v", k, src)
		}
	}
	if src["source_url"] != srcURL || src["active"] != true || src["known_entries"] != 1.0 || src["interval_seconds"] != 5.0 {
		t.Fatalf("source = %v", src)
	}
	if _, has := src["changed"]; has {
		t.Fatal("the listing must not carry the CheckNow-only field changed")
	}
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), "How much") || strings.Contains(string(raw), "costs $") {
		t.Fatal("the status must hold values only, never prompt or answer text")
	}

	// Without a schedule or watcher the endpoint still answers, with everything off.
	mux := http.NewServeMux()
	(&Server{Stats: r.cache.Stats}).Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/freshness/watch", nil))
	var off Status
	if err := json.Unmarshal(rec.Body.Bytes(), &off); err != nil || off.Adaptive || off.Watcher.Enabled || len(off.Sources) != 0 || off.Evidence.Lookups != 1 {
		t.Fatalf("flags-off status = %s (%v)", rec.Body.String(), err)
	}
}

func TestCheckEndpointNeedsToken(t *testing.T) {
	r := newRig(t, true, nil)
	ts := newServer(r, "s3cret")
	defer ts.Close()
	post := func(body, auth string) (*http.Response, map[string]any) {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/freshness/watch/check", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp, out
	}
	if resp, _ := post(`{"source_url":"`+srcURL+`"}`, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
	if resp, _ := post(`{"source_url":"`+srcURL+`"}`, "Bearer wrong"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", resp.StatusCode)
	}
	if resp, _ := post(`{"source_url":"a","fact":"b"}`, "Bearer s3cret"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("two selectors: %d", resp.StatusCode)
	}
	if resp, _ := post(`{"fact":"nope"}`, "Bearer s3cret"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown fact: %d", resp.StatusCode)
	}
	resp, out := post(`{"fact":"Acme Pro plan price"}`, "Bearer s3cret")
	if resp.StatusCode != http.StatusOK || out["source_url"] != srcURL || out["checks"] != 1.0 {
		t.Fatalf("check: %d %v", resp.StatusCode, out)
	}
	if _, has := out["changed"]; has {
		t.Fatal("first check: changed must be absent (false)")
	}
	r.provider.set(srcURL, page("59", 5))
	resp, out = post(`{"source_url":"`+srcURL+`"}`, "Bearer s3cret")
	if resp.StatusCode != http.StatusOK || out["changed"] != true {
		t.Fatalf("changed check: %d %v", resp.StatusCode, out)
	}

	// Over the cap: 429. Watcher off: 409.
	capped := newRig(t, true, func(o *Options) { o.MaxPerHour = 0 })
	ts2 := newServer(capped, "")
	defer ts2.Close()
	resp2, _ := http.Post(ts2.URL+"/freshness/watch/check", "application/json", strings.NewReader(`{"source_url":"`+srcURL+`"}`))
	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("over cap: %d", resp2.StatusCode)
	}
	off := newRig(t, false, nil)
	ts3 := newServer(off, "")
	defer ts3.Close()
	resp3, _ := http.Post(ts3.URL+"/freshness/watch/check", "application/json", strings.NewReader(`{"source_url":"`+srcURL+`"}`))
	if resp3.StatusCode != http.StatusConflict {
		t.Fatalf("watcher off: %d", resp3.StatusCode)
	}
}
