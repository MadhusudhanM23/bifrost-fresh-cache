package proxy

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"freshness-companion/internal/auditlog"
	"freshness-companion/internal/openai"
)

// Audit comparison modes.
const (
	// CompareExact: the two answers are the same after lower-casing and
	// dropping punctuation and extra spaces. Free, but strict: two correct
	// answers worded differently count as different, so the rate it gives is
	// an upper bound unless the model is deterministic or answers are short.
	CompareExact = "exact"
	// CompareJudge: a model is asked whether the two answers say the same
	// thing. It costs one more model call per audit and sends the question
	// and both answers to the model provider.
	CompareJudge = "judge"
)

// HeaderAudit marks a hit that is being audited; its value is the audit id.
const HeaderAudit = "X-Freshness-Audit"

// AuditConfig turns on the wrong-match audit. The zero value is off.
type AuditConfig struct {
	// Rate is the fraction (0..1) of semantic hits that are audited.
	Rate float64
	// NearMargin: a semantic hit that scored within this margin above the
	// threshold is always audited, because wrong matches gather there.
	NearMargin float64
	// MaxPerMinute caps audits (each costs a model call); extra hits are
	// not audited. Default 30.
	MaxPerMinute int
	// MaxConcurrent caps audits in flight. Default 4.
	MaxConcurrent int
	// Compare is CompareExact (default) or CompareJudge.
	Compare string
	// JudgeModel is the model that judges; empty uses the audited request's model.
	JudgeModel string
	// Timeout bounds one audit. Default 60s.
	Timeout time.Duration
	// Log receives the results. Required to enable auditing.
	Log *auditlog.Log
	// Sample returns a number in [0,1); tests inject one.
	Sample func() float64

	// VerifyBelow turns on the pre-serve gate: a semantic hit that scored
	// below it is checked against a fresh answer BEFORE it is served, and a
	// wrong one is removed and replaced. 0 = off. Poisoned or look-alike
	// entries match only near the threshold, so a value a little above the
	// threshold (e.g. 0.90 with a 0.80 threshold) covers the risky band.
	VerifyBelow float64
	// TrustFor is how long a verified (entry, caller, question) pair is
	// served without another check. Default 1h.
	TrustFor time.Duration
	// MaxTrusted bounds the trusted pairs kept. Default 10000.
	MaxTrusted int
	// GateMaxPerMinute caps gate checks (each costs a model call); a hit over
	// the cap is served unchecked and says so. Default 120.
	GateMaxPerMinute int
}

func (c AuditConfig) enabled() bool {
	return c.Log != nil && (c.Rate > 0 || c.NearMargin > 0 || c.VerifyBelow > 0)
}

// AuditStats are the counters shown by GET /freshness/audits.
type AuditStats struct {
	Scheduled int64 `json:"scheduled"`
	Completed int64 `json:"completed"`
	Same      int64 `json:"same"`
	Different int64 `json:"different"`
	Errors    int64 `json:"errors"`
	// Skipped counts hits chosen for an audit that was not run because the
	// per-minute or in-flight cap was reached.
	Skipped int64 `json:"skipped"`
	// The pre-serve gate: hits it checked and let through, trusted without a
	// new check, blocked, or could not check.
	GateVerified   int64 `json:"gate_verified"`
	GateTrusted    int64 `json:"gate_trusted"`
	GateBlocked    int64 `json:"gate_blocked"`
	GateUnverified int64 `json:"gate_unverified"`
}

type auditor struct {
	cfg  AuditConfig
	srv  *Server
	sem  chan struct{}
	seq  atomic.Int64
	pref string

	mu          sync.Mutex
	windowStart time.Time
	inWindow    int

	scheduled, completed, same, different, errs, skipped atomic.Int64

	trust                                                  *trustSet
	gateFlights                                            *flights[gateVerdict]
	gateBudget                                             *perMinute
	gateVerified, gateTrusted, gateBlocked, gateUnverified atomic.Int64
}

func newAuditor(cfg AuditConfig, s *Server) *auditor {
	if !cfg.enabled() {
		return nil
	}
	if cfg.MaxPerMinute <= 0 {
		cfg.MaxPerMinute = 30
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 4
	}
	if cfg.Compare == "" {
		cfg.Compare = CompareExact
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 60 * time.Second
	}
	if cfg.Sample == nil {
		cfg.Sample = rand.Float64
	}
	var b [3]byte
	_, _ = crand.Read(b[:])
	if cfg.TrustFor <= 0 {
		cfg.TrustFor = time.Hour
	}
	if cfg.MaxTrusted <= 0 {
		cfg.MaxTrusted = 10000
	}
	if cfg.GateMaxPerMinute <= 0 {
		cfg.GateMaxPerMinute = 120
	}
	return &auditor{cfg: cfg, srv: s, sem: make(chan struct{}, cfg.MaxConcurrent), pref: hex.EncodeToString(b[:]),
		trust:       newTrustSet(cfg.TrustFor, cfg.MaxTrusted),
		gateFlights: newFlights(2*time.Second, s.cfg.Now, func(v gateVerdict) bool { return v.kind != GateUnverified }),
		gateBudget:  &perMinute{limit: cfg.GateMaxPerMinute}}
}

func (a *auditor) stats() AuditStats {
	if a == nil {
		return AuditStats{}
	}
	return AuditStats{Scheduled: a.scheduled.Load(), Completed: a.completed.Load(), Same: a.same.Load(),
		Different: a.different.Load(), Errors: a.errs.Load(), Skipped: a.skipped.Load(),
		GateVerified: a.gateVerified.Load(), GateTrusted: a.gateTrusted.Load(),
		GateBlocked: a.gateBlocked.Load(), GateUnverified: a.gateUnverified.Load()}
}

// allow applies the per-minute cap.
func (a *auditor) allow(now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if now.Sub(a.windowStart) >= time.Minute {
		a.windowStart, a.inWindow = now, 0
	}
	if a.inWindow >= a.cfg.MaxPerMinute {
		return false
	}
	a.inWindow++
	return true
}

func parseScore(h http.Header, name string) *float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(h.Get(name)), 64)
	if err != nil {
		return nil
	}
	return &v
}

// maybeAudit decides whether a served semantic hit is audited and, if so,
// starts the audit in the background and returns its id. The client never
// waits for it.
func (a *auditor) maybeAudit(req *Request, question string, cached *Response) string {
	if a == nil || !strings.EqualFold(strings.TrimSpace(cached.Header.Get(HeaderCacheHitType)), "semantic") {
		return ""
	}
	sim, thr := parseScore(cached.Header, HeaderCacheSimilarity), parseScore(cached.Header, HeaderCacheThreshold)
	near := sim != nil && thr != nil && a.cfg.NearMargin > 0 && *sim-*thr <= a.cfg.NearMargin
	if !near && !(a.cfg.Rate > 0 && a.cfg.Sample() < a.cfg.Rate) {
		return ""
	}
	if !a.allow(a.srv.cfg.Now()) {
		a.skipped.Add(1)
		return ""
	}
	select {
	case a.sem <- struct{}{}:
	default:
		a.skipped.Add(1)
		return ""
	}
	id := fmt.Sprintf("aud-%s-%d", a.pref, a.seq.Add(1))
	a.scheduled.Add(1)
	cachedBody := inspectable(cached, a.srv.cfg.MaxResponseBytes)
	rec := auditlog.Record{ID: id, Time: a.srv.cfg.Now().UTC(), CacheID: cached.CacheID(), Similarity: sim, Threshold: thr, Near: near, Compare: a.cfg.Compare}
	go func() {
		defer func() { <-a.sem }()
		a.run(req.Clone(), question, cachedBody, rec)
	}()
	return id
}

func (a *auditor) run(req *Request, question string, cachedBody []byte, rec auditlog.Record) {
	start := time.Now()
	defer func() {
		if r := recover(); r != nil {
			rec.Verdict, rec.Error = auditlog.Error, "audit panicked"
		}
		rec.LatencyMS = float64(time.Since(start).Microseconds()) / 1000
		a.completed.Add(1)
		switch rec.Verdict {
		case auditlog.Same:
			a.same.Add(1)
		case auditlog.Different:
			a.different.Add(1)
		default:
			a.errs.Add(1)
		}
		if err := a.srv.cfg.Audit.Log.Append(rec); err != nil {
			a.srv.log.Warn("could not write the audit log", "error", err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), a.cfg.Timeout)
	defer cancel()

	fail := func(format string, args ...any) {
		rec.Verdict, rec.Error = auditlog.Error, fmt.Sprintf(format, args...)
	}

	// A throwaway cache key finds no entry and no-store writes none, whatever
	// default_cache_key Bifrost has, so this is always a real model call.
	fresh, err := a.srv.cfg.Upstream.Send(ctx, a.bypass(req, "audit-"+rec.ID))
	switch {
	case err != nil:
		fail("fresh answer failed: %s", oneLine(err.Error(), 200))
		return
	case fresh.StatusCode != http.StatusOK:
		fail("fresh answer returned HTTP %d", fresh.StatusCode)
		return
	case fresh.CacheHit():
		fail("the fresh request was served from cache, so it proves nothing")
		return
	}
	cachedText, err1 := openai.AnswerText(cachedBody)
	freshText, err2 := openai.AnswerText(inspectable(fresh, a.srv.cfg.MaxResponseBytes))
	if err1 != nil || err2 != nil {
		fail("an answer has no text to compare")
		return
	}
	same, reason, err := a.compare(ctx, req, question, cachedText, freshText)
	if err != nil {
		fail("%s", oneLine(err.Error(), 200))
		return
	}
	rec.Reason = reason
	rec.Verdict = auditlog.Different
	if same {
		rec.Verdict = auditlog.Same
	}
}

// bypass is a copy of req that can neither read nor write a cache entry.
func (a *auditor) bypass(req *Request, key string) *Request {
	out := req.Clone()
	out.Header.Set("X-Bf-Cache-Key", key)
	out.Header.Set(HeaderCacheNoStore, "true")
	out.Header.Del(HeaderCacheType)
	return out
}

func (a *auditor) compare(ctx context.Context, req *Request, question, cachedText, freshText string) (same bool, reason string, err error) {
	if a.cfg.Compare != CompareJudge {
		same = normalizeAnswer(cachedText) == normalizeAnswer(freshText)
		if same {
			return true, "answers are identical after normalising case and punctuation", nil
		}
		return false, "answers differ", nil
	}
	return a.judge(ctx, req, question, cachedText, freshText)
}

func normalizeAnswer(s string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}), " ")
}

const judgeInstruction = "You compare two answers to the same question. Reply with exactly one word: " +
	"SAME if both answers give the same information and advice, or DIFFERENT if they would lead the " +
	"reader to do or believe something different. Ignore wording, order and length."

// judge asks a model whether the two answers agree.
func (a *auditor) judge(ctx context.Context, req *Request, question, cachedText, freshText string) (bool, string, error) {
	model := a.cfg.JudgeModel
	if model == "" {
		var m struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(req.Body, &m)
		model = m.Model
	}
	if model == "" {
		return false, "", fmt.Errorf("judge mode needs a model: set -audit-judge-model")
	}
	body, err := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": judgeInstruction},
			{"role": "user", "content": "Question:\n" + question + "\n\nAnswer A:\n" + cachedText + "\n\nAnswer B:\n" + freshText},
		},
	})
	if err != nil {
		return false, "", err
	}
	jr := a.bypass(req, "audit-judge-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	jr.Body = bytes.Clone(body)
	jr.Header.Set("Content-Type", "application/json")
	resp, err := a.srv.cfg.Upstream.Send(ctx, jr)
	if err != nil {
		return false, "", fmt.Errorf("judge call failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK || resp.CacheHit() {
		return false, "", fmt.Errorf("judge call returned HTTP %d", resp.StatusCode)
	}
	text, err := openai.AnswerText(inspectable(resp, a.srv.cfg.MaxResponseBytes))
	if err != nil {
		return false, "", fmt.Errorf("judge gave no text")
	}
	switch word := strings.ToUpper(strings.Trim(strings.Fields(text + " ")[0], ".,:;!\"'")); word {
	case "SAME":
		return true, "judge: same", nil
	case "DIFFERENT":
		return false, "judge: different", nil
	default:
		return false, "", fmt.Errorf("judge answered %q, not SAME or DIFFERENT", oneLine(text, 40))
	}
}
