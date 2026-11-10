package proxy

import (
	crand "crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HeaderRequestID is set on every response to POST /v1/chat/completions. Its
// value is the id to give to GET /freshness/explain/{id}.
const HeaderRequestID = "X-Freshness-Request-Id"

// DefaultExplainRing is how many explanations are kept in memory.
const DefaultExplainRing = 2000

// How a request was answered.
const (
	HowMiss          = "miss"              // not in the cache: the model answered
	HowDirectHit     = "direct_hit"        // the exact earlier question was in the cache
	HowSemanticHit   = "semantic_hit"      // a similar earlier question was in the cache
	HowGuarded       = "bypassed_by_guard" // kept out of the cache on purpose
	HowVerified      = "verified_answer"   // answered from the curated table
	HowGateBlocked   = "blocked_by_gate"   // a risky cache match was wrong; the model's own answer was used
	HowRefreshed     = "refreshed"         // the cached answer was out of date; the model answered again
	HowRefreshFailed = "refresh_failed"    // out of date and no fresh answer could be had
	HowCorrected     = "corrected"         // the new answer was wrong; it was asked again with the true value
	HowUncached      = "uncached"          // the new answer was wrong; it was returned and its entry deleted
	HowNotCached     = "not_cached"        // no cache information came back (no cache key, a stream, an error)
	HowNoResponse    = "no_response"       // nothing was written (the caller left)
)

// Explanation says what happened to one chat request. It holds ids, scores,
// times, short extracted values and verdicts. It never holds a prompt or an
// answer.
type Explanation struct {
	ID         string    `json:"id"`
	Time       time.Time `json:"time"`
	HTTPStatus int       `json:"http_status"`
	How        string    `json:"how"`
	Streamed   bool      `json:"streamed,omitempty"`

	CacheHit   bool     `json:"cache_hit"`
	HitType    string   `json:"hit_type,omitempty"`
	Similarity *float64 `json:"similarity,omitempty"`
	Threshold  *float64 `json:"threshold,omitempty"`
	CacheID    string   `json:"cache_id,omitempty"`
	AgeSeconds *float64 `json:"age_seconds,omitempty"`

	FreshnessStatus string `json:"freshness_status,omitempty"`
	FreshnessAction string `json:"freshness_action,omitempty"`
	Category        string `json:"category,omitempty"`
	Fact            string `json:"fact,omitempty"`
	// AnswerValue and CurrentValue are the short values the policy's patterns
	// pulled out of the saved answer and out of the source page (for example
	// "$49"). They are not the answer text.
	AnswerValue       string     `json:"answer_value,omitempty"`
	CurrentValue      string     `json:"current_value,omitempty"`
	CheckedAt         *time.Time `json:"checked_at,omitempty"`
	CheckedAgoSeconds *float64   `json:"checked_ago_seconds,omitempty"`
	RefreshedCheck    string     `json:"refreshed_check,omitempty"`
	Correction        string     `json:"correction,omitempty"`

	Gate          string `json:"gate,omitempty"`
	AuditID       string `json:"audit_id,omitempty"`
	SharedRefresh bool   `json:"shared_refresh,omitempty"`
	ChangeNotice  string `json:"change_notice,omitempty"`
	GuardReason   string `json:"guard_reason,omitempty"`

	// VerifiedLabel is the label of the verified answer that was used (or
	// that was found out of date). VerifiedStatus is "human" or
	// "stale_verified".
	VerifiedLabel  string `json:"verified_label,omitempty"`
	VerifiedStatus string `json:"verified_status,omitempty"`

	// Receipt is the structured form of the X-Freshness-Receipt header: the
	// one-line summary of the freshness check behind the answer. Absent when
	// no check ran.
	Receipt *Receipt `json:"receipt,omitempty"`

	Plain string `json:"plain"`
}

// explainRing keeps the newest explanations; the oldest are dropped.
type explainRing struct {
	mu   sync.Mutex
	cap  int
	ids  []string
	next int
	m    map[string]*Explanation
}

func newExplainRing(n int) *explainRing {
	return &explainRing{cap: n, ids: make([]string, 0, min(n, 256)), m: map[string]*Explanation{}}
}

func (r *explainRing) add(e *Explanation) {
	if r.cap <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.ids) < r.cap {
		r.ids = append(r.ids, e.ID)
	} else {
		delete(r.m, r.ids[r.next])
		r.ids[r.next] = e.ID
		r.next = (r.next + 1) % r.cap
	}
	r.m[e.ID] = e
}

func (r *explainRing) get(id string) (*Explanation, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.m[id]
	return e, ok
}

// recent returns up to n explanations, newest first.
func (r *explainRing) recent(n int) []*Explanation {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Explanation, 0, min(n, len(r.ids)))
	total := len(r.ids)
	for i := 0; i < total && len(out) < n; i++ {
		var idx int
		if total < r.cap {
			idx = total - 1 - i
		} else {
			idx = ((r.next-1-i)%r.cap + r.cap) % r.cap
		}
		out = append(out, r.m[r.ids[idx]])
	}
	return out
}

func (r *explainRing) size() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.ids)
}

// newRequestID returns an id that is unique for this process and very
// unlikely to repeat after a restart.
func (s *Server) newRequestID() string {
	return "req-" + s.idPrefix + "-" + strconv.FormatInt(s.reqSeq.Add(1), 10)
}

func randomPrefix() string {
	var b [4]byte
	_, _ = crand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// captureWriter remembers the status and the headers a handler sent, which is
// all an explanation is built from.
type captureWriter struct {
	http.ResponseWriter
	status int
	header http.Header
	// onHeader runs once, before the headers go out, so the explanation is
	// stored before the caller can ask for it.
	onHeader func(*captureWriter)
	// beforeHeader runs once, just before the headers are captured and sent,
	// with the live header map: a last chance to add a header that sums up
	// the others (the receipt). Whatever it sets is captured too.
	beforeHeader func(http.Header)
}

func (c *captureWriter) WriteHeader(code int) {
	if c.header == nil {
		if c.beforeHeader != nil {
			c.beforeHeader(c.ResponseWriter.Header())
		}
		c.status = code
		c.header = c.ResponseWriter.Header().Clone()
		if c.onHeader != nil {
			c.onHeader(c)
		}
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *captureWriter) Write(b []byte) (int, error) {
	if c.header == nil {
		c.WriteHeader(http.StatusOK)
	}
	return c.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the real writer (flushing).
func (c *captureWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// explain builds the explanation of one finished response.
func (s *Server) explain(id string, cw *captureWriter) *Explanation {
	h := cw.header
	e := &Explanation{ID: id, Time: s.cfg.Now().UTC(), HTTPStatus: cw.status}
	if h == nil {
		e.How = HowNoResponse
		e.Plain = "The companion wrote no response for this request (the caller probably went away)."
		return e
	}
	e.Streamed = strings.HasPrefix(strings.ToLower(h.Get("Content-Type")), "text/event-stream")
	e.CacheHit = isCacheHit(h)
	e.CacheID = strings.TrimSpace(h.Get(HeaderCacheID))
	if e.CacheHit {
		e.HitType = strings.ToLower(strings.TrimSpace(h.Get(HeaderCacheHitType)))
		e.Similarity = parseScore(h, HeaderCacheSimilarity)
		e.Threshold = parseScore(h, HeaderCacheThreshold)
	}
	e.FreshnessStatus = h.Get(HeaderStatus)
	e.FreshnessAction = h.Get(HeaderAction)
	e.Category = h.Get(HeaderCategory)
	e.Fact = h.Get(HeaderFact)
	e.AnswerValue = h.Get(HeaderCachedValue)
	if e.AnswerValue == "" {
		e.AnswerValue = h.Get(HeaderAnswerValue)
	}
	e.CurrentValue = h.Get(HeaderCurrentValue)
	e.RefreshedCheck = h.Get(HeaderRefreshedCheck)
	e.Correction = h.Get(HeaderCorrection)
	e.Gate = h.Get(HeaderGate)
	e.AuditID = h.Get(HeaderAudit)
	e.SharedRefresh = h.Get(HeaderSharedRefresh) == "true"
	e.ChangeNotice = h.Get(HeaderChangeNotice)
	if g, ok := strings.CutPrefix(h.Get(HeaderGuard), "bypassed: "); ok {
		e.GuardReason = g
	}
	e.VerifiedLabel = h.Get(HeaderVerifiedLabel)
	e.VerifiedStatus = h.Get(HeaderVerified)
	if r, ok := receiptFromHeaders(h, e.Time); ok {
		e.Receipt = &r
	}
	if t, err := time.Parse(time.RFC3339, h.Get(HeaderCheckedAt)); err == nil {
		e.CheckedAt = &t
		ago := e.Time.Sub(t).Seconds()
		if ago < 0 {
			ago = 0
		}
		e.CheckedAgoSeconds = &ago
	}
	if v, err := strconv.ParseFloat(h.Get(HeaderAgeSeconds), 64); err == nil {
		e.AgeSeconds = &v
	} else if e.CacheHit && e.CacheID != "" {
		if age, ok := s.cfg.Ages.Age(e.CacheID); ok {
			v := age.Seconds()
			e.AgeSeconds = &v
		}
	}
	e.How = classify(e, h)
	e.Plain = plainText(e)
	return e
}

func classify(e *Explanation, h http.Header) string {
	switch {
	case h.Get(HeaderAnswerSource) == "verified":
		return HowVerified
	case e.GuardReason != "":
		return HowGuarded
	case e.Gate == GateBlocked:
		return HowGateBlocked
	case e.FreshnessAction == ActionCorrected:
		return HowCorrected
	case e.FreshnessAction == ActionUncached:
		return HowUncached
	case e.FreshnessAction == ActionRefreshed:
		return HowRefreshed
	case e.FreshnessAction == ActionRefreshFailed:
		return HowRefreshFailed
	case e.CacheHit && e.HitType == "semantic":
		return HowSemanticHit
	case e.CacheHit:
		return HowDirectHit
	case isCacheMiss(h):
		return HowMiss
	}
	return HowNotCached
}

// summary is the one line shown in the list.
func (e *Explanation) summary() string {
	parts := []string{e.How}
	if e.CacheHit && e.Similarity != nil {
		parts = append(parts, fmt.Sprintf("closeness %.2f", *e.Similarity))
	}
	if e.FreshnessStatus != "" {
		parts = append(parts, "freshness "+e.FreshnessStatus)
	}
	if e.Gate != "" {
		parts = append(parts, "gate "+e.Gate)
	}
	if e.GuardReason != "" {
		parts = append(parts, e.GuardReason)
	}
	if e.VerifiedStatus == "stale_verified" {
		parts = append(parts, "verified answer out of date")
	}
	parts = append(parts, "HTTP "+strconv.Itoa(e.HTTPStatus))
	return strings.Join(parts, ", ")
}

func ago(seconds float64) string {
	n := int64(seconds + 0.5)
	unit := func(v int64, name string) string {
		if v == 1 {
			return "1 " + name
		}
		return fmt.Sprintf("%d %ss", v, name)
	}
	switch {
	case n < 60:
		return unit(n, "second")
	case n < 3600:
		return unit(n/60, "minute")
	case n < 86400:
		return unit(n/3600, "hour")
	}
	return unit(n/86400, "day")
}

// guardWords turns a guard reason into plain words.
func guardWords(reason string) string {
	switch {
	case reason == "tool_results":
		return "it carried the result of a tool, so the answer depends on live data"
	case reason == "side_effect_tool":
		return "it offered a tool that does something (an action), and replaying an old answer would skip the action"
	}
	if kind, ok := strings.CutPrefix(reason, "sensitive:"); ok {
		names := map[string]string{
			"email": "an email address", "card": "a payment card number", "api_key": "an API key",
			"jwt": "a login token", "bearer_token": "a bearer token", "ssn": "a social security number",
			"iban": "a bank account number (IBAN)", "phone": "a phone number",
		}
		if n, ok := names[kind]; ok {
			return "it seemed to contain " + n
		}
		return "it seemed to contain sensitive text"
	}
	return "of the guard rule " + reason
}

// plainText writes 2 to 4 sentences in simple words. It uses the same facts as
// the structured fields and nothing else.
func plainText(e *Explanation) string {
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	closeness := func() string {
		if e.Similarity == nil {
			return ""
		}
		s := fmt.Sprintf("%.2f", *e.Similarity)
		if e.Threshold != nil {
			return fmt.Sprintf(" at %s closeness (it needed at least %.2f)", s, *e.Threshold)
		}
		return " at " + s + " closeness"
	}
	checked := func() string {
		if e.CheckedAgoSeconds == nil {
			return "the source page"
		}
		return "the source page, checked " + ago(*e.CheckedAgoSeconds) + " ago,"
	}
	switch e.How {
	case HowVerified:
		add("This answer came from the list of verified answers, which a person wrote and checked%s.", labelWords(e.VerifiedLabel))
		add("The model was not asked and the cache was not used.")
		add("The question matched a listed question word for word (ignoring capital letters and punctuation).")
	case HowGuarded:
		add("This request was kept out of the cache on purpose, because %s.", guardWords(e.GuardReason))
		add("It went straight to the model, and nothing about it was saved for other people.")
	case HowGateBlocked:
		add("The cache offered an earlier answer%s, but the double-check showed it did not fit this question.", closeness())
		add("So the model was asked directly and its own answer was used instead.")
	case HowRefreshed, HowCorrected, HowUncached:
		add("The cache had an earlier answer%s, but it was not served.", closeness())
		if e.FreshnessStatus == "stale" && e.AnswerValue != "" && e.CurrentValue != "" {
			add("It said %s while %s says %s, so it was out of date.", e.AnswerValue, checked(), e.CurrentValue)
		} else if e.FreshnessStatus == "stale" {
			add("%s showed it was out of date.", capitalize(checked()))
		} else {
			add("It could not be confirmed as current (freshness: %s).", orUnknown(e.FreshnessStatus))
		}
		switch e.How {
		case HowCorrected:
			add("The new model answer was also wrong, so the model was asked again with the true value and that answer is what you got.")
		case HowUncached:
			add("The new model answer disagreed with the source page too, so it was returned with a warning and its cache entry was deleted.")
		default:
			add("The old entry was cleared and the model was asked again, so this answer is new.")
		}
	case HowRefreshFailed:
		add("The cache had an earlier answer that could not be confirmed as current, so it was not served.")
		add("The companion tried to get a fresh answer and could not, so the request failed instead of giving you a possibly wrong answer.")
	case HowSemanticHit, HowDirectHit:
		if e.How == HowDirectHit {
			add("This answer came from the cache. The same question had been asked before.")
		} else {
			add("This answer came from the cache. It matched an earlier question%s.", closeness())
		}
		if e.AgeSeconds != nil {
			add("The saved answer is %s old.", ago(*e.AgeSeconds))
		}
		switch e.Gate {
		case GateVerified:
			add("Because the match was close to the limit, the model was asked again and agreed with the saved answer.")
		case GateTrusted:
			add("This question and saved answer had already passed a double-check earlier.")
		case GateUnverified:
			add("The double-check of this close match could not be run, so it was served unchecked.")
		}
		switch e.FreshnessStatus {
		case "confirmed":
			if e.CurrentValue != "" {
				add("%s still says %s, so the saved answer was served.", capitalize(checked()), e.CurrentValue)
			} else {
				add("%s still agrees with it, so it was served.", capitalize(checked()))
			}
		case "unable_to_verify", "unknown":
			add("The companion could not confirm it is still current (%s), and served it with a warning.", e.FreshnessStatus)
		case "":
			add("The question is not about something that changes (like a price), so no freshness check was needed.")
		}
	case HowMiss:
		add("No earlier answer matched this question, so the model was asked and the answer was saved in the cache.")
		if e.FreshnessStatus != "" {
			add("The new answer was also checked against the source page (%s).", e.FreshnessStatus)
		}
	default:
		add("Bifrost sent no cache information for this request, so the companion only passed it along.")
		add("That happens when no cache key was sent, the answer was streamed, or the request failed (HTTP %d).", e.HTTPStatus)
	}
	if e.VerifiedStatus == "stale_verified" && e.How != HowVerified {
		add("A verified answer exists for this question, but the source page disagrees with it, so it was not used.")
	}
	if e.SharedRefresh {
		add("Another request needed the same refresh at the same moment, so they shared one model call.")
	}
	if len(out) > 4 {
		out = out[:4]
	}
	return strings.Join(out, " ")
}

func labelWords(label string) string {
	if label == "" {
		return ""
	}
	return " (label: " + label + ")"
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

type explainListItem struct {
	ID      string    `json:"id"`
	Time    time.Time `json:"time"`
	How     string    `json:"how"`
	Summary string    `json:"summary"`
}

const maxExplainLimit = 500

func (s *Server) handleExplainList(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if q := r.URL.Query().Get("limit"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer", "invalid_request_error")
			return
		}
		limit = min(n, maxExplainLimit)
	}
	recent := s.explains.recent(limit)
	items := make([]explainListItem, 0, len(recent))
	for _, e := range recent {
		items = append(items, explainListItem{ID: e.ID, Time: e.Time, How: e.How, Summary: e.summary()})
	}
	writeJSON(w, http.StatusOK, struct {
		Count    int               `json:"count"`
		Kept     int               `json:"kept"`
		Capacity int               `json:"capacity"`
		Requests []explainListItem `json:"requests"`
	}{Count: len(items), Kept: s.explains.size(), Capacity: s.explains.cap, Requests: items})
}

func (s *Server) handleExplain(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.explains.cap <= 0 {
		writeError(w, http.StatusNotFound, "Request explanations are switched off (-explain-ring is negative).", "not_found")
		return
	}
	e, ok := s.explains.get(id)
	if !ok {
		writeError(w, http.StatusNotFound,
			fmt.Sprintf("No record of that request id. It may be mistyped, older than the last %d requests the companion remembers, or from before the companion restarted. Records are kept in memory only.", s.explains.cap),
			"not_found")
		return
	}
	writeJSON(w, http.StatusOK, e)
}
