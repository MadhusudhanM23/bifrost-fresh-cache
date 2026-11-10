package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strconv"
	"time"

	"freshness-companion/internal/agetrack"
	"freshness-companion/internal/decision"
	"freshness-companion/internal/decisionlog"
	"freshness-companion/internal/evidence"
	"freshness-companion/internal/policy"
	"freshness-companion/internal/watch"
)

// Arm is one checking configuration under test.
type Arm struct {
	Name            string        `json:"name"`
	Floor           time.Duration `json:"floor"`
	Max             time.Duration `json:"max"`
	Growth          float64       `json:"growth"`
	Adaptive        bool          `json:"adaptive"`
	Watch           bool          `json:"watch"`
	WatchMaxPerHour int           `json:"watch_max_per_hour"`
	WatchIdle       time.Duration `json:"watch_idle"`
	// Notices: the system that owns the page posts a change notice at the
	// moment of every change (cache.Invalidate, as POST /freshness/changes does).
	Notices bool `json:"notices"`
}

// Trace is a traffic and page-change scenario.
type Trace struct {
	Name              string          `json:"name"`
	Description       string          `json:"description"`
	Duration          time.Duration   `json:"duration"`
	RequestsPerMinute float64         `json:"requests_per_minute"`
	Changes           []time.Duration `json:"changes"` // offsets from the start
	Questions         int             `json:"questions"`
}

// Metrics is what one (trace, arm) run measured.
type Metrics struct {
	Trace string `json:"trace"`
	Arm   string `json:"arm"`

	Requests int `json:"requests"`
	Hits     int `json:"hits"`
	Misses   int `json:"misses"`

	EvidenceFetches int     `json:"evidence_fetches"`
	RequestFetches  int     `json:"request_fetches"`
	WatcherFetches  int     `json:"watcher_fetches"`
	FetchesPerHour  float64 `json:"fetches_per_hour"`
	RequestsWaited  int     `json:"requests_waited"`

	StaleServed  int     `json:"stale_served"`
	StalePercent float64 `json:"stale_percent"`
	Refreshed    int     `json:"refreshed"`
	Corrected    int     `json:"corrected"`
	Evictions    int     `json:"evictions"`
	BudgetSkips  int     `json:"budget_skips"`

	Changes        int     `json:"changes"`
	Detected       int     `json:"detected"`
	WorstLagSecs   float64 `json:"worst_detection_lag_seconds"`
	MeanLagSecs    float64 `json:"mean_detection_lag_seconds"`
	FinalIntervalS float64 `json:"final_interval_seconds"`
}

const sourceURL = "http://demoserver:8000/"

const simPolicy = `{
  "version": 1,
  "categories": [
    {"name": "price", "keywords": ["price", "cost", "per month", "plan"], "max_age": "10m",
     "on_unverifiable": "bypass", "verify_new_answers": true, "on_contradicted": "correct"}
  ],
  "facts": [
    {"name": "Acme Pro plan price", "category": "price", "source_url": "http://demoserver:8000/",
     "source_pattern": "Acme Pro plan[^$0-9]{0,20}\\$\\s*([0-9][0-9,]*(?:\\.[0-9]+)?)\\s*per month",
     "answer_pattern": "(?i)\\$\\s*([0-9][0-9,]*(?:\\.[0-9]+)?)\\s*(?:/\\s*mo(?:nth)?|per\\s+month|a\\s+month|monthly)\\b",
     "compare": "numeric"}
  ]
}`

// DefaultArms are the five configurations the plan asks for, plus the
// adaptive arm with the docker stack's 60 s cap (CHECK_MAX default).
func DefaultArms() []Arm {
	adaptive := Arm{Name: "adaptive", Floor: 5 * time.Second, Max: 5 * time.Minute, Growth: 2, Adaptive: true}
	cap60 := adaptive
	cap60.Name, cap60.Max = "adaptive-cap60s", 60*time.Second
	withWatch := adaptive
	withWatch.Name, withWatch.Watch, withWatch.WatchMaxPerHour, withWatch.WatchIdle = "adaptive+watch", true, 120, 30*time.Minute
	withNotices := withWatch
	withNotices.Name, withNotices.Notices = "adaptive+watch+notices", true
	return []Arm{
		{Name: "fixed-5s", Floor: 5 * time.Second, Max: 5 * time.Second, Growth: 1},
		{Name: "fixed-60s", Floor: 60 * time.Second, Max: 60 * time.Second, Growth: 1},
		adaptive,
		cap60,
		withWatch,
		withNotices,
	}
}

// DefaultTraces: quiet (one change in 24 h), sparse (same, little traffic)
// and sale (a change every 10 minutes for 2 h).
func DefaultTraces() []Trace {
	sale := Trace{Name: "sale", Description: "2 h, 6 requests/min, the price changes every 10 min (11 changes)", Duration: 2 * time.Hour, RequestsPerMinute: 6, Questions: 3}
	for m := 10; m <= 110; m += 10 {
		sale.Changes = append(sale.Changes, time.Duration(m)*time.Minute)
	}
	return []Trace{
		{Name: "quiet", Description: "24 h, 3 requests/min, one price change at hour 12", Duration: 24 * time.Hour, RequestsPerMinute: 3, Changes: []time.Duration{12 * time.Hour}, Questions: 3},
		{Name: "sparse", Description: "24 h, 1 request every 5 min, one price change at hour 12", Duration: 24 * time.Hour, RequestsPerMinute: 0.2, Changes: []time.Duration{12 * time.Hour}, Questions: 3},
		sale,
	}
}

// vclock is the virtual clock every component reads.
type vclock struct{ t time.Time }

func (c *vclock) Now() time.Time { return c.t }

// traceProvider serves the page for the value in force at the virtual time
// and remembers when each value was first seen by a fetch.
type traceProvider struct {
	clock     *vclock
	value     func(time.Time) int
	firstSeen map[int]time.Time
}

func (p *traceProvider) Name() string { return "sim_provider" }

func (p *traceProvider) Fetch(_ context.Context, url string) (evidence.Evidence, error) {
	now := p.clock.Now()
	v := p.value(now)
	if _, ok := p.firstSeen[v]; !ok {
		p.firstSeen[v] = now
	}
	return evidence.Evidence{URL: url, Content: "Acme pricing. Acme Pro plan — $" + strconv.Itoa(v) + " per month.", Provider: "sim_provider", FetchedAt: now}, nil
}

// entry is a Bifrost cache entry: the answer's stated value and write time.
type entry struct {
	value   int
	written time.Time
}

// fakeBifrost holds one deterministic entry per question, like Bifrost's
// direct-hit ids.
type fakeBifrost struct{ entries map[string]*entry }

func (b *fakeBifrost) Clear(_ context.Context, id string) error {
	delete(b.entries, id)
	return nil
}

type simulation struct {
	arm   Arm
	trace Trace
	rng   *rand.Rand
	clock *vclock
	start time.Time

	provider *traceProvider
	cache    *evidence.Cache
	sched    *watch.Schedule
	watcher  *watch.Watcher
	ages     *agetrack.Tracker
	dlog     *decisionlog.Log
	engine   *decision.Engine
	eff      policy.Effective
	bifrost  *fakeBifrost
	m        Metrics
}

// Run simulates trace under arm with a deterministic seed.
func Run(trace Trace, arm Arm, seed int64) (Metrics, error) {
	pol, err := policy.Parse([]byte(simPolicy))
	if err != nil {
		return Metrics{}, err
	}
	eff, ok := pol.Resolve("How much does the Acme Pro plan cost per month?")
	if !ok || len(eff.Facts) != 1 {
		return Metrics{}, fmt.Errorf("the simulation question did not resolve to the price fact")
	}
	start := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	s := &simulation{arm: arm, trace: trace, rng: rand.New(rand.NewSource(seed)), clock: &vclock{t: start}, start: start, eff: eff,
		bifrost: &fakeBifrost{entries: map[string]*entry{}}, m: Metrics{Trace: trace.Name, Arm: arm.Name, Changes: len(trace.Changes)}}
	s.provider = &traceProvider{clock: s.clock, value: s.trueValue, firstSeen: map[int]time.Time{}}
	s.sched, err = watch.NewSchedule(watch.Config{Floor: arm.Floor, Max: arm.Max, Growth: arm.Growth, Adaptive: arm.Adaptive, Now: s.clock.Now}, pol.Facts)
	if err != nil {
		return Metrics{}, err
	}
	s.cache = evidence.NewCache(s.provider, evidence.CacheConfig{TTL: arm.Floor, ErrorTTL: time.Second, Now: s.clock.Now, Schedule: s.sched})
	s.ages, _ = agetrack.Open(agetrack.Options{Now: s.clock.Now})
	s.dlog, _ = decisionlog.Open("", 16)
	s.engine = &decision.Engine{Evidence: s.cache}
	if arm.Watch {
		s.watcher, err = watch.New(watch.Options{Schedule: s.sched, Cache: s.cache, Ages: s.ages, Clearer: s.bifrost, Decisions: s.dlog,
			Enabled: true, MaxPerHour: arm.WatchMaxPerHour, Idle: arm.WatchIdle, Evict: true, Provider: "sim_provider", Now: s.clock.Now})
		if err != nil {
			return Metrics{}, err
		}
		s.dlog.Subscribe(s.watcher.Index().Learn)
	}
	s.run()
	return s.m, nil
}

// trueValue is the price the page states at t: 49, then +10 per change.
func (s *simulation) trueValue(t time.Time) int {
	v := 49
	for _, off := range s.trace.Changes {
		if !t.Before(s.start.Add(off)) {
			v += 10
		}
	}
	return v
}

func (s *simulation) run() {
	ctx := context.Background()
	steps := int(s.trace.Duration / time.Second)
	// Poisson arrivals, rounded down to whole seconds.
	arrivals := make([]int, 0, int(s.trace.RequestsPerMinute*s.trace.Duration.Minutes())+16)
	rate := s.trace.RequestsPerMinute / 60
	for t := 0.0; rate > 0; {
		t += s.rng.ExpFloat64() / rate
		if t >= float64(steps) {
			break
		}
		arrivals = append(arrivals, int(t))
	}
	changeAt := map[int]bool{}
	for _, off := range s.trace.Changes {
		changeAt[int(off/time.Second)] = true
	}
	questions := max(1, s.trace.Questions)
	next := 0
	for sec := 0; sec < steps; sec++ {
		s.clock.t = s.start.Add(time.Duration(sec) * time.Second)
		if changeAt[sec] && s.arm.Notices {
			s.cache.Invalidate(sourceURL)
		}
		for next < len(arrivals) && arrivals[next] == sec {
			s.request(ctx, "q-"+strconv.Itoa(s.rng.Intn(questions)))
			next++
		}
		if s.watcher != nil {
			s.watcher.Tick(ctx)
			s.watcher.Drain(ctx)
		}
	}
	s.finish()
}

func body(value int) []byte {
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{
		"role": "assistant", "content": "The Acme Pro plan costs $" + strconv.Itoa(value) + " per month."}}}})
	return b
}

// request plays one chat request through the fake Bifrost and the real
// decision engine, exactly as the proxy would: a miss writes a new answer
// (checked before reuse), a hit is decided against evidence.
func (s *simulation) request(ctx context.Context, id string) {
	now := s.clock.Now()
	truth := s.trueValue(now)
	before := s.cache.Stats()
	s.m.Requests++
	e := s.bifrost.entries[id]
	if e == nil {
		s.m.Misses++
		// The model answers from its (current) context: the true value.
		e = &entry{value: truth, written: now}
		s.bifrost.entries[id] = e
		_ = s.ages.RecordMiss(id)
		d := s.engine.CheckNewAnswer(ctx, s.eff, body(truth))
		rec := s.record(d, id)
		rec.AnswerOf = "new"
		rec.Action = "served"
		if d.Outcome == decision.Contradicted {
			// The evidence is older than the page: the proxy "corrects" the
			// (actually right) answer to the evidence value. That is a real
			// cost of stale evidence, so the model reproduces it.
			if cur, err := strconv.Atoi(d.CurrentValue); err == nil {
				e.value = cur
				rec.Action, rec.Correction = "corrected", "corrected"
				s.m.Corrected++
			}
		}
		_ = s.dlog.Append(rec)
		s.served(e.value, truth)
	} else {
		s.m.Hits++
		age := now.Sub(e.written)
		d := s.engine.Decide(ctx, decision.Input{Policy: s.eff, CacheID: id, Age: age, AgeKnown: true, CachedBody: body(e.value)})
		rec := s.record(d, id)
		rec.AnswerOf = "cached"
		switch d.Action {
		case decision.Serve, decision.ServeWithWarning:
			rec.Action = "served"
			s.served(e.value, truth)
		default: // Refresh: clear and re-ask; the model answers the true value
			s.m.Refreshed++
			e.value, e.written = truth, now
			_ = s.ages.RecordMiss(id)
			rec.Action, rec.RefreshStrategy, rec.RefreshedCacheID = "refreshed", "clear_and_resend", id
			value, check, ok := decision.CheckRefreshed(d, body(truth))
			if ok {
				rec.RefreshedCheck = check
				if check == decision.RefreshedDiffers {
					// Same correction rule as the proxy with on_contradicted=correct.
					if cur, err := strconv.Atoi(d.CurrentValue); err == nil {
						e.value = cur
						rec.Action, rec.Correction, rec.CorrectedValue = "corrected", "corrected", value
						s.m.Corrected++
					}
				}
			}
			s.served(e.value, truth)
		}
		_ = s.dlog.Append(rec)
	}
	after := s.cache.Stats()
	if after.Waited > before.Waited {
		s.m.RequestsWaited++
	}
}

func (s *simulation) served(value, truth int) {
	if value != truth {
		s.m.StaleServed++
	}
}

func (s *simulation) record(d decision.Decision, id string) decisionlog.Record {
	rec := decisionlog.Record{Time: s.clock.Now(), CacheID: id, Category: "price", Fact: d.Fact, Outcome: string(d.Outcome), Reason: d.Reason,
		SourceURL: d.SourceURL, Provider: d.Provider, CachedValue: d.CachedValue, CurrentValue: d.CurrentValue, EvidenceCached: d.EvidenceCached}
	if !d.CheckedAt.IsZero() {
		t := d.CheckedAt
		rec.CheckedAt = &t
	}
	return rec
}

func (s *simulation) finish() {
	st := s.cache.Stats()
	s.m.EvidenceFetches = int(st.ProviderCalls)
	s.m.WatcherFetches = int(st.Refreshes)
	s.m.RequestFetches = s.m.EvidenceFetches - s.m.WatcherFetches
	s.m.FetchesPerHour = float64(s.m.EvidenceFetches) / s.trace.Duration.Hours()
	if s.m.Requests > 0 {
		s.m.StalePercent = 100 * float64(s.m.StaleServed) / float64(s.m.Requests)
	}
	if s.watcher != nil {
		ws := s.watcher.Stats()
		s.m.Evictions = int(ws.Evictions)
		s.m.BudgetSkips = int(ws.SkippedOverBudget)
	}
	var lags []float64
	for i, off := range s.trace.Changes {
		newValue := 49 + 10*(i+1)
		seen, ok := s.provider.firstSeen[newValue]
		if !ok {
			continue
		}
		s.m.Detected++
		lags = append(lags, seen.Sub(s.start.Add(off)).Seconds())
	}
	if len(lags) > 0 {
		sort.Float64s(lags)
		s.m.WorstLagSecs = lags[len(lags)-1]
		sum := 0.0
		for _, l := range lags {
			sum += l
		}
		s.m.MeanLagSecs = math.Round(sum/float64(len(lags))*10) / 10
	}
	if src, ok := s.sched.Source(sourceURL); ok {
		s.m.FinalIntervalS = src.IntervalSeconds
	}
}
