package proxy

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Receipt headers. The receipt is a one-line summary of the freshness check
// that stands behind an answer: what was checked, when, with what result. It
// repeats facts that other x-freshness-* headers already carry, in one place
// and in a fixed order, so a client (or a person reading a log) does not have
// to collect them from a dozen headers.
const (
	// HeaderReceipt is set exactly when X-Freshness-Status is set: on a checked
	// cache hit, a checked new answer and a refused stale answer. Verified
	// answers and streams carry no status header and so no receipt.
	HeaderReceipt = "X-Freshness-Receipt"
	// HeaderEvidence says where the evidence behind the check came from:
	// "fetched" (the source was read for this request) or "reused" (notes kept
	// from an earlier read, at most -evidence-ttl old).
	HeaderEvidence = "X-Freshness-Evidence"
)

const (
	EvidenceFetched = "fetched"
	EvidenceReused  = "reused"

	receiptVersion = "1"
	// receiptMaxLen bounds the header. When the pairs do not fit, trailing
	// pairs are dropped whole; a value is never cut in the middle.
	receiptMaxLen = 1024
)

// Receipt is the structured form of the receipt header, as shown by
// GET /freshness/explain/{id}.
type Receipt struct {
	Verdict           string     `json:"verdict"`                       // X-Freshness-Status
	Action            string     `json:"action"`                        // X-Freshness-Action
	Answer            string     `json:"answer,omitempty"`              // cached | new
	Fact              string     `json:"fact,omitempty"`                // the watched fact that decided
	Source            string     `json:"source,omitempty"`              // its source URL
	Provider          string     `json:"provider,omitempty"`            // evidence provider
	Found             string     `json:"found,omitempty"`               // value the source states now
	Stated            string     `json:"stated,omitempty"`              // value the checked answer stated
	ServedCheck       string     `json:"served_check,omitempty"`        // how the served answer compared, after a refresh or correction
	CheckedAt         *time.Time `json:"checked_at,omitempty"`          // when the evidence was fetched
	CheckedAgoSeconds *int64     `json:"checked_ago_seconds,omitempty"` // how long before this answer that was
	Evidence          string     `json:"evidence,omitempty"`            // fetched | reused
}

// receiptFromHeaders builds the receipt from the freshness headers already set
// on a response. ok is false when no freshness check ran (no status header).
func receiptFromHeaders(h http.Header, now time.Time) (r Receipt, ok bool) {
	status := h.Get(HeaderStatus)
	if status == "" {
		return Receipt{}, false
	}
	r = Receipt{
		Verdict:  status,
		Action:   h.Get(HeaderAction),
		Answer:   h.Get(HeaderAnswer),
		Fact:     h.Get(HeaderFact),
		Source:   h.Get(HeaderSource),
		Provider: h.Get(HeaderProvider),
		Found:    h.Get(HeaderCurrentValue),
		Evidence: h.Get(HeaderEvidence),
	}
	// A cached answer states Cached-Value; a checked new answer states Answer-Value.
	r.Stated = h.Get(HeaderCachedValue)
	if r.Stated == "" {
		r.Stated = h.Get(HeaderAnswerValue)
	}
	// After a correction the served answer is the corrected one, so its check
	// is the one that matters; otherwise the refreshed answer's check.
	if h.Get(HeaderCorrection) == "corrected" {
		r.ServedCheck = h.Get(HeaderCorrectedCheck)
	} else {
		r.ServedCheck = h.Get(HeaderRefreshedCheck)
	}
	if t, err := time.Parse(time.RFC3339, h.Get(HeaderCheckedAt)); err == nil {
		t = t.UTC()
		r.CheckedAt = &t
		ago := int64(now.Sub(t) / time.Second)
		if ago < 0 {
			ago = 0
		}
		r.CheckedAgoSeconds = &ago
	}
	return r, true
}

// setReceipt adds X-Freshness-Receipt when the headers say a freshness check
// ran. It runs just before the headers go out, so every other header it reads
// is final. It never touches the body.
func setReceipt(h http.Header, now time.Time) {
	r, ok := receiptFromHeaders(h, now)
	if !ok {
		return
	}
	h.Set(HeaderReceipt, r.header())
}

// header renders the receipt as "; "-separated key=value pairs. Strings are
// quoted with \" and \\ escaped; fixed vocabulary and numbers are bare. The
// most useful pairs come first, so when the 1024-character bound is reached
// the least useful are the ones dropped.
func (r Receipt) header() string {
	type pair struct {
		key, value string
		quoted     bool
	}
	pairs := []pair{
		{"v", receiptVersion, false},
		{"verdict", r.Verdict, false},
		{"action", r.Action, false},
		{"found", r.Found, true},
	}
	if r.CheckedAgoSeconds != nil {
		pairs = append(pairs, pair{"checked_ago_s", strconv.FormatInt(*r.CheckedAgoSeconds, 10), false})
	}
	pairs = append(pairs, pair{"source", r.Source, true}, pair{"provider", r.Provider, true})
	if r.CheckedAt != nil {
		pairs = append(pairs, pair{"checked_at", r.CheckedAt.UTC().Format(time.RFC3339), true})
	}
	pairs = append(pairs,
		pair{"evidence", r.Evidence, false},
		pair{"stated", r.Stated, true},
		pair{"served_check", r.ServedCheck, false},
		pair{"fact", r.Fact, true},
		pair{"answer", r.Answer, false},
	)
	var b strings.Builder
	for _, p := range pairs {
		if p.value == "" {
			continue
		}
		v := p.value
		if p.quoted || !bareToken(v) {
			v = quoteReceipt(v)
		}
		seg := p.key + "=" + v
		sep := 0
		if b.Len() > 0 {
			sep = 2
		}
		if b.Len()+sep+len(seg) > receiptMaxLen {
			break // drop this pair and the ones after it
		}
		if sep > 0 {
			b.WriteString("; ")
		}
		b.WriteString(seg)
	}
	return b.String()
}

// bareToken reports whether s can stand unquoted: letters, digits, '_', '-'
// and '.' only. Everything the companion writes bare passes this; the check
// is a guard, not a parser.
func bareToken(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-', c == '.':
		default:
			return false
		}
	}
	return true
}

// quoteReceipt makes a quoted string: headerValue first (ASCII only, control
// characters removed, length bounded), then \ and " escaped. The order matters:
// headerValue writes non-ASCII as \uXXXX, and escaping afterwards keeps that
// text exactly as the other headers show it.
func quoteReceipt(s string) string {
	s = headerValue(s)
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' || s[i] == '"' {
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	b.WriteByte('"')
	return b.String()
}
