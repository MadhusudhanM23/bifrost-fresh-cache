// Command dashboard is a small, honest operator dashboard for the Bifrost
// pgvector semantic cache and its freshness companion. It answers one
// question: is the cache saving money, or is it quietly serving wrong
// answers?
//
// It is read-only towards freshness-companion/internal (never imports or
// edits it; DecisionRecord in decisions.go is a hand-copied mirror of its
// JSON shape) and never touches bifrost/, tools/mock-openai/ or
// freshness-companion/internal/ on disk. It talks to the already-running
// freshness-companion over HTTP only, and optionally generates a small,
// fixed, labeled set of its own demo traffic through it to populate the
// cache-savings panel (see traffic.go). See README.md for exactly which
// numbers are real-live and which header this deliberately avoids.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

func subFS(f embed.FS, dir string) (fs.FS, error) {
	return fs.Sub(f, dir)
}

//go:embed static
var staticFiles embed.FS

type server struct {
	companionBase string
	bifrostBase   string
	client        *http.Client
	tally         *CacheTally
	scenarios     *scenarioRunner
}

func main() {
	listen := flag.String("listen", ":8095", "address for the dashboard to listen on")
	companion := flag.String("companion", "http://127.0.0.1:8090", "base URL of the running freshness-companion")
	bifrost := flag.String("bifrost", "http://127.0.0.1:18090", "base URL of Bifrost, used only to empty the dashboard's own cache key before a fresh run")
	demoPage := flag.String("demo-page", "", "path to freshness-companion/demo-page/index.html; turns on the two \"Try it\" scenarios that change the demo price (the page is always put back)")
	flag.Parse()

	s := &server{
		companionBase: strings.TrimRight(*companion, "/"),
		bifrostBase:   strings.TrimRight(*bifrost, "/"),
		client:        &http.Client{Timeout: 20 * time.Second},
		tally:         &CacheTally{},
	}
	recoverDemoPage(*demoPage)
	s.scenarios = newScenarioRunner(s.client, s.companionBase, s.bifrostBase, *demoPage, s.tally)

	staticSub, err := subFS(staticFiles, "static")
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(staticSub)))
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/stats", s.handleStats)
	mux.HandleFunc("POST /api/generate", s.handleGenerate)
	mux.HandleFunc("POST /api/reset", s.handleReset)
	mux.HandleFunc("GET /api/decisions", s.handleDecisions)
	mux.HandleFunc("GET /api/audits", s.handleAudits)
	mux.HandleFunc("GET /api/guard", s.handleGuard)
	mux.HandleFunc("GET /api/explain/{id}", s.handleExplain)
	mux.HandleFunc("GET /api/summary", s.handleSummary)
	mux.HandleFunc("GET /api/changes", s.handleChanges)
	mux.HandleFunc("POST /api/notify", s.handleNotify)
	mux.HandleFunc("GET /api/scenarios", s.handleScenarios)
	mux.HandleFunc("POST /api/scenarios/{id}/run", s.handleScenarioRun)
	mux.HandleFunc("GET /api/scenario-runs/{id}", s.handleScenarioRunStatus)

	log.Printf("dashboard listening on %s, companion at %s, bifrost at %s", *listen, s.companionBase, s.bifrostBase)
	log.Fatal(http.ListenAndServe(*listen, mux))
}

// handleHealth reports whether the live freshness-companion (and, through
// it, Bifrost) is reachable, so the UI can honestly say where its numbers
// come from instead of silently showing zeros.
func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	h, err := fetchCompanionHealth(ctx, s.client, s.companionBase)
	resp := map[string]any{
		"companion_url": s.companionBase,
		"reachable":     err == nil,
	}
	if err != nil {
		resp["error"] = err.Error()
	} else {
		resp["companion"] = h
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *server) handleStats(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.tally.snapshot())
}

func (s *server) handleReset(w http.ResponseWriter, _ *http.Request) {
	s.tally.reset()
	writeJSON(w, http.StatusOK, s.tally.snapshot())
}

// handleGenerate replays the fixed probe script (traffic.go) through the
// live companion once per "round" and tallies the real responses. This is
// the dashboard's own demo traffic, clearly labeled as such in the UI; it
// is the pragmatic fallback the task calls out, since Bifrost exposes no
// aggregate cache-stats endpoint of its own.
func (s *server) handleGenerate(w http.ResponseWriter, r *http.Request) {
	rounds := parseLimit(r.URL.Query().Get("rounds"), 1, 20)
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	// fresh=1 empties only this dashboard's own cache key first, so the first
	// question of every pair really is a miss and the later ones can be hits.
	if r.URL.Query().Get("fresh") == "1" {
		if err := s.clearProbeKey(ctx); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": "could not empty the dashboard's cache key: " + err.Error()})
			return
		}
	}

	var allEvents []TrafficEvent
	for i := 0; i < rounds; i++ {
		events, err := runProbeScript(ctx, s.client, s.companionBase, s.tally)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "events": allEvents})
			return
		}
		allEvents = append(allEvents, events...)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"rounds": rounds,
		"events": allEvents,
		"stats":  s.tally.snapshot(),
	})
}

// handleDecisions fetches the live decision log from the companion and
// returns the computed trust-signal aggregates, including repeat-stale
// warnings. threshold is overridable via ?threshold=N for experimentation;
// it defaults to defaultStaleThreshold (2).
func (s *server) handleDecisions(w http.ResponseWriter, r *http.Request) {
	threshold := parseLimit(r.URL.Query().Get("threshold"), defaultStaleThreshold, 1000)

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.companionBase+"/freshness/decisions?limit=1000", nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	resp, err := s.client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "companion unreachable: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	if resp.StatusCode != http.StatusOK {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "companion answered HTTP " + resp.Status, "body": string(body)})
		return
	}
	var env decisionsEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "could not parse companion response: " + err.Error()})
		return
	}
	stats := computeDecisionStats(env.Decisions, threshold)
	writeJSON(w, http.StatusOK, stats)
}

// clearProbeKey deletes every cache entry under the dashboard's own cache key
// (and nothing else) through Bifrost's documented clear-by-key endpoint.
func (s *server) clearProbeKey(ctx context.Context) error {
	return clearCacheKey(ctx, s.client, s.bifrostBase, probeCacheKey)
}

// getCompanion fetches a JSON document from the companion and decodes it.
func (s *server) getCompanion(ctx context.Context, path string, into any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.companionBase+path, nil)
	if err != nil {
		return 0, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("companion unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, fmt.Errorf("companion answered HTTP %s", resp.Status)
	}
	return resp.StatusCode, json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(into)
}

// handleAudits serves the wrong-match audit results, with the rate, the
// threshold advice and the drift warning computed from them.
func (s *server) handleAudits(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	var env auditsEnvelope
	if _, err := s.getCompanion(ctx, "/freshness/audits?limit=1000", &env); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, analyzeAudits(env))
}

// fetchGuard asks the companion what the cache guard kept out of the cache. An
// older companion without the endpoint (or an unreachable one) is reported as
// not available instead of failing the page.
func (s *server) fetchGuard(ctx context.Context) GuardView {
	var g GuardStats
	if _, err := s.getCompanion(ctx, "/freshness/guard", &g); err != nil {
		return GuardView{Available: false, GuardStats: GuardStats{ByReason: map[string]int64{}}}
	}
	if g.ByReason == nil {
		g.ByReason = map[string]int64{}
	}
	return GuardView{Available: true, GuardStats: g}
}

// handleGuard serves the guard counters (requests kept out of the cache on
// purpose), passed through from the companion.
func (s *server) handleGuard(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, s.fetchGuard(ctx))
}

// handleSummary combines the decision log, the audits, the guard counters and
// this dashboard's own cache-hit tally into the headline numbers (summary.go).
// Parts the companion cannot supply count as zero and are named in "missing",
// so the page can say so instead of showing a made-up number.
func (s *server) handleSummary(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	var missing []string

	var denv decisionsEnvelope
	if _, err := s.getCompanion(ctx, "/freshness/decisions?limit=1000", &denv); err != nil {
		missing = append(missing, "decisions")
	}
	var aenv auditsEnvelope
	if _, err := s.getCompanion(ctx, "/freshness/audits?limit=1000", &aenv); err != nil {
		missing = append(missing, "audits")
	}
	g := s.fetchGuard(ctx)
	if !g.Available {
		missing = append(missing, "guard")
	}
	snap := s.tally.snapshot()
	sum := computeSummary(computeDecisionStats(denv.Decisions, defaultStaleThreshold), analyzeAudits(aenv), g, snap.DirectHits+snap.SemanticHits)
	writeJSON(w, http.StatusOK, map[string]any{"summary": sum, "missing": missing})
}

// handleChanges serves the companion's recent "this changed" notices.
func (s *server) handleChanges(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	var out json.RawMessage
	if _, err := s.getCompanion(ctx, "/freshness/changes?limit=50", &out); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}

// handleNotify forwards a "this changed" notice to the companion, which
// validates it. The bearer token, if the companion needs one, comes from the
// FRESHNESS_NOTICE_TOKEN environment variable and never reaches the page.
func (s *server) handleNotify(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "could not read the request"})
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.companionBase+"/freshness/changes", strings.NewReader(string(body)))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := os.Getenv("FRESHNESS_NOTICE_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "companion unreachable: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// handleScenarios lists the guided "Try it" scenarios and whether each can run.
func (s *server) handleScenarios(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"scenarios":    s.scenarios.list(),
		"plain_target": s.bifrostBase,
		"safe_target":  s.companionBase,
	})
}

// handleScenarioRun starts one scenario in the background and returns a run id
// the page polls. Only one scenario runs at a time.
func (s *server) handleScenarioRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	runID, err := s.scenarios.start(id)
	switch {
	case err == nil:
		writeJSON(w, http.StatusAccepted, map[string]any{"run_id": runID})
	case errors.Is(err, errUnknownScenario):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "There is no scenario called " + id + "."})
	case errors.Is(err, errScenarioBusy):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
	default:
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
	}
}

func (s *server) handleScenarioRunStatus(w http.ResponseWriter, r *http.Request) {
	snap, ok := s.scenarios.get(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "That run is no longer remembered."})
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// explainIDPattern is the shape of the companion's request ids; anything else
// is refused before it is put into a URL.
var explainIDPattern = regexp.MustCompile(`^req-[0-9a-f]{8}-[0-9]{1,12}$`)

// handleExplain passes "why did I get this answer?" through to the companion.
// The explanation holds no prompt or answer text.
func (s *server) handleExplain(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !explainIDPattern.MatchString(id) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "that is not a request id"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	var out map[string]any
	status, err := s.getCompanion(ctx, "/freshness/explain/"+id, &out)
	switch {
	case status == http.StatusNotFound:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "The companion no longer remembers this request (it keeps only the most recent ones)."})
	case err != nil:
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
	default:
		writeJSON(w, http.StatusOK, out)
	}
}
