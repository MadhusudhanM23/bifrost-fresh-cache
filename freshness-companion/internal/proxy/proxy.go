// Package proxy is the HTTP front of the freshness companion. It forwards
// /v1/* to Bifrost unchanged and, only for cache hits on time-sensitive chat
// completions, decides whether the cached answer may be served.
//
// The companion never changes how Bifrost caches: it adds no cache headers
// to normal requests, and if it is removed clients talk to Bifrost directly
// with the same behaviour (minus the freshness checks).
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"freshness-companion/internal/agetrack"
	"freshness-companion/internal/changes"
	"freshness-companion/internal/decision"
	"freshness-companion/internal/decisionlog"
	"freshness-companion/internal/openai"
	"freshness-companion/internal/policy"
	"freshness-companion/internal/verified"
)

// Config wires the proxy.
type Config struct {
	Upstream  *Upstream
	Policy    *policy.Policy
	Engine    *decision.Engine
	Ages      *agetrack.Tracker
	Decisions *decisionlog.Log
	Refresh   RefreshStrategy
	Logger    *slog.Logger
	// UpstreamTimeout bounds one buffered Bifrost call (default 2 min).
	UpstreamTimeout time.Duration
	// MaxRequestBytes: larger chat requests pass through unchecked (default 8 MiB).
	MaxRequestBytes int64
	// MaxResponseBytes: larger responses pass through unchecked (default 8 MiB).
	MaxResponseBytes int64
	// EvidenceProvider and BifrostURL are shown on /healthz.
	EvidenceProvider string
	BifrostURL       string
	Now              func() time.Time

	// Changes remembers "this changed" notices (POST /freshness/changes).
	// Optional: nil keeps them in memory only.
	Changes *changes.Store
	// ChangeToken, when set, must be sent as "Authorization: Bearer <token>"
	// to POST a change notice.
	ChangeToken string
	// Clearer deletes cache entries and keys: it removes contradicted new
	// answers (on_contradicted uncache/correct) and serves a notice's
	// clear_cache_keys. Optional: without it those actions report an error.
	Clearer interface {
		CacheClearer
		KeyClearer
	}
	// SettleDelay is how long to wait before deleting a cache entry that
	// Bifrost has only just written: it writes in the background, and a
	// delete that arrives first would find nothing. Default 150ms.
	SettleDelay time.Duration
	// Audit turns on the wrong-match audit of semantic hits. Zero = off.
	Audit AuditConfig
	// RefreshShareWindow is how long a finished refresh is still shared with
	// requests that read the same stale entry just before it finished.
	// Default 2s; negative = share only while the refresh is running.
	RefreshShareWindow time.Duration

	// Verified is the curated answer table (nil = off).
	Verified *verified.Store
	// VerifiedScopeHeader is the request header compared with an entry's
	// "scope" (default x-bf-vk).
	VerifiedScopeHeader string
	// ExplainRing is how many request explanations are kept in memory
	// (default 2000; negative = none are kept).
	ExplainRing int
}

// Server is the companion's HTTP handler.
type Server struct {
	cfg     Config
	log     *slog.Logger
	started time.Time
	mux     *http.ServeMux
	mcp     atomic.Pointer[http.Handler] // set by EnableMCP (mcp.go)
	changes *changes.Store
	auditor *auditor
	flights *flights[*refreshOutcome]
	guard   *guardCounter

	explains      *explainRing
	idPrefix      string
	reqSeq        atomic.Int64
	scopeHeader   string
	staleVerified sync.Map // verified entry id -> time the source page contradicted it
	verifiedStats verifiedStats
}

// New validates cfg and builds the routes.
func New(cfg Config) (*Server, error) {
	if cfg.Upstream == nil || cfg.Policy == nil || cfg.Engine == nil || cfg.Ages == nil || cfg.Decisions == nil || cfg.Refresh == nil {
		return nil, errors.New("proxy: Upstream, Policy, Engine, Ages, Decisions and Refresh are required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.UpstreamTimeout <= 0 {
		cfg.UpstreamTimeout = 2 * time.Minute
	}
	if cfg.MaxRequestBytes <= 0 {
		cfg.MaxRequestBytes = 8 << 20
	}
	if cfg.MaxResponseBytes <= 0 {
		cfg.MaxResponseBytes = 8 << 20
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.SettleDelay <= 0 {
		cfg.SettleDelay = 150 * time.Millisecond
	}
	window := cfg.RefreshShareWindow
	switch {
	case window == 0:
		window = 2 * time.Second
	case window < 0:
		window = 0
	}
	ring := cfg.ExplainRing
	if ring == 0 {
		ring = DefaultExplainRing
	}
	scopeHeader := cfg.VerifiedScopeHeader
	if scopeHeader == "" {
		scopeHeader = DefaultVerifiedScopeHeader
	}
	s := &Server{cfg: cfg, explains: newExplainRing(max(ring, 0)), idPrefix: randomPrefix(), scopeHeader: scopeHeader, log: cfg.Logger, started: cfg.Now(), changes: cfg.Changes, guard: &guardCounter{since: cfg.Now().UTC(), byReason: map[string]int64{}}, flights: newFlights(window, cfg.Now, func(o *refreshOutcome) bool { return o != nil && o.err == nil })}
	if s.changes == nil {
		s.changes, _ = changes.Open(changes.Options{Now: cfg.Now})
	}
	s.auditor = newAuditor(cfg.Audit, s)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /freshness/decisions", s.handleDecisions)
	mux.HandleFunc("GET /freshness/audits", s.handleAudits)
	mux.HandleFunc("GET /freshness/guard", s.handleGuard)
	mux.HandleFunc("GET /freshness/explain", s.handleExplainList)
	mux.HandleFunc("GET /freshness/explain/{id}", s.handleExplain)
	mux.HandleFunc("GET /freshness/verified", s.handleVerified)
	mux.HandleFunc("GET /freshness/changes", s.handleChanges)
	mux.HandleFunc("POST /freshness/changes", s.handleChangeNotice)
	mux.HandleFunc("POST /mcp", s.handleMCP) // MCP server (see mcp.go); off until EnableMCP is called
	mux.HandleFunc("/v1/", s.handleV1)
	s.mux = mux
	return s, nil
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) handleV1(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions" {
		// Every chat response carries an id for GET /freshness/explain/{id}.
		id := s.newRequestID()
		w.Header().Set(HeaderRequestID, id)
		cw := &captureWriter{ResponseWriter: w}
		cw.beforeHeader = func(h http.Header) { setReceipt(h, s.cfg.Now()) }
		cw.onHeader = func(c *captureWriter) { s.explains.add(s.explain(id, c)) }
		w = cw
		defer func() {
			if cw.header == nil { // nothing was written
				s.explains.add(s.explain(id, cw))
			}
		}()
	}
	defer func() {
		if rec := recover(); rec != nil {
			s.log.Error("recovered from a panic in the proxy handler", "path", r.URL.Path)
			writeError(w, http.StatusBadGateway, "freshness companion internal error", "freshness_companion_error")
		}
	}()
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
		s.passthrough(w, r, r.Body, r.ContentLength)
		return
	}
	body, complete, err := readBounded(r.Body, s.cfg.MaxRequestBytes)
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read the request body", "invalid_request_error")
		return
	}
	if !complete {
		s.log.Debug("request body over the limit; forwarding without freshness checks", "limit_bytes", s.cfg.MaxRequestBytes)
		s.passthrough(w, r, io.MultiReader(bytes.NewReader(body), r.Body), r.ContentLength)
		return
	}
	chat, err := openai.ParseChatRequest(body)
	if err == nil {
		reason := s.cfg.Policy.Guard.Check(chat.HasToolResults, chat.ToolNames)
		if reason == "" {
			reason = s.cfg.Policy.Guard.CheckSensitive(chat.MessageTexts)
		}
		if reason != "" {
			s.guarded(w, r, body, reason)
			return
		}
		// A curated answer comes before the cache and the model (after the
		// guard, which always wins).
		if s.tryVerified(w, r, chat) {
			return
		}
	}
	if err != nil || chat.Stream {
		// Streams pass through untouched and are never checked (documented
		// limit); so do bodies that are not JSON chat requests.
		s.passthrough(w, r, bytes.NewReader(body), int64(len(body)))
		return
	}
	req := &Request{
		Method:   r.Method,
		Path:     r.URL.EscapedPath(),
		RawQuery: r.URL.RawQuery,
		Header:   r.Header.Clone(),
		Body:     body,
	}
	s.handleChat(w, r, req, chat)
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request, req *Request, chat openai.ChatRequest) {
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.UpstreamTimeout)
	defer cancel()
	resp, err := s.cfg.Upstream.Open(ctx, req.Method, req.Path, req.RawQuery, req.Header, bytes.NewReader(req.Body), int64(len(req.Body)))
	if err != nil {
		s.upstreamFailed(w, err)
		return
	}
	defer resp.Body.Close()
	if isEventStream(resp.Header) {
		s.observe(req.Header, resp.Header)
		s.stream(w, resp, nil)
		return
	}
	body, complete, err := readBounded(resp.Body, s.cfg.MaxResponseBytes)
	if err != nil {
		s.upstreamFailed(w, err)
		return
	}
	if !complete {
		s.observe(req.Header, resp.Header)
		s.stream(w, resp, body)
		return
	}
	cached := &Response{StatusCode: resp.StatusCode, Header: resp.Header, Body: body}
	s.observe(req.Header, cached.Header)
	if cached.StatusCode != http.StatusOK {
		s.write(w, cached, nil)
		return
	}
	eff, timeSensitive := s.cfg.Policy.Resolve(chat.LastUserText)
	if !cached.CacheHit() {
		if timeSensitive && eff.VerifyNewAnswers && len(eff.Facts) > 0 && isCacheMiss(cached.Header) {
			s.verifyNew(w, r, req, cached, eff)
			return
		}
		s.write(w, cached, nil)
		return
	}
	if !timeSensitive {
		s.serveCached(ctx, w, req, chat.LastUserText, cached, nil) // not time-sensitive: untouched, no evidence fetch
		return
	}
	s.decide(w, r, req, chat.LastUserText, cached, eff)
}

// serveCached sends a cached answer that is being served as it is. A risky
// semantic hit is checked first (the gate): a wrong one is replaced by the
// model's own answer. A wrong-match audit is started when one is due. It
// returns what the gate did ("" when it did not apply).
func (s *Server) serveCached(ctx context.Context, w http.ResponseWriter, req *Request, question string, cached *Response, decorate func(http.Header)) string {
	v := s.auditor.gate(ctx, req, question, cached)
	if v.kind == GateBlocked {
		// The cached answer is wrong: it is not served, and neither are its
		// freshness headers, which describe an entry that no longer exists.
		s.write(w, v.fresh, func(h http.Header) {
			h.Set(HeaderGate, GateBlocked)
			h.Set(HeaderAudit, v.auditID)
		})
		return v.kind
	}
	// A hit the gate already checked needs no audit as well.
	auditID := ""
	if v.kind == "" || v.kind == GateUnverified {
		auditID = s.auditor.maybeAudit(req, question, cached)
	}
	s.write(w, cached, func(h http.Header) {
		if decorate != nil {
			decorate(h)
		}
		if v.kind != "" {
			h.Set(HeaderGate, v.kind)
			if v.err != "" {
				h.Set(HeaderError, headerValue(v.err))
			}
		}
		if auditID != "" {
			h.Set(HeaderAudit, auditID)
		}
	})
	return v.kind
}

// changeFor returns the change notice that makes an entry suspect: one that
// covers the request and arrived after the entry was written (or after an
// unknown time, when the entry's age is unknown).
func (s *Server) changeFor(eff policy.Effective, age time.Duration, ageKnown bool) (changes.Notice, bool) {
	n, ok := s.changes.Covering(eff.Categories, eff.FactNames())
	if !ok {
		return changes.Notice{}, false
	}
	if ageKnown && !n.Time.After(s.cfg.Now().Add(-age)) {
		return changes.Notice{}, false // the entry was written after the notice
	}
	return n, true
}

// decide runs the freshness decision for a time-sensitive cache hit and
// carries out its action.
func (s *Server) decide(w http.ResponseWriter, r *http.Request, req *Request, question string, cached *Response, eff policy.Effective) {
	start := time.Now()
	cacheID := cached.CacheID()
	var age time.Duration
	ageKnown := false
	if cacheID != "" {
		age, ageKnown = s.cfg.Ages.Age(cacheID)
	}
	in := decision.Input{
		Policy:     eff,
		CacheID:    cacheID,
		Age:        age,
		AgeKnown:   ageKnown,
		CachedBody: inspectable(cached, s.cfg.MaxResponseBytes),
		Question:   question, // only for a discovery query; never stored
	}
	notice, hasNotice := s.changeFor(eff, age, ageKnown)
	if hasNotice {
		in.Change = &decision.ChangeNotice{Subject: notice.Subject(), At: notice.Time}
	}
	d := s.cfg.Engine.Decide(r.Context(), in)
	rec := s.newRecord(d, cached)
	rec.AnswerOf = "cached"
	if hasNotice {
		rec.ChangeNotice = notice.ID
	}
	withNotice := func(h http.Header) {
		if hasNotice {
			h.Set(HeaderChangeNotice, notice.ID)
		}
	}

	switch d.Action {
	case decision.Serve:
		rec.Action = ActionServed
		rec.Gate = s.serveCached(r.Context(), w, req, question, cached, func(h http.Header) { annotate(h, d, ActionServed); withNotice(h) })
	case decision.ServeWithWarning:
		rec.Action = ActionServedWithWarning
		rec.Gate = s.serveCached(r.Context(), w, req, question, cached, func(h http.Header) { annotate(h, d, ActionServedWithWarning); withNotice(h) })
	default: // decision.Refresh: the cached answer is not served
		rec.RefreshStrategy = s.cfg.Refresh.Name()
		s.refresh(w, r, req, cacheID, d, eff, &rec, withNotice)
	}
	rec.LatencyMS = float64(time.Since(start).Microseconds()) / 1000
	if err := s.cfg.Decisions.Append(rec); err != nil {
		s.log.Warn("could not write the decision log", "error", err)
	}
	s.log.Info("freshness decision", "cache_id", rec.CacheID, "category", rec.Category, "outcome", rec.Outcome,
		"action", rec.Action, "provider", rec.Provider, "latency_ms", rec.LatencyMS)
}

// refreshOutcome is everything one refresh produced. Requests that share a
// refresh all answer from it.
type refreshOutcome struct {
	err       error // the refresh failed: nothing fresh was obtained
	sent      http.Header
	freshID   string    // cache id of the fresh answer
	badStatus string    // set when the re-request came back with a non-2xx status
	fresh     *Response // the model's new answer
	checked   bool      // value and check are set
	value     string
	check     string
	final     *Response // what to serve: fresh, or the corrected answer
	action    string
	corr      correctionResult
}

// doRefresh gets a fresh answer for a stale entry and, when that answer still
// disagrees with the evidence, acts on on_contradicted.
func (s *Server) doRefresh(ctx context.Context, req *Request, cacheID string, d decision.Decision, eff policy.Effective) *refreshOutcome {
	res, err := s.cfg.Refresh.Refresh(ctx, req, cacheID)
	if err != nil {
		return &refreshOutcome{err: err}
	}
	for _, id := range res.Cleared {
		if err := s.cfg.Ages.Forget(id); err != nil {
			s.log.Warn("could not update the age store", "error", err)
		}
	}
	fresh := res.Response
	if res.ServedFromCache {
		// Served from cache again under the id that was just cleared: either a
		// concurrent request already wrote the fresh answer, or the stale
		// entry is still there. The source decides which.
		if fresh.StatusCode < 200 || fresh.StatusCode > 299 {
			return &refreshOutcome{err: fmt.Errorf("re-request was served from cache again (entry %q)%s", fresh.CacheID(), res.ClearErrors)}
		}
		if _, check, ok := decision.CheckRefreshed(d, inspectable(fresh, s.cfg.MaxResponseBytes)); !ok || check != decision.RefreshedMatches {
			return &refreshOutcome{err: fmt.Errorf("re-request was served from cache again (entry %q) and that answer is not confirmed by the source%s", fresh.CacheID(), res.ClearErrors)}
		}
	}
	s.observe(res.SentHeader, fresh.Header)
	out := &refreshOutcome{sent: res.SentHeader, fresh: fresh, final: fresh, freshID: fresh.CacheID(), action: ActionRefreshed}
	if fresh.StatusCode < 200 || fresh.StatusCode > 299 {
		// The stale answer is still not served; Bifrost's own error goes back as-is.
		out.badStatus = "re-request returned HTTP " + strconv.Itoa(fresh.StatusCode)
		out.action = ActionRefreshFailed
		return out
	}
	out.value, out.check, out.checked = decision.CheckRefreshed(d, inspectable(fresh, s.cfg.MaxResponseBytes))
	// The model answered again and still disagrees with the evidence (its
	// context is outdated, or it is simply wrong): act on on_contradicted.
	if out.checked && out.check == decision.RefreshedDiffers && eff.OnContradicted != policy.ContradictedReport {
		out.corr = s.handleContradiction(ctx, req, d, eff.OnContradicted, fresh)
		switch out.corr.Kind {
		case "corrected":
			out.final, out.action = out.corr.Resp, ActionCorrected
		case "uncached":
			out.action = ActionUncached
		}
	}
	return out
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request, req *Request, cacheID string, d decision.Decision, eff policy.Effective, rec *decisionlog.Record, extra func(http.Header)) {
	if d.Discovered && eff.OnContradicted == policy.ContradictedCorrect {
		// Trust downgrade: a page that search found may trigger a refresh,
		// but its text never goes into a prompt. correct becomes uncache.
		eff.OnContradicted = policy.ContradictedUncache
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.UpstreamTimeout)
	defer cancel()
	// Requests that need the same refresh share one. The refresh itself must
	// not die with the request that happened to start it.
	out, shared, err := s.flights.do(ctx, askKey(req, cacheID), func() *refreshOutcome {
		fctx, fcancel := context.WithTimeout(context.WithoutCancel(r.Context()), s.cfg.UpstreamTimeout)
		defer fcancel()
		return s.doRefresh(fctx, req, cacheID, d, eff)
	})
	if err != nil {
		out = &refreshOutcome{err: err}
	}
	rec.SharedRefresh = shared
	if out.err != nil {
		rec.Action = ActionRefreshFailed
		rec.Error = oneLine(out.err.Error(), 300)
		s.log.Warn("refresh failed; the cached answer was not served", "cache_id", cacheID, "outcome", d.Outcome, "error", rec.Error)
		s.refuse(w, d, rec.Error)
		return
	}
	rec.RefreshedCacheID = out.freshID
	if out.badStatus != "" {
		rec.Action = ActionRefreshFailed
		rec.Error = out.badStatus
		s.write(w, out.fresh, func(h http.Header) {
			annotate(h, d, ActionRefreshFailed)
			h.Set(HeaderError, rec.Error)
			if shared {
				h.Set(HeaderSharedRefresh, "true")
			}
		})
		return
	}
	rec.Action = ActionRefreshed
	if out.checked {
		rec.RefreshedCheck = out.check
	}
	final := out.final
	action := s.applyCorrection(rec, out.corr, &final, ActionRefreshed)
	s.write(w, final, func(h http.Header) {
		annotate(h, d, action)
		if out.checked {
			if out.value != "" {
				h.Set(HeaderRefreshedValue, headerValue(out.value))
			}
			h.Set(HeaderRefreshedCheck, out.check)
		}
		out.corr.annotate(h)
		if shared {
			h.Set(HeaderSharedRefresh, "true")
		}
		if extra != nil {
			extra(h)
		}
	})
}

// correctionResult is what handleContradiction did to an answer that
// disagrees with the evidence.
type correctionResult struct {
	Kind  string    // "" (nothing done), reported, uncached or corrected
	Resp  *Response // the corrected answer to serve, when Kind is corrected
	Value string    // value stated by the corrected answer
	Check string    // matches_current | differs_from_current | no_value
	Err   string
}

func (c correctionResult) annotate(h http.Header) {
	if c.Kind == "" {
		return
	}
	h.Set(HeaderCorrection, c.Kind)
	if c.Value != "" {
		h.Set(HeaderCorrectedValue, headerValue(c.Value))
	}
	if c.Check != "" {
		h.Set(HeaderCorrectedCheck, c.Check)
	}
	if c.Err != "" {
		h.Set(HeaderError, headerValue(c.Err))
	}
}

// applyCorrection records a correction in the decision record, swaps in the
// corrected answer when there is one and returns the action to report.
func (s *Server) applyCorrection(rec *decisionlog.Record, c correctionResult, final **Response, action string) string {
	rec.Correction, rec.CorrectedValue, rec.CorrectedCheck = c.Kind, c.Value, c.Check
	if c.Err != "" {
		rec.Error = c.Err
	}
	switch c.Kind {
	case "corrected":
		*final = c.Resp
		return ActionCorrected
	case "uncached":
		return ActionUncached
	}
	return action
}

// dropEntry deletes a cache entry that Bifrost has just written. Bifrost
// writes in the background and a delete of an entry that is not there yet
// succeeds without doing anything, so it waits a moment first.
func (s *Server) dropEntry(ctx context.Context, cacheID string) error {
	if cacheID == "" {
		return errors.New("the response names no cache entry")
	}
	if s.cfg.Clearer == nil {
		return errors.New("no cache clearer is configured")
	}
	select {
	case <-time.After(s.cfg.SettleDelay):
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := s.cfg.Clearer.Clear(ctx, cacheID); err != nil {
		return err
	}
	if err := s.cfg.Ages.Forget(cacheID); err != nil {
		s.log.Warn("could not update the age store", "error", err)
	}
	return nil
}

// handleContradiction acts on a fresh answer that states a different value
// than the evidence. bad is that answer; its cache entry was written moments
// ago. It never serves anything itself.
//
// uncache deletes the entry (a second delete follows later in case the first
// beat the write), so nobody else is served the wrong answer. correct also
// asks the model again with the verified value in front of the conversation;
// that answer is used only if it matches the evidence.
func (s *Server) handleContradiction(ctx context.Context, req *Request, d decision.Decision, mode policy.OnContradicted, bad *Response) correctionResult {
	badID := bad.CacheID()
	uncache := func(extra string) correctionResult {
		if err := s.dropEntry(ctx, badID); err != nil {
			return correctionResult{Kind: "reported", Err: "could not uncache: " + oneLine(err.Error(), 200)}
		}
		time.AfterFunc(8*s.cfg.SettleDelay, func() {
			c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = s.cfg.Clearer.Clear(c, badID)
		})
		return correctionResult{Kind: "uncached", Err: extra}
	}
	if mode != policy.ContradictedCorrect {
		return uncache("")
	}
	prompt, ok := decision.CorrectionPrompt(d)
	if !ok {
		return uncache("no verified value to correct with")
	}
	body, err := openai.PrependSystemMessage(req.Body, prompt)
	if err != nil {
		return uncache("could not build the corrected request: " + err.Error())
	}
	if err := s.dropEntry(ctx, badID); err != nil {
		return correctionResult{Kind: "reported", Err: "could not correct: " + oneLine(err.Error(), 200)}
	}
	out := req.Clone()
	out.Body = body
	var resp *Response
	for attempt := 0; attempt < 3; attempt++ {
		resp, err = s.cfg.Upstream.Send(ctx, out)
		if err != nil || !resp.CacheHit() {
			break
		}
		// Served from cache again (the bad entry landed after the delete, or
		// a similar entry matched): delete it and ask once more.
		if cerr := s.cfg.Clearer.Clear(ctx, resp.CacheID()); cerr != nil {
			err = cerr
			break
		}
		resp = nil
	}
	if err != nil || resp == nil || resp.StatusCode < 200 || resp.StatusCode > 299 {
		return correctionResult{Kind: "reported", Err: "the corrected request failed"}
	}
	s.observe(out.Header, resp.Header)
	value, check, ok := decision.CheckRefreshed(d, inspectable(resp, s.cfg.MaxResponseBytes))
	if !ok || check != decision.RefreshedMatches {
		// Even with the verified value the model disagrees: keep neither entry.
		if err := s.dropEntry(ctx, resp.CacheID()); err != nil {
			s.log.Warn("could not remove a corrected answer that still disagrees", "error", err)
		}
		return correctionResult{Kind: "uncached", Value: value, Check: check, Err: "the corrected answer still disagrees with the source"}
	}
	return correctionResult{Kind: "corrected", Resp: resp, Value: value, Check: check}
}

// verifyNew checks a fresh answer (a cache miss) against the watched facts
// before the answer is reused, and acts on a contradiction.
func (s *Server) verifyNew(w http.ResponseWriter, r *http.Request, req *Request, miss *Response, eff policy.Effective) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.UpstreamTimeout)
	defer cancel()
	d := s.cfg.Engine.CheckNewAnswer(ctx, eff, inspectable(miss, s.cfg.MaxResponseBytes))
	rec := s.newRecord(d, miss)
	rec.AnswerOf = "new"
	final := miss
	action := ActionServed
	var corr correctionResult
	if d.Outcome == decision.Contradicted {
		switch d.Action {
		case decision.Uncache, decision.Correct:
			corr = s.handleContradiction(ctx, req, d, eff.OnContradicted, miss)
			action = s.applyCorrection(&rec, corr, &final, action)
		default:
			corr = correctionResult{Kind: "reported"}
			rec.Correction = "reported"
		}
	}
	rec.Action = action
	s.write(w, final, func(h http.Header) {
		annotateNew(h, d, action)
		corr.annotate(h)
	})
	rec.LatencyMS = float64(time.Since(start).Microseconds()) / 1000
	if err := s.cfg.Decisions.Append(rec); err != nil {
		s.log.Warn("could not write the decision log", "error", err)
	}
	s.log.Info("freshness decision", "cache_id", rec.CacheID, "category", rec.Category, "outcome", rec.Outcome,
		"action", rec.Action, "answer", "new", "provider", rec.Provider, "latency_ms", rec.LatencyMS)
}

func (s *Server) newRecord(d decision.Decision, cached *Response) decisionlog.Record {
	rec := decisionlog.Record{
		Time:           s.cfg.Now().UTC(),
		CacheID:        cached.CacheID(),
		HitType:        cached.Header.Get(HeaderCacheHitType),
		Similarity:     cached.Header.Get(HeaderCacheSimilarity),
		Category:       strings.Join(d.Categories, ","),
		Fact:           d.Fact,
		Outcome:        string(d.Outcome),
		Reason:         d.Reason,
		SourceURL:      d.SourceURL,
		Provider:       d.Provider,
		CachedValue:    d.CachedValue,
		CurrentValue:   d.CurrentValue,
		EvidenceCached: d.EvidenceCached,
		Discovered:     d.Discovered,
		Discovery:      d.DiscoveryResult,
		DiscoveryQuery: d.DiscoveryQuery,
		DiscoveryRank:  d.DiscoveryRank,
		DiscoveryScore: d.DiscoveryScore,
		FoundBy:        d.FoundBy,
	}
	if !d.CheckedAt.IsZero() {
		t := d.CheckedAt.UTC()
		rec.CheckedAt = &t
	}
	if d.AgeKnown {
		secs := d.Age.Seconds()
		rec.AgeSeconds = &secs
	}
	return rec
}

// observe records the write time of the entry a cache miss names. A request
// sent with x-bf-cache-no-store: true still gets an id on the miss, but
// Bifrost writes nothing, so it is not recorded.
func (s *Server) observe(sent, got http.Header) {
	if !isCacheMiss(got) {
		return
	}
	id := strings.TrimSpace(got.Get(HeaderCacheID))
	if id == "" || strings.EqualFold(strings.TrimSpace(sent.Get(HeaderCacheNoStore)), "true") {
		return
	}
	if err := s.cfg.Ages.RecordMiss(id); err != nil {
		s.log.Warn("could not persist a cache entry age", "cache_id", id, "error", err)
	}
}

func (s *Server) passthrough(w http.ResponseWriter, r *http.Request, body io.Reader, contentLength int64) {
	resp, err := s.cfg.Upstream.Open(r.Context(), r.Method, r.URL.EscapedPath(), r.URL.RawQuery, r.Header, body, contentLength)
	if err != nil {
		s.upstreamFailed(w, err)
		return
	}
	defer resp.Body.Close()
	s.observe(r.Header, resp.Header)
	s.stream(w, resp, nil)
}

// stream copies a live response, flushing as data arrives (SSE-safe).
func (s *Server) stream(w http.ResponseWriter, resp *http.Response, prefix []byte) {
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	rc := http.NewResponseController(w)
	if len(prefix) > 0 {
		if _, err := w.Write(prefix); err != nil {
			return
		}
		_ = rc.Flush()
	}
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			_ = rc.Flush()
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				s.log.Debug("upstream stream ended with an error", "error", err)
			}
			return
		}
	}
}

func (s *Server) write(w http.ResponseWriter, resp *Response, decorate func(http.Header)) {
	h := w.Header()
	copyResponseHeaders(h, resp.Header)
	if decorate != nil {
		decorate(h)
	}
	h.Set("Content-Length", strconv.Itoa(len(resp.Body)))
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(resp.Body)
}

// refuse answers 502 instead of serving a cached answer that must not be
// served, when no fresh answer could be obtained.
func (s *Server) refuse(w http.ResponseWriter, d decision.Decision, reason string) {
	annotate(w.Header(), d, ActionRefreshFailed)
	w.Header().Set(HeaderError, headerValue(reason))
	what := "stale"
	switch d.Outcome {
	case decision.UnableToVerify:
		what = "unverified"
	case decision.Unknown:
		what = "of unknown freshness"
	}
	writeError(w, http.StatusBadGateway,
		"freshness companion: the cached answer was not served because it is "+what+", and a fresh answer could not be obtained",
		"freshness_refresh_failed")
}

func (s *Server) upstreamFailed(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	if errors.Is(err, context.DeadlineExceeded) {
		status = http.StatusGatewayTimeout
	}
	s.log.Warn("bifrost request failed", "error", err)
	writeError(w, status, "freshness companion could not get a response from Bifrost", "bifrost_unavailable")
}

type healthResponse struct {
	Status              string `json:"status"`
	BifrostURL          string `json:"bifrost_url"`
	EvidenceProvider    string `json:"evidence_provider"`
	RefreshStrategy     string `json:"refresh_strategy"`
	PolicyCategories    int    `json:"policy_categories"`
	PolicyFacts         int    `json:"policy_facts"`
	TrackedCacheEntries int    `json:"tracked_cache_entries"`
	AuditEnabled        bool   `json:"audit_enabled"`
	StartedAt           string `json:"started_at"`
	UptimeSeconds       int64  `json:"uptime_seconds"`
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{
		Status:              "ok",
		BifrostURL:          s.cfg.BifrostURL,
		EvidenceProvider:    s.cfg.EvidenceProvider,
		RefreshStrategy:     s.cfg.Refresh.Name(),
		PolicyCategories:    len(s.cfg.Policy.Categories),
		PolicyFacts:         len(s.cfg.Policy.Facts),
		TrackedCacheEntries: s.cfg.Ages.Len(),
		AuditEnabled:        s.auditor != nil,
		StartedAt:           s.started.UTC().Format(time.RFC3339),
		UptimeSeconds:       int64(s.cfg.Now().Sub(s.started).Seconds()),
	})
}

const maxDecisionsLimit = 1000

func (s *Server) handleDecisions(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if q := r.URL.Query().Get("limit"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer", "invalid_request_error")
			return
		}
		limit = min(n, maxDecisionsLimit)
	}
	recs := s.cfg.Decisions.Recent(limit)
	writeJSON(w, http.StatusOK, struct {
		Count     int                  `json:"count"`
		Decisions []decisionlog.Record `json:"decisions"`
	}{Count: len(recs), Decisions: recs})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "encoding error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// writeError writes an OpenAI-style error body.
func writeError(w http.ResponseWriter, status int, message, kind string) {
	type apiError struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	}
	writeJSON(w, status, struct {
		Error apiError `json:"error"`
	}{Error: apiError{Message: message, Type: kind, Code: kind}})
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}
