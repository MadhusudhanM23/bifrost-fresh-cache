//go:build !windows

package clock

import "time"

var origin = time.Now()

// Now reads the monotonic clock, in nanoseconds since the process started.
func Now() Ticks { return Ticks(time.Since(origin)) }

func nanosPerTick() float64 { return 1 }

// Source names the clock behind Now.
func Source() string { return "time.Now (monotonic)" }
