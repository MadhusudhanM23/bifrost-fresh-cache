package proxy

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"freshness-companion/internal/decision"
)

// Freshness response headers. The JSON body is never changed, so OpenAI
// clients keep working; everything the companion adds is in these headers.
const (
	HeaderStatus         = "X-Freshness-Status"          // confirmed | stale | unable_to_verify | unknown
	HeaderAction         = "X-Freshness-Action"          // served | served_with_warning | refreshed | refresh_failed
	HeaderReason         = "X-Freshness-Reason"          // human-readable explanation
	HeaderCategory       = "X-Freshness-Category"        // matched categories, comma-separated
	HeaderFact           = "X-Freshness-Fact"            // watched fact that decided
	HeaderSource         = "X-Freshness-Source"          // the fact's source URL
	HeaderProvider       = "X-Freshness-Provider"        // evidence provider ("direct_fetch (not Tavily)" when not Tavily)
	HeaderCheckedAt      = "X-Freshness-Checked-At"      // when the evidence was fetched (RFC 3339)
	HeaderCachedValue    = "X-Freshness-Cached-Value"    // value extracted from the cached answer
	HeaderCurrentValue   = "X-Freshness-Current-Value"   // value extracted from the evidence
	HeaderAgeSeconds     = "X-Freshness-Age-Seconds"     // tracked entry age, when known
	HeaderWarning        = "X-Freshness-Warning"         // set when served without confirmation
	HeaderRefreshedValue = "X-Freshness-Refreshed-Value" // value in the refreshed answer
	HeaderRefreshedCheck = "X-Freshness-Refreshed-Check" // matches_current | differs_from_current | no_value
	HeaderError          = "X-Freshness-Error"           // why a refresh failed
	HeaderAnswer         = "X-Freshness-Answer"          // cached (a hit was checked) | new (a fresh answer was checked)
	HeaderChangeNotice   = "X-Freshness-Change-Notice"   // id of the change notice that forced this check
	HeaderAnswerValue    = "X-Freshness-Answer-Value"    // value stated by a new answer that was checked
	HeaderSharedRefresh  = "X-Freshness-Shared-Refresh"  // "true": this request shared a refresh another request ran
	HeaderCorrection     = "X-Freshness-Correction"      // reported | uncached | corrected
	HeaderCorrectedValue = "X-Freshness-Corrected-Value" // value in the answer asked for again with the verified value
	HeaderCorrectedCheck = "X-Freshness-Corrected-Check" // matches_current | differs_from_current | no_value
)

// Executed actions, as reported in x-freshness-action and the decision log.
const (
	ActionServed            = "served"
	ActionServedWithWarning = "served_with_warning"
	ActionRefreshed         = "refreshed"
	ActionRefreshFailed     = "refresh_failed"
	// ActionCorrected: the answer contradicted the evidence; the model was
	// asked again with the verified value and its new answer matches.
	ActionCorrected = "corrected"
	// ActionUncached: the answer contradicted the evidence; it is returned,
	// labelled, and its cache entry was deleted so nobody else reuses it.
	ActionUncached = "uncached"
)

const warningText = "cached answer served without evidence that it is current"

func annotate(h http.Header, d decision.Decision, action string) {
	set := func(name, value string) {
		if value != "" {
			h.Set(name, headerValue(value))
		}
	}
	set(HeaderStatus, string(d.Outcome))
	set(HeaderAction, action)
	set(HeaderAnswer, "cached")
	set(HeaderReason, d.Reason)
	set(HeaderCategory, strings.Join(d.Categories, ","))
	set(HeaderFact, d.Fact)
	set(HeaderSource, d.SourceURL)
	set(HeaderProvider, d.Provider)
	if !d.CheckedAt.IsZero() {
		set(HeaderCheckedAt, d.CheckedAt.UTC().Format(time.RFC3339))
		// Evidence was obtained: say whether it was read for this request or
		// reused from the evidence cache (the receipt repeats this).
		if d.EvidenceCached {
			set(HeaderEvidence, EvidenceReused)
		} else {
			set(HeaderEvidence, EvidenceFetched)
		}
	}
	set(HeaderCachedValue, d.CachedValue)
	set(HeaderCurrentValue, d.CurrentValue)
	if d.AgeKnown {
		set(HeaderAgeSeconds, strconv.FormatInt(int64(d.Age/time.Second), 10))
	}
	if action == ActionServedWithWarning {
		set(HeaderWarning, warningText)
	}
}

// annotateNew labels a checked fresh answer (a cache miss).
func annotateNew(h http.Header, d decision.Decision, action string) {
	annotate(h, d, action)
	h.Set(HeaderAnswer, "new")
	h.Del(HeaderAgeSeconds)
	h.Del(HeaderCachedValue)
	if d.CachedValue != "" {
		h.Set(HeaderAnswerValue, headerValue(d.CachedValue))
	}
}

// headerValue makes s safe for an HTTP header: control characters become
// spaces, non-ASCII is escaped as \uXXXX, and the length is bounded.
func headerValue(s string) string {
	const limit = 512
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= limit {
			b.WriteString("...")
			break
		}
		switch {
		case r == '\t' || (r >= 0x20 && r < 0x7f):
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			b.WriteByte(' ')
		case r <= 0xffff:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	return b.String()
}
