package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Guided "Try it" scenarios. Each one sends the same requests down two roads:
//
//	plain     -> straight to Bifrost (the -bifrost base), its own cache key
//	protected -> through the freshness companion, its own cache key
//
// and then says, in plain words, what each road did. The keys are unique per
// run, so a run never touches anyone else's cache entries, and both keys are
// emptied again at the end.
//
// How a road is judged (see each run* function for the exact rule):
//
//	price changed   : the price in the answer is compared with the price on the
//	                  page now.
//	look-alike      : the second question needs a different answer than the first,
//	                  so the same answer coming back is a wrong match.
//	tool data       : an answer built from tool data must not be reused when the
//	                  tool data changed. The practice model ignores tool messages,
//	                  so only the reuse (or the guard) can be shown, not wrong text.
//	everyone at once: the burst is counted from the replies (answers, errors, how
//	                  many shared one refresh, which price they show).

const (
	plainLabel     = "Plain cache (no protection)"
	protectedLabel = "With the freshness companion"

	idPriceChanged = "price-changed"
	idLookAlike    = "look-alike"
	idToolData     = "live-tool-data"
	idEveryoneAt   = "everyone-at-once"

	defaultEvidenceWait = 6 * time.Second // the companion keeps its notes about a page for 5 s
	defaultBurstSize    = 10
	defaultRunLimit     = 2 * time.Minute
	stepLimit           = 25 * time.Second
)

var (
	errUnknownScenario = errors.New("unknown scenario")
	errScenarioBusy    = errors.New("another scenario is running right now; wait for it to finish")
)

// ScenarioInfo is what the page needs to draw one scenario button.
type ScenarioInfo struct {
	ID              string `json:"id"`
	Title           string `json:"title"`
	Blurb           string `json:"blurb"`
	NeedsDemoPage   bool   `json:"needs_demo_page"`
	Available       bool   `json:"available"`
	Unavailable     string `json:"unavailable,omitempty"`
	ExpectedSeconds int    `json:"expected_seconds"`
}

// Step is one request, with the parts of the reply that matter.
type Step struct {
	Label        string  `json:"label"`
	Answer       string  `json:"answer,omitempty"`
	Reused       bool    `json:"reused"` // a saved answer was served
	HitType      string  `json:"hit_type"`
	Similarity   string  `json:"similarity,omitempty"`
	Status       string  `json:"status,omitempty"` // X-Freshness-Status
	Action       string  `json:"action,omitempty"` // X-Freshness-Action
	Gate         string  `json:"gate,omitempty"`
	Guard        string  `json:"guard,omitempty"`
	Shared       bool    `json:"shared_refresh,omitempty"`
	CachedValue  string  `json:"cached_value,omitempty"`
	CurrentValue string  `json:"current_value,omitempty"`
	Error        string  `json:"error,omitempty"`
	HTTP         int     `json:"http,omitempty"`
	LatencyMS    float64 `json:"latency_ms"`
	usage        chatUsage
}

// Side is one road's outcome.
type Side struct {
	Label       string   `json:"label"`
	Verdict     string   `json:"verdict"` // right | wrong | inconclusive | error
	VerdictText string   `json:"verdict_text"`
	Answer      string   `json:"answer,omitempty"`
	Facts       []string `json:"facts"`
	Steps       []Step   `json:"steps"`
}

// ScenarioResult is the whole side-by-side picture.
type ScenarioResult struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	Narration    []string `json:"narration"`
	Plain        Side     `json:"plain"`
	Protected    Side     `json:"protected"`
	Notes        []string `json:"notes,omitempty"`
	PageRestored *bool    `json:"page_restored,omitempty"`
	TookMS       int64    `json:"took_ms"`
}

// RunSnapshot is what GET /api/scenario-runs/{id} returns.
type RunSnapshot struct {
	RunID       string          `json:"run_id"`
	Scenario    string          `json:"scenario"`
	State       string          `json:"state"` // running | done | error
	Phase       string          `json:"phase"`
	WaitUntilMS int64           `json:"wait_until_ms,omitempty"`
	NowMS       int64           `json:"now_ms"`
	Result      *ScenarioResult `json:"result,omitempty"`
	Error       string          `json:"error,omitempty"`
}

type scenarioRun struct {
	mu   sync.Mutex
	snap RunSnapshot
}

func (r *scenarioRun) set(f func(s *RunSnapshot)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f(&r.snap)
}

func (r *scenarioRun) snapshot() RunSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.snap
	s.NowMS = time.Now().UnixMilli()
	return s
}

type progressFn func(phase string, waitUntil time.Time)

type scenarioRunner struct {
	client        *http.Client
	companionBase string
	bifrostBase   string
	demoPage      string // empty when -demo-page was not given
	tally         *CacheTally

	evidenceWait time.Duration
	burstSize    int
	runLimit     time.Duration
	sleep        func(ctx context.Context, d time.Duration) error

	mu   sync.Mutex
	busy bool
	runs map[string]*scenarioRun
	ord  []string
}

func newScenarioRunner(client *http.Client, companionBase, bifrostBase, demoPage string, tally *CacheTally) *scenarioRunner {
	return &scenarioRunner{
		client: client, companionBase: companionBase, bifrostBase: bifrostBase, demoPage: demoPage, tally: tally,
		evidenceWait: defaultEvidenceWait, burstSize: defaultBurstSize, runLimit: defaultRunLimit,
		sleep: sleepCtx, runs: map[string]*scenarioRun{},
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ---- listing and starting ---------------------------------------------------

func (r *scenarioRunner) demoPageProblem() string {
	if r.demoPage == "" {
		return "Needs the demo pricing page. Start the dashboard with -demo-page <path to freshness-companion/demo-page/index.html> to turn this on."
	}
	if _, err := os.Stat(r.demoPage); err != nil {
		return "The demo pricing page was not found at " + r.demoPage + "."
	}
	return ""
}

func (r *scenarioRunner) list() []ScenarioInfo {
	pageNote := r.demoPageProblem()
	wait := int(r.evidenceWait/time.Second) + 3
	infos := []ScenarioInfo{
		{ID: idPriceChanged, Title: "A price changed", NeedsDemoPage: true, ExpectedSeconds: wait,
			Blurb: "Ask the price, change the price on the page, ask again."},
		{ID: idLookAlike, Title: "Two questions that look alike", ExpectedSeconds: 3,
			Blurb: "Reset a password vs. reset a username: similar words, different answers."},
		{ID: idToolData, Title: "An answer built from live tool data", ExpectedSeconds: 3,
			Blurb: "An order lookup says something new the second time. Should the old answer be reused?"},
		{ID: idEveryoneAt, Title: "Everyone asks at once", NeedsDemoPage: true, ExpectedSeconds: wait + 3,
			Blurb: "Ten people ask the same question the moment a saved answer goes out of date."},
	}
	for i := range infos {
		infos[i].Available = true
		if infos[i].NeedsDemoPage && pageNote != "" {
			infos[i].Available = false
			infos[i].Unavailable = pageNote
		}
	}
	return infos
}

func (r *scenarioRunner) info(id string) (ScenarioInfo, bool) {
	for _, i := range r.list() {
		if i.ID == id {
			return i, true
		}
	}
	return ScenarioInfo{}, false
}

// start checks the scenario can run, claims the single "busy" slot (only one
// scenario at a time: they share one page and must not trample each other) and
// runs it in the background.
func (r *scenarioRunner) start(id string) (string, error) {
	info, ok := r.info(id)
	if !ok {
		return "", errUnknownScenario
	}
	if !info.Available {
		return "", errors.New(info.Unavailable)
	}
	r.mu.Lock()
	if r.busy {
		r.mu.Unlock()
		return "", errScenarioBusy
	}
	r.busy = true
	runID := newRunID()
	run := &scenarioRun{snap: RunSnapshot{RunID: runID, Scenario: id, State: "running", Phase: "Starting"}}
	r.runs[runID] = run
	r.ord = append(r.ord, runID)
	for len(r.ord) > 20 {
		delete(r.runs, r.ord[0])
		r.ord = r.ord[1:]
	}
	r.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), r.runLimit)
		defer cancel()
		res, err := r.execute(ctx, id, func(phase string, wait time.Time) {
			run.set(func(s *RunSnapshot) {
				s.Phase = phase
				s.WaitUntilMS = 0
				if !wait.IsZero() {
					s.WaitUntilMS = wait.UnixMilli()
				}
			})
		})
		run.set(func(s *RunSnapshot) {
			s.WaitUntilMS = 0
			if err != nil {
				s.State, s.Error, s.Phase = "error", err.Error(), "Stopped"
				return
			}
			s.State, s.Result, s.Phase = "done", res, "Done"
		})
		r.mu.Lock()
		r.busy = false
		r.mu.Unlock()
	}()
	return runID, nil
}

func (r *scenarioRunner) get(runID string) (RunSnapshot, bool) {
	r.mu.Lock()
	run := r.runs[runID]
	r.mu.Unlock()
	if run == nil {
		return RunSnapshot{}, false
	}
	return run.snapshot(), true
}

func newRunID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// execute runs one scenario to the end and returns the side-by-side result. A
// non-nil error means the run could not even be set up (for example the page
// could not be read); problems with one road are reported inside the result.
func (r *scenarioRunner) execute(ctx context.Context, id string, prog progressFn) (res *ScenarioResult, err error) {
	started := time.Now()
	defer func() {
		if p := recover(); p != nil {
			res, err = nil, fmt.Errorf("the scenario hit an unexpected problem: %v", p)
		}
		if res != nil {
			res.TookMS = time.Since(started).Milliseconds()
		}
		if err != nil && errors.Is(err, context.DeadlineExceeded) {
			err = errors.New("the scenario took too long and was stopped. Any change to the demo page was put back")
		}
	}()
	if prog == nil {
		prog = func(string, time.Time) {}
	}
	switch id {
	case idPriceChanged:
		return r.withPage(func(ed *pageEditor) (*ScenarioResult, error) { return r.runPriceChanged(ctx, ed, prog) })
	case idLookAlike:
		return r.runLookAlike(ctx, prog)
	case idToolData:
		return r.runToolData(ctx, prog)
	case idEveryoneAt:
		return r.withPage(func(ed *pageEditor) (*ScenarioResult, error) { return r.runEveryoneAtOnce(ctx, ed, prog) })
	}
	return nil, errUnknownScenario
}

// withPage opens the demo page, runs fn, and ALWAYS puts the original bytes
// back afterwards: on success, on error, on timeout and on panic.
func (r *scenarioRunner) withPage(fn func(ed *pageEditor) (*ScenarioResult, error)) (res *ScenarioResult, err error) {
	ed, err := openPageEditor(r.demoPage)
	if err != nil {
		return nil, err
	}
	defer func() {
		rerr := ed.restore()
		ok := rerr == nil
		if res != nil {
			res.PageRestored = &ok
			if ok {
				res.Notes = append(res.Notes, fmt.Sprintf("The demo page is back to its original $%d. The companion may remember the changed price for a few more seconds.", ed.price))
			} else {
				res.Notes = append(res.Notes, "WARNING: "+rerr.Error())
			}
		}
		if rerr != nil && err == nil {
			err = rerr
		} else if rerr != nil {
			err = fmt.Errorf("%v. Also: %v", err, rerr)
		}
	}()
	return fn(ed)
}

// ---- sending requests ------------------------------------------------------

func uniqueKey(id string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("try-it-%s-%d-%s", id, time.Now().Unix(), hex.EncodeToString(b))
}

func textMessage(role, content string) map[string]any {
	return map[string]any{"role": role, "content": content}
}

func priceMessages(price int) []any {
	return []any{
		textMessage("system", fmt.Sprintf("You answer questions about Acme.\nContext:\n- The Acme Pro plan costs $%d per month.\nAnswer in one sentence and state the monthly price.", price)),
		textMessage("user", "How much does the Acme Pro plan cost per month?"),
	}
}

func toolMessages(question, toolResult string) []any {
	return []any{
		textMessage("user", question),
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{
			"id": "call_try_1", "type": "function",
			"function": map[string]any{"name": "lookup_order", "arguments": "{\"order\":1234}"},
		}}},
		map[string]any{"role": "tool", "tool_call_id": "call_try_1", "content": toolResult},
	}
}

type chatAnswer struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage chatUsage `json:"usage"`
}

// ask sends one chat request to base with its own cache key. When record is
// true the reply also goes into the dashboard's tally (only the companion
// road does, since the tally describes what the companion served).
func (r *scenarioRunner) ask(ctx context.Context, base, key, label string, messages []any, record bool) Step {
	st := Step{Label: label, HitType: "error"}
	body, _ := json.Marshal(map[string]any{"model": "openai/gpt-4o-mini", "messages": messages})
	rctx, cancel := context.WithTimeout(ctx, stepLimit)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, base+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		st.Error = err.Error()
		return st
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-bf-cache-key", key)
	start := time.Now()
	resp, err := r.client.Do(req)
	if err != nil {
		st.Error = plainNetError(base, err)
		if record {
			r.record(st)
		}
		return st
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	st.LatencyMS = float64(time.Since(start).Microseconds()) / 1000
	st.HTTP = resp.StatusCode
	h := resp.Header
	st.Status, st.Action = h.Get("X-Freshness-Status"), h.Get("X-Freshness-Action")
	st.Gate, st.Guard = h.Get("X-Freshness-Gate"), h.Get("X-Freshness-Guard")
	st.Shared = h.Get("X-Freshness-Shared-Refresh") == "true"
	st.CachedValue, st.CurrentValue = h.Get("X-Freshness-Cached-Value"), h.Get("X-Freshness-Current-Value")
	st.Similarity = h.Get("X-Bifrost-Cache-Similarity")
	if f, err := strconv.ParseFloat(st.Similarity, 64); err == nil {
		st.Similarity = fmt.Sprintf("%.2f", f)
	}
	if resp.StatusCode != http.StatusOK {
		st.Error = fmt.Sprintf("the server answered with an error (HTTP %d)", resp.StatusCode)
		if e := h.Get("X-Freshness-Error"); e != "" {
			st.Error += ": " + e
		}
		if record {
			r.record(st)
		}
		return st
	}
	var ans chatAnswer
	_ = json.Unmarshal(raw, &ans)
	if len(ans.Choices) > 0 {
		st.Answer = strings.TrimSpace(ans.Choices[0].Message.Content)
	}
	st.usage = ans.Usage
	hit := h.Get("X-Bifrost-Cache-Hit") == "true"
	switch {
	case !hit:
		st.HitType = "miss"
	case h.Get("X-Bifrost-Cache-Hit-Type") == "semantic":
		st.HitType = "semantic"
	default:
		st.HitType = "direct"
	}
	// A hit whose answer was thrown away and re-asked (refreshed, corrected) or
	// replaced after a failed pre-serve check (blocked) did NOT serve a saved answer.
	st.Reused = hit && st.Gate != "blocked" && st.Action != "refreshed" && st.Action != "corrected" && st.Action != "uncached"
	if record {
		r.record(st)
	}
	return st
}

func (r *scenarioRunner) record(st Step) {
	if r.tally == nil {
		return
	}
	ht := st.HitType
	if st.HTTP != 0 && st.HTTP != http.StatusOK {
		ht = "error"
	}
	if ht == "direct" || ht == "semantic" {
		if !st.Reused {
			ht = "miss" // served fresh after a refresh or a block: not a saved answer
		}
	}
	ev := TrafficEvent{
		Time: time.Now().UTC(), Question: "Try it: " + st.Label, Kind: "try-it", HitType: ht, Similarity: st.Similarity,
		Gate: st.Gate, SharedRefresh: st.Shared, Guard: st.Guard, TotalTokens: st.usage.TotalTokens, LatencyMS: st.LatencyMS,
		Note: st.Error,
	}
	r.tally.record(ev, st.usage, st.Reused)
}

func plainNetError(base string, err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "no answer from " + base + " in time"
	case errors.Is(err, context.Canceled):
		return "the request was cancelled"
	}
	return "could not reach " + base + ". Is it running?"
}

// clearCacheKey empties one cache key through Bifrost's clear-by-key endpoint.
func clearCacheKey(ctx context.Context, client *http.Client, bifrostBase, key string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, bifrostBase+"/api/cache/clear-by-key/"+url.PathEscape(key), nil)
	if err != nil {
		return err
	}
	if auth := os.Getenv("BIFROST_ADMIN_AUTH"); auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("Bifrost answered HTTP %d", resp.StatusCode)
	}
	return nil
}

// cleanup empties this run's two cache keys. It uses its own short deadline so
// it still runs when the scenario's own deadline has passed.
func (r *scenarioRunner) cleanup(res *ScenarioResult, keys ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	failed := 0
	for _, k := range keys {
		if err := clearCacheKey(ctx, r.client, r.bifrostBase, k); err != nil {
			failed++
		}
	}
	if failed > 0 && res != nil {
		res.Notes = append(res.Notes, "Could not empty this demo's own cache entries afterwards; they are harmless and expire on their own.")
	}
}

// ---- judging helpers ---------------------------------------------------------

var dollarRE = regexp.MustCompile(`\$\s?([0-9][0-9,]*)`)

// priceIn returns the first $ amount in text (0, false if none).
func priceIn(text string) (int, bool) {
	m := dollarRE.FindStringSubmatch(text)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.ReplaceAll(m[1], ",", ""))
	return n, err == nil
}

func quote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 220 {
		s = s[:217] + "..."
	}
	return s
}

func failedSides(label string, steps ...Step) (Side, bool) {
	for _, st := range steps {
		if st.Error != "" {
			return Side{Label: label, Verdict: "error", VerdictText: "Did not finish: " + st.Error + ".",
				Facts: []string{}, Steps: steps}, true
		}
	}
	return Side{}, false
}

func anyError(steps ...Step) bool {
	for _, s := range steps {
		if s.Error != "" {
			return true
		}
	}
	return false
}

func errorResult(id, title string, plain, prot []Step, notes ...string) *ScenarioResult {
	res := &ScenarioResult{ID: id, Title: title, Notes: notes}
	var ok bool
	if res.Plain, ok = failedSides(plainLabel, plain...); !ok {
		res.Plain = Side{Label: plainLabel, Verdict: "inconclusive", VerdictText: "Stopped early because the other road failed.", Facts: []string{}, Steps: plain}
	}
	if res.Protected, ok = failedSides(protectedLabel, prot...); !ok {
		res.Protected = Side{Label: protectedLabel, Verdict: "inconclusive", VerdictText: "Stopped early because the other road failed.", Facts: []string{}, Steps: prot}
	}
	res.Narration = []string{"The scenario could not finish, so there is nothing to compare. The reason is shown under each side."}
	return res
}

func reuseWord(st Step) string {
	if st.Reused {
		return "served a saved answer"
	}
	return "asked the model"
}

// ---- scenario (a): a price changed ----------------------------------------

func (r *scenarioRunner) runPriceChanged(ctx context.Context, ed *pageEditor, prog progressFn) (res *ScenarioResult, err error) {
	title := "A price changed"
	key := uniqueKey("price")
	plainKey, protKey := key+"-plain", key+"-prot"
	defer func() { r.cleanup(res, plainKey, protKey) }()

	oldP := ed.price
	newP := oldP + 10
	prog(fmt.Sprintf("Asking what the plan costs while the page says $%d", oldP), time.Time{})
	var plain, prot []Step
	plain = append(plain, r.ask(ctx, r.bifrostBase, plainKey, "First ask", priceMessages(oldP), false))
	prot = append(prot, r.ask(ctx, r.companionBase, protKey, "First ask", priceMessages(oldP), true))
	plain = append(plain, r.ask(ctx, r.bifrostBase, plainKey, "Second ask, same page", priceMessages(oldP), false))
	prot = append(prot, r.ask(ctx, r.companionBase, protKey, "Second ask, same page", priceMessages(oldP), true))
	if anyError(plain...) || anyError(prot...) {
		return errorResult(idPriceChanged, title, plain, prot), nil
	}

	if err = ed.setPrice(newP); err != nil {
		return nil, fmt.Errorf("could not change the demo page: %v", err)
	}
	until := time.Now().Add(r.evidenceWait)
	prog(fmt.Sprintf("The page now says $%d. Waiting for the companion's notes about the page to expire", newP), until)
	if err = r.sleep(ctx, r.evidenceWait); err != nil {
		return nil, err
	}
	prog("Asking again after the change", time.Time{})
	plain = append(plain, r.ask(ctx, r.bifrostBase, plainKey, "After the price changed", priceMessages(newP), false))
	prot = append(prot, r.ask(ctx, r.companionBase, protKey, "After the price changed", priceMessages(newP), true))

	res = &ScenarioResult{ID: idPriceChanged, Title: title}
	res.Plain = judgePriceSide(plainLabel, plain, oldP, newP, false)
	res.Protected = judgePriceSide(protectedLabel, prot, oldP, newP, true)
	res.Narration = []string{
		fmt.Sprintf("We asked what the Pro plan costs while the pricing page said $%d, so both caches saved that answer. Then we changed the page to $%d and asked the same question again.", oldP, newP),
		fmt.Sprintf("The plain cache has no way to know the page changed. The companion looks at the page before it hands back an old price, and it %s.", ifElse(res.Protected.Verdict == "right", "noticed the change", "was supposed to notice the change, but did not this time")),
	}
	return res, nil
}

func ifElse(c bool, a, b string) string {
	if c {
		return a
	}
	return b
}

func judgePriceSide(label string, steps []Step, oldP, newP int, protected bool) Side {
	s := Side{Label: label, Steps: steps, Facts: []string{}}
	last := steps[len(steps)-1]
	s.Answer = last.Answer
	got, hasPrice := priceIn(last.Answer)
	s.Facts = append(s.Facts, fmt.Sprintf("Price on the page now: $%d (it was $%d)", newP, oldP))
	if hasPrice {
		s.Facts = append(s.Facts, fmt.Sprintf("Price in the answer: $%d", got))
	}
	s.Facts = append(s.Facts, "This time it "+reuseWord(last))
	if protected {
		if last.Status != "" {
			f := "Freshness check: " + plainStatus(last.Status)
			if last.CachedValue != "" || last.CurrentValue != "" {
				f += fmt.Sprintf(" (saved %s, page %s)", dash(last.CachedValue), dash(last.CurrentValue))
			}
			s.Facts = append(s.Facts, f)
		}
		if last.Action != "" {
			s.Facts = append(s.Facts, "What it did: "+plainAction(last.Action))
		}
	}
	switch {
	case !hasPrice:
		s.Verdict, s.VerdictText = "inconclusive", "The answer did not state a price, so it cannot be compared with the page."
	case got == newP && protected && (last.Status == "stale" || last.Action == "refreshed" || last.Action == "corrected"):
		s.Verdict, s.VerdictText = "right", fmt.Sprintf("Right. It saw the page now says $%d, threw away the old saved answer and asked again.", newP)
	case got == newP && last.Reused:
		s.Verdict, s.VerdictText = "right", fmt.Sprintf("Right: the answer shows $%d.", newP)
	case got == newP:
		s.Verdict, s.VerdictText = "inconclusive", fmt.Sprintf("It showed $%d, but only because it asked the model again, not because it noticed anything. There was no old answer to catch.", newP)
	case got == oldP && protected:
		s.Verdict, s.VerdictText = "wrong", fmt.Sprintf("Wrong: it still said $%d while the page says $%d. Its notes about the page may not have expired yet (it keeps them for about 5 seconds).", oldP, newP)
	case got == oldP:
		s.Verdict, s.VerdictText = "wrong", fmt.Sprintf("Wrong: it handed back the saved $%d answer while the page says $%d.", oldP, newP)
	default:
		s.Verdict, s.VerdictText = "inconclusive", fmt.Sprintf("The answer shows $%d, which is neither price.", got)
	}
	return s
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func plainStatus(s string) string {
	switch s {
	case "confirmed":
		return "the saved answer was still correct"
	case "stale":
		return "the saved answer was out of date"
	case "unable_to_verify":
		return "could not be checked"
	case "unknown":
		return "not checked"
	case "contradicted":
		return "the answer looked right but was not"
	}
	return s
}

func plainAction(a string) string {
	switch a {
	case "served":
		return "served the saved answer"
	case "served_with_warning":
		return "served the saved answer with a warning"
	case "refreshed":
		return "dropped the saved answer and asked the model again"
	case "refresh_failed":
		return "tried to refresh but could not"
	case "corrected":
		return "fixed a wrong answer"
	case "uncached":
		return "removed a wrong answer from the cache"
	}
	return a
}

// ---- scenario (b): two questions that look alike -----------------------------

func (r *scenarioRunner) runLookAlike(ctx context.Context, prog progressFn) (res *ScenarioResult, err error) {
	title := "Two questions that look alike"
	key := uniqueKey("lookalike")
	plainKey, protKey := key+"-plain", key+"-prot"
	defer func() { r.cleanup(res, plainKey, protKey) }()
	q1 := "How do I reset my Acme account password from the mobile app?"
	q2 := "How do I reset my Acme account username from the mobile app?"

	prog("Asking about resetting a password", time.Time{})
	plain := []Step{r.ask(ctx, r.bifrostBase, plainKey, "Question 1: reset a password", []any{textMessage("user", q1)}, false)}
	prot := []Step{r.ask(ctx, r.companionBase, protKey, "Question 1: reset a password", []any{textMessage("user", q1)}, true)}
	prog("Asking about resetting a username", time.Time{})
	plain = append(plain, r.ask(ctx, r.bifrostBase, plainKey, "Question 2: reset a username", []any{textMessage("user", q2)}, false))
	prot = append(prot, r.ask(ctx, r.companionBase, protKey, "Question 2: reset a username", []any{textMessage("user", q2)}, true))
	if anyError(plain...) || anyError(prot...) {
		return errorResult(idLookAlike, title, plain, prot), nil
	}
	res = &ScenarioResult{ID: idLookAlike, Title: title,
		Plain:     judgeLookAlike(plainLabel, plain, false),
		Protected: judgeLookAlike(protectedLabel, prot, true)}
	res.Narration = []string{
		"We asked how to reset a password, then how to reset a username. The words are almost the same, but the right answers are different.",
		"A plain cache goes by how similar the words look. The companion double-checks a close call by asking the model first, like reading the label before handing over the parcel.",
	}
	return res, nil
}

func judgeLookAlike(label string, steps []Step, protected bool) Side {
	s := Side{Label: label, Steps: steps, Facts: []string{}}
	first, second := steps[0], steps[1]
	s.Answer = second.Answer
	same := first.Answer != "" && first.Answer == second.Answer
	s.Facts = append(s.Facts, "Question 2 "+reuseWord(second))
	if second.Similarity != "" {
		s.Facts = append(s.Facts, "Closeness to question 1: "+second.Similarity+" (1.00 means identical)")
	}
	if protected && second.Gate != "" {
		s.Facts = append(s.Facts, "Check before serving: "+plainGate(second.Gate))
	}
	switch {
	case same && second.Reused:
		s.Verdict, s.VerdictText = "wrong", "Wrong: it answered the username question with the password answer, because the two questions look so alike."
	case same:
		s.Verdict, s.VerdictText = "inconclusive", "Both questions got the same answer, but it was not a reused one, so this is not a cache mistake."
	case protected && second.Gate == "blocked":
		s.Verdict, s.VerdictText = "right", "Right. It spotted that the saved answer was for a different question, did not serve it, and gave the right answer instead."
	case second.Reused:
		s.Verdict, s.VerdictText = "right", "The saved answer it reused is a different answer, so no wrong match here."
	default:
		s.Verdict, s.VerdictText = "right", "Right: question 2 got its own answer instead of question 1's."
	}
	return s
}

func plainGate(g string) string {
	switch g {
	case "blocked":
		return "the model disagreed with the saved answer, so it was not served"
	case "verified":
		return "the model agreed, so the saved answer was served"
	case "trusted":
		return "trusted from an earlier check"
	case "unverified":
		return "could not be checked, so the saved answer was served"
	}
	return g
}

// ---- scenario (c): an answer built from live tool data ----------------------------

func (r *scenarioRunner) runToolData(ctx context.Context, prog progressFn) (res *ScenarioResult, err error) {
	title := "An answer built from live tool data"
	key := uniqueKey("tool")
	plainKey, protKey := key+"-plain", key+"-prot"
	defer func() { r.cleanup(res, plainKey, protKey) }()
	q := "What does the order lookup tool say about order 1234?"
	t1, t2 := "Order 1234: shipped", "Order 1234: delayed, now arriving next week"

	prog("Asking once, with the tool saying the order shipped", time.Time{})
	plain := []Step{r.ask(ctx, r.bifrostBase, plainKey, "First ask: tool says shipped", toolMessages(q, t1), false)}
	prot := []Step{r.ask(ctx, r.companionBase, protKey, "First ask: tool says shipped", toolMessages(q, t1), true)}
	prog("Asking again, now the tool says the order is delayed", time.Time{})
	plain = append(plain, r.ask(ctx, r.bifrostBase, plainKey, "Second ask: tool says delayed", toolMessages(q, t2), false))
	prot = append(prot, r.ask(ctx, r.companionBase, protKey, "Second ask: tool says delayed", toolMessages(q, t2), true))
	if anyError(plain...) || anyError(prot...) {
		return errorResult(idToolData, title, plain, prot), nil
	}
	res = &ScenarioResult{ID: idToolData, Title: title,
		Plain:     judgeToolSide(plainLabel, plain, false),
		Protected: judgeToolSide(protectedLabel, prot, true)}
	res.Narration = []string{
		"We asked about an order, with a tool reporting \"shipped\". Then we asked the same thing again, but now the tool reports \"delayed\". The answer to the second question depends on that new tool result.",
		"A saved answer built from yesterday's tool result is wrong today. The companion keeps any request that carries tool data out of the cache on purpose.",
	}
	res.Notes = []string{"The practice model does not read tool results, so both answers read the same. What this shows is whether an old answer was reused, not wrong text. A real model would have said something different the second time."}
	return res, nil
}

func judgeToolSide(label string, steps []Step, protected bool) Side {
	s := Side{Label: label, Steps: steps, Facts: []string{}}
	second := steps[1]
	s.Answer = second.Answer
	s.Facts = append(s.Facts, "Second ask "+reuseWord(second))
	if protected {
		s.Facts = append(s.Facts, "Kept out of the cache on purpose: "+ifElse(second.Guard != "", "yes ("+second.Guard+")", "no"))
	}
	switch {
	case second.Reused:
		s.Verdict, s.VerdictText = "wrong", "Wrong: it reused the first answer even though the tool data changed. With a real model the answer could still say \"shipped\" for a delayed order."
	case protected && second.Guard != "":
		s.Verdict, s.VerdictText = "right", "Right. It saw that the request carried live tool data, did not reuse a saved answer, and asked the model again."
	case protected:
		s.Verdict, s.VerdictText = "inconclusive", "It asked the model again, but the companion did not report keeping the request out of the cache."
	default:
		s.Verdict, s.VerdictText = "right", "It asked the model again. Bifrost's own rules already skip requests like this one, so there was no old answer to reuse."
	}
	return s
}

// ---- scenario (d): everyone asks at once -----------------------------------------

type burstTally struct {
	Asked, Answered, Errors, Shared, Refreshers, OldPrice, NewPrice, Reused int
}

func tallyBurst(steps []Step, oldP, newP int) burstTally {
	b := burstTally{Asked: len(steps)}
	for _, st := range steps {
		if st.Error != "" {
			b.Errors++
			continue
		}
		b.Answered++
		if st.Shared {
			b.Shared++
		} else if st.Action == "refreshed" || st.Action == "corrected" {
			b.Refreshers++
		}
		if st.Reused {
			b.Reused++
		}
		if p, ok := priceIn(st.Answer); ok {
			switch p {
			case oldP:
				b.OldPrice++
			case newP:
				b.NewPrice++
			}
		}
	}
	return b
}

func (r *scenarioRunner) burst(ctx context.Context, base, key string, record bool, price int) []Step {
	n := r.burstSize
	steps := make([]Step, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			steps[i] = r.ask(ctx, base, key, fmt.Sprintf("Person %d", i+1), priceMessages(price), record)
		}(i)
	}
	close(start)
	wg.Wait()
	return steps
}

func (r *scenarioRunner) runEveryoneAtOnce(ctx context.Context, ed *pageEditor, prog progressFn) (res *ScenarioResult, err error) {
	title := "Everyone asks at once"
	key := uniqueKey("burst")
	plainKey, protKey := key+"-plain", key+"-prot"
	defer func() { r.cleanup(res, plainKey, protKey) }()
	oldP := ed.price
	newP := oldP + 10

	prog(fmt.Sprintf("Saving an answer while the page says $%d", oldP), time.Time{})
	var plainSetup, protSetup []Step
	for _, label := range []string{"Setup: first ask", "Setup: second ask"} {
		plainSetup = append(plainSetup, r.ask(ctx, r.bifrostBase, plainKey, label, priceMessages(oldP), false))
		protSetup = append(protSetup, r.ask(ctx, r.companionBase, protKey, label, priceMessages(oldP), true))
	}
	if anyError(plainSetup...) || anyError(protSetup...) {
		return errorResult(idEveryoneAt, title, plainSetup, protSetup), nil
	}
	if err = ed.setPrice(newP); err != nil {
		return nil, fmt.Errorf("could not change the demo page: %v", err)
	}
	until := time.Now().Add(r.evidenceWait)
	prog(fmt.Sprintf("The page now says $%d. Waiting for the companion's notes about the page to expire", newP), until)
	if err = r.sleep(ctx, r.evidenceWait); err != nil {
		return nil, err
	}
	n := r.burstSize
	prog(fmt.Sprintf("%d people ask at the same moment", n), time.Time{})
	plainBurst := r.burst(ctx, r.bifrostBase, plainKey, false, newP)
	protBurst := r.burst(ctx, r.companionBase, protKey, true, newP)

	res = &ScenarioResult{ID: idEveryoneAt, Title: title}
	res.Plain = judgeBurstSide(plainLabel, append(plainSetup, plainBurst...), plainBurst, oldP, newP, false)
	res.Protected = judgeBurstSide(protectedLabel, append(protSetup, protBurst...), protBurst, oldP, newP, true)
	res.Narration = []string{
		fmt.Sprintf("A saved answer says $%d, but the page now says $%d. Then %d people ask the same question in the same instant.", oldP, newP, n),
		"A careless fix, where everyone throws the old answer away and asks the model, would send a pile of identical calls and let them trample each other. The companion lets one request do the refresh and everyone else waits for that one result.",
	}
	res.Notes = append(res.Notes, "Model calls are counted from the replies (answers that were freshly made and not shared), not read from the model itself.")
	return res, nil
}

func judgeBurstSide(label string, all, burst []Step, oldP, newP int, protected bool) Side {
	s := Side{Label: label, Steps: all, Facts: []string{}}
	b := tallyBurst(burst, oldP, newP)
	for _, st := range burst {
		if st.Error == "" {
			s.Answer = st.Answer
			break
		}
	}
	s.Facts = append(s.Facts,
		fmt.Sprintf("%d people asked at once; %d got an answer, %d got an error", b.Asked, b.Answered, b.Errors),
		fmt.Sprintf("Answers showing the new price ($%d): %d. Showing the old price ($%d): %d", newP, b.NewPrice, oldP, b.OldPrice))
	if protected {
		s.Facts = append(s.Facts,
			fmt.Sprintf("Requests that shared one refresh: %d", b.Shared),
			fmt.Sprintf("Fresh model calls made for the refresh (counted from the replies): %d", b.Refreshers))
	} else {
		s.Facts = append(s.Facts, fmt.Sprintf("Answers served from the saved copy: %d", b.Reused))
	}
	switch {
	case b.Answered == 0:
		s.Verdict, s.VerdictText = "error", "Nobody got an answer."
	case b.OldPrice > 0 && !protected:
		s.Verdict, s.VerdictText = "wrong", fmt.Sprintf("Wrong: %d of %d people were handed the old $%d answer.", b.OldPrice, b.Asked, oldP)
	case b.OldPrice > 0:
		s.Verdict, s.VerdictText = "wrong", fmt.Sprintf("%d of %d people still got the old price. The companion's notes about the page may not have expired yet.", b.OldPrice, b.Asked)
	case b.Errors > 0:
		s.Verdict, s.VerdictText = "wrong", fmt.Sprintf("%d of %d people got an error instead of an answer.", b.Errors, b.Asked)
	case protected && b.NewPrice == b.Asked:
		s.Verdict, s.VerdictText = "right", fmt.Sprintf("Right. All %d got the new $%d, with %d fresh model call%s for the refresh and %d request%s sharing it.", b.Asked, newP, b.Refreshers, plural1(b.Refreshers), b.Shared, plural1(b.Shared))
	case b.NewPrice == b.Asked:
		s.Verdict, s.VerdictText = "inconclusive", "Everyone got the new price, but not from a saved answer, so nothing old was served."
	default:
		s.Verdict, s.VerdictText = "inconclusive", "Some answers did not state a price, so they cannot be compared."
	}
	return s
}

func plural1(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
