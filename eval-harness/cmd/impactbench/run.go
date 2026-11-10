package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"bifrost-eval-harness/internal/clock"
	"bifrost-eval-harness/internal/stats"
)

// Arm is one way of reaching the same Bifrost.
type Arm struct {
	Name    string            `json:"name"`
	Label   string            `json:"label"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"extra_headers,omitempty"`
}

// MockStats are the counters of tools/mock-openai (GET /mock/stats).
type MockStats struct {
	ChatCalls      int `json:"chat_calls"`
	EmbeddingCalls int `json:"embedding_calls"`
	UnknownCalls   int `json:"unknown_calls"`
	RejectedCalls  int `json:"rejected_calls"`
}

// CompanionStats are the counters of GET /freshness/audits and /freshness/guard.
type CompanionStats struct {
	Scheduled      int `json:"scheduled"`
	Completed      int `json:"completed"`
	Same           int `json:"same"`
	Different      int `json:"different"`
	Errors         int `json:"errors"`
	Skipped        int `json:"skipped"`
	GateVerified   int `json:"gate_verified"`
	GateTrusted    int `json:"gate_trusted"`
	GateBlocked    int `json:"gate_blocked"`
	GateUnverified int `json:"gate_unverified"`
	GuardBypassed  int `json:"guard_bypassed"`
}

func (a CompanionStats) minus(b CompanionStats) CompanionStats {
	return CompanionStats{a.Scheduled - b.Scheduled, a.Completed - b.Completed, a.Same - b.Same, a.Different - b.Different,
		a.Errors - b.Errors, a.Skipped - b.Skipped, a.GateVerified - b.GateVerified, a.GateTrusted - b.GateTrusted,
		a.GateBlocked - b.GateBlocked, a.GateUnverified - b.GateUnverified, a.GuardBypassed - b.GuardBypassed}
}

func (a MockStats) minus(b MockStats) MockStats {
	return MockStats{a.ChatCalls - b.ChatCalls, a.EmbeddingCalls - b.EmbeddingCalls, a.UnknownCalls - b.UnknownCalls, a.RejectedCalls - b.RejectedCalls}
}

// Outcome is what one request got back.
type Outcome struct {
	Item      Item
	Status    int
	Answer    string
	Err       string
	Latency   time.Duration
	CacheHit  bool
	HitType   string
	Verdict   string
	Served    string // price stated in the answer ("" = none)
	Truth     string // price a fresh call states
	Freshness map[string]string
}

// Runner holds everything one arm run needs.
type Runner struct {
	Client       *http.Client
	MockURL      string
	MockKey      string
	CompanionURL string
	Model        string
	Page         *Page
	Settle       time.Duration
	Poll         time.Duration // how often Drain looks at the counters (default 250ms)
	WriteGap     time.Duration // pause after a response not served from the cache, so the cache write has landed
	Cfg          WorkloadConfig
	Log          func(format string, args ...any)

	// Real-model options. The zero values give the mock run's behaviour.
	Truth      string      // TruthMock ("" too) or TruthContext
	MetricsURL string      // when set, chat calls are counted from Bifrost's Prometheus /metrics instead of the mock
	BodyOpt    BodyOptions // max_tokens and the tool-call message

	originMu sync.Mutex
	origins  map[string]string // -truth context: normalized answer text -> question it was first seen for (this arm)
}

func (r *Runner) body(it Item) []byte { return it.BodyWith(r.Model, r.BodyOpt) }

func (r *Runner) contextTruth() bool { return r.Truth == TruthContext }

// origin returns the question an answer text was first seen for in this arm.
func (r *Runner) origin(answer string) (string, bool) {
	r.originMu.Lock()
	defer r.originMu.Unlock()
	q, ok := r.origins[normalize(answer)]
	return q, ok
}

// remember records the question a fresh (not cached) answer was written for.
func (r *Runner) remember(answer, question string) {
	r.originMu.Lock()
	defer r.originMu.Unlock()
	if r.origins == nil {
		r.origins = map[string]string{}
	}
	if k := normalize(answer); k != "" {
		if _, ok := r.origins[k]; !ok {
			r.origins[k] = question
		}
	}
}

func (r *Runner) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log(format, args...)
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (r *Runner) getJSON(ctx context.Context, url string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := r.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != 200 {
		return fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	if into == nil {
		return nil
	}
	return json.Unmarshal(body, into)
}

// MockStats reads the mock model's counters. With MetricsURL set (a real
// model), ChatCalls is replaced by Bifrost's count of chat requests it sent to
// a provider; embedding calls still come from the mock, which serves them.
func (r *Runner) MockStats(ctx context.Context) (MockStats, error) {
	var s MockStats
	if err := r.getJSON(ctx, r.MockURL+"/mock/stats", &s); err != nil {
		return s, err
	}
	if r.MetricsURL == "" {
		return s, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.MetricsURL, nil)
	if err != nil {
		return s, err
	}
	resp, err := r.Client.Do(req)
	if err != nil {
		return s, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode != 200 {
		return s, fmt.Errorf("GET %s: HTTP %d", r.MetricsURL, resp.StatusCode)
	}
	s.ChatCalls = ProviderChatCalls(string(body))
	return s, nil
}

// ProviderChatCalls sums Bifrost's bifrost_upstream_requests_total over the
// chat_completion series that name a provider key. Cache hits are recorded
// with an empty selected_key_name (no key was used), so they are left out:
// what remains is the chat requests Bifrost actually sent to a provider.
func ProviderChatCalls(metrics string) int {
	total := 0.0
	for _, line := range strings.Split(metrics, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "bifrost_upstream_requests_total{") {
			continue
		}
		end := strings.LastIndex(line, "}")
		if end < 0 {
			continue
		}
		labels, value := line[:end], strings.TrimSpace(line[end+1:])
		if !strings.Contains(labels, `method="chat_completion"`) || !strings.Contains(labels, `selected_key_name="`) ||
			strings.Contains(labels, `selected_key_name=""`) {
			continue
		}
		if f, err := strconv.ParseFloat(strings.Fields(value + " 0")[0], 64); err == nil {
			total += f
		}
	}
	return int(total + 0.5)
}

// CompanionStats reads the companion's gate/audit/guard counters. Without a
// companion URL it returns zeros.
func (r *Runner) CompanionStats(ctx context.Context) (CompanionStats, error) {
	var out CompanionStats
	if r.CompanionURL == "" {
		return out, nil
	}
	var a struct {
		Stats CompanionStats `json:"stats"`
	}
	if err := r.getJSON(ctx, r.CompanionURL+"/freshness/audits?limit=1", &a); err != nil {
		return out, err
	}
	out = a.Stats
	var g struct {
		Bypassed int `json:"bypassed"`
	}
	if err := r.getJSON(ctx, r.CompanionURL+"/freshness/guard", &g); err != nil {
		return out, err
	}
	out.GuardBypassed = g.Bypassed
	return out, nil
}

// post sends one request body and returns status, headers and body.
func (r *Runner) post(ctx context.Context, url string, headers map[string]string, body []byte) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := r.Client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, resp.Header, b, err
}

// GroundTruth asks the mock model directly (no cache) what a fresh call
// returns for every price/FAQ/benign request. Identical bodies are asked once.
// The calls are made outside any arm's measuring window.
func (r *Runner) GroundTruth(ctx context.Context, items []Item) (map[string]string, error) {
	byBody := map[string]string{}
	truth := map[string]string{}
	if r.contextTruth() {
		// No model calls: a price question's right answer is the price its own
		// context states; other answers are judged by origin (ClassifyContext).
		for _, it := range items {
			if it.Kind == KindPrice {
				truth[it.ID] = fmt.Sprintf("$%d", it.Price)
			}
		}
		return truth, nil
	}
	h := map[string]string{}
	if r.MockKey != "" {
		h["Authorization"] = "Bearer " + r.MockKey
	}
	for _, it := range items {
		if it.Kind == KindTool {
			continue
		}
		body := r.body(it)
		if ans, ok := byBody[string(body)]; ok {
			truth[it.ID] = ans
			continue
		}
		status, _, resp, err := r.post(ctx, r.MockURL, h, body)
		if err != nil {
			return nil, fmt.Errorf("ground truth for %s: %w", it.ID, err)
		}
		if status != 200 {
			return nil, fmt.Errorf("ground truth for %s: HTTP %d: %s", it.ID, status, truncate(string(resp), 120))
		}
		ans := answerText(resp)
		if ans == "" {
			return nil, fmt.Errorf("ground truth for %s: no answer text", it.ID)
		}
		byBody[string(body)] = ans
		truth[it.ID] = ans
	}
	return truth, nil
}

// send makes one request through an arm and classifies the answer.
func (r *Runner) send(ctx context.Context, arm Arm, key string, it Item, truth map[string]string) Outcome {
	headers := map[string]string{"x-bf-cache-key": key}
	for k, v := range arm.Headers {
		headers[k] = v
	}
	t0 := clock.Now()
	status, hdr, body, err := r.post(ctx, arm.URL, headers, r.body(it))
	lat := clock.Since(t0)
	out := Outcome{Item: it, Status: status, Latency: lat, Freshness: map[string]string{}}
	if err != nil {
		out.Err = truncate(err.Error(), 160)
		out.Verdict = VError
		return out
	}
	out.Answer = answerText(body)
	out.CacheHit = strings.EqualFold(hdr.Get("X-Bifrost-Cache-Hit"), "true")
	out.HitType = hdr.Get("X-Bifrost-Cache-Hit-Type")
	for _, name := range []string{"X-Freshness-Status", "X-Freshness-Action", "X-Freshness-Gate", "X-Freshness-Guard"} {
		if v := hdr.Get(name); v != "" {
			out.Freshness[strings.ToLower(name)] = v
		}
	}
	if status != 200 {
		out.Err = fmt.Sprintf("HTTP %d: %s", status, truncate(string(body), 120))
	}
	out.Truth, _ = ExtractPrice(truth[it.ID])
	out.Served, _ = ExtractPrice(out.Answer)
	if r.contextTruth() {
		origin, known := r.origin(out.Answer)
		out.Verdict = ClassifyContext(it, status, out.Answer, out.CacheHit, out.HitType, origin, known)
		if status == 200 && !out.CacheHit {
			r.remember(out.Answer, it.Question)
		}
		return out
	}
	out.Verdict = Classify(it, status, out.Answer, truth[it.ID], out.CacheHit)
	return out
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// runSequential sends items one after another (one client, no pacing).
func (r *Runner) runSequential(ctx context.Context, arm Arm, key string, items []Item, truth map[string]string) ([]Outcome, error) {
	out := make([]Outcome, 0, len(items))
	for _, it := range items {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		o := r.send(ctx, arm, key, it, truth)
		out = append(out, o)
		if !o.CacheHit {
			// Bifrost writes a new cache entry after it has answered. Without a
			// pause the next request can overtake that write and miss an entry
			// that would otherwise be there; the pause is the same for every
			// arm and is not part of any latency.
			if err := sleep(ctx, r.WriteGap); err != nil {
				return out, err
			}
		}
	}
	return out, nil
}

// runBurst releases all items at the same moment.
func (r *Runner) runBurst(ctx context.Context, arm Arm, key string, items []Item, truth map[string]string) ([]Outcome, time.Duration) {
	out := make([]Outcome, len(items))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range items {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			out[i] = r.send(ctx, arm, key, items[i], truth)
		}(i)
	}
	t0 := clock.Now()
	close(start)
	wg.Wait()
	return out, clock.Since(t0)
}

// Drain waits until the companion has no audit in flight and the mock's call
// counter has stopped moving, so that background audits are counted in the
// arm that caused them.
func (r *Runner) Drain(ctx context.Context) error {
	deadline := time.Now().Add(30 * time.Second)
	last := -1
	stable := 0
	for time.Now().Before(deadline) {
		m, err := r.MockStats(ctx)
		if err != nil {
			return err
		}
		c, err := r.CompanionStats(ctx)
		if err != nil {
			return err
		}
		if c.Scheduled == c.Completed && m.ChatCalls == last {
			stable++
			if stable >= 3 {
				return nil
			}
		} else {
			stable = 0
		}
		last = m.ChatCalls
		if err := sleep(ctx, r.poll()); err != nil {
			return err
		}
	}
	return fmt.Errorf("background work did not settle within 30s")
}

// ScenarioTally counts the outcomes of one scenario.
type ScenarioTally struct {
	Total     int            `json:"total"`
	Bad       int            `json:"bad"`
	Hits      int            `json:"cache_hits"`
	ByVerdict map[string]int `json:"by_verdict"`
}

// OutcomeRecord is the per-request line kept in the JSON.
type OutcomeRecord struct {
	ID        string            `json:"id"`
	Phase     string            `json:"phase"`
	Scenario  string            `json:"scenario"`
	Question  string            `json:"question"`
	Tool      string            `json:"tool,omitempty"`
	Verdict   string            `json:"verdict"`
	Status    int               `json:"status"`
	CacheHit  bool              `json:"cache_hit"`
	HitType   string            `json:"hit_type,omitempty"`
	LatencyMs float64           `json:"latency_ms"`
	Served    string            `json:"price_served,omitempty"`
	Truth     string            `json:"price_fresh,omitempty"`
	Freshness map[string]string `json:"freshness,omitempty"`
	Error     string            `json:"error,omitempty"`
}

// ArmRun is one arm's result in one run.
type ArmRun struct {
	Arm       string                    `json:"arm"`
	CacheKey  string                    `json:"cache_key"`
	Metrics   Metrics                   `json:"metrics"`
	Scenarios map[string]*ScenarioTally `json:"scenarios"`
	Mock      MockStats                 `json:"mock_calls_during_arm"`
	Companion CompanionStats            `json:"companion_counters_during_arm"`
	Headers   map[string]map[string]int `json:"freshness_headers_seen,omitempty"`
	Notes     []string                  `json:"notes,omitempty"`
	Requests  []OutcomeRecord           `json:"requests"`
}

// RunArm plays the whole story against one arm: restore the page, wait, warm,
// change the price, wait, after-change traffic, change again, wait, burst,
// drain background work, restore the page. The page is restored even if a step
// fails.
func (r *Runner) RunArm(ctx context.Context, arm Arm, key string, items []Item, truth map[string]string, cost CostModel) (ar *ArmRun, err error) {
	defer func() {
		if rerr := r.Page.Restore(); rerr != nil && err == nil {
			err = rerr
		}
	}()
	var warm, after, burst []Item
	for _, it := range items {
		switch it.Phase {
		case PhaseWarm:
			warm = append(warm, it)
		case PhaseAfter:
			after = append(after, it)
		case PhaseBurst:
			burst = append(burst, it)
		}
	}

	r.originMu.Lock()
	r.origins = nil // every arm has its own cache key, so origins are per arm
	r.originMu.Unlock()
	if err := r.Page.Set(r.Cfg.BasePrice); err != nil {
		return nil, err
	}
	r.logf("[%s] page at $%d, waiting %s for the companion's evidence cache to expire", arm.Name, r.Cfg.BasePrice, r.Settle)
	if err := sleep(ctx, r.Settle); err != nil {
		return nil, err
	}
	if err := r.Drain(ctx); err != nil {
		return nil, err
	}
	mock0, err := r.MockStats(ctx)
	if err != nil {
		return nil, err
	}
	comp0, err := r.CompanionStats(ctx)
	if err != nil {
		return nil, err
	}

	r.logf("[%s] warm: %d requests", arm.Name, len(warm))
	o1, err := r.runSequential(ctx, arm, key, warm, truth)
	if err != nil {
		return nil, err
	}
	if err := r.Drain(ctx); err != nil {
		return nil, err
	}

	if err := r.Page.Set(r.Cfg.ChangedPrice); err != nil {
		return nil, err
	}
	r.logf("[%s] page changed to $%d, waiting %s", arm.Name, r.Cfg.ChangedPrice, r.Settle)
	if err := sleep(ctx, r.Settle); err != nil {
		return nil, err
	}
	r.logf("[%s] after change: %d requests", arm.Name, len(after))
	o2, err := r.runSequential(ctx, arm, key, after, truth)
	if err != nil {
		return nil, err
	}
	if err := r.Drain(ctx); err != nil {
		return nil, err
	}

	if err := r.Page.Set(r.Cfg.BurstPrice); err != nil {
		return nil, err
	}
	r.logf("[%s] page changed to $%d, waiting %s", arm.Name, r.Cfg.BurstPrice, r.Settle)
	if err := sleep(ctx, r.Settle); err != nil {
		return nil, err
	}
	if err := r.Drain(ctx); err != nil {
		return nil, err
	}
	mockB0, err := r.MockStats(ctx)
	if err != nil {
		return nil, err
	}
	r.logf("[%s] burst: %d concurrent requests", arm.Name, len(burst))
	o3, wall := r.runBurst(ctx, arm, key, burst, truth)
	if err := r.Drain(ctx); err != nil {
		return nil, err
	}
	mockB1, err := r.MockStats(ctx)
	if err != nil {
		return nil, err
	}
	mock1 := mockB1 // the burst is the last thing sent, and background work has drained
	comp1, err := r.CompanionStats(ctx)
	if err != nil {
		return nil, err
	}

	ar = BuildArmRun(arm.Name, key, o1, o2, o3, wall, mock1.minus(mock0), comp1.minus(comp0), mockB1.minus(mockB0), cost)
	return ar, nil
}

// CostModel carries the ASSUMED prices used for the cost illustration.
type CostModel struct {
	PerChatCall      float64
	PerEmbeddingCall float64
}

// BuildArmRun turns raw outcomes and counter deltas into the arm's metrics.
func BuildArmRun(name, key string, warm, after, burst []Outcome, burstWall time.Duration, mock MockStats, comp CompanionStats, burstMock MockStats, cost CostModel) *ArmRun {
	all := append(append(append([]Outcome(nil), warm...), after...), burst...)
	seq := append(append([]Outcome(nil), warm...), after...)
	m := Metrics{}
	ar := &ArmRun{Arm: name, CacheKey: key, Metrics: m, Scenarios: map[string]*ScenarioTally{}, Mock: mock, Companion: comp,
		Headers: map[string]map[string]int{}}

	byKind := map[string]int{}
	for _, o := range all {
		m["requests"]++
		v := o.Verdict
		switch v {
		case VCorrect:
			m["correct"]++
		case VStale:
			m["stale"]++
		case VWrongMatch:
			m["wrong_match"]++
		case VError:
			m["errors"]++
		case VToolReuse:
			m["tool_reused"]++
		case VUnjudged:
			m["unjudged"]++
		}
		if o.Item.Kind == KindTool {
			m["tool_requests"]++
		}
		if o.Status == 200 && o.CacheHit {
			m["cache_hits"]++
		}
		if IsBad(v) {
			m["bad"]++
			byKind[o.Item.Kind]++
		}
		sc := o.Item.Scenario
		if o.Item.Phase == PhaseWarm {
			sc = ScWarm
		}
		t := ar.Scenarios[sc]
		if t == nil {
			t = &ScenarioTally{ByVerdict: map[string]int{}}
			ar.Scenarios[sc] = t
		}
		t.Total++
		t.ByVerdict[v]++
		if IsBad(v) {
			t.Bad++
		}
		if o.Status == 200 && o.CacheHit {
			t.Hits++
		}
		for k, val := range o.Freshness {
			if ar.Headers[k] == nil {
				ar.Headers[k] = map[string]int{}
			}
			ar.Headers[k][val]++
		}
		ar.Requests = append(ar.Requests, OutcomeRecord{ID: o.Item.ID, Phase: o.Item.Phase, Scenario: o.Item.Scenario, Question: o.Item.Question,
			Tool: o.Item.Tool, Verdict: v, Status: o.Status, CacheHit: o.CacheHit, HitType: o.HitType,
			LatencyMs: float64(o.Latency.Microseconds()) / 1000, Served: o.Served, Truth: o.Truth, Freshness: o.Freshness, Error: o.Err})
	}
	m["cache_hit_rate_pct"] = pctOf(m["cache_hits"], m["requests"])
	m["bad_per_1000"] = 1000 * m["bad"] / nz(m["requests"])
	m["bad_price"] = float64(byKind[KindPrice])
	m["bad_faq"] = float64(byKind[KindFAQ])
	m["bad_benign"] = float64(byKind[KindBenign])

	m["model_calls"] = float64(mock.ChatCalls)
	m["embedding_calls"] = float64(mock.EmbeddingCalls)
	m["gate_calls"] = float64(comp.GateVerified + comp.GateBlocked)
	m["audit_calls"] = float64(comp.Completed)
	m["serving_calls"] = m["model_calls"] - m["gate_calls"] - m["audit_calls"]
	m["gate_blocked"] = float64(comp.GateBlocked)
	m["gate_trusted"] = float64(comp.GateTrusted)
	m["audit_different"] = float64(comp.Different)
	m["guard_bypassed"] = float64(comp.GuardBypassed)
	m["calls_saved_vs_no_cache"] = m["requests"] - m["model_calls"]
	m["cost_per_1000_usd"] = 1000 * (m["model_calls"]*cost.PerChatCall + m["embedding_calls"]*cost.PerEmbeddingCall) / nz(m["requests"])

	lat := func(os []Outcome, keep func(Outcome) bool) []time.Duration {
		var l []time.Duration
		for _, o := range os {
			if o.Status == 200 && keep(o) {
				l = append(l, o.Latency)
			}
		}
		return l
	}
	every := func(Outcome) bool { return true }
	s := stats.Summarize(lat(seq, every), 0, 0)
	m["lat_p50_ms"], m["lat_p95_ms"] = s.P50Ms, s.P95Ms
	sh := stats.Summarize(lat(seq, func(o Outcome) bool { return o.CacheHit }), 0, 0)
	m["lat_hit_p50_ms"], m["lat_hit_p95_ms"], m["lat_hit_n"] = sh.P50Ms, sh.P95Ms, float64(sh.Count)
	sn := stats.Summarize(lat(seq, func(o Outcome) bool { return !o.CacheHit }), 0, 0)
	m["lat_nonhit_p50_ms"], m["lat_nonhit_p95_ms"], m["lat_nonhit_n"] = sn.P50Ms, sn.P95Ms, float64(sn.Count)

	for _, o := range burst {
		m["burst_requests"]++
		switch o.Verdict {
		case VError:
			m["burst_errors"]++
		case VStale:
			m["burst_stale"]++
		case VCorrect:
			m["burst_correct"]++
		case VWrongMatch:
			m["burst_wrong_match"]++
		}
		if o.Status == 200 && o.CacheHit {
			m["burst_hits"]++
		}
	}
	m["burst_model_calls"] = float64(burstMock.ChatCalls)
	sb := stats.Summarize(lat(burst, every), 0, burstWall)
	m["burst_p50_ms"], m["burst_p95_ms"], m["burst_wall_ms"] = sb.P50Ms, sb.P95Ms, burstWall.Seconds()*1000
	if m["serving_calls"] < 0 {
		ar.Notes = append(ar.Notes, fmt.Sprintf("serving_calls is %.0f (negative): the counter deltas do not add up, so something else used the model during this arm", m["serving_calls"]))
	}
	if m["unjudged"] > 0 {
		ar.Notes = append(ar.Notes, fmt.Sprintf("%.0f semantic cache hit(s) served an answer the client never saw being written, so they were not judged (counted neither right nor bad)", m["unjudged"]))
	}
	if mock.UnknownCalls != 0 || mock.RejectedCalls != 0 {
		ar.Notes = append(ar.Notes, fmt.Sprintf("the mock counted %d unknown and %d rejected calls during this arm", mock.UnknownCalls, mock.RejectedCalls))
	}
	return ar
}

func nz(v float64) float64 {
	if v == 0 {
		return 1
	}
	return v
}

func pctOf(a, b float64) float64 { return 100 * a / nz(b) }

// post2 posts a JSON body to a full URL (used for the mock's helper endpoints).
func (r *Runner) post2(ctx context.Context, url string, body []byte) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.Client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, resp.Header, b, err
}

func (r *Runner) poll() time.Duration {
	if r.Poll > 0 {
		return r.Poll
	}
	return 250 * time.Millisecond
}
