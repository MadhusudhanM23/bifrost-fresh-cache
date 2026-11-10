package workload

import (
	"math"
	"math/rand/v2"
	"testing"
)

func TestGenerateIsDeterministic(t *testing.T) {
	cfg := Config{Dim: 32, N: 200, Keys: 5, ResponseBytes: 512, Seed: 7}
	a, b := Generate(cfg), Generate(cfg)
	for i := range a.Entries {
		if a.Entries[i].ID != b.Entries[i].ID || a.Entries[i].Vector[3] != b.Entries[i].Vector[3] {
			t.Fatalf("entry %d differs between two generations with the same seed", i)
		}
	}
	if got := len(a.Entries[0].Metadata["response"].(string)); got != 512 {
		t.Fatalf("response payload is %d bytes, want 512", got)
	}
	if a.PartitionSize(0) != 40 {
		t.Fatalf("partition 0 holds %d entries, want 40", a.PartitionSize(0))
	}
}

func TestRandomVectorsAreUnit(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	v := RandomUnitVector(rng, 1536)
	var norm float64
	for _, x := range v {
		norm += float64(x) * float64(x)
	}
	if math.Abs(norm-1) > 1e-5 {
		t.Fatalf("squared norm %v, want 1", norm)
	}
}

func TestWithSimilarityHitsTheTargetCosine(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	for _, dim := range []int{16, 384, 1536} {
		base := RandomUnitVector(rng, dim)
		for _, sim := range []float64{0.82, 0.9, 0.98} {
			q := WithSimilarity(rng, base, sim)
			if got := Cosine(base, q); math.Abs(got-sim) > 1e-5 {
				t.Fatalf("dim %d: cosine %v, want %v", dim, got, sim)
			}
		}
	}
}

func TestExactAnswers(t *testing.T) {
	ds := Generate(Config{Dim: 384, N: 1000, Keys: 10, ResponseBytes: 64, Seed: 11})
	for _, q := range ds.NearestQueries(ClassHit, 50, 0.8, 1) {
		if q.ExpectedID == "" || q.ExpectedSc < 0.8 {
			t.Fatalf("a should-hit query has no exact answer (best %.3f)", q.ExpectedSc)
		}
		idx, _ := ds.Index(q.ExpectedID)
		if ds.Entries[idx].Partition != q.Partition {
			t.Fatalf("exact answer outside the query's partition")
		}
	}
	for _, class := range []QueryClass{ClassMiss, ClassCross} {
		for _, q := range ds.NearestQueries(class, 50, 0.8, 2) {
			if q.ExpectedID != "" {
				t.Fatalf("%s query has an exact answer with cosine %.3f", class, q.ExpectedSc)
			}
		}
	}
	missing := ds.MissingIDs(20, 3)
	for _, id := range missing {
		if _, ok := ds.Index(id); ok {
			t.Fatalf("MissingIDs returned an existing id")
		}
	}
}
