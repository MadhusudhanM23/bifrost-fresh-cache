// Command impactbench measures what the freshness companion changes in front of
// Bifrost's semantic cache, on one deterministic workload replayed against each
// arm of the same running stack:
//
//	plain      the client talks straight to Bifrost
//	companion  the client talks to the freshness companion, which proxies to
//	           that same Bifrost (gate, audit and guard as configured)
//	plain-ttl  optional: straight to Bifrost with a short per-request cache TTL
//	           (x-bf-cache-ttl), the "what if we just expire entries quickly"
//	           alternative
//
// The story: a support bot whose app puts the CURRENT price into the system
// message at request time; a demo page is the source of truth for the price.
// Warm the cache at the base price, change the page, ask again, change it once
// more and fire a concurrent burst at the stale entry. The right answer to
// every request is whatever a fresh, uncached call to the mock model returns.
//
// The command edits the demo page in place and restores its exact original
// bytes afterwards (also on error and on Ctrl-C). It needs exclusive use of
// that file and of the stack while it runs. Everything is local: no network
// beyond the three stack URLs, no API keys.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"bifrost-eval-harness/internal/envinfo"
)

// Report is the results file.
type Report struct {
	Tool        string             `json:"tool"`
	Banner      string             `json:"banner"`
	Command     []string           `json:"command"`
	StartedAt   string             `json:"started_at"`
	FinishedAt  string             `json:"finished_at"`
	Settings    Settings           `json:"settings"`
	Stack       Stack              `json:"stack"`
	Environment envinfo.Env        `json:"environment"`
	Arms        []Arm              `json:"arms"`
	Workload    WorkloadInfo       `json:"workload"`
	Page        PageInfo           `json:"page"`
	Runs        []RunRecord        `json:"runs"`
	Median      map[string]Metrics `json:"median_of_runs"`
	Assumptions map[string]any     `json:"assumptions"`
	Limits      []string           `json:"limits"`
	Notes       []string           `json:"notes,omitempty"`
}

// Settings are the command's flags.
type Settings struct {
	Repeat          int     `json:"repeat"`
	Seed            int64   `json:"seed"`
	Settle          string  `json:"settle_wait"`
	WriteGap        string  `json:"write_gap_after_non_hit"`
	Burst           int     `json:"burst_size"`
	BasePrice       int     `json:"base_price"`
	ChangedPrice    int     `json:"changed_price"`
	BurstPrice      int     `json:"burst_price"`
	Model           string  `json:"model"`
	ArmsRequested   string  `json:"arms"`
	TTLArmTTL       string  `json:"plain_ttl_arm_ttl"`
	CostPerCall     float64 `json:"assumed_usd_per_model_call"`
	CostPerEmbed    float64 `json:"assumed_usd_per_embedding_call"`
	PlainURL        string  `json:"plain_url"`
	CompanionURL    string  `json:"companion_url"`
	MockURL         string  `json:"mock_url"`
	PagePath        string  `json:"page_path"`
	ConfigPath      string  `json:"bifrost_config_path"`
	ComposePath     string  `json:"compose_path"`
	PolicyPath      string  `json:"policy_path"`
	CacheKeyPattern string  `json:"cache_key_pattern"`
}

// Stack records the settings of the system under test, as read from the files
// that configure it (best effort; what cannot be read is said so).
type Stack struct {
	SemanticCache  map[string]any `json:"bifrost_semantic_cache_plugin_config,omitempty"`
	CompanionFlags []string       `json:"companion_flags_from_compose,omitempty"`
	PolicyGuard    any            `json:"policy_guard,omitempty"`
	Audits         map[string]any `json:"companion_audits_endpoint,omitempty"`
	Unavailable    []string       `json:"could_not_read,omitempty"`
}

// WorkloadInfo describes the request list.
type WorkloadInfo struct {
	PhaseCounts map[string]int `json:"requests_per_phase"`
	Seeds       []int64        `json:"seed_per_run"`
	FirstRun    []Item         `json:"first_run_items"`
	Similarity  []SimRow       `json:"mock_embedding_similarity"`
}

// SimRow is the mock embedding's cosine between two questions.
type SimRow struct {
	Label  string  `json:"label"`
	A      string  `json:"a"`
	B      string  `json:"b"`
	Cosine float64 `json:"cosine"`
}

// PageInfo records the demo page before and after.
type PageInfo struct {
	Path               string `json:"path"`
	OriginalBytes      int    `json:"original_bytes"`
	OriginalSHA256     string `json:"original_sha256"`
	BasePrice          int    `json:"base_price_on_page"`
	ByteIdenticalAfter bool   `json:"byte_identical_after_run"`
}

// RunRecord is one repetition.
type RunRecord struct {
	Index    int                `json:"run"`
	Seed     int64              `json:"seed"`
	ArmOrder []string           `json:"arm_order"`
	Arms     map[string]*ArmRun `json:"arms"`
}

var activePage *Page

func main() {
	var (
		plainURL    = flag.String("plain-url", "http://127.0.0.1:18090", "Bifrost, reached directly (arm plain)")
		compURL     = flag.String("companion-url", "http://127.0.0.1:8090", "freshness companion in front of the same Bifrost (arm companion)")
		mockURL     = flag.String("mock-url", "http://127.0.0.1:18080", "mock OpenAI model (ground truth and call counters)")
		mockKey     = flag.String("mock-api-key", "sk-mock-local", "key the mock model expects (a made-up local value, see tools/mock-openai/README.md; not a real credential)")
		pagePath    = flag.String("page", filepath.Join("..", "freshness-companion", "demo-page", "index.html"), "the demo page the companion verifies prices against (edited in place, restored byte for byte)")
		repeat      = flag.Int("repeat", 3, "how many times to repeat the whole experiment")
		seed        = flag.Int64("seed", 20261105, "workload seed; run i uses seed+i-1")
		settle      = flag.Duration("settle", 6*time.Second, "wait after each page change (must exceed the companion's -evidence-ttl)")
		burst       = flag.Int("burst", 16, "concurrent identical requests in the burst")
		basePrice   = flag.Int("base-price", 49, "price on the page at the start (must match the page)")
		changed     = flag.Int("changed-price", 59, "price after the first change")
		burstPrice  = flag.Int("burst-price", 69, "price after the second change")
		model       = flag.String("model", "openai/gpt-4o-mini", "model name sent in requests")
		armsFlag    = flag.String("arms", "plain,companion", "comma list of: plain, companion, plain-ttl")
		ttlArm      = flag.Duration("plain-ttl", 5*time.Second, "TTL sent by the plain-ttl arm (x-bf-cache-ttl)")
		costCall    = flag.Float64("assumed-usd-per-call", 0.002, "ASSUMPTION, not measured: price of one model call, for the cost illustration")
		costEmbed   = flag.Float64("assumed-usd-per-embedding", 0, "ASSUMPTION, not measured: price of one embedding call (0 = not counted)")
		outDir      = flag.String("out", "results", "output directory")
		label       = flag.String("label", "", "label used in the output file names")
		configPath  = flag.String("bifrost-config", filepath.Join("..", "docker", "bifrost-config.json"), "Bifrost config, read to record the cache settings")
		composePath = flag.String("compose", filepath.Join("..", "docker-compose.yml"), "docker-compose.yml, read to record the companion's flags")
		policyPath  = flag.String("policy", filepath.Join("..", "docker", "policy.json"), "companion policy, read to record the guard settings")
		containers  = flag.String("containers", "hackathon-workspace-bifrost-1,hackathon-workspace-freshness-companion-1,hackathon-workspace-mock-openai-1,hackathon-workspace-postgres-1,hackathon-workspace-demoserver-1", "container names to describe")
		writeGap    = flag.Duration("write-gap", 250*time.Millisecond, "pause (not timed) after every sequential response that was not served from the cache, so Bifrost's asynchronous cache write has landed before the next request")
		dryRun      = flag.Bool("dry-run", false, "print the workload for -seed and exit (no network, page untouched)")
	)
	renderFrom := flag.String("render", "", "re-render the Markdown of an existing results .json (next to it) and exit; nothing is measured")
	flag.Parse()
	if *renderFrom != "" {
		if err := rerender(*renderFrom); err != nil {
			fatal("%v", err)
		}
		return
	}
	cfg := WorkloadConfig{BasePrice: *basePrice, ChangedPrice: *changed, BurstPrice: *burstPrice, Burst: *burst}

	if *dryRun {
		items := BuildWorkload(*seed, cfg)
		fmt.Printf("seed %d: %d requests %v\n", *seed, len(items), PhaseCounts(items))
		for _, it := range items {
			tool := ""
			if it.Tool != "" {
				tool = "  tool=" + it.Tool
			}
			fmt.Printf("%-9s %-7s price=%-3d %-62s %s%s\n", it.ID, it.Kind, it.Price, it.Scenario, it.Question, tool)
		}
		return
	}
	if *repeat < 1 || *burst < 2 || *settle < time.Second {
		fatal("need -repeat >= 1, -burst >= 2 and -settle >= 1s")
	}

	arms, err := parseArms(*armsFlag, *plainURL, *compURL, *ttlArm)
	if err != nil {
		fatal("%v", err)
	}
	page, err := OpenPage(*pagePath)
	if err != nil {
		fatal("demo page: %v", err)
	}
	activePage = page
	if p, err := page.BasePrice(); err != nil || p != *basePrice {
		fatal("the page states price %d (err %v), -base-price is %d: put the page in its original state first", p, err, *basePrice)
	}
	logf("demo page %s: %d bytes, sha256 %s", *pagePath, page.OriginalSize(), page.OriginalSHA256())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Restore the page whatever happens below, including a panic.
	defer func() {
		if r := recover(); r != nil {
			restoreOrWarn(page)
			panic(r)
		}
	}()

	runner := &Runner{
		Client:  &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 64}},
		MockURL: strings.TrimRight(*mockURL, "/"), MockKey: *mockKey, CompanionURL: strings.TrimRight(*compURL, "/"),
		Model: *model, Page: page, Settle: *settle, WriteGap: *writeGap, Cfg: cfg, Log: logf,
	}
	if !hasArm(arms, "companion") {
		// the companion's counters are still read (zero deltas) when it is up, but not required
		if err := runner.getJSON(ctx, runner.CompanionURL+"/healthz", nil); err != nil {
			runner.CompanionURL = ""
		}
	}

	report := &Report{Tool: "impactbench", Command: append([]string{"go", "run", "./cmd/impactbench"}, os.Args[1:]...), StartedAt: time.Now().UTC().Format(time.RFC3339), Arms: arms,
		Banner: "MOCK MODEL + MOCK EMBEDDINGS: this benchmark shows how often each setup serves an answer a fresh call would not, on a small scripted workload. It is not a measurement of real model quality or real traffic."}
	report.Settings = Settings{Repeat: *repeat, Seed: *seed, Settle: settle.String(), WriteGap: writeGap.String(), Burst: *burst, BasePrice: *basePrice, ChangedPrice: *changed,
		BurstPrice: *burstPrice, Model: *model, ArmsRequested: *armsFlag, TTLArmTTL: ttlArm.String(), CostPerCall: *costCall, CostPerEmbed: *costEmbed,
		PlainURL: *plainURL, CompanionURL: *compURL, MockURL: *mockURL, PagePath: *pagePath, ConfigPath: *configPath, ComposePath: *composePath,
		PolicyPath: *policyPath, CacheKeyPattern: "impact-<arm>-<unix-time>-r<run>  (fresh per arm per run, different for every arm)"}

	if err := preflight(ctx, runner, arms); err != nil {
		restoreOrWarn(page)
		fatal("preflight: %v", err)
	}
	report.Stack = readStack(ctx, runner, *configPath, *composePath, *policyPath)
	report.Environment = envinfo.Collect(envinfo.Options{Containers: splitList(*containers), BifrostDir: filepath.Join("..", "bifrost")})
	report.Workload.Similarity = probeSimilarity(ctx, runner)

	cost := CostModel{PerChatCall: *costCall, PerEmbeddingCall: *costEmbed}
	stamp := time.Now().Unix()
	perArm := map[string][]Metrics{}
	for i := 0; i < *repeat; i++ {
		runSeed := *seed + int64(i)
		items := BuildWorkload(runSeed, cfg)
		if i == 0 {
			report.Workload.FirstRun = items
			report.Workload.PhaseCounts = PhaseCounts(items)
		}
		report.Workload.Seeds = append(report.Workload.Seeds, runSeed)
		truth, err := runner.GroundTruth(ctx, items)
		if err != nil {
			restoreOrWarn(page)
			fatal("%v", err)
		}
		order := rotate(arms, i)
		rec := RunRecord{Index: i + 1, Seed: runSeed, Arms: map[string]*ArmRun{}}
		for _, a := range order {
			rec.ArmOrder = append(rec.ArmOrder, a.Name)
		}
		logf("run %d/%d (seed %d), arm order %s", i+1, *repeat, runSeed, strings.Join(rec.ArmOrder, " -> "))
		for _, a := range order {
			key := fmt.Sprintf("impact-%s-%d-r%d", a.Name, stamp, i+1)
			ar, err := runner.RunArm(ctx, a, key, items, truth, cost)
			if err != nil {
				restoreOrWarn(page)
				fatal("run %d arm %s: %v", i+1, a.Name, err)
			}
			rec.Arms[a.Name] = ar
			perArm[a.Name] = append(perArm[a.Name], ar.Metrics)
			logf("[%s] done: %d requests, %d hits, %d model calls, stale %d, wrong_match %d, errors %d, bad/1000 %.1f",
				a.Name, int(ar.Metrics["requests"]), int(ar.Metrics["cache_hits"]), int(ar.Metrics["model_calls"]),
				int(ar.Metrics["stale"]), int(ar.Metrics["wrong_match"]), int(ar.Metrics["errors"]), ar.Metrics["bad_per_1000"])
			for _, n := range ar.Notes {
				report.Notes = append(report.Notes, fmt.Sprintf("run %d, arm %s: %s", i+1, a.Name, n))
			}
		}
		report.Runs = append(report.Runs, rec)
	}
	report.Median = map[string]Metrics{}
	for name, ms := range perArm {
		report.Median[name] = MedianMetrics(ms)
	}

	if err := page.Restore(); err != nil {
		fatal("could not restore the demo page: %v", err)
	}
	report.Page = PageInfo{Path: *pagePath, OriginalBytes: page.OriginalSize(), OriginalSHA256: page.OriginalSHA256(), BasePrice: *basePrice,
		ByteIdenticalAfter: page.Verify() == nil}
	report.Assumptions = map[string]any{
		"usd_per_model_call_ASSUMPTION_not_measured":     *costCall,
		"usd_per_embedding_call_ASSUMPTION_not_measured": *costEmbed,
	}
	report.Limits = limits(report)
	report.FinishedAt = time.Now().UTC().Format(time.RFC3339)

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fatal("%v", err)
	}
	name := "impactbench"
	if *label != "" {
		name += "-" + *label
	}
	base := filepath.Join(*outDir, fmt.Sprintf("%s-%s", name, time.Now().Format("20060102-150405")))
	raw, _ := json.MarshalIndent(report, "", "  ")
	if err := os.WriteFile(base+".json", raw, 0o644); err != nil {
		fatal("%v", err)
	}
	if err := os.WriteFile(base+".md", []byte(renderMarkdown(report)), 0o644); err != nil {
		fatal("%v", err)
	}
	logf("demo page restored, byte-identical: %v", report.Page.ByteIdenticalAfter)
	logf("wrote %s.json and %s.md", base, base)
}

func parseArms(list, plain, comp string, ttl time.Duration) ([]Arm, error) {
	var arms []Arm
	seen := map[string]bool{}
	for _, name := range splitList(list) {
		if seen[name] {
			return nil, fmt.Errorf("arm %q listed twice", name)
		}
		seen[name] = true
		switch name {
		case "plain":
			arms = append(arms, Arm{Name: "plain", Label: "Bifrost alone", URL: strings.TrimRight(plain, "/")})
		case "companion":
			arms = append(arms, Arm{Name: "companion", Label: "Bifrost + freshness companion", URL: strings.TrimRight(comp, "/")})
		case "plain-ttl":
			arms = append(arms, Arm{Name: "plain-ttl", Label: "Bifrost alone, short TTL " + ttl.String(), URL: strings.TrimRight(plain, "/"),
				Headers: map[string]string{"x-bf-cache-ttl": ttl.String()}})
		default:
			return nil, fmt.Errorf("unknown arm %q (want plain, companion, plain-ttl)", name)
		}
	}
	if len(arms) == 0 {
		return nil, fmt.Errorf("no arms")
	}
	return arms, nil
}

func hasArm(arms []Arm, name string) bool {
	for _, a := range arms {
		if a.Name == name {
			return true
		}
	}
	return false
}

// rotate moves the first n%len arms to the back, so the arm that goes first
// changes from run to run.
func rotate(arms []Arm, n int) []Arm {
	k := n % len(arms)
	return append(append([]Arm(nil), arms[k:]...), arms[:k]...)
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// preflight checks that every URL answers and that nothing else is using the
// model while we start (its counters are the measuring instrument).
func preflight(ctx context.Context, r *Runner, arms []Arm) error {
	checks := []struct{ name, url string }{{"mock model", r.MockURL + "/health"}}
	for _, a := range arms {
		path := "/health"
		if a.Name == "companion" {
			path = "/healthz"
		}
		checks = append(checks, struct{ name, url string }{"arm " + a.Name, a.URL + path})
	}
	for _, c := range checks {
		if err := r.getJSON(ctx, c.url, nil); err != nil {
			return fmt.Errorf("%s not healthy: %v", c.name, err)
		}
	}
	a, err := r.MockStats(ctx)
	if err != nil {
		return err
	}
	if err := sleep(ctx, 1500*time.Millisecond); err != nil {
		return err
	}
	b, err := r.MockStats(ctx)
	if err != nil {
		return err
	}
	if a != b {
		return fmt.Errorf("the mock model's counters moved while idle (%+v -> %+v): something else is using the stack; the call counts would be wrong", a, b)
	}
	return nil
}

// readStack records the settings that matter for judging the plain arm.
func readStack(ctx context.Context, r *Runner, configPath, composePath, policyPath string) Stack {
	var s Stack
	if raw, err := os.ReadFile(configPath); err != nil {
		s.Unavailable = append(s.Unavailable, "bifrost config: "+err.Error())
	} else {
		var cfg struct {
			Plugins []struct {
				Name   string         `json:"name"`
				Config map[string]any `json:"config"`
			} `json:"plugins"`
		}
		if json.Unmarshal(raw, &cfg) == nil {
			for _, p := range cfg.Plugins {
				if p.Name == "semantic_cache" {
					s.SemanticCache = p.Config
				}
			}
		}
		if s.SemanticCache == nil {
			s.Unavailable = append(s.Unavailable, "bifrost config has no semantic_cache plugin")
		}
	}
	if raw, err := os.ReadFile(composePath); err != nil {
		s.Unavailable = append(s.Unavailable, "docker-compose.yml: "+err.Error())
	} else {
		s.CompanionFlags = companionFlags(string(raw))
		if len(s.CompanionFlags) == 0 {
			s.Unavailable = append(s.Unavailable, "no freshness-companion flags found in docker-compose.yml")
		}
	}
	if raw, err := os.ReadFile(policyPath); err != nil {
		s.Unavailable = append(s.Unavailable, "policy: "+err.Error())
	} else {
		var p struct {
			Guard any `json:"guard"`
		}
		if json.Unmarshal(raw, &p) == nil {
			s.PolicyGuard = p.Guard
		}
	}
	if r.CompanionURL != "" {
		var a map[string]any
		if err := r.getJSON(ctx, r.CompanionURL+"/freshness/audits?limit=1", &a); err == nil {
			s.Audits = map[string]any{"enabled": a["enabled"], "compare": a["compare"]}
		}
	}
	return s
}

var flagLine = regexp.MustCompile(`^\s+-\s+"(-[^"]+)"\s*(#.*)?$`)

// companionFlags returns the "-flag=value" command items of the
// freshness-companion service of a compose file.
func companionFlags(compose string) []string {
	var out []string
	in := false
	for _, line := range strings.Split(compose, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "  freshness-companion:") {
			in = true
			continue
		}
		if in && regexp.MustCompile(`^  [A-Za-z0-9_-]+:`).MatchString(line) {
			break
		}
		if in {
			if m := flagLine.FindStringSubmatch(line); m != nil {
				out = append(out, m[1])
			}
		}
	}
	return out
}

// probeSimilarity asks the mock for the cosine of the question pairs the
// workload relies on, in the form the semantic cache embeds ("user: <lowercase>").
func probeSimilarity(ctx context.Context, r *Runner) []SimRow {
	var rows []SimRow
	ask := func(label, a, b string) {
		body, _ := json.Marshal(map[string]string{"a": "user: " + strings.ToLower(a), "b": "user: " + strings.ToLower(b)})
		status, _, resp, err := r.post2(ctx, r.MockURL+"/mock/similarity", body)
		if err != nil || status != 200 {
			return
		}
		var out struct {
			Cosine float64 `json:"cosine"`
		}
		if json.Unmarshal(resp, &out) == nil {
			rows = append(rows, SimRow{Label: label, A: a, B: b, Cosine: out.Cosine})
		}
	}
	for _, p := range lookAlikes {
		ask("look-alike pair: "+p.Name, p.A, p.B)
		if p.AParaphrase != "" {
			ask("same-meaning rewording: "+p.Name, p.A, p.AParaphrase)
		}
	}
	for _, q := range priceWarm[1:] {
		ask("price wording vs first price wording", priceWarm[0], q)
	}
	for _, q := range priceNew {
		ask("new price wording vs first price wording", priceWarm[0], q)
	}
	ask("benign: boiling water", benignWarm[0], benignAfter[0])
	ask("benign: boiling water, different wording", benignWarm[0], benignAfter[1])
	ask("benign: gold", benignWarm[1], benignAfter[2])
	return rows
}

func restoreOrWarn(p *Page) {
	if err := p.Restore(); err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: could not restore the demo page: %v\n", err)
	} else {
		logf("demo page restored")
	}
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, time.Now().Format("15:04:05")+" "+format+"\n", args...)
}

func fatal(format string, args ...any) {
	if activePage != nil {
		restoreOrWarn(activePage)
	}
	logf("fatal: "+format, args...)
	os.Exit(1)
}

// rerender rebuilds the Markdown file that belongs to a results .json.
func rerender(jsonPath string) error {
	raw, err := os.ReadFile(jsonPath)
	if err != nil {
		return err
	}
	var r Report
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("%s: %w", jsonPath, err)
	}
	r.Limits = limits(&r)
	out := strings.TrimSuffix(jsonPath, ".json") + ".md"
	if err := os.WriteFile(out, []byte(renderMarkdown(&r)), 0o644); err != nil {
		return err
	}
	logf("wrote %s", out)
	return nil
}
