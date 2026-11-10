package main

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The e2e run (../../e2e) relies on these exact prompts. Bifrost's semantic cache
// embeds a chat request as "<role>: <lowercased, trimmed content>" per message,
// so the tests embed the same strings Bifrost sends.
const (
	e2eBase       = "user: what is the capital city of france?"
	e2eParaphrase = "user: tell me the capital city of france."
	e2eUnrelated  = "user: how do i bake sourdough bread at home?"
)

func l2(v []float64) float64 {
	var s float64
	for _, x := range v {
		s += x * x
	}
	return math.Sqrt(s)
}

func TestEmbedIsDeterministic(t *testing.T) {
	texts := []string{e2eBase, e2eParaphrase, "Héllo wörld 123", "?!", ""}
	for _, dim := range []int{8, 256, 1536} {
		for _, text := range texts {
			a, b := Embed(text, dim), Embed(text, dim)
			if len(a) != dim {
				t.Fatalf("Embed(%q, %d) has %d dims", text, dim, len(a))
			}
			for i := range a {
				if math.Float64bits(a[i]) != math.Float64bits(b[i]) {
					t.Fatalf("Embed(%q, %d) differs at %d: %v vs %v", text, dim, i, a[i], b[i])
				}
			}
		}
	}
	// A fresh server instance must give the same vector too (no per-process seed).
	srvA, srvB := newMockServer(serverConfig{Dimension: 64}), newMockServer(serverConfig{Dimension: 64})
	if Cosine(Embed(e2eBase, srvA.cfg.Dimension), Embed(e2eBase, srvB.cfg.Dimension)) != 1 {
		t.Fatal("two server instances produced different vectors for the same text")
	}
}

func TestEmbedIsL2Normalised(t *testing.T) {
	texts := []string{
		e2eBase, e2eUnrelated,
		"what is it?", // only stopwords: falls back to all tokens
		"?!",          // no letters or digits: falls back to raw characters
		"",            // empty: falls back to a fixed unit vector
		"日本の首都はどこですか", // non-Latin script
		strings.Repeat("caching layers reduce latency ", 200),
	}
	for _, dim := range []int{1, 2, 256, 2000} {
		for _, text := range texts {
			if n := l2(Embed(text, dim)); math.Abs(n-1) > 1e-9 {
				t.Errorf("|Embed(%q, %d)| = %.12f, want 1", text, dim, n)
			}
		}
	}
}

func TestParaphrasesScoreHigherThanUnrelatedText(t *testing.T) {
	cases := []struct{ base, paraphrase, unrelated string }{
		{e2eBase, e2eParaphrase, e2eUnrelated},
		{"How do I reset my router password?", "How can I reset the password of my router?", "Best hiking trails near Denver"},
		{"Summarise the quarterly sales report", "Summarize the sales report for the quarter", "What time does the train to Leeds leave?"},
		{"What are the side effects of ibuprofen?", "Side effects of taking ibuprofen", "Convert 30 degrees Celsius to Fahrenheit"},
		{"Explain how HNSW indexes work in pgvector", "How does an HNSW index work in pgvector?", "Recipe for a chocolate cake"},
	}
	for _, dim := range []int{256, 1536} {
		for _, c := range cases {
			base := Embed(c.base, dim)
			para := Cosine(base, Embed(c.paraphrase, dim))
			unrel := Cosine(base, Embed(c.unrelated, dim))
			t.Logf("dim=%d %-45q paraphrase=%.4f unrelated=%.4f", dim, c.base, para, unrel)
			if para <= unrel {
				t.Errorf("dim=%d: paraphrase similarity %.4f is not above unrelated %.4f for %q", dim, para, unrel, c.base)
			}
		}
	}
}

// TestE2EPromptSimilarities pins the similarities the e2e run expects at the
// default dimension and the default semantic-cache threshold of 0.8.
func TestE2EPromptSimilarities(t *testing.T) {
	const dim, threshold = 256, 0.8
	base := Embed(e2eBase, dim)
	para := Cosine(base, Embed(e2eParaphrase, dim))
	unrel := Cosine(base, Embed(e2eUnrelated, dim))
	t.Logf("paraphrase=%.6f unrelated=%.6f", para, unrel)
	if para < threshold+0.03 {
		t.Errorf("paraphrase similarity %.4f is too close to (or below) the %.2f threshold", para, threshold)
	}
	if para >= 0.999 {
		t.Errorf("paraphrase similarity %.4f is ~1: the e2e would not show a real near-match", para)
	}
	if unrel > 0.5 {
		t.Errorf("unrelated similarity %.4f is too high", unrel)
	}
}

func TestAnswer(t *testing.T) {
	cases := []struct{ name, system, question, want string }{
		{"no system prompt", "", "What is the capital of France?", "Mock answer to: What is the capital of France?"},
		{"system prompt without context lines", "You are a helpful assistant.", "Hi there", "Mock answer to: Hi there"},
		{
			"bullet context, first matching line wins",
			"Answer using the context.\n- Berlin is the capital of Germany.\n- Paris is the capital of France.\n- France borders Spain.",
			"What is the capital of France?",
			"According to the provided information, Berlin is the capital of Germany.",
		},
		{
			"numbered context, match on a later line",
			"Use these facts:\n1. Bread needs flour.\n2) Paris hosts the Louvre.\n[3] The Eiffel Tower is in Paris.",
			"Where is the Eiffel Tower?",
			"According to the provided information, The Eiffel Tower is in Paris.",
		},
		{
			"Context: heading block",
			"You answer questions.\nContext:\nThe office opens at 9am.\nParking is free on weekends.",
			"Is parking free?",
			"According to the provided information, Parking is free on weekends.",
		},
		{
			"inline Context: text",
			"Context: Bifrost stores cache entries in pgvector.",
			"Where are cache entries stored?",
			"According to the provided information, Bifrost stores cache entries in pgvector.",
		},
		{
			"context present but no shared content word",
			"- The office opens at 9am.",
			"How do I bake bread?",
			"The provided information does not answer: How do I bake bread?",
		},
	}
	for _, c := range cases {
		if got := Answer(c.system, c.question); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

// --- HTTP handlers --------------------------------------------------------

func do(t *testing.T, h http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, rec.Body.String())
	}
	return out
}

func stat(t *testing.T, h http.Handler, name string) int {
	t.Helper()
	return int(decode(t, do(t, h, "GET", "/mock/stats", "", nil))[name].(float64))
}

func TestChatCompletionAndCounters(t *testing.T) {
	h := newMockServer(serverConfig{Dimension: 16}).routes()
	body := `{"model":"mock-chat","messages":[{"role":"system","content":"- Paris is the capital of France."},{"role":"user","content":[{"type":"text","text":"Capital of France?"}]}]}`
	rec := do(t, h, "POST", "/v1/chat/completions", body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	out := decode(t, rec)
	if out["object"] != "chat.completion" || out["model"] != "mock-chat" || !strings.HasPrefix(out["id"].(string), "chatcmpl-mock-") {
		t.Fatalf("unexpected envelope: %v", out)
	}
	choice := out["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if msg["content"] != "According to the provided information, Paris is the capital of France." || choice["finish_reason"] != "stop" {
		t.Fatalf("unexpected choice: %v", choice)
	}
	usage := out["usage"].(map[string]any)
	if usage["total_tokens"].(float64) != usage["prompt_tokens"].(float64)+usage["completion_tokens"].(float64) {
		t.Fatalf("usage does not add up: %v", usage)
	}
	if got := stat(t, h, "chat_calls"); got != 1 {
		t.Fatalf("chat_calls = %d, want 1", got)
	}
	// A second call gets a new id; reset zeroes counters but never reuses ids.
	second := decode(t, do(t, h, "POST", "/v1/chat/completions", body, nil))
	if second["id"] == out["id"] {
		t.Fatalf("two calls returned the same id %v", out["id"])
	}
	do(t, h, "POST", "/mock/reset", "", nil)
	if got := stat(t, h, "chat_calls"); got != 0 {
		t.Fatalf("chat_calls after reset = %d, want 0", got)
	}
	third := decode(t, do(t, h, "POST", "/v1/chat/completions", body, nil))
	if third["id"] == out["id"] || third["id"] == second["id"] {
		t.Fatalf("id reused after reset: %v", third["id"])
	}
}

func TestChatValidationAndAuth(t *testing.T) {
	h := newMockServer(serverConfig{Dimension: 16, APIKey: "sk-mock-local"}).routes()
	ok := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	if rec := do(t, h, "POST", "/v1/chat/completions", ok, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing key: status %d, want 401", rec.Code)
	}
	auth := map[string]string{"Authorization": "Bearer sk-mock-local"}
	if rec := do(t, h, "POST", "/v1/chat/completions", ok, auth); rec.Code != http.StatusOK {
		t.Fatalf("right key: status %d: %s", rec.Code, rec.Body.String())
	}
	for _, bad := range []string{`{"messages":[{"role":"user","content":"hi"}]}`, `{"model":"m","messages":[]}`, `not json`, `{"model":"m","n":2,"messages":[{"role":"user","content":"hi"}]}`} {
		if rec := do(t, h, "POST", "/v1/chat/completions", bad, auth); rec.Code != http.StatusBadRequest {
			t.Errorf("body %s: status %d, want 400", bad, rec.Code)
		}
	}
	if got := stat(t, h, "chat_calls"); got != 1 {
		t.Fatalf("chat_calls = %d, want 1 (rejected calls must not count)", got)
	}
	if got := stat(t, h, "rejected_calls"); got != 5 {
		t.Fatalf("rejected_calls = %d, want 5", got)
	}
}

func TestChatStreaming(t *testing.T) {
	h := newMockServer(serverConfig{Dimension: 16}).routes()
	body := `{"model":"mock-chat","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"Hello stream"}]}`
	rec := do(t, h, "POST", "/v1/chat/completions", body, nil)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d content-type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	var content strings.Builder
	var sawFinish, sawUsage, sawDone bool
	scanner := bufio.NewScanner(rec.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			sawDone = true
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("bad chunk %q: %v", data, err)
		}
		if chunk["object"] != "chat.completion.chunk" {
			t.Fatalf("bad object in %v", chunk)
		}
		if u, ok := chunk["usage"]; ok && u != nil {
			sawUsage = true
		}
		for _, c := range chunk["choices"].([]any) {
			choice := c.(map[string]any)
			if delta, ok := choice["delta"].(map[string]any); ok {
				if s, ok := delta["content"].(string); ok {
					content.WriteString(s)
				}
			}
			if choice["finish_reason"] == "stop" {
				sawFinish = true
			}
		}
	}
	if content.String() != "Mock answer to: Hello stream" || !sawFinish || !sawUsage || !sawDone {
		t.Fatalf("content=%q finish=%t usage=%t done=%t", content.String(), sawFinish, sawUsage, sawDone)
	}
	if stat(t, h, "chat_calls") != 1 || stat(t, h, "chat_stream_calls") != 1 {
		t.Fatal("streaming call not counted")
	}
}

func TestEmbeddingsEndpoint(t *testing.T) {
	h := newMockServer(serverConfig{Dimension: 32}).routes()
	rec := do(t, h, "POST", "/v1/embeddings", `{"model":"mock-embedding","input":"`+e2eBase+`"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	out := decode(t, rec)
	data := out["data"].([]any)
	first := data[0].(map[string]any)
	raw := first["embedding"].([]any)
	if out["object"] != "list" || first["object"] != "embedding" || len(raw) != 32 {
		t.Fatalf("unexpected body: object=%v first=%v len=%d", out["object"], first["object"], len(raw))
	}
	got := make([]float64, len(raw))
	for i, v := range raw {
		got[i] = v.(float64)
	}
	if c := Cosine(got, Embed(e2eBase, 32)); math.Abs(c-1) > 1e-12 {
		t.Fatalf("served vector differs from Embed: cosine %v", c)
	}
	if usage := out["usage"].(map[string]any); usage["prompt_tokens"].(float64) <= 0 || usage["total_tokens"].(float64) <= 0 {
		t.Fatalf("missing usage: %v", usage)
	}

	// Batch input, dimensions override and base64 encoding.
	rec = do(t, h, "POST", "/v1/embeddings", `{"model":"m","input":["a cat","a dog"],"dimensions":8,"encoding_format":"base64"}`, nil)
	out = decode(t, rec)
	data = out["data"].([]any)
	if len(data) != 2 {
		t.Fatalf("want 2 embeddings, got %d", len(data))
	}
	b, err := base64.StdEncoding.DecodeString(data[1].(map[string]any)["embedding"].(string))
	if err != nil || len(b) != 8*4 {
		t.Fatalf("bad base64 embedding: %v len=%d", err, len(b))
	}
	want := Embed("a dog", 8)
	for i := 0; i < 8; i++ {
		if v := math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:])); v != float32(want[i]) {
			t.Fatalf("base64 value %d = %v, want %v", i, v, float32(want[i]))
		}
	}

	// Token-id input is accepted; empty input is rejected like OpenAI does.
	if rec := do(t, h, "POST", "/v1/embeddings", `{"model":"m","input":[1,2,3]}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("token input: status %d", rec.Code)
	}
	for _, bad := range []string{`{"model":"m","input":""}`, `{"model":"m","input":[]}`, `{"model":"m"}`, `{"input":"x"}`, `{"model":"m","input":"x","encoding_format":"int8"}`} {
		if rec := do(t, h, "POST", "/v1/embeddings", bad, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("body %s: status %d, want 400", bad, rec.Code)
		}
	}
	if got := stat(t, h, "embedding_calls"); got != 3 {
		t.Fatalf("embedding_calls = %d, want 3", got)
	}
}

func TestRequestLogRecordsInputsAndResponseIDs(t *testing.T) {
	h := newMockServer(serverConfig{Dimension: 8}).routes()
	do(t, h, "POST", "/v1/embeddings", `{"model":"m","input":"`+e2eBase+`"}`, nil)
	chat := decode(t, do(t, h, "POST", "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"Q?"}]}`, nil))
	var log struct {
		Requests []requestRecord `json:"requests"`
	}
	if err := json.Unmarshal(do(t, h, "GET", "/mock/requests", "", nil).Body.Bytes(), &log); err != nil {
		t.Fatal(err)
	}
	if len(log.Requests) != 2 {
		t.Fatalf("want 2 records, got %d", len(log.Requests))
	}
	if r := log.Requests[0]; r.Kind != "embedding" || len(r.Inputs) != 1 || r.Inputs[0] != e2eBase || r.Dimension != 8 {
		t.Fatalf("bad embedding record: %+v", r)
	}
	if r := log.Requests[1]; r.Kind != "chat" || r.Question != "Q?" || r.ResponseID != chat["id"] {
		t.Fatalf("bad chat record: %+v", r)
	}
	var filtered struct {
		Requests []requestRecord `json:"requests"`
	}
	_ = json.Unmarshal(do(t, h, "GET", "/mock/requests?kind=chat", "", nil).Body.Bytes(), &filtered)
	if len(filtered.Requests) != 1 || filtered.Requests[0].Kind != "chat" {
		t.Fatalf("kind filter failed: %+v", filtered.Requests)
	}
}

func TestModelsAndUnknownRoutes(t *testing.T) {
	h := newMockServer(serverConfig{Dimension: 8, Models: []string{"a", "b"}}).routes()
	out := decode(t, do(t, h, "GET", "/v1/models", "", nil))
	if data := out["data"].([]any); len(data) != 2 || data[0].(map[string]any)["id"] != "a" {
		t.Fatalf("unexpected models: %v", out)
	}
	if rec := do(t, h, "GET", "/v1/nope", "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown route: status %d", rec.Code)
	}
	if stat(t, h, "unknown_calls") != 1 {
		t.Fatal("unknown call not counted")
	}
	sim := decode(t, do(t, h, "POST", "/mock/similarity", `{"a":"`+e2eBase+`","b":"`+e2eBase+`"}`, nil))
	if c := sim["cosine"].(float64); math.Abs(c-1) > 1e-12 {
		t.Fatalf("self similarity = %v", c)
	}
}

// --- Knowledge file -------------------------------------------------------

func mustParseKnowledge(t *testing.T, file string) []knowledgeRule {
	t.Helper()
	rules, err := parseKnowledge([]byte(file))
	if err != nil {
		t.Fatalf("parseKnowledge: %v", err)
	}
	return rules
}

func TestKnowledgeMatching(t *testing.T) {
	rules := mustParseKnowledge(t, `{"rules": [
		{"name": "2fa-on", "all": ["Two", " factor "], "any": ["on", "enable"], "none": ["off"], "answer": "on"},
		{"name": "2fa-off", "all": ["two", "factor"], "any": ["off"], "answer": "off"},
		{"name": "csv", "all": ["csv"], "answer": "csv"},
		{"name": "csv-export", "all": ["csv", "export"], "answer": "never chosen: csv comes first"}
	]}`)
	cases := []struct{ question, want string }{
		{"How do I turn on two factor login?", "2fa-on"},
		{"How do I ENABLE two-factor login?", "2fa-on"},      // another "any" word; case and the hyphen do not matter
		{"How do I turn off two factor login?", "2fa-off"},   // "on" and "off" are stopwords, but rules see every token
		{"Should two factor login be on or off?", "2fa-off"}, // a "none" word skips the first rule
		{"How do I set up two factor login?", ""},            // no "any" word
		{"How do I turn on factor login?", ""},               // an "all" word is missing
		{"Can I export my data to CSV?", "csv"},              // file order: the first match wins
	}
	for _, c := range cases {
		rule, ok := matchKnowledge(rules, c.question)
		if rule.Name != c.want || ok != (c.want != "") {
			t.Errorf("%q: matched %q (ok=%t), want %q", c.question, rule.Name, ok, c.want)
		}
	}
}

func TestAnswerWithKnowledge(t *testing.T) {
	rules := mustParseKnowledge(t, `{"rules": [{"name": "capital", "all": ["capital", "france"], "answer": "Paris is the capital of France."}]}`)
	cases := []struct{ name, system, question, want, rule string }{
		{"rule matches", "", "What is the capital of France?", "Paris is the capital of France.", "capital"},
		{"system prompt without context lines", "You are a helpful assistant.", "Capital of France?", "Paris is the capital of France.", "capital"},
		{
			"context lines beat rules",
			"- Berlin is the capital of Germany.",
			"What is the capital of France?",
			"According to the provided information, Berlin is the capital of Germany.",
			"",
		},
		{
			"context lines beat rules even when they do not answer",
			"Context: The office opens at 9am.",
			"What is the capital of France?",
			"The provided information does not answer: What is the capital of France?",
			"",
		},
		{"no rule matches: echo", "", "How do I bake bread?", "Mock answer to: How do I bake bread?", ""},
	}
	for _, c := range cases {
		answer, rule := answerWithKnowledge(rules, c.system, c.question)
		if answer != c.want || rule != c.rule {
			t.Errorf("%s:\n got %q (rule %q)\nwant %q (rule %q)", c.name, answer, rule, c.want, c.rule)
		}
		// Without rules the reply is exactly Answer's.
		if answer, rule := answerWithKnowledge(nil, c.system, c.question); answer != Answer(c.system, c.question) || rule != "" {
			t.Errorf("%s: without rules got %q (rule %q), want %q", c.name, answer, rule, Answer(c.system, c.question))
		}
	}
}

func TestParseKnowledgeRejectsBadFiles(t *testing.T) {
	cases := []struct{ name, file, want string }{
		{"not JSON", `{"rules": [`, "invalid knowledge JSON"},
		{"unknown top-level field", `{"rule": [{"name": "a", "all": ["x"], "answer": "y"}]}`, `unknown field "rule"`},
		{"unknown rule field", `{"rules": [{"name": "a", "al": ["x"], "answer": "y"}]}`, `unknown field "al"`},
		{"trailing data", `{"rules": [{"name": "a", "all": ["x"], "answer": "y"}]} {}`, "unexpected data after the top-level object"},
		{"no rules", `{"_comment": "nothing here"}`, "no rules"},
		{"missing name", `{"rules": [{"all": ["x"], "answer": "y"}]}`, "rules[0]: name is required"},
		{"duplicate name", `{"rules": [{"name": "a", "all": ["x"], "answer": "y"}, {"name": " a ", "all": ["z"], "answer": "w"}]}`, `rule "a" is defined twice`},
		{"blank answer", `{"rules": [{"name": "a", "all": ["x"], "answer": " \n "}]}`, `rule "a": answer is required`},
		{"empty rule", `{"rules": [{"name": "a", "none": ["x"], "answer": "y"}]}`, `rule "a": at least one word in "all" or "any" is required`},
		{"multi-word token", `{"rules": [{"name": "a", "all": ["two factor"], "answer": "y"}]}`, `word "two factor" in "all" is not a single token`},
		{"punctuation in a word", `{"rules": [{"name": "a", "any": ["what's"], "answer": "y"}]}`, `word "what's" in "any" is not a single token`},
		{"blank word", `{"rules": [{"name": "a", "all": ["x"], "none": [" "], "answer": "y"}]}`, `word " " in "none" is not a single token`},
		{"word both required and excluded", `{"rules": [{"name": "a", "all": ["on"], "none": ["ON"], "answer": "y"}]}`, `word "on" is in both "all" and "none"`},
	}
	for _, c := range cases {
		if _, err := parseKnowledge([]byte(c.file)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %v, want it to contain %q", c.name, err, c.want)
		}
	}

	// "_comment" is allowed at the top level and on each rule; names and words are
	// trimmed, words are lowercased, and the answer is kept verbatim.
	rules := mustParseKnowledge(t, `{"_comment": "c", "rules": [{"_comment": "c", "name": " a ", "all": [" CSV "], "answer": " Yes. "}]}`)
	if r := rules[0]; r.Name != "a" || len(r.All) != 1 || r.All[0] != "csv" || r.Answer != " Yes. " {
		t.Fatalf("unexpected rule: %+v", r)
	}

	// loadKnowledge names the file in validation errors.
	dir := t.TempDir()
	path := filepath.Join(dir, "knowledge.json")
	if err := os.WriteFile(path, []byte(`{"rules": []}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadKnowledge(path); err == nil || !strings.Contains(err.Error(), path+": no rules") {
		t.Errorf("loadKnowledge: error %v, want it to start with %q", err, path+": no rules")
	}
	if _, err := loadKnowledge(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("loadKnowledge of a missing file: no error")
	}
}

// TestDemoKnowledgeFile pins ../../docker/mock-knowledge.json, the knowledge file of
// the dashboard's wrong-match demo: questions that mean the same thing get the
// identical answer, and look-alike questions that mean different things get
// different answers, however close their mock embeddings are (logged below).
func TestDemoKnowledgeFile(t *testing.T) {
	rules, err := loadKnowledge("../../docker/mock-knowledge.json")
	if err != nil {
		t.Fatal(err)
	}
	const (
		twoFactorOn   = "To turn on two-factor login, open Settings > Security > Two-factor login and tap Enable, then scan the QR code with an authenticator app."
		twoFactorOff  = "To turn off two-factor login, open Settings > Security > Two-factor login and tap Disable. You must confirm with a current code, and your account becomes less secure."
		resetPassword = "To reset your Acme password, open Settings > Security > Reset password and follow the link we email you."
		username      = "Acme usernames cannot be changed in the mobile app. Contact support to request a new username."
		exportCSV     = "Yes. Open the project, choose Export > CSV, and Acme emails you a download link within a few minutes."
		importCSV     = "Yes. Open the project, choose Import > CSV, then map each column to a field before the import starts."
		boiling       = "Water boils at 100 degrees Celsius (212 degrees Fahrenheit) at sea level."
		mobyDick      = "Herman Melville wrote Moby-Dick, first published in 1851."
		gold          = "The chemical symbol for gold is Au."
		everest       = "Mount Everest is the tallest mountain on Earth, at 8,849 metres above sea level."
	)
	pairs := []struct{ a, b, wantA, wantB string }{
		// Look-alikes that mean different things: different answers.
		{"How do I turn on two factor login for my Acme account?", "How do I turn off two factor login for my Acme account?", twoFactorOn, twoFactorOff},
		{"How do I reset my Acme account password from the mobile app?", "How do I reset my Acme account username from the mobile app?", resetPassword, username},
		{"Can I export my Acme project data to a CSV file?", "Can I import my Acme project data from a CSV file?", exportCSV, importCSV},
		// Questions that mean the same thing: the identical answer.
		{"What is the boiling point of water in Celsius?", "What is the boiling point of water in degrees Celsius?", boiling, boiling},
		{"Who wrote the novel Moby Dick?", "Who is the author who wrote the novel Moby Dick?", mobyDick, mobyDick},
		{"Who wrote the novel Moby Dick?", "Name the author of the novel Moby Dick.", mobyDick, mobyDick},
		{"What is the chemical symbol for gold?", "What's the chemical symbol of gold?", gold, gold},
		{"Name the tallest mountain on Earth.", "Which mountain is the tallest on Earth?", everest, everest},
	}
	for _, p := range pairs {
		gotA, _ := answerWithKnowledge(rules, "", p.a)
		gotB, _ := answerWithKnowledge(rules, "", p.b)
		if gotA != p.wantA {
			t.Errorf("%q:\n got %q\nwant %q", p.a, gotA, p.wantA)
		}
		if gotB != p.wantB {
			t.Errorf("%q:\n got %q\nwant %q", p.b, gotB, p.wantB)
		}
		t.Logf("cosine=%.4f same answer=%-5t %q vs %q", Cosine(Embed(p.a, 256), Embed(p.b, 256)), gotA == gotB, p.a, p.b)
	}
	// Questions outside the demo keep the echo answer.
	for _, q := range []string{
		"How much does the Acme Pro plan cost per month?",
		"Is live chat support currently available?",
		"What is the capital city of France?",
	} {
		if got, rule := answerWithKnowledge(rules, "", q); got != "Mock answer to: "+q || rule != "" {
			t.Errorf("%q: got %q (rule %q), want the echo answer", q, got, rule)
		}
	}
}

// streamedContent joins the delta contents of a streamed chat completion.
func streamedContent(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var content strings.Builder
	scanner := bufio.NewScanner(rec.Body)
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("bad chunk %q: %v", data, err)
		}
		for _, c := range chunk.Choices {
			content.WriteString(c.Delta.Content)
		}
	}
	return content.String()
}

func TestChatUsesKnowledgeRules(t *testing.T) {
	const gold = "The chemical symbol for gold is Au."
	rules := mustParseKnowledge(t, `{"rules": [{"name": "gold-symbol", "all": ["gold", "symbol"], "answer": "`+gold+`"}]}`)
	h := newMockServer(serverConfig{Dimension: 8, Knowledge: rules}).routes()

	out := decode(t, do(t, h, "POST", "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"What is the chemical symbol for gold?"}]}`, nil))
	if msg := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any); msg["content"] != gold {
		t.Fatalf("content = %v, want %q", msg["content"], gold)
	}
	stream := do(t, h, "POST", "/v1/chat/completions", `{"model":"m","stream":true,"messages":[{"role":"user","content":"What's the chemical symbol of gold?"}]}`, nil)
	if got := streamedContent(t, stream); got != gold {
		t.Fatalf("streamed content = %q, want %q", got, gold)
	}
	echo := decode(t, do(t, h, "POST", "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"Q?"}]}`, nil))
	if msg := echo["choices"].([]any)[0].(map[string]any)["message"].(map[string]any); msg["content"] != "Mock answer to: Q?" {
		t.Fatalf("content = %v, want the echo answer", msg["content"])
	}

	// The request log names the rule that answered, and has no "rule" field otherwise.
	body := do(t, h, "GET", "/mock/requests", "", nil).Body.String()
	var log struct {
		Requests []requestRecord `json:"requests"`
	}
	if err := json.Unmarshal([]byte(body), &log); err != nil {
		t.Fatal(err)
	}
	if len(log.Requests) != 3 {
		t.Fatalf("want 3 records, got %d", len(log.Requests))
	}
	for i, want := range []struct{ kind, rule string }{{"chat", "gold-symbol"}, {"chat_stream", "gold-symbol"}, {"chat", ""}} {
		if r := log.Requests[i]; r.Kind != want.kind || r.Rule != want.rule {
			t.Errorf("record %d: kind %q rule %q, want kind %q rule %q", i, r.Kind, r.Rule, want.kind, want.rule)
		}
	}
	if strings.Count(body, `"rule":`) != 2 || strings.Count(body, `"rule":"gold-symbol"`) != 2 {
		t.Fatalf("want \"rule\":\"gold-symbol\" on the two rule-answered records only:\n%s", body)
	}
}
