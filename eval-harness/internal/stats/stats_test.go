package stats

import (
	"errors"
	"testing"
	"time"
)

func TestSummarizeNearestRank(t *testing.T) {
	var latencies []time.Duration
	for i := 1; i <= 100; i++ {
		latencies = append(latencies, time.Duration(i)*time.Millisecond)
	}
	s := Summarize(latencies, 3, 2*time.Second)
	if s.P50Ms != 50 || s.P95Ms != 95 || s.P99Ms != 99 {
		t.Fatalf("nearest-rank percentiles of 1..100 ms: got p50=%v p95=%v p99=%v", s.P50Ms, s.P95Ms, s.P99Ms)
	}
	if s.MeanMs != 50.5 || s.MinMs != 1 || s.MaxMs != 100 {
		t.Fatalf("mean/min/max: got %v/%v/%v", s.MeanMs, s.MinMs, s.MaxMs)
	}
	if s.Count != 103 || s.Errors != 3 || s.Throughput != 50 {
		t.Fatalf("count/errors/throughput: got %d/%d/%v", s.Count, s.Errors, s.Throughput)
	}
}

func TestMedianOfTakesEachFieldsMedianAndSumsCounts(t *testing.T) {
	runs := []Summary{
		{Count: 10, Errors: 1, P50Ms: 3, P99Ms: 30, Throughput: 100},
		{Count: 10, Errors: 0, P50Ms: 1, P99Ms: 90, Throughput: 300},
		{Count: 10, Errors: 2, P50Ms: 2, P99Ms: 10, Throughput: 200},
	}
	m := MedianOf(runs)
	if m.P50Ms != 2 || m.P99Ms != 30 || m.Throughput != 200 {
		t.Fatalf("medians: got p50=%v p99=%v tput=%v", m.P50Ms, m.P99Ms, m.Throughput)
	}
	if m.Count != 30 || m.Errors != 3 {
		t.Fatalf("sums: got count=%d errors=%d", m.Count, m.Errors)
	}
}

func TestRunPhaseRunsEveryIndexOnceAndCountsErrors(t *testing.T) {
	seen := make([]int32, 500)
	result := RunPhase(len(seen), 8, func(i int) error {
		seen[i]++
		if i%10 == 0 {
			return errors.New("boom")
		}
		return nil
	})
	for i, n := range seen {
		if n != 1 {
			t.Fatalf("index %d ran %d times", i, n)
		}
	}
	if result.Errors != 50 || len(result.Latencies) != 450 || result.FirstErr == nil {
		t.Fatalf("errors=%d latencies=%d firstErr=%v", result.Errors, len(result.Latencies), result.FirstErr)
	}
}
