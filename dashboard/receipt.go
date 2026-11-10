package main

import (
	"strconv"
	"strings"
)

// Receipt is this dashboard's reading of the companion's X-Freshness-Receipt
// header: one line, "; "-separated key=value pairs, strings quoted with \"
// and \\ escaped. It is parsed here independently (no import from the
// companion module, same approach as DecisionRecord), so the dashboard only
// depends on the documented header format.
type Receipt struct {
	Verdict           string `json:"verdict"`
	Action            string `json:"action,omitempty"`
	Answer            string `json:"answer,omitempty"` // cached | new
	Fact              string `json:"fact,omitempty"`
	Source            string `json:"source,omitempty"`
	Provider          string `json:"provider,omitempty"`
	Found             string `json:"found,omitempty"`        // what the source says now
	Stated            string `json:"stated,omitempty"`       // what the checked answer said
	ServedCheck       string `json:"served_check,omitempty"` // how the served answer compared, after a refresh or correction
	CheckedAt         string `json:"checked_at,omitempty"`
	CheckedAgoSeconds *int64 `json:"checked_ago_seconds,omitempty"`
	Evidence          string `json:"evidence,omitempty"` // fetched | reused
	// Text is the receipt in plain words, ready to show on the page.
	Text string `json:"text"`
}

// parseReceiptHeader reads the header. It returns nil when there is no
// receipt or no verdict in it. It never panics on a strange value: pairs it
// cannot read are skipped, and an unterminated quoted string ends the parse.
func parseReceiptHeader(raw string) *Receipt {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	r := &Receipt{}
	for raw != "" {
		eq := strings.IndexByte(raw, '=')
		if eq < 0 {
			break
		}
		key := strings.TrimSpace(raw[:eq])
		rest := raw[eq+1:]
		var val string
		if strings.HasPrefix(rest, `"`) {
			var b strings.Builder
			i, closed := 1, false
			for i < len(rest) {
				c := rest[i]
				switch {
				case c == '\\' && i+1 < len(rest):
					b.WriteByte(rest[i+1])
					i += 2
					continue
				case c == '"':
					closed = true
				default:
					b.WriteByte(c)
				}
				i++
				if closed {
					break
				}
			}
			if !closed {
				break
			}
			val, rest = b.String(), rest[i:]
		} else {
			end := strings.IndexByte(rest, ';')
			if end < 0 {
				end = len(rest)
			}
			val, rest = strings.TrimSpace(rest[:end]), rest[end:]
		}
		r.set(key, val)
		rest = strings.TrimSpace(rest)
		if !strings.HasPrefix(rest, ";") {
			break
		}
		raw = strings.TrimSpace(rest[1:])
	}
	if r.Verdict == "" {
		return nil
	}
	r.Text = receiptWords(r)
	return r
}

func (r *Receipt) set(key, val string) {
	switch key {
	case "verdict":
		r.Verdict = val
	case "action":
		r.Action = val
	case "answer":
		r.Answer = val
	case "fact":
		r.Fact = val
	case "source":
		r.Source = val
	case "provider":
		r.Provider = val
	case "found":
		r.Found = val
	case "stated":
		r.Stated = val
	case "served_check":
		r.ServedCheck = val
	case "checked_at":
		r.CheckedAt = val
	case "checked_ago_s":
		if n, err := strconv.ParseInt(val, 10, 64); err == nil && n >= 0 {
			r.CheckedAgoSeconds = &n
		}
	case "evidence":
		r.Evidence = val
	}
}

// receiptWords says the receipt in one plain sentence, for example:
//
//	Checked http://demoserver:8000/ 12 s before this answer via direct fetch
//	(notes reused): page said $59, still correct.
func receiptWords(r *Receipt) string {
	var b strings.Builder
	if r.Source != "" {
		b.WriteString("Checked " + r.Source)
		if r.CheckedAgoSeconds != nil {
			b.WriteString(" " + agoWords(*r.CheckedAgoSeconds) + " before this answer")
		}
		if p := providerWords(r.Provider); p != "" {
			b.WriteString(" via " + p)
		}
		switch r.Evidence {
		case "reused":
			b.WriteString(" (notes reused)")
		case "fetched":
			b.WriteString(" (fresh look)")
		}
		b.WriteString(": ")
	} else {
		b.WriteString("No source page was checked: ")
	}
	b.WriteString(verdictWords(r))
	b.WriteString(".")
	return b.String()
}

func verdictWords(r *Receipt) string {
	found := moneyish(r.Fact, r.Found)
	stated := moneyish(r.Fact, r.Stated)
	said := func() string {
		if found == "" {
			return ""
		}
		return "page said " + found
	}
	switch r.Verdict {
	case "confirmed":
		if found == "" {
			return "the saved answer was still correct"
		}
		return said() + ", still correct"
	case "stale":
		s := "the saved answer was out of date"
		if found != "" && stated != "" {
			s = said() + " but the saved answer said " + stated + ", so it was out of date"
		} else if found != "" {
			s = said() + ", so the saved answer was out of date"
		}
		return s + afterRefresh(r)
	case "contradicted":
		s := "the new answer was wrong"
		if found != "" && stated != "" {
			s = said() + " but the new answer said " + stated + ", so it was wrong"
		}
		switch r.Action {
		case "corrected":
			return s + "; it was corrected"
		case "uncached":
			return s + "; it was removed from the cache"
		}
		return s + "; it was served and labelled"
	case "unable_to_verify":
		return "it could not be checked" + afterUnchecked(r)
	case "unknown":
		return "it was not checked" + afterUnchecked(r)
	}
	return r.Verdict
}

func afterRefresh(r *Receipt) string {
	switch r.ServedCheck {
	case "matches_current":
		return "; the new answer matches the page"
	case "differs_from_current":
		return "; the new answer still differs from the page"
	case "no_value":
		return "; the new answer gives no value"
	}
	if r.Action == "refresh_failed" {
		return "; no new answer could be had"
	}
	return ""
}

func afterUnchecked(r *Receipt) string {
	switch r.Action {
	case "served_with_warning":
		return ", so the saved answer was served with a warning"
	case "refreshed":
		return ", so the model was asked again"
	case "refresh_failed":
		return ", and no new answer could be had"
	}
	return ""
}

// moneyish puts the "$" back on a plain number when the fact is about money.
// The companion compares prices as numbers and reports them without the sign.
func moneyish(fact, v string) string {
	if v == "" {
		return ""
	}
	f := strings.ToLower(fact)
	if !(strings.Contains(f, "price") || strings.Contains(f, "cost") || strings.Contains(f, "fee")) {
		return v
	}
	for _, c := range v {
		if !(c >= '0' && c <= '9' || c == '.' || c == ',') {
			return v
		}
	}
	return "$" + v
}

func providerWords(p string) string {
	switch {
	case p == "":
		return ""
	case strings.HasPrefix(p, "direct_fetch"):
		return "direct fetch"
	case p == "tavily_extract":
		return "Tavily Extract"
	case p == "tavily_search":
		return "Tavily Search"
	}
	return p
}

func agoWords(s int64) string {
	switch {
	case s < 60:
		return strconv.FormatInt(s, 10) + " s"
	case s < 3600:
		return strconv.FormatInt(s/60, 10) + " min"
	}
	return strconv.FormatInt(s/3600, 10) + " h"
}
