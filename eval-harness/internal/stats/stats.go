// Package stats runs timed phases and summarises their latencies.
package stats

import (
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"bifrost-eval-harness/internal/clock"
)

// Summary describes one timed phase: Count operations at a fixed concurrency.
// Percentiles use the nearest-rank method over successful operations only;
// failed operations are counted in Errors and excluded from the latencies.
type Summary struct {
	Count       int     `json:"count"`
	Errors      int     `json:"errors"`
	P50Ms       float64 `json:"p50_ms"`
	P95Ms       float64 `json:"p95_ms"`
	P99Ms       float64 `json:"p99_ms"`
	MeanMs      float64 `json:"mean_ms"`
	MinMs       float64 `json:"min_ms"`
	MaxMs       float64 `json:"max_ms"`
	WallSeconds float64 `json:"wall_seconds"`
	// Throughput is successful operations per second of wall time.
	Throughput float64 `json:"throughput_ops_per_s"`
}

// Summarize builds a Summary from successful latencies, an error count and the
// phase's wall time.
func Summarize(latencies []time.Duration, errors int, wall time.Duration) Summary {
	s := Summary{Count: len(latencies) + errors, Errors: errors, WallSeconds: wall.Seconds()}
	if len(latencies) == 0 {
		return s
	}
	sorted := append([]time.Duration(nil), latencies...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var total time.Duration
	for _, l := range sorted {
		total += l
	}
	s.P50Ms = ms(percentile(sorted, 50))
	s.P95Ms = ms(percentile(sorted, 95))
	s.P99Ms = ms(percentile(sorted, 99))
	s.MeanMs = ms(total) / float64(len(sorted))
	s.MinMs = ms(sorted[0])
	s.MaxMs = ms(sorted[len(sorted)-1])
	if wall > 0 {
		s.Throughput = float64(len(sorted)) / wall.Seconds()
	}
	return s
}

// percentile returns the nearest-rank percentile of sorted values.
func percentile(sorted []time.Duration, p float64) time.Duration {
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// MedianOf combines repeated runs of the same phase: each field is the median
// of that field across runs (so the p99 shown is the median run's p99, not a
// p99 over pooled samples). Errors and Count are summed, since they are counts.
func MedianOf(runs []Summary) Summary {
	if len(runs) == 0 {
		return Summary{}
	}
	pick := func(get func(Summary) float64) float64 {
		values := make([]float64, len(runs))
		for i, r := range runs {
			values[i] = get(r)
		}
		return median(values)
	}
	out := Summary{
		P50Ms:       pick(func(s Summary) float64 { return s.P50Ms }),
		P95Ms:       pick(func(s Summary) float64 { return s.P95Ms }),
		P99Ms:       pick(func(s Summary) float64 { return s.P99Ms }),
		MeanMs:      pick(func(s Summary) float64 { return s.MeanMs }),
		MinMs:       pick(func(s Summary) float64 { return s.MinMs }),
		MaxMs:       pick(func(s Summary) float64 { return s.MaxMs }),
		WallSeconds: pick(func(s Summary) float64 { return s.WallSeconds }),
		Throughput:  pick(func(s Summary) float64 { return s.Throughput }),
	}
	for _, r := range runs {
		out.Count += r.Count
		out.Errors += r.Errors
	}
	return out
}

func median(values []float64) float64 {
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// PhaseResult is the raw outcome of RunPhase.
type PhaseResult struct {
	Latencies []time.Duration
	Errors    int
	Wall      time.Duration
	FirstErr  error
}

// Summary summarises the phase.
func (p PhaseResult) Summary() Summary { return Summarize(p.Latencies, p.Errors, p.Wall) }

// RunPhase runs op(0) .. op(n-1) on `concurrency` workers that pull the next
// index as soon as they finish the previous one (a closed loop: no pacing, no
// think time). Each call is timed individually; the phase's wall time covers
// the first start to the last finish.
func RunPhase(n, concurrency int, op func(i int) error) PhaseResult {
	if concurrency < 1 {
		concurrency = 1
	}
	var next atomic.Int64
	var mu sync.Mutex
	result := PhaseResult{Latencies: make([]time.Duration, 0, n)}
	var wg sync.WaitGroup
	start := clock.Now()
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]time.Duration, 0, n/concurrency+1)
			localErrs := 0
			var firstErr error
			for {
				i := int(next.Add(1) - 1)
				if i >= n {
					break
				}
				t0 := clock.Now()
				err := op(i)
				elapsed := clock.Since(t0)
				if err != nil {
					localErrs++
					if firstErr == nil {
						firstErr = err
					}
					continue
				}
				local = append(local, elapsed)
			}
			mu.Lock()
			result.Latencies = append(result.Latencies, local...)
			result.Errors += localErrs
			if result.FirstErr == nil {
				result.FirstErr = firstErr
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	result.Wall = clock.Since(start)
	return result
}
