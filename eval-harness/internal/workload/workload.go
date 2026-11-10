// Package workload generates the deterministic, semantic-cache-shaped data the
// store benchmark loads and queries, and the exact answers it is checked against.
package workload

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"

	"github.com/google/uuid"
	"github.com/maximhq/bifrost/framework/vectorstore"
)

// CacheProperties mirrors semanticcache.VectorStoreProperties
// (bifrost/plugins/semanticcache/main.go). The plugin passes exactly this map
// to CreateNamespace; Qdrant turns every entry into a payload index, pgvector
// ignores it (one GIN index covers all metadata).
var CacheProperties = map[string]vectorstore.VectorStoreProperties{
	"response":                           {DataType: vectorstore.VectorStorePropertyTypeString, Description: "The response from the provider"},
	"stream_chunks":                      {DataType: vectorstore.VectorStorePropertyTypeStringArray, Description: "The stream chunks from the provider"},
	"expires_at":                         {DataType: vectorstore.VectorStorePropertyTypeInteger, Description: "The expiration time of the cache entry"},
	"cache_key":                          {DataType: vectorstore.VectorStorePropertyTypeString, Description: "The cache key from the request", Filterable: true},
	"provider":                           {DataType: vectorstore.VectorStorePropertyTypeString, Description: "The provider used for the request", Filterable: true},
	"model":                              {DataType: vectorstore.VectorStorePropertyTypeString, Description: "The model used for the request", Filterable: true},
	"params_hash":                        {DataType: vectorstore.VectorStorePropertyTypeString, Description: "The hash of the parameters used for the request", Filterable: true},
	"from_bifrost_semantic_cache_plugin": {DataType: vectorstore.VectorStorePropertyTypeBoolean, Description: "Whether the cache entry was created by the BifrostSemanticCachePlugin", Filterable: true},
}

// SelectFields is what the plugin projects on a non-streaming lookup
// (semanticcache.SelectFields without "stream_chunks", see search.go).
var SelectFields = []string{"response", "expires_at", "cache_key", "provider", "model"}

// Partition is one filter scope: the (cache_key, params_hash) pair that the
// plugin's strict filters select.
type Partition struct {
	CacheKey   string `json:"cache_key"`
	ParamsHash string `json:"params_hash"`
}

// Filters returns the plugin's strict GetNearest filters for this partition
// (search.go: cache_key, params_hash, from_bifrost_semantic_cache_plugin).
func (p Partition) Filters() []vectorstore.Query {
	return []vectorstore.Query{
		{Field: "cache_key", Operator: vectorstore.QueryOperatorEqual, Value: p.CacheKey},
		{Field: "params_hash", Operator: vectorstore.QueryOperatorEqual, Value: p.ParamsHash},
		{Field: "from_bifrost_semantic_cache_plugin", Operator: vectorstore.QueryOperatorEqual, Value: true},
	}
}

// Entry is one cache entry: a random unit vector plus the metadata the plugin writes.
type Entry struct {
	ID        string
	Vector    []float32
	Partition int
	Metadata  map[string]interface{}
}

// Dataset is everything loaded into one namespace.
type Dataset struct {
	Dim         int
	Seed        uint64
	Entries     []Entry
	Partitions  []Partition
	byPartition [][]int
	byID        map[string]int
}

// Index returns the position of the entry with the given id.
func (ds *Dataset) Index(id string) (int, bool) {
	i, ok := ds.byID[id]
	return i, ok
}

// Config controls dataset generation.
type Config struct {
	Dim           int
	N             int
	Keys          int
	ResponseBytes int
	Seed          uint64
}

// Generate builds the dataset deterministically from cfg.Seed and cfg.Dim, so
// every backend is loaded with byte-identical entries.
func Generate(cfg Config) *Dataset {
	rng := rand.New(rand.NewPCG(cfg.Seed, uint64(cfg.Dim)))
	ds := &Dataset{Dim: cfg.Dim, Seed: cfg.Seed, byPartition: make([][]int, cfg.Keys), byID: make(map[string]int, cfg.N)}
	for k := 0; k < cfg.Keys; k++ {
		sum := sha256.Sum256([]byte(fmt.Sprintf("temperature=0.%d;max_tokens=%d", k%10, 256+k)))
		ds.Partitions = append(ds.Partitions, Partition{
			CacheKey:   fmt.Sprintf("tenant-%02d", k),
			ParamsHash: hex.EncodeToString(sum[:8]),
		})
	}
	// Fixed so that every run writes the same bytes; far enough in the future
	// that nothing a TTL reaper might do applies.
	const expiresAt int64 = 4102444800 // 2100-01-01T00:00:00Z
	ds.Entries = make([]Entry, cfg.N)
	for i := 0; i < cfg.N; i++ {
		p := i % cfg.Keys
		id := randomUUID(rng)
		ds.Entries[i] = Entry{
			ID:        id,
			Vector:    RandomUnitVector(rng, cfg.Dim),
			Partition: p,
			Metadata: map[string]interface{}{
				"response":                           responsePayload(i, cfg.ResponseBytes),
				"stream_chunks":                      []string{},
				"cache_key":                          ds.Partitions[p].CacheKey,
				"params_hash":                        ds.Partitions[p].ParamsHash,
				"provider":                           "openai",
				"model":                              "gpt-4o-mini",
				"from_bifrost_semantic_cache_plugin": true,
				"expires_at":                         expiresAt,
			},
		}
		ds.byPartition[p] = append(ds.byPartition[p], i)
		ds.byID[id] = i
	}
	return ds
}

// PartitionSize returns how many entries partition p holds.
func (ds *Dataset) PartitionSize(p int) int { return len(ds.byPartition[p]) }

// RandomUnitVector draws a vector uniformly from the unit sphere.
func RandomUnitVector(rng *rand.Rand, dim int) []float32 {
	v := make([]float64, dim)
	for i := range v {
		v[i] = rng.NormFloat64()
	}
	return toUnit32(v)
}

func toUnit32(v []float64) []float32 {
	var norm float64
	for _, x := range v {
		norm += x * x
	}
	norm = math.Sqrt(norm)
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(x / norm)
	}
	return out
}

// WithSimilarity returns a unit vector whose cosine similarity to the unit
// vector base is sim: sim*base + sqrt(1-sim^2)*u, u a random unit vector
// orthogonal to base.
func WithSimilarity(rng *rand.Rand, base []float32, sim float64) []float32 {
	dim := len(base)
	u := make([]float64, dim)
	for i := range u {
		u[i] = rng.NormFloat64()
	}
	var dot float64
	for i := range u {
		dot += u[i] * float64(base[i])
	}
	var norm float64
	for i := range u {
		u[i] -= dot * float64(base[i])
		norm += u[i] * u[i]
	}
	norm = math.Sqrt(norm)
	rest := math.Sqrt(1 - sim*sim)
	q := make([]float64, dim)
	for i := range q {
		q[i] = sim*float64(base[i]) + rest*u[i]/norm
	}
	return toUnit32(q)
}

// Cosine is the exact cosine similarity, accumulated in float64.
func Cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func randomUUID(rng *rand.Rand) string {
	var b [16]byte
	for i := 0; i < 16; i += 8 {
		x := rng.Uint64()
		for j := 0; j < 8; j++ {
			b[i+j] = byte(x >> (8 * j))
		}
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return uuid.UUID(b).String()
}

// responsePayload is a chat-completion-shaped JSON string of about size bytes,
// standing in for the serialized response the plugin stores.
func responsePayload(i, size int) string {
	head := fmt.Sprintf(`{"id":"chatcmpl-eval-%06d","object":"chat.completion","model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"`, i)
	tail := `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":42,"completion_tokens":180,"total_tokens":222}}`
	fill := size - len(head) - len(tail)
	if fill < 0 {
		fill = 0
	}
	words := []string{"Thanks", "for", "reaching", "out.", "Your", "order", "can", "be", "updated", "from", "the", "account", "page", "within", "24", "hours."}
	var b strings.Builder
	for w := i; b.Len() < fill; w++ {
		b.WriteString(words[w%len(words)])
		b.WriteByte(' ')
	}
	content := b.String()
	if len(content) > fill {
		content = content[:fill]
	}
	return head + content + tail
}

// QueryClass names a kind of GetNearest query.
type QueryClass string

const (
	// ClassHit: a near-duplicate (cosine in the upper part of [threshold, 1]) of a
	// stored entry, filtered to that entry's partition. Exact answer: that entry.
	ClassHit QueryClass = "hit"
	// ClassMiss: a fresh random vector, filtered to some partition. Exact answer: nothing.
	ClassMiss QueryClass = "miss"
	// ClassCross: a near-duplicate of a stored entry, filtered to a different
	// partition, so only the filter stands between the query and a wrong hit.
	// Exact answer: nothing.
	ClassCross QueryClass = "cross"
)

// NearestQuery is one GetNearest call and its exact (brute-force) answer.
type NearestQuery struct {
	Class      QueryClass
	Vector     []float32
	Partition  int
	TargetSim  float64
	ExpectedID string // "" means no entry in the partition reaches the threshold
	ExpectedSc float64
}

// NearestQueries draws n queries of one class. stream separates the random
// streams of different query sets so they never coincide.
func (ds *Dataset) NearestQueries(class QueryClass, n int, threshold float64, stream uint64) []NearestQuery {
	rng := rand.New(rand.NewPCG(ds.Seed^0x9e3779b97f4a7c15, uint64(ds.Dim)<<32|stream))
	lo := threshold + (1-threshold)*0.1
	hi := threshold + (1-threshold)*0.9
	queries := make([]NearestQuery, 0, n)
	for len(queries) < n {
		q := NearestQuery{Class: class}
		switch class {
		case ClassHit, ClassCross:
			src := ds.Entries[rng.IntN(len(ds.Entries))]
			q.TargetSim = lo + rng.Float64()*(hi-lo)
			q.Vector = WithSimilarity(rng, src.Vector, q.TargetSim)
			q.Partition = src.Partition
			if class == ClassCross {
				if len(ds.Partitions) < 2 {
					return nil
				}
				other := rng.IntN(len(ds.Partitions) - 1)
				if other >= src.Partition {
					other++
				}
				q.Partition = other
			}
		case ClassMiss:
			q.Vector = RandomUnitVector(rng, ds.Dim)
			q.Partition = rng.IntN(len(ds.Partitions))
		}
		q.ExpectedID, q.ExpectedSc = ds.Exact(q.Vector, q.Partition, threshold)
		queries = append(queries, q)
	}
	return queries
}

// Exact is the brute-force answer: the entry of partition p with the highest
// cosine similarity to vector, if that similarity reaches threshold.
func (ds *Dataset) Exact(vector []float32, p int, threshold float64) (string, float64) {
	bestID, best := "", math.Inf(-1)
	for _, idx := range ds.byPartition[p] {
		if sim := Cosine(vector, ds.Entries[idx].Vector); sim > best {
			best, bestID = sim, ds.Entries[idx].ID
		}
	}
	if bestID == "" || best < threshold {
		return "", best
	}
	return bestID, best
}

// LookupIDs draws n existing entry ids (direct-lookup hits).
func (ds *Dataset) LookupIDs(n int, stream uint64) []int {
	rng := rand.New(rand.NewPCG(ds.Seed^0x5851f42d4c957f2d, uint64(ds.Dim)<<32|stream))
	out := make([]int, n)
	for i := range out {
		out[i] = rng.IntN(len(ds.Entries))
	}
	return out
}

// MissingIDs draws n well-formed UUIDs that are not in the dataset (direct-lookup misses).
func (ds *Dataset) MissingIDs(n int, stream uint64) []string {
	rng := rand.New(rand.NewPCG(ds.Seed^0x2545f4914f6cdd1d, uint64(ds.Dim)<<32|stream))
	known := make(map[string]struct{}, len(ds.Entries))
	for _, e := range ds.Entries {
		known[e.ID] = struct{}{}
	}
	out := make([]string, 0, n)
	for len(out) < n {
		id := randomUUID(rng)
		if _, ok := known[id]; !ok {
			out = append(out, id)
		}
	}
	return out
}
