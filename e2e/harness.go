package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Steps and checks
// ---------------------------------------------------------------------------

type step struct {
	id       string
	title    string
	required bool
	fails    []string
	summary  []string
	aborted  bool
	ended    bool
}

func (s *step) passed() bool { return len(s.fails) == 0 && !s.aborted }

// errAbort stops the run: a precondition for every later step failed.
var errAbort = errors.New("aborted")

func (r *runner) begin(id, title string, required bool) *step {
	s := &step{id: id, title: title, required: required}
	r.steps = append(r.steps, s)
	r.cur = s
	kind := "required"
	if !required {
		kind = "extra"
	}
	r.printf("\n==============================================================================\n")
	r.printf("[%s] %s  (%s)\n", id, title, kind)
	r.printf("==============================================================================\n")
	return s
}

// expect records a check on the current step and prints it.
func (r *runner) expect(ok bool, format string, args ...any) bool {
	msg := fmt.Sprintf(format, args...)
	if ok {
		r.printf("  [ok]   %s\n", msg)
	} else {
		r.printf("  [FAIL] %s\n", msg)
		r.cur.fails = append(r.cur.fails, msg)
	}
	return ok
}

// note adds a key observation to the current step's summary line.
func (r *runner) note(format string, args ...any) {
	r.cur.summary = append(r.cur.summary, fmt.Sprintf(format, args...))
}

func (r *runner) end() {
	s := r.cur
	s.ended = true
	result := "PASS"
	if !s.passed() {
		result = "FAIL"
	}
	r.printf("  ---> [%s] %s: %s\n", s.id, result, strings.Join(s.summary, "; "))
}

func (r *runner) printf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintf(r.out, format, args...)
}

// ---------------------------------------------------------------------------
// Child processes
// ---------------------------------------------------------------------------

type proc struct {
	name    string
	cmd     *exec.Cmd
	done    chan struct{}
	logPath string
}

func (p *proc) pid() int { return p.cmd.Process.Pid }

func (p *proc) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *proc) stop() {
	if p == nil || p.exited() {
		return
	}
	_ = p.cmd.Process.Kill()
	select {
	case <-p.done:
	case <-time.After(15 * time.Second):
	}
}

func startProc(name, bin string, args []string, dir, logPath string) (*proc, error) {
	f, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Stdout = f
	cmd.Stderr = f
	if err := cmd.Start(); err != nil {
		f.Close()
		return nil, fmt.Errorf("start %s: %w", name, err)
	}
	p := &proc{name: name, cmd: cmd, done: make(chan struct{}), logPath: logPath}
	go func() {
		_ = cmd.Wait()
		f.Close()
		close(p.done)
	}()
	return p, nil
}

func portInUse(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// waitHealthy polls url until it answers 200, the process exits, or timeout passes.
func (r *runner) waitHealthy(p *proc, url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		if p.exited() {
			return fmt.Errorf("%s exited during startup (see %s)", p.name, p.logPath)
		}
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("%s did not become healthy at %s within %s (see %s)", p.name, url, timeout, p.logPath)
}

func waitPortClosed(addr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !portInUse(addr) {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// ---------------------------------------------------------------------------
// HTTP calls to the gateway and the mock
// ---------------------------------------------------------------------------

type chatOpts struct {
	base    string // gateway base URL (default: the main gateway)
	path    string // default /v1/chat/completions
	model   string // default openai/mock-chat
	stream  bool
	headers map[string]string
	quiet   bool
}

type chatResult struct {
	Status     int
	Header     http.Header
	Cache      map[string]string // x-bifrost-cache-* headers, lowercase names
	Body       []byte
	JSON       map[string]any
	ID         string
	Content    string
	CacheDebug map[string]any // extra_fields.cache_debug (final chunk for streams)
	HasExtra   bool           // body carried extra_fields
	Elapsed    time.Duration
	Err        error
}

func (c *chatResult) hit() string { return c.Cache["x-bifrost-cache-hit"] }

func (c *chatResult) cacheID() string { return c.Cache["x-bifrost-cache-id"] }

func (c *chatResult) similarity() (float64, bool) {
	v, ok := c.Cache["x-bifrost-cache-similarity"]
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(v, 64)
	return f, err == nil
}

var cacheHeaderOrder = []string{
	"x-bifrost-cache-hit", "x-bifrost-cache-id", "x-bifrost-cache-hit-type",
	"x-bifrost-cache-similarity", "x-bifrost-cache-threshold", "x-bifrost-cache-hit-latency-ms",
	"x-bifrost-cache-provider-used", "x-bifrost-cache-model-used", "x-bifrost-cache-input-tokens",
	"x-bifrost-cache-requested-provider", "x-bifrost-cache-requested-model",
}

func sortedCacheHeaders(m map[string]string) []string {
	rank := map[string]int{}
	for i, h := range cacheHeaderOrder {
		rank[h] = i
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		ri, iok := rank[keys[i]]
		rj, jok := rank[keys[j]]
		switch {
		case iok && jok:
			return ri < rj
		case iok != jok:
			return iok
		default:
			return keys[i] < keys[j]
		}
	})
	return keys
}

func (r *runner) chat(question string, o chatOpts) *chatResult {
	if o.base == "" {
		o.base = r.gwURL
	}
	if o.path == "" {
		o.path = "/v1/chat/completions"
	}
	if o.model == "" {
		o.model = "openai/mock-chat"
	}
	payload := map[string]any{
		"model":    o.model,
		"messages": []any{map[string]any{"role": "user", "content": question}},
	}
	if o.stream {
		payload["stream"] = true
	}
	body, _ := json.Marshal(payload)
	res := &chatResult{Cache: map[string]string{}}
	req, err := http.NewRequest(http.MethodPost, o.base+o.path, bytes.NewReader(body))
	if err != nil {
		res.Err = err
		return res
	}
	req.Header.Set("Content-Type", "application/json")
	var hdrs []string
	for k, v := range o.headers {
		req.Header.Set(k, v)
		hdrs = append(hdrs, k+"="+v)
	}
	sort.Strings(hdrs)
	start := time.Now()
	resp, err := r.client.Do(req)
	if err != nil {
		res.Err = err
		r.printf("  -> POST %s  %s  %q\n  <- ERROR %v\n", o.path, strings.Join(hdrs, "  "), question, err)
		return res
	}
	res.Body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	res.Elapsed = time.Since(start)
	res.Status = resp.StatusCode
	res.Header = resp.Header
	for k, v := range resp.Header {
		if lk := strings.ToLower(k); strings.HasPrefix(lk, "x-bifrost-cache-") && len(v) > 0 {
			res.Cache[lk] = v[0]
		}
	}
	if o.stream {
		parseSSE(res)
	} else if err := json.Unmarshal(res.Body, &res.JSON); err == nil {
		res.ID, _ = res.JSON["id"].(string)
		if choices, ok := res.JSON["choices"].([]any); ok && len(choices) > 0 {
			if c, ok := choices[0].(map[string]any); ok {
				if m, ok := c["message"].(map[string]any); ok {
					res.Content, _ = m["content"].(string)
				}
			}
		}
		if ef, ok := res.JSON["extra_fields"].(map[string]any); ok {
			res.HasExtra = true
			res.CacheDebug, _ = ef["cache_debug"].(map[string]any)
		}
	}
	if !o.quiet {
		stream := ""
		if o.stream {
			stream = " (stream)"
		}
		r.printf("  -> POST %s%s  %s  %q\n", o.path, stream, strings.Join(hdrs, "  "), question)
		r.printf("  <- %d in %d ms  id=%s  content=%q\n", res.Status, res.Elapsed.Milliseconds(), res.ID, res.Content)
		if len(res.Cache) == 0 {
			r.printf("     (no x-bifrost-cache-* headers)\n")
		}
		for _, k := range sortedCacheHeaders(res.Cache) {
			r.printf("     %s: %s\n", k, res.Cache[k])
		}
		if res.Status != http.StatusOK {
			r.printf("     body: %s\n", truncate(string(res.Body), 600))
		}
	}
	return res
}

// parseSSE collects the content, id and final cache_debug of an SSE chat stream.
func parseSSE(res *chatResult) {
	var content strings.Builder
	for _, line := range strings.Split(string(res.Body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if id, ok := chunk["id"].(string); ok && id != "" {
			res.ID = id
		}
		if choices, ok := chunk["choices"].([]any); ok {
			for _, c := range choices {
				if cm, ok := c.(map[string]any); ok {
					if d, ok := cm["delta"].(map[string]any); ok {
						if s, ok := d["content"].(string); ok {
							content.WriteString(s)
						}
					}
				}
			}
		}
		if ef, ok := chunk["extra_fields"].(map[string]any); ok {
			res.HasExtra = true
			if cd, ok := ef["cache_debug"].(map[string]any); ok {
				res.CacheDebug = cd
			}
		}
	}
	res.Content = content.String()
}

type apiResult struct {
	Status int
	Body   string
}

func (r *runner) api(method, url string, headers map[string]string) apiResult {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return apiResult{Status: -1, Body: err.Error()}
	}
	var hdrs []string
	for k, v := range headers {
		req.Header.Set(k, v)
		hdrs = append(hdrs, k+": "+v)
	}
	sort.Strings(hdrs)
	resp, err := r.client.Do(req)
	if err != nil {
		r.printf("  -> %s %s  [%s]\n  <- ERROR %v\n", method, url, strings.Join(hdrs, ", "), err)
		return apiResult{Status: -1, Body: err.Error()}
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	hdrText := "no extra headers"
	if len(hdrs) > 0 {
		hdrText = strings.Join(hdrs, ", ")
	}
	r.printf("  -> %s %s  [%s]\n  <- %d %s\n", method, url, hdrText, resp.StatusCode, strings.TrimSpace(truncate(string(b), 400)))
	return apiResult{Status: resp.StatusCode, Body: string(b)}
}

type mockStats struct {
	Chat, Stream, Embed int64
}

func (r *runner) stats() mockStats {
	resp, err := r.client.Get(r.mockURL + "/mock/stats")
	if err != nil {
		r.printf("  (mock stats unavailable: %v)\n", err)
		return mockStats{-1, -1, -1}
	}
	defer resp.Body.Close()
	var m map[string]float64
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return mockStats{Chat: int64(m["chat_calls"]), Stream: int64(m["chat_stream_calls"]), Embed: int64(m["embedding_calls"])}
}

func (r *runner) printStats(label string, s mockStats) {
	r.printf("  mock %-7s chat_calls=%d (stream=%d) embedding_calls=%d\n", label+":", s.Chat, s.Stream, s.Embed)
}

type mockRecord struct {
	Kind       string   `json:"kind"`
	ResponseID string   `json:"response_id"`
	Question   string   `json:"question"`
	Inputs     []string `json:"inputs"`
}

func (r *runner) mockRequests(kind string) []mockRecord {
	resp, err := r.client.Get(r.mockURL + "/mock/requests?kind=" + kind)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var out struct {
		Requests []mockRecord `json:"requests"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.Requests
}

// lastEmbeddingInput is the exact text the gateway most recently asked the mock to embed.
func (r *runner) lastEmbeddingInput() string {
	recs := r.mockRequests("embedding")
	if len(recs) == 0 || len(recs[len(recs)-1].Inputs) == 0 {
		return ""
	}
	return recs[len(recs)-1].Inputs[0]
}

func (r *runner) mockCosine(a, b string) float64 {
	body, _ := json.Marshal(map[string]string{"a": a, "b": b})
	resp, err := r.client.Post(r.mockURL+"/mock/similarity", "application/json", bytes.NewReader(body))
	if err != nil {
		return -2
	}
	defer resp.Body.Close()
	var out struct {
		Cosine float64 `json:"cosine"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.Cosine
}

// ---------------------------------------------------------------------------
// PostgreSQL through docker exec psql
// ---------------------------------------------------------------------------

var identPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func isUUID(s string) bool { return uuidPattern.MatchString(s) }

func qident(name string) string { return `"` + name + `"` }

// table is the quoted, schema-qualified namespace table.
func (r *runner) table(namespace string) string { return qident("public") + "." + qident(namespace) }

func (r *runner) psqlArgs(db string, extra ...string) []string {
	args := []string{"exec", r.opt.pgContainer, "psql", "-U", r.opt.pgUser, "-d", db, "-v", "ON_ERROR_STOP=1", "-P", "pager=off"}
	return append(args, extra...)
}

// psqlRaw runs one SQL command and returns its output.
func (r *runner) psqlRaw(db, sql string, extra ...string) (string, error) {
	args := r.psqlArgs(db, append(extra, "-c", sql)...)
	out, err := exec.Command("docker", args...).CombinedOutput()
	return strings.TrimRight(string(out), "\r\n "), err
}

// psqlShow runs SQL against the cache database and prints the command and its output.
func (r *runner) psqlShow(sql string) string {
	out, err := r.psqlRaw(r.opt.pgDB, sql)
	r.printf("  psql> %s\n", sql)
	for _, line := range strings.Split(out, "\n") {
		r.printf("        %s\n", strings.TrimRight(line, "\r"))
	}
	if err != nil {
		r.printf("        (psql error: %v)\n", err)
	}
	return out
}

func (r *runner) psqlScalar(sql string) (string, error) {
	out, err := r.psqlRaw(r.opt.pgDB, sql, "-At")
	return strings.TrimSpace(out), err
}

func (r *runner) rowCount(namespace string) int {
	out, err := r.psqlScalar("SELECT count(*) FROM " + r.table(namespace))
	if err != nil {
		return -1
	}
	n, err := strconv.Atoi(out)
	if err != nil {
		return -1
	}
	return n
}

// waitRows polls until the namespace holds want rows (cache writes are asynchronous).
func (r *runner) waitRows(namespace string, want int, timeout time.Duration) int {
	deadline := time.Now().Add(timeout)
	n := r.rowCount(namespace)
	for n != want && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		n = r.rowCount(namespace)
	}
	return n
}

func (r *runner) rowExists(namespace, id string) bool {
	if !isUUID(id) {
		return false
	}
	out, err := r.psqlScalar(fmt.Sprintf("SELECT count(*) FROM %s WHERE id = '%s'", r.table(namespace), id))
	return err == nil && out == "1"
}

// waitRowState polls until the existence of the row with id matches want.
func (r *runner) waitRowState(namespace, id string, want bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	got := r.rowExists(namespace, id)
	for got != want && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		got = r.rowExists(namespace, id)
	}
	return got
}

// rowField returns one SQL expression evaluated on the row with id ("" when missing).
func (r *runner) rowField(namespace, id, expr string) string {
	if !isUUID(id) {
		return ""
	}
	out, err := r.psqlScalar(fmt.Sprintf("SELECT %s FROM %s WHERE id = '%s'", expr, r.table(namespace), id))
	if err != nil {
		return ""
	}
	return out
}

// cachedResponseIDExpr extracts the mock completion id stored in a cache row.
const cachedResponseIDExpr = `coalesce(substring(metadata->>'response' from 'chatcmpl-mock-[0-9]+'), substring((metadata->'stream_chunks')::text from 'chatcmpl-mock-[0-9]+'))`

// showRows prints the namespace table in a readable form.
func (r *runner) showRows(namespace, where string) {
	sql := fmt.Sprintf(`SELECT id, metadata->>'cache_key' AS cache_key, metadata->>'provider' AS provider, metadata->>'model' AS model, coalesce(metadata->>'params_hash', '(none)') AS params_hash, CASE WHEN embedding IS NULL THEN 'NULL' ELSE 'vector(' || vector_dims(embedding) || ')' END AS embedding, (metadata->>'expires_at')::bigint - extract(epoch from now())::bigint AS expires_in_s, jsonb_array_length(coalesce(metadata->'stream_chunks', '[]'::jsonb)) AS stream_chunks, %s AS cached_response_id FROM %s%s ORDER BY metadata->>'cache_key', id`,
		cachedResponseIDExpr, r.table(namespace), where)
	r.psqlShow(sql)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// teeWriter writes to the console and the log file.
type teeWriter struct {
	mu sync.Mutex
	ws []io.Writer
}

func (t *teeWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, w := range t.ws {
		if _, err := w.Write(p); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}
