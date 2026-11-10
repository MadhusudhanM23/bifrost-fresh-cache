package main

// "Tavily finds the source" (-discover). Off by default; with the defaults
// the companion behaves exactly as it did before these flags existed: no
// Tavily Search call is ever made. main.go touches this file at three points:
// addDiscoverFlags before Parse, newDiscoverSetup + engine() when the
// decision engine is built, and wire around the HTTP handler.

import (
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	"freshness-companion/internal/decision"
	"freshness-companion/internal/discover"
	"freshness-companion/internal/evidence"
	"freshness-companion/internal/policy"
	"freshness-companion/internal/watch"
)

type discoverFlags struct {
	on         *bool
	maxPerHour *int
	ttl        *time.Duration
	missTTL    *time.Duration
	maxSources *int
	timeout    *time.Duration
	store      *string
}

func addDiscoverFlags(fs *flag.FlagSet) *discoverFlags {
	return &discoverFlags{
		on:         fs.Bool("discover", false, `"Tavily finds the source": for a time-sensitive cache hit that no watched fact covers, in a category whose policy has an enabled "discover" block, search Tavily for a source page with a short query built from the policy template and allowlisted names (never the prompt or the answer), then check the answer against that page. Needs TAVILY_API_KEY`),
		maxPerHour: fs.Int("discover-max-per-hour", 20, "most Tavily Search calls for discovery per rolling hour (1 credit each); over the cap the companion behaves as without discovery"),
		ttl:        fs.Duration("discover-ttl", 24*time.Hour, "reuse a learned source for this long (no new search)"),
		missTTL:    fs.Duration("discover-miss-ttl", time.Hour, `remember "search found no usable page" for this long`),
		maxSources: fs.Int("discover-max-sources", 50, "most learned sources (and remembered misses) kept"),
		timeout:    fs.Duration("discover-timeout", 8*time.Second, "timeout for one discovery search; on timeout the companion behaves as without discovery"),
		store:      fs.String("discover-store", "data/discovered.json", `file that persists learned sources ("" = memory only)`),
	}
}

// discoverSetup is what the discovery flags build. service is nil when
// discovery is off.
type discoverSetup struct {
	service *discover.Service
}

// newDiscoverSetup validates the flags and builds the discovery service
// (nil with -discover off). -discover needs TAVILY_API_KEY and a Tavily
// evidence provider: provider is the evidence provider's name.
func newDiscoverSetup(df *discoverFlags, pol *policy.Policy, provider, tavilyURL string, logger *slog.Logger) (*discoverSetup, error) {
	ds := &discoverSetup{}
	if !*df.on {
		return ds, nil
	}
	if *df.maxPerHour < 0 {
		return nil, errors.New("-discover-max-per-hour must be >= 0")
	}
	if *df.ttl <= 0 || *df.missTTL <= 0 || *df.timeout <= 0 {
		return nil, errors.New("-discover-ttl, -discover-miss-ttl and -discover-timeout must be positive")
	}
	if *df.maxSources <= 0 {
		return nil, errors.New("-discover-max-sources must be positive")
	}
	if provider == evidence.DirectFetchName {
		return nil, errors.New("-discover cannot be used with -provider direct: discovered pages come from search results, and the direct provider would fetch them from this machine (a server-side request risk); use -provider tavily_extract or tavily_search")
	}
	key := os.Getenv("TAVILY_API_KEY")
	if key == "" {
		return nil, errors.New("-discover needs TAVILY_API_KEY: discovery searches with Tavily Search")
	}
	searcher, err := evidence.NewTavilySearcher(evidence.TavilyConfig{
		APIKey: key, BaseURL: tavilyURL, HTTPClient: &http.Client{Timeout: *df.timeout + 5*time.Second},
	})
	if err != nil {
		return nil, err
	}
	svc, err := discover.New(discover.Config{
		Searcher: searcher, Budget: watch.NewBudget(*df.maxPerHour, nil),
		TTL: *df.ttl, MissTTL: *df.missTTL, MaxSources: *df.maxSources, Timeout: *df.timeout,
		StorePath: *df.store, Logger: logger, Rules: discoverRules(pol),
	})
	if err != nil {
		return nil, err
	}
	ds.service = svc
	active := 0
	for _, c := range pol.Categories {
		if c.Discover.Active() {
			active++
		}
	}
	if active == 0 {
		logger.Warn("-discover is on, but no policy category has an enabled discover block, so nothing will be searched")
	}
	logger.Info("discovery (Tavily finds the source) is on: only the derived query and the chosen URL are sent to Tavily",
		"categories", active, "max_per_hour", *df.maxPerHour, "ttl", *df.ttl, "miss_ttl", *df.missTTL, "store", *df.store)
	return ds, nil
}

// engine returns the decision engine, with discovery when it is on. A nil
// service is left out on purpose: a typed nil would still count as "set".
func (ds *discoverSetup) engine(ev decision.EvidenceSource) *decision.Engine {
	e := &decision.Engine{Evidence: ev}
	if ds.service != nil {
		e.Discover = ds.service
	}
	return e
}

// wire returns a root handler that serves GET /freshness/discovered itself
// and hands everything else to inner. The endpoint exists with discovery off
// too; it then says so.
func (ds *discoverSetup) wire(inner http.Handler) http.Handler {
	mux := http.NewServeMux()
	ds.service.Register(mux)
	mux.Handle("/", inner)
	return mux
}

// discoverRules looks up a category's discover block, so learned sources
// loaded from the store file are rechecked against today's policy.
func discoverRules(pol *policy.Policy) func(string) *policy.Discover {
	return func(category string) *policy.Discover {
		for _, c := range pol.Categories {
			if c.Name == category {
				return c.Discover
			}
		}
		return nil
	}
}
