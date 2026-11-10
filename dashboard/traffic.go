package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// probeCacheKey scopes every request this dashboard sends through the
// companion/Bifrost under its own x-bf-cache-key, so the traffic generated
// for this demo never shares entries with (or clears entries for) whatever
// other cache keys other agents or demos are using on the same shared
// Bifrost instance.
const probeCacheKey = "dashboard-demo"

// probeQuestion is one line of the fixed traffic script. "kind" is only a
// label for the event log; Bifrost's cache decides the real outcome, and the
// companion's audit decides whether a reused answer was right.
//
// kindToolResult marks the one request that carries a tool's result (a "tool"
// role message), which the companion's guard keeps out of the cache on purpose.
type probeQuestion struct {
	text string
	kind string // "fresh" | "repeat" | "paraphrase" | "lookalike" | kindToolResult
}

const kindToolResult = "tool-result"

// probeScript is a small, fixed set of chat questions designed to exercise
// every outcome a cache can produce: a miss, an exact repeat, a reworded
// question that means the same (a right semantic hit) and a look-alike that
// means something different (a wrong semantic hit). The expected answers come
// from the mock model's knowledge file (docker/mock-knowledge.json), which
// gives same-meaning questions identical answers and look-alikes different
// ones; the mock embedding's similarity scores for these pairs were measured,
// not assumed (0.84 for the two look-alikes, 0.84-0.93 for the rewordings,
// 1.00 for on/off, which the mock's embedding cannot tell apart).
//
// None of the words match a category keyword in the demo policy (price/
// policy/schedule/availability), so this traffic is never intercepted by the
// freshness checks: it exercises Bifrost's own direct/semantic cache and the
// companion's audit of it.
var probeScript = []probeQuestion{
	{"How do I reset my Acme account password from the mobile app?", "fresh"},
	{"How do I reset my Acme account password from the mobile app?", "repeat"},
	{"How can I reset my Acme account password using the mobile app?", "paraphrase"},
	{"How do I reset my Acme account username from the mobile app?", "lookalike"},

	{"Can I export my Acme project data to a CSV file?", "fresh"},
	{"Is it possible to export my Acme project data to a CSV file?", "paraphrase"},
	{"Can I import my Acme project data from a CSV file?", "lookalike"},

	{"What is the boiling point of water in Celsius?", "fresh"},
	{"What is the boiling point of water in degrees Celsius?", "paraphrase"},

	{"Name the tallest mountain on Earth.", "fresh"},
	{"Which mountain is the tallest on Earth?", "paraphrase"},

	{"Who wrote the novel Moby Dick?", "fresh"},
	{"Who is the author who wrote the novel Moby Dick?", "paraphrase"},

	{"What is the chemical symbol for gold?", "fresh"},
	{"What is the chemical symbol for gold?", "repeat"},

	{"How do I turn on two factor login for my Acme account?", "fresh"},
	{"How do I turn off two factor login for my Acme account?", "lookalike"},

	// A request that carries a tool's result: its answer depends on live tool
	// data, so the guard must keep it out of the cache (it shows as a miss).
	{"What does the order lookup tool say about order 1234?", "tool-result"},
}

// TrafficEvent is one request this dashboard sent through the live stack,
// and what the real response said about it. Kept for the on-page log so an
// operator can see exactly which real request produced which outcome.
type TrafficEvent struct {
	Time       time.Time `json:"time"`
	Question   string    `json:"question"`
	Kind       string    `json:"kind"`
	HitType    string    `json:"hit_type"` // "direct" | "semantic" | "miss" | "error"
	Similarity string    `json:"similarity,omitempty"`
	Threshold  string    `json:"threshold,omitempty"`
	// AuditID is set when the companion is checking this reused answer by
	// asking the model again; GET /api/audits has the verdict.
	AuditID string `json:"audit_id,omitempty"`
	// Gate is what the check before serving did (verified, trusted, blocked,
	// unverified), SharedRefresh is true when the request waited for a refresh
	// another request was running, and Guard says why the request was kept
	// out of the cache on purpose. All come from response headers.
	Gate          string `json:"gate,omitempty"`
	SharedRefresh bool   `json:"shared_refresh,omitempty"`
	Guard         string `json:"guard,omitempty"`
	// Receipt is the companion's one-line summary of the freshness check
	// behind the answer (X-Freshness-Receipt), when a check ran.
	Receipt     *Receipt `json:"receipt,omitempty"`
	RequestID   string   `json:"request_id,omitempty"` // X-Freshness-Request-Id, for the "Why?" link
	TotalTokens int      `json:"total_tokens,omitempty"`
	LatencyMS   float64  `json:"latency_ms"`
	Note        string   `json:"note,omitempty"`
}

// CacheTally is the dashboard's own running count of real cache outcomes,
// built entirely from response headers and bodies this process received
// from the live companion/Bifrost stack. It is in-memory only (resets with
// the process) because Bifrost itself exposes no aggregate stats endpoint —
// see the README note on /api/stats for the full explanation surfaced in
// the UI.
type CacheTally struct {
	mu sync.Mutex

	TotalRequests int `json:"total_requests"`
	DirectHits    int `json:"direct_hits"`
	SemanticHits  int `json:"semantic_hits"`
	Misses        int `json:"misses"`
	Errors        int `json:"errors"`

	// CachedTotalTokens sums usage.total_tokens, read from the actual
	// response body, for every request that was a cache hit (direct or
	// semantic). That usage field is the real token count Bifrost's cached
	// JSON carries from the original model call: it is what would have
	// been reprocessed by the model had the cache not served the answer.
	// It is NOT the x-bifrost-cache-input-tokens header, which measures the
	// embedding lookup cost instead — see README.md "Where the numbers come
	// from" for why that header is deliberately not used here.
	CachedTotalTokens      int `json:"cached_total_tokens"`
	CachedPromptTokens     int `json:"cached_prompt_tokens"`
	CachedCompletionTokens int `json:"cached_completion_tokens"`

	Recent []TrafficEvent `json:"recent"`
}

const maxRecentEvents = 200

func (t *CacheTally) record(ev TrafficEvent, usage chatUsage, isHit bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.TotalRequests++
	switch ev.HitType {
	case "direct":
		t.DirectHits++
	case "semantic":
		t.SemanticHits++
	case "miss":
		t.Misses++
	default:
		t.Errors++
	}
	if isHit {
		t.CachedTotalTokens += usage.TotalTokens
		t.CachedPromptTokens += usage.PromptTokens
		t.CachedCompletionTokens += usage.CompletionTokens
	}
	t.Recent = append(t.Recent, ev)
	if len(t.Recent) > maxRecentEvents {
		t.Recent = t.Recent[len(t.Recent)-maxRecentEvents:]
	}
}

// CacheTallySnapshot is the JSON-safe, lock-free view of a CacheTally at one
// point in time (CacheTally itself holds a sync.Mutex and must never be
// copied or marshaled directly).
type CacheTallySnapshot struct {
	TotalRequests          int            `json:"total_requests"`
	DirectHits             int            `json:"direct_hits"`
	SemanticHits           int            `json:"semantic_hits"`
	Misses                 int            `json:"misses"`
	Errors                 int            `json:"errors"`
	CachedTotalTokens      int            `json:"cached_total_tokens"`
	CachedPromptTokens     int            `json:"cached_prompt_tokens"`
	CachedCompletionTokens int            `json:"cached_completion_tokens"`
	Recent                 []TrafficEvent `json:"recent"`
}

// snapshot returns a deep-enough copy for safe JSON encoding outside the lock.
func (t *CacheTally) snapshot() CacheTallySnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := CacheTallySnapshot{
		TotalRequests:          t.TotalRequests,
		DirectHits:             t.DirectHits,
		SemanticHits:           t.SemanticHits,
		Misses:                 t.Misses,
		Errors:                 t.Errors,
		CachedTotalTokens:      t.CachedTotalTokens,
		CachedPromptTokens:     t.CachedPromptTokens,
		CachedCompletionTokens: t.CachedCompletionTokens,
		Recent:                 make([]TrafficEvent, len(t.Recent)),
	}
	for i := len(t.Recent) - 1; i >= 0; i-- {
		// newest first for display
		out.Recent[len(t.Recent)-1-i] = t.Recent[i]
	}
	return out
}

func (t *CacheTally) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.TotalRequests, t.DirectHits, t.SemanticHits, t.Misses, t.Errors = 0, 0, 0, 0, 0
	t.CachedTotalTokens, t.CachedPromptTokens, t.CachedCompletionTokens = 0, 0, 0
	t.Recent = nil
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type chatCompletionBody struct {
	Usage chatUsage `json:"usage"`
}

// runProbeScript sends every question in probeScript, in order, through the
// companion's /v1/chat/completions endpoint (which forwards to Bifrost,
// which forwards to the mock model on a real miss) and tallies the real
// X-Bifrost-Cache-* response headers plus the real usage field in each
// response body. It returns the events it generated so the caller can log
// them; it also records them into tally directly.
func runProbeScript(ctx context.Context, client *http.Client, companionBase string, tally *CacheTally) ([]TrafficEvent, error) {
	events := make([]TrafficEvent, 0, len(probeScript))
	for _, q := range probeScript {
		ev, usage, isHit, err := sendProbe(ctx, client, companionBase, q)
		if err != nil {
			ev.HitType = "error"
			ev.Note = err.Error()
		}
		tally.record(ev, usage, isHit)
		events = append(events, ev)
	}
	return events, nil
}

// toolResultMessages builds a short conversation in which an assistant called
// a tool and the tool answered: the shape the companion's guard recognises as
// "this answer depends on live tool data".
func toolResultMessages(question string) []any {
	return []any{
		map[string]any{"role": "user", "content": question},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{
			"id": "call_probe_1", "type": "function",
			"function": map[string]any{"name": "lookup_order", "arguments": "{\"order\":1234}"},
		}}},
		map[string]any{"role": "tool", "tool_call_id": "call_probe_1", "content": "Order 1234: shipped"},
	}
}

func sendProbe(ctx context.Context, client *http.Client, companionBase string, q probeQuestion) (TrafficEvent, chatUsage, bool, error) {
	ev := TrafficEvent{Time: time.Now().UTC(), Question: q.text, Kind: q.kind}

	messages := []any{map[string]any{"role": "user", "content": q.text}}
	if q.kind == kindToolResult {
		messages = toolResultMessages(q.text)
	}
	payload := map[string]any{
		"model":    "openai/mock-chat",
		"messages": messages,
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, companionBase+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return ev, chatUsage{}, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-bf-cache-key", probeCacheKey)

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return ev, chatUsage{}, false, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	ev.LatencyMS = float64(time.Since(start).Microseconds()) / 1000

	var respBody chatCompletionBody
	_ = json.NewDecoder(resp.Body).Decode(&respBody)

	if resp.StatusCode != http.StatusOK {
		return ev, chatUsage{}, false, fmt.Errorf("companion answered HTTP %d", resp.StatusCode)
	}

	cacheHit := resp.Header.Get("X-Bifrost-Cache-Hit")
	hitType := resp.Header.Get("X-Bifrost-Cache-Hit-Type")
	similarity := resp.Header.Get("X-Bifrost-Cache-Similarity")

	isHit := cacheHit == "true"
	switch {
	case isHit && hitType == "semantic":
		ev.HitType = "semantic"
	case isHit && hitType == "direct":
		ev.HitType = "direct"
	case isHit:
		ev.HitType = "direct" // hit but hit-type header absent/unexpected: treat as direct
	default:
		ev.HitType = "miss"
	}
	ev.Similarity = similarity
	ev.Threshold = resp.Header.Get("X-Bifrost-Cache-Threshold")
	ev.AuditID = resp.Header.Get("X-Freshness-Audit")
	ev.Gate = resp.Header.Get("X-Freshness-Gate")
	ev.SharedRefresh = resp.Header.Get("X-Freshness-Shared-Refresh") == "true"
	ev.Guard = resp.Header.Get("X-Freshness-Guard")
	ev.Receipt = parseReceiptHeader(resp.Header.Get("X-Freshness-Receipt"))
	ev.RequestID = resp.Header.Get("X-Freshness-Request-Id")
	ev.TotalTokens = respBody.Usage.TotalTokens

	return ev, respBody.Usage, isHit, nil
}

// companionHealth mirrors the subset of freshness-companion's
// GET /healthz response this dashboard displays as its "data source" note.
type companionHealth struct {
	Status              string `json:"status"`
	BifrostURL          string `json:"bifrost_url"`
	EvidenceProvider    string `json:"evidence_provider"`
	RefreshStrategy     string `json:"refresh_strategy"`
	PolicyCategories    int    `json:"policy_categories"`
	PolicyFacts         int    `json:"policy_facts"`
	TrackedCacheEntries int    `json:"tracked_cache_entries"`
	StartedAt           string `json:"started_at"`
	UptimeSeconds       int64  `json:"uptime_seconds"`
}

func fetchCompanionHealth(ctx context.Context, client *http.Client, companionBase string) (companionHealth, error) {
	var h companionHealth
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, companionBase+"/healthz", nil)
	if err != nil {
		return h, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return h, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return h, fmt.Errorf("companion /healthz: HTTP %d", resp.StatusCode)
	}
	err = json.NewDecoder(resp.Body).Decode(&h)
	return h, err
}

func parseLimit(q string, def, max int) int {
	if q == "" {
		return def
	}
	n, err := strconv.Atoi(q)
	if err != nil || n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}
