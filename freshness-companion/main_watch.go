package main

// Smart checking (-check-adaptive) and the background watcher (-watch).
// Both are off by default; with the defaults the companion behaves exactly
// as it did before these flags existed: a fixed -evidence-ttl per source
// and no background fetches. main.go touches this file at three points:
// addWatchFlags before Parse, newWatchSetup + cacheConfig around the
// evidence cache, and wire + run around the HTTP server.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"freshness-companion/internal/agetrack"
	"freshness-companion/internal/decisionlog"
	"freshness-companion/internal/evidence"
	"freshness-companion/internal/policy"
	"freshness-companion/internal/watch"
)

type watchFlags struct {
	adaptive   *bool
	checkMax   *time.Duration
	growth     *float64
	watch      *bool
	maxPerHour *int
	idle       *time.Duration
	evict      *bool
}

func addWatchFlags(fs *flag.FlagSet) *watchFlags {
	return &watchFlags{
		adaptive:   fs.Bool("check-adaptive", false, "smart checking: a source that keeps saying the same thing is checked less often, from -evidence-ttl up to -check-max; any change resets it (needs -evidence-ttl > 0)"),
		checkMax:   fs.Duration("check-max", 5*time.Minute, "longest re-check interval with -check-adaptive (must be >= -evidence-ttl)"),
		growth:     fs.Float64("check-growth", 2, "with -check-adaptive, multiply the interval by this after an unchanged check (> 1)"),
		watch:      fs.Bool("watch", false, "background watcher: re-check active sources shortly before their evidence expires, and delete cached answers that state a value that changed"),
		maxPerHour: fs.Int("watch-max-per-hour", 60, "most watcher fetches per rolling hour; over the cap, checks are skipped and requests fetch as usual"),
		idle:       fs.Duration("watch-idle", 30*time.Minute, "a source nobody asked about for this long is not watched (0 = watch every source that was ever asked about)"),
		evict:      fs.Bool("watch-evict", true, "delete cached answers that state the old value when the watcher sees a change (false = only record it)"),
	}
}

// watchSetup is what the watch flags build. Everything is nil when both
// features are off.
type watchSetup struct {
	flags    *watchFlags
	schedule *watch.Schedule
	watcher  *watch.Watcher
	server   *watch.Server
	floor    time.Duration
}

// newWatchSetup validates the flags and builds the schedule (nil when both
// -check-adaptive and -watch are off).
func newWatchSetup(wf *watchFlags, pol *policy.Policy, evidenceTTL time.Duration) (*watchSetup, error) {
	ws := &watchSetup{flags: wf, floor: evidenceTTL}
	if *wf.maxPerHour < 0 {
		return nil, errors.New("-watch-max-per-hour must be >= 0")
	}
	if *wf.idle < 0 {
		return nil, errors.New("-watch-idle must be >= 0")
	}
	if *wf.adaptive {
		if evidenceTTL <= 0 {
			return nil, errors.New("-check-adaptive needs -evidence-ttl > 0 (it is the shortest interval)")
		}
		if *wf.checkMax < evidenceTTL {
			return nil, fmt.Errorf("-check-max %s must be >= -evidence-ttl %s", *wf.checkMax, evidenceTTL)
		}
		if *wf.growth <= 1 {
			return nil, fmt.Errorf("-check-growth must be > 1, got %g", *wf.growth)
		}
	}
	if *wf.watch && evidenceTTL <= 0 {
		return nil, errors.New("-watch needs -evidence-ttl > 0 (there is no evidence to keep fresh otherwise)")
	}
	if !*wf.adaptive && !*wf.watch {
		return ws, nil
	}
	cfg := watch.Config{Floor: evidenceTTL, Max: evidenceTTL, Growth: *wf.growth, Adaptive: *wf.adaptive}
	if *wf.adaptive {
		cfg.Max = *wf.checkMax
	}
	var facts []*policy.Fact
	if pol != nil {
		facts = pol.Facts
	}
	s, err := watch.NewSchedule(cfg, facts)
	if err != nil {
		return nil, err
	}
	ws.schedule = s
	return ws, nil
}

// cacheConfig adds the schedule to the evidence cache config when there is
// one. A nil schedule is left out on purpose: a typed nil would still count
// as "set".
func (ws *watchSetup) cacheConfig(cfg evidence.CacheConfig) evidence.CacheConfig {
	if ws.schedule != nil {
		cfg.Schedule = ws.schedule
	}
	return cfg
}

// wire builds the watcher (with -watch) and returns a root handler that
// serves GET /freshness/watch and POST /freshness/watch/check itself and
// hands everything else to inner. The endpoints exist even with both flags
// off; they then report adaptive false and an empty source list.
func (ws *watchSetup) wire(inner http.Handler, cache *evidence.Cache, ages *agetrack.Tracker, dlog *decisionlog.Log, clearer watch.Clearer, providerName, token string, logger *slog.Logger) http.Handler {
	wf := ws.flags
	ws.server = &watch.Server{Schedule: ws.schedule, Stats: cache.Stats, Token: token, Idle: *wf.idle, MaxPerHour: *wf.maxPerHour}
	if *wf.watch && ws.schedule != nil {
		w, err := watch.New(watch.Options{
			Schedule: ws.schedule, Cache: cache, Ages: ages, Clearer: clearer, Decisions: dlog,
			Enabled: true, MaxPerHour: *wf.maxPerHour, Idle: *wf.idle, Evict: *wf.evict,
			Provider: providerName, Logger: logger,
		})
		if err != nil {
			// Only a programming error reaches here (schedule and cache are set).
			logger.Error("background watcher not started", "error", err)
		} else {
			ws.watcher = w
			dlog.Subscribe(w.Index().Learn)
			ws.server.Watcher = w
		}
	}
	if ws.schedule != nil {
		cfg := ws.schedule.Config()
		logger.Info("smart checking", "adaptive", cfg.Adaptive, "floor", cfg.Floor, "max", cfg.Max, "growth", cfg.Growth,
			"watch", ws.watcher != nil, "watch_max_per_hour", *wf.maxPerHour, "watch_idle", *wf.idle, "watch_evict", *wf.evict,
			"sources", len(ws.schedule.Sources()))
	}
	mux := http.NewServeMux()
	ws.server.Register(mux)
	mux.Handle("/", inner)
	return mux
}

// run drives the watcher until ctx is done. It returns at once when there
// is no watcher, so `go ws.run(ctx)` is always safe.
func (ws *watchSetup) run(ctx context.Context) {
	if ws.watcher == nil {
		return
	}
	ws.watcher.Run(ctx)
}
