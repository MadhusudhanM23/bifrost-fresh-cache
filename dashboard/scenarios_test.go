package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The demo page's real shape, including the em dash (3 bytes in UTF-8).
const fakePage = "<!doctype html>\n<html><body>\n<main>\n" +
	"  <!-- edit only the number: $49 is the old one -->\n" +
	"  <p id=\"pro-plan-price\" class=\"price\" data-fact=\"x\">Acme Pro plan \xe2\x80\x94 $49 per month</p>\n" +
	"  <p class=\"note\">Last updated <time>2 November 2026</time>.</p>\n</main>\n</body></html>\n"

func writeFakePage(t *testing.T) (string, []byte) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "index.html")
	if err := os.WriteFile(p, []byte(fakePage), 0o644); err != nil {
		t.Fatal(err)
	}
	return p, []byte(fakePage)
}

func mustEqualFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("page bytes differ from the original:\n got %q\nwant %q", got, want)
	}
}

// ---- page editor --------------------------------------------------------------

func TestPageEditorChangesOnlyThePriceDigitsAndRestoresExactBytes(t *testing.T) {
	path, orig := writeFakePage(t)
	ed, err := openPageEditor(path)
	if err != nil {
		t.Fatal(err)
	}
	if ed.price != 49 {
		t.Fatalf("price = %d, want 49 (the $49 in the comment must be ignored)", ed.price)
	}
	if err := ed.setPrice(59); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if len(got) != len(orig) {
		t.Fatalf("same-length edit changed the size: %d vs %d", len(got), len(orig))
	}
	diff := 0
	for i := range got {
		if got[i] != orig[i] {
			diff++
		}
	}
	if diff != 1 || !strings.Contains(string(got), "\xe2\x80\x94 $59 per month") || !strings.Contains(string(got), "is the old one") {
		t.Fatalf("expected exactly one changed byte (4 to 5), got %d changed; page: %q", diff, got)
	}
	// A longer number must not leave stale bytes behind, and restoring must shrink the file back.
	if err := ed.setPrice(1000); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readString(t, path), "$1000 per month") {
		t.Fatal("longer price not written")
	}
	if err := ed.restore(); err != nil {
		t.Fatal(err)
	}
	mustEqualFile(t, path, orig)
	if _, err := os.Stat(ed.backup); !os.IsNotExist(err) {
		t.Fatalf("the safety copy should be removed after a good restore, stat err = %v", err)
	}
}

func readString(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPageEditorRefusesAPageItCannotEditSafely(t *testing.T) {
	dir := t.TempDir()
	noLine := filepath.Join(dir, "a.html")
	_ = os.WriteFile(noLine, []byte("<p>no price here</p>"), 0o644)
	if _, err := openPageEditor(noLine); err == nil || !strings.Contains(err.Error(), "pricing line") {
		t.Fatalf("err = %v", err)
	}
	noPrice := filepath.Join(dir, "b.html")
	_ = os.WriteFile(noPrice, []byte("<p id=\"pro-plan-price\">free</p>\n"), 0o644)
	if _, err := openPageEditor(noPrice); err == nil || !strings.Contains(err.Error(), "no $ price") {
		t.Fatalf("err = %v", err)
	}
	if _, err := openPageEditor(filepath.Join(dir, "missing.html")); err == nil || !strings.Contains(err.Error(), "could not be found") {
		t.Fatalf("err = %v", err)
	}
}

func TestRecoverDemoPagePutsBackAPageLeftEditedByACrash(t *testing.T) {
	path, orig := writeFakePage(t)
	ed, err := openPageEditor(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = ed.setPrice(77) // the process "dies" here: no restore
	recoverDemoPage(path)
	mustEqualFile(t, path, orig)
	if _, err := os.Stat(ed.backup); !os.IsNotExist(err) {
		t.Fatal("the safety copy should be gone after recovery")
	}
	recoverDemoPage(path) // nothing left to do, must not touch the page
	mustEqualFile(t, path, orig)
}

// ---- a small fake of Bifrost and of the companion -----------------------------

type fakeEntry struct {
	user   string
	answer string
}

type fakeRefresh struct{ done chan struct{} }

// fakeCache stands in for Bifrost (protected=false) or for the companion in
// front of it (protected=true). Entries are kept per cache key. A semantic hit
// is any "reset my Acme account ..." question after another one.
type fakeCache struct {
	protected bool
	pagePath  string
	mu        sync.Mutex
	entries   map[string][]fakeEntry
	refresh   map[string]*fakeRefresh
	modelHits int
	delay     time.Duration
	cleared   []string
}

func newFake(protected bool, pagePath string) *fakeCache {
	return &fakeCache{protected: protected, pagePath: pagePath, entries: map[string][]fakeEntry{}, refresh: map[string]*fakeRefresh{}}
}

func (f *fakeCache) pagePrice() int {
	b, _ := os.ReadFile(f.pagePath)
	i := bytes.Index(b, []byte(priceLineMarker))
	m := priceDigits.FindSubmatch(b[i:])
	n, _ := strconv.Atoi(string(m[1]))
	return n
}

type fakeReq struct {
	Messages []struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	} `json:"messages"`
}

func (f *fakeCache) model(system, user string) string {
	if system != "" {
		return "According to the provided information, " + strings.TrimPrefix(strings.Split(system, "\n")[2], "- ")
	}
	switch {
	case strings.Contains(user, "username"):
		return "Acme usernames cannot be changed in the mobile app."
	case strings.Contains(user, "password"):
		return "To reset your Acme password, open Settings."
	}
	return "Mock answer to: " + user
}

func (f *fakeCache) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/cache/clear-by-key/") {
		f.mu.Lock()
		k := strings.TrimPrefix(r.URL.Path, "/api/cache/clear-by-key/")
		delete(f.entries, k)
		f.cleared = append(f.cleared, k)
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		return
	}
	var req fakeReq
	_ = json.NewDecoder(r.Body).Decode(&req)
	var system, user string
	hasTool := false
	for _, m := range req.Messages {
		s, _ := m.Content.(string)
		switch m.Role {
		case "system":
			system = s
		case "user":
			user = s
		case "tool":
			hasTool = true
		}
	}
	key := r.Header.Get("x-bf-cache-key")
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	writeAnswer := func(answer string, hit bool, hitType string, extra map[string]string) {
		for k, v := range extra {
			w.Header().Set(k, v)
		}
		w.Header().Set("X-Bifrost-Cache-Hit", strconv.FormatBool(hit))
		if hit {
			w.Header().Set("X-Bifrost-Cache-Hit-Type", hitType)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": answer}}},
			"usage":   map[string]any{"total_tokens": 10},
		})
	}
	callModel := func() string {
		f.mu.Lock()
		f.modelHits++
		f.mu.Unlock()
		return f.model(system, user)
	}
	if hasTool && f.protected {
		writeAnswer(callModel(), false, "", map[string]string{"X-Freshness-Guard": "bypassed: tool_results"})
		return
	}

	f.mu.Lock()
	var found *fakeEntry
	semantic := false
	for i := range f.entries[key] {
		e := &f.entries[key][i]
		if e.user == user {
			found = e
			break
		}
		if strings.Contains(e.user, "reset my Acme account") && strings.Contains(user, "reset my Acme account") {
			found, semantic = e, true
		}
	}
	f.mu.Unlock()

	if found == nil {
		ans := callModel()
		f.mu.Lock()
		f.entries[key] = append(f.entries[key], fakeEntry{user: user, answer: ans})
		f.mu.Unlock()
		writeAnswer(ans, false, "", nil)
		return
	}
	hitType := "direct"
	extra := map[string]string{}
	if semantic {
		hitType = "semantic"
		extra["X-Bifrost-Cache-Similarity"] = "0.84"
		if f.protected {
			// The pre-serve check: ask the model, find a different answer, serve the fresh one.
			fresh := callModel()
			if fresh != found.answer {
				f.mu.Lock()
				f.entries[key] = append(f.entries[key], fakeEntry{user: user, answer: fresh})
				f.mu.Unlock()
				extra["X-Freshness-Gate"] = "blocked"
				writeAnswer(fresh, true, hitType, extra)
				return
			}
		}
		writeAnswer(found.answer, true, hitType, extra)
		return
	}
	if f.protected && strings.Contains(user, "cost") {
		cached, _ := priceIn(found.answer)
		live := f.pagePrice()
		extra["X-Freshness-Cached-Value"], extra["X-Freshness-Current-Value"] = strconv.Itoa(cached), strconv.Itoa(live)
		if cached != live {
			f.refreshEntry(w, key, found, system, user, extra, writeAnswer)
			return
		}
		extra["X-Freshness-Status"], extra["X-Freshness-Action"] = "confirmed", "served"
	}
	writeAnswer(found.answer, true, hitType, extra)
}

func (f *fakeCache) refreshEntry(w http.ResponseWriter, key string, found *fakeEntry, system, user string, extra map[string]string, write func(string, bool, string, map[string]string)) {
	extra["X-Freshness-Status"], extra["X-Freshness-Action"] = "stale", "refreshed"
	f.mu.Lock()
	if rf, ok := f.refresh[key]; ok {
		f.mu.Unlock()
		<-rf.done
		f.mu.Lock()
		var ans string
		for _, e := range f.entries[key] {
			if e.user == user {
				ans = e.answer
			}
		}
		f.mu.Unlock()
		extra["X-Freshness-Shared-Refresh"] = "true"
		write(ans, true, "direct", extra)
		return
	}
	rf := &fakeRefresh{done: make(chan struct{})}
	f.refresh[key] = rf
	f.mu.Unlock()

	time.Sleep(150 * time.Millisecond) // the refresh takes a moment, so the others pile up behind it
	f.mu.Lock()
	f.modelHits++
	fresh := f.model(system, user)
	for i := range f.entries[key] {
		if f.entries[key][i].user == user {
			f.entries[key][i].answer = fresh
		}
	}
	f.mu.Unlock()
	close(rf.done)
	time.AfterFunc(200*time.Millisecond, func() { f.mu.Lock(); delete(f.refresh, key); f.mu.Unlock() })
	write(fresh, true, "direct", extra)
}

type testStack struct {
	plain, prot *fakeCache
	runner      *scenarioRunner
	pagePath    string
	orig        []byte
	tally       *CacheTally
	close       func()
}

func newStack(t *testing.T) *testStack {
	t.Helper()
	path, orig := writeFakePage(t)
	st := &testStack{plain: newFake(false, path), prot: newFake(true, path), pagePath: path, orig: orig, tally: &CacheTally{}}
	ps := httptest.NewServer(st.plain)
	cs := httptest.NewServer(st.prot)
	st.runner = newScenarioRunner(&http.Client{Timeout: 5 * time.Second}, cs.URL, ps.URL, path, st.tally)
	st.runner.sleep = func(ctx context.Context, d time.Duration) error { return ctx.Err() } // no real 6 s wait in tests
	st.close = func() { ps.Close(); cs.Close() }
	t.Cleanup(st.close)
	return st
}

func run(t *testing.T, st *testStack, id string) *ScenarioResult {
	t.Helper()
	res, err := st.runner.execute(context.Background(), id, nil)
	if err != nil {
		t.Fatalf("%s: %v", id, err)
	}
	return res
}

func checkShape(t *testing.T, res *ScenarioResult) {
	t.Helper()
	if res.Plain.Label != "Plain cache (no protection)" || res.Protected.Label != "With the freshness companion" {
		t.Fatalf("labels = %q / %q", res.Plain.Label, res.Protected.Label)
	}
	if n := len(res.Narration); n < 1 || n > 3 {
		t.Fatalf("narration should be 1-3 sentences-blocks, got %d", n)
	}
	for _, s := range []Side{res.Plain, res.Protected} {
		switch s.Verdict {
		case "right", "wrong", "inconclusive", "error":
		default:
			t.Fatalf("bad verdict %q", s.Verdict)
		}
		if s.VerdictText == "" || len(s.Steps) == 0 || s.Facts == nil {
			t.Fatalf("incomplete side: %+v", s)
		}
	}
	// It must round-trip as JSON for the page.
	if _, err := json.Marshal(res); err != nil {
		t.Fatal(err)
	}
}

// ---- scenarios ------------------------------------------------------------------

func TestPriceChangedShowsPlainStaleAndProtectedFresh(t *testing.T) {
	st := newStack(t)
	var phases []string
	var sawWait bool
	res, err := st.runner.execute(context.Background(), idPriceChanged, func(phase string, wait time.Time) {
		phases = append(phases, phase)
		if !wait.IsZero() {
			sawWait = true
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	checkShape(t, res)
	if res.Plain.Verdict != "wrong" || !strings.Contains(res.Plain.Answer, "$49") {
		t.Fatalf("plain side = %+v", res.Plain)
	}
	if res.Protected.Verdict != "right" || !strings.Contains(res.Protected.Answer, "$59") {
		t.Fatalf("protected side = %+v", res.Protected)
	}
	if len(res.Plain.Steps) != 3 || len(res.Protected.Steps) != 3 {
		t.Fatalf("steps = %d / %d, want 3 each", len(res.Plain.Steps), len(res.Protected.Steps))
	}
	if !sawWait {
		t.Fatalf("the countdown phase was never reported: %v", phases)
	}
	if res.PageRestored == nil || !*res.PageRestored {
		t.Fatal("page_restored should be true")
	}
	mustEqualFile(t, st.pagePath, st.orig)
	// Only the companion road goes into the dashboard's tally.
	if snap := st.tally.snapshot(); snap.TotalRequests != 3 {
		t.Fatalf("tally has %d requests, want the 3 companion requests", snap.TotalRequests)
	}
	// Each road used its own cache key and left nothing behind.
	// (the fake Bifrost is the one that gets the clear calls)
	if len(st.plain.entries) != 0 || len(st.plain.cleared) != 2 || !strings.HasSuffix(st.plain.cleared[0], "-plain") || !strings.HasSuffix(st.plain.cleared[1], "-prot") {
		t.Fatalf("both keys should be emptied through Bifrost: %v", st.plain.cleared)
	}
}

func TestPriceChangedRestoresThePageWhenCancelledDuringTheWait(t *testing.T) {
	st := newStack(t)
	ctx, cancel := context.WithCancel(context.Background())
	st.runner.sleep = func(c context.Context, d time.Duration) error {
		if got := readString(t, st.pagePath); !strings.Contains(got, "$59 per month") {
			t.Errorf("the page should be edited while waiting")
		}
		cancel()
		return c.Err()
	}
	_, err := st.runner.execute(ctx, idPriceChanged, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	mustEqualFile(t, st.pagePath, st.orig)
}

func TestRunLimitStopsALongWaitAndRestoresThePage(t *testing.T) {
	st := newStack(t)
	st.runner.evidenceWait = 10 * time.Second
	st.runner.sleep = sleepCtx
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err := st.runner.execute(ctx, idEveryoneAt, nil)
	if err == nil || !strings.Contains(err.Error(), "took too long") {
		t.Fatalf("err = %v", err)
	}
	mustEqualFile(t, st.pagePath, st.orig)
}

func TestSlowServerGivesAPlainTimeoutInTheResultNotACrash(t *testing.T) {
	st := newStack(t)
	st.prot.delay = 400 * time.Millisecond
	st.runner.client = &http.Client{Timeout: 100 * time.Millisecond}
	res := run(t, st, idPriceChanged)
	checkShape(t, res)
	if res.Protected.Verdict != "error" || !strings.Contains(res.Protected.VerdictText, "Did not finish") {
		t.Fatalf("protected = %+v", res.Protected)
	}
	mustEqualFile(t, st.pagePath, st.orig)
}

func TestCompanionDownIsReportedInPlainWords(t *testing.T) {
	st := newStack(t)
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	st.runner.companionBase = dead.URL
	res := run(t, st, idLookAlike)
	checkShape(t, res)
	if res.Protected.Verdict != "error" || !strings.Contains(res.Protected.VerdictText, "Is it running?") {
		t.Fatalf("protected = %+v", res.Protected)
	}
}

func TestLookAlikePlainGivesTheWrongAnswerAndTheCheckCatchesIt(t *testing.T) {
	st := newStack(t)
	res := run(t, st, idLookAlike)
	checkShape(t, res)
	if res.Plain.Verdict != "wrong" || !strings.Contains(res.Plain.Answer, "password") {
		t.Fatalf("plain = %+v", res.Plain)
	}
	if res.Protected.Verdict != "right" || !strings.Contains(res.Protected.Answer, "username") {
		t.Fatalf("protected = %+v", res.Protected)
	}
	if res.PageRestored != nil {
		t.Fatal("this scenario never touches the page")
	}
}

func TestToolDataReusedByPlainButKeptOutByTheGuard(t *testing.T) {
	st := newStack(t)
	res := run(t, st, idToolData)
	checkShape(t, res)
	if res.Plain.Verdict != "wrong" || res.Protected.Verdict != "right" {
		t.Fatalf("plain %q / protected %q", res.Plain.Verdict, res.Protected.Verdict)
	}
	if len(res.Notes) == 0 || !strings.Contains(res.Notes[0], "practice model") {
		t.Fatalf("the practice-model caveat is missing: %v", res.Notes)
	}
	if st.prot.modelHits != 2 {
		t.Fatalf("the guarded road should call the model both times, got %d", st.prot.modelHits)
	}
}

func TestEveryoneAtOnceCountsAnswersErrorsAndSharedRefreshes(t *testing.T) {
	st := newStack(t)
	res := run(t, st, idEveryoneAt)
	checkShape(t, res)
	n := st.runner.burstSize
	if res.Plain.Verdict != "wrong" {
		t.Fatalf("plain = %+v", res.Plain)
	}
	if res.Protected.Verdict != "right" {
		t.Fatalf("protected = %+v", res.Protected)
	}
	b := tallyBurst(res.Protected.Steps[2:], 49, 59)
	if b.Asked != n || b.Answered != n || b.Errors != 0 || b.NewPrice != n || b.Refreshers != 1 || b.Shared < 1 {
		t.Fatalf("burst = %+v", b)
	}
	if got := fmt.Sprint(res.Protected.Facts); !strings.Contains(got, "shared one refresh: ") {
		t.Fatalf("facts = %v", res.Protected.Facts)
	}
	if st.prot.modelHits != 2 { // the first ask + exactly one refresh
		t.Fatalf("protected model calls = %d, want 2", st.prot.modelHits)
	}
	mustEqualFile(t, st.pagePath, st.orig)
}

// ---- flag missing, busy, HTTP ---------------------------------------------------

func TestWithoutDemoPageFlagThePriceScenariosAreOffWithAClearNote(t *testing.T) {
	st := newStack(t)
	st.runner.demoPage = ""
	byID := map[string]ScenarioInfo{}
	for _, i := range st.runner.list() {
		byID[i.ID] = i
	}
	for _, id := range []string{idPriceChanged, idEveryoneAt} {
		i := byID[id]
		if i.Available || !i.NeedsDemoPage || !strings.Contains(i.Unavailable, "-demo-page") {
			t.Fatalf("%s = %+v", id, i)
		}
		if _, err := st.runner.start(id); err == nil || !strings.Contains(err.Error(), "-demo-page") {
			t.Fatalf("start(%s) err = %v", id, err)
		}
	}
	for _, id := range []string{idLookAlike, idToolData} {
		if !byID[id].Available {
			t.Fatalf("%s should still work without the page", id)
		}
	}
	if len(byID) != 4 {
		t.Fatalf("want 4 scenarios, got %d", len(byID))
	}
}

func TestMissingPageFileDisablesTheButtonToo(t *testing.T) {
	st := newStack(t)
	st.runner.demoPage = filepath.Join(t.TempDir(), "nope.html")
	for _, i := range st.runner.list() {
		if i.NeedsDemoPage && (i.Available || !strings.Contains(i.Unavailable, "not found")) {
			t.Fatalf("%+v", i)
		}
	}
}

func TestOnlyOneScenarioRunsAtATimeAndPollingReturnsTheResult(t *testing.T) {
	st := newStack(t)
	release := make(chan struct{})
	st.runner.sleep = func(ctx context.Context, d time.Duration) error {
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	runID, err := st.runner.start(idPriceChanged)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.runner.start(idLookAlike); !errors.Is(err, errScenarioBusy) {
		t.Fatalf("second start err = %v", err)
	}
	// While it waits the snapshot says so.
	deadline := time.Now().Add(3 * time.Second)
	for {
		snap, _ := st.runner.get(runID)
		if snap.State == "running" && snap.WaitUntilMS > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never reached the waiting phase: %+v", snap)
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(release)
	for {
		snap, ok := st.runner.get(runID)
		if !ok {
			t.Fatal("run forgotten")
		}
		if snap.State == "done" {
			if snap.Result == nil || snap.Result.Plain.Verdict != "wrong" {
				t.Fatalf("result = %+v", snap.Result)
			}
			break
		}
		if snap.State == "error" || time.Now().After(deadline.Add(3*time.Second)) {
			t.Fatalf("snap = %+v", snap)
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := st.runner.start(idLookAlike); err != nil {
		t.Fatalf("after the first finished a new run must be allowed: %v", err)
	}
}

func TestScenarioHTTPHandlers(t *testing.T) {
	st := newStack(t)
	st.runner.demoPage = ""
	s := &server{scenarios: st.runner}
	call := func(method, path, id string, h http.HandlerFunc) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec
	}
	if rec := call("GET", "/api/scenarios", "", s.handleScenarios); rec.Code != 200 || !strings.Contains(rec.Body.String(), idPriceChanged) {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	if rec := call("POST", "/x", "nope", s.handleScenarioRun); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown: %d", rec.Code)
	}
	if rec := call("POST", "/x", idPriceChanged, s.handleScenarioRun); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "-demo-page") {
		t.Fatalf("no page: %d %s", rec.Code, rec.Body)
	}
	if rec := call("GET", "/x", "missing", s.handleScenarioRunStatus); rec.Code != http.StatusNotFound {
		t.Fatalf("status of unknown run: %d", rec.Code)
	}
	rec := call("POST", "/x", idToolData, s.handleScenarioRun)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	var started struct {
		RunID string `json:"run_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &started)
	for i := 0; i < 200; i++ {
		rec = call("GET", "/x", started.RunID, s.handleScenarioRunStatus)
		if strings.Contains(rec.Body.String(), `"state":"done"`) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("never finished: %s", rec.Body)
}

func TestPriceInReadsTheFirstDollarAmount(t *testing.T) {
	for in, want := range map[string]int{"costs $49 per month": 49, "It is $ 1,200 a year": 1200, "$59.": 59} {
		if got, ok := priceIn(in); !ok || got != want {
			t.Errorf("priceIn(%q) = %d, %v", in, got, ok)
		}
	}
	if _, ok := priceIn("free"); ok {
		t.Error("no price expected")
	}
}
