// Command checksim measures smart checking and the background watcher
// without any paid API: it drives the real evidence cache, schedule,
// watcher and decision engine on a virtual clock, with a fake page that
// follows a value trace and Poisson request arrivals.
//
//	go run ./cmd/checksim -seed 20261110 -out ../eval-harness/results/checksim-20261110
//
// It prints a Markdown report and, with -out, writes <out>.md and <out>.json.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

func main() {
	seed := flag.Int64("seed", 20261110, "random seed (the run is deterministic per seed)")
	out := flag.String("out", "", "write <out>.md and <out>.json")
	only := flag.String("traces", "", "comma-separated trace names to run (default all)")
	flag.Parse()

	traces := DefaultTraces()
	if *only != "" {
		want := map[string]bool{}
		for _, n := range strings.Split(*only, ",") {
			want[strings.TrimSpace(n)] = true
		}
		var kept []Trace
		for _, t := range traces {
			if want[t.Name] {
				kept = append(kept, t)
			}
		}
		traces = kept
	}
	report, err := Report(traces, DefaultArms(), *seed, time.Now().UTC())
	if err != nil {
		fmt.Fprintln(os.Stderr, "checksim:", err)
		os.Exit(1)
	}
	fmt.Print(report.Markdown)
	if *out != "" {
		if err := os.WriteFile(*out+".md", []byte(report.Markdown), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "checksim:", err)
			os.Exit(1)
		}
		data, _ := json.MarshalIndent(report, "", "  ")
		if err := os.WriteFile(*out+".json", data, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "checksim:", err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "wrote", *out+".md", "and", *out+".json")
	}
}

// ReportData is the JSON file.
type ReportData struct {
	GeneratedAt time.Time `json:"generated_at"`
	Seed        int64     `json:"seed"`
	Arms        []Arm     `json:"arms"`
	Traces      []Trace   `json:"traces"`
	Results     []Metrics `json:"results"`
	Markdown    string    `json:"-"`
}

// Report runs every trace against every arm and renders the Markdown.
func Report(traces []Trace, arms []Arm, seed int64, at time.Time) (ReportData, error) {
	r := ReportData{GeneratedAt: at, Seed: seed, Arms: arms, Traces: traces}
	for _, tr := range traces {
		for _, arm := range arms {
			m, err := Run(tr, arm, seed)
			if err != nil {
				return r, fmt.Errorf("%s / %s: %w", tr.Name, arm.Name, err)
			}
			r.Results = append(r.Results, m)
		}
	}
	r.Markdown = markdown(r)
	return r, nil
}

func markdown(r ReportData) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# checksim: smart checking and the background watcher, measured without paid APIs\n\n")
	fmt.Fprintf(&b, "Generated %s, seed %d. `go run ./cmd/checksim -seed %d` in `freshness-companion/` reproduces every number.\n\n", r.GeneratedAt.Format(time.RFC3339), r.Seed, r.Seed)
	b.WriteString("**What runs.** The real `evidence.Cache`, `watch.Schedule`, `watch.Watcher` and `decision.Engine` on a virtual clock. A fake page states the Acme Pro price and follows a trace (49, then +10 at each change). Requests arrive as a Poisson process (whole seconds); each asks one of a few questions that share the fact, so there are a few Bifrost entries with deterministic ids. A miss writes the model's answer (the true current value) and runs the new-answer check; a hit is decided against evidence exactly as the proxy does, including the correction rule (`on_contradicted: correct`), which rewrites a right answer with a stale evidence value when the evidence is older than the page. No model call is simulated as costing anything; only evidence fetches are counted.\n\n")
	b.WriteString("**Columns.** fetches = evidence fetches (request / watcher split); waited = requests that had to wait for a fetch; stale = answers served that state a value other than the page's value at that moment; refreshed = hits found stale and re-asked; corrected = answers rewritten to the evidence value; evicted = entries the watcher deleted; skips = watcher checks skipped over the hourly cap; lag = time from a page change to the first fetch that saw it (worst / mean).\n\n")
	b.WriteString("## Arms\n\n| Arm | Floor | Cap | Growth | Watcher | Notices |\n|---|---|---|---|---|---|\n")
	for _, a := range r.Arms {
		w := "off"
		if a.Watch {
			w = fmt.Sprintf("on, %d/h, idle %s", a.WatchMaxPerHour, a.WatchIdle)
		}
		growth := "-"
		if a.Adaptive {
			growth = fmt.Sprintf("x%g", a.Growth)
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %v |\n", a.Name, a.Floor, a.Max, growth, w, a.Notices)
	}
	b.WriteString("\n")
	for _, tr := range r.Traces {
		fmt.Fprintf(&b, "## Trace `%s`: %s\n\n", tr.Name, tr.Description)
		b.WriteString("| Arm | Requests (hits/misses) | Fetches (request + watcher) | Fetches/h | Waited | Stale served | Refreshed | Corrected | Evicted | Skips | Detected | Lag worst / mean |\n|---|---|---|---|---|---|---|---|---|---|---|---|\n")
		var base *Metrics
		for i := range r.Results {
			m := &r.Results[i]
			if m.Trace != tr.Name {
				continue
			}
			if base == nil {
				base = m
			}
			fetchNote := ""
			if base != nil && base != m && base.EvidenceFetches > 0 {
				fetchNote = fmt.Sprintf(" (%+.0f%%)", 100*float64(m.EvidenceFetches-base.EvidenceFetches)/float64(base.EvidenceFetches))
			}
			fmt.Fprintf(&b, "| %s | %d (%d/%d) | %d (%d + %d)%s | %.1f | %d | %d (%.2f%%) | %d | %d | %d | %d | %d/%d | %s / %s |\n",
				m.Arm, m.Requests, m.Hits, m.Misses, m.EvidenceFetches, m.RequestFetches, m.WatcherFetches, fetchNote, m.FetchesPerHour,
				m.RequestsWaited, m.StaleServed, m.StalePercent, m.Refreshed, m.Corrected, m.Evictions, m.BudgetSkips, m.Detected, m.Changes,
				secs(m.WorstLagSecs), secs(m.MeanLagSecs))
		}
		b.WriteString("\nThe percentage after the fetch count compares with the first arm (`fixed-5s`).\n\n")
	}
	b.WriteString("## Reading the numbers honestly\n\n")
	b.WriteString(reading(r))
	b.WriteString("\n## Assumptions\n\n")
	b.WriteString("- Tavily credit rate, if these fetches were Tavily Extract calls: basic extract costs 1 credit per 5 URLs, so credits = fetches / 5. The default docker stack uses the direct provider, where a fetch costs nothing but a local HTTP GET.\n")
	b.WriteString("- The model always answers the true current value on a miss or refresh. A real model can be wrong; that is a separate problem (the new-answer check and the audit) and is not measured here.\n")
	b.WriteString("- Arrivals are Poisson and rounded to whole seconds; the watcher ticks once per virtual second. Nothing runs concurrently in the simulator, so a watcher fetch never coincides with a request fetch.\n")
	b.WriteString("- The caps and floors are the flag defaults (`-evidence-ttl 5s`, `-check-max 5m`, `-check-growth 2`, `-watch-idle 30m`) except the watcher cap, which is the docker default `WATCH_MAX_PER_HOUR` 120 (the flag's own default is 60); they are not the docker `CHECK_MAX` default of 60s. A 60 s cap saves less and goes stale for less long.\n")
	return b.String()
}

func secs(v float64) string {
	if v == 0 {
		return "0s"
	}
	return (time.Duration(v*1000) * time.Millisecond).Round(100 * time.Millisecond).String()
}

// reading writes the plain-language conclusions straight from the numbers,
// so the text can never disagree with the table.
func reading(r ReportData) string {
	var b strings.Builder
	byKey := map[string]Metrics{}
	for _, m := range r.Results {
		byKey[m.Trace+"/"+m.Arm] = m
	}
	for _, tr := range r.Traces {
		base, ok := byKey[tr.Name+"/fixed-5s"]
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "**%s.** ", tr.Name)
		for _, arm := range []string{"adaptive", "adaptive-cap60s", "adaptive+watch", "adaptive+watch+notices"} {
			m, ok := byKey[tr.Name+"/"+arm]
			if !ok || base.EvidenceFetches == 0 {
				continue
			}
			delta := 100 * float64(m.EvidenceFetches-base.EvidenceFetches) / float64(base.EvidenceFetches)
			switch {
			case delta <= -10:
				fmt.Fprintf(&b, "`%s` fetches %.0f%% less than `fixed-5s` (%d vs %d)", arm, -delta, m.EvidenceFetches, base.EvidenceFetches)
			case delta >= 10:
				fmt.Fprintf(&b, "`%s` fetches %.0f%% MORE than `fixed-5s` (%d vs %d): no saving here", arm, delta, m.EvidenceFetches, base.EvidenceFetches)
			default:
				fmt.Fprintf(&b, "`%s` fetches about the same as `fixed-5s` (%d vs %d): no saving here", arm, m.EvidenceFetches, base.EvidenceFetches)
			}
			fmt.Fprintf(&b, "; it served %d stale answers (%s: %d) with a worst detection lag of %s (%s: %s)", m.StaleServed, "fixed-5s", base.StaleServed, secs(m.WorstLagSecs), "fixed-5s", secs(base.WorstLagSecs))
			if m.RequestsWaited < base.RequestsWaited {
				fmt.Fprintf(&b, "; requests that waited for a fetch: %d instead of %d", m.RequestsWaited, base.RequestsWaited)
			}
			if m.Evictions > 0 {
				fmt.Fprintf(&b, "; the watcher deleted %d old answers", m.Evictions)
			}
			if m.BudgetSkips > 0 {
				fmt.Fprintf(&b, "; %d watcher checks were skipped over the hourly cap", m.BudgetSkips)
			}
			if m.StalePercent >= 5 {
				fmt.Fprintf(&b, ". THAT IS A LOT OF STALE ANSWERS: the page changes faster than the cap lets the companion notice, and every one of them was served as \"confirmed\" against evidence up to the cap old; use a shorter -check-max or send change notices for a page like this")
			}
			b.WriteString(". ")
		}
		b.WriteString("\n\n")
	}
	return b.String()
}
