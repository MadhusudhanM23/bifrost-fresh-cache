// Command mock-openai is a small OpenAI-compatible HTTP server for local,
// key-free testing of Bifrost. It answers chat completions deterministically
// and returns deterministic hashing embeddings (not a semantic model).
//
// Endpoints:
//
//	POST /v1/chat/completions   OpenAI chat completion (stream:true is supported as SSE)
//	POST /v1/embeddings         OpenAI embeddings (float or base64)
//	GET  /v1/models             model list
//	GET  /v1/models/{id}        one model
//	GET  /mock/stats            call counters
//	POST /mock/reset            zero the counters and clear the request log
//	GET  /mock/requests         recent requests (newest last)
//	POST /mock/similarity       {"a": "...", "b": "..."} -> cosine of the mock embeddings
//	GET  /health                liveness
package main

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const maxDimension = 16000

type serverConfig struct {
	Dimension   int
	APIKey      string        // when set, /v1/* requires "Authorization: Bearer <APIKey>"
	ChatLatency time.Duration // artificial delay before each chat completion
	Models      []string
	Knowledge   []knowledgeRule // canned chat answers from -knowledge (none: off)
}

type requestRecord struct {
	Seq        int64     `json:"seq"`
	At         time.Time `json:"at"`
	Method     string    `json:"method"`
	Path       string    `json:"path"`
	Kind       string    `json:"kind"`
	Model      string    `json:"model,omitempty"`
	Status     int       `json:"status"`
	ResponseID string    `json:"response_id,omitempty"` // chat: the id of the completion returned
	Question   string    `json:"question,omitempty"`    // chat: the last user message
	Rule       string    `json:"rule,omitempty"`        // chat: the knowledge rule that answered, if any
	Inputs     []string  `json:"inputs,omitempty"`      // embeddings: the exact input texts
	Dimension  int       `json:"dimension,omitempty"`   // embeddings: dimension served
	Detail     string    `json:"detail,omitempty"`      // rejections and unknown routes
}

type mockServer struct {
	cfg     serverConfig
	started time.Time

	chatCalls       atomic.Int64
	chatStreamCalls atomic.Int64
	embeddingCalls  atomic.Int64
	embeddingInputs atomic.Int64
	modelsCalls     atomic.Int64
	rejectedCalls   atomic.Int64
	unknownCalls    atomic.Int64

	// responseSeq numbers chat completion ids. It is never reset, so every
	// response id is unique for the life of the process.
	responseSeq atomic.Int64

	mu      sync.Mutex
	recent  []requestRecord
	nextSeq int64
}

const recentLimit = 200

func newMockServer(cfg serverConfig) *mockServer {
	if cfg.Dimension <= 0 {
		cfg.Dimension = 256
	}
	if len(cfg.Models) == 0 {
		cfg.Models = []string{"mock-chat", "mock-embedding"}
	}
	return &mockServer{cfg: cfg, started: time.Now().UTC()}
}

func (s *mockServer) record(r *http.Request, kind, model, detail string, status int) {
	s.add(r, requestRecord{Kind: kind, Model: model, Detail: detail, Status: status})
}

func (s *mockServer) add(r *http.Request, rec requestRecord) {
	if len(rec.Detail) > 400 {
		rec.Detail = rec.Detail[:400] + "..."
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextSeq++
	rec.Seq, rec.At, rec.Method, rec.Path = s.nextSeq, time.Now().UTC(), r.Method, r.URL.Path
	s.recent = append(s.recent, rec)
	if len(s.recent) > recentLimit {
		s.recent = s.recent[len(s.recent)-recentLimit:]
	}
}

func (s *mockServer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.handleChat)
	mux.HandleFunc("POST /v1/embeddings", s.handleEmbeddings)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("GET /v1/models/{id}", s.handleModel)
	mux.HandleFunc("GET /mock/stats", s.handleStats)
	mux.HandleFunc("POST /mock/reset", s.handleReset)
	mux.HandleFunc("GET /mock/requests", s.handleRequests)
	mux.HandleFunc("POST /mock/similarity", s.handleSimilarity)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.unknownCalls.Add(1)
		s.record(r, "unknown", "", "", http.StatusNotFound)
		writeError(w, http.StatusNotFound, "invalid_request_error", "unknown_url",
			fmt.Sprintf("mock-openai does not implement %s %s", r.Method, r.URL.Path))
	})
	return mux
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// writeError writes an OpenAI-shaped error body.
func writeError(w http.ResponseWriter, status int, errType, code, message string) {
	body := map[string]any{"error": map[string]any{
		"message": message, "type": errType, "param": nil, "code": nil,
	}}
	if code != "" {
		body["error"].(map[string]any)["code"] = code
	}
	writeJSON(w, status, body)
}

// authorized enforces the optional API key on /v1/* routes.
func (s *mockServer) authorized(w http.ResponseWriter, r *http.Request, kind string) bool {
	if s.cfg.APIKey == "" {
		return true
	}
	if r.Header.Get("Authorization") == "Bearer "+s.cfg.APIKey {
		return true
	}
	s.rejectedCalls.Add(1)
	s.record(r, kind, "", "rejected: missing or wrong API key", http.StatusUnauthorized)
	writeError(w, http.StatusUnauthorized, "invalid_request_error", "invalid_api_key",
		"Incorrect API key provided (mock-openai was started with -api-key).")
	return false
}

func (s *mockServer) reject(w http.ResponseWriter, r *http.Request, kind, model, message string) {
	s.rejectedCalls.Add(1)
	s.record(r, kind, model, "rejected: "+message, http.StatusBadRequest)
	writeError(w, http.StatusBadRequest, "invalid_request_error", "", message)
}

// ---------------------------------------------------------------------------
// Chat completions
// ---------------------------------------------------------------------------

type chatRequest struct {
	Model         string        `json:"model"`
	Messages      []chatMessage `json:"messages"`
	Stream        bool          `json:"stream"`
	N             *int          `json:"n"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func (s *mockServer) handleChat(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r, "chat") {
		return
	}
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.reject(w, r, "chat", "", "request body is not valid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		s.reject(w, r, "chat", "", "you must provide a model parameter")
		return
	}
	if len(req.Messages) == 0 {
		s.reject(w, r, "chat", req.Model, "'messages' must contain at least one message")
		return
	}
	if req.N != nil && *req.N > 1 {
		s.reject(w, r, "chat", req.Model, "mock-openai only supports n=1")
		return
	}

	system, question := splitConversation(req.Messages)
	answer, rule := answerWithKnowledge(s.cfg.Knowledge, system, question)

	promptTokens := 0
	for _, m := range req.Messages {
		promptTokens += countTokens(messageText(m.Content))
	}
	usage := chatUsage{PromptTokens: promptTokens, CompletionTokens: countTokens(answer)}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens

	if s.cfg.ChatLatency > 0 {
		time.Sleep(s.cfg.ChatLatency)
	}

	id := fmt.Sprintf("chatcmpl-mock-%06d", s.responseSeq.Add(1))
	created := time.Now().Unix()
	s.chatCalls.Add(1)

	if req.Stream {
		s.chatStreamCalls.Add(1)
		s.add(r, requestRecord{Kind: "chat_stream", Model: req.Model, Status: http.StatusOK, ResponseID: id, Question: question, Rule: rule})
		includeUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage
		s.streamChat(w, id, created, req.Model, answer, usage, includeUsage)
		return
	}

	s.add(r, requestRecord{Kind: "chat", Model: req.Model, Status: http.StatusOK, ResponseID: id, Question: question, Rule: rule})
	writeJSON(w, http.StatusOK, map[string]any{
		"id":                 id,
		"object":             "chat.completion",
		"created":            created,
		"model":              req.Model,
		"system_fingerprint": "fp_mock",
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": answer, "refusal": nil},
			"logprobs":      nil,
			"finish_reason": "stop",
		}},
		"usage": usage,
	})
}

// streamChat writes the answer as OpenAI chat.completion.chunk SSE events:
// a role chunk, one chunk per word, a finish chunk, an optional usage chunk,
// then "data: [DONE]".
func (s *mockServer) streamChat(w http.ResponseWriter, id string, created int64, model, answer string, usage chatUsage, includeUsage bool) {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	send := func(v any) {
		data, _ := json.Marshal(v)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		if flusher != nil {
			flusher.Flush()
		}
	}
	chunk := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
			"system_fingerprint": "fp_mock",
			"choices": []any{map[string]any{
				"index": 0, "delta": delta, "logprobs": nil, "finish_reason": finish,
			}},
		}
	}

	send(chunk(map[string]any{"role": "assistant", "content": ""}, nil))
	words := strings.Fields(answer)
	for i, word := range words {
		if i < len(words)-1 {
			word += " "
		}
		send(chunk(map[string]any{"content": word}, nil))
	}
	send(chunk(map[string]any{}, "stop"))
	if includeUsage {
		send(map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
			"system_fingerprint": "fp_mock", "choices": []any{}, "usage": usage,
		})
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// ---------------------------------------------------------------------------
// Embeddings
// ---------------------------------------------------------------------------

type embeddingRequest struct {
	Model          string          `json:"model"`
	Input          json.RawMessage `json:"input"`
	EncodingFormat string          `json:"encoding_format"`
	Dimensions     *int            `json:"dimensions"`
}

// parseEmbeddingInput accepts the OpenAI input shapes: a string, an array of
// strings, an array of token ids, or an array of token-id arrays. Token ids are
// embedded as the words "tok<id>".
func parseEmbeddingInput(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, errors.New("'input' is required")
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return []string{single}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		return many, nil
	}
	tokensToText := func(ids []int) string {
		words := make([]string, len(ids))
		for i, id := range ids {
			words[i] = "tok" + strconv.Itoa(id)
		}
		return strings.Join(words, " ")
	}
	var ids []int
	if err := json.Unmarshal(raw, &ids); err == nil {
		return []string{tokensToText(ids)}, nil
	}
	var idLists [][]int
	if err := json.Unmarshal(raw, &idLists); err == nil {
		out := make([]string, len(idLists))
		for i, l := range idLists {
			out[i] = tokensToText(l)
		}
		return out, nil
	}
	return nil, errors.New("'input' must be a string, an array of strings, or an array of token arrays")
}

func encodeBase64Float32(vec []float64) string {
	buf := make([]byte, 4*len(vec))
	for i, v := range vec {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(float32(v)))
	}
	return base64.StdEncoding.EncodeToString(buf)
}

func (s *mockServer) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r, "embedding") {
		return
	}
	var req embeddingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.reject(w, r, "embedding", "", "request body is not valid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		s.reject(w, r, "embedding", "", "you must provide a model parameter")
		return
	}
	inputs, err := parseEmbeddingInput(req.Input)
	if err != nil {
		s.reject(w, r, "embedding", req.Model, err.Error())
		return
	}
	if len(inputs) == 0 {
		s.reject(w, r, "embedding", req.Model, "'input' must not be empty")
		return
	}
	for i, in := range inputs {
		if strings.TrimSpace(in) == "" {
			s.reject(w, r, "embedding", req.Model, fmt.Sprintf("'input[%d]' must not be empty", i))
			return
		}
	}
	dim := s.cfg.Dimension
	if req.Dimensions != nil {
		if *req.Dimensions <= 0 || *req.Dimensions > maxDimension {
			s.reject(w, r, "embedding", req.Model, fmt.Sprintf("'dimensions' must be between 1 and %d", maxDimension))
			return
		}
		dim = *req.Dimensions
	}
	format := req.EncodingFormat
	if format == "" {
		format = "float"
	}
	if format != "float" && format != "base64" {
		s.reject(w, r, "embedding", req.Model, "'encoding_format' must be 'float' or 'base64'")
		return
	}

	data := make([]any, len(inputs))
	tokens := 0
	for i, in := range inputs {
		vec := Embed(in, dim)
		tokens += countTokens(in)
		var embedding any = vec
		if format == "base64" {
			embedding = encodeBase64Float32(vec)
		}
		data[i] = map[string]any{"object": "embedding", "index": i, "embedding": embedding}
	}
	s.embeddingCalls.Add(1)
	s.embeddingInputs.Add(int64(len(inputs)))
	s.add(r, requestRecord{Kind: "embedding", Model: req.Model, Status: http.StatusOK, Inputs: inputs, Dimension: dim})
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   data,
		"model":  req.Model,
		"usage":  map[string]any{"prompt_tokens": tokens, "total_tokens": tokens},
	})
}

// ---------------------------------------------------------------------------
// Models
// ---------------------------------------------------------------------------

func (s *mockServer) modelObject(id string) map[string]any {
	return map[string]any{"id": id, "object": "model", "created": s.started.Unix(), "owned_by": "mock-openai"}
}

func (s *mockServer) handleModels(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r, "models") {
		return
	}
	s.modelsCalls.Add(1)
	s.record(r, "models", "", "", http.StatusOK)
	data := make([]any, len(s.cfg.Models))
	for i, m := range s.cfg.Models {
		data[i] = s.modelObject(m)
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *mockServer) handleModel(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r, "models") {
		return
	}
	s.modelsCalls.Add(1)
	id := r.PathValue("id")
	s.record(r, "models", id, "", http.StatusOK)
	writeJSON(w, http.StatusOK, s.modelObject(id))
}

// ---------------------------------------------------------------------------
// Mock control endpoints
// ---------------------------------------------------------------------------

func (s *mockServer) stats() map[string]any {
	return map[string]any{
		"chat_calls":        s.chatCalls.Load(),
		"chat_stream_calls": s.chatStreamCalls.Load(),
		"embedding_calls":   s.embeddingCalls.Load(),
		"embedding_inputs":  s.embeddingInputs.Load(),
		"models_calls":      s.modelsCalls.Load(),
		"rejected_calls":    s.rejectedCalls.Load(),
		"unknown_calls":     s.unknownCalls.Load(),
		"dimension":         s.cfg.Dimension,
		"started_at":        s.started.Format(time.RFC3339),
	}
}

func (s *mockServer) handleStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.stats())
}

func (s *mockServer) handleReset(w http.ResponseWriter, r *http.Request) {
	s.chatCalls.Store(0)
	s.chatStreamCalls.Store(0)
	s.embeddingCalls.Store(0)
	s.embeddingInputs.Store(0)
	s.modelsCalls.Store(0)
	s.rejectedCalls.Store(0)
	s.unknownCalls.Store(0)
	s.mu.Lock()
	s.recent = nil
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, s.stats())
}

func (s *mockServer) handleRequests(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	s.mu.Lock()
	out := make([]requestRecord, 0, len(s.recent))
	for _, rec := range s.recent {
		if kind == "" || rec.Kind == kind {
			out = append(out, rec)
		}
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"requests": out})
}

func (s *mockServer) handleSimilarity(w http.ResponseWriter, r *http.Request) {
	var req struct {
		A   string `json:"a"`
		B   string `json:"b"`
		Dim int    `json:"dim"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "", "body must be {\"a\": \"...\", \"b\": \"...\"}")
		return
	}
	dim := req.Dim
	if dim <= 0 || dim > maxDimension {
		dim = s.cfg.Dimension
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"cosine":          Cosine(Embed(req.A, dim), Embed(req.B, dim)),
		"dim":             dim,
		"a_content_words": contentWords(req.A),
		"b_content_words": contentWords(req.B),
	})
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func envString(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

func envInt(name string, def int) int {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		log.Printf("ignoring %s=%q: not an integer", name, v)
	}
	return def
}

func envDuration(name string, def time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		log.Printf("ignoring %s=%q: not a duration", name, v)
	}
	return def
}

func main() {
	host := flag.String("host", envString("MOCK_OPENAI_HOST", "127.0.0.1"), "address to bind (use 0.0.0.0 to reach the mock from Docker containers) [env MOCK_OPENAI_HOST]")
	port := flag.Int("port", envInt("MOCK_OPENAI_PORT", 18080), "port to listen on [env MOCK_OPENAI_PORT]")
	dim := flag.Int("dim", envInt("MOCK_OPENAI_DIM", 256), "embedding dimension [env MOCK_OPENAI_DIM]")
	apiKey := flag.String("api-key", envString("MOCK_OPENAI_API_KEY", ""), "if set, /v1/* requires 'Authorization: Bearer <api-key>' [env MOCK_OPENAI_API_KEY]")
	chatLatency := flag.Duration("chat-latency", envDuration("MOCK_OPENAI_CHAT_LATENCY", 0), "artificial delay before each chat completion, e.g. 200ms [env MOCK_OPENAI_CHAT_LATENCY]")
	models := flag.String("models", envString("MOCK_OPENAI_MODELS", "mock-chat,mock-embedding"), "comma-separated model ids for GET /v1/models (any model id is accepted on requests) [env MOCK_OPENAI_MODELS]")
	knowledge := flag.String("knowledge", envString("MOCK_OPENAI_KNOWLEDGE", ""), "JSON file of canned chat answers for questions asked without context lines; empty: off [env MOCK_OPENAI_KNOWLEDGE]")
	flag.Parse()

	if *dim <= 0 || *dim > maxDimension {
		log.Fatalf("-dim must be between 1 and %d, got %d", maxDimension, *dim)
	}
	var modelList []string
	for _, m := range strings.Split(*models, ",") {
		if m = strings.TrimSpace(m); m != "" {
			modelList = append(modelList, m)
		}
	}
	var rules []knowledgeRule
	if *knowledge != "" {
		var err error
		rules, err = loadKnowledge(*knowledge)
		if err != nil {
			log.Fatalf("-knowledge: %v", err)
		}
	}

	srv := newMockServer(serverConfig{Dimension: *dim, APIKey: *apiKey, ChatLatency: *chatLatency, Models: modelList, Knowledge: rules})
	addr := net.JoinHostPort(*host, strconv.Itoa(*port))
	log.Printf("mock-openai listening on http://%s (dim=%d, api-key required=%t, chat-latency=%s, models=%v, knowledge rules=%d)",
		addr, *dim, *apiKey != "", *chatLatency, modelList, len(rules))
	httpServer := &http.Server{Addr: addr, Handler: srv.routes(), ReadHeaderTimeout: 10 * time.Second}
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("mock-openai: %v", err)
	}
}
