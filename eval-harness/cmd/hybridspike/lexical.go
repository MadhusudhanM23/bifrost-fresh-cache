package main

import (
	"sort"
	"strings"
	"unicode"
)

// stopwords are words that carry no topic: a difference in one of these ("to"
// vs "from") is NOT seen by the lexical rule. That is deliberate (it is how a
// simple keyword check behaves) and the report lists the look-alikes it misses.
var stopwords = toSet(strings.Fields(`a an the i me my we our you your it its is are was be been am do does did
can could would should will shall may might to of in on at by for from with about as into
and or but if so than then there this that these those how what when where which who whom why
please just also not no yes get got have has had`))

// defaultSynonyms maps a word to a canonical word. It is short and general on
// purpose (the same list is used for every pair); every entry changes the answer
// for some pair, so the report shows results with and without it.
var defaultSynonyms = map[string]string{
	"2fa": "2fa", "two": "2fa", "factor": "2fa", "authentication": "2fa", "auth": "2fa",
	"cost": "price", "price": "price", "pricing": "price",
	"bill": "invoice", "invoice": "invoice",
	"e-mail": "email", "mail": "email",
	"pay": "payment", "payment": "payment",
	"order": "order", "purchase": "order",
	"money": "refund", "refund": "refund",
	"remove": "delete", "erase": "delete", "delete": "delete",
	"change": "update", "update": "update", "edit": "update",
	"enable": "enable", "turn": "enable",
	"rename": "rename",
	"token":  "key", "key": "key",
	"crash": "crash", "close": "crash",
	"stop": "cancel", "cancel": "cancel",
	"abroad": "international", "internationally": "international", "international": "international",
}

func toSet(words []string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}

// stem is a deliberately tiny stemmer (plural "s", "es", "ies", "ing", "ed").
func stem(w string) string {
	switch {
	case len(w) > 5 && strings.HasSuffix(w, "ies"):
		return w[:len(w)-3] + "y"
	case len(w) > 5 && strings.HasSuffix(w, "ing"):
		return w[:len(w)-3]
	case len(w) > 4 && strings.HasSuffix(w, "ed"):
		return w[:len(w)-2]
	case len(w) > 4 && strings.HasSuffix(w, "es"):
		return w[:len(w)-2]
	case len(w) > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss"):
		return w[:len(w)-1]
	}
	return w
}

// ContentWords lowercases s, splits it on anything that is not a letter or
// digit, drops stopwords, stems, maps synonyms (syn may be nil) and returns the
// sorted set of distinct words.
func ContentWords(s string, syn map[string]string) []string {
	seen := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if stopwords[w] {
			continue
		}
		w = stem(w)
		if c, ok := syn[w]; ok {
			w = c
		}
		seen[w] = true
	}
	out := make([]string, 0, len(seen))
	for w := range seen {
		out = append(out, w)
	}
	sort.Strings(out)
	return out
}

// Diff returns the words only a has and the words only b has.
func Diff(a, b []string) (aOnly, bOnly []string) {
	in := func(set []string, w string) bool {
		i := sort.SearchStrings(set, w)
		return i < len(set) && set[i] == w
	}
	for _, w := range a {
		if !in(b, w) {
			aOnly = append(aOnly, w)
		}
	}
	for _, w := range b {
		if !in(a, w) {
			bOnly = append(bOnly, w)
		}
	}
	return aOnly, bOnly
}

// StrictReject: reject when either question has a content word the other lacks.
// It rejects almost every real paraphrase, because paraphrases use other words.
func StrictReject(aOnly, bOnly []string) bool { return len(aOnly) > 0 || len(bOnly) > 0 }

// SwapReject: reject when the questions are the same sentence with a small number
// of words swapped for other words: each side has at least one word the other
// lacks, and neither side has more than k. A pair with many differences is a
// reworded question, where the rule says nothing and the vector decides.
func SwapReject(aOnly, bOnly []string, k int) bool {
	return len(aOnly) > 0 && len(bOnly) > 0 && len(aOnly) <= k && len(bOnly) <= k
}

// Confusion counts decisions against labels (shouldShare = label "hit").
type Confusion struct {
	TrueHits    int      `json:"true_hits"`
	FalseHits   int      `json:"false_hits"`
	FalseMisses int      `json:"false_misses"`
	TrueMisses  int      `json:"true_misses"`
	Precision   *float64 `json:"precision"`
	Recall      *float64 `json:"recall"`
	F1          float64  `json:"f1"`
}

// Count adds one decision.
func (c *Confusion) Count(shouldShare, served bool) {
	switch {
	case shouldShare && served:
		c.TrueHits++
	case !shouldShare && served:
		c.FalseHits++
	case shouldShare && !served:
		c.FalseMisses++
	default:
		c.TrueMisses++
	}
}

// Finish computes precision, recall and F1 (precision is nil when nothing was served).
func (c *Confusion) Finish() {
	if c.TrueHits+c.FalseHits > 0 {
		p := float64(c.TrueHits) / float64(c.TrueHits+c.FalseHits)
		c.Precision = &p
	}
	if c.TrueHits+c.FalseMisses > 0 {
		r := float64(c.TrueHits) / float64(c.TrueHits+c.FalseMisses)
		c.Recall = &r
	}
	if c.Precision != nil && c.Recall != nil && *c.Precision+*c.Recall > 0 {
		c.F1 = 2 * *c.Precision * *c.Recall / (*c.Precision + *c.Recall)
	}
}

// Evaluate serves a pair when served(i) is true and returns the confusion.
func Evaluate(shouldShare []bool, served func(i int) bool) Confusion {
	var c Confusion
	for i, s := range shouldShare {
		c.Count(s, served(i))
	}
	c.Finish()
	return c
}

// Grid returns 0.00, 0.02, ..., 1.00 as exact two-decimal values.
func Grid() []float64 {
	var g []float64
	for i := 0; i <= 50; i++ {
		g = append(g, float64(i*2)/100)
	}
	return g
}

// BestThreshold picks the threshold with the best F1. Ties go to the higher
// precision, then to the LOWER threshold (nothing is lost by being lenient when
// the scores tie). sims[i] >= t means the vector check passes.
func BestThreshold(sims []float64, shouldShare []bool, grid []float64) (float64, Confusion) {
	bestT, best := grid[0], Confusion{}
	first := true
	for _, t := range grid {
		c := Evaluate(shouldShare, func(i int) bool { return sims[i] >= t })
		better := first || c.F1 > best.F1+1e-12
		if !better && c.F1 >= best.F1-1e-12 && c.Precision != nil && best.Precision != nil && *c.Precision > *best.Precision+1e-12 {
			better = true
		}
		if better {
			bestT, best, first = t, c, false
		}
	}
	return bestT, best
}

// RecallThreshold returns the highest grid threshold whose recall is at least
// target; ok is false when even the lowest threshold cannot reach it.
func RecallThreshold(sims []float64, shouldShare []bool, grid []float64, target float64) (float64, bool) {
	for i := len(grid) - 1; i >= 0; i-- {
		t := grid[i]
		c := Evaluate(shouldShare, func(j int) bool { return sims[j] >= t })
		if c.Recall != nil && *c.Recall >= target-1e-12 {
			return t, true
		}
	}
	return 0, false
}
