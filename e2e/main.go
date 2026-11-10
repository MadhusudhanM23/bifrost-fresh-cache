// Command e2e proves the PostgreSQL + pgvector vector store end to end through
// the real bifrost-http gateway: the semantic cache plugin, the OpenAI provider
// pointed at a local mock (no paid API keys), and the pgvector test database.
//
// It starts the mock and the gateway itself (so it can restart the gateway for
// the persistence step), prints PASS/FAIL per step, and writes the console
// output to e2e-run.log. Run it from this directory:
//
//	go run . [flags]
package main

import (
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

type options struct {
	gatewayBin    string
	mockBin       string
	appDir        string
	authProbeDir  string
	gatewayPort   int
	mockPort      int
	authProbePort int
	pgContainer   string
	pgUser        string
	pgDB          string
	fresh         bool
	logPath       string
	procLogDir    string
	mockLatency   time.Duration
	extras        bool
	authProbe     bool
	keep          bool
}

// gatewayConfig is the part of config.json the runner reads to set its expectations.
type gatewayConfig struct {
	Providers struct {
		OpenAI struct {
			NetworkConfig struct {
				BaseURL string `json:"base_url"`
			} `json:"network_config"`
			Keys []struct {
				Value string `json:"value"`
			} `json:"keys"`
		} `json:"openai"`
	} `json:"providers"`
	VectorStore struct {
		Enabled bool           `json:"enabled"`
		Type    string         `json:"type"`
		Config  map[string]any `json:"config"`
	} `json:"vector_store"`
	Plugins []struct {
		Name    string         `json:"name"`
		Enabled bool           `json:"enabled"`
		Config  map[string]any `json:"config"`
	} `json:"plugins"`
}

type runner struct {
	opt     options
	out     io.Writer
	mu      sync.Mutex
	client  *http.Client
	started time.Time

	gwURL, mockURL   string
	gwAddr, mockAddr string
	mock, gw, probe  *proc
	gwRuns           int
	pids             []string

	steps []*step
	cur   *step

	// Expectations read from config.json.
	namespace      string
	threshold      float64
	dimension      int
	embeddingModel string
	mockKey        string
	baseRows       int

	// State carried between steps.
	respA, respB, respC *chatResult
	idA                 string // cache id written by step a
	textA               string // exact text the gateway embedded for the base question
	refreshedRespID     string // completion id written back after clear + re-request
	semanticOnlyID      string // entry written by the semantic-only miss in g2b
}

const (
	qBase       = "What is the capital city of France?"
	qParaphrase = "Tell me the capital city of France."
	qUnrelated  = "How do I bake sourdough bread at home?"
	qAfterBoot  = "Please tell me the capital city of France."
	qNoStore    = "Which planet is closest to the sun?"
	qDirectOnly = "List three uses of PostgreSQL."
	qDirectPara = "List three common uses of PostgreSQL."
	qTTL        = "What time zone is Tokyo in?"
	qStream     = "Explain what pgvector does."
	keyDemo     = "demo"
	keyOther    = "other"
)

func main() {
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	var opt options
	flag.StringVar(&opt.gatewayBin, "gateway-bin", filepath.Join("bin", "bifrost-http"+exe), "bifrost-http binary built from the fork")
	flag.StringVar(&opt.mockBin, "mock-bin", filepath.Join("bin", "mock-openai"+exe), "mock-openai binary")
	flag.StringVar(&opt.appDir, "app-dir", ".", "gateway -app-dir (must contain config.json)")
	flag.StringVar(&opt.authProbeDir, "authprobe-dir", "authprobe", "app dir with the config-store config for the auth probe (x5)")
	flag.IntVar(&opt.gatewayPort, "gateway-port", 18090, "gateway port")
	flag.IntVar(&opt.mockPort, "mock-port", 18080, "mock-openai port (must match providers.openai.network_config.base_url)")
	flag.IntVar(&opt.authProbePort, "authprobe-port", 18091, "port for the auth-probe gateway (x5)")
	flag.StringVar(&opt.pgContainer, "pg-container", "bifrost-pgvector-test", "pgvector Docker container used for psql")
	flag.StringVar(&opt.pgUser, "pg-user", "bifrost", "PostgreSQL user for psql")
	flag.StringVar(&opt.pgDB, "pg-db", "bifrost", "PostgreSQL database holding the cache tables")
	flag.BoolVar(&opt.fresh, "fresh", true, "drop the cache namespace table before starting, so row counts start at 0")
	flag.StringVar(&opt.logPath, "log", "e2e-run.log", "file that receives a copy of the console output")
	flag.StringVar(&opt.procLogDir, "proc-log-dir", "logs", "directory for the mock and gateway process logs")
	flag.DurationVar(&opt.mockLatency, "mock-chat-latency", 100*time.Millisecond, "artificial upstream latency of the mock chat endpoint")
	flag.BoolVar(&opt.extras, "extras", true, "run the extra checks x1-x4 and x6")
	flag.BoolVar(&opt.authProbe, "authprobe", true, "run the config-store auth probe x5 (creates and drops a scratch database)")
	flag.BoolVar(&opt.keep, "keep", false, "leave the mock and gateway running at the end")
	flag.Parse()
	os.Exit(run(opt))
}

func run(opt options) int {
	// Child processes run in their app dir, so binaries must not be relative paths.
	for _, p := range []*string{&opt.gatewayBin, &opt.mockBin} {
		if abs, err := filepath.Abs(*p); err == nil {
			*p = abs
		}
	}
	logFile, err := os.Create(opt.logPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot create %s: %v\n", opt.logPath, err)
		return 2
	}
	defer logFile.Close()
	r := &runner{
		opt:     opt,
		out:     &teeWriter{ws: []io.Writer{os.Stdout, logFile}},
		client:  &http.Client{Timeout: 60 * time.Second},
		started: time.Now(),
	}
	r.gwAddr = net.JoinHostPort("127.0.0.1", strconv.Itoa(opt.gatewayPort))
	r.mockAddr = net.JoinHostPort("127.0.0.1", strconv.Itoa(opt.mockPort))
	r.gwURL = "http://" + r.gwAddr
	r.mockURL = "http://" + r.mockAddr

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	go func() {
		<-interrupt
		r.printf("\ninterrupted: stopping child processes\n")
		r.stopAll(true)
		os.Exit(130)
	}()

	r.printf("Bifrost semantic cache + pgvector end-to-end run\n")
	r.printf("started:  %s\n", r.started.Format(time.RFC3339))
	for _, f := range []string{opt.gatewayBin, opt.mockBin, filepath.Join(opt.appDir, "config.json")} {
		if st, err := os.Stat(f); err == nil {
			r.printf("file:     %s (%d bytes, modified %s)\n", f, st.Size(), st.ModTime().Format(time.RFC3339))
		} else {
			r.printf("file:     %s MISSING (%v)\n", f, err)
		}
	}
	for _, src := range []string{"pgvector.go", "store.go"} {
		path := filepath.Join("..", "bifrost", "framework", "vectorstore", src)
		if data, err := os.ReadFile(path); err == nil {
			st, _ := os.Stat(path)
			r.printf("source:   %s sha256 %x (modified %s; the binary must be newer than this to contain it)\n", path, sha256.Sum256(data), st.ModTime().Format(time.RFC3339))
		}
	}
	r.printf("gateway:  %s   mock: %s   postgres: docker container %s, db %s\n", r.gwURL, r.mockURL, opt.pgContainer, opt.pgDB)

	steps := []func() error{
		r.stepSetup, r.stepA, r.stepB, r.stepC, r.stepD, r.stepE, r.stepF, r.stepG, r.stepH, r.stepI,
	}
	if opt.extras {
		steps = append(steps, r.stepX1DirectOnly, r.stepX2TTL, r.stepX3Stream, r.stepX4OpenAIRoute, r.stepX6QueryPlan)
	}
	if opt.authProbe {
		steps = append(steps, r.stepX5AuthProbe)
	}
	defer func() {
		if rec := recover(); rec != nil {
			r.stopAll(true)
			panic(rec)
		}
	}()
	for _, fn := range steps {
		if err := fn(); err != nil {
			if r.cur != nil && !r.cur.ended {
				r.cur.aborted = true
				r.cur.fails = append(r.cur.fails, err.Error())
				r.end()
			}
			r.printf("\nABORTING the remaining steps: %v\n", err)
			break
		}
	}
	r.stopAll(false)
	return r.summary()
}

func (r *runner) stopAll(force bool) {
	if r.opt.keep && !force {
		r.printf("\n-keep set: leaving processes running (%s)\n", strings.Join(r.pids, ", "))
		return
	}
	r.probe.stop()
	r.gw.stop()
	r.mock.stop()
	r.printf("\nstopped processes: %s\n", strings.Join(r.pids, ", "))
	for _, addr := range []string{r.gwAddr, r.mockAddr, net.JoinHostPort("127.0.0.1", strconv.Itoa(r.opt.authProbePort))} {
		r.printf("port %s still listening: %t\n", addr, portInUse(addr))
	}
}

func (r *runner) summary() int {
	r.printf("\n==============================================================================\n")
	r.printf("SUMMARY  (%s, %.1fs)\n", time.Now().Format(time.RFC3339), time.Since(r.started).Seconds())
	r.printf("==============================================================================\n")
	r.printf("%-6s %-6s %-9s %s\n", "STEP", "RESULT", "KIND", "TITLE / KEY OBSERVATIONS")
	requiredFailed, extraFailed := 0, 0
	for _, s := range r.steps {
		result := "PASS"
		if !s.passed() {
			result = "FAIL"
			if s.required {
				requiredFailed++
			} else {
				extraFailed++
			}
		}
		kind := "required"
		if !s.required {
			kind = "extra"
		}
		r.printf("%-6s %-6s %-9s %s\n", s.id, result, kind, s.title)
		if len(s.summary) > 0 {
			r.printf("%-23s %s\n", "", strings.Join(s.summary, "; "))
		}
		for _, f := range s.fails {
			r.printf("%-23s FAILED: %s\n", "", f)
		}
	}
	r.printf("\nrequired steps failed: %d, extra steps failed: %d, steps run: %d\n", requiredFailed, extraFailed, len(r.steps))
	r.printf("console log: %s; process logs: %s\n", r.opt.logPath, r.opt.procLogDir)
	if requiredFailed > 0 {
		r.printf("OVERALL: FAIL\n")
		return 1
	}
	r.printf("OVERALL: PASS\n")
	return 0
}

// readConfig loads the expectations from config.json.
func (r *runner) readConfig() error {
	path := filepath.Join(r.opt.appDir, "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var cfg gatewayConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	r.namespace, r.threshold, r.dimension = "BifrostSemanticCachePlugin", 0.8, 0
	found := false
	for _, p := range cfg.Plugins {
		if p.Name != "semantic_cache" || !p.Enabled {
			continue
		}
		found = true
		if v, ok := p.Config["vector_store_namespace"].(string); ok && v != "" {
			r.namespace = v
		}
		if v, ok := p.Config["threshold"].(float64); ok && v > 0 {
			r.threshold = v
		}
		if v, ok := p.Config["dimension"].(float64); ok {
			r.dimension = int(v)
		}
		r.embeddingModel, _ = p.Config["embedding_model"].(string)
	}
	if !found {
		return fmt.Errorf("%s has no enabled semantic_cache plugin", path)
	}
	if !identPattern.MatchString(r.namespace) {
		return fmt.Errorf("namespace %q is not a plain identifier", r.namespace)
	}
	if cfg.VectorStore.Type != "pgvector" || !cfg.VectorStore.Enabled {
		return fmt.Errorf("%s: vector_store must be enabled with type pgvector (got %q, enabled=%t)", path, cfg.VectorStore.Type, cfg.VectorStore.Enabled)
	}
	if got := strings.TrimRight(cfg.Providers.OpenAI.NetworkConfig.BaseURL, "/"); got != r.mockURL {
		return fmt.Errorf("%s: providers.openai.network_config.base_url is %q but the mock runs at %q", path, got, r.mockURL)
	}
	if len(cfg.Providers.OpenAI.Keys) > 0 {
		r.mockKey = cfg.Providers.OpenAI.Keys[0].Value
	}
	r.printf("  config.json: vector_store.type=%s namespace=%s dimension=%d threshold=%v embedding_model=%s openai.base_url=%s\n",
		cfg.VectorStore.Type, r.namespace, r.dimension, r.threshold, r.embeddingModel, cfg.Providers.OpenAI.NetworkConfig.BaseURL)
	return nil
}

func (r *runner) startMock() error {
	if err := os.MkdirAll(r.opt.procLogDir, 0o755); err != nil {
		return err
	}
	args := []string{"-host", "127.0.0.1", "-port", strconv.Itoa(r.opt.mockPort), "-dim", strconv.Itoa(r.dimension),
		"-api-key", r.mockKey, "-chat-latency", r.opt.mockLatency.String()}
	p, err := startProc("mock-openai", r.opt.mockBin, args, ".", filepath.Join(r.opt.procLogDir, "mock-openai.log"))
	if err != nil {
		return err
	}
	r.mock = p
	r.pids = append(r.pids, fmt.Sprintf("mock-openai pid %d on %s", p.pid(), r.mockAddr))
	r.printf("  started mock-openai pid=%d: %s %s\n", p.pid(), r.opt.mockBin, strings.Join(args, " "))
	return r.waitHealthy(p, r.mockURL+"/health", 20*time.Second)
}

func (r *runner) startGateway() error {
	r.gwRuns++
	appDir, err := filepath.Abs(r.opt.appDir)
	if err != nil {
		return err
	}
	args := []string{"-app-dir", appDir, "-port", strconv.Itoa(r.opt.gatewayPort), "-host", "127.0.0.1", "-log-level", "info", "-log-style", "pretty"}
	logPath := filepath.Join(r.opt.procLogDir, fmt.Sprintf("gateway-run%d.log", r.gwRuns))
	p, err := startProc("bifrost-http", r.opt.gatewayBin, args, appDir, logPath)
	if err != nil {
		return err
	}
	r.gw = p
	r.pids = append(r.pids, fmt.Sprintf("bifrost-http run %d pid %d on %s", r.gwRuns, p.pid(), r.gwAddr))
	r.printf("  started bifrost-http (run %d) pid=%d: %s %s\n  gateway log: %s\n", r.gwRuns, p.pid(), r.opt.gatewayBin, strings.Join(args, " "), logPath)
	start := time.Now()
	if err := r.waitHealthy(p, r.gwURL+"/health", 120*time.Second); err != nil {
		return err
	}
	r.printf("  gateway healthy after %.1fs\n", time.Since(start).Seconds())
	return nil
}

// gatewayLogLines returns the log lines of the current gateway run that contain any needle.
func gatewayLogLines(path string, needles ...string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		for _, n := range needles {
			if strings.Contains(line, n) {
				out = append(out, strings.TrimRight(line, "\r"))
				break
			}
		}
	}
	return out
}
