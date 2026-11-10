// Command hybridspike asks one question: do look-alike questions ("export CSV"
// vs "import CSV", "turn two-factor on" vs "off") get separated better when a
// keyword check is added to the vector similarity?
//
// It reuses the labelled pairs of cmd/cachequality (datasets/paraphrases.json)
// and the same embedding sources (the in-process hash embedder or an
// OpenAI-compatible endpoint such as the local mock). It loads the pairs into
// its own schema on the scratch PostgreSQL (default port 5433), computes the
// cosine and the full-text-search facts inside PostgreSQL, scores several rules
// in Go, writes results/hybridspike-<label>-<timestamp>.{json,md}, and drops its
// schema again.
//
// With bag-of-words embeddings (both sources here) a lexical rule can look
// better than it would next to a real embedding model. Every output says so.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

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
	Pairs []pair `json:"pairs"`
}

// PairFacts is everything measured about one pair.
type PairFacts struct {
	ID         string   `json:"id"`
	Label      string   `json:"label"`
	Note       string   `json:"note,omitempty"`
	A          string   `json:"a"`
	B          string   `json:"b"`
	Cosine     float64  `json:"cosine"`
	SQLCosine  float64  `json:"cosine_in_postgres"`
	FTSAOnly   []string `json:"fts_lexemes_only_in_a"`
	FTSBOnly   []string `json:"fts_lexemes_only_in_b"`
	FTSBothWay bool     `json:"fts_each_contains_all_words_of_the_other"`
	FTSRank    float64  `json:"fts_rank_any_word"`
	GoAOnly    []string `json:"words_only_in_a"`
	GoBOnly    []string `json:"words_only_in_b"`
	GoAOnlySyn []string `json:"words_only_in_a_with_synonyms"`
	GoBOnlySyn []string `json:"words_only_in_b_with_synonyms"`
}

// MethodResult is one method at one operating point.
type MethodResult struct {
	Method             string    `json:"method"`
	Confusion          Confusion `json:"confusion"`
	WrongKept          int       `json:"wrong_matches_kept"`
	WrongRemoved       int       `json:"wrong_matches_removed_vs_vector_only"`
	RightLost          int       `json:"right_matches_lost_vs_vector_only"`
	StillWrong         []string  `json:"wrong_matches_still_served,omitempty"`
	LostRight          []string  `json:"right_matches_lost_ids,omitempty"`
	TunedOnThisData    bool      `json:"tuned_on_this_data,omitempty"`
	TunedParameterNote string    `json:"tuned_parameter,omitempty"`
}

// Point is one vector operating point (a threshold) with every method on it.
type Point struct {
	Name      string         `json:"name"`
	Why       string         `json:"why"`
	Threshold float64        `json:"threshold"`
	Methods   []MethodResult `json:"methods"`
}

// Report is the results file.
type Report struct {
	Tool         string            `json:"tool"`
	Banner       string            `json:"banner"`
	Command      []string          `json:"command"`
	StartedAt    string            `json:"started_at"`
	FinishedAt   string            `json:"finished_at"`
	Embedder     string            `json:"embedder"`
	Meaningful   bool              `json:"meaningful_for_quality"`
	Dataset      string            `json:"dataset"`
	Pairs        int               `json:"pairs"`
	Positives    int               `json:"should_share_pairs"`
	Negatives    int               `json:"should_not_share_pairs"`
	Dimension    int               `json:"dimension"`
	Postgres     string            `json:"postgres"`
	Schema       string            `json:"schema_used_and_dropped"`
	MaxCosineErr float64           `json:"max_abs_go_cosine_minus_postgres_cosine"`
	SwapK        int               `json:"swap_rule_k"`
	SimilarityBy map[string]Spread `json:"similarity_by_label"`
	Points       []Point           `json:"operating_points"`
	KSensitivity []KRow            `json:"swap_rule_k_sensitivity_at_recall_point"`
	Pairwise     []PairFacts       `json:"pairs_detail"`
	Caveats      []string          `json:"caveats"`
}

// Spread summarises similarities of one label.
type Spread struct {
	Count  int     `json:"count"`
	Min    float64 `json:"min"`
	Median float64 `json:"median"`
	Max    float64 `json:"max"`
}

// KRow is the swap rule with another k.
type KRow struct {
	K         int       `json:"k"`
	Synonyms  bool      `json:"with_synonyms"`
	Confusion Confusion `json:"confusion"`
}

func main() {
	var (
		datasetPath = flag.String("dataset", filepath.Join("datasets", "paraphrases.json"), "labelled pairs (same file as cmd/cachequality)")
		embedder    = flag.String("embedder", "hash", "hash (in-process, deterministic) or openai (any OpenAI-compatible /v1/embeddings, e.g. the local mock)")
		baseURL     = flag.String("base-url", "http://127.0.0.1:18080/v1", "OpenAI-compatible base URL (localhost only for this spike)")
		model       = flag.String("model", "mock-embedding", "embedding model for -embedder openai")
		apiKeyEnv   = flag.String("api-key-env", "MOCK_OPENAI_KEY", "env var holding an API key (empty is fine for the local mock)")
		dimensions  = flag.Int("dimensions", 0, "optional 'dimensions' field for -embedder openai")
		hashDim     = flag.Int("hash-dim", 256, "dimension of the hash embedder")
		swapK       = flag.Int("swap-k", 2, "swap rule: reject when each side has 1..k words the other lacks")
		label       = flag.String("label", "", "label used in output file names")
		outDir      = flag.String("out", "results", "output directory")
		schema      = flag.String("schema", "hybridspike", "scratch schema (dropped at the end)")
		pgHost      = flag.String("pg-host", "localhost", "PostgreSQL host")
		pgPort      = flag.Int("pg-port", 5433, "PostgreSQL port (scratch container)")
		pgUser      = flag.String("pg-user", "bifrost", "PostgreSQL user")
		pgPassword  = flag.String("pg-password", envOr("PGVECTOR_PASSWORD", "bifrost_password"), "PostgreSQL password")
		pgDB        = flag.String("pg-db", "bifrost", "PostgreSQL database")
	)
	flag.Parse()
	ctx := context.Background()

	data, err := loadDataset(*datasetPath)
	if err != nil {
		fatal("%v", err)
	}
	rep := &Report{Tool: "hybridspike", Command: os.Args, StartedAt: time.Now().UTC().Format(time.RFC3339), Dataset: *datasetPath, SwapK: *swapK}
	var emb embed.Embedder
	switch *embedder {
	case "hash":
		emb = embed.Hash{Dim: *hashDim}
	case "openai":
		if !strings.Contains(*baseURL, "127.0.0.1") && !strings.Contains(*baseURL, "localhost") {
			fatal("this spike only talks to localhost; got -base-url %s", *baseURL)
		}
		emb = &embed.OpenAI{BaseURL: *baseURL, APIKey: os.Getenv(*apiKeyEnv), Model: *model, Dimensions: *dimensions}
	default:
		fatal("unknown -embedder %q", *embedder)
	}
	rep.Embedder = emb.Name()
	rep.Meaningful = false
	rep.Banner = "BAG-OF-WORDS EMBEDDINGS - NOT A SEMANTIC MODEL - A LEXICAL RULE LOOKS BETTER HERE THAN IT WOULD NEXT TO REAL EMBEDDINGS"
	logf("%s", rep.Banner)

	// Same text the semantic cache embeds for a one-message chat request.
	texts := make([]string, 0, 2*len(data.Pairs))
	for _, p := range data.Pairs {
		texts = append(texts, "user: "+strings.ToLower(strings.TrimSpace(p.A)), "user: "+strings.ToLower(strings.TrimSpace(p.B)))
	}
	vecs, err := emb.Embed(ctx, texts)
	if err != nil {
		fatal("embedding failed: %v", err)
	}
	rep.Dimension = len(vecs[0])
	rep.Pairs = len(data.Pairs)

	facts, pgVersion, err := measureInPostgres(ctx, data, vecs, *schema, fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", *pgUser, *pgPassword, *pgHost, *pgPort, *pgDB))
	if err != nil {
		fatal("postgres: %v", err)
	}
	rep.Postgres = pgVersion
	rep.Schema = *schema
	for i := range facts {
		facts[i].Cosine = workload.Cosine(vecs[2*i], vecs[2*i+1])
		if d := abs(facts[i].Cosine - facts[i].SQLCosine); d > rep.MaxCosineErr {
			rep.MaxCosineErr = d
		}
		fillWords(&facts[i], data.Pairs[i])
		if facts[i].Label == "hit" {
			rep.Positives++
		} else {
			rep.Negatives++
		}
	}
	rep.Pairwise = facts
	analyse(rep, facts)
	rep.Caveats = caveats(rep)
	rep.FinishedAt = time.Now().UTC().Format(time.RFC3339)

	if *label == "" {
		*label = *embedder
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fatal("%v", err)
	}
	base := filepath.Join(*outDir, fmt.Sprintf("hybridspike-%s-%s", *label, time.Now().Format("20060102-150405")))
	raw, _ := json.MarshalIndent(rep, "", "  ")
	if err := os.WriteFile(base+".json", raw, 0o644); err != nil {
		fatal("%v", err)
	}
	if err := os.WriteFile(base+".md", []byte(renderMarkdown(rep)), 0o644); err != nil {
		fatal("%v", err)
	}
	logf("wrote %s.json and %s.md", base, base)
}

// measureInPostgres loads the pairs into a scratch schema, lets PostgreSQL compute
// the cosine (pgvector) and the full-text facts, and drops the schema again.
func measureInPostgres(ctx context.Context, data *dataset, vecs [][]float32, schema, dsn string) ([]PairFacts, string, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, "", err
	}
	defer conn.Close(ctx)
	sch := pgx.Identifier{schema}.Sanitize()
	if _, err := conn.Exec(ctx, "DROP SCHEMA IF EXISTS "+sch+" CASCADE"); err != nil {
		return nil, "", err
	}
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+sch); err != nil {
		return nil, "", err
	}
	defer func() {
		if _, err := conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+sch+" CASCADE"); err != nil {
			logf("drop schema %s: %v", schema, err)
		}
	}()
	dim := len(vecs[0])
	if _, err := conn.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s.pairs (ord int PRIMARY KEY, id text, label text, a text, b text, av vector(%d), bv vector(%d))`, sch, dim, dim)); err != nil {
		return nil, "", err
	}
	for i, p := range data.Pairs {
		if _, err := conn.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.pairs VALUES ($1,$2,$3,$4,$5,$6::vector,$7::vector)`, sch),
			i, p.ID, p.Label, p.A, p.B, vecLiteral(vecs[2*i]), vecLiteral(vecs[2*i+1])); err != nil {
			return nil, "", fmt.Errorf("insert %s: %w", p.ID, err)
		}
	}
	// plainto_tsquery is an AND of the stemmed, stopword-free words; swapping its
	// "&" for "|" gives the "any word" query used for ts_rank.
	query := fmt.Sprintf(`
WITH x AS (
  SELECT ord, id, label, a, b, 1 - (av <=> bv) AS cos,
         to_tsvector('english', a) AS ta, to_tsvector('english', b) AS tb
  FROM %s.pairs)
SELECT ord, id, label, cos::float8,
  ARRAY(SELECT unnest(tsvector_to_array(ta)) EXCEPT SELECT unnest(tsvector_to_array(tb)) ORDER BY 1),
  ARRAY(SELECT unnest(tsvector_to_array(tb)) EXCEPT SELECT unnest(tsvector_to_array(ta)) ORDER BY 1),
  (ta @@ plainto_tsquery('english', b)) AND (tb @@ plainto_tsquery('english', a)),
  ((ts_rank(ta, replace(plainto_tsquery('english', b)::text, '&', '|')::tsquery)
   + ts_rank(tb, replace(plainto_tsquery('english', a)::text, '&', '|')::tsquery)) / 2)::float8
FROM x ORDER BY ord`, sch)
	rows, err := conn.Query(ctx, query)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []PairFacts
	for rows.Next() {
		var ord int
		var f PairFacts
		if err := rows.Scan(&ord, &f.ID, &f.Label, &f.SQLCosine, &f.FTSAOnly, &f.FTSBOnly, &f.FTSBothWay, &f.FTSRank); err != nil {
			return nil, "", err
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	var version string
	_ = conn.QueryRow(ctx, `SELECT split_part(version(), ' on ', 1) || ' / pgvector ' || (SELECT extversion FROM pg_extension WHERE extname='vector')`).Scan(&version)
	return out, version, nil
}

func fillWords(f *PairFacts, p pair) {
	f.A, f.B, f.Note = p.A, p.B, p.Note
	a, b := ContentWords(p.A, nil), ContentWords(p.B, nil)
	f.GoAOnly, f.GoBOnly = Diff(a, b)
	as, bs := ContentWords(p.A, defaultSynonyms), ContentWords(p.B, defaultSynonyms)
	f.GoAOnlySyn, f.GoBOnlySyn = Diff(as, bs)
}

func vecLiteral(v []float32) string {
	var sb strings.Builder
	sb.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, "%g", x)
	}
	sb.WriteByte(']')
	return sb.String()
}

func loadDataset(path string) (*dataset, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var d dataset
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(d.Pairs) == 0 {
		return nil, fmt.Errorf("%s: no pairs", path)
	}
	for _, p := range d.Pairs {
		if p.Label != "hit" && p.Label != "miss" {
			return nil, fmt.Errorf("%s: pair %s has label %q", path, p.ID, p.Label)
		}
	}
	return &d, nil
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
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
