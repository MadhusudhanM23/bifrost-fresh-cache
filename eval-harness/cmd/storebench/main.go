// Command storebench measures Bifrost vector-store backends through the
// vectorstore.VectorStore interface with a semantic-cache-shaped workload:
// Add (write), GetChunk (direct lookup, hit and miss) and filtered GetNearest
// (limit 1, the plugin's three Equal filters) for queries that should hit,
// should miss, and should be stopped by the filter alone. Every GetNearest
// answer is checked against an exact brute-force answer computed in Go.
//
// See README.md for the method and the exact commands used for the results.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/vectorstore"

	"bifrost-eval-harness/internal/backend"
	"bifrost-eval-harness/internal/envinfo"
	"bifrost-eval-harness/internal/stats"
	"bifrost-eval-harness/internal/workload"
)

type benchConfig struct {
	Label           string   `json:"label"`
	Backends        []string `json:"backends"`
	Dims            []int    `json:"dims"`
	N               int      `json:"entries"`
	Keys            int      `json:"cache_keys"`
	Queries         int      `json:"queries_per_phase"`
	Warmup          int      `json:"warmup_per_phase"`
	Runs            int      `json:"runs"`
	Concurrency     []int    `json:"concurrency"`
	Threshold       float64  `json:"threshold"`
	ResponseBytes   int      `json:"response_bytes"`
	Seed            uint64   `json:"seed"`
	WriteSample     int      `json:"measured_adds_per_phase"`
	LoadConcurrency int      `json:"preload_concurrency"`
	NamespacePrefix string   `json:"namespace_prefix"`
	Ops             []string `json:"query_ops"`
	Keep            bool     `json:"keep_namespaces"`
	PgHost          string   `json:"pg_host"`
	PgPort          int      `json:"pg_port"`
	PgDatabase      string   `json:"pg_database"`
	QdrantAddr      string   `json:"qdrant_addr"`
	RedisAddr       string   `json:"redis_addr,omitempty"`
}

// Op names, in report order.
const (
	opAdd          = "add"
	opChunkHit     = "get_chunk_hit"
	opChunkMiss    = "get_chunk_miss"
	opNearestHit   = "nearest_hit"
	opNearestMiss  = "nearest_miss"
	opNearestCross = "nearest_cross"
)

var allQueryOps = []string{opChunkHit, opChunkMiss, opNearestHit, opNearestMiss, opNearestCross}

var opTitles = map[string]string{
	opAdd:          "Add (write)",
	opChunkHit:     "GetChunk (direct hit)",
	opChunkMiss:    "GetChunk (direct miss)",
	opNearestHit:   "GetNearest (should hit)",
	opNearestMiss:  "GetNearest (should miss)",
	opNearestCross: "GetNearest (cross-filter, should miss)",
}

// Tally is the correctness record of a set of operations.
type Tally struct {
	mu                sync.Mutex
	Executions        int      `json:"executions"`
	ExpectedResult    int      `json:"expected_result"`
	CorrectTop1       int      `json:"correct_top1"`
	MissedResult      int      `json:"missed_result"`
	WrongTop1         int      `json:"wrong_top1"`
	ExpectedEmpty     int      `json:"expected_empty"`
	CorrectEmpty      int      `json:"correct_empty"`
	UnexpectedResult  int      `json:"unexpected_result"`
	FilterViolations  int      `json:"filter_violations"`
	BelowThreshold    int      `json:"below_threshold"`
	UnknownIDs        int      `json:"unknown_ids"`
	MissingFields     int      `json:"missing_selected_fields"`
	MaxScoreAbsError  float64  `json:"max_score_abs_error"`
	RecallAt1         *float64 `json:"recall_at_1,omitempty"`
	FalseResultRate   *float64 `json:"false_result_rate,omitempty"`
	CorrectnessDetail string   `json:"detail,omitempty"`
}

func (t *Tally) merge(o *Tally) {
	if o == nil {
		return
	}
	t.Executions += o.Executions
	t.ExpectedResult += o.ExpectedResult
	t.CorrectTop1 += o.CorrectTop1
	t.MissedResult += o.MissedResult
	t.WrongTop1 += o.WrongTop1
	t.ExpectedEmpty += o.ExpectedEmpty
	t.CorrectEmpty += o.CorrectEmpty
	t.UnexpectedResult += o.UnexpectedResult
	t.FilterViolations += o.FilterViolations
	t.BelowThreshold += o.BelowThreshold
	t.UnknownIDs += o.UnknownIDs
	t.MissingFields += o.MissingFields
	t.MaxScoreAbsError = math.Max(t.MaxScoreAbsError, o.MaxScoreAbsError)
}

func (t *Tally) finish() {
	if t.ExpectedResult > 0 {
		r := float64(t.CorrectTop1) / float64(t.ExpectedResult)
		t.RecallAt1 = &r
	}
	if t.ExpectedEmpty > 0 {
		r := float64(t.UnexpectedResult) / float64(t.ExpectedEmpty)
		t.FalseResultRate = &r
	}
}

// PhaseRecord is one timed phase.
type PhaseRecord struct {
	Op          string        `json:"op"`
	Concurrency int           `json:"concurrency"`
	Run         int           `json:"run"`
	Summary     stats.Summary `json:"summary"`
	FirstError  string        `json:"first_error,omitempty"`
	Correctness *Tally        `json:"correctness,omitempty"`
	// CPUBusyPct is how busy the CPUs of the machine running the client were
	// during the phase (/proc/stat; inside Docker Desktop that is the whole VM,
	// databases included). Absent where /proc/stat does not exist (Windows).
	CPUBusyPct *float64 `json:"vm_cpu_busy_pct,omitempty"`
	Note       string   `json:"note,omitempty"`
}

// MedianRecord combines the runs of one (op, concurrency).
type MedianRecord struct {
	Op              string        `json:"op"`
	Concurrency     int           `json:"concurrency"`
	Runs            int           `json:"runs"`
	Median          stats.Summary `json:"median"`
	P50RangeMs      [2]float64    `json:"p50_range_ms"`
	P99RangeMs      [2]float64    `json:"p99_range_ms"`
	ThroughputRange [2]float64    `json:"throughput_range"`
}

// BackendResult is everything measured for one backend at one dimension.
type BackendResult struct {
	Backend           string            `json:"backend"`
	Dim               int               `json:"dim"`
	Entries           int               `json:"entries"`
	Keys              int               `json:"cache_keys"`
	PartitionSize     int               `json:"entries_per_filter_partition"`
	Namespace         string            `json:"namespace"`
	Server            map[string]any    `json:"server"`
	CreateNamespaceMs float64           `json:"create_namespace_ms"`
	Preload           *PhaseRecord      `json:"preload,omitempty"`
	Ready             string            `json:"ready,omitempty"`
	StoreInfo         map[string]any    `json:"store_info,omitempty"`
	Plans             map[string]string `json:"plans,omitempty"`
	Phases            []PhaseRecord     `json:"phases"`
	Medians           []MedianRecord    `json:"medians"`
	Correctness       map[string]*Tally `json:"correctness"`
	Failure           string            `json:"failure,omitempty"`
	Dropped           bool              `json:"namespace_dropped"`
}

// Report is the results file.
type Report struct {
	Tool        string           `json:"tool"`
	Command     []string         `json:"command"`
	StartedAt   string           `json:"started_at"`
	FinishedAt  string           `json:"finished_at,omitempty"`
	Complete    bool             `json:"complete"`
	Config      benchConfig      `json:"config"`
	Environment envinfo.Env      `json:"environment"`
	Method      []string         `json:"method"`
	Results     []*BackendResult `json:"results"`
}

var method = []string{
	"All timed operations go through vectorstore.VectorStore built by vectorstore.NewVectorStore, the same path the semantic cache plugin uses.",
	"Entries are random unit vectors (uniform on the sphere) with the plugin's metadata keys; entry i belongs to filter partition i mod K (cache_key, params_hash).",
	"CreateNamespace receives the plugin's VectorStoreProperties map. GetNearest uses the plugin's three Equal filters (cache_key, params_hash, from_bifrost_semantic_cache_plugin=true), its non-streaming select fields, limit 1 and the threshold.",
	"Writes: the namespace is preloaded at a fixed concurrency (not part of the medians); then each run adds a fresh slice of entries at each concurrency, so the namespace grows to N while writes are measured and holds exactly N entries for every query phase.",
	"Before queries the backend is brought to a steady state: pgvector VACUUM (ANALYZE); Qdrant waits for collection status green with the optimizer idle.",
	"Each query phase runs warm-up operations (not recorded) and then the measured operations; the same query sets are used for every run, backend and concurrency at a given dimension.",
	"Backends are interleaved: all are loaded side by side, and for every run, op and concurrency the backends' phases run back to back, so outside load affects both backends' measurements alike rather than whichever backend happened to be running.",
	"Where /proc/stat exists (the Linux container) each phase records how busy the CPUs were (Docker VM: client and databases together). WSL2 reports no steal time, so load from Windows processes outside the VM is not visible there; it was sampled separately on the host.",
	"Workers run a closed loop (no think time). Latency is per operation; throughput is successful operations / wall time of the phase. Percentiles are nearest-rank over successful operations.",
	"Medians: each statistic is the median of that statistic across runs (the run-to-run range of p50, p99 and throughput is kept in the JSON).",
	"should-hit queries are unit vectors at a known cosine (uniform between threshold+10% and threshold+90% of the remaining gap) to a stored entry, filtered to that entry's partition. should-miss queries are fresh random vectors. cross-filter queries are near-duplicates of an entry filtered to a different partition, so only the filter prevents a wrong hit.",
	"Every GetNearest answer is compared with an exact brute-force answer (float64 cosine over the query's partition, same threshold). recall@1 = correct top-1 / queries whose exact answer is non-empty; false results = non-empty answers where the exact answer is empty.",
}

func main() {
	var (
		cfg          benchConfig
		backendsFlag = flag.String("backends", "pgvector,qdrant", "comma-separated backends: pgvector, qdrant, redis")
		dimsFlag     = flag.String("dims", "1536,384", "comma-separated vector dimensions")
		concFlag     = flag.String("concurrency", "1,8", "comma-separated concurrency levels")
		opsFlag      = flag.String("ops", strings.Join(allQueryOps, ","), "comma-separated query ops to run")
		seed         = flag.Uint64("seed", 42, "random seed for data and queries")
		outDir       = flag.String("out", "results", "output directory")
		bifrostDir   = flag.String("bifrost-dir", filepath.Join("..", "bifrost"), "Bifrost source tree, fingerprinted into the results")
		containers   = flag.String("containers", "bifrost-pgvector-test,eval-qdrant", "Docker containers to describe (needs the docker CLI)")
		images       = flag.String("container-image", "", "name=image pairs, comma-separated, recorded when the docker CLI is unavailable")
		notes        = flag.String("note", "", "key=value pairs, ';'-separated, recorded in the environment")
		cleanupOnly  = flag.Bool("cleanup-only", false, "drop every namespace starting with -prefix on each backend and exit")
		renderOnly   = flag.String("render", "", "re-render the Markdown of an existing results JSON (written next to it) and exit")
		pgPassword   = flag.String("pg-password", envOr("PGVECTOR_PASSWORD", "bifrost_password"), "PostgreSQL password (default from PGVECTOR_PASSWORD)")
		pgUser       = flag.String("pg-user", "bifrost", "PostgreSQL user")
		pgSSLMode    = flag.String("pg-sslmode", "disable", "PostgreSQL sslmode")
		pgMaxConns   = flag.Int("pg-max-conns", 0, "pgvector pool size (0 = the store's default, 10)")
		qdrantHost   = flag.String("qdrant-host", "localhost", "Qdrant host")
		qdrantPort   = flag.Int("qdrant-port", 6334, "Qdrant gRPC port")
	)
	flag.StringVar(&cfg.Label, "label", "", "label for this run, used in file names (e.g. in-docker, windows-host)")
	flag.IntVar(&cfg.N, "n", 10000, "entries per namespace")
	flag.IntVar(&cfg.Keys, "keys", 20, "cache keys (filter partitions)")
	flag.IntVar(&cfg.Queries, "queries", 1000, "measured operations per query phase")
	flag.IntVar(&cfg.Warmup, "warmup", 100, "warm-up operations before each query phase")
	flag.IntVar(&cfg.Runs, "runs", 3, "repetitions of every measured phase")
	flag.Float64Var(&cfg.Threshold, "threshold", 0.8, "GetNearest similarity threshold")
	flag.IntVar(&cfg.ResponseBytes, "response-bytes", 1024, "size of the stored response payload")
	flag.IntVar(&cfg.WriteSample, "write-sample", 0, "measured adds per (run, concurrency); 0 = N/10")
	flag.IntVar(&cfg.LoadConcurrency, "preload-concurrency", 8, "concurrency of the preload")
	flag.StringVar(&cfg.NamespacePrefix, "prefix", "eval_bench", "namespace prefix (namespaces are <prefix>_<backend>_d<dim>)")
	flag.BoolVar(&cfg.Keep, "keep", false, "keep namespaces after the run")
	flag.StringVar(&cfg.PgHost, "pg-host", "localhost", "PostgreSQL host")
	flag.IntVar(&cfg.PgPort, "pg-port", 5433, "PostgreSQL port")
	flag.StringVar(&cfg.PgDatabase, "pg-db", "bifrost", "PostgreSQL database")
	flag.StringVar(&cfg.RedisAddr, "redis-addr", "localhost:6380", "Redis Stack address (only with -backends redis)")
	flag.Parse()

	if *renderOnly != "" {
		raw, err := os.ReadFile(*renderOnly)
		if err != nil {
			fatal("%v", err)
		}
		var report Report
		if err := json.Unmarshal(raw, &report); err != nil {
			fatal("%s: %v", *renderOnly, err)
		}
		out := strings.TrimSuffix(*renderOnly, ".json") + ".md"
		if err := os.WriteFile(out, []byte(renderMarkdown(&report)), 0o644); err != nil {
			fatal("%v", err)
		}
		logf("wrote %s", out)
		return
	}

	cfg.Seed = *seed
	cfg.Backends = splitList(*backendsFlag)
	cfg.Ops = splitList(*opsFlag)
	cfg.QdrantAddr = fmt.Sprintf("%s:%d", *qdrantHost, *qdrantPort)
	var err error
	if cfg.Dims, err = intList(*dimsFlag); err != nil {
		fatal("bad -dims: %v", err)
	}
	if cfg.Concurrency, err = intList(*concFlag); err != nil {
		fatal("bad -concurrency: %v", err)
	}
	if cfg.WriteSample == 0 {
		cfg.WriteSample = cfg.N / 10
	}
	if cfg.Label == "" {
		cfg.Label = "run"
	}
	if !contains(cfg.Backends, "redis") {
		cfg.RedisAddr = ""
	}
	if cfg.Keys < 2 && contains(cfg.Ops, opNearestCross) {
		// A cross-filter query needs a second partition to point at.
		var ops []string
		for _, op := range cfg.Ops {
			if op != opNearestCross {
				ops = append(ops, op)
			}
		}
		cfg.Ops = ops
		logf("-keys %d: skipping %s (needs at least two cache keys)", cfg.Keys, opNearestCross)
	}

	opts := backend.Options{
		PgHost: cfg.PgHost, PgPort: cfg.PgPort, PgUser: *pgUser, PgPassword: *pgPassword,
		PgDatabase: cfg.PgDatabase, PgSSLMode: *pgSSLMode, PgMaxConns: *pgMaxConns,
		QdrantHost: *qdrantHost, QdrantPort: *qdrantPort, RedisAddr: cfg.RedisAddr,
	}
	logger := bifrost.NewDefaultLogger(schemas.LogLevelError)
	ctx := context.Background()

	if *cleanupOnly {
		cleanup(ctx, cfg, opts, logger)
		return
	}
	if measured := cfg.WriteSample * len(cfg.Concurrency) * cfg.Runs; measured > cfg.N {
		fatal("-n %d is smaller than the %d measured adds (%d per phase x %d concurrencies x %d runs)", cfg.N, measured, cfg.WriteSample, len(cfg.Concurrency), cfg.Runs)
	}

	logf("collecting environment")
	report := &Report{
		Tool:      "storebench",
		Command:   os.Args,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		Config:    cfg,
		Method:    method,
		Environment: envinfo.Collect(envinfo.Options{
			Containers:      splitList(*containers),
			ContainerImages: pairs(*images, ","),
			BifrostDir:      *bifrostDir,
			Notes:           pairs(*notes, ";"),
		}),
	}
	stamp := time.Now().Format("20060102-150405")
	base := filepath.Join(*outDir, fmt.Sprintf("storebench-%s-%s", cfg.Label, stamp))
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fatal("create %s: %v", *outDir, err)
	}
	save := func() {
		if err := writeJSON(base+".json", report); err != nil {
			logf("write json: %v", err)
		}
		if err := os.WriteFile(base+".md", []byte(renderMarkdown(report)), 0o644); err != nil {
			logf("write markdown: %v", err)
		}
	}

	for _, dim := range cfg.Dims {
		logf("D=%d: generating %d entries over %d cache keys (seed %d)", dim, cfg.N, cfg.Keys, cfg.Seed)
		ds := workload.Generate(workload.Config{Dim: dim, N: cfg.N, Keys: cfg.Keys, ResponseBytes: cfg.ResponseBytes, Seed: cfg.Seed})
		sets := buildQuerySets(ds, cfg)
		runDimension(ctx, ds, sets, cfg, opts, logger, report, save)
	}
	report.Complete = true
	report.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	save()
	logf("wrote %s.json and %s.md", base, base)
}

// querySet is the measured and warm-up input of one query op.
type querySet struct {
	nearest     []workload.NearestQuery
	nearestWarm []workload.NearestQuery
	ids         []string
	idsWarm     []string
	idIndex     []int // for direct hits: index of the entry behind ids[i]
}

func buildQuerySets(ds *workload.Dataset, cfg benchConfig) map[string]*querySet {
	sets := map[string]*querySet{}
	start := time.Now()
	for _, op := range cfg.Ops {
		s := &querySet{}
		switch op {
		case opChunkHit:
			s.idIndex = ds.LookupIDs(cfg.Queries, 1)
			for _, i := range s.idIndex {
				s.ids = append(s.ids, ds.Entries[i].ID)
			}
			for _, i := range ds.LookupIDs(cfg.Warmup, 101) {
				s.idsWarm = append(s.idsWarm, ds.Entries[i].ID)
			}
		case opChunkMiss:
			s.ids = ds.MissingIDs(cfg.Queries, 2)
			s.idsWarm = ds.MissingIDs(cfg.Warmup, 102)
		case opNearestHit:
			s.nearest = ds.NearestQueries(workload.ClassHit, cfg.Queries, cfg.Threshold, 3)
			s.nearestWarm = ds.NearestQueries(workload.ClassHit, cfg.Warmup, cfg.Threshold, 103)
		case opNearestMiss:
			s.nearest = ds.NearestQueries(workload.ClassMiss, cfg.Queries, cfg.Threshold, 4)
			s.nearestWarm = ds.NearestQueries(workload.ClassMiss, cfg.Warmup, cfg.Threshold, 104)
		case opNearestCross:
			s.nearest = ds.NearestQueries(workload.ClassCross, cfg.Queries, cfg.Threshold, 5)
			s.nearestWarm = ds.NearestQueries(workload.ClassCross, cfg.Warmup, cfg.Threshold, 105)
		default:
			fatal("unknown op %q", op)
		}
		sets[op] = s
	}
	logf("D=%d: query sets and exact answers ready in %.1fs", ds.Dim, time.Since(start).Seconds())
	return sets
}

func warmLen(s *querySet) int {
	if s.nearestWarm != nil {
		return len(s.nearestWarm)
	}
	return len(s.idsWarm)
}

func measuredLen(s *querySet) int {
	if s.nearest != nil {
		return len(s.nearest)
	}
	return len(s.ids)
}

// execOp runs operation i of a query op and records its correctness. Only
// transport-level failures are returned as errors; a wrong answer is a
// correctness finding, not an error.
func execOp(ctx context.Context, store vectorstore.VectorStore, ns string, ds *workload.Dataset, cfg benchConfig, op string, s *querySet, warm bool, i int, t *Tally) error {
	switch op {
	case opChunkHit, opChunkMiss:
		ids := s.ids
		if warm {
			ids = s.idsWarm
		}
		result, err := store.GetChunk(ctx, ns, ids[i])
		notFound := err != nil && (errors.Is(err, vectorstore.ErrNotFound) || strings.Contains(strings.ToLower(err.Error()), "not found"))
		if err != nil && !notFound {
			return err
		}
		if warm {
			return nil
		}
		t.mu.Lock()
		defer t.mu.Unlock()
		t.Executions++
		if op == opChunkMiss {
			t.ExpectedEmpty++
			if notFound {
				t.CorrectEmpty++
			} else {
				t.UnexpectedResult++
			}
			return nil
		}
		t.ExpectedResult++
		if notFound {
			t.MissedResult++
			return nil
		}
		want := ds.Entries[s.idIndex[i]]
		if result.ID == want.ID && result.Properties["cache_key"] == want.Metadata["cache_key"] && result.Properties["response"] == want.Metadata["response"] {
			t.CorrectTop1++
		} else {
			t.WrongTop1++
		}
		return nil
	default:
		queries := s.nearest
		if warm {
			queries = s.nearestWarm
		}
		q := queries[i]
		results, err := store.GetNearest(ctx, ns, q.Vector, ds.Partitions[q.Partition].Filters(), workload.SelectFields, cfg.Threshold, 1)
		if err != nil {
			return err
		}
		if warm {
			return nil
		}
		checkNearest(t, ds, q, results, cfg.Threshold)
		return nil
	}
}

func checkNearest(t *Tally, ds *workload.Dataset, q workload.NearestQuery, results []vectorstore.SearchResult, threshold float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Executions++
	for _, r := range results {
		idx, ok := ds.Index(r.ID)
		if !ok {
			t.UnknownIDs++
			continue
		}
		e := ds.Entries[idx]
		if e.Partition != q.Partition || r.Properties["cache_key"] != ds.Partitions[q.Partition].CacheKey {
			t.FilterViolations++
		}
		if _, ok := r.Properties["response"]; !ok {
			t.MissingFields++
		}
		exact := workload.Cosine(q.Vector, e.Vector)
		if r.Score != nil {
			t.MaxScoreAbsError = math.Max(t.MaxScoreAbsError, math.Abs(*r.Score-exact))
		}
		if exact < threshold {
			t.BelowThreshold++
		}
	}
	if q.ExpectedID != "" {
		t.ExpectedResult++
		switch {
		case len(results) == 0:
			t.MissedResult++
		case results[0].ID == q.ExpectedID:
			t.CorrectTop1++
		default:
			t.WrongTop1++
		}
		return
	}
	t.ExpectedEmpty++
	if len(results) == 0 {
		t.CorrectEmpty++
	} else {
		t.UnexpectedResult++
	}
}

func shortTally(op string, t *Tally) string {
	switch {
	case t.ExpectedResult > 0 && t.RecallAt1 != nil:
		return fmt.Sprintf(", correct %d/%d", t.CorrectTop1, t.ExpectedResult)
	case t.ExpectedEmpty > 0:
		return fmt.Sprintf(", false results %d/%d", t.UnexpectedResult, t.ExpectedEmpty)
	}
	return ""
}

func medians(phases []PhaseRecord) []MedianRecord {
	type key struct {
		op string
		c  int
	}
	groups := map[key][]stats.Summary{}
	var order []key
	for _, p := range phases {
		k := key{p.Op, p.Concurrency}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], p.Summary)
	}
	var out []MedianRecord
	for _, k := range order {
		runs := groups[k]
		rec := MedianRecord{Op: k.op, Concurrency: k.c, Runs: len(runs), Median: stats.MedianOf(runs)}
		rec.P50RangeMs = [2]float64{math.Inf(1), math.Inf(-1)}
		rec.P99RangeMs = rec.P50RangeMs
		rec.ThroughputRange = rec.P50RangeMs
		for _, r := range runs {
			rec.P50RangeMs = [2]float64{math.Min(rec.P50RangeMs[0], r.P50Ms), math.Max(rec.P50RangeMs[1], r.P50Ms)}
			rec.P99RangeMs = [2]float64{math.Min(rec.P99RangeMs[0], r.P99Ms), math.Max(rec.P99RangeMs[1], r.P99Ms)}
			rec.ThroughputRange = [2]float64{math.Min(rec.ThroughputRange[0], r.Throughput), math.Max(rec.ThroughputRange[1], r.Throughput)}
		}
		out = append(out, rec)
	}
	return out
}

func cleanup(ctx context.Context, cfg benchConfig, opts backend.Options, logger schemas.Logger) {
	for _, name := range cfg.Backends {
		b, err := backend.New(ctx, name, opts, logger)
		if err != nil {
			logf("[%s] connect: %v", name, err)
			continue
		}
		namespaces, err := b.Store.ListNamespaces(ctx, cfg.NamespacePrefix)
		if err != nil {
			logf("[%s] list: %v", name, err)
		}
		for _, ns := range namespaces {
			if err := b.Store.DeleteNamespace(ctx, ns); err != nil {
				logf("[%s] drop %s: %v", name, ns, err)
			} else {
				logf("[%s] dropped %s", name, ns)
			}
		}
		if len(namespaces) == 0 {
			logf("[%s] no namespaces with prefix %q", name, cfg.NamespacePrefix)
		}
		b.Close()
	}
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func intList(s string) ([]int, error) {
	var out []int
	for _, part := range splitList(s) {
		v, err := strconv.Atoi(part)
		if err != nil || v <= 0 {
			return nil, fmt.Errorf("%q is not a positive integer", part)
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty list")
	}
	return out, nil
}

func pairs(s, sep string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(s, sep) {
		if k, v, ok := strings.Cut(strings.TrimSpace(part), "="); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, time.Now().Format("15:04:05")+" "+format+"\n", args...)
}

func fatal(format string, args ...any) {
	logf("fatal: "+format, args...)
	os.Exit(1)
}
