package main

import (
	"fmt"
	"sort"
	"strings"
)

type method struct {
	name   string
	accept func(i int) bool // lexical check; nil = vector only
	tuned  string
}

// analyse scores every method at every operating point.
func analyse(rep *Report, facts []PairFacts) {
	n := len(facts)
	sims := make([]float64, n)
	should := make([]bool, n)
	byLabel := map[string][]float64{}
	for i, f := range facts {
		sims[i] = f.Cosine
		should[i] = f.Label == "hit"
		byLabel[f.Label] = append(byLabel[f.Label], f.Cosine)
	}
	rep.SimilarityBy = map[string]Spread{}
	for l, v := range byLabel {
		s := append([]float64(nil), v...)
		sort.Float64s(s)
		rep.SimilarityBy[l] = Spread{Count: len(s), Min: s[0], Median: s[len(s)/2], Max: s[len(s)-1]}
	}
	grid := Grid()

	type op struct {
		name, why string
		t         float64
	}
	var ops []op
	bt, _ := BestThreshold(sims, should, grid)
	ops = append(ops, op{"best-F1 vector threshold", "the threshold that separates the two groups best with the vector alone (ties: higher precision, then lower threshold)", bt})
	ops = append(ops, op{"deployed threshold 0.80", "the threshold in docker/bifrost-config.json", 0.80})
	if rt, ok := RecallThreshold(sims, should, grid, 0.80); ok {
		ops = append(ops, op{"recall >= 0.80 threshold", "the highest threshold where the vector still serves at least 80% of the true paraphrases: the regime a real embedding model lives in, where look-alikes sit next to paraphrases", rt})
	}
	ops = append(ops, op{"no vector filter", "every pair passes the vector check; shows what the keyword check can do on its own", -1})

	k := rep.SwapK
	for _, o := range ops {
		vecPass := func(i int) bool { return sims[i] >= o.t }
		base := Evaluate(should, vecPass)
		pt := Point{Name: o.name, Why: o.why, Threshold: o.t}
		methods := buildMethods(facts, k, vecPass, should)
		for _, m := range methods {
			accept := m.accept
			served := func(i int) bool { return vecPass(i) && (accept == nil || accept(i)) }
			c := Evaluate(should, served)
			r := MethodResult{Method: m.name, Confusion: c, WrongKept: c.FalseHits,
				WrongRemoved: base.FalseHits - c.FalseHits, RightLost: base.TrueHits - c.TrueHits}
			if m.tuned != "" {
				r.TunedOnThisData, r.TunedParameterNote = true, m.tuned
			}
			for i, f := range facts {
				if served(i) && !should[i] {
					r.StillWrong = append(r.StillWrong, f.ID)
				}
				if vecPass(i) && should[i] && !served(i) {
					r.LostRight = append(r.LostRight, f.ID)
				}
			}
			pt.Methods = append(pt.Methods, r)
		}
		rep.Points = append(rep.Points, pt)
	}

	// Sensitivity of the swap rule to k, at the "no vector filter" point so the
	// rule stands on its own.
	for _, syn := range []bool{false, true} {
		for kk := 1; kk <= 4; kk++ {
			kk, syn := kk, syn
			c := Evaluate(should, func(i int) bool { return !swapRejects(facts[i], kk, syn) })
			rep.KSensitivity = append(rep.KSensitivity, KRow{K: kk, Synonyms: syn, Confusion: c})
		}
	}
}

func swapRejects(f PairFacts, k int, syn bool) bool {
	if syn {
		return SwapReject(f.GoAOnlySyn, f.GoBOnlySyn, k)
	}
	return SwapReject(f.GoAOnly, f.GoBOnly, k)
}

func buildMethods(facts []PairFacts, k int, vecPass func(int) bool, should []bool) []method {
	ms := []method{
		{name: "vector only"},
		{name: fmt.Sprintf("vector + PostgreSQL full-text swap rule (k=%d)", k), accept: func(i int) bool {
			return !SwapReject(facts[i].FTSAOnly, facts[i].FTSBOnly, k)
		}},
		{name: "vector + PostgreSQL full-text strict (each question must contain every word of the other)", accept: func(i int) bool {
			return facts[i].FTSBothWay
		}},
		{name: fmt.Sprintf("vector + differing-word swap rule (k=%d, no synonyms)", k), accept: func(i int) bool {
			return !swapRejects(facts[i], k, false)
		}},
		{name: fmt.Sprintf("vector + differing-word swap rule (k=%d, with synonym list)", k), accept: func(i int) bool {
			return !swapRejects(facts[i], k, true)
		}},
		{name: "vector + differing-word strict rule (any differing word, with synonym list)", accept: func(i int) bool {
			return !StrictReject(facts[i].GoAOnlySyn, facts[i].GoBOnlySyn)
		}},
	}
	// ts_rank cut-off: the best one for THIS vector point on THIS data. It is an
	// upper bound for what tuning could give, not something a real deployment would get.
	bestR, bestF1 := -1.0, -1.0
	for _, f := range facts {
		r := f.FTSRank
		c := Evaluate(should, func(i int) bool { return vecPass(i) && facts[i].FTSRank >= r })
		if c.F1 > bestF1+1e-12 {
			bestF1, bestR = c.F1, r
		}
	}
	r := bestR
	ms = append(ms, method{name: fmt.Sprintf("vector + ts_rank >= %.3f (cut-off tuned on these same pairs: optimistic)", r),
		accept: func(i int) bool { return facts[i].FTSRank >= r },
		tuned:  fmt.Sprintf("ts_rank cut-off %.4f picked to maximise F1 on this data", r)})
	return ms
}

func caveats(rep *Report) []string {
	return []string{
		"Both embedding sources here are bag-of-words (feature hashing of words). Two texts are close only when they share words, which is exactly what a keyword check also looks at. A keyword check therefore adds less new information on top of a real embedding model than it appears to add here, and a real model scores look-alikes much closer to paraphrases than these do. Treat the numbers as an upper bound for the benefit, not a measurement of it.",
		fmt.Sprintf("Tiny, hand-written dataset: %d pairs (%d should share, %d should not). A single pair moves a rate by about 2 percentage points; there is no held-out set and no confidence interval.", rep.Pairs, rep.Positives, rep.Negatives),
		"The dataset was built so that every should-not-share pair is the SAME sentence with one concept swapped, and every should-share pair is a reworded sentence. A rule that looks for 'same sentence, one word different' is therefore close to the dataset's own definition. Real look-alikes are not always one swapped word, and real paraphrases are sometimes one swapped word ('stop' for 'cancel').",
		"The synonym list was written after reading the paraphrase pairs, so the 'with synonym list' rows are optimistic. The 'no synonyms' rows show how many true paraphrases the rule would hurt without that help. The ts_rank cut-off rows are tuned on the same pairs and are an upper bound.",
		"The keyword check cannot see stopwords, so 'ship to Canada' vs 'ship from Canada' passes it. It also cannot catch a look-alike whose difference is a number or a word the stemmer maps to a synonym.",
	}
}

func renderMarkdown(rep *Report) string {
	var sb strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&sb, format+"\n", args...) }
	w("# %s", rep.Banner)
	w("")
	w("Hybrid search spike (keywords + meaning). Generated by `hybridspike` (eval-harness). Started %s, finished %s.", rep.StartedAt, rep.FinishedAt)
	w("")
	w("Command: `%s`", strings.Join(rep.Command, " "))
	w("")
	w("| | |")
	w("|---|---|")
	w("| Embedder | %s |", rep.Embedder)
	w("| Dataset | `%s`: **%d pairs = %d should-share + %d should-not-share** (the same file `cmd/cachequality` uses) |", rep.Dataset, rep.Pairs, rep.Positives, rep.Negatives)
	w("| Text embedded | `user: <lowercased question>`, as the semantic cache does for a one-message chat request |")
	w("| Dimension | %d |", rep.Dimension)
	w("| PostgreSQL | %s, scratch schema `%s` (dropped after the run) |", rep.Postgres, rep.Schema)
	w("| Go cosine vs PostgreSQL cosine | max abs difference %.2e |", rep.MaxCosineErr)
	w("| Swap rule | reject when each question has 1..%d content words the other lacks |", rep.SwapK)
	for _, l := range []string{"hit", "miss"} {
		s := rep.SimilarityBy[l]
		name := map[string]string{"hit": "should-share", "miss": "should-not-share"}[l]
		w("| Cosine of %s pairs | min %.3f, median %.3f, max %.3f (n=%d) |", name, s.Min, s.Median, s.Max, s.Count)
	}
	w("")
	w("## Read this first")
	w("")
	for _, c := range rep.Caveats {
		w("- %s", c)
	}
	w("")
	for _, p := range rep.Points {
		w("## Operating point: %s (threshold %s)", p.Name, thr(p.Threshold))
		w("")
		w("%s.", p.Why)
		w("")
		w("TH = right matches served, FH = wrong matches served, FM = right matches not served, TM = wrong matches blocked. Removed/lost are measured against `vector only` at the same threshold.")
		w("")
		w("| Method | TH | FH (wrong kept) | FM | TM | Precision | Recall | F1 | Wrong removed | Right lost |")
		w("|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|")
		for _, m := range p.Methods {
			c := m.Confusion
			w("| %s | %d | %d | %d | %d | %s | %s | %.3f | %d | %d |", m.Method, c.TrueHits, c.FalseHits, c.FalseMisses, c.TrueMisses, pct(c.Precision), pct(c.Recall), c.F1, m.WrongRemoved, m.RightLost)
		}
		w("")
		for _, m := range p.Methods {
			if len(m.StillWrong)+len(m.LostRight) == 0 || strings.HasPrefix(m.Method, "vector only") {
				continue
			}
			if strings.Contains(m.Method, "swap rule (k=2, with synonym list)") || strings.Contains(m.Method, "PostgreSQL full-text swap") {
				w("- `%s`: wrong matches still served: %s; right matches lost: %s", m.Method, idList(m.StillWrong), idList(m.LostRight))
			}
		}
		w("")
	}
	w("## Swap rule on its own (no vector filter), by k")
	w("")
	w("| k | synonym list | TH | FH | FM | TM | Precision | Recall | F1 |")
	w("|---:|---|---:|---:|---:|---:|---:|---:|---:|")
	for _, r := range rep.KSensitivity {
		c := r.Confusion
		w("| %d | %v | %d | %d | %d | %d | %s | %s | %.3f |", r.K, r.Synonyms, c.TrueHits, c.FalseHits, c.FalseMisses, c.TrueMisses, pct(c.Precision), pct(c.Recall), c.F1)
	}
	w("")
	w("## Per pair")
	w("")
	w("| id | label | cosine | words only in A / only in B (synonyms applied) | PostgreSQL lexemes only in A / only in B | ts_rank |")
	w("|---|---|---:|---|---|---:|")
	for _, f := range rep.Pairwise {
		w("| %s | %s | %.3f | %s / %s | %s / %s | %.3f |", f.ID, f.Label, f.Cosine, join(f.GoAOnlySyn), join(f.GoBOnlySyn), join(f.FTSAOnly), join(f.FTSBOnly), f.FTSRank)
	}
	return sb.String()
}

func thr(t float64) string {
	if t < 0 {
		return "none"
	}
	return fmt.Sprintf("%.2f", t)
}

func pct(p *float64) string {
	if p == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.3f", *p)
}

func join(s []string) string {
	if len(s) == 0 {
		return "-"
	}
	return strings.Join(s, ", ")
}

func idList(ids []string) string {
	if len(ids) == 0 {
		return "none"
	}
	return strings.Join(ids, ", ")
}
