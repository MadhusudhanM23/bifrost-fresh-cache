package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"freshness-companion/internal/agetrack"
	"freshness-companion/internal/decisionlog"
	"freshness-companion/internal/evidence"
	"freshness-companion/internal/policy"
)

const watchTestPolicy = `{
  "version": 1,
  "categories": [{"name": "price", "keywords": ["price", "cost", "per month"], "max_age": "10m", "on_unverifiable": "bypass"}],
  "facts": [{"name": "Acme Pro plan price", "category": "price", "source_url": "http://demoserver:8000/",
    "source_pattern": "Acme Pro plan[^$0-9]{0,20}\\$\\s*([0-9]+)\\s*per month",
    "answer_pattern": "\\$\\s*([0-9]+)\\s*per month", "compare": "numeric"}]
}`

func parseWatchFlags(t *testing.T, args ...string) *watchFlags {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	wf := addWatchFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	return wf
}

type nopProvider struct{}

func (nopProvider) Name() string { return "nop" }
func (nopProvider) Fetch(_ context.Context, url string) (evidence.Evidence, error) {
	return evidence.Evidence{URL: url, Content: "Acme Pro plan $49 per month"}, nil
}

type nopClearer struct{}

func (nopClearer) Clear(context.Context, string) error { return nil }

func TestWatchFlagValidation(t *testing.T) {
	pol, err := policy.Parse([]byte(watchTestPolicy))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		args []string
		ttl  time.Duration
		want string // "" = ok
	}{
		{"defaults", nil, 60 * time.Second, ""},
		{"defaults with ttl off", nil, 0, ""},
		{"adaptive ok", []string{"-check-adaptive", "-check-max=5m"}, 5 * time.Second, ""},
		{"adaptive needs ttl", []string{"-check-adaptive"}, 0, "-check-adaptive needs -evidence-ttl"},
		{"max below floor", []string{"-check-adaptive", "-check-max=4s"}, 5 * time.Second, "-check-max 4s must be >= -evidence-ttl 5s"},
		{"growth too small", []string{"-check-adaptive", "-check-growth=1"}, 5 * time.Second, "-check-growth must be > 1"},
		{"growth ignored when fixed", []string{"-check-growth=1"}, 5 * time.Second, ""},
		{"watch needs ttl", []string{"-watch"}, 0, "-watch needs -evidence-ttl"},
		{"watch ok without adaptive", []string{"-watch"}, 5 * time.Second, ""},
		{"negative cap", []string{"-watch", "-watch-max-per-hour=-1"}, 5 * time.Second, "-watch-max-per-hour must be >= 0"},
		{"negative idle", []string{"-watch-idle=-1s"}, 5 * time.Second, "-watch-idle must be >= 0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws, err := newWatchSetup(parseWatchFlags(t, tc.args...), pol, tc.ttl)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
			if err == nil {
				on := *ws.flags.adaptive || *ws.flags.watch
				if (ws.schedule != nil) != on {
					t.Fatalf("schedule built = %v, want %v", ws.schedule != nil, on)
				}
			}
		})
	}
	// With -watch alone the interval stays fixed at the floor.
	ws, _ := newWatchSetup(parseWatchFlags(t, "-watch"), pol, 5*time.Second)
	if cfg := ws.schedule.Config(); cfg.Adaptive || cfg.Max != 5*time.Second {
		t.Fatalf("watch-only schedule = %+v, want fixed at the floor", cfg)
	}
}

func wireTest(t *testing.T, ws *watchSetup) (http.Handler, *evidence.Cache) {
	t.Helper()
	cache := evidence.NewCache(nopProvider{}, ws.cacheConfig(evidence.CacheConfig{TTL: 5 * time.Second}))
	ages, _ := agetrack.Open(agetrack.Options{})
	dlog, _ := decisionlog.Open("", 10)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "inner:"+r.URL.Path) })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return ws.wire(inner, cache, ages, dlog, nopClearer{}, "nop", "", logger), cache
}

func get(t *testing.T, h http.Handler, path string) (int, map[string]any, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body, rec.Body.String()
}

func TestDefaultsKeepTodayBehaviour(t *testing.T) {
	pol, _ := policy.Parse([]byte(watchTestPolicy))
	ws, err := newWatchSetup(parseWatchFlags(t), pol, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if ws.schedule != nil {
		t.Fatal("defaults must not build a schedule")
	}
	cfg := ws.cacheConfig(evidence.CacheConfig{TTL: 5 * time.Second})
	if cfg.Schedule != nil {
		t.Fatal("defaults must leave CacheConfig.Schedule nil (a typed nil would count as set)")
	}
	h, _ := wireTest(t, ws)
	if ws.watcher != nil {
		t.Fatal("defaults must not build a watcher")
	}
	// Everything else still reaches the companion's own handler.
	if code, _, body := get(t, h, "/healthz"); code != 200 || body != "inner:/healthz" {
		t.Fatalf("/healthz = %d %q", code, body)
	}
	if code, _, body := get(t, h, "/v1/chat/completions"); code != 200 || body != "inner:/v1/chat/completions" {
		t.Fatalf("/v1 = %d %q", code, body)
	}
	// The status endpoint exists and says everything is off.
	code, body, raw := get(t, h, "/freshness/watch")
	if code != 200 || body["adaptive"] != false || len(body["sources"].([]any)) != 0 {
		t.Fatalf("/freshness/watch = %d %s", code, raw)
	}
	if wt := body["watcher"].(map[string]any); wt["enabled"] != false || wt["max_per_hour"] != 60.0 || wt["idle_after_seconds"] != 1800.0 {
		t.Fatalf("watcher = %v", wt)
	}
	// A manual check is refused while the watcher is off.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/freshness/watch/check", strings.NewReader(`{"fact":"Acme Pro plan price"}`)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("check with watcher off = %d", rec.Code)
	}
	// run returns at once: no background goroutine with the defaults.
	done := make(chan struct{})
	go func() { ws.run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run must return immediately without a watcher")
	}
}

func TestWatchFlagsBuildScheduleAndWatcher(t *testing.T) {
	pol, _ := policy.Parse([]byte(watchTestPolicy))
	ws, err := newWatchSetup(parseWatchFlags(t, "-check-adaptive", "-check-max=60s", "-watch", "-watch-max-per-hour=7"), pol, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	h, cache := wireTest(t, ws)
	if ws.watcher == nil || !ws.watcher.Enabled() {
		t.Fatal("-watch must build an enabled watcher")
	}
	if _, err := cache.Get(context.Background(), "http://demoserver:8000/"); err != nil {
		t.Fatal(err)
	}
	code, body, raw := get(t, h, "/freshness/watch")
	if code != 200 || body["adaptive"] != true || body["max_seconds"] != 60.0 || body["floor_seconds"] != 5.0 {
		t.Fatalf("/freshness/watch = %d %s", code, raw)
	}
	if wt := body["watcher"].(map[string]any); wt["enabled"] != true || wt["max_per_hour"] != 7.0 {
		t.Fatalf("watcher = %v", wt)
	}
	srcs := body["sources"].([]any)
	if len(srcs) != 1 || srcs[0].(map[string]any)["active"] != true || srcs[0].(map[string]any)["checks"] != 1.0 {
		t.Fatalf("sources = %v", srcs)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { ws.run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not stop on cancel")
	}
}
