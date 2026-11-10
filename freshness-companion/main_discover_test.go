package main

import (
	"flag"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"freshness-companion/internal/evidence"
	"freshness-companion/internal/policy"
)

func parseDiscoverFlags(t *testing.T, args ...string) *discoverFlags {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	df := addDiscoverFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	return df
}

func TestDiscoverNeedsTavilyKey(t *testing.T) {
	t.Setenv("TAVILY_API_KEY", "")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, err := newDiscoverSetup(parseDiscoverFlags(t, "-discover"), policy.Default(), evidence.TavilyExtractName, "", logger)
	if err == nil || !strings.Contains(err.Error(), "TAVILY_API_KEY") {
		t.Fatalf("got %v, want a startup error naming TAVILY_API_KEY", err)
	}
	for _, bad := range [][]string{{"-discover", "-discover-max-per-hour=-1"}, {"-discover", "-discover-ttl=0"}, {"-discover", "-discover-max-sources=0"}} {
		t.Setenv("TAVILY_API_KEY", "tvly-test")
		if _, err := newDiscoverSetup(parseDiscoverFlags(t, bad...), policy.Default(), evidence.TavilyExtractName, "", logger); err == nil {
			t.Errorf("%v: expected an error", bad)
		}
	}
	t.Setenv("TAVILY_API_KEY", "tvly-test")
	ds, err := newDiscoverSetup(parseDiscoverFlags(t, "-discover", "-discover-store="+filepath.Join(t.TempDir(), "d.json")), policy.Default(), evidence.TavilyExtractName, "", logger)
	if err != nil || ds.service == nil || ds.engine(nil).Discover == nil {
		t.Fatalf("with a key: %+v %v", ds, err)
	}
}

// -discover with the direct provider is refused at startup: the direct
// provider would fetch search-chosen URLs from this machine.
func TestDiscoverRefusesDirectProvider(t *testing.T) {
	t.Setenv("TAVILY_API_KEY", "tvly-test")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, err := newDiscoverSetup(parseDiscoverFlags(t, "-discover"), policy.Default(), evidence.DirectFetchName, "", logger)
	if err == nil || !strings.Contains(err.Error(), "-provider direct") {
		t.Fatalf("got %v, want a startup error naming -provider direct", err)
	}
	// run() fails at startup too, before anything listens, key or not.
	for _, key := range []string{"", "tvly-test"} {
		t.Setenv("TAVILY_API_KEY", key)
		err = run([]string{"-discover", "-policy=", "-provider=direct", "-ages=", "-decisions=", "-changes="})
		if err == nil || !strings.Contains(err.Error(), "-discover cannot be used with -provider direct") {
			t.Fatalf("run with key %q: got %v", key, err)
		}
	}
	// Without -discover the direct provider is still fine.
	t.Setenv("TAVILY_API_KEY", "")
	if ds, err := newDiscoverSetup(parseDiscoverFlags(t), policy.Default(), evidence.DirectFetchName, "", logger); err != nil || ds.service != nil {
		t.Fatalf("direct without -discover: %+v %v", ds, err)
	}
}

// The compose policies parse, and only the Tavily ones carry a discover block.
func TestDockerPoliciesDiscoverBlocks(t *testing.T) {
	cases := map[string]struct {
		active bool
		facts  int
	}{
		"policy.json":          {false, 1},
		"policy.tavily.json":   {true, 1},
		"policy.discover.json": {true, 0},
	}
	for name, want := range cases {
		pol, err := policy.Load(filepath.Join("..", "docker", name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		eff, ok := pol.Resolve("How much does the Acme Pro plan cost per month?")
		if !ok || (eff.Discover != nil) != want.active || len(pol.Facts) != want.facts {
			t.Fatalf("%s: discover=%v facts=%d", name, eff.Discover != nil, len(pol.Facts))
		}
	}
}

func TestDiscoverOffByDefault(t *testing.T) {
	t.Setenv("TAVILY_API_KEY", "")
	df := parseDiscoverFlags(t)
	if *df.on || *df.maxPerHour != 20 || *df.store != "data/discovered.json" {
		t.Fatalf("defaults: on=%v max=%d store=%q", *df.on, *df.maxPerHour, *df.store)
	}
	ds, err := newDiscoverSetup(df, policy.Default(), evidence.TavilyExtractName, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || ds.service != nil {
		t.Fatalf("off: %+v %v", ds, err)
	}
	// No typed-nil discoverer sneaks into the engine.
	if e := ds.engine(nil); e.Discover != nil {
		t.Fatal("discovery off must leave Engine.Discover nil")
	}
	// The endpoint exists and says discovery is off; other paths reach inner.
	h := ds.wire(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/freshness/discovered", nil))
	if !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Fatalf("endpoint: %s", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("other paths must reach the proxy, got %d", rec.Code)
	}
}
