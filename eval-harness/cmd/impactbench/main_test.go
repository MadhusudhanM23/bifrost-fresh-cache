package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// pageBytes mimics the demo page: an ASCII price sentence next to an em dash.
func pageBytes(price int) []byte {
	return []byte(fmt.Sprintf("<html><body>\n<p id=\"pro-plan-price\">Acme Pro plan — $%d per month</p>\n<p>Last updated 2 November 2026 — demo</p>\n</body></html>\n", price))
}

func TestClassify(t *testing.T) {
	price := Item{Kind: KindPrice}
	faq := Item{Kind: KindFAQ}
	tool := Item{Kind: KindTool}
	fresh59 := "According to the provided information, The Acme Pro plan costs $59 per month."
	old49 := "According to the provided information, The Acme Pro plan costs $49 per month."
	cases := []struct {
		name      string
		it        Item
		status    int
		served    string
		truth     string
		fromCache bool
		want      string
	}{
		{"same text", price, 200, fresh59, fresh59, true, VCorrect},
		{"whitespace only differs", price, 200, "  " + fresh59 + "\n", fresh59, false, VCorrect},
		{"old price", price, 200, old49, fresh59, true, VStale},
		{"price question, different text but no price clash", price, 200, "The provided information does not answer: x", fresh59, false, VWrongMatch},
		{"price question, same price different words", price, 200, "It costs $59.", fresh59, false, VWrongMatch},
		{"look-alike answer", faq, 200, "To reset your Acme password", "Acme usernames cannot be changed", true, VWrongMatch},
		{"faq with a price in both is not stale", faq, 200, "costs $49", "costs $59", true, VWrongMatch},
		{"server error", price, 502, "", fresh59, false, VError},
		{"empty answer", price, 200, "  ", fresh59, false, VError},
		{"tool from cache", tool, 200, "x", "", true, VToolReuse},
		{"tool fresh", tool, 200, "x", "", false, VToolFresh},
		{"tool error", tool, 500, "", "", false, VError},
	}
	for _, c := range cases {
		if got := Classify(c.it, c.status, c.served, c.truth, c.fromCache); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
	for v, bad := range map[string]bool{VCorrect: false, VStale: true, VWrongMatch: true, VError: true, VToolReuse: false, VToolFresh: false} {
		if IsBad(v) != bad {
			t.Errorf("IsBad(%s) = %v", v, !bad)
		}
	}
}

func TestExtractPrice(t *testing.T) {
	for in, want := range map[string]string{"costs $49 per month": "49", "now $ 1,299.50 a month": "1299.50", "no price": ""} {
		got, ok := ExtractPrice(in)
		if got != want || ok != (want != "") {
			t.Errorf("ExtractPrice(%q) = %q, %v", in, got, ok)
		}
	}
}

func TestMedianAndMetrics(t *testing.T) {
	if median([]float64{3, 1, 2}) != 2 || median([]float64{4, 1, 2, 3}) != 2.5 || median(nil) != 0 {
		t.Fatal("median")
	}
	got := MedianMetrics([]Metrics{{"a": 1, "b": 10}, {"a": 5}, {"a": 3, "b": 30}})
	if got["a"] != 3 || got["b"] != 10 { // a missing key counts as 0 for that run
		t.Fatalf("MedianMetrics = %v", got)
	}
}

func TestBuildWorkloadIsDeterministicAndWellFormed(t *testing.T) {
	cfg := WorkloadConfig{BasePrice: 49, ChangedPrice: 59, BurstPrice: 69, Burst: 16}
	a, b := BuildWorkload(7, cfg), BuildWorkload(7, cfg)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("same seed must give the same list")
	}
	different := false
	for s := int64(8); s < 20 && !different; s++ {
		different = !reflect.DeepEqual(a, BuildWorkload(s, cfg))
	}
	if !different {
		t.Fatal("the seed has no effect on the order")
	}
	counts := PhaseCounts(a)
	if counts[PhaseBurst] != 16 || counts[PhaseWarm] == 0 || counts[PhaseAfter] == 0 {
		t.Fatalf("phase counts %v", counts)
	}
	ids := map[string]bool{}
	for _, it := range a {
		if ids[it.ID] {
			t.Fatalf("duplicate id %s", it.ID)
		}
		ids[it.ID] = true
		wantPrice := map[string]int{PhaseWarm: 49, PhaseAfter: 59, PhaseBurst: 69}[it.Phase]
		if it.Kind == KindPrice && it.Price != wantPrice {
			t.Fatalf("%s: price context %d, want %d", it.ID, it.Price, wantPrice)
		}
		if it.Kind != KindPrice && it.Price != 0 {
			t.Fatalf("%s: only price requests carry the price context", it.ID)
		}
	}
	// Look-alikes: A before B when warming, B before A after the change.
	pos := func(phase, q string) int {
		for i, it := range a {
			if it.Phase == phase && it.Question == q {
				return i
			}
		}
		return -1
	}
	for _, p := range lookAlikes {
		if !(pos(PhaseWarm, p.A) >= 0 && pos(PhaseWarm, p.A) < pos(PhaseWarm, p.B)) {
			t.Errorf("%s: warm order must be A then B", p.Name)
		}
		if !(pos(PhaseAfter, p.B) >= 0 && pos(PhaseAfter, p.B) < pos(PhaseAfter, p.A)) {
			t.Errorf("%s: after-change order must be B then A", p.Name)
		}
	}
	// Tool requests: same question twice in a row with a different tool result.
	tools := 0
	for i, it := range a {
		if it.Kind != KindTool {
			continue
		}
		tools++
		if tools%2 == 1 {
			next := a[i+1]
			if next.Kind != KindTool || next.Question != it.Question || next.Tool == it.Tool {
				t.Fatalf("tool pair broken at %s", it.ID)
			}
		}
	}
	if tools < 3 || tools > 4 {
		t.Fatalf("want 3-4 tool requests, got %d", tools)
	}
	// Burst: all identical bodies.
	var first []byte
	for _, it := range a {
		if it.Phase != PhaseBurst {
			continue
		}
		if first == nil {
			first = it.Body("m")
		} else if !bytes.Equal(first, it.Body("m")) {
			t.Fatal("burst bodies differ")
		}
	}
}

func TestItemBody(t *testing.T) {
	var got struct {
		Model    string
		Messages []struct{ Role, Content, Tool_call_id string }
	}
	price := Item{Kind: KindPrice, Question: "How much?", Price: 59}
	if err := json.Unmarshal(price.Body("m"), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 2 || got.Messages[0].Role != "system" || !strings.Contains(got.Messages[0].Content, "Context:\n- The Acme Pro plan costs $59 per month.") || got.Messages[1].Content != "How much?" {
		t.Fatalf("price body %+v", got)
	}
	tool := Item{Kind: KindTool, Question: "Status?", Tool: `{"s":1}`}
	got.Messages = nil
	if err := json.Unmarshal(tool.Body("m"), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 2 || got.Messages[1].Role != "tool" || got.Messages[1].Content != `{"s":1}` || got.Messages[1].Tool_call_id == "" {
		t.Fatalf("tool body %+v", got)
	}
}

func TestWithPriceChangesOnlyTheDigits(t *testing.T) {
	orig := pageBytes(49)
	if p, err := PriceOf(orig); err != nil || p != 49 {
		t.Fatalf("PriceOf = %d, %v", p, err)
	}
	edited, err := WithPrice(orig, 59)
	if err != nil {
		t.Fatal(err)
	}
	if len(edited) != len(orig) || !bytes.Contains(edited, []byte("— $59 per month")) || !bytes.Contains(edited, []byte("2 November 2026 — demo")) {
		t.Fatalf("edit lost bytes:\n%s", edited)
	}
	diff := 0
	for i := range orig {
		if orig[i] != edited[i] {
			diff++
		}
	}
	if diff != 1 { // 4 -> 5 is one byte
		t.Fatalf("%d bytes differ, want 1", diff)
	}
	longer, _ := WithPrice(orig, 1299)
	if back, _ := WithPrice(longer, 49); !bytes.Equal(back, orig) {
		t.Fatal("editing a longer number and back must restore the bytes")
	}
	if _, err := WithPrice([]byte("no price here"), 5); err == nil {
		t.Fatal("want an error when the page has no price sentence")
	}
	two := append(pageBytes(49), pageBytes(49)...)
	if _, err := WithPrice(two, 5); err == nil {
		t.Fatal("want an error when the page has two price sentences")
	}
}

func TestPageSetAndRestore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.html")
	orig := pageBytes(49)
	if err := os.WriteFile(path, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := OpenPage(path)
	if err != nil {
		t.Fatal(err)
	}
	if base, _ := p.BasePrice(); base != 49 || p.OriginalSize() != len(orig) || len(p.OriginalSHA256()) != 64 {
		t.Fatalf("base %d size %d sha %s", base, p.OriginalSize(), p.OriginalSHA256())
	}
	for _, price := range []int{59, 69, 7} {
		if err := p.Set(price); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(path)
		if got, _ := PriceOf(b); got != price {
			t.Fatalf("page states %d, want %d", got, price)
		}
	}
	if err := p.Verify(); err == nil {
		t.Fatal("Verify must notice the edit")
	}
	if err := p.Restore(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, orig) {
		t.Fatal("restore is not byte-identical")
	}
	if err := p.Restore(); err != nil { // idempotent
		t.Fatal(err)
	}
}

func TestParseArmsAndRotate(t *testing.T) {
	arms, err := parseArms("plain, companion ,plain-ttl", "http://p/", "http://c/", 5*time.Second)
	if err != nil || len(arms) != 3 || arms[0].URL != "http://p" || arms[1].URL != "http://c" || arms[2].Headers["x-bf-cache-ttl"] != "5s" {
		t.Fatalf("%+v %v", arms, err)
	}
	for _, bad := range []string{"", "plain,plain", "nope"} {
		if _, err := parseArms(bad, "p", "c", time.Second); err == nil {
			t.Errorf("parseArms(%q) should fail", bad)
		}
	}
	names := func(a []Arm) string {
		var s []string
		for _, x := range a {
			s = append(s, x.Name)
		}
		return strings.Join(s, ",")
	}
	if names(rotate(arms, 0)) != "plain,companion,plain-ttl" || names(rotate(arms, 1)) != "companion,plain-ttl,plain" || names(rotate(arms, 4)) != "companion,plain-ttl,plain" {
		t.Fatal("rotate")
	}
}

func TestCompanionFlags(t *testing.T) {
	compose := "services:\n  bifrost:\n    command:\n      - \"-nope\"\n  freshness-companion:\n    command:\n      - \"/usr/local/bin/freshness-companion\"\n      - \"-listen=:8090\"\n      # comment\n      - \"-verify-below=0.90\"\n    environment:\n      X: \"y\"\n  other:\n    command:\n      - \"-also-no\"\n"
	got := companionFlags(compose)
	if !reflect.DeepEqual(got, []string{"-listen=:8090", "-verify-below=0.90"}) {
		t.Fatalf("%v", got)
	}
}

// fakeStack is a stand-in for the mock model and a gateway with a naive cache
// that ignores the system message (like Bifrost's exclude_system_prompt).
type fakeStack struct {
	mu       sync.Mutex
	chat     int
	cache    map[string]string // cache key + user question -> answer
	failFrom int               // when > 0, the gateway returns 500 from this chat call on
	gwCalls  int
}

func (f *fakeStack) answer(req map[string]any) string {
	msgs, _ := req["messages"].([]any)
	system, question := "", ""
	for _, m := range msgs {
		mm := m.(map[string]any)
		switch mm["role"] {
		case "system":
			system, _ = mm["content"].(string)
		case "user":
			question, _ = mm["content"].(string)
		}
	}
	if i := strings.Index(system, "- "); i >= 0 {
		line := system[i+2:]
		return "According to the provided information, " + strings.SplitN(line, "\n", 2)[0]
	}
	return "Mock answer to: " + question
}

func (f *fakeStack) handler() http.Handler {
	mux := http.NewServeMux()
	chat := func(w http.ResponseWriter, r *http.Request, cached bool) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		msgs, _ := req["messages"].([]any)
		question := ""
		for _, m := range msgs {
			if mm := m.(map[string]any); mm["role"] == "user" {
				question, _ = mm["content"].(string)
			}
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		hit := false
		ans := ""
		if cached {
			f.gwCalls++
			if f.failFrom > 0 && f.gwCalls >= f.failFrom {
				http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
				return
			}
			k := r.Header.Get("x-bf-cache-key") + "|" + question
			if a, ok := f.cache[k]; ok {
				ans, hit = a, true
			} else {
				f.chat++
				ans = f.answer(req)
				f.cache[k] = ans
			}
		} else {
			f.chat++
			ans = f.answer(req)
		}
		w.Header().Set("X-Bifrost-Cache-Hit", fmt.Sprint(hit))
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": ans}}}})
	}
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"status":"ok"}`) })
	mux.HandleFunc("/mock/stats", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"chat_calls": f.chat})
	})
	mux.HandleFunc("/mock/v1", func(w http.ResponseWriter, r *http.Request) { chat(w, r, false) })
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) { chat(w, r, true) })
	return mux
}

// mockOnly serves the fake model's direct chat endpoint on /v1/chat/completions.
func (f *fakeStack) mockHandler() http.Handler {
	inner := f.handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			r.URL.Path = "/mock/v1"
		}
		inner.ServeHTTP(w, r)
	})
}

func newTestRunner(t *testing.T, f *fakeStack) (*Runner, Arm, string) {
	t.Helper()
	mock := httptest.NewServer(f.mockHandler())
	gw := httptest.NewServer(f.handler())
	t.Cleanup(mock.Close)
	t.Cleanup(gw.Close)
	path := filepath.Join(t.TempDir(), "index.html")
	if err := os.WriteFile(path, pageBytes(49), 0o644); err != nil {
		t.Fatal(err)
	}
	page, err := OpenPage(path)
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{Client: http.DefaultClient, MockURL: mock.URL, Model: "m", Page: page, Settle: time.Millisecond, Poll: 2 * time.Millisecond,
		Cfg: WorkloadConfig{BasePrice: 49, ChangedPrice: 59, BurstPrice: 69, Burst: 8}}
	return r, Arm{Name: "plain", URL: gw.URL}, path
}

func TestRunArmAgainstNaiveCacheCountsStaleAndRestoresPage(t *testing.T) {
	f := &fakeStack{cache: map[string]string{}}
	r, arm, path := newTestRunner(t, f)
	items := BuildWorkload(1, r.Cfg)
	ctx := context.Background()
	truth, err := r.GroundTruth(ctx, items)
	if err != nil {
		t.Fatal(err)
	}
	groundTruthCalls := f.chat
	if groundTruthCalls == 0 {
		t.Fatal("ground truth must call the model")
	}
	ar, err := r.RunArm(ctx, arm, "key-1", items, truth, CostModel{PerChatCall: 0.002})
	if err != nil {
		t.Fatal(err)
	}
	m := ar.Metrics
	if int(m["requests"]) != len(items) {
		t.Fatalf("requests %v, want %d", m["requests"], len(items))
	}
	// The fake cache keys on the question only, so every repeated price question
	// is answered with the warm-up price: stale.
	if m["stale"] == 0 || m["burst_stale"] != 8 || m["burst_model_calls"] != 0 {
		t.Fatalf("expected stale answers and a stale burst served without model calls: %+v", m)
	}
	if int(m["model_calls"]) != f.chat-groundTruthCalls {
		t.Fatalf("model_calls %v, but the fake model answered %d calls during the arm", m["model_calls"], f.chat-groundTruthCalls)
	}
	if m["model_calls"]+m["cache_hits"] != m["requests"] {
		t.Fatalf("each request is either a hit or a model call here: %v + %v != %v", m["model_calls"], m["cache_hits"], m["requests"])
	}
	if m["errors"] != 0 || m["wrong_match"] != 0 {
		t.Fatalf("unexpected errors / wrong matches: %+v", m)
	}
	if m["serving_calls"] != m["model_calls"] || m["gate_calls"] != 0 {
		t.Fatalf("no companion: all calls are serving calls: %+v", m)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, pageBytes(49)) {
		t.Fatal("page not restored after the arm")
	}
	if want := 1000 * m["model_calls"] * 0.002 / m["requests"]; m["cost_per_1000_usd"] != want {
		t.Fatalf("cost %v, want %v", m["cost_per_1000_usd"], want)
	}
}

func TestRunArmRestoresPageWhenCancelled(t *testing.T) {
	f := &fakeStack{cache: map[string]string{}}
	r, arm, path := newTestRunner(t, f)
	r.Settle = 200 * time.Millisecond
	items := BuildWorkload(1, r.Cfg)
	truth, err := r.GroundTruth(context.Background(), items)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel() // like Ctrl-C while the page is edited and the run is waiting
	}()
	if _, err := r.RunArm(ctx, arm, "key-2", items, truth, CostModel{}); err == nil {
		t.Fatal("want an error from a cancelled run")
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, pageBytes(49)) {
		t.Fatalf("page not restored after cancel: %s", got)
	}
}

func TestRunArmCountsGatewayErrorsAndRestoresPage(t *testing.T) {
	f := &fakeStack{cache: map[string]string{}, failFrom: 20}
	r, arm, path := newTestRunner(t, f)
	items := BuildWorkload(1, r.Cfg)
	truth, _ := r.GroundTruth(context.Background(), items)
	ar, err := r.RunArm(context.Background(), arm, "key-3", items, truth, CostModel{})
	if err != nil {
		t.Fatal(err)
	}
	if ar.Metrics["errors"] == 0 || ar.Metrics["bad"] < ar.Metrics["errors"] {
		t.Fatalf("gateway 500s must show up as errors and bad answers: %+v", ar.Metrics)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, pageBytes(49)) {
		t.Fatal("page not restored")
	}
}

func TestMarkdownRendersWithoutPanicking(t *testing.T) {
	f := &fakeStack{cache: map[string]string{}}
	r, arm, _ := newTestRunner(t, f)
	items := BuildWorkload(1, r.Cfg)
	truth, _ := r.GroundTruth(context.Background(), items)
	ar, err := r.RunArm(context.Background(), arm, "key-4", items, truth, CostModel{PerChatCall: 0.002})
	if err != nil {
		t.Fatal(err)
	}
	rep := &Report{Tool: "impactbench", Banner: "b", Arms: []Arm{arm}, Runs: []RunRecord{{Index: 1, Seed: 1, ArmOrder: []string{"plain"}, Arms: map[string]*ArmRun{"plain": ar}}},
		Median: map[string]Metrics{"plain": ar.Metrics}, Workload: WorkloadInfo{PhaseCounts: PhaseCounts(items), FirstRun: items}}
	rep.Limits = limits(rep)
	md := renderMarkdown(rep)
	for _, want := range []string{"## Headline", "Bad answers per 1000 requests", "## Honest limits", "ASSUMED"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q", want)
		}
	}
}

func TestRerenderFromJSONAndLookAlikeNotes(t *testing.T) {
	rep := &Report{Tool: "impactbench", Banner: "b", Arms: []Arm{{Name: "plain"}},
		Stack: Stack{SemanticCache: map[string]any{"threshold": 0.8}, PolicyGuard: map[string]any{"_comment": "x", "tool_results": "bypass"}},
		Workload: WorkloadInfo{PhaseCounts: map[string]int{}, Similarity: []SimRow{
			{Label: "look-alike pair: a", Cosine: 0.7}, {Label: "look-alike pair: b", Cosine: 0.84}, {Label: "look-alike pair: c", Cosine: 1},
			{Label: "same-meaning rewording: a", Cosine: 1}}},
		Median: map[string]Metrics{"plain": {}}, Runs: []RunRecord{}}
	notes := lookAlikeNotes(rep)
	if len(notes) != 3 || !strings.Contains(notes[0], "below the cache threshold") || !strings.Contains(notes[1], "can serve one question") || !strings.Contains(notes[2], "no similarity threshold") {
		t.Fatalf("notes %q", notes)
	}
	if g := guardLine(rep.Stack.PolicyGuard); g != `{"tool_results":"bypass"}` {
		t.Fatalf("guard line %s", g)
	}
	raw, _ := json.Marshal(rep)
	path := filepath.Join(t.TempDir(), "r.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rerender(path); err != nil {
		t.Fatal(err)
	}
	md, err := os.ReadFile(filepath.Join(filepath.Dir(path), "r.md"))
	if err != nil || !strings.Contains(string(md), "# Impact benchmark") {
		t.Fatalf("md %q err %v", md, err)
	}
	if got := containerMap(map[string]any{"a": "x"}); got["a"] != "x" {
		t.Fatal("containerMap")
	}
}

// ---- real-model options (-truth context, -calls-from bifrost-metrics, body options) ----

func TestBodyWithOptions(t *testing.T) {
	tool := Item{Kind: KindTool, Question: "Status?", Tool: `{"s":1}`}
	if !bytes.Equal(tool.Body("m"), tool.BodyWith("m", BodyOptions{})) {
		t.Fatal("the zero options must give the mock run's body")
	}
	if strings.Contains(string(tool.Body("m")), "max_tokens") || strings.Contains(string(tool.Body("m")), "tool_calls") {
		t.Fatalf("mock body changed: %s", tool.Body("m"))
	}
	var got struct {
		MaxTokens int `json:"max_tokens"`
		Messages  []struct {
			Role       string
			ToolCallID string                `json:"tool_call_id"`
			ToolCalls  []struct{ ID string } `json:"tool_calls"`
		}
	}
	if err := json.Unmarshal(tool.BodyWith("m", BodyOptions{MaxTokens: 800, ToolCallMessage: true}), &got); err != nil {
		t.Fatal(err)
	}
	if got.MaxTokens != 800 || len(got.Messages) != 3 || got.Messages[1].Role != "assistant" || len(got.Messages[1].ToolCalls) != 1 ||
		got.Messages[1].ToolCalls[0].ID != got.Messages[2].ToolCallID || got.Messages[2].Role != "tool" {
		t.Fatalf("real-model tool body %+v", got)
	}
}

func TestMeaningGroup(t *testing.T) {
	pw := lookAlikes[0]
	if MeaningGroup(pw.A) != MeaningGroup(pw.AParaphrase) || MeaningGroup(pw.A) == MeaningGroup(pw.B) {
		t.Fatal("a FAQ question and its rewording share a group; the look-alike does not")
	}
	tf := lookAlikes[2]
	if MeaningGroup(tf.A) == MeaningGroup(tf.B) || MeaningGroup("") == MeaningGroup(tf.A) {
		t.Fatal("two-factor on and off are different questions")
	}
	if MeaningGroup(benignWarm[0]) != MeaningGroup(benignAfter[1]) || MeaningGroup(benignWarm[1]) != MeaningGroup(benignAfter[2]) ||
		MeaningGroup(benignWarm[0]) == MeaningGroup(benignWarm[1]) {
		t.Fatal("benign groups")
	}
	if MeaningGroup("something else") == MeaningGroup("another thing") {
		t.Fatal("unknown questions are their own group")
	}
}

func TestClassifyContext(t *testing.T) {
	price := Item{Kind: KindPrice, Price: 59}
	pw := lookAlikes[0]
	faqA := Item{Kind: KindFAQ, Question: pw.A}
	cases := []struct {
		name      string
		it        Item
		status    int
		served    string
		fromCache bool
		hitType   string
		origin    string
		known     bool
		want      string
	}{
		{"price right, any wording", price, 200, "It's **$59 per month**.", true, "semantic", "", false, VCorrect},
		{"price with cents", price, 200, "The plan costs $59.00 a month.", false, "", "", false, VCorrect},
		{"old price", price, 200, "The Acme Pro plan costs $49 per month.", true, "direct", "", false, VStale},
		{"price question, no price", price, 200, "To reset your password, open Settings.", true, "semantic", "", false, VWrongMatch},
		{"error", price, 429, "", false, "", "", false, VError},
		{"empty answer", faqA, 200, "  ", false, "", "", false, VError},
		{"fresh FAQ answer", faqA, 200, "anything", false, "", "", false, VCorrect},
		{"cached from the same question", faqA, 200, "x", true, "semantic", pw.A, true, VCorrect},
		{"cached from a rewording", faqA, 200, "x", true, "semantic", pw.AParaphrase, true, VCorrect},
		{"cached from the look-alike", faqA, 200, "x", true, "semantic", pw.B, true, VWrongMatch},
		{"unknown origin, direct hit", faqA, 200, "x", true, "direct", "", false, VCorrect},
		{"unknown origin, semantic hit", faqA, 200, "x", true, "semantic", "", false, VUnjudged},
		{"tool from cache", Item{Kind: KindTool}, 200, "x", true, "semantic", "", false, VToolReuse},
	}
	for _, c := range cases {
		if got := ClassifyContext(c.it, c.status, c.served, c.fromCache, c.hitType, c.origin, c.known); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
	if IsBad(VUnjudged) {
		t.Fatal("an unjudged answer is not counted as bad")
	}
}

func TestProviderChatCalls(t *testing.T) {
	metrics := `# HELP bifrost_upstream_requests_total x
# TYPE bifrost_upstream_requests_total counter
bifrost_upstream_requests_total{method="chat_completion",model="openai/gpt-oss-20b",provider="groq",selected_key_id="k1",selected_key_name="groq-from-env",team_id=""} 7
bifrost_upstream_requests_total{method="chat_completion",model="openai/gpt-oss-20b",provider="groq",selected_key_id="",selected_key_name="",team_id=""} 30
bifrost_upstream_requests_total{method="list_models",model="",provider="groq",selected_key_id="",selected_key_name="groq-from-env"} 1
bifrost_upstream_requests_total{method="chat_completion",model="gpt-4o-mini",provider="openai",selected_key_id="k2",selected_key_name="mock-openai-local"} 2
bifrost_success_requests_total{method="chat_completion",provider="groq",selected_key_name="groq-from-env"} 7
`
	if got := ProviderChatCalls(metrics); got != 9 {
		t.Fatalf("got %d, want 9 (7 groq + 2 mock chat calls with a key; cache hits and model listing left out)", got)
	}
}

func TestRunArmContextTruthNeedsNoGroundTruthCallsAndCountsFromMetrics(t *testing.T) {
	f := &fakeStack{cache: map[string]string{}}
	r, arm, _ := newTestRunner(t, f)
	metrics := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		fmt.Fprintf(w, "bifrost_upstream_requests_total{method=\"chat_completion\",provider=\"groq\",selected_key_name=\"g\"} %d\n", f.chat)
	}))
	t.Cleanup(metrics.Close)
	r.Truth, r.MetricsURL, r.BodyOpt = TruthContext, metrics.URL, BodyOptions{MaxTokens: 800, ToolCallMessage: true}
	items := BuildWorkload(1, r.Cfg)
	ctx := context.Background()
	truth, err := r.GroundTruth(ctx, items)
	if err != nil {
		t.Fatal(err)
	}
	if f.chat != 0 {
		t.Fatalf("context truth must not call the model, made %d calls", f.chat)
	}
	ar, err := r.RunArm(ctx, arm, "key-ctx", items, truth, CostModel{})
	if err != nil {
		t.Fatal(err)
	}
	m := ar.Metrics
	if int(m["model_calls"]) != f.chat || m["model_calls"]+m["cache_hits"] != m["requests"] {
		t.Fatalf("model_calls %v from metrics, fake model answered %d, hits %v of %v", m["model_calls"], f.chat, m["cache_hits"], m["requests"])
	}
	// The fake caches by question and ignores the system message: repeated price
	// questions get the warm price (stale), the burst is all stale, and cached
	// FAQ answers were written for the same question (right).
	if m["stale"] == 0 || m["burst_stale"] != 8 || m["wrong_match"] != 0 || m["errors"] != 0 || m["unjudged"] != 0 {
		t.Fatalf("unexpected verdicts: %+v", m)
	}
}

func TestMarkdownRealModelWording(t *testing.T) {
	rep := &Report{Tool: "impactbench", Banner: "REAL", Arms: []Arm{{Name: "plain"}},
		Settings: Settings{Truth: TruthContext, Model: "groq/x", MaxTokens: 800, CallsFrom: "bifrost-metrics", ComposePath: "over.yml"},
		Stack:    Stack{SemanticCache: map[string]any{"threshold": 0.8}},
		Workload: WorkloadInfo{PhaseCounts: map[string]int{}}, Median: map[string]Metrics{"plain": {"unjudged": 1}}, Runs: []RunRecord{}}
	rep.Limits = limits(rep)
	md := renderMarkdown(rep)
	for _, want := range []string{"real model (`groq/x`)", "Cached answers not judged", "from over.yml", "max_tokens 800", "embeddings are still the local mock"} {
		if !strings.Contains(md, want) {
			t.Errorf("real-model markdown lacks %q", want)
		}
	}
	mock := &Report{Tool: "impactbench", Banner: "MOCK", Arms: []Arm{{Name: "plain"}}, Stack: Stack{SemanticCache: map[string]any{"threshold": 0.8}},
		Workload: WorkloadInfo{PhaseCounts: map[string]int{}}, Median: map[string]Metrics{"plain": {}}, Runs: []RunRecord{}}
	mock.Limits = limits(mock)
	if md := renderMarkdown(mock); strings.Contains(md, "Cached answers not judged") || !strings.Contains(md, "fresh, uncached call to the mock model") {
		t.Error("mock-run markdown must keep its wording")
	}
}
