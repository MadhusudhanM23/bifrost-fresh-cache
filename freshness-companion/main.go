// Command freshness-companion is a small HTTP proxy in front of Bifrost.
// For time-sensitive cache hits only, it checks current evidence for the
// cached answer and refuses to serve it when it is stale.
//
// Secrets come only from the environment, never from flags:
//
//	TAVILY_API_KEY      key for -provider tavily_extract / tavily_search and -discover
//	BIFROST_ADMIN_AUTH  optional value for the clear endpoint's auth header,
//	                    e.g. "Bearer <api key>" or "Basic <base64 user:pass>"
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/textproto"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"freshness-companion/internal/agetrack"
	"freshness-companion/internal/auditlog"
	"freshness-companion/internal/changes"
	"freshness-companion/internal/decisionlog"
	"freshness-companion/internal/evidence"
	"freshness-companion/internal/policy"
	"freshness-companion/internal/proxy"
	"freshness-companion/internal/verified"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "freshness-companion:", err)
		os.Exit(1)
	}
}

// headerSet is a repeatable "Name: value" flag.
type headerSet map[string]string

func (h headerSet) String() string {
	names := make([]string, 0, len(h))
	for name := range h {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

func (h headerSet) Set(v string) error {
	name, value, ok := strings.Cut(v, ":")
	if !ok {
		name, value, ok = strings.Cut(v, "=")
	}
	name = strings.TrimSpace(name)
	if !ok || name == "" {
		return fmt.Errorf("want \"Name: value\", got %q", v)
	}
	h[textproto.CanonicalMIMEHeaderKey(name)] = strings.TrimSpace(value)
	return nil
}

// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(v string) error {
	if v = strings.TrimSpace(v); v == "" {
		return errors.New("empty value")
	}
	*l = append(*l, v)
	return nil
}

func run(args []string) error {
	fs := flag.NewFlagSet("freshness-companion", flag.ContinueOnError)
	var (
		listen         = fs.String("listen", ":8090", "address to listen on")
		bifrostURL     = fs.String("bifrost", "http://localhost:8080", "Bifrost base URL")
		policyPath     = fs.String("policy", "examples/policy.example.json", `policy file (JSON); "" uses the built-in categories with no watched facts`)
		providerName   = fs.String("provider", "tavily_extract", "evidence provider: tavily_extract | tavily_search | direct")
		tavilyURL      = fs.String("tavily-url", evidence.DefaultTavilyBaseURL, "Tavily API base URL")
		tavilyDepth    = fs.String("tavily-depth", "basic", "Tavily extract_depth: basic | advanced")
		tavilyFormat   = fs.String("tavily-format", "text", "Tavily content format: text | markdown")
		tavilyTimeout  = fs.Duration("tavily-timeout", 10*time.Second, `Tavily's server-side "timeout" parameter (1s-60s)`)
		tavilyFresh    = fs.Bool("tavily-fresh", true, "add a unique _fresh=<unix> query parameter so Tavily fetches the live page instead of a remembered copy")
		directRaw      = fs.Bool("direct-raw", false, "direct provider: match patterns against raw HTML instead of visible text")
		evidenceTTL    = fs.Duration("evidence-ttl", 60*time.Second, "reuse evidence per source URL for this long (0 disables)")
		evidenceErrTTL = fs.Duration("evidence-error-ttl", 10*time.Second, "remember an evidence failure for this long (0 disables)")
		evidenceTO     = fs.Duration("evidence-timeout", 15*time.Second, "timeout for one evidence fetch")
		maxEvidence    = fs.Int64("max-evidence-bytes", 2<<20, "largest evidence body accepted")
		agesPath       = fs.String("ages", "data/cache-ages.json", `file that persists cache entry ages ("" = memory only)`)
		agesMax        = fs.Int("ages-max", agetrack.DefaultMaxEntries, "most cache entries tracked")
		decisionsPath  = fs.String("decisions", "data/decisions.jsonl", `decision log, JSON Lines ("" = memory only)`)
		decisionsRing  = fs.Int("decisions-ring", decisionlog.DefaultRingSize, "decisions kept in memory for /freshness/decisions")
		refreshName    = fs.String("refresh", "clear_and_resend", "refresh strategy: clear_and_resend | resend_with_headers")
		refreshTries   = fs.Int("refresh-max-attempts", 3, "clear_and_resend: entries cleared at most per refresh")
		adminHeader    = fs.String("bifrost-admin-header", "Authorization", "header that carries BIFROST_ADMIN_AUTH on the clear call")
		clearTimeout   = fs.Duration("clear-timeout", 5*time.Second, "timeout for the cache clear call")
		upstreamTO     = fs.Duration("upstream-timeout", 2*time.Minute, "timeout for one buffered Bifrost call")
		maxRequest     = fs.Int64("max-request-bytes", 8<<20, "larger chat requests pass through unchecked")
		maxResponse    = fs.Int64("max-response-bytes", 8<<20, "larger responses pass through unchecked")
		logLevel       = fs.String("log-level", "info", "debug | info | warn | error")
		changesPath    = fs.String("changes", "data/changes.json", `file that persists change notices ("" = memory only)`)
		refreshShare   = fs.Duration("refresh-share", 2*time.Second, "requests that need the same refresh share one; a finished refresh is still shared this long (negative = only while it runs)")
		settleDelay    = fs.Duration("settle-delay", 150*time.Millisecond, "wait before deleting a cache entry Bifrost has only just written (it writes in the background)")
		auditsPath     = fs.String("audits", "data/audits.jsonl", `wrong-match audit log, JSON Lines ("" = memory only)`)
		auditRate      = fs.Float64("audit-rate", 0, "fraction (0-1) of semantic cache hits to audit by asking the model again; 0 = off unless -audit-near-margin is set")
		auditNear      = fs.Float64("audit-near-margin", 0, "also audit every semantic hit that scored within this margin above the threshold (e.g. 0.05); 0 = off")
		auditPerMin    = fs.Int("audit-max-per-minute", 30, "most audits per minute (each one is a model call)")
		auditCompare   = fs.String("audit-compare", proxy.CompareExact, "how an audit compares answers: exact | judge (a model decides)")
		auditJudge     = fs.String("audit-judge-model", "", "model for -audit-compare judge (default: the audited request's model)")
		verifyBelow    = fs.Float64("verify-below", 0, "check every semantic hit that scored below this against a fresh answer BEFORE serving it (e.g. 0.90 with a 0.80 threshold); a wrong one is removed and replaced. 0 = off")
		verifyTrust    = fs.Duration("verify-trust", time.Hour, "how long a verified question/entry pair is served without another check")
		verifyPerMin   = fs.Int("verify-max-per-minute", 120, "most pre-serve checks per minute (each one is a model call); hits over the cap are served unchecked")
		verifiedPath   = fs.String("verified", "", `JSON file of human-checked {question, answer, label, source, scope} entries answered by the companion itself, exact match only ("" = off)`)
		verifiedScope  = fs.String("verified-scope-header", proxy.DefaultVerifiedScopeHeader, "request header compared with an entry's scope")
		verifiedEvery  = fs.Duration("verified-reload", 5*time.Second, "how often the verified file is checked for changes")
		explainRing    = fs.Int("explain-ring", proxy.DefaultExplainRing, "request explanations kept in memory for /freshness/explain (negative = none)")
		mcpOn          = fs.Bool("mcp", false, "serve an MCP server on POST /mcp with the tools check_freshness, explain_request, list_watched_sources and report_change (for Bifrost's MCP gateway)")
	)
	setHeaders := headerSet{}
	var dropHeaders stringList
	fs.Var(setHeaders, "refresh-set-header", `header set on the re-request, "Name: value" (repeatable)`)
	fs.Var(&dropHeaders, "refresh-drop-header", "header removed from the re-request (repeatable)")
	wf := addWatchFlags(fs)    // -check-adaptive, -check-max, -check-growth, -watch, -watch-* (main_watch.go)
	df := addDiscoverFlags(fs) // -discover, -discover-* (main_discover.go)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		return fmt.Errorf("-log-level: %w", err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	pol := policy.Default()
	if *policyPath != "" {
		p, err := policy.Load(*policyPath)
		if err != nil {
			return err
		}
		pol = p
	}

	provider, err := newProvider(*providerName, *tavilyURL, *tavilyDepth, *tavilyFormat, *tavilyTimeout, *tavilyFresh, *directRaw, *maxEvidence, *evidenceTO)
	if err != nil {
		return err
	}
	if provider.Name() == evidence.DirectFetchName {
		logger.Warn("evidence provider is direct_fetch: a plain HTTP GET of each source URL, NOT Tavily")
	}
	ws, err := newWatchSetup(wf, pol, *evidenceTTL) // smart checking + watcher; nil schedule with the defaults
	if err != nil {
		return err
	}
	cache := evidence.NewCache(provider, ws.cacheConfig(evidence.CacheConfig{TTL: *evidenceTTL, ErrorTTL: *evidenceErrTTL, FetchTimeout: *evidenceTO}))
	ds, err := newDiscoverSetup(df, pol, provider.Name(), *tavilyURL, logger) // nil service with the defaults
	if err != nil {
		return err
	}

	ages, err := agetrack.Open(agetrack.Options{Path: *agesPath, MaxEntries: *agesMax, Logger: logger})
	if err != nil {
		return err
	}
	dlog, err := decisionlog.Open(*decisionsPath, *decisionsRing)
	if err != nil {
		return fmt.Errorf("open decision log: %w", err)
	}
	defer dlog.Close()

	upstream, err := proxy.NewUpstream(*bifrostURL, proxy.NewTransport(*upstreamTO), *maxResponse)
	if err != nil {
		return err
	}
	adminAuth := os.Getenv("BIFROST_ADMIN_AUTH")
	edits := proxy.HeaderEdits{Set: setHeaders, Drop: dropHeaders}
	clearer, err := proxy.NewBifrostClearer(*bifrostURL, *adminHeader, adminAuth, *clearTimeout)
	if err != nil {
		return err
	}
	var strategy proxy.RefreshStrategy
	switch *refreshName {
	case "clear_and_resend":
		strategy = &proxy.ClearAndResend{Clearer: clearer, Sender: upstream, MaxAttempts: *refreshTries, Headers: edits}
	case "resend_with_headers":
		if edits.Empty() {
			return errors.New("-refresh resend_with_headers needs -refresh-set-header and/or -refresh-drop-header")
		}
		strategy = &proxy.ResendWithHeaders{Sender: upstream, Headers: edits}
	default:
		return fmt.Errorf("-refresh %q: want clear_and_resend or resend_with_headers", *refreshName)
	}

	changeStore, err := changes.Open(changes.Options{Path: *changesPath, Logger: logger})
	if err != nil {
		return fmt.Errorf("open change store: %w", err)
	}
	noticeToken := os.Getenv("FRESHNESS_NOTICE_TOKEN")
	if noticeToken == "" {
		logger.Warn("change notices are unauthenticated (set FRESHNESS_NOTICE_TOKEN to require a bearer token); keep the companion on a trusted network")
	}
	audit := proxy.AuditConfig{
		Rate: *auditRate, NearMargin: *auditNear, MaxPerMinute: *auditPerMin,
		Compare: *auditCompare, JudgeModel: *auditJudge,
		VerifyBelow: *verifyBelow, TrustFor: *verifyTrust, GateMaxPerMinute: *verifyPerMin,
	}
	if *auditCompare != proxy.CompareExact && *auditCompare != proxy.CompareJudge {
		return fmt.Errorf("-audit-compare %q: want exact or judge", *auditCompare)
	}
	if *auditRate < 0 || *auditRate > 1 || *auditNear < 0 || *auditNear > 1 {
		return errors.New("-audit-rate and -audit-near-margin must be between 0 and 1")
	}
	if *verifyBelow < 0 || *verifyBelow > 1 {
		return errors.New("-verify-below must be between 0 and 1")
	}
	if *auditRate > 0 || *auditNear > 0 || *verifyBelow > 0 {
		alog, err := auditlog.Open(*auditsPath, auditlog.DefaultRingSize)
		if err != nil {
			return fmt.Errorf("open audit log: %w", err)
		}
		defer alog.Close()
		audit.Log = alog
		if *auditCompare == proxy.CompareExact {
			logger.Info("wrong-match audit is on with -audit-compare exact: answers worded differently count as different, so the rate is an upper bound")
		}
	}

	var verifiedStore *verified.Store
	if *verifiedPath != "" {
		verifiedStore, err = verified.Open(*verifiedPath, logger)
		if err != nil {
			return err
		}
		logger.Info("verified answers loaded", "file", *verifiedPath, "entries", verifiedStore.Set().Len(), "scope_header", *verifiedScope)
	}

	srv, err := proxy.New(proxy.Config{
		Verified:            verifiedStore,
		VerifiedScopeHeader: *verifiedScope,
		ExplainRing:         *explainRing,
		Upstream:            upstream,
		Policy:              pol,
		Engine:              ds.engine(cache), // adds discovery when -discover is on
		Ages:                ages,
		Decisions:           dlog,
		Refresh:             strategy,
		Logger:              logger,
		UpstreamTimeout:     *upstreamTO,
		MaxRequestBytes:     *maxRequest,
		MaxResponseBytes:    *maxResponse,
		EvidenceProvider:    provider.Name(),
		BifrostURL:          *bifrostURL,
		Changes:             changeStore,
		ChangeToken:         noticeToken,
		Clearer:             clearer,
		SettleDelay:         *settleDelay,
		Audit:               audit,
		RefreshShareWindow:  *refreshShare,
	})
	if err != nil {
		return err
	}
	if err := enableMCP(srv, *mcpOn, logger); err != nil {
		return err
	}

	httpServer := &http.Server{
		Addr:              *listen,
		Handler:           ds.wire(ws.wire(srv.Handler(), cache, ages, dlog, clearer, provider.Name(), noticeToken, logger)), // adds /freshness/watch and /freshness/discovered in front of the proxy
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No WriteTimeout: streamed responses can legitimately run long.
	}
	logger.Info("freshness companion starting",
		"listen", *listen, "bifrost", *bifrostURL, "policy", *policyPath,
		"categories", len(pol.Categories), "facts", len(pol.Facts),
		"evidence_provider", provider.Name(), "evidence_ttl", *evidenceTTL,
		"refresh", strategy.Name(), "clear_auth_configured", adminAuth != "",
		"ages", *agesPath, "tracked_entries", ages.Len(), "decisions", *decisionsPath)

	if verifiedStore != nil {
		watchDone := make(chan struct{})
		defer close(watchDone)
		go verifiedStore.Watch(watchDone, *verifiedEvery)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go ws.run(ctx) // background watcher (-watch); returns at once when off
	errCh := make(chan error, 1)
	go func() { errCh <- httpServer.ListenAndServe() }()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return httpServer.Shutdown(shutdownCtx)
}

func newProvider(name, tavilyURL, depth, format string, serverTimeout time.Duration, fresh, directRaw bool, maxBytes int64, timeout time.Duration) (evidence.Provider, error) {
	tavily := evidence.TavilyConfig{
		APIKey:        os.Getenv("TAVILY_API_KEY"),
		BaseURL:       tavilyURL,
		ExtractDepth:  depth,
		Format:        format,
		ServerTimeout: serverTimeout,
		FreshStamp:    fresh,
		MaxBodyBytes:  maxBytes,
		HTTPClient:    &http.Client{Timeout: timeout + 5*time.Second},
	}
	switch name {
	case "tavily_extract", "tavily":
		p, err := evidence.NewTavilyExtract(tavily)
		if err != nil {
			return nil, fmt.Errorf("%w; or run with -provider direct for an offline demo", err)
		}
		return p, nil
	case "tavily_search":
		p, err := evidence.NewTavilySearch(tavily)
		if err != nil {
			return nil, fmt.Errorf("%w; or run with -provider direct for an offline demo", err)
		}
		return p, nil
	case "direct", "direct_fetch":
		return evidence.NewDirectFetch(evidence.DirectConfig{Timeout: timeout, MaxBodyBytes: maxBytes, Raw: directRaw}), nil
	default:
		return nil, fmt.Errorf("-provider %q: want tavily_extract, tavily_search or direct", name)
	}
}
