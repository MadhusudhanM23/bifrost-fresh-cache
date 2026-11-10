// Package clock is the latency clock used by every measurement in the harness.
//
// Go's time.Now on Windows is derived from the interrupt time, which on the
// machine this harness was written on advances in steps of roughly 0.3 to 0.6
// ms. That is too coarse for operations that take a fraction of a millisecond,
// so on Windows the harness reads QueryPerformanceCounter (100 ns resolution)
// instead. Elsewhere the monotonic reading of time.Now is used.
package clock

import (
	"sort"
	"time"
)

// Since returns the time elapsed since a reading taken with Now.
func Since(start Ticks) time.Duration { return Now().Sub(start) }

// Ticks is a reading of the latency clock.
type Ticks int64

// Sub returns t - u as a duration.
func (t Ticks) Sub(u Ticks) time.Duration {
	return time.Duration(float64(t-u) * nanosPerTick())
}

// Resolution reports the smallest step the latency clock was observed to take
// and its source, so a results file records how fine its latencies can be.
func Resolution() (step time.Duration, source string) {
	steps := make([]time.Duration, 0, 2000)
	prev := Now()
	for len(steps) < cap(steps) {
		now := Now()
		if now > prev {
			steps = append(steps, now.Sub(prev))
			prev = now
		}
	}
	sort.Slice(steps, func(i, j int) bool { return steps[i] < steps[j] })
	return steps[len(steps)/2], Source()
}
