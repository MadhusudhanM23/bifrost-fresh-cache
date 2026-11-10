package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// limits are the honest caveats printed with every result. Nothing in them is
// a measured number.
func limits(r *Report) []string {
	if r.Settings.realModel() {
		return realModelLimits(r)
	}
	out := []string{
		"The model and the embeddings are mocks (tools/mock-openai). The embedding is a bag-of-words hash, so which questions count as \"similar\" is decided by shared words, not by meaning. Look-alike pairs reach the cache only because the mock scores them high; a real embedding model would score some pairs higher and some lower. Do not read any rate here as a rate for real traffic.",
		"The workload is small and scripted (see \"Workload\"). The per-1000 rates are the counts of this one story scaled to 1000 requests, not a forecast. Repeats differ only in the order of independent items, so they show order sensitivity and run-to-run noise, not statistical confidence.",
		"The \"right\" answer is what a fresh, uncached call to the mock returns for the same request. For price questions that is the price in the system message the app sent, which is what a real RAG app's model would also state. For tool-result requests the mock cannot compute an answer, so they are reported as \"answer reused from the cache although the request carried live tool data\", not as right or wrong.",
		"The mock model answers instantly (no artificial chat latency) and the stack runs on one machine, so latencies are the cost of the extra hops and checks alone. A real model takes hundreds of milliseconds to seconds per call, which would make every avoided or added model call matter far more than the proxy hop does.",
		"FAQ and look-alike requests carry no system message. The mock only uses its canned look-alike answers when there are no context lines, so these requests cannot also carry the price context.",
		"The companion arm is configured as in the Docker demo (pre-serve gate below its -verify-below score, audit of every semantic hit, guard for tool results). A semantic hit scoring at or above -verify-below is served first and only audited afterwards, so a wrong match in that score range is detected after the user already received it. The two-factor on/off pair scores a similarity of 1.0 with this embedding (\"on\" and \"off\" are stopwords), so no score threshold can separate it.",
		"The plain arm runs Bifrost with the settings of docker/bifrost-config.json (see \"Settings\"), not a tuned setup. A shorter TTL removes stale answers by letting entries expire, at the price of fewer cache hits and so more model calls; the plain-ttl arm (when present) measures that trade-off on this workload. A higher similarity threshold would stop some look-alike matches but also loses true paraphrase hits, and it cannot separate pairs that score 1.0.",
		"Model calls are counted from the mock's own counters (GET /mock/stats) as a difference before and after each arm, after waiting for background audits to finish. The ground-truth calls are made outside those windows. Anything else that uses the mock during an arm would be counted too; the command checks that the counters are idle before starting and reports a negative serving-call count as a note. Embedding calls are counted the same way and reported separately; they are not model-answer calls.",
		"After every sequential response that was not served from the cache the client pauses (-write-gap) before the next request, because Bifrost writes a new cache entry after it has answered and the next request could otherwise overtake the write. The pause is the same for every arm and is not part of any latency. A first pilot without it gave different hit counts for the same request list in different arms, which is how the need for it was found.",
		"The plain-ttl arm expires entries after a few seconds, so what it serves depends on how far apart requests are in time. Its call counts and wrong matches can move from run to run with timing; read it as an illustration of the trade-off, not as a precise figure. With a short TTL an entry can expire while a look-alike neighbour that was written later is still alive, so a short TTL does not only remove stale answers: it can also change which wrong matches happen.",
		"The share of look-alike questions, price questions and tool-using requests in this workload is a choice made for the demonstration (look-alike pairs are in it on purpose). Real traffic will have different shares, so the per-1000 rates would be different.",
		"The companion's evidence fetches (an HTTP GET of the demo page) are not model calls and are not counted here.",
		"Cost figures multiply measured call counts by prices that are assumptions given on the command line. They are an illustration, not a measurement.",
		"The page is edited in place by this command, which needs exclusive use of it and of the stack. The same file is restored byte for byte at the end of every arm and on exit.",
	}
	return out
}

// realModelLimits are the caveats of a run against a real model (-truth context).
func realModelLimits(r *Report) []string {
	return []string{
		fmt.Sprintf("Answers come from a real model (%s) on a free tier; the embeddings are still the local mock (a bag-of-words hash). So which questions the cache treats as \"similar\" is decided by shared words, exactly as in the mock run, and look-alike pairs reach the cache only because the mock embedding scores them high. A real embedding model would score some pairs higher and some lower. Do not read any rate here as a rate for real traffic.", r.Settings.Model),
		"The workload is small and scripted (see \"Workload\"). The per-1000 rates are the counts of this one story scaled to 1000 requests, not a forecast. With a single run there is no run-to-run spread at all, and a real model adds its own randomness.",
		"No ground-truth model calls are made. A price answer is right when it states the price that its own request's context gives (the sanity check before the run showed the model doing this); a model that ignored its context would be counted as stale here even on a fresh call. Any other answer is judged by where it came from: a fresh answer is right, and a cached one is right only when the client saw it written for a question that means the same (see MeaningGroup in workload.go). This catches look-alike mix-ups, not every way a real answer can be poor; answer quality itself is not judged. Tool-result requests are reported as reused from the cache or not, not as right or wrong.",
		"The companion's settings differ from the Docker demo used for the mock run: background audits are off (-audit-rate=0, -audit-near-margin=0) because each one is a paid model call and they only report a wrong match after it was served, and the pre-serve gate compares answers with a model judge (-audit-compare=judge) because real answers never match word for word. So each gate check costs two model calls (a fresh answer and the judge). See \"Settings\".",
		"FAQ and look-alike requests carry no system message, as in the mock run.",
		"Model calls are counted from Bifrost's own Prometheus counter bifrost_upstream_requests_total (chat requests that Bifrost sent to a provider with a key; cache hits carry no key and are left out), as a difference before and after each arm, after waiting for background work to finish. Embedding calls are counted by the mock, which serves them. The command checks that the counters are idle before starting.",
		"Free tiers limit requests and tokens per minute. The client pauses after every non-cached response (-write-gap) to stay under the limit, and waits longer after each page change (-settle); the pauses are the same for every arm and are not part of any latency. A rate-limit refusal would show up as an error.",
		"The plain-ttl arm expires entries after its TTL, so what it serves depends on how far apart requests are in time. Its TTL and the pauses differ from the mock run (see \"Settings\"), so its numbers are not comparable one for one with the mock run's plain-ttl column.",
		"The plain arm runs Bifrost with the semantic-cache settings of docker/bifrost-config.real.json, which are the same as in the mock run (see \"Settings\"), not a tuned setup.",
		"The companion's evidence fetches (an HTTP GET of the demo page) are not model calls and are not counted here.",
		"Cost figures multiply measured call counts by prices that are assumptions given on the command line. They are an illustration, not a measurement.",
		"The page is edited in place by this command, which needs exclusive use of it and of the stack. The same file is restored byte for byte at the end of every arm and on exit.",
	}
}

type row struct {
	key, label string
	fmt        string // int, f1, f2, pct, ms, usd
}

func fmtVal(f string, v float64) string {
	switch f {
	case "int":
		return fmt.Sprintf("%.0f", v)
	case "f1":
		return fmt.Sprintf("%.1f", v)
	case "f2":
		return fmt.Sprintf("%.2f", v)
	case "pct":
		return fmt.Sprintf("%.1f%%", v)
	case "ms":
		return fmt.Sprintf("%.1f", v)
	case "usd":
		return fmt.Sprintf("$%.3f", v)
	}
	return fmt.Sprintf("%v", v)
}

func table(w func(string, ...any), arms []Arm, rows []row, get func(arm string) Metrics) {
	w("| Metric |")
	for _, a := range arms {
		w(" %s |", a.Name)
	}
	w("\n|---|")
	for range arms {
		w("---:|")
	}
	w("\n")
	for _, r := range rows {
		w("| %s |", r.label)
		for _, a := range arms {
			m := get(a.Name)
			w(" %s |", fmtVal(r.fmt, m[r.key]))
		}
		w("\n")
	}
	w("\n")
}

var headlineRows = []row{
	{"requests", "Requests", "int"},
	{"cache_hits", "Served from the cache (X-Bifrost-Cache-Hit)", "int"},
	{"cache_hit_rate_pct", "Cache hit rate", "pct"},
	{"model_calls", "Model calls made through the arm (all causes)", "int"},
	{"stale", "Stale answers served (old price)", "int"},
	{"wrong_match", "Wrong-match answers served", "int"},
	{"errors", "Errors (not HTTP 200)", "int"},
	{"bad", "Bad answers (stale + wrong match + errors)", "int"},
	{"bad_per_1000", "Bad answers per 1000 requests", "f1"},
	{"tool_requests", "Requests that carried a tool result", "int"},
	{"tool_reused", "...of which answered from the cache", "int"},
	{"lat_p50_ms", "Latency p50, warm + after-change requests (ms)", "ms"},
	{"lat_p95_ms", "Latency p95, warm + after-change requests (ms)", "ms"},
}

var breakdownRows = []row{
	{"bad_price", "Bad answers on price questions", "int"},
	{"bad_faq", "Bad answers on look-alike / FAQ questions", "int"},
	{"bad_benign", "Bad answers on benign questions", "int"},
}

var overheadRows = []row{
	{"model_calls", "Model calls, total", "int"},
	{"serving_calls", "...answering requests (misses, refreshes)", "int"},
	{"gate_calls", "...pre-serve gate checks", "int"},
	{"audit_calls", "...background audits", "int"},
	{"embedding_calls", "Embedding calls (cache lookups and writes, separate from model calls)", "int"},
	{"calls_saved_vs_no_cache", "Requests minus model calls (calls saved versus no cache at all)", "int"},
	{"gate_blocked", "Gate verdicts 'blocked' (wrong match replaced by a fresh answer)", "int"},
	{"gate_trusted", "Gate verdicts 'trusted' (no new call)", "int"},
	{"audit_different", "Audits that found the served answer different from a fresh one", "int"},
	{"guard_bypassed", "Requests kept out of the cache by the guard", "int"},
	{"cost_per_1000_usd", "ASSUMED cost per 1000 requests (model calls x assumed price)", "usd"},
}

var latencyRows = []row{
	{"lat_p50_ms", "All sequential requests: p50 (ms)", "ms"},
	{"lat_p95_ms", "All sequential requests: p95 (ms)", "ms"},
	{"lat_hit_n", "Requests served from the cache", "int"},
	{"lat_hit_p50_ms", "...p50 (ms)", "ms"},
	{"lat_hit_p95_ms", "...p95 (ms)", "ms"},
	{"lat_nonhit_n", "Requests not served from the cache", "int"},
	{"lat_nonhit_p50_ms", "...p50 (ms)", "ms"},
	{"lat_nonhit_p95_ms", "...p95 (ms)", "ms"},
}

var burstRows = []row{
	{"burst_requests", "Concurrent requests", "int"},
	{"burst_errors", "Errors", "int"},
	{"burst_stale", "Stale answers served", "int"},
	{"burst_wrong_match", "Wrong-match answers served", "int"},
	{"burst_correct", "Correct answers", "int"},
	{"burst_hits", "Served from the cache", "int"},
	{"burst_model_calls", "Model calls caused (burst window, including checks)", "int"},
	{"burst_p50_ms", "Latency p50 (ms)", "ms"},
	{"burst_p95_ms", "Latency p95 (ms)", "ms"},
	{"burst_wall_ms", "Time until the last response (ms)", "ms"},
}

func renderMarkdown(r *Report) string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }

	w("# Impact benchmark: Bifrost alone versus Bifrost with the freshness companion\n\n")
	w("> **%s**\n\n", r.Banner)
	w("Generated by `impactbench` (eval-harness). Started %s, finished %s. %d runs, seed %d (run i uses seed + i - 1).\n\n", r.StartedAt, r.FinishedAt, r.Settings.Repeat, r.Settings.Seed)
	w("Command: `%s`\n\n", strings.Join(r.Command, " "))

	w("## What was measured\n\n")
	w("A support bot answers price questions. Its app puts the **current** price into the system message at request time, and a demo page is the source of truth for that price. The same scripted request list is replayed against each arm, one arm completely after the other, with the first arm changing from run to run.\n\n")
	w("1. **Warm**: price questions in several wordings, plus each look-alike pair (A then B) and two ordinary questions, with the page at $%d.\n", r.Settings.BasePrice)
	w("2. **Change**: the page is set to $%d and the harness waits %s (longer than the companion's evidence cache).\n", r.Settings.ChangedPrice, r.Settings.Settle)
	w("3. **After the change**: the same and new price wordings (now with the $%d context), look-alike pairs in the opposite order (B then A), same-meaning rewordings, ordinary questions, and %d requests that carry a role \"tool\" message (the same question twice with a different tool result).\n", r.Settings.ChangedPrice, countKind(r.Workload.FirstRun, KindTool))
	w("4. **Burst**: the page is set to $%d, the harness waits %s, then %d identical price requests are released at the same moment at the now-stale entry.\n\n", r.Settings.BurstPrice, r.Settings.Settle, r.Settings.Burst)
	if r.Settings.realModel() {
		w("Answers come from a real model (`%s`), so the wording changes from call to call and an exact text compare would call almost everything wrong. Answers are judged without extra model calls instead (`-truth context`): **Right answer** = for a price question, an answer that states the price in that request's own context; for any other question, a fresh answer, or a cached answer that was first written for a question that means the same. **Stale** = a price question answered with a different price. **Wrong match** = a price question answered with no price, or a cached answer first written for a question that means something else (for example the other half of a look-alike pair). **Error** = not HTTP 200 or an empty answer. **Bad answer** = stale, wrong match or error.\n\n", r.Settings.Model)
	} else {
		w("**Right answer** = what a fresh, uncached call to the mock model returns for that exact request. **Stale** = a price question answered with a different price than the fresh answer states. **Wrong match** = any other answer that differs from the fresh answer. **Error** = not HTTP 200. **Bad answer** = stale, wrong match or error.\n\n")
	}
	w("Arms (each uses its own cache key in every run, so no two arms ever share cache entries):\n\n")
	for _, a := range r.Arms {
		extra := ""
		if len(a.Headers) > 0 {
			var hs []string
			for k, v := range a.Headers {
				hs = append(hs, k+": "+v)
			}
			sort.Strings(hs)
			extra = ", extra header " + strings.Join(hs, ", ")
		}
		w("- **%s**: %s, `%s`%s\n", a.Name, a.Label, a.URL, extra)
	}
	w("\n")

	w("## Headline: median of %d runs\n\n", len(r.Runs))
	head := headlineRows
	if r.Settings.realModel() {
		head = append(append([]row(nil), headlineRows[:8]...), append([]row{{"unjudged", "Cached answers not judged (origin unknown)", "int"}}, headlineRows[8:]...)...)
	}
	table(w, r.Arms, head, func(a string) Metrics { return r.Median[a] })
	w("Where the bad answers came from (median of runs):\n\n")
	table(w, r.Arms, breakdownRows, func(a string) Metrics { return r.Median[a] })

	w("### Burst after a price change (median of runs)\n\n")
	table(w, r.Arms, burstRows, func(a string) Metrics { return r.Median[a] })

	w("### Cost of the checks, and what the cache still saves (median of runs)\n\n")
	table(w, r.Arms, overheadRows, func(a string) Metrics { return r.Median[a] })
	w("Cost rows use **assumed** prices, not measured ones: $%g per model call and $%g per embedding call (command-line flags `-assumed-usd-per-call`, `-assumed-usd-per-embedding`). Change them and the cost rows scale; the call counts do not change.\n\n", r.Settings.CostPerCall, r.Settings.CostPerEmbed)

	w("### Latency (median of runs)\n\n")
	table(w, r.Arms, latencyRows, func(a string) Metrics { return r.Median[a] })
	if r.Settings.realModel() {
		w("Latency is measured by the client for each request (the pauses between requests are not included). Non-cached requests include a real model call over the internet (free tier), so they are far slower than on the mock and vary with the provider's load. A request")
	} else {
		w("Latency is measured by the client for each request (the pauses between requests are not included). The mock model has no artificial delay, so these are the costs of hops and checks only. A request")
	}
	w(" that the plain arm answers from a stale entry in a few milliseconds is, in the companion arm, either confirmed (a few milliseconds) or refreshed with a model call (about one model call of latency), so the sequential p50 and p95 mix cases that are not comparable one for one; the hit / non-hit rows split them by what served the request.\n\n")

	w("## Where each setup helped or did not (all runs added together)\n\n")
	w("Each cell is `bad answers / requests (served from cache)`. A look-alike row covers both questions of the pair, asked in the opposite order to the warm-up (the second question of the pair first); a bad answer there means one of the two got the other one's answer.\n\n")
	scenarios := map[string]bool{}
	for _, run := range r.Runs {
		for _, ar := range run.Arms {
			for s := range ar.Scenarios {
				scenarios[s] = true
			}
		}
	}
	var names []string
	for s := range scenarios {
		names = append(names, s)
	}
	sort.Strings(names)
	w("| Scenario |")
	for _, a := range r.Arms {
		w(" %s |", a.Name)
	}
	w("\n|---|")
	for range r.Arms {
		w("---:|")
	}
	w("\n")
	for _, s := range names {
		w("| %s |", s)
		for _, a := range r.Arms {
			bad, total, hits := 0, 0, 0
			for _, run := range r.Runs {
				if t := run.Arms[a.Name].Scenarios[s]; t != nil {
					bad += t.Bad
					total += t.Total
					hits += t.Hits
				}
			}
			w(" %d / %d (%d) |", bad, total, hits)
		}
		w("\n")
	}
	w("\nFor the tool-result row, \"bad\" is always 0 because those requests are not judged for correctness; read the number in brackets (answers reused from the cache although the request carried live tool data).\n\n")

	w("## Every run\n\n")
	perRun := []row{
		{"cache_hits", "cache hits", "int"}, {"model_calls", "model calls", "int"}, {"gate_calls", "gate calls", "int"}, {"audit_calls", "audit calls", "int"},
		{"stale", "stale", "int"}, {"wrong_match", "wrong match", "int"}, {"errors", "errors", "int"}, {"bad_per_1000", "bad / 1000", "f1"},
		{"tool_reused", "tool reused", "int"}, {"lat_p50_ms", "p50 ms", "ms"}, {"lat_p95_ms", "p95 ms", "ms"}, {"burst_model_calls", "burst model calls", "int"}, {"burst_stale", "burst stale", "int"}, {"burst_errors", "burst errors", "int"},
	}
	w("| Run | Seed | Arm order | Arm |")
	for _, p := range perRun {
		w(" %s |", p.label)
	}
	w("\n|---|---:|---|---|")
	for range perRun {
		w("---:|")
	}
	w("\n")
	for _, run := range r.Runs {
		for _, a := range r.Arms {
			m := run.Arms[a.Name].Metrics
			w("| %d | %d | %s | %s |", run.Index, run.Seed, strings.Join(run.ArmOrder, " > "), a.Name)
			for _, p := range perRun {
				w(" %s |", fmtVal(p.fmt, m[p.key]))
			}
			w("\n")
		}
	}
	w("\n")

	if hdr := freshnessSummary(r); hdr != "" {
		w("## What the companion reported on its responses (all runs added together)\n\n%s\n", hdr)
	}

	w("## Workload\n\n")
	w("Requests per phase (run 1): ")
	var ph []string
	for _, p := range []string{PhaseWarm, PhaseAfter, PhaseBurst} {
		ph = append(ph, fmt.Sprintf("%s %d", p, r.Workload.PhaseCounts[p]))
	}
	w("%s. The list is built from the seed (shuffles only), so a run is reproducible; the seeds used were %v.\n\n", strings.Join(ph, ", "), r.Workload.Seeds)
	w("Cosine similarity the mock embedding gives the question pairs the workload depends on (computed on `user: <lowercased question>`, as the cache embeds a one-message chat; Bifrost threshold %v):\n\n", r.Stack.SemanticCache["threshold"])
	w("| Pair | Cosine | A | B |\n|---|---:|---|---|\n")
	for _, s := range r.Workload.Similarity {
		w("| %s | %.4f | %s | %s |\n", s.Label, s.Cosine, s.A, s.B)
	}
	w("\n")
	for _, n := range lookAlikeNotes(r) {
		w("%s\n", n)
	}
	w("\n")

	w("## Settings\n\n")
	w("| | |\n|---|---|\n")
	w("| Bifrost semantic cache (plain arm) | %s |\n", mapLine(r.Stack.SemanticCache))
	composeName := "docker-compose.yml"
	if r.Settings.realModel() && r.Settings.ComposePath != "" {
		composeName = filepath.Base(r.Settings.ComposePath)
	}
	w("| Companion flags (from %s) | `%s` |\n", composeName, strings.Join(r.Stack.CompanionFlags, " "))
	if r.Settings.realModel() {
		w("| Model | `%s`, max_tokens %d, assistant tool-call message before tool results: %v |\n", r.Settings.Model, r.Settings.MaxTokens, r.Settings.ToolCallMessage)
		w("| Model calls counted from | %s |\n", r.Settings.CallsFrom)
		w("| Pause after each non-cached response | %s |\n", r.Settings.WriteGap)
	}
	if r.Stack.Audits != nil {
		w("| Companion audit endpoint | enabled=%v, compare=%v |\n", r.Stack.Audits["enabled"], r.Stack.Audits["compare"])
	}
	w("| Companion policy guard | `%s` |\n", guardLine(r.Stack.PolicyGuard))
	w("| Wait after each page change | %s |\n", r.Settings.Settle)
	w("| Prices on the page | %d, then %d, then %d |\n", r.Settings.BasePrice, r.Settings.ChangedPrice, r.Settings.BurstPrice)
	w("| Burst size | %d concurrent requests |\n", r.Settings.Burst)
	w("| Cache keys | `%s` |\n", r.Settings.CacheKeyPattern)
	w("| Demo page | `%s`, %d bytes, sha256 `%s`, restored byte for byte after the run: %v |\n", r.Page.Path, r.Page.OriginalBytes, r.Page.OriginalSHA256, r.Page.ByteIdenticalAfter)
	if len(r.Stack.Unavailable) > 0 {
		w("| Could not read | %s |\n", strings.Join(r.Stack.Unavailable, "; "))
	}
	w("\nIn this Bifrost configuration the system prompt is **not** part of the cache key (`exclude_system_prompt`), so a request with a new price in its system message still matches an entry written under the old price. That is the staleness mechanism the benchmark exercises; it is a setting of the plain arm, shown above.\n\n")

	w("## Environment\n\n")
	env := r.Environment
	w("| | |\n|---|---|\n")
	w("| Client | %s, %s, %d logical CPUs, %s |\n", env.ClientLocation, env.OS, env.LogicalCPUs, env.CPUModel)
	w("| Go | %s (%s/%s) |\n", env.GoVersion, env.GOOS, env.GOARCH)
	w("| Docker | %v |\n", env.Docker["engine"])
	cs := containerMap(env.Docker["containers"])
	var ckeys []string
	for k := range cs {
		ckeys = append(ckeys, k)
	}
	sort.Strings(ckeys)
	for _, k := range ckeys {
		w("| Container %s | %s |\n", k, cs[k])
	}
	w("\n")

	if len(r.Notes) > 0 {
		w("## Notes from the runs\n\n")
		for _, n := range r.Notes {
			w("- %s\n", n)
		}
		w("\n")
	}

	w("## Honest limits\n\n")
	for _, l := range r.Limits {
		w("- %s\n", l)
	}
	w("\n")
	return b.String()
}

func mapLine(m map[string]any) string {
	if len(m) == 0 {
		return "unavailable"
	}
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, m[k]))
	}
	return "`" + strings.Join(parts, ", ") + "`"
}

// freshnessSummary lists, for arms that sent through the companion, how often
// each x-freshness-* header value appeared.
func freshnessSummary(r *Report) string {
	var b strings.Builder
	for _, a := range r.Arms {
		totals := map[string]map[string]int{}
		for _, run := range r.Runs {
			for k, vals := range run.Arms[a.Name].Headers {
				if totals[k] == nil {
					totals[k] = map[string]int{}
				}
				for v, n := range vals {
					totals[k][v] += n
				}
			}
		}
		if len(totals) == 0 {
			continue
		}
		fmt.Fprintf(&b, "Arm **%s**:\n\n", a.Name)
		var keys []string
		for k := range totals {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			var vs []string
			for v, n := range totals[k] {
				vs = append(vs, fmt.Sprintf("%s: %d", v, n))
			}
			sort.Strings(vs)
			fmt.Fprintf(&b, "- `%s`: %s\n", k, strings.Join(vs, ", "))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func countKind(items []Item, kind string) int {
	n := 0
	for _, it := range items {
		if it.Kind == kind {
			n++
		}
	}
	return n
}

// containerMap reads the recorded container descriptions whether they were just
// collected (map[string]string) or read back from a JSON file (map[string]any).
func containerMap(v any) map[string]string {
	out := map[string]string{}
	switch m := v.(type) {
	case map[string]string:
		for k, s := range m {
			out[k] = s
		}
	case map[string]any:
		for k, s := range m {
			out[k] = fmt.Sprint(s)
		}
	}
	return out
}

// guardLine prints the policy's guard section without its comment.
func guardLine(g any) string {
	if m, ok := g.(map[string]any); ok {
		c := map[string]any{}
		for k, v := range m {
			if k != "_comment" {
				c[k] = v
			}
		}
		g = c
	}
	b, err := json.Marshal(g)
	if err != nil {
		return "unavailable"
	}
	return string(b)
}

// lookAlikeNotes says, for each look-alike pair, whether the plain cache could
// match it at all, from the similarities measured before the runs.
func lookAlikeNotes(r *Report) []string {
	thr, _ := r.Stack.SemanticCache["threshold"].(float64)
	var out []string
	for _, s := range r.Workload.Similarity {
		if !strings.HasPrefix(s.Label, "look-alike pair") {
			continue
		}
		switch {
		case thr == 0:
		case s.Cosine < thr:
			out = append(out, fmt.Sprintf("- %s: cosine %.4f is below the cache threshold %v, so Bifrost cannot confuse these two (a look-alike that is not a problem with this embedding).", s.Label, s.Cosine, thr))
		case s.Cosine >= 0.999:
			out = append(out, fmt.Sprintf("- %s: cosine %.4f. The embedding cannot tell the two questions apart at all, so no similarity threshold can separate them.", s.Label, s.Cosine))
		default:
			out = append(out, fmt.Sprintf("- %s: cosine %.4f is above the cache threshold %v, so Bifrost can serve one question's answer for the other.", s.Label, s.Cosine, thr))
		}
	}
	return out
}
