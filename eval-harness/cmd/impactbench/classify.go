package main

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Verdicts for one served answer.
const (
	VCorrect    = "correct"     // same text a fresh, uncached call returns
	VStale      = "stale"       // a price question answered with a different (older) price
	VWrongMatch = "wrong_match" // differs from the fresh answer and is not a price difference
	VError      = "error"       // not HTTP 200 (or no usable answer)
	VToolReuse  = "tool_reused" // tool request answered from the cache
	VToolFresh  = "tool_fresh"  // tool request answered by the model
	// VUnjudged: -truth context only. A semantic cache hit whose answer text the
	// client never saw being written (for example an entry the companion's
	// check stored), so it cannot say which question it was written for.
	VUnjudged = "unjudged"
)

// Truth modes (-truth).
const (
	TruthMock    = "mock"    // a fresh, uncached call to the mock; exact text compare
	TruthContext = "context" // real model: price from the request context, FAQ by origin
)

// ClassifyContext judges one served answer without a ground-truth call, for a
// real model whose wording changes from call to call:
//
//   - a price question is right when the first dollar amount in the answer is
//     the price the request's own context states, stale when it is another
//     amount, and a wrong match when it states no amount;
//   - any other answer that did not come from the cache is a fresh answer to
//     this request, so it is right;
//   - a cached FAQ or ordinary answer is right when it was first written for a
//     question that means the same (MeaningGroup), and a wrong match when it was
//     written for a different one, such as the other half of a look-alike pair.
//     origin is the question the answer text was first seen for in this arm; if
//     the text was never seen, a direct (exact) hit is right and a semantic hit
//     is unjudged.
func ClassifyContext(it Item, status int, served string, fromCache bool, hitType, origin string, originKnown bool) string {
	if status != 200 || strings.TrimSpace(served) == "" {
		return VError
	}
	if it.Kind == KindTool {
		if fromCache {
			return VToolReuse
		}
		return VToolFresh
	}
	if it.Kind == KindPrice {
		sp, ok := ExtractPrice(served)
		if !ok {
			return VWrongMatch
		}
		got, err1 := strconv.ParseFloat(sp, 64)
		if err1 != nil || got != float64(it.Price) {
			return VStale
		}
		return VCorrect
	}
	if !fromCache {
		return VCorrect
	}
	if originKnown {
		if MeaningGroup(origin) == MeaningGroup(it.Question) {
			return VCorrect
		}
		return VWrongMatch
	}
	if strings.EqualFold(hitType, "direct") {
		return VCorrect
	}
	return VUnjudged
}

var priceInText = regexp.MustCompile(`\$\s*([0-9][0-9,]*(?:\.[0-9]+)?)`)

// ExtractPrice returns the first dollar amount stated in text.
func ExtractPrice(text string) (string, bool) {
	m := priceInText.FindStringSubmatch(text)
	if m == nil {
		return "", false
	}
	return strings.ReplaceAll(m[1], ",", ""), true
}

// Classify judges one served answer against the answer a fresh, uncached call
// returned for the same request. Tool requests are not judged for correctness
// (the mock model cannot compute from a tool result); they report only whether
// the answer came out of the cache.
func Classify(it Item, status int, served, truth string, fromCache bool) string {
	if status != 200 || strings.TrimSpace(served) == "" {
		return VError
	}
	if it.Kind == KindTool {
		if fromCache {
			return VToolReuse
		}
		return VToolFresh
	}
	if normalize(served) == normalize(truth) {
		return VCorrect
	}
	if it.Kind == KindPrice {
		sp, sok := ExtractPrice(served)
		tp, tok := ExtractPrice(truth)
		if sok && tok && sp != tp {
			return VStale
		}
	}
	return VWrongMatch
}

func normalize(s string) string { return strings.Join(strings.Fields(s), " ") }

// IsBad reports whether a verdict counts as a bad answer: one that reached the
// user and is not what a fresh call returns, or no answer at all.
func IsBad(v string) bool { return v == VStale || v == VWrongMatch || v == VError }

// answerText pulls choices[0].message.content out of a chat completion.
func answerText(body []byte) string {
	var r struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &r) != nil || len(r.Choices) == 0 {
		return ""
	}
	return r.Choices[0].Message.Content
}

// median of values (mean of the two middle ones for an even count).
func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	s := append([]float64(nil), values...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// Metrics is one arm's numbers for one run, by metric key.
type Metrics map[string]float64

// MedianMetrics takes the median of each key across runs. A key missing from a
// run counts as 0 for that run.
func MedianMetrics(runs []Metrics) Metrics {
	keys := map[string]bool{}
	for _, m := range runs {
		for k := range m {
			keys[k] = true
		}
	}
	out := Metrics{}
	for k := range keys {
		vals := make([]float64, len(runs))
		for i, m := range runs {
			vals[i] = m[k]
		}
		out[k] = median(vals)
	}
	return out
}
