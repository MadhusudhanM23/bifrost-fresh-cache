// Command cachequality measures how well a similarity threshold separates
// prompts that should share a cached answer from prompts that should not.
//
// For every labelled pair (a, b) in the dataset, a's embedding plays the cached
// entry and b's embedding the incoming prompt. At each threshold a pair is a
// cache hit when cosine(a, b) >= threshold. The sweep is computed twice: once
// directly from the cosine similarities, and once through the pgvector store's
// GetNearest exactly as the semantic cache calls it (one cache_key per pair so
// each lookup sees only its own cached entry), and the two are compared.
//
// Real quality numbers need a real embedding model:
//
//	$env:OPENAI_API_KEY = "<key>"; go run ./cmd/cachequality -label openai
//
// Runs against the local mock server or the in-process hash embedder are
// labelled as not meaningful for quality in every output.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/vectorstore"

	"bifrost-eval-harness/internal/backend"
	"bifrost-eval-harness/internal/embed"
	"bifrost-eval-harness/internal/workload"
)

type pair struct {
	ID       string `json:"id"`
	Category string `json:"category"`
	Label    string `json:"label"`
	A        string `json:"a"`
	B        string `json:"b"`
	Note     string `json:"note,omitempty"`
}

type dataset struct {
	Description string `json:"description"`
	Version     int    `json:"version"`
	Pairs       []pair `json:"pairs"`
}

// Confusion counts cache decisions against the labels.
type Confusion struct {
	TrueHits    int      `json:"true_hits"`
	FalseHits   int      `json:"false_hits"`
	FalseMisses int      `json:"false_misses"`
	TrueMisses  int      `json:"true_misses"`
	Precision   *float64 `json:"precision"`
	Recall      *float64 `json:"recall"`
	F1          float64  `json:"f1"`
}

func (c *Confusion) count(shouldHit, hit bool) {
	switch {
	case shouldHit && hit:
		c.TrueHits++
	case !shouldHit && hit:
		c.FalseHits++
	case shouldHit && !hit:
		c.FalseMisses++
	default:
		c.TrueMisses++
	}
}

func (c *Confusion) finish() {
	if c.TrueHits+c.FalseHits > 0 {
		p := float64(c.TrueHits) / float64(c.TrueHits+c.FalseHits)
		c.Precision = &p
	}
	if c.TrueHits+c.FalseMisses > 0 {
		r := float64(c.TrueHits) / float64(c.TrueHits+c.FalseMisses)
		c.Recall = &r
	}
	if c.Precision != nil && c.Recall != nil && *c.Precision+*c.Recall > 0 {
		c.F1 = 2 * *c.Precision * *c.Recall / (*c.Precision + *c.Recall)
	}
}

// ThresholdRow is one threshold of the sweep.
type ThresholdRow struct {
	Threshold     float64    `json:"threshold"`
	Cosine        Confusion  `json:"cosine"`
	Store         *Confusion `json:"store,omitempty"`
	Disagreements int        `json:"disagreements"`
}

// PairResult is one pair's similarity.
type PairResult struct {
	ID         string   `json:"id"`
	Category   string   `json:"category"`
	Label      string   `json:"label"`
	A          string   `json:"a"`
	B          string   `json:"b"`
	Similarity float64  `json:"cosine"`
	StoreScore *float64 `json:"store_score_at_lowest_threshold,omitempty"`
}

// Disagreement is a (pair, threshold) where the store and the direct cosine
// computation made different cache decisions.
type Disagreement struct {
	Pair       string   `json:"pair"`
	Threshold  float64  `json:"threshold"`
	Cosine     float64  `json:"cosine"`
	StoreHit   bool     `json:"store_hit"`
	StoreScore *float64 `json:"store_score,omitempty"`
	ReturnedID string   `json:"returned_id,omitempty"`
}

// Report is the results file.
type Report struct {
	Tool              string             `json:"tool"`
	Meaningful        bool               `json:"meaningful_for_quality"`
	Banner            string             `json:"banner"`
	NotMeaningfulWhy  string             `json:"not_meaningful_because,omitempty"`
	Command           []string           `json:"command"`
	StartedAt         string             `json:"started_at"`
	FinishedAt        string             `json:"finished_at"`
	Embedder          string             `json:"embedder"`
	TextFormat        string             `json:"text_format"`
	Dataset           string             `json:"dataset"`
	Pairs             int                `json:"pairs"`
	Positives         int                `json:"positives"`
	Negatives         int                `json:"negatives"`
	Dimension         int                `json:"dimension"`
	Store             string             `json:"store"`
	StoreServer       map[string]any     `json:"store_server,omitempty"`
	Thresholds        []ThresholdRow     `json:"thresholds"`
	BestF1Thresholds  []float64          `json:"best_f1_thresholds"`
	BestF1            float64            `json:"best_f1"`
	TotalDecisions    int                `json:"store_decisions"`
	AgreeingDecisions int                `json:"store_decisions_agreeing_with_cosine"`
	MaxScoreAbsError  float64            `json:"max_abs_store_score_minus_cosine"`
	Disagreements     []Disagreement     `json:"disagreements"`
	SimilarityByLabel map[string]Summary `json:"similarity_by_label"`
	PairResults       []PairResult       `json:"pair_results"`
}

// Summary is the spread of similarities within one label.
type Summary struct {
	Count  int     `json:"count"`
	Min    float64 `json:"min"`
	P25    float64 `json:"p25"`
	Median float64 `json:"median"`
	P75    float64 `json:"p75"`
	Max    float64 `json:"max"`
}

func main() {
	var (
		datasetPath = flag.String("dataset", filepath.Join("datasets", "paraphrases.json"), "labelled pairs")
		embedder    = flag.String("embedder", "openai", "openai (any OpenAI-compatible /v1/embeddings) or hash (in-process, not semantic)")
		baseURL     = flag.String("base-url", envOr("EMBEDDINGS_BASE_URL", "https://api.openai.com/v1"), "OpenAI-compatible base URL [env EMBEDDINGS_BASE_URL]")
		model       = flag.String("model", envOr("EMBEDDINGS_MODEL", "text-embedding-3-small"), "embedding model [env EMBEDDINGS_MODEL]")
		apiKeyEnv   = flag.String("api-key-env", "OPENAI_API_KEY", "name of the environment variable that holds the API key (the key is never taken on the command line or written out)")
		dimensions  = flag.Int("dimensions", 0, "optional 'dimensions' request field (0 = model default)")
		hashDim     = flag.Int("hash-dim", 256, "dimension of the in-process hash embedder")
		format      = flag.String("format", "chat", "chat: embed 'user: <lowercased prompt>' as the semantic cache does for a one-message chat request; raw: embed the prompt as written")
		thresholds  = flag.String("thresholds", "0.70:0.98:0.02", "from:to:step")
		storeName   = flag.String("store", "pgvector", "pgvector, or none to skip the store check")
		label       = flag.String("label", "", "label used in output file names")
		outDir      = flag.String("out", "results", "output directory")
		prefix      = flag.String("prefix", "eval_quality", "namespace prefix")
		pgHost      = flag.String("pg-host", "localhost", "PostgreSQL host")
		pgPort      = flag.Int("pg-port", 5433, "PostgreSQL port")
		pgUser      = flag.String("pg-user", "bifrost", "PostgreSQL user")
		pgPassword  = flag.String("pg-password", envOr("PGVECTOR_PASSWORD", "bifrost_password"), "PostgreSQL password (default from PGVECTOR_PASSWORD)")
		pgDB        = flag.String("pg-db", "bifrost", "PostgreSQL database")
		pgSSLMode   = flag.String("pg-sslmode", "disable", "PostgreSQL sslmode")
	)
	flag.Parse()
	ctx := context.Background()

	data, err := loadDataset(*datasetPath)
	if err != nil {
		fatal("%v", err)
	}
	levels, err := parseThresholds(*thresholds)
	if err != nil {
		fatal("bad -thresholds: %v", err)
	}

	var emb embed.Embedder
	report := &Report{Tool: "cachequality", Command: os.Args, StartedAt: time.Now().UTC().Format(time.RFC3339), Dataset: *datasetPath, TextFormat: *format}
	switch *embedder {
	case "hash":
		emb = embed.Hash{Dim: *hashDim}
		report.NotMeaningfulWhy = "the in-process hash embedder only counts shared words; it is not a semantic model"
	case "openai":
		emb = &embed.OpenAI{BaseURL: *baseURL, APIKey: os.Getenv(*apiKeyEnv), Model: *model, Dimensions: *dimensions}
		switch {
		case embed.LooksLikeMockServer(ctx, *baseURL):
			report.NotMeaningfulWhy = "the endpoint is the local mock OpenAI server (it answers GET /mock/stats); its embeddings are feature hashing of words, not a semantic model"
		case strings.Contains(strings.ToLower(*model), "mock"):
			report.NotMeaningfulWhy = "the model name says mock"
		}
	default:
		fatal("unknown -embedder %q", *embedder)
	}
	report.Embedder = emb.Name()
	report.Meaningful = report.NotMeaningfulWhy == ""
	if report.Meaningful {
		report.Banner = fmt.Sprintf("REAL EMBEDDINGS (%s) on a small hand-written dataset of %d pairs: indicative, not a benchmark", *model, len(data.Pairs))
	} else {
		report.Banner = "MOCK EMBEDDINGS - NOT A SEMANTIC MODEL - NOT MEANINGFUL FOR QUALITY"
	}
	logf("%s", report.Banner)

	texts := make([]string, 0, 2*len(data.Pairs))
	for _, p := range data.Pairs {
		texts = append(texts, embeddingText(p.A, *format), embeddingText(p.B, *format))
	}
	logf("embedding %d texts with %s", len(texts), emb.Name())
	vectors, err := emb.Embed(ctx, texts)
	if err != nil {
		fatal("embedding failed: %v", err)
	}
	report.Dimension = len(vectors[0])
	for i, v := range vectors {
		if len(v) != report.Dimension {
			fatal("embedding %d has %d dimensions, expected %d", i, len(v), report.Dimension)
		}
	}

	bySim := map[string][]float64{}
	for i, p := range data.Pairs {
		sim := workload.Cosine(vectors[2*i], vectors[2*i+1])
		report.PairResults = append(report.PairResults, PairResult{ID: p.ID, Category: p.Category, Label: p.Label, A: p.A, B: p.B, Similarity: sim})
		bySim[p.Label] = append(bySim[p.Label], sim)
		if p.Label == "hit" {
			report.Positives++
		} else {
			report.Negatives++
		}
	}
	report.Pairs = len(data.Pairs)
	report.SimilarityByLabel = map[string]Summary{}
	for label, sims := range bySim {
		report.SimilarityByLabel[label] = summarize(sims)
	}

	for _, t := range levels {
		row := ThresholdRow{Threshold: t}
		for i, p := range data.Pairs {
			row.Cosine.count(p.Label == "hit", report.PairResults[i].Similarity >= t)
		}
		row.Cosine.finish()
		report.Thresholds = append(report.Thresholds, row)
	}

	switch *storeName {
	case "none":
		report.Store = "none (store check skipped)"
	case "pgvector":
		logger := bifrost.NewDefaultLogger(schemas.LogLevelError)
		b, err := backend.New(ctx, "pgvector", backend.Options{PgHost: *pgHost, PgPort: *pgPort, PgUser: *pgUser, PgPassword: *pgPassword, PgDatabase: *pgDB, PgSSLMode: *pgSSLMode}, logger)
		if err != nil {
			fatal("pgvector: %v", err)
		}
		report.StoreServer = b.Server(ctx)
		if err := storeSweep(ctx, b.Store, data, vectors, levels, *prefix, report); err != nil {
			b.Close()
			fatal("store sweep: %v", err)
		}
		b.Close()
	default:
		fatal("unknown -store %q", *storeName)
	}

	for _, row := range report.Thresholds {
		f1 := row.Cosine.F1
		switch {
		case f1 > report.BestF1+1e-12:
			report.BestF1, report.BestF1Thresholds = f1, []float64{row.Threshold}
		case math.Abs(f1-report.BestF1) <= 1e-12:
			report.BestF1Thresholds = append(report.BestF1Thresholds, row.Threshold)
		}
	}
	report.FinishedAt = time.Now().UTC().Format(time.RFC3339)

	if *label == "" {
		*label = "run"
		if !report.Meaningful {
			*label = "mock"
		}
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fatal("%v", err)
	}
	base := filepath.Join(*outDir, fmt.Sprintf("cachequality-%s-%s", *label, time.Now().Format("20060102-150405")))
	raw, _ := json.MarshalIndent(report, "", "  ")
	if err := os.WriteFile(base+".json", raw, 0o644); err != nil {
		fatal("%v", err)
	}
	if err := os.WriteFile(base+".md", []byte(renderMarkdown(report)), 0o644); err != nil {
		fatal("%v", err)
	}
	logf("%s", report.Banner)
	logf("wrote %s.json and %s.md", base, base)
}

// storeSweep stores every pair's cached prompt in pgvector under its own
// cache_key and repeats the sweep through GetNearest, with the semantic cache's
// filters, select fields and limit 1.
func storeSweep(ctx context.Context, store vectorstore.VectorStore, data *dataset, vectors [][]float32, levels []float64, prefix string, report *Report) error {
	ns := fmt.Sprintf("%s_d%d", prefix, report.Dimension)
	report.Store = fmt.Sprintf("pgvector, namespace %s (created for this run and dropped after it)", ns)
	if err := store.DeleteNamespace(ctx, ns); err != nil {
		return err
	}
	if err := store.CreateNamespace(ctx, ns, report.Dimension, workload.CacheProperties); err != nil {
		return err
	}
	defer func() {
		if err := store.DeleteNamespace(context.Background(), ns); err != nil {
			logf("drop %s: %v", ns, err)
		}
	}()
	partition := func(p pair) workload.Partition {
		return workload.Partition{CacheKey: "quality:" + p.ID, ParamsHash: "quality-eval"}
	}
	ids := make([]string, len(data.Pairs))
	for i, p := range data.Pairs {
		ids[i] = uuid.NewSHA1(uuid.NameSpaceURL, []byte("bifrost-eval-harness/cachequality/"+p.ID)).String()
		part := partition(p)
		metadata := map[string]interface{}{
			"response":                           "cached answer to: " + p.A,
			"stream_chunks":                      []string{},
			"cache_key":                          part.CacheKey,
			"params_hash":                        part.ParamsHash,
			"provider":                           "openai",
			"model":                              "gpt-4o-mini",
			"from_bifrost_semantic_cache_plugin": true,
			"expires_at":                         int64(4102444800),
		}
		if err := store.Add(ctx, ns, ids[i], vectors[2*i], metadata); err != nil {
			return fmt.Errorf("add %s: %w", p.ID, err)
		}
	}
	for li, t := range levels {
		row := &report.Thresholds[li]
		storeConfusion := &Confusion{}
		for i, p := range data.Pairs {
			results, err := store.GetNearest(ctx, ns, vectors[2*i+1], partition(p).Filters(), workload.SelectFields, t, 1)
			if err != nil {
				return fmt.Errorf("GetNearest %s at %.2f: %w", p.ID, t, err)
			}
			hit := len(results) > 0
			var score *float64
			returned := ""
			if hit {
				score, returned = results[0].Score, results[0].ID
				if score != nil {
					report.MaxScoreAbsError = math.Max(report.MaxScoreAbsError, math.Abs(*score-report.PairResults[i].Similarity))
				}
				if li == 0 {
					report.PairResults[i].StoreScore = score
				}
			}
			storeConfusion.count(p.Label == "hit", hit)
			report.TotalDecisions++
			cosineHit := report.PairResults[i].Similarity >= t
			if hit == cosineHit && (!hit || returned == ids[i]) {
				report.AgreeingDecisions++
			} else {
				row.Disagreements++
				report.Disagreements = append(report.Disagreements, Disagreement{Pair: p.ID, Threshold: t, Cosine: report.PairResults[i].Similarity, StoreHit: hit, StoreScore: score, ReturnedID: returned})
			}
		}
		storeConfusion.finish()
		row.Store = storeConfusion
	}
	return nil
}

func embeddingText(prompt, format string) string {
	if format == "raw" {
		return prompt
	}
	// semanticcache.extractTextForEmbedding: "<role>: " + normalizeText(content),
	// where normalizeText lowercases and trims.
	return "user: " + strings.ToLower(strings.TrimSpace(prompt))
}

func loadDataset(path string) (*dataset, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var data dataset
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	seen := map[string]bool{}
	for _, p := range data.Pairs {
		switch {
		case p.ID == "" || seen[p.ID]:
			return nil, fmt.Errorf("%s: missing or duplicate id %q", path, p.ID)
		case p.Label != "hit" && p.Label != "miss":
			return nil, fmt.Errorf("%s: pair %s has label %q (want hit or miss)", path, p.ID, p.Label)
		case strings.TrimSpace(p.A) == "" || strings.TrimSpace(p.B) == "":
			return nil, fmt.Errorf("%s: pair %s has an empty prompt", path, p.ID)
		}
		seen[p.ID] = true
	}
	if len(data.Pairs) == 0 {
		return nil, fmt.Errorf("%s: no pairs", path)
	}
	return &data, nil
}

// parseThresholds expands from:to:step on a grid of 1/10000 so 0.70+0.02*k
// lands on exact two-decimal values.
func parseThresholds(spec string) ([]float64, error) {
	parts := strings.Split(spec, ":")
	if len(parts) != 3 {
		return nil, fmt.Errorf("want from:to:step, got %q", spec)
	}
	var grid [3]int
	for i, part := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil {
			return nil, err
		}
		grid[i] = int(math.Round(v * 10000))
	}
	if grid[2] <= 0 || grid[1] < grid[0] {
		return nil, fmt.Errorf("need step > 0 and to >= from")
	}
	var out []float64
	for t := grid[0]; t <= grid[1]; t += grid[2] {
		out = append(out, float64(t)/10000)
	}
	return out, nil
}

func summarize(values []float64) Summary {
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	at := func(q float64) float64 { return sorted[int(math.Round(q*float64(len(sorted)-1)))] }
	return Summary{Count: len(sorted), Min: sorted[0], P25: at(0.25), Median: at(0.5), P75: at(0.75), Max: sorted[len(sorted)-1]}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, time.Now().Format("15:04:05")+" "+format+"\n", args...)
}

func fatal(format string, args ...any) {
	logf("fatal: "+format, args...)
	os.Exit(1)
}
